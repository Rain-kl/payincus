import { isIP } from 'node:net'

export interface EgressPolicy {
  mode: 'off' | 'direct' | 'proxy'
  strategy: 'balance' | 'failover' | 'leastPing'
  proxyNodeIds: number[]
  internalCidrs: string[]
  probeUrl: string
  probeIntervalSeconds: number
}
export interface FirewallRule {
  direction: 'ingress' | 'egress'
  action: 'allow' | 'drop' | 'reject'
  protocol: '' | 'tcp' | 'udp' | 'icmp4' | 'icmp6'
  source: string
  destination: string
  sourcePort: string
  destinationPort: string
  description: string
  enabled: boolean
}
export interface FirewallPolicy { enabled: boolean; ingressAction: 'allow' | 'drop' | 'reject'; egressAction: 'allow' | 'drop' | 'reject'; rules: FirewallRule[] }
export const defaultEgress: EgressPolicy = { mode: 'off', strategy: 'failover', proxyNodeIds: [], internalCidrs: [], probeUrl: 'https://1.1.1.1/cdn-cgi/trace', probeIntervalSeconds: 30 }
export const defaultFirewall: FirewallPolicy = { enabled: false, ingressAction: 'allow', egressAction: 'allow', rules: [] }
export class NetworkInputError extends Error { readonly statusCode = 400 }
export function object(value: unknown): Record<string, any> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new NetworkInputError('Expected an object')
  return value as Record<string, any>
}
export function cidr(value: unknown): string {
  if (typeof value !== 'string' || value.length > 64) throw new NetworkInputError('Invalid IP/CIDR')
  const [ip, prefix, extra] = value.trim().split('/')
  const family = isIP(ip)
  if (!family || extra !== undefined || (prefix !== undefined && (!/^\d+$/.test(prefix) || Number(prefix) > (family === 4 ? 32 : 128)))) throw new NetworkInputError('Invalid IP/CIDR')
  return value.trim()
}
export function ids(value: unknown, max = 16): number[] {
  if (!Array.isArray(value) || value.length > max || value.some(id => !Number.isSafeInteger(id) || id < 1 || id > 2147483647) || new Set(value).size !== value.length) throw new NetworkInputError('Invalid or duplicate node IDs')
  return value
}
export function probeUrl(value: unknown): string {
  if (typeof value !== 'string' || value.length > 2048) throw new NetworkInputError('Invalid probe URL')
  try { const url = new URL(value); if (url.protocol === 'https:' && !url.username && !url.password && url.hostname) return url.href } catch { /* rejected below */ }
  throw new NetworkInputError('Probe URL must use HTTPS without credentials')
}
export function validateEgress(value: unknown): EgressPolicy {
  const v = object(value)
  if (!['off', 'direct', 'proxy'].includes(v.mode) || !['balance', 'failover', 'leastPing'].includes(v.strategy)) throw new NetworkInputError('Invalid egress mode or strategy')
  const proxyNodeIds = ids(v.proxyNodeIds)
  if (v.mode === 'proxy' && !proxyNodeIds.length) throw new NetworkInputError('Select at least one proxy node')
  if (!Array.isArray(v.internalCidrs) || v.internalCidrs.length > 64) throw new NetworkInputError('Invalid internal networks')
  if (!Number.isInteger(v.probeIntervalSeconds) || v.probeIntervalSeconds < 10 || v.probeIntervalSeconds > 3600) throw new NetworkInputError('Probe interval must be 10–3600 seconds')
  return { mode: v.mode, strategy: v.strategy, proxyNodeIds, internalCidrs: v.internalCidrs.map(cidr), probeUrl: probeUrl(v.probeUrl), probeIntervalSeconds: v.probeIntervalSeconds }
}
function subjects(value: unknown): string {
  if (typeof value !== 'string' || value.length > 2048) throw new NetworkInputError('Invalid ACL addresses')
  if (!value.trim()) return ''
  const list = value.split(',').map(s => cidr(s.trim()))
  if (list.length > 32) throw new NetworkInputError('Too many ACL addresses')
  return list.join(',')
}
function ports(value: unknown): string {
  if (typeof value !== 'string' || value.length > 256) throw new NetworkInputError('Invalid ACL ports')
  if (!value.trim()) return ''
  const list = value.split(',').map(s => s.trim())
  for (const part of list) {
    if (!/^\d{1,5}(-\d{1,5})?$/.test(part)) throw new NetworkInputError('Invalid ACL port range')
    const [start, end = start] = part.split('-').map(Number)
    if (start < 1 || end > 65535 || end < start) throw new NetworkInputError('Invalid ACL port range')
  }
  return list.join(',')
}
export function validateFirewall(value: unknown): FirewallPolicy {
  const v = object(value)
  if (typeof v.enabled !== 'boolean' || !['allow', 'drop', 'reject'].includes(v.ingressAction) || !['allow', 'drop', 'reject'].includes(v.egressAction) || !Array.isArray(v.rules) || v.rules.length > 200) throw new NetworkInputError('Invalid firewall policy')
  const rules = v.rules.map((item: unknown): FirewallRule => {
    const r = object(item)
    if (!['ingress', 'egress'].includes(r.direction) || !['allow', 'drop', 'reject'].includes(r.action) || !['', 'tcp', 'udp', 'icmp4', 'icmp6'].includes(r.protocol) || typeof r.enabled !== 'boolean' || typeof r.description !== 'string' || r.description.length > 200) throw new NetworkInputError('Invalid ACL rule')
    const sourcePort = ports(r.sourcePort), destinationPort = ports(r.destinationPort)
    if ((sourcePort || destinationPort) && !['tcp', 'udp'].includes(r.protocol)) throw new NetworkInputError('Ports require TCP or UDP')
    const source = subjects(r.source), destination = subjects(r.destination)
    const families = new Set([source, destination].filter(Boolean).flatMap(s => s.split(',').map(ip => isIP(ip.split('/')[0]))))
    if (families.size > 1 || (r.protocol === 'icmp4' && families.has(6)) || (r.protocol === 'icmp6' && families.has(4))) throw new NetworkInputError('Use a separate ACL rule for each address family')
    return { direction: r.direction, action: r.action, protocol: r.protocol, source, destination, sourcePort, destinationPort, enabled: r.enabled, description: r.description }
  })
  return { enabled: v.enabled, ingressAction: v.ingressAction, egressAction: v.egressAction, rules }
}
