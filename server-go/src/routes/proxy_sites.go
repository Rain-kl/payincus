// Port of server/src/routes/proxy-sites.ts.
//
// Eight routes manage the reverse-proxy sites of an instance: the list, create,
// delete, DNS check + activation, refresh, partial update, enable/disable toggle and
// a TLS certificate probe.
//
// Caddy dependency (Phase 7 seam). Everything that talks to Caddy goes through
// lib.GetCaddyClientForHost, which refuses with an explicit error while the Agent
// tunnel is not wired (CaddyTunnelIsOnline defaults to false). The routes below
// reproduce the TypeScript try/catch structure exactly, so today:
//
//	GET    /instances/:id/sites                        — no Caddy (list only)
//	POST   /instances/:id/sites                        — no Caddy (row is 'pending')
//	DELETE /instances/:id/sites/:siteId                — Caddy failure swallowed, delete succeeds
//	POST   /instances/:id/sites/:siteId/check-dns      — Caddy only after a matching DNS answer
//	POST   /instances/:id/sites/:siteId/refresh        — 500 {"error":"配置失败: <seam error>"}
//	PATCH  /instances/:id/sites/:siteId                — 500 {"error":"更新失败: <seam error>"} for config changes
//	POST   /instances/:id/sites/:siteId/toggle         — 500 {"error":"操作失败: <seam error>"}
//	GET    /instances/:id/sites/:siteId/certificate    — no Caddy (direct TLS probe)
//
// That is the intended behaviour: a site must never look configured when Caddy was
// never reached.
//
// Auth split (matters, and mirrors the TypeScript decorators): the list and the
// certificate probe use `authenticate` (admins allowed, read-only view rights via
// requireInstanceViewPermission); every mutating route uses `authenticateUser`,
// which rejects admins with 403 USER_ONLY and then only allows the instance owner.
package routes

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== 常量与 schema ====================

const (
	// proxySitesDefaultSiteLimit mirrors the `?? 10` fallback.
	proxySitesDefaultSiteLimit = 10
	// proxySitesRemarkMaxLength mirrors `remark: { type: 'string', maxLength: 100 }`.
	proxySitesRemarkMaxLength = 100
	// proxySitesDomainMinLength mirrors `domain: { type: 'string', minLength: 4 }`.
	proxySitesDomainMinLength = 4
	// proxySitesCertificateTimeout mirrors tls.connect's `timeout: 10000`.
	proxySitesCertificateTimeout = 10 * time.Second
	// proxySitesCertificatePort is the fixed TLS probe port.
	proxySitesCertificatePort = 443
)

// proxySitesCreateFields mirrors the POST body schema, in schema order (AJV runs
// with allErrors:false, so the first failure is the only message).
var proxySitesCreateFields = []ajvFieldSpec{
	{Name: "domain", Kind: ajvKindString, MinLength: proxySitesDomainMinLength, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "targetPort", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 65535},
	{Name: "httpsEnabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "remark", Kind: ajvKindString, MinLength: -1, MaxLength: proxySitesRemarkMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
}

// proxySitesUpdateFields mirrors the PATCH body schema. `httpsEnabled` has no
// default here, so its absence must stay distinguishable from false.
var proxySitesUpdateFields = []ajvFieldSpec{
	{Name: "targetPort", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 65535},
	{Name: "httpsEnabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "remark", Kind: ajvKindString, MinLength: -1, MaxLength: proxySitesRemarkMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
}

// ==================== 小工具 ====================

// proxySitesISOTime mirrors `date.toISOString()`.
func proxySitesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// proxySitesRouteID parses one route parameter with the repository-wide strict
// positive-id rule. Values beyond int4 are rejected as invalid ids: the
// TypeScript would hand them to Prisma, which fails with a database error (500);
// every other Go route treats them as an invalid id (400 INVALID_ID).
func proxySitesRouteID(c *gin.Context, name string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(c.Param(name))
	if !ok || parsed > math.MaxInt32 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return 0, false
	}
	return int32(parsed), true
}

// proxySitesRouteIDs parses `:id` then `:siteId`, short-circuiting exactly like
// the TypeScript's `if (!instanceId || !siteId)`.
func proxySitesRouteIDs(c *gin.Context) (instanceID, siteID int32, ok bool) {
	instanceID, ok = proxySitesRouteID(c, "id")
	if !ok {
		return 0, 0, false
	}
	siteID, ok = proxySitesRouteID(c, "siteId")
	if !ok {
		return 0, 0, false
	}
	return instanceID, siteID, true
}

// proxySitesInternalError mirrors the generic branch of setErrorHandler for an
// unexpected failure: `{error: 'Internal server error'}` with status 500.
func proxySitesInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// proxySitesAuthUser converts the verified claims into the permission model's
// actor shape.
func proxySitesAuthUser(c *gin.Context) lib.AuthUser {
	user := middleware.CurrentUser(c)
	return lib.AuthUser{ID: user.ID, Role: user.Role, Username: user.Username}
}

// proxySitesSiteNotFound writes `reply.code(404).send({ error: '站点不存在' })`.
func proxySitesSiteNotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
}

// proxySitesRespondSuspended mirrors the repeated three-line suspension guard:
// expired gets its own error code, any other suspension the generic one.
func proxySitesRespondSuspended(c *gin.Context, status string, suspendReason *string) bool {
	if status != string(models.InstanceStatusSuspended) {
		return false
	}
	if suspendReason != nil && *suspendReason == "expired" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspendedExpired, "")
		return true
	}
	lib.RespondAPIError(c, http.StatusForbidden, lib.ErrInstanceSuspended, "")
	return true
}

// proxySitesFirstNonEmpty mirrors the JavaScript `a || b` chain used for the
// host's public address (an empty string is falsy, so it falls through).
func proxySitesFirstNonEmpty(values ...*string) *string {
	for _, value := range values {
		if value != nil && *value != "" {
			return value
		}
	}
	return nil
}

// proxySitesHTTPSEnabledDefault mirrors the schema default `httpsEnabled: true`
// combined with AJV's useDefaults. A nil/absent value falls back; an explicit
// false must stay false (tri-state rule, GO_MIGRATION.md §0).
func proxySitesHTTPSEnabledDefault(body ajvValidated, fallback bool) bool {
	if flag, present := body.Values["httpsEnabled"].(bool); present {
		return flag
	}
	return fallback
}

// proxySitesTrimmedRemark mirrors `remark?.trim() || undefined`: the trimmed text
// when non-empty, otherwise nil.
func proxySitesTrimmedRemark(raw string) *string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// proxySitesOptionalStringEqual mirrors JS `a !== b` for string|null values.
func proxySitesOptionalStringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// proxySitesSiteLimit mirrors `instance.site_limit ?? pkg?.site_limit ?? 10`.
//
// site_limit === 0 means "unlimited" and must NOT fall through to the package,
// which is why the pointer null check comes first.
func proxySitesSiteLimit(instance *db.ProxySitesInstanceRow, pkg *db.ProxySitesPackageRow) int32 {
	if instance.SiteLimit != nil {
		return *instance.SiteLimit
	}
	if pkg != nil {
		return pkg.SiteLimit
	}
	return proxySitesDefaultSiteLimit
}

// proxySitesCaddyHostRef adapts the host projection to lib.CaddyHostRef.
// CaddyPort is non-nullable in the schema (default 8444), and the override port is
// 0 so resolveCaddyAdminPort maps the legacy 8444 to 2019.
func proxySitesCaddyHostRef(host db.ProxySitesHostRef) lib.CaddyHostRef {
	enabled := host.CaddyEnabled
	port := int(host.CaddyPort)
	return lib.CaddyHostRef{ID: host.ID, CaddyEnabled: &enabled, CaddyPort: &port}
}

// proxySitesInstanceRef builds the permission model's instance projection.
func proxySitesInstanceRef(userID, hostID int32) lib.InstanceRef {
	return lib.InstanceRef{UserID: userID, HostID: hostID}
}

// proxySitesLog mirrors createLog(user.id, 'proxy', action, content, result, { instanceId }).
func proxySitesLog(c *gin.Context, action, content, result string, instanceID int32) {
	user := middleware.CurrentUser(c)
	userID := int64(user.ID)
	db.CreateLog(c.Request.Context(), &userID, "proxy", action, content, result,
		db.LogOptionsWithInstance(&instanceID))
}

// ==================== GET /instances/:id/sites ====================

// proxySitesListHandler mirrors `fastify.get('/instances/:id/sites', { onRequest: [fastify.authenticate] })`.
func proxySitesListHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := proxySitesRouteID(c, "id")
	if !ok {
		return
	}

	instance, err := db.ProxySitesInstanceByID(ctx, instanceID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	// 权限检查：实例所有者、宿主机所有者、管理员均可查看
	allowed, err := lib.RequireInstanceViewPermission(c, proxySitesAuthUser(c),
		proxySitesInstanceRef(instance.UserID, instance.HostID))
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if !allowed {
		return
	}

	host, err := db.ProxySitesHostByID(ctx, instance.HostID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	sites, err := db.ProxySitesByInstanceID(ctx, instanceID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}

	// 获取站点配额信息
	var pkg *db.ProxySitesPackageRow
	if instance.PackageID != nil {
		pkg, err = db.ProxySitesPackageByID(ctx, *instance.PackageID)
		if err != nil {
			proxySitesInternalError(c)
			return
		}
	}
	siteLimit := proxySitesSiteLimit(instance, pkg)

	items := make([]gin.H, 0, len(sites))
	for _, site := range sites {
		items = append(items, gin.H{
			"id":           site.ID,
			"domain":       site.Domain,
			"targetPort":   site.TargetPort,
			"httpsEnabled": site.HTTPSEnabled,
			"remark":       site.Remark,
			"status":       site.Status,
			"enabled":      site.Enabled,
			"error":        site.Error,
			"createdAt":    proxySitesISOTime(site.CreatedAt),
		})
	}

	// DNS 解析提示信息
	publicAddress := proxySitesFirstNonEmpty(host.NATPublicIP, host.IPAddress)
	var dnsRecordType any
	if publicAddress != nil {
		dnsRecordType = string(lib.GetDNSRecordType(*publicAddress))
	}

	c.JSON(http.StatusOK, gin.H{
		"sites":          items,
		"caddyEnabled":   host.CaddyEnabled,
		"dnsRecordType":  dnsRecordType,
		"dnsRecordValue": publicAddress,
		"siteQuota": gin.H{
			"used":  len(sites),
			"limit": siteLimit,
		},
		// 站点增删改仍保持实例所有者能力，管理员/宿主机所有者在详情页只读查看
		"canManageSites": user.Role != "admin" && instance.UserID == user.ID,
	})
}

// ==================== POST /instances/:id/sites ====================

// proxySitesCreateHandler mirrors `fastify.post('/instances/:id/sites', { onRequest: [fastify.authenticateUser], schema })`.
func proxySitesCreateHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	// Fastify validates the schema before the handler runs, so body errors win
	// over the route-id and ownership checks.
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, []string{"domain", "targetPort"}, proxySitesCreateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, ok := proxySitesRouteID(c, "id")
	if !ok {
		return
	}

	domain, _ := body.stringValue("domain")
	normalizedDomain := strings.ToLower(strings.TrimSpace(domain))
	// 域名格式验证（服务端安全边界：拒绝泛域名与内部主机名）
	if !db.ProxySitesIsValidDomain(normalizedDomain) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "域名格式无效（不支持泛域名）"})
		return
	}

	var targetPort int32
	if number, present := body.Values["targetPort"].(float64); present {
		targetPort = int32(number)
	}
	// `httpsEnabled = true` is the schema default (useDefaults).
	httpsEnabled := proxySitesHTTPSEnabledDefault(body, true)
	var remark *string
	if rawRemark, present := body.stringValue("remark"); present {
		remark = proxySitesTrimmedRemark(rawRemark)
	}

	instance, err := db.ProxySitesInstanceByID(ctx, instanceID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	// 封停状态不允许操作
	if proxySitesRespondSuspended(c, instance.Status, instance.SuspendReason) {
		return
	}

	// 权限检查
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	host, err := db.ProxySitesHostByID(ctx, instance.HostID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	// 检查 Caddy 是否启用
	if !host.CaddyEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先在宿主机安装 Caddy"})
		return
	}

	// 检查站点数量配额
	currentSiteCount, err := db.ProxySitesCountByInstance(ctx, instanceID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	var pkg *db.ProxySitesPackageRow
	if instance.PackageID != nil {
		pkg, err = db.ProxySitesPackageByID(ctx, *instance.PackageID)
		if err != nil {
			proxySitesInternalError(c)
			return
		}
	}
	siteLimit := proxySitesSiteLimit(instance, pkg)

	// siteLimit = 0 表示不限制
	if siteLimit != 0 && currentSiteCount >= int64(siteLimit) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "已达到站点数量上限 (" + proxySitesFormatInt32(siteLimit) + ")",
			"code":  "SITE_LIMIT_EXCEEDED",
		})
		return
	}

	// 检查域名是否已被使用
	domainUsed, err := db.ProxySitesDomainUsed(ctx, host.ID, normalizedDomain)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if domainUsed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该域名已被使用"})
		return
	}

	// 实例需要有内网 IP
	if instance.IPv4 == nil || *instance.IPv4 == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未分配 IPv4 地址"})
		return
	}

	// 在数据库创建记录（状态为 pending，等待 DNS 解析）
	enabled := httpsEnabled
	site, err := db.ProxySitesCreate(ctx, db.ProxySitesCreateData{
		InstanceID:   instanceID,
		HostID:       host.ID,
		Domain:       normalizedDomain,
		TargetPort:   targetPort,
		HTTPSEnabled: &enabled,
		Remark:       remark,
	})
	if err != nil {
		proxySitesInternalError(c)
		return
	}

	// 返回 DNS 解析提示，不立即发送到 Caddy
	dnsRecordValue := ""
	dnsRecordType := lib.DNSRecordTypeA
	if publicAddress := proxySitesFirstNonEmpty(host.NATPublicIP, host.IPAddress); publicAddress != nil {
		dnsRecordValue = *publicAddress
		dnsRecordType = lib.GetDNSRecordType(*publicAddress)
	}

	proxySitesLog(c, "site.create",
		"Created proxy site \""+normalizedDomain+"\" (pending DNS)",
		db.LogResultSuccess, instanceID)

	c.JSON(http.StatusOK, gin.H{
		"site": gin.H{
			"id":           site.ID,
			"domain":       site.Domain,
			"targetPort":   site.TargetPort,
			"httpsEnabled": site.HTTPSEnabled,
			"status":       "pending",
		},
		"dnsHint": gin.H{
			"type":  string(dnsRecordType),
			"host":  normalizedDomain,
			"value": dnsRecordValue,
		},
		"message": "站点已创建，请配置 DNS 后点击“检测 DNS”激活",
	})
}

// proxySitesFormatInt32 renders an int32 the way JavaScript renders a number.
func proxySitesFormatInt32(value int32) string {
	return fmt.Sprintf("%d", value)
}

// ==================== DELETE /instances/:id/sites/:siteId ====================

// proxySitesDeleteHandler mirrors `fastify.delete('/instances/:id/sites/:siteId', { onRequest: [fastify.authenticateUser] })`.
func proxySitesDeleteHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, siteID, ok := proxySitesRouteIDs(c)
	if !ok {
		return
	}

	site, err := db.ProxySitesByID(ctx, siteID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if site == nil {
		proxySitesSiteNotFound(c)
		return
	}

	// 验证站点属于该实例
	if site.InstanceID != instanceID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "站点不属于该实例"})
		return
	}

	// 封停状态不允许操作
	if proxySitesRespondSuspended(c, site.Instance.Status, site.Instance.SuspendReason) {
		return
	}

	// 权限检查
	if site.Instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	// 从 Caddy 删除（经 Agent 隧道）。失败只记录，数据库记录照删，与 TS 的
	// try/catch 一致；隧道未接线时 getCaddyClientForHost 抛错也落在这个 catch 里。
	if site.Host.CaddyEnabled {
		if client, clientErr := lib.GetCaddyClientForHost(proxySitesCaddyHostRef(site.Host), 0); clientErr == nil {
			_ = client.DeleteSite(ctx, site.Domain)
		}
	}

	// 删除数据库记录
	if _, err := db.ProxySitesDelete(ctx, siteID); err != nil {
		proxySitesInternalError(c)
		return
	}

	proxySitesLog(c, "site.delete",
		"Deleted proxy site \""+site.Domain+"\"",
		db.LogResultSuccess, instanceID)

	c.JSON(http.StatusOK, gin.H{"message": "站点已删除"})
}

// ==================== POST /instances/:id/sites/:siteId/check-dns ====================

// proxySitesResolveDomainIPs mirrors `dns.resolve4(domain)` followed by
// `dns.resolve6(domain)`, each independently ignored on failure and concatenated
// in that order. IPv4 answers therefore always precede IPv6 answers, like the
// TypeScript.
func proxySitesResolveDomainIPs(ctx context.Context, domain string) []string {
	resolved := make([]string, 0, 4)
	for _, network := range []string{"ip4", "ip6"} {
		addresses, err := net.DefaultResolver.LookupIP(ctx, network, domain)
		if err != nil {
			// IPv4/IPv6 解析失败
			continue
		}
		for _, address := range addresses {
			resolved = append(resolved, address.String())
		}
	}
	return resolved
}

// proxySitesContainsString mirrors Array.prototype.includes (strict equality).
func proxySitesContainsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// proxySitesCheckDNSHandler mirrors `fastify.post('/instances/:id/sites/:siteId/check-dns', { onRequest: [fastify.authenticateUser] })`.
func proxySitesCheckDNSHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, siteID, ok := proxySitesRouteIDs(c)
	if !ok {
		return
	}

	site, err := db.ProxySitesByID(ctx, siteID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if site == nil || site.InstanceID != instanceID {
		proxySitesSiteNotFound(c)
		return
	}

	// 封停状态不允许操作
	if proxySitesRespondSuspended(c, site.Instance.Status, site.Instance.SuspendReason) {
		return
	}

	if site.Instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	// 获取宿主机公网 IP
	expectedIP := proxySitesFirstNonEmpty(site.Host.NATPublicIP, site.Host.IPAddress)
	if expectedIP == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "宿主机未配置公网 IP"})
		return
	}

	// 检测 DNS 解析
	resolvedIPs := proxySitesResolveDomainIPs(ctx, site.Domain)

	if len(resolvedIPs) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"dnsResolved": false,
			"expectedIp":  *expectedIP,
			"resolvedIps": resolvedIPs,
			"error":       "DNS 未解析，请检查域名配置",
			"status":      site.Status,
		})
		return
	}

	// 检查 IP 是否匹配
	if !proxySitesContainsString(resolvedIPs, *expectedIP) {
		c.JSON(http.StatusOK, gin.H{
			"dnsResolved": true,
			"ipMatches":   false,
			"expectedIp":  *expectedIP,
			"resolvedIps": resolvedIPs,
			"error":       "DNS 解析到 " + strings.Join(resolvedIPs, ", ") + "，但期望的是 " + *expectedIP,
			"status":      site.Status,
		})
		return
	}

	// DNS 正确，如果站点还是 pending 状态，自动激活
	if site.Status == models.ProxySiteStatusPending {
		if activationErr := proxySitesActivate(ctx, site); activationErr != nil {
			message := activationErr.Error()
			if _, err := db.ProxySitesUpdateStatus(ctx, siteID, models.ProxySiteStatusError, &message, nil); err != nil {
				proxySitesInternalError(c)
				return
			}
			// request.log.error(err, 'Failed to activate site after DNS check')

			c.JSON(http.StatusOK, gin.H{
				"dnsResolved": true,
				"ipMatches":   true,
				"expectedIp":  *expectedIP,
				"resolvedIps": resolvedIPs,
				"activated":   false,
				"status":      "error",
				"error":       "激活失败: " + message,
			})
			return
		}

		proxySitesLog(c, "site.activate",
			"Activated proxy site \""+site.Domain+"\" after DNS verification",
			db.LogResultSuccess, instanceID)

		c.JSON(http.StatusOK, gin.H{
			"dnsResolved": true,
			"ipMatches":   true,
			"expectedIp":  *expectedIP,
			"resolvedIps": resolvedIPs,
			"activated":   true,
			"status":      "active",
			"message":     "DNS 验证通过，站点已激活",
		})
		return
	}

	// 站点已经是 active 或 error 状态
	message := "请点击刷新重试配置"
	if site.Status == models.ProxySiteStatusActive {
		message = "站点已激活"
	}
	c.JSON(http.StatusOK, gin.H{
		"dnsResolved": true,
		"ipMatches":   true,
		"expectedIp":  *expectedIP,
		"resolvedIps": resolvedIPs,
		"status":      site.Status,
		"message":     message,
	})
}

// proxySitesActivate mirrors the inner try block of check-dns: delete a possibly
// stale route (failures ignored), add the route for the instance's IPv4, then mark
// the site active — all of it inside the try, so a failure anywhere (including the
// status write) lands in the route's catch.
func proxySitesActivate(ctx context.Context, site *db.ProxySitesSiteDetail) error {
	if !site.Host.CaddyEnabled {
		return errors.New("Caddy 未启用")
	}
	if site.Instance.IPv4 == nil || *site.Instance.IPv4 == "" {
		return errors.New("实例未分配 IP")
	}

	client, err := lib.GetCaddyClientForHost(proxySitesCaddyHostRef(site.Host), 0)
	if err != nil {
		return err
	}
	// 先删除可能存在的旧配置（防止实例删除时 Caddy 删除失败导致残留）；
	// 删除失败不影响后续操作（可能路由不存在）。
	_ = client.DeleteSite(ctx, site.Domain)
	if err := client.AddSite(ctx, site.Domain, *site.Instance.IPv4, int(site.TargetPort), site.HTTPSEnabled); err != nil {
		return err
	}
	_, err = db.ProxySitesUpdateStatus(ctx, site.ID, models.ProxySiteStatusActive, nil, nil)
	return err
}

// ==================== POST /instances/:id/sites/:siteId/refresh ====================

// proxySitesRefreshHandler mirrors `fastify.post('/instances/:id/sites/:siteId/refresh', { onRequest: [fastify.authenticateUser] })`.
func proxySitesRefreshHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, siteID, ok := proxySitesRouteIDs(c)
	if !ok {
		return
	}

	site, err := db.ProxySitesByID(ctx, siteID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if site == nil || site.InstanceID != instanceID {
		proxySitesSiteNotFound(c)
		return
	}

	// 封停状态不允许操作
	if proxySitesRespondSuspended(c, site.Instance.Status, site.Instance.SuspendReason) {
		return
	}

	if site.Instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	// 重新尝试配置. The TypeScript wraps the whole block (including the three
	// early returns below) in try/catch; a returned response is not an error, so
	// the effect is identical to checking the conditions first.
	if !site.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "站点已禁用，请先启用"})
		return
	}
	if !site.Host.CaddyEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Caddy 未启用"})
		return
	}
	if site.Instance.IPv4 == nil || *site.Instance.IPv4 == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未分配 IP"})
		return
	}

	refreshErr := func() error {
		client, err := lib.GetCaddyClientForHost(proxySitesCaddyHostRef(site.Host), 0)
		if err != nil {
			return err
		}
		// 先删除旧路由（避免重复添加）；删除失败不影响后续操作。
		_ = client.DeleteSite(ctx, site.Domain)
		// 重新添加路由
		if err := client.AddSite(ctx, site.Domain, *site.Instance.IPv4, int(site.TargetPort), site.HTTPSEnabled); err != nil {
			return err
		}
		// The TypeScript updates the status inside the try, so a failure here is
		// also reported as "配置失败".
		_, err = db.ProxySitesUpdateStatus(ctx, siteID, models.ProxySiteStatusActive, nil, nil)
		return err
	}()
	if refreshErr != nil {
		message := refreshErr.Error()
		if _, err := db.ProxySitesUpdateStatus(ctx, siteID, models.ProxySiteStatusError, &message, nil); err != nil {
			proxySitesInternalError(c)
			return
		}

		proxySitesLog(c, "site.refresh",
			"Failed to refresh proxy site \""+site.Domain+"\": "+message,
			db.LogResultFailed, instanceID)

		c.JSON(http.StatusInternalServerError, gin.H{"error": "配置失败: " + message})
		return
	}

	proxySitesLog(c, "site.refresh",
		"Refreshed proxy site \""+site.Domain+"\"",
		db.LogResultSuccess, instanceID)

	c.JSON(http.StatusOK, gin.H{"message": "站点配置已刷新", "status": "active"})
}

// ==================== PATCH /instances/:id/sites/:siteId ====================

// proxySitesUpdateHandler mirrors `fastify.patch('/instances/:id/sites/:siteId', { onRequest: [fastify.authenticateUser], schema })`.
func proxySitesUpdateHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	// Body validation runs before the handler in Fastify.
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, nil, proxySitesUpdateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, siteID, ok := proxySitesRouteIDs(c)
	if !ok {
		return
	}

	targetPortValue, targetPortProvided := body.Values["targetPort"].(float64)
	httpsEnabledValue, httpsEnabledProvided := body.Values["httpsEnabled"].(bool)
	remarkRaw, remarkProvided := body.stringValue("remark")

	// 至少需要修改一个字段
	if !targetPortProvided && !httpsEnabledProvided && !remarkProvided {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供要修改的字段"})
		return
	}

	site, err := db.ProxySitesByID(ctx, siteID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if site == nil || site.InstanceID != instanceID {
		proxySitesSiteNotFound(c)
		return
	}

	// 封停状态不允许操作
	if proxySitesRespondSuspended(c, site.Instance.Status, site.Instance.SuspendReason) {
		return
	}

	if site.Instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	// 检查站点是否启用
	if !site.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先启用站点"})
		return
	}

	// 确定最终配置
	newTargetPort := site.TargetPort
	if targetPortProvided {
		newTargetPort = int32(targetPortValue)
	}
	newHTTPSEnabled := site.HTTPSEnabled
	if httpsEnabledProvided {
		newHTTPSEnabled = httpsEnabledValue
	}
	newRemark := site.Remark
	if remarkProvided {
		newRemark = proxySitesTrimmedRemark(remarkRaw)
	}

	// 检查是否只修改了 remark（不需要更新 Caddy）
	onlyRemarkChanged := newTargetPort == site.TargetPort &&
		newHTTPSEnabled == site.HTTPSEnabled &&
		!proxySitesOptionalStringEqual(newRemark, site.Remark)

	if onlyRemarkChanged {
		// 只修改备注，不需要更新 Caddy
		if _, err := db.ProxySitesUpdate(ctx, siteID, db.ProxySitesUpdateData{
			RemarkProvided: true,
			Remark:         newRemark,
		}); err != nil {
			proxySitesInternalError(c)
			return
		}

		proxySitesLog(c, "site.update",
			"Updated proxy site \""+site.Domain+"\" remark",
			db.LogResultSuccess, instanceID)

		c.JSON(http.StatusOK, gin.H{
			"message": "站点备注已更新",
			"site": gin.H{
				"id":           site.ID,
				"domain":       site.Domain,
				"targetPort":   site.TargetPort,
				"httpsEnabled": site.HTTPSEnabled,
				"remark":       newRemark,
				"status":       site.Status,
			},
		})
		return
	}

	// 如果配置没有变化（包括 remark），直接返回
	if newTargetPort == site.TargetPort &&
		newHTTPSEnabled == site.HTTPSEnabled &&
		proxySitesOptionalStringEqual(newRemark, site.Remark) {
		c.JSON(http.StatusOK, gin.H{
			"message": "配置无变化",
			"site": gin.H{
				"id":           site.ID,
				"domain":       site.Domain,
				"targetPort":   site.TargetPort,
				"httpsEnabled": site.HTTPSEnabled,
				"remark":       site.Remark,
				"status":       site.Status,
			},
		})
		return
	}

	// 需要更新 Caddy 配置
	if !site.Host.CaddyEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Caddy 未启用"})
		return
	}
	if site.Instance.IPv4 == nil || *site.Instance.IPv4 == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未分配 IP"})
		return
	}

	// 确定最终配置
	finalTargetPort := site.TargetPort
	if targetPortProvided {
		finalTargetPort = int32(targetPortValue)
	}
	finalHTTPSEnabled := site.HTTPSEnabled
	if httpsEnabledProvided {
		finalHTTPSEnabled = httpsEnabledValue
	}

	// 如果配置没有变化，直接返回（remark 变化已在上面处理，此处与 TS 保持一致）
	if finalTargetPort == site.TargetPort && finalHTTPSEnabled == site.HTTPSEnabled {
		c.JSON(http.StatusOK, gin.H{
			"message": "配置无变化",
			"site": gin.H{
				"id":           site.ID,
				"domain":       site.Domain,
				"targetPort":   site.TargetPort,
				"httpsEnabled": site.HTTPSEnabled,
				"remark":       site.Remark,
				"status":       site.Status,
			},
		})
		return
	}

	updateErr := func() error {
		client, err := lib.GetCaddyClientForHost(proxySitesCaddyHostRef(site.Host), 0)
		if err != nil {
			return err
		}
		// 先删除旧路由
		if err := client.DeleteSite(ctx, site.Domain); err != nil {
			return err
		}
		// 再添加新路由
		return client.AddSite(ctx, site.Domain, *site.Instance.IPv4, int(finalTargetPort), finalHTTPSEnabled)
	}()
	if updateErr != nil {
		message := updateErr.Error()
		if _, err := db.ProxySitesUpdateStatus(ctx, siteID, models.ProxySiteStatusError, &message, nil); err != nil {
			proxySitesInternalError(c)
			return
		}
		// request.log.error(err, 'Failed to update site configuration')
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败: " + message})
		return
	}

	// 更新数据库
	activeStatus := models.ProxySiteStatusActive
	if _, err := db.ProxySitesUpdate(ctx, siteID, db.ProxySitesUpdateData{
		TargetPortProvided:   true,
		TargetPort:           finalTargetPort,
		HTTPSEnabledProvided: true,
		HTTPSEnabled:         finalHTTPSEnabled,
		RemarkProvided:       true,
		Remark:               newRemark,
		StatusProvided:       true,
		Status:               activeStatus,
		ErrorProvided:        true,
		Error:                nil,
	}); err != nil {
		proxySitesInternalError(c)
		return
	}

	proxySitesLog(c, "site.update",
		"Updated proxy site \""+site.Domain+"\": port="+proxySitesFormatInt32(finalTargetPort)+", https="+proxySitesFormatBool(finalHTTPSEnabled),
		db.LogResultSuccess, instanceID)

	c.JSON(http.StatusOK, gin.H{
		"message": "站点配置已更新",
		"site": gin.H{
			"id":           site.ID,
			"domain":       site.Domain,
			"targetPort":   finalTargetPort,
			"httpsEnabled": finalHTTPSEnabled,
			"remark":       newRemark,
			"status":       "active",
		},
	})
}

// proxySitesFormatBool mirrors JavaScript's String(boolean).
func proxySitesFormatBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// ==================== POST /instances/:id/sites/:siteId/toggle ====================

// proxySitesToggleHandler mirrors `fastify.post('/instances/:id/sites/:siteId/toggle', { onRequest: [fastify.authenticateUser] })`.
func proxySitesToggleHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, siteID, ok := proxySitesRouteIDs(c)
	if !ok {
		return
	}

	site, err := db.ProxySitesByID(ctx, siteID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if site == nil || site.InstanceID != instanceID {
		proxySitesSiteNotFound(c)
		return
	}

	// 封停状态不允许操作
	if proxySitesRespondSuspended(c, site.Instance.Status, site.Instance.SuspendReason) {
		return
	}

	if site.Instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	// 检查 Caddy 是否启用
	if !site.Host.CaddyEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Caddy 未启用"})
		return
	}

	newEnabled := !site.Enabled

	// The TS returns 400 from INSIDE the try block when the instance has no IPv4
	// on the enable path; a return is not an exception, so it reaches the client
	// unchanged instead of the catch's 500.
	var instanceMissingIP bool
	operationErr := func() error {
		client, err := lib.GetCaddyClientForHost(proxySitesCaddyHostRef(site.Host), 0)
		if err != nil {
			return err
		}

		if newEnabled {
			// 启用站点：添加 Caddy 配置
			if site.Instance.IPv4 == nil || *site.Instance.IPv4 == "" {
				instanceMissingIP = true
				return nil
			}
			// 先删除可能存在的旧配置（防止残留）
			_ = client.DeleteSite(ctx, site.Domain)
			if err := client.AddSite(ctx, site.Domain, *site.Instance.IPv4, int(site.TargetPort), site.HTTPSEnabled); err != nil {
				return err
			}
			_, err := db.ProxySitesUpdateStatus(ctx, siteID, models.ProxySiteStatusActive, nil, &newEnabled)
			return err
		}

		// 禁用站点：删除 Caddy 配置
		if err := client.DeleteSite(ctx, site.Domain); err != nil {
			return err
		}
		// 设为 pending 表示未激活
		_, err = db.ProxySitesUpdateStatus(ctx, siteID, models.ProxySiteStatusPending, nil, &newEnabled)
		return err
	}()

	if instanceMissingIP {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未分配 IP"})
		return
	}
	if operationErr != nil {
		message := operationErr.Error()
		// request.log.error(err, `Failed to ${newEnabled ? 'enable' : 'disable'} site`)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "操作失败: " + message})
		return
	}

	action := "site.disable"
	content := "Disabled proxy site \"" + site.Domain + "\""
	if newEnabled {
		action = "site.enable"
		content = "Enabled proxy site \"" + site.Domain + "\""
	}
	proxySitesLog(c, action, content, db.LogResultSuccess, instanceID)

	message := "站点已禁用"
	status := "pending"
	if newEnabled {
		message = "站点已启用"
		status = "active"
	}
	c.JSON(http.StatusOK, gin.H{
		"message": message,
		"enabled": newEnabled,
		"status":  status,
	})
}

// ==================== GET /instances/:id/sites/:siteId/certificate ====================

// proxySitesCertificate mirrors the certificate object of the TLS probe.
type proxySitesCertificate struct {
	Valid         bool
	Issuer        string
	Subject       string
	ValidFrom     string
	ValidTo       string
	DaysRemaining int
}

// proxySitesCertificateHandler mirrors `fastify.get('/instances/:id/sites/:siteId/certificate', { onRequest: [fastify.authenticate] })`.
func proxySitesCertificateHandler(c *gin.Context) {
	ctx := c.Request.Context()

	instanceID, siteID, ok := proxySitesRouteIDs(c)
	if !ok {
		return
	}

	site, err := db.ProxySitesByID(ctx, siteID)
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if site == nil || site.InstanceID != instanceID {
		proxySitesSiteNotFound(c)
		return
	}

	// 证书状态属于查看类信息，允许实例所有者、宿主机所有者、管理员查看
	allowed, err := lib.RequireInstanceViewPermission(c, proxySitesAuthUser(c),
		proxySitesInstanceRef(site.Instance.UserID, site.Instance.HostID))
	if err != nil {
		proxySitesInternalError(c)
		return
	}
	if !allowed {
		return
	}

	// 如果未启用 HTTPS，返回特定状态
	if !site.HTTPSEnabled {
		c.JSON(http.StatusOK, gin.H{
			"httpsEnabled": false,
			"status":       "disabled",
			"message":      "HTTPS 未启用",
		})
		return
	}

	// 如果站点还是 pending 状态，返回提示
	if site.Status == models.ProxySiteStatusPending {
		c.JSON(http.StatusOK, gin.H{
			"httpsEnabled": true,
			"status":       "pending",
			"hint":         "站点待激活，请先配置 DNS 并点击“检测 DNS”",
		})
		return
	}

	// 尝试通过 TLS 连接获取证书信息
	certificate, err := proxySitesProbeCertificate(ctx, site.Domain)
	if err != nil {
		status, hint := proxySitesCertificateFailure(err)
		c.JSON(http.StatusOK, gin.H{
			"httpsEnabled": true,
			"status":       status,
			"error":        err.Error(),
			"hint":         hint,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"httpsEnabled": true,
		"status":       "valid",
		"certificate": gin.H{
			"valid":         certificate.Valid,
			"issuer":        certificate.Issuer,
			"subject":       certificate.Subject,
			"validFrom":     certificate.ValidFrom,
			"validTo":       certificate.ValidTo,
			"daysRemaining": certificate.DaysRemaining,
		},
	})
}

// proxySitesProbeCertificate mirrors the resolvePublicHostname + tls.connect block.
// The hostname is resolved through lib.ResolvePublicHostname (the SSRF guard the
// TypeScript uses), then the FIRST resolved address is dialled on 443 with
// SNI = domain and certificate verification disabled, exactly like
// `rejectUnauthorized: false`.
func proxySitesProbeCertificate(ctx context.Context, domain string) (proxySitesCertificate, error) {
	addresses, err := lib.ResolvePublicHostname(ctx, domain)
	if err != nil {
		return proxySitesCertificate{}, err
	}
	if len(addresses) == 0 {
		return proxySitesCertificate{}, errors.New("无法获取证书信息")
	}
	address := addresses[0]

	dialer := &net.Dialer{Timeout: proxySitesCertificateTimeout}
	connection, err := dialer.DialContext(ctx, "tcp",
		net.JoinHostPort(address.Address, proxySitesFormatPort(proxySitesCertificatePort)))
	if err != nil {
		return proxySitesCertificate{}, proxySitesNormalizeNetError(err, address.Address)
	}

	tlsConnection := tls.Client(connection, &tls.Config{
		ServerName: domain,
		// 允许自签名证书
		InsecureSkipVerify: true,
	})
	_ = tlsConnection.SetDeadline(time.Now().Add(proxySitesCertificateTimeout))
	handshakeErr := tlsConnection.HandshakeContext(ctx)
	if handshakeErr != nil {
		_ = connection.Close()
		return proxySitesCertificate{}, proxySitesNormalizeNetError(handshakeErr, address.Address)
	}
	defer func() { _ = tlsConnection.Close() }()

	state := tlsConnection.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return proxySitesCertificate{}, errors.New("无法获取证书信息")
	}
	leaf := state.PeerCertificates[0]

	now := time.Now()
	valid := !now.Before(leaf.NotBefore) && !now.After(leaf.NotAfter)
	daysRemaining := int(math.Floor(leaf.NotAfter.Sub(now).Hours() / 24))

	// 获取颁发者信息
	issuerParts := make([]string, 0, 2)
	if organization := leaf.Issuer.Organization; len(organization) > 0 && organization[0] != "" {
		issuerParts = append(issuerParts, organization[0])
	}
	if leaf.Issuer.CommonName != "" {
		issuerParts = append(issuerParts, leaf.Issuer.CommonName)
	}
	issuer := strings.Join(issuerParts, " - ")
	if issuer == "" {
		issuer = "Unknown"
	}
	subject := leaf.Subject.CommonName
	if subject == "" {
		subject = domain
	}

	return proxySitesCertificate{
		Valid:         valid,
		Issuer:        issuer,
		Subject:       subject,
		ValidFrom:     proxySitesISOTime(leaf.NotBefore),
		ValidTo:       proxySitesISOTime(leaf.NotAfter),
		DaysRemaining: daysRemaining,
	}, nil
}

// proxySitesFormatPort renders the fixed probe port.
func proxySitesFormatPort(port int) string {
	return fmt.Sprintf("%d", port)
}

// proxySitesNormalizeNetError rewrites Go network errors into the Node-style
// message the TypeScript classifies (and returns to the client). The classification
// below switches on substrings, so the translation is what keeps status/hint
// identical to Node.
func proxySitesNormalizeNetError(err error, address string) error {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		// Node's socket 'timeout' handler rejects with this literal.
		return errors.New("连接超时")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("连接超时")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("connect ECONNREFUSED %s:%d", address, proxySitesCertificatePort)
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return errors.New("read ECONNRESET")
	}
	return err
}

// proxySitesCertificateFailure mirrors the TypeScript's error-type analysis. The
// order of the branches is observable (a message containing both "timeout" and
// "handshake" is a timeout), so it is preserved verbatim.
func proxySitesCertificateFailure(err error) (string, string) {
	message := err.Error()

	switch {
	case strings.Contains(message, "ENOTFOUND") || strings.Contains(message, "getaddrinfo"):
		return "dns_error", "DNS 未解析，请检查域名是否正确指向服务器"
	case strings.Contains(message, "ECONNREFUSED"):
		return "connection_refused", "连接被拒绝，请检查服务器 443 端口是否开放"
	case strings.Contains(message, "timeout") || strings.Contains(message, "超时"):
		return "timeout", "连接超时，请检查网络连接"
	case strings.Contains(message, "handshake") || strings.Contains(message, "SSL") || strings.Contains(message, "TLS"):
		return "cert_pending", "证书正在申请中，请稍后重试"
	case strings.Contains(message, "certificate") || strings.Contains(message, "self signed"):
		return "cert_pending", "证书正在申请中，请稍后重试"
	case strings.Contains(message, "ECONNRESET"):
		return "cert_pending", "证书可能正在申请中，请稍后重试"
	}

	return "error", ""
}

// ==================== 注册 ====================

// RegisterProxySites mounts the proxy-site routes under /api. The TypeScript
// registers the module with `prefix: '/api'` and declares no route at the prefix
// root, so there is no "" / "/" pair to mount here.
//
// Auth is attached per route on purpose: the list and certificate probe use
// `authenticate`, the mutations use `authenticateUser` (regular users only), and a
// single group cannot express both.
func RegisterProxySites(r *gin.Engine) {
	api := r.Group("/api")

	api.GET("/instances/:id/sites", middleware.Authenticate(), proxySitesListHandler)
	api.POST("/instances/:id/sites", middleware.AuthenticateUser(), proxySitesCreateHandler)
	api.DELETE("/instances/:id/sites/:siteId", middleware.AuthenticateUser(), proxySitesDeleteHandler)
	api.POST("/instances/:id/sites/:siteId/check-dns", middleware.AuthenticateUser(), proxySitesCheckDNSHandler)
	api.POST("/instances/:id/sites/:siteId/refresh", middleware.AuthenticateUser(), proxySitesRefreshHandler)
	api.PATCH("/instances/:id/sites/:siteId", middleware.AuthenticateUser(), proxySitesUpdateHandler)
	api.POST("/instances/:id/sites/:siteId/toggle", middleware.AuthenticateUser(), proxySitesToggleHandler)
	api.GET("/instances/:id/sites/:siteId/certificate", middleware.Authenticate(), proxySitesCertificateHandler)
}

func init() {
	Register(RegisterProxySites)
}
