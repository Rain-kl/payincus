import { prisma } from '../../db/prisma.js'
import { StorageEncryptionError } from './encryption.js'
import { loadHostServiceStatuses } from '../host-service-status.js'
import { storageReportMaxAgeMs } from '../storage-capacity-policy.js'

export async function storedStoragePools(hostId: number) {
  const [pools, services, agent] = await Promise.all([
    prisma.storagePool.findMany({ where: { hostId }, orderBy: { createdAt: 'asc' } }),
    loadHostServiceStatuses([hostId]),
    prisma.hostAgent.findUnique({ where: { hostId }, select: { lastReport: true } })
  ])
  return pools.map(pool => {
    const telemetry = pool.telemetry as { usedBy?: number; backing?: { sizeBytes: number } } | null
    const config = (pool.config || {}) as Record<string, any>
    return { name: pool.name, driver: pool.driver, description: pool.description || '', purpose: pool.purpose,
      config: { ...config, ...(telemetry?.backing?.sizeBytes ? { size: `${telemetry.backing.sizeBytes / 1024 ** 3}GiB` } : {}) },
      status: services.get(hostId)?.status === 'locked' ? 'locked' : pool.status === 'ready' && (!services.get(hostId)?.available || !pool.reportedAt || Date.now() - pool.reportedAt.getTime() > storageReportMaxAgeMs(agent?.lastReport)) ? 'unavailable' : pool.status,
      pendingImport: !!config._payincusImport, usedBy: telemetry?.usedBy || 0,
      reportedAt: pool.reportedAt?.toISOString() || null,
      space: pool.totalBytes > 0n ? { total: Number(pool.totalBytes), used: Number(pool.usedBytes) } : null }
  })
}

export async function storedPoolEncryption(hostId: number, name: string) {
  const pool = await prisma.storagePool.findUnique({ where: { hostId_name: { hostId, name } } })
  if (!pool) throw new StorageEncryptionError(404, '存储池不存在')
  if (pool.driver !== 'zfs') return { encrypted: false, state: 'plain', dataset: '', encryptionRoot: '', keySaved: false }
  const state = (pool.telemetry as { encryption?: Record<string, any> } | null)?.encryption
  if (!state) throw new StorageEncryptionError(409, '尚未收到节点存储状态，请验证节点或等待心跳上报')
  const saved = state.encryptionRoot ? await prisma.nodeStorageKey.findUnique({ where: { hostId_dataset: { hostId, dataset: state.encryptionRoot } }, select: { id: true } }) : null
  return { ...state, state: state.encrypted ? state.state : 'plain', keySaved: !!saved, pendingImport: !!(pool.config as Record<string, unknown> | null)?._payincusImport }
}
