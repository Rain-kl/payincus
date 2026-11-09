// Port of server/src/routes/snapshots.ts (mounted at /api/instances, the prefix
// declared in app.ts; app.ts does not set prefixTrailingSlash and none of these
// routes is a plugin root "/", so there is nothing to mount twice).
//
// Every route runs `onRequest: [fastify.authenticateUser]`, i.e. the Go
// middleware.AuthenticateUser (authenticated *regular users only*; admins get
// 403 USER_ONLY). Fastify parses the body and validates it against `schema.body`
// BEFORE the handler parses route params, so the handlers below validate the body
// first and only then call lib.ParsePositiveRouteID. Body-parser failures
// (400/413/415) are already produced by the auth middleware, exactly as Fastify's
// onRequest → parser ordering requires.
//
// Security notes:
//
//   - Snapshot quota. `checkSnapshotQuota` measures an instance's quota as the
//     number of `snapshots` rows, so inserting the row is what consumes a slot.
//     The create route takes the *distributed* lock
//     `auto-policy:snapshot:<instanceId>` (30 min expiry, 5 s wait, 100 ms retry)
//     and re-checks the quota INSIDE the lock, then inserts the Incus snapshot and
//     only then the row. That ordering is the TypeScript's and must not move: it is
//     what stops two concurrent manual snapshots from both passing a pre-lock
//     read. Deleting the row releases the slot; there is no counter to decrement.
//   - Deleting a snapshot additionally claims a second-factor verification record
//     for `delete_snapshot` (single-use: ClaimOperationVerificationRequirement
//     deletes the verified row atomically).
//   - Host-owner permissions come from lib.CheckInstancePermission (AUTH004); no
//     route here trusts a client-supplied owner id.
//
// Faithfully preserved Node quirks:
//
//   - create/delete/restore return hand-built `{error, code:'OPERATION_FAILED'}`
//     envelopes (500) for failures — not apiError() payloads — and the exact
//     `Failed to <verb> ...: <message>` text.
//   - The delete and restore paths do NOT give their notification its own
//     try/catch (create does), so a *thrown* notification error would turn a
//     completed operation into a 500 after the Incus/DB change already happened.
//     In practice lib/notifier.ts catches internally and never throws, and the Go
//     notifier returns a SendResult instead of an error, so both sides just log.
//   - `createSnapshot` failures are cleaned up by deleting the Incus snapshot
//     before the DB error is rethrown.
//   - A 404 HOST_NOT_FOUND inside the try returns early without writing a log row.
//   - The snapshot-policy upsert does NOT recompute next_run_at on update, and
//     `intervalMinutes` defaults to 360 only when the key is absent.
//   - `snapshot_limit === null || <= 0` denies enabling the policy with
//     QUOTA_NOT_ALLOCATED (no details).
package routes

import (
	"context"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
	"unicode/utf16"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Snapshot create lock timings, mirroring SNAPSHOT_CREATE_LOCK_*.
const (
	snapshotsCreateLockExpireMs        = 30 * 60 * 1000
	snapshotsCreateLockWaitMs          = 5000
	snapshotsCreateLockRetryIntervalMs = 100

	// snapshotsLogModule is the literal module passed to createLog.
	snapshotsLogModule = "snapshot"

	// snapshotsDefaultPolicyIntervalMinutes mirrors the `|| 360` / `= 360`
	// defaults of the policy GET/PUT handlers.
	snapshotsDefaultPolicyIntervalMinutes = 360
)

// snapshotsPolicyIntervals mirrors `enum: [10, 60, 360, 1440]`.
var snapshotsPolicyIntervals = []float64{10, 60, 360, 1440}

// snapshotsIntervalAllowed mirrors the `enum` check (AJV validates it after the
// type coercion).
func snapshotsIntervalAllowed(value float64) bool {
	for _, candidate := range snapshotsPolicyIntervals {
		if value == candidate {
			return true
		}
	}
	return false
}

// snapshotsCreateFields / snapshotsPolicyFields describe the two Fastify body
// schemas (AJV semantics: required first, then properties in schema order, first
// error wins, coerceTypes:'array').
var (
	snapshotsCreateFields = []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: 50, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "description", Kind: ajvKindString, MinLength: -1, MaxLength: 200, Minimum: math.NaN(), Maximum: math.NaN()},
	}

	snapshotsPolicyFields = []ajvFieldSpec{
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "intervalMinutes", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	}
)

// ==================== helpers ====================

// snapshotsParseID mirrors parsePositiveId for a route param: `^[1-9]\d*$`, then a
// safe integer. Values above the PostgreSQL INT4 range are rejected as invalid
// (the same choice every other Go module makes; Node would hand them to Prisma and
// surface a 500).
func snapshotsParseID(raw string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// snapshotsIntPtr is the address-of helper for lib.LockOptions' optional fields.
func snapshotsIntPtr(value int) *int { return &value }

// snapshotsCreateLockKey mirrors getSnapshotCreateLockKey.
func snapshotsCreateLockKey(instanceID int32) string {
	return "auto-policy:snapshot:" + strconv.FormatInt(int64(instanceID), 10)
}

// snapshotsSanitizeIncusNameSegment mirrors
// `name.replace(/[^a-zA-Z0-9-_]/g, '-')`.
//
// In a JavaScript character class `-` between `0-9` and `_` is a literal, so the
// allowed set is [a-zA-Z0-9_-]. The regex has no `u` flag, so it walks UTF-16 code
// units: an astral character (e.g. an emoji, which the 50-unit maxLength permits)
// becomes two dashes. Encoding to UTF-16 first keeps that behaviour byte-identical.
func snapshotsSanitizeIncusNameSegment(name string) string {
	units := utf16.Encode([]rune(name))
	out := make([]uint16, 0, len(units))
	for _, unit := range units {
		switch {
		case unit >= 'a' && unit <= 'z',
			unit >= 'A' && unit <= 'Z',
			unit >= '0' && unit <= '9',
			unit == '-', unit == '_':
			out = append(out, unit)
		default:
			out = append(out, '-')
		}
	}
	return string(utf16.Decode(out))
}

// snapshotsIncusName mirrors `snap-${sanitized}-${Date.now()}`.
func snapshotsIncusName(name string, timestampMs int64) string {
	return "snap-" + snapshotsSanitizeIncusNameSegment(name) + "-" + strconv.FormatInt(timestampMs, 10)
}

// snapshotsISOTime mirrors Date#toISOString().
func snapshotsISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// snapshotsAuthUser adapts the verified claims to the permission model.
func snapshotsAuthUser(user *lib.AccessTokenClaims) lib.AuthUser {
	return lib.AuthUser{ID: user.ID, Role: user.Role, Username: user.Username}
}

// snapshotsInternalError is the generic 500 used where the TypeScript lets the
// error bubble to Fastify's handler (development leaks the message; every Go
// module in this port uses the fixed envelope instead).
func snapshotsInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// snapshotsRequireInstance resolves the instance and runs the AUTH004 permission
// check. It writes the response and returns nil when the caller must stop.
func snapshotsRequireInstance(c *gin.Context, instanceID int32) *db.SnapshotsInstance {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instance, err := db.SnapshotsGetInstanceByID(ctx, instanceID)
	if err != nil {
		snapshotsInternalError(c)
		return nil
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return nil
	}

	permission, err := lib.CheckInstancePermission(ctx, snapshotsAuthUser(user), lib.InstanceRef{
		UserID: instance.UserID,
		HostID: instance.HostID,
	})
	if err != nil {
		snapshotsInternalError(c)
		return nil
	}
	if !permission.Allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return nil
	}
	return instance
}

// snapshotsTransferLocked mirrors checkTransferLock: it writes the 400 and reports
// whether the caller must stop.
func snapshotsTransferLocked(c *gin.Context, instanceID int32) bool {
	pending, err := db.SnapshotsHasPendingTransfer(c.Request.Context(), instanceID)
	if err != nil {
		snapshotsInternalError(c)
		return true
	}
	if pending {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferInstanceLocked, "")
		return true
	}
	return false
}

// snapshotsRequireVerifiedOperation mirrors requireVerifiedOperation: the verified
// record is CLAIMED (deleted) here, so one verification authorizes one deletion.
func snapshotsRequireVerifiedOperation(c *gin.Context, userID, snapshotID int32) bool {
	verification, err := lib.ClaimOperationVerificationRequirement(
		c.Request.Context(), int64(userID), models.OperationTypeDeleteSnapshot, int64(snapshotID))
	if err != nil {
		snapshotsInternalError(c)
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

// snapshotsOperationFailed mirrors the shared catch block of the create / delete /
// restore handlers: a 'failed' log row plus the hand-built 500 envelope.
func snapshotsOperationFailed(c *gin.Context, userID, instanceID int32, action, prefix string, err error) {
	message := err.Error()
	log.Printf("[snapshot] %s failed for instance %d: %v", action, instanceID, err)

	logUserID := int64(userID)
	db.CreateLog(c.Request.Context(), &logUserID, snapshotsLogModule, action,
		prefix+message, db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusInternalServerError, gin.H{
		"error": prefix + message,
		"code":  "OPERATION_FAILED",
	})
}

// snapshotsIncusClient builds the pooled client for a host row.
func snapshotsIncusClient(ctx context.Context, host *db.SnapshotsHost) (*lib.IncusClient, error) {
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
}

// snapshotsNotify sends one snapshot notification. The Go notifier never returns
// an error (it logs per-channel failures), so the TypeScript's optional
// try/catch is satisfied structurally; a partial failure is logged like Fastify
// would.
func snapshotsNotify(ctx context.Context, userID int32, event lib.EventType, data lib.EventData) {
	result := lib.SendNotification(ctx, int64(userID), event, data)
	if result.Failed > 0 {
		log.Printf("[snapshot] notification %s partially failed: sent=%d failed=%d", event, result.Sent, result.Failed)
	}
}

// ==================== response views ====================

type snapshotsListItem struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	IncusName   string  `json:"incusName"`
	Description *string `json:"description"`
	Stateful    bool    `json:"stateful"`
	Size        int32   `json:"size"`
	CreatedAt   string  `json:"created_at"`
}

type snapshotsCreatedSnapshot struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	IncusName   string  `json:"incusName"`
	Description *string `json:"description,omitempty"`
	Stateful    bool    `json:"stateful"`
}

type snapshotsCreateResponse struct {
	Message  string                   `json:"message"`
	Snapshot snapshotsCreatedSnapshot `json:"snapshot"`
}

type snapshotsPolicyResponse struct {
	Enabled         bool    `json:"enabled"`
	IntervalMinutes int32   `json:"interval_minutes"`
	LastRunAt       *string `json:"last_run_at"`
	NextRunAt       *string `json:"next_run_at"`
}

// snapshotsListView mirrors the `snapshots.map(s => ({...}))` projection.
func snapshotsListView(snapshots []db.SnapshotsSnapshot) []snapshotsListItem {
	items := make([]snapshotsListItem, 0, len(snapshots))
	for _, snapshot := range snapshots {
		items = append(items, snapshotsListItem{
			ID:          snapshot.ID,
			Name:        snapshot.Name,
			IncusName:   snapshot.IncusName,
			Description: snapshot.Description,
			Stateful:    snapshot.Stateful,
			Size:        snapshot.Size,
			CreatedAt:   snapshotsISOTime(snapshot.CreatedAt),
		})
	}
	return items
}

// snapshotsPolicyView mirrors the `policy ? {...} : {...}` projection, including
// `intervalMinutes || 360`.
func snapshotsPolicyView(policy *db.SnapshotsPolicy) snapshotsPolicyResponse {
	if policy == nil {
		return snapshotsPolicyResponse{
			Enabled:         false,
			IntervalMinutes: snapshotsDefaultPolicyIntervalMinutes,
			LastRunAt:       nil,
			NextRunAt:       nil,
		}
	}

	intervalMinutes := policy.IntervalMinutes
	if intervalMinutes == 0 {
		intervalMinutes = snapshotsDefaultPolicyIntervalMinutes
	}
	var lastRunAt, nextRunAt *string
	if policy.LastRunAt != nil {
		formatted := snapshotsISOTime(*policy.LastRunAt)
		lastRunAt = &formatted
	}
	if policy.NextRunAt != nil {
		formatted := snapshotsISOTime(*policy.NextRunAt)
		nextRunAt = &formatted
	}

	return snapshotsPolicyResponse{
		Enabled:         policy.Enabled,
		IntervalMinutes: intervalMinutes,
		LastRunAt:       lastRunAt,
		NextRunAt:       nextRunAt,
	}
}

// ==================== handlers ====================

// snapshotsListHandler mirrors GET /:instanceId/snapshots.
func snapshotsListHandler(c *gin.Context) {
	instanceID, ok := snapshotsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := snapshotsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	snapshots, err := db.SnapshotsListByInstanceID(c.Request.Context(), instanceID)
	if err != nil {
		snapshotsInternalError(c)
		return
	}

	c.JSON(http.StatusOK, snapshotsListView(snapshots))
}

// snapshotsCreateHandler mirrors POST /:instanceId/snapshots.
func snapshotsCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, []string{"name"}, snapshotsCreateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, ok := snapshotsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	name, _ := body.stringValue("name")
	var description *string
	if text, present := body.stringValue("description"); present {
		description = &text
	}

	instance := snapshotsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	// 封停状态不允许创建快照
	if instance.Status == "suspended" {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
		return
	}
	// 实例必须是运行或停止状态
	if instance.Status != "running" && instance.Status != "stopped" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "")
		return
	}

	if snapshotsTransferLocked(c, instanceID) {
		return
	}

	incusName := snapshotsIncusName(name, time.Now().UnixMilli())
	lockKey := snapshotsCreateLockKey(instanceID)
	lockResult, lockErr := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
		ExpireMs:        snapshotsIntPtr(snapshotsCreateLockExpireMs),
		WaitTimeoutMs:   snapshotsIntPtr(snapshotsCreateLockWaitMs),
		RetryIntervalMs: snapshotsIntPtr(snapshotsCreateLockRetryIntervalMs),
	})
	if lockErr != nil {
		// acquireLock runs before the TypeScript's try block, so an error here
		// escapes to Fastify as a generic 500.
		snapshotsInternalError(c)
		return
	}
	if !lockResult.Success || lockResult.OwnerID == "" {
		lib.RespondAPIError(c, http.StatusConflict, lib.ErrOperationNotAllowed,
			"Snapshot operation is busy, please retry later")
		return
	}
	// The TypeScript releases in `finally`. context.WithoutCancel keeps the release
	// working even if the client already went away (Node's releaseLock is not
	// tied to the request abort signal either).
	releaseCtx := context.WithoutCancel(ctx)
	defer lib.ReleaseLock(releaseCtx, lockKey, lockResult.OwnerID)

	// 在实例级锁内重新检查配额，避免并发手动快照同时通过读前检查。
	quota, err := db.SnapshotsCheckSnapshotQuota(ctx, instanceID)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.create", "Failed to create snapshot: ", err)
		return
	}
	if !quota.Allowed {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaSnapshotExceeded, quota.Message)
		return
	}

	host, err := db.SnapshotsGetHostByID(ctx, instance.HostID)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.create", "Failed to create snapshot: ", err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := snapshotsIncusClient(ctx, host)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.create", "Failed to create snapshot: ", err)
		return
	}

	if _, err := lib.CreateSnapshot(ctx, client, instance.IncusID, incusName); err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.create", "Failed to create snapshot: ", err)
		return
	}

	snapshotID, err := db.SnapshotsCreate(ctx, db.SnapshotsCreateData{
		InstanceID:  instanceID,
		IncusName:   incusName,
		Name:        name,
		Description: description,
		Stateful:    false,
	})
	if err != nil {
		// 数据库写入失败时回滚 Incus 侧快照，然后抛出原始错误
		if _, cleanupErr := lib.DeleteSnapshot(ctx, client, instance.IncusID, incusName); cleanupErr != nil {
			log.Printf("[snapshot] failed to clean up Incus snapshot %q for instance %d after database create failure: %v",
				incusName, instanceID, cleanupErr)
		}
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.create", "Failed to create snapshot: ", err)
		return
	}

	// 发送通知（失败只记警告）
	snapshotsNotify(ctx, user.ID, lib.EventSnapshotCreated, lib.EventData{
		"instanceName": instance.Name,
		"snapshotName": name,
		"hostName":     host.Name,
	})

	logUserID := int64(user.ID)
	db.CreateLog(ctx, &logUserID, snapshotsLogModule, "snapshot.create",
		`Created snapshot "`+name+`" for instance "`+instance.Name+`" [host: `+host.Name+`]`,
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusCreated, snapshotsCreateResponse{
		Message: "Snapshot created",
		Snapshot: snapshotsCreatedSnapshot{
			ID:          snapshotID,
			Name:        name,
			IncusName:   incusName,
			Description: description,
			Stateful:    false,
		},
	})
}

// snapshotsDeleteHandler mirrors DELETE /:instanceId/snapshots/:snapshotId.
func snapshotsDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instanceID, instanceOK := snapshotsParseID(c.Param("id"))
	snapshotID, snapshotOK := snapshotsParseID(c.Param("snapshotId"))
	if !instanceOK || !snapshotOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := snapshotsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	snapshot, err := db.SnapshotsGetByID(ctx, snapshotID)
	if err != nil {
		snapshotsInternalError(c)
		return
	}
	if snapshot == nil || snapshot.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrSnapshotNotFound, "")
		return
	}

	if snapshotsTransferLocked(c, instanceID) {
		return
	}
	if !snapshotsRequireVerifiedOperation(c, user.ID, snapshotID) {
		return
	}

	host, err := db.SnapshotsGetHostByID(ctx, instance.HostID)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.delete", "Failed to delete snapshot: ", err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := snapshotsIncusClient(ctx, host)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.delete", "Failed to delete snapshot: ", err)
		return
	}

	if _, err := lib.DeleteSnapshot(ctx, client, instance.IncusID, snapshot.IncusName); err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.delete", "Failed to delete snapshot: ", err)
		return
	}
	if err := db.SnapshotsDelete(ctx, snapshotID); err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.delete", "Failed to delete snapshot: ", err)
		return
	}

	// Not wrapped in its own try/catch in the TypeScript (see the header note): a
	// thrown error here would reach the catch block after the deletion already
	// happened.
	snapshotsNotify(ctx, user.ID, lib.EventSnapshotDeleted, lib.EventData{
		"instanceName": instance.Name,
		"snapshotName": snapshot.Name,
		"hostName":     host.Name,
	})

	logUserID := int64(user.ID)
	db.CreateLog(ctx, &logUserID, snapshotsLogModule, "snapshot.delete",
		`Deleted snapshot "`+snapshot.Name+`" of instance "`+instance.Name+`" [host: `+host.Name+`]`,
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{"message": "Snapshot deleted"})
}

// snapshotsRestoreHandler mirrors POST /:instanceId/snapshots/:snapshotId/restore.
func snapshotsRestoreHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	// The restore schema is `{ type: 'object', properties: {} }`: with
	// coerceTypes:'array' and `properties` present, AJV has a rule for the object
	// type, so a non-object body (including an absent one, validated as null) is
	// rejected with "must be object". Nothing else is validated.
	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	if _, validationMessage := validateAJVObject(decoded.Value, nil, nil); validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, instanceOK := snapshotsParseID(c.Param("id"))
	snapshotID, snapshotOK := snapshotsParseID(c.Param("snapshotId"))
	if !instanceOK || !snapshotOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := snapshotsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	snapshot, err := db.SnapshotsGetByID(ctx, snapshotID)
	if err != nil {
		snapshotsInternalError(c)
		return
	}
	if snapshot == nil || snapshot.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrSnapshotNotFound, "")
		return
	}

	// 封停状态不允许恢复快照
	if instance.Status == "suspended" {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
		return
	}
	// 实例必须停止才能恢复
	if instance.Status != "stopped" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSnapshotRestoreRequiresStop, "")
		return
	}

	if snapshotsTransferLocked(c, instanceID) {
		return
	}

	host, err := db.SnapshotsGetHostByID(ctx, instance.HostID)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.restore", "Failed to restore snapshot: ", err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := snapshotsIncusClient(ctx, host)
	if err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.restore", "Failed to restore snapshot: ", err)
		return
	}

	if _, err := lib.RestoreSnapshot(ctx, client, instance.IncusID, snapshot.IncusName); err != nil {
		snapshotsOperationFailed(c, user.ID, instanceID, "snapshot.restore", "Failed to restore snapshot: ", err)
		return
	}

	snapshotsNotify(ctx, user.ID, lib.EventSnapshotRestored, lib.EventData{
		"instanceName": instance.Name,
		"snapshotName": snapshot.Name,
		"hostName":     host.Name,
	})

	logUserID := int64(user.ID)
	db.CreateLog(ctx, &logUserID, snapshotsLogModule, "snapshot.restore",
		`Restored instance "`+instance.Name+`" to snapshot "`+snapshot.Name+`" [host: `+host.Name+`]`,
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{"message": "Snapshot restored"})
}

// snapshotsGetPolicyHandler mirrors GET /:instanceId/snapshot-policy.
func snapshotsGetPolicyHandler(c *gin.Context) {
	instanceID, ok := snapshotsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := snapshotsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	policy, err := db.SnapshotsGetPolicy(c.Request.Context(), instanceID)
	if err != nil {
		snapshotsInternalError(c)
		return
	}
	c.JSON(http.StatusOK, snapshotsPolicyView(policy))
}

// snapshotsPutPolicyHandler mirrors PUT /:instanceId/snapshot-policy.
func snapshotsPutPolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, []string{"enabled"}, snapshotsPolicyFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	// The enum keyword is checked after the type coercion; validateAJVObject does
	// not model enums for integer fields, so it is applied here in AJV's order.
	if raw, present := body.Values["intervalMinutes"]; present {
		number, _ := raw.(float64)
		if !snapshotsIntervalAllowed(number) {
			respondAJVValidation(c, "must be equal to one of the allowed values")
			return
		}
	}

	instanceID, ok := snapshotsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// `const { enabled, intervalMinutes = 360 } = request.body`
	enabled := body.boolValue("enabled")
	intervalMinutes := int32(snapshotsDefaultPolicyIntervalMinutes)
	if raw, present := body.Values["intervalMinutes"]; present {
		if number, isNumber := raw.(float64); isNumber {
			intervalMinutes = int32(number)
		}
	}

	instance := snapshotsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	if snapshotsTransferLocked(c, instanceID) {
		return
	}

	// 如果启用自动快照，检查配额
	if enabled {
		if instance.SnapshotLimit == nil || *instance.SnapshotLimit <= 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaNotAllocated, "")
			return
		}
	}

	if err := db.SnapshotsUpsertPolicy(ctx, instanceID, enabled, intervalMinutes); err != nil {
		snapshotsInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Auto snapshot policy updated"})
}

// RegisterSnapshots mounts /api/instances.
//
// The route params are named `:id` / `:snapshotId` — NOT the TypeScript's
// `:instanceId` — because gin's radix tree requires a single wildcard name per path
// position and the instance routes (routes/instances.go, high-risk wave) will use
// `:id` for `/api/instances/:id/...`. A different name here would panic at startup
// as soon as both modules are mounted.
func RegisterSnapshots(r *gin.Engine) {
	group := r.Group("/api/instances", middleware.AuthenticateUser())

	group.GET("/:id/snapshots", snapshotsListHandler)
	group.POST("/:id/snapshots", snapshotsCreateHandler)
	group.DELETE("/:id/snapshots/:snapshotId", snapshotsDeleteHandler)
	group.POST("/:id/snapshots/:snapshotId/restore", snapshotsRestoreHandler)
	group.GET("/:id/snapshot-policy", snapshotsGetPolicyHandler)
	group.PUT("/:id/snapshot-policy", snapshotsPutPolicyHandler)
}

func init() {
	Register(RegisterSnapshots)
}
