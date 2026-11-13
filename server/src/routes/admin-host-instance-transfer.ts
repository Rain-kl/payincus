import { randomBytes } from 'node:crypto'
import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { LogModule, LogResult } from '../db/logs.js'
import { IncusApiError, nodeErrorDetail } from '../lib/incus/incus-client.js'
import { getIncusClient } from '../lib/incus/incus-pool.js'
import { getInstance, getInstanceState, listInstances } from '../lib/incus/incus-instances.js'
import type { IncusInstance } from '../types/incus.js'

const TRANSFER_EXT = 'incus_instance_transfer_v1'
const BACKUPS_ENDPOINT = '/1.0/agent/instance-backups'
const IMPORT_ENDPOINT = '/1.0/agent/instance-import'

const hostIdParams = {
  type: 'object',
  required: ['hostId'],
  properties: {
    hostId: { type: 'integer', minimum: 1, maximum: 2147483647 }
  }
}

function generateTaskId(): string {
  return randomBytes(16).toString('hex')
}

function parseSizeToMB(raw: unknown, fallbackMB: number): number {
  if (typeof raw !== 'string' || !raw.trim()) return fallbackMB
  const match = raw.trim().match(/^(\d+(?:\.\d+)?)\s*(B|KB|KIB|MB|MIB|GB|GIB|TB|TIB)?$/i)
  if (!match) return fallbackMB
  const num = Number(match[1])
  if (!Number.isFinite(num) || num <= 0) return fallbackMB
  const unit = (match[2] || 'B').toUpperCase()
  switch (unit) {
    case 'B':
      return Math.max(1, Math.round(num / (1024 * 1024)))
    case 'KB':
    case 'KIB':
      return Math.max(1, Math.round(num / 1024))
    case 'MB':
    case 'MIB':
      return Math.max(1, Math.round(num))
    case 'GB':
    case 'GIB':
      return Math.max(1, Math.round(num * 1024))
    case 'TB':
    case 'TIB':
      return Math.max(1, Math.round(num * 1024 * 1024))
    default:
      return fallbackMB
  }
}

function parseCpuPercentage(config: Record<string, unknown>): number {
  const allowance = typeof config['limits.cpu.allowance'] === 'string' ? config['limits.cpu.allowance'].trim() : ''
  if (allowance.endsWith('%')) {
    const val = Number.parseInt(allowance.slice(0, -1), 10)
    if (Number.isFinite(val) && val > 0) return val
  }
  const coresRaw = typeof config['limits.cpu'] === 'string' ? config['limits.cpu'].trim() : ''
  const cores = Number.parseInt(coresRaw, 10)
  if (Number.isFinite(cores) && cores > 0) return cores * 100
  return 100
}

function stripCidr(addr: string | undefined): string | null {
  if (!addr) return null
  const clean = addr.split('/')[0]?.trim()
  return clean || null
}

function extractNetworkDetails(
  inst: IncusInstance,
  state: unknown
): { ipv4: string | null; ipv6: string | null; networkMode: 'nat' | 'ipv6_only' | 'nat_ipv6' } {
  const devices = (inst.expanded_devices ?? inst.devices ?? {}) as Record<string, Record<string, unknown>>
  let ipv4: string | null = null
  let ipv6: string | null = null

  for (const dev of Object.values(devices)) {
    if (dev?.type !== 'nic') continue
    if (!ipv4 && typeof dev['ipv4.address'] === 'string') {
      ipv4 = stripCidr(dev['ipv4.address'])
    }
    if (!ipv6 && typeof dev['ipv6.address'] === 'string') {
      ipv6 = stripCidr(dev['ipv6.address'])
    }
  }

  const network = (state as { network?: Record<string, { addresses?: Array<{ family?: string; scope?: string; address?: string }> }> } | null)?.network
  if (network) {
    for (const [ifName, ifState] of Object.entries(network)) {
      if (ifName === 'lo' || !Array.isArray(ifState.addresses)) continue
      for (const addr of ifState.addresses) {
        if (addr.scope === 'link' || addr.scope === 'host' || !addr.address) continue
        if (addr.family === 'inet' && !ipv4) ipv4 = addr.address
        if (addr.family === 'inet6' && !ipv6) ipv6 = addr.address
      }
    }
  }

  let networkMode: 'nat' | 'ipv6_only' | 'nat_ipv6' = 'nat'
  if (ipv6 && !ipv4) networkMode = 'ipv6_only'
  else if (ipv6 && ipv4) networkMode = 'nat_ipv6'

  return { ipv4, ipv6, networkMode }
}

function extractDiskMB(inst: IncusInstance): { disk: number; storagePoolName: string | null } {
  const devices = (inst.expanded_devices ?? inst.devices ?? {}) as Record<string, Record<string, unknown>>
  const root = devices.root
  const disk = parseSizeToMB(root?.size, 10240)
  const storagePoolName = typeof root?.pool === 'string' && root.pool.trim() ? root.pool.trim() : null
  return { disk, storagePoolName }
}

function mapIncusStatus(status: string): 'running' | 'stopped' {
  return status.toLowerCase() === 'running' ? 'running' : 'stopped'
}

async function ensureHostTransferSupport(hostId: number) {
  const host = await prisma.host.findUnique({ where: { id: hostId } })
  if (!host) return { error: { code: 404, message: '节点不存在' } as const }
  const client = await getIncusClient(host)
  const info = await client.request<{ api_extensions?: string[] }>('GET', '/1.0/agent')
  const supported = Array.isArray(info.api_extensions) && info.api_extensions.includes(TRANSFER_EXT)
  return { host, client, supported }
}

export default async function adminHostInstanceTransferRoutes(app: FastifyInstance) {
  const auth = [app.authenticate, app.requireAdmin]

  // 查询导出/导入任务列表
  app.get<{ Params: { hostId: number } }>('/api/admin/hosts/:hostId/instance-transfer', {
    onRequest: auth,
    schema: { params: hostIdParams },
    handler: async (req, reply) => {
      reply.header('Cache-Control', 'no-store')
      try {
        const res = await ensureHostTransferSupport(req.params.hostId)
        if ('error' in res && res.error) return reply.code(res.error.code).send({ error: res.error.message })
        if (!res.supported) return { supported: false, tasks: [] }
        const data = await res.client.request<{ tasks?: unknown[] }>('GET', BACKUPS_ENDPOINT)
        return { supported: true, tasks: Array.isArray(data?.tasks) ? data.tasks : [] }
      } catch (error) {
        const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
        return reply.code(status).send({ error: nodeErrorDetail(error, '获取节点迁移任务失败') })
      }
    }
  })

  // 导出单个实例到宿主机指定位置
  app.post<{
    Params: { hostId: number; instanceId: number }
    Body: { id?: string; targetPath: string; mode?: 'directory' | 'file' }
  }>('/api/admin/hosts/:hostId/instances/:instanceId/export', {
    onRequest: auth,
    schema: {
      params: {
        type: 'object',
        required: ['hostId', 'instanceId'],
        properties: {
          hostId: { type: 'integer', minimum: 1, maximum: 2147483647 },
          instanceId: { type: 'integer', minimum: 1, maximum: 2147483647 }
        }
      },
      body: {
        type: 'object',
        additionalProperties: false,
        required: ['targetPath'],
        properties: {
          id: { type: 'string', pattern: '^[0-9a-f]{32}$' },
          targetPath: { type: 'string', minLength: 2, maxLength: 4096, pattern: '^/[^\\u0000\\r\\n]+$' },
          mode: { type: 'string', enum: ['directory', 'file'] }
        }
      }
    },
    handler: async (req, reply) => {
      reply.header('Cache-Control', 'no-store')
      const { hostId, instanceId } = req.params
      const instance = await prisma.instance.findFirst({
        where: { id: instanceId, hostId, status: { not: 'deleted' } }
      })
      if (!instance) return reply.code(404).send({ error: '实例不存在或不属于该宿主机' })

      try {
        const res = await ensureHostTransferSupport(hostId)
        if ('error' in res && res.error) return reply.code(res.error.code).send({ error: res.error.message })
        if (!res.supported) return reply.code(409).send({ error: '节点程序版本暂不支持单实例导出，请先升级节点程序包' })

        const targetPath = req.body.targetPath.trim()
        const isFileMode = req.body.mode === 'file'
          || (!req.body.mode && /\.(tar\.gz|tgz|tar\.zst|tar)$/i.test(targetPath))
        const payload = {
          id: req.body.id || generateTaskId(),
          instance: instance.incusId,
          ...(isFileMode ? { filePath: targetPath } : { directory: targetPath })
        }

        const task = await res.client.request('POST', BACKUPS_ENDPOINT, payload)
        await prisma.log.create({
          data: {
            userId: req.user.id,
            module: LogModule.HOST,
            action: 'host.instance.export',
            content: `节点 #${hostId} 管理员导出实例 ${instance.name} (${instance.incusId}) 至 ${targetPath}`,
            result: LogResult.SUCCESS
          }
        })
        return task
      } catch (error) {
        const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
        return reply.code(status).send({ error: nodeErrorDetail(error, '导出实例失败') })
      }
    }
  })

  // 从宿主机指定文件路径导入实例
  app.post<{
    Params: { hostId: number }
    Body: { id?: string; filePath: string; name?: string; storagePool?: string }
  }>('/api/admin/hosts/:hostId/instances/import', {
    onRequest: auth,
    schema: {
      params: hostIdParams,
      body: {
        type: 'object',
        additionalProperties: false,
        required: ['filePath'],
        properties: {
          id: { type: 'string', pattern: '^[0-9a-f]{32}$' },
          filePath: { type: 'string', minLength: 2, maxLength: 4096, pattern: '^/[^\\u0000\\r\\n]+$' },
          name: { type: 'string', minLength: 1, maxLength: 63, pattern: '^[a-zA-Z0-9][a-zA-Z0-9-]*$' },
          storagePool: { type: 'string', minLength: 1, maxLength: 64 }
        }
      }
    },
    handler: async (req, reply) => {
      reply.header('Cache-Control', 'no-store')
      const { hostId } = req.params
      try {
        const res = await ensureHostTransferSupport(hostId)
        if ('error' in res && res.error) return reply.code(res.error.code).send({ error: res.error.message })
        if (!res.supported) return reply.code(409).send({ error: '节点程序版本暂不支持实例导入，请先升级节点程序包' })

        const payload: Record<string, string> = {
          id: req.body.id || generateTaskId(),
          filePath: req.body.filePath.trim()
        }
        if (req.body.name?.trim()) payload.name = req.body.name.trim()
        if (req.body.storagePool?.trim()) payload.storagePool = req.body.storagePool.trim()

        const task = await res.client.request('POST', IMPORT_ENDPOINT, payload)
        await prisma.log.create({
          data: {
            userId: req.user.id,
            module: LogModule.HOST,
            action: 'host.instance.import',
            content: `节点 #${hostId} 管理员从路径 ${payload.filePath} 导入实例${payload.name ? ` (${payload.name})` : ''}`,
            result: LogResult.SUCCESS
          }
        })
        return task
      } catch (error) {
        const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
        return reply.code(status).send({ error: nodeErrorDetail(error, '导入实例失败') })
      }
    }
  })

  // 获取宿主机上无归属（未纳入面板数据库）的实例列表
  app.get<{ Params: { hostId: number } }>('/api/admin/hosts/:hostId/unassigned-instances', {
    onRequest: auth,
    schema: { params: hostIdParams },
    handler: async (req, reply) => {
      reply.header('Cache-Control', 'no-store')
      const { hostId } = req.params
      const host = await prisma.host.findUnique({ where: { id: hostId } })
      if (!host) return reply.code(404).send({ error: '节点不存在' })

      try {
        const client = await getIncusClient(host)
        const [incusInstances, dbInstances] = await Promise.all([
          listInstances(client),
          prisma.instance.findMany({
            where: { hostId, status: { not: 'deleted' } },
            select: { incusId: true }
          })
        ])
        const managedNames = new Set(dbInstances.map(i => i.incusId))

        const unassigned = incusInstances
          .filter(inst => !managedNames.has(inst.name))
          .map(inst => {
            const config = (inst.expanded_config ?? inst.config ?? {}) as Record<string, unknown>
            const { disk, storagePoolName } = extractDiskMB(inst)
            const { ipv4, ipv6, networkMode } = extractNetworkDetails(inst, null)
            return {
              incusName: inst.name,
              instanceType: inst.type === 'virtual-machine' ? 'vm' : 'container',
              status: mapIncusStatus(inst.status),
              cpu: parseCpuPercentage(config),
              memory: parseSizeToMB(config['limits.memory'], 512),
              disk,
              storagePoolName,
              ipv4,
              ipv6,
              networkMode,
              image: String(config['image.description'] || config['image.os'] || 'imported'),
              createdAt: (inst as unknown as { created_at?: string }).created_at ?? null
            }
          })

        return { instances: unassigned }
      } catch (error) {
        const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
        return reply.code(status).send({ error: nodeErrorDetail(error, '读取宿主机无归属实例失败') })
      }
    }
  })

  // 将宿主机上的无归属实例分配给指定用户
  app.post<{
    Params: { hostId: number; incusName: string }
    Body: { userId: number; name?: string }
  }>('/api/admin/hosts/:hostId/unassigned-instances/:incusName/assign', {
    onRequest: auth,
    schema: {
      params: {
        type: 'object',
        required: ['hostId', 'incusName'],
        properties: {
          hostId: { type: 'integer', minimum: 1, maximum: 2147483647 },
          incusName: { type: 'string', minLength: 1, maxLength: 63, pattern: '^[a-zA-Z0-9][a-zA-Z0-9-]*$' }
        }
      },
      body: {
        type: 'object',
        additionalProperties: false,
        required: ['userId'],
        properties: {
          userId: { type: 'integer', minimum: 1, maximum: 2147483647 },
          name: { type: 'string', minLength: 1, maxLength: 64 }
        }
      }
    },
    handler: async (req, reply) => {
      const { hostId, incusName } = req.params
      const { userId } = req.body

      const [host, targetUser, existing] = await Promise.all([
        prisma.host.findUnique({ where: { id: hostId } }),
        prisma.user.findUnique({ where: { id: userId }, select: { id: true, username: true, email: true } }),
        prisma.instance.findFirst({ where: { hostId, incusId: incusName, status: { not: 'deleted' } } })
      ])

      if (!host) return reply.code(404).send({ error: '节点不存在' })
      if (!targetUser) return reply.code(404).send({ error: '目标用户不存在' })
      if (existing) return reply.code(409).send({ error: '该实例已归属于面板用户，无需重复分配' })

      try {
        const client = await getIncusClient(host)
        const [incusInst, incusState] = await Promise.all([
          getInstance(client, incusName),
          getInstanceState(client, incusName).catch(() => null)
        ])

        const config = (incusInst.expanded_config ?? incusInst.config ?? {}) as Record<string, unknown>
        const cpu = parseCpuPercentage(config)
        const memory = parseSizeToMB(config['limits.memory'], 512)
        const { disk, storagePoolName } = extractDiskMB(incusInst)
        const { ipv4, ipv6, networkMode } = extractNetworkDetails(incusInst, incusState)
        const status = mapIncusStatus(incusInst.status)
        const displayName = req.body.name?.trim() || incusName
        const image = String(config['image.description'] || config['image.os'] || 'imported')

        const created = await prisma.$transaction(async (tx) => {
          const dup = await tx.instance.findFirst({
            where: { hostId, incusId: incusName, status: { not: 'deleted' } }
          })
          if (dup) throw new Error('ALREADY_ASSIGNED')

          const createdInstance = await tx.instance.create({
            data: {
              incusId: incusName,
              name: displayName,
              userId: targetUser.id,
              hostId,
              storagePoolName,
              image,
              status,
              cpu,
              memory,
              disk,
              ipv4,
              ipv6,
              networkMode
            }
          })

          await tx.log.create({
            data: {
              userId: req.user.id,
              module: LogModule.HOST,
              action: 'host.instance.assign',
              content: `节点 #${hostId} 管理员将无归属实例 ${incusName} 分配给用户 ${targetUser.username} (#${targetUser.id})`,
              result: LogResult.SUCCESS
            }
          })

          return createdInstance
        })

        return { instance: created }
      } catch (error) {
        if (error instanceof Error && error.message === 'ALREADY_ASSIGNED') {
          return reply.code(409).send({ error: '该实例已归属于面板用户' })
        }
        const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
        return reply.code(status).send({ error: nodeErrorDetail(error, '分配无归属实例失败') })
      }
    }
  })
}
