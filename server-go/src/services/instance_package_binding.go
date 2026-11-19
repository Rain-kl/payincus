// Port of server/src/lib/instance-package-binding.ts.
//
// Import presets (which package/plan/user an async import should bind once the
// node finishes) live in system_configs under ImportPresetConfigKey, guarded by
// the 74208 transaction advisory lock — single-process semantics, but the lock
// still keeps two HTTP workers from inter losing updates.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ImportPresetConfigKey mirrors IMPORT_PRESET_CONFIG_KEY.
const ImportPresetConfigKey = "instance_import_package_presets"

// importPresetLockKey mirrors IMPORT_PRESET_LOCK_KEY (pg_advisory_xact_lock).
const importPresetLockKey = 74208

// ImportPresetTTL mirrors IMPORT_PRESET_TTL_MS (30 days).
const ImportPresetTTL = 30 * 24 * time.Hour

// importPresetMaxValidDays mirrors MAX_VALID_DAYS.
const importPresetMaxValidDays = 36500

// PackageBindingError mirrors PackageBindingError(message, statusCode, code).
type PackageBindingError struct {
	Message    string
	StatusCode int
	Code       string
}

func (e *PackageBindingError) Error() string { return e.Message }

func newPackageBindingError(message string, statusCode int, code string) *PackageBindingError {
	return &PackageBindingError{Message: message, StatusCode: statusCode, Code: code}
}

// PackageBindingInput mirrors PackageBindingInput. Pointer fields distinguish
// "absent" (nil) from "explicitly null"; the route layer translates.
type PackageBindingInput struct {
	PackageID *int32
	PlanID    *int32
	ValidDays *int32
	ExpiresAt *string
}

// ImportPackagePlanOption mirrors ImportPackagePlanOption.
type ImportPackagePlanOption struct {
	ID                int32   `json:"id"`
	Name              string  `json:"name"`
	Description       *string `json:"description"`
	BillingCycle      int32   `json:"billingCycle"`
	Price             float64 `json:"price"`
	CPU               int32   `json:"cpu"`
	Memory            int32   `json:"memory"`
	Disk              int32   `json:"disk"`
	TrafficLimit      *string `json:"trafficLimit"`
	TrafficLimitSpeed *string `json:"trafficLimitSpeed"`
	PortLimit         int32   `json:"portLimit"`
	SnapshotLimit     int32   `json:"snapshotLimit"`
	BackupLimit       int32   `json:"backupLimit"`
	SiteLimit         int32   `json:"siteLimit"`
	IsActive          bool    `json:"isActive"`
	SortOrder         int32   `json:"sortOrder"`
}

// ImportPackageOption mirrors ImportPackageOption.
type ImportPackageOption struct {
	ID                  int32                     `json:"id"`
	Name                string                    `json:"name"`
	Description         *string                   `json:"description"`
	InstanceType        string                    `json:"instanceType"`
	NetworkMode         string                    `json:"networkMode"`
	CPUMax              int32                     `json:"cpuMax"`
	MemoryMax           int32                     `json:"memoryMax"`
	DiskMax             int32                     `json:"diskMax"`
	MonthlyTrafficLimit *string                   `json:"monthlyTrafficLimit"`
	PortLimit           int32                     `json:"portLimit"`
	SnapshotLimit       int32                     `json:"snapshotLimit"`
	BackupLimit         int32                     `json:"backupLimit"`
	SiteLimit           int32                     `json:"siteLimit"`
	IsActive            bool                      `json:"isActive"`
	BoundToHost         bool                      `json:"boundToHost"`
	Plans               []ImportPackagePlanOption `json:"plans"`
}

// PendingImportPreset mirrors PendingImportPreset.
type PendingImportPreset struct {
	PackageID   *int32  `json:"packageId,omitempty"`
	PlanID      *int32  `json:"planId,omitempty"`
	PackageName *string `json:"packageName,omitempty"`
	PlanName    *string `json:"planName,omitempty"`
	ValidDays   *int32  `json:"validDays,omitempty"`
	ExpiresAt   *string `json:"expiresAt,omitempty"`
	UserID      *int32  `json:"userId,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
	CreatedAt   string  `json:"createdAt"`
}

type storedTaskPreset struct {
	HostID    int32               `json:"hostId"`
	IncusName *string             `json:"incusName,omitempty"`
	FilePath  *string             `json:"filePath,omitempty"`
	Preset    PendingImportPreset `json:"preset"`
}

// ImportPresetStore mirrors ImportPresetStore.
type ImportPresetStore struct {
	Version      int                            `json:"version"`
	ByTask       map[string]storedTaskPreset    `json:"byTask"`
	ByUnassigned map[string]PendingImportPreset `json:"byUnassigned"`
}

// ExtractedIncusMetrics mirrors ExtractedIncusMetrics.
type ExtractedIncusMetrics struct {
	CPU             int32
	Memory          int32
	Disk            int32
	StoragePoolName *string
	IPv4            *string
	IPv6            *string
	NetworkMode     models.NetworkMode
	Image           string
	Status          string // 'running' | 'stopped'
	InstanceType    string // 'container' | 'vm'
}

// ResolvedPackageBinding mirrors ResolvedPackageBinding.
type ResolvedPackageBinding struct {
	PackageID               *int32
	PackagePlanID           *int32
	PackageName             *string
	PlanName                *string
	BillingPrice            *models.Decimal
	BillingCycle            *int32
	ExpiresAt               *time.Time
	PortLimit               int32
	SnapshotLimit           int32
	BackupLimit             int32
	SiteLimit               int32
	MonthlyTrafficLimit     *int64
	LimitsRead              *string
	LimitsWrite             *string
	LimitsReadIops          *int32
	LimitsWriteIops         *int32
	LimitsIngress           *string
	LimitsEgress            *string
	LimitsProcesses         *int32
	LimitsCPUPriority       *int32
	BootAutostart           bool
	BootAutostartPriority   *int32
	BootAutostartDelay      *int32
	BootHostShutdownTimeout *int32
	SwapEnabled             bool
	SwapSize                *int32
	EgressMode              string
	EgressProfileID         *int32
	HasLimitsTrafficSpeed   bool
}

func emptyPresetStore() ImportPresetStore {
	return ImportPresetStore{Version: 1, ByTask: map[string]storedTaskPreset{}, ByUnassigned: map[string]PendingImportPreset{}}
}

func unassignedPresetKey(hostID int32, incusName string) string {
	return fmt.Sprintf("%d:%s", hostID, incusName)
}

func parseOptionalPositiveInt(raw any) *int32 {
	switch v := raw.(type) {
	case float64:
		if v > 0 && v == math.Trunc(v) && v <= math.MaxInt32 {
			c := int32(v)
			return &c
		}
	case int:
		if v > 0 {
			c := int32(v)
			return &c
		}
	case int32:
		if v > 0 {
			return &v
		}
	case int64:
		if v > 0 && v <= math.MaxInt32 {
			c := int32(v)
			return &c
		}
	}
	return nil
}

func normalizePendingPreset(raw any) *PendingImportPreset {
	obj, ok := raw.(map[string]any)
	if !ok || obj == nil {
		return nil
	}
	createdAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if rawCreated, ok := obj["createdAt"].(string); ok && rawCreated != "" {
		if _, err := time.Parse(time.RFC3339, rawCreated); err == nil {
			createdAt = rawCreated
		}
	}
	preset := &PendingImportPreset{
		PackageID: parseOptionalPositiveInt(obj["packageId"]),
		PlanID:    parseOptionalPositiveInt(obj["planId"]),
		ValidDays: parseOptionalPositiveInt(obj["validDays"]),
		UserID:    parseOptionalPositiveInt(obj["userId"]),
		CreatedAt: createdAt,
	}
	if name, ok := obj["packageName"].(string); ok {
		preset.PackageName = &name
	}
	if name, ok := obj["planName"].(string); ok {
		preset.PlanName = &name
	}
	if exp, ok := obj["expiresAt"].(string); ok && strings.TrimSpace(exp) != "" {
		e := strings.TrimSpace(exp)
		preset.ExpiresAt = &e
	}
	if name, ok := obj["displayName"].(string); ok && strings.TrimSpace(name) != "" {
		d := strings.TrimSpace(name)
		preset.DisplayName = &d
	}
	return preset
}

// PruneAndNormalizePresetStore mirrors pruneAndNormalizePresetStore.
func PruneAndNormalizePresetStore(raw any, now time.Time) ImportPresetStore {
	store := emptyPresetStore()
	if obj, ok := raw.(map[string]any); !ok || obj == nil {
		return store
	} else {
		cutoff := now.Add(-ImportPresetTTL)

		if byTask, ok := obj["byTask"].(map[string]any); ok && byTask != nil {
			for taskID, rawEntry := range byTask {
				if taskID == "" {
					continue
				}
				entry, ok := rawEntry.(map[string]any)
				if !ok || entry == nil {
					continue
				}
				hostID := parseOptionalPositiveInt(entry["hostId"])
				preset := normalizePendingPreset(entry["preset"])
				if hostID == nil || preset == nil {
					continue
				}
				created, err := time.Parse(time.RFC3339, preset.CreatedAt)
				if err == nil && created.Before(cutoff) {
					continue
				}
				stored := storedTaskPreset{HostID: *hostID, Preset: *preset}
				if name, ok := entry["incusName"].(string); ok && strings.TrimSpace(name) != "" {
					n := strings.TrimSpace(name)
					stored.IncusName = &n
				}
				if path, ok := entry["filePath"].(string); ok && strings.TrimSpace(path) != "" {
					p := strings.TrimSpace(path)
					stored.FilePath = &p
				}
				store.ByTask[taskID] = stored
			}
		}

		if byUnassigned, ok := obj["byUnassigned"].(map[string]any); ok && byUnassigned != nil {
			for key, rawEntry := range byUnassigned {
				if key == "" {
					continue
				}
				preset := normalizePendingPreset(rawEntry)
				if preset == nil {
					continue
				}
				created, err := time.Parse(time.RFC3339, preset.CreatedAt)
				if err == nil && created.Before(cutoff) {
					continue
				}
				store.ByUnassigned[key] = *preset
			}
		}
	}
	return store
}

// ResolveExplicitExpiresAt mirrors resolveExplicitExpiresAt.
func ResolveExplicitExpiresAt(validDays *int32, expiresAt *string, defaultExpiresAt *time.Time, now time.Time) (*time.Time, error) {
	if expiresAt != nil && strings.TrimSpace(*expiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*expiresAt))
		if err != nil {
			return nil, newPackageBindingError("到期时间格式无效", 400, "INVALID_EXPIRES_AT")
		}
		return &parsed, nil
	}
	if validDays != nil {
		if *validDays < 1 || *validDays > importPresetMaxValidDays {
			return nil, newPackageBindingError(fmt.Sprintf("有效天数必须为 1 到 %d 之间的整数", importPresetMaxValidDays), 400, "INVALID_VALID_DAYS")
		}
		result := now.Add(time.Duration(*validDays) * 24 * time.Hour)
		return &result, nil
	}
	return defaultExpiresAt, nil
}

var incusSizePattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*([kmgt]i?b?)?$`)

// normalizeIncusSizeToMB mirrors normalizeIncusSizeToMB.
func normalizeIncusSizeToMB(raw any, fallbackMB int32) int32 {
	text, ok := raw.(string)
	if !ok {
		return fallbackMB
	}
	match := incusSizePattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return fallbackMB
	}
	num, err := strconv.ParseFloat(match[1], 64)
	if err != nil || num <= 0 || math.IsNaN(num) || math.IsInf(num, 0) {
		return fallbackMB
	}
	unit := strings.ToLower(match[2])
	if unit == "" {
		unit = "b"
	}
	factor := 1.0
	switch unit {
	case "b":
		factor = 1.0 / (1024 * 1024)
	case "k", "kib":
		factor = 1.0 / 1024
	case "kb":
		factor = 1.0 / 1000
	case "m", "mb", "mib":
		factor = 1
	case "g", "gib":
		factor = 1024
	case "gb":
		factor = 1000
	case "t", "tib":
		factor = 1024 * 1024
	case "tb":
		factor = 1000 * 1000
	}
	result := int64(math.Round(num * factor))
	if result < 1 {
		return 1
	}
	return int32(result)
}

// normalizeIncusCpuAllowance mirrors normalizeIncusCpuAllowance.
func normalizeIncusCpuAllowance(config map[string]any) int32 {
	if raw, ok := config["limits.cpu.allowance"].(string); ok {
		trimmed := strings.TrimSpace(raw)
		if strings.HasSuffix(trimmed, "%") {
			if pct, err := strconv.ParseFloat(strings.TrimSuffix(trimmed, "%"), 64); err == nil && pct > 0 {
				v := int32(math.Round(pct))
				if v < 1 {
					return 1
				}
				return v
			}
		}
	}
	if raw, ok := config["limits.cpu"].(string); ok {
		if cores, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && cores > 0 {
			v := int32(math.Round(cores * 100))
			if v < 1 {
				return 1
			}
			return v
		}
	}
	return 100
}

// InspectIncusInstanceMetrics mirrors inspectIncusInstanceMetrics.
func InspectIncusInstanceMetrics(ctx context.Context, client *lib.IncusClient, incusName string) (ExtractedIncusMetrics, error) {
	rawInst, err := lib.IncusInstanceFetchRaw(ctx, client, incusName)
	if err != nil {
		return ExtractedIncusMetrics{}, newPackageBindingError(fmt.Sprintf("节点上不存在实例 %s", incusName), 404, "INCUS_INSTANCE_NOT_FOUND")
	}

	config := map[string]any{}
	if expanded, ok := rawInst["expanded_config"].(map[string]any); ok && expanded != nil {
		config = expanded
	} else if base, ok := rawInst["config"].(map[string]any); ok && base != nil {
		config = base
	}
	devices := map[string]any{}
	if expanded, ok := rawInst["expanded_devices"].(map[string]any); ok && expanded != nil {
		devices = expanded
	} else if base, ok := rawInst["devices"].(map[string]any); ok && base != nil {
		devices = base
	}

	var storagePoolName *string
	if root, ok := devices["root"].(map[string]any); ok && root != nil {
		if pool, ok := root["pool"].(string); ok && strings.TrimSpace(pool) != "" {
			p := strings.TrimSpace(pool)
			storagePoolName = &p
		}
	}

	var ipv4, ipv6 *string
	stateRaw, stateErr := lib.IncusInstanceGetState(ctx, client, incusName)
	if stateErr == nil && len(stateRaw) > 0 {
		var state struct {
			Network map[string]struct {
				Addresses []struct {
					Family  string `json:"family"`
					Scope   string `json:"scope"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"network"`
		}
		if json.Unmarshal(stateRaw, &state) == nil {
			for iface, info := range state.Network {
				if iface == "lo" {
					continue
				}
				for _, addr := range info.Addresses {
					if addr.Scope != "" && addr.Scope != "global" {
						continue
					}
					if addr.Family == "inet" && ipv4 == nil && !strings.HasPrefix(addr.Address, "169.254.") {
						v := addr.Address
						ipv4 = &v
					} else if addr.Family == "inet6" && ipv6 == nil && !strings.HasPrefix(addr.Address, "fe80:") {
						v := addr.Address
						ipv6 = &v
					}
				}
			}
		}
	}

	networkMode := models.NetworkModeNat
	if ipv4 != nil && ipv6 != nil {
		networkMode = models.NetworkModeNatIpv6
	} else if ipv6 != nil {
		networkMode = models.NetworkModeIpv6Only
	}
	status := "stopped"
	if s, _ := rawInst["status"].(string); strings.EqualFold(s, "running") {
		status = "running"
	}
	instanceType := "container"
	if t, _ := rawInst["type"].(string); t == "virtual-machine" {
		instanceType = "vm"
	}

	image := "imported"
	if desc, ok := config["image.description"].(string); ok && desc != "" {
		image = desc
	} else if os, ok := config["image.os"].(string); ok && os != "" {
		image = os
	}

	var diskFallback int32 = 5120
	var rootSize any
	if root, ok := devices["root"].(map[string]any); ok && root != nil {
		rootSize = root["size"]
	}

	return ExtractedIncusMetrics{
		CPU:             normalizeIncusCpuAllowance(config),
		Memory:          normalizeIncusSizeToMB(config["limits.memory"], 512),
		Disk:            normalizeIncusSizeToMB(rootSize, diskFallback),
		StoragePoolName: storagePoolName,
		IPv4:            ipv4,
		IPv6:            ipv6,
		NetworkMode:     networkMode,
		Image:           image,
		Status:          status,
		InstanceType:    instanceType,
	}, nil
}

// ListImportablePackagesForHost mirrors listImportablePackagesForHost.
func ListImportablePackagesForHost(ctx context.Context, hostID int32) ([]ImportPackageOption, error) {
	if db.DB == nil {
		return nil, errors.New("database not initialized")
	}
	var host struct {
		ID           int32  `gorm:"column:id"`
		InstanceType string `gorm:"column:instance_type"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id, instance_type FROM hosts WHERE id = ? LIMIT 1`, hostID).Scan(&host).Error; err != nil {
		return nil, err
	}
	if host.ID == 0 {
		return nil, newPackageBindingError("宿主机不存在", 404, "HOST_NOT_FOUND")
	}

	query := `SELECT id, name, description, instance_type, network_mode, cpu_max, memory_max, disk_max,
		monthly_traffic_limit, port_limit, snapshot_limit, backup_limit, site_limit, active
		FROM packages`
	args := []any{}
	if host.InstanceType != "" && host.InstanceType != "both" {
		query += ` WHERE instance_type = ?`
		args = append(args, host.InstanceType)
	}
	query += ` ORDER BY active DESC, id DESC`

	var pkgRows []struct {
		ID                  int32   `gorm:"column:id"`
		Name                string  `gorm:"column:name"`
		Description         *string `gorm:"column:description"`
		InstanceType        string  `gorm:"column:instance_type"`
		NetworkMode         string  `gorm:"column:network_mode"`
		CPUMax              int32   `gorm:"column:cpu_max"`
		MemoryMax           int32   `gorm:"column:memory_max"`
		DiskMax             int32   `gorm:"column:disk_max"`
		MonthlyTrafficLimit *int64  `gorm:"column:monthly_traffic_limit"`
		PortLimit           int32   `gorm:"column:port_limit"`
		SnapshotLimit       int32   `gorm:"column:snapshot_limit"`
		BackupLimit         int32   `gorm:"column:backup_limit"`
		SiteLimit           int32   `gorm:"column:site_limit"`
		Active              bool    `gorm:"column:active"`
	}
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&pkgRows).Error; err != nil {
		return nil, err
	}

	out := make([]ImportPackageOption, 0, len(pkgRows))
	for _, row := range pkgRows {
		option := ImportPackageOption{
			ID:            row.ID,
			Name:          row.Name,
			Description:   row.Description,
			InstanceType:  row.InstanceType,
			NetworkMode:   row.NetworkMode,
			CPUMax:        row.CPUMax,
			MemoryMax:     row.MemoryMax,
			DiskMax:       row.DiskMax,
			PortLimit:     row.PortLimit,
			SnapshotLimit: row.SnapshotLimit,
			BackupLimit:   row.BackupLimit,
			SiteLimit:     row.SiteLimit,
			IsActive:      row.Active,
		}
		if row.MonthlyTrafficLimit != nil {
			limit := strconv.FormatInt(*row.MonthlyTrafficLimit, 10)
			option.MonthlyTrafficLimit = &limit
		}

		var boundCount int64
		_ = db.DB.WithContext(ctx).Raw(
			`SELECT COUNT(*) FROM package_hosts WHERE host_id = ? AND package_id = ?`, hostID, row.ID,
		).Scan(&boundCount).Error
		option.BoundToHost = boundCount > 0

		plans, err := db.GetPackagePlansByPackageId(ctx, row.ID, db.PackagePlansOptions{})
		if err != nil {
			return nil, err
		}
		sort.SliceStable(plans, func(i, j int) bool {
			if plans[i].SortOrder != plans[j].SortOrder {
				return plans[i].SortOrder < plans[j].SortOrder
			}
			return plans[i].ID < plans[j].ID
		})
		// prisma's include always yields an array (empty when the package has no
		// plans); a nil Go slice would serialize as JSON null.
		option.Plans = make([]ImportPackagePlanOption, 0, len(plans))
		for _, plan := range plans {
			traffic := strconv.FormatInt(plan.TrafficLimit, 10)
			speed := plan.TrafficLimitSpeed
			option.Plans = append(option.Plans, ImportPackagePlanOption{
				ID:                plan.ID,
				Name:              plan.Name,
				Description:       plan.Description,
				BillingCycle:      plan.BillingCycle,
				Price:             math.Round(plan.Price.InexactFloat64()/100*100) / 100,
				CPU:               plan.CPU,
				Memory:            plan.Memory,
				Disk:              plan.Disk,
				TrafficLimit:      &traffic,
				TrafficLimitSpeed: &speed,
				PortLimit:         plan.PortLimit,
				SnapshotLimit:     plan.SnapshotLimit,
				BackupLimit:       plan.BackupLimit,
				SiteLimit:         plan.SiteLimit,
				IsActive:          plan.IsActive,
				SortOrder:         plan.SortOrder,
			})
		}
		out = append(out, option)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BoundToHost != out[j].BoundToHost {
			return out[i].BoundToHost
		}
		if out[i].IsActive != out[j].IsActive {
			return out[i].IsActive
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ValidatedPackageBinding is the result of ValidatePackageBindingSelection.
type ValidatedPackageBinding struct {
	PackageName    *string
	PlanName       *string
	ResolvedPlanID *int32
}

// ValidatePackageBindingSelection mirrors validatePackageBindingSelection.
func ValidatePackageBindingSelection(
	ctx context.Context,
	hostID int32,
	binding PackageBindingInput,
	instanceType string,
	userID *int32,
) (ValidatedPackageBinding, error) {
	if db.DB == nil {
		return ValidatedPackageBinding{}, errors.New("database not initialized")
	}

	if userID != nil {
		var exists int64
		_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM users WHERE id = ?`, *userID).Scan(&exists).Error
		if exists == 0 {
			return ValidatedPackageBinding{}, newPackageBindingError("目标用户不存在", 404, "USER_NOT_FOUND")
		}
	}

	if binding.PackageID == nil || *binding.PackageID == 0 {
		if binding.PlanID != nil && *binding.PlanID != 0 {
			return ValidatedPackageBinding{}, newPackageBindingError("请先选择套餐再选择计费方案", 400, "PACKAGE_REQUIRED_FOR_PLAN")
		}
		if _, err := ResolveExplicitExpiresAt(binding.ValidDays, binding.ExpiresAt, nil, time.Now()); err != nil {
			return ValidatedPackageBinding{}, err
		}
		return ValidatedPackageBinding{}, nil
	}

	var pkg struct {
		ID           int32  `gorm:"column:id"`
		Name         string `gorm:"column:name"`
		InstanceType string `gorm:"column:instance_type"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, name, instance_type FROM packages WHERE id = ? LIMIT 1`, *binding.PackageID,
	).Scan(&pkg).Error; err != nil {
		return ValidatedPackageBinding{}, err
	}
	if pkg.ID == 0 {
		return ValidatedPackageBinding{}, newPackageBindingError("所选套餐不存在", 404, "PACKAGE_NOT_FOUND")
	}
	if instanceType != "" && pkg.InstanceType != instanceType {
		return ValidatedPackageBinding{}, newPackageBindingError(
			fmt.Sprintf("套餐类型为 %s，与实例类型 %s 不匹配", pkg.InstanceType, instanceType), 400, "PACKAGE_INSTANCE_TYPE_MISMATCH")
	}

	plans, err := db.GetPackagePlansByPackageId(ctx, pkg.ID, db.PackagePlansOptions{})
	if err != nil {
		return ValidatedPackageBinding{}, err
	}
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].SortOrder != plans[j].SortOrder {
			return plans[i].SortOrder < plans[j].SortOrder
		}
		return plans[i].ID < plans[j].ID
	})

	var selectedPlan *models.PackagePlan
	if binding.PlanID != nil {
		for i := range plans {
			if plans[i].ID == *binding.PlanID {
				selectedPlan = &plans[i]
				break
			}
		}
		if selectedPlan == nil {
			return ValidatedPackageBinding{}, newPackageBindingError("所选方案不属于当前套餐", 400, "PLAN_NOT_IN_PACKAGE")
		}
	} else if len(plans) > 0 {
		// planId absent: default to the first active plan so a paid package never
		// silently becomes a free instance.
		selectedPlan = &plans[0]
		for i := range plans {
			if plans[i].IsActive {
				selectedPlan = &plans[i]
				break
			}
		}
	}

	var defaultExpiresAt *time.Time
	if selectedPlan != nil {
		speed := selectedPlan.TrafficLimitSpeed
		if TrafficBandwidthNormalizePlanTrafficLimitSpeed(&speed).Invalid {
			return ValidatedPackageBinding{}, newPackageBindingError("所选方案的限速配置无效", 400, "INVALID_PLAN_SPEED")
		}
		billing := db.CalculateCreateBilling(*selectedPlan, time.Now())
		defaultExpiresAt = &billing.ExpiresAt
	}

	if _, err := ResolveExplicitExpiresAt(binding.ValidDays, binding.ExpiresAt, defaultExpiresAt, time.Now()); err != nil {
		return ValidatedPackageBinding{}, err
	}

	result := ValidatedPackageBinding{PackageName: &pkg.Name}
	if selectedPlan != nil {
		name := selectedPlan.Name
		id := selectedPlan.ID
		result.PlanName = &name
		result.ResolvedPlanID = &id
	}
	return result, nil
}

// loadPackageForBinding fetches package + plans inside the caller's transaction.
func loadPackageForBinding(ctx context.Context, tx *gorm.DB, packageID int32) (*models.Package, []models.PackagePlan, error) {
	var pkg models.Package
	if err := tx.WithContext(ctx).Where("id = ?", packageID).Limit(1).Find(&pkg).Error; err != nil {
		return nil, nil, err
	}
	if pkg.ID == 0 {
		return nil, nil, newPackageBindingError("所选套餐不存在", 404, "PACKAGE_NOT_FOUND")
	}
	var plans []models.PackagePlan
	if err := tx.WithContext(ctx).Where("package_id = ?", packageID).Order("sort_order ASC, id ASC").Find(&plans).Error; err != nil {
		return nil, nil, err
	}
	return &pkg, plans, nil
}

// ResolvePackageBindingForHost mirrors resolvePackageBindingForHost.
func ResolvePackageBindingForHost(
	ctx context.Context,
	tx *gorm.DB,
	hostID int32,
	binding PackageBindingInput,
	instanceType string,
	now time.Time,
) (ResolvedPackageBinding, error) {
	if binding.PackageID == nil || *binding.PackageID == 0 {
		if binding.PlanID != nil && *binding.PlanID != 0 {
			return ResolvedPackageBinding{}, newPackageBindingError("请先选择套餐再选择计费方案", 400, "PACKAGE_REQUIRED_FOR_PLAN")
		}
		return ResolvedPackageBinding{
			PortLimit:     10,
			SnapshotLimit: 3,
			BackupLimit:   2,
			SiteLimit:     5,
			EgressMode:    "direct",
		}, nil
	}

	pkg, plans, err := loadPackageForBinding(ctx, tx, *binding.PackageID)
	if err != nil {
		return ResolvedPackageBinding{}, err
	}
	if instanceType != "" && string(pkg.InstanceType) != instanceType {
		return ResolvedPackageBinding{}, newPackageBindingError(
			fmt.Sprintf("套餐类型为 %s，与实例类型 %s 不匹配", pkg.InstanceType, instanceType), 400, "PACKAGE_INSTANCE_TYPE_MISMATCH")
	}

	var selectedPlan *models.PackagePlan
	if binding.PlanID != nil {
		for i := range plans {
			if plans[i].ID == *binding.PlanID {
				selectedPlan = &plans[i]
				break
			}
		}
		if selectedPlan == nil {
			return ResolvedPackageBinding{}, newPackageBindingError("所选方案不属于当前套餐", 400, "PLAN_NOT_IN_PACKAGE")
		}
	} else if len(plans) > 0 {
		selectedPlan = &plans[0]
		for i := range plans {
			if plans[i].IsActive {
				selectedPlan = &plans[i]
				break
			}
		}
	}

	var normalizedSpeed *string
	if selectedPlan != nil {
		speed := selectedPlan.TrafficLimitSpeed
		parsed := TrafficBandwidthNormalizePlanTrafficLimitSpeed(&speed)
		if parsed.Invalid {
			return ResolvedPackageBinding{}, newPackageBindingError("所选方案的限速配置无效", 400, "INVALID_PLAN_SPEED")
		}
		if parsed.Value != nil {
			normalizedSpeed = parsed.Value
		}
	}

	var billingPrice *models.Decimal
	var billingCycle *int32
	var defaultExpiresAt *time.Time
	if selectedPlan != nil {
		billing := db.CalculateCreateBilling(*selectedPlan, now)
		price, priceErr := models.NewDecimalFromString(fmt.Sprintf("%.2f", billing.Price))
		if priceErr != nil {
			return ResolvedPackageBinding{}, priceErr
		}
		billingPrice = &price
		cycle := billing.BillingCycle
		billingCycle = &cycle
		defaultExpiresAt = &billing.ExpiresAt
	}
	resolvedExpiresAt, err := ResolveExplicitExpiresAt(binding.ValidDays, binding.ExpiresAt, defaultExpiresAt, now)
	if err != nil {
		return ResolvedPackageBinding{}, err
	}
	if selectedPlan == nil {
		resolvedExpiresAt = nil
	}

	var baseTrafficLimit *int64
	if selectedPlan != nil {
		limit := selectedPlan.TrafficLimit
		baseTrafficLimit = &limit
	} else if pkg.MonthlyTrafficLimit != nil {
		baseTrafficLimit = pkg.MonthlyTrafficLimit
	}
	effectiveTraffic, err := lib.ResolveInstanceTrafficLimitForHost(ctx, tx, lib.TrafficMultiplierResolveParams{
		PackageID:        &pkg.ID,
		HostID:           hostID,
		BaseTrafficLimit: baseTrafficLimit,
	})
	if err != nil {
		return ResolvedPackageBinding{}, err
	}

	egressBinding, err := lib.BindPackageEgress(ctx, tx, &pkg.ID, hostID, nil)
	if err != nil {
		return ResolvedPackageBinding{}, err
	}

	limitsIngress := normalizedSpeed
	if limitsIngress == nil {
		limitsIngress = strPtrNonEmpty(pkg.LimitsIngress)
	}
	limitsEgress := normalizedSpeed
	if limitsEgress == nil {
		limitsEgress = strPtrNonEmpty(pkg.LimitsEgress)
	}

	result := ResolvedPackageBinding{
		PackageID:               &pkg.ID,
		PackageName:             &pkg.Name,
		BillingPrice:            billingPrice,
		BillingCycle:            billingCycle,
		ExpiresAt:               resolvedExpiresAt,
		PortLimit:               pkg.PortLimit,
		SnapshotLimit:           pkg.SnapshotLimit,
		BackupLimit:             pkg.BackupLimit,
		SiteLimit:               pkg.SiteLimit,
		MonthlyTrafficLimit:     effectiveTraffic,
		LimitsRead:              strPtrNonEmpty(pkg.LimitsRead),
		LimitsWrite:             strPtrNonEmpty(pkg.LimitsWrite),
		LimitsReadIops:          int32PtrNonZero(pkg.LimitsReadIops),
		LimitsWriteIops:         int32PtrNonZero(pkg.LimitsWriteIops),
		LimitsIngress:           limitsIngress,
		LimitsEgress:            limitsEgress,
		LimitsProcesses:         int32PtrNonZero(pkg.LimitsProcesses),
		LimitsCPUPriority:       int32PtrNonZero(pkg.LimitsCPUPriority),
		BootAutostart:           pkg.BootAutostart,
		BootAutostartPriority:   int32PtrNonZero(pkg.BootAutostartPriority),
		BootAutostartDelay:      int32PtrNonZero(pkg.BootAutostartDelay),
		BootHostShutdownTimeout: int32PtrNonZero(pkg.BootHostShutdownTimeout),
		EgressMode:              egressBinding.EgressMode,
		EgressProfileID:         egressBinding.EgressProfileID,
		HasLimitsTrafficSpeed:   normalizedSpeed != nil,
	}
	if selectedPlan != nil {
		planID := selectedPlan.ID
		result.PackagePlanID = &planID
		result.PlanName = &selectedPlan.Name
		result.PortLimit = selectedPlan.PortLimit
		result.SnapshotLimit = selectedPlan.SnapshotLimit
		result.BackupLimit = selectedPlan.BackupLimit
		result.SiteLimit = selectedPlan.SiteLimit
		if selectedPlan.SwapSize > 0 {
			result.SwapEnabled = true
			swap := selectedPlan.SwapSize
			result.SwapSize = &swap
		}
	}
	return result, nil
}

// ==================== import preset store (system_configs) ====================

func mutateImportPresetStore(ctx context.Context, mutator func(*ImportPresetStore) error) error {
	if db.DB == nil {
		return errors.New("database not initialized")
	}
	return db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, importPresetLockKey).Error; err != nil {
			return err
		}
		var value *string
		if err := tx.WithContext(ctx).Raw(
			`SELECT value FROM system_configs WHERE key = ? LIMIT 1`, ImportPresetConfigKey,
		).Scan(&value).Error; err != nil {
			return err
		}

		var parsedRaw any
		if value != nil && *value != "" {
			var decoded any
			if json.Unmarshal([]byte(*value), &decoded) == nil {
				parsedRaw = decoded
			}
		}
		store := PruneAndNormalizePresetStore(parsedRaw, time.Now())
		if err := mutator(&store); err != nil {
			return err
		}
		serialized, err := json.Marshal(store)
		if err != nil {
			return err
		}
		if err := tx.Exec(`
			INSERT INTO system_configs (key, value, type, description, created_at, updated_at)
			VALUES (?, ?, 'json', '实例异步导入待分配套餐/用户预设（内部状态）', NOW(), NOW())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, type = 'json', updated_at = NOW()`,
			ImportPresetConfigKey, string(serialized),
		).Error; err != nil {
			return err
		}
		return nil
	})
}

func readImportPresetStore(ctx context.Context) (ImportPresetStore, error) {
	if db.DB == nil {
		return emptyPresetStore(), errors.New("database not initialized")
	}
	var value *string
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT value FROM system_configs WHERE key = ? LIMIT 1`, ImportPresetConfigKey,
	).Scan(&value).Error; err != nil {
		return ImportPresetStore{}, err
	}
	if value == nil || *value == "" {
		return emptyPresetStore(), nil
	}
	var decoded any
	if json.Unmarshal([]byte(*value), &decoded) != nil {
		return emptyPresetStore(), nil
	}
	return PruneAndNormalizePresetStore(decoded, time.Now()), nil
}

// SavePendingImportTaskPreset mirrors savePendingImportTaskPreset.
func SavePendingImportTaskPreset(ctx context.Context, taskID string, hostID int32, incusName, filePath *string, preset PendingImportPreset) error {
	if taskID == "" {
		return nil
	}
	return mutateImportPresetStore(ctx, func(store *ImportPresetStore) error {
		entry := storedTaskPreset{HostID: hostID, Preset: preset}
		if incusName != nil && strings.TrimSpace(*incusName) != "" {
			n := strings.TrimSpace(*incusName)
			entry.IncusName = &n
		}
		if filePath != nil && strings.TrimSpace(*filePath) != "" {
			p := strings.TrimSpace(*filePath)
			entry.FilePath = &p
		}
		if entry.Preset.CreatedAt == "" {
			entry.Preset.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		}
		store.ByTask[taskID] = entry
		return nil
	})
}

// ConsumePendingImportTaskPreset mirrors consumePendingImportTaskPreset.
func ConsumePendingImportTaskPreset(ctx context.Context, taskID string) (*PendingImportPreset, error) {
	if taskID == "" {
		return nil, nil
	}
	var found *PendingImportPreset
	err := mutateImportPresetStore(ctx, func(store *ImportPresetStore) error {
		entry, ok := store.ByTask[taskID]
		if !ok {
			return nil
		}
		preset := entry.Preset
		found = &preset
		delete(store.ByTask, taskID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// SaveUnassignedInstancePreset mirrors saveUnassignedInstancePreset.
func SaveUnassignedInstancePreset(ctx context.Context, hostID int32, incusName string, preset PendingImportPreset) error {
	if strings.TrimSpace(incusName) == "" {
		return nil
	}
	return mutateImportPresetStore(ctx, func(store *ImportPresetStore) error {
		store.ByUnassigned[unassignedPresetKey(hostID, strings.TrimSpace(incusName))] = preset
		return nil
	})
}

// GetUnassignedInstancePreset mirrors getUnassignedInstancePreset.
func GetUnassignedInstancePreset(ctx context.Context, hostID int32, incusName string) (*PendingImportPreset, error) {
	store, err := readImportPresetStore(ctx)
	if err != nil {
		return nil, err
	}
	if preset, ok := store.ByUnassigned[unassignedPresetKey(hostID, incusName)]; ok {
		return &preset, nil
	}
	return nil, nil
}

// ConsumeUnassignedInstancePreset mirrors consumeUnassignedInstancePreset.
func ConsumeUnassignedInstancePreset(ctx context.Context, hostID int32, incusName string) (*PendingImportPreset, error) {
	var found *PendingImportPreset
	err := mutateImportPresetStore(ctx, func(store *ImportPresetStore) error {
		key := unassignedPresetKey(hostID, incusName)
		if preset, ok := store.ByUnassigned[key]; ok {
			p := preset
			found = &p
			delete(store.ByUnassigned, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// ListUnassignedInstancePresetsForHost mirrors listUnassignedInstancePresetsForHost.
func ListUnassignedInstancePresetsForHost(ctx context.Context, hostID int32, incusNames []string) (map[string]PendingImportPreset, error) {
	nameSet := map[string]bool{}
	for _, name := range incusNames {
		nameSet[name] = true
	}
	prefix := fmt.Sprintf("%d:", hostID)

	result := map[string]PendingImportPreset{}
	err := mutateImportPresetStore(ctx, func(store *ImportPresetStore) error {
		for taskID, entry := range store.ByTask {
			if entry.HostID == hostID && entry.IncusName != nil && nameSet[*entry.IncusName] {
				key := unassignedPresetKey(hostID, *entry.IncusName)
				if _, exists := store.ByUnassigned[key]; !exists {
					store.ByUnassigned[key] = entry.Preset
				}
				delete(store.ByTask, taskID)
			}
		}
		for key, preset := range store.ByUnassigned {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			name := strings.TrimPrefix(key, prefix)
			if nameSet[name] {
				result[name] = preset
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ==================== small local helpers ====================

func strPtrNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func int32PtrNonZero(v int32) *int32 {
	if v == 0 {
		return nil
	}
	c := v
	return &c
}

// RegisteredImportedInstance is the route-facing shape of registerImportedHostInstance.
type RegisteredImportedInstance struct {
	AlreadyAssigned bool     `json:"-"`
	ID              int32    `json:"id"`
	IncusID         string   `json:"incusId"`
	Name            string   `json:"name"`
	UserID          int32    `json:"userId"`
	HostID          int32    `json:"hostId"`
	PackageID       *int32   `json:"packageId"`
	PackagePlanID   *int32   `json:"packagePlanId"`
	Status          string   `json:"status"`
	ExpiresAt       *string  `json:"expiresAt"`
	BillingPrice    *float64 `json:"billingPrice"`
	BillingCycle    *int32   `json:"billingCycle"`
	PackageName     *string  `json:"packageName"`
	PlanName        *string  `json:"planName"`
}

// RegisterImportedHostInstance mirrors registerImportedHostInstance.
func RegisterImportedHostInstance(
	ctx context.Context,
	hostID int32,
	client *lib.IncusClient,
	incusName string,
	userID int32,
	displayName *string,
	binding PackageBindingInput,
	metrics *ExtractedIncusMetrics,
	auditUserID *int32,
	auditSource string, // 'import_auto' | 'manual_assign'
	auditNote string,
	throwIfAlreadyAssigned bool,
) (RegisteredImportedInstance, error) {
	if db.DB == nil {
		return RegisteredImportedInstance{}, errors.New("database not initialized")
	}
	var targetUser struct {
		ID       int32  `gorm:"column:id"`
		Username string `gorm:"column:username"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT id, username FROM users WHERE id = ? LIMIT 1`, userID).Scan(&targetUser).Error; err != nil {
		return RegisteredImportedInstance{}, err
	}
	if targetUser.ID == 0 {
		return RegisteredImportedInstance{}, newPackageBindingError("目标用户不存在", 404, "USER_NOT_FOUND")
	}

	if metrics == nil {
		m, err := InspectIncusInstanceMetrics(ctx, client, incusName)
		if err != nil {
			return RegisteredImportedInstance{}, err
		}
		metrics = &m
	}

	name := incusName
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		name = strings.TrimSpace(*displayName)
	}

	var createdID int32
	var resolved ResolvedPackageBinding
	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var dupID int32
		if err := tx.WithContext(ctx).Raw(
			`SELECT id FROM instances WHERE host_id = ? AND incus_id = ? AND status <> 'deleted' LIMIT 1`,
			hostID, incusName,
		).Scan(&dupID).Error; err != nil {
			return err
		}
		if dupID != 0 {
			if throwIfAlreadyAssigned {
				return newPackageBindingError("该实例已在数据库中纳管", 409, "ALREADY_ASSIGNED")
			}
			return nil
		}

		r, err := ResolvePackageBindingForHost(ctx, tx, hostID, binding, metrics.InstanceType, time.Now())
		if err != nil {
			return err
		}
		resolved = r

		now := time.Now().UTC()
		var expiresAt any
		if r.ExpiresAt != nil {
			expiresAt = r.ExpiresAt.UTC()
		}
		if err := tx.WithContext(ctx).Raw(`
			INSERT INTO instances (
				incus_id, name, user_id, host_id, package_id, package_plan_id, storage_pool_name,
				image, status, cpu, memory, disk, ipv4, ipv6, network_mode,
				port_limit, snapshot_limit, backup_limit, site_limit, monthly_traffic_limit,
				limits_read, limits_write, limits_read_iops, limits_write_iops, limits_ingress, limits_egress,
				limits_processes, limits_cpu_priority, boot_autostart, boot_autostart_priority,
				boot_autostart_delay, boot_host_shutdown_timeout, swap_enabled, swap_size,
				egress_mode, egress_profile_id, billing_price, billing_cycle, expires_at,
				auto_renew, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::network_mode, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, false, ?, ?)
			RETURNING id`,
			incusName, name, targetUser.ID, hostID, r.PackageID, r.PackagePlanID, metrics.StoragePoolName,
			metrics.Image, metrics.Status, metrics.CPU, metrics.Memory, metrics.Disk, metrics.IPv4, metrics.IPv6, string(metrics.NetworkMode),
			r.PortLimit, r.SnapshotLimit, r.BackupLimit, r.SiteLimit, r.MonthlyTrafficLimit,
			r.LimitsRead, r.LimitsWrite, r.LimitsReadIops, r.LimitsWriteIops, r.LimitsIngress, r.LimitsEgress,
			r.LimitsProcesses, r.LimitsCPUPriority, r.BootAutostart, r.BootAutostartPriority,
			r.BootAutostartDelay, r.BootHostShutdownTimeout, r.SwapEnabled, r.SwapSize,
			r.EgressMode, r.EgressProfileID, r.BillingPrice, r.BillingCycle, expiresAt,
			now, now,
		).Scan(&createdID).Error; err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return RegisteredImportedInstance{}, txErr
	}
	if createdID == 0 {
		return RegisteredImportedInstance{AlreadyAssigned: true}, nil
	}

	used, err := db.HostsCalculateResourcesFromInstances(ctx, hostID)
	if err == nil {
		_ = db.HostsUpdateResources(ctx, hostID, db.HostsResourceUpdate{
			CPUUsed:    &used.CPUUsed,
			MemoryUsed: &used.MemoryUsed,
			DiskUsed:   &used.DiskUsed,
		}, nil)
	}

	pkgDesc := "（未挂载套餐，按免费实例处理）"
	if resolved.PackageName != nil {
		pkgDesc = fmt.Sprintf("，挂载套餐 %s", *resolved.PackageName)
		if resolved.PlanName != nil {
			pkgDesc += " / " + *resolved.PlanName
		}
	}
	sourceLabel := "分配未纳管实例"
	action := "host.instance.assign"
	if auditSource == "import_auto" {
		sourceLabel = "导入后自动分配实例"
		action = "host.instance.import_assign"
	}
	noteSuffix := ""
	if strings.TrimSpace(auditNote) != "" {
		noteSuffix = " " + strings.TrimSpace(auditNote)
	}

	logUserID := int64(targetUser.ID)
	if auditUserID != nil {
		logUserID = int64(*auditUserID)
	}
	db.CreateLog(ctx, &logUserID, db.LogModuleInstance, action,
		fmt.Sprintf("节点 #%d %s %s (#%d) 给用户 %s (#%d)%s%s",
			hostID, sourceLabel, incusName, createdID, targetUser.Username, targetUser.ID, pkgDesc, noteSuffix),
		db.LogResultSuccess, db.LogOptionsWithInstance(&createdID))

	instance := RegisteredImportedInstance{
		ID:            createdID,
		IncusID:       incusName,
		Name:          name,
		UserID:        targetUser.ID,
		HostID:        hostID,
		PackageID:     resolved.PackageID,
		PackagePlanID: resolved.PackagePlanID,
		Status:        metrics.Status,
		PackageName:   resolved.PackageName,
		PlanName:      resolved.PlanName,
		BillingCycle:  resolved.BillingCycle,
	}
	if resolved.ExpiresAt != nil {
		iso := resolved.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		instance.ExpiresAt = &iso
	}
	if resolved.BillingPrice != nil {
		price := resolved.BillingPrice.InexactFloat64()
		instance.BillingPrice = &price
	}
	return instance, nil
}

// RebindInstancePackageResult is the route-facing shape of rebindInstancePackage.
type RebindInstancePackageResult struct {
	ID            int32    `json:"id"`
	PackageID     *int32   `json:"packageId"`
	PackagePlanID *int32   `json:"packagePlanId"`
	PackageName   *string  `json:"packageName"`
	PlanName      *string  `json:"planName"`
	BillingPrice  *float64 `json:"billingPrice"`
	BillingCycle  *int32   `json:"billingCycle"`
	ExpiresAt     *string  `json:"expiresAt"`
}

// RebindInstancePackage mirrors rebindInstancePackage.
func RebindInstancePackage(
	ctx context.Context,
	hostID int32,
	instanceID int32,
	binding PackageBindingInput,
	auditUserID *int32,
	incusInstanceType string,
) (RebindInstancePackageResult, error) {
	if db.DB == nil {
		return RebindInstancePackageResult{}, errors.New("database not initialized")
	}
	var existing struct {
		ID      int32  `gorm:"column:id"`
		HostID  int32  `gorm:"column:host_id"`
		Name    string `gorm:"column:name"`
		IncusID string `gorm:"column:incus_id"`
		Status  string `gorm:"column:status"`
		UserID  int32  `gorm:"column:user_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, host_id, name, incus_id, status, user_id FROM instances WHERE id = ? LIMIT 1`, instanceID,
	).Scan(&existing).Error; err != nil {
		return RebindInstancePackageResult{}, err
	}
	if existing.ID == 0 || existing.Status == "deleted" || existing.HostID != hostID {
		return RebindInstancePackageResult{}, newPackageBindingError("实例不存在或不属于当前宿主机", 404, "INSTANCE_NOT_FOUND")
	}

	inferredType := incusInstanceType
	if inferredType == "" {
		var hostType string
		_ = db.DB.WithContext(ctx).Raw(`SELECT instance_type FROM hosts WHERE id = ? LIMIT 1`, hostID).Scan(&hostType).Error
		if hostType == "container" || hostType == "vm" {
			inferredType = hostType
		}
	}

	var resolved ResolvedPackageBinding
	var updated struct {
		ID            int32           `gorm:"column:id"`
		PackageID     *int32          `gorm:"column:package_id"`
		PackagePlanID *int32          `gorm:"column:package_plan_id"`
		BillingPrice  *models.Decimal `gorm:"column:billing_price"`
		BillingCycle  *int32          `gorm:"column:billing_cycle"`
		ExpiresAt     *time.Time      `gorm:"column:expires_at"`
	}
	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, err := ResolvePackageBindingForHost(ctx, tx, hostID, binding, inferredType, time.Now())
		if err != nil {
			return err
		}
		resolved = r

		var expiresAt any
		if r.ExpiresAt != nil {
			expiresAt = r.ExpiresAt.UTC()
		}
		if err := tx.WithContext(ctx).Raw(`
			UPDATE instances SET
				package_id = ?, package_plan_id = ?, port_limit = ?, snapshot_limit = ?, backup_limit = ?, site_limit = ?,
				monthly_traffic_limit = ?, billing_price = ?, billing_cycle = ?, expires_at = ?, auto_renew = false,
				egress_mode = ?, egress_profile_id = ?, updated_at = ?
			WHERE id = ?
			RETURNING id, package_id, package_plan_id, billing_price, billing_cycle, expires_at`,
			r.PackageID, r.PackagePlanID, r.PortLimit, r.SnapshotLimit, r.BackupLimit, r.SiteLimit,
			r.MonthlyTrafficLimit, r.BillingPrice, r.BillingCycle, expiresAt,
			r.EgressMode, r.EgressProfileID, time.Now().UTC(), instanceID,
		).Scan(&updated).Error; err != nil {
			return err
		}

		if r.HasLimitsTrafficSpeed || r.PackageID == nil {
			if err := tx.WithContext(ctx).Exec(
				`UPDATE instances SET limits_ingress = ?, limits_egress = ?, updated_at = ? WHERE id = ?`,
				r.LimitsIngress, r.LimitsEgress, time.Now().UTC(), instanceID,
			).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		return RebindInstancePackageResult{}, txErr
	}

	pkgDesc := "无套餐（免费实例）"
	if resolved.PackageName != nil {
		pkgDesc = *resolved.PackageName
		if resolved.PlanName != nil {
			pkgDesc += " / " + *resolved.PlanName
		}
	}

	logUserID := int64(existing.UserID)
	if auditUserID != nil {
		logUserID = int64(*auditUserID)
	}
	db.CreateLog(ctx, &logUserID, db.LogModuleInstance, "instance.bind_package",
		fmt.Sprintf("为实例 %s (#%d) 调整挂载套餐：%s", existing.Name, existing.ID, pkgDesc),
		db.LogResultSuccess, db.LogOptionsWithInstance(&existing.ID))

	result := RebindInstancePackageResult{
		ID:            updated.ID,
		PackageID:     updated.PackageID,
		PackagePlanID: updated.PackagePlanID,
		PackageName:   resolved.PackageName,
		PlanName:      resolved.PlanName,
		BillingCycle:  updated.BillingCycle,
	}
	if updated.BillingPrice != nil {
		price := updated.BillingPrice.InexactFloat64()
		result.BillingPrice = &price
	}
	if updated.ExpiresAt != nil {
		iso := updated.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		result.ExpiresAt = &iso
	}
	return result, nil
}
