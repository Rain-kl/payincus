package routes

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ==================== pagination / filter parsing ====================

// TestResourcePoolParseClampedPositiveIntegerQuery pins parseClampedPositiveIntegerQuery:
// strict ^[1-9]\d*$, JS safe-integer range, clamp to max, fallback otherwise.
func TestResourcePoolParseClampedPositiveIntegerQuery(t *testing.T) {
	cases := []struct {
		value    string
		fallback int
		max      int
		want     int
	}{
		{"", 20, 100, 20},
		{"20", 20, 100, 20},
		{"1", 20, 100, 1},
		{"100", 20, 100, 100},
		{"101", 20, 100, 100},
		{"999999", 20, 100, 100},
		{"0", 20, 100, 20},
		{"-1", 20, 100, 20},
		{"01", 20, 100, 20},
		{"007", 20, 100, 20},
		{"1.5", 20, 100, 20},
		{"abc", 20, 100, 20},
		{" 5", 20, 100, 20},
		{"9007199254740992", 20, 100, 20}, // 2^53, not a JS safe integer
		{"9007199254740991", 20, 100, 100},
	}

	for _, tc := range cases {
		if got := resourcePoolParseClampedPositiveIntegerQuery(tc.value, tc.fallback, tc.max); got != tc.want {
			t.Fatalf("%q: got %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestResourcePoolParseNonNegativeIntegerQuery pins parseNonNegativeIntegerQuery.
func TestResourcePoolParseNonNegativeIntegerQuery(t *testing.T) {
	cases := map[string]int{
		"":                 0,
		"0":                0,
		"1":                1,
		"250":              250,
		"-1":               0,
		"01":               0,
		"1.5":              0,
		"nope":             0,
		"9007199254740992": 0,
		"9007199254740991": math.MaxInt32,
	}
	for value, want := range cases {
		if got := resourcePoolParseNonNegativeIntegerQuery(value, 0); got != want {
			t.Fatalf("%q: got %d, want %d", value, got, want)
		}
	}
}

// TestResourcePoolQueryFilters pins the allowlists of the log filters.
func TestResourcePoolQueryFilters(t *testing.T) {
	for _, action := range []string{"checkin", "redeem", "admin_grant", "system_grant", "lottery", "apply", "system_redeem"} {
		if got := resourcePoolNormalizeAction(action); got == nil || *got != action {
			t.Fatalf("action %q should be allowed", action)
		}
	}
	for _, action := range []string{"", "nope", "signin", "APPLY", "p"} {
		if got := resourcePoolNormalizeAction(action); got != nil {
			t.Fatalf("action %q should be rejected, got %q", action, *got)
		}
	}

	for _, resourceType := range []string{"c", "r", "d", "t"} {
		if got := resourcePoolNormalizeType(resourceType); got == nil || *got != resourceType {
			t.Fatalf("type %q should be allowed", resourceType)
		}
	}
	for _, resourceType := range []string{"", "p", "cpu", "C"} {
		if got := resourcePoolNormalizeType(resourceType); got != nil {
			t.Fatalf("type %q should be rejected, got %q", resourceType, *got)
		}
	}
}

// TestResourcePoolSingleQueryValue pins the fast-querystring shape: a repeated key
// is an array and every allowlist check rejects it.
func TestResourcePoolSingleQueryValue(t *testing.T) {
	query := map[string][]string{
		"action": {"checkin"},
		"limit":  {"10", "20"},
	}
	if got := resourcePoolSingleQueryValue(query, "action"); got != "checkin" {
		t.Fatalf("action = %q", got)
	}
	if got := resourcePoolSingleQueryValue(query, "limit"); got != "" {
		t.Fatalf("repeated key must be treated as absent, got %q", got)
	}
	if got := resourcePoolSingleQueryValue(query, "offset"); got != "" {
		t.Fatalf("missing key must be absent, got %q", got)
	}
}

// ==================== VIP pool cost ====================

// TestResourcePoolCalculateVipResourcePoolCost pins the ceil formula:
// max(1, ceil(amount * 100 / (100 + bonusPercent))), bonusPercent clamped 0..100.
func TestResourcePoolCalculateVipResourcePoolCost(t *testing.T) {
	cases := []struct {
		amount int64
		bonus  float64
		want   int64
	}{
		{100, 0, 100},
		{100, 2, 99}, // ceil(10000/102) = 99
		{1, 2, 1},    // max(1, ceil(0.98)) = 1
		{1, 0, 1},
		{1000, 100, 500},
		{150, 100, 75},
		{3, 10, 3},                // ceil(300/110) = 3
		{11, 10, 10},              // ceil(1100/110) = 10
		{100, -50, 100},           // clamped to 0
		{100, 250, 50},            // clamped to 100
		{100, 2.5, 98},            // ceil(10000/102.5) = 98
		{104857600, 10, 95325091}, // ceil(10485760000/110)
	}

	for _, tc := range cases {
		got, err := resourcePoolCalculateVipResourcePoolCost(tc.amount, tc.bonus)
		if err != nil {
			t.Fatalf("amount=%d bonus=%v: %v", tc.amount, tc.bonus, err)
		}
		if got != tc.want {
			t.Fatalf("amount=%d bonus=%v: got %d, want %d", tc.amount, tc.bonus, got, tc.want)
		}
	}

	for _, amount := range []int64{0, -1} {
		if _, err := resourcePoolCalculateVipResourcePoolCost(amount, 5); err == nil {
			t.Fatalf("amount %d must be rejected", amount)
		}
	}
}

// TestResourcePoolVipBenefitsConfig pins the DEFAULT_VIP_BENEFITS_CONFIG values and
// the parse fallbacks (missing/invalid JSON and partial configs use the defaults).
func TestResourcePoolVipBenefitsConfig(t *testing.T) {
	defaults := resourcePoolDefaultVipBenefitsConfig()
	expected := map[int]float64{1: 2, 2: 4, 3: 6, 4: 8, 5: 10}
	for level, bonus := range expected {
		if defaults.Levels[level].ResourcePoolBonusPercent != bonus {
			t.Fatalf("level %d bonus = %v", level, defaults.Levels[level].ResourcePoolBonusPercent)
		}
	}

	if got := resourcePoolParseVipBenefitsConfigJSON(nil); got.Levels[5].ResourcePoolBonusPercent != 10 {
		t.Fatal("nil config must fall back to defaults")
	}
	invalid := "{not json"
	if got := resourcePoolParseVipBenefitsConfigJSON(&invalid); got.Levels[1].ResourcePoolBonusPercent != 2 {
		t.Fatal("invalid JSON must fall back to defaults")
	}
	partial := `{"levels":{"1":{"orderDiscountPercent":1,"extraTrafficPercent":2,"resourcePoolBonusPercent":2}}}`
	if got := resourcePoolParseVipBenefitsConfigJSON(&partial); got.Levels[5].ResourcePoolBonusPercent != 10 {
		t.Fatal("partial config must fall back to defaults")
	}

	validCustom := `{"levels":{` +
		`"1":{"orderDiscountPercent":1.239,"extraTrafficPercent":2,"resourcePoolBonusPercent":2},` +
		`"2":{"orderDiscountPercent":2,"extraTrafficPercent":4,"resourcePoolBonusPercent":4},` +
		`"3":{"orderDiscountPercent":3,"extraTrafficPercent":6,"resourcePoolBonusPercent":6},` +
		`"4":{"orderDiscountPercent":4,"extraTrafficPercent":8,"resourcePoolBonusPercent":8},` +
		`"5":{"orderDiscountPercent":5,"extraTrafficPercent":10,"resourcePoolBonusPercent":12.5}}}`
	parsed := resourcePoolParseVipBenefitsConfigJSON(&validCustom)
	// Math.round(1.239 * 100) / 100 = 1.24
	if parsed.Levels[1].OrderDiscountPercent != 1.24 {
		t.Fatalf("rounded discount = %v", parsed.Levels[1].OrderDiscountPercent)
	}
	if parsed.Levels[5].ResourcePoolBonusPercent != 12.5 {
		t.Fatalf("level 5 bonus = %v", parsed.Levels[5].ResourcePoolBonusPercent)
	}

	outOfRange := `{"levels":{` +
		`"1":{"orderDiscountPercent":101,"extraTrafficPercent":2,"resourcePoolBonusPercent":2},` +
		`"2":{"orderDiscountPercent":2,"extraTrafficPercent":4,"resourcePoolBonusPercent":4},` +
		`"3":{"orderDiscountPercent":3,"extraTrafficPercent":6,"resourcePoolBonusPercent":6},` +
		`"4":{"orderDiscountPercent":4,"extraTrafficPercent":8,"resourcePoolBonusPercent":8},` +
		`"5":{"orderDiscountPercent":5,"extraTrafficPercent":10,"resourcePoolBonusPercent":10}}}`
	if got := resourcePoolParseVipBenefitsConfigJSON(&outOfRange); got.Levels[1].OrderDiscountPercent != 1 {
		t.Fatal("out-of-range percentage must fall back to defaults")
	}
}

// ==================== /apply body schema ====================

// TestResourcePoolApplyBodyValidation pins the Fastify schema of POST /apply.
func TestResourcePoolApplyBodyValidation(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
		wantOK      bool
	}{
		{"empty object", `{}`, "instanceId is required", false},
		{"missing type", `{"instanceId":1}`, "resourceType is required", false},
		{"missing amount", `{"instanceId":1,"resourceType":"c"}`, "amount is required", false},
		{"unknown type", `{"instanceId":1,"resourceType":"p","amount":1}`, "must be equal to one of the allowed values", false},
		{"zero amount", `{"instanceId":1,"resourceType":"c","amount":0}`, "must be >= 1", false},
		{"over max", `{"instanceId":1,"resourceType":"c","amount":104857601}`, "must be <= 104857600", false},
		{"max amount", `{"instanceId":1,"resourceType":"c","amount":104857600}`, "", true},
		{"fractional amount", `{"instanceId":1,"resourceType":"c","amount":2.5}`, "must be integer", false},
		{"zero instance", `{"instanceId":0,"resourceType":"c","amount":1}`, "must be >= 1", false},
		{"coerced strings", `{"instanceId":"3","resourceType":"t","amount":"5"}`, "", true},
		{"coerced exponent", `{"instanceId":1,"resourceType":"c","amount":"1e3"}`, "", true},
		{"single-element array property", `{"instanceId":1,"resourceType":["c"],"amount":[5]}`, "", true},
		{"boolean instance id", `{"instanceId":true,"resourceType":"c","amount":1}`, "", true},
		{"valid", `{"instanceId":3,"resourceType":"r","amount":128}`, "", true},
		{"root array unwrap", `[{"instanceId":3,"resourceType":"d","amount":1024}]`, "must be object", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded any
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			validated, message := validateResourcePoolApplyBodyAJV(decoded)
			if tc.wantOK {
				if message != "" {
					t.Fatalf("want no message, got %q", message)
				}
				if _, present := validated.Present["amount"]; !present {
					t.Fatal("amount should be present")
				}
				return
			}
			if message != tc.wantMessage {
				t.Fatalf("want %q, got %q", tc.wantMessage, message)
			}
		})
	}
}

// ==================== /apply error mapping ====================

// TestResourcePoolApplyErrorMapping pins the catch block of POST /apply. The
// default branch writes an audit log and is covered by the conformance suite.
func TestResourcePoolApplyErrorMapping(t *testing.T) {
	cases := []struct {
		message     string
		wantStatus  int
		wantError   string
		wantCode    string
		wantDetails string
		wantRawBody bool
	}{
		{"INSTANCE_NOT_FOUND", http.StatusNotFound, "Instance not found", "INSTANCE_NOT_FOUND", "", false},
		{"FORBIDDEN", http.StatusForbidden, "Access denied", "FORBIDDEN", "", false},
		{"INSTANCE_STATUS_INVALID", http.StatusBadRequest, "Instance status does not allow this operation", "INSTANCE_STATUS_INVALID", "", false},
		{"HOST_NOT_FOUND", http.StatusNotFound, "Host not found", "HOST_NOT_FOUND", "", false},
		{"RESOURCE_POOL_DISABLED", http.StatusForbidden, "This feature has been disabled", "FEATURE_DISABLED", "该节点未开启资源池玩法", false},
		{"RESOURCE_POOL_KVM_CPU_MULTIPLE", http.StatusBadRequest, "KVM instance CPU must be a multiple of 100", "RESOURCE_POOL_KVM_CPU_MULTIPLE", "", false},
		{"RESOURCE_POOL_KVM_MEMORY_MULTIPLE", http.StatusBadRequest, "KVM instance memory must be a multiple of 128MB", "RESOURCE_POOL_KVM_MEMORY_MULTIPLE", "", false},
		{"RESOURCE_POOL_KVM_DISK_MULTIPLE", http.StatusBadRequest, "KVM instance disk must be a multiple of 1GB (1024MB)", "RESOURCE_POOL_KVM_DISK_MULTIPLE", "", false},
		{"RESOURCE_POOL_VM_MUST_STOP", http.StatusBadRequest, "KVM instance must be stopped to adjust memory or disk", "RESOURCE_POOL_VM_MUST_STOP", "", false},
		{"RESOURCE_POOL_INSUFFICIENT", http.StatusBadRequest, "Insufficient resource pool balance", "RESOURCE_POOL_INSUFFICIENT", "", false},
		{"HOST_RESOURCES_INSUFFICIENT", http.StatusBadRequest, "All hosts have insufficient resources, please try later or choose another package", "HOST_RESOURCES_INSUFFICIENT", "Host capacity is insufficient for this resource claim", false},
		{"RESOURCE_POOL_BUSY", http.StatusConflict, "", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			context, recorder := checkinTestRecorder(t, http.MethodPost, "/api/resource-pool/apply")
			resourcePoolRespondApplyError(context, 1, 9, errString(tc.message))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			body := checkinDecodeResponse(t, recorder)
			if tc.wantRawBody {
				if body["error"] != "RESOURCE_POOL_BUSY" {
					t.Fatalf("error = %v", body["error"])
				}
				if body["message"] != "Resource pool apply is busy, please retry" {
					t.Fatalf("message = %v", body["message"])
				}
				if _, present := body["code"]; present {
					t.Fatalf("busy body must not carry a code field: %v", body)
				}
				return
			}
			if body["error"] != tc.wantError {
				t.Fatalf("error = %v, want %v", body["error"], tc.wantError)
			}
			if body["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %v", body["code"], tc.wantCode)
			}
			if tc.wantDetails == "" {
				if _, present := body["details"]; present {
					t.Fatalf("unexpected details: %v", body["details"])
				}
			} else if body["details"] != tc.wantDetails {
				t.Fatalf("details = %v, want %v", body["details"], tc.wantDetails)
			}
		})
	}
}

// ==================== lock keys ====================

// TestResourcePoolApplyLockKeys pins the distributed-lock names and their order.
func TestResourcePoolApplyLockKeys(t *testing.T) {
	got := resourcePoolApplyLockKeys(7, 42, "c")
	want := []string{"resource-pool:7:c", "instance:42:resource-pool-apply"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("lock[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

// ==================== apply input guards ====================

// TestResourcePoolApplyInputGuards pins the DB-level argument validation that runs
// before any transaction (so it is testable without a database).
func TestResourcePoolApplyInputGuards(t *testing.T) {
	cases := []struct {
		name      string
		input     db.ResourcePoolApplyInput
		wantError error
	}{
		{"zero amount", db.ResourcePoolApplyInput{Amount: 0, ResourceType: "c"}, db.ErrResourcePoolInvalidAmount},
		{"negative amount", db.ResourcePoolApplyInput{Amount: -5, ResourceType: "c"}, db.ErrResourcePoolInvalidAmount},
		{"over max amount", db.ResourcePoolApplyInput{Amount: db.ResourcePoolMaxApplyAmount + 1, ResourceType: "c"}, db.ErrResourcePoolInvalidAmount},
		{"zero debit", db.ResourcePoolApplyInput{Amount: 10, PoolDebitAmount: int32Pointer(0), ResourceType: "c"}, db.ErrResourcePoolInvalidDebit},
		{"debit over amount", db.ResourcePoolApplyInput{Amount: 10, PoolDebitAmount: int32Pointer(11), ResourceType: "c"}, db.ErrResourcePoolInvalidDebit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ResourcePoolApplyToInstance(t.Context(), tc.input)
			if err != tc.wantError {
				t.Fatalf("err = %v, want %v", err, tc.wantError)
			}
		})
	}

	if _, err := db.ResourcePoolApplyToInstance(t.Context(), db.ResourcePoolApplyInput{
		Amount: 1, ResourceType: "p",
	}); err == nil || err.Error() != "Invalid resource type: p" {
		t.Fatalf("invalid type error = %v", err)
	}
}

func int32Pointer(value int32) *int32 { return &value }
