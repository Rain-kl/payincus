import { pendingImportMarker } from './import.js'
import { randomBytes } from 'node:crypto'
import { prisma } from '../../db/prisma.js'
import { withStorageSession } from './session.js'
import { StorageEncryptionError } from './encryption.js'

const marker = '_payincusDeletion'
interface DeletionIntent {
  source: string
  token: string
  roots: Array<{ dataset: string; guid: string }>
  keys: Array<{ id: number; dataset: string; fingerprint: string }>
}
const within = (dataset: string, source: string) => dataset === source || dataset.startsWith(source + '/')

// Keep a durable cleanup checkpoint until remote deletion, registration and key removal all finish.
export async function deleteManagedZfsPool(hostId: number, poolName: string) {
  return withStorageSession(hostId, true, async node => {
    const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/payincus')
    if (!info.api_extensions?.includes('payincus_storage_encryption_v1')) throw new StorageEncryptionError(409, '节点尚不支持存储密钥同步清理，请先更新节点集成包')
    const record = await prisma.storagePool.findUnique({ where: { hostId_name: { hostId, name: poolName } } })
    const config = record?.config && typeof record.config === 'object' && !Array.isArray(record.config) ? record.config as Record<string, any> : {}
    let intent = config[marker] as DeletionIntent | undefined
    const paths = await node.request<string[]>('GET', '/1.0/storage-pools')
    if (!Array.isArray(paths)) throw new StorageEncryptionError(502, '无法核验节点存储池列表')
    const path = `/1.0/storage-pools/${encodeURIComponent(poolName)}`
    const exists = paths.includes(path)
    if (!exists && !intent && config[pendingImportMarker]) {
      await prisma.storagePool.deleteMany({ where: { hostId, name: poolName } })
      return { message: '已移除待导入记录；外部数据集及已有托管密钥均已保留' }
    }
    if (!intent) {
      if (!exists) throw new StorageEncryptionError(404, '节点存储池不存在，未删除任何托管密钥')
      const pool = await node.request<{ driver: string; description?: string; config: Record<string, string> }>('GET', path)
      const source = pool.config?.['zfs.pool_name'] || pool.config?.source
      if (pool.driver !== 'zfs' || !source || !/^[A-Za-z][A-Za-z0-9_.:-]*(\/[A-Za-z0-9_.:-]+)*$/.test(source) || source.includes('..')) throw new StorageEncryptionError(409, '无法确定待删除 ZFS 数据集身份，未执行删除')
      const state = await node.request<{ targets: Array<{ dataset: string; guid: string }> | null }>('GET', '/1.0/payincus/security')
      if (state.targets !== null && !Array.isArray(state.targets)) throw new StorageEncryptionError(502, '无法读取节点存储登记信息')
      const keys = await prisma.nodeStorageKey.findMany({ where: { hostId, OR: [{ dataset: source }, { dataset: { startsWith: source + '/' } }] }, select: { id: true, dataset: true, fingerprint: true } })
      const roots = (state.targets || []).filter(t => within(t.dataset, source)).map(t => ({ dataset: t.dataset, guid: t.guid }))
      for (const key of keys) if (!roots.some(t => t.dataset === key.dataset)) roots.push({ dataset: key.dataset, guid: '' })
      intent = { source, token: randomBytes(16).toString('hex'), roots, keys }
      await prisma.storagePool.upsert({ where: { hostId_name: { hostId, name: poolName } }, create: { hostId, name: poolName, driver: 'zfs', purpose: record?.purpose || 'instance_data', config: { ...pool.config, [marker]: intent } as any }, update: { config: { ...pool.config, [marker]: intent } as any } })
    }
    if (!intent.source || !intent.token || !Array.isArray(intent.roots) || !Array.isArray(intent.keys) || intent.roots.some(t => !within(t.dataset, intent!.source)) || intent.keys.some(k => !within(k.dataset, intent!.source))) throw new StorageEncryptionError(409, '存储删除检查点无效，未清理密钥')
    // Never retry a delete against a different backing dataset under the same Incus name.
    if (exists) {
      const current = await node.request<{ config: Record<string, string> }>('GET', path)
      if ((current.config?.['zfs.pool_name'] || current.config?.source) !== intent.source) throw new StorageEncryptionError(409, '存储池来源已变化，拒绝继续原删除操作')
      await node.request('DELETE', path)
    }
    try {
      for (const target of intent.roots) await node.request('POST', '/1.0/payincus/storage/forget', target)
      await prisma.$transaction(async tx => {
        for (const key of intent!.keys) {
          const current = await tx.nodeStorageKey.findUnique({ where: { id: key.id } })
          if (current && (current.hostId !== hostId || current.dataset !== key.dataset || current.fingerprint !== key.fingerprint)) throw new Error('key identity changed')
          if (current) await tx.nodeStorageKey.delete({ where: { id: key.id } })
        }
        const current = await tx.storagePool.findUnique({ where: { hostId_name: { hostId, name: poolName } } })
        const checkpoint = current?.config as Record<string, any> | null
        if (checkpoint?.[marker]?.token !== intent!.token) throw new Error('deletion checkpoint changed')
        await tx.storagePool.deleteMany({ where: { hostId, name: poolName } })
      })
    } catch {
      throw new StorageEncryptionError(409, '节点存储已删除，但登记或密钥清理尚未完成；再次删除可继续清理，不会重删磁盘')
    }
    return { message: '存储池已删除，对应密钥与节点登记已同步清理' }
  })
}
