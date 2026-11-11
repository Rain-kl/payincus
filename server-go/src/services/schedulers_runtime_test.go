// Tests for the shared scheduler runtime: DISABLE_SCHEDULERS, stop semantics,
// overlap protection and panic isolation. None of these touch a database or the
// network.
package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// closedAfter returns a channel that is already closed so a loop fires as fast as
// the scheduler can spawn jobs.
func closedAfter() <-chan time.Time {
	ready := make(chan time.Time)
	close(ready)
	return ready
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}

func TestSchedulersDisabledByEnvironment(t *testing.T) {
	cases := []struct {
		value    string
		expected bool
	}{
		{"", true},
		{"0", true},
		{"1", false},
		{"true", false},
		{"false", true},
		{"TRUE", true},
	}
	for _, testCase := range cases {
		t.Setenv(SchedulerDisableEnv, testCase.value)
		if got := SchedulersEnabled(); got != testCase.expected {
			t.Fatalf("SchedulersEnabled() with %q = %v, want %v", testCase.value, got, testCase.expected)
		}
	}
}

func TestSchedulersDisableStartsNothing(t *testing.T) {
	t.Setenv(SchedulerDisableEnv, "1")

	var created atomic.Int64
	previousFactory := schedulerGroupFactory
	schedulerGroupFactory = func(parent context.Context) *schedulerGroup {
		created.Add(1)
		return newSchedulerGroup(parent)
	}
	defer func() { schedulerGroupFactory = previousFactory }()

	stop := StartSchedulers(context.Background())
	if created.Load() != 0 {
		t.Fatalf("DISABLE_SCHEDULERS created %d scheduler groups, want 0", created.Load())
	}
	// The no-op stop must be safe to call (and idempotent).
	stop()
	stop()
}

func TestSchedulersEnabledCreatesOneGroup(t *testing.T) {
	t.Setenv(SchedulerDisableEnv, "")
	var created atomic.Int64
	previousFactory := schedulerGroupFactory
	schedulerGroupFactory = func(parent context.Context) *schedulerGroup {
		created.Add(1)
		group := newSchedulerGroup(parent)
		// Never fire: this test only proves the wiring runs.
		group.after = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
		return group
	}
	defer func() { schedulerGroupFactory = previousFactory }()

	stop := StartSchedulers(context.Background())
	defer stop()
	if created.Load() != 1 {
		t.Fatalf("StartSchedulers created %d groups, want 1", created.Load())
	}
}

func TestSchedulersNamesCoverEveryJob(t *testing.T) {
	want := []string{
		"traffic", "auto-policy", "billing", "mail-expiry", "mail-autorenew",
		"mail-usage", "ticket-auto-close", "status", "startup-cleanup",
	}
	got := SchedulerNames()
	if len(got) != len(want) {
		t.Fatalf("SchedulerNames returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("SchedulerNames[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestSchedulersStopStopsFurtherTicks(t *testing.T) {
	group := newSchedulerGroup(context.Background())
	group.after = func(time.Duration) <-chan time.Time { return closedAfter() }

	var ticks atomic.Int64
	group.startLoop("test-loop", func(now time.Time) time.Time {
		return now.Add(time.Millisecond)
	}, func(context.Context) error {
		ticks.Add(1)
		return nil
	}, nil)

	if !waitFor(t, 2*time.Second, func() bool { return ticks.Load() >= 3 }) {
		t.Fatalf("loop fired %d times, want at least 3", ticks.Load())
	}

	group.stop()
	snapshot := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if after := ticks.Load(); after != snapshot {
		t.Fatalf("loop kept firing after stop: %d → %d", snapshot, after)
	}
	// stop is idempotent and must not block the second time.
	group.stop()
}

func TestSchedulersStopWaitsForInFlightJob(t *testing.T) {
	group := newSchedulerGroup(context.Background())
	group.after = func(time.Duration) <-chan time.Time { return make(chan time.Time) }

	var finished atomic.Bool
	group.spawn("in-flight", func(context.Context) error {
		time.Sleep(30 * time.Millisecond)
		finished.Store(true)
		return nil
	}, nil)

	group.stop()
	if !finished.Load() {
		t.Fatal("stop returned before the in-flight job finished")
	}
}

func TestSchedulersOverlapGuardSkipsWhileRunning(t *testing.T) {
	group := newSchedulerGroup(context.Background())
	guard := &atomic.Bool{}
	guard.Store(true)

	var runs atomic.Int64
	group.spawn("guarded", func(context.Context) error {
		runs.Add(1)
		return nil
	}, guard)

	time.Sleep(20 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatalf("guarded job ran %d times while the flag was set, want 0", runs.Load())
	}

	guard.Store(false)
	group.spawn("guarded", func(context.Context) error {
		runs.Add(1)
		return nil
	}, guard)
	if !waitFor(t, 2*time.Second, func() bool { return runs.Load() == 1 }) {
		t.Fatalf("guarded job ran %d times after clearing the flag, want 1", runs.Load())
	}
	// The guard must be released once the job returns, otherwise the next tick is
	// skipped forever (the TypeScript clears isRunning in a finally block).
	if !waitFor(t, 2*time.Second, func() bool { return !guard.Load() }) {
		t.Fatal("overlap guard was not released after the job finished")
	}
}

func TestSchedulersJobPanicIsIsolated(t *testing.T) {
	group := newSchedulerGroup(context.Background())

	var survived atomic.Bool
	group.spawn("panics", func(context.Context) error {
		panic("boom")
	}, nil)
	group.spawn("survives", func(context.Context) error {
		survived.Store(true)
		return nil
	}, nil)

	if !waitFor(t, 2*time.Second, func() bool { return survived.Load() }) {
		t.Fatal("a panicking job prevented the following job from running")
	}
	group.stop()
}

func TestSchedulersFailureDoesNotStopTheLoop(t *testing.T) {
	group := newSchedulerGroup(context.Background())
	group.after = func(time.Duration) <-chan time.Time { return closedAfter() }

	var attempts atomic.Int64
	group.startLoop("failing", func(now time.Time) time.Time {
		return now.Add(time.Millisecond)
	}, func(context.Context) error {
		attempts.Add(1)
		return context.DeadlineExceeded
	}, nil)

	if !waitFor(t, 2*time.Second, func() bool { return attempts.Load() >= 3 }) {
		t.Fatalf("failing loop stopped after %d attempts, want at least 3", attempts.Load())
	}
	group.stop()
}
