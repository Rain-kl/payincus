// Minimal RFC 6455 server-side WebSocket for the host-agent tunnel.
//
// Why hand-rolled: the Node backend gets WebSockets from @fastify/websocket (ws),
// but this Go module has no websocket dependency in go.mod (golang.org/x/net is
// only an indirect dependency and its legacy websocket package is deprecated).
// Adding a module is not allowed for this migration step, so the small subset the
// tunnel needs is implemented here:
//
//   - the opening handshake (Sec-WebSocket-Accept = base64(sha1(key + GUID)));
//   - binary frames in both directions, with the 1 MiB maxPayload @fastify/websocket
//     is configured with (a larger message closes the socket with 1009);
//   - ping → pong, pong → ignore, close → echo + teardown;
//   - fragmented messages (continuation frames) reassembled.
//
// No extensions (permessage-deflate) and no subprotocols are negotiated — the node
// agent never asks for them, and refusing them keeps the framing predictable.
package lib

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// AgentWebSocketMaxPayload mirrors @fastify/websocket's maxPayload (1 MiB).
const AgentWebSocketMaxPayload = 1048576

// agentWebSocketGUID is the RFC 6455 handshake magic string.
const agentWebSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes (RFC 6455 §5.2).
const (
	agentWSContinuation = 0x0
	agentWSText         = 0x1
	agentWSBinary       = 0x2
	agentWSClose        = 0x8
	agentWSPing         = 0x9
	agentWSPong         = 0xA
)

// ErrAgentWebSocketClosed is returned by ReadBinary once the peer closed the
// socket (or the connection failed).
var ErrAgentWebSocketClosed = errors.New("websocket closed")

// ErrAgentWebSocketNotUpgrade reports a request without the required upgrade
// headers.
var ErrAgentWebSocketNotUpgrade = errors.New("expected websocket upgrade")

// AgentWebSocket is a server-side WebSocket that satisfies TunnelSocket.
type AgentWebSocket struct {
	conn   net.Conn
	reader *bufio.Reader

	writeMu sync.Mutex
	readMu  sync.Mutex

	stateMu sync.Mutex
	closed  bool
	// closeSent records that a close frame was already written, so the echo path
	// does not try to send a second one.
	closeSent bool
}

var _ TunnelSocket = (*AgentWebSocket)(nil)

// IsAgentWebSocketUpgrade mirrors the check @fastify/websocket applies: an
// `Upgrade: websocket` request with `Connection` containing the upgrade token.
func IsAgentWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// UpgradeAgentWebSocket performs the RFC 6455 opening handshake on the hijacked
// connection. The caller owns authentication: it happens AFTER the upgrade, so an
// unauthenticated agent gets a close code (4001) exactly like the Node route.
func UpgradeAgentWebSocket(w http.ResponseWriter, r *http.Request) (*AgentWebSocket, error) {
	if !IsAgentWebSocketUpgrade(r) {
		return nil, ErrAgentWebSocketNotUpgrade
	}
	if version := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); version != "13" {
		return nil, fmt.Errorf("unsupported websocket version %q", version)
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return nil, errors.New("invalid Sec-WebSocket-Key")
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijacking")
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}

	acceptHash := sha1.Sum([]byte(key + agentWebSocketGUID))
	accept := base64.StdEncoding.EncodeToString(acceptHash[:])
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buffered.WriteString(response); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// No read deadline: an upgraded ws connection in Node has none either (the
	// keepAlive/request timeouts apply to pre-upgrade HTTP requests only). The
	// tunnel manager owns liveness through its own heartbeats.
	return &AgentWebSocket{conn: conn, reader: buffered.Reader}, nil
}

// TunnelReadyState mirrors WebSocket#readyState: 1 = OPEN, 3 = CLOSED.
func (s *AgentWebSocket) TunnelReadyState() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return 3
	}
	return TunnelSocketOpen
}

// TunnelSend writes one binary frame (Node's `ws.send(Buffer)` uses binary).
func (s *AgentWebSocket) TunnelSend(data []byte) error {
	return s.writeFrame(agentWSBinary, data)
}

// TunnelClose mirrors `ws.close(code, reason)`: send a close frame, then close
// the TCP connection.
func (s *AgentWebSocket) TunnelClose(code int, reason string) {
	s.stateMu.Lock()
	alreadyClosed := s.closed
	s.stateMu.Unlock()
	if alreadyClosed {
		return
	}
	payload := make([]byte, 0, len(reason)+2)
	if code == 0 {
		code = 1000
	}
	payload = binary.BigEndian.AppendUint16(payload, uint16(code))
	// A control frame payload is limited to 125 bytes.
	if len(reason) > 123 {
		reason = reason[:123]
	}
	payload = append(payload, reason...)
	_ = s.writeCloseFrame(payload)
	_ = s.conn.Close()
	s.markClosed()
}

func (s *AgentWebSocket) markClosed() {
	s.stateMu.Lock()
	s.closed = true
	s.stateMu.Unlock()
}

func (s *AgentWebSocket) writeCloseFrame(payload []byte) error {
	s.stateMu.Lock()
	if s.closeSent {
		s.stateMu.Unlock()
		return nil
	}
	s.closeSent = true
	s.stateMu.Unlock()
	return s.writeFrame(agentWSClose, payload)
}

// writeFrame serialises one unmasked (server → client) frame.
func (s *AgentWebSocket) writeFrame(opcode byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.TunnelReadyState() != TunnelSocketOpen {
		return ErrAgentWebSocketClosed
	}

	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode)
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length))
	case length <= 65535:
		header = append(header, 126)
		header = binary.BigEndian.AppendUint16(header, uint16(length))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(length))
	}
	if _, err := s.conn.Write(header); err != nil {
		s.markClosed()
		return err
	}
	if length > 0 {
		if _, err := s.conn.Write(payload); err != nil {
			s.markClosed()
			return err
		}
	}
	return nil
}

// readFrame reads one frame and validates the RFC constraints the agent must
// honour (masked client frames, no reserved bits, control limits).
func (s *AgentWebSocket) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(s.reader, header); err != nil {
		return false, 0, nil, err
	}
	fin = header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return false, 0, nil, errors.New("reserved bits set")
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(s.reader, extended); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(s.reader, extended); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended)
	}
	if opcode >= agentWSClose && (!fin || length > 125) {
		return false, 0, nil, errors.New("invalid control frame")
	}
	if length > AgentWebSocketMaxPayload {
		// ws closes with 1009 "Message Too Big".
		s.TunnelClose(1009, "Message Too Big")
		return false, 0, nil, ErrAgentWebSocketClosed
	}
	if !masked {
		return false, 0, nil, errors.New("client frames must be masked")
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(s.reader, mask); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(s.reader, payload); err != nil {
		return false, 0, nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%4]
	}
	return fin, opcode, payload, nil
}

// ReadBinary returns the payload of the next binary/text message, answering
// control frames on the way. Fragmented data frames are reassembled.
func (s *AgentWebSocket) ReadBinary() ([]byte, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	var (
		message    []byte
		messageSet bool
	)
	for {
		fin, opcode, payload, err := s.readFrame()
		if err != nil {
			s.markClosed()
			return nil, ErrAgentWebSocketClosed
		}
		switch opcode {
		case agentWSPing:
			if writeErr := s.writeFrame(agentWSPong, payload); writeErr != nil {
				return nil, ErrAgentWebSocketClosed
			}
			continue
		case agentWSPong:
			continue
		case agentWSClose:
			_ = s.writeCloseFrame(payload)
			_ = s.conn.Close()
			s.markClosed()
			return nil, ErrAgentWebSocketClosed
		case agentWSBinary, agentWSText:
			if messageSet {
				return nil, errors.New("unexpected new data frame inside a fragmented message")
			}
			if fin {
				return payload, nil
			}
			message = append(message, payload...)
			messageSet = true
		case agentWSContinuation:
			if !messageSet {
				return nil, errors.New("continuation without a message")
			}
			message = append(message, payload...)
			if fin {
				return message, nil
			}
		default:
			return nil, fmt.Errorf("unsupported opcode %d", opcode)
		}
	}
}

// AgentServeTunnel runs the authenticated tunnel loop: feed every message to the
// manager and report the close exactly once. This is the Go equivalent of the
// TypeScript route's `socket.on('message')` / `socket.on('close')` wiring.
func AgentServeTunnel(hostID int32, manager *TunnelManager, socket *AgentWebSocket) {
	defer func() {
		socket.markClosed()
		_ = socket.conn.Close()
		manager.HandleTunnelClosed(hostID, socket)
	}()
	for {
		message, err := socket.ReadBinary()
		if err != nil {
			return
		}
		manager.HandleTunnelMessage(hostID, socket, message)
	}
}
