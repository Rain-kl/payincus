// Unit tests for the routes/users.go port (server/src/routes/users.ts).
//
// DB-backed behaviour is covered by the dual-run conformance suite
// (server-go/test/conformance/requests-users.json). What is pinned here:
//
//   - the full route table (17 handlers; the list route is mounted twice because
//     Fastify's prefixTrailingSlash is 'both');
//   - registering this module together with routes/traffic.go does not panic, which
//     is only true when the /api/users wildcard matches traffic's ':userId';
//   - the AJV replica semantics the TypeScript schemas depend on: validation order,
//     required-before-properties, the password minLength message override, the
//     `format: email` message, and "must be object" for a missing body;
//   - the query parsers, email-target validation short circuits and demo redaction;
//   - the change_email verification-code helpers (hashing + single use).
package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== registration ====================

func TestUsersRegistersAllRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterUsers(router)

	prefix := "/api/users"
	expected := map[string]bool{
		"GET " + prefix:                                      true, // Fastify prefixTrailingSlash: both
		"GET " + prefix + "/":                                true,
		"GET " + prefix + "/lookup":                          true,
		"GET " + prefix + "/detect-linked-accounts":          true,
		"GET " + prefix + "/:userId":                         true,
		"POST " + prefix + "/:userId/change-email/send-code": true,
		"PATCH " + prefix + "/:userId":                       true,
		"PATCH " + prefix + "/:userId/role":                  true,
		"POST " + prefix + "/:userId/quota/recalculate":      true,
		"POST " + prefix + "/me/quota/increase":              true,
		"PATCH " + prefix + "/:userId/status":                true,
		"DELETE " + prefix + "/:userId":                      true,
		"GET " + prefix + "/:userId/sessions":                true,
		"POST " + prefix + "/:userId/revoke-sessions":        true,
		"POST " + prefix + "/:userId/reset-password":         true,
		"POST " + prefix + "/:userId/disable-2fa":            true,
		"DELETE " + prefix + "/:userId/oauth/:provider":      true,
		"GET " + prefix + "/:userId/login-records":           true,
	}

	seen := map[string]bool{}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if !expected[key] {
			t.Errorf("unexpected route %s", key)
		}
		seen[key] = true
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("missing route %s", key)
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("registered %d routes, want %d", len(seen), len(expected))
	}
}

// TestUsersRoutesCoexistWithTraffic pins the gin restriction that one wildcard
// name is allowed per path position. routes/traffic.go already owns
// "/api/users/:userId/traffic"; spelling this module's parameter ":id" panics at
// startup. This test fails loudly if someone renames it.
func TestUsersRoutesCoexistWithTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// traffic.go registers first in production (file-name init order), so this is
	// the exact ordering that used to panic.
	RegisterTraffic(router)
	RegisterUsers(router)

	for _, route := range router.Routes() {
		if route.Path == "/api/users/:userId/traffic" {
			return // traffic route survived: the trees merged
		}
	}
	t.Fatal("traffic /api/users/:userId/traffic route disappeared")
}

// TestUsersRoutesRequireAuth proves every route is wired to an auth middleware:
// anonymous requests must be 401, never 404.
func TestUsersRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterUsers(router)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/users"},
		{http.MethodGet, "/api/users/"},
		{http.MethodGet, "/api/users/lookup"},
		{http.MethodGet, "/api/users/detect-linked-accounts"},
		{http.MethodGet, "/api/users/1"},
		{http.MethodPost, "/api/users/1/change-email/send-code"},
		{http.MethodPatch, "/api/users/1"},
		{http.MethodPatch, "/api/users/1/role"},
		{http.MethodPost, "/api/users/1/quota/recalculate"},
		{http.MethodPost, "/api/users/me/quota/increase"},
		{http.MethodPatch, "/api/users/1/status"},
		{http.MethodDelete, "/api/users/1"},
		{http.MethodGet, "/api/users/1/sessions"},
		{http.MethodPost, "/api/users/1/revoke-sessions"},
		{http.MethodPost, "/api/users/1/reset-password"},
		{http.MethodPost, "/api/users/1/disable-2fa"},
		{http.MethodDelete, "/api/users/1/oauth/github"},
		{http.MethodGet, "/api/users/1/login-records"},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(testCase.method, testCase.path, nil)
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", testCase.method, testCase.path, recorder.Code)
		}
	}
}

// ==================== query parsing ====================

func TestUsersParseUserSearchFields(t *testing.T) {
	all := []string{"username", "id", "email"}
	cases := []struct {
		raw  *string
		want []string
	}{
		{nil, all},
		{usersStringPtr(""), all},
		{usersStringPtr("email"), []string{"email"}},
		{usersStringPtr(" email , username "), []string{"username", "email"}}, // canonical order
		{usersStringPtr("bogus"), all},
		{usersStringPtr("bogus,email"), []string{"email"}},
		{usersStringPtr("id,username,email"), all},
	}
	for _, testCase := range cases {
		got := usersParseUserSearchFields(testCase.raw)
		if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
			t.Fatalf("parseUserSearchFields(%v) = %v, want %v", formatUsersTestStringPtr(testCase.raw), got, testCase.want)
		}
	}
}

func TestUsersParseBooleanQuery(t *testing.T) {
	for _, truthy := range []string{"1", "true", "TRUE", "yes", "on"} {
		if !usersParseBooleanQuery(&truthy) {
			t.Fatalf("%q must parse as true", truthy)
		}
	}
	for _, falsy := range []string{"", "0", "false", "no", "off", "anything"} {
		if usersParseBooleanQuery(&falsy) {
			t.Fatalf("%q must parse as false", falsy)
		}
	}
	if usersParseBooleanQuery(nil) {
		t.Fatal("nil must parse as false")
	}
}

func TestUsersParsePositiveIntegerQueries(t *testing.T) {
	cases := []struct {
		raw      string
		fallback int
		want     int
	}{
		{"", 1, 1},
		{"0", 1, 1}, // pattern rejects 0
		{"-1", 20, 20},
		{"01", 20, 20}, // pattern rejects leading zeros
		{"7", 20, 7},
		{"abc", 20, 20},
		{"9999999999999999999999", 20, 20}, // > JS safe integer
	}
	for _, testCase := range cases {
		if got := usersParsePositiveIntegerQuery(testCase.raw, testCase.fallback); got != testCase.want {
			t.Fatalf("parsePositiveIntegerQuery(%q, %d) = %d, want %d", testCase.raw, testCase.fallback, got, testCase.want)
		}
	}

	if got := usersParseClampedPositiveIntegerQuery("500", 20, 100); got != 100 {
		t.Fatalf("clamped 500 -> %d, want 100", got)
	}
	if got := usersParseClampedPositiveIntegerQuery("37", 20, 50); got != 37 {
		t.Fatalf("clamped 37 -> %d, want 37", got)
	}
}

// ==================== email helpers ====================

func TestUsersEmailFormat(t *testing.T) {
	valid := []string{"a@b.co", "user.name+tag@example.com", "USER@EXAMPLE.COM", "x!#$%&'*+/=?^_`{|}~-@example.com"}
	for _, email := range valid {
		if !usersEmailFormatValid(email) {
			t.Fatalf("%q must be a valid email", email)
		}
	}
	// "a@b" is accepted by ajv-formats' full email pattern (the domain needs no dot).
	invalid := []string{"", "no-at", "a@", "@b.co", "a b@c.co", "a@-b.co", "a@b..co"}
	for _, email := range invalid {
		if usersEmailFormatValid(email) {
			t.Fatalf("%q must be rejected", email)
		}
	}
}

func TestUsersExtractEmailDomain(t *testing.T) {
	cases := map[string]string{
		"a@b.co":       "b.co",
		"A@B.CO":       "b.co",
		"a@" + "@b.co": "b.co", // lastIndexOf
		"no-at":        "",
		"a@":           "",
	}
	for email, want := range cases {
		if got := usersExtractEmailDomain(email); got != want {
			t.Fatalf("extractEmailDomain(%q) = %q, want %q", email, got, want)
		}
	}
}

// TestUsersValidateEmailChangeTargetShortCircuits covers the branches that run
// before any database access (the DB-backed branches are conformance cases).
func TestUsersValidateEmailChangeTargetShortCircuits(t *testing.T) {
	ctx := context.Background()
	current := "old@example.com"

	_, validationErr, err := usersValidateEmailChangeTarget(ctx, "", &current, 1)
	if err != nil || validationErr == nil || validationErr.Code != lib.ErrInvalidEmail {
		t.Fatalf("empty email -> %v / %v, want INVALID_EMAIL", validationErr, err)
	}

	_, validationErr, err = usersValidateEmailChangeTarget(ctx, "example.com", &current, 1)
	if err != nil || validationErr == nil || validationErr.Code != lib.ErrInvalidEmail {
		t.Fatalf("no @ -> %v / %v, want INVALID_EMAIL", validationErr, err)
	}

	_, validationErr, err = usersValidateEmailChangeTarget(ctx, "a<b@example.com", &current, 1)
	if err != nil || validationErr == nil || validationErr.Code != lib.ErrEmailContainsIllegal {
		t.Fatalf("dangerous chars -> %v / %v, want EMAIL_CONTAINS_ILLEGAL", validationErr, err)
	}

	_, validationErr, err = usersValidateEmailChangeTarget(ctx, " OLD@Example.com ", &current, 1)
	if err != nil || validationErr == nil || validationErr.Code != lib.ErrInvalidParams {
		t.Fatalf("same email -> %v / %v, want INVALID_PARAMS", validationErr, err)
	}
	if validationErr.Details != "New email must be different from current email" {
		t.Fatalf("details = %q", validationErr.Details)
	}
}

// ==================== body validation ====================

func TestUsersBodyObject(t *testing.T) {
	if _, message := usersBodyObject(nil); message != "must be object" {
		t.Fatalf("nil body message = %q", message)
	}
	if _, message := usersBodyObject([]any{}); message != "must be object" {
		t.Fatalf("array body message = %q", message)
	}
	if _, message := usersBodyObject("text"); message != "must be object" {
		t.Fatalf("string body message = %q", message)
	}
	if object, message := usersBodyObject(map[string]any{}); message != "" || object == nil {
		t.Fatalf("object body = %v / %q", object, message)
	}
}

func TestUsersValidateChangeEmailBody(t *testing.T) {
	cases := []struct {
		name    string
		body    any
		email   string
		message string
	}{
		{"missing body", nil, "", "must be object"},
		{"array", []any{}, "", "must be object"},
		{"missing email", map[string]any{}, "", "email is required"},
		{"null email", map[string]any{"email": nil}, "", "Please enter a valid email address"},
		{"bad email", map[string]any{"email": "nope"}, "", "Please enter a valid email address"},
		{"number email", map[string]any{"email": float64(123)}, "", "Please enter a valid email address"},
		{"object email", map[string]any{"email": map[string]any{}}, "", "must be string"},
		{"ok", map[string]any{"email": "A@B.co"}, "A@B.co", ""},
		{"extra keys survive", map[string]any{"email": "a@b.co", "x": 1}, "a@b.co", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			email, message := usersValidateChangeEmailBody(testCase.body)
			if message != testCase.message || email != testCase.email {
				t.Fatalf("got (%q, %q), want (%q, %q)", email, message, testCase.email, testCase.message)
			}
		})
	}
}

func TestUsersValidatePatchBody(t *testing.T) {
	t.Run("missing body", func(t *testing.T) {
		_, _, _, _, _, message := usersValidatePatchBody(nil)
		if message != "must be object" {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("email is validated first", func(t *testing.T) {
		// email format error outranks a too-short password (schema order).
		_, _, _, _, _, message := usersValidatePatchBody(map[string]any{
			"email": "bad", "password": "x",
		})
		if message != "Please enter a valid email address" {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("password min length uses the app.ts override", func(t *testing.T) {
		// app.ts rewrites every minLength failure on `password` to this text, even
		// though the schema says minLength 6.
		_, _, _, _, _, message := usersValidatePatchBody(map[string]any{"password": "12345"})
		if message != "Password must be at least 8 characters" {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("emailCode length", func(t *testing.T) {
		_, _, _, _, _, message := usersValidatePatchBody(map[string]any{"emailCode": "12345"})
		if message != "emailCode is too short" {
			t.Fatalf("message = %q", message)
		}
		_, _, _, _, _, message = usersValidatePatchBody(map[string]any{"emailCode": "1234567"})
		if message != "emailCode is too long" {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("avatarStyle enum", func(t *testing.T) {
		_, _, _, _, _, message := usersValidatePatchBody(map[string]any{"avatarStyle": "nope"})
		if message != "must be equal to one of the allowed values" {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("empty string avatarStyle is allowed", func(t *testing.T) {
		_, _, _, _, avatarStyle, message := usersValidatePatchBody(map[string]any{"avatarStyle": ""})
		if message != "" || avatarStyle == nil || *avatarStyle != "" {
			t.Fatalf("got (%v, %q)", avatarStyle, message)
		}
	})

	t.Run("coerces scalars like AJV", func(t *testing.T) {
		email, emailPresent, password, _, avatarStyle, message := usersValidatePatchBody(map[string]any{
			"email": "a@b.co", "password": float64(123456), "avatarStyle": "rings",
		})
		if message != "" {
			t.Fatalf("message = %q", message)
		}
		if !emailPresent || email != "a@b.co" {
			t.Fatalf("email = %q/%v", email, emailPresent)
		}
		if password == nil || *password != "123456" {
			t.Fatalf("password = %v", password)
		}
		if avatarStyle == nil || *avatarStyle != "rings" {
			t.Fatalf("avatarStyle = %v", avatarStyle)
		}

		// A boolean is coerced to "true", which is then rejected by the enum.
		_, _, _, _, _, message = usersValidatePatchBody(map[string]any{"avatarStyle": true})
		if message != "must be equal to one of the allowed values" {
			t.Fatalf("boolean avatarStyle message = %q", message)
		}
	})

	t.Run("empty object passes (no required fields)", func(t *testing.T) {
		_, emailPresent, password, emailCode, avatarStyle, message := usersValidatePatchBody(map[string]any{})
		if message != "" || emailPresent || password != nil || emailCode != nil || avatarStyle != nil {
			t.Fatalf("got (%v %v %v %v %q)", emailPresent, password, emailCode, avatarStyle, message)
		}
	})
}

func TestUsersAJVMessage(t *testing.T) {
	if got := usersAJVMessage("password is too short"); got != "Password must be at least 8 characters" {
		t.Fatalf("password override = %q", got)
	}
	if got := usersAJVMessage("emailCode is too short"); got != "emailCode is too short" {
		t.Fatalf("passthrough = %q", got)
	}
}

func TestUsersAvatarStyleEnum(t *testing.T) {
	enum := usersAvatarStyleEnum()
	if len(enum) != len(db.AvatarStyles)+1 || enum[0] != "" {
		t.Fatalf("enum = %v", enum)
	}
	for index, style := range db.AvatarStyles {
		if enum[index+1] != style {
			t.Fatalf("enum[%d] = %q, want %q", index+1, enum[index+1], style)
		}
	}
}

func TestUsersRawBodyField(t *testing.T) {
	body := map[string]any{"currentPassword": "secret", "other": 1}
	if value, present := usersRawBodyField(body, "currentPassword"); !present || value != "secret" {
		t.Fatalf("currentPassword = %q/%v", value, present)
	}
	if _, present := usersRawBodyField(body, "other"); present {
		t.Fatal("non-string property must be treated as undefined")
	}
	if _, present := usersRawBodyField(body, "missing"); present {
		t.Fatal("missing property must be absent")
	}
	if _, present := usersRawBodyField("not-an-object", "currentPassword"); present {
		t.Fatal("non-object body must be absent")
	}
}

func TestUsersEmailEquals(t *testing.T) {
	stored := "a@b.co"
	if !usersEmailEquals("a@b.co", &stored) {
		t.Fatal("equal emails")
	}
	if usersEmailEquals("A@B.CO", &stored) {
		t.Fatal("comparison is case-sensitive upstream")
	}
	if usersEmailEquals("a@b.co", nil) {
		t.Fatal("nil stored email is never equal")
	}
}

// ==================== demo redaction ====================

func TestUsersRedactDemoLoginRecord(t *testing.T) {
	country, region, city, isp, timezone, agent := "CN", "GD", "SZ", "ISP", "Asia/Shanghai", "curl"
	record := db.LoginRecordItem{
		IP: "1.2.3.4", Country: &country, Region: &region, City: &city,
		Isp: &isp, Timezone: &timezone, UserAgent: &agent,
	}
	redacted := usersRedactDemoLoginRecord(record)
	if redacted.IP != lib.DemoRedactedIP {
		t.Fatalf("ip = %q, want %q", redacted.IP, lib.DemoRedactedIP)
	}
	if redacted.Country != nil || redacted.Region != nil || redacted.City != nil ||
		redacted.Isp != nil || redacted.Timezone != nil || redacted.UserAgent != nil {
		t.Fatalf("fields were not nulled: %#v", redacted)
	}
}

func TestUsersShouldRedactDemoLoginRecords(t *testing.T) {
	gin.SetMode(gin.TestMode)

	build := func(host string) *gin.Context {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		request.Host = host
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request
		return context
	}

	demoEmail := "demo@payincus.com"
	demoUser := &db.UserRecord{Username: "someone", Email: &demoEmail}
	if !usersShouldRedactDemoLoginRecords(build("demo.payincus.com"), demoUser) {
		t.Fatal("demo host + demo email must redact")
	}

	normalEmail := "user@example.com"
	normalUser := &db.UserRecord{Username: "someone", Email: &normalEmail}
	if usersShouldRedactDemoLoginRecords(build("demo.payincus.com"), normalUser) {
		t.Fatal("non-demo account must not redact")
	}
	if usersShouldRedactDemoLoginRecords(build("pay.payincus.com"), demoUser) {
		t.Fatal("non-demo host must not redact")
	}
}

// ==================== verification codes ====================

func TestUsersGenerateVerificationCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		code, err := usersGenerateVerificationCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("code = %q, want 6 digits", code)
		}
		value := 0
		for _, char := range code {
			if char < '0' || char > '9' {
				t.Fatalf("code = %q, want digits only", code)
			}
			value = value*10 + int(char-'0')
		}
		if value < 100000 || value > 999999 {
			t.Fatalf("code %q out of the crypto.randomInt(100000, 1000000) range", code)
		}
		seen[code] = true
	}
	if len(seen) < 16 {
		t.Fatalf("only %d distinct codes out of 32; the generator looks non-random", len(seen))
	}
}

func TestUsersHashVerificationCode(t *testing.T) {
	hash := usersHashVerificationCode("123456")
	if !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64 {
		t.Fatalf("hash = %q", hash)
	}
	if hash == usersHashVerificationCode("654321") {
		t.Fatal("distinct codes hash identically")
	}
	if !usersVerificationCodeMatches(hash, "123456") {
		t.Fatal("hashed code must verify")
	}
	if usersVerificationCodeMatches(hash, "654321") {
		t.Fatal("wrong code verified")
	}
	// Legacy plaintext rows are still accepted (verificationCodeMatches).
	if !usersVerificationCodeMatches("123456", "123456") {
		t.Fatal("legacy plaintext code must verify")
	}
}

// ==================== role transaction preconditions ====================

func TestUsersUpdateUserRoleRequiresDatabase(t *testing.T) {
	previous := db.DB
	db.DB = nil
	defer func() { db.DB = previous }()

	if _, err := usersUpdateUserRole(context.Background(), 1, models.UserRoleAdmin); err == nil {
		t.Fatal("expected an error without a database")
	}
}

// ==================== key JSON shapes ====================

// TestUsersListQuotaViewJSON pins the always-present quota object with its
// documented defaults (`|| 0`, `|| '0'`, `|| 'NORMAL'`).
func TestUsersListQuotaViewJSON(t *testing.T) {
	quota := usersListQuotaView{
		MonthlyTrafficUsed: "0",
		ExtraTrafficQuota:  "0",
		TrafficStatus:      models.TrafficStatusNORMAL,
	}
	encoded, err := json.Marshal(quota)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"hostLimit", "hostUsed", "friendLimit", "friendUsed", "packageLimit", "packageUsed",
		"monthlyTrafficLimit", "monthlyTrafficUsed", "extraTrafficQuota", "trafficStatus",
	} {
		if _, present := decoded[key]; !present {
			t.Fatalf("missing key %q in %s", key, encoded)
		}
	}
	if decoded["monthlyTrafficLimit"] != nil {
		t.Fatalf("monthlyTrafficLimit = %v, want null", decoded["monthlyTrafficLimit"])
	}
	if decoded["trafficStatus"] != "NORMAL" {
		t.Fatalf("trafficStatus = %v", decoded["trafficStatus"])
	}
}

// usersStringPtr is local to these tests.
func usersStringPtr(value string) *string { return &value }

func formatUsersTestStringPtr(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

// ==================== handler ordering ====================

// usersTestContext builds a gin context with a fake admin claim, so the handlers
// can be called without the auth middleware (which needs a database).
func usersTestContext(t *testing.T, method, target, body string, claims *lib.AccessTokenClaims) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	context.Request = request
	if claims != nil {
		context.Set(middleware.ContextUserKey, claims)
	}
	return context, recorder
}

// TestUsersSchemaValidationPrecedesRouteID pins Fastify's ordering: the body
// schema is a preValidation hook, so it fails before the handler can parse the
// route parameter (and before any database access).
func TestUsersSchemaValidationPrecedesRouteID(t *testing.T) {
	admin := &lib.AccessTokenClaims{ID: 99, Role: "admin", Status: "active"}

	context, recorder := usersTestContext(t, http.MethodPatch, "/api/users/abc",
		`{"avatarStyle":"nope"}`, admin)
	context.Params = gin.Params{{Key: "userId", Value: "abc"}}
	usersUpdate(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["code"] != "VALIDATION_ERROR" || payload["error"] != "must be equal to one of the allowed values" {
		t.Fatalf("payload = %v", payload)
	}

	context, recorder = usersTestContext(t, http.MethodPatch, "/api/users/abc/role", `{"role":"nope"}`, admin)
	context.Params = gin.Params{{Key: "userId", Value: "abc"}}
	usersUpdateRole(context)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "VALIDATION_ERROR") {
		t.Fatalf("role status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestUsersInvalidRouteIDAfterValidBody pins the next step: once the schema passes,
// an invalid id is INVALID_ID and no database call happens.
func TestUsersInvalidRouteIDAfterValidBody(t *testing.T) {
	admin := &lib.AccessTokenClaims{ID: 99, Role: "admin", Status: "active"}

	context, recorder := usersTestContext(t, http.MethodPatch, "/api/users/0", `{"avatarStyle":"rings"}`, admin)
	context.Params = gin.Params{{Key: "userId", Value: "0"}}
	usersUpdate(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "INVALID_ID") {
		t.Fatalf("body = %s", recorder.Body.String())
	}

	context, recorder = usersTestContext(t, http.MethodPatch, "/api/users/abc/status", `{"status":"active"}`, admin)
	context.Params = gin.Params{{Key: "userId", Value: "abc"}}
	usersUpdateStatus(context)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "INVALID_ID") {
		t.Fatalf("status route = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestUsersBodylessPatchIsMustBeObject pins that a PATCH without a body is rejected
// by the schema, not treated as an empty update.
func TestUsersBodylessPatchIsMustBeObject(t *testing.T) {
	admin := &lib.AccessTokenClaims{ID: 99, Role: "admin", Status: "active"}
	context, recorder := usersTestContext(t, http.MethodPatch, "/api/users/1", "", admin)
	context.Params = gin.Params{{Key: "userId", Value: "1"}}
	usersUpdate(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "must be object") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}
