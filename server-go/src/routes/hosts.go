// Port of server/src/routes/hosts.ts (7987 lines, 63 routes).
//
// Mounting notes (observable on the wire):
//
//   - app.ts registers this plugin with `{ prefix: '/api/hosts' }`. Fastify's
//     `prefixTrailingSlash` defaults to 'both', so `/api/hosts` and `/api/hosts/`
//     are BOTH valid. Both spellings are registered here.
//   - Gin allows one wildcard name per path position tree-wide. The /api/hosts
//     tree already uses `:hostId` (batch_config.go, redeem_codes.go, storage_
//     encryption.go, egress.go, gpus.go, traffic.go, node_security.go), so this
//     module uses `:hostId` too — `:id` panics at startup with
//     "'id' in new path '/api/hosts/:id' conflicts with existing wildcard
//     ':hostId'". The Fastify source calls the parameter `:id`.
//   - Public (no JWT) routes: GET /install.sh, GET /install.sh/:token,
//     GET /cert/:token, GET /:hostId/agent/logs/stream. They are authorised by a
//     one-time install token / log-stream ticket instead.
//
// Seams: everything that lives in a module that is NOT ported yet is a package
// level function variable whose default returns an explicit, logged failure (never
// a silent success). Those routes keep their complete Fastify-side validation and
// permission chain; only the execution step fails. See the seam block below.
//
// Time: every time.Time that reaches SQL is `.UTC()` (WAVE_D_SPEC §0.2b).
package routes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants (mirroring the TS module header) ====================

const (
	hostsListMaxPageSize          = 100
	hostsAuditHistoryMaxPageSize  = 50
	hostsAuditIgnoreMaxExpiresDay = 365
	hostsAuditKillProcessMinPID   = 2
	hostsAuditKillProcessMaxPID   = 4194304
	hostsBatchMigrateMaxInstances = 30
	hostsBatchInstanceMaxItems    = 100
	hostsGiftDaysMax              = 365
	hostsRenewalPriceMax          = 99999

	// hostsLogModule is the `module` column every host action logs under.
	hostsLogModule = "host"

	// hostsMaxSafeInteger mirrors Number.MAX_SAFE_INTEGER.
	hostsMaxSafeInteger = int64(9007199254740991)
	// hostsMaxInt32 is the largest id Prisma can bind to an Int4 column. The
	// TypeScript parses route ids as JS numbers and lets Prisma reject the
	// overflow with a 500; this port answers 400 INVALID_ID instead (documented).
	hostsMaxInt32 = int64(math.MaxInt32)
)

// hostsPositiveRouteIDPattern mirrors POSITIVE_ROUTE_ID_PATTERN.
var hostsPositiveRouteIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// ==================== seams for modules that are not ported yet ====================

// errHostsSeam renders the explicit failure the seams below return. It is never
// disguised as a success: the message names the missing module so an operator can
// tell "not ported" apart from "the node said no".
func errHostsSeam(component string) error {
	return fmt.Errorf("宿主机模块依赖尚未移植（seam: %s）", component)
}

// HostsStoragePoolsSnapshot mirrors `storedStoragePools(hostId)` from
// lib/storage/read-model.ts (wave C storage pools are not ported).
var HostsStoragePoolsSnapshot = func(ctx context.Context, hostID int32) (any, error) {
	return nil, errHostsSeam("lib/storage/read-model.ts#storedStoragePools")
}

// HostsStoragePoolCreateData carries the resolved create/import arguments.
type HostsStoragePoolCreateData struct {
	HostID       int32
	Name         string
	Driver       string
	Source       string
	Size         string
	LoopDir      string
	Preallocate  bool
	UseLoop      bool
	ZFSPoolName  string
	LVMVGName    string
	LVMThinpool  string
	LVMUseThin   *bool
	Description  *string
	Purpose      string
	UseExisting  bool
	ExistingSrc  string
	ProvisionOps bool
}

// HostsStoragePoolCreate mirrors the create/import branches of POST
// /:hostId/storage-pools (lib/storage/{provision,import}.ts).
var HostsStoragePoolCreate = func(ctx context.Context, data HostsStoragePoolCreateData) (any, int, error) {
	return nil, http.StatusInternalServerError, errHostsSeam("lib/storage/provision.ts+import.ts")
}

// HostsStoragePoolDelete mirrors DELETE /:hostId/storage-pools/:poolName.
var HostsStoragePoolDelete = func(ctx context.Context, hostID int32, poolName string) error {
	return errHostsSeam("lib/storage/delete.ts+resize.ts")
}

// HostsStoragePoolUpdate mirrors PATCH /:hostId/storage-pools/:poolName.
var HostsStoragePoolUpdate = func(ctx context.Context, hostID int32, poolName string, size, description *string, purpose *string) error {
	return errHostsSeam("lib/storage/resize.ts")
}

// HostsAgentInstall is the tuple issueHostAgentInstallToken returns.
type HostsAgentInstall struct {
	InstallToken          string
	InstallTokenExpiresAt time.Time
	AgentID               string
}

// HostsIssueAgentInstallToken mirrors issueHostAgentInstallToken(hostId, rotate)
// from lib/host-agent-credentials.ts.
var HostsIssueAgentInstallToken = func(ctx context.Context, hostID int32, rotate bool) (*HostsAgentInstall, error) {
	return nil, errHostsSeam("lib/host-agent-credentials.ts#issueHostAgentInstallToken")
}

// HostsAgentLogTicket issues and consumes the one-time SSE tickets of
// lib/action-ticket.ts (generateAgentLogStreamTicket / consumeAgentLogStreamTicket).
// The default is a faithful in-process one-time ticket: process local, 60s TTL, and
// bound to (userID, hostID, token iat) exactly like the Node implementation, which
// is also process local (WAVE_D_SPEC §0.1 single instance).
var HostsAgentLogTicket = struct {
	Issue   func(userID, hostID int32, issuedAt int64, sid string) string
	Consume func(ticket string, hostID int32) bool
}{}

// HostsAgentLogStreamSubscribe mirrors agentLogStreamManager.subscribe(hostId, client).
var HostsAgentLogStreamSubscribe = func(ctx context.Context, hostID int32, write func(string) bool) error {
	return errHostsSeam("lib/agent-log-stream.ts#agentLogStreamManager")
}

// HostsCaddyInstallRequested / HostsCaddyUninstallRequested mirror the two sets
// exported by routes/agent.ts (the agent route module is Wave D item 14).
var (
	HostsCaddyInstallRequested   = func(hostID int32) error { return errHostsSeam("routes/agent.ts#caddyInstallRequested") }
	HostsCaddyUninstallRequested = func(hostID int32) error { return errHostsSeam("routes/agent.ts#caddyUninstallRequested") }
)

// HostsManagedInstanceProvision mirrors provisionManagedInstanceAsync(instanceId,
// host, options) from lib/managed-instance-provision.ts.
var HostsManagedInstanceProvision = func(ctx context.Context, instanceID int32, host *db.HostsSelectionRecord, options map[string]any) error {
	return errHostsSeam("lib/managed-instance-provision.ts#provisionManagedInstanceAsync")
}

// HostsInstanceRefund mirrors db.calculateInstanceRefund (instance-billing wave).
var HostsInstanceRefund = func(ctx context.Context, input HostsRefundInput) (HostsRefundInfo, error) {
	return HostsRefundInfo{}, errHostsSeam("db/instance-billing.ts#calculateInstanceRefund")
}

// HostsRefundInput is the argument object of calculateInstanceRefund.
type HostsRefundInput struct {
	ID            int32
	BillingPrice  *models.Decimal
	BillingCycle  *int32
	ExpiresAt     *time.Time
	PackagePlanID *int32
}

// HostsRefundInfo is the return object of calculateInstanceRefund.
type HostsRefundInfo struct {
	IsPaid        bool
	RemainingDays int
	RefundAmount  float64
}

// HostsBatchDeleteExecutor mirrors the Incus/refund/notification half of
// DELETE /:hostId/instances/batch (lib/incus + instance-billing + notifier).
var HostsBatchDeleteExecutor = func(ctx context.Context, plans []HostsBatchDeletePlan) ([]HostsBatchDeleteResult, float64, error) {
	return nil, 0, errHostsSeam("lib/incus + instance-billing + notifier（批量删除执行段）")
}

// HostsBatchDeletePlan is one instance to delete.
type HostsBatchDeletePlan struct {
	Instance models.Instance
	Host     *db.HostsRecord
	Reason   string
	DBOnly   bool
}

// HostsBatchDeleteResult mirrors one element of the `results` array.
type HostsBatchDeleteResult struct {
	ID           int32    `json:"id"`
	Name         string   `json:"name"`
	Success      bool     `json:"success"`
	Error        *string  `json:"error,omitempty"`
	RefundAmount *float64 `json:"refundAmount,omitempty"`
}

// HostsSnapshotSyncApplier mirrors persistResolvedInstanceNetworkAddresses(
// ... resolveInstanceNetworkAddresses(...)) from lib/instance-network-sync.ts.
// The default reports "nothing changed", which is what the TypeScript reports for a
// stopped instance with no network data.
var HostsSnapshotSyncApplier = func(ctx context.Context, instance models.Instance, incusInstance, state json.RawMessage) (HostsNetworkDelta, error) {
	return HostsNetworkDelta{}, nil
}

// HostsNetworkDelta mirrors the persistResolvedInstanceNetworkAddresses result.
type HostsNetworkDelta struct {
	IPv4Changed bool    `json:"ipv4Changed"`
	OldIPv4     *string `json:"oldIpv4"`
	NewIPv4     *string `json:"newIpv4"`
	IPv6Changed bool    `json:"ipv6Changed"`
	OldIPv6     *string `json:"oldIpv6"`
	NewIPv6     *string `json:"newIpv6"`
}

// HostsAuditExecutor mirrors the incus-exec + analyse half of POST
// /:hostId/ops/instances/:instanceId/audit/scan and the kill-process path.
var HostsAuditExecutor = func(ctx context.Context, req HostsAuditExecRequest) (HostsAuditExecResult, error) {
	return HostsAuditExecResult{}, errHostsSeam("lib/instance-audit.ts（审查命令执行/解析/研判）")
}

// HostsAuditExecRequest is what the audit routes need from the audit engine.
type HostsAuditExecRequest struct {
	HostID     int32
	InstanceID int32
	IncusID    string
	Command    string
	Timeout    time.Duration
}

// HostsAuditExecResult carries stdout/stderr of one exec.
type HostsAuditExecResult struct {
	Stdout string
	Stderr string
}

// HostsMigrateExecutor mirrors the per-instance migration body of
// POST /:hostId/instances/migrate (incus-config-generator + incus + billing).
var HostsMigrateExecutor = func(ctx context.Context, plan HostsMigratePlan) (int32, error) {
	return 0, errHostsSeam("lib/incus-config-generator.ts + lib/incus（跨节点迁移执行段）")
}

// HostsIncusStopInstance mirrors `stopInstance(client, name, force)` from
// lib/incus/incus-instances.ts. The batch-suspend path logs and continues when it
// fails, exactly like the TypeScript.
var HostsIncusStopInstance = func(ctx context.Context, client *lib.IncusClient, name string, force bool) error {
	return errHostsSeam("lib/incus#stopInstance")
}

// HostsMigratePlan is one instance migration.
type HostsMigratePlan struct {
	Instance     models.Instance
	SourceHost   *db.HostsRecord
	TargetHost   *db.HostsRecord
	TargetImage  string
	TargetPlanID *int32
	OperatorID   int32
	OperatorRole string
}

// hostsUserQuotaWarning mirrors the quota_warning notification payload.
type hostsUserQuota struct {
	HostLimit int32
	HostUsed  int32
}

// ==================== process-local log stream tickets ====================

type hostsTicketEntry struct {
	userID    int32
	hostID    int32
	expiresAt time.Time
}

var hostsTicketStore = struct {
	sync.Mutex
	entries map[string]hostsTicketEntry
}{entries: map[string]hostsTicketEntry{}}

func hostsRandomToken(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), bytes)
	}
	return hex.EncodeToString(buf)
}

func init() {
	// The default ticket implementation is the real one: process local, 60s TTL,
	// single use, bound to the host it was issued for. It matches the Node ticket
	// semantics this route relies on (POST issues, GET consumes exactly once).
	HostsAgentLogTicket.Issue = func(userID, hostID int32, issuedAt int64, sid string) string {
		ticket := "hs_" + hostsRandomToken(24)
		hostsTicketStore.Lock()
		defer hostsTicketStore.Unlock()
		now := time.Now().UTC()
		for key, entry := range hostsTicketStore.entries {
			if entry.expiresAt.Before(now) {
				delete(hostsTicketStore.entries, key)
			}
		}
		hostsTicketStore.entries[ticket] = hostsTicketEntry{
			userID:    userID,
			hostID:    hostID,
			expiresAt: now.Add(60 * time.Second),
		}
		return ticket
	}
	HostsAgentLogTicket.Consume = func(ticket string, hostID int32) bool {
		hostsTicketStore.Lock()
		defer hostsTicketStore.Unlock()
		entry, ok := hostsTicketStore.entries[ticket]
		if !ok {
			return false
		}
		delete(hostsTicketStore.entries, ticket)
		return entry.hostID == hostID && entry.expiresAt.After(time.Now().UTC())
	}
}

// ==================== parsing helpers ====================

// hostsParsePositiveRouteID mirrors parsePositiveRouteId: strict `^[1-9]\d*$` and
// a Number.isSafeInteger bound. Ids above int32 cannot be bound to the Int4
// primary key, so they are rejected here (the TypeScript lets Prisma throw a 500).
func hostsParsePositiveRouteID(value string) (int32, bool) {
	if !hostsPositiveRouteIDPattern.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > hostsMaxSafeInteger || parsed > hostsMaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// hostsParseIntegerBody mirrors parseIntegerBody(value, min, max).
func hostsParseIntegerBody(value any, min, max float64) (int64, bool) {
	number, ok := hostsJSONNumber(value)
	if !ok || number != math.Trunc(number) || number < min || number > max {
		return 0, false
	}
	return int64(number), true
}

// hostsParseOptionalIntegerBody mirrors parseOptionalIntegerBody.
func hostsParseOptionalIntegerBody(value any, min, max float64) (*int64, bool) {
	if value == nil {
		return nil, true
	}
	parsed, ok := hostsParseIntegerBody(value, min, max)
	if !ok {
		return nil, false
	}
	return &parsed, true
}

// hostsParseFiniteNumberBody mirrors parseFiniteNumberBody.
func hostsParseFiniteNumberBody(value any, min, max float64) (float64, bool) {
	number, ok := hostsJSONNumber(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < min || number > max {
		return 0, false
	}
	return number, true
}

// hostsJSONNumber mirrors `typeof value === 'number'`.
func hostsJSONNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	}
	return 0, false
}

// hostsParsePositiveIntegerArrayBody mirrors parsePositiveIntegerArrayBody.
func hostsParsePositiveIntegerArrayBody(value any, maxItems int) ([]int32, bool) {
	items, ok := value.([]any)
	if !ok || len(items) < 1 || len(items) > maxItems {
		return nil, false
	}
	parsed := make([]int32, 0, len(items))
	seen := map[int32]bool{}
	for _, item := range items {
		id, ok := hostsParseIntegerBody(item, 1, float64(hostsMaxSafeInteger))
		if !ok || id > hostsMaxInt32 || seen[int32(id)] {
			return nil, false
		}
		parsed = append(parsed, int32(id))
		seen[int32(id)] = true
	}
	return parsed, true
}

// hostsParseOptionalPositiveQueryInteger mirrors parseOptionalPositiveQueryInteger.
// The bool is false when the value was present but invalid (the TS `null`).
func hostsParseOptionalPositiveQueryInteger(value string) (*int32, bool) {
	if value == "" {
		return nil, true
	}
	parsed, ok := hostsParsePositiveRouteID(value)
	if !ok {
		return nil, false
	}
	return &parsed, true
}

// hostsParseOptionalQueryInt mirrors the same helper for query strings parsed with
// strconv (used by the caddy sites pagination, which uses parseInt instead).
func hostsParseOptionalQueryInt(value *string) *int32 {
	if value == nil || *value == "" {
		return nil
	}
	parsed, ok := hostsParsePositiveRouteID(*value)
	if !ok {
		return nil
	}
	return &parsed
}

// hostsParseHostListPageSize mirrors parseHostListPageSize.
func hostsParseHostListPageSize(value string) (*int32, bool) {
	parsed, ok := hostsParseOptionalPositiveQueryInteger(value)
	if !ok {
		return nil, false
	}
	if parsed == nil {
		return nil, true
	}
	size := *parsed
	if size > hostsListMaxPageSize {
		size = hostsListMaxPageSize
	}
	return &size, true
}

// hostsParseAuditHistoryPageSize mirrors parseHostAuditHistoryPageSize.
func hostsParseAuditHistoryPageSize(value string) (*int32, bool) {
	parsed, ok := hostsParseOptionalPositiveQueryInteger(value)
	if !ok {
		return nil, false
	}
	if parsed == nil {
		return nil, true
	}
	size := *parsed
	if size > hostsAuditHistoryMaxPageSize {
		size = hostsAuditHistoryMaxPageSize
	}
	return &size, true
}

// hostsParsePublicIpv4List mirrors parsePublicIpv4List.
func hostsParsePublicIpv4List(value any) []string {
	out := []string{}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			text := strings.TrimSpace(fmt.Sprint(item))
			if text != "" {
				out = append(out, text)
			}
		}
	case string:
		for _, item := range regexp.MustCompile(`[\n,\s]+`).Split(typed, -1) {
			text := strings.TrimSpace(item)
			if text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// ==================== response helpers ====================

func hostsRespondAPIError(c *gin.Context, status int, code lib.ErrorCode, details string) {
	lib.RespondAPIError(c, status, code, details)
}

// hostsRespondSeam reports a not-yet-ported dependency. 501 + SEAM_UNAVAILABLE is
// deliberately distinguishable from any Node behaviour.
func hostsRespondSeam(c *gin.Context, err error) {
	log.Printf("[hosts] seam unavailable: %v", err)
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err.Error(),
		"code":  "SEAM_UNAVAILABLE",
	})
}

// hostsRespondInternal mirrors the module's generic 500 (`throw` reaching
// app.ts's handler → INTERNAL_ERROR).
func hostsRespondInternal(c *gin.Context) {
	hostsRespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, "")
}

// hostsOwnerOrAdmin mirrors `host.user_id !== user.id && user.role !== 'admin'`.
func hostsOwnerOrAdmin(host *db.HostsRecord, user *lib.AccessTokenClaims) bool {
	return host.UserID == user.ID || user.Role == "admin"
}

// hostsOwnerOnly mirrors `host.user_id !== user.id`.
func hostsOwnerOnly(host *db.HostsRecord, user *lib.AccessTokenClaims) bool {
	return host.UserID == user.ID
}

// hostsLoadHost loads a host and writes the standard 404 when missing.
func hostsLoadHost(c *gin.Context, hostID int32) (*db.HostsRecord, bool) {
	host, err := db.HostsGetByID(c.Request.Context(), hostID)
	if err != nil {
		hostsRespondInternal(c)
		return nil, false
	}
	if host == nil {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return nil, false
	}
	return host, true
}

// hostsRouteID parses `:hostId` and writes 400 INVALID_ID on failure.
func hostsRouteID(c *gin.Context) (int32, bool) {
	hostID, ok := hostsParsePositiveRouteID(c.Param("hostId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return 0, false
	}
	return hostID, true
}

// hostsDecodedBody decodes the request body like Fastify's parser hook.
func hostsDecodedBody(c *gin.Context) (any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	return decoded.Value, true
}

// hostsBodyObject applies AJV's root `type: 'object'`. A bodyless request is
// `undefined` in Fastify, which the validator receives as null → "must be object".
func hostsBodyObject(decoded any) (map[string]any, string) {
	if decoded == nil {
		return nil, "must be object"
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	return object, ""
}

// hostsValidatedBody runs the shared AJV replica over one body object.
func hostsValidatedBody(c *gin.Context, required []string, fields []ajvFieldSpec) (ajvValidated, map[string]any, bool) {
	decoded, ok := hostsDecodedBody(c)
	if !ok {
		return ajvValidated{}, nil, false
	}
	object, message := hostsBodyObject(decoded)
	if message != "" {
		respondAJVValidation(c, message)
		return ajvValidated{}, nil, false
	}
	validated, message := validateAJVObject(object, required, fields)
	if message != "" {
		respondAJVValidation(c, message)
		return ajvValidated{}, nil, false
	}
	return validated, object, true
}

// hostsValidateArrayOfStrings mirrors an AJV `array of string` property.
func hostsValidateArrayOfStrings(c *gin.Context, object map[string]any, name string, required bool) ([]string, bool, bool) {
	value, present := object[name]
	if !present {
		if required {
			respondAJVValidation(c, name+" is required")
			return nil, false, false
		}
		return nil, false, true
	}
	items, ok := value.([]any)
	if !ok {
		respondAJVValidation(c, "must be array")
		return nil, false, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			respondAJVValidation(c, "must be string")
			return nil, false, false
		}
		out = append(out, text)
	}
	return out, true, true
}

// hostsValidateIntegerArray mirrors an AJV `array of integer` property.
func hostsValidateIntegerArray(c *gin.Context, object map[string]any, name string, required bool, minItems, maxItems int) ([]int32, bool, bool) {
	value, present := object[name]
	if !present {
		if required {
			respondAJVValidation(c, name+" is required")
			return nil, false, false
		}
		return nil, false, true
	}
	items, ok := value.([]any)
	if !ok {
		respondAJVValidation(c, "must be array")
		return nil, false, false
	}
	if minItems >= 0 && len(items) < minItems {
		respondAJVValidation(c, "must NOT have fewer than "+strconv.Itoa(minItems)+" items")
		return nil, false, false
	}
	if maxItems >= 0 && len(items) > maxItems {
		respondAJVValidation(c, "must NOT have more than "+strconv.Itoa(maxItems)+" items")
		return nil, false, false
	}
	out := make([]int32, 0, len(items))
	for _, item := range items {
		number, ok := hostsJSONNumber(item)
		if !ok || number != math.Trunc(number) {
			respondAJVValidation(c, "must be integer")
			return nil, false, false
		}
		out = append(out, int32(number))
	}
	return out, true, true
}

// ==================== small shared helpers ====================

// hostsDeriveEffectiveStatus mirrors deriveEffectiveHostStatus(host).
func hostsDeriveEffectiveStatus(host *db.HostsRecord) string {
	tunnelEnabled := host.TunnelEnabled
	return lib.TunnelDeriveEffectiveHostStatus(lib.TunnelEffectiveHostStatusInput{
		ID:            host.ID,
		Status:        host.Status,
		TunnelEnabled: &tunnelEnabled,
	})
}

// hostsDeriveEffectiveStatusSummary mirrors deriveEffectiveHostStatus for the
// pagination projection (snake_case record).
func hostsDeriveEffectiveStatusSummary(host models.Host) string {
	tunnelEnabled := host.TunnelEnabled
	return lib.TunnelDeriveEffectiveHostStatus(lib.TunnelEffectiveHostStatusInput{
		ID:            host.ID,
		Status:        string(host.Status),
		TunnelEnabled: &tunnelEnabled,
	})
}

// hostsShellDoubleQuote mirrors shellDoubleQuote.
func hostsShellDoubleQuote(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		`"`, `\"`,
		"$", "\\$",
		"`", "\\`",
	)
	return replacer.Replace(value)
}

// hostsInjectInstallVariable mirrors injectInstallVariable: it replaces the
// line-anchored `NAME=""` assignment with NAME="<escaped value>".
func hostsInjectInstallVariable(script, name, value string) string {
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=""`)
	return pattern.ReplaceAllLiteralString(script, name+`="`+hostsShellDoubleQuote(value)+`"`)
}

// hostsDerivePanelUrl mirrors derivePanelUrl.
func hostsDerivePanelUrl(c *gin.Context) string {
	configured := strings.TrimSpace(os.Getenv("SITE_URL"))
	if configured == "" {
		if frontend := os.Getenv("FRONTEND_URL"); frontend != "" {
			configured = strings.TrimSpace(strings.Split(frontend, ",")[0])
		}
	}
	if configured != "" {
		envURL := regexp.MustCompile(`^https?://(https?://)`).ReplaceAllString(configured, "$1")
		envURL = regexp.MustCompile(`/+$`).ReplaceAllString(envURL, "")
		if parsed, err := url.Parse(envURL); err == nil &&
			(parsed.Scheme == "http" || parsed.Scheme == "https") &&
			envURL != "https://incudal.com" {
			return parsed.Scheme + "://" + parsed.Host
		}
		if os.Getenv("NODE_ENV") == "production" {
			log.Printf("[hosts] Invalid SITE_URL/FRONTEND_URL for host install URL derivation: %s", envURL)
		}
	}

	referer := c.GetHeader("Referer")
	if referer == "" {
		referer = c.GetHeader("Origin")
	}
	if referer != "" {
		if parsed, err := url.Parse(referer); err == nil && parsed.Host != "" {
			return parsed.Scheme + "://" + parsed.Host
		}
	}

	if host := c.Request.Host; host != "" {
		proto := c.GetHeader("X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
		}
		proto = strings.TrimSuffix(proto, "://")
		return proto + "://" + host
	}

	return "https://incudal.com"
}

// hostsPanelCertificatePaths mirrors getPanelCertificatePaths().
func hostsPanelCertificatePaths() (string, string) {
	appDir := os.Getenv("INCUDAL_APP_DIR")
	if appDir == "" {
		appDir, _ = os.Getwd()
	}
	installDir := os.Getenv("INCUDAL_INSTALL_DIR")
	if installDir == "" {
		installDir = os.Getenv("INSTALL_DIR")
	}
	if installDir == "" && strings.HasSuffix(appDir, "/current") {
		installDir = filepath.Dir(appDir)
	}
	stableCert := ""
	stableKey := ""
	if installDir != "" {
		stableCert = filepath.Join(installDir, "server/certs/client.crt")
		stableKey = filepath.Join(installDir, "server/certs/client.key")
	}
	certPath := os.Getenv("PANEL_CRT_PATH")
	if certPath == "" {
		if stableCert != "" && hostsFileExists(stableCert) {
			certPath = stableCert
		} else if found, ok := hostsFindRepoPath("server/certs/client.crt"); ok {
			certPath = found
		} else {
			certPath = filepath.Join(hostsModuleDir(), "../../certs/client.crt")
		}
	}
	keyPath := os.Getenv("PANEL_KEY_PATH")
	if keyPath == "" {
		if stableKey != "" && hostsFileExists(stableKey) {
			keyPath = stableKey
		} else if found, ok := hostsFindRepoPath("server/certs/client.key"); ok {
			keyPath = found
		} else {
			keyPath = filepath.Join(hostsModuleDir(), "../../certs/client.key")
		}
	}
	return certPath, keyPath
}

// hostsModuleDir approximates `dirname(fileURLToPath(import.meta.url))` — the
// routes directory — starting from the executable and walking up.
func hostsModuleDir() string {
	if wd, err := os.Getwd(); err == nil {
		for _, candidate := range []string{
			filepath.Join(wd, "server/src/routes"),
			filepath.Join(wd, "../server/src/routes"),
			filepath.Join(wd, "src/routes"),
		} {
			if hostsFileExists(candidate) {
				return candidate
			}
		}
	}
	exe, err := os.Executable()
	if err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func hostsFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// hostsFindRepoPath walks up from the working directory and from the executable
// looking for `rel`. The TypeScript resolves both the install script and the panel
// certificate relative to its own source file (__dirname), which is fixed; the Go
// binary must not depend on the caller's working directory.
func hostsFindRepoPath(rel string) (string, bool) {
	starts := []string{}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		dir := start
		for depth := 0; depth < 8; depth++ {
			candidate := filepath.Join(dir, rel)
			if hostsFileExists(candidate) {
				return candidate, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", false
}

// hostsReadInstallScript reads the script template; ok=false means the Node 500
// branch (`# Error: Install script file not found`).
func hostsReadInstallScript() (string, bool) {
	candidates := []string{}
	if explicit := os.Getenv("INCUDAL_INSTALL_SCRIPT"); explicit != "" {
		candidates = append(candidates, explicit)
	}
	if appDir := os.Getenv("INCUDAL_APP_DIR"); appDir != "" {
		candidates = append(candidates, filepath.Join(appDir, "server/templates/install.sh"))
	}
	if found, ok := hostsFindRepoPath("server/templates/install.sh"); ok {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			return string(data), true
		}
	}
	return "", false
}

// hostsBuildNodeInstallCommand mirrors buildNodeInstallCommand.
func hostsBuildNodeInstallCommand(panelURL, installToken string) string {
	shellEscape := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	stripped := regexp.MustCompile(`/+$`).ReplaceAllString(panelURL, "")
	return fmt.Sprintf(
		"curl -fsSL %s -o incus-node-install.sh && sudo env INCUDAL_PANEL_URL=%s INCUDAL_AGENT_INSTALL_TOKEN=%s INCUS_NODE_CHANNEL=%s bash incus-node-install.sh",
		shellEscape(stripped+"/api/agent/install.sh"),
		shellEscape(stripped),
		shellEscape(installToken),
		shellEscape("canary"),
	)
}

// hostsCreateLog mirrors createLog(...) for this module.
func hostsCreateLog(ctx context.Context, userID *int64, action, content, result string, instanceID *int32) {
	options := db.CreateLogOptions{}
	if instanceID != nil {
		options.InstanceID = instanceID
		options.InstanceIDProvided = true
	}
	db.CreateLog(ctx, userID, hostsLogModule, action, content, result, options)
}

// hostsFormatStoragePoolCreateError mirrors formatStoragePoolCreateError.
func hostsFormatStoragePoolCreateError(err error) string {
	message := err.Error()
	lowered := strings.ToLower(message)
	if strings.Contains(lowered, "modprobe") && strings.Contains(lowered, "zfs") &&
		(strings.Contains(lowered, "module zfs not found") ||
			strings.Contains(lowered, `error loading "zfs" module`) ||
			strings.Contains(lowered, "error loading 'zfs' module")) {
		return strings.Join([]string{
			"宿主机未加载 ZFS 内核模块，当前系统无法创建 ZFS 存储池。",
			"Debian cloud 内核通常需要先安装匹配的 linux-headers 并让 zfs-dkms 编译成功；",
			"也可以改用 LVM、Btrfs 或 DIR 存储池。原始错误:",
			message,
		}, " ")
	}
	if strings.Contains(lowered, "required tool") && strings.Contains(lowered, "lvm") && strings.Contains(lowered, "missing") {
		return strings.Join([]string{
			"宿主机缺少 LVM 管理工具，当前系统无法创建 LVM 存储池。",
			"请先在宿主机执行 apt-get update && apt-get install -y lvm2，并确认 which lvm / lvm version 正常后再重试。原始错误:",
			message,
		}, " ")
	}
	if strings.Contains(lowered, "dm-thin-pool") ||
		(strings.Contains(lowered, "thin-pool") && strings.Contains(lowered, "device-mapper")) ||
		strings.Contains(lowered, "module dm-thin-pool not found") {
		return strings.Join([]string{
			"宿主机内核缺少 dm-thin-pool 模块，当前系统无法创建 LVM Thinpool（精简卷）。",
			"请在创建 LVM 存储池时取消勾选“启用 Thinpool（精简配置）”，或改用 DIR 存储池。原始错误:",
			message,
		}, " ")
	}
	if strings.Contains(lowered, "not authorized") {
		return strings.Join([]string{
			"Incus 拒绝了面板客户端证书，当前节点的 Incus trust 中没有信任面板证书或仍在使用旧证书。",
			"请在后台重新生成该节点的安装/重装命令并在真实 Incus 宿主机执行，确认导入证书后再创建存储池。原始错误:",
			message,
		}, " ")
	}
	return message
}

// ==================== host service status (port of lib/host-service-status.ts) ====================

// hostsHostServiceStatus mirrors HostServiceStatus.
type hostsHostServiceStatus struct {
	Status           string   `json:"status"`
	PendingInstances []string `json:"pendingInstances"`
	Available        bool     `json:"available"`
}

type hostsServiceStatusRow struct {
	ID            int32
	Status        string
	TunnelEnabled bool
	Report        []byte
	AgentStatus   *string
	AgentLastSeen *time.Time
	AgentReport   []byte
}

func hostsLoadHostServiceStatuses(ctx context.Context, ids []int32) (map[int32]hostsHostServiceStatus, error) {
	out := map[int32]hostsHostServiceStatus{}
	if ids != nil && len(ids) == 0 {
		return out, nil
	}
	if db.DB == nil {
		return out, errors.New("hosts: database not initialized")
	}
	query := db.DB.WithContext(ctx).
		Table("hosts AS h").
		Select(`h.id, h.status, h.tunnel_enabled,
			ns.report AS report,
			a.status AS agent_status, a.last_seen_at AS agent_last_seen, a.last_report AS agent_report`).
		Joins("LEFT JOIN node_security_states ns ON ns.host_id = h.id").
		Joins("LEFT JOIN host_agents a ON a.host_id = h.id AND a.enabled = true")
	if ids != nil {
		query = query.Where("h.id IN ?", ids)
	}
	var rows []hostsServiceStatusRow
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, row := range rows {
		out[row.ID] = hostsEvaluateHostService(row, now)
	}
	return out, nil
}

// hostsEvaluateHostService mirrors evaluateHostService.
func hostsEvaluateHostService(row hostsServiceStatusRow, now time.Time) hostsHostServiceStatus {
	state := map[string]any{}
	if len(row.Report) > 0 {
		_ = json.Unmarshal(row.Report, &state)
	}
	report := map[string]any{}
	if len(row.AgentReport) > 0 {
		_ = json.Unmarshal(row.AgentReport, &report)
	}

	status := hostsDeriveEffectiveStatusSummary(models.Host{
		ID:            row.ID,
		Status:        models.HostStatus(row.Status),
		TunnelEnabled: row.TunnelEnabled,
	})
	if locked, ok := state["locked"].(bool); ok && locked {
		status = "locked"
	}

	interval := float64(5)
	if metrics, ok := report["metrics"].(map[string]any); ok {
		if reported, ok := hostsJSONNumber(metrics["heartbeatIntervalSeconds"]); ok && reported >= 5 && reported <= 3600 {
			interval = reported
		}
	}
	stale := false
	if row.AgentStatus != nil {
		threshold := math.Max(120, interval*3)
		stale = *row.AgentStatus != "online" || row.AgentLastSeen == nil ||
			now.Sub(row.AgentLastSeen.UTC()).Seconds() > threshold
	}

	pending := []string{}
	if items, ok := state["resumeInstances"].([]any); ok {
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if project, _ := entry["project"].(string); project == "default" {
				if name, ok := entry["name"].(string); ok {
					pending = append(pending, name)
				}
			}
		}
	}

	recoveryPending, _ := state["recoveryPending"].(bool)
	agentPresent := row.AgentStatus != nil
	incusAvailable := false
	if incus, ok := report["incus"].(map[string]any); ok {
		incusAvailable, _ = incus["available"].(bool)
	}
	instancesUnavailable := false
	if instances, ok := report["instances"].(map[string]any); ok {
		if available, ok := instances["available"].(bool); ok && !available {
			instancesUnavailable = true
		}
	}

	available := !recoveryPending &&
		(status == "online" || status == "maintenance") &&
		(!row.TunnelEnabled || lib.TunnelManagerInstance().IsTunnelOnline(row.ID)) &&
		!stale &&
		(!agentPresent || incusAvailable) &&
		!instancesUnavailable &&
		lib.IsHostHealthy(row.ID)

	return hostsHostServiceStatus{Status: status, PendingInstances: pending, Available: available}
}

// ==================== instance helpers (route-local) ====================

// hostsInstanceCount mirrors db.getInstanceCountByHost.
func hostsInstanceCount(ctx context.Context, hostID int32) (int64, error) {
	return db.HostsInstanceCount(ctx, hostID)
}

// hostsInstancesOnHost mirrors db.getInstancesByHost.
func hostsInstancesOnHost(ctx context.Context, hostID int32) ([]models.Instance, error) {
	return db.HostsInstances(ctx, hostID)
}

// hostsInstanceIDsOnHost applies the `id IN (...) AND hostId = ? AND status ...`
// filters the TypeScript expresses with prisma.instance.findMany.
func hostsInstancesMatching(ctx context.Context, hostID int32, ids []int32, excludedStatuses []string, allowedStatuses []string) ([]models.Instance, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	query := db.DB.WithContext(ctx).Where("host_id = ?", hostID)
	if ids != nil {
		query = query.Where("id IN ?", ids)
	}
	if len(excludedStatuses) > 0 {
		query = query.Where("status NOT IN ?", excludedStatuses)
	}
	if len(allowedStatuses) > 0 {
		query = query.Where("status IN ?", allowedStatuses)
	}
	var instances []models.Instance
	if err := query.Find(&instances).Error; err != nil {
		return nil, err
	}
	return instances, nil
}

// hostsActiveWorkflowTask reports whether an instance has an active restore,
// upload or instance task (mirrors the three findFirst probes).
func hostsActiveWorkflowTask(ctx context.Context, instanceID int32) (kind string, taskID *int32, status *string, err error) {
	if db.DB == nil {
		return "", nil, nil, errors.New("hosts: database not initialized")
	}
	var restore struct {
		ID     int32
		Status string
	}
	result := db.DB.WithContext(ctx).Table("restore_tasks").
		Select("id, status").Where("instance_id = ? AND status IN ?", instanceID, []string{"PENDING", "PROCESSING"}).
		Limit(1).Scan(&restore)
	if result.Error != nil {
		return "", nil, nil, result.Error
	}
	if result.RowsAffected > 0 {
		return "restore", &restore.ID, &restore.Status, nil
	}
	var upload struct {
		ID     int32
		Status string
	}
	result = db.DB.WithContext(ctx).Table("backup_upload_tasks").
		Select("id, status").Where("instance_id = ? AND status IN ?", instanceID, []string{"PENDING", "PROCESSING"}).
		Limit(1).Scan(&upload)
	if result.Error != nil {
		return "", nil, nil, result.Error
	}
	if result.RowsAffected > 0 {
		return "upload", &upload.ID, &upload.Status, nil
	}
	active, err := db.GetActiveTaskForInstance(ctx, instanceID)
	if err != nil {
		return "", nil, nil, err
	}
	if active != nil {
		status := active.Status
		return "task", &active.ID, (*string)(&status), nil
	}
	return "", nil, nil, nil
}

// hostsSendTaskConflict mirrors sendHostActiveTaskConflict.
func hostsSendTaskConflict(c *gin.Context, taskID *int32, taskType, status *string) {
	payload := gin.H{"error": "Instance has an active task", "code": "TASK_IN_PROGRESS"}
	if taskID != nil {
		payload["taskId"] = *taskID
		payload["taskType"] = *taskType
		payload["status"] = *status
	}
	c.JSON(http.StatusConflict, payload)
}

// hostsRejectActiveWorkflowConflict mirrors rejectActiveHostInstanceWorkflowConflict.
// It returns true when a 409 has already been written.
func hostsRejectActiveWorkflowConflict(c *gin.Context, instanceID int32) bool {
	kind, taskID, status, err := hostsActiveWorkflowTask(c.Request.Context(), instanceID)
	if err != nil {
		hostsRespondInternal(c)
		return true
	}
	switch kind {
	case "restore":
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Instance has an active restore task",
			"code":   "RESTORE_IN_PROGRESS",
			"taskId": *taskID,
			"status": *status,
		})
		return true
	case "upload":
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Instance has an active backup upload task",
			"code":   "UPLOAD_IN_PROGRESS",
			"taskId": *taskID,
			"status": *status,
		})
		return true
	case "task":
		hostsSendTaskConflict(c, taskID, strPtrHosts(""), status)
		return true
	}
	return false
}

func strPtrHosts(value string) *string { return &value }

// hostsGetInstanceByID mirrors db.getInstanceById (the snake_case projection the
// host routes use: `instance.host_id`, `instance.incus_id`, `instance.user_id`).
func hostsGetInstanceByID(ctx context.Context, instanceID int32) (*models.Instance, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var instance models.Instance
	result := db.DB.WithContext(ctx).Where("id = ?", instanceID).Limit(1).Find(&instance)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &instance, nil
}

// hostsUserLookup mirrors prisma.user.findUnique({ where: { username } }).
type hostsUserLookup struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
	Email    *string
	Status   string
}

func hostsFindUserByUsername(ctx context.Context, username string) (*hostsUserLookup, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var row hostsUserLookup
	result := db.DB.WithContext(ctx).Table("users").
		Select("id, username, email, status").Where("username = ?", username).Limit(1).Scan(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &row, nil
}

// hostsIncusClientOptions builds the pooled-client options from a host record.
func hostsIncusClientOptions(host *db.HostsRecord) lib.IncusClientOptions {
	return lib.IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	}
}

// hostsStopInstance / hostsDeleteInstance / hostsListInstances / hostsGetInstanceState
// mirror the lib/incus helpers the TypeScript calls through dynamic import.
func hostsGetInstanceState(ctx context.Context, client *lib.IncusClient, name string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodGet, "/1.0/instances/"+name+"/state", nil, 0, nil)
}

func hostsListInstances(ctx context.Context, client *lib.IncusClient) ([]map[string]any, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	var urls []string
	if err := json.Unmarshal(raw, &urls); err != nil {
		return nil, err
	}
	instances := make([]map[string]any, 0, len(urls))
	for _, item := range urls {
		parts := strings.Split(strings.TrimSuffix(item, "/"), "/")
		name := parts[len(parts)-1]
		detail, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+name, nil, 0, nil)
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(detail, &decoded); err == nil {
			instances = append(instances, decoded)
		}
	}
	return instances, nil
}

// hostsMapInstanceStatus mirrors mapInstanceStatus.
func hostsMapInstanceStatus(incusStatus string) string {
	switch incusStatus {
	case "Running":
		return "running"
	case "Stopped", "Frozen":
		return "stopped"
	case "Error":
		return "error"
	case "Starting":
		return "creating"
	case "Stopping":
		return "running"
	}
	return "unknown"
}

// ==================== A. 列表 / 创建 / 安装脚本 / 证书 ====================

// hostsListHandler mirrors GET /api/hosts.
func hostsListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	page, ok := hostsParseOptionalPositiveQueryInteger(c.Query("page"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid host list query")
		return
	}
	pageSize, ok := hostsParseHostListPageSize(c.Query("pageSize"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid host list query")
		return
	}
	filterUserID, ok := hostsParseOptionalPositiveQueryInteger(c.Query("userId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid host list query")
		return
	}

	isAdmin := user.Role == "admin"
	if !isAdmin && c.Query("mine") != "true" {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	var queryUserID *int32
	var ownerRole *string
	scope := c.Query("scope")
	switch {
	case isAdmin && scope == "official":
		role := "admin"
		ownerRole = &role
	case isAdmin && scope == "mine":
		id := user.ID
		queryUserID = &id
	case !isAdmin:
		id := user.ID
		queryUserID = &id
	}
	_ = filterUserID // parsed for the 400 branch only; the TypeScript never applies it

	effectivePage := int32(1)
	if page != nil {
		effectivePage = *page
	}
	effectivePageSize := int32(20)
	if pageSize != nil {
		effectivePageSize = *pageSize
	}

	items, total, err := hostsQueryPaginated(ctx, c.Query("search"), queryUserID, ownerRole, effectivePage, effectivePageSize)
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	ids := make([]int32, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	statuses, err := hostsLoadHostServiceStatuses(ctx, ids)
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	hosts := make([]gin.H, 0, len(items))
	for _, item := range items {
		instanceCount, err := hostsInstanceCount(ctx, item.ID)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		overview, err := lib.GetStorageOverview(ctx, item.ID)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		effective := hostsDeriveEffectiveStatusSummary(item)
		status := effective
		if service, ok := statuses[item.ID]; ok && service.Status != "" {
			status = service.Status
		}
		tags := []string{}
		if len(item.Tags) > 0 {
			_ = json.Unmarshal(item.Tags, &tags)
		}
		country := item.CountryCode
		if country == "" {
			country = "us"
		}
		instanceType := string(item.InstanceType)
		if instanceType == "" {
			instanceType = "container"
		}
		// The pagination projection does not select nat_public_ipv6 / nat_bind_ip,
		// so `publicIpv6` is `undefined` in the TypeScript object literal and is
		// dropped by JSON.stringify; bindIp / bindIpv6 fall back to null.
		natConfig := gin.H{
			"publicIp":       item.NATPublicIP,
			"bindIp":         nil,
			"bindIpv6":       nil,
			"portRangeStart": item.NATPortStart,
			"portRangeEnd":   item.NATPortEnd,
			"portsUsedCount": item.NATPortsUsedCount,
		}
		hosts = append(hosts, gin.H{
			"id":           item.ID,
			"name":         item.Name,
			"url":          item.URL,
			"status":       status,
			"maintenance":  effective == "maintenance",
			"location":     item.Location,
			"countryCode":  country,
			"architecture": "x86_64",
			"tags":         tags,
			"resources": gin.H{
				"cpuUsed":    item.CPUUsed,
				"memoryUsed": item.MemoryUsed,
				"diskTotal":  overview.TotalMB,
				"diskUsed":   overview.AllocatedMB,
			},
			"cpuAllowanceMax": item.CPUAllowanceMax,
			"memoryMax":       item.MemoryMax,
			"instanceType":    instanceType,
			"instanceCount":   instanceCount,
			"natConfig":       natConfig,
			"tunnelEnabled":   item.TunnelEnabled,
			"targetHost":      item.TargetHost,
			"targetPort":      item.TargetPort,
			"tunnelOnline":    lib.TunnelManagerInstance().IsTunnelOnline(item.ID),
		})
	}

	totalPages := int32(0)
	if effectivePageSize > 0 {
		totalPages = int32(math.Ceil(float64(total) / float64(effectivePageSize)))
	}
	c.JSON(http.StatusOK, gin.H{
		"hosts":      hosts,
		"total":      total,
		"page":       effectivePage,
		"pageSize":   effectivePageSize,
		"totalPages": totalPages,
	})
}

// hostsQueryPaginated mirrors db.getHostsPaginated (db/pagination.ts). The owner
// relation is only joined when includeOwner is true, and this route never sets it.
func hostsQueryPaginated(
	ctx context.Context,
	search string,
	userID *int32,
	ownerRole *string,
	page, pageSize int32,
) ([]models.Host, int64, error) {
	if db.DB == nil {
		return nil, 0, errors.New("hosts: database not initialized")
	}
	base := db.DB.WithContext(ctx).Model(&models.Host{})
	if search != "" {
		pattern := "%" + search + "%"
		base = base.Where(`(name ILIKE ? OR location ILIKE ? OR url ILIKE ?)`, pattern, pattern, pattern)
	}
	if userID != nil {
		base = base.Where("user_id = ?", *userID)
	}
	if ownerRole != nil {
		base = base.Where("user_id IN (SELECT id FROM users WHERE role = ?)", *ownerRole)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var hosts []models.Host
	if err := base.Order("name ASC").
		Offset(int((page - 1) * pageSize)).
		Limit(int(pageSize)).
		Find(&hosts).Error; err != nil {
		return nil, 0, err
	}
	return hosts, total, nil
}

// hostsCreateHandler mirrors POST /api/hosts.
func hostsCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	validated, object, ok := hostsValidatedBody(c, []string{"name", "url"}, []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: 2, MaxLength: -1},
		{Name: "url", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "location", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "countryCode", Kind: ajvKindString, MinLength: 2, MaxLength: 2},
		{Name: "certPath", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "keyPath", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "ipAddress", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "storageDriver", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"zfs", "lvm"}},
		{Name: "storageType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"loop", "disk"}},
		{Name: "storagePath", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "storageSize", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "ipv6Mode", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "ipv6Subnet", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "ipv6Gateway", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "ipv6ParentInterface", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "cpuAllowanceMax", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "memoryMax", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "instanceType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"container", "vm", "both"}},
		{Name: "tunnelEnabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "targetHost", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "targetPort", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	})
	if !ok {
		return
	}
	tags, _, tagsOK := hostsValidateArrayOfStrings(c, object, "tags", false)
	if !tagsOK {
		return
	}
	natConfig, natOK := hostsValidateNatConfig(c, object)
	if !natOK {
		return
	}

	name, _ := validated.stringValue("name")
	rawURL, _ := validated.stringValue("url")

	// Validate input (prevent dangerous character injection).
	nameValidation := lib.ValidateIdentifier(name, "Host name", 2, 64)
	if !nameValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": nameValidation.Message})
		return
	}

	if user.Role != "admin" {
		prefix := fmt.Sprintf("PEER%d", user.ID)
		pattern := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(prefix) + `[-_]`)
		if !pattern.MatchString(name) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("Node name must start with your user ID prefix: %s-", prefix),
				"code":  "INVALID_HOST_NAME_PREFIX",
			})
			return
		}
	}

	existing, err := db.HostsGetByUserAndName(ctx, user.ID, name)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if existing != nil {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostNameExists, "")
		return
	}

	// 托管功能已移除：仅管理员可以创建节点
	if user.Role != "admin" {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	urlValidation := lib.ValidateURL(rawURL, "Host URL")
	if !urlValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": urlValidation.Message})
		return
	}
	sanitizedURL := rawURL
	if urlValidation.Sanitized != nil {
		sanitizedURL = *urlValidation.Sanitized
	}
	parsedURL, parseErr := url.Parse(sanitizedURL)
	hostname := ""
	if parseErr == nil {
		hostname = parsedURL.Hostname()
	}
	hostAddressValidation := lib.ValidateIPOrDomain(hostname, "Host address")
	if !hostAddressValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": hostAddressValidation.Message})
		return
	}

	location, hasLocation := validated.stringValue("location")
	if hasLocation && location != "" {
		locationValidation := lib.ValidateName(location, "Location", 1, 100)
		if !locationValidation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": locationValidation.Message})
			return
		}
		if locationValidation.Sanitized != nil {
			location = *locationValidation.Sanitized
		}
	}

	natPublicIP, natPublicIPv6, natBindIP, natBindIPv6, natPorts, natOK := hostsNormalizeNatConfig(c, natConfig)
	if !natOK {
		return
	}

	ipv6Subnet := strings.TrimSpace(stringValueOr(validated, "ipv6Subnet"))
	ipv6ParentInterface := strings.TrimSpace(stringValueOr(validated, "ipv6ParentInterface"))
	var normalizedIPv6Subnet *string
	if ipv6Subnet != "" {
		normalizedIPv6Subnet = &ipv6Subnet
	}
	var normalizedIPv6Parent *string
	if ipv6ParentInterface != "" {
		normalizedIPv6Parent = &ipv6ParentInterface
	}

	// 判断是否需要初始化
	certPath, hasCert := validated.stringValue("certPath")
	keyPath, hasKey := validated.stringValue("keyPath")
	needsInitialization := !hasCert || !hasKey || certPath == "" || keyPath == ""

	cpuAllowanceMax, cpuPresent := hostsValidatedInt64(validated, "cpuAllowanceMax")
	if cpuPresent && cpuAllowanceMax != nil && *cpuAllowanceMax < 0 {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostInvalidCpuMax, "")
		return
	}
	memoryMax, memoryPresent := hostsValidatedInt64(validated, "memoryMax")
	if memoryPresent && memoryMax != nil {
		if *memoryMax < 0 {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostInvalidMemoryMax, "")
			return
		}
		if *memoryMax > 0 && *memoryMax < 256 {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostInvalidMemoryMax, "")
			return
		}
	}

	storageDriver := stringValueOr(validated, "storageDriver")
	storageType := stringValueOr(validated, "storageType")
	storagePath := stringValueOr(validated, "storagePath")
	storageSize, storageSizePresent := hostsValidatedInt64(validated, "storageSize")
	ipv6Mode, ipv6ModePresent := hostsValidatedInt64(validated, "ipv6Mode")
	instanceType := stringValueOr(validated, "instanceType")

	if needsInitialization {
		if storagePath != "" && !regexp.MustCompile(`^/dev/[a-zA-Z0-9_-]+$`).MatchString(storagePath) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid storage path, must be a valid device path like /dev/sdb or /dev/nvme0n1"})
			return
		}
		if storageSizePresent && storageSize != nil && (*storageSize < 10 || *storageSize > 10000) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Storage size must be between 10 and 10000 GB"})
			return
		}
		if ipv6ModePresent && ipv6Mode != nil && (*ipv6Mode < 1 || *ipv6Mode > 3) {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostInvalidIpv6Mode, "")
			return
		}
		if ipv6ModePresent && ipv6Mode != nil && *ipv6Mode == 1 {
			if (normalizedIPv6Subnet == nil) != (normalizedIPv6Parent == nil) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "IPv6 subnet and parent interface must be provided together"})
				return
			}
		}
	}

	tunnelEnabled := true
	if value, present := hostsValidatedBool(validated, "tunnelEnabled"); present {
		tunnelEnabled = value
	}
	targetHost := "127.0.0.1"
	if value, present := validated.stringValue("targetHost"); present {
		targetHost = value
	}
	targetPort := int64(8443)
	if value, present := hostsValidatedInt64(validated, "targetPort"); present && value != nil {
		targetPort = *value
	}

	// 隧道模式端点是逐宿主独立的回环地址，不做全局唯一性注册。
	if !tunnelEnabled {
		if err := hostsPrepareAddressSnapshot(ctx, sanitizedURL, nil); err != nil {
			var registryErr *hostsAddressRegistryError
			if errors.As(err, &registryErr) {
				hostsRespondAPIError(c, http.StatusBadRequest, registryErr.Code, registryErr.Details)
				return
			}
			hostsRespondInternal(c)
			return
		}
	}

	countryCode := "us"
	if value, present := validated.stringValue("countryCode"); present {
		countryCode = value
	}
	storageDriverValue := "zfs"
	if storageDriver != "" {
		storageDriverValue = storageDriver
	}
	storageTypeValue := "loop"
	if storageType != "" {
		storageTypeValue = storageType
	}
	sizeValue := int32(60)
	if storageSizePresent && storageSize != nil {
		sizeValue = int32(*storageSize)
	}
	ipv6ModeValue := int32(1)
	if ipv6ModePresent && ipv6Mode != nil {
		ipv6ModeValue = int32(*ipv6Mode)
	}

	createData := db.HostsCreateData{
		UserID:              user.ID,
		Name:                name,
		URL:                 sanitizedURL,
		CountryCode:         &countryCode,
		Tags:                tags,
		NATPublicIP:         natPublicIP,
		NATPublicIPv6:       natPublicIPv6,
		NATBindIP:           natBindIP,
		NATBindIPv6:         natBindIPv6,
		NATPortStart:        natPorts.start,
		NATPortEnd:          natPorts.end,
		StorageDriver:       &storageDriverValue,
		StorageType:         &storageTypeValue,
		StorageSize:         &sizeValue,
		IPv6Mode:            &ipv6ModeValue,
		IPv6Subnet:          normalizedIPv6Subnet,
		IPv6ParentInterface: normalizedIPv6Parent,
		CPUAllowanceMax:     hostsInt32Ptr(cpuAllowanceMax),
		MemoryMax:           hostsInt32Ptr(memoryMax),
		IsInstalled:         hostsBoolPtr(!needsInitialization),
		CertDownloadCount:   hostsInt32PtrValue(0),
		TunnelEnabled:       &tunnelEnabled,
		TargetHost:          &targetHost,
		TargetPort:          hostsInt32PtrValue(int64(targetPort)),
	}
	if hasLocation && location != "" {
		createData.Location = &location
	}
	if hasCert && certPath != "" {
		createData.CertPath = &certPath
	}
	if hasKey && keyPath != "" {
		createData.KeyPath = &keyPath
	}
	if ipAddress, present := validated.stringValue("ipAddress"); present && ipAddress != "" {
		createData.IPAddress = &ipAddress
	}
	if storagePath != "" {
		createData.StoragePath = &storagePath
	}
	if ipv6Gateway, present := validated.stringValue("ipv6Gateway"); present && ipv6Gateway != "" {
		createData.IPv6Gateway = &ipv6Gateway
	}
	if instanceType != "" {
		createData.InstanceType = &instanceType
	}

	hostID, err := db.HostsCreate(ctx, createData, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	// 如果不需要初始化（已安装 Incus），尝试自动连接并同步资源
	autoConnected := false
	var resourceInfo any
	detectedArchitecture := "x86_64"
	if !needsInitialization && hasCert && certPath != "" && hasKey && keyPath != "" {
		client := lib.NewIncusClient(hostsIncusClientOptions(&db.HostsRecord{
			ID:       hostID,
			URL:      sanitizedURL,
			CertPath: &certPath,
			KeyPath:  &keyPath,
		}))
		if info, err := client.Connect(ctx); err != nil {
			log.Printf("自动连接节点 %s 失败: %v", name, err)
		} else {
			architecture := hostsDetectArchitecture(info)
			if architecture == "" {
				architecture = "x86_64"
			}
			detectedArchitecture = architecture
			_ = db.HostsUpdateStatus(ctx, hostID, models.HostStatusOnline)
			if cpuAllowanceMax != nil || memoryMax != nil || instanceType != "" || detectedArchitecture != "x86_64" {
				_ = db.HostsUpdateResources(ctx, hostID, db.HostsResourceUpdate{
					CPUAllowanceMax: cpuAllowanceMax,
					MemoryMax:       memoryMax,
					InstanceType:    hostsNilIfEmpty(instanceType),
					Architecture:    &detectedArchitecture,
				}, nil)
			}
			_ = client.Close()
			autoConnected = true
			lib.TunnelManagerInstance().RequestHeartbeat(hostID)
		}
	} else if cpuAllowanceMax != nil || memoryMax != nil || instanceType != "" {
		_ = db.HostsUpdateResources(ctx, hostID, db.HostsResourceUpdate{
			CPUAllowanceMax: cpuAllowanceMax,
			MemoryMax:       memoryMax,
			InstanceType:    hostsNilIfEmpty(instanceType),
		}, nil)
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.create", fmt.Sprintf(
		`Created host "%s" [url: %s, location: %s, country: %s, storage: %s/%s %dGB, instanceType: %s]`,
		name, sanitizedURL, valueOrNA(location), countryCode,
		storageDriverValue, storageTypeValue, sizeValue, valueOr(instanceType, "container"),
	), "success", nil)

	// 检查宿主机配额使用率，超过 80% 时发送警告通知
	if quota, err := db.GetUserQuota(ctx, user.ID); err == nil && quota != nil && quota.HostLimit > 0 {
		usagePercent := float64(quota.HostUsed) / float64(quota.HostLimit) * 100
		if usagePercent >= 80 {
			lib.SendNotification(ctx, int64(user.ID), "quota_warning", lib.EventData{
				"instanceName": "",
				"quotaType":    "宿主机",
				"currentUsed":  quota.HostUsed,
				"maxLimit":     quota.HostLimit,
				"usagePercent": math.Round(usagePercent),
			})
		}
	}

	message := "Host added, please test connection manually"
	if needsInitialization {
		message = "Host added, please run install script to complete initialization"
	} else if autoConnected {
		message = "Host added and connected"
	}

	response := gin.H{
		"message": message,
		"host": gin.H{
			"id":           hostID,
			"name":         name,
			"url":          sanitizedURL,
			"architecture": detectedArchitecture,
			"status":       map[bool]string{true: "online", false: "offline"}[autoConnected],
			"location":     hostsNilIfEmpty(location),
			"resources":    resourceInfo,
		},
	}

	if needsInitialization {
		installation, err := HostsIssueAgentInstallToken(ctx, hostID, true)
		if err != nil {
			hostsRespondSeam(c, err)
			return
		}
		panelURL := hostsDerivePanelUrl(c)
		response["installCommand"] = hostsBuildNodeInstallCommand(panelURL, installation.InstallToken)
		response["installToken"] = installation.InstallToken
		response["installTokenExpiresAt"] = installation.InstallTokenExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}

	c.JSON(http.StatusCreated, response)
}

// hostsNatPorts carries the validated port range.
type hostsNatPorts struct {
	start *int32
	end   *int32
}

// hostsValidateNatConfig mirrors the AJV part of the nested natConfig object.
func hostsValidateNatConfig(c *gin.Context, object map[string]any) (map[string]any, bool) {
	value, present := object["natConfig"]
	if !present {
		return nil, true
	}
	nested, ok := value.(map[string]any)
	if !ok {
		respondAJVValidation(c, "must be object")
		return nil, false
	}
	for _, name := range []string{"publicIp", "publicIpv6", "bindIp", "bindIpv6"} {
		if raw, present := nested[name]; present {
			if _, ok := raw.(string); !ok {
				respondAJVValidation(c, "must be string")
				return nil, false
			}
		}
	}
	for _, name := range []string{"portRangeStart", "portRangeEnd"} {
		if raw, present := nested[name]; present {
			number, ok := hostsJSONNumber(raw)
			if !ok || number != math.Trunc(number) {
				respondAJVValidation(c, "must be integer")
				return nil, false
			}
			nested[name] = number
		}
	}
	return nested, true
}

// hostsNormalizeNatConfig mirrors the validation/assignment block of POST /api/hosts.
func hostsNormalizeNatConfig(c *gin.Context, natConfig map[string]any) (*string, *string, *string, *string, hostsNatPorts, bool) {
	ports := hostsNatPorts{}
	var publicIP, publicIPv6, bindIP, bindIPv6 *string

	if natConfig == nil {
		return publicIP, publicIPv6, bindIP, bindIPv6, ports, true
	}
	read := func(key string) string {
		if raw, ok := natConfig[key].(string); ok {
			return raw
		}
		return ""
	}

	if value := read("publicIp"); value != "" {
		validation := lib.ValidateIPAddress(value, "NAT public IP")
		if !validation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": validation.Message})
			return nil, nil, nil, nil, ports, false
		}
		normalized := strings.TrimSpace(value)
		if validation.Sanitized != nil {
			normalized = *validation.Sanitized
		}
		publicIP = &normalized
	}
	if value := read("bindIp"); value != "" {
		validation := lib.ValidateIPAddress(value, "NAT bind IP")
		if !validation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": validation.Message})
			return nil, nil, nil, nil, ports, false
		}
		normalized := strings.TrimSpace(value)
		if validation.Sanitized != nil {
			normalized = *validation.Sanitized
		}
		bindIP = &normalized
	}
	if value := read("bindIpv6"); value != "" {
		validation := lib.ValidateIPAddress(value, "NAT bind IPv6")
		normalized := strings.TrimSpace(value)
		if validation.Sanitized != nil {
			normalized = *validation.Sanitized
		}
		if !validation.Valid || !strings.Contains(normalized, ":") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid NAT bind IPv6 address format"})
			return nil, nil, nil, nil, ports, false
		}
		bindIPv6 = &normalized
	}
	if value := read("publicIpv6"); value != "" {
		validation := lib.ValidateIPAddress(value, "NAT public IPv6")
		normalized := strings.TrimSpace(value)
		if validation.Sanitized != nil {
			normalized = *validation.Sanitized
		}
		if !validation.Valid || !strings.Contains(normalized, ":") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid NAT public IPv6 address format"})
			return nil, nil, nil, nil, ports, false
		}
		publicIPv6 = &normalized
	}

	if start, ok := natConfig["portRangeStart"]; ok && start != nil {
		if number, ok := hostsJSONNumber(start); ok {
			value := int32(number)
			ports.start = &value
		}
	}
	if end, ok := natConfig["portRangeEnd"]; ok && end != nil {
		if number, ok := hostsJSONNumber(end); ok {
			value := int32(number)
			ports.end = &value
		}
	}
	if ports.start != nil && ports.end != nil && *ports.end < *ports.start {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Port range end must be greater than or equal to start", "code": "INVALID_PORT_RANGE"})
		return nil, nil, nil, nil, ports, false
	}
	return publicIP, publicIPv6, bindIP, bindIPv6, ports, true
}

// hostsDetectArchitecture mirrors normalizeArchitecture(serverInfo.environment...).
func hostsDetectArchitecture(info json.RawMessage) string {
	var decoded struct {
		Environment struct {
			KernelArchitecture string   `json:"kernel_architecture"`
			Architectures      []string `json:"architectures"`
		} `json:"environment"`
	}
	if err := json.Unmarshal(info, &decoded); err != nil {
		return ""
	}
	raw := decoded.Environment.KernelArchitecture
	if raw == "" && len(decoded.Environment.Architectures) > 0 {
		raw = decoded.Environment.Architectures[0]
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "x86_64", "amd64":
		return "x86_64"
	case "aarch64", "arm64":
		return "aarch64"
	}
	return "x86_64"
}

// hostsAddressRegistryError mirrors HostAddressRegistryError.
type hostsAddressRegistryError struct {
	Code    lib.ErrorCode
	Details string
}

func (e *hostsAddressRegistryError) Error() string { return string(e.Code) }

// hostsPrepareAddressSnapshot mirrors prepareHostAddressSnapshotForWrite: it
// resolves the host name and refuses an address that cannot be resolved. The alias
// bookkeeping of services/host-address-monitor.ts (host_address_aliases /
// host_address_conflicts) is NOT ported yet, so the uniqueness registry is inert;
// resolution failures still produce the exact 400 the TypeScript returns.
func hostsPrepareAddressSnapshot(ctx context.Context, rawURL string, excludeHostID *int32) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return &hostsAddressRegistryError{Code: lib.ErrInvalidParams, Details: "host address cannot be resolved"}
	}
	hostname := parsed.Hostname()
	if net.ParseIP(hostname) != nil {
		return nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(resolveCtx, hostname); err != nil {
		return &hostsAddressRegistryError{Code: lib.ErrHostAddressUnresolvable, Details: hostname}
	}
	return nil
}

// hostsInstallScriptHandler mirrors GET /install.sh (public).
func hostsInstallScriptHandler(c *gin.Context) {
	script, ok := hostsReadInstallScript()
	if !ok {
		c.String(http.StatusInternalServerError, "# Error: Install script file not found")
		return
	}
	script = hostsInjectInstallVariable(script, "INJECT_PANEL_URL", hostsDerivePanelUrl(c))
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(script))
}

// hostsInstallScriptTokenHandler mirrors GET /install.sh/:token (public).
func hostsInstallScriptTokenHandler(c *gin.Context) {
	ctx := c.Request.Context()
	token := c.Param("token")
	if len(token) < 10 {
		c.String(http.StatusBadRequest, "# Error: Invalid token format")
		return
	}

	host, err := hostsHostByInstallToken(ctx, token)
	if err != nil {
		c.String(http.StatusInternalServerError, "# Error: Internal server error")
		return
	}
	if host == nil {
		c.String(http.StatusNotFound, "# Error: Invalid or expired token")
		return
	}
	if host.IsInstalled {
		c.String(http.StatusForbidden, "# Error: Host already installed")
		return
	}
	if host.InstallTokenExpire == nil {
		c.String(http.StatusForbidden, "# Error: Invalid install token")
		return
	}
	if time.Now().UTC().After(host.InstallTokenExpire.UTC()) {
		c.String(http.StatusForbidden, "# Error: Install token has expired (24 hours)")
		return
	}

	script, ok := hostsReadInstallScript()
	if !ok {
		c.String(http.StatusInternalServerError, "# Error: Install script file not found")
		return
	}
	script = hostsInjectInstallVariable(script, "INJECT_PANEL_URL", hostsDerivePanelUrl(c))

	if installation, err := HostsIssueAgentInstallToken(ctx, host.ID, true); err != nil {
		log.Printf("[hosts] Failed to prepare Host Agent bootstrap credentials for host %d: %v", host.ID, err)
	} else {
		script = hostsInjectInstallVariable(script, "INJECT_AGENT_INSTALL_TOKEN", installation.InstallToken)
		hostsCreateLog(ctx, nil, "host.agent_install_token_issue",
			fmt.Sprintf("为宿主机安装脚本生成 Agent 一次性安装 token: %s (#%d)", host.Name, host.ID), "success", nil)
	}

	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(script))
}

// hostsHostByInstallToken mirrors `prisma.host.findUnique({ where: { installToken } })`.
type hostsInstallTokenHost struct {
	ID                 int32
	Name               string
	IsInstalled        bool
	InstallTokenExpire *time.Time
}

func hostsHostByInstallToken(ctx context.Context, token string) (*hostsInstallTokenHost, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var host struct {
		ID                 int32
		Name               string
		IsInstalled        bool
		InstallTokenExpire *time.Time
	}
	result := db.DB.WithContext(ctx).Table("hosts").
		Select("id, name, is_installed, install_token_expire").
		Where("install_token = ?", token).Limit(1).Scan(&host)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &hostsInstallTokenHost{
		ID:                 host.ID,
		Name:               host.Name,
		IsInstalled:        host.IsInstalled,
		InstallTokenExpire: host.InstallTokenExpire,
	}, nil
}

// hostsCertHandler mirrors GET /cert/:token (public).
func hostsCertHandler(c *gin.Context) {
	ctx := c.Request.Context()
	token := c.Param("token")
	if len(token) < 10 {
		c.String(http.StatusBadRequest, "# Error: Invalid token format")
		return
	}

	host, err := hostsHostByInstallToken(ctx, token)
	if err != nil {
		c.String(http.StatusInternalServerError, "# Error: Internal server error")
		return
	}
	if host == nil {
		c.String(http.StatusNotFound, "# Error: Invalid or expired token")
		return
	}
	if host.IsInstalled {
		c.String(http.StatusForbidden, "# Error: Host already installed")
		return
	}
	if host.InstallTokenExpire == nil {
		c.String(http.StatusForbidden, "# Error: Invalid install token")
		return
	}
	if time.Now().UTC().After(host.InstallTokenExpire.UTC()) {
		c.String(http.StatusForbidden, "# Error: Install token has expired (24 hours)")
		return
	}

	certPath, _ := hostsPanelCertificatePaths()
	data, readErr := os.ReadFile(certPath)
	if readErr != nil {
		c.String(http.StatusInternalServerError, "# Error: Certificate file not found")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", data)
}

// hostsVerifyHandler mirrors POST /:id/verify.
func hostsVerifyHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if !lib.TunnelManagerInstance().RequestHeartbeat(hostID) {
		c.JSON(http.StatusConflict, gin.H{"error": "节点连接不可用或不支持立即上报，请检查节点版本与连接"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "queued": true, "message": "已通知节点立即上报心跳"})
}

// hostsRegenerateInstallHandler mirrors POST /:id/regenerate-install.
func hostsRegenerateInstallHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	installation, err := HostsIssueAgentInstallToken(ctx, hostID, true)
	if err != nil {
		hostsRespondSeam(c, err)
		return
	}
	installCommand := hostsBuildNodeInstallCommand(hostsDerivePanelUrl(c), installation.InstallToken)
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.regenerate_install",
		fmt.Sprintf(`Regenerated install command for host "%s"`, host.Name), "success", nil)

	c.JSON(http.StatusOK, gin.H{
		"success":               true,
		"installCommand":        installCommand,
		"installToken":          installation.InstallToken,
		"installTokenExpiresAt": installation.InstallTokenExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"message":               "Incus 节点接入命令已生成，请在30分钟内使用一次性令牌完成配置",
	})
}

// hostsTestHandler mirrors POST /:id/test.
func hostsTestHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), lib.TunnelDefaultHeartbeatTimeout)
	defer cancel()
	if err := lib.TunnelManagerInstance().RequestHeartbeatAndWait(ctx, hostID, lib.TunnelDefaultHeartbeatTimeout); err != nil {
		var heartbeatErr *lib.TunnelHeartbeatTestError
		if errors.As(err, &heartbeatErr) {
			c.JSON(heartbeatErr.StatusCode, gin.H{"success": false, "error": heartbeatErr.Message})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": "节点上报测试失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "测试通过"})
}

// hostsDetailHandler mirrors GET /:id.
func hostsDetailHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	effectiveStatus := hostsDeriveEffectiveStatus(host)
	if host.Status == "online" && effectiveStatus == "offline" {
		go func() {
			_ = db.HostsUpdateStatus(context.Background(), hostID, models.HostStatusOffline)
		}()
	}

	instances, err := hostsInstancesOnHost(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	storage, err := lib.GetStorageOverview(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	statuses, err := hostsLoadHostServiceStatuses(ctx, []int32{hostID})
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	status := effectiveStatus
	if service, ok := statuses[hostID]; ok && service.Status != "" {
		status = service.Status
	}

	instanceList := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		instanceList = append(instanceList, gin.H{
			"id":     instance.ID,
			"name":   instance.Name,
			"status": string(instance.Status),
		})
	}

	payload := gin.H{
		"id":           host.ID,
		"name":         host.Name,
		"url":          host.URL,
		"status":       status,
		"maintenance":  effectiveStatus == "maintenance",
		"location":     host.Location,
		"countryCode":  hostsValueOr(host.CountryCode, "us"),
		"architecture": hostsValueOr(host.Architecture, "x86_64"),
		"certPath":     hostsDerefString(host.CertPath),
		"keyPath":      hostsDerefString(host.KeyPath),
		"storage":      storage,
		"resources": gin.H{
			"cpuUsed":    host.CPUUsed,
			"memoryUsed": host.MemoryUsed,
			"diskTotal":  storage.TotalMB,
			"diskUsed":   storage.AllocatedMB,
		},
		"cpuAllowanceMax": host.CPUAllowanceMax,
		"memoryMax":       host.MemoryMax,
		"instanceType":    hostsValueOr(host.InstanceType, "container"),
		"natConfig": gin.H{
			"publicIp":       host.NATPublicIP,
			"publicIpv6":     host.NATPublicIPv6,
			"bindIp":         host.NATBindIP,
			"bindIpv6":       host.NATBindIPv6,
			"portRangeStart": host.NATPortStart,
			"portRangeEnd":   host.NATPortEnd,
		},
		"enableApi":          host.EnableAPI,
		"transferEnabled":    host.TransferEnabled,
		"trafficResetDay":    host.TrafficResetDay,
		"notifyPurchase":     host.NotifyPurchase,
		"notifyRenew":        host.NotifyRenew,
		"notifyDestroy":      host.NotifyDestroy,
		"enableResourcePool": host.EnableResourcePool,
		"announcement":       hostsNilIfEmptyPtr(host.Announcement),
		"probeUrl":           host.ProbeURL,
		"tunnelEnabled":      host.TunnelEnabled,
		"targetHost":         host.TargetHost,
		"targetPort":         host.TargetPort,
		"tunnelOnline":       lib.TunnelManagerInstance().IsTunnelOnline(host.ID),
		"instances":          instanceList,
	}
	// `x || undefined` keys disappear from JSON; only set them when truthy.
	if host.IPAddress != nil && *host.IPAddress != "" {
		payload["ipAddress"] = *host.IPAddress
	}
	if host.StorageDriver != "" {
		payload["storageDriver"] = host.StorageDriver
	}
	if host.StorageType != "" {
		payload["storageType"] = host.StorageType
	}
	if host.StoragePath != nil && *host.StoragePath != "" {
		payload["storagePath"] = *host.StoragePath
	}
	if host.StorageSize != 0 {
		payload["storageSize"] = host.StorageSize
	}
	if host.IPv6Mode != 0 {
		payload["ipv6Mode"] = host.IPv6Mode
	}
	if host.IPv6Subnet != nil && *host.IPv6Subnet != "" {
		payload["ipv6Subnet"] = *host.IPv6Subnet
	}
	if host.IPv6Gateway != nil && *host.IPv6Gateway != "" {
		payload["ipv6Gateway"] = *host.IPv6Gateway
	}
	if host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
		payload["ipv6ParentInterface"] = *host.IPv6ParentInterface
	}
	if host.SysctlConfig != nil && *host.SysctlConfig != "" {
		payload["sysctlConfig"] = *host.SysctlConfig
	}

	c.JSON(http.StatusOK, gin.H{"host": payload})
}

// hostsImagesGetHandler mirrors GET /:id/images.
func hostsImagesGetHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	selection, err := db.GetHostImageSelectionState(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if selection == nil {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}
	c.JSON(http.StatusOK, hostsImageSelectionPayload(selection, nil, nil))
}

// hostsImagesPutHandler mirrors PUT /:id/images.
func hostsImagesPutHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"useDefault"}, []ajvFieldSpec{
		{Name: "useDefault", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	imageIDs, _, ok := hostsValidateIntegerArray(c, object, "imageIds", false, -1, -1)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	useDefault, _ := hostsValidatedBool(validated, "useDefault")

	selection, err := db.SaveHostAllowedImages(ctx, hostID, useDefault, imageIDs)
	if err != nil {
		if strings.HasPrefix(err.Error(), "INVALID_IMAGE_FOR_HOST:") {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrImageInvalidHostId, "")
			return
		}
		hostsRespondInternal(c)
		return
	}
	if selection == nil {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	action := fmt.Sprintf(`Updated image policy for host "%s" with %d image(s)`, selection.HostName, len(selection.AllowedImageIDs))
	if selection.UseDefault {
		action = fmt.Sprintf(`Reset image policy for host "%s" to default`, selection.HostName)
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.image_policy", action, "success", nil)

	message := "Host image policy updated"
	if selection.UseDefault {
		message = "Host image policy reset to default"
	}
	c.JSON(http.StatusOK, hostsImageSelectionPayload(selection, &message, nil))
}

// hostsImageSelectionPayload renders the shared image-policy response object.
func hostsImageSelectionPayload(selection *db.HostImageSelectionState, message *string, _ any) gin.H {
	images := make([]gin.H, 0, len(selection.Images))
	for _, image := range selection.Images {
		images = append(images, gin.H{
			"id":           image.ID,
			"name":         image.Name,
			"remoteAlias":  image.RemoteAlias,
			"osType":       image.OSType,
			"architecture": image.Architecture,
			"instanceType": image.InstanceType,
			"icon":         image.Icon,
			"sortOrder":    image.SortOrder,
			"hidden":       image.Hidden,
		})
	}
	payload := gin.H{
		"success": true,
		"host": gin.H{
			"id":           selection.HostID,
			"name":         selection.HostName,
			"architecture": selection.Architecture,
			"instanceType": selection.InstanceType,
		},
		"useDefault":      selection.UseDefault,
		"allowedImageIds": selection.AllowedImageIDs,
		"images":          images,
	}
	if message != nil {
		payload["message"] = *message
	}
	return payload
}

// hostsUpdateHandler mirrors PATCH /:id.
func hostsUpdateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, nil, []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "url", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "location", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "countryCode", Kind: ajvKindString, MinLength: 2, MaxLength: 2},
		{Name: "certPath", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "keyPath", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "cpuAllowanceMax", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "memoryMax", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "instanceType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"container", "vm", "both"}},
		{Name: "ipv6ParentInterface", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "ipv6Subnet", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "transferEnabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "trafficResetDay", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 28},
		{Name: "notifyPurchase", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "notifyRenew", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "notifyDestroy", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "enableResourcePool", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "announcement", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 1000},
		{Name: "probeUrl", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 500},
		{Name: "tunnelEnabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "targetHost", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "targetPort", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	})
	if !ok {
		return
	}
	natConfig, natOK := hostsValidateNatConfig(c, object)
	if !natOK {
		return
	}

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	name, hasName := validated.stringValue("name")
	if hasName {
		nameValidation := lib.ValidateIdentifier(name, "Host name", 2, 64)
		if !nameValidation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": nameValidation.Message})
			return
		}
		if user.Role != "admin" {
			prefix := fmt.Sprintf("PEER%d", user.ID)
			pattern := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(prefix) + `[-_]`)
			if !pattern.MatchString(name) {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("Node name must start with your user ID prefix: %s-", prefix),
					"code":  "INVALID_HOST_NAME_PREFIX",
				})
				return
			}
		}
		duplicate, err := db.HostsGetByUserAndName(ctx, host.UserID, name)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if duplicate != nil && duplicate.ID != hostID {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostNameExists, "")
			return
		}
	}

	rawURL, hasURL := validated.stringValue("url")
	if hasURL {
		urlValidation := lib.ValidateURL(rawURL, "Host URL")
		if !urlValidation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": urlValidation.Message})
			return
		}
		if urlValidation.Sanitized != nil {
			rawURL = *urlValidation.Sanitized
		}
		parsedURL, err := url.Parse(rawURL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Host URL format is invalid"})
			return
		}
		hostAddressValidation := lib.ValidateIPOrDomain(parsedURL.Hostname(), "Host address")
		if !hostAddressValidation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": hostAddressValidation.Message})
			return
		}
	}

	location, hasLocation := validated.stringValue("location")
	if hasLocation && location != "" {
		locationValidation := lib.ValidateName(location, "Location", 1, 100)
		if !locationValidation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": locationValidation.Message})
			return
		}
		if locationValidation.Sanitized != nil {
			location = *locationValidation.Sanitized
		}
	}

	memoryMax, hasMemoryMax := hostsValidatedInt64(validated, "memoryMax")
	if hasMemoryMax && memoryMax != nil && *memoryMax > 0 && *memoryMax < 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "内存最大值不能低于256MB"})
		return
	}

	usedResources, err := db.HostsCalculateResourcesFromInstances(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	cpuAllowanceMax, hasCPUAllowanceMax := hostsValidatedInt64(validated, "cpuAllowanceMax")
	if hasCPUAllowanceMax && cpuAllowanceMax != nil && *cpuAllowanceMax > 0 && *cpuAllowanceMax < usedResources.CPUUsed {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostCpuBelowUsed,
			fmt.Sprintf("当前已使用: %d%%", usedResources.CPUUsed))
		return
	}
	if hasMemoryMax && memoryMax != nil && *memoryMax > 0 && *memoryMax < usedResources.MemoryUsed {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostMemoryBelowUsed,
			fmt.Sprintf("当前已使用: %dMB", usedResources.MemoryUsed))
		return
	}

	ipv6ParentInterface, hasIPv6Parent := validated.stringValue("ipv6ParentInterface")
	ipv6Subnet, hasIPv6Subnet := validated.stringValue("ipv6Subnet")
	var normalizedIPv6Parent *string
	if hasIPv6Parent {
		trimmed := strings.TrimSpace(ipv6ParentInterface)
		if trimmed != "" {
			normalizedIPv6Parent = &trimmed
		}
	}
	var normalizedIPv6Subnet *string
	if hasIPv6Subnet {
		trimmed := strings.TrimSpace(ipv6Subnet)
		if trimmed != "" {
			normalizedIPv6Subnet = &trimmed
		}
	}

	natPublicIP, natPublicIPv6, natBindIP, natBindIPv6, ports, natOK := hostsNormalizeNatConfigUpdate(c, natConfig)
	if !natOK {
		return
	}

	nextIPv6Mode := host.IPv6Mode
	if nextIPv6Mode == 0 {
		nextIPv6Mode = 1
	}
	nextIPv6Subnet := host.IPv6Subnet
	if hasIPv6Subnet {
		nextIPv6Subnet = normalizedIPv6Subnet
	}
	nextIPv6Parent := host.IPv6ParentInterface
	if hasIPv6Parent {
		nextIPv6Parent = normalizedIPv6Parent
	}
	if nextIPv6Mode == 1 && (nextIPv6Subnet == nil || nextIPv6Parent == nil) {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostIpv6RouteRequiresConfig, "")
		return
	}

	instanceType, hasInstanceType := validated.stringValue("instanceType")
	if hasInstanceType && instanceType != host.InstanceType {
		incompatible, err := db.GetIncompatiblePackagesByHostTypeChange(ctx, hostID, instanceType)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if len(incompatible) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":    "Host instance type is incompatible with bound packages",
				"code":     "HOST_INSTANCE_TYPE_MISMATCH",
				"packages": incompatible,
			})
			return
		}
	}

	tunnelEnabled, hasTunnelEnabled := hostsValidatedBool(validated, "tunnelEnabled")
	tunnelModeEffective := host.TunnelEnabled
	if hasTunnelEnabled {
		tunnelModeEffective = tunnelEnabled
	}

	update := db.HostsUpdateData{}
	if hasName {
		if nameValidation := lib.ValidateIdentifier(name, "Host name", 2, 64); nameValidation.Sanitized != nil {
			name = *nameValidation.Sanitized
		}
		update.Name = db.HostsValue(name)
	}
	if hasURL {
		update.URL = db.HostsValue(rawURL)
	}
	if hasLocation {
		update.Location = db.HostsValue(hostsNilIfEmpty(location))
	}
	if value, present := validated.stringValue("countryCode"); present {
		update.CountryCode = db.HostsValue(value)
	}
	if value, present := validated.stringValue("certPath"); present {
		update.CertPath = db.HostsValue(hostsNilIfEmpty(value))
	}
	if value, present := validated.stringValue("keyPath"); present {
		update.KeyPath = db.HostsValue(hostsNilIfEmpty(value))
	}
	if natPublicIP.set {
		update.NATPublicIP = db.HostsValue(natPublicIP.value)
	}
	if natBindIP.set {
		update.NATBindIP = db.HostsValue(natBindIP.value)
	}
	if natBindIPv6.set {
		update.NATBindIPv6 = db.HostsValue(natBindIPv6.value)
	}
	if ports.start != nil {
		update.NATPortStart = db.HostsValue(ports.start)
	}
	if ports.end != nil {
		update.NATPortEnd = db.HostsValue(ports.end)
	}
	if hasIPv6Parent {
		update.IPv6ParentInterface = db.HostsValue(normalizedIPv6Parent)
	}
	if hasIPv6Subnet {
		update.IPv6Subnet = db.HostsValue(normalizedIPv6Subnet)
	}
	if value, present := hostsValidatedInt64(validated, "trafficResetDay"); present && value != nil {
		update.TrafficResetDay = db.HostsValue(int32(*value))
	}
	if value, present := hostsValidatedBool(validated, "notifyPurchase"); present {
		update.NotifyPurchase = db.HostsValue(value)
	}
	if value, present := hostsValidatedBool(validated, "notifyRenew"); present {
		update.NotifyRenew = db.HostsValue(value)
	}
	if value, present := hostsValidatedBool(validated, "notifyDestroy"); present {
		update.NotifyDestroy = db.HostsValue(value)
	}
	if hasTunnelEnabled {
		update.TunnelEnabled = db.HostsValue(tunnelEnabled)
	}
	if value, present := validated.stringValue("targetHost"); present {
		update.TargetHost = db.HostsValue(value)
	}
	if value, present := hostsValidatedInt64(validated, "targetPort"); present && value != nil {
		update.TargetPort = db.HostsValue(int32(*value))
	}

	if hasURL && !tunnelModeEffective {
		if err := hostsPrepareAddressSnapshot(ctx, rawURL, &hostID); err != nil {
			var registryErr *hostsAddressRegistryError
			if errors.As(err, &registryErr) {
				hostsRespondAPIError(c, http.StatusBadRequest, registryErr.Code, registryErr.Details)
				return
			}
			hostsRespondInternal(c)
			return
		}
	}

	transactionErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := db.HostsUpdate(ctx, hostID, update, tx); err != nil {
			return err
		}
		if hasCPUAllowanceMax || hasMemoryMax || hasInstanceType {
			if err := db.HostsUpdateResources(ctx, hostID, db.HostsResourceUpdate{
				CPUAllowanceMax: cpuAllowanceMax,
				MemoryMax:       memoryMax,
				InstanceType:    hostsNilIfEmptyPtr(hostsNilIfEmpty(instanceType)),
			}, tx); err != nil {
				return err
			}
		}
		columns := map[string]any{}
		if value, present := hostsValidatedBool(validated, "transferEnabled"); present {
			columns["transfer_enabled"] = value
		}
		if value, present := hostsValidatedBool(validated, "enableResourcePool"); present {
			columns["enable_resource_pool"] = value
		}
		if value, present := validated.nullableString("announcement"); present {
			if value == nil || *value == "" {
				columns["announcement"] = nil
			} else {
				columns["announcement"] = *value
			}
		}
		if value, present := validated.nullableString("probeUrl"); present {
			if value == nil || *value == "" {
				columns["probe_url"] = nil
			} else {
				columns["probe_url"] = *value
			}
		}
		if natPublicIPv6.set {
			columns["nat_public_ipv6"] = natPublicIPv6.value
		}
		if len(columns) > 0 {
			if err := tx.WithContext(ctx).Table("hosts").Where("id = ?", hostID).Updates(columns).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if transactionErr != nil {
		if errors.Is(transactionErr, db.ErrHostsRecordNotFound) {
			hostsRespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
			return
		}
		hostsRespondInternal(c)
		return
	}

	if hasCertOrKeyOrURL(validated) || hasTunnelEnabled || hasTunnelTarget(validated) {
		lib.RemoveIncusClient(int64(hostID))
	}
	if hasTunnelEnabled || hasTunnelTarget(validated) {
		refreshed, err := db.HostsGetByID(ctx, hostID)
		if err == nil && refreshed != nil {
			// The TypeScript unregisters the tunnel socket itself; the Go tunnel
			// manager owns sockets by reference and has no per-host drop, so the
			// agent route (not ported yet) is responsible for the disconnect.
			if refreshed.TunnelEnabled {
				lib.TunnelManagerInstance().BroadcastConfig(hostID, lib.TunnelTargetConfig{
					TargetHost: refreshed.TargetHost,
					TargetPort: int(refreshed.TargetPort),
				})
			}
		}
	}

	changes := make([]string, 0, 12)
	if hasName && name != "" {
		changes = append(changes, fmt.Sprintf(`name -> "%s"`, name))
	}
	if hasURL && rawURL != "" {
		changes = append(changes, "url -> "+rawURL)
	}
	if hasLocation && location != "" {
		changes = append(changes, "location -> "+location)
	}
	if hasCPUAllowanceMax && cpuAllowanceMax != nil {
		changes = append(changes, fmt.Sprintf("cpuMax -> %d%%", *cpuAllowanceMax))
	}
	if hasMemoryMax && memoryMax != nil {
		changes = append(changes, fmt.Sprintf("memoryMax -> %dMB", *memoryMax))
	}
	if hasInstanceType && instanceType != "" {
		changes = append(changes, "instanceType -> "+instanceType)
	}
	if natPublicIP.set && natPublicIP.value != nil {
		changes = append(changes, "natPublicIp -> "+*natPublicIP.value)
	}
	if value, present := hostsValidatedBool(validated, "transferEnabled"); present {
		changes = append(changes, fmt.Sprintf("transferEnabled -> %t", value))
	}
	if value, present := hostsValidatedInt64(validated, "trafficResetDay"); present && value != nil {
		changes = append(changes, fmt.Sprintf("trafficResetDay -> %d", *value))
	}
	if value, present := hostsValidatedBool(validated, "notifyPurchase"); present {
		changes = append(changes, fmt.Sprintf("notifyPurchase -> %t", value))
	}
	if value, present := hostsValidatedBool(validated, "notifyRenew"); present {
		changes = append(changes, fmt.Sprintf("notifyRenew -> %t", value))
	}
	if value, present := hostsValidatedBool(validated, "notifyDestroy"); present {
		changes = append(changes, fmt.Sprintf("notifyDestroy -> %t", value))
	}
	detail := "no major changes"
	if len(changes) > 0 {
		detail = strings.Join(changes, ", ")
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.update",
		fmt.Sprintf(`Updated host "%s" [%s]`, host.Name, detail), "success", nil)

	c.JSON(http.StatusOK, gin.H{"message": "Host config updated"})
}

// hostsOptionalString is the tri-state used by the NAT normalisation: `set`
// distinguishes "not in the request" from "explicit null".
type hostsOptionalString struct {
	set   bool
	value *string
}

// hostsNormalizeNatConfigUpdate mirrors the PATCH NAT block, where `'publicIp' in
// natConfig` decides whether the value is touched at all.
func hostsNormalizeNatConfigUpdate(c *gin.Context, natConfig map[string]any) (hostsOptionalString, hostsOptionalString, hostsOptionalString, hostsOptionalString, hostsNatPorts, bool) {
	var publicIP, publicIPv6, bindIP, bindIPv6 hostsOptionalString
	ports := hostsNatPorts{}
	if natConfig == nil {
		return publicIP, publicIPv6, bindIP, bindIPv6, ports, true
	}

	if raw, present := natConfig["publicIp"]; present {
		publicIP.set = true
		value := strings.TrimSpace(fmt.Sprint(raw))
		if raw == nil {
			value = ""
		}
		if value != "" {
			validation := lib.ValidateIPAddress(value, "NAT public IP")
			if !validation.Valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": validation.Message})
				return publicIP, publicIPv6, bindIP, bindIPv6, ports, false
			}
			if validation.Sanitized != nil {
				value = *validation.Sanitized
			}
			publicIP.value = &value
		}
	}
	if raw, present := natConfig["bindIp"]; present {
		bindIP.set = true
		value := ""
		if raw != nil {
			value = strings.TrimSpace(fmt.Sprint(raw))
		}
		if value != "" {
			validation := lib.ValidateIPAddress(value, "NAT bind IP")
			if !validation.Valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": validation.Message})
				return publicIP, publicIPv6, bindIP, bindIPv6, ports, false
			}
			if validation.Sanitized != nil {
				value = *validation.Sanitized
			}
			bindIP.value = &value
		}
	}
	if raw, present := natConfig["publicIpv6"]; present {
		publicIPv6.set = true
		value := ""
		if raw != nil {
			value = strings.TrimSpace(fmt.Sprint(raw))
		}
		if value != "" {
			validation := lib.ValidateIPAddress(value, "NAT public IPv6")
			if validation.Sanitized != nil {
				value = *validation.Sanitized
			}
			if !validation.Valid || !strings.Contains(value, ":") {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid NAT public IPv6 address format"})
				return publicIP, publicIPv6, bindIP, bindIPv6, ports, false
			}
			publicIPv6.value = &value
		}
	}
	if raw, present := natConfig["bindIpv6"]; present {
		bindIPv6.set = true
		value := ""
		if raw != nil {
			value = strings.TrimSpace(fmt.Sprint(raw))
		}
		if value != "" {
			validation := lib.ValidateIPAddress(value, "NAT bind IPv6")
			if validation.Sanitized != nil {
				value = *validation.Sanitized
			}
			if !validation.Valid || !strings.Contains(value, ":") {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid NAT bind IPv6 address format"})
				return publicIP, publicIPv6, bindIP, bindIPv6, ports, false
			}
			bindIPv6.value = &value
		}
	}

	if start, ok := natConfig["portRangeStart"]; ok && start != nil {
		if number, ok := hostsJSONNumber(start); ok {
			value := int32(number)
			ports.start = &value
		}
	}
	if end, ok := natConfig["portRangeEnd"]; ok && end != nil {
		if number, ok := hostsJSONNumber(end); ok {
			value := int32(number)
			ports.end = &value
		}
	}
	if ports.start != nil && ports.end != nil && *ports.end < *ports.start {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Port range end must be greater than or equal to start", "code": "INVALID_PORT_RANGE"})
		return publicIP, publicIPv6, bindIP, bindIPv6, ports, false
	}
	return publicIP, publicIPv6, bindIP, bindIPv6, ports, true
}

func hasCertOrKeyOrURL(validated ajvValidated) bool {
	if _, ok := validated.stringValue("certPath"); ok {
		return true
	}
	if _, ok := validated.stringValue("keyPath"); ok {
		return true
	}
	_, ok := validated.stringValue("url")
	return ok
}

func hasTunnelTarget(validated ajvValidated) bool {
	if _, ok := validated.stringValue("targetHost"); ok {
		return true
	}
	_, ok := hostsValidatedInt64(validated, "targetPort")
	return ok
}

// hostsDeleteHandler mirrors DELETE /:id.
func hostsDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instanceCount, err := hostsInstanceCount(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if instanceCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("该宿主机上还有 %d 个实例，请先删除或迁移实例", instanceCount),
		})
		return
	}

	lib.RemoveIncusClient(int64(hostID))
	if err := db.HostsDelete(ctx, hostID); err != nil {
		hostsRespondInternal(c)
		return
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.delete",
		fmt.Sprintf(`Deleted host "%s" [url: %s, location: %s]`, host.Name, host.URL, valueOrNA(hostsDerefString(host.Location))),
		"success", nil)

	c.JSON(http.StatusOK, gin.H{"message": "Host deleted"})
}

// ==================== B. 维护与批量 ====================

// hostsMaintenanceHandler mirrors POST /:id/maintenance.
func hostsMaintenanceHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, _, ok := hostsValidatedBody(c, []string{"enabled"}, []ajvFieldSpec{
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	enabled, _ := hostsValidatedBool(validated, "enabled")

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	status := models.HostStatusOnline
	action := "host.maintenance.disable"
	message := "Maintenance mode disabled"
	if enabled {
		status = models.HostStatusMaintenance
		action = "host.maintenance.enable"
		message = "Maintenance mode enabled"
	}
	if err := db.HostsUpdateStatus(ctx, hostID, status); err != nil {
		hostsRespondInternal(c)
		return
	}
	verb := "Disabled"
	if enabled {
		verb = "Enabled"
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), action,
		fmt.Sprintf(`%s maintenance mode for host "%s"`, verb, host.Name), "success", nil)

	c.JSON(http.StatusOK, gin.H{"message": message})
}

// hostsSyncHandler mirrors POST /:id/sync.
func hostsSyncHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if !lib.TunnelManagerInstance().RequestHeartbeat(hostID) {
		c.JSON(http.StatusConflict, gin.H{"error": "节点连接不可用或不支持立即上报，请检查节点版本与连接"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "已通知节点立即上报心跳", "queued": true})
}

// hostsBatchDeletePreviewHandler mirrors POST /:id/instances/batch-delete-preview.
func hostsBatchDeletePreviewHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"instanceIds"}, []ajvFieldSpec{})
	_ = validated
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, 100)
	if !ok {
		return
	}

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instances, err := hostsInstancesMatching(ctx, hostID, instanceIDs, []string{"deleted"}, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	previews := make([]gin.H, 0, len(instances))
	totalRefund := 0.0
	for _, instance := range instances {
		refund, err := HostsInstanceRefund(ctx, HostsRefundInput{
			ID:            instance.ID,
			BillingPrice:  instance.BillingPrice,
			BillingCycle:  instance.BillingCycle,
			ExpiresAt:     instance.ExpiresAt,
			PackagePlanID: instance.PackagePlanID,
		})
		if err != nil {
			hostsRespondSeam(c, err)
			return
		}
		username, err := hostsUsernameByID(ctx, instance.UserID)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		isOwn := instance.UserID == host.UserID
		if !isOwn && refund.IsPaid {
			totalRefund += refund.RefundAmount
		}
		previews = append(previews, gin.H{
			"id":            instance.ID,
			"name":          instance.Name,
			"username":      username,
			"userId":        instance.UserID,
			"isOwnInstance": isOwn,
			"isPaid":        refund.IsPaid,
			"remainingDays": refund.RemainingDays,
			"refundAmount":  refund.RefundAmount,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"instances":         previews,
		"totalRefundAmount": math.Round(totalRefund*100) / 100,
	})
}

// hostsBatchDeleteHandler mirrors DELETE /:id/instances/batch.
func hostsBatchDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"instanceIds"}, []ajvFieldSpec{
		{Name: "reason", Kind: ajvKindString, MinLength: -1, MaxLength: 500},
		{Name: "databaseOnly", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, 100)
	if !ok {
		return
	}
	reason, _ := validated.stringValue("reason")
	databaseOnly, _ := hostsValidatedBool(validated, "databaseOnly")

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instances, err := hostsInstancesMatching(ctx, hostID, instanceIDs, []string{"deleted"}, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(instances) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有找到可删除的实例"})
		return
	}

	// 执行段（Incus 停止/删除、快照/备份/反代清理、退款、通知、资源回滚）依赖
	// lib/incus + instance-billing + notifier，均未移植 —— 显式失败，不再伪装成功。
	plan := make([]HostsBatchDeletePlan, 0, len(instances))
	for _, instance := range instances {
		plan = append(plan, HostsBatchDeletePlan{
			Instance: instance,
			Host:     host,
			Reason:   reason,
			DBOnly:   databaseOnly,
		})
	}
	results, totalRefund, err := HostsBatchDeleteExecutor(ctx, plan)
	_ = results
	_ = totalRefund
	if err != nil {
		hostsRespondSeam(c, err)
		return
	}
	hostsRespondSeam(c, errHostsSeam("DELETE /:hostId/instances/batch"))
}

// hostsUsernameByID mirrors the `user: { select: { username } }` join.
func hostsUsernameByID(ctx context.Context, userID int32) (string, error) {
	if db.DB == nil {
		return "", errors.New("hosts: database not initialized")
	}
	var username string
	result := db.DB.WithContext(ctx).Table("users").Select("username").Where("id = ?", userID).Limit(1).Scan(&username)
	if result.Error != nil {
		return "", result.Error
	}
	return username, nil
}

// ==================== C. 存储池 ====================

// hostsStoragePoolsListHandler mirrors GET /:id/storage-pools.
func hostsStoragePoolsListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	pools, err := HostsStoragePoolsSnapshot(ctx, hostID)
	if err != nil {
		hostsRespondSeam(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"pools": pools})
}

// hostsStoragePoolCreateHandler mirrors POST /:id/storage-pools.
func hostsStoragePoolCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, _, ok := hostsValidatedBody(c, []string{"name"}, []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: 64},
		{Name: "driver", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"zfs", "lvm", "btrfs", "dir"}},
		{Name: "source", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "size", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "useLoop", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "zfsPoolName", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "loopDirectory", Kind: ajvKindString, MinLength: -1, MaxLength: 255},
		{Name: "preallocate", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "lvmVgName", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "lvmThinpoolName", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "lvmUseThinpool", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "description", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "purpose", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"instance_data", "instance_storage"}},
		{Name: "useExisting", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "existingSource", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	name, _ := validated.stringValue("name")
	driver, _ := validated.stringValue("driver")
	source, _ := validated.stringValue("source")
	size, _ := validated.stringValue("size")
	useExisting, _ := hostsValidatedBool(validated, "useExisting")
	existingSource, _ := validated.stringValue("existingSource")
	loopDirectory, _ := validated.stringValue("loopDirectory")
	zfsPoolName, _ := validated.stringValue("zfsPoolName")
	lvmVgName, _ := validated.stringValue("lvmVgName")
	lvmThinpoolName, _ := validated.stringValue("lvmThinpoolName")
	lvmUseThinpool, hasLVMUseThinpool := hostsValidatedBool(validated, "lvmUseThinpool")
	preallocate, _ := hostsValidatedBool(validated, "preallocate")
	useLoop, _ := hostsValidatedBool(validated, "useLoop")
	description, hasDescription := validated.stringValue("description")
	purpose, _ := validated.stringValue("purpose")

	if !useExisting {
		trimmedSource := strings.TrimSpace(source)
		trimmedSize := strings.TrimSpace(size)
		supportsLoop := driver == "zfs" || driver == "lvm" || driver == "btrfs"
		if supportsLoop && trimmedSource != "" && trimmedSize != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能同时指定 source 和 size，请明确选择物理路径或 Loop 文件模式"})
			return
		}
		if driver == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "创建新存储池需要指定驱动类型"})
			return
		}
		loopMode := supportsLoop && (useLoop || (trimmedSource == "" && trimmedSize != ""))
		if (driver == "lvm" || driver == "btrfs") && trimmedSource == "" && trimmedSize == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": strings.ToUpper(driver) + " 需要指定 source（物理盘）或 size（Loop 文件）"})
			return
		}
		_ = loopMode
	}

	data := HostsStoragePoolCreateData{
		HostID:      hostID,
		Name:        name,
		Driver:      driver,
		Source:      source,
		Size:        size,
		LoopDir:     loopDirectory,
		Preallocate: preallocate,
		UseLoop:     useLoop,
		ZFSPoolName: zfsPoolName,
		LVMVGName:   lvmVgName,
		LVMThinpool: lvmThinpoolName,
		UseExisting: useExisting,
		ExistingSrc: existingSource,
		Purpose:     purpose,
	}
	if hasLVMUseThinpool {
		data.LVMUseThin = &lvmUseThinpool
	}
	if hasDescription {
		data.Description = &description
	}

	payload, status, err := HostsStoragePoolCreate(ctx, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建存储池失败: " + hostsFormatStoragePoolCreateError(err)})
		return
	}
	c.JSON(status, payload)
}

// hostsStoragePoolDeleteHandler mirrors DELETE /:id/storage-pools/:poolName.
func hostsStoragePoolDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	poolName := c.Param("poolName")
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if err := HostsStoragePoolDelete(ctx, hostID, poolName); err != nil {
		hostsRespondSeam(c, err)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "storage.delete",
		fmt.Sprintf(`Deleted storage pool "%s" from host "%s"`, poolName, host.Name), "success", nil)
	c.JSON(http.StatusOK, gin.H{"message": "存储池删除成功"})
}

// hostsStoragePoolUpdateHandler mirrors PATCH /:id/storage-pools/:poolName.
func hostsStoragePoolUpdateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	poolName := c.Param("poolName")
	validated, _, ok := hostsValidatedBody(c, nil, []ajvFieldSpec{
		{Name: "size", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "description", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "purpose", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"instance_data", "instance_storage"}},
	})
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	size, hasSize := validated.stringValue("size")
	description, hasDescription := validated.stringValue("description")
	purpose, hasPurpose := validated.stringValue("purpose")
	if !hasSize && !hasDescription && !hasPurpose {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供要修改的内容"})
		return
	}

	var sizePtr, descriptionPtr, purposePtr *string
	if hasSize {
		sizePtr = &size
	}
	if hasDescription {
		descriptionPtr = &description
	}
	if hasPurpose {
		purposePtr = &purpose
	}
	if err := HostsStoragePoolUpdate(ctx, hostID, poolName, sizePtr, descriptionPtr, purposePtr); err != nil {
		hostsRespondSeam(c, err)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "storage.update",
		fmt.Sprintf(`Updated storage pool "%s" on host "%s"`, poolName, host.Name), "success", nil)
	lib.TunnelManagerInstance().RequestHeartbeat(hostID)
	c.JSON(http.StatusOK, gin.H{"message": "存储池修改成功，容量将由节点心跳更新"})
}

// ==================== D. 公网 IPv4 地址池 ====================

// hostsPublicIpv4PoolsHandler mirrors GET /:id/public-ipv4/pools.
func hostsPublicIpv4PoolsHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	pools, err := db.ListPublicIpv4Pools(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	items := make([]gin.H, 0, len(pools))
	for _, pool := range pools {
		total := len(pool.Addresses)
		free, assigned, disabled := 0, 0, 0
		for _, address := range pool.Addresses {
			switch string(address.Status) {
			case "free":
				free++
			case "assigned":
				assigned++
			case "disabled":
				disabled++
			}
		}
		items = append(items, gin.H{
			"id":           pool.ID,
			"name":         pool.Name,
			"cidr":         pool.CIDR,
			"gateway":      pool.Gateway,
			"prefixLength": pool.PrefixLength,
			"dns":          pool.DNS,
			"enabled":      pool.Enabled,
			"notes":        pool.Notes,
			"stats": gin.H{
				"total":    total,
				"free":     free,
				"assigned": assigned,
				"disabled": disabled,
			},
			"addresses": pool.Addresses,
		})
	}
	c.JSON(http.StatusOK, gin.H{"pools": items})
}

// hostsPublicIpv4PoolCreateHandler mirrors POST /:id/public-ipv4/pools.
func hostsPublicIpv4PoolCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	body, ok := hostsDecodedBody(c)
	if !ok {
		return
	}
	object, message := hostsBodyObject(body)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	name, _ := object["name"].(string)
	gateway, _ := object["gateway"].(string)
	if strings.TrimSpace(name) == "" || strings.TrimSpace(gateway) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "地址池名称与网关必填"})
		return
	}
	var cidr *string
	if value, ok := object["cidr"].(string); ok && value != "" {
		cidr = &value
	}
	var prefixLength *int32
	if number, ok := hostsJSONNumber(object["prefixLength"]); ok {
		value := int32(number)
		prefixLength = &value
	}
	var notes *string
	if value, ok := object["notes"].(string); ok && value != "" {
		notes = &value
	}

	pool, err := db.CreatePublicIpv4Pool(ctx, db.PublicIpv4CreatePoolInput{
		HostID:       hostID,
		Name:         name,
		CIDR:         cidr,
		Gateway:      gateway,
		PrefixLength: prefixLength,
		DNS:          hostsParsePublicIpv4List(object["dns"]),
		Notes:        notes,
		Addresses:    hostsParsePublicIpv4List(object["addresses"]),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建 IPv4 地址池失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"message": "IPv4 地址池创建成功", "id": pool.ID})
}

// hostsPublicIpv4AddressesAddHandler mirrors POST /:id/public-ipv4/pools/:poolId/addresses.
func hostsPublicIpv4AddressesAddHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	poolID, ok := hostsParsePositiveRouteID(c.Param("poolId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, ok := hostsDecodedBody(c)
	if !ok {
		return
	}
	object, message := hostsBodyObject(body)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	addresses := hostsParsePublicIpv4List(object["addresses"])
	if len(addresses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少需要提供一个 IP 地址"})
		return
	}
	count, err := db.AddPublicIpv4Addresses(ctx, db.PublicIpv4AddAddressesInput{
		HostID:    hostID,
		PoolID:    poolID,
		Addresses: addresses,
	})
	if err != nil {
		if errors.Is(err, db.ErrPublicIpv4PoolNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "IPv4 地址池不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "添加地址失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "地址添加成功", "count": count})
}

// hostsPublicIpv4AddressStatusHandler mirrors PATCH
// /:id/public-ipv4/addresses/:addressId.
func hostsPublicIpv4AddressStatusHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	addressID, ok := hostsParsePositiveRouteID(c.Param("addressId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	body, ok := hostsDecodedBody(c)
	if !ok {
		return
	}
	object, message := hostsBodyObject(body)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	status, _ := object["status"].(string)
	if status != "free" && status != "disabled" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的地址状态"})
		return
	}
	_, err := db.SetPublicIpv4AddressStatus(ctx, hostID, addressID, models.PublicIpv4AddressStatus(status))
	if err != nil {
		switch {
		case errors.Is(err, db.ErrPublicIpv4AddressNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "IP 地址不存在"})
		case errors.Is(err, db.ErrPublicIpv4AddressAssigned):
			c.JSON(http.StatusBadRequest, gin.H{"error": "该地址已被实例占用，无法修改状态"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新地址状态失败: " + err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "地址状态更新成功"})
}

// hostsPublicIpv4AddressDeleteHandler mirrors DELETE
// /:id/public-ipv4/addresses/:addressId.
func hostsPublicIpv4AddressDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	addressID, ok := hostsParsePositiveRouteID(c.Param("addressId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if err := db.DeletePublicIpv4Address(ctx, hostID, addressID); err != nil {
		switch {
		case errors.Is(err, db.ErrPublicIpv4AddressNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "IP 地址不存在"})
		case errors.Is(err, db.ErrPublicIpv4AddressAssigned):
			c.JSON(http.StatusBadRequest, gin.H{"error": "该地址已被实例占用，无法删除"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除地址失败: " + err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "地址删除成功"})
}

// ==================== D. Caddy 反代管理 ====================

// hostsAgentHeartbeatAlive mirrors the agentHeartbeatAlive probes (3 minute window).
func hostsAgentHeartbeatAlive(ctx context.Context, hostID int32) (bool, error) {
	if db.DB == nil {
		return false, errors.New("hosts: database not initialized")
	}
	var agent struct {
		Status     string
		LastSeenAt *time.Time
	}
	result := db.DB.WithContext(ctx).Table("host_agents").
		Select("status, last_seen_at").Where("host_id = ? AND enabled = true", hostID).Limit(1).Scan(&agent)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	if agent.Status != "online" || agent.LastSeenAt == nil {
		return false, nil
	}
	return time.Now().UTC().Sub(agent.LastSeenAt.UTC()) < 3*time.Minute, nil
}

// hostsCaddyStatusHandler mirrors GET /:id/caddy.
func hostsCaddyStatusHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	sitesCount, err := db.ProxySitesCountByHost(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	agentOnline, err := hostsAgentHeartbeatAlive(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	natPublicIP := host.NATPublicIP
	if natPublicIP == nil {
		natPublicIP = host.IPAddress
	}
	port := host.CaddyPort
	if port == 0 {
		port = 2019
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":      host.CaddyEnabled,
		"port":         port,
		"hasPassword":  false,
		"natPublicIp":  natPublicIP,
		"sitesCount":   sitesCount,
		"agentOnline":  agentOnline,
		"tunnelOnline": lib.TunnelManagerInstance().IsTunnelOnline(hostID),
	})
}

// hostsCaddyInstallHandler mirrors POST /:id/caddy/install.
func hostsCaddyInstallHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if host.CaddyEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Caddy 已安装"})
		return
	}
	alive, err := hostsAgentHeartbeatAlive(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if !alive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "宿主机 Agent 未在线，无法安装 Caddy（请确认 Agent 心跳正常）"})
		return
	}
	if err := HostsCaddyInstallRequested(hostID); err != nil {
		hostsRespondSeam(c, err)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "caddy.install.request",
		fmt.Sprintf(`Requested Agent to install Caddy on host "%s"`, host.Name), "success", nil)
	c.JSON(http.StatusOK, gin.H{"message": "已通知 Agent 安装 Caddy，请等候其完成上报", "accepted": true})
}

// hostsCaddyUninstallHandler mirrors POST /:id/caddy/uninstall.
func hostsCaddyUninstallHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if !host.CaddyEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Caddy 未安装，无需卸载"})
		return
	}
	alive, err := hostsAgentHeartbeatAlive(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if !alive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "宿主机 Agent 未在线，无法卸载 Caddy（请确认 Agent 心跳正常）"})
		return
	}
	if err := HostsCaddyUninstallRequested(hostID); err != nil {
		hostsRespondSeam(c, err)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "caddy.uninstall.request",
		fmt.Sprintf(`Requested Agent to uninstall Caddy from host "%s"`, host.Name), "success", nil)
	c.JSON(http.StatusOK, gin.H{"message": "已通知 Agent 卸载 Caddy，请等候其完成上报", "accepted": true})
}

// hostsAgentLogTicketHandler mirrors POST /:id/agent/logs/ticket.
func hostsAgentLogTicketHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if user.IssuedAt == 0 {
		hostsRespondAPIError(c, http.StatusUnauthorized, lib.ErrUnauthorized, "")
		return
	}
	ticket := HostsAgentLogTicket.Issue(user.ID, hostID, user.IssuedAt, user.SID)
	c.JSON(http.StatusOK, gin.H{"ticket": ticket, "expiresIn": 60})
}

// hostsAgentLogStreamHandler mirrors GET /:id/agent/logs/stream (public, ticket auth).
func hostsAgentLogStreamHandler(c *gin.Context) {
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	ticket := c.Query("ticket")
	if ticket == "" {
		hostsRespondAPIError(c, http.StatusUnauthorized, lib.ErrUnauthorized, "")
		return
	}
	if !HostsAgentLogTicket.Consume(ticket, hostID) {
		hostsRespondAPIError(c, http.StatusUnauthorized, lib.ErrUnauthorized, "")
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	writer := c.Writer
	writer.WriteHeaderNow()
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	subscribeCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	write := func(chunk string) bool {
		if _, err := writer.WriteString(chunk); err != nil {
			return false
		}
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return true
	}
	if err := HostsAgentLogStreamSubscribe(subscribeCtx, hostID, write); err != nil {
		log.Printf("[hosts] agent log stream unavailable for host %d: %v", hostID, err)
	}
	<-subscribeCtx.Done()
}

// hostsCaddySitesHandler mirrors GET /:id/caddy/sites.
func hostsCaddySitesHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	rawPage := 1
	if value, err := strconv.Atoi(c.DefaultQuery("page", "1")); err == nil {
		rawPage = value
	}
	if rawPage < 1 {
		rawPage = 1
	}
	rawPageSize := 10
	if value, err := strconv.Atoi(c.DefaultQuery("pageSize", "10")); err == nil {
		rawPageSize = value
	}
	if rawPageSize > 100 {
		rawPageSize = 100
	}
	if rawPageSize < 1 {
		rawPageSize = 1
	}

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	sites, err := db.ProxySitesByHostID(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	total := len(sites)
	start := (rawPage - 1) * rawPageSize
	if start > total {
		start = total
	}
	end := start + rawPageSize
	if end > total {
		end = total
	}
	page := sites[start:end]
	items := make([]gin.H, 0, len(page))
	for _, site := range page {
		instance := gin.H(nil)
		if site.Instance.ID != 0 {
			instance = gin.H{
				"id":     site.Instance.ID,
				"name":   site.Instance.Name,
				"ipv4":   site.Instance.IPv4,
				"status": site.Instance.Status,
			}
		}
		items = append(items, gin.H{
			"id":           site.ID,
			"domain":       site.Domain,
			"targetPort":   site.TargetPort,
			"httpsEnabled": site.HTTPSEnabled,
			"status":       site.Status,
			"enabled":      site.Enabled,
			"error":        site.Error,
			"createdAt":    site.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			"instance":     instance,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"sites":      items,
		"total":      total,
		"page":       rawPage,
		"pageSize":   rawPageSize,
		"totalPages": int(math.Ceil(float64(total) / float64(rawPageSize))),
	})
}

// ==================== E. 容量与实例运维 ====================

// hostsIncusRequest is the shared "GET one Incus resource" helper.
func hostsIncusRequest(ctx context.Context, client *lib.IncusClient, path string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodGet, path, nil, 0, nil)
}

// hostsSyncStatusHandler mirrors POST /:id/instances/sync-status.
func hostsSyncStatusHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"instanceIds"}, []ajvFieldSpec{})
	_ = validated
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, 100)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instances, err := hostsInstancesMatching(ctx, hostID, instanceIDs, []string{"deleted", "creating"}, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(instances) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有找到可同步的实例"})
		return
	}

	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法连接到节点: " + err.Error()})
		return
	}

	results := make([]gin.H, 0, len(instances))
	syncedCount, changedCount, ipv4ChangedCount, ipv6ChangedCount := 0, 0, 0, 0
	for _, instance := range instances {
		stateRaw, stateErr := hostsGetInstanceState(ctx, client, instance.IncusID)
		if stateErr != nil {
			if lib.IsIncusNotFound(stateErr) {
				_ = db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatusDeleted, nil)
				changedCount++
				results = append(results, gin.H{
					"id": instance.ID, "name": instance.Name, "success": true,
					"from": string(instance.Status), "to": "deleted",
				})
				continue
			}
			results = append(results, gin.H{
				"id": instance.ID, "name": instance.Name, "success": false, "error": stateErr.Error(),
			})
			continue
		}
		instanceRaw, instanceErr := hostsIncusRequest(ctx, client, "/1.0/instances/"+instance.IncusID)
		if instanceErr != nil {
			results = append(results, gin.H{
				"id": instance.ID, "name": instance.Name, "success": false, "error": instanceErr.Error(),
			})
			continue
		}

		var state struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(stateRaw, &state)
		if state.Status == "" {
			results = append(results, gin.H{
				"id": instance.ID, "name": instance.Name, "success": false, "error": "无法获取实例状态",
			})
			continue
		}
		incusStatus := hostsMapInstanceStatus(state.Status)
		if incusStatus == "unknown" {
			results = append(results, gin.H{
				"id": instance.ID, "name": instance.Name, "success": false, "error": "未知状态: " + state.Status,
			})
			continue
		}

		statusChanged := false
		previous := string(instance.Status)
		if previous != incusStatus {
			_ = db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatus(incusStatus), nil)
			statusChanged = true
			changedCount++
		}
		delta, err := HostsSnapshotSyncApplier(ctx, instance, instanceRaw, stateRaw)
		if err != nil {
			results = append(results, gin.H{
				"id": instance.ID, "name": instance.Name, "success": false, "error": err.Error(),
			})
			continue
		}
		if delta.IPv4Changed {
			ipv4ChangedCount++
		}
		if delta.IPv6Changed {
			ipv6ChangedCount++
		}
		entry := gin.H{"id": instance.ID, "name": instance.Name, "success": true}
		if statusChanged {
			entry["from"] = previous
			entry["to"] = incusStatus
		}
		if delta.IPv4Changed {
			entry["ipv4Changed"] = true
			entry["oldIpv4"] = delta.OldIPv4
			entry["newIpv4"] = delta.NewIPv4
		}
		if delta.IPv6Changed {
			entry["ipv6Changed"] = true
			entry["oldIpv6"] = delta.OldIPv6
			entry["newIpv6"] = delta.NewIPv6
		}
		results = append(results, entry)
		syncedCount++
	}

	if changedCount > 0 {
		hostsRecalculateHostResources(ctx, hostID)
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.sync_instance_status", fmt.Sprintf(
		`Synced %d instances on host "%s", %d status changed, %d IPv4 changed, %d IPv6 changed`,
		syncedCount, host.Name, changedCount, ipv4ChangedCount, ipv6ChangedCount), "success", nil)

	c.JSON(http.StatusOK, gin.H{
		"message":          "同步完成",
		"results":          results,
		"syncedCount":      syncedCount,
		"changedCount":     changedCount,
		"ipv4ChangedCount": ipv4ChangedCount,
		"ipv6ChangedCount": ipv6ChangedCount,
		"failedCount":      len(instances) - syncedCount,
	})
}

// hostsRecalculateHostResources mirrors the "recalculate usage + port count"
// block repeated by several routes.
func hostsRecalculateHostResources(ctx context.Context, hostID int32) {
	if db.DB == nil {
		return
	}
	used, err := db.HostsCalculateResourcesFromInstances(ctx, hostID)
	if err != nil {
		log.Printf("[hosts] 重新计算宿主机资源失败: %v", err)
		return
	}
	var ports int64
	_ = db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM port_mappings pm
		 JOIN instances i ON i.id = pm.instance_id
		 WHERE i.host_id = ? AND i.status <> 'deleted'`, hostID).Scan(&ports).Error
	_ = db.DB.WithContext(ctx).Exec(
		`UPDATE hosts SET cpu_used = ?, memory_used = ?, disk_used = ?, nat_ports_used_count = ?,
		 updated_at = NOW() WHERE id = ?`,
		used.CPUUsed, used.MemoryUsed, used.DiskUsed, ports, hostID).Error
}

// hostsRecalculateResourcesHandler mirrors POST /:id/recalculate-resources.
func hostsRecalculateResourcesHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	before := gin.H{
		"cpuUsed":           host.CPUUsed,
		"memoryUsed":        host.MemoryUsed,
		"diskUsed":          host.DiskUsed,
		"natPortsUsedCount": nil,
		"cpuAllowanceMax":   host.CPUAllowanceMax,
		"memoryMax":         host.MemoryMax,
	}
	var portsBefore int32
	if db.DB != nil {
		_ = db.DB.WithContext(ctx).Raw(`SELECT nat_ports_used_count FROM hosts WHERE id = ?`, hostID).Scan(&portsBefore).Error
	}
	before["natPortsUsedCount"] = portsBefore

	used, err := db.HostsCalculateResourcesFromInstances(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	var ports int64
	if db.DB != nil {
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT COUNT(*) FROM port_mappings pm
			 JOIN instances i ON i.id = pm.instance_id
			 WHERE i.host_id = ? AND i.status <> 'deleted'`, hostID).Scan(&ports).Error; err != nil {
			hostsRespondInternal(c)
			return
		}
		if err := db.DB.WithContext(ctx).Exec(
			`UPDATE hosts SET cpu_used = ?, memory_used = ?, disk_used = ?, nat_ports_used_count = ?,
			 updated_at = NOW() WHERE id = ?`,
			used.CPUUsed, used.MemoryUsed, used.DiskUsed, ports, hostID).Error; err != nil {
			hostsRespondInternal(c)
			return
		}
	}

	after := gin.H{
		"cpuUsed":           used.CPUUsed,
		"memoryUsed":        used.MemoryUsed,
		"diskUsed":          used.DiskUsed,
		"natPortsUsedCount": ports,
		"cpuAllowanceMax":   host.CPUAllowanceMax,
		"memoryMax":         host.MemoryMax,
	}
	diff := gin.H{
		"cpuUsed":           used.CPUUsed - int64(host.CPUUsed),
		"memoryUsed":        used.MemoryUsed - int64(host.MemoryUsed),
		"diskUsed":          used.DiskUsed - int64(host.DiskUsed),
		"natPortsUsedCount": ports - int64(portsBefore),
		"cpuAllowanceMax":   0,
		"memoryMax":         0,
	}
	hasChanges := used.CPUUsed != int64(host.CPUUsed) || used.MemoryUsed != int64(host.MemoryUsed) ||
		used.DiskUsed != int64(host.DiskUsed) || ports != int64(portsBefore)

	message := "资源校对完成，无差异"
	if hasChanges {
		message = "资源校对完成，已修正差异"
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.recalculate_resources", fmt.Sprintf(
		`Recalculated resources for host "%s": CPU %d→%d, Memory %d→%d, Disk %d→%d, Ports %d→%d`,
		host.Name, host.CPUUsed, used.CPUUsed, host.MemoryUsed, used.MemoryUsed,
		host.DiskUsed, used.DiskUsed, portsBefore, ports), "success", nil)

	c.JSON(http.StatusOK, gin.H{
		"message":    message,
		"hasChanges": hasChanges,
		"before":     before,
		"after":      after,
		"diff":       diff,
	})
}

// hostsSuspendHandler mirrors POST /:id/instances/suspend.
func hostsSuspendHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"instanceIds"}, []ajvFieldSpec{
		{Name: "reason", Kind: ajvKindString, MinLength: -1, MaxLength: 500},
	})
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, 100)
	if !ok {
		return
	}
	reason, _ := validated.stringValue("reason")

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instances, err := hostsInstancesMatching(ctx, hostID, instanceIDs, []string{"deleted", "suspended"}, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(instances) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有找到可封停的实例"})
		return
	}

	client, clientErr := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if clientErr != nil {
		log.Printf("获取 Incus 客户端失败: %v", clientErr)
		client = nil
	}

	results := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		if instance.Status == "running" && client != nil {
			if err := HostsIncusStopInstance(ctx, client, instance.IncusID, true); err != nil {
				log.Printf("[BatchSuspend] Failed to stop instance %d: %v", instance.ID, err)
			}
		}
		suspendedBy := user.ID
		if err := db.SuspendInstance(ctx, instance.ID, db.SuspendInstanceData{
			SuspendedBy:   &suspendedBy,
			SuspendReason: reason,
		}); err != nil {
			results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": false, "error": err.Error()})
			continue
		}
		suffix := ""
		if reason != "" {
			suffix = fmt.Sprintf(" (reason: %s)", reason)
		}
		hostsCreateLog(ctx, i64Ptr(user.ID), "instance.suspend",
			fmt.Sprintf(`Batch suspended instance "%s"%s`, instance.Name, suffix), "success", &instance.ID)
		if instance.UserID != user.ID {
			lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceSuspended, lib.EventData{
				"instanceName":  instance.Name,
				"hostName":      host.Name,
				"suspendReason": reason,
			})
		}
		results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": true})
	}

	successCount, failedCount := 0, 0
	for _, result := range results {
		if result["success"] == true {
			successCount++
		} else {
			failedCount++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"message":      "批量封停完成",
		"results":      results,
		"successCount": successCount,
		"failedCount":  failedCount,
	})
}

// hostsUnsuspendHandler mirrors POST /:id/instances/unsuspend.
func hostsUnsuspendHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	_, object, ok := hostsValidatedBody(c, []string{"instanceIds"}, []ajvFieldSpec{})
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, 100)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instances, err := hostsInstancesMatching(ctx, hostID, instanceIDs, nil, []string{"suspended"})
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(instances) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有找到可解封的实例"})
		return
	}

	results := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		if err := db.UnsuspendInstance(ctx, instance.ID); err != nil {
			results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": false, "error": err.Error()})
			continue
		}
		hostsCreateLog(ctx, i64Ptr(user.ID), "instance.unsuspend",
			fmt.Sprintf(`Batch unsuspended instance "%s"`, instance.Name), "success", &instance.ID)
		if instance.UserID != user.ID {
			lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceUnsuspended, lib.EventData{
				"instanceName": instance.Name,
				"hostName":     host.Name,
			})
		}
		results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": true})
	}

	successCount, failedCount := 0, 0
	for _, result := range results {
		if result["success"] == true {
			successCount++
		} else {
			failedCount++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"message":      "批量解封完成",
		"results":      results,
		"successCount": successCount,
		"failedCount":  failedCount,
	})
}

// hostsExtendAllHandler mirrors POST /:id/extend-all (admin only).
func hostsExtendAllHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, _, ok := hostsValidatedBody(c, []string{"days"}, []ajvFieldSpec{
		{Name: "days", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 365},
	})
	if !ok {
		return
	}
	daysValue, _ := hostsValidatedInt64(validated, "days")
	days := *daysValue

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}

	var paid []struct {
		ID        int32
		Name      string
		ExpiresAt *time.Time
		UserID    int32
	}
	if err := db.DB.WithContext(ctx).Table("instances").
		Select("id, name, expires_at, user_id").
		Where("host_id = ? AND package_plan_id IS NOT NULL AND status <> 'deleted' AND expires_at IS NOT NULL", hostID).
		Scan(&paid).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(paid) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"message":       "该节点下没有可延期的付费实例",
			"extendedCount": 0,
		})
		return
	}

	extendedCount := 0
	affected := map[int32]bool{}
	daysMs := days * 24 * 60 * 60 * 1000
	for _, instance := range paid {
		if instance.ExpiresAt == nil {
			continue
		}
		newExpiresAt := instance.ExpiresAt.UTC().Add(time.Duration(daysMs) * time.Millisecond)
		if err := db.DB.WithContext(ctx).Exec(
			`UPDATE instances SET expires_at = ?, updated_at = NOW() WHERE id = ?`,
			newExpiresAt, instance.ID).Error; err != nil {
			continue
		}
		affected[instance.UserID] = true
		extendedCount++
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.batch_extend",
		fmt.Sprintf(`Batch extended %d paid instances on host "%s" by %d days`, extendedCount, host.Name, days),
		"success", nil)
	for userID := range affected {
		lib.SendNotification(ctx, int64(userID), lib.EventInstancesBatchExtended, lib.EventData{
			"hostName": host.Name,
			"days":     days,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"message":       fmt.Sprintf("成功为 %d 个付费实例延期 %d 天", extendedCount, days),
		"extendedCount": extendedCount,
	})
}

// hostsPlansHandler mirrors GET /:id/plans.
func hostsPlansHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	packageIDs, err := db.GetHostPackages(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(packageIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"plans": []any{}})
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	var rows []struct {
		PlanID       int32          `gorm:"column:plan_id"`
		Name         string         `gorm:"column:plan_name"`
		Price        models.Decimal `gorm:"column:price"`
		BillingCycle int32          `gorm:"column:billing_cycle"`
		PackageID    int32          `gorm:"column:package_id"`
		PackageName  string         `gorm:"column:package_name"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT pp.id AS plan_id, pp.name AS plan_name, pp.price, pp.billing_cycle,
		        p.id AS package_id, p.name AS package_name
		 FROM packages p JOIN package_plans pp ON pp.package_id = p.id
		 WHERE p.id IN ? AND p.active = true AND pp.is_active = true AND pp.is_sold_out = false
		 ORDER BY p.id ASC, pp.sort_order ASC`, packageIDs).Scan(&rows).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	plans := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		price, _ := row.Price.Float64()
		plans = append(plans, gin.H{
			"id":           row.PlanID,
			"name":         row.Name,
			"packageId":    row.PackageID,
			"packageName":  row.PackageName,
			"price":        price,
			"billingCycle": row.BillingCycle,
		})
	}
	c.JSON(http.StatusOK, gin.H{"plans": plans})
}

// hostsMigrateHandler mirrors POST /:id/instances/migrate.
func hostsMigrateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	sourceHostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	_, object, ok := hostsValidatedBody(c, []string{"instanceIds", "targetHostId", "targetImage"}, []ajvFieldSpec{
		{Name: "targetHostId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "targetImage", Kind: ajvKindString, MinLength: 1, MaxLength: -1},
		{Name: "targetPlanId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	})
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, hostsBatchMigrateMaxInstances)
	if !ok {
		return
	}
	targetImage, _ := object["targetImage"].(string)
	targetHostID64, _ := hostsParseIntegerBody(object["targetHostId"], 1, float64(hostsMaxInt32))
	targetPlanID, _ := hostsParseOptionalIntegerBody(object["targetPlanId"], 1, float64(hostsMaxInt32))
	if targetHostID64 == 0 || targetPlanID == nil {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	targetHostID := int32(targetHostID64)

	sourceHost, ok := hostsLoadHost(c, sourceHostID)
	if !ok {
		return
	}
	targetHost, err := db.HostsGetByID(ctx, targetHostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if targetHost == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "目标节点不存在"})
		return
	}

	if user.Role == "admin" {
		role, err := hostsHostOwnerRole(ctx, targetHostID)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if role != "admin" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "管理员迁移目标节点只能选择自营节点"})
			return
		}
	} else {
		if sourceHost.UserID != user.ID {
			hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
		if targetHost.UserID != user.ID {
			c.JSON(http.StatusForbidden, gin.H{"error": "目标节点不属于当前用户"})
			return
		}
	}
	if sourceHostID == targetHostID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源节点和目标节点不能相同"})
		return
	}
	if targetHost.Status != "online" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标节点不在线"})
		return
	}
	if valid, err := db.IsValidSystemImage(ctx, targetImage); err != nil {
		hostsRespondInternal(c)
		return
	} else if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标系统镜像不存在或已隐藏"})
		return
	}

	instances, err := hostsInstancesMatching(ctx, sourceHostID, instanceIDs, []string{"deleted"}, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(instances) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有找到可迁移的实例"})
		return
	}
	if len(instances) != len(instanceIDs) {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Some instances are not available on this host")
		return
	}

	paidCount := 0
	for _, instance := range instances {
		if instance.PackagePlanID != nil {
			paidCount++
		}
	}
	if paidCount > 0 {
		if targetPlanID == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "有付费实例需要迁移，请选择目标方案"})
			return
		}
		plan, err := db.GetPackagePlanById(ctx, int32(*targetPlanID))
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if plan == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "目标方案不存在"})
			return
		}
		if !plan.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "目标方案已下架"})
			return
		}
		if plan.IsSoldOut {
			c.JSON(http.StatusBadRequest, gin.H{"error": "目标方案已售罄", "code": "PLAN_SOLD_OUT"})
			return
		}
		packageHostIDs, err := db.GetPackageHostIds(ctx, plan.PackageID)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		bound := false
		for _, id := range packageHostIDs {
			if id == targetHostID {
				bound = true
			}
		}
		if !bound {
			c.JSON(http.StatusBadRequest, gin.H{"error": "目标方案的套餐未绑定目标节点"})
			return
		}
	}

	// 执行段依赖 lib/incus + incus-config-generator（未移植）——显式失败。
	plan := HostsMigratePlan{
		SourceHost:   sourceHost,
		TargetHost:   targetHost,
		TargetImage:  targetImage,
		TargetPlanID: targetPlanID32(targetPlanID),
		OperatorID:   user.ID,
		OperatorRole: user.Role,
	}
	results := make([]gin.H, 0, len(instances))
	failed := 0
	for _, instance := range instances {
		plan.Instance = instance
		if _, err := HostsMigrateExecutor(ctx, plan); err != nil {
			hostsRespondSeam(c, err)
			return
		}
	}
	_ = failed
	c.JSON(http.StatusOK, gin.H{
		"message":      fmt.Sprintf("成功迁移 %d 个实例", len(results)),
		"results":      results,
		"successCount": len(results),
		"failedCount":  failed,
	})
}

func targetPlanID32(value *int64) *int32 {
	if value == nil {
		return nil
	}
	converted := int32(*value)
	return &converted
}

// hostsHostOwnerRole mirrors `select: { user: { select: { role: true } } }`.
func hostsHostOwnerRole(ctx context.Context, hostID int32) (string, error) {
	if db.DB == nil {
		return "", errors.New("hosts: database not initialized")
	}
	var role string
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT u.role FROM hosts h JOIN users u ON u.id = h.user_id WHERE h.id = ?`, hostID).Scan(&role).Error; err != nil {
		return "", err
	}
	return role, nil
}

// hostsUsersLookupHandler mirrors GET /:id/users/lookup.
func hostsUsersLookupHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	username := strings.TrimSpace(c.Query("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Username is required"})
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	target, err := hostsFindUserByUsername(ctx, username)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if target == nil {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}
	keys, err := db.GetSSHKeysByUserID(ctx, target.ID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user": gin.H{
			"id":        target.ID,
			"username":  target.Username,
			"status":    target.Status,
			"hasSshKey": len(keys) > 0,
		},
	})
}

// hostsCreateForUserHandler mirrors POST /:id/instances/create-for-user.
func hostsCreateForUserHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, _, ok := hostsValidatedBody(c, []string{"username", "name", "packageId", "image"}, []ajvFieldSpec{
		{Name: "username", Kind: ajvKindString, MinLength: 2, MaxLength: 32},
		{Name: "name", Kind: ajvKindString, MinLength: 2, MaxLength: 64},
		{Name: "packageId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		{Name: "planId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		{Name: "giftDays", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 365},
		{Name: "image", Kind: ajvKindString, MinLength: 1, MaxLength: -1},
		{Name: "cpu", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 15, Maximum: 10000},
		{Name: "memory", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 128, Maximum: 524288},
		{Name: "disk", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 512, Maximum: 104857600},
	})
	if !ok {
		return
	}

	username, _ := validated.stringValue("username")
	name, _ := validated.stringValue("name")
	image, _ := validated.stringValue("image")
	packageIDValue, _ := hostsValidatedInt64(validated, "packageId")
	packageID := int32(*packageIDValue)
	planIDValue, _ := hostsValidatedInt64(validated, "planId")
	giftDaysValue, _ := hostsValidatedInt64(validated, "giftDays")
	giftDays := int64(30)
	if giftDaysValue != nil {
		giftDays = *giftDaysValue
	}
	cpuValue, _ := hostsValidatedInt64(validated, "cpu")
	memoryValue, _ := hostsValidatedInt64(validated, "memory")
	diskValue, _ := hostsValidatedInt64(validated, "disk")

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	isAdmin := user.Role == "admin"
	if host.UserID != user.ID && !isAdmin {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	nameValidation := lib.ValidateName(name, "Instance name", 2, 64)
	if !nameValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": nameValidation.Message})
		return
	}
	if nameValidation.Sanitized != nil {
		name = *nameValidation.Sanitized
	}

	target, err := hostsFindUserByUsername(ctx, strings.TrimSpace(username))
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if target == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf(`用户 "%s" 不存在`, username)})
		return
	}
	if target.ID == user.ID && !isAdmin {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能给自己赠送实例，请使用当前节点的普通创建功能"})
		return
	}
	if target.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf(`用户 "%s" 状态异常，无法创建实例`, username)})
		return
	}
	targetKeys, err := db.GetSSHKeysByUserID(ctx, target.ID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(targetKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf(`用户 "%s" 没有 SSH 密钥，请先让用户添加 SSH 密钥`, username)})
		return
	}

	pkg, err := hostsLoadPackageRow(ctx, packageID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if pkg == nil || !pkg.Active {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐不存在或已禁用"})
		return
	}
	if pkg.UserID != user.ID && !isAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "只能使用您自己的套餐为其他用户创建实例"})
		return
	}
	if len(pkg.HostIDs) > 0 {
		bound := false
		for _, id := range pkg.HostIDs {
			if id == hostID {
				bound = true
			}
		}
		if !bound {
			c.JSON(http.StatusBadRequest, gin.H{"error": "该套餐未绑定到当前节点"})
			return
		}
	} else if host.UserID != pkg.UserID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未绑定节点的套餐只能在套餐所有者的节点上创建实例"})
		return
	}

	var plan *models.PackagePlan
	if planIDValue != nil {
		plan, err = db.GetPackagePlanById(ctx, int32(*planIDValue))
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if plan == nil || plan.PackageID != packageID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案不存在或不属于该套餐"})
			return
		}
		if !plan.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案已下架"})
			return
		}
		if plan.IsSoldOut {
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案已售罄", "code": "PLAN_SOLD_OUT"})
			return
		}
	}

	requestedCPU := int64(15)
	requestedMemory := int64(128)
	requestedDisk := int64(512)
	if plan != nil {
		requestedCPU = int64(plan.CPU)
		requestedMemory = int64(plan.Memory)
		requestedDisk = int64(plan.Disk)
	} else {
		if cpuValue != nil {
			requestedCPU = *cpuValue
		}
		if memoryValue != nil {
			requestedMemory = *memoryValue
		}
		if diskValue != nil {
			requestedDisk = *diskValue
		}
	}

	if valid, err := db.IsValidSystemImage(ctx, image); err != nil {
		hostsRespondInternal(c)
		return
	} else if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像不存在"})
		return
	}
	pkgInstanceType := pkg.InstanceType
	if pkgInstanceType == "" {
		pkgInstanceType = "container"
	}
	if compatible, err := db.IsImageCompatibleWithInstanceType(ctx, image, pkgInstanceType); err != nil {
		hostsRespondInternal(c)
		return
	} else if !compatible {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像类型与套餐类型不兼容"})
		return
	}
	if compatible, err := db.IsImageCompatibleWithMemory(ctx, image, int(requestedMemory)); err != nil {
		hostsRespondInternal(c)
		return
	} else if !compatible {
		c.JSON(http.StatusBadRequest, gin.H{"error": "128MB 内存只能使用 Alpine/Debian 镜像"})
		return
	}

	networkMode := string(lib.NormalizeNetworkMode(pkg.NetworkMode))
	if pkgInstanceType == "vm" && (networkMode == "nat_ipv6_nat" || networkMode == "ipv6_nat") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "KVM packages do not support IPv4 NAT & IPv6 NAT or IPv6 NAT network modes"})
		return
	}

	// 执行段依赖 db.selectAvailableHost / selectAndReserveHostWithLock 的事务包装、
	// lib/incus + lib/managed-instance-provision.ts + GPU 预留 —— 未移植，显式失败。
	_ = giftDays
	_ = requestedCPU
	_ = requestedDisk
	_ = target
	hostsRespondSeam(c, errHostsSeam("lib/managed-instance-provision.ts#provisionManagedInstanceAsync"))
}

// hostsPackageRow is the subset of the package row the host routes read.
type hostsPackageRow struct {
	ID           int32
	Name         string
	UserID       int32
	Active       bool
	InstanceType string
	NetworkMode  string
	HostIDs      []int32
}

// hostsLoadPackageRow reads the package row and its host bindings.
func hostsLoadPackageRow(ctx context.Context, packageID int32) (*hostsPackageRow, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var row struct {
		ID           int32
		Name         string
		UserID       int32
		Active       bool
		InstanceType *string
		NetworkMode  *string
	}
	result := db.DB.WithContext(ctx).Table("packages").
		Select("id, name, user_id, active, instance_type, network_mode").
		Where("id = ?", packageID).Limit(1).Scan(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	hostIDs, err := db.GetPackageHostIds(ctx, packageID)
	if err != nil {
		return nil, err
	}
	pkg := &hostsPackageRow{
		ID:           row.ID,
		Name:         row.Name,
		UserID:       row.UserID,
		Active:       row.Active,
		InstanceType: hostsDerefString(row.InstanceType),
		NetworkMode:  hostsDerefString(row.NetworkMode),
		HostIDs:      hostIDs,
	}
	return pkg, nil
}

// hostsGiftDaysHandler mirrors POST /:id/instances/gift-days.
func hostsGiftDaysHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	_, object, ok := hostsValidatedBody(c, []string{"instanceIds", "days"}, []ajvFieldSpec{
		{Name: "days", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 365},
	})
	if !ok {
		return
	}
	instanceIDs, _, ok := hostsValidateIntegerArray(c, object, "instanceIds", true, 1, hostsBatchInstanceMaxItems)
	if !ok {
		return
	}
	days, ok := hostsParseIntegerBody(object["days"], 1, hostsGiftDaysMax)
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	// 权限检查：只有节点所有者可以赠送时长
	if !hostsOwnerOnly(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}

	var instances []models.Instance
	if err := db.DB.WithContext(ctx).
		Where("host_id = ? AND id IN ? AND status <> 'deleted' AND package_plan_id IS NOT NULL", hostID, instanceIDs).
		Find(&instances).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	if len(instances) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有找到可赠送时长的付费实例"})
		return
	}

	now := time.Now().UTC()
	results := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		base := now
		if instance.ExpiresAt != nil && instance.ExpiresAt.After(now) {
			base = instance.ExpiresAt.UTC()
		}
		newExpiresAt := base.Add(time.Duration(days) * 24 * time.Hour)
		status := string(instance.Status)
		clearSuspension := false
		if instance.Status == "suspended" && instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			status = "stopped"
			clearSuspension = true
		}
		query := `UPDATE instances SET expires_at = ?, status = ?, updated_at = NOW() WHERE id = ?`
		args := []any{newExpiresAt, status, instance.ID}
		if clearSuspension {
			query = `UPDATE instances SET expires_at = ?, status = 'stopped', suspended_at = NULL,
				suspended_by = NULL, suspend_reason = NULL, updated_at = NOW() WHERE id = ?`
			args = []any{newExpiresAt, instance.ID}
		}
		if err := db.DB.WithContext(ctx).Exec(query, args...).Error; err != nil {
			results = append(results, gin.H{
				"instanceId": instance.ID, "instanceName": instance.Name,
				"success": false, "error": err.Error(),
			})
			continue
		}
		results = append(results, gin.H{
			"instanceId":   instance.ID,
			"instanceName": instance.Name,
			"success":      true,
			"newExpiresAt": newExpiresAt.Format("2006-01-02T15:04:05.000Z"),
		})
	}

	successCount, failedCount := 0, 0
	for _, result := range results {
		if result["success"] == true {
			successCount++
		} else {
			failedCount++
		}
	}
	message := fmt.Sprintf("成功为 %d 个实例赠送 %d 天", successCount, days)
	if failedCount > 0 {
		message = fmt.Sprintf("成功为 %d 个实例赠送 %d 天，%d 个失败", successCount, days, failedCount)
	}
	c.JSON(http.StatusOK, gin.H{
		"message":      message,
		"results":      results,
		"successCount": successCount,
		"failedCount":  failedCount,
		"skippedCount": len(instanceIDs) - len(instances),
	})
}

// hostsRenewalPriceHandler mirrors PATCH /:id/instances/:instanceId/renewal-price.
func hostsRenewalPriceHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	instanceID, ok := hostsParsePositiveRouteID(c.Param("instanceId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	object, bodyOK := hostsDecodedBody(c)
	if !bodyOK {
		return
	}
	objectMap, message := hostsBodyObject(object)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	rawPrice, present := objectMap["newPrice"]
	if !present {
		respondAJVValidation(c, "newPrice is required")
		return
	}
	if _, isNumber := hostsJSONNumber(rawPrice); !isNumber {
		respondAJVValidation(c, "must be number")
		return
	}
	newPrice, ok := hostsParseFiniteNumberBody(rawPrice, 0, hostsRenewalPriceMax)
	if !ok {
		if number, isNumber := hostsJSONNumber(rawPrice); isNumber {
			if number > hostsRenewalPriceMax {
				respondAJVValidation(c, "must be <= 99999")
			} else {
				respondAJVValidation(c, "must be >= 0")
			}
			return
		}
		respondAJVValidation(c, "must be number")
		return
	}

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOnly(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instance, err := hostsGetInstanceByID(ctx, instanceID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if instance == nil {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.HostID != hostID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例不属于该节点"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只能修改付费实例的续费价格"})
		return
	}
	if instance.Status == "deleted" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	oldPrice := 0.0
	if instance.BillingPrice != nil {
		oldPrice, _ = instance.BillingPrice.Float64()
	}
	if math.Abs(oldPrice-newPrice) < 0.001 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新价格与原价格相同，无需修改"})
		return
	}

	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	if err := db.DB.WithContext(ctx).Exec(
		`UPDATE instances SET billing_price = ?, updated_at = NOW() WHERE id = ?`, newPrice, instanceID).Error; err != nil {
		hostsRespondInternal(c)
		return
	}

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceRenewalPriceUpdated, lib.EventData{
		"instanceName": instance.Name,
		"oldPrice":     oldPrice,
		"newPrice":     newPrice,
		"hostName":     host.Name,
	})
	if target, err := hostsUserEmailByID(ctx, instance.UserID); err == nil && target != nil && *target != "" {
		lib.SendRenewalPriceUpdatedEmail(ctx, *target, lib.RenewalPriceUpdatedEmailData{
			Username:     hostsUsernameOrFallback(ctx, instance.UserID),
			InstanceName: instance.Name,
			HostName:     host.Name,
			OldPrice:     oldPrice,
			NewPrice:     newPrice,
		})
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.update_instance_renewal_price", fmt.Sprintf(
		`Updated renewal price for instance "%s" (ID: %d): ¥%.2f -> ¥%.2f`,
		instance.Name, instanceID, oldPrice, newPrice), "success", nil)

	c.JSON(http.StatusOK, gin.H{
		"message":      "续费价格已更新",
		"instanceId":   instanceID,
		"instanceName": instance.Name,
		"oldPrice":     oldPrice,
		"newPrice":     newPrice,
	})
}

func hostsUserEmailByID(ctx context.Context, userID int32) (*string, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var email *string
	result := db.DB.WithContext(ctx).Table("users").Select("email").Where("id = ?", userID).Limit(1).Scan(&email)
	if result.Error != nil {
		return nil, result.Error
	}
	return email, nil
}

func hostsUsernameOrFallback(ctx context.Context, userID int32) string {
	username, err := hostsUsernameByID(ctx, userID)
	if err != nil {
		return ""
	}
	return username
}

// ==================== F. ops 与审计 ====================

// hostsOpsDiscoverHandler mirrors POST /:id/ops/discover.
func hostsOpsDiscoverHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	incusInstances, err := hostsListInstances(ctx, client)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	dbInstances, err := hostsInstancesOnHost(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	type incusEntry struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	incusByName := map[string]incusEntry{}
	managed := []gin.H{}
	orphaned := []gin.H{}
	for _, item := range incusInstances {
		entry := incusEntry{}
		if value, ok := item["name"].(string); ok {
			entry.Name = value
		}
		if value, ok := item["type"].(string); ok {
			entry.Type = value
		}
		if value, ok := item["status"].(string); ok {
			entry.Status = value
		}
		incusByName[entry.Name] = entry
	}
	byIncusID := map[string]models.Instance{}
	for _, instance := range dbInstances {
		byIncusID[instance.IncusID] = instance
	}
	for _, entry := range incusByName {
		if dbInstance, hit := byIncusID[entry.Name]; hit {
			managed = append(managed, gin.H{
				"incusName":   entry.Name,
				"incusType":   entry.Type,
				"incusStatus": entry.Status,
				"dbId":        dbInstance.ID,
				"dbStatus":    string(dbInstance.Status),
				"userId":      dbInstance.UserID,
			})
		} else {
			orphaned = append(orphaned, gin.H{
				"incusName":   entry.Name,
				"incusType":   entry.Type,
				"incusStatus": entry.Status,
			})
		}
	}
	missing := []gin.H{}
	for _, instance := range dbInstances {
		if _, hit := incusByName[instance.IncusID]; !hit {
			missing = append(missing, gin.H{
				"dbId":     instance.ID,
				"dbName":   instance.Name,
				"incusId":  instance.IncusID,
				"dbStatus": string(instance.Status),
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"managed":  managed,
		"orphaned": orphaned,
		"missing":  missing,
		"summary": gin.H{
			"totalIncus":    len(incusInstances),
			"totalDb":       len(dbInstances),
			"managedCount":  len(managed),
			"orphanedCount": len(orphaned),
			"missingCount":  len(missing),
		},
	})
}

// hostsOpsOptimizePortMappingsHandler mirrors POST /:id/ops/optimize-port-mappings.
func hostsOpsOptimizePortMappingsHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	bindable := lib.SelectBindableIpv4ListenAddress(host.NATBindIP, host.NATPublicIP, &host.URL, host.IPAddress)
	if bindable == "" || bindable == "0.0.0.0" {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			"该节点没有可用于监听的具体 IPv4 地址（请先在节点设置里补充 NAT 绑定 IP），内核 NAT 需要具体监听地址")
		return
	}
	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = client
	hostsRespondSeam(c, errHostsSeam("ops/optimize-port-mappings 的 Incus 设备改写段"))
}

// hostsOpsBaselineSyncHandler mirrors POST /:id/ops/baseline-sync.
func hostsOpsBaselineSyncHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	lib.TunnelManagerInstance().RequestHeartbeat(hostID)
	used, err := db.HostsCalculateResourcesFromInstances(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = db.HostsUpdateResources(ctx, hostID, db.HostsResourceUpdate{
		CPUUsed:    &used.CPUUsed,
		MemoryUsed: &used.MemoryUsed,
		DiskUsed:   &used.DiskUsed,
	}, nil)

	instances, err := hostsInstancesMatching(ctx, hostID, hostsAllInstanceIDs(), nil, []string{"running"})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	synced, ipChanged := 0, 0
	for _, instance := range instances {
		stateRaw, stateErr := hostsGetInstanceState(ctx, client, instance.IncusID)
		if stateErr != nil {
			log.Printf("[ops/baseline-sync] instance %s sync failed: %v", instance.IncusID, stateErr)
			continue
		}
		instanceRaw, instanceErr := hostsIncusRequest(ctx, client, "/1.0/instances/"+instance.IncusID)
		if instanceErr != nil {
			log.Printf("[ops/baseline-sync] instance %s sync failed: %v", instance.IncusID, instanceErr)
			continue
		}
		var state struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(stateRaw, &state)
		if state.Status != "" {
			mapped := hostsMapInstanceStatus(state.Status)
			if mapped != "unknown" && mapped != string(instance.Status) {
				_ = db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatus(mapped), nil)
			}
		}
		delta, err := HostsSnapshotSyncApplier(ctx, instance, instanceRaw, stateRaw)
		if err != nil {
			log.Printf("[ops/baseline-sync] instance %s sync failed: %v", instance.IncusID, err)
			continue
		}
		if delta.IPv4Changed || delta.IPv6Changed {
			ipChanged++
		}
		synced++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Baseline sync completed",
		"resources": gin.H{
			"cpuUsed":    used.CPUUsed,
			"memoryUsed": used.MemoryUsed,
			"diskUsed":   used.DiskUsed,
		},
		"instanceSync": gin.H{
			"total":     len(instances),
			"synced":    synced,
			"ipChanged": ipChanged,
		},
	})
}

// hostsAllInstanceIDs is a convenience for the "all running instances" queries.
func hostsAllInstanceIDs() []int32 { return nil }

// hostsOpsNetworkRepairHandler mirrors POST /:id/ops/network-repair.
func hostsOpsNetworkRepairHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法连接到节点: " + err.Error()})
		return
	}
	instances, err := hostsInstancesMatching(ctx, hostID, hostsAllInstanceIDs(), []string{"deleted", "creating"}, nil)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	results := make([]gin.H, 0, len(instances))
	successCount, changedCount := 0, 0
	for _, instance := range instances {
		stateRaw, stateErr := hostsGetInstanceState(ctx, client, instance.IncusID)
		if stateErr != nil {
			results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": false, "error": stateErr.Error()})
			continue
		}
		instanceRaw, instanceErr := hostsIncusRequest(ctx, client, "/1.0/instances/"+instance.IncusID)
		if instanceErr != nil {
			results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": false, "error": instanceErr.Error()})
			continue
		}
		var state struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(stateRaw, &state)
		statusChanged := false
		previous := string(instance.Status)
		newStatus := previous
		if state.Status != "" {
			mapped := hostsMapInstanceStatus(state.Status)
			if mapped != "unknown" && mapped != previous {
				_ = db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatus(mapped), nil)
				statusChanged = true
				newStatus = mapped
				changedCount++
			}
		}
		delta, err := HostsSnapshotSyncApplier(ctx, instance, instanceRaw, stateRaw)
		if err != nil {
			results = append(results, gin.H{"id": instance.ID, "name": instance.Name, "success": false, "error": err.Error()})
			continue
		}
		if delta.IPv4Changed || delta.IPv6Changed {
			changedCount++
		}
		entry := gin.H{"id": instance.ID, "name": instance.Name, "success": true}
		if statusChanged {
			entry["statusChanged"] = true
			entry["oldStatus"] = previous
			entry["newStatus"] = newStatus
		}
		if delta.IPv4Changed {
			entry["ipv4Changed"] = true
			entry["oldIpv4"] = delta.OldIPv4
			entry["newIpv4"] = delta.NewIPv4
		}
		if delta.IPv6Changed {
			entry["ipv6Changed"] = true
			entry["oldIpv6"] = delta.OldIPv6
			entry["newIpv6"] = delta.NewIPv6
		}
		results = append(results, entry)
		successCount++
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "Network repair completed",
		"results": results,
		"summary": gin.H{
			"total":   len(instances),
			"success": successCount,
			"failed":  len(instances) - successCount,
			"changed": changedCount,
		},
	})
}

// hostsOpsInstance loads `:instanceId` and enforces ownership + host membership.
func hostsOpsInstance(c *gin.Context, hostID int32) (*models.Instance, bool) {
	instanceID, ok := hostsParsePositiveRouteID(c.Param("instanceId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return nil, false
	}
	instance, err := hostsGetInstanceByID(c.Request.Context(), instanceID)
	if err != nil {
		hostsRespondInternal(c)
		return nil, false
	}
	if instance == nil {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return nil, false
	}
	if instance.HostID != hostID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例不属于该节点"})
		return nil, false
	}
	return instance, true
}

// hostsOpsInstancePreviewHandler mirrors POST
// /:id/ops/instances/:instanceId/preview.
func hostsOpsInstancePreviewHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	activeTask, err := db.GetActiveTaskForInstance(ctx, instance.ID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	isStopped := instance.Status == "stopped"
	isSuspended := instance.Status == "suspended"
	canRestart := activeTask == nil && !isSuspended
	notes := []string{}
	if activeTask != nil {
		notes = append(notes, fmt.Sprintf("当前存在进行中的 %s 任务 #%d", activeTask.TaskType, activeTask.ID))
	}
	if isSuspended {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			notes = append(notes, "实例处于到期封停状态，不允许高风险操作")
		} else {
			notes = append(notes, "实例处于封停状态，不允许高风险操作")
		}
	}
	if !isStopped {
		notes = append(notes, "实例当前不是 stopped，高风险重装需先停机")
	}
	suggestedAction := "none"
	if activeTask == nil && !isSuspended {
		if !isStopped {
			suggestedAction = "sync"
		} else {
			suggestedAction = "rebuild"
		}
	}
	var taskPayload any
	if activeTask != nil {
		taskPayload = gin.H{
			"id":       activeTask.ID,
			"taskType": activeTask.TaskType,
			"status":   activeTask.Status,
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"instanceId":      instance.ID,
		"instanceName":    instance.Name,
		"incusId":         instance.IncusID,
		"instanceStatus":  string(instance.Status),
		"hostName":        host.Name,
		"hostId":          host.ID,
		"imageAlias":      hostsNilIfEmpty(instance.Image),
		"canSync":         activeTask == nil,
		"canRestart":      canRestart,
		"canForceRestart": canRestart,
		"canRebuild":      activeTask == nil && !isSuspended && isStopped,
		"canRecreate":     activeTask == nil && !isSuspended,
		"activeTask":      taskPayload,
		"risk": gin.H{
			"status":          string(instance.Status),
			"isStopped":       isStopped,
			"hasActiveTask":   activeTask != nil,
			"suggestedAction": suggestedAction,
			"notes":           notes,
		},
	})
}

// hostsOpsInstanceSSHKeysHandler mirrors GET /:id/ops/instances/:instanceId/ssh-keys.
func hostsOpsInstanceSSHKeysHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	keys, err := db.GetSSHKeysByUserID(ctx, instance.UserID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	items := make([]gin.H, 0, len(keys))
	for _, key := range keys {
		items = append(items, gin.H{
			"id":          key.ID,
			"name":        key.Name,
			"fingerprint": key.Fingerprint,
			"created_at":  key.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"keys": items})
}

// hostsOpsInstanceInitCommandsHandler mirrors GET
// /:id/ops/instances/:instanceId/init-commands.
func hostsOpsInstanceInitCommandsHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	distro := strings.TrimSpace(c.DefaultQuery("distro", "linux"))
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	commands, err := db.GetEnabledCommandsByDistro(ctx, instance.UserID, distro)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	items := make([]gin.H, 0, len(commands))
	for _, command := range commands {
		count := 0
		for _, line := range regexp.MustCompile(`\r?\n`).Split(command.Command, -1) {
			if strings.TrimSpace(line) != "" {
				count++
			}
		}
		items = append(items, gin.H{
			"id":               command.ID,
			"name":             command.Name,
			"commandLineCount": count,
			"distros":          command.Distros,
			"description":      command.Description,
		})
	}
	c.JSON(http.StatusOK, gin.H{"commands": items})
}

// ==================== F. 审计规则 ====================

// hostsAuditRule mirrors AuditRuleDefinition.
type hostsAuditRule struct {
	ID             string
	Severity       string
	Category       string
	Name           string
	Description    *string
	TargetTypes    []string
	MatchType      string
	PatternText    string
	CaseSensitive  bool
	Source         string
	Enabled        bool
	Recommendation *string
}

// hostsBuiltinAuditRules mirrors BUILTIN_AUDIT_RULES (lib/instance-audit.ts).
var hostsBuiltinAuditRules = []hostsAuditRule{
	{
		ID: "packet-tool-hping", Severity: "high", Category: "network-abuse",
		Name: "疑似发包工具进程", Description: strPtrHosts("匹配常见的发包、构造数据包或压测类工具。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(hping[3]?|nping|mz|mausezahn|mzbench|pktgen|packit|nemesis|tcpreplay|scapy|flood|synflood|udpflood|icmpflood|httpflood|slowloris|goldeneye|hulk|xerxes)\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("请先确认进程归属、流量用途和客户业务背景，再决定是否手动处置。"),
	},
	{
		ID: "mass-scan-tool", Severity: "high", Category: "network-abuse",
		Name: "疑似大规模扫描工具", Description: strPtrHosts("匹配常见的公网大范围扫描或高频扫描工具。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(masscan|zmap|zdns|zgrab2?|naabu|rustscan|httpx|nuclei|fscan|kscan|gogo|goscan|scanrs|nmap)\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("请核实扫描是否经过授权，并结合网络连接、工单或客户说明判断。"),
	},
	{
		ID: "flood-tool-name", Severity: "high", Category: "network-abuse",
		Name: "疑似 Flood/DDoS 程序", Description: strPtrHosts("匹配常见 Flood、DDoS 工具名称或命令名称。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(syn[-_]?flood|udp[-_]?flood|http[-_]?flood|slowloris|goldeneye|hulk|xerxes|torshammer|ufonet|mhddos|ddosim|layer[47]|flooder|cc[-_]?attack|stress|hammer|python.*(ddos|flood|attack|hammer)|node.*(ddos|flood|attack))\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("建议按高风险处理。手动停止前请先保留进程参数和连接证据。"),
	},
	{
		ID: "proxy-panel", Severity: "medium", Category: "proxy-panel",
		Name: "疑似代理面板程序", Description: strPtrHosts("匹配常见代理面板或相关管理面板名称。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(3x-ui|x-ui|v2-ui|s-ui|h-ui|marzban|v2board|sspanel|xboard|trojan-panel|hiddify|nebula|alpine|sub-web|subconverter|bt-panel|aapanel|aa-panel)\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("请确认当前节点规则是否允许此类应用，并结合客户业务说明判断。"),
	},
	{
		ID: "proxy-core", Severity: "medium", Category: "proxy-panel",
		Name: "疑似代理核心进程", Description: strPtrHosts("匹配常见代理核心、转发核心或隧道类进程名称。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(xray|v2ray|sing-?box|trojan(-go|rs)?|hysteria2?|naive|brook|gost|ss-server|ss-rust|shadowsocks(-rust)?|tuic|juicity|clash(-meta)?|mihomo|reality|xtls|vless|vmess|frp[cs]?|nps|npc|chisel|ligolo(-ng)?|rathole|bore)\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("处置前请核对客户用途、节点政策和实际流量，避免误伤正常服务。"),
	},
	{
		ID: "miner", Severity: "high", Category: "resource-abuse",
		Name: "疑似挖矿进程", Description: strPtrHosts("匹配常见加密货币挖矿进程名称。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(xmrig|xmrig-proxy|xmr-stak|xmrigcc|cpuminer|minerd|ethminer|lolminer|nbminer|teamredminer|t-rex|gminer|bzminer|srbminer|wildrig|rigel|miniZ|SRBMiner|moneroocean)\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("请结合 CPU 占用、运行时间和服务条款人工确认，违规后再执行处置。"),
	},
	{
		ID: "bruteforce", Severity: "medium", Category: "credential-attack",
		Name: "疑似爆破/撞库工具", Description: strPtrHosts("匹配常见密码爆破、登录尝试或凭据攻击工具。"),
		TargetTypes: []string{"process", "startup"}, MatchType: "regex",
		PatternText: `\b(hydra|thc-hydra|medusa|ncrack|patator|crowbar|brutespray|ffuf|gobuster|dirsearch|dirb|sshpass|brute)\b`,
		Source:      "builtin", Enabled: true,
		Recommendation: strPtrHosts("请检查外连目标、登录尝试记录和客户说明，再决定是否手动处理。"),
	},
}

var hostsAuditSeverities = map[string]bool{"info": true, "low": true, "medium": true, "high": true}
var hostsAuditTargets = map[string]bool{"process": true, "network": true, "startup": true}
var hostsAuditMatchTypes = map[string]bool{"contains": true, "regex": true, "exact": true}
var hostsAuditFindingTargets = map[string]bool{"process": true, "network": true, "startup": true, "capability": true}

func hostsNormalizeAuditSeverity(value any) string {
	if text, ok := value.(string); ok && hostsAuditSeverities[text] {
		return text
	}
	return "medium"
}

func hostsNormalizeAuditMatchType(value any) string {
	if text, ok := value.(string); ok && hostsAuditMatchTypes[text] {
		return text
	}
	return "contains"
}

func hostsNormalizeAuditTargetTypes(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{"process"}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, item := range items {
		text, ok := item.(string)
		if !ok || !hostsAuditTargets[text] || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	if len(out) == 0 {
		return []string{"process"}
	}
	return out
}

func hostsNormalizeAuditFindingTarget(value any) *string {
	text, ok := value.(string)
	if !ok || !hostsAuditFindingTargets[text] {
		return nil
	}
	return &text
}

func hostsValidateAuditRegex(matchType, pattern string) error {
	if matchType != "regex" {
		return nil
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return err
	}
	return nil
}

// hostsApplyBuiltinAuditOverride mirrors applyBuiltinAuditRuleOverride.
func hostsApplyBuiltinAuditOverride(rule hostsAuditRule, override *models.InstanceAuditBuiltinRuleOverride) hostsAuditRule {
	if override == nil {
		return rule
	}
	applied := rule
	applied.Severity = hostsNormalizeAuditSeverity(override.Severity)
	if override.Category != "" {
		applied.Category = override.Category
	}
	if override.Name != "" {
		applied.Name = override.Name
	}
	applied.Description = override.Description
	applied.TargetTypes = hostsNormalizeAuditTargetTypes(hostsJSONArray(override.TargetTypes))
	applied.MatchType = hostsNormalizeAuditMatchType(override.MatchType)
	if override.Pattern != "" {
		applied.PatternText = override.Pattern
	}
	applied.CaseSensitive = override.CaseSensitive
	applied.Enabled = override.Enabled
	applied.Recommendation = override.Recommendation
	return applied
}

func hostsJSONArray(raw []byte) []any {
	var out []any
	if len(raw) == 0 {
		return nil
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// hostsBuiltinAuditRuleResponse mirrors toAuditRuleResponse for a builtin rule.
func hostsBuiltinAuditRuleResponse(rule, original hostsAuditRule, override *models.InstanceAuditBuiltinRuleOverride) gin.H {
	payload := hostsAuditRulePublic(rule)
	payload["pattern"] = rule.PatternText
	payload["readOnly"] = true
	payload["scope"] = "builtin"
	payload["originalName"] = original.Name
	payload["originalSeverity"] = original.Severity
	payload["originalCategory"] = original.Category
	payload["originalTargetTypes"] = original.TargetTypes
	payload["originalMatchType"] = original.MatchType
	payload["originalPattern"] = original.PatternText
	payload["originalCaseSensitive"] = original.CaseSensitive
	payload["originalRecommendation"] = original.Recommendation
	payload["overridden"] = override != nil
	if override != nil {
		payload["overrideId"] = override.ID
		payload["overrideUpdatedAt"] = override.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	} else {
		payload["overrideId"] = nil
		payload["overrideUpdatedAt"] = nil
	}
	return payload
}

// hostsAuditRulePublic is the shared scalar projection of AuditRuleDefinition.
func hostsAuditRulePublic(rule hostsAuditRule) gin.H {
	return gin.H{
		"id":             rule.ID,
		"severity":       rule.Severity,
		"category":       rule.Category,
		"name":           rule.Name,
		"description":    rule.Description,
		"targetTypes":    rule.TargetTypes,
		"matchType":      rule.MatchType,
		"caseSensitive":  rule.CaseSensitive,
		"source":         rule.Source,
		"enabled":        rule.Enabled,
		"recommendation": rule.Recommendation,
	}
}

// hostsCustomAuditRuleResponse mirrors toAuditRuleResponse for a custom rule.
func hostsCustomAuditRuleResponse(rule models.InstanceAuditRule, hostID int32) gin.H {
	scope := "global"
	if rule.HostID != nil {
		if *rule.HostID == hostID {
			scope = "host"
		} else {
			scope = "global"
		}
	}
	return gin.H{
		"id":             rule.ID,
		"ruleId":         fmt.Sprintf("custom:%d", rule.ID),
		"source":         "custom",
		"readOnly":       false,
		"scope":          scope,
		"hostId":         rule.HostID,
		"name":           rule.Name,
		"description":    rule.Description,
		"severity":       rule.Severity,
		"category":       rule.Category,
		"targetTypes":    hostsNormalizeAuditTargetTypes(hostsJSONArray(rule.TargetTypes)),
		"matchType":      rule.MatchType,
		"pattern":        rule.Pattern,
		"caseSensitive":  rule.CaseSensitive,
		"recommendation": rule.Recommendation,
		"enabled":        rule.Enabled,
		"createdAt":      rule.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"updatedAt":      rule.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// hostsLoadCustomAuditRules mirrors the instanceAuditRule.findMany query.
func hostsLoadCustomAuditRules(ctx context.Context, hostID int32, enabledOnly bool, descending bool) ([]models.InstanceAuditRule, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	order := "ASC"
	if descending {
		order = "DESC"
	}
	query := db.DB.WithContext(ctx).
		Where("host_id IS NULL OR host_id = ?", hostID)
	if enabledOnly {
		query = query.Where("enabled = true")
	}
	var rules []models.InstanceAuditRule
	if err := query.Order("host_id ASC").Order("id " + order).Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

// hostsLoadBuiltinAuditOverrides maps builtinRuleId → override.
func hostsLoadBuiltinAuditOverrides(ctx context.Context, hostID int32) (map[string]*models.InstanceAuditBuiltinRuleOverride, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var overrides []models.InstanceAuditBuiltinRuleOverride
	if err := db.DB.WithContext(ctx).Where("host_id = ?", hostID).Find(&overrides).Error; err != nil {
		return nil, err
	}
	out := map[string]*models.InstanceAuditBuiltinRuleOverride{}
	for index := range overrides {
		out[overrides[index].BuiltinRuleID] = &overrides[index]
	}
	return out, nil
}

// hostsLoadAuditIgnores mirrors getAuditIgnores.
func hostsLoadAuditIgnores(ctx context.Context, hostID, instanceID int32) ([]models.InstanceAuditIgnore, error) {
	if db.DB == nil {
		return nil, errors.New("hosts: database not initialized")
	}
	var ignores []models.InstanceAuditIgnore
	if err := db.DB.WithContext(ctx).
		Where("host_id = ? AND enabled = true AND (instance_id IS NULL OR instance_id = ?)", hostID, instanceID).
		Find(&ignores).Error; err != nil {
		return nil, err
	}
	return ignores, nil
}

// hostsAuditRulesHandler mirrors GET /:id/ops/audit/rules.
func hostsAuditRulesHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	rules, err := hostsLoadCustomAuditRules(ctx, hostID, false, true)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	overrides, err := hostsLoadBuiltinAuditOverrides(ctx, hostID)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	builtin := make([]gin.H, 0, len(hostsBuiltinAuditRules))
	for _, rule := range hostsBuiltinAuditRules {
		applied := hostsApplyBuiltinAuditOverride(rule, overrides[rule.ID])
		builtin = append(builtin, hostsBuiltinAuditRuleResponse(applied, rule, overrides[rule.ID]))
	}
	custom := make([]gin.H, 0, len(rules))
	for _, rule := range rules {
		custom = append(custom, hostsCustomAuditRuleResponse(rule, hostID))
	}
	c.JSON(http.StatusOK, gin.H{
		"builtin":         builtin,
		"custom":          custom,
		"canCreateGlobal": user.Role == "admin",
	})
}

// hostsAuditBuiltinRuleOverrideHandler mirrors PATCH
// /:id/ops/audit/builtin-rules/:ruleId.
func hostsAuditBuiltinRuleOverrideHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, nil, []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: 2, MaxLength: 80},
		{Name: "description", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 500},
		{Name: "severity", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"info", "low", "medium", "high"}},
		{Name: "category", Kind: ajvKindString, MinLength: -1, MaxLength: 64},
		{Name: "matchType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"contains", "regex", "exact"}},
		{Name: "pattern", Kind: ajvKindString, MinLength: 1, MaxLength: 500},
		{Name: "caseSensitive", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "recommendation", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 1000},
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	if _, _, ok := hostsValidateArrayOfStrings(c, object, "targetTypes", false); !ok {
		return
	}
	_ = validated
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	ruleID := c.Param("ruleId")
	base, found := hostsFindBuiltinAuditRule(ruleID)
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "系统内置规则不存在"})
		return
	}

	matchType := base.MatchType
	if text, present := validated.stringValue("matchType"); present {
		matchType = hostsNormalizeAuditMatchType(text)
	}
	pattern := base.PatternText
	if text, present := validated.stringValue("pattern"); present {
		pattern = strings.TrimSpace(text)
	}
	if err := hostsValidateAuditRegex(matchType, pattern); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "正则表达式无效"})
		return
	}

	name := base.Name
	if text, present := validated.stringValue("name"); present && strings.TrimSpace(text) != "" {
		name = strings.TrimSpace(text)
	}
	description := base.Description
	if value, present := validated.nullableString("description"); present {
		if value != nil && strings.TrimSpace(*value) != "" {
			trimmed := strings.TrimSpace(*value)
			description = &trimmed
		} else {
			description = nil
		}
	}
	severity := base.Severity
	if text, present := validated.stringValue("severity"); present {
		severity = hostsNormalizeAuditSeverity(text)
	}
	category := base.Category
	if text, present := validated.stringValue("category"); present && strings.TrimSpace(text) != "" {
		category = strings.TrimSpace(text)
	}
	targetTypes := base.TargetTypes
	if raw, present := object["targetTypes"]; present {
		targetTypes = hostsNormalizeAuditTargetTypes(raw)
	}
	caseSensitive := base.CaseSensitive
	if value, present := hostsValidatedBool(validated, "caseSensitive"); present {
		caseSensitive = value
	}
	recommendation := base.Recommendation
	if value, present := validated.nullableString("recommendation"); present {
		if value != nil && strings.TrimSpace(*value) != "" {
			trimmed := strings.TrimSpace(*value)
			recommendation = &trimmed
		} else {
			recommendation = nil
		}
	}
	enabled := base.Enabled
	if value, present := hostsValidatedBool(validated, "enabled"); present {
		enabled = value
	}

	targetTypesJSON, _ := json.Marshal(targetTypes)
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	columns := map[string]any{
		"created_by_id":  user.ID,
		"name":           name,
		"description":    description,
		"severity":       severity,
		"category":       category,
		"target_types":   datatypes.JSON(targetTypesJSON),
		"match_type":     matchType,
		"pattern":        pattern,
		"case_sensitive": caseSensitive,
		"recommendation": recommendation,
		"enabled":        enabled,
		"updated_at":     time.Now().UTC(),
	}
	var existing models.InstanceAuditBuiltinRuleOverride
	findResult := db.DB.WithContext(ctx).
		Where("host_id = ? AND builtin_rule_id = ?", hostID, base.ID).Limit(1).Find(&existing)
	if findResult.Error != nil {
		hostsRespondInternal(c)
		return
	}
	var override *models.InstanceAuditBuiltinRuleOverride
	if findResult.RowsAffected > 0 {
		if err := db.DB.WithContext(ctx).Model(&models.InstanceAuditBuiltinRuleOverride{}).
			Where("id = ?", existing.ID).Updates(columns).Error; err != nil {
			hostsRespondInternal(c)
			return
		}
		existing.UpdatedAt = time.Now().UTC()
		override = &existing
	} else {
		columns["host_id"] = hostID
		columns["builtin_rule_id"] = base.ID
		columns["created_at"] = time.Now().UTC()
		var created models.InstanceAuditBuiltinRuleOverride
		if err := db.DB.WithContext(ctx).Model(&models.InstanceAuditBuiltinRuleOverride{}).
			Create(columns).Error; err != nil {
			hostsRespondInternal(c)
			return
		}
		if err := db.DB.WithContext(ctx).Where("host_id = ? AND builtin_rule_id = ?", hostID, base.ID).
			Limit(1).Find(&created).Error; err != nil {
			hostsRespondInternal(c)
			return
		}
		created.UpdatedAt = created.CreatedAt
		override = &created
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.builtin_rule.override",
		fmt.Sprintf(`Updated builtin audit rule override "%s" on host "%s"`, base.ID, host.Name), "success", nil)
	applied := hostsApplyBuiltinAuditOverride(base, override)
	c.JSON(http.StatusOK, hostsBuiltinAuditRuleResponse(applied, base, override))
}

// hostsAuditBuiltinRuleResetHandler mirrors DELETE
// /:id/ops/audit/builtin-rules/:ruleId.
func hostsAuditBuiltinRuleResetHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	base, found := hostsFindBuiltinAuditRule(c.Param("ruleId"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "系统内置规则不存在"})
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	if err := db.DB.WithContext(ctx).
		Where("host_id = ? AND builtin_rule_id = ?", hostID, base.ID).
		Delete(&models.InstanceAuditBuiltinRuleOverride{}).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.builtin_rule.reset",
		fmt.Sprintf(`Reset builtin audit rule override "%s" on host "%s"`, base.ID, host.Name), "warning", nil)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func hostsFindBuiltinAuditRule(ruleID string) (hostsAuditRule, bool) {
	for _, rule := range hostsBuiltinAuditRules {
		if rule.ID == ruleID {
			return rule, true
		}
	}
	return hostsAuditRule{}, false
}

// hostsAuditRuleCreateHandler mirrors POST /:id/ops/audit/rules.
func hostsAuditRuleCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"name", "pattern"}, []ajvFieldSpec{
		{Name: "scope", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"host", "global"}},
		{Name: "name", Kind: ajvKindString, MinLength: 2, MaxLength: 80},
		{Name: "description", Kind: ajvKindString, MinLength: -1, MaxLength: 500},
		{Name: "severity", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"info", "low", "medium", "high"}},
		{Name: "category", Kind: ajvKindString, MinLength: -1, MaxLength: 64},
		{Name: "matchType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"contains", "regex", "exact"}},
		{Name: "pattern", Kind: ajvKindString, MinLength: 1, MaxLength: 500},
		{Name: "caseSensitive", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "recommendation", Kind: ajvKindString, MinLength: -1, MaxLength: 1000},
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	if _, _, ok := hostsValidateArrayOfStrings(c, object, "targetTypes", false); !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	scope := "host"
	if text, present := validated.stringValue("scope"); present && text == "global" {
		scope = "global"
	}
	if scope == "global" && user.Role != "admin" {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	matchType := hostsNormalizeAuditMatchType(stringOrNil(validated, "matchType"))
	pattern := strings.TrimSpace(stringValueOr(validated, "pattern"))
	if err := hostsValidateAuditRegex(matchType, pattern); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "正则表达式无效"})
		return
	}
	targetTypes := hostsNormalizeAuditTargetTypes(object["targetTypes"])
	targetTypesJSON, _ := json.Marshal(targetTypes)
	name := strings.TrimSpace(stringValueOr(validated, "name"))
	category := strings.TrimSpace(stringValueOr(validated, "category"))
	if category == "" {
		category = "custom"
	}
	enabled := true
	if value, present := hostsValidatedBool(validated, "enabled"); present {
		enabled = value
	}

	rule := models.InstanceAuditRule{
		CreatedByID:    user.ID,
		Name:           name,
		Description:    hostsNullableTrimmed(validated, "description"),
		Severity:       hostsNormalizeAuditSeverity(stringOrNil(validated, "severity")),
		Category:       category,
		TargetTypes:    datatypes.JSON(targetTypesJSON),
		MatchType:      matchType,
		Pattern:        pattern,
		CaseSensitive:  func() bool { value, _ := hostsValidatedBool(validated, "caseSensitive"); return value }(),
		Recommendation: hostsNullableTrimmed(validated, "recommendation"),
		Enabled:        enabled,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if scope == "host" {
		rule.HostID = &hostID
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	if err := db.DB.WithContext(ctx).Create(&rule).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.rule.create",
		fmt.Sprintf(`Created %s audit rule "%s" on host "%s"`, scope, rule.Name, host.Name), "success", nil)
	c.JSON(http.StatusOK, hostsCustomAuditRuleResponse(rule, hostID))
}

// hostsAuditRuleUpdateHandler mirrors PATCH /:id/ops/audit/rules/:ruleId.
func hostsAuditRuleUpdateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	ruleID, ok := hostsParsePositiveRouteID(c.Param("ruleId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	validated, object, ok := hostsValidatedBody(c, nil, []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "description", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1},
		{Name: "severity", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "category", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "matchType", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "pattern", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "caseSensitive", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
		{Name: "recommendation", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1},
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	if _, _, ok := hostsValidateArrayOfStrings(c, object, "targetTypes", false); !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	var existing models.InstanceAuditRule
	findResult := db.DB.WithContext(ctx).Where("id = ?", ruleID).Limit(1).Find(&existing)
	if findResult.Error != nil {
		hostsRespondInternal(c)
		return
	}
	if findResult.RowsAffected == 0 || (existing.HostID != nil && *existing.HostID != hostID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "审查规则不存在"})
		return
	}
	if existing.HostID == nil && user.Role != "admin" {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	columns := map[string]any{}
	if text, present := validated.stringValue("name"); present {
		columns["name"] = strings.TrimSpace(text)
	}
	if value, present := validated.nullableString("description"); present {
		if value != nil && strings.TrimSpace(*value) != "" {
			columns["description"] = strings.TrimSpace(*value)
		} else {
			columns["description"] = nil
		}
	}
	if text, present := validated.stringValue("severity"); present {
		columns["severity"] = hostsNormalizeAuditSeverity(text)
	}
	if text, present := validated.stringValue("category"); present {
		category := strings.TrimSpace(text)
		if category == "" {
			category = "custom"
		}
		columns["category"] = category
	}
	if raw, present := object["targetTypes"]; present {
		encoded, _ := json.Marshal(hostsNormalizeAuditTargetTypes(raw))
		columns["target_types"] = datatypes.JSON(encoded)
	}
	matchType := existing.MatchType
	if text, present := validated.stringValue("matchType"); present {
		matchType = hostsNormalizeAuditMatchType(text)
		columns["match_type"] = matchType
	}
	pattern := existing.Pattern
	if text, present := validated.stringValue("pattern"); present {
		pattern = strings.TrimSpace(text)
		columns["pattern"] = pattern
	}
	if value, present := hostsValidatedBool(validated, "caseSensitive"); present {
		columns["case_sensitive"] = value
	}
	if value, present := validated.nullableString("recommendation"); present {
		if value != nil && strings.TrimSpace(*value) != "" {
			columns["recommendation"] = strings.TrimSpace(*value)
		} else {
			columns["recommendation"] = nil
		}
	}
	if value, present := hostsValidatedBool(validated, "enabled"); present {
		columns["enabled"] = value
	}
	if err := hostsValidateAuditRegex(matchType, pattern); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "正则表达式无效"})
		return
	}
	if len(columns) > 0 {
		columns["updated_at"] = time.Now().UTC()
		if err := db.DB.WithContext(ctx).Model(&models.InstanceAuditRule{}).
			Where("id = ?", ruleID).Updates(columns).Error; err != nil {
			hostsRespondInternal(c)
			return
		}
	}
	if err := db.DB.WithContext(ctx).Where("id = ?", ruleID).Limit(1).Find(&existing).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.rule.update",
		fmt.Sprintf(`Updated audit rule "%s" on host "%s"`, existing.Name, host.Name), "success", nil)
	c.JSON(http.StatusOK, hostsCustomAuditRuleResponse(existing, hostID))
}

// hostsAuditRuleDeleteHandler mirrors DELETE /:id/ops/audit/rules/:ruleId.
func hostsAuditRuleDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	ruleID, ok := hostsParsePositiveRouteID(c.Param("ruleId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	var existing models.InstanceAuditRule
	findResult := db.DB.WithContext(ctx).Where("id = ?", ruleID).Limit(1).Find(&existing)
	if findResult.Error != nil {
		hostsRespondInternal(c)
		return
	}
	if findResult.RowsAffected == 0 || (existing.HostID != nil && *existing.HostID != hostID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "审查规则不存在"})
		return
	}
	if existing.HostID == nil && user.Role != "admin" {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if err := db.DB.WithContext(ctx).Where("id = ?", ruleID).Delete(&models.InstanceAuditRule{}).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.rule.delete",
		fmt.Sprintf(`Deleted audit rule "%s" on host "%s"`, existing.Name, host.Name), "warning", nil)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// hostsAuditIgnoresListHandler mirrors GET /:id/ops/audit/ignores.
func hostsAuditIgnoresListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	instanceID, ok := hostsParseOptionalPositiveQueryInteger(c.Query("instanceId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	query := db.DB.WithContext(ctx).Where("host_id = ? AND enabled = true", hostID)
	if instanceID != nil {
		query = query.Where("instance_id IS NULL OR instance_id = ?", *instanceID)
	}
	var ignores []models.InstanceAuditIgnore
	if err := query.Order("created_at DESC").Limit(100).Find(&ignores).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ignores": ignores})
}

// hostsAuditIgnoreCreateHandler mirrors POST /:id/ops/audit/ignores.
func hostsAuditIgnoreCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	body, ok := hostsDecodedBody(c)
	if !ok {
		return
	}
	object, message := hostsBodyObject(body)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	scope := "instance"
	if text, ok := object["scope"].(string); ok && text == "host" {
		scope = "host"
	}
	var instanceID *int64
	if scope == "instance" {
		value, ok := hostsParseIntegerBody(object["instanceId"], 1, float64(hostsMaxInt32))
		if !ok {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		if _, ok := hostsGetInstanceOwnedByHost(c, int32(value), hostID); !ok {
			return
		}
		instanceID = &value
	}
	ruleID := ""
	if text, ok := object["ruleId"].(string); ok {
		ruleID = strings.TrimSpace(text)
	}
	matchText := ""
	if text, ok := object["matchText"].(string); ok {
		matchText = strings.TrimSpace(text)
	}
	if ruleID == "" && matchText == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "白名单至少需要规则 ID 或匹配文本"})
		return
	}
	expiresInDays, ok := hostsParseOptionalIntegerBody(object["expiresInDays"], 1, hostsAuditIgnoreMaxExpiresDay)
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}
	var expiresAt *time.Time
	if expiresInDays != nil {
		value := time.Now().UTC().Add(time.Duration(*expiresInDays) * 24 * time.Hour)
		expiresAt = &value
	}
	reason := ""
	if text, ok := object["reason"].(string); ok {
		reason = strings.TrimSpace(text)
	}

	ignore := models.InstanceAuditIgnore{
		HostID:      hostID,
		CreatedByID: user.ID,
		RuleID:      hostsNilIfEmpty(ruleID),
		TargetType:  hostsNormalizeAuditFindingTarget(object["targetType"]),
		MatchText:   hostsNilIfEmpty(matchText),
		Scope:       scope,
		Reason:      hostsNilIfEmpty(reason),
		Enabled:     true,
		ExpiresAt:   expiresAt,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if instanceID != nil {
		converted := int32(*instanceID)
		ignore.InstanceID = &converted
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	if err := db.DB.WithContext(ctx).Create(&ignore).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.ignore.create",
		fmt.Sprintf(`Created %s audit whitelist on host "%s"`, scope, host.Name), "success", nil)
	c.JSON(http.StatusOK, ignore)
}

// hostsGetInstanceOwnedByHost mirrors the 404 branch of the ignore create path.
func hostsGetInstanceOwnedByHost(c *gin.Context, instanceID, hostID int32) (*models.Instance, bool) {
	instance, err := hostsGetInstanceByID(c.Request.Context(), instanceID)
	if err != nil {
		hostsRespondInternal(c)
		return nil, false
	}
	if instance == nil || instance.HostID != hostID {
		hostsRespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return nil, false
	}
	return instance, true
}

// hostsAuditIgnoreDeleteHandler mirrors DELETE /:id/ops/audit/ignores/:ignoreId.
func hostsAuditIgnoreDeleteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	ignoreID, ok := hostsParsePositiveRouteID(c.Param("ignoreId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	var ignore models.InstanceAuditIgnore
	findResult := db.DB.WithContext(ctx).Where("id = ?", ignoreID).Limit(1).Find(&ignore)
	if findResult.Error != nil {
		hostsRespondInternal(c)
		return
	}
	if findResult.RowsAffected == 0 || ignore.HostID != hostID {
		c.JSON(http.StatusNotFound, gin.H{"error": "白名单不存在"})
		return
	}
	if err := db.DB.WithContext(ctx).Model(&models.InstanceAuditIgnore{}).
		Where("id = ?", ignoreID).Updates(map[string]any{"enabled": false, "updated_at": time.Now().UTC()}).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops.audit.ignore.delete",
		fmt.Sprintf(`Disabled audit whitelist #%d on host "%s"`, ignoreID, host.Name), "warning", nil)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// hostsAuditHistoryHandler mirrors GET /:id/ops/audit/history.
func hostsAuditHistoryHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	instanceID, ok := hostsParseOptionalPositiveQueryInteger(c.Query("instanceId"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	pageSize, ok := hostsParseAuditHistoryPageSize(c.Query("pageSize"))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if db.DB == nil {
		hostsRespondInternal(c)
		return
	}
	limit := int32(20)
	if pageSize != nil {
		limit = *pageSize
	}
	scanQuery := db.DB.WithContext(ctx).Where("host_id = ?", hostID)
	if instanceID != nil {
		scanQuery = scanQuery.Where("instance_id = ?", *instanceID)
	}
	var scans []models.InstanceAuditScan
	if err := scanQuery.Order("created_at DESC").Limit(int(limit)).Find(&scans).Error; err != nil {
		hostsRespondInternal(c)
		return
	}
	actionQuery := db.DB.WithContext(ctx).Where("host_id = ?", hostID)
	if instanceID != nil {
		actionQuery = actionQuery.Where("instance_id = ?", *instanceID)
	}
	var actions []models.InstanceAuditAction
	if err := actionQuery.Order("created_at DESC").Limit(int(limit)).Find(&actions).Error; err != nil {
		hostsRespondInternal(c)
		return
	}

	userIDs := map[int32]bool{}
	for _, scan := range scans {
		userIDs[scan.UserID] = true
	}
	for _, action := range actions {
		userIDs[action.UserID] = true
	}
	users, err := hostsUsersBrief(ctx, userIDs)
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	scanPayload := make([]gin.H, 0, len(scans))
	for _, scan := range scans {
		scanPayload = append(scanPayload, gin.H{
			"id": scan.ID, "hostId": scan.HostID, "instanceId": scan.InstanceID,
			"userId": scan.UserID, "status": scan.Status, "capability": scan.Capability,
			"riskLevel": scan.RiskLevel, "findingCount": scan.FindingCount,
			"ignoredCount": scan.IgnoredCount, "processCount": scan.ProcessCount,
			"connectionCount": scan.ConnectionCount, "listeningCount": scan.ListeningCount,
			"startupItemCount": scan.StartupItemCount, "findings": scan.Findings,
			"error": scan.Error, "createdAt": scan.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			"user": users[scan.UserID],
		})
	}
	actionPayload := make([]gin.H, 0, len(actions))
	for _, action := range actions {
		actionPayload = append(actionPayload, gin.H{
			"id": action.ID, "scanId": action.ScanID, "hostId": action.HostID,
			"instanceId": action.InstanceID, "userId": action.UserID,
			"actionType": action.ActionType, "pid": action.Pid, "signal": action.Signal,
			"processCommand": action.ProcessCommand, "reason": action.Reason,
			"result": action.Result, "stdout": action.Stdout, "stderr": action.Stderr,
			"createdAt": action.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			"user":      users[action.UserID],
		})
	}
	c.JSON(http.StatusOK, gin.H{"scans": scanPayload, "actions": actionPayload})
}

// hostsUsersBrief mirrors `user: { select: { id, username } }`.
func hostsUsersBrief(ctx context.Context, ids map[int32]bool) (map[int32]gin.H, error) {
	out := map[int32]gin.H{}
	if len(ids) == 0 || db.DB == nil {
		return out, nil
	}
	list := make([]int32, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	var rows []struct {
		ID       int32  `gorm:"column:id"`
		Username string `gorm:"column:username"`
	}
	if err := db.DB.WithContext(ctx).Table("users").Select("id, username").Where("id IN ?", list).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = gin.H{"id": row.ID, "username": row.Username}
	}
	return out, nil
}

// hostsAuditScanHandler mirrors POST
// /:id/ops/instances/:instanceId/audit/scan.
func hostsAuditScanHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "审查扫描失败: " + err.Error()})
		return
	}
	stateRaw, err := hostsGetInstanceState(ctx, client, instance.IncusID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "审查扫描失败: " + err.Error()})
		return
	}
	instanceRaw, err := hostsIncusRequest(ctx, client, "/1.0/instances/"+instance.IncusID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "审查扫描失败: " + err.Error()})
		return
	}
	var state struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(stateRaw, &state)
	var detail struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(instanceRaw, &detail)
	incusStatus := state.Status
	if incusStatus == "" {
		incusStatus = detail.Status
	}
	if incusStatus != "Running" {
		status := incusStatus
		if status == "" {
			status = string(instance.Status)
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未运行，无法执行审查扫描", "status": status})
		return
	}

	// 执行/解析/研判依赖 lib/instance-audit.ts（未移植）——显式失败。
	hostsRespondSeam(c, errHostsSeam("lib/instance-audit.ts（审查扫描执行段）"))
}

// hostsAuditKillProcessHandler mirrors POST
// /:id/ops/instances/:instanceId/audit/kill-process.
func hostsAuditKillProcessHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	_, object, ok := hostsValidatedBody(c, []string{"pid", "reason", "confirmationText"}, []ajvFieldSpec{
		{Name: "pid", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: hostsAuditKillProcessMinPID, Maximum: hostsAuditKillProcessMaxPID},
		{Name: "signal", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"TERM", "KILL"}},
		{Name: "reason", Kind: ajvKindString, MinLength: 3, MaxLength: 500},
		{Name: "confirmationText", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "scanId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "expectedCommand", Kind: ajvKindString, MinLength: -1, MaxLength: 500},
	})
	if !ok {
		return
	}
	pid, ok := hostsParseIntegerBody(object["pid"], hostsAuditKillProcessMinPID, hostsAuditKillProcessMaxPID)
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	signalName := "TERM"
	if text, ok := object["signal"].(string); ok && text == "KILL" {
		signalName = "KILL"
	}
	reason := strings.TrimSpace(fmt.Sprint(object["reason"]))
	confirmationText := strings.TrimSpace(fmt.Sprint(object["confirmationText"]))
	parsedScanID, ok := hostsParseOptionalIntegerBody(object["scanId"], 1, float64(hostsMaxInt32))
	if !ok {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	expectedCommand := hostsNormalizeProcessText(fmt.Sprint(object["expectedCommand"]))

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	if confirmationText != instance.IncusID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "确认文本不匹配实例名称"})
		return
	}
	if len(reason) < 3 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写处置原因"})
		return
	}

	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "停止进程失败: " + err.Error()})
		return
	}
	stateRaw, err := hostsGetInstanceState(ctx, client, instance.IncusID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "停止进程失败: " + err.Error()})
		return
	}
	var state struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(stateRaw, &state)
	if state.Status != "Running" {
		status := state.Status
		if status == "" {
			status = string(instance.Status)
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未运行，无法停止进程", "status": status})
		return
	}

	_ = pid
	_ = signalName
	_ = parsedScanID
	_ = expectedCommand
	hostsRespondSeam(c, errHostsSeam("lib/instance-audit.ts（execWithOutput + 审查动作落库）"))
}

// hostsNormalizeProcessText mirrors normalizeProcessText.
func hostsNormalizeProcessText(value string) string {
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(value, " "))
}

// hostsOpsInstanceSyncHandler mirrors POST /:id/ops/instances/:instanceId/sync.
func hostsOpsInstanceSyncHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败: " + err.Error()})
		return
	}
	stateRaw, err := hostsGetInstanceState(ctx, client, instance.IncusID)
	if err != nil {
		if lib.IsIncusNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例在节点上不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败: " + err.Error()})
		return
	}
	instanceRaw, err := hostsIncusRequest(ctx, client, "/1.0/instances/"+instance.IncusID)
	if err != nil {
		if lib.IsIncusNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例在节点上不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败: " + err.Error()})
		return
	}
	var state struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(stateRaw, &state)
	if state.Status == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取实例状态"})
		return
	}
	incusStatus := hostsMapInstanceStatus(state.Status)
	if incusStatus == "unknown" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "未知状态: " + state.Status})
		return
	}
	previous := string(instance.Status)
	statusChanged := false
	if previous != incusStatus {
		_ = db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatus(incusStatus), nil)
		statusChanged = true
	}
	delta, err := HostsSnapshotSyncApplier(ctx, *instance, instanceRaw, stateRaw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败: " + err.Error()})
		return
	}
	payload := gin.H{
		"success":       true,
		"message":       "实例状态/IP 已同步",
		"statusChanged": statusChanged,
		"currentStatus": incusStatus,
		"ipv4Changed":   delta.IPv4Changed,
		"ipv6Changed":   delta.IPv6Changed,
	}
	if statusChanged {
		payload["from"] = previous
		payload["to"] = incusStatus
	}
	if delta.IPv4Changed {
		payload["oldIpv4"] = delta.OldIPv4
		payload["newIpv4"] = delta.NewIPv4
	}
	if delta.IPv6Changed {
		payload["oldIpv6"] = delta.OldIPv6
		payload["newIpv6"] = delta.NewIPv6
	}
	c.JSON(http.StatusOK, payload)
}

// hostsOpsInstanceRestartHandler mirrors POST /:id/ops/instances/:instanceId/restart.
func hostsOpsInstanceRestartHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, _, ok := hostsValidatedBody(c, nil, []ajvFieldSpec{
		{Name: "force", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	force, _ := hostsValidatedBool(validated, "force")
	_ = force

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	if instance.Status == "suspended" {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			hostsRespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
			return
		}
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
		return
	}
	if hostsRejectActiveWorkflowConflict(c, instance.ID) {
		return
	}
	client, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重启失败: " + err.Error()})
		return
	}
	_ = client
	hostsRespondSeam(c, errHostsSeam("lib/incus#restartInstance"))
}

// hostsDangerousActionHandler mirrors POST
// /:id/ops/instances/:instanceId/dangerous-action.
func hostsDangerousActionHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	hostID, ok := hostsRouteID(c)
	if !ok {
		return
	}
	validated, object, ok := hostsValidatedBody(c, []string{"action", "imageAlias", "confirmationText", "riskConfirmed"}, []ajvFieldSpec{
		{Name: "action", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"rebuild", "recreate"}},
		{Name: "imageAlias", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "sshKeyId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "confirmationText", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		{Name: "riskConfirmed", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if !ok {
		return
	}
	customInitCommandIDs, _, ok := hostsValidateIntegerArray(c, object, "customInitCommandIds", false, -1, 20)
	if !ok {
		return
	}
	action, _ := validated.stringValue("action")
	imageAlias, _ := validated.stringValue("imageAlias")
	confirmationText, _ := validated.stringValue("confirmationText")
	riskConfirmed, _ := hostsValidatedBool(validated, "riskConfirmed")
	sshKeyID, _ := hostsValidatedInt64(validated, "sshKeyId")

	host, ok := hostsLoadHost(c, hostID)
	if !ok {
		return
	}
	if !hostsOwnerOrAdmin(host, user) {
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	instance, ok := hostsOpsInstance(c, hostID)
	if !ok {
		return
	}
	if !riskConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先确认风险提示"})
		return
	}
	if strings.TrimSpace(confirmationText) != instance.IncusID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "确认文本不匹配实例名称"})
		return
	}
	if instance.Status == "suspended" {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			hostsRespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
			return
		}
		hostsRespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
		return
	}
	if action == "rebuild" && instance.Status != "stopped" {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStopRequired, "")
		return
	}
	if hostsRejectActiveWorkflowConflict(c, instance.ID) {
		return
	}
	if valid, err := db.IsValidSystemImage(ctx, imageAlias); err != nil {
		hostsRespondInternal(c)
		return
	} else if !valid {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrImageNotFound, "")
		return
	}
	if compatible, err := db.IsImageCompatibleWithMemory(ctx, imageAlias, int(instance.Memory)); err != nil {
		hostsRespondInternal(c)
		return
	} else if !compatible {
		hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrImageMemoryIncompatible, "")
		return
	}
	instanceType := "container"
	if instance.PackageID != nil {
		if pkg, err := hostsLoadPackageRow(ctx, *instance.PackageID); err == nil && pkg != nil && pkg.InstanceType != "" {
			instanceType = pkg.InstanceType
		}
	}
	availability, err := db.GetSystemImageAvailabilityForHost(ctx, imageAlias, instance.HostID, db.SystemImageAvailabilityOptions{
		InstanceType: &instanceType,
		Memory:       hostsIntPtr(int(instance.Memory)),
	})
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if !availability.OK {
		switch availability.Reason {
		case "host_not_found":
			hostsRespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		case "image_not_found":
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrImageNotFound, "")
		case "memory_incompatible":
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrImageMemoryIncompatible, "")
		case "instance_type_mismatch":
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrImageTypeMismatch, "")
		case "host_instance_type_mismatch":
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrHostInstanceTypeMismatch, "")
		default:
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceImageUnavailable, "")
		}
		return
	}
	if sshKeyID != nil {
		key, err := db.GetSSHKeyByID(ctx, int32(*sshKeyID))
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if key == nil || key.UserID != instance.UserID {
			hostsRespondAPIError(c, http.StatusBadRequest, lib.ErrSshKeyNotOwned, "")
			return
		}
	}
	if len(customInitCommandIDs) > 0 {
		validation, err := db.ValidateCustomInitCommandsOwnership(ctx, customInitCommandIDs, instance.UserID)
		if err != nil {
			hostsRespondInternal(c)
			return
		}
		if !validation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid custom init command IDs", "code": "INVALID_COMMAND_IDS"})
			return
		}
	}

	taskType := models.InstanceTaskTypeRebuild
	if action == "recreate" {
		taskType = models.InstanceTaskTypeRecreate
	}
	var sshKeyPtr *int32
	if sshKeyID != nil {
		converted := int32(*sshKeyID)
		sshKeyPtr = &converted
	}
	task, err := db.CreateInstanceTask(ctx, db.CreateInstanceTaskData{
		InstanceID:           instance.ID,
		HostID:               instance.HostID,
		UserID:               user.ID,
		TaskType:             taskType,
		ImageAlias:           &imageAlias,
		SSHKeyID:             sshKeyPtr,
		CustomInitCommandIDs: customInitCommandIDs,
	})
	if err != nil {
		var conflict *db.InstanceTaskConflictError
		if errors.As(err, &conflict) {
			var taskID *int32
			taskType := ""
			status := ""
			if conflict.ActiveTask != nil {
				taskID = &conflict.ActiveTask.ID
				taskType = string(conflict.ActiveTask.TaskType)
				status = string(conflict.ActiveTask.Status)
			}
			hostsSendTaskConflict(c, taskID, &taskType, &status)
			return
		}
		hostsRespondInternal(c)
		return
	}

	hostsCreateLog(ctx, i64Ptr(user.ID), "host.ops."+action,
		fmt.Sprintf(`Queued %s task for instance "%s" on host "%s" with image %s`, action, instance.Name, host.Name, imageAlias),
		"success", &instance.ID)
	message := "实例重建任务已排队"
	if action == "rebuild" {
		message = "实例重装任务已排队"
	}
	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": message,
		"taskId":  task.ID,
		"status":  task.Status,
	})
}

// ==================== 通用小工具 ====================

// hostsValidatedInt64 reads an integer body field.
func hostsValidatedInt64(validated ajvValidated, name string) (*int64, bool) {
	raw, present := validated.Values[name]
	if !present {
		return nil, false
	}
	number, ok := hostsJSONNumber(raw)
	if !ok {
		return nil, false
	}
	value := int64(number)
	return &value, true
}

// hostsValidatedBool reads a boolean body field with presence.
func hostsValidatedBool(validated ajvValidated, name string) (bool, bool) {
	raw, present := validated.Values[name]
	if !present {
		return false, false
	}
	value, ok := raw.(bool)
	return value, ok
}

// stringValueOr reads an optional string body field.
func stringValueOr(validated ajvValidated, name string) string {
	value, _ := validated.stringValue(name)
	return value
}

// stringOrNil returns the raw value for normalisers that take `unknown`.
func stringOrNil(validated ajvValidated, name string) any {
	raw, present := validated.Values[name]
	if !present {
		return nil
	}
	return raw
}

// hostsNullableTrimmed trims an optional nullable string field.
func hostsNullableTrimmed(validated ajvValidated, name string) *string {
	value, present := validated.nullableString(name)
	if !present || value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func hostsValueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func valueOrNA(value string) string {
	if value == "" {
		return "N/A"
	}
	return value
}

func hostsDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func hostsNilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func hostsNilIfEmptyPtr(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

func hostsInt32Ptr(value *int64) *int32 {
	if value == nil {
		return nil
	}
	converted := int32(*value)
	return &converted
}

func hostsInt32PtrValue(value int64) *int32 {
	converted := int32(value)
	return &converted
}

func hostsBoolPtr(value bool) *bool { return &value }

func i64Ptr(value int32) *int64 {
	converted := int64(value)
	return &converted
}

// ==================== registration ====================

// RegisterHosts mounts /api/hosts exactly like app.ts' prefix registration.
func RegisterHosts(r *gin.Engine) {
	public := r.Group("/api/hosts")
	// Public (one-time token / ticket) routes: declared before the authenticated
	// ones for readability only — gin dispatches static paths first regardless.
	public.GET("/install.sh", hostsInstallScriptHandler)
	public.GET("/install.sh/:token", hostsInstallScriptTokenHandler)
	public.GET("/cert/:token", hostsCertHandler)
	public.GET("/:hostId/agent/logs/stream", hostsAgentLogStreamHandler)

	group := r.Group("/api/hosts", middleware.Authenticate())
	group.GET("", hostsListHandler)
	group.GET("/", hostsListHandler)
	group.POST("", hostsCreateHandler)
	group.POST("/", hostsCreateHandler)

	group.POST("/:hostId/verify", hostsVerifyHandler)
	group.POST("/:hostId/regenerate-install", hostsRegenerateInstallHandler)
	group.POST("/:hostId/test", hostsTestHandler)
	group.GET("/:hostId", hostsDetailHandler)
	group.GET("/:hostId/images", hostsImagesGetHandler)
	group.PUT("/:hostId/images", hostsImagesPutHandler)
	group.PATCH("/:hostId", hostsUpdateHandler)
	group.DELETE("/:hostId", hostsDeleteHandler)

	group.POST("/:hostId/maintenance", hostsMaintenanceHandler)
	group.POST("/:hostId/sync", hostsSyncHandler)
	group.POST("/:hostId/instances/batch-delete-preview", hostsBatchDeletePreviewHandler)
	group.DELETE("/:hostId/instances/batch", hostsBatchDeleteHandler)

	group.GET("/:hostId/storage-pools", hostsStoragePoolsListHandler)
	group.POST("/:hostId/storage-pools", hostsStoragePoolCreateHandler)
	group.DELETE("/:hostId/storage-pools/:poolName", hostsStoragePoolDeleteHandler)
	group.PATCH("/:hostId/storage-pools/:poolName", hostsStoragePoolUpdateHandler)

	group.GET("/:hostId/public-ipv4/pools", hostsPublicIpv4PoolsHandler)
	group.POST("/:hostId/public-ipv4/pools", hostsPublicIpv4PoolCreateHandler)
	group.POST("/:hostId/public-ipv4/pools/:poolId/addresses", hostsPublicIpv4AddressesAddHandler)
	group.PATCH("/:hostId/public-ipv4/addresses/:addressId", hostsPublicIpv4AddressStatusHandler)
	group.DELETE("/:hostId/public-ipv4/addresses/:addressId", hostsPublicIpv4AddressDeleteHandler)

	group.GET("/:hostId/caddy", hostsCaddyStatusHandler)
	group.POST("/:hostId/caddy/install", hostsCaddyInstallHandler)
	group.POST("/:hostId/caddy/uninstall", hostsCaddyUninstallHandler)
	group.GET("/:hostId/caddy/sites", hostsCaddySitesHandler)
	group.POST("/:hostId/agent/logs/ticket", hostsAgentLogTicketHandler)

	group.POST("/:hostId/instances/sync-status", hostsSyncStatusHandler)
	group.POST("/:hostId/recalculate-resources", hostsRecalculateResourcesHandler)
	group.POST("/:hostId/instances/suspend", hostsSuspendHandler)
	group.POST("/:hostId/instances/unsuspend", hostsUnsuspendHandler)
	group.GET("/:hostId/plans", hostsPlansHandler)
	group.POST("/:hostId/instances/migrate", hostsMigrateHandler)
	group.GET("/:hostId/users/lookup", hostsUsersLookupHandler)
	group.POST("/:hostId/instances/create-for-user", hostsCreateForUserHandler)
	group.POST("/:hostId/instances/gift-days", hostsGiftDaysHandler)
	group.PATCH("/:hostId/instances/:instanceId/renewal-price", hostsRenewalPriceHandler)

	group.POST("/:hostId/ops/discover", hostsOpsDiscoverHandler)
	group.POST("/:hostId/ops/optimize-port-mappings", hostsOpsOptimizePortMappingsHandler)
	group.POST("/:hostId/ops/baseline-sync", hostsOpsBaselineSyncHandler)
	group.POST("/:hostId/ops/network-repair", hostsOpsNetworkRepairHandler)
	group.POST("/:hostId/ops/instances/:instanceId/preview", hostsOpsInstancePreviewHandler)
	group.GET("/:hostId/ops/instances/:instanceId/ssh-keys", hostsOpsInstanceSSHKeysHandler)
	group.GET("/:hostId/ops/instances/:instanceId/init-commands", hostsOpsInstanceInitCommandsHandler)
	group.GET("/:hostId/ops/audit/rules", hostsAuditRulesHandler)
	group.PATCH("/:hostId/ops/audit/builtin-rules/:ruleId", hostsAuditBuiltinRuleOverrideHandler)
	group.DELETE("/:hostId/ops/audit/builtin-rules/:ruleId", hostsAuditBuiltinRuleResetHandler)
	group.POST("/:hostId/ops/audit/rules", hostsAuditRuleCreateHandler)
	group.PATCH("/:hostId/ops/audit/rules/:ruleId", hostsAuditRuleUpdateHandler)
	group.DELETE("/:hostId/ops/audit/rules/:ruleId", hostsAuditRuleDeleteHandler)
	group.GET("/:hostId/ops/audit/ignores", hostsAuditIgnoresListHandler)
	group.POST("/:hostId/ops/audit/ignores", hostsAuditIgnoreCreateHandler)
	group.DELETE("/:hostId/ops/audit/ignores/:ignoreId", hostsAuditIgnoreDeleteHandler)
	group.GET("/:hostId/ops/audit/history", hostsAuditHistoryHandler)
	group.POST("/:hostId/ops/instances/:instanceId/audit/scan", hostsAuditScanHandler)
	group.POST("/:hostId/ops/instances/:instanceId/audit/kill-process", hostsAuditKillProcessHandler)
	group.POST("/:hostId/ops/instances/:instanceId/sync", hostsOpsInstanceSyncHandler)
	group.POST("/:hostId/ops/instances/:instanceId/restart", hostsOpsInstanceRestartHandler)
	group.POST("/:hostId/ops/instances/:instanceId/dangerous-action", hostsDangerousActionHandler)

	// POST /:id/extend-all is admin-only (fastify.authenticateAdmin).
	admin := r.Group("/api/hosts", middleware.AuthenticateAdmin())
	admin.POST("/:hostId/extend-all", hostsExtendAllHandler)
}

func init() {
	Register(RegisterHosts)
}

// hostsIntPtr returns a pointer to value (helpers above take *int for the
// SystemImageAvailabilityOptions memory bound).
func hostsIntPtr(value int) *int { return &value }
