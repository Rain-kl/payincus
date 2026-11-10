// Port of server/src/routes/backups.ts (17 handlers).
//
// Mounting
// --------
// app.ts registers this plugin with `{ prefix: '/api/instances' }`. The Go
// wildcard is `:id`, not the TypeScript's `:instanceId`: gin's radix tree allows
// only one parameter *name* per path position and the /api/instances tree is
// already owned by snapshots.go / instance_billing.go / ip_addresses.go with
// `:id`. The Fastify parameter names `:backupId` / `:taskId` are kept where they do
// not collide; POST /restore/:backupId and POST|DELETE /restore/:taskId(+rollback)
// share the `:taskId` name in Go (the create route reads it as a backup id), which
// is the only way both can be registered.
//
// Auth surface
// ------------
//   - almost every route: `fastify.authenticateUser` → middleware.AuthenticateUser
//     (admins get 403 USER_ONLY before any instance lookup);
//   - POST /backups/export/:taskId/download-token uses `fastify.authenticate` →
//     middleware.Authenticate (any role), and then an owner-only check;
//   - GET /backups/export/:taskId/download is PUBLIC: its only authorization is the
//     one-time download token (lib/download_token.go), bound to user + instance +
//     task, consumed on first use.
//
// Incus seams (explicit failure, never a fake success)
// ----------------------------------------------------
//   - The backup read/create/delete calls are ported (lib/incus_backups.go) and go
//     through lib/incus_client.go like every other module.
//   - BackupsOpenExportStream is a seam: the shared IncusClient only exposes a
//     buffered Request, and this route needs a streaming body (no modification of
//     lib/incus_client.go is allowed from this module).
//   - BackupsInstanceExists / BackupsStartInstance / BackupsDeleteInstance are
//     seams: incus-restore.ts / incus-instances.ts#startInstance run egress-package
//     and GPU side effects owned by the instances wave. Rollback therefore answers
//     500 ROLLBACK_FAILED with a message naming the missing component.
//   - The restore/upload workers are Wave E; this module only owns the DB task rows
//     and their state machine (PENDING → PROCESSING → COMPLETED/FAILED).
//
// Faithfully preserved Node quirks
//   - POST /backups returns 202 "Backup creating" before Incus finishes; the DB row
//     is inserted with status 'creating' and flipped to ready/error asynchronously.
//   - The quota fast-path (checkBackupQuota) runs before the transfer lock, and the
//     atomic reservation re-checks inside the advisory-locked transaction.
//   - PUT /backup-policy only refuses a missing quota when `enabled` is true, and
//     the policy upsert does NOT reschedule next_run_at on update.
//   - The export task store is process-local (lib/security.go) and the download
//     token store is process-local (lib/download_token.go), per WAVE_D_SPEC §0.1.
//   - DELETE /backups/:id swallows an Incus delete failure and still soft-deletes
//     the row (Node logs a warning and continues).
//   - A restore task can be cancelled only while PENDING; a failed task can be
//     rolled back only after the original instance is proven to exist.
//
// Time: every time.Time that reaches SQL is UTC (WAVE_D_SPEC §0.2b).
package routes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	// backupsLogModule is the `module` column every backup action logs under.
	backupsLogModule = "backup"

	// backupsDefaultPolicyIntervalMinutes mirrors the `= 1440` defaults of the
	// policy GET/PUT handlers.
	backupsDefaultPolicyIntervalMinutes = 1440

	// backupsQueueMaxLength mirrors QUEUE_MAX_LENGTH in both queue routes.
	backupsQueueMaxLength = 10

	// backupsDownloadTokenTTLSeconds / backupsDownloadTokenMaxUsage mirror
	// generateDownloadToken(..., 300, 1).
	backupsDownloadTokenTTLSeconds = 300
	backupsDownloadTokenMaxUsage   = 1
)

// backupsPolicyIntervals mirrors `enum: [60, 360, 1440, 4320]`.
var backupsPolicyIntervals = []float64{60, 360, 1440, 4320}

// backupsPolicyIntervalAllowed mirrors the `enum` check (AJV runs it after type
// coercion).
func backupsPolicyIntervalAllowed(value float64) bool {
	for _, candidate := range backupsPolicyIntervals {
		if value == candidate {
			return true
		}
	}
	return false
}

// backupsCreateFields / backupsPolicyFields / backupsUploadRemoteFields describe
// the three Fastify body schemas (AJV semantics: required first, then properties in
// schema order, first error wins, coerceTypes:'array').
var (
	backupsCreateFields = []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: 50, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "description", Kind: ajvKindString, MinLength: -1, MaxLength: 200, Minimum: math.NaN(), Maximum: math.NaN()},
	}

	backupsPolicyFields = []ajvFieldSpec{
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "intervalMinutes", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	}

	backupsUploadRemoteFields = []ajvFieldSpec{
		{Name: "storageConfigId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	}
)

// ==================== seams ====================

// errBackupsSeam renders the explicit failure of a component that is not ported.
// It is never disguised as success: the message names the missing module so an
// operator can tell "not ported" apart from "the node said no".
func errBackupsSeam(component string) error {
	return fmt.Errorf("备份模块依赖尚未移植（seam: %s）", component)
}

// BackupsOpenExportStream mirrors getBackupExportStream. The Go IncusClient only
// exposes a buffered Request (whole body in memory), while a backup can be many
// gigabytes, so this stays a seam until a streaming API is added to
// lib/incus_client.go by its owner.
var BackupsOpenExportStream = func(
	ctx context.Context,
	client *lib.IncusClient,
	instanceName, backupName string,
) (io.ReadCloser, int64, error) {
	return nil, 0, errBackupsSeam("lib/incus/incus-backups.ts#getBackupExportStream（流式 HTTP）")
}

// BackupsInstanceExists mirrors incus-restore.ts#instanceExists. Unlike the
// TypeScript helper this seam RETURNS the error instead of answering `false`:
// answering "does not exist" would drive the rollback into its 409 ROLLBACK_UNSAFE
// branch and claim the original instance is gone, which is exactly the kind of
// fake conclusion the seam must not produce.
var BackupsInstanceExists = func(
	ctx context.Context,
	client *lib.IncusClient,
	instanceName string,
) (bool, error) {
	return false, errBackupsSeam("lib/incus/incus-restore.ts#instanceExists")
}

// BackupsStartInstance mirrors lib/incus/incus-instances.ts#startInstance, which
// first applies egress-package bindings and GPU devices (instances wave).
var BackupsStartInstance = func(
	ctx context.Context,
	client *lib.IncusClient,
	instanceName string,
) error {
	return errBackupsSeam("lib/incus/incus-instances.ts#startInstance（含 egress/GPU 副作用）")
}

// BackupsDeleteInstance mirrors lib/incus/incus-instances.ts#deleteInstance, which
// releases egress-package bindings and GPU devices before/after the destroy
// (instances wave).
var BackupsDeleteInstance = func(
	ctx context.Context,
	client *lib.IncusClient,
	instanceName string,
) error {
	return errBackupsSeam("lib/incus/incus-instances.ts#deleteInstance（含 egress/GPU 释放）")
}

// ==================== helpers ====================

// backupsParseID mirrors parsePositiveRouteId: `^[1-9]\d*$`, then a safe integer.
// Values above the PostgreSQL INT4 range are rejected as invalid (the same choice
// every other Go module makes; Node hands them to Prisma and surfaces a 500).
func backupsParseID(raw string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// backupsISOTime mirrors Date#toISOString().
func backupsISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// backupsISOTimePtr renders an optional time like `task.startedAt?.toISOString()`.
func backupsISOTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := backupsISOTime(*value)
	return &formatted
}

// backupsISOTimeFromMillis mirrors `new Date(ms).toISOString()`.
func backupsISOTimeFromMillis(value int64) string {
	return time.UnixMilli(value).UTC().Format("2006-01-02T15:04:05.000Z")
}

// backupsAuthUser adapts the verified claims to the permission model.
func backupsAuthUser(user *lib.AccessTokenClaims) lib.AuthUser {
	return lib.AuthUser{ID: user.ID, Role: user.Role, Username: user.Username}
}

// backupsInternalError is the generic 500 used where the TypeScript lets the error
// bubble to Fastify's handler.
func backupsInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// backupsRequireInstance resolves the instance and runs the AUTH004 permission
// check (admin, owner, or host owner). It writes the response and returns nil when
// the caller must stop.
func backupsRequireInstance(c *gin.Context, instanceID int32) *db.InstanceDetailRecord {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		backupsInternalError(c)
		return nil
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return nil
	}

	permission, err := lib.CheckInstancePermission(ctx, backupsAuthUser(user), lib.InstanceRef{
		UserID: instance.UserID,
		HostID: instance.HostID,
	})
	if err != nil {
		backupsInternalError(c)
		return nil
	}
	if !permission.Allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return nil
	}
	return instance
}

// backupsRequireOwner writes 403 when the caller is not the instance owner. Used by
// the export/download/upload routes, which are owner-only even for admins.
func backupsRequireOwner(c *gin.Context, instance *db.InstanceDetailRecord) bool {
	user := middleware.CurrentUser(c)
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return false
	}
	return true
}

// backupsTransferLocked mirrors checkTransferLock: it writes the 400 and reports
// whether the caller must stop.
func backupsTransferLocked(c *gin.Context, instanceID int32) bool {
	pending, err := db.SnapshotsHasPendingTransfer(c.Request.Context(), instanceID)
	if err != nil {
		backupsInternalError(c)
		return true
	}
	if pending {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferInstanceLocked, "")
		return true
	}
	return false
}

// backupsRequireVerifiedOperation mirrors requireVerifiedOperation: the verified
// record is CLAIMED (deleted) here, so one verification authorizes one deletion.
func backupsRequireVerifiedOperation(c *gin.Context, userID, backupID int32) bool {
	verification, err := lib.ClaimOperationVerificationRequirement(
		c.Request.Context(), int64(userID), models.OperationTypeDeleteBackup, int64(backupID))
	if err != nil {
		backupsInternalError(c)
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

// backupsIncusClient builds the pooled client for a host row.
func backupsIncusClient(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
}

// backupsNotify sends one backup notification. The Go notifier never returns an
// error (it logs per-channel failures), so the TypeScript's fire-and-forget call is
// satisfied structurally.
func backupsNotify(ctx context.Context, userID int32, event lib.EventType, data lib.EventData) {
	result := lib.SendNotification(ctx, int64(userID), event, data)
	if result.Failed > 0 {
		log.Printf("[backup] notification %s partially failed: sent=%d failed=%d", event, result.Sent, result.Failed)
	}
}

// backupsLog writes one log row for a backup action.
func backupsLog(ctx context.Context, userID, instanceID int32, action, content, result string, withInstance bool) {
	logUserID := int64(userID)
	options := db.CreateLogOptions{}
	if withInstance {
		options = db.LogOptionsWithInstance(&instanceID)
	}
	db.CreateLog(ctx, &logUserID, backupsLogModule, action, content, result, options)
}

// backupsIncusName mirrors `backup-${name.replace(/[^a-zA-Z0-9-_]/g, '-')}-${Date.now()}`.
//
// The sanitizer is shared with snapshots.go: in a JavaScript character class
// `-` between `0-9` and `_` is literal, and the regex walks UTF-16 code units (an
// astral character becomes two dashes). Reusing that implementation keeps both
// modules byte-identical instead of maintaining two subtle copies.
func backupsIncusName(name string, timestampMs int64) string {
	return "backup-" + snapshotsSanitizeIncusNameSegment(name) + "-" + strconv.FormatInt(timestampMs, 10)
}

// backupsTempInstanceName mirrors generateTempInstanceName:
// `{originalName}-restore-{Date.now()}`.
func backupsTempInstanceName(originalName string, timestampMs int64) string {
	return originalName + "-restore-" + strconv.FormatInt(timestampMs, 10)
}

// backupsRandomUUID mirrors crypto.randomUUID() (RFC 4122 version 4, lowercase).
func backupsRandomUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	segments := []string{
		hex.EncodeToString(raw[0:4]),
		hex.EncodeToString(raw[4:6]),
		hex.EncodeToString(raw[6:8]),
		hex.EncodeToString(raw[8:10]),
		hex.EncodeToString(raw[10:16]),
	}
	return strings.Join(segments, "-")
}

// backupsDownloadTokenResourceID mirrors buildBackupExportDownloadTokenResourceId:
// the token is bound to BOTH the instance and the task.
func backupsDownloadTokenResourceID(instanceID int32, taskID string) string {
	return strconv.FormatInt(int64(instanceID), 10) + ":" + taskID
}

// backupsEncodeURIComponent mirrors encodeURIComponent (UTF-8 percent encoding,
// unreserved set A-Z a-z 0-9 - _ . ! ~ * ' ( )).
func backupsEncodeURIComponent(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		switch {
		case b >= 'A' && b <= 'Z',
			b >= 'a' && b <= 'z',
			b >= '0' && b <= '9',
			b == '-', b == '_', b == '.', b == '!', b == '~', b == '*', b == '\'', b == '(', b == ')':
			out.WriteByte(b)
		default:
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String()
}

// backupsBackupDateString mirrors:
//
//	backupDate.toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15)
func backupsBackupDateString(value time.Time) string {
	stamp := strings.ReplaceAll(backupsISOTime(value), "-", "")
	stamp = strings.ReplaceAll(stamp, ":", "")
	stamp = strings.Replace(stamp, "T", "-", 1)
	if len(stamp) > 15 {
		stamp = stamp[:15]
	}
	return stamp
}

// backupsDurationSeconds mirrors the repeated duration block:
// floor((finished - started) / 1000) when both exist, floor((now - started) / 1000)
// while running, null otherwise.
func backupsDurationSeconds(startedAt, finishedAt *time.Time, now time.Time) *int64 {
	var seconds int64
	switch {
	case startedAt != nil && finishedAt != nil:
		seconds = int64(math.Floor(float64(finishedAt.UnixMilli()-startedAt.UnixMilli()) / 1000))
	case startedAt != nil:
		seconds = int64(math.Floor(float64(now.UnixMilli()-startedAt.UnixMilli()) / 1000))
	default:
		return nil
	}
	return &seconds
}

// ==================== response views ====================

type backupsListItem struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	IncusName   string  `json:"incusName"`
	Description *string `json:"description"`
	Size        int32   `json:"size"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	ExpiresAt   *string `json:"expires_at"`
}

type backupsCreatedBackup struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	IncusName   string  `json:"incusName"`
	Description *string `json:"description"`
	Status      string  `json:"status"`
}

type backupsCreateResponse struct {
	Message string               `json:"message"`
	Backup  backupsCreatedBackup `json:"backup"`
}

type backupsPolicyResponse struct {
	Enabled         bool    `json:"enabled"`
	IntervalMinutes int32   `json:"interval_minutes"`
	LastRunAt       *string `json:"last_run_at"`
	NextRunAt       *string `json:"next_run_at"`
}

type backupsExportStatusResponse struct {
	TaskID    string  `json:"taskId"`
	Status    string  `json:"status"`
	Error     *string `json:"error,omitempty"`
	ExpiresAt string  `json:"expiresAt"`
}

type backupsRestoreTaskResponse struct {
	TaskID        int32   `json:"taskId"`
	Status        string  `json:"status"`
	Error         *string `json:"error"`
	QueuePosition int32   `json:"queuePosition"`
	Duration      *int64  `json:"duration"`
	CreatedAt     string  `json:"createdAt"`
	StartedAt     *string `json:"startedAt"`
	FinishedAt    *string `json:"finishedAt"`
}

type backupsUploadTaskResponse struct {
	TaskID         int32   `json:"taskId"`
	Status         string  `json:"status"`
	Error          *string `json:"error"`
	RemoteFileName *string `json:"remoteFileName"`
	FileSize       *string `json:"fileSize,omitempty"`
	StorageName    *string `json:"storageName,omitempty"`
	StorageType    *string `json:"storageType,omitempty"`
	QueuePosition  int32   `json:"queuePosition"`
	Duration       *int64  `json:"duration"`
	CreatedAt      string  `json:"createdAt"`
	StartedAt      *string `json:"startedAt"`
	FinishedAt     *string `json:"finishedAt"`
}

type backupsActiveUploadTaskView struct {
	TaskID        int32   `json:"taskId"`
	BackupID      int32   `json:"backupId"`
	Status        string  `json:"status"`
	StorageName   *string `json:"storageName,omitempty"`
	StorageType   *string `json:"storageType,omitempty"`
	QueuePosition int32   `json:"queuePosition"`
	Duration      *int64  `json:"duration"`
	CreatedAt     string  `json:"createdAt"`
	StartedAt     *string `json:"startedAt"`
	FinishedAt    *string `json:"finishedAt"`
}

type backupsActiveUploadResponse struct {
	Task *backupsActiveUploadTaskView `json:"task"`
}

// backupsListView mirrors the `backups.map(b => ({...}))` projection.
func backupsListView(records []db.BackupsRecord) []backupsListItem {
	items := make([]backupsListItem, 0, len(records))
	for _, record := range records {
		items = append(items, backupsListItem{
			ID:          record.ID,
			Name:        record.Name,
			IncusName:   record.IncusName,
			Description: record.Description,
			Size:        record.Size,
			Status:      record.Status,
			CreatedAt:   record.CreatedAt,
			ExpiresAt:   record.ExpiresAt,
		})
	}
	return items
}

// backupsPolicyView mirrors the policy ? {...} : {...} projection, including the
// `intervalMinutes || 1440` fallback and the `|| null` for the timestamps.
func backupsPolicyView(policy *db.BackupsPolicy) backupsPolicyResponse {
	if policy == nil {
		return backupsPolicyResponse{
			Enabled:         false,
			IntervalMinutes: backupsDefaultPolicyIntervalMinutes,
			LastRunAt:       nil,
			NextRunAt:       nil,
		}
	}

	intervalMinutes := policy.IntervalMinutes
	if intervalMinutes == 0 {
		intervalMinutes = backupsDefaultPolicyIntervalMinutes
	}
	return backupsPolicyResponse{
		Enabled:         policy.Enabled,
		IntervalMinutes: intervalMinutes,
		LastRunAt:       backupsISOTimePtr(policy.LastRunAt),
		NextRunAt:       backupsISOTimePtr(policy.NextRunAt),
	}
}

// ==================== handlers: backups ====================

// backupsListHandler mirrors GET /:instanceId/backups.
func backupsListHandler(c *gin.Context) {
	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	records, err := db.GetBackupsByInstanceID(c.Request.Context(), instanceID)
	if err != nil {
		backupsInternalError(c)
		return
	}

	c.JSON(http.StatusOK, backupsListView(records))
}

// backupsCreateHandler mirrors POST /:instanceId/backups.
func backupsCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, []string{"name"}, backupsCreateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	name, _ := body.stringValue("name")
	var description *string
	if text, present := body.stringValue("description"); present {
		description = &text
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	// 封停状态不允许创建备份
	if instance.Status == "suspended" {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
		return
	}

	// 检查配额（快速路径；真正的原子预占在 createBackupWithQuotaReservation 内）
	quotaCheck, err := db.CheckBackupQuota(ctx, instanceID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if !quotaCheck.Allowed {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaBackupExceeded, quotaCheck.Message)
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	incusName := backupsIncusName(name, time.Now().UTC().UnixMilli())

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.create",
			`Failed to create backup for instance "`+instance.Name+`": `+err.Error(),
			db.LogResultFailed, true)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupCreateFailed, err.Error())
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := backupsIncusClient(ctx, host)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.create",
			`Failed to create backup for instance "`+instance.Name+`": `+err.Error(),
			db.LogResultFailed, true)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupCreateFailed, err.Error())
		return
	}

	// 原子预留备份配额并保存到数据库（状态为 creating）
	reservation, err := db.CreateBackupWithQuotaReservation(ctx, db.BackupsCreateData{
		InstanceID:  instanceID,
		IncusName:   incusName,
		Name:        name,
		Description: description,
	})
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.create",
			`Failed to create backup for instance "`+instance.Name+`": `+err.Error(),
			db.LogResultFailed, true)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupCreateFailed, err.Error())
		return
	}
	if !reservation.Allowed || reservation.BackupID == nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaBackupExceeded, reservation.Message)
		return
	}
	backupID := *reservation.BackupID

	// 在 Incus 中创建备份（异步操作，响应先返回 202）
	instanceName := instance.Name
	instanceIncusID := instance.IncusID
	hostName := host.Name
	userID := user.ID
	go func() {
		// Node's promise chain is not tied to the request lifecycle either.
		bgCtx := context.WithoutCancel(ctx)
		if _, createErr := lib.CreateIncusBackup(bgCtx, client, instanceIncusID, incusName,
			lib.IncusCreateBackupOptions{}); createErr != nil {
			if err := db.UpdateBackupStatus(bgCtx, backupID, "error"); err != nil {
				log.Printf("[backup] failed to mark backup %d as error: %v", backupID, err)
			}
			backupsLog(bgCtx, userID, instanceID, "backup.create",
				`Failed to create backup for instance "`+instanceName+`": `+createErr.Error(),
				db.LogResultFailed, true)
			log.Printf("[backup] backup %d failed: %v", backupID, createErr)
			return
		}

		if err := db.UpdateBackupStatus(bgCtx, backupID, "ready"); err != nil {
			log.Printf("[backup] failed to mark backup %d as ready: %v", backupID, err)
		}
		backupsNotify(bgCtx, userID, lib.EventBackupCreated, lib.EventData{
			"instanceName": instanceName,
			"backupName":   name,
			"hostName":     hostName,
		})
		backupsLog(bgCtx, userID, instanceID, "backup.create",
			`Created backup "`+name+`" for instance "`+instanceName+`" [host: `+hostName+`]`,
			db.LogResultSuccess, true)
		log.Printf("[backup] backup %d created successfully", backupID)
	}()

	c.JSON(http.StatusAccepted, backupsCreateResponse{
		Message: "Backup creating",
		Backup: backupsCreatedBackup{
			ID:          backupID,
			Name:        name,
			IncusName:   incusName,
			Description: description,
			Status:      "creating",
		},
	})
}

// backupsDeleteHandler mirrors DELETE /:instanceId/backups/:backupId.
func backupsDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	backupID, backupOK := backupsParseID(c.Param("backupId"))
	if !instanceOK || !backupOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	backup, err := db.GetBackupByID(ctx, backupID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if backup == nil || backup.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrBackupNotFound, "")
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}
	if !backupsRequireVerifiedOperation(c, user.ID, backupID) {
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.delete", `Failed to delete backup: `+err.Error(), db.LogResultFailed, true)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupDeleteFailed, err.Error())
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := backupsIncusClient(ctx, host)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.delete", `Failed to delete backup: `+err.Error(), db.LogResultFailed, true)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupDeleteFailed, err.Error())
		return
	}

	// 如果 Incus 中不存在，继续删除数据库记录（Node 只记 warn）
	if _, deleteErr := lib.DeleteIncusBackup(ctx, client, instance.IncusID, backup.IncusName); deleteErr != nil {
		log.Printf("[backup] backup %s not found in Incus, proceeding with DB deletion: %v", backup.IncusName, deleteErr)
	}

	if err := db.DeleteBackup(ctx, backupID); err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.delete", `Failed to delete backup: `+err.Error(), db.LogResultFailed, true)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupDeleteFailed, err.Error())
		return
	}

	backupsNotify(ctx, user.ID, lib.EventBackupDeleted, lib.EventData{
		"instanceName": instance.Name,
		"backupName":   backup.Name,
		"hostName":     host.Name,
	})
	backupsLog(ctx, user.ID, instanceID, "backup.delete",
		`Deleted backup "`+backup.Name+`" of instance "`+instance.Name+`" [host: `+host.Name+`]`,
		db.LogResultSuccess, true)

	c.JSON(http.StatusOK, gin.H{"message": "Backup deleted"})
}

// backupsGetPolicyHandler mirrors GET /:instanceId/backup-policy.
func backupsGetPolicyHandler(c *gin.Context) {
	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	policy, err := db.GetBackupPolicy(c.Request.Context(), instanceID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	c.JSON(http.StatusOK, backupsPolicyView(policy))
}

// backupsPutPolicyHandler mirrors PUT /:instanceId/backup-policy.
func backupsPutPolicyHandler(c *gin.Context) {
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, []string{"enabled"}, backupsPolicyFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	// The enum keyword is checked after the type coercion; validateAJVObject does
	// not model enums for integer fields, so it is applied here in AJV's order.
	if raw, present := body.Values["intervalMinutes"]; present {
		number, _ := raw.(float64)
		if !backupsPolicyIntervalAllowed(number) {
			respondAJVValidation(c, "must be equal to one of the allowed values")
			return
		}
	}

	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// `const { enabled, intervalMinutes = 1440 } = request.body`
	enabled := body.boolValue("enabled")
	intervalMinutes := int32(backupsDefaultPolicyIntervalMinutes)
	if raw, present := body.Values["intervalMinutes"]; present {
		if number, isNumber := raw.(float64); isNumber {
			intervalMinutes = int32(number)
		}
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	// 如果启用自动备份，检查配额
	if enabled {
		if instance.BackupLimit == nil || *instance.BackupLimit <= 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBackupQuotaNotSet, "")
			return
		}
	}

	if err := db.UpsertBackupPolicy(ctx, instanceID, enabled, intervalMinutes); err != nil {
		backupsInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Auto backup policy updated"})
}

// ==================== handlers: export ====================

// backupsCreateExportHandler mirrors POST /:instanceId/backups/:backupId/export.
func backupsCreateExportHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	// Fastify validates `{ type: 'object', properties: {} }` before the handler.
	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	if _, validationMessage := validateAJVObject(decoded.Value, nil, nil); validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	backupID, backupOK := backupsParseID(c.Param("backupId"))
	if !instanceOK || !backupOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	// AUTH004: 备份导出/下载仅限实例所有者
	if !backupsRequireOwner(c, instance) {
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	backup, err := db.GetBackupByID(ctx, backupID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if backup == nil || backup.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrBackupNotFound, "")
		return
	}
	if backup.Status != "ready" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBackupNotReady, "")
		return
	}

	taskID := backupsRandomUUID()
	task := lib.CreateExportTask(lib.CreateExportTaskInput{
		ID:              taskID,
		InstanceID:      instanceID,
		BackupID:        backupID,
		IncusBackupName: backup.IncusName,
		UserID:          user.ID,
		Status:          lib.ExportTaskStatusReady,
	})

	backupsLog(ctx, user.ID, instanceID, "backup.export",
		`Generated export task for backup "`+backup.Name+`" of instance "`+instance.Name+`"`,
		db.LogResultSuccess, true)

	c.JSON(http.StatusOK, gin.H{
		"taskId":      taskID,
		"status":      "ready",
		"expiresAt":   backupsISOTimeFromMillis(task.ExpiresAt),
		"downloadUrl": "/api/instances/" + strconv.FormatInt(int64(instanceID), 10) + "/backups/export/" + taskID + "/download",
	})
}

// backupsExportStatusHandler mirrors GET /:instanceId/backups/export/:taskId/status.
func backupsExportStatusHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	taskID := c.Param("taskId")

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}
	if !backupsRequireOwner(c, instance) {
		return
	}

	task := lib.GetExportTask(taskID)
	if task == nil || task.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrExportTaskNotFound, "")
		return
	}
	if task.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	if time.Now().UnixMilli() > task.ExpiresAt {
		lib.DeleteExportTask(taskID)
		lib.RespondAPIError(c, http.StatusGone, lib.ErrExportTaskExpired, "")
		return
	}

	var taskError *string
	if task.Error != "" {
		taskError = &task.Error
	}
	c.JSON(http.StatusOK, backupsExportStatusResponse{
		TaskID:    task.ID,
		Status:    string(task.Status),
		Error:     taskError,
		ExpiresAt: backupsISOTimeFromMillis(task.ExpiresAt),
	})
}

// backupsDownloadTokenHandler mirrors POST
// /:instanceId/backups/export/:taskId/download-token.
//
// Auth is `fastify.authenticate` (any role), unlike the rest of the module.
func backupsDownloadTokenHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	taskID := c.Param("taskId")

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}
	if !backupsRequireOwner(c, instance) {
		return
	}

	task := lib.GetExportTask(taskID)
	if task == nil || task.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrExportTaskNotFound, "")
		return
	}
	if task.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	if time.Now().UnixMilli() > task.ExpiresAt {
		lib.DeleteExportTask(taskID)
		lib.RespondAPIError(c, http.StatusGone, lib.ErrExportTaskExpired, "")
		return
	}

	if task.Status != lib.ExportTaskStatusReady {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBackupExportStatusInvalid, string(task.Status))
		return
	}

	// 一次性下载 token（5 分钟有效，仅可使用 1 次）
	downloadToken := lib.GenerateDownloadToken(
		int64(user.ID),
		backupsDownloadTokenResourceID(instanceID, taskID),
		lib.DownloadTokenResourceBackupExport,
		backupsDownloadTokenTTLSeconds,
		backupsDownloadTokenMaxUsage,
	)

	c.JSON(http.StatusOK, gin.H{
		"downloadUrl": "/api/instances/" + strconv.FormatInt(int64(instanceID), 10) +
			"/backups/export/" + taskID + "/download?dt=" + downloadToken,
		"expiresIn": backupsDownloadTokenTTLSeconds,
	})
}

// backupsDownloadHandler mirrors the PUBLIC GET
// /:instanceId/backups/export/:taskId/download.
//
// The one-time token is consumed in the first pass (the TypeScript's onRequest
// hook); the handler then re-resolves the instance/task and uses the token's user
// id for the owner check, so a stolen token cannot be replayed and a valid token
// cannot be redirected to another instance.
func backupsDownloadHandler(c *gin.Context) {
	ctx := c.Request.Context()
	instanceID, ok := backupsParseID(c.Param("id"))
	taskID := c.Param("taskId")

	downloadToken := c.Query("dt")
	if downloadToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Download token required. Use POST /download-token to generate one.",
			"code":  "DOWNLOAD_TOKEN_REQUIRED",
		})
		return
	}

	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	result := lib.ConsumeDownloadToken(
		downloadToken,
		backupsDownloadTokenResourceID(instanceID, taskID),
		lib.DownloadTokenResourceBackupExport,
	)
	if !result.Valid {
		message := result.Error
		if message == "" {
			message = "Invalid or expired download token"
		}
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": message,
			"code":  "DOWNLOAD_TOKEN_INVALID",
		})
		return
	}

	// 验证实例归属（使用 token 中的用户 ID）
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	task := lib.GetExportTask(taskID)
	if task == nil || task.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrExportTaskNotFound, "")
		return
	}
	if int64(task.UserID) != result.UserID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	if time.Now().UnixMilli() > task.ExpiresAt {
		lib.DeleteExportTask(taskID)
		lib.RespondAPIError(c, http.StatusGone, lib.ErrExportTaskExpired, "")
		return
	}

	if task.Status != lib.ExportTaskStatusReady {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBackupExportStatusInvalid, string(task.Status))
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusError, err.Error())
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupExportFailed, err.Error())
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := backupsIncusClient(ctx, host)
	if err != nil {
		lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusError, err.Error())
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupExportFailed, err.Error())
		return
	}

	// 更新任务状态
	lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusDownloading, "")

	backup, err := db.GetBackupByID(ctx, task.BackupID)
	if err != nil {
		lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusError, err.Error())
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupExportFailed, err.Error())
		return
	}
	user, err := db.FindUserById(ctx, instance.UserID)
	if err != nil {
		lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusError, err.Error())
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupExportFailed, err.Error())
		return
	}

	// 文件名格式: 用户名-实例名-备份名-备份日期.tar.gz
	backupDate := time.Now().UTC()
	if backup != nil && backup.CreatedAt != "" {
		if parsed, parseErr := time.Parse("2006-01-02T15:04:05.000Z", backup.CreatedAt); parseErr == nil {
			backupDate = parsed
		}
	}
	dateStr := backupsBackupDateString(backupDate)
	userName := "unknown"
	if user != nil {
		userName = user.Username
	}
	backupName := task.IncusBackupName
	if backup != nil {
		backupName = backup.Name
	}
	fileName := userName + "-" + instance.Name + "-" + backupName + "-" + dateStr + ".tar.gz"

	stream, contentLength, err := BackupsOpenExportStream(ctx, client, instance.IncusID, task.IncusBackupName)
	if err != nil {
		lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusError, err.Error())
		log.Printf("[backup] backup export failed: %v", err)
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrBackupExportFailed, err.Error())
		return
	}
	if stream != nil {
		defer func() { _ = stream.Close() }()
	}

	c.Header("Content-Disposition", `attachment; filename="`+backupsEncodeURIComponent(fileName)+`"`)
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.Header("X-Content-Type-Options", "nosniff")
	if contentLength > 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	}

	if stream != nil {
		if _, copyErr := io.Copy(c.Writer, stream); copyErr != nil {
			lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusError, copyErr.Error())
			log.Printf("[backup] backup export stream error: %v", copyErr)
			return
		}
	}

	lib.UpdateExportTaskStatus(taskID, lib.ExportTaskStatusCompleted, "")
	log.Printf("[backup] backup export completed: %s", taskID)
	// Node deletes the finished task five seconds later.
	time.AfterFunc(5*time.Second, func() { lib.DeleteExportTask(taskID) })
}

// ==================== handlers: restore ====================

// backupsCreateRestoreHandler mirrors POST /:instanceId/restore/:backupId. The Go
// route param is named `:taskId` (see the file header) but carries a backup id.
func backupsCreateRestoreHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	if _, validationMessage := validateAJVObject(decoded.Value, nil, nil); validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	backupID, backupOK := backupsParseID(c.Param("taskId"))
	if !instanceOK || !backupOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	backup, err := db.GetBackupByID(ctx, backupID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if backup == nil || backup.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrBackupNotFound, "")
		return
	}
	if backup.Status != "ready" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBackupNotReady, "")
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	// 检查是否有正在进行的恢复任务
	activeTaskID, err := db.HasActiveRestoreTask(ctx, instanceID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if activeTaskID != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "RESTORE_IN_PROGRESS",
			"message": "该实例已有恢复任务正在进行中",
			"taskId":  *activeTaskID,
		})
		return
	}

	// 检查队列长度（防止队列过长）
	pendingCount, err := db.CountPendingRestoreTasksForHost(ctx, instance.HostID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if pendingCount >= backupsQueueMaxLength {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":   "QUEUE_TOO_LONG",
			"message": fmt.Sprintf("队列已满（%d/%d），请稍后再试", pendingCount, backupsQueueMaxLength),
		})
		return
	}

	tempName := backupsTempInstanceName(instance.IncusID, time.Now().UTC().UnixMilli())

	taskID, err := db.CreateRestoreTask(ctx, db.CreateRestoreTaskData{
		InstanceID:           instanceID,
		BackupID:             &backupID,
		HostID:               instance.HostID,
		UserID:               user.ID,
		TempInstanceName:     tempName,
		OriginalInstanceName: instance.Name,
		OriginalIncusID:      instance.IncusID,
	})
	if err != nil {
		backupsInternalError(c)
		return
	}
	if taskID == nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "RESTORE_IN_PROGRESS",
			"message": "该实例已有恢复任务正在进行中",
		})
		return
	}

	backupsLog(ctx, user.ID, instanceID, "backup.restore",
		`Queued restore task for instance "`+instance.Name+`" from backup "`+backup.Name+`"`,
		db.LogResultSuccess, false)

	queuePosition, err := db.GetRestoreTaskQueuePosition(ctx, *taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}

	message := "恢复任务已创建，即将开始执行"
	if queuePosition > 1 {
		message = fmt.Sprintf("恢复任务已加入队列，当前排队位置：第 %d 位", queuePosition)
	}
	c.JSON(http.StatusOK, gin.H{
		"taskId":        *taskID,
		"status":        "PENDING",
		"queuePosition": queuePosition,
		"message":       message,
	})
}

// backupsGetRestoreHandler mirrors GET /:instanceId/restore/:taskId.
func backupsGetRestoreHandler(c *gin.Context) {
	instanceID, instanceOK := backupsParseID(c.Param("id"))
	taskID, taskOK := backupsParseID(c.Param("taskId"))
	if !instanceOK || !taskOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	ctx := c.Request.Context()
	task, err := db.GetRestoreTaskByID(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil || task.InstanceID != instanceID {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "RESTORE_TASK_NOT_FOUND",
			"message": "恢复任务不存在",
		})
		return
	}

	queuePosition := int32(0)
	if task.Status == "PENDING" {
		queuePosition, err = db.GetRestoreTaskQueuePosition(ctx, taskID)
		if err != nil {
			backupsInternalError(c)
			return
		}
	}

	c.JSON(http.StatusOK, backupsRestoreTaskResponse{
		TaskID:        task.ID,
		Status:        task.Status,
		Error:         task.Error,
		QueuePosition: queuePosition,
		Duration:      backupsDurationSeconds(task.StartedAt, task.FinishedAt, time.Now()),
		CreatedAt:     backupsISOTime(task.CreatedAt),
		StartedAt:     backupsISOTimePtr(task.StartedAt),
		FinishedAt:    backupsISOTimePtr(task.FinishedAt),
	})
}

// backupsRollbackRestoreHandler mirrors POST /:instanceId/restore/:taskId/rollback.
func backupsRollbackRestoreHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	taskID, taskOK := backupsParseID(c.Param("taskId"))
	if !instanceOK || !taskOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	task, err := db.GetRestoreTaskByID(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil || task.InstanceID != instanceID {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "RESTORE_TASK_NOT_FOUND",
			"message": "恢复任务不存在",
		})
		return
	}

	// 只有失败的任务可以回滚
	if task.Status != "FAILED" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "ROLLBACK_NOT_ALLOWED",
			"message": "只有失败的恢复任务可以回滚",
		})
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
			`Failed to rollback restore task: `+err.Error(), db.LogResultFailed, true)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ROLLBACK_FAILED", "message": err.Error()})
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	client, err := backupsIncusClient(ctx, host)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
			`Failed to rollback restore task: `+err.Error(), db.LogResultFailed, true)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ROLLBACK_FAILED", "message": err.Error()})
		return
	}

	// 先确认原实例存在，再清理临时实例；不存在时保留临时实例用于人工恢复。
	originalExists, err := BackupsInstanceExists(ctx, client, task.OriginalIncusID)
	if err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
			`Failed to rollback restore task: `+err.Error(), db.LogResultFailed, true)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ROLLBACK_FAILED", "message": err.Error()})
		return
	}
	if !originalExists {
		message := "原实例不存在，已保留临时实例用于人工恢复"
		backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
			fmt.Sprintf("Refused rollback for restore task %d: %s", taskID, message),
			db.LogResultFailed, true)
		c.JSON(http.StatusConflict, gin.H{"error": "ROLLBACK_UNSAFE", "message": message})
		return
	}

	if err := BackupsStartInstance(ctx, client, task.OriginalIncusID); err != nil {
		backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
			`Failed to rollback restore task: `+err.Error(), db.LogResultFailed, true)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ROLLBACK_FAILED", "message": err.Error()})
		return
	}

	if task.TempInstanceName != nil {
		tempExists, existsErr := BackupsInstanceExists(ctx, client, *task.TempInstanceName)
		if existsErr != nil {
			backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
				`Failed to rollback restore task: `+existsErr.Error(), db.LogResultFailed, true)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "ROLLBACK_FAILED", "message": existsErr.Error()})
			return
		}
		if tempExists {
			if deleteErr := BackupsDeleteInstance(ctx, client, *task.TempInstanceName); deleteErr != nil {
				backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
					`Failed to rollback restore task: `+deleteErr.Error(), db.LogResultFailed, true)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "ROLLBACK_FAILED", "message": deleteErr.Error()})
				return
			}
			log.Printf("[backup] deleted temp instance: %s", *task.TempInstanceName)
		}
	}

	// 数据库没有 rolled_back 状态，标记为 FAILED 并记录
	rollbackMessage := "用户手动回滚"
	if _, err := db.UpdateRestoreTaskStatus(ctx, taskID, "FAILED", &rollbackMessage); err != nil {
		backupsInternalError(c)
		return
	}

	backupsLog(ctx, user.ID, instanceID, "backup.restore.rollback",
		`Rolled back restore task for instance "`+instance.Name+`"`, db.LogResultSuccess, true)

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "回滚完成"})
}

// backupsCancelRestoreHandler mirrors DELETE /:instanceId/restore/:taskId.
func backupsCancelRestoreHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	taskID, taskOK := backupsParseID(c.Param("taskId"))
	if !instanceOK || !taskOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	task, err := db.GetRestoreTaskByID(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil || task.InstanceID != instanceID {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "RESTORE_TASK_NOT_FOUND",
			"message": "恢复任务不存在",
		})
		return
	}

	// 只有 PENDING 状态的任务可以取消
	if task.Status != "PENDING" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "CANCEL_NOT_ALLOWED",
			"message": "只有排队中的任务可以取消",
		})
		return
	}

	cancelled, err := db.CancelRestoreTask(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if cancelled == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "CANCEL_FAILED",
			"message": "取消任务失败，任务可能已开始执行",
		})
		return
	}

	backupsLog(ctx, user.ID, instanceID, "backup.restore.cancel",
		`Cancelled restore task for instance "`+instance.Name+`"`, db.LogResultSuccess, true)

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "任务已取消"})
}

// ==================== handlers: upload ====================

// backupsUploadRemoteHandler mirrors POST /:instanceId/backups/:backupId/upload-remote.
func backupsUploadRemoteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, nil, backupsUploadRemoteFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	backupID, backupOK := backupsParseID(c.Param("backupId"))
	if !instanceOK || !backupOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}

	// AUTH004: 备份上传仅限实例所有者
	if !backupsRequireOwner(c, instance) {
		return
	}

	backup, err := db.GetBackupByID(ctx, backupID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if backup == nil || backup.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrBackupNotFound, "")
		return
	}
	if backup.Status != "ready" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBackupNotReady, "")
		return
	}

	if backupsTransferLocked(c, instanceID) {
		return
	}

	// 获取存储配置：`if (request.body.storageConfigId)` (0 is falsy)
	var storageConfig *models.StorageConfig
	if raw, present := body.Values["storageConfigId"]; present {
		if number, isNumber := raw.(float64); isNumber && number != 0 {
			config, configErr := db.GetStorageConfigByID(ctx, int32(number))
			if configErr != nil {
				backupsInternalError(c)
				return
			}
			if config == nil || config.UserID != user.ID {
				lib.RespondAPIError(c, http.StatusNotFound, lib.ErrStorageConfigNotFound, "")
				return
			}
			storageConfig = config
		}
	}
	if storageConfig == nil {
		config, configErr := db.GetDefaultStorageConfig(ctx, user.ID)
		if configErr != nil {
			backupsInternalError(c)
			return
		}
		if config == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "NO_DEFAULT_STORAGE",
				"message": "请先在设置中配置远程存储",
			})
			return
		}
		storageConfig = config
	}

	// 检查是否有进行中的上传任务
	activeTask, err := db.HasActiveUploadTask(ctx, user.ID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if activeTask != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "BACKUP_UPLOAD_IN_PROGRESS",
			"message": "您有上传任务正在进行中",
			"taskId":  activeTask.ID,
		})
		return
	}

	// 检查队列长度（防止队列过长）
	pendingCount, err := db.CountPendingUploadTasksForHost(ctx, instance.HostID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if pendingCount >= backupsQueueMaxLength {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":   "QUEUE_TOO_LONG",
			"message": fmt.Sprintf("队列已满（%d/%d），请稍后再试", pendingCount, backupsQueueMaxLength),
		})
		return
	}

	task, err := db.CreateBackupUploadTask(ctx, user.ID, instanceID, backupID, instance.HostID, storageConfig.ID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "BACKUP_UPLOAD_IN_PROGRESS",
			"message": "您有上传任务正在进行中",
		})
		return
	}

	backupsLog(ctx, user.ID, instanceID, "backup.upload.queue",
		`Queued backup upload for "`+backup.Name+`" to "`+storageConfig.Name+`"`,
		db.LogResultSuccess, true)

	queuePosition, err := db.GetUploadTaskQueuePosition(ctx, task.ID)
	if err != nil {
		backupsInternalError(c)
		return
	}

	message := "上传任务已创建，即将开始执行"
	if queuePosition > 1 {
		message = fmt.Sprintf("上传任务已加入队列，当前排队位置：第 %d 位", queuePosition)
	}
	c.JSON(http.StatusOK, gin.H{
		"taskId":        task.ID,
		"status":        "PENDING",
		"queuePosition": queuePosition,
		"storageName":   storageConfig.Name,
		"message":       message,
	})
}

// backupsGetUploadTaskHandler mirrors GET /:instanceId/upload-tasks/:taskId.
func backupsGetUploadTaskHandler(c *gin.Context) {
	ctx := c.Request.Context()

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	taskID, taskOK := backupsParseID(c.Param("taskId"))
	if !instanceOK || !taskOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}
	if !backupsRequireOwner(c, instance) {
		return
	}

	task, err := db.GetBackupUploadTaskWithDetails(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil || task.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrBackupUploadTaskNotFound, "")
		return
	}

	queuePosition := int32(0)
	if task.Status == "PENDING" {
		queuePosition, err = db.GetUploadTaskQueuePosition(ctx, taskID)
		if err != nil {
			backupsInternalError(c)
			return
		}
	}

	response := backupsUploadTaskResponse{
		TaskID:         task.ID,
		Status:         task.Status,
		Error:          task.Error,
		RemoteFileName: task.RemoteFileName,
		StorageName:    task.StorageConfigName,
		StorageType:    task.StorageConfigType,
		QueuePosition:  queuePosition,
		Duration:       backupsDurationSeconds(task.StartedAt, task.FinishedAt, time.Now()),
		CreatedAt:      backupsISOTime(task.CreatedAt),
		StartedAt:      backupsISOTimePtr(task.StartedAt),
		FinishedAt:     backupsISOTimePtr(task.FinishedAt),
	}
	if task.FileSize != nil {
		formatted := strconv.FormatInt(*task.FileSize, 10)
		response.FileSize = &formatted
	}

	c.JSON(http.StatusOK, response)
}

// backupsCancelUploadTaskHandler mirrors DELETE /:instanceId/upload-tasks/:taskId.
func backupsCancelUploadTaskHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instanceID, instanceOK := backupsParseID(c.Param("id"))
	taskID, taskOK := backupsParseID(c.Param("taskId"))
	if !instanceOK || !taskOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}
	if !backupsRequireOwner(c, instance) {
		return
	}

	task, err := db.GetBackupUploadTaskByID(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil || task.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrBackupUploadTaskNotFound, "")
		return
	}

	// 只有 PENDING 状态的任务可以取消
	if task.Status != "PENDING" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "CANCEL_NOT_ALLOWED",
			"message": "只有排队中的任务可以取消",
		})
		return
	}

	cancelled, err := db.CancelBackupUploadTask(ctx, taskID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if cancelled == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "CANCEL_FAILED",
			"message": "取消任务失败，任务可能已开始执行",
		})
		return
	}

	backupsLog(ctx, user.ID, instanceID, "backup.upload.cancel",
		"Cancelled backup upload task", db.LogResultSuccess, true)

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "任务已取消"})
}

// backupsActiveUploadTaskHandler mirrors GET /:instanceId/upload-tasks/active.
func backupsActiveUploadTaskHandler(c *gin.Context) {
	instanceID, ok := backupsParseID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	instance := backupsRequireInstance(c, instanceID)
	if instance == nil {
		return
	}
	if !backupsRequireOwner(c, instance) {
		return
	}

	ctx := c.Request.Context()
	task, err := db.GetActiveUploadTaskForInstance(ctx, instanceID)
	if err != nil {
		backupsInternalError(c)
		return
	}
	if task == nil {
		c.JSON(http.StatusOK, backupsActiveUploadResponse{Task: nil})
		return
	}

	queuePosition := int32(0)
	if task.Status == "PENDING" {
		queuePosition, err = db.GetUploadTaskQueuePosition(ctx, task.ID)
		if err != nil {
			backupsInternalError(c)
			return
		}
	}

	c.JSON(http.StatusOK, backupsActiveUploadResponse{
		Task: &backupsActiveUploadTaskView{
			TaskID:        task.ID,
			BackupID:      task.BackupID,
			Status:        task.Status,
			StorageName:   task.StorageConfigName,
			StorageType:   task.StorageConfigType,
			QueuePosition: queuePosition,
			Duration:      backupsDurationSeconds(task.StartedAt, task.FinishedAt, time.Now()),
			CreatedAt:     backupsISOTime(task.CreatedAt),
			StartedAt:     backupsISOTimePtr(task.StartedAt),
			FinishedAt:    backupsISOTimePtr(task.FinishedAt),
		},
	})
}

// ==================== route registration ====================

// RegisterBackups mounts the backup routes under /api/instances, matching
// `fastify.register(backupRoutes, { prefix: '/api/instances' })`.
//
// Param names: `:id` (the shared /api/instances wildcard) and `:backupId` /
// `:taskId` as in the TypeScript, except that POST /restore/:backupId and
// POST|DELETE /restore/:taskId share the `:taskId` name at that position — gin
// panics when two routes disagree on the wildcard name.
func RegisterBackups(r *gin.Engine) {
	group := r.Group("/api/instances")

	group.GET("/:id/backups", middleware.AuthenticateUser(), backupsListHandler)
	group.POST("/:id/backups", middleware.AuthenticateUser(), backupsCreateHandler)
	group.DELETE("/:id/backups/:backupId", middleware.AuthenticateUser(), backupsDeleteHandler)
	group.GET("/:id/backup-policy", middleware.AuthenticateUser(), backupsGetPolicyHandler)
	group.PUT("/:id/backup-policy", middleware.AuthenticateUser(), backupsPutPolicyHandler)

	group.POST("/:id/backups/:backupId/export", middleware.AuthenticateUser(), backupsCreateExportHandler)
	group.GET("/:id/backups/export/:taskId/status", middleware.AuthenticateUser(), backupsExportStatusHandler)
	// `fastify.authenticate` — any authenticated role (the handler then requires
	// instance ownership).
	group.POST("/:id/backups/export/:taskId/download-token", middleware.Authenticate(), backupsDownloadTokenHandler)
	// PUBLIC: authorized only by the one-time download token.
	group.GET("/:id/backups/export/:taskId/download", backupsDownloadHandler)

	group.POST("/:id/restore/:taskId", middleware.AuthenticateUser(), backupsCreateRestoreHandler)
	group.GET("/:id/restore/:taskId", middleware.AuthenticateUser(), backupsGetRestoreHandler)
	group.POST("/:id/restore/:taskId/rollback", middleware.AuthenticateUser(), backupsRollbackRestoreHandler)
	group.DELETE("/:id/restore/:taskId", middleware.AuthenticateUser(), backupsCancelRestoreHandler)

	group.POST("/:id/backups/:backupId/upload-remote", middleware.AuthenticateUser(), backupsUploadRemoteHandler)
	group.GET("/:id/upload-tasks/:taskId", middleware.AuthenticateUser(), backupsGetUploadTaskHandler)
	group.DELETE("/:id/upload-tasks/:taskId", middleware.AuthenticateUser(), backupsCancelUploadTaskHandler)
	group.GET("/:id/upload-tasks/active", middleware.AuthenticateUser(), backupsActiveUploadTaskHandler)
}

func init() {
	Register(RegisterBackups)
}
