// Unit tests for the distributed-lock port. They live in package lib so they can
// exercise the unexported helpers directly. The DB-backed paths require a live
// PostgreSQL and are covered by conformance runs, not here.
package lib

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
)

func TestNanoidLengthAndAlphabet(t *testing.T) {
	for _, size := range []int{1, 8, 21, 64} {
		got := nanoid(size)
		if len(got) != size {
			t.Fatalf("nanoid(%d) length = %d, want %d", size, len(got), size)
		}
		for _, char := range got {
			if !strings.ContainsRune(nanoidAlphabet, char) {
				t.Fatalf("nanoid(%d) produced %q outside the alphabet", size, char)
			}
		}
	}

	if nanoid(0) != "" || nanoid(-3) != "" {
		t.Error("non-positive sizes must produce an empty string")
	}
}

func TestNanoidIsNotDeterministic(t *testing.T) {
	if nanoid(21) == nanoid(21) {
		t.Fatal("two consecutive nanoids collided; randomness is broken")
	}
}

func TestGenerateLockOwnerID(t *testing.T) {
	first := generateLockOwnerID()
	second := generateLockOwnerID()

	if !strings.Contains(first, "-") {
		t.Fatalf("owner id %q should be <pid>-<nanoid>", first)
	}
	parts := strings.SplitN(first, "-", 2)
	if parts[1] == "" || len(parts[1]) != 8 {
		t.Fatalf("owner id %q should end with an 8-character nanoid", first)
	}
	if first == second {
		t.Fatal("two generated owner ids must differ")
	}
}

func TestResolveLockOptionsDefaults(t *testing.T) {
	got := resolveLockOptions(LockOptions{})
	if got.expireMs != 30*time.Second {
		t.Errorf("expireMs = %v, want 30s", got.expireMs)
	}
	if got.waitTimeout != 5*time.Second {
		t.Errorf("waitTimeout = %v, want 5s", got.waitTimeout)
	}
	if got.retryInterval != 100*time.Millisecond {
		t.Errorf("retryInterval = %v, want 100ms", got.retryInterval)
	}
	if got.ownerID == "" {
		t.Error("ownerID should be generated when omitted")
	}
}

func TestResolveLockOptionsOverrides(t *testing.T) {
	expire, wait, retry := 1000, 2000, 50
	got := resolveLockOptions(LockOptions{
		ExpireMs:        &expire,
		WaitTimeoutMs:   &wait,
		RetryIntervalMs: &retry,
		OwnerID:         "owner-1",
	})
	if got.expireMs != time.Second {
		t.Errorf("expireMs = %v, want 1s", got.expireMs)
	}
	if got.waitTimeout != 2*time.Second {
		t.Errorf("waitTimeout = %v, want 2s", got.waitTimeout)
	}
	if got.retryInterval != 50*time.Millisecond {
		t.Errorf("retryInterval = %v, want 50ms", got.retryInterval)
	}
	if got.ownerID != "owner-1" {
		t.Errorf("ownerID = %q, want owner-1", got.ownerID)
	}

	// An explicit zero is honoured (the TypeScript spread does not treat 0 as unset).
	zero := 0
	got = resolveLockOptions(LockOptions{WaitTimeoutMs: &zero})
	if got.waitTimeout != 0 {
		t.Errorf("explicit zero waitTimeout = %v, want 0", got.waitTimeout)
	}
}

func TestIsRetryableLockError(t *testing.T) {
	retryable := []string{"40001", "40P01"}
	for _, code := range retryable {
		err := &pgconn.PgError{Code: code}
		if !isRetryableLockError(err) {
			t.Errorf("SQLSTATE %s should be retryable", code)
		}
	}

	notRetryable := []error{
		&pgconn.PgError{Code: "23505"}, // unique_violation
		errors.New("network"),
		nil,
	}
	for _, err := range notRetryable {
		if isRetryableLockError(err) {
			t.Errorf("%v should not be retryable", err)
		}
	}

	// Wrapped errors are still recognised.
	wrapped := errors.Join(errors.New("context"), &pgconn.PgError{Code: "40001"})
	if !isRetryableLockError(wrapped) {
		t.Error("wrapped 40001 should be retryable")
	}
}

func TestSleepWithContext(t *testing.T) {
	if !sleepWithContext(context.Background(), 0) {
		t.Error("a zero-duration sleep should succeed")
	}
	if !sleepWithContext(context.Background(), time.Millisecond) {
		t.Error("a short sleep should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepWithContext(ctx, time.Second) {
		t.Error("a cancelled context should abort the sleep")
	}
}

// TestAcquireLockWithoutDatabase asserts the explicit failure path (never a silent
// success) when the shared pool is not open.
func TestAcquireLockWithoutDatabase(t *testing.T) {
	if db.Pool() != nil {
		t.Skip("a live database pool is configured; skipping the no-database path")
	}

	if _, err := AcquireLock(context.Background(), "some-key", LockOptions{}); !errors.Is(err, errDistributedLockNoDatabase) {
		t.Fatalf("AcquireLock error = %v, want errDistributedLockNoDatabase", err)
	}
	if ReleaseLock(context.Background(), "some-key", "owner") {
		t.Error("ReleaseLock without a database must report false")
	}
	if ExtendLock(context.Background(), "some-key", "owner", nil) {
		t.Error("ExtendLock without a database must report false")
	}
	if _, err := CleanupExpiredLocks(context.Background()); !errors.Is(err, errDistributedLockNoDatabase) {
		t.Fatalf("CleanupExpiredLocks error = %v, want errDistributedLockNoDatabase", err)
	}
	if _, err := GetLockStatus(context.Background(), "some-key"); !errors.Is(err, errDistributedLockNoDatabase) {
		t.Fatalf("GetLockStatus error = %v, want errDistributedLockNoDatabase", err)
	}
}

// TestWithLockPropagatesAcquireFailure checks the withLock envelope when the lock
// cannot be taken.
func TestWithLockPropagatesAcquireFailure(t *testing.T) {
	if db.Pool() != nil {
		t.Skip("a live database pool is configured; skipping the no-database path")
	}

	called := false
	result := WithLock(context.Background(), "some-key", func() (int, error) {
		called = true
		return 1, nil
	}, LockOptions{})
	if result.Success {
		t.Error("WithLock should report failure when the lock cannot be acquired")
	}
	if result.Error == "" {
		t.Error("WithLock should surface an error message")
	}
	if called {
		t.Error("fn must not run when the lock was not acquired")
	}
}
