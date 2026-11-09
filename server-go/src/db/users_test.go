// Unit tests for the db/users.go port.
//
// There is no live database in `go test` (see src/db/balance_test.go): the
// DB-backed half of the contract is pinned by the dual-run conformance suite in
// server-go/test/conformance/requests-users.json. What is anchored here are the
// parts that silently break API compatibility when they drift:
//
//   - the 2FA state machine is FOUR writes, and each step writes exactly its own
//     column (a "helpful" merge of two steps is how the Node backend once shipped
//     "2FA enabled, no recovery codes");
//   - Disable2FAComplete clears all three columns (the rollback);
//   - the empty-update early returns exist (Prisma would otherwise bump @updatedAt);
//   - the register-gift clamping is Math.min/Math.max/Math.round/Math.floor, and the
//     inbox copy is byte-identical;
//   - the lookups are (nil, nil) when absent — modelled by requiring a DB at all
//     rather than fabricating a zero record;
//   - the search WHERE builder matches the Prisma SQL fragment (ILIKE + ESCAPE,
//     exact match lowercasing, id cast to text).
package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestAvatarStylesMatchRouteEnum pins the exact list and order: the PATCH /:id
// body schema enumerates these values, so a change here changes validation.
func TestAvatarStyles(t *testing.T) {
	want := []string{
		"", // the route schema allows '' to reset the avatar
		"adventurer", "adventurerNeutral", "avataaars", "avataaarsNeutral",
		"bigEars", "bigEarsNeutral", "bigSmile", "bottts", "botttsNeutral",
		"croodles", "croodlesNeutral", "dylan", "funEmoji", "glass", "icons",
		"identicon", "initials", "lorelei", "loreleiNeutral", "micah", "miniavs",
		"notionists", "notionistsNeutral", "openPeeps", "personas", "pixelArt",
		"pixelArtNeutral", "rings", "shapes", "thumbs",
	}
	if len(AvatarStyles) != len(want)-1 {
		t.Fatalf("len(AvatarStyles) = %d, want %d", len(AvatarStyles), len(want)-1)
	}
	for index, style := range AvatarStyles {
		if style != want[index+1] {
			t.Fatalf("AvatarStyles[%d] = %q, want %q", index, style, want[index+1])
		}
	}
}

// TestTwoFAStateMachineSQL pins the four-step setup and the rollback. Each step is
// a separate statement in server/src/db/users.ts; merging them by accident is a
// real regression the Node backend has seen.
func TestTwoFAStateMachineSQL(t *testing.T) {
	cases := []struct {
		name       string
		statement  string
		mustSet    []string
		mustNotSet []string
		mustClear  []string
		args       int
	}{
		{
			name:      "step1 save2FASecret stores only the pending secret",
			statement: usersSQLSave2FASecret,
			mustSet:   []string{"two_factor_secret"},
			mustNotSet: []string{
				"two_factor_enabled", "two_factor_recovery_codes",
			},
			args: 2,
		},
		{
			name:      "step2 enable2FA flips only the flag",
			statement: usersSQLEnable2FA,
			mustSet:   []string{"two_factor_enabled = true"},
			mustNotSet: []string{
				"two_factor_secret", "two_factor_recovery_codes",
			},
			args: 1,
		},
		{
			name:      "step3 save2FARecoveryCodes stores only the codes",
			statement: usersSQLSave2FARecoveryCodes,
			mustSet:   []string{"two_factor_recovery_codes"},
			mustNotSet: []string{
				"two_factor_enabled", "two_factor_secret",
			},
			args: 2,
		},
		{
			name:      "step4 enable2FAWithRecoveryCodes sets flag and codes together",
			statement: usersSQLEnable2FAWithRecoveryCodes,
			mustSet:   []string{"two_factor_enabled = true", "two_factor_recovery_codes"},
			mustNotSet: []string{
				"two_factor_secret",
			},
			args: 2,
		},
		{
			name:      "disable2FA clears the flag and the secret but keeps codes",
			statement: usersSQLDisable2FA,
			mustSet:   []string{"two_factor_enabled = false", "two_factor_secret = NULL"},
			mustNotSet: []string{
				"two_factor_recovery_codes = NULL",
			},
			args: 1,
		},
		{
			name:      "rollback disable2FAComplete clears flag, secret and codes",
			statement: usersSQLDisable2FAComplete,
			mustSet:   []string{"two_factor_enabled = false", "two_factor_secret = NULL", "two_factor_recovery_codes = NULL"},
			args:      1,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, fragment := range testCase.mustSet {
				if !strings.Contains(testCase.statement, fragment) {
					t.Fatalf("statement does not set %q:\n%s", fragment, testCase.statement)
				}
			}
			for _, fragment := range testCase.mustNotSet {
				if strings.Contains(testCase.statement, fragment) {
					t.Fatalf("statement must not touch %q:\n%s", fragment, testCase.statement)
				}
			}
			if got := strings.Count(testCase.statement, "?"); got != testCase.args {
				t.Fatalf("placeholder count = %d, want %d:\n%s", got, testCase.args, testCase.statement)
			}
			if !strings.Contains(testCase.statement, "updated_at = NOW()") {
				t.Fatal("every 2FA write must bump updated_at (Prisma @updatedAt)")
			}
			if !strings.Contains(testCase.statement, "WHERE id = ?") {
				t.Fatal("2FA writes must target a single user id")
			}
		})
	}
}

// TestTwoFAWritesRequireDatabase proves every 2FA writer is a real DB write and
// fails loudly instead of silently succeeding when the pool is missing.
func TestTwoFAWritesRequireDatabase(t *testing.T) {
	previous := DB
	DB = nil
	defer func() { DB = previous }()

	ctx := context.Background()
	writers := map[string]func() error{
		"Save2FASecret":              func() error { return Save2FASecret(ctx, 1, "ciphertext") },
		"Enable2FA":                  func() error { return Enable2FA(ctx, 1) },
		"Disable2FA":                 func() error { return Disable2FA(ctx, 1) },
		"Save2FARecoveryCodes":       func() error { return Save2FARecoveryCodes(ctx, 1, "[1,2]") },
		"Enable2FAWithRecoveryCodes": func() error { return Enable2FAWithRecoveryCodes(ctx, 1, "[1,2]") },
		"Disable2FAComplete":         func() error { return Disable2FAComplete(ctx, 1) },
	}
	for name, writer := range writers {
		if err := writer(); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
			t.Fatalf("%s error = %v, want ErrUsersDatabaseNotInitialized", name, err)
		}
	}
}

// TestEmptyUpdateGuards pins the `if (Object.keys(updateData).length === 0) return`
// guards. With DB == nil an early return is the only way these can return nil; a
// non-empty update must fail with ErrUsersDatabaseNotInitialized instead of
// silently issuing an UPDATE that only bumps updated_at.
func TestEmptyUpdateGuards(t *testing.T) {
	previous := DB
	DB = nil
	defer func() { DB = previous }()

	ctx := context.Background()

	if err := UpdateUser(ctx, 1, UserUpdate{}); err != nil {
		t.Fatalf("UpdateUser(empty) = %v, want nil (early return)", err)
	}
	if err := UpdateUserQuotaLimits(ctx, 1, UserQuotaUpdate{}); err != nil {
		t.Fatalf("UpdateUserQuotaLimits(empty) = %v, want nil (early return)", err)
	}
	if err := UpdateUserQuotaUsed(ctx, 1, UserQuotaUsedUpdate{}); err != nil {
		t.Fatalf("UpdateUserQuotaUsed(empty) = %v, want nil (early return)", err)
	}

	hostLimit := int32(3)
	if err := UpdateUserQuotaLimits(ctx, 1, UserQuotaUpdate{HostLimit: &hostLimit}); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("UpdateUserQuotaLimits(non-empty) = %v, want ErrUsersDatabaseNotInitialized", err)
	}
	if err := UpdateUser(ctx, 1, UserUpdate{AvatarStyle: strPtr("")}); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("UpdateUser(non-empty) = %v, want ErrUsersDatabaseNotInitialized", err)
	}
}

// usersStringPtr is local to these tests; the db package already has strPtr in storage_configs_test.go.
func usersStringPtr(value string) *string { return &value }

// TestUpdateAssignmentsOrder pins the SET list order, which mirrors the
// TypeScript object insertion order and therefore which column a partial write
// touches first.
func TestUsersUpdateAssignments(t *testing.T) {
	email := "a@b.co"
	role := models.UserRoleAdmin
	status := models.UserStatusBanned
	hash := "$2a$12$hash"
	avatar := "rings"

	sets, args := usersUpdateAssignments(UserUpdate{
		Email: &email, Role: &role, Status: &status, PasswordHash: &hash, AvatarStyle: &avatar,
	})
	wantSets := []string{"email = ?", "role = ?", "status = ?", "password_hash = ?", "avatar_style = ?"}
	if len(sets) != len(wantSets) {
		t.Fatalf("sets = %v, want %v", sets, wantSets)
	}
	for index, want := range wantSets {
		if sets[index] != want {
			t.Fatalf("sets[%d] = %q, want %q", index, sets[index], want)
		}
	}
	if len(args) != 5 || args[0] != email || args[1] != "admin" || args[2] != "banned" || args[3] != hash || args[4] != avatar {
		t.Fatalf("args = %#v", args)
	}

	if sets, args := usersUpdateAssignments(UserUpdate{}); len(sets) != 0 || len(args) != 0 {
		t.Fatalf("empty update produced %v / %#v", sets, args)
	}
}

func TestUsersQuotaUpdateAssignments(t *testing.T) {
	host := int32(1)
	friend := int32(2)
	packages := int32(3)
	sets, args := usersQuotaUpdateAssignments(UserQuotaUpdate{HostLimit: &host, FriendLimit: &friend, PackageLimit: &packages})
	if strings.Join(sets, ",") != "host_limit = ?,friend_limit = ?,package_limit = ?" {
		t.Fatalf("sets = %v", sets)
	}
	if len(args) != 3 || args[0] != int32(1) || args[1] != int32(2) || args[2] != int32(3) {
		t.Fatalf("args = %#v", args)
	}
}

// TestSentinels pins the error strings the routes switch on.
func TestSentinels(t *testing.T) {
	cases := map[string]error{
		"USER_EXISTS":              ErrUserExists,
		"EMAIL_ALREADY_REGISTERED": ErrEmailAlreadyRegistered,
		"INVITE_CODE_UNAVAILABLE":  ErrInviteCodeUnavailable,
	}
	for message, sentinel := range cases {
		if sentinel.Error() != message {
			t.Fatalf("sentinel %v has message %q, want %q", sentinel, sentinel.Error(), message)
		}
	}
	if ErrUserDeletionConflict.Error() != "User has associated records and cannot be deleted" {
		t.Fatalf("ErrUserDeletionConflict = %q", ErrUserDeletionConflict.Error())
	}
	// Prisma's update()/delete() throw P2025 when no row matches; every single-row
	// writer in this module maps zero rows to this sentinel.
	if ErrUsersRecordNotFound.Error() != "users: record to update not found" {
		t.Fatalf("ErrUsersRecordNotFound = %q", ErrUsersRecordNotFound.Error())
	}
}

// TestNormalizeRegisterGift pins the Math.min/Math.max/Math.round/floor clamp and
// its extremes (including the JS negative-value behaviour after clamping to 0).
func TestNormalizeRegisterGift(t *testing.T) {
	cases := []struct {
		rawBalance float64
		rawPoints  int
		wantBal    float64
		wantPoints int32
	}{
		{0, 0, 0, 0},
		{3.456, 10, 3.46, 10},
		{3.454, 0, 3.45, 0},
		{-1, -1, 0, 0},
		{1e12, 1 << 40, maxRegisterGiftBalance, maxRegisterGiftPoints},
		{0.005, 7, 0.01, 7},
	}
	for _, testCase := range cases {
		balance, points := normalizeRegisterGift(testCase.rawBalance, testCase.rawPoints)
		if balance != testCase.wantBal || points != testCase.wantPoints {
			t.Fatalf("normalizeRegisterGift(%v, %v) = (%v, %v), want (%v, %v)",
				testCase.rawBalance, testCase.rawPoints, balance, points, testCase.wantBal, testCase.wantPoints)
		}
	}
}

// TestBuildRegisterGiftMessage pins the exact user-facing copy.
func TestBuildRegisterGiftMessage(t *testing.T) {
	cases := []struct {
		name        string
		gift        RegisterGiftResult
		wantSummary string
		wantArrival string
	}{
		{"balance", RegisterGiftResult{BalanceAmount: 12.5}, "余额 ¥12.50", "余额 ¥12.50"},
		{"points", RegisterGiftResult{PointsAmount: 30}, "30 积分", "30 积分"},
		{"both", RegisterGiftResult{BalanceAmount: 1, PointsAmount: 2}, "余额 ¥1.00 + 2 积分", "余额和积分"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			summary, content := buildRegisterGiftMessage(testCase.gift)
			if summary != testCase.wantSummary {
				t.Fatalf("summary = %q, want %q", summary, testCase.wantSummary)
			}
			if !strings.HasPrefix(content, "欢迎加入白嫖站！系统已经把 "+testCase.wantSummary+" 塞进你的账户背包，"+testCase.wantArrival) {
				t.Fatalf("content = %q", content)
			}
			if !strings.HasSuffix(content, "到账了。快去逛逛能薅点什么，优雅白嫖，从注册成功这一刻开始。") {
				t.Fatalf("content suffix = %q", content)
			}
		})
	}
}

// TestLoginRecordAndQuotaTypesAreInt64 pins that BigInt traffic columns never
// become float64.
func TestQuotaTrafficColumnsAreInt64(t *testing.T) {
	var limit int64 = 9_007_199_254_740_993 // > 2^53: a float64 would round this
	quota := usersQuotaRow{MonthlyTrafficLimit: &limit, MonthlyTrafficUsed: limit, ExtraTrafficQuota: limit}
	record := quota.toRecord()
	if record.MonthlyTrafficLimit == nil || *record.MonthlyTrafficLimit != limit {
		t.Fatalf("monthly traffic limit lost precision: %v", record.MonthlyTrafficLimit)
	}
	if record.MonthlyTrafficUsed != limit || record.ExtraTrafficQuota != limit {
		t.Fatalf("traffic counters lost precision: %d / %d", record.MonthlyTrafficUsed, record.ExtraTrafficQuota)
	}
}

// TestUserRecordJSONShape pins the snake_case wire projection the TypeScript
// returns and that routes/auth.ts consumes.
func TestUserRecordJSONShape(t *testing.T) {
	email := "a@b.co"
	badge := "badge-1"
	record := UserRecord{
		ID: 4, Username: "alice", Email: &email, PasswordHash: "",
		Role: models.UserRoleUser, Status: models.UserStatusActive,
		AvatarStyle: "rings", AvatarBadgeID: &badge,
		CreatedAt: "2027-01-02T03:04:05.000Z", UpdatedAt: "2027-01-02T03:04:06.000Z",
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"id", "username", "email", "password_hash", "role", "status", "ban_reason",
		"avatar_style", "avatar_badge_id", "created_at", "updated_at",
	} {
		if _, present := decoded[key]; !present {
			t.Fatalf("missing key %q in %s", key, encoded)
		}
	}
	if decoded["password_hash"] != "" {
		t.Fatalf("password_hash must never be populated by list/lookup projections: %v", decoded["password_hash"])
	}
	if decoded["created_at"] != "2027-01-02T03:04:05.000Z" {
		t.Fatalf("created_at = %v", decoded["created_at"])
	}
}

// TestBuildUserSearchWhereClause pins the SQL fragment and its arguments against
// db/pagination.ts buildUserSearchWhereClause.
func TestBuildUserSearchWhereClause(t *testing.T) {
	if clause, args := buildUserSearchWhereClause("   ", nil, false); clause != "" || len(args) != 0 {
		t.Fatalf("blank search = %q / %#v, want no clause", clause, args)
	}

	clause, args := buildUserSearchWhereClause("ali", []string{"username", "id", "email"}, false)
	wantClause := `(u.username ILIKE ? ESCAPE '\' OR CAST(u.id AS TEXT) ILIKE ? ESCAPE '\' OR u.email IS NOT NULL AND u.email ILIKE ? ESCAPE '\')`
	if clause != wantClause {
		t.Fatalf("clause = %q, want %q", clause, wantClause)
	}
	if len(args) != 3 || args[0] != "%ali%" || args[1] != "%ali%" || args[2] != "%ali%" {
		t.Fatalf("args = %#v", args)
	}

	clause, args = buildUserSearchWhereClause("Alice", []string{"email"}, true)
	if clause != "(u.email IS NOT NULL AND LOWER(u.email) = LOWER(?))" {
		t.Fatalf("exact clause = %q", clause)
	}
	if len(args) != 1 || args[0] != "Alice" {
		t.Fatalf("exact args = %#v", args)
	}

	// Field filtering + canonical order: unknown fields are dropped, the rest keep
	// USER_SEARCH_FIELDS order.
	clause, _ = buildUserSearchWhereClause("x", []string{"email", "bogus", "username"}, false)
	if !strings.HasPrefix(clause, "(u.username") || !strings.Contains(clause, "u.email ILIKE") {
		t.Fatalf("canonical ordering broken: %q", clause)
	}
	if strings.Contains(clause, "CAST") {
		t.Fatalf("unrequested id field leaked into the clause: %q", clause)
	}
}

func TestEscapeSQLLike(t *testing.T) {
	cases := map[string]string{
		"plain":     "plain",
		"50%_off":   `50\%\_off`,
		`back\lash`: `back\\lash`,
	}
	for input, want := range cases {
		if got := escapeSQLLike(input); got != want {
			t.Fatalf("escapeSQLLike(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUsersSimilarPrefix(t *testing.T) {
	cases := map[string]string{
		"Alice123":  "alice",
		"bob_99":    "bob",
		"carol-007": "carol",
		"dave":      "dave",
		"12345":     "",
		"Eve__---1": "eve",
		"frank1x":   "frank1x",
	}
	for input, want := range cases {
		if got := usersSimilarPrefix(input); got != want {
			t.Fatalf("usersSimilarPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUserDeletionBlockersHasBlockers(t *testing.T) {
	if (UserDeletionBlockers{}).HasBlockers() {
		t.Fatal("zero blockers must not block deletion")
	}
	cases := []UserDeletionBlockers{
		{Instances: 1},
		{Hosts: 1},
		{Packages: 1},
		{BalanceNonZero: true},
		{BalanceLogs: 1},
		{RechargeRecords: 1},
		{LoginRecords: 1},
		{RedeemCodeUsages: 1},
	}
	for index, blockers := range cases {
		if !blockers.HasBlockers() {
			t.Fatalf("case %d must block deletion: %#v", index, blockers)
		}
	}
}

// TestLookupsRequireDatabase documents the (nil, nil) contract: an absent row is
// not an error, but an absent *database* is. The "row absent" half is asserted in
// the conformance suite against a real database.
func TestLookupsRequireDatabase(t *testing.T) {
	previous := DB
	DB = nil
	defer func() { DB = previous }()

	ctx := context.Background()
	if _, err := FindUserByUsername(ctx, "alice"); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("FindUserByUsername = %v", err)
	}
	if _, err := FindUserById(ctx, 1); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("FindUserById = %v", err)
	}
	if _, err := FindUserByEmail(ctx, "a@b.co"); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("FindUserByEmail = %v", err)
	}
	if _, err := FindUserByUsernameOrEmail(ctx, "alice"); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("FindUserByUsernameOrEmail = %v", err)
	}
	if _, err := GetUserQuota(ctx, 1); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("GetUserQuota = %v", err)
	}
	if _, err := GetAllAdminUserIds(ctx); !errors.Is(err, ErrUsersDatabaseNotInitialized) {
		t.Fatalf("GetAllAdminUserIds = %v", err)
	}
}
