import type { FastifyInstance } from 'fastify'
import { prisma } from '../db/prisma.js'
import { createLog } from '../db/logs.js'
import { checkInstancePermission } from '../lib/permission.js'

const id = { type: 'integer', minimum: 1, maximum: 2147483647 }
const versionFields = { proxyNodeId: id, hostIds: { type: 'array', minItems: 1, maxItems: 1000, uniqueItems: true, items: id } }
export default async function egressProfileRoutes(app: FastifyInstance) {
  app.get<{ Params: { id: number } }>('/api/instances/:id/egress', {
    onRequest: [app.authenticate], schema: { params: { type: 'object', required: ['id'], properties: { id } } }
  }, async (request, reply) => {
    const instance = await prisma.instance.findUnique({ where: { id: request.params.id }, include: { egressProfileVersion: { include: { profile: true } } } })
    if (!instance || instance.status === 'deleted') return reply.code(404).send({ error: '实例不存在' })
    if (!(await checkInstancePermission(request.user, { user_id: instance.userId, host_id: instance.hostId })).allowed) return reply.code(403).send({ error: '无权查看实例出口' })
    const binding = await prisma.instanceEgressAttachment.findUnique({ where: { hostId_incusName: { hostId: instance.hostId, incusName: instance.incusId } } })
    const policy = await prisma.hostNetworkPolicy.findUnique({ where: { hostId: instance.hostId } })
    const status = policy?.status as { bindings?: Array<{ instanceName: string; generation: number }>; groups?: Array<{ key: string; state: string }> } | undefined
    const applied = Boolean(binding && policy?.revision === policy?.appliedRevision && status?.bindings?.some(item => item.instanceName === binding.incusName && item.generation === binding.generation))
    const group = status?.groups?.find(item => item.key === `v${instance.egressProfileVersionId}`)
    return { mode: instance.egressMode, profileName: instance.egressProfileVersion?.profile.name ?? null, version: instance.egressProfileVersion?.version ?? null, state: instance.egressMode === 'legacy' ? 'legacy' : !applied ? 'pending' : instance.egressMode === 'direct' ? 'ready' : group?.state ?? 'blocked', reportedAt: policy?.reportedAt ?? null }
  })
  app.get('/api/egress/profiles', { onRequest: [app.authenticate] }, async request => {
    const own = request.user.role === 'admin' ? null : (await prisma.host.findMany({ where: { userId: request.user.id }, select: { id: true } })).map(host => host.id)
    const profiles = await prisma.egressProfile.findMany({ include: { versions: { orderBy: { version: 'desc' } } }, orderBy: { id: 'desc' }, take: 1000 })
    return { profiles: profiles.flatMap(profile => profile.versions.filter(version => own === null || version.allowedHostIds.some(hostId => own.includes(hostId))).map(version => ({ id: version.id, profileId: profile.id, version: version.version, name: profile.name, active: profile.active, proxyNodeId: request.user.role === 'admin' ? version.proxyNodeId : undefined, hostIds: own === null ? version.allowedHostIds : version.allowedHostIds.filter(hostId => own.includes(hostId)) }))) }
  })
  app.post<{ Body: { name: string; proxyNodeId: number; hostIds: number[] } }>('/api/admin/egress-profiles', {
    onRequest: [app.authenticate, app.requireAdmin], schema: { body: { type: 'object', additionalProperties: false, required: ['name', 'proxyNodeId', 'hostIds'], properties: { name: { type: 'string', minLength: 1, maxLength: 80, pattern: '\\S' }, ...versionFields } } }
  }, async (request, reply) => {
    const { proxyNodeId, hostIds } = request.body
    const profile = await prisma.$transaction(async tx => {
      await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
      if (!await tx.proxyNode.findUnique({ where: { id: proxyNodeId } }) || await tx.host.count({ where: { id: { in: hostIds } } }) !== hostIds.length) return null
      return tx.egressProfile.create({ data: { name: request.body.name.trim(), versions: { create: { version: 1, proxyNodeId, allowedHostIds: hostIds } } }, include: { versions: true } })
    })
    if (!profile) return reply.code(400).send({ error: '代理或授权节点不存在' })
    await createLog(request.user.id, 'host', 'egress.profile.create', `Created egress profile ${profile.id}`, 'success')
    return { id: profile.id, versionId: profile.versions[0].id }
  })
  app.post<{ Params: { id: number }; Body: { proxyNodeId: number; hostIds: number[] } }>('/api/admin/egress-profiles/:id/versions', {
    onRequest: [app.authenticate, app.requireAdmin], schema: { params: { type: 'object', required: ['id'], properties: { id } }, body: { type: 'object', additionalProperties: false, required: ['proxyNodeId', 'hostIds'], properties: versionFields } }
  }, async (request, reply) => {
    const version = await prisma.$transaction(async tx => {
      await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
      const profile = await tx.egressProfile.findUnique({ where: { id: request.params.id }, include: { versions: { orderBy: { version: 'desc' }, take: 1 } } })
      if (!profile || !await tx.proxyNode.findUnique({ where: { id: request.body.proxyNodeId } }) || await tx.host.count({ where: { id: { in: request.body.hostIds } } }) !== request.body.hostIds.length) return null
      return tx.egressProfileVersion.create({ data: { profileId: profile.id, version: (profile.versions[0]?.version ?? 0) + 1, proxyNodeId: request.body.proxyNodeId, allowedHostIds: request.body.hostIds } })
    })
    if (!version) return reply.code(400).send({ error: '出口策略、代理或授权节点不存在' })
    await createLog(request.user.id, 'host', 'egress.profile.version', `Created egress profile version ${version.id}`, 'success')
    return { id: version.id, version: version.version }
  })
}
