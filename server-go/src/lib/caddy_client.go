// Port of server/src/lib/caddy-client.ts.
//
// Security model, unchanged: the Caddy Admin API listens in PLAINTEXT on the node's
// loopback at 127.0.0.1:2019 and is never exposed publicly. The panel reaches it
// through the existing Agent reverse-WebSocket tunnel, so the trust chain is
// panel <-> Agent HMAC plus the tunnel itself. There is deliberately no Caddy
// credential, no TLS and no Basic Auth.
//
// The tunnel (hostTunnelManager) is a Phase 7 component, so it is a seam here:
// CaddyTunnelOpenStream below. A missing tunnel surfaces as an explicit error, never
// as a silent success — a site that was not actually configured must not look added.
//
// The interesting logic that IS ported is addSite's strict-create state machine. It
// exists because Caddy distinguishes "server missing" from "routes field missing"
// by returning 404 only for the former, and PUT on an object path is a strict create
// that must never clobber an existing server. Getting that wrong silently wipes a
// node's entire reverse-proxy configuration.
package lib

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// CaddyAdminPort is the Admin API port. New installs always use 2019; the old 8444
// value is only recognised for compatibility.
const CaddyAdminPort = 2019

// caddyRequestTimeout mirrors REQUEST_TIMEOUT (30s).
const caddyRequestTimeout = 30 * time.Second

// CaddyClientConfig mirrors CaddyClientConfig.
type CaddyClientConfig struct {
	// HostID selects the tunnel target.
	HostID int32
	// Port is the Admin port; 0 means CaddyAdminPort.
	Port int
	// TargetHost is the tunnel peer; empty means 127.0.0.1 (overridable for tests).
	TargetHost string
}

// CaddyRoute mirrors CaddyRoute. Fields the client writes are typed; the rest of the
// document stays raw so a captured config round-trips unchanged.
type CaddyRoute struct {
	ID       string            `json:"@id,omitempty"`
	Match    []CaddyRouteMatch `json:"match,omitempty"`
	Handle   []CaddyHandler    `json:"handle,omitempty"`
	Terminal bool              `json:"terminal,omitempty"`
}

// CaddyRouteMatch mirrors the match object.
type CaddyRouteMatch struct {
	Host     []string `json:"host,omitempty"`
	Protocol string   `json:"protocol,omitempty"`
}

// CaddyHandler mirrors a handle entry.
type CaddyHandler struct {
	Handler   string          `json:"handler"`
	Upstreams []CaddyUpstream `json:"upstreams,omitempty"`
	Routes    []CaddyRoute    `json:"routes,omitempty"`
}

// CaddyUpstream mirrors an upstreams entry.
type CaddyUpstream struct {
	Dial string `json:"dial"`
}

// CaddyAPIError mirrors the internal CaddyApiError: the status is kept so callers can
// distinguish a genuine 404 (missing object) from any other failure. Treating a 5xx or
// a network error as "missing" would make addSite overwrite live configuration.
type CaddyAPIError struct {
	StatusCode int
	Body       string
}

// Error implements error.
func (e *CaddyAPIError) Error() string {
	return fmt.Sprintf("Caddy API error: %d %s", e.StatusCode, e.Body)
}

// IsCaddyNotFound reports whether err is a Caddy 404.
func IsCaddyNotFound(err error) bool {
	var apiErr *CaddyAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}

// CaddyTunnelOpenStream is the seam for
// `hostTunnelManager.createDuplexStream(hostId, targetHost, port)`. The WebSocket
// wave assigns it. The default reports an explicit error: without a tunnel there is
// no way to reach the node's loopback, and pretending otherwise would report a
// configured site that does not exist.
var CaddyTunnelOpenStream = func(hostID int32, targetHost string, port int) (net.Conn, error) {
	return nil, errors.New("宿主机内网穿透通道未移植，无法访问 Caddy Admin API")
}

// CaddyTunnelIsOnline is the seam for `hostTunnelManager.isTunnelOnline(hostId)`.
var CaddyTunnelIsOnline = func(hostID int32) bool { return false }

// CaddyClient mirrors the TypeScript class.
type CaddyClient struct {
	hostID     int32
	port       int
	targetHost string
}

// NewCaddyClient mirrors `new CaddyClient(config)`.
func NewCaddyClient(config CaddyClientConfig) *CaddyClient {
	port := config.Port
	if port == 0 {
		port = CaddyAdminPort
	}
	targetHost := config.TargetHost
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	return &CaddyClient{hostID: config.HostID, port: port, targetHost: targetHost}
}

// Request mirrors the private request(): one virtual TCP stream per call over the
// tunnel, JSON in and JSON out.
func (c *CaddyClient) Request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	connection, err := CaddyTunnelOpenStream(c.hostID, c.targetHost, c.port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}

	// Dial straight through the tunnel stream: the request is addressed to
	// 127.0.0.1:port on the NODE, which is not reachable from here.
	transport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return connection, nil
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: transport, Timeout: caddyRequestTimeout}

	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", c.port, path), payload)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Caddy request timeout")
		}
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	text, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	if response.StatusCode >= 400 {
		return nil, &CaddyAPIError{StatusCode: response.StatusCode, Body: string(text)}
	}
	if len(text) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(text) {
		return nil, errors.New("Caddy API 返回了非 JSON 响应")
	}
	return json.RawMessage(text), nil
}

// GetConfig mirrors getConfig().
func (c *CaddyClient) GetConfig(ctx context.Context) (json.RawMessage, error) {
	return c.Request(ctx, http.MethodGet, "/config/", nil)
}

// TestConnection mirrors testConnection(): any failure is reported as false, not as
// an error.
func (c *CaddyClient) TestConnection(ctx context.Context) bool {
	_, err := c.GetConfig(ctx)
	return err == nil
}

// caddyRouteID mirrors `site-${domain.replace(/\./g, '-')}`. The replacement is
// global and only dots are touched, so a wildcard domain keeps its '*'.
func caddyRouteID(domain string) string {
	return "site-" + strings.ReplaceAll(domain, ".", "-")
}

// AddSite mirrors addSite(domain, targetIp, targetPort, httpsEnabled).
//
// The flow is a state machine because Caddy reports 404 for a missing object but also
// for a missing FIELD, and the two need different recovery:
//
//  1. POST the route onto .../servers/sites/routes  (append, never replace)
//  2. on 404, GET .../servers/sites to tell "no server" from "no routes field"
//  3. server exists + routes array  -> POST again
//     server exists + no routes      -> PUT [route] (strict create of the array)
//     no server                      -> PUT the whole server object
//  4. if PUT of the server 404s, the parent http app is missing: PUT it with the
//     server inside; but only after a GET confirms it is genuinely absent
//
// Every "catch" re-checks that the failure was a 404 before recovering. A network
// error or a 5xx is rethrown, because treating it as "missing" would overwrite live
// configuration.
func (c *CaddyClient) AddSite(ctx context.Context, domain, targetIP string, targetPort int, httpsEnabled bool) error {
	routeID := caddyRouteID(domain)

	route := CaddyRoute{
		ID: routeID,
		Match: []CaddyRouteMatch{
			{Host: []string{domain}},
		},
		Handle: []CaddyHandler{{
			Handler:   "reverse_proxy",
			Upstreams: []CaddyUpstream{{Dial: fmt.Sprintf("%s:%d", targetIP, targetPort)}},
		}},
		Terminal: true,
	}
	// An HTTP-only site matches protocol http, so Caddy does not try to obtain a
	// certificate for it.
	if !httpsEnabled {
		route.Match = []CaddyRouteMatch{{Host: []string{domain}, Protocol: "http"}}
	}

	const serverName = "sites"
	serverPath := "/config/apps/http/servers/" + serverName
	routesPath := serverPath + "/routes"

	// 1. Append.
	if _, err := c.Request(ctx, http.MethodPost, routesPath, route); err == nil {
		return nil
	} else if !IsCaddyNotFound(err) {
		return err
	}

	// 2. Distinguish "no server" from "no routes field".
	var existingServer struct {
		Routes []CaddyRoute `json:"routes"`
	}
	serverExists := false
	if raw, err := c.Request(ctx, http.MethodGet, serverPath, nil); err == nil {
		serverExists = true
		if err := json.Unmarshal(raw, &existingServer); err != nil {
			return err
		}
	} else if !IsCaddyNotFound(err) {
		return err
	}

	if serverExists {
		if existingServer.Routes != nil {
			// 3a. The routes array exists; append to it.
			_, err := c.Request(ctx, http.MethodPost, routesPath, route)
			return err
		}
		// 3b. The server exists without a routes field; create the array only.
		_, err := c.Request(ctx, http.MethodPut, routesPath, []CaddyRoute{route})
		return err
	}

	// 3c. The server is genuinely absent; strict-create it.
	serverConfig := map[string]any{
		"listen": []string{":80", ":443"},
		"routes": []CaddyRoute{route},
		"automatic_https": map[string]any{
			"disable_redirects": true,
		},
	}

	if _, err := c.Request(ctx, http.MethodPut, serverPath, serverConfig); err == nil {
		return nil
	} else if !IsCaddyNotFound(err) {
		return err
	}

	// 4. Only a 404 here can mean the parent http app is missing.
	const httpAppPath = "/config/apps/http"
	if _, err := c.Request(ctx, http.MethodGet, httpAppPath, nil); err != nil {
		if !IsCaddyNotFound(err) {
			return err
		}
		// The parent is confirmed absent: create it without touching anything else.
		_, putErr := c.Request(ctx, http.MethodPut, httpAppPath, map[string]any{
			"servers": map[string]any{serverName: serverConfig},
		})
		return putErr
	}

	// Someone else created http while we were checking; retry the strict create.
	_, err := c.Request(ctx, http.MethodPut, serverPath, serverConfig)
	return err
}

// DeleteSite mirrors deleteSite(domain): a 404 is ignored, everything else is not.
func (c *CaddyClient) DeleteSite(ctx context.Context, domain string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/id/"+caddyRouteID(domain), nil)
	if err == nil {
		return nil
	}
	if IsCaddyNotFound(err) {
		return nil
	}
	return err
}

// GetSites mirrors getSites(): every host across every server's routes. Any failure
// yields an empty list, matching the TS catch.
func (c *CaddyClient) GetSites(ctx context.Context) []string {
	raw, err := c.GetConfig(ctx)
	if err != nil {
		return []string{}
	}

	var config struct {
		Apps *struct {
			HTTP *struct {
				Servers map[string]struct {
					Routes []CaddyRoute `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return []string{}
	}

	domains := make([]string, 0, 8)
	if config.Apps == nil || config.Apps.HTTP == nil {
		return domains
	}
	for _, server := range config.Apps.HTTP.Servers {
		for _, route := range server.Routes {
			if len(route.Match) > 0 && len(route.Match[0].Host) > 0 {
				domains = append(domains, route.Match[0].Host...)
			}
		}
	}
	return domains
}

// Reload mirrors reload(): Caddy applies configuration automatically, so this is
// retained only for call-site compatibility.
func (c *CaddyClient) Reload(_ context.Context) {}

// ResolveCaddyAdminPort mirrors resolveCaddyAdminPort: 8444 is the retired public
// reverse-proxy port, and any unset/invalid value normalises to 2019.
func ResolveCaddyAdminPort(port int) int {
	if port == 8444 || port <= 0 {
		return CaddyAdminPort
	}
	return port
}

// CaddyHostRef is the host projection getCaddyClientForHost needs.
type CaddyHostRef struct {
	ID           int32
	CaddyEnabled *bool
	CaddyPort    *int
}

// GetCaddyClientForHost mirrors getCaddyClientForHost.
//
// It refuses before any I/O when the host is unusable, Caddy is not installed, or the
// Agent tunnel is offline — the same three explicit failures the TS raises.
func GetCaddyClientForHost(host CaddyHostRef, overridePort int) (*CaddyClient, error) {
	if host.ID == 0 {
		return nil, errors.New("宿主机无效，无法建立 Caddy 隧道连接")
	}
	if host.CaddyEnabled != nil && !*host.CaddyEnabled {
		return nil, errors.New("请先在宿主机安装 Caddy")
	}
	if !CaddyTunnelIsOnline(host.ID) {
		return nil, errors.New("宿主机 Agent 隧道未连接，无法管理 Caddy（请确认 Agent 在线）")
	}

	port := 0
	if host.CaddyPort != nil {
		port = *host.CaddyPort
	}
	selected := overridePort
	if selected == 0 {
		selected = ResolveCaddyAdminPort(port)
	}
	return NewCaddyClient(CaddyClientConfig{HostID: host.ID, Port: selected}), nil
}
