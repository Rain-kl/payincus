import type { FastifyInstance } from 'fastify'
import { createLog } from '../db/logs.js'
import { prisma } from '../db/prisma.js'
import { countGpuReservations, listHostGpus } from '../db/gpus.js'
import { GPU_SHARING_NOTICE, summarizeGpuModels } from '../lib/gpu.js'

const idSchema = { type: 'integer', minimum: 1, maximum: 2147483647 }
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
    const instance = await prisma.instance.findUnique({ where: { id: request.params.id }, include: { gpuDevice: true, gpuAllocations: { include: { gpuDevice: true }, orderBy: { gpuDeviceId: 'asc' } }, host: { select: { userId: true } } } })
    if (!instance || instance.status === 'deleted') return reply.code(404).send({ error: '实例不存在' })
    if (request.user.role !== 'admin' && request.user.id !== instance.userId && request.user.id !== instance.host.userId) return reply.code(403).send({ error: '无权访问该实例' })
    const devices = instance.gpuAllocations.length ? instance.gpuAllocations.map(allocation => allocation.gpuDevice) : instance.gpuDevice ? [instance.gpuDevice] : []
    return { gpu: devices.length ? { name: devices[0].name, memoryTotalMiB: devices[0].memoryTotalMiB, devices: summarizeGpuModels(devices), count: devices.length, shared: true, reservationReleased: Boolean(instance.gpuReleasedAt), sharingNotice: GPU_SHARING_NOTICE } : null }
  })
}
