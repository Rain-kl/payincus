/** Local-only business smoke: real HTTP routes and PostgreSQL, simulated Incus.
 * No GPU hardware, external host, driver installation or production credentials.
 */
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { execFileSync } from 'node:child_process'
import { createServer } from 'node:https'
import Fastify from 'fastify'
import { prisma } from '../src/db/prisma.js'
import gpuRoutes from '../src/routes/gpus.js'
import packageRoutes from '../src/routes/packages.js'
import instanceRoutes from '../src/routes/instances.js'
import { syncHostGpuReport } from '../src/db/gpus.js'
import { closeAllClients } from '../src/lib/incus/incus-pool.js'

assert.ok(['127.0.0.1', 'localhost'].includes(new URL(process.env.DATABASE_URL || '').hostname), 'Local database required')
const folder = mkdtempSync(join(tmpdir(), 'gpu-purchase-'))
const cert = join(folder, 'cert.pem'), key = join(folder, 'key.pem')
execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-days', '1', '-subj', '/CN=localhost'], { stdio: 'ignore' })
const guests = new Map<string, any>()
let failNextCreate = false
let failNextGpuCheck = false
const gpuUuidsByPci = new Map<string, string>()
const execLogs = new Map<string, string>()
const incus = createServer({ cert: readFileSync(cert), key: readFileSync(key) }, async (req, res) => {
  let raw = ''; for await (const chunk of req) raw += chunk
  const body = raw ? JSON.parse(raw) : {}
  const path = new URL(req.url || '/', 'https://localhost').pathname
  const reply = (metadata: any, status = 200) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify(status >= 400 ? { type: 'error', error: metadata, error_code: status } : { type: 'sync', status: 'Success', status_code: 200, metadata })) }
  if (path === '/1.0') return reply({ environment: { server_name: 'GPU smoke fixture' }, api_extensions: ['gpu', 'container_nvidia'] })
  if (path === '/1.0/instances' && req.method === 'POST') {
    if (failNextCreate) { failNextCreate = false; return reply('simulated NVIDIA runtime failure', 500) }
    guests.set(body.name, { ...body, status: 'Stopped', status_code: 102 }); return reply({})
  }
  if (execLogs.has(path)) { res.writeHead(200, { 'content-type': 'text/plain' }); return res.end(execLogs.get(path)) }
  const match = path.match(/^\/1.0\/instances\/([^/]+)(\/state|\/exec)?$/)
  if (match) {
    const guest = guests.get(match[1])
    if (!guest) return reply('Instance not found', 404)
    if (match[2] === '/state') {
      if (req.method === 'PUT') { guest.status = body.action === 'stop' ? 'Stopped' : 'Running'; guest.status_code = body.action === 'stop' ? 102 : 103 }
      return reply({ status: guest.status, status_code: guest.status_code, network: {}, cpu: { usage: 0 }, memory: { usage: 0 }, disk: {} })
    }
    if (match[2] === '/exec') {
      if (body.command?.[0] === '/usr/bin/nvidia-smi') {
        const stdout = `/1.0/instances/${match[1]}/logs/gpu-stdout`
        const stderr = `/1.0/instances/${match[1]}/logs/gpu-stderr`
        const fail = failNextGpuCheck; failNextGpuCheck = false
        execLogs.set(stdout, fail ? '' : Object.values(guest.devices).filter((device: any) => device.type === 'gpu').map((device: any) => gpuUuidsByPci.get(device.pci)).join('\n'))
        execLogs.set(stderr, fail ? 'Error relocating nvidia-smi: gnu_get_libc_version: symbol not found' : '')
        return reply({ return: fail ? 127 : 0, output: { '1': stdout, '2': stderr } })
      }
      return reply({ return: 0, output: {} })
    }
    if (req.method === 'DELETE') { guests.delete(match[1]); return reply({}) }
    if (req.method === 'PUT') { guests.set(match[1], { ...guest, ...body }); return reply({}) }
    return reply(guest)
  }
  if (path.startsWith('/1.0/networks/')) return reply({ config: { 'ipv4.address': '10.10.0.1/22' } })
  return reply({})
})
await new Promise<void>(resolve => incus.listen(0, '127.0.0.1', resolve))
const port = (incus.address() as { port: number }).port
const app = Fastify()
const prefix = `gpu-smoke-${randomUUID().slice(0, 12)}`
const seller = await prisma.user.create({ data: { username: `${prefix}-seller`, passwordHash: 'test-only', role: 'admin' } })
const buyer = await prisma.user.create({ data: { username: `${prefix}-buyer`, passwordHash: 'test-only', balance: 100 } })
let hostId: number | undefined
let imageId: number | undefined
async function waitFor<T>(read: () => Promise<T>, predicate: (value: T) => boolean, label: string) {
  for (let i = 0; i < 150; i++) { const value = await read(); if (predicate(value)) return value; await new Promise(resolve => setTimeout(resolve, 100)) }
  throw new Error(`Timed out: ${label}`)
}
try {
  const host = await prisma.host.create({ data: { userId: seller.id, name: prefix, url: `https://127.0.0.1:${port}`, status: 'online', certPath: cert, keyPath: key, cpuAllowanceMax: 10000, memoryMax: 32768, storageSize: 100, natPublicIp: '127.0.0.1', natPortStart: 20000, natPortEnd: 25000 } }); hostId = host.id
  await prisma.storagePool.create({ data: { hostId, name: 'default', driver: 'dir' } })
  const image = await prisma.systemImage.create({ data: { name: 'GPU smoke Linux', remoteAlias: `${prefix}-image`, icon: 'ubuntu', instanceType: 'container' } }); imageId = image.id
  await syncHostGpuReport(hostId, { status: 'ready', runtimeReady: true, cards: [1, 2].map(n => ({ uuid: `GPU-${String(n).repeat(8)}-2222-2222-2222-222222222222`, pciAddress: n === 1 ? '0000:65:00.0' : '0000:b3:00.0', name: 'NVIDIA A10', memoryTotalMiB: 23028, computeMode: 'Default' })) })
  const cards = await prisma.hostGpu.findMany({ where: { hostId }, orderBy: { id: 'asc' } })
  for (const card of cards) gpuUuidsByPci.set(card.pciAddress, card.uuid)
  const authenticate = async (request: any) => { request.user = request.headers['x-seller'] ? seller : buyer }
  app.decorate('authenticate', authenticate); app.decorate('authenticateUser', authenticate)
  await app.register(gpuRoutes, { prefix: '/api' })
  await app.register(packageRoutes, { prefix: '/api/packages' })
  await app.register(instanceRoutes, { prefix: '/api/instances' })
  for (const [i, card] of cards.entries()) {
    const response = await app.inject({ method: 'PUT', url: `/api/hosts/${hostId}/gpus/${card.id}`, headers: { 'x-seller': 'yes' }, payload: { sharingEnabled: true, maxInstances: i + 1 } })
    assert.equal(response.statusCode, 200, response.body)
  }
  const packageResponse = await app.inject({ method: 'POST', url: '/api/packages', headers: { 'x-seller': 'yes' }, payload: { name: prefix, hostIds: [hostId], cpuMax: 1000, memoryMax: 8192, diskMax: 51200, gpuShared: true, gpuDeviceIds: [cards[0].id], globalShared: true, globalMaxInstances: 5 } })
  assert.equal(packageResponse.statusCode, 201, packageResponse.body)
  const packageId = packageResponse.json().package.id
  const packageDetails = await app.inject({ url: `/api/packages/${packageId}`, headers: { 'x-seller': 'yes' } })
  assert.equal(packageDetails.statusCode, 200, packageDetails.body)
  assert.equal(packageDetails.json().package.gpu_shared, true, 'Editing must receive the persisted GPU checkbox')
  assert.deepEqual(packageDetails.json().package.gpu_device_ids, [cards[0].id], 'Editing must restore selected cards')
  const plans: number[] = []
  for (let i = 0; i < 2; i++) {
    const response = await app.inject({ method: 'POST', url: `/api/packages/${packageId}/plans`, headers: { 'x-seller': 'yes' }, payload: { name: `GPU pool ${i + 1}`, cpu: 100, memory: 1024, disk: 5120, portLimit: 3, snapshotLimit: 1, backupLimit: 0, siteLimit: 1, swapSize: 0, trafficLimit: '1073741824', price: (i + 1) * 100, ...(i === 1 ? { gpuShared: true, gpuDeviceIds: [cards[1].id] } : {}) } })
    assert.equal(response.statusCode, 200, response.body); plans.push(response.json().id)
  }
  // Editing must persist the same policy as creation, including nullable inheritance.
  for (const card of [cards[1], cards[0]]) {
    const response = await app.inject({ method: 'PATCH', url: `/api/packages/${packageId}`, headers: { 'x-seller': 'yes' }, payload: { gpuShared: true, gpuDeviceIds: [card.id] } })
    assert.equal(response.statusCode, 200, response.body)
    assert.deepEqual((await prisma.package.findUniqueOrThrow({ where: { id: packageId } })).gpuDeviceIds, [card.id])
  }
  for (const policy of [{ gpuShared: null, gpuDeviceIds: [] }, { gpuShared: true, gpuDeviceIds: [cards[1].id] }]) {
    const response = await app.inject({ method: 'PUT', url: `/api/packages/${packageId}/plans/${plans[1]}`, headers: { 'x-seller': 'yes' }, payload: policy })
    assert.equal(response.statusCode, 200, response.body)
    const saved = await prisma.packagePlan.findUniqueOrThrow({ where: { id: plans[1] } })
    assert.equal(saved.gpuShared, policy.gpuShared); assert.deepEqual(saved.gpuDeviceIds, policy.gpuDeviceIds)
  }
  const privileged = await app.inject({ method: 'PATCH', url: `/api/packages/${packageId}`, headers: { 'x-seller': 'yes' }, payload: { privileged: true } })
  assert.equal(privileged.statusCode, 400, privileged.body)
  assert.equal((await prisma.package.findUniqueOrThrow({ where: { id: packageId } })).privileged, false)
  const purchase = (planId: number) => app.inject({ method: 'POST', url: '/api/instances', payload: { name: `${prefix}-${randomUUID().slice(0, 8)}`, packageId, planId, hostId, image: image.remoteAlias, autoRemotePort: false, idempotencyKey: randomUUID() } })
  const instanceIds: number[] = []
  for (let i = 0; i < 2; i++) {
    const response = await purchase(plans[i]); assert.equal(response.statusCode, 202, response.body)
    const instance = await waitFor(() => prisma.instance.findFirst({ where: { userId: buyer.id, packagePlanId: plans[i] }, orderBy: { id: 'desc' } }), value => value?.status === 'running', 'GPU purchase provisioning')
    assert.ok(instance); instanceIds.push(instance.id)
    assert.equal(instance.gpuDeviceId, cards[i].id)
    assert.equal(guests.get(instance.incusId).devices.gpu0.pci, cards[i].pciAddress)
    assert.equal(guests.get(instance.incusId).config['nvidia.runtime'], 'true')
    await waitFor(() => prisma.log.count({ where: { instanceId: instance.id, action: 'instance.create' } }), count => count > 0, 'provisioner completion')
  }
  assert.equal(Number((await prisma.user.findUniqueOrThrow({ where: { id: buyer.id } })).balance), 97)
  const soldOut = await purchase(plans[0]); assert.ok([409, 503].includes(soldOut.statusCode), soldOut.body)
  assert.equal(Number((await prisma.user.findUniqueOrThrow({ where: { id: buyer.id } })).balance), 97, 'Full GPU pool must not charge')
  failNextCreate = true
  const failed = await purchase(plans[1]); assert.equal(failed.statusCode, 202, failed.body)
  const failedInstance = await waitFor(() => prisma.instance.findFirst({ where: { userId: buyer.id, packagePlanId: plans[1] }, orderBy: { id: 'desc' } }), value => value?.status === 'error' && value.gpuReleasedAt !== null, 'failed GPU creation cleanup')
  assert.ok(failedInstance)
  await waitFor(() => prisma.user.findUniqueOrThrow({ where: { id: buyer.id } }), user => Number(user.balance) === 97, 'failed purchase refund')
  assert.equal(await prisma.instanceBillingRecord.count({ where: { instanceId: failedInstance.id, type: 'refund' } }), 1)
  const cpuPlan = await app.inject({ method: 'POST', url: `/api/packages/${packageId}/plans`, headers: { 'x-seller': 'yes' }, payload: { name: 'CPU only', cpu: 100, memory: 1024, disk: 5120, portLimit: 3, snapshotLimit: 1, backupLimit: 0, siteLimit: 1, swapSize: 0, trafficLimit: '1073741824', price: 100, gpuShared: false } })
  assert.equal(cpuPlan.statusCode, 200, cpuPlan.body)
  const cpuPurchase = await purchase(cpuPlan.json().id); assert.equal(cpuPurchase.statusCode, 202, cpuPurchase.body)
  const cpuInstance = await waitFor(() => prisma.instance.findFirst({ where: { userId: buyer.id, packagePlanId: cpuPlan.json().id } }), value => value?.status === 'running', 'CPU-only delivery alongside full GPU pool')
  assert.ok(cpuInstance); assert.equal(cpuInstance.gpuDeviceId, null)
  assert.equal(guests.get(cpuInstance.incusId).devices.gpu0, undefined)
  assert.equal(guests.get(cpuInstance.incusId).config['nvidia.runtime'], undefined)
  await waitFor(() => prisma.log.count({ where: { instanceId: cpuInstance.id, action: 'instance.create' } }), count => count > 0, 'CPU provisioner completion')
  assert.equal(Number((await prisma.user.findUniqueOrThrow({ where: { id: buyer.id } })).balance), 96)
  for (const card of cards) {
    const response = await app.inject({ method: 'PUT', url: `/api/hosts/${hostId}/gpus/${card.id}`, headers: { 'x-seller': 'yes' }, payload: { sharingEnabled: true, maxInstances: 3 } })
    assert.equal(response.statusCode, 200, response.body)
  }
  const bundlePlan = await app.inject({ method: 'POST', url: `/api/packages/${packageId}/plans`, headers: { 'x-seller': 'yes' }, payload: { name: 'Dual A10', cpu: 100, memory: 1024, disk: 5120, portLimit: 3, snapshotLimit: 1, backupLimit: 0, siteLimit: 1, swapSize: 0, trafficLimit: '1073741824', price: 300, gpuShared: true, gpuDeviceIds: cards.map(card => card.id) } })
  assert.equal(bundlePlan.statusCode, 200, bundlePlan.body)
  const offers = await app.inject({ url: `/api/packages/${packageId}/plans?activeOnly=true` })
  assert.equal(offers.statusCode, 200, offers.body)
  const offer = offers.json().plans.find((plan: any) => plan.id === bundlePlan.json().id)
  assert.deepEqual(offer.gpuConfigurations, [{ hostIds: [hostId], devices: [{ name: 'NVIDIA A10', memoryTotalMiB: 23028, count: 2 }], count: 2 }])
  assert.equal(offers.json().plans.find((plan: any) => plan.id === plans[0]).gpuConfigurations[0].count, 1, 'Package GPU inheritance must show the actual hardware')
  assert.deepEqual(offers.json().plans.find((plan: any) => plan.id === cpuPlan.json().id).gpuConfigurations, [])
  const bundlePurchase = await purchase(bundlePlan.json().id); assert.equal(bundlePurchase.statusCode, 202, bundlePurchase.body)
  const bundleInstance = await waitFor(() => prisma.instance.findFirst({ where: { userId: buyer.id, packagePlanId: bundlePlan.json().id } }), value => value?.status === 'running', 'multi-GPU delivery')
  assert.ok(bundleInstance)
  assert.equal(await prisma.instanceGpuAllocation.count({ where: { instanceId: bundleInstance.id } }), 2)
  assert.deepEqual([guests.get(bundleInstance.incusId).devices.gpu0.pci, guests.get(bundleInstance.incusId).devices.gpu1.pci], cards.map(card => card.pciAddress))
  const gpuInfo = await app.inject({ url: `/api/instances/${bundleInstance.id}/gpu` })
  assert.equal(gpuInfo.json().gpu.count, 2)
  assert.equal(gpuInfo.json().gpu.devices[0].count, 2)
  await waitFor(() => prisma.log.count({ where: { instanceId: bundleInstance.id, action: 'instance.create' } }), count => count > 0, 'bundle provisioner completion')
  assert.equal(Number((await prisma.user.findUniqueOrThrow({ where: { id: buyer.id } })).balance), 93)
  failNextGpuCheck = true
  const incompatible = await purchase(plans[1]); assert.equal(incompatible.statusCode, 202, incompatible.body)
  const incompatibleInstance = await waitFor(() => prisma.instance.findFirst({ where: { userId: buyer.id, packagePlanId: plans[1] }, orderBy: { id: 'desc' } }), value => value?.status === 'error' && value.gpuReleasedAt !== null, 'glibc-incompatible GPU image rejection')
  assert.ok(incompatibleInstance)
  await waitFor(() => prisma.user.findUniqueOrThrow({ where: { id: buyer.id } }), user => Number(user.balance) === 93, 'GPU ABI failure refund')
  assert.equal(guests.has(incompatibleInstance.incusId), false)
  assert.equal(await prisma.instanceBillingRecord.count({ where: { instanceId: incompatibleInstance.id, type: 'refund' } }), 1)
  console.log('GPU business smoke passed: package edit round-trip; inherited/CPU/dual-GPU catalogue; exact dual PCI delivery and billing; capacity rollback and refunds; musl/glibc failure detection; CPU-only coexistence')
} finally {
  await app.close(); await closeAllClients()
  await new Promise<void>(resolve => incus.close(() => resolve()))
  await prisma.instance.deleteMany({ where: { userId: buyer.id } })
  await prisma.package.deleteMany({ where: { userId: seller.id } })
  await prisma.host.deleteMany({ where: { userId: seller.id } })
  if (imageId) await prisma.systemImage.delete({ where: { id: imageId } })
  await prisma.user.deleteMany({ where: { id: { in: [seller.id, buyer.id] } } })
  await prisma.$disconnect(); rmSync(folder, { recursive: true, force: true })
}
