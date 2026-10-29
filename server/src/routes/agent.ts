import { processAgentStorageReport } from '../services/agent-storage-report.js'
import { recordHostSuccess } from '../lib/incus/host-health.js'
import { recordReport } from '../lib/node-security/report.js'
import { syncHostGpuReport } from '../db/gpus.js'
import { recordNetworkStatus } from '../lib/egress/sync.js'
import { pushNetworkPolicy } from '../lib/egress/realtime.js'
/**
 * 宿主机 Agent 路由
 * 负责 Agent 凭据签发、HMAC 鉴权和心跳上报。
 */

import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify'
import { Prisma } from '@prisma/client'
import { createHash } from 'crypto'
import { readFileSync } from 'fs'
import { isIP } from 'net'
import { dirname, join } from 'path'
import { fileURLToPath } from 'url'
import { prisma } from '../db/prisma.js'
import { createLog, LogModule, LogResult } from '../db/logs.js'
import { decryptSensitiveData } from '../lib/security.js'
import { buildNodeInstallCommand } from '../lib/node-install.js'
import { getPanelCertificatePaths, readPanelClientCertificate } from '../lib/panel-certificate.js'
import { extractHostPublicIpv6FromResources } from '../lib/host-resources.js'
import {
  agentNonceTtlMs,
  createAgentBodyHash,
  isAgentTimestampFresh,
  isValidAgentSecret,
  readAgentAuthHeaders,
  validateAgentHeaders,
  verifyAgentSignature
} from '../lib/agent-auth.js'
import {
  consumeHostAgentInstallToken,
  issueHostAgentInstallToken,
  rotateHostAgentCredentials,
  type HostAgentRecord
} from '../lib/host-agent-credentials.js'
import { processAgentInstanceReport } from '../services/agent-instance-report.js'
import { hostTunnelManager } from '../lib/incus/tunnel-manager.js'

interface AgentCredentialsParams {
  hostId: string
}

interface AgentCredentialsBody {
  enabled?: boolean
}

interface AgentInstallCommandBody {
  enabled?: boolean
  baseUrl?: string
  channel?: 'latest' | 'canary'
}

interface AgentHeartbeatBody {
  version?: string
  capabilities?: string[]
  runtime?: Record<string, unknown>
  incus?: Record<string, unknown>
  caddy?: Record<string, unknown>
  instances?: Record<string, unknown>
  resources?: Record<string, unknown>
  metrics?: Record<string, unknown>
  storage?: Record<string, unknown>
  heartbeatRequestId?: string
  nodeApi?: string
  nodeSecurity?: { state?: { locked?: boolean } }
  networkStatus?: Record<string, unknown>
}

interface AgentInstallTokenParams {
  token: string
}

const agentModel = prisma.hostAgent
const nonceModel = prisma.hostAgentNonce
const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)

/**
 * 显式请求过「安装 Caddy」的宿主机集合（进程内状态）。
 * 安装指令只对集合内的宿主机下发；Agent 上报可用后清除。
 * 重启丢失可接受：用户重新点击「安装 Caddy」即可。
 */
export const caddyInstallRequested = new Set<number>()

/**
 * 显式请求过「卸载 Caddy」的宿主机集合（进程内状态）。
 * 卸载指令只对集合内的宿主机下发；Agent 上报已移除后清除。
 */
export const caddyUninstallRequested = new Set<number>()

const maxAgentInstallUrlLength = 2048
const defaultAgentHeartbeatIntervalSeconds = 5
const minAgentHeartbeatIntervalSeconds = 5
const maxAgentHeartbeatIntervalSeconds = 3600
const minAgentOfflineThresholdSeconds = 120
const positiveRouteIdPattern = /^[1-9]\d*$/

function parsePositiveId(value: string): number | null {
  if (!positiveRouteIdPattern.test(value)) {
    return null
  }
  const id = Number(value)
  return Number.isSafeInteger(id) && id > 0 ? id : null
}

function sanitizeShortString(value: unknown, maxLength: number): string | null {
  if (typeof value !== 'string') {
    return null
  }
  const trimmed = value.trim()
  return trimmed ? trimmed.slice(0, maxLength) : null
}

function normalizeCapabilities(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return []
  }

  const uniqueCapabilities = new Set<string>()
  for (const item of value) {
    const capability = sanitizeShortString(item, 80)
    if (capability) {
      uniqueCapabilities.add(capability)
    }
    if (uniqueCapabilities.size >= 64) {
      break
    }
  }
  return Array.from(uniqueCapabilities)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function parseInteger(value: unknown): number | null {
  if (typeof value === 'number' && Number.isSafeInteger(value)) {
    return value
  }
  if (typeof value === 'string' && /^\d+$/.test(value.trim())) {
    const parsed = Number.parseInt(value.trim(), 10)
    return Number.isSafeInteger(parsed) ? parsed : null
  }
  return null
}

function clampAgentHeartbeatIntervalSeconds(value: unknown): number {
  const parsed = parseInteger(value)
  if (!parsed) {
    return defaultAgentHeartbeatIntervalSeconds
  }
  if (parsed < minAgentHeartbeatIntervalSeconds) {
    return minAgentHeartbeatIntervalSeconds
  }
  if (parsed > maxAgentHeartbeatIntervalSeconds) {
    return maxAgentHeartbeatIntervalSeconds
  }
  return parsed
}

function buildRequestPath(request: FastifyRequest): string {
  return request.url.split('?')[0] || request.url
}

function firstHeaderValue(value: string | string[] | undefined): string | null {
  if (Array.isArray(value)) {
    return value[0] ?? null
  }
  return value ?? null
}

function normalizeIpCandidate(value: unknown): string | null {
  if (typeof value !== 'string') {
    return null
  }

  let candidate = value.trim()
  if (!candidate) {
    return null
  }

  if (candidate.startsWith('[')) {
    const closingBracketIndex = candidate.indexOf(']')
    if (closingBracketIndex > 0) {
      candidate = candidate.slice(1, closingBracketIndex)
    }
  }

  const ipv4WithPort = candidate.match(/^(\d{1,3}(?:\.\d{1,3}){3})(?::\d+)?$/)
  if (ipv4WithPort) {
    candidate = ipv4WithPort[1]
  }

  if (candidate.startsWith('::ffff:')) {
    const mapped = candidate.slice(7)
    if (isIP(mapped)) {
      return mapped
    }
  }

  return isIP(candidate) ? candidate : null
}

function firstForwardedIp(request: FastifyRequest): string | null {
  const headerValues = [
    firstHeaderValue(request.headers['cf-connecting-ip']),
    firstHeaderValue(request.headers['true-client-ip']),
    firstHeaderValue(request.headers['x-real-ip']),
    firstHeaderValue(request.headers['x-forwarded-for'])
  ]

  for (const value of headerValues) {
    if (!value) {
      continue
    }
    for (const part of value.split(',')) {
      const ip = normalizeIpCandidate(part)
      if (ip) {
        return ip
      }
    }
  }

  return null
}

function isLocalOrPrivateIp(ip: string | null): boolean {
  if (!ip) {
    return false
  }

  if (ip === '::1' || ip === '127.0.0.1' || ip.startsWith('127.')) {
    return true
  }

  if (ip.startsWith('10.') || ip.startsWith('192.168.') || ip.startsWith('169.254.')) {
    return true
  }

  const parts = ip.split('.').map((part) => Number.parseInt(part, 10))
  if (parts.length === 4 && parts.every((part) => Number.isInteger(part))) {
    return parts[0] === 172 && parts[1] >= 16 && parts[1] <= 31
  }

  const lowerIp = ip.toLowerCase()
  return lowerIp.startsWith('fc') || lowerIp.startsWith('fd') || lowerIp.startsWith('fe80:')
}

function getAgentHeartbeatIp(request: FastifyRequest): string {
  const directIp = normalizeIpCandidate(request.ip) ?? request.ip
  const forwardedIp = firstForwardedIp(request)
  const trustForwardedIp = process.env.AGENT_TRUST_FORWARDED_IP === 'true' || isLocalOrPrivateIp(directIp)

  return forwardedIp && trustForwardedIp ? forwardedIp : directIp
}

function normalizeBaseUrl(value: string | null | undefined): string | null {
  const trimmed = value?.trim()
  if (!trimmed) {
    return null
  }
  if (trimmed.length > maxAgentInstallUrlLength) {
    return null
  }

  try {
    const parsed = new URL(trimmed)
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
      return null
    }
    return `${parsed.protocol}//${parsed.host}`
  } catch {
    return null
  }
}

function derivePanelUrl(request: FastifyRequest, explicitBaseUrl?: string): string {
  const explicit = normalizeBaseUrl(explicitBaseUrl)
  if (explicit) {
    return explicit
  }

  const frontendUrl = normalizeBaseUrl(process.env.FRONTEND_URL?.split(',')[0])
  if (frontendUrl) {
    return frontendUrl
  }

  const refererBaseUrl = normalizeBaseUrl(firstHeaderValue(request.headers.origin) ?? firstHeaderValue(request.headers.referer))
  if (refererBaseUrl) {
    return refererBaseUrl
  }

  const forwardedProto = firstHeaderValue(request.headers['x-forwarded-proto'])?.split(',')[0]?.trim()
  const forwardedHost = firstHeaderValue(request.headers['x-forwarded-host'])?.split(',')[0]?.trim()
  const directHost = firstHeaderValue(request.headers.host)?.trim()
  const protocol = forwardedProto === 'https' || forwardedProto === 'http'
    ? forwardedProto
    : (request.protocol || 'http')
  const host = forwardedHost || directHost

  if (host) {
    return `${protocol}://${host}`
  }

  return 'https://incudal.com'
}

function shellEscape(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

function buildAgentInstallConfig(input: {
  agentId: string
  agentSecret: string
}): string {
  return [
    `INCUDAL_AGENT_ID=${shellEscape(input.agentId)}`,
    `INCUDAL_AGENT_SECRET=${shellEscape(input.agentSecret)}`
  ].join('\n') + '\n'
}

function normalizeAgentMetrics(metrics: Record<string, unknown> | undefined): Record<string, unknown> {
  return {
    ...(metrics ?? {}),
    heartbeatIntervalSeconds: clampAgentHeartbeatIntervalSeconds(metrics?.heartbeatIntervalSeconds)
  }
}

function getAgentHeartbeatIntervalSeconds(agent: HostAgentRecord): number {
  if (!isRecord(agent.lastReport)) {
    return defaultAgentHeartbeatIntervalSeconds
  }
  const metrics = agent.lastReport.metrics
  if (!isRecord(metrics)) {
    return defaultAgentHeartbeatIntervalSeconds
  }
  return clampAgentHeartbeatIntervalSeconds(metrics.heartbeatIntervalSeconds)
}

function deriveAgentStatus(agent: HostAgentRecord, now = new Date()): string {
  if (!agent.enabled || agent.status !== 'online') {
    return agent.status
  }
  if (!agent.lastSeenAt) {
    return 'offline'
  }

  const heartbeatIntervalSeconds = getAgentHeartbeatIntervalSeconds(agent)
  const offlineThresholdSeconds = Math.max(heartbeatIntervalSeconds * 3, minAgentOfflineThresholdSeconds)
  const lastSeenAgeMs = now.getTime() - agent.lastSeenAt.getTime()

  return lastSeenAgeMs > offlineThresholdSeconds * 1000 ? 'offline' : agent.status
}

async function serializeAgent(agent: HostAgentRecord) {
  const versionStatus = Array.isArray(agent.capabilities) && agent.capabilities.includes('payincus_node_v1') ? 'managed' : 'unknown'

  return {
    id: agent.id,
    hostId: agent.hostId,
    agentId: agent.agentId,
    enabled: agent.enabled,
    status: deriveAgentStatus(agent),
    version: agent.version,
    versionStatus,
    capabilities: agent.capabilities ?? [],
    lastReport: agent.lastReport ?? {},
    lastSeenAt: agent.lastSeenAt?.toISOString() ?? null,
    lastHeartbeatIp: agent.lastHeartbeatIp,
    createdAt: agent.createdAt.toISOString(),
    updatedAt: agent.updatedAt.toISOString()
  }
}

async function serializeAgentStatus(agent: HostAgentRecord) {
  const versionStatus = Array.isArray(agent.capabilities) && agent.capabilities.includes('payincus_node_v1') ? 'managed' : 'unknown'

  return {
    id: agent.id,
    hostId: agent.hostId,
    agentId: agent.agentId,
    enabled: agent.enabled,
    status: deriveAgentStatus(agent),
    version: agent.version,
    versionStatus,
    capabilities: agent.capabilities ?? [],
    lastReport: agent.lastReport ?? {},
    lastSeenAt: agent.lastSeenAt?.toISOString() ?? null,
    lastHeartbeatIp: agent.lastHeartbeatIp,
    createdAt: agent.createdAt.toISOString(),
    updatedAt: agent.updatedAt.toISOString()
  }
}

function buildHeartbeatReport(body: AgentHeartbeatBody, previous: unknown): Record<string, unknown> {
  const resources = { ...(body.resources || {}) }
  const previousResources = isRecord(previous) && isRecord(previous.resources) ? previous.resources : {}
  if (resources.storageAvailable === false && previousResources.storage) resources.storage = previousResources.storage
  return {
    runtime: body.runtime ?? {},
    incus: body.incus ?? {},
    instances: { available: body.instances?.available === true },
    caddy: body.caddy ?? {},
    resources,
    metrics: normalizeAgentMetrics(body.metrics)
  }
}

async function authenticateAgentRequest(
  request: FastifyRequest<any>,
  reply?: FastifyReply
): Promise<HostAgentRecord | null> {
  const headers = readAgentAuthHeaders(request.headers)
  if (!headers) {
    if (reply) reply.code(401).send({ error: 'Agent authentication headers are required', code: 'AGENT_AUTH_REQUIRED' })
    return null
  }

  const headerError = validateAgentHeaders(headers)
  if (headerError) {
    if (reply) reply.code(401).send({ error: 'Invalid Agent authentication headers', code: 'AGENT_AUTH_INVALID', details: headerError })
    return null
  }

  if (!isAgentTimestampFresh(headers.timestamp)) {
    if (reply) reply.code(401).send({ error: 'Agent request timestamp is expired', code: 'AGENT_AUTH_EXPIRED' })
    return null
  }

  const emptyBodyHash = createHash('sha256').update('').digest('hex')
  const bodyHash = (request.method === 'GET' && !request.body)
    ? emptyBodyHash
    : createAgentBodyHash(request.body ?? {})
  if (headers.bodyHash.toLowerCase() !== bodyHash.toLowerCase()) {
    if (reply) reply.code(401).send({ error: 'Agent body hash mismatch', code: 'AGENT_BODY_HASH_MISMATCH' })
    return null
  }

  const agent = await agentModel.findUnique({
    where: { agentId: headers.agentId }
  })

  if (!agent) {
    if (reply) reply.code(401).send({ error: 'Agent identity is not registered or has been rotated', code: 'AGENT_NOT_REGISTERED' })
    return null
  }
  if (!agent.enabled) {
    if (reply) reply.code(401).send({ error: 'Agent is not enabled', code: 'AGENT_DISABLED' })
    return null
  }

  const secret = decryptSensitiveData(agent.secretEncrypted)
  if (!secret || !isValidAgentSecret(secret)) {
    if (reply) reply.code(401).send({ error: 'Agent secret is not available', code: 'AGENT_SECRET_INVALID' })
    return null
  }

  const signatureOk = verifyAgentSignature(secret, {
    method: request.method,
    path: buildRequestPath(request),
    timestamp: headers.timestamp,
    nonce: headers.nonce,
    bodyHash: headers.bodyHash.toLowerCase()
  }, headers.signature)

  if (!signatureOk) {
    if (reply) reply.code(401).send({ error: 'Agent signature verification failed', code: 'AGENT_SIGNATURE_INVALID' })
    return null
  }

  try {
    await prisma.$transaction([
      nonceModel.deleteMany({
        where: {
          expiresAt: { lt: new Date() }
        }
      }),
      nonceModel.create({
        data: {
          agentId: agent.agentId,
          nonce: headers.nonce,
          expiresAt: new Date(Date.now() + agentNonceTtlMs)
        }
      })
    ])
  } catch (error) {
    if (error instanceof Prisma.PrismaClientKnownRequestError && error.code === 'P2002') {
      if (reply) reply.code(401).send({ error: 'Agent nonce was already used', code: 'AGENT_NONCE_REPLAY' })
      return null
    }
    throw error
  }

  return agent
}

export default async function agentRoutes(fastify: FastifyInstance) {
  const onControlReady = (hostId: number) => {
    void pushNetworkPolicy(hostId).catch(() => fastify.log.warn({ hostId }, 'Realtime network synchronization failed'))
  }
  hostTunnelManager.on('control_ready', onControlReady)
  fastify.addHook('onClose', async () => { hostTunnelManager.off('control_ready', onControlReady) })
  fastify.get('/install.sh', async (_request: FastifyRequest, reply: FastifyReply) => {
    const scriptPath = join(__dirname, '../../templates/install.sh')
    const script = readFileSync(scriptPath, 'utf8')
    return reply
      .header('Content-Type', 'text/x-shellscript; charset=utf-8')
      .header('Cache-Control', 'no-store')
      .send(script)
  })

  fastify.get('/distribution', async () => ({ repository: 'Rain-kl/incus', package: 'incus-payincus', channels: ['latest', 'canary'], installer: '/api/agent/install.sh', managedBy: 'apt' }))

  fastify.get<{ Params: AgentInstallTokenParams; Querystring: { format?: 'json' } }>('/install-config/:token', {
    schema: { querystring: { type: 'object', properties: { format: { type: 'string', enum: ['json'] } } } }
  }, async (
    request: FastifyRequest<{ Params: AgentInstallTokenParams; Querystring: { format?: 'json' } }>,
    reply: FastifyReply
  ) => {
    const { token } = request.params
    const jsonFormat = request.query.format === 'json'
    reply.header('Cache-Control', 'no-store')
    try {
      // Validate the certificate before consuming the one-time token.
      let panelCertificate: ReturnType<typeof readPanelClientCertificate> | undefined
      if (jsonFormat) {
        try {
          panelCertificate = readPanelClientCertificate()
        } catch (error) {
          return reply.code(503).send({
            code: 'PANEL_CERTIFICATE_UNAVAILABLE',
            error: `Panel client certificate unavailable: ${error instanceof Error ? error.message : String(error)}`
          })
        }
      }
      const result = await consumeHostAgentInstallToken(token)
      request.log.info(
        { hostId: result.host.id, agentId: result.agent.agentId },
        'Host Agent install token consumed'
      )
      await createLog(
        null,
        LogModule.HOST,
        'host.agent_install_token_consume',
        `宿主机 Agent 一次性安装 token 已消费: ${result.host.name} (#${result.host.id})`,
        LogResult.SUCCESS
      )

      if (panelCertificate) {
        return reply.send({
          schemaVersion: 1,
          agentId: result.agent.agentId,
          agentSecret: result.agentSecret,
          panelClientCertificate: panelCertificate.certificate,
          panelClientCertificateFingerprint: panelCertificate.fingerprint
        })
      }
      return reply
        .header('Content-Type', 'text/plain; charset=utf-8')
        .header('Cache-Control', 'no-store')
        .send(buildAgentInstallConfig({
          agentId: result.agent.agentId,
          agentSecret: result.agentSecret
        }))
    } catch (error) {
      if (error instanceof Error && error.message === 'AGENT_INSTALL_TOKEN_EXPIRED') {
        if (jsonFormat) return reply.code(403).send({ code: error.message, error: 'Agent install token has expired' })
        return reply.code(403).send('# Error: Agent install token has expired')
      }
      if (error instanceof Error && (
        error.message === 'AGENT_INSTALL_TOKEN_INVALID' ||
        error.message === 'AGENT_SECRET_INVALID'
      )) {
        if (jsonFormat) return reply.code(403).send({ code: error.message, error: 'Invalid Agent install token' })
        return reply.code(403).send('# Error: Invalid Agent install token')
      }
      throw error
    }
  })

  fastify.get<{ Params: AgentCredentialsParams }>('/hosts/:hostId/status', {
    onRequest: [fastify.authenticate]
  }, async (request: FastifyRequest<{ Params: AgentCredentialsParams }>, reply: FastifyReply) => {
    const hostId = parsePositiveId(request.params.hostId)
    if (!hostId) {
      return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
    }

    const host = await prisma.host.findUnique({
      where: { id: hostId },
      select: { id: true, name: true, userId: true }
    })
    if (!host) {
      return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
    }

    if (host.userId !== request.user.id && request.user.role !== 'admin') {
      return reply.code(403).send({ error: 'Forbidden', code: 'FORBIDDEN' })
    }

    const agent = await agentModel.findUnique({
      where: { hostId }
    })

    return {
      host: {
        id: host.id,
        name: host.name
      },
      agent: agent ? await serializeAgentStatus(agent) : null
    }
  })

  fastify.post<{ Params: AgentCredentialsParams; Body: AgentCredentialsBody }>('/hosts/:hostId/install-command', {
    onRequest: [fastify.authenticate],
    schema: {
      body: {
        type: 'object',
        additionalProperties: false,
        properties: {
          enabled: { type: 'boolean' }
        }
      }
    }
  }, async (
    request: FastifyRequest<{ Params: AgentCredentialsParams; Body: AgentCredentialsBody }>,
    reply: FastifyReply
  ) => {
    const hostId = parsePositiveId(request.params.hostId)
    if (!hostId) {
      return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
    }

    const host = await prisma.host.findUnique({
      where: { id: hostId },
      select: { id: true, name: true, userId: true }
    })
    if (!host) {
      return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
    }

    if (host.userId !== request.user.id && request.user.role !== 'admin') {
      return reply.code(403).send({ error: 'Forbidden', code: 'FORBIDDEN' })
    }

    const panelUrl = derivePanelUrl(request)
    let result: Awaited<ReturnType<typeof issueHostAgentInstallToken>>
    try {
      result = await issueHostAgentInstallToken(hostId, request.body?.enabled ?? true)
    } catch (error) {
      if (error instanceof Error && error.message === 'HOST_NOT_FOUND') {
        return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
      }
      throw error
    }
    const installCommand = buildNodeInstallCommand({
      panelUrl,
      installToken: result.installToken
    })

    await createLog(
      request.user.id,
      LogModule.HOST,
      'host.agent_install_command_generate',
      `生成宿主机 Agent 安装命令: ${host.name} (#${host.id})`,
      LogResult.SUCCESS
    )

    return reply.code(201).send({
      host: result.host,
      agent: await serializeAgentStatus(result.agent),
      installToken: result.installToken,
      installTokenExpiresAt: result.installTokenExpiresAt.toISOString(),
      installScriptUrl: `${panelUrl}/api/agent/install.sh`,
      installCommand,
      warning: 'installCommand 内包含一次性 Agent 安装 token，30 分钟内有效且只能使用一次。'
    })
  })

  fastify.get<{ Params: AgentCredentialsParams }>('/admin/hosts/:hostId/status', {
    onRequest: [fastify.authenticateAdmin]
  }, async (request: FastifyRequest<{ Params: AgentCredentialsParams }>, reply: FastifyReply) => {
    const hostId = parsePositiveId(request.params.hostId)
    if (!hostId) {
      return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
    }

    const host = await prisma.host.findUnique({
      where: { id: hostId },
      select: { id: true, name: true }
    })
    if (!host) {
      return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
    }

    const agent = await agentModel.findUnique({
      where: { hostId }
    })

    return {
      host,
      agent: agent ? await serializeAgent(agent) : null
    }
  })

  fastify.post<{ Params: AgentCredentialsParams; Body: AgentCredentialsBody }>('/admin/hosts/:hostId/credentials', {
    onRequest: [fastify.authenticateAdmin],
    schema: {
      body: {
        type: 'object',
        additionalProperties: false,
        properties: {
          enabled: { type: 'boolean' }
        }
      }
    }
  }, async (
    request: FastifyRequest<{ Params: AgentCredentialsParams; Body: AgentCredentialsBody }>,
    reply: FastifyReply
  ) => {
    const hostId = parsePositiveId(request.params.hostId)
    if (!hostId) {
      return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
    }

    let result: Awaited<ReturnType<typeof rotateHostAgentCredentials>>
    try {
      result = await rotateHostAgentCredentials(hostId, request.body?.enabled ?? true)
    } catch (error) {
      if (error instanceof Error && error.message === 'HOST_NOT_FOUND') {
        return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
      }
      throw error
    }

    await createLog(
      request.user.id,
      LogModule.HOST,
      'host.agent_credentials_rotate',
      `重置宿主机 Agent 凭据: ${result.host.name} (#${result.host.id})`,
      LogResult.SUCCESS
    )

    return reply.code(201).send({
      host: result.host,
      agent: await serializeAgent(result.agent),
      credentials: {
        agentId: result.agentId,
        agentSecret: result.agentSecret
      },
      warning: 'agentSecret 只会在本次响应中返回，请写入宿主机 Agent 配置后妥善保存。'
    })
  })

  fastify.post<{ Params: AgentCredentialsParams; Body: AgentInstallCommandBody }>('/admin/hosts/:hostId/install-command', {
    onRequest: [fastify.authenticateAdmin],
    schema: {
      body: {
        type: 'object',
        additionalProperties: false,
        properties: {
          enabled: { type: 'boolean' },
          baseUrl: { type: 'string', minLength: 1 },
          channel: { type: 'string', enum: ['latest', 'canary'] }
        }
      }
    }
  }, async (
    request: FastifyRequest<{ Params: AgentCredentialsParams; Body: AgentInstallCommandBody }>,
    reply: FastifyReply
  ) => {
    const hostId = parsePositiveId(request.params.hostId)
    if (!hostId) {
      return reply.code(400).send({ error: 'Invalid host ID', code: 'INVALID_HOST_ID' })
    }
    const baseUrlInput = request.body?.baseUrl
    if (typeof baseUrlInput === 'string' && !normalizeBaseUrl(baseUrlInput)) {
      return reply.code(400).send({
        error: 'baseUrl must be a valid HTTP(S) URL',
        code: 'INVALID_AGENT_BASE_URL'
      })
    }

    const panelUrl = derivePanelUrl(request, request.body?.baseUrl)
    let result: Awaited<ReturnType<typeof issueHostAgentInstallToken>>
    try {
      result = await issueHostAgentInstallToken(hostId, request.body?.enabled ?? true)
    } catch (error) {
      if (error instanceof Error && error.message === 'HOST_NOT_FOUND') {
        return reply.code(404).send({ error: 'Host not found', code: 'HOST_NOT_FOUND' })
      }
      throw error
    }

    const installCommand = buildNodeInstallCommand({
      panelUrl,
      installToken: result.installToken,
      channel: request.body?.channel
    })

    await createLog(
      request.user.id,
      LogModule.HOST,
      'host.agent_install_command_generate',
      `生成宿主机 Agent 安装命令: ${result.host.name} (#${result.host.id})`,
      LogResult.SUCCESS
    )

    return reply.code(201).send({
      host: result.host,
      agent: await serializeAgent(result.agent),
      installToken: result.installToken,
      installTokenExpiresAt: result.installTokenExpiresAt.toISOString(),
      installScriptUrl: `${panelUrl}/api/agent/install.sh`,
      installCommand,
      warning: 'installCommand 内包含一次性 Agent 安装 token，30 分钟内有效且只能使用一次。'
    })
  })

  fastify.post<{ Body: unknown }>('/security-report', { bodyLimit: 128 * 1024 }, async (request, reply) => {
    // Event batches must bind the actual body, independently of the GET WebSocket handshake.
    const signed = readAgentAuthHeaders(request.headers)
    if (!signed || signed.bodyHash?.toLowerCase() !== createAgentBodyHash(request.body ?? {}).toLowerCase()) return reply.code(401).send({ error: 'Report unavailable' })
    const agent = await authenticateAgentRequest(request, reply)
    if (!agent) return
    reply.header('Cache-Control', 'no-store')
    try { return { ack: await recordReport(agent.hostId, request.body) } }
    catch { return reply.code(503).send({ error: 'Report unavailable' }) }
  })

  fastify.post<{ Body: AgentHeartbeatBody }>('/heartbeat', {
    schema: {
      body: {
        type: 'object',
        additionalProperties: true,
        properties: {
          heartbeatRequestId: { type: 'string', pattern: '^[0-9a-f-]{36}$' },
          version: { type: 'string', maxLength: 128 },
          capabilities: {
            type: 'array',
            maxItems: 64,
            items: { type: 'string', maxLength: 80 }
          },
          runtime: { type: 'object', additionalProperties: true },
          incus: { type: 'object', additionalProperties: true },
          instances: { type: 'object', additionalProperties: true },
          resources: { type: 'object', additionalProperties: true },
          metrics: { type: 'object', additionalProperties: true }
        }
      }
    }
  }, async (request: FastifyRequest<{ Body: AgentHeartbeatBody }>, reply: FastifyReply) => {
    const agent = await authenticateAgentRequest(request, reply)
    if (!agent) {
      return
    }

    let reportError: string | undefined = '本次节点上报未完成，测试失败'
    // Only authenticated, persisted reports can complete a waiting test.
    reply.raw.once('finish', () => {
      hostTunnelManager.completeHeartbeat(agent.hostId, request.body.heartbeatRequestId, reply.statusCode >= 400 ? '本次节点上报未能保存，测试失败' : reportError)
    })
    if (request.body.nodeApi !== 'payincus_node_v1') return reply.code(409).send({ code: 'NODE_API_REQUIRED', error: 'Install the Incus node package' })
    const now = new Date()
    // Locked nodes remain online without turning an intentionally absent inventory into deletions.
    if (request.body.nodeSecurity?.state?.locked === true) {
      await agentModel.update({ where: { agentId: agent.agentId }, data: { status: 'online', version: sanitizeShortString(request.body.version, 128), capabilities: normalizeCapabilities(request.body.capabilities), lastSeenAt: now, lastHeartbeatIp: sanitizeShortString(getAgentHeartbeatIp(request), 128) } })
      const storageAccepted = await processAgentStorageReport(agent.hostId, request.body.storage, now)
      reportError = storageAccepted && request.body.storage?.available === true && request.body.storage?.collectionComplete === true ? undefined : '节点已连接，但锁定存储状态采集或保存未完成，测试失败'
      const connection = await prisma.host.findUnique({ where: { id: agent.hostId }, select: { tunnelEnabled: true, targetHost: true, targetPort: true } })
      return { ok: true, serverTime: now.toISOString(), tunnel: { enabled: connection?.tunnelEnabled === true || (Array.isArray(agent.capabilities) && agent.capabilities.includes('network-control')), targetHost: connection?.targetHost ?? '127.0.0.1', targetPort: connection?.targetPort ?? 8443 } }
    }
    let instanceReport: Awaited<ReturnType<typeof processAgentInstanceReport>> | null = null
    let instancePersisted = true
    let gpuPersisted = true
    try {
      instanceReport = await processAgentInstanceReport(agent.hostId, request.body.instances)
    } catch (error) {
      instancePersisted = false
      request.log.warn(
        { agentId: agent.agentId, hostId: agent.hostId, error },
        'Failed to process Agent instance report'
      )
    }

    await agentModel.update({
      where: { agentId: agent.agentId },
      data: {
        status: 'online',
        version: sanitizeShortString(request.body.version, 128),
        capabilities: normalizeCapabilities(request.body.capabilities) as Prisma.InputJsonValue,
        lastReport: buildHeartbeatReport(request.body, agent.lastReport) as Prisma.InputJsonObject,
        lastSeenAt: now,
        lastHeartbeatIp: sanitizeShortString(getAgentHeartbeatIp(request), 128)
      }
    })

    await syncHostGpuReport(agent.hostId, request.body.resources?.nvidiaGpu).catch(error => {
      gpuPersisted = false
      request.log.warn({ error, hostId: agent.hostId }, 'Failed to update NVIDIA GPU inventory')
    })

    const storageAccepted = await processAgentStorageReport(agent.hostId, request.body.storage, now)
    reportError = request.body.incus?.available !== true ? '节点已连接，但 Incus 状态采集失败，测试失败'
      : !storageAccepted || request.body.storage?.available !== true || request.body.storage?.collectionComplete !== true ? '节点存储状态采集或保存失败，测试失败'
      : !instancePersisted || request.body.instances?.available !== true || !instanceReport || instanceReport.errors > 0 ? '节点实例状态采集或保存失败，测试失败'
      : !gpuPersisted || request.body.resources?.storageAvailable !== true ? '节点硬件状态采集或保存失败，测试失败' : undefined

    if (request.body.incus?.available === true) {
      await prisma.host.updateMany({ where: { id: agent.hostId, status: 'offline' }, data: { status: 'online' } })
      // Enrollment already trusted this public client certificate; first heartbeat completes the panel record.
      const architecture = request.body.runtime?.goarch === 'arm64' ? 'aarch64' : request.body.runtime?.goarch === 'amd64' ? 'x86_64' : undefined
      await prisma.host.updateMany({ where: { id: agent.hostId, isInstalled: false }, data: { isInstalled: true, ...(architecture ? { architecture } : {}) } })
      const certificates = getPanelCertificatePaths()
      await prisma.host.updateMany({ where: { id: agent.hostId, certPath: null, keyPath: null }, data: certificates })
      const ipv6 = extractHostPublicIpv6FromResources(request.body.resources || {})
      if (ipv6) await prisma.host.updateMany({ where: { id: agent.hostId, natPublicIpv6: null }, data: { natPublicIpv6: ipv6 } })
      recordHostSuccess(agent.hostId, 0)
    }

    const host = await prisma.host.findUnique({
      where: { id: agent.hostId },
      select: { caddyEnabled: true, tunnelEnabled: true, targetHost: true, targetPort: true }
    })

    // Agent 上报 Caddy 已可用时，自动将宿主机标记为已启用（替代人工确认）。
    const caddyReported = typeof request.body.caddy === 'object' && request.body.caddy !== null
      ? (request.body.caddy as Record<string, unknown>)
      : {}
    const caddyAvailable = caddyReported.available === true
    let caddyEnabled = host?.caddyEnabled ?? false
    if (caddyAvailable && !caddyEnabled) {
      await prisma.host.update({
        where: { id: agent.hostId },
        data: { caddyEnabled: true }
      })
      caddyEnabled = true
      caddyInstallRequested.delete(agent.hostId)
      request.log.info({ agentId: agent.agentId, hostId: agent.hostId }, '[Caddy] Agent reported Caddy available; auto-enabled host')
    }

    // Agent 上报 Caddy 已移除时，自动将宿主机标记为未启用（卸载完成）。
    const caddyUninstallRequestedNow = caddyUninstallRequested.has(agent.hostId)
    if (isRecord(request.body.caddy) && caddyUninstallRequestedNow && caddyEnabled && !caddyAvailable) {
      await prisma.host.update({
        where: { id: agent.hostId },
        data: { caddyEnabled: false }
      })
      caddyEnabled = false
      caddyUninstallRequested.delete(agent.hostId)
      caddyInstallRequested.delete(agent.hostId)
      request.log.info({ agentId: agent.agentId, hostId: agent.hostId }, '[Caddy] Agent reported Caddy removed; auto-disabled host')
    }

    const caddyPort = typeof caddyReported.port === 'number' && caddyReported.port > 0
      ? Math.min(65535, Math.floor(caddyReported.port))
      : 2019

    // 指令优先级：卸载 > 安装 > 空转。均由面板显式触发（安装/卸载按钮）。
    let caddyCommand = 'idle'
    if (caddyUninstallRequestedNow) {
      caddyCommand = 'uninstall'
    } else if (!caddyEnabled && caddyInstallRequested.has(agent.hostId)) {
      caddyCommand = 'install'
    }

    if (request.body.networkStatus) await recordNetworkStatus(agent.hostId, request.body.networkStatus)
    return {
      ok: true,
      serverTime: now.toISOString(),
      taskPollIntervalSeconds: 15,
      instanceReport,
      caddy: {
        command: caddyCommand,
        port: caddyCommand === 'install' ? caddyPort : 0
      },
      tunnel: {
        // 实时控制复用此通道；Incus 的直连/穿透模式仍由宿主设置决定。
        enabled: request.body.capabilities?.includes('network-control') === true || (host?.tunnelEnabled ?? true),
        targetHost: host?.targetHost ?? '127.0.0.1',
        targetPort: host?.targetPort ?? 8443
      }
    }
  })

  // 宿主机 Agent 反向 WebSocket 隧道
  fastify.get('/tunnel', {
    websocket: true
  }, async (socket, request) => {
    const agent = await authenticateAgentRequest(request)
    if (!agent) {
      try {
        socket.close(4001, 'Agent authentication failed')
      } catch {
        // ignore
      }
      return
    }

    const host = await prisma.host.findUnique({
      where: { id: agent.hostId },
      select: { id: true, tunnelEnabled: true, targetHost: true, targetPort: true }
    })

    // 直连 Incus 的宿主也可建立经过 HMAC 认证的实时控制通道。
    if (!host || (host.tunnelEnabled === false && !(Array.isArray(agent.capabilities) && agent.capabilities.includes('network-control')))) {
      try {
        socket.close(4003, 'Tunnel mode is disabled for this host')
      } catch {
        // ignore
      }
      return
    }

    hostTunnelManager.registerTunnel(agent.hostId, socket as any)
  })
}
