import { storedPoolEncryption } from '../lib/storage/read-model.js'
import { syncHostStorageBaseline } from '../services/storage-baseline-sync.js'
import { IncusApiError, nodeErrorDetail } from '../lib/incus/incus-client.js'
import { unlockAndBindPool } from '../lib/storage/import.js'
import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { lockPool } from '../lib/storage/lock.js'
import { withStorageSession } from '../lib/storage/session.js'
import { StorageEncryptionError } from '../lib/storage/encryption.js'
import { unseal } from '../lib/node-security/vault.js'
import { createLog, LogModule, LogResult } from '../db/logs.js'

const params = { type: 'object', additionalProperties: false, required: ['id', 'poolName'], properties: {
  id: { type: 'string', pattern: '^[1-9][0-9]{0,9}$' }, poolName: { type: 'string', pattern: '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$' }
} }
export default async function storageEncryptionRoutes(app: FastifyInstance) {
  const url = '/api/hosts/:id/storage-pools/:poolName/encryption'
  for (const method of ['GET', 'POST', 'PUT'] as const) {
    app.route<{ Params: { id: string; poolName: string }; Body: { key?: string } }>({
      method, url, onRequest: [app.authenticate],
      schema: { params, ...(method !== 'GET' ? { body: { type: 'object', additionalProperties: false, properties: { key: { type: 'string', pattern: '^[0-9a-f]{64}$' } } } } : {}) },
      handler: async (req, reply) => {
        reply.header('Cache-Control', 'no-store').header('Pragma', 'no-cache')
        const hostId = Number(req.params.id)
        if (!Number.isSafeInteger(hostId) || hostId > 2147483647) return reply.code(400).send({ error: '节点 ID 无效' })
        const host = await prisma.host.findUnique({ where: { id: hostId } })
        if (!host) return reply.code(404).send({ error: '节点不存在' })
        if (req.user.role !== 'admin' && req.user.id !== host.userId) return reply.code(403).send({ error: '无权管理此节点存储' })
        try {
          if (method === 'GET') return await storedPoolEncryption(hostId, req.params.poolName)
          let syncError: unknown = null
          const result = await withStorageSession(hostId, true, async (node, vault) => {
            const res = method === 'PUT'
              ? await lockPool(node, req.params.poolName, vault, req.user.id, hostId)
              : await unlockAndBindPool(node, hostId, req.params.poolName, vault, req.body?.key)
            if (method === 'POST') {
              try {
                await syncHostStorageBaseline(hostId, node, { skipPoolReload: true })
              } catch (err) {
                syncError = err
              }
            }
            return res
          })
          // 解锁操作已在节点完成；通过同一 mTLS 连接立即同步存储基线，无需再等 5 秒心跳。
          if (method === 'POST') {
            if (syncError) {
              req.log.warn({ hostId, error: syncError }, 'Storage unlock completed but baseline confirmation failed')
              return reply.code(504).send({ error: '节点已加载密钥，但未能同步确认最新存储基线；请点击「基线同步」后刷新' })
            }
            const confirmed = await storedPoolEncryption(hostId, req.params.poolName)
            if (confirmed?.encrypted && confirmed.state !== 'unlocked') {
              return reply.code(409).send({ error: `节点已加载密钥，但存储状态仍为 ${confirmed.state}；请刷新后重试或检查节点日志`, detail: confirmed })
            }
            return confirmed
          }
          return result
        } catch (error) {
          if (error instanceof StorageEncryptionError) return reply.code(error.statusCode).send({ error: error.message })
          const status = error instanceof IncusApiError ? error.statusCode === 401 ? 502 : error.statusCode : 409
          return reply.code(status).send({ error: nodeErrorDetail(error, '节点存储操作未完成，请检查连接与状态', req.body) })
        }
      }
    })
  }

  // Reveal the control-plane custody copy so an operator can unlock this pool
  // from the node TUI when the panel is unreachable. Every reveal is audited
  // before the key is returned, mirroring the node-security reveal contract.
  app.post<{ Params: { id: string; poolName: string } }>(`${url}/reveal`, {
    onRequest: [app.authenticate],
    schema: { params }
  }, async (req, reply) => {
    reply.header('Cache-Control', 'no-store').header('Pragma', 'no-cache')
    const hostId = Number(req.params.id)
    if (!Number.isSafeInteger(hostId) || hostId > 2147483647) return reply.code(400).send({ error: '节点 ID 无效' })
    const host = await prisma.host.findUnique({ where: { id: hostId } })
    if (!host) return reply.code(404).send({ error: '节点不存在' })
    if (req.user.role !== 'admin' && req.user.id !== host.userId) return reply.code(403).send({ error: '无权管理此节点存储' })

    const pool = await prisma.storagePool.findUnique({ where: { hostId_name: { hostId, name: req.params.poolName } } })
    if (!pool) return reply.code(404).send({ error: '存储池不存在' })
    if (pool.driver !== 'zfs') return reply.code(409).send({ error: '此存储池未加密，没有可领取的密钥' })
    const encryption = (pool.telemetry as { encryption?: { encryptionRoot?: string } } | null)?.encryption
    const dataset = encryption?.encryptionRoot
    if (!dataset) return reply.code(409).send({ error: '尚未收到此存储池的加密状态，请等待节点心跳上报后重试' })

    const record = await prisma.nodeStorageKey.findUnique({ where: { hostId_dataset: { hostId, dataset } } })
    if (!record) return reply.code(404).send({ error: '此存储池的密钥尚未托管，无法领取' })
    let key: string
    try { key = unseal(record.ciphertext, { hostId, dataset: record.dataset, version: record.version }) }
    catch { return reply.code(409).send({ error: '密钥校验失败，请检查控制端主密钥配置' }) }
    try {
      await createLog(req.user.id, LogModule.SECURITY, 'storage.key.reveal', `节点 #${hostId} 管理员领取存储池 ${req.params.poolName} 密钥`, LogResult.SUCCESS)
    } catch { key = ''; return reply.code(503).send({ error: '审计记录未能保存，未提供密钥' }) }
    return { key, fingerprint: record.fingerprint, dataset: record.dataset }
  })
}
