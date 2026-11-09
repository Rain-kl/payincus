package routes

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/services"
)

func TestVipBenefitsParseRewardID(t *testing.T) {
	cases := []struct {
		raw            string
		want           int32
		wantOK         bool
		wantOutOfRange bool
	}{
		{"1", 1, true, false},
		{"2147483647", 2147483647, true, false},
		{"0", 0, false, false},
		{"01", 0, false, false},
		{"-1", 0, false, false},
		{"abc", 0, false, false},
		{"1.5", 0, false, false},
		{"2147483648", 0, true, true}, // parses, but Prisma P2020 → 500
	}
	for _, tc := range cases {
		got, ok, outOfRange := vipBenefitsParseRewardID(tc.raw)
		if ok != tc.wantOK || outOfRange != tc.wantOutOfRange || (ok && got != tc.want) {
			t.Errorf("id=%q: got (%d, %v, %v), want (%d, %v, %v)", tc.raw, got, ok, outOfRange, tc.want, tc.wantOK, tc.wantOutOfRange)
		}
	}
}

func TestVipBenefitsWriteError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	vipBenefitsWriteError(context, &services.VipBenefitError{Code: "CLAIM_LIMIT_REACHED", Message: "already claimed", StatusCode: 409})
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if recorder.Code != http.StatusConflict || body["error"] != "already claimed" || body["code"] != "CLAIM_LIMIT_REACHED" {
		t.Fatalf("status=%d body=%v", recorder.Code, body)
	}

	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	vipBenefitsWriteError(context, errors.New("boom"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("generic error status = %d, want 500", recorder.Code)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != "Internal server error" {
		t.Fatalf("generic error body = %v", body)
	}
}
