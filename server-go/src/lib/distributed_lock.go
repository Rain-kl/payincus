// Port of server/src/lib/distributed-lock.ts.
//
// LOGIC004: a *table-level* distributed lock for cross-process critical sections
// (quota updates to avoid overselling, task scheduling to avoid double execution,
// resource allocation to avoid races).
//
// Distinct from db/advisory_locks.go: those are transaction-scoped PostgreSQL
// advisory locks that are released the instant their transaction ends. This lock
// is a persisted row in "distributed_locks" with an expiry timestamp, so it
// survives across transactions and processes and self-heals after a crash once it
// expires. Mutual exclusion comes from the primary key on lock_key plus the
// conditional ON CONFLICT ... DO UPDATE, which is atomic in PostgreSQL.
package lib

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// Default lock timings, mirroring DEFAULT_OPTIONS.
const (
	DistributedLockDefaultExpireMs        = 30000
	DistributedLockDefaultWaitTimeoutMs   = 5000
	DistributedLockDefaultRetryIntervalMs = 100
	DistributedLockDefaultExtendMs        = 30000
)

// Lock acquisition SQL, a verbatim port of the Prisma.sql template in acquireLock.
// The WHERE clause on DO UPDATE is what makes the statement a lock: an existing
// row is only taken over when it has expired or is already owned by this owner.
const distributedLockAcquireSQL = `
INSERT INTO "distributed_locks" ("lock_key", "owner_id", "acquired_at", "expires_at")
VALUES ($1, $2, $3, $4)
ON CONFLICT ("lock_key") DO UPDATE
SET
  "owner_id" = EXCLUDED."owner_id",
  "acquired_at" = EXCLUDED."acquired_at",
  "expires_at" = EXCLUDED."expires_at"
WHERE
  "distributed_locks"."expires_at" <= $3
  OR "distributed_locks"."owner_id" = EXCLUDED."owner_id"
RETURNING "id", "owner_id"`

// errDistributedLockNoDatabase is returned when the shared pool is not open.
var errDistributedLockNoDatabase = errors.New("distributed lock: database not initialized")

// nanoidAlphabet is the default nanoid alphabet (64 characters, so the modulo in
// nanoid() is unbiased for a full byte range).
const nanoidAlphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"

// LockOptions mirrors the LockOptions interface. A nil field means "use the
// default" (30000 / 5000 / 100 ms); an explicit zero is honoured, matching the
// TypeScript `{ ...DEFAULT_OPTIONS, ...options }` spread.
type LockOptions struct {
	ExpireMs        *int
	WaitTimeoutMs   *int
	RetryIntervalMs *int
	OwnerID         string
}

// LockResult mirrors the LockResult interface.
type LockResult struct {
	Success bool   `json:"success"`
	LockID  string `json:"lockId,omitempty"`
	OwnerID string `json:"ownerId,omitempty"`
	Error   string `json:"error,omitempty"`
}

// WithLockResult mirrors the withLock() return shape.
type WithLockResult[T any] struct {
	Success bool   `json:"success"`
	Result  T      `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
}

// LockStatus mirrors the getLockStatus() return shape.
type LockStatus struct {
	Exists     bool       `json:"exists"`
	IsExpired  bool       `json:"isExpired"`
	OwnerID    *string    `json:"ownerId,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	AcquiredAt *time.Time `json:"acquiredAt,omitempty"`
}

type resolvedLockOptions struct {
	expireMs      time.Duration
	waitTimeout   time.Duration
	retryInterval time.Duration
	ownerID       string
}

func resolveLockOptions(options LockOptions) resolvedLockOptions {
	expireMs := DistributedLockDefaultExpireMs
	if options.ExpireMs != nil {
		expireMs = *options.ExpireMs
	}
	waitTimeout := DistributedLockDefaultWaitTimeoutMs
	if options.WaitTimeoutMs != nil {
		waitTimeout = *options.WaitTimeoutMs
	}
	retryInterval := DistributedLockDefaultRetryIntervalMs
	if options.RetryIntervalMs != nil {
		retryInterval = *options.RetryIntervalMs
	}
	ownerID := options.OwnerID
	if ownerID == "" {
		ownerID = generateLockOwnerID()
	}

	return resolvedLockOptions{
		expireMs:      time.Duration(expireMs) * time.Millisecond,
		waitTimeout:   time.Duration(waitTimeout) * time.Millisecond,
		retryInterval: time.Duration(retryInterval) * time.Millisecond,
		ownerID:       ownerID,
	}
}

// generateLockOwnerID mirrors generateOwnerId: "<pid>-<nanoid(8)>".
func generateLockOwnerID() string {
	return strconv.Itoa(os.Getpid()) + "-" + nanoid(8)
}

// nanoid returns a random string of the given length over the nanoid alphabet.
func nanoid(size int) string {
	if size <= 0 {
		return ""
	}
	out := make([]byte, size)
	if _, err := rand.Read(out); err != nil {
		// crypto/rand failing is not recoverable; degrade to a time-seeded suffix so
		// the caller still gets a non-empty, most-likely-unique owner id.
		seed := strconv.FormatInt(time.Now().UnixNano(), 36)
		for i := range out {
			out[i] = nanoidAlphabet[int(seed[i%len(seed)])%len(nanoidAlphabet)]
		}
		return string(out)
	}
	for i, b := range out {
		out[i] = nanoidAlphabet[int(b)%len(nanoidAlphabet)]
	}
	return string(out)
}

// AcquireLock mirrors acquireLock. It polls until the lock is taken or the wait
// timeout elapses. A retryable write-conflict/deadlock (Prisma P2034, i.e.
// SQLSTATE 40001/40P01) is retried; any other database error is returned.
func AcquireLock(ctx context.Context, lockKey string, options LockOptions) (LockResult, error) {
	opts := resolveLockOptions(options)

	pool := db.Pool()
	if pool == nil {
		return LockResult{}, errDistributedLockNoDatabase
	}

	deadline := time.Now().Add(opts.waitTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return LockResult{}, err
		}

		now := time.Now()
		expiresAt := now.Add(opts.expireMs)

		var id int64
		var ownerID string
		err := pool.QueryRow(ctx, distributedLockAcquireSQL, lockKey, opts.ownerID, now, expiresAt).Scan(&id, &ownerID)
		if err == nil {
			return LockResult{Success: true, LockID: strconv.FormatInt(id, 10), OwnerID: ownerID}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			if isRetryableLockError(err) {
				sleepWithContext(ctx, opts.retryInterval)
				continue
			}
			return LockResult{}, err
		}

		// No row returned: another owner holds a live lock. Wait and retry.
		sleepWithContext(ctx, opts.retryInterval)
	}

	return LockResult{Success: false, Error: "Lock acquisition timeout"}, nil
}

// ReleaseLock mirrors releaseLock. Only the owner may release the lock.
func ReleaseLock(ctx context.Context, lockKey, ownerID string) bool {
	pool := db.Pool()
	if pool == nil {
		return false
	}

	tag, err := pool.Exec(ctx,
		`DELETE FROM "distributed_locks" WHERE "lock_key" = $1 AND "owner_id" = $2`,
		lockKey, ownerID)
	if err != nil {
		log.Printf("[DistributedLock] Failed to release lock %s: %v", lockKey, err)
		return false
	}
	return tag.RowsAffected() > 0
}

// ExtendLock mirrors extendLock. Only a still-unexpired lock held by ownerID can
// be extended. A nil extendMs uses the 30000 ms default.
func ExtendLock(ctx context.Context, lockKey, ownerID string, extendMs *int) bool {
	pool := db.Pool()
	if pool == nil {
		return false
	}

	extend := DistributedLockDefaultExtendMs
	if extendMs != nil {
		extend = *extendMs
	}

	now := time.Now()
	expiresAt := now.Add(time.Duration(extend) * time.Millisecond)

	tag, err := pool.Exec(ctx,
		`UPDATE "distributed_locks" SET "expires_at" = $3
		 WHERE "lock_key" = $1 AND "owner_id" = $2 AND "expires_at" > $4`,
		lockKey, ownerID, expiresAt, now)
	if err != nil {
		log.Printf("[DistributedLock] Failed to extend lock %s: %v", lockKey, err)
		return false
	}
	return tag.RowsAffected() > 0
}

// WithLock mirrors withLock: acquire, run fn, always release. fn returning an error
// is reported through the envelope, not propagated.
func WithLock[T any](ctx context.Context, lockKey string, fn func() (T, error), options LockOptions) WithLockResult[T] {
	lockResult, err := AcquireLock(ctx, lockKey, options)
	if err != nil {
		return WithLockResult[T]{Success: false, Error: err.Error()}
	}
	if !lockResult.Success || lockResult.OwnerID == "" {
		message := lockResult.Error
		if message == "" {
			message = "Failed to acquire lock"
		}
		return WithLockResult[T]{Success: false, Error: message}
	}

	defer ReleaseLock(ctx, lockKey, lockResult.OwnerID)

	result, fnErr := fn()
	if fnErr != nil {
		return WithLockResult[T]{Success: false, Error: fnErr.Error()}
	}
	return WithLockResult[T]{Success: true, Result: result}
}

// CleanupExpiredLocks mirrors cleanupExpiredLocks: delete every expired row and
// report how many were removed.
func CleanupExpiredLocks(ctx context.Context) (int64, error) {
	pool := db.Pool()
	if pool == nil {
		return 0, errDistributedLockNoDatabase
	}

	tag, err := pool.Exec(ctx, `DELETE FROM "distributed_locks" WHERE "expires_at" < $1`, time.Now())
	if err != nil {
		return 0, err
	}

	count := tag.RowsAffected()
	if count > 0 {
		log.Printf("[DistributedLock] Cleaned up %d expired locks", count)
	}
	return count, nil
}

// GetLockStatus mirrors getLockStatus. A missing lock reports exists=false; the
// TypeScript function never returns null.
func GetLockStatus(ctx context.Context, lockKey string) (LockStatus, error) {
	pool := db.Pool()
	if pool == nil {
		return LockStatus{}, errDistributedLockNoDatabase
	}

	var ownerID string
	var expiresAt, acquiredAt time.Time
	err := pool.QueryRow(ctx,
		`SELECT "owner_id", "expires_at", "acquired_at" FROM "distributed_locks" WHERE "lock_key" = $1`,
		lockKey).Scan(&ownerID, &expiresAt, &acquiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LockStatus{Exists: false, IsExpired: false}, nil
	}
	if err != nil {
		return LockStatus{}, err
	}

	return LockStatus{
		Exists:     true,
		IsExpired:  expiresAt.Before(time.Now()),
		OwnerID:    &ownerID,
		ExpiresAt:  &expiresAt,
		AcquiredAt: &acquiredAt,
	}, nil
}

// isRetryableLockError reports whether err is the Go/PostgreSQL equivalent of
// Prisma's P2034 ("write conflict or deadlock"): SQLSTATE 40001
// (serialization_failure) or 40P01 (deadlock_detected).
func isRetryableLockError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// sleepWithContext waits for d, or until ctx is done. It reports whether the wait
// completed normally.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
