package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

type readinessLevel string

const (
	levelWarn readinessLevel = "warn"
	levelFail readinessLevel = "fail"
)

type readinessFinding struct {
	level   readinessLevel
	message string
}

// verifyProductionDatabase runs the production DB readiness checks (parity port
// of server/src/scripts/verify-production-db-readiness.ts).
func verifyProductionDatabase(ctx context.Context) int {
	var findings []readinessFinding
	addFail := func(msg string) { findings = append(findings, readinessFinding{level: levelFail, message: msg}) }
	addWarn := func(msg string) { findings = append(findings, readinessFinding{level: levelWarn, message: msg}) }

	if err := checkDatabaseConnectivity(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[verify-production-db] ERROR: %v\n", err)
		return 1
	}

	if err := checkPaymentProviders(ctx, addFail, addWarn); err != nil {
		fmt.Fprintf(os.Stderr, "[verify-production-db] ERROR: %v\n", err)
		return 1
	}
	if err := checkSMTPConfig(ctx, addFail, addWarn); err != nil {
		fmt.Fprintf(os.Stderr, "[verify-production-db] ERROR: %v\n", err)
		return 1
	}
	if err := checkLskyConfig(ctx, addFail, addWarn); err != nil {
		fmt.Fprintf(os.Stderr, "[verify-production-db] ERROR: %v\n", err)
		return 1
	}
	if err := checkResourceDeliveryConfig(ctx, addFail, addWarn); err != nil {
		fmt.Fprintf(os.Stderr, "[verify-production-db] ERROR: %v\n", err)
		return 1
	}

	hasFailure := false
	for _, f := range findings {
		prefix := "WARN"
		if f.level == levelFail {
			prefix = "ERROR"
			hasFailure = true
		}
		fmt.Fprintf(os.Stderr, "[verify-production-db] %s: %s\n", prefix, f.message)
	}
	if hasFailure {
		return 1
	}

	fmt.Println("[verify-production-db] passed")
	return 0
}

func checkDatabaseConnectivity(ctx context.Context) error {
	var one int
	return db.DB.WithContext(ctx).Raw("SELECT 1").Scan(&one).Error
}

func checkPaymentProviders(ctx context.Context, addFail, addWarn func(string)) error {
	providers, err := db.GetAllPaymentProviders(ctx)
	if err != nil {
		return err
	}
	var active []db.PaymentProviderRecord
	for _, p := range providers {
		if p.Status == models.PaymentProviderStatusActive {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		addWarn("No active payment providers are configured; recharge will be unavailable until one is enabled")
		return nil
	}

	hasGlobalWhitelist := strings.TrimSpace(os.Getenv("PAYMENT_CALLBACK_IP_WHITELIST")) != ""
	whitelistRequired := strings.TrimSpace(os.Getenv("PAYMENT_CALLBACK_IP_WHITELIST_REQUIRED")) != "false"
	if !hasGlobalWhitelist && whitelistRequired {
		var withoutDefaults []string
		for _, p := range active {
			if p.Type != models.PaymentProviderTypeHeleket {
				withoutDefaults = append(withoutDefaults, fmt.Sprintf("#%d (%s, %s)", p.ID, p.Name, p.Type))
			}
		}
		if len(withoutDefaults) > 0 {
			addWarn(fmt.Sprintf(
				"PAYMENT_CALLBACK_IP_WHITELIST is empty while active payment providers without built-in callback IP defaults exist: %s. Configure provider callback source IPs when available; signature, status, amount and idempotency checks still run before crediting balance.",
				strings.Join(withoutDefaults, ", "),
			))
		}
	} else if !hasGlobalWhitelist && !whitelistRequired {
		fmt.Println("[production-db-readiness] PAYMENT_CALLBACK_IP_WHITELIST is intentionally empty; active providers must still pass signature, status, amount and idempotency checks before crediting balance")
	}

	for _, p := range active {
		prefix := fmt.Sprintf("Payment provider #%d (%s, %s)", p.ID, p.Name, p.Type)
		switch p.Type {
		case models.PaymentProviderTypeYipay, models.PaymentProviderTypeHeleket, models.PaymentProviderTypeAntom:
		default:
			addFail(prefix + " uses unsupported provider type for recharge")
			continue
		}

		if p.Type == models.PaymentProviderTypeAntom {
			_, valid, errMsg := lib.BuildAntomConfig(p.Config)
			if !valid {
				if errMsg == "" {
					errMsg = "unknown error"
				}
				addFail(prefix + " configuration is invalid: " + errMsg)
			}
			continue
		}

		apiURL := configStringField(p.Config, "apiurl")
		if apiURL == "" {
			addFail(prefix + " is missing apiurl")
		} else if _, err := lib.AssertSafeHTTPURL(ctx, apiURL, prefix+" apiurl"); err != nil {
			addWarn(fmt.Sprintf("%s apiurl could not be verified as a safe outbound HTTP(S) URL: %v（不阻断发布，请核查该支付渠道配置）", prefix, err))
		}

		if p.Type == models.PaymentProviderTypeYipay {
			version := configStringField(p.Config, "version")
			if version == "" {
				version = "v2"
			}
			if configStringField(p.Config, "pid") == "" {
				addFail(prefix + " is missing pid")
			}
			if version == "v1" {
				if configStringField(p.Config, "key") == "" {
					addFail(prefix + " v1 is missing key")
				}
			} else {
				if configStringField(p.Config, "platform_public_key") == "" {
					addFail(prefix + " v2 is missing platform_public_key")
				}
				if configStringField(p.Config, "merchant_private_key") == "" {
					addFail(prefix + " v2 is missing merchant_private_key")
				}
			}
		}

		if p.Type == models.PaymentProviderTypeHeleket {
			if configStringField(p.Config, "api_key") == "" {
				addFail(prefix + " is missing api_key")
			}
			if configStringField(p.Config, "merchant_uuid") == "" {
				addFail(prefix + " is missing merchant_uuid")
			}
		}
	}
	return nil
}

func checkSMTPConfig(ctx context.Context, addFail, addWarn func(string)) error {
	enabledVal, err := db.GetSystemConfig(ctx, "smtp_enabled", false)
	if err != nil {
		return err
	}
	host, err := db.GetSystemConfig(ctx, "smtp_host", false)
	if err != nil {
		return err
	}
	username, err := db.GetSystemConfig(ctx, "smtp_username", false)
	if err != nil {
		return err
	}
	password, err := db.GetSystemConfig(ctx, "smtp_password", true)
	if err != nil {
		return err
	}
	fromEmail, err := db.GetSystemConfig(ctx, "smtp_from_email", false)
	if err != nil {
		return err
	}

	smtpEnabled := derefStr(enabledVal) == "true"
	hasPartial := !isBlankStr(host) || !isBlankStr(username) || !isBlankStr(password) || !isBlankStr(fromEmail)
	if !smtpEnabled && hasPartial {
		addWarn("SMTP fields are partially configured while smtp_enabled=false; email verification and notifications will not send")
		return nil
	}
	if !smtpEnabled {
		addWarn("SMTP is disabled; registration/password-reset/change-email verification emails will be unavailable if those flows require email")
		return nil
	}
	if isBlankStr(host) {
		addFail("smtp_enabled=true but smtp_host is empty")
	}
	if isBlankStr(username) {
		addFail("smtp_enabled=true but smtp_username is empty")
	}
	if isBlankStr(password) {
		addFail("smtp_enabled=true but smtp_password is empty")
	}
	if isBlankStr(fromEmail) {
		addFail("smtp_enabled=true but smtp_from_email is empty")
	}
	return nil
}

func checkLskyConfig(ctx context.Context, addFail, addWarn func(string)) error {
	baseURL, err := db.GetSystemConfig(ctx, "ticket_image_lsky_base_url", false)
	if err != nil {
		return err
	}
	token, err := db.GetSystemConfig(ctx, "ticket_image_lsky_token", true)
	if err != nil {
		return err
	}
	apiVersion, err := db.GetSystemConfig(ctx, "ticket_image_lsky_api_version", false)
	if err != nil {
		return err
	}

	if isBlankStr(baseURL) && isBlankStr(token) {
		addWarn("Lsky ticket image upload is not configured; ticket image uploads will fail until configured or avoided")
		return nil
	}
	if isBlankStr(baseURL) {
		addFail("Lsky token is configured but ticket_image_lsky_base_url is empty")
	} else {
		safeURL, safeErr := lib.AssertSafeHTTPURL(ctx, strings.TrimSpace(*baseURL), "Lsky base URL")
		if safeErr != nil {
			addFail("Lsky base URL is not safe: " + safeErr.Error())
		} else {
			validateHTTPSInProd(safeURL, "Lsky base URL", addFail)
		}
	}
	if isBlankStr(token) {
		addFail("ticket_image_lsky_base_url is configured but ticket_image_lsky_token is empty")
	}
	if ver := strings.TrimSpace(derefStr(apiVersion)); ver != "" && ver != "v1" && ver != "v2" {
		addFail("ticket_image_lsky_api_version must be v1 or v2")
	}
	return nil
}

func checkResourceDeliveryConfig(ctx context.Context, addFail, addWarn func(string)) error {
	type onlineHostRow struct {
		ID              int32  `gorm:"column:id"`
		Name            string `gorm:"column:name"`
		Architecture    string `gorm:"column:architecture"`
		InstanceType    string `gorm:"column:instance_type"`
		CPUAllowanceMax int32  `gorm:"column:cpu_allowance_max"`
		MemoryMax       int32  `gorm:"column:memory_max"`
		StorageSize     int32  `gorm:"column:storage_size"`
		EnableAPI       bool   `gorm:"column:enable_api"`
		IsInstalled     bool   `gorm:"column:is_installed"`
	}
	var onlineHosts []onlineHostRow
	if err := db.DB.WithContext(ctx).
		Table("hosts").
		Select("id, name, architecture, instance_type, cpu_allowance_max, memory_max, storage_size, enable_api, is_installed").
		Where("status = ?", string(models.HostStatusOnline)).
		Find(&onlineHosts).Error; err != nil {
		return err
	}

	type visibleImageRow struct {
		ID           int32  `gorm:"column:id"`
		Name         string `gorm:"column:name"`
		Architecture string `gorm:"column:architecture"`
		InstanceType string `gorm:"column:instance_type"`
	}
	var visibleImages []visibleImageRow
	if err := db.DB.WithContext(ctx).
		Table("system_images").
		Select("id, name, architecture, instance_type").
		Where("hidden = false").
		Find(&visibleImages).Error; err != nil {
		return err
	}

	type publicPackageRow struct {
		ID           int32  `gorm:"column:id"`
		Name         string `gorm:"column:name"`
		InstanceType string `gorm:"column:instance_type"`
	}
	var publicPackages []publicPackageRow
	if err := db.DB.WithContext(ctx).
		Table("packages").
		Select("id, name, instance_type").
		Where("active = true AND global_shared = true").
		Find(&publicPackages).Error; err != nil {
		return err
	}

	if len(onlineHosts) == 0 {
		addWarn("No online hosts are configured; instance creation cannot run until at least one Incus host is online")
	}
	allocatableCount := 0
	onlineHostIDs := make([]int32, 0, len(onlineHosts))
	for _, h := range onlineHosts {
		onlineHostIDs = append(onlineHostIDs, h.ID)
		if h.CPUAllowanceMax > 0 && h.MemoryMax > 0 {
			allocatableCount++
		}
	}
	if len(onlineHosts) > 0 && allocatableCount == 0 {
		addWarn("Online hosts exist but none have positive CPU and memory quotas configured")
	}

	agentByHostID := make(map[int32]models.HostAgent)
	allowedImagesByHostID := make(map[int32]map[int32]bool)
	if len(onlineHostIDs) > 0 {
		var agents []models.HostAgent
		if err := db.DB.WithContext(ctx).Where("host_id IN ?", onlineHostIDs).Find(&agents).Error; err != nil {
			return err
		}
		for _, a := range agents {
			agentByHostID[a.HostID] = a
		}
		var allowed []models.HostAllowedImage
		if err := db.DB.WithContext(ctx).Where("host_id IN ?", onlineHostIDs).Find(&allowed).Error; err != nil {
			return err
		}
		for _, item := range allowed {
			if allowedImagesByHostID[item.HostID] == nil {
				allowedImagesByHostID[item.HostID] = make(map[int32]bool)
			}
			allowedImagesByHostID[item.HostID][item.ImageID] = true
		}
	}

	const agentStaleAfter = 5 * time.Minute
	now := time.Now().UTC()
	for _, host := range onlineHosts {
		prefix := fmt.Sprintf("Online host #%d (%s)", host.ID, host.Name)
		if !host.IsInstalled {
			addWarn(prefix + " is online but isInstalled=false")
		}
		if !host.EnableAPI {
			addWarn(prefix + " is online but Incus API enableApi=false")
		}
		agent, ok := agentByHostID[host.ID]
		if !ok {
			addWarn(prefix + " has no Agent credentials; heartbeat and resource reports cannot run")
			continue
		}
		if !agent.Enabled {
			addWarn(prefix + " Agent is disabled")
		}
		if agent.Status != "online" {
			addWarn(fmt.Sprintf("%s Agent status is %s", prefix, agent.Status))
			continue
		}
		if agent.LastSeenAt == nil {
			addWarn(prefix + " Agent is online but lastSeenAt is empty")
			continue
		}
		if now.Sub(agent.LastSeenAt.UTC()) > agentStaleAfter {
			addWarn(prefix + " Agent heartbeat is older than 5 minutes")
		}
		if isBlankStr(agent.Version) {
			addWarn(prefix + " Agent is online but did not report a version")
		}
		if !jsonArrayNonEmpty(agent.Capabilities) {
			addWarn(prefix + " Agent is online but did not report capabilities")
		}
		reportObj := parseJSONObject(agent.LastReport)
		if len(reportObj) == 0 {
			addWarn(prefix + " Agent is online but lastReport is empty; resource and metric reports cannot be verified")
			continue
		}
		if !nonEmptyObjectField(reportObj["resources"]) {
			addWarn(prefix + " Agent lastReport.resources is empty; host resource reporting cannot be verified")
		}
		if !nonEmptyObjectField(reportObj["metrics"]) {
			addWarn(prefix + " Agent lastReport.metrics is empty; heartbeat interval and metric reporting cannot be verified")
		}
	}

	if len(onlineHostIDs) > 0 {
		type runningRow struct {
			ID      int32 `gorm:"column:id"`
			HostID  int32 `gorm:"column:host_id"`
			HasSnap bool  `gorm:"column:has_snap"`
		}
		var running []runningRow
		if err := db.DB.WithContext(ctx).Raw(`
			SELECT i.id, i.host_id, (s.id IS NOT NULL) AS has_snap
			FROM instances i
			LEFT JOIN traffic_snapshots s ON s.instance_id = i.id
			WHERE i.status = 'running' AND i.host_id IN ?
			ORDER BY i.id ASC
		`, onlineHostIDs).Scan(&running).Error; err != nil {
			return err
		}

		var missingSnapIDs []string
		var staleAgentIDs []string
		for _, r := range running {
			if !r.HasSnap && len(missingSnapIDs) < 10 {
				missingSnapIDs = append(missingSnapIDs, fmt.Sprintf("%d", r.ID))
			}
			a, ok := agentByHostID[r.HostID]
			if !ok || !a.Enabled || a.Status != "online" || a.LastSeenAt == nil || now.Sub(a.LastSeenAt.UTC()) > agentStaleAfter {
				if len(staleAgentIDs) < 10 {
					staleAgentIDs = append(staleAgentIDs, fmt.Sprintf("%d", r.ID))
				}
			}
		}
		if len(missingSnapIDs) > 0 {
			addWarn("Running instances on online hosts are missing traffic snapshots; first traffic collection/Agent report has not established a baseline (sample instance IDs: " + strings.Join(missingSnapIDs, ", ") + ")")
		}
		if len(staleAgentIDs) > 0 {
			addWarn("Running instances exist on hosts without a fresh online Agent heartbeat; Agent status/resource/traffic reports need live validation (sample instance IDs: " + strings.Join(staleAgentIDs, ", ") + ")")
		}
	}

	if len(visibleImages) == 0 {
		addWarn("No visible system images are configured; instance creation image selection will be empty")
	}

	hostHasCompatibleImage := func(host onlineHostRow) bool {
		allowed := allowedImagesByHostID[host.ID]
		for _, img := range visibleImages {
			if img.Architecture != host.Architecture {
				continue
			}
			if !isInstanceTypeCompatible(img.InstanceType, host.InstanceType) {
				continue
			}
			if len(allowed) == 0 || allowed[img.ID] {
				return true
			}
		}
		return false
	}

	if len(onlineHosts) > 0 && len(visibleImages) > 0 {
		anyCompatible := false
		for _, h := range onlineHosts {
			if hostHasCompatibleImage(h) {
				anyCompatible = true
				break
			}
		}
		if !anyCompatible {
			addWarn("Online hosts exist but none have a compatible visible system image")
		}
	}

	if len(publicPackages) == 0 {
		addWarn("No active global-shared packages are configured; ordinary users will not see public instance packages")
		return nil
	}

	for _, pkg := range publicPackages {
		prefix := fmt.Sprintf("Public package #%d (%s)", pkg.ID, pkg.Name)
		var hostIDs []int32
		if err := db.DB.WithContext(ctx).
			Table("package_hosts").
			Select("host_id").
			Where("package_id = ?", pkg.ID).
			Pluck("host_id", &hostIDs).Error; err != nil {
			return err
		}
		if len(hostIDs) == 0 {
			addFail(prefix + " is active but not bound to any host")
			continue
		}

		var pkgHosts []onlineHostRow
		if err := db.DB.WithContext(ctx).
			Table("hosts").
			Select("id, name, architecture, instance_type, cpu_allowance_max, memory_max, storage_size, enable_api, is_installed").
			Where("id IN ? AND status = ?", hostIDs, string(models.HostStatusOnline)).
			Find(&pkgHosts).Error; err != nil {
			return err
		}
		if len(pkgHosts) == 0 {
			addFail(prefix + " is active but none of its bound hosts are online")
			continue
		}

		hasImage := false
		for _, h := range pkgHosts {
			if isInstanceTypeCompatible(h.InstanceType, pkg.InstanceType) && hostHasCompatibleImage(h) {
				hasImage = true
				break
			}
		}
		if !hasImage {
			addFail(prefix + " has no compatible visible image on its online bound hosts")
		}

		type planRow struct {
			CPU       int32 `gorm:"column:cpu"`
			Memory    int32 `gorm:"column:memory"`
			Disk      int32 `gorm:"column:disk"`
			IsSoldOut bool  `gorm:"column:is_sold_out"`
		}
		var plans []planRow
		if err := db.DB.WithContext(ctx).
			Table("package_plans").
			Select("cpu, memory, disk, is_sold_out").
			Where("package_id = ? AND is_active = true", pkg.ID).
			Find(&plans).Error; err != nil {
			return err
		}
		var availablePlans []planRow
		for _, pl := range plans {
			if !pl.IsSoldOut {
				availablePlans = append(availablePlans, pl)
			}
		}
		if len(plans) > 0 && len(availablePlans) == 0 {
			addWarn(prefix + " has active plans but all are marked sold out")
			continue
		}

		minCPU, minMem, minDisk := int32(15), int32(128), int64(512)
		if len(availablePlans) > 0 {
			minCPU = availablePlans[0].CPU
			minMem = availablePlans[0].Memory
			minDisk = int64(availablePlans[0].Disk)
			for _, pl := range availablePlans[1:] {
				if pl.CPU < minCPU {
					minCPU = pl.CPU
				}
				if pl.Memory < minMem {
					minMem = pl.Memory
				}
				if int64(pl.Disk) < minDisk {
					minDisk = int64(pl.Disk)
				}
			}
		}

		hasCapacity := false
		for _, h := range pkgHosts {
			type usageRow struct {
				CPU    int64 `gorm:"column:cpu"`
				Memory int64 `gorm:"column:memory"`
			}
			var usage usageRow
			_ = db.DB.WithContext(ctx).Raw(`
				SELECT COALESCE(SUM(cpu), 0) AS cpu, COALESCE(SUM(memory), 0) AS memory
				FROM instances
				WHERE host_id = ? AND status <> 'deleted'
			`, h.ID).Scan(&usage).Error
			pkgID := pkg.ID
			capInfo, capErr := lib.GetStorageCapacity(ctx, h.ID, lib.StorageCapacityOptions{
				PackageID:  &pkgID,
				RequiredMB: &minDisk,
			})
			if int64(h.CPUAllowanceMax)-usage.CPU >= int64(minCPU) &&
				int64(h.MemoryMax)-usage.Memory >= int64(minMem) &&
				capErr == nil && capInfo.Reason == nil {
				hasCapacity = true
				break
			}
		}
		if !hasCapacity {
			addWarn(prefix + " is active but online bound hosts cannot satisfy its minimum CPU/memory/disk requirement")
		}
	}
	return nil
}

func isInstanceTypeCompatible(imageType, targetType string) bool {
	if imageType == "" {
		imageType = "both"
	}
	if targetType == "" {
		targetType = "container"
	}
	return targetType == "both" || imageType == "both" || imageType == targetType
}

func validateHTTPSInProd(parsed *url.URL, label string, addFail func(string)) {
	if lib.IsProductionEnv() && parsed.Scheme != "https" {
		addFail(label + " must use HTTPS in production")
	}
}

func configStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func isBlankStr(v *string) bool {
	return v == nil || strings.TrimSpace(*v) == ""
}

func jsonArrayNonEmpty(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var arr []any
	return json.Unmarshal(raw, &arr) == nil && len(arr) > 0
}

func parseJSONObject(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj
}

func nonEmptyObjectField(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) > 0
}
