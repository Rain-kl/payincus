import { prisma } from '../../db/prisma.js'
import { restoreNodeInstances, type RecoveryState, type ResumeInstance } from '../node-security/recovery.js'
import { StorageEncryptionError, type StorageNode } from './encryption.js'

export async function restoreStorageInstances(node: StorageNode, dataset: string, hostId: number) {
  const state = await node.request<{ locked: boolean; revision: number; storageLocks?: Record<string, { locked: boolean; resumeInstances: ResumeInstance[] }> }>('GET', '/1.0/payincus/security')
  const lock = state.storageLocks?.[dataset]
  if (!lock?.resumeInstances?.length) return
  if (state.locked || lock.locked) throw new StorageEncryptionError(423, '存储密钥已加载，但节点或存储仍被锁定，实例尚未恢复')
  const adapter: StorageNode = {
    request: async <T>(method: string, path: string, body?: any): Promise<T> => {
      if (path === '/1.0/payincus/security/resume') return node.request<T>('POST', '/1.0/payincus/storage/resume', { ...body, dataset })
      const value = await node.request<T>(method, path, body)
      if (method === 'GET' && /^\/1\.0\/instances\/[^/]+\?/.test(path)) {
        const instance = value as { expanded_devices?: Record<string, Record<string, string>> }
        let related = false
        for (const device of Object.values(instance.expanded_devices || {})) {
          if (device.type !== 'disk' || !device.pool) continue
          const pool = await node.request<{ config: Record<string, string> }>('GET', `/1.0/storage-pools/${encodeURIComponent(device.pool)}`)
          const source = pool.config?.['zfs.pool_name'] || pool.config?.source
          if (source === dataset || source?.startsWith(dataset + '/')) related = true
        }
        if (!related) throw new Error('workload no longer uses the locked storage')
      }
      return value
    }
  }
  const initial: RecoveryState = { locked: false, revision: state.revision, resumeInstances: lock.resumeInstances }
  const result = await restoreNodeInstances(adapter, initial, async item => {
    if (item.project !== 'default') return true
    const instance = await prisma.instance.findFirst({ where: { hostId, incusId: item.name }, select: { status: true, expiresAt: true } })
    return !instance || (['running', 'stopped', 'error'].includes(instance.status) && (!instance.expiresAt || instance.expiresAt.getTime() > Date.now()))
  }, { types: ['container', 'virtual-machine'] })
  if (result.failed) throw new StorageEncryptionError(409, `磁盘已解锁，但 ${result.failed} 个相关实例尚未恢复；再次解密可继续恢复`)
}
