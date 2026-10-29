import type { FastifyInstance } from 'fastify'
import { Prisma } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { createLog } from '../db/logs.js'
import { checkInstancePermission } from '../lib/permission.js'
import { validateEgress, NetworkInputError } from '../lib/egress/policy.js'
import { bumpEgressHost } from '../lib/egress/package.js'
import { pushNetworkPolicy } from '../lib/egress/realtime.js'
import { assertSafeHttpUrl } from '../lib/outbound-security.js'

const id = { type: 'integer', minimum: 1, maximum: 2147483647 }
const fields = { name: { type: 'string', minLength: 1, maxLength: 80, pattern: '\\S' }, hostIds: { type: 'array', minItems: 1, maxItems: 1000, uniqueItems: true, items: id }, egress: { type: 'object', additionalProperties: true } }
export default async function egressProfileRoutes(app: FastifyInstance) {
  app.get<{ Params: { id: number } }>('/api/instances/:id/egress', {
    onRequest: [app.authenticate], schema: { params: { type: 'object', required: ['id'], properties: { id } } }
  }, async (request, reply) => {
    const instance = await prisma.instance.findUnique({ where: { id: request.params.id }, include: { egressProfile: true } })
    if (!instance || instance.status === 'deleted') return reply.code(404).send({ error: '实例不存在' })
    if (!(await checkInstancePermission(request.user, { user_id: instance.userId, host_id: instance.hostId })).allowed) return reply.code(403).send({ error: '无权查看实例出口' })
    const binding = await prisma.instanceEgressAttachment.findUnique({ where: { hostId_incusName: { hostId: instance.hostId, incusName: instance.incusId } } })
    const policy = await prisma.hostNetworkPolicy.findUnique({ where: { hostId: instance.hostId } })
    const status = policy?.status as { bindings?: Array<{ instanceName: string; generation: number }>; groups?: Array<{ key: string; state: string }> } | undefined
    const applied = Boolean(binding && policy?.revision === policy?.appliedRevision && status?.bindings?.some(item => item.instanceName === binding.incusName && item.generation === binding.generation))
    const group = status?.groups?.find(item => item.key === `p${instance.egressProfileId}`)
    return { mode: instance.egressMode, profileName: instance.egressProfile?.name ?? null, state: instance.egressMode === 'host' ? 'host' : !applied ? 'pending' : instance.egressMode === 'direct' ? 'ready' : group?.state ?? 'blocked', reportedAt: policy?.reportedAt ?? null }
  })
  app.get('/api/egress/profiles', { onRequest: [app.authenticate] }, async request => {
    const own = request.user.role === 'admin' ? null : (await prisma.host.findMany({ where: { userId: request.user.id }, select: { id: true } })).map(host => host.id)
    const profiles = await prisma.egressProfile.findMany({ orderBy: { id: 'desc' }, take: 1000 })
    return { profiles: profiles.filter(profile => own === null || profile.allowedHostIds.some(hostId => own.includes(hostId))).map(profile => ({ id: profile.id, name: profile.name, active: profile.active, egress: request.user.role === 'admin' ? profile.egress : undefined, hostIds: own === null ? profile.allowedHostIds : profile.allowedHostIds.filter(hostId => own.includes(hostId)) })) }
  })
  for (const editing of [false, true]) {
    app.route<{ Params: { id: number }; Body: { name: string; hostIds: number[]; egress: unknown } }>({
      method: editing ? 'PUT' : 'POST', url: editing ? '/api/admin/egress-profiles/:id' : '/api/admin/egress-profiles',
      onRequest: [app.authenticate, app.requireAdmin], schema: { ...(editing ? { params: { type: 'object', required: ['id'], properties: { id } } } : {}), body: { type: 'object', additionalProperties: false, required: Object.keys(fields), properties: fields } },
      handler: async (request, reply) => {
        const egress = validateEgress(request.body.egress)
        if (egress.mode !== 'proxy') throw new NetworkInputError('出口策略必须选择代理节点')
        await assertSafeHttpUrl(egress.probeUrl, 'Probe URL')
        const result = await prisma.$transaction(async tx => {
          await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
          if (await tx.proxyNode.count({ where: { id: { in: egress.proxyNodeIds } } }) !== egress.proxyNodeIds.length || await tx.host.count({ where: { id: { in: request.body.hostIds } } }) !== request.body.hostIds.length) throw new NetworkInputError('代理或授权宿主节点不存在')
          const existing = editing ? await tx.egressProfile.findUnique({ where: { id: request.params.id } }) : null
          if (editing && !existing) return null
          const used = existing ? await tx.instanceEgressAttachment.findMany({ where: { profileId: existing.id }, select: { hostId: true }, distinct: ['hostId'] }) : []
          const hostBindings = existing ? await tx.hostNetworkPolicy.findMany({ where: { egressProfileId: existing.id }, select: { hostId: true } }) : []
          const packages = existing ? await tx.packageHost.findMany({ where: { package: { egressProfileId: existing.id } }, select: { hostId: true } }) : []
          const instances = existing ? await tx.instance.findMany({ where: { egressProfileId: existing.id, status: { not: 'deleted' } }, select: { hostId: true }, distinct: ['hostId'] }) : []
          const affected = [...new Set([...used, ...hostBindings, ...packages, ...instances].map(item => item.hostId))].sort((a,b) => a-b)
          if (affected.some(hostId => !request.body.hostIds.includes(hostId))) throw new NetworkInputError('授权宿主范围仍有套餐、实例或节点引用，不能移除')
          const data = { name: request.body.name.trim(), allowedHostIds: request.body.hostIds, egress: egress as unknown as Prisma.InputJsonValue }
          const profile = existing ? await tx.egressProfile.update({ where: { id: existing.id }, data }) : await tx.egressProfile.create({ data })
          for (const hostId of affected) await bumpEgressHost(tx, hostId)
          return { profile, affected }
        })
        if (!result) return reply.code(404).send({ error: '出口策略不存在' })
        await createLog(request.user.id, 'host', editing ? 'egress.profile.update' : 'egress.profile.create', `Saved egress policy ${result.profile.id}`, 'success')
        // Persisted host revisions retry on heartbeat/reconnect; slow or offline nodes never hold the editor open.
        for (const hostId of result.affected) void pushNetworkPolicy(hostId).catch(() => app.log.warn({ hostId }, 'Egress synchronization pending'))
        return reply.code(editing ? 202 : 201).send({ id: result.profile.id, pendingHostIds: result.affected })
      }
    })
  }
  app.delete<{ Params: { id: number } }>('/api/admin/egress-profiles/:id', { onRequest: [app.authenticate, app.requireAdmin], schema: { params: { type: 'object', required: ['id'], properties: { id } } } }, async (request, reply) => {
    try {
      const count = await prisma.$transaction(async tx => {
        await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
        const profileId = request.params.id
        const used = await tx.package.count({ where: { egressProfileId: profileId } })
          + await tx.instance.count({ where: { egressProfileId: profileId } })
          + await tx.hostNetworkPolicy.count({ where: { egressProfileId: profileId } })
          + await tx.instanceEgressAttachment.count({ where: { profileId } })
        if (used) return -1
        return (await tx.egressProfile.deleteMany({ where: { id: profileId } })).count
      })
      if (count === -1) return reply.code(409).send({ error: '出口策略仍被引用，无法删除' })
      if (!count) return reply.code(404).send({ error: '出口策略不存在' })
      await createLog(request.user.id, 'host', 'egress.profile.delete', `Deleted egress policy ${request.params.id}`, 'success')
      return { success: true }
    } catch (error) {
      if (error instanceof Prisma.PrismaClientKnownRequestError && ['P2003', 'P2014'].includes(error.code)) return reply.code(409).send({ error: '出口策略仍被引用，无法删除' })
      throw error
    }
  })
}
