package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// ==================== invite codes: pagination & config ====================

// clampInvitePagination is the whole pagination contract of the invite list: any
// non-positive page becomes 1, pageSize is clamped into [1,100] with 20 as the
// fallback.
func TestUserInvitesClampPagination(t *testing.T) {
	cases := []struct {
		page, pageSize         int64
		wantPage, wantPageSize int64
	}{
		{1, 20, 1, 20},
		{0, 0, 1, 20},
		{-5, -5, 1, 1},
		{3, 500, 3, 100},
		{2, 1, 2, 1},
		{1, 100, 1, 100},
		{0, 7, 1, 7},
	}
	for _, tc := range cases {
		page, pageSize := userInvitesClampPagination(tc.page, tc.pageSize)
		if page != tc.wantPage || pageSize != tc.wantPageSize {
			t.Errorf("clamp(%d,%d) = (%d,%d), want (%d,%d)",
				tc.page, tc.pageSize, page, pageSize, tc.wantPage, tc.wantPageSize)
		}
	}
}

// parseInviteCostConfig must survive garbage and always fall back to the built-in
// two disabled options; it must also keep the configured order and normalize the
// per-resource amounts.
func TestUserInvitesParseCostConfig(t *testing.T) {
	empty := userInvitesParseCostConfig(nil)
	if len(empty) != 2 || empty[0].Resource != "balance" || empty[1].Resource != "points" {
		t.Fatalf("nil config = %+v", empty)
	}
	if empty[0].Enabled || empty[1].Enabled {
		t.Fatalf("defaults must be disabled: %+v", empty)
	}

	for _, raw := range []string{"", "not json", "{}", `"str"`, "42", "null"} {
		got := userInvitesParseCostConfig(&raw)
		if len(got) != 2 {
			t.Errorf("config %q = %+v, want the two defaults", raw, got)
		}
	}

	// balance rounds to 2 decimals, points floors, unknown resources keep
	// Math.max(0, amount), and entries without a resource are dropped.
	raw := `[{"resource":" balance ","amount":1.239,"enabled":true},
	         {"resource":"points","amount":2.9,"enabled":true},
	         {"resource":"coupon","amount":-3,"enabled":true},
	         {"amount":5},
	         {"resource":"","amount":1},
	         null,
	         "nope"]`
	options := userInvitesParseCostConfig(&raw)
	if len(options) != 3 {
		t.Fatalf("options = %+v, want 3 usable entries", options)
	}
	if options[0].Resource != "balance" || options[0].Amount != 1.24 || !options[0].Enabled {
		t.Errorf("balance option = %+v", options[0])
	}
	if options[1].Resource != "points" || options[1].Amount != 2 {
		t.Errorf("points option = %+v", options[1])
	}
	if options[2].Resource != "coupon" || options[2].Amount != 0 {
		t.Errorf("unknown resource option = %+v", options[2])
	}

	// `enabled` is only true for a literal JSON true.
	raw = `[{"resource":"balance","amount":0,"enabled":"true"}]`
	options = userInvitesParseCostConfig(&raw)
	if options[0].Enabled {
		t.Errorf(`enabled:"true" must not enable the option: %+v`, options[0])
	}

	// A non-numeric amount becomes 0 (Number('abc') is NaN → the handler's
	// `Number.isFinite` guard substitutes 0).
	raw = `[{"resource":"balance","amount":"abc","enabled":true}]`
	options = userInvitesParseCostConfig(&raw)
	if options[0].Amount != 0 {
		t.Errorf("NaN amount = %v, want 0", options[0].Amount)
	}
}

// ensureDefaultCostOptions appends the missing defaults without moving or
// duplicating the user's entries (JavaScript Map.set semantics).
func TestUserInvitesEnsureDefaultCostOptions(t *testing.T) {
	configured := []UserInvitesCostOption{
		{Resource: "coupon", Amount: 3, Enabled: true},
	}
	got := userInvitesEnsureDefaultCostOptions(configured)
	if len(got) != 3 {
		t.Fatalf("got %+v, want coupon + the two defaults", got)
	}
	if got[0].Resource != "coupon" || got[1].Resource != "balance" || got[2].Resource != "points" {
		t.Fatalf("order = %+v", got)
	}

	// A duplicate resource keeps its first position and the last value.
	duplicated := []UserInvitesCostOption{
		{Resource: "balance", Amount: 1, Enabled: true},
		{Resource: "points", Amount: 2, Enabled: true},
		{Resource: "balance", Amount: 9, Enabled: false},
	}
	got = userInvitesEnsureDefaultCostOptions(duplicated)
	if len(got) != 2 {
		t.Fatalf("duplicates collapsed to %+v", got)
	}
	if got[0].Resource != "balance" || got[0].Amount != 9 || got[0].Enabled {
		t.Fatalf("duplicate value = %+v (want the last one)", got[0])
	}

	normalized := userInvitesNormalizeCostOptions(duplicated)
	if normalized[0].Amount != 9 {
		t.Fatalf("normalize kept %+v", normalized[0])
	}
}

// chargeInviteGenerationCost looks options up by (resource, enabled), so the
// label/unit/format helpers are part of the stored snapshot contract.
func TestUserInvitesCostLabelsAndFormats(t *testing.T) {
	label, unit, ok := userInvitesCostLabel("balance")
	if !ok || label != "余额" || unit != "元" {
		t.Fatalf("balance label = %q/%q/%v", label, unit, ok)
	}
	if got := userInvitesCostFormat("balance", 12.5); got != "¥12.50" {
		t.Errorf("balance format = %q", got)
	}
	if got := userInvitesCostFormat("balance", 1.239); got != "¥1.24" {
		t.Errorf("balance format = %q, want 2 decimals", got)
	}

	label, unit, ok = userInvitesCostLabel("points")
	if !ok || label != "积分" || unit != "积分" {
		t.Fatalf("points label = %q/%q/%v", label, unit, ok)
	}
	if got := userInvitesCostFormat("points", 7); got != "7 积分" {
		t.Errorf("points format = %q", got)
	}
	if got := userInvitesCostFormat("points", 7.9); got != "7 积分" {
		t.Errorf("points format must floor: %q", got)
	}

	if _, _, ok := userInvitesCostLabel("coupon"); ok {
		t.Error("unknown resource must not report a handler")
	}
}

// Number formatting shows up in `displayAmount`, which the UI renders verbatim.
func TestUserInvitesFormatNumber(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{1.5, "1.5"},
		{12.5, "12.5"},
		{-3, "-3"},
		{100, "100"},
		{0.1, "0.1"},
	}
	for _, tc := range cases {
		if got := userInvitesFormatNumber(tc.in); got != tc.want {
			t.Errorf("formatNumber(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got := userInvitesFormatFixed2(12.5); got != "12.50" {
		t.Errorf("toFixed(2) = %q", got)
	}
	if got := userInvitesFormatFixed1(66.666); got != "66.7" {
		t.Errorf("toFixed(1) = %q", got)
	}
	if got := userInvitesFormatFixed1(80); got != "80.0" {
		t.Errorf("toFixed(1) = %q, want the trailing .0", got)
	}
}

// nanoid(12) draws from a 64-character URL-safe alphabet, so a generated code must
// be 12 characters long and contain only that alphabet.
func TestUserInvitesRandomCode(t *testing.T) {
	if len(userInvitesURLAlphabet) != 64 {
		t.Fatalf("alphabet length = %d, nanoid's mask assumes 64", len(userInvitesURLAlphabet))
	}
	// Pin the alphabet to nanoid's urlAlphabet: any change here changes every
	// generated code, and the mask semantics depend on it staying 64 chars.
	const nanoidAlphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"
	if userInvitesURLAlphabet != nanoidAlphabet {
		t.Fatalf("alphabet drifted from nanoid's urlAlphabet:\n got %q\nwant %q",
			userInvitesURLAlphabet, nanoidAlphabet)
	}

	seen := map[rune]bool{}
	for i := 0; i < 200; i++ {
		code, err := userInvitesRandomCode(userInvitesCodeLength)
		if err != nil {
			t.Fatalf("randomCode: %v", err)
		}
		if len(code) != userInvitesCodeLength {
			t.Fatalf("code %q length = %d", code, len(code))
		}
		for _, ch := range code {
			if !strings.ContainsRune(userInvitesURLAlphabet, ch) {
				t.Fatalf("code %q contains %q outside the nanoid alphabet", code, ch)
			}
			seen[ch] = true
		}
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct characters drawn; randomness looks broken", len(seen))
	}

	empty, err := userInvitesRandomCode(0)
	if err != nil || empty != "" {
		t.Errorf("randomCode(0) = %q, %v", empty, err)
	}
}

// Number(value): unlike parseFloat it rejects trailing garbage.
func TestUserInvitesToNumberJS(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{nil, 0, true},
		{true, 1, true},
		{false, 0, true},
		{2.5, 2.5, true},
		{"12", 12, true},
		{" 12 ", 12, true},
		{"", 0, true},
		{"   ", 0, true},
		{"12abc", 0, false},
		{"abc", 0, false},
		{"-1.5", -1.5, true},
		{"1e3", 1000, true},
		{map[string]any{}, 0, false},
		{[]any{1}, 0, false},
	}
	for _, tc := range cases {
		got, ok := userInvitesToNumberJS(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("toNumberJS(%#v) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// The cost snapshot stored in jsonb is exactly the object literal of
// chargeInviteGenerationCost — key order included.
func TestUserInvitesCostSnapshotJSON(t *testing.T) {
	snapshot := userInvitesCostSnapshot{
		Resource:      "balance",
		Amount:        1.5,
		Label:         "余额",
		Unit:          "元",
		DisplayAmount: "¥1.50",
		ChargedAt:     "2026-10-29T12:00:00.000Z",
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"resource":"balance","amount":1.5,"label":"余额","unit":"元",` +
		`"displayAmount":"¥1.50","chargedAt":"2026-10-29T12:00:00.000Z"}`
	if string(encoded) != want {
		t.Fatalf("snapshot = %s\nwant      %s", encoded, want)
	}
}

// Decoding cost_snapshot mirrors `costSnapshot ?? null`: an absent or malformed
// column renders as JSON null, never as an empty object.
func TestDecodeUserInvitesCostSnapshot(t *testing.T) {
	if got := decodeUserInvitesCostSnapshot(nil); got != nil {
		t.Errorf("nil snapshot = %#v", got)
	}
	if got := decodeUserInvitesCostSnapshot([]byte("")); got != nil {
		t.Errorf("empty snapshot = %#v", got)
	}
	if got := decodeUserInvitesCostSnapshot([]byte("{oops")); got != nil {
		t.Errorf("malformed snapshot = %#v", got)
	}
	decoded := decodeUserInvitesCostSnapshot([]byte(`{"resource":"points"}`))
	object, ok := decoded.(map[string]any)
	if !ok || object["resource"] != "points" {
		t.Errorf("decoded = %#v", decoded)
	}
}

// usageRate uses toFixed(1) in the deny message, so the formatter must keep the
// trailing digit.
func TestUserInvitesCanGenerateMessageShape(t *testing.T) {
	reason := "邀请码使用率小于80%，无法创建更多邀请码。您已有 11 个邀请码，使用率为 " +
		userInvitesFormatFixed1(18.1818) + "%，需要达到 80% 才能继续生成"
	if !strings.Contains(reason, "18.2%") {
		t.Fatalf("reason = %q, want a 1-decimal usage rate", reason)
	}
}

// Invite rows carry raw times; isoTimestamp is what the route puts on the wire.
func TestUserInvitesISOTimestamp(t *testing.T) {
	zoned := time.Date(2026, 10, 29, 20, 34, 56, 789_000_000, time.FixedZone("CST", 8*3600))
	if got := isoTimestamp(zoned); got != "2026-10-29T12:34:56.789Z" {
		t.Fatalf("isoTimestamp = %q", got)
	}
}

// ==================== oauth apps: scope catalog ====================

// The scope catalog is duplicated in db because lib imports db (a db → lib import
// would be a cycle). This test pins it to the Node PUBLIC_API_SCOPES literal so
// drift is caught here rather than in production.
func TestOAuthAppsScopeCatalogMatchesNode(t *testing.T) {
	// Literal copy of server/src/lib/public-api-auth.ts PUBLIC_API_SCOPES, in
	// source order.
	want := []string{
		"profile:read",
		"profile:write",
		"balance:read",
		"balance:write",
		"billing:read",
		"products:read",
		"services:read",
		"services:operate",
		"services:billing",
		"orders:read",
		"tickets:read",
		"tickets:write",
		"notifications:read",
		"notifications:send",
	}
	if len(OAuthAppsPublicAPIScopes) != len(want) {
		t.Fatalf("scope count = %d, want %d", len(OAuthAppsPublicAPIScopes), len(want))
	}
	for index, scope := range want {
		if OAuthAppsPublicAPIScopes[index] != scope {
			t.Fatalf("scope[%d] = %q, want %q", index, OAuthAppsPublicAPIScopes[index], scope)
		}
	}
}

// ==================== oauth apps: redirect URI normalization ====================

// The URL re-serialization is the highest-risk helper: normalizeOAuthRedirectUris
// stores `new URL(input).toString()`, so any divergence silently rewrites operator
// input. Expected values were produced by Node 26 (`new URL(s); u.hash = ”`).
func TestOAuthAppsNormalizeRedirectURIsMatchesNode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// identity, trailing slash, fragment removal
		{"https://example.com/cb", "https://example.com/cb"},
		{"https://example.com", "https://example.com/"},
		{"https://example.com/a#frag", "https://example.com/a"},
		{"http://localhost:3000/cb", "http://localhost:3000/cb"},
		{"http://127.0.0.1:8080/x?y=1#z", "http://127.0.0.1:8080/x?y=1"},
		{"http://[::1]:3000/cb", "http://[::1]:3000/cb"},
		{"https://example.com:8443/", "https://example.com:8443/"},
		{"https://example.com//x", "https://example.com//x"},
		{"https://example.com/x/", "https://example.com/x/"},
		{"https://example.com/a?b#c", "https://example.com/a?b"},

		// scheme/host lowercasing
		{"HTTPS://Example.COM:443/Path", "https://example.com/Path"},
		{"https://ExAmPle.com/Path", "https://example.com/Path"},
		{"HTTPS://EXAMPLE.COM", "https://example.com/"},
		{"https://LOCALHOST/x", "https://localhost/x"},
		{"http://LOCALHOST/x", "http://localhost/x"},
		{"https://example.com./x", "https://example.com./x"},

		// default-port removal, leading zeros, port 0
		{"https://example.com:443", "https://example.com/"},
		{"https://example.com:443/x", "https://example.com/x"},
		{"http://localhost:80/x", "http://localhost/x"},
		{"https://e:443/", "https://e/"},
		{"https://e:80/", "https://e:80/"},
		{"https://example.com:0/x", "https://example.com:0/x"},
		{"https://e:0443/p", "https://e/p"},

		// missing scheme slashes (`https:foo` becomes an authority)
		{"https:foo", "https://foo/"},
		{"https:/example.com", "https://example.com/"},
		{"https:///x", "https://x/"},

		// dot segments, including %2e spellings
		{"https://example.com/a/../b", "https://example.com/b"},
		{"https://example.com/./x", "https://example.com/x"},
		{"https://example.com/%2e%2e/x", "https://example.com/x"},
		{"https://e/a/.", "https://e/a/"},
		{"https://e/a/..", "https://e/"},
		{"https://e/a/./", "https://e/a/"},
		{"https://e/a/../", "https://e/"},
		{"https://e/.", "https://e/"},
		{"https://e/..", "https://e/"},
		{"https://e/a/../..", "https://e/"},
		{"https://e/a/../../b", "https://e/b"},

		// path/query encode sets
		{"https://example.com/a b", "https://example.com/a%20b"},
		{"https://e/a b/c", "https://e/a%20b/c"},
		{"https://e/p/a'b", "https://e/p/a'b"},
		{"https://e/p/a<b>", "https://e/p/a%3Cb%3E"},
		{"https://e/p/a{b}", "https://e/p/a%7Bb%7D"},
		{"https://e/p/a|b", "https://e/p/a|b"},
		{"https://e/p/a^b", "https://e/p/a%5Eb"},
		{"https://e/p/a`b", "https://e/p/a%60b"},
		{"https://e/p/a\\b", "https://e/p/a/b"},
		{`https://e/p?q=a'b`, "https://e/p?q=a%27b"},
		{`https://e/p?q=a"b`, "https://e/p?q=a%22b"},
		{"https://e/p?q=a<b>c", "https://e/p?q=a%3Cb%3Ec"},
		{"https://e/p?q=a{b}c", "https://e/p?q=a{b}c"},
		{"https://e/p?q=a\\b", "https://e/p?q=a\\b"},
		{"https://e/p?q=a`b", "https://e/p?q=a`b"},
		{"https://e/p?q=a b", "https://e/p?q=a%20b"},
		{"https://example.com?q=a b", "https://example.com/?q=a%20b"},
		{`https://e/?q='a'`, "https://e/?q=%27a%27"},
		{"https://e/?q=a|b", "https://e/?q=a|b"},
		{"https://e/p?q=[1]", "https://e/p?q=[1]"},
		{"https://e/p?q=^", "https://e/p?q=^"},
		{"https://e/p?q=a+b", "https://e/p?q=a+b"},
		{"https://e/p?q=a&=b", "https://e/p?q=a&=b"},
		{"https://e/p?q=%41", "https://e/p?q=%41"},
		{"https://e/p/a%41", "https://e/p/a%41"},
		{"https://example.com/a%20b", "https://example.com/a%20b"},
		{"https://example.com/a%zz", "https://example.com/a%zz"},
		{"https://example.com/%7euser", "https://example.com/%7euser"},

		// non-ASCII path/query/host
		{"https://example.com/ü", "https://example.com/%C3%BC"},
		{"https://example.com/x?q=ü", "https://example.com/x?q=%C3%BC"},
		{"https://bücher.example/x", "https://xn--bcher-kva.example/x"},
		{"https://xn--bcher-kva.example/x", "https://xn--bcher-kva.example/x"},
		{"https://例子.测试/x", "https://xn--fsqu00a.xn--0zwm56d/x"},
		{"https://%E4%BE%8B.com/x", "https://xn--fsq.com/x"},

		// userinfo and IPv6
		{"https://user:pass@example.com/x", "https://user:pass@example.com/x"},
		{"https://user@e/p", "https://user@e/p"},
		{"https://USER:pa ss@e/p", "https://USER:pa%20ss@e/p"},
		{"https://[2001:db8::1]/x", "https://[2001:db8::1]/x"},
		{"http://[::1]/x", "http://[::1]/x"},

		// empty query is preserved, empty fragment is not
		{"https://e/?", "https://e/?"},
		{"https://e/#", "https://e/"},

		// host edge cases the URL parser accepts verbatim
		{"https://a_b.com/x", "https://a_b.com/x"},
		{"https://a}b.com/x", "https://a}b.com/x"},
		{"https://a{b.com/x", "https://a{b.com/x"},
		{"https://a%7Db.com/x", "https://a}b.com/x"},
		{"https://a%2Eb/x", "https://a.b/x"},
		{"https://a%2Db.com/x", "https://a-b.com/x"},
		{"https://a..b/x", "https://a..b/x"},
		{"https://.x/x", "https://.x/x"},
		{"https://x../x", "https://x../x"},
		{"https://-x/x", "https://-x/x"},
		{"https://x-.com/x", "https://x-.com/x"},
	}

	for _, tc := range cases {
		got, ok := OAuthAppsNormalizeRedirectURIs([]any{tc.in})
		if !ok {
			t.Errorf("%q was rejected, want %q", tc.in, tc.want)
			continue
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%q -> %v, want %q", tc.in, got, tc.want)
		}
	}
}

// Everything `new URL` throws on must be rejected, and so must non-https
// non-local schemes (the route's own guard).
func TestOAuthAppsNormalizeRedirectURIsRejectsInvalid(t *testing.T) {
	invalid := []any{
		"",                            // empty
		"//example.com/x",             // protocol-relative: throws
		"https://",                    // empty host: throws
		"https://example.com:abc/x",   // non-numeric port: throws
		"http://::1/x",                // bare IPv6: throws
		"https://exa mple.com/x",      // space in host: throws
		"https://exa%20mple.com/x",    // decoded space in host: throws
		"https://a<b.com/x",           // forbidden host code point: throws
		"https://a|b.com/x",           // forbidden host code point: throws
		"https://a^b.com/x",           // forbidden host code point: throws
		"https://a%2Fb.com/x",         // decoded slash in host: throws
		"https://exa%00mple.com/x",    // NUL in host: throws
		"https://example.com:99999/x", // port > 65535: throws
		"ftp://x/y",                   // non-https
		"javascript:alert(1)",         // non-https
		"mailto:a@b",                  // non-https
		"http://example.com/x",        // plain http to a non-local host
		"http://e:443/",               // plain http, non-local host (port is irrelevant)
		"http://localhost.evil.com/x", // not one of the three local names
		strings.Repeat("a", 501),      // over the 500-char cap
	}
	for _, value := range invalid {
		if got, ok := OAuthAppsNormalizeRedirectURIs([]any{value}); ok {
			t.Errorf("%#v was accepted as %v", value, got)
		}
	}

	// Structural rejections.
	if _, ok := OAuthAppsNormalizeRedirectURIs([]any{}); ok {
		t.Error("empty list must be rejected")
	}
	if _, ok := OAuthAppsNormalizeRedirectURIs("https://example.com/cb"); ok {
		t.Error("a bare string must be rejected (not an array)")
	}
	if _, ok := OAuthAppsNormalizeRedirectURIs(nil); ok {
		t.Error("nil must be rejected")
	}
	if _, ok := OAuthAppsNormalizeRedirectURIs([]any{42}); ok {
		t.Error("a non-string entry must be rejected")
	}
	if _, ok := OAuthAppsNormalizeRedirectURIs([]any{"   "}); ok {
		t.Error("a blank entry must be rejected")
	}
	tooMany := make([]any, 21)
	for i := range tooMany {
		tooMany[i] = "https://example.com/cb"
	}
	if _, ok := OAuthAppsNormalizeRedirectURIs(tooMany); ok {
		t.Error("more than 20 entries must be rejected")
	}
	// 20 is inclusive and duplicates are de-duplicated afterwards, so the length
	// check happens on the *raw* input.
	twenty := make([]any, 20)
	for i := range twenty {
		twenty[i] = "https://example.com/cb"
	}
	if got, ok := OAuthAppsNormalizeRedirectURIs(twenty); !ok || len(got) != 1 {
		t.Errorf("20 duplicates = %v,%v want one entry", got, ok)
	}
}

// Localhost http is the one non-https allowance, and duplicates collapse in
// first-seen order after normalization.
func TestOAuthAppsNormalizeRedirectURIsLocalAndDedup(t *testing.T) {
	for _, local := range []string{
		"http://localhost/cb",
		"http://localhost:3000/cb",
		"http://127.0.0.1/cb",
		"http://127.0.0.1:8080/cb",
		"http://[::1]/cb",
		"http://[::1]:3000/cb",
	} {
		if _, ok := OAuthAppsNormalizeRedirectURIs([]any{local}); !ok {
			t.Errorf("%q must be accepted", local)
		}
	}

	got, ok := OAuthAppsNormalizeRedirectURIs([]any{
		"https://example.com/cb",
		"https://example.com/cb#frag",
		"https://EXAMPLE.com/cb",
		"https://other.example.com/cb",
	})
	if !ok {
		t.Fatal("valid list was rejected")
	}
	want := []string{"https://example.com/cb", "https://other.example.com/cb"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dedup = %v, want %v", got, want)
	}
}

// ==================== oauth apps: names & scopes ====================

func TestOAuthAppsNormalizeName(t *testing.T) {
	if name, ok := OAuthAppsNormalizeName("  My App  "); !ok || name != "My App" {
		t.Errorf("name = %q,%v", name, ok)
	}
	if _, ok := OAuthAppsNormalizeName(""); ok {
		t.Error("empty name must be rejected")
	}
	if _, ok := OAuthAppsNormalizeName("   "); ok {
		t.Error("whitespace-only name must be rejected")
	}
	if _, ok := OAuthAppsNormalizeName(nil); ok {
		t.Error("null name must be rejected")
	}
	if _, ok := OAuthAppsNormalizeName(42); ok {
		t.Error("non-string name must be rejected")
	}
	if _, ok := OAuthAppsNormalizeName(strings.Repeat("a", 121)); ok {
		t.Error("121-char name must be rejected")
	}
	// 120 is inclusive, and astral characters count as two UTF-16 units.
	if _, ok := OAuthAppsNormalizeName(strings.Repeat("a", 120)); !ok {
		t.Error("120-char name must be accepted")
	}
	if _, ok := OAuthAppsNormalizeName(strings.Repeat("😀", 61)); ok {
		t.Error("61 astral chars = 122 UTF-16 units, must be rejected")
	}
}

func TestOAuthAppsNormalizeScopes(t *testing.T) {
	// Arrays, whitespace trimmed, duplicates dropped in first-seen order.
	got, ok := OAuthAppsNormalizeScopes([]any{
		"profile:read", "  balance:read ", "profile:read",
	})
	if !ok || len(got) != 2 || got[0] != "profile:read" || got[1] != "balance:read" {
		t.Fatalf("array scopes = %v,%v", got, ok)
	}

	// A space-separated string splits on /\s+/.
	got, ok = OAuthAppsNormalizeScopes("profile:read\tbalance:read\n")
	if !ok || len(got) != 2 || got[0] != "profile:read" {
		t.Fatalf("string scopes = %v,%v", got, ok)
	}

	// Unknown scopes reject the whole list.
	if _, ok := OAuthAppsNormalizeScopes([]any{"profile:read", "root:all"}); ok {
		t.Error("unknown scope must reject the list")
	}
	// Empty / blank / wrong types.
	if _, ok := OAuthAppsNormalizeScopes([]any{}); ok {
		t.Error("empty scopes must be rejected")
	}
	if _, ok := OAuthAppsNormalizeScopes([]any{"", "  "}); ok {
		t.Error("blank-only scopes must be rejected")
	}
	if _, ok := OAuthAppsNormalizeScopes(nil); ok {
		t.Error("null scopes must be rejected")
	}
	if _, ok := OAuthAppsNormalizeScopes(42); ok {
		t.Error("a number must be rejected")
	}
	tooMany := make([]any, 25)
	for i := range tooMany {
		tooMany[i] = "profile:read"
	}
	if _, ok := OAuthAppsNormalizeScopes(tooMany); ok {
		t.Error("more than 24 raw entries must be rejected")
	}

	// Every catalog scope is accepted, in catalog order.
	all := make([]any, 0, len(OAuthAppsPublicAPIScopes))
	for _, scope := range OAuthAppsPublicAPIScopes {
		all = append(all, scope)
	}
	if got, ok := OAuthAppsNormalizeScopes(all); !ok || len(got) != len(all) || got[0] != all[0] {
		t.Errorf("full catalog rejected: %v,%v", got, ok)
	}
}

// serializeScopes degrades invalid stored values to [] — never null.
func TestOAuthAppsSerializeScopes(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("null"), []byte("{}"), []byte("[]"), []byte(`["bad"]`)} {
		got := OAuthAppsSerializeScopes(raw)
		if got == nil || len(got) != 0 {
			t.Errorf("SerializeScopes(%q) = %#v, want an empty slice", raw, got)
		}
	}
	got := OAuthAppsSerializeScopes([]byte(`["profile:read","balance:read"]`))
	if len(got) != 2 || got[0] != "profile:read" {
		t.Errorf("SerializeScopes = %v", got)
	}
	// redirectUris behaves the same way, keeping only string entries.
	for _, raw := range [][]byte{nil, []byte("{}"), []byte("null")} {
		if got := oauthAppsRedirectURIsJSON(raw); got == nil || len(got) != 0 {
			t.Errorf("redirectUris(%q) = %#v", raw, got)
		}
	}
	if got := oauthAppsRedirectURIsJSON([]byte(`["https://a/cb",2,null]`)); len(got) != 1 {
		t.Errorf("redirectUris filter = %v", got)
	}
}

// ==================== oauth apps: secrets ====================

// Secret shape, digest, and the constant-time comparison. The plaintext secret is
// never stored, so only the digest and the lengths are contract.
func TestOAuthAppsSecretsAndHashes(t *testing.T) {
	clientID, err := oauthAppsGenerateClientID()
	if err != nil {
		t.Fatalf("client id: %v", err)
	}
	if !strings.HasPrefix(clientID, OAuthAppsClientIDPrefix) {
		t.Errorf("client id %q lacks the %q prefix", clientID, OAuthAppsClientIDPrefix)
	}
	// randomBytes(18) -> 24 unpadded base64url chars.
	if got, want := len(clientID)-len(OAuthAppsClientIDPrefix), 24; got != want {
		t.Errorf("client id body length = %d, want %d", got, want)
	}

	secret, err := oauthAppsGenerateClientSecret()
	if err != nil {
		t.Fatalf("client secret: %v", err)
	}
	if !strings.HasPrefix(secret, OAuthAppsClientSecretPrefix) {
		t.Errorf("secret %q lacks the %q prefix", secret, OAuthAppsClientSecretPrefix)
	}
	// randomBytes(32) -> 43 unpadded base64url chars.
	if got, want := len(secret)-len(OAuthAppsClientSecretPrefix), 43; got != want {
		t.Errorf("secret body length = %d, want %d", got, want)
	}
	if strings.ContainsAny(secret, "+/=") {
		t.Errorf("secret %q is not unpadded base64url", secret)
	}

	hash := OAuthAppsHashSecret("posec_abc")
	expected := sha256.Sum256([]byte("posec_abc"))
	if want := hex.EncodeToString(expected[:]); hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want a sha256 hex digest", len(hash))
	}
	if hash != OAuthAppsHashSecret("posec_abc") {
		t.Error("hash is not deterministic")
	}
	if hash == OAuthAppsHashSecret("posec_abd") {
		t.Error("different secrets must hash differently")
	}

	if !OAuthAppsVerifyClientSecret(hash, "posec_abc") {
		t.Error("correct secret must verify")
	}
	if OAuthAppsVerifyClientSecret(hash, "posec_xyz") {
		t.Error("wrong secret must not verify")
	}
	if OAuthAppsVerifyClientSecret(hash, "posec_abc ") {
		t.Error("trailing space must not verify")
	}
	if OAuthAppsVerifyClientSecret("", "") {
		t.Error("empty digests must not compare equal")
	}
}

// ==================== oauth apps: LIKE escaping & serialization ====================

// Prisma's `contains` escapes LIKE metacharacters, so an operator searching "a_b"
// must not match "axb".
func TestOAuthAppsEscapeLike(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a_b", `a\_b`},
		{"100%", `100\%`},
		{`back\slash`, `back\\slash`},
		{`a%_\`, `a\%\_\\`},
	}
	for _, tc := range cases {
		if got := oauthAppsEscapeLike(tc.in); got != tc.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The wire shape of a grant row: `active` is falsy when the grant is revoked OR the
// app is disabled, and revokedAt is null rather than "".
func TestOAuthAppsAdminAuthorizationSerialization(t *testing.T) {
	lastAuthorized := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	row := oauthAppsGrantRow{
		ID:               5,
		AppID:            2,
		UserID:           9,
		Scopes:           []byte(`["profile:read"]`),
		LastAuthorizedAt: lastAuthorized,
		CreatedAt:        lastAuthorized,
		UpdatedAt:        lastAuthorized,
		AppName:          "Partner",
		AppClientID:      "pocli_abc",
		AppEnabled:       true,
		Username:         stringPtr("alice"),
		Role:             stringPtr("user"),
		Status:           stringPtr("active"),
	}

	active := oauthAppsAdminAuthorization(row, OAuthAppsTokenStats{
		ActiveAccessTokens:  2,
		ActiveRefreshTokens: 1,
	})
	if !active.Active {
		t.Error("an unrevoked grant of an enabled app must be active")
	}
	if active.RevokedAt != nil {
		t.Errorf("revokedAt = %v, want null", active.RevokedAt)
	}
	if active.LastAuthorizedAt != "2026-10-29T12:00:00.000Z" {
		t.Errorf("lastAuthorizedAt = %q", active.LastAuthorizedAt)
	}
	if active.User.Username != "alice" || active.User.Role != "user" || active.User.Status != "active" {
		t.Errorf("user projection = %+v", active.User)
	}
	if active.TokenStats.ActiveAccessTokens != 2 || active.TokenStats.ActiveRefreshTokens != 1 {
		t.Errorf("tokenStats = %+v", active.TokenStats)
	}

	// A disabled app makes the same grant inactive.
	disabled := row
	disabled.AppEnabled = false
	if oauthAppsAdminAuthorization(disabled, OAuthAppsTokenStats{}).Active {
		t.Error("a grant of a disabled app must not be active")
	}

	// A revoked grant reports the timestamp and is inactive.
	revokedAt := time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)
	revoked := row
	revoked.RevokedAt = &revokedAt
	authorization := oauthAppsAdminAuthorization(revoked, OAuthAppsTokenStats{})
	if authorization.Active {
		t.Error("a revoked grant must not be active")
	}
	if authorization.RevokedAt == nil || *authorization.RevokedAt != "2026-10-30T01:02:03.000Z" {
		t.Errorf("revokedAt = %v", authorization.RevokedAt)
	}
}

// An app row never serializes its secret digest, and createdByUsername is null when
// the creator relation is missing.
func TestOAuthAppsClientAppSerialization(t *testing.T) {
	created := time.Date(2026, 10, 29, 12, 0, 0, 500_000_000, time.UTC)
	row := oauthAppsRow{
		ID:               3,
		Name:             "Partner",
		ClientID:         "pocli_abc",
		ClientSecretHash: "deadbeef",
		RedirectURIs:     []byte(`["https://example.com/cb"]`),
		Scopes:           []byte(`["profile:read"]`),
		Enabled:          true,
		CreatedByUserID:  1,
		CreatedAt:        created,
		UpdatedAt:        created,
		Username:         stringPtr("admin"),
	}

	app := oauthAppsClientApp(row)
	if app.CreatedByUsername == nil || *app.CreatedByUsername != "admin" {
		t.Errorf("createdByUsername = %v", app.CreatedByUsername)
	}
	if app.CreatedAt != "2026-10-29T12:00:00.500Z" || app.UpdatedAt != app.CreatedAt {
		t.Errorf("timestamps = %q / %q", app.CreatedAt, app.UpdatedAt)
	}

	encoded, err := json.Marshal(app)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"deadbeef", "clientSecretHash", "client_secret_hash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("serialized app leaks %q: %s", forbidden, encoded)
		}
	}
	// redirectUris must ride as an array, never null.
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["redirectUris"].([]any); !ok {
		t.Fatalf("redirectUris = %#v, want an array", decoded["redirectUris"])
	}

	// No creator relation → null.
	noCreator := row
	noCreator.Username = nil
	if got := oauthAppsClientApp(noCreator); got.CreatedByUsername != nil {
		t.Errorf("createdByUsername = %v, want null", got.CreatedByUsername)
	}
	emptyCreator := row
	emptyCreator.Username = stringPtr("")
	if got := oauthAppsClientApp(emptyCreator); got.CreatedByUsername != nil {
		t.Errorf("empty username must serialize as null, got %v", got.CreatedByUsername)
	}
}

func stringPtr(value string) *string { return &value }
