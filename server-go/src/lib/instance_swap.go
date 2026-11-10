// Port of server/src/lib/instance-swap.ts.
//
// Four tiny helpers that decide how an instance's swap size is stored and what
// Incus should be told. They are shared by routes/instances, routes/admin-billing,
// routes/batch-config, db/billing-operations, lib/incus-instances and the
// instance task worker, so the semantics have to be identical everywhere:
//
//   - an instance row that has an explicit swap_size wins over the plan;
//   - a NULL swap_size means "inherit the plan" and resolves to the plan value;
//   - negative values clamp to 0 (Math.max(0, Math.floor(x)));
//   - swap disabled, unset or zero is the Incus literal "false";
//   - a positive size becomes "<n>MiB".
//
// Fractional inputs: the TypeScript signature is `number`, but every caller feeds
// an Int column (instances.swap_size / package_plans.swap_size), so the Go port
// takes int32. `Math.floor` is therefore a no-op; the only observable behaviour
// left is the clamp at zero, which is preserved exactly.
package lib

import "strconv"

// instanceSwapNormalizeConfiguredSize mirrors normalizeConfiguredSwapSize:
// null/undefined stay null, anything else is clamped to a non-negative integer.
func instanceSwapNormalizeConfiguredSize(sizeMb *int32) *int32 {
	if sizeMb == nil {
		return nil
	}
	normalized := *sizeMb
	if normalized < 0 {
		normalized = 0
	}
	return &normalized
}

// InstanceSwapResolveEffectiveSize mirrors resolveEffectiveSwapSize.
func InstanceSwapResolveEffectiveSize(instanceSizeMb, planSizeMb *int32) int32 {
	if normalized := instanceSwapNormalizeConfiguredSize(instanceSizeMb); normalized != nil {
		return *normalized
	}
	if normalized := instanceSwapNormalizeConfiguredSize(planSizeMb); normalized != nil {
		return *normalized
	}
	return 0
}

// InstanceSwapShouldSyncWithPlan mirrors shouldSyncInstanceSwapSizeWithPlan.
//
// It answers "is the instance still tracking the plan?", which decides whether a
// plan change is allowed to rewrite the instance's swap size.
func InstanceSwapShouldSyncWithPlan(instanceSizeMb, previousPlanSizeMb *int32) bool {
	normalized := instanceSwapNormalizeConfiguredSize(instanceSizeMb)
	if normalized == nil {
		return true
	}
	planSize := int32(0)
	if normalizedPlan := instanceSwapNormalizeConfiguredSize(previousPlanSizeMb); normalizedPlan != nil {
		planSize = *normalizedPlan
	}
	return *normalized == planSize
}

// InstanceSwapFormatIncusLimit mirrors formatIncusSwapLimit.
func InstanceSwapFormatIncusLimit(sizeMb int32) string {
	if sizeMb < 0 {
		sizeMb = 0
	}
	return strconv.FormatInt(int64(sizeMb), 10) + "MiB"
}

// InstanceSwapResolveIncusValue mirrors resolveIncusSwapValue.
//
// `!enabled || !sizeMb || sizeMb <= 0` is one branch in the TypeScript; Go spells
// out the three conditions because a nil pointer is not falsy.
func InstanceSwapResolveIncusValue(enabled bool, sizeMb *int32) string {
	if !enabled || sizeMb == nil || *sizeMb <= 0 {
		return "false"
	}
	return InstanceSwapFormatIncusLimit(*sizeMb)
}
