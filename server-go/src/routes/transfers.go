// Port of server/src/routes/transfers.ts (1152 lines, 9 routes).
//
// app.ts registers this module as `fastify.register(transferRoutes, { prefix:
// '/api/transfers' })`, and every route uses `onRequest: [fastify.authenticateUser]`.
// The prefix applies to ALL routes, including the create route, so the real paths
// are:
//
//	GET  /api/transfers/users/search
//	POST /api/transfers/instances/:instanceId/transfer
//	GET  /api/transfers                (+ "/" — prefixTrailingSlash 'both')
//	GET  /api/transfers/pending-count
//	GET  /api/transfers/:id
//	POST /api/transfers/:id/accept
//	POST /api/transfers/:id/reject
//	POST /api/transfers/:id/cancel
//	POST /api/transfers/:id/push
//
// The client (client/src/api/index.ts) calls exactly these paths.
//
// Risk posture (docs/WAVE_D_SPEC.md):
//
//   - accept/push change OWNERSHIP. The idempotency gate is the atomic
//     `pending -> processing` claim (db.ClaimTransferForAccept /
//     db.ClaimTransferForPush): a second accept, a concurrent accept, a cancelled
//     or a rejected transfer all find zero rows and answer 400
//     TRANSFER_NOT_PENDING without touching Incus or money. executeTransfer is
//     deliberately NOT guarded internally — matching the TypeScript, where the
//     claim is the only gate.
//   - the acceptance ORDER is load-bearing: rename in Incus FIRST, then commit the
//     database transaction, then delete the attached resources best-effort. A failed
//     rename leaves the DB untouched (status rolls back to pending); a failed DB
//     commit attempts to rename back and rolls the status back; an attachment
//     cleanup failure is logged and does not fail the transfer.
//   - the fee is charged when the request is CREATED (db.CreateTransferWithFee) and
//     refunded on reject/cancel under the user-balance lock (4110). Accepting never
//     moves money except the instance_billing_records row inside executeTransfer.
//   - notifications and log rows are best-effort: the Node notifier catches
//     internally, and every log write is fire-and-forget.
//
// Incus access is behind package seams (TransfersGetIncusClient,
// TransfersRenameInstance, TransfersRemoveDevice, TransfersDeleteSnapshot,
// TransfersDeleteBackup, TransfersGetCaddyClient, TransfersNanoid) so the state
// machine can be tested without a real node. There is no real-node verification
// for this module (docs/WAVE_D_SPEC.md §4.3).
//
// Known, intentional divergence: if reading the attached resources (proxy sites,
// port mappings, snapshots, backups) fails at the driver level AFTER the ownership
// transaction committed, Node turns the request into a 500 although the transfer
// already happened; this port logs and continues, because the alternative is to
// report failure for a completed ownership change.
package routes

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	// transfersTargetUsernameMaxLength mirrors TRANSFER_TARGET_USERNAME_MAX_LENGTH.
	transfersTargetUsernameMaxLength = 64
	// transfersRemarkMaxLength mirrors TRANSFER_REMARK_MAX_LENGTH (and the body
	// schema's maxLength).
	transfersRemarkMaxLength = 200
	// transfersInstanceNameMaxLength mirrors the `64` in the new-name builder.
	transfersInstanceNameMaxLength = 64
	// transfersMaxSafeInteger mirrors Number.MAX_SAFE_INTEGER, the default `max`
	// of parsePositiveInteger.
	transfersMaxSafeInteger int64 = 1<<53 - 1
	// transfersNanoidLength mirrors the `8` passed to customAlphabet.
	transfersNanoidLength = 8
	// transfersNanoidAlphabet is the TS customAlphabet set: lowercase + digits
	// (Incus rejects underscores).
	transfersNanoidAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	// transfersLogModule is the literal module passed to createLog.
	transfersLogModule = "transfer"
)

// Body schemas, in schema order (AJV allErrors:false reports the first failure).
var (
	transfersCreateFields = []ajvFieldSpec{
		{Name: "targetUsername", Kind: ajvKindString, MinLength: 2, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "remark", Kind: ajvKindString, MinLength: -1, MaxLength: transfersRemarkMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	}
	transfersRejectFields = []ajvFieldSpec{
		{Name: "reason", Kind: ajvKindString, MinLength: -1, MaxLength: transfersRemarkMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	}
)

// ==================== seams ====================

// transfersCaddySiteDeleter is the slice of lib.CaddyClient the cleanup path uses.
type transfersCaddySiteDeleter interface {
	DeleteSite(ctx context.Context, domain string) error
}

var (
	// TransfersGetIncusClient builds the pooled client for a host row. Overridden
	// in tests (no real node exists in CI).
	TransfersGetIncusClient = func(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
		return lib.GetIncusClientForHost(ctx, host)
	}
	// TransfersRenameInstance is lib.RenameIncusInstance.
	TransfersRenameInstance = lib.RenameIncusInstance
	// TransfersRemoveDevice is lib.IncusInstanceRemoveDevice.
	TransfersRemoveDevice = lib.IncusInstanceRemoveDevice
	// TransfersDeleteSnapshot is lib.DeleteSnapshot.
	TransfersDeleteSnapshot = lib.DeleteSnapshot
	// TransfersDeleteBackup is lib.DeleteIncusBackup.
	TransfersDeleteBackup = lib.DeleteIncusBackup
	// TransfersGetCaddyClient mirrors getCaddyClientForHost (sync in the TS).
	TransfersGetCaddyClient = func(host *db.HostsRecord) (transfersCaddySiteDeleter, error) {
		enabled := host.CaddyEnabled
		port := int(host.CaddyPort)
		return lib.GetCaddyClientForHost(lib.CaddyHostRef{
			ID:           host.ID,
			CaddyEnabled: &enabled,
			CaddyPort:    &port,
		}, 0)
	}
	// TransfersNanoid generates the 8-character suffix embedded in the new Incus
	// id. It is a variable so tests can pin the value.
	TransfersNanoid = transfersRandomID
)

// ==================== helpers ====================

// transfersParsePositiveInteger mirrors parsePositiveInteger:
//
//	if (!value) return fallback ?? null
//	const parsed = Number(value)
//	if (!Number.isInteger(parsed) || parsed <= 0 || parsed > max) return fallback ?? null
//
// A nil fallback means "invalid". The Number() grammar (leading zeros, exponents,
// hex literals and trimmed whitespace) is reproduced on purpose: route params in
// the Node implementation are NOT parsed with a strict ^[1-9]\d*$ regex, so
// `/api/transfers/01` resolves to transfer 1 while `/api/transfers/snapshots/01`
// does not. Values outside int4 cannot reach PostgreSQL; they are reported as
// invalid instead of reproducing Prisma's 500.
func transfersParsePositiveInteger(raw string, fallback *int64, max int64) *int64 {
	if raw == "" {
		return fallback
	}
	parsed, ok := transfersJSNumber(raw)
	if !ok || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return fallback
	}
	if math.Trunc(parsed) != parsed || parsed <= 0 || parsed > float64(max) {
		return fallback
	}
	if parsed > float64(math.MaxInt32) {
		// int4 column: Node would hand the value to Prisma and surface a 500. The
		// routes treat it as invalid, which is the only safe answer without
		// duplicating Prisma's error text.
		return nil
	}
	value := int64(parsed)
	return &value
}

// transfersJSNumber lexes the ECMAScript Number() grammar (StringNumericLiteral).
func transfersJSNumber(raw string) (float64, bool) {
	text := transfersJSTrim(raw)
	if text == "" {
		return 0, true
	}

	sign := 1.0
	if text[0] == '+' || text[0] == '-' {
		if text[0] == '-' {
			sign = -1
		}
		text = text[1:]
		if text == "" {
			return 0, false
		}
	}
	if text == "Infinity" {
		return sign * math.Inf(1), true
	}
	// Radix prefixes are only valid without a sign in the ES grammar.
	if sign == 1 && len(text) > 2 && text[0] == '0' {
		base := 0
		switch text[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			value, err := strconv.ParseUint(text[2:], base, 64)
			if err != nil {
				return 0, false
			}
			return float64(value), true
		}
	}

	// DecimalLiteral: digits [ '.' digits? ] [exponent] | '.' digits [exponent]
	seenDigit := false
	seenDot := false
	index := 0
	for index < len(text) && text[index] >= '0' && text[index] <= '9' {
		seenDigit = true
		index++
	}
	if index < len(text) && text[index] == '.' {
		seenDot = true
		index++
		for index < len(text) && text[index] >= '0' && text[index] <= '9' {
			seenDigit = true
			index++
		}
	}
	if !seenDigit {
		return 0, false
	}
	_ = seenDot
	if index < len(text) && (text[index] == 'e' || text[index] == 'E') {
		index++
		if index < len(text) && (text[index] == '+' || text[index] == '-') {
			index++
		}
		expDigits := 0
		for index < len(text) && text[index] >= '0' && text[index] <= '9' {
			index++
			expDigits++
		}
		if expDigits == 0 {
			return 0, false
		}
	}
	if index != len(text) {
		return 0, false
	}

	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return sign * value, true
}

// transfersParseRouteID parses a route param with the TS helper and rejects
// anything that cannot be an int4 primary key.
func transfersParseRouteID(raw string) (int32, bool) {
	parsed := transfersParsePositiveInteger(raw, nil, transfersMaxSafeInteger)
	if parsed == nil {
		return 0, false
	}
	return int32(*parsed), true
}

// transfersQuerySingle returns the single value of a query key. Fastify gives an
// array for a repeated key; the TypeScript helpers would then see an array, so a
// repeated key is treated as absent (Number(array) is NaN in the common cases).
func transfersQuerySingle(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// transfersCreateInput mirrors TransferCreateInput.
type transfersCreateInput struct {
	TargetUsername string
	Remark         *string
}

// transfersNormalizeCreateInput mirrors normalizeTransferCreateInput. The returned
// string is the `{ error: ... }` payload the TypeScript sends (NOT an apiError
// envelope — that is why the handler writes gin.H{"error": message}).
func transfersNormalizeCreateInput(input any) (transfersCreateInput, string) {
	record, ok := input.(map[string]any)
	if !ok {
		return transfersCreateInput{}, "Invalid request body"
	}

	rawUsername, present := record["targetUsername"]
	username, isString := rawUsername.(string)
	if !present || !isString {
		return transfersCreateInput{}, "Target username is required"
	}
	username = transfersJSTrim(username)
	if length := transfersJSStringLength(username); length < 2 || length > transfersTargetUsernameMaxLength {
		return transfersCreateInput{}, "Target username must be 2-" +
			strconv.Itoa(transfersTargetUsernameMaxLength) + " characters"
	}

	var remark *string
	if rawRemark, remarkPresent := record["remark"]; remarkPresent && rawRemark != nil {
		remarkText, remarkIsString := rawRemark.(string)
		if !remarkIsString {
			return transfersCreateInput{}, "Remark must be a string"
		}
		normalized := transfersJSTrim(remarkText)
		if transfersJSStringLength(normalized) > transfersRemarkMaxLength {
			return transfersCreateInput{}, "Remark must be no longer than " +
				strconv.Itoa(transfersRemarkMaxLength) + " characters"
		}
		if normalized != "" {
			remark = &normalized
		}
	}

	return transfersCreateInput{TargetUsername: username, Remark: remark}, ""
}

// transfersJSStringLength mirrors String.prototype.length (UTF-16 code units).
func transfersJSStringLength(value string) int {
	units := 0
	for _, char := range value {
		if char > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// transfersJSTrim mirrors String.prototype.trim.
func transfersJSTrim(value string) string {
	return strings.TrimFunc(value, func(char rune) bool {
		if char == '\uFEFF' {
			return true
		}
		return char != '\u0085' && unicode.IsSpace(char)
	})
}

// transfersJSSubstring mirrors String.prototype.substring(0, end) for a
// non-negative end (the only form the name builder uses).
func transfersJSSubstring(value string, end int) string {
	if end < 0 {
		end = 0
	}
	units := utf16.Encode([]rune(value))
	if end > len(units) {
		end = len(units)
	}
	return string(utf16.Decode(units[:end]))
}

// transfersTransferredName mirrors the name builder shared by accept and push:
//
//	if (!name.endsWith(suffix)) {
//	    name = name + suffix
//	    if (name.length > 64) name = original.substring(0, 64 - suffix.length) + suffix
//	}
func transfersTransferredName(original, suffix string) string {
	if strings.HasSuffix(original, suffix) {
		return original
	}
	combined := original + suffix
	if transfersJSStringLength(combined) <= transfersInstanceNameMaxLength {
		return combined
	}
	limit := transfersInstanceNameMaxLength - transfersJSStringLength(suffix)
	return transfersJSSubstring(original, limit) + suffix
}

// transfersRandomID mirrors the custom nanoid: 8 characters from
// [a-z0-9]. crypto/rand with rejection sampling (252 = 36*7) so every character
// is uniform; a failure of the system CSPRNG aborts the request rather than
// silently producing a predictable id.
func transfersRandomID() string {
	output := make([]byte, transfersNanoidLength)
	buffer := make([]byte, 1)
	for index := range output {
		for {
			if _, err := rand.Read(buffer); err != nil {
				return ""
			}
			if int(buffer[0]) < 252 {
				output[index] = transfersNanoidAlphabet[int(buffer[0])%len(transfersNanoidAlphabet)]
				break
			}
		}
	}
	return string(output)
}

// transfersISOTime mirrors Date#toISOString for a timestamp(3) value.
func transfersISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// transfersISONullable renders an optional Date, null when absent.
func transfersISONullable(value *time.Time) *string {
	if value == nil {
		return nil
	}
	rendered := transfersISOTime(*value)
	return &rendered
}

// transfersUserViewFrom mirrors the explicit user projection the route builds
// (`avatarBadgeId ?? null`, never undefined).
func transfersUserViewFrom(reference *db.TransferUserRef) *transfersUserView {
	if reference == nil {
		return nil
	}
	return &transfersUserView{
		ID:            reference.ID,
		Username:      reference.Username,
		Email:         reference.Email,
		AvatarStyle:   reference.AvatarStyle,
		AvatarBadgeID: reference.AvatarBadgeID,
	}
}

// transfersLog mirrors createLog(userId, 'transfer', action, content, result,
// { instanceId }); a write failure is never allowed to affect the transfer.
func transfersLog(ctx context.Context, userID, instanceID int32, action, content, result string) {
	uid := int64(userID)
	db.CreateLog(ctx, &uid, transfersLogModule, action, content, result, db.LogOptionsWithInstance(&instanceID))
}

// transfersNotify mirrors sendNotification; the Go notifier returns a result
// instead of throwing, so a partial failure is logged only.
func transfersNotify(ctx context.Context, userID int32, event lib.EventType, data lib.EventData) {
	result := lib.SendNotification(ctx, int64(userID), event, data)
	if result.Failed > 0 {
		log.Printf("[transfer] notification %s partially failed: sent=%d failed=%d",
			event, result.Sent, result.Failed)
	}
}

// transfersRequireVerifiedOperation mirrors requireVerifiedOperation: the verified
// record is CLAIMED (deleted) by the helper, so one verification authorizes exactly
// one transfer creation.
func transfersRequireVerifiedOperation(c *gin.Context, userID, instanceID int32) bool {
	verification, err := lib.ClaimOperationVerificationRequirement(
		c.Request.Context(), int64(userID), models.OperationTypeTransferInstance, int64(instanceID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return false
	}
	if !verification.Verified {
		c.JSON(http.StatusForbidden, gin.H{
			"error":    "Sensitive operation requires verification",
			"code":     "VERIFICATION_REQUIRED",
			"required": verification.Required,
		})
		return false
	}
	return true
}

// transfersRollbackDetached runs the status rollback even when the client has
// already disconnected; Node's Prisma call is not bound to the request either.
func transfersRollbackDetached(ctx context.Context, transferID int32) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	transfersRollback(rollbackCtx, transferID)
}

// ==================== view types ====================

type transfersUserView struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

// transfersListView is the GET / item shape. `canPush` only exists on the list.
type transfersListView struct {
	ID             int32              `json:"id"`
	InstanceID     int32              `json:"instanceId"`
	InstanceName   string             `json:"instanceName"`
	InstanceStatus string             `json:"instanceStatus"`
	InstanceImage  string             `json:"instanceImage"`
	FromUser       *transfersUserView `json:"fromUser"`
	ToUser         *transfersUserView `json:"toUser"`
	Status         string             `json:"status"`
	Snapshot       json.RawMessage    `json:"snapshot"`
	Remark         *string            `json:"remark"`
	RejectReason   *string            `json:"rejectReason"`
	CreatedAt      string             `json:"createdAt"`
	AcceptedAt     *string            `json:"acceptedAt"`
	RejectedAt     *string            `json:"rejectedAt"`
	CancelledAt    *string            `json:"cancelledAt"`
	CanPush        bool               `json:"canPush"`
}

// transfersDetailView is the GET /:id shape (no canPush, no instanceImage).
type transfersDetailView struct {
	ID             int32              `json:"id"`
	InstanceID     int32              `json:"instanceId"`
	InstanceName   string             `json:"instanceName"`
	InstanceStatus string             `json:"instanceStatus"`
	FromUser       *transfersUserView `json:"fromUser"`
	ToUser         *transfersUserView `json:"toUser"`
	Status         string             `json:"status"`
	Snapshot       json.RawMessage    `json:"snapshot"`
	Remark         *string            `json:"remark"`
	RejectReason   *string            `json:"rejectReason"`
	CreatedAt      string             `json:"createdAt"`
	AcceptedAt     *string            `json:"acceptedAt"`
	RejectedAt     *string            `json:"rejectedAt"`
	CancelledAt    *string            `json:"cancelledAt"`
}

// ==================== registration ====================

// RegisterTransfers mounts /api/transfers. Every route is user-only
// (authenticateUser), so admins get 403 USER_ONLY before any handler runs.
func RegisterTransfers(r *gin.Engine) {
	group := r.Group("/api/transfers", middleware.AuthenticateUser())
	transfersMount(group)
}

// transfersMount is separate from RegisterTransfers so tests can mount the routes
// behind a fake auth middleware.
func transfersMount(group *gin.RouterGroup) {
	group.GET("/users/search", transfersSearchUsers)
	group.POST("/instances/:instanceId/transfer", transfersCreate)
	// Fastify's prefixTrailingSlash default ('both') registers the plugin root as
	// both /api/transfers and /api/transfers/.
	group.GET("", transfersList)
	group.GET("/", transfersList)
	group.GET("/pending-count", transfersPendingCount)
	group.GET("/:id", transfersDetail)
	group.POST("/:id/accept", transfersAccept)
	group.POST("/:id/reject", transfersReject)
	group.POST("/:id/cancel", transfersCancel)
	group.POST("/:id/push", transfersPush)
}

func init() { Register(RegisterTransfers) }

// ==================== GET /users/search ====================

// transfersSearchUsers mirrors `GET /users/search`.
func transfersSearchUsers(c *gin.Context) {
	user := middleware.CurrentUser(c)
	values := c.Request.URL.Query()["username"]
	username, ok := transfersQuerySingle(values)
	if !ok || username == "" || transfersJSStringLength(username) < 2 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			"Username must be at least 2 characters")
		return
	}

	target, err := db.FindUserByUsername(c.Request.Context(), username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if target == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}
	if target.ID == user.ID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferToSelf, "")
		return
	}
	if target.Status == models.UserStatusBanned {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferToBanned, "")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": gin.H{
			"id":       target.ID,
			"username": target.Username,
			"status":   target.Status,
		},
	})
}

// ==================== POST /instances/:instanceId/transfer ====================

// transfersCreate mirrors `POST /instances/:instanceId/transfer`.
//
// Order is the TypeScript's and is observable:
// body (schema -> normalize) -> id -> instance 404 -> owner 403 -> status 400 ->
// suspended 403 -> host disabled 400 -> pending 400 -> verification 403 ->
// target 404/400 -> balance 400 -> create.
func transfersCreate(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	validated, validationMessage := validateAJVObject(decoded.Value, []string{"targetUsername"}, transfersCreateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	// Fastify's AJV mutates request.body with the coerced values (coerceTypes), so
	// the handler's normalize step sees strings, not the original JSON types.
	coercedBody := make(map[string]any, len(validated.Values))
	for name, value := range validated.Values {
		coercedBody[name] = value
	}

	// parsePositiveInteger(request.params.instanceId) runs before the id check but
	// AFTER the body normalization in the TypeScript; both are 400s, and the
	// normalize error wins when both are invalid.
	instanceID, instanceIDValid := transfersParseRouteID(c.Param("instanceId"))
	createInput, normalizeError := transfersNormalizeCreateInput(coercedBody)
	if normalizeError != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": normalizeError})
		return
	}
	if !instanceIDValid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusCreating || instance.Status == models.InstanceStatusDeleted {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferInvalidStatus, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if host != nil && !host.TransferEnabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferHostDisabled, "")
		return
	}

	pending, err := db.HasPendingTransfer(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if pending {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferAlreadyPending, "")
		return
	}

	if !transfersRequireVerifiedOperation(c, user.ID, instanceID) {
		return
	}

	targetUser, err := db.FindUserByUsername(ctx, createInput.TargetUsername)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if targetUser == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}
	if targetUser.ID == instance.UserID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferToSelf, "")
		return
	}
	if targetUser.Status == models.UserStatusBanned {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferToBanned, "")
		return
	}

	// Snapshot inputs. Every read failure becomes a 500 (the TypeScript would throw).
	var packageName *string
	if instance.PackageID != nil {
		packageName, err = db.TransferPackageNameByID(ctx, *instance.PackageID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
			return
		}
		// `pkg?.name || null`: an empty name becomes null, not "".
		if packageName != nil && *packageName == "" {
			packageName = nil
		}
	}
	portMappings, err := db.PortMappingsList(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	snapshots, err := db.SnapshotsListByInstanceID(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	backups, err := db.GetBackupsByInstanceID(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}

	transferFee, err := db.GetSystemConfigFloat(ctx, "transfer_fee", 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}

	if transferFee > 0 {
		balance, balanceErr := db.GetUserBalance(ctx, user.ID)
		if balanceErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
			return
		}
		if balance < transferFee {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferInsufficientBalance, "")
			return
		}
	}

	backupCount := 0
	for _, backup := range backups {
		if backup.Status != "deleted" {
			backupCount++
		}
	}

	hostName := "unknown"
	hostCountryCode := "us"
	var hostLocation *string
	if host != nil {
		if host.Name != "" {
			hostName = host.Name
		}
		if host.CountryCode != "" {
			hostCountryCode = host.CountryCode
		}
		hostLocation = host.Location
		if hostLocation != nil && *hostLocation == "" {
			// `host?.location || null`: an empty string becomes null.
			hostLocation = nil
		}
	}

	snapshot := db.TransferSnapshot{
		PackageID:         instance.PackageID,
		PackageName:       packageName,
		HostID:            instance.HostID,
		HostName:          hostName,
		HostLocation:      hostLocation,
		HostCountryCode:   hostCountryCode,
		OriginalName:      instance.Name,
		CPU:               instance.CPU,
		Memory:            instance.Memory,
		Disk:              instance.Disk,
		NetworkMode:       string(instance.NetworkMode),
		PortMappingsCount: len(portMappings),
		SnapshotsCount:    len(snapshots),
		BackupsCount:      backupCount,
		IPv4:              instance.IPv4,
		IPv6:              instance.IPv6,
	}

	result, createErr := db.CreateTransferWithFee(ctx, db.TransferCreateWithFeeInput{
		InstanceID:   instanceID,
		FromUserID:   user.ID,
		ToUserID:     targetUser.ID,
		Snapshot:     snapshot,
		Remark:       createInput.Remark,
		Fee:          transferFee,
		InstanceName: instance.Name,
	})
	switch {
	case createErr == nil:
		// continue
	case errors.Is(createErr, db.ErrTransferInsufficientBalance):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferInsufficientBalance, "")
		return
	case errors.Is(createErr, db.ErrTransferBalanceConflict):
		lib.RespondAPIError(c, http.StatusConflict, lib.ErrTransferInsufficientBalance, "余额正在处理，请稍后重试")
		return
	case errors.Is(createErr, db.ErrTransferAlreadyPending):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferAlreadyPending, "")
		return
	default:
		log.Printf("[Transfer] Failed to create transfer: %v", createErr)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, "")
		return
	}

	transfersLog(ctx, user.ID, instanceID, "transfer.create",
		"Created transfer request for instance \""+instance.Name+"\" to user \""+targetUser.Username+"\"",
		db.LogResultSuccess)

	transfersNotify(c.Request.Context(), targetUser.ID, lib.EventTransferReceived, lib.EventData{
		"instanceName":   instance.Name,
		"senderUsername": user.Username,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Transfer request created",
		"transfer": gin.H{
			"id":     result.TransferID,
			"status": string(models.TransferStatusPending),
		},
	})
}

// ==================== GET / ====================

// transfersList mirrors `GET /`.
func transfersList(c *gin.Context) {
	user := middleware.CurrentUser(c)
	query := c.Request.URL.Query()

	typeValues := query["type"]
	listType, typeOK := transfersQuerySingle(typeValues)
	if !typeOK || (listType != "sent" && listType != "received") {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			`type must be "sent" or "received"`)
		return
	}

	page := 1
	if raw, ok := transfersQuerySingle(query["page"]); ok {
		if parsed := transfersParsePositiveInteger(raw, nil, transfersMaxSafeInteger); parsed != nil {
			if *parsed > math.MaxInt32 {
				page = math.MaxInt32
			} else {
				page = int(*parsed)
			}
		}
	}
	pageSize := 20
	if raw, ok := transfersQuerySingle(query["pageSize"]); ok {
		if parsed := transfersParsePositiveInteger(raw, nil, 100); parsed != nil {
			pageSize = int(*parsed)
		}
	}

	options := db.TransferListOptions{
		Page:     page,
		PageSize: pageSize,
		Status:   query.Get("status"),
		Search:   query.Get("search"),
	}

	var result db.TransferListResult
	var err error
	if listType == "sent" {
		result, err = db.GetTransfersSent(c.Request.Context(), user.ID, options)
	} else {
		result, err = db.GetTransfersReceived(c.Request.Context(), user.ID, options)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}

	transferViews := make([]transfersListView, 0, len(result.Items))
	for _, transfer := range result.Items {
		instanceName := ""
		instanceStatus := ""
		instanceImage := ""
		if transfer.Instance != nil {
			instanceName = transfer.Instance.Name
			instanceStatus = string(transfer.Instance.Status)
			instanceImage = transfer.Instance.Image
		}
		// Prisma's `include` only loads the counterpart the list needs, so a sent
		// row has fromUser: null and a received row has toUser: null.
		var fromUser, toUser *transfersUserView
		if listType == "sent" {
			toUser = transfersUserViewFrom(transfer.ToUser)
		} else {
			fromUser = transfersUserViewFrom(transfer.FromUser)
		}
		transferViews = append(transferViews, transfersListView{
			ID:             transfer.ID,
			InstanceID:     transfer.InstanceID,
			InstanceName:   instanceName,
			InstanceStatus: instanceStatus,
			InstanceImage:  instanceImage,
			FromUser:       fromUser,
			ToUser:         toUser,
			Status:         string(transfer.Status),
			Snapshot:       transfer.Snapshot,
			Remark:         transfer.Remark,
			RejectReason:   transfer.RejectReason,
			CreatedAt:      transfersISOTime(transfer.CreatedAt),
			AcceptedAt:     transfersISONullable(transfer.AcceptedAt),
			RejectedAt:     transfersISONullable(transfer.RejectedAt),
			CancelledAt:    transfersISONullable(transfer.CancelledAt),
			CanPush: listType == "sent" && transfer.Status == models.TransferStatusPending &&
				transfer.FromUserID == user.ID,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"transfers":  transferViews,
		"total":      result.Total,
		"page":       result.Page,
		"pageSize":   result.PageSize,
		"totalPages": result.TotalPages,
	})
}

// ==================== GET /pending-count ====================

func transfersPendingCount(c *gin.Context) {
	user := middleware.CurrentUser(c)
	count, err := db.GetPendingReceivedCount(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"count": count})
}

// ==================== GET /:id ====================

func transfersDetail(c *gin.Context) {
	user := middleware.CurrentUser(c)
	transferID, ok := transfersParseRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	transfer, err := db.GetTransferByID(c.Request.Context(), transferID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if transfer == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrTransferNotFound, "")
		return
	}
	// Admins manage the platform, not individual transfers: only the two parties
	// may read one.
	if transfer.FromUserID != user.ID && transfer.ToUserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instanceName := ""
	instanceStatus := ""
	if transfer.Instance != nil {
		instanceName = transfer.Instance.Name
		instanceStatus = string(transfer.Instance.Status)
	}

	c.JSON(http.StatusOK, gin.H{
		"transfer": transfersDetailView{
			ID:             transfer.ID,
			InstanceID:     transfer.InstanceID,
			InstanceName:   instanceName,
			InstanceStatus: instanceStatus,
			FromUser:       transfersUserViewFrom(transfer.FromUser),
			ToUser:         transfersUserViewFrom(transfer.ToUser),
			Status:         string(transfer.Status),
			Snapshot:       transfer.Snapshot,
			Remark:         transfer.Remark,
			RejectReason:   transfer.RejectReason,
			CreatedAt:      transfersISOTime(transfer.CreatedAt),
			AcceptedAt:     transfersISONullable(transfer.AcceptedAt),
			RejectedAt:     transfersISONullable(transfer.RejectedAt),
			CancelledAt:    transfersISONullable(transfer.CancelledAt),
		},
	})
}

// ==================== POST /:id/accept ====================

// transfersAccept mirrors `POST /:id/accept`.
func transfersAccept(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	transferID, ok := transfersParseRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// Atomic optimistic lock: only a pending transfer addressed to this user moves
	// to processing. Two concurrent accepts cannot both win this UPDATE.
	claimed, err := db.ClaimTransferForAccept(ctx, transferID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if claimed == 0 {
		transfersClaimFailure(c, transferID, user.ID, true)
		return
	}

	transfer, err := db.GetTransferByID(ctx, transferID)
	if err != nil {
		transfersRollbackDetached(ctx, transferID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if transfer == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrTransferNotFound, "")
		return
	}

	rollback := func() { transfersRollbackDetached(ctx, transferID) }

	instance, err := db.GetInstanceById(ctx, transfer.InstanceID)
	if err != nil {
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if instance == nil || instance.Status == models.InstanceStatusDeleted {
		if cancelErr := db.CancelTransfer(ctx, transferID); cancelErr != nil {
			log.Printf("[Transfer] Failed to cancel transfer: %v", cancelErr)
		}
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.Status == models.InstanceStatusRunning {
		rollback()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Instance must be stopped before transfer",
			"code":  "INSTANCE_MUST_BE_STOPPED",
		})
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	var client *lib.IncusClient
	if host != nil {
		client, err = TransfersGetIncusClient(ctx, host)
		if err != nil {
			log.Printf("[Transfer] Failed to get Incus client: %v", err)
			client = nil
		}
	}
	if client == nil {
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Cannot connect to host",
			"code":  "HOST_UNAVAILABLE",
		})
		return
	}

	// 1. Rename first: the critical operation. A failure here leaves every attached
	// resource untouched and the DB unchanged.
	oldIncusID := instance.IncusID
	newIncusID := "u" + strconv.FormatInt(int64(user.ID), 10) + "-" + TransfersNanoid()
	if renameErr := TransfersRenameInstance(ctx, client, oldIncusID, newIncusID); renameErr != nil {
		log.Printf("[Transfer] Failed to rename instance in Incus: %v", renameErr)
		transfersLog(ctx, user.ID, instance.ID, "transfer.accept",
			"Failed to rename instance \""+instance.Name+"\" in Incus: "+renameErr.Error(),
			db.LogResultFailed)
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to rename instance",
			"code":  "INCUS_RENAME_FAILED",
		})
		return
	}
	log.Printf("[Transfer] Renamed instance from %s to %s", oldIncusID, newIncusID)

	// 2. Database ownership change BEFORE deleting attached resources.
	suffix := "（" + transfersUsername(transfer.FromUser) + "转）"
	newName := transfersTransferredName(instance.Name, suffix)
	if dbErr := db.ExecuteTransfer(ctx, transferID, transfer.InstanceID, transfer.FromUserID,
		transfer.ToUserID, newName, newIncusID); dbErr != nil {
		log.Printf("[Transfer] Database update failed: %v", dbErr)
		transfersRollbackIncusRename(client, oldIncusID, newIncusID)
		transfersLog(ctx, user.ID, instance.ID, "transfer.accept",
			"Failed to update database for instance \""+instance.Name+"\": "+dbErr.Error(),
			db.LogResultFailed)
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to update database",
			"code":  "DATABASE_ERROR",
		})
		return
	}

	// 3. Attached resources are dropped best-effort; the ownership change stands.
	transfersCleanupAttachments(ctx, client, host, transfer.InstanceID, newIncusID)

	transfersLog(ctx, user.ID, transfer.InstanceID, "transfer.accept",
		"Accepted transfer of instance \""+instance.Name+"\" from \""+transfersUsername(transfer.FromUser)+"\"",
		db.LogResultSuccess)

	transfersNotify(ctx, transfer.FromUserID, lib.EventTransferAccepted, lib.EventData{
		"instanceName":     instance.Name,
		"receiverUsername": user.Username,
	})
	// The new owner must reset credentials themselves; a normal transfer does not
	// reinstall the instance.
	transfersNotify(ctx, user.ID, lib.EventTransferOwnershipReceived, lib.EventData{
		"instanceName": newName,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Transfer accepted"})
}

// ==================== POST /:id/reject ====================

func transfersReject(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, nil, transfersRejectFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	transferID, ok := transfersParseRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	var reason *string
	if value, present := body.stringValue("reason"); present && value != "" {
		reason = &value
	}

	rejected, err := db.RejectPendingTransfer(ctx, transferID, user.ID, reason)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if rejected == 0 {
		transfersClaimFailure(c, transferID, user.ID, true)
		return
	}

	transfer, err := db.GetTransferByIDWithFee(ctx, transferID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if transfer == nil {
		// The status write succeeded but the re-read failed: report success.
		c.JSON(http.StatusOK, gin.H{"message": "Transfer rejected"})
		return
	}

	if transfer.Fee != nil && transfer.Fee.IsPositive() {
		success, refundError := db.RefundTransferFee(ctx, db.TransferRefundInput{
			TransferID:   transfer.ID,
			FromUserID:   transfer.FromUserID,
			Fee:          transfer.Fee.InexactFloat64(),
			InstanceID:   transfer.InstanceID,
			InstanceName: transfersInstanceName(transfer),
			Reason:       db.TransferRefundReasonRejected,
		})
		if !success {
			log.Printf("[Transfer] Failed to refund transfer fee: %s", refundError)
		}
	}

	content := "Rejected transfer of instance \"" + transfersInstanceName(transfer) +
		"\" from \"" + transfersUsername(transfer.FromUser) + "\""
	if reason != nil {
		content += ": " + *reason
	}
	transfersLog(ctx, user.ID, transfer.InstanceID, "transfer.reject", content, db.LogResultSuccess)

	transfersNotify(ctx, transfer.FromUserID, lib.EventTransferRejected, lib.EventData{
		"instanceName":     transfersInstanceName(transfer),
		"receiverUsername": user.Username,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Transfer rejected"})
}

// ==================== POST /:id/cancel ====================

func transfersCancel(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	transferID, ok := transfersParseRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	cancelled, err := db.CancelPendingTransfer(ctx, transferID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if cancelled == 0 {
		transfersClaimFailure(c, transferID, user.ID, false)
		return
	}

	transfer, err := db.GetTransferByIDWithFee(ctx, transferID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if transfer == nil {
		c.JSON(http.StatusOK, gin.H{"message": "Transfer cancelled"})
		return
	}

	if transfer.Fee != nil && transfer.Fee.IsPositive() {
		success, refundError := db.RefundTransferFee(ctx, db.TransferRefundInput{
			TransferID:   transfer.ID,
			FromUserID:   transfer.FromUserID,
			Fee:          transfer.Fee.InexactFloat64(),
			InstanceID:   transfer.InstanceID,
			InstanceName: transfersInstanceName(transfer),
			Reason:       db.TransferRefundReasonCancelled,
		})
		if !success {
			log.Printf("[Transfer] Failed to refund transfer fee on cancel: %s", refundError)
		}
	}

	transfersLog(ctx, user.ID, transfer.InstanceID, "transfer.cancel",
		"Cancelled transfer of instance \""+transfersInstanceName(transfer)+
			"\" to \""+transfersUsername(transfer.ToUser)+"\"",
		db.LogResultSuccess)

	transfersNotify(ctx, transfer.ToUserID, lib.EventTransferCancelled, lib.EventData{
		"instanceName":   transfersInstanceName(transfer),
		"senderUsername": user.Username,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Transfer cancelled"})
}

// ==================== POST /:id/push ====================

func transfersPush(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	transferID, ok := transfersParseRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	claimed, err := db.ClaimTransferForPush(ctx, transferID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if claimed == 0 {
		transfersClaimFailure(c, transferID, user.ID, false)
		return
	}

	transfer, err := db.GetTransferByID(ctx, transferID)
	if err != nil {
		transfersRollbackDetached(ctx, transferID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if transfer == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrTransferNotFound, "")
		return
	}

	rollback := func() { transfersRollbackDetached(ctx, transferID) }

	instance, err := db.GetInstanceById(ctx, transfer.InstanceID)
	if err != nil {
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if instance == nil || instance.Status == models.InstanceStatusDeleted {
		if cancelErr := db.CancelTransfer(ctx, transferID); cancelErr != nil {
			log.Printf("[Transfer] Failed to cancel transfer: %v", cancelErr)
		}
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceNotFound, "")
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if host == nil {
		rollback()
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	if instance.Status == models.InstanceStatusRunning {
		rollback()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Instance must be stopped before transfer",
			"code":  "INSTANCE_MUST_BE_STOPPED",
		})
		return
	}

	client, err := TransfersGetIncusClient(ctx, host)
	if err != nil {
		log.Printf("[Transfer] Failed to get Incus client: %v", err)
		client = nil
	}
	if client == nil {
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Cannot connect to host",
			"code":  "HOST_UNAVAILABLE",
		})
		return
	}

	oldIncusID := instance.IncusID
	newIncusID := "u" + strconv.FormatInt(int64(transfer.ToUserID), 10) + "-" + TransfersNanoid()
	if renameErr := TransfersRenameInstance(ctx, client, oldIncusID, newIncusID); renameErr != nil {
		log.Printf("[Transfer] Failed to rename instance in Incus: %v", renameErr)
		transfersLog(ctx, user.ID, instance.ID, "transfer.push",
			"Failed to rename instance \""+instance.Name+"\" in Incus: "+renameErr.Error(),
			db.LogResultFailed)
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to rename instance",
			"code":  "INCUS_RENAME_FAILED",
		})
		return
	}
	log.Printf("[Transfer] Renamed instance from %s to %s", oldIncusID, newIncusID)

	// The push suffix names the INITIATOR (the host owner who pushed), not the
	// transfer's fromUser — the TypeScript uses `user.username` here.
	suffix := "（" + user.Username + "转）"
	newName := transfersTransferredName(instance.Name, suffix)
	if dbErr := db.ExecuteTransfer(ctx, transferID, transfer.InstanceID, transfer.FromUserID,
		transfer.ToUserID, newName, newIncusID); dbErr != nil {
		log.Printf("[Transfer] Database update failed: %v", dbErr)
		transfersRollbackIncusRename(client, oldIncusID, newIncusID)
		transfersLog(ctx, user.ID, instance.ID, "transfer.push",
			"Failed to update database for instance \""+instance.Name+"\": "+dbErr.Error(),
			db.LogResultFailed)
		rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to update database",
			"code":  "DATABASE_ERROR",
		})
		return
	}

	transfersCleanupAttachments(ctx, client, host, transfer.InstanceID, newIncusID)

	transfersLog(ctx, user.ID, transfer.InstanceID, "transfer.push",
		"Pushed instance \""+instance.Name+"\" to user \""+transfersUsername(transfer.ToUser)+"\"",
		db.LogResultSuccess)

	transfersNotify(ctx, transfer.ToUserID, lib.EventTransferOwnershipReceived, lib.EventData{
		"instanceName": newName,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Transfer pushed successfully"})
}

// ==================== shared route plumbing ====================

// transfersClaimFailure mirrors the three-way discrimination after a zero-row
// claim: 404 when the transfer does not exist, 403 when the caller is not the
// required party, 400 TRANSFER_NOT_PENDING otherwise.
func transfersClaimFailure(c *gin.Context, transferID, userID int32, recipient bool) {
	transfer, err := db.GetTransferByID(c.Request.Context(), transferID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error", "code": "INTERNAL_ERROR"})
		return
	}
	if transfer == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrTransferNotFound, "")
		return
	}
	requiredParty := transfer.FromUserID
	if recipient {
		requiredParty = transfer.ToUserID
	}
	if requiredParty != userID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferNotPending, "")
}

// transfersRollback mirrors rollbackToPending: log and continue on failure.
func transfersRollback(ctx context.Context, transferID int32) {
	if err := db.RollbackTransferToPending(ctx, transferID); err != nil {
		log.Printf("[Transfer] Failed to rollback status: %v", err)
	}
}

// transfersRollbackIncusRename mirrors the best-effort rollback of a rename after
// the database transaction failed. A failure here needs human intervention and is
// logged as CRITICAL, exactly like the TypeScript.
func transfersRollbackIncusRename(client *lib.IncusClient, oldIncusID, newIncusID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := TransfersRenameInstance(ctx, client, newIncusID, oldIncusID); err != nil {
		log.Printf("[Transfer] CRITICAL: Failed to rollback Incus rename! old=%s new=%s error=%v",
			oldIncusID, newIncusID, err)
		return
	}
	log.Printf("[Transfer] Rolled back Incus rename from %s to %s", newIncusID, oldIncusID)
}

// transfersUsername mirrors `transfer.fromUser.username` / `toUser.username`.
func transfersUsername(reference *db.TransferUserRef) string {
	if reference == nil {
		return ""
	}
	return reference.Username
}

// transfersInstanceName mirrors `transfer.instance?.name || 'Unknown'`.
func transfersInstanceName(transfer *db.TransferRecord) string {
	if transfer.Instance == nil || transfer.Instance.Name == "" {
		return "Unknown"
	}
	return transfer.Instance.Name
}

// transfersCleanupAttachments mirrors steps 3.1 - 3.5 of accept/push.
//
// Every remote and database failure is logged and swallowed: the ownership change
// already committed, and a stale proxy site or snapshot must not turn it into an
// error. The one ordering rule kept from the TypeScript: the Caddy site is removed
// before its DB row, and each port mapping/snapshot/backup is removed remotely
// before its row.
func transfersCleanupAttachments(
	ctx context.Context,
	client *lib.IncusClient,
	host *db.HostsRecord,
	instanceID int32,
	incusID string,
) {
	// 3.1 Reverse-proxy sites (Caddy first, then the rows).
	proxySites, err := db.ProxySitesByInstanceID(ctx, instanceID)
	if err != nil {
		log.Printf("[Transfer] Failed to list proxy sites for instance %d: %v", instanceID, err)
	} else if len(proxySites) > 0 {
		if host != nil && host.CaddyEnabled {
			caddyClient, caddyErr := TransfersGetCaddyClient(host)
			if caddyErr != nil {
				log.Printf("[Transfer] Failed to get Caddy client: %v", caddyErr)
			} else if caddyClient != nil {
				for _, site := range proxySites {
					if deleteErr := caddyClient.DeleteSite(ctx, site.Domain); deleteErr != nil {
						log.Printf("[Transfer] Failed to delete Caddy site (%s): %v", site.Domain, deleteErr)
					}
				}
			}
		}
		for _, site := range proxySites {
			if _, deleteErr := db.ProxySitesDelete(ctx, site.ID); deleteErr != nil {
				log.Printf("[Transfer] Failed to delete proxy site DB record (%s): %v", site.Domain, deleteErr)
			}
		}
	}

	// 3.2 Port mappings. The instance was renamed, so the Incus device lives on the
	// NEW incus id.
	portMappings, err := db.PortMappingsList(ctx, instanceID)
	if err != nil {
		log.Printf("[Transfer] Failed to list port mappings for instance %d: %v", instanceID, err)
	} else if len(portMappings) > 0 {
		for _, mapping := range portMappings {
			deviceName := "proxy-" + string(mapping.Protocol) + "-" +
				strconv.FormatInt(int64(mapping.PublicPort), 10)
			if _, removeErr := TransfersRemoveDevice(ctx, client, incusID, deviceName); removeErr != nil {
				log.Printf("[Transfer] Failed to remove port mapping device (%s): %v", deviceName, removeErr)
			}
			if deleteErr := db.PortMappingsDelete(ctx, mapping.ID); deleteErr != nil {
				log.Printf("[Transfer] Failed to delete port mapping DB record (%d): %v", mapping.ID, deleteErr)
			}
		}
	}

	// 3.3 Snapshots.
	snapshots, err := db.SnapshotsListByInstanceID(ctx, instanceID)
	if err != nil {
		log.Printf("[Transfer] Failed to list snapshots for instance %d: %v", instanceID, err)
	} else if len(snapshots) > 0 {
		for _, snapshot := range snapshots {
			if _, deleteErr := TransfersDeleteSnapshot(ctx, client, incusID, snapshot.IncusName); deleteErr != nil {
				log.Printf("[Transfer] Failed to delete snapshot (%s): %v", snapshot.Name, deleteErr)
			}
			if deleteErr := db.SnapshotsDelete(ctx, snapshot.ID); deleteErr != nil {
				log.Printf("[Transfer] Failed to delete snapshot DB record (%d): %v", snapshot.ID, deleteErr)
			}
		}
	}

	// 3.4 Backups (soft delete on the DB side).
	backups, err := db.GetBackupsByInstanceID(ctx, instanceID)
	if err != nil {
		log.Printf("[Transfer] Failed to list backups for instance %d: %v", instanceID, err)
	} else if len(backups) > 0 {
		for _, backup := range backups {
			if _, deleteErr := TransfersDeleteBackup(ctx, client, incusID, backup.IncusName); deleteErr != nil {
				log.Printf("[Transfer] Failed to delete backup (%s): %v", backup.Name, deleteErr)
			}
			if deleteErr := db.DeleteBackup(ctx, backup.ID); deleteErr != nil {
				log.Printf("[Transfer] Failed to delete backup DB record (%d): %v", backup.ID, deleteErr)
			}
		}
	}

	// 3.5 Snapshot and backup policies: a transfer drops the previous owner's
	// schedules. The second delete only runs when the first succeeded, matching the
	// TypeScript's single try block.
	if deleteErr := db.TransfersDeleteSnapshotPolicyByInstance(ctx, instanceID); deleteErr != nil {
		log.Printf("[Transfer] Failed to delete policies: %v", deleteErr)
		return
	}
	if deleteErr := db.TransfersDeleteBackupPolicyByInstance(ctx, instanceID); deleteErr != nil {
		log.Printf("[Transfer] Failed to delete policies: %v", deleteErr)
	}
}
