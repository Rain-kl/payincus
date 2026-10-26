import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { IncusClient } from '../lib/incus/incus-client.js'
import { createLog, LogModule, LogResult } from '../db/logs.js'
import { datasetPattern, fingerprint, seal, unseal } from '../lib/node-security/vault.js'

const params = { type: 'object', required: ['hostId'], properties: { hostId: { type: 'integer', minimum: 1, maximum: 2147483647 } } }
const revision = { type: 'integer', minimum: 0, maximum: Number.MAX_SAFE_INTEGER }
const dataset = { type: 'string', minLength: 3, maxLength: 200, pattern: datasetPattern.source }
const noStore = (reply: any) => reply.header('Cache-Control', 'no-store').header('Pragma', 'no-cache')
export default async function nodeSecurityRoutes(app: FastifyInstance) {
  const auth = [app.authenticate, app.requireAdmin]
  const base = '/api/admin/hosts/:hostId/node-security'
  async function client(hostId: number) {
    const host = await prisma.host.findUnique({ where: { id: hostId } })
    if (!host) throw new Error('节点不存在')
    const identity = await prisma.nodeSecurityState.findUnique({ where: { hostId } })
    if (!identity?.serverFingerprint) throw new Error('请先核对并保存节点证书指纹')
    return {
      request: async <T = unknown>(method: string, path: string, body?: unknown): Promise<T> => {
        const node = new IncusClient({ url: host.url, certPath: host.certPath, keyPath: host.keyPath, hostId, tunnelEnabled: host.tunnelEnabled, targetHost: host.targetHost, targetPort: host.targetPort, serverFingerprint: identity.serverFingerprint! })
        try {
          await node.connect()
          const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/payincus')
          if (!Array.isArray(info?.api_extensions) || !info.api_extensions.includes('payincus_security_v1')) throw new Error('节点不支持此功能，请升级集成包')
          return await node.request<T>(method, path, body)
        } finally { await node.close() }
      }
    }
  }
  app.post<{ Params: { hostId: number }; Body: { fingerprint: string } }>(`${base}/identity`, {
    onRequest: auth, schema: { params, body: { type: 'object', additionalProperties: false, required: ['fingerprint'], properties: { fingerprint: { type: 'string', pattern: '^[0-9a-f]{64}$' } } } }
  }, async (req, reply) => {
    noStore(reply)
    const hostId = req.params.hostId
    if (!await prisma.host.findUnique({ where: { id: hostId }, select: { id: true } })) return reply.code(404).send({ error: '节点不存在' })
    await prisma.nodeSecurityState.upsert({ where: { hostId }, create: { hostId, report: {}, serverFingerprint: req.body.fingerprint }, update: { serverFingerprint: req.body.fingerprint } })
    await createLog(req.user.id, LogModule.SECURITY, 'node.identity', `节点 #${hostId} 更新人工核对的服务端证书指纹`, LogResult.SUCCESS)
    return { fingerprint: req.body.fingerprint }
  })
  app.get<{ Params: { hostId: number } }>(base, { onRequest: auth, schema: { params } }, async (req, reply) => {
    noStore(reply)
    const hostId = req.params.hostId
    const [keys, events, reported] = await Promise.all([
      prisma.nodeStorageKey.findMany({ where: { hostId }, select: { id: true, dataset: true, fingerprint: true, version: true, createdAt: true } }),
      prisma.nodeCliEvent.findMany({ where: { hostId }, orderBy: { receivedAt: 'desc' }, take: 100 }),
      prisma.nodeSecurityState.findUnique({ where: { hostId } })
    ])
    const identity = { serverFingerprint: reported?.serverFingerprint ?? null }
    try { return { ...identity, state: await (await client(hostId)).request('GET', '/1.0/payincus/security'), keys, events: events.map(e => ({ ...e, uid: Number(e.uid) })), reportedAt: reported?.reportedAt } }
    catch { return { ...identity, state: reported?.report && typeof reported.report === 'object' && 'locked' in reported.report ? reported.report : null, keys, events: events.map(e => ({ ...e, uid: Number(e.uid) })), reportedAt: reported?.reportedAt, offline: true } }
  })
  for (const action of ['policy', 'recover', 'lock'] as const) {
    const properties = action === 'policy' ? { enabled: { type: 'boolean' }, revision } : action === 'recover' ? { revision } : {}
    app.post<{ Params: { hostId: number }; Body: { enabled?: boolean; revision?: number } }>(`${base}/${action}`, {
      onRequest: auth, schema: { params, body: { type: 'object', additionalProperties: false, required: Object.keys(properties), properties } }
    }, async (req, reply) => {
      noStore(reply)
      try {
        const state = await (await client(req.params.hostId)).request('POST', `/1.0/payincus/security/${action}`, req.body)
        await createLog(req.user.id, LogModule.SECURITY, `node.${action}`, `节点 #${req.params.hostId} 管理操作 ${action}`, LogResult.SUCCESS)
        return { state }
      } catch { return reply.code(409).send({ error: '节点操作未完成，请刷新状态并检查连接或存储' }) }
    })
  }
  app.post<{ Params: { hostId: number }; Body: { dataset: string; key: string } }>(`${base}/keys`, {
    onRequest: auth, schema: { params, body: { type: 'object', additionalProperties: false, required: ['dataset', 'key'], properties: { dataset, key: { type: 'string', pattern: '^[0-9a-f]{64}$' } } } }
  }, async (req, reply) => {
    noStore(reply)
    const hostId = req.params.hostId
    if (!await prisma.host.findUnique({ where: { id: hostId }, select: { id: true } })) return reply.code(404).send({ error: '节点不存在' })
    const { dataset, key } = req.body
    const identity = { hostId, dataset, version: 1 }
    const keyFingerprint = fingerprint(key)
    let ciphertext: string
    try { ciphertext = seal(key, identity) } catch { return reply.code(503).send({ error: '控制端密钥托管未就绪，请检查主密钥配置' }) }
    // Never overwrite the sole active key through an enrollment request.
    const existing = await prisma.nodeStorageKey.findUnique({ where: { hostId_dataset: { hostId, dataset } } })
    if (existing) {
      if (existing.fingerprint !== keyFingerprint) return reply.code(409).send({ error: '已有不同密钥，不能直接覆盖' })
      try { if (unseal(existing.ciphertext, { hostId, dataset, version: existing.version }) !== key) throw new Error('mismatch') }
      catch { return reply.code(409).send({ error: '已有密钥无法校验，请先检查控制端托管配置' }) }
      return { id: existing.id, dataset, fingerprint: existing.fingerprint, version: existing.version }
    }
    const saved = await prisma.nodeStorageKey.create({ data: { hostId, dataset, fingerprint: keyFingerprint, ciphertext }, select: { id: true, dataset: true, fingerprint: true, version: true } })
    await createLog(req.user.id, LogModule.SECURITY, 'node.key.enroll', `节点 #${hostId} 保存存储密钥`, LogResult.SUCCESS)
    return saved
  })
  for (const action of ['reveal', 'unlock'] as const) {
    app.post<{ Params: { hostId: number }; Body: { dataset: string } }>(`${base}/keys/${action}`, {
      onRequest: auth, schema: { params, body: { type: 'object', additionalProperties: false, required: ['dataset'], properties: { dataset } } }
    }, async (req, reply) => {
      noStore(reply)
      const { hostId } = req.params
      const record = await prisma.nodeStorageKey.findUnique({ where: { hostId_dataset: { hostId, dataset: req.body.dataset } } })
      if (!record) return reply.code(404).send({ error: '密钥尚未托管' })
      let key: string
      try { key = unseal(record.ciphertext, { hostId, dataset: record.dataset, version: record.version }) } catch { return reply.code(409).send({ error: '密钥校验失败，未执行解锁' }) }
      try {
        await prisma.log.create({ data: { userId: req.user.id, module: LogModule.SECURITY, action: `node.key.${action}.request`, content: `节点 #${hostId} 管理员申请${action === 'reveal' ? '领取' : '加载'}存储密钥`, result: LogResult.SUCCESS } })
      } catch { return reply.code(503).send({ error: '审计记录未能保存，未提供密钥' }) }
      if (action === 'reveal') return { key, fingerprint: record.fingerprint }
      try {
        const state = await (await client(hostId)).request('POST', '/1.0/payincus/security/unlock', { dataset: record.dataset, key })
        return { state }
      } catch { return reply.code(409).send({ error: '存储解锁未完成，节点锁定状态未解除' }) }
      finally { key = '' }
    })
  }
}
