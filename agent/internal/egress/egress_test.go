package egress

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testInstruction() Instruction {
	return Instruction{Revision: 1, Egress: Policy{Mode: "proxy", Strategy: "balance", ProxyNodeIDs: []int{1, 10}, ProbeURL: "https://www.gstatic.com/generate_204", ProbeIntervalSeconds: 30}, Firewall: Firewall{Enabled: true, IngressAction: "allow", EgressAction: "allow", Rules: []Rule{{Direction: "egress", Action: "drop", Protocol: "tcp", DestinationPort: "25", Enabled: true}}}, Nodes: []Node{{ID: 1, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`)}, {ID: 10, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1081}]}}`)}}}
}
func TestXrayConfiguration(t *testing.T) {
	in := testInstruction()
	data, err := xrayConfig(in, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	_ = json.Unmarshal(data, &c)
	outbounds := c["outbounds"].([]any)
	if outbounds[0].(map[string]any)["protocol"] != "blackhole" {
		t.Fatal("default outbound must block")
	}
	b := c["routing"].(map[string]any)["balancers"].([]any)[0].(map[string]any)
	if b["fallbackTag"] != "blocked" || b["strategy"].(map[string]any)["type"] != "roundRobin" {
		t.Fatal("native healthy-node balancing and fail-closed fallback required")
	}
	if strings.HasPrefix(nodeTag(10), nodeTag(1)) {
		t.Fatal("Xray selector prefix collision")
	}
	if !Available() {
		t.Skip("source-only build; prepare pinned Xray to run core validation")
	}
	dir := t.TempDir()
	binary, err := extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"direct", "proxy"} {
		for _, strategy := range []string{"balance", "failover", "leastPing"} {
			in.Egress.Mode = mode
			in.Egress.Strategy = strategy
			body, e := xrayConfig(in, nil, true)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(dir, "config.json")
			_ = os.WriteFile(path, body, 0600)
			if output, e := exec.Command(binary, "run", "-test", "-config", path).CombinedOutput(); e != nil {
				t.Fatalf("core rejected %s/%s: %s", mode, strategy, output)
			}
		}
	}
}
func TestConnectionAndACLBoundaries(t *testing.T) {
	in := testInstruction()
	rules, err := hostRules(in, inventory{Bridges: []string{"incusbr0"}, Routed: []string{"veth123"}, Internal: []string{"10.10.0.0/16"}, RoutedSources: []string{"2001:db8:10::2/128"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"ct direction reply return", "ct status dnat return", "ct state != new return", "ct state { invalid, untracked } drop", "10.10.0.0/16", "0xff00ffff", "@ready drop", "oifname != \"igw-host\" drop", "priority -5", "priority 10", "iceg*", "tcp dport { 25 } drop"} {
		if !strings.Contains(rules, required) {
			t.Fatalf("missing protection %q", required)
		}
	}
	if strings.Index(rules, "ct direction reply return") > strings.Index(rules, "ct mark set") {
		t.Fatal("inbound reply bypass must precede interception")
	}
	if !strings.Contains(rules, "ip6 saddr { 2001:db8:10::2/128 } jump selected") || !strings.Contains(rules, "chain acl_ingress_forward") {
		t.Fatal("routed source protection and independent ingress ACL evaluation required")
	}
	if _, err = hostRules(in, inventory{Bridges: []string{"bad\"; flush ruleset"}}); err == nil {
		t.Fatal("unsafe interface accepted")
	}
	in.Egress.Mode = "off"
	rules, err = hostRules(in, inventory{Bridges: []string{"incusbr0"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rules, "chain classify") || !strings.Contains(rules, "chain acl_forward") {
		t.Fatal("ACL must remain when proxy is disabled")
	}
}

func TestWriteNftCheckFixtures(t *testing.T) {
	dir := os.Getenv("EGRESS_NFT_FIXTURES_DIR")
	if dir == "" {
		t.Skip("optional isolated nft parser verification")
	}
	for _, mode := range []string{"off", "direct", "proxy"} {
		in := testInstruction()
		in.Egress.Mode = mode
		rules, err := hostRules(in, inventory{Bridges: []string{"incusbr0"}, Routed: []string{"veth123"}, Internal: []string{"10.10.0.0/16", "fd00:10::/64"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, mode+".nft"), []byte(rules), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway.nft"), []byte(namespaceRules()), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestACLPreservesUnrelatedReferencesAndDefaults(t *testing.T) {
	if refs := aclReferences("other,"+aclName, true); refs != "other,"+aclName {
		t.Fatal(refs)
	}
	f := Firewall{Enabled: true, IngressAction: "allow", EgressAction: "drop"}
	patch := aclConfig(map[string]string{"security.acls": "other", "security.acls.default.ingress.action": "reject"}, f)
	if patch["user.incudal.acl.ingress"] != "reject" || patch["security.acls"] != "other,"+aclName {
		t.Fatal(patch)
	}
	previous := map[string]string{}
	for k, v := range patch {
		previous[k] = v
	}
	f.Enabled = false
	restored := aclConfig(previous, f)
	if restored["security.acls"] != "other" || restored["security.acls.default.ingress.action"] != "reject" || restored["security.acls.default.egress.action"] != "" {
		t.Fatal(restored)
	}
}

func TestFailoverPriorityAndRecovery(t *testing.T) {
	nodes := []Node{{ID: 1}, {ID: 2}, {ID: 3}}
	for _, tc := range []struct {
		label               string
		active              int
		successes, failures map[int]int
		want                int
	}{
		{"first healthy is primary", 0, map[int]int{1: 1, 2: 1}, nil, 1},
		{"unhealthy primary chooses backup", 0, map[int]int{2: 1, 3: 1}, nil, 2},
		{"one miss does not flap", 1, map[int]int{2: 2}, map[int]int{1: 1}, 1},
		{"three misses switch", 1, map[int]int{2: 2}, map[int]int{1: 3}, 2},
		{"recovered primary waits for stability", 2, map[int]int{1: 1, 2: 1}, nil, 2},
		{"stable primary takes precedence", 2, map[int]int{1: 2, 2: 2}, nil, 1},
		{"all failed blocks", 2, map[int]int{}, map[int]int{1: 3, 2: 3, 3: 3}, 0},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := chooseFailover(nodes, tc.active, tc.successes, tc.failures); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestOverlappingInternalNetworks(t *testing.T) {
	got := compactNetworks([]string{"10.10.1.0/24", "10.10.0.0/16", "10.10.0.1", "10.10.0.0/16"})
	if len(got) != 1 || got[0] != "10.10.0.0/16" {
		t.Fatal(got)
	}
}
func socksServer(t *testing.T, count *atomic.Int64) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, e := l.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				header := make([]byte, 2)
				if _, e := io.ReadFull(conn, header); e != nil {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, e := io.ReadFull(conn, methods); e != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				request := make([]byte, 4)
				if _, e := io.ReadFull(conn, request); e != nil {
					return
				}
				var host string
				switch request[3] {
				case 1:
					b := make([]byte, 4)
					_, _ = io.ReadFull(conn, b)
					host = net.IP(b).String()
				case 3:
					b := make([]byte, 1)
					_, _ = io.ReadFull(conn, b)
					v := make([]byte, int(b[0]))
					_, _ = io.ReadFull(conn, v)
					host = string(v)
				case 4:
					b := make([]byte, 16)
					_, _ = io.ReadFull(conn, b)
					host = net.IP(b).String()
				default:
					return
				}
				p := make([]byte, 2)
				if _, e := io.ReadFull(conn, p); e != nil {
					return
				}
				dest, e := net.DialTimeout("tcp", net.JoinHostPort(host, fmtPort(int(p[0])<<8|int(p[1]))), time.Second)
				if e != nil {
					_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer dest.Close()
				count.Add(1)
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go io.Copy(dest, conn)
				_, _ = io.Copy(conn, dest)
			}()
		}
	}()
	return l
}
func fmtPort(p int) string { return fmt.Sprintf("%d", p) }
func TestRealXrayHTTPSProbe(t *testing.T) {
	if !Available() {
		t.Skip("source-only build; embedded Xray absent")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	t.Setenv("SSL_CERT_FILE", certPath)
	var count atomic.Int64
	proxy := socksServer(t, &count)
	port := proxy.Addr().(*net.TCPAddr).Port
	outbound, _ := json.Marshal(map[string]any{"protocol": "socks", "settings": map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": port}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := executeJob(ctx, dir, ProbeJob{JobID: "integration", ProbeURL: server.URL, Nodes: []Node{{ID: 7, Outbound: outbound}}})
	if len(result.Items) != 1 || !result.Items[0].Success || count.Load() == 0 {
		t.Fatalf("HTTPS through real Xray and SOCKS outbound failed: %+v", result)
	}
	t.Run("native-balancer-override", func(t *testing.T) {
		freePort := func() int {
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			return l.Addr().(*net.TCPAddr).Port
		}
		entryPort, controlPort := freePort(), freePort()
		in := testInstruction()
		in.Nodes = []Node{{ID: 7, Outbound: outbound}}
		in.Egress.ProbeURL = server.URL
		in.Egress.ProbeIntervalSeconds = 3600
		body, e := xrayConfig(in, nil, true)
		if e != nil {
			t.Fatal(e)
		}
		var config map[string]any
		_ = json.Unmarshal(body, &config)
		// Replace only the privileged transparent listener with a local SOCKS
		// entry. Production routing, observatory and RoutingService stay intact.
		listeners := []any{}
		for _, raw := range config["inbounds"].([]any) {
			v := raw.(map[string]any)
			switch v["tag"] {
			case "transparent":
				listeners = append(listeners, map[string]any{"tag": "transparent", "listen": "127.0.0.1", "port": entryPort, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}})
			case "api-in":
				v["port"] = controlPort
				listeners = append(listeners, v)
			}
		}
		config["inbounds"] = listeners
		body, _ = json.Marshal(config)
		path := filepath.Join(dir, "native.json")
		if e = os.WriteFile(path, body, 0600); e != nil {
			t.Fatal(e)
		}
		binary, e := extractBinary(dir)
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.CommandContext(ctx, binary, "run", "-config", path)
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		for attempt := 0; attempt < 30; attempt++ {
			c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", controlPort), 100*time.Millisecond)
			if e == nil {
				c.Close()
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		override := func(tag string) {
			t.Helper()
			if out, e := exec.CommandContext(ctx, binary, "api", "bo", fmt.Sprintf("--server=127.0.0.1:%d", controlPort), "-b", "egress", tag).CombinedOutput(); e != nil {
				t.Fatalf("Xray RoutingService override failed: %s", out)
			}
		}
		override("blocked")
		if p := ProbeURL(ctx, entryPort, server.URL); p.Success {
			t.Fatal("blackhole override leaked to direct outbound")
		}
		override(nodeTag(7))
		if p := ProbeURL(ctx, entryPort, server.URL); !p.Success {
			t.Fatalf("native balancer failed to select the healthy proxy: %+v", p)
		}
	})
	proxy.Close()
	result = executeJob(ctx, dir, ProbeJob{JobID: "failure", ProbeURL: server.URL, Nodes: []Node{{ID: 7, Outbound: outbound}}})
	if result.Items[0].Success {
		t.Fatal("dead proxy incorrectly reported healthy")
	}
}
