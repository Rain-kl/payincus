// Pure-logic tests for the verification route port
// (server/src/routes/verification.ts).
//
// The DB-backed paths (requesting / verifying codes, notification-channel lookup)
// are exercised by the dual-run conformance suite. What is pinned here is the
// wire contract that the Fastify pipeline alone decides:
//
//   - AJV defaults: allErrors:false (first message only), coerceTypes:'array'
//     (scalars stringified, single-element arrays unwrapped), required-before-
//     properties ordering, and the setErrorHandler messages;
//   - normalizeResourceIdForOperation: resource ops demand a strict positive
//     safe-integer id, account ops forbid one, and the two INVALID_RESOURCE_ID
//     payloads differ by operation kind;
//   - the /operation-types projection (account=email, resource=notification) and
//     the exact bilingual operation names.
package routes

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestNormalizeVerificationResourceID pins the resource-id rules.
func TestNormalizeVerificationResourceID(t *testing.T) {
	t.Run("resource operation requires an id", func(t *testing.T) {
		if _, ok := normalizeVerificationResourceID(models.OperationTypeDeleteInstance, nil, false); ok {
			t.Fatal("absent resourceId must be rejected for a resource operation")
		}
		if _, ok := normalizeVerificationResourceID(models.OperationTypeDeleteInstance, "", false); ok {
			t.Fatal("absent resourceId (empty string marker) must be rejected")
		}
	})

	t.Run("resource operation accepts a strict positive string id", func(t *testing.T) {
		id, ok := normalizeVerificationResourceID(models.OperationTypeDeleteInstance, "42", true)
		if !ok || id != 42 {
			t.Fatalf("resourceId \"42\" = (%d, %v), want (42, true)", id, ok)
		}
	})

	t.Run("resource operation rejects malformed string ids", func(t *testing.T) {
		for _, raw := range []string{"0", "-1", "01", "1.5", "1e3", "abc", "", " 1", "9007199254740993"} {
			if _, ok := normalizeVerificationResourceID(models.OperationTypeDeleteInstance, raw, true); ok {
				t.Fatalf("resourceId %q must be rejected", raw)
			}
		}
	})

	t.Run("resource operation accepts a numeric safe integer", func(t *testing.T) {
		id, ok := normalizeVerificationResourceID(models.OperationTypeDeleteBackup, float64(7), true)
		if !ok || id != 7 {
			t.Fatalf("numeric resourceId = (%d, %v), want (7, true)", id, ok)
		}
	})

	t.Run("resource operation rejects non-integer numbers", func(t *testing.T) {
		for _, value := range []float64{0, -3, 1.5, math.Inf(1), math.NaN(), 1 << 53} {
			if _, ok := normalizeVerificationResourceID(models.OperationTypeDeleteBackup, value, true); ok {
				t.Fatalf("numeric resourceId %v must be rejected", value)
			}
		}
	})

	t.Run("account operation forbids a resource id", func(t *testing.T) {
		if _, ok := normalizeVerificationResourceID(models.OperationTypeChangePassword, "5", true); ok {
			t.Fatal("account operation with a resourceId must be rejected")
		}
		if id, ok := normalizeVerificationResourceID(models.OperationTypeChangePassword, nil, false); !ok || id != 0 {
			t.Fatalf("account operation without resourceId = (%d, %v), want (0, true)", id, ok)
		}
	})
}

// TestVerificationInvalidResourcePayloads pins the two distinct messages.
func TestVerificationInvalidResourcePayloads(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	respondVerificationInvalidResource(c, models.OperationTypeDeleteInstance)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["error"] != "Resource ID is required for this operation" {
		t.Fatalf("error = %v", payload["error"])
	}
	if payload["code"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("code = %v", payload["code"])
	}
	// The TypeScript sends a literal object, never apiError(), so no `details`.
	if _, present := payload["details"]; present {
		t.Fatalf("details must not be present: %v", payload)
	}

	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	respondVerificationInvalidResource(c, models.OperationTypeChangeEmail)

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["error"] != "Resource ID is not allowed for this operation" {
		t.Fatalf("error = %v", payload["error"])
	}
	if payload["code"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("code = %v", payload["code"])
	}
}

// TestValidOperationTypesMatchesTypeScript pins the enum order (which the
// /operation-types response exposes) and that every entry classifies exactly once.
func TestValidOperationTypesMatchesTypeScript(t *testing.T) {
	want := []models.OperationType{
		models.OperationTypeChangePassword,
		models.OperationTypeDisable2fa,
		models.OperationTypeChangeEmail,
		models.OperationTypeDeleteAccount,
		models.OperationTypeDeleteInstance,
		models.OperationTypeReinstallInstance,
		models.OperationTypeRecreateInstance,
		models.OperationTypeTransferInstance,
		models.OperationTypeDeleteSnapshot,
		models.OperationTypeDeleteBackup,
	}
	if len(validOperationTypes) != len(want) {
		t.Fatalf("VALID_OPERATION_TYPES size = %d, want %d", len(validOperationTypes), len(want))
	}
	for i, expected := range want {
		if validOperationTypes[i] != expected {
			t.Fatalf("VALID_OPERATION_TYPES[%d] = %q, want %q", i, validOperationTypes[i], expected)
		}
	}

	for _, operationType := range validOperationTypes {
		account := lib.IsAccountOperation(operationType)
		resource := lib.IsResourceOperation(operationType)
		if account == resource {
			t.Fatalf("operation %q must be exactly one of account/resource (account=%v resource=%v)",
				operationType, account, resource)
		}
	}
}

// TestVerificationOperationTypesProjection pins the channel mapping.
func TestVerificationOperationTypesProjection(t *testing.T) {
	accountOperations := 0
	resourceOperations := 0
	for _, operationType := range validOperationTypes {
		switch {
		case lib.IsAccountOperation(operationType):
			accountOperations++
		case lib.IsResourceOperation(operationType):
			resourceOperations++
		}
	}
	// 4 account operations (change_password/disable_2fa/change_email/delete_account)
	// and 6 resource operations, exactly like VALID_OPERATION_TYPES.
	if accountOperations != 4 || resourceOperations != 6 {
		t.Fatalf("account=%d resource=%d, want 4 and 6", accountOperations, resourceOperations)
	}

	if got := lib.GetOperationName(models.OperationTypeChangePassword, "zh"); got != "修改密码" {
		t.Fatalf("zh name = %q", got)
	}
	if got := lib.GetOperationName(models.OperationTypeChangePassword, "en"); got != "Change Password" {
		t.Fatalf("en name = %q", got)
	}
	if got := lib.GetOperationName(models.OperationTypeDeleteBackup, "zh"); got != "删除备份" {
		t.Fatalf("zh name = %q", got)
	}
	if got := lib.GetOperationName(models.OperationTypeDeleteBackup, "en"); got != "Delete Backup" {
		t.Fatalf("en name = %q", got)
	}
}

// TestValidateVerificationRequestAJV pins the POST /request schema behaviour.
func TestValidateVerificationRequestAJV(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"missing operationType", `{"resourceId":1}`, "operationType is required"},
		{"non-object body", `[]`, "must be object"},
		{"empty object", `{}`, "operationType is required"},
		{"unknown operation", `{"operationType":"nope"}`, "must be equal to one of the allowed values"},
		{"scalar operation is stringified", `{"operationType":123}`, "must be equal to one of the allowed values"},
		{"single-element array unwraps", `{"operationType":["delete_instance"],"resourceId":3}`, ""},
		{"resourceId must be integer", `{"operationType":"delete_instance","resourceId":"abc"}`, "must be integer"},
		{"resourceId minimum", `{"operationType":"delete_instance","resourceId":0}`, "must be >= 1"},
		{"resourceId string digits coerce", `{"operationType":"delete_instance","resourceId":"7"}`, ""},
		{"resourceType must be string", `{"operationType":"delete_instance","resourceId":1,"resourceType":{}}`, "must be string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded any
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				// `[]` is valid JSON; a decode error here is a test bug.
				t.Fatalf("decode %s: %v", tc.body, err)
			}
			_, message := validateAJVObject(decoded, []string{"operationType"}, verificationRequestBodyFields)
			if message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", message, tc.wantMsg)
			}
		})
	}
}

// TestValidateVerificationVerifyAJV pins the POST /verify schema: the code must be
// exactly six characters and both required keys are checked first.
func TestValidateVerificationVerifyAJV(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"missing operationType", `{"code":"123456"}`, "operationType is required"},
		{"missing code", `{"operationType":"change_password"}`, "code is required"},
		{"short code", `{"operationType":"change_password","code":"12345"}`, "code is too short"},
		{"long code", `{"operationType":"change_password","code":"1234567"}`, "code is too long"},
		{"exact code", `{"operationType":"change_password","code":"123456"}`, ""},
		{"enum first", `{"operationType":"nope","code":"123456"}`, "must be equal to one of the allowed values"},
		{"resourceId minimum", `{"operationType":"delete_instance","code":"123456","resourceId":0}`, "must be >= 1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded any
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				t.Fatalf("decode %s: %v", tc.body, err)
			}
			_, message := validateAJVObject(decoded, []string{"operationType", "code"}, verificationVerifyBodyFields)
			if message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", message, tc.wantMsg)
			}
		})
	}
}

// TestVerificationQueryValue pins the fast-querystring shape helper.
func TestVerificationQueryValue(t *testing.T) {
	query := url.Values{}
	if _, present := verificationQueryValue(query, "operationType"); present {
		t.Fatal("absent key must report present=false")
	}

	query.Set("operationType", "change_password")
	if value, present := verificationQueryValue(query, "operationType"); !present || value != "change_password" {
		t.Fatalf("single value = %v (present=%v)", value, present)
	}

	query.Add("operationType", "delete_instance")
	value, present := verificationQueryValue(query, "operationType")
	if !present {
		t.Fatal("repeated key must report present=true")
	}
	items, ok := value.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("repeated key = %#v, want a 2-element array", value)
	}
}

// TestRespondVerificationInternalShape pins the 500 envelope.
func TestRespondVerificationInternalShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	respondVerificationInternal(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["error"] != "Internal server error" {
		t.Fatalf("error = %v", payload["error"])
	}
	if _, present := payload["code"]; present {
		t.Fatalf("internal error must not carry a code: %v", payload)
	}
}
