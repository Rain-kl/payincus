package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"incudal-agent/internal/protocol"
)

type WorkerConfig struct {
	PanelURL    string
	AgentID     string
	AgentSecret string
	TargetHost  string
	TargetPort  int
	InsecureTLS bool
}

type Worker struct {
	mu         sync.Mutex
	cfg        WorkerConfig
	enabled    bool
	targetHost string
	targetPort int
	cancel     context.CancelFunc
	running    bool
}

func NewWorker(cfg WorkerConfig) *Worker {
	th := cfg.TargetHost
	if th == "" {
		th = "127.0.0.1"
	}
	tp := cfg.TargetPort
	if tp == 0 {
		tp = 8443
	}
	return &Worker{
		cfg:        cfg,
		targetHost: th,
		targetPort: tp,
	}
}

// SyncConfig 由心跳模块调用，根据服务端指令动态启停反向隧道并更新目标
func (w *Worker) SyncConfig(enabled bool, targetHost string, targetPort int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if targetHost != "" {
		w.targetHost = targetHost
	}
	if targetPort > 0 {
		w.targetPort = targetPort
	}

	if enabled == w.enabled {
		return
	}

	w.enabled = enabled
	if enabled {
		if !w.running {
			ctx, cancel := context.WithCancel(context.Background())
			w.cancel = cancel
			w.running = true
			log.Printf("[tunnel] tunnel enabled by server, starting reverse websocket worker (target: %s:%d)", w.targetHost, w.targetPort)
			go w.runLoop(ctx)
		}
	} else {
		if w.running && w.cancel != nil {
			log.Printf("[tunnel] tunnel disabled by server, stopping reverse websocket worker")
			w.cancel()
			w.cancel = nil
			w.running = false
		}
	}
}

func (w *Worker) getTarget() (string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.targetHost, w.targetPort
}

func (w *Worker) setTarget(host string, port int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if host != "" {
		w.targetHost = host
	}
	if port > 0 {
		w.targetPort = port
	}
}

func (w *Worker) runLoop(ctx context.Context) {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := w.connectAndServe(ctx)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			log.Printf("[tunnel] tunnel disconnected: %v, reconnecting in %v", err, backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

type streamEntry struct {
	writeChan chan []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func newStreamEntry() *streamEntry {
	return &streamEntry{
		writeChan: make(chan []byte, 128),
		closed:    make(chan struct{}),
	}
}

func (s *streamEntry) close() {
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func (w *Worker) connectAndServe(ctx context.Context) error {
	panelURL := strings.TrimRight(w.cfg.PanelURL, "/")
	var wsURL string
	if strings.HasPrefix(panelURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(panelURL, "https://") + "/api/agent/tunnel"
	} else {
		wsURL = "ws://" + strings.TrimPrefix(panelURL, "http://") + "/api/agent/tunnel"
	}

	timestamp := protocol.NewTimestamp()
	nonce, err := protocol.NewNonce()
	if err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	bodyHash := protocol.BodySHA256(nil)
	signingPayload := protocol.SigningPayload(http.MethodGet, "/api/agent/tunnel", timestamp, nonce, bodyHash)
	signature := protocol.Signature(w.cfg.AgentSecret, signingPayload)

	header := http.Header{}
	header.Set("x-incudal-agent-id", w.cfg.AgentID)
	header.Set("x-incudal-timestamp", timestamp)
	header.Set("x-incudal-nonce", nonce)
	header.Set("x-incudal-body-sha256", bodyHash)
	header.Set("x-incudal-signature", signature)

	dialer := *websocket.DefaultDialer
	if w.cfg.InsecureTLS {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // nolint:gosec
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial websocket (status %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()

	log.Printf("[tunnel] reverse tunnel established to %s", wsURL)

	writeChan := make(chan []byte, 128)
	writeErrChan := make(chan error, 1)

	var streamsMu sync.Mutex
	streams := make(map[uint32]*streamEntry)
	defer func() {
		streamsMu.Lock()
		for _, s := range streams {
			s.close()
		}
		streams = make(map[uint32]*streamEntry)
		streamsMu.Unlock()
	}()

	// Write pump
	go func() {
		pingTicker := time.NewTicker(25 * time.Second)
		defer pingTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					writeErrChan <- err
					return
				}
			case frame, ok := <-writeChan:
				if !ok {
					return
				}
				conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
					writeErrChan <- err
					return
				}
			}
		}
	}()

	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		return nil
	})

	// Read pump
	for {
		select {
		case err := <-writeErrChan:
			return err
		case <-ctx.Done():
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read websocket: %w", err)
		}

		if msgType != websocket.BinaryMessage {
			continue
		}

		frameType, streamID, payload, err := DecodeFrame(msg)
		if err != nil {
			log.Printf("[tunnel] decode frame error: %v", err)
			continue
		}

		switch frameType {
		case FrameTypeOpen:
			var openCfg TargetConfig
			if len(payload) > 0 {
				_ = json.Unmarshal(payload, &openCfg)
			}
			tHost, tPort := w.getTarget()
			if openCfg.TargetHost != "" {
				tHost = openCfg.TargetHost
			}
			if openCfg.TargetPort > 0 {
				tPort = openCfg.TargetPort
			}

			s := newStreamEntry()
			streamsMu.Lock()
			if old, exists := streams[streamID]; exists {
				old.close()
			}
			streams[streamID] = s
			streamsMu.Unlock()

			go func(sID uint32, host string, port int, entry *streamEntry) {
				defer entry.close()

				addr := net.JoinHostPort(host, fmt.Sprint(port))
				targetConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					log.Printf("[tunnel] stream %d: dial local target %s failed: %v", sID, addr, err)
					streamsMu.Lock()
					if streams[sID] == entry {
						delete(streams, sID)
					}
					streamsMu.Unlock()
					select {
					case writeChan <- EncodeFrame(FrameTypeReset, sID, nil):
					case <-ctx.Done():
					}
					return
				}
				defer targetConn.Close()

				// Goroutine to drain incoming data from server and write to targetConn
				go func() {
					for {
						select {
						case data, ok := <-entry.writeChan:
							if !ok {
								if tcpConn, isTCP := targetConn.(*net.TCPConn); isTCP {
									_ = tcpConn.CloseWrite()
								}
								return
							}
							if _, wErr := targetConn.Write(data); wErr != nil {
								entry.close()
								return
							}
						case <-entry.closed:
							return
						case <-ctx.Done():
							return
						}
					}
				}()

				// Main stream goroutine reads from targetConn and sends data back to server
				buf := make([]byte, 32*1024)
				for {
					n, readErr := targetConn.Read(buf)
					if n > 0 {
						dataFrame := EncodeFrame(FrameTypeData, sID, buf[:n])
						select {
						case writeChan <- dataFrame:
						case <-entry.closed:
							return
						case <-ctx.Done():
							return
						}
					}
					if readErr != nil {
						streamsMu.Lock()
						if streams[sID] == entry {
							delete(streams, sID)
						}
						streamsMu.Unlock()

						if readErr == io.EOF {
							select {
							case writeChan <- EncodeFrame(FrameTypeClose, sID, nil):
							case <-ctx.Done():
							}
						} else {
							select {
							case writeChan <- EncodeFrame(FrameTypeReset, sID, nil):
							case <-ctx.Done():
							}
						}
						return
					}
				}
			}(streamID, tHost, tPort, s)

		case FrameTypeData:
			streamsMu.Lock()
			entry, ok := streams[streamID]
			streamsMu.Unlock()
			if ok && entry != nil {
				select {
				case entry.writeChan <- payload:
				case <-entry.closed:
				case <-ctx.Done():
				}
			}

		case FrameTypeClose:
			streamsMu.Lock()
			entry, ok := streams[streamID]
			if streams[streamID] == entry {
				delete(streams, streamID)
			}
			streamsMu.Unlock()
			if ok && entry != nil {
				close(entry.writeChan)
			}

		case FrameTypeReset:
			streamsMu.Lock()
			entry, ok := streams[streamID]
			if streams[streamID] == entry {
				delete(streams, streamID)
			}
			streamsMu.Unlock()
			if ok && entry != nil {
				entry.close()
			}

		case FrameTypeConfig:
			var cfg TargetConfig
			if err := json.Unmarshal(payload, &cfg); err == nil {
				log.Printf("[tunnel] dynamic config update received: target=%s:%d", cfg.TargetHost, cfg.TargetPort)
				w.setTarget(cfg.TargetHost, cfg.TargetPort)
			}
		}
	}
}
