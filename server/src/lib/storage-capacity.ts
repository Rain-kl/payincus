import type { Prisma, Host as StoredHost } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { storageHeadroomMB, unconsumedStorageMB } from './storage-capacity-policy.js'

type Client = Prisma.TransactionClient | typeof prisma
const connectionFields = { id: true, url: true, certPath: true, keyPath: true, tunnelEnabled: true, targetHost: true, targetPort: true, diskUsed: true } as const
type Connection = Pick<StoredHost, keyof typeof connectionFields>
const MIB = 1024 * 1024
const MAX_AGE_MS = 5000
const READ_TIMEOUT_MS = 2500
type Pool = { name: string; driver: string; config: unknown; updatedAt: Date }
type Space = { totalMB: number; usedMB: number }
type Snapshot = { at: number; key: string; pools: Map<string, Space>; volumeUsedMB: Map<string, number> }
const snapshots = new Map<number, Snapshot>()
const pending = new Map<number, { key: string; value: Promise<Snapshot> }>()

export type StorageUnavailableReason = 'storage_unavailable' | 'storage_pool_missing' | 'disk_full'
export interface StorageCapacity {
  poolName: string | null
  totalMB: number
  usedMB: number
  allocatedMB: number
  availableMB: number
  headroomMB: number
  reason: StorageUnavailableReason | null
}

function space(value: any): Space | null {
  const total = value?.space?.total
  const used = value?.space?.used
  if (!Number.isSafeInteger(total) || !Number.isSafeInteger(used) || total <= 0 || used < 0 || used > total) return null
  return { totalMB: Math.floor(total / MIB), usedMB: Math.ceil(used / MIB) }
}

// Incus does not expose a filesystem ID for directory-backed pools. Treat all
// DIR/Btrfs paths on a host as potentially shared, instead of summing the same
// filesystem once per path. This is intentionally conservative on separate disks.
function group(pool: Pool): string {
  const config = (pool.config || {}) as Record<string, string>
  if (pool.driver === 'dir' || pool.driver === 'btrfs') return 'filesystem-paths'
  if (pool.driver === 'zfs') return `zfs:${config['zfs.pool_name']?.split('/')[0] || config.source?.split('/')[0] || 'unknown'}`
  if (pool.driver === 'lvm') return `lvm:${config['lvm.vg_name'] || config.source || 'unknown'}`
  return `${pool.driver}:unknown`
}

export function invalidateHostStorageCapacity(hostId: number): void {
  snapshots.delete(hostId)
}

async function snapshot(hostId: number, pools: Pool[], instances: Array<{ incusId: string; storagePoolName: string | null; package: { instanceType: string } | null }>, host: Connection): Promise<Snapshot> {
  const key = JSON.stringify([host.url, host.certPath, host.keyPath, host.tunnelEnabled, host.targetHost, host.targetPort,
    pools.map(p => [p.name, p.driver, p.config, p.updatedAt.getTime()])])
  const cached = snapshots.get(hostId)
  if (cached?.key === key && Date.now() - cached.at < MAX_AGE_MS) return cached
  const running = pending.get(hostId)
  if (running?.key === key) return running.value
  const work = (async (): Promise<Snapshot> => {
    const startedAt = Date.now()
    const result: Snapshot = { at: Date.now(), key, pools: new Map(), volumeUsedMB: new Map() }
    const signal = AbortSignal.timeout(READ_TIMEOUT_MS)
    try {
      const { getIncusClient } = await import('./incus/incus-pool.js')
      const client = await getIncusClient(host)
      let encryption: { locked?: boolean; datasets: Array<{ dataset: string; encryptionRoot: string; algorithm: string; keyStatus: string }>; locks?: Record<string, { locked: boolean }> } | undefined
      if (pools.some(pool => pool.driver === 'zfs')) {
        try { encryption = await client.request('GET', '/1.0/payincus/storage', null, READ_TIMEOUT_MS, signal) }
        catch { /* Encryption state is unknown: do not allocate ZFS based only on free bytes. */ }
      }
      await Promise.all(pools.map(async pool => {
        const config = pool.config as Record<string, unknown> | null
        if (config?._payincusImport || config?._payincusDeletion) return
        try {
          if (pool.driver === 'zfs') {
            if (!encryption || encryption.locked === true || !Array.isArray(encryption.datasets)) return
            let source = (config as Record<string, string> | null)?.['zfs.pool_name'] || (config as Record<string, string> | null)?.source
            if (!source || source.startsWith('/')) {
              const actual = await client.request<{ config: Record<string, string> }>('GET', `/1.0/storage-pools/${encodeURIComponent(pool.name)}`, null, READ_TIMEOUT_MS, signal)
              source = actual.config['zfs.pool_name'] || actual.config.source
            }
            const dataset = encryption.datasets.find(item => item.dataset === source)
            if (!dataset || (dataset.algorithm !== 'off' && dataset.algorithm !== '-' && (dataset.keyStatus !== 'available' || encryption.locks?.[dataset.encryptionRoot]?.locked))) return
          }
          const resources = await client.request('GET', `/1.0/storage-pools/${encodeURIComponent(pool.name)}/resources`, null, READ_TIMEOUT_MS, signal)
          const current = space(resources)
          if (current) result.pools.set(pool.name, current)
        } catch { /* Missing or unreachable capacity is unavailable, never unlimited. */ }
      }))
      // Actual root-volume usage is credited against logical reservations. If an
      // older driver cannot report it, reserve the full logical size for safety.
      const queue = instances.filter(i => i.storagePoolName && result.pools.has(i.storagePoolName)).slice(0, 128)
      await Promise.all(Array.from({ length: Math.min(4, queue.length) }, async () => {
        while (queue.length && !signal.aborted) {
          const instance = queue.shift()!
          for (const type of [instance.package?.instanceType === 'vm' ? 'virtual-machine' : 'container']) {
            try {
              const resources = await client.request('GET', `/1.0/storage-pools/${encodeURIComponent(instance.storagePoolName!)}/volumes/${type}/${encodeURIComponent(instance.incusId)}/resources`, null, READ_TIMEOUT_MS, signal)
              const used = (resources as any)?.space?.used
              if (Number.isSafeInteger(used) && used >= 0) result.volumeUsedMB.set(instance.incusId, Math.floor(used / MIB))
              break
            } catch { if (signal.aborted) break }
          }
        }
      }))
    } catch { /* An offline daemon is a capacity read failure, not disk exhaustion. */ }
    if (Date.now() - startedAt > READ_TIMEOUT_MS + 500) return { at: Date.now(), key, pools: new Map(), volumeUsedMB: new Map() }
    result.at = Date.now()
    if (snapshots.size >= 256) snapshots.delete(snapshots.keys().next().value!)
    snapshots.set(hostId, result)
    return result
  })()
  // Bound connect time too; a late request may not populate a fresh cache entry.
  let timer: ReturnType<typeof setTimeout>
  const bounded = Promise.race([work, new Promise<Snapshot>(resolve => {
    timer = setTimeout(() => {
      const failed: Snapshot = { at: Date.now(), key, pools: new Map(), volumeUsedMB: new Map() }
      snapshots.set(hostId, failed)
      resolve(failed)
    }, READ_TIMEOUT_MS + 500)
  })]).finally(() => { clearTimeout(timer); if (pending.get(hostId)?.value === bounded) pending.delete(hostId) })
  pending.set(hostId, { key, value: bounded })
  return bounded
}

export async function getStorageCapacity(hostId: number, options: {
  client?: Client
  packageId?: number | null
  poolName?: string | null
  requiredMB?: number
} = {}): Promise<StorageCapacity> {
  const client = options.client || prisma
  const [pools, instances, host, binding] = await Promise.all([
    client.storagePool.findMany({ where: { hostId, purpose: 'instance_data' }, orderBy: { name: 'asc' } }),
    client.instance.findMany({ where: { hostId, status: { not: 'deleted' } }, select: { incusId: true, storagePoolName: true, disk: true, package: { select: { instanceType: true } } } }),
    client.host.findUnique({ where: { id: hostId }, select: connectionFields }),
    options.packageId ? client.packageHost.findUnique({ where: { packageId_hostId: { packageId: options.packageId, hostId } }, select: { storagePoolName: true } }) : null
  ])
  const empty = (reason: StorageUnavailableReason): StorageCapacity => ({ poolName: null, totalMB: 0, usedMB: 0, allocatedMB: 0, availableMB: 0, headroomMB: 0, reason })
  const selected = options.poolName || binding?.storagePoolName
  const candidates = selected ? pools.filter(p => p.name === selected) : pools
  if (!candidates.length) return empty('storage_pool_missing')
  if (!host) return empty('storage_unavailable')
  const current = await snapshot(hostId, pools, instances, host)
  if (Date.now() - current.at > MAX_AGE_MS) return empty('storage_unavailable')
  const allocatedTotal = instances.reduce((sum, instance) => sum + instance.disk, 0)
  // Reservations not yet represented by an instance (e.g. host migration) count
  // against every candidate until the instance is attached to its destination.
  const unassigned = Math.max(0, (host?.diskUsed || 0) - allocatedTotal)
  const unconsumed = (rows: typeof instances, physicallyUsedMB: number) => unconsumedStorageMB(rows, current.volumeUsedMB, physicallyUsedMB)
  const choices: StorageCapacity[] = []
  for (const pool of candidates) {
    const currentSpace = current.pools.get(pool.name)
    if (!currentSpace) continue
    const siblings = pools.filter(p => group(p) === group(pool))
    // Shared backing filesystems are counted once, never as N independent pools.
    const groupFree = Math.max(...siblings.map(p => {
      const s = current.pools.get(p.name)
      return s ? s.totalMB - s.usedMB - storageHeadroomMB(s.totalMB) : 0
    }))
    const groupUsed = Math.max(...siblings.map(p => current.pools.get(p.name)?.usedMB || 0))
    const members = new Set(siblings.map(p => p.name))
    const own = instances.filter(i => !i.storagePoolName || i.storagePoolName === pool.name)
    const shared = instances.filter(i => !i.storagePoolName || members.has(i.storagePoolName))
    const headroomMB = storageHeadroomMB(currentSpace.totalMB)
    const free = currentSpace.totalMB - currentSpace.usedMB - headroomMB
    const availableMB = Math.max(0, Math.floor(Math.min(
      free - unconsumed(own, currentSpace.usedMB) - unassigned,
      groupFree - unconsumed(shared, groupUsed) - unassigned
    )))
    choices.push({ poolName: pool.name, ...currentSpace, allocatedMB: own.reduce((sum, i) => sum + i.disk, 0), availableMB, headroomMB,
      reason: availableMB >= (options.requiredMB || 0) ? null : 'disk_full' })
  }
  choices.sort((a, b) => b.availableMB - a.availableMB || a.poolName!.localeCompare(b.poolName!))
  if (choices[0]?.reason && candidates.some(p => !current.pools.has(p.name))) {
    return { ...choices[0], reason: 'storage_unavailable' }
  }
  return choices[0] || empty('storage_unavailable')
}

export async function getStorageOverview(hostId: number) {
  const pools = await prisma.storagePool.findMany({ where: { hostId, purpose: 'instance_data' }, orderBy: { name: 'asc' } })
  const capacities = await Promise.all(pools.map(pool => getStorageCapacity(hostId, { poolName: pool.name })))
  const groups = new Map<string, { totalMB: number; usedMB: number }>()
  for (const [index, capacity] of capacities.entries()) {
    const key = group(pools[index])
    const existing = groups.get(key) || { totalMB: 0, usedMB: 0 }
    groups.set(key, { totalMB: Math.max(existing.totalMB, capacity.totalMB), usedMB: Math.max(existing.usedMB, capacity.usedMB) })
  }
  return {
    totalMB: [...groups.values()].reduce((sum, g) => sum + g.totalMB, 0),
    usedMB: [...groups.values()].reduce((sum, g) => sum + g.usedMB, 0),
    availableMB: Math.max(0, ...capacities.map(c => c.availableMB)),
    reason: capacities.some(c => !c.reason) ? null : (capacities[0]?.reason || 'storage_pool_missing'),
    pools: capacities
  }
}

export async function getInstanceStorageCapacity(instanceId: number, requiredMB: number, client: Client = prisma): Promise<StorageCapacity | null> {
  const instance = await client.instance.findUnique({ where: { id: instanceId }, select: { hostId: true, incusId: true, storagePoolName: true } })
  if (!instance) return null
  let poolName = instance.storagePoolName
  if (!poolName) {
    try {
      const { getIncusClient } = await import('./incus/incus-pool.js')
      const host = await client.host.findUnique({ where: { id: instance.hostId }, select: connectionFields })
      if (!host) return null
      let timer: ReturnType<typeof setTimeout>
      const metadata = await Promise.race([
        (async () => {
          const incus = await getIncusClient(host)
          return incus.request<any>('GET', `/1.0/instances/${encodeURIComponent(instance.incusId)}`, null, READ_TIMEOUT_MS, AbortSignal.timeout(READ_TIMEOUT_MS))
        })(),
        new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error('Storage read timed out')), READ_TIMEOUT_MS + 500) })
      ]).finally(() => clearTimeout(timer))
      const devices = metadata.expanded_devices || metadata.devices || {}
      const root = Object.values(devices).find((d: any) => d.type === 'disk' && d.path === '/') as { pool?: string } | undefined
      poolName = root?.pool || null
    } catch { return null }
  }
  if (!poolName) return null
  return getStorageCapacity(instance.hostId, { client, poolName, requiredMB })
}
