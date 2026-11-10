// Unit tests for the two agent telemetry services. Everything that needs rows is
// covered by the dual-run conformance harness; what can be proven without a
// database is the normalisation layer, which is where the TypeScript's security
// decisions (allow-listed statuses, host-scoped rows, counters) live.
package services

import (
	"testing"
	"time"
	"unicode/utf16"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== instance report normalisation ====================

func TestAgentNormalizeStatus(t *testing.T) {
	cases := []struct {
		value any
		want  string
		ok    bool
	}{
		{"running", "running", true},
		{"stopped", "stopped", true},
		{"error", "error", true},
		{"Running", "running", true},   // mapInstanceStatus
		{"Stopped", "stopped", true},   // mapInstanceStatus
		{"Frozen", "stopped", true},    // mapInstanceStatus
		{"Starting", "unknown", false}, // creating is not an acceptable report
		{"bogus", "unknown", false},
		{"  running  ", "running", true},
		{"", "", false},
		{float64(1), "", false},
	}
	for _, testCase := range cases {
		got := agentNormalizeStatus(testCase.value)
		if !testCase.ok {
			if got != nil {
				t.Errorf("normalizeStatus(%v) = %v, want nil", testCase.value, *got)
			}
			continue
		}
		if got == nil || string(*got) != testCase.want {
			t.Errorf("normalizeStatus(%v) = %v, want %s", testCase.value, got, testCase.want)
		}
	}
}

func TestAgentParseCounter(t *testing.T) {
	cases := []struct {
		value any
		want  *int64
	}{
		{float64(0), int64Ptr(0)},
		{float64(1024), int64Ptr(1024)},
		{"2048", int64Ptr(2048)},
		{" 4096 ", int64Ptr(4096)},
		{float64(-1), nil},
		{float64(1.5), nil},
		{"-5", nil},
		{"abc", nil},
		{"1e3", nil},
		{nil, nil},
		// Larger than int64: dropped rather than erroring the whole report.
		{"999999999999999999999999999999", nil},
	}
	for _, testCase := range cases {
		got := agentParseCounter(testCase.value)
		if (got == nil) != (testCase.want == nil) {
			t.Errorf("parseCounter(%v) = %v, want %v", testCase.value, got, testCase.want)
			continue
		}
		if got != nil && testCase.want != nil && *got != *testCase.want {
			t.Errorf("parseCounter(%v) = %d, want %d", testCase.value, *got, *testCase.want)
		}
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestAgentNormalizeInstanceItems(t *testing.T) {
	payload := map[string]any{
		"items": []any{
			map[string]any{
				"name":            "  web-01  ",
				"status":          "Running",
				"storagePoolName": "tank",
				"traffic":         map[string]any{"rxBytes": float64(10), "txBytes": "20"},
				"network":         map[string]any{"ipv4": "10.0.0.2", "ipv6": "2001:db8::2"},
			},
			map[string]any{
				"name":    "db-01",
				"status":  "Stopped",
				"network": map[string]any{"ipv4": nil},
			},
			map[string]any{"name": ""},
			map[string]any{"name": "ignored-bad-pool", "storagePoolName": "-bad"},
			"not-an-object",
		},
	}
	items := agentNormalizeInstanceItems(payload)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (%+v)", len(items), items)
	}
	first := items[0]
	if first.Name != "web-01" {
		t.Fatalf("name = %q", first.Name)
	}
	if first.Status == nil || *first.Status != models.InstanceStatusRunning {
		t.Fatalf("status = %v", first.Status)
	}
	if first.RxBytes == nil || *first.RxBytes != 10 || first.TxBytes == nil || *first.TxBytes != 20 {
		t.Fatalf("counters = %v/%v", first.RxBytes, first.TxBytes)
	}
	if !first.IPv4Set || first.IPv4 == nil || *first.IPv4 != "10.0.0.2" {
		t.Fatalf("ipv4 = %v (set=%v)", first.IPv4, first.IPv4Set)
	}
	if !first.IPv6Set || first.IPv6 == nil || *first.IPv6 != "2001:db8::2" {
		t.Fatalf("ipv6 = %v (set=%v)", first.IPv6, first.IPv6Set)
	}
	if first.StoragePoolName == nil || *first.StoragePoolName != "tank" {
		t.Fatalf("storage pool = %v", first.StoragePoolName)
	}
	second := items[1]
	if !second.IPv4Set || second.IPv4 != nil {
		t.Fatalf("explicit null ipv4 must set the column to NULL: %v (set=%v)", second.IPv4, second.IPv4Set)
	}
	if second.IPv6Set {
		t.Fatal("missing ipv6 must not touch the column")
	}
	if items[2].StoragePoolName != nil {
		t.Fatalf("invalid storage pool accepted: %v", items[2].StoragePoolName)
	}

	if got := agentNormalizeInstanceItems(nil); len(got) != 0 {
		t.Fatalf("nil payload items = %v", got)
	}
	if got := agentNormalizeInstanceItems(map[string]any{"items": "nope"}); len(got) != 0 {
		t.Fatalf("non-array items = %v", got)
	}
}

func TestAgentInstanceReportItemCap(t *testing.T) {
	rawItems := make([]any, 0, AgentInstanceReportMaxItems+10)
	for index := 0; index < AgentInstanceReportMaxItems+10; index++ {
		rawItems = append(rawItems, map[string]any{"name": "instance-" + intToString(index)})
	}
	items := agentNormalizeInstanceItems(map[string]any{"items": rawItems})
	if len(items) != AgentInstanceReportMaxItems {
		t.Fatalf("items = %d, want cap %d", len(items), AgentInstanceReportMaxItems)
	}
}

func intToString(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestAgentNetworkAddressNormalization(t *testing.T) {
	cases := []struct {
		value  any
		family int
		want   string
		keep   bool
	}{
		{"10.0.0.2", 4, "10.0.0.2", true},
		{" 10.0.0.2 ", 4, "10.0.0.2", true},
		{"2001:db8::1", 4, "", false}, // wrong family: not reported
		{"2001:db8::1", 6, "2001:db8::1", true},
		{"10.0.0.2", 6, "", false},
		{"", 4, "", true},  // empty string means NULL
		{nil, 4, "", true}, // explicit null means NULL
		{float64(1), 4, "", false},
		{"fe80::1%eth0", 6, "", false}, // zone ids are not IP literals for isIP
	}
	for _, testCase := range cases {
		got, keep := agentNormalizeNetworkAddress(testCase.value, testCase.family)
		if keep != testCase.keep || got != testCase.want {
			t.Errorf("normalizeNetworkAddress(%v,%d) = (%q,%v), want (%q,%v)",
				testCase.value, testCase.family, got, keep, testCase.want, testCase.keep)
		}
	}
}

func TestAgentShouldSyncStatusAndTraffic(t *testing.T) {
	running := models.InstanceStatusRunning
	stopped := models.InstanceStatusStopped
	errorStatus := models.InstanceStatusError
	if !agentShouldSyncStatus(running, &stopped) {
		t.Fatal("running row with a reported status must sync")
	}
	if !agentShouldSyncStatus(stopped, &errorStatus) {
		t.Fatal("stopped row with a reported status must sync")
	}
	if agentShouldSyncStatus(models.InstanceStatusCreating, &running) {
		t.Fatal("creating row must never be synced by an agent report")
	}
	if agentShouldSyncStatus(running, nil) {
		t.Fatal("missing reported status must not sync")
	}

	rx, tx := int64(1), int64(2)
	item := agentInstanceItem{Status: &running, RxBytes: &rx, TxBytes: &tx}
	if !agentShouldApplyTraffic(running, item) {
		t.Fatal("running/running with counters must apply traffic")
	}
	if agentShouldApplyTraffic(stopped, item) {
		t.Fatal("stopped row must not accrue traffic")
	}
	item.Status = &stopped
	if agentShouldApplyTraffic(running, item) {
		t.Fatal("non-running report must not accrue traffic")
	}
	item.Status = &running
	item.RxBytes = nil
	if agentShouldApplyTraffic(running, item) {
		t.Fatal("missing counters must not accrue traffic")
	}
}

func TestAgentNormalizeReportSampledAt(t *testing.T) {
	receivedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if got := agentNormalizeReportSampledAt(map[string]any{"reportedAt": "2026-11-11T11:59:00.000Z"}, receivedAt); !got.Equal(receivedAt.Add(-time.Minute)) {
		t.Fatalf("reportedAt = %v", got)
	}
	if got := agentNormalizeReportSampledAt(map[string]any{"reportedAt": "not-a-date"}, receivedAt); !got.Equal(receivedAt) {
		t.Fatalf("invalid reportedAt = %v, want receivedAt", got)
	}
	if got := agentNormalizeReportSampledAt(map[string]any{}, receivedAt); !got.Equal(receivedAt) {
		t.Fatalf("missing reportedAt = %v", got)
	}
	if got := agentNormalizeReportSampledAt("nope", receivedAt); !got.Equal(receivedAt) {
		t.Fatalf("non-object payload = %v", got)
	}
}

func TestAgentBuildStatusUpdateData(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	running := models.InstanceStatusRunning
	ipv4 := "10.0.0.9"
	update := agentBuildStatusUpdateData(agentInstanceItem{
		Status: &running, IPv4: &ipv4, IPv4Set: true,
	}, now)
	if update == nil || update.Status != running || !update.LastSynced.Equal(now) || !update.IPv4Present || *update.IPv4 != ipv4 {
		t.Fatalf("update = %+v", update)
	}
	if agentBuildStatusUpdateData(agentInstanceItem{}, now) != nil {
		t.Fatal("item without a status must not produce an update")
	}
}

// ==================== storage report normalisation ====================

func TestAgentStorageIsBytes(t *testing.T) {
	cases := []struct {
		value any
		want  int64
		ok    bool
	}{
		{float64(0), 0, true},
		{float64(1024), 1024, true},
		{float64(-1), 0, false},
		{float64(1.5), 0, false},
		{"1024", 0, false}, // storage bytes are numbers, not strings
		{nil, 0, false},
	}
	for _, testCase := range cases {
		got, ok := agentStorageIsBytes(testCase.value)
		if ok != testCase.ok || got != testCase.want {
			t.Errorf("bytes(%v) = (%d,%v), want (%d,%v)", testCase.value, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestAgentStorageJSTruthy(t *testing.T) {
	truthy := []any{true, "x", float64(1), map[string]any{}, []any{}}
	falsy := []any{false, "", float64(0), nil}
	for _, value := range truthy {
		if !agentStorageJSTruthy(value) {
			t.Errorf("truthy(%v) = false", value)
		}
	}
	for _, value := range falsy {
		if agentStorageJSTruthy(value) {
			t.Errorf("truthy(%v) = true", value)
		}
	}
}

func TestAgentStorageParseJSDate(t *testing.T) {
	parsed, ok := agentStorageParseJSDate("2026-11-11T12:00:00.000Z")
	if !ok || !parsed.Equal(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("parse = %v ok=%v", parsed, ok)
	}
	parsed, ok = agentStorageParseJSDate("2026-11-11T20:00:00+08:00")
	if !ok || !parsed.Equal(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("offset parse = %v ok=%v", parsed, ok)
	}
	if _, ok := agentStorageParseJSDate("garbage"); ok {
		t.Fatal("garbage date accepted")
	}
}

func TestAgentStorageSlice(t *testing.T) {
	if got := agentStorageSlice("abcdef", 3); got != "abc" {
		t.Fatalf("slice = %q", got)
	}
	if got := agentStorageSlice("abc", 10); got != "abc" {
		t.Fatalf("short slice = %q", got)
	}
	// An astral character counts as two UTF-16 units and is never split.
	emoji := "a" + "\U0001F600" + "b"
	if got := agentStorageSlice(emoji, 2); got != "a" {
		t.Fatalf("emoji slice = %q (must not split the surrogate pair)", got)
	}
	if len(utf16.Encode([]rune(emoji))) != 4 {
		t.Fatal("test assumption broken: emoji is two UTF-16 units")
	}
}

func TestAgentStorageJSNumber(t *testing.T) {
	cases := map[float64]string{
		0:      "0",
		1:      "1",
		1.5:    "1.5",
		1000:   "1000",
		1e21:   "1e+21",
		1e-7:   "1e-7",
		0.0001: "0.0001",
	}
	for value, want := range cases {
		if got := agentStorageJSNumber(value); got != want {
			t.Errorf("number(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestAgentStorageIsRecord(t *testing.T) {
	if _, ok := agentStorageIsRecord(map[string]any{}); !ok {
		t.Fatal("empty object rejected")
	}
	for _, value := range []any{nil, "x", []any{}, float64(1), true} {
		if _, ok := agentStorageIsRecord(value); ok {
			t.Fatalf("%v accepted as a record", value)
		}
	}
}
