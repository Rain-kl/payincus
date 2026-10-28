import type { Prisma, StoragePool } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { evaluateHostService } from './host-service-status.js'
import { storageHeadroomMB, storageReportMaxAgeMs } from './storage-capacity-policy.js'

type Client = Prisma.TransactionClient | typeof prisma
const MIB = 1024 * 1024
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

function group(pool: StoragePool): string {
  const config = (pool.config || {}) as Record<string, string>
  if (pool.driver === 'dir' || pool.driver === 'btrfs') return `filesystem:${(pool.telemetry as { filesystemId?: string } | null)?.filesystemId || 'unknown'}`
  if (pool.driver === 'zfs') return `zfs:${config['zfs.pool_name']?.split('/')[0] || config.source?.split('/')[0] || pool.name}`
  if (pool.driver === 'lvm') return `lvm:${config['lvm.vg_name'] || config.source || pool.name}`
  return `${pool.driver}:${pool.name}`
}

export async function getStorageCapacity(hostId: number, options: {
  client?: Client; packageId?: number | null; poolName?: string | null; requiredMB?: number
} = {}): Promise<StorageCapacity> {
  const client = options.client || prisma
  // Node availability is checked before pool health and capacity. No node I/O is permitted here.
  const host = await client.host.findUnique({ where: { id: hostId }, include: {
    nodeSecurity: { select: { report: true } }, agent: { select: { status: true, lastSeenAt: true, lastReport: true } }
  } })
  const empty = (reason: StorageUnavailableReason): StorageCapacity => ({ poolName: null, totalMB: 0, usedMB: 0, allocatedMB: 0, availableMB: 0, headroomMB: 0, reason })
  if (!host || host.status !== 'online' || !evaluateHostService(host).available) return empty('storage_unavailable')
  const [pools, instances, binding] = await Promise.all([
    client.storagePool.findMany({ where: { hostId, purpose: 'instance_data' }, orderBy: { name: 'asc' } }),
    client.instance.findMany({ where: { hostId, status: { not: 'deleted' } }, select: { incusId: true, storagePoolName: true, disk: true } }),
    options.packageId ? client.packageHost.findUnique({ where: { packageId_hostId: { packageId: options.packageId, hostId } }, select: { storagePoolName: true } }) : null
  ])
  if (options.packageId && !binding) return empty('storage_pool_missing')
  const selected = options.poolName || binding?.storagePoolName
  const candidates = selected ? pools.filter(p => p.name === selected) : pools
  if (!candidates.length) return empty('storage_pool_missing')
  const maxAge = storageReportMaxAgeMs(host.agent?.lastReport)
  const ready = (pool: StoragePool) => pool.status === 'ready' && !!pool.reportedAt && Date.now() - pool.reportedAt.getTime() <= maxAge &&
    !(pool.config as Record<string, unknown> | null)?._payincusImport && !(pool.config as Record<string, unknown> | null)?._payincusDeletion && pool.totalBytes > 0n
  const unassigned = Math.max(0, host.diskUsed - instances.reduce((sum, i) => sum + i.disk, 0))
  const choices = candidates.map(pool => {
    const totalMB = Math.floor(Number(pool.totalBytes) / MIB)
    const usedMB = Math.ceil(Number(pool.usedBytes) / MIB)
    const siblings = pools.filter(p => group(p) === group(pool))
    const members = new Set(siblings.map(p => p.name))
    // Unmapped instances reserve space conservatively until their root pool is reported.
    const allocatedMB = instances.filter(i => !i.storagePoolName || i.storagePoolName === pool.name).reduce((sum, i) => sum + i.disk, 0)
    const groupAllocated = instances.filter(i => !i.storagePoolName || members.has(i.storagePoolName)).reduce((sum, i) => sum + i.disk, 0)
    const headroomMB = storageHeadroomMB(totalMB)
    const groupTotal = Math.max(...siblings.map(p => Math.floor(Number(p.totalBytes) / MIB)))
    const backing = (pool.telemetry as { backing?: { availableBytes: number; id: string } } | null)?.backing
    const backingPools = backing ? pools.filter(p => (p.telemetry as { backing?: { id: string } } | null)?.backing?.id === backing.id) : []
    const backingNames = new Set(backingPools.map(p => p.name))
    const backingReserved = instances.filter(i => !i.storagePoolName || backingNames.has(i.storagePoolName)).reduce((sum, i) => sum + i.disk, 0)
    const availableMB = ready(pool) ? Math.max(0, Math.floor(Math.min(
      totalMB - headroomMB - allocatedMB - unassigned,
      totalMB - usedMB - headroomMB,
      groupTotal - storageHeadroomMB(groupTotal) - groupAllocated - unassigned,
      // Sparse vdevs share their backing filesystem: never promise its remaining bytes twice.
      backing ? Math.floor(backing.availableBytes / MIB) - 1024 - backingReserved - unassigned : Infinity
    ))) : 0
    return { poolName: pool.name, totalMB, usedMB, allocatedMB, availableMB, headroomMB,
      reason: !ready(pool) ? 'storage_unavailable' as const : availableMB < (options.requiredMB ?? 1) ? 'disk_full' as const : null }
  })
  choices.sort((a, b) => Number(!!a.reason) - Number(!!b.reason) || b.availableMB - a.availableMB || a.poolName.localeCompare(b.poolName))
  return choices[0]
}

export async function getStorageOverview(hostId: number) {
  const [pools, instances] = await Promise.all([
    prisma.storagePool.findMany({ where: { hostId, purpose: 'instance_data' }, orderBy: { name: 'asc' } }),
    prisma.instance.findMany({ where: { hostId, status: { not: 'deleted' } }, select: { disk: true } })
  ])
  // Last known totals survive outages/locks. Allocation gates independently reject unavailable telemetry.
  return {
    totalMB: pools.reduce((sum, p) => sum + Math.floor(Number(p.totalBytes) / MIB), 0),
    usedMB: pools.reduce((sum, p) => sum + Math.ceil(Number(p.usedBytes) / MIB), 0),
    allocatedMB: instances.reduce((sum, i) => sum + i.disk, 0),
    pools: pools.map(p => ({ poolName: p.name, totalMB: Math.floor(Number(p.totalBytes) / MIB), usedMB: Math.ceil(Number(p.usedBytes) / MIB), status: p.status, reportedAt: p.reportedAt }))
  }
}

export async function getInstanceStorageCapacity(instanceId: number, requiredMB: number, client: Client = prisma): Promise<StorageCapacity | null> {
  const instance = await client.instance.findUnique({ where: { id: instanceId }, select: { hostId: true, storagePoolName: true } })
  if (!instance?.storagePoolName) return null
  return getStorageCapacity(instance.hostId, { client, poolName: instance.storagePoolName, requiredMB })
}
