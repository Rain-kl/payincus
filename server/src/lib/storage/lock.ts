import { fingerprint, unseal } from '../node-security/vault.js'
import { prisma } from '../../db/prisma.js'
import { poolEncryption, StorageEncryptionError, type StorageNode, type StorageKeyVault } from './encryption.js'

export async function lockPool(node: StorageNode, poolName: string, vault: StorageKeyVault, actorId: number, hostId: number) {
  const state = await poolEncryption(node, poolName, vault)
  if (!state.encrypted) throw new StorageEncryptionError(409, '此磁盘未配置 ZFS 加密，不能卸载密钥锁盘')
  if (!state.keySaved) throw new StorageEncryptionError(409, '请先保存并校验解锁密钥，再执行锁盘')
  const stored = await vault.find(state.encryptionRoot)
  try {
    if (!stored || fingerprint(unseal(stored.ciphertext, { hostId, dataset: stored.dataset, version: stored.version })) !== stored.fingerprint) throw new Error('invalid custody')
  } catch { throw new StorageEncryptionError(503, '托管密钥暂不可用，未停止实例或锁盘，请先检查控制端主密钥配置') }
  const registration = await node.request<{ targets: Array<{ dataset: string; fingerprint: string }> | null }>('GET', '/1.0/payincus/security')
  if (!registration.targets?.some(target => target.dataset === state.encryptionRoot && target.fingerprint === stored!.fingerprint)) throw new StorageEncryptionError(409, '托管密钥与节点加密卷登记不一致，未停止实例或锁盘')
  // Audit before any stop/unload; a failed audit must not interrupt workloads.
  await prisma.log.create({ data: { userId: actorId, module: 'security', action: 'storage.lock.request', content: `节点 #${hostId} 管理员锁定存储池 ${poolName}`, result: 'success' } })
  await node.request('POST', '/1.0/payincus/storage/lock-pool', { dataset: state.encryptionRoot })
  const verified = await poolEncryption(node, poolName, vault)
  if (!verified.encrypted || verified.state !== 'locked' || !verified.manualLocked || verified.lockPending) throw new StorageEncryptionError(409, '锁盘未完成，请检查相关实例和存储占用后重试')
  return verified
}
