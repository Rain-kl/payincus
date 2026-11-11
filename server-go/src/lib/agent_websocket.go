// Host-agent tunnel socket: a thin adapter over lib/websocket.go.
//
// The RFC 6455 engine (handshake, masking, fragmentation, ping/pong, close codes,
// maxPayload) lives in websocket.go so the terminal and SPICE consoles share one
// implementation. This file keeps the AgentWebSocket API the tunnel manager and
// routes/agent.go were written against, so Phase-5/6 call sites do not change:
//
//	TunnelReadyState / TunnelSend / TunnelClose  — tunnel_manager.TunnelSocket
//	ReadBinary                                    — the agent's request loop
//
// The tunnel is binary-only: the Node agent sends Buffers, and `ws.send(Buffer)`
// is a binary frame. Text frames are accepted and delivered as bytes, exactly like
// the previous implementation.
package lib

import (
	"net/http"
)

// AgentWebSocketMaxPayload mirrors @fastify/websocket's maxPayload (1 MiB). It is
// left untyped so existing uint64/byte arithmetic in callers keeps compiling.
const AgentWebSocketMaxPayload = 1048576

// ErrAgentWebSocketClosed is returned by ReadBinary once the peer closed the
// socket (or the connection failed).
var ErrAgentWebSocketClosed = ErrWebSocketClosed

// ErrAgentWebSocketNotUpgrade reports a request without the required upgrade
// headers.
var ErrAgentWebSocketNotUpgrade = ErrWebSocketNotUpgrade

// AgentWebSocket is a server-side WebSocket that satisfies TunnelSocket.
type AgentWebSocket struct {
	*WebSocket
}

var _ TunnelSocket = (*AgentWebSocket)(nil)

// IsAgentWebSocketUpgrade mirrors the check @fastify/websocket applies: an
// `Upgrade: websocket` request with `Connection` containing the upgrade token.
func IsAgentWebSocketUpgrade(r *http.Request) bool {
	return IsWebSocketUpgrade(r)
}

// UpgradeAgentWebSocket performs the RFC 6455 opening handshake on the hijacked
// connection. The caller owns authentication: it happens AFTER the upgrade, so an
// unauthenticated agent gets a close code (4001) exactly like the Node route.
func UpgradeAgentWebSocket(w http.ResponseWriter, r *http.Request) (*AgentWebSocket, error) {
	socket, err := UpgradeWebSocket(w, r, nil)
	if err != nil {
		return nil, err
	}
	return &AgentWebSocket{WebSocket: socket}, nil
}

// TunnelReadyState mirrors WebSocket#readyState: 1 = OPEN, 3 = CLOSED.
func (s *AgentWebSocket) TunnelReadyState() int {
	return s.ReadyState()
}

// TunnelSend writes one binary frame (Node's `ws.send(Buffer)` uses binary).
func (s *AgentWebSocket) TunnelSend(data []byte) error {
	return s.SendBinary(data)
}

// TunnelClose mirrors `ws.close(code, reason)`.
func (s *AgentWebSocket) TunnelClose(code int, reason string) {
	s.Close(code, reason)
}

// ReadBinary returns the payload of the next binary/text message, answering
// control frames on the way. Fragmented data frames are reassembled.
func (s *AgentWebSocket) ReadBinary() ([]byte, error) {
	payload, _, err := s.ReadMessage()
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// AgentServeTunnel runs the authenticated tunnel loop: feed every message to the
// manager and report the close exactly once. This is the Go equivalent of the
// TypeScript route's `socket.on('message')` / `socket.on('close')` wiring.
func AgentServeTunnel(hostID int32, manager *TunnelManager, socket *AgentWebSocket) {
	defer func() {
		socket.Terminate()
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
