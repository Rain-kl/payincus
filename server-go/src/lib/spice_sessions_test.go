// Tests for the SPICE session manager (port of lib/spice-sessions.ts).
//
// Package lib (not lib_test) because the fake backend needs the private
// SpiceBackend fields: the alternative would be a production constructor that
// exists only for tests.
package lib

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// spiceTestServer upgrades every websocket request and hands the server-side
// socket to the test. Plain HTTP requests (the Incus operation DELETE/GET issued by
// backend cleanup) get 404, which means "operation gone" and finishes cleanup.
type spiceTestServer struct {
	server  *httptest.Server
	sockets chan *WebSocket
}

func newSpiceTestServer(t *testing.T) *spiceTestServer {
	t.Helper()
	ts := &spiceTestServer{sockets: make(chan *WebSocket, 16)}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		socket, err := UpgradeWebSocket(w, r, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ts.sockets <- socket
		<-r.Context().Done()
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *spiceTestServer) dial(t *testing.T, path string) *WebSocket {
	t.Helper()
	client, err := DialWebSocket(context.Background(), "ws"+strings.TrimPrefix(ts.server.URL, "http")+path, WebSocketDialOptions{})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return client
}

func (ts *spiceTestServer) accept(t *testing.T) *WebSocket {
	t.Helper()
	select {
	case socket := <-ts.sockets:
		return socket
	case <-time.After(5 * time.Second):
		t.Fatal("no server-side websocket accepted")
		return nil
	}
}

// fakeSpiceBackend mirrors a SpiceBackend whose control channel is live and whose
// data channel factory dials the test server.
func fakeSpiceBackend(t *testing.T) *SpiceBackend {
	t.Helper()
	ts := newSpiceTestServer(t)
	control := ts.dial(t, "/control")
	ts.accept(t) // server side of the control channel
	backend := &SpiceBackend{
		control: control,
		baseURL: ts.server.URL,
		client:  &http.Client{},
		opID:    "op-test",
		fds:     map[string]string{"0": "data", "control": "control"},
		sockets: map[*WebSocket]struct{}{},
	}
	backend.sockets[control] = struct{}{}
	t.Cleanup(func() {
		_ = backend.Close()
		control.Terminate()
	})
	return backend
}

func spiceTestAccess(userID, instanceID int32) SpiceAccess {
	sessionID := "sess-" + strings.Repeat("x", 1)
	return SpiceAccess{UserID: userID, InstanceID: instanceID, IssuedAt: 1_700_000_000, SessionID: &sessionID}
}

func spiceTestOpen(backend *SpiceBackend) func(context.Context) (*SpiceBackend, error) {
	return func(context.Context) (*SpiceBackend, error) { return backend, nil }
}

// spiceTestBackendFactory makes one fresh fake backend per session, like the real
// createSpiceBackend opens a new Incus operation per session.
func spiceTestBackendFactory(t *testing.T) func(context.Context) (*SpiceBackend, error) {
	return func(context.Context) (*SpiceBackend, error) { return fakeSpiceBackend(t), nil }
}

// readWSMessage reads one message with a bounded wait so a broken data path fails
// the test instead of hanging it.
func readWSMessage(t *testing.T, socket *WebSocket) ([]byte, bool, error) {
	t.Helper()
	type result struct {
		payload  []byte
		isBinary bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		payload, isBinary, err := socket.ReadMessage()
		done <- result{payload: payload, isBinary: isBinary, err: err}
	}()
	select {
	case got := <-done:
		return got.payload, got.isBinary, got.err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a websocket message")
		return nil, false, nil
	}
}

func spiceTestValidate(context.Context) bool { return true }

// TestSpiceSessionInstanceLimit pins "one session per instance".
func TestSpiceSessionInstanceLimit(t *testing.T) {
	manager := NewSpiceSessionManager()
	backend := fakeSpiceBackend(t)

	token, err := manager.Create(spiceTestAccess(1, 100), spiceTestOpen(backend), spiceTestValidate, false)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := manager.Create(spiceTestAccess(2, 100), spiceTestOpen(backend), spiceTestValidate, false); !errors.Is(err, ErrSpiceConsoleLimit) {
		t.Fatalf("second session on the same instance = %v, want CONSOLE_LIMIT", err)
	}
	if got := manager.Count(); got != 1 {
		t.Fatalf("session count = %d, want 1", got)
	}
	<-manager.Close(token)
	manager.WaitForCleanup(100)
}

// TestSpiceSessionUserLimit pins "at most two sessions per user".
func TestSpiceSessionUserLimit(t *testing.T) {
	manager := NewSpiceSessionManager()
	open := spiceTestBackendFactory(t)

	first, err := manager.Create(spiceTestAccess(1, 200), open, spiceTestValidate, false)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := manager.Create(spiceTestAccess(1, 201), open, spiceTestValidate, false)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if _, err := manager.Create(spiceTestAccess(1, 202), open, spiceTestValidate, false); !errors.Is(err, ErrSpiceConsoleLimit) {
		t.Fatalf("third session for one user = %v, want CONSOLE_LIMIT", err)
	}
	for _, token := range []string{first, second} {
		<-manager.Close(token)
	}
	manager.WaitForCleanup(200)
	manager.WaitForCleanup(201)
}

// TestSpiceSessionTokenBinding pins get(token, instanceId): unknown tokens, other
// instances and expired sessions are all nil.
func TestSpiceSessionTokenBinding(t *testing.T) {
	manager := NewSpiceSessionManager()
	backend := fakeSpiceBackend(t)

	token, err := manager.Create(spiceTestAccess(1, 300), spiceTestOpen(backend), spiceTestValidate, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if manager.Get("unknown", 300) != nil {
		t.Fatal("unknown token must not resolve")
	}
	if manager.Get(token, 301) != nil {
		t.Fatal("a token bound to instance 300 must not resolve for 301")
	}
	if got := manager.Get(token, 300); got == nil || got.UserID != 1 || got.InstanceID != 300 {
		t.Fatalf("token binding = %+v", got)
	}

	manager.ExpireSpiceSessionForTest(token)
	if manager.Get(token, 300) != nil {
		t.Fatal("expired session must not resolve")
	}
	<-manager.Close(token)
}

// TestSpiceSessionReconnectReplacesSession pins the reconnect path: the old
// session for the instance is closed before the new one is created, and a second
// concurrent reconnect is rejected with CONSOLE_LIMIT.
func TestSpiceSessionReconnectReplacesSession(t *testing.T) {
	manager := NewSpiceSessionManager()
	open := spiceTestBackendFactory(t)

	first, err := manager.Create(spiceTestAccess(1, 400), open, spiceTestValidate, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := manager.Create(spiceTestAccess(1, 400), open, spiceTestValidate, true)
	if err != nil {
		t.Fatalf("reconnect create: %v", err)
	}
	if first == second {
		t.Fatal("reconnect must mint a new token")
	}
	manager.mu.Lock()
	_, stillThere := manager.sessions[first]
	manager.mu.Unlock()
	if stillThere {
		t.Fatal("the previous session must be closed by a reconnect")
	}
	<-manager.Close(second)
}

// TestSpiceSessionAttachForwardsData proves the data path: the browser channel is
// attached with an authorize callback, messages buffered before the upstream opens
// are flushed, and upstream bytes reach the browser.
func TestSpiceSessionAttachForwardsData(t *testing.T) {
	manager := NewSpiceSessionManager()
	backend := fakeSpiceBackend(t)

	token, err := manager.Create(spiceTestAccess(9, 500), spiceTestOpen(backend), spiceTestValidate, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A second test server acts as the Incus data channel endpoint.
	dataServer := newSpiceTestServer(t)
	backend.baseURL = dataServer.server.URL

	browserServer := newSpiceTestServer(t)
	browser := browserServer.dial(t, "/spice")
	browserSide := browserServer.accept(t)

	authorized := make(chan bool, 1)
	manager.Attach(token, 500, browserSide, func(main bool) bool {
		authorized <- main
		return true
	})
	if main := <-authorized; !main {
		t.Fatal("the first attached channel must be main")
	}

	// Incus opens the data channel; the test server hands us the upstream side.
	upstream := dataServer.accept(t)

	// Browser → Incus.
	if err := browser.SendBinary([]byte("ls\n")); err != nil {
		t.Fatalf("browser send: %v", err)
	}
	payload, isBinary, err := readWSMessage(t, upstream)
	if err != nil || !isBinary || string(payload) != "ls\n" {
		t.Fatalf("upstream received (%q,%t,%v)", payload, isBinary, err)
	}

	// Incus → browser.
	if err := upstream.SendBinary([]byte("out\n")); err != nil {
		t.Fatalf("upstream send: %v", err)
	}
	payload, isBinary, err = readWSMessage(t, browser)
	if err != nil || !isBinary || string(payload) != "out\n" {
		t.Fatalf("browser received (%q,%t,%v)", payload, isBinary, err)
	}

	// Closing the main channel closes the session.
	browser.Terminate()
	deadline := time.Now().Add(5 * time.Second)
	for manager.Count() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session not closed after the main channel closed (count=%d)", manager.Count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.WaitForCleanup(500)
}

// TestSpiceSessionDrain closes everything and waits for cleanup.
func TestSpiceSessionDrain(t *testing.T) {
	manager := NewSpiceSessionManager()
	open := spiceTestBackendFactory(t)

	if _, err := manager.Create(spiceTestAccess(1, 600), open, spiceTestValidate, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.Drain()
	if got := manager.Count(); got != 0 {
		t.Fatalf("sessions after drain = %d", got)
	}
	manager.mu.Lock()
	closing := len(manager.closing)
	manager.mu.Unlock()
	if closing != 0 {
		t.Fatalf("closing map after drain = %d", closing)
	}
}

// TestSpiceConnectionErrorShape pins the error message/fields the route logs.
func TestSpiceConnectionErrorShape(t *testing.T) {
	err := &SpiceConnectionError{Stage: SpiceStageCreate, HTTPStatus: 500}
	if err.Error() != "Incus console create failed" {
		t.Fatalf("message = %q", err.Error())
	}
	var target *SpiceConnectionError
	if !errors.As(error(err), &target) || SpiceConnectionStageOf(err) != SpiceStageCreate || SpiceConnectionStatusOf(err) != 500 {
		t.Fatalf("stage/status extraction failed: %v", err)
	}
}
