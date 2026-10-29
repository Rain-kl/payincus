// Port of server/src/lib/public-api-auth.ts.
//
// This is the second of the three authentication surfaces (Web JWT / Public API
// pat_+poa_ tokens / host-agent HMAC). Public API tokens are opaque secrets: the
// database only ever stores a SHA-256 hex digest (public_api_tokens.token_hash /
// oauth_access_tokens.token_hash), never the plaintext. Verification is done with
// a constant-time comparison so a timing side channel cannot leak the digest.
//
// The plaintext token must never be logged. Everything this file emits to the log
// table contains only the token *name* / *prefix*, and lib.SanitizeTokensInString
// (used by APIErrorFor) additionally redacts any pat_/poa_ shaped value, matching
// the `\b(?:pat|poa)_[A-Za-z0-9_-]{43}\b` rule in lib/log_sanitizer.go.
package lib

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ==================== constants ====================

const (
	// PublicAPITokenPrefix is the `pat_` prefix for user-created API tokens.
	PublicAPITokenPrefix = "pat_"
	// OAuthAccessTokenPrefix is the `poa_` prefix for OAuth-issued access tokens.
	OAuthAccessTokenPrefix = "poa_"
	// PublicAPITokenPrefixLength is the number of leading characters stored in
	// token_prefix (a display hint, never a secret).
	PublicAPITokenPrefixLength = 16
	// PublicAPITokenMaxNameLength bounds a token's display name.
	PublicAPITokenMaxNameLength = 80
	// PublicAPITokenMaxScopes bounds the number of scopes on a single token.
	PublicAPITokenMaxScopes = 24

	// publicAPITokenRawBytes is the entropy of a generated token: 32 random bytes
	// → 43 base64url characters (matching `randomBytes(32).toString('base64url')`).
	publicAPITokenRawBytes = 32
	// publicAPITokenMinLength is the minimum bearer-token length accepted by
	// readPublicApiBearerToken (the Node code uses `token.length < 32`).
	publicAPITokenMinLength = 32
)

// Authenticated token sources, matching the Node `source` union.
const (
	PublicAPITokenSourceAPI   = "api_token"
	PublicAPITokenSourceOAuth = "oauth_access_token"
)

// PublicAPIScope is a single Public API permission.
type PublicAPIScope string

// The Public API scope catalog, kept in the exact order of the Node source.
const (
	PublicAPIScopeProfileRead       PublicAPIScope = "profile:read"
	PublicAPIScopeProfileWrite      PublicAPIScope = "profile:write"
	PublicAPIScopeBalanceRead       PublicAPIScope = "balance:read"
	PublicAPIScopeBalanceWrite      PublicAPIScope = "balance:write"
	PublicAPIScopeBillingRead       PublicAPIScope = "billing:read"
	PublicAPIScopeProductsRead      PublicAPIScope = "products:read"
	PublicAPIScopeServicesRead      PublicAPIScope = "services:read"
	PublicAPIScopeServicesOperate   PublicAPIScope = "services:operate"
	PublicAPIScopeServicesBilling   PublicAPIScope = "services:billing"
	PublicAPIScopeOrdersRead        PublicAPIScope = "orders:read"
	PublicAPIScopeTicketsRead       PublicAPIScope = "tickets:read"
	PublicAPIScopeTicketsWrite      PublicAPIScope = "tickets:write"
	PublicAPIScopeNotificationsRead PublicAPIScope = "notifications:read"
	PublicAPIScopeNotificationsSend PublicAPIScope = "notifications:send"
)

// PublicAPIScopes mirrors the Node PUBLIC_API_SCOPES tuple.
var PublicAPIScopes = []PublicAPIScope{
	PublicAPIScopeProfileRead,
	PublicAPIScopeProfileWrite,
	PublicAPIScopeBalanceRead,
	PublicAPIScopeBalanceWrite,
	PublicAPIScopeBillingRead,
	PublicAPIScopeProductsRead,
	PublicAPIScopeServicesRead,
	PublicAPIScopeServicesOperate,
	PublicAPIScopeServicesBilling,
	PublicAPIScopeOrdersRead,
	PublicAPIScopeTicketsRead,
	PublicAPIScopeTicketsWrite,
	PublicAPIScopeNotificationsRead,
	PublicAPIScopeNotificationsSend,
}

// ==================== error codes ====================
//
// The Node module replies with literal codes that are NOT part of the shared
// lib/errors.ts catalog (it sends `reply.code(status).send({ error, code })`
// directly, bypassing apiError). The generated errors_gen.go therefore does not
// contain them, so they are declared here with their byte-identical codes and
// messages. ACCOUNT_BANNED *is* in the catalog but the module uses a different
// message ("Account banned" vs the catalog's "Account has been disabled"), so it
// is emitted with its explicit message too.
const (
	ErrPublicAPITokenRequired ErrorCode = "PUBLIC_API_TOKEN_REQUIRED"
	ErrPublicAPITokenInvalid  ErrorCode = "PUBLIC_API_TOKEN_INVALID"
	ErrPublicAPITokenRevoked  ErrorCode = "PUBLIC_API_TOKEN_REVOKED"
	ErrPublicAPITokenExpired  ErrorCode = "PUBLIC_API_TOKEN_EXPIRED"
	ErrPublicAPIScopeRequired ErrorCode = "PUBLIC_API_SCOPE_REQUIRED"
	ErrOAuthAppDisabled       ErrorCode = "OAUTH_APP_DISABLED"
)

// ==================== scope metadata ====================

// PublicAPIScopeMetadata mirrors the Node PublicApiScopeMetadata interface.
type PublicAPIScopeMetadata struct {
	Scope       PublicAPIScope `json:"scope"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Risk        string         `json:"risk"`
	Access      string         `json:"access"`
	Resources   []string       `json:"resources"`
	Implemented bool           `json:"implemented"`
	Notes       []string       `json:"notes"`
}

// PublicAPIScopeMetadataByScope mirrors PUBLIC_API_SCOPE_METADATA.
var PublicAPIScopeMetadataByScope = map[PublicAPIScope]PublicAPIScopeMetadata{
	PublicAPIScopeProfileRead: {
		Scope:       PublicAPIScopeProfileRead,
		Title:       "读取个人资料",
		Description: "读取当前授权用户的基础资料摘要。",
		Risk:        "low",
		Access:      "read",
		Resources:   []string{"/api/v1/me"},
		Implemented: true,
		Notes:       []string{"只返回 token 所属用户自己的资料字段。"},
	},
	PublicAPIScopeProfileWrite: {
		Scope:       PublicAPIScopeProfileWrite,
		Title:       "更新个人资料",
		Description: "更新当前授权用户的低风险展示资料字段。",
		Risk:        "medium",
		Access:      "write",
		Resources:   []string{"/api/v1/me"},
		Implemented: true,
		Notes:       []string{"仅允许更新 avatarStyle；邮箱、密码、角色、状态、余额和 2FA 设置不可写。"},
	},
	PublicAPIScopeBalanceRead: {
		Scope:       PublicAPIScopeBalanceRead,
		Title:       "读取余额",
		Description: "读取当前授权用户的账户余额和安全余额流水。",
		Risk:        "medium",
		Access:      "read",
		Resources:   []string{"/api/v1/balance", "/api/v1/balance/logs"},
		Implemented: true,
		Notes:       []string{"不暴露支付 provider payload、调账对象、AFF 余额或其他用户数据。"},
	},
	PublicAPIScopeBalanceWrite: {
		Scope:       PublicAPIScopeBalanceWrite,
		Title:       "提交余额调整申请",
		Description: "提交并读取当前授权用户自己的待审批余额调整申请。",
		Risk:        "high",
		Access:      "write",
		Resources:   []string{"/api/v1/balance/adjustment-requests"},
		Implemented: true,
		Notes:       []string{"不会直接修改余额、余额流水、支付记录或充值订单。"},
	},
	PublicAPIScopeBillingRead: {
		Scope:       PublicAPIScopeBillingRead,
		Title:       "读取账单",
		Description: "读取当前授权用户的服务账单记录。",
		Risk:        "medium",
		Access:      "read",
		Resources:   []string{"/api/v1/billing-records", "/api/v1/billing-records/:id"},
		Implemented: true,
		Notes:       []string{"仅返回账单摘要；不返回 provider payload 和内部对账数据。"},
	},
	PublicAPIScopeProductsRead: {
		Scope:       PublicAPIScopeProductsRead,
		Title:       "读取产品目录",
		Description: "读取已启用的公开产品和套餐目录。",
		Risk:        "low",
		Access:      "read",
		Resources:   []string{"/api/v1/products"},
		Implemented: true,
		Notes:       []string{"仅返回已启用的套餐和方案。"},
	},
	PublicAPIScopeServicesRead: {
		Scope:       PublicAPIScopeServicesRead,
		Title:       "读取服务",
		Description: "读取当前授权用户的服务摘要和安全关联信息。",
		Risk:        "medium",
		Access:      "read",
		Resources:   []string{"/api/v1/services", "/api/v1/services/:id"},
		Implemented: true,
		Notes:       []string{"不暴露 root 密码、Incus ID、节点内部配置或高权限连接材料。"},
	},
	PublicAPIScopeServicesOperate: {
		Scope:       PublicAPIScopeServicesOperate,
		Title:       "服务电源操作",
		Description: "为当前授权用户的服务排队、查询和取消受控电源任务。",
		Risk:        "high",
		Access:      "operate",
		Resources:   []string{"/api/v1/services/:id/actions", "/api/v1/services/:id/tasks/:taskId"},
		Implemented: true,
		Notes:       []string{"仅允许 start、stop、restart；不开放创建、暂停、续费、重装、删除、迁移和交付操作。"},
	},
	PublicAPIScopeServicesBilling: {
		Scope:       PublicAPIScopeServicesBilling,
		Title:       "服务计费操作",
		Description: "通过内部计费状态机为当前授权用户的服务续费。",
		Risk:        "high",
		Access:      "write",
		Resources:   []string{"/api/v1/services/:id/renew"},
		Implemented: true,
		Notes:       []string{"仅允许续费；余额扣减、账单记录和过期服务解封由内部续费事务处理。"},
	},
	PublicAPIScopeOrdersRead: {
		Scope:       PublicAPIScopeOrdersRead,
		Title:       "读取订单",
		Description: "读取当前授权用户的充值和实例账单订单。",
		Risk:        "medium",
		Access:      "read",
		Resources:   []string{"/api/v1/orders", "/api/v1/orders/:id"},
		Implemented: true,
		Notes:       []string{"不暴露支付回调、provider 配置快照、原始查询结果或完整交易号。"},
	},
	PublicAPIScopeTicketsRead: {
		Scope:       PublicAPIScopeTicketsRead,
		Title:       "读取工单",
		Description: "读取当前授权用户的工单、消息和安全附件元数据。",
		Risk:        "medium",
		Access:      "read",
		Resources:   []string{"/api/v1/tickets", "/api/v1/tickets/:id"},
		Implemented: true,
		Notes:       []string{"不暴露内部备注、存储 provider ID 和其他用户工单。"},
	},
	PublicAPIScopeTicketsWrite: {
		Scope:       PublicAPIScopeTicketsWrite,
		Title:       "写入工单",
		Description: "创建工单、添加公开回复、上传受控图片附件，并关闭或重新打开自己的工单。",
		Risk:        "high",
		Access:      "write",
		Resources:   []string{"/api/v1/tickets", "/api/v1/tickets/:id/replies", "/api/v1/tickets/:id/status"},
		Implemented: true,
		Notes:       []string{"不允许内部备注、任意状态、优先级/分类覆盖、处理人变更或跨用户写入。"},
	},
	PublicAPIScopeNotificationsRead: {
		Scope:       PublicAPIScopeNotificationsRead,
		Title:       "读取通知",
		Description: "读取当前授权用户的站内通知和未读数量。",
		Risk:        "low",
		Access:      "read",
		Resources:   []string{"/api/v1/notifications", "/api/v1/notifications/unread-count"},
		Implemented: true,
		Notes:       []string{"不暴露渠道配置、外部投递日志、原始事件 payload、广播目标或其他用户消息。"},
	},
	PublicAPIScopeNotificationsSend: {
		Scope:       PublicAPIScopeNotificationsSend,
		Title:       "发送自有通知",
		Description: "向当前授权用户自己发送短内容、受控模板通知。",
		Risk:        "medium",
		Access:      "write",
		Resources:   []string{"/api/v1/notifications"},
		Implemented: true,
		Notes:       []string{"仅允许自有通知、平台模板和已启用扩展声明的模板；不允许广播、HTML、任意渠道或覆盖内部事件类型。"},
	},
}

// ListPublicAPIScopeMetadata mirrors listPublicApiScopeMetadata(); with no
// arguments it returns metadata for the full catalog (the Node default param).
func ListPublicAPIScopeMetadata(scopes ...PublicAPIScope) []PublicAPIScopeMetadata {
	if len(scopes) == 0 {
		scopes = PublicAPIScopes
	}
	metadata := make([]PublicAPIScopeMetadata, 0, len(scopes))
	for _, scope := range scopes {
		metadata = append(metadata, PublicAPIScopeMetadataByScope[scope])
	}
	return metadata
}

// ==================== authenticated token shape ====================

// AuthenticatedPublicAPIUser is the trimmed user projection embedded in an
// authenticated token (matches the Prisma `select` in the Node source).
type AuthenticatedPublicAPIUser struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	Role          string  `json:"role"`
	Status        string  `json:"status"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

// AuthenticatedPublicAPIToken mirrors the Node AuthenticatedPublicApiToken.
type AuthenticatedPublicAPIToken struct {
	ID          int32                      `json:"id"`
	Source      string                     `json:"source"`
	UserID      int32                      `json:"userId"`
	Name        string                     `json:"name"`
	TokenPrefix string                     `json:"tokenPrefix"`
	Scopes      []PublicAPIScope           `json:"scopes"`
	User        AuthenticatedPublicAPIUser `json:"user"`
}

// ==================== pure helpers ====================

// safeTimingEqual mirrors safeTimingEqual(): constant-time comparison that
// short-circuits only on a length mismatch (the Node timingSafeEqual also
// requires equal lengths).
func safeTimingEqual(a, b string) bool {
	aBytes := []byte(a)
	bBytes := []byte(b)
	if len(aBytes) != len(bBytes) {
		return false
	}
	return subtle.ConstantTimeCompare(aBytes, bBytes) == 1
}

// GeneratePublicAPIToken mirrors generatePublicApiToken(): `pat_` + 43 base64url
// characters (32 random bytes). Panics only if the system CSPRNG fails, which
// mirrors the Node randomBytes throwing.
func GeneratePublicAPIToken() string {
	buf := make([]byte, publicAPITokenRawBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return PublicAPITokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// HashPublicAPIToken mirrors hashPublicApiToken(): lowercase SHA-256 hex digest.
// Only the digest is ever persisted; the plaintext token is never stored or logged.
func HashPublicAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GetPublicAPITokenPrefix mirrors getPublicApiTokenPrefix(): the first 16 chars.
func GetPublicAPITokenPrefix(token string) string {
	if len(token) <= PublicAPITokenPrefixLength {
		return token
	}
	return token[:PublicAPITokenPrefixLength]
}

// NormalizePublicAPITokenName mirrors normalizePublicApiTokenName(); returns nil
// (the Node `null`) for anything that is not a non-empty string of at most
// PUBLIC_API_TOKEN_MAX_NAME_LENGTH characters.
func NormalizePublicAPITokenName(value any) *string {
	var raw string
	switch v := value.(type) {
	case string:
		raw = v
	case *string:
		if v == nil {
			return nil
		}
		raw = *v
	default:
		return nil
	}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if len([]rune(trimmed)) > PublicAPITokenMaxNameLength {
		return nil
	}
	return &trimmed
}

// NormalizePublicAPIScopes mirrors normalizePublicApiScopes(); returns nil (the
// Node `null`) unless the input is an array of 1..MAX scopes whose entries are all
// known, non-duplicate scope strings. Unknown / duplicate / non-string entries
// make the whole input invalid, exactly like the Node length check.
func NormalizePublicAPIScopes(value any) []PublicAPIScope {
	var raw []any
	switch v := value.(type) {
	case []any:
		raw = v
	case []string:
		raw = make([]any, len(v))
		for i, item := range v {
			raw[i] = item
		}
	default:
		return nil
	}

	if len(raw) == 0 || len(raw) > PublicAPITokenMaxScopes {
		return nil
	}

	allowed := make(map[PublicAPIScope]bool, len(PublicAPIScopes))
	for _, scope := range PublicAPIScopes {
		allowed[scope] = true
	}

	seen := make(map[PublicAPIScope]bool, len(raw))
	scopes := make([]PublicAPIScope, 0, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		scope := PublicAPIScope(strings.TrimSpace(text))
		if !allowed[scope] || seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}

	if len(scopes) != len(raw) {
		return nil
	}
	return scopes
}

// ReadPublicAPIBearerToken mirrors readPublicApiBearerToken(): extracts a
// `Bearer pat_…` / `Bearer poa_…` token, or nil when absent / malformed / short.
func ReadPublicAPIBearerToken(request *http.Request) *string {
	if request == nil {
		return nil
	}
	auth := request.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil
	}

	token := strings.TrimSpace(auth[len("Bearer "):])
	isPublicAPIToken := strings.HasPrefix(token, PublicAPITokenPrefix)
	isOAuthAccessToken := strings.HasPrefix(token, OAuthAccessTokenPrefix)
	if (!isPublicAPIToken && !isOAuthAccessToken) || len(token) < publicAPITokenMinLength {
		return nil
	}
	return &token
}

// serializePublicAPIScopes mirrors the Node serializeScopes(): it never returns
// nil, so the JSON is always an array (`[]`) rather than `null`.
func serializePublicAPIScopes(value any) []PublicAPIScope {
	if normalized := NormalizePublicAPIScopes(value); normalized != nil {
		return normalized
	}
	return []PublicAPIScope{}
}

// decodeStoredScopes turns the jsonb `scopes` column into the normalized slice.
func decodeStoredScopes(raw []byte) []PublicAPIScope {
	if len(raw) == 0 {
		return []PublicAPIScope{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return []PublicAPIScope{}
	}
	return serializePublicAPIScopes(value)
}

// ==================== persistence records + seams ====================

type publicAPIUserProjection struct {
	ID            int32
	Username      string
	Email         *string
	Role          string
	Status        string
	AvatarStyle   string
	AvatarBadgeID *string
}

type publicAPITokenRecord struct {
	ID          int32
	UserID      int32
	Name        string
	TokenPrefix string
	TokenHash   string
	Scopes      []byte
	RevokedAt   *time.Time
	ExpiresAt   *time.Time
	User        publicAPIUserProjection
}

type oauthAccessTokenRecord struct {
	ID          int32
	UserID      int32
	TokenPrefix string
	TokenHash   string
	Scopes      []byte
	RevokedAt   *time.Time
	ExpiresAt   time.Time
	AppName     string
	AppEnabled  bool
	User        publicAPIUserProjection
}

// The lookup / touch / log calls are indirected through these package vars so the
// auth flow (expired / revoked / scope-denied / banned) is unit-testable without a
// live database. The production implementations hit db.DB, mirroring the Prisma
// queries in the Node source.
var (
	publicAPIAuthLookupToken = func(ctx context.Context, tokenHash string) (*publicAPITokenRecord, error) {
		return queryPublicAPITokenByHash(ctx, tokenHash)
	}
	publicAPIAuthLookupOAuthToken = func(ctx context.Context, tokenHash string) (*oauthAccessTokenRecord, error) {
		return queryOAuthAccessTokenByHash(ctx, tokenHash)
	}
	publicAPIAuthTouchToken = func(ctx context.Context, id int32) error {
		return touchPublicAPITokenLastUsed(ctx, id)
	}
	publicAPIAuthTouchOAuthToken = func(ctx context.Context, id int32) error {
		return touchOAuthAccessTokenLastUsed(ctx, id)
	}
	publicAPIAuthWriteLog = func(ctx context.Context, userID *int64, action, content string) {
		db.CreateLog(ctx, userID, db.LogModuleSecurity, action, content, db.LogResultFailed, db.CreateLogOptions{})
	}
)

type publicAPITokenRow struct {
	ID            int32      `gorm:"column:id"`
	UserID        int32      `gorm:"column:user_id"`
	Name          string     `gorm:"column:name"`
	TokenPrefix   string     `gorm:"column:token_prefix"`
	TokenHash     string     `gorm:"column:token_hash"`
	Scopes        []byte     `gorm:"column:scopes"`
	RevokedAt     *time.Time `gorm:"column:revoked_at"`
	ExpiresAt     *time.Time `gorm:"column:expires_at"`
	Username      string     `gorm:"column:username"`
	Email         *string    `gorm:"column:email"`
	Role          string     `gorm:"column:role"`
	Status        string     `gorm:"column:status"`
	AvatarStyle   string     `gorm:"column:avatar_style"`
	AvatarBadgeID *string    `gorm:"column:avatar_badge_id"`
}

func queryPublicAPITokenByHash(ctx context.Context, tokenHash string) (*publicAPITokenRecord, error) {
	const sql = `
SELECT t.id, t.user_id, t.name, t.token_prefix, t.token_hash, t.scopes, t.revoked_at, t.expires_at,
       u.username, u.email, u.role::text AS role, u.status::text AS status, u.avatar_style, u.avatar_badge_id
FROM public_api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = ?
LIMIT 1`

	var rows []publicAPITokenRow
	if err := db.DB.WithContext(ctx).Raw(sql, tokenHash).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	row := rows[0]
	return &publicAPITokenRecord{
		ID:          row.ID,
		UserID:      row.UserID,
		Name:        row.Name,
		TokenPrefix: row.TokenPrefix,
		TokenHash:   row.TokenHash,
		Scopes:      row.Scopes,
		RevokedAt:   row.RevokedAt,
		ExpiresAt:   row.ExpiresAt,
		User: publicAPIUserProjection{
			ID:            row.UserID,
			Username:      row.Username,
			Email:         row.Email,
			Role:          row.Role,
			Status:        row.Status,
			AvatarStyle:   row.AvatarStyle,
			AvatarBadgeID: row.AvatarBadgeID,
		},
	}, nil
}

type oauthAccessTokenRow struct {
	ID            int32      `gorm:"column:id"`
	UserID        int32      `gorm:"column:user_id"`
	TokenPrefix   string     `gorm:"column:token_prefix"`
	TokenHash     string     `gorm:"column:token_hash"`
	Scopes        []byte     `gorm:"column:scopes"`
	RevokedAt     *time.Time `gorm:"column:revoked_at"`
	ExpiresAt     time.Time  `gorm:"column:expires_at"`
	AppName       string     `gorm:"column:app_name"`
	AppEnabled    bool       `gorm:"column:app_enabled"`
	Username      string     `gorm:"column:username"`
	Email         *string    `gorm:"column:email"`
	Role          string     `gorm:"column:role"`
	Status        string     `gorm:"column:status"`
	AvatarStyle   string     `gorm:"column:avatar_style"`
	AvatarBadgeID *string    `gorm:"column:avatar_badge_id"`
}

func queryOAuthAccessTokenByHash(ctx context.Context, tokenHash string) (*oauthAccessTokenRecord, error) {
	const sql = `
SELECT t.id, t.user_id, t.token_prefix, t.token_hash, t.scopes, t.revoked_at, t.expires_at,
       a.name AS app_name, a.enabled AS app_enabled,
       u.username, u.email, u.role::text AS role, u.status::text AS status, u.avatar_style, u.avatar_badge_id
FROM oauth_access_tokens t
JOIN oauth_client_apps a ON a.id = t.app_id
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = ?
LIMIT 1`

	var rows []oauthAccessTokenRow
	if err := db.DB.WithContext(ctx).Raw(sql, tokenHash).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	row := rows[0]
	return &oauthAccessTokenRecord{
		ID:          row.ID,
		UserID:      row.UserID,
		TokenPrefix: row.TokenPrefix,
		TokenHash:   row.TokenHash,
		Scopes:      row.Scopes,
		RevokedAt:   row.RevokedAt,
		ExpiresAt:   row.ExpiresAt,
		AppName:     row.AppName,
		AppEnabled:  row.AppEnabled,
		User: publicAPIUserProjection{
			ID:            row.UserID,
			Username:      row.Username,
			Email:         row.Email,
			Role:          row.Role,
			Status:        row.Status,
			AvatarStyle:   row.AvatarStyle,
			AvatarBadgeID: row.AvatarBadgeID,
		},
	}, nil
}

func touchPublicAPITokenLastUsed(ctx context.Context, id int32) error {
	now := time.Now()
	return db.DB.WithContext(ctx).
		Exec("UPDATE public_api_tokens SET last_used_at = ?, updated_at = ? WHERE id = ?", now, now, id).
		Error
}

func touchOAuthAccessTokenLastUsed(ctx context.Context, id int32) error {
	now := time.Now()
	return db.DB.WithContext(ctx).
		Exec("UPDATE oauth_access_tokens SET last_used_at = ?, updated_at = ? WHERE id = ?", now, now, id).
		Error
}

// ==================== authentication ====================

func respondPublicAPIError(c *gin.Context, status int, code ErrorCode, message string) {
	c.AbortWithStatusJSON(status, APIError{Error: message, Code: code})
}

func containsPublicAPIScope(scopes []PublicAPIScope, required PublicAPIScope) bool {
	for _, scope := range scopes {
		if scope == required {
			return true
		}
	}
	return false
}

func serializeAuthenticatedUser(user publicAPIUserProjection) AuthenticatedPublicAPIUser {
	return AuthenticatedPublicAPIUser{
		ID:            user.ID,
		Username:      user.Username,
		Email:         user.Email,
		Role:          user.Role,
		Status:        user.Status,
		AvatarStyle:   user.AvatarStyle,
		AvatarBadgeID: user.AvatarBadgeID,
	}
}

func serializeAuthenticatedAPIToken(token *publicAPITokenRecord) *AuthenticatedPublicAPIToken {
	return &AuthenticatedPublicAPIToken{
		ID:          token.ID,
		Source:      PublicAPITokenSourceAPI,
		UserID:      token.UserID,
		Name:        token.Name,
		TokenPrefix: token.TokenPrefix,
		Scopes:      decodeStoredScopes(token.Scopes),
		User:        serializeAuthenticatedUser(token.User),
	}
}

func serializeAuthenticatedOAuthToken(token *oauthAccessTokenRecord) *AuthenticatedPublicAPIToken {
	return &AuthenticatedPublicAPIToken{
		ID:          token.ID,
		Source:      PublicAPITokenSourceOAuth,
		UserID:      token.UserID,
		Name:        "OAuth app " + token.AppName,
		TokenPrefix: token.TokenPrefix,
		Scopes:      decodeStoredScopes(token.Scopes),
		User:        serializeAuthenticatedUser(token.User),
	}
}

// AuthenticatePublicAPIRequest mirrors authenticatePublicApiRequest(). On success
// it returns the authenticated token (and refreshes last_used_at). On failure it
// writes the exact Node `{ error, code }` envelope and returns nil — the caller
// must simply return when the result is nil. A nil reply is never emitted, so the
// plaintext token never leaves this function except in the caller's own response.
func AuthenticatePublicAPIRequest(c *gin.Context, requiredScope PublicAPIScope) *AuthenticatedPublicAPIToken {
	ctx := c.Request.Context()

	rawToken := ReadPublicAPIBearerToken(c.Request)
	if rawToken == nil {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenRequired, "Unauthorized")
		return nil
	}

	if strings.HasPrefix(*rawToken, OAuthAccessTokenPrefix) {
		return authenticateOAuthAccessToken(c, ctx, *rawToken, requiredScope)
	}
	return authenticateStoredAPIToken(c, ctx, *rawToken, requiredScope)
}

func authenticateOAuthAccessToken(c *gin.Context, ctx context.Context, rawToken string, requiredScope PublicAPIScope) *AuthenticatedPublicAPIToken {
	tokenHash := HashPublicAPIToken(rawToken)
	tokenPrefix := GetPublicAPITokenPrefix(rawToken)

	token, err := publicAPIAuthLookupOAuthToken(ctx, tokenHash)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil
	}

	if token == nil || !safeTimingEqual(token.TokenHash, tokenHash) || token.TokenPrefix != tokenPrefix {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenInvalid, "Unauthorized")
		return nil
	}

	if !token.AppEnabled {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrOAuthAppDisabled, "OAuth app disabled")
		return nil
	}

	if token.RevokedAt != nil {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenRevoked, "Token revoked")
		return nil
	}

	if !token.ExpiresAt.After(time.Now()) {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenExpired, "Token expired")
		return nil
	}

	if token.User.Status != "active" {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrAccountBanned, "Account banned")
		return nil
	}

	scopes := decodeStoredScopes(token.Scopes)
	if !containsPublicAPIScope(scopes, requiredScope) {
		userID := int64(token.UserID)
		publicAPIAuthWriteLog(
			ctx,
			&userID,
			"oauth_provider.token_scope_denied",
			`OAuth app "`+token.AppName+`" missing required scope "`+string(requiredScope)+`"`,
		)
		respondPublicAPIError(c, http.StatusForbidden, ErrPublicAPIScopeRequired, "Missing required API scope")
		return nil
	}

	if err := publicAPIAuthTouchOAuthToken(ctx, token.ID); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil
	}

	return serializeAuthenticatedOAuthToken(token)
}

func authenticateStoredAPIToken(c *gin.Context, ctx context.Context, rawToken string, requiredScope PublicAPIScope) *AuthenticatedPublicAPIToken {
	tokenHash := HashPublicAPIToken(rawToken)
	tokenPrefix := GetPublicAPITokenPrefix(rawToken)

	token, err := publicAPIAuthLookupToken(ctx, tokenHash)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil
	}

	if token == nil || !safeTimingEqual(token.TokenHash, tokenHash) || token.TokenPrefix != tokenPrefix {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenInvalid, "Unauthorized")
		return nil
	}

	if token.RevokedAt != nil {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenRevoked, "Token revoked")
		return nil
	}

	if token.ExpiresAt != nil && !token.ExpiresAt.After(time.Now()) {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrPublicAPITokenExpired, "Token expired")
		return nil
	}

	if token.User.Status != "active" {
		respondPublicAPIError(c, http.StatusUnauthorized, ErrAccountBanned, "Account banned")
		return nil
	}

	scopes := decodeStoredScopes(token.Scopes)
	if !containsPublicAPIScope(scopes, requiredScope) {
		userID := int64(token.UserID)
		publicAPIAuthWriteLog(
			ctx,
			&userID,
			"public_api.token_scope_denied",
			`Public API token "`+token.Name+`" missing required scope "`+string(requiredScope)+`"`,
		)
		respondPublicAPIError(c, http.StatusForbidden, ErrPublicAPIScopeRequired, "Missing required API scope")
		return nil
	}

	if err := publicAPIAuthTouchToken(ctx, token.ID); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil
	}

	return serializeAuthenticatedAPIToken(token)
}
