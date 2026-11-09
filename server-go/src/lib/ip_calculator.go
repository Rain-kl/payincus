// Port of server/src/lib/ip-calculator.ts.
//
// The IPv4 side is a fixed NAT pool (10.10.0.0/22 with 10.10.1.1–10.10.3.254
// assignable). The pool is small — 766 addresses — so the allocator does NOT do
// "random then dedupe": at a few hundred instances the birthday problem makes that
// retry loop fail outright. ListNatIpv4Pool exists so callers can pick from the
// free set instead.
//
// The IPv6 side is IPAM arithmetic: an address is `subnetStart + offset + 2`, where
// the +2 skips ::0 (Subnet Router Anycast) and ::1 (reserved for the gateway).
// Keeping that constant identical matters: a different skip would hand a tenant the
// subnet-router anycast address or collide with the gateway.
//
// Address STRING formatting matches the `ip-address` library's correctForm(), which
// is RFC 5952 (lowercase, leading zeros dropped, longest zero run compressed, and a
// single zero group NOT compressed). Go's netip.Addr.String() follows the same
// rules, verified against real ip-address output on cases that distinguish the two
// algorithms.
package lib

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"strings"
)

// NAT IPv4 pool bounds. Mirrors NAT_IPV4_POOL_START / NAT_IPV4_POOL_END.
const (
	NatIPv4PoolStart = "10.10.1.1"
	NatIPv4PoolEnd   = "10.10.3.254"
	// NatIPv4Subnet is the host-internal subnet every node uses.
	NatIPv4Subnet = "10.10.0.0/22"
	// NatIPv4Gateway is the gateway on that subnet.
	NatIPv4Gateway = "10.10.0.1"
)

// NatIPv4DNS mirrors IPV4_CONFIG.dns.
var NatIPv4DNS = []string{"8.8.8.8", "1.1.1.1"}

// ipv6ReservedOffset mirrors the `+ BigInt(2)` skip in calculateIPv6: ::0 is the
// subnet-router anycast address and ::1 is reserved for the gateway.
const ipv6ReservedOffset = 2

// ipamIpv4ToUint32 mirrors the TS ipv4ToInt (with unsigned semantics).
func ipamIpv4ToUint32(address string) (uint32, bool) {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() {
		return 0, false
	}
	bytes := parsed.As4()
	return binary.BigEndian.Uint32(bytes[:]), true
}

// ipamIpv4FromUint32 mirrors intToIpv4.
func ipamIpv4FromUint32(value uint32) string {
	var bytes [4]byte
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes).String()
}

// IpAllocatorRandomUint64 is injectable so tests can pin allocation outcomes.
// Production reads crypto/rand: Node's Math.random is not cryptographically secure
// but these values are not secrets, and crypto/rand avoids also matching Node's
// reproducibility problem.
var IpAllocatorRandomUint64 = func() uint64 {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 0
	}
	return binary.BigEndian.Uint64(buffer[:])
}

// GenerateRandomIPv4 mirrors generateRandomIPv4: uniform over the assignable pool.
func GenerateRandomIPv4() string {
	start, _ := ipamIpv4ToUint32(NatIPv4PoolStart)
	end, _ := ipamIpv4ToUint32(NatIPv4PoolEnd)
	size := uint64(end-start) + 1

	offset := IpAllocatorRandomUint64() % size
	return ipamIpv4FromUint32(start + uint32(offset))
}

// ListNatIpv4Pool mirrors listNatIpv4Pool: every assignable address, in order.
func ListNatIpv4Pool() []string {
	start, _ := ipamIpv4ToUint32(NatIPv4PoolStart)
	end, _ := ipamIpv4ToUint32(NatIPv4PoolEnd)

	pool := make([]string, 0, end-start+1)
	for value := start; value <= end; value++ {
		pool = append(pool, ipamIpv4FromUint32(value))
	}
	return pool
}

// IsIpv4InRange mirrors isIpv4InRange: four numeric octets inside the pool.
func IsIpv4InRange(address string) bool {
	value, ok := ipamIpv4ToUint32(address)
	if !ok {
		// Reject forms netip accepts but the TS would not, e.g. "10.10.1.1/32".
		if strings.ContainsRune(address, '/') {
			return false
		}
		return false
	}
	start, _ := ipamIpv4ToUint32(NatIPv4PoolStart)
	end, _ := ipamIpv4ToUint32(NatIPv4PoolEnd)
	return value >= start && value <= end
}

// parseIPv6Subnet mirrors `new Address6(value)` followed by startAddress().
//
// The library accepts both a bare address and a CIDR; a bare address behaves as a
// /128 whose start address is itself. It also accepts an ADDRESS WITH A PREFIX
// (e.g. "2001:db8::1/64"), whose start address is the network address — netip's
// ParsePrefix keeps the address as written, so the address is masked explicitly.
func parseIPv6Subnet(value string) (netip.Prefix, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return netip.Prefix{}, false
	}

	if strings.ContainsRune(trimmed, '/') {
		prefix, err := netip.ParsePrefix(trimmed)
		if err != nil {
			return netip.Prefix{}, false
		}
		// Mask so start == network address even for "2001:db8::1/64".
		return prefix.Masked(), true
	}

	address, err := netip.ParseAddr(trimmed)
	if err != nil || !address.Is6() || address.Is4In6() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(address, 128), true
}

// isValidIPv6SubnetValue mirrors Address6.isValid for the subnet inputs.
func isValidIPv6SubnetValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if strings.ContainsRune(trimmed, '/') {
		prefix, err := netip.ParsePrefix(trimmed)
		return err == nil && prefix.Addr().Is6() && !prefix.Addr().Is4In6() && prefix.Bits() >= 0 && prefix.Bits() <= 128
	}
	address, err := netip.ParseAddr(trimmed)
	return err == nil && address.Is6() && !address.Is4In6()
}

// IsValidIPv6Subnet mirrors isValidIPv6Subnet.
func IsValidIPv6Subnet(subnet string) bool {
	return isValidIPv6SubnetValue(subnet)
}

// ipv6ToBigInt converts an address to its 128-bit integer value.
func ipv6ToBigInt(address netip.Addr) *big.Int {
	bytes := address.As16()
	value := new(big.Int).SetBytes(bytes[:])
	return value
}

// ipv6FromBigInt mirrors Address6.fromBigInt(...).correctForm().
//
// An out-of-range value wraps modulo 2^128 rather than erroring, matching the
// library's behaviour when an offset pushes past the subnet end.
func ipv6FromBigInt(value *big.Int) string {
	modulus := new(big.Int).Lsh(big.NewInt(1), 128)
	reduced := new(big.Int).Mod(value, modulus)

	bytes := make([]byte, 16)
	reduced.FillBytes(bytes)

	var array [16]byte
	copy(array[:], bytes)
	return netip.AddrFrom16(array).String()
}

// ipv6SubnetBounds returns the first and last address of a prefix as big ints.
func ipv6SubnetBounds(prefix netip.Prefix) (start *big.Int, end *big.Int) {
	start = ipv6ToBigInt(prefix.Addr())
	size := new(big.Int).Lsh(big.NewInt(1), uint(128-prefix.Bits()))
	end = new(big.Int).Sub(new(big.Int).Add(start, size), big.NewInt(1))
	return start, end
}

// CalculateIPv6 mirrors calculateIPv6(subnetStr, offset): start + offset + 2.
func CalculateIPv6(subnet string, offset int64) (string, error) {
	prefix, ok := parseIPv6Subnet(subnet)
	if !ok {
		return "", fmt.Errorf("Invalid IPv6 subnet: %s", subnet)
	}

	start, _ := ipv6SubnetBounds(prefix)
	target := new(big.Int).Add(start, big.NewInt(offset))
	target.Add(target, big.NewInt(ipv6ReservedOffset))
	return ipv6FromBigInt(target), nil
}

// GetIPv6Offset mirrors getIPv6Offset(subnetStr, address): addr - start - 2.
//
// The result is an int64 because offsets are database ids; a negative or oversized
// difference is reported as an error rather than silently truncated.
func GetIPv6Offset(subnet, address string) (int64, error) {
	prefix, ok := parseIPv6Subnet(subnet)
	if !ok {
		return 0, errors.New("Invalid subnet or address")
	}

	parsed, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || !parsed.Is6() || parsed.Is4In6() {
		return 0, errors.New("Invalid subnet or address")
	}

	start, _ := ipv6SubnetBounds(prefix)
	difference := new(big.Int).Sub(ipv6ToBigInt(parsed), start)
	difference.Sub(difference, big.NewInt(ipv6ReservedOffset))

	if !difference.IsInt64() {
		return 0, errors.New("IPv6 offset out of range")
	}
	return difference.Int64(), nil
}

// GenerateRandomIPv6 mirrors generateRandomIPv6: uniform over [start+2, end].
func GenerateRandomIPv6(subnet string) (string, error) {
	prefix, ok := parseIPv6Subnet(subnet)
	if !ok {
		return "", fmt.Errorf("Invalid IPv6 subnet: %s", subnet)
	}

	start, end := ipv6SubnetBounds(prefix)
	minimum := new(big.Int).Add(start, big.NewInt(ipv6ReservedOffset))
	rangeSize := new(big.Int).Sub(end, minimum)
	rangeSize.Add(rangeSize, big.NewInt(1)) // inclusive

	offset := new(big.Int).SetUint64(IpAllocatorRandomUint64())
	offset.Mod(offset, rangeSize)

	return ipv6FromBigInt(new(big.Int).Add(minimum, offset)), nil
}

// GenerateRandomSubnet mirrors generateRandomSubnet(hostSubnet, prefix): pick one
// aligned /prefix block inside hostSubnet.
func GenerateRandomSubnet(hostSubnet string, prefix int) (string, error) {
	host, ok := parseIPv6Subnet(hostSubnet)
	if !ok {
		return "", fmt.Errorf("Invalid IPv6 subnet: %s", hostSubnet)
	}
	if prefix <= host.Bits() || prefix > 128 {
		return "", fmt.Errorf("Target prefix /%d must be greater than host prefix /%d", prefix, host.Bits())
	}

	subnetBits := uint(prefix - host.Bits())
	maxSubnets := new(big.Int).Lsh(big.NewInt(1), subnetBits)

	index := new(big.Int).SetUint64(IpAllocatorRandomUint64())
	index.Mod(index, maxSubnets)

	hostStart, _ := ipv6SubnetBounds(host)
	subnetSize := new(big.Int).Lsh(big.NewInt(1), uint(128-prefix))
	blockStart := new(big.Int).Add(hostStart, new(big.Int).Mul(index, subnetSize))

	return fmt.Sprintf("%s/%d", ipv6FromBigInt(blockStart), prefix), nil
}

// IsIpv6InSubnet mirrors isIpv6InSubnet: is the address inside the subnet range?
// Any parse failure is false, matching the TS try/catch.
func IsIpv6InSubnet(address, subnet string) bool {
	prefix, ok := parseIPv6Subnet(subnet)
	if !ok {
		return false
	}
	parsed, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || !parsed.Is6() || parsed.Is4In6() {
		return false
	}

	start, end := ipv6SubnetBounds(prefix)
	value := ipv6ToBigInt(parsed)
	return value.Cmp(start) >= 0 && value.Cmp(end) <= 0
}

// IsIpv6SubnetWithinSubnet mirrors isIpv6SubnetWithinSubnet: does the candidate
// block sit entirely inside the parent block?
func IsIpv6SubnetWithinSubnet(candidate, parent string) bool {
	candidatePrefix, ok := parseIPv6Subnet(candidate)
	if !ok {
		return false
	}
	parentPrefix, ok := parseIPv6Subnet(parent)
	if !ok {
		return false
	}

	candidateStart, candidateEnd := ipv6SubnetBounds(candidatePrefix)
	parentStart, parentEnd := ipv6SubnetBounds(parentPrefix)

	return candidateStart.Cmp(parentStart) >= 0 && candidateEnd.Cmp(parentEnd) <= 0
}

// IsStrictValidIpv6 mirrors isStrictValidIpv6: trims, drops any suffix after '/',
// then validates as IPv6.
func IsStrictValidIpv6(address string) bool {
	trimmed := strings.TrimSpace(address)
	if before, _, found := strings.Cut(trimmed, "/"); found {
		trimmed = before
	}

	parsed, err := netip.ParseAddr(trimmed)
	if err != nil || !parsed.Is6() || parsed.Is4In6() {
		return false
	}
	return true
}
