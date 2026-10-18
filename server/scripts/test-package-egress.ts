import assert from 'node:assert/strict'
import Fastify from 'fastify'
const databaseTests = process.env.RUN_PACKAGE_EGRESS_DB_TESTS === '1'
if (databaseTests) {
  const url = new URL(process.env.DATABASE_URL || '')
  assert(['127.0.0.1', 'localhost'].includes(url.hostname) && url.pathname.endsWith('_test'), 'Disposable local *_test database required')
} else process.env.DATABASE_URL = 'postgresql://test:test@127.0.0.1:1/test'
const { prisma, closePrismaDatabase } = await import('../src/db/prisma.js')
const { normalizePackageEgress, validatePackageEgress, snapshotPackageEgress, packageEgressInstruction } = await import('../src/lib/egress/package.js')
const { preparePackageEgress, ensurePackageEgressBeforeStart, releasePackageEgress } = await import('../src/lib/egress/attachments.js')
const { hostTunnelManager } = await import('../src/lib/incus/tunnel-manager.js')
const { encryptSensitiveData } = await import('../src/lib/security.js')
const { createInstance: createIncusInstance } = await import('../src/lib/incus/incus-instances.js')
const { default: routes } = await import('../src/routes/egress-profiles.js')
const { updatePackage } = await import('../src/db/packages.js')
assert.deepEqual(normalizePackageEgress({}), {})
for (const input of [{ egressMode: 'invalid' }, { egressProfileVersionId: 0 }, { egressProfileVersionId: 1.1 }]) assert.throws(() => normalizePackageEgress(input))
await validatePackageEgress(prisma, { hostIds: [1], egressMode: 'legacy' })
for (const input of [{ networkMode: 'public_ipv4' }, { instanceType: 'vm' }, { privileged: true }]) await assert.rejects(validatePackageEgress(prisma, { hostIds: [1], egressMode: 'direct', ...input }))
if (!databaseTests) { console.log('Package egress validation passed; disposable DB tests opt-in'); await closePrismaDatabase(); process.exit(0) }
const prefix = `egress-${Date.now()}`, users: number[] = [], hosts: number[] = [], nodes: number[] = [], profiles: number[] = []
let pkgId: number | undefined
const originalSupports = hostTunnelManager.supportsControl, originalOnline = hostTunnelManager.isTunnelOnline, originalRequest = hostTunnelManager.requestControl
let online = true, acknowledge = true, groupReady = true
let activePushes = 0, maxPushes = 0
hostTunnelManager.supportsControl = () => online
hostTunnelManager.isTunnelOnline = () => online
hostTunnelManager.requestControl = async (_hostId, _method, instruction: any) => {
  activePushes++; maxPushes = Math.max(maxPushes,activePushes)
  try {
    await new Promise(resolve => setTimeout(resolve,15))
    return { appliedRevision: instruction.revision, state: 'applied', error: 'none', bindings: acknowledge ? instruction.bindings.map((b: any) => ({ instanceName: b.instanceName, generation: b.generation })) : [], groups: instruction.groups.map((g: any) => ({ key: g.key, state: groupReady ? 'ready' : 'blocked', error: groupReady ? 'none' : 'health' })) } as any
  } finally {activePushes--}
}
const app = Fastify()
try {
  for (const role of ['admin', 'user', 'user'] as const) { const u = await prisma.user.create({ data: { username: `${prefix}-${users.length}`, passwordHash: 'fixture-unused', role } }); users.push(u.id) }
  for (let i = 0; i < 2; i++) { const h = await prisma.host.create({ data: { userId: users[i + 1], name: `${prefix}-${i}`, url: 'https://fixture.invalid' } }); hosts.push(h.id); await prisma.hostAgent.create({ data: { hostId: h.id, agentId: `${prefix}-${i}`, secretHash: 'fixture-unused', secretEncrypted: 'fixture-unused' } }) }
  const outbound = { protocol: 'socks', settings: { servers: [{ address: 'proxy.invalid', port: 1080 }] } }
  const node = await prisma.proxyNode.create({ data: { name: prefix, protocol: 'socks', server: 'proxy.invalid', port: 1080, fingerprint: prefix, configEncrypted: encryptSensitiveData(JSON.stringify(outbound)) } }); nodes.push(node.id)
  app.decorate('authenticate', async (req: any, reply: any) => { const idx = Number(req.headers['x-fixture-user']); if (!Number.isInteger(idx) || !users[idx]) return reply.code(401).send(); req.user = { id: users[idx], role: idx === 0 ? 'admin' : 'user' } })
  app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user.role !== 'admin') return reply.code(403).send() })
  await app.register(routes)
  const inject = (method: any, url: string, payload?: any, user = 0) => app.inject({ method, url, payload, headers: { 'x-fixture-user': String(user) } })
  assert.equal((await inject('POST', '/api/admin/egress-profiles', { name: 'denied', proxyNodeId: node.id, hostIds: hosts }, 1)).statusCode, 403)
  assert.equal((await inject('POST', '/api/admin/egress-profiles', { name: '   ', proxyNodeId: node.id, hostIds: hosts })).statusCode, 400)
  const response = await inject('POST', '/api/admin/egress-profiles', { name: prefix, proxyNodeId: node.id, hostIds: [hosts[0]] })
  assert.equal(response.statusCode, 200, response.body)
  const created = response.json(); profiles.push(created.id)
  const v1 = created.versionId
  assert.equal((await inject('GET', '/api/egress/profiles', undefined, 2)).json().profiles.length, 0)
  assert((await inject('GET', '/api/egress/profiles', undefined, 1)).json().profiles.some((p: any) => p.id === v1 && !('proxyNodeId' in p) && p.hostIds.length === 1))
  const versions = await Promise.all([1, 2].map(() => inject('POST', `/api/admin/egress-profiles/${created.id}/versions`, { proxyNodeId: node.id, hostIds: hosts })))
  assert(versions.every(v => v.statusCode === 200)); assert.deepEqual(versions.map(v => v.json().version).sort(), [2, 3])
  const v2 = versions[0].json().id
  await assert.rejects(validatePackageEgress(prisma, { egressMode: 'profile', egressProfileVersionId: v1, hostIds: hosts }))
  const pkg = await prisma.package.create({ data: { userId: users[1], name: prefix, cpuMax: 100, memoryMax: 256, diskMax: 1024, egressMode: 'profile', egressProfileVersionId: v1, packageHosts: { create: { hostId: hosts[0] } } } }); pkgId = pkg.id
  const base = { userId: users[1], hostId: hosts[0], packageId: pkg.id, name: prefix, image: 'debian', cpu: 100, memory: 256, disk: 1024 }
  const instance = await prisma.$transaction(async tx => tx.instance.create({ data: { ...base, incusId: `${prefix}-original`, ...(await snapshotPackageEgress(tx, pkg.id, hosts[0])) } }))
  assert.equal((await inject('GET', `/api/instances/${instance.id}/egress`, undefined, 2)).statusCode, 403)
  assert.equal((await inject('GET', `/api/instances/${instance.id}/egress`, undefined, 1)).json().state, 'pending')
  await updatePackage(pkg.id, { name: `${prefix}-edited` }, true)
  await assert.rejects(updatePackage(pkg.id, { networkMode: 'public_ipv4' }, true), /NAT container/)
  await updatePackage(pkg.id, { egressMode: 'profile', egressProfileVersionId: v2 }, true)
  assert.equal((await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })).egressProfileVersionId, v1)
  assert.equal((await snapshotPackageEgress(prisma, pkg.id, hosts[0], instance.id)).egressProfileVersionId, v1)
  await assert.rejects(snapshotPackageEgress(prisma, pkg.id, hosts[1], instance.id), /not permitted/)
  online = false; await assert.rejects(snapshotPackageEgress(prisma, pkg.id, hosts[0]), /online Agent/); online = true
  await assert.rejects(prisma.$transaction(async tx => { await tx.instance.create({ data: { ...base, incusId: `${prefix}-rollback`, ...(await snapshotPackageEgress(tx, pkg.id, hosts[0])) } }); throw new Error('rollback') }))
  assert.equal(await prisma.instance.count({ where: { incusId: `${prefix}-rollback` } }), 0)
  const config = { name: instance.incusId, profiles: [], devices: { eth0: { type: 'nic', network: 'incusbr0', 'ipv4.address': '10.10.0.2' } }, config: {} }
  let written: any
  const client = { options: { hostId: hosts[0] }, request: async (method: string) => { if (method === 'GET') return config; if (method === 'PUT') written = true } } as any
  const prepared = await preparePackageEgress(client, config, instance.id) as any
  assert.match(prepared.devices.eth0.host_name, /^ipg[a-f0-9]{11}$/); assert.equal(prepared.devices.eth0['security.ipv4_filtering'], 'true'); assert(!('host_name' in config.devices.eth0))
  assert.equal((await packageEgressInstruction(hosts[0])).bindings.length, 1)
  const reported = (await inject('GET', `/api/instances/${instance.id}/egress`, undefined, 1)).json()
  assert.equal(reported.state, 'ready'); assert.equal(reported.profileName, prefix); assert(!JSON.stringify(reported).includes('proxy.invalid'))
  acknowledge = false; await assert.rejects(preparePackageEgress(client, config, instance.id), /not ready/); acknowledge = true
  groupReady = false; await assert.rejects(ensurePackageEgressBeforeStart(client, instance.incusId), /not ready/); assert.equal(written, undefined); groupReady = true
  await ensurePackageEgressBeforeStart(client, instance.incusId); assert.equal(written, true)
  const parallelNames = ['parallel-a','parallel-b'].map(name => `${prefix}-${name}`)
  await Promise.all(parallelNames.map(name => preparePackageEgress(client,{...config,name},instance.id)))
  assert.equal(maxPushes,1,'host control applies are serialized across concurrent provisions')
  await Promise.all(parallelNames.map(name => releasePackageEgress(client,name)))
  await releasePackageEgress(client, instance.incusId); assert.equal((await packageEgressInstruction(hosts[0])).bindings.length, 0)
  const failingClient = { ...client, request: async (method: string) => { if (method === 'POST') throw new Error('fixture create failure'); throw Object.assign(new Error('fixture absent'), { statusCode: 404 }) } }
  await assert.rejects(createIncusInstance(failingClient as any, config, instance.id), /fixture create failure/)
  assert.equal((await packageEgressInstruction(hosts[0])).bindings.length, 0, 'failed create releases its intent')
  const duplicateClient = { ...client, request: async (method: string) => { if (method === 'POST') throw new Error('fixture duplicate'); return config } }
  await assert.rejects(createIncusInstance(duplicateClient as any, config, instance.id), /fixture duplicate/)
  assert.equal((await packageEgressInstruction(hosts[0])).bindings.length, 1, 'existing or uncertain physical guest retains its binding')
  await releasePackageEgress(client,instance.incusId)
  await assert.rejects(prisma.proxyNode.delete({ where: { id: node.id } }), /foreign key/i)
  console.log('Package egress DB/API passed: grants, concurrent versions, snapshots, rollback, clone/migration, NIC generation/readiness, deletion protection')
} finally {
  hostTunnelManager.supportsControl = originalSupports; hostTunnelManager.isTunnelOnline = originalOnline; hostTunnelManager.requestControl = originalRequest
  await app.close()
  await prisma.operationTask.deleteMany({ where: { hostId: { in: hosts } } })
  await prisma.instance.deleteMany({ where: { userId: { in: users } } })
  if (pkgId) await prisma.package.delete({ where: { id: pkgId } })
  await prisma.egressProfileVersion.deleteMany({ where: { profileId: { in: profiles } } }); await prisma.egressProfile.deleteMany({ where: { id: { in: profiles } } })
  await prisma.proxyNode.deleteMany({ where: { id: { in: nodes } } }); await prisma.host.deleteMany({ where: { id: { in: hosts } } })
  await prisma.log.deleteMany({ where: { userId: { in: users } } }); await prisma.user.deleteMany({ where: { id: { in: users } } })
  await closePrismaDatabase()
}
