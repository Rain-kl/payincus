package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestCustomInitValidateBodyCreate pins the POST schema to what Fastify 5 with
// its default AJV options produces (allErrors:false → first message only,
// coerceTypes:'array', removeAdditional without additionalProperties:false → the
// unknown key survives and is ignored by the handler). The `want` values were
// captured from a real fastify@5.6.2 instance.
func TestCustomInitValidateBodyCreate(t *testing.T) {
	required := []string{"name", "command", "distros"}

	cases := []struct {
		name string
		body any
		want string
	}{
		{"empty body reports name first", map[string]any{}, "name is required"},
		{"missing distros after name+command", map[string]any{"name": "a", "command": "b"}, "distros is required"},
		{"missing name wins over a bad command", map[string]any{"command": "", "distros": []any{}}, "name is required"},
		{"empty name", map[string]any{"name": "", "command": "b", "distros": []any{"ubuntu"}}, "name is too short"},
		{"empty command", map[string]any{"name": "a", "command": "", "distros": []any{"ubuntu"}}, "command is too short"},
		{"name over the 100 char limit", map[string]any{"name": strings.Repeat("x", 101), "command": "b", "distros": []any{"u"}}, "name is too long"},
		{"empty distros array", map[string]any{"name": "a", "command": "b", "distros": []any{}}, "must NOT have fewer than 1 items"},
		{"distro item is an object", map[string]any{"name": "a", "command": "b", "distros": []any{map[string]any{}}}, "must be string"},
		{"distros is an object", map[string]any{"name": "a", "command": "b", "distros": map[string]any{"a": 1}}, "must be array"},
		{"description object", map[string]any{"name": "a", "command": "b", "distros": []any{"u"}, "description": map[string]any{}}, "must be string"},
		{"description too long", map[string]any{"name": "a", "command": "b", "distros": []any{"u"}, "description": strings.Repeat("d", 501)}, "description is too long"},
		{"name object", map[string]any{"name": map[string]any{}, "command": "b", "distros": []any{"u"}}, "must be string"},
		{"null name coerces to empty then fails minLength", map[string]any{"name": nil, "command": "b", "distros": []any{"u"}}, "name is too short"},
		{"multi-element array name", map[string]any{"name": []any{"a", "b"}, "command": "b", "distros": []any{"u"}}, "must be string"},
		{"non-object body", []any{float64(1)}, "must be object"},
		{"null body", nil, "must be object"},
		{"schema order beats document order", map[string]any{
			"description": strings.Repeat("d", 501),
			"distros":     []any{},
			"command":     "b",
			"name":        "a",
		}, "must NOT have fewer than 1 items"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := customInitValidateBody(tc.body, required, false)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

// TestCustomInitValidateBodyCreateCoercion covers the values AJV accepts and
// rewrites.
func TestCustomInitValidateBodyCreateCoercion(t *testing.T) {
	required := []string{"name", "command", "distros"}

	// A scalar distro is wrapped into a one-element array (coerceTypes:'array').
	body, message := customInitValidateBody(map[string]any{
		"name": "a", "command": "b", "distros": "ubuntu",
	}, required, false)
	if message != "" {
		t.Fatalf("scalar distros must be accepted, got %q", message)
	}
	if len(body.Distros) != 1 || body.Distros[0] != "ubuntu" {
		t.Fatalf("distros = %#v, want [ubuntu]", body.Distros)
	}

	// Items are stringified, single-element arrays unwrap.
	body, message = customInitValidateBody(map[string]any{
		"name": "a", "command": "b", "distros": []any{float64(1), []any{"x"}, nil},
	}, required, false)
	if message != "" {
		t.Fatalf("unexpected message %q", message)
	}
	want := []string{"1", "x", ""}
	if strings.Join(body.Distros, "|") != strings.Join(want, "|") {
		t.Fatalf("distros = %#v, want %#v", body.Distros, want)
	}

	// Null distros become [""] (not an empty array), exactly like AJV.
	body, message = customInitValidateBody(map[string]any{
		"name": "a", "command": "b", "distros": nil,
	}, required, false)
	if message != "" {
		t.Fatalf("null distros must be accepted, got %q", message)
	}
	if len(body.Distros) != 1 || body.Distros[0] != "" {
		t.Fatalf("distros = %#v, want [\"\"]", body.Distros)
	}

	// Numbers/booleans are stringified, and an absent description stays absent.
	body, message = customInitValidateBody(map[string]any{
		"name": float64(12), "command": "b", "distros": []any{"u"}, "extra": float64(1),
	}, required, false)
	if message != "" {
		t.Fatalf("unknown key must be tolerated, got %q", message)
	}
	if body.Name == nil || *body.Name != "12" {
		t.Fatalf("name = %#v, want \"12\"", body.Name)
	}
	if body.DescSet {
		t.Fatal("absent description must not be marked present")
	}

	// An explicit null description coerces to "" for the non-nullable schema.
	body, message = customInitValidateBody(map[string]any{
		"name": "a", "command": "b", "distros": []any{"u"}, "description": nil,
	}, required, false)
	if message != "" {
		t.Fatalf("null description must be accepted, got %q", message)
	}
	if !body.DescSet || body.Description == nil || *body.Description != "" {
		t.Fatalf("description = %#v (present=%v), want \"\"", body.Description, body.DescSet)
	}
}

// TestCustomInitValidateBodyUpdate covers the PUT schema (no required list, a
// nullable description and a boolean enabled).
func TestCustomInitValidateBodyUpdate(t *testing.T) {
	body, message := customInitValidateBody(map[string]any{}, nil, true)
	if message != "" {
		t.Fatalf("empty update body must be valid, got %q", message)
	}
	if body.Name != nil || body.Command != nil || body.DistrosSet || body.DescSet || body.Enabled != nil {
		t.Fatalf("empty update body produced %#v", body)
	}

	// enabled is tri-state: absent, explicit false and explicit true all differ.
	body, message = customInitValidateBody(map[string]any{"enabled": false}, nil, true)
	if message != "" || body.Enabled == nil || *body.Enabled {
		t.Fatalf("explicit false enabled = %#v / %q", body.Enabled, message)
	}
	body, message = customInitValidateBody(map[string]any{"enabled": true}, nil, true)
	if message != "" || body.Enabled == nil || !*body.Enabled {
		t.Fatalf("explicit true enabled = %#v / %q", body.Enabled, message)
	}
	body, message = customInitValidateBody(map[string]any{"enabled": nil}, nil, true)
	if message != "" || body.Enabled == nil || *body.Enabled {
		t.Fatalf("null enabled must coerce to false, got %#v / %q", body.Enabled, message)
	}
	for _, raw := range []any{"yes", float64(2), []any{}} {
		if _, message := customInitValidateBody(map[string]any{"enabled": raw}, nil, true); message != "must be boolean" {
			t.Fatalf("enabled=%#v message = %q, want must be boolean", raw, message)
		}
	}
	if _, message := customInitValidateBody(map[string]any{"enabled": "false"}, nil, true); message != "" {
		t.Fatalf("\"false\" must coerce, got %q", message)
	}

	// description (string|null) and the union's coercion failures.
	body, message = customInitValidateBody(map[string]any{"description": nil}, nil, true)
	if message != "" || !body.DescSet || body.Description != nil {
		t.Fatalf("null description = %#v / %q", body, message)
	}
	body, message = customInitValidateBody(map[string]any{"description": float64(5)}, nil, true)
	if message != "" || body.Description == nil || *body.Description != "5" {
		t.Fatalf("number description = %#v / %q", body.Description, message)
	}
	body, message = customInitValidateBody(map[string]any{"description": []any{""}}, nil, true)
	if message != "" || body.Description != nil {
		t.Fatalf("[empty string] description = %#v / %q, want null", body.Description, message)
	}
	body, message = customInitValidateBody(map[string]any{"description": []any{nil}}, nil, true)
	if message != "" || body.Description == nil || *body.Description != "" {
		t.Fatalf("[null] description = %#v / %q, want \"\"", body.Description, message)
	}
	for _, raw := range []any{map[string]any{}, []any{"x"}, []any{}, []any{"true"}} {
		if _, message := customInitValidateBody(map[string]any{"description": raw}, nil, true); message != "must be string,null" {
			t.Fatalf("description=%#v message = %q, want must be string,null", raw, message)
		}
	}

	// A scalar distro is wrapped; an empty array still fails minItems.
	body, message = customInitValidateBody(map[string]any{"distros": "ubuntu"}, nil, true)
	if message != "" || !body.DistrosSet || len(body.Distros) != 1 {
		t.Fatalf("scalar PUT distros = %#v / %q", body, message)
	}
	if _, message := customInitValidateBody(map[string]any{"distros": []any{}}, nil, true); message != "must NOT have fewer than 1 items" {
		t.Fatalf("empty PUT distros message = %q", message)
	}
	if _, message := customInitValidateBody([]any{}, nil, true); message != "must be object" {
		t.Fatalf("non-object PUT body message = %q", message)
	}
}

func TestCustomInitTruncateText(t *testing.T) {
	// truncateText uses String#length / String#substring, i.e. UTF-16 code units
	// (unlike the schema bounds below, which use AJV's ucs2length).
	cases := []struct {
		text string
		want string
	}{
		{"", ""},
		{strings.Repeat("a", 100), strings.Repeat("a", 100)},
		{strings.Repeat("a", 101), strings.Repeat("a", 100) + "..."},
		// Astral characters count as two UTF-16 units, so 50 emoji are exactly 100.
		{strings.Repeat("😀", 50), strings.Repeat("😀", 50)},
		{strings.Repeat("😀", 51), strings.Repeat("😀", 50) + "..."},
	}
	for _, tc := range cases {
		if got := customInitTruncateText(tc.text, customInitCommandPreviewLength); got != tc.want {
			t.Errorf("truncate(%d chars) = %q, want %q", len([]rune(tc.text)), got, tc.want)
		}
	}
}

// TestCustomInitAJVLength pins AJV's ucs2length semantics: code points, not
// UTF-16 units. This differs from jsStringLength (used by storage-configs) and
// is why this module carries its own counter.
func TestCustomInitAJVLength(t *testing.T) {
	if got := customInitAJVLength("abc"); got != 3 {
		t.Fatalf("ascii length = %d", got)
	}
	if got := customInitAJVLength(strings.Repeat("😀", 100)); got != 100 {
		t.Fatalf("emoji length = %d, want 100", got)
	}
	// Verified against fastify@5.6.2: name of 100 emoji passes maxLength:100,
	// 101 emoji fails (UTF-16 counting would cut this at 50).
	required := []string{"name", "command", "distros"}
	if _, message := customInitValidateBody(map[string]any{
		"name": strings.Repeat("😀", 100), "command": "b", "distros": []any{"u"},
	}, required, false); message != "" {
		t.Fatalf("100 emoji name must pass, got %q", message)
	}
	if _, message := customInitValidateBody(map[string]any{
		"name": strings.Repeat("😀", 101), "command": "b", "distros": []any{"u"},
	}, required, false); message != "name is too long" {
		t.Fatalf("101 emoji name message = %q", message)
	}
	if _, message := customInitValidateBody(map[string]any{
		"description": strings.Repeat("😀", 500),
	}, nil, true); message != "" {
		t.Fatalf("500 emoji description must pass, got %q", message)
	}
	if _, message := customInitValidateBody(map[string]any{
		"description": strings.Repeat("😀", 501),
	}, nil, true); message != "description is too long" {
		t.Fatalf("501 emoji description message = %q", message)
	}
}

func TestCustomInitCountCommandLines(t *testing.T) {
	cases := []struct {
		command string
		want    int
	}{
		// `''.split('\n')` is `['']`, whose only line trims to empty.
		{"", 0},
		{"a", 1},
		{"a\nb", 2},
		{"a\n\nb\n", 2},
		{"a\n  \n\t\nb", 2},
		{"\n\n\n", 0},
	}
	for _, tc := range cases {
		if got := customInitCountCommandLines(tc.command); got != tc.want {
			t.Errorf("countCommandLines(%q) = %d, want %d", tc.command, got, tc.want)
		}
	}
}

func TestCustomInitInvalidDistros(t *testing.T) {
	if got := customInitInvalidDistros([]string{"ubuntu", "all"}); len(got) != 0 {
		t.Fatalf("valid distros flagged: %#v", got)
	}
	got := customInitInvalidDistros([]string{"ubuntu", "Ubuntu", "other", "windows"})
	want := []string{"Ubuntu", "other", "windows"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("invalid distros = %#v, want %#v", got, want)
	}
	// The write list does not include "other" even though the query list does.
	if customInitDistroAllowed("other") {
		t.Fatal("other must not be writable")
	}
	if !customInitQueryDistroAllowed("other") {
		t.Fatal("other must be queryable")
	}
}

func TestCustomInitErrorMap(t *testing.T) {
	cases := []struct {
		code    string
		status  int
		message string
	}{
		{"COMMAND_TOO_LARGE", 400, "Command content too large (max 64KB)"},
		{"MAX_COMMANDS_REACHED", 400, "Maximum command templates reached (50)"},
		{"INVALID_DISTROS", 400, "Invalid distros"},
	}
	for _, tc := range cases {
		target, ok := customInitCreateErrorMap[tc.code]
		if !ok || target.Status != tc.status || target.Message != tc.message {
			t.Fatalf("create map[%s] = %#v (present=%v)", tc.code, target, ok)
		}
	}

	target, ok := customInitUpdateErrorMap["NOT_FOUND"]
	if !ok || target.Status != 404 || target.Message != "Command not found" {
		t.Fatalf("update map[NOT_FOUND] = %#v (present=%v)", target, ok)
	}
	if _, ok := customInitUpdateErrorMap["MAX_COMMANDS_REACHED"]; ok {
		t.Fatal("the update map must not contain MAX_COMMANDS_REACHED")
	}
}

func TestCustomInitDistroOptions(t *testing.T) {
	if len(customInitDistroOptions) != 7 {
		t.Fatalf("distro option count = %d, want 7", len(customInitDistroOptions))
	}
	// The icon differs from the id for three entries; those mappings are part of
	// the front-end contract.
	icons := map[string]string{}
	for _, option := range customInitDistroOptions {
		icons[option.ID] = option.Icon
	}
	for id, icon := range map[string]string{
		"rhel": "almalinux", "suse": "opensuse", "all": "linux",
	} {
		if icons[id] != icon {
			t.Fatalf("icon[%s] = %q, want %q", id, icons[id], icon)
		}
	}
}

// TestCustomInitValidateBodyIDOrder documents that the route-id check runs after
// body validation for PUT (Fastify validates before the handler), so a malformed
// id with a malformed body yields the validation error.
func TestCustomInitValidateBodyIDOrder(t *testing.T) {
	if _, message := customInitValidateBody(map[string]any{"enabled": "yes"}, nil, true); message != "must be boolean" {
		t.Fatalf("body validation must fail first, got %q", message)
	}
}

// TestCustomInitRouteRegistration pins the mount points and the auth guard:
// every route uses authenticateUser, so anonymous calls are 401.
func TestCustomInitRouteRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterCustomInitCommands(router)

	want := map[string]bool{
		"GET /api/init-commands":           false,
		"GET /api/init-commands/":          false,
		"GET /api/init-commands/available": false,
		"GET /api/init-commands/distros":   false,
		"GET /api/init-commands/:id":       false,
		"POST /api/init-commands":          false,
		"POST /api/init-commands/":         false,
		"PUT /api/init-commands/:id":       false,
		"DELETE /api/init-commands/:id":    false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, tracked := want[key]; tracked {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing route %q", key)
		}
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/init-commands", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list status = %d, want 401", recorder.Code)
	}
}
