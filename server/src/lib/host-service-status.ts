import type { Prisma } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { deriveEffectiveHostStatus, hostTunnelManager } from './incus/tunnel-manager.js'
import { isHostHealthy } from './incus/host-health.js'

export interface HostServiceStatus {
  status: 'online' | 'offline' | 'maintenance' | 'locked'
  available: boolean
  pendingInstances?: string[]
}
export function evaluateHostService(host: {
  id: number; status: string; tunnelEnabled?: boolean
  nodeSecurity?: { report: unknown } | null
  agent?: { status: string; lastSeenAt: Date | null; lastReport: unknown } | null
}, now = Date.now()): HostServiceStatus {
  const state = host.nodeSecurity?.report as { locked?: boolean; resumeInstances?: Array<{project: string; name: string}> } | undefined
  const report = host.agent?.lastReport as { incus?: { available?: boolean }; instances?: { available?: boolean }; metrics?: { heartbeatIntervalSeconds?: number } } | undefined
  const status = state?.locked === true ? 'locked' : deriveEffectiveHostStatus(host)
  const reportedInterval = Number(report?.metrics?.heartbeatIntervalSeconds)
  const interval = Number.isFinite(reportedInterval) && reportedInterval >= 5 && reportedInterval <= 3600 ? reportedInterval : 5
  const stale = host.agent && (host.agent.status !== 'online' || !host.agent.lastSeenAt || now - host.agent.lastSeenAt.getTime() > Math.max(120, interval * 3) * 1000)
  return { status, pendingInstances: Array.isArray(state?.resumeInstances) ? state.resumeInstances.filter(item => item.project === 'default').map(item => item.name) : [], available: (status === 'online' || status === 'maintenance') && (!host.tunnelEnabled || hostTunnelManager.isTunnelOnline(host.id)) && !stale && (!host.agent || report?.incus?.available === true) && report?.instances?.available !== false && isHostHealthy(host.id) }
}

export async function loadHostServiceStatuses(ids?: number[]): Promise<Map<number, HostServiceStatus>> {
  const unique = ids ? [...new Set(ids)] : undefined
  if (unique && !unique.length) return new Map()
  const hosts = await prisma.host.findMany({ where: unique ? { id: { in: unique } } : {}, select: {
    id: true, status: true, tunnelEnabled: true,
    nodeSecurity: { select: { report: true } },
    agent: { select: { status: true, lastSeenAt: true, lastReport: true } }
  } })
  return new Map(hosts.map(host => [host.id, evaluateHostService(host)]))
}

// Presentation only: an unavailable node must never change provisioning/refund or suspension state.
export function instanceServiceStatus(status: string, host: HostServiceStatus | undefined, name?: string | null): string {
  return ['running', 'stopped', 'error'].includes(status) && (!host?.available || (!!name && host.pendingInstances?.includes(name))) ? 'error' : status
}

// Keep pagination counts and status filters consistent with the presentation overlay.
export function instanceServiceStatusFilter(status: 'running' | 'stopped' | 'error', hosts: Map<number, HostServiceStatus>): Prisma.InstanceWhereInput {
  const unavailable: Prisma.InstanceWhereInput[] = []
  for (const [hostId, host] of hosts) {
    if (!host.available) unavailable.push({ hostId })
    else if (host.pendingInstances?.length) unavailable.push({ hostId, incusId: { in: host.pendingInstances } })
  }
  if (!unavailable.length) return { status }
  return status === 'error'
    ? { OR: [{ status: 'error' }, { status: { in: ['running', 'stopped'] }, OR: unavailable }] }
    : { status, NOT: { OR: unavailable } }
}
