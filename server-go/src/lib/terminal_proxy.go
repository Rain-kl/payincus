// Port of server/src/lib/terminal-proxy.ts (979 lines).
//
// Bridges a browser WebSocket to the Incus console of one instance:
//
//	browser  <-- ws -->  terminal session  <-- wss -->  Incus exec / console
//
// Containers always use `POST /1.0/instances/<name>/exec` with an interactive
// shell. VMs try exec first (needs qemu-guest-agent, closer to an SSH feel) and
// fall back to the serial/VGA console. Two channels are opened per operation:
// fd "0" carries the terminal bytes, fd "control" carries window-resize commands.
//
// Behaviour carried over from the TypeScript, all observable:
//   - a `{"type":"resize","cols":N,"rows":N}` text message is intercepted and
//     translated to `{"command":"window-resize","args":{"width":N,"height":N}}` on
//     the control channel instead of being forwarded;
//   - messages larger than 1 MiB are dropped (never forwarded);
//   - a 25 s heartbeat pings browser + both Incus channels; a missed browser pong
//     closes the session ("Connection timeout"), idle sessions die after 12 h;
//   - an Incus channel drop triggers up to 10 reconnects with a growing delay,
//     announcing `reconnecting` / `connected` messages to the browser;
//   - the first close/limit/lifecycle action sends `{"type":"disconnected"}`
//     before the socket closes.
//
// The active-session map is process local (AGENTS.md single-instance invariant).
package lib

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// TerminalConfig mirrors TERMINAL_CONFIG.
const (
	TerminalHeartbeatInterval    = 25 * time.Second
	TerminalHeartbeatTimeout     = 30 * time.Second
	TerminalIdleTimeout          = 12 * time.Hour
	TerminalMaxMessageSize       = 1048576
	TerminalReconnectDelay       = 1 * time.Second
	TerminalMaxReconnectAttempts = 10
)

// TerminalSession mirrors the TerminalSession interface.
type TerminalSession struct {
	ID            string
	InstanceID    int32
	UserID        int32
	AuthSessionID *string
	Host          *db.HostsRecord
	InstanceName  string
	InstanceType  string

	client *WebSocket

	mu                  sync.Mutex
	incus               *WebSocket
	control             *WebSocket
	mode                string
	createdAt           time.Time
	lastActivity        atomic.Int64 // unix nanoseconds
	reconnecting        bool
	reconnectAttempts   int
	reconnectGeneration int

	heartbeatStop chan struct{}
	stopHeartbeat sync.Once
	pongReceived  atomic.Bool
}

// ConnectionMode returns the live channel mode ("exec" or "console").
func (s *TerminalSession) ConnectionMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *TerminalSession) touch() {
	s.lastActivity.Store(time.Now().UTC().UnixNano())
}

func (s *TerminalSession) lastActivityAt() time.Time {
	nanos := s.lastActivity.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

func (s *TerminalSession) incusSockets() (*WebSocket, *WebSocket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.incus, s.control
}

var terminalSessions = struct {
	sync.Mutex
	sessions map[string]*TerminalSession
}{sessions: map[string]*TerminalSession{}}

// TerminalSessionStats mirrors getActiveSessionStats().
type TerminalSessionStats struct {
	Total      int
	ByInstance map[int32]int
	ByUser     map[int32]int
}

// GetActiveSessionStats mirrors getActiveSessionStats().
func GetActiveSessionStats() TerminalSessionStats {
	terminalSessions.Lock()
	defer terminalSessions.Unlock()
	stats := TerminalSessionStats{
		Total:      len(terminalSessions.sessions),
		ByInstance: map[int32]int{},
		ByUser:     map[int32]int{},
	}
	for _, session := range terminalSessions.sessions {
		stats.ByInstance[session.InstanceID]++
		stats.ByUser[session.UserID]++
	}
	return stats
}

func terminalGenerateSessionID() string {
	return fmt.Sprintf("term_%d_%s", time.Now().UTC().UnixMilli(), strconv.FormatInt(rand.Int63(), 36))
}

// ==================== certificate cache ====================

type terminalCertificateCacheEntry struct {
	cert     []byte
	key      []byte
	loadedAt time.Time
}

var terminalCertificateCache = struct {
	sync.Mutex
	entries map[string]terminalCertificateCacheEntry
}{entries: map[string]terminalCertificateCacheEntry{}}

const terminalCertificateCacheTTL = time.Hour

// getCachedCertificates mirrors getCachedCertificates(certPath, keyPath): one hour
// of caching, with the same error text on a read failure.
func getCachedCertificates(certPath, keyPath string) ([]byte, []byte, error) {
	resolvedCert, resolvedKey := ResolveCertificatePair(&certPath, &keyPath)
	if resolvedCert == nil || resolvedKey == nil {
		return nil, nil, errors.New("Failed to read certificate files: certificate or key path is missing")
	}
	cacheKey := *resolvedCert + ":" + *resolvedKey

	terminalCertificateCache.Lock()
	entry, ok := terminalCertificateCache.entries[cacheKey]
	if ok && time.Since(entry.loadedAt) < terminalCertificateCacheTTL {
		terminalCertificateCache.Unlock()
		return entry.cert, entry.key, nil
	}
	terminalCertificateCache.Unlock()

	cert, err := os.ReadFile(*resolvedCert)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to read certificate files: %v", err)
	}
	key, err := os.ReadFile(*resolvedKey)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to read certificate files: %v", err)
	}

	terminalCertificateCache.Lock()
	terminalCertificateCache.entries[cacheKey] = terminalCertificateCacheEntry{
		cert: cert, key: key, loadedAt: time.Now().UTC(),
	}
	terminalCertificateCache.Unlock()
	return cert, key, nil
}

// ==================== Incus payloads ====================

// terminalExecShell is the exact `command[2]` string of buildExecPayload().
const terminalExecShell = "setup_term() { " +
	"  if [ -e /usr/share/terminfo/x/xterm-256color ] || [ -e /lib/terminfo/x/xterm-256color ] || " +
	"     [ -e /etc/terminfo/x/xterm-256color ] || infocmp xterm-256color >/dev/null 2>&1; then " +
	"    export TERM=xterm-256color; " +
	"  elif [ -e /usr/share/terminfo/x/xterm ] || [ -e /lib/terminfo/x/xterm ] || " +
	"       [ -e /etc/terminfo/x/xterm ] || infocmp xterm >/dev/null 2>&1; then " +
	"    export TERM=xterm; " +
	"  else " +
	"    export TERM=linux; " +
	"  fi; " +
	"}; " +
	"setup_term; " +
	"export LANG=en_US.UTF-8; export LC_ALL=en_US.UTF-8; " +
	"if [ -x /bin/bash ]; then exec /bin/bash -l; " +
	"elif [ -x /usr/bin/bash ]; then exec /usr/bin/bash -l; " +
	"else exec /bin/sh -l; fi"

func buildExecPayload() map[string]any {
	return map[string]any{
		"command":            []string{"/bin/sh", "-c", terminalExecShell},
		"wait-for-websocket": true,
		"interactive":        true,
		"environment":        map[string]string{"TERM": "xterm", "LANG": "en_US.UTF-8", "LC_ALL": "en_US.UTF-8", "HOME": "/root", "USER": "root", "SHELL": "/bin/bash"},
		"width":              120,
		"height":             40,
	}
}

func buildConsolePayload() map[string]any {
	return map[string]any{"wait-for-websocket": true, "type": "console"}
}

// terminalOperationResult is the subset requestTerminalOperation reads.
type terminalOperationResult struct {
	operationID string
	fds         map[string]string
}

// requestTerminalOperation mirrors the helper of the same name: POST an exec or
// console request and extract the operation id plus the channel secrets.
func requestTerminalOperation(ctx context.Context, client *http.Client, baseURL, endpoint string, payload map[string]any) (*terminalOperationResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	var data struct {
		Type      string `json:"type"`
		Operation string `json:"operation"`
		ErrorCode *int   `json:"error_code"`
		Error     string `json:"error"`
		Metadata  struct {
			ID       string `json:"id"`
			Metadata struct {
				Fds map[string]string `json:"fds"`
			} `json:"metadata"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, errors.New("Failed to create terminal session")
	}
	if data.Type == "error" || (data.ErrorCode != nil && *data.ErrorCode != 0) {
		if data.Error != "" {
			return nil, errors.New(data.Error)
		}
		return nil, errors.New("Failed to create terminal session")
	}

	operationID := ""
	if slash := strings.LastIndex(data.Operation, "/"); slash >= 0 {
		operationID = data.Operation[slash+1:]
	} else {
		operationID = data.Operation
	}
	if operationID == "" {
		operationID = data.Metadata.ID
	}
	if operationID == "" {
		return nil, errors.New("No operation ID returned from terminal request")
	}
	if data.Metadata.Metadata.Fds == nil {
		return nil, errors.New("No file descriptors returned from terminal operation")
	}
	return &terminalOperationResult{operationID: operationID, fds: data.Metadata.Metadata.Fds}, nil
}

// ==================== Incus console connection ====================

// CreateIncusConsoleConnection mirrors createIncusConsoleConnection(host, instanceName, instanceType).
func CreateIncusConsoleConnection(ctx context.Context, host *db.HostsRecord, instanceName string, instanceType string) (control *WebSocket, data *WebSocket, operationID string, mode string, err error) {
	if host == nil {
		return nil, nil, "", "", errors.New("Host certificate configuration missing")
	}
	tunnelEnabled := host.TunnelEnabled
	targetHost := host.TargetHost
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	targetPort := int(host.TargetPort)
	if targetPort == 0 {
		targetPort = 8443
	}

	if tunnelEnabled && !TunnelManagerInstance().IsTunnelOnline(host.ID) {
		return nil, nil, "", "", errors.New("宿主机内网穿透通道未连接（Agent 离线），无法连接到终端")
	}

	baseURL := host.URL
	if tunnelEnabled && (host.URL == "" || !strings.HasPrefix(host.URL, "http")) {
		baseURL = "https://" + net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
	}
	if host.CertPath == nil || host.KeyPath == nil || *host.CertPath == "" || *host.KeyPath == "" {
		return nil, nil, "", "", errors.New("Host certificate configuration missing")
	}
	certPEM, keyPEM, err := getCachedCertificates(*host.CertPath, *host.KeyPath)
	if err != nil {
		return nil, nil, "", "", err
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, "", "", err
	}

	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{certificate},
		InsecureSkipVerify: true, //nolint:gosec // Incus is self-signed; matches rejectUnauthorized:false
		MinVersion:         tls.VersionTLS12,
	}
	var dial func(ctx context.Context, network, address string) (net.Conn, error)
	if tunnelEnabled {
		hostID := host.ID
		dial = func(_ context.Context, _, _ string) (net.Conn, error) {
			return TunnelManagerInstance().CreateDuplexStream(hostID, targetHost, targetPort)
		}
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, DialContext: dial}
	if dial == nil {
		transport.DialContext = (&net.Dialer{}).DialContext
	}
	httpClient := &http.Client{Transport: transport, Timeout: 120 * time.Second}
	defer transport.CloseIdleConnections()

	encodedName := url.PathEscape(instanceName)
	requestCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	result := &terminalOperationResult{}
	mode = "exec"
	if instanceType == "vm" {
		execResult, execErr := requestTerminalOperation(requestCtx, httpClient, baseURL, "/1.0/instances/"+encodedName+"/exec", buildExecPayload())
		if execErr != nil {
			// VM exec depends on qemu-guest-agent; fall back to the serial console.
			consoleResult, consoleErr := requestTerminalOperation(requestCtx, httpClient, baseURL, "/1.0/instances/"+encodedName+"/console", buildConsolePayload())
			if consoleErr != nil {
				return nil, nil, "", "", consoleErr
			}
			result = consoleResult
			mode = "console"
		} else {
			result = execResult
		}
	} else {
		execResult, execErr := requestTerminalOperation(requestCtx, httpClient, baseURL, "/1.0/instances/"+encodedName+"/exec", buildExecPayload())
		if execErr != nil {
			return nil, nil, "", "", execErr
		}
		result = execResult
	}

	dataSecret := result.fds["0"]
	if dataSecret == "" {
		return nil, nil, "", "", errors.New("No data file descriptor (fd 0) returned from console operation")
	}

	wsBaseURL := strings.Replace(baseURL, "https://", "wss://", 1)
	wsBaseURL = strings.Replace(wsBaseURL, "http://", "ws://", 1)
	dataURL := wsBaseURL + "/1.0/operations/" + url.PathEscape(result.operationID) + "/websocket?secret=" + url.QueryEscape(dataSecret)
	controlSecret := result.fds["control"]
	controlURL := ""
	if controlSecret != "" {
		controlURL = wsBaseURL + "/1.0/operations/" + url.PathEscape(result.operationID) + "/websocket?secret=" + url.QueryEscape(controlSecret)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	dataWS, err := DialWebSocket(dialCtx, dataURL, WebSocketDialOptions{
		TLSConfig:        tlsConfig,
		Dial:             dial,
		HandshakeTimeout: 10 * time.Second,
		MaxPayload:       TerminalMaxMessageSize,
	})
	if err != nil {
		return nil, nil, "", "", errors.New("Data WebSocket connection timeout")
	}
	var controlWS *WebSocket
	if controlURL != "" {
		controlWS, err = DialWebSocket(dialCtx, controlURL, WebSocketDialOptions{
			TLSConfig:        tlsConfig,
			Dial:             dial,
			HandshakeTimeout: 10 * time.Second,
			MaxPayload:       TerminalMaxMessageSize,
		})
		if err != nil {
			dataWS.Terminate()
			return nil, nil, "", "", errors.New("Control WebSocket connection timeout")
		}
	}
	return controlWS, dataWS, result.operationID, mode, nil
}

// ==================== session lifecycle ====================

func closeIncusConnectionPair(dataWS, controlWS *WebSocket) {
	if dataWS != nil {
		dataWS.Terminate()
	}
	if controlWS != nil {
		controlWS.Terminate()
	}
}

func cleanupIncusSockets(session *TerminalSession) {
	session.mu.Lock()
	dataWS := session.incus
	controlWS := session.control
	session.incus = nil
	session.control = nil
	session.mu.Unlock()
	if dataWS != nil {
		dataWS.Terminate()
	}
	if controlWS != nil {
		controlWS.Terminate()
	}
}

func safeSendClientMessage(session *TerminalSession, message map[string]any) {
	if !session.client.IsOpen() {
		return
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return
	}
	_ = session.client.SendText(string(encoded))
}

// terminalClientMessage is one browser message handed to the session.
type terminalClientMessage struct {
	payload  []byte
	isBinary bool
}

// terminalClientReader drains the browser socket from the moment the session
// starts opening, so an early disconnect is noticed while the Incus console is
// still connecting. Messages are buffered (the TypeScript would lose them: its
// `message` listener is installed only after the backend connects).
type terminalClientReader struct {
	socket   *WebSocket
	messages chan terminalClientMessage
	closed   atomic.Bool
}

func newTerminalClientReader(socket *WebSocket) *terminalClientReader {
	reader := &terminalClientReader{socket: socket, messages: make(chan terminalClientMessage, 8)}
	go func() {
		defer close(reader.messages)
		for {
			payload, isBinary, err := socket.ReadMessage()
			if err != nil {
				reader.closed.Store(true)
				return
			}
			select {
			case reader.messages <- terminalClientMessage{payload: payload, isBinary: isBinary}:
			default:
				// A burst larger than the buffer is dropped, exactly like messages
				// that arrive before the TypeScript installs its listener.
			}
		}
	}()
	return reader
}

// bindIncusSocketHandlers mirrors bindIncusSocketHandlers(session, dataWs, controlWs).
func bindIncusSocketHandlers(session *TerminalSession, dataWS, controlWS *WebSocket) {
	go func() {
		for {
			payload, isBinary, err := dataWS.ReadMessage()
			if err != nil {
				go reconnectIncusConnection(session, "Incus connection closed")
				return
			}
			session.touch()
			if session.client.IsOpen() {
				_ = session.client.SendMessage(payload, isBinary)
			}
		}
	}()

	if controlWS != nil {
		controlWS.SetPongHandler(func() { session.touch() })
		go func() {
			for {
				if _, _, err := controlWS.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}
}

// terminalNumber mirrors `Number(value)` for the JSON values AJV/JS can produce.
func terminalNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil || math.IsNaN(parsed) {
			// JS: Number("NaN") is NaN, and NaN is falsy in `Number(x) || default`.
			return 0
		}
		return parsed
	case bool:
		if typed {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// terminalResizeBounds mirrors
//
//	cols = Math.max(1, Math.min(500, Number(message.cols) || 80))
//	rows = Math.max(1, Math.min(200, Number(message.rows) || 24))
func terminalResizeBounds(rawCols, rawRows any) (float64, float64) {
	cols := terminalNumber(rawCols)
	if cols == 0 {
		cols = 80
	}
	rows := terminalNumber(rawRows)
	if rows == 0 {
		rows = 24
	}
	if cols < 1 {
		cols = 1
	}
	if cols > 500 {
		cols = 500
	}
	if rows < 1 {
		rows = 1
	}
	if rows > 200 {
		rows = 200
	}
	return cols, rows
}

// handleClientMessage mirrors the `clientWs.on('message')` handler.
func handleClientMessage(session *TerminalSession, message terminalClientMessage) {
	session.touch()

	if int64(len(message.payload)) > TerminalMaxMessageSize {
		return
	}

	trimmed := bytes.TrimSpace(message.payload)
	if len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' {
		var control struct {
			Type string `json:"type"`
			Cols any    `json:"cols"`
			Rows any    `json:"rows"`
		}
		if err := json.Unmarshal(trimmed, &control); err == nil && control.Type == "resize" {
			session.mu.Lock()
			currentControl := session.control
			session.mu.Unlock()
			if currentControl != nil {
				cols, rows := terminalResizeBounds(control.Cols, control.Rows)
				resizeCommand, _ := json.Marshal(map[string]any{
					"command": "window-resize",
					"args":    map[string]any{"width": cols, "height": rows},
				})
				if currentControl.IsOpen() {
					_ = currentControl.SendText(string(resizeCommand))
				}
				return
			}
		}
	}

	session.mu.Lock()
	dataWS := session.incus
	session.mu.Unlock()
	if dataWS != nil && dataWS.IsOpen() {
		_ = dataWS.SendMessage(message.payload, message.isBinary)
	}
}

// reconnectIncusConnection mirrors reconnectIncusConnection(sessionId, reason).
func reconnectIncusConnection(session *TerminalSession, reason string) {
	if !terminalSessionRegistered(session.ID) {
		return
	}
	if !session.client.IsOpen() {
		CloseTerminalSession(session.ID, reason)
		return
	}

	session.mu.Lock()
	if session.reconnecting {
		session.mu.Unlock()
		return
	}
	session.reconnecting = true
	session.reconnectAttempts++
	attempts := session.reconnectAttempts
	generation := session.reconnectGeneration + 1
	session.reconnectGeneration = generation
	mode := session.mode
	session.mu.Unlock()

	if attempts > TerminalMaxReconnectAttempts {
		CloseTerminalSession(session.ID, "Terminal backend reconnection limit exceeded")
		return
	}

	safeSendClientMessage(session, map[string]any{
		"type":   "reconnecting",
		"reason": reason,
		"mode":   mode,
	})

	cleanupIncusSockets(session)

	delay := TerminalReconnectDelay + time.Duration(attempts-1)*2*time.Second
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	select {
	case <-time.After(delay):
	case <-session.heartbeatStop:
		return
	}

	if !terminalSessionRegistered(session.ID) || !sessionStillCurrent(session, generation) {
		return
	}
	if !session.client.IsOpen() {
		CloseTerminalSession(session.ID, reason)
		return
	}

	controlWS, dataWS, _, newMode, err := CreateIncusConsoleConnection(context.Background(), session.Host, session.InstanceName, session.InstanceType)
	if err != nil {
		if !terminalSessionRegistered(session.ID) || !sessionStillCurrent(session, generation) {
			return
		}
		if !session.client.IsOpen() {
			CloseTerminalSession(session.ID, reason)
			return
		}
		session.mu.Lock()
		session.reconnecting = false
		session.mu.Unlock()
		reconnectIncusConnection(session, "Incus reconnection failed")
		return
	}

	if !terminalSessionRegistered(session.ID) || !sessionStillCurrent(session, generation) || !session.client.IsOpen() {
		closeIncusConnectionPair(dataWS, controlWS)
		if terminalSessionRegistered(session.ID) && sessionStillCurrent(session, generation) {
			CloseTerminalSession(session.ID, reason)
		}
		return
	}

	session.mu.Lock()
	session.mode = newMode
	session.incus = dataWS
	session.control = controlWS
	session.reconnecting = false
	session.reconnectAttempts = 0
	session.mu.Unlock()
	session.touch()

	bindIncusSocketHandlers(session, dataWS, controlWS)
	safeSendClientMessage(session, map[string]any{
		"type":      "connected",
		"sessionId": session.ID,
		"message":   "Terminal backend reconnected",
		"mode":      newMode,
	})
}

func terminalSessionRegistered(id string) bool {
	terminalSessions.Lock()
	defer terminalSessions.Unlock()
	_, ok := terminalSessions.sessions[id]
	return ok
}

func sessionStillCurrent(session *TerminalSession, generation int) bool {
	terminalSessions.Lock()
	current, ok := terminalSessions.sessions[session.ID]
	terminalSessions.Unlock()
	if !ok || current != session {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.reconnectGeneration == generation
}

// CreateTerminalSession mirrors createTerminalSession(clientWs, host, instanceId,
// instanceName, userId, instanceType, authSessionId).
//
// clientWS is the already-upgraded browser socket. The caller is responsible for
// every authorisation step before the upgrade (see routes/terminal.go).
func CreateTerminalSession(
	clientWS *WebSocket,
	host *db.HostsRecord,
	instanceID int32,
	instanceName string,
	userID int32,
	instanceType string,
	authSessionID *string,
) (*TerminalSession, error) {
	startSessionCleanupOnce()
	if instanceType != "vm" {
		instanceType = "container"
	}
	sessionID := terminalGenerateSessionID()
	reader := newTerminalClientReader(clientWS)

	controlWS, dataWS, _, mode, err := CreateIncusConsoleConnection(context.Background(), host, instanceName, instanceType)
	if err != nil {
		if reader.closed.Load() {
			return nil, errors.New("Client disconnected while terminal backend connection was opening")
		}
		return nil, err
	}
	if reader.closed.Load() || !clientWS.IsOpen() {
		closeIncusConnectionPair(dataWS, controlWS)
		return nil, errors.New("Client disconnected while terminal backend connection was opening")
	}

	session := &TerminalSession{
		ID:            sessionID,
		InstanceID:    instanceID,
		UserID:        userID,
		AuthSessionID: authSessionID,
		Host:          host,
		InstanceName:  instanceName,
		InstanceType:  instanceType,
		client:        clientWS,
		mode:          mode,
		createdAt:     time.Now().UTC(),
		heartbeatStop: make(chan struct{}),
	}
	session.touch()
	session.mu.Lock()
	session.incus = dataWS
	session.control = controlWS
	session.mu.Unlock()

	bindIncusSocketHandlers(session, dataWS, controlWS)

	// Client → Incus. The reader was installed before the asynchronous backend work,
	// so an early disconnect or message is not lost.
	go func() {
		for message := range reader.messages {
			handleClientMessage(session, message)
		}
		if terminalSessionRegistered(session.ID) {
			CloseTerminalSession(session.ID, "Client disconnected")
		}
	}()

	// Heartbeat / idle timeout.
	session.pongReceived.Store(true)
	clientWS.SetPongHandler(func() {
		session.pongReceived.Store(true)
		session.touch()
	})
	go func() {
		ticker := time.NewTicker(TerminalHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-session.heartbeatStop:
				return
			case <-ticker.C:
				if !clientWS.IsOpen() {
					return
				}
				if !session.pongReceived.Load() {
					CloseTerminalSession(session.ID, "Connection timeout")
					return
				}
				session.pongReceived.Store(false)
				_ = clientWS.Ping()
				incusWS, controlWS := session.incusSockets()
				if incusWS != nil && incusWS.IsOpen() {
					_ = incusWS.Ping()
				}
				if controlWS != nil && controlWS.IsOpen() {
					_ = controlWS.Ping()
				}
				if time.Since(session.lastActivityAt()) > TerminalIdleTimeout {
					CloseTerminalSession(session.ID, "Idle timeout")
					return
				}
			}
		}
	}()

	terminalSessions.Lock()
	terminalSessions.sessions[sessionID] = session
	terminalSessions.Unlock()

	safeSendClientMessage(session, map[string]any{
		"type":      "connected",
		"sessionId": sessionID,
		"message":   fmt.Sprintf("Terminal session established (%s)", mode),
		"mode":      mode,
	})
	return session, nil
}

// CloseTerminalSession mirrors closeTerminalSession(sessionId, reason).
func CloseTerminalSession(sessionID string, reason string) {
	if reason == "" {
		reason = "Session closed"
	}
	terminalSessions.Lock()
	session, ok := terminalSessions.sessions[sessionID]
	if !ok {
		terminalSessions.Unlock()
		return
	}
	delete(terminalSessions.sessions, sessionID)
	terminalSessions.Unlock()

	session.mu.Lock()
	session.reconnectGeneration++
	session.mu.Unlock()

	if session.heartbeatStop != nil {
		session.stopHeartbeat.Do(func() { close(session.heartbeatStop) })
	}
	cleanupIncusSockets(session)

	if session.client != nil && session.client.IsOpen() {
		safeSendClientMessage(session, map[string]any{"type": "disconnected", "reason": reason})
		session.client.Close(1000, "")
	}
}

// CloseInstanceSessions mirrors closeInstanceSessions(instanceId, reason).
func CloseInstanceSessions(instanceID int32, reason string) int {
	if reason == "" {
		reason = "Instance session closed"
	}
	closedGraphics := SpiceSessions.CloseMatching(func(session SpiceAccess) bool { return session.InstanceID == instanceID })

	terminalSessions.Lock()
	ids := []string{}
	for id, session := range terminalSessions.sessions {
		if session.InstanceID == instanceID {
			ids = append(ids, id)
		}
	}
	terminalSessions.Unlock()
	for _, id := range ids {
		CloseTerminalSession(id, reason)
	}
	return len(ids) + closedGraphics
}

// CleanupStaleSessions mirrors cleanupStaleSessions().
func CleanupStaleSessions() int {
	threshold := TerminalIdleTimeout + TerminalHeartbeatInterval*2
	terminalSessions.Lock()
	ids := []string{}
	for id, session := range terminalSessions.sessions {
		if time.Since(session.lastActivityAt()) > threshold {
			ids = append(ids, id)
		}
	}
	terminalSessions.Unlock()
	for _, id := range ids {
		CloseTerminalSession(id, "Session stale cleanup")
	}
	return len(ids)
}

var (
	terminalCleanupMu      sync.Mutex
	terminalCleanupStarted bool
	terminalCleanupStop    chan struct{}
)

func startSessionCleanupOnce() {
	terminalCleanupMu.Lock()
	defer terminalCleanupMu.Unlock()
	if terminalCleanupStarted {
		return
	}
	terminalCleanupStarted = true
	terminalCleanupStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-terminalCleanupStop:
				return
			case <-ticker.C:
				CleanupStaleSessions()
			}
		}
	}()
}

// StartSessionCleanup mirrors startSessionCleanup().
func StartSessionCleanup() { startSessionCleanupOnce() }

// StopSessionCleanup mirrors stopSessionCleanup().
func StopSessionCleanup() {
	terminalCleanupMu.Lock()
	defer terminalCleanupMu.Unlock()
	if !terminalCleanupStarted {
		return
	}
	terminalCleanupStarted = false
	close(terminalCleanupStop)
	terminalCleanupStop = nil
}

// CloseUserSessions mirrors closeUserSessions(userId, reason).
func CloseUserSessions(userID int32, reason string) int {
	if reason == "" {
		reason = "User session closed"
	}
	closedGraphics := SpiceSessions.CloseMatching(func(session SpiceAccess) bool { return session.UserID == userID })

	terminalSessions.Lock()
	ids := []string{}
	for id, session := range terminalSessions.sessions {
		if session.UserID == userID {
			ids = append(ids, id)
		}
	}
	terminalSessions.Unlock()
	for _, id := range ids {
		CloseTerminalSession(id, reason)
	}
	return len(ids) + closedGraphics
}

// CloseSessionTerminalSessions mirrors closeSessionTerminalSessions(authSessionId, reason).
func CloseSessionTerminalSessions(authSessionID string, reason string) int {
	if reason == "" {
		reason = "Session closed"
	}
	closedGraphics := SpiceSessions.CloseMatching(func(session SpiceAccess) bool {
		return session.SessionID != nil && *session.SessionID == authSessionID
	})

	terminalSessions.Lock()
	ids := []string{}
	for id, session := range terminalSessions.sessions {
		if session.AuthSessionID != nil && *session.AuthSessionID == authSessionID {
			ids = append(ids, id)
		}
	}
	terminalSessions.Unlock()
	for _, id := range ids {
		CloseTerminalSession(id, reason)
	}
	return len(ids) + closedGraphics
}

// ResetTerminalSessionsForTest clears the active session map (tests only).
func ResetTerminalSessionsForTest() {
	terminalSessions.Lock()
	terminalSessions.sessions = map[string]*TerminalSession{}
	terminalSessions.Unlock()
}

// AddTerminalSessionForTest inserts a session into the process map (tests only).
// The entry has no live sockets, so only the counter/limits code may read it.
func AddTerminalSessionForTest(session *TerminalSession) {
	if session == nil || session.ID == "" {
		return
	}
	terminalSessions.Lock()
	terminalSessions.sessions[session.ID] = session
	terminalSessions.Unlock()
}

// CloseAllSessions mirrors closeAllSessions(reason) (graceful shutdown).
func CloseAllSessions(reason string) int {
	if reason == "" {
		reason = "Server shutdown"
	}
	closedGraphics := SpiceSessions.CloseMatching(func(SpiceAccess) bool { return true })

	terminalSessions.Lock()
	ids := make([]string, 0, len(terminalSessions.sessions))
	for id := range terminalSessions.sessions {
		ids = append(ids, id)
	}
	terminalSessions.Unlock()
	for _, id := range ids {
		CloseTerminalSession(id, reason)
	}
	return len(ids) + closedGraphics
}
