import { prisma } from '../../db/prisma.js'
import { IncusClient } from '../incus/incus-client.js'
import { hostTunnelManager } from '../incus/tunnel-manager.js'
import { recordReport } from './report.js'

const policySyncCooldownMs = 15_000
const lastPolicySyncAt = new Map<number, number>()
const policySyncInFlight = new Set<number>()

export function clearPolicySyncCooldown(hostId: number): void {
  lastPolicySyncAt.delete(hostId)
}

/**
 * 依据宿主机最近一次认证心跳/基线状态，同步控制端预期安全模式到节点（仅通过 mTLS 通道下发）。
 */
export async function syncDesiredSecurityPolicy(hostId: number, force = false): Promise<boolean> {
  if (policySyncInFlight.has(hostId)) return false
  const now = Date.now()
  if (!force && (lastPolicySyncAt.get(hostId) ?? 0) + policySyncCooldownMs > now) {
    return false
  }

  const securityState = await prisma.nodeSecurityState.findUnique({
    where: { hostId },
    select: { serverFingerprint: true, recoveryBlocked: true, report: true }
  })
  const report = securityState?.report && typeof securityState.report === 'object' && !Array.isArray(securityState.report)
    ? (securityState.report as Record<string, unknown>)
    : null
  if (!securityState?.serverFingerprint || securityState.recoveryBlocked || !report || typeof report._desiredEnabled !== 'boolean') {
    return false
  }
  const desiredEnabled = report._desiredEnabled
  if (report.locked === true || report.enabled === desiredEnabled) {
    return false
  }

  const host = await prisma.host.findUnique({ where: { id: hostId } })
  if (!host || (host.tunnelEnabled && !hostTunnelManager.isTunnelOnline(hostId))) {
    return false
  }

  lastPolicySyncAt.set(hostId, now)
  policySyncInFlight.add(hostId)
  const client = new IncusClient({
    url: host.url,
    certPath: host.certPath,
    keyPath: host.keyPath,
    hostId,
    tunnelEnabled: host.tunnelEnabled,
    targetHost: host.targetHost,
    targetPort: host.targetPort,
    serverFingerprint: securityState.serverFingerprint
  })

  try {
    await client.connect()
    const live = await client.request<{ enabled?: boolean; locked?: boolean; revision?: number }>('GET', '/1.0/agent/security')
    await recordReport(hostId, { state: live, events: [] }, { authoritative: true, desiredEnabled })
    if (live.locked || live.enabled === desiredEnabled || !Number.isSafeInteger(live.revision)) {
      return live.enabled === desiredEnabled
    }
    const updated = await client.request('POST', '/1.0/agent/security/policy', {
      enabled: desiredEnabled,
      revision: live.revision
    })
    await recordReport(hostId, { state: updated, events: [] }, { authoritative: true, desiredEnabled })
    return true
  } catch {
    return false
  } finally {
    policySyncInFlight.delete(hostId)
    await client.close().catch(() => {})
  }
}
