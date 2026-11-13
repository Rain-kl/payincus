import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { IncusClient, IncusApiError, nodeErrorDetail } from '../lib/incus/incus-client.js'
import { LogModule, LogResult } from '../db/logs.js'

const endpoint = '/1.0/agent/instance-backups'
const extension = 'incus_instance_backup_v1'
const params = { type: 'object', required: ['hostId'], properties: { hostId: { type: 'integer', minimum: 1, maximum: 2147483647 } } }

// Only submit and inspect node-owned jobs; exports never occupy a panel request.
export default async function hostInstanceBackupRoutes(app: FastifyInstance) {
  const auth = [app.authenticate, app.requireAdmin]
  const base = '/api/admin/hosts/:hostId/instance-backups'
  for (const method of ['GET', 'POST'] as const) {
    app.route<{ Params: { hostId: number }; Body: { id: string; directory: string } }>({
      method, url: base, onRequest: auth,
      schema: { params, ...(method === 'POST' ? { body: {
        type: 'object', additionalProperties: false, required: ['id', 'directory'], properties: {
          id: { type: 'string', pattern: '^[0-9a-f]{32}$' },
          directory: { type: 'string', minLength: 2, maxLength: 4096, pattern: '^/[^\\u0000\\r\\n]+$' }
        }
      } } : {}) },
      handler: async (req, reply) => {
        reply.header('Cache-Control', 'no-store')
        const hostId = req.params.hostId
        const host = await prisma.host.findUnique({ where: { id: hostId } })
        if (!host) return reply.code(404).send({ error: '节点不存在' })
        const identity = await prisma.nodeSecurityState.findUnique({ where: { hostId } })
        if (!identity?.serverFingerprint) return reply.code(409).send({ error: '请先核对并保存节点证书指纹' })
        const node = new IncusClient({ url: host.url, certPath: host.certPath, keyPath: host.keyPath, hostId, tunnelEnabled: host.tunnelEnabled, targetHost: host.targetHost, targetPort: host.targetPort, serverFingerprint: identity.serverFingerprint })
        try {
          await node.connect()
          const info = await node.request<{ api_extensions: string[] }>('GET', '/1.0/agent')
          if (!info.api_extensions?.includes(extension)) return reply.code(409).send({ error: '节点尚不支持实例备份，请升级节点集成包' })
          if (method === 'POST') await prisma.log.create({ data: { userId: req.user.id, module: LogModule.HOST, action: 'host.backup.start', content: `节点 #${hostId} 管理员提交实例备份任务 ${req.body.id}`, result: LogResult.SUCCESS } })
          return await node.request(method, endpoint, method === 'POST' ? req.body : undefined)
        } catch (error) {
          const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
          return reply.code(status).send({ error: nodeErrorDetail(error, '节点备份请求失败') })
        } finally { await node.close() }
      }
    })
  }
}
