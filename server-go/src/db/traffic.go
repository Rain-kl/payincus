// Port of server/src/db/traffic.ts.
//
// Traffic accounting is a money-adjacent boundary, so three rules shape this file:
//
//  1. Every counter column is a PostgreSQL bigint and is handled as int64. Nothing
//     is ever rounded through a float except the display helpers and the user
//     facing traffic-reset price (which the TypeScript also converts with
//     Number()).
//  2. The monthly reset path keeps the exact lock ordering of the TypeScript:
//     a table-level distributed lock "traffic:instance:<id>" is acquired first,
//     the reset sample is read, then "traffic:user:<userId>" is acquired, and only
//     then does the transaction start. Inside that transaction the route takes the
//     *transaction-scoped* advisory lock 4121 (instance) and, when a reset price
//     applies, 4110 (user balance) — same namespaces, same order, same nesting.
//     Double charging is prevented by the balance advisory lock plus the
//     conditional `balance >= price` UPDATE; a lost charge is prevented by the
//     balance_log insert sharing the transaction.
//  3. db/ must not import lib/ (lib/ imports db/), so the small pure helpers that
//     traffic-utils.ts provides — getEffectiveLimit, calculateUserTrafficStatus,
//     calculateInstanceTrafficStatus, advanceTrafficSnapshot — are duplicated
//     here. They are byte-for-byte equivalent and pinned by
//     traffic_utils_test.go on the lib side and by the parity test in
//     db/traffic_test.go.
//
// port of the distributed-lock helper: withLock() lives in lib/distributed-lock.ts
// and lib/distributed_lock.go cannot be imported from here, so trafficWithLock
// below reproduces acquireLock/releaseLock against the same "distributed_locks"
// table with the same SQL. The lock keys and option values are identical.
package db

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TrafficTimeZone mirrors TRAFFIC_TIME_ZONE.
const TrafficTimeZone = "Asia/Shanghai"

// trafficBatchSize mirrors BATCH_SIZE: a hard cap on rows per statement.
const trafficBatchSize = 1000

// shanghaiUTCOffsetMS mirrors SHANGHAI_UTC_OFFSET_MS.
const shanghaiUTCOffsetMS = 8 * 60 * 60 * 1000

// trafficResetLockOptions mirrors TRAFFIC_RESET_LOCK_OPTIONS.
var trafficResetLockOptions = trafficLockOptions{
	expireMs:        30000,
	waitTimeoutMs:   10000,
	retryIntervalMs: 50,
}

// errTrafficRecordNotFound is the Go stand-in for Prisma P2025 ("Record to update
// not found"), raised by the helpers that mirror `prisma.<model>.update`.
var errTrafficRecordNotFound = errors.New("记录不存在")

// ==================== Shanghai calendar helpers ====================

// ShanghaiDateParts mirrors shanghaiDateParts. month is ZERO-BASED, exactly like
// Date#getUTCMonth, because callers mirror `Date.UTC(year, month, ...)`.
func ShanghaiDateParts(date time.Time) (year int, month int, day int) {
	shifted := date.UTC().Add(time.Duration(shanghaiUTCOffsetMS) * time.Millisecond)
	return shifted.Year(), int(shifted.Month()) - 1, shifted.Day()
}

// ShanghaiDateOnly mirrors shanghaiDateOnly: the UTC-midnight value for the
// Shanghai calendar day, shifted by dayOffset days. This is the canonical value
// stored in `daily_traffic.date`.
func ShanghaiDateOnly(date time.Time, dayOffset int) time.Time {
	year, month, day := ShanghaiDateParts(date)
	return time.Date(year, time.Month(month+1), day+dayOffset, 0, 0, 0, 0, time.UTC)
}

// StartOfMonthShanghai mirrors startOfMonthShanghai: the instant at 00:00 on the
// first day of the Shanghai calendar month (16:00 UTC of the previous day).
func StartOfMonthShanghai(date time.Time) time.Time {
	year, month, _ := ShanghaiDateParts(date)
	return time.Date(year, time.Month(month+1), 1, 0, 0, 0, 0, time.UTC).
		Add(-time.Duration(shanghaiUTCOffsetMS) * time.Millisecond)
}

// ==================== JS / bigint helpers ====================

// trafficNormalizeMoney mirrors `Number(value.toFixed(2))`. The rounding is
// ECMAScript toFixed's: round the exact binary value half away from zero at two
// decimals (so 2.675 → 2.67, matching V8, and 1.005 → 1.00).
func trafficNormalizeMoney(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	return trafficDecimalRound2(value)
}

// trafficDecimalRound2 rounds half away from zero at 2 decimals via big.Rat, so no
// binary-float rounding is introduced.
func trafficDecimalRound2(value float64) float64 {
	rational := new(big.Rat).SetFloat64(value)
	if rational == nil {
		return value
	}
	scale := big.NewRat(100, 1)
	scaled := new(big.Rat).Mul(rational, scale)
	// Round half away from zero on the integer part.
	num := new(big.Int).Set(scaled.Num())
	den := scaled.Denom()
	quotient, remainder := new(big.Int).QuoRem(num, den, new(big.Int))
	absoluteRemainder := new(big.Int).Abs(remainder)
	if new(big.Int).Lsh(absoluteRemainder, 1).Cmp(den) >= 0 {
		if num.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	result, _ := new(big.Rat).SetFrac(quotient, big.NewInt(100)).Float64()
	return result
}

// trafficNumberOrZero mirrors `Number(value) || 0`: an unparsable or NaN value
// collapses to zero, as it does in JavaScript.
func trafficNumberOrZero(text *string) float64 {
	if text == nil {
		return 0
	}
	parsed, err := strconv.ParseFloat(*text, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0
	}
	return parsed
}

// trafficDecimalFromFloat mirrors Prisma's number → Decimal conversion, which
// parses the shortest round-trippable representation of the double.
func trafficDecimalFromFloat(value float64) models.Decimal {
	return models.Decimal{Decimal: decimal.NewFromFloat(value)}
}

// trafficSanitizeBigInt mirrors sanitizeNumber(value: bigint): a negative counter
// is a programming error because every traffic counter is non-negative.
func trafficSanitizeBigInt(value int64) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("Invalid bigint value: %d", value)
	}
	return value, nil
}

// trafficSanitizeDate mirrors sanitizeDate: render the UTC date part and refuse
// anything that is not a real calendar day.
func trafficSanitizeDate(date time.Time) (string, error) {
	rendered := date.UTC().Format("2006-01-02")
	if len(rendered) != 10 {
		return "", fmt.Errorf("Invalid date format: %s", rendered)
	}
	return rendered, nil
}

// ==================== duplicated traffic-utils helpers ====================

// trafficGetEffectiveLimit mirrors getEffectiveLimit from services/traffic-utils.ts.
func trafficGetEffectiveLimit(limit *int64, extraQuota int64) *int64 {
	if limit == nil {
		return nil
	}
	total := *limit + extraQuota
	return &total
}

// trafficIsOverLimit mirrors isOverLimit (`used >= limit`).
func trafficIsOverLimit(used int64, limit *int64) bool {
	if limit == nil {
		return false
	}
	return used >= *limit
}

// trafficIsWarningThreshold mirrors isWarningThreshold (`used >= limit*80/100`),
// keeping the bigint truncating arithmetic of the TypeScript.
func trafficIsWarningThreshold(used int64, limit *int64) bool {
	if limit == nil {
		return false
	}
	return used >= (*limit*80)/100
}

// trafficCalculateUserStatus mirrors calculateUserTrafficStatus.
func trafficCalculateUserStatus(used int64, limit *int64) models.TrafficStatus {
	if trafficIsOverLimit(used, limit) {
		return models.TrafficStatusLIMITED
	}
	if trafficIsWarningThreshold(used, limit) {
		return models.TrafficStatusWARNING
	}
	return models.TrafficStatusNORMAL
}

// trafficCalculateInstanceStatus mirrors calculateInstanceTrafficStatus: an
// instance only has NORMAL and LIMITED.
func trafficCalculateInstanceStatus(used int64, limit *int64) models.TrafficStatus {
	if trafficIsOverLimit(used, limit) {
		return models.TrafficStatusLIMITED
	}
	return models.TrafficStatusNORMAL
}

// ==================== snapshot sample ====================

// TrafficSnapshotSample mirrors TrafficSnapshotSample. Pointers keep "not
// reported" distinguishable from a reported zero, exactly like the optional
// fields in the TypeScript interface.
type TrafficSnapshotSample struct {
	RxRaw     int64
	TxRaw     int64
	SampledAt time.Time
	Source    string
	// RxPacketsRaw / TxPacketsRaw mirror `rxPacketsRaw?: bigint`: nil means the
	// sample does not carry the counter, so the stored column is left untouched.
	RxPacketsRaw *int64
	TxPacketsRaw *int64
	// CPUUsageRaw mirrors `cpuUsageRaw?: bigint | null`. The traffic module's two
	// producers always include the key, so nil is written as SQL NULL.
	CPUUsageRaw *int64
}

// TrafficInstanceCounters mirrors NetworkCounters from lib/incus/incus-traffic.ts.
type TrafficInstanceCounters struct {
	RxBytes     int64
	TxBytes     int64
	RxPackets   int64
	TxPackets   int64
	CPUUsageRaw *int64
	SampledAt   time.Time
}

// TrafficFetchInstanceCounters mirrors getTrafficCounters(client, incusId) from
// server/src/lib/incus/incus-traffic.ts.
//
// SEAM: that module belongs to the Incus/流量 substrate wave and is not ported, and
// db/ cannot import lib/ (which owns the mTLS IncusClient). Until it lands this
// seam fails explicitly: getTrafficResetSample only needs it for instances whose
// status is 'running', and pretending the counters were read would corrupt the
// post-reset baseline (the next collection would re-charge the whole counter).
var TrafficFetchInstanceCounters = func(
	ctx context.Context, hostID int32, hostURL string, certPath, keyPath *string, incusID string,
) (TrafficInstanceCounters, error) {
	return TrafficInstanceCounters{}, errors.New(
		"实例流量计数器采集未接线（server/src/lib/incus/incus-traffic.ts 尚未移植）")
}

// trafficGetResetSample mirrors getTrafficResetSample.
type trafficResetSample struct {
	TrafficSnapshotSample
	UserID int32
}

func trafficGetResetSample(ctx context.Context, instanceID int32) (trafficResetSample, error) {
	if Pool() == nil {
		return trafficResetSample{}, errors.New("database not initialized")
	}

	var (
		userID       int32
		status       string
		incusID      string
		hostID       *int32
		hostURL      *string
		certPath     *string
		keyPath      *string
		snapshotID   *int32
		rxRaw        *int64
		txRaw        *int64
		rxPacketsRaw *int64
		txPacketsRaw *int64
		cpuUsageRaw  *int64
	)
	err := Pool().QueryRow(ctx, `
		SELECT i.user_id, i.status::text, i.incus_id,
		       h.id, h.url, h.cert_path, h.key_path,
		       ts.id, ts.rx_raw, ts.tx_raw, ts.rx_packets_raw, ts.tx_packets_raw, ts.cpu_usage_raw
		FROM instances i
		LEFT JOIN hosts h ON h.id = i.host_id
		LEFT JOIN traffic_snapshots ts ON ts.instance_id = i.id
		WHERE i.id = $1`, instanceID,
	).Scan(&userID, &status, &incusID, &hostID, &hostURL, &certPath, &keyPath,
		&snapshotID, &rxRaw, &txRaw, &rxPacketsRaw, &txPacketsRaw, &cpuUsageRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return trafficResetSample{}, fmt.Errorf("实例 %d 不存在", instanceID)
	}
	if err != nil {
		return trafficResetSample{}, err
	}
	if status == string(models.InstanceStatusDeleted) {
		return trafficResetSample{}, fmt.Errorf("实例 %d 不存在", instanceID)
	}

	if status == string(models.InstanceStatusRunning) {
		if hostID == nil || hostURL == nil {
			return trafficResetSample{}, fmt.Errorf("实例 %d 未绑定节点，无法重建流量基线", instanceID)
		}
		counters, err := TrafficFetchInstanceCounters(ctx, *hostID, *hostURL, certPath, keyPath, incusID)
		if err != nil {
			return trafficResetSample{}, err
		}
		return trafficResetSample{
			UserID: userID,
			TrafficSnapshotSample: TrafficSnapshotSample{
				RxRaw:        counters.RxBytes,
				TxRaw:        counters.TxBytes,
				RxPacketsRaw: &counters.RxPackets,
				TxPacketsRaw: &counters.TxPackets,
				CPUUsageRaw:  counters.CPUUsageRaw,
				SampledAt:    counters.SampledAt,
				Source:       "active-collector",
			},
		}, nil
	}

	// Non-running instances reuse the stored snapshot (`?.rxRaw ?? 0n`).
	zero := int64(0)
	if rxRaw == nil {
		rxRaw = &zero
	}
	if txRaw == nil {
		txRaw = &zero
	}
	if rxPacketsRaw == nil {
		rxPacketsRaw = &zero
	}
	if txPacketsRaw == nil {
		txPacketsRaw = &zero
	}
	return trafficResetSample{
		UserID: userID,
		TrafficSnapshotSample: TrafficSnapshotSample{
			RxRaw:        *rxRaw,
			TxRaw:        *txRaw,
			RxPacketsRaw: rxPacketsRaw,
			TxPacketsRaw: txPacketsRaw,
			CPUUsageRaw:  cpuUsageRaw,
			SampledAt:    time.Now(),
			Source:       "active-collector",
		},
	}, nil
}

// AdvanceTrafficSnapshotResult mirrors the `{ accepted, previous }` return value.
type AdvanceTrafficSnapshotResult struct {
	Accepted bool
	Previous *models.TrafficSnapshot
}

// AdvanceTrafficSnapshot mirrors advanceTrafficSnapshot: the two collection
// channels share one snapshot and `updatedAt` is the sample time, so a late
// sample can neither roll the baseline back nor contribute to usage.
func AdvanceTrafficSnapshot(
	ctx context.Context, tx pgx.Tx, instanceID int32, sample TrafficSnapshotSample,
) (AdvanceTrafficSnapshotResult, error) {
	previous, err := trafficSnapshotInTransaction(ctx, tx, instanceID)
	if err != nil {
		return AdvanceTrafficSnapshotResult{}, err
	}

	if previous == nil {
		// Create the baseline row. Omitted packet counters take the column
		// defaults (0), exactly like the Prisma create.
		if _, err := tx.Exec(ctx, `
			INSERT INTO traffic_snapshots
				(instance_id, rx_raw, tx_raw, rx_packets_raw, tx_packets_raw, cpu_usage_raw, updated_at)
			VALUES ($1, $2, $3, COALESCE($4, 0), COALESCE($5, 0), $6, $7)`,
			instanceID, sample.RxRaw, sample.TxRaw, sample.RxPacketsRaw, sample.TxPacketsRaw,
			sample.CPUUsageRaw, sample.SampledAt,
		); err != nil {
			return AdvanceTrafficSnapshotResult{}, err
		}
		return AdvanceTrafficSnapshotResult{Accepted: true, Previous: nil}, nil
	}

	tag, err := tx.Exec(ctx, `
		UPDATE traffic_snapshots
		SET rx_raw = $2,
		    tx_raw = $3,
		    updated_at = $4,
		    rx_packets_raw = COALESCE($5, rx_packets_raw),
		    tx_packets_raw = COALESCE($6, tx_packets_raw),
		    cpu_usage_raw = $7
		WHERE instance_id = $1 AND updated_at <= $4`,
		instanceID, sample.RxRaw, sample.TxRaw, sample.SampledAt,
		sample.RxPacketsRaw, sample.TxPacketsRaw, sample.CPUUsageRaw,
	)
	if err != nil {
		return AdvanceTrafficSnapshotResult{}, err
	}
	if tag.RowsAffected() == 0 {
		// Dropped stale sample: the baseline is newer than this sample.
		return AdvanceTrafficSnapshotResult{Accepted: false, Previous: previous}, nil
	}
	return AdvanceTrafficSnapshotResult{Accepted: true, Previous: previous}, nil
}

// trafficSnapshotInTransaction mirrors `tx.trafficSnapshot.findUnique`.
func trafficSnapshotInTransaction(ctx context.Context, tx pgx.Tx, instanceID int32) (*models.TrafficSnapshot, error) {
	var snapshot models.TrafficSnapshot
	err := tx.QueryRow(ctx, `
		SELECT id, instance_id, rx_raw, tx_raw, rx_packets_raw, tx_packets_raw, cpu_usage_raw, updated_at
		FROM traffic_snapshots WHERE instance_id = $1`, instanceID,
	).Scan(&snapshot.ID, &snapshot.InstanceID, &snapshot.RxRaw, &snapshot.TxRaw,
		&snapshot.RxPacketsRaw, &snapshot.TxPacketsRaw, &snapshot.CPUUsageRaw, &snapshot.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// ==================== distributed lock (table-scoped) ====================

// trafficLockOptions mirrors LockOptions.
type trafficLockOptions struct {
	expireMs        int
	waitTimeoutMs   int
	retryIntervalMs int
}

// trafficLockAcquireSQL is a verbatim copy of the SQL in
// lib/distributed_lock.go / lib/distributed-lock.ts.
const trafficLockAcquireSQL = `
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

// trafficLockOwnerID mirrors generateOwnerId: "<pid>-<8 random chars>".
func trafficLockOwnerID() string {
	const alphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	for index, value := range buffer {
		buffer[index] = alphabet[int(value)%len(alphabet)]
	}
	return strconv.Itoa(os.Getpid()) + "-" + string(buffer)
}

// trafficAcquireLock mirrors acquireLock: poll the conditional upsert until the
// lock is taken or waitTimeoutMs elapses.
func trafficAcquireLock(ctx context.Context, lockKey string, options trafficLockOptions) (string, error) {
	pool := Pool()
	if pool == nil {
		return "", errors.New("database not initialized")
	}

	ownerID := trafficLockOwnerID()
	deadline := time.Now().Add(time.Duration(options.waitTimeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		now := time.Now()
		expiresAt := now.Add(time.Duration(options.expireMs) * time.Millisecond)

		var id int64
		var owner string
		err := pool.QueryRow(ctx, trafficLockAcquireSQL, lockKey, ownerID, now, expiresAt).Scan(&id, &owner)
		if err == nil {
			return owner, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			if trafficIsRetryableLockError(err) {
				trafficSleep(ctx, options.retryIntervalMs)
				continue
			}
			return "", err
		}
		trafficSleep(ctx, options.retryIntervalMs)
	}
	return "", errors.New("Lock acquisition timeout")
}

// trafficReleaseLock mirrors releaseLock: only the owner may delete the row. The
// release always runs (even when the request context is already cancelled), like
// the TypeScript, so a disconnected client cannot leave the lock held until its
// 30s expiry.
func trafficReleaseLock(ctx context.Context, lockKey, ownerID string) {
	pool := Pool()
	if pool == nil {
		return
	}
	_, _ = pool.Exec(context.WithoutCancel(ctx),
		`DELETE FROM "distributed_locks" WHERE "lock_key" = $1 AND "owner_id" = $2`,
		lockKey, ownerID)
}

// trafficSleep waits retryIntervalMs, or until ctx is done.
func trafficSleep(ctx context.Context, retryIntervalMs int) {
	timer := time.NewTimer(time.Duration(retryIntervalMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// trafficIsRetryableLockError mirrors the P2034 retry rule: SQLSTATE 40001
// (serialization_failure) or 40P01 (deadlock_detected).
func trafficIsRetryableLockError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// trafficInstanceLockKey mirrors getTrafficInstanceLockKey.
func trafficInstanceLockKey(instanceID int32) string {
	return "traffic:instance:" + strconv.FormatInt(int64(instanceID), 10)
}

// trafficUserLockKey mirrors getTrafficUserLockKey.
func trafficUserLockKey(userID int32) string {
	return "traffic:user:" + strconv.FormatInt(int64(userID), 10)
}

// trafficWithLock mirrors withLock: acquire, run, always release.
func trafficWithLock[T any](
	ctx context.Context, lockKey string, options trafficLockOptions, fn func() (T, error),
) (T, error) {
	var zero T
	ownerID, err := trafficAcquireLock(ctx, lockKey, options)
	if err != nil {
		return zero, err
	}
	defer trafficReleaseLock(ctx, lockKey, ownerID)
	return fn()
}

// trafficRunTransaction mirrors prisma.$transaction for the reset paths. The
// advisory locks taken on tx are released by COMMIT/ROLLBACK.
func trafficRunTransaction[T any](
	ctx context.Context, fn func(context.Context, pgx.Tx) (T, error),
) (T, error) {
	var zero T
	pool := Pool()
	if pool == nil {
		return zero, errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := fn(ctx, tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

// ==================== reset locks ====================

// WithInstanceTrafficResetLock mirrors withInstanceTrafficResetLock. The nesting
// order is load-bearing: the instance lock is taken first, the live/stored sample
// is read, and only then the user lock and the transaction.
func WithInstanceTrafficResetLock[T any](
	ctx context.Context,
	instanceID int32,
	operation func(context.Context, pgx.Tx, TrafficSnapshotSample) (T, error),
) (T, error) {
	var zero T
	result, err := trafficWithLock(ctx, trafficInstanceLockKey(instanceID), trafficResetLockOptions, func() (T, error) {
		sample, err := trafficGetResetSample(ctx, instanceID)
		if err != nil {
			return zero, err
		}
		inner, err := trafficWithLock(ctx, trafficUserLockKey(sample.UserID), trafficResetLockOptions, func() (T, error) {
			return trafficRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (T, error) {
				return operation(txCtx, tx, sample.TrafficSnapshotSample)
			})
		})
		if err != nil {
			// Mirrors `throw new Error(userLockResult.error || 用户 X 流量锁获取失败)`.
			return zero, err
		}
		return inner, nil
	})
	if err != nil {
		// Mirrors `throw new Error(result.error || 实例 X 流量重置锁获取失败)`.
		return zero, err
	}
	return result, nil
}

// ==================== snapshots ====================

// GetTrafficSnapshot mirrors getTrafficSnapshot.
func GetTrafficSnapshot(ctx context.Context, instanceID int32) (*models.TrafficSnapshot, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var snapshot models.TrafficSnapshot
	err := Pool().QueryRow(ctx, `
		SELECT id, instance_id, rx_raw, tx_raw, rx_packets_raw, tx_packets_raw, cpu_usage_raw, updated_at
		FROM traffic_snapshots WHERE instance_id = $1`, instanceID,
	).Scan(&snapshot.ID, &snapshot.InstanceID, &snapshot.RxRaw, &snapshot.TxRaw,
		&snapshot.RxPacketsRaw, &snapshot.TxPacketsRaw, &snapshot.CPUUsageRaw, &snapshot.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// UpsertTrafficSnapshot mirrors upsertTrafficSnapshot.
func UpsertTrafficSnapshot(ctx context.Context, instanceID int32, rxRaw, txRaw int64) (models.TrafficSnapshot, error) {
	if Pool() == nil {
		return models.TrafficSnapshot{}, errors.New("database not initialized")
	}
	var snapshot models.TrafficSnapshot
	err := Pool().QueryRow(ctx, `
		INSERT INTO traffic_snapshots (instance_id, rx_raw, tx_raw, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (instance_id) DO UPDATE
		SET rx_raw = EXCLUDED.rx_raw, tx_raw = EXCLUDED.tx_raw, updated_at = NOW()
		RETURNING id, instance_id, rx_raw, tx_raw, rx_packets_raw, tx_packets_raw, cpu_usage_raw, updated_at`,
		instanceID, rxRaw, txRaw,
	).Scan(&snapshot.ID, &snapshot.InstanceID, &snapshot.RxRaw, &snapshot.TxRaw,
		&snapshot.RxPacketsRaw, &snapshot.TxPacketsRaw, &snapshot.CPUUsageRaw, &snapshot.UpdatedAt)
	if err != nil {
		return models.TrafficSnapshot{}, err
	}
	return snapshot, nil
}

// TrafficSnapshotUpsert mirrors one entry of bulkUpsertSnapshots.
type TrafficSnapshotUpsert struct {
	InstanceID int32
	RxRaw      int64
	TxRaw      int64
}

// BulkUpsertSnapshots mirrors bulkUpsertSnapshots: INSERT ... ON CONFLICT in
// batches of trafficBatchSize, so a 10k-instance sweep never builds one giant
// statement.
func BulkUpsertSnapshots(ctx context.Context, snapshots []TrafficSnapshotUpsert) error {
	if len(snapshots) == 0 {
		return nil
	}
	if Pool() == nil {
		return errors.New("database not initialized")
	}

	for start := 0; start < len(snapshots); start += trafficBatchSize {
		end := start + trafficBatchSize
		if end > len(snapshots) {
			end = len(snapshots)
		}
		batch := snapshots[start:end]

		args := make([]any, 0, len(batch)*3)
		placeholders := make([]string, 0, len(batch))
		for _, snapshot := range batch {
			if _, err := trafficSanitizeBigInt(int64(snapshot.InstanceID)); err != nil {
				return err
			}
			rx, err := trafficSanitizeBigInt(snapshot.RxRaw)
			if err != nil {
				return err
			}
			tx, err := trafficSanitizeBigInt(snapshot.TxRaw)
			if err != nil {
				return err
			}
			placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d, NOW())",
				len(args)+1, len(args)+2, len(args)+3))
			args = append(args, snapshot.InstanceID, rx, tx)
		}

		query := `INSERT INTO traffic_snapshots (instance_id, rx_raw, tx_raw, updated_at) VALUES ` +
			strings.Join(placeholders, ", ") + `
			ON CONFLICT (instance_id) DO UPDATE
			SET rx_raw = EXCLUDED.rx_raw, tx_raw = EXCLUDED.tx_raw, updated_at = NOW()`
		if _, err := Pool().Exec(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// ==================== daily aggregation ====================

// UpsertDailyTraffic mirrors upsertDailyTraffic.
func UpsertDailyTraffic(
	ctx context.Context, instanceID int32, date time.Time, rxIncrement, txIncrement int64,
) (models.DailyTraffic, error) {
	if Pool() == nil {
		return models.DailyTraffic{}, errors.New("database not initialized")
	}
	normalizedDate := ShanghaiDateOnly(date, 0)

	var existingID int32
	var rxTotal, txTotal int64
	err := Pool().QueryRow(ctx,
		`SELECT id, rx_total, tx_total FROM daily_traffic WHERE instance_id = $1 AND date = $2::date`,
		instanceID, normalizedDate).Scan(&existingID, &rxTotal, &txTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		var created models.DailyTraffic
		err = Pool().QueryRow(ctx, `
			INSERT INTO daily_traffic (instance_id, date, rx_total, tx_total)
			VALUES ($1, $2::date, $3, $4)
			RETURNING id, instance_id, date, rx_total, tx_total`,
			instanceID, normalizedDate, rxIncrement, txIncrement,
		).Scan(&created.ID, &created.InstanceID, &created.Date, &created.RxTotal, &created.TxTotal)
		if err != nil {
			return models.DailyTraffic{}, err
		}
		return created, nil
	}
	if err != nil {
		return models.DailyTraffic{}, err
	}

	var updated models.DailyTraffic
	err = Pool().QueryRow(ctx, `
		UPDATE daily_traffic SET rx_total = rx_total + $2, tx_total = tx_total + $3
		WHERE id = $1
		RETURNING id, instance_id, date, rx_total, tx_total`,
		existingID, rxIncrement, txIncrement,
	).Scan(&updated.ID, &updated.InstanceID, &updated.Date, &updated.RxTotal, &updated.TxTotal)
	if err != nil {
		return models.DailyTraffic{}, err
	}
	return updated, nil
}

// TrafficDailyUpsert mirrors one entry of bulkUpsertDailyTraffic.
type TrafficDailyUpsert struct {
	InstanceID  int32
	Date        time.Time
	RxIncrement int64
	TxIncrement int64
}

// BulkUpsertDailyTraffic mirrors bulkUpsertDailyTraffic: normalize to the
// Shanghai calendar day, merge duplicate (instance, day) entries, then upsert in
// batches and ADD to the stored totals.
func BulkUpsertDailyTraffic(ctx context.Context, updates []TrafficDailyUpsert) error {
	if len(updates) == 0 {
		return nil
	}
	if Pool() == nil {
		return errors.New("database not initialized")
	}

	type dailyKey struct {
		instanceID int32
		date       time.Time
	}
	merged := make(map[dailyKey]*models.DailyTraffic, len(updates))
	order := make([]dailyKey, 0, len(updates))
	for _, update := range updates {
		key := dailyKey{instanceID: update.InstanceID, date: ShanghaiDateOnly(update.Date, 0)}
		existing, ok := merged[key]
		if ok {
			existing.RxTotal += update.RxIncrement
			existing.TxTotal += update.TxIncrement
			continue
		}
		merged[key] = &models.DailyTraffic{
			InstanceID: update.InstanceID,
			Date:       key.date,
			RxTotal:    update.RxIncrement,
			TxTotal:    update.TxIncrement,
		}
		order = append(order, key)
	}

	for start := 0; start < len(order); start += trafficBatchSize {
		end := start + trafficBatchSize
		if end > len(order) {
			end = len(order)
		}
		batch := order[start:end]

		args := make([]any, 0, len(batch)*4)
		placeholders := make([]string, 0, len(batch))
		for _, key := range batch {
			row := merged[key]
			if _, err := trafficSanitizeBigInt(int64(row.InstanceID)); err != nil {
				return err
			}
			rxTotal, err := trafficSanitizeBigInt(row.RxTotal)
			if err != nil {
				return err
			}
			txTotal, err := trafficSanitizeBigInt(row.TxTotal)
			if err != nil {
				return err
			}
			dateText, err := trafficSanitizeDate(row.Date)
			if err != nil {
				return err
			}
			placeholders = append(placeholders, fmt.Sprintf("($%d, $%d::date, $%d, $%d)",
				len(args)+1, len(args)+2, len(args)+3, len(args)+4))
			args = append(args, row.InstanceID, dateText, rxTotal, txTotal)
		}

		query := `INSERT INTO daily_traffic (instance_id, date, rx_total, tx_total) VALUES ` +
			strings.Join(placeholders, ", ") + `
			ON CONFLICT (instance_id, date) DO UPDATE
			SET rx_total = daily_traffic.rx_total + EXCLUDED.rx_total,
			    tx_total = daily_traffic.tx_total + EXCLUDED.tx_total`
		if _, err := Pool().Exec(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// GetDailyTraffic mirrors getDailyTraffic: records on or after the Shanghai
// calendar day `days` ago, oldest first. The TypeScript's `days = 30` default only
// applies to an `undefined` argument, so callers here pass it explicitly.
func GetDailyTraffic(ctx context.Context, instanceID int32, days int) ([]models.DailyTraffic, error) {
	startDate := ShanghaiDateOnly(time.Now(), -days)
	return trafficDailyRowsBetween(ctx, instanceID, &startDate)
}

// GetDailyTrafficByPeriod mirrors getDailyTrafficByPeriod.
//
// Faithful quirk: Prisma serializes an @db.Date parameter by taking the UTC date
// part of the instant (adapter-pg's formatDate → getUTCFullYear/Month/Date).
// periodStart comes from getTrafficPeriod, which builds LOCAL midnight, so in a
// UTC+8 process the date actually compared is the *previous* calendar day. The
// history chart has therefore always started one day early; reproducing that is
// required for byte parity.
func GetDailyTrafficByPeriod(ctx context.Context, instanceID int32, periodStart time.Time) ([]models.DailyTraffic, error) {
	return trafficDailyRowsBetween(ctx, instanceID, trafficPrismaDateParam(periodStart))
}

// trafficPrismaDateParam mirrors how Prisma binds a JS Date to an @db.Date
// comparison: the UTC calendar date of the instant, rendered as a date-only value.
func trafficPrismaDateParam(instant time.Time) *time.Time {
	utc := instant.UTC()
	start := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return &start
}

// trafficDailyRowsBetween runs the shared `date >= start` ordered read.
func trafficDailyRowsBetween(ctx context.Context, instanceID int32, start *time.Time) ([]models.DailyTraffic, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx, `
		SELECT id, instance_id, date, rx_total, tx_total
		FROM daily_traffic
		WHERE instance_id = $1 AND date >= $2::date
		ORDER BY date ASC`, instanceID, *start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]models.DailyTraffic, 0, 32)
	for rows.Next() {
		var record models.DailyTraffic
		if err := rows.Scan(&record.ID, &record.InstanceID, &record.Date, &record.RxTotal, &record.TxTotal); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// HostDailyTrafficRow mirrors one element of getHostDailyTraffic's projection.
type HostDailyTrafficRow struct {
	Date    time.Time
	RxTotal int64
	TxTotal int64
}

// GetHostDailyTraffic mirrors getHostDailyTraffic: aggregate every non-deleted
// instance on the host per calendar day, from the host's reset day.
func GetHostDailyTraffic(ctx context.Context, hostID int32, trafficResetDay int) ([]HostDailyTrafficRow, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}

	day := trafficResetDay
	if day < 1 {
		day = 1
	}
	if day > 28 {
		day = 28
	}

	year, month, currentDay := ShanghaiDateParts(time.Now())
	var periodStart time.Time
	if currentDay >= day {
		periodStart = time.Date(year, time.Month(month+1), day, 0, 0, 0, 0, time.UTC)
	} else {
		periodStart = time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	}

	rows, err := Pool().Query(ctx, `
		SELECT dt.date,
		       COALESCE(SUM(dt.rx_total), 0)::bigint AS rx_total,
		       COALESCE(SUM(dt.tx_total), 0)::bigint AS tx_total
		FROM daily_traffic dt
		INNER JOIN instances i ON dt.instance_id = i.id
		WHERE i.host_id = $1
		  AND i.status != 'deleted'
		  AND dt.date >= $2::date
		GROUP BY dt.date
		ORDER BY dt.date ASC`, hostID, periodStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]HostDailyTrafficRow, 0, 32)
	for rows.Next() {
		var row HostDailyTrafficRow
		if err := rows.Scan(&row.Date, &row.RxTotal, &row.TxTotal); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// HostTrafficSummary mirrors getHostTrafficSummary's `{ totalUsed, totalLimit }`.
type HostTrafficSummary struct {
	TotalUsed  int64
	TotalLimit int64
}

// GetHostTrafficSummary mirrors getHostTrafficSummary (SQL SUM, NULL → 0).
func GetHostTrafficSummary(ctx context.Context, hostID int32) (HostTrafficSummary, error) {
	if Pool() == nil {
		return HostTrafficSummary{}, errors.New("database not initialized")
	}
	var summary HostTrafficSummary
	err := Pool().QueryRow(ctx, `
		SELECT COALESCE(SUM(monthly_traffic_used), 0)::bigint,
		       COALESCE(SUM(monthly_traffic_limit), 0)::bigint
		FROM instances
		WHERE host_id = $1 AND status <> 'deleted'`, hostID,
	).Scan(&summary.TotalUsed, &summary.TotalLimit)
	if err != nil {
		return HostTrafficSummary{}, err
	}
	return summary, nil
}

// CleanOldDailyTraffic mirrors cleanOldDailyTraffic. The TypeScript's
// `retentionDays = 30` default only applies to an `undefined` argument, so callers
// here pass it explicitly (the scheduler uses 35).
func CleanOldDailyTraffic(ctx context.Context, retentionDays int) (int64, error) {
	if Pool() == nil {
		return 0, errors.New("database not initialized")
	}
	cutoffDate := ShanghaiDateOnly(time.Now(), -retentionDays)
	tag, err := Pool().Exec(ctx, `DELETE FROM daily_traffic WHERE date < $1::date`, cutoffDate)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ==================== usage increments ====================

// IncrementInstanceTrafficUsage mirrors incrementInstanceTrafficUsage.
func IncrementInstanceTrafficUsage(ctx context.Context, instanceID int32, delta int64) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `
		UPDATE instances SET monthly_traffic_used = monthly_traffic_used + $2, updated_at = NOW()
		WHERE id = $1`, instanceID, delta)
}

// TrafficInstanceDelta mirrors one entry of bulkIncrementInstanceTrafficUsage.
type TrafficInstanceDelta struct {
	InstanceID int32
	Delta      int64
}

// BulkIncrementInstanceTrafficUsage mirrors bulkIncrementInstanceTrafficUsage.
func BulkIncrementInstanceTrafficUsage(ctx context.Context, updates []TrafficInstanceDelta) error {
	if len(updates) == 0 {
		return nil
	}
	if Pool() == nil {
		return errors.New("database not initialized")
	}

	deltas := make(map[int32]int64, len(updates))
	order := make([]int32, 0, len(updates))
	for _, update := range updates {
		current, ok := deltas[update.InstanceID]
		if !ok {
			order = append(order, update.InstanceID)
		}
		deltas[update.InstanceID] = current + update.Delta
	}

	for start := 0; start < len(order); start += trafficBatchSize {
		end := start + trafficBatchSize
		if end > len(order) {
			end = len(order)
		}
		batch := order[start:end]

		args := make([]any, 0, len(batch)*2)
		placeholders := make([]string, 0, len(batch))
		for _, instanceID := range batch {
			delta, err := trafficSanitizeBigInt(deltas[instanceID])
			if err != nil {
				return err
			}
			placeholders = append(placeholders, fmt.Sprintf("($%d, $%d)", len(args)+1, len(args)+2))
			args = append(args, instanceID, delta)
		}

		// Faithful to the TypeScript raw SQL: this bulk path does NOT touch
		// updated_at (unlike prisma.instance.update).
		query := `UPDATE instances AS i
			SET monthly_traffic_used = i.monthly_traffic_used + v.delta::bigint
			FROM (VALUES ` + strings.Join(placeholders, ", ") + `) AS v(id, delta)
			WHERE i.id = v.id`
		if _, err := Pool().Exec(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// IncrementUserTrafficUsage mirrors incrementUserTrafficUsage.
func IncrementUserTrafficUsage(ctx context.Context, userID int32, delta int64) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `
		UPDATE user_quotas SET monthly_traffic_used = monthly_traffic_used + $2
		WHERE user_id = $1`, userID, delta)
}

// trafficSyncUserExtraTrafficUsedInTransaction mirrors
// syncUserExtraTrafficUsedInTransaction: each instance's usage above its own base
// limit consumes the user's extra traffic quota, capped at that quota.
func trafficSyncUserExtraTrafficUsedInTransaction(ctx context.Context, tx pgx.Tx, userID int32) (int64, error) {
	var extraTrafficQuota int64
	err := tx.QueryRow(ctx, `SELECT extra_traffic_quota FROM user_quotas WHERE user_id = $1`, userID).
		Scan(&extraTrafficQuota)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx, `
		SELECT monthly_traffic_limit, monthly_traffic_used
		FROM instances
		WHERE user_id = $1 AND status <> 'deleted'`, userID)
	if err != nil {
		return 0, err
	}
	var consumed int64
	for rows.Next() {
		var limit *int64
		var used int64
		if err := rows.Scan(&limit, &used); err != nil {
			rows.Close()
			return 0, err
		}
		if limit == nil || used <= *limit {
			continue
		}
		consumed += used - *limit
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	extraTrafficUsed := consumed
	if extraTrafficUsed > extraTrafficQuota {
		extraTrafficUsed = extraTrafficQuota
	}

	tag, err := tx.Exec(ctx, `UPDATE user_quotas SET extra_traffic_used = $2 WHERE user_id = $1`,
		userID, extraTrafficUsed)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, errTrafficRecordNotFound
	}
	return extraTrafficUsed, nil
}

// SyncUserExtraTrafficUsed mirrors syncUserExtraTrafficUsed: serialized by the
// per-user traffic lock.
func SyncUserExtraTrafficUsed(ctx context.Context, userID int32) (int64, error) {
	return trafficWithLock(ctx, trafficUserLockKey(userID), trafficResetLockOptions, func() (int64, error) {
		return trafficRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (int64, error) {
			return trafficSyncUserExtraTrafficUsedInTransaction(txCtx, tx, userID)
		})
	})
}

// TrafficUserDelta mirrors one entry of bulkIncrementUserTrafficUsage.
type TrafficUserDelta struct {
	UserID int32
	Delta  int64
}

// BulkIncrementUserTrafficUsage mirrors bulkIncrementUserTrafficUsage.
func BulkIncrementUserTrafficUsage(ctx context.Context, updates []TrafficUserDelta) error {
	if len(updates) == 0 {
		return nil
	}
	if Pool() == nil {
		return errors.New("database not initialized")
	}

	deltas := make(map[int32]int64, len(updates))
	order := make([]int32, 0, len(updates))
	for _, update := range updates {
		current, ok := deltas[update.UserID]
		if !ok {
			order = append(order, update.UserID)
		}
		deltas[update.UserID] = current + update.Delta
	}

	for start := 0; start < len(order); start += trafficBatchSize {
		end := start + trafficBatchSize
		if end > len(order) {
			end = len(order)
		}
		batch := order[start:end]

		args := make([]any, 0, len(batch)*2)
		placeholders := make([]string, 0, len(batch))
		for _, userID := range batch {
			delta, err := trafficSanitizeBigInt(deltas[userID])
			if err != nil {
				return err
			}
			placeholders = append(placeholders, fmt.Sprintf("($%d, $%d)", len(args)+1, len(args)+2))
			args = append(args, userID, delta)
		}

		query := `UPDATE user_quotas AS uq
			SET monthly_traffic_used = uq.monthly_traffic_used + v.delta::bigint
			FROM (VALUES ` + strings.Join(placeholders, ", ") + `) AS v(user_id, delta)
			WHERE uq.user_id = v.user_id`
		if _, err := Pool().Exec(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// ==================== status updates ====================

// UpdateInstanceTrafficStatus mirrors updateInstanceTrafficStatus.
func UpdateInstanceTrafficStatus(ctx context.Context, instanceID int32, status models.TrafficStatus) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `
		UPDATE instances SET traffic_status = $2, updated_at = NOW() WHERE id = $1`,
		instanceID, string(status))
}

// MarkInstanceTrafficLimitedIfNeeded mirrors markInstanceTrafficLimitedIfNeeded:
// only a transition out of a non-LIMITED state wins the notification claim.
func MarkInstanceTrafficLimitedIfNeeded(ctx context.Context, instanceID int32) (bool, error) {
	if Pool() == nil {
		return false, errors.New("database not initialized")
	}
	tag, err := Pool().Exec(ctx, `
		UPDATE instances SET traffic_status = 'LIMITED', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted' AND traffic_status <> 'LIMITED'`, instanceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateUserTrafficStatus mirrors updateUserTrafficStatus.
func UpdateUserTrafficStatus(ctx context.Context, userID int32, status models.TrafficStatus) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `UPDATE user_quotas SET traffic_status = $2 WHERE user_id = $1`,
		userID, string(status))
}

// UpdateUserTrafficWarningSentAt mirrors updateUserTrafficWarningSentAt.
func UpdateUserTrafficWarningSentAt(ctx context.Context, userID int32, sentAt time.Time) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `UPDATE user_quotas SET traffic_warning_sent_at = $2 WHERE user_id = $1`,
		userID, sentAt)
}

// MarkUserTrafficWarningIfNeeded mirrors markUserTrafficWarningIfNeeded: the
// conditional UPDATE is the claim that lets exactly one replica send the warning.
func MarkUserTrafficWarningIfNeeded(ctx context.Context, userID int32, sentAt time.Time) (bool, error) {
	if Pool() == nil {
		return false, errors.New("database not initialized")
	}
	currentMonthStart := StartOfMonthShanghai(sentAt)
	tag, err := Pool().Exec(ctx, `
		UPDATE user_quotas SET traffic_status = 'WARNING', traffic_warning_sent_at = $2
		WHERE user_id = $1
		  AND (traffic_warning_sent_at IS NULL OR traffic_warning_sent_at < $3)`,
		userID, sentAt, currentMonthStart)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ==================== monthly resets ====================

// ResetAllInstanceMonthlyTraffic mirrors resetAllInstanceMonthlyTraffic.
func ResetAllInstanceMonthlyTraffic(ctx context.Context) (int64, error) {
	instanceIDs, err := trafficInstanceIDs(ctx, `WHERE status <> 'deleted'`)
	if err != nil {
		return 0, err
	}
	var count int64
	for _, instanceID := range instanceIDs {
		reset, err := ResetInstanceMonthlyTraffic(ctx, instanceID)
		if err != nil {
			return 0, err
		}
		count += reset
	}
	return count, nil
}

// ResetInstanceMonthlyTraffic mirrors resetInstanceMonthlyTraffic: instance lock,
// sample, user lock, transaction. The user quota is reduced by the instance's
// contribution rather than zeroed, so a user's other instances keep their usage.
func ResetInstanceMonthlyTraffic(ctx context.Context, instanceID int32) (int64, error) {
	result, err := trafficWithLock(ctx, trafficInstanceLockKey(instanceID), trafficResetLockOptions, func() (int64, error) {
		sample, err := trafficGetResetSample(ctx, instanceID)
		if err != nil {
			return 0, err
		}
		return trafficWithLock(ctx, trafficUserLockKey(sample.UserID), trafficResetLockOptions, func() (int64, error) {
			return trafficRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (int64, error) {
				return trafficResetInstanceInTransaction(txCtx, tx, instanceID, sample.TrafficSnapshotSample)
			})
		})
	})
	if err != nil {
		return 0, err
	}
	return result, nil
}

// trafficResetInstanceInTransaction is the body of resetInstanceMonthlyTraffic's
// transaction.
func trafficResetInstanceInTransaction(
	ctx context.Context, tx pgx.Tx, instanceID int32, sample TrafficSnapshotSample,
) (int64, error) {
	var (
		currentUserID int32
		status        string
		monthlyUsed   int64
	)
	err := tx.QueryRow(ctx, `
		SELECT user_id, status::text, monthly_traffic_used FROM instances WHERE id = $1`, instanceID,
	).Scan(&currentUserID, &status, &monthlyUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("实例 %d 不存在", instanceID)
	}
	if err != nil {
		return 0, err
	}
	if status == string(models.InstanceStatusDeleted) {
		return 0, fmt.Errorf("实例 %d 不存在", instanceID)
	}

	var (
		quotaLimit   *int64
		quotaUsed    int64
		hasUserQuota bool
	)
	err = tx.QueryRow(ctx,
		`SELECT monthly_traffic_limit, monthly_traffic_used FROM user_quotas WHERE user_id = $1`,
		currentUserID).Scan(&quotaLimit, &quotaUsed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		hasUserQuota = false
	case err != nil:
		return 0, err
	default:
		hasUserQuota = true
	}

	if _, err := AdvanceTrafficSnapshot(ctx, tx, instanceID, sample); err != nil {
		return 0, err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE instances SET monthly_traffic_used = 0, traffic_status = 'NORMAL', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'`, instanceID)
	if err != nil {
		return 0, err
	}
	updated := tag.RowsAffected()

	if updated > 0 && hasUserQuota {
		nextUsed := int64(0)
		if quotaUsed > monthlyUsed {
			nextUsed = quotaUsed - monthlyUsed
		}
		effectiveLimit := trafficGetEffectiveLimit(quotaLimit, 0)
		nextStatus := trafficCalculateUserStatus(nextUsed, effectiveLimit)
		tag, err := tx.Exec(ctx, `
			UPDATE user_quotas
			SET monthly_traffic_used = $2, extra_traffic_quota = 0, extra_traffic_used = 0,
			    traffic_status = $3
			WHERE user_id = $1`,
			currentUserID, nextUsed, string(nextStatus))
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, errTrafficRecordNotFound
		}
	}

	return updated, nil
}

// UpdateInstanceBandwidthLimits mirrors updateInstanceBandwidthLimits.
func UpdateInstanceBandwidthLimits(ctx context.Context, instanceID int32, limitsIngress, limitsEgress *string) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `
		UPDATE instances SET limits_ingress = $2, limits_egress = $3, updated_at = NOW()
		WHERE id = $1`, instanceID, limitsIngress, limitsEgress)
}

// ResetHostInstancesMonthlyTraffic mirrors resetHostInstancesMonthlyTraffic.
func ResetHostInstancesMonthlyTraffic(ctx context.Context, hostIDs []int32) (int64, error) {
	if len(hostIDs) == 0 {
		return 0, nil
	}
	if Pool() == nil {
		return 0, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx,
		`SELECT id FROM instances WHERE host_id = ANY($1) AND status <> 'deleted'`, hostIDs)
	if err != nil {
		return 0, err
	}
	var instanceIDs []int32
	for rows.Next() {
		var instanceID int32
		if err := rows.Scan(&instanceID); err != nil {
			rows.Close()
			return 0, err
		}
		instanceIDs = append(instanceIDs, instanceID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	var count int64
	for _, instanceID := range instanceIDs {
		reset, err := ResetInstanceMonthlyTraffic(ctx, instanceID)
		if err != nil {
			return 0, err
		}
		count += reset
	}
	return count, nil
}

// ==================== running instances ====================

// RunningTrafficHost mirrors the host projection of getRunningInstancesForTraffic.
type RunningTrafficHost struct {
	ID       int32
	Name     string
	URL      string
	CertPath *string
	KeyPath  *string
	Status   models.HostStatus
}

// RunningTrafficUser mirrors `user: { select: { id, quota } }`.
type RunningTrafficUser struct {
	ID    int32
	Quota *models.UserQuota
}

// RunningTrafficPackage mirrors the package projection.
type RunningTrafficPackage struct {
	BandwidthMax  *int32
	LimitsIngress *string
	LimitsEgress  *string
}

// RunningTrafficPackagePlan mirrors the packagePlan projection.
type RunningTrafficPackagePlan struct {
	TrafficLimitSpeed string
}

// RunningTrafficInstance mirrors one element of getRunningInstancesForTraffic. Only
// the columns services/traffic-scheduler.ts reads are projected (the TypeScript
// returns the whole Instance row, but no other consumer exists).
type RunningTrafficInstance struct {
	ID                  int32
	IncusID             string
	Name                string
	UserID              int32
	HostID              int32
	MonthlyTrafficLimit *int64
	MonthlyTrafficUsed  int64
	TrafficStatus       models.TrafficStatus
	Host                *RunningTrafficHost
	User                *RunningTrafficUser
	Package             *RunningTrafficPackage
	PackagePlan         *RunningTrafficPackagePlan
	TrafficSnapshot     *models.TrafficSnapshot
}

// trafficRunningInstanceColumns is the shared projection for the three
// getRunningInstancesForTraffic variants.
const trafficRunningInstanceColumns = `
	i.id, i.incus_id, i.name, i.user_id, i.host_id,
	i.monthly_traffic_limit, i.monthly_traffic_used, i.traffic_status::text,
	h.id, h.name, h.url, h.cert_path, h.key_path, h.status::text,
	u.id,
	q.id, q.user_id, q.host_limit, q.host_used, q.friend_limit, q.friend_used,
	q.package_limit, q.package_used, q.monthly_traffic_limit, q.monthly_traffic_used,
	q.traffic_status::text, q.traffic_warning_sent_at, q.extra_traffic_quota, q.extra_traffic_used,
	p.bandwidth_max, p.limits_ingress, p.limits_egress,
	pp.traffic_limit_speed,
	ts.id, ts.instance_id, ts.rx_raw, ts.tx_raw, ts.rx_packets_raw, ts.tx_packets_raw,
	ts.cpu_usage_raw, ts.updated_at`

const trafficRunningInstanceFrom = `
	FROM instances i
	LEFT JOIN hosts h ON h.id = i.host_id
	LEFT JOIN users u ON u.id = i.user_id
	LEFT JOIN user_quotas q ON q.user_id = i.user_id
	LEFT JOIN packages p ON p.id = i.package_id
	LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
	LEFT JOIN traffic_snapshots ts ON ts.instance_id = i.id`

// GetRunningInstancesForTraffic mirrors getRunningInstancesForTraffic.
func GetRunningInstancesForTraffic(ctx context.Context) ([]RunningTrafficInstance, error) {
	return trafficQueryRunningInstances(ctx, `WHERE i.status = 'running'`)
}

// GetRunningInstancesForTrafficByIDs mirrors getRunningInstancesForTrafficByIds.
func GetRunningInstancesForTrafficByIDs(ctx context.Context, instanceIDs []int32) ([]RunningTrafficInstance, error) {
	if len(instanceIDs) == 0 {
		return []RunningTrafficInstance{}, nil
	}
	return trafficQueryRunningInstances(ctx, `WHERE i.id = ANY($1) AND i.status = 'running'`, instanceIDs)
}

// GetRunningInstancesForTrafficByUserID mirrors
// getRunningInstancesForTrafficByUserId.
func GetRunningInstancesForTrafficByUserID(ctx context.Context, userID int32) ([]RunningTrafficInstance, error) {
	return trafficQueryRunningInstances(ctx, `WHERE i.user_id = $1 AND i.status = 'running'`, userID)
}

// trafficQueryRunningInstances scans the shared projection.
func trafficQueryRunningInstances(ctx context.Context, where string, args ...any) ([]RunningTrafficInstance, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx, `SELECT `+trafficRunningInstanceColumns+trafficRunningInstanceFrom+` `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]RunningTrafficInstance, 0, 16)
	for rows.Next() {
		var (
			instance RunningTrafficInstance
			status   string

			hostID     *int32
			hostName   *string
			hostURL    *string
			hostCert   *string
			hostKey    *string
			hostStatus *string

			userID *int32

			quotaID                *int32
			quotaUserID            *int32
			quotaHostLimit         *int32
			quotaHostUsed          *int32
			quotaFriendLimit       *int32
			quotaFriendUsed        *int32
			quotaPackageLimit      *int32
			quotaPackageUsed       *int32
			quotaMonthlyLimit      *int64
			quotaMonthlyUsed       *int64
			quotaTrafficStatus     *string
			quotaWarningSentAt     *time.Time
			quotaExtraTrafficQuota *int64
			quotaExtraTrafficUsed  *int64

			packageBandwidthMax  *int32
			packageLimitsIngress *string
			packageLimitsEgress  *string

			planTrafficLimitSpeed *string

			snapshotID           *int32
			snapshotInstanceID   *int32
			snapshotRxRaw        *int64
			snapshotTxRaw        *int64
			snapshotRxPacketsRaw *int64
			snapshotTxPacketsRaw *int64
			snapshotCPUUsageRaw  *int64
			snapshotUpdatedAt    *time.Time
		)
		err := rows.Scan(
			&instance.ID, &instance.IncusID, &instance.Name, &instance.UserID, &instance.HostID,
			&instance.MonthlyTrafficLimit, &instance.MonthlyTrafficUsed, &status,
			&hostID, &hostName, &hostURL, &hostCert, &hostKey, &hostStatus,
			&userID,
			&quotaID, &quotaUserID, &quotaHostLimit, &quotaHostUsed, &quotaFriendLimit, &quotaFriendUsed,
			&quotaPackageLimit, &quotaPackageUsed, &quotaMonthlyLimit, &quotaMonthlyUsed,
			&quotaTrafficStatus, &quotaWarningSentAt, &quotaExtraTrafficQuota, &quotaExtraTrafficUsed,
			&packageBandwidthMax, &packageLimitsIngress, &packageLimitsEgress,
			&planTrafficLimitSpeed,
			&snapshotID, &snapshotInstanceID, &snapshotRxRaw, &snapshotTxRaw, &snapshotRxPacketsRaw,
			&snapshotTxPacketsRaw, &snapshotCPUUsageRaw, &snapshotUpdatedAt,
		)
		if err != nil {
			return nil, err
		}

		instance.TrafficStatus = models.TrafficStatus(status)

		if hostID != nil {
			host := &RunningTrafficHost{ID: *hostID, CertPath: hostCert, KeyPath: hostKey}
			if hostName != nil {
				host.Name = *hostName
			}
			if hostURL != nil {
				host.URL = *hostURL
			}
			if hostStatus != nil {
				host.Status = models.HostStatus(*hostStatus)
			}
			instance.Host = host
		}

		if userID != nil {
			user := &RunningTrafficUser{ID: *userID}
			if quotaID != nil {
				quota := &models.UserQuota{
					ID:                   *quotaID,
					MonthlyTrafficLimit:  quotaMonthlyLimit,
					ExtraTrafficQuota:    valueOrZero(quotaExtraTrafficQuota),
					ExtraTrafficUsed:     valueOrZero(quotaExtraTrafficUsed),
					TrafficWarningSentAt: quotaWarningSentAt,
				}
				if quotaUserID != nil {
					quota.UserID = *quotaUserID
				}
				quota.HostLimit = valueOrZero(quotaHostLimit)
				quota.HostUsed = valueOrZero(quotaHostUsed)
				quota.FriendLimit = valueOrZero(quotaFriendLimit)
				quota.FriendUsed = valueOrZero(quotaFriendUsed)
				quota.PackageLimit = valueOrZero(quotaPackageLimit)
				quota.PackageUsed = valueOrZero(quotaPackageUsed)
				quota.MonthlyTrafficUsed = valueOrZero(quotaMonthlyUsed)
				if quotaTrafficStatus != nil {
					quota.TrafficStatus = models.TrafficStatus(*quotaTrafficStatus)
				}
				user.Quota = quota
			}
			instance.User = user
		}

		if packageBandwidthMax != nil || packageLimitsIngress != nil || packageLimitsEgress != nil {
			instance.Package = &RunningTrafficPackage{
				BandwidthMax:  packageBandwidthMax,
				LimitsIngress: packageLimitsIngress,
				LimitsEgress:  packageLimitsEgress,
			}
		}

		if planTrafficLimitSpeed != nil {
			instance.PackagePlan = &RunningTrafficPackagePlan{TrafficLimitSpeed: *planTrafficLimitSpeed}
		}

		if snapshotID != nil {
			snapshot := &models.TrafficSnapshot{
				ID:           *snapshotID,
				RxRaw:        valueOrZero(snapshotRxRaw),
				TxRaw:        valueOrZero(snapshotTxRaw),
				RxPacketsRaw: valueOrZero(snapshotRxPacketsRaw),
				TxPacketsRaw: valueOrZero(snapshotTxPacketsRaw),
				CPUUsageRaw:  snapshotCPUUsageRaw,
			}
			if snapshotInstanceID != nil {
				snapshot.InstanceID = *snapshotInstanceID
			}
			if snapshotUpdatedAt != nil {
				snapshot.UpdatedAt = *snapshotUpdatedAt
			}
			instance.TrafficSnapshot = snapshot
		}

		result = append(result, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// valueOrZero dereferences a nullable integer, mirroring Prisma's non-null
// defaults for omitted creates.
func valueOrZero[T int32 | int64](value *T) T {
	if value == nil {
		return 0
	}
	return *value
}

// ==================== queries ====================

// UserTrafficQuota mirrors getUserTrafficQuota's projection.
type UserTrafficQuota struct {
	MonthlyTrafficLimit  *int64
	MonthlyTrafficUsed   int64
	ExtraTrafficQuota    int64
	ExtraTrafficUsed     int64
	TrafficStatus        models.TrafficStatus
	TrafficWarningSentAt *time.Time
}

// GetUserTrafficQuota mirrors getUserTrafficQuota. nil means the user has no
// quota row.
func GetUserTrafficQuota(ctx context.Context, userID int32) (*UserTrafficQuota, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var (
		quota  UserTrafficQuota
		status string
	)
	err := Pool().QueryRow(ctx, `
		SELECT monthly_traffic_limit, monthly_traffic_used, extra_traffic_quota,
		       extra_traffic_used, traffic_status::text, traffic_warning_sent_at
		FROM user_quotas WHERE user_id = $1`, userID,
	).Scan(&quota.MonthlyTrafficLimit, &quota.MonthlyTrafficUsed, &quota.ExtraTrafficQuota,
		&quota.ExtraTrafficUsed, &status, &quota.TrafficWarningSentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	quota.TrafficStatus = models.TrafficStatus(status)
	return &quota, nil
}

// InstanceTrafficInfo mirrors getInstanceTrafficInfo plus the traffic-reset price
// resolution from routes/traffic.ts.
type InstanceTrafficInfo struct {
	ID                  int32
	Name                string
	UserID              int32
	HostID              *int32
	MonthlyTrafficLimit *int64
	MonthlyTrafficUsed  int64
	TrafficStatus       models.TrafficStatus
	// PackagePlanTrafficResetPrice mirrors `packagePlan?.trafficResetPrice`: nil
	// both when there is no plan and when the plan's price is NULL (the
	// TypeScript falls back to the package in both cases).
	PackagePlanTrafficResetPrice *float64
	// PackageTrafficResetPrice mirrors `package?.trafficResetPrice`, with 0 when
	// the package relation is null.
	PackageTrafficResetPrice float64
}

// TrafficResetPriceYuan mirrors resolveTrafficResetPriceYuan from
// routes/traffic.ts.
func (info InstanceTrafficInfo) TrafficResetPriceYuan() float64 {
	if info.PackagePlanTrafficResetPrice != nil {
		return trafficNormalizeMoney(math.Max(0, *info.PackagePlanTrafficResetPrice))
	}
	return trafficNormalizeMoney(math.Max(0, info.PackageTrafficResetPrice))
}

// GetInstanceTrafficInfo mirrors getInstanceTrafficInfo.
func GetInstanceTrafficInfo(ctx context.Context, instanceID int32) (*InstanceTrafficInfo, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var (
		info         InstanceTrafficInfo
		status       string
		planPrice    *string
		packagePrice *string
	)
	err := Pool().QueryRow(ctx, `
		SELECT i.id, i.name, i.user_id, i.host_id, i.monthly_traffic_limit, i.monthly_traffic_used,
		       i.traffic_status::text,
		       pp.traffic_reset_price::text,
		       COALESCE(p.traffic_reset_price, 0)::text
		FROM instances i
		LEFT JOIN packages p ON p.id = i.package_id
		LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		WHERE i.id = $1`, instanceID,
	).Scan(&info.ID, &info.Name, &info.UserID, &info.HostID, &info.MonthlyTrafficLimit,
		&info.MonthlyTrafficUsed, &status, &planPrice, &packagePrice)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info.TrafficStatus = models.TrafficStatus(status)
	if planPrice != nil {
		value := trafficNumberOrZero(planPrice)
		info.PackagePlanTrafficResetPrice = &value
	}
	info.PackageTrafficResetPrice = trafficNumberOrZero(packagePrice)
	return &info, nil
}

// TrafficHost is the getHostById projection routes/traffic.ts reads.
type TrafficHost struct {
	ID              int32
	UserID          int32
	TrafficResetDay int32
}

// TrafficGetHostByID mirrors db.getHostById for the columns the traffic routes read.
func TrafficGetHostByID(ctx context.Context, hostID int32) (*TrafficHost, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var host TrafficHost
	err := Pool().QueryRow(ctx,
		`SELECT id, user_id, traffic_reset_day FROM hosts WHERE id = $1`, hostID,
	).Scan(&host.ID, &host.UserID, &host.TrafficResetDay)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &host, nil
}

// GetUserInstanceIDs mirrors getUserInstanceIds.
func GetUserInstanceIDs(ctx context.Context, userID int32) ([]int32, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx,
		`SELECT id FROM instances WHERE user_id = $1 AND status <> 'deleted'`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]int32, 0, 8)
	for rows.Next() {
		var instanceID int32
		if err := rows.Scan(&instanceID); err != nil {
			return nil, err
		}
		result = append(result, instanceID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// BatchUpdateInstanceTrafficStatus mirrors batchUpdateInstanceTrafficStatus.
func BatchUpdateInstanceTrafficStatus(ctx context.Context, instanceIDs []int32, status models.TrafficStatus) (int64, error) {
	if len(instanceIDs) == 0 {
		return 0, nil
	}
	if Pool() == nil {
		return 0, errors.New("database not initialized")
	}
	tag, err := Pool().Exec(ctx, `
		UPDATE instances SET traffic_status = $2, updated_at = NOW() WHERE id = ANY($1)`,
		instanceIDs, string(status))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UpdateUserTrafficLimit mirrors updateUserTrafficLimit.
func UpdateUserTrafficLimit(ctx context.Context, userID int32, limit *int64) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `UPDATE user_quotas SET monthly_traffic_limit = $2 WHERE user_id = $1`,
		userID, limit)
}

// UpdateInstanceTrafficLimit mirrors updateInstanceTrafficLimit.
func UpdateInstanceTrafficLimit(ctx context.Context, instanceID int32, limit *int64) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	return trafficExecOne(ctx, `
		UPDATE instances SET monthly_traffic_limit = $2, updated_at = NOW() WHERE id = $1`,
		instanceID, limit)
}

// trafficExecOne runs an UPDATE that must affect a row. A missing row is the
// Prisma P2025 error `prisma.update` raises.
func trafficExecOne(ctx context.Context, query string, args ...any) error {
	tag, err := Pool().Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errTrafficRecordNotFound
	}
	return nil
}

// trafficInstanceIDs selects instance ids matching the supplied WHERE clause.
func trafficInstanceIDs(ctx context.Context, where string) ([]int32, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx, `SELECT id FROM instances `+where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]int32, 0, 16)
	for rows.Next() {
		var instanceID int32
		if err := rows.Scan(&instanceID); err != nil {
			return nil, err
		}
		result = append(result, instanceID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ==================== reset transaction for POST /instances/:id/traffic/reset ====

// ResetInstanceTrafficForOwnerInTransaction ports the body of the inline callback
// in routes/traffic.ts POST /instances/:instanceId/traffic/reset. It must run on
// the transaction handed to WithInstanceTrafficResetLock so the two advisory locks
// (4121 instance, then 4110 balance) and the mutations they guard share it.
//
// Error strings are the TypeScript's, because the route forwards message text to
// the client (only 余额不足/正在处理 change the status code).
func ResetInstanceTrafficForOwnerInTransaction(
	ctx context.Context, tx pgx.Tx, instanceID, requestingUserID int32, sample TrafficSnapshotSample,
) (float64, error) {
	locked, err := TryAdvisoryTransactionLock(ctx, tx, InstanceTrafficResetLockNamespace, instanceID)
	if err != nil {
		return 0, err
	}
	if !locked {
		return 0, errors.New("流量重置正在处理，请稍后重试")
	}

	var (
		currentID    int32
		name         string
		userID       int32
		status       string
		monthlyUsed  int64
		planPrice    *string
		packagePrice *string
	)
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.name, i.user_id, i.status::text, i.monthly_traffic_used,
		       pp.traffic_reset_price::text,
		       COALESCE(p.traffic_reset_price, 0)::text
		FROM instances i
		LEFT JOIN packages p ON p.id = i.package_id
		LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		WHERE i.id = $1`, instanceID,
	).Scan(&currentID, &name, &userID, &status, &monthlyUsed, &planPrice, &packagePrice)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("Instance not found")
	}
	if err != nil {
		return 0, err
	}
	if status == string(models.InstanceStatusDeleted) {
		return 0, errors.New("Instance not found")
	}
	if userID != requestingUserID {
		return 0, errors.New("Access denied")
	}

	// resolveTrafficResetPriceYuan: the plan price wins when it is present (null
	// falls through to the package price).
	resetPrice := packagePrice
	if planPrice != nil {
		resetPrice = planPrice
	}
	priceYuan := trafficNormalizeMoney(math.Max(0, trafficNumberOrZero(resetPrice)))

	if priceYuan > 0 {
		balanceLocked, err := TryAdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID)
		if err != nil {
			return 0, err
		}
		if !balanceLocked {
			return 0, errors.New("余额正在处理，请稍后重试")
		}

		// The TypeScript reads user.balance as part of the instance query *before*
		// taking the balance lock. Reading it after the lock (this port) keeps the
		// same lock order while guaranteeing the ledger's balance_before is the
		// balance the UPDATE actually operates on — i.e. no stale ledger row under
		// concurrency, which is the invariant that matters for a monthly reset.
		var balance models.Decimal
		err = tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("用户不存在")
		}
		if err != nil {
			return 0, err
		}

		balanceBefore := decimalToFloat64(balance)
		balanceAfter := trafficNormalizeMoney(balanceBefore - priceYuan)
		if balanceAfter < 0 {
			return 0, errors.New("余额不足")
		}

		priceDecimal := trafficDecimalFromFloat(priceYuan)
		tag, err := tx.Exec(ctx, `
			UPDATE users SET balance = balance - $1, updated_at = NOW()
			WHERE id = $2 AND balance >= $1`, priceDecimal, userID)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, errors.New("余额不足或并发冲突")
		}

		var updatedBalance models.Decimal
		err = tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&updatedBalance)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("用户不存在")
		}
		if err != nil {
			return 0, err
		}

		amount := models.Decimal{Decimal: priceDecimal.Decimal.Neg()}
		remark := "重置实例 " + name + " 月流量"
		if _, err := insertBalanceLog(ctx, tx, balanceLogInsert{
			userID:        userID,
			logType:       models.BalanceLogTypeConsume,
			amount:        amount,
			balanceBefore: balance,
			balanceAfter:  updatedBalance,
			instanceID:    &instanceID,
			remark:        &remark,
		}); err != nil {
			return 0, err
		}
	}

	if _, err := AdvanceTrafficSnapshot(ctx, tx, instanceID, sample); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE instances SET monthly_traffic_used = 0, traffic_status = 'NORMAL', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'`, instanceID); err != nil {
		return 0, err
	}

	var (
		quotaLimit   *int64
		quotaUsed    int64
		hasUserQuota bool
	)
	err = tx.QueryRow(ctx,
		`SELECT monthly_traffic_limit, monthly_traffic_used FROM user_quotas WHERE user_id = $1`, userID).
		Scan(&quotaLimit, &quotaUsed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		hasUserQuota = false
	case err != nil:
		return 0, err
	default:
		hasUserQuota = true
	}

	if hasUserQuota {
		nextUsed := int64(0)
		if quotaUsed > monthlyUsed {
			nextUsed = quotaUsed - monthlyUsed
		}
		effectiveLimit := trafficGetEffectiveLimit(quotaLimit, 0)
		nextStatus := trafficCalculateUserStatus(nextUsed, effectiveLimit)
		tag, err := tx.Exec(ctx, `
			UPDATE user_quotas
			SET monthly_traffic_used = $2, extra_traffic_quota = 0, extra_traffic_used = 0,
			    traffic_status = $3
			WHERE user_id = $1`, userID, nextUsed, string(nextStatus))
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, errTrafficRecordNotFound
		}
	}

	return priceYuan, nil
}

// ==================== package traffic multipliers ====================

// trafficMultiplierScale mirrors MULTIPLIER_SCALE.
const trafficMultiplierScale = int64(1000)

// NormalizeTrafficMultiplier mirrors normalizeTrafficMultiplier from
// server/src/lib/traffic-multiplier.ts (duplicated here for the db↛lib rule).
func NormalizeTrafficMultiplier(value any) (float64, error) {
	switch typed := value.(type) {
	case nil:
		return 1, nil
	case string:
		if typed == "" {
			return 1, nil
		}
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0.001 || parsed > 100 {
			return 0, errors.New("Traffic multiplier must be at least 0.001 and no more than 100")
		}
		return math.Round(parsed*float64(trafficMultiplierScale)) / float64(trafficMultiplierScale), nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < 0.001 || typed > 100 {
			return 0, errors.New("Traffic multiplier must be at least 0.001 and no more than 100")
		}
		return math.Round(typed*float64(trafficMultiplierScale)) / float64(trafficMultiplierScale), nil
	case models.Decimal:
		return NormalizeTrafficMultiplier(decimalToFloat64(typed))
	case *models.Decimal:
		if typed == nil {
			return 1, nil
		}
		return NormalizeTrafficMultiplier(decimalToFloat64(*typed))
	default:
		return 0, errors.New("Traffic multiplier must be at least 0.001 and no more than 100")
	}
}

// ApplyTrafficMultiplier mirrors applyTrafficMultiplier from
// server/src/lib/traffic-multiplier.ts: `(limit * scaled) / 1000` with bigint
// truncation toward zero. A nil limit stays unlimited.
func ApplyTrafficMultiplier(limit *int64, multiplier any) (*int64, error) {
	if limit == nil {
		return nil, nil
	}
	normalized, err := NormalizeTrafficMultiplier(multiplier)
	if err != nil {
		return nil, err
	}
	scaled := int64(math.Round(normalized * float64(trafficMultiplierScale)))

	product := new(big.Int).Mul(big.NewInt(*limit), big.NewInt(scaled))
	quotient := new(big.Int).Quo(product, big.NewInt(trafficMultiplierScale))
	if !quotient.IsInt64() {
		return nil, errors.New("Traffic multiplier result out of range")
	}
	result := quotient.Int64()
	return &result, nil
}

// SyncPackageTrafficLimitsResult mirrors
// syncPackageTrafficLimitsToInstances's `{ count, instanceIds }`.
type SyncPackageTrafficLimitsResult struct {
	Count       int
	InstanceIDs []int32
}

// SyncPackageTrafficLimitsToInstances mirrors
// syncPackageTrafficLimitsToInstances: copy each package plan's/pack's traffic
// limit onto its instances, scaled by the package↔host traffic multiplier.
func SyncPackageTrafficLimitsToInstances(ctx context.Context, packageID *int32) (SyncPackageTrafficLimitsResult, error) {
	if Pool() == nil {
		return SyncPackageTrafficLimitsResult{}, errors.New("database not initialized")
	}

	query := `
		SELECT i.id, i.host_id, i.package_id, i.monthly_traffic_used,
		       p.monthly_traffic_limit,
		       pp.traffic_limit
		FROM instances i
		LEFT JOIN packages p ON p.id = i.package_id
		LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		WHERE i.status <> 'deleted' AND i.package_id IS NOT NULL`
	args := []any{}
	if packageID != nil {
		query += ` AND i.package_id = $1`
		args = append(args, *packageID)
	}

	rows, err := Pool().Query(ctx, query, args...)
	if err != nil {
		return SyncPackageTrafficLimitsResult{}, err
	}

	type syncInstance struct {
		id           int32
		hostID       int32
		packageID    int32
		monthlyUsed  int64
		packageLimit *int64
		planLimit    *int64
	}
	instances := make([]syncInstance, 0, 16)
	for rows.Next() {
		var (
			instance   syncInstance
			packageIDV *int32
			packageVal *int64
		)
		if err := rows.Scan(&instance.id, &instance.hostID, &packageIDV, &instance.monthlyUsed,
			&packageVal, &instance.planLimit); err != nil {
			rows.Close()
			return SyncPackageTrafficLimitsResult{}, err
		}
		if packageIDV != nil {
			instance.packageID = *packageIDV
		}
		instance.packageLimit = packageVal
		instances = append(instances, instance)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SyncPackageTrafficLimitsResult{}, err
	}
	rows.Close()

	packageIDSet := make(map[int32]bool, len(instances))
	packageIDs := make([]int32, 0, len(instances))
	for _, instance := range instances {
		if instance.packageID == 0 || packageIDSet[instance.packageID] {
			continue
		}
		packageIDSet[instance.packageID] = true
		packageIDs = append(packageIDs, instance.packageID)
	}
	if len(packageIDs) == 0 {
		return SyncPackageTrafficLimitsResult{Count: 0, InstanceIDs: []int32{}}, nil
	}

	bindingRows, err := Pool().Query(ctx, `
		SELECT package_id, host_id, traffic_multiplier
		FROM package_hosts WHERE package_id = ANY($1)`, packageIDs)
	if err != nil {
		return SyncPackageTrafficLimitsResult{}, err
	}
	type packageHostKey struct {
		packageID int32
		hostID    int32
	}
	multiplierByPackageHost := make(map[packageHostKey]models.Decimal, len(packageIDs))
	for bindingRows.Next() {
		var (
			bindingPackageID int32
			bindingHostID    int32
			multiplier       models.Decimal
		)
		if err := bindingRows.Scan(&bindingPackageID, &bindingHostID, &multiplier); err != nil {
			bindingRows.Close()
			return SyncPackageTrafficLimitsResult{}, err
		}
		multiplierByPackageHost[packageHostKey{packageID: bindingPackageID, hostID: bindingHostID}] = multiplier
	}
	if err := bindingRows.Err(); err != nil {
		bindingRows.Close()
		return SyncPackageTrafficLimitsResult{}, err
	}
	bindingRows.Close()

	updatedInstanceIDs := make([]int32, 0, len(instances))
	for _, instance := range instances {
		baseTrafficLimit := instance.packageLimit
		if instance.planLimit != nil {
			baseTrafficLimit = instance.planLimit
		}

		multiplier := any(float64(1))
		if bound, ok := multiplierByPackageHost[packageHostKey{packageID: instance.packageID, hostID: instance.hostID}]; ok {
			multiplier = bound
		}
		monthlyTrafficLimit, err := ApplyTrafficMultiplier(baseTrafficLimit, multiplier)
		if err != nil {
			return SyncPackageTrafficLimitsResult{}, err
		}

		updatedInstanceIDs = append(updatedInstanceIDs, instance.id)
		nextStatus := trafficCalculateInstanceStatus(instance.monthlyUsed, monthlyTrafficLimit)
		if err := trafficExecOne(ctx, `
			UPDATE instances SET monthly_traffic_limit = $2, traffic_status = $3, updated_at = NOW()
			WHERE id = $1`, instance.id, monthlyTrafficLimit, string(nextStatus)); err != nil {
			return SyncPackageTrafficLimitsResult{}, err
		}
	}

	return SyncPackageTrafficLimitsResult{Count: len(instances), InstanceIDs: updatedInstanceIDs}, nil
}
