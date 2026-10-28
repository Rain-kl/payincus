import { prisma } from '../db/prisma.js'
import { poolEncryption, type StorageNode, type StorageKeyVault } from '../lib/storage/encryption.js'
import { nodeForPendingPool, unlockAndBindPool } from '../lib/storage/import.js'
import { withStorageSession } from '../lib/storage/session.js'

async function automaticStorageNeeded(node: StorageNode, hostId: number, name: string, vault: StorageKeyVault) {
  const security = await node.request<{ locked: boolean }>('GET', '/1.0/payincus/security')
  if (security.locked !== false) return false
  const readNode = await nodeForPendingPool(node, hostId, name)
  const state = await poolEncryption(readNode, name, vault)
  if (!state.encrypted || state.manualLocked || !state.keySaved) return false
  if (state.state === 'unlocked' && !state.pendingImport && !state.pendingRecovery) return false
  return true
}

export async function autoLoadStoragePool(node: StorageNode, hostId: number, name: string, vault: StorageKeyVault) {
  if (!await automaticStorageNeeded(node, hostId, name, vault)) return 'skipped' as const
  await unlockAndBindPool(node, hostId, name, vault, undefined, true)
  return 'loaded' as const
}

// The supported backend is single-process; the same per-host mutex also protects manual writes.
export function startStorageEncryptionLoader() {
  let stopped = false
  let active: Promise<void> | undefined
  const failed = new Set<string>()
  const run = async () => {
    const pools = await prisma.storagePool.findMany({ where: { driver: 'zfs' }, select: { hostId: true, name: true, config: true }, orderBy: { id: 'asc' } })
    for (const pool of pools) {
      if (stopped) return
      if (pool.config && typeof pool.config === 'object' && '_payincusDeletion' in pool.config) continue
      const identity = `${pool.hostId}/${pool.name}`
      try {
        const needed = await withStorageSession(pool.hostId, false, (node, vault) => automaticStorageNeeded(node, pool.hostId, pool.name, vault))
        if (needed) await withStorageSession(pool.hostId, true, (node, vault) => autoLoadStoragePool(node, pool.hostId, pool.name, vault))
        failed.delete(identity)
      } catch {
        if (!failed.has(identity)) console.warn(`[StorageEncryption] 节点 #${pool.hostId} 存储 ${pool.name} 自动加载未完成；请检查节点或托管状态`)
        failed.add(identity)
      }
    }
  }
  const tick = () => {
    if (stopped || active) return
    active = run().catch(() => { console.warn('[StorageEncryption] 无法读取存储记录，本轮自动加载已跳过') }).finally(() => { active = undefined })
  }
  const timer = setInterval(tick, 30000)
  timer.unref()
  tick()
  return async () => { stopped = true; clearInterval(timer); await active }
}
