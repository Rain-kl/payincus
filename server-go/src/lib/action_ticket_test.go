// Tests for the action-ticket port: single use, expiry, type isolation, binding to
// user/session, host/instance mismatch, and the atomic consume under concurrency.
package lib

import (
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withActionTicketClock pins the ticket clock for the duration of a test.
func withActionTicketClock(t *testing.T, now *int64) {
	t.Helper()
	original := actionTicketNow
	actionTicketNow = func() int64 { return *now }
	t.Cleanup(func() { actionTicketNow = original })
}

// resetActionTicketStore empties the process-local store so tests never share state.
func resetActionTicketStore(t *testing.T) {
	t.Helper()
	actionTicketStore.Lock()
	actionTicketStore.tickets = make(map[string]actionTicket)
	actionTicketStore.Unlock()
	t.Cleanup(func() {
		actionTicketStore.Lock()
		actionTicketStore.tickets = make(map[string]actionTicket)
		actionTicketStore.Unlock()
	})
}

func ptrString(value string) *string { return &value }

func TestActionTicketOAuthBindSingleUse(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	token := GenerateOAuthBindTicket(42, 1_699_999_000, ptrString("sess-1"))
	if len(token) != 32 {
		t.Fatalf("ticket length = %d, want 32", len(token))
	}

	first := ConsumeOAuthBindTicket(token)
	if !first.Valid {
		t.Fatalf("first consume must succeed, got %+v", first)
	}
	if first.UserID != 42 || first.IssuedAt != 1_699_999_000 {
		t.Fatalf("ticket identity = %+v", first)
	}
	if first.SessionID == nil || *first.SessionID != "sess-1" {
		t.Fatalf("session binding = %v", first.SessionID)
	}

	// Replay: the ticket was deleted by the maxUsage=1 rule.
	second := ConsumeOAuthBindTicket(token)
	if second.Valid {
		t.Fatalf("replay must fail")
	}
	if second.Error != "Ticket not found or already used" {
		t.Fatalf("replay error = %q", second.Error)
	}
}

func TestActionTicketExpiry(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	token := GenerateOAuthBindTicket(7, now, nil)
	if result := ConsumeOAuthBindTicket(token); !result.Valid {
		t.Fatalf("fresh ticket must be valid")
	}

	token = GenerateOAuthBindTicket(7, now, nil)
	now += 60*1000 + 1 // oauth-bind TTL is 60 s; Node uses `now > expiresAt`
	expired := ConsumeOAuthBindTicket(token)
	if expired.Valid || expired.Error != "Ticket expired" {
		t.Fatalf("expired ticket = %+v", expired)
	}
	// The expired entry is deleted on first touch.
	if again := ConsumeOAuthBindTicket(token); again.Error != "Ticket not found or already used" {
		t.Fatalf("expired ticket must be forgotten, got %q", again.Error)
	}

	// Exactly at the expiry instant the ticket is still valid (`now > expiresAt`).
	now = int64(1_700_000_000_000)
	token = GenerateOAuthBindTicket(8, now, nil)
	now += 60 * 1000
	edge := ConsumeOAuthBindTicket(token)
	if !edge.Valid {
		t.Fatalf("ticket at the exact expiry instant must still be valid, got %+v", edge)
	}
}

func TestActionTicketTypeIsolation(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	terminal := GenerateTerminalAccessTicket(9, 5, now, nil)
	if result := ConsumeOAuthBindTicket(terminal); result.Valid {
		t.Fatalf("a terminal ticket must never satisfy an oauth-bind consume")
	}
	if result := ConsumeTerminalAccessTicket(terminal, nil); !result.Valid {
		t.Fatalf("the terminal ticket must still be consumable through its own path")
	}
	// Unknown token: same generic error, no type disclosure.
	if result := ConsumeOAuthBindTicket("not-a-ticket"); result.Error != "Ticket not found or already used" {
		t.Fatalf("unknown token error = %q", result.Error)
	}
}

func TestActionTicketConcurrentConsumeOnlyOnce(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	for round := 0; round < 20; round++ {
		token := GenerateOAuthBindTicket(11, now, nil)
		var validCount int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if ConsumeOAuthBindTicket(token).Valid {
					atomic.AddInt32(&validCount, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if validCount != 1 {
			t.Fatalf("round %d: %d consumers succeeded, want exactly 1", round, validCount)
		}
	}
}

func TestActionTicketSessionRevocation(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	keep := GenerateOAuthBindTicket(1, now, ptrString("session-a"))
	revoked := GenerateOAuthBindTicket(1, now, ptrString("session-b"))
	secondRevoked := GenerateTerminalAccessTicket(1, 3, now, ptrString("session-b"))
	noSession := GenerateOAuthBindTicket(1, now, nil)

	if count := RevokeActionTicketsForSession("session-b"); count != 2 {
		t.Fatalf("revoked %d tickets, want 2", count)
	}
	if ConsumeOAuthBindTicket(revoked).Valid {
		t.Fatalf("revoked ticket must not be consumable")
	}
	if ConsumeTerminalAccessTicket(secondRevoked, nil).Valid {
		t.Fatalf("revoked terminal ticket must not be consumable")
	}
	if !ConsumeOAuthBindTicket(keep).Valid {
		t.Fatalf("tickets of other sessions must survive")
	}
	if !ConsumeOAuthBindTicket(noSession).Valid {
		t.Fatalf("session-less tickets must survive")
	}
	if count := RevokeActionTicketsForSession("session-b"); count != 0 {
		t.Fatalf("second revoke = %d, want 0", count)
	}
}

func TestActionTicketTerminalInstanceBinding(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	other := int32(99)
	token := GenerateTerminalAccessTicket(3, 12, now, nil)
	mismatch := ConsumeTerminalAccessTicket(token, &other)
	if mismatch.Valid || mismatch.Error != "Instance mismatch" {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	// Node deletes the ticket on an instance mismatch.
	if retry := ConsumeTerminalAccessTicket(token, nil); retry.Valid {
		t.Fatalf("mismatched ticket must be burned")
	}

	expected := int32(12)
	token = GenerateTerminalAccessTicket(3, 12, now, nil)
	if result := ConsumeTerminalAccessTicket(token, &expected); !result.Valid || result.InstanceID != 12 {
		t.Fatalf("expected-instance consume = %+v", result)
	}
}

func TestActionTicketAgentLogStreamHostBinding(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	token := GenerateAgentLogStreamTicket(4, 77, now, ptrString("s"))
	expected := int32(77)
	result := ConsumeAgentLogStreamTicket(token, &expected)
	if !result.Valid || result.HostID != 77 || result.UserID != 4 {
		t.Fatalf("consume = %+v", result)
	}
	if ConsumeAgentLogStreamTicket(token, &expected).Valid {
		t.Fatalf("agent-log-stream ticket must be single use")
	}

	other := int32(78)
	token = GenerateAgentLogStreamTicket(4, 77, now, nil)
	if mismatch := ConsumeAgentLogStreamTicket(token, &other); mismatch.Error != "Host mismatch" {
		t.Fatalf("host mismatch = %+v", mismatch)
	}
	if ConsumeAgentLogStreamTicket(token, nil).Valid {
		t.Fatalf("host-mismatched ticket must be burned")
	}
}

func TestActionTicketIncusUIMultiUse(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	hostID := int32(21)
	token := GenerateIncusUIAccessTicket(5, hostID, now, ptrString("sess"))

	// A host mismatch must NOT burn the ticket (unlike the consume helpers).
	other := int32(22)
	if result := VerifyIncusUIAccessTicket(token, &other); result.Valid || result.Error != "Host mismatch" {
		t.Fatalf("host mismatch = %+v", result)
	}
	// 256 uses, then the ticket is exhausted.
	for i := 0; i < 256; i++ {
		result := VerifyIncusUIAccessTicket(token, &hostID)
		if !result.Valid {
			t.Fatalf("use %d must be valid, got %+v", i+1, result)
		}
	}
	if result := VerifyIncusUIAccessTicket(token, &hostID); result.Valid || result.Error != "Ticket not found or expired" {
		t.Fatalf("exhausted ticket = %+v", result)
	}

	// Expiry message differs from the not-found message.
	token = GenerateIncusUIAccessTicket(5, hostID, now, nil)
	now += 5*60*1000 + 1
	if result := VerifyIncusUIAccessTicket(token, nil); result.Valid || result.Error != "Ticket expired" {
		t.Fatalf("expired incus-ui ticket = %+v", result)
	}
}

func TestActionTicketCleanupExpired(t *testing.T) {
	resetActionTicketStore(t)
	now := int64(1_700_000_000_000)
	withActionTicketClock(t, &now)

	GenerateOAuthBindTicket(1, now, nil)
	GenerateAgentLogStreamTicket(1, 2, now, nil)
	if removed := CleanupExpiredActionTickets(); removed != 0 {
		t.Fatalf("fresh tickets must not be removed, got %d", removed)
	}
	now += 60*1000 + 1
	if removed := CleanupExpiredActionTickets(); removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
}

func TestActionTicketTokenShape(t *testing.T) {
	resetActionTicketStore(t)
	allowed := regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)
	seen := map[string]bool{}
	for i := 0; i < 128; i++ {
		token := GenerateOAuthBindTicket(1, 1, nil)
		if !allowed.MatchString(token) {
			t.Fatalf("token %q is not 32 URL-safe characters", token)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q", token)
		}
		seen[token] = true
	}
	// Cheap sanity: time-based fallback tokens are still 32 chars.
	if value := actionTicketRandomToken(); len(value) != 32 {
		t.Fatalf("random token length = %d", len(value))
	}
	_ = time.Now
}
