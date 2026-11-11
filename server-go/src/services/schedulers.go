// Unified entry point for the background schedulers (Wave E).
//
// The Node backend starts its schedulers from `app.ts` after `fastify.listen()`
// (services/*-scheduler.ts) and four inline `setInterval` cleanups at the bottom of
// the same file. `cmd/server/main.go` deliberately does not start anything yet, so
// this file is the single place the main program will call:
//
//	stop := services.StartSchedulers(ctx)
//	defer stop()
//
// Design constraints that come from docs/WAVE_D_SPEC.md:
//
//   - §0.1 single instance. Every scheduler keeps its bookkeeping in process memory
//     (the `isRunning` flags and the cron cursor). There is no Redis / external
//     queue and no leader election. Running two instances would double every
//     billing tick, exactly as it would in Node.
//   - §0.2b UTC. `now` is always `time.Now().UTC()`, and every value that reaches
//     SQL is `.UTC()`.
//   - Trigger times replicate node-cron. node-cron computes the next fire from the
//     schedule expression and the *current* time; it never fires twice for the same
//     boundary. The per-scheduler `...NextRun` helpers implement that rule as pure
//     functions so the boundaries are unit-testable.
//
// Timezone note: node-cron without a `timezone` option uses the system zone, and
// production containers run with a UTC clock. The Go port makes that explicit —
// plain cron expressions are evaluated in UTC — so a developer machine in UTC+8
// cannot shift the schedule. The only zoned schedule is the daily traffic reset,
// which node-cron pins to TRAFFIC_TIME_ZONE (Asia/Shanghai).
package services

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// SchedulerStopTimeout bounds how long StartSchedulers' stop function waits for
// in-flight work after cancelling the context. A hung DB call must not block
// process shutdown forever; the goroutines are left to finish on their own.
const SchedulerStopTimeout = 30 * time.Second

// SchedulerDisableEnv is the documented escape hatch: DISABLE_SCHEDULERS=1 turns
// every background job off. It exists for runs that must not mutate rows (parity /
// test harnesses, one-off migrations) and as an operational kill switch.
const SchedulerDisableEnv = "DISABLE_SCHEDULERS"

// SchedulersEnabled reports whether background jobs should start.
//
// Only the two spellings the rest of the codebase treats as "on" are accepted
// (the same rule as RESET_DATABASE in app.ts: `"true"` or `"1"`).
func SchedulersEnabled() bool {
	value := os.Getenv(SchedulerDisableEnv)
	return value != "1" && value != "true"
}

// schedulerGroup owns the context, the wait group and the two seams (`now` and
// `after`) every scheduler runs on.
//
// `now` and `after` exist so tests can drive the loops without sleeping for a
// minute. Production always uses time.Now().UTC and time.After.
type schedulerGroup struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	stopped atomic.Bool
}

// schedulerGroupFactory is a seam for the DISABLE_SCHEDULERS test: it must prove
// that no group (and therefore no goroutine) is created at all.
var schedulerGroupFactory = newSchedulerGroup

func newSchedulerGroup(parent context.Context) *schedulerGroup {
	ctx, cancel := context.WithCancel(parent)
	return &schedulerGroup{
		ctx:    ctx,
		cancel: cancel,
		now:    func() time.Time { return time.Now().UTC() },
		after:  time.After,
	}
}

// startLoop registers a cron-style loop. `next` is the pure "next fire time"
// function; `fire` is invoked on every boundary in its own goroutine.
//
// When `guarded` is non-nil the Node `isRunning` flag semantics apply: a tick that
// arrives while the previous run is still executing is skipped and logged, never
// queued. status-scheduler.ts is the reference implementation for that comment.
func (g *schedulerGroup) startLoop(name string, next func(time.Time) time.Time, fire func(context.Context) error, guarded *atomic.Bool) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		for {
			now := g.now()
			nextAt := next(now)
			wait := nextAt.Sub(now)
			if wait < 0 {
				wait = 0
			}
			select {
			case <-g.ctx.Done():
				return
			case <-g.after(wait):
			}
			if g.ctx.Err() != nil {
				return
			}
			g.spawn(name, fire, guarded)
		}
	}()
}

// startIntervalLoop mirrors `setInterval` with an optional immediate first run
// (the Node mail-expiry and ticket-auto-close schedulers call the job once before
// installing the interval).
//
// node's setInterval does not fire immediately, so `runImmediately` is a separate
// explicit flag rather than a property of the loop.
func (g *schedulerGroup) startIntervalLoop(name string, interval time.Duration, runImmediately bool, fire func(context.Context) error, guarded *atomic.Bool) {
	if runImmediately {
		g.spawn(name, fire, guarded)
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		next := g.now().Add(interval)
		for {
			wait := next.Sub(g.now())
			if wait < 0 {
				wait = 0
			}
			select {
			case <-g.ctx.Done():
				return
			case <-g.after(wait):
			}
			if g.ctx.Err() != nil {
				return
			}
			g.spawn(name, fire, guarded)
			next = next.Add(interval)
			// node's setInterval coalesces missed ticks: a job that ran longer than
			// one interval resumes on the next tick instead of bursting to catch up.
			if next.Before(g.now()) {
				next = g.now().Add(interval)
			}
		}
	}()
}

// runIsolated runs one per-item unit of work with the try/catch semantics the
// TypeScript uses inside every job loop: a panic is logged and the round continues
// with the next item. Without this, one corrupted row could abort a whole billing
// round (or kill the process, since the loop goroutines have no recover of their
// own).
func runIsolated(label string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[%s] Item panicked: %v", label, recovered)
		}
	}()
	fn()
}

// spawn runs one job invocation, honouring the overlap guard and converting a
// panic into a logged error so a single bad row cannot kill the process.
func (g *schedulerGroup) spawn(name string, fire func(context.Context) error, guarded *atomic.Bool) {
	if g.stopped.Load() || g.ctx.Err() != nil {
		return
	}
	if guarded != nil && !guarded.CompareAndSwap(false, true) {
		log.Printf("[%s] Previous run is still active, skipping this tick", name)
		return
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if guarded != nil {
			defer guarded.Store(false)
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("[%s] Job panicked: %v", name, recovered)
			}
		}()
		if err := fire(g.ctx); err != nil {
			log.Printf("[%s] Job failed: %v", name, err)
		}
	}()
}

// wait blocks until every loop and in-flight job has returned, or the timeout
// expires.
func (g *schedulerGroup) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// stop cancels every loop, then waits for in-flight work.
func (g *schedulerGroup) stop() {
	if !g.stopped.CompareAndSwap(false, true) {
		return
	}
	g.cancel()
	if !g.wait(SchedulerStopTimeout) {
		log.Printf("[Schedulers] Stop timed out after %s; in-flight jobs continue in the background", SchedulerStopTimeout)
	}
}

// SchedulerNames lists every job the unified entry point starts, in start order.
//
// The list is the port of app.ts: seven service schedulers plus the four inline
// cleanups. status-scheduler.ts is included because app.ts also starts it and
// cmd/server/main.go does not.
func SchedulerNames() []string {
	return []string{
		"traffic",
		"auto-policy",
		"billing",
		"mail-expiry",
		"mail-autorenew",
		"mail-usage",
		"ticket-auto-close",
		"status",
		"startup-cleanup",
	}
}

// StartSchedulers starts every background job and returns a stop function.
//
// The returned stop cancels all tickers and waits for in-flight jobs to exit (up
// to SchedulerStopTimeout). After stop returns no new tick can start, which is
// what a graceful shutdown needs: the database connection pool must not be closed
// underneath a running billing transaction.
//
// DISABLE_SCHEDULERS=1 (or "true") returns a no-op stop and starts nothing.
func StartSchedulers(ctx context.Context) (stop func()) {
	if !SchedulersEnabled() {
		log.Printf("[Schedulers] %s is set; background jobs are disabled", SchedulerDisableEnv)
		return func() {}
	}

	group := schedulerGroupFactory(ctx)

	// Order mirrors app.ts: traffic first (it is the oldest scheduler), then the
	// policy/billing jobs, the mail jobs, status, and finally the inline cleanups.
	startTrafficScheduler(group)
	startAutoPolicyScheduler(group)
	startBillingScheduler(group)
	startMailExpiryScheduler(group)
	startMailAutoRenewScheduler(group)
	startMailUsageScheduler(group)
	startTicketAutoCloseScheduler(group)
	startStatusSchedulerJob(group)
	startStartupCleanup(group)

	log.Printf("[Schedulers] All background jobs started: %v", SchedulerNames())
	return group.stop
}

// startStatusSchedulerJob wires the already-ported status reconciler into the
// unified group. status_scheduler.go owns the cron arithmetic; this only adapts it
// to the shared stop semantics.
func startStatusSchedulerJob(group *schedulerGroup) {
	group.startLoop("StatusSync", StatusSchedulerNextRun, func(ctx context.Context) error {
		RunStatusSyncJob(ctx)
		return nil
	}, nil)
}

// ==================== cron helpers (pure) ====================

// schedulerEveryNMinutesNextRun mirrors node-cron's `*/n * * * *`.
//
// The next boundary is strictly after `now`: sitting exactly on a boundary yields
// the following one, because cron does not re-fire the boundary it just fired.
func schedulerEveryNMinutesNextRun(now time.Time, minutes int) time.Time {
	if minutes <= 0 {
		minutes = 1
	}
	step := time.Duration(minutes) * time.Minute
	utc := now.UTC()
	truncated := utc.Truncate(step)
	if truncated.Before(utc) {
		return truncated.Add(step)
	}
	return truncated.Add(step)
}

// schedulerDailyUTCNextRun mirrors node-cron's `m h * * *` on a UTC clock.
func schedulerDailyUTCNextRun(now time.Time, hour, minute int) time.Time {
	utc := now.UTC()
	target := time.Date(utc.Year(), utc.Month(), utc.Day(), hour, minute, 0, 0, time.UTC)
	if !target.After(utc) {
		target = target.AddDate(0, 0, 1)
	}
	return target
}

// schedulerHourlyAtNextRun mirrors node-cron's `m * * * *`: the next HH:m on the
// UTC clock.
func schedulerHourlyAtNextRun(now time.Time, minute int) time.Time {
	utc := now.UTC()
	target := time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour(), minute, 0, 0, time.UTC)
	if !target.After(utc) {
		target = target.Add(time.Hour)
	}
	return target
}

// schedulerDailyInLocationNextRun mirrors `schedule(expr, { timezone })`: the next
// hh:mm wall-clock time in `location`, returned as a UTC instant.
func schedulerDailyInLocationNextRun(now time.Time, location *time.Location, hour, minute int) time.Time {
	local := now.In(location)
	target := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, location)
	if !target.After(local) {
		target = target.AddDate(0, 0, 1)
	}
	return target.UTC()
}

// trafficLocation is TRAFFIC_TIME_ZONE, the one zone a scheduler pins explicitly.
func trafficLocation() *time.Location {
	location, err := time.LoadLocation(lib.TrafficTimeZone)
	if err != nil {
		return time.UTC
	}
	return location
}

// int64FromInt32 adapts the id types used by the notification layer (int64) and
// the database layer (int32).
func int64FromInt32(value int32) *int64 {
	converted := int64(value)
	return &converted
}
