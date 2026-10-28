import { IncusApiError, nodeErrorDetail } from '../lib/incus/incus-client.js'
import { nodeForPendingPool, unlockAndBindPool } from '../lib/storage/import.js'
import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { lockPool } from '../lib/storage/lock.js'
import { withStorageSession } from '../lib/storage/session.js'
import { poolEncryption, StorageEncryptionError } from '../lib/storage/encryption.js'

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
          return await withStorageSession(hostId, method !== 'GET', async (node, vault) => method === 'GET'
            ? poolEncryption(await nodeForPendingPool(node, hostId, req.params.poolName), req.params.poolName, vault)
            : method === 'PUT' ? lockPool(node, req.params.poolName, vault, req.user.id, hostId)
            : unlockAndBindPool(node, hostId, req.params.poolName, vault, req.body?.key))
        } catch (error) {
          if (error instanceof StorageEncryptionError) return reply.code(error.statusCode).send({ error: error.message })
          const status = error instanceof IncusApiError ? error.statusCode === 401 ? 502 : error.statusCode : 409
          return reply.code(status).send({ error: nodeErrorDetail(error, '节点存储操作未完成，请检查连接与状态', req.body) })
        }
      }
    })
  }
}
