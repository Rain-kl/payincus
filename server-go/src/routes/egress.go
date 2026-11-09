// Port of server/src/routes/egress.ts plus the pure halves of
// server/src/lib/egress/{policy,package,realtime,sync,import}.ts that only this
// route consumes.
//
// app.ts registers this plugin with `{ prefix: '/api' }`, so every path below is
// absolute under /api.
//
// ---------------------------------------------------------------------------
// SEAMS (Phase 7 must wire these to the WebSocket host tunnel)
// ---------------------------------------------------------------------------
// hostTunnelManager is not ported (it belongs to the WebSocket wave). The four
// variables below are the entire tunnel surface this module needs. Their
// defaults FAIL EXPLICITLY — a missing tunnel must never look like a successful
// apply:
//
//	EgressTunnelIsOnline        hostTunnelManager.isTunnelOnline(hostId)
//	EgressTunnelSupportsControl hostTunnelManager.supportsControl(hostId, method)
//	EgressTunnelRequestControl  hostTunnelManager.requestControl(hostId, method, payload, timeout)
//
// A default supportsControl=false makes a live node look like it needs an Agent
// upgrade (409 UPGRADE_REQUIRED), which is exactly what a Go build without the
// tunnel module can honestly promise. Nothing is ever reported as applied.
//
// Everything that does NOT need the tunnel is ported for real: validation, the
// advisory-locked DB writes, the instruction DOCUMENT builder
// (networkInstruction → packageEgressInstruction → resolveProfile), the status
// sanitizer, the probe-result sanitizer and the entire proxy-import parser.
package routes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== seams ====================

// EgressTunnelIsOnline reports whether the host's control tunnel is connected.
// SEAM: wire to hostTunnelManager.isTunnelOnline in Phase 7.
var EgressTunnelIsOnline = func(hostID int32) bool { return false }

// EgressTunnelSupportsControl reports whether the connected Agent advertises the
// control method. SEAM: wire to hostTunnelManager.supportsControl in Phase 7.
var EgressTunnelSupportsControl = func(hostID int32, method string) bool { return false }

// EgressTunnelRequestControl performs one control request over the tunnel.
// SEAM: wire to hostTunnelManager.requestControl in Phase 7.
var EgressTunnelRequestControl = func(
	ctx context.Context, hostID int32, method string, payload any, timeout time.Duration,
) (json.RawMessage, error) {
	return nil, &egressTunnelControlError{Code: egressTunnelCodeUpgradeRequired}
}

// egressTunnelControlError mirrors TunnelControlError.
type egressTunnelControlError struct {
	Code string
}

// Error implements error.
func (e *egressTunnelControlError) Error() string {
	return "节点隧道控制失败: " + e.Code
}

// Tunnel control error codes, mirroring the TypeScript union.
const (
	egressTunnelCodeOffline         = "OFFLINE"
	egressTunnelCodeUpgradeRequired = "UPGRADE_REQUIRED"
	egressTunnelCodeBusy            = "BUSY"
	egressTunnelCodeTimeout         = "TIMEOUT"
	egressTunnelCodeDisconnected    = "DISCONNECTED"
	egressTunnelCodeCancelled       = "CANCELLED"
	egressTunnelCodeInvalidResponse = "INVALID_RESPONSE"
	egressTunnelCodeRemoteError     = "REMOTE_ERROR"
)

// egressControlFailure mirrors controlFailure(error): the HTTP status and message
// for a tunnel failure. Unknown errors map to REMOTE_ERROR.
func egressControlFailure(err error) (int, string) {
	code := egressTunnelCodeRemoteError
	var tunnelError *egressTunnelControlError
	if errors.As(err, &tunnelError) {
		code = tunnelError.Code
	}
	switch code {
	case egressTunnelCodeOffline:
		return http.StatusServiceUnavailable, "节点隧道未连接，请确认 Agent 在线后重试"
	case egressTunnelCodeUpgradeRequired:
		return http.StatusConflict, "请升级 Agent 后使用实时配置与检测"
	case egressTunnelCodeBusy:
		return http.StatusConflict, "节点正在执行检测或配置，请稍后重试"
	case egressTunnelCodeTimeout:
		return http.StatusGatewayTimeout, "等待节点响应超时，请检查节点状态"
	case egressTunnelCodeDisconnected:
		return http.StatusServiceUnavailable, "节点隧道已断开，请重新连接后重试"
	case egressTunnelCodeCancelled:
		return 499, "请求已取消"
	case egressTunnelCodeInvalidResponse:
		return http.StatusBadGateway, "节点返回的数据无效"
	default:
		return http.StatusBadGateway, "节点执行失败，请查看 Agent 日志"
	}
}

// egressRequireNetworkControl mirrors requireNetworkControl.
func egressRequireNetworkControl(hostID int32, method string) error {
	if !EgressTunnelIsOnline(hostID) {
		return &egressTunnelControlError{Code: egressTunnelCodeOffline}
	}
	if !EgressTunnelSupportsControl(hostID, method) {
		return &egressTunnelControlError{Code: egressTunnelCodeUpgradeRequired}
	}
	return nil
}

// ==================== JS helpers ====================

// egressJSString mirrors String(value).
func egressJSString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return egressJSNumberString(typed)
	case nil:
		return "null"
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, egressJSString(item))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return fmt.Sprint(value)
}

// egressJSNumberString mirrors Number.prototype.toString for the values that can
// appear in these documents (integers; non-integers use Go's shortest form).
func egressJSNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 0) {
		if value > 0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e15 {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// egressJSNumber mirrors Number(value).
func egressJSNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		return egressJSStringToNumber(typed)
	case bool:
		if typed {
			return 1
		}
		return 0
	case nil:
		return 0
	case []any:
		return egressJSStringToNumber(egressJSString(typed))
	}
	return math.NaN()
}

// egressJSStringToNumber mirrors Number(string). It reuses the AJV replica's
// unary-plus implementation so hex/binary/octal literals and Infinity behave
// exactly like JavaScript (Number("0x10") === 16).
func egressJSStringToNumber(raw string) float64 {
	number, ok := ajvToJSNumber(raw)
	if !ok {
		return math.NaN()
	}
	return number
}

// egressIsSafeInteger mirrors Number.isSafeInteger.
func egressIsSafeInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) && math.Abs(value) <= 9007199254740991
}

// egressIsInteger mirrors Number.isInteger.
func egressIsInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value)
}

// egressJSNumberString for JSON output: NaN/Infinity become null.
func egressJSONNumber(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return value
}

// egressJSONQuote mirrors JSON.stringify(string) escaping (no HTML escaping, no
// U+2028/U+2029 escaping).
func egressJSONQuote(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if char < 0x20 {
				builder.WriteString(fmt.Sprintf(`\u%04x`, char))
			} else {
				builder.WriteRune(char)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// egressJSONStringify mirrors JSON.stringify with stable (sorted) object keys.
// Only object key ORDER differs from V8 (which uses insertion order); every
// consumer here either hashes the result (fingerprints) or measures its length,
// both of which are order-insensitive. Nil object values are DROPPED, mirroring
// `undefined` keys disappearing from JSON.
func egressJSONStringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case string:
		return egressJSONQuote(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return "null"
		}
		return egressJSNumberString(typed)
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return typed.String()
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, egressJSONStringify(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key, item := range typed {
			if item == nil {
				// `undefined` values never reach JSON.
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, egressJSONQuote(key)+":"+egressJSONStringify(typed[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

// egressJSONRoundTrip mirrors JSON.parse(JSON.stringify(value)).
func egressJSONRoundTrip(value any) any {
	var decoded any
	if err := json.Unmarshal([]byte(egressJSONStringify(value)), &decoded); err != nil {
		return nil
	}
	return decoded
}

// egressJSWhitespace mirrors the characters JavaScript's trim() removes
// (WhiteSpace + LineTerminator, including the BOM).
func egressJSWhitespace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00A0', '\u1680', '\u2028', '\u2029',
		'\u202F', '\u205F', '\u3000', '\uFEFF':
		return true
	}
	return char >= '\u2000' && char <= '\u200A'
}

// egressJSTrim mirrors JavaScript's String.prototype.trim().
func egressJSTrim(value string) string {
	runes := []rune(value)
	start, end := 0, len(runes)
	for start < end && egressJSWhitespace(runes[start]) {
		start++
	}
	for end > start && egressJSWhitespace(runes[end-1]) {
		end--
	}
	return string(runes[start:end])
}

// egressToAny decodes a jsonb document into the Go shape lib.ValidateEgress
// expects (maps, []any, float64).
func egressToAny(raw []byte) (any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// egressContains mounts Array.prototype.includes for int32 ids.
func egressContains(values []int32, candidate int32) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// ==================== policy ids / probe URL ====================

// egressMaxNodeIDs mirrors `ids(value, 10)`'s cap for proxy probes.
const egressMaxNodeIDs = 10

// egressPolicyIDs mirrors `ids(value, max)` from lib/egress/policy.ts.
//
// lib exports ValidateEgress/ValidateFirewall but keeps ids()/probeUrl()
// unexported; these two helpers are byte-identical copies so the route can reuse
// them for proxy probes. Phase 7 should export lib.EgressIDs / lib.EgressProbeURL
// and delete these.
func egressPolicyIDs(value any, max int) ([]int32, error) {
	array, ok := value.([]any)
	if !ok || len(array) > max {
		return nil, &lib.NetworkInputError{Message: "Invalid or duplicate node IDs"}
	}
	seen := make(map[int32]bool, len(array))
	result := make([]int32, 0, len(array))
	for _, item := range array {
		number, ok := item.(float64)
		if !ok || !egressIsSafeInteger(number) || number < 1 || number > 2147483647 {
			return nil, &lib.NetworkInputError{Message: "Invalid or duplicate node IDs"}
		}
		id := int32(number)
		if seen[id] {
			return nil, &lib.NetworkInputError{Message: "Invalid or duplicate node IDs"}
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}

// egressPolicyProbeURL mirrors `probeUrl(value)` from lib/egress/policy.ts.
func egressPolicyProbeURL(value any) (string, error) {
	text, ok := value.(string)
	if !ok || jsStringLength(text) > 2048 {
		return "", &lib.NetworkInputError{Message: "Invalid probe URL"}
	}
	parsed, err := url.Parse(text)
	if err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Hostname() != "" {
		return parsed.String(), nil
	}
	return "", &lib.NetworkInputError{Message: "Probe URL must use HTTPS without credentials"}
}

// ==================== response helpers ====================

// egressInternal reports an unexpected failure like fastify's generic handler.
func egressInternal(c *gin.Context, err error) {
	message := "Internal server error"
	if !lib.IsProductionEnv() && err != nil {
		message = err.Error()
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
}

// egressThrowable mirrors a thrown JS error: NetworkInputError is 400, everything
// else is a 500 the generic handler renders.
func egressThrowable(c *gin.Context, err error) {
	var networkError *lib.NetworkInputError
	if errors.As(err, &networkError) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": networkError.Message})
		return
	}
	egressInternal(c, err)
}

// egressISOTime mirrors Date.prototype.toISOString.
func egressISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// egressNullableISOTime mirrors `value?.toISOString() ?? null`.
func egressNullableISOTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return egressISOTime(*value)
}

// egressRouteID parses `:id` for the integer params schema used by this module.
func egressRouteID(c *gin.Context) (int32, bool) {
	return egressParamRouteID(c, "id")
}

// egressParamRouteID parses a named path parameter.
//
// gin requires every route under the same prefix to use ONE wildcard name, so the
// /api/hosts/:hostId/... tree (batch-config, redeem-codes) forces `:hostId` here
// even though the TypeScript writes `:id`. The URL a client sends is identical.
func egressParamRouteID(c *gin.Context, name string) (int32, bool) {
	raw := c.Param(name)
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok || parsed > 2147483647 {
		respondAJVValidation(c, egressProfilesIntegerParamMessage(raw, 1, 2147483647))
		return 0, false
	}
	return int32(parsed), true
}

// ==================== instruction builder ====================

// egressResolvedProfile mirrors `{ egress, nodes }` from resolveProfile.
type egressResolvedProfile struct {
	Egress lib.EgressPolicy
	Nodes  []gin.H
}

// egressResolveProfile mirrors resolveProfile(profile, hostId).
func egressResolveProfile(ctx context.Context, profile db.EgressProfilesRow, hostID int32) (egressResolvedProfile, error) {
	if !profile.Active || !egressContains(profile.AllowedHostIDs, hostID) {
		return egressResolvedProfile{}, &lib.NetworkInputError{Message: "Egress policy unavailable for this host"}
	}
	document, err := egressToAny(profile.Egress)
	if err != nil {
		return egressResolvedProfile{}, err
	}
	egress, err := lib.ValidateEgress(document)
	if err != nil {
		return egressResolvedProfile{}, err
	}
	nodes, err := db.EgressProxyNodesByIDs(ctx, egress.ProxyNodeIDs)
	if err != nil {
		return egressResolvedProfile{}, err
	}
	byID := make(map[int32]db.EgressProxyNode, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	out := make([]gin.H, 0, len(egress.ProxyNodeIDs))
	for _, id := range egress.ProxyNodeIDs {
		node, ok := byID[id]
		if !ok {
			return egressResolvedProfile{}, &lib.NetworkInputError{Message: "Referenced proxy node is missing"}
		}
		outbound, err := egressDecodeProxyConfig(node.ConfigEncrypted)
		if err != nil {
			return egressResolvedProfile{}, err
		}
		out = append(out, gin.H{"id": id, "outbound": outbound})
	}
	return egressResolvedProfile{Egress: egress, Nodes: out}, nil
}

// egressMaxGroups mirrors MAX_EGRESS_GROUPS.
const egressMaxGroups = 16

// egressPackageEgressInstruction mirrors packageEgressInstruction(hostId).
func egressPackageEgressInstruction(ctx context.Context, hostID int32) (gin.H, error) {
	attachments, err := db.EgressAttachmentsForHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	active := make([]db.EgressAttachmentRow, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.InstanceStatus != "deleted" {
			active = append(active, attachment)
		}
	}

	attachedIDs := make([]int32, 0, len(active))
	for _, attachment := range active {
		if attachment.ProfileID == nil {
			continue
		}
		if !egressContains(attachedIDs, *attachment.ProfileID) {
			attachedIDs = append(attachedIDs, *attachment.ProfileID)
		}
	}
	if len(attachedIDs) > egressMaxGroups {
		return nil, &lib.NetworkInputError{Message: "Host package egress group capacity exceeded"}
	}

	attached, err := db.EgressProfilesByIDs(ctx, attachedIDs)
	if err != nil {
		return nil, err
	}
	if len(attached) != len(attachedIDs) {
		return nil, &lib.NetworkInputError{Message: "Referenced egress policy is missing"}
	}
	byID := make(map[int32]db.EgressProfilesRow, len(attached))
	for _, profile := range attached {
		byID[profile.ID] = profile
	}

	groups := make([]gin.H, 0, len(attachedIDs))
	for _, id := range attachedIDs {
		profile := byID[id]
		resolved, err := egressResolveProfile(ctx, profile, hostID)
		if err != nil {
			return nil, err
		}
		groups = append(groups, gin.H{"key": fmt.Sprintf("p%d", profile.ID), "egress": resolved.Egress, "nodes": resolved.Nodes})
	}

	// An authorized host pre-provisions the policy group before its first instance
	// exists. Groups required by live bindings stay strict; pre-provisioning is
	// best effort and an unusable authorized policy is skipped, never fatal.
	authorized, err := db.EgressAuthorizedProfilesForHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	for _, profile := range authorized {
		if len(groups) >= egressMaxGroups || egressContains(attachedIDs, profile.ID) {
			continue
		}
		resolved, err := egressResolveProfile(ctx, profile, hostID)
		if err != nil {
			continue
		}
		groups = append(groups, gin.H{"key": fmt.Sprintf("p%d", profile.ID), "egress": resolved.Egress, "nodes": resolved.Nodes})
	}

	bindings := make([]gin.H, 0, len(active))
	for _, attachment := range active {
		groupKey := ""
		if attachment.ProfileID != nil {
			groupKey = fmt.Sprintf("p%d", *attachment.ProfileID)
		}
		bindings = append(bindings, gin.H{
			"instanceName": attachment.IncusName,
			"generation":   attachment.Generation,
			"mode":         attachment.Mode,
			"groupKey":     groupKey,
			"nics":         json.RawMessage(attachment.Nics),
		})
	}

	return gin.H{"schemaVersion": 2, "groups": groups, "bindings": bindings}, nil
}

// egressNetworkInstruction mirrors networkInstruction(hostId): the document the
// node applies, including the current revision so a stale push is rejected.
func egressNetworkInstruction(ctx context.Context, hostID int32) (gin.H, error) {
	policy, err := db.EgressHostNetworkPolicyGet(ctx, hostID)
	if err != nil {
		return nil, err
	}

	var egress lib.EgressPolicy
	var nodes []gin.H = []gin.H{}
	if policy != nil && policy.EgressMode == "profile" && policy.EgressProfileID != nil && *policy.EgressProfileID != 0 {
		profiles, err := db.EgressProfilesByIDs(ctx, []int32{*policy.EgressProfileID})
		if err != nil {
			return nil, err
		}
		if len(profiles) == 0 {
			// Prisma findUniqueOrThrow: the policy references a profile that is gone.
			return nil, fmt.Errorf("egress profile %d no longer exists", *policy.EgressProfileID)
		}
		resolved, err := egressResolveProfile(ctx, profiles[0], hostID)
		if err != nil {
			return nil, err
		}
		egress = resolved.Egress
		nodes = resolved.Nodes
	} else {
		egress = lib.DefaultEgress()
		if policy != nil && policy.EgressMode == "direct" {
			egress.Mode = lib.EgressModeDirect
		} else {
			egress.Mode = lib.EgressModeOff
		}
	}

	instruction, err := egressPackageEgressInstruction(ctx, hostID)
	if err != nil {
		return nil, err
	}
	revision := int32(0)
	if policy != nil {
		revision = policy.Revision
	}
	var firewall any = lib.DefaultFirewall()
	if policy != nil && len(policy.Firewall) > 0 {
		firewall = json.RawMessage(policy.Firewall)
	}
	instruction["revision"] = revision
	instruction["egress"] = egress
	instruction["firewall"] = firewall
	instruction["nodes"] = nodes
	return instruction, nil
}

// ==================== status sanitising ====================

// egressFailureReasons mirrors the failureReasons allow-list in sync.ts.
var egressFailureReasons = []string{
	"command_failed", "timeout", "permission_denied", "missing_file", "config_invalid",
	"reality_key_invalid", "reality_id_invalid", "tls_config_invalid", "unsupported_transport",
}

// egressGroupStates mirrors the accepted group states.
var egressGroupStates = []string{"ready", "blocked", "error"}

// egressGroupErrors mirrors the accepted group error codes.
var egressGroupErrors = []string{"none", "preflight", "network", "xray", "health"}

// egressReportStates mirrors the accepted overall states.
var egressReportStates = []string{"applied", "applying", "error", "unsupported", "idle"}

// egressReportErrors mirrors the accepted overall error codes.
var egressReportErrors = []string{
	"none", "preflight", "incus", "acl", "xray", "network", "unsupported-nic",
	"unsupported-platform", "restore",
}

// egressReportErrorStages mirrors the accepted error stages.
var egressReportErrorStages = []string{
	"extract", "config", "start", "health", "preflight", "incus", "acl", "xray",
	"network", "unsupported-nic", "unsupported-platform", "restore",
}

// egressStringIn reports whether value is exactly one of the allowed strings.
func egressStringIn(values []string, candidate any) (string, bool) {
	text, ok := candidate.(string)
	if !ok {
		return "", false
	}
	for _, value := range values {
		if value == text {
			return text, true
		}
	}
	return "", false
}

// egressSanitizeNetworkStatus mirrors sanitizeNetworkStatus(status): it is the only
// writer of host_network_policies.status, so an arbitrary node report never
// reaches the panel untouched.
func egressSanitizeNetworkStatus(report any) (gin.H, error) {
	object, ok := report.(map[string]any)
	if !ok {
		return nil, errors.New("Invalid network status")
	}
	appliedRevision, ok := object["appliedRevision"].(float64)
	if !ok || !egressIsSafeInteger(appliedRevision) || appliedRevision < 0 {
		return nil, errors.New("Invalid network revision")
	}

	groups := make([]gin.H, 0, 4)
	if rawGroups, ok := object["groups"].([]any); ok {
		for _, item := range rawGroups {
			record, ok := item.(map[string]any)
			if !ok {
				continue
			}
			key, keyOK := record["key"].(string)
			state, stateOK := egressStringIn(egressGroupStates, record["state"])
			if !keyOK || !egressEgressGroupKeyPattern.MatchString(key) || !stateOK {
				continue
			}
			errorCode, _ := egressStringIn(egressGroupErrors, record["error"])
			if errorCode == "" {
				errorCode = "network"
			}
			groups = append(groups, gin.H{"key": key, "state": state, "error": errorCode})
			if len(groups) == 16 {
				break
			}
		}
	}

	bindings := make([]gin.H, 0, 8)
	if rawBindings, ok := object["bindings"].([]any); ok {
		for _, item := range rawBindings {
			record, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, nameOK := record["instanceName"].(string)
			generation, generationOK := record["generation"].(float64)
			if !nameOK || jsStringLength(name) > 100 || !generationOK || !egressIsSafeInteger(generation) || generation <= 0 {
				continue
			}
			bindings = append(bindings, gin.H{"instanceName": name, "generation": generation})
			if len(bindings) == 10000 {
				break
			}
		}
	}

	state, _ := egressStringIn(egressReportStates, egressJSString(object["state"]))
	if state == "" {
		state = "error"
	}
	errorCode, _ := egressStringIn(egressReportErrors, egressJSString(object["error"]))
	if errorCode == "" {
		errorCode = "network"
	}
	errorStage, _ := egressStringIn(egressReportErrorStages, egressJSString(object["errorStage"]))
	errorDetail, _ := egressStringIn(egressFailureReasons, egressJSString(object["errorDetail"]))

	var activeNodeID any
	if number, ok := object["activeNodeId"].(float64); ok && egressIsSafeInteger(number) {
		activeNodeID = number
	}

	managedNetworks := make([]any, 0, 8)
	if rawNetworks, ok := object["managedNetworks"].([]any); ok {
		for _, item := range rawNetworks {
			text, ok := item.(string)
			if !ok || !egressManagedNetworkPattern.MatchString(text) {
				continue
			}
			managedNetworks = append(managedNetworks, text)
			if len(managedNetworks) == 64 {
				break
			}
		}
	}

	healthyNodeIDs := make([]any, 0, 8)
	if rawHealthy, ok := object["healthyNodeIds"].([]any); ok {
		for _, item := range rawHealthy {
			number, ok := item.(float64)
			if !ok || !egressIsSafeInteger(number) || number <= 0 {
				continue
			}
			healthyNodeIDs = append(healthyNodeIDs, number)
			if len(healthyNodeIDs) == 16 {
				break
			}
		}
	}

	return gin.H{
		"groups":          groups,
		"bindings":        bindings,
		"state":           state,
		"error":           errorCode,
		"errorStage":      errorStage,
		"errorDetail":     errorDetail,
		"xrayAvailable":   object["xrayAvailable"] == true,
		"activeNodeId":    activeNodeID,
		"managedNetworks": managedNetworks,
		"routedFallback":  object["routedFallback"] == true,
		"healthyNodeIds":  healthyNodeIDs,
	}, nil
}

// egressSanitizeProbeResults mirrors sanitizeProbeResults(raw, nodeIds).
func egressSanitizeProbeResults(raw json.RawMessage, nodeIDs []int32) ([]gin.H, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}
	items, ok := object["items"].([]any)
	if !ok || len(items) != len(nodeIDs) {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}

	seen := make(map[float64]bool, len(items))
	records := make(map[float64]map[string]any, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
		}
		nodeID, ok := record["nodeId"].(float64)
		if !ok || !egressIsSafeInteger(nodeID) || !egressContains(nodeIDs, int32(nodeID)) {
			return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
		}
		if seen[nodeID] {
			return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
		}
		seen[nodeID] = true
		records[nodeID] = record
	}
	if len(seen) != len(nodeIDs) {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}

	probeErrors := []string{"timeout", "handshake", "http", "xray", "unsupported", "network"}
	result := make([]gin.H, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		record := records[float64(id)]
		success := record["success"] == true
		var latency any
		if number, ok := record["latencyMs"].(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) {
			clamped := math.Max(0, math.Min(120000, math.Round(number)))
			latency = clamped
		}
		errorCode := ""
		if !success {
			errorCode, _ = egressStringIn(probeErrors, egressJSString(record["error"]))
			if errorCode == "" {
				errorCode = "network"
			}
		}
		errorDetail := ""
		if !success {
			errorDetail, _ = egressStringIn(egressFailureReasons, egressJSString(record["errorDetail"]))
		}
		result = append(result, gin.H{
			"nodeId":      id,
			"success":     success,
			"latencyMs":   latency,
			"error":       errorCode,
			"errorDetail": errorDetail,
		})
	}
	return result, nil
}

// egressDecodeProxyConfig mirrors decodeProxyConfig(encrypted).
func egressDecodeProxyConfig(encrypted string) (any, error) {
	plain := lib.DecryptSensitiveData(encrypted)
	if plain == "" {
		return nil, errors.New("Proxy configuration cannot be decrypted")
	}
	var value any
	if err := json.Unmarshal([]byte(plain), &value); err != nil {
		return nil, err
	}
	return value, nil
}

// ==================== push ====================

// egressNetworkPushLocks serialises pushes per host. The backend is single
// process (AGENTS.md invariant), so a process-local mutex reproduces the
// TypeScript's per-host promise chain: two concurrent provisions never supersede
// each other's in-flight revision.
var egressNetworkPushLocks sync.Map // map[int32]*sync.Mutex

// egressAppliedResult mirrors the object applyNetworkPolicy resolves with.
type egressAppliedResult struct {
	Revision        int32 `json:"revision"`
	AppliedRevision int32 `json:"appliedRevision"`
	Status          gin.H `json:"status"`
	Success         bool  `json:"success"`
}

// egressPushNetworkPolicy mirrors pushNetworkPolicy(hostId).
func egressPushNetworkPolicy(ctx context.Context, hostID int32) (*egressAppliedResult, error) {
	value, _ := egressNetworkPushLocks.LoadOrStore(hostID, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	return egressApplyNetworkPolicy(ctx, hostID)
}

// egressApplyNetworkPolicy mirrors applyNetworkPolicy(hostId).
func egressApplyNetworkPolicy(ctx context.Context, hostID int32) (*egressAppliedResult, error) {
	agent, err := db.EgressHostAgentGet(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if agent == nil || !agent.Enabled {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeOffline}
	}

	instruction, err := egressNetworkInstruction(ctx, hostID)
	if err != nil {
		return nil, err
	}
	revision, _ := instruction["revision"].(int32)
	if revision == 0 {
		return nil, nil
	}

	method := "network.apply"
	if bindings, ok := instruction["bindings"].([]gin.H); ok && len(bindings) > 0 {
		method = "network.apply.v2"
	} else if EgressTunnelSupportsControl(hostID, "network.apply.v2") {
		method = "network.apply.v2"
	}
	if err := egressRequireNetworkControl(hostID, method); err != nil {
		return nil, err
	}

	raw, err := EgressTunnelRequestControl(ctx, hostID, method, instruction, 100*time.Second)
	if err != nil {
		return nil, err
	}
	var rawValue any
	if err := json.Unmarshal(raw, &rawValue); err != nil {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}
	status, err := egressSanitizeNetworkStatus(rawValue)
	if err != nil {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}
	rawObject, _ := rawValue.(map[string]any)
	appliedNumber, _ := rawObject["appliedRevision"].(float64)
	if appliedNumber > float64(revision) {
		return nil, &egressTunnelControlError{Code: egressTunnelCodeInvalidResponse}
	}

	statusJSON, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	expected := revision
	if err := db.EgressRecordNetworkStatus(ctx, hostID, int32(appliedNumber), statusJSON, &expected); err != nil {
		return nil, err
	}

	state, _ := status["state"].(string)
	return &egressAppliedResult{
		Revision:        revision,
		AppliedRevision: int32(appliedNumber),
		Status:          status,
		Success:         state == "applied" && int32(appliedNumber) == revision,
	}, nil
}

// ==================== handlers ====================

// egressProxyProbeHosts mirrors GET /api/admin/proxy-probe-hosts.
func egressProxyProbeHosts(c *gin.Context) {
	hosts, err := db.EgressHostsWithAgentCapability(c.Request.Context(), "network-control", 1000)
	if err != nil {
		egressInternal(c, err)
		return
	}
	items := make([]gin.H, 0, len(hosts))
	for _, host := range hosts {
		if !EgressTunnelSupportsControl(host.ID, "proxy.probe") {
			continue
		}
		items = append(items, gin.H{"id": host.ID, "name": host.Name})
	}
	c.JSON(http.StatusOK, gin.H{"hosts": items})
}

// egressProxyNodes mirrors GET /api/admin/proxy-nodes.
func egressProxyNodes(c *gin.Context) {
	nodes, err := db.EgressProxyNodeList(c.Request.Context(), 1000)
	if err != nil {
		egressInternal(c, err)
		return
	}
	items := make([]gin.H, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, gin.H{
			"id":        node.ID,
			"name":      node.Name,
			"protocol":  node.Protocol,
			"server":    node.Server,
			"port":      node.Port,
			"createdAt": egressISOTime(node.CreatedAt),
			"updatedAt": egressISOTime(node.UpdatedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"nodes": items})
}

// egressProxyImport mirrors POST /api/admin/proxy-nodes/import.
func egressProxyImport(c *gin.Context) {
	ctx := c.Request.Context()

	// Route bodyLimit: 600 * 1024. Fastify enforces it while reading the body, so
	// it wins over schema validation. The shared middleware already buffered the
	// payload and recorded its exact length on the request.
	if c.Request.ContentLength > 600*1024 {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": middleware.FastifyTooLargeMessage})
		return
	}

	body, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		respondAJVValidation(c, bodyMessage)
		return
	}
	validated, message := validateAJVObject(body.Value, []string{"confirm"}, []ajvFieldSpec{
		{Name: "content", Kind: ajvKindString, MinLength: -1, MaxLength: 524288},
		{Name: "subscriptionUrl", Kind: ajvKindString, MinLength: -1, MaxLength: 2048},
		{Name: "confirm", Kind: ajvKindBoolean},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	content, _ := validated.stringValue("content")
	subscriptionURL, _ := validated.stringValue("subscriptionUrl")
	confirm := validated.boolValue("confirm")

	importContent, err := egressImportContent(ctx, content, subscriptionURL)
	if err != nil {
		egressThrowable(c, err)
		return
	}

	parsed, err := egressParseProxyImport(importContent)
	if err != nil {
		egressThrowable(c, err)
		return
	}

	if !confirm {
		nodes := make([]gin.H, 0, len(parsed.Nodes))
		for _, node := range parsed.Nodes {
			nodes = append(nodes, gin.H{
				"name":        node.Name,
				"protocol":    node.Protocol,
				"server":      node.Server,
				"port":        node.Port,
				"fingerprint": node.Fingerprint,
			})
		}
		c.JSON(http.StatusOK, gin.H{"nodes": nodes, "errors": parsed.Errors})
		return
	}

	added, duplicates := 0, 0
	for _, node := range parsed.Nodes {
		ciphertext, err := lib.EncryptSensitiveData(egressJSONStringify(node.Outbound))
		if err != nil {
			egressInternal(c, err)
			return
		}
		created, err := db.EgressProxyNodeInsert(ctx, db.EgressProxyNode{
			Name:            node.Name,
			Protocol:        node.Protocol,
			Server:          node.Server,
			Port:            int32(node.Port),
			Fingerprint:     node.Fingerprint,
			ConfigEncrypted: ciphertext,
		})
		if err != nil {
			egressInternal(c, err)
			return
		}
		added += int(created)
		duplicates += 1 - int(created)
	}

	userID := int64(middleware.CurrentUser(c).ID)
	db.CreateLog(ctx, &userID, db.LogModuleHost, "host.proxy.import",
		fmt.Sprintf("Imported proxy nodes: added=%d, duplicate=%d, invalid=%d", added, duplicates, len(parsed.Errors)),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"added": added, "duplicates": duplicates, "errors": parsed.Errors})
}

// egressImportContent mirrors importContent(body).
func egressImportContent(ctx context.Context, content, subscriptionURL string) (string, error) {
	if (content != "") == (subscriptionURL != "") {
		return "", &lib.NetworkInputError{Message: "Provide content or a subscription URL"}
	}
	if content != "" {
		return content, nil
	}
	return egressDownloadSubscription(ctx, subscriptionURL)
}

// egressDownloadSubscription mirrors the redirect loop of importContent.
func egressDownloadSubscription(ctx context.Context, rawURL string) (string, error) {
	current := rawURL
	for redirects := 0; redirects <= 3; redirects++ {
		response, cancel, err := egressFetchSubscription(ctx, current)
		if err != nil {
			return "", egressSubscriptionError(err)
		}

		status := response.StatusCode
		if status == http.StatusMovedPermanently || status == http.StatusFound ||
			status == http.StatusSeeOther || status == http.StatusTemporaryRedirect ||
			status == http.StatusPermanentRedirect {
			location := response.Header.Get("Location")
			response.Body.Close()
			cancel()
			if location == "" || redirects == 3 {
				return "", &lib.NetworkInputError{Message: "Subscription redirect limit exceeded"}
			}
			base, baseErr := url.Parse(current)
			reference, referenceErr := url.Parse(location)
			if baseErr != nil || referenceErr != nil {
				return "", &lib.NetworkInputError{Message: "Invalid subscription redirect"}
			}
			current = base.ResolveReference(reference).String()
			continue
		}

		// `!response.ok || !response.body`
		if status < 200 || status > 299 || status == http.StatusNoContent ||
			status == http.StatusResetContent || status == http.StatusNotModified {
			response.Body.Close()
			cancel()
			return "", &lib.NetworkInputError{Message: fmt.Sprintf("Unable to download subscription (HTTP %d)", status)}
		}

		limited := io.LimitReader(response.Body, 512*1024+1)
		payload, readErr := io.ReadAll(limited)
		response.Body.Close()
		cancel()
		if readErr != nil {
			return "", &lib.NetworkInputError{Message: "Unable to read subscription"}
		}
		if len(payload) > 512*1024 {
			return "", &lib.NetworkInputError{Message: "Subscription exceeds 512 KiB"}
		}
		return strings.ToValidUTF8(string(payload), "\uFFFD"), nil
	}
	return "", &lib.NetworkInputError{Message: "Unable to download subscription"}
}

// egressFetchSubscription performs one manual-redirect GET through the
// SSRF-guarded, connect-time-revalidating client.
func egressFetchSubscription(ctx context.Context, rawURL string) (*http.Response, context.CancelFunc, error) {
	if _, err := lib.AssertSafeHTTPURL(ctx, rawURL, "Proxy subscription"); err != nil {
		return nil, func() {}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	client := *lib.SafeOutboundHTTPClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return response, cancel, nil
}

// egressSubscriptionError mirrors the catch block of the download loop.
func egressSubscriptionError(err error) error {
	var validation *lib.OutboundTargetValidationError
	if errors.As(err, &validation) {
		// safeFetch validates before the request; Node sees a plain error with no
		// `code`, so the message carries no suffix.
		return &lib.NetworkInputError{Message: "Unable to download subscription"}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &lib.NetworkInputError{Message: "Subscription download timed out"}
	}
	if code := egressNetworkErrorCode(err); code != "" {
		return &lib.NetworkInputError{Message: "Unable to download subscription (" + code + ")"}
	}
	return &lib.NetworkInputError{Message: "Unable to download subscription"}
}

// egressNetworkErrorCode mirrors the safeCodes allow-list for socket errors.
func egressNetworkErrorCode(err error) string {
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		switch {
		case dnsError.IsNotFound:
			return "ENOTFOUND"
		case dnsError.IsTimeout:
			return "ETIMEDOUT"
		case dnsError.IsTemporary:
			return "EAI_AGAIN"
		}
		return ""
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.ECONNRESET):
		return "ECONNRESET"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "ETIMEDOUT"
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH"
	}
	return ""
}

// egressProxyNodeDelete mirrors DELETE /api/admin/proxy-nodes/:id.
func egressProxyNodeDelete(c *gin.Context) {
	ctx := c.Request.Context()
	nodeID, ok := egressRouteID(c)
	if !ok {
		return
	}

	state, err := db.EgressProxyNodeDelete(ctx, nodeID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	switch state {
	case "used":
		c.JSON(http.StatusConflict, gin.H{"error": "代理节点仍被节点出口或套餐出口策略引用，无法删除"})
		return
	case "missing":
		c.JSON(http.StatusNotFound, gin.H{"error": "代理节点不存在"})
		return
	}

	userID := int64(middleware.CurrentUser(c).ID)
	db.CreateLog(ctx, &userID, db.LogModuleHost, "host.proxy.delete",
		fmt.Sprintf("Deleted proxy node %d", nodeID), db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// egressNetworkPolicy mirrors GET /api/hosts/:id/network-policy.
func egressNetworkPolicy(c *gin.Context) {
	ctx := c.Request.Context()
	hostID, ok := egressParamRouteID(c, "hostId")
	if !ok {
		return
	}

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "宿主节点不存在"})
		return
	}
	agent, err := db.EgressHostAgentGet(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	policy, err := db.EgressHostNetworkPolicyGet(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}

	var policyJSON gin.H
	if policy != nil {
		policyJSON = gin.H{
			"hostId":          policy.HostID,
			"revision":        policy.Revision,
			"egressMode":      policy.EgressMode,
			"egressProfileId": policy.EgressProfileID,
			"firewall":        json.RawMessage(policy.Firewall),
			"appliedRevision": policy.AppliedRevision,
			"status":          json.RawMessage(policy.Status),
			"reportedAt":      egressNullableISOTime(policy.ReportedAt),
			"updatedAt":       egressISOTime(policy.UpdatedAt),
		}
	} else {
		policyJSON = gin.H{
			"hostId":          host.ID,
			"revision":        0,
			"appliedRevision": 0,
			"egressMode":      "off",
			"egressProfileId": nil,
			"firewall":        lib.DefaultFirewall(),
			"status":          gin.H{},
			"reportedAt":      nil,
		}
	}

	var agentJSON any
	networkControlReady := false
	if agent != nil {
		agentJSON = gin.H{
			"enabled":      agent.Enabled,
			"version":      agent.Version,
			"capabilities": json.RawMessage(agent.Capabilities),
			"lastSeenAt":   egressNullableISOTime(agent.LastSeenAt),
		}
		networkControlReady = agent.Enabled && EgressTunnelSupportsControl(hostID, "network.apply")
	}

	c.JSON(http.StatusOK, gin.H{
		"policy":              policyJSON,
		"agent":               agentJSON,
		"networkControlReady": networkControlReady,
	})
}

// egressNetworkPolicyUpdate mirrors PUT /api/hosts/:id/network-policy/:section for
// both sections the TypeScript registers.
func egressNetworkPolicyUpdate(c *gin.Context, section string) {
	ctx := c.Request.Context()
	hostID, ok := egressParamRouteID(c, "hostId")
	if !ok {
		return
	}

	body, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		respondAJVValidation(c, bodyMessage)
		return
	}
	validated, message := validateAJVObject(body.Value, []string{"revision", "value"}, []ajvFieldSpec{
		{Name: "revision", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 2147483646},
		{Name: "value", Kind: ajvKindObject, MinLength: -1, MaxLength: -1},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	revisionRaw, _ := validated.Values["revision"].(float64)
	revision := int32(revisionRaw)
	value := validated.Values["value"]

	var selection *db.EgressNetworkBinding
	var firewallJSON []byte
	if section == "firewall" {
		policy, err := lib.ValidateFirewall(value)
		if err != nil {
			egressThrowable(c, err)
			return
		}
		encoded, err := json.Marshal(policy)
		if err != nil {
			egressInternal(c, err)
			return
		}
		firewallJSON = encoded
	} else {
		input, ok := value.(map[string]any)
		if !ok {
			egressThrowable(c, &lib.NetworkInputError{Message: "Invalid egress binding"})
			return
		}
		rawMode := input["egressMode"]
		rawModeString := egressJSString(rawMode)
		if rawModeString != "off" && rawModeString != "direct" && rawModeString != "profile" {
			egressThrowable(c, &lib.NetworkInputError{Message: "Invalid egress binding"})
			return
		}
		// `input.egressProfileId ?? null`
		var profileValue any
		if raw, present := input["egressProfileId"]; present {
			profileValue = raw
		}
		modeIsProfile := false
		if text, ok := rawMode.(string); ok && text == "profile" {
			modeIsProfile = true
		}
		var profileID *int32
		if profileValue != nil {
			number, ok := profileValue.(float64)
			if !ok || !egressIsSafeInteger(number) {
				egressThrowable(c, &lib.NetworkInputError{Message: "Invalid egress profile"})
				return
			}
			converted := int32(number)
			profileID = &converted
		}
		if modeIsProfile {
			if profileID == nil || *profileID <= 0 {
				egressThrowable(c, &lib.NetworkInputError{Message: "Invalid egress profile"})
				return
			}
		} else if profileID != nil {
			egressThrowable(c, &lib.NetworkInputError{Message: "Invalid egress profile"})
			return
		}
		selection = &db.EgressNetworkBinding{EgressMode: rawModeString, EgressProfileID: profileID}
	}

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "宿主节点不存在"})
		return
	}
	agent, err := db.EgressHostAgentGet(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	if agent == nil || !agent.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "请先启用宿主 Agent"})
		return
	}
	if err := egressRequireNetworkControl(hostID, "network.apply"); err != nil {
		failureStatus, failureMessage := egressControlFailure(err)
		c.JSON(failureStatus, gin.H{"error": failureMessage})
		return
	}

	defaultFirewall, err := json.Marshal(lib.DefaultFirewall())
	if err != nil {
		egressInternal(c, err)
		return
	}
	state, err := db.EgressNetworkPolicyUpdate(ctx, db.EgressNetworkPolicyUpdateInput{
		HostID:           hostID,
		ExpectedRevision: revision,
		Selection:        selection,
		Firewall:         firewallJSON,
		DefaultFirewall:  defaultFirewall,
	})
	if err != nil {
		egressInternal(c, err)
		return
	}
	switch state {
	case "missing":
		c.JSON(http.StatusNotFound, gin.H{"error": "宿主节点不存在"})
		return
	case "conflict":
		c.JSON(http.StatusConflict, gin.H{"error": "配置已更新，请刷新后重试"})
		return
	case "nodes":
		c.JSON(http.StatusBadRequest, gin.H{"error": "出口策略不存在或未授权该宿主节点"})
		return
	}

	userID := int64(middleware.CurrentUser(c).ID)
	db.CreateLog(ctx, &userID, db.LogModuleHost, "host.network."+section,
		fmt.Sprintf("Updated host %d %s policy", hostID, section), db.LogResultSuccess, db.CreateLogOptions{})

	applied, err := egressPushNetworkPolicy(ctx, hostID)
	if err != nil {
		failureStatus, failureMessage := egressControlFailure(err)
		c.JSON(failureStatus, gin.H{"error": failureMessage, "revision": revision + 1})
		return
	}
	if applied == nil || !applied.Success {
		response := gin.H{"error": "节点配置应用失败，请查看下方应用状态"}
		if applied != nil {
			response["revision"] = applied.Revision
			response["appliedRevision"] = applied.AppliedRevision
			response["status"] = applied.Status
			response["success"] = applied.Success
		}
		c.JSON(http.StatusUnprocessableEntity, response)
		return
	}
	c.JSON(http.StatusOK, applied)
}

// egressProxyProbes mirrors POST /api/admin/proxy-probes.
func egressProxyProbes(c *gin.Context) {
	ctx := c.Request.Context()

	body, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		respondAJVValidation(c, bodyMessage)
		return
	}
	object, ok := body.Value.(map[string]any)
	if !ok {
		respondAJVValidation(c, "must be object")
		return
	}
	for _, field := range []string{"hostId", "nodeIds", "probeUrl"} {
		if _, present := object[field]; !present {
			respondAJVValidation(c, field+" is required")
			return
		}
	}
	// AJV validates the properties in schema order: hostId, nodeIds, probeUrl. The
	// shared replica has no array kind, so nodeIds is validated by the dedicated
	// helper in the middle of that order.
	hostIDValue, message := ajvCoerceField(
		ajvFieldSpec{Name: "hostId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
		object["hostId"])
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	hostIDRaw, _ := hostIDValue.(float64)
	hostID := int32(hostIDRaw)

	if _, nodeMessage := egressNodeIDsValue(body.Value, "nodeIds", 1, 10); nodeMessage != "" {
		respondAJVValidation(c, nodeMessage)
		return
	}
	probeURLValue, message := ajvCoerceField(
		ajvFieldSpec{Name: "probeUrl", Kind: ajvKindString, MinLength: -1, MaxLength: 2048},
		object["probeUrl"])
	if message != "" {
		respondAJVValidation(c, message)
		return
	}

	// `ids(request.body.nodeIds, 10)` then `probeUrl(request.body.probeUrl)`
	nodeIDs, err := egressPolicyIDs(egressRawArrayFromBody(body.Value, "nodeIds"), egressMaxNodeIDs)
	if err != nil {
		egressThrowable(c, err)
		return
	}
	probeURL, err := egressPolicyProbeURL(probeURLValue)
	if err != nil {
		egressThrowable(c, err)
		return
	}
	if _, err := lib.AssertSafeHTTPURL(ctx, probeURL, "Probe URL"); err != nil {
		egressThrowable(c, err)
		return
	}

	agent, err := db.EgressHostAgentGet(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	if agent == nil || !agent.Enabled || !egressAgentHasCapability(agent.Capabilities, "network-control") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择已安装新版 Xray Agent 的宿主节点"})
		return
	}

	nodes, err := db.EgressProxyNodesByIDs(ctx, nodeIDs)
	if err != nil {
		egressInternal(c, err)
		return
	}
	if len(nodes) != len(nodeIDs) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "代理节点不存在"})
		return
	}
	byID := make(map[int32]db.EgressProxyNode, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}

	c.Header("Cache-Control", "no-store")

	if err := egressRequireNetworkControl(hostID, "proxy.probe"); err != nil {
		failureStatus, failureMessage := egressControlFailure(err)
		c.JSON(failureStatus, gin.H{"error": failureMessage})
		return
	}

	payloadNodes := make([]gin.H, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		outbound, err := egressDecodeProxyConfig(byID[id].ConfigEncrypted)
		if err != nil {
			failureStatus, failureMessage := egressControlFailure(err)
			c.JSON(failureStatus, gin.H{"error": failureMessage})
			return
		}
		payloadNodes = append(payloadNodes, gin.H{"id": id, "outbound": outbound})
	}

	raw, err := EgressTunnelRequestControl(ctx, hostID, "proxy.probe",
		gin.H{"probeUrl": probeURL, "nodes": payloadNodes}, 45*time.Second)
	if err != nil {
		failureStatus, failureMessage := egressControlFailure(err)
		c.JSON(failureStatus, gin.H{"error": failureMessage})
		return
	}
	items, err := egressSanitizeProbeResults(raw, nodeIDs)
	if err != nil {
		failureStatus, failureMessage := egressControlFailure(err)
		c.JSON(failureStatus, gin.H{"error": failureMessage})
		return
	}

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		egressInternal(c, err)
		return
	}
	var hostJSON any
	if host != nil {
		hostJSON = gin.H{"id": host.ID, "name": host.Name}
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "host": hostJSON})
}

// egressAgentHasCapability mirrors
// `Array.isArray(capabilities) && capabilities.includes('network-control')`.
func egressAgentHasCapability(raw []byte, capability string) bool {
	if len(raw) == 0 {
		return false
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return false
	}
	for _, value := range values {
		if text, ok := value.(string); ok && text == capability {
			return true
		}
	}
	return false
}

// egressRawArrayFromBody re-reads a validated array field in its raw JSON shape so
// the policy helper (which expects []any of float64) sees AJV-coerced values.
func egressRawArrayFromBody(value any, field string) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return object[field]
}

// egressNodeIDsValue mirrors AJV's validation of an array field, returning the
// first message in AJV's keyword order (maxItems → minItems → items →
// uniqueItems) and the coerced integer values.
func egressNodeIDsValue(body any, field string, minItems, maxItems int) ([]int32, string) {
	object, ok := body.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	raw := object[field]
	array, ok := raw.([]any)
	if !ok {
		if raw == nil {
			array = []any{nil}
		} else {
			array = []any{raw}
		}
	}
	if len(array) > maxItems {
		return nil, fmt.Sprintf("must NOT have more than %d items", maxItems)
	}
	if len(array) < minItems {
		return nil, fmt.Sprintf("must NOT have fewer than %d items", minItems)
	}
	ids := make([]int32, 0, len(array))
	for _, item := range array {
		number, ok := ajvCoerceInteger(item)
		if !ok {
			return nil, "must be integer"
		}
		if message := ajvIntegerBoundsMessage(ajvFieldSpec{Minimum: 1, Maximum: 2147483647}, number); message != "" {
			return nil, message
		}
		ids = append(ids, int32(number))
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[i] == ids[j] {
				return nil, fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", j, i)
			}
		}
	}
	return ids, ""
}

// ==================== proxy import parser ====================

// egressEgressGroupKeyPattern mirrors /^p[1-9]\d*$/.
var egressEgressGroupKeyPattern = regexp.MustCompile(`^p[1-9]\d*$`)

// egressManagedNetworkPattern mirrors /^[A-Za-z0-9_.:-]{1,64}$/.
var egressManagedNetworkPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// egressImportedProxy mirrors ImportedProxy.
type egressImportedProxy struct {
	Name        string
	Protocol    string
	Server      string
	Port        int
	Outbound    map[string]any
	Fingerprint string
}

// egressImportError mirrors `{ index, error }`.
type egressImportError struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// egressImportResult mirrors `{ nodes, errors }`.
type egressImportResult struct {
	Nodes  []egressImportedProxy
	Errors []egressImportError
}

// egressSupportedProtocols mirrors `supported`.
var egressSupportedProtocols = []string{"socks", "http", "vless", "vmess", "trojan", "shadowsocks"}

// egressShadowsocksCiphers mirrors the accepted Shadowsocks methods.
var egressShadowsocksCiphers = []string{
	"aes-128-gcm", "aes-256-gcm", "chacha20-poly1305", "chacha20-ietf-poly1305",
	"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
}

// egressImportControlChars mirrors /[\x00-\x1f]/.
var egressImportControlChars = regexp.MustCompile(`[\x00-\x1f]`)

// egressProxyServerPattern mirrors /^[\w.:[\]-]+$/.
var egressProxyServerPattern = regexp.MustCompile(`^[\w.:\[\]-]+$`)

// egressUUIDPattern mirrors the vless/vmess credential check.
var egressUUIDPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// egressBase64Pattern mirrors /^[A-Za-z0-9+/_=\s-]+$/.
var egressBase64Pattern = regexp.MustCompile(`^[A-Za-z0-9+/_=\s-]+$`)

// egressSupportedProtocol reports membership in the protocol allow-list.
func egressSupportedProtocol(protocol string) bool {
	for _, candidate := range egressSupportedProtocols {
		if candidate == protocol {
			return true
		}
	}
	return false
}

// egressImportText mirrors `text(value, label, max)`.
func egressImportText(value any, label string, max int) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) == 0 || jsStringLength(text) > max || egressImportControlChars.MatchString(text) {
		return "", &lib.NetworkInputError{Message: "Invalid " + label}
	}
	return text, nil
}

// egressImportObject mirrors `object(value)` from policy.ts.
func egressImportObject(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, &lib.NetworkInputError{Message: "Expected an object"}
	}
	return object, nil
}

// egressImportEndpoint mirrors `endpoint(address, port)`.
func egressImportEndpoint(address, port any) (string, int, error) {
	server, err := egressImportText(address, "server", 253)
	if err != nil {
		return "", 0, err
	}
	server = strings.TrimSuffix(strings.TrimPrefix(server, "["), "]")
	if !egressProxyServerPattern.MatchString(server) {
		return "", 0, &lib.NetworkInputError{Message: "Invalid proxy server"}
	}
	number := egressJSNumber(port)
	if !egressIsInteger(number) || number < 1 || number > 65535 {
		return "", 0, &lib.NetworkInputError{Message: "Invalid proxy port"}
	}
	return server, int(number), nil
}

// egressImportPickStrings mirrors `pickStrings(input, keys)`.
func egressImportPickStrings(input map[string]any, keys []string) (map[string]any, error) {
	result := map[string]any{}
	for _, key := range keys {
		value, present := input[key]
		if !present || value == "" {
			continue
		}
		text, err := egressImportText(value, key, 2048)
		if err != nil {
			return nil, err
		}
		result[key] = text
	}
	return result, nil
}

// egressImportStreamSettings mirrors streamSettings(value).
func egressImportStreamSettings(value any, present bool) (map[string]any, error) {
	if !present {
		return nil, nil
	}
	input, err := egressImportObject(value)
	if err != nil {
		return nil, err
	}
	allowedTop := []string{
		"network", "security", "tlsSettings", "realitySettings", "wsSettings",
		"grpcSettings", "httpupgradeSettings", "xhttpSettings", "tcpSettings",
	}
	for key := range input {
		if !egressStringInSlice(allowedTop, key) {
			return nil, &lib.NetworkInputError{Message: "Unsupported stream option (local files and socket overrides are not imported)"}
		}
	}

	network := "tcp"
	if raw, present := input["network"]; present && raw != nil {
		network = egressJSString(raw)
	}
	security := "none"
	if raw, present := input["security"]; present && raw != nil {
		security = egressJSString(raw)
	}
	if !egressStringInSlice([]string{"tcp", "raw", "ws", "grpc", "httpupgrade", "xhttp"}, network) ||
		!egressStringInSlice([]string{"none", "tls", "reality"}, security) {
		return nil, &lib.NetworkInputError{Message: "Unsupported Xray transport or security"}
	}

	result := map[string]any{"network": network, "security": security}

	if raw, present := input["tcpSettings"]; present && egressJSTruthy(raw) {
		settings, err := egressImportObject(raw)
		if err != nil {
			return nil, err
		}
		if len(settings) > 0 {
			return nil, &lib.NetworkInputError{Message: "TCP header settings are not supported"}
		}
	}

	if security == "tls" {
		raw, present := input["tlsSettings"]
		if !present || raw == nil {
			raw = map[string]any{}
		}
		tls, err := egressImportObject(raw)
		if err != nil {
			return nil, err
		}
		for key := range tls {
			if !egressStringInSlice([]string{"serverName", "fingerprint", "alpn", "allowInsecure"}, key) {
				return nil, &lib.NetworkInputError{Message: "Unsupported TLS option"}
			}
		}
		picked, err := egressImportPickStrings(tls, []string{"serverName", "fingerprint"})
		if err != nil {
			return nil, err
		}
		if rawInsecure, present := tls["allowInsecure"]; present && rawInsecure != nil {
			flag, ok := rawInsecure.(bool)
			if !ok {
				return nil, &lib.NetworkInputError{Message: "Invalid TLS verification setting"}
			}
			picked["allowInsecure"] = flag
		}
		if rawALPN, present := tls["alpn"]; present && rawALPN != nil {
			list, ok := rawALPN.([]any)
			if !ok || len(list) > 8 {
				return nil, &lib.NetworkInputError{Message: "Invalid ALPN"}
			}
			values := make([]any, 0, len(list))
			for _, item := range list {
				text, err := egressImportText(item, "ALPN", 64)
				if err != nil {
					return nil, err
				}
				values = append(values, text)
			}
			picked["alpn"] = values
		}
		result["tlsSettings"] = picked
	}

	if security == "reality" {
		reality, err := egressImportObject(input["realitySettings"])
		if err != nil {
			return nil, err
		}
		for key := range reality {
			if !egressStringInSlice([]string{"serverName", "fingerprint", "publicKey", "shortId", "spiderX", "password"}, key) {
				return nil, &lib.NetworkInputError{Message: "Unsupported Reality option"}
			}
		}
		picked, err := egressImportPickStrings(reality, []string{"serverName", "fingerprint", "publicKey", "shortId", "spiderX", "password"})
		if err != nil {
			return nil, err
		}
		if !egressJSTruthy(reality["publicKey"]) && !egressJSTruthy(reality["password"]) {
			return nil, &lib.NetworkInputError{Message: "Reality public key is required"}
		}
		result["realitySettings"] = picked
	}

	transports := []struct {
		transport string
		field     string
		keys      []string
	}{
		{"ws", "wsSettings", []string{"path"}},
		{"grpc", "grpcSettings", []string{"serviceName", "authority"}},
		{"httpupgrade", "httpupgradeSettings", []string{"path", "host"}},
		{"xhttp", "xhttpSettings", []string{"path", "host", "mode"}},
	}
	for _, transport := range transports {
		if network != transport.transport {
			continue
		}
		raw, present := input[transport.field]
		if !present || raw == nil {
			raw = map[string]any{}
		}
		settings, err := egressImportObject(raw)
		if err != nil {
			return nil, err
		}
		for key := range settings {
			if egressStringInSlice(transport.keys, key) {
				continue
			}
			if transport.transport == "ws" && key == "headers" {
				continue
			}
			return nil, &lib.NetworkInputError{Message: "Unsupported " + transport.transport + " option"}
		}
		picked, err := egressImportPickStrings(settings, transport.keys)
		if err != nil {
			return nil, err
		}
		if transport.transport == "ws" {
			if rawHeaders, present := settings["headers"]; present && egressJSTruthy(rawHeaders) {
				headers, err := egressImportObject(rawHeaders)
				if err != nil {
					return nil, err
				}
				values := map[string]any{}
				for key, item := range headers {
					if strings.ToLower(key) != "host" {
						return nil, &lib.NetworkInputError{Message: "Only the WebSocket Host header is supported"}
					}
					text, err := egressImportText(item, "Host header", 253)
					if err != nil {
						return nil, err
					}
					values[key] = text
				}
				picked["headers"] = values
			}
		}
		result[transport.field] = picked
	}

	return result, nil
}

// egressJSTruthy mirrors JavaScript truthiness for decoded JSON values.
func egressJSTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case []any:
		return true
	case map[string]any:
		return true
	}
	return true
}

// egressStringInSlice reports membership.
func egressStringInSlice(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// egressImportNormalizeOutbound mirrors normalizeOutbound(value, name?).
func egressImportNormalizeOutbound(value any, name *string) (egressImportedProxy, error) {
	input, err := egressImportObject(value)
	if err != nil {
		return egressImportedProxy{}, err
	}
	protocol, _ := input["protocol"].(string)
	if !egressSupportedProtocol(protocol) {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported proxy protocol"}
	}
	for key := range input {
		if !egressStringInSlice([]string{"tag", "protocol", "settings", "streamSettings"}, key) {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported outbound option"}
		}
	}

	settings, err := egressImportObject(input["settings"])
	if err != nil {
		return egressImportedProxy{}, err
	}
	sourceKey := "servers"
	if protocol == "vless" || protocol == "vmess" {
		sourceKey = "vnext"
	}
	source, ok := settings[sourceKey].([]any)
	if !ok || len(source) != 1 {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Each outbound must contain exactly one server"}
	}
	entry, err := egressImportObject(source[0])
	if err != nil {
		return egressImportedProxy{}, err
	}
	serverAddress, serverPort, err := egressImportEndpoint(entry["address"], entry["port"])
	if err != nil {
		return egressImportedProxy{}, err
	}
	server := map[string]any{"address": serverAddress, "port": float64(serverPort)}

	switch protocol {
	case "vless", "vmess":
		users, ok := entry["users"].([]any)
		if !ok || len(users) != 1 {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Exactly one proxy credential is required"}
		}
		user, err := egressImportObject(users[0])
		if err != nil {
			return egressImportedProxy{}, err
		}
		id, err := egressImportText(user["id"], "UUID", 64)
		if err != nil {
			return egressImportedProxy{}, err
		}
		if !egressUUIDPattern.MatchString(id) {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Invalid proxy UUID"}
		}
		if protocol == "vless" && egressJSTruthy(user["encryption"]) && egressJSString(user["encryption"]) != "none" {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported VLESS encryption"}
		}
		if protocol == "vmess" && egressJSTruthy(user["alterId"]) && egressJSNumber(user["alterId"]) != 0 {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Legacy VMess alterId is unsupported"}
		}
		credential := map[string]any{"id": id}
		if protocol == "vless" {
			credential["encryption"] = "none"
			if egressJSTruthy(user["flow"]) {
				flow, err := egressImportText(user["flow"], "flow", 64)
				if err != nil {
					return egressImportedProxy{}, err
				}
				credential["flow"] = flow
			}
		} else {
			security := any("auto")
			if raw, present := user["security"]; present && raw != nil {
				security = raw
			}
			credential["security"] = security
			credential["alterId"] = float64(0)
		}
		server["users"] = []any{credential}

	case "trojan", "shadowsocks":
		password, err := egressImportText(entry["password"], "password", 1024)
		if err != nil {
			return egressImportedProxy{}, err
		}
		server["password"] = password
		if protocol == "shadowsocks" {
			method, _ := entry["method"].(string)
			if !egressStringInSlice(egressShadowsocksCiphers, method) {
				return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported Shadowsocks cipher"}
			}
			server["method"] = method
		}

	default:
		if rawUsers, present := entry["users"]; present {
			users, ok := rawUsers.([]any)
			if !ok || len(users) != 1 {
				return egressImportedProxy{}, &lib.NetworkInputError{Message: "Exactly one proxy credential is supported"}
			}
			user, err := egressImportObject(users[0])
			if err != nil {
				return egressImportedProxy{}, err
			}
			username, err := egressImportText(user["user"], "username", 255)
			if err != nil {
				return egressImportedProxy{}, err
			}
			var password string
			if text, ok := user["pass"].(string); ok && jsStringLength(text) <= 1024 {
				password = text
			} else {
				password, err = egressImportText(user["pass"], "password", 1024)
				if err != nil {
					return egressImportedProxy{}, err
				}
			}
			server["users"] = []any{map[string]any{"user": username, "pass": password}}
		}
	}

	allowedEntry := []string{"address", "port"}
	switch protocol {
	case "vless", "vmess", "socks", "http":
		allowedEntry = append(allowedEntry, "users")
	case "trojan":
		allowedEntry = append(allowedEntry, "password")
	default:
		allowedEntry = append(allowedEntry, "password", "method")
	}
	for key := range entry {
		if !egressStringInSlice(allowedEntry, key) {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported proxy settings"}
		}
	}
	allowedSettings := []string{"servers"}
	if protocol == "vless" || protocol == "vmess" {
		allowedSettings = []string{"vnext"}
	}
	for key := range settings {
		if !egressStringInSlice(allowedSettings, key) {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported proxy settings"}
		}
	}

	streamValue, streamPresent := input["streamSettings"]
	stream, err := egressImportStreamSettings(streamValue, streamPresent)
	if err != nil {
		return egressImportedProxy{}, err
	}
	outbound := map[string]any{
		"protocol": protocol,
		"settings": map[string]any{sourceKey: []any{server}},
	}
	if stream != nil {
		outbound["streamSettings"] = stream
	}
	if jsStringLength(egressJSONStringify(outbound)) > 8192 {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Proxy configuration is too large"}
	}

	var nameValue any
	switch {
	case name != nil && *name != "":
		nameValue = *name
	case egressJSTruthy(input["tag"]):
		// `v.tag` is passed through raw, so a non-string tag fails text() exactly
		// like Node's `text(value, 'name', 200)`.
		nameValue = input["tag"]
	default:
		nameValue = fmt.Sprintf("%s %s:%d", protocol, serverAddress, serverPort)
	}
	finalName, err := egressImportText(nameValue, "name", 200)
	if err != nil {
		return egressImportedProxy{}, err
	}

	sum := sha256.Sum256([]byte(egressJSONStringify(outbound)))
	return egressImportedProxy{
		Name:        finalName,
		Protocol:    protocol,
		Server:      serverAddress,
		Port:        serverPort,
		Outbound:    outbound,
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

// egressImportSingbox mirrors singbox(value).
func egressImportSingbox(value any) (egressImportedProxy, error) {
	input, err := egressImportObject(value)
	if err != nil {
		return egressImportedProxy{}, err
	}
	allowed := []string{
		"type", "tag", "server", "server_port", "username", "password", "uuid", "flow",
		"security", "alter_id", "method", "tls", "transport", "version", "packet_encoding",
	}
	for key := range input {
		if !egressStringInSlice(allowed, key) {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported sing-box outbound option"}
		}
	}
	protocol := egressJSString(input["type"])
	if protocol == "socks" && egressJSTruthy(input["version"]) && egressJSString(input["version"]) != "5" {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Only SOCKS5 is supported"}
	}
	if egressJSTruthy(input["packet_encoding"]) {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported sing-box packet encoding"}
	}

	server := map[string]any{"address": input["server"], "port": input["server_port"]}
	switch {
	case protocol == "vless" || protocol == "vmess":
		server["users"] = []any{map[string]any{
			"id":       input["uuid"],
			"flow":     input["flow"],
			"security": input["security"],
			"alterId":  input["alter_id"],
		}}
	case protocol == "socks" || protocol == "http":
		if egressJSTruthy(input["username"]) {
			pass := any("")
			if raw, present := input["password"]; present && raw != nil {
				pass = raw
			}
			server["users"] = []any{map[string]any{"user": input["username"], "pass": pass}}
		}
	default:
		server["password"] = input["password"]
		if protocol == "shadowsocks" {
			server["method"] = input["method"]
		}
	}

	stream := map[string]any{"network": "tcp", "security": "none"}
	if tlsValue, present := input["tls"]; present && egressJSTruthy(tlsValue) {
		tls, err := egressImportObject(tlsValue)
		if err != nil {
			return egressImportedProxy{}, err
		}
		if egressJSTruthy(tls["enabled"]) {
			for key := range tls {
				if !egressStringInSlice([]string{"enabled", "server_name", "insecure", "alpn", "utls", "reality"}, key) {
					return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported sing-box TLS option"}
				}
			}
			realityEnabled := false
			var reality map[string]any
			if realityValue, present := tls["reality"]; present && egressJSTruthy(realityValue) {
				reality, err = egressImportObject(realityValue)
				if err != nil {
					return egressImportedProxy{}, err
				}
				realityEnabled = egressJSTruthy(reality["enabled"])
			}
			if realityEnabled {
				stream["security"] = "reality"
			} else {
				stream["security"] = "tls"
			}
			common := map[string]any{"serverName": tls["server_name"]}
			if utlsValue, present := tls["utls"]; present && egressJSTruthy(utlsValue) {
				utls, err := egressImportObject(utlsValue)
				if err != nil {
					return egressImportedProxy{}, err
				}
				if egressJSTruthy(utls["enabled"]) {
					common["fingerprint"] = utls["fingerprint"]
				}
			}
			if realityEnabled {
				common["publicKey"] = reality["public_key"]
				common["shortId"] = reality["short_id"]
				stream["realitySettings"] = common
			} else {
				common["allowInsecure"] = tls["insecure"]
				common["alpn"] = tls["alpn"]
				stream["tlsSettings"] = common
			}
		}
	}

	if transportValue, present := input["transport"]; present && egressJSTruthy(transportValue) {
		transport, err := egressImportObject(transportValue)
		if err != nil {
			return egressImportedProxy{}, err
		}
		switch {
		case egressJSString(transport["type"]) == "ws" && egressImportTransportKeysAllowed(transport, []string{"type", "path", "headers"}):
			stream["network"] = "ws"
			stream["wsSettings"] = map[string]any{"path": transport["path"], "headers": transport["headers"]}
		case egressJSString(transport["type"]) == "grpc" && egressImportTransportKeysAllowed(transport, []string{"type", "service_name"}):
			stream["network"] = "grpc"
			stream["grpcSettings"] = map[string]any{"serviceName": transport["service_name"]}
		case egressJSString(transport["type"]) == "httpupgrade" && egressImportTransportKeysAllowed(transport, []string{"type", "path", "host"}):
			stream["network"] = "httpupgrade"
			stream["httpupgradeSettings"] = map[string]any{"path": transport["path"], "host": transport["host"]}
		default:
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported sing-box transport"}
		}
	}

	settingsKey := "servers"
	if protocol == "vless" || protocol == "vmess" {
		settingsKey = "vnext"
	}
	constructed := map[string]any{
		"protocol":       protocol,
		"tag":            input["tag"],
		"settings":       map[string]any{settingsKey: []any{server}},
		"streamSettings": stream,
	}
	// JSON removes absent optional fields before the strict Xray validator runs.
	return egressImportNormalizeOutbound(egressJSONRoundTrip(constructed), nil)
}

// egressImportTransportKeysAllowed mirrors
// `Object.keys(tr).every(k => allowed.includes(k))`.
func egressImportTransportKeysAllowed(transport map[string]any, allowed []string) bool {
	for key := range transport {
		if !egressStringInSlice(allowed, key) {
			return false
		}
	}
	return true
}

// egressImportDecodeBase64 mirrors decode64(value).
func egressImportDecodeBase64(value string) (string, error) {
	if !egressBase64Pattern.MatchString(value) {
		return "", &lib.NetworkInputError{Message: "Invalid base64 data"}
	}
	cleaned := strings.Map(func(char rune) rune {
		if unicode.IsSpace(char) {
			return -1
		}
		return char
	}, value)
	cleaned = strings.NewReplacer("-", "+", "_", "/").Replace(cleaned)
	if remainder := len(cleaned) % 4; remainder != 0 {
		if remainder == 1 {
			return "", &lib.NetworkInputError{Message: "Invalid base64 data"}
		}
		cleaned += strings.Repeat("=", 4-remainder)
	}
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", &lib.NetworkInputError{Message: "Invalid base64 data"}
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD"), nil
}

// egressImportURIQuery mirrors the ordered key/value pairs of a URL query, which
// JS URLSearchParams iterates (Go's url.Values loses the order).
func egressImportURIQuery(raw string) [][2]string {
	pairs := make([][2]string, 0, 8)
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			decodedValue = value
		}
		pairs = append(pairs, [2]string{decodedKey, decodedValue})
	}
	return pairs
}

// egressImportQueryGet mirrors `searchParams.get(key)` (first value).
func egressImportQueryGet(pairs [][2]string, key string) string {
	for _, pair := range pairs {
		if pair[0] == key {
			return pair[1]
		}
	}
	return ""
}

// egressImportQueryHas mirrors `searchParams.has(key)`.
func egressImportQueryHas(pairs [][2]string, key string) bool {
	for _, pair := range pairs {
		if pair[0] == key {
			return true
		}
	}
	return false
}

// egressImportURI mirrors uri(input).
func egressImportURI(input string) (egressImportedProxy, error) {
	if strings.HasPrefix(input, "vmess://") {
		decoded, err := egressImportDecodeBase64(input[len("vmess://"):])
		if err != nil {
			return egressImportedProxy{}, err
		}
		var parsed any
		if err := json.Unmarshal([]byte(decoded), &parsed); err != nil {
			return egressImportedProxy{}, err
		}
		object, err := egressImportObject(parsed)
		if err != nil {
			return egressImportedProxy{}, err
		}
		if network := egressJSString(object["net"]); egressJSTruthy(object["net"]) &&
			network != "tcp" && network != "ws" && network != "grpc" {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported VMess transport"}
		}
		if egressJSTruthy(object["type"]) && egressJSString(object["type"]) != "none" {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported VMess header type"}
		}

		network := "tcp"
		if egressJSTruthy(object["net"]) {
			network = egressJSString(object["net"])
		}
		security := "none"
		if egressJSString(object["tls"]) == "tls" {
			security = "tls"
		}
		stream := map[string]any{"network": network, "security": security}
		if security == "tls" {
			serverName := egressJSString(object["add"])
			if egressJSTruthy(object["sni"]) {
				serverName = egressJSString(object["sni"])
			} else if egressJSTruthy(object["host"]) {
				serverName = egressJSString(object["host"])
			}
			stream["tlsSettings"] = map[string]any{"serverName": serverName}
		}
		if network == "ws" {
			path := "/"
			if egressJSTruthy(object["path"]) {
				path = egressJSString(object["path"])
			}
			ws := map[string]any{"path": path}
			if egressJSTruthy(object["host"]) {
				ws["headers"] = map[string]any{"Host": egressJSString(object["host"])}
			}
			stream["wsSettings"] = ws
		}
		if network == "grpc" {
			serviceName := ""
			if egressJSTruthy(object["path"]) {
				serviceName = egressJSString(object["path"])
			}
			stream["grpcSettings"] = map[string]any{"serviceName": serviceName}
		}

		alterID := float64(0)
		if raw, present := object["aid"]; present && raw != nil {
			alterID = egressJSNumber(raw)
		}
		securityMode := any("auto")
		if raw, present := object["scy"]; present && raw != nil {
			securityMode = raw
		}
		outbound := map[string]any{
			"protocol": "vmess",
			"tag":      object["ps"],
			"settings": map[string]any{"vnext": []any{map[string]any{
				"address": object["add"],
				"port":    object["port"],
				"users": []any{map[string]any{
					"id":       object["id"],
					"alterId":  alterID,
					"security": securityMode,
				}},
			}}},
			"streamSettings": stream,
		}
		return egressImportNormalizeOutbound(egressJSONRoundTrip(outbound), nil)
	}

	value := input
	if strings.HasPrefix(value, "ss://") && !strings.Contains(strings.Split(value[5:], "#")[0], "@") {
		encodedAndName := value[5:]
		encoded := encodedAndName
		name := ""
		if hash := strings.Index(encodedAndName, "#"); hash >= 0 {
			encoded = encodedAndName[:hash]
			name = encodedAndName[hash:]
		}
		decoded, err := egressImportDecodeBase64(encoded)
		if err != nil {
			return egressImportedProxy{}, err
		}
		value = "ss://" + decoded + name
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return egressImportedProxy{}, err
	}
	protocol := parsed.Scheme
	switch parsed.Scheme {
	case "socks5", "socks":
		protocol = "socks"
	case "https":
		protocol = "http"
	case "ss":
		protocol = "shadowsocks"
	}
	if !egressSupportedProtocol(protocol) {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported URI protocol"}
	}

	pairs := egressImportURIQuery(parsed.RawQuery)
	allowedParams := []string{
		"type", "security", "sni", "peer", "fp", "pbk", "sid", "spx", "alpn", "host",
		"path", "serviceName", "mode", "flow", "encryption", "allowInsecure", "headerType",
		"support-x25519mlkem768",
	}
	for _, pair := range pairs {
		if !egressStringInSlice(allowedParams, pair[0]) {
			sanitized := regexp.MustCompile(`[^\w-]`).ReplaceAllString(pair[0], "")
			runes := []rune(sanitized)
			if len(runes) > 40 {
				sanitized = string(runes[:40])
			}
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported URI parameter: " + sanitized}
		}
	}
	if headerType := egressImportQueryGet(pairs, "headerType"); headerType != "" && headerType != "none" {
		return egressImportedProxy{}, &lib.NetworkInputError{Message: "Unsupported TCP header type (only none is supported)"}
	}
	if egressImportQueryHas(pairs, "support-x25519mlkem768") {
		capability := egressImportQueryGet(pairs, "support-x25519mlkem768")
		if capability != "true" && capability != "false" && capability != "1" && capability != "0" {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Invalid Reality capability flag"}
		}
	}

	hostname := parsed.Hostname()
	port := 0
	if rawPort := parsed.Port(); rawPort != "" {
		if number, err := strconv.Atoi(rawPort); err == nil {
			port = number
		}
	}
	if port == 0 {
		switch {
		case parsed.Scheme == "https":
			port = 443
		case protocol == "http":
			port = 80
		default:
			port = 1080
		}
	}

	server := map[string]any{"address": hostname, "port": float64(port)}
	username := ""
	if parsed.User != nil {
		username = parsed.User.Username()
	}
	password, passwordPresent := "", false
	if parsed.User != nil {
		password, passwordPresent = parsed.User.Password()
	}

	switch protocol {
	case "vless", "vmess":
		credential := map[string]any{"id": username}
		encryption := "none"
		if egressImportQueryHas(pairs, "encryption") {
			encryption = egressImportQueryGet(pairs, "encryption")
		}
		if protocol == "vless" {
			credential["encryption"] = encryption
		}
		if flow := egressImportQueryGet(pairs, "flow"); flow != "" {
			credential["flow"] = flow
		}
		server["users"] = []any{credential}
	case "trojan":
		server["password"] = username
	case "shadowsocks":
		var auth string
		if passwordPresent && password != "" {
			auth = username + ":" + password
		} else {
			decoded, err := egressImportDecodeBase64(username)
			if err != nil {
				return egressImportedProxy{}, err
			}
			auth = decoded
		}
		separator := strings.Index(auth, ":")
		if separator < 1 {
			return egressImportedProxy{}, &lib.NetworkInputError{Message: "Invalid Shadowsocks credential"}
		}
		server["method"] = auth[:separator]
		server["password"] = auth[separator+1:]
	default:
		if username != "" {
			server["users"] = []any{map[string]any{"user": username, "pass": password}}
		}
	}

	network := "tcp"
	if value := egressImportQueryGet(pairs, "type"); value != "" {
		network = value
	}
	security := "none"
	if value := egressImportQueryGet(pairs, "security"); value != "" {
		security = value
	} else if protocol == "trojan" || parsed.Scheme == "https" {
		security = "tls"
	}
	stream := map[string]any{"network": network, "security": security}

	if security == "tls" {
		serverName := hostname
		if value := egressImportQueryGet(pairs, "sni"); value != "" {
			serverName = value
		} else if value := egressImportQueryGet(pairs, "peer"); value != "" {
			serverName = value
		}
		tls := map[string]any{"serverName": serverName}
		if value := egressImportQueryGet(pairs, "fp"); value != "" {
			tls["fingerprint"] = value
		}
		if value := egressImportQueryGet(pairs, "alpn"); value != "" {
			parts := strings.Split(value, ",")
			values := make([]any, 0, len(parts))
			for _, part := range parts {
				values = append(values, part)
			}
			tls["alpn"] = values
		}
		tls["allowInsecure"] = egressImportQueryGet(pairs, "allowInsecure") == "1"
		stream["tlsSettings"] = tls
	}
	if security == "reality" {
		serverName := hostname
		if value := egressImportQueryGet(pairs, "sni"); value != "" {
			serverName = value
		}
		fingerprint := egressImportQueryGet(pairs, "fp")
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		reality := map[string]any{
			"serverName":  serverName,
			"fingerprint": fingerprint,
			"publicKey":   egressImportQueryGet(pairs, "pbk"),
		}
		if value := egressImportQueryGet(pairs, "sid"); value != "" {
			reality["shortId"] = value
		}
		if value := egressImportQueryGet(pairs, "spx"); value != "" {
			reality["spiderX"] = value
		}
		stream["realitySettings"] = reality
	}
	if network == "ws" {
		path := egressImportQueryGet(pairs, "path")
		if path == "" {
			path = "/"
		}
		ws := map[string]any{"path": path}
		if value := egressImportQueryGet(pairs, "host"); value != "" {
			ws["headers"] = map[string]any{"Host": value}
		}
		stream["wsSettings"] = ws
	}
	if network == "grpc" {
		grpc := map[string]any{}
		if value := egressImportQueryGet(pairs, "serviceName"); value != "" {
			grpc["serviceName"] = value
		}
		stream["grpcSettings"] = grpc
	}
	if network == "httpupgrade" || network == "xhttp" {
		path := egressImportQueryGet(pairs, "path")
		if path == "" {
			path = "/"
		}
		settings := map[string]any{"path": path}
		if value := egressImportQueryGet(pairs, "host"); value != "" {
			settings["host"] = value
		}
		if network == "xhttp" {
			if value := egressImportQueryGet(pairs, "mode"); value != "" {
				settings["mode"] = value
			}
		}
		stream[network+"Settings"] = settings
	}

	settingsKey := "servers"
	if protocol == "vless" || protocol == "vmess" {
		settingsKey = "vnext"
	}
	outbound := map[string]any{
		"protocol": protocol,
		"settings": map[string]any{settingsKey: []any{server}},
	}
	if parsed.Fragment != "" {
		outbound["tag"] = parsed.Fragment
	}
	outbound["streamSettings"] = stream
	return egressImportNormalizeOutbound(outbound, nil)
}

// egressParseProxyImport mirrors parseProxyImport(input).
func egressParseProxyImport(input string) (egressImportResult, error) {
	if len(input) > 512*1024 {
		return egressImportResult{}, &lib.NetworkInputError{Message: "Import exceeds 512 KiB"}
	}
	content := egressJSTrim(input)
	if content == "" {
		return egressImportResult{}, &lib.NetworkInputError{Message: "Import is empty"}
	}
	if content[0] != '{' && content[0] != '[' && !strings.Contains(content, "://") {
		decoded, err := egressImportDecodeBase64(content)
		if err != nil {
			return egressImportResult{}, err
		}
		content = egressJSTrim(decoded)
	}

	var entries []any
	if content != "" && (content[0] == '{' || content[0] == '[') {
		var parsed any
		if err := json.Unmarshal([]byte(content), &parsed); err != nil {
			return egressImportResult{}, &lib.NetworkInputError{Message: "Invalid configuration JSON"}
		}
		if array, ok := parsed.([]any); ok {
			entries = array
		} else if object, ok := parsed.(map[string]any); ok {
			if outbounds, present := object["outbounds"]; present && outbounds != nil {
				if array, ok := outbounds.([]any); ok {
					entries = array
				} else {
					return egressImportResult{}, &lib.NetworkInputError{Message: "Import must contain at most 200 nodes"}
				}
			} else {
				entries = []any{parsed}
			}
		} else {
			entries = []any{parsed}
		}
	} else {
		lines := strings.Split(content, "\n")
		entries = make([]any, 0, len(lines))
		for _, line := range lines {
			trimmed := egressJSTrim(strings.TrimSuffix(line, "\r"))
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			entries = append(entries, trimmed)
		}
	}
	if len(entries) > 200 {
		return egressImportResult{}, &lib.NetworkInputError{Message: "Import must contain at most 200 nodes"}
	}

	result := egressImportResult{
		Nodes:  make([]egressImportedProxy, 0, len(entries)),
		Errors: make([]egressImportError, 0, 4),
	}
	for index, entry := range entries {
		var node egressImportedProxy
		var err error
		if text, ok := entry.(string); ok {
			node, err = egressImportURI(text)
		} else {
			object, objectErr := egressImportObject(entry)
			if objectErr != nil {
				err = objectErr
			} else if egressJSTruthy(object["type"]) {
				node, err = egressImportSingbox(entry)
			} else {
				node, err = egressImportNormalizeOutbound(entry, nil)
			}
		}
		if err != nil {
			message := "Invalid node configuration"
			var inputError *lib.NetworkInputError
			if errors.As(err, &inputError) {
				message = inputError.Message
			}
			result.Errors = append(result.Errors, egressImportError{Index: index + 1, Error: message})
			continue
		}
		result.Nodes = append(result.Nodes, node)
	}
	return result, nil
}

// RegisterEgress mounts the egress routes (app.ts uses `{ prefix: '/api' }`).
func RegisterEgress(r *gin.Engine) {
	admin := r.Group("/api", middleware.Authenticate(), middleware.RequireAdmin())
	admin.GET("/admin/proxy-probe-hosts", egressProxyProbeHosts)
	admin.GET("/admin/proxy-nodes", egressProxyNodes)
	admin.POST("/admin/proxy-nodes/import", egressProxyImport)
	admin.DELETE("/admin/proxy-nodes/:id", egressProxyNodeDelete)
	admin.GET("/hosts/:hostId/network-policy", egressNetworkPolicy)
	admin.PUT("/hosts/:hostId/network-policy/egress", func(c *gin.Context) { egressNetworkPolicyUpdate(c, "egress") })
	admin.PUT("/hosts/:hostId/network-policy/firewall", func(c *gin.Context) { egressNetworkPolicyUpdate(c, "firewall") })
	admin.POST("/admin/proxy-probes", egressProxyProbes)
}

func init() {
	Register(RegisterEgress)
}
