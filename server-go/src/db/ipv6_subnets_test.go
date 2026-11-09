package db

import (
	"errors"
	"net/netip"
	"testing"
)

// Expected values were produced by the real `ip-address` library:
//
//	new Address6(cidr).startAddress().correctForm() + '/' + subnetMask
//
// so the canonicalisation below cannot drift from what Node stores.
func TestIpv6SubnetsNormalizeCidrRange(t *testing.T) {
	cases := []struct {
		input      string
		wantCIDR   string
		wantPrefix int
		wantStart  string
		wantEnd    string
	}{
		{
			input:      "2001:db8::1/64",
			wantCIDR:   "2001:db8::/64",
			wantPrefix: 64,
			wantStart:  "42540766411282592856903984951653826560",
			wantEnd:    "42540766411282592875350729025363378175",
		},
		{
			input:      "2001:DB8:0:0:0:0:0:0/64",
			wantCIDR:   "2001:db8::/64",
			wantPrefix: 64,
			wantStart:  "42540766411282592856903984951653826560",
			wantEnd:    "42540766411282592875350729025363378175",
		},
		{
			input:      "2001:db8::/064",
			wantCIDR:   "2001:db8::/64",
			wantPrefix: 64,
			wantStart:  "42540766411282592856903984951653826560",
			wantEnd:    "42540766411282592875350729025363378175",
		},
		{
			input:      "::/0",
			wantCIDR:   "::/0",
			wantPrefix: 0,
			wantStart:  "0",
			wantEnd:    "340282366920938463463374607431768211455",
		},
		{
			input:      "::ffff:1.2.3.4/128",
			wantCIDR:   "::ffff:102:304/128",
			wantPrefix: 128,
			wantStart:  "281470698652420",
			wantEnd:    "281470698652420",
		},
		{
			input:      "2a01:4f9:c012:e7:aaaa::/112",
			wantCIDR:   "2a01:4f9:c012:e7:aaaa::/112",
			wantPrefix: 112,
			wantStart:  "55832869036718768972037022889250127872",
			wantEnd:    "55832869036718768972037022889250193407",
		},
		{
			// The TypeScript trims the whole CIDR before splitting.
			input:      "  2001:db8::1/112  ",
			wantCIDR:   "2001:db8::/112",
			wantPrefix: 112,
			wantStart:  "42540766411282592856903984951653826560",
			wantEnd:    "42540766411282592856903984951653892095",
		},
		{
			input:      "2001:db8::1/65",
			wantCIDR:   "2001:db8::/65",
			wantPrefix: 65,
			wantStart:  "42540766411282592856903984951653826560",
			wantEnd:    "42540766411282592866127356988508602367",
		},
		{
			input:      "fe80::1/10",
			wantCIDR:   "fe80::/10",
			wantPrefix: 10,
			wantStart:  "338288524927261089654018896841347694592",
			wantEnd:    "338620831926207318622244848606417780735",
		},
		{
			input:      "2001:db8:1234:5678:9abc:def0:1234:5678/97",
			wantCIDR:   "2001:db8:1234:5678:9abc:def0::/97",
			wantPrefix: 97,
			wantStart:  "42540766416916595524585004440255201280",
			wantEnd:    "42540766416916595524585004442402684927",
		},
		{
			input:      "::/127",
			wantCIDR:   "::/127",
			wantPrefix: 127,
			wantStart:  "0",
			wantEnd:    "1",
		},
		{
			input:      "ff00::/8",
			wantCIDR:   "ff00::/8",
			wantPrefix: 8,
			wantStart:  "338953138925153547590470800371487866880",
			wantEnd:    "340282366920938463463374607431768211455",
		},
		{
			input:      "abcd:ef01::/48",
			wantCIDR:   "abcd:ef01::/48",
			wantPrefix: 48,
			wantStart:  "228367255710343689193895420042030350336",
			wantEnd:    "228367255710344898119715034671205056511",
		},
	}

	for _, tc := range cases {
		got, err := Ipv6SubnetsNormalizeCidrRange(tc.input)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.input, err)
			continue
		}
		if got.CIDR != tc.wantCIDR {
			t.Errorf("%q: cidr = %q, want %q", tc.input, got.CIDR, tc.wantCIDR)
		}
		if got.Prefix != tc.wantPrefix {
			t.Errorf("%q: prefix = %d, want %d", tc.input, got.Prefix, tc.wantPrefix)
		}
		if got.Start.String() != tc.wantStart {
			t.Errorf("%q: start = %s, want %s", tc.input, got.Start, tc.wantStart)
		}
		if got.End.String() != tc.wantEnd {
			t.Errorf("%q: end = %s, want %s", tc.input, got.End, tc.wantEnd)
		}
	}
}

func TestIpv6SubnetsNormalizeCidrRangeRejects(t *testing.T) {
	invalid := []string{
		"",
		"2001:db8::",        // no prefix
		"2001:db8::/129",    // out of range
		"2001:db8::/abc",    // not numeric
		"2001:db8::/112abc", // prefix regex requires digits only
		"2001:db8::/0/1",    // three parts
		"10.0.0.0/8",        // IPv4 is not an Address6
		"2001:db8::/112/",   // trailing slash
		"not-an-address/64", // garbage
		"2001:db8::/-1",     // negative prefix
		"2001:db8::/0000",   // four digits
		"2001:db8::/ 112",   // whitespace inside the prefix part
	}
	for _, input := range invalid {
		if _, err := Ipv6SubnetsNormalizeCidrRange(input); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestIpv6SubnetsRangesOverlap(t *testing.T) {
	mustRange := func(cidr string) Ipv6CidrRange {
		value, err := Ipv6SubnetsNormalizeCidrRange(cidr)
		if err != nil {
			t.Fatalf("%q: %v", cidr, err)
		}
		return value
	}

	base := mustRange("2001:db8::/112")
	if !Ipv6SubnetsRangesOverlap(base, mustRange("2001:db8::/112")) {
		t.Error("identical ranges must overlap")
	}
	if !Ipv6SubnetsRangesOverlap(base, mustRange("2001:db8::/120")) {
		t.Error("a nested range must overlap")
	}
	if !Ipv6SubnetsRangesOverlap(mustRange("2001:db8::/120"), base) {
		t.Error("overlap must be symmetric")
	}
	if Ipv6SubnetsRangesOverlap(base, mustRange("2001:db8:0:1::/112")) {
		t.Error("disjoint ranges must not overlap")
	}
	if !Ipv6SubnetsRangesOverlap(base, mustRange("2001:db8::/64")) {
		t.Error("a containing range must overlap its child")
	}
}

func TestIpv6SubnetsFormatCorrect(t *testing.T) {
	format := func(address string) string {
		parsed, err := netip.ParseAddr(address)
		if err != nil {
			t.Fatalf("%q: %v", address, err)
		}
		return ipv6SubnetsFormatCorrect(parsed.As16())
	}

	cases := map[string]string{
		"::":                   "::",
		"::1":                  "::1",
		"2001:db8::1":          "2001:db8::1",
		"1:2:3:4:5:6:7:0":      "1:2:3:4:5:6:7:0", // a single zero group is not compressed
		"1:0:0:2:0:0:3:4":      "1::2:0:0:3:4",    // first longest run wins
		"2001:0:0:1:0:0:0:1":   "2001:0:0:1::1",   // only the longest run is compressed
		"0:0:0:1:0:0:0:0":      "0:0:0:1::",       // tie → first run
		"2001:db8:0:0:1:0:0:1": "2001:db8::1:0:0:1",
		"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff": "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
	}
	for input, want := range cases {
		if got := format(input); got != want {
			t.Errorf("%q formatted to %q, want %q", input, got, want)
		}
	}
}

func TestIpv6SubnetsConstants(t *testing.T) {
	if Ipv6SubnetAllocationLockNamespace != 4202 {
		t.Errorf("namespace = %d, want 4202", Ipv6SubnetAllocationLockNamespace)
	}
	if Ipv6SubnetOverlapError != "IPV6_SUBNET_OVERLAP" {
		t.Errorf("overlap error = %q", Ipv6SubnetOverlapError)
	}
	want := []int{112, 120, 124}
	if len(Ipv6SubnetAllowedPrefixes) != len(want) {
		t.Fatalf("allowed prefixes = %v, want %v", Ipv6SubnetAllowedPrefixes, want)
	}
	for index := range want {
		if Ipv6SubnetAllowedPrefixes[index] != want[index] {
			t.Fatalf("allowed prefixes = %v, want %v", Ipv6SubnetAllowedPrefixes, want)
		}
	}
}

func TestIpv6SubnetsIsOverlapError(t *testing.T) {
	if !Ipv6SubnetsIsOverlapError(errors.New("IPV6_SUBNET_OVERLAP")) {
		t.Error("sentinel message must match")
	}
	if Ipv6SubnetsIsOverlapError(errors.New("IPV6_SUBNET_NOT_IN_HOST_SUBNET")) {
		t.Error("a different message must not match")
	}
	if Ipv6SubnetsIsOverlapError(nil) {
		t.Error("nil must not match")
	}
}
