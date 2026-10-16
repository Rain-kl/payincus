import { Prisma } from '@prisma/client'
import { prisma } from '../../db/prisma.js'
import { decryptSensitiveData } from '../security.js'
import { defaultEgress, defaultFirewall, type EgressPolicy } from './policy.js'

const failureReasons = ['command_failed', 'timeout', 'permission_denied', 'missing_file', 'config_invalid', 'reality_key_invalid', 'reality_id_invalid', 'tls_config_invalid', 'unsupported_transport']

export function decodeProxyConfig(encrypted: string): any {
  const plain = decryptSensitiveData(encrypted)
  if (!plain) throw new Error('Proxy configuration cannot be decrypted')
  return JSON.parse(plain)
}

export async function networkHeartbeat(hostId: number, status: unknown, results: unknown) {
  const acknowledgedProbeJobs: string[] = []
  const policy = await prisma.hostNetworkPolicy.findUnique({ where: { hostId } })
  if (policy && status && typeof status === 'object') {
    const report = status as Record<string, unknown>
    const applied = Number.isSafeInteger(report.appliedRevision) && Number(report.appliedRevision) >= 0 && Number(report.appliedRevision) <= policy.revision ? Number(report.appliedRevision) : policy.appliedRevision
    const safe = {
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
    await prisma.hostNetworkPolicy.updateMany({ where: { hostId, revision: policy.revision }, data: { appliedRevision: applied, status: safe, reportedAt: new Date() } })
  }
  if (Array.isArray(results)) for (const raw of results.slice(0, 3)) {
    if (!raw || typeof raw !== 'object') continue
    const value = raw as Record<string, unknown>
    if (typeof value.jobId !== 'string' || !Array.isArray(value.items)) continue
    const job = await prisma.proxyProbeJob.findFirst({ where: { id: value.jobId, hostId } })
    if (!job) continue
    if (job.state === 'completed' || job.state === 'expired') { acknowledgedProbeJobs.push(job.id); continue }
    if (job.state !== 'running' || job.expiresAt <= new Date()) continue
    const nodeIds = job.nodeIds as number[]
    const items = value.items as Array<Record<string, unknown>>
    if (items.length !== nodeIds.length || new Set(items.map(i => i?.nodeId)).size !== nodeIds.length || items.some(i => !i || !nodeIds.includes(Number(i.nodeId)))) continue
    const safe = items.map(i => ({ nodeId: Number(i.nodeId), success: i.success === true, latencyMs: typeof i.latencyMs === 'number' && Number.isFinite(i.latencyMs) ? Math.max(0, Math.min(120000, Math.round(i.latencyMs))) : null, error: i.success === true ? '' : ['timeout', 'handshake', 'http', 'xray', 'unsupported', 'network'].includes(String(i.error)) ? String(i.error) : 'network', errorDetail: i.success !== true && failureReasons.includes(String(i.errorDetail)) ? String(i.errorDetail) : '' }))
    const completed = await prisma.proxyProbeJob.updateMany({ where: { id: job.id, hostId, state: 'running' }, data: { state: 'completed', result: safe, completedAt: new Date(), leaseUntil: null, snapshotEncrypted: '' } })
    if (completed.count) acknowledgedProbeJobs.push(job.id)
  }
  const now = new Date()
  await prisma.proxyProbeJob.updateMany({ where: { hostId, state: { in: ['pending', 'running'] }, expiresAt: { lte: now } }, data: { state: 'expired', snapshotEncrypted: '', leaseUntil: null } })
  const candidate = await prisma.proxyProbeJob.findFirst({ where: { hostId, expiresAt: { gt: now }, OR: [{ state: 'pending' }, { state: 'running', leaseUntil: { lte: now } }] }, orderBy: { createdAt: 'asc' } })
  const probes: Array<Record<string, unknown>> = []
  if (candidate) {
    const claimed = await prisma.proxyProbeJob.updateMany({ where: { id: candidate.id, state: candidate.state, leaseUntil: candidate.leaseUntil }, data: { state: 'running', leaseUntil: new Date(now.getTime() + 5 * 60_000) } })
    if (claimed.count) probes.push({ jobId: candidate.id, probeUrl: candidate.probeUrl, nodes: decodeProxyConfig(candidate.snapshotEncrypted) })
  }
  const egress = (policy?.egress ?? defaultEgress) as unknown as EgressPolicy
  const ids = egress.mode === 'proxy' ? egress.proxyNodeIds : []
  const nodes = ids.length ? await prisma.proxyNode.findMany({ where: { id: { in: ids } } }) : []
  const byId = new Map(nodes.map(node => [node.id, node]))
  return {
    revision: policy?.revision ?? 0,
    egress,
    firewall: policy?.firewall ?? defaultFirewall,
    nodes: ids.map(id => { const node = byId.get(id); if (!node) throw new Error('Referenced proxy node is missing'); return { id, outbound: decodeProxyConfig(node.configEncrypted) } }),
    probes,
    acknowledgedProbeJobs
  } as unknown as Prisma.InputJsonObject
}
