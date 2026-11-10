// Port of server/src/db/users.ts (862 lines, 27 exports).
//
// The whole module is ported, not a selection: the Node 2FA flow is a FOUR-write
// state machine (save2FASecret → enable2FA → save2FARecoveryCodes →
// enable2FAWithRecoveryCodes) and dropping any one of the writes leaves a user
// with 2FA enabled but no recovery codes — a window the Node backend has actually
// shipped before. `Disable2FAComplete` is the rollback that clears secret AND
// recovery codes.
//
// Encryption boundary: the TypeScript `db/users.ts` stores whatever string the
// caller hands it. Every caller passes the output of `encryptSensitiveData`
// (server/src/routes/auth.ts, scripts/smoke-split-auth.ts), so this layer MUST NOT
// encrypt again — double encryption would make `verify2FAToken` reject valid
// codes. The Go callers use lib.EncryptSensitiveData / lib.DecryptSensitiveData.
//
// Contract notes that are easy to get wrong:
//   - Lookups return (nil, nil) when the row is absent, NOT an error.
//   - Unique-constraint violations are mapped to sentinels (ErrUserExists /
//     ErrEmailAlreadyRegistered) so the routes can answer 400 USER_EXISTS exactly
//     like the TypeScript (`reply.code(400).send(apiError(ErrorCode.USER_EXISTS))`
//     in routes/auth.ts). WAVE_D_SPEC §1 says "409 / USERNAME_EXISTS"; the code
//     that actually runs (and the only code `lib/errors.ts` defines) is
//     400 / USER_EXISTS, so the TypeScript wins.
//   - Quota traffic columns are BigInt → int64, never float.
//   - Every update with an `Object.keys(updateData).length === 0` guard keeps the
//     early return (Prisma would otherwise run an empty UPDATE that only bumps
//     @updatedAt).
package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// maxRegisterGiftBalance mirrors MAX_REGISTER_GIFT_BALANCE.
	maxRegisterGiftBalance = 99999999.99
	// maxRegisterGiftPoints mirrors MAX_REGISTER_GIFT_POINTS.
	maxRegisterGiftPoints = 2147483647
)

// Sentinels mirroring the string errors the TypeScript throws. Routes map them to
// HTTP status codes; never compare error strings.
var (
	// ErrUserExists mirrors `new Error('USER_EXISTS')` (and Prisma P2002 on
	// users.username).
	ErrUserExists = errors.New("USER_EXISTS")
	// ErrEmailAlreadyRegistered mirrors `new Error('EMAIL_ALREADY_REGISTERED')`.
	ErrEmailAlreadyRegistered = errors.New("EMAIL_ALREADY_REGISTERED")
	// ErrInviteCodeUnavailable mirrors `new Error('INVITE_CODE_UNAVAILABLE')`.
	ErrInviteCodeUnavailable = errors.New("INVITE_CODE_UNAVAILABLE")
	// ErrUserDeletionConflict mirrors UserDeletionConflictError. Its message keeps
	// the Node class's wording so logs stay greppable.
	ErrUserDeletionConflict = errors.New("User has associated records and cannot be deleted")
	// ErrUsersDatabaseNotInitialized is returned by write helpers when db.Init has
	// not run, so an unwired call site fails loudly instead of panicking.
	ErrUsersDatabaseNotInitialized = errors.New("users: database not initialized")
	// ErrUsersRecordNotFound mirrors Prisma P2025 ("record to update not found"):
	// Prisma's update() throws instead of silently updating zero rows.
	ErrUsersRecordNotFound = errors.New("users: record to update not found")
)

// AvatarStyles mirrors AVATAR_STYLES (order is part of the route schema enum).
var AvatarStyles = []string{
	"adventurer", "adventurerNeutral", "avataaars", "avataaarsNeutral",
	"bigEars", "bigEarsNeutral", "bigSmile", "bottts", "botttsNeutral",
	"croodles", "croodlesNeutral", "dylan", "funEmoji", "glass", "icons",
	"identicon", "initials", "lorelei", "loreleiNeutral", "micah", "miniavs",
	"notionists", "notionistsNeutral", "openPeeps", "personas", "pixelArt",
	"pixelArtNeutral", "rings", "shapes", "thumbs",
}

// ==================== types ====================

// UserRecord mirrors the `User` interface of server/src/types/database.ts: the
// snake_case wire projection every lookup returns. Timestamps are already
// ISO-8601 strings because the TypeScript layer calls `.toISOString()`.
type UserRecord struct {
	ID            int32             `json:"id"`
	Username      string            `json:"username"`
	Email         *string           `json:"email"`
	PasswordHash  string            `json:"password_hash"`
	Role          models.UserRole   `json:"role"`
	Status        models.UserStatus `json:"status"`
	BanReason     *string           `json:"ban_reason"`
	AvatarStyle   string            `json:"avatar_style"`
	AvatarBadgeID *string           `json:"avatar_badge_id"`
	CreatedAt     string            `json:"created_at"`
	UpdatedAt     string            `json:"updated_at"`
}

// UserQuotaRecord mirrors the `UserQuota` interface of types/database.ts.
// `monthly_traffic_limit` is `bigint | null` and the two other traffic counters
// are `bigint` in the TypeScript; here they are int64 so no precision is lost.
type UserQuotaRecord struct {
	ID                  int32                `json:"id"`
	UserID              int32                `json:"user_id"`
	HostLimit           int32                `json:"host_limit"`
	HostUsed            int32                `json:"host_used"`
	FriendLimit         int32                `json:"friend_limit"`
	FriendUsed          int32                `json:"friend_used"`
	PackageLimit        int32                `json:"package_limit"`
	PackageUsed         int32                `json:"package_used"`
	MonthlyTrafficLimit *int64               `json:"monthly_traffic_limit"`
	MonthlyTrafficUsed  int64                `json:"monthly_traffic_used"`
	ExtraTrafficQuota   int64                `json:"extra_traffic_quota"`
	TrafficStatus       models.TrafficStatus `json:"traffic_status"`
}

// UserWithQuotaRecord mirrors `User & Partial<UserQuota>` from getAllUsersWithQuota.
type UserWithQuotaRecord struct {
	UserRecord
	HostLimit           *int32                `json:"host_limit,omitempty"`
	HostUsed            *int32                `json:"host_used,omitempty"`
	FriendLimit         *int32                `json:"friend_limit,omitempty"`
	FriendUsed          *int32                `json:"friend_used,omitempty"`
	PackageLimit        *int32                `json:"package_limit,omitempty"`
	PackageUsed         *int32                `json:"package_used,omitempty"`
	MonthlyTrafficLimit *string               `json:"monthly_traffic_limit,omitempty"`
	MonthlyTrafficUsed  *string               `json:"monthly_traffic_used,omitempty"`
	ExtraTrafficQuota   *string               `json:"extra_traffic_quota,omitempty"`
	TrafficStatus       *models.TrafficStatus `json:"traffic_status,omitempty"`
}

// UserQuotaUpdate mirrors the partial `data` object of updateUserQuotaLimits.
type UserQuotaUpdate struct {
	HostLimit    *int32
	FriendLimit  *int32
	PackageLimit *int32
}

// UserUpdate mirrors the `data` object of updateUser.
type UserUpdate struct {
	Email        *string
	Role         *models.UserRole
	Status       *models.UserStatus
	PasswordHash *string
	AvatarStyle  *string
}

// UserQuotaUsedUpdate mirrors updateUserQuotaUsed's `data` object.
type UserQuotaUsedUpdate struct {
	HostUsed    *int32
	FriendUsed  *int32
	PackageUsed *int32
}

// RegisterGiftResult mirrors RegisterGiftResult.
type RegisterGiftResult struct {
	BalanceAmount float64 `json:"balanceAmount"`
	PointsAmount  int32   `json:"pointsAmount"`
	NewBalance    float64 `json:"newBalance"`
	NewPoints     int32   `json:"newPoints"`
}

// CreateRegisteredUserResult mirrors CreateRegisteredUserResult.
type CreateRegisteredUserResult struct {
	UserID       int32               `json:"userId"`
	RegisterGift *RegisterGiftResult `json:"registerGift"`
}

// CreateRegisteredUserInput mirrors createRegisteredUser's input object.
type CreateRegisteredUserInput struct {
	Username     string
	Email        *string
	PasswordHash string
	InviteCode   *string
}

// TwoFAStatus mirrors getUser2FAStatus's return shape.
type TwoFAStatus struct {
	Enabled   bool `json:"enabled"`
	HasSecret bool `json:"hasSecret"`
}

type registerGiftConfig struct {
	balanceAmount float64
	pointsAmount  int32
}

// ==================== helpers ====================

// usersISOTime mirrors Date.prototype.toISOString().
func usersISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// decimalFromFloat2 converts an already-rounded 2-decimal JS number into a
// Decimal without float artefacts (the value is re-parsed from its fixed form).
func decimalFromFloat2(value float64) decimal.Decimal {
	parsed, err := decimal.NewFromString(strconv.FormatFloat(value, 'f', 2, 64))
	if err != nil {
		return decimal.Zero
	}
	return parsed
}

// marshalUsersJSON is intentionally absent: UserRecord carries snake_case json
// tags directly, so encoding/json produces the TypeScript wire shape.

// usersIsUniqueViolation mirrors `error.code === 'P2002'`.
func usersIsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// usersIsForeignKeyViolation mirrors Prisma P2003 (foreign key constraint failed).
func usersIsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// usersIsRequiredRelationViolation mirrors Prisma P2014 (a required relation
// would be violated). PostgreSQL reports this as a RESTRICT foreign-key error,
// which surfaces as 23503 as well.
func usersIsRequiredRelationViolation(err error) bool {
	return usersIsForeignKeyViolation(err)
}

// usersRequireDB fails loudly when db.Init has not run.
func usersRequireDB() error {
	if DB == nil {
		return ErrUsersDatabaseNotInitialized
	}
	return nil
}

// usersExecExpectRow runs an UPDATE/DELETE and mirrors Prisma's update/delete
// semantics: zero matched rows is P2025, not a silent success.
func usersExecExpectRow(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrUsersRecordNotFound
	}
	return nil
}

// userRow is the raw users projection shared by every lookup.
type userRow struct {
	ID            int32             `gorm:"column:id"`
	Username      string            `gorm:"column:username"`
	Email         *string           `gorm:"column:email"`
	PasswordHash  string            `gorm:"column:password_hash"`
	Role          models.UserRole   `gorm:"column:role"`
	Status        models.UserStatus `gorm:"column:status"`
	BanReason     *string           `gorm:"column:ban_reason"`
	AvatarStyle   string            `gorm:"column:avatar_style"`
	AvatarBadgeID *string           `gorm:"column:avatar_badge_id"`
	CreatedAt     time.Time         `gorm:"column:created_at"`
	UpdatedAt     time.Time         `gorm:"column:updated_at"`
}

const usersSelectColumns = `id, username, email, password_hash, role::text AS role, status::text AS status,
	ban_reason, avatar_style, avatar_badge_id, created_at, updated_at`

func (r userRow) toRecord() UserRecord {
	return UserRecord{
		ID:            r.ID,
		Username:      r.Username,
		Email:         r.Email,
		PasswordHash:  r.PasswordHash,
		Role:          r.Role,
		Status:        r.Status,
		BanReason:     r.BanReason,
		AvatarStyle:   r.AvatarStyle,
		AvatarBadgeID: r.AvatarBadgeID,
		CreatedAt:     usersISOTime(r.CreatedAt),
		UpdatedAt:     usersISOTime(r.UpdatedAt),
	}
}

func usersQueryFirst(ctx context.Context, query string, args ...any) (*UserRecord, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []userRow
	if err := DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	record := rows[0].toRecord()
	return &record, nil
}

// usersQuotaRow is the raw user_quotas projection.
type usersQuotaRow struct {
	ID                  int32                `gorm:"column:id"`
	UserID              int32                `gorm:"column:user_id"`
	HostLimit           int32                `gorm:"column:host_limit"`
	HostUsed            int32                `gorm:"column:host_used"`
	FriendLimit         int32                `gorm:"column:friend_limit"`
	FriendUsed          int32                `gorm:"column:friend_used"`
	PackageLimit        int32                `gorm:"column:package_limit"`
	PackageUsed         int32                `gorm:"column:package_used"`
	MonthlyTrafficLimit *int64               `gorm:"column:monthly_traffic_limit"`
	MonthlyTrafficUsed  int64                `gorm:"column:monthly_traffic_used"`
	ExtraTrafficQuota   int64                `gorm:"column:extra_traffic_quota"`
	TrafficStatus       models.TrafficStatus `gorm:"column:traffic_status"`
}

func (r usersQuotaRow) toRecord() UserQuotaRecord {
	return UserQuotaRecord{
		ID:                  r.ID,
		UserID:              r.UserID,
		HostLimit:           r.HostLimit,
		HostUsed:            r.HostUsed,
		FriendLimit:         r.FriendLimit,
		FriendUsed:          r.FriendUsed,
		PackageLimit:        r.PackageLimit,
		PackageUsed:         r.PackageUsed,
		MonthlyTrafficLimit: r.MonthlyTrafficLimit,
		MonthlyTrafficUsed:  r.MonthlyTrafficUsed,
		ExtraTrafficQuota:   r.ExtraTrafficQuota,
		TrafficStatus:       r.TrafficStatus,
	}
}

const usersQuotaSelectColumns = `id, user_id, host_limit, host_used, friend_limit, friend_used,
	package_limit, package_used, monthly_traffic_limit, monthly_traffic_used,
	extra_traffic_quota, traffic_status`

// ==================== lookups ====================
// Every lookup returns (nil, nil) when the row is absent, mirroring Prisma's
// findUnique/findFirst returning null.

// FindUserByUsername mirrors findUserByUsername.
func FindUserByUsername(ctx context.Context, username string) (*UserRecord, error) {
	return usersQueryFirst(ctx, "SELECT "+usersSelectColumns+" FROM users WHERE username = ? LIMIT 1", username)
}

// FindUserByUsernameOrEmail mirrors findUserByUsernameOrEmail: exact username
// first, then a case-insensitive email match on the trimmed identifier.
//
// The TypeScript uses findFirst without an order for the email branch and notes
// that duplicate emails "should not" exist. id ASC is added so the choice is
// deterministic when legacy duplicates do exist.
func FindUserByUsernameOrEmail(ctx context.Context, identifier string) (*UserRecord, error) {
	byUsername, err := usersQueryFirst(ctx, "SELECT "+usersSelectColumns+" FROM users WHERE username = ? LIMIT 1", identifier)
	if err != nil {
		return nil, err
	}
	if byUsername != nil {
		return byUsername, nil
	}

	normalized := strings.TrimSpace(identifier)
	return usersQueryFirst(ctx,
		"SELECT "+usersSelectColumns+" FROM users WHERE LOWER(email) = LOWER(?) ORDER BY id ASC LIMIT 1", normalized)
}

// FindUserById mirrors findUserById.
func FindUserById(ctx context.Context, id int32) (*UserRecord, error) {
	return usersQueryFirst(ctx, "SELECT "+usersSelectColumns+" FROM users WHERE id = ? LIMIT 1", id)
}

// FindUserByEmail mirrors findUserByEmail (case-insensitive, trimmed).
func FindUserByEmail(ctx context.Context, email string) (*UserRecord, error) {
	normalized := strings.TrimSpace(email)
	return usersQueryFirst(ctx,
		"SELECT "+usersSelectColumns+" FROM users WHERE LOWER(email) = LOWER(?) ORDER BY id ASC LIMIT 1", normalized)
}

// ==================== create ====================

// CreateUser mirrors createUser. The role argument is the Go equivalent of the
// TypeScript default parameter: pass "" for 'user'.
//
// Prisma's nested `quota: { create: ... }` is two INSERTs; they run in one
// transaction here and a unique violation on users.username maps to ErrUserExists.
func CreateUser(ctx context.Context, username string, email *string, passwordHash string, role models.UserRole) (int32, error) {
	if role == "" {
		role = models.UserRoleUser
	}
	if err := usersRequireDB(); err != nil {
		return 0, err
	}

	defaultQuota, err := GetDefaultQuotaConfig(ctx)
	if err != nil {
		return 0, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID int32
	// users.updated_at is NOT NULL with no database default: Prisma's @updatedAt
	// fills it client-side, so a raw INSERT must supply it. Omitting it makes every
	// call fail with a not-null violation at runtime (registration was broken).
	err = tx.QueryRow(ctx,
		`INSERT INTO users (username, email, password_hash, role, avatar_style, updated_at)
		 VALUES ($1, $2, $3, $4, '', $5)
		 RETURNING id`,
		username, email, passwordHash, string(role), time.Now().UTC(),
	).Scan(&userID)
	if err != nil {
		if usersIsUniqueViolation(err) {
			return 0, ErrUserExists
		}
		return 0, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO user_quotas (user_id, host_limit, friend_limit, package_limit)
		 VALUES ($1, $2, $3, $4)`,
		userID, defaultQuota.HostLimit, defaultQuota.FriendLimit, defaultQuota.PackageLimit,
	); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return userID, nil
}

// normalizeRegisterGift mirrors the clamping block of
// getFreeSiteRegisterGiftConfig:
//
//	balance = Math.min(MAX, Math.max(0, Math.round(raw * 100) / 100))
//	points  = Math.min(MAX, Math.max(0, Math.floor(raw)))
func normalizeRegisterGift(rawBalanceAmount float64, rawPointsAmount int) (balanceAmount float64, pointsAmount int32) {
	balanceAmount = math.Min(maxRegisterGiftBalance, math.Max(0, math.Round(rawBalanceAmount*100)/100))
	pointsAmount = int32(math.Min(maxRegisterGiftPoints, math.Max(0, math.Floor(float64(rawPointsAmount)))))
	return balanceAmount, pointsAmount
}

// getFreeSiteRegisterGiftConfig mirrors getFreeSiteRegisterGiftConfig.
func getFreeSiteRegisterGiftConfig(ctx context.Context) (*registerGiftConfig, error) {
	freeSiteMode, err := GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		return nil, err
	}
	giftEnabled, err := GetSystemConfigBoolean(ctx, "free_site_register_gift_enabled", false)
	if err != nil {
		return nil, err
	}
	if !freeSiteMode || !giftEnabled {
		return nil, nil
	}

	rawBalanceAmount, err := GetSystemConfigFloat(ctx, "free_site_register_gift_balance", 0)
	if err != nil {
		return nil, err
	}
	rawPointsAmount, err := GetSystemConfigNumber(ctx, "free_site_register_gift_points", 0)
	if err != nil {
		return nil, err
	}

	balanceAmount, pointsAmount := normalizeRegisterGift(rawBalanceAmount, rawPointsAmount)
	if balanceAmount <= 0 && pointsAmount <= 0 {
		return nil, nil
	}
	return &registerGiftConfig{
		balanceAmount: balanceAmount,
		pointsAmount:  pointsAmount,
	}, nil
}

// buildRegisterGiftMessage mirrors buildRegisterGiftMessage.
func buildRegisterGiftMessage(gift RegisterGiftResult) (summary string, content string) {
	giftParts := make([]string, 0, 2)
	if gift.BalanceAmount > 0 {
		// `余额 ¥${gift.balanceAmount.toFixed(2)}`
		giftParts = append(giftParts, "余额 ¥"+strconv.FormatFloat(gift.BalanceAmount, 'f', 2, 64))
	}
	if gift.PointsAmount > 0 {
		giftParts = append(giftParts, strconv.FormatInt(int64(gift.PointsAmount), 10)+" 积分")
	}

	summary = strings.Join(giftParts, " + ")
	arrivalLabel := "白嫖补给"
	if len(giftParts) > 1 {
		arrivalLabel = "余额和积分"
	} else if len(giftParts) == 1 && giftParts[0] != "" {
		arrivalLabel = giftParts[0]
	}

	content = "欢迎加入白嫖站！系统已经把 " + summary + " 塞进你的账户背包，" + arrivalLabel +
		"到账了。快去逛逛能薅点什么，优雅白嫖，从注册成功这一刻开始。"
	return summary, content
}

// applyFreeSiteRegisterGiftInTransaction mirrors
// applyFreeSiteRegisterGiftInTransaction. It must run on the registration
// transaction so the balance lock, the ledger rows and the inbox message commit
// together.
func applyFreeSiteRegisterGiftInTransaction(ctx context.Context, tx pgx.Tx, userID int32, giftConfig registerGiftConfig) (RegisterGiftResult, error) {
	balanceAmount := giftConfig.balanceAmount
	pointsAmount := giftConfig.pointsAmount
	var newBalance float64
	var newPoints int32

	if balanceAmount > 0 {
		locked, err := TryAdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID)
		if err != nil {
			return RegisterGiftResult{}, err
		}
		if !locked {
			return RegisterGiftResult{}, errors.New("余额正在处理，请稍后重试")
		}

		var balanceBefore models.Decimal
		err = tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", userID).Scan(&balanceBefore)
		if errors.Is(err, pgx.ErrNoRows) {
			return RegisterGiftResult{}, errors.New("用户不存在")
		}
		if err != nil {
			return RegisterGiftResult{}, err
		}

		increment := models.Decimal{Decimal: decimalFromFloat2(balanceAmount)}
		var balanceAfter models.Decimal
		err = tx.QueryRow(ctx,
			"UPDATE users SET balance = balance + $1, updated_at = NOW() WHERE id = $2 RETURNING balance",
			increment, userID,
		).Scan(&balanceAfter)
		if err != nil {
			return RegisterGiftResult{}, err
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO balance_logs (user_id, type, amount, balance_before, balance_after, remark, created_at)
			 VALUES ($1, 'gift', $2, $3, $4, $5, NOW())`,
			userID, increment, balanceBefore, balanceAfter, "白嫖站注册自动赠送：白嫖启动资金",
		); err != nil {
			return RegisterGiftResult{}, err
		}

		newBalance = decimalToFloat64(balanceAfter)
	} else {
		var balance models.Decimal
		err := tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", userID).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			newBalance = 0
		} else if err != nil {
			return RegisterGiftResult{}, err
		} else {
			newBalance = decimalToFloat64(balance)
		}
	}

	if pointsAmount > 0 {
		// tx.userPoints.upsert({ where: { userId }, update: {}, create: { userId } })
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_points (user_id, points, total_earned, total_spent, converted_consume, created_at, updated_at)
			 VALUES ($1, 0, 0, 0, 0, NOW(), NOW())
			 ON CONFLICT (user_id) DO NOTHING`,
			userID,
		); err != nil {
			return RegisterGiftResult{}, err
		}

		var pointsBefore int32
		if err := tx.QueryRow(ctx, "SELECT points FROM user_points WHERE user_id = $1", userID).Scan(&pointsBefore); err != nil {
			return RegisterGiftResult{}, err
		}

		var pointsAfter int32
		if err := tx.QueryRow(ctx,
			`UPDATE user_points
			    SET points = points + $1, total_earned = total_earned + $1, updated_at = NOW()
			  WHERE user_id = $2
			  RETURNING points`,
			pointsAmount, userID,
		).Scan(&pointsAfter); err != nil {
			return RegisterGiftResult{}, err
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO points_logs (user_id, type, amount, points_before, points_after, remark, created_at)
			 VALUES ($1, 'admin_adjust', $2, $3, $4, $5, NOW())`,
			userID, pointsAmount, pointsBefore, pointsAfter, "白嫖站注册自动赠送：白嫖能量包",
		); err != nil {
			return RegisterGiftResult{}, err
		}

		newPoints = pointsAfter
	} else {
		var points int32
		err := tx.QueryRow(ctx, "SELECT points FROM user_points WHERE user_id = $1", userID).Scan(&points)
		if errors.Is(err, pgx.ErrNoRows) {
			newPoints = 0
		} else if err != nil {
			return RegisterGiftResult{}, err
		} else {
			newPoints = points
		}
	}

	gift := RegisterGiftResult{
		BalanceAmount: balanceAmount,
		PointsAmount:  pointsAmount,
		NewBalance:    newBalance,
		NewPoints:     newPoints,
	}
	_, content := buildRegisterGiftMessage(gift)

	// tx.inboxMessage.create({ data: { ..., data: { balanceAmount, pointsAmount, newBalance, newPoints } } })
	inboxData := fmt.Sprintf(`{"balanceAmount":%s,"pointsAmount":%d,"newBalance":%s,"newPoints":%d}`,
		strconv.FormatFloat(balanceAmount, 'f', -1, 64),
		pointsAmount,
		strconv.FormatFloat(newBalance, 'f', -1, 64),
		newPoints,
	)
	if _, err := tx.Exec(ctx,
		`INSERT INTO inbox_messages (user_id, event_type, title, content, data, created_at)
		 VALUES ($1, 'free_site_register_gift', '白嫖补给到账啦', $2, $3::jsonb, NOW())`,
		userID, content, inboxData,
	); err != nil {
		return RegisterGiftResult{}, err
	}

	return gift, nil
}

// CreateRegisteredUser mirrors createRegisteredUser.
//
// The email-registration advisory lock (USER_EMAIL_REGISTRATION_LOCK_NAMESPACE =
// 4117) serializes concurrent registrations for the same email so the
// case-insensitive duplicate check cannot race. Unique violations map to
// ErrUserExists exactly like the TypeScript P2002 branch.
func CreateRegisteredUser(ctx context.Context, input CreateRegisteredUserInput) (CreateRegisteredUserResult, error) {
	if err := usersRequireDB(); err != nil {
		return CreateRegisteredUserResult{}, err
	}

	// The TypeScript resolves both config reads in parallel before the transaction.
	defaultQuota, err := GetDefaultQuotaConfig(ctx)
	if err != nil {
		return CreateRegisteredUserResult{}, err
	}
	giftConfig, err := getFreeSiteRegisterGiftConfig(ctx)
	if err != nil {
		return CreateRegisteredUserResult{}, err
	}

	var normalizedEmail *string
	if input.Email != nil {
		value := strings.ToLower(strings.TrimSpace(*input.Email))
		if value != "" {
			normalizedEmail = &value
		}
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return CreateRegisteredUserResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if normalizedEmail != nil {
		if err := AdvisoryTransactionLockString(ctx, tx, UserEmailRegistrationLockNamespace, *normalizedEmail); err != nil {
			return CreateRegisteredUserResult{}, err
		}
	}

	var existingUsernameID int32
	err = tx.QueryRow(ctx, "SELECT id FROM users WHERE username = $1 LIMIT 1", input.Username).Scan(&existingUsernameID)
	switch {
	case err == nil:
		return CreateRegisteredUserResult{}, ErrUserExists
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return CreateRegisteredUserResult{}, err
	}

	if normalizedEmail != nil {
		var existingEmailID int32
		err = tx.QueryRow(ctx,
			"SELECT id FROM users WHERE LOWER(email) = LOWER($1) ORDER BY id ASC LIMIT 1", *normalizedEmail,
		).Scan(&existingEmailID)
		switch {
		case err == nil:
			return CreateRegisteredUserResult{}, ErrEmailAlreadyRegistered
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return CreateRegisteredUserResult{}, err
		}
	}

	var userID int32
	// See CreateUser: updated_at has no database default and Prisma fills it
	// client-side, so this INSERT must supply it or registration 500s.
	err = tx.QueryRow(ctx,
		`INSERT INTO users (username, email, password_hash, role, avatar_style, updated_at)
		 VALUES ($1, $2, $3, 'user', '', $4)
		 RETURNING id`,
		input.Username, normalizedEmail, input.PasswordHash, time.Now().UTC(),
	).Scan(&userID)
	if err != nil {
		if usersIsUniqueViolation(err) {
			return CreateRegisteredUserResult{}, ErrUserExists
		}
		return CreateRegisteredUserResult{}, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO user_quotas (user_id, host_limit, friend_limit, package_limit)
		 VALUES ($1, $2, $3, $4)`,
		userID, defaultQuota.HostLimit, defaultQuota.FriendLimit, defaultQuota.PackageLimit,
	); err != nil {
		return CreateRegisteredUserResult{}, err
	}

	if input.InviteCode != nil {
		tag, err := tx.Exec(ctx,
			`UPDATE invite_codes
			    SET used_by = $1, used_at = NOW()
			  WHERE code = $2
			    AND used_by IS NULL
			    AND (expires_at IS NULL OR expires_at > NOW())`,
			userID, *input.InviteCode,
		)
		if err != nil {
			return CreateRegisteredUserResult{}, err
		}
		if tag.RowsAffected() != 1 {
			return CreateRegisteredUserResult{}, ErrInviteCodeUnavailable
		}
	}

	var registerGift *RegisterGiftResult
	if giftConfig != nil {
		gift, err := applyFreeSiteRegisterGiftInTransaction(ctx, tx, userID, *giftConfig)
		if err != nil {
			return CreateRegisteredUserResult{}, err
		}
		registerGift = &gift
	}

	if err := tx.Commit(ctx); err != nil {
		if usersIsUniqueViolation(err) {
			return CreateRegisteredUserResult{}, ErrUserExists
		}
		return CreateRegisteredUserResult{}, err
	}

	return CreateRegisteredUserResult{UserID: userID, RegisterGift: registerGift}, nil
}

// ==================== list ====================

// GetAllUsers mirrors getAllUsers. Note the TypeScript deliberately reuses
// createdAt for updated_at because the query does not select updatedAt.
func GetAllUsers(ctx context.Context) ([]UserRecord, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []userRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT " + usersSelectColumns + " FROM users ORDER BY id ASC",
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	users := make([]UserRecord, 0, len(rows))
	for _, row := range rows {
		record := row.toRecord()
		record.PasswordHash = "" // 不返回密码哈希
		record.UpdatedAt = record.CreatedAt
		users = append(users, record)
	}
	return users, nil
}

// GetAllUsersWithQuota mirrors getAllUsersWithQuota.
func GetAllUsersWithQuota(ctx context.Context) ([]UserWithQuotaRecord, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []userRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT " + usersSelectColumns + " FROM users ORDER BY id ASC",
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	var quotaRows []usersQuotaRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT " + usersQuotaSelectColumns + " FROM user_quotas",
	).Scan(&quotaRows).Error; err != nil {
		return nil, err
	}
	quotaByUser := make(map[int32]usersQuotaRow, len(quotaRows))
	for _, quota := range quotaRows {
		quotaByUser[quota.UserID] = quota
	}

	users := make([]UserWithQuotaRecord, 0, len(rows))
	for _, row := range rows {
		record := row.toRecord()
		record.PasswordHash = ""
		item := UserWithQuotaRecord{UserRecord: record}

		if quota, ok := quotaByUser[row.ID]; ok {
			hostLimit := quota.HostLimit
			hostUsed := quota.HostUsed
			friendLimit := quota.FriendLimit
			friendUsed := quota.FriendUsed
			packageLimit := quota.PackageLimit
			packageUsed := quota.PackageUsed
			monthlyTrafficUsed := strconv.FormatInt(quota.MonthlyTrafficUsed, 10)
			extraTrafficQuota := strconv.FormatInt(quota.ExtraTrafficQuota, 10)
			trafficStatus := quota.TrafficStatus

			item.HostLimit = &hostLimit
			item.HostUsed = &hostUsed
			item.FriendLimit = &friendLimit
			item.FriendUsed = &friendUsed
			item.PackageLimit = &packageLimit
			item.PackageUsed = &packageUsed
			item.MonthlyTrafficUsed = &monthlyTrafficUsed
			item.ExtraTrafficQuota = &extraTrafficQuota
			item.TrafficStatus = &trafficStatus
			if quota.MonthlyTrafficLimit != nil {
				limit := strconv.FormatInt(*quota.MonthlyTrafficLimit, 10)
				item.MonthlyTrafficLimit = &limit
			}
		}
		users = append(users, item)
	}
	return users, nil
}

// ==================== quota ====================

// GetUserQuota mirrors getUserQuota; a missing quota row is (nil, nil).
func GetUserQuota(ctx context.Context, userID int32) (*UserQuotaRecord, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []usersQuotaRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+usersQuotaSelectColumns+" FROM user_quotas WHERE user_id = ? LIMIT 1", userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	record := rows[0].toRecord()
	return &record, nil
}

// CreateUserQuota mirrors createUserQuota (unspecified limits default to 0).
func CreateUserQuota(ctx context.Context, userID int32, data UserQuotaUpdate) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	valueOrZero := func(value *int32) int32 {
		if value == nil {
			return 0
		}
		return *value
	}
	return DB.WithContext(ctx).Exec(
		`INSERT INTO user_quotas (user_id, host_limit, friend_limit, package_limit)
		 VALUES (?, ?, ?, ?)`,
		userID, valueOrZero(data.HostLimit), valueOrZero(data.FriendLimit), valueOrZero(data.PackageLimit),
	).Error
}

// UpdateUserQuotaLimits mirrors updateUserQuotaLimits, including the
// empty-update early return (`if (Object.keys(updateData).length === 0) return`).
func UpdateUserQuotaLimits(ctx context.Context, userID int32, data UserQuotaUpdate) error {
	columns, args := usersQuotaUpdateAssignments(data)
	if len(columns) == 0 {
		return nil
	}

	if err := usersRequireDB(); err != nil {
		return err
	}
	args = append(args, userID)
	return usersExecExpectRow(DB.WithContext(ctx).Exec(
		"UPDATE user_quotas SET "+strings.Join(columns, ", ")+" WHERE user_id = ?",
		args...,
	))
}

// usersQuotaUpdateAssignments builds the `column = ?` list in TypeScript field
// order (hostLimit, friendLimit, packageLimit).
func usersQuotaUpdateAssignments(data UserQuotaUpdate) ([]string, []any) {
	columns := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if data.HostLimit != nil {
		columns = append(columns, "host_limit = ?")
		args = append(args, *data.HostLimit)
	}
	if data.FriendLimit != nil {
		columns = append(columns, "friend_limit = ?")
		args = append(args, *data.FriendLimit)
	}
	if data.PackageLimit != nil {
		columns = append(columns, "package_limit = ?")
		args = append(args, *data.PackageLimit)
	}
	return columns, args
}

// UpdateUserQuotaUsed mirrors updateUserQuotaUsed (quota-operations.ts): direct
// overwrite of the used counters, with the same empty-update early return.
func UpdateUserQuotaUsed(ctx context.Context, userID int32, data UserQuotaUsedUpdate) error {
	columns := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if data.HostUsed != nil {
		columns = append(columns, "host_used = ?")
		args = append(args, *data.HostUsed)
	}
	if data.FriendUsed != nil {
		columns = append(columns, "friend_used = ?")
		args = append(args, *data.FriendUsed)
	}
	if data.PackageUsed != nil {
		columns = append(columns, "package_used = ?")
		args = append(args, *data.PackageUsed)
	}
	if len(columns) == 0 {
		return nil
	}

	if err := usersRequireDB(); err != nil {
		return err
	}
	args = append(args, userID)
	return usersExecExpectRow(DB.WithContext(ctx).Exec(
		"UPDATE user_quotas SET "+strings.Join(columns, ", ")+" WHERE user_id = ?",
		args...,
	))
}

// CalculateUserQuotaUsed mirrors calculateUserQuotaUsed: hosts, accepted
// friendships on either side, packages. Instances are intentionally not counted
// (the new quota system does not limit them).
func CalculateUserQuotaUsed(ctx context.Context, userID int32) (UserQuotaUsedUpdate, error) {
	if err := usersRequireDB(); err != nil {
		return UserQuotaUsedUpdate{}, err
	}

	var hostUsed int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM hosts WHERE user_id = ?", userID).Scan(&hostUsed).Error; err != nil {
		return UserQuotaUsedUpdate{}, err
	}
	var friendUsed int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM friendships
		  WHERE (user_id = ? AND status = 'accepted') OR (friend_id = ? AND status = 'accepted')`,
		userID, userID,
	).Scan(&friendUsed).Error; err != nil {
		return UserQuotaUsedUpdate{}, err
	}
	var packageUsed int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM packages WHERE user_id = ?", userID).Scan(&packageUsed).Error; err != nil {
		return UserQuotaUsedUpdate{}, err
	}

	hostValue := int32(hostUsed)
	friendValue := int32(friendUsed)
	packageValue := int32(packageUsed)
	return UserQuotaUsedUpdate{HostUsed: &hostValue, FriendUsed: &friendValue, PackageUsed: &packageValue}, nil
}

// ==================== update ====================

// UpdateUser mirrors updateUser, including the empty-update early return.
func UpdateUser(ctx context.Context, id int32, data UserUpdate) error {
	sets, args := usersUpdateAssignments(data)
	if len(sets) == 0 {
		return nil
	}

	if err := usersRequireDB(); err != nil {
		return err
	}
	args = append(args, id)
	// Prisma's @updatedAt bumps updated_at on every write; raw SQL must do it.
	sets = append(sets, "updated_at = NOW()")
	return usersExecExpectRow(DB.WithContext(ctx).Exec(
		"UPDATE users SET "+strings.Join(sets, ", ")+" WHERE id = ?",
		args...,
	))
}

// usersUpdateAssignments builds the SET list in TypeScript field order
// (email, role, status, passwordHash, avatarStyle).
func usersUpdateAssignments(data UserUpdate) ([]string, []any) {
	sets := make([]string, 0, 5)
	args := make([]any, 0, 5)
	if data.Email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *data.Email)
	}
	if data.Role != nil {
		sets = append(sets, "role = ?")
		args = append(args, string(*data.Role))
	}
	if data.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*data.Status))
	}
	if data.PasswordHash != nil {
		sets = append(sets, "password_hash = ?")
		args = append(args, *data.PasswordHash)
	}
	if data.AvatarStyle != nil {
		sets = append(sets, "avatar_style = ?")
		args = append(args, *data.AvatarStyle)
	}
	return sets, args
}

// UpdateUserStatus mirrors updateUserStatus: banning stores the reason, any
// other status clears it.
//
// Access-token invalidation is NOT done here: the TypeScript route
// (`PATCH /api/users/:id/status`) performs it right after this call, and moving
// it down a layer would change who is invalidated (e.g. a direct unban write
// would also revoke sessions). Keep it in routes/users.go.
func UpdateUserStatus(ctx context.Context, id int32, status models.UserStatus, banReason *string) error {
	if err := usersRequireDB(); err != nil {
		return err
	}

	var reason *string
	if status == models.UserStatusBanned {
		if banReason != nil && *banReason != "" {
			reason = banReason
		}
	}

	return usersExecExpectRow(DB.WithContext(ctx).Exec(
		`UPDATE users SET status = ?, ban_reason = ?, updated_at = NOW() WHERE id = ?`,
		string(status), reason, id,
	))
}

// DeleteUser mirrors deleteUser: a foreign-key violation (Prisma P2003/P2014)
// becomes ErrUserDeletionConflict so the route can answer 409.
func DeleteUser(ctx context.Context, id int32) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	err := usersExecExpectRow(DB.WithContext(ctx).Exec("DELETE FROM users WHERE id = ?", id))
	if err != nil {
		if usersIsForeignKeyViolation(err) || usersIsRequiredRelationViolation(err) {
			return ErrUserDeletionConflict
		}
		return err
	}
	return nil
}

// ==================== 2FA ====================
// The secret and the recovery codes are stored as the opaque (AES-GCM encrypted)
// strings produced by lib.EncryptSensitiveData at the route layer. This module
// must never encrypt or decrypt them.
//
// The four-step setup is deliberately four separate statements (and stays that
// way): routes/auth.ts writes step 1, 2, 3 in order and can resume/recover between
// them. `Enable2FAWithRecoveryCodes` is the single-statement alternative for flows
// that complete setup atomically, and `Disable2FAComplete` is the rollback.
const (
	// usersSQLSave2FASecret — step 1/4: persist the pending secret only.
	usersSQLSave2FASecret = "UPDATE users SET two_factor_secret = ?, updated_at = NOW() WHERE id = ?"
	// usersSQLEnable2FA — step 2/4: flip the flag, leave the secret in place.
	usersSQLEnable2FA = "UPDATE users SET two_factor_enabled = true, updated_at = NOW() WHERE id = ?"
	// usersSQLSave2FARecoveryCodes — step 3/4: persist the encrypted recovery codes.
	usersSQLSave2FARecoveryCodes = "UPDATE users SET two_factor_recovery_codes = ?, updated_at = NOW() WHERE id = ?"
	// usersSQLEnable2FAWithRecoveryCodes — atomic enable + recovery codes.
	usersSQLEnable2FAWithRecoveryCodes = `UPDATE users SET two_factor_enabled = true, two_factor_recovery_codes = ?, updated_at = NOW() WHERE id = ?`
	// usersSQLDisable2FA — flag off + secret cleared, recovery codes kept.
	usersSQLDisable2FA = "UPDATE users SET two_factor_enabled = false, two_factor_secret = NULL, updated_at = NOW() WHERE id = ?"
	// usersSQLDisable2FAComplete — rollback: clear flag, secret AND recovery codes.
	usersSQLDisable2FAComplete = `UPDATE users
		    SET two_factor_enabled = false, two_factor_secret = NULL, two_factor_recovery_codes = NULL,
		        updated_at = NOW()
		  WHERE id = ?`
)

// GetUser2FAStatus mirrors getUser2FAStatus. A missing user is
// {enabled:false, hasSecret:false}, not an error.
func GetUser2FAStatus(ctx context.Context, userID int32) (TwoFAStatus, error) {
	if err := usersRequireDB(); err != nil {
		return TwoFAStatus{}, err
	}
	var rows []struct {
		TwoFactorEnabled bool    `gorm:"column:two_factor_enabled"`
		TwoFactorSecret  *string `gorm:"column:two_factor_secret"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT two_factor_enabled, two_factor_secret FROM users WHERE id = ? LIMIT 1", userID,
	).Scan(&rows).Error; err != nil {
		return TwoFAStatus{}, err
	}
	if len(rows) == 0 {
		return TwoFAStatus{Enabled: false, HasSecret: false}, nil
	}
	return TwoFAStatus{Enabled: rows[0].TwoFactorEnabled, HasSecret: rows[0].TwoFactorSecret != nil}, nil
}

// Save2FASecret mirrors save2FASecret (step 1/4: temporary secret before the code
// is confirmed).
func Save2FASecret(ctx context.Context, userID int32, secret string) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return usersExecExpectRow(DB.WithContext(ctx).Exec(usersSQLSave2FASecret, secret, userID))
}

// Enable2FA mirrors enable2FA (step 2/4: flip the flag only — recovery codes are
// written by step 3).
func Enable2FA(ctx context.Context, userID int32) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return usersExecExpectRow(DB.WithContext(ctx).Exec(usersSQLEnable2FA, userID))
}

// Disable2FA mirrors disable2FA (flag off + secret cleared, recovery codes kept).
func Disable2FA(ctx context.Context, userID int32) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return usersExecExpectRow(DB.WithContext(ctx).Exec(usersSQLDisable2FA, userID))
}

// Get2FASecret mirrors get2FASecret. Callers decrypt the returned ciphertext.
func Get2FASecret(ctx context.Context, userID int32) (*string, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []struct {
		TwoFactorSecret *string `gorm:"column:two_factor_secret"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT two_factor_secret FROM users WHERE id = ? LIMIT 1", userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].TwoFactorSecret, nil
}

// Is2FAEnabled mirrors is2FAEnabled (missing user → false).
func Is2FAEnabled(ctx context.Context, userID int32) (bool, error) {
	if err := usersRequireDB(); err != nil {
		return false, err
	}
	var rows []struct {
		TwoFactorEnabled bool `gorm:"column:two_factor_enabled"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT two_factor_enabled FROM users WHERE id = ? LIMIT 1", userID,
	).Scan(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	return rows[0].TwoFactorEnabled, nil
}

// Save2FARecoveryCodes mirrors save2FARecoveryCodes (step 3/4).
func Save2FARecoveryCodes(ctx context.Context, userID int32, encryptedCodes string) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return usersExecExpectRow(DB.WithContext(ctx).Exec(usersSQLSave2FARecoveryCodes, encryptedCodes, userID))
}

// Get2FARecoveryCodes mirrors get2FARecoveryCodes. Callers decrypt the result.
func Get2FARecoveryCodes(ctx context.Context, userID int32) (*string, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []struct {
		TwoFactorRecoveryCodes *string `gorm:"column:two_factor_recovery_codes"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT two_factor_recovery_codes FROM users WHERE id = ? LIMIT 1", userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].TwoFactorRecoveryCodes, nil
}

// Enable2FAWithRecoveryCodes mirrors enable2FAWithRecoveryCodes (step 4/4: the
// atomic alternative used when setup completes in one shot).
func Enable2FAWithRecoveryCodes(ctx context.Context, userID int32, encryptedCodes string) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return usersExecExpectRow(DB.WithContext(ctx).Exec(usersSQLEnable2FAWithRecoveryCodes, encryptedCodes, userID))
}

// Disable2FAComplete mirrors disable2FAComplete: the rollback that clears the
// flag, the secret and the recovery codes together.
func Disable2FAComplete(ctx context.Context, userID int32) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return usersExecExpectRow(DB.WithContext(ctx).Exec(usersSQLDisable2FAComplete, userID))
}

// GetAllAdminUserIds mirrors getAllAdminUserIds: role='admin' AND status='active'.
func GetAllAdminUserIds(ctx context.Context) ([]int32, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	var rows []struct {
		ID int32 `gorm:"column:id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT id FROM users WHERE role = 'admin' AND status = 'active' ORDER BY id ASC",
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]int32, 0, len(rows))
	seen := make(map[int32]bool, len(rows))
	for _, row := range rows {
		if seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// ============================================================================================
// Narrow projections for dependencies of routes/users.ts that live in modules not ported yet.
//
// routes/users.ts calls into db/pagination.ts (getUsersPaginated),
// db/login-records.ts, db/quota-operations.ts, db/oauth.ts and db/instances.ts. Those modules
// belong to other waves; rather than let a Wave D agent invent them, the helpers below select
// ONLY the columns routes/users.ts reads. When the real modules land they can replace these
// functions without touching the ported part of this file.
// ============================================================================================

// UsersPaginatedOptions mirrors PaginationOptions for the users list.
type UsersPaginatedOptions struct {
	Page         int
	PageSize     int
	Search       string
	SearchFields []string
	ExactMatch   bool
}

// UsersPaginatedResult mirrors PaginatedResponse<User>.
type UsersPaginatedResult struct {
	Items      []UserRecord `json:"items"`
	Total      int64        `json:"total"`
	Page       int          `json:"page"`
	PageSize   int          `json:"pageSize"`
	TotalPages int          `json:"totalPages"`
}

// userSearchFields is USER_SEARCH_FIELDS in declaration order.
var userSearchFields = []string{"username", "id", "email"}

// normalizeUserSearchFieldsDB mirrors normalizeUserSearchFields.
func normalizeUserSearchFieldsDB(fields []string) []string {
	if len(fields) == 0 {
		return append([]string(nil), userSearchFields...)
	}
	valid := map[string]bool{}
	for _, field := range userSearchFields {
		valid[field] = true
	}
	normalized := make([]string, 0, len(fields))
	for _, field := range fields {
		if valid[field] {
			normalized = append(normalized, field)
		}
	}
	if len(normalized) == 0 {
		return append([]string(nil), userSearchFields...)
	}
	// Canonical order, like `USER_SEARCH_FIELDS.filter(...)`.
	ordered := make([]string, 0, len(normalized))
	for _, field := range userSearchFields {
		for _, candidate := range normalized {
			if candidate == field {
				ordered = append(ordered, field)
				break
			}
		}
	}
	return ordered
}

// escapeSQLLike mirrors escapeSqlLike: escape backslash, percent and underscore.
func escapeSQLLike(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, char := range value {
		switch char {
		case '\\', '%', '_':
			builder.WriteByte('\\')
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

// buildUserSearchWhereClause mirrors buildUserSearchWhereClause. It returns the
// SQL fragment plus its arguments (already placeholder-ordered).
func buildUserSearchWhereClause(search string, searchFields []string, exactMatch bool) (string, []any) {
	trimmed := strings.TrimSpace(search)
	if trimmed == "" {
		return "", nil
	}

	fields := normalizeUserSearchFieldsDB(searchFields)
	fuzzyPattern := "%" + escapeSQLLike(trimmed) + "%"
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)

	for _, field := range fields {
		switch field {
		case "username":
			if exactMatch {
				clauses = append(clauses, "LOWER(u.username) = LOWER(?)")
				args = append(args, trimmed)
			} else {
				clauses = append(clauses, `u.username ILIKE ? ESCAPE '\'`)
				args = append(args, fuzzyPattern)
			}
		case "id":
			if exactMatch {
				clauses = append(clauses, "CAST(u.id AS TEXT) = ?")
				args = append(args, trimmed)
			} else {
				clauses = append(clauses, `CAST(u.id AS TEXT) ILIKE ? ESCAPE '\'`)
				args = append(args, fuzzyPattern)
			}
		case "email":
			if exactMatch {
				clauses = append(clauses, "u.email IS NOT NULL AND LOWER(u.email) = LOWER(?)")
				args = append(args, trimmed)
			} else {
				clauses = append(clauses, `u.email IS NOT NULL AND u.email ILIKE ? ESCAPE '\'`)
				args = append(args, fuzzyPattern)
			}
		}
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// GetUsersPaginated mirrors getUsersPaginated from db/pagination.ts: newest
// first, ILIKE search over username/id/email, quota joined into the projection.
func GetUsersPaginated(ctx context.Context, options UsersPaginatedOptions) (UsersPaginatedResult, error) {
	if err := usersRequireDB(); err != nil {
		return UsersPaginatedResult{}, err
	}

	page, pageSize := clampPagination(options.Page, options.PageSize)
	skip := (page - 1) * pageSize

	result := UsersPaginatedResult{Page: page, PageSize: pageSize}
	whereClause, whereArgs := buildUserSearchWhereClause(options.Search, options.SearchFields, options.ExactMatch)

	rows := make([]userRow, 0, pageSize)
	if whereClause != "" {
		if err := DB.WithContext(ctx).Raw(
			`SELECT COUNT(*)::bigint AS count FROM users u WHERE `+whereClause, whereArgs...,
		).Scan(&result.Total).Error; err != nil {
			return UsersPaginatedResult{}, err
		}

		var idRows []struct {
			ID int32 `gorm:"column:id"`
		}
		args := append(append([]any{}, whereArgs...), pageSize, skip)
		if err := DB.WithContext(ctx).Raw(
			`SELECT u.id FROM users u WHERE `+whereClause+` ORDER BY u.created_at DESC LIMIT ? OFFSET ?`, args...,
		).Scan(&idRows).Error; err != nil {
			return UsersPaginatedResult{}, err
		}

		if len(idRows) > 0 {
			ids := make([]int32, 0, len(idRows))
			for _, idRow := range idRows {
				ids = append(ids, idRow.ID)
			}
			var unordered []userRow
			if err := DB.WithContext(ctx).Raw(
				"SELECT "+usersSelectColumns+" FROM users WHERE id IN ?", ids,
			).Scan(&unordered).Error; err != nil {
				return UsersPaginatedResult{}, err
			}
			byID := make(map[int32]userRow, len(unordered))
			for _, row := range unordered {
				byID[row.ID] = row
			}
			for _, id := range ids {
				if row, ok := byID[id]; ok {
					rows = append(rows, row)
				}
			}
		}
	} else {
		if err := DB.WithContext(ctx).Raw(`SELECT COUNT(*)::bigint AS count FROM users`).Scan(&result.Total).Error; err != nil {
			return UsersPaginatedResult{}, err
		}
		if err := DB.WithContext(ctx).Raw(
			"SELECT "+usersSelectColumns+" FROM users ORDER BY created_at DESC LIMIT ? OFFSET ?", pageSize, skip,
		).Scan(&rows).Error; err != nil {
			return UsersPaginatedResult{}, err
		}
	}

	users := make([]UserRecord, 0, len(rows))
	for _, row := range rows {
		record := row.toRecord()
		record.PasswordHash = ""
		users = append(users, record)
	}
	result.Items = users
	if pageSize > 0 {
		result.TotalPages = int(math.Ceil(float64(result.Total) / float64(pageSize)))
	}
	return result, nil
}

// GetUserQuotaMap loads the quota rows for a page of users in one query (the
// Prisma `include: { quota: true }` equivalent).
func GetUserQuotaMap(ctx context.Context, userIDs []int32) (map[int32]UserQuotaRecord, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	result := map[int32]UserQuotaRecord{}
	if len(userIDs) == 0 {
		return result, nil
	}
	var rows []usersQuotaRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+usersQuotaSelectColumns+" FROM user_quotas WHERE user_id IN ?", userIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.UserID] = row.toRecord()
	}
	return result, nil
}

// CountInstancesByUser mirrors `(await getInstancesByUserId(id)).length` without
// loading full instance rows (db/instances.ts belongs to the instances wave).
// getInstancesByUserId filters `status != 'deleted'`, so this count must too — the
// deletion-blocker count in GetUserDeletionBlockers deliberately does not.
func CountInstancesByUser(ctx context.Context, userID int32) (int64, error) {
	if err := usersRequireDB(); err != nil {
		return 0, err
	}
	var count int64
	err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM instances WHERE user_id = ? AND status <> 'deleted'", userID,
	).Scan(&count).Error
	return count, err
}

// CountInstancesByUserIDs is the batch form of CountInstancesByUser used by the
// admin list route (calling getInstancesByUserId per row would be N+1).
func CountInstancesByUserIDs(ctx context.Context, userIDs []int32) (map[int32]int64, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	counts := make(map[int32]int64, len(userIDs))
	if len(userIDs) == 0 {
		return counts, nil
	}
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
		Count  int64 `gorm:"column:count"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT user_id, COUNT(*) AS count
		   FROM instances
		  WHERE user_id IN ? AND status <> 'deleted'
		  GROUP BY user_id`, userIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		counts[row.UserID] = row.Count
	}
	return counts, nil
}

// LoginRecordItem mirrors db/login-records.ts LoginRecordItem.
type LoginRecordItem struct {
	ID        int32     `gorm:"column:id" json:"id"`
	UserID    int32     `gorm:"column:user_id" json:"userId"`
	IP        string    `gorm:"column:ip" json:"ip"`
	Country   *string   `gorm:"column:country" json:"country"`
	Region    *string   `gorm:"column:region" json:"region"`
	City      *string   `gorm:"column:city" json:"city"`
	Isp       *string   `gorm:"column:isp" json:"isp"`
	Timezone  *string   `gorm:"column:timezone" json:"timezone"`
	UserAgent *string   `gorm:"column:user_agent" json:"userAgent"`
	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
}

// LoginRecordsPage mirrors getUserLoginRecords' return shape.
type LoginRecordsPage struct {
	Records    []LoginRecordItem `json:"records"`
	Total      int64             `json:"total"`
	Page       int               `json:"page"`
	PageSize   int               `json:"pageSize"`
	TotalPages int               `json:"totalPages"`
}

// GetUserLoginRecords mirrors getUserLoginRecords (pageSize clamped to 1..50).
func GetUserLoginRecords(ctx context.Context, userID int32, page, pageSize int) (LoginRecordsPage, error) {
	if err := usersRequireDB(); err != nil {
		return LoginRecordsPage{}, err
	}

	safePage := 1
	if page > 0 {
		safePage = page
	}
	safePageSize := 20
	if pageSize != 0 {
		safePageSize = pageSize
		if safePageSize < 1 {
			safePageSize = 1
		}
		if safePageSize > 50 {
			safePageSize = 50
		}
	}
	skip := (safePage - 1) * safePageSize

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM login_records WHERE user_id = ?", userID,
	).Scan(&total).Error; err != nil {
		return LoginRecordsPage{}, err
	}

	records := make([]LoginRecordItem, 0, safePageSize)
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, user_id, ip, country, region, city, isp, timezone, user_agent, created_at
		   FROM login_records WHERE user_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		userID, safePageSize, skip,
	).Scan(&records).Error; err != nil {
		return LoginRecordsPage{}, err
	}

	totalPages := 0
	if safePageSize > 0 {
		totalPages = int(math.Ceil(float64(total) / float64(safePageSize)))
	}
	return LoginRecordsPage{Records: records, Total: total, Page: safePage, PageSize: safePageSize, TotalPages: totalPages}, nil
}

// GetLastLoginRecordsForUsers mirrors getLastLoginRecordsForUsers: the newest
// record per user (DISTINCT ON is the ordered equivalent of the TypeScript's
// "order by createdAt desc, keep the first seen").
func GetLastLoginRecordsForUsers(ctx context.Context, userIDs []int32) (map[int32]*LoginRecordItem, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	result := make(map[int32]*LoginRecordItem, len(userIDs))
	for _, userID := range userIDs {
		result[userID] = nil
	}
	if len(userIDs) == 0 {
		return result, nil
	}

	var records []LoginRecordItem
	if err := DB.WithContext(ctx).Raw(
		`SELECT DISTINCT ON (user_id) id, user_id, ip, country, region, city, isp, timezone, user_agent, created_at
		   FROM login_records
		  WHERE user_id IN ?
		  ORDER BY user_id, created_at DESC`, userIDs,
	).Scan(&records).Error; err != nil {
		return nil, err
	}
	for index := range records {
		record := records[index]
		result[record.UserID] = &record
	}
	return result, nil
}

// LinkedAccountUser mirrors the user projection attached to each linked-account
// group.
type LinkedAccountUser struct {
	ID         int32     `json:"id"`
	Username   string    `json:"username"`
	Email      *string   `json:"email"`
	Status     string    `json:"status"`
	LoginCount int64     `json:"loginCount,omitempty"`
	LastLogin  time.Time `json:"lastLogin,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitempty"`
}

// SharedIPGroup mirrors getSharedIPGroups' group shape.
type SharedIPGroup struct {
	IP          string              `json:"ip"`
	Users       []LinkedAccountUser `json:"users"`
	TotalLogins int64               `json:"totalLogins"`
}

// EmailPatternGroup mirrors getSimilarEmailGroups' group shape.
type EmailPatternGroup struct {
	Pattern string              `json:"pattern"`
	Users   []LinkedAccountUser `json:"users"`
}

// UsernamePatternGroup mirrors getSimilarUsernameGroups' group shape.
type UsernamePatternGroup struct {
	Pattern string              `json:"pattern"`
	Users   []LinkedAccountUser `json:"users"`
}

// usersClampPositiveInteger mirrors clampPositiveInteger.
func usersClampPositiveInteger(value, fallback, max int) int {
	if value > 0 {
		if value > max {
			return max
		}
		return value
	}
	return fallback
}

// GetSharedIPGroups mirrors getSharedIPGroups (login_records.ts): IPs used by at
// least minUsers distinct users in the window, users sorted by last login desc.
func GetSharedIPGroups(ctx context.Context, days, minUsers, groupLimit, maxUsersPerGroup int) ([]SharedIPGroup, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}

	safeGroupLimit := usersClampPositiveInteger(groupLimit, 50, 100)
	safeMaxUsersPerGroup := usersClampPositiveInteger(maxUsersPerGroup, 20, 20)
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	// Step 1: IPs with at least minUsers login rows in the window (Prisma's
	// groupBy counts rows, not distinct users; the distinct check is step 2).
	var ipRows []struct {
		IP string `gorm:"column:ip"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT ip FROM login_records
		  WHERE created_at >= ?
		  GROUP BY ip
		 HAVING COUNT(user_id) >= ?`,
		since, minUsers,
	).Scan(&ipRows).Error; err != nil {
		return nil, err
	}
	sharedIPs := make([]string, 0, len(ipRows))
	for _, ipRow := range ipRows {
		sharedIPs = append(sharedIPs, ipRow.IP)
	}
	if len(sharedIPs) == 0 {
		return []SharedIPGroup{}, nil
	}

	// Step 2: every record for those IPs, newest first, joined to its user.
	type sharedRow struct {
		IP        string    `gorm:"column:ip"`
		UserID    int32     `gorm:"column:user_id"`
		Username  string    `gorm:"column:username"`
		Email     *string   `gorm:"column:email"`
		Status    string    `gorm:"column:status"`
		CreatedAt time.Time `gorm:"column:created_at"`
	}
	var records []sharedRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT l.ip, l.user_id, u.username, u.email, u.status::text AS status, l.created_at
		   FROM login_records l
		   JOIN users u ON u.id = l.user_id
		  WHERE l.ip IN ? AND l.created_at >= ?
		  ORDER BY l.created_at DESC`, sharedIPs, since,
	).Scan(&records).Error; err != nil {
		return nil, err
	}

	type groupState struct {
		users       []LinkedAccountUser
		index       map[int32]int
		totalLogins int64
	}
	grouped := map[string]*groupState{}
	order := make([]string, 0, len(sharedIPs))
	for _, record := range records {
		state, ok := grouped[record.IP]
		if !ok {
			state = &groupState{index: map[int32]int{}}
			grouped[record.IP] = state
			order = append(order, record.IP)
		}
		position, seen := state.index[record.UserID]
		if !seen {
			state.index[record.UserID] = len(state.users)
			state.users = append(state.users, LinkedAccountUser{
				ID:       record.UserID,
				Username: record.Username,
				Email:    record.Email,
				Status:   record.Status,
				// First sighting is the newest record because of the ORDER BY.
				LastLogin: record.CreatedAt,
			})
			position = state.index[record.UserID]
		}
		state.users[position].LoginCount++
		state.totalLogins++
	}

	groups := make([]SharedIPGroup, 0, len(order))
	for _, ip := range order {
		state := grouped[ip]
		if len(state.users) < minUsers {
			continue
		}
		users := state.users
		if len(users) > safeMaxUsersPerGroup {
			users = users[:safeMaxUsersPerGroup]
		}
		groups = append(groups, SharedIPGroup{IP: ip, Users: users, TotalLogins: state.totalLogins})
	}

	// Sort by number of linked users, descending (stable, like Array#sort).
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i].Users) > len(groups[j].Users) })
	if len(groups) > safeGroupLimit {
		groups = groups[:safeGroupLimit]
	}
	return groups, nil
}

// usersSimilarPrefix mirrors `value.replace(/[\d_-]+$/, ”).toLowerCase()`.
func usersSimilarPrefix(value string) string {
	end := len(value)
	for end > 0 {
		char := value[end-1]
		if (char >= '0' && char <= '9') || char == '_' || char == '-' {
			end--
			continue
		}
		break
	}
	return strings.ToLower(value[:end])
}

type similarUserRow struct {
	ID        int32     `gorm:"column:id"`
	Username  string    `gorm:"column:username"`
	Email     *string   `gorm:"column:email"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// GetSimilarEmailGroups mirrors getSimilarEmailGroups: same local part prefix
// (trailing digits/underscores/hyphens stripped) and same domain.
func GetSimilarEmailGroups(ctx context.Context, groupLimit, maxUsersPerGroup int) ([]EmailPatternGroup, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	safeGroupLimit := usersClampPositiveInteger(groupLimit, 50, 100)
	safeMaxUsersPerGroup := usersClampPositiveInteger(maxUsersPerGroup, 20, 20)

	var rows []similarUserRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, username, email, status::text AS status, created_at FROM users WHERE email IS NOT NULL`,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	type groupState struct {
		users []LinkedAccountUser
	}
	grouped := map[string]*groupState{}
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Email == nil {
			continue
		}
		// `const [localPart, domain] = email.split('@')` keeps only the first two
		// segments, so a malformed "a@b@c" yields domain "b".
		segments := strings.Split(*row.Email, "@")
		if len(segments) < 2 {
			continue
		}
		localPart, domain := segments[0], segments[1]
		if localPart == "" || domain == "" {
			continue
		}
		prefix := usersSimilarPrefix(localPart)
		if jsStringLengthDB(prefix) < 2 {
			continue
		}
		key := prefix + "*@" + domain
		state, ok := grouped[key]
		if !ok {
			state = &groupState{}
			grouped[key] = state
			order = append(order, key)
		}
		state.users = append(state.users, LinkedAccountUser{
			ID: row.ID, Username: row.Username, Email: row.Email, Status: row.Status, CreatedAt: row.CreatedAt,
		})
	}

	groups := make([]EmailPatternGroup, 0, len(order))
	for _, pattern := range order {
		users := grouped[pattern].users
		if len(users) < 2 {
			continue
		}
		sort.SliceStable(users, func(i, j int) bool { return users[i].CreatedAt.After(users[j].CreatedAt) })
		if len(users) > safeMaxUsersPerGroup {
			users = users[:safeMaxUsersPerGroup]
		}
		groups = append(groups, EmailPatternGroup{Pattern: pattern, Users: users})
	}
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i].Users) > len(groups[j].Users) })
	if len(groups) > safeGroupLimit {
		groups = groups[:safeGroupLimit]
	}
	return groups, nil
}

// GetSimilarUsernameGroups mirrors getSimilarUsernameGroups.
func GetSimilarUsernameGroups(ctx context.Context, groupLimit, maxUsersPerGroup int) ([]UsernamePatternGroup, error) {
	if err := usersRequireDB(); err != nil {
		return nil, err
	}
	safeGroupLimit := usersClampPositiveInteger(groupLimit, 50, 100)
	safeMaxUsersPerGroup := usersClampPositiveInteger(maxUsersPerGroup, 20, 20)

	var rows []similarUserRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, username, email, status::text AS status, created_at FROM users`,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	grouped := map[string][]LinkedAccountUser{}
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		prefix := usersSimilarPrefix(row.Username)
		if jsStringLengthDB(prefix) < 2 {
			continue
		}
		key := prefix + "*"
		if _, ok := grouped[key]; !ok {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], LinkedAccountUser{
			ID: row.ID, Username: row.Username, Email: row.Email, Status: row.Status, CreatedAt: row.CreatedAt,
		})
	}

	groups := make([]UsernamePatternGroup, 0, len(order))
	for _, pattern := range order {
		users := grouped[pattern]
		if len(users) < 2 {
			continue
		}
		sort.SliceStable(users, func(i, j int) bool { return users[i].CreatedAt.After(users[j].CreatedAt) })
		if len(users) > safeMaxUsersPerGroup {
			users = users[:safeMaxUsersPerGroup]
		}
		groups = append(groups, UsernamePatternGroup{Pattern: pattern, Users: users})
	}
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i].Users) > len(groups[j].Users) })
	if len(groups) > safeGroupLimit {
		groups = groups[:safeGroupLimit]
	}
	return groups, nil
}

// HasOAuthBinding mirrors hasOAuthBinding from db/oauth.ts.
func HasOAuthBinding(ctx context.Context, userID int32, provider string) (bool, error) {
	if err := usersRequireDB(); err != nil {
		return false, err
	}
	var exists bool
	err := DB.WithContext(ctx).Raw(
		"SELECT EXISTS (SELECT 1 FROM user_oauth_bindings WHERE user_id = ? AND provider = ?::text)",
		userID, provider,
	).Scan(&exists).Error
	return exists, err
}

// DeleteOAuthBinding mirrors deleteOAuthBinding from db/oauth.ts.
func DeleteOAuthBinding(ctx context.Context, userID int32, provider string) error {
	if err := usersRequireDB(); err != nil {
		return err
	}
	return DB.WithContext(ctx).Exec(
		"DELETE FROM user_oauth_bindings WHERE user_id = ? AND provider = ?::text", userID, provider,
	).Error
}

// UserDeletionBlockers mirrors the route-local UserDeletionBlockers type.
type UserDeletionBlockers struct {
	Instances        int64
	Hosts            int64
	Packages         int64
	BalanceNonZero   bool
	BalanceLogs      int64
	RechargeRecords  int64
	LoginRecords     int64
	RedeemCodeUsages int64
}

// HasBlockers mirrors hasUserDeletionBlockers.
func (b UserDeletionBlockers) HasBlockers() bool {
	return b.Instances > 0 || b.Hosts > 0 || b.Packages > 0 || b.BalanceNonZero ||
		b.BalanceLogs > 0 || b.RechargeRecords > 0 || b.LoginRecords > 0 || b.RedeemCodeUsages > 0
}

// GetUserDeletionBlockers mirrors the route-local getUserDeletionBlockers.
func GetUserDeletionBlockers(ctx context.Context, userID int32) (UserDeletionBlockers, error) {
	if err := usersRequireDB(); err != nil {
		return UserDeletionBlockers{}, err
	}

	var blockers UserDeletionBlockers
	var balance *models.Decimal
	var rows []struct {
		Balance models.Decimal `gorm:"column:balance"`
	}
	if err := DB.WithContext(ctx).Raw("SELECT balance FROM users WHERE id = ? LIMIT 1", userID).Scan(&rows).Error; err != nil {
		return blockers, err
	}
	if len(rows) > 0 {
		value := rows[0].Balance
		balance = &value
	}
	blockers.BalanceNonZero = balance != nil && !balance.IsZero()

	counts := []struct {
		table       string
		destination *int64
	}{
		{"instances", &blockers.Instances},
		{"hosts", &blockers.Hosts},
		{"packages", &blockers.Packages},
		{"balance_logs", &blockers.BalanceLogs},
		{"recharge_records", &blockers.RechargeRecords},
		{"login_records", &blockers.LoginRecords},
		{"redeem_code_usages", &blockers.RedeemCodeUsages},
	}
	for _, count := range counts {
		if err := DB.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM "+count.table+" WHERE user_id = ?", userID,
		).Scan(count.destination).Error; err != nil {
			return UserDeletionBlockers{}, err
		}
	}
	return blockers, nil
}
