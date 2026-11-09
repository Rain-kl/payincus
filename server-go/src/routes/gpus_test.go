package routes

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
