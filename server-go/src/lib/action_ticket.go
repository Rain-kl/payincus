// Port of server/src/lib/action-ticket.ts.
//
// Short-lived, process-local bearer tickets that let a browser start a privileged
// action without putting the login JWT in a URL:
//
//	oauth-bind       — binds the current session while completing an OAuth bind flow
//	terminal         — opens a terminal WebSocket (Wave E caller)
//	agent-log-stream — opens the host agent SSE log stream (routes/hosts.go has its
//	                   own equivalent store today; the seam is documented there)
//	incus-ui         — opens the Incus UI proxy (1 ticket reused up to 256 requests)
//
// Security invariants, all identical to the TypeScript:
//   - a ticket is single use (maxUsage 1) except incus-ui (256);
//   - consumption is atomic under one mutex, so two concurrent requests can never
//     both consume the same single-use ticket;
//   - a ticket is bound to the user id, the token `iat` it was issued for and the
//     refresh-session id, so it dies with that session
//     (RevokeActionTicketsForSession);
//   - an expired ticket is deleted on first touch;
//   - a type mismatch is a plain "not found" (never leaks that the token exists);
//   - a host/instance mismatch on the bound ticket types burns the ticket.
//
// Deployment invariant (AGENTS.md / WAVE_D_SPEC §0.1): the store is in-process.
// A second backend replica would accept a ticket it never issued. Do not move this
// to a shared store without an owner-approved architecture change.
package lib

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"sync"
	"time"
)

// ActionTicketType mirrors the ActionTicketType union.
type ActionTicketType string

const (
	ActionTicketOAuthBind      ActionTicketType = "oauth-bind"
	ActionTicketTerminal       ActionTicketType = "terminal"
	ActionTicketAgentLogStream ActionTicketType = "agent-log-stream"
	ActionTicketIncusUI        ActionTicketType = "incus-ui"
)

// actionTicketTTLMs mirrors ACTION_TICKET_TTL_MS.
var actionTicketTTLMs = map[ActionTicketType]int64{
	ActionTicketOAuthBind:      60 * 1000,
	ActionTicketTerminal:       60 * 1000,
	ActionTicketAgentLogStream: 60 * 1000,
	ActionTicketIncusUI:        5 * 60 * 1000,
}

// actionTicketIncusUIMaxUsage mirrors the incus-ui `maxUsage: 256`.
const actionTicketIncusUIMaxUsage = 256

// actionTicketTokenBytes: 24 random bytes encode to exactly 32 URL-safe characters,
// the same shape (and alphabet) as nanoid(32) in the TypeScript.
const actionTicketTokenBytes = 24

// actionTicketNow is a test seam only; production code always reads the clock.
var actionTicketNow = func() int64 { return time.Now().UnixMilli() }

// actionTicket is the internal record (never serialized).
type actionTicket struct {
	Type       ActionTicketType
	UserID     int32
	IssuedAt   int64
	SessionID  *string
	ExpiresAt  int64
	CreatedAt  int64
	UsageCount int
	MaxUsage   int
	InstanceID int32
	HostID     int32
}

var actionTicketStore = struct {
	sync.Mutex
	tickets map[string]actionTicket
}{tickets: make(map[string]actionTicket)}

// actionTicketRandomToken returns 32 URL-safe characters.
func actionTicketRandomToken() string {
	buf := make([]byte, actionTicketTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand never fails in practice. If it does, still return 32 URL-safe
		// characters (a shorter token would weaken the ticket).
		seed := time.Now().UTC().Format(time.RFC3339Nano) + strconv.FormatInt(time.Now().UnixNano(), 36)
		for len(seed) < actionTicketTokenBytes {
			seed += seed
		}
		buf = []byte(seed[:actionTicketTokenBytes])
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// createActionTicket inserts a ticket and performs the TypeScript module's
// interval sweep lazily: every insert drops already-expired tickets, which keeps the
// map bounded exactly like the unref'd `setInterval(..., 60s)` cleanup. The sweep is
// observationally invisible (expired tickets are invalid either way).
func createActionTicket(ticket actionTicket) string {
	token := actionTicketRandomToken()
	now := actionTicketNow()

	actionTicketStore.Lock()
	defer actionTicketStore.Unlock()
	for key, entry := range actionTicketStore.tickets {
		if now > entry.ExpiresAt || entry.UsageCount >= entry.MaxUsage {
			delete(actionTicketStore.tickets, key)
		}
	}
	actionTicketStore.tickets[token] = ticket
	return token
}

// GenerateOAuthBindTicket mirrors generateOAuthBindTicket(userId, issuedAt, sessionId).
func GenerateOAuthBindTicket(userID int32, issuedAt int64, sessionID *string) string {
	now := actionTicketNow()
	return createActionTicket(actionTicket{
		Type:      ActionTicketOAuthBind,
		UserID:    userID,
		IssuedAt:  issuedAt,
		SessionID: sessionID,
		ExpiresAt: now + actionTicketTTLMs[ActionTicketOAuthBind],
		CreatedAt: now,
		MaxUsage:  1,
	})
}

// GenerateTerminalAccessTicket mirrors generateTerminalAccessTicket(...).
func GenerateTerminalAccessTicket(userID, instanceID int32, issuedAt int64, sessionID *string) string {
	now := actionTicketNow()
	return createActionTicket(actionTicket{
		Type:       ActionTicketTerminal,
		UserID:     userID,
		InstanceID: instanceID,
		IssuedAt:   issuedAt,
		SessionID:  sessionID,
		ExpiresAt:  now + actionTicketTTLMs[ActionTicketTerminal],
		CreatedAt:  now,
		MaxUsage:   1,
	})
}

// GenerateAgentLogStreamTicket mirrors generateAgentLogStreamTicket(...).
func GenerateAgentLogStreamTicket(userID, hostID int32, issuedAt int64, sessionID *string) string {
	now := actionTicketNow()
	return createActionTicket(actionTicket{
		Type:      ActionTicketAgentLogStream,
		UserID:    userID,
		HostID:    hostID,
		IssuedAt:  issuedAt,
		SessionID: sessionID,
		ExpiresAt: now + actionTicketTTLMs[ActionTicketAgentLogStream],
		CreatedAt: now,
		MaxUsage:  1,
	})
}

// GenerateIncusUIAccessTicket mirrors generateIncusUiAccessTicket(...).
func GenerateIncusUIAccessTicket(userID, hostID int32, issuedAt int64, sessionID *string) string {
	now := actionTicketNow()
	return createActionTicket(actionTicket{
		Type:      ActionTicketIncusUI,
		UserID:    userID,
		HostID:    hostID,
		IssuedAt:  issuedAt,
		SessionID: sessionID,
		ExpiresAt: now + actionTicketTTLMs[ActionTicketIncusUI],
		CreatedAt: now,
		MaxUsage:  actionTicketIncusUIMaxUsage,
	})
}

// OAuthBindTicketConsumeResult mirrors OAuthBindTicketConsumeResult.
type OAuthBindTicketConsumeResult struct {
	Valid     bool    `json:"valid"`
	UserID    int32   `json:"userId,omitempty"`
	IssuedAt  int64   `json:"issuedAt,omitempty"`
	SessionID *string `json:"sessionId,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// TerminalTicketConsumeResult mirrors TerminalTicketConsumeResult.
type TerminalTicketConsumeResult struct {
	Valid      bool    `json:"valid"`
	UserID     int32   `json:"userId,omitempty"`
	InstanceID int32   `json:"instanceId,omitempty"`
	IssuedAt   int64   `json:"issuedAt,omitempty"`
	SessionID  *string `json:"sessionId,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// AgentLogStreamTicketConsumeResult mirrors AgentLogStreamTicketConsumeResult.
type AgentLogStreamTicketConsumeResult struct {
	Valid     bool    `json:"valid"`
	UserID    int32   `json:"userId,omitempty"`
	HostID    int32   `json:"hostId,omitempty"`
	IssuedAt  int64   `json:"issuedAt,omitempty"`
	SessionID *string `json:"sessionId,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// IncusUITicketVerifyResult mirrors IncusUiTicketVerifyResult.
type IncusUITicketVerifyResult struct {
	Valid     bool    `json:"valid"`
	UserID    int32   `json:"userId,omitempty"`
	HostID    int32   `json:"hostId,omitempty"`
	IssuedAt  int64   `json:"issuedAt,omitempty"`
	SessionID *string `json:"sessionId,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// consumeActionTicketAtomically runs the shared consume step under the store lock:
// not-found/type mismatch, expiry, then the usage counter. `extraCheck` runs after
// the expiry check (the bound-type mismatch rules differ per ticket type) and
// returns (ok, errorText, burn).
func consumeActionTicketAtomically(
	token string,
	ticketType ActionTicketType,
	notFoundError string,
	extraCheck func(entry actionTicket) (bool, string, bool),
) (actionTicket, bool, string) {
	now := actionTicketNow()

	actionTicketStore.Lock()
	defer actionTicketStore.Unlock()

	entry, ok := actionTicketStore.tickets[token]
	if !ok || entry.Type != ticketType {
		return actionTicket{}, false, notFoundError
	}

	if now > entry.ExpiresAt {
		delete(actionTicketStore.tickets, token)
		return actionTicket{}, false, "Ticket expired"
	}

	if extraCheck != nil {
		if valid, message, burn := extraCheck(entry); !valid {
			if burn {
				delete(actionTicketStore.tickets, token)
			}
			return actionTicket{}, false, message
		}
	}

	entry.UsageCount++
	if entry.UsageCount >= entry.MaxUsage {
		delete(actionTicketStore.tickets, token)
	} else {
		actionTicketStore.tickets[token] = entry
	}
	return entry, true, ""
}

// ConsumeOAuthBindTicket mirrors consumeOAuthBindTicket().
func ConsumeOAuthBindTicket(token string) OAuthBindTicketConsumeResult {
	entry, valid, message := consumeActionTicketAtomically(
		token, ActionTicketOAuthBind, "Ticket not found or already used", nil)
	if !valid {
		return OAuthBindTicketConsumeResult{Valid: false, Error: message}
	}
	return OAuthBindTicketConsumeResult{
		Valid:     true,
		UserID:    entry.UserID,
		IssuedAt:  entry.IssuedAt,
		SessionID: entry.SessionID,
	}
}

// ConsumeTerminalAccessTicket mirrors consumeTerminalAccessTicket(token, expectedInstanceId).
// A nil expectedInstanceID means "any instance".
func ConsumeTerminalAccessTicket(token string, expectedInstanceID *int32) TerminalTicketConsumeResult {
	var check func(actionTicket) (bool, string, bool)
	if expectedInstanceID != nil {
		check = func(entry actionTicket) (bool, string, bool) {
			if entry.InstanceID != *expectedInstanceID {
				return false, "Instance mismatch", true
			}
			return true, "", false
		}
	}
	entry, valid, message := consumeActionTicketAtomically(
		token, ActionTicketTerminal, "Ticket not found or already used", check)
	if !valid {
		return TerminalTicketConsumeResult{Valid: false, Error: message}
	}
	return TerminalTicketConsumeResult{
		Valid:      true,
		UserID:     entry.UserID,
		InstanceID: entry.InstanceID,
		IssuedAt:   entry.IssuedAt,
		SessionID:  entry.SessionID,
	}
}

// ConsumeAgentLogStreamTicket mirrors consumeAgentLogStreamTicket(token, expectedHostId).
func ConsumeAgentLogStreamTicket(token string, expectedHostID *int32) AgentLogStreamTicketConsumeResult {
	var check func(actionTicket) (bool, string, bool)
	if expectedHostID != nil {
		check = func(entry actionTicket) (bool, string, bool) {
			if entry.HostID != *expectedHostID {
				return false, "Host mismatch", true
			}
			return true, "", false
		}
	}
	entry, valid, message := consumeActionTicketAtomically(
		token, ActionTicketAgentLogStream, "Ticket not found or already used", check)
	if !valid {
		return AgentLogStreamTicketConsumeResult{Valid: false, Error: message}
	}
	return AgentLogStreamTicketConsumeResult{
		Valid:     true,
		UserID:    entry.UserID,
		HostID:    entry.HostID,
		IssuedAt:  entry.IssuedAt,
		SessionID: entry.SessionID,
	}
}

// VerifyIncusUIAccessTicket mirrors verifyIncusUiAccessTicket(token, expectedHostId).
// Unlike the consume helpers a host mismatch does NOT burn the ticket.
func VerifyIncusUIAccessTicket(token string, expectedHostID *int32) IncusUITicketVerifyResult {
	now := actionTicketNow()

	actionTicketStore.Lock()
	defer actionTicketStore.Unlock()

	entry, ok := actionTicketStore.tickets[token]
	if !ok || entry.Type != ActionTicketIncusUI {
		return IncusUITicketVerifyResult{Valid: false, Error: "Ticket not found or expired"}
	}
	if now > entry.ExpiresAt {
		delete(actionTicketStore.tickets, token)
		return IncusUITicketVerifyResult{Valid: false, Error: "Ticket expired"}
	}
	if expectedHostID != nil && entry.HostID != *expectedHostID {
		return IncusUITicketVerifyResult{Valid: false, Error: "Host mismatch"}
	}

	entry.UsageCount++
	if entry.UsageCount >= entry.MaxUsage {
		delete(actionTicketStore.tickets, token)
	} else {
		actionTicketStore.tickets[token] = entry
	}
	return IncusUITicketVerifyResult{
		Valid:     true,
		UserID:    entry.UserID,
		HostID:    entry.HostID,
		IssuedAt:  entry.IssuedAt,
		SessionID: entry.SessionID,
	}
}

// RevokeActionTicketsForSession mirrors revokeActionTicketsForSession(sessionId):
// drops every ticket tied to that refresh session and reports how many were revoked.
//
// This is the real implementation behind routes.AuthRevokeActionTicketsForSession
// (wired in routes/oauth.go's init).
func RevokeActionTicketsForSession(sessionID string) int {
	actionTicketStore.Lock()
	defer actionTicketStore.Unlock()

	revoked := 0
	for token, entry := range actionTicketStore.tickets {
		if entry.SessionID != nil && *entry.SessionID == sessionID {
			delete(actionTicketStore.tickets, token)
			revoked++
		}
	}
	return revoked
}

// CleanupExpiredActionTickets drops expired/exhausted tickets and returns how many
// were removed. The TypeScript does this on a 60 s unref'd interval; like the other
// process-local stores this port exposes it explicitly for the Wave E scheduler, and
// createActionTicket additionally sweeps lazily so the map stays bounded even
// before that scheduler exists.
func CleanupExpiredActionTickets() int {
	now := actionTicketNow()

	actionTicketStore.Lock()
	defer actionTicketStore.Unlock()

	removed := 0
	for token, entry := range actionTicketStore.tickets {
		if now > entry.ExpiresAt || entry.UsageCount >= entry.MaxUsage {
			delete(actionTicketStore.tickets, token)
			removed++
		}
	}
	return removed
}
