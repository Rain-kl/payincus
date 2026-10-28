import { invalidateHostStorageCapacity } from '../storage-capacity.js'
import { prisma } from '../../db/prisma.js'
import { IncusClient } from '../incus/incus-client.js'
import { StorageEncryptionError, type StorageKeyVault } from './encryption.js'

const busy = new Set<number>()
export async function withStorageSession<T>(hostId: number, write: boolean, fn: (node: IncusClient, vault: StorageKeyVault) => Promise<T>): Promise<T> {
  if (write && busy.has(hostId)) throw new StorageEncryptionError(409, '节点正在执行存储操作，请稍后重试')
  if (write) { busy.add(hostId); invalidateHostStorageCapacity(hostId) }
  let node: IncusClient | undefined
  try {
    const host = await prisma.host.findUnique({ where: { id: hostId } })
    if (!host) throw new StorageEncryptionError(404, '节点不存在')
    const identity = await prisma.nodeSecurityState.findUnique({ where: { hostId } })
    if (!identity?.serverFingerprint) throw new StorageEncryptionError(412, '节点身份尚未核验，不能传输存储密钥')
    node = new IncusClient({ url: host.url, certPath: host.certPath, keyPath: host.keyPath, hostId, tunnelEnabled: host.tunnelEnabled, targetHost: host.targetHost, targetPort: host.targetPort, serverFingerprint: identity.serverFingerprint })
    const vault: StorageKeyVault = {
      find: dataset => prisma.nodeStorageKey.findUnique({ where: { hostId_dataset: { hostId, dataset } } }),
      save: async record => {
        const stored = await prisma.nodeStorageKey.upsert({ where: { hostId_dataset: { hostId, dataset: record.dataset } }, create: { hostId, ...record }, update: {} })
        if (stored.fingerprint !== record.fingerprint) throw new StorageEncryptionError(409, '已有不同密钥，不能直接覆盖')
        const updated = await prisma.nodeStorageKey.updateMany({ where: { hostId, dataset: record.dataset, fingerprint: record.fingerprint, version: record.version }, data: { ciphertext: record.ciphertext } })
        if (updated.count !== 1) throw new StorageEncryptionError(409, '存储密钥状态已变化，请刷新后重试')
      }
    }
    await node.connect()
    return await fn(node, vault)
  } finally {
    try { await node?.close() } finally { if (write) { busy.delete(hostId); invalidateHostStorageCapacity(hostId) } }
  }
}
