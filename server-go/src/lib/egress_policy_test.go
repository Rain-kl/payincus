package lib

import "testing"

// TestValidateEgressPinsModes pins the mode/strategy unions and the proxy-node rule.
func TestValidateEgressPinsModes(t *testing.T) {
	base := func(overrides map[string]any) map[string]any {
		value := map[string]any{
			"mode":                 "off",
			"strategy":             "failover",
			"proxyNodeIds":         []any{},
			"internalCidrs":        []any{},
			"probeUrl":             "https://1.1.1.1/cdn-cgi/trace",
			"probeIntervalSeconds": float64(30),
		}
		for key, item := range overrides {
			value[key] = item
		}
		return value
	}

	if _, err := ValidateEgress(base(nil)); err != nil {
		t.Fatalf("default policy rejected: %v", err)
	}

	// Proxy mode with no exit node would route nothing.
	if _, err := ValidateEgress(base(map[string]any{"mode": "proxy"})); err == nil {
		t.Fatal("proxy mode without nodes must be rejected")
	}
	if _, err := ValidateEgress(base(map[string]any{
		"mode": "proxy", "proxyNodeIds": []any{float64(3)},
	})); err != nil {
		t.Fatalf("proxy mode with a node rejected: %v", err)
	}

	for _, bad := range []map[string]any{
		{"mode": "bogus"},
		{"strategy": "bogus"},
		{"probeIntervalSeconds": float64(9)},
		{"probeIntervalSeconds": float64(3601)},
		{"probeIntervalSeconds": float64(10.5)},
		{"proxyNodeIds": []any{float64(1), float64(1)}}, // duplicate
		{"proxyNodeIds": []any{float64(0)}},
		{"probeUrl": "http://example.com/x"},
		{"probeUrl": "https://user:pass@example.com/x"},
		{"internalCidrs": []any{"not-an-ip"}},
		{"internalCidrs": []any{"10.0.0.0/33"}},
		{"internalCidrs": []any{"::1/129"}},
	} {
		if _, err := ValidateEgress(base(bad)); err == nil {
			t.Errorf("expected rejection for %#v", bad)
		}
	}
}

// TestValidateEgressCIDRFamilies pins the family-aware prefix limits.
func TestValidateEgressCIDRFamilies(t *testing.T) {
	makePolicy := func(cidrs ...string) map[string]any {
		values := make([]any, 0, len(cidrs))
		for _, cidr := range cidrs {
			values = append(values, cidr)
		}
		return map[string]any{
			"mode": "direct", "strategy": "balance", "proxyNodeIds": []any{},
			"internalCidrs": values, "probeUrl": "https://a.example/x",
			"probeIntervalSeconds": float64(30),
		}
	}

	valid := []string{"10.0.0.0/8", "192.168.1.1", "2001:db8::/32", "::1", "fd00::/8"}
	for _, cidr := range valid {
		if _, err := ValidateEgress(makePolicy(cidr)); err != nil {
			t.Errorf("%q rejected: %v", cidr, err)
		}
	}

	invalid := []string{"10.0.0.0/33", "192.168.1.256", "::1/129", "10.0.0.0/8/9", "abc", ""}
	for _, cidr := range invalid {
		if _, err := ValidateEgress(makePolicy(cidr)); err == nil {
			t.Errorf("%q must be rejected", cidr)
		}
	}
}

// TestValidateFirewallPinsRuleShape pins the ACL rule constraints: ports need
// TCP/UDP, and one rule cannot mix address families.
func TestValidateFirewallPinsRuleShape(t *testing.T) {
	rule := func(overrides map[string]any) map[string]any {
		value := map[string]any{
			"direction":       "ingress",
			"action":          "allow",
			"protocol":        "tcp",
			"source":          "10.0.0.0/8",
			"destination":     "",
			"sourcePort":      "",
			"destinationPort": "22",
			"enabled":         true,
			"description":     "ssh",
		}
		for key, item := range overrides {
			value[key] = item
		}
		return value
	}
	policy := func(rules ...any) map[string]any {
		return map[string]any{
			"enabled":       true,
			"ingressAction": "allow",
			"egressAction":  "drop",
			"rules":         rules,
		}
	}

	if _, err := ValidateFirewall(policy(rule(nil))); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}

	// Ports on ICMP must be refused.
	if _, err := ValidateFirewall(policy(rule(map[string]any{"protocol": "icmp4"}))); err == nil {
		t.Fatal("ports with icmp4 must be rejected")
	}

	// Mixed families in one rule must be refused.
	if _, err := ValidateFirewall(policy(rule(map[string]any{
		"source": "10.0.0.0/8,2001:db8::/32",
	}))); err == nil {
		t.Fatal("mixed address families must be rejected")
	}

	// icmp4 with an IPv6 subject must be refused.
	if _, err := ValidateFirewall(policy(rule(map[string]any{
		"protocol": "icmp4", "sourcePort": "", "destinationPort": "", "source": "2001:db8::1",
	}))); err == nil {
		t.Fatal("icmp4 with an IPv6 subject must be rejected")
	}

	// Bad action / direction / protocol.
	for _, bad := range []map[string]any{
		{"action": "deny"},
		{"direction": "sideways"},
		{"protocol": "sctp"},
		{"destinationPort": "0"},
		{"destinationPort": "70000"},
		{"destinationPort": "22-1"},
		{"destinationPort": "ssh"},
		{"enabled": "yes"},
	} {
		if _, err := ValidateFirewall(policy(rule(bad))); err == nil {
			t.Errorf("expected rejection for %#v", bad)
		}
	}

	// A port range is fine.
	if _, err := ValidateFirewall(policy(rule(map[string]any{"destinationPort": "8000-8100"}))); err != nil {
		t.Fatalf("port range rejected: %v", err)
	}

	// The policy envelope itself.
	if _, err := ValidateFirewall(map[string]any{
		"enabled": true, "ingressAction": "allow", "egressAction": "allow",
	}); err == nil {
		t.Fatal("missing rules array must be rejected")
	}
	if _, err := ValidateFirewall(nil); err == nil {
		t.Fatal("nil policy must be rejected")
	}
}

// TestValidateFirewallDefaults pins the default documents.
func TestValidateFirewallDefaults(t *testing.T) {
	firewall := DefaultFirewall()
	if firewall.Enabled || firewall.IngressAction != "allow" || firewall.EgressAction != "allow" {
		t.Fatalf("default firewall = %#v", firewall)
	}
	if len(firewall.Rules) != 0 {
		t.Fatalf("default rules = %#v", firewall.Rules)
	}

	egress := DefaultEgress()
	if egress.Mode != "off" || egress.Strategy != "failover" || egress.ProbeIntervalSeconds != 30 {
		t.Fatalf("default egress = %#v", egress)
	}
	if egress.ProbeURL != "https://1.1.1.1/cdn-cgi/trace" {
		t.Fatalf("default probe url = %q", egress.ProbeURL)
	}
}

// TestNormalizePackageEgress pins the partial-update contract: only provided keys
// appear in the result.
func TestNormalizePackageEgress(t *testing.T) {
	result, err := NormalizePackageEgress(map[string]any{})
	if err != nil || len(result) != 0 {
		t.Fatalf("empty input = %#v, err=%v", result, err)
	}

	result, err = NormalizePackageEgress(map[string]any{"egressMode": "profile"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["egressMode"] != "profile" {
		t.Fatalf("result = %#v", result)
	}
	if _, present := result["egressProfileId"]; present {
		t.Fatal("egressProfileId must be absent when not provided")
	}

	result, err = NormalizePackageEgress(map[string]any{"egressProfileId": nil})
	if err != nil {
		t.Fatalf("null profile must be accepted: %v", err)
	}
	if value, present := result["egressProfileId"]; !present || value != nil {
		t.Fatalf("explicit null = %#v (present=%v)", value, present)
	}

	for _, bad := range []map[string]any{
		{"egressMode": "bogus"},
		{"egressProfileId": float64(0)},
		{"egressProfileId": float64(-1)},
		{"egressProfileId": float64(2147483648)},
		{"egressProfileId": "3"},
	} {
		if _, err := NormalizePackageEgress(bad); err == nil {
			t.Errorf("expected rejection for %#v", bad)
		}
	}
}

// TestEgressErrorShape pins the 400 status the routes rely on.
func TestEgressErrorShape(t *testing.T) {
	_, err := ValidateEgress(nil)
	networkErr, ok := err.(*NetworkInputError)
	if !ok {
		t.Fatalf("expected *NetworkInputError, got %T", err)
	}
	if networkErr.StatusCode() != 400 || networkErr.Error() != "Expected an object" {
		t.Fatalf("error = %#v", networkErr)
	}
}

// TestJsIsIPFamilies pins the literal detection used by the family checks.
func TestJsIsIPFamilies(t *testing.T) {
	cases := map[string]int{
		"10.0.0.1":        4,
		"255.255.255.255": 4,
		"256.0.0.1":       0,
		"10.0.0":          0,
		"::1":             6,
		"2001:db8::1":     6,
		"fe80::1%eth0":    0,
		"gggg::1":         0,
		"":                0,
	}
	for input, want := range cases {
		family, ok := jsIsIP(input)
		if want == 0 {
			if ok {
				t.Errorf("jsIsIP(%q) = %d, want invalid", input, family)
			}
			continue
		}
		if !ok || family != want {
			t.Errorf("jsIsIP(%q) = %d/%v, want %d", input, family, ok, want)
		}
	}
}
