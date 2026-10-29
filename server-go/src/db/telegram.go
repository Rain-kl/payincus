// Port of the Telegram-binding persistence used by server/src/routes/telegram.ts.
//
// The TypeScript route talks to two Prisma models:
//
//	prisma.userTelegramBinding  (table user_telegram_bindings)
//	prisma.telegramBindToken    (table telegram_bind_tokens)
//
// Every query below mirrors the exact Prisma call it replaces, including the
// `select` projections of the `user` relation (the admin list selects
// id/username/email/status, the webhook's binding lookup selects id/username/status,
// and the admin delete selects id/username only). Prisma's `contains` compiles to a
// case-sensitive LIKE with `%`/`_`/`\` escaped, so the search filter here does the
// same rather than using ILIKE.
//
// Every exported symbol is prefixed `Telegram*` because db is a flat package shared
// by parallel module ports.
package db

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// telegramUserIDMax mirrors the range of Prisma's Int (32-bit signed). A numeric
	// search term outside it makes Prisma raise a client error, i.e. a 500.
	telegramUserIDMax = int64(1<<31 - 1)
	// telegramMaxSafeInteger mirrors Number.MAX_SAFE_INTEGER (2^53-1). The
	// TypeScript only adds the id/userId equality clauses for safe integers;
	// larger digit strings silently keep just the `contains` clauses.
	telegramMaxSafeInteger = int64(1)<<53 - 1
)

// ErrTelegramSearchIDOutOfRange reports a numeric admin search that fits a JS safe
// integer but not a Prisma Int column. The TypeScript lets Prisma throw (→ 500).
var ErrTelegramSearchIDOutOfRange = errors.New("Unable to fit value into a 32-bit signed integer")

// TelegramBindingUser is the `user` relation projection of a binding.
type TelegramBindingUser struct {
	ID       int32   `json:"id"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
	Status   string  `json:"status"`
}

// TelegramBindingWithUser is a binding plus its (always present) user relation.
type TelegramBindingWithUser struct {
	ID               int32
	UserID           int32
	TelegramUserID   string
	TelegramUsername *string
	FirstName        *string
	LastName         *string
	BoundAt          time.Time
	UpdatedAt        time.Time
	User             *TelegramBindingUser
}

// TelegramBindingListOptions mirrors the admin list query.
//
// Page/PageSize are already clamped by the route; Search is already trimmed and
// nil when absent.
type TelegramBindingListOptions struct {
	Page     int
	PageSize int
	Search   *string
}

// TelegramUpsertBindingInput mirrors the `update`/`create` halves of the webhook's
// `telegramBindingModel.upsert` call.
type TelegramUpsertBindingInput struct {
	UserID           int32
	TelegramUserID   string
	TelegramUsername *string
	FirstName        *string
	LastName         *string
	BoundAt          time.Time
}

// telegramBindingColumns is the projection of a binding row.
const telegramBindingColumns = `
	id, user_id, telegram_user_id, telegram_username, first_name, last_name,
	bound_at, updated_at`

// telegramBindingJoinedColumns is telegramBindingColumns with the "b." alias plus
// the user relation columns.
const telegramBindingJoinedColumns = `
	b.id, b.user_id, b.telegram_user_id, b.telegram_username, b.first_name, b.last_name,
	b.bound_at, b.updated_at,
	u.id AS joined_user_id, u.username AS joined_username, u.email AS joined_email,
	u.status::text AS joined_status`

// telegramBindingJoinedRow is the flat scan target for a joined select.
type telegramBindingJoinedRow struct {
	ID               int32     `gorm:"column:id"`
	UserID           int32     `gorm:"column:user_id"`
	TelegramUserID   string    `gorm:"column:telegram_user_id"`
	TelegramUsername *string   `gorm:"column:telegram_username"`
	FirstName        *string   `gorm:"column:first_name"`
	LastName         *string   `gorm:"column:last_name"`
	BoundAt          time.Time `gorm:"column:bound_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
	JoinedUserID     *int32    `gorm:"column:joined_user_id"`
	JoinedUsername   *string   `gorm:"column:joined_username"`
	JoinedEmail      *string   `gorm:"column:joined_email"`
	JoinedStatus     *string   `gorm:"column:joined_status"`
}

// toBinding assembles the relation. The relation is required in the schema, but a
// missing join row degrades to a nil user rather than panicking.
func (row telegramBindingJoinedRow) toBinding() TelegramBindingWithUser {
	binding := TelegramBindingWithUser{
		ID:               row.ID,
		UserID:           row.UserID,
		TelegramUserID:   row.TelegramUserID,
		TelegramUsername: row.TelegramUsername,
		FirstName:        row.FirstName,
		LastName:         row.LastName,
		BoundAt:          row.BoundAt,
		UpdatedAt:        row.UpdatedAt,
	}
	if row.JoinedUserID != nil && row.JoinedUsername != nil && row.JoinedStatus != nil {
		binding.User = &TelegramBindingUser{
			ID:       *row.JoinedUserID,
			Username: *row.JoinedUsername,
			Email:    row.JoinedEmail,
			Status:   *row.JoinedStatus,
		}
	}
	return binding
}

// telegramEscapeLike mirrors Prisma's LIKE-pattern escaping for `contains`
// (backslash is the escape character; `%` and `_` become literals).
func telegramEscapeLike(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, ch := range value {
		switch ch {
		case '\\', '%', '_':
			builder.WriteByte('\\')
		}
		builder.WriteRune(ch)
	}
	return builder.String()
}

// telegramBindingSearchConditions builds the `where.OR` array of the admin list.
//
// The TypeScript pushes six `contains` clauses (telegramUserId, telegramUsername,
// firstName, lastName, user.username, user.email) and, when the term is all digits
// and a safe integer, two equality clauses (id, userId).
func telegramBindingSearchConditions(search string) ([]string, []any, error) {
	pattern := "%" + telegramEscapeLike(search) + "%"
	clauses := []string{
		"b.telegram_user_id LIKE ?",
		"b.telegram_username LIKE ?",
		"b.first_name LIKE ?",
		"b.last_name LIKE ?",
		"u.username LIKE ?",
		"u.email LIKE ?",
	}
	args := []any{pattern, pattern, pattern, pattern, pattern, pattern}

	if telegramAllDigits(search) {
		numeric, err := strconv.ParseInt(search, 10, 64)
		if err != nil {
			// Beyond int64 the JS value is 1e19+, i.e. not a safe integer, so the
			// TypeScript skips the id/userId clauses instead of erroring.
			return clauses, args, nil
		}
		if numeric > telegramMaxSafeInteger {
			// Number.isSafeInteger is false → the clauses are not added at all.
			return clauses, args, nil
		}
		if numeric > telegramUserIDMax {
			// A safe integer that Prisma cannot store in an Int column: the query
			// throws, which the route surfaces as a 500.
			return nil, nil, ErrTelegramSearchIDOutOfRange
		}
		clauses = append(clauses, "b.id = ?", "b.user_id = ?")
		args = append(args, numeric, numeric)
	}

	return clauses, args, nil
}

// telegramAllDigits mirrors /^\d+$/ (ASCII digits only, at least one).
func telegramAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// TelegramGetBindingByUserID mirrors
// `userTelegramBinding.findUnique({ where: { userId } })`.
func TelegramGetBindingByUserID(ctx context.Context, userID int32) (*models.UserTelegramBinding, error) {
	var bindings []models.UserTelegramBinding
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+telegramBindingColumns+" FROM user_telegram_bindings WHERE user_id = ? LIMIT 1",
		userID,
	).Scan(&bindings).Error; err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	return &bindings[0], nil
}

// TelegramGetBindingByTelegramUserID mirrors
// `userTelegramBinding.findUnique({ where: { telegramUserId }, include: { user: { select: { id, username, status } } } })`.
func TelegramGetBindingByTelegramUserID(ctx context.Context, telegramUserID string) (*TelegramBindingWithUser, error) {
	return telegramFindOneBinding(ctx,
		"SELECT "+telegramBindingJoinedColumns+
			" FROM user_telegram_bindings b JOIN users u ON u.id = b.user_id"+
			" WHERE b.telegram_user_id = ? LIMIT 1",
		telegramUserID)
}

// TelegramGetBindingByID mirrors
// `userTelegramBinding.findUnique({ where: { id }, include: { user: { select: { id, username } } } })`.
func TelegramGetBindingByID(ctx context.Context, id int32) (*TelegramBindingWithUser, error) {
	return telegramFindOneBinding(ctx,
		"SELECT "+telegramBindingJoinedColumns+
			" FROM user_telegram_bindings b JOIN users u ON u.id = b.user_id"+
			" WHERE b.id = ? LIMIT 1",
		id)
}

func telegramFindOneBinding(ctx context.Context, query string, args ...any) (*TelegramBindingWithUser, error) {
	var rows []telegramBindingJoinedRow
	if err := DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	binding := rows[0].toBinding()
	return &binding, nil
}

// TelegramListBindings mirrors the admin list's findMany + count pair:
// `orderBy: { boundAt: 'desc' }, skip: (page-1)*pageSize, take: pageSize`.
func TelegramListBindings(ctx context.Context, options TelegramBindingListOptions) ([]TelegramBindingWithUser, int64, error) {
	where := ""
	args := make([]any, 0, 10)

	if options.Search != nil && *options.Search != "" {
		clauses, clauseArgs, err := telegramBindingSearchConditions(*options.Search)
		if err != nil {
			return nil, 0, err
		}
		where = " WHERE (" + strings.Join(clauses, " OR ") + ")"
		args = append(args, clauseArgs...)
	}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM user_telegram_bindings b JOIN users u ON u.id = b.user_id"+where,
		args...,
	).Scan(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (options.Page - 1) * options.PageSize
	if offset < 0 {
		offset = 0
	}
	listArgs := append(append([]any{}, args...), options.PageSize, offset)

	var rows []telegramBindingJoinedRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+telegramBindingJoinedColumns+
			" FROM user_telegram_bindings b JOIN users u ON u.id = b.user_id"+where+
			" ORDER BY b.bound_at DESC LIMIT ? OFFSET ?",
		listArgs...,
	).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}

	bindings := make([]TelegramBindingWithUser, 0, len(rows))
	for _, row := range rows {
		bindings = append(bindings, row.toBinding())
	}
	return bindings, total, nil
}

// TelegramUpsertBinding mirrors `userTelegramBinding.upsert({ where: { userId },
// update, create })`. Prisma's @updatedAt refreshes updated_at on the update path,
// which the ON CONFLICT clause reproduces explicitly.
func TelegramUpsertBinding(ctx context.Context, input TelegramUpsertBindingInput) error {
	return DB.WithContext(ctx).Exec(
		`INSERT INTO user_telegram_bindings
			(user_id, telegram_user_id, telegram_username, first_name, last_name, bound_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET
			telegram_user_id = EXCLUDED.telegram_user_id,
			telegram_username = EXCLUDED.telegram_username,
			first_name = EXCLUDED.first_name,
			last_name = EXCLUDED.last_name,
			bound_at = EXCLUDED.bound_at,
			updated_at = EXCLUDED.updated_at`,
		input.UserID,
		input.TelegramUserID,
		input.TelegramUsername,
		input.FirstName,
		input.LastName,
		input.BoundAt,
		input.BoundAt,
	).Error
}

// TelegramDeleteBindingByID mirrors `userTelegramBinding.delete({ where: { id } })`.
func TelegramDeleteBindingByID(ctx context.Context, id int32) error {
	return DB.WithContext(ctx).Exec(
		"DELETE FROM user_telegram_bindings WHERE id = ?", id).Error
}

// TelegramDeleteBindingsByUserID mirrors
// `userTelegramBinding.deleteMany({ where: { userId } })`.
func TelegramDeleteBindingsByUserID(ctx context.Context, userID int32) error {
	return DB.WithContext(ctx).Exec(
		"DELETE FROM user_telegram_bindings WHERE user_id = ?", userID).Error
}

// TelegramDeleteUnusedBindTokens mirrors
// `telegramBindToken.deleteMany({ where: { userId, usedAt: null } })`.
func TelegramDeleteUnusedBindTokens(ctx context.Context, userID int32) error {
	return DB.WithContext(ctx).Exec(
		"DELETE FROM telegram_bind_tokens WHERE user_id = ? AND used_at IS NULL", userID).Error
}

// TelegramCreateBindToken mirrors `telegramBindToken.create({ data })`.
func TelegramCreateBindToken(ctx context.Context, userID int32, tokenHash string, expiresAt time.Time) error {
	return DB.WithContext(ctx).Exec(
		"INSERT INTO telegram_bind_tokens (user_id, token_hash, expires_at) VALUES (?, ?, ?)",
		userID, tokenHash, expiresAt).Error
}

// TelegramFindBindToken mirrors
// `telegramBindToken.findFirst({ where: { tokenHash, usedAt: null, expiresAt: { gt: now } } })`.
func TelegramFindBindToken(ctx context.Context, tokenHash string, now time.Time) (*models.TelegramBindToken, error) {
	var tokens []models.TelegramBindToken
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, user_id, token_hash, expires_at, used_at, created_at"+
			" FROM telegram_bind_tokens WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? LIMIT 1",
		tokenHash, now,
	).Scan(&tokens).Error; err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	return &tokens[0], nil
}

// TelegramConsumeBindToken mirrors the webhook's conditional
// `telegramBindToken.updateMany({ where: { id, usedAt: null, expiresAt: { gt: now } }, data: { usedAt: now } })`.
// It returns the number of rows claimed; the route treats anything other than 1 as
// a stale token. The WHERE clause is the whole point of the guard: a token is
// claimed before the binding side effects run.
func TelegramConsumeBindToken(ctx context.Context, id int32, now time.Time) (int64, error) {
	result := DB.WithContext(ctx).Exec(
		"UPDATE telegram_bind_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL AND expires_at > ?",
		now, id, now)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
