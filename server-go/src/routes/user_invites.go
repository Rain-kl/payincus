// Port of server/src/routes/user-invites.ts (mounted at /api/user-invites).
//
// Behavioural notes that are easy to get wrong:
//
//   - All three routes are regular-user routes (fastify.authenticateUser), so an
//     admin token gets 403 USER_ONLY rather than access.
//   - GET / uses the loose parseInt helpers (`parseInt(request.query.page, 10)`),
//     NOT the strict route-id regex: "12abc" becomes 12 and anything non-positive
//     falls back to the default. pageSize is additionally capped at 100.
//   - GET / always reports totalPages as at least 1, even for an empty list.
//   - POST / lets Fastify validate the JSON schema first (400 VALIDATION_ERROR,
//     first message only), then runs normalizeGenerateInviteBody, whose throws are
//     plain `{ error }` bodies without a `code`.
//   - The Node route declares an inline `rateLimit: { max: 20 }`, but the app-level
//     onRoute hook that would install it is registered after the rate-limit plugin,
//     so Node never enforces it (documented in GO_MIGRATION.md). Go therefore uses
//     the same global/per-route table as every other module rather than a bespoke
//     20/min limit.
package routes

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// Schema bounds, mirroring the constants and the Fastify body schema.
const (
	userInvitesMaxGenerationCount  = 10
	userInvitesCostResourceMaxLen  = 64
	userInvitesDefaultPage         = 1
	userInvitesDefaultPageSize     = 20
	userInvitesMaxPageSize         = 100
	userInvitesInvalidBodyError    = "请求参数无效"
	userInvitesMissingCostError    = "请选择生成方式"
	userInvitesInvalidCostError    = "生成方式无效"
	userInvitesInvalidCountMessage = "生成数量需为 1-10 的整数"
)

// ==================== helpers ====================

// userInvitesParseIntJS mirrors `parseInt(value || ”, 10)`: leading whitespace and
// an optional sign are consumed, then digits up to the first non-digit. No digits
// at all is NaN (ok=false).
// userInvitesParseIntJS mirrors `parseInt(value, 10)`: leading whitespace and an
// optional sign are consumed, then digits up to the first non-digit. No digits at
// all is NaN (ok=false).
//
// The result is a float64 because JavaScript numbers are doubles: truly enormous
// inputs round to a finite 1e20-ish value (or +Inf, which Number.isInteger then
// rejects), and returning int64 would make `pageSize=99999999999999999999` fall
// back to 20 instead of clamping to 100.
func userInvitesParseIntJS(value string) (float64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return 0, false
	}
	end := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		end = 1
	}
	digitsStart := end
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	if end == digitsStart {
		return 0, false
	}
	// ParseFloat overruns to ±Inf exactly where the double conversion does and
	// reports ErrRange; that still counts as a parsed value, and the caller's finite
	// check then rejects it — matching Number.isInteger(Infinity) === false.
	parsed, err := strconv.ParseFloat(trimmed[:end], 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	if math.IsNaN(parsed) {
		return 0, false
	}
	return parsed, true
}

// userInvitesParsePositiveInteger mirrors parsePositiveInteger: a finite integer >
// 0 wins, anything else (NaN, ±Inf, 0, negatives, fractions) falls back.
//
// The result is int64 so the echoed `page` prints with the same digits as the
// JavaScript number; values beyond int64 saturate (any page that large overflows
// the skip offset and errors out, exactly like Prisma).
func userInvitesParsePositiveInteger(value string, fallback int64) int64 {
	parsed, ok := userInvitesParseIntJS(value)
	if !ok || parsed <= 0 || math.IsInf(parsed, 0) || math.Trunc(parsed) != parsed {
		return fallback
	}
	if parsed >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(parsed)
}

// userInvitesGenerateBody is the normalized POST body.
type userInvitesGenerateBody struct {
	CostResource string
	Count        int
}

// userInvitesNormalizeGenerateBody mirrors normalizeGenerateInviteBody. The second
// return value is the exact `{ error }` body the TypeScript throws ("" when valid).
func userInvitesNormalizeGenerateBody(input any) (userInvitesGenerateBody, string) {
	record, ok := input.(map[string]any)
	if !ok {
		return userInvitesGenerateBody{}, userInvitesInvalidBodyError
	}

	rawCostResource, ok := record["costResource"].(string)
	if !ok {
		return userInvitesGenerateBody{}, userInvitesMissingCostError
	}
	costResource := strings.TrimSpace(rawCostResource)
	if costResource == "" || utf16Length(costResource) > userInvitesCostResourceMaxLen {
		return userInvitesGenerateBody{}, userInvitesInvalidCostError
	}

	count := 1
	if countInput, present := record["count"]; present {
		number, isNumber := countInput.(float64)
		if !isNumber || math.Trunc(number) != number ||
			number < 1 || number > userInvitesMaxGenerationCount ||
			math.IsInf(number, 0) || math.IsNaN(number) {
			return userInvitesGenerateBody{}, userInvitesInvalidCountMessage
		}
		count = int(number)
	}

	return userInvitesGenerateBody{CostResource: costResource, Count: count}, ""
}

// userInvitesPayload mirrors toInviteCodePayload. Keys are camelCase here because
// the TypeScript builds a camelCase object literal for this route.
type userInvitesUsedByUser struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
	CreatedAt     string  `json:"createdAt"`
}

type userInvitesPayload struct {
	ID           int32                  `json:"id"`
	Code         string                 `json:"code"`
	CreatedBy    int32                  `json:"createdBy"`
	UsedBy       *int32                 `json:"usedBy"`
	UsedAt       *string                `json:"usedAt"`
	ExpiresAt    *string                `json:"expiresAt"`
	CreatedAt    string                 `json:"createdAt"`
	CostSnapshot json.RawMessage        `json:"costSnapshot"`
	RegisterURL  string                 `json:"registerUrl"`
	UsedByUser   *userInvitesUsedByUser `json:"usedByUser"`
}

// userInvitesOptionalISO mirrors `date?.toISOString() ?? null`.
func userInvitesOptionalISO(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02T15:04:05.000Z")
	return &formatted
}

// userInvitesToPayload mirrors toInviteCodePayload.
func userInvitesToPayload(row db.UserInvitesCodeRow) userInvitesPayload {
	var usedByUser *userInvitesUsedByUser
	if row.UserID != nil {
		username := ""
		if row.Username != nil {
			username = *row.Username
		}
		avatarStyle := ""
		if row.AvatarStyle != nil {
			avatarStyle = *row.AvatarStyle
		}
		createdAt := ""
		if row.UserCreatedAt != nil {
			createdAt = row.UserCreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		usedByUser = &userInvitesUsedByUser{
			ID:            *row.UserID,
			Username:      username,
			Email:         row.Email,
			AvatarStyle:   avatarStyle,
			AvatarBadgeID: row.AvatarBadgeID,
			CreatedAt:     createdAt,
		}
	}

	return userInvitesPayload{
		ID:           row.ID,
		Code:         row.Code,
		CreatedBy:    row.CreatedBy,
		UsedBy:       row.UsedBy,
		UsedAt:       userInvitesOptionalISO(row.UsedAt),
		ExpiresAt:    userInvitesOptionalISO(row.ExpiresAt),
		CreatedAt:    row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		CostSnapshot: userInvitesCostSnapshotValue(row.CostSnapshot),
		RegisterURL:  "/register/" + row.Code,
		UsedByUser:   usedByUser,
	}
}

// userInvitesCostSnapshotValue mirrors `costSnapshot ?? null` on the wire.
//
// The bytes are emitted verbatim instead of being decoded into a map: Go maps
// re-serialize keys in sorted order, while Node hands the jsonb column's own
// text (Postgres' canonical jsonb ordering: shorter keys first, then bytewise)
// straight to JSON.stringify. Decoding would silently reorder the object.
func userInvitesCostSnapshotValue(raw []byte) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return json.RawMessage(raw)
}

// ==================== routes ====================

// RegisterUserInvites mounts /api/user-invites. Every route is a regular-user
// route; Fastify registers both the bare and trailing-slash forms of the plugin
// root (prefixTrailingSlash 'both'), so both are mounted here.
func RegisterUserInvites(r *gin.Engine) {
	group := r.Group("/api/user-invites", middleware.AuthenticateUser())

	// GET /summary — pricing quotes, the user's funding, and usage stats.
	group.GET("/summary", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		costOptions, err := db.UserInvitesGetCostQuotes(ctx)
		if err != nil {
			userInvitesInternalError(c)
			return
		}
		balances, err := db.UserInvitesGetBalances(ctx, user.ID)
		if err != nil {
			userInvitesInternalError(c)
			return
		}
		total, used, err := db.UserInvitesCounts(ctx, user.ID)
		if err != nil {
			userInvitesInternalError(c)
			return
		}

		// Math.round((used / total) * 1000) / 10, and 0 when there is nothing yet.
		usageRate := 0.0
		if total > 0 {
			usageRate = math.Round(float64(used)/float64(total)*1000) / 10
		}

		c.JSON(http.StatusOK, gin.H{
			"costOptions": costOptions,
			"balances":    balances,
			"stats": gin.H{
				"total":     total,
				"used":      used,
				"unused":    total - used,
				"usageRate": usageRate,
			},
		})
	})

	// GET / — the caller's invite codes, newest first.
	listHandler := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		page := userInvitesParsePositiveInteger(query.Get("page"), userInvitesDefaultPage)
		pageSize := userInvitesParsePositiveInteger(query.Get("pageSize"), userInvitesDefaultPageSize)
		if pageSize > userInvitesMaxPageSize {
			pageSize = userInvitesMaxPageSize
		}

		result, err := db.UserInvitesListByCreator(c.Request.Context(), user.ID, page, pageSize)
		if err != nil {
			// An overflowing skip offset is a client error in Prisma (→ 500).
			userInvitesInternalError(c)
			return
		}

		invites := make([]userInvitesPayload, 0, len(result.Items))
		for _, row := range result.Items {
			invites = append(invites, userInvitesToPayload(row))
		}

		// Math.max(1, Math.ceil(total / pageSize)).
		totalPages := int64(1)
		if result.Total > 0 {
			totalPages = (result.Total + pageSize - 1) / pageSize
			if totalPages < 1 {
				totalPages = 1
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"invites":    invites,
			"total":      result.Total,
			"page":       page,
			"pageSize":   pageSize,
			"totalPages": totalPages,
		})
	}
	group.GET("", listHandler)
	group.GET("/", listHandler)

	// POST / — generate 1..10 codes, charging the configured cost per code.
	createHandler := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// Fastify validates the body before the handler runs.
		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		validated, validationMessage := validateAJVObject(decoded.Value,
			[]string{"costResource"},
			[]ajvFieldSpec{
				{Name: "costResource", Kind: ajvKindString, MinLength: 1, MaxLength: userInvitesCostResourceMaxLen},
				{Name: "count", Kind: ajvKindInteger, Minimum: 1, Maximum: userInvitesMaxGenerationCount},
			})
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		// The handler reads the *coerced* body, exactly like Fastify replaces
		// request.body with the validated value.
		coercedBody := make(map[string]any, len(validated.Values))
		for key, value := range validated.Values {
			coercedBody[key] = value
		}
		if raw, present := decoded.Value.(map[string]any); present {
			for key, value := range raw {
				if _, overridden := coercedBody[key]; !overridden {
					coercedBody[key] = value
				}
			}
		}

		input, normalizeError := userInvitesNormalizeGenerateBody(coercedBody)
		if normalizeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": normalizeError})
			return
		}

		expiryDays, err := db.UserInvitesGetExpiryDays(ctx)
		if err != nil {
			userInvitesInternalError(c)
			return
		}
		costOptions, err := db.UserInvitesGetCostOptions(ctx)
		if err != nil {
			userInvitesInternalError(c)
			return
		}

		var expiresAt *time.Time
		if expiryDays > 0 {
			value := time.Now().UTC().Add(time.Duration(expiryDays) * 24 * time.Hour)
			expiresAt = &value
		}

		result, err := db.UserInvitesGenerate(ctx, user.ID, input.CostResource, input.Count, expiresAt, costOptions)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrUserInvitesCostOptionUnavailable):
				c.JSON(http.StatusBadRequest, gin.H{
					"error": "当前邀请码生成方式不可用",
					"code":  "INVITE_COST_OPTION_UNAVAILABLE",
				})
			case errors.Is(err, db.ErrUserInvitesCostResourceUnsupported):
				c.JSON(http.StatusBadRequest, gin.H{
					"error": "暂不支持该生成方式",
					"code":  "INVITE_COST_RESOURCE_UNSUPPORTED",
				})
			case errors.Is(err, db.ErrUserInvitesInsufficientBalance):
				c.JSON(http.StatusBadRequest, gin.H{"error": "余额不足", "code": "INSUFFICIENT_BALANCE"})
			case errors.Is(err, db.ErrUserInvitesInsufficientPoints):
				c.JSON(http.StatusBadRequest, gin.H{"error": "积分不足", "code": "INSUFFICIENT_POINTS"})
			case errors.Is(err, db.ErrUserInvitesUserNotFound):
				lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			default:
				userInvitesInternalError(c)
			}
			return
		}

		invites := make([]userInvitesPayload, 0, result.Count)
		for _, row := range result.Invites {
			invites = append(invites, userInvitesToPayload(row))
		}

		c.JSON(http.StatusOK, gin.H{
			"invites": invites,
			"count":   result.Count,
		})
	}
	group.POST("", createHandler)
	group.POST("/", createHandler)
}

// userInvitesInternalError mirrors the generic 500 branch of app.ts' error
// handler (production hides the detail behind "Internal server error").
func userInvitesInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

func init() {
	Register(RegisterUserInvites)
}
