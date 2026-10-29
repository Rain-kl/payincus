package lib

import "testing"

// TestCheckHostPermission pins the host-level rule: admins always win, the host
// owner wins, everyone else is denied.
func TestCheckHostPermission(t *testing.T) {
	cases := []struct {
		name    string
		user    AuthUser
		ownerID int32
		allowed bool
		kind    PermissionType
	}{
		{"admin manages any host", AuthUser{ID: 9, Role: "admin"}, 1, true, PermissionAdmin},
		{"host owner manages own host", AuthUser{ID: 1, Role: "user"}, 1, true, PermissionOwner},
		{"other user is denied", AuthUser{ID: 2, Role: "user"}, 1, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := CheckHostPermission(tc.user, tc.ownerID)
			if result.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v", result.Allowed, tc.allowed)
			}
			if tc.allowed && result.PermissionType != tc.kind {
				t.Fatalf("PermissionType = %q, want %q", result.PermissionType, tc.kind)
			}
		})
	}
}

// TestCheckPackagePermission pins that package operations are owner-only and
// that admins get NO special power (unlike instance/host checks).
func TestCheckPackagePermission(t *testing.T) {
	if !CheckPackagePermission(AuthUser{ID: 3, Role: "user"}, 3).Allowed {
		t.Fatal("package owner must be allowed")
	}
	if CheckPackagePermission(AuthUser{ID: 3, Role: "user"}, 4).Allowed {
		t.Fatal("non-owner must be denied")
	}
	if CheckPackagePermission(AuthUser{ID: 99, Role: "admin"}, 4).Allowed {
		t.Fatal("admin must NOT inherit package ownership")
	}
}

// TestCheckOwnership covers both ownership helpers, which the TS defines twice
// with identical bodies.
func TestCheckOwnership(t *testing.T) {
	user := AuthUser{ID: 5, Role: "user"}
	if !CheckOwnership(user, 5).Allowed {
		t.Fatal("owner must be allowed")
	}
	if CheckOwnership(user, 6).Allowed {
		t.Fatal("non-owner must be denied")
	}
	if !CheckStrictOwnership(user, 5).Allowed {
		t.Fatal("strict ownership must allow the owner")
	}
	if CheckStrictOwnership(AuthUser{ID: 7, Role: "admin"}, 6).Allowed {
		t.Fatal("strict ownership must not grant admins ownership")
	}
}

// TestOwnerOnlyOperationsTable pins the transfer/quota/backup/ssh/rebuild/port
// allow-list. Every one of these is a security boundary, so the test spells the
// list out instead of deriving it.
func TestOwnerOnlyOperationsTable(t *testing.T) {
	want := map[string]bool{
		"transfer":        true,
		"modify_quota":    true,
		"backup_download": true,
		"backup_upload":   true,
		"ssh_key_access":  true,
		"rebuild":         true,
		"port_mapping":    true,
	}
	if len(ownerOnlyOperations) != len(want) {
		t.Fatalf("ownerOnlyOperations has %d entries, want %d", len(ownerOnlyOperations), len(want))
	}
	for op, expected := range want {
		if ownerOnlyOperations[HostOwnerOperation(op)] != expected {
			t.Errorf("ownerOnlyOperations[%q] != %v", op, expected)
		}
	}

	// Host-owner operations must NOT appear in the deny-list.
	for _, op := range []HostOwnerOperation{
		HostOpStart, HostOpStop, HostOpRestart, HostOpForceStop,
		HostOpViewStatus, HostOpViewLogs,
		HostOpSnapshotCreate, HostOpSnapshotRestore, HostOpSnapshotDelete, HostOpDelete,
	} {
		if ownerOnlyOperations[op] {
			t.Errorf("%q must be allowed for host owners", op)
		}
	}
}

// TestCheckOwnerOnlyPermissionAdminDenied pins AUTH004: admins may not act as
// the instance owner.
func TestCheckOwnerOnlyPermissionAdminDenied(t *testing.T) {
	// Call with an admin looking at someone else's instance. The DB is not
	// touched because the admin branch returns before the host lookup.
	result, err := CheckOwnerOnlyPermission(t.Context(), AuthUser{ID: 1, Role: "admin"}, InstanceRef{UserID: 2, HostID: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("admin must not be allowed to act as instance owner")
	}
	if result.PermissionType != PermissionAdmin {
		t.Fatalf("PermissionType = %q, want %q", result.PermissionType, PermissionAdmin)
	}
	if result.Reason != "This operation is only allowed for the instance owner, not admin" {
		t.Fatalf("Reason = %q", result.Reason)
	}
}

// TestCheckOwnerOnlyPermissionOwnerAllowed pins the fast path that must not hit
// the database at all.
func TestCheckOwnerOnlyPermissionOwnerAllowed(t *testing.T) {
	result, err := CheckOwnerOnlyPermission(t.Context(), AuthUser{ID: 7, Role: "user"}, InstanceRef{UserID: 7, HostID: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed || result.PermissionType != PermissionOwner {
		t.Fatalf("result = %#v, want allowed+owner", result)
	}
}

// TestCheckHostOwnerOperationPermissionOwnerFastPath pins that the owner check
// precedes the host lookup (so a DB outage cannot deny an owner their own box).
func TestCheckHostOwnerOperationPermissionOwnerFastPath(t *testing.T) {
	result, err := CheckHostOwnerOperationPermission(
		t.Context(), AuthUser{ID: 4, Role: "user"}, InstanceRef{UserID: 4, HostID: 8}, string(OwnerOpTransfer))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed || result.PermissionType != PermissionOwner {
		t.Fatalf("owner must be allowed even for transfer, got %#v", result)
	}
}

func TestIsAdminAndIsRegularUser(t *testing.T) {
	admin := AuthUser{Role: "admin"}
	user := AuthUser{Role: "user"}
	if !IsAdmin(admin) || IsAdmin(user) {
		t.Fatal("IsAdmin mismatch")
	}
	if !IsRegularUser(user) || IsRegularUser(admin) {
		t.Fatal("IsRegularUser mismatch")
	}
}
