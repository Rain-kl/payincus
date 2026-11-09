package db

import (
	"errors"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIpAddressesFallbackNatPool pins the db-side copy of the NAT IPv4 pool
// against the bounds in lib/ip-calculator.ts (10.10.1.1–10.10.3.254, 766
// addresses). lib.ListNatIpv4Pool replaces this fallback at runtime through
// IpAddressesNatIpv4PoolSource; the copy exists only because db must not import
// lib.
func TestIpAddressesFallbackNatPool(t *testing.T) {
	addresses := ipAddressesFallbackNatPool()
	if len(addresses) != 766 {
		t.Fatalf("pool size = %d, want 766", len(addresses))
	}
	if addresses[0] != "10.10.1.1" {
		t.Errorf("first = %q, want 10.10.1.1", addresses[0])
	}
	if addresses[len(addresses)-1] != "10.10.3.254" {
		t.Errorf("last = %q, want 10.10.3.254", addresses[len(addresses)-1])
	}
	if addresses[255] != "10.10.2.0" {
		t.Errorf("addresses[255] = %q, want 10.10.2.0", addresses[255])
	}
	if addresses[256] != "10.10.2.1" {
		t.Errorf("addresses[256] = %q, want 10.10.2.1", addresses[256])
	}
	for _, address := range addresses {
		if _, ok := ipAddressesIPv4ToUint32(address); !ok {
			t.Fatalf("pool entry %q is not a plain IPv4 address", address)
		}
	}
}

// The db test binary does not link lib, so the registration hook has not run and
// the fallback must be what callers see.
func TestIpAddressesNatPoolSourceDefaultsToFallback(t *testing.T) {
	if got, want := len(IpAddressesNatIpv4PoolSource()), 766; got != want {
		t.Fatalf("pool source size = %d, want %d", got, want)
	}
}

func TestIpAddressesIsAlreadyExistsError(t *testing.T) {
	if !IpAddressesIsAlreadyExistsError(IpAddressesErrAlreadyExists) {
		t.Error("sentinel must match")
	}
	// The TypeScript compares error.message, so a same-message error matches too.
	if !IpAddressesIsAlreadyExistsError(errors.New("IP_ADDRESS_ALREADY_EXISTS")) {
		t.Error("same-message error must match")
	}
	if IpAddressesIsAlreadyExistsError(errors.New("IP_ADDRESS_NOT_FOUND")) {
		t.Error("unrelated error must not match")
	}
	if IpAddressesIsAlreadyExistsError(nil) {
		t.Error("nil must not match")
	}
}

func TestIpAddressesIsUniqueViolation(t *testing.T) {
	if !ipAddressesIsUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Error("23505 must be a unique violation")
	}
	if ipAddressesIsUniqueViolation(&pgconn.PgError{Code: "23503"}) {
		t.Error("23503 must not be a unique violation")
	}
	if ipAddressesIsUniqueViolation(errors.New("23505")) {
		t.Error("a plain error must not be a unique violation")
	}
}

func TestIpAddressesDevicePattern(t *testing.T) {
	cases := map[string]int{
		"eth0":  0,
		"eth1":  1,
		"eth12": 12,
	}
	for device, want := range cases {
		match := ipAddressesDevicePattern.FindStringSubmatch(device)
		if match == nil {
			t.Fatalf("%q did not match", device)
		}
		parsed, ok := new(big.Int).SetString(match[1], 10)
		if !ok || parsed.Int64() != int64(want) {
			t.Errorf("%q parsed to %v, want %d", device, match[1], want)
		}
	}
	for _, device := range []string{"eth", "ethX", "eth-1", "ETH0", "eth0x"} {
		if ipAddressesDevicePattern.MatchString(device) {
			t.Errorf("%q must not match", device)
		}
	}
}

func TestIpAddressesIPv4RoundTrip(t *testing.T) {
	for _, address := range []string{"10.10.1.1", "10.10.3.254", "0.0.0.0", "255.255.255.255"} {
		value, ok := ipAddressesIPv4ToUint32(address)
		if !ok {
			t.Fatalf("%q did not parse", address)
		}
		if got := ipAddressesIPv4FromUint32(value); got != address {
			t.Errorf("round trip %q -> %q", address, got)
		}
	}
	if _, ok := ipAddressesIPv4ToUint32("2001:db8::1"); ok {
		t.Error("IPv6 must not parse as IPv4")
	}
	if _, ok := ipAddressesIPv4ToUint32("10.10.1.1/32"); ok {
		t.Error("CIDR must not parse as a bare IPv4 address")
	}
}
