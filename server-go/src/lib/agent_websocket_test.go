package lib_test

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// wsClient is a bare RFC 6455 client: no third-party websocket library exists in
// this module, and the test must not add one either.
type wsClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

func wsDial(t *testing.T, serverURL, path string, extraHeaders map[string]string) (*wsClient, *http.Response) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("random key: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
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

func (c *wsClient) close() { _ = c.conn.Close() }

// writeFrame writes a masked client frame.
func (c *wsClient) writeFrame(t *testing.T, fin bool, opcode byte, payload []byte) {
	t.Helper()
	header := make([]byte, 0, 14)
	first := opcode
	if fin {
		first |= 0x80
	}
	header = append(header, first)
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length)|0x80)
	case length <= 65535:
		header = append(header, 126|0x80)
		header = binary.BigEndian.AppendUint16(header, uint16(length))
	default:
		header = append(header, 127|0x80)
		header = binary.BigEndian.AppendUint64(header, uint64(length))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		t.Fatalf("random mask: %v", err)
	}
	header = append(header, mask...)
	masked := make([]byte, len(payload))
	for index := range payload {
		masked[index] = payload[index] ^ mask[index%4]
	}
	if _, err := c.conn.Write(append(header, masked...)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// readFrame reads one unmasked server frame.
func (c *wsClient) readFrame(t *testing.T) (bool, byte, []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0F
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(c.reader, extended); err != nil {
			t.Fatalf("read extended length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(c.reader, extended); err != nil {
			t.Fatalf("read extended length: %v", err)
		}
		length = binary.BigEndian.Uint64(extended)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return fin, opcode, payload
}

func TestAgentWebSocketHandshakeAndEcho(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeAgentWebSocket(w, r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			message, err := socket.ReadBinary()
			if err != nil {
				return
			}
			if err := socket.TunnelSend(message); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	if !strings.HasPrefix(server.URL, "http://") {
		t.Fatalf("unexpected server url %q", server.URL)
	}
	client, response := wsDial(t, server.URL, "/ws", nil)
	defer client.close()

	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	if got := response.Header.Get("Sec-WebSocket-Accept"); len(got) != 28 {
		t.Fatalf("Sec-WebSocket-Accept = %q", got)
	}
	if upgrade := response.Header.Get("Upgrade"); !strings.EqualFold(upgrade, "websocket") {
		t.Fatalf("Upgrade = %q", upgrade)
	}

	// Binary echo (the tunnel uses binary frames only).
	client.writeFrame(t, true, 0x2, []byte("hello tunnel"))
	_, opcode, payload := client.readFrame(t)
	if opcode != 0x2 || string(payload) != "hello tunnel" {
		t.Fatalf("echo = opcode %d payload %q", opcode, payload)
	}

	// Ping gets an automatic pong with the same payload.
	client.writeFrame(t, true, 0x9, []byte("ping"))
	_, opcode, payload = client.readFrame(t)
	if opcode != 0xA || string(payload) != "ping" {
		t.Fatalf("pong = opcode %d payload %q", opcode, payload)
	}

	// Fragmented text frames are reassembled.
	client.writeFrame(t, false, 0x1, []byte("frag-"))
	client.writeFrame(t, true, 0x0, []byte("ment"))
	_, opcode, payload = client.readFrame(t)
	if opcode != 0x2 || string(payload) != "frag-ment" {
		t.Fatalf("fragmented echo = opcode %d payload %q", opcode, payload)
	}

	// Close is echoed and tears the connection down.
	closePayload := binary.BigEndian.AppendUint16(nil, 1000)
	client.writeFrame(t, true, 0x8, closePayload)
	_, opcode, payload = client.readFrame(t)
	if opcode != 0x8 {
		t.Fatalf("close echo opcode = %d", opcode)
	}
	if code := binary.BigEndian.Uint16(payload); code != 1000 {
		t.Fatalf("close echo code = %d, want 1000", code)
	}
}

func TestAgentWebSocketRejectsOversizedMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeAgentWebSocket(w, r)
		if err != nil {
			return
		}
		_, _ = socket.ReadBinary()
	}))
	defer server.Close()

	client, response := wsDial(t, server.URL, "/ws", nil)
	defer client.close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", response.StatusCode)
	}

	// Declare a payload above the 1 MiB maxPayload. The server checks the length
	// before reading the body, so nothing else has to be sent.
	header := []byte{0x82, 127 | 0x80}
	header = binary.BigEndian.AppendUint64(header, lib.AgentWebSocketMaxPayload+1)
	header = append(header, 1, 2, 3, 4)
	if _, err := client.conn.Write(header); err != nil {
		t.Fatalf("write oversized header: %v", err)
	}
	_, opcode, payload := client.readFrame(t)
	if opcode != 0x8 {
		t.Fatalf("expected close frame, got opcode %d", opcode)
	}
	if code := binary.BigEndian.Uint16(payload); code != 1009 {
		t.Fatalf("close code = %d, want 1009 (message too big)", code)
	}
}

func TestAgentWebSocketRejectsUnmaskedClientFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeAgentWebSocket(w, r)
		if err != nil {
			return
		}
		_, _ = socket.ReadBinary()
	}))
	defer server.Close()

	client, _ := wsDial(t, server.URL, "/ws", nil)
	defer client.close()
	// An unmasked client frame violates RFC 6455 §5.1.
	if _, err := client.conn.Write([]byte{0x82, 0x02, 'h', 'i'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The server drops the connection without a close handshake.
	if _, err := client.reader.ReadByte(); err == nil {
		t.Fatal("expected the connection to be closed")
	}
}

func TestAgentWebSocketUpgradeRequirements(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if lib.IsAgentWebSocketUpgrade(request) {
		t.Fatal("plain GET reported as upgrade")
	}
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "keep-alive, Upgrade")
	if !lib.IsAgentWebSocketUpgrade(request) {
		t.Fatal("Upgrade + Connection token not detected")
	}
	request.Header.Set("Sec-WebSocket-Version", "12")
	recorder := httptest.NewRecorder()
	if _, err := lib.UpgradeAgentWebSocket(recorder, request); err == nil ||
		!strings.Contains(err.Error(), "unsupported websocket version") {
		t.Fatalf("version 12 error = %v", err)
	}
}

// TestAgentWebSocketTunnelRegistration proves the socket satisfies TunnelSocket
// and that the manager sends its CONTROL_HELLO on registration and reports the
// disconnect on close.
func TestAgentWebSocketTunnelRegistration(t *testing.T) {
	manager := lib.NewTunnelManager(nil)
	hostID := int32(4242)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeAgentWebSocket(w, r)
		if err != nil {
			return
		}
		manager.RegisterTunnel(hostID, socket)
		lib.AgentServeTunnel(hostID, manager, socket)
	}))
	defer server.Close()

	client, _ := wsDial(t, server.URL, "/ws", nil)
	defer client.close()

	// CONTROL_HELLO is the first frame after registration.
	_, opcode, payload := client.readFrame(t)
	if opcode != 0x2 {
		t.Fatalf("hello opcode = %d", opcode)
	}
	frame, err := lib.TunnelDecodeFrame(payload)
	if err != nil {
		t.Fatalf("decode hello frame: %v", err)
	}
	if frame.Type != lib.TunnelFrameControlHello {
		t.Fatalf("hello frame type = %v", frame.Type)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !manager.IsTunnelOnline(hostID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !manager.IsTunnelOnline(hostID) {
		t.Fatal("manager does not consider the tunnel online")
	}

	client.close()
	for manager.IsTunnelOnline(hostID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if manager.IsTunnelOnline(hostID) {
		t.Fatal("tunnel still online after the client closed")
	}
}
