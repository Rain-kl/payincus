import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { storageHeadroomMB, unconsumedStorageMB } from '../src/lib/storage-capacity-policy.js'

assert.equal(storageHeadroomMB(100000), 1024)
assert.equal(storageHeadroomMB(4500000), 10240)
const allocations = [{ incusId: 'a', disk: 30720 }, { incusId: 'b', disk: 30720 }]
assert.equal(unconsumedStorageMB(allocations, new Map(), 2000000), 61440, 'Unknown volume usage must keep the full promise')
assert.equal(unconsumedStorageMB(allocations, new Map([['a', 10240], ['b', 10240]]), 2000000), 40960)
assert.equal(unconsumedStorageMB(allocations, new Map([['a', 1000000], ['b', 1000000]]), 10240), 51200, 'Credit must never exceed physically occupied space')
console.log('Storage capacity reservation policy tests passed')

if (process.env.RUN_STORAGE_DB_TESTS === '1') {
  const target = new URL(process.env.DATABASE_URL || '')
  assert.ok(['127.0.0.1', 'localhost'].includes(target.hostname))
  assert.ok(target.pathname.endsWith('_test'), 'Use a disposable local database ending in _test')
  const { prisma, closePrismaDatabase } = await import('../src/db/prisma.js')
  const { IncusClient } = await import('../src/lib/incus/incus-client.js')
  const { closeAllClients } = await import('../src/lib/incus/incus-pool.js')
  const { getStorageCapacity, getStorageOverview, invalidateHostStorageCapacity } = await import('../src/lib/storage-capacity.js')
  const { checkPackageSoldOut, checkPackagesSoldOut } = await import('../src/db/packages.js')
  const { selectAndReserveHostWithLock, checkPlanUpgradeCapacity, reservePlanUpgradeCapacityWithLock } = await import('../src/db/hosts.js')
  const { default: Fastify } = await import('fastify')
  const { default: instanceRoutes } = await import('../src/routes/instances.js')
  const originalConnect = IncusClient.prototype.connect
  const originalRequest = IncusClient.prototype.request
  const spaces = new Map<string, { total: number; used: number }>()
  let readFailure = false
  const GiB = 1024 ** 3
  IncusClient.prototype.connect = async function () { this.connected = true; return {} }
  IncusClient.prototype.request = async function <T>(_method: string, path: string): Promise<T> {
    if (readFailure) throw new Error('simulated unreachable daemon')
    const pool = decodeURIComponent(path.split('/')[3] || '')
    if (path.includes('/volumes/')) return { space: { used: 0 } } as T
    if (path.endsWith('/resources') && spaces.has(pool)) return { space: spaces.get(pool) } as T
    throw new Error('simulated missing pool')
  }
  const owner = await prisma.user.create({ data: { username: `capacity-${randomUUID()}`, passwordHash: 'test-only' } })
  const app = Fastify()
  const auth = async (request: any) => { request.user = { id: owner.id, role: 'user' } }
  app.decorate('authenticate', auth)
  app.decorate('authenticateUser', auth)
  app.decorate('authenticateAdmin', auth)
  try {
    await app.register(instanceRoutes, { prefix: '/api/instances' })
    const host = await prisma.host.create({ data: {
      userId: owner.id, name: 'capacity-host', url: 'https://127.0.0.1:58443', status: 'online',
      instanceType: 'both', cpuAllowanceMax: 10000, memoryMax: 102400, storageSize: 60,
      cpuUsed: 400, memoryUsed: 8192, diskUsed: 61440
    } })
    await prisma.storagePool.create({ data: { hostId: host.id, name: 'data', driver: 'dir', purpose: 'instance_data' } })
    spaces.set('data', { total: 4468 * GiB, used: 1690 * GiB })
    const packages = await Promise.all((['container', 'vm'] as const).map(instanceType => prisma.package.create({ data: {
      userId: owner.id, name: `capacity-${instanceType}`, cpuMax: 3000, memoryMax: 102400, diskMax: 102400,
      instanceType, packageHosts: { create: { hostId: host.id, storagePoolName: 'data' } }
    } })))
    for (const pkg of packages) await prisma.packagePlan.create({ data: { packageId: pkg.id, name: 'Basic', cpu: 200, memory: 4096, disk: 30720, price: 10, portLimit: 0, snapshotLimit: 0, backupLimit: 0, siteLimit: 0, trafficLimit: 0 } })
    await prisma.instance.createMany({ data: [1, 2].map(i => ({
      userId: owner.id, hostId: host.id, packageId: packages[0].id, incusId: `capacity-${i}`, name: `capacity-${i}`,
      image: 'test', status: 'running' as const, cpu: 200, memory: 4096, disk: 30720, storagePoolName: 'data'
    })) })
    const available = () => app.inject({ url: `/api/instances/available-hosts?packageId=${packages[0].id}&cpu=200&memory=4096&disk=30720` })
    const refresh = () => invalidateHostStorageCapacity(host.id)
    assert.equal((await getStorageCapacity(host.id, { packageId: packages[0].id, requiredMB: 30720 })).reason, null)
    for (const pkg of packages) assert.equal(await checkPackageSoldOut(pkg.id), false, 'Initialization size 60 GB must not mark LXC or KVM sold out')
    const batch = await checkPackagesSoldOut(packages.map(pkg => pkg.id))
    assert.ok(packages.every(pkg => batch.get(pkg.id) === false))
    const response = await available()
    assert.equal(response.statusCode, 200, response.body)
    assert.deepEqual(response.json().hosts.map((h: any) => h.id), [host.id])
    assert.ok(response.json().hosts[0].resources.diskAvailable > 2_000_000)
    assert.equal((await prisma.host.findUniqueOrThrow({ where: { id: host.id } })).storageSize, 60)

    // A real HTTP request must honor the capacity reader's abort signal.
    const { createServer } = await import('node:http')
    const { Agent } = await import('undici')
    const slow = createServer(() => { /* Deliberately leave headers unanswered. */ })
    await new Promise<void>(resolve => slow.listen(0, '127.0.0.1', resolve))
    const transport = new IncusClient({ url: `http://127.0.0.1:${(slow.address() as any).port}`, certPath: null, keyPath: null })
    transport.agent = new Agent()
    try {
      await assert.rejects(originalRequest.call(transport, 'GET', '/slow', null, 100, AbortSignal.timeout(30)), /abort|timeout/i)
    } finally {
      await transport.agent.destroy()
      slow.closeAllConnections()
      await new Promise<void>(resolve => slow.close(() => resolve()))
    }

    const instance = await prisma.instance.findFirstOrThrow({ where: { hostId: host.id } })
    const larger = { cpu: instance.cpu, memory: instance.memory, disk: instance.disk + 10240 }
    assert.equal((await checkPlanUpgradeCapacity(instance, larger)).canUpgrade, true)
    await prisma.$transaction(async tx => {
      assert.equal((await reservePlanUpgradeCapacityWithLock(tx, instance, larger)).canUpgrade, true)
      await tx.instance.update({ where: { id: instance.id }, data: { disk: larger.disk } })
    })

    // Resource-pool expansion must use the same pool and roll back its debit
    // when capacity cannot be read or the target pool is full.
    const { applyResourcePoolToInstance } = await import('../src/db/resource-pool.js')
    await prisma.userResourcePool.create({ data: { userId: owner.id, disk: 20480 } })
    const claim = () => applyResourcePoolToInstance({ userId: owner.id, instanceId: instance.id, hostId: host.id,
      resourceType: 'd', amount: 1024, hostResourceDelta: { diskUsed: 1024 },
      instanceResources: { disk: larger.disk + 1024 } })
    assert.ok(await claim())
    assert.equal((await prisma.userResourcePool.findUniqueOrThrow({ where: { userId: owner.id } })).disk, 19456)

    // Full and unreachable are different public API reasons.
    spaces.set('data', { total: 4468 * GiB, used: 4467 * GiB }); refresh()
    assert.equal((await available()).json().unavailableHosts[0].reason, 'disk_full')
    assert.equal(await checkPackageSoldOut(packages[0].id), true)
    await assert.rejects(claim(), /HOST_RESOURCES_INSUFFICIENT/)
    assert.equal((await prisma.userResourcePool.findUniqueOrThrow({ where: { userId: owner.id } })).disk, 19456, 'Rejected growth rolls back the debit')
    readFailure = true; refresh()
    assert.equal((await available()).json().unavailableHosts[0].reason, 'storage_unavailable')
    readFailure = false
    spaces.set('data', { total: 4468 * GiB, used: 1690 * GiB }); refresh()

    // A broken explicit binding cannot silently spill into another pool.
    await prisma.packageHost.update({ where: { packageId_hostId: { packageId: packages[0].id, hostId: host.id } }, data: { storagePoolName: 'missing' } })
    assert.equal((await available()).json().unavailableHosts[0].reason, 'storage_pool_missing')
    await prisma.packageHost.update({ where: { packageId_hostId: { packageId: packages[0].id, hostId: host.id } }, data: { storagePoolName: null } })
    await prisma.storagePool.create({ data: { hostId: host.id, name: 'full', driver: 'zfs', purpose: 'instance_data', config: { source: 'full-tank' } } })
    spaces.set('full', { total: 100 * GiB, used: 100 * GiB }); refresh()
    assert.equal((await getStorageCapacity(host.id, { packageId: packages[0].id, requiredMB: 30720 })).poolName, 'data')

    // Alias DIR pools do not multiply physical capacity or escape reservations.
    await prisma.storagePool.create({ data: { hostId: host.id, name: 'alias', driver: 'dir', purpose: 'instance_data' } })
    spaces.set('alias', spaces.get('data')!); refresh()
    const overview = await getStorageOverview(host.id)
    assert.equal(overview.totalMB, (4468 + 100) * 1024)
    const beforeAlias = await getStorageCapacity(host.id, { poolName: 'alias' })
    const beforeData = await getStorageCapacity(host.id, { poolName: 'data' })
    assert.equal(beforeAlias.availableMB, beforeData.availableMB)

    // Leave room for exactly one 30 GB reservation; cached physical metrics must
    // not let two simultaneous transactions both consume it.
    await prisma.instance.deleteMany({ where: { hostId: host.id } })
    await prisma.host.update({ where: { id: host.id }, data: { cpuUsed: 0, memoryUsed: 0, diskUsed: 0 } })
    spaces.set('data', { total: 40 * GiB, used: 0 }); spaces.set('alias', spaces.get('data')!); refresh()
    const create = () => prisma.$transaction(async tx => {
      const selected = await selectAndReserveHostWithLock(tx, { packageHostIds: [host.id], hostId: host.id, packageId: packages[0].id, cpu: 200, memory: 4096, disk: 30720 })
      if (!selected) throw new Error('storage full')
      return tx.instance.create({ data: {
        userId: owner.id, hostId: host.id, packageId: packages[0].id, incusId: randomUUID(), name: randomUUID(),
        image: 'test', status: 'creating', cpu: 200, memory: 4096, disk: 30720, storagePoolName: selected.storage_pool_name
      } })
    }, { timeout: 15000 })
    const race = await Promise.allSettled([create(), create()])
    assert.equal(race.filter(r => r.status === 'fulfilled').length, 1)
    assert.equal(await prisma.instance.count({ where: { hostId: host.id } }), 1)
    console.log('Storage capacity DB/API tests passed: legacy 60 GB regression, LXC/KVM stock, expansion, full/offline/broken bindings, pool choice, shared DIR capacity, and concurrent reservations')
  } finally {
    await app.close()
    await closeAllClients()
    IncusClient.prototype.connect = originalConnect
    IncusClient.prototype.request = originalRequest
    await prisma.instance.deleteMany({ where: { userId: owner.id } })
    await prisma.package.deleteMany({ where: { userId: owner.id } })
    await prisma.host.deleteMany({ where: { userId: owner.id } })
    await prisma.user.delete({ where: { id: owner.id } })
    await closePrismaDatabase()
  }
}
