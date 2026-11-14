import type { Prisma } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { IncusApiError, type IncusClient } from '../lib/incus/incus-client.js'
import { getIncusClient } from '../lib/incus/incus-pool.js'
import { recordHostSuccess } from '../lib/incus/host-health.js'
import { extractHostPublicIpv6FromResources } from '../lib/host-resources.js'
import { syncHostGpuReport } from '../db/gpus.js'
import { recordReport } from '../lib/node-security/report.js'
import { syncDesiredSecurityPolicy } from '../lib/node-security/policy-sync.js'
import { storedStoragePools } from '../lib/storage/read-model.js'
import { processAgentStorageReport } from './agent-storage-report.js'

const isRecord = (v: unknown): v is Record<string, any> => !!v && typeof v === 'object' && !Array.isArray(v)

export interface StorageBaselineSyncResult {
  usedBaselineEndpoint: boolean
  storageAccepted: boolean
  locked: boolean
  nodePoolNames: string[]
  missingPools: string[]
  untrackedPools: string[]
  pools: Awaited<ReturnType<typeof storedStoragePools>>
  nodeSecuritySynced: boolean
}

function isEndpointUnsupported(err: unknown): boolean {
  if (err instanceof IncusApiError) {
    return err.statusCode === 404 || err.statusCode === 405 || err.statusCode === 501
  }
  return false
}

async function computeMonotonicSyncTimestamp(hostId: number): Promise<Date> {
  let floorMs = Date.now()
  if (typeof prisma.storagePool?.findMany !== 'function') {
    return new Date(floorMs)
  }
  const existingPools = await prisma.storagePool.findMany({
    where: { hostId },
    select: { createdAt: true, reportedAt: true }
  })
  for (const pool of existingPools) {
    if (pool.createdAt && pool.createdAt.getTime() > floorMs) floorMs = pool.createdAt.getTime()
    if (pool.reportedAt && pool.reportedAt.getTime() + 1 > floorMs) floorMs = pool.reportedAt.getTime() + 1
  }
  return new Date(floorMs)
}

async function collectFallbackStoragePayload(
  client: IncusClient
): Promise<{ storage: Record<string, any>; securityState: Record<string, any> | null }> {
  const [incusPools, agentStorage, securityState] = await Promise.all([
    client.request<Array<{ name: string; driver: string; status?: string; config?: Record<string, string>; used_by?: string[] }>>(
      'GET',
      '/1.0/storage-pools?recursion=1'
    ),
    client.request<Record<string, any>>('GET', '/1.0/agent/storage').catch(() => null),
    client.request<Record<string, any>>('GET', '/1.0/agent/security').catch(() => null)
  ])

  const datasets = Array.isArray(agentStorage?.datasets) ? (agentStorage!.datasets as Record<string, any>[]) : []
  const locks = isRecord(agentStorage?.locks) ? (agentStorage!.locks as Record<string, any>) : {}
  const nodeLocked = securityState?.locked === true

  const pools: Record<string, any>[] = []
  for (const pool of Array.isArray(incusPools) ? incusPools : []) {
    if (!pool || typeof pool.name !== 'string') continue
    const config = isRecord(pool.config) ? pool.config : {}
    const source = String(config['zfs.pool_name'] || config.source || '')
    let space: { total: number; used: number; available: number } | undefined
    if (!nodeLocked) {
      try {
        const res = await client.request<{ space?: { total?: number; used?: number; available?: number } }>(
          'GET',
          `/1.0/storage-pools/${encodeURIComponent(pool.name)}/resources`
        )
        if (res?.space && typeof res.space.total === 'number' && typeof res.space.used === 'number') {
          const total = Math.max(0, Math.floor(res.space.total))
          const used = Math.max(0, Math.min(total, Math.floor(res.space.used)))
          const avail = typeof res.space.available === 'number' && Number.isFinite(res.space.available)
            ? Math.max(0, Math.min(total, Math.floor(res.space.available)))
            : Math.max(0, total - used)
          space = { total, used, available: avail }
        }
      } catch {
        // Locked / unavailable dataset resources are tolerated gracefully below.
      }
    }

    let encryption: Record<string, any> | undefined
    if (pool.driver === 'zfs') {
      const ds = datasets.find(d => d?.dataset === source)
      const lock = ds?.encryptionRoot ? locks[ds.encryptionRoot] : undefined
      if (ds) {
        const encrypted = ds.algorithm ? ds.algorithm !== 'none' : true
        encryption = {
          encrypted,
          state: ds.keyStatus === 'available' ? 'unlocked' : 'locked',
          source: ds.dataset,
          encryptionRoot: ds.encryptionRoot || ds.dataset,
          manualLocked: lock?.locked === true,
          lockPending: lock?.locked === true && lock?.complete !== true,
          pendingRecovery: Array.isArray(lock?.resumeInstances) ? lock.resumeInstances.length : 0,
          nodeLocked,
          keyFormat: ds.keyFormat || ''
        }
      } else {
        encryption = {
          encrypted: false,
          state: nodeLocked ? 'locked' : 'unlocked',
          source,
          encryptionRoot: '',
          manualLocked: false,
          lockPending: false,
          pendingRecovery: 0,
          nodeLocked,
          keyFormat: ''
        }
      }
    }

    const poolStatus = nodeLocked
      ? 'locked'
      : pool.status && pool.status !== 'Created'
        ? 'unavailable'
        : space && space.total > 0
          ? 'ready'
          : 'unavailable'

    pools.push({
      name: pool.name,
      driver: pool.driver,
      status: poolStatus,
      usedBy: Array.isArray(pool.used_by) ? pool.used_by.length : 0,
      config,
      ...(space ? { space } : {}),
      ...(encryption ? { encryption } : {})
    })
  }

  return {
    securityState,
    storage: {
      available: true,
      complete: true,
      collectionComplete: true,
      pools,
      ...(datasets.length > 0 ? { datasets } : {}),
      ...(Object.keys(locks).length > 0 ? { locks } : {})
    }
  }
}

export interface SyncHostStorageBaselineOptions {
  skipPoolReload?: boolean
}

/**
 * 主动向节点同步存储池基线（及可选硬件/安全基线），替代 5 秒高频心跳里的全量存储扫描。
 */
export async function syncHostStorageBaseline(
  hostId: number,
  existingClient?: IncusClient,
  options: SyncHostStorageBaselineOptions = {}
): Promise<StorageBaselineSyncResult> {
  const host = await prisma.host.findUnique({
    where: { id: hostId },
    include: { agent: true, nodeSecurity: true }
  })
  if (!host) throw new Error('宿主机不存在')

  const client = existingClient ?? await getIncusClient(host)

  let usedBaselineEndpoint = false
  let baselinePayload: Record<string, any> | null = null
  let storagePayload: Record<string, any> | null = null
  let securityState: Record<string, any> | null = null

  try {
    baselinePayload = await client.request<Record<string, any>>('GET', '/1.0/agent/baseline')
    usedBaselineEndpoint = true
    if (isRecord(baselinePayload?.storage)) {
      storagePayload = { ...baselinePayload.storage }
    }
    if (isRecord(baselinePayload?.nodeSecurity?.state)) {
      securityState = baselinePayload.nodeSecurity.state
    }
  } catch (err) {
    if (!isEndpointUnsupported(err)) throw err
  }

  if (!storagePayload) {
    const fallback = await collectFallbackStoragePayload(client)
    storagePayload = fallback.storage
    securityState = securityState ?? fallback.securityState
  }

  const syncTime = await computeMonotonicSyncTimestamp(hostId)
  storagePayload = {
    ...storagePayload,
    reportedAt: syncTime.toISOString()
  }

  recordHostSuccess(hostId, 0)

  let nodeSecuritySynced = false
  let locked = securityState?.locked === true
  if (securityState && typeof securityState.enabled === 'boolean' && typeof securityState.locked === 'boolean' && Number.isSafeInteger(securityState.revision)) {
    await recordReport(hostId, { state: securityState, events: [] }, { authoritative: true })
    nodeSecuritySynced = true
    locked = securityState.locked === true
  }

  // 解锁状态下，顺便把最新静态硬件/GPU/IPv6 基线写入 hostAgent.lastReport 与 Host 记录
  if (baselinePayload && !locked && isRecord(baselinePayload.resources) && host.agent) {
    const prevReport = isRecord(host.agent.lastReport) ? (host.agent.lastReport as Record<string, any>) : {}
    const prevResources = isRecord(prevReport.resources) ? prevReport.resources : {}
    const mergedReport = {
      ...prevReport,
      ...(isRecord(baselinePayload.runtime) ? { runtime: baselinePayload.runtime } : {}),
      ...(isRecord(baselinePayload.incus) ? { incus: baselinePayload.incus } : {}),
      ...(isRecord(baselinePayload.caddy) ? { caddy: baselinePayload.caddy } : {}),
      resources: {
        ...prevResources,
        ...baselinePayload.resources
      }
    } as Prisma.InputJsonObject

    await prisma.hostAgent.update({
      where: { hostId },
      data: { lastReport: mergedReport }
    }).catch(() => {})

    if (baselinePayload.resources.nvidiaGpu !== undefined) {
      await syncHostGpuReport(hostId, baselinePayload.resources.nvidiaGpu).catch(() => {})
    }
    const goarch = baselinePayload.runtime?.goarch
    const architecture = goarch === 'arm64' ? 'aarch64' : goarch === 'amd64' ? 'x86_64' : undefined
    const ipv6 = extractHostPublicIpv6FromResources(baselinePayload.resources)
    if (architecture || ipv6) {
      await prisma.host.updateMany({
        where: { id: hostId },
        data: {
          ...(architecture && !host.architecture ? { architecture } : {}),
          ...(ipv6 && !host.natPublicIpv6 ? { natPublicIpv6: ipv6 } : {})
        }
      }).catch(() => {})
    }
  }

  const storageAccepted = await processAgentStorageReport(hostId, storagePayload, syncTime)

  if (!locked && nodeSecuritySynced) {
    void syncDesiredSecurityPolicy(hostId)
  }

  const pools = options.skipPoolReload ? [] : await storedStoragePools(hostId)
  const dbPoolNames = new Set(pools.map(p => p.name))
  const nodePoolNames = Array.isArray(storagePayload.pools)
    ? storagePayload.pools
        .map((p: any) => (isRecord(p) && typeof p.name === 'string' ? p.name : ''))
        .filter(Boolean)
    : []

  const missingPools = pools.filter(p => p.status === 'missing').map(p => p.name)
  const untrackedPools = options.skipPoolReload ? [] : nodePoolNames.filter(name => !dbPoolNames.has(name))

  return {
    usedBaselineEndpoint,
    storageAccepted,
    locked,
    nodePoolNames,
    missingPools,
    untrackedPools,
    pools,
    nodeSecuritySynced
  }
}
