// Port of server/src/lib/incus/tunnel-manager.ts (544 lines), plus the 5-byte
// frame codec of server/src/lib/incus/tunnel-protocol.ts.
//
// The manager owns every reverse WebSocket tunnel from a host agent and provides
// three things to the rest of the backend:
//
//   - a full-duplex byte stream per Incus connection (CreateDuplexStream), used
//     by lib/incus_client.go's tunnel branch, lib/caddy_client.go and
//     lib/node_security_transport.go;
//   - bounded control RPCs to the node agent (RequestControl → Incus
//     /1.0/agent endpoints), 4 in flight per host;
//   - heartbeat round-trips, agent log streaming and task-log streaming.
//
// Transport boundary (IMPORTANT)
// ------------------------------
// The Node class takes a `WebSocketLike` and attaches `message`/`close`/`error`
// listeners itself. The Go port deliberately does NOT depend on a WebSocket
// library (gorilla/websocket is not in go.mod and this port must not add
// dependencies). Instead the caller that terminates the socket (the agent route)
// drives the manager:
//
//	manager.RegisterTunnel(hostID, socket)   // sends CONTROL_HELLO
//	for { data := socket read loop }          // caller owns I/O
//	    manager.HandleTunnelMessage(hostID, socket, data)
//	manager.HandleTunnelClosed(hostID, socket)
//
// TunnelSocket implementations MUST be pointers: the TypeScript compares sockets
// by reference (`existing !== ws`) and so does this port.
//
// Process-local state (WAVE_D_SPEC §0.1)
// --------------------------------------
// activeTunnels / streams / control capabilities / pending heartbeats and
// controls are in-process maps, exactly like the Node singleton. A second
// backend replica would not see another replica's tunnels; that is one more
// reason the private backend must stay a single process. Do not move this to
// PostgreSQL.
package lib

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TunnelSocketOpen mirrors WebSocket.OPEN.
const TunnelSocketOpen = 1

// TunnelDefaultHeartbeatTimeout mirrors the `timeoutMs = 105000` default.
const TunnelDefaultHeartbeatTimeout = 105 * time.Second

// tunnelControlPayloadLimit mirrors `1024 * 1024`.
const tunnelControlPayloadLimit = 1024 * 1024

// tunnelMaxControlsPerHost mirrors the `>= 4` BUSY threshold.
const tunnelMaxControlsPerHost = 4

// TunnelFrameType is the 1-byte frame discriminator of tunnel-protocol.ts.
type TunnelFrameType uint8

const (
	TunnelFrameOpen           TunnelFrameType = 0x01 // Server -> Agent: JSON {targetHost, targetPort}
	TunnelFrameData           TunnelFrameType = 0x02 // bidirectional bytes (max 64KB chunks)
	TunnelFrameClose          TunnelFrameType = 0x03 // graceful stream end / EOF
	TunnelFrameReset          TunnelFrameType = 0x04 // abnormal stream reset
	TunnelFrameConfig         TunnelFrameType = 0x05 // Server -> Agent: JSON config (stream 0)
	TunnelFrameLogControl     TunnelFrameType = 0x06 // Server -> Agent: JSON {action, lines?}
	TunnelFrameLogData        TunnelFrameType = 0x07 // Agent -> Server: one UTF-8 log line
	TunnelFrameControlHello   TunnelFrameType = 0x0a
	TunnelFrameHeartbeat      TunnelFrameType = 0x0b
	TunnelFrameTaskLogControl TunnelFrameType = 0x0c
	TunnelFrameTaskLogData    TunnelFrameType = 0x0d
)

// TunnelFrame mirrors DecodedFrame.
type TunnelFrame struct {
	Type     TunnelFrameType
	StreamID uint32
	Payload  []byte
}

// TunnelEncodeFrame mirrors encodeFrame: 1-byte type + 4-byte big-endian stream
// id + payload.
func TunnelEncodeFrame(frameType TunnelFrameType, streamID uint32, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = byte(frameType)
	frame[1] = byte(streamID >> 24)
	frame[2] = byte(streamID >> 16)
	frame[3] = byte(streamID >> 8)
	frame[4] = byte(streamID)
	copy(frame[5:], payload)
	return frame
}

// TunnelDecodeFrame mirrors decodeFrame.
func TunnelDecodeFrame(buf []byte) (TunnelFrame, error) {
	if len(buf) < 5 {
		return TunnelFrame{}, fmt.Errorf("Frame too short: expected at least 5 bytes, got %d", len(buf))
	}
	return TunnelFrame{
		Type:     TunnelFrameType(buf[0]),
		StreamID: uint32(buf[1])<<24 | uint32(buf[2])<<16 | uint32(buf[3])<<8 | uint32(buf[4]),
		Payload:  buf[5:],
	}, nil
}

// TunnelTargetConfig mirrors TunnelTargetConfig.
type TunnelTargetConfig struct {
	TargetHost string `json:"targetHost"`
	TargetPort int    `json:"targetPort"`
}

// TunnelControlMethod mirrors ControlMethod.
type TunnelControlMethod string

const (
	TunnelControlNetworkApply   TunnelControlMethod = "network.apply"
	TunnelControlNetworkApplyV2 TunnelControlMethod = "network.apply.v2"
	TunnelControlProxyProbe     TunnelControlMethod = "proxy.probe"
)

// TunnelLogAction mirrors the 'start' | 'stop' union used by the log controls.
type TunnelLogAction string

const (
	TunnelLogActionStart TunnelLogAction = "start"
	TunnelLogActionStop  TunnelLogAction = "stop"
)

// TunnelControlErrorCode mirrors the TunnelControlError code union.
type TunnelControlErrorCode string

const (
	TunnelControlOffline         TunnelControlErrorCode = "OFFLINE"
	TunnelControlUpgradeRequired TunnelControlErrorCode = "UPGRADE_REQUIRED"
	TunnelControlBusy            TunnelControlErrorCode = "BUSY"
	TunnelControlTimeout         TunnelControlErrorCode = "TIMEOUT"
	TunnelControlDisconnected    TunnelControlErrorCode = "DISCONNECTED"
	TunnelControlCancelled       TunnelControlErrorCode = "CANCELLED"
	TunnelControlInvalidResponse TunnelControlErrorCode = "INVALID_RESPONSE"
	TunnelControlRemoteError     TunnelControlErrorCode = "REMOTE_ERROR"
)

// TunnelControlError mirrors the TunnelControlError class. The message is the
// code, exactly like the TypeScript (`super(code)`).
type TunnelControlError struct {
	Code TunnelControlErrorCode
}

// Error implements error.
func (e *TunnelControlError) Error() string { return string(e.Code) }

// TunnelHeartbeatTestError mirrors HeartbeatTestError: an HTTP status plus the
// user-facing message the route returns verbatim.
type TunnelHeartbeatTestError struct {
	StatusCode int
	Message    string
}

// Error implements error.
func (e *TunnelHeartbeatTestError) Error() string { return e.Message }

// TunnelNodeAPIRequester mirrors NodeAPIRequester.
type TunnelNodeAPIRequester func(
	ctx context.Context,
	hostID int32,
	method TunnelControlMethod,
	params any,
) (any, error)

// TunnelSocket is the minimal WebSocket surface the manager needs. The caller
// owns reads and close notifications; see the package comment.
type TunnelSocket interface {
	// TunnelReadyState returns 1 when the socket is open (WebSocketLike.readyState).
	TunnelReadyState() int
	// TunnelSend mirrors `ws.send(data, cb?)`; the returned error is the send error.
	TunnelSend(data []byte) error
	// TunnelClose mirrors `ws.close(code, reason)`.
	TunnelClose(code int, reason string)
}

type tunnelControlResult struct {
	value any
	err   error
}

type tunnelPendingControl struct {
	hostID int32
	socket TunnelSocket
	ch     chan tunnelControlResult
	once   sync.Once
}

func (p *tunnelPendingControl) finish(err error, value any) {
	p.once.Do(func() { p.ch <- tunnelControlResult{value: value, err: err} })
}

type tunnelPendingHeartbeat struct {
	hostID int32
	socket TunnelSocket
	ch     chan error
	once   sync.Once
}

func (p *tunnelPendingHeartbeat) finish(err error) {
	p.once.Do(func() { p.ch <- err })
}

// TunnelManager mirrors HostTunnelManager. Safe for concurrent use.
type TunnelManager struct {
	nodeRequest TunnelNodeAPIRequester

	mu                  sync.Mutex
	activeTunnels       map[int32]TunnelSocket
	streams             map[string]*TunnelStream
	nextStreamID        uint32
	controlCapabilities map[int32]map[string]bool
	pendingHeartbeats   map[string]*tunnelPendingHeartbeat
	pendingControls     map[string]*tunnelPendingControl

	listenerSeq               uint64
	tunnelConnectedListeners  map[uint64]func(hostID int32)
	tunnelDisconnectListeners map[uint64]func(hostID int32)
	controlReadyListeners     map[uint64]func(hostID int32)
	taskLogListeners          map[uint64]func(hostID int32, taskID int64, line string)
	logDataListeners          map[uint64]func(hostID int32, line *string)
}

// NewTunnelManager mirrors `new HostTunnelManager(nodeRequest?)`.
func NewTunnelManager(nodeRequest TunnelNodeAPIRequester) *TunnelManager {
	if nodeRequest == nil {
		nodeRequest = tunnelDefaultNodeRequest
	}
	return &TunnelManager{
		nodeRequest:               nodeRequest,
		activeTunnels:             map[int32]TunnelSocket{},
		streams:                   map[string]*TunnelStream{},
		controlCapabilities:       map[int32]map[string]bool{},
		pendingHeartbeats:         map[string]*tunnelPendingHeartbeat{},
		pendingControls:           map[string]*tunnelPendingControl{},
		tunnelConnectedListeners:  map[uint64]func(int32){},
		tunnelDisconnectListeners: map[uint64]func(int32){},
		controlReadyListeners:     map[uint64]func(int32){},
		taskLogListeners:          map[uint64]func(int32, int64, string){},
		logDataListeners:          map[uint64]func(int32, *string){},
	}
}

var (
	tunnelManagerOnce     sync.Once
	tunnelManagerInstance *TunnelManager
)

// TunnelManagerInstance returns the global singleton (hostTunnelManager).
func TunnelManagerInstance() *TunnelManager {
	tunnelManagerOnce.Do(func() {
		tunnelManagerInstance = NewTunnelManager(nil)
	})
	return tunnelManagerInstance
}

// ==================== event listeners ====================

// OnTunnelConnected mirrors `on('tunnel_connected', …)`. The returned function
// is the matching `off`.
func (m *TunnelManager) OnTunnelConnected(listener func(hostID int32)) func() {
	return tunnelAddListener(&m.mu, &m.listenerSeq, m.tunnelConnectedListeners, listener)
}

// OnTunnelDisconnected mirrors `on('tunnel_disconnected', …)`.
func (m *TunnelManager) OnTunnelDisconnected(listener func(hostID int32)) func() {
	return tunnelAddListener(&m.mu, &m.listenerSeq, m.tunnelDisconnectListeners, listener)
}

// OnControlReady mirrors `on('control_ready', …)`.
func (m *TunnelManager) OnControlReady(listener func(hostID int32)) func() {
	return tunnelAddListener(&m.mu, &m.listenerSeq, m.controlReadyListeners, listener)
}

// OnTaskLog mirrors `on('task_log', …)`.
func (m *TunnelManager) OnTaskLog(listener func(hostID int32, taskID int64, line string)) func() {
	return tunnelAddListener(&m.mu, &m.listenerSeq, m.taskLogListeners, listener)
}

// OnLogData mirrors `on('log_data', …)`; line is nil when the agent closes the
// log stream (empty payload).
func (m *TunnelManager) OnLogData(listener func(hostID int32, line *string)) func() {
	return tunnelAddListener(&m.mu, &m.listenerSeq, m.logDataListeners, listener)
}

func tunnelAddListener[F any](mu *sync.Mutex, seq *uint64, listeners map[uint64]F, listener F) func() {
	mu.Lock()
	defer mu.Unlock()
	*seq++
	id := *seq
	listeners[id] = listener
	return func() {
		mu.Lock()
		defer mu.Unlock()
		delete(listeners, id)
	}
}

func tunnelListenerSnapshot[F any](mu *sync.Mutex, listeners map[uint64]F) []F {
	mu.Lock()
	defer mu.Unlock()
	snapshot := make([]F, 0, len(listeners))
	for _, listener := range listeners {
		snapshot = append(snapshot, listener)
	}
	return snapshot
}

func (m *TunnelManager) emitTunnelConnected(hostID int32) {
	for _, listener := range tunnelListenerSnapshot(&m.mu, m.tunnelConnectedListeners) {
		listener(hostID)
	}
}

func (m *TunnelManager) emitTunnelDisconnected(hostID int32) {
	for _, listener := range tunnelListenerSnapshot(&m.mu, m.tunnelDisconnectListeners) {
		listener(hostID)
	}
}

func (m *TunnelManager) emitControlReady(hostID int32) {
	for _, listener := range tunnelListenerSnapshot(&m.mu, m.controlReadyListeners) {
		listener(hostID)
	}
}

func (m *TunnelManager) emitTaskLog(hostID int32, taskID int64, line string) {
	for _, listener := range tunnelListenerSnapshot(&m.mu, m.taskLogListeners) {
		listener(hostID, taskID, line)
	}
}

func (m *TunnelManager) emitLogData(hostID int32, line *string) {
	for _, listener := range tunnelListenerSnapshot(&m.mu, m.logDataListeners) {
		listener(hostID, line)
	}
}

// tunnelSameSocket mirrors `a === b` for interface values. A non-comparable
// implementation cannot be compared; it is treated as a different socket.
func tunnelSameSocket(left, right TunnelSocket) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	same := false
	func() {
		defer func() { _ = recover() }()
		same = left == right
	}()
	return same
}

// ==================== registration ====================

// RegisterTunnel mirrors registerTunnel: replace any existing socket, reset the
// capability set, announce the connection and ask for a CONTROL_HELLO.
func (m *TunnelManager) RegisterTunnel(hostID int32, socket TunnelSocket) {
	if socket == nil {
		return
	}

	m.mu.Lock()
	existing := m.activeTunnels[hostID]
	replaced := existing != nil && !tunnelSameSocket(existing, socket)
	if replaced {
		m.cleanupHostControlsLocked(hostID)
	}
	// Install the new socket before closing the old one: a TunnelClose
	// implementation that reports the close synchronously calls
	// HandleTunnelClosed, whose identity guard then sees the NEW socket and
	// correctly does nothing (Node relies on ws emitting 'close' asynchronously).
	m.activeTunnels[hostID] = socket
	delete(m.controlCapabilities, hostID)
	m.mu.Unlock()

	if replaced {
		// Close outside the lock: TunnelClose may block.
		tunnelSafeClose(existing, 1000, "Replaced by newer agent tunnel connection")
	}

	// `this.emit('tunnel_connected', hostId)` runs before the hello is sent.
	m.emitTunnelConnected(hostID)

	// Ask after authentication/listener installation; an Agent's initial hello
	// could otherwise arrive while the websocket route is still authenticating.
	_ = socket.TunnelSend(TunnelEncodeFrame(TunnelFrameControlHello, 0, nil))
}

func tunnelSafeClose(socket TunnelSocket, code int, reason string) {
	if socket == nil {
		return
	}
	defer func() { _ = recover() }()
	socket.TunnelClose(code, reason)
}

// UnregisterTunnel mirrors unregisterTunnel(hostId, ws?). A nil socket means
// "unconditional" (`!ws`); a non-nil socket only unregisters when it is still
// the active tunnel.
func (m *TunnelManager) UnregisterTunnel(hostID int32, socket TunnelSocket) {
	m.mu.Lock()
	current := m.activeTunnels[hostID]
	if socket != nil && !tunnelSameSocket(current, socket) {
		m.mu.Unlock()
		return
	}
	delete(m.activeTunnels, hostID)
	m.cleanupHostControlsLocked(hostID)
	streams := m.detachHostStreamsLocked(hostID)
	m.mu.Unlock()

	if current != nil {
		tunnelSafeClose(current, 1000, "Unregistered")
	}
	err := fmt.Errorf("Host %d tunnel unregistered", hostID)
	for _, stream := range streams {
		stream.reset(err)
	}
	m.emitTunnelDisconnected(hostID)
}

// HandleTunnelClosed mirrors the socket's `close` listener: only the currently
// registered socket may tear the tunnel down.
func (m *TunnelManager) HandleTunnelClosed(hostID int32, socket TunnelSocket) {
	if socket == nil {
		return
	}
	m.mu.Lock()
	current := m.activeTunnels[hostID]
	if !tunnelSameSocket(current, socket) {
		m.mu.Unlock()
		return
	}
	delete(m.activeTunnels, hostID)
	m.cleanupHostControlsLocked(hostID)
	streams := m.detachHostStreamsLocked(hostID)
	m.mu.Unlock()

	err := fmt.Errorf("Host %d tunnel closed", hostID)
	for _, stream := range streams {
		stream.reset(err)
	}
	m.emitTunnelDisconnected(hostID)
}

// IsTunnelOnline mirrors isTunnelOnline.
func (m *TunnelManager) IsTunnelOnline(hostID int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	socket := m.activeTunnels[hostID]
	return socket != nil && socket.TunnelReadyState() == TunnelSocketOpen
}

// GetTunnel mirrors getTunnel.
func (m *TunnelManager) GetTunnel(hostID int32) (TunnelSocket, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	socket, ok := m.activeTunnels[hostID]
	return socket, ok
}

// cleanupHostControlsLocked mirrors cleanupHostControls. The caller holds m.mu;
// finish() only writes to a buffered channel, so it never blocks.
func (m *TunnelManager) cleanupHostControlsLocked(hostID int32) {
	delete(m.controlCapabilities, hostID)
	for _, pending := range m.pendingHeartbeats {
		if pending.hostID == hostID {
			pending.finish(&TunnelHeartbeatTestError{StatusCode: 502, Message: "节点连接已断开，上报测试失败"})
		}
	}
	for _, pending := range m.pendingControls {
		if pending.hostID == hostID {
			pending.finish(&TunnelControlError{Code: TunnelControlDisconnected}, nil)
		}
	}
}

func (m *TunnelManager) detachHostStreamsLocked(hostID int32) []*TunnelStream {
	prefix := fmt.Sprintf("%d:", hostID)
	streams := make([]*TunnelStream, 0, 2)
	for key, stream := range m.streams {
		if strings.HasPrefix(key, prefix) {
			delete(m.streams, key)
			streams = append(streams, stream)
		}
	}
	return streams
}

func (m *TunnelManager) removeStream(key string, stream *TunnelStream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.streams[key]; ok && current == stream {
		delete(m.streams, key)
	}
}

// ==================== heartbeat ====================

// RequestHeartbeat mirrors requestHeartbeat (fire-and-forget trigger).
func (m *TunnelManager) RequestHeartbeat(hostID int32) bool {
	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	capabilities := m.controlCapabilities[hostID]
	m.mu.Unlock()

	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen || !capabilities["heartbeat-trigger-v1"] {
		return false
	}
	if err := socket.TunnelSend(TunnelEncodeFrame(TunnelFrameHeartbeat, 0, nil)); err != nil {
		return false
	}
	return true
}

// RequestHeartbeatAndWait mirrors requestHeartbeatAndWait: completion only comes
// from the authenticated HTTP report (CompleteHeartbeat).
func (m *TunnelManager) RequestHeartbeatAndWait(ctx context.Context, hostID int32, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = TunnelDefaultHeartbeatTimeout
	}

	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen {
		m.mu.Unlock()
		return &TunnelHeartbeatTestError{StatusCode: 409, Message: "节点连接不可用，上报测试失败"}
	}
	if !m.controlCapabilities[hostID]["heartbeat-confirm-v1"] {
		m.mu.Unlock()
		return &TunnelHeartbeatTestError{StatusCode: 409, Message: "节点版本不支持上报结果确认，请更新节点集成包"}
	}
	for _, pending := range m.pendingHeartbeats {
		if pending.hostID == hostID {
			m.mu.Unlock()
			return &TunnelHeartbeatTestError{StatusCode: 409, Message: "此节点正在执行上报测试，请等待结果"}
		}
	}
	requestID := tunnelNewRequestID()
	pending := &tunnelPendingHeartbeat{hostID: hostID, socket: socket, ch: make(chan error, 1)}
	m.pendingHeartbeats[requestID] = pending
	m.mu.Unlock()

	defer m.removePendingHeartbeat(requestID, pending)

	payload, err := json.Marshal(map[string]string{"requestId": requestID})
	if err != nil {
		return &TunnelHeartbeatTestError{StatusCode: 502, Message: "无法发送本次上报请求，测试失败"}
	}
	if err := socket.TunnelSend(TunnelEncodeFrame(TunnelFrameHeartbeat, 0, payload)); err != nil {
		return &TunnelHeartbeatTestError{StatusCode: 502, Message: "无法发送本次上报请求，测试失败"}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-pending.ch:
		return err
	case <-timer.C:
		return &TunnelHeartbeatTestError{StatusCode: 504, Message: "等待本次节点上报超时，测试失败"}
	case <-ctx.Done():
		// Go-only cancellation path (the TypeScript promise has no AbortSignal).
		return ctx.Err()
	}
}

func (m *TunnelManager) removePendingHeartbeat(requestID string, pending *tunnelPendingHeartbeat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.pendingHeartbeats[requestID]; ok && current == pending {
		delete(m.pendingHeartbeats, requestID)
	}
}

// CompleteHeartbeat mirrors completeHeartbeat(hostId, requestId, error?).
func (m *TunnelManager) CompleteHeartbeat(hostID int32, requestID any, message string) {
	key, ok := requestID.(string)
	if !ok {
		return
	}

	m.mu.Lock()
	pending := m.pendingHeartbeats[key]
	if pending == nil || pending.hostID != hostID || !tunnelSameSocket(m.activeTunnels[hostID], pending.socket) {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	if message != "" {
		pending.finish(&TunnelHeartbeatTestError{StatusCode: 502, Message: message})
		return
	}
	pending.finish(nil)
}

// tunnelNewRequestID mirrors randomUUID() (crypto-random v4 UUID).
func tunnelNewRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// CSPRNG failure leaves no safe id to hand out; fall back to a monotonic
		// value so the round-trip still has a unique key.
		return fmt.Sprintf("tunnel-%d", time.Now().UTC().UnixNano())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// ==================== control RPC ====================

// SupportsControl mirrors supportsControl. The method argument is unused in the
// TypeScript as well.
func (m *TunnelManager) SupportsControl(hostID int32, _ TunnelControlMethod) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	socket := m.activeTunnels[hostID]
	return socket != nil && socket.TunnelReadyState() == TunnelSocketOpen &&
		m.controlCapabilities[hostID]["incus_node_v1"]
}

// RequestControl mirrors requestControl(hostId, method, params, timeout, signal?).
// Caller cancellation surfaces as CANCELLED, the timeout as TIMEOUT, an agent
// 409 as BUSY and an agent 504 as TIMEOUT (same mapping as the TypeScript).
func (m *TunnelManager) RequestControl(
	ctx context.Context,
	hostID int32,
	method TunnelControlMethod,
	params any,
	timeout time.Duration,
) (any, error) {
	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen {
		m.mu.Unlock()
		return nil, &TunnelControlError{Code: TunnelControlOffline}
	}
	supported := m.controlCapabilities[hostID]["incus_node_v1"]
	pendingCount := 0
	for _, pending := range m.pendingControls {
		if pending.hostID == hostID {
			pendingCount++
		}
	}
	m.mu.Unlock()

	if !supported {
		return nil, &TunnelControlError{Code: TunnelControlUpgradeRequired}
	}
	if ctx.Err() != nil {
		return nil, &TunnelControlError{Code: TunnelControlCancelled}
	}
	if pendingCount >= tunnelMaxControlsPerHost {
		return nil, &TunnelControlError{Code: TunnelControlBusy}
	}

	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if len(payload) > tunnelControlPayloadLimit {
		return nil, &TunnelControlError{Code: TunnelControlInvalidResponse}
	}

	return m.requestNativeControl(ctx, hostID, socket, method, params, timeout)
}

func (m *TunnelManager) requestNativeControl(
	ctx context.Context,
	hostID int32,
	socket TunnelSocket,
	method TunnelControlMethod,
	params any,
	timeout time.Duration,
) (any, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	streamID := m.allocateStreamID()
	key := fmt.Sprintf("%d:%d", hostID, streamID)
	pending := &tunnelPendingControl{
		hostID: hostID,
		socket: socket,
		ch:     make(chan tunnelControlResult, 1),
	}
	m.mu.Lock()
	m.pendingControls[key] = pending
	m.mu.Unlock()
	defer m.removePendingControl(key, pending)

	go func() {
		value, err := m.nodeRequest(requestCtx, hostID, method, params)
		pending.finish(err, value)
	}()

	if timeout <= 0 {
		timeout = TunnelDefaultHeartbeatTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-pending.ch:
		if result.err != nil {
			return nil, tunnelControlErrorFrom(result.err)
		}
		return result.value, nil
	case <-timer.C:
		return nil, &TunnelControlError{Code: TunnelControlTimeout}
	case <-ctx.Done():
		return nil, &TunnelControlError{Code: TunnelControlCancelled}
	}
}

func (m *TunnelManager) removePendingControl(key string, pending *tunnelPendingControl) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.pendingControls[key]; ok && current == pending {
		delete(m.pendingControls, key)
	}
}

// tunnelControlErrorFrom mirrors
// `status === 409 ? 'BUSY' : status === 504 ? 'TIMEOUT' : 'REMOTE_ERROR'`,
// passing through errors raised by this layer (DISCONNECTED / CANCELLED).
func tunnelControlErrorFrom(err error) error {
	var control *TunnelControlError
	if errors.As(err, &control) {
		return err
	}
	var apiError *IncusApiError
	if errors.As(err, &apiError) {
		switch apiError.StatusCode {
		case http.StatusConflict:
			return &TunnelControlError{Code: TunnelControlBusy}
		case http.StatusGatewayTimeout:
			return &TunnelControlError{Code: TunnelControlTimeout}
		}
	}
	return &TunnelControlError{Code: TunnelControlRemoteError}
}

func (m *TunnelManager) allocateStreamID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextStreamID = (m.nextStreamID + 1) & 0x7fffffff
	if m.nextStreamID == 0 {
		m.nextStreamID = 1
	}
	return m.nextStreamID
}

// ==================== outgoing control frames ====================

// BroadcastConfig mirrors broadcastConfig.
func (m *TunnelManager) BroadcastConfig(hostID int32, config TunnelTargetConfig) bool {
	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	m.mu.Unlock()

	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen {
		return false
	}
	payload, err := json.Marshal(config)
	if err != nil {
		log.Printf("[HostTunnelManager] Failed to send config frame to host %d: %v", hostID, err)
		return false
	}
	if err := socket.TunnelSend(TunnelEncodeFrame(TunnelFrameConfig, 0, payload)); err != nil {
		log.Printf("[HostTunnelManager] Failed to send config frame to host %d: %v", hostID, err)
		return false
	}
	return true
}

// SendTaskLogControl mirrors sendTaskLogControl.
func (m *TunnelManager) SendTaskLogControl(hostID int32, taskID int32, name string, action TunnelLogAction) bool {
	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	m.mu.Unlock()

	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen {
		return false
	}
	payload, err := json.Marshal(map[string]any{"taskId": taskID, "name": name, "action": string(action)})
	if err != nil {
		return false
	}
	if err := socket.TunnelSend(TunnelEncodeFrame(TunnelFrameTaskLogControl, 0, payload)); err != nil {
		return false
	}
	return true
}

// SendLogControl mirrors sendLogControl. `lines` is omitted when nil OR zero,
// because the TypeScript spreads it only when the value is truthy.
func (m *TunnelManager) SendLogControl(hostID int32, action TunnelLogAction, lines *int32) bool {
	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	m.mu.Unlock()

	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen {
		return false
	}
	payloadMap := map[string]any{"action": string(action)}
	if lines != nil && *lines != 0 {
		payloadMap["lines"] = *lines
	}
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		log.Printf("[HostTunnelManager] Failed to send log control frame to host %d: %v", hostID, err)
		return false
	}
	if err := socket.TunnelSend(TunnelEncodeFrame(TunnelFrameLogControl, 0, payload)); err != nil {
		log.Printf("[HostTunnelManager] Failed to send log control frame to host %d: %v", hostID, err)
		return false
	}
	return true
}

// ==================== duplex streams ====================

// CreateDuplexStream mirrors createDuplexStream: allocate a stream id, send an
// OPEN frame and return the full-duplex byte stream that traffic is tunnelled
// through. The returned net.Conn is what the Incus/Caddy/SPICE transports dial.
func (m *TunnelManager) CreateDuplexStream(hostID int32, targetHost string, targetPort int) (net.Conn, error) {
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	if targetPort == 0 {
		targetPort = 8443
	}

	m.mu.Lock()
	socket := m.activeTunnels[hostID]
	if socket == nil || socket.TunnelReadyState() != TunnelSocketOpen {
		m.mu.Unlock()
		return nil, fmt.Errorf("Host %d tunnel is not connected or offline", hostID)
	}
	m.nextStreamID = (m.nextStreamID + 1) & 0x7fffffff
	if m.nextStreamID == 0 {
		m.nextStreamID = 1
	}
	streamID := m.nextStreamID
	key := fmt.Sprintf("%d:%d", hostID, streamID)
	stream := newTunnelStream(m, hostID, streamID, key, socket)
	m.streams[key] = stream
	m.mu.Unlock()

	payload, err := json.Marshal(TunnelTargetConfig{TargetHost: targetHost, TargetPort: targetPort})
	if err != nil {
		m.removeStream(key, stream)
		return nil, err
	}
	if err := socket.TunnelSend(TunnelEncodeFrame(TunnelFrameOpen, streamID, payload)); err != nil {
		m.removeStream(key, stream)
		return nil, err
	}
	return stream, nil
}

// HandleTunnelMessage mirrors the socket's `message` listener. The caller feeds
// every binary frame it reads; frames from a replaced socket are ignored.
func (m *TunnelManager) HandleTunnelMessage(hostID int32, socket TunnelSocket, data []byte) {
	if socket == nil {
		return
	}
	m.mu.Lock()
	current := m.activeTunnels[hostID]
	m.mu.Unlock()
	if !tunnelSameSocket(current, socket) {
		return
	}

	frame, err := TunnelDecodeFrame(data)
	if err != nil {
		log.Printf("[HostTunnelManager] Malformed frame from host %d: %v", hostID, err)
		return
	}
	m.handleIncomingFrame(hostID, frame)
}

func (m *TunnelManager) handleIncomingFrame(hostID int32, frame TunnelFrame) {
	streamKey := fmt.Sprintf("%d:%d", hostID, frame.StreamID)

	m.mu.Lock()
	stream := m.streams[streamKey]
	m.mu.Unlock()

	switch frame.Type {
	case TunnelFrameControlHello:
		if frame.StreamID != 0 || len(frame.Payload) > 4096 {
			return
		}
		var hello struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.Unmarshal(frame.Payload, &hello); err != nil {
			// never log control payloads
			return
		}
		capabilities := map[string]bool{}
		for _, capability := range hello.Capabilities {
			switch capability {
			case "incus_node_v1", "heartbeat-trigger-v1", "heartbeat-confirm-v1":
				capabilities[capability] = true
			}
		}

		m.mu.Lock()
		_, hadCapabilities := m.controlCapabilities[hostID]
		m.controlCapabilities[hostID] = capabilities
		m.mu.Unlock()

		if !hadCapabilities && capabilities["incus_node_v1"] {
			m.emitControlReady(hostID)
		}

	case TunnelFrameData:
		if stream != nil {
			stream.push(frame.Payload)
		}

	case TunnelFrameClose:
		if stream != nil {
			stream.pushEOF()
		}

	case TunnelFrameReset:
		if stream != nil {
			stream.reset(fmt.Errorf("Stream %d reset by remote agent", frame.StreamID))
		}

	case TunnelFrameOpen, TunnelFrameConfig, TunnelFrameLogControl:
		// The server only sends OPEN / CONFIG / LOG_CTL; ignore the agent echoing them.

	case TunnelFrameTaskLogData:
		if len(frame.Payload) > 10000 {
			return
		}
		var data struct {
			TaskID any     `json:"taskId"`
			Line   *string `json:"line"`
		}
		if err := json.Unmarshal(frame.Payload, &data); err != nil {
			return
		}
		taskID, ok := tunnelSafeInteger(data.TaskID)
		// `typeof data.line === 'string'`: a missing / null line is skipped.
		if !ok || data.Line == nil {
			return
		}
		m.emitTaskLog(hostID, taskID, tunnelSliceRunes(*data.Line, 8000))

	case TunnelFrameLogData:
		if len(frame.Payload) > 0 {
			line := string(frame.Payload)
			m.emitLogData(hostID, &line)
		} else {
			m.emitLogData(hostID, nil)
		}
	}
}

// tunnelSafeInteger mirrors Number.isSafeInteger for a decoded JSON value.
func tunnelSafeInteger(value any) (int64, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return 0, false
	}
	if math.Abs(number) > 9007199254740991 {
		return 0, false
	}
	return int64(number), true
}

// tunnelSliceRunes mirrors `line.slice(0, max)`. The TypeScript slices UTF-16
// code units; runes are used here so a multi-byte character is never split (the
// only difference is log text containing astral-plane characters).
func tunnelSliceRunes(value string, maxLength int) string {
	runes := []rune(value)
	if len(runes) <= maxLength {
		return value
	}
	return string(runes[:maxLength])
}

// ==================== duplex stream ====================

// TunnelStream is the Go equivalent of the `Duplex` the TypeScript hands to the
// TLS layer: a full-duplex net.Conn whose reads are fed by incoming DATA frames
// and whose writes are chunked into 64KB DATA frames.
type TunnelStream struct {
	manager  *TunnelManager
	hostID   int32
	streamID uint32
	key      string
	socket   TunnelSocket

	writeMu     sync.Mutex
	writeClosed bool

	mu            sync.Mutex
	buffer        bytes.Buffer
	eof           bool
	err           error
	readDeadline  time.Time
	writeDeadline time.Time
	notify        chan struct{}

	closeOnce sync.Once
}

func newTunnelStream(manager *TunnelManager, hostID int32, streamID uint32, key string, socket TunnelSocket) *TunnelStream {
	return &TunnelStream{
		manager:  manager,
		hostID:   hostID,
		streamID: streamID,
		key:      key,
		socket:   socket,
		notify:   make(chan struct{}, 1),
	}
}

func (s *TunnelStream) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// push mirrors `stream.push(payload)` for a DATA frame.
func (s *TunnelStream) push(payload []byte) {
	s.mu.Lock()
	if s.err != nil || s.eof {
		s.mu.Unlock()
		return
	}
	_, _ = s.buffer.Write(payload)
	s.mu.Unlock()
	s.signal()
}

// pushEOF mirrors `stream.push(null)` for a CLOSE frame.
func (s *TunnelStream) pushEOF() {
	s.mu.Lock()
	s.eof = true
	s.mu.Unlock()
	s.signal()
}

// reset mirrors `stream.destroy(err)` for a RESET frame / host disconnect.
func (s *TunnelStream) reset(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.signal()
	s.manager.removeStream(s.key, s)
}

// Read implements net.Conn.
func (s *TunnelStream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if s.buffer.Len() > 0 {
			count, _ := s.buffer.Read(p)
			s.mu.Unlock()
			return count, nil
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return 0, err
		}
		if s.eof {
			s.mu.Unlock()
			return 0, io.EOF
		}
		deadline := s.readDeadline
		s.mu.Unlock()

		if deadline.IsZero() {
			<-s.notify
			continue
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(wait)
		select {
		case <-s.notify:
			timer.Stop()
		case <-timer.C:
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// Write implements net.Conn: 64KB chunks, exactly like the TypeScript.
func (s *TunnelStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.writeClosed {
		return 0, errors.New("Tunnel stream is closed")
	}
	// A destroyed stream refuses writes (Duplex after destroy throws
	// ERR_STREAM_DESTROYED); this also covers a tunnel that went away while the
	// socket object itself still reports OPEN.
	s.mu.Lock()
	terminalErr := s.err
	s.mu.Unlock()
	if terminalErr != nil {
		return 0, terminalErr
	}
	if !s.writeDeadline.IsZero() && time.Now().After(s.writeDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if s.socket == nil || s.socket.TunnelReadyState() != TunnelSocketOpen {
		return 0, errors.New("Tunnel connection lost while writing")
	}

	const chunkSize = 64 * 1024
	written := 0
	for written < len(p) {
		end := written + chunkSize
		if end > len(p) {
			end = len(p)
		}
		if err := s.socket.TunnelSend(TunnelEncodeFrame(TunnelFrameData, s.streamID, p[written:end])); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

// Close sends the CLOSE frame (once) and deregisters the stream. Reads keep
// working until the peer closes (matching Duplex.end, which only ends writes).
func (s *TunnelStream) Close() error {
	s.closeOnce.Do(func() {
		s.writeMu.Lock()
		s.writeClosed = true
		s.writeMu.Unlock()

		if s.socket != nil && s.socket.TunnelReadyState() == TunnelSocketOpen {
			_ = s.socket.TunnelSend(TunnelEncodeFrame(TunnelFrameClose, s.streamID, nil))
		}
		s.manager.removeStream(s.key, s)
	})
	return nil
}

// LocalAddr implements net.Conn.
func (s *TunnelStream) LocalAddr() net.Addr { return tunnelAddr("tunnel") }

// RemoteAddr implements net.Conn.
func (s *TunnelStream) RemoteAddr() net.Addr {
	return tunnelAddr(fmt.Sprintf("host-%d:%d", s.hostID, s.streamID))
}

// SetDeadline implements net.Conn.
func (s *TunnelStream) SetDeadline(deadline time.Time) error {
	_ = s.SetReadDeadline(deadline)
	return s.SetWriteDeadline(deadline)
}

// SetReadDeadline implements net.Conn.
func (s *TunnelStream) SetReadDeadline(deadline time.Time) error {
	s.mu.Lock()
	s.readDeadline = deadline
	s.mu.Unlock()
	s.signal()
	return nil
}

// SetWriteDeadline implements net.Conn. Writes are synchronous, so the deadline
// can only be enforced before a send starts.
func (s *TunnelStream) SetWriteDeadline(deadline time.Time) error {
	s.writeMu.Lock()
	s.writeDeadline = deadline
	s.writeMu.Unlock()
	return nil
}

type tunnelAddr string

func (a tunnelAddr) Network() string { return "tunnel" }
func (a tunnelAddr) String() string  { return string(a) }

// ==================== node API requester ====================

// tunnelDefaultNodeRequest mirrors requestNodeAPI: resolve the host, reuse the
// pooled mTLS Incus client, then call the native payincus endpoints.
func tunnelDefaultNodeRequest(ctx context.Context, hostID int32, method TunnelControlMethod, params any) (any, error) {
	if db.DB == nil {
		return nil, errors.New("Node not found")
	}
	var host models.Host
	err := db.DB.WithContext(ctx).Where("id = ?", hostID).Take(&host).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("Node not found")
	}
	if err != nil {
		return nil, err
	}

	client, err := GetIncusClient(ctx, IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
	if err != nil {
		return nil, err
	}

	return TunnelCallNodeAPI(ctx, client, method, params)
}

// TunnelCallNodeAPI mirrors callNodeAPI: the node API extension must be
// advertised before any control call is sent.
func TunnelCallNodeAPI(ctx context.Context, client *IncusClient, method TunnelControlMethod, params any) (any, error) {
	infoRaw, err := client.Request(ctx, http.MethodGet, "/1.0/agent", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	var info struct {
		APIExtensions []string `json:"api_extensions"`
	}
	if err := json.Unmarshal(infoRaw, &info); err != nil {
		return nil, errors.New("Node API extension unavailable")
	}
	available := false
	for _, extension := range info.APIExtensions {
		if extension == "incus_node_v1" {
			available = true
			break
		}
	}
	if !available {
		return nil, errors.New("Node API extension unavailable")
	}

	verb := http.MethodPut
	path := "/1.0/agent/network"
	if method == TunnelControlProxyProbe {
		verb = http.MethodPost
		path = "/1.0/agent/proxy-probes"
	}
	raw, err := client.Request(ctx, verb, path, params, 0, params)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, NewIncusAPIError(err.Error(), 502, params)
	}
	return value, nil
}

// ==================== status synchronisation ====================

// TunnelEffectiveHostStatusInput mirrors the object deriveEffectiveHostStatus
// takes. Both casing variants are accepted because callers pass either a Prisma
// row or a snake_case host record.
type TunnelEffectiveHostStatusInput struct {
	ID                 int32
	Status             string
	TunnelEnabled      *bool
	TunnelEnabledSnake *bool
}

// TunnelDeriveEffectiveHostStatus mirrors deriveEffectiveHostStatus: in tunnel
// mode a host without a live tunnel is effectively offline (maintenance always
// wins because an admin chose it explicitly).
func TunnelDeriveEffectiveHostStatus(host TunnelEffectiveHostStatusInput) string {
	if host.Status == string(models.HostStatusMaintenance) {
		return string(models.HostStatusMaintenance)
	}
	enabled := false
	if host.TunnelEnabled != nil {
		enabled = *host.TunnelEnabled
	} else if host.TunnelEnabledSnake != nil {
		enabled = *host.TunnelEnabledSnake
	}
	if enabled && !TunnelManagerInstance().IsTunnelOnline(host.ID) {
		return string(models.HostStatusOffline)
	}
	return host.Status
}

// tunnelSyncHostStatus mirrors syncTunnelHostStatus: only tunnel-mode hosts are
// touched, maintenance is never overwritten, and the status only moves
// online <-> offline.
func tunnelSyncHostStatus(hostID int32, isOnline bool) {
	if db.DB == nil {
		return
	}
	ctx := context.Background()

	var row struct {
		Status        string `gorm:"column:status"`
		TunnelEnabled bool   `gorm:"column:tunnel_enabled"`
	}
	err := db.DB.WithContext(ctx).
		Table("hosts").
		Select("status::text AS status, tunnel_enabled").
		Where("id = ?", hostID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}
	if err != nil {
		log.Printf("[HostTunnel] Failed to sync status for host %d: %v", hostID, err)
		return
	}
	if !row.TunnelEnabled {
		return
	}
	// 维护模式由管理员显式设置，不自动覆盖
	if row.Status == string(models.HostStatusMaintenance) {
		return
	}

	target := string(models.HostStatusOffline)
	if isOnline {
		target = string(models.HostStatusOnline)
	}
	if row.Status == target {
		return
	}

	err = db.DB.WithContext(ctx).
		Table("hosts").
		Where("id = ?", hostID).
		Updates(map[string]any{"status": target, "updated_at": time.Now().UTC().Truncate(time.Millisecond)}).Error
	if err != nil {
		log.Printf("[HostTunnel] Failed to sync status for host %d: %v", hostID, err)
		return
	}
	log.Printf("[HostTunnel] Host %d tunnel %s, status updated to %s", hostID, map[bool]string{true: "connected", false: "disconnected"}[isOnline], target)
}

// TunnelReconcileHostsStatus mirrors reconcileTunnelHostsStatus: correctness pass
// for tunnel hosts whose status says online but whose tunnel is gone. Returns the
// number of corrected rows; a database failure returns 0 (the TypeScript swallows
// the error as well).
func TunnelReconcileHostsStatus(ctx context.Context) int {
	if db.DB == nil {
		return 0
	}

	var hosts []struct {
		ID int32 `gorm:"column:id"`
	}
	if err := db.DB.WithContext(ctx).
		Table("hosts").
		Select("id").
		Where("tunnel_enabled = ? AND status = ?", true, string(models.HostStatusOnline)).
		Find(&hosts).Error; err != nil {
		log.Printf("[HostTunnel] Failed to reconcile tunnel hosts status: %v", err)
		return 0
	}

	corrected := 0
	for _, host := range hosts {
		if TunnelManagerInstance().IsTunnelOnline(host.ID) {
			continue
		}
		err := db.DB.WithContext(ctx).
			Table("hosts").
			Where("id = ?", host.ID).
			Updates(map[string]any{
				"status":     string(models.HostStatusOffline),
				"updated_at": time.Now().UTC().Truncate(time.Millisecond),
			}).Error
		if err != nil {
			log.Printf("[HostTunnel] Failed to reconcile tunnel hosts status: %v", err)
			return corrected
		}
		corrected++
	}
	if corrected > 0 {
		log.Printf("[HostTunnel] Reconciled %d offline tunnel host(s) to status 'offline'", corrected)
	}
	return corrected
}

// ==================== wiring ====================

// hostsStorageCapacityFromStoragePolicy bridges db/hosts.go's storage-capacity
// provider hook to lib/storage_capacity.go (getStorageCapacity).
//
// db must not import lib, and lib's storage port needs a pgx querier
// (pgxpool.Pool / pgx.Tx) while db/hosts.go hands over its GORM handle, so the
// capacity read goes through db.Pool() rather than the caller's *gorm.DB. With
// PostgreSQL's default READ COMMITTED isolation this observes the same committed
// rows a statement inside the caller's transaction would (Prisma's interactive
// transactions are READ COMMITTED too), and the host row is already locked with
// FOR UPDATE before the call, so concurrent selections on the same host are
// still serialised.
//
// (This glue lives here because the hosts data layer may only add db/hosts.go and
// lib/tunnel_manager.go in this wave; moving it to lib/hosts_storage_provider.go
// later is behaviour-neutral.)
func hostsStorageCapacityFromStoragePolicy(
	ctx context.Context,
	_ *gorm.DB,
	hostID int32,
	options db.HostsStorageCapacityOptions,
) (db.ResourcePoolStorageCapacity, error) {
	capacity, err := GetStorageCapacity(ctx, hostID, StorageCapacityOptions{
		PackageID:  options.PackageID,
		PoolName:   options.PoolName,
		RequiredMB: options.RequiredMB,
	})
	if err != nil {
		return db.ResourcePoolStorageCapacity{}, err
	}

	var reason *string
	if capacity.Reason != nil {
		value := string(*capacity.Reason)
		reason = &value
	}
	return db.ResourcePoolStorageCapacity{
		PoolName:    capacity.PoolName,
		TotalMB:     capacity.TotalMB,
		UsedMB:      capacity.UsedMB,
		AllocatedMB: capacity.AllocatedMB,
		AvailableMB: capacity.AvailableMB,
		HeadroomMB:  capacity.HeadroomMB,
		Reason:      reason,
	}, nil
}

func init() {
	// The remaining seam an earlier module left for this wave. The Caddy seams now
	// default to the tunnel manager in caddy_client.go itself.
	TunnelLogControl = func(hostID int64, taskID int32, name, action string) bool {
		return TunnelManagerInstance().SendTaskLogControl(int32(hostID), taskID, name, TunnelLogAction(action))
	}

	// `hostTunnelManager.on('tunnel_connected' | 'tunnel_disconnected', …)`.
	//
	// The Node listeners are `void syncTunnelHostStatus(...)`: they run detached,
	// so a slow UPDATE never blocks the websocket handler. Go keeps that.
	TunnelManagerInstance().OnTunnelConnected(func(hostID int32) {
		go tunnelSyncHostStatus(hostID, true)
	})
	TunnelManagerInstance().OnTunnelDisconnected(func(hostID int32) {
		go tunnelSyncHostStatus(hostID, false)
	})

	// host selection's storage gate (db/hosts.go) and lib/storage_capacity.go's
	// own tunnel seam: both are `hostTunnelManager` / `getStorageCapacity`
	// wiring, which belongs to this wave.
	db.RegisterHostsStorageCapacityProvider(hostsStorageCapacityFromStoragePolicy)
	StorageCapacityTunnelIsOnline = func(hostID int32) bool {
		return TunnelManagerInstance().IsTunnelOnline(hostID)
	}

	// Instance creation validates package egress inside its own pgx transaction
	// (db cannot import lib), so db exposes the capability check as a variable.
	// Until it is set, a non-'host' egress mode is refused rather than bound
	// without a node that can enforce it.
	db.InstanceEgressHostSupportsControl = func(hostID int32, capability string) bool {
		return EgressPackageTunnelSupportsControl(hostID, capability)
	}
}
