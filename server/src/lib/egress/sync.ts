import { prisma } from '../../db/prisma.js'
import { decryptSensitiveData } from '../security.js'
import { defaultEgress, defaultFirewall, type EgressPolicy } from './policy.js'

const failureReasons = ['command_failed', 'timeout', 'permission_denied', 'missing_file', 'config_invalid', 'reality_key_invalid', 'reality_id_invalid', 'tls_config_invalid', 'unsupported_transport']

export function decodeProxyConfig(encrypted: string): any {
  const plain = decryptSensitiveData(encrypted)
  if (!plain) throw new Error('Proxy configuration cannot be decrypted')
  return JSON.parse(plain)
}

export function sanitizeNetworkStatus(status: unknown) {
  if (!status || typeof status !== 'object') throw new Error('Invalid network status')
  const report = status as Record<string, unknown>
  if (!Number.isSafeInteger(report.appliedRevision) || Number(report.appliedRevision) < 0) throw new Error('Invalid network revision')
  return {
      state: ['applied', 'applying', 'error', 'unsupported', 'idle'].includes(String(report.state)) ? String(report.state) : 'error',
      error: ['none', 'preflight', 'incus', 'acl', 'xray', 'network', 'unsupported-nic', 'unsupported-platform', 'restore'].includes(String(report.error)) ? String(report.error) : 'network',
      errorStage: ['extract', 'config', 'start', 'health', 'preflight', 'incus', 'acl', 'xray', 'network', 'unsupported-nic', 'unsupported-platform', 'restore'].includes(String(report.errorStage)) ? String(report.errorStage) : '',
      errorDetail: failureReasons.includes(String(report.errorDetail)) ? String(report.errorDetail) : '',
      xrayAvailable: report.xrayAvailable === true,
      activeNodeId: Number.isSafeInteger(report.activeNodeId) ? Number(report.activeNodeId) : null,
      managedNetworks: Array.isArray(report.managedNetworks) ? report.managedNetworks.filter(x => typeof x === 'string' && /^[A-Za-z0-9_.:-]{1,64}$/.test(x)).slice(0, 64) : [],
      routedFallback: report.routedFallback === true,
      healthyNodeIds: Array.isArray(report.healthyNodeIds) ? report.healthyNodeIds.filter(x => Number.isSafeInteger(x) && Number(x) > 0).slice(0, 16) : []
    }
}
export async function recordNetworkStatus(hostId: number, report: unknown, expectedRevision?: number) {
  const policy = await prisma.hostNetworkPolicy.findUnique({ where: { hostId } })
  if (!policy || !report || typeof report !== 'object' || (expectedRevision !== undefined && policy.revision !== expectedRevision)) return
  const raw = report as Record<string, unknown>
  let status: ReturnType<typeof sanitizeNetworkStatus>
  try { status = sanitizeNetworkStatus(raw) } catch { return }
  const applied = Number(raw.appliedRevision)
  if (applied > policy.revision || applied < policy.appliedRevision) return
  await prisma.hostNetworkPolicy.updateMany({ where: { hostId, revision: policy.revision }, data: { appliedRevision: applied, status, reportedAt: new Date() } })
}
export async function networkInstruction(hostId: number) {
  const policy = await prisma.hostNetworkPolicy.findUnique({ where: { hostId } })
  const egress = (policy?.egress ?? defaultEgress) as unknown as EgressPolicy
  const ids = egress.mode === 'proxy' ? egress.proxyNodeIds : []
  const nodes = ids.length ? await prisma.proxyNode.findMany({ where: { id: { in: ids } } }) : []
  const byId = new Map(nodes.map(node => [node.id, node]))
  return {
    revision: policy?.revision ?? 0,
    egress,
    firewall: policy?.firewall ?? defaultFirewall,
    nodes: ids.map(id => { const node = byId.get(id); if (!node) throw new Error('Referenced proxy node is missing'); return { id, outbound: decodeProxyConfig(node.configEncrypted) } })
  }
}
