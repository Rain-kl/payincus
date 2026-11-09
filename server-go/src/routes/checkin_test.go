package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// ==================== harness ====================

// checkinTestRecorder builds a bare Gin context: the handlers under test here never
// touch the database, so no auth middleware or DB is required.
func checkinTestRecorder(t *testing.T, method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, target, nil)
	return context, recorder
}

func checkinDecodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

// ==================== /redeem body schema ====================

// TestCheckinRedeemBodyValidation pins the Fastify schema of POST /redeem:
// required ['redeemCode','instanceId'], redeemCode 5..30 chars, instanceId
// integer >= 1, allErrors:false (first failure wins) and coerceTypes:'array'.
func TestCheckinRedeemBodyValidation(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
		wantOk      bool
	}{
		{"empty object", `{}`, "redeemCode is required", false},
		{"missing instance id", `{"redeemCode":"h-12345"}`, "instanceId is required", false},
		{"too short", `{"redeemCode":"h-12","instanceId":1}`, "redeemCode is too short", false},
		{"too long", `{"redeemCode":"` + "h-12345678901234567890123456789" + `","instanceId":1}`, "redeemCode is too long", false},
		{"instance id zero", `{"redeemCode":"h-12345","instanceId":0}`, "must be >= 1", false},
		{"instance id negative", `{"redeemCode":"h-12345","instanceId":-3}`, "must be >= 1", false},
		{"instance id fractional", `{"redeemCode":"h-12345","instanceId":1.5}`, "must be integer", false},
		{"instance id string", `{"redeemCode":"h-12345","instanceId":"42"}`, "", true},
		{"instance id boolean", `{"redeemCode":"h-12345","instanceId":true}`, "", true},
		{"instance id null", `{"redeemCode":"h-12345","instanceId":null}`, "must be >= 1", false},
		{"code coerced from number", `{"redeemCode":123456,"instanceId":1}`, "", true},
		{"valid", `{"redeemCode":"h-12345","instanceId":7}`, "", true},
		{"root single-element array is not unwrapped", `[{"redeemCode":"h-12345","instanceId":7}]`, "must be object", false},
		{"root two-element array", `[{"redeemCode":"h-12345","instanceId":7},{"redeemCode":"h-12345","instanceId":7}]`, "must be object", false},
		{"unknown fields kept", `{"redeemCode":"h-12345","instanceId":7,"extra":true}`, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded any
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			validated, message := validateCheckinRedeemBodyAJV(decoded)
			if tc.wantOk {
				if message != "" {
					t.Fatalf("want no message, got %q", message)
				}
				if _, present := validated.Present["instanceId"]; !present {
					t.Fatal("instanceId should be present after validation")
				}
				return
			}
			if message != tc.wantMessage {
				t.Fatalf("want %q, got %q", tc.wantMessage, message)
			}
		})
	}
}

// TestCheckinRedeemBodyNonObject pins the root type check.
func TestCheckinRedeemBodyNonObject(t *testing.T) {
	for _, body := range []string{`"nope"`, `123`, `null`, `true`} {
		var decoded any
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("invalid fixture %s: %v", body, err)
		}
		if _, message := validateCheckinRedeemBodyAJV(decoded); message != "must be object" {
			t.Fatalf("%s: want %q, got %q", body, "must be object", message)
		}
	}
}

// ==================== claim error mapping ====================

// TestCheckinClaimErrorMapping pins the catch block of POST /checkin for the three
// branches that do not write an audit log.
func TestCheckinClaimErrorMapping(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantError  string
		wantCode   string
		wantDetail string
	}{
		{errString("CHECKIN_ALREADY_TODAY"), http.StatusBadRequest, "You have already checked in today", "CHECKIN_ALREADY_TODAY", ""},
		{errString("CHECKIN_NO_INSTANCE"), http.StatusBadRequest, "You need at least one instance to check in", "CHECKIN_NO_INSTANCE", ""},
		{errString("CHECKIN_USER_BLOCKED"), http.StatusForbidden, "Access denied", "FORBIDDEN", "账号状态异常，无法签到"},
	}

	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			context, recorder := checkinTestRecorder(t, http.MethodPost, "/api/checkin/checkin")
			checkinRespondClaimError(context, 1, tc.err)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			body := checkinDecodeResponse(t, recorder)
			if body["error"] != tc.wantError {
				t.Fatalf("error = %v, want %v", body["error"], tc.wantError)
			}
			if body["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %v", body["code"], tc.wantCode)
			}
			if tc.wantDetail == "" {
				if _, present := body["details"]; present {
					t.Fatalf("unexpected details: %v", body["details"])
				}
			} else if body["details"] != tc.wantDetail {
				t.Fatalf("details = %v, want %v", body["details"], tc.wantDetail)
			}
		})
	}
}

// ==================== redeem error mapping ====================

// TestCheckinRedeemErrorMapping pins the catch block of POST /redeem. The default
// branch writes an audit log (and therefore needs a DB) and is covered by the
// dual-run conformance suite instead.
func TestCheckinRedeemErrorMapping(t *testing.T) {
	cases := []struct {
		message     string
		wantStatus  int
		wantError   string
		wantCode    string
		wantDetails string
		wantRawBody bool
	}{
		{"REDEEM_CODE_NOT_FOUND", http.StatusNotFound, "Redeem code not found", "REDEEM_CODE_NOT_FOUND", "", false},
		{"REDEEM_CODE_DISABLED", http.StatusBadRequest, "This redeem code has been disabled", "REDEEM_CODE_DISABLED", "", false},
		{"REDEEM_CODE_EXPIRED", http.StatusBadRequest, "Redeem code has expired", "REDEEM_CODE_EXPIRED", "", false},
		{"REDEEM_CODE_INVALID_FORMAT", http.StatusBadRequest, "Invalid code format, only h- prefix codes are supported", "REDEEM_CODE_INVALID_FORMAT", "", false},
		{"REDEEM_CODE_EXHAUSTED", http.StatusBadRequest, "This redeem code has reached its maximum usage limit", "REDEEM_CODE_EXHAUSTED", "", false},
		{"REDEEM_CODE_ALREADY_USED_BY_USER", http.StatusBadRequest, "You have already used this redeem code", "REDEEM_CODE_ALREADY_USED_BY_USER", "", false},
		{"REDEEM_CODE_BATCH_LIMIT", http.StatusBadRequest, "You have already used a redeem code from this batch", "REDEEM_CODE_BATCH_LIMIT", "", false},
		{"INSTANCE_NOT_FOUND", http.StatusNotFound, "Instance not found", "INSTANCE_NOT_FOUND", "", false},
		{"FORBIDDEN", http.StatusForbidden, "Access denied", "FORBIDDEN", "", false},
		{"REDEEM_CODE_TARGET_USER_MISMATCH", http.StatusForbidden, "Access denied", "FORBIDDEN", "This redeem code is assigned to another user", false},
		{"INSTANCE_STATUS_INVALID", http.StatusBadRequest, "Instance status does not allow this operation", "INSTANCE_STATUS_INVALID", "", false},
		{"REDEEM_CODE_HOST_MISMATCH", http.StatusBadRequest, "This redeem code can only be used on instances from the same host", "REDEEM_CODE_HOST_MISMATCH", "", false},
		{"HOST_NOT_FOUND", http.StatusNotFound, "Host not found", "HOST_NOT_FOUND", "", false},
		{"REDEEM_CODE_BUSY", http.StatusConflict, "", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			context, recorder := checkinTestRecorder(t, http.MethodPost, "/api/checkin/redeem")
			checkinRespondRedeemError(context, 1, 9, "h-abcde", errString(tc.message))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			body := checkinDecodeResponse(t, recorder)
			if tc.wantRawBody {
				// {error:'REDEEM_CODE_BUSY', message:'...'} — NOT the apiError envelope.
				if body["error"] != "REDEEM_CODE_BUSY" {
					t.Fatalf("error = %v", body["error"])
				}
				if body["message"] != "Redeem code is busy, please retry" {
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

// TestCheckinRedeemLockKeys pins the distributed-lock names of the redeem flow.
func TestCheckinRedeemLockKeys(t *testing.T) {
	if got := checkinRedeemLockKeys(11, 7, nil); len(got) != 1 || got[0] != "redeem-code:11" {
		t.Fatalf("no batch: %v", got)
	}
	batch := "abc123"
	got := checkinRedeemLockKeys(11, 7, &batch)
	if len(got) != 2 || got[0] != "redeem-code:11" || got[1] != "redeem-batch:abc123:user:7" {
		t.Fatalf("batch: %v", got)
	}
	empty := ""
	if got := checkinRedeemLockKeys(11, 7, &empty); len(got) != 1 {
		t.Fatalf("empty batch id must not add a lock: %v", got)
	}
}

// ==================== misc ====================

// TestCheckinRedeemTypeLabel pins the log label mapping.
func TestCheckinRedeemTypeLabel(t *testing.T) {
	cases := map[string][2]string{
		"c": {"CPU", "%"},
		"r": {"Memory", "MB"},
		"d": {"Disk", "MB"},
		"t": {"Traffic", "GB"},
		"p": {"p", ""},
	}
	for codeType, expected := range cases {
		name, unit := checkinRedeemTypeLabel(codeType)
		if name != expected[0] || unit != expected[1] {
			t.Fatalf("%s: got (%q,%q), want (%q,%q)", codeType, name, unit, expected[0], expected[1])
		}
	}
}

// errString is a local errors.New replacement to keep the table literals compact.
type basicError string

func (e basicError) Error() string { return string(e) }

func errString(message string) error { return basicError(message) }
