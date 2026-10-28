import { nodeErrorDetail } from '../incus/incus-client.js'
import { prisma } from '../../db/prisma.js'
import { withStorageSession } from './session.js'
import { poolEncryption, unlockPool, StorageEncryptionError, type StorageNode, type StorageKeyVault, type EncryptedDataset } from './encryption.js'

export const pendingImportMarker = '_payincusImport'
interface PendingImport { source: string; encryptionRoot: string }
const validSource = (s: string) => /^[A-Za-z][A-Za-z0-9_.:-]*(\/[A-Za-z0-9_.:-]+)*$/.test(s) && !s.includes('..') && s.length <= 200

async function pendingRecord(hostId: number, poolName: string) {
  const record = await prisma.storagePool.findUnique({ where: { hostId_name: { hostId, name: poolName } } })
  const config = record?.config as Record<string, any> | null
  const pending = config?.[pendingImportMarker] as PendingImport | undefined
  if (pending && (!validSource(pending.source) || !validSource(pending.encryptionRoot))) throw new StorageEncryptionError(409, '待导入存储记录无效，请检查节点数据集身份')
  return { record, pending }
}

// Pending encrypted imports are visible in the panel before Incus can mount the locked dataset.
export async function nodeForPendingPool(node: StorageNode, hostId: number, poolName: string): Promise<StorageNode> {
  const { pending } = await pendingRecord(hostId, poolName)
  if (!pending) return node
  const path = `/1.0/storage-pools/${encodeURIComponent(poolName)}`
  return { request: async <T>(method: string, url: string, body?: unknown): Promise<T> => {
    if (method === 'GET' && url === path) {
      const paths = await node.request<string[]>('GET', '/1.0/storage-pools')
      if (!Array.isArray(paths)) throw new StorageEncryptionError(502, '无法读取节点存储列表')
      if (!paths.includes(path)) return { driver: 'zfs', config: { source: pending.source }, pendingImport: true } as T
      const actual = await node.request<{ driver: string; config: Record<string, string> }>('GET', path)
      if (actual.driver !== 'zfs' || (actual.config['zfs.pool_name'] || actual.config.source) !== pending.source) throw new StorageEncryptionError(409, '同名存储池来源已变化，拒绝绑定待导入卷')
      return { ...actual, pendingImport: true } as T
    }
    return node.request<T>(method, url, body)
  } }
}

export async function unlockAndBindPool(node: StorageNode, hostId: number, poolName: string, vault: StorageKeyVault, key?: string, automatic = false) {
  const decorated = await nodeForPendingPool(node, hostId, poolName)
  const result = await unlockPool(decorated, poolName, hostId, vault, key, { automatic })
  const { record, pending } = await pendingRecord(hostId, poolName)
  if (!pending || !record || (result.encrypted && result.nodeLocked)) return result
  try {
    const paths = await node.request<string[]>('GET', '/1.0/storage-pools')
    const path = `/1.0/storage-pools/${encodeURIComponent(poolName)}`
    if (!paths.includes(path)) await node.request('POST', '/1.0/storage-pools', { name: poolName, driver: 'zfs', config: { source: pending.source }, description: record.description || '' })
    const actual = await node.request<{ driver: string; config: Record<string, string> }>('GET', path)
    if (actual.driver !== 'zfs' || (actual.config['zfs.pool_name'] || actual.config.source) !== pending.source) throw new Error('source changed')
    await prisma.storagePool.update({ where: { id: record.id }, data: { config: actual.config } })
  } catch (error) { throw new StorageEncryptionError(409, `密钥已加载并保存，但 Incus 存储绑定未完成：${nodeErrorDetail(error, '节点未返回具体错误', { key })}；再次解密可继续绑定`) }
  return poolEncryption(node, poolName, vault)
}

export async function importEncryptedPool(hostId: number, input: { name: string; source?: string; purpose?: 'instance_data' | 'instance_storage'; description?: string }) {
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/.test(input.name) || (input.source !== undefined && !validSource(input.source))) throw new StorageEncryptionError(400, '请输入有效的存储池名和 ZFS 数据集名称')
  return withStorageSession(hostId, true, async (node, vault) => {
    const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/payincus')
    if (!info.api_extensions?.includes('payincus_storage_encryption_v1')) throw new StorageEncryptionError(409, '节点尚不支持加密卷导入，请先更新节点集成包')
    const paths = await node.request<string[]>('GET', '/1.0/storage-pools')
    const poolPath = `/1.0/storage-pools/${encodeURIComponent(input.name)}`
    let requestedSource = input.source || input.name
    if (paths.includes(poolPath)) {
      const actual = await node.request<{ driver: string; config: Record<string, string> }>('GET', poolPath)
      const source = actual.config['zfs.pool_name'] || actual.config.source
      if (actual.driver !== 'zfs' || !source || (input.source && input.source !== source)) throw new StorageEncryptionError(409, '已存在的 Incus 存储池与指定来源不一致')
      requestedSource = source
    }
    const inventory = await node.request<{ datasets: EncryptedDataset[] }>('GET', '/1.0/payincus/storage')
    if (!Array.isArray(inventory.datasets)) throw new StorageEncryptionError(502, '节点返回的加密卷信息无效')
    let item = inventory.datasets.find(d => d.dataset === requestedSource)
    if (!item) throw new StorageEncryptionError(409, '节点暂未发现此数据集，请确认外部 ZFS 池已导入且在线')
    if (item.algorithm === 'off' || item.algorithm === '-') {
      const roots = inventory.datasets.filter(d => d.dataset.startsWith(requestedSource + '/') && d.encryptionRoot === d.dataset && d.algorithm !== 'off' && d.algorithm !== '-')
      if (roots.length > 1) throw new StorageEncryptionError(400, '此池包含多个加密数据集，请在存储源中指定需要导入的数据集')
      if (!roots.length) return null
      item = roots[0]
    }
    if (!validSource(item.encryptionRoot)) throw new StorageEncryptionError(502, '节点返回的加密根无效')
    const existing = await pendingRecord(hostId, input.name)
    if (existing.record && (!existing.pending || existing.pending.source !== item.dataset)) throw new StorageEncryptionError(409, '此存储池名称已登记，请从存储列表管理')
    await prisma.storagePool.upsert({ where: { hostId_name: { hostId, name: input.name } }, create: { hostId, name: input.name, driver: 'zfs', purpose: input.purpose || 'instance_data', description: input.description || null, config: { source: item.dataset, [pendingImportMarker]: { source: item.dataset, encryptionRoot: item.encryptionRoot } } }, update: {} })
    const stored = await vault.find(item.encryptionRoot)
    const state = await node.request<{ locked: boolean }>('GET', '/1.0/payincus/security')
    if (stored && state.locked === false) {
      try {
        const ready = await unlockAndBindPool(node, hostId, input.name, vault, undefined, true)
        return { name: input.name, imported: true, pendingImport: false, message: '加密存储已导入，并使用保存的密钥解锁', ...ready }
      } catch (error) {
        return { name: input.name, imported: true, pendingImport: true, encrypted: true, source: item.dataset, message: `加密卷已添加，但自动加载或绑定未完成：${nodeErrorDetail(error, '请检查节点状态')}；请在存储页继续处理` }
      }
    }
    return { name: input.name, imported: true, pendingImport: true, encrypted: true, source: item.dataset, message: '加密卷已添加，请点击“解密磁盘”输入密钥并完成导入' }
  })
}
