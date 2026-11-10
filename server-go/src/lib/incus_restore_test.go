// Unit tests for lib/incus_restore.go. The node is an in-process httptest server
// reached through an *IncusClient whose unexported http field points at it, so no
// real node and no real certificate exchange is needed for the request-shape
// assertions (the mTLS path itself is covered by TestIncusPool*).
package lib

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// incusRestoreTestRequest is one request the fake node received, including the
// headers and raw body the restore endpoint cares about.
type incusRestoreTestRequest struct {
	Method      string
	Path        string
	ContentType string
	NameHeader  string
	PoolHeader  string
	BodyText    string
}

// incusRestoreTestNode is a scripted Incus API for the restore path.
type incusRestoreTestNode struct {
	mu       sync.Mutex
	requests []incusRestoreTestRequest
	handler  func(request incusRestoreTestRequest) (int, string)
}

func (n *incusRestoreTestNode) Requests() []incusRestoreTestRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]incusRestoreTestRequest(nil), n.requests...)
}

func (n *incusRestoreTestNode) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requests)
}

// incusRestoreTestClient builds a configured-looking client (cert paths set) that
// talks to the supplied test server.
func incusRestoreTestClient(t *testing.T, handler func(request incusRestoreTestRequest) (int, string)) (*IncusClient, *incusRestoreTestNode) {
	t.Helper()
	node := &incusRestoreTestNode{handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		record := incusRestoreTestRequest{
			Method:      request.Method,
			Path:        request.URL.Path,
			ContentType: request.Header.Get("Content-Type"),
			NameHeader:  request.Header.Get("X-Incus-name"),
			PoolHeader:  request.Header.Get("X-Incus-pool"),
			BodyText:    string(raw),
		}
		if request.URL.RawQuery != "" {
			record.Path += "?" + request.URL.RawQuery
		}

		node.mu.Lock()
		node.requests = append(node.requests, record)
		node.mu.Unlock()

		status, body := node.handler(record)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	client := &IncusClient{baseURL: server.URL, http: server.Client(), connected: true}
	client.certPath = "/tmp/restore-test.crt"
	client.keyPath = "/tmp/restore-test.key"
	return client, node
}

// TestIncusRestoreFromStreamPostsOctetStreamAndWaits pins the exact request the
// backup restore makes: POST /1.0/instances with the raw stream body and the
// X-Incus-name / X-Incus-pool headers, followed by the /wait poll.
func TestIncusRestoreFromStreamPostsOctetStreamAndWaits(t *testing.T) {
	client, node := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			if request.ContentType != "application/octet-stream" {
				return http.StatusBadRequest, `{"type":"error","error":"bad content type"}`
			}
			if request.NameHeader != "restored" {
				return http.StatusBadRequest, `{"type":"error","error":"bad name header"}`
			}
			if request.BodyText != "backup-bytes" {
				return http.StatusBadRequest, `{"type":"error","error":"bad body"}`
			}
			return http.StatusOK, `{"type":"async","status":"Operation created","status_code":100,"operation":"/1.0/operations/op-1"}`
		case request.Method == http.MethodGet && strings.HasPrefix(request.Path, "/1.0/operations/op-1/wait"):
			return http.StatusOK, `{"type":"sync","status":"Success","status_code":200,"metadata":{"status":"Success","status_code":200}}`
		}
		return http.StatusNotFound, `{"type":"error","error":"unexpected"}`
	})

	err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client:     client,
		Backup:     strings.NewReader("backup-bytes"),
		TargetName: "restored",
		PoolName:   "pool-1",
	})
	if err != nil {
		t.Fatalf("RestoreFromStream: %v", err)
	}

	requests := node.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want POST + wait", requests)
	}
	if requests[0].Method != http.MethodPost || requests[0].Path != "/1.0/instances" {
		t.Fatalf("first request = %#v", requests[0])
	}
	if requests[0].PoolHeader != "pool-1" {
		t.Fatalf("X-Incus-pool = %q, want pool-1", requests[0].PoolHeader)
	}
	if !strings.HasPrefix(requests[1].Path, "/1.0/operations/op-1/wait?timeout=") {
		t.Fatalf("wait request = %#v", requests[1])
	}
}

// TestIncusRestoreFromStreamOmitPoolHeader pins that an absent pool name does not
// send an empty X-Incus-pool header.
func TestIncusRestoreFromStreamOmitPoolHeader(t *testing.T) {
	client, node := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		return http.StatusOK, `{"type":"sync","status":"Success","status_code":200}`
	})
	if err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: strings.NewReader("x"), TargetName: "restored",
	}); err != nil {
		t.Fatalf("RestoreFromStream: %v", err)
	}
	if header := node.Requests()[0].PoolHeader; header != "" {
		t.Fatalf("X-Incus-pool = %q, want absent", header)
	}
}

// TestIncusRestoreFromStreamRejectsErrorEnvelope pins `type:error` → the node's
// message (restore must never look successful).
func TestIncusRestoreFromStreamRejectsErrorEnvelope(t *testing.T) {
	client, _ := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		return http.StatusOK, `{"type":"error","error":"unsupported backup"}`
	})

	err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: strings.NewReader("x"), TargetName: "restored",
	})
	if err == nil || err.Error() != "unsupported backup" {
		t.Fatalf("err = %v, want unsupported backup", err)
	}
}

// TestIncusRestoreWaitsForFailureStatus pins the Failure/status_code mapping.
func TestIncusRestoreWaitsForFailureStatus(t *testing.T) {
	client, _ := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		if request.Method == http.MethodPost {
			return http.StatusOK, `{"type":"async","status":"Operation created","status_code":100,"operation":"/1.0/operations/op-2"}`
		}
		return http.StatusOK, `{"type":"sync","metadata":{"status":"Failure","status_code":500,"err":"disk full"}}`
	})

	err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: strings.NewReader("x"), TargetName: "restored",
	})
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v, want disk full", err)
	}
}

// TestIncusRestoreWaitsForStatusCodeOnly pins `status_code == 200` counting as
// success even when the status string is not "Success".
func TestIncusRestoreWaitsForStatusCodeOnly(t *testing.T) {
	client, _ := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		if request.Method == http.MethodPost {
			return http.StatusOK, `{"type":"async","status":"Operation created","status_code":100,"operation":"/1.0/operations/op-4"}`
		}
		return http.StatusOK, `{"type":"sync","metadata":{"status":"Whatever","status_code":200}}`
	})
	if err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: strings.NewReader("x"), TargetName: "restored",
	}); err != nil {
		t.Fatalf("status_code 200 must succeed: %v", err)
	}
}

// TestIncusRestoreTimesOut pins the 5-minute-budget message and that a Running
// operation keeps polling instead of returning early.
func TestIncusRestoreTimesOut(t *testing.T) {
	client, node := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		if request.Method == http.MethodPost {
			return http.StatusOK, `{"type":"async","status":"Operation created","status_code":100,"operation":"/1.0/operations/op-3"}`
		}
		// Hold each long-poll briefly so a 1.1s budget produces a handful of
		// polls instead of a busy spin.
		time.Sleep(100 * time.Millisecond)
		return http.StatusOK, `{"type":"sync","metadata":{"status":"Running","status_code":103}}`
	})

	start := time.Now()
	err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: strings.NewReader("x"), TargetName: "restored",
		Timeout: 1100 * time.Millisecond,
	})
	if err == nil || err.Error() != "恢复操作超时 (5 分钟)" {
		t.Fatalf("err = %v, want the timeout message", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v; the loop must not wait the full 5 minutes", elapsed)
	}
	if node.Count() < 2 {
		t.Fatalf("expected the POST plus at least one wait, got %#v", node.Requests())
	}
}

// TestIncusRestoreRequiresCertificatePair pins the pre-I/O guard.
func TestIncusRestoreRequiresCertificatePair(t *testing.T) {
	client, node := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		return http.StatusOK, `{"type":"sync"}`
	})
	client.certPath = ""
	err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: strings.NewReader("x"), TargetName: "restored",
	})
	if err == nil || err.Error() != "Certificate path not configured" {
		t.Fatalf("err = %v, want Certificate path not configured", err)
	}
	if node.Count() != 0 {
		t.Fatalf("no request may be made before the certificate check, got %#v", node.Requests())
	}
}

// TestIncusRestoreRenameAndExists pins renameInstance and instanceExists.
func TestIncusRestoreRenameAndExists(t *testing.T) {
	client, node := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances/old-name":
			return http.StatusOK, `{"type":"sync","status":"Success","status_code":200}`
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/old-name":
			return http.StatusOK, `{"type":"sync","metadata":{"name":"old-name"}}`
		}
		return http.StatusNotFound, `{"type":"error","error":"Instance not found","error_code":404}`
	})

	if err := RenameIncusInstance(t.Context(), client, "old-name", "new-name"); err != nil {
		t.Fatalf("RenameIncusInstance: %v", err)
	}
	if !InstanceExists(t.Context(), client, "old-name") {
		t.Fatal("InstanceExists must be true for a 200")
	}
	if InstanceExists(t.Context(), client, "missing") {
		t.Fatal("InstanceExists must be false for a 404")
	}

	requests := node.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests = %#v", requests)
	}
	if !strings.Contains(requests[0].BodyText, `"name":"new-name"`) {
		t.Fatalf("rename body = %q, want {\"name\":\"new-name\"}", requests[0].BodyText)
	}
}

// TestIncusRestoreGenerateTempInstanceName pins `{name}-restore-{epochMillis}`.
func TestIncusRestoreGenerateTempInstanceName(t *testing.T) {
	before := time.Now().UTC().UnixMilli()
	name := GenerateTempInstanceName("box")
	after := time.Now().UTC().UnixMilli()

	prefix := "box-restore-"
	if !strings.HasPrefix(name, prefix) {
		t.Fatalf("name = %q, want prefix %q", name, prefix)
	}
	millis, err := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
	if err != nil {
		t.Fatalf("name = %q, suffix is not epoch millis: %v", name, err)
	}
	if millis < before || millis > after {
		t.Fatalf("timestamp %d outside [%d,%d]", millis, before, after)
	}
}

// TestIncusRestoreRejectsUnparsableEnvelope pins the `无效的响应` branch.
func TestIncusRestoreRejectsUnparsableEnvelope(t *testing.T) {
	client, _ := incusRestoreTestClient(t, func(request incusRestoreTestRequest) (int, string) {
		return http.StatusOK, "not json at all"
	})

	err := RestoreFromStream(t.Context(), RestoreFromStreamParams{
		Client: client, Backup: io.NopCloser(strings.NewReader("x")), TargetName: "restored",
	})
	if err == nil || !strings.Contains(err.Error(), "无效的响应") {
		t.Fatalf("err = %v, want 无效的响应", err)
	}
}
