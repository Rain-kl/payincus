package lib_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// TestWebSocketServerHandshake pins the RFC 6455 opening handshake of the shared
// engine: 101, the exact Sec-WebSocket-Accept value, and the `ws`-style default
// subprotocol negotiation (echo the first protocol the client offered — this is
// what keeps the SPICE ticket out of the response).
func TestWebSocketServerHandshake(t *testing.T) {
	var serverSocket *lib.WebSocket
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		serverSocket = socket
		close(ready)
		<-r.Context().Done()
	}))
	defer server.Close()

	key := "dGhlIHNhbXBsZSBub25jZQ=="
	client, response := wsDialWithKey(t, server.URL, "/ws", key, map[string]string{
		"Sec-WebSocket-Protocol": "binary, incudal-spice.ticket123",
	})
	defer client.close()

	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	sum := sha1.Sum([]byte(key + lib.WebSocketGUID))
	wantAccept := base64.StdEncoding.EncodeToString(sum[:])
	if got := response.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, wantAccept)
	}
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != "binary" {
		t.Fatalf("negotiated protocol = %q, want binary (the first one offered)", got)
	}
	<-ready
	if got := serverSocket.Subprotocol(); got != "binary" {
		t.Fatalf("server socket subprotocol = %q", got)
	}
}

// TestWebSocketUpgradeRequirements pins the pre-handshake rejections.
func TestWebSocketUpgradeRequirements(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if _, err := lib.UpgradeWebSocket(httptest.NewRecorder(), request, nil); err != lib.ErrWebSocketNotUpgrade {
		t.Fatalf("plain GET error = %v, want ErrWebSocketNotUpgrade", err)
	}

	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Sec-WebSocket-Version", "12")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if _, err := lib.UpgradeWebSocket(httptest.NewRecorder(), request, nil); err == nil ||
		!strings.Contains(err.Error(), "unsupported websocket version") {
		t.Fatalf("version 12 error = %v", err)
	}

	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "too-short")
	if _, err := lib.UpgradeWebSocket(httptest.NewRecorder(), request, nil); err == nil ||
		!strings.Contains(err.Error(), "invalid Sec-WebSocket-Key") {
		t.Fatalf("short key error = %v", err)
	}
}

// TestWebSocketServerCloseCode proves a close with a 4xxx application code and a
// reason reaches the peer as a close frame (the terminal/SPICE rejection codes).
func TestWebSocketServerCloseCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, nil)
		if err != nil {
			return
		}
		socket.Close(4001, "Unauthorized")
	}))
	defer server.Close()

	client, response := wsDial(t, server.URL, "/ws", nil)
	defer client.close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}

	_, opcode, payload := client.readFrame(t)
	if opcode != lib.WebSocketClose {
		t.Fatalf("opcode = %d, want close", opcode)
	}
	if len(payload) < 2 {
		t.Fatalf("close payload too short: %x", payload)
	}
	if code := binary.BigEndian.Uint16(payload); code != 4001 {
		t.Fatalf("close code = %d, want 4001", code)
	}
	if reason := string(payload[2:]); reason != "Unauthorized" {
		t.Fatalf("close reason = %q", reason)
	}
}

// TestWebSocketServerRejectsMaskedAndUnmaskedFrames pins RFC 6455 §5.1 in both
// directions of the shared engine.
func TestWebSocketServerRejectsUnmaskedFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = socket.ReadMessage()
	}))
	defer server.Close()

	client, _ := wsDial(t, server.URL, "/ws", nil)
	defer client.close()
	if _, err := client.conn.Write([]byte{0x82, 0x02, 'h', 'i'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.reader.ReadByte(); err == nil {
		t.Fatal("expected the connection to be dropped")
	}
}

// TestWebSocketServerPayloadLimit pins maxPayload: an oversized message closes
// with 1009 before its body is read.
func TestWebSocketServerPayloadLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, &lib.WebSocketUpgradeOptions{MaxPayload: 8})
		if err != nil {
			return
		}
		_, _, _ = socket.ReadMessage()
	}))
	defer server.Close()

	client, response := wsDial(t, server.URL, "/ws", nil)
	defer client.close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", response.StatusCode)
	}
	client.writeFrame(t, true, lib.WebSocketBinary, []byte("123456789"))
	_, opcode, payload := client.readFrame(t)
	if opcode != lib.WebSocketClose {
		t.Fatalf("opcode = %d, want close", opcode)
	}
	if code := binary.BigEndian.Uint16(payload); code != 1009 {
		t.Fatalf("close code = %d, want 1009", code)
	}
}

// TestWebSocketClientDialEcho exercises the client endpoint of the shared engine
// (used by the terminal proxy and the SPICE backend): masking, text and binary
// frames, and the negotiated subprotocol.
func TestWebSocketClientDialEcho(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, nil)
		if err != nil {
			return
		}
		for {
			payload, isBinary, err := socket.ReadMessage()
			if err != nil {
				return
			}
			if err := socket.SendMessage(payload, isBinary); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client, err := lib.DialWebSocket(context.Background(), wsURL, lib.WebSocketDialOptions{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     []string{"binary"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Terminate()
	if !client.IsOpen() {
		t.Fatal("client not open")
	}

	if err := client.SendBinary([]byte("hello")); err != nil {
		t.Fatalf("send binary: %v", err)
	}
	payload, isBinary, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if !isBinary || string(payload) != "hello" {
		t.Fatalf("binary echo = (%q,%t)", payload, isBinary)
	}

	if err := client.SendText("world"); err != nil {
		t.Fatalf("send text: %v", err)
	}
	payload, isBinary, err = client.ReadMessage()
	if err != nil {
		t.Fatalf("read text: %v", err)
	}
	if isBinary || string(payload) != "world" {
		t.Fatalf("text echo = (%q,%t)", payload, isBinary)
	}
}

// TestWebSocketPingPong proves the engine answers control frames on both ends and
// surfaces pongs through the handler (the heartbeat contract of both consoles).
func TestWebSocketPingPong(t *testing.T) {
	serverPong := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, nil)
		if err != nil {
			return
		}
		socket.SetPongHandler(func() { serverPong <- struct{}{} })
		if err := socket.Ping(); err != nil {
			return
		}
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client, err := lib.DialWebSocket(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), lib.WebSocketDialOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Terminate()
	clientPong := make(chan struct{}, 4)
	client.SetPongHandler(func() { clientPong <- struct{}{} })

	// A reader loop is what answers pings automatically on the client side.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// The client answered the server's ping.
	select {
	case <-serverPong:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received a pong")
	}

	// And the server answers a client ping.
	if err := client.Ping(); err != nil {
		t.Fatalf("client ping: %v", err)
	}
	select {
	case <-clientPong:
	case <-time.After(5 * time.Second):
		t.Fatal("client never received a pong")
	}
	client.Terminate()
	<-readDone
}

// wsDialWithKey is wsDial with a caller-chosen Sec-WebSocket-Key, so the accept
// hash can be checked against the RFC example.
func wsDialWithKey(t *testing.T, serverURL, path, key string, extraHeaders map[string]string) (*wsClient, *http.Response) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	request := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	for name, value := range extraHeaders {
		request += name + ": " + value + "\r\n"
	}
	request += "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	return &wsClient{conn: conn, reader: reader}, response
}
