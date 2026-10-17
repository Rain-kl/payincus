import type { OutgoingHttpHeaders } from 'node:http'
import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { operationTaskControl, redactTaskLog, stopOperationTask } from '../lib/operation-task-log.js'

const taskIdSchema = { type: 'object', required: ['id'], properties: { id: { type: 'integer', minimum: 1, maximum: 2147483647 } } }
const connections = new Map<number, number>()

export default async function adminTaskRoutes(app: FastifyInstance): Promise<void> {
  const auth = [app.authenticate, app.requireAdmin]
  app.get<{ Querystring: { page?: number; status?: string; kind?: string; search?: string } }>('/api/admin/tasks', {
    onRequest: auth,
    schema: { querystring: { type: 'object', properties: {
      page: { type: 'integer', minimum: 1, maximum: 100000, default: 1 },
      status: { type: 'string', enum: ['running', 'success', 'failed', 'cancelled', 'interrupted'] },
      kind: { type: 'string', enum: ['create', 'destroy'] }, search: { type: 'string', maxLength: 100 }
    } } }
  }, async request => {
    const { page = 1, status, kind, search } = request.query
    const where = { ...(status ? { status } : {}), ...(kind ? { kind } : {}), ...(search ? { instanceName: { contains: search } } : {}) }
    const [items, total] = await Promise.all([
      prisma.operationTask.findMany({ where, orderBy: { id: 'desc' }, take: 20, skip: (page - 1) * 20 }),
      prisma.operationTask.count({ where })
    ])
    const hosts = await prisma.host.findMany({ where: { id: { in: [...new Set(items.map(item => item.hostId))] } }, select: { id: true, name: true } })
    return { items: items.map(item => ({ ...item, ...operationTaskControl(item.id), hostName: hosts.find(host => host.id === item.hostId)?.name || null })), total }
  })

  app.post<{ Params: { id: number } }>('/api/admin/tasks/:id/stop', {
    onRequest: auth,
    config: { rateLimit: { max: 10, timeWindow: '1 minute' } },
    schema: { params: taskIdSchema }
  }, async (request, reply) => {
    const task = await prisma.operationTask.findUnique({ where: { id: request.params.id } })
    if (!task) return reply.code(404).send({ error: '任务不存在' })
    if (task.status !== 'running') return reply.code(409).send({ error: '任务已结束' })
    try {
      await stopOperationTask(task.id)
      return { message: '取消信号已发送，等待节点确认结束及清理完成' }
    } catch (error) {
      return reply.code(409).send({ error: redactTaskLog(error instanceof Error ? error.message : String(error)) })
    }
  })

  app.get<{ Params: { id: number }; Querystring: { after?: number } }>('/api/admin/tasks/:id/logs/stream', {
    onRequest: auth,
    config: { rateLimit: { max: 60, timeWindow: '1 minute' } },
    schema: { params: taskIdSchema, querystring: { type: 'object', properties: { after: { type: 'integer', minimum: 0, maximum: 2147483647, default: 0 } } } }
  }, async (request, reply) => {
    const taskId = request.params.id
    const task = await prisma.operationTask.findUnique({ where: { id: taskId } })
    if (!task) return reply.code(404).send({ error: '任务不存在' })
    const userId = request.user.id
    if ((connections.get(userId) || 0) >= 5 || [...connections.values()].reduce((a, b) => a + b, 0) >= 100) {
      return reply.code(429).send({ error: '日志连接过多，请关闭其他日志窗口' })
    }
    connections.set(userId, (connections.get(userId) || 0) + 1)
    reply.hijack()
    const res = reply.raw
    res.writeHead(200, { ...(reply.getHeaders() as OutgoingHttpHeaders), 'Content-Type': 'text/event-stream; charset=utf-8', 'Cache-Control': 'no-cache, no-transform', 'X-Accel-Buffering': 'no', Connection: 'keep-alive' })
    let closed = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const deadline = Date.now() + 60000 // 短连接定期重新鉴权，客户端凭游标续传。
    let cursor = request.query.after || 0
    const cleanup = () => {
      if (closed) return
      closed = true
      clearTimeout(timer)
      const count = (connections.get(userId) || 1) - 1
      if (count > 0) connections.set(userId, count)
      else connections.delete(userId)
    }
    res.once('close', cleanup)
    const send = (event: string, data: unknown, id?: number) => {
      if (closed || res.destroyed || res.writableEnded) return false
      // A slow consumer reconnects from its last received cursor; never grow the socket buffer indefinitely.
      if (res.writableLength > 256 * 1024) { res.destroy(); cleanup(); return false }
      res.write(`${id ? `id: ${id}\n` : ''}event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)
      return true
    }
    const tick = async () => {
      if (closed) return
      try {
        const logs = await prisma.operationTaskLog.findMany({ where: { taskId, id: { gt: cursor } }, orderBy: { id: 'asc' }, take: 200 })
        for (const log of logs) {
          if (!send('log', log, log.id)) return
          cursor = log.id
        }
        const current = await prisma.operationTask.findUnique({ where: { id: taskId } })
        if (!send('task', current ? { ...current, ...operationTaskControl(taskId) } : null)) return
        // Drain all historical pages before completing; never truncate a finished task's logs.
        if ((current?.status !== 'running' && logs.length < 200) || Date.now() >= deadline) {
          send(current?.status === 'running' ? 'reconnect' : 'end', {})
          res.end(); cleanup(); return
        }
        timer = setTimeout(() => { void tick() }, logs.length === 200 ? 0 : 1000)
      } catch {
        send('stream-error', { message: '读取日志失败，请重连' })
        res.end(); cleanup()
      }
    }
    await tick()
  })
}
