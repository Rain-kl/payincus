package db

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The sharing notice is a user-facing contract string. It is duplicated here
// because db must not import lib (lib imports db); this test pins the exact
// bytes so a drift from lib.GpuSharingNotice fails loudly.
func TestGpusSharingNotice(t *testing.T) {
	const want = "共享物理 GPU；显存和算力由同池实例共同使用，不提供独立显存或算力保证。"
	if GpusSharingNotice != want {
		t.Fatalf("GpusSharingNotice = %q, want %q", GpusSharingNotice, want)
	}
}

func TestGpusErrorCarriesTheTypeScriptMessage(t *testing.T) {
	err := &GpusError{Message: "套餐不存在"}
	if err.Error() != "套餐不存在" {
		t.Fatalf("Error() = %q", err.Error())
	}
	if _, ok := any(err).(error); !ok {
		t.Fatal("GpusError must implement error")
	}
}

func TestGpusIsFresh(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		available    bool
		runtimeReady bool
		lastSeen     time.Time
		want         bool
	}{
		{"exactly at the 180s bound", true, true, now.Add(-180 * time.Second), true},
		{"one second past the bound", true, true, now.Add(-181 * time.Second), false},
		{"not available", false, true, now, false},
		{"runtime not ready", true, false, now, false},
		{"29s in the future allowed", true, true, now.Add(29 * time.Second), true},
		{"exactly 30s in the future rejected (strict <)", true, true, now.Add(30 * time.Second), false},
		{"31s in the future rejected", true, true, now.Add(31 * time.Second), false},
	}
	for _, tc := range cases {
		if got := gpusIsFresh(tc.available, tc.runtimeReady, tc.lastSeen, now); got != tc.want {
			t.Errorf("%s: gpusIsFresh = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGpusResolvePolicy(t *testing.T) {
	pkg := GpusPolicy{GpuShared: true, GpuDeviceIDs: []int32{1, 2}}
	explicitFalse := false
	explicitTrue := true

	if got := gpusResolvePolicy(pkg, nil); got.GpuShared != true || len(got.GpuDeviceIDs) != 2 {
		t.Fatalf("nil plan must keep the package policy, got %+v", got)
	}
	if got := gpusResolvePolicy(pkg, &gpusPolicyPlanRow{}); got.GpuShared != true {
		t.Fatalf("null plan override must keep the package policy, got %+v", got)
	}
	nine := "[9]"
	if got := gpusResolvePolicy(pkg, &gpusPolicyPlanRow{GPUShared: &explicitFalse, GPUDeviceIDsJSON: &nine}); got.GpuShared != false || len(got.GpuDeviceIDs) != 1 || got.GpuDeviceIDs[0] != 9 {
		t.Fatalf("explicit false override must win, got %+v", got)
	}
	three := "[3]"
	if got := gpusResolvePolicy(GpusPolicy{GpuShared: false}, &gpusPolicyPlanRow{GPUShared: &explicitTrue, GPUDeviceIDsJSON: &three}); !got.GpuShared || got.GpuDeviceIDs[0] != 3 {
		t.Fatalf("explicit true override must win, got %+v", got)
	}
}

// The array column is fetched through array_to_json; a NULL/empty/garbage value must
// degrade to an empty selection exactly like Prisma's Int[] default.
func TestGpusJSONInt32Array(t *testing.T) {
	if got := gpusJSONInt32Array(nil); len(got) != 0 {
		t.Fatalf("nil = %v", got)
	}
	empty := ""
	if got := gpusJSONInt32Array(&empty); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	nullText := "null"
	if got := gpusJSONInt32Array(&nullText); len(got) != 0 {
		t.Fatalf("null json = %v", got)
	}
	values := "[2,1,1]"
	if got := gpusJSONInt32Array(&values); !equalIDs(got, []int32{2, 1, 1}) {
		t.Fatalf("values = %v", got)
	}
	garbage := "{1,2}"
	if got := gpusJSONInt32Array(&garbage); len(got) != 0 {
		t.Fatalf("garbage = %v", got)
	}
}

func TestGpusAllocatedIDs(t *testing.T) {
	primary := int32(7)
	if got := GpusAllocatedIDs(&primary, []int32{3, 7, 5, 3}); !equalIDs(got, []int32{3, 5, 7}) {
		t.Fatalf("GpusAllocatedIDs = %v, want [3 5 7]", got)
	}
	if got := GpusAllocatedIDs(nil, nil); len(got) != 0 {
		t.Fatalf("GpusAllocatedIDs(nil, nil) = %v, want empty", got)
	}
	if got := GpusAllocatedIDs(nil, []int32{9}); !equalIDs(got, []int32{9}) {
		t.Fatalf("GpusAllocatedIDs(nil, [9]) = %v", got)
	}
}

func equalIDs(left, right []int32) bool {
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

func TestGpusMeasurement(t *testing.T) {
	if value := gpusMeasurement(float64(42), 100); value == nil || *value != 42 {
		t.Fatalf("finite in-range value must pass, got %v", value)
	}
	if value := gpusMeasurement(float64(101), 100); value != nil {
		t.Fatalf("out-of-range value must be null, got %v", *value)
	}
	if value := gpusMeasurement(float64(-1), 1e9); value != nil {
		t.Fatalf("negative value must be null, got %v", *value)
	}
	for _, bad := range []any{"1", true, nil, json.Number("1"), math.Inf(1), math.NaN()} {
		if value := gpusMeasurement(bad, 100); value != nil {
			t.Fatalf("gpusMeasurement(%v) must be null, got %v", bad, *value)
		}
	}
}

func TestGpusJSTruthy(t *testing.T) {
	truthy := []any{true, float64(1), "x", []any{}, map[string]any{}}
	for _, value := range truthy {
		if !gpusJSTruthy(value) {
			t.Errorf("gpusJSTruthy(%v) = false, want true", value)
		}
	}
	falsy := []any{false, float64(0), "", nil}
	for _, value := range falsy {
		if gpusJSTruthy(value) {
			t.Errorf("gpusJSTruthy(%v) = true, want false", value)
		}
	}
}

func TestGpusSliceJSCountsUTF16Units(t *testing.T) {
	if got := gpusSliceJS("abcdef", 3); got != "abc" {
		t.Fatalf("ascii slice = %q", got)
	}
	// A supplementary-plane rune (emoji) is two UTF-16 units; a slice boundary in
	// the middle of the pair must not split the rune.
	if got := gpusSliceJS("a😀b", 2); got != "a" {
		t.Fatalf("emoji slice = %q, want %q", got, "a")
	}
	if got := gpusSliceJS("a😀b", 3); got != "a😀" {
		t.Fatalf("emoji slice = %q, want %q", got, "a😀")
	}
	if got := gpusSliceJS("abc", 0); got != "" {
		t.Fatalf("zero-length slice = %q", got)
	}
}

func TestGpusNormalizeReportCards(t *testing.T) {
	validCard := func(uuid, pci string, memory float64) map[string]any {
		return map[string]any{
			"uuid":           uuid,
			"pciAddress":     pci,
			"name":           "NVIDIA GeForce RTX 4090",
			"memoryTotalMiB": memory,
			"memoryUsedMiB":  float64(100),
			"gpuUtilization": float64(10),
			"computeMode":    "Default",
		}
	}

	report := map[string]any{
		"status": "ready",
		"cards": []any{
			validCard("GPU-0123456789abcdef", "0000:01:00.0", 24576),
			validCard("GPU-0123456789abcdef", "0000:02:00.0", 24576), // duplicate uuid
			validCard("GPU-fedcba9876543210", "0000:01:00.0", 24576), // duplicate pci
			validCard("GPU-aaaaaaaaaaaaaaaa", "0000:03:00.0", 0),     // falsy memory
			"not-a-card",
			map[string]any{"uuid": "nope", "pciAddress": "bad", "memoryTotalMiB": float64(1)},
		},
	}
	cards := gpusNormalizeReportCards(report)
	if len(cards) != 1 {
		t.Fatalf("gpusNormalizeReportCards kept %d cards, want 1", len(cards))
	}
	if cards[0].Name != "NVIDIA GeForce RTX 4090" || cards[0].MemoryTotalMiB != 24576 {
		t.Fatalf("unexpected card: %+v", cards[0])
	}
	if mode, _ := cards[0].Metrics["computeMode"].(string); mode != "Default" {
		t.Fatalf("computeMode = %v", cards[0].Metrics["computeMode"])
	}
	if value := cards[0].Metrics["temperature"]; value != nil {
		t.Fatalf("missing temperature must be null, got %v", value)
	}

	if cards := gpusNormalizeReportCards(map[string]any{"status": "error", "cards": []any{validCard("GPU-0123456789abcdef", "0000:01:00.0", 1)}}); cards != nil {
		t.Fatalf("non-ready reports carry no cards, got %d", len(cards))
	}
	if cards := gpusNormalizeReportCards(map[string]any{"status": "ready"}); cards != nil {
		t.Fatalf("missing cards array must yield nothing, got %d", len(cards))
	}

	// The default name applies when the report omits it.
	cards = gpusNormalizeReportCards(map[string]any{"status": "ready", "cards": []any{
		map[string]any{"uuid": "GPU-0123456789abcdef", "pciAddress": "0000:01:00.0", "memoryTotalMiB": float64(1)},
	}})
	if len(cards) != 1 || cards[0].Name != "NVIDIA GPU" {
		t.Fatalf("default name = %+v", cards)
	}

	// More than 64 cards are truncated before validation.
	many := make([]any, 0, 70)
	for index := 0; index < 70; index++ {
		uuid := "GPU-" + strings.Repeat("a", 16) + string(rune('a'+index%26))
		many = append(many, validCard(uuid, "0000:01:00.0", 1))
	}
	if cards := gpusNormalizeReportCards(map[string]any{"status": "ready", "cards": many}); len(cards) != 1 {
		// every card shares one PCI address, so exactly one survives de-duplication
		t.Fatalf("truncation/de-dupe = %d cards, want 1", len(cards))
	}
}

func TestFormatJSNumber(t *testing.T) {
	cases := map[float64]string{
		0:    "0",
		5:    "5",
		1.5:  "1.5",
		-2:   "-2",
		1000: "1000",
	}
	for value, want := range cases {
		if got := formatJSNumber(value); got != want {
			t.Errorf("formatJSNumber(%v) = %q, want %q", value, got, want)
		}
	}
}
