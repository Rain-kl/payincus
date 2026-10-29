package lib

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNormalizeIncusURL pins the bare-IPv6 repair, which is the only reason the
// helper exists.
func TestNormalizeIncusURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://10.0.0.1:8443", "https://10.0.0.1:8443"},
		{"https://[2a06:de04::1]:8443", "https://[2a06:de04::1]:8443"},
		{"https://2a06:de04:10:1250:::8443", "https://[2a06:de04:10:1250::]:8443"},
		// Verified against node: new URL() rejects the bare form and the repair
		// splits on the LAST colon, so the tail "1" is treated as a port.
		{"https://2a06:de04::1", "https://[2a06:de04:]:1"},
		{"not-a-url", "not-a-url"},
		{"https://host:notaport", "https://host:notaport"},
	}

	for _, tc := range cases {
		if got := NormalizeIncusURL(tc.input); got != tc.want {
			t.Errorf("NormalizeIncusURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestMapInstanceStatus pins the Incus → DB status projection, including the two
// surprising mappings (Frozen→stopped, Stopping→running).
func TestMapInstanceStatus(t *testing.T) {
	cases := map[string]string{
		"Running":  "running",
		"Stopped":  "stopped",
		"Frozen":   "stopped",
		"Error":    "error",
		"Starting": "creating",
		"Stopping": "running",
		"Bogus":    "unknown",
		"":         "unknown",
	}

	for input, want := range cases {
		if got := MapInstanceStatus(input); got != want {
			t.Errorf("MapInstanceStatus(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestRedactNodeTextRemovesSecretValues pins that a secret echoed back by the
// node never reaches a log or an error detail.
func TestRedactNodeTextRemovesSecretValues(t *testing.T) {
	body := map[string]any{
		"name":     "vm-1",
		"password": "hunter2",
		"config": map[string]any{
			"secret": "abc123",
		},
	}

	message := "failed vm-1: password=hunter2 and secret abc123"
	redacted := RedactNodeText(message, body)

	if strings.Contains(redacted, "hunter2") {
		t.Fatalf("password leaked: %q", redacted)
	}
	if strings.Contains(redacted, "abc123") {
		t.Fatalf("nested secret leaked: %q", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got %q", redacted)
	}
	if !strings.Contains(redacted, "vm-1") {
		t.Fatalf("non-secret value should survive, got %q", redacted)
	}
}

// TestRedactNodeTextAlsoStripsJSONQuotedForm pins the third candidate in the TS
// loop: `JSON.stringify(item).slice(1, -1)`.
func TestRedactNodeTextAlsoStripsJSONQuotedForm(t *testing.T) {
	body := map[string]any{"token": "a\"b"}
	redacted := RedactNodeText(`error with escaped a\"b inside`, body)
	if strings.Contains(redacted, `a\"b`) {
		t.Fatalf("escaped form leaked: %q", redacted)
	}
}

// TestNewIncusAPIErrorClampsStatus pins the [400,599] clamp, which keeps a
// malformed node response from looking like a 2xx.
func TestNewIncusAPIErrorClampsStatus(t *testing.T) {
	cases := map[int]int{200: 502, 0: 502, 399: 502, 400: 400, 502: 502, 599: 599, 600: 502}
	for input, want := range cases {
		if got := NewIncusAPIError("x", input, nil).StatusCode; got != want {
			t.Errorf("status %d clamped to %d, want %d", input, got, want)
		}
	}
}

// TestNewIncusAPIErrorTruncates pins the 4096-rune cap.
func TestNewIncusAPIErrorTruncates(t *testing.T) {
	message := strings.Repeat("x", 5000)
	err := NewIncusAPIError(message, 502, nil)
	if runes := []rune(err.Message); len(runes) != 4096 {
		t.Fatalf("message length = %d, want 4096", len(runes))
	}
}

// TestOperationErrorPreservesAbort pins that a cancellation stays a cancellation
// instead of being flattened into a 502.
func TestOperationErrorPreservesAbort(t *testing.T) {
	aborted := &NodeOperationAborted{Kind: "TimeoutError", Message: "took too long"}
	result := OperationError(aborted, nil)
	kind, ok := IsNodeOperationAborted(result)
	if !ok {
		t.Fatalf("expected a NodeOperationAborted, got %T", result)
	}
	if kind.Kind != "TimeoutError" || kind.Message != "took too long" {
		t.Fatalf("result = %#v", kind)
	}
}

// TestOperationErrorKeepsIncusStatus pins that an IncusApiError keeps its status
// when it is rewrapped.
func TestOperationErrorKeepsIncusStatus(t *testing.T) {
	original := NewIncusAPIError("node said no", 409, nil)
	result := OperationError(original, nil)
	var apiErr *IncusApiError
	if !errors.As(result, &apiErr) {
		t.Fatalf("expected *IncusApiError, got %T", result)
	}
	if apiErr.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", apiErr.StatusCode)
	}
}

// TestRequestRejectsWithoutConnect pins that an unconnected client fails loudly.
func TestRequestRejectsWithoutConnect(t *testing.T) {
	client := NewIncusClient(IncusClientOptions{URL: "https://127.0.0.1:8443"})
	if _, err := client.Request(t.Context(), http.MethodGet, "/1.0", nil, time.Second, nil); err == nil {
		t.Fatal("expected an error from an unconnected client")
	}
}

// TestConnectRequiresCertificatePair pins the explicit failure instead of a
// silent unauthenticated connection.
func TestConnectRequiresCertificatePair(t *testing.T) {
	client := NewIncusClient(IncusClientOptions{URL: "https://127.0.0.1:8443"})
	if _, err := client.Connect(t.Context()); err == nil {
		t.Fatal("expected an error when no certificate pair is configured")
	}
}

// TestConnectRejectsPinnedFingerprint pins that the unimplemented pinning path
// refuses to connect rather than downgrading to an unpinned connection.
func TestConnectRejectsPinnedFingerprint(t *testing.T) {
	certPath := "/tmp/does-not-exist.crt"
	keyPath := "/tmp/does-not-exist.key"
	client := NewIncusClient(IncusClientOptions{
		URL:               "https://127.0.0.1:8443",
		CertPath:          &certPath,
		KeyPath:           &keyPath,
		ServerFingerprint: strings.Repeat("a", 64),
	})
	_, err := client.Connect(t.Context())
	if err == nil {
		t.Fatal("expected pinning to be rejected")
	}
	if !strings.Contains(err.Error(), "尚未移植") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRequestParsesSyncResponse exercises the envelope handling against a local
// TLS server so the metadata/whole-envelope fallback is pinned.
func TestRequestParsesSyncResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"type":"sync","status":"Success","status_code":200,"metadata":{"name":"vm-1"}}`))
	}))
	defer server.Close()

	client := &IncusClient{
		baseURL: server.URL,
		http:    server.Client(),
	}

	raw, err := client.Request(t.Context(), http.MethodGet, "/1.0/instances/vm-1", nil, time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["name"] != "vm-1" {
		t.Fatalf("metadata not returned: %#v", decoded)
	}
}

// TestRequestSurfacesErrorEnvelope pins that an `type:error` envelope becomes an
// IncusApiError carrying the node's message.
func TestRequestSurfacesErrorEnvelope(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"type":"error","error":"Instance not found","error_code":404}`))
	}))
	defer server.Close()

	client := &IncusClient{baseURL: server.URL, http: server.Client()}
	_, err := client.Request(t.Context(), http.MethodGet, "/1.0/instances/nope", nil, time.Second, nil)

	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *IncusApiError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != 404 || apiErr.Message != "Instance not found" {
		t.Fatalf("error = %#v", apiErr)
	}
}

// TestWaitForOperationSuccess pins the success path of the polling loop.
func TestWaitForOperationSuccess(t *testing.T) {
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if calls == 1 {
			_, _ = writer.Write([]byte(`{"type":"sync","status":"Running","status_code":200,"metadata":{"status":"Running","status_code":200}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"type":"sync","status":"Success","status_code":200,"metadata":{"status":"Success","status_code":200,"metadata":{"done":true}}}`))
	}))
	defer server.Close()

	client := &IncusClient{baseURL: server.URL, http: server.Client()}
	raw, err := client.WaitForOperation(t.Context(), "/1.0/operations/abc", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), `"done":true`) {
		t.Fatalf("metadata = %s", raw)
	}
}

// TestWaitForOperationCancelled pins the 409 mapping for Cancelled.
func TestWaitForOperationCancelled(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"type":"sync","metadata":{"status":"Cancelled","status_code":200}}`))
	}))
	defer server.Close()

	client := &IncusClient{baseURL: server.URL, http: server.Client()}
	_, err := client.WaitForOperation(t.Context(), "/1.0/operations/abc", 5*time.Second, nil)

	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *IncusApiError, got %T", err)
	}
	if apiErr.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", apiErr.StatusCode)
	}
}
