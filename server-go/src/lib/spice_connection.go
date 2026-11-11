// Port of server/src/lib/incus/spice-connection.ts.
//
// Opens the Incus graphical (VGA) console of one VM:
//
//	POST /1.0/instances/<name>/console {type:"vga", "wait-for-websocket":true, force}
//	  → async operation whose metadata.fds carries one secret per SPICE channel;
//	WS   /1.0/operations/<id>/websocket?secret=<secret>
//	  → one socket per channel: "control" is kept by the session manager,
//	    "0" is the display/input data channel opened when a browser attaches.
//
// Security invariants carried over verbatim:
//   - Incus responses are never logged: operation metadata contains channel
//     secrets.
//   - WebSocket URLs carry the secret, so dial errors are reduced to a generic
//     "Incus console <stage> failed" (SpiceConnectionError) and never propagated
//     with their URL.
//   - Cleanup cancels the Incus operation with its own 5 s signal, because the
//     session signal is already aborted when cleanup runs; a 404 means the
//     operation is gone (success), 400/409 mean cancellation raced a normal close.
package lib

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// SpiceStage mirrors the four stages of the TypeScript error class.
type SpiceStage string

const (
	SpiceStageCreate   SpiceStage = "create"
	SpiceStageMetadata SpiceStage = "metadata"
	SpiceStageControl  SpiceStage = "control"
	SpiceStageCleanup  SpiceStage = "cleanup"
)

// SpiceConnectionError mirrors `class SpiceConnectionError extends Error`.
type SpiceConnectionError struct {
	Stage      SpiceStage
	HTTPStatus int
}

// Error mirrors `Incus console ${stage} failed`.
func (e *SpiceConnectionError) Error() string {
	return "Incus console " + string(e.Stage) + " failed"
}

// SpiceBackend mirrors the `SpiceBackend` interface: one control channel plus a
// factory for additional data channels of the same console operation.
type SpiceBackend struct {
	control    *WebSocket
	baseURL    string
	client     *http.Client
	tlsConf    *tls.Config
	dial       func(ctx context.Context, network, address string) (net.Conn, error)
	opID       string
	fds        map[string]string
	sockets    map[*WebSocket]struct{}
	socketsMu  sync.Mutex
	closedFlag bool

	cleanupMu   sync.Mutex
	cleanupDone chan struct{}
	cleanupErr  error
}

const (
	spiceChannelMaxPayload = 16 * 1024 * 1024
	spiceRequestTimeout    = 15 * time.Second
	spiceCleanupTimeout    = 5 * time.Second
)

// Control returns the control channel (`backend.control` in the TypeScript).
func (b *SpiceBackend) Control() *WebSocket { return b.control }

// OpenData opens the display/input data channel (`openData(signal)`).
func (b *SpiceBackend) OpenData(ctx context.Context) (*WebSocket, error) {
	b.socketsMu.Lock()
	closed := b.closedFlag
	b.socketsMu.Unlock()
	if closed {
		return nil, errors.New("Console closed")
	}
	secret, ok := b.fds["0"]
	if !ok || secret == "" {
		return nil, errors.New("No data file descriptor (fd 0) returned from console operation")
	}
	return b.openSecret(ctx, secret)
}

func (b *SpiceBackend) openSecret(ctx context.Context, secret string) (*WebSocket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wsURL := spiceWebSocketURL(b.baseURL, b.opID, secret)
	dialCtx, cancel := context.WithTimeout(ctx, spiceRequestTimeout)
	defer cancel()
	socket, err := DialWebSocket(dialCtx, wsURL, WebSocketDialOptions{
		TLSConfig:        b.tlsConf,
		Dial:             b.dial,
		HandshakeTimeout: spiceRequestTimeout,
		MaxPayload:       spiceChannelMaxPayload,
	})
	if err != nil {
		// Never surface the URL (it embeds the channel secret).
		return nil, errors.New("Console channel unavailable")
	}
	b.socketsMu.Lock()
	if b.closedFlag {
		b.socketsMu.Unlock()
		socket.Terminate()
		return nil, errors.New("Console closed")
	}
	if b.sockets == nil {
		b.sockets = map[*WebSocket]struct{}{}
	}
	b.sockets[socket] = struct{}{}
	b.socketsMu.Unlock()
	socket.OnClose(func() {
		b.socketsMu.Lock()
		delete(b.sockets, socket)
		b.socketsMu.Unlock()
	})
	return socket, nil
}

// Close terminates every channel and cancels the Incus operation. It is
// idempotent; a transport failure is reported so the session manager can retry
// (`failedCleanup` in the TypeScript).
func (b *SpiceBackend) Close() error {
	b.cleanupMu.Lock()
	if b.cleanupDone != nil {
		done := b.cleanupDone
		b.cleanupMu.Unlock()
		<-done
		b.cleanupMu.Lock()
		err := b.cleanupErr
		b.cleanupMu.Unlock()
		return err
	}
	done := make(chan struct{})
	b.cleanupDone = done
	b.cleanupMu.Unlock()

	b.socketsMu.Lock()
	b.closedFlag = true
	sockets := make([]*WebSocket, 0, len(b.sockets))
	for socket := range b.sockets {
		sockets = append(sockets, socket)
	}
	b.sockets = map[*WebSocket]struct{}{}
	b.socketsMu.Unlock()
	for _, socket := range sockets {
		socket.Terminate()
	}

	err := b.cancelOperation()

	b.cleanupMu.Lock()
	b.cleanupErr = err
	if err != nil {
		// A later reconnect may retry cleanup after a transient transport failure.
		b.cleanupDone = nil
	}
	b.cleanupMu.Unlock()
	close(done)
	return err
}

// cancelOperation mirrors the cleanup closure: DELETE the operation, then poll it
// until it reaches a terminal status.
func (b *SpiceBackend) cancelOperation() error {
	ctx, cancel := context.WithTimeout(context.Background(), spiceCleanupTimeout)
	defer cancel()

	operationURL := b.baseURL + "/1.0/operations/" + url.PathEscape(b.opID)
	status, err := b.operationRequest(ctx, http.MethodDelete, operationURL)
	if err != nil {
		return &SpiceConnectionError{Stage: SpiceStageCleanup}
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 400 && status != http.StatusBadRequest && status != http.StatusConflict {
		return &SpiceConnectionError{Stage: SpiceStageCleanup, HTTPStatus: status}
	}

	for {
		if err := ctx.Err(); err != nil {
			return &SpiceConnectionError{Stage: SpiceStageCleanup}
		}
		status, body, err := b.operationFetch(ctx, http.MethodGet, operationURL)
		if err != nil {
			return &SpiceConnectionError{Stage: SpiceStageCleanup}
		}
		if status == http.StatusNotFound {
			return nil
		}
		if status >= 400 {
			return &SpiceConnectionError{Stage: SpiceStageCleanup, HTTPStatus: status}
		}
		var envelope struct {
			Metadata struct {
				Status string `json:"status"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return &SpiceConnectionError{Stage: SpiceStageCleanup}
		}
		switch envelope.Metadata.Status {
		case "Success", "Failure", "Cancelled":
			return nil
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return &SpiceConnectionError{Stage: SpiceStageCleanup}
		}
	}
}

func (b *SpiceBackend) operationRequest(ctx context.Context, method, operationURL string) (int, error) {
	status, _, err := b.operationFetch(ctx, method, operationURL)
	return status, err
}

func (b *SpiceBackend) operationFetch(ctx context.Context, method, operationURL string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, operationURL, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := httpClientWithTimeout(b.client, spiceCleanupTimeout).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}

// httpClientWithTimeout returns a shallow copy of client with a hard timeout, so
// the pooled transport keeps its mTLS/tunnel dialing but a stuck upstream cannot
// pin a cleanup forever.
func httpClientWithTimeout(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		return &http.Client{Timeout: timeout}
	}
	clone := *client
	clone.Timeout = timeout
	return &clone
}

// spiceWebSocketURL mirrors the URL construction in open():
// `new URL('/1.0/operations/<id>/websocket', baseUrl)` with the scheme switched to
// ws(s) and the secret in the query string.
func spiceWebSocketURL(baseURL, operationID, secret string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		// Callers passed a validated base URL; keep the fallback simple.
		return strings.Replace(baseURL, "https://", "wss://", 1) +
			"/1.0/operations/" + url.PathEscape(operationID) + "/websocket?secret=" + url.QueryEscape(secret)
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else {
		parsed.Scheme = "ws"
	}
	parsed.Path = "/1.0/operations/" + operationID + "/websocket"
	parsed.RawPath = ""
	parsed.RawQuery = "secret=" + url.QueryEscape(secret)
	return parsed.String()
}

// CreateSpiceBackend mirrors createSpiceBackend(host, instanceName, signal, reconnect).
//
// The caller (a SpiceSessionManager) passes a context that is cancelled when the
// session is abandoned; every blocking step observes it.
func CreateSpiceBackend(ctx context.Context, host *db.HostsRecord, instanceName string, reconnect bool) (*SpiceBackend, error) {
	client, err := GetIncusClientForHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client.certPath == "" || client.keyPath == "" {
		return nil, errors.New("Incus connection unavailable")
	}

	transport, _ := client.http.Transport.(*http.Transport)
	if transport == nil {
		return nil, errors.New("Incus connection unavailable")
	}
	tlsConf := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // Incus is self-signed, matches rejectUnauthorized:false
	if transport.TLSClientConfig != nil {
		tlsConf = transport.TLSClientConfig.Clone()
	}
	dial := func(dialCtx context.Context, network, address string) (net.Conn, error) {
		if transport.DialContext != nil {
			// Covers the pinned and tunnelled branches: the transport already knows
			// how to reach this node, and the SPICE channels must use the same path.
			return transport.DialContext(dialCtx, network, address)
		}
		return (&net.Dialer{}).DialContext(dialCtx, network, address)
	}

	requestCtx, cancel := context.WithTimeout(ctx, spiceRequestTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"type":               "vga",
		"wait-for-websocket": true,
		"force":              reconnect,
	})
	if err != nil {
		return nil, err
	}
	consoleURL := client.baseURL + "/1.0/instances/" + url.PathEscape(instanceName) + "/console"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, consoleURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClientWithTimeout(client.http, spiceRequestTimeout).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &SpiceConnectionError{Stage: SpiceStageCreate}
	}
	defer func() { _ = response.Body.Close() }()
	payload, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return nil, &SpiceConnectionError{Stage: SpiceStageCreate, HTTPStatus: response.StatusCode}
	}

	var envelope struct {
		Type     string `json:"type"`
		Metadata struct {
			ID       string `json:"id"`
			Metadata struct {
				Fds map[string]string `json:"fds"`
			} `json:"metadata"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(payload, &envelope)
	operationID := envelope.Metadata.ID
	fds := envelope.Metadata.Metadata.Fds
	if response.StatusCode >= 400 || envelope.Type != string(IncusAPITypeAsync) || operationID == "" {
		// Do not log Incus responses: operation metadata contains channel secrets.
		return nil, &SpiceConnectionError{Stage: SpiceStageCreate, HTTPStatus: response.StatusCode}
	}

	backend := &SpiceBackend{
		baseURL: client.baseURL,
		client:  client.http,
		tlsConf: tlsConf,
		dial:    dial,
		opID:    operationID,
		fds:     fds,
		sockets: map[*WebSocket]struct{}{},
	}

	dataSecret, hasData := fds["0"]
	controlSecret, hasControl := fds["control"]
	if !hasData || dataSecret == "" || !hasControl || controlSecret == "" {
		_ = backend.Close()
		return nil, &SpiceConnectionError{Stage: SpiceStageMetadata}
	}

	control, err := backend.openSecret(ctx, controlSecret)
	if err != nil {
		_ = backend.Close()
		return nil, &SpiceConnectionError{Stage: SpiceStageControl}
	}
	backend.control = control
	return backend, nil
}

// SpiceConnectionStageOf extracts the stage of an error for logging (never the
// message: it can embed secrets).
func SpiceConnectionStageOf(err error) SpiceStage {
	var connectionError *SpiceConnectionError
	if errors.As(err, &connectionError) {
		return connectionError.Stage
	}
	return ""
}

// SpiceConnectionStatusOf extracts the HTTP status of a SpiceConnectionError.
func SpiceConnectionStatusOf(err error) int {
	var connectionError *SpiceConnectionError
	if errors.As(err, &connectionError) {
		return connectionError.HTTPStatus
	}
	return 0
}
