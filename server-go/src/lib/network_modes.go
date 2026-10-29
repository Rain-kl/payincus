// Port of server/src/lib/network-modes.ts.
package lib

// NetworkMode enumerates the supported instance network modes.
type NetworkMode string

const (
	NetworkModeNat            NetworkMode = "nat"
	NetworkModeNatIpv6        NetworkMode = "nat_ipv6"
	NetworkModeNatIpv6Nat     NetworkMode = "nat_ipv6_nat"
	NetworkModeIpv6Only       NetworkMode = "ipv6_only"
	NetworkModeIpv6Nat        NetworkMode = "ipv6_nat"
	NetworkModePublicIpv4     NetworkMode = "public_ipv4"
	NetworkModePublicIpv4Ipv6 NetworkMode = "public_ipv4_ipv6"
)

// ActivePackageNetworkModes are the modes selectable for new packages.
var ActivePackageNetworkModes = []NetworkMode{
	NetworkModeNat,
	NetworkModeNatIpv6,
	NetworkModeIpv6Only,
	NetworkModePublicIpv4,
	NetworkModePublicIpv4Ipv6,
}

// LegacyIpv6NatNetworkModes are deprecated modes still present in stored data.
var LegacyIpv6NatNetworkModes = map[NetworkMode]bool{
	NetworkModeNatIpv6Nat: true,
	NetworkModeIpv6Nat:    true,
}

// NormalizeNetworkMode coerces any value to a known mode, defaulting to "nat".
func NormalizeNetworkMode(value string) NetworkMode {
	switch NetworkMode(value) {
	case NetworkModeNatIpv6,
		NetworkModeNatIpv6Nat,
		NetworkModeIpv6Only,
		NetworkModeIpv6Nat,
		NetworkModePublicIpv4,
		NetworkModePublicIpv4Ipv6:
		return NetworkMode(value)
	default:
		return NetworkModeNat
	}
}

// NetworkModeNeedsNatIpv4 reports whether the mode consumes a NAT IPv4 address.
func NetworkModeNeedsNatIpv4(mode string) bool {
	switch NormalizeNetworkMode(mode) {
	case NetworkModeNat, NetworkModeNatIpv6, NetworkModeNatIpv6Nat:
		return true
	default:
		return false
	}
}

// NetworkModeNeedsPublicIpv4 reports whether the mode consumes a public IPv4 address.
func NetworkModeNeedsPublicIpv4(mode string) bool {
	switch NormalizeNetworkMode(mode) {
	case NetworkModePublicIpv4, NetworkModePublicIpv4Ipv6:
		return true
	default:
		return false
	}
}

// NetworkModeNeedsRoutedIpv6 reports whether the mode consumes routed IPv6.
func NetworkModeNeedsRoutedIpv6(mode string) bool {
	switch NormalizeNetworkMode(mode) {
	case NetworkModeNatIpv6, NetworkModeIpv6Only, NetworkModePublicIpv4Ipv6:
		return true
	default:
		return false
	}
}

// NetworkModeAllowsPortMapping reports whether the mode supports port mapping.
func NetworkModeAllowsPortMapping(mode string) bool {
	return NetworkModeNeedsNatIpv4(mode)
}
