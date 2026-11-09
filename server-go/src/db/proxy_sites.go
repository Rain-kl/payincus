// Port of server/src/db/proxy-sites.ts.
//
// The module is the persistence half of the reverse-proxy site feature. Two things
// in it are load-bearing beyond plain CRUD:
//
//   - ProxySitesIsValidDomain is a SECURITY boundary. It is the server-side check
//     that refuses wildcard domains and any value that is not a public-looking DNS
//     name (an internal host such as "localhost", "127.0.0.1" or "[::1]" must not
//     become a Caddy route). The TypeScript regex uses a lookbehind, which RE2 does
//     not support, so it is reimplemented label-by-label below; the tests pin the
//     regex's exact accept/reject set, including its quirks.
//   - the (hostId, domain) unique key is what makes "该域名已被使用" a real guard
//     rather than a best-effort pre-check.
//
// server-go/src/lib already imports server-go/src/db, so db must not import lib.
// isIpLiteral() is therefore mirrored locally (proxySitesIsIPLiteral) exactly like
// db/images.go mirrors normalizeArchitecture.
package db

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== 类型 ====================

// ProxySitesCreateData mirrors CreateProxySiteData.
type ProxySitesCreateData struct {
	InstanceID int32
	HostID     int32
	Domain     string
	TargetPort int32
	// HTTPSEnabled mirrors `httpsEnabled?: boolean`. nil means "use the default
	// true"; explicit false must stay false (tri-state, see GO_MIGRATION.md §0).
	HTTPSEnabled *bool
	// Remark mirrors `remark?: string`. The TypeScript stores `remark || null`, so
	// an empty string becomes NULL rather than ''.
	Remark *string
}

// ProxySitesUpdateData mirrors the `data` argument of updateProxySite. Every
// field carries its own "provided" bit because Prisma distinguishes `undefined`
// (leave the column alone) from `null` (write NULL).
type ProxySitesUpdateData struct {
	TargetPortProvided   bool
	TargetPort           int32
	HTTPSEnabledProvided bool
	HTTPSEnabled         bool
	RemarkProvided       bool
	Remark               *string
	StatusProvided       bool
	Status               models.ProxySiteStatus
	ErrorProvided        bool
	Error                *string
}

// ProxySitesInstanceRef mirrors the `instance` include of getProxySiteById:
// select { id, name, ipv4, hostId, userId, status, suspendReason }.
type ProxySitesInstanceRef struct {
	ID            int32
	Name          string
	IPv4          *string
	HostID        int32
	UserID        int32
	Status        string
	SuspendReason *string
}

// ProxySitesHostRef mirrors the `host` include of getProxySiteById:
// select { id, name, natPublicIp, ipAddress, caddyEnabled, caddyUsername,
// caddyPassword, caddyPort }.
type ProxySitesHostRef struct {
	ID            int32
	Name          string
	NATPublicIP   *string
	IPAddress     *string
	CaddyEnabled  bool
	CaddyUsername *string
	CaddyPassword *string
	CaddyPort     int32
}

// ProxySitesSiteDetail is what getProxySiteById returns: the site row plus both
// includes. The embedded model provides the site columns.
type ProxySitesSiteDetail struct {
	models.ProxySite
	Instance ProxySitesInstanceRef
	Host     ProxySitesHostRef
}

// ProxySitesInstanceRow is the narrow projection the routes need from
// `db.getInstanceById` (db/instances.ts). That module belongs to the high-risk
// Phase 6 wave, so only the columns the proxy-sites routes read are selected.
type ProxySitesInstanceRow struct {
	ID            int32
	UserID        int32
	HostID        int32
	PackageID     *int32
	IPv4          *string
	Status        string
	SuspendReason *string
	SiteLimit     *int32
}

// ProxySitesHostRow is the narrow projection of `db.getHostById` (db/hosts.ts):
// only the columns the proxy-sites routes read.
type ProxySitesHostRow struct {
	ID           int32
	NATPublicIP  *string
	IPAddress    *string
	CaddyEnabled bool
	CaddyPort    int32
}

// ProxySitesPackageRow is the narrow projection of `db.getPackageById`
// (db/packages.ts); the routes only read site_limit.
type ProxySitesPackageRow struct {
	ID        int32
	SiteLimit int32
}

// ProxySitesInstanceBrief mirrors the `instance` include of
// getProxySitesByHostId: select { id, name, ipv4, status }.
type ProxySitesInstanceBrief struct {
	ID     int32
	Name   string
	IPv4   *string
	Status string
}

// ProxySitesHostSite mirrors getProxySitesByHostId's result shape: the site plus
// the brief instance include.
type ProxySitesHostSite struct {
	models.ProxySite
	Instance ProxySitesInstanceBrief
}

// ==================== 站点 CRUD ====================

// ProxySitesCreate mirrors createProxySite(). Prisma fills `createdAt`,
// `updatedAt`, `status` and `enabled` from the schema; they are written
// explicitly here so the returned row carries the same values (createdAt comes
// back from the database through RETURNING, like Prisma's create()).
func ProxySitesCreate(ctx context.Context, data ProxySitesCreateData) (models.ProxySite, error) {
	httpsEnabled := true
	if data.HTTPSEnabled != nil {
		httpsEnabled = *data.HTTPSEnabled
	}
	remark := data.Remark
	if remark != nil && *remark == "" {
		remark = nil
	}

	now := time.Now().UTC()
	site := models.ProxySite{
		InstanceID:   data.InstanceID,
		HostID:       data.HostID,
		Domain:       data.Domain,
		TargetPort:   data.TargetPort,
		HTTPSEnabled: httpsEnabled,
		Remark:       remark,
		Status:       models.ProxySiteStatusPending,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// The columns are `timestamp(3)` without time zone and Node renders them with
	// Date#toISOString (always UTC); writing UTC keeps the round trip exact.
	if err := DB.WithContext(ctx).Clauses(clause.Returning{}).Create(&site).Error; err != nil {
		return models.ProxySite{}, err
	}
	return site, nil
}

// ProxySitesByInstanceID mirrors
// `proxySite.findMany({ where: { instanceId }, orderBy: { createdAt: 'desc' } })`.
// An empty result is an empty slice (a JSON `[]`), never nil, because the route
// maps it.
func ProxySitesByInstanceID(ctx context.Context, instanceID int32) ([]models.ProxySite, error) {
	sites := make([]models.ProxySite, 0, 8)
	err := DB.WithContext(ctx).
		Where("instance_id = ?", instanceID).
		Order("created_at DESC").
		Find(&sites).Error
	if err != nil {
		return nil, err
	}
	return sites, nil
}

// ProxySitesByHostID mirrors getProxySitesByHostId().
func ProxySitesByHostID(ctx context.Context, hostID int32) ([]ProxySitesHostSite, error) {
	sites := make([]models.ProxySite, 0, 8)
	if err := DB.WithContext(ctx).
		Where("host_id = ?", hostID).
		Order("created_at DESC").
		Find(&sites).Error; err != nil {
		return nil, err
	}

	results := make([]ProxySitesHostSite, 0, len(sites))
	for _, site := range sites {
		brief, err := proxySitesInstanceBrief(ctx, site.InstanceID)
		if err != nil {
			return nil, err
		}
		if brief == nil {
			// Impossible while the FK holds; Prisma would return `instance: null`.
			return nil, fmt.Errorf("代理站点 %d 的实例 %d 不存在", site.ID, site.InstanceID)
		}
		results = append(results, ProxySitesHostSite{ProxySite: site, Instance: *brief})
	}
	return results, nil
}

// ProxySitesByID mirrors getProxySiteById(). A missing row returns (nil, nil);
// the includes are separate queries exactly like Prisma's `include`.
func ProxySitesByID(ctx context.Context, id int32) (*ProxySitesSiteDetail, error) {
	sites := make([]models.ProxySite, 0, 1)
	if err := DB.WithContext(ctx).
		Where("id = ?", id).
		Limit(1).
		Find(&sites).Error; err != nil {
		return nil, err
	}
	if len(sites) == 0 {
		return nil, nil
	}
	site := sites[0]

	instance, err := proxySitesInstanceRef(ctx, site.InstanceID)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		// Impossible while the FK holds: Prisma would return `instance: null` and
		// every route would then throw on `site.instance.status`. Failing loudly is
		// preferable to serving a site whose owner cannot be resolved.
		return nil, fmt.Errorf("代理站点 %d 的实例 %d 不存在", site.ID, site.InstanceID)
	}
	host, err := proxySitesHostRef(ctx, site.HostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, fmt.Errorf("代理站点 %d 的宿主机 %d 不存在", site.ID, site.HostID)
	}
	return &ProxySitesSiteDetail{ProxySite: site, Instance: *instance, Host: *host}, nil
}

// ProxySitesByDomain mirrors
// `findUnique({ where: { hostId_domain: { hostId, domain } } })`.
func ProxySitesByDomain(ctx context.Context, hostID int32, domain string) (*models.ProxySite, error) {
	sites := make([]models.ProxySite, 0, 1)
	if err := DB.WithContext(ctx).
		Where("host_id = ? AND domain = ?", hostID, domain).
		Limit(1).
		Find(&sites).Error; err != nil {
		return nil, err
	}
	if len(sites) == 0 {
		return nil, nil
	}
	return &sites[0], nil
}

// ProxySitesUpdateStatus mirrors updateProxySiteStatus(id, status, error?,
// enabled?): `error` is written even when absent (`error ?? null`), so the column
// is cleared on a successful activation; `enabled` is only touched when provided.
func ProxySitesUpdateStatus(
	ctx context.Context,
	id int32,
	status models.ProxySiteStatus,
	statusError *string,
	enabled *bool,
) (models.ProxySite, error) {
	updates := map[string]any{
		"status":     string(status),
		"error":      statusError,
		"updated_at": time.Now().UTC(),
	}
	if enabled != nil {
		updates["enabled"] = *enabled
	}

	var site models.ProxySite
	result := DB.WithContext(ctx).
		Model(&site).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Updates(updates)
	if result.Error != nil {
		return models.ProxySite{}, result.Error
	}
	if result.RowsAffected == 0 {
		// Prisma's update() throws P2025 for a missing row.
		return models.ProxySite{}, fmt.Errorf("代理站点 %d 不存在", id)
	}
	return site, nil
}

// ProxySitesUpdate mirrors updateProxySite().
//
// The route always sends at least one field (it early-returns before calling
// this), and the TypeScript has no empty-update guard of its own, so none is
// added here — an empty `data` would still bump updated_at, exactly like Prisma.
func ProxySitesUpdate(ctx context.Context, id int32, data ProxySitesUpdateData) (models.ProxySite, error) {
	updates := map[string]any{
		// Prisma's @updatedAt always bumps the column.
		"updated_at": time.Now().UTC(),
	}
	if data.TargetPortProvided {
		updates["target_port"] = data.TargetPort
	}
	if data.HTTPSEnabledProvided {
		updates["https_enabled"] = data.HTTPSEnabled
	}
	if data.RemarkProvided {
		updates["remark"] = data.Remark
	}
	if data.StatusProvided {
		updates["status"] = string(data.Status)
	}
	if data.ErrorProvided {
		updates["error"] = data.Error
	}

	var site models.ProxySite
	result := DB.WithContext(ctx).
		Model(&site).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Updates(updates)
	if result.Error != nil {
		return models.ProxySite{}, result.Error
	}
	if result.RowsAffected == 0 {
		return models.ProxySite{}, fmt.Errorf("代理站点 %d 不存在", id)
	}
	return site, nil
}

// ProxySitesToggleEnabled mirrors toggleProxySiteEnabled(). The route no longer
// calls it (the flag is updated inside updateProxySiteStatus), but the function
// stays part of the module's surface.
func ProxySitesToggleEnabled(ctx context.Context, id int32, enabled bool) (models.ProxySite, error) {
	var site models.ProxySite
	result := DB.WithContext(ctx).
		Model(&site).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"enabled":    enabled,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return models.ProxySite{}, result.Error
	}
	if result.RowsAffected == 0 {
		return models.ProxySite{}, fmt.Errorf("代理站点 %d 不存在", id)
	}
	return site, nil
}

// ProxySitesDelete mirrors deleteProxySite(): Prisma's delete() returns the row
// and throws when it does not exist.
func ProxySitesDelete(ctx context.Context, id int32) (models.ProxySite, error) {
	var site models.ProxySite
	result := DB.WithContext(ctx).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Delete(&site)
	if result.Error != nil {
		return models.ProxySite{}, result.Error
	}
	if result.RowsAffected == 0 {
		return models.ProxySite{}, fmt.Errorf("代理站点 %d 不存在", id)
	}
	return site, nil
}

// ProxySitesDomainUsed mirrors isDomainUsed(): the unique (hostId, domain) key
// lookup.
func ProxySitesDomainUsed(ctx context.Context, hostID int32, domain string) (bool, error) {
	site, err := ProxySitesByDomain(ctx, hostID, domain)
	if err != nil {
		return false, err
	}
	return site != nil, nil
}

// ProxySitesCountByInstance mirrors `proxySite.count({ where: { instanceId } })`.
func ProxySitesCountByInstance(ctx context.Context, instanceID int32) (int64, error) {
	var count int64
	err := DB.WithContext(ctx).
		Model(&models.ProxySite{}).
		Where("instance_id = ?", instanceID).
		Count(&count).Error
	return count, err
}

// ProxySitesCountByHost mirrors `proxySite.count({ where: { hostId } })`.
func ProxySitesCountByHost(ctx context.Context, hostID int32) (int64, error) {
	var count int64
	err := DB.WithContext(ctx).
		Model(&models.ProxySite{}).
		Where("host_id = ?", hostID).
		Count(&count).Error
	return count, err
}

// ==================== 域名校验（安全边界） ====================

// ProxySitesIsValidDomain mirrors isValidDomain(domain): no wildcard, and the
// value must be a DNS name whose final label is a letters-only TLD of two or
// more characters.
//
// The TypeScript regex is
//
//	/^(?!-)[A-Za-z0-9-]{1,63}(?<!-)(\.[A-Za-z0-9-]{1,63})*\.[A-Za-z]{2,}$/
//
// RE2 has no lookbehind, so the same grammar is checked label by label:
//
//	first label  [A-Za-z0-9-]{1,63}, no leading hyphen (?!-), no trailing hyphen (?<!-)
//	middle       \.[A-Za-z0-9-]{1,63} — leading/trailing hyphens ARE accepted
//	last label   \.[A-Za-z]{2,}       — letters only, at least two, no upper bound
//
// That asymmetry (middle labels may start or end with '-') is a quirk of the
// original regex, not an intention; it is reproduced because the Node backend is
// still the reference implementation. Internal targets are rejected as a side
// effect: "localhost" has no dot, "127.0.0.1" ends in a digit-only label and
// "[::1]" contains characters outside the class.
func ProxySitesIsValidDomain(domain string) bool {
	// 不允许泛域名（以 * 开头）
	if strings.HasPrefix(domain, "*") {
		return false
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}

	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	for index := 0; index < len(tld); index++ {
		if !proxySitesIsASCIILetter(tld[index]) {
			return false
		}
	}

	for index, label := range labels[:len(labels)-1] {
		if len(label) < 1 || len(label) > 63 {
			return false
		}
		for position := 0; position < len(label); position++ {
			character := label[position]
			if !proxySitesIsASCIILetter(character) &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
		if index == 0 && (label[0] == '-' || label[len(label)-1] == '-') {
			return false
		}
	}
	return true
}

func proxySitesIsASCIILetter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
}

// ProxySitesIsIPAddress mirrors isIpAddress(), which is isIpLiteral().
func ProxySitesIsIPAddress(value string) bool {
	return proxySitesIsIPLiteral(value)
}

// proxySitesIsIPLiteral mirrors lib.isIpLiteral without importing lib (db -> lib
// would be an import cycle). Kept in sync with lib/network_address.go: a zone
// index is accepted ("fe80::1%eth0"), a prefix length is not ("10.0.0.0/8"), and
// a bracketed IPv6 literal is unwrapped first.
func proxySitesIsIPLiteral(value string) bool {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.ContainsRune(inner, ':') {
			trimmed = inner
		}
	}
	if trimmed == "" || strings.ContainsRune(trimmed, '/') {
		return false
	}
	parsed, err := netip.ParseAddr(trimmed)
	if err != nil {
		return false
	}
	return parsed.Is4() || parsed.Is6()
}

// ==================== 窄投影查询 ====================

// ProxySitesInstanceByID mirrors the `db.getInstanceById` slice the routes use.
// found is false when the instance does not exist.
func ProxySitesInstanceByID(ctx context.Context, id int32) (*ProxySitesInstanceRow, error) {
	rows := make([]struct {
		ID            int32   `gorm:"column:id"`
		UserID        int32   `gorm:"column:user_id"`
		HostID        int32   `gorm:"column:host_id"`
		PackageID     *int32  `gorm:"column:package_id"`
		IPv4          *string `gorm:"column:ipv4"`
		Status        string  `gorm:"column:status"`
		SuspendReason *string `gorm:"column:suspend_reason"`
		SiteLimit     *int32  `gorm:"column:site_limit"`
	}, 0, 1)
	err := DB.WithContext(ctx).
		Table("instances").
		Select("id, user_id, host_id, package_id, ipv4, status, suspend_reason, site_limit").
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &ProxySitesInstanceRow{
		ID:            row.ID,
		UserID:        row.UserID,
		HostID:        row.HostID,
		PackageID:     row.PackageID,
		IPv4:          row.IPv4,
		Status:        row.Status,
		SuspendReason: row.SuspendReason,
		SiteLimit:     row.SiteLimit,
	}, nil
}

// ProxySitesHostByID mirrors the `db.getHostById` slice the routes use.
func ProxySitesHostByID(ctx context.Context, id int32) (*ProxySitesHostRow, error) {
	rows := make([]struct {
		ID           int32   `gorm:"column:id"`
		NATPublicIP  *string `gorm:"column:nat_public_ip"`
		IPAddress    *string `gorm:"column:ip_address"`
		CaddyEnabled bool    `gorm:"column:caddy_enabled"`
		CaddyPort    int32   `gorm:"column:caddy_port"`
	}, 0, 1)
	err := DB.WithContext(ctx).
		Table("hosts").
		Select("id, nat_public_ip, ip_address, caddy_enabled, caddy_port").
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &ProxySitesHostRow{
		ID:           row.ID,
		NATPublicIP:  row.NATPublicIP,
		IPAddress:    row.IPAddress,
		CaddyEnabled: row.CaddyEnabled,
		CaddyPort:    row.CaddyPort,
	}, nil
}

// ProxySitesPackageByID mirrors the `db.getPackageById` slice the routes use.
func ProxySitesPackageByID(ctx context.Context, id int32) (*ProxySitesPackageRow, error) {
	rows := make([]struct {
		ID        int32 `gorm:"column:id"`
		SiteLimit int32 `gorm:"column:site_limit"`
	}, 0, 1)
	err := DB.WithContext(ctx).
		Table("packages").
		Select("id, site_limit").
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &ProxySitesPackageRow{ID: rows[0].ID, SiteLimit: rows[0].SiteLimit}, nil
}

// proxySitesInstanceRef reads the include projection of getProxySiteById.
func proxySitesInstanceRef(ctx context.Context, id int32) (*ProxySitesInstanceRef, error) {
	rows := make([]struct {
		ID            int32   `gorm:"column:id"`
		Name          string  `gorm:"column:name"`
		IPv4          *string `gorm:"column:ipv4"`
		HostID        int32   `gorm:"column:host_id"`
		UserID        int32   `gorm:"column:user_id"`
		Status        string  `gorm:"column:status"`
		SuspendReason *string `gorm:"column:suspend_reason"`
	}, 0, 1)
	err := DB.WithContext(ctx).
		Table("instances").
		Select("id, name, ipv4, host_id, user_id, status, suspend_reason").
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &ProxySitesInstanceRef{
		ID:            rows[0].ID,
		Name:          rows[0].Name,
		IPv4:          rows[0].IPv4,
		HostID:        rows[0].HostID,
		UserID:        rows[0].UserID,
		Status:        rows[0].Status,
		SuspendReason: rows[0].SuspendReason,
	}, nil
}

// proxySitesInstanceBrief reads the include projection of getProxySitesByHostId.
func proxySitesInstanceBrief(ctx context.Context, id int32) (*ProxySitesInstanceBrief, error) {
	rows := make([]struct {
		ID     int32   `gorm:"column:id"`
		Name   string  `gorm:"column:name"`
		IPv4   *string `gorm:"column:ipv4"`
		Status string  `gorm:"column:status"`
	}, 0, 1)
	err := DB.WithContext(ctx).
		Table("instances").
		Select("id, name, ipv4, status").
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &ProxySitesInstanceBrief{
		ID:     rows[0].ID,
		Name:   rows[0].Name,
		IPv4:   rows[0].IPv4,
		Status: rows[0].Status,
	}, nil
}

// proxySitesHostRef reads the include projection of getProxySiteById.
func proxySitesHostRef(ctx context.Context, id int32) (*ProxySitesHostRef, error) {
	rows := make([]struct {
		ID            int32   `gorm:"column:id"`
		Name          string  `gorm:"column:name"`
		NATPublicIP   *string `gorm:"column:nat_public_ip"`
		IPAddress     *string `gorm:"column:ip_address"`
		CaddyEnabled  bool    `gorm:"column:caddy_enabled"`
		CaddyUsername *string `gorm:"column:caddy_username"`
		CaddyPassword *string `gorm:"column:caddy_password"`
		CaddyPort     int32   `gorm:"column:caddy_port"`
	}, 0, 1)
	err := DB.WithContext(ctx).
		Table("hosts").
		Select("id, name, nat_public_ip, ip_address, caddy_enabled, caddy_username, caddy_password, caddy_port").
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &ProxySitesHostRef{
		ID:            rows[0].ID,
		Name:          rows[0].Name,
		NATPublicIP:   rows[0].NATPublicIP,
		IPAddress:     rows[0].IPAddress,
		CaddyEnabled:  rows[0].CaddyEnabled,
		CaddyUsername: rows[0].CaddyUsername,
		CaddyPassword: rows[0].CaddyPassword,
		CaddyPort:     rows[0].CaddyPort,
	}, nil
}
