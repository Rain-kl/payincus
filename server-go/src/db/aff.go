// Port of server/src/db/aff.ts (AFF referral programme DB layer).
//
// Every package-level identifier is `Aff`/`aff`-prefixed because the db package
// is shared by parallel module ports.
//
// Money and concurrency
// ---------------------
// Two PostgreSQL transaction-scoped advisory locks guard every AFF balance move,
// taken in the same order and with the same namespaces as the TypeScript:
//
//	USER_AFF_BALANCE_LOCK_NAMESPACE = 4112  (UserAffBalanceLockNamespace)
//	USER_BALANCE_LOCK_NAMESPACE     = 4110  (UserBalanceLockNamespace)
//
// A withdrawal (conversion) takes 4112 first and 4110 second. Because a
// transaction-scoped lock is released when the transaction ends, the lock call
// and the balance mutation must run on the *same* pgx.Tx, which is why every
// mutation below takes a pgx.Tx (pool.Begin when the caller did not supply one).
//
// Single-payable withdrawal: approveAffWithdrawal flips the row with
// `UPDATE ... WHERE id = $1 AND status = 'pending'` and aborts when the affected
// row count is not exactly 1. The advisory locks serialize concurrent reviewers,
// and the status guard is the final authority — a withdrawal can only ever be
// credited once. Rejection never moves money (pending rows were never debited).
//
// Wire money shapes
// -----------------
// The TypeScript returns `number` from the AFF statistics/log/list functions
// (via `Number(...)` / `parseFloat(...)`), so those Go fields are float64. The
// raw `AffCode` / `AffLog` / `AffWithdrawal` rows keep models.Decimal, because
// Prisma serializes those as strings. JS number → Decimal conversions use
// affDecimalFromNumber (shortest round-trip decimal, matching decimal.js'
// `new Decimal(number)`), never a fixed-point format.
package db

import (
	"context"
	"crypto/rand"
	"database/sql/driver"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants / guards ====================

// affLogTypes mirrors AFF_LOG_TYPES. Types outside the set are dropped by
// normalizeAffLogType (the query string filter is silently ignored, not rejected,
// at this layer — the route rejects it first).
var affLogTypes = map[models.AffLogType]bool{
	models.AffLogTypeNewPurchase: true,
	models.AffLogTypeRenew:       true,
	models.AffLogTypeConvert:     true,
}

// affWithdrawalStatuses mirrors AFF_WITHDRAWAL_STATUSES.
var affWithdrawalStatuses = map[models.AffWithdrawalStatus]bool{
	models.AffWithdrawalStatusPending:  true,
	models.AffWithdrawalStatusApproved: true,
	models.AffWithdrawalStatusRejected: true,
}

// affCodeAlphabet is nanoid's default `urlAlphabet`. generateAffCode() runs
// nanoid(8).toUpperCase(), so codes contain only A-Z, 0-9, "_" and "-".
const affCodeAlphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"

// ==================== types ====================

// AffLogListOptions mirrors getAffLogs' options object. A zero Page / PageSize
// means "not provided" (the Node options are optional), and an empty Type means
// "no filter". The Node clamp turns an explicit pageSize of 0 into 1, which no
// caller produces — the route clamps to >= 1 before calling.
type AffLogListOptions struct {
	Page     int
	PageSize int
	Type     models.AffLogType
}

// AffWithdrawalListOptions mirrors getUserAffWithdrawals' options object.
type AffWithdrawalListOptions struct {
	Page     int
	PageSize int
	Status   models.AffWithdrawalStatus
}

// AffPendingWithdrawalListOptions mirrors getPendingAffWithdrawals' options
// object (identical shape, no user filter).
type AffPendingWithdrawalListOptions struct {
	Page     int
	PageSize int
	Status   models.AffWithdrawalStatus
}

// AffLogCodeRef is the `affCode: { select: { code: true } }` projection.
type AffLogCodeRef struct {
	Code string `json:"code"`
}

// AffLogWithDetails mirrors AffLogWithDetails (`AffLog & { affCode?: {code} }`).
// It is an explicit projection rather than an embedded models.AffLog so the
// AffCode relation keeps exactly the selected shape.
type AffLogWithDetails struct {
	ID                 int32             `json:"id"`
	UserID             int32             `json:"userId"`
	AffCodeID          *int32            `json:"affCodeId"`
	InstanceID         *int32            `json:"instanceId"`
	MailSubscriptionID *int32            `json:"mailSubscriptionId"`
	Type               models.AffLogType `json:"type"`
	Amount             models.Decimal    `json:"amount"`
	OriginalAmount     *models.Decimal   `json:"originalAmount"`
	BalanceBefore      models.Decimal    `json:"balanceBefore"`
	BalanceAfter       models.Decimal    `json:"balanceAfter"`
	Remark             *string           `json:"remark"`
	CreatedAt          time.Time         `json:"createdAt"`
	AffCode            *AffLogCodeRef    `json:"affCode"`
}

// AffLogsResult mirrors getAffLogs' return shape.
type AffLogsResult struct {
	Logs     []AffLogWithDetails `json:"logs"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"pageSize"`
}

// AffStats mirrors getAffStats' return shape. Every field is a JavaScript number
// in the TypeScript (`Promise<{...: number}>`), so money is float64 here.
type AffStats struct {
	TotalEarnings  float64 `json:"totalEarnings"`
	TotalConverted float64 `json:"totalConverted"`
	CurrentBalance float64 `json:"currentBalance"`
	TotalCodes     int64   `json:"totalCodes"`
	TotalUsed      int64   `json:"totalUsed"`
}

// AffPackageRef is the `package: { select: { id, name } }` projection.
type AffPackageRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// AffCodePackagePlan mirrors the nested packagePlan select of
// getUserAffCodes plus its `price: Number(price)` conversion.
type AffCodePackagePlan struct {
	ID      int32         `json:"id"`
	Name    string        `json:"name"`
	Price   float64       `json:"price"`
	Package AffPackageRef `json:"package"`
}

// AffCodeWithPlan mirrors AffCode & { packagePlan: {...} | null }.
type AffCodeWithPlan struct {
	ID             int32               `json:"id"`
	Code           string              `json:"code"`
	UserID         int32               `json:"userId"`
	PackagePlanID  *int32              `json:"packagePlanId"`
	DiscountRate   models.Decimal      `json:"discountRate"`
	CommissionRate models.Decimal      `json:"commissionRate"`
	Enabled        bool                `json:"enabled"`
	UsedCount      int32               `json:"usedCount"`
	TotalEarnings  models.Decimal      `json:"totalEarnings"`
	CreatedAt      time.Time           `json:"createdAt"`
	PackagePlan    *AffCodePackagePlan `json:"packagePlan"`
}

// AffAvailablePlan mirrors one entry of getAvailablePlansForAffCode's `plans`.
type AffAvailablePlan struct {
	ID      int32         `json:"id"`
	Name    string        `json:"name"`
	Price   float64       `json:"price"`
	Package AffPackageRef `json:"package"`
	HasCode bool          `json:"hasCode"`
}

// AffAvailablePlansResult mirrors getAvailablePlansForAffCode's return shape.
type AffAvailablePlansResult struct {
	Plans         []AffAvailablePlan `json:"plans"`
	HasGlobalCode bool               `json:"hasGlobalCode"`
}

// AffCreateCodeResult mirrors createAffCode's return shape.
type AffCreateCodeResult struct {
	Success bool
	AffCode *models.AffCode
	Error   string
}

// AffDeleteCodeResult mirrors deleteAffCode's return shape.
type AffDeleteCodeResult struct {
	Success bool
	Code    string
	Error   string
}

// AffValidateCodeResult mirrors validateAffCode / validateMailAffCode. The rate
// pointers are number-or-absent, matching the TypeScript optional fields.
type AffValidateCodeResult struct {
	Valid          bool            `json:"valid"`
	AffCode        *models.AffCode `json:"affCode,omitempty"`
	DiscountRate   *float64        `json:"discountRate,omitempty"`
	CommissionRate *float64        `json:"commissionRate,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// AffBalanceChangeInput mirrors AffBalanceChangeInput. Amount is signed:
// positive credits, negative debits.
type AffBalanceChangeInput struct {
	UserID             int32
	Type               models.AffLogType
	Amount             float64
	AffCodeID          *int32
	InstanceID         *int32
	MailSubscriptionID *int32
	OriginalAmount     *float64
	Remark             *string
}

// AffBalanceChangeResult mirrors AffBalanceChangeResult.
type AffBalanceChangeResult struct {
	Success    bool           `json:"success"`
	AffLog     *models.AffLog `json:"affLog,omitempty"`
	NewBalance *float64       `json:"newBalance,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// AffInstanceBinding mirrors getInstanceAffBinding's return shape.
type AffInstanceBinding struct {
	AffCode models.AffCode
	UserID  int32
}

// AffCreateWithdrawalResult mirrors createAffWithdrawal's return shape.
type AffCreateWithdrawalResult struct {
	Success    bool                  `json:"success"`
	Withdrawal *models.AffWithdrawal `json:"withdrawal,omitempty"`
	Error      string                `json:"error,omitempty"`
}

// AffWithdrawalResult mirrors approveAffWithdrawal / rejectAffWithdrawal.
type AffWithdrawalResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// AffWithdrawalsResult mirrors getUserAffWithdrawals' return shape.
type AffWithdrawalsResult struct {
	Withdrawals []models.AffWithdrawal `json:"withdrawals"`
	Total       int64                  `json:"total"`
	Page        int                    `json:"page"`
	PageSize    int                    `json:"pageSize"`
}

// AffPendingWithdrawalUser is the `user: { select: {id, username, affBalance} }`
// projection with affBalance converted to a number.
type AffPendingWithdrawalUser struct {
	ID         int32   `json:"id"`
	Username   string  `json:"username"`
	AffBalance float64 `json:"affBalance"`
}

// AffPendingWithdrawal mirrors
//
//	AffWithdrawal & { user: { id: number; username: string; affBalance: number } }
//
// The user object shadows the model's `gorm:"-"` relation field.
type AffPendingWithdrawal struct {
	ID           int32                      `json:"id"`
	UserID       int32                      `json:"userId"`
	Amount       models.Decimal             `json:"amount"`
	Status       models.AffWithdrawalStatus `json:"status"`
	RejectReason *string                    `json:"rejectReason"`
	ReviewedBy   *int32                     `json:"reviewedBy"`
	ReviewedAt   *time.Time                 `json:"reviewedAt"`
	CreatedAt    time.Time                  `json:"createdAt"`
	User         AffPendingWithdrawalUser   `json:"user"`
}

// AffPendingWithdrawalsResult mirrors getPendingAffWithdrawals' return shape.
type AffPendingWithdrawalsResult struct {
	Withdrawals []AffPendingWithdrawal `json:"withdrawals"`
	Total       int64                  `json:"total"`
	Page        int                    `json:"page"`
	PageSize    int                    `json:"pageSize"`
}

// AffLeaderboardEntry mirrors AffLeaderboardEntry.
type AffLeaderboardEntry struct {
	Rank          int     `json:"rank"`
	Username      string  `json:"username"`
	TotalEarnings float64 `json:"totalEarnings"`
	IsCurrentUser bool    `json:"isCurrentUser"`
}

// ==================== value helpers ====================

// affClampPagination mirrors clampPagination(page, pageSize) with the AFF
// module's 20/100 defaults. A zero value means "undefined".
func affClampPagination(page, pageSize int) (int, int) {
	clampedPage := 1
	if page > 0 {
		clampedPage = page
	}

	clampedPageSize := 20
	if pageSize != 0 {
		clampedPageSize = pageSize
		if clampedPageSize < 1 {
			clampedPageSize = 1
		}
		if clampedPageSize > 100 {
			clampedPageSize = 100
		}
	}
	return clampedPage, clampedPageSize
}

// affNormalizeLogType mirrors normalizeAffLogType: an unknown type removes the
// filter instead of failing.
func affNormalizeLogType(logType models.AffLogType) models.AffLogType {
	if logType != "" && affLogTypes[logType] {
		return logType
	}
	return ""
}

// affNormalizeWithdrawalStatus mirrors normalizeAffWithdrawalStatus.
func affNormalizeWithdrawalStatus(status models.AffWithdrawalStatus) models.AffWithdrawalStatus {
	if status != "" && affWithdrawalStatuses[status] {
		return status
	}
	return ""
}

// affDecimalFromNumber mirrors `new Prisma.Decimal(number)`: decimal.js parses
// the number's shortest round-trip string, so 0.1 stays 0.1 (it must not become
// 0.1000000000000000055511151231257827).
func affDecimalFromNumber(value float64) models.Decimal {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return models.Decimal{Decimal: decimal.Zero}
	}
	parsed, err := decimal.NewFromString(strconv.FormatFloat(value, 'f', -1, 64))
	if err != nil {
		return models.Decimal{Decimal: decimal.Zero}
	}
	return models.Decimal{Decimal: parsed}
}

// affDecimalToNumber mirrors `Number(decimal)` / `parseFloat(String(decimal))`.
func affDecimalToNumber(value models.Decimal) float64 {
	return value.Decimal.InexactFloat64()
}

// affNullDecimal scans a nullable numeric column. shopspring's Decimal.Scan
// rejects SQL NULL ("can't scan NULL into a Decimal"), so a plain models.Decimal
// destination would fail on every NULL row; this wrapper turns NULL into a
// zero-valued, !Valid result instead.
type affNullDecimal struct {
	// Amount is the parsed numeric value. The field cannot be called Value because
	// driver.Valuer requires a method with that exact name.
	Amount models.Decimal
	Valid  bool
}

// Scan implements sql.Scanner for numeric/NULL sources.
func (value *affNullDecimal) Scan(source any) error {
	if source == nil {
		value.Amount = models.Decimal{Decimal: decimal.Zero}
		value.Valid = false
		return nil
	}
	if err := value.Amount.Scan(source); err != nil {
		return err
	}
	value.Valid = true
	return nil
}

// Value implements driver.Valuer.
//
// GORM decides whether a struct field is a data column or a RELATION by checking
// for the Valuer/Scanner pair. Scan alone is not enough: without Value() it treats
// affNullDecimal as a has-one relation and fails at runtime with
//
//	invalid field found for struct ()'s field AffBalance: define a valid foreign
//	key for relations or implement the Valuer/Scanner interface
//
// which is exactly what the aff-me conformance case caught.
func (value affNullDecimal) Value() (driver.Value, error) {
	if !value.Valid {
		return nil, nil
	}
	return value.Amount.Decimal.Value()
}

// Pointer returns the JSON-facing value: nil for SQL NULL.
func (value affNullDecimal) Pointer() *models.Decimal {
	if !value.Valid {
		return nil
	}
	copied := value.Amount
	return &copied
}

// affJSRound mirrors `Math.round` for non-negative inputs (the only inputs the
// commission and conversion maths ever produces): Go's math.Round rounds half
// away from zero, JavaScript rounds half toward +Infinity.
func affJSRound(value float64) float64 {
	return math.Round(value)
}

// affMaskUsername mirrors maskUsername: keep the first and last two UTF-16 code
// units and fill the middle with `***`; usernames of four units or fewer keep
// only their first unit. UTF-16 units are used because JavaScript's `length`,
// `slice` and `charAt` count code units, not runes.
func affMaskUsername(username string) string {
	units := utf16.Encode([]rune(username))
	if len(units) <= 4 {
		if len(units) == 0 {
			return "***"
		}
		return string(utf16.Decode(units[:1])) + "***"
	}
	first := string(utf16.Decode(units[:2]))
	last := string(utf16.Decode(units[len(units)-2:]))
	return first + "***" + last
}

// affGenerateCode mirrors generateAffCode: `AFF-<userId>-<8 random chars>` where
// the random part is nanoid(8) uppercased.
func affGenerateCode(userID int32) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	code := make([]byte, len(random))
	for index, value := range random {
		// 64 alphabet characters, so the low 6 bits are uniform.
		code[index] = affCodeAlphabet[int(value)&0x3f]
	}
	return "AFF-" + strconv.FormatInt(int64(userID), 10) + "-" + strings.ToUpper(string(code)), nil
}

// affBeginTx starts the pgx transaction that owns a lock-guarded mutation.
func affBeginTx(ctx context.Context) (pgx.Tx, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return pool.Begin(ctx)
}

// ==================== AFF balance ====================

// AffGetBalance mirrors getAffBalance: the user's AFF balance, or 0 when the
// user does not exist. TypeScript returns `Promise<number>`.
func AffGetBalance(ctx context.Context, userID int32) (float64, error) {
	var user models.User
	err := DB.WithContext(ctx).Select("aff_balance").Where("id = ?", userID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// GORM leaves the destination untouched for an empty result, so a real user
	// is the only path that reaches here.
	return affDecimalToNumber(user.AffBalance), nil
}

// AffIsActivated mirrors isAffActivated. The requirement changed: the referral
// programme no longer needs a recharge, so every user is activated. The userId
// parameter is kept for API compatibility with the TypeScript (which ignores it).
func AffIsActivated(_ context.Context, _ int32) bool {
	return true
}

// AffActivate mirrors activateAff (kept for the flows that still call it).
func AffActivate(ctx context.Context, userID int32) error {
	return DB.WithContext(ctx).Model(&models.User{}).
		Where("id = ?", userID).
		Update("aff_activated_at", time.Now().UTC()).Error
}

// affLogInsert is the createAffLog payload.
type affLogInsert struct {
	userID             int32
	logType            models.AffLogType
	amount             models.Decimal
	affCodeID          *int32
	instanceID         *int32
	mailSubscriptionID *int32
	originalAmount     *models.Decimal
	balanceBefore      models.Decimal
	balanceAfter       models.Decimal
	remark             *string
}

// affInsertLog writes the AFF ledger row and returns it with its generated id.
func affInsertLog(ctx context.Context, tx pgx.Tx, input affLogInsert) (models.AffLog, error) {
	const query = `INSERT INTO aff_logs
		(user_id, type, amount, aff_code_id, instance_id, mail_subscription_id,
		 original_amount, balance_before, balance_after, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		RETURNING id, created_at`

	var id int32
	var createdAt time.Time
	err := tx.QueryRow(ctx, query,
		input.userID,
		string(input.logType),
		input.amount,
		input.affCodeID,
		input.instanceID,
		input.mailSubscriptionID,
		input.originalAmount,
		input.balanceBefore,
		input.balanceAfter,
		input.remark,
	).Scan(&id, &createdAt)
	if err != nil {
		return models.AffLog{}, err
	}

	return models.AffLog{
		ID:                 id,
		UserID:             input.userID,
		AffCodeID:          input.affCodeID,
		InstanceID:         input.instanceID,
		MailSubscriptionID: input.mailSubscriptionID,
		Type:               input.logType,
		Amount:             input.amount,
		OriginalAmount:     input.originalAmount,
		BalanceBefore:      input.balanceBefore,
		BalanceAfter:       input.balanceAfter,
		Remark:             input.remark,
		CreatedAt:          createdAt,
	}, nil
}

// affChangeAffBalanceInTransaction mirrors changeAffBalanceInTransaction. It must
// run on a pgx.Tx so the advisory lock, the UPDATE and the ledger insert share
// one transaction.
func affChangeAffBalanceInTransaction(ctx context.Context, tx pgx.Tx, input AffBalanceChangeInput) (AffBalanceChangeResult, error) {
	if err := AdvisoryTransactionLock(ctx, tx, UserAffBalanceLockNamespace, input.UserID); err != nil {
		return AffBalanceChangeResult{}, err
	}

	var current models.Decimal
	err := tx.QueryRow(ctx, "SELECT aff_balance FROM users WHERE id = $1", input.UserID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return AffBalanceChangeResult{}, errors.New("用户不存在")
	}
	if err != nil {
		return AffBalanceChangeResult{}, err
	}

	// Number(user.affBalance) + amount — a JavaScript float addition.
	balanceBefore := affDecimalToNumber(current)
	balanceAfter := balanceBefore + input.Amount

	if input.Amount < 0 && balanceAfter < 0 {
		return AffBalanceChangeResult{}, errors.New("AFF 余额不足")
	}

	if _, err := tx.Exec(ctx,
		"UPDATE users SET aff_balance = aff_balance + $1, updated_at = NOW() WHERE id = $2",
		affDecimalFromNumber(input.Amount), input.UserID); err != nil {
		return AffBalanceChangeResult{}, err
	}

	var originalAmount *models.Decimal
	if input.OriginalAmount != nil {
		converted := affDecimalFromNumber(*input.OriginalAmount)
		originalAmount = &converted
	}

	logRow, err := affInsertLog(ctx, tx, affLogInsert{
		userID:             input.UserID,
		logType:            input.Type,
		amount:             affDecimalFromNumber(input.Amount),
		affCodeID:          input.AffCodeID,
		instanceID:         input.InstanceID,
		mailSubscriptionID: input.MailSubscriptionID,
		originalAmount:     originalAmount,
		balanceBefore:      affDecimalFromNumber(balanceBefore),
		balanceAfter:       affDecimalFromNumber(balanceAfter),
		remark:             input.Remark,
	})
	if err != nil {
		return AffBalanceChangeResult{}, err
	}

	newBalance := balanceAfter
	return AffBalanceChangeResult{Success: true, AffLog: &logRow, NewBalance: &newBalance}, nil
}

// AffChangeBalance mirrors changeAffBalance. A nil tx means "open a transaction";
// a non-nil tx joins the caller's transaction (settlement flows pass theirs).
// Like the TypeScript it never returns an error, surfacing failures through
// Result.Error.
func AffChangeBalance(ctx context.Context, input AffBalanceChangeInput, tx pgx.Tx) AffBalanceChangeResult {
	if tx != nil {
		result, err := affChangeAffBalanceInTransaction(ctx, tx, input)
		if err != nil {
			return AffBalanceChangeResult{Success: false, Error: err.Error()}
		}
		return result
	}

	owned, err := affBeginTx(ctx)
	if err != nil {
		return AffBalanceChangeResult{Success: false, Error: err.Error()}
	}
	defer func() { _ = owned.Rollback(ctx) }()

	result, err := affChangeAffBalanceInTransaction(ctx, owned, input)
	if err != nil {
		return AffBalanceChangeResult{Success: false, Error: err.Error()}
	}
	if err := owned.Commit(ctx); err != nil {
		return AffBalanceChangeResult{Success: false, Error: err.Error()}
	}
	return result
}

// ==================== AFF codes ====================

// AffGetUserCodes mirrors getUserAffCodes (global codes included).
func AffGetUserCodes(ctx context.Context, userID int32) ([]AffCodeWithPlan, error) {
	type codeRow struct {
		ID             int32          `gorm:"column:id"`
		Code           string         `gorm:"column:code"`
		UserID         int32          `gorm:"column:user_id"`
		PackagePlanID  *int32         `gorm:"column:package_plan_id"`
		DiscountRate   models.Decimal `gorm:"column:discount_rate"`
		CommissionRate models.Decimal `gorm:"column:commission_rate"`
		Enabled        bool           `gorm:"column:enabled"`
		UsedCount      int32          `gorm:"column:used_count"`
		TotalEarnings  models.Decimal `gorm:"column:total_earnings"`
		CreatedAt      time.Time      `gorm:"column:created_at"`
		PlanID         *int32         `gorm:"column:plan_id"`
		PlanName       *string        `gorm:"column:plan_name"`
		PlanPrice      affNullDecimal `gorm:"column:plan_price"`
		PackageID      *int32         `gorm:"column:package_id"`
		PackageName    *string        `gorm:"column:package_name"`
	}

	var rows []codeRow
	err := DB.WithContext(ctx).Raw(
		`SELECT c.id, c.code, c.user_id, c.package_plan_id, c.discount_rate,
		        c.commission_rate, c.enabled, c.used_count, c.total_earnings, c.created_at,
		        p.id AS plan_id, p.name AS plan_name, p.price AS plan_price,
		        pkg.id AS package_id, pkg.name AS package_name
		   FROM aff_codes c
		   LEFT JOIN package_plans p ON p.id = c.package_plan_id
		   LEFT JOIN packages pkg ON pkg.id = p.package_id
		  WHERE c.user_id = ?
		  ORDER BY c.created_at DESC`, userID,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	codes := make([]AffCodeWithPlan, 0, len(rows))
	for _, row := range rows {
		code := AffCodeWithPlan{
			ID:             row.ID,
			Code:           row.Code,
			UserID:         row.UserID,
			PackagePlanID:  row.PackagePlanID,
			DiscountRate:   row.DiscountRate,
			CommissionRate: row.CommissionRate,
			Enabled:        row.Enabled,
			UsedCount:      row.UsedCount,
			TotalEarnings:  row.TotalEarnings,
			CreatedAt:      row.CreatedAt,
			PackagePlan:    nil,
		}
		if row.PlanID != nil && row.PlanName != nil && row.PlanPrice.Valid &&
			row.PackageID != nil && row.PackageName != nil {
			code.PackagePlan = &AffCodePackagePlan{
				ID:      *row.PlanID,
				Name:    *row.PlanName,
				Price:   affDecimalToNumber(row.PlanPrice.Amount),
				Package: AffPackageRef{ID: *row.PackageID, Name: *row.PackageName},
			}
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// AffGetAvailablePlansForCode mirrors getAvailablePlansForAffCode: the paid plans
// the user has bought that are still active, plus whether a global code exists.
func AffGetAvailablePlansForCode(ctx context.Context, userID int32) (AffAvailablePlansResult, error) {
	// 1. Existing global code?
	var globalCount int64
	if err := DB.WithContext(ctx).Model(&models.AffCode{}).
		Where("user_id = ? AND package_plan_id IS NULL", userID).
		Count(&globalCount).Error; err != nil {
		return AffAvailablePlansResult{}, err
	}
	hasGlobalCode := globalCount > 0

	// 2. Distinct paid plans the user has purchased (packagePlan.price > 0).
	var purchasedRows []struct {
		PackagePlanID int32 `gorm:"column:package_plan_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT DISTINCT i.package_plan_id
		   FROM instances i
		   JOIN package_plans p ON p.id = i.package_plan_id
		  WHERE i.user_id = ? AND i.package_plan_id IS NOT NULL AND p.price > 0`,
		userID,
	).Scan(&purchasedRows).Error; err != nil {
		return AffAvailablePlansResult{}, err
	}
	purchasedIDs := make([]int32, 0, len(purchasedRows))
	for _, row := range purchasedRows {
		// `filter(Boolean)` in the TypeScript drops zero/falsy ids.
		if row.PackagePlanID != 0 {
			purchasedIDs = append(purchasedIDs, row.PackagePlanID)
		}
	}

	if len(purchasedIDs) == 0 {
		return AffAvailablePlansResult{Plans: []AffAvailablePlan{}, HasGlobalCode: hasGlobalCode}, nil
	}

	// 3. Active plans among those ids, with the user's existing codes.
	type planRow struct {
		ID        int32          `gorm:"column:id"`
		Name      string         `gorm:"column:name"`
		Price     models.Decimal `gorm:"column:price"`
		PackageID int32          `gorm:"column:package_id"`
		PkgName   string         `gorm:"column:pkg_name"`
	}
	var plans []planRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT p.id, p.name, p.price, pkg.id AS package_id, pkg.name AS pkg_name
		   FROM package_plans p
		   JOIN packages pkg ON pkg.id = p.package_id
		  WHERE p.id IN ? AND p.is_active = true
		  ORDER BY p.id`,
		purchasedIDs,
	).Scan(&plans).Error; err != nil {
		return AffAvailablePlansResult{}, err
	}

	var codedRows []struct {
		PackagePlanID *int32 `gorm:"column:package_plan_id"`
	}
	if len(plans) > 0 {
		if err := DB.WithContext(ctx).Raw(
			`SELECT package_plan_id FROM aff_codes
			  WHERE user_id = ? AND package_plan_id IN ?`,
			userID, purchasedIDs,
		).Scan(&codedRows).Error; err != nil {
			return AffAvailablePlansResult{}, err
		}
	}
	coded := make(map[int32]bool, len(codedRows))
	for _, row := range codedRows {
		if row.PackagePlanID != nil {
			coded[*row.PackagePlanID] = true
		}
	}

	result := AffAvailablePlansResult{
		Plans:         make([]AffAvailablePlan, 0, len(plans)),
		HasGlobalCode: hasGlobalCode,
	}
	for _, plan := range plans {
		result.Plans = append(result.Plans, AffAvailablePlan{
			ID:      plan.ID,
			Name:    plan.Name,
			Price:   affDecimalToNumber(plan.Price),
			Package: AffPackageRef{ID: plan.PackageID, Name: plan.PkgName},
			HasCode: coded[plan.ID],
		})
	}
	return result, nil
}

// AffCreateCode mirrors createAffCode. A nil packagePlanID creates a global code
// (several globals are allowed because PostgreSQL treats NULLs as distinct in the
// (user_id, package_plan_id) unique index).
func AffCreateCode(ctx context.Context, userID int32, packagePlanID *int32) AffCreateCodeResult {
	// 1. The referral programme is always activated (see AffIsActivated).
	if !AffIsActivated(ctx, userID) {
		return AffCreateCodeResult{Success: false, Error: "推荐计划尚未激活，请先充值任意金额"}
	}

	discountRate, err := GetSystemConfigFloat(ctx, "aff_discount_rate", 0.05)
	if err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	commissionRate, err := GetSystemConfigFloat(ctx, "aff_commission_rate", 0.05)
	if err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}

	// Global code path — several per user are allowed.
	if packagePlanID == nil {
		code, err := affGenerateCode(userID)
		if err != nil {
			return AffCreateCodeResult{Success: false, Error: "创建全局优惠码失败"}
		}
		var created models.AffCode
		if err := DB.WithContext(ctx).Exec(
			`INSERT INTO aff_codes (code, user_id, package_plan_id, discount_rate, commission_rate)
			 VALUES (?, ?, NULL, ?, ?)`,
			code, userID, affDecimalFromNumber(discountRate), affDecimalFromNumber(commissionRate),
		).Error; err != nil {
			return AffCreateCodeResult{Success: false, Error: "创建全局优惠码失败"}
		}
		if err := affLoadCodeByCode(ctx, code, &created); err != nil {
			return AffCreateCodeResult{Success: false, Error: "创建全局优惠码失败"}
		}
		return AffCreateCodeResult{Success: true, AffCode: &created}
	}

	// 2. Plan must exist and be a paid plan.
	planID := *packagePlanID
	var plan models.PackagePlan
	err = DB.WithContext(ctx).Raw(
		`SELECT id, price, is_active FROM package_plans WHERE id = ?`, planID,
	).Scan(&plan).Error
	if err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	// A zero id means "no row"; a non-positive price means "not a paid plan".
	// Out-of-range ids are treated as missing (Prisma would reject them).
	if plan.ID == 0 || affDecimalToNumber(plan.Price) <= 0 {
		return AffCreateCodeResult{Success: false, Error: "方案不存在或不是付费方案"}
	}
	if !plan.IsActive {
		return AffCreateCodeResult{Success: false, Error: "方案已下架"}
	}

	// 3. The user must have bought the plan.
	var purchaseCount int64
	if err := DB.WithContext(ctx).Model(&models.Instance{}).
		Where("user_id = ? AND package_plan_id = ?", userID, planID).
		Count(&purchaseCount).Error; err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	if purchaseCount == 0 {
		return AffCreateCodeResult{Success: false, Error: "您尚未购买过此方案，无法创建优惠码"}
	}

	// 4. Pre-check the unique (user_id, package_plan_id) constraint for a friendly
	// message; the constraint itself still arbitrates a race.
	var existingCount int64
	if err := DB.WithContext(ctx).Model(&models.AffCode{}).
		Where("user_id = ? AND package_plan_id = ?", userID, planID).
		Count(&existingCount).Error; err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	if existingCount > 0 {
		return AffCreateCodeResult{Success: false, Error: "该方案已创建过优惠码"}
	}

	// 5. Create.
	code, err := affGenerateCode(userID)
	if err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	var created models.AffCode
	if err := DB.WithContext(ctx).Exec(
		`INSERT INTO aff_codes (code, user_id, package_plan_id, discount_rate, commission_rate)
		 VALUES (?, ?, ?, ?, ?)`,
		code, userID, planID,
		affDecimalFromNumber(discountRate), affDecimalFromNumber(commissionRate),
	).Error; err != nil {
		// A concurrent create trips the unique index. Prisma surfaces its P2002
		// message here, which is not reproducible byte-for-byte; the friendly text
		// is what the pre-check above returns for the same situation.
		if affIsUniqueViolation(err) {
			return AffCreateCodeResult{Success: false, Error: "该方案已创建过优惠码"}
		}
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	if err := affLoadCodeByCode(ctx, code, &created); err != nil {
		return AffCreateCodeResult{Success: false, Error: err.Error()}
	}
	return AffCreateCodeResult{Success: true, AffCode: &created}
}

// affIsUniqueViolation reports whether err is PostgreSQL's unique_violation
// (SQLSTATE 23505), the error a concurrent create trips.
func affIsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// affLoadCodeByCode loads the freshly created row (Prisma's create returns it).
func affLoadCodeByCode(ctx context.Context, code string, destination *models.AffCode) error {
	return DB.WithContext(ctx).Raw(
		`SELECT id, code, user_id, package_plan_id, discount_rate, commission_rate,
		        enabled, used_count, total_earnings, created_at
		   FROM aff_codes WHERE code = ?`, code,
	).Scan(destination).Error
}

// AffDeleteCode mirrors deleteAffCode: only unused, unbound codes owned by the
// caller may be removed.
func AffDeleteCode(ctx context.Context, userID, codeID int32) AffDeleteCodeResult {
	var affCode struct {
		ID        int32  `gorm:"column:id"`
		Code      string `gorm:"column:code"`
		UserID    int32  `gorm:"column:user_id"`
		UsedCount int32  `gorm:"column:used_count"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, code, user_id, used_count FROM aff_codes WHERE id = ?`, codeID,
	).Scan(&affCode).Error; err != nil {
		return AffDeleteCodeResult{Success: false, Error: err.Error()}
	}
	if affCode.ID == 0 {
		return AffDeleteCodeResult{Success: false, Error: "优惠码不存在"}
	}
	if affCode.UserID != userID {
		return AffDeleteCodeResult{Success: false, Error: "无权删除此优惠码"}
	}
	if affCode.UsedCount > 0 {
		return AffDeleteCodeResult{Success: false, Error: "该优惠码已被使用，无法删除"}
	}

	var instanceBindingCount int64
	if err := DB.WithContext(ctx).Model(&models.AffBinding{}).
		Where("aff_code_id = ?", codeID).Count(&instanceBindingCount).Error; err != nil {
		return AffDeleteCodeResult{Success: false, Error: err.Error()}
	}
	var mailBindingCount int64
	if err := DB.WithContext(ctx).Model(&models.MailSubscriptionAffBinding{}).
		Where("aff_code_id = ?", codeID).Count(&mailBindingCount).Error; err != nil {
		return AffDeleteCodeResult{Success: false, Error: err.Error()}
	}
	if instanceBindingCount > 0 || mailBindingCount > 0 {
		return AffDeleteCodeResult{Success: false, Error: "该优惠码已绑定订阅，无法删除"}
	}

	result := DB.WithContext(ctx).Exec(`DELETE FROM aff_codes WHERE id = ?`, codeID)
	if result.Error != nil {
		return AffDeleteCodeResult{Success: false, Error: result.Error.Error()}
	}
	return AffDeleteCodeResult{Success: true, Code: affCode.Code}
}

// AffValidateCode mirrors validateAffCode (globals skip the plan check).
//
// Check order is load-bearing: plan mismatch → own code → disabled.
func AffValidateCode(ctx context.Context, code string, packagePlanID, userID int32) (AffValidateCodeResult, error) {
	var affCode models.AffCode
	err := DB.WithContext(ctx).Raw(
		`SELECT id, code, user_id, package_plan_id, discount_rate, commission_rate,
		        enabled, used_count, total_earnings, created_at
		   FROM aff_codes WHERE code = ?`, strings.ToUpper(code),
	).Scan(&affCode).Error
	if err != nil {
		return AffValidateCodeResult{}, err
	}
	if affCode.ID == 0 {
		return AffValidateCodeResult{Valid: false, Error: "优惠码不存在"}, nil
	}

	if affCode.PackagePlanID != nil && *affCode.PackagePlanID != packagePlanID {
		return AffValidateCodeResult{Valid: false, Error: "优惠码不适用于此方案"}, nil
	}
	if affCode.UserID == userID {
		return AffValidateCodeResult{Valid: false, Error: "不能使用自己的优惠码"}, nil
	}
	if !affCode.Enabled {
		return AffValidateCodeResult{Valid: false, Error: "优惠码已禁用"}, nil
	}

	discountRate := affDecimalToNumber(affCode.DiscountRate)
	commissionRate := affDecimalToNumber(affCode.CommissionRate)
	return AffValidateCodeResult{
		Valid:          true,
		AffCode:        &affCode,
		DiscountRate:   &discountRate,
		CommissionRate: &commissionRate,
	}, nil
}

// ==================== bindings and commission ====================

// AffCreateBinding mirrors createAffBinding.
func AffCreateBinding(ctx context.Context, instanceID, affCodeID int32, tx pgx.Tx) error {
	if tx != nil {
		_, err := tx.Exec(ctx,
			"INSERT INTO aff_bindings (instance_id, aff_code_id) VALUES ($1, $2)",
			instanceID, affCodeID)
		return err
	}
	return DB.WithContext(ctx).Exec(
		`INSERT INTO aff_bindings (instance_id, aff_code_id) VALUES (?, ?)`,
		instanceID, affCodeID,
	).Error
}

// affRowQuerier is satisfied by both pgx.Tx and *pgxpool.Pool, so one scan helper
// serves transactional and plain reads.
type affRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// affCodeColumns is the projection every full aff_codes row read shares.
const affCodeColumns = `c.id, c.code, c.user_id, c.package_plan_id, c.discount_rate,
        c.commission_rate, c.enabled, c.used_count, c.total_earnings, c.created_at`

// affQueryCodeRow loads one aff_codes row. nil, nil means "no row".
func affQueryCodeRow(ctx context.Context, querier affRowQuerier, query string, args ...any) (*models.AffCode, error) {
	var (
		id             int32
		code           string
		userID         int32
		packagePlanID  *int32
		discountRate   models.Decimal
		commissionRate models.Decimal
		enabled        bool
		usedCount      int32
		totalEarnings  models.Decimal
		createdAt      time.Time
	)

	err := querier.QueryRow(ctx, query, args...).Scan(&id, &code, &userID, &packagePlanID,
		&discountRate, &commissionRate, &enabled, &usedCount, &totalEarnings, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &models.AffCode{
		ID:             id,
		Code:           code,
		UserID:         userID,
		PackagePlanID:  packagePlanID,
		DiscountRate:   discountRate,
		CommissionRate: commissionRate,
		Enabled:        enabled,
		UsedCount:      usedCount,
		TotalEarnings:  totalEarnings,
		CreatedAt:      createdAt,
	}, nil
}

// affCodeQuerier resolves the pgx handle for a read that may join a caller's
// transaction.
func affCodeQuerier(tx pgx.Tx) (affRowQuerier, error) {
	if tx != nil {
		return tx, nil
	}
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return pool, nil
}

// AffGetInstanceBinding mirrors getInstanceAffBinding.
func AffGetInstanceBinding(ctx context.Context, instanceID int32, tx pgx.Tx) (*AffInstanceBinding, error) {
	const query = `SELECT ` + affCodeColumns + `
	                 FROM aff_bindings b
	                 JOIN aff_codes c ON c.id = b.aff_code_id
	                WHERE b.instance_id = $1`

	querier, err := affCodeQuerier(tx)
	if err != nil {
		return nil, err
	}
	affCode, err := affQueryCodeRow(ctx, querier, query, instanceID)
	if err != nil || affCode == nil {
		return nil, err
	}
	return &AffInstanceBinding{AffCode: *affCode, UserID: affCode.UserID}, nil
}

// AffProcessCommission mirrors processAffCommission. A failed balance change is
// deliberately ignored: the TypeScript awaits changeAffBalance (which never
// throws) and then increments the code statistics regardless.
func AffProcessCommission(ctx context.Context, affCodeID, instanceID int32, originalAmount float64, logType models.AffLogType, tx pgx.Tx) error {
	affCode, err := affFindCodeByID(ctx, affCodeID, tx)
	if err != nil {
		return err
	}
	if affCode == nil {
		return nil
	}

	commissionRate := affDecimalToNumber(affCode.CommissionRate)
	commission := affJSRound(originalAmount*commissionRate*100) / 100
	if commission <= 0 {
		return nil
	}

	remark := "续费返利"
	if logType == models.AffLogTypeNewPurchase {
		remark = "新购返利"
	}

	AffChangeBalance(ctx, AffBalanceChangeInput{
		UserID:         affCode.UserID,
		Type:           logType,
		Amount:         commission,
		AffCodeID:      &affCodeID,
		InstanceID:     &instanceID,
		OriginalAmount: &originalAmount,
		Remark:         &remark,
	}, tx)

	return affIncrementCodeStats(ctx, affCodeID, commission, logType == models.AffLogTypeNewPurchase, tx)
}

// affFindCodeByID mirrors affCode.findUnique({ where: { id } }).
func affFindCodeByID(ctx context.Context, codeID int32, tx pgx.Tx) (*models.AffCode, error) {
	querier, err := affCodeQuerier(tx)
	if err != nil {
		return nil, err
	}
	return affQueryCodeRow(ctx, querier,
		`SELECT `+affCodeColumns+` FROM aff_codes c WHERE c.id = $1`, codeID)
}

// affIncrementCodeStats mirrors
//
//	affCode.update({ data: { usedCount: new_purchase ? {increment:1} : undefined,
//	                         totalEarnings: {increment: commission} } })
func affIncrementCodeStats(ctx context.Context, codeID int32, commission float64, incrementUsed bool, tx pgx.Tx) error {
	commissionDecimal := affDecimalFromNumber(commission)

	if tx != nil {
		if incrementUsed {
			_, err := tx.Exec(ctx,
				`UPDATE aff_codes SET used_count = used_count + 1, total_earnings = total_earnings + $1 WHERE id = $2`,
				commissionDecimal, codeID)
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE aff_codes SET total_earnings = total_earnings + $1 WHERE id = $2`,
			commissionDecimal, codeID)
		return err
	}

	query := `UPDATE aff_codes SET total_earnings = total_earnings + ? WHERE id = ?`
	args := []any{commissionDecimal, codeID}
	if incrementUsed {
		query = `UPDATE aff_codes SET used_count = used_count + 1, total_earnings = total_earnings + ? WHERE id = ?`
	}
	return DB.WithContext(ctx).Exec(query, args...).Error
}

// ==================== AFF logs ====================

// AffGetLogs mirrors getAffLogs (paged, newest first, optional type filter).
func AffGetLogs(ctx context.Context, userID int32, options AffLogListOptions) (AffLogsResult, error) {
	page, pageSize := affClampPagination(options.Page, options.PageSize)
	logType := affNormalizeLogType(options.Type)
	skip := (page - 1) * pageSize

	// Both predicates are table-qualified: the list query LEFT JOINs aff_codes, and
	// an unqualified `user_id` is ambiguous once the join is in play (SQLSTATE 42702),
	// which the aff-me-logs conformance case caught.
	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.AffLog{}).
			Where("aff_logs.user_id = ?", userID)
		if logType != "" {
			query = query.Where("aff_logs.type::text = ?", string(logType))
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return AffLogsResult{}, err
	}

	type logRow struct {
		ID                 int32             `gorm:"column:id"`
		UserID             int32             `gorm:"column:user_id"`
		AffCodeID          *int32            `gorm:"column:aff_code_id"`
		InstanceID         *int32            `gorm:"column:instance_id"`
		MailSubscriptionID *int32            `gorm:"column:mail_subscription_id"`
		Type               models.AffLogType `gorm:"column:type"`
		Amount             models.Decimal    `gorm:"column:amount"`
		OriginalAmount     affNullDecimal    `gorm:"column:original_amount"`
		BalanceBefore      models.Decimal    `gorm:"column:balance_before"`
		BalanceAfter       models.Decimal    `gorm:"column:balance_after"`
		Remark             *string           `gorm:"column:remark"`
		CreatedAt          time.Time         `gorm:"column:created_at"`
		AffCodeCode        *string           `gorm:"column:aff_code_code"`
	}

	var rows []logRow
	if err := build().
		Select(`aff_logs.id, aff_logs.user_id, aff_logs.aff_code_id, aff_logs.instance_id,
		        aff_logs.mail_subscription_id, aff_logs.type::text AS type, aff_logs.amount,
		        aff_logs.original_amount, aff_logs.balance_before, aff_logs.balance_after,
		        aff_logs.remark, aff_logs.created_at, aff_codes.code AS aff_code_code`).
		Joins("LEFT JOIN aff_codes ON aff_codes.id = aff_logs.aff_code_id").
		Order("aff_logs.created_at DESC").Offset(skip).Limit(pageSize).Scan(&rows).Error; err != nil {
		return AffLogsResult{}, err
	}

	logs := make([]AffLogWithDetails, 0, len(rows))
	for _, row := range rows {
		item := AffLogWithDetails{
			ID:                 row.ID,
			UserID:             row.UserID,
			AffCodeID:          row.AffCodeID,
			InstanceID:         row.InstanceID,
			MailSubscriptionID: row.MailSubscriptionID,
			Type:               row.Type,
			Amount:             row.Amount,
			OriginalAmount:     row.OriginalAmount.Pointer(),
			BalanceBefore:      row.BalanceBefore,
			BalanceAfter:       row.BalanceAfter,
			Remark:             row.Remark,
			CreatedAt:          row.CreatedAt,
		}
		if row.AffCodeCode != nil {
			item.AffCode = &AffLogCodeRef{Code: *row.AffCodeCode}
		}
		logs = append(logs, item)
	}

	return AffLogsResult{Logs: logs, Total: total, Page: page, PageSize: pageSize}, nil
}

// AffGetStats mirrors getAffStats.
//
// `totalEarnings` sums every positive AFF log amount (not the per-code counter),
// and `totalConverted` is the absolute value of every `convert` log amount.
func AffGetStats(ctx context.Context, userID int32) (AffStats, error) {
	var stats AffStats

	// Every aggregate is NULL when the user has no rows; the TypeScript maps that
	// NULL to 0, so COALESCE takes the same branch without a nullable Decimal scan
	// (shopspring's Decimal.Scan rejects NULL).
	var user struct {
		AffBalance affNullDecimal `gorm:"column:aff_balance"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT aff_balance FROM users WHERE id = ?`, userID,
	).Scan(&user).Error; err != nil {
		return AffStats{}, err
	}
	if user.AffBalance.Valid {
		stats.CurrentBalance = affDecimalToNumber(user.AffBalance.Amount)
	}

	var codes struct {
		Total   int64          `gorm:"column:total"`
		UsedSum affNullDecimal `gorm:"column:used_sum"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) AS total, COALESCE(SUM(used_count), 0) AS used_sum
		   FROM aff_codes WHERE user_id = ?`, userID,
	).Scan(&codes).Error; err != nil {
		return AffStats{}, err
	}
	stats.TotalCodes = codes.Total
	if codes.UsedSum.Valid {
		stats.TotalUsed = int64(affDecimalToNumber(codes.UsedSum.Amount))
	}

	var earnings struct {
		Sum affNullDecimal `gorm:"column:sum"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0) AS sum FROM aff_logs WHERE user_id = ? AND amount > 0`, userID,
	).Scan(&earnings).Error; err != nil {
		return AffStats{}, err
	}
	if earnings.Sum.Valid {
		stats.TotalEarnings = affDecimalToNumber(earnings.Sum.Amount)
	}

	var converted struct {
		Sum affNullDecimal `gorm:"column:sum"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0) AS sum FROM aff_logs WHERE user_id = ? AND type = ?`,
		userID, string(models.AffLogTypeConvert),
	).Scan(&converted).Error; err != nil {
		return AffStats{}, err
	}
	if converted.Sum.Valid {
		stats.TotalConverted = math.Abs(affDecimalToNumber(converted.Sum.Amount))
	}

	return stats, nil
}

// ==================== conversions (withdrawals) ====================

// AffCreateWithdrawal mirrors createAffWithdrawal: one transaction creates the
// (auto-approved) request, debits the AFF balance, credits the main balance and
// writes both ledger rows. Lock order is AFF (4112) then balance (4110).
func AffCreateWithdrawal(ctx context.Context, userID int32, amount float64) AffCreateWithdrawalResult {
	if !affIsFinite(amount) || amount < 0.1 {
		return AffCreateWithdrawalResult{Success: false, Error: "最低转化金额为 0.1 元"}
	}

	tx, err := affBeginTx(ctx)
	if err != nil {
		return AffCreateWithdrawalResult{Success: false, Error: err.Error()}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	withdrawal, err := affCreateWithdrawalInTransaction(ctx, tx, userID, amount)
	if err != nil {
		return AffCreateWithdrawalResult{Success: false, Error: err.Error()}
	}
	if err := tx.Commit(ctx); err != nil {
		return AffCreateWithdrawalResult{Success: false, Error: err.Error()}
	}
	return AffCreateWithdrawalResult{Success: true, Withdrawal: withdrawal}
}

// affCreateWithdrawalInTransaction mirrors the prisma.$transaction body.
func affCreateWithdrawalInTransaction(ctx context.Context, tx pgx.Tx, userID int32, amount float64) (*models.AffWithdrawal, error) {
	if err := AdvisoryTransactionLock(ctx, tx, UserAffBalanceLockNamespace, userID); err != nil {
		return nil, err
	}
	if err := AdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID); err != nil {
		return nil, err
	}

	var affBalance, balance models.Decimal
	err := tx.QueryRow(ctx, "SELECT aff_balance, balance FROM users WHERE id = $1", userID).
		Scan(&affBalance, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("用户不存在")
	}
	if err != nil {
		return nil, err
	}

	affBalanceBefore := affDecimalToNumber(affBalance)
	balanceBefore := affDecimalToNumber(balance)
	if affBalanceBefore < amount {
		return nil, errors.New("AFF 余额不足")
	}

	// 2.3 Create the request already marked approved (system auto-approval).
	var (
		withdrawalID int32
		storedAmount models.Decimal
		reviewedAt   time.Time
		createdAt    time.Time
	)
	err = tx.QueryRow(ctx,
		`INSERT INTO aff_withdrawals (user_id, amount, status, reviewed_by, reviewed_at)
		 VALUES ($1, $2, 'approved', NULL, NOW())
		 RETURNING id, amount, reviewed_at, created_at`,
		userID, affDecimalFromNumber(amount),
	).Scan(&withdrawalID, &storedAmount, &reviewedAt, &createdAt)
	if err != nil {
		return nil, err
	}

	// 2.4 Debit AFF, credit the main balance.
	affBalanceAfter := affBalanceBefore - amount
	var balanceAfterDecimal models.Decimal
	if err := tx.QueryRow(ctx,
		`UPDATE users
		    SET aff_balance = aff_balance - $1, balance = balance + $1, updated_at = NOW()
		  WHERE id = $2
		  RETURNING balance`,
		affDecimalFromNumber(amount), userID,
	).Scan(&balanceAfterDecimal); err != nil {
		return nil, err
	}
	balanceAfter := affDecimalToNumber(balanceAfterDecimal)

	// 2.5 AFF ledger.
	affRemark := "转化申请 #" + strconv.FormatInt(int64(withdrawalID), 10) + " 审核通过"
	if _, err := affInsertLog(ctx, tx, affLogInsert{
		userID:        userID,
		logType:       models.AffLogTypeConvert,
		amount:        affDecimalFromNumber(-amount),
		balanceBefore: affDecimalFromNumber(affBalanceBefore),
		balanceAfter:  affDecimalFromNumber(affBalanceAfter),
		remark:        &affRemark,
	}); err != nil {
		return nil, err
	}

	// 2.6 Balance ledger.
	balanceRemark := "AFF 余额转化 #" + strconv.FormatInt(int64(withdrawalID), 10)
	if _, err := insertBalanceLog(ctx, tx, balanceLogInsert{
		userID:        userID,
		logType:       models.BalanceLogTypeGift,
		amount:        affDecimalFromNumber(amount),
		balanceBefore: affDecimalFromNumber(balanceBefore),
		balanceAfter:  affDecimalFromNumber(balanceAfter),
		remark:        &balanceRemark,
	}); err != nil {
		return nil, err
	}

	return &models.AffWithdrawal{
		ID:           withdrawalID,
		UserID:       userID,
		Amount:       storedAmount,
		Status:       models.AffWithdrawalStatusApproved,
		RejectReason: nil,
		ReviewedBy:   nil,
		ReviewedAt:   &reviewedAt,
		CreatedAt:    createdAt,
	}, nil
}

// affIsFinite mirrors Number.isFinite.
func affIsFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// AffGetWithdrawalByID mirrors getAffWithdrawalById.
func AffGetWithdrawalByID(ctx context.Context, withdrawalID int32) (*models.AffWithdrawal, error) {
	var withdrawal models.AffWithdrawal
	err := DB.WithContext(ctx).Raw(
		`SELECT id, user_id, amount, status::text AS status, reject_reason, reviewed_by,
		        reviewed_at, created_at
		   FROM aff_withdrawals WHERE id = ?`, withdrawalID,
	).Scan(&withdrawal).Error
	if err != nil {
		return nil, err
	}
	if withdrawal.ID == 0 {
		return nil, nil
	}
	return &withdrawal, nil
}

// AffGetUserWithdrawals mirrors getUserAffWithdrawals.
func AffGetUserWithdrawals(ctx context.Context, userID int32, options AffWithdrawalListOptions) (AffWithdrawalsResult, error) {
	page, pageSize := affClampPagination(options.Page, options.PageSize)
	status := affNormalizeWithdrawalStatus(options.Status)
	skip := (page - 1) * pageSize

	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.AffWithdrawal{}).Where("user_id = ?", userID)
		if status != "" {
			query = query.Where("status = ?", string(status))
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return AffWithdrawalsResult{}, err
	}

	var withdrawals []models.AffWithdrawal
	if err := build().
		Select("id, user_id, amount, status::text AS status, reject_reason, reviewed_by, reviewed_at, created_at").
		Order("created_at DESC").Offset(skip).Limit(pageSize).Scan(&withdrawals).Error; err != nil {
		return AffWithdrawalsResult{}, err
	}
	if withdrawals == nil {
		withdrawals = []models.AffWithdrawal{}
	}

	return AffWithdrawalsResult{
		Withdrawals: withdrawals,
		Total:       total,
		Page:        page,
		PageSize:    pageSize,
	}, nil
}

// ==================== admin operations ====================

// AffGetPendingWithdrawals mirrors getPendingAffWithdrawals. Despite the name it
// is not restricted to pending rows: the handler passes an optional status and
// otherwise lists every withdrawal for every user.
func AffGetPendingWithdrawals(ctx context.Context, options AffPendingWithdrawalListOptions) (AffPendingWithdrawalsResult, error) {
	page, pageSize := affClampPagination(options.Page, options.PageSize)
	status := affNormalizeWithdrawalStatus(options.Status)
	skip := (page - 1) * pageSize

	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.AffWithdrawal{})
		if status != "" {
			query = query.Where("status = ?", string(status))
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return AffPendingWithdrawalsResult{}, err
	}

	var withdrawals []models.AffWithdrawal
	if err := build().
		Select("id, user_id, amount, status::text AS status, reject_reason, reviewed_by, reviewed_at, created_at").
		Order("created_at DESC").Offset(skip).Limit(pageSize).Scan(&withdrawals).Error; err != nil {
		return AffPendingWithdrawalsResult{}, err
	}

	userIDs := make([]int32, 0, len(withdrawals))
	for _, withdrawal := range withdrawals {
		userIDs = append(userIDs, withdrawal.UserID)
	}
	users := map[int32]AffPendingWithdrawalUser{}
	if len(userIDs) > 0 {
		var rows []struct {
			ID         int32          `gorm:"column:id"`
			Username   string         `gorm:"column:username"`
			AffBalance models.Decimal `gorm:"column:aff_balance"`
		}
		if err := DB.WithContext(ctx).Raw(
			`SELECT id, username, aff_balance FROM users WHERE id IN ?`, userIDs,
		).Scan(&rows).Error; err != nil {
			return AffPendingWithdrawalsResult{}, err
		}
		for _, row := range rows {
			users[row.ID] = AffPendingWithdrawalUser{
				ID:         row.ID,
				Username:   row.Username,
				AffBalance: affDecimalToNumber(row.AffBalance),
			}
		}
	}

	items := make([]AffPendingWithdrawal, 0, len(withdrawals))
	for _, withdrawal := range withdrawals {
		items = append(items, AffPendingWithdrawal{
			ID:           withdrawal.ID,
			UserID:       withdrawal.UserID,
			Amount:       withdrawal.Amount,
			Status:       withdrawal.Status,
			RejectReason: withdrawal.RejectReason,
			ReviewedBy:   withdrawal.ReviewedBy,
			ReviewedAt:   withdrawal.ReviewedAt,
			CreatedAt:    withdrawal.CreatedAt,
			User:         users[withdrawal.UserID],
		})
	}

	return AffPendingWithdrawalsResult{
		Withdrawals: items,
		Total:       total,
		Page:        page,
		PageSize:    pageSize,
	}, nil
}

// AffApproveWithdrawal mirrors approveAffWithdrawal.
//
// Lock order (AFF 4112, then balance 4110) and the "read before lock, re-read
// after lock" sequence are copied from the TypeScript. The status guard on the
// UPDATE is what makes a withdrawal single-payable.
func AffApproveWithdrawal(ctx context.Context, withdrawalID, adminID int32) AffWithdrawalResult {
	tx, err := affBeginTx(ctx)
	if err != nil {
		return AffWithdrawalResult{Success: false, Error: err.Error()}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := affApproveWithdrawalInTransaction(ctx, tx, withdrawalID, adminID); err != nil {
		return AffWithdrawalResult{Success: false, Error: err.Error()}
	}
	if err := tx.Commit(ctx); err != nil {
		return AffWithdrawalResult{Success: false, Error: err.Error()}
	}
	return AffWithdrawalResult{Success: true}
}

// affApproveWithdrawalInTransaction mirrors the prisma.$transaction body.
func affApproveWithdrawalInTransaction(ctx context.Context, tx pgx.Tx, withdrawalID, adminID int32) error {
	type withdrawalSnapshot struct {
		id     int32
		userID int32
		amount models.Decimal
		status string
	}

	read := func() (*withdrawalSnapshot, error) {
		var (
			id     int32
			userID int32
			amount models.Decimal
			status string
		)
		err := tx.QueryRow(ctx,
			`SELECT id, user_id, amount, status::text FROM aff_withdrawals WHERE id = $1`,
			withdrawalID,
		).Scan(&id, &userID, &amount, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &withdrawalSnapshot{id: id, userID: userID, amount: amount, status: status}, nil
	}

	// 1. Pre-lock read: gives the error precedence the TypeScript has.
	withdrawal, err := read()
	if err != nil {
		return err
	}
	if withdrawal == nil {
		return errors.New("申请不存在")
	}
	if withdrawal.status != string(models.AffWithdrawalStatusPending) {
		return errors.New("申请已处理")
	}

	if err := AdvisoryTransactionLock(ctx, tx, UserAffBalanceLockNamespace, withdrawal.userID); err != nil {
		return err
	}
	if err := AdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, withdrawal.userID); err != nil {
		return err
	}

	// Re-read under the lock; a concurrent reviewer may have handled it.
	pendingWithdrawal, err := read()
	if err != nil {
		return err
	}
	if pendingWithdrawal == nil {
		return errors.New("申请不存在")
	}
	if pendingWithdrawal.status != string(models.AffWithdrawalStatusPending) {
		return errors.New("申请已处理")
	}

	var affBalance, balance models.Decimal
	err = tx.QueryRow(ctx, "SELECT aff_balance, balance FROM users WHERE id = $1", pendingWithdrawal.userID).
		Scan(&affBalance, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("用户不存在")
	}
	if err != nil {
		return err
	}

	// Note: the TypeScript converts the *pre-lock* row's amount, not the re-read
	// one (both address the same row, so the value cannot differ in practice).
	amount := affDecimalToNumber(withdrawal.amount)
	affBalanceBefore := affDecimalToNumber(affBalance)
	balanceBefore := affDecimalToNumber(balance)
	if affBalanceBefore < amount {
		return errors.New("AFF 余额不足")
	}

	// Flip the row only if it is still pending — the single-payable guard.
	tag, err := tx.Exec(ctx,
		`UPDATE aff_withdrawals
		    SET status = 'approved', reviewed_by = $2, reviewed_at = NOW()
		  WHERE id = $1 AND status = 'pending'`,
		withdrawalID, adminID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("申请已处理")
	}

	affBalanceAfter := affBalanceBefore - amount
	var balanceAfterDecimal models.Decimal
	if err := tx.QueryRow(ctx,
		`UPDATE users
		    SET aff_balance = aff_balance - $1, balance = balance + $1, updated_at = NOW()
		  WHERE id = $2
		  RETURNING balance`,
		affDecimalFromNumber(amount), pendingWithdrawal.userID,
	).Scan(&balanceAfterDecimal); err != nil {
		return err
	}
	balanceAfter := affDecimalToNumber(balanceAfterDecimal)

	affRemark := "转化申请 #" + strconv.FormatInt(int64(withdrawalID), 10) + " 审核通过"
	if _, err := affInsertLog(ctx, tx, affLogInsert{
		userID:        pendingWithdrawal.userID,
		logType:       models.AffLogTypeConvert,
		amount:        affDecimalFromNumber(-amount),
		balanceBefore: affDecimalFromNumber(affBalanceBefore),
		balanceAfter:  affDecimalFromNumber(affBalanceAfter),
		remark:        &affRemark,
	}); err != nil {
		return err
	}

	balanceRemark := "AFF 余额转化 #" + strconv.FormatInt(int64(withdrawalID), 10)
	if _, err := insertBalanceLog(ctx, tx, balanceLogInsert{
		userID:        pendingWithdrawal.userID,
		logType:       models.BalanceLogTypeGift,
		amount:        affDecimalFromNumber(amount),
		balanceBefore: affDecimalFromNumber(balanceBefore),
		balanceAfter:  affDecimalFromNumber(balanceAfter),
		remark:        &balanceRemark,
	}); err != nil {
		return err
	}

	return nil
}

// AffRejectWithdrawal mirrors rejectAffWithdrawal. No money moves: a pending
// withdrawal was never debited, so rejection only records the decision.
func AffRejectWithdrawal(ctx context.Context, withdrawalID, adminID int32, reason string) AffWithdrawalResult {
	result := DB.WithContext(ctx).Exec(
		`UPDATE aff_withdrawals
		    SET status = 'rejected', reject_reason = ?, reviewed_by = ?, reviewed_at = NOW()
		  WHERE id = ? AND status = 'pending'`,
		reason, adminID, withdrawalID)
	if result.Error != nil {
		return AffWithdrawalResult{Success: false, Error: result.Error.Error()}
	}
	if result.RowsAffected != 1 {
		exists, err := AffGetWithdrawalByID(ctx, withdrawalID)
		if err != nil {
			return AffWithdrawalResult{Success: false, Error: err.Error()}
		}
		if exists != nil {
			return AffWithdrawalResult{Success: false, Error: "申请已处理"}
		}
		return AffWithdrawalResult{Success: false, Error: "申请不存在"}
	}
	return AffWithdrawalResult{Success: true}
}

// ==================== leaderboard ====================

// AffGetLeaderboard mirrors getAffLeaderboard: top 10 by summed positive AFF
// earnings, usernames masked, the caller flagged.
func AffGetLeaderboard(ctx context.Context, currentUserID *int32) ([]AffLeaderboardEntry, error) {
	var rows []struct {
		UserID int32          `gorm:"column:user_id"`
		Total  models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT user_id, SUM(amount) AS total
		   FROM aff_logs
		  WHERE amount > 0
		  GROUP BY user_id
		  ORDER BY SUM(amount) DESC
		  LIMIT 10`,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	entries := make([]AffLeaderboardEntry, 0, len(rows))
	if len(rows) == 0 {
		return entries, nil
	}

	userIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		userIDs = append(userIDs, row.UserID)
	}
	usernames := map[int32]string{}
	var users []struct {
		ID       int32  `gorm:"column:id"`
		Username string `gorm:"column:username"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, username FROM users WHERE id IN ?`, userIDs,
	).Scan(&users).Error; err != nil {
		return nil, err
	}
	for _, user := range users {
		usernames[user.ID] = user.Username
	}

	for index, row := range rows {
		username, ok := usernames[row.UserID]
		if !ok || username == "" {
			username = "Unknown"
		}
		entries = append(entries, AffLeaderboardEntry{
			Rank:          index + 1,
			Username:      affMaskUsername(username),
			TotalEarnings: affDecimalToNumber(row.Total),
			IsCurrentUser: currentUserID != nil && row.UserID == *currentUserID,
		})
	}
	return entries, nil
}

// ==================== mail subscriptions ====================

// AffValidateMailCode mirrors validateMailAffCode: mail subscriptions accept
// global codes only.
func AffValidateMailCode(ctx context.Context, code string, userID int32) (AffValidateCodeResult, error) {
	var affCode models.AffCode
	err := DB.WithContext(ctx).Raw(
		`SELECT id, code, user_id, package_plan_id, discount_rate, commission_rate,
		        enabled, used_count, total_earnings, created_at
		   FROM aff_codes WHERE code = ?`, strings.ToUpper(code),
	).Scan(&affCode).Error
	if err != nil {
		return AffValidateCodeResult{}, err
	}
	if affCode.ID == 0 {
		return AffValidateCodeResult{Valid: false, Error: "优惠码不存在"}, nil
	}

	if affCode.PackagePlanID != nil {
		return AffValidateCodeResult{Valid: false, Error: "该优惠码仅适用于特定实例套餐，不支持邮箱订阅"}, nil
	}
	if affCode.UserID == userID {
		return AffValidateCodeResult{Valid: false, Error: "不能使用自己的优惠码"}, nil
	}
	if !affCode.Enabled {
		return AffValidateCodeResult{Valid: false, Error: "优惠码已禁用"}, nil
	}

	discountRate := affDecimalToNumber(affCode.DiscountRate)
	commissionRate := affDecimalToNumber(affCode.CommissionRate)
	return AffValidateCodeResult{
		Valid:          true,
		AffCode:        &affCode,
		DiscountRate:   &discountRate,
		CommissionRate: &commissionRate,
	}, nil
}

// AffCreateMailBinding mirrors createMailAffBinding.
func AffCreateMailBinding(ctx context.Context, mailSubscriptionID, affCodeID int32, tx pgx.Tx) error {
	if tx != nil {
		_, err := tx.Exec(ctx,
			"INSERT INTO mail_subscription_aff_bindings (mail_subscription_id, aff_code_id) VALUES ($1, $2)",
			mailSubscriptionID, affCodeID)
		return err
	}
	return DB.WithContext(ctx).Exec(
		`INSERT INTO mail_subscription_aff_bindings (mail_subscription_id, aff_code_id) VALUES (?, ?)`,
		mailSubscriptionID, affCodeID,
	).Error
}

// AffProcessMailCommission mirrors processMailAffCommission (same shape as the
// instance variant, with the mail subscription column and texts).
func AffProcessMailCommission(ctx context.Context, affCodeID, mailSubscriptionID int32, originalAmount float64, logType models.AffLogType, tx pgx.Tx) error {
	affCode, err := affFindCodeByID(ctx, affCodeID, tx)
	if err != nil {
		return err
	}
	if affCode == nil {
		return nil
	}

	commissionRate := affDecimalToNumber(affCode.CommissionRate)
	commission := affJSRound(originalAmount*commissionRate*100) / 100
	if commission <= 0 {
		return nil
	}

	remark := "邮箱续费返利"
	if logType == models.AffLogTypeNewPurchase {
		remark = "邮箱新购返利"
	}

	AffChangeBalance(ctx, AffBalanceChangeInput{
		UserID:             affCode.UserID,
		Type:               logType,
		Amount:             commission,
		AffCodeID:          &affCodeID,
		MailSubscriptionID: &mailSubscriptionID,
		OriginalAmount:     &originalAmount,
		Remark:             &remark,
	}, tx)

	return affIncrementCodeStats(ctx, affCodeID, commission, logType == models.AffLogTypeNewPurchase, tx)
}

// AffGetMailSubscriptionBinding mirrors getMailSubscriptionAffBinding.
func AffGetMailSubscriptionBinding(ctx context.Context, mailSubscriptionID int32) (*AffInstanceBinding, error) {
	const query = `SELECT ` + affCodeColumns + `
	                 FROM mail_subscription_aff_bindings b
	                 JOIN aff_codes c ON c.id = b.aff_code_id
	                WHERE b.mail_subscription_id = $1`

	querier, err := affCodeQuerier(nil)
	if err != nil {
		return nil, err
	}
	affCode, err := affQueryCodeRow(ctx, querier, query, mailSubscriptionID)
	if err != nil || affCode == nil {
		return nil, err
	}
	return &AffInstanceBinding{AffCode: *affCode, UserID: affCode.UserID}, nil
}
