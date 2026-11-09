package lib

import "testing"

// TestCalculateIPv6MatchesNode pins calculateIPv6 against values produced by the real
// `ip-address` library. Every expectation in the table was printed by node, not
// derived — the compression rules are subtle enough that deriving them is how you get
// an address that differs by one hextet.
func TestCalculateIPv6MatchesNode(t *testing.T) {
	cases := []struct {
		subnet string
		offset int64
		want   string
	}{
		{"2a0a:51c1:9:13b:1::/80", 0, "2a0a:51c1:9:13b:1::2"},
		{"2a0a:51c1:9:13b:1::/80", 1, "2a0a:51c1:9:13b:1::3"},
		{"2a0a:51c1:9:13b:1::/80", 255, "2a0a:51c1:9:13b:1::101"},
		{"2a0a:51c1:9:13b:1::/80", 256, "2a0a:51c1:9:13b:1::102"},
		{"2a0a:51c1:9:13b:1::/80", 65535, "2a0a:51c1:9:13b:1:0:1:1"},
		{"2a0a:51c1:9:13b:1::/80", 4294967296, "2a0a:51c1:9:13b:1:1:0:2"},
		{"2a0a:51c1:9:13b::/64", 0, "2a0a:51c1:9:13b::2"},
		{"2a0a:51c1:9:13b::/64", 65535, "2a0a:51c1:9:13b::1:1"},
		{"2a0a:51c1:9:13b::/64", 4294967296, "2a0a:51c1:9:13b:0:1:0:2"},
		{"2001:db8::/32", 0, "2001:db8::2"},
		{"2001:db8::/32", 65535, "2001:db8::1:1"},
		{"2001:db8::/32", 4294967296, "2001:db8::1:0:2"},
		{"fd00::/8", 256, "fd00::102"},
	}

	for _, tc := range cases {
		got, err := CalculateIPv6(tc.subnet, tc.offset)
		if err != nil {
			t.Errorf("CalculateIPv6(%q, %d): %v", tc.subnet, tc.offset, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CalculateIPv6(%q, %d) = %q, want %q", tc.subnet, tc.offset, got, tc.want)
		}
	}

	if _, err := CalculateIPv6("not-an-ipv6", 0); err == nil {
		t.Fatal("an invalid subnet must be rejected")
	}
}

// TestGetIPv6OffsetMatchesNode pins the inverse operation with node-produced values.
func TestGetIPv6OffsetMatchesNode(t *testing.T) {
	cases := []struct {
		subnet  string
		address string
		want    int64
	}{
		{"2a0a:51c1:9:13b:1::/80", "2a0a:51c1:9:13b:1::2", 0},
		{"2a0a:51c1:9:13b:1::/80", "2a0a:51c1:9:13b:1::102", 256},
		{"2001:db8::/32", "2001:db8::1:0", 65534},
	}

	for _, tc := range cases {
		got, err := GetIPv6Offset(tc.subnet, tc.address)
		if err != nil {
			t.Errorf("GetIPv6Offset(%q, %q): %v", tc.subnet, tc.address, err)
			continue
		}
		if got != tc.want {
			t.Errorf("GetIPv6Offset(%q, %q) = %d, want %d", tc.subnet, tc.address, got, tc.want)
		}
	}

	if _, err := GetIPv6Offset("bogus", "::1"); err == nil {
		t.Fatal("an invalid subnet must be rejected")
	}
	if _, err := GetIPv6Offset("2001:db8::/64", "bogus"); err == nil {
		t.Fatal("an invalid address must be rejected")
	}
}

// TestCalculateIPv6SkipsReserved pins that offset 0 is NOT the subnet address: ::0 is
// the subnet-router anycast address and ::1 is the gateway.
func TestCalculateIPv6SkipsReserved(t *testing.T) {
	first, err := CalculateIPv6("2001:db8::/64", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != "2001:db8::2" {
		t.Fatalf("first assignable = %q, want 2001:db8::2", first)
	}
}

// TestIPv4Pool pins the pool bounds and the in-range check.
func TestIPv4Pool(t *testing.T) {
	pool := ListNatIpv4Pool()
	// 10.10.1.1 .. 10.10.3.254 inclusive.
	wantSize := (3-1)*256 + (254 - 1) + 1
	if len(pool) != wantSize {
		t.Fatalf("pool size = %d, want %d", len(pool), wantSize)
	}
	if pool[0] != "10.10.1.1" {
		t.Fatalf("first = %q", pool[0])
	}
	if pool[len(pool)-1] != "10.10.3.254" {
		t.Fatalf("last = %q", pool[len(pool)-1])
	}

	for _, inside := range []string{"10.10.1.1", "10.10.2.5", "10.10.3.254"} {
		if !IsIpv4InRange(inside) {
			t.Errorf("%q should be in range", inside)
		}
	}
	for _, outside := range []string{
		"10.10.0.1", // gateway, reserved
		"10.10.4.1", // past the pool
		"10.10.1.0", // network address of the first /24
		"192.168.1.1",
		"not-an-ip",
		"10.10.1.1/32",
	} {
		if IsIpv4InRange(outside) {
			t.Errorf("%q should be out of range", outside)
		}
	}
}

// TestGenerateRandomIPv4StaysInPool pins the allocator's bounds across many draws.
func TestGenerateRandomIPv4StaysInPool(t *testing.T) {
	original := IpAllocatorRandomUint64
	defer func() { IpAllocatorRandomUint64 = original }()

	// Pin both endpoints of the modulo range.
	IpAllocatorRandomUint64 = func() uint64 { return 0 }
	if got := GenerateRandomIPv4(); got != "10.10.1.1" {
		t.Fatalf("offset 0 = %q, want 10.10.1.1", got)
	}

	size := uint64(len(ListNatIpv4Pool()))
	IpAllocatorRandomUint64 = func() uint64 { return size - 1 }
	if got := GenerateRandomIPv4(); got != "10.10.3.254" {
		t.Fatalf("last offset = %q, want 10.10.3.254", got)
	}

	// A value past the size must wrap, never escape the pool.
	IpAllocatorRandomUint64 = func() uint64 { return size + 7 }
	if got := GenerateRandomIPv4(); !IsIpv4InRange(got) {
		t.Fatalf("wrapped value %q left the pool", got)
	}
}

// TestIsStrictValidIpv6 pins the trim + drop-suffix behaviour.
func TestIsStrictValidIpv6(t *testing.T) {
	valid := []string{"2001:db8::1", "  2001:db8::1  ", "2001:db8::1/64", "::1", "fd00::5"}
	for _, value := range valid {
		if !IsStrictValidIpv6(value) {
			t.Errorf("%q should be valid", value)
		}
	}

	// Counter-intuitive but measured: the TS does `address.trim().split('/')[0]`, so
	// ANY suffix is discarded before validation and "2001:db8::1/999" is valid. Only
	// the part before the slash is checked, which is why "bogus/64" fails while
	// "2001:db8::1/999" passes.
	if !IsStrictValidIpv6("2001:db8::1/999") {
		t.Error("a prefix suffix is discarded before validation, so this is valid")
	}
	if !IsStrictValidIpv6("::1/") {
		t.Error("an empty suffix is discarded, so this is valid")
	}
	if IsStrictValidIpv6("bogus/64") {
		t.Error("the part before the slash still has to be an IPv6 address")
	}

	invalid := []string{"", "bogus", "10.0.0.1", "2001:db8::g"}
	for _, value := range invalid {
		if IsStrictValidIpv6(value) {
			t.Errorf("%q should be invalid", value)
		}
	}
}

// TestIsValidIPv6SubnetValue pins the CIDR and bare-address acceptance.
func TestIsValidIPv6SubnetValue(t *testing.T) {
	valid := []string{"2a0a:51c1:9:13b:1::/80", "2a0a:51c1:9:13b::", "::1", "2001:db8::1/64"}
	for _, value := range valid {
		if !IsValidIPv6Subnet(value) {
			t.Errorf("%q should be a valid subnet", value)
		}
	}

	invalid := []string{"", "bogus", "2001:db8::/129", "10.0.0.0/8"}
	for _, value := range invalid {
		if IsValidIPv6Subnet(value) {
			t.Errorf("%q should be invalid", value)
		}
	}
}

// TestSubnetContainment pins the two containment helpers.
func TestSubnetContainment(t *testing.T) {
	if !IsIpv6InSubnet("2a0a:51c1:9:13b:1::5", "2a0a:51c1:9:13b:1::/80") {
		t.Fatal("an address inside the subnet must be reported as inside")
	}
	if IsIpv6InSubnet("2a0a:51c1:9:13b:2::5", "2a0a:51c1:9:13b:1::/80") {
		t.Fatal("an address outside the subnet must be reported as outside")
	}
	if IsIpv6InSubnet("bogus", "2a0a:51c1:9:13b:1::/80") {
		t.Fatal("a malformed address must be outside")
	}

	if !IsIpv6SubnetWithinSubnet("2a0a:51c1:9:13b:1::/80", "2a0a:51c1:9:13b::/64") {
		t.Fatal("a /80 inside a /64 must be contained")
	}
	if IsIpv6SubnetWithinSubnet("2a0a:51c1:9:13c:1::/80", "2a0a:51c1:9:13b::/64") {
		t.Fatal("a sibling block must not be contained")
	}
	if IsIpv6SubnetWithinSubnet("2a0a:51c1:9:13b::/64", "2a0a:51c1:9:13b:1::/80") {
		t.Fatal("a parent block must not be contained in its child")
	}
}

// TestGenerateRandomSubnetStaysInside pins the alignment and containment of generated
// blocks.
func TestGenerateRandomSubnetStaysInside(t *testing.T) {
	original := IpAllocatorRandomUint64
	defer func() { IpAllocatorRandomUint64 = original }()

	IpAllocatorRandomUint64 = func() uint64 { return 0 }
	first, err := GenerateRandomSubnet("2a0a:51c1:9:13b::/64", 80)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != "2a0a:51c1:9:13b::/80" {
		t.Fatalf("first block = %q", first)
	}
	if !IsIpv6SubnetWithinSubnet(first, "2a0a:51c1:9:13b::/64") {
		t.Fatal("the first block must be inside the host subnet")
	}

	IpAllocatorRandomUint64 = func() uint64 { return ^uint64(0) }
	last, err := GenerateRandomSubnet("2a0a:51c1:9:13b::/64", 80)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !IsIpv6SubnetWithinSubnet(last, "2a0a:51c1:9:13b::/64") {
		t.Fatalf("the last block %q must be inside the host subnet", last)
	}

	// A prefix not greater than the host prefix is an error.
	if _, err := GenerateRandomSubnet("2a0a:51c1:9:13b::/64", 64); err == nil {
		t.Fatal("prefix == host prefix must be rejected")
	}
	if _, err := GenerateRandomSubnet("2a0a:51c1:9:13b::/64", 32); err == nil {
		t.Fatal("prefix < host prefix must be rejected")
	}
}

// TestGenerateRandomIPv6SkipsReserved pins that generated addresses never land on
// ::0 or ::1 of the subnet.
func TestGenerateRandomIPv6SkipsReserved(t *testing.T) {
	original := IpAllocatorRandomUint64
	defer func() { IpAllocatorRandomUint64 = original }()

	IpAllocatorRandomUint64 = func() uint64 { return 0 }
	lowest, err := GenerateRandomIPv6("2001:db8::/64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lowest != "2001:db8::2" {
		t.Fatalf("lowest generated = %q, want 2001:db8::2", lowest)
	}

	IpAllocatorRandomUint64 = func() uint64 { return ^uint64(0) }
	highest, err := GenerateRandomIPv6("2001:db8::/64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !IsIpv6InSubnet(highest, "2001:db8::/64") {
		t.Fatalf("highest generated %q left the subnet", highest)
	}
}
