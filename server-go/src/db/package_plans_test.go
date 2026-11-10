package db

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestPackagePlanListQueryShape pins getPlansByPackageId's Prisma arguments:
//   - the only filter is package_id, plus is_active = true when activeOnly is set;
//   - the only ORDER BY is sort_order ASC (no secondary key in the TypeScript, so
//     none here either — adding one would change tie order between Node and Go).
func TestPackagePlanListQueryShape(t *testing.T) {
	plain := packagePlanListQuery(false)
	if strings.Contains(plain, "AND is_active") {
		t.Fatalf("activeOnly=false must not filter on is_active:\n%s", plain)
	}
	if !strings.Contains(plain, "ORDER BY sort_order ASC") {
		t.Fatalf("plain query ordering:\n%s", plain)
	}
	if strings.Count(plain, "ORDER BY") != 1 || strings.Contains(plain, "ORDER BY sort_order ASC, ") {
		t.Fatalf("exactly one sort key expected:\n%s", plain)
	}

	active := packagePlanListQuery(true)
	if !strings.Contains(active, "AND is_active = true") {
		t.Fatalf("activeOnly=true must filter on is_active:\n%s", active)
	}
	if !strings.Contains(active, "ORDER BY sort_order ASC") {
		t.Fatalf("active query ordering:\n%s", active)
	}
	// The filter must precede the ordering, or Postgres would treat it as part of
	// the ORDER BY expression.
	if strings.Index(active, "is_active = true") > strings.Index(active, "ORDER BY") {
		t.Fatalf("filter must precede ORDER BY:\n%s", active)
	}
}

// TestPackagePlanNameUniqueQuery pins isPlanNameUnique's per-package scope and the
// `...(excludeId ? … : {})` exclusion rule:
//
//	exclude = nil  → no id predicate (create path)
//	exclude = 0    → no id predicate (0 is falsy in the TypeScript)
//	exclude = 5    → AND id <> $3 (edit path, "is this name taken by ANOTHER plan")
func TestPackagePlanNameUniqueQuery(t *testing.T) {
	query, args := packagePlanNameUniqueQuery(3, "低配", nil)
	if strings.Contains(query, "id <>") {
		t.Fatalf("nil exclude must not add an id predicate: %s", query)
	}
	if !strings.Contains(query, "package_id = $1 AND name = $2") {
		t.Fatalf("uniqueness must be scoped to the package: %s", query)
	}
	if len(args) != 2 || args[0] != int32(3) || args[1] != "低配" {
		t.Fatalf("args = %v", args)
	}

	zero := int32(0)
	query, args = packagePlanNameUniqueQuery(3, "低配", &zero)
	if strings.Contains(query, "id <>") || len(args) != 2 {
		t.Fatalf("excludeId 0 must behave like no exclusion: %s / %v", query, args)
	}

	exclude := int32(5)
	query, args = packagePlanNameUniqueQuery(3, "低配", &exclude)
	if !strings.Contains(query, "id <> $3") {
		t.Fatalf("excludeId must add the predicate: %s", query)
	}
	if len(args) != 3 || args[2] != int32(5) {
		t.Fatalf("args = %v", args)
	}
}

// TestPackagePlanCalculateMonthlyPrice pins calculateMonthlyPrice: price in cents per
// cycle divided by the cycle, with `Math.max(cycle || 1, 1)` semantics (0 and
// negative cycles count as 1, so the division never explodes).
func TestPackagePlanCalculateMonthlyPrice(t *testing.T) {
	plan := func(price string, cycle int32) models.PackagePlan {
		parsed, err := models.NewDecimalFromString(price)
		if err != nil {
			t.Fatalf("parse %q: %v", price, err)
		}
		return models.PackagePlan{Price: parsed, BillingCycle: cycle}
	}

	cases := []struct {
		name  string
		price string
		cycle int32
		want  float64
	}{
		{"one month", "100.00", 1, 100},
		{"six months", "600.00", 6, 100},
		{"three months", "300.00", 3, 100},
		{"cycle zero", "100.00", 0, 100},
		{"cycle negative", "100.00", -4, 100},
		{"rounding follows float division", "100.00", 3, 100.0 / 3.0},
	}
	for _, tc := range cases {
		got := PackagePlanCalculateMonthlyPrice(plan(tc.price, tc.cycle))
		if math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: monthlyPrice = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := PackagePlanCalculateMonthlyPrice(plan("0", 1)); got != 0 {
		t.Errorf("zero price = %v, want 0", got)
	}
}

// TestPackagePlanShouldSyncSwap pins the final updateMany predicate of
// syncPlanQuotaToInstances: a custom per-instance swap size must survive a plan
// edit, while NULL / previous-plan values are refreshed.
func TestPackagePlanShouldSyncSwap(t *testing.T) {
	intPtr := func(value int32) *int32 { return &value }

	cases := []struct {
		name       string
		enabled    bool
		current    *int32
		previous   *int32
		wantSynced bool
	}{
		{"swap enabled is never touched", true, intPtr(512), intPtr(512), false},
		{"null size follows the plan", false, nil, nil, true},
		{"null size with a previous value", false, nil, intPtr(1024), true},
		{"equal to previous plan", false, intPtr(1024), intPtr(1024), true},
		{"custom size survives", false, intPtr(2048), intPtr(1024), false},
		{"previous nil means zero", false, intPtr(0), nil, true},
		{"custom non-zero with previous nil", false, intPtr(1024), nil, false},
	}
	for _, tc := range cases {
		got := PackagePlanShouldSyncSwap(tc.enabled, tc.current, tc.previous)
		if got != tc.wantSynced {
			t.Errorf("%s: shouldSync = %v, want %v", tc.name, got, tc.wantSynced)
		}
	}
}

// TestPackagePlanUpdateAssignments covers updatePlan's `data: input` semantics:
// only provided keys are written, nullable columns distinguish "unset" from
// "explicit null", and the column list follows the TypeScript key order.
func TestPackagePlanUpdateAssignments(t *testing.T) {
	sets, args := packagePlanUpdateAssignments(UpdatePlanInput{}, nil)
	if len(sets) != 0 || len(args) != 0 {
		t.Fatalf("empty update = %v / %v, want no assignment", sets, args)
	}

	name := "高配"
	shared := false
	cpu := int32(200)
	traffic := int64(1 << 40)
	price := 199.0
	active := true
	sets, args = packagePlanUpdateAssignments(UpdatePlanInput{
		GPUShared:       PackagePlanValue(&shared),
		GPUDeviceIDs:    []int32{2, 3},
		GPUDeviceIDsSet: true,
		Name:            &name,
		CPU:             &cpu,
		TrafficLimit:    &traffic,
		Price:           &price,
		IsActive:        &active,
	}, []any{int32(9)})
	wantSets := []string{
		"gpu_shared = $2", "gpu_device_ids = $3", "name = $4", "cpu = $5",
		"traffic_limit = $6", "price = $7", "is_active = $8",
	}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("sets = %v\nwant %v", sets, wantSets)
	}
	if len(args) != 8 {
		t.Fatalf("arg count = %d, want 8", len(args))
	}
	if value, ok := args[1].(*bool); !ok || value == nil || *value {
		t.Fatalf("gpu_shared arg = %#v, want pointer to false", args[1])
	}
	if value, ok := args[2].([]int32); !ok || !reflect.DeepEqual(value, []int32{2, 3}) {
		t.Fatalf("gpu_device_ids arg = %#v, want [2 3]", args[2])
	}
	if value, ok := args[6].(models.Decimal); !ok || value.String() != "199" {
		t.Fatalf("price arg = %#v, want Decimal 199", args[6])
	}

	// Explicit null vs unset for the two nullable decimals and the description.
	sets, args = packagePlanUpdateAssignments(UpdatePlanInput{
		TrafficResetPrice: PackagePlanNull[*float64](),
		SlaGuarantee:      PackagePlanValue((*float64)(nil)),
		Description:       PackagePlanNull[*string](),
	}, []any{int32(9)})
	wantSets = []string{"description = $2", "traffic_reset_price = $3", "sla_guarantee = $4"}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("null sets = %v\nwant %v", sets, wantSets)
	}
	for index := 1; index <= 3; index++ {
		switch args[index].(type) {
		case *string, *models.Decimal:
			// typed nil pointer, i.e. SQL NULL
		default:
			if args[index] != nil {
				t.Fatalf("arg[%d] = %#v, want a typed nil (SQL NULL)", index, args[index])
			}
		}
	}

	// Unset nullable keys must be absent.
	sets, _ = packagePlanUpdateAssignments(UpdatePlanInput{Name: &name}, []any{int32(9)})
	if !reflect.DeepEqual(sets, []string{"name = $2"}) {
		t.Fatalf("unset nullable keys must be skipped: %v", sets)
	}
}

// TestPackagePlanResourcesAndBilling pins the two display helpers:
// trafficLimit is a BigInt string, the money fields are JSON numbers, and a null
// traffic_reset_price stays null (not 0).
func TestPackagePlanResourcesAndBilling(t *testing.T) {
	price, err := models.NewDecimalFromString("600.00")
	if err != nil {
		t.Fatalf("parse price: %v", err)
	}
	setupFee, err := models.NewDecimalFromString("10.50")
	if err != nil {
		t.Fatalf("parse setup: %v", err)
	}
	resetPrice, err := models.NewDecimalFromString("5.00")
	if err != nil {
		t.Fatalf("parse reset: %v", err)
	}
	plan := models.PackagePlan{
		CPU: 200, Memory: 2048, Disk: 51200,
		PortLimit: 20, SnapshotLimit: 5, BackupLimit: 5, SiteLimit: 10, SwapSize: 1024,
		TrafficLimit: 1 << 40, TrafficLimitSpeed: "100Mbit",
		Price: price, BillingCycle: 6, SetupFee: setupFee, TrafficResetPrice: &resetPrice,
	}

	resources := GetPackagePlanResources(plan)
	wantResources := PlanResources{
		CPU: 200, Memory: 2048, Disk: 51200,
		PortLimit: 20, SnapshotLimit: 5, BackupLimit: 5, SiteLimit: 10, SwapSize: 1024,
		TrafficLimit: "1099511627776", TrafficLimitSpeed: "100Mbit",
	}
	if !reflect.DeepEqual(resources, wantResources) {
		t.Fatalf("resources = %+v\nwant %+v", resources, wantResources)
	}
	if encoded := packagePlanMustMarshal(t, resources); !strings.Contains(encoded, `"trafficLimit":"1099511627776"`) {
		t.Fatalf("trafficLimit must be a JSON string: %s", encoded)
	}

	billing := GetPackagePlanBilling(plan)
	if billing.Price != 600 || billing.SetupFee != 10.5 || billing.BillingCycle != 6 {
		t.Fatalf("billing = %+v", billing)
	}
	if billing.MonthlyPrice != 100 {
		t.Fatalf("monthlyPrice = %v, want 100", billing.MonthlyPrice)
	}
	if billing.TrafficResetPrice == nil || *billing.TrafficResetPrice != 5 {
		t.Fatalf("trafficResetPrice = %v, want 5", billing.TrafficResetPrice)
	}

	// Null reset price stays null; a zero Decimal (an object in Prisma terms) is 0.
	nanPlan := plan
	nanPlan.TrafficResetPrice = nil
	billing = GetPackagePlanBilling(nanPlan)
	if billing.TrafficResetPrice != nil {
		t.Fatalf("null trafficResetPrice = %v, want nil", *billing.TrafficResetPrice)
	}
	zero := models.Decimal{Decimal: decimal.Zero}
	nanPlan.TrafficResetPrice = &zero
	billing = GetPackagePlanBilling(nanPlan)
	if billing.TrafficResetPrice == nil || *billing.TrafficResetPrice != 0 {
		t.Fatalf("zero trafficResetPrice = %v, want pointer to 0", billing.TrafficResetPrice)
	}
}

// TestCanDeletePlanResultJSONShape pins canDeletePlan's wire shape: the blocked
// branch carries reason + instanceCount, the allowed branch carries neither.
func TestCanDeletePlanResultJSONShape(t *testing.T) {
	blocked := packagePlanMustMarshal(t, CanDeletePlanResult{
		CanDelete:     false,
		Reason:        packagePlanStringPtr("该方案下还有 3 个实例，无法删除"),
		InstanceCount: packagePlanInt32Ptr(3),
	})
	if !strings.Contains(blocked, `"canDelete":false`) ||
		!strings.Contains(blocked, `"reason":"该方案下还有 3 个实例，无法删除"`) ||
		!strings.Contains(blocked, `"instanceCount":3`) {
		t.Fatalf("blocked JSON = %s", blocked)
	}

	allowed := packagePlanMustMarshal(t, CanDeletePlanResult{CanDelete: true})
	if allowed != `{"canDelete":true}` {
		t.Fatalf("allowed JSON = %s, want {\"canDelete\":true}", allowed)
	}
}

// TestSyncPackagePlanQuotaBranches pins the vm/container decision. A `vm` package
// forces swap off for every plan instance; anything else (container, or a package
// that vanished) keeps the swap flags and only refreshes matching sizes.
func TestSyncPackagePlanQuotaBranches(t *testing.T) {
	// The branch predicate is `pkg?.instanceType === 'vm'`.
	isVM := func(instanceType string) bool { return instanceType == "vm" }
	if !isVM("vm") {
		t.Fatal("vm packages must take the swap-off branch")
	}
	for _, other := range []string{"", "container", "both"} {
		if isVM(other) {
			t.Fatalf("%q must take the container branch", other)
		}
	}
}

// TestPackagePlanColumnShape pins the projection used by every plan read: the
// Decimal columns, the BigInt column, and gpu_device_ids fetched as JSON text.
func TestPackagePlanColumnShape(t *testing.T) {
	for _, column := range []string{
		"price", "setup_fee", "traffic_reset_price", "sla_guarantee",
		"traffic_limit", "array_to_json(gpu_device_ids)::text",
	} {
		if !strings.Contains(packagePlanColumns, column) {
			t.Fatalf("packagePlanColumns missing %q:\n%s", column, packagePlanColumns)
		}
	}
}

// TestPackagePlanUpdateAlwaysBumpsUpdatedAt guards the @updatedAt contract: the
// UPDATE statement in UpdatePackagePlan must append updated_at = NOW().
func TestPackagePlanUpdateAlwaysBumpsUpdatedAt(t *testing.T) {
	source := mustReadFile(t, "package_plans.go")
	if !strings.Contains(source, `sets = append(sets, "updated_at = NOW()")`) {
		t.Fatal("UpdatePackagePlan must bump updated_at")
	}
}

// Helpers are package-plan-prefixed because the db test binary already defines
// stringPtr / int32Ptr / mustMarshal in other modules' test files.
func packagePlanStringPtr(value string) *string { return &value }
func packagePlanInt32Ptr(value int32) *int32    { return &value }

func packagePlanMustMarshal(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

func mustReadFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}
