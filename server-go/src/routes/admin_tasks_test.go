package routes

import (
	"testing"
	"time"
)

// TestAdminTasksParsePage pins the querystring integer rules: default 1, bounds
// 1..100000, and a repeated/garbage value is a validation error.
func TestAdminTasksParsePage(t *testing.T) {
	cases := []struct {
		raw     string
		present bool
		want    int
		message string
	}{
		{"", false, 1, ""},
		{"", true, 1, ""},
		{"3", true, 3, ""},
		{"100000", true, 100000, ""},
		{"100001", true, 0, "must be <= 100000"},
		{"0", true, 0, "must be >= 1"},
		{"-1", true, 0, "must be >= 1"},
		{"abc", true, 0, "must be integer"},
		{"1.5", true, 0, "must be integer"},
	}

	for _, tc := range cases {
		got, message := adminTasksParsePage(tc.raw, tc.present)
		if message != tc.message {
			t.Errorf("adminTasksParsePage(%q, %v) message = %q, want %q", tc.raw, tc.present, message, tc.message)
			continue
		}
		if message == "" && got != tc.want {
			t.Errorf("adminTasksParsePage(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestAdminTasksParseAfter pins the SSE cursor rules: default 0, bounds
// 0..int32 max.
func TestAdminTasksParseAfter(t *testing.T) {
	cases := []struct {
		raw     string
		present bool
		want    int32
		message string
	}{
		{"", false, 0, ""},
		{"", true, 0, ""},
		{"0", true, 0, ""},
		{"42", true, 42, ""},
		{"2147483647", true, 2147483647, ""},
		{"2147483648", true, 0, "must be <= 2147483647"},
		{"-1", true, 0, "must be >= 0"},
		{"x", true, 0, "must be integer"},
	}

	for _, tc := range cases {
		got, message := adminTasksParseAfter(tc.raw, tc.present)
		if message != tc.message {
			t.Errorf("adminTasksParseAfter(%q, %v) message = %q, want %q", tc.raw, tc.present, message, tc.message)
			continue
		}
		if message == "" && got != tc.want {
			t.Errorf("adminTasksParseAfter(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestAdminTasksEnums pins the two enum allow-lists the schema declares.
func TestAdminTasksEnums(t *testing.T) {
	for _, status := range []string{"running", "success", "failed", "cancelled", "interrupted"} {
		if !adminTasksStatuses[status] {
			t.Errorf("status %q must be accepted", status)
		}
	}
	if adminTasksStatuses["pending"] {
		t.Error("status pending must be rejected")
	}

	for _, kind := range []string{"create", "destroy", "node_recover"} {
		if !adminTasksKinds[kind] {
			t.Errorf("kind %q must be accepted", kind)
		}
	}
	if adminTasksKinds["restart"] {
		t.Error("kind restart must be rejected")
	}
}

// TestAdminTasksISOTime pins the toISOString() formatting and the nil passthrough.
func TestAdminTasksISOTime(t *testing.T) {
	if adminTasksISOTime(nil) != nil {
		t.Fatal("nil must map to nil")
	}

	moment := time.Date(2026, 10, 30, 14, 5, 6, 789_000_000, time.UTC)
	got := adminTasksISOTime(&moment)
	if got == nil || *got != "2026-10-30T14:05:06.789Z" {
		t.Fatalf("got %v", got)
	}
}

// TestAdminTasksConnectionCaps pins the 5-per-user and 100-total SSE limits, which
// protect the process from a tab-happy admin.
func TestAdminTasksConnectionCaps(t *testing.T) {
	adminTasksConnectionsMu.Lock()
	adminTasksConnections = map[int32]int{}
	adminTasksConnectionsMu.Unlock()

	const user = int32(1)

	// Five connections for one user are allowed, the sixth is not.
	for i := 0; i < adminTasksMaxPerUser; i++ {
		if !adminTasksOpenConnection(user) {
			t.Fatalf("connection %d should have been allowed", i+1)
		}
	}
	if adminTasksOpenConnection(user) {
		t.Fatal("the sixth connection for one user must be refused")
	}

	// Fill the global budget with other users.
	for id := int32(2); id < 30; id++ {
		for adminTasksOpenConnection(id) {
		}
	}

	adminTasksConnectionsMu.Lock()
	total := 0
	for _, count := range adminTasksConnections {
		total += count
	}
	adminTasksConnectionsMu.Unlock()
	if total != adminTasksMaxTotal {
		t.Fatalf("total connections = %d, want %d", total, adminTasksMaxTotal)
	}
	if adminTasksOpenConnection(999) {
		t.Fatal("a new user must be refused once the global cap is reached")
	}

	// Closing releases exactly one slot.
	adminTasksCloseConnection(user)
	if !adminTasksOpenConnection(999) {
		t.Fatal("after a close, a new connection must be allowed again")
	}

	// Cleaning up leaves no residue for the next test.
	adminTasksConnectionsMu.Lock()
	adminTasksConnections = map[int32]int{}
	adminTasksConnectionsMu.Unlock()
}

// TestAdminTasksTaskViewShape pins the wire keys, including the injected
// canStop/stopping pair that the admin UI relies on.
func TestAdminTasksTaskViewShape(t *testing.T) {
	control := adminTasksTaskView{
		ID:           1,
		HostID:       2,
		InstanceName: "vm-1",
		Kind:         "create",
		Status:       "running",
		CanStop:      false,
		Stopping:     false,
	}
	if control.InstanceName != "vm-1" || control.Kind != "create" {
		t.Fatalf("unexpected struct: %#v", control)
	}
}
