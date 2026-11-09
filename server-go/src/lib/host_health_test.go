// Tests for the shared host-health cache.
package lib

import "testing"

// resetHostHealth clears the process-wide cache between cases.
func resetHostHealth() {
	for hostID := range GetAllHostHealth() {
		ClearHostHealth(hostID)
	}
}

// pinHostHealthClock fixes Date.now() for the duration of the test.
func pinHostHealthClock(t *testing.T, now int64) {
	t.Helper()
	previous := hostHealthNow
	hostHealthNow = func() int64 { return now }
	t.Cleanup(func() { hostHealthNow = previous })
}

func TestHostHealthUnknownHostIsHealthy(t *testing.T) {
	resetHostHealth()
	if !IsHostHealthy(42) {
		t.Fatal("an unseen host must be considered healthy")
	}
	if GetHostHealth(42) != nil {
		t.Fatal("an unseen host must have no entry")
	}
}

func TestHostHealthSuccessResetsErrors(t *testing.T) {
	resetHostHealth()
	pinHostHealthClock(t, 1000)

	RecordHostError(1)
	RecordHostError(1)
	RecordHostError(1)
	if IsHostHealthy(1) {
		t.Fatal("3 consecutive errors must mark the host unhealthy")
	}

	RecordHostSuccess(1, 25)
	health := GetHostHealth(1)
	if health == nil {
		t.Fatal("expected an entry")
	}
	if health.ErrorCount != 0 || health.LastResponseTime != 25 || health.LastSuccess != 1000 || health.LastCheck != 1000 {
		t.Fatalf("health = %+v", health)
	}
	if !IsHostHealthy(1) {
		t.Fatal("a success must clear the unhealthy state")
	}
}

func TestHostHealthCooldown(t *testing.T) {
	resetHostHealth()
	now := int64(1_000_000)
	pinHostHealthClock(t, now)

	RecordHostError(7)
	RecordHostError(7)
	RecordHostError(7)
	if IsHostHealthy(7) {
		t.Fatal("expected unhealthy")
	}

	// Exactly at the cooldown boundary the host is still unhealthy (`>` not `>=`).
	hostHealthNow = func() int64 { return now + HostRecoveryCooldownMillis }
	if IsHostHealthy(7) {
		t.Fatal("the cooldown boundary must still be unhealthy")
	}

	// One millisecond later the host is retried and the counter is reset.
	hostHealthNow = func() int64 { return now + HostRecoveryCooldownMillis + 1 }
	if !IsHostHealthy(7) {
		t.Fatal("after the cooldown the host must be retried")
	}
	if health := GetHostHealth(7); health.ErrorCount != 0 {
		t.Fatalf("errorCount = %d after a cooldown retry, want 0", health.ErrorCount)
	}
}

func TestHostHealthUnhealthyListAndSummary(t *testing.T) {
	resetHostHealth()
	pinHostHealthClock(t, 500)

	RecordHostSuccess(1, 4)
	RecordHostSuccess(2, 3)
	RecordHostError(3)
	RecordHostError(3)
	RecordHostError(3)

	unhealthy := GetUnhealthyHosts()
	if len(unhealthy) != 1 || unhealthy[0] != 3 {
		t.Fatalf("unhealthy = %v, want [3]", unhealthy)
	}

	summary := GetHealthSummary()
	if summary.TotalHosts != 3 || summary.HealthyHosts != 2 || summary.UnhealthyHosts != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	// (4 + 3) / 2 = 3.5 → Math.round → 4.
	if summary.AvgResponseTime != 4 {
		t.Fatalf("avgResponseTime = %d, want 4", summary.AvgResponseTime)
	}
}

func TestHostHealthSummaryIgnoresZeroResponseTime(t *testing.T) {
	resetHostHealth()
	pinHostHealthClock(t, 1)

	RecordHostSuccess(1, 0)
	RecordHostSuccess(2, 9)

	summary := GetHealthSummary()
	if summary.AvgResponseTime != 9 {
		t.Fatalf("avgResponseTime = %d, want 9 (a zero response time is not averaged)", summary.AvgResponseTime)
	}
}

func TestHostHealthClearAndSnapshot(t *testing.T) {
	resetHostHealth()
	pinHostHealthClock(t, 100)

	RecordHostSuccess(5, 12)
	snapshot := GetAllHostHealth()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %v", snapshot)
	}

	// GetHostHealth / GetAllHostHealth return copies: mutating them must not
	// change the cache.
	snapshot[5] = HostHealth{ErrorCount: 99}
	entry := GetHostHealth(5)
	entry.ErrorCount = 99
	if GetHostHealth(5).ErrorCount != 0 {
		t.Fatal("the cache was mutated through a returned value")
	}

	ClearHostHealth(5)
	if GetHostHealth(5) != nil {
		t.Fatal("ClearHostHealth did not remove the entry")
	}
	if !IsHostHealthy(5) {
		t.Fatal("a cleared host must be healthy again")
	}
}
