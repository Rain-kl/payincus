import type { FastifyInstance } from 'fastify'
import { Prisma } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { createLog } from '../db/logs.js'
import { encryptSensitiveData } from '../lib/security.js'
import { safeFetch, assertSafeHttpUrl } from '../lib/outbound-security.js'
import { parseProxyImport } from '../lib/egress/import.js'
import { decodeProxyConfig } from '../lib/egress/sync.js'
import { defaultEgress, defaultFirewall, validateEgress, validateFirewall, ids, probeUrl, NetworkInputError } from '../lib/egress/policy.js'

const idSchema = { type: 'integer', minimum: 1, maximum: 2147483647 }
const hostParams = { type: 'object', required: ['id'], properties: { id: idSchema } }
const safeNodeSelect = { id: true, name: true, protocol: true, server: true, port: true, createdAt: true, updatedAt: true } as const
async function importContent(body: { content?: string; subscriptionUrl?: string }): Promise<string> {
  if (Boolean(body.content) === Boolean(body.subscriptionUrl)) throw new NetworkInputError('Provide content or a subscription URL')
  if (body.content) return body.content
  let url = body.subscriptionUrl!
  for (let redirects = 0; redirects <= 3; redirects++) {
    let response: Response
    try { response = await safeFetch(url, { redirect: 'manual', signal: AbortSignal.timeout(15000) }, 'Proxy subscription') }
    catch { throw new NetworkInputError('Unable to download subscription') }
    if ([301, 302, 303, 307, 308].includes(response.status)) {
      const location = response.headers.get('location'); await response.body?.cancel()
      if (!location || redirects === 3) throw new NetworkInputError('Subscription redirect limit exceeded')
      try { url = new URL(location, url).href } catch { throw new NetworkInputError('Invalid subscription redirect') }
      continue
    }
    if (!response.ok || !response.body) { await response.body?.cancel(); throw new NetworkInputError('Unable to download subscription') }
    const reader = response.body.getReader(), chunks: Uint8Array[] = []; let size = 0
    try {
      while (true) {
        const { done, value } = await reader.read(); if (done) break
        size += value.byteLength
        if (size > 512 * 1024) throw new NetworkInputError('Subscription exceeds 512 KiB')
        chunks.push(value)
      }
    } catch (error) {
      if (error instanceof NetworkInputError) throw error
      throw new NetworkInputError('Unable to read subscription')
    } finally { await reader.cancel().catch(() => {}) }
    return Buffer.concat(chunks).toString('utf8')
  }
  throw new NetworkInputError('Unable to download subscription')
}
export default async function egressRoutes(app: FastifyInstance) {
  const admin = [app.authenticate, app.requireAdmin]
  app.get('/admin/proxy-probe-hosts', { onRequest: admin }, async () => ({ hosts: await prisma.host.findMany({ where: { agent: { enabled: true, capabilities: { array_contains: ['xray-egress'] } } }, select: { id: true, name: true }, orderBy: { id: 'asc' }, take: 1000 }) }))
  app.get('/admin/proxy-nodes', { onRequest: admin }, async () => ({ nodes: await prisma.proxyNode.findMany({ select: safeNodeSelect, orderBy: { id: 'asc' }, take: 1000 }) }))
  app.post<{ Body: { content?: string; subscriptionUrl?: string; confirm: boolean } }>('/admin/proxy-nodes/import', {
    onRequest: admin, bodyLimit: 600 * 1024, schema: { body: { type: 'object', additionalProperties: false, required: ['confirm'], properties: { content: { type: 'string', maxLength: 524288 }, subscriptionUrl: { type: 'string', maxLength: 2048 }, confirm: { type: 'boolean' } } } }
  }, async (request) => {
    const parsed = parseProxyImport(await importContent(request.body))
    if (!request.body.confirm) return { nodes: parsed.nodes.map(({ name, protocol, server, port, fingerprint }) => ({ name, protocol, server, port, fingerprint })), errors: parsed.errors }
    let added = 0, duplicates = 0
    for (const node of parsed.nodes) {
      const created = await prisma.proxyNode.createMany({ data: [{ name: node.name, protocol: node.protocol, server: node.server, port: node.port, fingerprint: node.fingerprint, configEncrypted: encryptSensitiveData(JSON.stringify(node.outbound)) }], skipDuplicates: true })
      added += created.count; duplicates += 1 - created.count
    }
    await createLog(request.user.id, 'host', 'host.proxy.import', `Imported proxy nodes: added=${added}, duplicate=${duplicates}, invalid=${parsed.errors.length}`, 'success')
    return { added, duplicates, errors: parsed.errors }
  })
  app.delete<{ Params: { id: number } }>('/admin/proxy-nodes/:id', { onRequest: admin, schema: { params: hostParams } }, async (request, reply) => {
    const state = await prisma.$transaction(async tx => {
      await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
      if (await tx.hostNetworkPolicy.count({ where: { egress: { path: ['proxyNodeIds'], array_contains: [request.params.id] } } })) return 'used'
      return (await tx.proxyNode.deleteMany({ where: { id: request.params.id } })).count ? 'ok' : 'missing'
    })
    if (state === 'used') return reply.code(409).send({ error: '代理节点仍被宿主节点引用，请先移除出口配置' })
    if (state === 'missing') return reply.code(404).send({ error: '代理节点不存在' })
    await createLog(request.user.id, 'host', 'host.proxy.delete', `Deleted proxy node ${request.params.id}`, 'success')
    return { success: true }
  })
  app.get<{ Params: { id: number } }>('/hosts/:id/network-policy', { onRequest: admin, schema: { params: hostParams } }, async (request, reply) => {
    const host = await prisma.host.findUnique({ where: { id: request.params.id }, select: { id: true, agent: { select: { enabled: true, version: true, capabilities: true, lastSeenAt: true } } } })
    if (!host) return reply.code(404).send({ error: '宿主节点不存在' })
    return { policy: await prisma.hostNetworkPolicy.findUnique({ where: { hostId: host.id } }) ?? { hostId: host.id, revision: 0, appliedRevision: 0, egress: defaultEgress, firewall: defaultFirewall, status: {}, reportedAt: null }, agent: host.agent }
  })
  for (const section of ['egress', 'firewall'] as const) app.put<{ Params: { id: number }; Body: { revision: number; value: unknown } }>(`/hosts/:id/network-policy/${section}`, {
    onRequest: admin, schema: { params: hostParams, body: { type: 'object', additionalProperties: false, required: ['revision', 'value'], properties: { revision: { type: 'integer', minimum: 0, maximum: 2147483646 }, value: { type: 'object', additionalProperties: true } } } }
  }, async (request, reply) => {
    const value = section === 'egress' ? validateEgress(request.body.value) : validateFirewall(request.body.value)
    if (section === 'egress' && (value as ReturnType<typeof validateEgress>).mode === 'proxy') {
      try { await assertSafeHttpUrl((value as ReturnType<typeof validateEgress>).probeUrl, 'Probe URL') }
      catch { throw new NetworkInputError('Probe URL must resolve to a public HTTP target') }
    }
    const state = await prisma.$transaction(async tx => {
      const hosts = await tx.$queryRaw<Array<{ id: number }>>`SELECT id FROM hosts WHERE id = ${request.params.id} FOR UPDATE`
      if (!hosts.length) return 'missing'
      // Serialize references and deletion using the same advisory lock.
      await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
      const current = await tx.hostNetworkPolicy.findUnique({ where: { hostId: request.params.id } })
      if ((current?.revision ?? 0) !== request.body.revision) return 'conflict'
      if (section === 'egress') {
        const selected = (value as ReturnType<typeof validateEgress>).proxyNodeIds
        if (await tx.proxyNode.count({ where: { id: { in: selected } } }) !== selected.length) return 'nodes'
      }
      const data = { [section]: value as unknown as Prisma.InputJsonValue, revision: { increment: 1 } }
      await tx.hostNetworkPolicy.upsert({ where: { hostId: request.params.id }, create: { hostId: request.params.id, egress: (section === 'egress' ? value : defaultEgress) as unknown as Prisma.InputJsonValue, firewall: (section === 'firewall' ? value : defaultFirewall) as unknown as Prisma.InputJsonValue }, update: data })
      return 'ok'
    })
    if (state === 'missing') return reply.code(404).send({ error: '宿主节点不存在' })
    if (state === 'conflict') return reply.code(409).send({ error: '配置已更新，请刷新后重试' })
    if (state === 'nodes') return reply.code(400).send({ error: '所选代理节点不存在' })
    await createLog(request.user.id, 'host', `host.network.${section}`, `Updated host ${request.params.id} ${section} policy`, 'success')
    return { success: true, pending: true }
  })
  app.post<{ Body: { hostId: number; nodeIds: number[]; probeUrl: string } }>('/admin/proxy-probes', {
    onRequest: admin, schema: { body: { type: 'object', additionalProperties: false, required: ['hostId', 'nodeIds', 'probeUrl'], properties: { hostId: idSchema, nodeIds: { type: 'array', minItems: 1, maxItems: 10, uniqueItems: true, items: idSchema }, probeUrl: { type: 'string', maxLength: 2048 } } } }
  }, async (request, reply) => {
    const nodeIds = ids(request.body.nodeIds, 10), url = probeUrl(request.body.probeUrl)
    await assertSafeHttpUrl(url, 'Probe URL')
    const agent = await prisma.hostAgent.findUnique({ where: { hostId: request.body.hostId } })
    if (!agent?.enabled || !Array.isArray(agent.capabilities) || !agent.capabilities.includes('xray-egress')) return reply.code(400).send({ error: '请选择已安装新版 Xray Agent 的宿主节点' })
    const nodes = await prisma.proxyNode.findMany({ where: { id: { in: nodeIds } } })
    if (nodes.length !== nodeIds.length) return reply.code(400).send({ error: '代理节点不存在' })
    const byId = new Map(nodes.map(n => [n.id, n]))
    const job = await prisma.$transaction(async tx => {
      await tx.$queryRaw`SELECT id FROM hosts WHERE id = ${request.body.hostId} FOR UPDATE`
      const outstanding = await tx.proxyProbeJob.count({ where: { hostId: request.body.hostId, state: { in: ['pending', 'running'] }, expiresAt: { gt: new Date() } } })
      if (outstanding >= 10) throw new NetworkInputError('Too many pending probe jobs on this host')
      return tx.proxyProbeJob.create({ data: { hostId: request.body.hostId, nodeIds, probeUrl: url, snapshotEncrypted: encryptSensitiveData(JSON.stringify(nodeIds.map(id => ({ id, outbound: decodeProxyConfig(byId.get(id)!.configEncrypted) })))), expiresAt: new Date(Date.now() + 20 * 60_000) }, select: { id: true, state: true } })
    })
    return { job }
  })
  app.get('/admin/proxy-probes', { onRequest: admin }, async () => {
    await prisma.proxyProbeJob.updateMany({ where: { state: { in: ['pending', 'running'] }, expiresAt: { lte: new Date() } }, data: { state: 'expired', snapshotEncrypted: '', leaseUntil: null } })
    return { jobs: await prisma.proxyProbeJob.findMany({ select: { id: true, hostId: true, nodeIds: true, probeUrl: true, state: true, result: true, createdAt: true, completedAt: true, expiresAt: true, host: { select: { name: true } } }, orderBy: { createdAt: 'desc' }, take: 50 }) }
  })
}
