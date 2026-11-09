package routes

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// egressProfilesTestContext builds a gin context carrying a JSON body, mirroring
// the request the Fastify schema validator would have already parsed.
func egressProfilesTestContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest("POST", "/api/admin/egress-profiles", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	return context, recorder
}

func TestEgressProfilesIntegerParamMessage(t *testing.T) {
	cases := map[string]string{
		"0":            "must be >= 1",
		"-1":           "must be >= 1",
		"abc":          "must be integer",
		"1.5":          "must be integer",
		"999999999999": "must be <= 2147483647",
	}
	for raw, want := range cases {
		if got := egressProfilesIntegerParamMessage(raw, 1, 2147483647); got != want {
			t.Fatalf("message(%q) = %q, want %q", raw, got, want)
		}
	}
	// ParsePositiveRouteID rejects these even though AJV would coerce them; the
	// fallback message is the AJV "type" message.
	for _, raw := range []string{"01", "0x4", " 5 "} {
		if got := egressProfilesIntegerParamMessage(raw, 1, 2147483647); got != "must be integer" {
			t.Fatalf("message(%q) = %q", raw, got)
		}
	}
}

func TestEgressProfilesHostIDs(t *testing.T) {
	ids, message := egressProfilesHostIDs([]any{float64(3), float64(1)})
	if message != "" || len(ids) != 2 || ids[0] != 3 || ids[1] != 1 {
		t.Fatalf("ids=%v message=%q", ids, message)
	}
	// AJV validates items BEFORE uniqueItems, so the out-of-range element wins.
	if _, message := egressProfilesHostIDs([]any{float64(1), float64(1), float64(0)}); message != "must be >= 1" {
		t.Fatalf("item-before-uniqueItems = %q", message)
	}
	if _, message := egressProfilesHostIDs([]any{float64(1), float64(2), float64(1)}); message != "must NOT have duplicate items (items ## 2 and 0 are identical)" {
		t.Fatalf("duplicate message = %q", message)
	}
	if _, message := egressProfilesHostIDs([]any{}); message != "must NOT have fewer than 1 items" {
		t.Fatalf("minItems message = %q", message)
	}
	tooMany := make([]any, 0, 1001)
	for index := 0; index < 1001; index++ {
		tooMany = append(tooMany, float64(index+1))
	}
	if _, message := egressProfilesHostIDs(tooMany); message != "must NOT have more than 1000 items" {
		t.Fatalf("maxItems message = %q", message)
	}
	if _, message := egressProfilesHostIDs([]any{"x"}); message != "must be integer" {
		t.Fatalf("item type message = %q", message)
	}
	if _, message := egressProfilesHostIDs(map[string]any{"a": float64(1)}); message != "must be integer" {
		t.Fatalf("object coercion message = %q", message)
	}
	ids, message = egressProfilesHostIDs([]any{"1", "2"})
	if message != "" || len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("string coercion = %v, %q", ids, message)
	}
	ids, message = egressProfilesHostIDs(float64(7))
	if message != "" || len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("scalar coercion = %v, %q", ids, message)
	}
}

func TestEgressProfilesBodyValidation(t *testing.T) {
	validEgress := `{"mode":"proxy","strategy":"failover","proxyNodeIds":[1],"internalCidrs":[],"probeUrl":"https://1.1.1.1/cdn-cgi/trace","probeIntervalSeconds":30}`

	cases := []struct {
		name      string
		body      string
		wantError string
	}{
		{"missing-name", `{"hostIds":[1],"egress":{}}`, "name is required"},
		{"missing-hostIds", `{"name":"x","egress":{}}`, "hostIds is required"},
		{"missing-egress", `{"name":"x","hostIds":[1]}`, "egress is required"},
		{"empty-name", `{"name":"","hostIds":[1],"egress":{}}`, "name is too short"},
		{"long-name", `{"name":"` + strings.Repeat("a", 81) + `","hostIds":[1],"egress":{}}`, "name is too long"},
		{"whitespace-name", `{"name":"   ","hostIds":[1],"egress":{}}`, "name format is invalid"},
		{"empty-hosts", `{"name":"x","hostIds":[],"egress":{}}`, "must NOT have fewer than 1 items"},
		{"duplicate-hosts", `{"name":"x","hostIds":[1,1],"egress":{}}`, "must NOT have duplicate items (items ## 1 and 0 are identical)"},
		{"bad-host", `{"name":"x","hostIds":[0],"egress":{}}`, "must be >= 1"},
		{"bad-egress", `{"name":"x","hostIds":[1],"egress":[]}`, "must be object"},
		{"non-object", `[]`, "must be object"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			context, _ := egressProfilesTestContext(t, testCase.body)
			_, _, _, message, handled := egressProfilesBody(context)
			if handled {
				t.Fatalf("body unexpectedly handled by the parser")
			}
			if message != testCase.wantError {
				t.Fatalf("message = %q, want %q", message, testCase.wantError)
			}
		})
	}

	context, _ := egressProfilesTestContext(t,
		`{"name":5,"hostIds":["1","2"],"egress":`+validEgress+`,"ignored":true}`)
	name, hostIDs, egress, message, handled := egressProfilesBody(context)
	if handled || message != "" {
		t.Fatalf("valid body rejected: %q handled=%v", message, handled)
	}
	if name != "5" {
		t.Fatalf("coerced name = %q", name)
	}
	if len(hostIDs) != 2 || hostIDs[0] != 1 || hostIDs[1] != 2 {
		t.Fatalf("coerced hostIds = %v", hostIDs)
	}
	object, ok := egress.(map[string]any)
	if !ok || object["mode"] != "proxy" {
		t.Fatalf("egress document = %#v", egress)
	}
}

func TestEgressProfilesJSONHelpers(t *testing.T) {
	object := egressProfilesJSONObject([]byte(`{"bindings":[{"instanceName":"box","generation":2}],"groups":[{"key":"p3","state":"ready"}]}`))
	bindings := egressProfilesJSONArray(object, "bindings")
	if len(bindings) != 1 {
		t.Fatalf("bindings = %v", bindings)
	}
	record, _ := bindings[0].(map[string]any)
	if !egressProfilesJSONEqualsString(record["instanceName"], "box") {
		t.Fatalf("instanceName comparison failed")
	}
	if !egressProfilesJSONEqualsInt(record["generation"], 2) {
		t.Fatalf("generation comparison failed")
	}
	if egressProfilesJSONEqualsInt(record["generation"], 3) {
		t.Fatalf("generation compared equal to 3")
	}
	if egressProfilesJSONEqualsString(record["generation"], "2") {
		t.Fatalf("non-string compared equal to a string")
	}

	empty := egressProfilesJSONObject([]byte(`[]`))
	if len(empty) != 0 {
		t.Fatalf("array decoded as an object: %v", empty)
	}
	if len(egressProfilesJSONObject(nil)) != 0 {
		t.Fatalf("nil decoded as an object")
	}
	if len(egressProfilesJSONArray(egressProfilesJSONObject([]byte(`{"bindings":{}}`)), "bindings")) != 0 {
		t.Fatalf("non-array accepted")
	}
}

func TestEgressProfilesJSString(t *testing.T) {
	if egressProfilesJSString(nil) != "null" {
		t.Fatalf("nil = %q", egressProfilesJSString(nil))
	}
	five := int32(5)
	if egressProfilesJSString(&five) != "5" {
		t.Fatalf("pointer = %q", egressProfilesJSString(&five))
	}
}

func TestEgressProfilesHostFiltering(t *testing.T) {
	owned := []int32{2, 4}
	if !egressProfilesAnyIn([]int32{1, 4}, owned) {
		t.Fatalf("membership missed")
	}
	if egressProfilesAnyIn([]int32{1, 3}, owned) {
		t.Fatalf("false membership")
	}
	filtered := egressProfilesFilterIn([]int32{1, 2, 3, 4}, owned)
	if len(filtered) != 2 || filtered[0] != 2 || filtered[1] != 4 {
		t.Fatalf("filter = %v", filtered)
	}
	if filtered := egressProfilesFilterIn([]int32{1, 3}, owned); filtered == nil || len(filtered) != 0 {
		t.Fatalf("empty filter must be a non-nil slice, got %#v", filtered)
	}
}

func TestEgressProfilesBodyParserErrors(t *testing.T) {
	// The shared body-parser middleware normally rejects these before the handler;
	// the handler still mirrors Fastify's ordering when it sees them.
	gin.SetMode(gin.TestMode)
	for _, testCase := range []struct{ body, contentType string }{
		{``, "application/json"},
		{`{`, "application/json"},
		{`{"name":"x"}`, "text/csv"},
	} {
		request := httptest.NewRequest("POST", "/api/admin/egress-profiles", strings.NewReader(testCase.body))
		if testCase.contentType != "" {
			request.Header.Set("Content-Type", testCase.contentType)
		}
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request

		_, _, _, _, handled := egressProfilesBody(context)
		if !handled {
			t.Fatalf("body %q (%s) was not rejected by the parser", testCase.body, testCase.contentType)
		}
		var payload map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if payload["code"] != "VALIDATION_ERROR" {
			t.Fatalf("response = %s", recorder.Body.String())
		}
	}
}
