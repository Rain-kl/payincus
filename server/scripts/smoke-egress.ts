/** Isolated PostgreSQL + real HTTP route smoke. Refuses any other database. */
import assert from 'node:assert/strict'
import { createHash, randomUUID } from 'node:crypto'
import Fastify from 'fastify'
import websocket from '@fastify/websocket'
import { prisma } from '../src/db/prisma.js'
import egressRoutes from '../src/routes/egress.js'
import agentRoutes from '../src/routes/agent.js'
import { createAgentBodyHash, createAgentSignature, generateAgentId, generateAgentSecret, hashAgentSecret } from '../src/lib/agent-auth.js'
import { recordNetworkStatus } from '../src/lib/egress/sync.js'
import WebSocket from 'ws'
import { hostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import { encodeFrame, decodeFrame, TunnelFrameType } from '../src/lib/incus/tunnel-protocol.js'
import { protectProvisionedNetwork } from '../src/lib/egress/provision.js'
import { defaultEgress, defaultFirewall } from '../src/lib/egress/policy.js'
import { encryptSensitiveData } from '../src/lib/security.js'

const target = new URL(process.env.DATABASE_URL || '')
assert.ok(target.hostname === '127.0.0.1' && target.port === '55432' && target.pathname === '/egress_test', 'Dedicated local egress_test database on port 55432 required')
const app = Fastify()
const marker = `egress-${randomUUID()}`
const user = await prisma.user.create({ data: { username: marker, passwordHash: 'test-only', role: 'admin' } })
const host = await prisma.host.create({ data: { userId: user.id, name: marker, url: 'https://127.0.0.1:8443', tunnelEnabled: false } })
const nodeIds: number[] = []
const agentId = generateAgentId(), agentSecret = generateAgentSecret()
try {
  await prisma.hostAgent.create({ data: { hostId: host.id, agentId, secretHash: hashAgentSecret(agentSecret), secretEncrypted: encryptSensitiveData(agentSecret), capabilities: ['xray-egress', 'network-control'] } })
  app.decorate('authenticate', async (req: any, reply: any) => { if (!req.headers['x-test-role']) return reply.code(401).send({ error: 'required' }); req.user = { id: user.id, role: req.headers['x-test-role'] } })
  app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user.role !== 'admin') return reply.code(403).send({ error: 'forbidden' }) })
  app.decorate('authenticateAdmin', app.requireAdmin)
  await app.register(websocket)
  await app.register(egressRoutes, { prefix: '/api' })
  await app.register(agentRoutes, { prefix: '/api/agent' })
  const heartbeat = async (extra: Record<string, unknown> = {}) => {
    const body = { version: 'v1.2.6', capabilities: ['xray-egress', 'network-control'], ...extra }
    const timestamp = String(Date.now()), nonce = randomUUID(), bodyHash = createAgentBodyHash(body)
    const signature = createAgentSignature(agentSecret, { method: 'POST', path: '/api/agent/heartbeat', timestamp, nonce, bodyHash })
    const response = await app.inject({ method: 'POST', url: '/api/agent/heartbeat', payload: body, headers: { 'x-incudal-agent-id': agentId, 'x-incudal-timestamp': timestamp, 'x-incudal-nonce': nonce, 'x-incudal-body-sha256': bodyHash, 'x-incudal-signature': signature } })
    assert.equal(response.statusCode, 200, response.body)
    assert.equal(response.json().network, undefined, 'Heartbeats must not deliver configuration or test tasks')
    assert.equal(response.json().tunnel.enabled, body.capabilities.includes('network-control'), 'Direct Incus hosts need control connections when supported')
    return response.json()
  }
  const request = (method: any, url: string, payload?: any, role = 'admin') => app.inject({ method, url: `/api${url}`, headers: { 'x-test-role': role }, ...(payload ? { payload } : {}) })
  for (const path of ['/admin/proxy-nodes', `/hosts/${host.id}/network-policy`]) assert.equal((await request('GET', path, undefined, 'user')).statusCode, 403)
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
  const offline = await request('PUT', `/hosts/${host.id}/network-policy/egress`, { revision: 0, value: egress })
  assert.equal(offline.statusCode, 503, offline.body)
  assert.equal(await prisma.hostNetworkPolicy.count({ where: { hostId: host.id } }), 0, 'Offline saves must fail immediately')
  await heartbeat()
  const address = await app.listen({ host: '127.0.0.1', port: 0 })
  const timestamp = String(Date.now()), nonce = randomUUID(), bodyHash = createHash('sha256').update('').digest('hex')
  // A real websocket goes through the same HMAC route as the Go Agent.
  const signature = createAgentSignature(agentSecret, { method: 'GET', path: '/api/agent/tunnel', timestamp, nonce, bodyHash })
  const socket = new WebSocket(address.replace('http:', 'ws:') + '/api/agent/tunnel', { headers: { 'x-incudal-agent-id': agentId, 'x-incudal-timestamp': timestamp, 'x-incudal-nonce': nonce, 'x-incudal-body-sha256': bodyHash, 'x-incudal-signature': signature } })
  let failApply = false
  let probeRelease: (() => void) | undefined
  let controlRequests = 0
  let cancelReceived: (() => void) | undefined
  const helloReady = new Promise<void>(resolve => hostTunnelManager.once('control_ready', () => resolve()))
  socket.on('message', data => {
    const frame = decodeFrame(Buffer.from(data as Buffer))
    if (frame.type === TunnelFrameType.CONTROL_HELLO) {
      socket.send(encodeFrame(TunnelFrameType.CONTROL_HELLO, 0, Buffer.from(JSON.stringify({ capabilities: ['network.apply', 'proxy.probe'] }))))
    } else if (frame.type === TunnelFrameType.CONTROL_CANCEL) { cancelReceived?.()
    } else if (frame.type === TunnelFrameType.CONTROL_REQUEST) {
      controlRequests++
      const { method, params } = JSON.parse(frame.payload.toString())
      const respond = (result: unknown) => socket.send(encodeFrame(TunnelFrameType.CONTROL_RESPONSE, frame.streamId, Buffer.from(JSON.stringify({ ok: true, result }))))
      if (method === 'network.apply') respond({ appliedRevision: failApply ? params.revision - 1 : params.revision, state: failApply ? 'error' : 'applied', error: failApply ? 'xray' : 'none', errorStage: failApply ? 'config' : '', errorDetail: failApply ? 'config_invalid' : '', xrayAvailable: true })
      else if (method === 'proxy.probe') probeRelease = () => respond({ items: params.nodes.map((node: any) => ({ nodeId: node.id, success: true, latencyMs: 12, error: 'private-test-password' })) })
    }
  })
  await Promise.race([helloReady, new Promise((_, reject) => setTimeout(() => reject(new Error('authenticated control handshake timed out')), 3000).unref())])
  // Reproduce an older binary's -once probe overwriting capability metadata.
  // The authenticated live tunnel remains authoritative for the warning UI.
  await heartbeat({ capabilities: ['heartbeat', 'report', 'host-metrics', 'instance-status', 'traffic-counters', 'nvidia-gpu'] })
  const staleCapabilities = (await request('GET', `/hosts/${host.id}/network-policy`)).json()
  assert.equal(staleCapabilities.agent.capabilities.includes('network-control'), false)
  assert.equal(staleCapabilities.networkControlReady, true)
  await heartbeat()
  const saved = await request('PUT', `/hosts/${host.id}/network-policy/egress`, { revision: 0, value: egress }); assert.equal(saved.statusCode, 200, saved.body)
  assert.equal((await request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 0, value: defaultFirewall })).statusCode, 409)
  const firewall = { ...defaultFirewall, enabled: true, rules: [{ direction: 'egress', action: 'drop', protocol: 'tcp', source: '', destination: '', sourcePort: '', destinationPort: '25', description: 'SMTP', enabled: true }] }
  assert.equal((await request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 1, value: firewall })).statusCode, 200)
  assert.equal((await request('DELETE', `/admin/proxy-nodes/${node.id}`)).statusCode, 409)
  assert.ok(controlRequests >= 2, 'Configuration must apply during the save request')
  await recordNetworkStatus(host.id, { appliedRevision: 99, state: 'applied', error: 'none', secret: 'private-test-password' })
  policy = (await request('GET', `/hosts/${host.id}/network-policy`)).json().policy
  assert.equal(policy.appliedRevision, 2); assert.ok(!JSON.stringify(policy).includes('private-test-password'))
  const client: any = { options: { hostId: host.id }, request: async () => ({ devices: { eth0: { type: 'nic', network: 'incusbr0' }, eth1: { type: 'nic', nictype: 'routed', parent: 'eth0' } } }) }
  const protectedConfig: any = await protectProvisionedNetwork(client, { name: 'fixture', devices: {} })
  assert.ok(protectedConfig.devices.eth0['security.acls'].includes('incudal-host-firewall')); assert.match(protectedConfig.devices.eth1.host_name, /^iceg[a-f0-9]{10}$/)
  const before = await prisma.proxyProbeJob.count()
  let returned = false
  const probe = request('POST', '/admin/proxy-probes', { hostId: host.id, nodeIds: [node.id], probeUrl: 'https://1.1.1.1/' }).then(response => { returned = true; return response })
  for (let i = 0; i < 100 && !probeRelease; i++) await new Promise(resolve => setTimeout(resolve, 10))
  assert.ok(probeRelease, 'Test must arrive immediately over the tunnel without heartbeat')
  assert.equal(returned, false, 'The HTTP call must wait for actual test completion')
  assert.equal(await prisma.proxyProbeJob.count(), before)
  probeRelease!()
  const completed = await probe
  assert.equal(completed.statusCode, 200, completed.body)
  assert.equal(completed.headers['cache-control'], 'no-store')
  assert.deepEqual(completed.json().items, [{ nodeId: node.id, success: true, latencyMs: 12, error: '', errorDetail: '' }])
  assert.equal(completed.json().host.id, host.id)
  assert.ok(!completed.body.includes('private-test-password'))
  assert.equal(await prisma.proxyProbeJob.count(), before, 'Test requests and results must never be persisted')
  assert.equal((await request('GET', '/admin/proxy-probes')).statusCode, 404, 'There is no test history endpoint')
  probeRelease = undefined
  const abort = new AbortController()
  const cancelled = new Promise<void>(resolve => { cancelReceived = resolve })
  const abortedRequest = fetch(address + '/api/admin/proxy-probes', { method: 'POST', headers: { 'x-test-role': 'admin', 'content-type': 'application/json' }, body: JSON.stringify({ hostId: host.id, nodeIds: [node.id], probeUrl: 'https://1.1.1.1/' }), signal: abort.signal }).catch(() => undefined)
  for (let i = 0; i < 100 && !probeRelease; i++) await new Promise(resolve => setTimeout(resolve, 10))
  assert.ok(probeRelease)
  abort.abort()
  await abortedRequest
  await Promise.race([cancelled, new Promise((_, reject) => setTimeout(() => reject(new Error('HTTP disconnect did not cancel Agent request')), 1500).unref())])
  assert.equal(await prisma.proxyProbeJob.count(), before)
  const races = await Promise.all([request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 2, value: firewall }), request('PUT', `/hosts/${host.id}/network-policy/firewall`, { revision: 2, value: firewall })])
  assert.deepEqual(races.map(r => r.statusCode).sort(), [200, 409])
  failApply = true
  const failed = await request('PUT', `/hosts/${host.id}/network-policy/egress`, { revision: 3, value: { ...defaultEgress, mode: 'direct' } })
  assert.equal(failed.statusCode, 422, failed.body)
  assert.equal(failed.json().revision, 4); assert.equal(failed.json().status.errorStage, 'config')
  policy = (await request('GET', `/hosts/${host.id}/network-policy`)).json().policy
  assert.equal(policy.revision, 4); assert.equal(policy.appliedRevision, 3); assert.equal(policy.status.errorDetail, 'config_invalid')
  await assert.rejects(() => protectProvisionedNetwork(client, { name: 'pending', devices: {} }))
  socket.close()
  await new Promise<void>(resolve => socket.once('close', () => resolve()))
  assert.equal((await request('GET', `/hosts/${host.id}/network-policy`)).json().networkControlReady, false)
  // Reconnect must recover the latest persisted configuration without a heartbeat.
  const reconnectTimestamp = String(Date.now()), reconnectNonce = randomUUID()
  const reconnectSignature = createAgentSignature(agentSecret, { method: 'GET', path: '/api/agent/tunnel', timestamp: reconnectTimestamp, nonce: reconnectNonce, bodyHash })
  const reconnect = new WebSocket(address.replace('http:', 'ws:') + '/api/agent/tunnel', { headers: { 'x-incudal-agent-id': agentId, 'x-incudal-timestamp': reconnectTimestamp, 'x-incudal-nonce': reconnectNonce, 'x-incudal-body-sha256': bodyHash, 'x-incudal-signature': reconnectSignature } })
  reconnect.on('message', data => {
    const frame = decodeFrame(Buffer.from(data as Buffer))
    if (frame.type === TunnelFrameType.CONTROL_HELLO) reconnect.send(encodeFrame(TunnelFrameType.CONTROL_HELLO, 0, Buffer.from(JSON.stringify({ capabilities: ['network.apply', 'proxy.probe'] }))))
    if (frame.type === TunnelFrameType.CONTROL_REQUEST) {
      const { params } = JSON.parse(frame.payload.toString())
      assert.equal(params.revision, 4)
      reconnect.send(encodeFrame(TunnelFrameType.CONTROL_RESPONSE, frame.streamId, Buffer.from(JSON.stringify({ ok: true, result: { state: 'applied', appliedRevision: 4, error: 'none', xrayAvailable: true } }))))
    }
  })
  for (let i = 0; i < 100; i++) {
    policy = await prisma.hostNetworkPolicy.findUniqueOrThrow({ where: { hostId: host.id } })
    if (policy.appliedRevision === 4) break
    await new Promise(resolve => setTimeout(resolve, 10))
  }
  assert.equal(policy.appliedRevision, 4, 'Reconnect must immediately synchronize the saved policy')
  reconnect.close()
  await new Promise<void>(resolve => reconnect.once('close', () => resolve()))
  console.log('egress PostgreSQL/HTTP/websocket smoke passed: HMAC, control-only hosts, immediate apply, reconnect recovery, revision conflicts, synchronous unsaved tests, HTTP cancellation, apply failures and provisioning')
} finally {
  hostTunnelManager.unregisterTunnel(host.id)
  await app.close()
  await prisma.host.delete({ where: { id: host.id } })
  await prisma.proxyNode.deleteMany({ where: { id: { in: nodeIds } } })
  await prisma.log.deleteMany({ where: { userId: user.id } })
  await prisma.user.delete({ where: { id: user.id } })
  await prisma.$disconnect()
}
