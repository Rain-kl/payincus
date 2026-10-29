package db

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// mustMarshal encodes value and decodes it back into a generic map, so the tests
// assert on the actual wire shape rather than on struct tags alone.
func mustMarshal(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

// makeFriendshipRow builds a bare row for the pure state-machine tests.
func makeFriendshipRow(id, userID, friendID int32, status models.FriendshipStatus) *friendshipRow {
	return &friendshipRow{ID: id, UserID: userID, FriendID: friendID, Status: string(status)}
}

// TestPlanFriendRequestStateMachine pins every branch of sendFriendRequest() to
// the TypeScript ladder so the transition table cannot drift silently.
func TestPlanFriendRequestStateMachine(t *testing.T) {
	const me, other = int32(1), int32(2)

	cases := []struct {
		name        string
		existing    *friendshipRow
		wantAction  friendRequestAction
		wantRowID   int32
		description string
	}{
		{
			name:        "no existing row creates a pending request",
			existing:    nil,
			wantAction:  friendActionCreate,
			description: "fresh pair",
		},
		{
			name:        "accepted row reports ALREADY_FRIENDS",
			existing:    makeFriendshipRow(10, me, other, models.FriendshipStatusAccepted),
			wantAction:  friendActionAlreadyFriends,
			wantRowID:   10,
			description: "cannot re-friend an existing friend",
		},
		{
			name:        "accepted reverse-direction row also reports ALREADY_FRIENDS",
			existing:    makeFriendshipRow(11, other, me, models.FriendshipStatusAccepted),
			wantAction:  friendActionAlreadyFriends,
			wantRowID:   11,
			description: "uniqueness is bidirectional",
		},
		{
			name:        "same-direction pending reports REQUEST_PENDING",
			existing:    makeFriendshipRow(12, me, other, models.FriendshipStatusPending),
			wantAction:  friendActionPending,
			wantRowID:   12,
			description: "duplicate send is rejected",
		},
		{
			name:        "reverse-direction pending auto-accepts",
			existing:    makeFriendshipRow(13, other, me, models.FriendshipStatusPending),
			wantAction:  friendActionAutoAccept,
			wantRowID:   13,
			description: "crossed requests complete the friendship",
		},
		{
			name:        "rejected row is re-sent",
			existing:    makeFriendshipRow(14, me, other, models.FriendshipStatusRejected),
			wantAction:  friendActionResend,
			wantRowID:   14,
			description: "a rejected request may be retried",
		},
		{
			name:        "rejected reverse-direction row is also re-sent",
			existing:    makeFriendshipRow(15, other, me, models.FriendshipStatusRejected),
			wantAction:  friendActionResend,
			wantRowID:   15,
			description: "resend reuses the existing row regardless of direction",
		},
		{
			name:        "removed row falls through to create",
			existing:    makeFriendshipRow(16, me, other, models.FriendshipStatusRemoved),
			wantAction:  friendActionCreate,
			wantRowID:   16,
			description: "mirrors the TypeScript fall-through",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, rowID := planFriendRequest(tc.existing, me, other)
			if action != tc.wantAction {
				t.Fatalf("action = %v, want %v (%s)", action, tc.wantAction, tc.description)
			}
			if tc.wantRowID != 0 && rowID != tc.wantRowID {
				t.Fatalf("rowID = %d, want %d", rowID, tc.wantRowID)
			}
		})
	}
}

// TestFriendHistoryFilterParsing pins parseFriendHistoryFilter (the route helper
// delegates to IsFriendHistoryFilter).
func TestFriendHistoryFilterParsing(t *testing.T) {
	cases := []struct {
		filter string
		valid  bool
	}{
		{"", true},
		{"all", true},
		{"accepted", true},
		{"rejected", true},
		{"removed", true},
		{"pending", false},
		{"ACCEPTED", false},
		{"nonsense", false},
	}
	for _, tc := range cases {
		if got := IsFriendHistoryFilter(tc.filter); got != tc.valid {
			t.Errorf("IsFriendHistoryFilter(%q) = %v, want %v", tc.filter, got, tc.valid)
		}
	}
}

// TestFriendshipErrorCodes ensures the sentinel errors keep the exact strings the
// route switches on.
func TestFriendshipErrorCodes(t *testing.T) {
	cases := map[error]string{
		ErrFriendAlreadyFriends:    "ALREADY_FRIENDS",
		ErrFriendRequestPending:    "REQUEST_PENDING",
		ErrFriendRequestNotFound:   "REQUEST_NOT_FOUND",
		ErrFriendRequestNotPending: "REQUEST_NOT_PENDING",
		ErrFriendForbidden:         "FORBIDDEN",
		ErrFriendshipNotFound:      "FRIENDSHIP_NOT_FOUND",
	}
	for err, want := range cases {
		if err.Error() != want {
			t.Errorf("%v.Error() = %q, want %q", err, err.Error(), want)
		}
	}
}

// TestFriendInfoJSONShape pins the FriendInfo wire shape (camelCase, ISO dates,
// explicit nulls for email/avatarBadgeId/acceptedAt).
func TestFriendInfoJSONShape(t *testing.T) {
	info := FriendInfo{
		ID:            7,
		FriendshipID:  99,
		FriendID:      7,
		Username:      "alice",
		AvatarStyle:   "bigSmile",
		Status:        models.FriendshipStatusAccepted,
		CreatedAt:     "2026-01-02T03:04:05.000Z",
		InitiatedByMe: true,
		HostCount:     2,
		InstanceCount: 3,
	}

	encoded := mustMarshal(t, info)
	wantKeys := []string{"id", "friendshipId", "friendId", "username", "email",
		"avatarStyle", "avatarBadgeId", "status", "createdAt", "acceptedAt",
		"initiatedByMe", "hostCount", "instanceCount"}
	for _, key := range wantKeys {
		if _, present := encoded[key]; !present {
			t.Errorf("FriendInfo JSON is missing key %q", key)
		}
	}
	if encoded["email"] != nil {
		t.Errorf("email = %v, want explicit null", encoded["email"])
	}
	if encoded["acceptedAt"] != nil {
		t.Errorf("acceptedAt = %v, want explicit null", encoded["acceptedAt"])
	}
	if encoded["status"] != "accepted" {
		t.Errorf("status = %v, want \"accepted\"", encoded["status"])
	}
	// id and friendId must both carry the *friend's* user id (frontend contract).
	if encoded["id"] != float64(7) || encoded["friendId"] != float64(7) {
		t.Errorf("id/friendId = %v/%v, want 7/7", encoded["id"], encoded["friendId"])
	}
}

// TestFriendRequestJSONShape covers the pending/sent list projection.
func TestFriendRequestJSONShape(t *testing.T) {
	request := FriendRequest{
		ID:          1,
		UserID:      42,
		Username:    "bob",
		AvatarStyle: "notionists",
		CreatedAt:   "2026-05-06T07:08:09.000Z",
	}
	encoded := mustMarshal(t, request)
	for _, key := range []string{"id", "userId", "username", "email", "avatarStyle",
		"avatarBadgeId", "remark", "createdAt"} {
		if _, present := encoded[key]; !present {
			t.Errorf("FriendRequest JSON is missing key %q", key)
		}
	}
	if encoded["remark"] != nil {
		t.Errorf("remark = %v, want explicit null", encoded["remark"])
	}
}

// TestFriendshipJSONShape pins the mutation result shape (no relation keys).
func TestFriendshipJSONShape(t *testing.T) {
	now := time.Now()
	row := friendshipRow{
		ID:        5,
		UserID:    1,
		FriendID:  2,
		Status:    string(models.FriendshipStatusPending),
		CreatedAt: now,
	}
	encoded := mustMarshal(t, friendshipJSONOf(row))
	want := map[string]any{
		"id":         float64(5),
		"userId":     float64(1),
		"friendId":   float64(2),
		"status":     "pending",
		"remark":     nil,
		"acceptedAt": nil,
		"rejectedAt": nil,
	}
	for key, value := range want {
		if got := encoded[key]; got != value {
			t.Errorf("%s = %#v, want %#v", key, got, value)
		}
	}
	if _, present := encoded["createdAt"]; !present {
		t.Error("createdAt missing")
	}
}

// TestFriendshipDetailShape verifies getFriendshipById's nested user refs.
func TestFriendshipDetailShape(t *testing.T) {
	row := friendshipJoinedRow{
		ID:                9,
		UserID:            1,
		FriendID:          2,
		Status:            string(models.FriendshipStatusPending),
		CreatedAt:         time.Now(),
		UserRefID:         1,
		UserUsername:      "sender",
		UserAvatarStyle:   "bigSmile",
		FriendRefID:       2,
		FriendUsername:    "receiver",
		FriendAvatarStyle: "lorelei",
	}
	detail := friendshipDetailOf(row)
	if detail.User == nil || detail.User.Username != "sender" {
		t.Fatalf("user ref = %#v", detail.User)
	}
	if detail.Friend == nil || detail.Friend.Username != "receiver" {
		t.Fatalf("friend ref = %#v", detail.Friend)
	}
	encoded := mustMarshal(t, detail)
	if _, present := encoded["user"]; !present {
		t.Error("user key missing")
	}
	if _, present := encoded["friend"]; !present {
		t.Error("friend key missing")
	}
}

// TestFriendTimestampFormatting pins the Date#toISOString rendering.
func TestFriendTimestampFormatting(t *testing.T) {
	value := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	if got := friendTimestamp(value); got != "2026-01-02T03:04:05.006Z" {
		t.Fatalf("friendTimestamp = %q", got)
	}
	if got := friendTimestampPtr(nil); got != nil {
		t.Fatalf("friendTimestampPtr(nil) = %v, want nil", got)
	}
	pointer := friendTimestampPtr(&value)
	if pointer == nil || *pointer != "2026-01-02T03:04:05.006Z" {
		t.Fatalf("friendTimestampPtr = %v", pointer)
	}
}

// TestFriendSentinelUniqueness guards against accidental error aliasing.
func TestFriendSentinelUniqueness(t *testing.T) {
	all := []error{
		ErrFriendAlreadyFriends, ErrFriendRequestPending, ErrFriendRequestNotFound,
		ErrFriendRequestNotPending, ErrFriendForbidden, ErrFriendshipNotFound,
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("sentinel %v aliases %v", a, b)
			}
		}
	}
}
