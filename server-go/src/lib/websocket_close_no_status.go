// `ws.close()` without a status code, split into its own file so the RFC 6455
// engine (websocket.go) stays untouched by the routes that need it.
//
// The TypeScript Incus UI bridge forwards the upstream close code through
// `toSendableCloseCode` and, for every code that must not appear on the wire
// (1005 "no status received", 1006 "abnormal closure", 1015, < 1000, > 4999),
// falls back to `socket.close()` — a close frame with an EMPTY payload. A browser
// reports that frame as 1005, which is what the Node backend produces.
//
// lib.WebSocket.Close always writes the two-byte code and turns 0 into 1000, so it
// cannot express "empty close frame". This method is the missing primitive; it is
// deliberately the only addition needed by the Incus UI route.
package lib

// CloseNoStatus mirrors `ws.close()` with no arguments: send a close frame with an
// empty payload and tear the connection down. The peer observes 1005.
func (s *WebSocket) CloseNoStatus() {
	s.stateMu.Lock()
	alreadyClosed := s.state == WebSocketClosed
	s.stateMu.Unlock()
	if alreadyClosed {
		return
	}
	s.setState(WebSocketClosing)
	// An empty payload is a valid close frame: RFC 6455 §5.5.1 makes the status
	// code optional, and the subset in websocket.go decodes a missing code as 1005.
	_ = s.writeCloseFrame(nil)
	s.Terminate()
}
