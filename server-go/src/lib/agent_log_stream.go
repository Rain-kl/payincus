// Port of server/src/lib/agent-log-stream.ts.
//
// Manages in-process SSE subscriptions for live host agent journalctl logs,
// multiplexing LOG_DATA / LOG_CTL frames over the process-local TunnelManager.
package lib

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

const (
	agentLogSSEKeepaliveInterval = 15 * time.Second
	agentLogHeartbeatStaleAge    = 3 * time.Minute
	agentLogLineMaxRunes         = 8000
)

type agentLogClient struct {
	id    uint64
	write func(string) bool
	mu    sync.Mutex
}

func (c *agentLogClient) send(chunk string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.write(chunk)
}

type agentLogSession struct {
	clients map[uint64]*agentLogClient
	stopCh  chan struct{}
}

// AgentLogStreamManager mirrors AgentLogStreamManager in agent-log-stream.ts.
type AgentLogStreamManager struct {
	tunnel     *TunnelManager
	keepalive  time.Duration
	probeAgent func(ctx context.Context, hostID int32) bool
	attachOnce sync.Once
	mu         sync.Mutex
	seq        uint64
	sessions   map[int32]*agentLogSession
}

var (
	agentLogManagerOnce     sync.Once
	agentLogManagerInstance *AgentLogStreamManager
)

// DefaultAgentLogStreamManager returns the process-wide AgentLogStreamManager singleton.
func DefaultAgentLogStreamManager() *AgentLogStreamManager {
	agentLogManagerOnce.Do(func() {
		agentLogManagerInstance = NewAgentLogStreamManager(TunnelManagerInstance())
	})
	return agentLogManagerInstance
}

// NewAgentLogStreamManager builds a manager bound to tunnel (used by tests and singleton).
func NewAgentLogStreamManager(tunnel *TunnelManager) *AgentLogStreamManager {
	if tunnel == nil {
		tunnel = TunnelManagerInstance()
	}
	return &AgentLogStreamManager{
		tunnel:     tunnel,
		keepalive:  agentLogSSEKeepaliveInterval,
		probeAgent: defaultAgentOnlineProbe,
		sessions:   map[int32]*agentLogSession{},
	}
}

func defaultAgentOnlineProbe(ctx context.Context, hostID int32) bool {
	if db.DB == nil {
		return false
	}
	var row struct {
		Status     string     `gorm:"column:status"`
		LastSeenAt *time.Time `gorm:"column:last_seen_at"`
	}
	err := db.DB.WithContext(ctx).Raw(
		`SELECT status::text AS status, last_seen_at
		 FROM host_agents
		 WHERE host_id = ? AND enabled = true
		 ORDER BY id ASC LIMIT 1`,
		hostID,
	).Scan(&row).Error
	if err != nil || row.Status != "online" || row.LastSeenAt == nil {
		return false
	}
	return time.Now().UTC().Sub(row.LastSeenAt.UTC()) < agentLogHeartbeatStaleAge
}

func (m *AgentLogStreamManager) ensureAttached() {
	m.attachOnce.Do(func() {
		m.tunnel.OnTunnelConnected(func(hostID int32) {
			clients := m.sessionClients(hostID)
			if len(clients) == 0 {
				return
			}
			lines := int32(100)
			m.tunnel.SendLogControl(hostID, TunnelLogActionStart, &lines)
			m.broadcast(hostID, clients, formatAgentLogSSE("status", map[string]bool{"tunnelOnline": true}))
		})
		m.tunnel.OnTunnelDisconnected(func(hostID int32) {
			clients := m.sessionClients(hostID)
			if len(clients) == 0 {
				return
			}
			m.broadcast(hostID, clients, formatAgentLogSSE("status", map[string]bool{"tunnelOnline": false}))
		})
		m.tunnel.OnLogData(func(hostID int32, line *string) {
			clients := m.sessionClients(hostID)
			if len(clients) == 0 {
				return
			}
			if line == nil {
				m.broadcast(hostID, clients, "event: end\ndata: {}\n\n")
				return
			}
			payload := truncateAgentLogLine(*line, agentLogLineMaxRunes)
			encoded, err := json.Marshal(payload)
			if err != nil {
				return
			}
			m.broadcast(hostID, clients, "event: log\ndata: "+string(encoded)+"\n\n")
		})
	})
}

func truncateAgentLogLine(line string, maxRunes int) string {
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line
	}
	return string(runes[:maxRunes]) + "…"
}

func formatAgentLogSSE(event string, payload any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "event: " + event + "\ndata: " + string(encoded) + "\n\n"
}

func (m *AgentLogStreamManager) sessionClients(hostID int32) []*agentLogClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[hostID]
	if session == nil || len(session.clients) == 0 {
		return nil
	}
	out := make([]*agentLogClient, 0, len(session.clients))
	for _, c := range session.clients {
		out = append(out, c)
	}
	return out
}

func (m *AgentLogStreamManager) broadcast(hostID int32, clients []*agentLogClient, chunk string) {
	if chunk == "" {
		return
	}
	for _, client := range clients {
		if !client.send(chunk) {
			m.unsubscribe(hostID, client.id)
		}
	}
}

// Subscribe registers an SSE subscriber for hostID until ctx is canceled.
func (m *AgentLogStreamManager) Subscribe(ctx context.Context, hostID int32, write func(string) bool) error {
	if write == nil {
		return nil
	}
	m.ensureAttached()

	m.mu.Lock()
	m.seq++
	clientID := m.seq
	client := &agentLogClient{id: clientID, write: write}
	session := m.sessions[hostID]
	if session == nil {
		session = &agentLogSession{
			clients: map[uint64]*agentLogClient{},
			stopCh:  make(chan struct{}),
		}
		m.sessions[hostID] = session
		go m.runKeepalive(hostID, session.stopCh)
	}
	session.clients[clientID] = client
	m.mu.Unlock()

	go func() {
		<-ctx.Done()
		m.unsubscribe(hostID, clientID)
	}()

	tunnelOnline := m.tunnel.IsTunnelOnline(hostID)
	agentOnline := m.probeAgent(ctx, hostID)
	if !client.send(formatAgentLogSSE("status", map[string]bool{
		"agentOnline":  agentOnline,
		"tunnelOnline": tunnelOnline,
	})) {
		m.unsubscribe(hostID, clientID)
		return nil
	}
	if tunnelOnline {
		lines := int32(100)
		m.tunnel.SendLogControl(hostID, TunnelLogActionStart, &lines)
	}
	return nil
}

func (m *AgentLogStreamManager) runKeepalive(hostID int32, stopCh <-chan struct{}) {
	ticker := time.NewTicker(m.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			clients := m.sessionClients(hostID)
			if len(clients) > 0 {
				m.broadcast(hostID, clients, ": ping\n\n")
			}
		}
	}
}

func (m *AgentLogStreamManager) unsubscribe(hostID int32, clientID uint64) {
	var shouldStopControl bool

	m.mu.Lock()
	session := m.sessions[hostID]
	if session == nil {
		m.mu.Unlock()
		return
	}
	delete(session.clients, clientID)
	if len(session.clients) == 0 {
		close(session.stopCh)
		delete(m.sessions, hostID)
		shouldStopControl = true
	}
	m.mu.Unlock()

	if shouldStopControl && m.tunnel.IsTunnelOnline(hostID) {
		m.tunnel.SendLogControl(hostID, TunnelLogActionStop, nil)
	}
}
