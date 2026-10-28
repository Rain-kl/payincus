import { fingerprint, seal, unseal } from '../node-security/vault.js'
import { restoreStorageInstances } from './recovery.js'

export interface StorageNode {
  request<T = unknown>(method: string, path: string, body?: unknown): Promise<T>
}
export interface StoredKey { dataset: string; ciphertext: string; fingerprint: string; version: number }
export interface StorageKeyVault {
  find(dataset: string): Promise<StoredKey | null>
  save(record: StoredKey): Promise<void>
}
export interface EncryptedDataset { dataset: string; encryptionRoot: string; algorithm: string; keyStatus: 'available' | 'unavailable' | '-'; keyFormat: string }
export class StorageEncryptionError extends Error {
  constructor(readonly statusCode: number, message: string) { super(message) }
}

export async function poolEncryption(node: StorageNode, poolName: string, vault: StorageKeyVault) {
  const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/payincus')
  if (!Array.isArray(info.api_extensions) || !info.api_extensions.includes('payincus_storage_encryption_v1')) throw new StorageEncryptionError(409, '节点尚不支持存储加密管理，请先更新节点集成包')
  const pool = await node.request<{ driver: string; config: Record<string, string>; pendingImport?: boolean }>('GET', `/1.0/storage-pools/${encodeURIComponent(poolName)}`)
  if (pool.driver !== 'zfs') return { encrypted: false as const, state: 'plain' as const, dataset: '', encryptionRoot: '', keySaved: false }
  const source = pool.config?.['zfs.pool_name'] || pool.config?.source
  if (!source || source.startsWith('/')) throw new StorageEncryptionError(409, '无法确定此存储池的 ZFS 数据集，请刷新存储配置')
  const inventory = await node.request<{ locked?: boolean; datasets: EncryptedDataset[]; locks?: Record<string, { locked: boolean; complete: boolean; resumeInstances: unknown[] }> }>('GET', '/1.0/payincus/storage')
  if (!Array.isArray(inventory.datasets)) throw new StorageEncryptionError(502, '节点返回的加密状态无效')
  const item = inventory.datasets.find(d => d.dataset === source)
  if (!item) throw new StorageEncryptionError(409, 'ZFS 数据集暂不可见，可能未导入或已离线，请检查节点状态')
  if (item.algorithm === 'off' || item.algorithm === '-') return { encrypted: false as const, state: 'plain' as const, dataset: source, encryptionRoot: '', keySaved: false }
  if (!item.encryptionRoot || !['available', 'unavailable'].includes(item.keyStatus) || !item.algorithm?.startsWith('aes-')) throw new StorageEncryptionError(502, '节点返回的加密状态无效')
  const stored = await vault.find(item.encryptionRoot)
  const lock = inventory.locks?.[item.encryptionRoot]
  return { nodeLocked: inventory.locked === true, pendingImport: !!pool.pendingImport, manualLocked: lock?.locked === true, lockPending: lock?.locked === true && lock.complete !== true, pendingRecovery: Array.isArray(lock?.resumeInstances) ? lock.resumeInstances.length : 0, encrypted: true as const, state: item.keyStatus === 'available' ? 'unlocked' as const : 'locked' as const, dataset: source, encryptionRoot: item.encryptionRoot, keySaved: !!stored, keyFormat: item.keyFormat }
}

// Validate against the actual node encryption root before accepting custody; never echo keys in errors.
export async function unlockPool(node: StorageNode, poolName: string, hostId: number, vault: StorageKeyVault, suppliedKey?: string, options: { automatic?: boolean } = {}) {
  const state = await poolEncryption(node, poolName, vault)
  if (!state.encrypted) throw new StorageEncryptionError(409, '此存储池没有 ZFS 加密，不能执行解锁')
  if (options.automatic && state.manualLocked) throw new StorageEncryptionError(423, '存储已手动锁盘，必须手动解密')
  if (state.keyFormat !== 'hex') throw new StorageEncryptionError(409, '此加密卷使用的密钥格式暂不支持，请先在节点转换为 hex 密钥格式')
  const existing = await vault.find(state.encryptionRoot)
  let key = suppliedKey
  if (!key) {
    if (!existing) throw new StorageEncryptionError(409, '请先输入此加密卷的密钥并保存')
    try { key = unseal(existing.ciphertext, { hostId, dataset: existing.dataset, version: existing.version }) }
    catch { throw new StorageEncryptionError(503, '已保存密钥无法读取，请检查控制端主密钥配置') }
  }
  if (!/^[0-9a-f]{64}$/.test(key)) throw new StorageEncryptionError(400, '请输入 64 位十六进制 ZFS 密钥')
  const fp = fingerprint(key)
  if (existing && existing.fingerprint !== fp) throw new StorageEncryptionError(409, '已有不同的托管密钥，不能直接覆盖，请先核对存储卷身份')
  let ciphertext: string
  try { ciphertext = seal(key, { hostId, dataset: state.encryptionRoot, version: existing?.version || 1 }) }
  catch { throw new StorageEncryptionError(503, '控制端密钥托管未就绪，请检查主密钥配置') }
  const nodeState = await node.request<{ locked: boolean; targets: Array<{ dataset: string; fingerprint: string }> | null }>('GET', '/1.0/payincus/security')
  if (typeof nodeState.locked !== 'boolean' || (nodeState.targets !== null && !Array.isArray(nodeState.targets))) throw new StorageEncryptionError(502, '无法核验节点存储注册状态')
  const registered = nodeState.targets?.find(t => t.dataset === state.encryptionRoot)
  if (registered && registered.fingerprint !== fp) throw new StorageEncryptionError(409, '密钥与节点登记的加密卷不匹配')
  if (!registered && nodeState.locked) throw new StorageEncryptionError(423, '节点安全锁定中，不能登记新的加密卷')
  await node.request('POST', `/1.0/payincus/storage/${registered ? state.manualLocked ? 'unlock-pool' : options.automatic ? 'auto-unlock' : 'unlock' : 'register'}`, { dataset: state.encryptionRoot, key })
  try { await vault.save({ dataset: state.encryptionRoot, fingerprint: fp, ciphertext, version: existing?.version || 1 }) }
  catch { throw new StorageEncryptionError(503, '节点已加载密钥，但控制端保存失败，请保留密钥后重试') }
  key = ''
  if (!options.automatic || state.pendingRecovery > 0) await restoreStorageInstances(node, state.encryptionRoot, hostId)
  const verified = await poolEncryption(node, poolName, vault)
  if (!verified.encrypted || verified.state !== 'unlocked' || !verified.keySaved) throw new StorageEncryptionError(409, '节点尚未确认解锁完成，请刷新状态后重试')
  return verified
}
