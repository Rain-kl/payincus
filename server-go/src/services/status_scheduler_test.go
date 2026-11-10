package services

import (
	"context"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

func TestStatusSchedulerNextRun(t *testing.T) {
	utc := time.UTC
	cst := time.FixedZone("CST", 8*3600)

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			// 12:00:00.000 is a boundary: cron already fired for this minute, so the
			// next run is one minute later.
			name: "exact boundary",
			now:  time.Date(2026, 11, 10, 12, 0, 0, 0, utc),
			want: time.Date(2026, 11, 10, 12, 1, 0, 0, utc),
		},
		{
			name: "one millisecond past the boundary",
			now:  time.Date(2026, 11, 10, 12, 0, 0, 1_000_000, utc), // 1 ms
			want: time.Date(2026, 11, 10, 12, 1, 0, 0, utc),
		},
		{
			name: "mid-minute",
			now:  time.Date(2026, 11, 10, 12, 34, 56, 789_000_000, utc),
			want: time.Date(2026, 11, 10, 12, 35, 0, 0, utc),
		},
		{
			name: "last millisecond of the minute",
			now:  time.Date(2026, 11, 10, 12, 34, 59, 999_000_000, utc),
			want: time.Date(2026, 11, 10, 12, 35, 0, 0, utc),
		},
		{
			name: "crosses midnight",
			now:  time.Date(2026, 11, 10, 23, 59, 30, 0, utc),
			want: time.Date(2026, 11, 11, 0, 0, 0, 0, utc),
		},
		{
			// Same instant expressed in UTC+8 must produce the same absolute next run
			// (20:34:56+08:00 == 12:34:56Z).
			name: "UTC+8 wall clock is irrelevant",
			now:  time.Date(2026, 11, 10, 20, 34, 56, 789_000_000, cst),
			want: time.Date(2026, 11, 10, 12, 35, 0, 0, utc),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StatusSchedulerNextRun(tc.now)
			if !got.Equal(tc.want) {
				t.Errorf("NextRun(%s) = %s, want %s", tc.now.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
			}
			if got.Location() != time.UTC {
				t.Errorf("NextRun must be expressed in UTC, got %s", got.Location())
			}
			if got.Second() != 0 || got.Nanosecond() != 0 {
				t.Errorf("NextRun must land on a whole minute, got %s", got.Format(time.RFC3339Nano))
			}
			if !got.After(tc.now) {
				t.Error("NextRun must be strictly after now")
			}
		})
	}
}

func TestStatusSchedulerSyncIntervalMs(t *testing.T) {
	cases := map[string]int64{
		"running":   2 * 60 * 1000,
		"stopped":   10 * 60 * 1000,
		"error":     10 * 60 * 1000,
		"suspended": 10 * 60 * 1000,
		"":          10 * 60 * 1000,
	}
	for status, want := range cases {
		if got := StatusSchedulerSyncIntervalMs(status); got != want {
			t.Errorf("SyncIntervalMs(%q) = %d, want %d", status, got, want)
		}
	}
	if StatusSchedulerSyncIntervalRunning != 120_000 || StatusSchedulerSyncIntervalStopped != 600_000 {
		t.Error("interval constants must stay 2 min / 10 min")
	}
}

func TestStatusSchedulerDueForSync(t *testing.T) {
	const now = int64(1_800_000_000_000)

	cases := []struct {
		name     string
		lastSync int64
		status   string
		want     bool
	}{
		// running: strictly greater than 2 minutes.
		{"running exactly 2 min", now - 120_000, "running", false},
		{"running 2 min + 1ms", now - 120_001, "running", true},
		{"running 1 min", now - 60_000, "running", false},
		{"running 3 min", now - 180_000, "running", true},
		// everything else: strictly greater than 10 minutes.
		{"stopped exactly 10 min", now - 600_000, "stopped", false},
		{"stopped 10 min + 1ms", now - 600_001, "stopped", true},
		{"stopped 2 min", now - 120_000, "stopped", false},
		{"error exactly 10 min", now - 600_000, "error", false},
		{"error 11 min", now - 660_000, "error", true},
		// A clock that moved backwards never makes an instance due.
		{"future last sync", now + 60_000, "running", false},
	}

	for _, tc := range cases {
		if got := StatusSchedulerDueForSync(tc.lastSync, now, tc.status); got != tc.want {
			t.Errorf("%s: DueForSync(last=%d, now=%d, %q) = %v, want %v", tc.name, tc.lastSync, now, tc.status, got, tc.want)
		}
	}
}

func TestStatusSchedulerLastSyncMillisFallsBackToUpdatedAt(t *testing.T) {
	updated := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	synced := time.Date(2026, 11, 10, 12, 5, 0, 0, time.UTC)

	withoutSync := StatusSchedulerInstance{UpdatedAt: updated}
	if got := StatusSchedulerLastSyncMillis(withoutSync); got != updated.UnixMilli() {
		t.Errorf("last sync = %d, want updatedAt %d", got, updated.UnixMilli())
	}

	withSync := StatusSchedulerInstance{UpdatedAt: updated, LastSyncedAt: &synced}
	if got := StatusSchedulerLastSyncMillis(withSync); got != synced.UnixMilli() {
		t.Errorf("last sync = %d, want lastSyncedAt %d", got, synced.UnixMilli())
	}

	// Timezone-shifted representations of the same instant are equal.
	inCST := time.Date(2026, 11, 10, 20, 5, 0, 0, time.FixedZone("CST", 8*3600))
	if got := StatusSchedulerLastSyncMillis(StatusSchedulerInstance{LastSyncedAt: &inCST, UpdatedAt: updated}); got != synced.UnixMilli() {
		t.Errorf("last sync with UTC+8 timestamp = %d, want %d", got, synced.UnixMilli())
	}
}

func TestStatusSchedulerFilterDue(t *testing.T) {
	const now = int64(1_800_000_000_000)
	fresh := time.UnixMilli(now - 60_000)
	stale := time.UnixMilli(now - 11*60_000)

	instances := []StatusSchedulerInstance{
		{ID: 1, Status: "running", LastSyncedAt: &stale},
		{ID: 2, Status: "running", LastSyncedAt: &fresh}, // not due yet
		{ID: 3, Status: "stopped", LastSyncedAt: &stale},
		{ID: 4, Status: "error", UpdatedAt: time.UnixMilli(now - 12*60_000)}, // falls back to updatedAt
		{ID: 5, Status: "running", UpdatedAt: time.UnixMilli(now - 90_000)},  // 1.5 min: not due
	}

	got := StatusSchedulerFilterDue(instances, now, 50)
	if len(got) != 3 {
		t.Fatalf("got %d due instances, want 3: %+v", len(got), statusSchedulerIDs(got))
	}
	if got[0].ID != 1 || got[1].ID != 3 || got[2].ID != 4 {
		t.Errorf("due order = %v, want [1 3 4]", statusSchedulerIDs(got))
	}

	// The slice truncates to batchSize but keeps the SQL order.
	truncated := StatusSchedulerFilterDue(instances, now, 2)
	if len(truncated) != 2 || truncated[0].ID != 1 || truncated[1].ID != 3 {
		t.Errorf("truncated = %v, want [1 3]", statusSchedulerIDs(truncated))
	}

	// batchSize 0 yields nothing (the TypeScript would slice to an empty batch).
	if got := StatusSchedulerFilterDue(instances, now, 0); len(got) != 0 {
		t.Errorf("batchSize 0 returned %v", statusSchedulerIDs(got))
	}

	// An empty candidate list is normal.
	if got := StatusSchedulerFilterDue(nil, now, 50); len(got) != 0 {
		t.Errorf("nil returned %v", statusSchedulerIDs(got))
	}
}

func TestStatusSchedulerGroupByHost(t *testing.T) {
	instances := []StatusSchedulerInstance{
		{ID: 1, HostID: 7},
		{ID: 2, HostID: 3},
		{ID: 3, HostID: 7},
		{ID: 4, HostID: 9},
		{ID: 5, HostID: 3},
	}
	groups := StatusSchedulerGroupByHost(instances)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	// First-appearance order, like the JavaScript Map.
	if groups[0].HostID != 7 || groups[1].HostID != 3 || groups[2].HostID != 9 {
		t.Errorf("group order = %d,%d,%d", groups[0].HostID, groups[1].HostID, groups[2].HostID)
	}
	if len(groups[0].Instances) != 2 || groups[0].Instances[0].ID != 1 || groups[0].Instances[1].ID != 3 {
		t.Errorf("host 7 instances = %v", statusSchedulerIDs(groups[0].Instances))
	}
	if len(groups[1].Instances) != 2 || groups[1].Instances[0].ID != 2 || groups[1].Instances[1].ID != 5 {
		t.Errorf("host 3 instances = %v", statusSchedulerIDs(groups[1].Instances))
	}

	if got := StatusSchedulerGroupByHost(nil); len(got) != 0 {
		t.Errorf("nil input produced %d groups", len(got))
	}
}

func TestStatusSchedulerEvaluate(t *testing.T) {
	const now int64 = 1_800_000_000_000

	t.Run("empty node status forgets and skips", func(t *testing.T) {
		observations := lib.NewInstanceStatusObservations()
		observations.ShouldApply(1, "running", "error", now) // open a window
		guard := StatusSchedulerEvaluate(observations, 1, "running", "", false, now)
		if guard.Action != StatusSchedulerActionSkip {
			t.Errorf("action = %v", guard.Action)
		}
		if observations.Len() != 0 {
			t.Error("the observation window must be forgotten")
		}
	})

	t.Run("unknown mapping forgets and skips", func(t *testing.T) {
		observations := lib.NewInstanceStatusObservations()
		observations.ShouldApply(1, "running", "error", now)
		guard := StatusSchedulerEvaluate(observations, 1, "running", "unknown", false, now)
		if guard.Action != StatusSchedulerActionSkip || observations.Len() != 0 {
			t.Errorf("action = %v, len = %d", guard.Action, observations.Len())
		}
	})

	t.Run("active operation forgets and skips", func(t *testing.T) {
		observations := lib.NewInstanceStatusObservations()
		observations.ShouldApply(1, "running", "error", now-60_000) // old enough to apply
		guard := StatusSchedulerEvaluate(observations, 1, "running", "error", true, now)
		if guard.Action != StatusSchedulerActionSkip {
			t.Errorf("action = %v, want skip", guard.Action)
		}
		if observations.Len() != 0 {
			t.Error("the window must be forgotten during an operation")
		}
	})

	t.Run("error transition is debounced", func(t *testing.T) {
		observations := lib.NewInstanceStatusObservations()
		if guard := StatusSchedulerEvaluate(observations, 1, "running", "error", false, now); guard.Action != StatusSchedulerActionSkip {
			t.Errorf("first error observation must skip, got %v", guard.Action)
		}
		if guard := StatusSchedulerEvaluate(observations, 1, "running", "error", false, now+29_999); guard.Action != StatusSchedulerActionSkip {
			t.Errorf("29 999 ms later must still skip, got %v", guard.Action)
		}
		if guard := StatusSchedulerEvaluate(observations, 1, "running", "error", false, now+30_000); guard.Action != StatusSchedulerActionApplyTransition {
			t.Errorf("30 s later must apply, got %v", guard.Action)
		}
	})

	t.Run("running to stopped announces", func(t *testing.T) {
		observations := lib.NewInstanceStatusObservations()
		guard := StatusSchedulerEvaluate(observations, 1, "running", "stopped", false, now)
		if guard.Action != StatusSchedulerActionApplyTransition || !guard.Announce {
			t.Errorf("guard = %+v, want apply+announce", guard)
		}
	})

	t.Run("other transitions do not announce", func(t *testing.T) {
		for _, pair := range [][2]string{
			{"stopped", "running"},
			{"error", "running"},
			{"stopped", "error"},
			{"running", "running"},
		} {
			observations := lib.NewInstanceStatusObservations()
			guard := StatusSchedulerEvaluate(observations, 1, pair[0], pair[1], false, now)
			if guard.Announce {
				t.Errorf("%s → %s must not announce", pair[0], pair[1])
			}
		}
	})

	t.Run("same status only touches", func(t *testing.T) {
		observations := lib.NewInstanceStatusObservations()
		for _, status := range []string{"running", "stopped", "error"} {
			guard := StatusSchedulerEvaluate(observations, 1, status, status, false, now)
			if guard.Action != StatusSchedulerActionTouch {
				t.Errorf("%s → %s = %v, want touch", status, status, guard.Action)
			}
		}
	})
}

func TestStatusSchedulerHostHealth(t *testing.T) {
	const hostID int32 = 990_001
	lib.ClearHostHealth(hostID)
	t.Cleanup(func() { lib.ClearHostHealth(hostID) })

	lib.RecordHostSuccess(hostID, 120)
	health := GetStatusSchedulerHostHealth()[hostID]
	if health.LastResponseTime != 120 || health.ErrorCount != 0 || !health.Healthy {
		t.Errorf("after success: %+v", health)
	}

	for i := 0; i < lib.HostErrorThreshold; i++ {
		lib.RecordHostError(hostID)
	}
	health = GetStatusSchedulerHostHealth()[hostID]
	if health.ErrorCount != lib.HostErrorThreshold || health.Healthy {
		t.Errorf("after %d errors: %+v", lib.HostErrorThreshold, health)
	}
}

func TestStatusSchedulerJobReentrancyGuard(t *testing.T) {
	statusSchedulerSetRunningForTest(true)
	t.Cleanup(func() { statusSchedulerSetRunningForTest(false) })

	// Must return immediately without touching the database.
	RunStatusSyncJob(context.Background())

	if !statusSchedulerIsJobRunning() {
		t.Error("the guard must not be cleared by a skipped run")
	}
}

func TestStatusSchedulerStartIsIdempotent(t *testing.T) {
	statusSchedulerResetForTest()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the ticker goroutine exits immediately

	StartStatusScheduler(ctx)
	if !statusSchedulerStarted.Load() {
		t.Fatal("the started flag must be set")
	}
	// A second call is a no-op (and must not spawn a second ticker).
	StartStatusScheduler(ctx)

	t.Cleanup(func() { statusSchedulerResetForTest() })
}

func TestHasActiveInstanceOperationRequiresDatabase(t *testing.T) {
	if _, err := HasActiveInstanceOperation(context.Background(), 1); err == nil {
		t.Fatal("expected an error without a database")
	}
}

func statusSchedulerIDs(instances []StatusSchedulerInstance) []int32 {
	out := make([]int32, 0, len(instances))
	for _, instance := range instances {
		out = append(out, instance.ID)
	}
	return out
}
