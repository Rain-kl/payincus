import type { StorageNode } from './encryption.js'
import { StorageEncryptionError } from './encryption.js'

// Encrypted file pools use an external dataset source, not Incus's native loop-file source.
export async function resizeStoragePool(node: StorageNode, name: string, size: string) {
  const path = `/1.0/storage-pools/${encodeURIComponent(name)}`
  const pool = await node.request<{ driver: string; config: Record<string, string> }>('GET', path)
  const source = pool.config['zfs.pool_name'] || pool.config.source
  if (pool.driver === 'zfs' && source?.endsWith('/incus') && !pool.config.size) {
    const match = /^([1-9][0-9]*)(GiB|TiB)$/.exec(size)
    const sizeGiB = match ? Number(match[1]) * (match[2] === 'TiB' ? 1024 : 1) : 0
    if (!Number.isSafeInteger(sizeGiB) || sizeGiB < 1 || sizeGiB > 65536) throw new StorageEncryptionError(400, '文件池容量必须为 1–65536 GiB，请使用 GiB 或 TiB')
    const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/payincus')
    if (!info.api_extensions?.includes('payincus_storage_pool_resize_v1')) throw new StorageEncryptionError(409, '节点尚不支持加密 Loop 存储扩容，请先更新节点集成包')
    await node.request('POST', '/1.0/payincus/storage/resize', { dataset: source, sizeGiB })
  } else {
    await node.request('PATCH', path, { config: { size } })
  }
}
