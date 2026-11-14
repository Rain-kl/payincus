import type { FastifyInstance } from 'fastify'
import { createLog } from '../db/logs.js'
import { prisma } from '../db/prisma.js'
import { allocatedGpuIds, countGpuReservations, formatInstanceGpuSummary, listHostGpus, updateInstanceGpuAllocation } from '../db/gpus.js'
import { hasPendingTransfer } from '../db/transfers.js'
import { getActiveTaskForInstance } from '../db/instance-tasks.js'
import { applyGpuDevice, clearGpuDevices, GpuError, GPU_SHARING_NOTICE, listIncusGpuDevices, normalizeGpuInput } from '../lib/gpu.js'
import { IncusApiError, nodeErrorDetail } from '../lib/incus/incus-client.js'
import { getInstance } from '../lib/incus/incus-instances.js'
import { getIncusClient } from '../lib/incus/incus-pool.js'
import type { IncusInstance } from '../types/incus.js'

const idSchema = { type: 'integer', minimum: 1, maximum: 2147483647 }

async function buildInstanceGpuResponse(
  instanceId: number,
  includeManagement: boolean,
  liveOverride?: { live: IncusInstance | null; syncError: string | null }
) {
  const instance = await prisma.instance.findUnique({
    where: { id: instanceId },
    include: {
      gpuDevice: true,
      gpuAllocations: { include: { gpuDevice: true }, orderBy: { gpuDeviceId: 'asc' } },
      host: true,
      package: { select: { instanceType: true, privileged: true } }
    }
  })
  if (!instance || instance.status === 'deleted') return null

  const devices = instance.gpuAllocations.length
    ? instance.gpuAllocations.map(allocation => allocation.gpuDevice)
    : instance.gpuDevice
      ? [instance.gpuDevice]
      : []
  const gpu = formatInstanceGpuSummary(devices, Boolean(instance.gpuReleasedAt))
  if (!includeManagement) return { gpu }

  const hostGpus = await listHostGpus(instance.hostId)
  let live: IncusInstance | null = liveOverride ? liveOverride.live : null
  let liveSyncError: string | null = liveOverride ? liveOverride.syncError : null

  if (!liveOverride) {
    try {
      const client = await getIncusClient(instance.host as any)
      live = await getInstance(client, instance.incusId)
    } catch (error) {
      liveSyncError = nodeErrorDetail(error, '无法读取节点实例 GPU 配置')
    }
  }

  const effectiveInstanceType: 'container' | 'vm' = live
    ? (live.type === 'virtual-machine' ? 'vm' : 'container')
    : (instance.package?.instanceType === 'vm' ? 'vm' : 'container')
  const privileged = live
    ? live.config?.['security.privileged'] === 'true'
    : Boolean(instance.package?.privileged)
  const hostPciSet = new Set(hostGpus.map(card => card.pciAddress.toLowerCase()))
  const liveGpuDevices = live
    ? listIncusGpuDevices(live).map(dev => ({
        ...dev,
        hostMatched: Boolean(dev.pci && hostPciSet.has(dev.pci.toLowerCase()))
      }))
    : null

  return {
    gpu,
    management: {
      canManage: true,
      instanceId: instance.id,
      hostId: instance.hostId,
      instanceStatus: instance.status,
      instanceType: effectiveInstanceType,
      privileged,
      supported: effectiveInstanceType === 'container' && !privileged,
      allocatedDeviceIds: allocatedGpuIds(instance),
      reservationReleased: Boolean(instance.gpuReleasedAt),
      hostGpus,
      liveGpuDevices,
      liveSyncError
    }
  }
}

export default async function gpuRoutes(fastify: FastifyInstance) {
  fastify.get<{ Params: { id: number } }>('/hosts/:id/gpus', {
    onRequest: [fastify.authenticate], schema: { params: { type: 'object', required: ['id'], properties: { id: idSchema } } }
  }, async (request, reply) => {
    const host = await prisma.host.findUnique({ where: { id: request.params.id }, select: { userId: true, agent: { select: { enabled: true, version: true, lastSeenAt: true } } } })
    if (!host) return reply.code(404).send({ error: '宿主机不存在' })
    if (request.user.role !== 'admin' && request.user.id !== host.userId) return reply.code(403).send({ error: '无权访问该宿主机' })
    return { gpus: await listHostGpus(request.params.id), agent: host.agent, sharingNotice: GPU_SHARING_NOTICE }
  })
  fastify.put<{ Params: { id: number; gpuId: number }; Body: { sharingEnabled: boolean; maxInstances: number } }>('/hosts/:id/gpus/:gpuId', {
    onRequest: [fastify.authenticate], schema: {
      params: { type: 'object', required: ['id', 'gpuId'], properties: { id: idSchema, gpuId: idSchema } },
      body: { type: 'object', additionalProperties: false, required: ['sharingEnabled', 'maxInstances'], properties: { sharingEnabled: { type: 'boolean' }, maxInstances: { type: 'integer', minimum: 1, maximum: 1000 } } }
    }
  }, async (request, reply) => {
    const host = await prisma.host.findUnique({ where: { id: request.params.id }, select: { userId: true, instanceType: true } })
    if (!host) return reply.code(404).send({ error: '宿主机不存在' })
    if (request.user.role !== 'admin' && request.user.id !== host.userId) return reply.code(403).send({ error: '无权修改该宿主机' })
    if (host.instanceType === 'vm' && request.body.sharingEnabled) return reply.code(400).send({ error: '共享 GPU 仅支持 LXC 宿主机' })
    const result = await prisma.$transaction(async tx => {
      await tx.$queryRaw`SELECT id FROM host_gpus WHERE id = ${request.params.gpuId} AND host_id = ${request.params.id} FOR UPDATE`
      const gpu = await tx.hostGpu.findFirst({ where: { id: request.params.gpuId, hostId: request.params.id } })
      if (!gpu) return 'not_found'
      const used = await countGpuReservations(tx, gpu.id)
      if (used > request.body.maxInstances) return 'in_use'
      await tx.hostGpu.update({ where: { id: gpu.id }, data: request.body })
      return 'ok'
    })
    if (result === 'not_found') return reply.code(404).send({ error: 'GPU 不存在' })
    if (result === 'in_use') return reply.code(409).send({ error: '共享名额不能低于当前已分配实例数（包含已停止和封停实例）' })
    await createLog(request.user.id, 'host', 'host.gpu.configure', `GPU ${request.params.gpuId}: sharing=${request.body.sharingEnabled}, maxInstances=${request.body.maxInstances}`, 'success')
    return { success: true }
  })
  fastify.get<{ Params: { id: number } }>('/instances/:id/gpu', {
    onRequest: [fastify.authenticate], schema: { params: { type: 'object', required: ['id'], properties: { id: idSchema } } }
  }, async (request, reply) => {
    const instance = await prisma.instance.findUnique({ where: { id: request.params.id }, include: { host: { select: { userId: true } } } })
    if (!instance || instance.status === 'deleted') return reply.code(404).send({ error: '实例不存在' })
    const isManager = request.user.role === 'admin' || request.user.id === instance.host.userId
    if (!isManager && request.user.id !== instance.userId) return reply.code(403).send({ error: '无权访问该实例' })
    return await buildInstanceGpuResponse(instance.id, isManager)
  })

  fastify.put<{ Params: { id: number }; Body: { gpuShared: boolean; gpuDeviceIds?: number[] } }>('/instances/:id/gpu', {
    onRequest: [fastify.authenticate],
    schema: {
      params: { type: 'object', required: ['id'], properties: { id: idSchema } },
      body: {
        type: 'object',
        additionalProperties: false,
        required: ['gpuShared'],
        properties: {
          gpuShared: { type: 'boolean' },
          gpuDeviceIds: { type: 'array', items: idSchema, maxItems: 64 }
        }
      }
    }
  }, async (request, reply) => {
    const instance = await prisma.instance.findUnique({
      where: { id: request.params.id },
      include: { host: true }
    })
    if (!instance || instance.status === 'deleted') return reply.code(404).send({ error: '实例不存在' })
    const isManager = request.user.role === 'admin' || request.user.id === instance.host.userId
    if (!isManager) return reply.code(403).send({ error: '无权修改该实例的 GPU 共享配置' })

    if (await hasPendingTransfer(instance.id)) {
      return reply.code(400).send({ error: '实例正在转移中，暂时无法修改 GPU 配置', code: 'TRANSFER_INSTANCE_LOCKED' })
    }
    const [activeRestoreTask, activeUploadTask, activeInstanceTask] = await Promise.all([
      prisma.restoreTask.findFirst({ where: { instanceId: instance.id, status: { in: ['PENDING', 'PROCESSING'] } }, select: { id: true } }),
      prisma.backupUploadTask.findFirst({ where: { instanceId: instance.id, status: { in: ['PENDING', 'PROCESSING'] } }, select: { id: true } }),
      getActiveTaskForInstance(instance.id)
    ])
    if (activeRestoreTask || activeUploadTask || activeInstanceTask) {
      return reply.code(409).send({ error: '实例有正在执行的任务，请等待完成后再修改 GPU 配置', code: 'TASK_IN_PROGRESS' })
    }

    let normalized: { gpuShared?: boolean | null; gpuDeviceIds?: number[] }
    try {
      normalized = normalizeGpuInput(request.body as Record<string, unknown>)
    } catch (error) {
      if (error instanceof GpuError) return reply.code(error.statusCode).send({ error: error.message, code: error.code })
      throw error
    }
    const gpuShared = normalized.gpuShared === true
    const gpuDeviceIds = gpuShared ? (normalized.gpuDeviceIds ?? []) : []

    let client: Awaited<ReturnType<typeof getIncusClient>>
    let live: IncusInstance | null = null
    try {
      client = await getIncusClient(instance.host as any)
    } catch (error) {
      return reply.code(502).send({ error: nodeErrorDetail(error, '无法连接宿主机以更新 GPU 配置') })
    }

    try {
      live = await getInstance(client, instance.incusId)
    } catch (error) {
      const missingOnNode = error instanceof IncusApiError && error.statusCode === 404
      if (!(!gpuShared && missingOnNode && instance.status === 'error')) {
        const statusCode = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
        return reply.code(statusCode).send({ error: nodeErrorDetail(error, '读取节点实例配置失败') })
      }
    }

    const liveInstanceType: 'container' | 'vm' | null = live
      ? (live.type === 'virtual-machine' ? 'vm' : 'container')
      : null
    const livePrivileged: boolean | null = live
      ? live.config?.['security.privileged'] === 'true'
      : null

    let incusUpdated = false
    try {
      const { pciAddresses } = await updateInstanceGpuAllocation({
        instanceId: instance.id,
        gpuShared,
        gpuDeviceIds,
        liveInstanceType,
        livePrivileged,
        syncIncus: async (pcis) => {
          if (!live) return
          if (!pcis) {
            if (!clearGpuDevices(live)) return
          } else {
            applyGpuDevice(live, pcis)
          }
          try {
            await client.request('PUT', `/1.0/instances/${encodeURIComponent(instance.incusId)}`, live)
            incusUpdated = true
          } catch (error) {
            const detail = error instanceof Error ? error.message : String(error)
            if (instance.status === 'running' && /running|stopped|hotplug/i.test(detail)) {
              throw new GpuError(
                pcis ? '实例正在运行且节点不支持热挂载 GPU，请先停止实例后再分配' : '实例正在运行且节点不支持热卸载 GPU，请先停止实例后再取消共享',
                409,
                'INSTANCE_RUNNING'
              )
            }
            const status = error instanceof IncusApiError && [400, 403, 404, 409, 503].includes(error.statusCode) ? error.statusCode : 502
            throw new GpuError(nodeErrorDetail(error, '同步节点 Incus GPU 配置失败'), status)
          }
        }
      })

      await createLog(
        request.user.id,
        'instance',
        'instance.gpu.configure',
        pciAddresses
          ? `Instance #${instance.id} (${instance.incusId}) assigned shared GPU PCIs: ${pciAddresses.join(', ')}`
          : `Instance #${instance.id} (${instance.incusId}) cleared shared GPU allocation`,
        'success',
        { instanceId: instance.id }
      )

      const payload = await buildInstanceGpuResponse(instance.id, true, { live, syncError: null })
      return {
        success: true,
        restartRequired: instance.status === 'running' && incusUpdated,
        ...payload
      }
    } catch (error) {
      if (error instanceof GpuError) {
        return reply.code(error.statusCode).send({ error: error.message, code: error.code })
      }
      throw error
    }
  })
}
