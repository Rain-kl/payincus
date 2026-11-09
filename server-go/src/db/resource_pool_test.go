package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== storage headroom / report age ====================

// TestResourcePoolStorageHeadroomMB pins storageHeadroomMB:
// min(10240, max(1024, ceil(totalMB / 100))).
func TestResourcePoolStorageHeadroomMB(t *testing.T) {
	cases := map[int64]int64{
		0:         1024,
		100:       1024,
		102_400:   1024, // ceil(1024) = 1024
		102_401:   1025,
		500_000:   5000,
		1_000_000: 10000,
		2_000_000: 10240,
	}
	for totalMB, want := range cases {
		if got := resourcePoolStorageHeadroomMB(totalMB); got != want {
			t.Fatalf("%d: got %d, want %d", totalMB, got, want)
		}
	}
}

// TestResourcePoolStorageReportMaxAgeMs pins storageReportMaxAgeMs:
// max(120s, clamp(heartbeatIntervalSeconds, 5, 3600) * 3000).
func TestResourcePoolStorageReportMaxAgeMs(t *testing.T) {
	cases := []struct {
		report []byte
		want   time.Duration
	}{
		{nil, 120 * time.Second},
		{[]byte(`{}`), 120 * time.Second},
		{[]byte(`{"metrics":{"heartbeatIntervalSeconds":10}}`), 120 * time.Second}, // 30s → floor
		{[]byte(`{"metrics":{"heartbeatIntervalSeconds":100}}`), 300 * time.Second},
		{[]byte(`{"metrics":{"heartbeatIntervalSeconds":3600}}`), 3 * time.Hour},
		{[]byte(`{"metrics":{"heartbeatIntervalSeconds":2}}`), 120 * time.Second},    // below 5 → 5
		{[]byte(`{"metrics":{"heartbeatIntervalSeconds":7200}}`), 120 * time.Second}, // above 3600 → 5
		{[]byte(`{"metrics":{"heartbeatIntervalSeconds":"nope"}}`), 120 * time.Second},
		{[]byte(`not json`), 120 * time.Second},
	}
	for _, tc := range cases {
		if got := resourcePoolStorageReportMaxAgeMs(tc.report); got != tc.want {
			t.Fatalf("%s: got %v, want %v", string(tc.report), got, tc.want)
		}
	}
}

// ==================== pool grouping ====================

// TestResourcePoolStorageGroup pins group(pool) for every driver family.
func TestResourcePoolStorageGroup(t *testing.T) {
	cases := []struct {
		name string
		pool resourcePoolStoragePoolRow
		want string
	}{
		{"dir with filesystem id", resourcePoolStoragePoolRow{
			Name: "default", Driver: "dir",
			Telemetry: []byte(`{"filesystemId":"fs-1"}`),
		}, "filesystem:fs-1"},
		{"btrfs without telemetry", resourcePoolStoragePoolRow{
			Name: "default", Driver: "btrfs",
		}, "filesystem:unknown"},
		{"zfs pool name", resourcePoolStoragePoolRow{
			Name: "default", Driver: "zfs",
			Config: []byte(`{"zfs.pool_name":"tank/instances"}`),
		}, "zfs:tank"},
		{"zfs source", resourcePoolStoragePoolRow{
			Name: "default", Driver: "zfs",
			Config: []byte(`{"source":"pool1/dataset"}`),
		}, "zfs:pool1"},
		{"zfs fallback", resourcePoolStoragePoolRow{Name: "tank", Driver: "zfs"}, "zfs:tank"},
		{"lvm vg", resourcePoolStoragePoolRow{
			Name: "default", Driver: "lvm",
			Config: []byte(`{"lvm.vg_name":"vg0"}`),
		}, "lvm:vg0"},
		{"lvm source", resourcePoolStoragePoolRow{
			Name: "default", Driver: "lvm",
			Config: []byte(`{"source":"/dev/sdb"}`),
		}, "lvm:/dev/sdb"},
		{"lvm fallback", resourcePoolStoragePoolRow{Name: "pool2", Driver: "lvm"}, "lvm:pool2"},
		{"other driver", resourcePoolStoragePoolRow{Name: "ceph", Driver: "ceph"}, "ceph:ceph"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourcePoolStorageGroup(tc.pool); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ==================== pool readiness ====================

// TestResourcePoolStoragePoolReady pins the `ready(pool)` predicate, including the
// JS truthiness of _payincusImport/_payincusDeletion.
func TestResourcePoolStoragePoolReady(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute)
	old := now.Add(-10 * time.Minute)
	maxAge := 5 * time.Minute

	ready := func(pool resourcePoolStoragePoolRow) bool {
		return resourcePoolStoragePoolReady(pool, now, maxAge)
	}

	if !ready(resourcePoolStoragePoolRow{Status: "ready", ReportedAt: &recent, TotalBytes: 1}) {
		t.Fatal("fresh ready pool must pass")
	}
	if ready(resourcePoolStoragePoolRow{Status: "pending", ReportedAt: &recent, TotalBytes: 1}) {
		t.Fatal("non-ready status must fail")
	}
	if ready(resourcePoolStoragePoolRow{Status: "ready", ReportedAt: nil, TotalBytes: 1}) {
		t.Fatal("missing reportedAt must fail")
	}
	if ready(resourcePoolStoragePoolRow{Status: "ready", ReportedAt: &recent, TotalBytes: 0}) {
		t.Fatal("zero totalBytes must fail")
	}
	if ready(resourcePoolStoragePoolRow{Status: "ready", ReportedAt: &old, TotalBytes: 1}) {
		t.Fatal("stale report must fail")
	}
	if ready(resourcePoolStoragePoolRow{
		Status: "ready", ReportedAt: &recent, TotalBytes: 1,
		Config: []byte(`{"_payincusImport":true}`),
	}) {
		t.Fatal("import marker must fail")
	}
	if ready(resourcePoolStoragePoolRow{
		Status: "ready", ReportedAt: &recent, TotalBytes: 1,
		Config: []byte(`{"_payincusDeletion":"yes"}`),
	}) {
		t.Fatal("deletion marker must fail")
	}
	// JS truthiness: an explicit false does not disqualify the pool.
	if !ready(resourcePoolStoragePoolRow{
		Status: "ready", ReportedAt: &recent, TotalBytes: 1,
		Config: []byte(`{"_payincusImport":false,"_payincusDeletion":false}`),
	}) {
		t.Fatal("falsy markers must pass")
	}
}

// ==================== host service availability ====================

// TestResourcePoolHostServiceAvailable pins the DB-derivable half of
// evaluateHostService(host).available.
func TestResourcePoolHostServiceAvailable(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-10 * time.Second)
	staleSeen := now.Add(-10 * time.Minute)
	online := "online"
	offlineAgent := "offline"

	base := resourcePoolHostStateRow{Status: "online"}

	if !resourcePoolHostServiceAvailable(base, now) {
		t.Fatal("online host without agent must be available")
	}
	if !resourcePoolHostServiceAvailable(resourcePoolHostStateRow{Status: "maintenance"}, now) {
		t.Fatal("maintenance host must be available")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{Status: "offline"}, now) {
		t.Fatal("offline host must be unavailable")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", NodeReport: []byte(`{"locked":true}`),
	}, now) {
		t.Fatal("locked node must be unavailable")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", NodeReport: []byte(`{"recoveryPending":true}`),
	}, now) {
		t.Fatal("recoveryPending node must be unavailable")
	}
	if !resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", NodeReport: []byte(`{"locked":false,"recoveryPending":false}`),
	}, now) {
		t.Fatal("explicit false flags must stay available")
	}

	// A present agent must positively report incus availability.
	agentNoReport := resourcePoolHostStateRow{Status: "online", AgentStatus: &online, AgentSeenAt: &seen}
	if resourcePoolHostServiceAvailable(agentNoReport, now) {
		t.Fatal("agent without incus report must be unavailable")
	}
	agentAvailable := resourcePoolHostStateRow{
		Status: "online", AgentStatus: &online, AgentSeenAt: &seen,
		AgentReport: []byte(`{"incus":{"available":true},"instances":{"available":true}}`),
	}
	if !resourcePoolHostServiceAvailable(agentAvailable, now) {
		t.Fatal("agent reporting availability must pass")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", AgentStatus: &online, AgentSeenAt: &seen,
		AgentReport: []byte(`{"incus":{"available":false}}`),
	}, now) {
		t.Fatal("incus unavailable must fail")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", AgentStatus: &online, AgentSeenAt: &seen,
		AgentReport: []byte(`{"incus":{"available":true},"instances":{"available":false}}`),
	}, now) {
		t.Fatal("instances unavailable must fail")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", AgentStatus: &offlineAgent, AgentSeenAt: &seen,
		AgentReport: []byte(`{"incus":{"available":true}}`),
	}, now) {
		t.Fatal("agent status not online must fail")
	}
	if resourcePoolHostServiceAvailable(resourcePoolHostStateRow{
		Status: "online", AgentStatus: &online, AgentSeenAt: &staleSeen,
		AgentReport: []byte(`{"incus":{"available":true}}`),
	}, now) {
		t.Fatal("stale agent must fail")
	}
	// A 600s heartbeat widens the staleness window to 1800s.
	wideInterval := resourcePoolHostStateRow{
		Status: "online", AgentStatus: &online, AgentSeenAt: &staleSeen,
		AgentReport: []byte(`{"incus":{"available":true},"metrics":{"heartbeatIntervalSeconds":600}}`),
	}
	if !resourcePoolHostServiceAvailable(wideInterval, now) {
		t.Fatal("stale window must follow the reported heartbeat interval")
	}
}

// TestResourcePoolStorageBackingOf pins the sparse-vdev telemetry projection.
func TestResourcePoolStorageBackingOf(t *testing.T) {
	if got := resourcePoolStorageBackingOf(nil); got != nil {
		t.Fatalf("nil telemetry: %v", got)
	}
	if got := resourcePoolStorageBackingOf([]byte(`{}`)); got != nil {
		t.Fatalf("no backing: %v", got)
	}
	got := resourcePoolStorageBackingOf([]byte(`{"backing":{"availableBytes":123456789,"id":"b1"}}`))
	if got == nil || got.ID != "b1" || got.AvailableBytes != 123456789 {
		t.Fatalf("backing: %+v", got)
	}
}

// TestResourcePoolJSONFalsy pins JavaScript truthiness for decoded JSON values.
func TestResourcePoolJSONFalsy(t *testing.T) {
	falsy := []any{nil, false, float64(0), ""}
	for _, value := range falsy {
		if !resourcePoolJSONFalsy(value) {
			t.Fatalf("%v should be falsy", value)
		}
	}
	truthy := []any{true, float64(1), "x", map[string]any{}}
	for _, value := range truthy {
		if resourcePoolJSONFalsy(value) {
			t.Fatalf("%v should be truthy", value)
		}
	}
}

// ==================== pending remark / field mapping ====================

// TestResourcePoolPendingRemark pins the "[incus-pending] " stamping rule.
func TestResourcePoolPendingRemark(t *testing.T) {
	remark := "应用到实例 demo"
	if got := resourcePoolPendingRemark("c", &remark); got != ResourcePoolIncusPendingPrefix+remark {
		t.Fatalf("cpu: %q", got)
	}
	if got := resourcePoolPendingRemark("r", nil); got != ResourcePoolIncusPendingPrefix {
		t.Fatalf("nil remark: %q", got)
	}
	if got := resourcePoolPendingRemark("t", &remark); got != remark {
		t.Fatalf("traffic must not be stamped: %q", got)
	}
}

// TestResourcePoolField pins the resource-type -> column mapping.
func TestResourcePoolField(t *testing.T) {
	cases := map[string]string{"c": "cpu", "r": "memory", "d": "disk", "t": "traffic"}
	for resourceType, want := range cases {
		got, ok := resourcePoolField(models.RedeemCodeType(resourceType))
		if !ok || got != want {
			t.Fatalf("%s: got (%q,%v)", resourceType, got, ok)
		}
	}
	if _, ok := resourcePoolField(models.RedeemCodeType("p")); ok {
		t.Fatal("points must not map to a pool column")
	}
	if got := resourcePoolInvalidTypeError(models.RedeemCodeType("p")).Error(); got != "Invalid resource type: p" {
		t.Fatalf("invalid type message: %q", got)
	}
}

// TestResourcePoolStripPendingPrefix pins the log remark normalization.
func TestResourcePoolStripPendingPrefix(t *testing.T) {
	if got := resourcePoolStripPendingPrefix(nil); got != nil {
		t.Fatalf("nil: %v", got)
	}
	plain := "hello"
	if got := resourcePoolStripPendingPrefix(&plain); got == nil || *got != plain {
		t.Fatalf("plain: %v", got)
	}
	stamped := ResourcePoolIncusPendingPrefix + "应用到实例 demo"
	if got := resourcePoolStripPendingPrefix(&stamped); got == nil || *got != "应用到实例 demo" {
		t.Fatalf("stamped: %v", got)
	}
	onlyPrefix := ResourcePoolIncusPendingPrefix
	if got := resourcePoolStripPendingPrefix(&onlyPrefix); got == nil || *got != "" {
		t.Fatalf("prefix only: %v", got)
	}
}

// TestResourcePoolBalancesShape pins the string-typed JSON of GET /api/resource-pool.
func TestResourcePoolBalancesShape(t *testing.T) {
	encoded, err := json.Marshal(ResourcePoolBalances{CPU: "1024", Memory: "2048", Disk: "0", Traffic: "10"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cpu":"1024","memory":"2048","disk":"0","traffic":"10"}`
	if string(encoded) != want {
		t.Fatalf("got %s, want %s", encoded, want)
	}
}
