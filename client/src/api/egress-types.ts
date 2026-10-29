export interface ProxyNode { id: number; name: string; protocol: string; server: string; port: number }
export interface EgressPolicy { mode: 'off' | 'direct' | 'proxy'; strategy: 'balance' | 'failover' | 'leastPing'; proxyNodeIds: number[]; internalCidrs: string[]; probeUrl: string; probeIntervalSeconds: number }
export interface FirewallRule { direction: 'ingress' | 'egress'; action: 'allow' | 'drop' | 'reject'; protocol: '' | 'tcp' | 'udp' | 'icmp4' | 'icmp6'; source: string; destination: string; sourcePort: string; destinationPort: string; description: string; enabled: boolean }
export interface FirewallPolicy { enabled: boolean; ingressAction: 'allow' | 'drop' | 'reject'; egressAction: 'allow' | 'drop' | 'reject'; rules: FirewallRule[] }
export interface EgressSelection { egressMode: 'off' | 'direct' | 'profile'; egressProfileId: number | null }
export interface EgressProfile { id: number; name: string; active: boolean; egress?: EgressPolicy; hostIds: number[] }
export interface EgressProfileInput { name: string; egress: EgressPolicy; hostIds: number[] }
export interface NetworkPolicy extends EgressSelection { hostId: number; revision: number; appliedRevision: number; firewall: FirewallPolicy; reportedAt: string | null; status: { state?: string; error?: string; errorStage?: string; errorDetail?: string; xrayAvailable?: boolean; activeNodeId?: number; healthyNodeIds?: number[]; managedNetworks?: string[]; routedFallback?: boolean } }
export interface ProxyImportResult { nodes?: Array<Omit<ProxyNode, 'id'>>; errors: Array<{ index: number; error: string }>; added?: number; duplicates?: number }
export interface ProxyProbeResult { nodeId: number; success: boolean; latencyMs: number | null; error: string; errorDetail?: string }
export interface ProxyProbeResponse { host: { id: number; name: string }; items: ProxyProbeResult[] }
