// Port of server/src/lib/host-resources.ts (shared IPv6 extraction helper).
package lib

import "strings"

// ExtractHostPublicIPv6FromResources extracts the first global non-link-local/ULA
// IPv6 address from a host agent's resources JSON object.
func ExtractHostPublicIPv6FromResources(resources any) *string {
	object, ok := resources.(map[string]any)
	if !ok || object == nil {
		return nil
	}
	network, ok := object["network"].(map[string]any)
	if !ok || network == nil {
		return nil
	}
	cards, ok := network["cards"].([]any)
	if !ok {
		return nil
	}
	check := func(addresses any) *string {
		list, ok := addresses.([]any)
		if !ok {
			return nil
		}
		for _, entry := range list {
			address, ok := entry.(map[string]any)
			if !ok || address == nil {
				continue
			}
			if address["family"] != "inet6" || address["scope"] != "global" {
				continue
			}
			ip, ok := address["address"].(string)
			if !ok || ip == "" {
				continue
			}
			if strings.HasPrefix(ip, "fe80:") || strings.HasPrefix(ip, "fd") || strings.HasPrefix(ip, "fc") {
				continue
			}
			return &ip
		}
		return nil
	}
	for _, cardValue := range cards {
		card, ok := cardValue.(map[string]any)
		if !ok || card == nil {
			continue
		}
		if ip := check(card["addresses"]); ip != nil {
			return ip
		}
		ports, ok := card["ports"].([]any)
		if !ok {
			continue
		}
		for _, portValue := range ports {
			port, ok := portValue.(map[string]any)
			if !ok || port == nil {
				continue
			}
			if ip := check(port["addresses"]); ip != nil {
				return ip
			}
		}
	}
	return nil
}
