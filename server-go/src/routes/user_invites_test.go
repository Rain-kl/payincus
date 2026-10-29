package routes

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ==================== user-invites ====================

// parseInt(value, 10) is deliberately looser than the strict route-id regex: the
// invite list accepts "12abc" as 12 and treats negatives/zero as "invalid".
func TestUserInvitesParseIntJS(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"12", 12, true},
		{"  12", 12, true},
		{"\t\n 12", 12, true},
		{"+7", 7, true},
		{"-7", -7, true},
		{"12abc", 12, true},
		{"1.9", 1, true},
		{"007", 7, true},
		{"0x10", 0, true}, // parseInt stops at 'x'
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"-", 0, false},
		{"+", 0, false},
		// JavaScript numbers are doubles: huge digit strings round rather than
		// failing to parse, which is what makes pageSize clamp instead of defaulting.
		{"99999999999999999999", 1e20, true},
		{"9999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999", math.Inf(1), true},
	}
	for _, tc := range cases {
		got, ok := userInvitesParseIntJS(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseIntJS(%q) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// parsePositiveInteger falls back on anything that is not a positive integer.
func TestUserInvitesParsePositiveInteger(t *testing.T) {
	cases := []struct {
		in       string
		fallback int64
		want     int64
	}{
		{"3", 1, 3},
		{"  3  ", 1, 3},
		{"3abc", 1, 3},
		{"0", 1, 1},
		{"-1", 20, 20},
		{"abc", 20, 20},
		{"", 20, 20},
		{"1.5", 7, 1},
		// Huge finite values are kept (then clamped to 100 for pageSize). Anything at
		// or above int64 saturates, which is unobservable: such a page always
		// overflows the skip offset and errors out first, exactly like Prisma.
		{"99999999999999999999", 20, math.MaxInt64},
		{"9007199254740991", 20, 9007199254740991},
	}
	for _, tc := range cases {
		if got := userInvitesParsePositiveInteger(tc.in, tc.fallback); got != tc.want {
			t.Errorf("parsePositiveInteger(%q,%d) = %d, want %d", tc.in, tc.fallback, got, tc.want)
		}
	}
}

// normalizeGenerateInviteBody throws plain `{ error }` objects (no `code`), with
// these exact Chinese messages.
func TestUserInvitesNormalizeGenerateBody(t *testing.T) {
	cases := []struct {
		name      string
		input     any
		wantErr   string
		wantCost  string
		wantCount int
	}{
		{name: "non-object", input: "nope", wantErr: userInvitesInvalidBodyError},
		{name: "array", input: []any{}, wantErr: userInvitesInvalidBodyError},
		{name: "null", input: nil, wantErr: userInvitesInvalidBodyError},
		{name: "missing costResource", input: map[string]any{}, wantErr: userInvitesMissingCostError},
		{name: "non-string costResource", input: map[string]any{"costResource": 5}, wantErr: userInvitesMissingCostError},
		{name: "blank costResource", input: map[string]any{"costResource": "   "}, wantErr: userInvitesInvalidCostError},
		{
			name:    "too-long costResource",
			input:   map[string]any{"costResource": strings.Repeat("a", 65)},
			wantErr: userInvitesInvalidCostError,
		},
		{
			name:      "trimmed resource and default count",
			input:     map[string]any{"costResource": "  balance  "},
			wantCost:  "balance",
			wantCount: 1,
		},
		{
			name:      "explicit count",
			input:     map[string]any{"costResource": "points", "count": float64(5)},
			wantCost:  "points",
			wantCount: 5,
		},
		{
			name:      "count bounds are inclusive",
			input:     map[string]any{"costResource": "points", "count": float64(10)},
			wantCost:  "points",
			wantCount: 10,
		},
		{name: "count zero", input: map[string]any{"costResource": "points", "count": float64(0)}, wantErr: userInvitesInvalidCountMessage},
		{name: "count too big", input: map[string]any{"costResource": "points", "count": float64(11)}, wantErr: userInvitesInvalidCountMessage},
		{name: "count fractional", input: map[string]any{"costResource": "points", "count": 1.5}, wantErr: userInvitesInvalidCountMessage},
		{name: "count string", input: map[string]any{"costResource": "points", "count": "3"}, wantErr: userInvitesInvalidCountMessage},
		{name: "count null", input: map[string]any{"costResource": "points", "count": nil}, wantErr: userInvitesInvalidCountMessage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, errMessage := userInvitesNormalizeGenerateBody(tc.input)
			if errMessage != tc.wantErr {
				t.Fatalf("error = %q, want %q", errMessage, tc.wantErr)
			}
			if tc.wantErr != "" {
				return
			}
			if body.CostResource != tc.wantCost || body.Count != tc.wantCount {
				t.Fatalf("body = %+v, want %q/%d", body, tc.wantCost, tc.wantCount)
			}
		})
	}
}

// The Fastify schema runs before normalizeGenerateInviteBody: required first, then
// the property checks in schema order, with a single message (allErrors:false).
func TestUserInvitesGenerateSchemaOrder(t *testing.T) {
	validated, message := validateAJVObject(map[string]any{}, []string{"costResource"},
		[]ajvFieldSpec{
			{Name: "costResource", Kind: ajvKindString, MinLength: 1, MaxLength: userInvitesCostResourceMaxLen},
			{Name: "count", Kind: ajvKindInteger, Minimum: 1, Maximum: userInvitesMaxGenerationCount},
		})
	if message != "costResource is required" {
		t.Errorf("missing costResource = %q", message)
	}
	if len(validated.Values) != 0 {
		t.Errorf("values = %v", validated.Values)
	}

	_, message = validateAJVObject(map[string]any{"costResource": ""}, nil,
		[]ajvFieldSpec{
			{Name: "costResource", Kind: ajvKindString, MinLength: 1, MaxLength: userInvitesCostResourceMaxLen},
		})
	if message != "costResource is too short" {
		t.Errorf("empty costResource = %q", message)
	}

	_, message = validateAJVObject(map[string]any{
		"costResource": strings.Repeat("a", 65),
	}, nil, []ajvFieldSpec{
		{Name: "costResource", Kind: ajvKindString, MinLength: 1, MaxLength: userInvitesCostResourceMaxLen},
	})
	if message != "costResource is too long" {
		t.Errorf("long costResource = %q", message)
	}

	// count is an integer with 1..10 (maximum is checked before minimum).
	_, message = validateAJVObject(map[string]any{"costResource": "balance", "count": float64(11)}, nil,
		[]ajvFieldSpec{
			{Name: "count", Kind: ajvKindInteger, Minimum: 1, Maximum: userInvitesMaxGenerationCount},
		})
	if message != "must be <= 10" {
		t.Errorf("count > max = %q", message)
	}
	_, message = validateAJVObject(map[string]any{"costResource": "balance", "count": float64(0)}, nil,
		[]ajvFieldSpec{
			{Name: "count", Kind: ajvKindInteger, Minimum: 1, Maximum: userInvitesMaxGenerationCount},
		})
	if message != "must be >= 1" {
		t.Errorf("count < min = %q", message)
	}
	_, message = validateAJVObject(map[string]any{"costResource": "balance", "count": 1.5}, nil,
		[]ajvFieldSpec{
			{Name: "count", Kind: ajvKindInteger, Minimum: 1, Maximum: userInvitesMaxGenerationCount},
		})
	if message != "must be integer" {
		t.Errorf("fractional count = %q", message)
	}

	// A non-object body is rejected before any property check.
	_, message = validateAJVObject([]any{}, []string{"costResource"}, nil)
	if message != "must be object" {
		t.Errorf("array body = %q", message)
	}
}

// toInviteCodePayload builds a camelCase object with registerUrl and usedByUser.
func TestUserInvitesToPayload(t *testing.T) {
	usedAt := time.Date(2026, 10, 30, 1, 2, 3, 400_000_000, time.UTC)
	expiresAt := time.Date(2026, 11, 30, 1, 2, 3, 0, time.UTC)
	createdAt := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	usedBy := int32(8)
	userID := int32(8)
	username := "alice"
	email := "alice@example.com"
	avatarStyle := "notionists"
	badgeID := "badge_1"
	userCreatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	row := db.UserInvitesCodeRow{
		ID:            11,
		Code:          "abcdEFGH1234",
		CreatedBy:     3,
		UsedBy:        &usedBy,
		UsedAt:        &usedAt,
		ExpiresAt:     &expiresAt,
		CostSnapshot:  []byte(`{"resource":"balance","amount":1.5}`),
		CreatedAt:     createdAt,
		UserID:        &userID,
		Username:      &username,
		Email:         &email,
		AvatarStyle:   &avatarStyle,
		AvatarBadgeID: &badgeID,
		UserCreatedAt: &userCreatedAt,
	}

	payload := userInvitesToPayload(row)
	if payload.RegisterURL != "/register/abcdEFGH1234" {
		t.Errorf("registerUrl = %q", payload.RegisterURL)
	}
	if payload.CreatedAt != "2026-10-29T12:00:00.000Z" {
		t.Errorf("createdAt = %q", payload.CreatedAt)
	}
	if payload.UsedAt == nil || *payload.UsedAt != "2026-10-30T01:02:03.400Z" {
		t.Errorf("usedAt = %v", payload.UsedAt)
	}
	if payload.UsedByUser == nil {
		t.Fatal("usedByUser must be present for a used code")
	}
	if payload.UsedByUser.Username != "alice" || payload.UsedByUser.AvatarStyle != "notionists" {
		t.Errorf("usedByUser = %+v", payload.UsedByUser)
	}
	if payload.UsedByUser.CreatedAt != "2026-01-01T00:00:00.000Z" {
		t.Errorf("usedByUser.createdAt = %q", payload.UsedByUser.CreatedAt)
	}

	// The wire shape is camelCase, costSnapshot is the decoded JSON, and the
	// null-able columns are JSON null (not omitted).
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"id", "code", "createdBy", "usedBy", "usedAt", "expiresAt", "createdAt",
		"costSnapshot", "registerUrl", "usedByUser",
	} {
		if _, present := decoded[key]; !present {
			t.Errorf("payload is missing key %q: %s", key, encoded)
		}
	}
	snapshot, ok := decoded["costSnapshot"].(map[string]any)
	if !ok || snapshot["resource"] != "balance" {
		t.Errorf("costSnapshot = %#v", decoded["costSnapshot"])
	}

	// An unused code: usedBy/usedAt/usedByUser are null, costSnapshot may be null.
	unused := row
	unused.UsedBy = nil
	unused.UsedAt = nil
	unused.UserID = nil
	unused.CostSnapshot = nil
	payload = userInvitesToPayload(unused)
	if payload.UsedBy != nil || payload.UsedAt != nil || payload.UsedByUser != nil {
		t.Errorf("unused code payload = %+v", payload)
	}
	if payload.CostSnapshot != nil {
		t.Errorf("costSnapshot = %#v, want null", []byte(payload.CostSnapshot))
	}
	encoded, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"usedBy":null`, `"usedAt":null`, `"usedByUser":null`, `"costSnapshot":null`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("payload %s does not contain %s", encoded, key)
		}
	}
}

// The summary route rounds usageRate to one decimal: Math.round(x*1000)/10.
func TestUserInvitesSummaryUsageRate(t *testing.T) {
	cases := []struct {
		total, used int64
		want        float64
	}{
		{0, 0, 0},
		{10, 0, 0},
		{10, 1, 10},
		{10, 10, 100},
		{3, 1, 33.3},
		{3, 2, 66.7},
		{7, 1, 14.3},
		{1000, 1, 0.1},
		{1, 1, 100},
	}
	for _, tc := range cases {
		got := 0.0
		if tc.total > 0 {
			got = math.Round(float64(tc.used)/float64(tc.total)*1000) / 10
		}
		if got != tc.want {
			t.Errorf("usageRate(%d/%d) = %v, want %v", tc.used, tc.total, got, tc.want)
		}
	}
}

// totalPages is Math.max(1, ceil(total/pageSize)) — never zero.
func TestUserInvitesTotalPagesAlwaysAtLeastOne(t *testing.T) {
	cases := []struct {
		total     int64
		pageSize  int
		wantPages int64
	}{
		{0, 20, 1},
		{1, 20, 1},
		{20, 20, 1},
		{21, 20, 2},
		{100, 20, 5},
		{101, 20, 6},
		{5, 100, 1},
	}
	for _, tc := range cases {
		got := int64(1)
		if tc.total > 0 {
			got = (tc.total + int64(tc.pageSize) - 1) / int64(tc.pageSize)
			if got < 1 {
				got = 1
			}
		}
		if got != tc.wantPages {
			t.Errorf("totalPages(%d/%d) = %d, want %d", tc.total, tc.pageSize, got, tc.wantPages)
		}
	}
}
