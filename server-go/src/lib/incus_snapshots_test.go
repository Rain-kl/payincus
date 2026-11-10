// Unit tests for lib/incus_snapshots.go (the rename helper; the other five live in
// lib/incus_instances.go and are asserted here through the same fake node so the
// whole TypeScript module has coverage).
package lib

import (
	"net/http"
	"strings"
	"testing"
)

// TestIncusSnapshotRenamePostsName pins renameSnapshot's request shape.
func TestIncusSnapshotRenamePostsName(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		return http.StatusOK, incusSyncEnvelope(`{}`)
	})

	raw, err := RenameSnapshot(t.Context(), client, "box", "snap-1", "snap-2")
	if err != nil {
		t.Fatalf("RenameSnapshot: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("expected metadata")
	}

	requests := node.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Method != http.MethodPost || requests[0].Path != "/1.0/instances/box/snapshots/snap-1" {
		t.Fatalf("request = %#v", requests[0])
	}
	if requests[0].Body["name"] != "snap-2" {
		t.Fatalf("body = %#v, want name=snap-2", requests[0].Body)
	}
}

// TestIncusSnapshotModuleRequestShapes pins the five helpers that already live in
// incus_instances.go: list/get/create/delete/restore hit the documented paths and
// methods. Restore is the surprising one (PUT `{restore: name}` to the instance).
func TestIncusSnapshotModuleRequestShapes(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch request.Method {
		case http.MethodGet:
			if strings.Contains(request.Path, "/snapshots/snap") {
				return http.StatusOK, incusSyncEnvelope(`{"name":"snap"}`)
			}
			return http.StatusOK, incusSyncEnvelope(`[]`)
		}
		return http.StatusOK, incusSyncEnvelope(`{}`)
	})

	if _, err := ListSnapshots(t.Context(), client, "box"); err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if _, err := GetSnapshot(t.Context(), client, "box", "snap"); err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if _, err := CreateSnapshot(t.Context(), client, "box", "snap"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, err := DeleteSnapshot(t.Context(), client, "box", "snap"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := RestoreSnapshot(t.Context(), client, "box", "snap"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	requests := node.Requests()
	wanted := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/1.0/instances/box/snapshots"},
		{http.MethodGet, "/1.0/instances/box/snapshots/snap"},
		{http.MethodPost, "/1.0/instances/box/snapshots"},
		{http.MethodDelete, "/1.0/instances/box/snapshots/snap"},
		{http.MethodPut, "/1.0/instances/box"},
	}
	if len(requests) != len(wanted) {
		t.Fatalf("requests = %#v", requests)
	}
	for index, want := range wanted {
		if requests[index].Method != want.method || requests[index].Path != want.path {
			t.Fatalf("request %d = %#v, want %s %s", index, requests[index], want.method, want.path)
		}
	}
	if requests[2].Body["name"] != "snap" {
		t.Fatalf("create body = %#v", requests[2].Body)
	}
	if requests[4].Body["restore"] != "snap" {
		t.Fatalf("restore body = %#v", requests[4].Body)
	}
}
