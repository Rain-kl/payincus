// Port of server/src/routes/agent.ts (mount prefix /api/agent).
//
// This is the HOST AGENT authentication plane (WAVE_D_SPEC §0.2, third of three
// mutually exclusive planes). The credential is an HMAC-SHA256 signature over
// METHOD \n PATH \n TIMESTAMP \n NONCE \n BODY-SHA256 with a per-host shared
// secret (lib/agent_auth.go). Web JWTs and public-API pat_ tokens are NOT accepted
// anywhere in this file: the routes below either call lib.AuthenticateAgent or are
// public one-time-token endpoints with no JWT check at all. The reverse test lives
// in routes/agent_test.go.
//
// Route inventory (12):
//
//	GET  /install.sh                          public (static installer)
//	GET  /distribution                        public (static descriptor)
//	GET  /install-config/:token               public (one-time install token)
//	GET  /hosts/:hostId/status                web user (owner or admin)
//	POST /hosts/:hostId/install-command       web user (owner or admin)
//	GET  /admin/hosts/:hostId/status          web admin
//	POST /admin/hosts/:hostId/credentials     web admin
//	POST /admin/hosts/:hostId/install-command web admin
//	POST /startup-keys                        HMAC
//	POST /security-report                     HMAC
//	POST /heartbeat                           HMAC
//	GET  /tunnel                              HMAC + WebSocket upgrade
//
// Time: every time.Time that reaches SQL is UTC (WAVE_D_SPEC §0.2b).
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== constants ====================

const (
	// agentLogModule mirrors LogModule.HOST.
	agentLogModule = db.LogModuleHost

	// agentMaxInstallURL is maxAgentInstallUrlLength.
	agentMaxInstallURL = 2048

	// Agent heartbeat interval bounds.
	agentDefaultHeartbeatIntervalSeconds = 5
	agentMinHeartbeatIntervalSeconds     = 5
	agentMaxHeartbeatIntervalSeconds     = 3600
	agentMinOfflineThresholdSeconds      = 120

	// Route body limits mirror the Fastify `bodyLimit` options.
	agentStartupKeysBodyLimit = 16 * 1024
	agentSecurityReportLimit  = 128 * 1024

	// agentMaxInt32 bounds a route id that Prisma would bind to an Int4 column.
	agentMaxInt32 = int64(math.MaxInt32)
)

var agentPositiveRouteIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// ==================== process-local state ====================

// caddyInstallRequested / caddyUninstallRequested mirror the two exported sets in
// routes/agent.ts. They are process-local by design (WAVE_D_SPEC §0.1): the
// install/uninstall buttons in the panel set them and the next heartbeat consumes
// them. Losing them on restart is acceptable — the button can be pressed again.
var agentCaddyState = struct {
	sync.Mutex
	install   map[int32]bool
	uninstall map[int32]bool
}{install: map[int32]bool{}, uninstall: map[int32]bool{}}

func agentCaddyRequestInstall(hostID int32) {
	agentCaddyState.Lock()
	agentCaddyState.install[hostID] = true
	agentCaddyState.Unlock()
}

func agentCaddyRequestUninstall(hostID int32) {
	agentCaddyState.Lock()
	agentCaddyState.uninstall[hostID] = true
	agentCaddyState.Unlock()
}

func agentCaddyDelete(set map[int32]bool, hostID int32) {
	agentCaddyState.Lock()
	delete(set, hostID)
	agentCaddyState.Unlock()
}

func agentCaddyHas(set map[int32]bool, hostID int32) bool {
	agentCaddyState.Lock()
	defer agentCaddyState.Unlock()
	return set[hostID]
}

// agentNudgeInstanceTaskWorker is the seam for workers/instanceTaskWorker.ts
// (Wave E). The TypeScript only signals an in-process worker; there is no worker
// process in the Go tree yet, so the nudge is a documented no-op.
var agentNudgeInstanceTaskWorker = func() {}

// ==================== id / string helpers ====================

// agentParsePositiveID mirrors parsePositiveId.
func agentParsePositiveID(value string) (int32, bool) {
	if !agentPositiveRouteIDPattern.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > 9007199254740991 || parsed > agentMaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// agentSanitizeShortString mirrors sanitizeShortString (trim, reject empty,
// truncate to maxLength UTF-16 units).
func agentSanitizeShortString(value any, maxLength int) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	truncated := agentTruncateUTF16(trimmed, maxLength)
	return &truncated
}

// agentTruncateUTF16 mirrors String#slice(0, limit).
func agentTruncateUTF16(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	units := 0
	for index, runeValue := range value {
		width := 1
		if runeValue > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			return value[:index]
		}
		units += width
	}
	return value
}

// agentNormalizeCapabilities mirrors normalizeCapabilities: trim, dedupe, cap at
// 64 entries of at most 80 characters each.
func agentNormalizeCapabilities(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	seen := make(map[string]bool, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		capability := agentSanitizeShortString(item, 80)
		if capability != nil && !seen[*capability] {
			seen[*capability] = true
			result = append(result, *capability)
		}
		if len(result) >= 64 {
			break
		}
	}
	return result
}

// agentIsRecord mirrors `typeof value === 'object' && value !== null && !Array.isArray(value)`.
func agentIsRecord(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, false
	}
	return object, true
}

// agentParseInteger mirrors parseInteger.
func agentParseInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			return 0, false
		}
		if typed > 9007199254740991 || typed < -9007199254740991 {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if !regexp.MustCompile(`^\d+$`).MatchString(trimmed) {
			return 0, false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || parsed > 9007199254740991 {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// agentClampHeartbeatInterval mirrors clampAgentHeartbeatIntervalSeconds.
func agentClampHeartbeatInterval(value any) int {
	parsed, ok := agentParseInteger(value)
	if !ok || parsed == 0 {
		return agentDefaultHeartbeatIntervalSeconds
	}
	if parsed < agentMinHeartbeatIntervalSeconds {
		return agentMinHeartbeatIntervalSeconds
	}
	if parsed > agentMaxHeartbeatIntervalSeconds {
		return agentMaxHeartbeatIntervalSeconds
	}
	return int(parsed)
}

// ==================== IP helpers ====================

// agentNormalizeIPCandidate mirrors normalizeIpCandidate.
func agentNormalizeIPCandidate(value string) *string {
	candidate := strings.TrimSpace(value)
	if candidate == "" {
		return nil
	}
	if strings.HasPrefix(candidate, "[") {
		if closing := strings.Index(candidate, "]"); closing > 0 {
			candidate = candidate[1:closing]
		}
	}
	ipv4WithPort := regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3})(?::\d+)?$`)
	if match := ipv4WithPort.FindStringSubmatch(candidate); match != nil {
		candidate = match[1]
	}
	if strings.HasPrefix(candidate, "::ffff:") {
		mapped := candidate[7:]
		if net.ParseIP(mapped) != nil {
			return &mapped
		}
	}
	if net.ParseIP(candidate) == nil {
		return nil
	}
	return &candidate
}

// agentFirstForwardedIP mirrors firstForwardedIp.
func agentFirstForwardedIP(c *gin.Context) *string {
	headers := []string{
		"Cf-Connecting-Ip",
		"True-Client-Ip",
		"X-Real-Ip",
		"X-Forwarded-For",
	}
	for _, header := range headers {
		value := c.GetHeader(header)
		if value == "" {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			if ip := agentNormalizeIPCandidate(part); ip != nil {
				return ip
			}
		}
	}
	return nil
}

// agentIsLocalOrPrivateIP mirrors isLocalOrPrivateIp.
func agentIsLocalOrPrivateIP(ip string) bool {
	if ip == "" {
		return false
	}
	if ip == "::1" || ip == "127.0.0.1" || strings.HasPrefix(ip, "127.") {
		return true
	}
	if strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "192.168.") || strings.HasPrefix(ip, "169.254.") {
		return true
	}
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		allIntegers := true
		numbers := make([]int, 4)
		for index, part := range parts {
			parsed, err := strconv.Atoi(part)
			if err != nil {
				allIntegers = false
				break
			}
			numbers[index] = parsed
		}
		if allIntegers {
			return numbers[0] == 172 && numbers[1] >= 16 && numbers[1] <= 31
		}
	}
	lower := strings.ToLower(ip)
	return strings.HasPrefix(lower, "fc") || strings.HasPrefix(lower, "fd") || strings.HasPrefix(lower, "fe80:")
}

// agentHeartbeatIP mirrors getAgentHeartbeatIp.
func agentHeartbeatIP(c *gin.Context) string {
	direct := middleware.ClientIP(c)
	if normalized := agentNormalizeIPCandidate(direct); normalized != nil {
		direct = *normalized
	}
	forwarded := agentFirstForwardedIP(c)
	trustForwarded := os.Getenv("AGENT_TRUST_FORWARDED_IP") == "true" || agentIsLocalOrPrivateIP(direct)
	if forwarded != nil && trustForwarded {
		return *forwarded
	}
	return direct
}

// ==================== URL / shell helpers ====================

// agentNormalizeBaseURL mirrors normalizeBaseUrl.
func agentNormalizeBaseURL(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" || len(trimmed) > agentMaxInstallURL {
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil
	}
	host := strings.ToLower(parsed.Host)
	// The WHATWG URL parser drops the default port for the scheme.
	if scheme == "http" && strings.HasSuffix(host, ":80") {
		host = strings.TrimSuffix(host, ":80")
	}
	if scheme == "https" && strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}
	result := scheme + "://" + host
	return &result
}

// agentDerivePanelURL mirrors derivePanelUrl (agent.ts), NOT hostsDerivePanelUrl:
// explicit baseUrl → FRONTEND_URL → Origin ?? Referer → forwarded proto/host → Host.
func agentDerivePanelURL(c *gin.Context, explicitBaseURL *string) string {
	if explicit := agentNormalizeBaseURL(explicitBaseURL); explicit != nil {
		return *explicit
	}
	if frontend := os.Getenv("FRONTEND_URL"); frontend != "" {
		if normalized := agentNormalizeBaseURL(&[]string{strings.Split(frontend, ",")[0]}[0]); normalized != nil {
			return *normalized
		}
	}
	referer := c.GetHeader("Origin")
	if referer == "" {
		referer = c.GetHeader("Referer")
	}
	if referer != "" {
		if normalized := agentNormalizeBaseURL(&referer); normalized != nil {
			return *normalized
		}
	}

	forwardedProto := ""
	if value := c.GetHeader("X-Forwarded-Proto"); value != "" {
		forwardedProto = strings.TrimSpace(strings.Split(value, ",")[0])
	}
	forwardedHost := ""
	if value := c.GetHeader("X-Forwarded-Host"); value != "" {
		forwardedHost = strings.TrimSpace(strings.Split(value, ",")[0])
	}
	directHost := strings.TrimSpace(c.GetHeader("Host"))
	protocol := c.Request.URL.Scheme
	if protocol == "" {
		protocol = "http"
	}
	if forwardedProto == "https" || forwardedProto == "http" {
		protocol = forwardedProto
	}
	host := forwardedHost
	if host == "" {
		host = directHost
	}
	if host != "" {
		return protocol + "://" + host
	}
	return "https://incudal.com"
}

// agentShellEscape mirrors shellEscape.
func agentShellEscape(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// agentBuildAgentInstallConfig mirrors buildAgentInstallConfig.
func agentBuildAgentInstallConfig(agentID, agentSecret string) string {
	return "INCUS_AGENT_ID=" + agentShellEscape(agentID) + "\n" +
		"INCUS_AGENT_SECRET=" + agentShellEscape(agentSecret) + "\n"
}

// agentBuildNodeInstallCommand mirrors buildNodeInstallCommand from
// lib/node-install.ts (including the channel argument, which the hosts-module
// helper omits).
func agentBuildNodeInstallCommand(panelURL, installToken string, channel *string) string {
	stripped := regexp.MustCompile(`/+$`).ReplaceAllString(panelURL, "")
	channelValue := "canary"
	if channel != nil && *channel != "" {
		channelValue = *channel
	}
	return "curl -fsSL " + agentShellEscape(stripped+"/api/agent/install.sh") +
		" -o incus-node-install.sh && sudo env INCUS_PANEL_URL=" + agentShellEscape(stripped) +
		" INCUS_AGENT_INSTALL_TOKEN=" + agentShellEscape(installToken) +
		" INCUS_NODE_CHANNEL=" + agentShellEscape(channelValue) +
		" bash incus-node-install.sh"
}

// ==================== agent serialisation ====================

// agentOptionalISO mirrors `date?.toISOString() ?? null`.
func agentOptionalISO(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// agentJSONArray decodes a Json column into an array; invalid/null becomes [].
func agentJSONArray(raw []byte) []any {
	if len(raw) == 0 {
		return []any{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return []any{}
	}
	array, ok := decoded.([]any)
	if !ok {
		return []any{}
	}
	return array
}

// agentJSONObject decodes a Json column into an object; invalid/null becomes {}.
func agentJSONObject(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return object
}

// agentGetHeartbeatIntervalSeconds mirrors getAgentHeartbeatIntervalSeconds.
func agentGetHeartbeatIntervalSeconds(agent *models.HostAgent) int {
	report := agentJSONObject(agent.LastReport)
	metrics, ok := agentIsRecord(report["metrics"])
	if !ok {
		return agentDefaultHeartbeatIntervalSeconds
	}
	return agentClampHeartbeatInterval(metrics["heartbeatIntervalSeconds"])
}

// agentDeriveStatus mirrors deriveAgentStatus. A stale agent is reported offline
// without rewriting the stored status.
func agentDeriveStatus(agent *models.HostAgent, now time.Time) string {
	if !agent.Enabled || agent.Status != "online" {
		return agent.Status
	}
	if agent.LastSeenAt == nil {
		return "offline"
	}
	heartbeatInterval := agentGetHeartbeatIntervalSeconds(agent)
	offlineThreshold := heartbeatInterval * 3
	if offlineThreshold < agentMinOfflineThresholdSeconds {
		offlineThreshold = agentMinOfflineThresholdSeconds
	}
	ageMs := now.UnixMilli() - agent.LastSeenAt.UTC().UnixMilli()
	if ageMs > int64(offlineThreshold)*1000 {
		return "offline"
	}
	return agent.Status
}

// agentSerialize mirrors serializeAgent/serializeAgentStatus (the TypeScript has
// two identical functions; one implementation serves both).
func agentSerialize(agent *models.HostAgent) gin.H {
	capabilities := agentJSONArray(agent.Capabilities)
	versionStatus := "unknown"
	for _, capability := range capabilities {
		if text, ok := capability.(string); ok && text == "incus_node_v1" {
			versionStatus = "managed"
			break
		}
	}
	return gin.H{
		"id":              agent.ID,
		"hostId":          agent.HostID,
		"agentId":         agent.AgentID,
		"enabled":         agent.Enabled,
		"status":          agentDeriveStatus(agent, time.Now().UTC()),
		"version":         agent.Version,
		"versionStatus":   versionStatus,
		"capabilities":    capabilities,
		"lastReport":      agentJSONObject(agent.LastReport),
		"lastSeenAt":      agentOptionalISO(agent.LastSeenAt),
		"lastHeartbeatIp": agent.LastHeartbeatIP,
		"createdAt":       agent.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"updatedAt":       agent.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// agentNormalizeAgentMetrics mirrors normalizeAgentMetrics.
func agentNormalizeAgentMetrics(metrics map[string]any) map[string]any {
	result := make(map[string]any, len(metrics)+1)
	for key, value := range metrics {
		result[key] = value
	}
	result["heartbeatIntervalSeconds"] = agentClampHeartbeatInterval(metrics["heartbeatIntervalSeconds"])
	return result
}

// agentBuildHeartbeatReport mirrors buildHeartbeatReport. When the node says it
// could not enumerate storage, the previous storage snapshot is preserved so a
// transient failure does not look like a deletion.
func agentBuildHeartbeatReport(body *agentHeartbeatBody, previous map[string]any) map[string]any {
	resources := map[string]any{}
	if typed, ok := agentIsRecord(body.Raw["resources"]); ok {
		resources = typed
	}
	previousResources := map[string]any{}
	if typed, ok := agentIsRecord(previous["resources"]); ok {
		previousResources = typed
	}
	if resources["storageAvailable"] == false {
		if storage, present := previousResources["storage"]; present {
			resources["storage"] = storage
		}
	}
	instances := map[string]any{"available": false}
	if typed, ok := agentIsRecord(body.Raw["instances"]); ok {
		instances["available"] = typed["available"] == true
	}
	metrics := map[string]any{}
	if typed, ok := agentIsRecord(body.Raw["metrics"]); ok {
		metrics = typed
	}
	result := map[string]any{
		"runtime":   mapOrEmpty(body.Raw["runtime"]),
		"incus":     mapOrEmpty(body.Raw["incus"]),
		"instances": instances,
		"caddy":     mapOrEmpty(body.Raw["caddy"]),
		"resources": resources,
		"metrics":   agentNormalizeAgentMetrics(metrics),
	}
	return result
}

// mapOrEmpty mirrors `value ?? {}` for the object-typed fields.
func mapOrEmpty(value any) map[string]any {
	if typed, ok := agentIsRecord(value); ok {
		return typed
	}
	return map[string]any{}
}

// agentExtractHostPublicIPv6 mirrors extractHostPublicIpv6FromResources from
// lib/host-resources.ts.
func agentExtractHostPublicIPv6(resources any) *string {
	object, ok := agentIsRecord(resources)
	if !ok {
		return nil
	}
	network, ok := agentIsRecord(object["network"])
	if !ok {
		return nil
	}
	cards, ok := network["cards"].([]any)
	if !ok {
		return nil
	}
	check := func(addresses any) *string {
		list, ok := addresses.([]any)
		if !ok {
			return nil
		}
		for _, entry := range list {
			address, ok := agentIsRecord(entry)
			if !ok {
				continue
			}
			if address["family"] != "inet6" || address["scope"] != "global" {
				continue
			}
			ip, ok := address["address"].(string)
			if !ok || ip == "" {
				continue
			}
			if strings.HasPrefix(ip, "fe80:") || strings.HasPrefix(ip, "fd") || strings.HasPrefix(ip, "fc") {
				continue
			}
			return &ip
		}
		return nil
	}
	for _, cardValue := range cards {
		card, ok := agentIsRecord(cardValue)
		if !ok {
			continue
		}
		if ip := check(card["addresses"]); ip != nil {
			return ip
		}
		ports, ok := card["ports"].([]any)
		if !ok {
			continue
		}
		for _, portValue := range ports {
			port, ok := agentIsRecord(portValue)
			if !ok {
				continue
			}
			if ip := check(port["addresses"]); ip != nil {
				return ip
			}
		}
	}
	return nil
}

// ==================== request body / errors ====================

// agentBodyCacheKey stores the decoded body so the HMAC verifier and the handler
// see the SAME document. Reading twice is impossible: Gin's GetRawData drains the
// request body, so a second read would silently hash "{}" and reject a valid
// signature. `/security-report` deliberately pre-checks the body hash and then
// calls the common verifier, which is exactly the double-read this avoids.
const agentBodyCacheKey = "agentDecodedJSONBody"

type agentBodyState struct {
	decoded   any
	rawLength int
}

// agentBodyStateOf returns the cached body state, reading the body if needed.
func agentBodyStateOf(c *gin.Context) *agentBodyState {
	agentReadBody(c)
	state, _ := c.Get(agentBodyCacheKey)
	if typed, ok := state.(*agentBodyState); ok {
		return typed
	}
	return &agentBodyState{}
}

// agentEnforceBodyLimit mirrors a Fastify route-level `bodyLimit`: the parser
// rejects an oversized payload before the handler, answering 413 with the
// message app.ts' error handler renders (`{ error }` only).
func agentEnforceBodyLimit(c *gin.Context, limit int) bool {
	if agentBodyStateOf(c).rawLength <= limit {
		return true
	}
	c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Request body is too large"})
	return false
}

// agentReadBody decodes the JSON body once. `middleware.RequestBody` has already
// enforced Fastify's parser rules and replayed the payload, so an empty read here
// means "no body".
func agentReadBody(c *gin.Context) any {
	if cached, ok := c.Get(agentBodyCacheKey); ok {
		if state, ok := cached.(*agentBodyState); ok {
			return state.decoded
		}
	}
	var decoded any
	state := &agentBodyState{}
	raw, err := c.GetRawData()
	if err == nil {
		state.rawLength = len(raw)
		if len(strings.TrimSpace(string(raw))) > 0 {
			_ = json.Unmarshal(raw, &decoded)
		}
	}
	state.decoded = decoded
	c.Set(agentBodyCacheKey, state)
	return decoded
}

// agentBodyObject mirrors Fastify's `type: 'object'` requirement: a missing body
// or a non-object is a validation error.
func agentBodyObject(c *gin.Context) (map[string]any, bool) {
	body := agentReadBody(c)
	if body == nil {
		return map[string]any{}, true
	}
	object, ok := agentIsRecord(body)
	if !ok {
		respondAJVValidation(c, "body must be object")
		return nil, false
	}
	return object, true
}

// agentValidateOptionalBoolean mirrors `{ enabled: { type: 'boolean' },
// additionalProperties: false }` under Fastify's AJV defaults
// (coerceTypes: 'array', removeAdditional: true — see GO_MIGRATION.md and the
// ported convention in admin_notification_channels.go):
//
//   - unknown keys are silently DROPPED (removeAdditional: true), not rejected;
//   - "true"/"false"/0/1 (and single-element arrays) coerce to a boolean, every
//     other value is a hard type error.
func agentValidateOptionalBoolean(c *gin.Context, object map[string]any, allowed ...string) (map[string]any, bool) {
	allowedSet := map[string]bool{"enabled": true}
	for _, name := range allowed {
		allowedSet[name] = true
	}
	filtered := make(map[string]any, len(allowedSet))
	for key, value := range object {
		if allowedSet[key] {
			filtered[key] = value
		}
	}
	if value, present := filtered["enabled"]; present {
		coerced, ok := coerceChannelBool(value)
		if !ok {
			respondAJVValidation(c, "must be boolean")
			return nil, false
		}
		filtered["enabled"] = coerced
	}
	return filtered, true
}

// agentCoerceArray mirrors AJV `coerceTypes: 'array'` for type:'array': a
// non-array value is wrapped in a single-element array.
func agentCoerceArray(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return []any{value}
}

// agentUTF16Length mirrors String#length (what AJV's maxLength counts here: the
// JavaScript string length).
func agentUTF16Length(value string) int {
	units := 0
	for _, runeValue := range value {
		if runeValue > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}

// agentValidateHeartbeatBody mirrors the POST /heartbeat AJV schema. Fastify
// validates before the handler runs (there is no auth hook on this route), so a
// malformed body answers 400 even without a valid signature — that ordering is
// reproduced by calling this before agentAuthenticateRequest.
func agentValidateHeartbeatBody(c *gin.Context, raw map[string]any) bool {
	if value, present := raw["heartbeatRequestId"]; present {
		text, ok := coerceChannelString(value)
		if !ok {
			respondAJVValidation(c, "must be string")
			return false
		}
		if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(text) {
			respondAJVValidation(c, `must match pattern "^[0-9a-f-]{36}$"`)
			return false
		}
		raw["heartbeatRequestId"] = text
	}
	if value, present := raw["version"]; present {
		text, ok := coerceChannelString(value)
		if !ok {
			respondAJVValidation(c, "must be string")
			return false
		}
		if agentUTF16Length(text) > 128 {
			respondAJVValidation(c, "version is too long")
			return false
		}
		raw["version"] = text
	}
	if value, present := raw["capabilities"]; present {
		items := agentCoerceArray(value)
		if len(items) > 64 {
			respondAJVValidation(c, "must NOT have more than 64 items")
			return false
		}
		coerced := make([]any, 0, len(items))
		for index, item := range items {
			text, ok := coerceChannelString(item)
			if !ok {
				respondAJVValidation(c, "must be string")
				return false
			}
			if agentUTF16Length(text) > 80 {
				respondAJVValidation(c, fmt.Sprintf("capabilities/%d is too long", index))
				return false
			}
			coerced = append(coerced, text)
		}
		raw["capabilities"] = coerced
	}
	for _, name := range []string{"runtime", "incus", "instances", "resources", "metrics"} {
		if value, present := raw[name]; present {
			if _, ok := agentIsRecord(value); !ok {
				respondAJVValidation(c, "must be object")
				return false
			}
		}
	}
	return true
}

// agentEnabledFromBody mirrors `request.body?.enabled ?? true`.
func agentEnabledFromBody(object map[string]any) bool {
	if value, ok := object["enabled"].(bool); ok {
		return value
	}
	return true
}

// agentRespondAPIError writes the `{ error, code }` envelope the TypeScript sends.
func agentRespondAPIError(c *gin.Context, status int, message, code string, details string) {
	payload := gin.H{"error": message, "code": code}
	if details != "" {
		payload["details"] = details
	}
	c.JSON(status, payload)
}

// ==================== HMAC authentication ====================

// agentAuthenticateRequest mirrors authenticateAgentRequest. It writes the 401
// response itself and returns nil when the request must stop.
func agentAuthenticateRequest(c *gin.Context) *models.HostAgent {
	headers := lib.ReadAgentAuthHeaders(c.Request.Header)
	if headers == nil {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Agent authentication headers are required", "AGENT_AUTH_REQUIRED", "")
		return nil
	}
	if headerError := lib.ValidateAgentHeaders(headers); headerError != "" {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Invalid Agent authentication headers", "AGENT_AUTH_INVALID", headerError)
		return nil
	}
	if !lib.IsAgentTimestampFresh(headers.Timestamp, time.Now().UTC().UnixMilli()) {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Agent request timestamp is expired", "AGENT_AUTH_EXPIRED", "")
		return nil
	}

	body := agentReadBody(c)
	bodyHash := lib.AgentBodyHashForRequest(c.Request.Method, body != nil, body)
	if !strings.EqualFold(headers.BodyHash, bodyHash) {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Agent body hash mismatch", "AGENT_BODY_HASH_MISMATCH", "")
		return nil
	}

	agent := agentLoadByIdentity(c.Request.Context(), headers.AgentID)
	if agent == nil {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Agent identity is not registered or has been rotated", "AGENT_NOT_REGISTERED", "")
		return nil
	}
	if !agent.Enabled {
		agentRespondAPIError(c, http.StatusUnauthorized, "Agent is not enabled", "AGENT_DISABLED", "")
		return nil
	}

	secret := lib.DecryptSensitiveData(agent.SecretEncrypted)
	if secret == "" || !lib.IsValidAgentSecret(secret) {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Agent secret is not available", "AGENT_SECRET_INVALID", "")
		return nil
	}

	signatureOK := lib.VerifyAgentSignature(secret, lib.AgentSigningPayload{
		Method:    c.Request.Method,
		Path:      lib.AgentBuildRequestPath(c.Request.URL.RequestURI()),
		Timestamp: headers.Timestamp,
		Nonce:     headers.Nonce,
		BodyHash:  headers.BodyHash,
	}, headers.Signature)
	if !signatureOK {
		agentRespondAPIError(c, http.StatusUnauthorized,
			"Agent signature verification failed", "AGENT_SIGNATURE_INVALID", "")
		return nil
	}

	if err := agentRecordNonce(c.Request.Context(), agent.AgentID, headers.Nonce); err != nil {
		if errors.Is(err, lib.ErrAgentNonceReplay) {
			agentRespondAPIError(c, http.StatusUnauthorized,
				"Agent nonce was already used", "AGENT_NONCE_REPLAY", "")
			return nil
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil
	}
	return agent
}

// ==================== DB access for the agent plane ====================

// Test seams: the authenticator's two DB touch points are function variables so
// the HMAC accept/reject matrix can be proven without a database. Production
// wires them to the real implementations below and never reassigns them.
var (
	agentLoadByIdentity       = agentFindByAgentID
	agentRecordNonce          = agentConsumeNonce
	agentRecordSecurityReport = lib.RecordReport
	agentReadSecurityState    = db.GetNodeSecurityState
	agentHeartbeatDepsFor     = func() *agentHeartbeatDeps { return &agentDefaultHeartbeatDeps }
)

func agentFindByAgentID(ctx context.Context, agentID string) *models.HostAgent {
	if db.DB == nil {
		return nil
	}
	var agent models.HostAgent
	if err := db.DB.WithContext(ctx).Where("agent_id = ?", agentID).Limit(1).Find(&agent).Error; err != nil {
		log.Printf("[agent] failed to load agent %s: %v", agentID, err)
		return nil
	}
	if agent.ID == 0 {
		return nil
	}
	return &agent
}

// agentConsumeNonce mirrors the TypeScript transaction that prunes expired nonces
// and inserts the new one, mapping the unique (agent_id, nonce) violation to a
// replay. Both statements share one transaction so a replay rolls the prune back.
func agentConsumeNonce(ctx context.Context, agentID, nonce string) error {
	pool := db.Pool()
	if pool == nil {
		return errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `DELETE FROM host_agent_nonces WHERE expires_at < $1`, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO host_agent_nonces (agent_id, nonce, expires_at) VALUES ($1, $2, $3)`,
		agentID, nonce, now.Add(time.Duration(lib.AgentNonceTTLMs)*time.Millisecond),
	); err != nil {
		if isUniqueViolation(err) {
			return lib.ErrAgentNonceReplay
		}
		return err
	}
	return tx.Commit(ctx)
}

// isUniqueViolation reports a PostgreSQL 23505 (unique constraint) error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ==================== handlers: public ====================

// agentInstallScriptHandler mirrors GET /install.sh.
func agentInstallScriptHandler(c *gin.Context) {
	script, ok := hostsReadInstallScript()
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"statusCode": http.StatusInternalServerError,
			"error":      "Internal Server Error",
			"message":    "Install script file not found",
		})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(script))
}

// agentDistributionHandler mirrors GET /distribution.
func agentDistributionHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"repository": "Rain-kl/incus",
		"package":    "incus-payincus",
		"channels":   []string{"latest", "canary"},
		"installer":  "/api/agent/install.sh",
		"managedBy":  "apt",
	})
}

// agentInstallConfigHandler mirrors GET /install-config/:token.
func agentInstallConfigHandler(c *gin.Context) {
	format := c.Query("format")
	if _, present := c.GetQuery("format"); present && format != "json" {
		respondAJVValidation(c, "must be equal to one of the allowed values")
		return
	}
	jsonFormat := format == "json"
	c.Header("Cache-Control", "no-store")

	var panelCertificate *lib.PanelClientCertificate
	if jsonFormat {
		certificate, err := lib.ReadPanelClientCertificate()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"code":  "PANEL_CERTIFICATE_UNAVAILABLE",
				"error": "Panel client certificate unavailable: " + err.Error(),
			})
			return
		}
		panelCertificate = certificate
	}

	token := c.Param("token")
	result, err := lib.ConsumeHostAgentInstallToken(c.Request.Context(), token)
	if err != nil {
		switch {
		case errors.Is(err, lib.ErrAgentInstallTokenExpired):
			if jsonFormat {
				c.JSON(http.StatusForbidden, gin.H{
					"code":  "AGENT_INSTALL_TOKEN_EXPIRED",
					"error": "Agent install token has expired",
				})
				return
			}
			c.String(http.StatusForbidden, "# Error: Agent install token has expired")
			return
		case errors.Is(err, lib.ErrAgentInstallTokenInvalid), errors.Is(err, lib.ErrAgentInstallSecretInvalid):
			if jsonFormat {
				c.JSON(http.StatusForbidden, gin.H{
					"code":  err.Error(),
					"error": "Invalid Agent install token",
				})
				return
			}
			c.String(http.StatusForbidden, "# Error: Invalid Agent install token")
			return
		default:
			log.Printf("[agent] install token consume failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"statusCode": http.StatusInternalServerError,
				"error":      "Internal Server Error",
				"message":    "Internal server error",
			})
			return
		}
	}

	log.Printf("[agent] Host Agent install token consumed (hostId=%d agentId=%s)", result.Host.ID, result.Agent.AgentID)
	db.CreateLog(c.Request.Context(), nil, agentLogModule, "host.agent_install_token_consume",
		fmt.Sprintf("宿主机 Agent 一次性安装 token 已消费: %s (#%d)", result.Host.Name, result.Host.ID),
		db.LogResultSuccess, db.CreateLogOptions{})

	if panelCertificate != nil {
		c.JSON(http.StatusOK, gin.H{
			"schemaVersion":                     1,
			"agentId":                           result.Agent.AgentID,
			"agentSecret":                       result.AgentSecret,
			"panelClientCertificate":            panelCertificate.Certificate,
			"panelClientCertificateFingerprint": panelCertificate.Fingerprint,
		})
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, "%s", agentBuildAgentInstallConfig(result.Agent.AgentID, result.AgentSecret))
}

// ==================== handlers: web user ====================

// agentHostProjection is the `select: { id, name, userId }` shape.
type agentHostProjection struct {
	ID     int32
	Name   string
	UserID int32
}

func agentLoadHost(c *gin.Context, hostID int32) (*agentHostProjection, bool) {
	var rows []agentHostProjection
	if err := db.DB.WithContext(c.Request.Context()).Model(&models.Host{}).
		Select("id", "name", "user_id").Where("id = ?", hostID).Limit(1).Scan(&rows).Error; err != nil {
		log.Printf("[agent] host %d lookup failed: %v", hostID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil, false
	}
	if len(rows) == 0 {
		agentRespondAPIError(c, http.StatusNotFound, "Host not found", "HOST_NOT_FOUND", "")
		return nil, false
	}
	return &rows[0], true
}

// agentOwnerOrAdmin mirrors `host.userId !== request.user.id && request.user.role !== 'admin'`.
func agentOwnerOrAdmin(c *gin.Context, host *agentHostProjection) bool {
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return false
	}
	if host.UserID != user.ID && user.Role != "admin" {
		agentRespondAPIError(c, http.StatusForbidden, "Forbidden", "FORBIDDEN", "")
		return false
	}
	return true
}

func agentFindByHostID(ctx context.Context, hostID int32) *models.HostAgent {
	var agent models.HostAgent
	if err := db.DB.WithContext(ctx).Where("host_id = ?", hostID).Limit(1).Find(&agent).Error; err != nil {
		log.Printf("[agent] failed to load agent for host %d: %v", hostID, err)
		return nil
	}
	if agent.ID == 0 {
		return nil
	}
	return &agent
}

// agentHostStatusHandler mirrors GET /hosts/:hostId/status.
func agentHostStatusHandler(c *gin.Context) {
	hostID, ok := agentParsePositiveID(c.Param("hostId"))
	if !ok {
		agentRespondAPIError(c, http.StatusBadRequest, "Invalid host ID", "INVALID_HOST_ID", "")
		return
	}
	host, ok := agentLoadHost(c, hostID)
	if !ok {
		return
	}
	if !agentOwnerOrAdmin(c, host) {
		return
	}
	agent := agentFindByHostID(c.Request.Context(), hostID)
	var serialized any
	if agent != nil {
		serialized = agentSerialize(agent)
	}
	c.JSON(http.StatusOK, gin.H{
		"host":  gin.H{"id": host.ID, "name": host.Name},
		"agent": serialized,
	})
}

// agentInstallCommandHandler mirrors POST /hosts/:hostId/install-command.
func agentInstallCommandHandler(c *gin.Context) {
	hostID, ok := agentParsePositiveID(c.Param("hostId"))
	if !ok {
		agentRespondAPIError(c, http.StatusBadRequest, "Invalid host ID", "INVALID_HOST_ID", "")
		return
	}
	object, ok := agentBodyObject(c)
	if !ok {
		return
	}
	if _, ok := agentValidateOptionalBoolean(c, object); !ok {
		return
	}
	host, ok := agentLoadHost(c, hostID)
	if !ok {
		return
	}
	if !agentOwnerOrAdmin(c, host) {
		return
	}

	panelURL := agentDerivePanelURL(c, nil)
	result, err := lib.IssueHostAgentInstallToken(c.Request.Context(), hostID, agentEnabledFromBody(object))
	if err != nil {
		agentRespondIssueError(c, err)
		return
	}
	installCommand := agentBuildNodeInstallCommand(panelURL, result.InstallToken, nil)

	user := middleware.CurrentUser(c)
	db.CreateLog(c.Request.Context(), &[]int64{int64(user.ID)}[0], agentLogModule, "host.agent_install_command_generate",
		fmt.Sprintf("生成宿主机 Agent 安装命令: %s (#%d)", host.Name, host.ID),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusCreated, gin.H{
		"host":                  gin.H{"id": result.Host.ID, "name": result.Host.Name},
		"agent":                 agentSerialize(&result.Agent),
		"installToken":          result.InstallToken,
		"installTokenExpiresAt": result.InstallTokenExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"installScriptUrl":      panelURL + "/api/agent/install.sh",
		"installCommand":        installCommand,
		"warning":               "installCommand 内包含一次性 Agent 安装 token，30 分钟内有效且只能使用一次。",
	})
}

// agentRespondIssueError maps HOST_NOT_FOUND and friends.
func agentRespondIssueError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, lib.ErrHostAgentHostNotFound):
		agentRespondAPIError(c, http.StatusNotFound, "Host not found", "HOST_NOT_FOUND", "")
	default:
		log.Printf("[agent] agent credential operation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
	}
}

// ==================== handlers: web admin ====================

// agentAdminStatusHandler mirrors GET /admin/hosts/:hostId/status.
func agentAdminStatusHandler(c *gin.Context) {
	hostID, ok := agentParsePositiveID(c.Param("hostId"))
	if !ok {
		agentRespondAPIError(c, http.StatusBadRequest, "Invalid host ID", "INVALID_HOST_ID", "")
		return
	}
	var rows []agentHostProjection
	if err := db.DB.WithContext(c.Request.Context()).Model(&models.Host{}).
		Select("id", "name").Where("id = ?", hostID).Limit(1).Scan(&rows).Error; err != nil {
		log.Printf("[agent] admin host %d lookup failed: %v", hostID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if len(rows) == 0 {
		agentRespondAPIError(c, http.StatusNotFound, "Host not found", "HOST_NOT_FOUND", "")
		return
	}
	agent := agentFindByHostID(c.Request.Context(), hostID)
	var serialized any
	if agent != nil {
		serialized = agentSerialize(agent)
	}
	c.JSON(http.StatusOK, gin.H{
		"host":  gin.H{"id": rows[0].ID, "name": rows[0].Name},
		"agent": serialized,
	})
}

// agentAdminCredentialsHandler mirrors POST /admin/hosts/:hostId/credentials.
func agentAdminCredentialsHandler(c *gin.Context) {
	hostID, ok := agentParsePositiveID(c.Param("hostId"))
	if !ok {
		agentRespondAPIError(c, http.StatusBadRequest, "Invalid host ID", "INVALID_HOST_ID", "")
		return
	}
	object, ok := agentBodyObject(c)
	if !ok {
		return
	}
	if _, ok := agentValidateOptionalBoolean(c, object); !ok {
		return
	}

	result, err := lib.RotateHostAgentCredentials(c.Request.Context(), hostID, agentEnabledFromBody(object))
	if err != nil {
		agentRespondIssueError(c, err)
		return
	}
	user := middleware.CurrentUser(c)
	db.CreateLog(c.Request.Context(), &[]int64{int64(user.ID)}[0], agentLogModule, "host.agent_credentials_rotate",
		fmt.Sprintf("重置宿主机 Agent 凭据: %s (#%d)", result.Host.Name, result.Host.ID),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusCreated, gin.H{
		"host":  gin.H{"id": result.Host.ID, "name": result.Host.Name},
		"agent": agentSerialize(&result.Agent),
		"credentials": gin.H{
			"agentId":     result.AgentID,
			"agentSecret": result.AgentSecret,
		},
		"warning": "agentSecret 只会在本次响应中返回，请写入宿主机 Agent 配置后妥善保存。",
	})
}

// agentAdminInstallCommandHandler mirrors POST /admin/hosts/:hostId/install-command.
func agentAdminInstallCommandHandler(c *gin.Context) {
	hostID, ok := agentParsePositiveID(c.Param("hostId"))
	if !ok {
		agentRespondAPIError(c, http.StatusBadRequest, "Invalid host ID", "INVALID_HOST_ID", "")
		return
	}
	object, ok := agentBodyObject(c)
	if !ok {
		return
	}
	if _, ok := agentValidateOptionalBoolean(c, object, "baseUrl", "channel"); !ok {
		return
	}
	var baseURL *string
	if value, present := object["baseUrl"]; present {
		text, ok := coerceChannelString(value)
		if !ok {
			respondAJVValidation(c, "must be string")
			return
		}
		if text == "" {
			respondAJVValidation(c, "baseUrl is too short")
			return
		}
		baseURL = &text
		if agentNormalizeBaseURL(baseURL) == nil {
			agentRespondAPIError(c, http.StatusBadRequest,
				"baseUrl must be a valid HTTP(S) URL", "INVALID_AGENT_BASE_URL", "")
			return
		}
	}
	var channel *string
	if value, present := object["channel"]; present {
		text, ok := coerceChannelString(value)
		if !ok {
			respondAJVValidation(c, "must be string")
			return
		}
		if text != "latest" && text != "canary" {
			respondAJVValidation(c, "must be equal to one of the allowed values")
			return
		}
		channel = &text
	}

	panelURL := agentDerivePanelURL(c, baseURL)
	result, err := lib.IssueHostAgentInstallToken(c.Request.Context(), hostID, agentEnabledFromBody(object))
	if err != nil {
		agentRespondIssueError(c, err)
		return
	}
	installCommand := agentBuildNodeInstallCommand(panelURL, result.InstallToken, channel)

	user := middleware.CurrentUser(c)
	db.CreateLog(c.Request.Context(), &[]int64{int64(user.ID)}[0], agentLogModule, "host.agent_install_command_generate",
		fmt.Sprintf("生成宿主机 Agent 安装命令: %s (#%d)", result.Host.Name, result.Host.ID),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusCreated, gin.H{
		"host":                  gin.H{"id": result.Host.ID, "name": result.Host.Name},
		"agent":                 agentSerialize(&result.Agent),
		"installToken":          result.InstallToken,
		"installTokenExpiresAt": result.InstallTokenExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"installScriptUrl":      panelURL + "/api/agent/install.sh",
		"installCommand":        installCommand,
		"warning":               "installCommand 内包含一次性 Agent 安装 token，30 分钟内有效且只能使用一次。",
	})
}

// ==================== handlers: HMAC ====================

// agentStartupKeysHandler mirrors POST /startup-keys.
func agentStartupKeysHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if !agentEnforceBodyLimit(c, agentStartupKeysBodyLimit) {
		return
	}

	agent := agentAuthenticateRequest(c)
	if agent == nil {
		return
	}
	secret := lib.DecryptSensitiveData(agent.SecretEncrypted)
	if secret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Startup service unavailable"})
		return
	}
	ctx := c.Request.Context()
	result, err := lib.GrantStartupKeys(ctx, agent.HostID, agent.AgentID, secret, agentReadBody(c))
	if err != nil {
		// The TypeScript lets an unexpected error escape to Fastify's 500 handler;
		// only the explicit `unavailable` result below is a 503.
		log.Printf("[agent] startup key grant failed for host %d: %v", agent.HostID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"statusCode": http.StatusInternalServerError,
			"error":      "Internal Server Error",
			"message":    "Internal server error",
		})
		return
	}
	if result.Unavailable {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Startup service unavailable"})
		return
	}
	if result.Invalid {
		// Ask the node to lock itself. Failure is only logged: the lock state is
		// already durable (recoveryBlocked) and the panel will retry.
		hostID, generation := agent.HostID, result.Generation
		go func() {
			lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, lockErr := storageEncryptionWithStorageSession(lockCtx, hostID, false,
				func(node StorageEncryptionNode, _ storageEncryptionKeyVault) (storageEncryptionPoolState, error) {
					_, requestErr := node.Request(lockCtx, http.MethodPost, "/1.0/agent/security/lock",
						map[string]any{"generation": generation})
					return storageEncryptionPoolState{}, requestErr
				})
			if lockErr != nil {
				log.Printf("[agent] Node action pending for host %d: %v", hostID, lockErr)
			}
		}()
		c.JSON(http.StatusConflict, gin.H{"error": "Internal service unavailable"})
		return
	}
	if result.Replay {
		c.JSON(http.StatusConflict, gin.H{"error": "Request could not be completed"})
		return
	}
	c.JSON(http.StatusOK, result.Envelope)
}

// agentSecurityReportHandler mirrors POST /security-report.
func agentSecurityReportHandler(c *gin.Context) {
	if !agentEnforceBodyLimit(c, agentSecurityReportLimit) {
		return
	}
	// The event batch must bind the actual body independently of the tunnel
	// handshake: a valid HMAC over a GET body must not authorise a report.
	signed := lib.ReadAgentAuthHeaders(c.Request.Header)
	body := agentReadBody(c)
	if signed == nil || !strings.EqualFold(signed.BodyHash, lib.AgentBodyHashForRequest(c.Request.Method, body != nil, body)) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Report unavailable"})
		return
	}
	agent := agentAuthenticateRequest(c)
	if agent == nil {
		return
	}
	c.Header("Cache-Control", "no-store")

	ctx := c.Request.Context()
	ack, err := agentRecordSecurityReport(ctx, agent.HostID, body)
	if err != nil {
		log.Printf("[agent] security report failed for host %d: %v", agent.HostID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Report unavailable"})
		return
	}
	state, err := agentReadSecurityState(ctx, agent.HostID)
	if err != nil {
		log.Printf("[agent] security state read failed for host %d: %v", agent.HostID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Report unavailable"})
		return
	}
	recoveryBlocked := false
	generation := int32(0)
	if state != nil {
		recoveryBlocked = state.RecoveryBlocked
		generation = state.RecoveryGeneration
	}
	c.JSON(http.StatusOK, gin.H{
		"ack":                ack,
		"recoveryBlocked":    recoveryBlocked,
		"recoveryGeneration": generation,
	})
}

// ==================== handler: heartbeat ====================

// agentHeartbeatBody is the raw JSON body (additionalProperties: true).
type agentHeartbeatBody struct {
	Raw map[string]any
}

// agentPendingCreateTask mirrors one entry of pendingCreateTasks.
type agentPendingCreateTask struct {
	TaskID     int32  `json:"taskId"`
	InstanceID *int32 `json:"instanceId"`
	RetryCount int32  `json:"retryCount"`
	CreatedAt  string `json:"createdAt"`
}

// agentHostConnection mirrors the host columns the heartbeat needs.
type agentHostConnection struct {
	CaddyEnabled  bool
	TunnelEnabled bool
	TargetHost    string
	TargetPort    int32
}

// agentHeartbeatAgentUpdate is the agent row update.
type agentHeartbeatAgentUpdate struct {
	Status            string
	Version           *string
	Capabilities      []string
	LastReport        map[string]any
	IncludeLastReport bool
	LastSeenAt        time.Time
	LastHeartbeatIP   *string
}

// agentHeartbeatDeps isolates every side effect of the heartbeat so the state
// machine is unit-testable without a database.
type agentHeartbeatDeps struct {
	UpdateAgent         func(ctx context.Context, agentID string, update agentHeartbeatAgentUpdate) error
	ProcessStorage      func(ctx context.Context, hostID int32, payload any, receivedAt time.Time) (bool, error)
	ProcessInstances    func(ctx context.Context, hostID int32, payload any) (services.AgentInstanceReportResult, error)
	SyncGPU             func(ctx context.Context, hostID int32, resources any) error
	HostConnection      func(ctx context.Context, hostID int32) (*agentHostConnection, error)
	MarkHostOnline      func(ctx context.Context, hostID int32) error
	MarkHostInstalled   func(ctx context.Context, hostID int32, architecture string) error
	SetHostCertificates func(ctx context.Context, hostID int32) error
	SetHostIPv6         func(ctx context.Context, hostID int32, ipv6 string) error
	RecordHostSuccess   func(hostID int32)
	SetCaddyEnabled     func(ctx context.Context, hostID int32, enabled bool) error
	ListPendingCreates  func(ctx context.Context, hostID int32) ([]agentPendingCreateTask, error)
	NudgeWorker         func()
	RecordNetworkStatus func(ctx context.Context, hostID int32, report any) error
	CompleteHeartbeat   func(hostID int32, requestID any, message string)
	Now                 func() time.Time
}

// agentHeartbeatHandler mirrors POST /heartbeat.
func agentHeartbeatHandler(c *gin.Context) {
	body := agentHeartbeatBody{Raw: map[string]any{}}
	if object, ok := agentIsRecord(agentReadBody(c)); ok {
		body.Raw = object
	}
	// Fastify validates the schema before the handler runs, so this precedes the
	// (handler-internal) HMAC check.
	if !agentValidateHeartbeatBody(c, body.Raw) {
		return
	}
	agent := agentAuthenticateRequest(c)
	if agent == nil {
		return
	}
	agentHeartbeatStateMachine(c, agent, body, agentHeartbeatDepsFor())
}

// agentHeartbeatRequestID mirrors `request.body.heartbeatRequestId` (any type:
// the tunnel manager ignores non-strings).
func agentHeartbeatRequestID(body agentHeartbeatBody) any {
	if value, present := body.Raw["heartbeatRequestId"]; present {
		return value
	}
	return nil
}

// agentHeartbeatStateMachine is the ported handler body.
func agentHeartbeatStateMachine(c *gin.Context, agent *models.HostAgent, body agentHeartbeatBody, deps *agentHeartbeatDeps) {
	ctx := c.Request.Context()
	heartbeatRequestID := agentHeartbeatRequestID(body)

	// Only an authenticated, persisted report may complete a waiting test.
	reportError := "本次节点上报未完成，测试失败"
	complete := func() {
		message := reportError
		if c.Writer.Status() >= 400 {
			message = "本次节点上报未能保存，测试失败"
		}
		deps.CompleteHeartbeat(agent.HostID, heartbeatRequestID, message)
	}

	nodeAPI, _ := body.Raw["nodeApi"].(string)
	if nodeAPI != "incus_node_v1" && nodeAPI != "payincus_node_v1" {
		c.JSON(http.StatusConflict, gin.H{
			"code":  "NODE_API_REQUIRED",
			"error": "Install the Incus node package",
		})
		complete()
		return
	}

	now := deps.Now().UTC()
	version := agentSanitizeShortString(body.Raw["version"], 128)
	capabilities := agentNormalizeCapabilities(body.Raw["capabilities"])
	lastSeenAt := now
	heartbeatIP := agentSanitizeShortString(agentHeartbeatIP(c), 128)
	locked := false
	if nodeSecurity, ok := agentIsRecord(body.Raw["nodeSecurity"]); ok {
		if state, ok := agentIsRecord(nodeSecurity["state"]); ok {
			locked = state["locked"] == true
		}
	}

	// A locked node stays online, but its deliberately absent inventory must not
	// be turned into deletions.
	if locked {
		if err := deps.UpdateAgent(ctx, agent.AgentID, agentHeartbeatAgentUpdate{
			Status:          "online",
			Version:         version,
			Capabilities:    capabilities,
			LastSeenAt:      lastSeenAt,
			LastHeartbeatIP: heartbeatIP,
		}); err != nil {
			log.Printf("[agent] heartbeat agent update failed: %v", err)
		}
		storage := body.Raw["storage"]
		storageAccepted, err := deps.ProcessStorage(ctx, agent.HostID, storage, now)
		if err != nil {
			log.Printf("[agent] locked-node storage report failed: %v", err)
			storageAccepted = false
		}
		storageObject, _ := agentIsRecord(storage)
		if storageAccepted && storageObject != nil &&
			storageObject["available"] == true && storageObject["collectionComplete"] == true {
			reportError = ""
		} else {
			reportError = "节点已连接，但锁定存储状态采集或保存未完成，测试失败"
		}
		connection, err := deps.HostConnection(ctx, agent.HostID)
		if err != nil {
			log.Printf("[agent] host connection read failed: %v", err)
		}
		tunnelEnabled := false
		targetHost := "127.0.0.1"
		targetPort := int32(8443)
		if connection != nil {
			// `??` only replaces null/undefined: an existing row's values are used
			// verbatim, even when they are "" or 0.
			tunnelEnabled = connection.TunnelEnabled
			targetHost = connection.TargetHost
			targetPort = connection.TargetPort
		}
		if agentCapabilitiesContain(agentJSONArrayAsStrings(agent.Capabilities), "network-control") {
			tunnelEnabled = true
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":         true,
			"serverTime": now.Format("2006-01-02T15:04:05.000Z"),
			"tunnel": gin.H{
				"enabled":    tunnelEnabled,
				"targetHost": targetHost,
				"targetPort": targetPort,
			},
		})
		complete()
		return
	}

	var (
		instanceReport    *services.AgentInstanceReportResult
		instancePersisted = true
	)
	if result, err := deps.ProcessInstances(ctx, agent.HostID, body.Raw["instances"]); err != nil {
		instancePersisted = false
		log.Printf("[agent] failed to process Agent instance report (agentId=%s hostId=%d): %v",
			agent.AgentID, agent.HostID, err)
	} else {
		instanceReport = &result
	}

	if err := deps.UpdateAgent(ctx, agent.AgentID, agentHeartbeatAgentUpdate{
		Status:            "online",
		Version:           version,
		Capabilities:      capabilities,
		LastReport:        agentBuildHeartbeatReport(&body, agentJSONObject(agent.LastReport)),
		IncludeLastReport: true,
		LastSeenAt:        lastSeenAt,
		LastHeartbeatIP:   heartbeatIP,
	}); err != nil {
		log.Printf("[agent] heartbeat agent update failed: %v", err)
	}

	gpuPersisted := true
	resources, _ := agentIsRecord(body.Raw["resources"])
	if err := deps.SyncGPU(ctx, agent.HostID, resources["nvidiaGpu"]); err != nil {
		gpuPersisted = false
		log.Printf("[agent] failed to update NVIDIA GPU inventory (hostId=%d): %v", agent.HostID, err)
	}

	storage := body.Raw["storage"]
	storageAccepted, err := deps.ProcessStorage(ctx, agent.HostID, storage, now)
	if err != nil {
		log.Printf("[agent] storage report failed (hostId=%d): %v", agent.HostID, err)
		storageAccepted = false
	}
	storageObject, _ := agentIsRecord(storage)
	incus, _ := agentIsRecord(body.Raw["incus"])
	instances, _ := agentIsRecord(body.Raw["instances"])
	switch {
	case incus["available"] != true:
		reportError = "节点已连接，但 Incus 状态采集失败，测试失败"
	case !storageAccepted || storageObject == nil || storageObject["available"] != true || storageObject["collectionComplete"] != true:
		reportError = "节点存储状态采集或保存失败，测试失败"
	case !instancePersisted || instances["available"] != true || instanceReport == nil || instanceReport.Errors > 0:
		reportError = "节点实例状态采集或保存失败，测试失败"
	case !gpuPersisted || resources["storageAvailable"] != true:
		reportError = "节点硬件状态采集或保存失败，测试失败"
	default:
		reportError = ""
	}

	if incus["available"] == true {
		if err := deps.MarkHostOnline(ctx, agent.HostID); err != nil {
			log.Printf("[agent] mark host online failed: %v", err)
		}
		architecture := ""
		if runtime, ok := agentIsRecord(body.Raw["runtime"]); ok {
			switch runtime["goarch"] {
			case "arm64":
				architecture = "aarch64"
			case "amd64":
				architecture = "x86_64"
			}
		}
		if err := deps.MarkHostInstalled(ctx, agent.HostID, architecture); err != nil {
			log.Printf("[agent] mark host installed failed: %v", err)
		}
		if err := deps.SetHostCertificates(ctx, agent.HostID); err != nil {
			log.Printf("[agent] set host certificates failed: %v", err)
		}
		if ipv6 := agentExtractHostPublicIPv6(body.Raw["resources"]); ipv6 != nil {
			if err := deps.SetHostIPv6(ctx, agent.HostID, *ipv6); err != nil {
				log.Printf("[agent] set host ipv6 failed: %v", err)
			}
		}
		deps.RecordHostSuccess(agent.HostID)
	}

	connection, err := deps.HostConnection(ctx, agent.HostID)
	if err != nil {
		log.Printf("[agent] host connection read failed: %v", err)
	}

	caddyReported, _ := agentIsRecord(body.Raw["caddy"])
	caddyAvailable := caddyReported != nil && caddyReported["available"] == true
	caddyEnabled := connection != nil && connection.CaddyEnabled
	if caddyAvailable && !caddyEnabled {
		if err := deps.SetCaddyEnabled(ctx, agent.HostID, true); err != nil {
			log.Printf("[agent] enable caddy failed: %v", err)
		}
		caddyEnabled = true
		agentCaddyDelete(agentCaddyState.install, agent.HostID)
		log.Printf("[Caddy] Agent reported Caddy available; auto-enabled host (agentId=%s hostId=%d)",
			agent.AgentID, agent.HostID)
	}

	uninstallRequested := agentCaddyHas(agentCaddyState.uninstall, agent.HostID)
	if caddyReported != nil && uninstallRequested && caddyEnabled && !caddyAvailable {
		if err := deps.SetCaddyEnabled(ctx, agent.HostID, false); err != nil {
			log.Printf("[agent] disable caddy failed: %v", err)
		}
		caddyEnabled = false
		agentCaddyDelete(agentCaddyState.uninstall, agent.HostID)
		agentCaddyDelete(agentCaddyState.install, agent.HostID)
		log.Printf("[Caddy] Agent reported Caddy removed; auto-disabled host (agentId=%s hostId=%d)",
			agent.AgentID, agent.HostID)
	}

	caddyPort := 2019
	if caddyReported != nil {
		if port, ok := caddyReported["port"].(float64); ok && port > 0 {
			if port > 65535 {
				port = 65535
			}
			caddyPort = int(port)
		}
	}

	// Command priority: uninstall > install > idle. Both are explicit panel
	// actions, never inferred from the report alone.
	caddyCommand := "idle"
	if uninstallRequested {
		caddyCommand = "uninstall"
	} else if !caddyEnabled && agentCaddyHas(agentCaddyState.install, agent.HostID) {
		caddyCommand = "install"
	}

	if networkStatus, present := body.Raw["networkStatus"]; present && networkStatus != nil {
		if err := deps.RecordNetworkStatus(ctx, agent.HostID, networkStatus); err != nil {
			log.Printf("[agent] network status record failed: %v", err)
		}
	}

	pendingCreateTasks, err := deps.ListPendingCreates(ctx, agent.HostID)
	if err != nil {
		log.Printf("[agent] pending create task read failed: %v", err)
		pendingCreateTasks = []agentPendingCreateTask{}
	}
	if len(pendingCreateTasks) > 0 {
		deps.NudgeWorker()
	}

	caddyInstallPort := 0
	if caddyCommand == "install" {
		caddyInstallPort = caddyPort
	}
	// `host?.tunnelEnabled ?? true`: a missing row means "enabled" (the Node
	// default), an existing row's value is used verbatim.
	tunnelEnabled := true
	targetHost := "127.0.0.1"
	targetPort := int32(8443)
	if connection != nil {
		tunnelEnabled = connection.TunnelEnabled
		targetHost = connection.TargetHost
		targetPort = connection.TargetPort
	}
	if agentRawCapabilitiesContain(body.Raw["capabilities"], "network-control") {
		tunnelEnabled = true
	}

	var instanceReportValue any
	if instanceReport != nil {
		instanceReportValue = instanceReport
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":                      true,
		"serverTime":              now.Format("2006-01-02T15:04:05.000Z"),
		"taskPollIntervalSeconds": 15,
		"instanceReport":          instanceReportValue,
		"pendingCreateTasks":      pendingCreateTasks,
		"caddy": gin.H{
			"command": caddyCommand,
			"port":    caddyInstallPort,
		},
		"tunnel": gin.H{
			"enabled":    tunnelEnabled,
			"targetHost": targetHost,
			"targetPort": targetPort,
		},
	})
	complete()
}

// agentRawCapabilitiesContain mirrors `request.body.capabilities?.includes(name)`
// over the RAW array (no trimming), which is what the TypeScript checks here.
func agentRawCapabilitiesContain(value any, target string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if text, ok := item.(string); ok && text == target {
			return true
		}
	}
	return false
}

func agentCapabilitiesContain(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

// ==================== real heartbeat dependencies ====================

var agentDefaultHeartbeatDeps = agentHeartbeatDeps{
	UpdateAgent: func(ctx context.Context, agentID string, update agentHeartbeatAgentUpdate) error {
		if db.DB == nil {
			return errors.New("database not initialized")
		}
		values := map[string]any{
			"status":            update.Status,
			"version":           update.Version,
			"capabilities":      jsonArrayJSON(update.Capabilities),
			"last_seen_at":      update.LastSeenAt,
			"last_heartbeat_ip": update.LastHeartbeatIP,
			"updated_at":        time.Now().UTC(),
		}
		if update.IncludeLastReport {
			values["last_report"] = jsonObjectJSON(update.LastReport)
		}
		return db.DB.WithContext(ctx).Model(&models.HostAgent{}).
			Where("agent_id = ?", agentID).Updates(values).Error
	},
	ProcessStorage: services.ProcessAgentStorageReport,
	ProcessInstances: func(ctx context.Context, hostID int32, payload any) (services.AgentInstanceReportResult, error) {
		return services.ProcessAgentInstanceReport(ctx, hostID, payload)
	},
	SyncGPU: func(ctx context.Context, hostID int32, resources any) error {
		return db.GpusSyncHostReport(ctx, hostID, resources)
	},
	HostConnection: func(ctx context.Context, hostID int32) (*agentHostConnection, error) {
		if db.DB == nil {
			return nil, errors.New("database not initialized")
		}
		var rows []agentHostConnection
		if err := db.DB.WithContext(ctx).Model(&models.Host{}).
			Select("caddy_enabled", "tunnel_enabled", "target_host", "target_port").
			Where("id = ?", hostID).Limit(1).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		return &rows[0], nil
	},
	MarkHostOnline: func(ctx context.Context, hostID int32) error {
		return db.DB.WithContext(ctx).Model(&models.Host{}).
			Where("id = ? AND status = 'offline'", hostID).
			Updates(map[string]any{"status": "online", "updated_at": time.Now().UTC()}).Error
	},
	MarkHostInstalled: func(ctx context.Context, hostID int32, architecture string) error {
		values := map[string]any{"is_installed": true, "updated_at": time.Now().UTC()}
		if architecture != "" {
			values["architecture"] = architecture
		}
		return db.DB.WithContext(ctx).Model(&models.Host{}).
			Where("id = ? AND is_installed = false", hostID).Updates(values).Error
	},
	SetHostCertificates: func(ctx context.Context, hostID int32) error {
		certPath, keyPath := lib.PanelCertificatePaths()
		return db.DB.WithContext(ctx).Model(&models.Host{}).
			Where("id = ? AND cert_path IS NULL AND key_path IS NULL", hostID).
			Updates(map[string]any{
				"cert_path":  certPath,
				"key_path":   keyPath,
				"updated_at": time.Now().UTC(),
			}).Error
	},
	SetHostIPv6: func(ctx context.Context, hostID int32, ipv6 string) error {
		return db.DB.WithContext(ctx).Model(&models.Host{}).
			Where("id = ? AND nat_public_ipv6 IS NULL", hostID).
			Updates(map[string]any{"nat_public_ipv6": ipv6, "updated_at": time.Now().UTC()}).Error
	},
	RecordHostSuccess: func(hostID int32) {
		lib.RecordHostSuccess(hostID, 0)
	},
	SetCaddyEnabled: func(ctx context.Context, hostID int32, enabled bool) error {
		return db.DB.WithContext(ctx).Model(&models.Host{}).
			Where("id = ?", hostID).
			Updates(map[string]any{"caddy_enabled": enabled, "updated_at": time.Now().UTC()}).Error
	},
	ListPendingCreates: func(ctx context.Context, hostID int32) ([]agentPendingCreateTask, error) {
		rows, err := db.Pool().Query(ctx, `
			SELECT id, instance_id, retry_count, created_at FROM instance_tasks
			 WHERE host_id = $1 AND task_type = 'create' AND status = 'PENDING'
			 ORDER BY created_at ASC LIMIT 100`, hostID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		tasks := make([]agentPendingCreateTask, 0, 8)
		for rows.Next() {
			var (
				taskID     int32
				instanceID *int32
				retryCount int32
				createdAt  time.Time
			)
			if err := rows.Scan(&taskID, &instanceID, &retryCount, &createdAt); err != nil {
				return nil, err
			}
			tasks = append(tasks, agentPendingCreateTask{
				TaskID:     taskID,
				InstanceID: instanceID,
				RetryCount: retryCount,
				CreatedAt:  createdAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}
		return tasks, rows.Err()
	},
	NudgeWorker: func() { agentNudgeInstanceTaskWorker() },
	RecordNetworkStatus: func(ctx context.Context, hostID int32, report any) error {
		_, err := lib.AgentRecordNetworkStatus(ctx, hostID, report, nil)
		return err
	},
	CompleteHeartbeat: func(hostID int32, requestID any, message string) {
		lib.TunnelManagerInstance().CompleteHeartbeat(hostID, requestID, message)
	},
	Now: func() time.Time { return time.Now().UTC() },
}

// jsonArrayJSON renders []string as a JSON column value.
func jsonArrayJSON(values []string) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// jsonObjectJSON renders map[string]any as a JSON column value.
func jsonObjectJSON(values map[string]any) string {
	if values == nil {
		return "{}"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// ==================== handler: tunnel ====================

// agentTunnelHandler mirrors GET /tunnel (websocket: true).
//
// Ordering matters and mirrors the TypeScript: the socket is upgraded FIRST, then
// authenticated, then closed with 4001/4003. An agent therefore sees a close code
// rather than an HTTP 401 — that is the observable contract.
func agentTunnelHandler(c *gin.Context) {
	if !lib.IsAgentWebSocketUpgrade(c.Request) {
		// Measured against the running Node backend: @fastify/websocket answers a
		// plain GET on a websocket route with 404 and an EMPTY body (it does not
		// reach the route handler at all). Reproduce that instead of inventing a 400.
		c.Status(http.StatusNotFound)
		return
	}
	socket, err := lib.UpgradeAgentWebSocket(c.Writer, c.Request)
	if err != nil {
		log.Printf("[agent] websocket upgrade failed: %v", err)
		return
	}

	// authenticateAgentRequest writes HTTP responses; after an upgrade there is no
	// HTTP response left, so replicate its decision without writing a body.
	agent := agentAuthenticateTunnel(c)
	if agent == nil {
		socket.TunnelClose(4001, "Agent authentication failed")
		return
	}

	host := agentTunnelHost(c.Request.Context(), agent.HostID)
	if host == nil || (!host.TunnelEnabled && !agentCapabilitiesContain(agentJSONArrayAsStrings(agent.Capabilities), "network-control")) {
		socket.TunnelClose(4003, "Tunnel mode is disabled for this host")
		return
	}

	manager := lib.TunnelManagerInstance()
	manager.RegisterTunnel(agent.HostID, socket)
	lib.AgentServeTunnel(agent.HostID, manager, socket)
}

// agentAuthenticateTunnel is authenticateAgentRequest for the upgraded path: it
// never writes an HTTP response.
func agentAuthenticateTunnel(c *gin.Context) *models.HostAgent {
	headers := lib.ReadAgentAuthHeaders(c.Request.Header)
	if headers == nil {
		return nil
	}
	if lib.ValidateAgentHeaders(headers) != "" {
		return nil
	}
	if !lib.IsAgentTimestampFresh(headers.Timestamp, time.Now().UTC().UnixMilli()) {
		return nil
	}
	body := agentReadBody(c)
	if !strings.EqualFold(headers.BodyHash, lib.AgentBodyHashForRequest(c.Request.Method, body != nil, body)) {
		return nil
	}
	agent := agentLoadByIdentity(c.Request.Context(), headers.AgentID)
	if agent == nil || !agent.Enabled {
		return nil
	}
	secret := lib.DecryptSensitiveData(agent.SecretEncrypted)
	if secret == "" || !lib.IsValidAgentSecret(secret) {
		return nil
	}
	if !lib.VerifyAgentSignature(secret, lib.AgentSigningPayload{
		Method:    c.Request.Method,
		Path:      lib.AgentBuildRequestPath(c.Request.URL.RequestURI()),
		Timestamp: headers.Timestamp,
		Nonce:     headers.Nonce,
		BodyHash:  headers.BodyHash,
	}, headers.Signature) {
		return nil
	}
	if err := agentRecordNonce(c.Request.Context(), agent.AgentID, headers.Nonce); err != nil {
		return nil
	}
	return agent
}

// agentTunnelHost mirrors the `select: { id, tunnelEnabled, targetHost, targetPort }`.
func agentTunnelHost(ctx context.Context, hostID int32) *agentHostConnection {
	var rows []agentHostConnection
	if err := db.DB.WithContext(ctx).Model(&models.Host{}).
		Select("tunnel_enabled").Where("id = ?", hostID).Limit(1).Scan(&rows).Error; err != nil {
		log.Printf("[agent] tunnel host lookup failed: %v", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// agentJSONArrayAsStrings renders the Json capabilities column as strings.
func agentJSONArrayAsStrings(raw []byte) []string {
	values := agentJSONArray(raw)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

// ==================== registration ====================

// RegisterAgent mounts /api/agent.
func RegisterAgent(r *gin.Engine) {
	public := r.Group("/api/agent")
	public.GET("/install.sh", agentInstallScriptHandler)
	public.GET("/distribution", agentDistributionHandler)
	public.GET("/install-config/:token", agentInstallConfigHandler)
	public.POST("/startup-keys", middleware.RequestBody(), agentStartupKeysHandler)
	public.POST("/security-report", middleware.RequestBody(), agentSecurityReportHandler)
	public.POST("/heartbeat", middleware.RequestBody(), agentHeartbeatHandler)
	public.GET("/tunnel", agentTunnelHandler)

	userGroup := r.Group("/api/agent", middleware.Authenticate())
	userGroup.GET("/hosts/:hostId/status", agentHostStatusHandler)
	userGroup.POST("/hosts/:hostId/install-command", agentInstallCommandHandler)

	adminGroup := r.Group("/api/agent", middleware.AuthenticateAdmin())
	adminGroup.GET("/admin/hosts/:hostId/status", agentAdminStatusHandler)
	adminGroup.POST("/admin/hosts/:hostId/credentials", agentAdminCredentialsHandler)
	adminGroup.POST("/admin/hosts/:hostId/install-command", agentAdminInstallCommandHandler)
}

func init() {
	// The egress control plane lives in package routes (it predates this module);
	// install it as the sanitizer seam lib.AgentRecordNetworkStatus expects.
	lib.AgentNetworkStatusSanitizer = func(report any) (any, error) {
		return egressSanitizeNetworkStatus(report)
	}

	// Fill the seams the hosts module declared for this file. Without them the
	// panel's "regenerate install command" and Caddy install/uninstall buttons
	// answer 500/501 instead of doing the work.
	HostsIssueAgentInstallToken = func(ctx context.Context, hostID int32, rotate bool) (*HostsAgentInstall, error) {
		result, err := lib.IssueHostAgentInstallToken(ctx, hostID, rotate)
		if err != nil {
			return nil, err
		}
		return &HostsAgentInstall{
			InstallToken:          result.InstallToken,
			InstallTokenExpiresAt: result.InstallTokenExpiresAt,
			AgentID:               result.Agent.AgentID,
		}, nil
	}
	HostsCaddyInstallRequested = func(hostID int32) error {
		agentCaddyRequestInstall(hostID)
		return nil
	}
	HostsCaddyUninstallRequested = func(hostID int32) error {
		agentCaddyRequestUninstall(hostID)
		return nil
	}

	// GET /tunnel's Node counterpart registers this listener when the route
	// module loads: once a tunnel reports control readiness, push the current
	// network policy. The push is fire-and-forget and logs failures (Node:
	// `void pushNetworkPolicy(hostId).catch(() => fastify.log.warn(...))`).
	lib.TunnelManagerInstance().OnControlReady(func(hostID int32) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := egressPushNetworkPolicy(ctx, hostID); err != nil {
				log.Printf("[agent] Realtime network synchronization failed for host %d: %v", hostID, err)
			}
		}()
	})

	Register(RegisterAgent)
}
