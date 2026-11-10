// Port of server/src/routes/instances.ts (34 routes, prefix /api/instances).
//
// app.ts registers this module as `fastify.register(instanceRoutes, { prefix:
// '/api/instances' })`, so every path below is relative to /api/instances. The
// wildcard is `:id` everywhere because snapshots / backups / instance-billing /
// instance-destroy already own `/api/instances/:id/...` with that parameter name
// (Gin allows one parameter name per path position; see registry.go).
//
// High-risk invariants this file implements (docs/WAVE_D_SPEC.md):
//
//   - §4.1 resource delivery is "intent first, execute, confirm": POST / writes the
//     `creating` row (plus host reservation, balance debit, billing record) in ONE
//     transaction, calls Incus afterwards, flips the row to `running` on success and
//     runs the compensation path on failure. The compensation gate is the atomic
//     `creating -> error` UPDATE: a second compensation run finds zero rows and
//     returns `already_handled` without touching balance, quota or IPv4.
//   - §0.3/§0.4 money: the create transaction takes USER_BALANCE_LOCK_NAMESPACE
//     (4110) before reading/writing the balance, snapshots balance_before/after in
//     balance_logs and stores amounts as models.Decimal. Host capacity uses
//     db.HostsSelectAndReserveWithLock (row lock FOR UPDATE) inside the same
//     transaction.
//   - §0.2b time: every timestamp bound into SQL is UTC.
//   - tri-state updates (quota / config / swap / resources) keep the "undefined"
//     vs "explicit NULL" distinction; empty updates return early (db layer does it).
//
// Incus-touching paths (start/stop/restart/suspend-stop/rebuild/recreate/clone/
// change-host/create/delete/ports/swap/config/reassign-ipv6) are deliberately NOT
// part of the dual-run conformance suite: Node's axios and Go's net/http produce
// different error text for an unreachable node (documented in GO_MIGRATION.md).
// They are covered by unit tests instead.
//
// db and lib files are owned by other waves, so the few operations with no exported
// Go helper are implemented here on the same SQL/client primitives and documented:
//
//   - the create transaction cannot call db.CreateInstance because that function
//     opens its own pgx transaction (it has no tx parameter), which would split the
//     "reserve + insert + charge" atomicity. instancesCreateRow mirrors its INSERT
//     and the egress-binding helper inside the caller's *gorm.DB transaction.
//   - getInstancesPaginated / getInstanceAffBinding-with-plan / the promo+VIP read
//     in GET /:id have no exported equivalent, so they are read-only SQL here.
package routes

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

const (
	// instancesListMaxPageSize mirrors INSTANCE_LIST_MAX_PAGE_SIZE.
	instancesListMaxPageSize = 100

	// instancesLogModule mirrors the literal 'instance' module passed to createLog.
	instancesLogModule = "instance"

	// instancesPortMappingLock* mirror PORT_MAPPING_LOCK_OPTIONS.
	instancesPortMappingLockExpireMs       = 120000
	instancesPortMappingLockWaitTimeoutMs  = 5000
	instancesPortMappingLockRetryInterval  = 100
	instancesMaxBatchPortMappings          = 100
	instancesPortMappingLockKeyFormat      = "instance:%d:port-mappings"
	instancesNameTimestampLayout           = "20060102150405"
	instancesAutoNameRegionPattern         = `[^a-z0-9]`
	instancesCreateTransactionTimeoutMS    = 15000
	instancesDeleteTransactionTimeoutMS    = 15000
	instancesCloudInitStatusJSONPath       = "/run/cloud-init/status.json"
	instancesCloudInitStatusJSONPathLegacy = "/var/lib/cloud/data/status.json"
	instancesCloudInitBootFinishedPath     = "/var/lib/cloud/instance/boot-finished"
	instancesDefaultNATIPv4Gateway         = "10.10.0.1"
	instancesDefaultNATIPv4CIDR            = "/22"
	instancesHostIPFallbackGateway         = "10.10.1.1"
)

// instancesPositiveRouteIDPattern mirrors POSITIVE_ROUTE_ID_PATTERN.
var instancesPositiveRouteIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// instancesListStatuses mirrors INSTANCE_LIST_STATUSES.
var instancesListStatuses = map[string]bool{
	string(models.InstanceStatusCreating):  true,
	string(models.InstanceStatusRunning):   true,
	string(models.InstanceStatusStopped):   true,
	string(models.InstanceStatusSuspended): true,
	string(models.InstanceStatusError):     true,
	string(models.InstanceStatusDeleted):   true,
}

// instancesIntegerOnlyPattern is used by normalizePlanTrafficLimitSpeed callers
// that receive a raw string (kept local to avoid a shared helper rename).
var instancesNonRegionPattern = regexp.MustCompile(instancesAutoNameRegionPattern)

// ==================== small helpers ====================

func instancesStringPtr(value string) *string { return &value }

func instancesInt32Ptr(value int32) *int32 { return &value }

func instancesBoolPtr(value bool) *bool { return &value }

func instancesInt64Ptr(value int64) *int64 { return &value }

func instancesIntPtr(value int) *int { return &value }

// instancesScopeUser returns the authenticated claims. The auth middlewares guarantee
// a user, so a nil here is an internal wiring bug, not a client error.
func instancesScopeUser(c *gin.Context) *lib.AccessTokenClaims {
	return middleware.CurrentUser(c)
}

// instancesAuthUser projects the claims onto lib.AuthUser for the permission module.
func instancesAuthUser(claims *lib.AccessTokenClaims) lib.AuthUser {
	return lib.AuthUser{ID: claims.ID, Role: claims.Role, Username: claims.Username}
}

// instancesParseRouteID mirrors parsePositiveRouteId: `^[1-9]\d*$` then a safe
// integer. valid=false is the 400 INVALID_ID branch. A syntactically valid value
// outside int32 cannot exist in the database (the column is int4), so it returns
// id=0 with valid=true and every caller treats 0 as "row not found" — this keeps
// the 400/404 split identical to Node for every id that can actually match a row.
func instancesParseRouteID(value string) (id int32, valid bool) {
	if !instancesPositiveRouteIDPattern.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		// Longer than int64: the pattern matched, so Node would treat it as a
		// (safe-integer-violating) id. Treat as not found rather than invalid.
		return 0, true
	}
	if parsed > math.MaxInt32 {
		return 0, true
	}
	return int32(parsed), true
}

// instancesParseOptionalPositiveQueryInteger mirrors parseOptionalPositiveQueryInteger:
// (value=nil|"") -> (0,false,true); invalid -> (0,false,false) [the 400 branch].
func instancesParseOptionalPositiveQueryInteger(value string, present bool) (id int32, provided bool, valid bool) {
	if !present || value == "" {
		return 0, false, true
	}
	parsed, ok := instancesParseRouteID(value)
	if !ok {
		return 0, false, false
	}
	if parsed == 0 {
		// Syntactically valid but not representable: Node's parsePositiveRouteId
		// returns a number here, so it is "provided but matches nothing".
		return 0, false, false
	}
	return parsed, true, true
}

// instancesServiceStatus mirrors instanceServiceStatus: presentation only. An
// unavailable node (or a resume-pending name on it) displays running/stopped/error
// as 'error' without changing provisioning, refund or suspension state.
func instancesServiceStatus(status string, host *hostsHostServiceStatus, incusID string) string {
	if status != string(models.InstanceStatusRunning) &&
		status != string(models.InstanceStatusStopped) &&
		status != string(models.InstanceStatusError) {
		return status
	}
	if host == nil || !host.Available {
		return string(models.InstanceStatusError)
	}
	if incusID != "" {
		for _, pending := range host.PendingInstances {
			if pending == incusID {
				return string(models.InstanceStatusError)
			}
		}
	}
	return status
}

// instancesServiceStatusFilterSQL mirrors instanceServiceStatusFilter: it keeps the
// paginated total and the status filter consistent with the presentation overlay.
// The returned fragment references the `instances` alias `i`.
func instancesServiceStatusFilterSQL(status string, statuses map[int32]hostsHostServiceStatus) (string, []any) {
	unavailable := make([]string, 0, len(statuses))
	args := make([]any, 0, 8)
	hostIDs := make([]int32, 0, len(statuses))
	for hostID := range statuses {
		hostIDs = append(hostIDs, hostID)
	}
	sort.Slice(hostIDs, func(a, b int) bool { return hostIDs[a] < hostIDs[b] })
	for _, hostID := range hostIDs {
		host := statuses[hostID]
		if !host.Available {
			unavailable = append(unavailable, "i.host_id = ?")
			args = append(args, hostID)
			continue
		}
		if len(host.PendingInstances) > 0 {
			// Bind in placeholder order: the host id first, then the pending names.
			args = append(args, hostID)
			placeholders := make([]string, 0, len(host.PendingInstances))
			for _, name := range host.PendingInstances {
				placeholders = append(placeholders, "?")
				args = append(args, name)
			}
			unavailable = append(unavailable,
				"(i.host_id = ? AND i.incus_id IN ("+strings.Join(placeholders, ", ")+"))")
		}
	}
	if len(unavailable) == 0 {
		return "i.status = ?", []any{status}
	}
	orClause := strings.Join(unavailable, " OR ")
	if status == string(models.InstanceStatusError) {
		return "(i.status = 'error' OR (i.status IN ('running', 'stopped') AND (" + orClause + ")))", args
	}
	return "(i.status = ? AND NOT (" + orClause + "))", append([]any{status}, args...)
}

// instancesHostIPAddress mirrors the `host.ip_address || URL hostname` fallback.
func instancesHostIPAddress(host *db.HostsRecord) *string {
	if host == nil {
		return nil
	}
	if host.IPAddress != nil && *host.IPAddress != "" {
		return host.IPAddress
	}
	if host.URL != "" {
		return lib.ExtractHostAddressFromURL(&host.URL)
	}
	return nil
}

// instancesISOTime mirrors Date#toISOString() for a timestamp(3) column.
func instancesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func instancesOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := instancesISOTime(*value)
	return &formatted
}

// instancesNumber mirrors I/O with a Prisma Decimal/float that Node passes through
// Number(): JSON must carry a number, not a string.
func instancesNumber(value *float64) *float64 { return value }

// ==================== permission / guard helpers ====================

// instancesRequireInstancePermission mirrors checkInstanceOperationPermission + the
// 403 reply: admin, instance owner, or host owner.
func instancesRequireInstancePermission(c *gin.Context, claims *lib.AccessTokenClaims, instance *db.InstanceDetailRecord) (bool, error) {
	ok, err := lib.RequireInstancePermission(c, instancesAuthUser(claims), lib.InstanceRef{
		UserID: instance.UserID,
		HostID: instance.HostID,
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// instancesCheckTransferLock mirrors checkTransferLock: a pending transfer freezes
// the instance with 400 TRANSFER_INSTANCE_LOCKED.
func instancesCheckTransferLock(c *gin.Context, instanceID int32) (bool, error) {
	pending, err := db.SnapshotsHasPendingTransfer(c.Request.Context(), instanceID)
	if err != nil {
		return false, err
	}
	if pending {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTransferInstanceLocked, "")
		return true, nil
	}
	return false, nil
}

// instancesSendActiveTaskConflict mirrors sendActiveTaskConflict (409 TASK_IN_PROGRESS).
func instancesSendActiveTaskConflict(c *gin.Context, task *db.InstanceTaskWithDetails) {
	payload := gin.H{"error": "Instance has an active task", "code": "TASK_IN_PROGRESS"}
	if task != nil {
		payload["taskId"] = task.ID
		payload["taskType"] = task.TaskType
		payload["status"] = task.Status
	}
	c.JSON(http.StatusConflict, payload)
}

// instancesRejectActiveWorkflowConflict mirrors rejectActiveInstanceWorkflowConflict:
// restore task, backup upload task, instance task — in that order.
func instancesRejectActiveWorkflowConflict(c *gin.Context, instanceID int32) (bool, error) {
	ctx := c.Request.Context()

	var restoreTask struct {
		ID     int32
		Status string
	}
	err := db.DB.WithContext(ctx).
		Raw(`SELECT id, status FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') ORDER BY id ASC LIMIT 1`, instanceID).
		Scan(&restoreTask).Error
	if err != nil {
		return false, err
	}
	if restoreTask.ID != 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Instance has an active restore task",
			"code":   "RESTORE_IN_PROGRESS",
			"taskId": restoreTask.ID, "status": restoreTask.Status,
		})
		return true, nil
	}

	var uploadTask struct {
		ID     int32
		Status string
	}
	err = db.DB.WithContext(ctx).
		Raw(`SELECT id, status FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') ORDER BY id ASC LIMIT 1`, instanceID).
		Scan(&uploadTask).Error
	if err != nil {
		return false, err
	}
	if uploadTask.ID != 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Instance has an active backup upload task",
			"code":   "UPLOAD_IN_PROGRESS",
			"taskId": uploadTask.ID, "status": uploadTask.Status,
		})
		return true, nil
	}

	active, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		return false, err
	}
	if active != nil {
		instancesSendActiveTaskConflict(c, active)
		return true, nil
	}
	return false, nil
}

// instancesCreateTaskOrConflict mirrors createInstanceTaskOrConflict: it writes the
// 409 on InstanceTaskConflictError and returns nil, or propagates any other error.
func instancesCreateTaskOrConflict(c *gin.Context, data db.CreateInstanceTaskData) (*db.InstanceTaskWithDetails, error) {
	task, err := db.CreateInstanceTask(c.Request.Context(), data)
	if err != nil {
		var conflict *db.InstanceTaskConflictError
		if errors.As(err, &conflict) {
			instancesSendActiveTaskConflict(c, conflict.ActiveTask)
			return nil, nil
		}
		return nil, err
	}
	return task, nil
}

// instancesRequireVerifiedOperation mirrors requireVerifiedOperation.
func instancesRequireVerifiedOperation(c *gin.Context, userID int32, operation models.OperationType, instanceID int32) (bool, error) {
	verification, err := lib.ClaimOperationVerificationRequirement(
		c.Request.Context(), int64(userID), operation, int64(instanceID))
	if err != nil {
		return false, err
	}
	if !verification.Verified {
		c.JSON(http.StatusForbidden, gin.H{
			"error":    "Sensitive operation requires verification",
			"code":     "VERIFICATION_REQUIRED",
			"required": verification.Required,
		})
		return false, nil
	}
	return true, nil
}

// ==================== instance naming ====================

// instancesFormatNameTimestamp mirrors formatInstanceNameTimestamp (local time!).
func instancesFormatNameTimestamp(date time.Time) string {
	return date.Format(instancesNameTimestampLayout)
}

// instancesNameRegionPrefix mirrors getInstanceNameRegionPrefix.
func instancesNameRegionPrefix(countryCode *string) string {
	raw := "incus"
	if countryCode != nil && *countryCode != "" {
		raw = *countryCode
	}
	normalized := instancesNonRegionPattern.ReplaceAllString(strings.ToLower(raw), "")
	if len(normalized) > 6 {
		normalized = normalized[:6]
	}
	if normalized == "" {
		return "incus"
	}
	return normalized
}

// instancesRandomAlphabet mirrors nanoid's custom alphabet generator: a uniformly
// random string over the given alphabet.
func instancesRandomAlphabet(alphabet string, size int) string {
	out := make([]byte, size)
	buffer := make([]byte, size)
	if _, err := cryptorand.Read(buffer); err != nil {
		for index := range buffer {
			buffer[index] = byte(time.Now().UnixNano() >> uint(index%8))
		}
	}
	for index, value := range buffer {
		out[index] = alphabet[int(value)%len(alphabet)]
	}
	return string(out)
}

// instancesNanoid8 mirrors `customAlphabet('abcdefghijklmnopqrstuvwxyz0123456789', 8)`.
func instancesNanoid8() string {
	return instancesRandomAlphabet("abcdefghijklmnopqrstuvwxyz0123456789", 8)
}

// instancesGenerateAutoInstanceName mirrors generateAutoInstanceName.
func instancesGenerateAutoInstanceName(userID int32, countryCode *string) string {
	uidTail := fmt.Sprintf("%03d", userID)
	if len(uidTail) > 3 {
		uidTail = uidTail[len(uidTail)-3:]
	}
	random := instancesNanoid8()
	return instancesNameRegionPrefix(countryCode) + uidTail + "-" +
		instancesFormatNameTimestamp(time.Now()) + "-" + random[:4]
}

// ==================== change-host options ====================

type instancesChangeHostResources struct {
	CPUUsed         int32 `json:"cpuUsed"`
	CPUAllowanceMax int32 `json:"cpuAllowanceMax"`
	CPUAvailable    int32 `json:"cpuAvailable"`
	MemoryUsed      int32 `json:"memoryUsed"`
	MemoryMax       int32 `json:"memoryMax"`
	MemoryAvailable int32 `json:"memoryAvailable"`
}

type instancesChangeHostOption struct {
	ID                int32                        `json:"id"`
	Name              string                       `json:"name"`
	Location          *string                      `json:"location"`
	CountryCode       string                       `json:"countryCode"`
	Architecture      string                       `json:"architecture"`
	Status            string                       `json:"status"`
	ProbeURL          *string                      `json:"probeUrl"`
	IsCurrent         bool                         `json:"isCurrent"`
	CanChange         bool                         `json:"canChange"`
	UnavailableReason *string                      `json:"unavailableReason"`
	Resources         instancesChangeHostResources `json:"resources"`
	TrafficMultiplier float64                      `json:"trafficMultiplier"`
	EffectiveTraffic  *string                      `json:"effectiveTrafficLimit"`
}

type instancesChangeHostSSHKey struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Fingerprint *string `json:"fingerprint"`
}

type instancesChangeHostOptions struct {
	PackageID         *int32                      `json:"packageId"`
	PackageName       *string                     `json:"packageName"`
	CurrentHostID     int32                       `json:"currentHostId"`
	Required          instancesChangeHostRequired `json:"required"`
	Hosts             []instancesChangeHostOption `json:"hosts"`
	SSHKeys           []instancesChangeHostSSHKey `json:"sshKeys"`
	CanChangeHost     bool                        `json:"canChangeHost"`
	UnavailableReason *string                     `json:"unavailableReason,omitempty"`
}

type instancesChangeHostRequired struct {
	CPU    int32 `json:"cpu"`
	Memory int32 `json:"memory"`
}

// instancesHostCompatibleWithPackageType mirrors isHostCompatibleWithPackageInstanceType.
func instancesHostCompatibleWithPackageType(hostInstanceType *string, packageInstanceType string) bool {
	hostType := "container"
	if hostInstanceType != nil && *hostInstanceType != "" {
		hostType = *hostInstanceType
	}
	if packageInstanceType == "vm" {
		return hostType == "vm" || hostType == "both"
	}
	return hostType == "container" || hostType == "both"
}

// instancesBuildChangeHostOptions mirrors buildChangeHostOptions.
func instancesBuildChangeHostOptions(ctx context.Context, instance *db.InstanceDetailRecord) (instancesChangeHostOptions, error) {
	sshKeys, err := db.GetSSHKeysByUserID(ctx, instance.UserID)
	if err != nil {
		return instancesChangeHostOptions{}, err
	}

	base := instancesChangeHostOptions{
		CurrentHostID: instance.HostID,
		Required: instancesChangeHostRequired{
			CPU:    instance.CPU,
			Memory: instance.Memory,
		},
		Hosts:         []instancesChangeHostOption{},
		SSHKeys:       make([]instancesChangeHostSSHKey, 0, len(sshKeys)),
		CanChangeHost: false,
	}
	for _, key := range sshKeys {
		fingerprint := key.Fingerprint
		base.SSHKeys = append(base.SSHKeys, instancesChangeHostSSHKey{
			ID:          key.ID,
			Name:        key.Name,
			Fingerprint: &fingerprint,
		})
	}

	noPackage := "no_package"
	if instance.PackageID == nil || *instance.PackageID == 0 {
		base.UnavailableReason = &noPackage
		return base, nil
	}

	pkg, err := db.GetPackageById(ctx, *instance.PackageID)
	if err != nil {
		return instancesChangeHostOptions{}, err
	}
	if pkg == nil {
		base.UnavailableReason = &noPackage
		return base, nil
	}
	base.PackageID = &pkg.ID
	base.PackageName = &pkg.Name

	packageHostIDs := pkg.HostIDs
	if len(packageHostIDs) == 0 {
		singleHost := "single_host"
		base.UnavailableReason = &singleHost
		return base, nil
	}

	// instance sums per host (status<>'deleted'), exactly like the Prisma groupBy.
	type hostSum struct {
		HostID     int32
		CPU        int64
		Memory     int64
		Multiplier *float64
		CreatedAt  time.Time
	}
	var sums []hostSum
	err = db.DB.WithContext(ctx).Raw(`
		SELECT i.host_id AS host_id, COALESCE(SUM(i.cpu), 0) AS cpu,
		       COALESCE(SUM(i.memory), 0) AS memory,
		       ph.traffic_multiplier AS multiplier, ph.created_at AS created_at
		  FROM instances i
		  LEFT JOIN package_hosts ph ON ph.package_id = ? AND ph.host_id = i.host_id
		 WHERE i.host_id IN ? AND i.status <> 'deleted'
		 GROUP BY i.host_id, ph.traffic_multiplier, ph.created_at
		 ORDER BY ph.created_at ASC`,
		pkg.ID, packageHostIDs).Scan(&sums).Error
	if err != nil {
		return instancesChangeHostOptions{}, err
	}
	sumByHost := map[int32]struct {
		CPU    int64
		Memory int64
	}{}
	for _, row := range sums {
		sumByHost[row.HostID] = struct {
			CPU    int64
			Memory int64
		}{CPU: row.CPU, Memory: row.Memory}
	}

	hosts := make([]*db.HostsRecord, 0, len(packageHostIDs))
	for _, hostID := range packageHostIDs {
		host, hostErr := db.HostsGetByID(ctx, hostID)
		if hostErr != nil {
			return instancesChangeHostOptions{}, hostErr
		}
		if host == nil {
			continue
		}
		hosts = append(hosts, host)
	}
	multipliers := pkg.HostTrafficMultipliers

	packageInstanceType := pkg.InstanceType
	if packageInstanceType == "" {
		packageInstanceType = "container"
	}
	baseTrafficLimit := instancesBigIntStringToInt64(pkg.MonthlyTrafficLimit)
	if instance.PackagePlanID != nil && *instance.PackagePlanID != 0 {
		plan, planErr := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
		if planErr != nil {
			return instancesChangeHostOptions{}, planErr
		}
		if plan != nil && plan.PackageID == pkg.ID {
			limit := plan.TrafficLimit
			baseTrafficLimit = &limit
		}
	}

	options := make([]instancesChangeHostOption, 0, len(hosts))
	for _, host := range hosts {
		used := sumByHost[host.ID]
		cpuUsed := int32(used.CPU)
		if host.CPUUsed > cpuUsed {
			cpuUsed = host.CPUUsed
		}
		memoryUsed := int32(used.Memory)
		if host.MemoryUsed > memoryUsed {
			memoryUsed = host.MemoryUsed
		}
		cpuAllowanceMax := host.CPUAllowanceMax
		memoryMax := host.MemoryMax
		cpuAvailable := int32(0)
		if cpuAllowanceMax > 0 {
			cpuAvailable = cpuAllowanceMax - cpuUsed
			if cpuAvailable < 0 {
				cpuAvailable = 0
			}
		}
		memoryAvailable := int32(0)
		if memoryMax > 0 {
			memoryAvailable = memoryMax - memoryUsed
			if memoryAvailable < 0 {
				memoryAvailable = 0
			}
		}
		isCurrent := host.ID == instance.HostID

		var unavailableReason *string
		switch {
		case isCurrent:
			unavailableReason = instancesStringPtr("current_host")
		case host.Status != string(models.HostStatusOnline):
			unavailableReason = instancesStringPtr("host_offline")
		case !instancesHostCompatibleWithPackageType(&host.InstanceType, packageInstanceType):
			unavailableReason = instancesStringPtr("host_type_mismatch")
		case cpuAllowanceMax <= 0 || memoryMax <= 0:
			unavailableReason = instancesStringPtr("resource_unconfigured")
		case cpuAvailable < instance.CPU:
			unavailableReason = instancesStringPtr("cpu_full")
		case memoryAvailable < instance.Memory:
			unavailableReason = instancesStringPtr("memory_full")
		default:
			instanceType := packageInstanceType
			memory := int(instance.Memory)
			availability, availErr := db.GetSystemImageAvailabilityForHost(ctx, instance.Image, host.ID,
				db.SystemImageAvailabilityOptions{InstanceType: &instanceType, Memory: &memory})
			if availErr != nil {
				return instancesChangeHostOptions{}, availErr
			}
			if !availability.OK {
				unavailableReason = instancesStringPtr("image_unavailable")
			}
		}

		countryCode := host.CountryCode
		if countryCode == "" {
			countryCode = "us"
		}
		architecture := host.Architecture
		if architecture == "" {
			architecture = "x86_64"
		}
		multiplier := 1.0
		if value, ok := multipliers[strconv.FormatInt(int64(host.ID), 10)]; ok {
			normalized, normalizeErr := lib.NormalizeTrafficMultiplier(value)
			if normalizeErr == nil {
				multiplier = normalized
			}
		}

		options = append(options, instancesChangeHostOption{
			ID:                host.ID,
			Name:              host.Name,
			Location:          host.Location,
			CountryCode:       countryCode,
			Architecture:      architecture,
			Status:            host.Status,
			ProbeURL:          host.ProbeURL,
			IsCurrent:         isCurrent,
			CanChange:         unavailableReason == nil,
			UnavailableReason: unavailableReason,
			Resources: instancesChangeHostResources{
				CPUUsed:         cpuUsed,
				CPUAllowanceMax: cpuAllowanceMax,
				CPUAvailable:    cpuAvailable,
				MemoryUsed:      memoryUsed,
				MemoryMax:       memoryMax,
				MemoryAvailable: memoryAvailable,
			},
			TrafficMultiplier: multiplier,
			EffectiveTraffic:  instancesApplyTrafficMultiplier(baseTrafficLimit, multiplier),
		})
	}

	hasTarget := false
	for _, option := range options {
		if option.CanChange {
			hasTarget = true
			break
		}
	}
	sort.SliceStable(options, func(a, b int) bool {
		if options[a].IsCurrent != options[b].IsCurrent {
			return options[a].IsCurrent
		}
		if options[a].CanChange != options[b].CanChange {
			return options[a].CanChange
		}
		return strings.Compare(options[a].Name, options[b].Name) < 0
	})

	base.Hosts = options
	base.CanChangeHost = hasTarget && len(sshKeys) > 0
	switch {
	case len(packageHostIDs) <= 1:
		base.UnavailableReason = instancesStringPtr("single_host")
	case len(sshKeys) == 0:
		base.UnavailableReason = instancesStringPtr("no_ssh_key")
	}
	return base, nil
}

// instancesBigIntStringToInt64 mirrors `x ? BigInt(x) : null` for a numeric string.
func instancesBigIntStringToInt64(value *string) *int64 {
	if value == nil || *value == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(*value), 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

// instancesApplyTrafficMultiplier mirrors applyTrafficMultiplier + `.toString()`.
func instancesApplyTrafficMultiplier(base *int64, multiplier float64) *string {
	if base == nil {
		return nil
	}
	value, err := lib.ApplyTrafficMultiplier(base, multiplier)
	if err != nil || value == nil {
		return nil
	}
	formatted := strconv.FormatInt(*value, 10)
	return &formatted
}

// ==================== body validation ====================

// instancesIntArrayField validates `{type:'array', items:{type:'integer'}, maxItems:n}`
// with AJV coerceTypes semantics (single-element array unwrap, string→number).
func instancesValidateIntArray(raw any, maxItems int) ([]int32, bool) {
	if raw == nil {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		// AJV coerceTypes:'array' wraps a scalar into a one-element array.
		list = []any{raw}
	}
	if len(list) > maxItems {
		return nil, false
	}
	out := make([]int32, 0, len(list))
	for _, item := range list {
		number, ok := ajvCoerceInteger(item)
		if !ok {
			return nil, false
		}
		out = append(out, int32(number))
	}
	return out, true
}

// instancesBodyUtils groups the schema constructors used below.
func instancesStringField(name string, maxLength int) ajvFieldSpec {
	return ajvFieldSpec{Name: name, Kind: ajvKindString, MinLength: -1, MaxLength: maxLength, Minimum: math.NaN(), Maximum: math.NaN()}
}

func instancesIntegerField(name string, minimum, maximum float64) ajvFieldSpec {
	return ajvFieldSpec{Name: name, Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: minimum, Maximum: maximum}
}

func instancesBooleanField(name string) ajvFieldSpec {
	return ajvFieldSpec{Name: name, Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()}
}

func instancesEnumField(name string, values ...string) ajvFieldSpec {
	return ajvFieldSpec{Name: name, Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: values}
}

// ==================== route 1: list ====================

type instancesListQuery struct {
	Page       int
	PageSize   int
	Search     string
	UserID     *int32
	HostID     *int32
	Country    *string
	Status     *string
	SortBy     string
	SortOrder  string
	ShowUsers  bool
	Statuses   map[int32]hostsHostServiceStatus
	StatusSQL  string
	StatusArgs []any
}

// instancesListHandler mirrors GET /.
func instancesListHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	search := c.Query("search")

	page, _, pageValid := instancesParseOptionalPositiveQueryInteger(c.Query("page"), c.Query("page") != "")
	pageSizeRaw, pageSizeProvided, pageSizeValid := instancesParseOptionalPositiveQueryInteger(c.Query("pageSize"), c.Query("pageSize") != "")
	hostID, hostProvided, hostValid := instancesParseOptionalPositiveQueryInteger(c.Query("hostId"), c.Query("hostId") != "")
	queryUserID, userProvided, userValid := instancesParseOptionalPositiveQueryInteger(c.Query("userId"), c.Query("userId") != "")

	var status *string
	if raw := c.Query("status"); raw != "" {
		if !instancesListStatuses[raw] {
			pageValid = false
		} else {
			status = &raw
		}
	}

	if !pageValid || !pageSizeValid || !hostValid || !userValid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid instance list query")
		return
	}

	options := instancesListQuery{
		Page:     1,
		PageSize: 20,
		Search:   search,
		Status:   status,
		SortBy:   c.Query("sortBy"),
	}
	if page != 0 {
		options.Page = int(page)
	}
	if pageSizeProvided {
		size := int(pageSizeRaw)
		if size > instancesListMaxPageSize {
			size = instancesListMaxPageSize
		}
		options.PageSize = size
	}
	if sortOrder := c.Query("sortOrder"); sortOrder == "asc" || sortOrder == "desc" {
		options.SortOrder = sortOrder
	}
	if country := c.Query("countryCode"); country != "" {
		lowered := strings.ToLower(country)
		options.Country = &lowered
	}

	if claims.Role == "admin" {
		options.ShowUsers = true
		if userProvided {
			options.UserID = &queryUserID
		}
		if hostProvided {
			options.HostID = &hostID
		}
	} else if hostProvided {
		options.HostID = &hostID
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if host != nil && host.UserID == claims.ID {
			options.ShowUsers = true
		} else {
			options.UserID = &claims.ID
		}
	} else {
		options.UserID = &claims.ID
	}

	// The service-status overlay applies only to the three statuses it can rewrite.
	var filteredStatuses map[int32]hostsHostServiceStatus
	if options.Status != nil {
		switch *options.Status {
		case string(models.InstanceStatusRunning), string(models.InstanceStatusStopped), string(models.InstanceStatusError):
			statuses, err := hostsLoadHostServiceStatuses(ctx, nil)
			if err != nil {
				instancesRespondInternal(c, err)
				return
			}
			filteredStatuses = statuses
		}
	}

	result, err := instancesListPage(ctx, options, filteredStatuses)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// instancesRespondInternal mirrors the catch-all of the TypeScript handlers that
// answer 500 {error: message}.
func instancesRespondInternal(c *gin.Context, err error) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// ==================== route 2: reorder ====================

var instancesOrderActions = []string{"top", "up", "down", "bottom"}

// instancesOrderHandler mirrors PATCH /:id/order.
func instancesOrderHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	// The route declares a params schema (`id` pattern ^[1-9]\d*$), and Fastify
	// validates params before the body: an invalid id answers VALIDATION_ERROR with
	// the pattern message, not the handler's INVALID_ID.
	if !instancesPositiveRouteIDPattern.MatchString(c.Param("id")) {
		respondAJVValidation(c, "id format is invalid")
		return
	}
	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value,
		[]string{"action"},
		[]ajvFieldSpec{instancesEnumField("action", instancesOrderActions...)})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	action, _ := coerced.stringValue("action")

	var result struct {
		updated int
		found   bool
	}
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		type orderedRow struct {
			ID int32
		}
		var rows []orderedRow
		if err := tx.Raw(`SELECT id FROM instances
			WHERE user_id = ? AND status <> 'deleted'
			ORDER BY display_order ASC, created_at DESC, id ASC`, claims.ID).Scan(&rows).Error; err != nil {
			return err
		}
		currentIndex := -1
		for index, row := range rows {
			if row.ID == instanceID {
				currentIndex = index
				break
			}
		}
		if currentIndex < 0 {
			return nil
		}
		result.found = true
		target := currentIndex
		switch action {
		case "top":
			target = 0
		case "up":
			target = currentIndex - 1
		case "down":
			target = currentIndex + 1
		default:
			target = len(rows) - 1
		}
		if target < 0 {
			target = 0
		}
		if target > len(rows)-1 {
			target = len(rows) - 1
		}
		if target == currentIndex {
			return nil
		}
		next := make([]orderedRow, 0, len(rows))
		next = append(next, rows[:currentIndex]...)
		next = append(next, rows[currentIndex+1:]...)
		next = append(next[:target], append([]orderedRow{rows[currentIndex]}, next[target:]...)...)
		result.updated = len(next)
		now := time.Now().UTC()
		for index, row := range next {
			if err := tx.Exec(`UPDATE instances SET display_order = ?, updated_at = ? WHERE id = ?`,
				index*1000, now, row.ID).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !result.found {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Instance order updated", "updated": result.updated})
}

// ==================== route 3: available hosts ====================

// instancesAvailableHostsHandler mirrors GET /available-hosts.
func instancesAvailableHostsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)
	isAdmin := claims.Role == "admin"

	packageIDRaw := c.Query("packageId")
	if packageIDRaw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Package ID is required"})
		return
	}
	packageID, err := strconv.ParseInt(packageIDRaw, 10, 64)
	if err != nil || packageID <= 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	var planID *int32
	if raw, present := c.GetQuery("planId"); present && raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed <= 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		value := int32(parsed)
		planID = &value
	}

	cpu := 15
	if raw := c.Query("cpu"); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			cpu = parsed
		}
	}
	memory := 128
	if raw := c.Query("memory"); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			memory = parsed
		}
	}
	disk := 512
	if raw := c.Query("disk"); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			disk = parsed
		}
	}

	pkg, err := db.GetPackageById(ctx, int32(packageID))
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}

	if !isAdmin {
		canAccess, accessErr := db.CanUserAccessPackage(ctx, claims.ID, int32(packageID))
		if accessErr != nil {
			instancesRespondInternal(c, accessErr)
			return
		}
		if !canAccess {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
	}

	packageHostIDs := pkg.HostIDs
	if len(packageHostIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Package must bind at least one host", "code": "PACKAGE_NO_HOSTS"})
		return
	}

	var packageOwnerID *int32
	if pkg.UserID != claims.ID {
		ownerID := pkg.UserID
		packageOwnerID = &ownerID
	}

	unavailable := make([]gin.H, 0)
	hosts, listErr := instancesAvailableHosts(ctx, instancesAvailableHostsOptions{
		PackageHostIDs: packageHostIDs,
		CPU:            cpu,
		Memory:         memory,
		Disk:           disk,
		UserID:         claims.ID,
		PackageID:      int32(packageID),
		PackageOwnerID: packageOwnerID,
		OnUnavailable: func(hostID int32, reason string) {
			unavailable = append(unavailable, gin.H{"hostId": hostID, "reason": reason})
		},
	})
	if listErr != nil {
		instancesRespondInternal(c, listErr)
		return
	}

	baseTrafficLimit := instancesBigIntStringToInt64(pkg.MonthlyTrafficLimit)
	if planID != nil {
		plan, planErr := db.GetPackagePlanById(ctx, *planID)
		if planErr != nil {
			instancesRespondInternal(c, planErr)
			return
		}
		if plan != nil && plan.PackageID == int32(packageID) {
			limit := plan.TrafficLimit
			baseTrafficLimit = &limit
		}
	}

	hostTrafficMultipliers := pkg.HostTrafficMultipliers
	responseHosts := make([]gin.H, 0, len(hosts))
	for _, host := range hosts {
		normalized := 1.0
		if hostTrafficMultipliers != nil {
			if value, ok := hostTrafficMultipliers[strconv.FormatInt(int64(host.ID), 10)]; ok {
				if parsed, parseErr := lib.NormalizeTrafficMultiplier(value); parseErr == nil {
					normalized = parsed
				}
			}
		}
		cpuAllowanceMax := int32(0)
		if host.CPUAllowance != nil {
			cpuAllowanceMax = *host.CPUAllowance
		}
		cpuUsed := host.CPUUsed
		effectiveCPUMax := int32(0)
		if cpuAllowanceMax > 0 {
			effectiveCPUMax = cpuAllowanceMax
		}
		cpuAvailable := effectiveCPUMax - cpuUsed
		if cpuAvailable < 0 {
			cpuAvailable = 0
		}
		memoryMax := int32(0)
		if host.MemoryMax != nil {
			memoryMax = *host.MemoryMax
		}
		memoryUsed := host.MemoryUsed
		memoryAvailable := int32(0)
		if memoryMax > 0 {
			memoryAvailable = memoryMax - memoryUsed
			if memoryAvailable < 0 {
				memoryAvailable = 0
			}
		}
		countryCode := "us"
		if host.CountryCode != nil && *host.CountryCode != "" {
			countryCode = *host.CountryCode
		}
		architecture := "x86_64"
		if host.Architecture != nil && *host.Architecture != "" {
			architecture = *host.Architecture
		}
		responseHosts = append(responseHosts, gin.H{
			"id":            host.ID,
			"name":          host.Name,
			"location":      host.Location,
			"countryCode":   countryCode,
			"architecture":  architecture,
			"ownerId":       host.UserID,
			"ownerUsername": host.OwnerUsername,
			"isOwn":         host.IsOwn,
			"resources": gin.H{
				"cpuUsed":         cpuUsed,
				"cpuAllowanceMax": cpuAllowanceMax,
				"cpuEffectiveMax": effectiveCPUMax,
				"cpuAvailable":    cpuAvailable,
				"memoryUsed":      memoryUsed,
				"memoryMax":       memoryMax,
				"memoryAvailable": memoryAvailable,
				"diskTotal":       host.DiskTotal,
				"diskAvailable":   host.DiskAvailable,
			},
			"trafficMultiplier":     normalized,
			"effectiveTrafficLimit": instancesApplyTrafficMultiplier(baseTrafficLimit, normalized),
			"probeUrl":              host.ProbeURL,
		})
	}

	c.JSON(http.StatusOK, gin.H{"unavailableHosts": unavailable, "hosts": responseHosts})
}

// ==================== list query (port of db/pagination.ts getInstancesPaginated) ====================

type instancesListResult struct {
	Instances          []map[string]any `json:"instances"`
	Total              int64            `json:"total"`
	Page               int              `json:"page"`
	PageSize           int              `json:"pageSize"`
	TotalPages         int64            `json:"totalPages"`
	AvailableCountries []string         `json:"availableCountries"`
}

// instancesListEscapedLike mirrors escapeSqlLike.
func instancesListEscapedLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// instancesListBaseWhere mirrors the baseWhere assembly. `forCountries` drops the
// country filter (availableCountries is computed without it).
func instancesListBaseWhere(options instancesListQuery, statuses map[int32]hostsHostServiceStatus, forCountries bool) ([]string, []any) {
	where := make([]string, 0, 8)
	args := make([]any, 0, 12)

	switch {
	case statuses != nil && options.Status != nil:
		fragment, fragmentArgs := instancesServiceStatusFilterSQL(*options.Status, statuses)
		where = append(where, fragment)
		args = append(args, fragmentArgs...)
	case options.Status != nil && *options.Status == string(models.InstanceStatusDeleted):
		where = append(where, "i.status = ?")
		args = append(args, string(models.InstanceStatusDeleted))
	case options.Status != nil:
		where = append(where, "i.status = ?")
		args = append(args, *options.Status)
	default:
		where = append(where, "i.status <> ?")
		args = append(args, string(models.InstanceStatusDeleted))
	}

	if options.Search != "" {
		// Prisma `contains` is case-insensitive (mode:'insensitive'); it does not
		// escape LIKE wildcards, so neither does this port.
		pattern := "%" + options.Search + "%"
		conditions := []string{
			"i.name ILIKE ?", "i.incus_id ILIKE ?", "i.ipv4 ILIKE ?", "i.ipv6 ILIKE ?",
			"u.username ILIKE ?", "u.email ILIKE ?",
		}
		searchArgs := []any{pattern, pattern, pattern, pattern, pattern, pattern}
		trimmed := strings.TrimSpace(options.Search)
		if number, err := strconv.ParseInt(trimmed, 10, 64); err == nil && number > 0 && number <= math.MaxInt32 {
			// parseInt(search,10) then `searchNum.toString() === search.trim()`.
			conditions = append(conditions, "i.id = ?")
			searchArgs = append(searchArgs, number)
		}
		where = append(where, "("+strings.Join(conditions, " OR ")+")")
		args = append(args, searchArgs...)
	}

	if options.UserID != nil {
		where = append(where, "i.user_id = ?")
		args = append(args, *options.UserID)
	}
	if options.HostID != nil {
		where = append(where, "i.host_id = ?")
		args = append(args, *options.HostID)
	}
	if !forCountries && options.Country != nil {
		where = append(where, "i.host_id IN (SELECT id FROM hosts WHERE country_code = ?)")
		args = append(args, *options.Country)
	}
	return where, args
}

type instancesListRow struct {
	ID                   int32      `gorm:"column:id"`
	IncusID              string     `gorm:"column:incus_id"`
	Name                 string     `gorm:"column:name"`
	UserID               int32      `gorm:"column:user_id"`
	HostID               int32      `gorm:"column:host_id"`
	PackageID            *int32     `gorm:"column:package_id"`
	DisplayOrder         int32      `gorm:"column:display_order"`
	IconBadgeID          *string    `gorm:"column:icon_badge_id"`
	Image                string     `gorm:"column:image"`
	Status               string     `gorm:"column:status"`
	CPU                  int32      `gorm:"column:cpu"`
	Memory               int32      `gorm:"column:memory"`
	Disk                 int32      `gorm:"column:disk"`
	IPv4                 *string    `gorm:"column:ipv4"`
	IPv6                 *string    `gorm:"column:ipv6"`
	NetworkMode          string     `gorm:"column:network_mode"`
	SSHPort              *int32     `gorm:"column:ssh_port"`
	PortLimit            *int32     `gorm:"column:port_limit"`
	SnapshotLimit        *int32     `gorm:"column:snapshot_limit"`
	BackupLimit          *int32     `gorm:"column:backup_limit"`
	SiteLimit            *int32     `gorm:"column:site_limit"`
	MonthlyTrafficLimit  *int64     `gorm:"column:monthly_traffic_limit"`
	MonthlyTrafficUsed   int64      `gorm:"column:monthly_traffic_used"`
	LimitsIngress        *string    `gorm:"column:limits_ingress"`
	LimitsEgress         *string    `gorm:"column:limits_egress"`
	ExpiresAt            *time.Time `gorm:"column:expires_at"`
	AutoRenew            bool       `gorm:"column:auto_renew"`
	CreatedAt            time.Time  `gorm:"column:created_at"`
	PackagePlanID        *int32     `gorm:"column:package_plan_id"`
	BillingPrice         *float64   `gorm:"column:billing_price"`
	HostName             string     `gorm:"column:host_name"`
	HostLocation         *string    `gorm:"column:host_location"`
	HostCountryCode      string     `gorm:"column:host_country_code"`
	HostNATPublicIP      *string    `gorm:"column:host_nat_public_ip"`
	HostNATPublicIPv6    *string    `gorm:"column:host_nat_public_ipv6"`
	HostIPv6Gateway      *string    `gorm:"column:host_ipv6_gateway"`
	HostIPAddress        *string    `gorm:"column:host_ip_address"`
	HostURL              *string    `gorm:"column:host_url"`
	Username             *string    `gorm:"column:username"`
	UserEmail            *string    `gorm:"column:user_email"`
	UserAvatarStyle      *string    `gorm:"column:user_avatar_style"`
	UserAvatarBadgeID    *string    `gorm:"column:user_avatar_badge_id"`
	PackageName          *string    `gorm:"column:package_name"`
	PackageInstanceType  *string    `gorm:"column:package_instance_type"`
	PackageLimitsIngress *string    `gorm:"column:package_limits_ingress"`
	PackageLimitsEgress  *string    `gorm:"column:package_limits_egress"`
	PackageAllowDeletion *bool      `gorm:"column:package_allow_deletion"`
	PackageTrafficReset  *float64   `gorm:"column:package_traffic_reset_price"`
	PlanTrafficReset     *float64   `gorm:"column:plan_traffic_reset_price"`
}

// instancesListOrderBy mirrors the Prisma orderBy array.
func instancesListOrderBy(options instancesListQuery) string {
	if options.SortBy == "createdAt" {
		direction := "DESC"
		if options.SortOrder == "asc" {
			direction = "ASC"
		}
		return "i.created_at " + direction + ", i.id ASC"
	}
	if options.UserID != nil {
		return "i.display_order ASC, i.created_at DESC, i.id ASC"
	}
	return "i.created_at DESC, i.id ASC"
}

// instancesListPage runs the paginated list, the availableCountries aggregate and
// the presentation overlay.
func instancesListPage(ctx context.Context, options instancesListQuery, statuses map[int32]hostsHostServiceStatus) (instancesListResult, error) {
	where, args := instancesListBaseWhere(options, statuses, false)
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM instances i LEFT JOIN users u ON u.id = i.user_id `+whereSQL,
		args...).Scan(&total).Error; err != nil {
		return instancesListResult{}, err
	}

	countryWhere, countryArgs := instancesListBaseWhere(options, statuses, true)
	var countryCodes []*string
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT DISTINCT h.country_code FROM hosts h
		  WHERE EXISTS (SELECT 1 FROM instances i LEFT JOIN users u ON u.id = i.user_id
		                 WHERE i.host_id = h.id AND `+strings.Join(countryWhere, " AND ")+`)
		  ORDER BY h.country_code ASC`, countryArgs...).Scan(&countryCodes).Error; err != nil {
		return instancesListResult{}, err
	}
	availableCountries := make([]string, 0, len(countryCodes))
	seenCountry := map[string]bool{}
	for _, code := range countryCodes {
		normalized := "us"
		if code != nil && *code != "" {
			normalized = strings.ToLower(*code)
		}
		if seenCountry[normalized] {
			continue
		}
		seenCountry[normalized] = true
		availableCountries = append(availableCountries, normalized)
	}

	offset := (options.Page - 1) * options.PageSize
	rowsSQL := `SELECT i.id, i.incus_id, i.name, i.user_id, i.host_id, i.package_id,
			i.display_order, i.icon_badge_id, i.image, i.status, i.cpu, i.memory, i.disk,
			i.ipv4, i.ipv6, i.network_mode, i.ssh_port, i.port_limit, i.snapshot_limit,
			i.backup_limit, i.site_limit, i.monthly_traffic_limit, i.monthly_traffic_used,
			i.limits_ingress, i.limits_egress, i.expires_at, i.auto_renew, i.created_at,
			i.package_plan_id, i.billing_price,
			h.name AS host_name, h.location AS host_location, h.country_code AS host_country_code,
			h.nat_public_ip AS host_nat_public_ip, h.nat_public_ipv6 AS host_nat_public_ipv6,
			h.ipv6_gateway AS host_ipv6_gateway, h.ip_address AS host_ip_address, h.url AS host_url,
			u.username AS username, u.email AS user_email, u.avatar_style AS user_avatar_style,
			u.avatar_badge_id AS user_avatar_badge_id,
			p.name AS package_name, p.instance_type AS package_instance_type,
			p.limits_ingress AS package_limits_ingress, p.limits_egress AS package_limits_egress,
			p.allow_instance_deletion AS package_allow_deletion,
			p.traffic_reset_price AS package_traffic_reset_price,
			pp.traffic_reset_price AS plan_traffic_reset_price
		   FROM instances i
		   LEFT JOIN users u ON u.id = i.user_id
		   LEFT JOIN hosts h ON h.id = i.host_id
		   LEFT JOIN packages p ON p.id = i.package_id
		   LEFT JOIN package_plans pp ON pp.id = i.package_plan_id ` +
		whereSQL + ` ORDER BY ` + instancesListOrderBy(options) +
		` LIMIT ? OFFSET ?`

	queryArgs := append(append([]any{}, args...), options.PageSize, offset)
	var rows []instancesListRow
	if err := db.DB.WithContext(ctx).Raw(rowsSQL, queryArgs...).Scan(&rows).Error; err != nil {
		return instancesListResult{}, err
	}

	imageAliases := make([]string, 0, len(rows))
	for _, row := range rows {
		imageAliases = append(imageAliases, row.Image)
	}
	imageNames, err := db.GetImageDisplayNames(ctx, imageAliases)
	if err != nil {
		return instancesListResult{}, err
	}

	hostIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		hostIDs = append(hostIDs, row.HostID)
	}
	serviceStatuses := statuses
	if serviceStatuses == nil {
		serviceStatuses, err = hostsLoadHostServiceStatuses(ctx, hostIDs)
		if err != nil {
			return instancesListResult{}, err
		}
	}

	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		hostStatus := serviceStatuses[row.HostID]
		hostStatusPtr := &hostStatus
		hostIPAddress := row.HostIPAddress
		if (hostIPAddress == nil || *hostIPAddress == "") && row.HostURL != nil && *row.HostURL != "" {
			hostIPAddress = lib.ExtractHostAddressFromURL(row.HostURL)
		}
		hostCountry := row.HostCountryCode
		if hostCountry == "" {
			hostCountry = "us"
		}
		hostName := row.HostName
		if hostName == "" {
			hostName = "unknown"
		}
		instanceType := "container"
		if row.PackageInstanceType != nil && *row.PackageInstanceType == "vm" {
			instanceType = "vm"
		}
		limitsIngress := row.LimitsIngress
		if limitsIngress == nil {
			limitsIngress = row.PackageLimitsIngress
		}
		limitsEgress := row.LimitsEgress
		if limitsEgress == nil {
			limitsEgress = row.PackageLimitsEgress
		}
		trafficResetPrice := 0.0
		if row.PlanTrafficReset != nil {
			trafficResetPrice = *row.PlanTrafficReset
		} else if row.PackageTrafficReset != nil {
			trafficResetPrice = *row.PackageTrafficReset
		}
		allowDeletion := true
		if row.PackageAllowDeletion != nil {
			allowDeletion = *row.PackageAllowDeletion
		}
		monthlyTrafficUsed := "0"
		if row.MonthlyTrafficUsed != 0 {
			monthlyTrafficUsed = strconv.FormatInt(row.MonthlyTrafficUsed, 10)
		}
		var monthlyTrafficLimit *string
		if row.MonthlyTrafficLimit != nil {
			formatted := strconv.FormatInt(*row.MonthlyTrafficLimit, 10)
			monthlyTrafficLimit = &formatted
		}
		expiresAt := instancesOptionalISOTime(row.ExpiresAt)

		item := map[string]any{
			"id":        row.ID,
			"name":      row.Name,
			"incusId":   row.IncusID,
			"status":    instancesServiceStatus(row.Status, hostStatusPtr, row.IncusID),
			"image":     row.Image,
			"imageName": imageNames[row.Image],
			"cpu":       row.CPU,
			"memory":    row.Memory,
			"disk":      row.Disk,
			"ipv4":      row.IPv4,
			"ipv6":      row.IPv6,
			"host": gin.H{
				"name":          hostName,
				"country_code":  hostCountry,
				"nat_public_ip": row.HostNATPublicIP,
				"ip_address":    hostIPAddress,
			},
			"hostCountryCode":         hostCountry,
			"networkMode":             row.NetworkMode,
			"natPublicIp":             row.HostNATPublicIP,
			"hostNatPublicIpv6":       row.HostNATPublicIPv6,
			"hostIpv6Gateway":         row.HostIPv6Gateway,
			"hostIpAddress":           hostIPAddress,
			"displayOrder":            row.DisplayOrder,
			"createdAt":               instancesISOTime(row.CreatedAt),
			"expiresAt":               expiresAt,
			"expires_at":              expiresAt,
			"autoRenew":               row.AutoRenew,
			"iconBadgeId":             row.IconBadgeID,
			"packageName":             row.PackageName,
			"packagePlanId":           row.PackagePlanID,
			"billingPrice":            row.BillingPrice,
			"trafficResetPrice":       trafficResetPrice,
			"monthlyTrafficLimit":     monthlyTrafficLimit,
			"monthlyTrafficUsed":      monthlyTrafficUsed,
			"limitsIngress":           limitsIngress,
			"limitsEgress":            limitsEgress,
			"allow_instance_deletion": allowDeletion,
			"portLimit":               row.PortLimit,
			"snapshotLimit":           row.SnapshotLimit,
			"backupLimit":             row.BackupLimit,
			"siteLimit":               row.SiteLimit,
			"instanceType":            instanceType,
		}
		if imageName, ok := imageNames[row.Image]; ok && imageName != "" {
			item["imageName"] = imageName
		} else {
			item["imageName"] = nil
		}
		if options.ShowUsers {
			item["username"] = row.Username
			item["userEmail"] = row.UserEmail
			item["userAvatarStyle"] = row.UserAvatarStyle
			item["userAvatarBadgeId"] = row.UserAvatarBadgeID
			item["userId"] = row.UserID
			item["hostId"] = row.HostID
		}
		items = append(items, item)
	}

	totalPages := int64(0)
	if options.PageSize > 0 {
		totalPages = (total + int64(options.PageSize) - 1) / int64(options.PageSize)
	}
	return instancesListResult{
		Instances:          items,
		Total:              total,
		Page:               options.Page,
		PageSize:           options.PageSize,
		TotalPages:         totalPages,
		AvailableCountries: availableCountries,
	}, nil
}

// ==================== available hosts query (port of db/pagination.ts getAvailableHosts) ====================

type instancesAvailableHostsOptions struct {
	PackageHostIDs []int32
	CPU            int
	Memory         int
	Disk           int
	UserID         int32
	PackageID      int32
	PackageOwnerID *int32
	OnUnavailable  func(hostID int32, reason string)
}

type instancesAvailableHost struct {
	ID            int32   `gorm:"column:id"`
	UserID        int32   `gorm:"column:user_id"`
	OwnerUsername *string `gorm:"column:owner_username"`
	Name          string  `gorm:"column:name"`
	Location      *string `gorm:"column:location"`
	CountryCode   *string `gorm:"column:country_code"`
	Architecture  *string `gorm:"column:architecture"`
	Status        string  `gorm:"column:status"`
	CPUUsed       int32   `gorm:"column:cpu_used"`
	CPUAllowance  *int32  `gorm:"column:cpu_allowance_max"`
	MemoryUsed    int32   `gorm:"column:memory_used"`
	MemoryMax     *int32  `gorm:"column:memory_max"`
	ProbeURL      *string `gorm:"column:probe_url"`
	IsOwn         bool    `gorm:"column:is_own"`
	DiskTotal     int64   `gorm:"column:disk_total"`
	DiskAvailable int64   `gorm:"column:disk_available"`
}

// instancesAvailableHosts mirrors getAvailableHosts: online hosts limited to the
// package binding (or the package owner's online hosts), filtered by the CPU/memory
// allowance and by storage capacity, sorted by free memory descending.
func instancesAvailableHosts(ctx context.Context, options instancesAvailableHostsOptions) ([]instancesAvailableHost, error) {
	where := []string{"h.status = 'online'"}
	args := make([]any, 0, 4)
	if len(options.PackageHostIDs) > 0 {
		where = append(where, "h.id IN ?")
		args = append(args, options.PackageHostIDs)
	} else if options.PackageOwnerID != nil {
		where = append(where, "h.user_id = ?")
		args = append(args, *options.PackageOwnerID)
	} else {
		where = append(where, "h.user_id = ?")
		args = append(args, options.UserID)
	}

	type hostRow struct {
		ID            int32   `gorm:"column:id"`
		UserID        int32   `gorm:"column:user_id"`
		OwnerUsername *string `gorm:"column:owner_username"`
		Name          string  `gorm:"column:name"`
		Location      *string `gorm:"column:location"`
		CountryCode   *string `gorm:"column:country_code"`
		Architecture  *string `gorm:"column:architecture"`
		Status        string  `gorm:"column:status"`
		CPUAllowance  *int32  `gorm:"column:cpu_allowance_max"`
		MemoryMax     *int32  `gorm:"column:memory_max"`
		CPUUsed       int64   `gorm:"column:cpu_used"`
		MemoryUsed    int64   `gorm:"column:memory_used"`
		DiskUsed      int64   `gorm:"column:disk_used"`
		StorageSize   int32   `gorm:"column:storage_size"`
		ProbeURL      *string `gorm:"column:probe_url"`
	}
	var rows []hostRow
	err := db.DB.WithContext(ctx).Raw(`
		SELECT h.id, h.user_id, u.username AS owner_username, h.name, h.location,
		       h.country_code, h.architecture, h.status, h.cpu_allowance_max, h.memory_max,
		       COALESCE(SUM(i.cpu), 0) AS cpu_used, COALESCE(SUM(i.memory), 0) AS memory_used,
		       COALESCE(SUM(i.disk), 0) AS disk_used, h.storage_size, h.probe_url
		  FROM hosts h
		  LEFT JOIN users u ON u.id = h.user_id
		  LEFT JOIN instances i ON i.host_id = h.id AND i.status <> 'deleted'
		 WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY h.id, u.username`, args...).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	candidates := make([]hostRow, 0, len(rows))
	for _, row := range rows {
		cpuAllowance := int32(0)
		if row.CPUAllowance != nil {
			cpuAllowance = *row.CPUAllowance
		}
		// No CPU allowance configured means "unavailable": the panel requires an
		// explicit quota (same prohibition as the TypeScript).
		if cpuAllowance <= 0 || row.CPUUsed+int64(options.CPU) > int64(cpuAllowance) {
			continue
		}
		memoryMax := int32(0)
		if row.MemoryMax != nil {
			memoryMax = *row.MemoryMax
		}
		if memoryMax <= 0 || row.MemoryUsed+int64(options.Memory) > int64(memoryMax) {
			continue
		}
		candidates = append(candidates, row)
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		leftMax := int64(0)
		if candidates[a].MemoryMax != nil {
			leftMax = int64(*candidates[a].MemoryMax)
		}
		rightMax := int64(0)
		if candidates[b].MemoryMax != nil {
			rightMax = int64(*candidates[b].MemoryMax)
		}
		return (rightMax-candidates[b].MemoryUsed)-(leftMax-candidates[a].MemoryUsed) < 0
	})

	out := make([]instancesAvailableHost, 0, len(candidates))
	for _, row := range candidates {
		storage, storageErr := lib.GetStorageCapacity(ctx, row.ID, lib.StorageCapacityOptions{
			PackageID:  instancesInt32Ptr(options.PackageID),
			RequiredMB: instancesInt64Ptr(int64(options.Disk)),
		})
		if storageErr != nil {
			return nil, storageErr
		}
		if storage.Reason != nil {
			if options.OnUnavailable != nil {
				options.OnUnavailable(row.ID, string(*storage.Reason))
			}
			continue
		}
		out = append(out, instancesAvailableHost{
			ID:            row.ID,
			UserID:        row.UserID,
			OwnerUsername: row.OwnerUsername,
			Name:          row.Name,
			Location:      row.Location,
			CountryCode:   row.CountryCode,
			Architecture:  row.Architecture,
			Status:        row.Status,
			CPUUsed:       int32(row.CPUUsed),
			CPUAllowance:  row.CPUAllowance,
			MemoryUsed:    int32(row.MemoryUsed),
			MemoryMax:     row.MemoryMax,
			ProbeURL:      row.ProbeURL,
			IsOwn:         row.UserID == options.UserID,
			DiskTotal:     storage.TotalMB,
			DiskAvailable: storage.AvailableMB,
		})
	}
	return out, nil
}

// ==================== route 4: create ====================

// instancesCreateAbort carries an already-formed HTTP response out of the create
// transaction and the post-commit provisioning step.
type instancesCreateAbort struct {
	Status  int
	Payload any
}

// Error implements error.
func (e *instancesCreateAbort) Error() string { return "instance create aborted" }

// instancesDecimalFromJSNumber mirrors Prisma's Decimal.set(number): the value is
// converted through JS number→string, i.e. the shortest round-trip representation.
func instancesDecimalFromJSNumber(value float64) models.Decimal {
	parsed, err := models.NewDecimalFromString(strconv.FormatFloat(value, 'f', -1, 64))
	if err != nil {
		return models.Decimal{}
	}
	return parsed
}

// instancesCreateRequest is the validated body of POST /.
type instancesCreateRequest struct {
	Name                 string
	PackageID            int32
	PlanID               *int32
	Image                string
	CPU                  *int32
	Memory               *int32
	Disk                 *int32
	HostID               int32
	SSHKeyID             *int32
	SSHKey               *string
	Password             *string
	CustomInitCommandIDs []int32
	PromoCode            *string
	IdempotencyKey       *string
	TurnstileToken       *string
	AutoRemotePort       *bool
}

// instancesCreatePlan is everything the transaction needs, computed by the
// pre-flight phase.
type instancesCreatePlan struct {
	UserID              int32
	PackageID           int32
	Package             *db.PackageDetailRow
	Plan                *models.PackagePlan
	Name                string
	IncusID             string
	Image               string
	CPU                 int32
	Memory              int32
	Disk                int32
	NetworkMode         string
	InstanceType        string
	PackagePlanID       *int32
	SnapshotSpecs       []byte
	MetaData            lib.IncusConfigMetaData
	RootPasswordEnc     *string
	Billing             *db.CreateBillingResult
	ActualPrice         float64
	DiscountAmount      float64
	PricingSource       services.VipPriceSource
	PromoValidation     *services.PromoValidationResult
	AffValidation       *db.AffValidateCodeResult
	PlanBandwidthLimit  *string
	PackageTrafficLimit *int64
	RequiredPackageID   *int32
	ShareInfo           *db.PackageShareForUser
	HostID              int32
	IdempotencyKey      *string
	IsOwnPackage        bool
	RawCPU              int32
	RawMemory           int32
}

// instancesCreateResult is the outcome of the transaction.
type instancesCreateResult struct {
	InstanceID int32
	Host       *db.HostsSelectionRecord
	// Bandwidth limit resolved inside the transaction (nil means "use package values").
	PlanBandwidthLimit *string
}

// instancesCreateHandler mirrors POST /.
func instancesCreateHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value,
		[]string{"packageId", "image", "hostId"},
		[]ajvFieldSpec{
			instancesStringField("name", 64),
			instancesIntegerField("packageId", math.NaN(), math.NaN()),
			instancesStringField("image", -1),
			instancesIntegerField("cpu", 15, 10000),
			instancesIntegerField("memory", 128, 524288),
			instancesIntegerField("disk", 512, 104857600),
			instancesIntegerField("hostId", 1, math.NaN()),
			instancesIntegerField("sshKeyId", math.NaN(), math.NaN()),
			instancesStringField("sshKey", -1),
			instancesStringField("password", -1),
			instancesIntegerField("planId", 1, math.NaN()),
			instancesStringField("idempotencyKey", 128),
			instancesStringField("turnstileToken", 2048),
			instancesStringField("promoCode", 32),
			instancesBooleanField("autoRemotePort"),
		})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	request := instancesCreateRequest{
		Name:           strings.TrimSpace(instancesCoercedString(coerced, "name")),
		Image:          instancesCoercedString(coerced, "image"),
		PackageID:      int32(instancesCoercedNumber(coerced, "packageId")),
		HostID:         int32(instancesCoercedNumber(coerced, "hostId")),
		SSHKey:         instancesCoercedOptionalString(coerced, "sshKey"),
		Password:       instancesCoercedOptionalString(coerced, "password"),
		PromoCode:      instancesCoercedOptionalString(coerced, "promoCode"),
		IdempotencyKey: instancesCoercedOptionalString(coerced, "idempotencyKey"),
		TurnstileToken: instancesCoercedOptionalString(coerced, "turnstileToken"),
	}
	if value, present := coerced.Values["cpu"]; present && value != nil {
		request.CPU = instancesInt32Ptr(int32(value.(float64)))
	}
	if value, present := coerced.Values["memory"]; present && value != nil {
		request.Memory = instancesInt32Ptr(int32(value.(float64)))
	}
	if value, present := coerced.Values["disk"]; present && value != nil {
		request.Disk = instancesInt32Ptr(int32(value.(float64)))
	}
	if value, present := coerced.Values["planId"]; present && value != nil {
		request.PlanID = instancesInt32Ptr(int32(value.(float64)))
	}
	if value, present := coerced.Values["sshKeyId"]; present && value != nil {
		request.SSHKeyID = instancesInt32Ptr(int32(value.(float64)))
	}
	if value, present := coerced.Values["autoRemotePort"]; present {
		if boolean, ok := value.(bool); ok {
			request.AutoRemotePort = instancesBoolPtr(boolean)
		}
	}
	if raw, present := body.Value.(map[string]any)["customInitCommandIds"]; present {
		ids, ok := instancesValidateIntArray(raw, 20)
		if !ok {
			respondAJVValidation(c, "must be array")
			return
		}
		request.CustomInitCommandIDs = ids
	}

	normalPaidIdempotencyKey := (*string)(nil)
	if request.IdempotencyKey != nil {
		trimmed := strings.TrimSpace(*request.IdempotencyKey)
		if trimmed != "" {
			normalPaidIdempotencyKey = &trimmed
		}
	}

	// 0. Turnstile (Fastify validates the body before the handler, so the token is
	// readable from the already-parsed body). decodeJSONRequestBody consumed the
	// stream, so the decoded document is re-encoded as the request body first —
	// lib.TurnstileVerifier reads the token from the JSON body exactly like
	// fastify's request.body.
	if encoded, marshalErr := json.Marshal(body.Value); marshalErr == nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(encoded))
		c.Request.ContentLength = int64(len(encoded))
	}
	lib.TurnstileVerifier(c)
	if c.IsAborted() {
		return
	}

	// 0.1 Authentication material: SSH key or password.
	sshKey := request.SSHKey
	if request.SSHKeyID != nil && (sshKey == nil || *sshKey == "") {
		keyRecord, err := db.GetSSHKeyByID(ctx, *request.SSHKeyID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if keyRecord == nil || keyRecord.UserID != claims.ID {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSshKeyNotOwned, "")
			return
		}
		sshKey = &keyRecord.PublicKey
	}

	effectivePassword := ""
	if request.Password != nil && strings.TrimSpace(*request.Password) != "" {
		trimmed := strings.TrimSpace(*request.Password)
		passwordCheck := lib.ValidatePassword(trimmed)
		if !passwordCheck.Valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPasswordTooWeak, passwordCheck.Message)
			return
		}
		lowered := strings.ToLower(trimmed)
		for _, weak := range []string{"password", "admin123", "root123", "12345678", "qwertyuiop"} {
			if strings.Contains(lowered, weak) {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPasswordTooWeak, "Password cannot contain common weak patterns")
				return
			}
		}
		effectivePassword = trimmed
	} else {
		effectivePassword = lib.GenerateRandomPassword(16)
	}

	// 1. Package availability and access.
	pkg, err := db.GetPackageById(ctx, request.PackageID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if pkg == nil || pkg.Active == 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPackageUnavailable, "")
		return
	}

	canAccess, err := db.CanUserAccessPackage(ctx, claims.ID, request.PackageID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !canAccess {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	requiredPackageID := pkg.RequiredPackageID
	if requiredPackageID != nil {
		hasRequired, reqErr := db.UserHasNormalInstanceForPackage(ctx, claims.ID, *requiredPackageID, nil)
		if reqErr != nil {
			instancesRespondInternal(c, reqErr)
			return
		}
		if !hasRequired {
			requiredName := fmt.Sprintf("#%d", *requiredPackageID)
			if pkg.RequiredPackageName != nil && *pkg.RequiredPackageName != "" {
				requiredName = *pkg.RequiredPackageName
			}
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPackagePrerequisiteMissing,
				"必须持有 "+requiredName+" 实例才可以开通本套餐的实例")
			return
		}
	}

	// 1.2 Shared-package quota pre-check (fast fail; re-checked in the transaction).
	isOwnPackage := pkg.UserID == claims.ID
	var shareInfo *db.PackageShareForUser
	if !isOwnPackage {
		shareInfo, err = db.GetPackageShareForUser(ctx, request.PackageID, claims.ID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if shareInfo != nil {
			if shareInfo.MaxInstances != nil && *shareInfo.MaxInstances == 0 {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrShareQuotaInstancesExceeded,
					"该套餐的实例数量限制为 0，不允许创建实例")
				return
			}
			usage, usageErr := db.GetSharedPackageUsage(ctx, request.PackageID, claims.ID)
			if usageErr != nil {
				instancesRespondInternal(c, usageErr)
				return
			}
			if shareInfo.MaxInstances != nil && *shareInfo.MaxInstances > 0 &&
				int32(usage.InstanceCount) >= *shareInfo.MaxInstances {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrShareQuotaInstancesExceeded,
					fmt.Sprintf("已开通 %d/%d 台", usage.InstanceCount, *shareInfo.MaxInstances))
				return
			}
			if shareInfo.QuotaMultiplier != nil {
				maxCPU := int32(math.Floor(float64(pkg.CPUMax) * *shareInfo.QuotaMultiplier))
				maxMemory := int32(math.Floor(float64(pkg.MemoryMax) * *shareInfo.QuotaMultiplier))
				requestedCPUCheck := int32(15)
				if request.CPU != nil {
					requestedCPUCheck = *request.CPU
				}
				requestedMemoryCheck := int32(128)
				if request.Memory != nil {
					requestedMemoryCheck = *request.Memory
				}
				if int32(usage.TotalCPU)+requestedCPUCheck > maxCPU {
					lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrShareQuotaCpuExceeded,
						fmt.Sprintf("已使用 %d%%，限额 %d%%", usage.TotalCPU, maxCPU))
					return
				}
				if int32(usage.TotalMemory)+requestedMemoryCheck > maxMemory {
					lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrShareQuotaMemoryExceeded,
						fmt.Sprintf("已使用 %dMB，限额 %dMB", usage.TotalMemory, maxMemory))
					return
				}
			}
		}
	}

	// 1.3 Paid plan validation.
	var selectedPlan *models.PackagePlan
	packageIsPaid, err := db.IsPaidPackage(ctx, request.PackageID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	isHostOwnerCreatingFree := false
	if isOwnPackage && request.HostID != 0 && request.PlanID == nil {
		specifiedHost, hostErr := db.HostsGetByID(ctx, request.HostID)
		if hostErr != nil {
			instancesRespondInternal(c, hostErr)
			return
		}
		if specifiedHost != nil && specifiedHost.UserID == claims.ID {
			isHostOwnerCreatingFree = true
		}
	}

	if packageIsPaid && !isHostOwnerCreatingFree {
		if request.PlanID == nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPlanRequired, "该套餐为付费套餐，必须选择一个方案")
			return
		}
		selectedPlan, err = db.GetPackagePlanById(ctx, *request.PlanID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if selectedPlan == nil || selectedPlan.PackageID != request.PackageID {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPlanNotFound, "方案不存在或不属于该套餐")
			return
		}
		if !selectedPlan.IsActive {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPlanInactive, "方案已下架")
			return
		}
		if selectedPlan.IsSoldOut {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPlanSoldOut, "方案已售罄")
			return
		}
	} else if request.PlanID != nil && !isHostOwnerCreatingFree {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "该套餐为免费套餐，不需要选择方案")
		return
	}

	if isOwnPackage && packageIsPaid && !isHostOwnerCreatingFree {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotCreateOwnPaidPackage, "不能使用自己的付费套餐创建实例")
		return
	}

	if selectedPlan != nil && normalPaidIdempotencyKey != nil {
		replay, replayErr := instancesFindNormalPaidCreateReplay(ctx, claims.ID, *normalPaidIdempotencyKey)
		if replayErr != nil {
			instancesRespondInternal(c, replayErr)
			return
		}
		if replay != nil {
			c.JSON(http.StatusOK, replay)
			return
		}
	}

	// 1.4 Price, promo code, VIP arbitration and balance check.
	var billing *db.CreateBillingResult
	var validatedAffCode *db.AffValidateCodeResult
	var promoValidation *services.PromoValidationResult
	var promoQuote *services.PromoCreationQuoteResult
	discountAmount := 0.0
	actualPrice := 0.0
	pricingSource := services.VipPriceSourceBase

	if selectedPlan == nil && request.PromoCode != nil && strings.TrimSpace(*request.PromoCode) != "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "免费套餐不能使用优惠码")
		return
	}

	if selectedPlan != nil {
		calculated := db.CalculateCreateBilling(*selectedPlan, time.Now().UTC())
		billing = &calculated

		if request.PromoCode != nil && strings.TrimSpace(*request.PromoCode) != "" {
			code := strings.TrimSpace(*request.PromoCode)
			validation, promoErr := services.PromoValidate(ctx, services.PromoValidateParams{
				Code:          code,
				PackageID:     pkg.ID,
				PackagePlanID: selectedPlan.ID,
				UserID:        claims.ID,
			})
			if promoErr != nil {
				instancesRespondInternal(c, promoErr)
				return
			}
			if validation.Valid {
				promoValidation = &validation
				quote := services.PromoQuoteCreation(billing.TotalPrice, services.PromoQuotePromo{
					DiscountType:  *validation.DiscountType,
					DiscountValue: models.Decimal{},
				})
				// PromoQuotePromo carries a Decimal discount value; reuse the code's
				// own value so percentage and fixed discounts both work.
				if validation.PromoCode != nil {
					quote = services.PromoQuoteCreation(billing.TotalPrice, services.PromoQuotePromo{
						DiscountType:  validation.PromoCode.DiscountType,
						DiscountValue: validation.PromoCode.DiscountValue,
					})
				}
				promoQuote = &quote
			} else if validation.ErrorCode == "PROMO_NOT_FOUND" {
				affValidation, affErr := db.AffValidateCode(ctx, code, selectedPlan.ID, claims.ID)
				if affErr != nil {
					instancesRespondInternal(c, affErr)
					return
				}
				if !affValidation.Valid {
					details := affValidation.Error
					if details == "" {
						details = validation.Error
					}
					if details == "" {
						details = "优惠码无效"
					}
					lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, details)
					return
				}
				validatedAffCode = &affValidation
			} else {
				details := validation.Error
				if details == "" {
					details = "优惠码无效"
				}
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, details)
				return
			}
		}

		var affDiscountRate *float64
		if promoQuote != nil {
			rate := 0.0
			if billing.TotalPrice > 0 {
				rate = promoQuote.DiscountAmount / billing.TotalPrice
			}
			affDiscountRate = &rate
		} else if validatedAffCode != nil && validatedAffCode.DiscountRate != nil {
			affDiscountRate = validatedAffCode.DiscountRate
		}

		_, vipBenefit, vipErr := services.VipGetUserContinuousVipBenefit(ctx, claims.ID, nil)
		if vipErr != nil {
			instancesRespondInternal(c, vipErr)
			return
		}
		vipPercent := vipBenefit.OrderDiscountPercent
		decision := services.VipArbitratePrice(services.VipPriceArbitrationInput{
			BasePrice:          billing.TotalPrice,
			AffDiscountRate:    affDiscountRate,
			VipDiscountPercent: &vipPercent,
		})
		actualPrice = decision.FinalPrice
		discountAmount = decision.DiscountAmount
		pricingSource = decision.Source

		userBalance, balanceErr := db.GetUserBalance(ctx, claims.ID)
		if balanceErr != nil {
			instancesRespondInternal(c, balanceErr)
			return
		}
		if userBalance < actualPrice {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrBalanceInsufficient,
				fmt.Sprintf("余额不足，需要 ¥%.2f，当前余额 ¥%.2f", actualPrice, userBalance))
			return
		}
	}

	// 2. Requested resources.
	requestedCPU := int32(15)
	if request.CPU != nil {
		requestedCPU = *request.CPU
	}
	requestedMemory := int32(128)
	if request.Memory != nil {
		requestedMemory = *request.Memory
	}
	requestedDisk := int32(512)
	if request.Disk != nil {
		requestedDisk = *request.Disk
	}
	if selectedPlan != nil {
		requestedCPU = selectedPlan.CPU
		requestedMemory = selectedPlan.Memory
		requestedDisk = selectedPlan.Disk
	} else {
		if requestedCPU > pkg.CPUMax {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
				fmt.Sprintf("CPU 不能超过套餐上限 %d%%", pkg.CPUMax))
			return
		}
		if requestedMemory > pkg.MemoryMax {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
				fmt.Sprintf("内存不能超过套餐上限 %dMB", pkg.MemoryMax))
			return
		}
		if requestedDisk > pkg.DiskMax {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
				fmt.Sprintf("磁盘不能超过套餐上限 %dMB", pkg.DiskMax))
			return
		}
	}

	// 3. Authentication material is mandatory.
	if (sshKey == nil || *sshKey == "") && effectivePassword == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSshKeyRequired, "")
		return
	}

	// 5. Image checks.
	validImage, err := db.IsValidSystemImage(ctx, request.Image)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !validImage {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageNotFound, "")
		return
	}
	pkgInstanceType := pkg.InstanceType
	if pkgInstanceType == "" {
		pkgInstanceType = "container"
	}
	typeCompatible, err := db.IsImageCompatibleWithInstanceType(ctx, request.Image, pkgInstanceType)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !typeCompatible {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageTypeMismatch, "")
		return
	}
	memoryCompatible, err := db.IsImageCompatibleWithMemory(ctx, request.Image, int(requestedMemory))
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !memoryCompatible {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageMemoryIncompatible, "")
		return
	}

	// Phase 2: host pre-selection.
	packageHostIDs := pkg.HostIDs
	nodeSelectors := []string{}
	hostPreCheck, err := db.HostsSelectAvailable(ctx, db.HostsSelectOptions{
		PackageHostIDs: packageHostIDs,
		NodeSelectors:  nodeSelectors,
		CPU:            int64(requestedCPU),
		Memory:         int64(requestedMemory),
		Disk:           int64(requestedDisk),
		HostID:         &request.HostID,
		OwnerID:        &pkg.UserID,
		PackageID:      &pkg.ID,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if hostPreCheck == nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostUnavailable, "")
		return
	}

	hostInstanceType := hostPreCheck.InstanceType
	if hostInstanceType == "" {
		hostInstanceType = "container"
	}
	if pkgInstanceType == "vm" && hostInstanceType == "container" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostInstanceTypeMismatch, "")
		return
	}
	if pkgInstanceType == "container" && hostInstanceType == "vm" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostInstanceTypeMismatch, "")
		return
	}

	imageAvailability, err := db.GetSystemImageAvailabilityForHost(ctx, request.Image, hostPreCheck.ID,
		db.SystemImageAvailabilityOptions{InstanceType: &pkgInstanceType, Memory: instancesIntPtr(int(requestedMemory))})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !imageAvailability.OK {
		switch imageAvailability.Reason {
		case "host_not_found":
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		case "image_not_found":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageNotFound, "")
		case "memory_incompatible":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageMemoryIncompatible, "")
		case "instance_type_mismatch":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageTypeMismatch, "")
		case "host_instance_type_mismatch":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostInstanceTypeMismatch, "")
		default:
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceImageUnavailable, "")
		}
		return
	}

	name := request.Name
	if name == "" {
		countryCode := hostPreCheck.CountryCode
		var countryPtr *string
		if countryCode != "" {
			countryPtr = &countryCode
		}
		name = instancesGenerateAutoInstanceName(claims.ID, countryPtr)
	}
	nameValidation := lib.ValidateName(name, "Instance name", 2, 64)
	if !nameValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": nameValidation.Message})
		return
	}

	networkMode := lib.NormalizeNetworkMode(pkg.NetworkMode)
	if pkgInstanceType == "vm" &&
		(networkMode == lib.NetworkModeNatIpv6Nat || networkMode == lib.NetworkModeIpv6Nat) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			"KVM instances do not support IPv4 NAT & IPv6 NAT or IPv6 NAT package network modes")
		return
	}

	// Phase 2.5: custom init commands.
	extraShellCommands := ""
	if len(request.CustomInitCommandIDs) > 0 {
		commandValidation, cmdErr := db.ValidateCustomInitCommandsOwnership(ctx, request.CustomInitCommandIDs, claims.ID)
		if cmdErr != nil {
			instancesRespondInternal(c, cmdErr)
			return
		}
		if !commandValidation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid custom init command IDs", "code": "INVALID_COMMAND_IDS"})
			return
		}
		imageDistro := db.GetImageDistroFromAlias(request.Image)
		compatible := make([]db.CustomInitCommandData, 0, len(commandValidation.Commands))
		for _, command := range commandValidation.Commands {
			for _, distro := range command.Distros {
				if distro == "all" || distro == imageDistro {
					compatible = append(compatible, command)
					break
				}
			}
		}
		if len(compatible) > 0 {
			extraShellCommands = db.MergeCustomInitCommandContents(compatible)
		}
	}

	// Phase 3: cloud-init payload.
	instanceType := "container"
	if pkgInstanceType == "vm" {
		instanceType = "virtual-machine"
	}
	generated, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName:       name,
		ImageAlias:         request.Image,
		RootPassword:       effectivePassword,
		SSHKey:             sshKey,
		NetworkMode:        string(networkMode),
		Type:               instanceType,
		ExtraShellCommands: extraShellCommands,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}

	// Phase 4: identifiers.
	incusID := fmt.Sprintf("u%d-%s", claims.ID, instancesNanoid8())

	snapshotSpecs := map[string]any{
		"packageId":     pkg.ID,
		"packageName":   pkg.Name,
		"cpuMax":        pkg.CPUMax,
		"memoryMax":     pkg.MemoryMax,
		"diskMax":       pkg.DiskMax,
		"networkMode":   string(networkMode),
		"portLimit":     instancesPortLimitOrDefault(pkg.PortLimit, 20),
		"nested":        pkg.Nested != 0,
		"privileged":    pkg.Privileged != 0,
		"nodeSelectors": nodeSelectors,
		"createdAt":     instancesISOTime(time.Now().UTC()),
	}
	snapshotJSON, err := json.Marshal(snapshotSpecs)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}

	if selectedPlan != nil {
		normalizedSpeed := services.TrafficBandwidthNormalizePlanTrafficLimitSpeed(instancesStringPtr(selectedPlan.TrafficLimitSpeed))
		if normalizedSpeed.Invalid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid plan traffic limit speed")
			return
		}
	}

	encryptedPassword, err := lib.EncryptSensitiveData(generated.MetaData.RootPassword)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}

	plan := instancesCreatePlan{
		UserID:              claims.ID,
		PackageID:           pkg.ID,
		Package:             pkg,
		Plan:                selectedPlan,
		Name:                name,
		IncusID:             incusID,
		Image:               request.Image,
		CPU:                 requestedCPU,
		Memory:              requestedMemory,
		Disk:                requestedDisk,
		NetworkMode:         string(networkMode),
		InstanceType:        pkgInstanceType,
		SnapshotSpecs:       snapshotJSON,
		MetaData:            generated.MetaData,
		RootPasswordEnc:     &encryptedPassword,
		Billing:             billing,
		ActualPrice:         actualPrice,
		DiscountAmount:      discountAmount,
		PricingSource:       pricingSource,
		PromoValidation:     promoValidation,
		AffValidation:       validatedAffCode,
		PackageTrafficLimit: instancesBigIntStringToInt64(pkg.MonthlyTrafficLimit),
		RequiredPackageID:   requiredPackageID,
		ShareInfo:           shareInfo,
		IsOwnPackage:        isOwnPackage,
		RawCPU:              requestedCPU,
		RawMemory:           requestedMemory,
		HostID:              request.HostID,
	}
	if selectedPlan != nil {
		plan.PackagePlanID = &selectedPlan.ID
		plan.IdempotencyKey = normalPaidIdempotencyKey
	}

	result, err := instancesCreateTransaction(ctx, plan)
	if err != nil {
		var abort *instancesCreateAbort
		if errors.As(err, &abort) {
			c.JSON(abort.Status, abort.Payload)
			return
		}
		var gpuErr *db.GpusError
		if errors.As(err, &gpuErr) {
			c.JSON(http.StatusConflict, gin.H{"error": gpuErr.Message, "code": "GPU_RESOURCES_INSUFFICIENT"})
			return
		}
		if normalPaidIdempotencyKey != nil && instancesIsUniqueViolation(err) {
			replay, replayErr := instancesFindNormalPaidCreateReplay(ctx, claims.ID, *normalPaidIdempotencyKey)
			if replayErr == nil && replay != nil {
				c.JSON(http.StatusOK, replay)
				return
			}
			c.JSON(http.StatusConflict, gin.H{
				"error": "开通请求已提交，请勿重复操作",
				"code":  "INSTANCE_CREATE_DUPLICATE_REQUEST",
			})
			return
		}
		instancesRespondInternal(c, err)
		return
	}

	// Phase 6: post-transaction IP allocation, storage pool and async dispatch.
	postInput := instancesProvisionInput{
		InstanceID:         result.InstanceID,
		Host:               result.Host,
		UserID:             claims.ID,
		Plan:               plan,
		PlanBandwidthLimit: result.PlanBandwidthLimit,
		BaseConfigPayload:  generated.ConfigPayload,
		ExtraShellCommands: extraShellCommands,
		SSHKey:             sshKey,
	}

	provisioned, provisionErr := instancesPostCommitProvisioning(c, postInput)
	if provisionErr != nil {
		return
	}

	createConfig := instancesBuildCreateConfig(plan, provisioned, request.AutoRemotePort)
	queued, err := instancesEnqueueCreateTaskIfNodeUnavailable(ctx, result.InstanceID, result.Host.ID, claims.ID, createConfig,
		instancesResources{CPU: requestedCPU, Memory: requestedMemory, Disk: requestedDisk})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !queued {
		go func() {
			if err := instancesCreateInstanceAsync(context.Background(), result.InstanceID, result.Host, createConfig, claims.ID,
				instancesResources{CPU: requestedCPU, Memory: requestedMemory, Disk: requestedDisk}); err != nil {
				log.Printf("[Provisioning] 实例 %d 创建失败: %v", result.InstanceID, err)
			}
		}()
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": func() string {
			if queued {
				return "Instance queued, waiting for the node to become available"
			}
			return "Instance creating, please refresh later"
		}(),
		"queued": queued,
		"instance": gin.H{
			"id":           result.InstanceID,
			"name":         name,
			"incusId":      incusID,
			"host":         result.Host.Name,
			"status":       "creating",
			"sshPort":      generated.MetaData.SSHPort,
			"rootPassword": generated.MetaData.RootPassword,
		},
	})
}

// ==================== create helpers ====================

func instancesCoercedString(values ajvValidated, name string) string {
	text, _ := values.Values[name].(string)
	return text
}

func instancesCoercedOptionalString(values ajvValidated, name string) *string {
	raw, present := values.Values[name]
	if !present {
		return nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil
	}
	return &text
}

func instancesCoercedNumber(values ajvValidated, name string) float64 {
	raw, present := values.Values[name]
	if !present {
		return 0
	}
	number, _ := raw.(float64)
	return number
}

func instancesPortLimitOrDefault(value int32, fallback int32) int32 {
	if value == 0 {
		return fallback
	}
	return value
}

// instancesLoadNodeSelectors mirrors JSON.parse(pkg.node_selectors || '[]').
func instancesLoadNodeSelectors(ctx context.Context, packageID int32) ([]string, error) {
	var raw *string
	if err := db.DB.WithContext(ctx).Raw(`SELECT node_selectors FROM packages WHERE id = ?`, packageID).Scan(&raw).Error; err != nil {
		return nil, err
	}
	if raw == nil || *raw == "" || *raw == "null" {
		return []string{}, nil
	}
	var selectors []string
	if err := json.Unmarshal([]byte(*raw), &selectors); err != nil {
		return nil, err
	}
	return selectors, nil
}

// instancesFindNormalPaidCreateReplay mirrors findNormalPaidCreateReplay.
func instancesFindNormalPaidCreateReplay(ctx context.Context, userID int32, idempotencyKey string) (gin.H, error) {
	var row struct {
		ID      int32
		Name    string
		IncusID *string
		UserID  int32
		HostID  int32
		Status  string
		SSHPort *int32
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id, name, incus_id, user_id, host_id, status, ssh_port
		FROM instances WHERE idempotency_key = ? LIMIT 1`, idempotencyKey).Scan(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == 0 || row.UserID != userID {
		return nil, nil
	}
	host, err := db.HostsGetByID(ctx, row.HostID)
	if err != nil {
		return nil, err
	}
	hostName := ""
	if host != nil {
		hostName = host.Name
	}
	return gin.H{
		"message": "Instance request already accepted",
		"instance": gin.H{
			"id":      row.ID,
			"name":    row.Name,
			"incusId": row.IncusID,
			"host":    hostName,
			"status":  row.Status,
			"sshPort": row.SSHPort,
		},
	}, nil
}

// instancesIsUniqueViolation mirrors isUniqueConstraintError (P2002 → SQLSTATE 23505).
func instancesIsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key value") || strings.Contains(message, "unique constraint")
}

func instancesAbortWith(status int, code lib.ErrorCode, details string) *instancesCreateAbort {
	return &instancesCreateAbort{Status: status, Payload: lib.APIErrorFor(code, details)}
}

// instancesTxBindPackageEgress mirrors db's private instancesBindPackageEgress on a
// GORM transaction. It must share the instance INSERT transaction: the 74201 lock is
// transaction-scoped and the resolved policy commits with the row.
func instancesTxBindPackageEgress(ctx context.Context, tx *gorm.DB, packageID *int32, hostID int32, sourceInstanceID *int32) (db.InstanceEgressBinding, error) {
	if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(74201)).Error; err != nil {
		return db.InstanceEgressBinding{}, err
	}

	binding := db.InstanceEgressBinding{EgressMode: "host"}
	sourcePresent := sourceInstanceID != nil && *sourceInstanceID != 0
	var source *db.InstanceEgressBinding
	if sourcePresent {
		var row struct {
			EgressMode      string
			EgressProfileID *int32
		}
		err := tx.WithContext(ctx).Raw(`SELECT egress_mode, egress_profile_id FROM instances WHERE id = ?`,
			*sourceInstanceID).Scan(&row).Error
		if err != nil {
			return db.InstanceEgressBinding{}, err
		}
		if row.EgressMode != "" {
			source = &db.InstanceEgressBinding{EgressMode: row.EgressMode, EgressProfileID: row.EgressProfileID}
		}
	}

	packagePresent := packageID != nil && *packageID != 0
	var pkg *instancesPackageEgressPolicy
	if packagePresent {
		var policy instancesPackageEgressPolicy
		err := tx.WithContext(ctx).Raw(`SELECT egress_mode, egress_profile_id, network_mode,
				instance_type, privileged FROM packages WHERE id = ?`, *packageID).
			Scan(&policy).Error
		if err != nil {
			return db.InstanceEgressBinding{}, err
		}
		if policy.EgressMode != "" {
			pkg = &policy
		}
	}

	if (sourcePresent && source == nil) || (packagePresent && pkg == nil && source == nil) {
		return db.InstanceEgressBinding{}, &db.InstanceEgressInputError{Message: "Egress binding source no longer exists"}
	}
	if source != nil {
		binding = *source
	} else if pkg != nil {
		binding = db.InstanceEgressBinding{EgressMode: pkg.EgressMode, EgressProfileID: pkg.EgressProfileID}
	}

	if binding.EgressMode != "host" {
		if db.InstanceEgressHostSupportsControl == nil ||
			!db.InstanceEgressHostSupportsControl(hostID, "network.apply.v2") {
			return db.InstanceEgressBinding{}, &db.InstanceEgressInputError{
				Message: "The host needs an online Agent supporting package egress",
			}
		}
		if source == nil {
			if err := instancesTxValidatePackageEgress(ctx, tx, pkg, []int32{hostID}); err != nil {
				return db.InstanceEgressBinding{}, err
			}
		}
		if source != nil && source.EgressProfileID != nil && *source.EgressProfileID != 0 {
			var row struct {
				Active        bool
				AllowedHostID []int32
			}
			err := tx.WithContext(ctx).Raw(`SELECT active, allowed_host_ids FROM egress_profiles WHERE id = ?`,
				*source.EgressProfileID).Scan(&row).Error
			if err != nil {
				return db.InstanceEgressBinding{}, err
			}
			permitted := false
			for _, allowed := range row.AllowedHostID {
				if allowed == hostID {
					permitted = true
					break
				}
			}
			if !row.Active || !permitted {
				return db.InstanceEgressBinding{}, &db.InstanceEgressInputError{
					Message: "The instance egress profile is not permitted on the target host",
				}
			}
		}
	}
	return binding, nil
}

// instancesPackageEgressPolicy is the package projection both egress helpers read.
type instancesPackageEgressPolicy struct {
	EgressMode      string
	EgressProfileID *int32
	NetworkMode     string
	InstanceType    string
	Privileged      bool
}

// instancesTxValidatePackageEgress mirrors db's private validatePackageEgress.
func instancesTxValidatePackageEgress(ctx context.Context, tx *gorm.DB, pkg *instancesPackageEgressPolicy, hostIDs []int32) error {
	if pkg == nil {
		return &db.InstanceEgressInputError{Message: "Invalid package egress mode"}
	}
	egressMode := pkg.EgressMode
	if egressMode == "" {
		egressMode = "host"
	}
	switch egressMode {
	case "host", "direct", "profile":
	default:
		return &db.InstanceEgressInputError{Message: "Invalid package egress mode"}
	}
	if egressMode != "profile" && pkg.EgressProfileID != nil {
		return &db.InstanceEgressInputError{Message: "Only proxy mode may select an egress profile"}
	}
	if egressMode == "host" {
		return nil
	}
	networkMode := pkg.NetworkMode
	if networkMode == "" {
		networkMode = "nat"
	}
	instanceType := pkg.InstanceType
	if instanceType == "" {
		instanceType = "container"
	}
	if networkMode != "nat" || instanceType != "container" || pkg.Privileged {
		return &db.InstanceEgressInputError{
			Message: "Package egress currently requires an unprivileged IPv4 NAT container",
		}
	}
	if egressMode == "profile" {
		permitted := false
		if pkg.EgressProfileID != nil {
			var row struct {
				Active        bool
				AllowedHostID []int32
			}
			err := tx.WithContext(ctx).Raw(`SELECT active, allowed_host_ids FROM egress_profiles WHERE id = ?`,
				*pkg.EgressProfileID).Scan(&row).Error
			if err != nil {
				return err
			}
			if row.Active {
				permitted = true
				for _, hostID := range hostIDs {
					found := false
					for _, allowed := range row.AllowedHostID {
						if allowed == hostID {
							found = true
							break
						}
					}
					if !found {
						permitted = false
						break
					}
				}
			}
		}
		if !permitted {
			return &db.InstanceEgressInputError{
				Message: "Egress profile is inactive or not permitted on the selected hosts",
			}
		}
	}
	return nil
}

// instancesTxPackageShareForUser mirrors db.GetPackageShareForUser on a transaction
// (the db helper has no client parameter, so the in-transaction re-check would
// otherwise read outside the transaction).
func instancesTxPackageShareForUser(ctx context.Context, tx *gorm.DB, packageID, userID int32) (*db.PackageShareForUser, error) {
	var pkg struct {
		ID                 int32
		GlobalShared       bool
		GlobalMaxInstances *int32
		UserID             int32
	}
	if err := tx.WithContext(ctx).Raw(`SELECT id, global_shared, global_max_instances, user_id
		FROM packages WHERE id = ? LIMIT 1`, packageID).Scan(&pkg).Error; err != nil {
		return nil, err
	}
	if pkg.ID == 0 {
		return nil, nil
	}
	if pkg.GlobalShared && pkg.UserID != userID {
		maxInstances := int32(1)
		if pkg.GlobalMaxInstances != nil && *pkg.GlobalMaxInstances > 0 {
			maxInstances = *pkg.GlobalMaxInstances
		}
		return &db.PackageShareForUser{ID: packageID, MaxInstances: &maxInstances, IsGlobalShared: true}, nil
	}
	if pkg.UserID != userID {
		var share struct {
			ID              int32
			QuotaMultiplier *models.Decimal
			MaxInstances    *int32
		}
		if err := tx.WithContext(ctx).Raw(`SELECT id, quota_multiplier, max_instances
			FROM package_shares WHERE package_id = ? AND shared_to_id = ? LIMIT 1`,
			packageID, userID).Scan(&share).Error; err != nil {
			return nil, err
		}
		if share.ID != 0 {
			var multiplier *float64
			if share.QuotaMultiplier != nil {
				value := share.QuotaMultiplier.InexactFloat64()
				multiplier = &value
			}
			return &db.PackageShareForUser{
				ID:              share.ID,
				QuotaMultiplier: multiplier,
				MaxInstances:    share.MaxInstances,
			}, nil
		}
	}
	return nil, nil
}

// instancesTxSharedPackageUsage mirrors db.GetSharedPackageUsage on a transaction.
func instancesTxSharedPackageUsage(ctx context.Context, tx *gorm.DB, packageID, userID int32) (db.SharedPackageUsage, error) {
	var row struct {
		InstanceCount int   `gorm:"column:instance_count"`
		TotalCPU      int64 `gorm:"column:total_cpu"`
		TotalMemory   int64 `gorm:"column:total_memory"`
	}
	err := tx.WithContext(ctx).Raw(`SELECT COUNT(*) AS instance_count,
			COALESCE(SUM(cpu), 0) AS total_cpu, COALESCE(SUM(memory), 0) AS total_memory
		FROM instances WHERE user_id = ? AND package_id = ? AND status IN ('running', 'stopped')`,
		userID, packageID).Scan(&row).Error
	if err != nil {
		return db.SharedPackageUsage{}, err
	}
	return db.SharedPackageUsage{InstanceCount: row.InstanceCount, TotalCPU: row.TotalCPU, TotalMemory: row.TotalMemory}, nil
}

// instancesCreateTransaction implements the atomic reserve→insert→charge sequence.
func instancesCreateTransaction(ctx context.Context, plan instancesCreatePlan) (*instancesCreateResult, error) {
	var result *instancesCreateResult
	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 0. Shared egress policy lock (74201, single-argument form).
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(74201)).Error; err != nil {
			return err
		}

		// 1. Row-lock the host and reserve its resources.
		nodeSelectors, err := instancesLoadNodeSelectors(ctx, plan.PackageID)
		if err != nil {
			return err
		}
		portCount := int32(0)
		if lib.NetworkModeAllowsPortMapping(plan.NetworkMode) {
			portCount = plan.Package.PortLimit
		}
		reserveOptions := db.HostsReserveOptions{
			PackageHostIDs: plan.Package.HostIDs,
			NodeSelectors:  nodeSelectors,
			CPU:            int64(plan.CPU),
			Memory:         int64(plan.Memory),
			Disk:           int64(plan.Disk),
			OwnerID:        &plan.Package.UserID,
			PackageID:      &plan.PackageID,
			PortCount:      portCount,
			PackagePlanID:  plan.PackagePlanID,
			NetworkMode:    &plan.NetworkMode,
			HostID:         &plan.HostID,
		}
		lockedHost, err := db.HostsSelectAndReserveWithLock(ctx, tx, reserveOptions)
		if err != nil {
			return err
		}
		if lockedHost == nil {
			return instancesAbortWith(http.StatusServiceUnavailable, lib.ErrHostResourcesInsufficient,
				"宿主机资源不足或已被其他请求占用，请稍后重试")
		}

		// 2. Re-check the package prerequisite inside the transaction.
		if plan.RequiredPackageID != nil {
			hasRequired, reqErr := db.UserHasNormalInstanceForPackage(ctx, plan.UserID, *plan.RequiredPackageID, tx)
			if reqErr != nil {
				return reqErr
			}
			if !hasRequired {
				requiredName := fmt.Sprintf("#%d", *plan.RequiredPackageID)
				if plan.Package.RequiredPackageName != nil && *plan.Package.RequiredPackageName != "" {
					requiredName = *plan.Package.RequiredPackageName
				}
				return instancesAbortWith(http.StatusBadRequest, lib.ErrPackagePrerequisiteMissing,
					"必须持有 "+requiredName+" 实例才可以开通本套餐的实例")
			}
		}

		// 3. Re-check the shared-package quota inside the transaction.
		if !plan.IsOwnPackage {
			txShareInfo, shareErr := instancesTxPackageShareForUser(ctx, tx, plan.PackageID, plan.UserID)
			if shareErr != nil {
				return shareErr
			}
			if txShareInfo != nil {
				txUsage, usageErr := instancesTxSharedPackageUsage(ctx, tx, plan.PackageID, plan.UserID)
				if usageErr != nil {
					return usageErr
				}
				if txShareInfo.MaxInstances != nil {
					if *txShareInfo.MaxInstances == 0 {
						return instancesAbortWith(http.StatusBadRequest, lib.ErrShareQuotaInstancesExceeded,
							"该套餐的实例数量限制为 0，不允许创建实例")
					}
					if int32(txUsage.InstanceCount) >= *txShareInfo.MaxInstances {
						return instancesAbortWith(http.StatusBadRequest, lib.ErrShareQuotaInstancesExceeded,
							fmt.Sprintf("已开通 %d/%d 台", txUsage.InstanceCount, *txShareInfo.MaxInstances))
					}
				}
				if txShareInfo.QuotaMultiplier != nil {
					maxCPU := int32(math.Floor(float64(plan.Package.CPUMax) * *txShareInfo.QuotaMultiplier))
					maxMemory := int32(math.Floor(float64(plan.Package.MemoryMax) * *txShareInfo.QuotaMultiplier))
					if int32(txUsage.TotalCPU)+plan.RawCPU > maxCPU {
						return instancesAbortWith(http.StatusBadRequest, lib.ErrShareQuotaCpuExceeded,
							fmt.Sprintf("已使用 %d%%，限额 %d%%", txUsage.TotalCPU, maxCPU))
					}
					if int32(txUsage.TotalMemory)+plan.RawMemory > maxMemory {
						return instancesAbortWith(http.StatusBadRequest, lib.ErrShareQuotaMemoryExceeded,
							fmt.Sprintf("已使用 %dMB，限额 %dMB", txUsage.TotalMemory, maxMemory))
					}
				}
			}
		}

		// 4. Egress binding (advisory lock 74201 already held).
		binding, err := instancesTxBindPackageEgress(ctx, tx, &plan.PackageID, lockedHost.ID, nil)
		if err != nil {
			return err
		}

		// 5. Effective monthly traffic limit (plan limit × host multiplier).
		baseTrafficLimit := plan.PackageTrafficLimit
		if plan.Plan != nil {
			limit := plan.Plan.TrafficLimit
			baseTrafficLimit = &limit
		}
		effectiveTrafficLimit, err := lib.ResolveInstanceTrafficLimitForHost(ctx, tx, lib.TrafficMultiplierResolveParams{
			PackageID:        &plan.PackageID,
			HostID:           lockedHost.ID,
			BaseTrafficLimit: baseTrafficLimit,
		})
		if err != nil {
			return err
		}

		// 6. Quota profile: paid plan values win, otherwise package values.
		var portLimit, snapshotLimit, backupLimit, siteLimit *int32
		var swapSize *int32
		var limitsIngress, limitsEgress *string
		if plan.Plan != nil {
			portLimit = instancesInt32Ptr(plan.Plan.PortLimit)
			snapshotLimit = instancesInt32Ptr(plan.Plan.SnapshotLimit)
			backupLimit = instancesInt32Ptr(plan.Plan.BackupLimit)
			siteLimit = instancesInt32Ptr(plan.Plan.SiteLimit)
			if plan.InstanceType == "container" && plan.Plan.SwapSize != 0 {
				swapSize = instancesInt32Ptr(plan.Plan.SwapSize)
			}
			normalized := services.TrafficBandwidthNormalizePlanTrafficLimitSpeed(instancesStringPtr(plan.Plan.TrafficLimitSpeed))
			if normalized.Value != nil {
				limitsIngress = normalized.Value
				limitsEgress = normalized.Value
				plan.PlanBandwidthLimit = normalized.Value
			}
		} else {
			if plan.Package.PortLimit != 0 {
				portLimit = instancesInt32Ptr(plan.Package.PortLimit)
			}
			if plan.Package.SnapshotLimit != 0 {
				snapshotLimit = instancesInt32Ptr(plan.Package.SnapshotLimit)
			}
			if plan.Package.BackupLimit != 0 {
				backupLimit = instancesInt32Ptr(plan.Package.BackupLimit)
			}
			if plan.Package.SiteLimit != 0 {
				siteLimit = instancesInt32Ptr(plan.Package.SiteLimit)
			}
		}

		// 7. GPU reservation (shared seats are locked FOR UPDATE inside).
		gpuReservation, err := db.GpusReserveInstanceGpu(ctx, tx, db.GpusReserveInput{
			HostID:    lockedHost.ID,
			PackageID: &plan.PackageID,
			PlanID:    plan.PackagePlanID,
		})
		if err != nil {
			return err
		}

		// 8. Intent row: status='creating'. Mirrors db.CreateInstance's INSERT, but on
		// the caller's transaction so reserve + insert + charge stay atomic.
		now := time.Now().UTC()
		var expiresAt *time.Time
		var billingPrice *models.Decimal
		var billingCycle *int32
		if plan.Billing != nil {
			value := plan.Billing.ExpiresAt.UTC()
			expiresAt = &value
			price := instancesDecimalFromJSNumber(plan.Billing.Price)
			billingPrice = &price
			cycle := plan.Billing.BillingCycle
			billingCycle = &cycle
		}
		var idempotencyKey *string
		if plan.Plan != nil {
			idempotencyKey = plan.IdempotencyKey
		}
		var instanceID int32
		if err := tx.Raw(`INSERT INTO instances (
				egress_mode, egress_profile_id, incus_id, idempotency_key, name, user_id, host_id,
				storage_pool_name, package_id, package_plan_id, gpu_device_id, image, cpu, memory, disk,
				network_mode, snapshotted_specs, ssh_port, root_password, status,
				monthly_traffic_limit, port_limit, snapshot_limit, backup_limit, site_limit,
				swap_enabled, swap_size, limits_ingress, limits_egress, expires_at, billing_price,
				billing_cycle, auto_renew, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'creating',
				?, ?, ?, ?, ?, false, ?, ?, ?, ?, ?, ?, false, ?, ?)
			RETURNING id`,
			binding.EgressMode, binding.EgressProfileID, plan.IncusID, idempotencyKey, plan.Name,
			plan.UserID, lockedHost.ID, lockedHost.StoragePoolName, plan.PackageID, plan.PackagePlanID,
			gpuReservation.GpuDeviceID, plan.Image, plan.CPU, plan.Memory, plan.Disk,
			plan.NetworkMode, string(plan.SnapshotSpecs), plan.MetaData.SSHPort, plan.RootPasswordEnc,
			effectiveTrafficLimit, portLimit, snapshotLimit, backupLimit, siteLimit,
			swapSize, limitsIngress, limitsEgress, expiresAt, billingPrice, billingCycle, now, now,
		).Scan(&instanceID).Error; err != nil {
			return err
		}
		for _, gpuID := range gpuReservation.GpuDeviceIDs {
			if err := tx.Exec(`INSERT INTO instance_gpu_allocations (instance_id, gpu_device_id)
				VALUES (?, ?) ON CONFLICT DO NOTHING`, instanceID, gpuID).Error; err != nil {
				return err
			}
		}

		// 9. Charge the balance (paid plans only), then record the purchase.
		if plan.Plan != nil && plan.Billing != nil {
			locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, plan.UserID)
			if lockErr != nil {
				return lockErr
			}
			if !locked {
				return instancesAbortWith(http.StatusConflict, lib.ErrBalanceInsufficient, "余额正在处理，请稍后重试")
			}

			var balanceBefore models.Decimal
			if err := tx.Raw(`SELECT balance FROM users WHERE id = ?`, plan.UserID).Scan(&balanceBefore).Error; err != nil {
				return err
			}
			if plan.ActualPrice > 0 && balanceBefore.InexactFloat64() < plan.ActualPrice {
				return instancesAbortWith(http.StatusBadRequest, lib.ErrBalanceInsufficient, "余额不足")
			}
			priceDecimal := instancesDecimalFromJSNumber(plan.ActualPrice)
			update := tx.Exec(`UPDATE users SET balance = balance - ?, updated_at = ? WHERE id = ? AND balance >= ?`,
				priceDecimal, time.Now().UTC(), plan.UserID, priceDecimal)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected == 0 {
				return instancesAbortWith(http.StatusBadRequest, lib.ErrBalanceInsufficient, "余额不足或并发冲突")
			}
			var balanceAfter models.Decimal
			if err := tx.Raw(`SELECT balance FROM users WHERE id = ?`, plan.UserID).Scan(&balanceAfter).Error; err != nil {
				return err
			}

			remark := fmt.Sprintf("开通实例（%d个月）：%s - %s", plan.Billing.BillingCycle, plan.Package.Name, plan.Plan.Name)
			if plan.DiscountAmount > 0 {
				remark += fmt.Sprintf("，优惠码折扣 -¥%.2f", plan.DiscountAmount)
			}
			balanceLogID, logErr := instancesTxCreateBalanceLog(tx, plan.UserID, models.BalanceLogTypeConsume,
				instancesNegDecimal(priceDecimal), balanceBefore, balanceAfter, instanceID, remark)
			if logErr != nil {
				return logErr
			}

			billingRemark := fmt.Sprintf("新开通 %d 个月", plan.Billing.BillingCycle)
			if plan.DiscountAmount > 0 {
				billingRemark = fmt.Sprintf("新开通 %d 个月，优惠码折扣 -¥%.2f", plan.Billing.BillingCycle, plan.DiscountAmount)
			}
			if err := tx.Exec(`INSERT INTO instance_billing_records
					(instance_id, user_id, type, amount, months, period_start, period_end, balance_log_id, remark, created_at)
				VALUES (?, ?, 'newPurchase', ?, ?, ?, ?, ?, ?, ?)`,
				instanceID, plan.UserID, priceDecimal, plan.Billing.BillingCycle, now,
				plan.Billing.ExpiresAt.UTC(), balanceLogID, billingRemark, now).Error; err != nil {
				return err
			}

			if plan.PromoValidation != nil && plan.PromoValidation.PromoCode != nil {
				promoDiscount := 0.0
				if plan.PricingSource == services.VipPriceSourceAff {
					promoDiscount = plan.DiscountAmount
				}
				originalPrice := plan.Billing.TotalPrice
				commissionRate := plan.PromoValidation.PromoCode.CommissionRate.InexactFloat64()
				durationType := models.PromoDurationTypeFOREVER
				if plan.PromoValidation.DurationType != nil {
					durationType = *plan.PromoValidation.DurationType
				}
				if _, settleErr := services.PromoSettleOnCreate(ctx, tx, services.PromoSettleCreateParams{
					PromoCodeID:    plan.PromoValidation.PromoCode.ID,
					InstanceID:     instanceID,
					UserID:         plan.UserID,
					DurationType:   durationType,
					DurationCycles: plan.PromoValidation.DurationCycles,
					CommissionRate: &commissionRate,
					Quote: &services.PromoSettleCreateQuote{
						DiscountAmount: promoDiscount,
						FinalPrice:     plan.ActualPrice,
						OriginalPrice:  &originalPrice,
					},
				}); settleErr != nil {
					return settleErr
				}
			} else if plan.AffValidation != nil && plan.AffValidation.AffCode != nil {
				if err := tx.Exec(`INSERT INTO aff_bindings (instance_id, aff_code_id) VALUES (?, ?)`,
					instanceID, plan.AffValidation.AffCode.ID).Error; err != nil {
					return err
				}
				if err := instancesTxAffCommission(ctx, tx, plan.AffValidation.AffCode.ID, instanceID,
					plan.Billing.Price, models.AffLogTypeNewPurchase); err != nil {
					return err
				}
			}
		}

		result = &instancesCreateResult{
			InstanceID:         instanceID,
			Host:               lockedHost,
			PlanBandwidthLimit: plan.PlanBandwidthLimit,
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if txErr != nil {
		return nil, txErr
	}
	if result == nil {
		return nil, errors.New("instance create transaction produced no result")
	}
	return result, nil
}

// instancesTxCreateBalanceLog inserts one balance_logs snapshot row.
func instancesTxCreateBalanceLog(tx *gorm.DB, userID int32, logType models.BalanceLogType, amount,
	before, after models.Decimal, instanceID int32, remark string) (int32, error) {
	var id int32
	err := tx.Raw(`INSERT INTO balance_logs
			(user_id, type, amount, balance_before, balance_after, instance_id, remark, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		userID, string(logType), amount, before, after, instanceID, remark, time.Now().UTC()).Scan(&id).Error
	return id, err
}

// instancesTxAffCommission mirrors db.AffProcessCommission on a GORM transaction
// (the db helper takes pgx.Tx). Semantics are identical: 2-decimal JS rounding, the
// 4112 advisory lock, an aff_logs row and the code's used_count/total_earnings move.
func instancesTxAffCommission(ctx context.Context, tx *gorm.DB, affCodeID, instanceID int32, originalAmount float64, logType models.AffLogType) error {
	var code struct {
		ID             int32
		UserID         int32
		CommissionRate models.Decimal
	}
	if err := tx.WithContext(ctx).Raw(`SELECT id, user_id, commission_rate FROM aff_codes WHERE id = ?`,
		affCodeID).Scan(&code).Error; err != nil {
		return err
	}
	if code.ID == 0 {
		return nil
	}
	commission := math.Round(originalAmount*code.CommissionRate.InexactFloat64()*100) / 100
	if commission <= 0 {
		return nil
	}
	remark := "续费返利"
	if logType == models.AffLogTypeNewPurchase {
		remark = "新购返利"
	}
	if err := db.AdvisoryTransactionLockTx(tx, db.UserAffBalanceLockNamespace, code.UserID); err != nil {
		return err
	}
	var affBalance models.Decimal
	if err := tx.WithContext(ctx).Raw(`SELECT aff_balance FROM users WHERE id = ?`, code.UserID).Scan(&affBalance).Error; err != nil {
		return err
	}
	commissionDecimal := instancesDecimalFromJSNumber(commission)
	if err := tx.Exec(`UPDATE users SET aff_balance = aff_balance + ?, updated_at = ? WHERE id = ?`,
		commissionDecimal, time.Now().UTC(), code.UserID).Error; err != nil {
		return err
	}
	var after models.Decimal
	if err := tx.WithContext(ctx).Raw(`SELECT aff_balance FROM users WHERE id = ?`, code.UserID).Scan(&after).Error; err != nil {
		return err
	}
	if err := tx.Exec(`INSERT INTO aff_logs
			(user_id, type, amount, aff_code_id, instance_id, original_amount, balance_before, balance_after, remark, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code.UserID, string(logType), commissionDecimal, affCodeID, instanceID, originalAmount,
		affBalance, after, remark, time.Now().UTC()).Error; err != nil {
		return err
	}
	incrementUsed := false
	if logType == models.AffLogTypeNewPurchase {
		incrementUsed = true
	}
	if incrementUsed {
		return tx.Exec(`UPDATE aff_codes SET used_count = used_count + 1, total_earnings = total_earnings + ? WHERE id = ?`,
			commissionDecimal, affCodeID).Error
	}
	return tx.Exec(`UPDATE aff_codes SET total_earnings = total_earnings + ? WHERE id = ?`,
		commissionDecimal, affCodeID).Error
}

// ==================== compensation (port of instance-provisioning-compensation.ts) ====================

// instancesResources mirrors `{cpu, memory, disk}`.
type instancesResources struct {
	CPU    int32 `json:"cpu"`
	Memory int32 `json:"memory"`
	Disk   int32 `json:"disk"`
}

// instancesPurchaseCompensation mirrors compensateFailedInstancePurchase's result.
type instancesPurchaseCompensation struct {
	Refunded     bool
	RefundAmount float64
	Reason       string
}

// instancesCompensationOutcome mirrors compensateFailedInstanceCreation's result.
type instancesCompensationOutcome struct {
	Claimed      bool
	RefundStatus string
	RefundAmount float64
	RefundReason string
	RefundError  string
}

// instancesCompensationDeps are the four side effects of the compensation path.
// They are variables so the unit tests can inject failures and assert the
// idempotency gate without a database.
type instancesCompensationDeps struct {
	ClaimCreatingToError func(ctx context.Context, instanceID int32) (bool, error)
	RollbackResources    func(ctx context.Context, options db.RollbackResourcesOptions) error
	ReleasePublicIPv4    func(ctx context.Context, instanceID int32) error
	CompensatePurchase   func(ctx context.Context, instanceID, userID, hostID int32) (instancesPurchaseCompensation, error)
}

var instancesCompensation = instancesCompensationDeps{
	ClaimCreatingToError: instancesClaimCreatingToError,
	RollbackResources:    db.RollbackResources,
	ReleasePublicIPv4:    instancesReleaseReservedPublicIPv4,
	CompensatePurchase:   instancesCompensateFailedPurchase,
}

// instancesClaimCreatingToError is the idempotency gate: exactly one compensation
// run can move the row out of `creating`.
func instancesClaimCreatingToError(ctx context.Context, instanceID int32) (bool, error) {
	result := db.DB.WithContext(ctx).Exec(
		`UPDATE instances SET status = 'error', updated_at = ? WHERE id = ? AND status = 'creating'`,
		time.Now().UTC(), instanceID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// instancesReleaseReservedPublicIPv4 mirrors
// releaseReservedPublicIpv4ForFailedCreation; a non-public_ipv4 mode is a no-op.
func instancesReleaseReservedPublicIPv4(ctx context.Context, instanceID int32) error {
	var networkMode *string
	if err := db.DB.WithContext(ctx).Raw(`SELECT network_mode FROM instances WHERE id = ?`, instanceID).
		Scan(&networkMode).Error; err != nil {
		return err
	}
	if networkMode == nil || !lib.NetworkModeNeedsPublicIpv4(*networkMode) {
		return nil
	}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := db.ReleasePublicIpv4ForInstance(ctx, tx, instanceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// instancesCompensateFailedPurchase mirrors compensateFailedInstancePurchase: one
// serializable transaction under the user-balance lock, refund record dedupe by
// remark, AFF commission reversal, no double refund.
func instancesCompensateFailedPurchase(ctx context.Context, instanceID, userID, hostID int32) (instancesPurchaseCompensation, error) {
	outcome := instancesPurchaseCompensation{}
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := db.AdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, userID); err != nil {
			return err
		}

		var instance struct {
			ID     int32
			Name   string
			UserID int32
			HostID int32
		}
		if err := tx.WithContext(ctx).Raw(`SELECT id, name, user_id, host_id FROM instances WHERE id = ?`,
			instanceID).Scan(&instance).Error; err != nil {
			return err
		}
		if instance.ID == 0 || instance.UserID != userID || instance.HostID != hostID {
			outcome.Reason = "instance_not_found_or_mismatch"
			return nil
		}

		var purchase struct {
			ID     int32
			Amount models.Decimal
		}
		if err := tx.WithContext(ctx).Raw(`SELECT id, amount FROM instance_billing_records
			WHERE instance_id = ? AND user_id = ? AND type = 'newPurchase'
			ORDER BY created_at ASC LIMIT 1`, instanceID, userID).Scan(&purchase).Error; err != nil {
			return err
		}
		if purchase.ID == 0 {
			outcome.Reason = "not_paid_purchase"
			return nil
		}

		var existingRefund struct {
			ID int32
		}
		if err := tx.WithContext(ctx).Raw(`SELECT id FROM instance_billing_records
			WHERE instance_id = ? AND user_id = ? AND type = 'refund' AND remark LIKE '%实例创建失败自动退款%'
			LIMIT 1`, instanceID, userID).Scan(&existingRefund).Error; err != nil {
			return err
		}
		if existingRefund.ID != 0 {
			outcome.Reason = "already_refunded"
			return nil
		}

		refundAmount := purchase.Amount.InexactFloat64()
		if refundAmount <= 0 || math.IsNaN(refundAmount) || math.IsInf(refundAmount, 0) {
			outcome.Reason = "invalid_purchase_amount"
			return nil
		}

		var balanceBefore models.Decimal
		if err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, userID).Scan(&balanceBefore).Error; err != nil {
			return err
		}
		refundDecimal := instancesDecimalFromJSNumber(refundAmount)
		if err := tx.Exec(`UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ?`,
			refundDecimal, time.Now().UTC(), userID).Error; err != nil {
			return err
		}
		var balanceAfter models.Decimal
		if err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, userID).Scan(&balanceAfter).Error; err != nil {
			return err
		}

		now := time.Now().UTC()
		remark := "实例创建失败自动退款：" + instance.Name
		balanceLogID, err := instancesTxCreateBalanceLog(tx, userID, models.BalanceLogTypeRefund,
			refundDecimal, balanceBefore, balanceAfter, instanceID, remark)
		if err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO instance_billing_records
				(instance_id, user_id, type, amount, months, period_start, period_end, balance_log_id, remark, created_at)
			VALUES (?, ?, 'refund', ?, 0, ?, ?, ?, ?, ?)`,
			instanceID, userID, instancesNegDecimal(refundDecimal), now, now, balanceLogID, remark, now).Error; err != nil {
			return err
		}

		// Reverse any AFF commission earned by this purchase.
		var affLogs []struct {
			ID             int32
			UserID         int32
			AffCodeID      *int32
			Amount         models.Decimal
			OriginalAmount *float64
		}
		if err := tx.WithContext(ctx).Raw(`SELECT id, user_id, aff_code_id, amount, original_amount
			FROM aff_logs WHERE instance_id = ? AND type = 'new_purchase' AND amount > 0`,
			instanceID).Scan(&affLogs).Error; err != nil {
			return err
		}
		for _, affLog := range affLogs {
			commissionAmount := affLog.Amount.InexactFloat64()
			if commissionAmount <= 0 || math.IsNaN(commissionAmount) || math.IsInf(commissionAmount, 0) {
				continue
			}
			if err := db.AdvisoryTransactionLockTx(tx, db.UserAffBalanceLockNamespace, affLog.UserID); err != nil {
				return err
			}
			var affBalanceBefore models.Decimal
			if err := tx.WithContext(ctx).Raw(`SELECT aff_balance FROM users WHERE id = ?`, affLog.UserID).
				Scan(&affBalanceBefore).Error; err != nil {
				return err
			}
			commissionDecimal := instancesDecimalFromJSNumber(commissionAmount)
			if err := tx.Exec(`UPDATE users SET aff_balance = aff_balance - ?, updated_at = ? WHERE id = ?`,
				commissionDecimal, time.Now().UTC(), affLog.UserID).Error; err != nil {
				return err
			}
			var affBalanceAfter models.Decimal
			if err := tx.WithContext(ctx).Raw(`SELECT aff_balance FROM users WHERE id = ?`, affLog.UserID).
				Scan(&affBalanceAfter).Error; err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO aff_logs
					(user_id, type, amount, aff_code_id, instance_id, original_amount, balance_before, balance_after, remark, created_at)
				VALUES (?, 'new_purchase', ?, ?, ?, ?, ?, ?, ?, ?)`,
				affLog.UserID, instancesNegDecimal(commissionDecimal), affLog.AffCodeID, instanceID, affLog.OriginalAmount,
				affBalanceBefore, affBalanceAfter, "实例创建失败返利冲正："+instance.Name, time.Now().UTC()).Error; err != nil {
				return err
			}
			if affLog.AffCodeID != nil {
				if err := tx.Exec(`UPDATE aff_codes SET used_count = used_count - 1
					WHERE id = ? AND used_count > 0`, *affLog.AffCodeID).Error; err != nil {
					return err
				}
				if err := tx.Exec(`UPDATE aff_codes SET total_earnings = total_earnings - ? WHERE id = ?`,
					commissionDecimal, *affLog.AffCodeID).Error; err != nil {
					return err
				}
			}
		}

		outcome.Refunded = true
		outcome.RefundAmount = refundAmount
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return instancesPurchaseCompensation{}, err
	}
	return outcome, nil
}

// instancesCompensationInput mirrors compensateFailedInstanceCreation's argument.
type instancesCompensationInput struct {
	InstanceID   int32
	HostID       int32
	UserID       int32
	Resources    instancesResources
	NetworkMode  string
	PortLimit    int32
	InstanceName string
	ErrorMessage string
}

// instancesCompensateFailedCreation mirrors compensateFailedInstanceCreation.
func instancesCompensateFailedCreation(ctx context.Context, input instancesCompensationInput) instancesCompensationOutcome {
	claimed, err := instancesCompensation.ClaimCreatingToError(ctx, input.InstanceID)
	if err != nil {
		return instancesCompensationOutcome{RefundStatus: "failed", RefundError: err.Error()}
	}
	if !claimed {
		// creating -> error is the idempotency gate for resources, IPv4 and money.
		return instancesCompensationOutcome{RefundStatus: "already_handled"}
	}

	rollbackPorts := int32(0)
	if lib.NetworkModeAllowsPortMapping(input.NetworkMode) {
		rollbackPorts = input.PortLimit
	}
	_ = instancesCompensation.RollbackResources(ctx, db.RollbackResourcesOptions{
		HostID:    input.HostID,
		CPU:       input.Resources.CPU,
		Memory:    input.Resources.Memory,
		Disk:      input.Resources.Disk,
		PortCount: rollbackPorts,
	})
	_ = instancesCompensation.ReleasePublicIPv4(ctx, input.InstanceID)

	compensation, err := instancesCompensation.CompensatePurchase(ctx, input.InstanceID, input.UserID, input.HostID)
	if err != nil {
		return instancesCompensationOutcome{Claimed: true, RefundStatus: "failed", RefundError: err.Error()}
	}
	status := "not_completed"
	switch {
	case compensation.Refunded:
		status = "refunded"
	case compensation.Reason == "not_paid_purchase":
		status = "not_required"
	case compensation.Reason == "already_refunded":
		status = "already_refunded"
	}
	return instancesCompensationOutcome{
		Claimed:      true,
		RefundStatus: status,
		RefundAmount: compensation.RefundAmount,
		RefundReason: compensation.Reason,
	}
}

// ==================== post-commit provisioning ====================

// instancesProvisionInput carries everything the post-transaction phase needs.
type instancesProvisionInput struct {
	InstanceID         int32
	Host               *db.HostsSelectionRecord
	UserID             int32
	Plan               instancesCreatePlan
	PlanBandwidthLimit *string
	BaseConfigPayload  lib.IncusConfigPayload
	ExtraShellCommands string
	SSHKey             *string
}

// instancesProvisioned is the IP/storage state produced after the transaction.
type instancesProvisioned struct {
	IPv4          *string
	IPv6          *string
	IPv6Gateway   *string
	HostInterface string
	StoragePool   *string
	ConfigPayload lib.IncusConfigPayload
	PublicIPv4    *db.PublicIpv4Assignment
}

// instancesFailProvisioning runs the compensation path and writes the 503 reply,
// mirroring the TypeScript catch block around the post-transaction preparation.
func instancesFailProvisioning(c *gin.Context, input instancesProvisionInput, message string) error {
	ctx := c.Request.Context()
	instancesCompensateFailedCreation(ctx, instancesCompensationInput{
		InstanceID:   input.InstanceID,
		HostID:       input.Host.ID,
		UserID:       input.UserID,
		Resources:    instancesResources{CPU: input.Plan.CPU, Memory: input.Plan.Memory, Disk: input.Plan.Disk},
		NetworkMode:  input.Plan.NetworkMode,
		PortLimit:    input.Plan.Package.PortLimit,
		InstanceName: input.Plan.IncusID,
		ErrorMessage: message,
	})
	_ = db.GpusReleaseFailed(ctx, input.InstanceID)
	lib.RespondAPIError(c, http.StatusServiceUnavailable, lib.ErrHostResourcesInsufficient, message)
	return &instancesCreateAbort{Status: http.StatusServiceUnavailable,
		Payload: lib.APIErrorFor(lib.ErrHostResourcesInsufficient, message)}
}

// instancesPostCommitProvisioning allocates the IPs, regenerates the cloud-init
// network configuration, resolves the storage pool and persists both. Any failure
// runs the compensation path and answers 503 (the transaction already committed).
func instancesPostCommitProvisioning(c *gin.Context, input instancesProvisionInput) (instancesProvisioned, error) {
	ctx := c.Request.Context()
	host := input.Host
	provisioned := instancesProvisioned{
		HostInterface: "eth0",
		ConfigPayload: input.BaseConfigPayload,
	}

	var staticIPv4 *string
	var staticIPv6 *string
	var publicAssignment *db.PublicIpv4Assignment
	allocationError := ""

	if lib.NetworkModeNeedsNatIpv4(input.Plan.NetworkMode) {
		ipv4, err := db.IpAddressesAllocateAndReserveNatIpv4(ctx, host.ID, &input.InstanceID)
		if err != nil {
			allocationError = "NAT 内网 IPv4 分配错误: " + err.Error()
		} else if ipv4 != nil {
			staticIPv4 = ipv4
		} else {
			allocationError = "NAT 内网 IPv4 分配失败（该宿主机内网地址池已满）"
		}
	} else if lib.NetworkModeNeedsPublicIpv4(input.Plan.NetworkMode) {
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			return provisioned, instancesFailProvisioning(c, input, err.Error())
		}
		assignment, reserveErr := db.ReservePublicIpv4ForInstance(ctx, tx, db.PublicIpv4ReserveInput{
			HostID:     host.ID,
			InstanceID: input.InstanceID,
		})
		if reserveErr != nil {
			_ = tx.Rollback(ctx)
			return provisioned, instancesFailProvisioning(c, input, reserveErr.Error())
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return provisioned, instancesFailProvisioning(c, input, commitErr.Error())
		}
		if assignment == nil {
			return provisioned, instancesFailProvisioning(c, input,
				fmt.Sprintf("宿主机 %s 没有可用独立 IPv4 地址", host.Name))
		}
		publicAssignment = assignment
		staticIPv4 = &assignment.Address
	}

	if lib.NetworkModeNeedsRoutedIpv6(input.Plan.NetworkMode) {
		if host.IPv6Subnet == nil || *host.IPv6Subnet == "" {
			if allocationError == "" {
				allocationError = "宿主机未配置 routed IPv6 子网"
			}
		} else {
			for attempts := 0; attempts < 50; attempts++ {
				candidate, genErr := lib.GenerateRandomIPv6(*host.IPv6Subnet)
				if genErr != nil {
					if allocationError == "" {
						allocationError = "routed IPv6 分配错误: " + genErr.Error()
					}
					break
				}
				exists, existsErr := db.IpAddressesExists(ctx, candidate)
				if existsErr != nil {
					if allocationError == "" {
						allocationError = "routed IPv6 分配错误: " + existsErr.Error()
					}
					break
				}
				if !exists {
					value := candidate
					staticIPv6 = &value
					break
				}
			}
			if staticIPv6 == nil && allocationError == "" {
				allocationError = "routed IPv6 分配失败（已尝试 50 次）"
			}
		}
	}

	if allocationError != "" {
		return provisioned, instancesFailProvisioning(c, input, allocationError)
	}

	ipv4CIDR := ""
	if publicAssignment != nil {
		ipv4CIDR = fmt.Sprintf("%s/%d", publicAssignment.Address, publicAssignment.PrefixLength)
	} else if staticIPv4 != nil {
		ipv4CIDR = *staticIPv4 + instancesDefaultNATIPv4CIDR
	}
	ipv4Gateway := instancesDefaultNATIPv4Gateway
	var ipv4DNS []string
	if publicAssignment != nil {
		if publicAssignment.Gateway != "" {
			ipv4Gateway = publicAssignment.Gateway
		}
		if len(publicAssignment.DNS) > 0 {
			ipv4DNS = publicAssignment.DNS
		}
	}
	if len(ipv4DNS) == 0 {
		ipv4DNS = []string{instancesDefaultNATIPv4Gateway}
	}

	if input.Plan.InstanceType == "vm" {
		var network *lib.IncusConfigStaticNetwork
		if staticIPv4 != nil && ipv4CIDR != "" {
			network = &lib.IncusConfigStaticNetwork{
				IPAddress: ipv4CIDR,
				Gateway:   ipv4Gateway,
				DNS:       ipv4DNS,
			}
			if staticIPv6 != nil {
				network.IPv6Address = *staticIPv6
				if host.IPv6Gateway != nil {
					network.IPv6Gateway = *host.IPv6Gateway
				}
			}
		}
		vmPayload, vmErr := lib.IncusConfigGenerateVM(lib.IncusConfigVMParams{
			InstanceName:       input.Plan.Name,
			InstanceIDSeed:     input.Plan.IncusID,
			ImageAlias:         input.Plan.Image,
			RootPassword:       input.Plan.MetaData.RootPassword,
			SSHKey:             input.SSHKey,
			Network:            network,
			ExtraShellCommands: input.ExtraShellCommands,
			NetworkMode:        input.Plan.NetworkMode,
		})
		if vmErr != nil {
			return provisioned, instancesFailProvisioning(c, input, vmErr.Error())
		}
		provisioned.ConfigPayload = vmPayload
	} else if staticIPv4 != nil && ipv4CIDR != "" {
		network := &lib.IncusConfigStaticNetwork{
			IPAddress: ipv4CIDR,
			Gateway:   ipv4Gateway,
			DNS:       ipv4DNS,
		}
		if staticIPv6 != nil {
			network.IPv6Address = *staticIPv6 + "/128"
			network.IPv6Gateway = "fe80::1"
		}
		containerResult, genErr := lib.GenerateIncusConfig(lib.IncusConfigParams{
			InstanceName:       input.Plan.Name,
			ImageAlias:         input.Plan.Image,
			RootPassword:       input.Plan.MetaData.RootPassword,
			SSHKey:             input.SSHKey,
			NetworkMode:        input.Plan.NetworkMode,
			Type:               "container",
			Network:            network,
			ExtraShellCommands: input.ExtraShellCommands,
		})
		if genErr != nil {
			return provisioned, instancesFailProvisioning(c, input, genErr.Error())
		}
		provisioned.ConfigPayload = containerResult.ConfigPayload
	}

	if host.StoragePoolName == nil || *host.StoragePoolName == "" {
		message := fmt.Sprintf("STORAGE_POOL_UNAVAILABLE: 宿主机 %s 未配置可用于实例系统盘的存储池", host.Name)
		return provisioned, instancesFailProvisioning(c, input, message)
	}
	provisioned.StoragePool = host.StoragePoolName

	if err := db.DB.WithContext(ctx).Exec(`UPDATE instances SET storage_pool_name = ?, updated_at = ? WHERE id = ?`,
		provisioned.StoragePool, time.Now().UTC(), input.InstanceID).Error; err != nil {
		return provisioned, instancesFailProvisioning(c, input, err.Error())
	}

	provisioned.IPv4 = staticIPv4
	provisioned.IPv6 = staticIPv6
	provisioned.PublicIPv4 = publicAssignment
	if host.IPv6Gateway != nil {
		provisioned.IPv6Gateway = host.IPv6Gateway
	}
	if host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
		provisioned.HostInterface = *host.IPv6ParentInterface
	}
	return provisioned, nil
}

// ==================== create dispatch ====================

// instancesCreateConfig mirrors the `createConfig` object handed to
// createInstanceAsync (and to the queued task payload).
type instancesCreateConfig struct {
	AutoRemotePort          *bool                  `json:"autoRemotePort,omitempty"`
	Name                    string                 `json:"name"`
	Image                   string                 `json:"image"`
	CPU                     int32                  `json:"cpu"`
	Memory                  int32                  `json:"memory"`
	Disk                    int32                  `json:"disk"`
	SwapEnabled             bool                   `json:"swapEnabled"`
	SwapSize                *int32                 `json:"swapSize"`
	CloudInitConfig         lib.IncusConfigPayload `json:"cloudInitConfig"`
	NetworkMode             string                 `json:"networkMode"`
	Nested                  bool                   `json:"nested"`
	Privileged              bool                   `json:"privileged"`
	PortLimit               int32                  `json:"portLimit"`
	InstanceType            string                 `json:"instanceType"`
	SSHPort                 *int32                 `json:"sshPort"`
	StoragePool             *string                `json:"storagePool"`
	IPv4Address             *string                `json:"ipv4Address"`
	IPv6Address             *string                `json:"ipv6Address"`
	IPv6Gateway             *string                `json:"ipv6Gateway"`
	HostInterface           string                 `json:"hostInterface"`
	LimitsRead              *string                `json:"limitsRead"`
	LimitsWrite             *string                `json:"limitsWrite"`
	LimitsReadIops          *int32                 `json:"limitsReadIops"`
	LimitsWriteIops         *int32                 `json:"limitsWriteIops"`
	LimitsIngress           *string                `json:"limitsIngress"`
	LimitsEgress            *string                `json:"limitsEgress"`
	LimitsProcesses         *int32                 `json:"limitsProcesses"`
	LimitsCPUPriority       *int32                 `json:"limitsCpuPriority"`
	BootAutostart           *bool                  `json:"bootAutostart"`
	BootAutostartPriority   *int32                 `json:"bootAutostartPriority"`
	BootAutostartDelay      *int32                 `json:"bootAutostartDelay"`
	BootHostShutdownTimeout *int32                 `json:"bootHostShutdownTimeout"`
}

// instancesBuildCreateConfig mirrors the createConfig literal (package advanced
// configuration filtered by io_limit_mode).
func instancesBuildCreateConfig(plan instancesCreatePlan, provisioned instancesProvisioned, autoRemotePort *bool) instancesCreateConfig {
	pkg := plan.Package
	config := instancesCreateConfig{
		AutoRemotePort:  autoRemotePort,
		Name:            plan.IncusID,
		Image:           plan.Image,
		CPU:             plan.CPU,
		Memory:          plan.Memory,
		Disk:            plan.Disk,
		SwapEnabled:     false,
		CloudInitConfig: provisioned.ConfigPayload,
		NetworkMode:     plan.NetworkMode,
		Nested:          pkg.Nested != 0,
		Privileged:      pkg.Privileged != 0,
		PortLimit:       instancesPortLimitOrDefault(pkg.PortLimit, 20),
		InstanceType:    plan.InstanceType,
		SSHPort:         instancesInt32Ptr(int32(plan.MetaData.SSHPort)),
		StoragePool:     provisioned.StoragePool,
		IPv4Address:     provisioned.IPv4,
		IPv6Address:     provisioned.IPv6,
		IPv6Gateway:     provisioned.IPv6Gateway,
		HostInterface:   provisioned.HostInterface,
	}
	if plan.InstanceType == "container" && plan.Plan != nil && plan.Plan.SwapSize != 0 {
		config.SwapSize = instancesInt32Ptr(plan.Plan.SwapSize)
	}
	ioMode := pkg.IOLimitMode
	if ioMode == "" {
		ioMode = "throughput"
	}
	read := pkg.LimitsRead
	write := pkg.LimitsWrite
	readIops := pkg.LimitsReadIops
	writeIops := pkg.LimitsWriteIops
	if ioMode == "throughput" {
		config.LimitsRead = &read
		config.LimitsWrite = &write
	} else {
		config.LimitsReadIops = &readIops
		config.LimitsWriteIops = &writeIops
	}
	ingress := pkg.LimitsIngress
	egress := pkg.LimitsEgress
	if plan.PlanBandwidthLimit != nil {
		ingress = *plan.PlanBandwidthLimit
		egress = *plan.PlanBandwidthLimit
	}
	config.LimitsIngress = &ingress
	config.LimitsEgress = &egress
	processes := pkg.LimitsProcesses
	priority := pkg.LimitsCPUPriority
	autostart := pkg.BootAutostart
	autostartPriority := pkg.BootAutostartPriority
	autostartDelay := pkg.BootAutostartDelay
	shutdownTimeout := pkg.BootHostShutdownTimeout
	config.LimitsProcesses = &processes
	config.LimitsCPUPriority = &priority
	config.BootAutostart = &autostart
	config.BootAutostartPriority = &autostartPriority
	config.BootAutostartDelay = &autostartDelay
	config.BootHostShutdownTimeout = &shutdownTimeout
	return config
}

// instancesEnqueueCreateTaskIfNodeUnavailable mirrors the queueing helper: a node
// that cannot serve right now keeps the paid reservation in a queued task.
func instancesEnqueueCreateTaskIfNodeUnavailable(ctx context.Context, instanceID, hostID, userID int32,
	config instancesCreateConfig, resources instancesResources) (bool, error) {
	statuses, err := hostsLoadHostServiceStatuses(ctx, []int32{hostID})
	if err != nil {
		return false, err
	}
	if status, ok := statuses[hostID]; ok && status.Available {
		return false, nil
	}

	payload, err := json.Marshal(gin.H{"config": config, "userId": userID, "resources": resources})
	if err != nil {
		return false, err
	}
	err = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing struct {
			ID int32
		}
		if err := tx.Raw(`SELECT id FROM instance_tasks
			WHERE instance_id = ? AND task_type = 'create' AND status IN ('PENDING','PROCESSING') LIMIT 1`,
			instanceID).Scan(&existing).Error; err != nil {
			return err
		}
		if existing.ID != 0 {
			return nil
		}
		now := time.Now().UTC()
		return tx.Exec(`INSERT INTO instance_tasks
				(instance_id, host_id, user_id, task_type, status, payload, error, retry_count, created_at)
			VALUES (?, ?, ?, 'create', 'PENDING', ?, ?, 0, ?)`,
			instanceID, hostID, userID, string(payload), "节点当前不可用，创建任务已排队等待重试", now).Error
	})
	if err != nil {
		// A queueing failure must not silently drop the purchase: fall back to the
		// direct path so compensation still runs.
		log.Printf("[Provisioning] 实例 %d 排队失败，回退直接创建: %v", instanceID, err)
		return false, nil
	}
	return true, nil
}

// InstancesIncusProvisionInput is the Incus-side input of the async creation.
type InstancesIncusProvisionInput struct {
	Host    *db.HostsRecord
	Config  instancesCreateConfig
	GPUPCIs []string
}

// InstancesIncusProvisioner is the seam for buildInstanceConfig + createInstance +
// startInstance (lib/incus/incus-instances.ts). That module is owned by the
// instances-deps wave and does not export the body builder yet. Unwired, the async
// creation fails loudly, the purchase is refunded and the instance lands in `error`
// — never a fake `running` row.
var InstancesIncusProvisioner func(ctx context.Context, client *lib.IncusClient, input InstancesIncusProvisionInput) error

// InstancesIncusCleanup is the seam for the best-effort `deleteInstance` cleanup
// after a failed creation.
var InstancesIncusCleanup func(ctx context.Context, client *lib.IncusClient, instanceName string) error

// instancesCreateInstanceAsync mirrors createInstanceAsync: create + start on the
// node, flip creating→running, write IP records, provision the automatic remote
// port and notify. Failures run the compensation path and clean the node.
func instancesCreateInstanceAsync(ctx context.Context, instanceID int32, host *db.HostsSelectionRecord,
	config instancesCreateConfig, userID int32, resources instancesResources) error {
	if lib.CurrentOperationTask(ctx) == nil {
		return lib.RunOperationTask(ctx, host.ID, config.Name, lib.OperationTaskKindCreate, func(taskCtx context.Context) error {
			return instancesCreateInstanceAsync(taskCtx, instanceID, host, config, userID, resources)
		}, nil)
	}

	lib.TaskLog(ctx, "info", "[Provisioning] 开始创建实例 "+config.Name)

	hostRecord, err := db.HostsGetByID(ctx, host.ID)
	if err != nil || hostRecord == nil {
		return fmt.Errorf("节点不存在，创建任务无法继续")
	}
	client, err := lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:           hostRecord.URL,
		CertPath:      hostRecord.CertPath,
		KeyPath:       hostRecord.KeyPath,
		TunnelEnabled: hostRecord.TunnelEnabled,
		HostID:        int64(hostRecord.ID),
		TargetHost:    hostRecord.TargetHost,
		TargetPort:    int(hostRecord.TargetPort),
	})
	if err != nil {
		return instancesCreateInstanceAsyncFailed(ctx, instanceID, hostRecord, config, userID, resources, err)
	}

	gpuPCIs, gpuErr := db.GpusGetInstancePCIs(ctx, instanceID, host.ID)
	if gpuErr != nil {
		return instancesCreateInstanceAsyncFailed(ctx, instanceID, hostRecord, config, userID, resources, gpuErr)
	}

	if InstancesIncusProvisioner == nil {
		return instancesCreateInstanceAsyncFailed(ctx, instanceID, hostRecord, config, userID, resources,
			errors.New("lib/incus instance module 尚未移植：InstancesIncusProvisioner 未接线"))
	}
	if err := InstancesIncusProvisioner(ctx, client, InstancesIncusProvisionInput{
		Host:    hostRecord,
		Config:  config,
		GPUPCIs: gpuPCIs,
	}); err != nil {
		return instancesCreateInstanceAsyncFailed(ctx, instanceID, hostRecord, config, userID, resources, err)
	}

	// Only flip a still-`creating` row; a timeout sweeper may have claimed it.
	update := db.DB.WithContext(ctx).Exec(`UPDATE instances
		SET status = 'running', ipv4 = ?, ipv6 = ?, storage_pool_name = ?, updated_at = ?
		WHERE id = ? AND status = 'creating'`,
		config.IPv4Address, config.IPv6Address, config.StoragePool, time.Now().UTC(), instanceID)
	if update.Error != nil {
		return instancesCreateInstanceAsyncFailed(ctx, instanceID, hostRecord, config, userID, resources, update.Error)
	}
	if update.RowsAffected == 0 {
		lib.TaskLog(ctx, "info", "[Provisioning] 实例已被超时清理任务处理，清理已创建的 Incus 实例")
		if InstancesIncusCleanup != nil {
			_ = InstancesIncusCleanup(ctx, client, config.Name)
		}
		return nil
	}

	if config.IPv4Address != nil {
		if _, ipErr := db.IpAddressesCreate(ctx, db.IpAddressesCreateData{
			Address: *config.IPv4Address, Type: models.IpTypeInet4, IsPrimary: true, Device: "eth0", InstanceID: instanceID,
		}); ipErr != nil {
			lib.TaskLog(ctx, "warn", "[Provisioning] 创建 IPv4 记录失败: "+ipErr.Error())
		}
	}
	if config.IPv6Address != nil {
		if _, ipErr := db.IpAddressesCreate(ctx, db.IpAddressesCreateData{
			Address: *config.IPv6Address, Type: models.IpTypeInet6, IsPrimary: true, Device: "eth1", InstanceID: instanceID,
		}); ipErr != nil {
			lib.TaskLog(ctx, "warn", "[Provisioning] 创建 IPv6 记录失败: "+ipErr.Error())
		}
	}

	lib.ProvisionAutoRemotePort(ctx, lib.InstancePortMappingAutoRemotePortInput{
		InstanceID:     instanceID,
		Image:          config.Image,
		NetworkMode:    config.NetworkMode,
		AutoRemotePort: config.AutoRemotePort,
	})

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil || instance == nil {
		return err
	}
	lib.SendNotification(ctx, int64(userID), lib.EventInstanceCreated, lib.EventData{
		"instanceName": instance.Name,
		"status":       "running",
		"hostName":     hostRecord.Name,
		"hostLocation": hostRecord.Location,
		"image":        config.Image,
		"cpu":          config.CPU,
		"memory":       config.Memory,
		"disk":         config.Disk,
		"networkMode":  config.NetworkMode,
		"ipv4":         config.IPv4Address,
		"ipv6":         config.IPv6Address,
	})

	// Creation email (best effort, like the TypeScript try/catch).
	if user, userErr := db.FindUserById(ctx, userID); userErr == nil && user != nil && user.Email != nil && *user.Email != "" {
		var billing struct {
			PackagePlanID *int32
			BillingPrice  *models.Decimal
			ExpiresAt     *time.Time
			PlanName      *string
		}
		if err := db.DB.WithContext(ctx).Raw(`SELECT i.package_plan_id, i.billing_price, i.expires_at, p.name AS plan_name
			FROM instances i LEFT JOIN package_plans p ON p.id = i.package_plan_id WHERE i.id = ?`, instanceID).
			Scan(&billing).Error; err == nil {
			var amount *float64
			if billing.BillingPrice != nil {
				value := billing.BillingPrice.InexactFloat64()
				amount = &value
			}
			planName := ""
			if billing.PlanName != nil {
				planName = *billing.PlanName
			}
			ipv4 := ""
			if config.IPv4Address != nil {
				ipv4 = *config.IPv4Address
			}
			ipv6 := ""
			if config.IPv6Address != nil {
				ipv6 = *config.IPv6Address
			}
			lib.SendInstanceCreatedEmail(ctx, *user.Email, lib.InstanceCreatedEmailData{
				Username:     user.Username,
				InstanceName: instance.Name,
				HostName:     hostRecord.Name,
				Image:        config.Image,
				CPU:          float64(config.CPU),
				Memory:       float64(config.Memory),
				Disk:         float64(config.Disk),
				IPv4:         ipv4,
				IPv6:         ipv6,
				IsPaid:       billing.PackagePlanID != nil,
				PlanName:     planName,
				Amount:       amount,
				ExpiresAt:    billing.ExpiresAt,
			})
		}
	}

	logUserID := int64(userID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.create",
		fmt.Sprintf("Created instance %q [host: %s, image: %s, CPU: %d%%, Memory: %dMB, Disk: %dMB, IPv4: %s, IPv6: %s, network: %s]",
			instance.Name, hostRecord.Name, config.Image, config.CPU, config.Memory, config.Disk,
			instancesDerefOr(config.IPv4Address, "N/A"), instancesDerefOr(config.IPv6Address, "N/A"), config.NetworkMode),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	return nil
}

// instancesCreateInstanceAsyncFailed mirrors the catch block of createInstanceAsync.
func instancesCreateInstanceAsyncFailed(ctx context.Context, instanceID int32, host *db.HostsRecord,
	config instancesCreateConfig, userID int32, resources instancesResources, cause error) error {
	message := cause.Error()
	if config.AutoRemotePort != nil {
		_ = config.AutoRemotePort
	}
	instancesCompensateFailedCreation(ctx, instancesCompensationInput{
		InstanceID:   instanceID,
		HostID:       host.ID,
		UserID:       userID,
		Resources:    resources,
		NetworkMode:  config.NetworkMode,
		PortLimit:    config.PortLimit,
		InstanceName: config.Name,
		ErrorMessage: message,
	})
	if InstancesIncusCleanup != nil {
		if client, err := lib.GetIncusClient(ctx, lib.IncusClientOptions{
			URL:           host.URL,
			CertPath:      host.CertPath,
			KeyPath:       host.KeyPath,
			TunnelEnabled: host.TunnelEnabled,
			HostID:        int64(host.ID),
			TargetHost:    host.TargetHost,
			TargetPort:    int(host.TargetPort),
		}); err == nil {
			_ = InstancesIncusCleanup(ctx, client, config.Name)
		}
	}

	if instance, err := db.GetInstanceById(ctx, instanceID); err == nil && instance != nil {
		lib.SendNotification(ctx, int64(userID), lib.EventInstanceCreateFailed, lib.EventData{
			"instanceName": instance.Name,
			"hostName":     host.Name,
			"error":        message,
		})
		logUserID := int64(userID)
		db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.create",
			fmt.Sprintf("Failed to create instance %q: %s", instance.Name, message),
			db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
	}
	return cause
}

// instancesDerefOr mirrors `value || fallback`.
func instancesDerefOr(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

// instancesNegDecimal negates a Decimal (models.Decimal embeds shopspring's type,
// so the promoted Neg returns the raw decimal and needs re-wrapping).
func instancesNegDecimal(value models.Decimal) models.Decimal {
	return models.Decimal{Decimal: value.Decimal.Neg()}
}

// ==================== Incus primitives (route-local) ====================

// instancesIncusClient mirrors `getIncusClient(host)`.
func instancesIncusClient(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
}

// instancesIncusGetState mirrors getInstanceState.
func instancesIncusGetState(ctx context.Context, client *lib.IncusClient, name string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodGet, "/1.0/instances/"+name+"/state", nil, 30*time.Minute, nil)
}

// instancesIncusAction mirrors startInstance/stopInstance/restartInstance's PUT.
func instancesIncusAction(ctx context.Context, client *lib.IncusClient, name, action string, force bool) error {
	body := map[string]any{"action": action, "timeout": 60}
	if action == "stop" || action == "restart" {
		body["force"] = force
	}
	_, err := client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body, 30*time.Minute, body)
	return err
}

// instancesIncusDelete mirrors deleteInstance.
func instancesIncusDelete(ctx context.Context, client *lib.IncusClient, name string) error {
	_, err := client.Request(ctx, http.MethodDelete, "/1.0/instances/"+name, nil, 30*time.Minute, nil)
	return err
}

// instancesIncusRemoveDevice mirrors removeDevice.
func instancesIncusRemoveDevice(ctx context.Context, client *lib.IncusClient, name, deviceName string) error {
	instance, err := lib.GetInstance(ctx, client, name)
	if err != nil {
		return err
	}
	if instance.Devices == nil {
		return nil
	}
	if _, ok := instance.Devices[deviceName]; !ok {
		return nil
	}
	devices := make(map[string]map[string]any, len(instance.Devices))
	for key, value := range instance.Devices {
		if key == deviceName {
			continue
		}
		devices[key] = value
	}
	_, err = lib.UpdateInstance(ctx, client, name, map[string]any{"devices": devices})
	return err
}

// instancesIncusPatchResourceLimits mirrors patchInstanceResources.
func instancesIncusPatchResourceLimits(ctx context.Context, client *lib.IncusClient, name string,
	cpu, memory, disk *int32) error {
	var cpuValue, memoryValue, diskValue *int
	if cpu != nil {
		cpuValue = instancesIntPtr(int(*cpu))
	}
	if memory != nil {
		memoryValue = instancesIntPtr(int(*memory))
	}
	if disk != nil {
		diskValue = instancesIntPtr(int(*disk))
	}
	_, err := lib.PatchInstanceResources(ctx, client, name, cpuValue, memoryValue, diskValue)
	return err
}

// ==================== route 5: detail ====================

// instancesDetailHandler mirrors GET /:id.
func instancesDetailHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}

	portMappings, err := db.PortMappingsList(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	var natPublicIP *string
	if host != nil {
		natPublicIP = host.NATPublicIP
	}
	hostIPAddress := instancesHostIPAddress(host)

	hostIPv6Address := instancesHostIPv6Address(ctx, host, instance.NetworkMode, hostIPAddress)

	serviceStatuses, err := hostsLoadHostServiceStatuses(ctx, []int32{instance.HostID})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	serviceStatus := serviceStatuses[instance.HostID]

	currentPortCount := int32(0)
	for _, mapping := range portMappings {
		if !mapping.SystemManaged {
			currentPortCount++
		}
	}
	var currentSnapshotCount, currentBackupCount, currentSiteCount int64
	countRow := struct {
		Snapshots int64 `gorm:"column:snapshots"`
		Backups   int64 `gorm:"column:backups"`
		Sites     int64 `gorm:"column:sites"`
	}{}
	if err := db.DB.WithContext(ctx).Raw(`SELECT
			(SELECT COUNT(*) FROM snapshots WHERE instance_id = ?) AS snapshots,
			(SELECT COUNT(*) FROM backups WHERE instance_id = ? AND status <> 'deleted') AS backups,
			(SELECT COUNT(*) FROM proxy_sites WHERE instance_id = ?) AS sites`,
		instanceID, instanceID, instanceID).Scan(&countRow).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	currentSnapshotCount = countRow.Snapshots
	currentBackupCount = countRow.Backups
	currentSiteCount = countRow.Sites

	var pkg *db.PackageDetailRow
	if instance.PackageID != nil {
		pkg, err = db.GetPackageById(ctx, *instance.PackageID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	effectiveSiteLimit := int32(10)
	if instance.SiteLimit != nil {
		effectiveSiteLimit = *instance.SiteLimit
	} else if pkg != nil {
		effectiveSiteLimit = pkg.SiteLimit
	}

	var planName *string
	var planPrice *float64
	var billingCycle *int32
	trafficResetPrice := 0.0
	if pkg != nil && pkg.TrafficResetPrice != 0 {
		trafficResetPrice = pkg.TrafficResetPrice
	}
	var affDiscountRate *float64
	hasAffBinding := false
	var nextRenewPrice *float64
	var activePromoBinding *db.PromoInstanceBindingWithCode
	now := time.Now().UTC()
	if instance.PackagePlanID != nil {
		plan, planErr := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
		if planErr != nil {
			instancesRespondInternal(c, planErr)
			return
		}
		if plan != nil {
			planName = &plan.Name
			if plan.TrafficResetPrice != nil {
				trafficResetPrice = plan.TrafficResetPrice.InexactFloat64()
			}
			if plan.Price.InexactFloat64() != 0 && plan.BillingCycle != 0 {
				price := plan.Price.InexactFloat64() / 100
				planPrice = &price
				cycle := plan.BillingCycle
				billingCycle = &cycle
			}
		}

		promoBinding, promoErr := db.PromoGetInstanceBinding(ctx, instanceID, nil)
		if promoErr != nil {
			instancesRespondInternal(c, promoErr)
			return
		}
		activePromoBinding = promoBinding
		isPromoActive := false
		if promoBinding != nil {
			expiresActive := true
			if promoBinding.PromoCode.ExpiresAt != nil {
				expiresActive = promoBinding.PromoCode.ExpiresAt.After(now)
			}
			remaining := int32(1)
			if promoBinding.RemainingCycles != nil {
				remaining = *promoBinding.RemainingCycles
			}
			isPromoActive = promoBinding.PromoCode.Enabled &&
				(promoBinding.DurationType == models.PromoDurationTypeFOREVER || remaining > 0 || expiresActive)
		}

		affBinding, affErr := db.AffGetInstanceBinding(ctx, instanceID, nil)
		if affErr != nil {
			instancesRespondInternal(c, affErr)
			return
		}

		discountRate := 0.0
		switch {
		case promoBinding != nil && isPromoActive:
			hasAffBinding = true
			if promoBinding.PromoCode.DiscountType == models.PromoDiscountTypePERCENTAGE {
				discountRate = promoBinding.PromoCode.DiscountValue.InexactFloat64()
			}
		case affBinding != nil && affBinding.AffCode.Enabled:
			hasAffBinding = true
			discountRate = affBinding.AffCode.DiscountRate.InexactFloat64()
		case promoBinding != nil || affBinding != nil:
			hasAffBinding = true
		}

		baseCyclePrice := 0.0
		hasBase := false
		if instance.BillingPrice != nil {
			baseCyclePrice = *instance.BillingPrice
			hasBase = true
		} else if planPrice != nil {
			baseCyclePrice = *planPrice
			hasBase = true
		}
		if hasBase {
			cycleMonths := int32(1)
			if billingCycle != nil {
				cycleMonths = *billingCycle
			}
			monthlyPrice := lib.CalculateMonthlyPrice(baseCyclePrice, int(cycleMonths))
			var promoQuote *services.PromoRenewalSplitResult
			if isPromoActive && promoBinding != nil {
				quote := services.PromoQuoteRenewal(monthlyPrice, int(cycleMonths), promoBinding)
				promoQuote = &quote
			}
			_, vipBenefit, vipErr := services.VipGetUserContinuousVipBenefit(ctx, instance.UserID, nil)
			if vipErr != nil {
				instancesRespondInternal(c, vipErr)
				return
			}
			affRate := 0.0
			if affBinding != nil && affBinding.AffCode.Enabled {
				affRate = affBinding.AffCode.DiscountRate.InexactFloat64()
			}
			vipPercent := vipBenefit.OrderDiscountPercent
			renewalPrice := services.VipArbitratePrice(services.VipPriceArbitrationInput{
				BasePrice:          baseCyclePrice,
				AffDiscountRate:    &affRate,
				VipDiscountPercent: &vipPercent,
			})
			finalCycleAmount := renewalPrice.FinalPrice
			if promoQuote != nil && promoQuote.FinalAmount <= finalCycleAmount && promoQuote.DiscountAmount > 0 {
				finalCycleAmount = promoQuote.FinalAmount
			}
			nextRenewPrice = &finalCycleAmount
			if baseCyclePrice > 0 {
				rate := math.Round(((baseCyclePrice-finalCycleAmount)/baseCyclePrice)*10000) / 10000
				discountRate = rate
			}
		}
		if discountRate > 0 {
			affDiscountRate = &discountRate
		}
	}

	effectivePortLimit := int32(20)
	if instance.PortLimit != nil {
		effectivePortLimit = *instance.PortLimit
	}
	effectiveSnapshotLimit := int32(5)
	if instance.SnapshotLimit != nil {
		effectiveSnapshotLimit = *instance.SnapshotLimit
	}
	effectiveBackupLimit := int32(3)
	if instance.BackupLimit != nil {
		effectiveBackupLimit = *instance.BackupLimit
	}
	remaining := func(limit, used int32) int32 {
		if limit == 0 {
			return 0
		}
		if limit-used < 0 {
			return 0
		}
		return limit - used
	}
	quotaUsage := gin.H{
		"port": currentPortCount, "snapshot": currentSnapshotCount,
		"backup": currentBackupCount, "site": currentSiteCount,
	}
	effectiveQuotaLimit := gin.H{
		"port": effectivePortLimit, "snapshot": effectiveSnapshotLimit,
		"backup": effectiveBackupLimit, "site": effectiveSiteLimit,
	}
	remainingQuota := gin.H{
		"port":     remaining(effectivePortLimit, currentPortCount),
		"snapshot": remaining(effectiveSnapshotLimit, int32(currentSnapshotCount)),
		"backup":   remaining(effectiveBackupLimit, int32(currentBackupCount)),
		"site":     remaining(effectiveSiteLimit, int32(currentSiteCount)),
	}

	imageName, err := db.GetImageDisplayName(ctx, instance.Image)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	instanceType := "container"
	if pkg != nil && pkg.InstanceType == "vm" {
		instanceType = "vm"
	}
	hostStatusPtr := &serviceStatus
	hostCountry := instance.HostCountryCode
	if hostCountry == "" {
		hostCountry = "us"
	}
	hostName := instance.HostName
	if hostName == "" {
		hostName = "unknown"
	}

	portMappingViews := make([]gin.H, 0, len(portMappings))
	for _, mapping := range portMappings {
		portMappingViews = append(portMappingViews, gin.H{
			"id":           mapping.ID,
			"protocol":     mapping.Protocol,
			"public_port":  mapping.PublicPort,
			"private_port": mapping.PrivatePort,
			"remark":       mapping.Remark,
		})
	}

	response := map[string]any{
		"id":        instance.ID,
		"incusId":   instance.IncusID,
		"name":      instance.Name,
		"status":    instancesServiceStatus(string(instance.Status), hostStatusPtr, instance.IncusID),
		"image":     instance.Image,
		"imageName": imageName,
		"cpu":       instance.CPU,
		"memory":    instance.Memory,
		"disk":      instance.Disk,
		"ipv4":      instance.IPv4,
		"ipv6":      instance.IPv6,
		"host": gin.H{
			"name":          hostName,
			"country_code":  hostCountry,
			"storageDriver": instance.HostStorageDriver,
		},
		"hostCountryCode":       hostCountry,
		"network_mode":          instance.NetworkMode,
		"instance_type":         instanceType,
		"instanceType":          instanceType,
		"iconBadgeId":           instance.IconBadgeID,
		"ssh_port":              instance.SSHPort,
		"port_limit":            instance.PortLimit,
		"snapshot_limit":        instance.SnapshotLimit,
		"backup_limit":          instance.BackupLimit,
		"monthlyTrafficLimit":   instance.MonthlyTrafficLimit,
		"remaining_quota":       remainingQuota,
		"quota_usage":           quotaUsage,
		"effective_quota_limit": effectiveQuotaLimit,
		"port_mappings":         portMappingViews,
		"created_at":            instance.CreatedAt,
		"package_id":            instance.PackageID,
		"packageName":           packageName(pkg),
		"packagePlanId":         instance.PackagePlanID,
		"planId":                instance.PackagePlanID,
		"planName":              planName,
		"planPrice":             planPrice,
		"billingPrice":          instance.BillingPrice,
		"trafficResetPrice":     trafficResetPrice,
		"billingCycle":          billingCycle,
		"nextRenewPrice":        nextRenewPrice,
		"affDiscountRate":       affDiscountRate,
		"hasAffBinding":         hasAffBinding,
		"promoBinding":          instancesPromoBindingView(activePromoBinding, now),
		"limitsIngress":         instancesFirstNonNil(instance.LimitsIngress, packageLimitsIngress(pkg)),
		"limitsEgress":          instancesFirstNonNil(instance.LimitsEgress, packageLimitsEgress(pkg)),
		"expires_at":            instance.ExpiresAt,
		"suspended_at":          instance.SuspendedAt,
		"suspended_by":          instance.SuspendedBy,
		"suspend_reason":        instance.SuspendReason,
		"autoRenew":             instance.AutoRenew,
	}
	if host != nil {
		response["hostAnnouncement"] = host.Announcement
	} else {
		response["hostAnnouncement"] = nil
	}

	isInstanceOwner := instance.UserID == claims.ID
	isHostOwner := host != nil && host.UserID == claims.ID
	isAdmin := claims.Role == "admin"

	if isInstanceOwner {
		if natPublicIP != nil {
			response["nat_public_ip"] = *natPublicIP
		}
		if hostIPAddress != nil {
			response["host_ip_address"] = *hostIPAddress
		}
		if hostIPv6Address != nil {
			response["host_ipv6_address"] = *hostIPv6Address
		}
		if host != nil && host.NATPortStart != nil && host.NATPortEnd != nil {
			response["port_range_start"] = *host.NATPortStart
			response["port_range_end"] = *host.NATPortEnd
		}
		response["hostId"] = instance.HostID
		response["user_id"] = instance.UserID
		response["isInstanceOwner"] = true
	}
	if isHostOwner || isAdmin {
		response["hostId"] = instance.HostID
		if natPublicIP != nil {
			response["nat_public_ip"] = *natPublicIP
		}
		if hostIPAddress != nil {
			response["host_ip_address"] = *hostIPAddress
		}
		if hostIPv6Address != nil {
			response["host_ipv6_address"] = *hostIPv6Address
		}
		response["isHostOwner"] = true
		response["isInstanceOwner"] = isInstanceOwner
		response["isAdmin"] = isAdmin
	}
	enableResourcePool := false
	if host != nil {
		enableResourcePool = host.EnableResourcePool
	}
	response["enableResourcePool"] = enableResourcePool

	c.JSON(http.StatusOK, gin.H{"instance": response})
}

// instancesInstanceHostLabel mirrors `instance.host_name || instance.host_id`.
func instancesInstanceHostLabel(instance *db.InstanceDetailRecord) string {
	if instance.HostName != "" {
		return instance.HostName
	}
	return strconv.FormatInt(int64(instance.HostID), 10)
}

func packageName(pkg *db.PackageDetailRow) *string {
	if pkg == nil {
		return nil
	}
	name := pkg.Name
	return &name
}

func packageLimitsIngress(pkg *db.PackageDetailRow) *string {
	if pkg == nil || pkg.LimitsIngress == "" {
		return nil
	}
	value := pkg.LimitsIngress
	return &value
}

func packageLimitsEgress(pkg *db.PackageDetailRow) *string {
	if pkg == nil || pkg.LimitsEgress == "" {
		return nil
	}
	value := pkg.LimitsEgress
	return &value
}

func instancesFirstNonNil(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// instancesPromoBindingView mirrors the promoBinding response object.
func instancesPromoBindingView(binding *db.PromoInstanceBindingWithCode, now time.Time) any {
	if binding == nil {
		return nil
	}
	expiresActive := true
	if binding.PromoCode.ExpiresAt != nil {
		expiresActive = binding.PromoCode.ExpiresAt.After(now)
	}
	remaining := int32(1)
	if binding.RemainingCycles != nil {
		remaining = *binding.RemainingCycles
	}
	isActive := binding.PromoCode.Enabled &&
		(binding.DurationType == models.PromoDurationTypeFOREVER || remaining > 0 || expiresActive)
	return gin.H{
		"code":            binding.PromoCode.Code,
		"discountType":    binding.PromoCode.DiscountType,
		"discountValue":   binding.PromoCode.DiscountValue.InexactFloat64(),
		"durationType":    binding.DurationType,
		"remainingCycles": binding.RemainingCycles,
		"isActive":        isActive,
	}
}

// instancesHostIPv6Address mirrors the nat_ipv6_nat / ipv6_nat host IPv6 lookup
// chain (nat_public_ipv6 → ip_address → ipv6_gateway → alias → url).
func instancesHostIPv6Address(ctx context.Context, host *db.HostsRecord, networkMode models.NetworkMode,
	hostIPAddress *string) *string {
	if host == nil {
		return nil
	}
	mode := string(networkMode)
	if mode != string(models.NetworkModeNatIpv6Nat) && mode != string(models.NetworkModeIpv6Nat) {
		return nil
	}
	if host.NATPublicIPv6 != nil && *host.NATPublicIPv6 != "" {
		return host.NATPublicIPv6
	}
	if hostIPAddress != nil && strings.Contains(*hostIPAddress, ":") {
		return hostIPAddress
	}
	if host.IPv6Gateway != nil && *host.IPv6Gateway != "" {
		return host.IPv6Gateway
	}
	var alias struct {
		Address string
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT address FROM host_address_aliases
		WHERE host_id = ? AND kind = 'ipv6' LIMIT 1`, host.ID).Scan(&alias).Error; err == nil && alias.Address != "" {
		value := alias.Address
		return &value
	}
	if host.URL != "" {
		if extracted := lib.ExtractHostAddressFromURL(&host.URL); extracted != nil && strings.Contains(*extracted, ":") {
			return extracted
		}
	}
	return nil
}

// ==================== route 6: password ====================

// instancesPasswordHandler mirrors GET /:id/password (AUTH004: owner or admin only).
func instancesPasswordHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID && claims.Role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.RootPassword != nil && *instance.RootPassword != "" {
		c.JSON(http.StatusOK, gin.H{"rootPassword": lib.DecryptSensitiveData(*instance.RootPassword)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rootPassword": nil})
}

// ==================== route 7: stats ====================

// instancesCPUSampleCache mirrors the process-local `cpuSampleCache` (single
// process is an architectural invariant, see WAVE_D_SPEC §0.1).
type instancesCPUSample struct {
	UsageNs int64
	Ts      int64
}

var instancesCPUSamples = struct {
	sync.Mutex
	values map[int32]instancesCPUSample
}{values: map[int32]instancesCPUSample{}}

// instancesComputeCPUUsagePercent mirrors computeCpuUsagePercent.
func instancesComputeCPUUsagePercent(instanceID int32, usageNs int64, allocatedCores int) int {
	if usageNs <= 0 || allocatedCores <= 0 {
		return 0
	}
	now := time.Now().UnixMilli()
	instancesCPUSamples.Lock()
	previous, seen := instancesCPUSamples.values[instanceID]
	instancesCPUSamples.values[instanceID] = instancesCPUSample{UsageNs: usageNs, Ts: now}
	instancesCPUSamples.Unlock()
	if !seen {
		return 0
	}
	deltaMs := now - previous.Ts
	if deltaMs <= 0 {
		return 0
	}
	deltaNs := usageNs - previous.UsageNs
	if deltaNs < 0 {
		return 0
	}
	totalCells := float64(deltaMs) * 1_000_000 * float64(allocatedCores)
	if totalCells <= 0 {
		return 0
	}
	return int(math.Max(0, math.Min(100, math.Round((float64(deltaNs)/totalCells)*100))))
}

// instancesIncusState is the subset of GET /1.0/instances/{name}/state the stats
// route reads.
type instancesIncusState struct {
	Status string `json:"status"`
	CPU    *struct {
		Usage float64 `json:"usage"`
	} `json:"cpu"`
	Memory *struct {
		Usage float64 `json:"usage"`
	} `json:"memory"`
	Disk *struct {
		Root *struct {
			Usage float64 `json:"usage"`
		} `json:"root"`
	} `json:"disk"`
}

// instancesEmptyStats mirrors the zero-stats payload.
func instancesEmptyStats(instance *db.InstanceDetailRecord, status string) gin.H {
	return gin.H{
		"stats": gin.H{
			"cpu":     gin.H{"usage": 0, "usagePercent": 0},
			"memory":  gin.H{"usage": 0, "limit": instance.Memory, "usagePercent": 0},
			"disk":    gin.H{"usage": 0, "limit": instance.Disk, "usagePercent": 0},
			"network": gin.H{"bytesReceived": 0, "bytesSent": 0},
		},
		"status": status,
	}
}

// instancesStatsHandler mirrors GET /:id/stats.
func instancesStatsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}

	serviceStatuses, err := hostsLoadHostServiceStatuses(ctx, []int32{instance.HostID})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	serviceStatus := serviceStatuses[instance.HostID]
	pending := false
	for _, name := range serviceStatus.PendingInstances {
		if name == instance.IncusID {
			pending = true
			break
		}
	}
	if !serviceStatus.Available || pending ||
		(instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusError) {
		c.JSON(http.StatusOK, instancesEmptyStats(instance,
			instancesServiceStatus(string(instance.Status), &serviceStatus, instance.IncusID)))
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil || host == nil {
		c.JSON(http.StatusOK, instancesEmptyStats(instance,
			instancesServiceStatus(string(instance.Status), &serviceStatus, instance.IncusID)))
		return
	}
	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		c.JSON(http.StatusOK, instancesEmptyStats(instance,
			instancesServiceStatus(string(instance.Status), &serviceStatus, instance.IncusID)))
		return
	}
	rawState, err := instancesIncusGetState(ctx, client, instance.IncusID)
	if err != nil {
		lib.InstanceStatusObservationsShared.Forget(instanceID)
		c.JSON(http.StatusOK, instancesEmptyStats(instance,
			instancesServiceStatus(string(instance.Status), &hostsHostServiceStatus{Status: "offline", Available: false}, instance.IncusID)))
		return
	}
	var state instancesIncusState
	if err := json.Unmarshal(rawState, &state); err != nil {
		lib.InstanceStatusObservationsShared.Forget(instanceID)
		c.JSON(http.StatusOK, instancesEmptyStats(instance,
			instancesServiceStatus(string(instance.Status), &hostsHostServiceStatus{Status: "offline", Available: false}, instance.IncusID)))
		return
	}

	if state.Status != "" {
		incusStatus := lib.MapInstanceStatus(state.Status)
		hasActive, activeErr := services.HasActiveInstanceOperation(ctx, instanceID)
		if activeErr != nil {
			instancesRespondInternal(c, activeErr)
			return
		}
		if hasActive {
			lib.InstanceStatusObservationsShared.Forget(instanceID)
		} else if lib.InstanceStatusObservationsShared.ShouldApply(instanceID, string(instance.Status), incusStatus, lib.InstanceStatusObservationsNow()) &&
			string(instance.Status) != incusStatus {
			update := db.DB.WithContext(ctx).Exec(`UPDATE instances
				SET status = ?, last_synced_at = ?, updated_at = ? 
				WHERE id = ? AND incus_id = ? AND host_id = ? AND status = ?`,
				incusStatus, time.Now().UTC(), time.Now().UTC(), instanceID, instance.IncusID, instance.HostID, string(instance.Status))
			if update.Error != nil {
				instancesRespondInternal(c, update.Error)
				return
			}
		}
	} else {
		lib.InstanceStatusObservationsShared.Forget(instanceID)
	}

	memoryUsage := 0.0
	if state.Memory != nil {
		memoryUsage = state.Memory.Usage
	}
	memoryLimit := float64(instance.Memory) * 1024 * 1024
	memoryPercent := 0.0
	if memoryLimit > 0 {
		memoryPercent = math.Round((memoryUsage / memoryLimit) * 100)
	}
	cpuUsageNs := int64(0)
	if state.CPU != nil {
		cpuUsageNs = int64(state.CPU.Usage)
	}
	cpuPercent := instancesComputeCPUUsagePercent(instanceID, cpuUsageNs, lib.AllowanceToCores(int(instance.CPU)))
	diskUsage := 0.0
	if state.Disk != nil && state.Disk.Root != nil {
		diskUsage = state.Disk.Root.Usage
	}
	diskLimit := float64(instance.Disk) * 1024 * 1024
	diskPercent := 0.0
	if diskLimit > 0 {
		diskPercent = math.Round((diskUsage / diskLimit) * 100)
	}
	counters, _ := services.InstanceTrafficCountersFromState(instance.IncusID, rawState)

	updated, err := db.GetInstanceById(ctx, instanceID)
	currentStatus := string(instance.Status)
	if err == nil && updated != nil {
		currentStatus = string(updated.Status)
	}

	c.JSON(http.StatusOK, gin.H{
		"stats": gin.H{
			"cpu": gin.H{"usage": cpuUsageNs, "usagePercent": cpuPercent},
			"memory": gin.H{
				"usage":        math.Round(memoryUsage / 1024 / 1024),
				"limit":        instance.Memory,
				"usagePercent": math.Min(memoryPercent, 100),
			},
			"disk": gin.H{
				"usage":        math.Round(diskUsage / 1024 / 1024),
				"limit":        instance.Disk,
				"usagePercent": math.Min(diskPercent, 100),
			},
			"network": gin.H{"bytesReceived": counters.RxBytes, "bytesSent": counters.TxBytes},
		},
		"status": instancesServiceStatus(currentStatus, &serviceStatus, instance.IncusID),
	})
}

// ==================== routes 8-10: start / stop / restart ====================

// instancesQueueLifecycleTask mirrors the shared tail of start/stop/restart.
func instancesQueueLifecycleTask(c *gin.Context, claims *lib.AccessTokenClaims, instance *db.InstanceDetailRecord,
	taskType models.InstanceTaskType, logAction, logContent, responseMessage string) {
	ctx := c.Request.Context()
	task, err := instancesCreateTaskOrConflict(c, db.CreateInstanceTaskData{
		InstanceID: instance.ID,
		HostID:     instance.HostID,
		UserID:     claims.ID,
		TaskType:   taskType,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, logAction, logContent, db.LogResultSuccess,
		db.LogOptionsWithInstance(&instance.ID))
	c.JSON(http.StatusAccepted, gin.H{"message": responseMessage, "taskId": task.ID, "status": task.Status})
}

// instancesLifecycleHandler serves POST /:id/start, /:id/stop and /:id/restart.
func instancesLifecycleHandler(taskType models.InstanceTaskType) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		claims := instancesScopeUser(c)

		instanceID, valid := instancesParseRouteID(c.Param("id"))
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		if instanceID == 0 {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}
		instance, err := db.GetInstanceById(ctx, instanceID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if instance == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}
		allowed, err := instancesRequireInstancePermission(c, claims, instance)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if !allowed {
			return
		}

		switch taskType {
		case models.InstanceTaskTypeStart:
			if instance.Status == models.InstanceStatusRunning {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceAlreadyRunning, "")
				return
			}
			if instance.Status == models.InstanceStatusSuspended {
				instancesRespondSuspended(c, instance)
				return
			}
		case models.InstanceTaskTypeStop:
			if instance.Status == models.InstanceStatusSuspended {
				host, hostErr := db.HostsGetByID(ctx, instance.HostID)
				if hostErr != nil {
					instancesRespondInternal(c, hostErr)
					return
				}
				if host == nil || host.UserID != claims.ID {
					instancesRespondSuspended(c, instance)
					return
				}
			}
			if instance.Status == models.InstanceStatusStopped {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceAlreadyStopped, "")
				return
			}
		case models.InstanceTaskTypeRestart:
			if instance.Status == models.InstanceStatusSuspended {
				instancesRespondSuspended(c, instance)
				return
			}
		}

		locked, err := instancesCheckTransferLock(c, instanceID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if locked {
			return
		}
		blocked, err := instancesRejectActiveWorkflowConflict(c, instanceID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if blocked {
			return
		}

		action := string(taskType)
		instancesQueueLifecycleTask(c, claims, instance, taskType,
			"instance."+action,
			fmt.Sprintf("Queued %s task for instance %q", action, instance.Name),
			"Instance "+action+" task queued")
	}
}

// instancesRespondSuspended mirrors the expired-vs-manual suspension split.
func instancesRespondSuspended(c *gin.Context, instance *db.InstanceDetailRecord) {
	if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
		return
	}
	lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
}

// ==================== routes 11-12: suspend / unsuspend ====================

// instancesSuspendHandler mirrors POST /:id/suspend.
func instancesSuspendHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	reason := ""
	if body, status, message := decodeJSONRequestBody(c); status == 0 {
		if object, ok := body.Value.(map[string]any); ok && object != nil {
			if raw, present := object["reason"]; present {
				text, isString := raw.(string)
				if !isString {
					respondAJVValidation(c, "must be string")
					return
				}
				if len([]rune(text)) > 500 {
					respondAJVValidation(c, "reason is too long")
					return
				}
				reason = text
			}
		}
	} else {
		middleware.AbortFastifyBody(c, status, message)
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	isHostOwner := host != nil && host.UserID == claims.ID
	isAdmin := claims.Role == "admin"
	if !isAdmin && !isHostOwner {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Instance is already suspended", "code": "INSTANCE_ALREADY_SUSPENDED"})
		return
	}
	if instance.Status == models.InstanceStatusDeleted {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceNotFound, "")
		return
	}

	if instance.Status == models.InstanceStatusRunning && host != nil {
		if client, clientErr := instancesIncusClient(ctx, host); clientErr == nil {
			_ = instancesIncusAction(ctx, client, instance.IncusID, "stop", true)
		}
	}

	if err := db.SuspendInstance(ctx, instanceID, db.SuspendInstanceData{
		SuspendedBy:   &claims.ID,
		SuspendReason: reason,
	}); err != nil {
		instancesRespondInternal(c, err)
		return
	}

	logUserID := int64(claims.ID)
	content := fmt.Sprintf("Suspended instance %q", instance.Name)
	if reason != "" {
		content = fmt.Sprintf("Suspended instance %q (reason: %s)", instance.Name, reason)
	}
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.suspend", content, db.LogResultSuccess,
		db.LogOptionsWithInstance(&instanceID))

	if instance.UserID != claims.ID {
		hostName := ""
		if host != nil {
			hostName = host.Name
		}
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceSuspended, lib.EventData{
			"instanceName":  instance.Name,
			"hostName":      hostName,
			"suspendReason": reason,
		})
	}
	c.JSON(http.StatusOK, gin.H{"message": "Instance suspended successfully"})
}

// instancesUnsuspendHandler mirrors POST /:id/unsuspend.
func instancesUnsuspendHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	isHostOwner := host != nil && host.UserID == claims.ID
	isAdmin := claims.Role == "admin"
	if !isAdmin && !isHostOwner {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status != models.InstanceStatusSuspended {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceNotSuspended, "")
		return
	}
	if instance.SuspendReason != nil && *instance.SuspendReason == "expired" && instance.ExpiresAt != nil {
		if expiresAt, parseErr := time.Parse("2006-01-02T15:04:05.000Z", *instance.ExpiresAt); parseErr == nil {
			if !expiresAt.After(time.Now().UTC()) {
				lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
				return
			}
		}
	}

	if err := db.UnsuspendInstance(ctx, instanceID); err != nil {
		instancesRespondInternal(c, err)
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.unsuspend",
		fmt.Sprintf("Unsuspended instance %q", instance.Name), db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	if instance.UserID != claims.ID {
		hostName := ""
		if host != nil {
			hostName = host.Name
		}
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceUnsuspended, lib.EventData{
			"instanceName": instance.Name,
			"hostName":     hostName,
		})
	}
	c.JSON(http.StatusOK, gin.H{"message": "Instance unsuspended successfully"})
}

// ==================== route 13: sync-status ====================

// instancesSyncStatusHandler mirrors POST /:id/sync-status.
func instancesSyncStatusHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	isInstanceOwner := instance.UserID == claims.ID
	isHostOwner := host != nil && host.UserID == claims.ID
	isAdmin := claims.Role == "admin"
	if !isInstanceOwner && !isHostOwner && !isAdmin {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}

	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法连接到节点: " + err.Error()})
		return
	}

	rawState, stateErr := instancesIncusGetState(ctx, client, instance.IncusID)
	incusInstance, instanceErr := lib.GetInstance(ctx, client, instance.IncusID)
	if stateErr != nil || instanceErr != nil {
		message := ""
		if stateErr != nil {
			message = stateErr.Error()
		} else {
			message = instanceErr.Error()
		}
		if strings.Contains(message, "not found") || strings.Contains(message, "Instance not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例在节点上不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败: " + message})
		return
	}
	var state instancesIncusState
	if err := json.Unmarshal(rawState, &state); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败: " + err.Error()})
		return
	}
	if state.Status == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取实例状态"})
		return
	}
	incusStatus := lib.MapInstanceStatus(state.Status)
	if incusStatus == "unknown" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "未知状态: " + state.Status})
		return
	}

	currentStatus := string(instance.Status)
	statusChanged := false
	if currentStatus != string(models.InstanceStatusSuspended) && currentStatus != incusStatus {
		if err := db.UpdateInstanceStatus(ctx, instanceID, models.InstanceStatus(incusStatus), nil); err != nil {
			instancesRespondInternal(c, err)
			return
		}
		statusChanged = true
	}

	resolvedIPv4, resolvedIPv6 := instancesResolveStateAddresses(instance, incusInstance, rawState)
	oldIPv4, oldIPv6 := instance.IPv4, instance.IPv6
	ipv4Changed := !instancesStringPtrEqual(oldIPv4, resolvedIPv4)
	ipv6Changed := !instancesStringPtrEqual(oldIPv6, resolvedIPv6)
	if ipv4Changed || ipv6Changed {
		network := db.InstanceNetworkUpdate{}
		if ipv4Changed {
			network.IPv4 = db.InstanceValue(resolvedIPv4)
		}
		if ipv6Changed {
			network.IPv6 = db.InstanceValue(resolvedIPv6)
		}
		updates := []string{"last_synced_at = ?", "updated_at = ?"}
		args := []any{time.Now().UTC(), time.Now().UTC()}
		if ipv4Changed {
			updates = append(updates, "ipv4 = ?")
			args = append(args, resolvedIPv4)
		}
		if ipv6Changed {
			updates = append(updates, "ipv6 = ?")
			args = append(args, resolvedIPv6)
		}
		args = append(args, instanceID)
		if err := db.DB.WithContext(ctx).Exec(
			"UPDATE instances SET "+strings.Join(updates, ", ")+" WHERE id = ?", args...).Error; err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}

	proxySitesUpdated := 0
	if ipv4Changed && resolvedIPv4 != nil && host.CaddyEnabled {
		sites, siteErr := db.ProxySitesByInstanceID(ctx, instanceID)
		if siteErr != nil {
			instancesRespondInternal(c, siteErr)
			return
		}
		if len(sites) > 0 {
			if caddy, caddyErr := lib.GetCaddyClientForHost(lib.CaddyHostRef{
				ID: host.ID, CaddyEnabled: &host.CaddyEnabled, CaddyPort: instancesIntPtr(int(host.CaddyPort)),
			}, 0); caddyErr == nil {
				for _, site := range sites {
					if site.Status != models.ProxySiteStatusActive || !site.Enabled {
						continue
					}
					if delErr := caddy.DeleteSite(ctx, site.Domain); delErr != nil {
						continue
					}
					if addErr := caddy.AddSite(ctx, site.Domain, *resolvedIPv4, int(site.TargetPort), site.HTTPSEnabled); addErr == nil {
						proxySitesUpdated++
					}
				}
			}
		}
	}

	payload := gin.H{
		"success":       true,
		"statusChanged": statusChanged,
		"ipv4Changed":   ipv4Changed,
		"ipv6Changed":   ipv6Changed,
	}
	if statusChanged {
		payload["from"] = currentStatus
		payload["to"] = incusStatus
	}
	if currentStatus == string(models.InstanceStatusSuspended) {
		payload["currentStatus"] = currentStatus
	} else {
		payload["currentStatus"] = incusStatus
	}
	if ipv4Changed {
		payload["oldIpv4"] = oldIPv4
		payload["newIpv4"] = resolvedIPv4
		payload["proxySitesUpdated"] = proxySitesUpdated
	}
	if ipv6Changed {
		payload["oldIpv6"] = oldIPv6
		payload["newIpv6"] = resolvedIPv6
	}
	c.JSON(http.StatusOK, payload)
}

// instancesStringPtrEqual mirrors `a === b` for optional strings.
func instancesStringPtrEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// instancesResolveStateAddresses mirrors the IPv4/IPv6 extraction of
// lib/instance-network-sync.ts closely enough for the sync route: eth0's first
// global IPv4 and the routed/observed IPv6.
//
// NOT a full port of resolveInstanceNetworkAddresses: the configured-IPv6 device
// scan and the IpAddress table reconciliation are not implemented (reported).
func instancesResolveStateAddresses(instance *db.InstanceDetailRecord, incusInstance *lib.IncusInstance,
	rawState json.RawMessage) (ipv4 *string, ipv6 *string) {
	var payload struct {
		Network map[string]struct {
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	}
	if err := json.Unmarshal(rawState, &payload); err != nil {
		return instance.IPv4, instance.IPv6
	}
	findAddress := func(interfaceName, family string) *string {
		entry, ok := payload.Network[interfaceName]
		if !ok {
			return nil
		}
		for _, address := range entry.Addresses {
			if address.Family != family || address.Address == "" {
				continue
			}
			if family == "inet" && address.Scope == "link" {
				continue
			}
			value := address.Address
			return &value
		}
		return nil
	}
	resolvedIPv4 := findAddress("eth0", "inet")
	if resolvedIPv4 == nil {
		resolvedIPv4 = instance.IPv4
	}
	var resolvedIPv6 *string
	configured := instancesConfiguredPrimaryIPv6(incusInstance)
	if configured != nil {
		resolvedIPv6 = configured
	}
	if resolvedIPv6 == nil {
		resolvedIPv6 = findAddress("eth1", "inet6")
	}
	if resolvedIPv6 == nil {
		resolvedIPv6 = instance.IPv6
	}
	return resolvedIPv4, resolvedIPv6
}

// instancesConfiguredPrimaryIPv6 mirrors extractConfiguredPrimaryIpv6 (devices /
// expanded_devices eth1 ipv6.address, comma list, first entry).
func instancesConfiguredPrimaryIPv6(instance *lib.IncusInstance) *string {
	if instance == nil {
		return nil
	}
	devices := instance.Devices
	if devices == nil {
		devices = instance.ExpandedDevices
	}
	if devices == nil {
		return nil
	}
	device, ok := devices["eth1"]
	if !ok {
		return nil
	}
	raw, ok := device["ipv6.address"].(string)
	if !ok || raw == "" {
		return nil
	}
	first := strings.TrimSpace(strings.Split(raw, ",")[0])
	if first == "" {
		return nil
	}
	return &first
}

// ==================== route 14: change-host options ====================

// instancesChangeHostOptionsHandler mirrors GET /:id/change-host-options.
func instancesChangeHostOptionsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil || instance.Status == models.InstanceStatusDeleted {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	options, err := instancesBuildChangeHostOptions(ctx, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	c.JSON(http.StatusOK, options)
}

// ==================== route 15: change-host ====================

// instancesChangeHostHandler mirrors POST /:id/change-host.
func instancesChangeHostHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value,
		[]string{"targetHostId", "sshKeyId"},
		[]ajvFieldSpec{
			instancesIntegerField("targetHostId", 1, math.NaN()),
			instancesIntegerField("sshKeyId", 1, math.NaN()),
		})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	targetHostID := int32(instancesCoercedNumber(coerced, "targetHostId"))
	sshKeyID := int32(instancesCoercedNumber(coerced, "sshKeyId"))

	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil || instance.Status == models.InstanceStatusDeleted {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, instance)
		return
	}
	if instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusStopped &&
		instance.Status != models.InstanceStatusError {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Instance status does not allow host change", "code": "INSTANCE_STATUS_NOT_ALLOWED"})
		return
	}

	activeTask, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeTask != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": "Instance has an active task", "code": "TASK_IN_PROGRESS",
			"taskId": activeTask.ID, "taskType": activeTask.TaskType, "status": activeTask.Status,
		})
		return
	}
	var activeRestore, activeUpload struct {
		ID int32
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`,
		instanceID).Scan(&activeRestore).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeRestore.ID != 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Instance has an active restore task", "code": "RESTORE_IN_PROGRESS"})
		return
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`,
		instanceID).Scan(&activeUpload).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeUpload.ID != 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Instance has an active backup upload task", "code": "UPLOAD_IN_PROGRESS"})
		return
	}

	keyRecord, err := db.GetSSHKeyByID(ctx, sshKeyID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if keyRecord == nil || keyRecord.UserID != instance.UserID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSshKeyNotOwned, "")
		return
	}

	options, err := instancesBuildChangeHostOptions(ctx, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if options.PackageID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Instance does not belong to a package", "code": "CHANGE_HOST_NO_PACKAGE"})
		return
	}
	if len(options.Hosts) <= 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Package has no other host", "code": "CHANGE_HOST_SINGLE_HOST"})
		return
	}
	var targetOption *instancesChangeHostOption
	for index := range options.Hosts {
		if options.Hosts[index].ID == targetHostID {
			targetOption = &options.Hosts[index]
			break
		}
	}
	if targetOption == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Target host is not bound to this package", "code": "CHANGE_HOST_TARGET_NOT_BOUND"})
		return
	}
	if !targetOption.CanChange {
		payload := gin.H{"error": "Target host is not available", "code": "CHANGE_HOST_TARGET_UNAVAILABLE"}
		if targetOption.UnavailableReason != nil {
			payload["reason"] = *targetOption.UnavailableReason
		}
		c.JSON(http.StatusBadRequest, payload)
		return
	}
	targetHost, err := db.HostsGetByID(ctx, targetHostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if targetHost == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	task, err := instancesCreateTaskOrConflict(c, db.CreateInstanceTaskData{
		InstanceID:   instanceID,
		HostID:       targetHostID,
		UserID:       claims.ID,
		TaskType:     models.InstanceTaskTypeChangeHost,
		TargetHostID: &targetHostID,
		SSHKeyID:     &sshKeyID,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.change_host",
		fmt.Sprintf("Queued host change for instance %q from host %q to %q",
			instance.Name, instancesInstanceHostLabel(instance), targetHost.Name),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusAccepted, gin.H{"message": "Instance host change task queued", "taskId": task.ID, "status": task.Status})
}

// ==================== routes 16-17: rebuild / recreate ====================

// instancesRebuildHandler serves POST /:id/rebuild (stopped + owner only).
func instancesRebuildHandler(c *gin.Context) {
	instancesRebuildOrRecreate(c, models.InstanceTaskTypeRebuild, models.OperationTypeReinstallInstance)
}

// instancesRecreateHandler serves POST /:id/recreate (any status, owner only).
func instancesRecreateHandler(c *gin.Context) {
	instancesRebuildOrRecreate(c, models.InstanceTaskTypeRecreate, models.OperationTypeRecreateInstance)
}

// instancesRebuildOrRecreate mirrors the shared body of rebuild and recreate.
func instancesRebuildOrRecreate(c *gin.Context, taskType models.InstanceTaskType, operation models.OperationType) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value, nil, []ajvFieldSpec{
		instancesStringField("image", -1),
		instancesStringField("imageAlias", -1),
		instancesIntegerField("sshKeyId", math.NaN(), math.NaN()),
	})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if raw, present := body.Value.(map[string]any)["customInitCommandIds"]; present {
		if _, ok := instancesValidateIntArray(raw, 20); !ok {
			respondAJVValidation(c, "must be array")
			return
		}
	}

	imageAlias := instancesCoercedString(coerced, "image")
	if imageAlias == "" {
		imageAlias = instancesCoercedString(coerced, "imageAlias")
	}
	if imageAlias == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceImageRequired, "")
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, instance)
		return
	}
	if taskType == models.InstanceTaskTypeRebuild && instance.Status != models.InstanceStatusStopped {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStopRequired, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	blocked, err := instancesRejectActiveWorkflowConflict(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if blocked {
		return
	}
	verified, err := instancesRequireVerifiedOperation(c, claims.ID, operation, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !verified {
		return
	}

	validImage, err := db.IsValidSystemImage(ctx, imageAlias)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !validImage {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageNotFound, "")
		return
	}
	memoryCompatible, err := db.IsImageCompatibleWithMemory(ctx, imageAlias, int(instance.Memory))
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !memoryCompatible {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageMemoryIncompatible, "")
		return
	}

	var pkg *db.PackageDetailRow
	if instance.PackageID != nil {
		pkg, err = db.GetPackageById(ctx, *instance.PackageID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	instanceType := "container"
	if pkg != nil && pkg.InstanceType == "vm" {
		instanceType = "vm"
	}
	availability, err := db.GetSystemImageAvailabilityForHost(ctx, imageAlias, instance.HostID,
		db.SystemImageAvailabilityOptions{InstanceType: &instanceType, Memory: instancesIntPtr(int(instance.Memory))})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !availability.OK {
		switch availability.Reason {
		case "host_not_found":
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		case "image_not_found":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageNotFound, "")
		case "memory_incompatible":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageMemoryIncompatible, "")
		case "instance_type_mismatch":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrImageTypeMismatch, "")
		case "host_instance_type_mismatch":
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostInstanceTypeMismatch, "")
		default:
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceImageUnavailable, "")
		}
		return
	}

	var sshKeyID *int32
	if raw, present := coerced.Values["sshKeyId"]; present && raw != nil {
		value := int32(raw.(float64))
		sshKeyID = &value
		keyRecord, keyErr := db.GetSSHKeyByID(ctx, value)
		if keyErr != nil {
			instancesRespondInternal(c, keyErr)
			return
		}
		if keyRecord == nil || keyRecord.UserID != claims.ID {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSshKeyNotOwned, "")
			return
		}
	}

	var customInitCommandIDs []int32
	if raw, present := body.Value.(map[string]any)["customInitCommandIds"]; present {
		customInitCommandIDs, _ = instancesValidateIntArray(raw, 20)
		if len(customInitCommandIDs) > 0 {
			commandValidation, cmdErr := db.ValidateCustomInitCommandsOwnership(ctx, customInitCommandIDs, claims.ID)
			if cmdErr != nil {
				instancesRespondInternal(c, cmdErr)
				return
			}
			if !commandValidation.Valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid custom init command IDs", "code": "INVALID_COMMAND_IDS"})
				return
			}
		}
	}

	task, err := instancesCreateTaskOrConflict(c, db.CreateInstanceTaskData{
		InstanceID:           instanceID,
		HostID:               instance.HostID,
		UserID:               claims.ID,
		TaskType:             taskType,
		ImageAlias:           &imageAlias,
		SSHKeyID:             sshKeyID,
		CustomInitCommandIDs: customInitCommandIDs,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance."+string(taskType),
		fmt.Sprintf("Queued %s task for instance %q with image %s", taskType, instance.Name, imageAlias),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusAccepted, gin.H{
		"message": "Instance " + string(taskType) + " task queued",
		"taskId":  task.ID,
		"status":  task.Status,
	})
}

// ==================== route 18: delete ====================

// instancesDeleteHandler mirrors DELETE /:id.
func instancesDeleteHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	reason := ""
	if body, status, message := decodeJSONRequestBody(c); status == 0 {
		if object, ok := body.Value.(map[string]any); ok && object != nil {
			if raw, present := object["reason"]; present {
				text, isString := raw.(string)
				if !isString {
					respondAJVValidation(c, "must be string")
					return
				}
				if len([]rune(text)) > 500 {
					respondAJVValidation(c, "reason is too long")
					return
				}
				reason = text
			}
		}
	} else {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	isHostOwner := host != nil && host.UserID == claims.ID
	isAdmin := claims.Role == "admin"
	isPrivilegedDeleter := isHostOwner || isAdmin

	if !isPrivilegedDeleter && instance.PackagePlanID != nil && *instance.PackagePlanID != 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "付费实例不允许删除", "code": "PAID_INSTANCE_DELETION_NOT_ALLOWED"})
		return
	}
	if instance.Status == models.InstanceStatusSuspended && !isPrivilegedDeleter {
		instancesRespondSuspended(c, instance)
		return
	}
	if !isPrivilegedDeleter && instance.PackageID != nil {
		pkg, pkgErr := db.GetPackageById(ctx, *instance.PackageID)
		if pkgErr != nil {
			instancesRespondInternal(c, pkgErr)
			return
		}
		if pkg != nil && !pkg.AllowInstanceDeletion {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Package does not allow instance deletion",
				"code":  "INSTANCE_DELETION_NOT_ALLOWED",
			})
			return
		}
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	verified, err := instancesRequireVerifiedOperation(c, claims.ID, models.OperationTypeDeleteInstance, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !verified {
		return
	}
	if host == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Host not found"})
		return
	}

	// Conflict checks before claiming the row.
	var activeRestore, activeUpload struct {
		ID int32
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`,
		instanceID).Scan(&activeRestore).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeRestore.ID != 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "该实例有正在进行的恢复任务，请等待完成或取消", "code": "RESTORE_IN_PROGRESS"})
		return
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`,
		instanceID).Scan(&activeUpload).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeUpload.ID != 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "该实例有正在进行的备份上传任务，请等待完成或取消", "code": "UPLOAD_IN_PROGRESS"})
		return
	}
	activeTask, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeTask != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": "该实例有正在进行的操作任务，请等待完成", "code": "TASK_IN_PROGRESS",
			"taskId": activeTask.ID, "taskType": activeTask.TaskType, "status": activeTask.Status,
		})
		return
	}

	claimed, err := instancesClaimInstanceForDelete(ctx, instanceID, string(instance.Status))
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !claimed {
		c.JSON(http.StatusConflict, gin.H{"error": "实例正在删除或已删除", "code": "INSTANCE_DELETE_IN_PROGRESS"})
		return
	}

	restoreStatus := func() {
		if err := db.DB.WithContext(ctx).Exec(`UPDATE instances SET status = ?, updated_at = ?
			WHERE id = ? AND status = 'deleted'`, string(instance.Status), time.Now().UTC(), instanceID).Error; err != nil {
			log.Printf("[Delete] 恢复实例状态失败: %v", err)
		}
	}

	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		restoreStatus()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取 Incus 客户端失败: " + err.Error()})
		return
	}

	proxySites, err := db.ProxySitesByInstanceID(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if len(proxySites) > 0 && host.CaddyEnabled {
		if caddy, caddyErr := lib.GetCaddyClientForHost(lib.CaddyHostRef{
			ID: host.ID, CaddyEnabled: &host.CaddyEnabled, CaddyPort: instancesIntPtr(int(host.CaddyPort)),
		}, 0); caddyErr == nil {
			for _, site := range proxySites {
				_ = caddy.DeleteSite(ctx, site.Domain)
			}
		}
	}

	portMappings, err := db.PortMappingsList(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	for _, mapping := range portMappings {
		deviceName := fmt.Sprintf("proxy-%s-%d", mapping.Protocol, mapping.PublicPort)
		_ = instancesIncusRemoveDevice(ctx, client, instance.IncusID, deviceName)
	}

	snapshots, err := db.SnapshotsListByInstanceID(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	for _, snapshot := range snapshots {
		_, _ = lib.DeleteSnapshot(ctx, client, instance.IncusID, snapshot.IncusName)
	}

	backups, err := db.GetBackupsByInstanceID(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	for _, backup := range backups {
		_, _ = lib.DeleteIncusBackup(ctx, client, instance.IncusID, backup.IncusName)
	}

	// Delete the Incus instance (wrapped in a destroy operation task).
	deleteErr := lib.RunOperationTask(ctx, host.ID, instance.IncusID, lib.OperationTaskKindDestroy, func(taskCtx context.Context) error {
		if instance.Status == models.InstanceStatusRunning {
			if stopErr := instancesIncusAction(taskCtx, client, instance.IncusID, "stop", true); stopErr != nil {
				log.Printf("[Delete] 停止实例失败，继续尝试删除: %v", stopErr)
			}
		}
		return instancesIncusDelete(taskCtx, client, instance.IncusID)
	}, nil)
	if deleteErr != nil {
		restoreStatus()
		c.JSON(http.StatusInternalServerError, gin.H{"error": deleteErr.Error()})
		return
	}

	hostOwnerRefundAmount := 0.0
	if isPrivilegedDeleter && instance.UserID != claims.ID {
		refund, refundErr := instancesCalculateInstanceRefund(ctx, instance)
		if refundErr != nil {
			instancesRespondInternal(c, refundErr)
			return
		}
		if refund.IsPaid && refund.RefundAmount > 0 {
			hostOwnerRefundAmount = refund.RefundAmount
			refundErr = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := db.AdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, instance.UserID); err != nil {
					return err
				}
				var oldBalance models.Decimal
				if err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, instance.UserID).
					Scan(&oldBalance).Error; err != nil {
					return err
				}
				refundDecimal := instancesDecimalFromJSNumber(hostOwnerRefundAmount)
				if err := tx.Exec(`UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ?`,
					refundDecimal, time.Now().UTC(), instance.UserID).Error; err != nil {
					return err
				}
				var newBalance models.Decimal
				if err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, instance.UserID).
					Scan(&newBalance).Error; err != nil {
					return err
				}
				_, logErr := instancesTxCreateBalanceLog(tx, instance.UserID, models.BalanceLogTypeRefund,
					refundDecimal, oldBalance, newBalance, instanceID, "实例被删除退款："+instance.Name)
				return logErr
			}, &sql.TxOptions{Isolation: sql.LevelSerializable})
			if refundErr != nil {
				instancesRespondInternal(c, refundErr)
				return
			}
		}
	}

	// Dependent database rows (best effort, exactly like the TypeScript try/catch).
	for _, site := range proxySites {
		_, _ = db.ProxySitesDelete(ctx, site.ID)
	}
	for _, mapping := range portMappings {
		_ = db.PortMappingsDelete(ctx, mapping.ID)
	}
	for _, snapshot := range snapshots {
		_ = db.SnapshotsDelete(ctx, snapshot.ID)
	}
	for _, backup := range backups {
		_ = db.DeleteBackup(ctx, backup.ID)
	}
	cleanupQueries := []string{
		`DELETE FROM snapshot_policies WHERE instance_id = ?`,
		`DELETE FROM backup_policies WHERE instance_id = ?`,
		`DELETE FROM traffic_snapshots WHERE instance_id = ?`,
		`DELETE FROM daily_traffic WHERE instance_id = ?`,
		`DELETE FROM ip_addresses WHERE instance_id = ?`,
		`DELETE FROM ipv6_subnets WHERE instance_id = ?`,
		`DELETE FROM restore_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
		`DELETE FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
		`DELETE FROM instance_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
	}
	for _, query := range cleanupQueries {
		if err := db.DB.WithContext(ctx).Exec(query, instanceID).Error; err != nil {
			log.Printf("[Delete] 清理附属记录失败: %v", err)
		}
	}
	if err := db.DB.WithContext(ctx).Exec(`UPDATE instance_transfers SET status = 'cancelled', cancelled_at = ?
		WHERE instance_id = ? AND status = 'pending'`, time.Now().UTC(), instanceID).Error; err != nil {
		log.Printf("[Delete] 取消转移请求失败: %v", err)
	}

	portMappingsCount := int32(len(portMappings))
	if err := db.RollbackResources(ctx, db.RollbackResourcesOptions{
		HostID:    instance.HostID,
		CPU:       instance.CPU,
		Memory:    instance.Memory,
		Disk:      instance.Disk,
		PortCount: portMappingsCount,
	}); err != nil {
		log.Printf("[Delete] 释放宿主机资源失败: %v", err)
	}
	usedResources, err := db.HostsCalculateResourcesFromInstances(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if err := db.HostsUpdateResources(ctx, instance.HostID, db.HostsResourceUpdate{
		CPUUsed:    &usedResources.CPUUsed,
		MemoryUsed: &usedResources.MemoryUsed,
		DiskUsed:   &usedResources.DiskUsed,
	}, nil); err != nil {
		instancesRespondInternal(c, err)
		return
	}
	var actualPortsUsed int64
	if err := db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM port_mappings pm
		JOIN instances i ON i.id = pm.instance_id
		WHERE i.host_id = ? AND i.status <> 'deleted'`, instance.HostID).Scan(&actualPortsUsed).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if err := db.DB.WithContext(ctx).Exec(`UPDATE hosts SET nat_ports_used_count = ?, updated_at = ?
		WHERE id = ?`, actualPortsUsed, time.Now().UTC(), instance.HostID).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}

	if isPrivilegedDeleter && instance.UserID != claims.ID {
		if strings.TrimSpace(reason) != "" {
			lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceDeletedByHostOwner, lib.EventData{
				"instanceName": instance.Name,
				"hostName":     host.Name,
				"deleteReason": strings.TrimSpace(reason),
			})
		} else {
			lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceDeleted, lib.EventData{
				"instanceName": instance.Name,
				"hostName":     host.Name,
			})
		}
	} else {
		lib.SendNotification(ctx, int64(claims.ID), lib.EventInstanceDeleted, lib.EventData{
			"instanceName": instance.Name,
			"hostName":     host.Name,
		})
	}
	if instance.PackageID != nil {
		source := "用户删除实例"
		if isPrivilegedDeleter && instance.UserID != claims.ID {
			if isAdmin {
				source = "管理员删除实例"
			} else {
				source = "节点所有者删除实例"
			}
		}
		lib.SendReleaseNotification(ctx, lib.ReleaseNotificationParams{
			PackageID: *instance.PackageID,
			CPU:       instance.CPU,
			Memory:    instance.Memory,
			Source:    source,
		})
	}

	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.delete",
		fmt.Sprintf("Deleted instance %q [host: %s, user: %d, released resources: CPU=%d%%, Memory=%dMB, Disk=%dMB, ports=%d]",
			instance.Name, host.Name, instance.UserID, instance.CPU, instance.Memory, instance.Disk, portMappingsCount),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	payload := gin.H{"message": "Instance deleted"}
	if hostOwnerRefundAmount > 0 {
		payload["refundAmount"] = hostOwnerRefundAmount
	}
	c.JSON(http.StatusOK, payload)
}

// instancesClaimInstanceForDelete mirrors claimInstanceForDelete: 4106 try-lock,
// no active tasks, atomic status transition to 'deleted'.
func instancesClaimInstanceForDelete(ctx context.Context, instanceID int32, currentStatus string) (bool, error) {
	claimed := false
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := db.TryAdvisoryTransactionLockTx(tx, db.InstanceOperationLockNamespace, instanceID)
		if err != nil {
			return err
		}
		if !locked {
			return nil
		}
		var active struct {
			Restore int32 `gorm:"column:restore"`
			Upload  int32 `gorm:"column:upload"`
			Task    int32 `gorm:"column:task"`
		}
		if err := tx.WithContext(ctx).Raw(`SELECT
				(SELECT COUNT(*) FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')) AS restore,
				(SELECT COUNT(*) FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')) AS upload,
				(SELECT COUNT(*) FROM instance_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')) AS task`,
			instanceID, instanceID, instanceID).Scan(&active).Error; err != nil {
			return err
		}
		if active.Restore > 0 || active.Upload > 0 || active.Task > 0 {
			return nil
		}
		result := tx.Exec(`UPDATE instances SET status = 'deleted', updated_at = ?
			WHERE id = ? AND status = ?`, time.Now().UTC(), instanceID, currentStatus)
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		return nil
	})
	return claimed, err
}

// instancesInstanceBillingCycle reads instances.billing_cycle (the detail record
// does not carry it).
func instancesInstanceBillingCycle(ctx context.Context, instanceID int32) *int32 {
	var value *int32
	if err := db.DB.WithContext(ctx).Raw(`SELECT billing_cycle FROM instances WHERE id = ?`, instanceID).
		Scan(&value).Error; err != nil {
		return nil
	}
	return value
}

// instancesRefundInfo mirrors calculateInstanceRefund's result.
type instancesRefundInfo struct {
	RemainingDays   int32
	RemainingValue  float64
	RefundableValue float64
	RefundAmount    float64
	MaxRefundable   float64
	IsPaid          bool
}

// instancesCalculateInstanceRefund mirrors calculateInstanceRefund (host-owner
// deletion: no fee, so refundAmount == refundableValue).
func instancesCalculateInstanceRefund(ctx context.Context, instance *db.InstanceDetailRecord) (instancesRefundInfo, error) {
	isPaid := instance.PackagePlanID != nil && instance.ExpiresAt != nil && instance.BillingPrice != nil
	if !isPaid {
		return instancesRefundInfo{}, nil
	}
	expiresAt, parseErr := time.Parse("2006-01-02T15:04:05.000Z", *instance.ExpiresAt)
	if parseErr != nil {
		return instancesRefundInfo{IsPaid: true}, nil
	}
	now := time.Now().UTC()
	if !expiresAt.After(now) {
		return instancesRefundInfo{IsPaid: true}, nil
	}
	promoEligibility, err := services.PromoGetRefundEligibility(ctx, instance.ID, nil)
	if err != nil {
		return instancesRefundInfo{}, err
	}
	if !promoEligibility.IsRefundable {
		return instancesRefundInfo{IsPaid: true}, nil
	}

	remainingDays := int32(lib.CalculateRemainingDays(expiresAt, now))
	maxRefundable, err := db.GetInstanceBillingMaxRefundable(ctx, instance.ID)
	if err != nil {
		return instancesRefundInfo{}, err
	}
	periods, err := db.GetInstanceBillingPositivePeriods(ctx, instance.ID)
	if err != nil {
		return instancesRefundInfo{}, err
	}
	remainingValue := 0.0
	for _, record := range periods {
		periodStart := record.PeriodStart.UTC()
		periodEnd := record.PeriodEnd.UTC()
		totalMs := periodEnd.Sub(periodStart).Milliseconds()
		if totalMs <= 0 || !periodEnd.After(now) {
			continue
		}
		remainingMs := periodEnd.Sub(now).Milliseconds()
		if remainingMs < 0 {
			remainingMs = 0
		}
		unusedRatio := math.Min(float64(remainingMs)/float64(totalMs), 1)
		remainingValue += record.Amount.InexactFloat64() * unusedRatio
	}
	billingCycle := instancesInstanceBillingCycle(ctx, instance.ID)
	if len(periods) == 0 && instance.BillingPrice != nil && billingCycle != nil {
		discountRate := 0.0
		if affBinding, affErr := db.AffGetInstanceBinding(ctx, instance.ID, nil); affErr == nil &&
			affBinding != nil && affBinding.AffCode.Enabled {
			discountRate = affBinding.AffCode.DiscountRate.InexactFloat64()
		}
		remainingValue = lib.CalculateRemainingValue(*instance.BillingPrice, int(*billingCycle),
			float64(remainingDays), discountRate)
	}
	remainingValue = math.Round(remainingValue*100) / 100
	refundable := math.Round(math.Min(remainingValue, maxRefundable)*100) / 100
	return instancesRefundInfo{
		RemainingDays:   remainingDays,
		RemainingValue:  remainingValue,
		RefundableValue: refundable,
		RefundAmount:    refundable,
		MaxRefundable:   maxRefundable,
		IsPaid:          true,
	}, nil
}

// ==================== route 19: clone ====================

// instancesCloneHandler mirrors POST /:id/clone.
func instancesCloneHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	source, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if source == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if source.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if source.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, source)
		return
	}
	host, err := db.HostsGetByID(ctx, source.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	if source.Status != models.InstanceStatusStopped {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStopRequired, "")
		return
	}
	blocked, err := instancesRejectActiveWorkflowConflict(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if blocked {
		return
	}

	cpuAvailable := int64(host.CPUAllowanceMax) - int64(host.CPUUsed)
	memoryAvailable := int64(host.MemoryMax) - int64(host.MemoryUsed)
	storage, err := lib.GetInstanceStorageCapacity(ctx, instanceID, int64(source.Disk), db.Pool())
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	diskAvailable := int64(0)
	if storage != nil {
		diskAvailable = storage.AvailableMB
	}
	if int64(source.CPU) > cpuAvailable {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("宿主机 CPU 资源不足，需要 %d%%，可用 %d%%", source.CPU, cpuAvailable),
			"code":  "HOST_RESOURCES_INSUFFICIENT",
		})
		return
	}
	if int64(source.Memory) > memoryAvailable {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("宿主机内存资源不足，需要 %dMB，可用 %dMB", source.Memory, memoryAvailable),
			"code":  "HOST_RESOURCES_INSUFFICIENT",
		})
		return
	}
	if int64(source.Disk) > diskAvailable {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("宿主机磁盘资源不足，需要 %dMB，可用 %dMB", source.Disk, diskAvailable),
			"code":  "HOST_RESOURCES_INSUFFICIENT",
		})
		return
	}

	task, err := instancesCreateTaskOrConflict(c, db.CreateInstanceTaskData{
		InstanceID: instanceID,
		HostID:     source.HostID,
		UserID:     claims.ID,
		TaskType:   models.InstanceTaskTypeClone,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.clone",
		fmt.Sprintf("Queued clone task for instance %q", source.Name), db.LogResultSuccess,
		db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusAccepted, gin.H{"message": "Instance clone task queued", "taskId": task.ID, "status": task.Status})
}

// ==================== route 20: rename ====================

// instancesRenameHandler mirrors PATCH /:id/rename.
func instancesRenameHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value,
		[]string{"name"}, []ajvFieldSpec{instancesStringField("name", 100)})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	name := instancesCoercedString(coerced, "name")
	if len([]rune(name)) < 1 {
		respondAJVValidation(c, "name is too short")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, instance)
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}

	oldName := instance.Name
	if err := db.UpdateInstance(ctx, instanceID, db.InstanceUpdate{Name: &name}); err != nil {
		instancesRespondInternal(c, err)
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.rename",
		fmt.Sprintf("Renamed instance from %q to %q", oldName, name), db.LogResultSuccess,
		db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{"message": "Instance renamed", "name": name})
}

// ==================== route 21: instance quota ====================

// instancesQuotaHandler mirrors PATCH /:id/quota.
func instancesQuotaHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value, nil, []ajvFieldSpec{
		{Name: "portLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 1000},
		{Name: "snapshotLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 1000},
		{Name: "backupLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 1000},
		{Name: "siteLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 1000},
	})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	if claims.Role != "admin" && host.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}

	newPortLimit := instance.PortLimit
	portProvided := false
	if value, present := instancesAJVNullableInt32(coerced, "portLimit"); present {
		newPortLimit = value
		portProvided = true
	}
	newSnapshotLimit := instance.SnapshotLimit
	snapshotProvided := false
	if value, present := instancesAJVNullableInt32(coerced, "snapshotLimit"); present {
		newSnapshotLimit = value
		snapshotProvided = true
	}
	newBackupLimit := instance.BackupLimit
	backupProvided := false
	if value, present := instancesAJVNullableInt32(coerced, "backupLimit"); present {
		newBackupLimit = value
		backupProvided = true
	}
	newSiteLimit := instance.SiteLimit
	siteProvided := false
	if value, present := instancesAJVNullableInt32(coerced, "siteLimit"); present {
		newSiteLimit = value
		siteProvided = true
	}

	portMappings, err := db.PortMappingsList(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	currentPortUsed := int32(len(portMappings))
	counts := struct {
		Snapshots int64 `gorm:"column:snapshots"`
		Backups   int64 `gorm:"column:backups"`
	}{}
	if err := db.DB.WithContext(ctx).Raw(`SELECT
			(SELECT COUNT(*) FROM snapshots WHERE instance_id = ?) AS snapshots,
			(SELECT COUNT(*) FROM backups WHERE instance_id = ? AND status <> 'deleted') AS backups`,
		instanceID, instanceID).Scan(&counts).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	currentSnapshotUsed := int32(counts.Snapshots)
	currentBackupUsed := int32(counts.Backups)

	if newPortLimit != nil && *newPortLimit < currentPortUsed {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaPortBelowUsed,
			fmt.Sprintf("used: %d, set: %d", currentPortUsed, *newPortLimit))
		return
	}
	if newSnapshotLimit != nil && *newSnapshotLimit < currentSnapshotUsed {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaSnapshotBelowUsed,
			fmt.Sprintf("used: %d, set: %d", currentSnapshotUsed, *newSnapshotLimit))
		return
	}
	if newBackupLimit != nil && *newBackupLimit < currentBackupUsed {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaBackupBelowUsed,
			fmt.Sprintf("used: %d, set: %d", currentBackupUsed, *newBackupLimit))
		return
	}
	if newPortLimit != nil && (*newPortLimit < 1 || *newPortLimit > 1000) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, "Port limit must be between 1 and 1000")
		return
	}
	if newSnapshotLimit != nil && (*newSnapshotLimit < 0 || *newSnapshotLimit > 1000) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue,
			"Snapshot limit must be between 0 and 1000 (0 = no quota)")
		return
	}
	if newBackupLimit != nil && (*newBackupLimit < 0 || *newBackupLimit > 1000) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue,
			"Backup limit must be between 0 and 1000 (0 = no quota)")
		return
	}
	if newSiteLimit != nil && (*newSiteLimit < 0 || *newSiteLimit > 1000) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue,
			"Site limit must be between 0 and 1000 (0 = no quota)")
		return
	}

	update := db.InstanceQuotaUpdate{}
	if portProvided {
		update.PortLimit = db.InstanceValue(newPortLimit)
	}
	if snapshotProvided {
		update.SnapshotLimit = db.InstanceValue(newSnapshotLimit)
	}
	if backupProvided {
		update.BackupLimit = db.InstanceValue(newBackupLimit)
	}
	if siteProvided {
		update.SiteLimit = db.InstanceValue(newSiteLimit)
	}
	if err := db.UpdateInstanceQuota(ctx, instanceID, update); err != nil {
		instancesRespondInternal(c, err)
		return
	}

	changes := make([]string, 0, 4)
	if portProvided {
		changes = append(changes, fmt.Sprintf("port: %s", instancesNullableInt32Text(newPortLimit)))
	}
	if snapshotProvided {
		if newSnapshotLimit != nil && *newSnapshotLimit == 0 {
			changes = append(changes, "snapshot: no quota")
		} else {
			changes = append(changes, "snapshot: "+instancesNullableInt32Text(newSnapshotLimit))
		}
	}
	if backupProvided {
		if newBackupLimit != nil && *newBackupLimit == 0 {
			changes = append(changes, "backup: no quota")
		} else {
			changes = append(changes, "backup: "+instancesNullableInt32Text(newBackupLimit))
		}
	}
	if siteProvided {
		if newSiteLimit != nil && *newSiteLimit == 0 {
			changes = append(changes, "site: no quota")
		} else if newSiteLimit == nil {
			changes = append(changes, "site: inherit")
		} else {
			changes = append(changes, "site: "+instancesNullableInt32Text(newSiteLimit))
		}
	}

	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.update_quota",
		fmt.Sprintf("Updated instance %q quota: %s", instance.Name, strings.Join(changes, ", ")),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{"message": "Instance quota updated"})
}

// instancesAJVNullableInt32 reads a `type: ['integer','null']` property.
func instancesAJVNullableInt32(values ajvValidated, name string) (*int32, bool) {
	raw, present := values.Values[name]
	if !present {
		return nil, false
	}
	if raw == nil {
		return nil, true
	}
	number, ok := raw.(float64)
	if !ok {
		return nil, true
	}
	value := int32(number)
	return &value, true
}

func instancesNullableInt32Text(value *int32) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatInt(int64(*value), 10)
}

// ==================== route 22: config GET ====================

// instancesConfigGetHandler mirrors GET /:id/config.
func instancesConfigGetHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}
	instanceConfig, err := db.GetInstanceConfig(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instanceConfig == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	var packageDefaults map[string]any
	ioLimitMode := "throughput"
	swapKind := "container"
	panelSwapSupported := true
	if instance.PackageID != nil {
		pkg, pkgErr := db.GetPackageById(ctx, *instance.PackageID)
		if pkgErr != nil {
			instancesRespondInternal(c, pkgErr)
			return
		}
		if pkg != nil {
			if pkg.IOLimitMode == "iops" {
				ioLimitMode = "iops"
			}
			if pkg.InstanceType == "vm" {
				swapKind = "vm"
				panelSwapSupported = false
			}
			packageDefaults = map[string]any{
				"limits_read":                pkg.LimitsRead,
				"limits_write":               pkg.LimitsWrite,
				"limits_read_iops":           pkg.LimitsReadIops,
				"limits_write_iops":          pkg.LimitsWriteIops,
				"limits_ingress":             instancesNullIfEmpty(pkg.LimitsIngress),
				"limits_egress":              instancesNullIfEmpty(pkg.LimitsEgress),
				"limits_processes":           pkg.LimitsProcesses,
				"limits_cpu_priority":        pkg.LimitsCPUPriority,
				"boot_autostart":             pkg.BootAutostart,
				"boot_autostart_priority":    pkg.BootAutostartPriority,
				"boot_autostart_delay":       pkg.BootAutostartDelay,
				"boot_host_shutdown_timeout": pkg.BootHostShutdownTimeout,
			}
		}
	}
	var currentPlan *models.PackagePlan
	if instance.PackagePlanID != nil {
		currentPlan, err = db.GetPackagePlanById(ctx, *instance.PackagePlanID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	if currentPlan != nil && packageDefaults != nil {
		speed := services.TrafficBandwidthNormalizePlanTrafficLimitSpeed(instancesStringPtr(currentPlan.TrafficLimitSpeed))
		if speed.Value != nil {
			packageDefaults["limits_ingress"] = *speed.Value
			packageDefaults["limits_egress"] = *speed.Value
		}
	}

	systemDefaults := map[string]any{
		"limits_read":                "100MB",
		"limits_write":               "100MB",
		"limits_read_iops":           500,
		"limits_write_iops":          500,
		"limits_ingress":             "300Mbit",
		"limits_egress":              "300Mbit",
		"limits_processes":           500,
		"limits_cpu_priority":        10,
		"boot_autostart":             true,
		"boot_autostart_priority":    20,
		"boot_autostart_delay":       15,
		"boot_host_shutdown_timeout": 30,
	}
	defaults := packageDefaults
	if defaults == nil {
		defaults = systemDefaults
	}

	// Typed helpers, not `value == nil`: a typed nil pointer boxed into `any` is a
	// non-nil interface and would serialise as null instead of falling back.
	pickString := func(value *string, fallback any) any {
		if value != nil {
			return *value
		}
		return fallback
	}
	pickInt32 := func(value *int32, fallback any) any {
		if value != nil {
			return *value
		}
		return fallback
	}
	pickBool := func(value *bool, fallback any) any {
		if value != nil {
			return *value
		}
		return fallback
	}
	effectiveConfig := map[string]any{
		"limits_read":                pickString(instanceConfig.LimitsRead, defaults["limits_read"]),
		"limits_write":               pickString(instanceConfig.LimitsWrite, defaults["limits_write"]),
		"limits_read_iops":           pickInt32(instanceConfig.LimitsReadIops, defaults["limits_read_iops"]),
		"limits_write_iops":          pickInt32(instanceConfig.LimitsWriteIops, defaults["limits_write_iops"]),
		"limits_ingress":             pickString(instanceConfig.LimitsIngress, defaults["limits_ingress"]),
		"limits_egress":              pickString(instanceConfig.LimitsEgress, defaults["limits_egress"]),
		"limits_processes":           pickInt32(instanceConfig.LimitsProcesses, defaults["limits_processes"]),
		"limits_cpu_priority":        pickInt32(instanceConfig.LimitsCPUPriority, defaults["limits_cpu_priority"]),
		"boot_autostart":             pickBool(instanceConfig.BootAutostart, defaults["boot_autostart"]),
		"boot_autostart_priority":    pickInt32(instanceConfig.BootAutostartPriority, defaults["boot_autostart_priority"]),
		"boot_autostart_delay":       pickInt32(instanceConfig.BootAutostartDelay, defaults["boot_autostart_delay"]),
		"boot_host_shutdown_timeout": pickInt32(instanceConfig.BootHostShutdownTimeout, defaults["boot_host_shutdown_timeout"]),
	}
	overrides := map[string]bool{
		"limits_read":                instanceConfig.LimitsRead != nil,
		"limits_write":               instanceConfig.LimitsWrite != nil,
		"limits_read_iops":           instanceConfig.LimitsReadIops != nil,
		"limits_write_iops":          instanceConfig.LimitsWriteIops != nil,
		"limits_ingress":             instanceConfig.LimitsIngress != nil,
		"limits_egress":              instanceConfig.LimitsEgress != nil,
		"limits_processes":           instanceConfig.LimitsProcesses != nil,
		"limits_cpu_priority":        instanceConfig.LimitsCPUPriority != nil,
		"boot_autostart":             instanceConfig.BootAutostart != nil,
		"boot_autostart_priority":    instanceConfig.BootAutostartPriority != nil,
		"boot_autostart_delay":       instanceConfig.BootAutostartDelay != nil,
		"boot_host_shutdown_timeout": instanceConfig.BootHostShutdownTimeout != nil,
	}

	effectiveSwapSizeMb := int32(0)
	if panelSwapSupported {
		var planSwap *int32
		if currentPlan != nil {
			planSwap = &currentPlan.SwapSize
		}
		effectiveSwapSizeMb = lib.InstanceSwapResolveEffectiveSize(instance.SwapSize, planSwap)
	}
	swapEnabled := panelSwapSupported && instance.SwapEnabled
	swapAvailable := panelSwapSupported && (swapEnabled || effectiveSwapSizeMb > 0)

	c.JSON(http.StatusOK, gin.H{
		"config":          effectiveConfig,
		"overrides":       overrides,
		"packageDefaults": defaults,
		"ioLimitMode":     ioLimitMode,
		"swap": gin.H{
			"available":       swapAvailable,
			"enabled":         swapEnabled,
			"sizeMb":          effectiveSwapSizeMb,
			"kind":            swapKind,
			"requiresRunning": swapKind == "vm",
		},
	})
}

func instancesNullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// ==================== route 23: config PATCH ====================

// instancesConfigPatchHandler mirrors PATCH /:id/config.
func instancesConfigPatchHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value, nil, []ajvFieldSpec{
		instancesIntegerField("cpu", 15, 10000),
		instancesIntegerField("memory", 128, 524288),
		{Name: "monthlyTrafficLimit", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	var cpu, memory *int32
	if raw, present := coerced.Values["cpu"]; present && raw != nil {
		cpu = instancesInt32Ptr(int32(raw.(float64)))
	}
	if raw, present := coerced.Values["memory"]; present && raw != nil {
		memory = instancesInt32Ptr(int32(raw.(float64)))
	}
	var diskValue *float64
	if object, ok := body.Value.(map[string]any); ok {
		if raw, present := object["disk"]; present {
			number, isNumber := raw.(float64)
			if !isNumber {
				respondAJVValidation(c, "must be number")
				return
			}
			if number < 512 || number > 10485760 {
				respondAJVValidation(c, "must be >= 512")
				return
			}
			diskValue = &number
		}
	}
	if cpu == nil && memory == nil && diskValue == nil && !coerced.Present["monthlyTrafficLimit"] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigNoChanges, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	if claims.Role != "admin" && host.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	if instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusStopped {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "current status: "+string(instance.Status))
		return
	}

	newCPU := instance.CPU
	if cpu != nil {
		newCPU = *cpu
	}
	newMemory := instance.Memory
	if memory != nil {
		newMemory = *memory
	}
	newDisk := instance.Disk
	if diskValue != nil {
		newDisk = int32(math.Round(*diskValue))
	}
	newTrafficLimit := instancesBigIntStringToInt64(instance.MonthlyTrafficLimit)
	if coerced.Present["monthlyTrafficLimit"] {
		raw := coerced.Values["monthlyTrafficLimit"]
		parsed, present, ok := lib.ParseNullablePostgresBigIntInput(raw)
		if present && !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid monthly traffic limit")
			return
		}
		if !present {
			newTrafficLimit = nil
		} else {
			value := int64(parsed)
			newTrafficLimit = &value
		}
	}

	if diskValue != nil && float64(newDisk) < float64(instance.Disk) {
		storageDriver := host.StorageDriver
		if storageDriver == "" {
			storageDriver = "lvm"
		}
		if storageDriver != "zfs" {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigDiskCannotShrink,
				fmt.Sprintf("current: %d MB", instance.Disk))
			return
		}
		diskUsageMB := int32(0)
		if instance.Status == models.InstanceStatusRunning {
			client, clientErr := instancesIncusClient(ctx, host)
			if clientErr != nil {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigDiskCannotShrink, "无法获取磁盘使用量，不允许缩小")
				return
			}
			rawState, stateErr := instancesIncusGetState(ctx, client, instance.IncusID)
			if stateErr != nil {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigDiskCannotShrink, "无法获取磁盘使用量，不允许缩小")
				return
			}
			var state instancesIncusState
			if err := json.Unmarshal(rawState, &state); err != nil || state.Disk == nil || state.Disk.Root == nil {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigDiskCannotShrink, "无法获取磁盘使用量，不允许缩小")
				return
			}
			diskUsageMB = int32(math.Ceil(state.Disk.Root.Usage / 1024 / 1024))
		} else {
			diskUsageMB = int32(math.Ceil(float64(instance.Disk) * 0.9))
		}
		if newDisk < diskUsageMB {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigDiskCannotShrink,
				fmt.Sprintf("不能缩小到小于已用容量 %d MB", diskUsageMB))
			return
		}
	}

	cpuDelta := newCPU - instance.CPU
	memoryDelta := newMemory - instance.Memory
	diskDelta := newDisk - instance.Disk
	trafficChanged := coerced.Present["monthlyTrafficLimit"] &&
		((newTrafficLimit == nil) != (instancesBigIntStringToInt64(instance.MonthlyTrafficLimit) == nil) ||
			(newTrafficLimit != nil && *newTrafficLimit != *instancesBigIntStringToInt64(instance.MonthlyTrafficLimit)))
	if cpuDelta == 0 && memoryDelta == 0 && diskDelta == 0 && !trafficChanged {
		c.JSON(http.StatusOK, gin.H{
			"message":  "No changes applied",
			"instance": gin.H{"id": instanceID, "cpu": instance.CPU, "memory": instance.Memory, "disk": instance.Disk},
		})
		return
	}
	if cpuDelta > 0 && host.CPUAllowanceMax > 0 && host.CPUUsed+cpuDelta > host.CPUAllowanceMax {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostResourcesInsufficient, "CPU allowance insufficient")
		return
	}
	if memoryDelta > 0 && host.MemoryMax > 0 && host.MemoryUsed+memoryDelta > host.MemoryMax {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostResourcesInsufficient, "Memory insufficient")
		return
	}

	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if err := instancesIncusPatchResourceLimits(ctx, client, instance.IncusID,
		func() *int32 {
			if cpu != nil {
				return &newCPU
			}
			return nil
		}(),
		func() *int32 {
			if memory != nil {
				return &newMemory
			}
			return nil
		}(),
		func() *int32 {
			if diskValue != nil {
				return &newDisk
			}
			return nil
		}()); err != nil {
		logUserID := int64(claims.ID)
		db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.update_config",
			fmt.Sprintf("Failed to update instance %q config: %s", instance.Name, err.Error()),
			db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrConfigUpdateFailed, err.Error())
		return
	}

	resourcesUpdate := db.InstanceResourcesUpdate{}
	if cpu != nil {
		resourcesUpdate.CPU = &newCPU
	}
	if memory != nil {
		resourcesUpdate.Memory = &newMemory
	}
	if diskValue != nil {
		resourcesUpdate.Disk = &newDisk
	}
	if coerced.Present["monthlyTrafficLimit"] {
		resourcesUpdate.MonthlyTrafficLimit = db.InstanceValue(newTrafficLimit)
	}
	if err := db.UpdateInstanceResources(ctx, instanceID, resourcesUpdate); err != nil {
		instancesRespondInternal(c, err)
		return
	}

	if cpuDelta != 0 || memoryDelta != 0 || diskDelta != 0 {
		usedResources, usedErr := db.HostsCalculateResourcesFromInstances(ctx, instance.HostID)
		if usedErr != nil {
			instancesRespondInternal(c, usedErr)
			return
		}
		if err := db.HostsUpdateResources(ctx, instance.HostID, db.HostsResourceUpdate{
			CPUUsed:    &usedResources.CPUUsed,
			MemoryUsed: &usedResources.MemoryUsed,
			DiskUsed:   &usedResources.DiskUsed,
		}, nil); err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}

	changes := make([]string, 0, 4)
	if cpu != nil {
		changes = append(changes, fmt.Sprintf("CPU: %d%% → %d%%", instance.CPU, newCPU))
	}
	if memory != nil {
		changes = append(changes, fmt.Sprintf("Memory: %dMB → %dMB", instance.Memory, newMemory))
	}
	if diskValue != nil {
		changes = append(changes, fmt.Sprintf("Disk: %dMB → %dMB", instance.Disk, newDisk))
	}
	if coerced.Present["monthlyTrafficLimit"] {
		currentLimit := instancesBigIntStringToInt64(instance.MonthlyTrafficLimit)
		currentGB := "0"
		if currentLimit != nil {
			currentGB = strconv.FormatFloat(float64(*currentLimit)/float64(1024*1024*1024), 'f', 1, 64)
		}
		newGB := "0"
		if newTrafficLimit != nil {
			newGB = strconv.FormatFloat(float64(*newTrafficLimit)/float64(1024*1024*1024), 'f', 1, 64)
		}
		changes = append(changes, fmt.Sprintf("Traffic: %sGB → %sGB", currentGB, newGB))
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.update_config",
		fmt.Sprintf("Updated instance %q config: %s", instance.Name, strings.Join(changes, ", ")),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{
		"message":  "Instance configuration updated",
		"instance": gin.H{"id": instanceID, "cpu": newCPU, "memory": newMemory, "disk": newDisk},
	})
}

// ==================== route 24: add port mapping ====================

// instancesPortsAddHandler mirrors POST /:id/ports.
func instancesPortsAddHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	coerced, validationMessage := validateAJVObject(body.Value,
		[]string{"protocol", "privatePort"},
		[]ajvFieldSpec{
			instancesEnumField("protocol", "tcp", "udp"),
			instancesIntegerField("publicPort", 1, 65535),
			instancesIntegerField("privatePort", 1, 65535),
			instancesStringField("remark", 100),
		})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	protocol := lib.InstancePortMappingProtocol(instancesCoercedString(coerced, "protocol"))
	privatePort := int32(instancesCoercedNumber(coerced, "privatePort"))
	var publicPort *int32
	if raw, present := coerced.Values["publicPort"]; present && raw != nil {
		publicPort = instancesInt32Ptr(int32(raw.(float64)))
	}
	remark := instancesCoercedOptionalString(coerced, "remark")

	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, instance)
		return
	}
	switch string(instance.NetworkMode) {
	case "nat", "nat_ipv6", "nat_ipv6_nat":
	default:
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPortMappingNatOnly, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}

	lockKey := fmt.Sprintf(instancesPortMappingLockKeyFormat, instanceID)
	portLock, err := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
		ExpireMs:        instancesIntPtr(instancesPortMappingLockExpireMs),
		WaitTimeoutMs:   instancesIntPtr(instancesPortMappingLockWaitTimeoutMs),
		RetryIntervalMs: instancesIntPtr(instancesPortMappingLockRetryInterval),
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !portLock.Success || portLock.OwnerID == "" {
		lib.RespondAPIError(c, http.StatusConflict, lib.ErrPortConflict, "Port mapping operation is busy, please retry later")
		return
	}
	defer func() { lib.ReleaseLock(context.Background(), lockKey, portLock.OwnerID) }()

	quotaCheck, err := db.CheckPortQuota(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !quotaCheck.Allowed {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaPortExceeded, quotaCheck.Message)
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	result, err := lib.AddInstancePortMapping(ctx, lib.InstancePortMappingAddInput{
		Instance:    *lib.InstancePortMappingInstanceFromDetail(instance),
		Host:        lib.InstancePortMappingHostFromRecord(host),
		Protocol:    protocol,
		PrivatePort: privatePort,
		PublicPort:  publicPort,
		Remark:      remark,
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !result.Ok {
		logUserID := int64(claims.ID)
		if result.Code == lib.ErrInternalError {
			detail := ""
			if result.Detail != nil {
				detail = *result.Detail
			}
			db.CreateLog(ctx, &logUserID, instancesLogModule, "port.add",
				"Failed to add port mapping: "+detail, db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": detail})
			return
		}
		status := http.StatusBadRequest
		switch result.Code {
		case lib.ErrPortNoAvailable:
			status = http.StatusServiceUnavailable
		case lib.ErrHostNotFound:
			status = http.StatusNotFound
		}
		details := ""
		if result.Detail != nil {
			details = *result.Detail
		}
		lib.RespondAPIError(c, status, result.Code, details)
		return
	}

	logUserID := int64(claims.ID)
	remarkText := ""
	if remark != nil && *remark != "" {
		remarkText = ", remark: " + *remark
	}
	db.CreateLog(ctx, &logUserID, instancesLogModule, "port.add",
		fmt.Sprintf("Added port mapping for instance %q [host: %s, %s %d:%d%s]",
			instance.Name, host.Name, strings.ToUpper(string(protocol)),
			result.Mapping.PublicPort, result.Mapping.PrivatePort, remarkText),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{"message": "Port mapping added", "mapping": result.Mapping})
}

// ==================== route 25: delete port mapping ====================

// instancesPortsDeleteHandler mirrors DELETE /:id/ports/:portId.
func instancesPortsDeleteHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, instanceValid := instancesParseRouteID(c.Param("id"))
	portMappingID, portValid := instancesParseRouteID(c.Param("portId"))
	if !instanceValid || !portValid || instanceID == 0 || portMappingID == 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPortMappingInvalidId, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, instance)
		return
	}
	mapping, err := db.PortMappingsGetByID(ctx, portMappingID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if mapping == nil || mapping.InstanceID != instanceID {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPortMappingNotFound, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}

	lockKey := fmt.Sprintf(instancesPortMappingLockKeyFormat, instanceID)
	portLock, err := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
		ExpireMs:        instancesIntPtr(instancesPortMappingLockExpireMs),
		WaitTimeoutMs:   instancesIntPtr(instancesPortMappingLockWaitTimeoutMs),
		RetryIntervalMs: instancesIntPtr(instancesPortMappingLockRetryInterval),
	})
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !portLock.Success || portLock.OwnerID == "" {
		lib.RespondAPIError(c, http.StatusConflict, lib.ErrPortConflict, "Port mapping operation is busy, please retry later")
		return
	}
	defer func() { lib.ReleaseLock(context.Background(), lockKey, portLock.OwnerID) }()

	logUserID := int64(claims.ID)
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil || host == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Host not found"})
		return
	}
	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		db.CreateLog(ctx, &logUserID, instancesLogModule, "port.delete",
			"Failed to delete port mapping: "+err.Error(), db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	deviceName := fmt.Sprintf("proxy-%s-%d", mapping.Protocol, mapping.PublicPort)
	if err := instancesIncusRemoveDevice(ctx, client, instance.IncusID, deviceName); err != nil {
		db.CreateLog(ctx, &logUserID, instancesLogModule, "port.delete",
			"Failed to delete port mapping: "+err.Error(), db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = instancesIncusRemoveDevice(ctx, client, instance.IncusID, deviceName+"-v6")
	if err := db.PortMappingsDelete(ctx, portMappingID); err != nil {
		db.CreateLog(ctx, &logUserID, instancesLogModule, "port.delete",
			"Failed to delete port mapping: "+err.Error(), db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	db.CreateLog(ctx, &logUserID, instancesLogModule, "port.delete",
		fmt.Sprintf("Deleted port mapping for instance %q [%s %d:%d]", instance.Name,
			strings.ToUpper(string(mapping.Protocol)), mapping.PublicPort, mapping.PrivatePort),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{"message": "Port mapping deleted"})
}

// ==================== routes 26-27: swap enable / disable ====================

// instancesSwapHandler serves POST /:id/swap/enable and /:id/swap/disable.
func instancesSwapHandler(enable bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		claims := instancesScopeUser(c)

		instanceID, valid := instancesParseRouteID(c.Param("id"))
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		if instanceID == 0 {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}
		instance, err := db.GetInstanceById(ctx, instanceID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if instance == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}
		if claims.Role != "admin" && instance.UserID != claims.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
		locked, err := instancesCheckTransferLock(c, instanceID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if locked {
			return
		}
		if instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusStopped {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "current status: "+string(instance.Status))
			return
		}
		activeTask, err := db.GetActiveTaskForInstance(ctx, instanceID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if activeTask != nil {
			c.JSON(http.StatusConflict, gin.H{
				"error": "Instance has an active task", "code": "TASK_IN_PROGRESS",
				"taskId": activeTask.ID, "taskType": activeTask.TaskType, "status": activeTask.Status,
			})
			return
		}

		var pkg *db.PackageDetailRow
		if instance.PackageID != nil {
			pkg, err = db.GetPackageById(ctx, *instance.PackageID)
			if err != nil {
				instancesRespondInternal(c, err)
				return
			}
		}
		instanceKind := "container"
		if pkg != nil && pkg.InstanceType == "vm" {
			instanceKind = "vm"
		}
		if instanceKind == "vm" {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrFeatureDisabled, "虚拟机不支持面板管理 SWAP")
			return
		}
		var currentPlan *models.PackagePlan
		if instance.PackagePlanID != nil {
			currentPlan, err = db.GetPackagePlanById(ctx, *instance.PackagePlanID)
			if err != nil {
				instancesRespondInternal(c, err)
				return
			}
		}
		var planSwap *int32
		if currentPlan != nil {
			planSwap = &currentPlan.SwapSize
		}
		effectiveSwapSizeMb := lib.InstanceSwapResolveEffectiveSize(instance.SwapSize, planSwap)

		if enable && instance.SwapEnabled {
			c.JSON(http.StatusOK, gin.H{"message": "SWAP already enabled", "swapEnabled": true, "swapSize": effectiveSwapSizeMb})
			return
		}
		if enable && effectiveSwapSizeMb <= 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrFeatureDisabled, "当前方案未提供 SWAP")
			return
		}
		if !enable && !instance.SwapEnabled {
			c.JSON(http.StatusOK, gin.H{"message": "SWAP already disabled", "swapEnabled": false, "swapSize": effectiveSwapSizeMb})
			return
		}

		host, err := db.HostsGetByID(ctx, instance.HostID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
		if host == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
			return
		}
		logUserID := int64(claims.ID)
		logAction := "instance.enable_swap"
		if !enable {
			logAction = "instance.disable_swap"
		}
		client, err := instancesIncusClient(ctx, host)
		if err != nil {
			db.CreateLog(ctx, &logUserID, instancesLogModule, logAction, err.Error(), db.LogResultFailed,
				db.LogOptionsWithInstance(&instanceID))
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrConfigUpdateFailed, err.Error())
			return
		}
		size := effectiveSwapSizeMb
		value := lib.InstanceSwapResolveIncusValue(enable, &size)
		if _, err := lib.UpdateInstance(ctx, client, instance.IncusID, map[string]any{
			"config": map[string]any{"limits.memory.swap": value},
		}); err != nil {
			db.CreateLog(ctx, &logUserID, instancesLogModule, logAction, err.Error(), db.LogResultFailed,
				db.LogOptionsWithInstance(&instanceID))
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrConfigUpdateFailed, err.Error())
			return
		}
		if enable {
			if err := db.UpdateInstanceSwap(ctx, instanceID, db.InstanceSwapUpdate{
				SwapEnabled: instancesBoolPtr(true),
				SwapSize:    db.InstanceValue(&effectiveSwapSizeMb),
			}); err != nil {
				instancesRespondInternal(c, err)
				return
			}
			db.CreateLog(ctx, &logUserID, instancesLogModule, logAction,
				fmt.Sprintf("Enabled SWAP for instance %q (%dMB)", instance.Name, effectiveSwapSizeMb),
				db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
			c.JSON(http.StatusOK, gin.H{"message": "SWAP enabled", "swapEnabled": true, "swapSize": effectiveSwapSizeMb})
			return
		}
		if err := db.UpdateInstanceSwap(ctx, instanceID, db.InstanceSwapUpdate{
			SwapEnabled: instancesBoolPtr(false),
		}); err != nil {
			instancesRespondInternal(c, err)
			return
		}
		db.CreateLog(ctx, &logUserID, instancesLogModule, logAction,
			fmt.Sprintf("Disabled SWAP for instance %q", instance.Name),
			db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
		c.JSON(http.StatusOK, gin.H{"message": "SWAP disabled", "swapEnabled": false, "swapSize": effectiveSwapSizeMb})
	}
}

// ==================== route 28: boost processes ====================

// instancesBoostProcessesHandler mirrors POST /:id/boost-processes.
func instancesBoostProcessesHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID && claims.Role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}

	var pkg *db.PackageDetailRow
	if instance.PackageID != nil {
		pkg, err = db.GetPackageById(ctx, *instance.PackageID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	instanceType := "container"
	if pkg != nil && pkg.InstanceType == "vm" {
		instanceType = "vm"
	}
	targetLimit := int32(1000)
	if instanceType == "vm" {
		targetLimit = 2000
	}

	instanceConfig, err := db.GetInstanceConfig(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	var previous *int32
	if instanceConfig != nil {
		previous = instanceConfig.LimitsProcesses
	}
	current := int32(500)
	if previous != nil {
		current = *previous
	}
	if current >= targetLimit {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Process limit already at maximum (%d)", targetLimit),
			"code":  "PROCESS_LIMIT_ALREADY_MAX",
		})
		return
	}

	if err := db.UpdateInstanceConfig(ctx, instanceID, db.InstanceConfigUpdate{
		LimitsProcesses: db.InstanceValue(&targetLimit),
	}, nil); err != nil {
		instancesRespondInternal(c, err)
		return
	}

	client, clientErr := instancesIncusClient(ctx, host)
	var incusErr error
	if clientErr != nil {
		incusErr = clientErr
	} else {
		_, incusErr = lib.UpdateInstance(ctx, client, instance.IncusID, map[string]any{
			"config": map[string]any{"limits.processes": strconv.FormatInt(int64(targetLimit), 10)},
		})
	}
	logUserID := int64(claims.ID)
	if incusErr != nil {
		errorMessage := incusErr.Error()
		rollbackErr := db.UpdateInstanceConfig(ctx, instanceID, db.InstanceConfigUpdate{
			LimitsProcesses: db.InstanceValue(previous),
		}, nil)
		if rollbackErr != nil {
			db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.boost_processes",
				fmt.Sprintf("Failed to apply process limit for instance %q and database rollback failed", instance.Name),
				db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "Process limit boost partially failed",
				"code":  "PROCESS_LIMIT_BOOST_PARTIAL_FAILURE",
				"details": gin.H{
					"incusError":            errorMessage,
					"rollbackError":         rollbackErr.Error(),
					"databaseLimit":         targetLimit,
					"previousDatabaseLimit": previous,
					"incusLimitApplied":     "unknown",
				},
			})
			return
		}
		db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.boost_processes",
			fmt.Sprintf("Failed to apply process limit for instance %q; database change rolled back", instance.Name),
			db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrConfigUpdateFailed,
			errorMessage+"; database change rolled back")
		return
	}

	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.boost_processes",
		fmt.Sprintf("Boosted process limit for instance %q to %d", instance.Name, targetLimit),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{"message": "Process limit boosted successfully", "newLimit": targetLimit})
}

// ==================== routes 29-31: instance tasks ====================

// instancesTaskHandler mirrors GET /:id/task.
func instancesTaskHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := instancesRequireInstancePermission(c, claims, instance)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if !allowed {
		return
	}
	task, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		c.JSON(http.StatusOK, gin.H{"task": nil})
		return
	}
	queuePosition := int64(0)
	if task.Status == models.InstanceTaskStatusPENDING {
		queuePosition, err = db.GetTaskQueuePosition(ctx, task.ID, task.HostID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"task": gin.H{
		"id":            task.ID,
		"taskType":      task.TaskType,
		"status":        task.Status,
		"progress":      task.Progress,
		"error":         task.Error,
		"queuePosition": queuePosition,
		"createdAt":     task.CreatedAt,
		"startedAt":     task.StartedAt,
		"finishedAt":    task.FinishedAt,
	}})
}

// instancesTaskDetailHandler mirrors GET /tasks/:taskId.
func instancesTaskDetailHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	taskIDNum, valid := instancesParseRouteID(c.Param("taskId"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if taskIDNum == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found", "code": "TASK_NOT_FOUND"})
		return
	}
	task, err := db.GetInstanceTaskById(ctx, taskIDNum)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found", "code": "TASK_NOT_FOUND"})
		return
	}
	hasPermission := task.UserID == claims.ID || claims.Role == "admin"
	if !hasPermission {
		host, hostErr := db.HostsGetByID(ctx, task.HostID)
		if hostErr != nil {
			instancesRespondInternal(c, hostErr)
			return
		}
		if host != nil && host.UserID == claims.ID {
			hasPermission = true
		}
	}
	if !hasPermission {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	queuePosition := int64(0)
	if task.Status == models.InstanceTaskStatusPENDING {
		queuePosition, err = db.GetTaskQueuePosition(ctx, task.ID, task.HostID)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	instance, err := db.GetInstanceById(ctx, task.InstanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	instanceName := any(nil)
	if instance != nil {
		instanceName = instance.Name
	}
	c.JSON(http.StatusOK, gin.H{"task": gin.H{
		"id":            task.ID,
		"instanceId":    task.InstanceID,
		"instanceName":  instanceName,
		"taskType":      task.TaskType,
		"status":        task.Status,
		"progress":      task.Progress,
		"error":         task.Error,
		"queuePosition": queuePosition,
		"createdAt":     task.CreatedAt,
		"startedAt":     task.StartedAt,
		"finishedAt":    task.FinishedAt,
		"newInstanceId": task.NewInstanceID,
	}})
}

// instancesTaskCancelHandler mirrors DELETE /tasks/:taskId.
func instancesTaskCancelHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	taskIDNum, valid := instancesParseRouteID(c.Param("taskId"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if taskIDNum == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found", "code": "TASK_NOT_FOUND"})
		return
	}
	task, err := db.GetInstanceTaskById(ctx, taskIDNum)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found", "code": "TASK_NOT_FOUND"})
		return
	}
	if task.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if task.Status != models.InstanceTaskStatusPENDING {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "只能取消等待中的任务", "code": "TASK_CANNOT_CANCEL", "currentStatus": task.Status,
		})
		return
	}
	cancelled, err := db.CancelInstanceTask(ctx, taskIDNum)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if cancelled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "取消任务失败", "code": "CANCEL_FAILED"})
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "task.cancel",
		fmt.Sprintf("Cancelled %s task #%d for instance #%d", task.TaskType, task.ID, task.InstanceID),
		db.LogResultSuccess, db.LogOptionsWithInstance(&task.InstanceID))
	c.JSON(http.StatusOK, gin.H{"message": "Task cancelled", "task": gin.H{"id": cancelled.ID, "status": cancelled.Status}})
}

// ==================== routes 32-33: cloud-init status ====================

var instancesCloudInitReadyStates = map[string]bool{
	"manual": true, "done": true, "done_with_errors": true, "disabled": true, "unsupported": true,
}

var instancesCloudInitCacheableStates = map[string]bool{
	"manual": true, "done": true, "done_with_errors": true, "disabled": true, "unsupported": true,
}

// instancesCloudInitResponse mirrors CloudInitStatusResponse.
func instancesCloudInitResponse(state, source, message string, detectedAt time.Time, manualOverride bool) gin.H {
	return gin.H{
		"ready":          instancesCloudInitReadyStates[state],
		"state":          state,
		"source":         source,
		"manualOverride": manualOverride,
		"message":        message,
		"detectedAt":     instancesISOTime(detectedAt),
	}
}

// instancesCloudInitCached mirrors getCachedCloudInitStatus.
func instancesCloudInitCached(instance *db.InstanceDetailRecord) gin.H {
	detectedAt := instance.CloudInitLastCheckedAt
	if detectedAt == nil {
		detectedAt = instance.CloudInitManualCompletedAt
	}
	if detectedAt == nil {
		detectedAt = instance.CloudInitCompletedAt
	}
	if detectedAt == nil {
		return nil
	}
	state := ""
	if instance.CloudInitState != nil {
		state = *instance.CloudInitState
	}
	source := "unknown"
	if instance.CloudInitSource != nil && *instance.CloudInitSource != "" {
		source = *instance.CloudInitSource
	}
	if state == "" || !instancesCloudInitCacheableStates[state] {
		return nil
	}
	message := "Image does not appear to use cloud-init"
	switch state {
	case "manual":
		message = "Cloud-init marked complete manually"
	case "done":
		message = "Cloud-init completed"
	case "done_with_errors":
		message = "Cloud-init completed with warnings"
	case "disabled":
		message = "Cloud-init is disabled for this instance"
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", *detectedAt)
	if err != nil {
		parsed = time.Now().UTC()
	}
	return instancesCloudInitResponse(state, source, message, parsed, state == "manual")
}

// instancesCloudInitHasIssues mirrors hasIssues.
func instancesCloudInitHasIssues(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case []any:
		for _, item := range typed {
			if instancesCloudInitHasIssues(item) {
				return true
			}
		}
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		for key, nested := range typed {
			switch key {
			case "errors", "_errors", "recoverable_errors", "recoverableErrors":
				if instancesCloudInitHasIssues(nested) {
					return true
				}
				continue
			}
			if nested != nil {
				switch nested.(type) {
				case map[string]any, []any:
					if instancesCloudInitHasIssues(nested) {
						return true
					}
				}
			}
		}
		return false
	default:
		return false
	}
}

// instancesCloudInitNestedString mirrors getNestedString over getNestedValue.
func instancesCloudInitNestedString(source any, paths [][]string) string {
	for _, path := range paths {
		current := source
		valid := true
		for _, segment := range path {
			object, ok := current.(map[string]any)
			if !ok {
				valid = false
				break
			}
			next, present := object[segment]
			if !present {
				valid = false
				break
			}
			current = next
		}
		if !valid {
			continue
		}
		if text, ok := current.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// instancesCloudInitParseStatus mirrors parseStatusPayload.
func instancesCloudInitParseStatus(rawContent string) (string, string, bool) {
	if strings.TrimSpace(rawContent) == "" {
		return "", "", false
	}
	var parsed any
	if err := json.Unmarshal([]byte(rawContent), &parsed); err != nil {
		return "", "", false
	}
	rawStatus := strings.ToLower(instancesCloudInitNestedString(parsed, [][]string{
		{"extended_status"}, {"status"}, {"v1", "extended_status"}, {"v1", "status"}, {"v1", "stage"}, {"stage"},
	}))
	hasErrors := instancesCloudInitHasIssues(parsed)
	if strings.Contains(rawStatus, "disabled") {
		return "disabled", "Cloud-init is disabled for this instance", true
	}
	if strings.Contains(rawStatus, "done") || strings.Contains(rawStatus, "finished") || strings.Contains(rawStatus, "final") {
		if hasErrors || strings.Contains(rawStatus, "error") || strings.Contains(rawStatus, "degraded") {
			return "done_with_errors", "Cloud-init completed with warnings", true
		}
		return "done", "Cloud-init completed", true
	}
	if strings.Contains(rawStatus, "running") || strings.Contains(rawStatus, "init") ||
		strings.Contains(rawStatus, "modules") || strings.Contains(rawStatus, "not started") ||
		strings.Contains(rawStatus, "not run") {
		return "running", "Cloud-init is still running", true
	}
	if strings.Contains(rawStatus, "error") || strings.Contains(rawStatus, "degraded") {
		return "running", "Cloud-init reported issues before completion", true
	}
	return "", "", false
}

// instancesCloudInitReadFile mirrors readInstanceFile.
func instancesCloudInitReadFile(ctx context.Context, client *lib.IncusClient, instanceName, path string) (int, string) {
	content, status, err := client.ReadFile(ctx,
		"/1.0/instances/"+instanceName+"/files?path="+url.QueryEscape(path), 0)
	if err != nil {
		return status, ""
	}
	return status, content
}

// instancesCloudInitDetect mirrors detectCloudInitStatus.
func instancesCloudInitDetect(ctx context.Context, client *lib.IncusClient, instance *db.InstanceDetailRecord) gin.H {
	detectedAt := time.Now().UTC()
	if !strings.Contains(strings.ToLower(instance.Image), "/cloud") {
		return instancesCloudInitResponse("unsupported", "image", "Image does not appear to use cloud-init", detectedAt, false)
	}
	sawAgentError := false
	for _, path := range []string{instancesCloudInitStatusJSONPath, instancesCloudInitStatusJSONPathLegacy} {
		status, content := instancesCloudInitReadFile(ctx, client, instance.IncusID, path)
		if status == http.StatusOK {
			if state, message, ok := instancesCloudInitParseStatus(content); ok {
				return instancesCloudInitResponse(state, "status_json", message, detectedAt, false)
			}
			break
		}
		if status >= 500 || status == http.StatusForbidden {
			sawAgentError = true
		}
	}
	status, _ := instancesCloudInitReadFile(ctx, client, instance.IncusID, instancesCloudInitBootFinishedPath)
	if status == http.StatusOK {
		return instancesCloudInitResponse("done", "boot_finished", "Cloud-init completed", detectedAt, false)
	}
	if status >= 500 || status == http.StatusForbidden {
		sawAgentError = true
	}
	if sawAgentError {
		return instancesCloudInitResponse("agent_unavailable", "unknown", "Guest agent is not ready for cloud-init inspection", detectedAt, false)
	}
	if status == http.StatusNotFound {
		return instancesCloudInitResponse("running", "boot_finished", "Cloud-init is still running", detectedAt, false)
	}
	return instancesCloudInitResponse("unknown", "unknown", "Unable to determine cloud-init status", detectedAt, false)
}

// instancesCloudInitPersist mirrors persistCloudInitStatus.
func instancesCloudInitPersist(ctx context.Context, instanceID int32, result gin.H, manualCompletedBy *int32) error {
	detectedAt, _ := result["detectedAt"].(string)
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", detectedAt)
	if err != nil {
		parsed = time.Now().UTC()
	}
	state, _ := result["state"].(string)
	source, _ := result["source"].(string)
	ready, _ := result["ready"].(bool)
	var completedAt *time.Time
	if ready {
		value := parsed
		completedAt = &value
	}
	var manualCompletedAt *time.Time
	var manualBy *int32
	if state == "manual" {
		value := parsed
		manualCompletedAt = &value
		manualBy = manualCompletedBy
	}
	return db.DB.WithContext(ctx).Exec(`UPDATE instances SET
			cloud_init_state = ?, cloud_init_source = ?, cloud_init_last_checked_at = ?,
			cloud_init_completed_at = ?, cloud_init_manual_completed_at = ?,
			cloud_init_manual_completed_by = ?, updated_at = ?
		WHERE id = ?`,
		state, source, parsed, completedAt, manualCompletedAt, manualBy, time.Now().UTC(), instanceID).Error
}

// instancesCloudInitStatusHandler mirrors GET /:id/cloud-init-status.
func instancesCloudInitStatusHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	hasPermission := false
	if claims.Role == "admin" {
		hasPermission = true
	} else {
		hasPermission, err = instancesRequireInstancePermission(c, claims, instance)
		if err != nil {
			instancesRespondInternal(c, err)
			return
		}
	}
	if !hasPermission {
		return
	}
	if instance.Status != models.InstanceStatusRunning {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Instance must be running to check cloud-init status",
			"code":  "INSTANCE_NOT_RUNNING",
		})
		return
	}
	if instance.IncusID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Instance is not fully provisioned", "code": "INSTANCE_NOT_PROVISIONED"})
		return
	}
	if cached := instancesCloudInitCached(instance); cached != nil {
		c.JSON(http.StatusOK, cached)
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		c.JSON(http.StatusOK, instancesCloudInitResponse("unknown", "unknown",
			"Unable to check cloud-init status", time.Now().UTC(), false))
		return
	}
	result := instancesCloudInitDetect(ctx, client, instance)
	if err := instancesCloudInitPersist(ctx, instance.ID, result, nil); err != nil {
		instancesRespondInternal(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// instancesCloudInitManualCompleteHandler mirrors
// POST /:id/cloud-init-status/manual-complete.
func instancesCloudInitManualCompleteHandler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if claims.Role != "admin" && instance.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	if instance.Status != models.InstanceStatusRunning {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Instance must be running to manually complete cloud-init",
			"code":  "INSTANCE_NOT_RUNNING",
		})
		return
	}
	activeTask, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeTask != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": "Instance has an active task", "code": "TASK_IN_PROGRESS",
			"taskId": activeTask.ID, "taskType": activeTask.TaskType, "status": activeTask.Status,
		})
		return
	}
	if instance.IncusID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Instance is not fully provisioned", "code": "INSTANCE_NOT_PROVISIONED"})
		return
	}

	result := instancesCloudInitResponse("manual", "manual", "Cloud-init marked complete manually",
		time.Now().UTC(), true)
	if err := instancesCloudInitPersist(ctx, instance.ID, result, &claims.ID); err != nil {
		instancesRespondInternal(c, err)
		return
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.cloud_init_manual_complete",
		fmt.Sprintf("Manually marked cloud-init as completed for instance %q", instance.Name),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, result)
}

// ==================== route 34: reassign IPv6 ====================

// instancesReassignIPv6Handler mirrors POST /:id/reassign-ipv6.
func instancesReassignIPv6Handler(c *gin.Context) {
	ctx := c.Request.Context()
	claims := instancesScopeUser(c)

	instanceID, valid := instancesParseRouteID(c.Param("id"))
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if instanceID == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		instancesRespondSuspended(c, instance)
		return
	}
	if instance.Status != models.InstanceStatusStopped {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStopRequired, "")
		return
	}
	if instance.NetworkMode != models.NetworkModeNatIpv6 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceIpv6NotSupported, "")
		return
	}
	if instance.LastIPv6ReassignAt != nil && *instance.LastIPv6ReassignAt != "" {
		if last, parseErr := time.Parse("2006-01-02T15:04:05.000Z", *instance.LastIPv6ReassignAt); parseErr == nil {
			cooldown := 24 * time.Hour
			elapsed := time.Since(last.UTC())
			if elapsed < cooldown {
				remainingHours := int(math.Ceil((cooldown - elapsed).Hours()))
				lib.RespondAPIError(c, http.StatusTooManyRequests, lib.ErrIpv6ReassignCooldown,
					fmt.Sprintf("请等待 %d 小时后再试", remainingHours))
				return
			}
		}
	}
	locked, err := instancesCheckTransferLock(c, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if locked {
		return
	}
	activeTask, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if activeTask != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": "Instance has an active task", "code": "TASK_IN_PROGRESS",
			"taskId": activeTask.ID, "taskType": activeTask.TaskType, "status": activeTask.Status,
		})
		return
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	if host.IPv6Subnet == nil || *host.IPv6Subnet == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostNoIpv6Subnet, "")
		return
	}

	newIPv6 := ""
	for attempts := 0; attempts < 50; attempts++ {
		candidate, genErr := lib.GenerateRandomIPv6(*host.IPv6Subnet)
		if genErr != nil {
			break
		}
		exists, existsErr := db.IpAddressesExists(ctx, candidate)
		if existsErr != nil {
			break
		}
		if !exists {
			newIPv6 = candidate
			break
		}
	}
	if newIPv6 == "" {
		lib.RespondAPIError(c, http.StatusServiceUnavailable, lib.ErrIpv6PoolExhausted, "")
		return
	}

	oldIPv6 := instance.IPv6
	hostInterface := "eth0"
	if host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
		hostInterface = *host.IPv6ParentInterface
	}
	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		logUserID := int64(claims.ID)
		db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.reassign_ipv6",
			"Failed to reassign IPv6 for instance \""+instance.Name+"\": "+err.Error(),
			db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInstanceIpv6ReassignFailed, err.Error())
		return
	}
	// Rewrites eth1's ipv6.address and regenerates the cloud-init network-config
	// (the routed-interface + MAC handling lives in lib/network_payload_builder.go).
	if err := lib.NetworkPayloadSyncInstanceNetwork(ctx, client, instance.IncusID,
		lib.NetworkPayloadIPv6Config{PrimaryIP: newIPv6}, hostInterface, false); err != nil {
		logUserID := int64(claims.ID)
		db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.reassign_ipv6",
			"Failed to reassign IPv6 for instance \""+instance.Name+"\": "+err.Error(),
			db.LogResultFailed, db.LogOptionsWithInstance(&instanceID))
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInstanceIpv6ReassignFailed, err.Error())
		return
	}

	now := time.Now().UTC()
	if err := db.DB.WithContext(ctx).Exec(`UPDATE instances SET ipv6 = ?, last_ipv6_reassign_at = ?, updated_at = ?
		WHERE id = ?`, newIPv6, now, now, instanceID).Error; err != nil {
		instancesRespondInternal(c, err)
		return
	}
	if oldIPv6 != nil && *oldIPv6 != "" {
		if err := db.DB.WithContext(ctx).Exec(`DELETE FROM ip_addresses
			WHERE instance_id = ? AND type = 'inet6' AND address = ?`, instanceID, *oldIPv6).Error; err != nil {
			log.Printf("[ReassignIPv6] 删除旧 IPv6 记录失败: %v", err)
		}
	}
	if _, err := db.IpAddressesCreate(ctx, db.IpAddressesCreateData{
		Address: newIPv6, Type: models.IpTypeInet6, IsPrimary: true, Device: "eth1", InstanceID: instanceID,
	}); err != nil {
		log.Printf("[ReassignIPv6] 创建新 IPv6 记录失败: %v", err)
	}
	logUserID := int64(claims.ID)
	db.CreateLog(ctx, &logUserID, instancesLogModule, "instance.reassign_ipv6",
		fmt.Sprintf("Reassigned IPv6 for instance %q: %s → %s", instance.Name,
			instancesDerefOr(oldIPv6, "none"), newIPv6),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	c.JSON(http.StatusOK, gin.H{
		"message": "IPv6 已重新分配，请重装系统使其生效",
		"oldIpv6": oldIPv6,
		"newIpv6": newIPv6,
	})
}

// ==================== route registration ====================

// RegisterInstances mounts the instance routes under /api/instances, matching
// `fastify.register(instanceRoutes, { prefix: '/api/instances' })`.
func RegisterInstances(r *gin.Engine) {
	group := r.Group("/api/instances")

	group.GET("", middleware.Authenticate(), instancesListHandler)
	group.PATCH("/:id/order", middleware.Authenticate(), instancesOrderHandler)
	group.GET("/available-hosts", middleware.Authenticate(), instancesAvailableHostsHandler)
	group.POST("", middleware.AuthenticateUser(), instancesCreateHandler)

	group.GET("/:id", middleware.Authenticate(), instancesDetailHandler)
	group.GET("/:id/password", middleware.Authenticate(), instancesPasswordHandler)
	group.GET("/:id/stats", middleware.Authenticate(), instancesStatsHandler)

	group.POST("/:id/start", middleware.Authenticate(), instancesLifecycleHandler(models.InstanceTaskTypeStart))
	group.POST("/:id/stop", middleware.Authenticate(), instancesLifecycleHandler(models.InstanceTaskTypeStop))
	group.POST("/:id/restart", middleware.Authenticate(), instancesLifecycleHandler(models.InstanceTaskTypeRestart))
	group.POST("/:id/suspend", middleware.Authenticate(), instancesSuspendHandler)
	group.POST("/:id/unsuspend", middleware.Authenticate(), instancesUnsuspendHandler)
	group.POST("/:id/sync-status", middleware.Authenticate(), instancesSyncStatusHandler)

	group.GET("/:id/change-host-options", middleware.Authenticate(), instancesChangeHostOptionsHandler)
	group.POST("/:id/change-host", middleware.Authenticate(), instancesChangeHostHandler)
	group.POST("/:id/rebuild", middleware.AuthenticateUser(), instancesRebuildHandler)
	group.POST("/:id/recreate", middleware.AuthenticateUser(), instancesRecreateHandler)

	group.DELETE("/:id", middleware.Authenticate(), instancesDeleteHandler)

	group.POST("/:id/ports", middleware.AuthenticateUser(), instancesPortsAddHandler)
	group.DELETE("/:id/ports/:portId", middleware.AuthenticateUser(), instancesPortsDeleteHandler)

	group.PATCH("/:id/quota", middleware.Authenticate(), instancesQuotaHandler)
	group.PATCH("/:id/config", middleware.Authenticate(), instancesConfigPatchHandler)
	group.POST("/:id/clone", middleware.AuthenticateUser(), instancesCloneHandler)
	group.PATCH("/:id/rename", middleware.AuthenticateUser(), instancesRenameHandler)
	group.GET("/:id/config", middleware.Authenticate(), instancesConfigGetHandler)

	group.POST("/:id/swap/enable", middleware.Authenticate(), instancesSwapHandler(true))
	group.POST("/:id/swap/disable", middleware.Authenticate(), instancesSwapHandler(false))
	group.POST("/:id/boost-processes", middleware.Authenticate(), instancesBoostProcessesHandler)

	group.GET("/:id/task", middleware.Authenticate(), instancesTaskHandler)
	group.GET("/tasks/:taskId", middleware.Authenticate(), instancesTaskDetailHandler)
	group.DELETE("/tasks/:taskId", middleware.AuthenticateUser(), instancesTaskCancelHandler)

	group.GET("/:id/cloud-init-status", middleware.Authenticate(), instancesCloudInitStatusHandler)
	group.POST("/:id/cloud-init-status/manual-complete", middleware.Authenticate(), instancesCloudInitManualCompleteHandler)
	group.POST("/:id/reassign-ipv6", middleware.AuthenticateUser(), instancesReassignIPv6Handler)
}

func init() {
	Register(RegisterInstances)
}
