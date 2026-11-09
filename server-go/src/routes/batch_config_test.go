package routes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// batchConfigDecodeBody runs a raw JSON document through the shared Fastify body
// decoder so the tests exercise the same entry point the handler uses.
func batchConfigDecodeBody(t *testing.T, raw string) *fastifyJSONBody {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("test payload is not valid JSON: %v", err)
	}
	return &fastifyJSONBody{Value: decoded, ObjectKeys: jsonTopLevelKeys([]byte(raw))}
}

// batchConfigValidatedConfig runs a config object through the real validator.
func batchConfigValidatedConfig(t *testing.T, config string) batchConfigConfig {
	t.Helper()
	body, message := batchConfigValidateBody(batchConfigDecodeBody(t, `{"instanceIds":[1],"config":`+config+`}`).Value)
	if message != "" {
		t.Fatalf("config %s failed validation: %s", config, message)
	}
	return body.Config
}

// TestBatchConfigValidateBody pins the AJV messages. The order is observable
// because allErrors is false: required (in order) → instanceIds (type, minItems,
// maxItems, uniqueItems, items) → config (type) → properties in schema order.
func TestBatchConfigValidateBody(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty object", `{}`, "instanceIds is required"},
		{"missing config", `{"instanceIds":[1]}`, "config is required"},
		{"instanceIds not an array", `{"instanceIds":{},"config":{}}`, "must be array"},
		{"instanceIds empty", `{"instanceIds":[],"config":{}}`, "must NOT have fewer than 1 items"},
		{"instanceIds too many", `{"instanceIds":[` + strings.TrimSuffix(strings.Repeat("1,", 1001), ",") + `],"config":{}}`, "must NOT have more than 1000 items"},
		{"instanceIds duplicate", `{"instanceIds":[1,1],"config":{}}`, "must NOT have duplicate items (items ## 1 and 0 are identical)"},
		{"instanceIds duplicate non-adjacent", `{"instanceIds":[5,6,7,6],"config":{}}`, "must NOT have duplicate items (items ## 3 and 1 are identical)"},
		{"instanceIds item zero", `{"instanceIds":[0],"config":{}}`, "must be >= 1"},
		{"instanceIds item negative", `{"instanceIds":[-2],"config":{}}`, "must be >= 1"},
		{"instanceIds item text", `{"instanceIds":["abc"],"config":{}}`, "must be integer"},
		{"instanceIds item null coerces to 0", `{"instanceIds":[null],"config":{}}`, "must be >= 1"},
		{"instanceIds scalar wraps", `{"instanceIds":5,"config":{}}`, ""},
		{"instanceIds boolean wraps to 1", `{"instanceIds":true,"config":{}}`, ""},
		{"instanceIds string number wraps", `{"instanceIds":"7","config":{}}`, ""},
		{"instanceIds nested single array unwraps", `{"instanceIds":[[5,6]],"config":{}}`, ""},
		{"config not an object", `{"instanceIds":[1],"config":[]}`, "must be object"},
		{"config null", `{"instanceIds":[1],"config":null}`, "must be object"},
		{"non-object root", `[]`, "must be object"},
		{"null root", `null`, "must be object"},
		{"cpu text", `{"instanceIds":[1],"config":{"cpu":"abc"}}`, "must be integer"},
		{"cpu zero", `{"instanceIds":[1],"config":{"cpu":0}}`, "must be >= 1"},
		{"cpu fractional", `{"instanceIds":[1],"config":{"cpu":1.5}}`, "must be integer"},
		{"memory zero", `{"instanceIds":[1],"config":{"memory":0}}`, "must be >= 1"},
		{"disk text", `{"instanceIds":[1],"config":{"disk":"abc"}}`, "must be number"},
		{"disk below minimum", `{"instanceIds":[1],"config":{"disk":0.5}}`, "must be >= 1"},
		{"monthly traffic object", `{"instanceIds":[1],"config":{"monthlyTrafficLimit":{}}}`, "must be string,null"},
		{"swap size negative", `{"instanceIds":[1],"config":{"swapSize":-1}}`, "must be >= 0"},
		{"swap size above max", `{"instanceIds":[1],"config":{"swapSize":1048577}}`, "must be <= 1048576"},
		{"port limit zero", `{"instanceIds":[1],"config":{"portLimit":0}}`, "must be >= 1"},
		{"port limit above max", `{"instanceIds":[1],"config":{"portLimit":1001}}`, "must be <= 1000"},
		{"snapshot limit negative", `{"instanceIds":[1],"config":{"snapshotLimit":-1}}`, "must be >= 0"},
		{"backup limit above max", `{"instanceIds":[1],"config":{"backupLimit":1001}}`, "must be <= 1000"},
		{"site limit negative", `{"instanceIds":[1],"config":{"siteLimit":-1}}`, "must be >= 0"},
		{"nested text", `{"instanceIds":[1],"config":{"nested":"yes"}}`, "must be boolean"},
		{"privileged text", `{"instanceIds":[1],"config":{"privileged":"yes"}}`, "must be boolean"},
		{"limits read iops text", `{"instanceIds":[1],"config":{"limitsReadIops":"abc"}}`, "must be integer,null"},
		{"limits cpu priority above max", `{"instanceIds":[1],"config":{"limitsCpuPriority":11}}`, "must be <= 10"},
		{"boot autostart text", `{"instanceIds":[1],"config":{"bootAutostart":"yes"}}`, "must be boolean,null"},
		{"boot priority above max", `{"instanceIds":[1],"config":{"bootAutostartPriority":101}}`, "must be <= 100"},
		{"boot delay below min", `{"instanceIds":[1],"config":{"bootAutostartDelay":4}}`, "must be >= 5"},
		{"boot delay above max", `{"instanceIds":[1],"config":{"bootAutostartDelay":601}}`, "must be <= 600"},
		{"shutdown timeout below min", `{"instanceIds":[1],"config":{"bootHostShutdownTimeout":29}}`, "must be >= 30"},
		{"first error wins: cpu before disk", `{"instanceIds":[1],"config":{"disk":0,"cpu":0}}`, "must be >= 1"},
		{"first error wins: instanceIds before config", `{"instanceIds":[0],"config":{"cpu":0}}`, "must be >= 1"},
		{"unknown config keys pass AJV", `{"instanceIds":[1],"config":{"mystery":123}}`, ""},
		{"single-element array property unwraps", `{"instanceIds":[1],"config":{"cpu":[4]}}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := batchConfigValidateBody(batchConfigDecodeBody(t, tc.raw).Value)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

// TestBatchConfigValidateBodyCoercion pins the coerced values (AJV writes them
// back into the body, and they are what the handler and retryPayload see).
func TestBatchConfigValidateBodyCoercion(t *testing.T) {
	raw := `{"instanceIds":[1,"2",3],"config":{` +
		`"cpu":"4","memory":true,"disk":"2.5",` +
		`"monthlyTrafficLimit":123,"swapEnabled":"true","swapSize":null,` +
		`"nested":1,"bootAutostart":"",` +
		`"limitsProcesses":"8","mystery":{"a":1}}}`
	body, message := batchConfigValidateBody(batchConfigDecodeBody(t, raw).Value)
	if message != "" {
		t.Fatalf("unexpected validation error: %s", message)
	}

	wantIDs := []float64{1, 2, 3}
	if fmt.Sprint(body.InstanceIDValues) != fmt.Sprint(wantIDs) {
		t.Fatalf("instanceIds = %v, want %v", body.InstanceIDValues, wantIDs)
	}

	if !body.Config.CPU.Present || body.Config.CPU.Value == nil || *body.Config.CPU.Value != 4 {
		t.Errorf("cpu = %+v", body.Config.CPU)
	}
	if !body.Config.Memory.Present || body.Config.Memory.Value == nil || *body.Config.Memory.Value != 1 {
		t.Errorf("memory = %+v", body.Config.Memory)
	}
	if !body.Config.Disk.Present || body.Config.Disk.Value == nil || *body.Config.Disk.Value != 2.5 {
		t.Errorf("disk = %+v", body.Config.Disk)
	}
	if !body.Config.MonthlyTrafficLimit.Present || body.Config.MonthlyTrafficLimit.Value == nil || *body.Config.MonthlyTrafficLimit.Value != "123" {
		t.Errorf("monthlyTrafficLimit = %+v", body.Config.MonthlyTrafficLimit)
	}
	if !body.Config.SwapEnabled.Present || body.Config.SwapEnabled.Value == nil || !*body.Config.SwapEnabled.Value {
		t.Errorf("swapEnabled = %+v", body.Config.SwapEnabled)
	}
	if !body.Config.SwapSize.Present || body.Config.SwapSize.Value != nil {
		t.Errorf("swapSize should be present-and-null: %+v", body.Config.SwapSize)
	}
	if !body.Config.Nested.Present || body.Config.Nested.Value == nil || !*body.Config.Nested.Value {
		t.Errorf("nested = %+v", body.Config.Nested)
	}
	// `""` is coerced to null by the ['boolean','null'] chain.
	if !body.Config.BootAutostart.Present || body.Config.BootAutostart.Value != nil {
		t.Errorf("bootAutostart should be present-and-null: %+v", body.Config.BootAutostart)
	}
	if body.Config.LimitsProcesses.Value == nil || *body.Config.LimitsProcesses.Value != 8 {
		t.Errorf("limitsProcesses = %+v", body.Config.LimitsProcesses)
	}

	// Unknown keys survive (no additionalProperties:false) and are echoed by
	// retryPayload.config.
	if _, present := body.RawConfig["mystery"]; !present {
		t.Errorf("unknown config key was dropped")
	}
	if rawCPU, ok := body.RawConfig["cpu"].(float64); !ok || rawCPU != 4 {
		t.Errorf("RawConfig cpu should hold the coerced number, got %#v", body.RawConfig["cpu"])
	}
}

// TestBatchConfigNormalizeInstanceIDs pins normalizeBatchInstanceIds, including
// the duplicate-after-coercion case that only it can catch.
func TestBatchConfigNormalizeInstanceIDs(t *testing.T) {
	normalized, ok := batchConfigNormalizeInstanceIDs([]float64{3, 1, 2})
	if !ok || fmt.Sprint(normalized) != "[3 1 2]" {
		t.Fatalf("normalize = %v,%v", normalized, ok)
	}

	invalid := map[string][]float64{
		"empty":           {},
		"zero":            {0},
		"negative":        {-1},
		"fraction":        {1.5},
		"duplicate":       {1, 1},
		"duplicate later": {2, 3, 2},
		"beyond int4":     {2147483648},
	}
	for name, values := range invalid {
		if _, ok := batchConfigNormalizeInstanceIDs(values); ok {
			t.Errorf("%s: %v should be rejected", name, values)
		}
	}
}

// TestBatchConfigIsRetryableError pins RETRYABLE_ERRORS (case-insensitive
// substring match).
func TestBatchConfigIsRetryableError(t *testing.T) {
	retryable := []string{
		"ETIMEDOUT",
		"connect ECONNREFUSED 127.0.0.1:8443",
		"ECONNRESET",
		"context deadline exceeded",
		"request timeout",
		"operation Timeout while updating",
	}
	for _, message := range retryable {
		if !batchConfigIsRetryableError(message) {
			t.Errorf("%q should be retryable", message)
		}
	}
	notRetryable := []string{
		"not found",
		"必须填写名称",
		"实例存在进行中的任务: create",
		"虚拟机不支持批量 SWAP 配置",
		"",
	}
	for _, message := range notRetryable {
		if batchConfigIsRetryableError(message) {
			t.Errorf("%q should not be retryable", message)
		}
	}
}

// TestBatchConfigConfigPredicates pins hasIncusConfig / hasAdvancedIncusConfig /
// hasDbAdvancedConfig. nested and privileged are Incus-only; cpu/memory/disk/swap
// are Incus-only too but are not "advanced".
func TestBatchConfigConfigPredicates(t *testing.T) {
	advanced := []string{
		`{"nested":true}`,
		`{"privileged":false}`,
		`{"limitsRead":"10MB"}`,
		`{"limitsWriteIops":100}`,
		`{"limitsIngress":"1Mbit"}`,
		`{"limitsEgress":null}`,
		`{"limitsProcesses":10}`,
		`{"limitsCpuPriority":5}`,
		`{"bootAutostart":true}`,
		`{"bootAutostartPriority":1}`,
		`{"bootAutostartDelay":10}`,
		`{"bootHostShutdownTimeout":60}`,
	}
	for _, raw := range advanced {
		config := batchConfigValidatedConfig(t, raw)
		if !batchConfigHasAdvancedIncusConfig(config) {
			t.Errorf("%s: expected advanced Incus config", raw)
		}
		if !batchConfigHasIncusConfig(config) {
			t.Errorf("%s: expected Incus config", raw)
		}
	}

	resourcesOnly := []string{`{"cpu":2}`, `{"memory":512}`, `{"disk":20}`, `{"swapEnabled":true}`, `{"swapSize":128}`}
	for _, raw := range resourcesOnly {
		config := batchConfigValidatedConfig(t, raw)
		if batchConfigHasAdvancedIncusConfig(config) {
			t.Errorf("%s: should not be advanced", raw)
		}
		if !batchConfigHasIncusConfig(config) {
			t.Errorf("%s: should need Incus", raw)
		}
	}

	dbOnly := []string{`{"portLimit":5}`, `{"snapshotLimit":5}`, `{"backupLimit":5}`, `{"siteLimit":5}`, `{"monthlyTrafficLimit":"100"}`}
	for _, raw := range dbOnly {
		config := batchConfigValidatedConfig(t, raw)
		if batchConfigHasIncusConfig(config) {
			t.Errorf("%s: should not need Incus", raw)
		}
		if batchConfigHasDBAdvancedConfig(config) {
			t.Errorf("%s: should not be advanced DB config", raw)
		}
	}

	// limitsRead/limitsWrite are Incus *and* DB.
	config := batchConfigValidatedConfig(t, `{"limitsRead":"10MB"}`)
	if !batchConfigHasDBAdvancedConfig(config) {
		t.Errorf("limitsRead should be stored in the DB")
	}
	if batchConfigHasDBAdvancedConfig(batchConfigValidatedConfig(t, `{"nested":true}`)) {
		t.Errorf("nested is Incus-only and must not be a DB advanced field")
	}
}

// TestBatchConfigResolveStorageOverrides pins limitsRead > limitsReadIops (and
// the write pair), with the other half explicitly NULLed for the DB.
func TestBatchConfigResolveStorageOverrides(t *testing.T) {
	read := batchConfigValidatedConfig(t, `{"limitsRead":"10MB"}`)
	overrides := batchConfigResolveStorageOverrides(read)
	if !overrides.LimitsRead.Present || overrides.LimitsRead.Value == nil || *overrides.LimitsRead.Value != "10MB" {
		t.Fatalf("limitsRead = %+v", overrides.LimitsRead)
	}
	if !overrides.LimitsReadIops.Present || overrides.LimitsReadIops.Value != nil {
		t.Fatalf("limitsReadIops must be explicitly NULL: %+v", overrides.LimitsReadIops)
	}

	readIops := batchConfigValidatedConfig(t, `{"limitsReadIops":500}`)
	overrides = batchConfigResolveStorageOverrides(readIops)
	if !overrides.LimitsRead.Present || overrides.LimitsRead.Value != nil {
		t.Fatalf("limitsRead must be explicitly NULL: %+v", overrides.LimitsRead)
	}
	if !overrides.LimitsReadIops.Present || overrides.LimitsReadIops.Value == nil || *overrides.LimitsReadIops.Value != 500 {
		t.Fatalf("limitsReadIops = %+v", overrides.LimitsReadIops)
	}

	// Both provided: the bandwidth string wins (the `else if`).
	both := batchConfigValidatedConfig(t, `{"limitsRead":"10MB","limitsReadIops":500}`)
	overrides = batchConfigResolveStorageOverrides(both)
	if overrides.LimitsRead.Value == nil || *overrides.LimitsRead.Value != "10MB" {
		t.Fatalf("limitsRead should win: %+v", overrides.LimitsRead)
	}
	if overrides.LimitsReadIops.Value != nil {
		t.Fatalf("limitsReadIops should be NULL when limitsRead wins: %+v", overrides.LimitsReadIops)
	}

	// Neither provided: both untouched.
	none := batchConfigResolveStorageOverrides(batchConfigValidatedConfig(t, `{"cpu":2}`))
	if none.LimitsRead.Present || none.LimitsReadIops.Present || none.LimitsWrite.Present || none.LimitsWriteIops.Present {
		t.Fatalf("unexpected overrides: %+v", none)
	}
}

// TestBatchConfigSwapHelpers pins lib/instance-swap.ts.
func TestBatchConfigSwapHelpers(t *testing.T) {
	zero, negative, configured := int32(0), int32(-5), int32(256)

	if got := batchConfigNormalizeConfiguredSwapSize(nil); got != nil {
		t.Errorf("nil should stay nil, got %v", got)
	}
	if got := batchConfigNormalizeConfiguredSwapSize(&negative); got == nil || *got != 0 {
		t.Errorf("negative should clamp to 0, got %v", got)
	}
	if got := batchConfigNormalizeConfiguredSwapSize(&configured); got == nil || *got != 256 {
		t.Errorf("configured should pass through, got %v", got)
	}

	if got := batchConfigResolveEffectiveSwapSize(&configured, &zero); got != 256 {
		t.Errorf("instance size should win, got %d", got)
	}
	if got := batchConfigResolveEffectiveSwapSize(nil, &configured); got != 256 {
		t.Errorf("plan size should be the fallback, got %d", got)
	}
	if got := batchConfigResolveEffectiveSwapSize(&zero, &configured); got != 0 {
		t.Errorf("an explicit 0 instance size must win, got %d", got)
	}
	if got := batchConfigResolveEffectiveSwapSize(nil, nil); got != 0 {
		t.Errorf("nil,nil should be 0, got %d", got)
	}

	if got := batchConfigFormatIncusSwapLimit(512); got != "512MiB" {
		t.Errorf("format = %q", got)
	}
	if got := batchConfigResolveIncusSwapValue(false, 512); got != "false" {
		t.Errorf("disabled = %q", got)
	}
	if got := batchConfigResolveIncusSwapValue(true, 0); got != "false" {
		t.Errorf("zero size = %q", got)
	}
	if got := batchConfigResolveIncusSwapValue(true, 512); got != "512MiB" {
		t.Errorf("enabled = %q", got)
	}
}

// TestBatchConfigNumericConversion pins JS Math.round and the 32-bit guard.
func TestBatchConfigNumericConversion(t *testing.T) {
	roundCases := []struct {
		input float64
		want  int32
	}{
		{1, 1},
		{1.4, 1},
		{1.5, 2},
		{2.5, 3},
		{-1.5, -1},
		{-0.5, 0},
		{1048576, 1048576},
	}
	for _, tc := range roundCases {
		got, err := batchConfigRound(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("batchConfigRound(%v) = %d,%v, want %d", tc.input, got, err, tc.want)
		}
	}
	for _, bad := range []float64{1e20, -1e20} {
		if _, err := batchConfigRound(bad); err == nil {
			t.Errorf("batchConfigRound(%v) should fail", bad)
		}
	}

	if got, err := batchConfigInt32(2147483647); err != nil || got != 2147483647 {
		t.Errorf("batchConfigInt32(max) = %d,%v", got, err)
	}
	for _, bad := range []float64{2147483648, -2147483649, 1e20} {
		if _, err := batchConfigInt32(bad); err == nil {
			t.Errorf("batchConfigInt32(%v) should fail", bad)
		}
	}
}

// TestBatchConfigResultItemJSON pins the per-item JSON shape: `error` is dropped
// for successes and present for failures.
func TestBatchConfigResultItemJSON(t *testing.T) {
	success, err := json.Marshal(batchConfigResultItem{InstanceID: 5, IncusID: "i-5", Name: "n", Success: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(success) != `{"instanceId":5,"incusId":"i-5","name":"n","success":true}` {
		t.Fatalf("success item = %s", success)
	}

	message := "boom"
	failure, err := json.Marshal(batchConfigResultItem{InstanceID: 5, IncusID: "i-5", Name: "n", Success: false, Error: &message})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(failure) != `{"instanceId":5,"incusId":"i-5","name":"n","success":false,"error":"boom"}` {
		t.Fatalf("failure item = %s", failure)
	}
}

// TestBatchConfigResponseJSON pins the response envelope: no retryPayload when
// everything succeeded, and the retryPayload.config echoed back when not.
func TestBatchConfigResponseJSON(t *testing.T) {
	allGood, err := json.Marshal(batchConfigResponse{
		Success:      true,
		TotalCount:   2,
		SuccessCount: 2,
		FailedCount:  0,
		FailedItems:  []batchConfigResultItem{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantGood := `{"success":true,"totalCount":2,"successCount":2,"failedCount":0,"failedItems":[]}`
	if string(allGood) != wantGood {
		t.Fatalf("success response = %s", allGood)
	}

	message := "boom"
	partial, err := json.Marshal(batchConfigResponse{
		Success:      false,
		TotalCount:   2,
		SuccessCount: 1,
		FailedCount:  1,
		FailedItems:  []batchConfigResultItem{{InstanceID: 9, IncusID: "i-9", Name: "nine", Success: false, Error: &message}},
		RetryPayload: &batchConfigRetryPayload{InstanceIDs: []int32{9}, Config: map[string]any{"cpu": float64(2)}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantPartial := `{"success":false,"totalCount":2,"successCount":1,"failedCount":1,` +
		`"failedItems":[{"instanceId":9,"incusId":"i-9","name":"nine","success":false,"error":"boom"}],` +
		`"retryPayload":{"instanceIds":[9],"config":{"cpu":2}}}`
	if string(partial) != wantPartial {
		t.Fatalf("partial response = %s", partial)
	}
}

// TestBatchConfigLogResult pins `failedCount > 0 ? 'partial' : 'success'`.
func TestBatchConfigLogResult(t *testing.T) {
	if got := batchConfigLogResult(false); got != "success" {
		t.Errorf("log result = %q", got)
	}
	if got := batchConfigLogResult(true); got != "partial" {
		t.Errorf("log result = %q", got)
	}
}

// TestBatchConfigResourceDeltas pins the delta accounting (Math.round(disk)).
func TestBatchConfigResourceDeltas(t *testing.T) {
	instance := db.BatchConfigInstance{ID: 1, CPU: 2, Memory: 512, Disk: 10}
	config := batchConfigValidatedConfig(t, `{"cpu":4,"memory":1024,"disk":12.5}`)

	cpu, memory, disk := batchConfigResourceDeltas(config, instance)
	if cpu != 2 || memory != 512 || disk != 3 {
		t.Fatalf("deltas = (%d,%d,%d), want (2,512,3)", cpu, memory, disk)
	}

	// Absent fields contribute nothing.
	cpu, memory, disk = batchConfigResourceDeltas(batchConfigValidatedConfig(t, `{"portLimit":5}`), instance)
	if cpu != 0 || memory != 0 || disk != 0 {
		t.Fatalf("deltas for a quota-only config = (%d,%d,%d)", cpu, memory, disk)
	}
}

// TestBatchConfigForEachLimitedRespectsLimit proves the worker pool runs exactly
// BATCH_CONFIG_CONCURRENCY workers: 10 tasks must be able to run at once (a lower
// limit deadlocks the barrier and fails on the timeout) and never more than 10
// (the peak is asserted afterwards).
func TestBatchConfigForEachLimitedRespectsLimit(t *testing.T) {
	const total = 40
	items := make([]int, total)
	for i := range items {
		items[i] = i
	}

	var (
		mu      sync.Mutex
		running int
		peak    int
		ran     int
	)
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		batchConfigForEachLimited(batchConfigConcurrency, items, func(int) {
			mu.Lock()
			running++
			ran++
			if running > peak {
				peak = running
			}
			mu.Unlock()

			<-release

			mu.Lock()
			running--
			mu.Unlock()
		})
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		current := running
		mu.Unlock()
		if current >= batchConfigConcurrency {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d workers ran concurrently, want %d", current, batchConfigConcurrency)
		case <-time.After(time.Millisecond):
		}
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker pool did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if peak != batchConfigConcurrency {
		t.Fatalf("peak concurrency = %d, want %d", peak, batchConfigConcurrency)
	}
	if ran != total {
		t.Fatalf("ran %d items, want %d", ran, total)
	}
	if running != 0 {
		t.Fatalf("running = %d after completion", running)
	}
}

// TestBatchConfigRoutesRegistered pins the mounted path and verb.
func TestBatchConfigRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterBatchConfig(engine)

	found := false
	for _, route := range engine.Routes() {
		if route.Method == "POST" && route.Path == "/api/hosts/:hostId/instances/batch-config" {
			found = true
		}
	}
	if !found {
		t.Fatalf("POST /api/hosts/:hostId/instances/batch-config is not registered")
	}
}

// TestBatchConfigRoutesCoexistWithRedeemCodes guards the gin wildcard naming:
// routes/redeem_codes.go already mounts /api/hosts/:hostId/... and gin panics if a
// second route uses a different wildcard name at the same position.
func TestBatchConfigRoutesCoexistWithRedeemCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterRedeemCodes(engine)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("mounting the batch-config route conflicted with the redeem-code routes: %v", recovered)
		}
	}()
	RegisterBatchConfig(engine)
	// routes/hosts.go will mount /api/hosts/:hostId/... as well.
	engine.GET("/api/hosts/:hostId", func(c *gin.Context) {})
}

// ==================== handler-level checks (no database) ====================

// batchConfigRunHandler drives the real handler through gin. Every case below is
// rejected before any database access, so no DB is required.
func batchConfigRunHandler(t *testing.T, hostID, contentType, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/hosts/:hostId/instances/batch-config", func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{ID: 1, Role: "admin", Username: "root"})
		batchConfigHandler(c)
	})

	request := httptest.NewRequest(http.MethodPost, "/api/hosts/"+hostID+"/instances/batch-config", strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// batchConfigAssertJSON compares a response body to an expected JSON document
// without depending on key order.
func batchConfigAssertJSON(t *testing.T, raw, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(raw), &gotValue); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected value is not JSON: %v (%s)", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("body = %s, want %s", raw, want)
	}
}

// TestBatchConfigHandlerValidation pins the response envelope and the
// body-validation-before-route-id ordering.
func TestBatchConfigHandlerValidation(t *testing.T) {
	cases := []struct {
		name       string
		hostID     string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name: "missing instanceIds", hostID: "1", body: `{}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"instanceIds is required","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "duplicate ids", hostID: "1", body: `{"instanceIds":[1,1],"config":{}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must NOT have duplicate items (items ## 1 and 0 are identical)","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "cpu zero", hostID: "1", body: `{"instanceIds":[1],"config":{"cpu":0}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be >= 1","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "disk text", hostID: "1", body: `{"instanceIds":[1],"config":{"disk":"abc"}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be number","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "non-object body", hostID: "1", body: `[]`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be object","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "validation before host id", hostID: "0", body: `{"instanceIds":[1,1],"config":{"cpu":1}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must NOT have duplicate items (items ## 1 and 0 are identical)","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "valid body then invalid host id", hostID: "0", body: `{"instanceIds":[1],"config":{"cpu":1}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"Invalid ID","code":"INVALID_ID"}`,
		},
		{
			name: "malformed json", hostID: "1", body: `{"instanceIds":`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"Body is not valid JSON but content-type is set to 'application/json'"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := batchConfigRunHandler(t, tc.hostID, "application/json", tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.wantStatus, body)
			}
			batchConfigAssertJSON(t, body, tc.wantBody)
		})
	}
}
