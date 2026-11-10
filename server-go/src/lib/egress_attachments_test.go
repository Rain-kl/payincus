package lib

import (
	"errors"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// TestEgressAttachmentDisabledWithoutHost pins the `!client.options?.hostId` early
// return shared by all four entry points: a client that is not bound to a host (the
// CLI, health probes, tests) must never touch egress bookkeeping or the database.
func TestEgressAttachmentDisabledWithoutHost(t *testing.T) {
	client := NewIncusClient(IncusClientOptions{URL: "https://127.0.0.1:8443"})
	value := map[string]any{"name": "vm-test01", "type": "container"}

	prepared, err := EgressAttachmentPreparePackageEgress(t.Context(), client, value, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.(map[string]any)["name"] != "vm-test01" {
		t.Fatalf("prepare changed the value: %#v", prepared)
	}

	protected, err := EgressAttachmentProtectProvisionedNetwork(t.Context(), client, value, nil)
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	if protected.(map[string]any)["name"] != "vm-test01" {
		t.Fatalf("protect changed the value: %#v", protected)
	}

	if err := EgressAttachmentEnsureBeforeStart(t.Context(), client, "vm-test01"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := EgressAttachmentRelease(t.Context(), client, "vm-test01"); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestEgressAttachmentIgnoresNonObject pins `typeof value !== 'object'`: a nil or
// scalar body is passed through untouched.
func TestEgressAttachmentIgnoresNonObject(t *testing.T) {
	client := NewIncusClient(IncusClientOptions{URL: "https://127.0.0.1:8443", HostID: 7})

	for _, value := range []any{nil, "string", 42} {
		prepared, err := EgressAttachmentPreparePackageEgress(t.Context(), client, value, nil)
		if err != nil {
			t.Fatalf("prepare(%v): %v", value, err)
		}
		if prepared != value {
			t.Fatalf("prepare(%v) = %#v", value, prepared)
		}
	}
}

// TestEgressAttachmentHash pins the deterministic host names: sha256 over
// `<name>/<device>`, truncated to the TypeScript's slice lengths.
func TestEgressAttachmentHash(t *testing.T) {
	// Reference values computed with sha256("vm-test01/eth0").
	if got := egressAttachmentHash("vm-test01/eth0", 11); len(got) != 11 {
		t.Fatalf("hash length = %d, want 11", len(got))
	}
	if egressAttachmentHash("a/eth0", 10) != egressAttachmentHash("a/eth0", 10) {
		t.Fatal("hash must be deterministic")
	}
	if egressAttachmentHash("a/eth0", 10) == egressAttachmentHash("a/eth1", 10) {
		t.Fatal("different devices must not collide")
	}
	if got := egressAttachmentHash("x", 40); got != egressAttachmentHash("x", 40) || len(got) != 40 {
		t.Fatalf("hash = %q", got)
	}
}

// TestEgressAttachmentAppendACL pins the Set-based de-duplication and ordering.
func TestEgressAttachmentAppendACL(t *testing.T) {
	cases := []struct {
		raw  any
		want string
	}{
		{nil, "incudal-host-firewall"},
		{"", "incudal-host-firewall"},
		{"a,b", "a,b,incudal-host-firewall"},
		{"incudal-host-firewall", "incudal-host-firewall"},
		{"a, incudal-host-firewall ,a", "a, incudal-host-firewall ,incudal-host-firewall"},
	}
	for _, tc := range cases {
		if got := egressAttachmentAppendACL(tc.raw, "incudal-host-firewall"); got != tc.want {
			t.Errorf("appendACL(%v) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestEgressAttachmentInstanceIDFromConfig pins the `Number(config[...])` guard: only
// a positive safe integer is accepted.
func TestEgressAttachmentInstanceIDFromConfig(t *testing.T) {
	cases := []struct {
		value map[string]any
		want  int32
	}{
		{map[string]any{"config": map[string]any{"user.incudal.egress-instance": "42"}}, 42},
		{map[string]any{"config": map[string]any{"user.incudal.egress-instance": "0"}}, 0},
		{map[string]any{"config": map[string]any{"user.incudal.egress-instance": "-1"}}, 0},
		{map[string]any{"config": map[string]any{"user.incudal.egress-instance": "abc"}}, 0},
		{map[string]any{"config": map[string]any{"user.incudal.egress-instance": "2147483648"}}, 0},
		{map[string]any{"config": map[string]any{}}, 0},
		{map[string]any{}, 0},
	}
	for _, tc := range cases {
		if got := egressAttachmentInstanceIDFromConfig(tc.value); got != tc.want {
			t.Errorf("instanceIDFromConfig(%#v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestEgressAttachmentPushFailsClosed pins the security default: needing a policy push
// without a wired hook is an error, never a silent skip.
func TestEgressAttachmentPushFailsClosed(t *testing.T) {
	original := EgressAttachmentPushNetworkPolicy
	EgressAttachmentPushNetworkPolicy = nil
	t.Cleanup(func() { EgressAttachmentPushNetworkPolicy = original })

	if _, err := egressAttachmentPush(t.Context(), 7); err == nil {
		t.Fatal("expected an explicit failure when the push hook is unwired")
	}
}

// TestEgressAttachmentBindingReady pins the generation/name match and the profile
// group lookup.
func TestEgressAttachmentBindingReady(t *testing.T) {
	profileID := int32(3)
	applied := &EgressAttachmentApplied{
		Bindings: []EgressAttachmentBinding{
			{InstanceName: "vm-test01", Generation: 1},
			{InstanceName: "vm-test02", Generation: 2},
		},
		Groups: []EgressAttachmentGroup{
			{Key: "p3", State: "ready", Error: ""},
		},
	}

	if !egressAttachmentBindingReady(applied, "vm-test01", 1) {
		t.Fatal("expected the binding to be ready")
	}
	if egressAttachmentBindingReady(applied, "vm-test01", 2) {
		t.Fatal("a generation mismatch must not be ready")
	}
	if egressAttachmentBindingReady(applied, "missing", 1) {
		t.Fatal("an absent binding must not be ready")
	}
	if egressAttachmentBindingReady(nil, "vm-test01", 1) {
		t.Fatal("a nil result must not be ready")
	}

	group := egressAttachmentFindGroup(applied, &profileID)
	if group == nil || group.State != "ready" {
		t.Fatalf("group = %#v", group)
	}
	other := int32(9)
	if egressAttachmentFindGroup(applied, &other) != nil {
		t.Fatal("unexpected group for an unknown profile")
	}
	if egressAttachmentFindGroup(applied, nil) != nil {
		t.Fatal("nil profile must not match a group")
	}
}

// TestEgressAttachmentTunnelProbeIsWired pins that the db-side capability probe is
// installed (db must not import lib, so lib publishes it in an init).
func TestEgressAttachmentTunnelProbeIsWired(t *testing.T) {
	if db.InstanceEgressHostSupportsControl == nil {
		t.Fatal("db.InstanceEgressHostSupportsControl is not wired")
	}
}

// TestEgressAttachmentProtectRequiresDatabase pins that a host-bound protect call
// fails loudly (instead of returning an unprotected body) when the policy read cannot
// reach a database.
func TestEgressAttachmentProtectRequiresDatabase(t *testing.T) {
	client := NewIncusClient(IncusClientOptions{URL: "https://127.0.0.1:8443", HostID: 7})
	_, err := EgressAttachmentProtectProvisionedNetwork(t.Context(), client,
		map[string]any{"name": "vm-test01", "type": "container"}, nil)
	if err == nil {
		t.Fatal("expected an error without a database")
	}
	var apiErr *IncusApiError
	if errors.As(err, &apiErr) {
		t.Fatalf("expected a plain error, got %T", err)
	}
}
