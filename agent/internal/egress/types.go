// Package egress manages the host's optional transparent Xray gateway.
// The host forwards packets through Incus ACLs BEFORE the namespace intercepts them.
package egress

import "encoding/json"

type Policy struct {
	Mode                 string   `json:"mode"`
	Strategy             string   `json:"strategy"`
	ProxyNodeIDs         []int    `json:"proxyNodeIds"`
	InternalCIDRs        []string `json:"internalCidrs"`
	ProbeURL             string   `json:"probeUrl"`
	ProbeIntervalSeconds int      `json:"probeIntervalSeconds"`
}
type Rule struct {
	Direction       string `json:"direction"`
	Action          string `json:"action"`
	Protocol        string `json:"protocol"`
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	SourcePort      string `json:"sourcePort"`
	DestinationPort string `json:"destinationPort"`
	Description     string `json:"description"`
	Enabled         bool   `json:"enabled"`
}
type Firewall struct {
	Enabled       bool   `json:"enabled"`
	IngressAction string `json:"ingressAction"`
	EgressAction  string `json:"egressAction"`
	Rules         []Rule `json:"rules"`
}
type Node struct {
	ID       int             `json:"id"`
	Outbound json.RawMessage `json:"outbound"`
}
type ProbeJob struct {
	JobID    string `json:"jobId"`
	ProbeURL string `json:"probeUrl"`
	Nodes    []Node `json:"nodes"`
}
type Instruction struct {
	Revision int        `json:"revision"`
	Egress   Policy     `json:"egress"`
	Firewall Firewall   `json:"firewall"`
	Nodes    []Node     `json:"nodes"`
	Probes   []ProbeJob `json:"probes,omitempty"`
}
type Status struct {
	AppliedRevision int      `json:"appliedRevision"`
	State           string   `json:"state"`
	Error           string   `json:"error"`
	XrayAvailable   bool     `json:"xrayAvailable"`
	ActiveNodeID    int      `json:"activeNodeId,omitempty"`
	HealthyNodeIDs  []int    `json:"healthyNodeIds"`
	ManagedNetworks []string `json:"managedNetworks"`
	RoutedFallback  bool     `json:"routedFallback"`
}
type ProbeResult struct {
	NodeID    int    `json:"nodeId"`
	Success   bool   `json:"success"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
}
type JobResult struct {
	JobID string        `json:"jobId"`
	Items []ProbeResult `json:"items"`
}
