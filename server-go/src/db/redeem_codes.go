// Port of server/src/db/redeem-codes.ts.
//
// System h-codes ("h-" prefix) grant host resources (CPU / memory / disk /
// traffic) to an instance. Redemption is the high-risk path, so every consume
// path is serialized with PostgreSQL *transaction-scoped* advisory locks held on
// the very same pgx.Tx as the guarded statements:
//
//   - useSystemRedeemCode     -> try-lock REDEEM_CODE_LOCK_NAMESPACE, retry with
//     linear backoff, exactly like tryAdvisoryTransactionLock in the TypeScript;
//   - applySystemRedeemCodeToInstance -> blocking locks on REDEEM_CODE_LOCK_NAMESPACE
//     *and* INSTANCE_OPERATION_LOCK_NAMESPACE (same order as the TypeScript), so
//     the redeem-code counter, host usage, instance resources, extra traffic
//     quota and the usage row commit or roll back together.
//
// Over-redemption is impossible because the affected-row count of
//
//	UPDATE redeem_codes SET used_count = used_count + 1
//	 WHERE ... AND used_count < max_uses
//
// is checked (0 rows -> REDEEM_CODE_EXHAUSTED). Per-user idempotency comes from
// the explicit (redeem_code_id, user_id) existence check inside the lock, and the
// per-batch limit from the (user_id, batch_id) unique index (23505 -> batch limit).
package db

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// System redeem-code prefixes and list bounds, mirroring the TypeScript module.
const (
	systemCodePrefix = "h"
	// redeemCodeListMaxLimit mirrors REDEEM_CODE_LIST_MAX_LIMIT.
	redeemCodeListMaxLimit = 100
)

// Consume-path sentinel errors. The messages are byte-identical to the
// `throw new Error('...')` payloads in the TypeScript so a future
// routes/checkin.go port can map them the way routes/checkin.ts does
// (`if (errorMessage === 'REDEEM_CODE_BATCH_LIMIT') ...`).
var (
	ErrRedeemCodeInvalidType          = errors.New("REDEEM_CODE_INVALID_TYPE")
	ErrRedeemCodeAlreadyUsedByUser    = errors.New("REDEEM_CODE_ALREADY_USED_BY_USER")
	ErrRedeemCodeBatchLimit           = errors.New("REDEEM_CODE_BATCH_LIMIT")
	ErrRedeemCodeExhausted            = errors.New("REDEEM_CODE_EXHAUSTED")
	ErrRedeemCodeBusy                 = errors.New("REDEEM_CODE_BUSY")
	errRedeemCodeUserQuotaNotFound    = errors.New("redeem code: user quota not found")
	errRedeemCodeInstanceNotFound     = errors.New("redeem code: instance not found")
	errRedeemCodeHostNotFound         = errors.New("redeem code: host not found")
	errRedeemCodeDatabaseNotReady     = errors.New("redeem code: database not initialized")
	redeemResourceRedeemCodeTypeIndex = map[models.RedeemCodeType]bool{
		models.RedeemCodeTypeC: true,
		models.RedeemCodeTypeR: true,
		models.RedeemCodeTypeD: true,
		models.RedeemCodeTypeT: true,
	}
)

// CodeValues mirrors CODE_VALUES — the fixed pools the check-in system draws from
// (do not change: they are persisted semantics, not UI defaults).
var CodeValues = map[models.RedeemCodeType][]int32{
	models.RedeemCodeTypeC: {1, 2, 3, 5},      // CPU: 1-5%
	models.RedeemCodeTypeR: {2, 4, 6, 12},     // 内存: 2/4/6/12 MB
	models.RedeemCodeTypeD: {16, 32, 64, 128}, // 硬盘: 16/32/64/128 MB
	models.RedeemCodeTypeT: {1, 2, 3, 5},      // 流量: 1/2/3/5 GB
}

// CodeValueRange mirrors one entry of CODE_VALUE_RANGES.
type CodeValueRange struct {
	Min  int32  `json:"min"`
	Max  int32  `json:"max"`
	Unit string `json:"unit"`
}

// CodeValueRanges mirrors CODE_VALUE_RANGES (host redeem codes allow any value
// inside these bounds). The `p` (points) type is deliberately absent: points
// redeem codes are deprecated and every h-code creation path rejects them.
var CodeValueRanges = map[models.RedeemCodeType]CodeValueRange{
	models.RedeemCodeTypeC: {Min: 1, Max: 100, Unit: "%"},      // CPU: 1-100%
	models.RedeemCodeTypeR: {Min: 1, Max: 65536, Unit: "MB"},   // 内存: 1MB - 64GB
	models.RedeemCodeTypeD: {Min: 1, Max: 1048576, Unit: "MB"}, // 硬盘: 1MB - 1TB
	models.RedeemCodeTypeT: {Min: 1, Max: 10240, Unit: "GB"},   // 流量: 1GB - 10TB
}

// IsValidCodeValue mirrors isValidCodeValue.
func IsValidCodeValue(codeType models.RedeemCodeType, value int32) bool {
	codeRange, ok := CodeValueRanges[codeType]
	if !ok {
		return false
	}
	return value >= codeRange.Min && value <= codeRange.Max
}

// GenerateSystemCode mirrors generateSystemCode:
// `h-` + base64url(randomBytes(12)).slice(0, 18). 12 random bytes encode to 16
// unpadded base64url characters, so the slice is a no-op in practice but kept for
// byte-exact behaviour.
func GenerateSystemCode() string {
	randomBytes := make([]byte, 12)
	if _, err := rand.Read(randomBytes); err != nil {
		// crypto/rand only fails when the OS entropy source is broken; the Node
		// implementation would throw as well.
		panic(fmt.Sprintf("redeem code: crypto/rand failed: %v", err))
	}
	encoded := base64.RawURLEncoding.EncodeToString(randomBytes)
	if len(encoded) > 18 {
		encoded = encoded[:18]
	}
	return systemCodePrefix + "-" + encoded
}

// IsSystemCode mirrors isSystemCode.
func IsSystemCode(code string) bool {
	return strings.HasPrefix(code, systemCodePrefix+"-")
}

func assertResourceRedeemCodeType(codeType models.RedeemCodeType) error {
	if !redeemResourceRedeemCodeTypeIndex[codeType] {
		return ErrRedeemCodeInvalidType
	}
	return nil
}

// clampRedeemCodeListBounds mirrors clampRedeemCodeListBounds. A nil value stands
// for the TypeScript `undefined` (use defaultLimit); a non-integer or out-of-range
// numeric value is clamped instead of rejected.
func clampRedeemCodeListBounds(limit, offset *int, defaultLimit int) (int, int) {
	safeLimit := defaultLimit
	if limit != nil {
		safeLimit = *limit
		if safeLimit < 1 {
			safeLimit = 1
		}
		if safeLimit > redeemCodeListMaxLimit {
			safeLimit = redeemCodeListMaxLimit
		}
	}
	safeOffset := 0
	if offset != nil && *offset >= 0 {
		safeOffset = *offset
	}
	return safeLimit, safeOffset
}

// ==================== creation ====================

// RedeemCodeCreateData mirrors createRedeemCode's argument object.
type RedeemCodeCreateData struct {
	HostID       int64
	CreatedByID  int64
	CodeType     models.RedeemCodeType
	CodeValue    int32
	MaxUses      *int32
	ExpiresAt    *time.Time
	Remark       *string
	TargetUserID *int32
}

// RedeemCodeBatchCreateData mirrors createRedeemCodeBatch's argument object.
type RedeemCodeBatchCreateData struct {
	HostID       int64
	CreatedByID  int64
	CodeType     models.RedeemCodeType
	CodeValue    int32
	Count        int
	ExpiresAt    *time.Time
	Remark       *string
	TargetUserID *int32
}

// RedeemCodeBatchResult is the `{codes, batchId}` return value.
type RedeemCodeBatchResult struct {
	Codes   []string `json:"codes"`
	BatchID string   `json:"batchId"`
}

// CreateRedeemCode mirrors createRedeemCode. created_at/enabled/used_count are set
// explicitly because the generated model has no autoCreateTime/autoDefault tags
// and GORM writes zero values into every INSERT.
func CreateRedeemCode(ctx context.Context, data RedeemCodeCreateData) (models.RedeemCode, error) {
	if err := assertResourceRedeemCodeType(data.CodeType); err != nil {
		return models.RedeemCode{}, err
	}

	maxUses := int32(1)
	if data.MaxUses != nil {
		maxUses = *data.MaxUses
	}

	record := models.RedeemCode{
		Code:         GenerateSystemCode(),
		HostID:       int32(data.HostID),
		CreatedByID:  int32(data.CreatedByID),
		CodeType:     data.CodeType,
		CodeValue:    data.CodeValue,
		MaxUses:      maxUses,
		UsedCount:    0,
		ExpiresAt:    data.ExpiresAt,
		Enabled:      true,
		Remark:       data.Remark,
		TargetUserID: data.TargetUserID,
	}
	// Returning{} reads the row back so createdAt comes from the database exactly
	// like Prisma's create().
	if err := DB.WithContext(ctx).Clauses(clause.Returning{}).Create(&record).Error; err != nil {
		return models.RedeemCode{}, err
	}
	return record, nil
}

// CreateRedeemCodeBatch mirrors createRedeemCodeBatch: `count` one-use codes that
// share a batch id (one code per user per batch).
func CreateRedeemCodeBatch(ctx context.Context, data RedeemCodeBatchCreateData) (RedeemCodeBatchResult, error) {
	if err := assertResourceRedeemCodeType(data.CodeType); err != nil {
		return RedeemCodeBatchResult{}, err
	}

	batchBytes := make([]byte, 8)
	if _, err := rand.Read(batchBytes); err != nil {
		return RedeemCodeBatchResult{}, fmt.Errorf("redeem code: crypto/rand failed: %w", err)
	}
	batchID := hex.EncodeToString(batchBytes)

	codes := make([]string, 0, data.Count)
	records := make([]models.RedeemCode, 0, data.Count)
	for i := 0; i < data.Count; i++ {
		code := GenerateSystemCode()
		codes = append(codes, code)
		batch := batchID
		records = append(records, models.RedeemCode{
			Code:         code,
			HostID:       int32(data.HostID),
			CreatedByID:  int32(data.CreatedByID),
			CodeType:     data.CodeType,
			CodeValue:    data.CodeValue,
			MaxUses:      1,
			UsedCount:    0,
			ExpiresAt:    data.ExpiresAt,
			Enabled:      true,
			Remark:       data.Remark,
			BatchID:      &batch,
			TargetUserID: data.TargetUserID,
		})
	}

	if len(records) > 0 {
		if err := DB.WithContext(ctx).Create(&records).Error; err != nil {
			return RedeemCodeBatchResult{}, err
		}
	}

	return RedeemCodeBatchResult{Codes: codes, BatchID: batchID}, nil
}

// ==================== reads ====================

// RedeemCodeUserRef is the `{id, username}` projection used by createdBy/user.
type RedeemCodeUserRef struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
}

// RedeemCodeHostRef is the `{id, name}` projection attached to usage rows.
type RedeemCodeHostRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// RedeemCodeListItem mirrors the per-row projection of getRedeemCodesByHost. The
// timestamps are ISO strings because the TypeScript calls toISOString().
type RedeemCodeListItem struct {
	ID           int32                 `json:"id"`
	Code         string                `json:"code"`
	CodeType     models.RedeemCodeType `json:"codeType"`
	CodeValue    int32                 `json:"codeValue"`
	MaxUses      int32                 `json:"maxUses"`
	UsedCount    int32                 `json:"usedCount"`
	ExpiresAt    *string               `json:"expiresAt"`
	Enabled      bool                  `json:"enabled"`
	Remark       *string               `json:"remark"`
	BatchID      *string               `json:"batchId"`
	TargetUserID *int32                `json:"targetUserId"`
	CreatedBy    *RedeemCodeUserRef    `json:"createdBy"`
	CreatedAt    string                `json:"createdAt"`
	IsExpired    bool                  `json:"isExpired"`
	IsExhausted  bool                  `json:"isExhausted"`
}

// RedeemCodeListResult mirrors `{codes, total}`.
type RedeemCodeListResult struct {
	Codes []RedeemCodeListItem `json:"codes"`
	Total int64                `json:"total"`
}

// RedeemCodeListOptions mirrors the optional `{limit, offset, enabled}` object.
type RedeemCodeListOptions struct {
	Limit   *int
	Offset  *int
	Enabled *bool
}

type redeemCodeListRow struct {
	ID              int32      `gorm:"column:id"`
	Code            string     `gorm:"column:code"`
	CodeType        string     `gorm:"column:code_type"`
	CodeValue       int32      `gorm:"column:code_value"`
	MaxUses         int32      `gorm:"column:max_uses"`
	UsedCount       int32      `gorm:"column:used_count"`
	ExpiresAt       *time.Time `gorm:"column:expires_at"`
	Enabled         bool       `gorm:"column:enabled"`
	Remark          *string    `gorm:"column:remark"`
	BatchID         *string    `gorm:"column:batch_id"`
	TargetUserID    *int32     `gorm:"column:target_user_id"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	CreatorID       *int32     `gorm:"column:creator_id"`
	CreatorUsername *string    `gorm:"column:creator_username"`
}

// GetRedeemCodesByHost mirrors getRedeemCodesByHost.
func GetRedeemCodesByHost(ctx context.Context, hostID int64, options RedeemCodeListOptions) (RedeemCodeListResult, error) {
	limit, offset := clampRedeemCodeListBounds(options.Limit, options.Offset, 50)

	where := "WHERE rc.host_id = ?"
	args := []any{hostID}
	if options.Enabled != nil {
		where += " AND rc.enabled = ?"
		args = append(args, *options.Enabled)
	}

	var total int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM redeem_codes rc "+where, args...).Scan(&total).Error; err != nil {
		return RedeemCodeListResult{}, err
	}

	listArgs := append(append([]any{}, args...), limit, offset)
	listSQL := "SELECT rc.id, rc.code, rc.code_type::text AS code_type, rc.code_value, rc.max_uses," +
		" rc.used_count, rc.expires_at, rc.enabled, rc.remark, rc.batch_id, rc.target_user_id," +
		" rc.created_at, u.id AS creator_id, u.username AS creator_username" +
		" FROM redeem_codes rc LEFT JOIN users u ON u.id = rc.created_by_id " +
		where + " ORDER BY rc.created_at DESC LIMIT ? OFFSET ?"

	var rows []redeemCodeListRow
	if err := DB.WithContext(ctx).Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return RedeemCodeListResult{}, err
	}

	now := time.Now().UTC()
	items := make([]RedeemCodeListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, RedeemCodeListItem{
			ID:           row.ID,
			Code:         row.Code,
			CodeType:     models.RedeemCodeType(row.CodeType),
			CodeValue:    row.CodeValue,
			MaxUses:      row.MaxUses,
			UsedCount:    row.UsedCount,
			ExpiresAt:    redeemCodeOptionalISOTime(row.ExpiresAt),
			Enabled:      row.Enabled,
			Remark:       row.Remark,
			BatchID:      row.BatchID,
			TargetUserID: row.TargetUserID,
			CreatedBy:    redeemCodeCreatorRef(row),
			CreatedAt:    redeemCodeISOTime(row.CreatedAt),
			IsExpired:    row.ExpiresAt != nil && row.ExpiresAt.Before(now),
			IsExhausted:  row.UsedCount >= row.MaxUses,
		})
	}

	return RedeemCodeListResult{Codes: items, Total: total}, nil
}

// RedeemCodeWithRelations mirrors getRedeemCodeByCode's `include` result (host and
// createdBy are required relations, so they are always projected).
type RedeemCodeWithRelations struct {
	models.RedeemCode
	Host      *RedeemCodeDetailHostRef `json:"host"`
	CreatedBy *RedeemCodeUserRef       `json:"createdBy"`
}

// RedeemCodeDetailHostRef is the `{id, name, userId}` host projection.
type RedeemCodeDetailHostRef struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	UserID int32  `json:"userId"`
}

type redeemCodeDetailRow struct {
	ID           int32      `gorm:"column:id"`
	Code         string     `gorm:"column:code"`
	HostID       int32      `gorm:"column:host_id"`
	CreatedByID  int32      `gorm:"column:created_by_id"`
	CodeType     string     `gorm:"column:code_type"`
	CodeValue    int32      `gorm:"column:code_value"`
	MaxUses      int32      `gorm:"column:max_uses"`
	UsedCount    int32      `gorm:"column:used_count"`
	ExpiresAt    *time.Time `gorm:"column:expires_at"`
	Enabled      bool       `gorm:"column:enabled"`
	Remark       *string    `gorm:"column:remark"`
	BatchID      *string    `gorm:"column:batch_id"`
	TargetUserID *int32     `gorm:"column:target_user_id"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	HostRefID    *int32     `gorm:"column:host_ref_id"`
	HostName     *string    `gorm:"column:host_name"`
	HostUserID   *int32     `gorm:"column:host_user_id"`
	CreatorID    *int32     `gorm:"column:creator_id"`
	CreatorName  *string    `gorm:"column:creator_username"`
}

// GetRedeemCodeByCode mirrors getRedeemCodeByCode; it returns nil when the code
// does not exist.
func GetRedeemCodeByCode(ctx context.Context, code string) (*RedeemCodeWithRelations, error) {
	sql := "SELECT rc.id, rc.code, rc.host_id, rc.created_by_id, rc.code_type::text AS code_type," +
		" rc.code_value, rc.max_uses, rc.used_count, rc.expires_at, rc.enabled, rc.remark," +
		" rc.batch_id, rc.target_user_id, rc.created_at," +
		" h.id AS host_ref_id, h.name AS host_name, h.user_id AS host_user_id," +
		" u.id AS creator_id, u.username AS creator_username" +
		" FROM redeem_codes rc" +
		" LEFT JOIN hosts h ON h.id = rc.host_id" +
		" LEFT JOIN users u ON u.id = rc.created_by_id" +
		" WHERE rc.code = ? LIMIT 1"

	var rows []redeemCodeDetailRow
	if err := DB.WithContext(ctx).Raw(sql, code).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	row := rows[0]
	record := &RedeemCodeWithRelations{
		RedeemCode: models.RedeemCode{
			ID:           row.ID,
			Code:         row.Code,
			HostID:       row.HostID,
			CreatedByID:  row.CreatedByID,
			CodeType:     models.RedeemCodeType(row.CodeType),
			CodeValue:    row.CodeValue,
			MaxUses:      row.MaxUses,
			UsedCount:    row.UsedCount,
			ExpiresAt:    row.ExpiresAt,
			Enabled:      row.Enabled,
			Remark:       row.Remark,
			BatchID:      row.BatchID,
			TargetUserID: row.TargetUserID,
			CreatedAt:    row.CreatedAt,
		},
	}
	if row.HostRefID != nil {
		record.Host = &RedeemCodeDetailHostRef{
			ID:     *row.HostRefID,
			Name:   redeemCodeDerefString(row.HostName),
			UserID: redeemCodeDerefInt32(row.HostUserID),
		}
	}
	if row.CreatorID != nil {
		record.CreatedBy = &RedeemCodeUserRef{ID: *row.CreatorID, Username: redeemCodeDerefString(row.CreatorName)}
	}
	return record, nil
}

// HasUserUsedCode mirrors hasUserUsedCode.
func HasUserUsedCode(ctx context.Context, redeemCodeID int64, userID int64) (bool, error) {
	var exists bool
	err := DB.WithContext(ctx).Raw(
		"SELECT EXISTS (SELECT 1 FROM redeem_code_usages WHERE redeem_code_id = ? AND user_id = ?)",
		redeemCodeID, userID,
	).Scan(&exists).Error
	if err != nil {
		return false, err
	}
	return exists, nil
}

// HasUserUsedBatch mirrors hasUserUsedBatch. The TypeScript first loads every code
// of the batch and then queries the usages with `redeemCodeId in [...]`; the join
// below is the same predicate and yields no rows when the batch is empty.
func HasUserUsedBatch(ctx context.Context, batchID string, userID int64) (bool, error) {
	var exists bool
	err := DB.WithContext(ctx).Raw(
		"SELECT EXISTS ("+
			"SELECT 1 FROM redeem_code_usages u"+
			" JOIN redeem_codes rc ON rc.id = u.redeem_code_id"+
			" WHERE rc.batch_id = ? AND u.user_id = ?)",
		batchID, userID,
	).Scan(&exists).Error
	if err != nil {
		return false, err
	}
	return exists, nil
}

// ==================== consumption ====================

// redeemCodeLockRetry mirrors LOCK_RETRY_LIMIT / LOCK_RETRY_DELAY_MS.
const (
	redeemCodeLockRetryLimit = 10
	redeemCodeLockRetryDelay = 50 * time.Millisecond
)

// UseSystemRedeemCode mirrors useSystemRedeemCode: it retries the try-lock path up
// to LOCK_RETRY_LIMIT times and fails with REDEEM_CODE_BUSY when the code stays
// locked. It is unused by the ported routes (routes/checkin.ts is not ported yet)
// but is part of the module contract and pins the concurrency semantics.
func UseSystemRedeemCode(ctx context.Context, redeemCodeID, userID, instanceID int64, batchID *string) error {
	for attempt := 0; attempt < redeemCodeLockRetryLimit; attempt++ {
		completed, err := tryUseSystemRedeemCode(ctx, redeemCodeID, userID, instanceID, batchID)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
		time.Sleep(redeemCodeLockRetryDelay * time.Duration(attempt+1))
	}
	return ErrRedeemCodeBusy
}

func tryUseSystemRedeemCode(ctx context.Context, redeemCodeID, userID, instanceID int64, batchID *string) (bool, error) {
	if pool == nil {
		return false, errRedeemCodeDatabaseNotReady
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := TryAdvisoryTransactionLock(ctx, tx, RedeemCodeLockNamespace, int32(redeemCodeID))
	if err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}

	if err := guardRedeemCodeUsage(ctx, tx, redeemCodeID, userID, batchID); err != nil {
		return false, err
	}

	tag, err := tx.Exec(ctx,
		`UPDATE redeem_codes SET used_count = used_count + 1
		  WHERE id = $1
		    AND (target_user_id IS NULL OR target_user_id = $2)
		    AND used_count < max_uses`,
		redeemCodeID, userID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, ErrRedeemCodeExhausted
	}

	if err := insertRedeemCodeUsage(ctx, tx, redeemCodeID, userID, instanceID, batchID); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// guardRedeemCodeUsage mirrors the two findFirst checks that must run inside the
// redeem-code lock before the counter is incremented.
func guardRedeemCodeUsage(ctx context.Context, tx pgx.Tx, redeemCodeID, userID int64, batchID *string) error {
	var existingID int32
	err := tx.QueryRow(ctx,
		"SELECT id FROM redeem_code_usages WHERE redeem_code_id = $1 AND user_id = $2 LIMIT 1",
		redeemCodeID, userID).Scan(&existingID)
	if err == nil {
		return ErrRedeemCodeAlreadyUsedByUser
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if batchID != nil {
		err = tx.QueryRow(ctx,
			"SELECT id FROM redeem_code_usages WHERE user_id = $1 AND batch_id = $2 LIMIT 1",
			userID, *batchID).Scan(&existingID)
		if err == nil {
			return ErrRedeemCodeBatchLimit
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

// insertRedeemCodeUsage mirrors the Prisma usage create; the (user_id, batch_id)
// unique index violation is translated to the batch-limit error exactly like the
// P2002 branch.
func insertRedeemCodeUsage(ctx context.Context, tx pgx.Tx, redeemCodeID, userID, instanceID int64, batchID *string) error {
	_, err := tx.Exec(ctx,
		"INSERT INTO redeem_code_usages (redeem_code_id, user_id, instance_id, batch_id) VALUES ($1, $2, $3, $4)",
		redeemCodeID, userID, instanceID, batchID)
	if err != nil {
		if isRedeemCodeUniqueViolation(err) && batchID != nil {
			return ErrRedeemCodeBatchLimit
		}
		return err
	}
	return nil
}

func isRedeemCodeUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// SystemRedeemInstanceResources mirrors `instanceResources?: {cpu?, memory?, disk?}`.
type SystemRedeemInstanceResources struct {
	CPU    *int32
	Memory *int32
	Disk   *int32
}

// SystemRedeemHostResourceDelta mirrors `hostResourceDelta?: {cpuUsed?, memoryUsed?, diskUsed?}`.
type SystemRedeemHostResourceDelta struct {
	CPUUsed    *int32
	MemoryUsed *int32
	DiskUsed   *int32
}

// SystemRedeemCodeApplication mirrors applySystemRedeemCodeToInstance's argument.
type SystemRedeemCodeApplication struct {
	RedeemCodeID           int64
	UserID                 int64
	InstanceID             int64
	HostID                 int64
	BatchID                *string
	InstanceResources      *SystemRedeemInstanceResources
	HostResourceDelta      *SystemRedeemHostResourceDelta
	ExtraTrafficQuotaDelta *int64
}

// ApplySystemRedeemCodeToInstance mirrors applySystemRedeemCodeToInstance: consume
// the code, bump host usage (increments, never absolute writes from a stale read),
// patch the instance resources, extend the extra traffic quota and write the usage
// row — all in one transaction guarded by the redeem-code and instance locks.
func ApplySystemRedeemCodeToInstance(ctx context.Context, data SystemRedeemCodeApplication) error {
	if pool == nil {
		return errRedeemCodeDatabaseNotReady
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock order matches the TypeScript: redeem code first, then instance.
	if err := AdvisoryTransactionLock(ctx, tx, RedeemCodeLockNamespace, int32(data.RedeemCodeID)); err != nil {
		return err
	}
	if err := AdvisoryTransactionLock(ctx, tx, InstanceOperationLockNamespace, int32(data.InstanceID)); err != nil {
		return err
	}

	if err := guardRedeemCodeUsage(ctx, tx, data.RedeemCodeID, data.UserID, data.BatchID); err != nil {
		return err
	}

	now := time.Now().UTC()
	tag, err := tx.Exec(ctx,
		`UPDATE redeem_codes SET used_count = used_count + 1
		  WHERE id = $1
		    AND enabled = true
		    AND (expires_at IS NULL OR expires_at > $2)
		    AND (target_user_id IS NULL OR target_user_id = $3)
		    AND used_count < max_uses`,
		data.RedeemCodeID, now, data.UserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRedeemCodeExhausted
	}

	if data.HostResourceDelta != nil {
		sets := make([]string, 0, 3)
		args := make([]any, 0, 4)
		if data.HostResourceDelta.CPUUsed != nil {
			args = append(args, *data.HostResourceDelta.CPUUsed)
			sets = append(sets, fmt.Sprintf("cpu_used = cpu_used + $%d", len(args)))
		}
		if data.HostResourceDelta.MemoryUsed != nil {
			args = append(args, *data.HostResourceDelta.MemoryUsed)
			sets = append(sets, fmt.Sprintf("memory_used = memory_used + $%d", len(args)))
		}
		if data.HostResourceDelta.DiskUsed != nil {
			args = append(args, *data.HostResourceDelta.DiskUsed)
			sets = append(sets, fmt.Sprintf("disk_used = disk_used + $%d", len(args)))
		}
		if len(sets) > 0 {
			args = append(args, data.HostID)
			hostTag, err := tx.Exec(ctx,
				"UPDATE hosts SET "+strings.Join(sets, ", ")+fmt.Sprintf(" WHERE id = $%d", len(args)), args...)
			if err != nil {
				return err
			}
			if hostTag.RowsAffected() == 0 {
				return errRedeemCodeHostNotFound
			}
		}
	}

	if data.InstanceResources != nil {
		sets := make([]string, 0, 3)
		args := make([]any, 0, 4)
		if data.InstanceResources.CPU != nil {
			args = append(args, *data.InstanceResources.CPU)
			sets = append(sets, fmt.Sprintf("cpu = $%d", len(args)))
		}
		if data.InstanceResources.Memory != nil {
			args = append(args, *data.InstanceResources.Memory)
			sets = append(sets, fmt.Sprintf("memory = $%d", len(args)))
		}
		if data.InstanceResources.Disk != nil {
			args = append(args, *data.InstanceResources.Disk)
			sets = append(sets, fmt.Sprintf("disk = $%d", len(args)))
		}
		if len(sets) > 0 {
			args = append(args, data.InstanceID)
			instanceTag, err := tx.Exec(ctx,
				"UPDATE instances SET "+strings.Join(sets, ", ")+fmt.Sprintf(" WHERE id = $%d", len(args)), args...)
			if err != nil {
				return err
			}
			if instanceTag.RowsAffected() == 0 {
				return errRedeemCodeInstanceNotFound
			}
		}
	}

	if data.ExtraTrafficQuotaDelta != nil {
		quotaTag, err := tx.Exec(ctx,
			"UPDATE user_quotas SET extra_traffic_quota = extra_traffic_quota + $1 WHERE user_id = $2",
			*data.ExtraTrafficQuotaDelta, data.UserID)
		if err != nil {
			return err
		}
		if quotaTag.RowsAffected() == 0 {
			return errRedeemCodeUserQuotaNotFound
		}
	}

	if err := insertRedeemCodeUsage(ctx, tx, data.RedeemCodeID, data.UserID, data.InstanceID, data.BatchID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ==================== management ====================

// RedeemCodeUpdateData mirrors updateRedeemCodeForHost's data argument. Nil
// pointers mean "key absent" (Prisma leaves the column alone); RemarkProvided
// with a nil Remark clears it, ExpiresAtProvided with nil clears the expiry.
type RedeemCodeUpdateData struct {
	EnabledProvided   bool
	Enabled           bool
	RemarkProvided    bool
	Remark            *string
	MaxUsesProvided   bool
	MaxUses           int32
	ExpiresAtProvided bool
	ExpiresAt         *time.Time
}

// UpdateRedeemCodeForHost mirrors updateRedeemCodeForHost; the host scope is part
// of the WHERE clause so a code can never be edited through another host.
func UpdateRedeemCodeForHost(ctx context.Context, hostID, id int64, data RedeemCodeUpdateData) (bool, error) {
	updates := map[string]any{}
	if data.EnabledProvided {
		updates["enabled"] = data.Enabled
	}
	if data.RemarkProvided {
		updates["remark"] = data.Remark
	}
	if data.MaxUsesProvided {
		updates["max_uses"] = data.MaxUses
	}
	if data.ExpiresAtProvided {
		updates["expires_at"] = data.ExpiresAt
	}

	query := DB.WithContext(ctx).Model(&models.RedeemCode{}).Where("id = ? AND host_id = ?", id, hostID)
	if len(updates) == 0 {
		// Prisma receives `data: {}`; mirror the observable result (count of rows
		// matching the host-scoped predicate) instead of emitting an empty UPDATE.
		var count int64
		if err := query.Count(&count).Error; err != nil {
			return false, err
		}
		return count > 0, nil
	}

	result := query.Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// RedeemCodeDeleteOutcome mirrors the 'deleted' | 'disabled' | 'not_found' union.
type RedeemCodeDeleteOutcome string

const (
	RedeemCodeDeleted  RedeemCodeDeleteOutcome = "deleted"
	RedeemCodeDisabled RedeemCodeDeleteOutcome = "disabled"
	RedeemCodeNotFound RedeemCodeDeleteOutcome = "not_found"
)

// DeleteRedeemCodeForHost mirrors deleteRedeemCodeForHost: a code with usage
// history is disabled instead of deleted so the audit trail survives.
func DeleteRedeemCodeForHost(ctx context.Context, hostID, id int64) (RedeemCodeDeleteOutcome, error) {
	if pool == nil {
		return RedeemCodeNotFound, errRedeemCodeDatabaseNotReady
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return RedeemCodeNotFound, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := AdvisoryTransactionLock(ctx, tx, RedeemCodeLockNamespace, int32(id)); err != nil {
		return RedeemCodeNotFound, err
	}

	var foundID int32
	err = tx.QueryRow(ctx, "SELECT id FROM redeem_codes WHERE id = $1 AND host_id = $2 LIMIT 1", id, hostID).Scan(&foundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RedeemCodeNotFound, nil
	}
	if err != nil {
		return RedeemCodeNotFound, err
	}

	var usageCount int64
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM redeem_code_usages WHERE redeem_code_id = $1", id).Scan(&usageCount); err != nil {
		return RedeemCodeNotFound, err
	}

	if usageCount > 0 {
		if _, err := tx.Exec(ctx, "UPDATE redeem_codes SET enabled = false WHERE id = $1 AND host_id = $2", id, hostID); err != nil {
			return RedeemCodeNotFound, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RedeemCodeNotFound, err
		}
		return RedeemCodeDisabled, nil
	}

	if _, err := tx.Exec(ctx, "DELETE FROM redeem_codes WHERE id = $1 AND host_id = $2", id, hostID); err != nil {
		return RedeemCodeNotFound, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RedeemCodeNotFound, err
	}
	return RedeemCodeDeleted, nil
}

// RedeemCodeBatchDeleteResult mirrors `{foundCount, deletedCount, disabledCount}`.
type RedeemCodeBatchDeleteResult struct {
	FoundCount    int `json:"foundCount"`
	DeletedCount  int `json:"deletedCount"`
	DisabledCount int `json:"disabledCount"`
}

// DeleteRedeemCodeBatchForHost mirrors deleteRedeemCodeBatchForHost. Locks are
// taken in ascending id order (deadlock avoidance), then used codes are disabled
// and unused ones deleted.
func DeleteRedeemCodeBatchForHost(ctx context.Context, hostID int64, ids []int64) (RedeemCodeBatchDeleteResult, error) {
	if len(ids) == 0 {
		return RedeemCodeBatchDeleteResult{}, nil
	}
	if pool == nil {
		return RedeemCodeBatchDeleteResult{}, errRedeemCodeDatabaseNotReady
	}

	sortedIDs := append([]int64(nil), ids...)
	sortInt64Ascending(sortedIDs)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return RedeemCodeBatchDeleteResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, id := range sortedIDs {
		if err := AdvisoryTransactionLock(ctx, tx, RedeemCodeLockNamespace, int32(id)); err != nil {
			return RedeemCodeBatchDeleteResult{}, err
		}
	}

	rows, err := tx.Query(ctx, "SELECT id FROM redeem_codes WHERE id = ANY($1) AND host_id = $2", sortedIDs, hostID)
	if err != nil {
		return RedeemCodeBatchDeleteResult{}, err
	}
	foundIDs := make([]int64, 0, len(sortedIDs))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return RedeemCodeBatchDeleteResult{}, err
		}
		foundIDs = append(foundIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RedeemCodeBatchDeleteResult{}, err
	}

	if len(foundIDs) == 0 {
		return RedeemCodeBatchDeleteResult{}, nil
	}

	usedRows, err := tx.Query(ctx,
		"SELECT DISTINCT redeem_code_id FROM redeem_code_usages WHERE redeem_code_id = ANY($1)", foundIDs)
	if err != nil {
		return RedeemCodeBatchDeleteResult{}, err
	}
	usedSet := map[int64]bool{}
	for usedRows.Next() {
		var id int64
		if err := usedRows.Scan(&id); err != nil {
			usedRows.Close()
			return RedeemCodeBatchDeleteResult{}, err
		}
		usedSet[id] = true
	}
	usedRows.Close()
	if err := usedRows.Err(); err != nil {
		return RedeemCodeBatchDeleteResult{}, err
	}

	usedIDs := make([]int64, 0, len(usedSet))
	unusedIDs := make([]int64, 0, len(foundIDs))
	for _, id := range foundIDs {
		if usedSet[id] {
			usedIDs = append(usedIDs, id)
			continue
		}
		unusedIDs = append(unusedIDs, id)
	}

	if len(usedIDs) > 0 {
		if _, err := tx.Exec(ctx,
			"UPDATE redeem_codes SET enabled = false WHERE id = ANY($1) AND host_id = $2", usedIDs, hostID); err != nil {
			return RedeemCodeBatchDeleteResult{}, err
		}
	}
	if len(unusedIDs) > 0 {
		if _, err := tx.Exec(ctx,
			"DELETE FROM redeem_codes WHERE id = ANY($1) AND host_id = $2", unusedIDs, hostID); err != nil {
			return RedeemCodeBatchDeleteResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return RedeemCodeBatchDeleteResult{}, err
	}

	return RedeemCodeBatchDeleteResult{
		FoundCount:    len(foundIDs),
		DeletedCount:  len(unusedIDs),
		DisabledCount: len(usedIDs),
	}, nil
}

func sortInt64Ascending(values []int64) {
	// Insertion sort: the slices are at most 100 entries and this avoids pulling
	// sort into the hot path of a transaction.
	for i := 1; i < len(values); i++ {
		current := values[i]
		j := i - 1
		for j >= 0 && values[j] > current {
			values[j+1] = values[j]
			j--
		}
		values[j+1] = current
	}
}

// RedeemCodeUsageItem mirrors the per-row projection of getRedeemCodeUsagesForHost.
type RedeemCodeUsageItem struct {
	ID       int32              `json:"id"`
	User     *RedeemCodeUserRef `json:"user"`
	Instance *RedeemCodeHostRef `json:"instance"`
	UsedAt   string             `json:"usedAt"`
}

// RedeemCodeUsageListResult mirrors `{usages, total}`.
type RedeemCodeUsageListResult struct {
	Usages []RedeemCodeUsageItem `json:"usages"`
	Total  int64                 `json:"total"`
}

// RedeemCodeUsageListOptions mirrors the `{limit, offset}` argument.
type RedeemCodeUsageListOptions struct {
	Limit  *int
	Offset *int
}

type redeemCodeUsageRow struct {
	ID           int32     `gorm:"column:id"`
	UsedAt       time.Time `gorm:"column:used_at"`
	UserRefID    *int32    `gorm:"column:user_ref_id"`
	UserUsername *string   `gorm:"column:user_username"`
	InstanceID   *int32    `gorm:"column:instance_ref_id"`
	InstanceName *string   `gorm:"column:instance_name"`
}

// GetRedeemCodeUsagesForHost mirrors getRedeemCodeUsagesForHost. The
// `redeemCode: { hostId }` relation filter is part of the WHERE clause, so usage
// rows can never leak across hosts.
func GetRedeemCodeUsagesForHost(ctx context.Context, hostID, redeemCodeID int64, options RedeemCodeUsageListOptions) (RedeemCodeUsageListResult, error) {
	limit, offset := clampRedeemCodeListBounds(options.Limit, options.Offset, 20)

	where := "WHERE u.redeem_code_id = ? AND rc.host_id = ?"
	args := []any{redeemCodeID, hostID}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM redeem_code_usages u JOIN redeem_codes rc ON rc.id = u.redeem_code_id "+where,
		args...).Scan(&total).Error; err != nil {
		return RedeemCodeUsageListResult{}, err
	}

	listArgs := append(append([]any{}, args...), limit, offset)
	listSQL := "SELECT u.id, u.used_at," +
		" usr.id AS user_ref_id, usr.username AS user_username," +
		" i.id AS instance_ref_id, i.name AS instance_name" +
		" FROM redeem_code_usages u" +
		" JOIN redeem_codes rc ON rc.id = u.redeem_code_id" +
		" LEFT JOIN users usr ON usr.id = u.user_id" +
		" LEFT JOIN instances i ON i.id = u.instance_id " +
		where + " ORDER BY u.used_at DESC LIMIT ? OFFSET ?"

	var rows []redeemCodeUsageRow
	if err := DB.WithContext(ctx).Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return RedeemCodeUsageListResult{}, err
	}

	items := make([]RedeemCodeUsageItem, 0, len(rows))
	for _, row := range rows {
		item := RedeemCodeUsageItem{ID: row.ID, UsedAt: redeemCodeISOTime(row.UsedAt)}
		if row.UserRefID != nil {
			item.User = &RedeemCodeUserRef{ID: *row.UserRefID, Username: redeemCodeDerefString(row.UserUsername)}
		}
		if row.InstanceID != nil {
			item.Instance = &RedeemCodeHostRef{ID: *row.InstanceID, Name: redeemCodeDerefString(row.InstanceName)}
		}
		items = append(items, item)
	}

	return RedeemCodeUsageListResult{Usages: items, Total: total}, nil
}

// IsRedeemCodeBelongsToHost mirrors isRedeemCodeBelongsToHost.
func IsRedeemCodeBelongsToHost(ctx context.Context, redeemCodeID, hostID int64) (bool, error) {
	var count int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM redeem_codes WHERE id = ? AND host_id = ?", redeemCodeID, hostID).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// IsInstanceBelongsToHost mirrors isInstanceBelongsToHost.
func IsInstanceBelongsToHost(ctx context.Context, instanceID, hostID int64) (bool, error) {
	var count int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM instances WHERE id = ? AND host_id = ?", instanceID, hostID).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetRedeemCodeHostByID is the stand-in for db/hosts.ts' getHostById (that module
// is not ported yet). Only the three columns the redeem-code routes need are
// selected. It is deliberately named after this module so it cannot collide with a
// future db.GetHostByID.
func GetRedeemCodeHostByID(ctx context.Context, hostID int64) (*models.Host, error) {
	var hosts []models.Host
	if err := DB.WithContext(ctx).
		Select("id", "name", "user_id").
		Where("id = ?", hostID).
		Limit(1).
		Find(&hosts).Error; err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	return &hosts[0], nil
}

// ==================== helpers ====================

// redeemCodeISOTime mirrors Date.prototype.toISOString().
func redeemCodeISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func redeemCodeOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := redeemCodeISOTime(*value)
	return &formatted
}

func redeemCodeCreatorRef(row redeemCodeListRow) *RedeemCodeUserRef {
	if row.CreatorID == nil {
		return nil
	}
	return &RedeemCodeUserRef{ID: *row.CreatorID, Username: redeemCodeDerefString(row.CreatorUsername)}
}

func redeemCodeDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func redeemCodeDerefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}
