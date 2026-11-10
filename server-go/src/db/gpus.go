// Port of server/src/db/gpus.ts.
//
// Shared-GPU admission and reservation bookkeeping. GPUs are shared per physical
// device, so every check here exists to stop a caller from claiming more of a card
// than the host actually sold:
//
//   - a reservation counts an instance while gpu_released_at IS NULL, whether the
//     card is the instance's primary gpu_device_id or one of its
//     instance_gpu_allocations rows;
//   - seats are only counted after the host_gpus rows are locked FOR UPDATE, so two
//     concurrent reservations cannot both read the same free-slot count;
//   - a card is only available when the node's report is fresh (see GpuReportMaxAge).
//
// Naming: this package is shared by parallel module ports, so every package-level
// identifier carries the `gpus`/`Gpus` prefix.
//
// The db package must not import lib (lib imports db: see lib/token_invalidation.go),
// so the two trivial lib/gpu.ts helpers this module needs are reproduced here:
// `gpusIsFresh`, `gpusResolvePolicy` and `GpusSharingNotice`. db/gpus_test.go is an
// external test package (db_test) and asserts the notice stays byte-identical to
// lib.GpuSharingNotice, the same pattern used for the OAuth scope catalog.
//
// Deliberately not ported: compensateGpuPreparationFailure. It dynamically imports
// quota-operations, public-ipv4, instance-provisioning-compensation and
// lib/network-modes, none of which exist in the Go port yet; the instance
// provisioner that calls it is a later wave. Wiring it to stubs would fake success.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// GpusSharingNotice mirrors GPU_SHARING_NOTICE.
const GpusSharingNotice = "共享物理 GPU；显存和算力由同池实例共同使用，不提供独立显存或算力保证。"

// gpusReportMaxAge mirrors GPU_REPORT_MAX_AGE_MS.
const gpusReportMaxAge = 180 * time.Second

var (
	// gpusUUIDPattern mirrors /^GPU-[a-fA-F0-9-]{16,64}$/.
	gpusUUIDPattern = regexp.MustCompile(`^GPU-[a-fA-F0-9-]{16,64}$`)
	// gpusPCIAddressPattern mirrors /^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$/.
	gpusPCIAddressPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
)

// GpusError mirrors the TypeScript GpuError (message only; the route layer reports
// the same literals as plain JSON bodies).
type GpusError struct {
	Message string
}

// Error implements error.
func (e *GpusError) Error() string { return e.Message }

// GpusPolicy mirrors lib.GpuPolicy without importing lib.
type GpusPolicy struct {
	GpuShared    bool
	GpuDeviceIDs []int32
}

// GpusSelectionInput mirrors the object passed to validateGpuSelection.
type GpusSelectionInput struct {
	GpuShared    bool
	GpuDeviceIDs []int32
	InstanceType string
	Privileged   bool
	HostIDs      []int32
}

// GpusReserveInput mirrors the object passed to reserveInstanceGpu.
type GpusReserveInput struct {
	HostID    int32
	PackageID *int32
	PlanID    *int32
}

// GpusReservation mirrors reserveInstanceGpu's return value. Allocations is nil
// when the policy is not shared (the TS returns `{}`).
type GpusReservation struct {
	GpuDeviceID  *int32
	GpuDeviceIDs []int32
}

// GpusHostGpu mirrors listHostGpus' enriched device row.
type GpusHostGpu struct {
	ID             int32           `json:"id"`
	HostID         int32           `json:"hostId"`
	UUID           string          `json:"uuid"`
	PciAddress     string          `json:"pciAddress"`
	Name           string          `json:"name"`
	MemoryTotalMiB int32           `json:"memoryTotalMiB"`
	Metrics        json.RawMessage `json:"metrics"`
	Available      bool            `json:"available"`
	RuntimeReady   bool            `json:"runtimeReady"`
	SharingEnabled bool            `json:"sharingEnabled"`
	MaxInstances   int32           `json:"maxInstances"`
	LastSeenAt     string          `json:"lastSeenAt"`

	Ready          bool   `json:"ready"`
	UsedInstances  int64  `json:"usedInstances"`
	AvailableSlots int64  `json:"availableSlots"`
	SharingNotice  string `json:"sharingNotice"`
}

// GpusReportCard mirrors one validated card from a host GPU report.
type GpusReportCard struct {
	UUID           string
	PciAddress     string
	Name           string
	MemoryTotalMiB int64
	Metrics        map[string]any
}

func gpusClient(client *gorm.DB) *gorm.DB {
	if client == nil {
		return DB
	}
	return client
}

// gpusIsFresh mirrors isGpuFresh. The upper bound guards against a clock skew
// where the node reports a timestamp in the future.
func gpusIsFresh(available, runtimeReady bool, lastSeenAt, now time.Time) bool {
	return available && runtimeReady &&
		now.Sub(lastSeenAt) <= gpusReportMaxAge &&
		lastSeenAt.Before(now.Add(30*time.Second))
}

// gpusPolicyPackageRow is the package projection getGpuPolicy reads. The array
// column is fetched as JSON: models.Package carries GPUDeviceIds []int32, and GORM's
// schema parser rejects slice fields ("unsupported data type: &[]"), so a model query
// on that struct fails before SQL is even built. Raw + Scan on a row struct avoids
// the model parser entirely (same workaround as db/aff.go's package-plan reads).
type gpusPolicyPackageRow struct {
	ID               int32   `gorm:"column:id"`
	GPUShared        bool    `gorm:"column:gpu_shared"`
	GPUDeviceIDsJSON *string `gorm:"column:gpu_device_ids_json"`
	InstanceType     string  `gorm:"column:instance_type"`
	Privileged       bool    `gorm:"column:privileged"`
}

// gpusPolicyPlanRow is the plan projection resolveGpuPolicy reads.
type gpusPolicyPlanRow struct {
	ID               int32   `gorm:"column:id"`
	PackageID        int32   `gorm:"column:package_id"`
	GPUShared        *bool   `gorm:"column:gpu_shared"`
	GPUDeviceIDsJSON *string `gorm:"column:gpu_device_ids_json"`
	IsActive         bool    `gorm:"column:is_active"`
	IsSoldOut        bool    `gorm:"column:is_sold_out"`
}

// gpusJSONInt32Array parses `array_to_json(integer[])::text`.
func gpusJSONInt32Array(raw *string) []int32 {
	if raw == nil || *raw == "" || *raw == "null" {
		return []int32{}
	}
	var values []int32
	if err := json.Unmarshal([]byte(*raw), &values); err != nil {
		return []int32{}
	}
	if values == nil {
		return []int32{}
	}
	return values
}

// gpusResolvePolicy mirrors resolveGpuPolicy: a plan override wins only when it
// explicitly sets gpuShared (not null); otherwise the package policy applies.
func gpusResolvePolicy(pkg GpusPolicy, plan *gpusPolicyPlanRow) GpusPolicy {
	if plan != nil && plan.GPUShared != nil {
		return GpusPolicy{GpuShared: *plan.GPUShared, GpuDeviceIDs: gpusJSONInt32Array(plan.GPUDeviceIDsJSON)}
	}
	return pkg
}

// gpusPackagePolicy projects the package columns resolveGpuPolicy reads.
func gpusPackagePolicy(pkg gpusPolicyPackageRow) GpusPolicy {
	return GpusPolicy{GpuShared: pkg.GPUShared, GpuDeviceIDs: gpusJSONInt32Array(pkg.GPUDeviceIDsJSON)}
}

// GpusGetPolicy mirrors getGpuPolicy.
func GpusGetPolicy(ctx context.Context, client *gorm.DB, packageID, planID *int32) (GpusPolicy, error) {
	if packageID == nil || *packageID == 0 {
		return GpusPolicy{GpuShared: false, GpuDeviceIDs: []int32{}}, nil
	}

	var pkg gpusPolicyPackageRow
	err := gpusClient(client).WithContext(ctx).Raw(
		`SELECT id, gpu_shared, array_to_json(gpu_device_ids)::text AS gpu_device_ids_json,
		        instance_type, privileged
		 FROM packages WHERE id = ? LIMIT 1`, *packageID,
	).Scan(&pkg).Error
	if err != nil {
		return GpusPolicy{}, err
	}
	if pkg.ID == 0 {
		return GpusPolicy{}, &GpusError{Message: "套餐不存在"}
	}

	var plan *gpusPolicyPlanRow
	if planID != nil {
		var row gpusPolicyPlanRow
		err := gpusClient(client).WithContext(ctx).Raw(
			`SELECT id, package_id, gpu_shared, array_to_json(gpu_device_ids)::text AS gpu_device_ids_json,
			        is_active, is_sold_out
			 FROM package_plans WHERE id = ? LIMIT 1`, *planID,
		).Scan(&row).Error
		if err != nil {
			return GpusPolicy{}, err
		}
		if row.ID == 0 || row.PackageID != *packageID {
			return GpusPolicy{}, &GpusError{Message: "方案不属于该套餐"}
		}
		plan = &row
	}

	policy := gpusResolvePolicy(gpusPackagePolicy(pkg), plan)
	if policy.GpuShared && (pkg.InstanceType != "container" || pkg.Privileged || len(policy.GpuDeviceIDs) == 0) {
		return GpusPolicy{}, &GpusError{Message: "共享 GPU 必须使用非特权 LXC 套餐并选择显卡"}
	}
	return policy, nil
}

// GpusValidateSelection mirrors validateGpuSelection.
func GpusValidateSelection(ctx context.Context, client *gorm.DB, input GpusSelectionInput) error {
	if !input.GpuShared {
		return nil
	}
	if input.InstanceType != "container" || input.Privileged {
		return &GpusError{Message: "共享 GPU 仅支持非特权 LXC 容器"}
	}
	if len(input.GpuDeviceIDs) == 0 {
		return &GpusError{Message: "请选择至少一张共享 GPU"}
	}

	var count int64
	err := gpusClient(client).WithContext(ctx).Model(&models.HostGpu{}).
		Where("id IN ? AND host_id IN ? AND sharing_enabled = ?", input.GpuDeviceIDs, input.HostIDs, true).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count != int64(len(input.GpuDeviceIDs)) {
		return &GpusError{Message: "GPU 不属于套餐绑定的宿主机，或尚未开放共享"}
	}
	return nil
}

// GpusCountReservations mirrors countGpuReservations: an instance keeps its seat
// while gpu_released_at IS NULL, either as its primary device or as an allocation.
func GpusCountReservations(ctx context.Context, client *gorm.DB, gpuDeviceID int32, excludeInstanceID *int32) (int64, error) {
	query := gpusClient(client).WithContext(ctx).Model(&models.Instance{}).
		Where("gpu_released_at IS NULL").
		Where("gpu_device_id = ? OR EXISTS (SELECT 1 FROM instance_gpu_allocations a WHERE a.instance_id = instances.id AND a.gpu_device_id = ?)",
			gpuDeviceID, gpuDeviceID)
	if excludeInstanceID != nil {
		query = query.Where("id <> ?", *excludeInstanceID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// GpusFindAvailable mirrors findAvailableGpus: every card in the bundle must be
// shared-open, fresh and have a free seat, otherwise the bundle is unavailable.
func GpusFindAvailable(ctx context.Context, client *gorm.DB, hostID int32, policy GpusPolicy, excludeInstanceID *int32) ([]models.HostGpu, error) {
	if !policy.GpuShared {
		return nil, nil
	}
	if len(policy.GpuDeviceIDs) == 0 {
		return nil, nil
	}

	var devices []models.HostGpu
	if err := gpusClient(client).WithContext(ctx).
		Where("host_id = ? AND id IN ?", hostID, policy.GpuDeviceIDs).
		Order("id ASC").Find(&devices).Error; err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, nil
	}

	now := time.Now().UTC()
	for _, device := range devices {
		if !device.SharingEnabled || !gpusIsFresh(device.Available, device.RuntimeReady, device.LastSeenAt, now) {
			return nil, nil
		}
		used, err := GpusCountReservations(ctx, client, device.ID, excludeInstanceID)
		if err != nil {
			return nil, err
		}
		if used >= int64(device.MaxInstances) {
			return nil, nil
		}
	}
	return devices, nil
}

// GpusReserveInstanceGpu mirrors reserveInstanceGpu. The host's whole GPU set is
// locked FOR UPDATE before the free-seat count so concurrent reservations serialize.
func GpusReserveInstanceGpu(ctx context.Context, client *gorm.DB, input GpusReserveInput) (GpusReservation, error) {
	policy, err := GpusGetPolicy(ctx, client, input.PackageID, input.PlanID)
	if err != nil {
		return GpusReservation{}, err
	}
	if !policy.GpuShared {
		return GpusReservation{}, nil
	}

	if err := gpusClient(client).WithContext(ctx).
		Exec("SELECT id FROM host_gpus WHERE host_id = ? ORDER BY id FOR UPDATE", input.HostID).Error; err != nil {
		return GpusReservation{}, err
	}

	devices, err := GpusFindAvailable(ctx, client, input.HostID, policy, nil)
	if err != nil {
		return GpusReservation{}, err
	}
	if len(devices) == 0 {
		return GpusReservation{}, &GpusError{Message: "GPU_RESOURCES_INSUFFICIENT: 所选 GPU 组不可用、监控已过期或其中一张卡共享名额已满"}
	}

	ids := make([]int32, 0, len(devices))
	for _, device := range devices {
		ids = append(ids, device.ID)
	}
	primary := devices[0].ID
	return GpusReservation{GpuDeviceID: &primary, GpuDeviceIDs: ids}, nil
}

// GpusAllocatedIDs mirrors allocatedGpuIds: the primary device plus every
// allocation, de-duplicated and sorted ascending.
func GpusAllocatedIDs(gpuDeviceID *int32, allocationIDs []int32) []int32 {
	seen := map[int32]bool{}
	ids := make([]int32, 0, len(allocationIDs)+1)
	if gpuDeviceID != nil {
		seen[*gpuDeviceID] = true
		ids = append(ids, *gpuDeviceID)
	}
	for _, id := range allocationIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	// JS sort is numeric here; both paths above append in row/array order.
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

// GpusGetInstancePCIs mirrors getInstanceGpuPcis. Stopped/suspended instances keep
// all seats: a released bundle is re-validated and re-claimed in the same tx.
func GpusGetInstancePCIs(ctx context.Context, instanceID, hostID int32) ([]string, error) {
	var result []string
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var instance models.Instance
		err := tx.Select("id", "gpu_device_id", "gpu_released_at").
			Where("id = ?", instanceID).First(&instance).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}

		var allocations []models.InstanceGpuAllocation
		if err := tx.Where("instance_id = ?", instanceID).Find(&allocations).Error; err != nil {
			return err
		}
		allocationIDs := make([]int32, 0, len(allocations))
		for _, allocation := range allocations {
			allocationIDs = append(allocationIDs, allocation.GPUDeviceID)
		}
		ids := GpusAllocatedIDs(instance.GPUDeviceID, allocationIDs)
		if len(ids) == 0 {
			return nil
		}

		if err := tx.Exec("SELECT id FROM host_gpus WHERE id IN ? ORDER BY id FOR UPDATE", ids).Error; err != nil {
			return err
		}

		var devices []models.HostGpu
		if err := tx.Where("id IN ?", ids).Order("id ASC").Find(&devices).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if len(devices) != len(ids) {
			return &GpusError{Message: "绑定的 GPU 组已离线或不在目标宿主机上，请检查宿主 GPU 状态"}
		}
		for _, device := range devices {
			if device.HostID != hostID || !gpusIsFresh(device.Available, device.RuntimeReady, device.LastSeenAt, now) {
				return &GpusError{Message: "绑定的 GPU 组已离线或不在目标宿主机上，请检查宿主 GPU 状态"}
			}
		}

		if instance.GPUReleasedAt != nil {
			for _, device := range devices {
				if !device.SharingEnabled {
					return &GpusError{Message: "GPU 共享名额不足，无法重新创建实例"}
				}
				used, err := GpusCountReservations(ctx, tx, device.ID, &instanceID)
				if err != nil {
					return err
				}
				if used >= int64(device.MaxInstances) {
					return &GpusError{Message: "GPU 共享名额不足，无法重新创建实例"}
				}
			}
			if err := tx.Model(&models.Instance{}).Where("id = ?", instanceID).
				Update("gpu_released_at", nil).Error; err != nil {
				return err
			}
		}

		result = make([]string, 0, len(devices))
		for _, device := range devices {
			result = append(result, device.PciAddress)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GpusReleaseFailed mirrors releaseFailedGpu: only error/deleted instances that
// still hold their seats are released.
func GpusReleaseFailed(ctx context.Context, instanceID int32) error {
	return DB.WithContext(ctx).Model(&models.Instance{}).
		Where("id = ? AND status IN ? AND gpu_released_at IS NULL", instanceID, []string{"error", "deleted"}).
		Update("gpu_released_at", time.Now().UTC()).Error
}

// GpusAssertPlanChange mirrors assertGpuPlanChange.
func GpusAssertPlanChange(ctx context.Context, client *gorm.DB, instanceID, planID int32) error {
	var instance models.Instance
	err := gpusClient(client).WithContext(ctx).
		Select("id", "host_id", "package_id", "gpu_device_id").
		Where("id = ?", instanceID).First(&instance).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	policy, err := GpusGetPolicy(ctx, client, instance.PackageID, &planID)
	if err != nil {
		return err
	}
	var allocations []models.InstanceGpuAllocation
	if err := gpusClient(client).WithContext(ctx).Where("instance_id = ?", instanceID).Find(&allocations).Error; err != nil {
		return err
	}
	allocationIDs := make([]int32, 0, len(allocations))
	for _, allocation := range allocations {
		allocationIDs = append(allocationIDs, allocation.GPUDeviceID)
	}
	currentIDs := GpusAllocatedIDs(instance.GPUDeviceID, allocationIDs)

	targetIDs := []int32{}
	if policy.GpuShared && len(policy.GpuDeviceIDs) > 0 {
		var rows []models.HostGpu
		if err := gpusClient(client).WithContext(ctx).
			Select("id").
			Where("host_id = ? AND id IN ?", instance.HostID, policy.GpuDeviceIDs).
			Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			targetIDs = append(targetIDs, row.ID)
		}
	}

	mismatch := policy.GpuShared != (len(currentIDs) > 0) ||
		(policy.GpuShared && len(targetIDs) == 0) ||
		!gpusSameIDs(currentIDs, targetIDs)
	if mismatch {
		return &GpusError{Message: "该方案会改变 GPU 绑定，请新购所需 GPU 方案；已有实例仅支持保留原显卡的方案变更"}
	}
	return nil
}

func gpusSameIDs(left, right []int32) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// GpusAssertMigration mirrors assertGpuMigration.
func GpusAssertMigration(ctx context.Context, instanceID int32) error {
	var instance models.Instance
	err := DB.WithContext(ctx).Select("id", "gpu_device_id").
		Where("id = ?", instanceID).First(&instance).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if instance.GPUDeviceID != nil {
		return &GpusError{Message: "共享 GPU 实例不支持跨宿主迁移或克隆，请在目标宿主新购 GPU 实例"}
	}
	return nil
}

// gpusMeasurement mirrors measurement(value, max): a finite number in [0, max],
// otherwise null.
func gpusMeasurement(value any, max float64) *float64 {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > max {
		return nil
	}
	return &number
}

// gpusMetricValue renders a measurement for JSON: an untyped nil (not a typed nil
// pointer, which would still be non-nil as an `any`) when the report omitted it.
func gpusMetricValue(value any, max float64) any {
	if measured := gpusMeasurement(value, max); measured != nil {
		return *measured
	}
	return nil
}

// gpusSafeInteger mirrors Number.isSafeInteger.
func gpusSafeInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) && math.Abs(value) <= 9007199254740991
}

// gpusStringJS mirrors String(value) for the subset of JSON values a host report
// can carry.
func gpusStringJS(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return formatJSNumber(typed)
	default:
		return ""
	}
}

// gpusSliceJS mirrors String.prototype.slice(0, max) at UTF-16 code-unit
// boundaries (the unit JavaScript uses for `.length`).
func gpusSliceJS(value string, max int) string {
	if max <= 0 {
		return ""
	}
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > max {
			return value[:index]
		}
		units += width
	}
	return value
}

// gpusNormalizeReportCards mirrors the validation loop of syncHostGpuReport: at
// most 64 valid, de-duplicated cards.
func gpusNormalizeReportCards(report map[string]any) []GpusReportCard {
	status := ""
	if raw, ok := report["status"].(string); ok {
		status = raw
	}
	cards, _ := report["cards"].([]any)
	if status != "ready" || cards == nil {
		return nil
	}
	if len(cards) > 64 {
		cards = cards[:64]
	}

	seen := map[string]bool{}
	pciSeen := map[string]bool{}
	valid := make([]GpusReportCard, 0, len(cards))
	for _, value := range cards {
		card, ok := value.(map[string]any)
		if !ok {
			continue
		}
		uuid, uuidOK := card["uuid"].(string)
		pciAddress, pciOK := card["pciAddress"].(string)
		if !uuidOK || !gpusUUIDPattern.MatchString(uuid) || !pciOK || !gpusPCIAddressPattern.MatchString(pciAddress) {
			continue
		}
		if seen[uuid] || pciSeen[pciAddress] {
			continue
		}
		total := gpusMeasurement(card["memoryTotalMiB"], 10_000_000)
		if total == nil || *total == 0 || !gpusSafeInteger(*total) {
			continue
		}
		seen[uuid] = true
		pciSeen[pciAddress] = true

		name := "NVIDIA GPU"
		if candidate, present := card["name"]; present && gpusJSTruthy(candidate) {
			name = gpusStringJS(candidate)
		}
		valid = append(valid, GpusReportCard{
			UUID:           uuid,
			PciAddress:     pciAddress,
			Name:           gpusSliceJS(name, 128),
			MemoryTotalMiB: int64(*total),
			Metrics: map[string]any{
				"memoryUsedMiB":     gpusMetricValue(card["memoryUsedMiB"], *total),
				"gpuUtilization":    gpusMetricValue(card["gpuUtilization"], 100),
				"memoryUtilization": gpusMetricValue(card["memoryUtilization"], 100),
				"temperature":       gpusMetricValue(card["temperature"], 150),
				"powerDrawWatts":    gpusMetricValue(card["powerDrawWatts"], 1e9),
				"powerLimitWatts":   gpusMetricValue(card["powerLimitWatts"], 1e9),
				"driverVersion":     gpusSliceJS(gpusStringJS(card["driverVersion"]), 80),
				"computeMode":       gpusSliceJS(gpusStringJS(card["computeMode"]), 80),
			},
		})
	}
	return valid
}

// GpusSyncHostReport mirrors syncHostGpuReport. Unknown/missing input is ignored so
// old agents remain compatible.
func GpusSyncHostReport(ctx context.Context, hostID int32, input any) error {
	report, ok := input.(map[string]any)
	if !ok {
		return nil
	}
	status, _ := report["status"].(string)
	if status != "ready" && status != "error" && status != "unavailable" {
		return nil
	}
	cards := gpusNormalizeReportCards(report)

	now := time.Now().UTC()
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.HostGpu{}).Where("host_id = ?", hostID).
			Updates(map[string]any{"available": false, "runtime_ready": false}).Error; err != nil {
			return err
		}
		reportRuntimeReady, _ := report["runtimeReady"].(bool)
		for _, card := range cards {
			metrics, err := json.Marshal(card.Metrics)
			if err != nil {
				return err
			}
			runtimeReady := false
			if mode, _ := card.Metrics["computeMode"].(string); mode == "Default" {
				runtimeReady = reportRuntimeReady
			}
			row := models.HostGpu{
				HostID:         hostID,
				UUID:           card.UUID,
				PciAddress:     card.PciAddress,
				Name:           card.Name,
				MemoryTotalMiB: int32(card.MemoryTotalMiB),
				Metrics:        datatypes.JSON(metrics),
				Available:      true,
				RuntimeReady:   runtimeReady,
				LastSeenAt:     now,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "host_id"}, {Name: "uuid"}},
				DoUpdates: clause.Assignments(map[string]any{
					"pci_address":      row.PciAddress,
					"name":             row.Name,
					"memory_total_mib": row.MemoryTotalMiB,
					"metrics":          row.Metrics,
					"available":        true,
					"runtime_ready":    runtimeReady,
					"last_seen_at":     now,
				}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// GpusListHostGpus mirrors listHostGpus: every device plus its live seat count.
func GpusListHostGpus(ctx context.Context, hostID int32) ([]GpusHostGpu, error) {
	var devices []models.HostGpu
	if err := DB.WithContext(ctx).Where("host_id = ?", hostID).Order("id ASC").Find(&devices).Error; err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := make([]GpusHostGpu, 0, len(devices))
	for _, device := range devices {
		used, err := GpusCountReservations(ctx, nil, device.ID, nil)
		if err != nil {
			return nil, err
		}
		availableSlots := int64(device.MaxInstances) - used
		if availableSlots < 0 {
			availableSlots = 0
		}
		result = append(result, GpusHostGpu{
			ID:             device.ID,
			HostID:         device.HostID,
			UUID:           device.UUID,
			PciAddress:     device.PciAddress,
			Name:           device.Name,
			MemoryTotalMiB: device.MemoryTotalMiB,
			Metrics:        json.RawMessage(device.Metrics),
			Available:      device.Available,
			RuntimeReady:   device.RuntimeReady,
			SharingEnabled: device.SharingEnabled,
			MaxInstances:   device.MaxInstances,
			LastSeenAt:     isoTimestamp(device.LastSeenAt),
			Ready:          gpusIsFresh(device.Available, device.RuntimeReady, device.LastSeenAt, now),
			UsedInstances:  used,
			AvailableSlots: availableSlots,
			SharingNotice:  GpusSharingNotice,
		})
	}
	return result, nil
}

// GpusOffersForPackages mirrors getGpuOffersForPackages.
func GpusOffersForPackages(ctx context.Context, packageIDs []int32) (map[int32][]GpusPolicy, error) {
	offers := map[int32][]GpusPolicy{}
	if len(packageIDs) == 0 {
		return offers, nil
	}

	var packages []gpusPolicyPackageRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, gpu_shared, array_to_json(gpu_device_ids)::text AS gpu_device_ids_json
		 FROM packages WHERE id IN ?`, packageIDs,
	).Scan(&packages).Error; err != nil {
		return nil, err
	}

	for _, pkg := range packages {
		var plans []gpusPolicyPlanRow
		if err := DB.WithContext(ctx).Raw(
			`SELECT id, package_id, gpu_shared, array_to_json(gpu_device_ids)::text AS gpu_device_ids_json,
			        is_active, is_sold_out
			 FROM package_plans WHERE package_id = ? AND is_active = ?`, pkg.ID, true,
		).Scan(&plans).Error; err != nil {
			return nil, err
		}
		if len(plans) == 0 {
			offers[pkg.ID] = []GpusPolicy{gpusPackagePolicy(pkg)}
			continue
		}
		policies := make([]GpusPolicy, 0, len(plans))
		for index := range plans {
			if plans[index].IsSoldOut {
				continue
			}
			policies = append(policies, gpusResolvePolicy(gpusPackagePolicy(pkg), &plans[index]))
		}
		offers[pkg.ID] = policies
	}
	return offers, nil
}

// GpusOffersAvailableOnHost mirrors gpuOffersAvailableOnHost.
func GpusOffersAvailableOnHost(ctx context.Context, hostID int32, offers []GpusPolicy) (bool, error) {
	for _, policy := range offers {
		if !policy.GpuShared {
			return true, nil
		}
		devices, err := GpusFindAvailable(ctx, nil, hostID, policy, nil)
		if err != nil {
			return false, err
		}
		if len(devices) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// GpusPlanAvailable mirrors gpuPlanAvailable.
func GpusPlanAvailable(ctx context.Context, packageID, planID int32) (bool, error) {
	policy, err := GpusGetPolicy(ctx, nil, &packageID, &planID)
	if err != nil {
		return false, err
	}
	if !policy.GpuShared {
		return true, nil
	}

	var bindings []models.PackageHost
	if err := DB.WithContext(ctx).
		Select("package_hosts.host_id").
		Joins("JOIN hosts ON hosts.id = package_hosts.host_id").
		Where("package_hosts.package_id = ? AND hosts.status = ?", packageID, "online").
		Find(&bindings).Error; err != nil {
		return false, err
	}
	for _, binding := range bindings {
		devices, err := GpusFindAvailable(ctx, nil, binding.HostID, policy, nil)
		if err != nil {
			return false, err
		}
		if len(devices) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// gpusJSTruthy mirrors JavaScript's boolean coercion for JSON values.
func gpusJSTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case string:
		return typed != ""
	default:
		// Objects and arrays are always truthy in JavaScript.
		return true
	}
}

// formatJSNumber renders a float64 close to JavaScript's String(number) for the
// short device-name values a host report can carry.
func formatJSNumber(value float64) string {
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
