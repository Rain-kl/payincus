// Port of server/src/workers/db-failure-backoff.ts.
//
// All four Wave E workers poll the database on a fixed interval. When PostgreSQL
// is briefly unavailable (pool exhaustion, an administrator terminating
// connections, a failover) they must not keep hammering it: arm a shared backoff
// and skip ticks until it expires. The Window is advisory and per-process, which is
// correct for a single-instance backend (AGENTS.md "私有后端必须单实例运行"); a
// second replica would arm its own timer, exactly like Node.
package workers

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const defaultWorkerDBBackoff = 15 * time.Second

// parsePositiveInt mirrors the helper in db-failure-backoff.ts: a missing,
// non-numeric or non-positive value falls back to the default.
func parsePositiveInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// workerDBBackoff mirrors createWorkerDbBackoff. Unlike the TS closure it is
// safe for concurrent use because Go workers may share one instance between the
// poll loop and the timeout loop.
type workerDBBackoff struct {
	label   string
	backoff time.Duration

	mu           sync.Mutex
	backoffUntil time.Time
	now          func() time.Time
}

// newWorkerDBBackoff builds a backoff whose duration comes from
// DB_WORKER_BACKOFF_MS, read once at construction like the module-level constant in
// the TypeScript.
func newWorkerDBBackoff(label string) *workerDBBackoff {
	ms := parsePositiveInt(os.Getenv("DB_WORKER_BACKOFF_MS"), int(defaultWorkerDBBackoff/time.Millisecond))
	return &workerDBBackoff{label: label, backoff: time.Duration(ms) * time.Millisecond, now: time.Now}
}

func (b *workerDBBackoff) shouldSkip() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now().Before(b.backoffUntil)
}

// arm mirrors arm(): only transient database errors arm the timer. A business
// error (bad payload, missing row) must fail the task, not pause the queue.
func (b *workerDBBackoff) arm(err error) {
	if b == nil || !isTransientDatabaseError(err) {
		return
	}
	b.mu.Lock()
	b.backoffUntil = b.now().Add(b.backoff)
	backoff := b.backoff
	b.mu.Unlock()
	log.Printf("[%s] Database backoff armed for %s", b.label, backoff)
}

func (b *workerDBBackoff) reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.backoffUntil = time.Time{}
	b.mu.Unlock()
}

// isTransientDatabaseError mirrors isTransientDatabaseError. Prisma reports the
// interesting cases as P1001 (can't reach database server), P2028 (transaction
// API error) and P2034 (transaction conflict, retry); in pgx they surface as
// connection errors and the messages listed below. String matching is kept on
// purpose: it is what the Node code does, and the messages are produced by the
// server, not by our code.
func isTransientDatabaseError(err error) bool {
	if err == nil {
		return false
	}

	// Shutdown cancellation is not a database failure.
	if errors.Is(err, context.Canceled) {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "53300", // too_many_connections
			"57P01", // admin_shutdown
			"57P02", // crash_shutdown
			"57P03", // cannot_connect_now
			"08000", // connection_exception
			"08003", // connection_does_not_exist
			"08006", // connection_failure
			"40001", // serialization_failure (P2034)
			"40P01": // deadlock_detected
			return true
		}
	}

	message := strings.ToLower(err.Error())
	for _, needle := range []string{
		"timeout exceeded when trying to connect",
		"unable to start a transaction in the given time",
		"too many clients",
		"remaining connection slots are reserved",
		"connection terminated unexpectedly",
		"terminating connection due to administrator command",
		"connection refused",
		"broken pipe",
		"server closed the connection unexpectedly",
		"failed to connect",
	} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}
