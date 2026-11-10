package lib

import (
	"net/http"
	"testing"
)

// TestIncusStorageListPoolsFollowsURLs pins that the URL list is resolved and that a
// single unreadable pool degrades to {name, driver: 'unknown'} instead of failing the
// whole listing (the TypeScript catch does the same).
func TestIncusStorageListPoolsFollowsURLs(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch request.Path {
		case "/1.0/storage-pools":
			return http.StatusOK, incusSyncEnvelope(`["/1.0/storage-pools/default","/1.0/storage-pools/broken"]`)
		case "/1.0/storage-pools/default":
			return http.StatusOK, incusSyncEnvelope(`{"name":"default","driver":"zfs","status":"Created","description":"main","config":{"size":"100GiB"}}`)
		case "/1.0/storage-pools/broken":
			return http.StatusInternalServerError, incusErrorEnvelope("pool unreadable", 500)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	pools, err := IncusStorageListPools(t.Context(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("pools = %#v", pools)
	}
	if pools[0].Name != "default" || pools[0].Driver != "zfs" || pools[0].Config["size"] != "100GiB" {
		t.Fatalf("default pool = %#v", pools[0])
	}
	if pools[1].Name != "broken" || pools[1].Driver != "unknown" || pools[1].Config == nil {
		t.Fatalf("fallback pool = %#v", pools[1])
	}
}

// TestIncusStorageListPoolsEmpty pins the short-circuit for an empty list.
func TestIncusStorageListPoolsEmpty(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Path == "/1.0/storage-pools" {
			return http.StatusOK, incusSyncEnvelope(`[]`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	pools, err := IncusStorageListPools(t.Context(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("pools = %#v", pools)
	}
	if node.Count() != 1 {
		t.Fatalf("expected only the list call, got %#v", node.Requests())
	}
}

// TestIncusStoragePoolResources pins GET /resources.
func TestIncusStoragePoolResources(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Path == "/1.0/storage-pools/default/resources" {
			return http.StatusOK, incusSyncEnvelope(`{"space":{"used":1073741824,"total":10737418240}}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	resources, err := IncusStorageGetPoolResources(t.Context(), client, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resources.Space == nil || resources.Space.Used != 1073741824 || resources.Space.Total != 10737418240 {
		t.Fatalf("resources = %#v", resources)
	}
}

// TestIncusStorageCreatePoolBody pins the create body, including the empty
// description default.
func TestIncusStorageCreatePoolBody(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodPost && request.Path == "/1.0/storage-pools" {
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	err := IncusStorageCreatePool(t.Context(), client, IncusStorageCreatePoolOptions{
		Name:   "data",
		Driver: "zfs",
		Config: map[string]string{"size": "50GiB"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requests := node.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	body := requests[0].Body
	if body["name"] != "data" || body["driver"] != "zfs" {
		t.Fatalf("body = %#v", body)
	}
	if body["description"] != "" {
		t.Fatalf("description = %#v, want the empty-string default", body["description"])
	}
	config := body["config"].(map[string]any)
	if config["size"] != "50GiB" {
		t.Fatalf("config = %#v", config)
	}
}

// TestIncusStorageUpdatePoolOmitsAbsentFields pins the PATCH contract: absent fields
// are not sent, an explicit empty config is.
func TestIncusStorageUpdatePoolOmitsAbsentFields(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodPatch && request.Path == "/1.0/storage-pools/data" {
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	if err := IncusStorageUpdatePool(t.Context(), client, "data", IncusStorageUpdatePoolOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := IncusStorageUpdatePool(t.Context(), client, "data", IncusStorageUpdatePoolOptions{
		Config:      map[string]string{},
		Description: incusStringPtr(""),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requests := node.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %#v", requests)
	}
	if len(requests[0].Body) != 0 {
		t.Fatalf("absent fields must not be sent: %#v", requests[0].Body)
	}
	if _, ok := requests[1].Body["config"]; !ok {
		t.Fatalf("explicit empty config must be sent: %#v", requests[1].Body)
	}
	if description, ok := requests[1].Body["description"]; !ok || description != "" {
		t.Fatalf("explicit empty description must be sent: %#v", requests[1].Body)
	}
}

// TestIncusStorageDeletePool pins DELETE.
func TestIncusStorageDeletePool(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodDelete && request.Path == "/1.0/storage-pools/data" {
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	if err := IncusStorageDeletePool(t.Context(), client, "data"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Count() != 1 {
		t.Fatalf("requests = %#v", node.Requests())
	}
}

// TestIncusStorageGetPoolError pins that a missing pool stays an error (callers must
// be able to distinguish it from an empty pool).
func TestIncusStorageGetPoolError(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		return http.StatusNotFound, incusErrorEnvelope("Storage pool not found", 404)
	})

	_, err := IncusStorageGetPool(t.Context(), client, "missing")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsIncusNotFound(err) {
		t.Fatalf("expected a 404, got %v", err)
	}
}
