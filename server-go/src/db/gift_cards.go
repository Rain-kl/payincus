// Port of server/src/db/gift-cards.ts.
//
// Money handling
// --------------
// The Prisma columns are Decimal(10,2). Every value that crosses the wire goes
// through `toMoney()` in the TypeScript, i.e. `Number(Number(value).toFixed(2))`,
// which means the *API* shape is a JSON **number** — not Prisma's decimal string.
// The Go port therefore:
//   - stores/reads amounts with models.Decimal (exact, no float drift in SQL),
//   - converts to float64 at the JSON boundary (moneyFloat), matching toMoney().
//
// Concurrency
// -----------
// Redemption and all refund paths are serialized with *transaction-scoped*
// PostgreSQL advisory locks on the same pgx.Tx as their mutations
// (db/advisory_locks.go). The order is fixed — card lock first, then user balance
// lock — mirroring the TypeScript, so two concurrent redemptions of the same code
// cannot both credit a balance: the second one re-reads the card after acquiring
// the lock and finds status <> 'active'.
//
// Every state transition also carries a `WHERE ... AND status = 'active'`
// predicate and checks the affected row count, so even an unlocked race degrades
// to GIFT_CARD_USED instead of double crediting.
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
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	// giftCardRandomLength mirrors GIFT_CARD_RANDOM_LENGTH.
	giftCardRandomLength = 24
	// GiftCardCodePrefix mirrors GIFT_CARD_CODE_PREFIX.
	GiftCardCodePrefix = "gc-"
	// GiftCardMaxBatchCount mirrors MAX_GIFT_CARD_BATCH_COUNT.
	GiftCardMaxBatchCount = 500
	// GiftCardMaxRemarkLength mirrors MAX_GIFT_CARD_REMARK_LENGTH.
	GiftCardMaxRemarkLength = 200
	// GiftCardMaxFaceValue mirrors MAX_GIFT_CARD_FACE_VALUE.
	GiftCardMaxFaceValue = 10000
	// GiftCardMaxBatchFaceValue mirrors MAX_GIFT_CARD_BATCH_FACE_VALUE.
	GiftCardMaxBatchFaceValue = 100000
	// GiftCardMaxListPageSize mirrors MAX_GIFT_CARD_LIST_PAGE_SIZE.
	GiftCardMaxListPageSize = 100

	// giftCardCodeTotalLength is len("gc-") + 24, the full code length.
	giftCardCodeTotalLength = 3 + giftCardRandomLength
)

// giftCardMaskThreshold / giftCardMaskHead / giftCardMaskTail mirror
// maskGiftCardCode: codes longer than 12 characters render as
// "<first 7>...<last 4>".
const (
	giftCardMaskThreshold = 12
	giftCardMaskHead      = 7
	giftCardMaskTail      = 4
)

// Gift card state-machine error codes (the route maps them to APIError codes).
var (
	ErrGiftCardNotFound   = errors.New("GIFT_CARD_NOT_FOUND")
	ErrGiftCardUsed       = errors.New("GIFT_CARD_USED")
	ErrGiftCardSelfRedeem = errors.New("GIFT_CARD_SELF_REDEEM")
	// ErrGiftCardAlreadyUsedByUser mirrors GIFT_CARD_ALREADY_USED_BY_USER. The
	// current TypeScript db layer never raises it, but mapGiftCardError has a
	// branch for it, so the sentinel exists to keep the mapping table 1:1.
	ErrGiftCardAlreadyUsedByUser   = errors.New("GIFT_CARD_ALREADY_USED_BY_USER")
	ErrGiftCardExpired             = errors.New("GIFT_CARD_EXPIRED")
	ErrGiftCardDisabled            = errors.New("GIFT_CARD_DISABLED")
	ErrGiftCardInsufficientBalance = errors.New("GIFT_CARD_INSUFFICIENT_BALANCE")
	ErrGiftCardBatchTooLarge       = errors.New("GIFT_CARD_BATCH_TOO_LARGE")
	ErrGiftCardBatchValueTooLarge  = errors.New("GIFT_CARD_BATCH_VALUE_TOO_LARGE")
	ErrGiftCardBalanceExceedsFace  = errors.New("GIFT_CARD_BALANCE_EXCEEDS_FACE_VALUE")
	ErrGiftCardNotIssuer           = errors.New("GIFT_CARD_NOT_ISSUER")
	ErrGiftCardCodeCollision       = errors.New("GIFT_CARD_CODE_COLLISION")
	ErrGiftCardUserNotFound        = errors.New("USER_NOT_FOUND")
)

// ==================== inputs / results ====================

// GiftCardCreateInput mirrors GiftCardCreateInput.
type GiftCardCreateInput struct {
	FaceValue    float64
	BalanceValue float64
	CreatedByID  *int32
	OwnerID      *int32
	ExpiresAt    *time.Time
	Remark       *string
	BatchID      *string
}

// GiftCardAdminListOptions mirrors GiftCardAdminListOptions. Page/PageSize 0
// means "undefined" (clamped to 1 / 20).
type GiftCardAdminListOptions struct {
	Page       int
	PageSize   int
	Status     models.GiftCardStatus
	BatchID    *string
	RevealCode bool
}

// GiftCardUserListOptions mirrors GiftCardUserListOptions.
type GiftCardUserListOptions struct {
	Page     int
	PageSize int
	Status   models.GiftCardStatus
}

// GiftCardBalanceResult mirrors GiftCardBalanceResult (all values are numbers).
type GiftCardBalanceResult struct {
	BalanceValue  float64 `json:"balanceValue"`
	BalanceBefore float64 `json:"balanceBefore"`
	BalanceAfter  float64 `json:"balanceAfter"`
}

// GiftCardUserRef mirrors the `{ select: { id, username } }` relation projection.
type GiftCardUserRef struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
}

// The TypeScript `serializeGiftCard` spreads the Prisma `select` result and then
// overwrites the two amount fields with numbers. Because each call site uses a
// different `select`, the JSON key set differs per endpoint — so each projection
// gets its own struct with explicit (non-omitempty) fields, which reproduces the
// spread's "null keys are present" behaviour.

// GiftCardCreateView mirrors serializeGiftCard(create-result, {revealCode:true}).
// Select: id, code, faceValue, balanceValue, status, expiresAt, batchId, createdAt.
type GiftCardCreateView struct {
	ID           int32                 `json:"id"`
	Code         string                `json:"code"`
	CodeMasked   string                `json:"codeMasked"`
	FaceValue    float64               `json:"faceValue"`
	BalanceValue float64               `json:"balanceValue"`
	Status       models.GiftCardStatus `json:"status"`
	ExpiresAt    *string               `json:"expiresAt"`
	BatchID      *string               `json:"batchId"`
	CreatedAt    string                `json:"createdAt"`
}

// GiftCardAdminView mirrors serializeGiftCard(admin-list row, {revealCode}).
// Select: id, code, faceValue, balanceValue, status, expiresAt, remark, batchId,
// createdAt, usedAt, createdBy, owner, usedBy.
type GiftCardAdminView struct {
	ID           int32                 `json:"id"`
	Code         string                `json:"code"`
	CodeMasked   string                `json:"codeMasked"`
	FaceValue    float64               `json:"faceValue"`
	BalanceValue float64               `json:"balanceValue"`
	Status       models.GiftCardStatus `json:"status"`
	ExpiresAt    *string               `json:"expiresAt"`
	Remark       *string               `json:"remark"`
	BatchID      *string               `json:"batchId"`
	CreatedAt    string                `json:"createdAt"`
	UsedAt       *string               `json:"usedAt"`
	CreatedBy    *GiftCardUserRef      `json:"createdBy"`
	Owner        *GiftCardUserRef      `json:"owner"`
	UsedBy       *GiftCardUserRef      `json:"usedBy"`
}

// GiftCardUserView mirrors serializeGiftCard(user-list row, {revealCode:true}).
// Select: id, code, faceValue, balanceValue, status, expiresAt, createdAt, usedAt.
type GiftCardUserView struct {
	ID           int32                 `json:"id"`
	Code         string                `json:"code"`
	CodeMasked   string                `json:"codeMasked"`
	FaceValue    float64               `json:"faceValue"`
	BalanceValue float64               `json:"balanceValue"`
	Status       models.GiftCardStatus `json:"status"`
	ExpiresAt    *string               `json:"expiresAt"`
	CreatedAt    string                `json:"createdAt"`
	UsedAt       *string               `json:"usedAt"`
}

// GiftCardDetailView mirrors findGiftCardByCode()'s select, which keeps the scalar
// owner/usage ids *and* the relation objects.
type GiftCardDetailView struct {
	ID           int32                 `json:"id"`
	Code         string                `json:"code"`
	CodeMasked   string                `json:"codeMasked"`
	FaceValue    float64               `json:"faceValue"`
	BalanceValue float64               `json:"balanceValue"`
	Status       models.GiftCardStatus `json:"status"`
	CreatedByID  *int32                `json:"createdById"`
	OwnerID      *int32                `json:"ownerId"`
	UsedByID     *int32                `json:"usedById"`
	UsedAt       *string               `json:"usedAt"`
	ExpiresAt    *string               `json:"expiresAt"`
	Remark       *string               `json:"remark"`
	BatchID      *string               `json:"batchId"`
	CreatedAt    string                `json:"createdAt"`
	CreatedBy    *GiftCardUserRef      `json:"createdBy"`
	Owner        *GiftCardUserRef      `json:"owner"`
	UsedBy       *GiftCardUserRef      `json:"usedBy"`
}

// GiftCardAdminPage mirrors listGiftCardsByAdmin's return shape.
type GiftCardAdminPage struct {
	Records  []GiftCardAdminView `json:"records"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"pageSize"`
}

// GiftCardUserPage mirrors listGiftCardsByUser's return shape.
type GiftCardUserPage struct {
	Records  []GiftCardUserView `json:"records"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"pageSize"`
}

// GiftCardCreateResult mirrors createGiftCardBatch's `{ codes, batchId, count }`.
type GiftCardCreateResult struct {
	Codes   []GiftCardCreateView `json:"codes"`
	BatchID string               `json:"batchId"`
	Count   int                  `json:"count"`
}

// GiftCardGenerateFromBalanceResult mirrors generateGiftCardFromBalance's result.
// BalanceLog is the raw created row (its Decimal fields stay JSON strings, exactly
// like Prisma returns them).
type GiftCardGenerateFromBalanceResult struct {
	GiftCard      GiftCardCreateView `json:"giftCard"`
	BalanceLog    models.BalanceLog  `json:"balanceLog"`
	BalanceBefore float64            `json:"balanceBefore"`
	BalanceAfter  float64            `json:"balanceAfter"`
}

// GiftCardRevokeResult mirrors revokeGiftCard's `{ success, balanceAfter }`
// (balanceAfter is null when the card was not user-funded).
type GiftCardRevokeResult struct {
	Success      bool     `json:"success"`
	BalanceAfter *float64 `json:"balanceAfter"`
}

// GiftCardMutationCount mirrors the Prisma `{ count }` results.
type GiftCardMutationCount struct {
	Count int64 `json:"count"`
}

// GiftCardStats mirrors getGiftCardStats().
type GiftCardStats struct {
	Total              int64   `json:"total"`
	Active             int64   `json:"active"`
	Used               int64   `json:"used"`
	Disabled           int64   `json:"disabled"`
	Expired            int64   `json:"expired"`
	OutstandingValue   float64 `json:"outstandingValue"`
	TotalRedeemedValue float64 `json:"totalRedeemedValue"`
}

// ==================== helpers ====================

// moneyFloat mirrors toMoney(): Number(x).toFixed(2) then back to a number.
func moneyFloat(value decimal.Decimal) float64 {
	rounded := value.Round(2)
	result, _ := rounded.Float64()
	return result
}

// toDecimal mirrors toDecimal(value): `new Prisma.Decimal(value.toFixed(2))`.
func toDecimal(value float64) models.Decimal {
	return models.Decimal{Decimal: decimal.NewFromFloat(value).Round(2)}
}

// giftCardMaskCode mirrors maskGiftCardCode.
func giftCardMaskCode(code string) string {
	if len(code) <= giftCardMaskThreshold {
		return code
	}
	return code[:giftCardMaskHead] + "..." + code[len(code)-giftCardMaskTail:]
}

// generateGiftCardCode mirrors generateGiftCardCode(): "gc-" + the first 24
// characters of base64url(18 random bytes).
func generateGiftCardCode() (string, error) {
	buffer := make([]byte, 18)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(buffer)
	return GiftCardCodePrefix + encoded[:giftCardRandomLength], nil
}

// generateBatchID mirrors generateBatchId(): hex(8 random bytes).
func generateBatchID() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// clampGiftCardPage mirrors clampPage.
func clampGiftCardPage(value int) int {
	if value > 0 {
		return value
	}
	return 1
}

// clampGiftCardPageSize mirrors clampPageSize (20 default, capped at 100).
func clampGiftCardPageSize(value int) int {
	if value == 0 {
		return 20
	}
	if value < 1 {
		return 1
	}
	if value > GiftCardMaxListPageSize {
		return GiftCardMaxListPageSize
	}
	return value
}

// isGiftCardUniqueViolation reports whether err is the PostgreSQL unique
// constraint violation Prisma surfaces as P2002 (23505).
func isGiftCardUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate key value")
}

// giftCardRow is the raw SQL projection. Amounts stay decimal; timestamps are
// converted to ISO strings at mapping time.
type giftCardRow struct {
	ID           int32          `gorm:"column:id"`
	Code         string         `gorm:"column:code"`
	FaceValue    models.Decimal `gorm:"column:face_value"`
	BalanceValue models.Decimal `gorm:"column:balance_value"`
	Status       string         `gorm:"column:status"`
	CreatedByID  *int32         `gorm:"column:created_by_id"`
	OwnerID      *int32         `gorm:"column:owner_id"`
	UsedByID     *int32         `gorm:"column:used_by_id"`
	UsedAt       *time.Time     `gorm:"column:used_at"`
	ExpiresAt    *time.Time     `gorm:"column:expires_at"`
	Remark       *string        `gorm:"column:remark"`
	BatchID      *string        `gorm:"column:batch_id"`
	CreatedAt    time.Time      `gorm:"column:created_at"`

	CreatedByUsername *string `gorm:"column:created_by_username"`
	OwnerUsername     *string `gorm:"column:owner_username"`
	UsedByUsername    *string `gorm:"column:used_by_username"`
}

// giftCardBasicColumns is the projection used by the mutation paths.
const giftCardBasicColumns = `
	id, code, face_value, balance_value, status::text AS status,
	created_by_id, owner_id, used_by_id, used_at, expires_at, remark, batch_id, created_at`

// giftCardBasicColumnsAliased is the same list with the "g." table alias used by
// the joined selects.
const giftCardBasicColumnsAliased = `
	g.id, g.code, g.face_value, g.balance_value, g.status::text AS status,
	g.created_by_id, g.owner_id, g.used_by_id, g.used_at, g.expires_at, g.remark,
	g.batch_id, g.created_at`

// giftCardRelationColumns is the projection with the three user relations.
const giftCardRelationColumns = giftCardBasicColumns + `,
	cb.username AS created_by_username,
	ow.username AS owner_username,
	ub.username AS used_by_username`

const giftCardRelationColumnsAliased = giftCardBasicColumnsAliased + `,
	cb.username AS created_by_username,
	ow.username AS owner_username,
	ub.username AS used_by_username`

const giftCardRelationFrom = ` FROM gift_cards g
	LEFT JOIN users cb ON cb.id = g.created_by_id
	LEFT JOIN users ow ON ow.id = g.owner_id
	LEFT JOIN users ub ON ub.id = g.used_by_id `

func giftCardTimestampPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := isoTimestamp(*value)
	return &formatted
}

// serializeGiftCardCreate mirrors serializeGiftCard(row, {revealCode:true}) for the
// create/batch projection.
func serializeGiftCardCreate(row giftCardRow) GiftCardCreateView {
	return GiftCardCreateView{
		ID:           row.ID,
		Code:         row.Code,
		CodeMasked:   giftCardMaskCode(row.Code),
		FaceValue:    moneyFloat(row.FaceValue.Decimal),
		BalanceValue: moneyFloat(row.BalanceValue.Decimal),
		Status:       models.GiftCardStatus(row.Status),
		ExpiresAt:    giftCardTimestampPtr(row.ExpiresAt),
		BatchID:      row.BatchID,
		CreatedAt:    isoTimestamp(row.CreatedAt),
	}
}

// serializeGiftCardAdmin mirrors serializeGiftCard(row, {revealCode}) for the
// admin list projection.
func serializeGiftCardAdmin(row giftCardRow, revealCode bool) GiftCardAdminView {
	code := row.Code
	if !revealCode {
		code = giftCardMaskCode(code)
	}
	view := GiftCardAdminView{
		ID:           row.ID,
		Code:         code,
		CodeMasked:   giftCardMaskCode(row.Code),
		FaceValue:    moneyFloat(row.FaceValue.Decimal),
		BalanceValue: moneyFloat(row.BalanceValue.Decimal),
		Status:       models.GiftCardStatus(row.Status),
		ExpiresAt:    giftCardTimestampPtr(row.ExpiresAt),
		Remark:       row.Remark,
		BatchID:      row.BatchID,
		CreatedAt:    isoTimestamp(row.CreatedAt),
		UsedAt:       giftCardTimestampPtr(row.UsedAt),
	}
	if row.CreatedByID != nil && row.CreatedByUsername != nil {
		view.CreatedBy = &GiftCardUserRef{ID: *row.CreatedByID, Username: *row.CreatedByUsername}
	}
	if row.OwnerID != nil && row.OwnerUsername != nil {
		view.Owner = &GiftCardUserRef{ID: *row.OwnerID, Username: *row.OwnerUsername}
	}
	if row.UsedByID != nil && row.UsedByUsername != nil {
		view.UsedBy = &GiftCardUserRef{ID: *row.UsedByID, Username: *row.UsedByUsername}
	}
	return view
}

// serializeGiftCardUser mirrors serializeGiftCard(row, {revealCode:true}) for the
// user list projection.
func serializeGiftCardUser(row giftCardRow) GiftCardUserView {
	return GiftCardUserView{
		ID:           row.ID,
		Code:         row.Code,
		CodeMasked:   giftCardMaskCode(row.Code),
		FaceValue:    moneyFloat(row.FaceValue.Decimal),
		BalanceValue: moneyFloat(row.BalanceValue.Decimal),
		Status:       models.GiftCardStatus(row.Status),
		ExpiresAt:    giftCardTimestampPtr(row.ExpiresAt),
		CreatedAt:    isoTimestamp(row.CreatedAt),
		UsedAt:       giftCardTimestampPtr(row.UsedAt),
	}
}

// serializeGiftCardDetail mirrors findGiftCardByCode's serialize(row,
// {revealCode:true}) with the full scalar + relation projection.
func serializeGiftCardDetail(row giftCardRow) GiftCardDetailView {
	view := GiftCardDetailView{
		ID:           row.ID,
		Code:         row.Code,
		CodeMasked:   giftCardMaskCode(row.Code),
		FaceValue:    moneyFloat(row.FaceValue.Decimal),
		BalanceValue: moneyFloat(row.BalanceValue.Decimal),
		Status:       models.GiftCardStatus(row.Status),
		CreatedByID:  row.CreatedByID,
		OwnerID:      row.OwnerID,
		UsedByID:     row.UsedByID,
		UsedAt:       giftCardTimestampPtr(row.UsedAt),
		ExpiresAt:    giftCardTimestampPtr(row.ExpiresAt),
		Remark:       row.Remark,
		BatchID:      row.BatchID,
		CreatedAt:    isoTimestamp(row.CreatedAt),
	}
	if row.CreatedByID != nil && row.CreatedByUsername != nil {
		view.CreatedBy = &GiftCardUserRef{ID: *row.CreatedByID, Username: *row.CreatedByUsername}
	}
	if row.OwnerID != nil && row.OwnerUsername != nil {
		view.Owner = &GiftCardUserRef{ID: *row.OwnerID, Username: *row.OwnerUsername}
	}
	if row.UsedByID != nil && row.UsedByUsername != nil {
		view.UsedBy = &GiftCardUserRef{ID: *row.UsedByID, Username: *row.UsedByUsername}
	}
	return view
}

// beginGiftCardTx starts the pgx transaction the advisory locks require.
func beginGiftCardTx(ctx context.Context) (pgx.Tx, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return pool.Begin(ctx)
}

// ==================== create ====================

// validateGiftCardAmounts mirrors the guard at the top of
// createGiftCardInTransaction: both values must be positive and the credited
// balance may not exceed the face value.
func validateGiftCardAmounts(faceValue, balanceValue decimal.Decimal) error {
	if faceValue.LessThanOrEqual(decimal.Zero) ||
		balanceValue.LessThanOrEqual(decimal.Zero) ||
		balanceValue.GreaterThan(faceValue) {
		return ErrGiftCardBalanceExceedsFace
	}
	return nil
}

// createGiftCardInTx mirrors createGiftCardInTransaction.
func createGiftCardInTx(ctx context.Context, tx pgx.Tx, input GiftCardCreateInput) (giftCardRow, error) {
	faceValue := decimal.NewFromFloat(input.FaceValue).Round(2)
	balanceValue := decimal.NewFromFloat(input.BalanceValue).Round(2)
	if err := validateGiftCardAmounts(faceValue, balanceValue); err != nil {
		return giftCardRow{}, err
	}

	code, err := generateGiftCardCode()
	if err != nil {
		return giftCardRow{}, err
	}

	var row giftCardRow
	err = tx.QueryRow(ctx,
		`INSERT INTO gift_cards
			(code, face_value, balance_value, status, created_by_id, owner_id, expires_at, remark, batch_id, created_at)
		 VALUES ($1, $2, $3, 'active', $4, $5, $6, $7, $8, NOW())
		 RETURNING `+giftCardBasicColumns,
		code, models.Decimal{Decimal: faceValue}, models.Decimal{Decimal: balanceValue},
		input.CreatedByID, input.OwnerID, input.ExpiresAt, input.Remark, input.BatchID).
		Scan(&row.ID, &row.Code, &row.FaceValue, &row.BalanceValue, &row.Status,
			&row.CreatedByID, &row.OwnerID, &row.UsedByID, &row.UsedAt, &row.ExpiresAt,
			&row.Remark, &row.BatchID, &row.CreatedAt)
	if err != nil {
		return giftCardRow{}, err
	}
	return row, nil
}

// CreateGiftCard mirrors createGiftCard(): retry up to 3 times on a code
// collision, then GIFT_CARD_CODE_COLLISION.
func CreateGiftCard(ctx context.Context, input GiftCardCreateInput) (GiftCardCreateView, error) {
	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		tx, err := beginGiftCardTx(ctx)
		if err != nil {
			return GiftCardCreateView{}, err
		}
		row, err := createGiftCardInTx(ctx, tx, input)
		if err != nil {
			_ = tx.Rollback(ctx)
			if isGiftCardUniqueViolation(err) && attempt < attempts-1 {
				lastErr = err
				continue
			}
			return GiftCardCreateView{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GiftCardCreateView{}, err
		}
		return serializeGiftCardCreate(row), nil
	}
	_ = lastErr
	return GiftCardCreateView{}, ErrGiftCardCodeCollision
}

// CreateGiftCardBatch mirrors createGiftCardBatch().
func CreateGiftCardBatch(ctx context.Context, count int, input GiftCardCreateInput) (GiftCardCreateResult, error) {
	if count < 1 || count > GiftCardMaxBatchCount {
		return GiftCardCreateResult{}, ErrGiftCardBatchTooLarge
	}
	totalFaceValue := decimal.NewFromFloat(input.FaceValue).Round(2).
		Mul(decimal.NewFromInt(int64(count)))
	if totalFaceValue.GreaterThan(decimal.NewFromInt(GiftCardMaxBatchFaceValue)) {
		return GiftCardCreateResult{}, ErrGiftCardBatchValueTooLarge
	}

	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := createGiftCardBatchOnce(ctx, count, input)
		if err == nil {
			return result, nil
		}
		if !isGiftCardUniqueViolation(err) || attempt == attempts-1 {
			if isGiftCardUniqueViolation(err) {
				return GiftCardCreateResult{}, ErrGiftCardCodeCollision
			}
			return GiftCardCreateResult{}, err
		}
		lastErr = err
	}
	if lastErr != nil {
		return GiftCardCreateResult{}, ErrGiftCardCodeCollision
	}
	return GiftCardCreateResult{}, ErrGiftCardCodeCollision
}

func createGiftCardBatchOnce(ctx context.Context, count int, input GiftCardCreateInput) (GiftCardCreateResult, error) {
	tx, err := beginGiftCardTx(ctx)
	if err != nil {
		return GiftCardCreateResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batchID, err := generateBatchID()
	if err != nil {
		return GiftCardCreateResult{}, err
	}

	codes := make([]GiftCardCreateView, 0, count)
	for i := 0; i < count; i++ {
		scoped := input
		scoped.BatchID = &batchID
		row, err := createGiftCardInTx(ctx, tx, scoped)
		if err != nil {
			return GiftCardCreateResult{}, err
		}
		codes = append(codes, serializeGiftCardCreate(row))
	}

	if err := tx.Commit(ctx); err != nil {
		return GiftCardCreateResult{}, err
	}
	return GiftCardCreateResult{Codes: codes, BatchID: batchID, Count: len(codes)}, nil
}

// ==================== generate from balance ====================

// GenerateGiftCardFromBalance mirrors generateGiftCardFromBalance(): deduct the
// user's balance (guarded UPDATE + gift_card_issue ledger row) and issue a card of
// the same value, all inside one transaction under the user-balance advisory lock.
func GenerateGiftCardFromBalance(ctx context.Context, userID int32, faceValue float64, remark *string) (GiftCardGenerateFromBalanceResult, error) {
	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := generateGiftCardFromBalanceOnce(ctx, userID, faceValue, remark)
		if err == nil {
			return result, nil
		}
		if !isGiftCardUniqueViolation(err) || attempt == attempts-1 {
			if isGiftCardUniqueViolation(err) {
				return GiftCardGenerateFromBalanceResult{}, ErrGiftCardCodeCollision
			}
			return GiftCardGenerateFromBalanceResult{}, err
		}
		lastErr = err
	}
	_ = lastErr
	return GiftCardGenerateFromBalanceResult{}, ErrGiftCardCodeCollision
}

func generateGiftCardFromBalanceOnce(ctx context.Context, userID int32, faceValue float64, remark *string) (GiftCardGenerateFromBalanceResult, error) {
	tx, err := beginGiftCardTx(ctx)
	if err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 用户余额锁：与 changeBalance 共用同一 namespace，避免与充值/消费交叉。
	if err := AdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID); err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}

	var balanceBefore models.Decimal
	if err := tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", userID).Scan(&balanceBefore); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GiftCardGenerateFromBalanceResult{}, ErrGiftCardUserNotFound
		}
		return GiftCardGenerateFromBalanceResult{}, err
	}

	amount := decimal.NewFromFloat(faceValue).Round(2)

	// 条件扣款：余额不足时 0 行受影响，转成 GIFT_CARD_INSUFFICIENT_BALANCE。
	tag, err := tx.Exec(ctx,
		"UPDATE users SET balance = balance - $1, updated_at = NOW() WHERE id = $2 AND balance >= $1",
		models.Decimal{Decimal: amount}, userID)
	if err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return GiftCardGenerateFromBalanceResult{}, ErrGiftCardInsufficientBalance
	}

	var balanceAfter models.Decimal
	if err := tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", userID).Scan(&balanceAfter); err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}

	// 发行扣款用独立 ledger 类型，避免计入 getUsersTotalConsumeMap 的消费额。
	balanceRemark := "生成礼品卡"
	if remark != nil && *remark != "" {
		balanceRemark = *remark
	}
	balanceLog, err := insertBalanceLog(ctx, tx, balanceLogInsert{
		userID:        userID,
		logType:       models.BalanceLogTypeGiftCardIssue,
		amount:        models.Decimal{Decimal: amount.Neg()},
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		remark:        &balanceRemark,
	})
	if err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}

	cardRemark := "用户余额生成礼品卡"
	if remark != nil && *remark != "" {
		cardRemark = *remark
	}
	createdBy := userID
	owner := userID
	row, err := createGiftCardInTx(ctx, tx, GiftCardCreateInput{
		FaceValue:    faceValue,
		BalanceValue: faceValue,
		CreatedByID:  &createdBy,
		OwnerID:      &owner,
		Remark:       &cardRemark,
	})
	if err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return GiftCardGenerateFromBalanceResult{}, err
	}

	return GiftCardGenerateFromBalanceResult{
		GiftCard:      serializeGiftCardCreate(row),
		BalanceLog:    balanceLog,
		BalanceBefore: moneyFloat(balanceBefore.Decimal),
		BalanceAfter:  moneyFloat(balanceAfter.Decimal),
	}, nil
}

// ==================== read ====================

// FindGiftCardByCode mirrors findGiftCardByCode() (always reveals the code).
func FindGiftCardByCode(ctx context.Context, code string) (*GiftCardDetailView, error) {
	rows, err := queryGiftCardRows(ctx,
		"SELECT "+giftCardRelationColumnsAliased+giftCardRelationFrom+
			" WHERE g.code = $1 LIMIT 1", code)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	detail := serializeGiftCardDetail(rows[0])
	return &detail, nil
}

// FindGiftCardByID mirrors findGiftCardById() (no relation fields).
func FindGiftCardByID(ctx context.Context, id int32, revealCode bool) (*GiftCardAdminView, error) {
	rows, err := queryGiftCardRows(ctx,
		"SELECT "+giftCardBasicColumns+" FROM gift_cards WHERE id = $1 LIMIT 1", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	view := serializeGiftCardAdmin(rows[0], revealCode)
	return &view, nil
}

func queryGiftCardRows(ctx context.Context, sql string, args ...any) ([]giftCardRow, error) {
	var rows []giftCardRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ==================== redeem ====================

// RedeemGiftCard mirrors redeemGiftCard().
//
// Lock order (matching the TypeScript): card lock, then user-balance lock. Both
// are transaction-scoped, so the whole read-check-credit sequence is serialized
// against a concurrent redeem of the same card *and* against any other balance
// mutation of the same user.
func RedeemGiftCard(ctx context.Context, code string, userID int32) (GiftCardBalanceResult, error) {
	tx, err := beginGiftCardTx(ctx)
	if err != nil {
		return GiftCardBalanceResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 先取卡 id（未加锁）：与 TS 一致，用于确定锁的 key。
	var cardID int32
	err = tx.QueryRow(ctx, "SELECT id FROM gift_cards WHERE code = $1", code).Scan(&cardID)
	if errors.Is(err, pgx.ErrNoRows) {
		return GiftCardBalanceResult{}, ErrGiftCardNotFound
	}
	if err != nil {
		return GiftCardBalanceResult{}, err
	}

	// 锁顺序固定：卡先、余额后。
	if err := AdvisoryTransactionLock(ctx, tx, GiftCardLockNamespace, cardID); err != nil {
		return GiftCardBalanceResult{}, err
	}
	if err := AdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID); err != nil {
		return GiftCardBalanceResult{}, err
	}

	// 加锁后重读：并发输家在这里看到 status <> 'active'。
	var (
		status       string
		balanceValue models.Decimal
		expiresAt    *time.Time
		ownerID      *int32
		cardCode     string
	)
	err = tx.QueryRow(ctx,
		"SELECT code, status::text, balance_value, expires_at, owner_id"+
			" FROM gift_cards WHERE id = $1", cardID).
		Scan(&cardCode, &status, &balanceValue, &expiresAt, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return GiftCardBalanceResult{}, ErrGiftCardNotFound
	}
	if err != nil {
		return GiftCardBalanceResult{}, err
	}

	if status != string(models.GiftCardStatusActive) {
		if status == string(models.GiftCardStatusUsed) {
			return GiftCardBalanceResult{}, ErrGiftCardUsed
		}
		return GiftCardBalanceResult{}, ErrGiftCardDisabled
	}
	// 不允许兑换自己用余额生成的卡
	if ownerID != nil && *ownerID == userID {
		return GiftCardBalanceResult{}, ErrGiftCardSelfRedeem
	}
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		expired, err := tx.Exec(ctx,
			"UPDATE gift_cards SET status = 'expired' WHERE id = $1 AND status = 'active'", cardID)
		if err != nil {
			return GiftCardBalanceResult{}, err
		}
		if expired.RowsAffected() == 0 {
			return GiftCardBalanceResult{}, ErrGiftCardUsed
		}
		// 过期是正常业务结果，需要提交状态变更后再报错。
		if err := tx.Commit(ctx); err != nil {
			return GiftCardBalanceResult{}, err
		}
		return GiftCardBalanceResult{}, ErrGiftCardExpired
	}

	var balanceBefore models.Decimal
	if err := tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", userID).Scan(&balanceBefore); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GiftCardBalanceResult{}, ErrGiftCardUserNotFound
		}
		return GiftCardBalanceResult{}, err
	}

	amount := balanceValue.Decimal.Round(2)

	// 原子认领：只有仍为 active 才能置为 used。
	claimed, err := tx.Exec(ctx,
		"UPDATE gift_cards SET status = 'used', used_by_id = $2, used_at = NOW()"+
			" WHERE id = $1 AND status = 'active'", cardID, userID)
	if err != nil {
		return GiftCardBalanceResult{}, err
	}
	if claimed.RowsAffected() != 1 {
		return GiftCardBalanceResult{}, ErrGiftCardUsed
	}

	var balanceAfter models.Decimal
	if err := tx.QueryRow(ctx,
		"UPDATE users SET balance = balance + $1, updated_at = NOW() WHERE id = $2 RETURNING balance",
		models.Decimal{Decimal: amount}, userID).Scan(&balanceAfter); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GiftCardBalanceResult{}, ErrGiftCardUserNotFound
		}
		return GiftCardBalanceResult{}, err
	}

	logRemark := "礼品卡兑换：" + giftCardMaskCode(cardCode)
	if _, err := insertBalanceLog(ctx, tx, balanceLogInsert{
		userID:        userID,
		logType:       models.BalanceLogTypeGift,
		amount:        models.Decimal{Decimal: amount},
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		remark:        &logRemark,
	}); err != nil {
		return GiftCardBalanceResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return GiftCardBalanceResult{}, err
	}

	return GiftCardBalanceResult{
		BalanceValue:  moneyFloat(amount),
		BalanceBefore: moneyFloat(balanceBefore.Decimal),
		BalanceAfter:  moneyFloat(balanceAfter.Decimal),
	}, nil
}

// ==================== refund ====================

// giftCardRefundRemark mirrors giftCardRefundRemark(id). The marker is what makes
// the refund idempotent.
func giftCardRefundRemark(id int32) string {
	return fmt.Sprintf("Gift card cancellation refund [gift-card-refund:%d]", id)
}

// refundGiftCardRecord is the subset of the card the refund path needs.
type refundGiftCardRecord struct {
	ID          int32
	FaceValue   models.Decimal
	CreatedByID *int32
	OwnerID     *int32
	Status      string
}

// refundUserFundedGiftCard mirrors refundUserFundedGiftCard(): refund the face
// value to the issuer exactly once, keyed by the deterministic ledger remark.
// Returns nil when the card was not user-funded (nothing to refund).
func refundUserFundedGiftCard(ctx context.Context, tx pgx.Tx, card refundGiftCardRecord) (*float64, error) {
	issuerID := card.OwnerID
	// 只有「用户用余额生成」的卡才退款：ownerId 存在且等于 createdById。
	if issuerID == nil || card.CreatedByID == nil || *card.CreatedByID != *issuerID {
		return nil, nil
	}

	if err := AdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, *issuerID); err != nil {
		return nil, err
	}

	remark := giftCardRefundRemark(card.ID)

	// 幂等：已有同 remark 的 refund 流水则直接返回当前余额。
	var existingLogID int32
	err := tx.QueryRow(ctx,
		"SELECT id FROM balance_logs WHERE user_id = $1 AND type = 'refund' AND remark = $2 LIMIT 1",
		*issuerID, remark).Scan(&existingLogID)
	if err == nil {
		var current models.Decimal
		if err := tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", *issuerID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		balance := moneyFloat(current.Decimal)
		return &balance, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var balanceBefore models.Decimal
	if err := tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", *issuerID).Scan(&balanceBefore); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrGiftCardUserNotFound
		}
		return nil, err
	}

	amount := card.FaceValue.Decimal.Round(2)

	// 卡片状态变更、余额变动、退款流水必须在同一事务内完成。
	var balanceAfter models.Decimal
	if err := tx.QueryRow(ctx,
		"UPDATE users SET balance = balance + $1, updated_at = NOW() WHERE id = $2 RETURNING balance",
		models.Decimal{Decimal: amount}, *issuerID).Scan(&balanceAfter); err != nil {
		return nil, err
	}

	if _, err := insertBalanceLog(ctx, tx, balanceLogInsert{
		userID:        *issuerID,
		logType:       models.BalanceLogTypeRefund,
		amount:        models.Decimal{Decimal: amount},
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		remark:        &remark,
	}); err != nil {
		return nil, err
	}

	balance := moneyFloat(balanceAfter.Decimal)
	return &balance, nil
}

// lockAndReadGiftCard takes the card advisory lock and re-reads the row.
// It returns (nil, nil) when the card disappeared between the initial probe and
// the lock.
func lockAndReadGiftCard(ctx context.Context, tx pgx.Tx, id int32) (*refundGiftCardRecord, error) {
	if err := AdvisoryTransactionLock(ctx, tx, GiftCardLockNamespace, id); err != nil {
		return nil, err
	}
	var record refundGiftCardRecord
	err := tx.QueryRow(ctx,
		"SELECT id, status::text, face_value, created_by_id, owner_id FROM gift_cards WHERE id = $1", id).
		Scan(&record.ID, &record.Status, &record.FaceValue, &record.CreatedByID, &record.OwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// disableGiftCardWithRefund mirrors disableGiftCardWithRefund.
func disableGiftCardWithRefund(ctx context.Context, id int32) (GiftCardMutationCount, error) {
	tx, err := beginGiftCardTx(ctx)
	if err != nil {
		return GiftCardMutationCount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var probe int32
	if err := tx.QueryRow(ctx, "SELECT id FROM gift_cards WHERE id = $1", id).Scan(&probe); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GiftCardMutationCount{Count: 0}, nil
		}
		return GiftCardMutationCount{}, err
	}

	card, err := lockAndReadGiftCard(ctx, tx, id)
	if err != nil {
		return GiftCardMutationCount{}, err
	}
	if card == nil || card.Status != string(models.GiftCardStatusActive) {
		return GiftCardMutationCount{Count: 0}, nil
	}

	if _, err := refundUserFundedGiftCard(ctx, tx, *card); err != nil {
		return GiftCardMutationCount{}, err
	}

	updated, err := tx.Exec(ctx,
		"UPDATE gift_cards SET status = 'disabled' WHERE id = $1 AND status = 'active'", id)
	if err != nil {
		return GiftCardMutationCount{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return GiftCardMutationCount{}, err
	}
	return GiftCardMutationCount{Count: updated.RowsAffected()}, nil
}

// deleteGiftCardWithRefund mirrors deleteGiftCardWithRefund.
func deleteGiftCardWithRefund(ctx context.Context, id int32) (GiftCardMutationCount, error) {
	tx, err := beginGiftCardTx(ctx)
	if err != nil {
		return GiftCardMutationCount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var probe int32
	if err := tx.QueryRow(ctx, "SELECT id FROM gift_cards WHERE id = $1", id).Scan(&probe); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GiftCardMutationCount{Count: 0}, nil
		}
		return GiftCardMutationCount{}, err
	}

	card, err := lockAndReadGiftCard(ctx, tx, id)
	if err != nil {
		return GiftCardMutationCount{}, err
	}
	if card == nil || card.Status == string(models.GiftCardStatusUsed) {
		return GiftCardMutationCount{Count: 0}, nil
	}

	if _, err := refundUserFundedGiftCard(ctx, tx, *card); err != nil {
		return GiftCardMutationCount{}, err
	}

	deleted, err := tx.Exec(ctx,
		"DELETE FROM gift_cards WHERE id = $1 AND status IN ('active', 'disabled', 'expired')", id)
	if err != nil {
		return GiftCardMutationCount{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return GiftCardMutationCount{}, err
	}
	return GiftCardMutationCount{Count: deleted.RowsAffected()}, nil
}

// RevokeGiftCard mirrors revokeGiftCard(): only the issuer may revoke, only while
// the card is unredeemed; the face value is refunded exactly once.
func RevokeGiftCard(ctx context.Context, id int32, issuerID int32) (GiftCardRevokeResult, error) {
	tx, err := beginGiftCardTx(ctx)
	if err != nil {
		return GiftCardRevokeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var probe int32
	if err := tx.QueryRow(ctx, "SELECT id FROM gift_cards WHERE id = $1", id).Scan(&probe); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GiftCardRevokeResult{}, ErrGiftCardNotFound
		}
		return GiftCardRevokeResult{}, err
	}

	card, err := lockAndReadGiftCard(ctx, tx, id)
	if err != nil {
		return GiftCardRevokeResult{}, err
	}
	if card == nil {
		return GiftCardRevokeResult{}, ErrGiftCardNotFound
	}
	if card.CreatedByID == nil || card.OwnerID == nil ||
		*card.CreatedByID != issuerID || *card.OwnerID != issuerID {
		return GiftCardRevokeResult{}, ErrGiftCardNotIssuer
	}
	if card.Status == string(models.GiftCardStatusUsed) {
		return GiftCardRevokeResult{}, ErrGiftCardUsed
	}

	balanceAfter, err := refundUserFundedGiftCard(ctx, tx, *card)
	if err != nil {
		return GiftCardRevokeResult{}, err
	}

	if card.Status != string(models.GiftCardStatusDisabled) {
		if _, err := tx.Exec(ctx,
			"UPDATE gift_cards SET status = 'disabled' WHERE id = $1", id); err != nil {
			return GiftCardRevokeResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return GiftCardRevokeResult{}, err
	}
	return GiftCardRevokeResult{Success: true, BalanceAfter: balanceAfter}, nil
}

// DisableGiftCard mirrors disableGiftCard (delegates to the refunding variant).
func DisableGiftCard(ctx context.Context, id int32) (GiftCardMutationCount, error) {
	return disableGiftCardWithRefund(ctx, id)
}

// EnableGiftCard mirrors enableGiftCard(): only unowned, still-valid, currently
// disabled cards can be re-enabled — so a refunded (user-funded) card can never
// come back to life.
func EnableGiftCard(ctx context.Context, id int32) (GiftCardMutationCount, error) {
	result := DB.WithContext(ctx).Exec(
		`UPDATE gift_cards SET status = 'active'
		 WHERE id = ? AND status = 'disabled' AND owner_id IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW())`, id)
	if result.Error != nil {
		return GiftCardMutationCount{}, result.Error
	}
	return GiftCardMutationCount{Count: result.RowsAffected}, nil
}

// DeleteGiftCard mirrors deleteGiftCard (delegates to the refunding variant).
func DeleteGiftCard(ctx context.Context, id int32) (GiftCardMutationCount, error) {
	return deleteGiftCardWithRefund(ctx, id)
}

// BatchDisableGiftCards mirrors batchDisableGiftCards: ids are processed in
// ascending order so concurrent batches take locks in the same order.
func BatchDisableGiftCards(ctx context.Context, ids []int32) (GiftCardMutationCount, error) {
	sorted := append([]int32(nil), ids...)
	sortInt32s(sorted)

	var total int64
	for _, id := range sorted {
		count, err := disableGiftCardWithRefund(ctx, id)
		if err != nil {
			return GiftCardMutationCount{}, err
		}
		total += count.Count
	}
	return GiftCardMutationCount{Count: total}, nil
}

// BatchDeleteGiftCards mirrors batchDeleteGiftCards.
func BatchDeleteGiftCards(ctx context.Context, ids []int32) (GiftCardMutationCount, error) {
	sorted := append([]int32(nil), ids...)
	sortInt32s(sorted)

	var total int64
	for _, id := range sorted {
		count, err := deleteGiftCardWithRefund(ctx, id)
		if err != nil {
			return GiftCardMutationCount{}, err
		}
		total += count.Count
	}
	return GiftCardMutationCount{Count: total}, nil
}

// sortInt32s is a tiny insertion sort; the batch id list is capped at 500.
func sortInt32s(values []int32) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

// ==================== listings ====================

// giftCardAdminSelect mirrors the admin list `select` (relations + usage columns).
const giftCardAdminSelect = giftCardRelationColumnsAliased

// ListGiftCardsByAdmin mirrors listGiftCardsByAdmin().
func ListGiftCardsByAdmin(ctx context.Context, options GiftCardAdminListOptions) (GiftCardAdminPage, error) {
	page := clampGiftCardPage(options.Page)
	pageSize := clampGiftCardPageSize(options.PageSize)

	where := " WHERE 1=1"
	args := []any{}
	if options.Status != "" {
		where += " AND g.status = ?"
		args = append(args, string(options.Status))
	}
	if options.BatchID != nil && *options.BatchID != "" {
		where += " AND g.batch_id = ?"
		args = append(args, *options.BatchID)
	}

	var total int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM gift_cards g"+where, args...).
		Scan(&total).Error; err != nil {
		return GiftCardAdminPage{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := queryGiftCardRows(ctx,
		"SELECT "+giftCardAdminSelect+giftCardRelationFrom+where+
			" ORDER BY g.created_at DESC LIMIT ? OFFSET ?", listArgs...)
	if err != nil {
		return GiftCardAdminPage{}, err
	}

	records := make([]GiftCardAdminView, 0, len(rows))
	for _, row := range rows {
		records = append(records, serializeGiftCardAdmin(row, options.RevealCode))
	}
	return GiftCardAdminPage{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// ListGiftCardsByUser mirrors listGiftCardsByUser() — the user always sees their
// own full codes.
func ListGiftCardsByUser(ctx context.Context, userID int32, options GiftCardUserListOptions) (GiftCardUserPage, error) {
	page := clampGiftCardPage(options.Page)
	pageSize := clampGiftCardPageSize(options.PageSize)

	where := " WHERE (owner_id = ? OR created_by_id = ? OR used_by_id = ?)"
	args := []any{userID, userID, userID}
	if options.Status != "" {
		where += " AND status = ?"
		args = append(args, string(options.Status))
	}

	var total int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM gift_cards"+where, args...).
		Scan(&total).Error; err != nil {
		return GiftCardUserPage{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := queryGiftCardRows(ctx,
		"SELECT "+giftCardBasicColumns+" FROM gift_cards"+where+
			" ORDER BY created_at DESC LIMIT ? OFFSET ?", listArgs...)
	if err != nil {
		return GiftCardUserPage{}, err
	}

	records := make([]GiftCardUserView, 0, len(rows))
	for _, row := range rows {
		records = append(records, serializeGiftCardUser(row))
	}
	return GiftCardUserPage{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// GetGiftCardStats mirrors getGiftCardStats(): "active" excludes cards whose
// expiry already passed, so the stats tick down in real time.
func GetGiftCardStats(ctx context.Context) (GiftCardStats, error) {
	const liveActive = " status = 'active' AND (expires_at IS NULL OR expires_at > NOW())"

	var stats GiftCardStats
	counts := []struct {
		target *int64
		where  string
	}{
		{&stats.Total, ""},
		{&stats.Active, " WHERE " + liveActive},
		{&stats.Used, " WHERE status = 'used'"},
		{&stats.Disabled, " WHERE status = 'disabled'"},
		{&stats.Expired, " WHERE status = 'expired'"},
	}
	for _, item := range counts {
		if err := DB.WithContext(ctx).
			Raw("SELECT COUNT(*) FROM gift_cards" + item.where).Scan(item.target).Error; err != nil {
			return GiftCardStats{}, err
		}
	}

	var outstanding, redeemed struct {
		Total models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).
		Raw("SELECT COALESCE(SUM(balance_value), 0)::numeric AS total FROM gift_cards WHERE " + liveActive).
		Scan(&outstanding).Error; err != nil {
		return GiftCardStats{}, err
	}
	if err := DB.WithContext(ctx).
		Raw("SELECT COALESCE(SUM(balance_value), 0)::numeric AS total FROM gift_cards WHERE status = 'used'").
		Scan(&redeemed).Error; err != nil {
		return GiftCardStats{}, err
	}

	stats.OutstandingValue = moneyFloat(outstanding.Total.Decimal)
	stats.TotalRedeemedValue = moneyFloat(redeemed.Total.Decimal)
	return stats, nil
}
