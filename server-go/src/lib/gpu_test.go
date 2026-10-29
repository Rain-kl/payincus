package lib

import (
	"testing"
	"time"
)

// TestNormalizeGpuInputTriState pins the gpuShared tri-state: absent,
// present-null (only when nullable) and present-bool are three distinct outcomes.
func TestNormalizeGpuInputTriState(t *testing.T) {
	shared, present, _, _, err := NormalizeGpuInput(map[string]any{}, false)
	if err != nil || present || shared != nil {
		t.Fatalf("absent field: shared=%v present=%v err=%v", shared, present, err)
	}

	shared, present, _, _, err = NormalizeGpuInput(map[string]any{"gpuShared": nil}, true)
	if err != nil || !present || shared != nil {
		t.Fatalf("nullable null: shared=%v present=%v err=%v", shared, present, err)
	}

	if _, _, _, _, err := NormalizeGpuInput(map[string]any{"gpuShared": nil}, false); err == nil {
		t.Fatal("non-nullable null must be rejected")
	}

	shared, present, _, _, err = NormalizeGpuInput(map[string]any{"gpuShared": false}, false)
	if err != nil || !present || shared == nil || *shared {
		t.Fatalf("explicit false: shared=%v present=%v err=%v", shared, present, err)
	}

	if _, _, _, _, err := NormalizeGpuInput(map[string]any{"gpuShared": "true"}, false); err == nil {
		t.Fatal("a string must not be coerced")
	}
}

// TestNormalizeGpuInputDeviceIDs pins the id validation and de-duplication.
func TestNormalizeGpuInputDeviceIDs(t *testing.T) {
	_, _, ids, present, err := NormalizeGpuInput(map[string]any{"gpuDeviceIDs": []any{float64(3), float64(1), float64(3)}}, false)
	if err != nil || !present {
		t.Fatalf("unexpected: ids=%v present=%v err=%v", ids, present, err)
	}
	if len(ids) != 2 || ids[0] != 3 || ids[1] != 1 {
		t.Fatalf("ids = %v, want [3 1] (deduplicated, order preserved)", ids)
	}

	bad := []any{
		[]any{float64(0)},
		[]any{float64(-1)},
		[]any{float64(2147483648)},
		[]any{float64(1.5)},
		[]any{"1"},
		// 1001 entries exceeds the 1000 cap.
		func() []any {
			values := make([]any, 1001)
			for index := range values {
				values[index] = float64(index + 1)
			}
			return values
		}(),
	}
	for _, input := range bad {
		if _, _, _, _, err := NormalizeGpuInput(map[string]any{"gpuDeviceIDs": input}, false); err == nil {
			t.Errorf("expected rejection for %v", input)
		}
	}
}

// TestResolveGpuPolicy pins that a plan override only wins when gpuShared is set.
func TestResolveGpuPolicy(t *testing.T) {
	pkg := GpuPolicy{GpuShared: true, GpuDeviceIDs: []int32{1}}

	sharedFalse := false
	override := ResolveGpuPolicy(pkg, &GpuPlanPolicy{GpuShared: &sharedFalse, GpuDeviceIDs: []int32{2}})
	if override.GpuShared || len(override.GpuDeviceIDs) != 1 || override.GpuDeviceIDs[0] != 2 {
		t.Fatalf("override = %#v", override)
	}

	inherited := ResolveGpuPolicy(pkg, &GpuPlanPolicy{GpuDeviceIDs: []int32{2}})
	if !inherited.GpuShared || inherited.GpuDeviceIDs[0] != 1 {
		t.Fatalf("inherited = %#v", inherited)
	}

	nilPlan := ResolveGpuPolicy(pkg, nil)
	if !nilPlan.GpuShared || nilPlan.GpuDeviceIDs[0] != 1 {
		t.Fatalf("nil plan = %#v", nilPlan)
	}
}

// TestIsGpuFresh pins the 180-second staleness window and the future-timestamp guard.
func TestIsGpuFresh(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	if !IsGpuFresh(true, true, now.Add(-179*time.Second), now) {
		t.Fatal("179s old must be fresh")
	}
	if IsGpuFresh(true, true, now.Add(-181*time.Second), now) {
		t.Fatal("181s old must be stale")
	}
	if IsGpuFresh(false, true, now, now) {
		t.Fatal("unavailable must be stale")
	}
	if IsGpuFresh(true, false, now, now) {
		t.Fatal("runtime not ready must be stale")
	}
	if IsGpuFresh(true, true, now.Add(31*time.Second), now) {
		t.Fatal("more than 30s in the future must be rejected")
	}
	if !IsGpuFresh(true, true, now.Add(29*time.Second), now) {
		t.Fatal("29s in the future is tolerated clock skew")
	}
}

// TestSummarizeGpuModels pins grouping and ordering.
func TestSummarizeGpuModels(t *testing.T) {
	devices := []GpuDevice{
		{Name: "Tesla T4", MemoryTotalMiB: 15360},
		{Name: "A100", MemoryTotalMiB: 40960},
		{Name: "Tesla T4", MemoryTotalMiB: 15360},
		{Name: "A100", MemoryTotalMiB: 20480},
	}

	summary := SummarizeGpuModels(devices)
	if len(summary) != 3 {
		t.Fatalf("got %d groups, want 3: %#v", len(summary), summary)
	}
	if summary[0].Name != "A100" || summary[0].MemoryTotalMiB != 20480 || summary[0].Count != 1 {
		t.Fatalf("first = %#v", summary[0])
	}
	if summary[1].Name != "A100" || summary[1].MemoryTotalMiB != 40960 || summary[1].Count != 1 {
		t.Fatalf("second = %#v", summary[1])
	}
	if summary[2].Name != "Tesla T4" || summary[2].Count != 2 {
		t.Fatalf("third = %#v", summary[2])
	}
}

// TestSummarizeGpuConfigurationsGroupsEqualBundles pins that identical bundles on
// different hosts collapse into one entry listing both hosts.
func TestSummarizeGpuConfigurationsGroupsEqualBundles(t *testing.T) {
	devices := []GpuHostDevice{
		{HostID: 1, GpuDevice: GpuDevice{Name: "A100", MemoryTotalMiB: 40960}},
		{HostID: 1, GpuDevice: GpuDevice{Name: "A100", MemoryTotalMiB: 40960}},
		{HostID: 2, GpuDevice: GpuDevice{Name: "A100", MemoryTotalMiB: 40960}},
		{HostID: 2, GpuDevice: GpuDevice{Name: "A100", MemoryTotalMiB: 40960}},
	}

	configurations := SummarizeGpuConfigurations(devices)
	if len(configurations) != 1 {
		t.Fatalf("got %d configurations, want 1: %#v", len(configurations), configurations)
	}
	only := configurations[0]
	if len(only.HostIDs) != 2 || only.HostIDs[0] != 1 || only.HostIDs[1] != 2 {
		t.Fatalf("hostIds = %v", only.HostIDs)
	}
	if only.Count != 2 {
		t.Fatalf("count = %d, want 2 (cards per host, not total)", only.Count)
	}
}

// TestApplyGpuDeviceRejectsPrivilegedAndVM pins the isolation rules.
func TestApplyGpuDeviceRejectsPrivilegedAndVM(t *testing.T) {
	validPCI := "0000:00:1e.0"

	vmConfig := map[string]any{"type": "virtual-machine"}
	if err := ApplyGpuDevice(vmConfig, validPCI); err == nil {
		t.Fatal("VMs must be rejected")
	}

	privileged := map[string]any{
		"config": map[string]any{"security.privileged": "true"},
	}
	if err := ApplyGpuDevice(privileged, validPCI); err == nil {
		t.Fatal("privileged containers must be rejected")
	}
}

// TestApplyGpuDeviceValidatesPCI pins the address format and duplicate rejection.
func TestApplyGpuDeviceValidatesPCI(t *testing.T) {
	base := func() map[string]any { return map[string]any{"config": map[string]any{}} }

	for _, bad := range []any{
		"",
		"00:1e.0",
		"0000:00:1e.8",
		"0000:00:1E.0",
		[]any{},
		[]string{"0000:00:1e.0", "0000:00:1e.0"},
	} {
		if err := ApplyGpuDevice(base(), bad); err == nil {
			t.Errorf("expected rejection for %#v", bad)
		}
	}
}

// TestApplyGpuDeviceWritesDevices pins the exact mutation: profiles cleared,
// nvidia runtime flags set, old gpu devices replaced.
func TestApplyGpuDeviceWritesDevices(t *testing.T) {
	config := map[string]any{
		"config": map[string]any{"security.privileged": "false"},
		"devices": map[string]any{
			"existing-gpu": map[string]any{"type": "gpu", "pci": "0000:00:1f.0"},
			"root":         map[string]any{"type": "disk", "path": "/"},
		},
		"profiles": []any{"default"},
	}

	if err := ApplyGpuDevice(config, []string{"0000:00:1e.0", "0000:00:1f.1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	profiles, _ := config["profiles"].([]any)
	if len(profiles) != 0 {
		t.Fatalf("profiles must be cleared, got %v", profiles)
	}

	settings := config["config"].(map[string]any)
	if settings["nvidia.runtime"] != "true" {
		t.Fatalf("nvidia.runtime = %v", settings["nvidia.runtime"])
	}
	if settings["nvidia.driver.capabilities"] != "compute,utility" {
		t.Fatalf("nvidia.driver.capabilities = %v", settings["nvidia.driver.capabilities"])
	}

	devices := config["devices"].(map[string]any)
	if _, stale := devices["existing-gpu"]; stale {
		t.Fatal("the pre-existing gpu device must be removed")
	}
	if _, kept := devices["root"]; !kept {
		t.Fatal("non-gpu devices must survive")
	}
	gpu0, _ := devices["gpu0"].(map[string]any)
	if gpu0["pci"] != "0000:00:1e.0" || gpu0["gputype"] != "physical" || gpu0["type"] != "gpu" {
		t.Fatalf("gpu0 = %#v", gpu0)
	}
	gpu1, _ := devices["gpu1"].(map[string]any)
	if gpu1["pci"] != "0000:00:1f.1" {
		t.Fatalf("gpu1 = %#v", gpu1)
	}
}

func TestGpuErrorShape(t *testing.T) {
	err := &GpuError{Message: "x"}
	if err.StatusCode() != 400 {
		t.Fatalf("StatusCode = %d", err.StatusCode())
	}
	if err.Code() != "GPU_CONFIGURATION_INVALID" {
		t.Fatalf("Code = %q", err.Code())
	}
	if err.Error() != "x" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
