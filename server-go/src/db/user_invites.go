// Port of server/src/db/invite-codes.ts plus the invite-cost engine from
// server/src/lib/invite-pricing.ts.
//
// The TypeScript route (routes/user-invites.ts) imports the pricing module from
// lib/, but the Go db package may not import lib (lib imports db), so the pricing
// code lives here. Every exported symbol is prefixed `UserInvites` / the internal
// ones `userInvites`, because db is a flat package shared by parallel module
// ports.
//
// Behavioural notes:
//
//   - Invite codes are generated with nanoid(12) over nanoid's URL alphabet
//     (`useandom-26T198340PX75JACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict`). The
//     alphabet is exactly 64 characters, so nanoid's mask is 63 and each random
//     byte maps to one character (`byte & 63`). userInvitesRandomCode reproduces
//     that byte-for-byte shape; a unique-key collision is *not* retried, matching
//     the Node route (which would surface a Prisma P2002 as a 500).
//   - Charging runs inside the caller's transaction with the same blocking
//     transaction-scoped advisory locks the TypeScript uses
//     (USER_BALANCE_LOCK_NAMESPACE / USER_POINTS_LOCK_NAMESPACE), so the guarded
//     UPDATE and its log row commit or roll back together.
//   - Cost amounts are JavaScript numbers on the wire, so they are float64 here,
//     never models.Decimal. Money columns stay decimal.Decimal.
package db

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	// userInvitesDefaultPageSize mirrors the `20` fallback of
	// clampInvitePagination.
	userInvitesDefaultPageSize = 20
	// userInvitesMaxPageSize mirrors the `100` cap of clampInvitePagination.
	userInvitesMaxPageSize = 100

	// userInvitesCostConfigKey mirrors the getSystemConfig('invite_generation_costs')
	// key.
	userInvitesCostConfigKey = "invite_generation_costs"
	// userInvitesExpiryDaysKey mirrors the getSystemConfigNumber key.
	userInvitesExpiryDaysKey = "invite_default_expire_days"

	// userInvitesCostResourceMaxLength mirrors INVITE_COST_RESOURCE_MAX_LENGTH.
	userInvitesCostResourceMaxLength = 64

	// userInvitesCodeLength mirrors nanoid(12).
	userInvitesCodeLength = 12
)

// userInvitesURLAlphabet mirrors nanoid's urlAlphabet verbatim (64 unique
// characters, so the mask is 63 and no rejection sampling is needed).
const userInvitesURLAlphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"

// ==================== sentinels ====================
//
// The Node route maps these exact error.message strings onto HTTP responses, so
// the Go sentinels carry the same text and the route matches with errors.Is.

var (
	// ErrUserInvitesCostOptionUnavailable mirrors 'INVITE_COST_OPTION_UNAVAILABLE'.
	ErrUserInvitesCostOptionUnavailable = errors.New("INVITE_COST_OPTION_UNAVAILABLE")
	// ErrUserInvitesCostResourceUnsupported mirrors 'INVITE_COST_RESOURCE_UNSUPPORTED'.
	ErrUserInvitesCostResourceUnsupported = errors.New("INVITE_COST_RESOURCE_UNSUPPORTED")
	// ErrUserInvitesInsufficientBalance mirrors 'INSUFFICIENT_BALANCE'.
	ErrUserInvitesInsufficientBalance = errors.New("INSUFFICIENT_BALANCE")
	// ErrUserInvitesInsufficientPoints mirrors 'INSUFFICIENT_POINTS'.
	ErrUserInvitesInsufficientPoints = errors.New("INSUFFICIENT_POINTS")
	// ErrUserInvitesUserNotFound mirrors 'USER_NOT_FOUND'.
	ErrUserInvitesUserNotFound = errors.New("USER_NOT_FOUND")
	// ErrUserInvitesSkipOutOfRange reports a pagination offset that does not fit in
	// int64 — Prisma turns this into a client error (→ 500), never a 400.
	ErrUserInvitesSkipOutOfRange = errors.New("Unable to fit pagination offset into a 64-bit signed integer for field `skip`")
)

// ==================== types ====================

// UserInvitesCostOption mirrors InviteCostOption. Amount is a JS number.
type UserInvitesCostOption struct {
	Resource string  `json:"resource"`
	Amount   float64 `json:"amount"`
	Enabled  bool    `json:"enabled"`
}

// UserInvitesCostQuote mirrors InviteCostQuote.
type UserInvitesCostQuote struct {
	Resource      string  `json:"resource"`
	Amount        float64 `json:"amount"`
	Enabled       bool    `json:"enabled"`
	Label         string  `json:"label"`
	Unit          string  `json:"unit"`
	DisplayAmount string  `json:"displayAmount"`
}

// userInvitesCostSnapshot mirrors InviteGenerationCostSnapshot. Field order is
// the object-literal order used by the TypeScript, so the stored jsonb text is
// byte-shape compatible.
type userInvitesCostSnapshot struct {
	Resource      string  `json:"resource"`
	Amount        float64 `json:"amount"`
	Label         string  `json:"label"`
	Unit          string  `json:"unit"`
	DisplayAmount string  `json:"displayAmount"`
	ChargedAt     string  `json:"chargedAt"`
}

// UserInvitesBalance mirrors getUserInviteBalances' return value.
type UserInvitesBalance struct {
	Balance float64 `json:"balance"`
	Points  int32   `json:"points"`
}

// UserInvitesStats mirrors getUserInviteCodeStats' return value.
type UserInvitesStats struct {
	Total     int64   `json:"total"`
	Used      int64   `json:"used"`
	Unused    int64   `json:"unused"`
	UsageRate float64 `json:"usageRate"`
}

// UserInvitesGenerateAbility mirrors canUserGenerateInviteCode's return value.
type UserInvitesGenerateAbility struct {
	Allowed bool
	Reason  string
}

// UserInvitesCodeRow is one invite_codes row joined with the *used-by* user (the
// `include: { user: ... }` projection). The user columns are nil when the code is
// unused or was created by a deleted account.
type UserInvitesCodeRow struct {
	ID           int32
	Code         string
	CreatedBy    int32
	UsedBy       *int32
	UsedAt       *time.Time
	ExpiresAt    *time.Time
	CostSnapshot []byte
	CreatedAt    time.Time

	UserID        *int32
	Username      *string
	Email         *string
	AvatarStyle   *string
	AvatarBadgeID *string
	UserCreatedAt *time.Time
}

// UserInvitesListResult is the { invites, total } envelope.
type UserInvitesListResult struct {
	Items []UserInvitesCodeRow
	Total int64
}

// ==================== helpers ====================

// userInvitesClampPagination mirrors clampInvitePagination.
func userInvitesClampPagination(page, pageSize int64) (int64, int64) {
	clampedPage := int64(1)
	if page > 0 {
		clampedPage = page
	}
	clampedPageSize := int64(userInvitesDefaultPageSize)
	if pageSize != 0 {
		clampedPageSize = pageSize
		if clampedPageSize < 1 {
			clampedPageSize = 1
		}
		if clampedPageSize > userInvitesMaxPageSize {
			clampedPageSize = userInvitesMaxPageSize
		}
	}
	return clampedPage, clampedPageSize
}

// userInvitesOffset mirrors `(page - 1) * pageSize`. A page so large that the
// product overflows makes the Node query fail ("Unable to fit value … into a
// 64-bit signed integer for field `skip`"), so it reports ok=false.
func userInvitesOffset(page, pageSize int64) (int64, bool) {
	if page-1 > math.MaxInt64/pageSize {
		return 0, false
	}
	return (page - 1) * pageSize, true
}

// userInvitesFormatNumber mirrors String(number) for finite values: integers use
// plain decimal notation below 1e21, everything else uses Go's shortest
// round-trippable representation (identical to JS Number#toString for these).
func userInvitesFormatNumber(value float64) string {
	if value == 0 {
		// Also normalizes -0 to "0", matching String(-0).
		return "0"
	}
	if math.Trunc(value) == value && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// userInvitesFormatFixed2 mirrors Number#toFixed(2).
func userInvitesFormatFixed2(value float64) string {
	return fmt.Sprintf("%.2f", value)
}

// userInvitesRandomCode mirrors nanoid(12) for the URL alphabet. The alphabet has
// 64 entries, so nanoid's mask is 63 and every byte selects one character.
func userInvitesRandomCode(size int) (string, error) {
	if size <= 0 {
		return "", nil
	}
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.Grow(size)
	for _, value := range buffer {
		builder.WriteByte(userInvitesURLAlphabet[int(value)&63])
	}
	return builder.String(), nil
}

// userInvitesBeginTx starts the pgx transaction the advisory locks require.
func userInvitesBeginTx(ctx context.Context) (pgx.Tx, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return pool.Begin(ctx)
}

// userInvitesParseCostConfig mirrors parseInviteCostConfig.
func userInvitesParseCostConfig(raw *string) []UserInvitesCostOption {
	if raw == nil || *raw == "" {
		return userInvitesDefaultCostOptions()
	}

	var parsed any
	if err := json.Unmarshal([]byte(*raw), &parsed); err != nil {
		return userInvitesDefaultCostOptions()
	}
	items, ok := parsed.([]any)
	if !ok {
		return userInvitesDefaultCostOptions()
	}

	options := make([]UserInvitesCostOption, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			// `if (!item || typeof item !== 'object') return null`. Arrays are
			// objects in JavaScript but have no `resource`, so they are dropped by
			// the empty-resource check either way.
			continue
		}
		resource := strings.TrimSpace(userInvitesToStringJS(record["resource"]))
		if resource == "" {
			continue
		}
		amount, finite := userInvitesToNumberJS(record["amount"])
		if !finite {
			amount = 0
		}
		enabled, _ := record["enabled"].(bool)
		options = append(options, UserInvitesCostOption{
			Resource: resource,
			Amount:   userInvitesNormalizeCostAmount(resource, amount),
			Enabled:  enabled,
		})
	}
	return options
}

// userInvitesDefaultCostOptions mirrors defaultInviteCostOptions.
func userInvitesDefaultCostOptions() []UserInvitesCostOption {
	return []UserInvitesCostOption{
		{Resource: "balance", Amount: 0, Enabled: false},
		{Resource: "points", Amount: 0, Enabled: false},
	}
}

// userInvitesEnsureDefaultCostOptions mirrors ensureDefaultCostOptions: the
// defaults are appended (in their own order) when the configured list omits them,
// and a duplicate resource keeps its first position but the last value, exactly
// like a JavaScript Map built with `map.set`.
func userInvitesEnsureDefaultCostOptions(options []UserInvitesCostOption) []UserInvitesCostOption {
	order := make([]string, 0, len(options)+2)
	values := make(map[string]UserInvitesCostOption, len(options)+2)
	for _, option := range options {
		if _, seen := values[option.Resource]; !seen {
			order = append(order, option.Resource)
		}
		values[option.Resource] = option
	}
	for _, option := range userInvitesDefaultCostOptions() {
		if _, seen := values[option.Resource]; !seen {
			order = append(order, option.Resource)
			values[option.Resource] = option
		}
	}

	result := make([]UserInvitesCostOption, 0, len(order))
	for _, resource := range order {
		result = append(result, values[resource])
	}
	return result
}

// userInvitesNormalizeCostOptions mirrors normalizeInviteCostOptions.
func userInvitesNormalizeCostOptions(options []UserInvitesCostOption) []UserInvitesCostOption {
	ensured := userInvitesEnsureDefaultCostOptions(options)
	result := make([]UserInvitesCostOption, 0, len(ensured))
	for _, option := range ensured {
		result = append(result, UserInvitesCostOption{
			Resource: option.Resource,
			Amount:   userInvitesNormalizeCostAmount(option.Resource, option.Amount),
			Enabled:  option.Enabled,
		})
	}
	return result
}

// userInvitesNormalizeCostAmount mirrors the per-resource normalizeAmount hooks.
func userInvitesNormalizeCostAmount(resource string, amount float64) float64 {
	switch resource {
	case "balance":
		return math.Round(math.Max(0, amount)*100) / 100
	case "points":
		return math.Floor(math.Max(0, amount))
	}
	return math.Max(0, amount)
}

// userInvitesCostLabel / Unit / Format mirror the handler's label, unit and
// format functions. ok=false means "no handler for this resource".
func userInvitesCostLabel(resource string) (label, unit string, ok bool) {
	switch resource {
	case "balance":
		return "余额", "元", true
	case "points":
		return "积分", "积分", true
	}
	return "", "", false
}

func userInvitesCostFormat(resource string, amount float64) string {
	switch resource {
	case "balance":
		return "¥" + userInvitesFormatFixed2(amount)
	case "points":
		return userInvitesFormatNumber(math.Floor(amount)) + " 积分"
	}
	return ""
}

// userInvitesToStringJS mirrors String(value) for the scalar JSON types.
func userInvitesToStringJS(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return userInvitesFormatNumber(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	}
	// Objects and arrays stringify to "[object Object]" / "a,b" in JavaScript,
	// which never yields a usable resource name.
	return ""
}

// userInvitesToNumberJS mirrors Number(value); ok=false is NaN.
func userInvitesToNumberJS(value any) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, true
	case float64:
		return typed, true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case string:
		return userInvitesParseNumberString(typed)
	}
	return 0, false
}

// userInvitesParseNumberString mirrors Number('…') for strings: empty/whitespace
// is 0, otherwise the whole trimmed string must be a decimal literal.
func userInvitesParseNumberString(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, true
	}
	parsed, ok := parseFloatJS(trimmed)
	if !ok {
		return 0, false
	}
	// parseFloat accepts a numeric prefix ("12abc" → 12) while Number rejects it.
	if trimmed != userInvitesNumberPrefix(trimmed) {
		return 0, false
	}
	return parsed, true
}

// userInvitesNumberPrefix returns the longest numeric prefix of a string, used to
// detect trailing garbage that Number() rejects.
func userInvitesNumberPrefix(value string) string {
	end := 0
	if end < len(value) && (value[end] == '+' || value[end] == '-') {
		end++
	}
	digits := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
		digits++
	}
	if end < len(value) && value[end] == '.' {
		end++
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
			digits++
		}
	}
	if digits == 0 {
		return ""
	}
	if end < len(value) && (value[end] == 'e' || value[end] == 'E') {
		exponentEnd := end + 1
		if exponentEnd < len(value) && (value[exponentEnd] == '+' || value[exponentEnd] == '-') {
			exponentEnd++
		}
		exponentDigits := 0
		for exponentEnd < len(value) && value[exponentEnd] >= '0' && value[exponentEnd] <= '9' {
			exponentEnd++
			exponentDigits++
		}
		if exponentDigits > 0 {
			end = exponentEnd
		}
	}
	return value[:end]
}

// decodeUserInvitesCostSnapshot mirrors `ic.costSnapshot ?? null`.
func decodeUserInvitesCostSnapshot(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}

// userInvitesCodeRowFrom scans one joined invite_codes row.
func userInvitesCodeRowFrom(
	id int32, code string, createdBy int32, usedBy *int32,
	usedAt, expiresAt *time.Time, costSnapshot []byte, createdAt time.Time,
	userID *int32, username, email, avatarStyle, avatarBadgeID *string,
	userCreatedAt *time.Time,
) UserInvitesCodeRow {
	return UserInvitesCodeRow{
		ID:            id,
		Code:          code,
		CreatedBy:     createdBy,
		UsedBy:        usedBy,
		UsedAt:        usedAt,
		ExpiresAt:     expiresAt,
		CostSnapshot:  costSnapshot,
		CreatedAt:     createdAt,
		UserID:        userID,
		Username:      username,
		Email:         email,
		AvatarStyle:   avatarStyle,
		AvatarBadgeID: avatarBadgeID,
		UserCreatedAt: userCreatedAt,
	}
}

// ==================== read ====================

// userInvitesSelectColumns is the shared projection for the joined queries.
const userInvitesSelectColumns = `c.id, c.code, c.created_by, c.used_by, c.used_at,
	c.expires_at, c.cost_snapshot, c.created_at,
	u.id, u.username, u.email, u.avatar_style, u.avatar_badge_id, u.created_at`

// scanUserInvitesCodeRow scans the columns selected by userInvitesSelectColumns.
func scanUserInvitesCodeRow(scan func(...any) error) (UserInvitesCodeRow, error) {
	var row UserInvitesCodeRow
	err := scan(
		&row.ID, &row.Code, &row.CreatedBy, &row.UsedBy, &row.UsedAt,
		&row.ExpiresAt, &row.CostSnapshot, &row.CreatedAt,
		&row.UserID, &row.Username, &row.Email, &row.AvatarStyle, &row.AvatarBadgeID,
		&row.UserCreatedAt,
	)
	return row, err
}

// UserInvitesFindByCode mirrors findInviteCode: the first *unused* code, or nil.
func UserInvitesFindByCode(ctx context.Context, code string) (*UserInvitesCodeRow, error) {
	row, err := scanUserInvitesCodeRow(func(dest ...any) error {
		return DB.WithContext(ctx).Raw(
			"SELECT "+userInvitesSelectColumns+" FROM invite_codes c"+
				" LEFT JOIN users u ON u.id = c.used_by"+
				" WHERE c.code = ? AND c.used_by IS NULL LIMIT 1", code,
		).Row().Scan(dest...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// UserInvitesGetByID mirrors getInviteCodeById.
func UserInvitesGetByID(ctx context.Context, id int32) (*UserInvitesCodeRow, error) {
	row, err := scanUserInvitesCodeRow(func(dest ...any) error {
		return DB.WithContext(ctx).Raw(
			"SELECT "+userInvitesSelectColumns+" FROM invite_codes c"+
				" LEFT JOIN users u ON u.id = c.used_by"+
				" WHERE c.id = ? LIMIT 1", id,
		).Row().Scan(dest...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// UserInvitesGetAll mirrors getInviteCodes: newest first, optionally filtered by
// creator (createdBy nil means "every code").
func UserInvitesGetAll(ctx context.Context, createdBy *int32) ([]UserInvitesCodeRow, error) {
	query := "SELECT " + userInvitesSelectColumns + " FROM invite_codes c" +
		" LEFT JOIN users u ON u.id = c.used_by"
	args := make([]any, 0, 1)
	if createdBy != nil {
		query += " WHERE c.created_by = ?"
		args = append(args, *createdBy)
	}
	query += " ORDER BY c.id DESC"

	rows, err := DB.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]UserInvitesCodeRow, 0, 8)
	for rows.Next() {
		row, err := scanUserInvitesCodeRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// UserInvitesListByCreator mirrors the paginated findMany used by
// /api/user-invites: newest id first with the used-by user projection.
func UserInvitesListByCreator(ctx context.Context, userID int32, page, pageSize int64) (UserInvitesListResult, error) {
	clampedPage, clampedPageSize := userInvitesClampPagination(page, pageSize)
	offset, ok := userInvitesOffset(clampedPage, clampedPageSize)
	if !ok {
		return UserInvitesListResult{}, ErrUserInvitesSkipOutOfRange
	}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM invite_codes WHERE created_by = ?", userID,
	).Scan(&total).Error; err != nil {
		return UserInvitesListResult{}, err
	}

	rows, err := DB.WithContext(ctx).Raw(
		"SELECT "+userInvitesSelectColumns+" FROM invite_codes c"+
			" LEFT JOIN users u ON u.id = c.used_by"+
			" WHERE c.created_by = ? ORDER BY c.id DESC LIMIT ? OFFSET ?",
		userID, clampedPageSize, offset,
	).Rows()
	if err != nil {
		return UserInvitesListResult{}, err
	}
	defer rows.Close()

	items := make([]UserInvitesCodeRow, 0, clampedPageSize)
	for rows.Next() {
		row, err := scanUserInvitesCodeRow(rows.Scan)
		if err != nil {
			return UserInvitesListResult{}, err
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return UserInvitesListResult{}, err
	}

	return UserInvitesListResult{Items: items, Total: total}, nil
}

// UserInvitesListWithUsers mirrors getInviteCodesWithUsers (admin listing). The
// creator relation is required, so it is joined as well.
func UserInvitesListWithUsers(ctx context.Context, page, pageSize int64, status string) (UserInvitesListResult, error) {
	clampedPage, clampedPageSize := userInvitesClampPagination(page, pageSize)
	offset, ok := userInvitesOffset(clampedPage, clampedPageSize)
	if !ok {
		return UserInvitesListResult{}, ErrUserInvitesSkipOutOfRange
	}

	where := ""
	switch status {
	case "used":
		where = " WHERE c.used_by IS NOT NULL"
	case "unused":
		where = " WHERE c.used_by IS NULL"
	}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM invite_codes c" + where,
	).Scan(&total).Error; err != nil {
		return UserInvitesListResult{}, err
	}

	rows, err := DB.WithContext(ctx).Raw(
		"SELECT "+userInvitesSelectColumns+" FROM invite_codes c"+
			" LEFT JOIN users u ON u.id = c.used_by"+where+
			" ORDER BY c.id DESC LIMIT ? OFFSET ?",
		clampedPageSize, offset,
	).Rows()
	if err != nil {
		return UserInvitesListResult{}, err
	}
	defer rows.Close()

	items := make([]UserInvitesCodeRow, 0, clampedPageSize)
	for rows.Next() {
		row, err := scanUserInvitesCodeRow(rows.Scan)
		if err != nil {
			return UserInvitesListResult{}, err
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return UserInvitesListResult{}, err
	}
	return UserInvitesListResult{Items: items, Total: total}, nil
}

// UserInvitesCounts mirrors the two `inviteCode.count` calls of the summary
// route: total created by the user and how many of those were used.
func UserInvitesCounts(ctx context.Context, userID int32) (total, used int64, err error) {
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM invite_codes WHERE created_by = ?", userID,
	).Scan(&total).Error; err != nil {
		return 0, 0, err
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM invite_codes WHERE created_by = ? AND used_by IS NOT NULL", userID,
	).Scan(&used).Error; err != nil {
		return 0, 0, err
	}
	return total, used, nil
}

// UserInvitesStatsForUser mirrors getUserInviteCodeStats (usageRate in percent).
func UserInvitesStatsForUser(ctx context.Context, userId int32) (UserInvitesStats, error) {
	total, used, err := UserInvitesCounts(ctx, userId)
	if err != nil {
		return UserInvitesStats{}, err
	}
	usageRate := 0.0
	if total > 0 {
		usageRate = float64(used) / float64(total) * 100
	}
	return UserInvitesStats{
		Total:     total,
		Used:      used,
		Unused:    total - used,
		UsageRate: usageRate,
	}, nil
}

// UserInvitesCanGenerate mirrors canUserGenerateInviteCode: more than ten codes
// requires an 80% usage rate, and usageRate is rendered with toFixed(1).
func UserInvitesCanGenerate(ctx context.Context, userID int32) (UserInvitesGenerateAbility, error) {
	stats, err := UserInvitesStatsForUser(ctx, userID)
	if err != nil {
		return UserInvitesGenerateAbility{}, err
	}
	if stats.Total <= 10 {
		return UserInvitesGenerateAbility{Allowed: true}, nil
	}
	if stats.UsageRate >= 80 {
		return UserInvitesGenerateAbility{Allowed: true}, nil
	}
	return UserInvitesGenerateAbility{
		Allowed: false,
		Reason: fmt.Sprintf(
			"邀请码使用率小于80%%，无法创建更多邀请码。您已有 %d 个邀请码，使用率为 %s%%，需要达到 80%% 才能继续生成",
			stats.Total, userInvitesFormatFixed1(stats.UsageRate),
		),
	}, nil
}

// userInvitesFormatFixed1 mirrors Number#toFixed(1).
func userInvitesFormatFixed1(value float64) string {
	return fmt.Sprintf("%.1f", value)
}

// ==================== pricing ====================

// UserInvitesGetCostOptions mirrors getInviteCostOptions.
func UserInvitesGetCostOptions(ctx context.Context) ([]UserInvitesCostOption, error) {
	raw, err := GetSystemConfig(ctx, userInvitesCostConfigKey, true)
	if err != nil {
		return nil, err
	}
	return userInvitesNormalizeCostOptions(userInvitesParseCostConfig(raw)), nil
}

// UserInvitesGetExpiryDays mirrors getInviteExpiryDays (Math.max(0, …)).
func UserInvitesGetExpiryDays(ctx context.Context) (int, error) {
	days, err := GetSystemConfigNumber(ctx, userInvitesExpiryDaysKey, 0)
	if err != nil {
		return 0, err
	}
	if days < 0 {
		return 0, nil
	}
	return days, nil
}

// UserInvitesGetCostQuotes mirrors getInviteCostQuotes.
func UserInvitesGetCostQuotes(ctx context.Context) ([]UserInvitesCostQuote, error) {
	options, err := UserInvitesGetCostOptions(ctx)
	if err != nil {
		return nil, err
	}
	quotes := make([]UserInvitesCostQuote, 0, len(options))
	for _, option := range options {
		label, unit, hasHandler := userInvitesCostLabel(option.Resource)
		displayAmount := ""
		if hasHandler {
			displayAmount = userInvitesCostFormat(option.Resource, option.Amount)
		}
		if displayAmount == "" {
			// `handler?.label || option.resource` and
			// `handler?.format(...) || `${option.amount}${unit}``.
			if !hasHandler {
				label = option.Resource
				unit = ""
			}
			displayAmount = userInvitesFormatNumber(option.Amount) + unit
		}
		quotes = append(quotes, UserInvitesCostQuote{
			Resource:      option.Resource,
			Amount:        option.Amount,
			Enabled:       option.Enabled,
			Label:         label,
			Unit:          unit,
			DisplayAmount: displayAmount,
		})
	}
	return quotes, nil
}

// UserInvitesGetBalances mirrors getUserInviteBalances: the user's balance plus
// the (upserted) points row. A missing user reports 0 without error.
func UserInvitesGetBalances(ctx context.Context, userID int32) (UserInvitesBalance, error) {
	var balance decimal.Decimal
	err := DB.WithContext(ctx).Raw(
		"SELECT balance FROM users WHERE id = ?", userID,
	).Scan(&balance).Error
	if err != nil {
		return UserInvitesBalance{}, err
	}

	// prisma.userPoints.upsert({ update: {}, create: { userId } }) — an empty
	// update never bumps updated_at, so ON CONFLICT DO NOTHING is the exact
	// equivalent.
	if err := DB.WithContext(ctx).Exec(
		"INSERT INTO user_points (user_id, updated_at) VALUES (?, NOW())"+
			" ON CONFLICT (user_id) DO NOTHING", userID,
	).Error; err != nil {
		return UserInvitesBalance{}, err
	}

	var points int32
	if err := DB.WithContext(ctx).Raw(
		"SELECT points FROM user_points WHERE user_id = ?", userID,
	).Scan(&points).Error; err != nil {
		return UserInvitesBalance{}, err
	}

	return UserInvitesBalance{Balance: decimalToFloat64Decimal(balance), Points: points}, nil
}

// decimalToFloat64Decimal mirrors Number(decimal).
func decimalToFloat64Decimal(value decimal.Decimal) float64 {
	asFloat, _ := value.Float64()
	return asFloat
}

// userInvitesChargeGenerationCost mirrors chargeInviteGenerationCost. It must run
// on the same pgx.Tx as the invite insert so the advisory lock covers both.
func userInvitesChargeGenerationCost(
	ctx context.Context,
	tx pgx.Tx,
	userID int32,
	inviteCode string,
	resource string,
	costOptions []UserInvitesCostOption,
) (userInvitesCostSnapshot, error) {
	var option *UserInvitesCostOption
	for index := range costOptions {
		if costOptions[index].Resource == resource && costOptions[index].Enabled {
			option = &costOptions[index]
			break
		}
	}
	if option == nil {
		return userInvitesCostSnapshot{}, ErrUserInvitesCostOptionUnavailable
	}

	label, unit, hasHandler := userInvitesCostLabel(option.Resource)
	if !hasHandler {
		return userInvitesCostSnapshot{}, ErrUserInvitesCostResourceUnsupported
	}

	switch option.Resource {
	case "balance":
		if err := userInvitesChargeBalance(ctx, tx, userID, option.Amount, inviteCode); err != nil {
			return userInvitesCostSnapshot{}, err
		}
	case "points":
		if err := userInvitesChargePoints(ctx, tx, userID, option.Amount, inviteCode); err != nil {
			return userInvitesCostSnapshot{}, err
		}
	}

	return userInvitesCostSnapshot{
		Resource:      option.Resource,
		Amount:        option.Amount,
		Label:         label,
		Unit:          unit,
		DisplayAmount: userInvitesCostFormat(option.Resource, option.Amount),
		ChargedAt:     isoTimestamp(time.Now().UTC()),
	}, nil
}

// userInvitesChargeBalance mirrors the balance handler of invite-pricing.ts.
func userInvitesChargeBalance(ctx context.Context, tx pgx.Tx, userID int32, amount float64, inviteCode string) error {
	if amount <= 0 {
		return nil
	}
	if err := AdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID); err != nil {
		return err
	}

	amountDecimal := models.Decimal{Decimal: decimal.NewFromFloat(amount).Round(2)}
	rows, err := tx.Query(ctx,
		`UPDATE users SET balance = balance - $1
		 WHERE id = $2 AND balance >= $1
		 RETURNING (balance + $1)::numeric AS balance_before, balance::numeric AS balance_after`,
		amountDecimal, userID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var balanceBefore, balanceAfter decimal.Decimal
	found := false
	for rows.Next() {
		if err := rows.Scan(&balanceBefore, &balanceAfter); err != nil {
			return err
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		var exists int32
		err := tx.QueryRow(ctx, "SELECT id FROM users WHERE id = $1", userID).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserInvitesUserNotFound
		}
		if err != nil {
			return err
		}
		return ErrUserInvitesInsufficientBalance
	}

	remark := "生成邀请码 " + inviteCode
	_, err = tx.Exec(ctx,
		`INSERT INTO balance_logs (user_id, type, amount, balance_before, balance_after, remark, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, string(models.BalanceLogTypeInviteGenerate),
		models.Decimal{Decimal: amountDecimal.Neg()},
		models.Decimal{Decimal: balanceBefore}, models.Decimal{Decimal: balanceAfter}, remark)
	return err
}

// userInvitesChargePoints mirrors the points handler of invite-pricing.ts.
func userInvitesChargePoints(ctx context.Context, tx pgx.Tx, userID int32, amount float64, inviteCode string) error {
	if amount <= 0 {
		return nil
	}
	if err := AdvisoryTransactionLock(ctx, tx, UserPointsLockNamespace, userID); err != nil {
		return err
	}

	// `tx.userPoints.upsert({ update: {}, create: { userId } })`.
	if _, err := tx.Exec(ctx,
		"INSERT INTO user_points (user_id, updated_at) VALUES ($1, NOW())"+
			" ON CONFLICT (user_id) DO NOTHING", userID,
	); err != nil {
		return err
	}

	pointsAmount := int32(amount)
	rows, err := tx.Query(ctx,
		`UPDATE user_points SET points = points - $1,
		        total_spent = total_spent + $1,
		        updated_at = NOW()
		 WHERE user_id = $2 AND points >= $1
		 RETURNING points + $1 AS points_before, points AS points_after`,
		pointsAmount, userID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var pointsBefore, pointsAfter int32
	found := false
	for rows.Next() {
		if err := rows.Scan(&pointsBefore, &pointsAfter); err != nil {
			return err
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return ErrUserInvitesInsufficientPoints
	}

	remark := "生成邀请码 " + inviteCode
	_, err = tx.Exec(ctx,
		`INSERT INTO points_logs (user_id, type, amount, points_before, points_after, remark, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, string(models.PointsLogTypeInviteGenerate), -pointsAmount,
		pointsBefore, pointsAfter, remark)
	return err
}

// ==================== write ====================

// UserInvitesCreate mirrors createInviteCode(code, createdBy, expiresAt) and
// returns the new row id. The standalone helper is unused by the routes (they
// create inside the generate transaction) but completes the module port.
func UserInvitesCreate(ctx context.Context, code string, createdBy int32, expiresAt *time.Time) (int32, error) {
	var id int32
	if err := DB.WithContext(ctx).Raw(
		"INSERT INTO invite_codes (code, created_by, expires_at, created_at) VALUES (?, ?, ?, NOW()) RETURNING id",
		code, createdBy, expiresAt,
	).Scan(&id).Error; err != nil {
		return 0, err
	}
	return id, nil
}

// UserInvitesUse mirrors useInviteCode: updateMany by code, no row-count check.
func UserInvitesUse(ctx context.Context, code string, userID int32) error {
	return DB.WithContext(ctx).Exec(
		"UPDATE invite_codes SET used_by = ?, used_at = NOW() WHERE code = ?",
		userID, code,
	).Error
}

// UserInvitesDelete mirrors deleteInviteCode. Prisma's delete rejects a missing
// row (P2025), which is reported as an error here too.
func UserInvitesDelete(ctx context.Context, id int32) error {
	result := DB.WithContext(ctx).Exec("DELETE FROM invite_codes WHERE id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UserInvitesGenerateResult is the { invites, count } body produced by the
// transaction.
type UserInvitesGenerateResult struct {
	Invites []UserInvitesCodeRow
	Count   int
}

// UserInvitesGenerate mirrors the prisma.$transaction loop of the POST route: for
// each of `count` iterations it generates a code, charges the cost on the same
// transaction and inserts the invite row. Any failure rolls the whole batch back.
func UserInvitesGenerate(
	ctx context.Context,
	userID int32,
	resource string,
	count int,
	expiresAt *time.Time,
	costOptions []UserInvitesCostOption,
) (UserInvitesGenerateResult, error) {
	tx, err := userInvitesBeginTx(ctx)
	if err != nil {
		return UserInvitesGenerateResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	invites := make([]UserInvitesCodeRow, 0, count)
	for index := 0; index < count; index++ {
		code, err := userInvitesRandomCode(userInvitesCodeLength)
		if err != nil {
			return UserInvitesGenerateResult{}, err
		}

		snapshot, err := userInvitesChargeGenerationCost(ctx, tx, userID, code, resource, costOptions)
		if err != nil {
			return UserInvitesGenerateResult{}, err
		}
		encodedSnapshot, err := json.Marshal(snapshot)
		if err != nil {
			return UserInvitesGenerateResult{}, err
		}

		var id int32
		var createdAt time.Time
		err = tx.QueryRow(ctx,
			`INSERT INTO invite_codes (code, created_by, expires_at, cost_snapshot, created_at)
			 VALUES ($1, $2, $3, $4::jsonb, NOW())
			 RETURNING id, created_at`,
			code, userID, expiresAt, string(encodedSnapshot),
		).Scan(&id, &createdAt)
		if err != nil {
			return UserInvitesGenerateResult{}, err
		}

		invites = append(invites, UserInvitesCodeRow{
			ID:           id,
			Code:         code,
			CreatedBy:    userID,
			ExpiresAt:    expiresAt,
			CostSnapshot: encodedSnapshot,
			CreatedAt:    createdAt,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return UserInvitesGenerateResult{}, err
	}
	return UserInvitesGenerateResult{Invites: invites, Count: len(invites)}, nil
}
