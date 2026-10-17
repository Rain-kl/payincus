import type { FastifyInstance } from 'fastify'
import * as db from '../db/index.js'
import { createLog } from '../db/logs.js'
import { isAccessTokenInvalidated } from '../lib/security.js'
import { createSpiceBackend, SpiceConnectionError } from '../lib/incus/spice-connection.js'
import { spiceSessions, type SpiceAccess, type SpiceSessionManager } from '../lib/spice-sessions.js'
import { checkIPConnectionLimit, checkUserConnectionLimit, getClientIP, registerConnection, validateWebSocketOrigin } from '../lib/websocket-security.js'

const defaultDependencies = {
  getInstance: db.getInstanceById, getHost: db.getHostById, getPackage: db.getPackageById,
  getUser: db.findUserById, invalidated: isAccessTokenInvalidated,
  open: createSpiceBackend, audit: createLog
}

export default async function spiceRoutes(fastify: FastifyInstance, options: {
  dependencies?: Partial<typeof defaultDependencies>; sessions?: SpiceSessionManager
}) {
  const deps = { ...defaultDependencies, ...options.dependencies }
  const sessions = options.sessions || spiceSessions
  function parseId(value: string): number | null {
    if (!/^[1-9]\d*$/.test(value)) return null
    const id = Number(value)
    return Number.isSafeInteger(id) ? id : null
  }
  async function access(context: SpiceAccess) {
    if (await deps.invalidated(context.userId, context.issuedAt, context.sessionId)) return null
    const user = await deps.getUser(context.userId)
    if (!user || user.status === 'banned') return null
    const instance = await deps.getInstance(context.instanceId)
    if (!instance || instance.status !== 'running') return null
    const host = await deps.getHost(instance.host_id)
    if (!host || (user.role !== 'admin' && instance.user_id !== user.id && host.user_id !== user.id)) return null
    if ((context.hostId !== undefined && host.id !== context.hostId) ||
        (context.instanceName !== undefined && instance.incus_id !== context.instanceName)) return null
    const pkg = instance.package_id ? await deps.getPackage(instance.package_id) : null
    if (pkg?.instance_type !== 'vm') return null
    return { user, instance, host }
  }

  fastify.post<{ Params: { id: string } }>('/:id/spice-session', {
    onRequest: [fastify.authenticate], config: { rateLimit: { max: 10, timeWindow: '1 minute' } }
  }, async (request, reply) => {
    const instanceId = parseId(request.params.id)
    if (instanceId === null) return reply.code(400).send({ error: 'Invalid instance ID', code: 'INVALID_ID' })
    if (!request.user.iat) return reply.code(401).send({ error: 'Unauthorized', code: 'UNAUTHORIZED' })
    const context: SpiceAccess = { userId: request.user.id, instanceId, issuedAt: request.user.iat, sessionId: request.user.sid }
    const current = await access(context)
    if (!current) return reply.code(403).send({ error: 'Running VM access required', code: 'CONSOLE_FORBIDDEN' })
    context.hostId = current.host.id
    context.instanceName = current.instance.incus_id
    if (!checkIPConnectionLimit(getClientIP(request)).allowed || !checkUserConnectionLimit(context.userId).allowed) {
      return reply.code(429).send({ error: 'Connection limit exceeded', code: 'CONSOLE_LIMIT' })
    }
    let disconnected = false
    reply.raw.once('close', () => { if (!reply.raw.writableFinished) disconnected = true })
    try {
      const ticket = await sessions.create(context,
        signal => deps.open(current.host, current.instance.incus_id, signal),
        async () => Boolean(await access(context)))
      if (disconnected) { await sessions.close(ticket); return }
      // This capability is limited to this console session; it is NOT a reusable login token.
      reply.header('Cache-Control', 'no-store')
      return { ticket, expiresIn: 7200 }
    } catch (error) {
      const limited = error instanceof Error && error.message === 'CONSOLE_LIMIT'
      request.log.warn({ instanceId, stage: error instanceof SpiceConnectionError ? error.stage : 'session',
        httpStatus: error instanceof SpiceConnectionError ? error.httpStatus : undefined },
      limited ? 'SPICE console limit reached' : 'SPICE console setup failed')
      return reply.code(limited ? 409 : 502).send({
        error: limited ? 'Console already open or connection limit reached' : 'Unable to open Incus graphical console',
        code: limited ? 'CONSOLE_LIMIT' : 'CONSOLE_UNAVAILABLE'
      })
    }
  })

  fastify.delete<{ Params: { id: string }; Body: { ticket: string } }>('/:id/spice-session', {
    onRequest: [fastify.authenticate],
    schema: { body: { type: 'object', required: ['ticket'], properties: { ticket: { type: 'string', maxLength: 64 } } } }
  }, async (request, reply) => {
    const instanceId = parseId(request.params.id)
    if (instanceId === null) return reply.code(400).send({ error: 'Invalid instance ID' })
    const context = sessions.get(request.body.ticket, instanceId)
    if (!context) return { success: true } // Main-channel close may have cleaned it already.
    if (context.userId !== request.user.id || context.sessionId !== request.user.sid) {
      return reply.code(403).send({ error: 'Forbidden' })
    }
    await sessions.close(request.body.ticket)
    await sessions.waitForCleanup(instanceId)
    return { success: true }
  })

  fastify.get<{ Params: { id: string } }>('/:id/spice', {
    websocket: true
  }, (socket, request) => {
    const instanceId = parseId(request.params.id)
    if (instanceId === null || !validateWebSocketOrigin(request).allowed) return socket.close(4003, 'Forbidden')
    // Keep the capability out of URLs and standard reverse-proxy access logs.
    // "binary" is first so ws negotiates that protocol, never echoing the ticket.
    const protocols = (request.headers['sec-websocket-protocol'] || '').split(',').map(value => value.trim())
    const ticket = protocols.find(value => value.startsWith('incudal-spice.'))?.slice('incudal-spice.'.length)
    const context = ticket && ticket.length <= 64 ? sessions.get(ticket, instanceId) : undefined
    if (!context || !ticket) return socket.close(4001, 'Console session expired')
    // attach() captures initial SPICE handshake bytes BEFORE any asynchronous authorization.
    sessions.attach(ticket, instanceId, socket, async (main) => {
      const current = await access(context)
      if (!current) return false
      // The main/display/input sockets are one logical console for generic limits.
      if (main) {
        const ip = getClientIP(request)
        if (!checkIPConnectionLimit(ip).allowed || !checkUserConnectionLimit(context.userId).allowed) return false
        registerConnection(ip, socket, context.userId)
        socket.once('close', () => {
          void deps.audit(context.userId, 'terminal', 'console.disconnect', `Graphical console disconnected (#${instanceId})`, 'success', { instanceId }).catch(() => {})
        })
        void deps.audit(context.userId, 'terminal', 'console.connect', `Graphical console connected (#${instanceId})`, 'success', { instanceId }).catch(() => {})
      }
      return true
    })
  })
  fastify.addHook('onClose', async () => { await sessions.drain() })
}
