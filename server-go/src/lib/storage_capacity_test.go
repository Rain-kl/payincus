package lib

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// Every expected value in this file was produced by running the TypeScript
// implementation (server/src/lib/storage-capacity.ts and
// storage-capacity-policy.ts) under Node.
func TestStorageHeadroomMBMatchesNode(t *testing.T) {
	cases := []struct {
		totalMB int64
		want    int64
	}{
		{0, 1024},
		{-100, 1024},
		{1, 1024},
		{102399, 1024},
		{102400, 1024},
		{102401, 1025},
		{100000, 1024},
		{1024000, 10240},
		{1024001, 10240},
		{4500000, 10240},
		{1_000_000_000_000_000, 10240},
	}
	for _, tc := range cases {
		if got := StorageHeadroomMB(tc.totalMB); got != tc.want {
			t.Errorf("StorageHeadroomMB(%d) = %d, want %d", tc.totalMB, got, tc.want)
		}
	}
}

func TestStorageReportMaxAgeMSMatchesNode(t *testing.T) {
	cases := []struct {
		name   string
		report string
		want   float64
	}{
		{"no report", "", 120000},
		{"null", "null", 120000},
		{"empty object", "{}", 120000},
		{"no metrics", `{"metrics":{}}`, 120000},
		{"lower bound 5s", `{"metrics":{"heartbeatIntervalSeconds":5}}`, 120000},
		{"41s", `{"metrics":{"heartbeatIntervalSeconds":41}}`, 123000},
		{"60s", `{"metrics":{"heartbeatIntervalSeconds":60}}`, 180000},
		{"upper bound 3600s", `{"metrics":{"heartbeatIntervalSeconds":3600}}`, 10800000},
		{"below the bound", `{"metrics":{"heartbeatIntervalSeconds":4}}`, 120000},
		{"above the bound", `{"metrics":{"heartbeatIntervalSeconds":3601}}`, 120000},
		{"numeric string (Number())", `{"metrics":{"heartbeatIntervalSeconds":"60"}}`, 180000},
		{"null interval", `{"metrics":{"heartbeatIntervalSeconds":null}}`, 120000},
		{"fractional interval", `{"metrics":{"heartbeatIntervalSeconds":7.5}}`, 120000},
		{"fractional interval above the floor", `{"metrics":{"heartbeatIntervalSeconds":100.5}}`, 301500},
		{"garbage interval", `{"metrics":{"heartbeatIntervalSeconds":"soon"}}`, 120000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StorageReportMaxAgeMS([]byte(tc.report))
			if got != tc.want {
				t.Fatalf("StorageReportMaxAgeMS(%s) = %v, want %v", tc.report, got, tc.want)
			}
		})
	}
}

func TestStorageCapacityGroup(t *testing.T) {
	cases := []struct {
		name      string
		driver    string
		config    string
		telemetry string
		poolName  string
		want      string
	}{
		{"dir with filesystem id", "dir", `{}`, `{"filesystemId":"fs-1"}`, "data", "filesystem:fs-1"},
		{"dir without telemetry", "dir", `{}`, ``, "data", "filesystem:unknown"},
		{"dir with empty filesystem id", "dir", `{}`, `{"filesystemId":""}`, "data", "filesystem:unknown"},
		{"btrfs shares the dir grouping", "btrfs", `{}`, `{"filesystemId":"fs-9"}`, "btrfs-pool", "filesystem:fs-9"},
		{"zfs pool name", "zfs", `{"zfs.pool_name":"tank/vm"}`, ``, "pool0", "zfs:tank"},
		{"zfs source path", "zfs", `{"source":"/dev/sda"}`, ``, "pool0", "zfs:pool0"},
		{"zfs bare source", "zfs", `{"source":"tank2/x"}`, ``, "pool0", "zfs:tank2"},
		{"zfs default", "zfs", `{}`, ``, "pool0", "zfs:pool0"},
		{"lvm vg name", "lvm", `{"lvm.vg_name":"vg0"}`, ``, "pool0", "lvm:vg0"},
		{"lvm source", "lvm", `{"source":"vg1"}`, ``, "pool0", "lvm:vg1"},
		{"lvm default", "lvm", `{}`, ``, "pool0", "lvm:pool0"},
		{"other driver", "ceph", `{}`, ``, "pool0", "ceph:pool0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := storageCapacityPoolRow{
				Name:      tc.poolName,
				Driver:    tc.driver,
				Config:    []byte(tc.config),
				Telemetry: []byte(tc.telemetry),
			}
			if got := storageCapacityGroup(pool); got != tc.want {
				t.Fatalf("group = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStorageCapacityPoolReady(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nowMS := now.UnixMilli()
	reported := func(offset time.Duration) *time.Time {
		value := now.Add(offset)
		return &value
	}
	const maxAge = 120000.0

	base := storageCapacityPoolRow{
		Name:       "data",
		Driver:     "dir",
		Status:     "ready",
		TotalBytes: 1024 * 1024 * 1024,
		ReportedAt: reported(-time.Second),
	}
	if !storageCapacityPoolReady(base, nowMS, maxAge) {
		t.Fatal("a fresh ready pool must be usable")
	}

	stale := base
	stale.ReportedAt = reported(-121 * time.Second)
	if storageCapacityPoolReady(stale, nowMS, maxAge) {
		t.Fatal("a report older than maxAge must be unusable")
	}

	future := base
	future.ReportedAt = reported(60 * time.Second)
	if !storageCapacityPoolReady(future, nowMS, maxAge) {
		t.Fatal("a future report passes `now - reportedAt <= maxAge` in JS")
	}

	pending := base
	pending.Status = "pending"
	if storageCapacityPoolReady(pending, nowMS, maxAge) {
		t.Fatal("only status=ready pools are usable")
	}

	missing := base
	missing.ReportedAt = nil
	if storageCapacityPoolReady(missing, nowMS, maxAge) {
		t.Fatal("a pool without reportedAt is unusable")
	}

	empty := base
	empty.TotalBytes = 0
	if storageCapacityPoolReady(empty, nowMS, maxAge) {
		t.Fatal("a pool with zero bytes is unusable")
	}

	for _, config := range []string{
		`{"_payincusImport":true}`,
		`{"_payincusDeletion":true}`,
		`{"_payincusImport":1}`,
		`{"_payincusDeletion":"yes"}`,
	} {
		flagged := base
		flagged.Config = []byte(config)
		if storageCapacityPoolReady(flagged, nowMS, maxAge) {
			t.Fatalf("config %s must make the pool unusable", config)
		}
	}
	for _, config := range []string{
		`{"_payincusImport":false}`,
		`{"_payincusDeletion":0}`,
		`{"_payincusImport":""}`,
		`{"_payincusImport":null}`,
	} {
		allowed := base
		allowed.Config = []byte(config)
		if !storageCapacityPoolReady(allowed, nowMS, maxAge) {
			t.Fatalf("config %s must not disable the pool", config)
		}
	}
}

func TestStorageCapacityJSTruthy(t *testing.T) {
	truthy := []any{true, 1.0, -1.0, "x", map[string]any{}, []any{}}
	for _, value := range truthy {
		if !storageCapacityJSTruthy(value) {
			t.Errorf("%#v must be truthy", value)
		}
	}
	falsy := []any{nil, false, 0.0, "", math.NaN()}
	for _, value := range falsy {
		if storageCapacityJSTruthy(value) {
			t.Errorf("%#v must be falsy", value)
		}
	}
}

// capacityBytes renders a MiB count as bytes for the arithmetic cases below.
func capacityBytes(mib int64) int64 { return mib * StorageCapacityMIB }

func TestStorageCapacityChoiceUnitConversionAndBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nowMS := now.UnixMilli()
	fresh := func() *time.Time { value := now.Add(-time.Second); return &value }
	readyPool := func(name string, totalBytes, usedBytes int64) storageCapacityPoolRow {
		return storageCapacityPoolRow{
			Name:       name,
			Driver:     "dir",
			Status:     "ready",
			TotalBytes: totalBytes,
			UsedBytes:  usedBytes,
			ReportedAt: fresh(),
		}
	}

	t.Run("GiB to MiB, floor total / ceil used", func(t *testing.T) {
		const gib = int64(1024 * 1024 * 1024)
		pool := readyPool("data", 4468*gib, 1690*gib)
		got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, nil, 0, nowMS, 120000, 1)
		if got.TotalMB != 4575232 || got.UsedMB != 1730560 {
			t.Fatalf("total/used = %d/%d, want 4575232/1730560", got.TotalMB, got.UsedMB)
		}
		if got.HeadroomMB != 10240 {
			t.Fatalf("headroom = %d, want 10240", got.HeadroomMB)
		}
		// min(4575232-10240-0-0, 4575232-1730560-10240, same group)
		// 4575232 - 1730560 - 10240
		if got.AvailableMB != 2834432 {
			t.Fatalf("available = %d, want 2834432", got.AvailableMB)
		}
		if got.Reason != nil {
			t.Fatalf("reason = %v, want nil", *got.Reason)
		}
	})

	t.Run("fractional MiB floors the total and ceils the used", func(t *testing.T) {
		pool := readyPool("data", StorageCapacityMIB+StorageCapacityMIB/2, 1)
		got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, nil, 0, nowMS, 120000, 1)
		if got.TotalMB != 1 {
			t.Fatalf("totalMB = %d, want 1 (floor of 1.5)", got.TotalMB)
		}
		if got.UsedMB != 1 {
			t.Fatalf("usedMB = %d, want 1 (ceil of ~0)", got.UsedMB)
		}
	})

	t.Run("zero bytes is unavailable, not full", func(t *testing.T) {
		pool := readyPool("data", 0, 0)
		got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, nil, 0, nowMS, 120000, 1)
		if got.TotalMB != 0 || got.Reason == nil || *got.Reason != StorageUnavailable {
			t.Fatalf("got %+v, want totalMB 0 and storage_unavailable", got)
		}
	})

	t.Run("negative bytes keep JS floor/ceil semantics", func(t *testing.T) {
		pool := readyPool("data", -StorageCapacityMIB, -3*StorageCapacityMIB/2)
		got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, nil, 0, nowMS, 120000, 1)
		if got.TotalMB != -1 {
			t.Fatalf("totalMB = %d, want -1 (Math.floor(-1))", got.TotalMB)
		}
		if got.UsedMB != -1 {
			t.Fatalf("usedMB = %d, want -1 (Math.ceil(-1.5))", got.UsedMB)
		}
		if got.Reason == nil || *got.Reason != StorageUnavailable {
			t.Fatalf("reason = %v, want storage_unavailable", got.Reason)
		}
	})

	t.Run("int64 max bytes lose precision exactly like Number(BigInt)", func(t *testing.T) {
		pool := readyPool("data", math.MaxInt64, 0)
		got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, nil, 0, nowMS, 120000, 1)
		if got.TotalMB != 8796093022208 {
			t.Fatalf("totalMB = %d, want 8796093022208", got.TotalMB)
		}
	})

	t.Run("2^53+1 bytes round to 2^53 like a JS double", func(t *testing.T) {
		pool := readyPool("data", 9007199254740993, 0)
		got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, nil, 0, nowMS, 120000, 1)
		if got.TotalMB != 8589934592 {
			t.Fatalf("totalMB = %d, want 8589934592", got.TotalMB)
		}
	})
}

func TestStorageCapacityChoiceReservations(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nowMS := now.UnixMilli()
	fresh := func() *time.Time { value := now.Add(-time.Second); return &value }
	const gib = int64(1024 * 1024 * 1024)
	pool := storageCapacityPoolRow{
		Name:       "data",
		Driver:     "dir",
		Status:     "ready",
		TotalBytes: 100 * gib,
		UsedBytes:  10 * gib,
		ReportedAt: fresh(),
		Telemetry:  []byte(`{"filesystemId":"fs-1"}`),
	}
	instances := []storageCapacityInstanceRow{{IncusID: "a", StoragePoolName: capacityStringPtr("data"), Disk: 2048}}

	// 102400 total - 10240 used - 1024 headroom = 91136 is the tightest bound.
	got := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, instances, 2048, nowMS, 120000, 30720)
	if got.AvailableMB != 91136 || got.AllocatedMB != 2048 || got.Reason != nil {
		t.Fatalf("got %+v, want available 91136 allocated 2048 and no reason", got)
	}

	// One byte more than the remaining space flips the reason to disk_full.
	full := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, instances, 2048, nowMS, 120000, 91137)
	if full.Reason == nil || *full.Reason != StorageDiskFull {
		t.Fatalf("reason = %v, want disk_full", full.Reason)
	}

	// requiredMB defaults to 1 when the option is absent.
	if reason := storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, instances, 2048, nowMS, 120000, 1); reason.Reason != nil {
		t.Fatalf("requiredMB 1 must not mark the pool full: %v", *reason.Reason)
	}

	// An instance without a root pool is charged to every candidate.
	unmapped := []storageCapacityInstanceRow{{IncusID: "b", StoragePoolName: nil, Disk: 4096}}
	got = storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, unmapped, 0, nowMS, 120000, 1)
	if got.AllocatedMB != 4096 {
		t.Fatalf("allocatedMB = %d, want 4096", got.AllocatedMB)
	}

	// host.diskUsed above the instance sum is reserved as unassigned space; it
	// only binds once it exceeds the gap to the used-bytes limit (91136).
	got = storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, instances, 2048+9000, nowMS, 120000, 1)
	if got.AvailableMB != 99328-9000 {
		t.Fatalf("availableMB = %d, want %d", got.AvailableMB, 99328-9000)
	}
	// ... but a host.diskUsed below the sum clamps to zero.
	got = storageCapacityChoice(pool, []storageCapacityPoolRow{pool}, instances, 0, nowMS, 120000, 1)
	if got.AvailableMB != 91136 {
		t.Fatalf("availableMB = %d, want 91136", got.AvailableMB)
	}

	// used 99 GiB: 102400 - 101376 - 1024 = 0
	nearlyFull := pool
	nearlyFull.UsedBytes = 99 * gib
	got = storageCapacityChoice(nearlyFull, []storageCapacityPoolRow{nearlyFull}, nil, 0, nowMS, 120000, 1)
	if got.AvailableMB != 0 || got.Reason == nil || *got.Reason != StorageDiskFull {
		t.Fatalf("got %+v, want available 0 and disk_full", got)
	}
}

func TestStorageCapacityChoiceGroupsAndBacking(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nowMS := now.UnixMilli()
	fresh := func() *time.Time { value := now.Add(-time.Second); return &value }
	const gib = int64(1024 * 1024 * 1024)

	// Two zfs datasets on the same pool: the group total is the largest member,
	// and instances in either dataset reserve from that total.
	poolA := storageCapacityPoolRow{
		Name: "zfs-a", Driver: "zfs", Status: "ready",
		Config: []byte(`{"zfs.pool_name":"tank/a"}`), TotalBytes: 100 * gib, ReportedAt: fresh(),
	}
	poolB := storageCapacityPoolRow{
		Name: "zfs-b", Driver: "zfs", Status: "ready",
		Config: []byte(`{"zfs.pool_name":"tank/b"}`), TotalBytes: 200 * gib, ReportedAt: fresh(),
	}
	instances := []storageCapacityInstanceRow{
		{IncusID: "a", StoragePoolName: capacityStringPtr("zfs-a"), Disk: 1024},
		{IncusID: "b", StoragePoolName: capacityStringPtr("zfs-b"), Disk: 2048},
	}
	got := storageCapacityChoice(poolA, []storageCapacityPoolRow{poolA, poolB}, instances, 3072, nowMS, 120000, 1)
	// allocatedMB only counts zfs-a; groupAllocated counts both. The binding
	// limits are 102400-1024-1024=100352, 102400-0-1024=101376 and
	// 204800-2048-3072=199680, so the pool's own total wins.
	if got.AllocatedMB != 1024 || got.AvailableMB != 100352 {
		t.Fatalf("got %+v", got)
	}

	// Sparse vdevs share a backing filesystem: the 4th limit caps the answer.
	sparse := storageCapacityPoolRow{
		Name: "sparse", Driver: "zfs", Status: "ready",
		Config:     []byte(`{"zfs.pool_name":"tank/sparse"}`),
		TotalBytes: 100 * gib,
		UsedBytes:  10 * gib,
		ReportedAt: fresh(),
		Telemetry:  []byte(`{"backing":{"availableBytes":53687091200,"id":"backing-1"}}`),
	}
	got = storageCapacityChoice(sparse, []storageCapacityPoolRow{sparse}, nil, 0, nowMS, 120000, 1)
	// min(99328, 91136, 99328, 51200-1024) = 50176
	if got.AvailableMB != 50176 {
		t.Fatalf("availableMB = %d, want 50176", got.AvailableMB)
	}

	// A second pool on the same backing filesystem shares the reserved bytes.
	sibling := sparse
	sibling.Name = "sparse-2"
	instances = []storageCapacityInstanceRow{
		{IncusID: "x", StoragePoolName: capacityStringPtr("sparse"), Disk: 1024},
		{IncusID: "y", StoragePoolName: capacityStringPtr("sparse-2"), Disk: 2048},
	}
	got = storageCapacityChoice(sparse, []storageCapacityPoolRow{sparse, sibling}, instances, 3072, nowMS, 120000, 1)
	// backing limit: 51200 - 1024 - (1024+2048) - 0 = 47104
	if got.AvailableMB != 47104 {
		t.Fatalf("availableMB = %d, want 47104", got.AvailableMB)
	}
}

// capacityStringPtr and Instances keep the fixture above readable.
func capacityStringPtr(value string) *string { return &value }

func TestStorageCapacityPoolNameJSON(t *testing.T) {
	// The wire shape is camelCase; routes return StorageCapacity unchanged.
	capacity := storageCapacityEmpty(StorageDiskFull)
	payload, err := json.Marshal(capacity)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"poolName":null,"totalMB":0,"usedMB":0,"allocatedMB":0,"availableMB":0,"headroomMB":0,"reason":"disk_full"}`
	if string(payload) != want {
		t.Fatalf("json = %s, want %s", payload, want)
	}
}

func TestStorageCapacityLocaleCompare(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"data", "data", 0},
		{"alpha", "beta", -1},
		{"beta", "alpha", 1},
		{"data", "Data", -1}, // ICU root puts the lowercase member first
		{"Data", "data", 1},
		{"data", "Data2", -1},
	}
	for _, tc := range cases {
		got := storageCapacityLocaleCompare(tc.left, tc.right)
		if (got < 0) != (tc.want < 0) || (got > 0) != (tc.want > 0) {
			t.Errorf("localeCompare(%q,%q) = %d, want sign %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestStorageCapacityHostServiceAvailable(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nowMS := now.UnixMilli()
	online := "online"
	seen := now.Add(-time.Second)

	base := storageCapacityHostRow{
		Status:      "online",
		AgentStatus: &online,
		AgentSeenAt: &seen,
		AgentReport: []byte(`{"incus":{"available":true},"instances":{"available":true},"metrics":{"heartbeatIntervalSeconds":30}}`),
	}
	originalTunnel := StorageCapacityTunnelIsOnline
	defer func() { StorageCapacityTunnelIsOnline = originalTunnel }()

	if !storageCapacityHostServiceAvailable(900001, base, nowMS) {
		t.Fatal("an online host with a fresh agent and available incus must be usable")
	}

	locked := base
	locked.NodeReport = []byte(`{"locked":true}`)
	if storageCapacityHostServiceAvailable(900001, locked, nowMS) {
		t.Fatal("a locked host must be unusable")
	}

	recovering := base
	recovering.NodeReport = []byte(`{"recoveryPending":true}`)
	if storageCapacityHostServiceAvailable(900001, recovering, nowMS) {
		t.Fatal("a host with a pending recovery must be unusable")
	}

	offline := base
	offline.Status = "offline"
	if storageCapacityHostServiceAvailable(900001, offline, nowMS) {
		t.Fatal("an offline host must be unusable")
	}

	maintenance := base
	maintenance.Status = "maintenance"
	if !storageCapacityHostServiceAvailable(900001, maintenance, nowMS) {
		t.Fatal("maintenance is an available state in evaluateHostService")
	}

	stale := base
	oldSeen := now.Add(-121 * time.Second)
	stale.AgentSeenAt = &oldSeen
	if storageCapacityHostServiceAvailable(900001, stale, nowMS) {
		t.Fatal("an agent older than max(120s, heartbeat*3) must be unusable")
	}

	offlineAgent := base
	agentOffline := "offline"
	offlineAgent.AgentStatus = &agentOffline
	if storageCapacityHostServiceAvailable(900001, offlineAgent, nowMS) {
		t.Fatal("an agent whose status is not online must be unusable")
	}

	incusDown := base
	incusDown.AgentReport = []byte(`{"incus":{"available":false},"instances":{"available":true}}`)
	if storageCapacityHostServiceAvailable(900001, incusDown, nowMS) {
		t.Fatal("a present agent must report incus.available === true")
	}

	instancesDown := base
	instancesDown.AgentReport = []byte(`{"incus":{"available":true},"instances":{"available":false}}`)
	if storageCapacityHostServiceAvailable(900001, instancesDown, nowMS) {
		t.Fatal("instances.available === false must make the host unusable")
	}

	noAgent := storageCapacityHostRow{Status: "online"}
	if !storageCapacityHostServiceAvailable(900001, noAgent, nowMS) {
		t.Fatal("a host without an agent row is not gated on agent reports")
	}

	// A tunnel-enabled host is offline until the tunnel seam reports online.
	StorageCapacityTunnelIsOnline = func(int32) bool { return false }
	tunneled := base
	tunneled.TunnelEnabled = true
	if storageCapacityHostServiceAvailable(900001, tunneled, nowMS) {
		t.Fatal("a tunnel-enabled host with no live tunnel must be unusable")
	}
	StorageCapacityTunnelIsOnline = func(int32) bool { return true }
	if !storageCapacityHostServiceAvailable(900001, tunneled, nowMS) {
		t.Fatal("a tunnel-enabled host with a live tunnel must be usable")
	}
}

// The db package reaches lib through Register* hooks (db ↛ lib). An unregistered
// resolver answers "not registered"; a registered one reaches the database layer
// and fails with "database not initialized" in a unit test.
func TestPackageStorageCapacityResolverIsRegistered(t *testing.T) {
	_, err := db.PackageStorageCapacityResolver(t.Context(), 1, nil, 1)
	if err == nil {
		t.Fatal("no database is configured in a unit test, so the resolver must fail")
	}
	if strings.Contains(err.Error(), "not registered") {
		t.Fatalf("resolver was not registered by init(): %v", err)
	}
}
