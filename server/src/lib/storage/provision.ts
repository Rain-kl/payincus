import { IncusApiError, nodeErrorDetail } from '../incus/incus-client.js'
import { randomBytes } from 'node:crypto'
import { prisma } from '../../db/prisma.js'
import { fingerprint, seal, unseal } from '../node-security/vault.js'
import { withStorageSession } from './session.js'
import { StorageEncryptionError, poolEncryption } from './encryption.js'

export interface EncryptedPoolOptions {
  name: string; source?: string; size?: string; useLoop?: boolean; zfsPoolName?: string
  purpose?: 'instance_data' | 'instance_storage'; description?: string
}

export async function provisionEncryptedPool(hostId: number, input: EncryptedPoolOptions) {
  const poolName = input.zfsPoolName?.trim() || input.name
  if (!/^[A-Za-z][A-Za-z0-9_-]{0,39}$/.test(poolName) || !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/.test(input.name)) throw new StorageEncryptionError(400, '存储池名称格式不正确')
  const device = input.source?.trim() || ''
  const size = input.size?.trim() || ''
  if (!!device === !!size || (input.useLoop && device)) throw new StorageEncryptionError(400, '请选择一项：专用空磁盘或文件池容量')
  let sizeGiB = 0
  if (device && (!device.startsWith('/dev/') || device.includes('..'))) throw new StorageEncryptionError(400, '创建 ZFS 需要 /dev 下的专用空磁盘；已有存储请使用导入')
  if (size) {
    const match = /^([1-9][0-9]*)(GiB|TiB)$/.exec(size)
    if (!match) throw new StorageEncryptionError(400, '文件池容量请使用 GiB 或 TiB')
    sizeGiB = Number(match[1]) * (match[2] === 'TiB' ? 1024 : 1)
    if (!Number.isSafeInteger(sizeGiB) || sizeGiB < 1 || sizeGiB > 65536) throw new StorageEncryptionError(400, '文件池容量必须为 1–65536 GiB')
  }
  return withStorageSession(hostId, true, async (node, vault) => {
    const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/payincus')
    if (!Array.isArray(info.api_extensions) || !info.api_extensions.includes('payincus_storage_encryption_v1')) throw new StorageEncryptionError(409, '节点尚不支持自动加密存储创建，请先更新节点集成包')
    const security = await node.request<{ locked: boolean }>('GET', '/1.0/payincus/security')
    if (security.locked !== false) throw new StorageEncryptionError(423, '节点安全锁定中，不能创建存储池')
    const dataset = `${poolName}/incus`
    const existing = await vault.find(dataset)
    let key: string
    try { key = existing ? unseal(existing.ciphertext, { hostId, dataset, version: existing.version }) : randomBytes(32).toString('hex') }
    catch { throw new StorageEncryptionError(503, '已有初始化密钥无法读取，请检查控制端主密钥配置') }
    const fp = fingerprint(key)
    let ciphertext: string
    try { ciphertext = seal(key, { hostId, dataset, version: existing?.version || 1 }) }
    catch { throw new StorageEncryptionError(503, '控制端密钥托管未就绪，未创建存储池') }
    // Persist before the first node write; every retry reuses the same key and node identity marker.
    await vault.save({ dataset, fingerprint: fp, ciphertext, version: existing?.version || 1 })
    try {
      const paths = await node.request<string[]>('GET', '/1.0/storage-pools')
      if (!Array.isArray(paths)) throw new Error('invalid storage pool list')
      const poolPath = `/1.0/storage-pools/${encodeURIComponent(input.name)}`
      if (!paths.includes(poolPath)) {
        const state = await node.request<{ targets: Array<{ dataset: string; fingerprint: string }> }>('POST', '/1.0/payincus/storage/pool', { name: poolName, device, sizeGiB, key })
        if (!state.targets?.some(t => t.dataset === dataset && t.fingerprint === fp)) throw new Error('node did not confirm dataset identity')
        await node.request('POST', '/1.0/storage-pools', { name: input.name, driver: 'zfs', config: { source: dataset }, description: input.description || '' })
      }
      key = ''
      const state = await poolEncryption(node, input.name, vault)
      if (!state.encrypted || state.encryptionRoot !== dataset || state.state !== 'unlocked') throw new Error('storage identity changed')
      await prisma.storagePool.upsert({ where: { hostId_name: { hostId, name: input.name } }, create: { hostId, name: input.name, driver: 'zfs', purpose: input.purpose || 'instance_data', description: input.description || null, config: { source: dataset } }, update: {} })
      return { name: input.name, encrypted: true, source: dataset, keySaved: true, message: '加密 ZFS 存储池已创建，密钥已自动保存' }
    } catch (error) {
      const detail = nodeErrorDetail(error, '节点未返回具体错误', { key })
      key = ''
      throw new StorageEncryptionError(error instanceof IncusApiError && error.statusCode !== 401 ? error.statusCode : 409, `加密存储初始化未全部完成：${detail}；密钥已保留，使用相同名称和参数重试可继续，请勿删除已有数据`)
    }
  })
}
