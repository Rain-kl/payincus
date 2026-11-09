package lib

import (
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// The expected YAML below is the exact template from
// server/src/lib/network-payload-builder.ts' generateCloudInitNetworkConfig,
// including the trailing newline and the hard-coded /22 + 10.10.0.1 values.
func TestNetworkPayloadGenerateCloudInitNetworkConfigEth0Only(t *testing.T) {
	want := `version: 2
ethernets:
  eth0:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
      - 10.10.1.5/22
    routes:
      - to: 0.0.0.0/0
        via: 10.10.0.1
    nameservers:
      addresses:
        - 10.10.0.1
`
	if got := networkPayloadGenerateCloudInitNetworkConfig("10.10.1.5", nil); got != want {
		t.Errorf("config =\n%q\nwant\n%q", got, want)
	}
	if got := networkPayloadGenerateCloudInitNetworkConfig("10.10.1.5", []string{}); got != want {
		t.Errorf("empty list config =\n%q\nwant\n%q", got, want)
	}
}

func TestNetworkPayloadGenerateCloudInitNetworkConfigWithIPv6(t *testing.T) {
	want := `version: 2
ethernets:
  eth0:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
      - 10.10.1.5/22
    routes:
      - to: 0.0.0.0/0
        via: 10.10.0.1
    nameservers:
      addresses:
        - 10.10.0.1
  eth1:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
      - 2001:db8::5/128
      - 2001:db8::6/64
    gateway6: fe80::1
    nameservers:
      addresses:
        - 10.10.0.1
`
	got := networkPayloadGenerateCloudInitNetworkConfig("10.10.1.5", []string{"2001:db8::5", " 2001:db8::6/64 "})
	if got != want {
		t.Errorf("config =\n%q\nwant\n%q", got, want)
	}
}

// A list made only of blank entries is non-empty, so the TypeScript still emits
// the eth1 block with an empty address list. Preserve that quirk.
func TestNetworkPayloadGenerateCloudInitNetworkConfigBlankIPv6(t *testing.T) {
	got := networkPayloadGenerateCloudInitNetworkConfig("10.10.1.5", []string{"   "})
	wantFragment := `  eth1:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:

    gateway6: fe80::1
`
	if !strings.Contains(got, wantFragment) {
		t.Errorf("config =\n%q\nwant fragment\n%q", got, wantFragment)
	}
	if strings.Contains(got, "      - \n") {
		t.Errorf("blank address must not produce an address line: %q", got)
	}
}

func TestNetworkPayloadGenerateVmNicMacs(t *testing.T) {
	// Values produced by the real implementation:
	//   createHash('sha256').update(`incudal-vm-nic:${seed}:${nic}`)
	// then [0x02, hash[0..4]] formatted as lowercase hex.
	cases := []struct {
		seed string
		eth0 string
		eth1 string
	}{
		{"test", "02:a6:2f:5e:86:bc", "02:58:36:3c:d6:8e"},
		{"vm-alpha", "02:0e:30:24:42:a4", "02:00:f6:0d:5d:41"},
		{"实例-1", "02:7d:f5:a3:ca:f2", "02:16:50:d6:d7:26"},
	}
	for _, tc := range cases {
		eth0, eth1 := networkPayloadGenerateVmNicMacs(tc.seed)
		if eth0 != tc.eth0 || eth1 != tc.eth1 {
			t.Errorf("%q: got (%s, %s), want (%s, %s)", tc.seed, eth0, eth1, tc.eth0, tc.eth1)
		}
	}

	// Deterministic and distinct per NIC.
	firstEth0, firstEth1 := networkPayloadGenerateVmNicMacs("stable")
	secondEth0, secondEth1 := networkPayloadGenerateVmNicMacs("stable")
	if firstEth0 != secondEth0 || firstEth1 != secondEth1 {
		t.Error("MAC generation must be deterministic")
	}
	if firstEth0 == firstEth1 {
		t.Error("eth0 and eth1 must differ")
	}
	if !strings.HasPrefix(firstEth0, "02:") {
		t.Errorf("MAC must start with the locally-administered 02 prefix: %s", firstEth0)
	}
}

func TestNetworkPayloadIsValidIpv6(t *testing.T) {
	valid := []string{"::1", "::", "2001:db8::1", "2001:db8::1/64", " 2001:db8::1 ", "fe80::1"}
	for _, address := range valid {
		if !NetworkPayloadIsValidIpv6(address) {
			t.Errorf("%q must be valid", address)
		}
	}
	invalid := []string{"", "10.0.0.1", "abc", "2001:db8", "1:2:3:4:5:6:7:8:9"}
	for _, address := range invalid {
		if NetworkPayloadIsValidIpv6(address) {
			t.Errorf("%q must be invalid", address)
		}
	}

	// The regex is deliberately loose; these are accepted by the TypeScript too
	// (verified against the live pattern).
	for _, address := range []string{"1:2:3", "2001:db8::1::2"} {
		if !NetworkPayloadIsValidIpv6(address) {
			t.Errorf("%q is accepted by the TypeScript regex and must be valid here", address)
		}
	}
}

func TestNetworkPayloadIsValidIpv6Subnet(t *testing.T) {
	valid := []string{
		"2001:db8::/112",
		"2001:db8::/120",
		"2001:db8::/124",
		"2001:db8::/48",
		"2001:db8::/128",
		// parseInt quirk: trailing garbage after the digits is ignored.
		"2001:db8::/112abc",
		"2001:db8::/ 120",
	}
	for _, cidr := range valid {
		if !NetworkPayloadIsValidIpv6Subnet(cidr) {
			t.Errorf("%q must be valid", cidr)
		}
	}

	invalid := []string{
		"2001:db8::",
		"2001:db8::/47",
		"2001:db8::/129",
		"2001:db8::/abc",
		"10.0.0.0/64",
		"2001:db8::/112/1",
		"/112",
		"",
	}
	for _, cidr := range invalid {
		if NetworkPayloadIsValidIpv6Subnet(cidr) {
			t.Errorf("%q must be invalid", cidr)
		}
	}
}

func TestNetworkPayloadParseIntDecimal(t *testing.T) {
	cases := []struct {
		value  string
		want   float64
		wantOK bool
	}{
		{"112", 112, true},
		{" 112", 112, true},
		{"\t112", 112, true},
		{"112abc", 112, true},
		{"+112", 112, true},
		{"-112", -112, true},
		{"0x70", 0, true}, // parseInt(...,10) stops at 'x'
		{"", 0, false},
		{"abc", 0, false},
		{"-", 0, false},
	}
	for _, tc := range cases {
		got, ok := networkPayloadParseIntDecimal(tc.value)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("parseInt(%q) = (%v, %v), want (%v, %v)", tc.value, got, ok, tc.want, tc.wantOK)
		}
	}
}

// lib's init() installs ListNatIpv4Pool into the db allocator, so the two must be
// the same set. This is the single-source-of-truth check for the pool that
// db/ip_addresses.go only keeps as a fallback.
func TestNetworkPayloadInstallsNatIpv4Pool(t *testing.T) {
	installed := db.IpAddressesNatIpv4PoolSource()
	canonical := ListNatIpv4Pool()
	if len(canonical) != 766 {
		t.Fatalf("canonical pool size = %d, want 766", len(canonical))
	}
	if len(installed) != len(canonical) {
		t.Fatalf("installed pool size = %d, want %d", len(installed), len(canonical))
	}
	for index := range canonical {
		if installed[index] != canonical[index] {
			t.Fatalf("pool[%d] = %q, want %q", index, installed[index], canonical[index])
		}
	}
}
