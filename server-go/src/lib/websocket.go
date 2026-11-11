// Generic RFC 6455 WebSocket, both endpoints, for the Wave E long-lived
// connections (host-agent tunnel, terminal proxy, SPICE console).
//
// Why hand-rolled: this module has no websocket dependency in go.mod and adding
// one is not allowed for this migration step (golang.org/x/net's legacy websocket
// package is deprecated and x/net is only an indirect dependency). The subset the
// Node backend uses through `ws` (and @fastify/websocket) is implemented here:
//
//   - the opening handshake in both directions
//     (Sec-WebSocket-Accept = base64(sha1(key + GUID)), version 13, one
//     subprotocol negotiated the way `ws` does it: the first offered one wins
//     unless the caller selects another);
//   - text and binary frames, with client→server masking enforced and
//     server→client masking rejected (RFC 6455 §5.1);
//   - ping → pong automatically answered, pong surfaced through a hook;
//   - close frames carrying code + reason in both directions;
//   - fragmented messages (continuation frames) reassembled;
//   - a configurable max message size. Exceeding it closes with 1009, exactly
//     like `ws`'s `maxPayload` (`@fastify/websocket` defaults to 1 MiB, the SPICE
//     backend sets 16 MiB).
//
// No extensions (permessage-deflate) and no compression: neither the node agent
// nor the browser terminal negotiates them through this backend, and refusing them
// keeps the framing predictable.
package lib

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultWebSocketMaxPayload mirrors `ws`'s default maxPayload and the
// @fastify/websocket server default (1 MiB).
const DefaultWebSocketMaxPayload int64 = 1048576

// WebSocketGUID is the RFC 6455 handshake magic string.
const WebSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes (RFC 6455 §5.2).
const (
	WebSocketContinuation byte = 0x0
	WebSocketText         byte = 0x1
	WebSocketBinary       byte = 0x2
	WebSocketClose        byte = 0x8
	WebSocketPing         byte = 0x9
	WebSocketPong         byte = 0xA
)

// WebSocketState mirrors ws's `readyState` numbering.
type WebSocketState int

const (
	WebSocketConnecting WebSocketState = 0
	WebSocketOpen       WebSocketState = 1
	WebSocketClosing    WebSocketState = 2
	WebSocketClosed     WebSocketState = 3
)

// ErrWebSocketClosed is returned once the peer closed the socket (or the
// connection failed). Use CloseCode to tell a normal close from a transport error.
var ErrWebSocketClosed = errors.New("websocket closed")

// ErrWebSocketNotUpgrade reports a request without the required upgrade headers.
var ErrWebSocketNotUpgrade = errors.New("expected websocket upgrade")

// ErrWebSocketProtocol wraps framing violations.
var ErrWebSocketProtocol = errors.New("websocket protocol error")

// errWebSocketTooLarge is internal: the message exceeded maxPayload.
var errWebSocketTooLarge = errors.New("websocket message too large")

// WebSocket is one established RFC 6455 connection. Reads are serialised on one
// goroutine (ReadMessage); writes may come from any goroutine.
type WebSocket struct {
	conn   net.Conn
	reader *bufio.Reader

	// clientSide records whether the local endpoint opened the connection. Client
	// frames are masked, server frames are not.
	clientSide bool

	writeMu sync.Mutex
	readMu  sync.Mutex

	stateMu     sync.Mutex
	state       WebSocketState
	closeSent   bool
	closeCode   int
	closeReason string

	maxPayload int64
	buffered   atomic.Int64

	pongMu      sync.Mutex
	pongHandler func()

	closeFired    atomic.Bool
	closeMu       sync.Mutex
	closeHandlers []func()

	subprotocol string
}

// OnClose registers a callback invoked exactly once when the connection is torn
// down (peer close, local Close, Terminate or a transport failure). The callback
// runs on the goroutine that observed the teardown and must not block.
func (s *WebSocket) OnClose(handler func()) {
	if handler == nil {
		return
	}
	s.closeMu.Lock()
	if s.closeFired.Load() {
		s.closeMu.Unlock()
		handler()
		return
	}
	s.closeHandlers = append(s.closeHandlers, handler)
	s.closeMu.Unlock()
}

// closeFired is an atomic flag rather than sync.Once on purpose: a close handler
// may tear down a peer socket whose handler terminates this one again, and
// sync.Once.Do re-entered from the same goroutine deadlocks. Re-entry is a no-op.
func (s *WebSocket) runCloseHandlers() {
	if !s.closeFired.CompareAndSwap(false, true) {
		return
	}
	s.closeMu.Lock()
	handlers := s.closeHandlers
	s.closeHandlers = nil
	s.closeMu.Unlock()
	for _, handler := range handlers {
		handler()
	}
}

// SetPongHandler installs a callback invoked whenever a pong frame arrives.
func (s *WebSocket) SetPongHandler(handler func()) {
	s.pongMu.Lock()
	s.pongHandler = handler
	s.pongMu.Unlock()
}

// ReadyState mirrors ws's readyState (0 connecting, 1 open, 2 closing, 3 closed).
func (s *WebSocket) ReadyState() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return int(s.state)
}

// IsOpen reports ReadyState() == OPEN.
func (s *WebSocket) IsOpen() bool { return s.ReadyState() == int(WebSocketOpen) }

// Subprotocol returns the negotiated subprotocol ("" when none).
func (s *WebSocket) Subprotocol() string { return s.subprotocol }

// BufferedAmount approximates ws's bufferedAmount: bytes queued for the wire.
func (s *WebSocket) BufferedAmount() int64 { return s.buffered.Load() }

// CloseCode returns the close code/reason observed on the wire (1005 when the
// peer sent an empty close frame, 1006 when the connection died without one).
func (s *WebSocket) CloseCode() (int, string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closeCode, s.closeReason
}

func (s *WebSocket) setState(state WebSocketState) {
	s.stateMu.Lock()
	s.state = state
	s.stateMu.Unlock()
}

// Terminate closes the TCP connection without a close handshake (`ws.terminate`).
func (s *WebSocket) Terminate() {
	s.setState(WebSocketClosed)
	_ = s.conn.Close()
	s.runCloseHandlers()
}

// Close sends a close frame and closes the connection, mirroring
// `ws.close(code, reason)` as observed from the peer (the close frame is on the
// wire before the FIN).
func (s *WebSocket) Close(code int, reason string) {
	s.stateMu.Lock()
	alreadyClosed := s.state == WebSocketClosed
	s.stateMu.Unlock()
	if alreadyClosed {
		return
	}
	s.setState(WebSocketClosing)
	if code == 0 {
		code = 1000
	}
	// A control frame payload is limited to 125 bytes (2 for the code).
	if len(reason) > 123 {
		reason = reason[:123]
	}
	payload := make([]byte, 0, len(reason)+2)
	payload = binary.BigEndian.AppendUint16(payload, uint16(code))
	payload = append(payload, reason...)
	_ = s.writeCloseFrame(payload)
	s.Terminate()
}

// SendText writes one text message.
func (s *WebSocket) SendText(message string) error {
	return s.writeFrame(WebSocketText, []byte(message))
}

// SendBinary writes one binary message.
func (s *WebSocket) SendBinary(message []byte) error {
	return s.writeFrame(WebSocketBinary, message)
}

// SendMessage writes one message with the given opcode.
func (s *WebSocket) SendMessage(message []byte, isBinary bool) error {
	if isBinary {
		return s.SendBinary(message)
	}
	return s.writeFrame(WebSocketText, message)
}

// Ping writes a ping control frame.
func (s *WebSocket) Ping() error {
	return s.writeFrame(WebSocketPing, nil)
}

func (s *WebSocket) writeCloseFrame(payload []byte) error {
	s.stateMu.Lock()
	if s.closeSent {
		s.stateMu.Unlock()
		return nil
	}
	s.closeSent = true
	s.stateMu.Unlock()
	return s.writeFrame(WebSocketClose, payload)
}

// writeFrame serialises one frame. Client frames are masked, server frames are not.
func (s *WebSocket) writeFrame(opcode byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.stateMu.Lock()
	closed := s.state == WebSocketClosed
	s.stateMu.Unlock()
	if closed {
		return ErrWebSocketClosed
	}

	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	length := len(payload)
	maskBit := byte(0)
	if s.clientSide {
		maskBit = 0x80
	}
	switch {
	case length < 126:
		header = append(header, maskBit|byte(length))
	case length <= 65535:
		header = append(header, maskBit|126)
		header = binary.BigEndian.AppendUint16(header, uint16(length))
	default:
		header = append(header, maskBit|127)
		header = binary.BigEndian.AppendUint64(header, uint64(length))
	}

	var mask [4]byte
	body := payload
	if s.clientSide {
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		header = append(header, mask[:]...)
		body = make([]byte, len(payload))
		for index := range payload {
			body[index] = payload[index] ^ mask[index%4]
		}
	}

	s.buffered.Add(int64(len(header) + len(body)))
	defer s.buffered.Add(-int64(len(header) + len(body)))

	if _, err := s.conn.Write(header); err != nil {
		s.setState(WebSocketClosed)
		return err
	}
	if len(body) > 0 {
		if _, err := s.conn.Write(body); err != nil {
			s.setState(WebSocketClosed)
			return err
		}
	}
	return nil
}

// readFrame reads one frame and validates the RFC constraints for this endpoint.
func (s *WebSocket) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(s.reader, header); err != nil {
		return false, 0, nil, err
	}
	fin = header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return false, 0, nil, fmt.Errorf("%w: reserved bits set", ErrWebSocketProtocol)
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(s.reader, extended); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(s.reader, extended); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended))
		if length < 0 {
			return false, 0, nil, fmt.Errorf("%w: invalid length", ErrWebSocketProtocol)
		}
	}
	if opcode >= WebSocketClose {
		if !fin || length > 125 {
			return false, 0, nil, fmt.Errorf("%w: invalid control frame", ErrWebSocketProtocol)
		}
	}
	if length > s.maxPayload {
		s.Close(1009, "Message Too Big")
		return false, 0, nil, errWebSocketTooLarge
	}
	if s.clientSide {
		if masked {
			return false, 0, nil, fmt.Errorf("%w: server frames must not be masked", ErrWebSocketProtocol)
		}
	} else if !masked {
		return false, 0, nil, fmt.Errorf("%w: client frames must be masked", ErrWebSocketProtocol)
	}

	mask := make([]byte, 4)
	if masked {
		if _, err := io.ReadFull(s.reader, mask); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(s.reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	return fin, opcode, payload, nil
}

// ReadMessage returns the next text/binary message, answering control frames on
// the way and reassembling fragmented messages. It is safe to call from exactly
// one goroutine at a time.
func (s *WebSocket) ReadMessage() (payload []byte, isBinary bool, err error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	var (
		message       []byte
		messageSet    bool
		messageBinary bool
	)
	for {
		fin, opcode, frame, readErr := s.readFrame()
		if readErr != nil {
			if errors.Is(readErr, errWebSocketTooLarge) {
				// readFrame already sent the 1009 close frame.
				s.Terminate()
				return nil, false, ErrWebSocketClosed
			}
			// A protocol violation drops the connection without a close handshake
			// (measured `ws` behaviour) and so does a transport error.
			s.Terminate()
			return nil, false, ErrWebSocketClosed
		}
		switch opcode {
		case WebSocketPing:
			if writeErr := s.writeFrame(WebSocketPong, frame); writeErr != nil {
				s.Terminate()
				return nil, false, ErrWebSocketClosed
			}
			continue
		case WebSocketPong:
			s.pongMu.Lock()
			handler := s.pongHandler
			s.pongMu.Unlock()
			if handler != nil {
				handler()
			}
			continue
		case WebSocketClose:
			s.handlePeerClose(frame)
			return nil, false, ErrWebSocketClosed
		case WebSocketBinary, WebSocketText:
			if messageSet {
				s.Terminate()
				return nil, false, ErrWebSocketClosed
			}
			if fin {
				return frame, opcode == WebSocketBinary, nil
			}
			message = append(message, frame...)
			messageSet = true
			messageBinary = opcode == WebSocketBinary
		case WebSocketContinuation:
			if !messageSet {
				s.Terminate()
				return nil, false, ErrWebSocketClosed
			}
			message = append(message, frame...)
			if int64(len(message)) > s.maxPayload {
				s.Close(1009, "Message Too Big")
				return nil, false, ErrWebSocketClosed
			}
			if fin {
				return message, messageBinary, nil
			}
		default:
			s.Terminate()
			return nil, false, ErrWebSocketClosed
		}
	}
}

// handlePeerClose records the peer's close code/reason, echoes the close frame
// when it was not sent yet, and tears the connection down.
func (s *WebSocket) handlePeerClose(payload []byte) {
	code := 1005
	reason := ""
	if len(payload) >= 2 {
		code = int(binary.BigEndian.Uint16(payload))
		reason = string(payload[2:])
	}
	s.stateMu.Lock()
	s.closeCode = code
	s.closeReason = reason
	s.state = WebSocketClosing
	s.stateMu.Unlock()

	// Echo the peer's close frame before tearing the connection down. The state
	// must still be CLOSING here, otherwise writeFrame refuses to write.
	if len(payload) > 125 {
		payload = payload[:125]
	}
	_ = s.writeCloseFrame(payload)
	s.Terminate()
}

// ==================== opening handshake: server ====================

// WebSocketUpgradeOptions configures the server-side handshake.
type WebSocketUpgradeOptions struct {
	// MaxPayload defaults to DefaultWebSocketMaxPayload.
	MaxPayload int64
	// SelectSubprotocol overrides subprotocol negotiation. When nil, the first
	// protocol offered by the client is echoed, which is `ws`'s default behaviour
	// (and what @fastify/websocket relies on for the SPICE console).
	SelectSubprotocol func(offered []string) string
}

// IsWebSocketUpgrade mirrors the check @fastify/websocket applies: an
// `Upgrade: websocket` request with `Connection` containing the upgrade token.
func IsWebSocketUpgrade(r *http.Request) bool {
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

// WSOfferedSubprotocols returns the protocols the client asked for, in order.
func WSOfferedSubprotocols(r *http.Request) []string {
	raw := r.Header.Get("Sec-WebSocket-Protocol")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	offered := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			offered = append(offered, trimmed)
		}
	}
	return offered
}

// UpgradeWebSocket performs the RFC 6455 opening handshake on the hijacked
// connection. The caller owns authentication and origin checks; they must run
// BEFORE this function is called so an unauthorised caller never receives 101.
func UpgradeWebSocket(w http.ResponseWriter, r *http.Request, options *WebSocketUpgradeOptions) (*WebSocket, error) {
	opts := WebSocketUpgradeOptions{}
	if options != nil {
		opts = *options
	}
	if opts.MaxPayload <= 0 {
		opts.MaxPayload = DefaultWebSocketMaxPayload
	}

	if !IsWebSocketUpgrade(r) {
		return nil, ErrWebSocketNotUpgrade
	}
	if version := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); version != "13" {
		return nil, fmt.Errorf("unsupported websocket version %q", version)
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return nil, errors.New("invalid Sec-WebSocket-Key")
	}

	offered := WSOfferedSubprotocols(r)
	selected := ""
	if opts.SelectSubprotocol != nil {
		selected = opts.SelectSubprotocol(offered)
	} else if len(offered) > 0 {
		selected = offered[0]
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijacking")
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}

	acceptHash := sha1.Sum([]byte(key + WebSocketGUID))
	accept := base64.StdEncoding.EncodeToString(acceptHash[:])
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n"
	if selected != "" {
		response += "Sec-WebSocket-Protocol: " + selected + "\r\n"
	}
	response += "\r\n"
	if _, err := buffered.WriteString(response); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &WebSocket{
		conn:        conn,
		reader:      buffered.Reader,
		state:       WebSocketOpen,
		maxPayload:  opts.MaxPayload,
		subprotocol: selected,
	}, nil
}

// ==================== opening handshake: client ====================

// WebSocketDialOptions configures the client-side handshake.
type WebSocketDialOptions struct {
	// TLSConfig is required for wss:// URLs (mTLS certificate + InsecureSkipVerify
	// mirroring the Node client).
	TLSConfig *tls.Config
	// Dial overrides the TCP dial. The tunnel branch passes a duplex stream to the
	// node agent here.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// HandshakeTimeout defaults to 15s (the Node ws handshakeTimeout).
	HandshakeTimeout time.Duration
	// MaxPayload defaults to DefaultWebSocketMaxPayload.
	MaxPayload int64
	// Subprotocols are offered to the server; the first one the server selects is
	// recorded on the socket.
	Subprotocols []string
	// Header carries extra handshake headers.
	Header http.Header
}

// DialWebSocket opens an RFC 6455 client connection.
func DialWebSocket(ctx context.Context, rawURL string, options WebSocketDialOptions) (*WebSocket, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported websocket scheme %q", parsed.Scheme)
	}

	handshakeTimeout := options.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 15 * time.Second
	}
	maxPayload := options.MaxPayload
	if maxPayload <= 0 {
		maxPayload = DefaultWebSocketMaxPayload
	}

	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(parsed.Hostname(), port)

	dial := options.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	cleanup := func() { _ = conn.Close() }

	useTLS := parsed.Scheme == "wss"
	if useTLS {
		config := options.TLSConfig
		if config == nil {
			cleanup()
			return nil, errors.New("wss requires a TLS config")
		}
		clone := config.Clone()
		if clone.ServerName == "" && net.ParseIP(parsed.Hostname()) == nil {
			clone.ServerName = parsed.Hostname()
		}
		tlsConn := tls.Client(conn, clone)
		handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
		err = tlsConn.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			cleanup()
			return nil, err
		}
		conn = tlsConn
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		cleanup()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	var request strings.Builder
	request.WriteString("GET " + parsed.RequestURI() + " HTTP/1.1\r\n")
	request.WriteString("Host: " + parsed.Host + "\r\n")
	request.WriteString("Upgrade: websocket\r\n")
	request.WriteString("Connection: Upgrade\r\n")
	request.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	request.WriteString("Sec-WebSocket-Version: 13\r\n")
	if len(options.Subprotocols) > 0 {
		request.WriteString("Sec-WebSocket-Protocol: " + strings.Join(options.Subprotocols, ", ") + "\r\n")
	}
	for name, values := range options.Header {
		if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Upgrade") ||
			strings.EqualFold(name, "Connection") || strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
			continue
		}
		for _, value := range values {
			request.WriteString(name + ": " + value + "\r\n")
		}
	}
	request.WriteString("\r\n")

	deadline := time.Now().Add(handshakeTimeout)
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write([]byte(request.String())); err != nil {
		cleanup()
		return nil, err
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = response.Body.Close()
		cleanup()
		return nil, fmt.Errorf("websocket handshake rejected with status %d", response.StatusCode)
	}
	if !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
		_ = response.Body.Close()
		cleanup()
		return nil, errors.New("websocket handshake missing Upgrade header")
	}
	expectedAccept := sha1.Sum([]byte(key + WebSocketGUID))
	if response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(expectedAccept[:]) {
		_ = response.Body.Close()
		cleanup()
		return nil, errors.New("websocket handshake accept mismatch")
	}
	// Clear the handshake deadline: an established ws connection has no read
	// deadline (liveness is the caller's heartbeat).
	_ = conn.SetDeadline(time.Time{})

	return &WebSocket{
		conn:        conn,
		reader:      reader,
		clientSide:  true,
		state:       WebSocketOpen,
		maxPayload:  maxPayload,
		subprotocol: strings.TrimSpace(response.Header.Get("Sec-WebSocket-Protocol")),
	}, nil
}

// webSocketAccept computes the Sec-WebSocket-Accept value (exposed for tests).
func webSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + WebSocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}
