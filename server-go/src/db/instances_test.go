package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestInstancesStatusTransitionApplies pins the WHERE filter of updateInstanceStatus'
// updateMany branch. This is the instance state machine the routes rely on:
//
//	running → stopped        applies
//	running → suspended      applies (an admin suspend)
//	running → deleted        applies (single-row branch)
//	suspended → running      DOES NOT apply (silently ignored, no error)
//	suspended → deleted      applies (deleting a suspended instance is the only
//	                                escape from the suspension filter)
//	deleted → running        DOES NOT apply
func TestInstancesStatusTransitionApplies(t *testing.T) {
	cases := []struct {
		name    string
		current models.InstanceStatus
		target  models.InstanceStatus
		want    bool
	}{
		{"creating to running", models.InstanceStatusCreating, models.InstanceStatusRunning, true},
		{"running to stopped", models.InstanceStatusRunning, models.InstanceStatusStopped, true},
		{"running to error", models.InstanceStatusRunning, models.InstanceStatusError, true},
		{"running to suspended", models.InstanceStatusRunning, models.InstanceStatusSuspended, true},
		{"running to deleted", models.InstanceStatusRunning, models.InstanceStatusDeleted, true},
		{"suspended to running", models.InstanceStatusSuspended, models.InstanceStatusRunning, false},
		{"suspended to stopped", models.InstanceStatusSuspended, models.InstanceStatusStopped, false},
		{"suspended to deleted", models.InstanceStatusSuspended, models.InstanceStatusDeleted, true},
		{"deleted to running", models.InstanceStatusDeleted, models.InstanceStatusRunning, false},
		{"deleted to stopped", models.InstanceStatusDeleted, models.InstanceStatusStopped, false},
		{"deleted to deleted", models.InstanceStatusDeleted, models.InstanceStatusDeleted, true},
	}
	for _, tc := range cases {
		if got := instanceStatusTransitionApplies(tc.current, tc.target); got != tc.want {
			t.Errorf("%s: instanceStatusTransitionApplies(%s, %s) = %v, want %v",
				tc.name, tc.current, tc.target, got, tc.want)
		}
	}
}

// TestInstancesStatusUpdateFilterSQL pins the generated WHERE filter of the
// updateMany branch. The Go predicate and the SQL must be built from the same list;
// this is the one place the list order/text is visible.
func TestInstancesStatusUpdateFilterSQL(t *testing.T) {
	if got := instanceStatusUpdateFilterSQL(); got != "status NOT IN ('deleted', 'suspended')" {
		t.Fatalf("filter = %q", got)
	}
	if len(instanceStatusUpdateBlocked) != 2 {
		t.Fatalf("blocked statuses = %v, want exactly deleted + suspended", instanceStatusUpdateBlocked)
	}
	// Every non-blocked status must reach the predicate as "applies".
	for _, status := range []models.InstanceStatus{
		models.InstanceStatusCreating, models.InstanceStatusRunning,
		models.InstanceStatusStopped, models.InstanceStatusError,
	} {
		if !instanceStatusTransitionApplies(status, models.InstanceStatusRunning) {
			t.Fatalf("%s must be able to transition to running", status)
		}
	}
}

// TestInstancesStatusAssignments pins the column list of updateInstanceStatus,
// including that 'deleted' also stamps gpu_released_at (a freed GPU seat must be
// visible at once) and that network fields are tri-state.
func TestInstancesStatusAssignments(t *testing.T) {
	sets, args := instanceStatusAssignments(models.InstanceStatusRunning, nil)
	if !reflect.DeepEqual(sets, []string{"status = ?"}) {
		t.Fatalf("no-network sets = %v, want [status = ?]", sets)
	}
	if len(args) != 1 || args[0] != string(models.InstanceStatusRunning) {
		t.Fatalf("no-network args = %v", args)
	}

	ipv4 := "10.10.1.5"
	sets, args = instanceStatusAssignments(models.InstanceStatusRunning, &InstanceNetworkUpdate{
		IPv4: InstanceValue(&ipv4),
		IPv6: InstanceNull[*string](),
	})
	wantSets := []string{"status = ?", "ipv4 = ?", "ipv6 = ?"}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("sets = %v, want %v", sets, wantSets)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v", args)
	}
	if value, ok := args[1].(*string); !ok || value == nil || *value != ipv4 {
		t.Fatalf("ipv4 arg = %#v, want %q", args[1], ipv4)
	}
	// InstanceNull must pass a typed nil pointer, i.e. SQL NULL, not "".
	if args[2] == "" {
		t.Fatal("explicit IPv6 clear must be NULL, not the empty string")
	}
	if value, ok := args[2].(*string); !ok || value != nil {
		t.Fatalf("IPv6 arg = %#v, want (*string)(nil)", args[2])
	}

	// The deleted branch appends gpu_released_at; verify the UTC clock is used so a
	// UTC+8 host does not persist a timestamp eight hours ahead.
	released := time.Now().UTC()
	if released.Location() != time.UTC {
		t.Fatalf("clock location = %v, want UTC", released.Location())
	}
}

// TestInstancesUpdateAssignmentsCoversEveryKey is the updateInstance contract:
// every optional key maps to exactly one column, the order matches the TypeScript
// literal, and an empty patch produces no statement at all (the early return that
// prevents a spurious updated_at bump).
func TestInstancesUpdateAssignmentsCoversEveryKey(t *testing.T) {
	empty, emptyArgs := instanceUpdateAssignments(InstanceUpdate{})
	if len(empty) != 0 || len(emptyArgs) != 0 {
		t.Fatalf("empty update produced %v / %v, want no assignment", empty, emptyArgs)
	}

	image := "images/debian12"
	name := "web-1"
	status := models.InstanceStatusRunning
	sshPort := int32(2222)
	password := "secret"
	portLimit := int32(10)
	snapshotLimit := int32(2)
	backupLimit := int32(3)
	siteLimit := int32(4)
	swapEnabled := true
	swapSize := int32(512)
	poolName := "local"
	sets, args := instanceUpdateAssignments(InstanceUpdate{
		Image: &image, Name: &name, Status: &status,
		SSHPort: InstanceValue(&sshPort), RootPassword: InstanceValue(&password),
		PortLimit: InstanceValue(&portLimit), SnapshotLimit: InstanceValue(&snapshotLimit),
		BackupLimit: InstanceValue(&backupLimit), SiteLimit: InstanceValue(&siteLimit),
		SwapEnabled: &swapEnabled, SwapSize: InstanceValue(&swapSize),
		StoragePoolName: InstanceValue(&poolName),
	})
	wantSets := []string{
		"image = ?", "name = ?", "status = ?", "ssh_port = ?", "root_password = ?",
		"port_limit = ?", "snapshot_limit = ?", "backup_limit = ?", "site_limit = ?",
		"swap_enabled = ?", "swap_size = ?", "storage_pool_name = ?",
	}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("sets = %v\nwant %v", sets, wantSets)
	}
	if len(args) != len(wantSets) {
		t.Fatalf("arg count = %d, want %d", len(args), len(wantSets))
	}

	// Unset != explicit null. An unset nullable field must not appear at all.
	sets, _ = instanceUpdateAssignments(InstanceUpdate{Image: &image})
	if !reflect.DeepEqual(sets, []string{"image = ?"}) {
		t.Fatalf("unset ssh_port must be skipped, sets = %v", sets)
	}
	sets, args = instanceUpdateAssignments(InstanceUpdate{SSHPort: InstanceNull[*int32]()})
	if !reflect.DeepEqual(sets, []string{"ssh_port = ?"}) {
		t.Fatalf("explicit null ssh_port must be written, sets = %v", sets)
	}
	if value, ok := args[0].(*int32); !ok || value != nil {
		t.Fatalf("ssh_port arg = %#v, want (*int32)(nil)", args[0])
	}
}

// TestInstancesQuotaAssignments pins updateInstanceQuota's port/snapshot/backup/site
// quota writes. Quotas are decremented/incremented by replacing the limit, so the
// only boundaries that matter are: all four keys are written in order, an explicit
// null is a NULL, and an empty patch is a no-op (idempotent re-application).
func TestInstancesQuotaAssignments(t *testing.T) {
	empty, _ := instanceQuotaAssignments(InstanceQuotaUpdate{})
	if len(empty) != 0 {
		t.Fatalf("empty quota update = %v, want no assignment", empty)
	}

	portLimit := int32(0) // zero is a real value: "no port mappings allowed"
	backupLimit := int32(7)
	sets, args := instanceQuotaAssignments(InstanceQuotaUpdate{
		PortLimit:     InstanceValue(&portLimit),
		SnapshotLimit: InstanceNull[*int32](),
		BackupLimit:   InstanceValue(&backupLimit),
	})
	wantSets := []string{"port_limit = ?", "snapshot_limit = ?", "backup_limit = ?"}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("sets = %v, want %v", sets, wantSets)
	}
	if len(args) != 3 {
		t.Fatalf("arg count = %d, want 3", len(args))
	}
	// A zero limit must be written, not skipped as falsy.
	if value, ok := args[0].(*int32); !ok || value == nil || *value != 0 {
		t.Fatalf("port_limit arg = %#v, want pointer to 0", args[0])
	}
	if value, ok := args[1].(*int32); !ok || value != nil {
		t.Fatalf("snapshot_limit arg = %#v, want (*int32)(nil)", args[1])
	}
}

// TestInstancesResourcesAssignments covers the CPU / memory / disk / traffic-limit
// resource accounting path. CPU and memory are quotas in percentage and MiB, so a
// zero is legal and must be written; a nil traffic limit means "unlimited" and must
// be written as NULL; an empty patch must not touch the row at all.
func TestInstancesResourcesAssignments(t *testing.T) {
	empty, _ := instanceResourcesAssignments(InstanceResourcesUpdate{})
	if len(empty) != 0 {
		t.Fatalf("empty resource update = %v, want no assignment", empty)
	}

	cpu := int32(0)
	memory := int32(512)
	disk := int32(10240)
	limit := int64(math.MaxInt64)
	sets, args := instanceResourcesAssignments(InstanceResourcesUpdate{
		CPU: &cpu, Memory: &memory, Disk: &disk,
		MonthlyTrafficLimit: InstanceValue(&limit),
	})
	wantSets := []string{"cpu = ?", "memory = ?", "disk = ?", "monthly_traffic_limit = ?"}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("sets = %v, want %v", sets, wantSets)
	}
	if value, ok := args[0].(int32); !ok || value != 0 {
		t.Fatalf("cpu arg = %#v, want 0 (a zero quota is written, not skipped)", args[0])
	}
	if value, ok := args[3].(*int64); !ok || value == nil || *value != math.MaxInt64 {
		t.Fatalf("monthly_traffic_limit arg = %#v, want pointer to MaxInt64", args[3])
	}

	sets, args = instanceResourcesAssignments(InstanceResourcesUpdate{
		MonthlyTrafficLimit: InstanceNull[*int64](),
	})
	if !reflect.DeepEqual(sets, []string{"monthly_traffic_limit = ?"}) {
		t.Fatalf("null limit sets = %v", sets)
	}
	if value, ok := args[0].(*int64); !ok || value != nil {
		t.Fatalf("null limit arg = %#v, want (*int64)(nil)", args[0])
	}
}

// TestInstancesConfigAssignments pins the two bandwidth column families:
// limitsIngress/limitsEgress write the CONFIGURED override columns, while the
// effectiveBandwidth argument writes the EFFECTIVE limits_ingress/limits_egress.
// Swapping them silently changes the instance's applied bandwidth after a restart.
func TestInstancesConfigAssignments(t *testing.T) {
	empty, _ := instanceConfigAssignments(InstanceConfigUpdate{}, nil)
	if len(empty) != 0 {
		t.Fatalf("empty config update = %v, want no assignment", empty)
	}

	ingress := "10Mbit"
	egress := "20Mbit"
	processes := int32(200)
	autostart := true
	sets, args := instanceConfigAssignments(InstanceConfigUpdate{
		LimitsIngress:     InstanceValue(&ingress),
		LimitsEgress:      InstanceValue(&egress),
		LimitsProcesses:   InstanceValue(&processes),
		BootAutostart:     InstanceValue(&autostart),
		LimitsRead:        InstanceNull[*string](),
		LimitsReadIOps:    InstanceNull[*int32](),
		LimitsCPUPriority: InstanceValue((*int32)(nil)),
	}, nil)
	// The order is the TypeScript literal order: limitsRead, limitsWrite,
	// limitsReadIops, limitsWriteIops, limitsIngress, limitsEgress, … — so the
	// read-iops entry must precede the ingress/egress ones.
	wantOrdered := []string{
		"limits_read = ?", "limits_read_iops = ?", "configured_limits_ingress = ?",
		"configured_limits_egress = ?", "limits_processes = ?", "limits_cpu_priority = ?",
		"boot_autostart = ?",
	}
	if !reflect.DeepEqual(sets, wantOrdered) {
		t.Fatalf("sets = %v\nwant %v", sets, wantOrdered)
	}
	if len(args) != len(wantOrdered) {
		t.Fatalf("arg count = %d, want %d", len(args), len(wantOrdered))
	}

	sets, args = instanceConfigAssignments(InstanceConfigUpdate{}, &InstanceEffectiveBandwidth{
		Ingress: &ingress,
		Egress:  &egress,
	})
	if !reflect.DeepEqual(sets, []string{"limits_ingress = ?", "limits_egress = ?"}) {
		t.Fatalf("effective bandwidth sets = %v", sets)
	}
	if args[0] != ingress || args[1] != egress {
		t.Fatalf("effective bandwidth args = %v", args)
	}
}

// TestInstancesSwapAssignments pins updateInstanceSwap's two columns.
func TestInstancesSwapAssignments(t *testing.T) {
	empty, _ := instanceSwapAssignments(InstanceSwapUpdate{})
	if len(empty) != 0 {
		t.Fatalf("empty swap update = %v, want no assignment", empty)
	}
	enabled := false
	size := int32(0)
	sets, args := instanceSwapAssignments(InstanceSwapUpdate{
		SwapEnabled: &enabled,
		SwapSize:    InstanceValue(&size),
	})
	if !reflect.DeepEqual(sets, []string{"swap_enabled = ?", "swap_size = ?"}) {
		t.Fatalf("sets = %v", sets)
	}
	if len(args) != 2 {
		t.Fatalf("args = %v", args)
	}
	if value, ok := args[1].(*int32); !ok || value == nil || *value != 0 {
		t.Fatalf("swap_size arg = %#v, want pointer to 0", args[1])
	}
	if value, ok := args[0].(bool); !ok || value {
		t.Fatalf("swap_enabled arg = %#v, want false (explicitly provided)", args[0])
	}
}

// TestInstancesMonthlyTrafficLimitString pins the BigInt string projection. A
// stored 0 is FALSY in JavaScript (`inst.monthlyTrafficLimit ? … : null`), so it
// must serialise as null instead of "0".
func TestInstancesMonthlyTrafficLimitString(t *testing.T) {
	if got := instancesMonthlyTrafficLimitString(nil); got != nil {
		t.Fatalf("nil = %v, want nil", *got)
	}
	zero := int64(0)
	if got := instancesMonthlyTrafficLimitString(&zero); got != nil {
		t.Fatalf("0 = %q, want nil (BigInt 0n is falsy)", *got)
	}
	one := int64(1)
	if got := instancesMonthlyTrafficLimitString(&one); got == nil || *got != "1" {
		t.Fatalf("1 = %v, want \"1\"", got)
	}
	max := int64(math.MaxInt64)
	if got := instancesMonthlyTrafficLimitString(&max); got == nil || *got != "9223372036854775807" {
		t.Fatalf("MaxInt64 = %v", got)
	}
}

// TestInstancesISOTime pins Date.prototype.toISOString formatting (UTC, exactly
// three fractional digits).
func TestInstancesISOTime(t *testing.T) {
	local := time.Date(2026, 11, 10, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if got := instancesISOTime(local); got != "2026-11-10T12:00:00.000Z" {
		t.Fatalf("instancesISOTime = %q", got)
	}
	withMillis := time.Date(2026, 11, 10, 12, 0, 0, 123_000_000, time.UTC)
	if got := instancesISOTime(withMillis); got != "2026-11-10T12:00:00.123Z" {
		t.Fatalf("instancesISOTime(ms) = %q", got)
	}
	if got := instancesOptionalISOTime(nil); got != nil {
		t.Fatalf("nil time = %v, want nil", *got)
	}
	formatted := instancesOptionalISOTime(&local)
	if formatted == nil || *formatted != "2026-11-10T12:00:00.000Z" {
		t.Fatalf("optional time = %v", formatted)
	}
}

// TestInstancesOptionalDecimalNumber pins `billingPrice ? Number(...) : null`.
// Prisma's Decimal is an object and therefore always truthy, so a stored 0 renders
// as 0, never as null.
func TestInstancesOptionalDecimalNumber(t *testing.T) {
	if got := instancesOptionalDecimalNumber(nil); got != nil {
		t.Fatalf("nil = %v, want nil", *got)
	}
	zero := models.Decimal{Decimal: decimal.Zero}
	if got := instancesOptionalDecimalNumber(&zero); got == nil || *got != 0 {
		t.Fatalf("0.00 = %v, want pointer to 0", got)
	}
	price, err := models.NewDecimalFromString("12.50")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := instancesOptionalDecimalNumber(&price); got == nil || *got != 12.5 {
		t.Fatalf("12.50 = %v, want 12.5", got)
	}
}

// TestInstanceHostedRecordJSONShape pins the wire shape of getInstancesByUserId:
// snake_case keys in the TypeScript literal's order, numbers as numbers, BigInt as
// a string, nullable columns as null, and host_location omitted when the host has
// none (`?? undefined` disappears from JSON).
func TestInstanceHostedRecordJSONShape(t *testing.T) {
	encoded, err := json.Marshal(InstanceHostedRecord{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{
		"id", "incus_id", "name", "user_id", "host_id", "package_id", "package_plan_id",
		"storage_pool_name", "icon_badge_id", "image", "status", "cpu", "memory", "disk",
		"ipv4", "ipv6", "network_mode", "ssh_port", "root_password", "port_limit",
		"snapshot_limit", "backup_limit", "site_limit", "swap_enabled", "swap_size",
		"monthly_traffic_limit", "limits_read", "limits_write", "limits_read_iops",
		"limits_write_iops", "limits_ingress", "limits_egress", "limits_processes",
		"limits_cpu_priority", "boot_autostart", "boot_autostart_priority",
		"boot_autostart_delay", "boot_host_shutdown_timeout", "expires_at", "suspended_at",
		"suspended_by", "suspend_reason", "created_at", "updated_at", "host_name",
		"host_country_code",
	}
	if len(decoded) != len(wantKeys) {
		t.Fatalf("key count = %d, want %d (%v)", len(decoded), len(wantKeys), keysOf(decoded))
	}
	order := keyOrder(t, encoded)
	if !reflect.DeepEqual(order, wantKeys) {
		t.Fatalf("key order mismatch:\n got %v\nwant %v", order, wantKeys)
	}

	location := "Tokyo"
	hosted := InstanceHostedRecord{HostLocation: &location}
	encoded, err = json.Marshal(hosted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"host_location":"Tokyo"`) {
		t.Fatalf("host_location missing: %s", encoded)
	}
}

// TestInstanceDetailRecordJSONShape pins the getInstanceById literal, whose tail
// (host_name … billing_price) differs from the list records.
func TestInstanceDetailRecordJSONShape(t *testing.T) {
	encoded, err := json.Marshal(InstanceDetailRecord{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	order := keyOrder(t, encoded)
	wantTail := []string{
		"host_name", "host_country_code", "host_storage_driver",
		"last_ipv6_reassign_at", "package_plan_id", "auto_renew", "billing_price",
	}
	if len(order) < len(wantTail) {
		t.Fatalf("too few keys: %v", order)
	}
	gotTail := order[len(order)-len(wantTail):]
	// nat_public_ip is omitempty and sits between host_country_code and
	// host_storage_driver when present, so the nil case is checked separately.
	if !reflect.DeepEqual(gotTail, wantTail) {
		t.Fatalf("detail tail = %v\nwant %v", gotTail, wantTail)
	}
	if strings.Contains(string(encoded), "nat_public_ip") {
		t.Fatalf("nil nat_public_ip must be omitted: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"billing_price":null`) {
		t.Fatalf("nil billing_price must be null: %s", encoded)
	}
}

// TestInstanceConfigSelectShape guards the classic mix-up: getInstanceConfig must
// read the CONFIGURED ingress/egress override columns, not the effective
// limits_ingress / limits_egress that updateInstanceConfig writes from
// effectiveBandwidth. Swapping them makes the admin form show the applied value as
// if it were the override.
func TestInstanceConfigSelectShape(t *testing.T) {
	if !strings.Contains(instanceConfigSelect, "configured_limits_ingress") ||
		!strings.Contains(instanceConfigSelect, "configured_limits_egress") {
		t.Fatalf("instanceConfigSelect must read the configured columns: %s", instanceConfigSelect)
	}
	if strings.Contains(instanceConfigSelect, " limits_ingress") ||
		strings.Contains(instanceConfigSelect, " limits_egress") {
		t.Fatalf("instanceConfigSelect must not read the effective columns: %s", instanceConfigSelect)
	}
}

// TestInstancesEgressErrorMessages pins the three NetworkInputError texts that
// bindPackageEgress can raise. The route returns them verbatim.
func TestInstancesEgressErrorMessages(t *testing.T) {
	cases := map[string]string{
		"source":   "Egress binding source no longer exists",
		"host":     "The host needs an online Agent supporting package egress",
		"profile":  "The instance egress profile is not permitted on the target host",
		"mode":     "Invalid package egress mode",
		"only":     "Only proxy mode may select an egress profile",
		"requires": "Package egress currently requires an unprivileged IPv4 NAT container",
	}
	for name, message := range cases {
		err := &InstanceEgressInputError{Message: message}
		if !errors.Is(err, err) || err.Error() != message {
			t.Errorf("%s: %q", name, err.Error())
		}
	}
	// The hook must stay unregistered by default: an unregistered hook means "no
	// proof this host supports package egress" and the bind fails closed.
	if InstanceEgressHostSupportsControl != nil {
		t.Fatal("InstanceEgressHostSupportsControl must default to nil (fail closed)")
	}
}

func TestInstancesEgressLock(t *testing.T) {
	if instanceEgressLockNamespace != 74201 {
		t.Fatalf("instanceEgressLockNamespace = %d, want 74201", instanceEgressLockNamespace)
	}
	if InstanceOperationLockNamespace != 4106 {
		t.Fatalf("InstanceOperationLockNamespace = %d, want 4106", InstanceOperationLockNamespace)
	}
}

// TestInstancesZeroLimitIsNotUnlimited documents the direction of the traffic limit
// boundary: 0 means "no traffic allowed" (LIMITED immediately), not "unlimited".
func TestInstancesZeroLimitIsNotUnlimited(t *testing.T) {
	zero := int64(0)
	if got := trafficCalculateInstanceStatus(0, &zero); got != models.TrafficStatusLIMITED {
		t.Fatalf("used=0 limit=0 status = %s, want LIMITED", got)
	}
	if got := trafficCalculateInstanceStatus(0, nil); got != models.TrafficStatusNORMAL {
		t.Fatalf("nil limit status = %s, want NORMAL", got)
	}
}

// keyOrder returns the JSON object's keys in the order they were encoded, which is
// what the Node backend produces (JavaScript preserves insertion order).
func keyOrder(t *testing.T, encoded []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil {
		t.Fatalf("read object start: %v", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		t.Fatalf("expected a JSON object, got %v", token)
	}
	keys := make([]string, 0, 16)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			t.Fatalf("expected a string key, got %v", keyToken)
		}
		keys = append(keys, key)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("read value of %q: %v", key, err)
		}
	}
	return keys
}

// keysOf renders the map keys sorted, for failure messages.
func keysOf(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
