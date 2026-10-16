/** Isolated PostgreSQL + real HTTP route smoke. Refuses any other database. */
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import Fastify from 'fastify'
import websocket from '@fastify/websocket'
import { prisma } from '../src/db/prisma.js'
import egressRoutes from '../src/routes/egress.js'
import agentRoutes from '../src/routes/agent.js'
import { createAgentBodyHash, createAgentSignature, generateAgentId, generateAgentSecret, hashAgentSecret } from '../src/lib/agent-auth.js'
import { networkHeartbeat } from '../src/lib/egress/sync.js'
import { protectProvisionedNetwork } from '../src/lib/egress/provision.js'
import { defaultEgress, defaultFirewall } from '../src/lib/egress/policy.js'
import { encryptSensitiveData } from '../src/lib/security.js'

const target = new URL(process.env.DATABASE_URL || '')
assert.ok(target.hostname === '127.0.0.1' && target.port === '55432' && target.pathname === '/egress_test', 'Dedicated local egress_test database on port 55432 required')
const app = Fastify()
const marker = `egress-${randomUUID()}`
const user = await prisma.user.create({ data: { username: marker, passwordHash: 'test-only', role: 'admin' } })
const host = await prisma.host.create({ data: { userId: user.id, name: marker, url: 'https://127.0.0.1:8443' } })
const nodeIds: number[] = []
const agentId = generateAgentId(), agentSecret = generateAgentSecret()
try {
  await prisma.hostAgent.create({ data: { hostId: host.id, agentId, secretHash: hashAgentSecret(agentSecret), secretEncrypted: encryptSensitiveData(agentSecret), capabilities: ['xray-egress'] } })
  app.decorate('authenticate', async (req: any, reply: any) => { if (!req.headers['x-test-role']) return reply.code(401).send({ error: 'required' }); req.user = { id: user.id, role: req.headers['x-test-role'] } })
  app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user.role !== 'admin') return reply.code(403).send({ error: 'forbidden' }) })
  app.decorate('authenticateAdmin', app.requireAdmin)
  await app.register(websocket)
  await app.register(egressRoutes, { prefix: '/api' })
  await app.register(agentRoutes, { prefix: '/api/agent' })
  const heartbeat = async (extra: Record<string, unknown> = {}) => {
    const body = { version: 'v1.2.5', capabilities: ['xray-egress'], ...extra }
    const timestamp = String(Date.now()), nonce = randomUUID(), bodyHash = createAgentBodyHash(body)
    const signature = createAgentSignature(agentSecret, { method: 'POST', path: '/api/agent/heartbeat', timestamp, nonce, bodyHash })
    const response = await app.inject({ method: 'POST', url: '/api/agent/heartbeat', payload: body, headers: { 'x-incudal-agent-id': agentId, 'x-incudal-timestamp': timestamp, 'x-incudal-nonce': nonce, 'x-incudal-body-sha256': bodyHash, 'x-incudal-signature': signature } })
    assert.equal(response.statusCode, 200, response.body)
    return response.json().network
  }
  const request = (method: any, url: string, payload?: any, role = 'admin') => app.inject({ method, url: `/api${url}`, headers: { 'x-test-role': role }, ...(payload ? { payload } : {}) })
  for (const path of ['/admin/proxy-nodes', `/hosts/${host.id}/network-policy`, '/admin/proxy-probes']) assert.equal((await request('GET', path, undefined, 'user')).statusCode, 403)
  assert.equal((await app.inject({ url: '/api/admin/proxy-nodes' })).statusCode, 401)
  assert.equal((await request('DELETE', '/admin/proxy-nodes/not-a-number')).statusCode, 400)
  const content = `socks5://fixture:private-test-password@example.com:1080#${marker}`
  const preview = await request('POST', '/admin/proxy-nodes/import', { content, confirm: false })
  assert.equal(preview.statusCode, 200, preview.body); assert.equal(preview.json().nodes.length, 1); assert.ok(!preview.body.includes('private-test-password'))
  const added = await request('POST', '/admin/proxy-nodes/import', { content, confirm: true }); assert.equal(added.statusCode, 200, added.body); assert.equal(added.json().added, 1)
  const duplicate = await request('POST', '/admin/proxy-nodes/import', { content, confirm: true }); assert.equal(duplicate.json().duplicates, 1)
  const catalog = await request('GET', '/admin/proxy-nodes'); assert.ok(!catalog.body.includes('private-test-password')); assert.ok(!catalog.body.includes('configEncrypted'))
  const node = catalog.json().nodes.find((n: any) => n.name === marker); assert.ok(node); nodeIds.push(node.id)
  const encrypted = await prisma.proxyNode.findUniqueOrThrow({ where: { id: node.id } }); assert.ok(!encrypted.configEncrypted.includes('private-test-password'))
  let policy = (await request('GET', `/hosts/${host.id}/network-policy`)).json().policy; assert.equal(policy.revision, 0)
  const egress = { ...defaultEgress, mode: 'proxy', proxyNodeIds: [node.id], probeUrl: 'https://1.1.1.1/' }
  const saved = await request('PUT', `/hosts/${host.id}/network-policy/egress`, { revision: 0, value: egress }); assert.equal(saved.statusCode, 200, saved.body)
  assert.equal((await request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 0, value: defaultFirewall })).statusCode, 409)
  const firewall = { ...defaultFirewall, enabled: true, rules: [{ direction: 'egress', action: 'drop', protocol: 'tcp', source: '', destination: '', sourcePort: '', destinationPort: '25', description: 'SMTP', enabled: true }] }
  assert.equal((await request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 1, value: firewall })).statusCode, 200)
  assert.equal((await request('DELETE', `/admin/proxy-nodes/${node.id}`)).statusCode, 409)
  const delivery: any = await networkHeartbeat(host.id, null, []); assert.equal(delivery.revision, 2); assert.equal(delivery.nodes[0].outbound.settings.servers[0].users[0].pass, 'private-test-password')
  await networkHeartbeat(host.id, { appliedRevision: 99, state: 'applied', error: 'none', secret: 'private-test-password' }, [])
  policy = (await request('GET', `/hosts/${host.id}/network-policy`)).json().policy; assert.equal(policy.appliedRevision, 0); assert.ok(!JSON.stringify(policy).includes('private-test-password'))
  await networkHeartbeat(host.id, { appliedRevision: 2, state: 'applied', error: 'none', xrayAvailable: true }, [])
  const client: any = { options: { hostId: host.id }, request: async () => ({ devices: { eth0: { type: 'nic', network: 'incusbr0' }, eth1: { type: 'nic', nictype: 'routed', parent: 'eth0' } } }) }
  const protectedConfig: any = await protectProvisionedNetwork(client, { name: 'fixture', devices: {} })
  assert.ok(protectedConfig.devices.eth0['security.acls'].includes('incudal-host-firewall')); assert.match(protectedConfig.devices.eth1.host_name, /^iceg[a-f0-9]{10}$/)
  const probe = await request('POST', '/admin/proxy-probes', { hostId: host.id, nodeIds: [node.id], probeUrl: 'https://1.1.1.1/' }); assert.equal(probe.statusCode, 200, probe.body)
  const jobId = probe.json().job.id
  const jobDelivery: any = await heartbeat(); assert.equal(jobDelivery.probes[0].jobId, jobId)
  assert.equal((await networkHeartbeat(host.id, null, []) as any).probes.length, 0, 'Leased jobs are not duplicated')
  const rejectedReport: any = await networkHeartbeat(host.id, null, [{ jobId, items: [{ nodeId: node.id + 100000, success: true }] }]); assert.equal((await prisma.proxyProbeJob.findUniqueOrThrow({ where: { id: jobId } })).state, 'running'); assert.deepEqual(rejectedReport.acknowledgedProbeJobs, [])
  const acceptedReport: any = await heartbeat({ proxyProbeResults: [{ jobId, items: [{ nodeId: node.id, success: true, latencyMs: 12, error: 'private-test-password' }] }], networkStatus: { appliedRevision: 2, state: 'error', error: 'xray', errorStage: 'config', errorDetail: 'reality_key_invalid' } }); assert.deepEqual(acceptedReport.acknowledgedProbeJobs, [jobId])
  const diagnostics: any = (await prisma.hostNetworkPolicy.findUniqueOrThrow({ where: { hostId: host.id } })).status
  assert.equal(diagnostics.errorStage, 'config'); assert.equal(diagnostics.errorDetail, 'reality_key_invalid')
  const done = await prisma.proxyProbeJob.findUniqueOrThrow({ where: { id: jobId } }); assert.equal(done.state, 'completed'); assert.equal(done.snapshotEncrypted, ''); assert.ok(!JSON.stringify(done.result).includes('private-test-password'))
  const duplicateReport: any = await networkHeartbeat(host.id, null, [{ jobId, items: [{ nodeId: node.id, success: false, latencyMs: 999 }] }]); assert.deepEqual((await prisma.proxyProbeJob.findUniqueOrThrow({ where: { id: jobId } })).result, done.result); assert.deepEqual(duplicateReport.acknowledgedProbeJobs, [jobId])
  const batch: Array<{ jobId: string; items: Array<{ nodeId: number; success: boolean; latencyMs: number }> }> = []
  for (let i = 0; i < 4; i++) {
    const queued = await request('POST', '/admin/proxy-probes', { hostId: host.id, nodeIds: [node.id], probeUrl: 'https://1.1.1.1/' })
    assert.equal(queued.statusCode, 200, queued.body)
    const jobId = queued.json().job.id
    assert.equal((await heartbeat()).probes[0].jobId, jobId)
    batch.push({ jobId, items: [{ nodeId: node.id, success: true, latencyMs: 7 }] })
  }
  const batchReport = await heartbeat({ proxyProbeResults: batch })
  assert.equal(batchReport.acknowledgedProbeJobs.length, 3, 'Only processed jobs may be acknowledged')
  assert.equal((await prisma.proxyProbeJob.findUniqueOrThrow({ where: { id: batch[3].jobId } })).state, 'running')
  assert.deepEqual((await heartbeat({ proxyProbeResults: [batch[3]] })).acknowledgedProbeJobs, [batch[3].jobId])
  const races = await Promise.all([request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 2, value: firewall }), request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 2, value: firewall })]); assert.deepEqual(races.map(r => r.statusCode).sort(), [200, 409])
  await assert.rejects(() => protectProvisionedNetwork(client, { name: 'pending', devices: {} }))
  const listing = await request('GET', '/admin/proxy-probes'); assert.ok(!listing.body.includes('snapshotEncrypted')); assert.ok(!listing.body.includes('private-test-password'))
  console.log('egress PostgreSQL/HTTP smoke passed: permissions, encrypted import, revisions, references, delivery, probes, idempotency and protected provisioning')
} finally {
  await app.close()
  await prisma.host.delete({ where: { id: host.id } })
  await prisma.proxyNode.deleteMany({ where: { id: { in: nodeIds } } })
  await prisma.log.deleteMany({ where: { userId: user.id } })
  await prisma.user.delete({ where: { id: user.id } })
  await prisma.$disconnect()
}
