import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify'
import { readFileSync } from 'node:fs'
import tls from 'node:tls'
import { pipeline } from 'node:stream/promises'
import { Agent, request as undiciRequest } from 'undici'
import WebSocket from 'ws'
import * as db from '../db/index.js'
import { createLog, LogModule, LogResult } from '../db/logs.js'
import {
  generateIncusUiAccessTicket,
  verifyIncusUiAccessTicket
} from '../lib/action-ticket.js'
import { resolveCertificatePair } from '../lib/incus/certificate-paths.js'
import { hostTunnelManager } from '../lib/incus/tunnel-manager.js'
import { isAccessTokenInvalidated } from '../lib/security.js'
import {
  validateWebSocketOrigin
} from '../lib/websocket-security.js'

const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\d*$/
const INCUS_UI_SUBPROTOCOL_PREFIX = 'incudal-incus-ui.'

const HOP_BY_HOP_HEADERS = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
  'host',
  'authorization',
  'cookie',
  'content-length'
])

const STRIPPED_RESPONSE_HEADERS = new Set([
  'connection',
  'keep-alive',
  'transfer-encoding',
  'content-security-policy',
  'x-frame-options',
  'strict-transport-security'
])

interface HostProxyParams {
  hostId: string
  '*'?: string
}

interface HostTicketParams {
  hostId: string
}

interface ProxyDownloadQuery {
  __incudal_ticket?: string
}

function parsePositiveRouteId(value: string): number | null {
  if (!POSITIVE_ROUTE_ID_PATTERN.test(value)) {
    return null
  }
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : null
}

/**
 * 规范化并校验代理到宿主机 incusd 的目标路径：
 * 1. 仅允许 /1.0 及 /1.0/* 路径
 * 2. 严禁路径穿越 (.. 或 \)
 * 3. 严禁访问内部节点控制扩展 /1.0/payincus
 */
export function normalizeIncusProxyTargetPath(wildcard: string, rawSearch = ''): string | null {
  const trimmed = String(wildcard || '').replace(/^\/+/, '')
  if (!trimmed) {
    return null
  }

  let decoded: string
  try {
    decoded = decodeURIComponent(trimmed)
  } catch {
    return null
  }

  if (
    trimmed.includes('..') ||
    decoded.includes('..') ||
    trimmed.includes('\\') ||
    decoded.includes('\\') ||
    decoded.includes('\0')
  ) {
    return null
  }

  const cleanPath = ('/' + trimmed).replace(/\/+/g, '/')
  if (cleanPath !== '/1.0' && !cleanPath.startsWith('/1.0/')) {
    return null
  }

  if (cleanPath === '/1.0/payincus' || cleanPath.startsWith('/1.0/payincus/')) {
    return null
  }

  const sanitizedSearch = sanitizeProxyQueryString(rawSearch)
  return `${cleanPath}${sanitizedSearch}`
}

function sanitizeProxyQueryString(rawSearch: string): string {
  if (!rawSearch || rawSearch === '?') return ''
  const raw = rawSearch.startsWith('?') ? rawSearch.slice(1) : rawSearch
  const kept = raw
    .split('&')
    .filter(Boolean)
    .filter((segment) => {
      const eqIdx = segment.indexOf('=')
      const key = eqIdx >= 0 ? segment.slice(0, eqIdx) : segment
      return key !== '__incudal_ticket' && key !== 'ticket'
    })
  return kept.length > 0 ? `?${kept.join('&')}` : ''
}

export function extractIncusUiTicketFromSubprotocols(headerValue: string | string[] | undefined): {
  ticket: string | null
  forwardProtocols: string[]
} {
  const raw = Array.isArray(headerValue) ? headerValue.join(',') : (headerValue || '')
  const parts = raw
    .split(',')
    .map(part => part.trim())
    .filter(Boolean)

  let ticket: string | null = null
  const forwardProtocols: string[] = []

  for (const part of parts) {
    if (part.startsWith(INCUS_UI_SUBPROTOCOL_PREFIX)) {
      const candidate = part.slice(INCUS_UI_SUBPROTOCOL_PREFIX.length).trim()
      if (candidate && candidate.length <= 128) {
        ticket = candidate
      }
    } else if (/^[a-zA-Z0-9._-]+$/.test(part)) {
      forwardProtocols.push(part)
    }
  }

  return { ticket, forwardProtocols }
}

const INCUS_UI_MAX_WS_PER_USER = 64
const incusUiUserConnections = new Map<number, number>()

/** ws 只允许发送 1000-1003、1007-1014、3000-4999；1005/1006 等保留码直接 close 会抛错导致另一端永不关闭。 */
function toSendableCloseCode(code: number): number {
  if (code === 1000 || code === 1001 || code === 1002 || code === 1003) return code
  if (code >= 1007 && code <= 1014) return code
  if (code >= 3000 && code <= 4999) return code
  return 1000
}

interface CachedTunnelDispatcher {
  agent: Agent
  certPath: string
  keyPath: string
  targetHost: string
  targetPort: number
}

const tunnelDispatchers = new Map<number, CachedTunnelDispatcher>()

function getOrCreateTunnelDispatcher(host: {
  id: number
  cert_path?: string | null
  key_path?: string | null
  target_host?: string | null
  target_port?: number | null
}): { agent: Agent; cert: Buffer; key: Buffer; targetHost: string; targetPort: number } {
  const pair = resolveCertificatePair(host.cert_path || null, host.key_path || null)
  if (!pair.certPath || !pair.keyPath) {
    throw new Error('宿主机未配置有效的客户端证书或私钥')
  }

  const cert = readFileSync(pair.certPath)
  const key = readFileSync(pair.keyPath)
  const targetHost = host.target_host || '127.0.0.1'
  const targetPort = host.target_port || 8443

  const existing = tunnelDispatchers.get(host.id)
  if (
    existing &&
    existing.certPath === pair.certPath &&
    existing.keyPath === pair.keyPath &&
    existing.targetHost === targetHost &&
    existing.targetPort === targetPort
  ) {
    return { agent: existing.agent, cert, key, targetHost, targetPort }
  }

  if (existing) {
    void existing.agent.close().catch(() => {})
    tunnelDispatchers.delete(host.id)
  }

  const agent = new Agent({
    connect: (_opts: unknown, cb: (err: Error | null, socket: any) => void) => {
      try {
        const duplex = hostTunnelManager.createDuplexStream(host.id, targetHost, targetPort)
        const tlsSocket = tls.connect({
          socket: duplex,
          cert,
          key,
          rejectUnauthorized: false
        })
        let settled = false
        tlsSocket.once('secureConnect', () => {
          if (!settled) {
            settled = true
            cb(null, tlsSocket)
          }
        })
        tlsSocket.once('error', (err) => {
          if (!settled) {
            settled = true
            cb(err, null)
          }
        })
      } catch (err: any) {
        cb(err instanceof Error ? err : new Error(String(err)), null)
      }
    },
    headersTimeout: 120_000,
    bodyTimeout: 0
  })

  tunnelDispatchers.set(host.id, {
    agent,
    certPath: pair.certPath,
    keyPath: pair.keyPath,
    targetHost,
    targetPort
  })

  return { agent, cert, key, targetHost, targetPort }
}

async function authenticateAdminProxyRequest(
  fastify: FastifyInstance,
  request: FastifyRequest,
  reply: FastifyReply,
  hostId: number
): Promise<{ id: number; username: string; role: string; sid?: string } | null> {
  const query = (request.query || {}) as ProxyDownloadQuery
  const queryTicket = typeof query.__incudal_ticket === 'string' ? query.__incudal_ticket : null

  if (!request.headers.authorization && queryTicket && (request.method === 'GET' || request.method === 'HEAD')) {
    const ticketResult = verifyIncusUiAccessTicket(queryTicket, hostId)
    if (!ticketResult.valid || !ticketResult.userId || !ticketResult.issuedAt) {
      reply.code(401).send({ error: 'Invalid or expired Incus UI ticket', code: 'UNAUTHORIZED' })
      return null
    }

    const invalidated = await isAccessTokenInvalidated(
      ticketResult.userId,
      ticketResult.issuedAt,
      ticketResult.sessionId
    )
    if (invalidated) {
      reply.code(401).send({ error: 'Session expired', code: 'SESSION_INVALIDATED' })
      return null
    }

    const dbUser = await db.findUserById(ticketResult.userId)
    if (!dbUser || dbUser.status !== 'active') {
      reply.code(401).send({ error: 'Unauthorized', code: 'UNAUTHORIZED' })
      return null
    }

    const user = {
      id: dbUser.id,
      username: dbUser.username,
      role: dbUser.role,
      sid: ticketResult.sessionId
    }
    if (user.role !== 'admin') {
      reply.code(403).send({ error: 'Admin privileges required', code: 'ADMIN_REQUIRED' })
      return null
    }
    return user
  }

  await fastify.authenticateAdmin(request, reply)
  if (reply.sent) {
    return null
  }

  const user = request.user as { id: number; username: string; role: string; sid?: string }
  if (user.role !== 'admin') {
    reply.code(403).send({ error: 'Admin privileges required', code: 'ADMIN_REQUIRED' })
    return null
  }

  return user
}

export default async function adminIncusUiRoutes(fastify: FastifyInstance) {
  /**
   * 签发 Incus UI 短期多路复用访问票据（用于 WebSocket 子协议鉴权与原生文件下载）
   * POST /api/admin/hosts/:hostId/incus-ui-ticket
   */
  fastify.post<{ Params: HostTicketParams }>('/api/admin/hosts/:hostId/incus-ui-ticket', {
    onRequest: [fastify.authenticateAdmin]
  }, async (request, reply) => {
    const hostId = parsePositiveRouteId(request.params.hostId)
    if (!hostId) {
      return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
    }

    const user = request.user as { id: number; role: string; iat?: number; sid?: string }
    if (user.role !== 'admin') {
      return reply.code(403).send({ error: 'Admin privileges required', code: 'ADMIN_REQUIRED' })
    }
    if (!user.iat) {
      return reply.code(401).send({ error: 'Unauthorized', code: 'UNAUTHORIZED' })
    }

    const host = await db.getHostById(hostId)
    if (!host) {
      return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
    }

    if (!hostTunnelManager.isTunnelOnline(host.id)) {
      return reply.code(503).send({
        error: '宿主机反向隧道未连接（Agent 离线），无法访问 Incus UI',
        code: 'TUNNEL_OFFLINE'
      })
    }

    const ticket = generateIncusUiAccessTicket(user.id, host.id, user.iat, user.sid)
    reply.header('Cache-Control', 'no-store')
    return {
      ticket,
      expiresIn: 300
    }
  })

  /**
   * 封装 HTTP 反向隧道代理子插件（保留二进制/流请求体，同时保证 Content-Length 同步转发给 incusd）
   */
  await fastify.register(async function incusHttpProxyPlugin(proxyApp) {
    proxyApp.removeAllContentTypeParsers()
    proxyApp.addContentTypeParser('*', { parseAs: 'buffer', bodyLimit: 100 * 1024 * 1024 }, (_req, body, done) => {
      done(null, body)
    })

    const handleHttpProxy = async (
      request: FastifyRequest<{ Params: HostProxyParams }>,
      reply: FastifyReply
    ) => {
      const hostId = parsePositiveRouteId(request.params.hostId)
      if (!hostId) {
        return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
      }

      const user = await authenticateAdminProxyRequest(proxyApp, request, reply, hostId)
      if (!user) {
        return
      }
      if (user.role !== 'admin') {
        return reply.code(403).send({ error: 'Admin privileges required', code: 'ADMIN_REQUIRED' })
      }

      const rawUrl = request.raw.url || ''
      const queryIndex = rawUrl.indexOf('?')
      const rawSearch = queryIndex >= 0 ? rawUrl.slice(queryIndex) : ''
      const wildcard = request.params['*'] || ''
      const targetPath = normalizeIncusProxyTargetPath(wildcard, rawSearch)

      if (!targetPath) {
        return reply.code(403).send({
          error: 'Forbidden Incus proxy path',
          code: 'FORBIDDEN_PROXY_PATH'
        })
      }

      const host = await db.getHostById(hostId)
      if (!host) {
        return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
      }

      if (!hostTunnelManager.isTunnelOnline(host.id)) {
        return reply.code(503).send({
          error: '宿主机反向隧道未连接（Agent 离线），无法访问 Incus UI',
          code: 'TUNNEL_OFFLINE'
        })
      }

      let dispatcherInfo: ReturnType<typeof getOrCreateTunnelDispatcher>
      try {
        dispatcherInfo = getOrCreateTunnelDispatcher(host)
      } catch (err: any) {
        return reply.code(502).send({
          error: err?.message || '宿主机客户端证书不可用',
          code: 'INCUS_CERT_UNAVAILABLE'
        })
      }

      const forwardHeaders: Record<string, string> = {}
      for (const [key, val] of Object.entries(request.headers)) {
        const lower = key.toLowerCase()
        if (HOP_BY_HOP_HEADERS.has(lower) || val === undefined) continue
        forwardHeaders[key] = Array.isArray(val) ? val.join(', ') : String(val)
      }

      const method = request.method.toUpperCase()
      const rawBody = request.body as Buffer | undefined
      const hasBody = !['GET', 'HEAD'].includes(method) && Buffer.isBuffer(rawBody) && rawBody.length > 0

      try {
        const upstream = await undiciRequest(`https://127.0.0.1:8443${targetPath}`, {
          method: method as any,
          dispatcher: dispatcherInfo.agent,
          headers: forwardHeaders,
          body: hasBody ? rawBody : undefined,
          headersTimeout: 120_000,
          bodyTimeout: 0
        })

        if (hasBody && upstream.statusCode < 400) {
          void createLog(
            user.id,
            LogModule.HOST,
            'host.incus_ui_mutation',
            `IncusUI 节点操作: ${host.name} (#${host.id}) | ${method} ${targetPath.split('?')[0]}`,
            LogResult.SUCCESS
          ).catch(() => {})
        }

        reply.hijack()
        const rawRes = reply.raw
        rawRes.statusCode = upstream.statusCode

        for (const [key, val] of Object.entries(upstream.headers)) {
          const lower = key.toLowerCase()
          if (STRIPPED_RESPONSE_HEADERS.has(lower) || val === undefined) continue
          rawRes.setHeader(key, val as string | string[])
        }

        await pipeline(upstream.body, rawRes)
      } catch (err: any) {
        if (!reply.raw.headersSent) {
          return reply.code(502).send({
            error: 'Failed to proxy request to host Incus daemon over tunnel',
            code: 'INCUS_PROXY_ERROR'
          })
        } else {
          reply.raw.destroy()
        }
      }
    }

    proxyApp.get<{ Params: HostProxyParams }>('/api/admin/hosts/:hostId/incus-proxy/*', {
      onRequest: [
        async (request, reply) => {
          const query = (request.query || {}) as ProxyDownloadQuery
          if (!request.headers.authorization && typeof query.__incudal_ticket === 'string') {
            return
          }
          await proxyApp.authenticateAdmin(request, reply)
        }
      ]
    }, handleHttpProxy)

    proxyApp.post<{ Params: HostProxyParams }>('/api/admin/hosts/:hostId/incus-proxy/*', {
      onRequest: [proxyApp.authenticateAdmin]
    }, handleHttpProxy)

    proxyApp.put<{ Params: HostProxyParams }>('/api/admin/hosts/:hostId/incus-proxy/*', {
      onRequest: [proxyApp.authenticateAdmin]
    }, handleHttpProxy)

    proxyApp.patch<{ Params: HostProxyParams }>('/api/admin/hosts/:hostId/incus-proxy/*', {
      onRequest: [proxyApp.authenticateAdmin]
    }, handleHttpProxy)

    proxyApp.delete<{ Params: HostProxyParams }>('/api/admin/hosts/:hostId/incus-proxy/*', {
      onRequest: [proxyApp.authenticateAdmin]
    }, handleHttpProxy)
  })

  /**
   * 反向隧道 WebSocket 全双工桥接（用于 /1.0/events、/1.0/operations/:id/websocket 终端与控制台）
   * GET /api/ws/hosts/:hostId/incus-proxy/*
   */
  fastify.get<{
    Params: HostProxyParams
    Querystring: { ticket?: string }
  }>('/api/ws/hosts/:hostId/incus-proxy/*', {
    websocket: true
  }, async (socket, request) => {
    const hostId = parsePositiveRouteId(request.params.hostId)
    if (!hostId) {
      socket.close(4000, 'Invalid host ID')
      return
    }

    const originCheck = validateWebSocketOrigin(request)
    if (!originCheck.allowed) {
      socket.close(4003, 'Origin not allowed')
      return
    }

    // 在异步鉴权完成前先缓存客户端可能立即发出的帧
    const pendingClientMessages: Array<{ data: WebSocket.RawData; isBinary: boolean }> = []
    let upstreamReady = false
    let clientClosed = false

    const earlyMessageHandler = (data: WebSocket.RawData, isBinary: boolean) => {
      if (!upstreamReady && pendingClientMessages.length < 128) {
        pendingClientMessages.push({ data, isBinary })
      }
    }
    const earlyCloseHandler = () => {
      clientClosed = true
    }
    socket.on('message', earlyMessageHandler)
    socket.once('close', earlyCloseHandler)

    const { ticket: subprotocolTicket, forwardProtocols } = extractIncusUiTicketFromSubprotocols(
      request.headers['sec-websocket-protocol']
    )
    const ticket = subprotocolTicket || (typeof request.query?.ticket === 'string' ? request.query.ticket : null)
    if (!ticket) {
      socket.close(4001, 'Authentication ticket required')
      return
    }

    const ticketResult = verifyIncusUiAccessTicket(ticket, hostId)
    if (!ticketResult.valid || !ticketResult.userId || !ticketResult.issuedAt) {
      socket.close(4001, 'Invalid or expired ticket')
      return
    }

    const invalidated = await isAccessTokenInvalidated(
      ticketResult.userId,
      ticketResult.issuedAt,
      ticketResult.sessionId
    )
    if (invalidated) {
      socket.close(4001, 'Session expired')
      return
    }

    const dbUser = await db.findUserById(ticketResult.userId)
    if (!dbUser || dbUser.status !== 'active') {
      socket.close(4001, 'Unauthorized')
      return
    }

    const user = {
      id: dbUser.id,
      username: dbUser.username,
      role: dbUser.role
    }
    if (user.role !== 'admin') {
      socket.close(4003, 'Admin privileges required')
      return
    }

    // Incus UI 单个页面会并行持有 events + 终端(data/control) + 控制台(data/control) 等多条长连接，
    // 不能复用通用 WS 的 5/用户、10/IP 限额，否则超限后前端 events 会在 101 后被立刻关闭并无限重连。
    if ((incusUiUserConnections.get(user.id) || 0) >= INCUS_UI_MAX_WS_PER_USER) {
      socket.close(4008, 'WebSocket connection limit exceeded')
      return
    }

    const rawUrl = request.raw.url || ''
    const queryIndex = rawUrl.indexOf('?')
    const rawSearch = queryIndex >= 0 ? rawUrl.slice(queryIndex) : ''
    const wildcard = request.params['*'] || ''
    const targetPath = normalizeIncusProxyTargetPath(wildcard, rawSearch)

    if (!targetPath) {
      socket.close(4003, 'Forbidden Incus proxy path')
      return
    }

    const host = await db.getHostById(hostId)
    if (!host) {
      socket.close(4004, 'Host not found')
      return
    }

    if (!hostTunnelManager.isTunnelOnline(host.id)) {
      socket.close(4004, 'Host tunnel offline')
      return
    }

    if (clientClosed) {
      return
    }

    let dispatcherInfo: ReturnType<typeof getOrCreateTunnelDispatcher>
    try {
      dispatcherInfo = getOrCreateTunnelDispatcher(host)
    } catch {
      socket.close(4000, 'Host certificate unavailable')
      return
    }

    incusUiUserConnections.set(user.id, (incusUiUserConnections.get(user.id) || 0) + 1)
    socket.once('close', () => {
      const remaining = (incusUiUserConnections.get(user.id) || 1) - 1
      if (remaining > 0) incusUiUserConnections.set(user.id, remaining)
      else incusUiUserConnections.delete(user.id)
    })

    const upstreamUrl = `wss://127.0.0.1:8443${targetPath}`
    const upstream = new WebSocket(upstreamUrl, forwardProtocols.length > 0 ? forwardProtocols : undefined, {
      cert: dispatcherInfo.cert,
      key: dispatcherInfo.key,
      rejectUnauthorized: false,
      handshakeTimeout: 15_000,
      maxPayload: 16 * 1024 * 1024,
      createConnection: () => {
        const duplex = hostTunnelManager.createDuplexStream(
          host.id,
          dispatcherInfo.targetHost,
          dispatcherInfo.targetPort
        )
        return tls.connect({
          socket: duplex,
          cert: dispatcherInfo.cert,
          key: dispatcherInfo.key,
          rejectUnauthorized: false
        })
      }
    })

    const cleanupBoth = () => {
      try {
        if (upstream.readyState === WebSocket.OPEN || upstream.readyState === WebSocket.CONNECTING) {
          upstream.terminate()
        }
      } catch {
        // ignore
      }
      try {
        if (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING) {
          socket.close()
        }
      } catch {
        // ignore
      }
    }

    upstream.once('open', () => {
      if (clientClosed || socket.readyState !== WebSocket.OPEN) {
        cleanupBoth()
        return
      }
      upstreamReady = true
      socket.off('message', earlyMessageHandler)

      for (const item of pendingClientMessages.splice(0)) {
        try {
          upstream.send(item.data, { binary: item.isBinary })
        } catch {
          cleanupBoth()
          return
        }
      }

      socket.on('message', (data: WebSocket.RawData, isBinary: boolean) => {
        if (upstream.readyState === WebSocket.OPEN) {
          upstream.send(data, { binary: isBinary })
        }
      })
    })

    upstream.on('message', (data: WebSocket.RawData, isBinary: boolean) => {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(data, { binary: isBinary })
      }
    })

    upstream.once('close', (code) => {
      try {
        if (socket.readyState === WebSocket.OPEN) {
          socket.close(toSendableCloseCode(code))
        }
      } catch {
        // ignore
      }
    })

    upstream.once('error', () => {
      cleanupBoth()
    })

    socket.once('close', () => {
      clientClosed = true
      cleanupBoth()
    })

    socket.once('error', () => {
      clientClosed = true
      cleanupBoth()
    })
  })

  fastify.addHook('onClose', async () => {
    for (const item of tunnelDispatchers.values()) {
      await item.agent.close().catch(() => {})
    }
    tunnelDispatchers.clear()
  })
}
