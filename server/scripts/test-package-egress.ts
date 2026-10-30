import assert from 'node:assert/strict'
import Fastify from 'fastify'
const databaseTests = process.env.RUN_PACKAGE_EGRESS_DB_TESTS === '1'
if (databaseTests) {
  const url = new URL(process.env.DATABASE_URL || '')
  assert(['127.0.0.1', 'localhost'].includes(url.hostname) && url.pathname.endsWith('_test'), 'Disposable local *_test database required')
} else process.env.DATABASE_URL = 'postgresql://test:test@127.0.0.1:1/test'
const { prisma, closePrismaDatabase } = await import('../src/db/prisma.js')
const { normalizePackageEgress, validatePackageEgress, bindPackageEgress, packageEgressInstruction } = await import('../src/lib/egress/package.js')
const { preparePackageEgress, ensurePackageEgressBeforeStart, releasePackageEgress } = await import('../src/lib/egress/attachments.js')
const { hostTunnelManager } = await import('../src/lib/incus/tunnel-manager.js')
const { encryptSensitiveData } = await import('../src/lib/security.js')
const { createInstance: createIncusInstance } = await import('../src/lib/incus/incus-instances.js')
const { default: routes } = await import('../src/routes/egress-profiles.js')
const { createPackage, updatePackage } = await import('../src/db/packages.js')
assert.deepEqual(normalizePackageEgress({}), {})
for (const input of [{ egressMode: 'invalid' }, { egressProfileId: 0 }, { egressProfileId: 1.1 }]) assert.throws(() => normalizePackageEgress(input))
await validatePackageEgress(prisma, { hostIds: [1], egressMode: 'host' })
for (const input of [{ networkMode: 'public_ipv4' }, { instanceType: 'vm' }, { privileged: true }]) await assert.rejects(validatePackageEgress(prisma, { hostIds: [1], egressMode: 'direct', ...input }))
if (!databaseTests) { console.log('Package egress validation passed; disposable DB tests opt-in'); await closePrismaDatabase(); process.exit(0) }
const prefix = `egress-${Date.now()}`, users: number[] = [], hosts: number[] = [], nodes: number[] = [], profiles: number[] = []
let pkgId: number | undefined, racePkgId: number | undefined
const originalSupports = hostTunnelManager.supportsControl, originalOnline = hostTunnelManager.isTunnelOnline, originalRequest = hostTunnelManager.requestControl
let online = true, acknowledge = true, groupReady = true
let activePushes = 0, maxPushes = 0
const pushedInstructions: any[] = []
hostTunnelManager.supportsControl = () => online
hostTunnelManager.isTunnelOnline = () => online
hostTunnelManager.requestControl = async (_hostId, _method, instruction: any) => {
  pushedInstructions.push(instruction)
  activePushes++; maxPushes = Math.max(maxPushes,activePushes)
  try {
    await new Promise(resolve => setTimeout(resolve,15))
    return { appliedRevision: instruction.revision, state: 'applied', error: 'none', bindings: acknowledge ? instruction.bindings.map((b: any) => ({ instanceName: b.instanceName, generation: b.generation })) : [], groups: instruction.groups.map((g: any) => ({ key: g.key, state: groupReady ? 'ready' : 'blocked', error: groupReady ? 'none' : 'health' })) } as any
  } finally {activePushes--}
}
const app = Fastify()
try {
  for (const role of ['admin', 'user', 'user'] as const) { const u = await prisma.user.create({ data: { username: `${prefix}-${users.length}`, passwordHash: 'fixture-unused', role } }); users.push(u.id) }
  for (let i = 0; i < 2; i++) { const h = await prisma.host.create({ data: { userId: users[i + 1], name: `${prefix}-${i}`, url: 'https://fixture.invalid' } }); hosts.push(h.id); await prisma.hostAgent.create({ data: { hostId: h.id, agentId: `${prefix}-${i}`, secretHash: 'fixture-unused', secretEncrypted: 'fixture-unused', enabled: true } }) }
  const outbound = { protocol: 'socks', settings: { servers: [{ address: 'proxy.invalid', port: 1080 }] } }
  const node = await prisma.proxyNode.create({ data: { name: prefix, protocol: 'socks', server: 'proxy.invalid', port: 1080, fingerprint: prefix, configEncrypted: encryptSensitiveData(JSON.stringify(outbound)) } }); nodes.push(node.id)
  const nextNode = await prisma.proxyNode.create({ data: { name: `${prefix}-next`, protocol: 'socks', server: 'next-proxy.invalid', port: 1080, fingerprint: `${prefix}-next`, configEncrypted: encryptSensitiveData(JSON.stringify({ ...outbound, settings: { servers: [{ address: 'next-proxy.invalid', port: 1080 }] } })) } }); nodes.push(nextNode.id)
  app.decorate('authenticate', async (req: any, reply: any) => { const idx = Number(req.headers['x-fixture-user']); if (!Number.isInteger(idx) || !users[idx]) return reply.code(401).send(); req.user = { id: users[idx], role: idx === 0 ? 'admin' : 'user' } })
  app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user.role !== 'admin') return reply.code(403).send() })
  await app.register(routes)
  const inject = (method: any, url: string, payload?: any, user = 0) => app.inject({ method, url, payload, headers: { 'x-fixture-user': String(user) } })
  const egress = (proxyNodeId: number) => ({ mode: 'proxy', strategy: 'failover', proxyNodeIds: [proxyNodeId], internalCidrs: [], probeUrl: 'https://1.1.1.1/cdn-cgi/trace', probeIntervalSeconds: 30 })
  assert.equal((await inject('POST', '/api/admin/egress-profiles', { name: 'denied', egress: egress(node.id), hostIds: hosts }, 1)).statusCode, 403)
  assert.equal((await inject('POST', '/api/admin/egress-profiles', { name: '   ', egress: egress(node.id), hostIds: hosts })).statusCode, 400)
  const response = await inject('POST', '/api/admin/egress-profiles', { name: prefix, egress: egress(node.id), hostIds: [hosts[0]] })
  assert.equal(response.statusCode, 201, response.body)
  const created = response.json(); profiles.push(created.id)
  const profileId = created.id
  assert.equal((await inject('GET', '/api/egress/profiles', undefined, 2)).json().profiles.length, 0)
  assert((await inject('GET', '/api/egress/profiles', undefined, 1)).json().profiles.some((p: any) => p.id === profileId && !('proxyNodeId' in p) && p.hostIds.length === 1))
  // PayIncus: authorizing a host pre-provisions the policy group before any instance exists,
  // while unauthorized hosts stay untouched and offline pushes never hold the editor open.
  assert.equal(created.pendingHostIds.includes(hosts[0]), true, 'authorized host must be queued for synchronization')
  assert.equal(created.pendingHostIds.includes(hosts[1]), false, 'unauthorized host must not be synchronized')
  assert.equal((await packageEgressInstruction(hosts[0])).groups.some((group: any) => group.key === `p${profileId}`), true, 'authorized host must pre-provision the group')
  assert.equal((await packageEgressInstruction(hosts[1])).groups.length, 0, 'unauthorized host must not receive the group')
  await assert.rejects(validatePackageEgress(prisma, { egressMode: 'profile', egressProfileId: profileId, hostIds: hosts }))
  const pkg = await prisma.package.create({ data: { userId: users[1], name: prefix, cpuMax: 100, memoryMax: 256, diskMax: 1024, egressMode: 'profile', egressProfileId: profileId, packageHosts: { create: { hostId: hosts[0] } } } }); pkgId = pkg.id
  const base = { userId: users[1], hostId: hosts[0], packageId: pkg.id, name: prefix, image: 'debian', cpu: 100, memory: 256, disk: 1024 }
  const instance = await prisma.$transaction(async tx => tx.instance.create({ data: { ...base, incusId: `${prefix}-original`, ...(await bindPackageEgress(tx, pkg.id, hosts[0])) } }))
  assert.equal((await inject('GET', `/api/instances/${instance.id}/egress`, undefined, 2)).statusCode, 403)
  assert.equal((await inject('GET', `/api/instances/${instance.id}/egress`, undefined, 1)).json().state, 'pending')
  await updatePackage(pkg.id, { name: `${prefix}-edited` }, true)
  await assert.rejects(updatePackage(pkg.id, { networkMode: 'public_ipv4' }, true), /NAT container/)
  await updatePackage(pkg.id, { egressMode: 'profile', egressProfileId: profileId }, true)
  assert.equal((await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })).egressProfileId, profileId)
  // The edit form reloads through GET /packages/:id; the response must echo the
  // saved egress binding or the form silently falls back to "host" on reopen.
  const detail = (await inject('GET', `/api/packages/${pkg.id}`, undefined, 1)).json().package
  assert.equal(detail.egress_mode, 'profile', 'package detail must return the saved egress mode')
  assert.equal(detail.egress_profile_id, profileId, 'package detail must return the saved egress profile')
  assert.equal((await bindPackageEgress(prisma, pkg.id, hosts[0], instance.id)).egressProfileId, profileId)
  await assert.rejects(bindPackageEgress(prisma, pkg.id, hosts[1], instance.id), /not permitted/)
  online = false; await assert.rejects(bindPackageEgress(prisma, pkg.id, hosts[0]), /online Agent/); online = true
  await assert.rejects(prisma.$transaction(async tx => { await tx.instance.create({ data: { ...base, incusId: `${prefix}-rollback`, ...(await bindPackageEgress(tx, pkg.id, hosts[0])) } }); throw new Error('rollback') }))
  assert.equal(await prisma.instance.count({ where: { incusId: `${prefix}-rollback` } }), 0)
  const config = { name: instance.incusId, profiles: [], devices: { eth0: { type: 'nic', network: 'incusbr0', 'ipv4.address': '10.10.0.2' } }, config: {} }
  let written: any
  const client = { options: { hostId: hosts[0] }, request: async (method: string) => { if (method === 'GET') return config; if (method === 'PUT') written = true } } as any
  const prepared = await preparePackageEgress(client, config, instance.id) as any
  assert.match(prepared.devices.eth0.host_name, /^ipg[a-f0-9]{11}$/); assert.equal(prepared.devices.eth0['security.ipv4_filtering'], 'true'); assert(!('host_name' in config.devices.eth0))
  assert.equal((await packageEgressInstruction(hosts[0])).bindings.length, 1)
  const reported = (await inject('GET', `/api/instances/${instance.id}/egress`, undefined, 1)).json()
  assert.equal(reported.state, 'ready'); assert.equal(reported.profileName, prefix); assert(!JSON.stringify(reported).includes('proxy.invalid'))
  const revisionBeforeEdit = (await prisma.hostNetworkPolicy.findUniqueOrThrow({ where: { hostId: hosts[0] } })).revision
  const deniedScope = await inject('PUT', `/api/admin/egress-profiles/${profileId}`, { name: prefix, egress: egress(nextNode.id), hostIds: [hosts[1]] })
  assert.equal(deniedScope.statusCode, 400, 'cannot remove a host still authorized by package, instance or runtime attachment')
  assert.equal((await prisma.egressProfile.findUniqueOrThrow({ where: { id: profileId } })).egress && (await prisma.egressProfile.findUniqueOrThrow({ where: { id: profileId } })).allowedHostIds[0], hosts[0])
  const edited = await inject('PUT', `/api/admin/egress-profiles/${profileId}`, { name: `${prefix}-edited`, egress: egress(nextNode.id), hostIds: [hosts[0]] })
  assert.equal(edited.statusCode, 202, edited.body)
  assert.equal(edited.json().pendingHostIds.includes(hosts[0]), true)
  const updatedProfile = await prisma.egressProfile.findUniqueOrThrow({ where: { id: profileId } })
  assert.deepEqual((updatedProfile.egress as any).proxyNodeIds, [nextNode.id])
  assert.equal((await prisma.package.findUniqueOrThrow({ where: { id: pkg.id } })).egressProfileId, profileId)
  assert.equal((await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })).egressProfileId, profileId)
  const updatedInstruction = await packageEgressInstruction(hosts[0])
  assert.equal(updatedInstruction.groups[0].key, `p${profileId}`)
  assert.deepEqual(updatedInstruction.groups[0].egress.proxyNodeIds, [nextNode.id], 'existing bindings resolve the edited live policy')
  assert((await prisma.hostNetworkPolicy.findUniqueOrThrow({ where: { hostId: hosts[0] } })).revision > revisionBeforeEdit)
  for (let attempt = 0; attempt < 50 && !pushedInstructions.some(instruction => instruction.groups?.some((group: any) => group.key === `p${profileId}` && group.nodes?.some((entry: any) => entry.id === nextNode.id))); attempt++) await new Promise(resolve => setTimeout(resolve, 10))
  assert(pushedInstructions.some(instruction => instruction.groups?.some((group: any) => group.key === `p${profileId}` && group.nodes?.some((entry: any) => entry.id === nextNode.id))), 'profile edit did not reconcile the new live policy to the bound host')
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
  const protectedDelete = await inject('DELETE', `/api/admin/egress-profiles/${profileId}`)
  assert.equal(protectedDelete.statusCode, 409, `shared policy cannot be deleted while package or instance references remain: ${protectedDelete.body}`)
  const raceProfile = await inject('POST', '/api/admin/egress-profiles', { name: `${prefix}-race`, egress: egress(node.id), hostIds: [hosts[0]] })
  assert.equal(raceProfile.statusCode, 201, raceProfile.body)
  profiles.push(raceProfile.json().id)
  const packageCreation = createPackage({ userId: users[1], name: `${prefix}-race-package`, cpuMax: 100, memoryMax: 256, diskMax: 1024, hostIds: [hosts[0]], egressMode: 'profile', egressProfileId: raceProfile.json().id, active: false }, true)
  const policyEdit = inject('PUT', `/api/admin/egress-profiles/${raceProfile.json().id}`, { name: `${prefix}-race-edited`, egress: egress(nextNode.id), hostIds: [hosts[1]] })
  const [createdRacePackage, editedRacePolicy] = await Promise.allSettled([packageCreation, policyEdit])
  const editResponse = editedRacePolicy.status === 'fulfilled' ? editedRacePolicy.value : null
  assert(editResponse && [202, 400].includes(editResponse.statusCode), `unexpected concurrent policy edit result: ${editedRacePolicy.status === 'rejected' ? editedRacePolicy.reason : editResponse.body}`)
  if (editResponse.statusCode === 202) {
    assert.equal(createdRacePackage.status, 'rejected', 'policy edit removed host authorization while a package was being created')
    assert.match(String(createdRacePackage.status === 'rejected' ? createdRacePackage.reason : ''), /not permitted/i)
  } else {
    assert.equal(createdRacePackage.status, 'fulfilled', 'policy edit saw a concurrent package reference but package creation also failed')
    racePkgId = createdRacePackage.value
  }
  console.log('Package egress DB/API passed: shared binding, in-place reconcile, rollback, host scope, NIC readiness, deletion protection, and serialized package-creation/policy-edit race')
} finally {
  hostTunnelManager.supportsControl = originalSupports; hostTunnelManager.isTunnelOnline = originalOnline; hostTunnelManager.requestControl = originalRequest
  await app.close()
  await prisma.operationTask.deleteMany({ where: { hostId: { in: hosts } } })
  await prisma.instance.deleteMany({ where: { userId: { in: users } } })
  if (pkgId) await prisma.package.delete({ where: { id: pkgId } })
  if (racePkgId) await prisma.package.delete({ where: { id: racePkgId } })
  await prisma.egressProfile.deleteMany({ where: { id: { in: profiles } } })
  await prisma.proxyNode.deleteMany({ where: { id: { in: nodes } } }); await prisma.host.deleteMany({ where: { id: { in: hosts } } })
  await prisma.log.deleteMany({ where: { userId: { in: users } } }); await prisma.user.deleteMany({ where: { id: { in: users } } })
  await closePrismaDatabase()
}
