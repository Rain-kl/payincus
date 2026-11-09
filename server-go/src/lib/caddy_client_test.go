package lib

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCaddy is a scripted stand-in for the Caddy Admin API, reached without a tunnel.
type fakeCaddy struct {
	mu       sync.Mutex
	requests []string
	handler  func(writer http.ResponseWriter, request *http.Request)
	server   *httptest.Server
}

func newFakeCaddy(t *testing.T, handler func(writer http.ResponseWriter, request *http.Request)) *fakeCaddy {
	t.Helper()
	fake := &fakeCaddy{handler: handler}
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fake.mu.Lock()
		fake.requests = append(fake.requests, request.Method+" "+request.URL.Path)
		fake.mu.Unlock()
		fake.handler(writer, request)
	}))
	t.Cleanup(fake.server.Close)

	// Route the tunnel seam straight at the fake server.
	originalOpen := CaddyTunnelOpenStream
	originalOnline := CaddyTunnelIsOnline
	CaddyTunnelOpenStream = func(_ int32, _ string, _ int) (net.Conn, error) {
		return net.Dial("tcp", strings.TrimPrefix(fake.server.URL, "http://"))
	}
	CaddyTunnelIsOnline = func(_ int32) bool { return true }
	t.Cleanup(func() {
		CaddyTunnelOpenStream = originalOpen
		CaddyTunnelIsOnline = originalOnline
	})
	return fake
}

func (f *fakeCaddy) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.requests))
	copy(out, f.requests)
	return out
}

// TestCaddyTunnelSeamDefaultsToExplicitFailure pins that without the WebSocket wave a
// Caddy call fails loudly instead of pretending the site was configured.
func TestCaddyTunnelSeamDefaultsToExplicitFailure(t *testing.T) {
	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	if _, err := client.GetConfig(t.Context()); err == nil {
		t.Fatal("a missing tunnel must surface as an error")
	}
	if client.TestConnection(t.Context()) {
		t.Fatal("TestConnection must be false without a tunnel")
	}
}

// TestCaddyRouteIDReplacesOnlyDots pins the id derivation, including wildcards.
func TestCaddyRouteIDReplacesOnlyDots(t *testing.T) {
	cases := map[string]string{
		"example.com":       "site-example-com",
		"a.b.c.example.com": "site-a-b-c-example-com",
		"*.example.com":     "site-*-example-com",
		"localhost":         "site-localhost",
	}
	for domain, want := range cases {
		if got := caddyRouteID(domain); got != want {
			t.Errorf("caddyRouteID(%q) = %q, want %q", domain, got, want)
		}
	}
}

// TestCaddyAddSiteAppendsWhenRoutesExist pins the happy path: a single POST appends.
func TestCaddyAddSiteAppendsWhenRoutesExist(t *testing.T) {
	fake := newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/config/apps/http/servers/sites/routes" {
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	})

	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	if err := client.AddSite(t.Context(), "example.com", "10.10.1.5", 8080, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	seen := fake.seen()
	if len(seen) != 1 || seen[0] != "POST /config/apps/http/servers/sites/routes" {
		t.Fatalf("requests = %v, want a single POST", seen)
	}
}

// TestCaddyAddSiteCreatesServerWhenAbsent pins the strict-create branch: no server,
// so the whole server object is PUT with the route inside.
func TestCaddyAddSiteCreatesServerWhenAbsent(t *testing.T) {
	var putBody map[string]any
	fake := newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/config/apps/http/servers/sites/routes":
			writer.WriteHeader(http.StatusNotFound)
		case request.Method == http.MethodGet && request.URL.Path == "/config/apps/http/servers/sites":
			writer.WriteHeader(http.StatusNotFound)
		case request.Method == http.MethodPut && request.URL.Path == "/config/apps/http/servers/sites":
			data := make([]byte, request.ContentLength)
			_, _ = request.Body.Read(data)
			_ = json.Unmarshal(data, &putBody)
			writer.WriteHeader(http.StatusOK)
		default:
			writer.WriteHeader(http.StatusInternalServerError)
		}
	})

	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	if err := client.AddSite(t.Context(), "example.com", "10.10.1.5", 8080, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	listen, _ := putBody["listen"].([]any)
	if len(listen) != 2 || listen[0] != ":80" || listen[1] != ":443" {
		t.Fatalf("listen = %#v", putBody["listen"])
	}

	routes, _ := putBody["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("routes = %#v", putBody["routes"])
	}
	route, _ := routes[0].(map[string]any)
	if route["@id"] != "site-example-com" {
		t.Fatalf("route id = %#v", route["@id"])
	}
	// HTTP-only site must carry protocol http so Caddy skips ACME.
	match, _ := route["match"].([]any)
	first, _ := match[0].(map[string]any)
	if first["protocol"] != "http" {
		t.Fatalf("protocol = %#v, want http", first["protocol"])
	}

	if len(fake.seen()) != 3 {
		t.Fatalf("requests = %v, want POST+GET+PUT", fake.seen())
	}
}

// TestCaddyAddSiteNeverClobbersOnServerError pins the security property: a 500 from
// Caddy must NOT be treated as "missing", because that would overwrite live config.
func TestCaddyAddSiteNeverClobbersOnServerError(t *testing.T) {
	fake := newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	err := client.AddSite(t.Context(), "example.com", "10.10.1.5", 8080, true)
	if err == nil {
		t.Fatal("a 500 must abort, not recover")
	}

	seen := fake.seen()
	if len(seen) != 1 {
		t.Fatalf("requests = %v, want only the first POST (no recovery writes)", seen)
	}
}

// TestCaddyDeleteSiteIgnoresNotFound pins the idempotent delete.
func TestCaddyDeleteSiteIgnoresNotFound(t *testing.T) {
	newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	})
	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	if err := client.DeleteSite(t.Context(), "example.com"); err != nil {
		t.Fatalf("a 404 delete must be ignored: %v", err)
	}
}

// TestCaddyDeleteSitePropagatesOtherErrors pins that a 500 is NOT ignored.
func TestCaddyDeleteSitePropagatesOtherErrors(t *testing.T) {
	newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})
	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	if err := client.DeleteSite(t.Context(), "example.com"); err == nil {
		t.Fatal("a 500 delete must propagate")
	}
}

// TestCaddyGetSitesCollectsHostsAcrossServers pins the read path.
func TestCaddyGetSitesCollectsHostsAcrossServers(t *testing.T) {
	newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"apps":{"http":{"servers":{
			"sites":{"routes":[{"match":[{"host":["a.example.com"]}]}]},
			"other":{"routes":[{"match":[{"host":["b.example.com","c.example.com"]}]}]}
		}}}}`))
	})

	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	domains := client.GetSites(t.Context())
	if len(domains) != 3 {
		t.Fatalf("domains = %v, want 3", domains)
	}
}

// TestCaddyGetSitesReturnsEmptyOnFailure pins the TS catch-all.
func TestCaddyGetSitesReturnsEmptyOnFailure(t *testing.T) {
	newFakeCaddy(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})
	client := NewCaddyClient(CaddyClientConfig{HostID: 1})
	if domains := client.GetSites(t.Context()); len(domains) != 0 {
		t.Fatalf("domains = %v, want empty", domains)
	}
}

// TestResolveCaddyAdminPort pins the 8444 retirement.
func TestResolveCaddyAdminPort(t *testing.T) {
	cases := map[int]int{0: 2019, -1: 2019, 8444: 2019, 2019: 2019, 9000: 9000}
	for input, want := range cases {
		if got := ResolveCaddyAdminPort(input); got != want {
			t.Errorf("ResolveCaddyAdminPort(%d) = %d, want %d", input, got, want)
		}
	}
}

// TestGetCaddyClientForHostRefusals pins the three explicit pre-flight failures.
func TestGetCaddyClientForHostRefusals(t *testing.T) {
	originalOnline := CaddyTunnelIsOnline
	defer func() { CaddyTunnelIsOnline = originalOnline }()
	CaddyTunnelIsOnline = func(_ int32) bool { return true }

	if _, err := GetCaddyClientForHost(CaddyHostRef{}, 0); err == nil {
		t.Fatal("an invalid host must be refused")
	}

	disabled := false
	if _, err := GetCaddyClientForHost(CaddyHostRef{ID: 1, CaddyEnabled: &disabled}, 0); err == nil {
		t.Fatal("Caddy disabled must be refused")
	}
	if _, err := GetCaddyClientForHost(CaddyHostRef{ID: 1, CaddyEnabled: &disabled}, 0); !strings.Contains(err.Error(), "安装 Caddy") {
		t.Fatalf("unexpected message: %v", err)
	}

	CaddyTunnelIsOnline = func(_ int32) bool { return false }
	if _, err := GetCaddyClientForHost(CaddyHostRef{ID: 1}, 0); err == nil {
		t.Fatal("an offline tunnel must be refused")
	}

	CaddyTunnelIsOnline = func(_ int32) bool { return true }
	port := 8444
	enabled := true
	client, err := GetCaddyClientForHost(CaddyHostRef{ID: 1, CaddyEnabled: &enabled, CaddyPort: &port}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.port != CaddyAdminPort {
		t.Fatalf("port = %d, want %d", client.port, CaddyAdminPort)
	}
}

// TestIsCaddyNotFound pins the error classification helper.
func TestIsCaddyNotFound(t *testing.T) {
	if !IsCaddyNotFound(&CaddyAPIError{StatusCode: 404}) {
		t.Fatal("404 must classify as not-found")
	}
	if IsCaddyNotFound(&CaddyAPIError{StatusCode: 500}) {
		t.Fatal("500 must not classify as not-found")
	}
	if IsCaddyNotFound(errors.New("network down")) {
		t.Fatal("a plain error must not classify as not-found")
	}
}
