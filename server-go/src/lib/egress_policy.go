// Port of server/src/lib/egress/policy.ts.
//
// Validation for the two JSON documents the egress subsystem stores: an EgressProfile
// (`egress` column) and a host firewall policy (`firewall` column). Both are
// attacker-reachable through the admin API and end up as firewall/ACL rules on a
// node, so every constraint here is a security control rather than a nicety:
//
//   - a proxy-mode policy must name at least one proxy node, otherwise instances
//     would be routed with no exit
//   - a probe URL must be HTTPS and credential-free, because it is fetched from the
//     node
//   - an ACL rule may not mix IPv4 and IPv6 subjects, and ICMP4/ICMP6 rules may not
//     carry the wrong family, because a mixed rule cannot be expressed in nftables
//     without silently matching the wrong traffic
//   - ports are only meaningful for TCP/UDP
//
// All errors are NetworkInputError (HTTP 400), which the routes translate verbatim.
package lib

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Egress modes and strategies mirror the TypeScript unions.
const (
	EgressModeOff    = "off"
	EgressModeDirect = "direct"
	EgressModeProxy  = "proxy"

	EgressStrategyBalance   = "balance"
	EgressStrategyFailover  = "failover"
	EgressStrategyLeastPing = "leastPing"
)

// Firewall actions mirror the union.
const (
	FirewallActionAllow  = "allow"
	FirewallActionDrop   = "drop"
	FirewallActionReject = "reject"
)

// egressMaxGroups mirrors MAX_EGRESS_GROUPS in package.ts.
const egressMaxGroups = 16

// NetworkInputError mirrors the TypeScript class: statusCode 400, message verbatim.
type NetworkInputError struct {
	Message string
}

// Error implements error.
func (e *NetworkInputError) Error() string { return e.Message }

// StatusCode mirrors the class field.
func (e *NetworkInputError) StatusCode() int { return 400 }

func networkError(message string) *NetworkInputError { return &NetworkInputError{Message: message} }

// EgressPolicy mirrors the EgressPolicy interface.
type EgressPolicy struct {
	Mode                 string   `json:"mode"`
	Strategy             string   `json:"strategy"`
	ProxyNodeIDs         []int32  `json:"proxyNodeIds"`
	InternalCIDRs        []string `json:"internalCidrs"`
	ProbeURL             string   `json:"probeUrl"`
	ProbeIntervalSeconds int      `json:"probeIntervalSeconds"`
}

// FirewallRule mirrors the FirewallRule interface.
type FirewallRule struct {
	Direction       string `json:"direction"`
	Action          string `json:"action"`
	Protocol        string `json:"protocol"`
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	SourcePort      string `json:"sourcePort"`
	DestinationPort string `json:"destinationPort"`
	Enabled         bool   `json:"enabled"`
	Description     string `json:"description"`
}

// FirewallPolicy mirrors the FirewallPolicy interface.
type FirewallPolicy struct {
	Enabled       bool           `json:"enabled"`
	IngressAction string         `json:"ingressAction"`
	EgressAction  string         `json:"egressAction"`
	Rules         []FirewallRule `json:"rules"`
}

// DefaultEgress mirrors defaultEgress.
func DefaultEgress() EgressPolicy {
	return EgressPolicy{
		Mode:                 EgressModeOff,
		Strategy:             EgressStrategyFailover,
		ProxyNodeIDs:         []int32{},
		InternalCIDRs:        []string{},
		ProbeURL:             "https://1.1.1.1/cdn-cgi/trace",
		ProbeIntervalSeconds: 30,
	}
}

// DefaultFirewall mirrors defaultFirewall.
func DefaultFirewall() FirewallPolicy {
	return FirewallPolicy{
		Enabled:       false,
		IngressAction: FirewallActionAllow,
		EgressAction:  FirewallActionAllow,
		Rules:         []FirewallRule{},
	}
}

// egressObject mirrors `object(value)`: a non-object (including an array) is
// rejected.
func egressObject(value any) (map[string]any, error) {
	if value == nil {
		return nil, networkError("Expected an object")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, networkError("Expected an object")
	}
	return object, nil
}

// egressCIDR mirrors `cidr(value)`: a bare IP or a CIDR with an in-range prefix,
// capped at 64 characters, returned trimmed.
func egressCIDR(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) > 64 {
		return "", networkError("Invalid IP/CIDR")
	}

	parts := strings.Split(strings.TrimSpace(text), "/")
	if len(parts) > 2 {
		return "", networkError("Invalid IP/CIDR")
	}

	family, valid := jsIsIP(parts[0])
	if !valid {
		return "", networkError("Invalid IP/CIDR")
	}
	if len(parts) == 2 {
		prefix := parts[1]
		if !egressDigitsPattern.MatchString(prefix) {
			return "", networkError("Invalid IP/CIDR")
		}
		number, err := strconv.Atoi(prefix)
		if err != nil {
			return "", networkError("Invalid IP/CIDR")
		}
		maxPrefix := 128
		if family == 4 {
			maxPrefix = 32
		}
		if number > maxPrefix {
			return "", networkError("Invalid IP/CIDR")
		}
	}
	return strings.TrimSpace(text), nil
}

var egressDigitsPattern = regexp.MustCompile(`^\d+$`)

// jsIsIP mirrors node's net.isIP: 4 for a dotted-quad, 6 for anything with a colon
// that parses as a bare IPv6 address, 0 otherwise.
func jsIsIP(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	if strings.Contains(value, ":") {
		if isIPv6Literal(value) {
			return 6, true
		}
		return 0, false
	}
	if isIPv4Literal(value) {
		return 4, true
	}
	return 0, false
}

// isIPv4Literal mirrors the dotted-quad test: four 1-3 digit groups, each ≤ 255.
func isIPv4Literal(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 3 || !egressDigitsPattern.MatchString(part) {
			return false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number > 255 {
			return false
		}
	}
	return true
}

// isIPv6Literal accepts the textual IPv6 forms net.isIP(6) accepts: hex groups with
// optional `::` compression and an optional dotted-quad tail.
func isIPv6Literal(value string) bool {
	if strings.Count(value, "::") > 1 {
		return false
	}
	hasCompression := strings.Contains(value, "::")

	head, tail, split := strings.Cut(value, "::")
	if !split {
		head = value
	}

	groups := make([]string, 0, 8)
	appendGroups := func(segment string) bool {
		if segment == "" {
			return true
		}
		for _, group := range strings.Split(segment, ":") {
			groups = append(groups, group)
		}
		return true
	}
	if !appendGroups(head) {
		return false
	}
	if split && !appendGroups(tail) {
		return false
	}

	// A trailing dotted-quad counts as two groups.
	if len(groups) > 0 && strings.Contains(groups[len(groups)-1], ".") {
		if !isIPv4Literal(groups[len(groups)-1]) {
			return false
		}
		groups = groups[:len(groups)-1]
		groups = append(groups, "0", "0")
	}

	for _, group := range groups {
		if len(group) == 0 || len(group) > 4 {
			return false
		}
		for _, char := range group {
			isHex := (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')
			if !isHex {
				return false
			}
		}
	}

	if hasCompression {
		return len(groups) < 8
	}
	return len(groups) == 8
}

// egressIDs mirrors `ids(value, max)`: a de-duplicated array of positive int32 ids.
func egressIDs(value any, max int) ([]int32, error) {
	array, ok := value.([]any)
	if !ok || len(array) > max {
		return nil, networkError("Invalid or duplicate node IDs")
	}

	seen := make(map[int32]bool, len(array))
	result := make([]int32, 0, len(array))
	for _, item := range array {
		number, ok := item.(float64)
		if !ok || number != float64(int64(number)) || number < 1 || number > 2147483647 {
			return nil, networkError("Invalid or duplicate node IDs")
		}
		id := int32(number)
		if seen[id] {
			return nil, networkError("Invalid or duplicate node IDs")
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}

// egressProbeURL mirrors `probeUrl(value)`: HTTPS, no credentials, ≤ 2048 chars,
// normalised through WHATWG URL (which the TS gets from `new URL(...).href`).
func egressProbeURL(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) > 2048 {
		return "", networkError("Invalid probe URL")
	}

	parsed, err := url.Parse(text)
	if err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Hostname() != "" {
		return parsed.String(), nil
	}
	return "", networkError("Probe URL must use HTTPS without credentials")
}

// ValidateEgress mirrors validateEgress.
func ValidateEgress(value any) (EgressPolicy, error) {
	object, err := egressObject(value)
	if err != nil {
		return EgressPolicy{}, err
	}

	mode, _ := object["mode"].(string)
	strategy, _ := object["strategy"].(string)
	modeValid := mode == EgressModeOff || mode == EgressModeDirect || mode == EgressModeProxy
	strategyValid := strategy == EgressStrategyBalance ||
		strategy == EgressStrategyFailover ||
		strategy == EgressStrategyLeastPing
	if !modeValid || !strategyValid {
		return EgressPolicy{}, networkError("Invalid egress mode or strategy")
	}

	proxyNodeIDs, err := egressIDs(object["proxyNodeIds"], egressMaxGroups)
	if err != nil {
		return EgressPolicy{}, err
	}
	if mode == EgressModeProxy && len(proxyNodeIDs) == 0 {
		return EgressPolicy{}, networkError("Select at least one proxy node")
	}

	rawCIDRs, ok := object["internalCidrs"].([]any)
	if !ok || len(rawCIDRs) > 64 {
		return EgressPolicy{}, networkError("Invalid internal networks")
	}
	internalCIDRs := make([]string, 0, len(rawCIDRs))
	for _, item := range rawCIDRs {
		cidr, err := egressCIDR(item)
		if err != nil {
			return EgressPolicy{}, err
		}
		internalCIDRs = append(internalCIDRs, cidr)
	}

	interval, ok := object["probeIntervalSeconds"].(float64)
	if !ok || interval != float64(int64(interval)) || interval < 10 || interval > 3600 {
		return EgressPolicy{}, networkError("Probe interval must be 10–3600 seconds")
	}

	probe, err := egressProbeURL(object["probeUrl"])
	if err != nil {
		return EgressPolicy{}, err
	}

	return EgressPolicy{
		Mode:                 mode,
		Strategy:             strategy,
		ProxyNodeIDs:         proxyNodeIDs,
		InternalCIDRs:        internalCIDRs,
		ProbeURL:             probe,
		ProbeIntervalSeconds: int(interval),
	}, nil
}

// egressACLSubjects mirrors `subjects(value)`: a comma-separated CIDR list capped at
// 32 entries, re-joined with single commas.
func egressACLSubjects(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) > 2048 {
		return "", networkError("Invalid ACL addresses")
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}

	parts := strings.Split(text, ",")
	if len(parts) > 32 {
		return "", networkError("Too many ACL addresses")
	}
	normalised := make([]string, 0, len(parts))
	for _, part := range parts {
		cidr, err := egressCIDR(strings.TrimSpace(part))
		if err != nil {
			return "", err
		}
		normalised = append(normalised, cidr)
	}
	return strings.Join(normalised, ","), nil
}

// egressACLPortsPattern mirrors /^\d{1,5}(-\d{1,5})?$/.
var egressACLPortsPattern = regexp.MustCompile(`^\d{1,5}(-\d{1,5})?$`)

// egressACLPorts mirrors `ports(value)`.
func egressACLPorts(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) > 256 {
		return "", networkError("Invalid ACL ports")
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}

	parts := strings.Split(text, ",")
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if !egressACLPortsPattern.MatchString(trimmed) {
			return "", networkError("Invalid ACL port range")
		}
		lowText, highText, hasRange := strings.Cut(trimmed, "-")
		if !hasRange {
			highText = lowText
		}
		low, errLow := strconv.Atoi(lowText)
		high, errHigh := strconv.Atoi(highText)
		if errLow != nil || errHigh != nil || low < 1 || high > 65535 || high < low {
			return "", networkError("Invalid ACL port range")
		}
	}

	trimmedParts := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmedParts = append(trimmedParts, strings.TrimSpace(part))
	}
	return strings.Join(trimmedParts, ","), nil
}

// ValidateFirewall mirrors validateFirewall.
func ValidateFirewall(value any) (FirewallPolicy, error) {
	object, err := egressObject(value)
	if err != nil {
		return FirewallPolicy{}, err
	}

	enabled, enabledOK := object["enabled"].(bool)
	ingress, _ := object["ingressAction"].(string)
	egress, _ := object["egressAction"].(string)
	rawRules, rulesOK := object["rules"].([]any)

	actionsValid := func(action string) bool {
		return action == FirewallActionAllow || action == FirewallActionDrop || action == FirewallActionReject
	}
	if !enabledOK || !actionsValid(ingress) || !actionsValid(egress) || !rulesOK || len(rawRules) > 200 {
		return FirewallPolicy{}, networkError("Invalid firewall policy")
	}

	rules := make([]FirewallRule, 0, len(rawRules))
	for _, item := range rawRules {
		ruleObject, err := egressObject(item)
		if err != nil {
			return FirewallPolicy{}, networkError("Invalid ACL rule")
		}

		direction, _ := ruleObject["direction"].(string)
		action, _ := ruleObject["action"].(string)
		protocol, _ := ruleObject["protocol"].(string)
		ruleEnabled, enabledOK := ruleObject["enabled"].(bool)
		description, descriptionOK := ruleObject["description"].(string)

		directionValid := direction == "ingress" || direction == "egress"
		protocolValid := protocol == "" || protocol == "tcp" || protocol == "udp" ||
			protocol == "icmp4" || protocol == "icmp6"
		if !directionValid || !actionsValid(action) || !protocolValid ||
			!enabledOK || !descriptionOK || len([]rune(description)) > 200 {
			return FirewallPolicy{}, networkError("Invalid ACL rule")
		}

		sourcePort, err := egressACLPorts(ruleObject["sourcePort"])
		if err != nil {
			return FirewallPolicy{}, err
		}
		destinationPort, err := egressACLPorts(ruleObject["destinationPort"])
		if err != nil {
			return FirewallPolicy{}, err
		}
		// Ports are meaningless for ICMP, and nftables would reject the rule.
		if (sourcePort != "" || destinationPort != "") && protocol != "tcp" && protocol != "udp" {
			return FirewallPolicy{}, networkError("Ports require TCP or UDP")
		}

		source, err := egressACLSubjects(ruleObject["source"])
		if err != nil {
			return FirewallPolicy{}, err
		}
		destination, err := egressACLSubjects(ruleObject["destination"])
		if err != nil {
			return FirewallPolicy{}, err
		}

		// One rule cannot mix address families, and ICMP4/ICMP6 must match.
		families := map[int]bool{}
		for _, subjects := range []string{source, destination} {
			if subjects == "" {
				continue
			}
			for _, entry := range strings.Split(subjects, ",") {
				ip := entry
				if slash := strings.IndexByte(ip, '/'); slash >= 0 {
					ip = ip[:slash]
				}
				if family, ok := jsIsIP(ip); ok {
					families[family] = true
				}
			}
		}
		if len(families) > 1 {
			return FirewallPolicy{}, networkError("Use a separate ACL rule for each address family")
		}
		if protocol == "icmp4" && families[6] {
			return FirewallPolicy{}, networkError("Use a separate ACL rule for each address family")
		}
		if protocol == "icmp6" && families[4] {
			return FirewallPolicy{}, networkError("Use a separate ACL rule for each address family")
		}

		rules = append(rules, FirewallRule{
			Direction:       direction,
			Action:          action,
			Protocol:        protocol,
			Source:          source,
			Destination:     destination,
			SourcePort:      sourcePort,
			DestinationPort: destinationPort,
			Enabled:         ruleEnabled,
			Description:     description,
		})
	}

	return FirewallPolicy{
		Enabled:       enabled,
		IngressAction: ingress,
		EgressAction:  egress,
		Rules:         rules,
	}, nil
}

// EgressPackageMode mirrors the PackageEgressMode union.
const (
	EgressPackageModeHost    = "host"
	EgressPackageModeDirect  = "direct"
	EgressPackageModeProfile = "profile"
)

// NormalizePackageEgress mirrors normalizePackageEgress: only the keys the caller
// actually sent are returned, so an update can be partial.
func NormalizePackageEgress(input map[string]any) (map[string]any, error) {
	result := make(map[string]any, 2)

	if raw, present := input["egressMode"]; present {
		mode := fmt.Sprint(raw)
		if mode != EgressPackageModeHost && mode != EgressPackageModeDirect && mode != EgressPackageModeProfile {
			return nil, networkError("Invalid package egress mode")
		}
		result["egressMode"] = mode
	}

	if raw, present := input["egressProfileId"]; present {
		if raw == nil {
			result["egressProfileId"] = nil
		} else {
			number, ok := raw.(float64)
			if !ok || number != float64(int64(number)) || number <= 0 || number > 2147483647 {
				return nil, networkError("Invalid egress profile")
			}
			result["egressProfileId"] = int32(number)
		}
	}

	return result, nil
}
