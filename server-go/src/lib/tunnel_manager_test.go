// Unit tests for the lib/tunnel_manager.go port.
//
// These are the Go translation of server/scripts/test-agent-tunnel-protocol-guards.ts
// plus the state machine branches that guard would hide behind a live socket:
//
//   - frame codec (5-byte header, big-endian stream id, short-frame error);
//   - registration / replacement / close cleanup (start, stop, abnormal exit);
//   - control RPC gating (UPGRADE_REQUIRED, BUSY, TIMEOUT, CANCELLED,
//     DISCONNECTED, OFFLINE, INVALID_RESPONSE and the 409/504 remapping);
//   - heartbeat round-trip (409/502/504 branches);
//   - log/task-log control frames and inbound log events;
//   - the duplex net.Conn (OPEN, DATA chunking, CLOSE/EOF, RESET, deadlines);
//   - deriveEffectiveHostStatus and the seam wiring to Caddy/operation-task-log.
//
// The socket is a mock because the Go port owns no WebSocket library; see the
// package comment in tunnel_manager.go.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// tunnelTestSocket is a WebSocketLike stand-in.
type tunnelTestSocket struct {
	mu          sync.Mutex
	readyState  int
	frames      [][]byte
	sendErr     error
	closeCode   int
	closeReason string
	hostID      int32
}

func newTunnelTestSocket() *tunnelTestSocket {
	return &tunnelTestSocket{readyState: TunnelSocketOpen}
}

func (s *tunnelTestSocket) TunnelReadyState() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyState
}

func (s *tunnelTestSocket) TunnelSend(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	s.frames = append(s.frames, copied)
	return nil
}

func (s *tunnelTestSocket) TunnelClose(code int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyState = 3
	s.closeCode = code
	s.closeReason = reason
}

func (s *tunnelTestSocket) sentFrames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames := make([][]byte, len(s.frames))
	copy(frames, s.frames)
	return frames
}

func (s *tunnelTestSocket) sentFramesOfType(frameType TunnelFrameType) []TunnelFrame {
	decoded := make([]TunnelFrame, 0, 1)
	for _, raw := range s.sentFrames() {
		frame, err := TunnelDecodeFrame(raw)
		if err != nil {
			continue
		}
		if frame.Type == frameType {
			decoded = append(decoded, frame)
		}
	}
	return decoded
}

func (s *tunnelTestSocket) waitForFrame(t *testing.T, frameType TunnelFrameType, count int) []TunnelFrame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frames := s.sentFramesOfType(frameType); len(frames) >= count {
			return frames
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frame(s) of type %#x", count, byte(frameType))
	return nil
}

func tunnelTestDecode(t *testing.T, raw []byte) TunnelFrame {
	t.Helper()
	frame, err := TunnelDecodeFrame(raw)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return frame
}

func TestTunnelFrameCodec(t *testing.T) {
	payload := []byte("hello-mTLS-bytes")
	frame := TunnelEncodeFrame(TunnelFrameData, 101, payload)
	if len(frame) != 5+len(payload) {
		t.Fatalf("frame length = %d", len(frame))
	}
	decoded, err := TunnelDecodeFrame(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Type != TunnelFrameData || decoded.StreamID != 101 || string(decoded.Payload) != string(payload) {
		t.Fatalf("decoded = %+v", decoded)
	}

	// Big-endian stream id (0x01020304).
	header := TunnelEncodeFrame(TunnelFrameOpen, 0x01020304, nil)
	if header[1] != 0x01 || header[2] != 0x02 || header[3] != 0x03 || header[4] != 0x04 {
		t.Fatalf("stream id is not big-endian: %v", header)
	}
	if len(TunnelEncodeFrame(TunnelFrameClose, 7, nil)) != 5 {
		t.Fatalf("empty payload must still be 5 bytes")
	}

	if _, err := TunnelDecodeFrame([]byte{0x01, 0x00}); err == nil ||
		!strings.Contains(err.Error(), "Frame too short: expected at least 5 bytes, got 2") {
		t.Fatalf("short frame err = %v", err)
	}
}

func TestTunnelManagerRegistrationAndConfig(t *testing.T) {
	manager := NewTunnelManager(nil)
	hostID := int32(777)
	socket := newTunnelTestSocket()

	if manager.IsTunnelOnline(hostID) {
		t.Fatalf("a fresh manager must not report the host online")
	}

	var connected []int32
	manager.OnTunnelConnected(func(id int32) { connected = append(connected, id) })

	manager.RegisterTunnel(hostID, socket)
	if !manager.IsTunnelOnline(hostID) {
		t.Fatalf("host must be online after RegisterTunnel")
	}
	if len(connected) != 1 || connected[0] != hostID {
		t.Fatalf("tunnel_connected = %v", connected)
	}

	// The first frame is the CONTROL_HELLO probe.
	frames := socket.sentFrames()
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want 1", len(frames))
	}
	if hello := tunnelTestDecode(t, frames[0]); hello.Type != TunnelFrameControlHello || hello.StreamID != 0 || len(hello.Payload) != 0 {
		t.Fatalf("hello frame = %+v", hello)
	}

	// BroadcastConfig.
	if !manager.BroadcastConfig(hostID, TunnelTargetConfig{TargetHost: "127.0.0.1", TargetPort: 8443}) {
		t.Fatalf("BroadcastConfig = false")
	}
	configFrames := socket.sentFramesOfType(TunnelFrameConfig)
	if len(configFrames) != 1 || configFrames[0].StreamID != 0 {
		t.Fatalf("config frames = %+v", configFrames)
	}
	var config map[string]any
	if err := json.Unmarshal(configFrames[0].Payload, &config); err != nil {
		t.Fatalf("config payload: %v", err)
	}
	if config["targetHost"] != "127.0.0.1" || config["targetPort"] != float64(8443) {
		t.Fatalf("config payload = %v", config)
	}

	// CreateDuplexStream sends OPEN with a positive stream id.
	stream, err := manager.CreateDuplexStream(hostID, "127.0.0.1", 8443)
	if err != nil {
		t.Fatalf("CreateDuplexStream: %v", err)
	}
	openFrames := socket.sentFramesOfType(TunnelFrameOpen)
	if len(openFrames) != 1 || openFrames[0].StreamID == 0 {
		t.Fatalf("open frames = %+v", openFrames)
	}
	streamID := openFrames[0].StreamID
	var openPayload map[string]any
	if err := json.Unmarshal(openFrames[0].Payload, &openPayload); err != nil {
		t.Fatalf("open payload: %v", err)
	}
	if openPayload["targetHost"] != "127.0.0.1" || openPayload["targetPort"] != float64(8443) {
		t.Fatalf("open payload = %v", openPayload)
	}

	// Writing to the stream emits DATA frames with that stream id.
	if _, err := stream.Write([]byte("client-request-payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	dataFrames := socket.sentFramesOfType(TunnelFrameData)
	if len(dataFrames) != 1 || dataFrames[0].StreamID != streamID || string(dataFrames[0].Payload) != "client-request-payload" {
		t.Fatalf("data frames = %+v", dataFrames)
	}

	// An inbound DATA frame feeds the reader.
	manager.HandleTunnelMessage(hostID, socket, TunnelEncodeFrame(TunnelFrameData, streamID, []byte("agent-response-payload")))
	buffer := make([]byte, 64)
	count, err := stream.Read(buffer)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buffer[:count]) != "agent-response-payload" {
		t.Fatalf("read = %q", buffer[:count])
	}

	// Closing the stream sends CLOSE.
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closeFrames := socket.sentFramesOfType(TunnelFrameClose)
	if len(closeFrames) != 1 || closeFrames[0].StreamID != streamID {
		t.Fatalf("close frames = %+v", closeFrames)
	}

	// The peer closing the stream produces EOF for the reader.
	second, err := manager.CreateDuplexStream(hostID, "", 0)
	if err != nil {
		t.Fatalf("CreateDuplexStream: %v", err)
	}
	secondOpen := socket.sentFramesOfType(TunnelFrameOpen)
	secondID := secondOpen[len(secondOpen)-1].StreamID
	manager.HandleTunnelMessage(hostID, socket, TunnelEncodeFrame(TunnelFrameClose, secondID, nil))
	if _, err := second.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("read after CLOSE err = %v, want io.EOF", err)
	}
	// Defaults: empty targetHost / zero port become 127.0.0.1:8443.
	var secondPayload map[string]any
	if err := json.Unmarshal(secondOpen[len(secondOpen)-1].Payload, &secondPayload); err != nil {
		t.Fatalf("open payload: %v", err)
	}
	if secondPayload["targetHost"] != "127.0.0.1" || secondPayload["targetPort"] != float64(8443) {
		t.Fatalf("default target = %v", secondPayload)
	}

	// Disconnection.
	var disconnected []int32
	manager.OnTunnelDisconnected(func(id int32) { disconnected = append(disconnected, id) })
	manager.HandleTunnelClosed(hostID, socket)
	if manager.IsTunnelOnline(hostID) {
		t.Fatalf("host must be offline after the socket closed")
	}
	if len(disconnected) != 1 || disconnected[0] != hostID {
		t.Fatalf("tunnel_disconnected = %v", disconnected)
	}
	if _, ok := manager.GetTunnel(hostID); ok {
		t.Fatalf("GetTunnel must report no tunnel")
	}
	// The stream that was still open is destroyed with the tunnel.
	if _, err := second.Read(buffer); err == nil {
		t.Fatalf("an open stream must fail once the tunnel closes")
	}
}

func TestTunnelManagerDeferredStreamWrites(t *testing.T) {
	manager := NewTunnelManager(nil)
	hostID := int32(12)
	socket := newTunnelTestSocket()
	manager.RegisterTunnel(hostID, socket)

	stream, err := manager.CreateDuplexStream(hostID, "", 0)
	if err != nil {
		t.Fatalf("CreateDuplexStream: %v", err)
	}
	// 150KB → 64KB + 64KB + 22KB.
	payload := make([]byte, 150*1024)
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	dataFrames := socket.sentFramesOfType(TunnelFrameData)
	if len(dataFrames) != 3 {
		t.Fatalf("data frames = %d, want 3", len(dataFrames))
	}
	if len(dataFrames[0].Payload) != 64*1024 || len(dataFrames[1].Payload) != 64*1024 || len(dataFrames[2].Payload) != 22*1024 {
		t.Fatalf("chunk sizes = %d/%d/%d", len(dataFrames[0].Payload), len(dataFrames[1].Payload), len(dataFrames[2].Payload))
	}

	// A RESET frame destroys the stream with "reset by remote agent".
	manager.HandleTunnelMessage(hostID, socket, TunnelEncodeFrame(TunnelFrameReset, dataFrames[0].StreamID, nil))
	if _, err := stream.Read(make([]byte, 8)); err == nil ||
		!strings.Contains(err.Error(), "reset by remote agent") {
		t.Fatalf("read after RESET err = %v", err)
	}

	// Read deadline in the past fails immediately.
	other, err := manager.CreateDuplexStream(hostID, "", 0)
	if err != nil {
		t.Fatalf("CreateDuplexStream: %v", err)
	}
	if err := other.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := other.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read err = %v, want deadline exceeded", err)
	}

	// Writing after the tunnel is gone fails (not a silent drop).
	manager.HandleTunnelClosed(hostID, socket)
	if _, err := other.Write([]byte("x")); err == nil {
		t.Fatalf("write after disconnect must fail")
	}
}

func TestTunnelManagerControlGating(t *testing.T) {
	t.Run("offline host", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		_, err := manager.RequestControl(context.Background(), 1, TunnelControlNetworkApply, nil, time.Second)
		if !tunnelIsControlError(err, TunnelControlOffline) {
			t.Fatalf("err = %v, want OFFLINE", err)
		}
	})

	t.Run("retired capabilities never enable control", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(1, socket)
		manager.HandleTunnelMessage(1, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["network.apply","proxy.probe"]}`)))
		if manager.SupportsControl(1, TunnelControlNetworkApply) {
			t.Fatalf("legacy capability names must not enable node control")
		}
		if _, err := manager.RequestControl(context.Background(), 1, TunnelControlNetworkApply, nil, time.Second); !tunnelIsControlError(err, TunnelControlUpgradeRequired) {
			t.Fatalf("err = %v, want UPGRADE_REQUIRED", err)
		}
	})

	t.Run("native routing", func(t *testing.T) {
		type call struct {
			hostID int32
			method TunnelControlMethod
			params any
		}
		var calls []call
		manager := NewTunnelManager(func(_ context.Context, hostID int32, method TunnelControlMethod, params any) (any, error) {
			calls = append(calls, call{hostID: hostID, method: method, params: params})
			return map[string]any{"appliedRevision": 7}, nil
		})
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(8101, socket)
		manager.HandleTunnelMessage(8101, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["payincus_node_v1"]}`)))
		if !manager.SupportsControl(8101, TunnelControlNetworkApplyV2) {
			t.Fatalf("payincus_node_v1 must enable control")
		}

		params := map[string]any{"schemaVersion": 2, "revision": 7}
		value, err := manager.RequestControl(context.Background(), 8101, TunnelControlNetworkApplyV2, params, time.Second)
		if err != nil {
			t.Fatalf("RequestControl: %v", err)
		}
		response, ok := value.(map[string]any)
		if !ok || response["appliedRevision"] != 7 {
			t.Fatalf("value = %#v", value)
		}
		if len(calls) != 1 || calls[0].hostID != 8101 {
			t.Fatalf("calls = %+v", calls)
		}
		// No business-RPC frame is written to the socket (the call goes over the
		// Incus client, not the tunnel).
		if len(socket.sentFramesOfType(TunnelFrameType(0x08))) != 0 {
			t.Fatalf("RequestControl must not emit a business RPC frame")
		}
		manager.UnregisterTunnel(8101, nil)
	})

	t.Run("remote status mapping", func(t *testing.T) {
		cases := []struct {
			status int
			code   TunnelControlErrorCode
		}{
			{http.StatusConflict, TunnelControlBusy},
			{http.StatusGatewayTimeout, TunnelControlTimeout},
			{http.StatusBadGateway, TunnelControlRemoteError},
			{http.StatusInternalServerError, TunnelControlRemoteError},
		}
		for _, testCase := range cases {
			manager := NewTunnelManager(func(context.Context, int32, TunnelControlMethod, any) (any, error) {
				return nil, NewIncusAPIError("boom", testCase.status, nil)
			})
			socket := newTunnelTestSocket()
			manager.RegisterTunnel(3, socket)
			manager.HandleTunnelMessage(3, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
				[]byte(`{"capabilities":["payincus_node_v1"]}`)))
			_, err := manager.RequestControl(context.Background(), 3, TunnelControlProxyProbe, nil, time.Second)
			if !tunnelIsControlError(err, testCase.code) {
				t.Fatalf("status %d: err = %v, want %s", testCase.status, err, testCase.code)
			}
			manager.UnregisterTunnel(3, nil)
		}
	})

	t.Run("timeout and cancellation", func(t *testing.T) {
		manager := NewTunnelManager(func(ctx context.Context, _ int32, _ TunnelControlMethod, _ any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(88, socket)
		manager.HandleTunnelMessage(88, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["payincus_node_v1"]}`)))

		if _, err := manager.RequestControl(context.Background(), 88, TunnelControlProxyProbe, nil, 5*time.Millisecond); !tunnelIsControlError(err, TunnelControlTimeout) {
			t.Fatalf("err = %v, want TIMEOUT", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		pending := make(chan error, 1)
		go func() {
			_, err := manager.RequestControl(ctx, 88, TunnelControlProxyProbe, nil, time.Second)
			pending <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		if err := <-pending; !tunnelIsControlError(err, TunnelControlCancelled) {
			t.Fatalf("err = %v, want CANCELLED", err)
		}
		manager.UnregisterTunnel(88, nil)
	})

	t.Run("bounded concurrency and disconnect cleanup", func(t *testing.T) {
		manager := NewTunnelManager(func(ctx context.Context, _ int32, _ TunnelControlMethod, _ any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(88, socket)
		manager.HandleTunnelMessage(88, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["payincus_node_v1"]}`)))

		results := make(chan error, 4)
		for index := 0; index < 4; index++ {
			go func() {
				_, err := manager.RequestControl(context.Background(), 88, TunnelControlProxyProbe, nil, 5*time.Second)
				results <- err
			}()
		}

		deadline := time.Now().Add(2 * time.Second)
		for {
			manager.mu.Lock()
			pending := len(manager.pendingControls)
			manager.mu.Unlock()
			if pending == 4 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pending controls = %d, want 4", pending)
			}
			time.Sleep(2 * time.Millisecond)
		}

		if _, err := manager.RequestControl(context.Background(), 88, TunnelControlProxyProbe, nil, time.Second); !tunnelIsControlError(err, TunnelControlBusy) {
			t.Fatalf("5th control err = %v, want BUSY", err)
		}

		manager.UnregisterTunnel(88, nil)
		for index := 0; index < 4; index++ {
			if err := <-results; !tunnelIsControlError(err, TunnelControlDisconnected) {
				t.Fatalf("pending control err = %v, want DISCONNECTED", err)
			}
		}

		manager.mu.Lock()
		pending := len(manager.pendingControls)
		manager.mu.Unlock()
		if pending != 0 {
			t.Fatalf("pending controls after disconnect = %d, want 0", pending)
		}
	})

	t.Run("payload limit", func(t *testing.T) {
		manager := NewTunnelManager(func(context.Context, int32, TunnelControlMethod, any) (any, error) {
			return nil, nil
		})
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(5, socket)
		manager.HandleTunnelMessage(5, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["payincus_node_v1"]}`)))
		huge := strings.Repeat("x", 1024*1024)
		if _, err := manager.RequestControl(context.Background(), 5, TunnelControlNetworkApply, map[string]any{"blob": huge}, time.Second); !tunnelIsControlError(err, TunnelControlInvalidResponse) {
			t.Fatalf("err = %v, want INVALID_RESPONSE", err)
		}
	})

	t.Run("control_ready fires once per tunnel", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		ready := 0
		manager.OnControlReady(func(int32) { ready++ })
		manager.RegisterTunnel(6, socket)
		hello := TunnelEncodeFrame(TunnelFrameControlHello, 0, []byte(`{"capabilities":["payincus_node_v1","heartbeat-trigger-v1","bogus"]}`))
		manager.HandleTunnelMessage(6, socket, hello)
		manager.HandleTunnelMessage(6, socket, hello)
		if ready != 1 {
			t.Fatalf("control_ready count = %d, want 1", ready)
		}

		// A hello with only retired capabilities must not emit control_ready.
		manager2 := NewTunnelManager(nil)
		socket2 := newTunnelTestSocket()
		fired := false
		manager2.OnControlReady(func(int32) { fired = true })
		manager2.RegisterTunnel(7, socket2)
		manager2.HandleTunnelMessage(7, socket2, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["network.apply"]}`)))
		if fired {
			t.Fatalf("retired capabilities must not emit control_ready")
		}

		// Oversized hello payloads are ignored.
		manager2.HandleTunnelMessage(7, socket2, TunnelEncodeFrame(TunnelFrameControlHello, 0, make([]byte, 4097)))
		if manager2.SupportsControl(7, TunnelControlNetworkApply) {
			t.Fatalf("oversized hello must be ignored")
		}
	})
}

func tunnelIsControlError(err error, code TunnelControlErrorCode) bool {
	var control *TunnelControlError
	if !errors.As(err, &control) {
		return false
	}
	return control.Code == code
}

func TestTunnelManagerHeartbeat(t *testing.T) {
	t.Run("no tunnel", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		err := manager.RequestHeartbeatAndWait(context.Background(), 1, time.Second)
		if !tunnelIsHeartbeatError(err, 409, "节点连接不可用，上报测试失败") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(1, socket)
		manager.HandleTunnelMessage(1, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["payincus_node_v1","heartbeat-trigger-v1"]}`)))
		// Trigger requires heartbeat-trigger-v1; it is present.
		if !manager.RequestHeartbeat(1) {
			t.Fatalf("RequestHeartbeat = false with heartbeat-trigger-v1")
		}
		err := manager.RequestHeartbeatAndWait(context.Background(), 1, time.Second)
		if !tunnelIsHeartbeatError(err, 409, "节点版本不支持上报结果确认，请更新节点集成包") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(2, socket)
		manager.HandleTunnelMessage(2, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["payincus_node_v1","heartbeat-trigger-v1","heartbeat-confirm-v1"]}`)))
		if !manager.RequestHeartbeat(2) {
			t.Fatalf("RequestHeartbeat = false")
		}

		done := make(chan error, 1)
		go func() { done <- manager.RequestHeartbeatAndWait(context.Background(), 2, 2*time.Second) }()

		frames := socket.waitForFrame(t, TunnelFrameHeartbeat, 2) // trigger + wait request
		var payload struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(frames[len(frames)-1].Payload, &payload); err != nil || payload.RequestID == "" {
			t.Fatalf("heartbeat payload = %s (%v)", frames[len(frames)-1].Payload, err)
		}

		// Duplicate in-flight heartbeat for the same host is rejected.
		if err := manager.RequestHeartbeatAndWait(context.Background(), 2, time.Second); !tunnelIsHeartbeatError(err, 409, "此节点正在执行上报测试，请等待结果") {
			t.Fatalf("duplicate err = %v", err)
		}

		// Wrong host / non-string id are ignored.
		manager.CompleteHeartbeat(99, payload.RequestID, "")
		manager.CompleteHeartbeat(2, 1234, "")
		select {
		case err := <-done:
			t.Fatalf("complete must not settle for a wrong host, got %v", err)
		case <-time.After(20 * time.Millisecond):
		}

		manager.CompleteHeartbeat(2, payload.RequestID, "")
		if err := <-done; err != nil {
			t.Fatalf("err = %v, want success", err)
		}
		manager.UnregisterTunnel(2, nil)
	})

	t.Run("report failure message", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(4, socket)
		manager.HandleTunnelMessage(4, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["heartbeat-confirm-v1"]}`)))
		done := make(chan error, 1)
		go func() { done <- manager.RequestHeartbeatAndWait(context.Background(), 4, 2*time.Second) }()
		frames := socket.waitForFrame(t, TunnelFrameHeartbeat, 1)
		var payload struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(frames[0].Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		manager.CompleteHeartbeat(4, payload.RequestID, "本次节点上报未能保存，测试失败")
		if !tunnelIsHeartbeatError(<-done, 502, "本次节点上报未能保存，测试失败") {
			t.Fatalf("report failure must surface with status 502")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(5, socket)
		manager.HandleTunnelMessage(5, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["heartbeat-confirm-v1"]}`)))
		err := manager.RequestHeartbeatAndWait(context.Background(), 5, 5*time.Millisecond)
		if !tunnelIsHeartbeatError(err, 504, "等待本次节点上报超时，测试失败") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("disconnect fails the wait", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(6, socket)
		manager.HandleTunnelMessage(6, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["heartbeat-confirm-v1"]}`)))
		done := make(chan error, 1)
		go func() { done <- manager.RequestHeartbeatAndWait(context.Background(), 6, 2*time.Second) }()
		socket.waitForFrame(t, TunnelFrameHeartbeat, 1)
		manager.UnregisterTunnel(6, nil)
		if err := <-done; !tunnelIsHeartbeatError(err, 502, "节点连接已断开，上报测试失败") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("send failure", func(t *testing.T) {
		manager := NewTunnelManager(nil)
		socket := newTunnelTestSocket()
		manager.RegisterTunnel(7, socket)
		manager.HandleTunnelMessage(7, socket, TunnelEncodeFrame(TunnelFrameControlHello, 0,
			[]byte(`{"capabilities":["heartbeat-confirm-v1"]}`)))
		socket.mu.Lock()
		socket.sendErr = errors.New("socket gone")
		socket.mu.Unlock()
		err := manager.RequestHeartbeatAndWait(context.Background(), 7, time.Second)
		if !tunnelIsHeartbeatError(err, 502, "无法发送本次上报请求，测试失败") {
			t.Fatalf("err = %v", err)
		}
		// RequestHeartbeat reports false when the send throws.
		if manager.RequestHeartbeat(7) {
			t.Fatalf("RequestHeartbeat must be false when the send fails")
		}
	})
}

func tunnelIsHeartbeatError(err error, status int, message string) bool {
	var heartbeat *TunnelHeartbeatTestError
	if !errors.As(err, &heartbeat) {
		return false
	}
	return heartbeat.StatusCode == status && heartbeat.Message == message
}

func TestTunnelManagerLogFrames(t *testing.T) {
	manager := NewTunnelManager(nil)
	socket := newTunnelTestSocket()
	manager.RegisterTunnel(9, socket)

	// SendLogControl: lines are only included when truthy.
	if !manager.SendLogControl(9, TunnelLogActionStart, hostsInt32ForTunnelTest(100)) {
		t.Fatalf("SendLogControl = false")
	}
	if !manager.SendLogControl(9, TunnelLogActionStop, hostsInt32ForTunnelTest(0)) {
		t.Fatalf("SendLogControl = false")
	}
	logFrames := socket.sentFramesOfType(TunnelFrameLogControl)
	if len(logFrames) != 2 || logFrames[0].StreamID != 0 {
		t.Fatalf("log frames = %+v", logFrames)
	}
	var startPayload map[string]any
	if err := json.Unmarshal(logFrames[0].Payload, &startPayload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if startPayload["action"] != "start" || startPayload["lines"] != float64(100) {
		t.Fatalf("start payload = %v", startPayload)
	}
	var stopPayload map[string]any
	if err := json.Unmarshal(logFrames[1].Payload, &stopPayload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, ok := stopPayload["lines"]; ok {
		t.Fatalf("lines=0 must be omitted (truthiness), got %v", stopPayload)
	}

	// Task log control.
	if !manager.SendTaskLogControl(9, 42, "create instance", TunnelLogActionStart) {
		t.Fatalf("SendTaskLogControl = false")
	}
	taskFrames := socket.sentFramesOfType(TunnelFrameTaskLogControl)
	if len(taskFrames) != 1 {
		t.Fatalf("task log frames = %+v", taskFrames)
	}
	var taskPayload map[string]any
	if err := json.Unmarshal(taskFrames[0].Payload, &taskPayload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if taskPayload["taskId"] != float64(42) || taskPayload["name"] != "create instance" || taskPayload["action"] != "start" {
		t.Fatalf("task payload = %v", taskPayload)
	}

	// Inbound log lines.
	lines := make([]*string, 0, 2)
	manager.OnLogData(func(_ int32, line *string) { lines = append(lines, line) })
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameLogData, 0, []byte("line one")))
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameLogData, 0, nil))
	if len(lines) != 2 || lines[0] == nil || *lines[0] != "line one" || lines[1] != nil {
		t.Fatalf("log data = %+v", lines)
	}

	// Inbound task log lines.
	type taskLine struct {
		taskID int64
		line   string
	}
	var taskLines []taskLine
	manager.OnTaskLog(func(_ int32, taskID int64, line string) { taskLines = append(taskLines, taskLine{taskID, line}) })
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameTaskLogData, 0, []byte(`{"taskId":7,"line":"step 1"}`)))
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameTaskLogData, 0, []byte(`{"taskId":7.5,"line":"fraction"}`)))
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameTaskLogData, 0, []byte(`{"taskId":"7","line":"string id"}`)))
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameTaskLogData, 0, []byte(`{"taskId":7}`)))
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameTaskLogData, 0, make([]byte, 10001)))
	if len(taskLines) != 1 || taskLines[0].taskID != 7 || taskLines[0].line != "step 1" {
		t.Fatalf("task lines = %+v", taskLines)
	}

	// Long lines are truncated to 8000 runes.
	long := strings.Repeat("a", 9000)
	manager.HandleTunnelMessage(9, socket, TunnelEncodeFrame(TunnelFrameTaskLogData, 0, []byte(`{"taskId":8,"line":"`+long+`"}`)))
	if len(taskLines) != 2 || len([]rune(taskLines[1].line)) != 8000 {
		t.Fatalf("truncated line length = %d", len([]rune(taskLines[1].line)))
	}

	manager.UnregisterTunnel(9, nil)
	// Offline sockets never send.
	if manager.SendLogControl(9, TunnelLogActionStart, nil) || manager.SendTaskLogControl(9, 1, "x", TunnelLogActionStart) {
		t.Fatalf("offline host must not accept log controls")
	}
}

func hostsInt32ForTunnelTest(value int32) *int32 { return &value }

func TestTunnelManagerReplacementAndStaleFrames(t *testing.T) {
	manager := NewTunnelManager(func(ctx context.Context, _ int32, _ TunnelControlMethod, _ any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	first := newTunnelTestSocket()
	manager.RegisterTunnel(11, first)
	manager.HandleTunnelMessage(11, first, TunnelEncodeFrame(TunnelFrameControlHello, 0,
		[]byte(`{"capabilities":["payincus_node_v1"]}`)))

	// A pending control belongs to the first socket.
	pending := make(chan error, 1)
	go func() {
		_, err := manager.RequestControl(context.Background(), 11, TunnelControlProxyProbe, nil, 5*time.Second)
		pending <- err
	}()
	time.Sleep(20 * time.Millisecond)

	second := newTunnelTestSocket()
	manager.RegisterTunnel(11, second)

	if first.TunnelReadyState() != 3 {
		t.Fatalf("the replaced socket must be closed")
	}
	first.mu.Lock()
	code := first.closeCode
	first.mu.Unlock()
	if code != 1000 {
		t.Fatalf("close code = %d, want 1000", code)
	}
	if !manager.IsTunnelOnline(11) {
		t.Fatalf("the new socket must be online")
	}
	// Replacing the socket clears the capability set.
	if manager.SupportsControl(11, TunnelControlNetworkApply) {
		t.Fatalf("capabilities must be reset until the new agent says hello")
	}
	if err := <-pending; !tunnelIsControlError(err, TunnelControlDisconnected) {
		t.Fatalf("pending control err = %v, want DISCONNECTED", err)
	}

	// Stale frames and stale close notifications from the old socket are ignored.
	manager.HandleTunnelMessage(11, first, TunnelEncodeFrame(TunnelFrameControlHello, 0,
		[]byte(`{"capabilities":["payincus_node_v1"]}`)))
	if manager.SupportsControl(11, TunnelControlNetworkApply) {
		t.Fatalf("frames from a replaced socket must be ignored")
	}
	manager.HandleTunnelClosed(11, first)
	if !manager.IsTunnelOnline(11) {
		t.Fatalf("a stale close must not tear down the new tunnel")
	}

	// UnregisterTunnel with a non-current socket is a no-op.
	manager.UnregisterTunnel(11, first)
	if !manager.IsTunnelOnline(11) {
		t.Fatalf("UnregisterTunnel(stale) must be a no-op")
	}
	manager.UnregisterTunnel(11, second)
	if manager.IsTunnelOnline(11) {
		t.Fatalf("UnregisterTunnel(current) must close the tunnel")
	}
	second.mu.Lock()
	reason := second.closeReason
	second.mu.Unlock()
	if reason != "Unregistered" {
		t.Fatalf("close reason = %q", reason)
	}
}

func TestTunnelDeriveEffectiveHostStatus(t *testing.T) {
	manager := TunnelManagerInstance()
	socket := newTunnelTestSocket()

	enabled := true
	disabled := false

	// maintenance always wins.
	if got := TunnelDeriveEffectiveHostStatus(TunnelEffectiveHostStatusInput{ID: 1, Status: "maintenance", TunnelEnabled: &enabled}); got != "maintenance" {
		t.Fatalf("status = %q", got)
	}

	// Tunnel mode without a live tunnel is effectively offline.
	if got := TunnelDeriveEffectiveHostStatus(TunnelEffectiveHostStatusInput{ID: 5001, Status: "online", TunnelEnabled: &enabled}); got != "offline" {
		t.Fatalf("status = %q", got)
	}

	// With a live tunnel the stored status stands.
	manager.RegisterTunnel(5001, socket)
	if got := TunnelDeriveEffectiveHostStatus(TunnelEffectiveHostStatusInput{ID: 5001, Status: "online", TunnelEnabled: &enabled}); got != "online" {
		t.Fatalf("status = %q", got)
	}
	manager.UnregisterTunnel(5001, nil)

	// Non-tunnel hosts keep their status; the snake_case field is honoured.
	if got := TunnelDeriveEffectiveHostStatus(TunnelEffectiveHostStatusInput{ID: 5002, Status: "online", TunnelEnabled: &disabled}); got != "online" {
		t.Fatalf("status = %q", got)
	}
	if got := TunnelDeriveEffectiveHostStatus(TunnelEffectiveHostStatusInput{ID: 5002, Status: "online", TunnelEnabledSnake: &enabled}); got != "offline" {
		t.Fatalf("snake_case tunnel_enabled ignored: %q", got)
	}
	// camelCase wins over snake_case (`??`).
	if got := TunnelDeriveEffectiveHostStatus(TunnelEffectiveHostStatusInput{ID: 5003, Status: "online", TunnelEnabled: &disabled, TunnelEnabledSnake: &enabled}); got != "online" {
		t.Fatalf("camelCase must take precedence: %q", got)
	}
}

func TestTunnelSeamsAreWired(t *testing.T) {
	manager := TunnelManagerInstance()
	socket := newTunnelTestSocket()

	if CaddyTunnelIsOnline(5010) {
		t.Fatalf("CaddyTunnelIsOnline must follow the manager")
	}
	if _, err := CaddyTunnelOpenStream(5010, "127.0.0.1", 8443); err == nil {
		t.Fatalf("CaddyTunnelOpenStream must fail for an offline host")
	}
	if TunnelLogControl(5010, 1, "task", "start") {
		t.Fatalf("TunnelLogControl must be false for an offline host")
	}

	manager.RegisterTunnel(5010, socket)
	if !CaddyTunnelIsOnline(5010) {
		t.Fatalf("CaddyTunnelIsOnline must be true once registered")
	}
	connection, err := CaddyTunnelOpenStream(5010, "127.0.0.1", 8444)
	if err != nil {
		t.Fatalf("CaddyTunnelOpenStream: %v", err)
	}
	_ = connection.Close()
	if !TunnelLogControl(5010, 7, "task", "start") {
		t.Fatalf("TunnelLogControl must be true once registered")
	}
	taskFrames := socket.sentFramesOfType(TunnelFrameTaskLogControl)
	if len(taskFrames) != 1 {
		t.Fatalf("task log frames = %d", len(taskFrames))
	}
	manager.UnregisterTunnel(5010, nil)
}

// TestHostsStorageCapacityBridge: init() must register the db/hosts.go storage
// gate against lib.GetStorageCapacity. Without a database the bridge surfaces the
// storage port's own "database not initialized" error — proving it delegates
// instead of returning a fake capacity.
func TestHostsStorageCapacityBridge(t *testing.T) {
	requiredMB := int64(1024)
	packageID := int32(3)
	poolName := "pool-1"
	_, err := hostsStorageCapacityFromStoragePolicy(context.Background(), nil, 5, db.HostsStorageCapacityOptions{
		PackageID:  &packageID,
		PoolName:   &poolName,
		RequiredMB: &requiredMB,
	})
	if err == nil || !strings.Contains(err.Error(), "database not initialized") {
		t.Fatalf("err = %v, want the storage port's database-not-initialized error", err)
	}
}

// TestStorageCapacityTunnelSeam: lib/storage_capacity.go's
// StorageCapacityTunnelIsOnline seam must follow the tunnel manager.
func TestStorageCapacityTunnelSeam(t *testing.T) {
	manager := TunnelManagerInstance()
	socket := newTunnelTestSocket()
	if StorageCapacityTunnelIsOnline(5020) {
		t.Fatalf("seam must be false for an offline host")
	}
	manager.RegisterTunnel(5020, socket)
	if !StorageCapacityTunnelIsOnline(5020) {
		t.Fatalf("seam must follow the registered tunnel")
	}
	manager.UnregisterTunnel(5020, nil)
	if StorageCapacityTunnelIsOnline(5020) {
		t.Fatalf("seam must be false after unregister")
	}
}

// TestTunnelReconcileWithoutDatabase: the app boot path calls
// reconcileTunnelHostsStatus; without a database it must degrade to 0 (the
// TypeScript swallows the error the same way) instead of panicking.
func TestTunnelReconcileWithoutDatabase(t *testing.T) {
	// Do not mutate db.DB here: registering a tunnel starts a detached
	// tunnelSyncHostStatus goroutine that reads it, and writing the global from a
	// test would race with that goroutine under -race. The test binary never calls
	// db.Init, so the handle is nil already.
	if db.DB != nil {
		t.Skip("database handle already initialised in this test binary")
	}
	if corrected := TunnelReconcileHostsStatus(context.Background()); corrected != 0 {
		t.Fatalf("corrected = %d, want 0", corrected)
	}
}

func TestTunnelSafeIntegerAndSlicing(t *testing.T) {
	if value, ok := tunnelSafeInteger(float64(42)); !ok || value != 42 {
		t.Fatalf("42 = %d, %v", value, ok)
	}
	for _, value := range []any{float64(1.5), "42", nil, math.Inf(1), math.NaN(), float64(9007199254740992)} {
		if _, ok := tunnelSafeInteger(value); ok {
			t.Fatalf("%v must not be a safe integer", value)
		}
	}
	if got := tunnelSliceRunes("中文测试", 2); got != "中文" {
		t.Fatalf("slice = %q", got)
	}
	if got := tunnelSliceRunes("short", 100); got != "short" {
		t.Fatalf("slice = %q", got)
	}
}
