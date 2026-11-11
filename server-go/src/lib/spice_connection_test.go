// Tests for the Incus graphical-console backend (port of
// lib/incus/spice-connection.ts) against a fake Incus node.
package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// TestSpiceConnectionBackendWithFakeIncus drives the whole backend lifecycle:
// POST /console (VGA, wait-for-websocket, takeover flag), control + data channels,
// and the DELETE/GET operation cleanup.
func TestSpiceConnectionBackendWithFakeIncus(t *testing.T) {
	const hostID = int32(910002)
	const operationID = "op-spice-test"
	RemoveIncusClient(int64(hostID))
	t.Cleanup(func() { RemoveIncusClient(int64(hostID)) })

	controlSockets := make(chan *WebSocket, 1)
	dataSockets := make(chan *WebSocket, 1)
	var consoleBody map[string]any
	cleanupCalls := make(chan string, 4)

	fakeIncus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/1.0":
			_, _ = w.Write([]byte(`{"type":"sync","metadata":{"environment":{"server":"fake"}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/instances/test-vm/console":
			var decoded map[string]any
			_ = json.NewDecoder(r.Body).Decode(&decoded)
			consoleBody = decoded
			_, _ = w.Write([]byte(`{"type":"async","status":"Operation created","operation":"/1.0/operations/` + operationID +
				`","metadata":{"id":"` + operationID + `","metadata":{"fds":{"0":"data-secret","control":"control-secret"}}}}`))
		case IsWebSocketUpgrade(r) && strings.HasSuffix(r.URL.Path, "/operations/"+operationID+"/websocket"):
			socket, err := UpgradeWebSocket(w, r, nil)
			if err != nil {
				return
			}
			if r.URL.Query().Get("secret") == "control-secret" {
				controlSockets <- socket
				for {
					if _, _, readErr := socket.ReadMessage(); readErr != nil {
						return
					}
				}
			}
			dataSockets <- socket
			for {
				payload, _, readErr := socket.ReadMessage()
				if readErr != nil {
					return
				}
				_ = socket.SendBinary(payload)
			}
		case r.URL.Path == "/1.0/operations/"+operationID:
			cleanupCalls <- r.Method
			_, _ = w.Write([]byte(`{"type":"sync","metadata":{"status":"Success"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fakeIncus.Close()

	certPath, keyPath := terminalTestCertificates(t)
	host := &db.HostsRecord{
		ID:         hostID,
		Name:       "fake-spice-incus",
		URL:        fakeIncus.URL,
		CertPath:   &certPath,
		KeyPath:    &keyPath,
		TargetHost: "127.0.0.1",
		TargetPort: 8443,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend, err := CreateSpiceBackend(ctx, host, "test-vm", false)
	if err != nil {
		t.Fatalf("CreateSpiceBackend: %v", err)
	}
	if backend.Control() == nil || !backend.Control().IsOpen() {
		t.Fatal("control channel not open")
	}
	if consoleBody["type"] != "vga" || consoleBody["wait-for-websocket"] != true || consoleBody["force"] != false {
		t.Fatalf("console request body = %v", consoleBody)
	}
	<-controlSockets

	data, err := backend.OpenData(ctx)
	if err != nil {
		t.Fatalf("OpenData: %v", err)
	}
	dataServerSide := <-dataSockets
	if err := data.SendBinary([]byte("screen")); err != nil {
		t.Fatalf("data send: %v", err)
	}
	payload, isBinary, err := readWSMessage(t, data)
	if err != nil || !isBinary || string(payload) != "screen" {
		t.Fatalf("data echo = (%q,%t,%v)", payload, isBinary, err)
	}

	if err := backend.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case method := <-cleanupCalls:
		if method != http.MethodDelete && method != http.MethodGet {
			t.Fatalf("cleanup used %s", method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never reached the operation endpoint")
	}
	if backend.Control().IsOpen() {
		t.Fatal("control channel must be terminated by Close")
	}
	if dataServerSide.IsOpen() {
		t.Fatal("data channel must be terminated by Close")
	}
}

// TestSpiceConnectionCreateErrorShape pins the create-stage error when Incus
// answers with an error envelope.
func TestSpiceConnectionCreateErrorShape(t *testing.T) {
	const hostID = int32(910003)
	RemoveIncusClient(int64(hostID))
	t.Cleanup(func() { RemoveIncusClient(int64(hostID)) })

	fakeIncus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/1.0" {
			_, _ = w.Write([]byte(`{"type":"sync","metadata":{}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":"boom","error_code":500}`))
	}))
	defer fakeIncus.Close()

	certPath, keyPath := terminalTestCertificates(t)
	host := &db.HostsRecord{ID: hostID, URL: fakeIncus.URL, CertPath: &certPath, KeyPath: &keyPath}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := CreateSpiceBackend(ctx, host, "test-vm", false); err == nil {
		t.Fatal("expected a create-stage failure")
	} else if SpiceConnectionStageOf(err) != SpiceStageCreate && SpiceConnectionStageOf(err) != "" {
		t.Fatalf("stage = %q, want create", SpiceConnectionStageOf(err))
	}
}
