import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { applyGpuDevice, isGpuFresh, normalizeGpuInput, resolveGpuPolicy } from '../src/lib/gpu.js'

let checks = 0
function check(fn: () => void) { fn(); checks++ }
check(() => assert.deepEqual(normalizeGpuInput({ gpuShared: true, gpuDeviceIds: [2, 1, 2] }), { gpuShared: true, gpuDeviceIds: [2, 1] }))
for (const input of [{ gpuShared: 'true' }, { gpuDeviceIds: [0] }, { gpuDeviceIds: ['1'] }, { gpuDeviceIds: [-1] }]) check(() => assert.throws(() => normalizeGpuInput(input)))
const inherited = { gpuShared: true, gpuDeviceIds: [1] }
check(() => assert.deepEqual(resolveGpuPolicy(inherited, { gpuShared: null, gpuDeviceIds: [] }), inherited))
check(() => assert.equal(resolveGpuPolicy(inherited, { gpuShared: false, gpuDeviceIds: [] }).gpuShared, false))
check(() => assert.deepEqual(resolveGpuPolicy(inherited, { gpuShared: true, gpuDeviceIds: [2] }).gpuDeviceIds, [2]))
const config: Record<string, any> = { type: 'container', profiles: ['expose-all-gpus'], config: {}, devices: { oldGpu: { type: 'gpu' }, root: { type: 'disk' } } }
applyGpuDevice(config, '0000:65:00.0')
check(() => assert.deepEqual(config.devices.gpu0, { type: 'gpu', gputype: 'physical', pci: '0000:65:00.0' }))
check(() => assert.equal(config.devices.oldGpu, undefined))
check(() => assert.equal(config.devices.root.type, 'disk'))
check(() => assert.deepEqual(config.profiles, []))
check(() => assert.equal(config.config['nvidia.runtime'], 'true'))
check(() => assert.throws(() => applyGpuDevice({ type: 'virtual-machine' }, '0000:65:00.0')))
check(() => assert.throws(() => applyGpuDevice({ config: { 'security.privileged': 'true' } }, '0000:65:00.0')))
check(() => assert.throws(() => applyGpuDevice({}, 'all')))
check(() => assert.equal(isGpuFresh({ available: true, runtimeReady: true, lastSeenAt: new Date() }), true))
check(() => assert.equal(isGpuFresh({ available: true, runtimeReady: true, lastSeenAt: new Date(Date.now() - 200_000) }), false))
check(() => assert.equal(isGpuFresh({ available: true, runtimeReady: false, lastSeenAt: new Date() }), false))
// Guard every existing provisioner against accidentally omitting the GPU reservation/binding.
const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
for (const path of ['server/src/routes/instances.ts', 'server/src/routes/admin-billing.ts', 'server/src/routes/hosts.ts']) check(() => assert.ok(readFileSync(resolve(root, path), 'utf8').includes('await reserveInstanceGpu(tx,'), `Missing purchase reservation: ${path}`))
check(() => assert.ok(readFileSync(resolve(root, 'server/src/workers/instanceTaskWorker.ts'), 'utf8').includes('gpuPciAddress: await getInstanceGpuPci(task.instanceId, host.id)')))
console.log(`GPU unit and provisioner guards passed (${checks} checks)`)

if (process.env.RUN_GPU_DB_TESTS === '1') {
  const url = new URL(process.env.DATABASE_URL || '')
  assert.ok(['127.0.0.1', 'localhost'].includes(url.hostname), 'DB tests require an explicitly configured local database')
  const { prisma } = await import('../src/db/prisma.js')
  const { syncHostGpuReport, reserveInstanceGpu, findAvailableGpu, getGpuPolicy, getInstanceGpuPci, assertGpuPlanChange, assertGpuMigration, validateGpuSelection } = await import('../src/db/gpus.js')
  const { deleteInstance, startInstance } = await import('../src/lib/incus/incus-instances.js')
  const { default: Fastify } = await import('fastify')
  const { default: gpuRoutes } = await import('../src/routes/gpus.js')
  const prefix = `gpu-test-${randomUUID()}`
  const owner = await prisma.user.create({ data: { username: prefix, passwordHash: 'test-only', balance: 100 } })
  let app: ReturnType<typeof Fastify> | undefined
  try {
    const host = await prisma.host.create({ data: { userId: owner.id, name: prefix, url: 'https://127.0.0.1:18443', status: 'online' } })
    const report = { status: 'ready', runtimeReady: true, cards: [1, 2].map((n) => ({ uuid: `GPU-${String(n).repeat(8)}-1111-1111-1111-111111111111`, pciAddress: n === 1 ? '0000:65:00.0' : '0000:b3:00.0', name: 'NVIDIA A10', memoryTotalMiB: 23028, memoryUsedMiB: 1024, gpuUtilization: n * 10, driverVersion: '550', computeMode: 'Default' })) }
    await syncHostGpuReport(host.id, report)
    const gpus = await prisma.hostGpu.findMany({ where: { hostId: host.id }, orderBy: { id: 'asc' } })
    assert.equal(gpus.length, 2)
    for (const [index, gpu] of gpus.entries()) await prisma.hostGpu.update({ where: { id: gpu.id }, data: { sharingEnabled: true, maxInstances: index + 1 } })
    const pkg = await prisma.package.create({ data: { userId: owner.id, name: prefix, cpuMax: 100, memoryMax: 1024, diskMax: 10240, gpuShared: true, gpuDeviceIds: [gpus[0].id], packageHosts: { create: { hostId: host.id } } } })
    const base = { userId: owner.id, hostId: host.id, packageId: pkg.id, image: 'test', cpu: 100, memory: 512, disk: 1024 }
    const create = async (gpuIds: number[]) => prisma.$transaction(async tx => {
      const target = await tx.package.create({ data: { userId: owner.id, name: randomUUID(), cpuMax: 100, memoryMax: 1024, diskMax: 10240, gpuShared: true, gpuDeviceIds: gpuIds } })
      const assignment = await reserveInstanceGpu(tx, { hostId: host.id, packageId: target.id })
      return tx.instance.create({ data: { ...base, packageId: target.id, ...assignment, incusId: randomUUID(), name: randomUUID() } })
    }, { timeout: 15_000 })
    const race = await Promise.allSettled([create([gpus[0].id]), create([gpus[0].id])])
    assert.equal(race.filter(result => result.status === 'fulfilled').length, 1, 'Only one buyer can reserve the final seat')
    const first = (race.find(result => result.status === 'fulfilled') as PromiseFulfilledResult<Awaited<ReturnType<typeof create>>>).value
    const second = await create([gpus[1].id])
    assert.equal(second.gpuDeviceId, gpus[1].id, 'Other pool remains independent')
    await prisma.instance.update({ where: { id: first.id }, data: { status: 'stopped' } })
    await assert.rejects(create([gpus[0].id]), /GPU_RESOURCES_INSUFFICIENT/)
    // Destroy first claims status=deleted; the seat must remain held until the
    // physical Incus deletion has actually completed.
    await prisma.instance.update({ where: { id: first.id }, data: { status: 'deleted' } })
    await assert.rejects(create([gpus[0].id]), /GPU_RESOURCES_INSUFFICIENT/)
    const failingClient = { options: { hostId: host.id }, request: async () => { throw Object.assign(new Error('offline'), { statusCode: 503 }) } }
    await assert.rejects(deleteInstance(failingClient as any, first.incusId))
    await assert.rejects(create([gpus[0].id]), /GPU_RESOURCES_INSUFFICIENT/)
    const absentClient = { options: { hostId: host.id }, request: async () => { throw Object.assign(new Error('absent'), { statusCode: 404 }) } }
    await deleteInstance(absentClient as any, first.incusId)
    const replacement = await create([gpus[0].id])
    assert.equal(replacement.gpuDeviceId, gpus[0].id)
    const beforeCount = await prisma.instance.count({ where: { hostId: host.id } })
    await assert.rejects(prisma.$transaction(async tx => {
      await tx.package.update({ where: { id: pkg.id }, data: { gpuDeviceIds: [gpus[1].id] } })
      const assignment = await reserveInstanceGpu(tx, { hostId: host.id, packageId: pkg.id })
      await tx.instance.create({ data: { ...base, ...assignment, incusId: randomUUID(), name: randomUUID() } })
      await tx.user.update({ where: { id: owner.id }, data: { balance: { decrement: 10 } } })
      throw new Error('rollback purchase')
    }), /rollback purchase/)
    assert.equal(Number((await prisma.user.findUniqueOrThrow({ where: { id: owner.id } })).balance), 100)
    assert.equal(await prisma.instance.count({ where: { hostId: host.id } }), beforeCount)
    const calls: Array<{ method: string; body: any }> = []
    const mockClient = { options: { hostId: host.id }, request: async (method: string, path: string, body: unknown) => { calls.push({ method, body }); return method === 'GET' ? { name: second.incusId, type: 'container', profiles: ['unsafe-default'], devices: { other: { type: 'gpu' } }, config: {} } : {} } }
    await startInstance(mockClient as any, second.incusId)
    assert.deepEqual(calls.find(call => call.method === 'PUT')?.body.devices.gpu0, { type: 'gpu', gputype: 'physical', pci: '0000:b3:00.0' })
    assert.equal(calls.find(call => call.method === 'PUT')?.body.devices.other, undefined)
    assert.deepEqual(calls.find(call => call.method === 'PUT')?.body.profiles, [])
    await assert.rejects(assertGpuMigration(second.id))
    await assert.rejects(validateGpuSelection(prisma, { gpuShared: true, gpuDeviceIds: [gpus[0].id], hostIds: [host.id + 10000], instanceType: 'container', privileged: false }))
    await assert.rejects(validateGpuSelection(prisma, { gpuShared: true, gpuDeviceIds: [gpus[0].id], hostIds: [host.id], instanceType: 'vm', privileged: false }))
    const plan = await prisma.packagePlan.create({ data: { packageId: replacement.packageId!, name: 'CPU only', cpu: 100, memory: 512, disk: 1024, portLimit: 0, snapshotLimit: 0, backupLimit: 0, siteLimit: 0, swapSize: 0, trafficLimit: 0, price: 100, gpuShared: false } })
    assert.equal((await getGpuPolicy(prisma, replacement.packageId, plan.id)).gpuShared, false)
    await assert.rejects(assertGpuPlanChange(prisma, replacement.id, plan.id))
    // API authorization and per-device config checks use the actual route plugin.
    app = Fastify()
    app.decorate('authenticate', async (request: any) => { request.user = { id: request.headers['x-outsider'] ? owner.id + 10000 : owner.id, role: 'user' } })
    await app.register(gpuRoutes, { prefix: '/api' })
    assert.equal((await app.inject({ url: `/api/hosts/${host.id}/gpus`, headers: { 'x-outsider': 'yes' } })).statusCode, 403)
    assert.equal((await app.inject({ url: `/api/hosts/${host.id}/gpus` })).json().gpus.length, 2)
    assert.equal((await app.inject({ method: 'PUT', url: `/api/hosts/${host.id}/gpus/${gpus[0].id}`, payload: { sharingEnabled: true, maxInstances: 0 } })).statusCode, 400)
    assert.equal((await app.inject({ method: 'PUT', url: `/api/hosts/${host.id}/gpus/${gpus[0].id}`, headers: { 'x-outsider': 'yes' }, payload: { sharingEnabled: true, maxInstances: 10 } })).statusCode, 403)
    assert.equal((await app.inject({ url: `/api/instances/${second.id}/gpu` })).json().gpu.name, 'NVIDIA A10')
    const pool = await app.inject({ method: 'PUT', url: `/api/hosts/${host.id}/gpus/${gpus[1].id}`, payload: { sharingEnabled: false, maxInstances: 2 } })
    assert.equal(pool.statusCode, 200, pool.body)
    assert.equal(await getInstanceGpuPci(second.id, host.id), '0000:b3:00.0', 'Disabling sales preserves existing seats')
    await syncHostGpuReport(host.id, { ...report, cards: [report.cards[0]] })
    assert.equal((await prisma.hostGpu.findUniqueOrThrow({ where: { id: gpus[1].id } })).available, false)
    await syncHostGpuReport(host.id, report)
    assert.equal((await prisma.hostGpu.findUniqueOrThrow({ where: { id: gpus[1].id } })).sharingEnabled, false, 'Heartbeat cannot enable sales or overwrite limits')
    await prisma.hostGpu.update({ where: { id: gpus[0].id }, data: { lastSeenAt: new Date(Date.now() - 200_000) } })
    assert.equal(await findAvailableGpu(prisma, host.id, { gpuShared: true, gpuDeviceIds: [gpus[0].id] }), null)
    await syncHostGpuReport(host.id, { status: 'error' })
    assert.equal(await prisma.hostGpu.count({ where: { hostId: host.id, available: true } }), 0)
    console.log('GPU PostgreSQL integration passed: concurrent last-seat reservation, independent cards, billing rollback, confirmed deletion, restore binding, lifecycle restrictions, telemetry freshness and API authorization')
  } finally {
    if (app) await app.close()
    await prisma.instance.deleteMany({ where: { userId: owner.id } })
    await prisma.package.deleteMany({ where: { userId: owner.id } })
    await prisma.host.deleteMany({ where: { userId: owner.id } })
    await prisma.user.delete({ where: { id: owner.id } })
    await prisma.$disconnect()
  }
}
