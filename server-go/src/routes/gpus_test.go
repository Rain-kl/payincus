package routes

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
)

func newParamContext(params gin.Params) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = params
	return context, recorder
}

// The params schema is `{type:integer, minimum:1, maximum:2147483647}` and Fastify
// runs it with AJV's coerceTypes, so URL strings are coerced — verified against
// fastify@5's default AJV options.
func TestGpusValidateRouteID(t *testing.T) {
	cases := []struct {
		raw        string
		wantOK     bool
		wantID     int32
		wantErrMsg string
	}{
		{"7", true, 7, ""},
		{"01", true, 1, ""}, // AJV coerces leading zeros
		{"abc", false, 0, "must be integer"},
		{"1.5", false, 0, "must be integer"},
		{"0", false, 0, "must be >= 1"},
		{"-1", false, 0, "must be >= 1"},
		{"2147483647", true, 2147483647, ""},
		{"2147483648", false, 0, "must be <= 2147483647"},
	}
	for _, tc := range cases {
		context, recorder := newParamContext(gin.Params{{Key: "id", Value: tc.raw}})
		id, ok := gpusValidateRouteID(context, "id")
		if ok != tc.wantOK || (ok && id != tc.wantID) {
			t.Errorf("id=%q: got (%d, %v), want (%d, %v)", tc.raw, id, ok, tc.wantID, tc.wantOK)
			continue
		}
		if !tc.wantOK {
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("id=%q: response is not JSON: %v", tc.raw, err)
			}
			if body["error"] != tc.wantErrMsg || body["code"] != "VALIDATION_ERROR" {
				t.Errorf("id=%q: body = %v, want error=%q code=VALIDATION_ERROR", tc.raw, body, tc.wantErrMsg)
			}
		}
	}
}

// gpuId uses the same schema; the second parameter must be validated independently.
func TestGpusValidateRouteIDUsesParamName(t *testing.T) {
	context, recorder := newParamContext(gin.Params{{Key: "gpuId", Value: "9"}})
	id, ok := gpusValidateRouteID(context, "gpuId")
	if !ok || id != 9 {
		t.Fatalf("gpuId=9: got (%d, %v)", id, ok)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("unexpected body for a valid id: %s", recorder.Body.String())
	}
}

func TestGpusParsePutInstanceGpuBody(t *testing.T) {
	body, msg := gpusParsePutInstanceGpuBody(map[string]any{
		"gpuShared":    true,
		"gpuDeviceIds": []any{float64(1), "2"},
	})
	if msg != "" || body["gpuShared"] != true {
		t.Fatalf("valid payload rejected: body=%v msg=%q", body, msg)
	}
	ids, _ := body["gpuDeviceIds"].([]any)
	if len(ids) != 2 || ids[0] != float64(1) || ids[1] != float64(2) {
		t.Fatalf("gpuDeviceIds = %#v", ids)
	}

	if _, msg := gpusParsePutInstanceGpuBody(map[string]any{}); msg != "gpuShared is required" {
		t.Fatalf("missing required msg = %q", msg)
	}
	if _, msg := gpusParsePutInstanceGpuBody(map[string]any{"gpuShared": false, "extra": 1}); msg != "must NOT have additional properties" {
		t.Fatalf("additionalProperties msg = %q", msg)
	}
	if _, msg := gpusParsePutInstanceGpuBody(map[string]any{"gpuShared": true, "gpuDeviceIds": "bad"}); msg != "must be array" {
		t.Fatalf("non-array msg = %q", msg)
	}
	over64 := make([]any, 65)
	for i := range over64 {
		over64[i] = float64(i + 1)
	}
	if _, msg := gpusParsePutInstanceGpuBody(map[string]any{"gpuShared": true, "gpuDeviceIds": over64}); msg != "must NOT have more than 64 items" {
		t.Fatalf("maxItems msg = %q", msg)
	}
	if _, msg := gpusParsePutInstanceGpuBody(map[string]any{"gpuShared": true, "gpuDeviceIds": []any{float64(0)}}); msg != "must be >= 1" {
		t.Fatalf("item minimum msg = %q", msg)
	}
}

func TestGpusManagementAndStatusHelpers(t *testing.T) {
	if code := gpusIncusStatusCode(404); code != 404 {
		t.Fatalf("404 mapped to %d", code)
	}
	if code := gpusIncusStatusCode(429); code != 502 {
		t.Fatalf("429 mapped to %d, want 502", code)
	}
	view := gpusManagementView{
		CanManage:          true,
		AllocatedDeviceIDs: []int32{2},
		HostGpus:           []db.GpusHostGpu{},
		LiveGpuDevices:     []gpusLiveGpuDeviceView{{Name: "gpu0", HostMatched: true}},
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	for _, key := range []string{"canManage", "instanceId", "hostId", "instanceStatus", "instanceType", "privileged", "supported", "allocatedDeviceIds", "reservationReleased", "hostGpus", "liveGpuDevices", "liveSyncError"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing management JSON key %q", key)
		}
	}
}
