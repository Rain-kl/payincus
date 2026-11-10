// Port of server/src/db/transfers.ts (612 lines, 17 exports).
//
// Instance transfers move resource *ownership*, so this file carries three
// invariants from docs/WAVE_D_SPEC.md:
//
//  1. Creation is serialised per instance with the NON-BLOCKING
//     transaction-scoped advisory lock (TRANSFER_CREATE_LOCK_NAMESPACE = 4103,
//     key = instanceId). The TypeScript wraps the whole create-and-charge step in
//     `prisma.$transaction` and retries the transaction when the lock is busy:
//     `TRANSFER_CREATE_LOCK_RETRY_LIMIT = 20` attempts with an *increasing* delay
//     (`100ms * (attempt + 1)`, i.e. 100ms, 200ms, ... 2s). Both numbers are kept
//     as package variables so tests can shorten them; the production defaults are
//     pinned by TestTransferCreateLockRetryDefaults. Replacing this with a
//     blocking `pg_advisory_xact_lock` would change the failure mode (a request
//     would queue instead of answering TRANSFER_ALREADY_PENDING) and is
//     forbidden.
//
//  2. Money (docs/WAVE_D_SPEC.md §0.3): the transfer fee is debited in the SAME
//     transaction as the transfer row, under the user-balance lock (4110), with a
//     `balance_logs` row whose balance_before/balance_after are snapshots. Refunds
//     are their own transaction under the same lock. Amounts are Decimal in SQL
//     and only converted to float64 where the TypeScript does `Number(...)`.
//
//  3. UTC (§0.2b): every timestamp that reaches SQL is time.Now().UTC().
//     `instance_transfers.updated_at` is NOT NULL with no database default
//     (Prisma fills @updatedAt client-side), so the literal INSERT names it.
//
// What the TypeScript deliberately does NOT do, and this port therefore also does
// not do: it does not touch user quotas (the comment in executeTransfer is
// explicit), it does not change expires_at/billing_price, and it does not guard
// executeTransfer against a repeated call — the route's atomic
// pending -> processing claim is the idempotency gate (see
// ClaimTransferForAccept).
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	transferListMaxPageSize   = 100
	transferListSearchMaxLen  = 128
	transferTransactionTimeMs = 10000
)

// Retry schedule for the create lock. Variables (not constants) so tests can run
// the retry loop quickly; the values must stay identical to the TypeScript.
var (
	transferCreateLockRetryLimit = 20
	transferCreateLockRetryDelay = 100 * time.Millisecond
	transferSleep                = time.Sleep
	transferTransactionTimeout   = transferTransactionTimeMs * time.Millisecond
)

// Sentinel errors mirroring the `throw new Error('...')` strings the route
// switches on. Message equality is part of the contract (the TS compares
// error.message), so the strings are the identifiers.
var (
	ErrTransferAlreadyPending      = errors.New("TRANSFER_ALREADY_PENDING")
	ErrTransferBalanceConflict     = errors.New("BALANCE_CONFLICT")
	ErrTransferUserNotFound        = errors.New("USER_NOT_FOUND")
	ErrTransferInsufficientBalance = errors.New("INSUFFICIENT_BALANCE")
)

// transferStatuses mirrors TRANSFER_STATUSES.
var transferStatuses = map[models.TransferStatus]bool{
	models.TransferStatusPending:    true,
	models.TransferStatusProcessing: true,
	models.TransferStatusAccepted:   true,
	models.TransferStatusRejected:   true,
	models.TransferStatusCancelled:  true,
}

// ==================== types ====================

// TransferSnapshot mirrors the TransferSnapshot interface. Field order matches the
// TypeScript object literal so a freshly written jsonb value is byte-identical
// before PostgreSQL normalises it.
type TransferSnapshot struct {
	PackageID         *int32  `json:"packageId"`
	PackageName       *string `json:"packageName"`
	HostID            int32   `json:"hostId"`
	HostName          string  `json:"hostName"`
	HostLocation      *string `json:"hostLocation"`
	HostCountryCode   string  `json:"hostCountryCode"`
	OriginalName      string  `json:"originalName"`
	CPU               int32   `json:"cpu"`
	Memory            int32   `json:"memory"`
	Disk              int32   `json:"disk"`
	NetworkMode       string  `json:"networkMode"`
	PortMappingsCount int     `json:"portMappingsCount"`
	SnapshotsCount    int     `json:"snapshotsCount"`
	BackupsCount      int     `json:"backupsCount"`
	IPv4              *string `json:"ipv4"`
	IPv6              *string `json:"ipv6"`
}

// TransferInstanceRef mirrors the `select { id, name, status, image }` projection.
type TransferInstanceRef struct {
	ID     int32                 `json:"id"`
	Name   string                `json:"name"`
	Status models.InstanceStatus `json:"status"`
	Image  string                `json:"image"`
}

// TransferUserRef mirrors
// `select { id, username, email, avatarStyle, avatarBadgeId }`.
type TransferUserRef struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

// TransferRecord mirrors an InstanceTransfer row with its loaded relations. The
// TypeScript returns Prisma objects whose camelCase fields the route copies
// explicitly; the JSON tags here are the same camelCase names so the module can
// also be used directly by future callers.
type TransferRecord struct {
	ID           int32                 `json:"id"`
	InstanceID   int32                 `json:"instanceId"`
	FromUserID   int32                 `json:"fromUserId"`
	ToUserID     int32                 `json:"toUserId"`
	Status       models.TransferStatus `json:"status"`
	Snapshot     json.RawMessage       `json:"snapshot"`
	Remark       *string               `json:"remark"`
	RejectReason *string               `json:"rejectReason"`
	Fee          *models.Decimal       `json:"fee"`
	CreatedAt    time.Time             `json:"createdAt"`
	UpdatedAt    time.Time             `json:"updatedAt"`
	AcceptedAt   *time.Time            `json:"acceptedAt"`
	RejectedAt   *time.Time            `json:"rejectedAt"`
	CancelledAt  *time.Time            `json:"cancelledAt"`
	Instance     *TransferInstanceRef  `json:"instance,omitempty"`
	FromUser     *TransferUserRef      `json:"fromUser,omitempty"`
	ToUser       *TransferUserRef      `json:"toUser,omitempty"`
}

// TransferListOptions mirrors the options object of getTransfersSent/Received.
type TransferListOptions struct {
	Page     int
	PageSize int
	Status   string
	Search   string
}

// TransferListResult mirrors the {items,total,page,pageSize,totalPages} shape.
type TransferListResult struct {
	Items      []TransferRecord `json:"items"`
	Total      int64            `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"pageSize"`
	TotalPages int              `json:"totalPages"`
}

// TransferCreateWithFeeInput mirrors createTransferWithFee's argument object.
type TransferCreateWithFeeInput struct {
	InstanceID   int32
	FromUserID   int32
	ToUserID     int32
	Snapshot     TransferSnapshot
	Remark       *string
	Fee          float64
	InstanceName string
}

// TransferCreateWithFeeResult mirrors {transferId, feeDeducted}.
type TransferCreateWithFeeResult struct {
	TransferID  int32 `json:"transferId"`
	FeeDeducted bool  `json:"feeDeducted"`
}

// TransferCreateInput mirrors createTransfer's argument object.
type TransferCreateInput struct {
	InstanceID int32
	FromUserID int32
	ToUserID   int32
	Snapshot   TransferSnapshot
	Remark     *string
	Fee        *float64
}

// TransferRefundReason mirrors `reason: 'rejected' | 'cancelled'`.
type TransferRefundReason string

const (
	TransferRefundReasonRejected  TransferRefundReason = "rejected"
	TransferRefundReasonCancelled TransferRefundReason = "cancelled"
)

// TransferRefundInput mirrors refundTransferFee's argument object.
type TransferRefundInput struct {
	TransferID   int32
	FromUserID   int32
	Fee          float64
	InstanceID   int32
	InstanceName string
	Reason       TransferRefundReason
}

// ==================== pagination helpers ====================

// transferClampPagination mirrors clampTransferListPagination. Note the
// TypeScript's `Number.isInteger(page) && page !== undefined && page > 0`
// checks: a zero, negative or fractional page falls back to 1, and pageSize is
// clamped into [1, 100].
func transferClampPagination(page, pageSize int) (int, int) {
	clampedPage := 1
	if page > 0 {
		clampedPage = page
	}
	clampedSize := 20
	if pageSize > 0 {
		clampedSize = pageSize
		if clampedSize > transferListMaxPageSize {
			clampedSize = transferListMaxPageSize
		}
	}
	return clampedPage, clampedSize
}

// transferNormalizeStatus mirrors normalizeTransferStatus: an unknown value means
// "no filter", never an error.
func transferNormalizeStatus(status string) models.TransferStatus {
	candidate := models.TransferStatus(status)
	if transferStatuses[candidate] {
		return candidate
	}
	return ""
}

// transferNormalizeSearch mirrors normalizeTransferSearch (`trim`, then a
// UTF-16 slice to 128 code units). An empty result means "no search".
func transferNormalizeSearch(search string) string {
	trimmed := transferJSTrim(search)
	if trimmed == "" {
		return ""
	}
	return transferSliceUTF16(trimmed, transferListSearchMaxLen)
}

// transferSliceUTF16 mirrors `String.prototype.slice(0, max)` for a non-negative
// bound: JavaScript counts UTF-16 code units, so an emoji at the boundary counts
// as two. A split surrogate pair cannot be represented in a Go string; it becomes
// U+FFFD, which is the only observable difference and needs a 128-unit boundary
// inside a supplementary character.
func transferSliceUTF16(value string, max int) string {
	runes := []rune(value)
	units := 0
	for index, char := range runes {
		charUnits := 1
		if char > 0xFFFF {
			charUnits = 2
		}
		if units+charUnits > max {
			return string(runes[:index])
		}
		units += charUnits
	}
	return value
}

// transferJSTrim mirrors `String.prototype.trim`: ECMAScript WhiteSpace is the
// Unicode space separators plus U+FEFF, and notably does NOT include U+0085
// (which unicode.IsSpace would strip).
func transferJSTrim(value string) string {
	return strings.TrimFunc(value, func(char rune) bool {
		if char == '\uFEFF' {
			return true
		}
		return char != '\u0085' && unicode.IsSpace(char)
	})
}

// ==================== cleanup ====================

// CleanupStaleTransfers mirrors cleanupStaleTransfers: every `processing` row goes
// back to `pending` so a restart never strands a transfer. Returns the row count.
func CleanupStaleTransfers(ctx context.Context) (int64, error) {
	if Pool() == nil {
		return 0, errors.New("database not initialized")
	}
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'pending', updated_at = $1 WHERE status = 'processing'`,
		time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CleanupTimeoutTransfers mirrors cleanupTimeoutTransfers: a `processing` row older
// than 30 minutes goes back to `pending`. updated_at is the age signal, so the
// UPDATE writes a fresh one (Prisma's @updatedAt does the same).
func CleanupTimeoutTransfers(ctx context.Context) (int64, error) {
	if Pool() == nil {
		return 0, errors.New("database not initialized")
	}
	now := time.Now().UTC()
	cutoff := now.Add(-30 * time.Minute)
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'pending', updated_at = $1
		 WHERE status = 'processing' AND updated_at < $2`, now, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ==================== create ====================

// CreateTransferWithFee mirrors createTransferWithFee: create the transfer row and
// debit the fee in one transaction, retrying only while the per-instance lock is
// busy. The balance conflict aborts immediately (the TypeScript throws, so the
// Prisma transaction rolls back and the loop is not re-entered).
func CreateTransferWithFee(ctx context.Context, data TransferCreateWithFeeInput) (TransferCreateWithFeeResult, error) {
	snapshot, err := json.Marshal(data.Snapshot)
	if err != nil {
		return TransferCreateWithFeeResult{}, err
	}

	for attempt := 0; attempt < transferCreateLockRetryLimit; attempt++ {
		result, locked, err := transferCreateOnce(ctx, data, snapshot)
		if err != nil {
			return TransferCreateWithFeeResult{}, err
		}
		if locked {
			return result, nil
		}
		// The TypeScript sleeps 100ms * (attempt + 1), including after the last
		// attempt, before throwing TRANSFER_ALREADY_PENDING.
		transferSleep(transferCreateLockRetryDelay * time.Duration(attempt+1))
	}
	return TransferCreateWithFeeResult{}, ErrTransferAlreadyPending
}

// transferCreateOnce runs one transaction attempt. locked=false means the advisory
// lock was busy and the caller may retry; any other failure is terminal.
func transferCreateOnce(
	ctx context.Context,
	data TransferCreateWithFeeInput,
	snapshot json.RawMessage,
) (TransferCreateWithFeeResult, bool, error) {
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return TransferCreateWithFeeResult{}, false, err
	}
	// Every early return rolls back, which also releases the transaction-scoped
	// advisory lock. A successful path commits explicitly.
	rollback := func() { _ = tx.Rollback(context.Background()) }

	locked, err := TryAdvisoryTransactionLock(ctx, tx, TransferCreateLockNamespace, data.InstanceID)
	if err != nil {
		rollback()
		return TransferCreateWithFeeResult{}, false, err
	}
	if !locked {
		rollback()
		return TransferCreateWithFeeResult{}, false, nil
	}

	// 0. Re-check inside the lock: another request may have created a transfer
	// while this one was waiting for the lock.
	var existingID int32
	err = tx.QueryRow(ctx,
		`SELECT id FROM instance_transfers
		  WHERE instance_id = $1 AND status IN ('pending','processing') LIMIT 1`,
		data.InstanceID).Scan(&existingID)
	switch {
	case err == nil:
		rollback()
		return TransferCreateWithFeeResult{}, false, ErrTransferAlreadyPending
	case !errors.Is(err, pgx.ErrNoRows):
		rollback()
		return TransferCreateWithFeeResult{}, false, err
	}

	feeDeducted := data.Fee > 0
	if feeDeducted {
		balanceLocked, lockErr := TryAdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, data.FromUserID)
		if lockErr != nil {
			rollback()
			return TransferCreateWithFeeResult{}, false, lockErr
		}
		if !balanceLocked {
			rollback()
			return TransferCreateWithFeeResult{}, false, ErrTransferBalanceConflict
		}

		var balance models.Decimal
		err = tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, data.FromUserID).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			rollback()
			return TransferCreateWithFeeResult{}, false, ErrTransferUserNotFound
		}
		if err != nil {
			rollback()
			return TransferCreateWithFeeResult{}, false, err
		}

		balanceBefore := balance.InexactFloat64()
		if balanceBefore < data.Fee {
			rollback()
			return TransferCreateWithFeeResult{}, false, ErrTransferInsufficientBalance
		}

		feeDecimal := decimalFromFloat(data.Fee)
		tag, updateErr := tx.Exec(ctx,
			`UPDATE users SET balance = balance - $1, updated_at = $2
			  WHERE id = $3 AND balance >= $1`,
			feeDecimal, time.Now().UTC(), data.FromUserID)
		if updateErr != nil {
			rollback()
			return TransferCreateWithFeeResult{}, false, updateErr
		}
		if tag.RowsAffected() == 0 {
			rollback()
			return TransferCreateWithFeeResult{}, false, ErrTransferInsufficientBalance
		}

		var updated models.Decimal
		if err := tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, data.FromUserID).Scan(&updated); err != nil {
			rollback()
			return TransferCreateWithFeeResult{}, false, err
		}

		remark := fmt.Sprintf("转移实例 \"%s\" 手续费", data.InstanceName)
		if _, err := tx.Exec(ctx,
			`INSERT INTO balance_logs
			   (user_id, type, amount, balance_before, balance_after, order_id, instance_id, remark, created_at)
			 VALUES ($1, $2, $3, $4, $5, NULL, $6, $7, NOW())`,
			data.FromUserID, string(models.BalanceLogTypeTransferFee),
			decimalFromFloat(-data.Fee),
			decimalFromFloat(balanceBefore),
			decimalFromFloat(updated.InexactFloat64()),
			data.InstanceID, remark); err != nil {
			rollback()
			return TransferCreateWithFeeResult{}, false, err
		}
	}

	var feeColumn any = nil
	if feeDeducted {
		feeColumn = decimalFromFloat(data.Fee)
	}
	now := time.Now().UTC()
	var transferID int32
	err = tx.QueryRow(ctx,
		`INSERT INTO instance_transfers
		   (instance_id, from_user_id, to_user_id, status, snapshot, remark, fee, created_at, updated_at)
		 VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, $8)
		 RETURNING id`,
		data.InstanceID, data.FromUserID, data.ToUserID, datatypes.JSON(snapshot),
		data.Remark, feeColumn, now, now).Scan(&transferID)
	if err != nil {
		rollback()
		return TransferCreateWithFeeResult{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TransferCreateWithFeeResult{}, false, err
	}
	return TransferCreateWithFeeResult{TransferID: transferID, FeeDeducted: feeDeducted}, true, nil
}

// CreateTransfer mirrors the legacy createTransfer (no fee handling, same lock and
// retry schedule).
func CreateTransfer(ctx context.Context, data TransferCreateInput) (int32, error) {
	snapshot, err := json.Marshal(data.Snapshot)
	if err != nil {
		return 0, err
	}

	for attempt := 0; attempt < transferCreateLockRetryLimit; attempt++ {
		transferID, done, err := transferCreateLegacyOnce(ctx, data, snapshot)
		if err != nil {
			return 0, err
		}
		if done {
			return transferID, nil
		}
		transferSleep(transferCreateLockRetryDelay * time.Duration(attempt+1))
	}
	return 0, ErrTransferAlreadyPending
}

func transferCreateLegacyOnce(
	ctx context.Context,
	data TransferCreateInput,
	snapshot json.RawMessage,
) (int32, bool, error) {
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	rollback := func() { _ = tx.Rollback(context.Background()) }

	locked, err := TryAdvisoryTransactionLock(ctx, tx, TransferCreateLockNamespace, data.InstanceID)
	if err != nil {
		rollback()
		return 0, false, err
	}
	if !locked {
		rollback()
		return 0, false, nil
	}

	var existingID int32
	err = tx.QueryRow(ctx,
		`SELECT id FROM instance_transfers
		  WHERE instance_id = $1 AND status IN ('pending','processing') LIMIT 1`,
		data.InstanceID).Scan(&existingID)
	switch {
	case err == nil:
		rollback()
		return 0, false, ErrTransferAlreadyPending
	case !errors.Is(err, pgx.ErrNoRows):
		rollback()
		return 0, false, err
	}

	var feeColumn any = nil
	if data.Fee != nil {
		// The legacy signature writes whatever it receives, including 0.
		feeColumn = decimalFromFloat(*data.Fee)
	}
	now := time.Now().UTC()
	var transferID int32
	err = tx.QueryRow(ctx,
		`INSERT INTO instance_transfers
		   (instance_id, from_user_id, to_user_id, status, snapshot, remark, fee, created_at, updated_at)
		 VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, $8)
		 RETURNING id`,
		data.InstanceID, data.FromUserID, data.ToUserID, datatypes.JSON(snapshot),
		data.Remark, feeColumn, now, now).Scan(&transferID)
	if err != nil {
		rollback()
		return 0, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return transferID, true, nil
}

// ==================== reads ====================

// HasPendingTransfer mirrors hasPendingTransfer (pending OR processing).
func HasPendingTransfer(ctx context.Context, instanceID int32) (bool, error) {
	var count int64
	err := Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM instance_transfers
		  WHERE instance_id = $1 AND status IN ('pending','processing')`, instanceID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetPendingTransferByInstanceID mirrors getPendingTransferByInstanceId. The
// instance relation is not loaded in the TypeScript, so Instance stays nil.
func GetPendingTransferByInstanceID(ctx context.Context, instanceID int32) (*TransferRecord, error) {
	query := transferSelectColumns + `
		FROM instance_transfers t
		JOIN instances i ON i.id = t.instance_id
		JOIN users fu ON fu.id = t.from_user_id
		JOIN users tu ON tu.id = t.to_user_id
		WHERE t.instance_id = $1 AND t.status IN ('pending','processing')
		LIMIT 1`
	return transferQueryOne(ctx, query, instanceID)
}

// GetTransferByID mirrors getTransferById.
func GetTransferByID(ctx context.Context, id int32) (*TransferRecord, error) {
	return transferQueryOne(ctx, transferSelectFrom+" WHERE t.id = $1", id)
}

// GetTransferByIDWithFee mirrors getTransferByIdWithFee. The TypeScript's only
// difference is that it also selects `fee`; the shared projection already carries
// it (the route never exposes fee for this endpoint).
func GetTransferByIDWithFee(ctx context.Context, id int32) (*TransferRecord, error) {
	return transferQueryOne(ctx, transferSelectFrom+" WHERE t.id = $1", id)
}

// TransferPackageNameByID returns packages.name for the transfer snapshot. The
// TypeScript calls the full getPackageById and reads `.name`; only that field is
// consumed, so the query projects exactly one column.
func TransferPackageNameByID(ctx context.Context, id int32) (*string, error) {
	var name string
	err := Pool().QueryRow(ctx, `SELECT name FROM packages WHERE id = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &name, nil
}

// GetTransfersSent mirrors getTransfersSent.
func GetTransfersSent(ctx context.Context, userID int32, options TransferListOptions) (TransferListResult, error) {
	return transferList(ctx, userID, options, true)
}

// GetTransfersReceived mirrors getTransfersReceived.
func GetTransfersReceived(ctx context.Context, userID int32, options TransferListOptions) (TransferListResult, error) {
	return transferList(ctx, userID, options, false)
}

// GetPendingReceivedCount mirrors getPendingReceivedCount: only `pending`, because
// `processing` means "being accepted right now".
func GetPendingReceivedCount(ctx context.Context, userID int32) (int64, error) {
	var count int64
	err := Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM instance_transfers WHERE to_user_id = $1 AND status = 'pending'`,
		userID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ==================== state transitions ====================

// AcceptTransfer mirrors acceptTransfer: unconditional status write by id
// (Prisma's update()).
func AcceptTransfer(ctx context.Context, id int32) error {
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'accepted', accepted_at = $1, updated_at = $1 WHERE id = $2`,
		time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTransferRecordNotFound
	}
	return nil
}

// RejectTransfer mirrors rejectTransfer. `reason` nil writes NULL.
func RejectTransfer(ctx context.Context, id int32, reason *string) error {
	now := time.Now().UTC()
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers
		    SET status = 'rejected', rejected_at = $1, reject_reason = $2, updated_at = $1
		  WHERE id = $3`, now, reason, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTransferRecordNotFound
	}
	return nil
}

// CancelTransfer mirrors cancelTransfer.
func CancelTransfer(ctx context.Context, id int32) error {
	now := time.Now().UTC()
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'cancelled', cancelled_at = $1, updated_at = $1 WHERE id = $2`,
		now, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTransferRecordNotFound
	}
	return nil
}

// ErrTransferRecordNotFound mirrors Prisma's P2025 for the unconditional updates
// above. The route only calls them after a successful read, so reaching this
// sentinel means a concurrent delete (500 in Node).
var ErrTransferRecordNotFound = errors.New("instance_transfers: record to update not found")

// ClaimTransferForAccept is the atomic, idempotent gate of POST /:id/accept:
// pending -> processing only when the caller is the recipient. RowsAffected==0
// means "not pending, not found, or not the recipient"; the caller re-reads the
// row to choose between 404 / 403 / 400.
func ClaimTransferForAccept(ctx context.Context, id, toUserID int32) (int64, error) {
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'processing', updated_at = $1
		  WHERE id = $2 AND status = 'pending' AND to_user_id = $3`,
		time.Now().UTC(), id, toUserID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ClaimTransferForPush is the same gate for POST /:id/push, keyed on the initiator
// (only the sender may push).
func ClaimTransferForPush(ctx context.Context, id, fromUserID int32) (int64, error) {
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'processing', updated_at = $1
		  WHERE id = $2 AND status = 'pending' AND from_user_id = $3`,
		time.Now().UTC(), id, fromUserID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RejectPendingTransfer mirrors the route's updateMany for POST /:id/reject.
func RejectPendingTransfer(ctx context.Context, id, toUserID int32, reason *string) (int64, error) {
	now := time.Now().UTC()
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers
		    SET status = 'rejected', reject_reason = $1, rejected_at = $2, updated_at = $2
		  WHERE id = $3 AND status = 'pending' AND to_user_id = $4`,
		reason, now, id, toUserID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CancelPendingTransfer mirrors the route's updateMany for POST /:id/cancel.
func CancelPendingTransfer(ctx context.Context, id, fromUserID int32) (int64, error) {
	now := time.Now().UTC()
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'cancelled', cancelled_at = $1, updated_at = $1
		  WHERE id = $2 AND status = 'pending' AND from_user_id = $3`,
		now, id, fromUserID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RollbackTransferToPending mirrors the route's rollbackToPending closure: an
// unconditional write by id. The route logs and continues on error, exactly like
// the TypeScript's try/catch.
func RollbackTransferToPending(ctx context.Context, id int32) error {
	tag, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET status = 'pending', updated_at = $1 WHERE id = $2`,
		time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTransferRecordNotFound
	}
	return nil
}

// ==================== refund ====================

// RefundTransferFee mirrors refundTransferFee. The TypeScript swallows every error
// into {success:false,error}; the Go version returns the same shape so the caller's
// logging branch stays identical.
func RefundTransferFee(ctx context.Context, data TransferRefundInput) (bool, string) {
	if Pool() == nil {
		return false, "database not initialized"
	}

	err := transferRefundOnce(ctx, data)
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}

func transferRefundOnce(ctx context.Context, data TransferRefundInput) error {
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback(context.Background()) }

	locked, err := TryAdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, data.FromUserID)
	if err != nil {
		rollback()
		return err
	}
	if !locked {
		rollback()
		return ErrTransferBalanceConflict
	}

	var balance models.Decimal
	err = tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, data.FromUserID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		rollback()
		return ErrTransferUserNotFound
	}
	if err != nil {
		rollback()
		return err
	}
	balanceBefore := balance.InexactFloat64()

	feeDecimal := decimalFromFloat(data.Fee)
	var updated models.Decimal
	err = tx.QueryRow(ctx,
		`UPDATE users SET balance = balance + $1, updated_at = $2 WHERE id = $3 RETURNING balance`,
		feeDecimal, time.Now().UTC(), data.FromUserID).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		rollback()
		return ErrTransferUserNotFound
	}
	if err != nil {
		rollback()
		return err
	}
	balanceAfter := updated.InexactFloat64()

	remark := fmt.Sprintf("转移实例 \"%s\" 被拒绝，退还手续费", data.InstanceName)
	if data.Reason == TransferRefundReasonCancelled {
		remark = fmt.Sprintf("取消转移实例 \"%s\"，退还手续费", data.InstanceName)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO balance_logs
		   (user_id, type, amount, balance_before, balance_after, order_id, instance_id, remark, created_at)
		 VALUES ($1, $2, $3, $4, $5, NULL, $6, $7, NOW())`,
		data.FromUserID, string(models.BalanceLogTypeTransferRefund),
		feeDecimal,
		decimalFromFloat(balanceBefore),
		decimalFromFloat(balanceAfter),
		data.InstanceID, remark); err != nil {
		rollback()
		return err
	}

	return tx.Commit(ctx)
}

// ==================== execute ====================

// ExecuteTransfer mirrors executeTransfer: move ownership, rename, disable
// auto-renew and (when a fee was charged) write the instance_billing_records row
// in ONE serializable transaction.
//
// `_fromUserID` is kept for signature compatibility with the TypeScript, which
// documents that it is unused (quotas are not updated).
func ExecuteTransfer(
	ctx context.Context,
	transferID int32,
	instanceID int32,
	_fromUserID int32,
	toUserID int32,
	newInstanceName string,
	newIncusID string,
) error {
	if Pool() == nil {
		return errors.New("database not initialized")
	}

	txCtx, cancel := context.WithTimeout(ctx, transferTransactionTimeout)
	defer cancel()

	tx, err := Pool().BeginTx(txCtx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback(context.Background()) }

	var feeText *string
	err = tx.QueryRow(txCtx,
		`SELECT fee::text FROM instance_transfers WHERE id = $1`, transferID).Scan(&feeText)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		rollback()
		return err
	}

	now := time.Now().UTC()

	// Ownership, name, Incus id; the new owner must re-enable auto-renew
	// themselves and the instance moves to the top of their list.
	tag, err := tx.Exec(txCtx,
		`UPDATE instances
		    SET user_id = $1, name = $2, incus_id = $3, auto_renew = false, display_order = 0, updated_at = $4
		  WHERE id = $5`, toUserID, newInstanceName, newIncusID, now, instanceID)
	if err != nil {
		rollback()
		return err
	}
	if tag.RowsAffected() == 0 {
		rollback()
		return ErrInstanceRecordNotFound
	}

	tag, err = tx.Exec(txCtx,
		`UPDATE instance_transfers SET status = 'accepted', accepted_at = $1, updated_at = $1 WHERE id = $2`,
		now, transferID)
	if err != nil {
		rollback()
		return err
	}
	if tag.RowsAffected() == 0 {
		rollback()
		return ErrTransferRecordNotFound
	}

	var fee models.Decimal
	if feeText != nil {
		parsed, parseErr := models.NewDecimalFromString(*feeText)
		if parseErr != nil {
			rollback()
			return parseErr
		}
		fee = parsed
	}
	if feeText != nil && fee.IsPositive() {
		var fromUserID int32
		if err := tx.QueryRow(txCtx,
			`SELECT from_user_id FROM instance_transfers WHERE id = $1`, transferID).Scan(&fromUserID); err != nil {
			rollback()
			return err
		}
		if _, err := tx.Exec(txCtx,
			`INSERT INTO instance_billing_records
			   (instance_id, user_id, type, amount, months, period_start, period_end, remark)
			 VALUES ($1, $2, $3, $4, 0, $5, $5, $6)`,
			instanceID, fromUserID, string(models.BillingRecordTypeTransferFee), fee, now,
			"转移实例手续费"); err != nil {
			rollback()
			return err
		}
	}

	return tx.Commit(txCtx)
}

// ==================== shared query plumbing ====================

const transferSelectColumns = `SELECT
	t.id, t.instance_id, t.from_user_id, t.to_user_id, t.status, t.snapshot, t.remark,
	t.reject_reason, t.fee::text, t.created_at, t.updated_at,
	t.accepted_at, t.rejected_at, t.cancelled_at,
	i.id, i.name, i.status, i.image,
	fu.id, fu.username, fu.email, fu.avatar_style, fu.avatar_badge_id,
	tu.id, tu.username, tu.email, tu.avatar_style, tu.avatar_badge_id`

const transferSelectFrom = transferSelectColumns + `
	FROM instance_transfers t
	JOIN instances i ON i.id = t.instance_id
	JOIN users fu ON fu.id = t.from_user_id
	JOIN users tu ON tu.id = t.to_user_id`

// transferQueryOne runs a single-row query and returns (nil, nil) when absent,
// mirroring Prisma's findUnique.
func transferQueryOne(ctx context.Context, query string, args ...any) (*TransferRecord, error) {
	rows, err := Pool().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	record, err := transferScanRecord(rows)
	if err != nil {
		return nil, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return record, nil
}

// transferScanRecord scans the shared projection. Times come back in UTC; the
// route formats them as ISO-8601 with milliseconds (Date#toISOString).
func transferScanRecord(row pgx.Row) (*TransferRecord, error) {
	record := &TransferRecord{}
	var (
		snapshot     []byte
		feeText      *string
		instanceID   int32
		instanceName string
		instanceStat models.InstanceStatus
		instanceImg  string
		fromID       int32
		fromName     string
		fromEmail    *string
		fromAvatar   string
		fromBadge    *string
		toID         int32
		toName       string
		toEmail      *string
		toAvatar     string
		toBadge      *string
	)
	err := row.Scan(
		&record.ID, &record.InstanceID, &record.FromUserID, &record.ToUserID,
		&record.Status, &snapshot, &record.Remark, &record.RejectReason, &feeText,
		&record.CreatedAt, &record.UpdatedAt, &record.AcceptedAt, &record.RejectedAt, &record.CancelledAt,
		&instanceID, &instanceName, &instanceStat, &instanceImg,
		&fromID, &fromName, &fromEmail, &fromAvatar, &fromBadge,
		&toID, &toName, &toEmail, &toAvatar, &toBadge,
	)
	if err != nil {
		return nil, err
	}

	if len(snapshot) > 0 {
		record.Snapshot = json.RawMessage(snapshot)
	}
	if feeText != nil {
		parsed, err := models.NewDecimalFromString(*feeText)
		if err != nil {
			return nil, err
		}
		record.Fee = &parsed
	}

	record.Instance = &TransferInstanceRef{
		ID: instanceID, Name: instanceName, Status: instanceStat, Image: instanceImg,
	}
	record.FromUser = &TransferUserRef{
		ID: fromID, Username: fromName, Email: fromEmail, AvatarStyle: fromAvatar, AvatarBadgeID: fromBadge,
	}
	record.ToUser = &TransferUserRef{
		ID: toID, Username: toName, Email: toEmail, AvatarStyle: toAvatar, AvatarBadgeID: toBadge,
	}
	return record, nil
}

// transferList builds the filtered/paginated query shared by the sent and
// received lists. `sent` switches both the owner column and the searchable user
// relation.
func transferList(ctx context.Context, userID int32, options TransferListOptions, sent bool) (TransferListResult, error) {
	page, pageSize := transferClampPagination(options.Page, options.PageSize)

	args := []any{}
	where := ""
	searchUser := "tu.username"
	ownerColumn := "t.to_user_id"
	if sent {
		ownerColumn = "t.from_user_id"
		searchUser = "tu.username"
	} else {
		searchUser = "fu.username"
	}
	args = append(args, userID)
	where = fmt.Sprintf(" WHERE %s = $1", ownerColumn)

	status := transferNormalizeStatus(options.Status)
	if status != "" {
		args = append(args, string(status))
		where += fmt.Sprintf(" AND t.status = $%d", len(args))
	}

	search := transferNormalizeSearch(options.Search)
	if search != "" {
		args = append(args, search)
		index := len(args)
		where += fmt.Sprintf(
			" AND (i.name ILIKE '%%' || $%d || '%%' OR %s ILIKE '%%' || $%d || '%%' OR t.remark ILIKE '%%' || $%d || '%%')",
			index, searchUser, index, index)
	}

	var total int64
	countQuery := `SELECT COUNT(*)
		FROM instance_transfers t
		JOIN instances i ON i.id = t.instance_id
		JOIN users fu ON fu.id = t.from_user_id
		JOIN users tu ON tu.id = t.to_user_id` + where
	if err := Pool().QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return TransferListResult{}, err
	}

	args = append(args, pageSize, (page-1)*pageSize)
	listQuery := transferSelectFrom + where +
		fmt.Sprintf(" ORDER BY t.created_at DESC, t.id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := Pool().Query(ctx, listQuery, args...)
	if err != nil {
		return TransferListResult{}, err
	}
	defer rows.Close()

	items := make([]TransferRecord, 0, pageSize)
	for rows.Next() {
		record, scanErr := transferScanRecord(rows)
		if scanErr != nil {
			return TransferListResult{}, scanErr
		}
		items = append(items, *record)
	}
	if err := rows.Err(); err != nil {
		return TransferListResult{}, err
	}

	totalPages := 0
	if total > 0 {
		totalPages = int((total + int64(pageSize) - 1) / int64(pageSize))
	}

	return TransferListResult{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
	}, nil
}

// TransfersDeleteSnapshotPolicyByInstance mirrors
// `prisma.snapshotPolicy.deleteMany({ where: { instanceId } })`. A transfer
// deliberately drops the snapshot/backup policies: they belong to the previous
// owner's schedule.
func TransfersDeleteSnapshotPolicyByInstance(ctx context.Context, instanceID int32) error {
	_, err := Pool().Exec(ctx, `DELETE FROM snapshot_policies WHERE instance_id = $1`, instanceID)
	return err
}

// TransfersDeleteBackupPolicyByInstance mirrors
// `prisma.backupPolicy.deleteMany({ where: { instanceId } })`.
func TransfersDeleteBackupPolicyByInstance(ctx context.Context, instanceID int32) error {
	_, err := Pool().Exec(ctx, `DELETE FROM backup_policies WHERE instance_id = $1`, instanceID)
	return err
}
