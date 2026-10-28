import type { Prisma } from '@prisma/client'
import { prisma } from '../db/prisma.js'

const record = (value: unknown): value is Record<string, any> => !!value && typeof value === 'object' && !Array.isArray(value)
const bytes = (value: unknown): value is number => Number.isSafeInteger(value) && Number(value) >= 0

// Only update registered pools. Telemetry must never recreate a deleted pool or change its purpose.
export async function processAgentStorageReport(hostId: number, payload: unknown, receivedAt = new Date()) {
  if (!record(payload)) return
  const sampledAt = new Date(payload.reportedAt)
  if (!Number.isFinite(sampledAt.getTime()) || sampledAt.getTime() > receivedAt.getTime() + 30000 || sampledAt.getTime() < receivedAt.getTime() - 120000) return
  if (payload.available !== false && (payload.available !== true || !Array.isArray(payload.pools) || payload.pools.length > 256)) return
  const items = payload.available === true ? payload.pools as unknown[] : []
  const byName = new Map<string, Record<string, any>>()
  for (const item of items) {
    if (!record(item) || typeof item.name !== 'string' || byName.has(item.name)) return
    byName.set(item.name, item)
  }
  const pools = await prisma.storagePool.findMany({ where: { hostId } })
  for (const pool of pools) {
    if (sampledAt < pool.createdAt || (pool.reportedAt && sampledAt <= pool.reportedAt)) continue
    const config: Record<string, any> = record(pool.config) ? pool.config as Record<string, any> : {}
    if (config._payincusDeletion) continue
    let item = byName.get(pool.name)
    if (config._payincusImport && !item && Array.isArray(payload.datasets)) {
      const dataset = payload.datasets.find((d: any) => record(d) && d.dataset === config._payincusImport.source)
      if (dataset) {
        const lock = payload.locks?.[dataset.encryptionRoot]
        item = { name: pool.name, driver: pool.driver, status: 'locked', config: { source: dataset.dataset }, encryption: { encrypted: true, source: dataset.dataset, encryptionRoot: dataset.encryptionRoot,
          state: dataset.keyStatus === 'available' ? 'unlocked' : 'locked', keyFormat: dataset.keyFormat, manualLocked: lock?.locked === true,
          lockPending: lock?.locked === true && lock?.complete !== true, pendingRecovery: Array.isArray(lock?.resumeInstances) ? lock.resumeInstances.length : 0 } }
      }
    }
    if (!item && payload.available === true && payload.complete !== true) continue
    const data: Prisma.StoragePoolUpdateManyMutationInput = { reportedAt: sampledAt, status: item ? 'unavailable' : payload.available === true ? 'missing' : 'unavailable' }
    const source = config['zfs.pool_name'] || config.source
    const actualSource = item?.config?.['zfs.pool_name'] || item?.config?.source
    const matchingSource = !source || String(source).startsWith('/') || source === actualSource
    if (item && item.driver === pool.driver && matchingSource) {
      const validSpace = record(item.space) && bytes(item.space.total) && item.space.total > 0 && bytes(item.space.used) && item.space.used <= item.space.total
      data.status = config._payincusImport ? 'pending' : item.status === 'locked' ? 'locked' : item.status === 'ready' && validSpace ? 'ready' : 'unavailable'
      if (pool.driver === 'zfs' && !record(item.encryption)) data.status = 'unavailable'
      if (item.encryption?.encrypted === true && item.encryption?.state !== 'unlocked') data.status = 'locked'
      if (validSpace) {
        data.totalBytes = BigInt(item.space.total)
        data.usedBytes = BigInt(item.space.used)
      }
      const encryption = record(item.encryption) ? {
        encrypted: item.encryption.encrypted === true,
        state: item.encryption.state === 'unlocked' ? 'unlocked' : 'locked',
        dataset: String(item.encryption.source || '').slice(0, 200),
        encryptionRoot: String(item.encryption.encryptionRoot || '').slice(0, 200),
        manualLocked: item.encryption.manualLocked === true,
        lockPending: item.encryption.lockPending === true,
        pendingRecovery: Number.isSafeInteger(item.encryption.pendingRecovery) ? item.encryption.pendingRecovery : 0,
        keyFormat: String(item.encryption.keyFormat || '').slice(0, 20)
      } : null
      // Never store arbitrary node fields (especially key material) in pool telemetry.
      const backing = record(item.backing) && bytes(item.backing.sizeBytes) && bytes(item.backing.availableBytes) && typeof item.backing.id === 'string'
        ? { sizeBytes: item.backing.sizeBytes, availableBytes: item.backing.availableBytes, id: item.backing.id.slice(0, 100) } : null
      data.telemetry = { usedBy: Number.isSafeInteger(item.usedBy) ? item.usedBy : 0, encryption, backing,
        filesystemId: typeof item.filesystemId === 'string' ? item.filesystemId.slice(0, 100) : null }
      if (record(item.config) && Object.values(item.config).every(v => typeof v === 'string')) data.config = { ...config, ...item.config } as Prisma.InputJsonObject
    }
    await prisma.storagePool.updateMany({ where: { id: pool.id, createdAt: { lte: sampledAt }, OR: [{ reportedAt: null }, { reportedAt: { lt: sampledAt } }] }, data })
  }
}
