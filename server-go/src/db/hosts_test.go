// Unit tests for the db/hosts.go port.
//
// There is no live database in `go test` (same convention as balance_test.go /
// users_test.go): the DB-backed half of the contract is pinned by the dual-run
// conformance suite once routes/hosts.go lands. What is anchored here are the
// parts that silently break production when they drift:
//
//   - the plan-upgrade capacity result (every rejection reason + boundary value);
//   - the host-selection gates (CPU/memory quota, NAT port exhaustion, public
//     IPv4 exhaustion, routed IPv6, tag selectors);
//   - the exact `FOR UPDATE` lock SQL — including the fact that it is a plain
//     row lock and NOT an advisory lock nor SKIP LOCKED;
//   - createHost's defaults (they must reproduce Prisma's "only send the columns
//     it sets" behaviour, otherwise DB defaults are overwritten);
//   - the tri-state update maps (`undefined` vs `null`) and the empty-update
//     early return;
//   - the JSON key subsets each lookup returns (the routes serialise them
//     directly, so an extra/missing key is an API change).
package db

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func hostsTestInt32Ptr(value int32) *int32 { return &value }

func hostsTestStringPtr(value string) *string { return &value }

// hostsTestSameValue compares map values, treating a typed nil pointer as nil
// (the create map stores typed nils for absent nullable columns).
func hostsTestSameValue(got, want any) bool {
	if want == nil {
		if got == nil {
			return true
		}
		value := reflect.ValueOf(got)
		switch value.Kind() {
		case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			return value.IsNil()
		}
		return false
	}
	// reflect.DeepEqual because the create map holds uncomparable values
	// (datatypes.JSON is a []byte).
	return reflect.DeepEqual(got, want)
}

// TestHostsPlanUpgradeCapacityResult pins buildPlanUpgradeCapacityResult.
func TestHostsPlanUpgradeCapacityResult(t *testing.T) {
	instance := HostsInstanceSpec{ID: 11, HostID: 9, CPU: 100, Memory: 1024, Disk: 10240}
	// upPlan requests more of everything; the deltas are +50 / +512 / +5120.
	upPlan := HostsPlanSpec{CPU: 150, Memory: 1536, Disk: 15360}
	downPlan := HostsPlanSpec{CPU: 50, Memory: 512, Disk: 5120}

	onlineHost := func() *HostsCapacityHost {
		return &HostsCapacityHost{
			ID:              9,
			Status:          "online",
			CPUUsed:         hostsTestInt32Ptr(0),
			MemoryUsed:      hostsTestInt32Ptr(0),
			DiskUsed:        hostsTestInt32Ptr(0),
			CPUAllowanceMax: hostsTestInt32Ptr(200),
			MemoryMax:       hostsTestInt32Ptr(2048),
			StorageSize:     hostsTestInt32Ptr(60),
		}
	}

	t.Run("host missing", func(t *testing.T) {
		check := hostsBuildPlanUpgradeCapacityResult(instance, upPlan, nil, HostsInstanceResourceSum{}, 0)
		if check.CanUpgrade || check.Reason == nil || *check.Reason != HostsPlanUpgradeHostNotFound {
			t.Fatalf("reason = %v, want %s", check.Reason, HostsPlanUpgradeHostNotFound)
		}
		if check.Message == nil || *check.Message != "HOST_NOT_FOUND" {
			t.Fatalf("message = %v, want HOST_NOT_FOUND", check.Message)
		}
		// The TypeScript reports instance.hostId here, not host.id (host is nil).
		if check.HostID != instance.HostID {
			t.Fatalf("hostId = %d, want %d", check.HostID, instance.HostID)
		}
		if check.CurrentUsage != (HostsResourceTriple{}) || check.ProjectedUsage != (HostsResourceTriple{}) {
			t.Fatalf("usage = %+v/%+v, want zero", check.CurrentUsage, check.ProjectedUsage)
		}
		if check.Limits.CPU != nil || check.Limits.Memory != nil || check.Limits.Disk != nil {
			t.Fatalf("limits = %+v, want all null", check.Limits)
		}
		if check.Deltas != (HostsResourceTriple{CPU: 50, Memory: 512, Disk: 5120}) {
			t.Fatalf("deltas = %+v", check.Deltas)
		}
	})

	t.Run("host offline", func(t *testing.T) {
		host := onlineHost()
		host.Status = "maintenance"
		check := hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, HostsInstanceResourceSum{}, 1000)
		if check.CanUpgrade || check.Reason == nil || *check.Reason != HostsPlanUpgradeHostNotOnline {
			t.Fatalf("reason = %v, want %s", check.Reason, HostsPlanUpgradeHostNotOnline)
		}
		if check.Limits.CPU == nil || *check.Limits.CPU != 200 {
			t.Fatalf("limits.cpu = %v, want 200", check.Limits.CPU)
		}
		if check.Limits.Disk == nil || *check.Limits.Disk != 1000 {
			t.Fatalf("limits.disk = %v, want 1000 (currentUsage.disk + diskAvailableMB)", check.Limits.Disk)
		}
	})

	t.Run("cpu boundary", func(t *testing.T) {
		cases := []struct {
			name      string
			cpuMax    *int32
			hostUsed  int32
			aggregate int64
			want      bool
			reason    string
		}{
			{"fits exactly", hostsTestInt32Ptr(150), 0, 100, true, ""},
			{"one over", hostsTestInt32Ptr(149), 0, 100, false, HostsPlanUpgradeCPUInsufficient},
			{"no limit", nil, 0, 100, false, HostsPlanUpgradeCPUInsufficient},
			{"zero limit", hostsTestInt32Ptr(0), 0, 100, false, HostsPlanUpgradeCPUInsufficient},
			{"negative limit", hostsTestInt32Ptr(-1), 0, 100, false, HostsPlanUpgradeCPUInsufficient},
			// Math.max(aggregate, host column): the host column wins below.
			{"host column wins", hostsTestInt32Ptr(160), 120, 0, false, HostsPlanUpgradeCPUInsufficient},
		}
		for _, testCase := range cases {
			host := onlineHost()
			host.CPUAllowanceMax = testCase.cpuMax
			host.CPUUsed = hostsTestInt32Ptr(testCase.hostUsed)
			aggregate := HostsInstanceResourceSum{CPUUsed: testCase.aggregate}
			check := hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, aggregate, 100000)
			if check.CanUpgrade != testCase.want {
				t.Fatalf("%s: canUpgrade = %v, want %v (%v)", testCase.name, check.CanUpgrade, testCase.want, check.Reason)
			}
			if !testCase.want && (check.Reason == nil || *check.Reason != testCase.reason) {
				t.Fatalf("%s: reason = %v, want %s", testCase.name, check.Reason, testCase.reason)
			}
		}
	})

	t.Run("memory boundary", func(t *testing.T) {
		// The instance itself counts as usage: 1024 + delta 512 = 1536.
		aggregate := HostsInstanceResourceSum{MemoryUsed: 1024}
		host := onlineHost()
		host.MemoryMax = hostsTestInt32Ptr(1536)
		check := hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, aggregate, 100000)
		if !check.CanUpgrade {
			t.Fatalf("memory exactly at the limit must pass, got %v", check.Reason)
		}
		host.MemoryMax = hostsTestInt32Ptr(1535)
		check = hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, aggregate, 100000)
		if check.CanUpgrade || *check.Reason != HostsPlanUpgradeMemoryInsufficient {
			t.Fatalf("reason = %v, want %s", check.Reason, HostsPlanUpgradeMemoryInsufficient)
		}
		host.MemoryMax = nil
		check = hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, aggregate, 100000)
		if check.CanUpgrade || *check.Reason != HostsPlanUpgradeMemoryInsufficient {
			t.Fatalf("missing memoryMax must reject, got %v", check.Reason)
		}
	})

	t.Run("disk boundary uses storage headroom", func(t *testing.T) {
		// currentUsage.disk = 10240 (the instance itself), available = 5120.
		host := onlineHost()
		check := hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, HostsInstanceResourceSum{DiskUsed: 10240}, 5120)
		if !check.CanUpgrade {
			t.Fatalf("disk exactly at the limit must pass, got %v", check.Reason)
		}
		if check.Limits.Disk == nil || *check.Limits.Disk != 15360 {
			t.Fatalf("limits.disk = %v, want 15360", check.Limits.Disk)
		}
		if check.Available.Disk == nil || *check.Available.Disk != 5120 {
			t.Fatalf("available.disk = %v, want 5120", check.Available.Disk)
		}
		check = hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, HostsInstanceResourceSum{DiskUsed: 10240}, 5119)
		if check.CanUpgrade || *check.Reason != HostsPlanUpgradeDiskInsufficient {
			t.Fatalf("reason = %v, want %s", check.Reason, HostsPlanUpgradeDiskInsufficient)
		}
		// storage_size is not used for the disk limit (only diskAvailableMB is).
		host.StorageSize = hostsTestInt32Ptr(999999)
		check = hostsBuildPlanUpgradeCapacityResult(instance, upPlan, host, HostsInstanceResourceSum{DiskUsed: 10240}, 5119)
		if check.CanUpgrade {
			t.Fatalf("storage_size must not widen the disk limit")
		}
	})

	t.Run("downgrade never rejects", func(t *testing.T) {
		host := onlineHost()
		host.CPUAllowanceMax = nil
		host.MemoryMax = nil
		check := hostsBuildPlanUpgradeCapacityResult(instance, downPlan, host, HostsInstanceResourceSum{CPUUsed: 100, MemoryUsed: 1024, DiskUsed: 10240}, 0)
		if !check.CanUpgrade {
			t.Fatalf("negative deltas must pass, got %v", check.Reason)
		}
		if check.Deltas != (HostsResourceTriple{CPU: -50, Memory: -512, Disk: -5120}) {
			t.Fatalf("deltas = %+v", check.Deltas)
		}
		if check.Limits.CPU != nil || check.Limits.Memory != nil {
			t.Fatalf("limits cpu/memory = %+v/%+v, want null", check.Limits.CPU, check.Limits.Memory)
		}
		if check.Available.CPU != nil || check.Available.Memory != nil {
			t.Fatalf("available cpu/memory must be null when the limit is null")
		}
	})

	t.Run("current usage takes the max of aggregate and column", func(t *testing.T) {
		host := onlineHost()
		host.CPUUsed = hostsTestInt32Ptr(120)
		host.MemoryUsed = hostsTestInt32Ptr(10)
		host.DiskUsed = hostsTestInt32Ptr(30)
		check := hostsBuildPlanUpgradeCapacityResult(instance, HostsPlanSpec{CPU: 100, Memory: 1024, Disk: 10240}, host,
			HostsInstanceResourceSum{CPUUsed: 80, MemoryUsed: 2048, DiskUsed: 20}, 0)
		if check.CurrentUsage != (HostsResourceTriple{CPU: 120, Memory: 2048, Disk: 30}) {
			t.Fatalf("currentUsage = %+v", check.CurrentUsage)
		}
	})

	t.Run("available is clamped at zero", func(t *testing.T) {
		host := onlineHost()
		host.CPUAllowanceMax = hostsTestInt32Ptr(100)
		check := hostsBuildPlanUpgradeCapacityResult(instance, HostsPlanSpec{CPU: 100, Memory: 1024, Disk: 10240}, host,
			HostsInstanceResourceSum{CPUUsed: 150}, 0)
		if check.Available.CPU == nil || *check.Available.CPU != 0 {
			t.Fatalf("available.cpu = %v, want 0", check.Available.CPU)
		}
	})

	t.Run("success clears reason and message", func(t *testing.T) {
		check := hostsBuildPlanUpgradeCapacityResult(instance, HostsPlanSpec{CPU: 100, Memory: 1024, Disk: 10240}, onlineHost(), HostsInstanceResourceSum{}, 100000)
		if !check.CanUpgrade || check.Reason != nil || check.Message != nil {
			t.Fatalf("result = %+v, want canUpgrade with nil reason/message", check)
		}
		if check.HostID != 9 {
			t.Fatalf("hostId = %d, want host.id", check.HostID)
		}
	})
}

// TestHostsSelectionGates pins the reject branches of the host selection
// (CPU/memory quota, public IPv4 exhaustion, NAT port exhaustion, routed IPv6,
// tag selectors). The loops that use them need a database, so their pure gates
// are exercised directly; the sql/IO half is covered by conformance.
func TestHostsSelectionGates(t *testing.T) {
	t.Run("cpu quota", func(t *testing.T) {
		cases := []struct {
			name   string
			max    *int32
			used   int64
			req    int64
			allows bool
		}{
			{"exactly fits", hostsTestInt32Ptr(100), 80, 20, true},
			{"one over", hostsTestInt32Ptr(100), 80, 21, false},
			{"no quota", nil, 0, 1, false},
			{"zero quota", hostsTestInt32Ptr(0), 0, 1, false},
			{"negative quota", hostsTestInt32Ptr(-5), 0, 0, false},
			{"zero request on full host", hostsTestInt32Ptr(100), 100, 0, true},
			{"already over", hostsTestInt32Ptr(100), 120, 0, false},
		}
		for _, testCase := range cases {
			if got := hostsCPUQuotaAllows(testCase.max, testCase.used, testCase.req); got != testCase.allows {
				t.Fatalf("%s: got %v, want %v", testCase.name, got, testCase.allows)
			}
		}
	})

	t.Run("memory quota", func(t *testing.T) {
		if !hostsMemoryQuotaAllows(hostsTestInt32Ptr(2048), 1024, 1024) {
			t.Fatalf("exact fit must pass")
		}
		if hostsMemoryQuotaAllows(hostsTestInt32Ptr(2048), 1024, 1025) {
			t.Fatalf("one over must reject")
		}
		if hostsMemoryQuotaAllows(nil, 0, 1) {
			t.Fatalf("missing quota must reject")
		}
	})

	t.Run("public ipv4 exhaustion", func(t *testing.T) {
		if hostsPublicIpv4Allows(0) || hostsPublicIpv4Allows(-1) {
			t.Fatalf("no free address must reject")
		}
		if !hostsPublicIpv4Allows(1) {
			t.Fatalf("one free address must pass")
		}
	})

	t.Run("nat port exhaustion", func(t *testing.T) {
		// 20000..20999 → 1000 ports.
		start, end := hostsTestInt32Ptr(20000), hostsTestInt32Ptr(20999)
		if !hostsNATPortsAllow(start, end, 995, 5) {
			t.Fatalf("exactly filling the pool must pass")
		}
		if hostsNATPortsAllow(start, end, 995, 6) {
			t.Fatalf("one port over must reject")
		}
		if hostsNATPortsAllow(start, end, 1000, 1) {
			t.Fatalf("exhausted pool must reject")
		}
		if hostsNATPortsAllow(nil, nil, 0, 1) {
			t.Fatalf("a host without a NAT range must reject any port request")
		}
		if !hostsNATPortsAllow(nil, nil, 0, 0) {
			t.Fatalf("no port request must always pass")
		}
		// Inverted range: end - start + 1 is negative, so any request rejects.
		invertedStart, invertedEnd := hostsTestInt32Ptr(30000), hostsTestInt32Ptr(29999)
		if hostsNATPortsAllow(invertedStart, invertedEnd, 0, 1) {
			t.Fatalf("inverted range must reject")
		}
	})

	t.Run("routed ipv6 configuration", func(t *testing.T) {
		empty := ""
		subnet := "2a06:de04::/64"
		parent := "eth0"
		if hostsRoutedIpv6Configured(nil, &parent) {
			t.Fatalf("missing subnet must reject")
		}
		if hostsRoutedIpv6Configured(&subnet, nil) {
			t.Fatalf("missing parent interface must reject")
		}
		if hostsRoutedIpv6Configured(&empty, &parent) {
			t.Fatalf("empty subnet is falsy in the TypeScript")
		}
		if !hostsRoutedIpv6Configured(&subnet, &parent) {
			t.Fatalf("fully configured host must pass")
		}
	})

	t.Run("tag selectors", func(t *testing.T) {
		tags := []string{"cn", "zfs"}
		if !hostsHasAllTags(tags, []string{"cn", "zfs"}) {
			t.Fatalf("all selectors present must pass")
		}
		if hostsHasAllTags(tags, []string{"cn", "nvme"}) {
			t.Fatalf("missing selector must reject")
		}
		if !hostsHasAllTags(tags, nil) {
			t.Fatalf("no selectors must pass")
		}
		if !hostsHasAllTags(nil, nil) {
			t.Fatalf("no selectors on an untagged host must pass")
		}
	})
}

// TestHostsLockQueryShape pins selectAndReserveHostWithLock's SQL.
//
// The lock is a plain `FOR UPDATE` row lock (server/src/db/hosts.ts:1057-1069).
// It must not become pg_advisory_xact_lock (different concurrency semantics) and
// — contrary to docs/WAVE_D_SPEC §4.4 — the TypeScript does NOT use SKIP LOCKED
// here (that lives in db/public-ipv4.ts for a different table).
func TestHostsLockQueryShape(t *testing.T) {
	const expected = `SELECT id, name, url, location, country_code, status, cert_path, key_path,
           nat_public_ip, nat_port_start, nat_port_end, 
           cpu_used, memory_used, disk_used,
           cpu_allowance_max, memory_max, storage_size,
           ipv6_mode, ipv6_subnet, ipv6_gateway, ipv6_parent_interface,
           instance_type, tags, nat_ports_used_count,
           created_at, updated_at
    FROM hosts
    WHERE status = 'online'
    ORDER BY memory_used ASC, cpu_used ASC
    FOR UPDATE`

	query, params := hostsBuildLockQuery(nil, nil, nil)
	if query != expected {
		t.Fatalf("lock query =\n%s\nwant\n%s", query, expected)
	}
	if len(params) != 0 {
		t.Fatalf("params = %v, want none", params)
	}
	if strings.Contains(query, "SKIP LOCKED") {
		t.Fatalf("SKIP LOCKED is not part of the Node lock query")
	}
	if strings.Contains(strings.ToLower(query), "advisory") {
		t.Fatalf("the lock must stay a row lock, not an advisory lock")
	}
	if !strings.Contains(query, "FOR UPDATE") {
		t.Fatalf("FOR UPDATE missing — the reservation would race")
	}

	t.Run("host id filter", func(t *testing.T) {
		hostID := int32(7)
		query, params := hostsBuildLockQuery(&hostID, []int32{1, 2}, nil)
		if !strings.Contains(query, "WHERE status = 'online' AND id = $1") {
			t.Fatalf("query = %s", query)
		}
		if len(params) != 1 || params[0] != int32(7) {
			t.Fatalf("params = %#v", params)
		}
	})

	t.Run("package host ids filter", func(t *testing.T) {
		query, params := hostsBuildLockQuery(nil, []int32{3, 4}, nil)
		if !strings.Contains(query, "AND id = ANY($1::int[])") {
			t.Fatalf("query = %s", query)
		}
		if len(params) != 1 {
			t.Fatalf("params = %#v", params)
		}
		if values, ok := params[0].([]int32); !ok || len(values) != 2 || values[0] != 3 || values[1] != 4 {
			t.Fatalf("params[0] = %#v", params[0])
		}
	})

	t.Run("owner filter", func(t *testing.T) {
		ownerID := int32(42)
		query, params := hostsBuildLockQuery(nil, nil, &ownerID)
		if !strings.Contains(query, "AND user_id = $1") {
			t.Fatalf("query = %s", query)
		}
		if len(params) != 1 || params[0] != int32(42) {
			t.Fatalf("params = %#v", params)
		}
	})

	t.Run("zero host id is treated as absent", func(t *testing.T) {
		zero := int32(0)
		ownerID := int32(42)
		query, params := hostsBuildLockQuery(&zero, nil, &ownerID)
		if strings.Contains(query, "id = $1") && !strings.Contains(query, "user_id") {
			t.Fatalf("a falsy hostId must be ignored: %s", query)
		}
		if !strings.Contains(query, "user_id = $1") || params[0] != int32(42) {
			t.Fatalf("owner filter expected, got %s %#v", query, params)
		}
	})
}

// TestHostsCreateValuesDefaults pins createHost's defaults. They are the reason
// HostsCreate inserts an explicit column map instead of a models.Host struct.
func TestHostsCreateValuesDefaults(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 123000000, time.UTC)
	values := hostsCreateValues(HostsCreateData{UserID: 3, Name: "node-1", URL: "https://10.0.0.1:8443"}, now)

	expectations := map[string]any{
		"user_id":               int32(3),
		"name":                  "node-1",
		"url":                   "https://10.0.0.1:8443",
		"location":              nil,
		"country_code":          "us",
		"architecture":          "x86_64",
		"tags":                  datatypes.JSON("[]"),
		"cert_path":             nil,
		"key_path":              nil,
		"nat_public_ip":         nil,
		"nat_public_ipv6":       nil,
		"nat_bind_ip":           nil,
		"nat_bind_ipv6":         nil,
		"nat_port_start":        nil,
		"nat_port_end":          nil,
		"ip_address":            nil,
		"storage_driver":        "zfs",
		"storage_type":          "loop",
		"storage_path":          nil,
		"storage_size":          int32(60),
		"ipv6_mode":             int32(1),
		"ipv6_subnet":           nil,
		"ipv6_gateway":          nil,
		"ipv6_parent_interface": nil,
		"enable_api":            true,
		"sysctl_config":         nil,
		"tunnel_enabled":        false,
		"target_host":           "127.0.0.1",
		"target_port":           int32(8443),
		"install_token":         nil,
		"install_token_expire":  nil,
		"is_installed":          false,
		"cert_download_count":   int32(0),
		"cert_download_expire":  nil,
		"cpu_allowance_max":     int32(0),
		"memory_max":            int32(0),
		"instance_type":         "container",
		"status":                "offline",
		"created_at":            now,
		"updated_at":            now,
	}
	for key, want := range expectations {
		got, ok := values[key]
		if !ok {
			t.Fatalf("missing column %q", key)
		}
		if !hostsTestSameValue(got, want) {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
	}
	if len(values) != len(expectations) {
		t.Fatalf("column count = %d, want %d (a new column silently changes the insert)", len(values), len(expectations))
	}

	// The defaults createHost never sends must NOT be written, otherwise the DB
	// defaults (notify_purchase=true, caddy_port=8444, traffic_reset_day=1 …) are
	// clobbered with zero values.
	for _, column := range []string{"caddy_enabled", "caddy_port", "transfer_enabled", "traffic_reset_day",
		"notify_purchase", "notify_renew", "notify_destroy", "enable_resource_pool", "announcement", "probe_url"} {
		if _, ok := values[column]; ok {
			t.Fatalf("column %q must not be part of the INSERT (database default must win)", column)
		}
	}

	if tags, ok := values["tags"].(datatypes.JSON); !ok || string(tags) != "[]" {
		t.Fatalf("tags = %#v, want []", values["tags"])
	}
}

// TestHostsCreateValuesOverrides checks the tri-state handling of enableApi and
// the explicit values.
func TestHostsCreateValuesOverrides(t *testing.T) {
	disabled := false
	country := "de"
	storageSize := int32(200)
	values := hostsCreateValues(HostsCreateData{
		UserID:       1,
		Name:         "n",
		URL:          "u",
		CountryCode:  &country,
		EnableAPI:    &disabled,
		StorageSize:  &storageSize,
		Tags:         []string{"a", "b"},
		InstanceType: hostsTestStringPtr("vm"),
	}, time.Now().UTC())

	if values["enable_api"] != false {
		t.Fatalf("explicit enableApi=false must be kept")
	}
	if values["country_code"] != "de" || values["storage_size"] != int32(200) {
		t.Fatalf("explicit values not applied: %#v %#v", values["country_code"], values["storage_size"])
	}
	if string(values["tags"].(datatypes.JSON)) != `["a","b"]` {
		t.Fatalf("tags = %s", values["tags"])
	}
	if values["instance_type"] != "vm" {
		t.Fatalf("instance_type = %v", values["instance_type"])
	}
}

// TestHostsUpdateValuesTriState pins `undefined` vs `null` and the empty-update
// early return.
func TestHostsUpdateValuesTriState(t *testing.T) {
	if values := hostsUpdateValues(HostsUpdateData{}); len(values) != 0 {
		t.Fatalf("empty update = %#v, want no keys (Prisma early return)", values)
	}

	values := hostsUpdateValues(HostsUpdateData{
		Name:            HostsValue("renamed"),
		Location:        HostsNull[*string](),
		NATPortStart:    HostsValue(hostsTestInt32Ptr(20000)),
		TrafficResetDay: HostsValue(int32(5)),
	})
	if values["name"] != "renamed" {
		t.Fatalf("name = %v", values["name"])
	}
	if location, ok := values["location"]; !ok || location != (*string)(nil) {
		t.Fatalf("location = %#v, want explicit nil", values["location"])
	}
	if start, ok := values["nat_port_start"].(*int32); !ok || start == nil || *start != 20000 {
		t.Fatalf("nat_port_start = %#v", values["nat_port_start"])
	}
	if values["traffic_reset_day"] != int32(5) {
		t.Fatalf("traffic_reset_day = %v", values["traffic_reset_day"])
	}

	// Explicit clear of a non-nullable column is a distinct value, not a skip.
	values = hostsUpdateValues(HostsUpdateData{NotifyPurchase: HostsValue(false)})
	if value, ok := values["notify_purchase"]; !ok || value != false {
		t.Fatalf("notify_purchase = %#v", values["notify_purchase"])
	}

	// Tags are stored as JSON.
	values = hostsUpdateValues(HostsUpdateData{Tags: HostsValue([]string{"x"})})
	if string(values["tags"].(datatypes.JSON)) != `["x"]` {
		t.Fatalf("tags = %s", values["tags"])
	}
}

// TestHostsResourceUpdateValues pins updateHostResources' map and its range guard.
func TestHostsResourceUpdateValues(t *testing.T) {
	values, err := hostsResourceUpdateValues(HostsResourceUpdate{})
	if err != nil || len(values) != 0 {
		t.Fatalf("empty = %#v, %v", values, err)
	}

	cpu := int64(150)
	instanceType := "both"
	values, err = hostsResourceUpdateValues(HostsResourceUpdate{CPUUsed: &cpu, InstanceType: &instanceType})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if values["cpu_used"] != int32(150) || values["instance_type"] != "both" {
		t.Fatalf("values = %#v", values)
	}

	overflow := int64(math.MaxInt32) + 1
	if _, err := hostsResourceUpdateValues(HostsResourceUpdate{MemoryUsed: &overflow}); err == nil {
		t.Fatalf("out-of-range int column value must fail instead of wrapping")
	}
}

func TestHostsInt32FromJS(t *testing.T) {
	cases := []struct {
		value   int64
		wantErr bool
	}{
		{math.MaxInt32, false},
		{math.MaxInt32 + 1, true},
		{math.MinInt32, false},
		{math.MinInt32 - 1, true},
		{0, false},
	}
	for _, testCase := range cases {
		_, err := hostsInt32FromJS(testCase.value, "col")
		if (err != nil) != testCase.wantErr {
			t.Fatalf("hostsInt32FromJS(%d) err = %v, want error=%v", testCase.value, err, testCase.wantErr)
		}
	}
}

// TestHostsISOTime pins Date.prototype.toISOString output.
func TestHostsISOTime(t *testing.T) {
	value := time.Date(2026, 11, 10, 12, 0, 0, 123000000, time.FixedZone("UTC+8", 8*3600))
	if got := hostsISOTime(value); got != "2026-11-10T04:00:00.123Z" {
		t.Fatalf("hostsISOTime = %q", got)
	}
	zero := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := hostsISOTime(zero); got != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("hostsISOTime = %q (milliseconds are mandatory)", got)
	}
	if hostsISOTimePtr(nil) != nil {
		t.Fatalf("nil time must render null")
	}
}

// TestHostsRecordJSONShape pins the per-function key subsets. The routes return
// these records verbatim, so the key set is part of the API contract.
func TestHostsRecordJSONShape(t *testing.T) {
	keys := func(t *testing.T, value any) map[string]json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		decoded := map[string]json.RawMessage{}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return decoded
	}

	host := models.Host{
		ID: 1, Name: "n", URL: "u", CountryCode: "us", Architecture: "x86_64",
		Status: models.HostStatusOnline, CPUUsed: 10, MemoryUsed: 20, DiskUsed: 30,
		CPUAllowanceMax: 100, MemoryMax: 200, StorageSize: 60, TargetHost: "127.0.0.1",
		TargetPort: 8443, InstanceType: models.InstanceTypeContainer,
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		Tags: datatypes.JSON("[]"),
	}

	summary := keys(t, hostsSummaryRecord(host))
	for _, absent := range []string{"user_id", "ip_address", "storage_driver", "ipv6_mode", "enable_api", "caddy_port"} {
		if _, ok := summary[absent]; ok {
			t.Fatalf("getAllHosts must not emit %q", absent)
		}
	}
	for _, present := range []string{"id", "name", "url", "location", "country_code", "architecture", "status",
		"cert_path", "key_path", "nat_public_ip", "nat_public_ipv6", "nat_bind_ip", "nat_bind_ipv6",
		"nat_port_start", "nat_port_end", "cpu_used", "memory_used", "disk_used", "cpu_allowance_max",
		"memory_max", "storage_size", "tunnel_enabled", "target_host", "target_port", "created_at", "updated_at"} {
		if _, ok := summary[present]; !ok {
			t.Fatalf("getAllHosts is missing %q", present)
		}
	}

	full := keys(t, hostsRecord(host))
	for _, present := range []string{"user_id", "ip_address", "storage_driver", "storage_type", "storage_path",
		"storage_size", "ipv6_mode", "ipv6_subnet", "ipv6_gateway", "ipv6_parent_interface", "enable_api",
		"sysctl_config", "instance_type", "caddy_enabled", "caddy_username", "caddy_password", "caddy_port",
		"transfer_enabled", "traffic_reset_day", "notify_purchase", "notify_renew", "notify_destroy",
		"enable_resource_pool", "announcement", "probe_url", "tunnel_enabled", "target_host", "target_port"} {
		if _, ok := full[present]; !ok {
			t.Fatalf("getHostById is missing %q", present)
		}
	}

	user := keys(t, HostsUserRecord{})
	if _, ok := user["user_id"]; !ok {
		t.Fatalf("getHostByUserAndName must emit user_id")
	}
	if _, ok := user["ip_address"]; ok {
		t.Fatalf("getHostByUserAndName must not emit ip_address")
	}

	// selectAndReserveHostWithLock: no disk_total, no tunnel fields, and the
	// allowance fields disappear when the raw column was NULL.
	reserved := keys(t, HostsSelectionRecord{})
	for _, absent := range []string{"disk_total", "tunnel_enabled", "target_host", "target_port", "cpu_allowance_max", "memory_max"} {
		if _, ok := reserved[absent]; ok {
			t.Fatalf("selectAndReserveHostWithLock must omit %q when unset", absent)
		}
	}
	if _, ok := reserved["storage_pool_name"]; !ok {
		t.Fatalf("storage_pool_name is always present (may be null)")
	}

	// selectAvailableHost always sets them, including zero/false values.
	diskTotal := int64(0)
	cpuMax := int32(0)
	memoryMax := int32(0)
	tunnelEnabled := false
	targetHost := ""
	targetPort := int32(8443)
	selected := keys(t, HostsSelectionRecord{
		DiskTotal: &diskTotal, CPUAllowanceMax: &cpuMax, MemoryMax: &memoryMax,
		TunnelEnabled: &tunnelEnabled, TargetHost: &targetHost, TargetPort: &targetPort,
	})
	for _, present := range []string{"disk_total", "tunnel_enabled", "target_host", "target_port", "cpu_allowance_max", "memory_max"} {
		if _, ok := selected[present]; !ok {
			t.Fatalf("selectAvailableHost must emit %q even at the zero value", present)
		}
	}
}

// TestHostsInstallTokenRecordExpire pins the Date → ISO string rendering.
func TestHostsInstallTokenRecordExpire(t *testing.T) {
	expire := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	record := HostsInstallTokenRecord{InstallTokenExpire: hostsISOTimePtr(&expire)}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"install_token_expire":"2026-11-10T12:00:00.000Z"`) {
		t.Fatalf("encoded = %s", encoded)
	}

	encoded, err = json.Marshal(HostsInstallTokenRecord{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"install_token_expire":null`) {
		t.Fatalf("encoded = %s", encoded)
	}
}

// TestHostsDatabaseNotInitialized makes an unwired call site fail loudly instead
// of dereferencing a nil GORM handle.
func TestHostsDatabaseNotInitialized(t *testing.T) {
	previous := DB
	DB = nil
	defer func() { DB = previous }()

	ctx := context.Background()
	if _, err := HostsGetAll(ctx); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsGetAll err = %v", err)
	}
	if _, err := HostsGetByID(ctx, 1); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsGetByID err = %v", err)
	}
	if _, err := HostsGetByUserAndName(ctx, 1, "n"); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsGetByUserAndName err = %v", err)
	}
	if _, err := HostsGetByInstallToken(ctx, "t"); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsGetByInstallToken err = %v", err)
	}
	if _, err := HostsCreate(ctx, HostsCreateData{}, nil); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsCreate err = %v", err)
	}
	if err := HostsUpdate(ctx, 1, HostsUpdateData{}, nil); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsUpdate err = %v", err)
	}
	if err := HostsUpdateStatus(ctx, 1, models.HostStatusOnline); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsUpdateStatus err = %v", err)
	}
	if _, err := HostsCalculateResourcesFromInstances(ctx, 1); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsCalculateResourcesFromInstances err = %v", err)
	}
	if err := HostsDelete(ctx, 1); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsDelete err = %v", err)
	}
	if _, err := HostsInstanceCount(ctx, 1); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsInstanceCount err = %v", err)
	}
	if _, err := HostsSelectAvailable(ctx, HostsSelectOptions{}); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsSelectAvailable err = %v", err)
	}
	if _, err := HostsCheckPlanUpgradeCapacity(ctx, HostsInstanceSpec{ID: 1, HostID: 1}, HostsPlanSpec{}, nil); !errors.Is(err, ErrHostsDatabaseNotInitialized) {
		t.Fatalf("HostsCheckPlanUpgradeCapacity err = %v", err)
	}
}

// TestHostsReservationRequiresTransaction: both lock paths must refuse to run
// without a transaction, otherwise the row lock silently disappears.
func TestHostsReservationRequiresTransaction(t *testing.T) {
	previous := DB
	DB = &gorm.DB{}
	defer func() { DB = previous }()

	ctx := context.Background()
	if _, err := HostsSelectAndReserveWithLock(ctx, nil, HostsReserveOptions{}); err == nil ||
		!strings.Contains(err.Error(), "requires a transaction") {
		t.Fatalf("selectAndReserveHostWithLock err = %v", err)
	}
	if _, err := HostsReservePlanUpgradeCapacityWithLock(ctx, nil, HostsInstanceSpec{ID: 1, HostID: 1}, HostsPlanSpec{}); err == nil ||
		!strings.Contains(err.Error(), "requires a transaction") {
		t.Fatalf("reservePlanUpgradeCapacityWithLock err = %v", err)
	}
}

// TestHostsStorageCapacityFailClosed: without a registered provider (the state
// until lib/storage_capacity.go lands) selection must reject, never assume disk.
func TestHostsStorageCapacityFailClosed(t *testing.T) {
	previous := hostsStorageCapacityProvider
	hostsStorageCapacityProvider = nil
	defer func() { hostsStorageCapacityProvider = previous }()

	capacity, err := hostsStorageCapacity(context.Background(), nil, 1, HostsStorageCapacityOptions{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if capacity.Reason == nil || *capacity.Reason != ResourcePoolStorageUnavailable {
		t.Fatalf("reason = %v, want %s", capacity.Reason, ResourcePoolStorageUnavailable)
	}
	if capacity.AvailableMB != 0 {
		t.Fatalf("availableMB = %d, want 0", capacity.AvailableMB)
	}

	// A registered provider is used verbatim.
	hostsStorageCapacityProvider = func(_ context.Context, _ *gorm.DB, hostID int32, options HostsStorageCapacityOptions) (ResourcePoolStorageCapacity, error) {
		if hostID != 5 {
			t.Fatalf("hostID = %d", hostID)
		}
		total := int64(1000)
		totalMB := int64(10)
		_ = total
		return ResourcePoolStorageCapacity{PoolName: hostsTestStringPtr("pool-1"), TotalMB: totalMB}, nil
	}
	capacity, err = hostsStorageCapacity(context.Background(), nil, 5, HostsStorageCapacityOptions{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if capacity.Reason != nil || capacity.PoolName == nil || *capacity.PoolName != "pool-1" {
		t.Fatalf("capacity = %+v", capacity)
	}
}

// TestHostsNetworkModePredicates pins the private copies of lib/network-modes.ts.
func TestHostsNetworkModePredicates(t *testing.T) {
	publicIpv4 := "public_ipv4"
	publicIpv4Ipv6 := "public_ipv4_ipv6"
	natIpv6 := "nat_ipv6"
	ipv6Only := "ipv6_only"
	nat := "nat"
	unknown := "bogus"
	empty := ""

	if !hostsNetworkModeNeedsPublicIpv4(&publicIpv4) || !hostsNetworkModeNeedsPublicIpv4(&publicIpv4Ipv6) {
		t.Fatalf("public ipv4 modes must be detected")
	}
	if hostsNetworkModeNeedsPublicIpv4(&nat) || hostsNetworkModeNeedsPublicIpv4(nil) || hostsNetworkModeNeedsPublicIpv4(&unknown) {
		t.Fatalf("nat/nil/unknown must not need public ipv4")
	}
	if !hostsNetworkModeNeedsRoutedIpv6(&natIpv6) || !hostsNetworkModeNeedsRoutedIpv6(&ipv6Only) || !hostsNetworkModeNeedsRoutedIpv6(&publicIpv4Ipv6) {
		t.Fatalf("routed ipv6 modes must be detected")
	}
	if hostsNetworkModeNeedsRoutedIpv6(&nat) || hostsNetworkModeNeedsRoutedIpv6(&empty) {
		t.Fatalf("nat/empty must not need routed ipv6")
	}
}

// TestHostsTagsHelpers pins the Json column round-trip.
func TestHostsTagsHelpers(t *testing.T) {
	if got := string(hostsJSONTags(nil)); got != "[]" {
		t.Fatalf("nil tags = %s, want []", got)
	}
	if got := hostsTagsOf(datatypes.JSON(`["a","b"]`)); len(got) != 2 || got[0] != "a" {
		t.Fatalf("tags = %#v", got)
	}
	if got := hostsTagsOf(nil); got != nil {
		t.Fatalf("nil json = %#v", got)
	}
	if got := hostsTagsOf(datatypes.JSON("not-json")); got != nil {
		t.Fatalf("malformed json = %#v", got)
	}
}
