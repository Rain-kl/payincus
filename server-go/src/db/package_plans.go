// Port of server/src/db/package-plans.ts (409 lines, 14 exports).
//
// Package plans are the money adjacency in this wave: `price` / `setup_fee` /
// `traffic_reset_price` / `sla_guarantee` are Decimal columns (never float64 in
// storage, WAVE_D_SPEC §0.3), `traffic_limit` is BigInt → int64, and every plan
// mutation re-validates the shared-GPU selection against the package's bound hosts
// BEFORE the write (validatePlanGpu).
//
// Three behaviours are easy to lose and are pinned by unit tests below:
//
//  1. Ordering/filtering: getPlansByPackageId orders ONLY by sort_order and, with
//     activeOnly, adds is_active = true. No secondary sort key exists in the
//     TypeScript, so none is added here (ties stay database-defined in both).
//  2. Name uniqueness is per package: isPlanNameUnique scopes to package_id and
//     excludes the edited plan only when a truthy excludeId is passed (0 behaves
//     like "no exclusion", matching `...(excludeId ? … : {})`).
//  3. Quota sync branches on the package's instance_type: a `vm` package forces
//     swapEnabled=false / swapSize=NULL for every plan instance, while a container
//     package keeps the swap flags and only rewrites swap_size for instances whose
//     swap is disabled and whose size was NULL or equal to the previous plan's
//     value — so a user's custom swap size survives a plan edit.
//
// package_plans.updated_at is NOT NULL with no database default (Prisma @updatedAt
// fills it client-side): every raw INSERT names it and every UPDATE bumps it.
package db

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Package plan errors.
var (
	// ErrPackagePlanNotFound mirrors Prisma P2025 for a missing package_plans row
	// (findUniqueOrThrow / update / delete all throw).
	ErrPackagePlanNotFound = errors.New("packagePlans: record not found")
	// ErrPackagePlanPackageNotFound mirrors findUniqueOrThrow on the owning package
	// (validatePlanGpu runs it first).
	ErrPackagePlanPackageNotFound = errors.New("packagePlans: package record not found")
	// ErrPackagePlansDatabaseNotInitialized keeps an unwired call site loud.
	ErrPackagePlansDatabaseNotInitialized = errors.New("packagePlans: database not initialized")
)

// ==================== tri-state helper ====================

// PackagePlanOptional is the same tri-state as InstanceOptional, kept local to keep
// the four ported files independently owned.
type PackagePlanOptional[T any] struct {
	Set   bool
	Value T
}

// PackagePlanValue marks a field as explicitly provided.
func PackagePlanValue[T any](value T) PackagePlanOptional[T] {
	return PackagePlanOptional[T]{Set: true, Value: value}
}

// PackagePlanNull marks a nullable field as explicitly cleared.
func PackagePlanNull[T any]() PackagePlanOptional[T] {
	var zero T
	return PackagePlanOptional[T]{Set: true, Value: zero}
}

// ==================== payload types ====================

// CreatePlanInput mirrors CreatePlanInput. Fields the TypeScript gives a default
// (`=` in the destructuring) are pointers here; nil means "use the default".
// Decimal columns take float64 because that is what the route hands in
// (`price: number`), exactly like the TypeScript.
type CreatePlanInput struct {
	GPUShared         *bool
	GPUDeviceIDs      []int32
	PackageID         int32
	Name              string
	Description       *string
	CPU               int32
	Memory            int32
	Disk              int32
	PortLimit         int32
	SnapshotLimit     int32
	BackupLimit       int32
	SiteLimit         int32
	SwapSize          int32
	TrafficLimit      int64
	TrafficLimitSpeed *string
	Price             float64
	TrafficResetPrice *float64
	BillingCycle      *int32
	SetupFee          *float64
	IsActive          *bool
	IsSoldOut         *bool
	SortOrder         *int32
	SlaGuarantee      *float64
}

// UpdatePlanInput mirrors UpdatePlanInput. Nullable columns use the tri-state
// because Prisma distinguishes `undefined` (leave alone) from `null` (clear).
type UpdatePlanInput struct {
	GPUShared         PackagePlanOptional[*bool]
	GPUDeviceIDs      []int32
	GPUDeviceIDsSet   bool
	Name              *string
	Description       PackagePlanOptional[*string]
	CPU               *int32
	Memory            *int32
	Disk              *int32
	PortLimit         *int32
	SnapshotLimit     *int32
	BackupLimit       *int32
	SiteLimit         *int32
	SwapSize          *int32
	TrafficLimit      *int64
	TrafficLimitSpeed *string
	Price             *float64
	TrafficResetPrice PackagePlanOptional[*float64]
	BillingCycle      *int32
	SetupFee          *float64
	IsActive          *bool
	IsSoldOut         *bool
	SortOrder         *int32
	SlaGuarantee      PackagePlanOptional[*float64]
}

// PackagePlansOptions mirrors `{ activeOnly?: boolean }`.
type PackagePlansOptions struct {
	ActiveOnly bool
}

// PackagePlanQuotaSyncResult mirrors syncPlanQuotaToInstances'
// `{ count: number; instanceIds: number[] }`.
type PackagePlanQuotaSyncResult struct {
	Count       int64   `json:"count"`
	InstanceIDs []int32 `json:"instanceIds"`
}

// CanDeletePlanResult mirrors canDeletePlan's return value. `reason` and
// `instanceCount` are absent (not null) when the plan can be deleted.
type CanDeletePlanResult struct {
	CanDelete     bool    `json:"canDelete"`
	Reason        *string `json:"reason,omitempty"`
	InstanceCount *int32  `json:"instanceCount,omitempty"`
}

// PlanResources mirrors getPlanResources' return value. trafficLimit is a string
// because the TypeScript calls BigInt.prototype.toString().
type PlanResources struct {
	CPU               int32  `json:"cpu"`
	Memory            int32  `json:"memory"`
	Disk              int32  `json:"disk"`
	PortLimit         int32  `json:"portLimit"`
	SnapshotLimit     int32  `json:"snapshotLimit"`
	BackupLimit       int32  `json:"backupLimit"`
	SiteLimit         int32  `json:"siteLimit"`
	SwapSize          int32  `json:"swapSize"`
	TrafficLimit      string `json:"trafficLimit"`
	TrafficLimitSpeed string `json:"trafficLimitSpeed"`
}

// PlanBilling mirrors getPlanBilling' return value. Numbers, not strings: the
// TypeScript converts each Decimal with Number().
type PlanBilling struct {
	Price             float64  `json:"price"`
	TrafficResetPrice *float64 `json:"trafficResetPrice"`
	BillingCycle      int32    `json:"billingCycle"`
	SetupFee          float64  `json:"setupFee"`
	MonthlyPrice      float64  `json:"monthlyPrice"`
}

// ==================== plumbing ====================

// packagePlanColumns is the projection in Prisma schema order. gpu_device_ids is
// an Int[] column, fetched as JSON text: GORM's schema parser rejects slice fields
// on a model query and pgx cannot scan int4[] into []int32 without a type map, so
// the text form is the one representation both sides agree on (same workaround as
// db/gpus.go).
const packagePlanColumns = `id, package_id, name, description, gpu_shared,
	array_to_json(gpu_device_ids)::text AS gpu_device_ids_json, cpu, memory, disk,
	port_limit, snapshot_limit, backup_limit, site_limit, swap_size, traffic_limit,
	traffic_limit_speed, price, billing_cycle, setup_fee, traffic_reset_price,
	is_active, is_sold_out, sort_order, sla_guarantee, created_at, updated_at`

// packagePlanRow is the scan target for packagePlanColumns.
type packagePlanRow struct {
	ID                int32
	PackageID         int32
	Name              string
	Description       *string
	GPUShared         *bool
	GPUDeviceIDsJSON  *string
	CPU               int32
	Memory            int32
	Disk              int32
	PortLimit         int32
	SnapshotLimit     int32
	BackupLimit       int32
	SiteLimit         int32
	SwapSize          int32
	TrafficLimit      int64
	TrafficLimitSpeed string
	Price             models.Decimal
	BillingCycle      int32
	SetupFee          models.Decimal
	TrafficResetPrice *models.Decimal
	IsActive          bool
	IsSoldOut         bool
	SortOrder         int32
	SlaGuarantee      *models.Decimal
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func packagePlanScanRow(scanner interface {
	Scan(dest ...any) error
}) (*packagePlanRow, error) {
	var row packagePlanRow
	err := scanner.Scan(&row.ID, &row.PackageID, &row.Name, &row.Description, &row.GPUShared,
		&row.GPUDeviceIDsJSON, &row.CPU, &row.Memory, &row.Disk, &row.PortLimit,
		&row.SnapshotLimit, &row.BackupLimit, &row.SiteLimit, &row.SwapSize,
		&row.TrafficLimit, &row.TrafficLimitSpeed, &row.Price, &row.BillingCycle,
		&row.SetupFee, &row.TrafficResetPrice, &row.IsActive, &row.IsSoldOut,
		&row.SortOrder, &row.SlaGuarantee, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// toModel mirrors the Prisma model shape the routes serialise.
func (row packagePlanRow) toModel() models.PackagePlan {
	return models.PackagePlan{
		ID:                row.ID,
		PackageID:         row.PackageID,
		Name:              row.Name,
		Description:       row.Description,
		GPUShared:         row.GPUShared,
		GPUDeviceIds:      gpusJSONInt32Array(row.GPUDeviceIDsJSON),
		CPU:               row.CPU,
		Memory:            row.Memory,
		Disk:              row.Disk,
		PortLimit:         row.PortLimit,
		SnapshotLimit:     row.SnapshotLimit,
		BackupLimit:       row.BackupLimit,
		SiteLimit:         row.SiteLimit,
		SwapSize:          row.SwapSize,
		TrafficLimit:      row.TrafficLimit,
		TrafficLimitSpeed: row.TrafficLimitSpeed,
		Price:             row.Price,
		BillingCycle:      row.BillingCycle,
		SetupFee:          row.SetupFee,
		TrafficResetPrice: row.TrafficResetPrice,
		IsActive:          row.IsActive,
		IsSoldOut:         row.IsSoldOut,
		SortOrder:         row.SortOrder,
		SlaGuarantee:      row.SlaGuarantee,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
}

func packagePlansRequireDB() error {
	if Pool() == nil || DB == nil {
		return ErrPackagePlansDatabaseNotInitialized
	}
	return nil
}

// PackagePlanDecimalFromNumber mirrors Prisma's Decimal(number): the JS double's
// shortest round-trip decimal string, never a binary artefact. Reuses the shared
// db/aff.go converter — the semantics are identical and having one implementation
// keeps every Decimal write consistent.
func PackagePlanDecimalFromNumber(value float64) models.Decimal {
	return affDecimalFromNumber(value)
}

// ==================== reads ====================

// GetPackagePlanById mirrors getPlanById: nil, nil when absent.
func GetPackagePlanById(ctx context.Context, id int32) (*models.PackagePlan, error) {
	if err := packagePlansRequireDB(); err != nil {
		return nil, err
	}
	row, err := packagePlanScanRow(Pool().QueryRow(ctx, `SELECT `+packagePlanColumns+`
		FROM package_plans WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plan := row.toModel()
	return &plan, nil
}

// packagePlanListQuery mirrors getPlansByPackageId's Prisma arguments: the filter
// is `package_id` plus, when activeOnly, `is_active = true`; the only sort key is
// sort_order ASC. No secondary key is added — the TypeScript has none, so ties stay
// database-defined in both implementations.
func packagePlanListQuery(activeOnly bool) string {
	query := `SELECT ` + packagePlanColumns + ` FROM package_plans WHERE package_id = $1`
	if activeOnly {
		query += ` AND is_active = true`
	}
	return query + ` ORDER BY sort_order ASC`
}

// GetPackagePlansByPackageId mirrors getPlansByPackageId: ordered by sort_order
// only, optionally restricted to is_active rows.
func GetPackagePlansByPackageId(ctx context.Context, packageID int32, options PackagePlansOptions) ([]models.PackagePlan, error) {
	if err := packagePlansRequireDB(); err != nil {
		return nil, err
	}
	rows, err := Pool().Query(ctx, packagePlanListQuery(options.ActiveOnly), packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plans := make([]models.PackagePlan, 0, 4)
	for rows.Next() {
		row, err := packagePlanScanRow(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, row.toModel())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return plans, nil
}

// GetActivePackagePlansByPackageId mirrors getActivePlansByPackageId.
func GetActivePackagePlansByPackageId(ctx context.Context, packageID int32) ([]models.PackagePlan, error) {
	return GetPackagePlansByPackageId(ctx, packageID, PackagePlansOptions{ActiveOnly: true})
}

// IsPaidPackage mirrors isPaidPackage: a package is paid when it has at least one
// active plan.
func IsPaidPackage(ctx context.Context, packageID int32) (bool, error) {
	if err := packagePlansRequireDB(); err != nil {
		return false, err
	}
	var count int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM package_plans
		WHERE package_id = $1 AND is_active = true`, packageID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// packagePlanNameUniqueQuery builds the uniqueness probe. excludeID is applied only
// when truthy, matching `...(excludeId ? { id: { not: excludeId } } : {})`: a nil
// or zero exclude id means "no exclusion", which is what the create path passes.
func packagePlanNameUniqueQuery(packageID int32, name string, excludeID *int32) (string, []any) {
	query := `SELECT EXISTS(SELECT 1 FROM package_plans WHERE package_id = $1 AND name = $2`
	args := []any{packageID, name}
	if excludeID != nil && *excludeID != 0 {
		query += ` AND id <> $3`
		args = append(args, *excludeID)
	}
	return query + `)`, args
}

// IsPackagePlanNameUnique mirrors isPlanNameUnique. excludeID is only applied when
// truthy, matching `...(excludeId ? { id: { not: excludeId } } : {})`.
func IsPackagePlanNameUnique(ctx context.Context, packageID int32, name string, excludeID *int32) (bool, error) {
	if err := packagePlansRequireDB(); err != nil {
		return false, err
	}
	query, args := packagePlanNameUniqueQuery(packageID, name, excludeID)

	var exists bool
	if err := Pool().QueryRow(ctx, query, args...).Scan(&exists); err != nil {
		return false, err
	}
	return !exists, nil
}

// ==================== create / update ====================

// CreatePackagePlan mirrors createPlan: validation first, then the insert with the
// column named, defaults applied, and updated_at written explicitly.
func CreatePackagePlan(ctx context.Context, input CreatePlanInput) (*models.PackagePlan, error) {
	if err := packagePlansRequireDB(); err != nil {
		return nil, err
	}

	gpuDeviceIDs := input.GPUDeviceIDs
	if gpuDeviceIDs == nil {
		gpuDeviceIDs = []int32{}
	}
	if err := packagePlanValidateGpu(ctx, input.PackageID, packagePlanGpuOverride{
		GpuShared:    input.GPUShared,
		GpuDeviceIDs: gpuDeviceIDs,
	}); err != nil {
		return nil, err
	}

	trafficLimitSpeed := "1Mbit"
	if input.TrafficLimitSpeed != nil {
		trafficLimitSpeed = *input.TrafficLimitSpeed
	}
	billingCycle := int32(1)
	if input.BillingCycle != nil {
		billingCycle = *input.BillingCycle
	}
	setupFee := 0.0
	if input.SetupFee != nil {
		setupFee = *input.SetupFee
	}
	isActive := true
	if input.IsActive != nil {
		isActive = *input.IsActive
	}
	isSoldOut := false
	if input.IsSoldOut != nil {
		isSoldOut = *input.IsSoldOut
	}
	sortOrder := int32(0)
	if input.SortOrder != nil {
		sortOrder = *input.SortOrder
	}

	var trafficResetPrice *models.Decimal
	if input.TrafficResetPrice != nil {
		converted := PackagePlanDecimalFromNumber(*input.TrafficResetPrice)
		trafficResetPrice = &converted
	}
	var slaGuarantee *models.Decimal
	if input.SlaGuarantee != nil {
		converted := PackagePlanDecimalFromNumber(*input.SlaGuarantee)
		slaGuarantee = &converted
	}

	now := time.Now().UTC()
	row, err := packagePlanScanRow(Pool().QueryRow(ctx, `INSERT INTO package_plans (
			gpu_shared, gpu_device_ids, package_id, name, description, cpu, memory, disk,
			port_limit, snapshot_limit, backup_limit, site_limit, swap_size,
			traffic_limit, traffic_limit_speed, price, traffic_reset_price, billing_cycle,
			setup_fee, is_active, is_sold_out, sort_order, sla_guarantee,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
			$16, $17, $18, $19, $20, $21, $22, $23, $24, $24)
		RETURNING `+packagePlanColumns,
		input.GPUShared, gpuDeviceIDs, input.PackageID, input.Name, input.Description,
		input.CPU, input.Memory, input.Disk, input.PortLimit, input.SnapshotLimit,
		input.BackupLimit, input.SiteLimit, input.SwapSize, input.TrafficLimit,
		trafficLimitSpeed, PackagePlanDecimalFromNumber(input.Price), trafficResetPrice,
		billingCycle, PackagePlanDecimalFromNumber(setupFee), isActive, isSoldOut,
		sortOrder, slaGuarantee, now,
	))
	if err != nil {
		return nil, err
	}
	plan := row.toModel()
	return &plan, nil
}

// packagePlanUpdateAssignments builds the SET list for UpdatePackagePlan.
func packagePlanUpdateAssignments(input UpdatePlanInput, args []any) ([]string, []any) {
	sets := make([]string, 0, 22)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}
	if input.GPUShared.Set {
		add("gpu_shared", input.GPUShared.Value)
	}
	if input.GPUDeviceIDsSet {
		add("gpu_device_ids", input.GPUDeviceIDs)
	}
	if input.Name != nil {
		add("name", *input.Name)
	}
	if input.Description.Set {
		add("description", input.Description.Value)
	}
	if input.CPU != nil {
		add("cpu", *input.CPU)
	}
	if input.Memory != nil {
		add("memory", *input.Memory)
	}
	if input.Disk != nil {
		add("disk", *input.Disk)
	}
	if input.PortLimit != nil {
		add("port_limit", *input.PortLimit)
	}
	if input.SnapshotLimit != nil {
		add("snapshot_limit", *input.SnapshotLimit)
	}
	if input.BackupLimit != nil {
		add("backup_limit", *input.BackupLimit)
	}
	if input.SiteLimit != nil {
		add("site_limit", *input.SiteLimit)
	}
	if input.SwapSize != nil {
		add("swap_size", *input.SwapSize)
	}
	if input.TrafficLimit != nil {
		add("traffic_limit", *input.TrafficLimit)
	}
	if input.TrafficLimitSpeed != nil {
		add("traffic_limit_speed", *input.TrafficLimitSpeed)
	}
	if input.Price != nil {
		add("price", PackagePlanDecimalFromNumber(*input.Price))
	}
	if input.TrafficResetPrice.Set {
		var value *models.Decimal
		if input.TrafficResetPrice.Value != nil {
			converted := PackagePlanDecimalFromNumber(*input.TrafficResetPrice.Value)
			value = &converted
		}
		add("traffic_reset_price", value)
	}
	if input.BillingCycle != nil {
		add("billing_cycle", *input.BillingCycle)
	}
	if input.SetupFee != nil {
		add("setup_fee", PackagePlanDecimalFromNumber(*input.SetupFee))
	}
	if input.IsActive != nil {
		add("is_active", *input.IsActive)
	}
	if input.IsSoldOut != nil {
		add("is_sold_out", *input.IsSoldOut)
	}
	if input.SortOrder != nil {
		add("sort_order", *input.SortOrder)
	}
	if input.SlaGuarantee.Set {
		var value *models.Decimal
		if input.SlaGuarantee.Value != nil {
			converted := PackagePlanDecimalFromNumber(*input.SlaGuarantee.Value)
			value = &converted
		}
		add("sla_guarantee", value)
	}
	return sets, args
}

// UpdatePackagePlan mirrors updatePlan: findUniqueOrThrow, validate the merged GPU
// selection, then update with exactly the provided keys. `data: input` in the
// TypeScript forwards only own enumerable keys, so absent keys are untouched; an
// explicitly empty patch still bumps updated_at (Prisma @updatedAt semantics).
func UpdatePackagePlan(ctx context.Context, id int32, input UpdatePlanInput) (*models.PackagePlan, error) {
	if err := packagePlansRequireDB(); err != nil {
		return nil, err
	}

	current, err := packagePlanScanRow(Pool().QueryRow(ctx, `SELECT `+packagePlanColumns+`
		FROM package_plans WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, ErrPackagePlanNotFound
	}
	if err != nil {
		return nil, err
	}

	// `{ ...current, ...input }` — a provided key wins, an absent one keeps the
	// current value. GPUDeviceIDs doubles as the plan's array column.
	mergedShared := current.GPUShared
	if input.GPUShared.Set {
		mergedShared = input.GPUShared.Value
	}
	mergedDevices := gpusJSONInt32Array(current.GPUDeviceIDsJSON)
	if input.GPUDeviceIDsSet {
		mergedDevices = input.GPUDeviceIDs
	}
	if err := packagePlanValidateGpu(ctx, current.PackageID, packagePlanGpuOverride{
		GpuShared:    mergedShared,
		GpuDeviceIDs: mergedDevices,
	}); err != nil {
		return nil, err
	}

	args := []any{id}
	sets, args := packagePlanUpdateAssignments(input, args)
	sets = append(sets, "updated_at = NOW()")

	row, err := packagePlanScanRow(Pool().QueryRow(ctx, `UPDATE package_plans SET `+
		strings.Join(sets, ", ")+` WHERE id = $1 RETURNING `+packagePlanColumns, args...))
	if isNoRows(err) {
		return nil, ErrPackagePlanNotFound
	}
	if err != nil {
		return nil, err
	}
	plan := row.toModel()
	return &plan, nil
}

// TogglePackagePlanActive mirrors togglePlanActive.
func TogglePackagePlanActive(ctx context.Context, id int32, isActive bool) (*models.PackagePlan, error) {
	if err := packagePlansRequireDB(); err != nil {
		return nil, err
	}
	row, err := packagePlanScanRow(Pool().QueryRow(ctx, `UPDATE package_plans
		SET is_active = $2, updated_at = NOW() WHERE id = $1
		RETURNING `+packagePlanColumns, id, isActive))
	if isNoRows(err) {
		return nil, ErrPackagePlanNotFound
	}
	if err != nil {
		return nil, err
	}
	plan := row.toModel()
	return &plan, nil
}

// ==================== delete ====================

// CanDeletePlan mirrors canDeletePlan: a plan that still owns instances cannot be
// deleted. The reason string interpolates the count with the TypeScript wording.
func CanDeletePlan(ctx context.Context, id int32) (CanDeletePlanResult, error) {
	if err := packagePlansRequireDB(); err != nil {
		return CanDeletePlanResult{}, err
	}
	var instanceCount int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM instances WHERE package_plan_id = $1`, id).
		Scan(&instanceCount); err != nil {
		return CanDeletePlanResult{}, err
	}
	if instanceCount > 0 {
		count := int32(instanceCount)
		reason := "该方案下还有 " + strconv.FormatInt(instanceCount, 10) + " 个实例，无法删除"
		return CanDeletePlanResult{CanDelete: false, Reason: &reason, InstanceCount: &count}, nil
	}
	return CanDeletePlanResult{CanDelete: true}, nil
}

// DeletePackagePlan mirrors deletePlan. Prisma's delete throws when the row is
// gone, so a zero-row delete is ErrPackagePlanNotFound rather than a silent no-op.
func DeletePackagePlan(ctx context.Context, id int32) error {
	if err := packagePlansRequireDB(); err != nil {
		return err
	}
	tag, err := Pool().Exec(ctx, `DELETE FROM package_plans WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrPackagePlanNotFound
	}
	return nil
}

// ==================== quota sync ====================

// PackagePlanShouldSyncSwap mirrors the final updateMany predicate of
// syncPlanQuotaToInstances: only instances whose swap is disabled and whose size
// is NULL or still equal to the previous plan value are overwritten, so a custom
// per-instance swap size survives a plan edit. It is the documented Go view of the
// SQL predicate in SyncPackagePlanQuotaToInstances — change both together (the unit
// test fails if the pair drifts semantically).
func PackagePlanShouldSyncSwap(swapEnabled bool, currentSwapSize *int32, previousPlanSwapSize *int32) bool {
	if swapEnabled {
		return false
	}
	if currentSwapSize == nil {
		return true
	}
	previous := int32(0)
	if previousPlanSwapSize != nil {
		previous = *previousPlanSwapSize
	}
	return *currentSwapSize == previous
}

// SyncPackagePlanQuotaToInstances mirrors syncPlanQuotaToInstances: copy the plan's
// quotas (and the traffic limit scaled by each host's multiplier) onto every
// instance of the plan.
//
// Like the TypeScript it is NOT wrapped in a transaction: each statement commits on
// its own and the traffic pass runs last, after the quota pass.
func SyncPackagePlanQuotaToInstances(
	ctx context.Context,
	planID int32,
	plan models.PackagePlan,
	previousPlanSwapSize *int32,
) (PackagePlanQuotaSyncResult, error) {
	if err := packagePlansRequireDB(); err != nil {
		return PackagePlanQuotaSyncResult{}, err
	}

	// pkg is optional in the TypeScript (`pkg?.instanceType`): a vanished package
	// makes the sync take the container branch with no multiplier bindings.
	instanceType := ""
	var packageExists bool
	err := Pool().QueryRow(ctx, `SELECT instance_type FROM packages WHERE id = $1`, plan.PackageID).
		Scan(&instanceType)
	if err != nil && !isNoRows(err) {
		return PackagePlanQuotaSyncResult{}, err
	}
	packageExists = err == nil

	multiplierByHostID := make(map[int32]models.Decimal, 8)
	if packageExists {
		rows, err := Pool().Query(ctx, `SELECT host_id, traffic_multiplier FROM package_hosts
			WHERE package_id = $1`, plan.PackageID)
		if err != nil {
			return PackagePlanQuotaSyncResult{}, err
		}
		for rows.Next() {
			var (
				hostID     int32
				multiplier models.Decimal
			)
			if err := rows.Scan(&hostID, &multiplier); err != nil {
				rows.Close()
				return PackagePlanQuotaSyncResult{}, err
			}
			multiplierByHostID[hostID] = multiplier
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return PackagePlanQuotaSyncResult{}, err
		}
		rows.Close()
	}

	applyTrafficLimits := func() ([]int32, error) {
		rows, err := Pool().Query(ctx, `SELECT id, host_id, monthly_traffic_used
			FROM instances WHERE package_plan_id = $1`, planID)
		if err != nil {
			return nil, err
		}
		type planInstance struct {
			id          int32
			hostID      int32
			monthlyUsed int64
		}
		instances := make([]planInstance, 0, 8)
		for rows.Next() {
			var instance planInstance
			if err := rows.Scan(&instance.id, &instance.hostID, &instance.monthlyUsed); err != nil {
				rows.Close()
				return nil, err
			}
			instances = append(instances, instance)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()

		updatedIDs := make([]int32, 0, len(instances))
		for _, instance := range instances {
			multiplier := any(float64(1))
			if bound, ok := multiplierByHostID[instance.hostID]; ok {
				multiplier = bound
			}
			base := plan.TrafficLimit
			monthlyTrafficLimit, err := ApplyTrafficMultiplier(&base, multiplier)
			if err != nil {
				return nil, err
			}
			// The push happens before the write in the TypeScript too (inside the
			// synchronous part of the map callback).
			updatedIDs = append(updatedIDs, instance.id)
			status := trafficCalculateInstanceStatus(instance.monthlyUsed, monthlyTrafficLimit)
			if _, err := Pool().Exec(ctx, `UPDATE instances
				SET monthly_traffic_limit = $2, traffic_status = $3, updated_at = NOW()
				WHERE id = $1`, instance.id, monthlyTrafficLimit, string(status)); err != nil {
				return nil, err
			}
		}
		return updatedIDs, nil
	}

	if instanceType == "vm" {
		tag, err := Pool().Exec(ctx, `UPDATE instances SET
				port_limit = $2, snapshot_limit = $3, backup_limit = $4, site_limit = $5,
				swap_enabled = false, swap_size = NULL, updated_at = NOW()
			WHERE package_plan_id = $1`,
			planID, plan.PortLimit, plan.SnapshotLimit, plan.BackupLimit, plan.SiteLimit)
		if err != nil {
			return PackagePlanQuotaSyncResult{}, err
		}
		instanceIDs, err := applyTrafficLimits()
		if err != nil {
			return PackagePlanQuotaSyncResult{}, err
		}
		return PackagePlanQuotaSyncResult{Count: tag.RowsAffected(), InstanceIDs: instanceIDs}, nil
	}

	// Enabled and disabled rows get the same quota update; they are two statements
	// because the TypeScript sums their counts.
	enabledTag, err := Pool().Exec(ctx, `UPDATE instances SET
			port_limit = $2, snapshot_limit = $3, backup_limit = $4, site_limit = $5,
			updated_at = NOW()
		WHERE package_plan_id = $1 AND swap_enabled = true`,
		planID, plan.PortLimit, plan.SnapshotLimit, plan.BackupLimit, plan.SiteLimit)
	if err != nil {
		return PackagePlanQuotaSyncResult{}, err
	}
	disabledTag, err := Pool().Exec(ctx, `UPDATE instances SET
			port_limit = $2, snapshot_limit = $3, backup_limit = $4, site_limit = $5,
			updated_at = NOW()
		WHERE package_plan_id = $1 AND swap_enabled = false`,
		planID, plan.PortLimit, plan.SnapshotLimit, plan.BackupLimit, plan.SiteLimit)
	if err != nil {
		return PackagePlanQuotaSyncResult{}, err
	}

	previousSwapSize := int32(0)
	if previousPlanSwapSize != nil {
		previousSwapSize = *previousPlanSwapSize
	}
	if _, err := Pool().Exec(ctx, `UPDATE instances SET swap_size = $2, updated_at = NOW()
		WHERE package_plan_id = $1 AND swap_enabled = false
			AND (swap_size IS NULL OR swap_size = $3)`,
		planID, plan.SwapSize, previousSwapSize); err != nil {
		return PackagePlanQuotaSyncResult{}, err
	}

	instanceIDs, err := applyTrafficLimits()
	if err != nil {
		return PackagePlanQuotaSyncResult{}, err
	}
	return PackagePlanQuotaSyncResult{
		Count:       enabledTag.RowsAffected() + disabledTag.RowsAffected(),
		InstanceIDs: instanceIDs,
	}, nil
}

// ==================== pure helpers ====================

// PackagePlanCalculateMonthlyPrice mirrors calculateMonthlyPrice: price is stored
// in cents per billing cycle, the result is cents per month. A zero/negative cycle
// is treated as 1 so the division never explodes (`Math.max(cycle || 1, 1)`).
func PackagePlanCalculateMonthlyPrice(plan models.PackagePlan) float64 {
	price := decimalToFloat64(plan.Price)
	if math.IsNaN(price) {
		price = 0
	}
	cycle := plan.BillingCycle
	if cycle < 1 {
		cycle = 1
	}
	return price / float64(cycle)
}

// GetPackagePlanResources mirrors getPlanResources.
func GetPackagePlanResources(plan models.PackagePlan) PlanResources {
	return PlanResources{
		CPU:               plan.CPU,
		Memory:            plan.Memory,
		Disk:              plan.Disk,
		PortLimit:         plan.PortLimit,
		SnapshotLimit:     plan.SnapshotLimit,
		BackupLimit:       plan.BackupLimit,
		SiteLimit:         plan.SiteLimit,
		SwapSize:          plan.SwapSize,
		TrafficLimit:      strconv.FormatInt(plan.TrafficLimit, 10),
		TrafficLimitSpeed: plan.TrafficLimitSpeed,
	}
}

// GetPackagePlanBilling mirrors getPlanBilling.
func GetPackagePlanBilling(plan models.PackagePlan) PlanBilling {
	var trafficResetPrice *float64
	if plan.TrafficResetPrice != nil {
		value := decimalToFloat64(*plan.TrafficResetPrice)
		trafficResetPrice = &value
	}
	return PlanBilling{
		Price:             decimalToFloat64(plan.Price),
		TrafficResetPrice: trafficResetPrice,
		BillingCycle:      plan.BillingCycle,
		SetupFee:          decimalToFloat64(plan.SetupFee),
		MonthlyPrice:      PackagePlanCalculateMonthlyPrice(plan),
	}
}

// ==================== GPU validation (file-private) ====================

// packagePlanGpuOverride mirrors the `{ gpuShared, gpuDeviceIds }` pair the
// TypeScript spreads over the package before validating.
type packagePlanGpuOverride struct {
	GpuShared    *bool
	GpuDeviceIDs []int32
}

// packagePlanValidateGpu mirrors validatePlanGpu:
//
//	const pkg = await prisma.package.findUniqueOrThrow({ where: { id: packageId },
//	  include: { packageHosts: true } })
//	await validateGpuSelection(prisma, { ...pkg, ...resolveGpuPolicy(pkg, plan),
//	  hostIds: pkg.packageHosts.map(b => b.hostId) })
//
// The package read is raw because packages carries Int[] columns (see
// packagePlanColumns note) and the selection needs instance_type / privileged /
// the bound host ids.
func packagePlanValidateGpu(ctx context.Context, packageID int32, override packagePlanGpuOverride) error {
	if err := packagePlansRequireDB(); err != nil {
		return err
	}

	var (
		pkgShared    bool
		pkgDevices   *string
		instanceType string
		privileged   bool
	)
	err := Pool().QueryRow(ctx, `SELECT gpu_shared,
			array_to_json(gpu_device_ids)::text AS gpu_device_ids_json,
			instance_type, privileged
		FROM packages WHERE id = $1`, packageID).
		Scan(&pkgShared, &pkgDevices, &instanceType, &privileged)
	if isNoRows(err) {
		return ErrPackagePlanPackageNotFound
	}
	if err != nil {
		return err
	}

	// resolveGpuPolicy: a non-nil override gpuShared wins, otherwise the package
	// policy applies (gpuShared === null means "inherit").
	resolvedShared := pkgShared
	resolvedDevices := gpusJSONInt32Array(pkgDevices)
	if override.GpuShared != nil {
		resolvedShared = *override.GpuShared
		resolvedDevices = override.GpuDeviceIDs
	}

	hostIDs := make([]int32, 0, 4)
	rows, err := Pool().Query(ctx, `SELECT host_id FROM package_hosts WHERE package_id = $1`, packageID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var hostID int32
		if err := rows.Scan(&hostID); err != nil {
			rows.Close()
			return err
		}
		hostIDs = append(hostIDs, hostID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	return GpusValidateSelection(ctx, nil, GpusSelectionInput{
		GpuShared:    resolvedShared,
		GpuDeviceIDs: resolvedDevices,
		InstanceType: instanceType,
		Privileged:   privileged,
		HostIDs:      hostIDs,
	})
}
