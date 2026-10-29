import assert from 'node:assert/strict'
import { evaluateHostService, instanceServiceStatus, loadHostServiceStatuses, instanceServiceStatusFilter } from '../src/lib/host-service-status.js'
import { prisma } from '../src/db/prisma.js'
const now = Date.now()
const online = { id: 77, status: 'online', tunnelEnabled: false, nodeSecurity: { report: { locked: false } } }
assert.equal(evaluateHostService(online).available, true)
assert.equal(evaluateHostService({ ...online, status: 'maintenance' }).available, true, 'maintenance alone does not stop existing workloads')
assert.equal(evaluateHostService({ ...online, status: 'maintenance', tunnelEnabled: true }).available, false)
for (const host of [
  { ...online, status: 'offline' },
  { ...online, nodeSecurity: { report: { locked: true } } },
  { ...online, tunnelEnabled: true },
  { ...online, agent: { status: 'online', lastSeenAt: new Date(now), lastReport: {} } },
  { ...online, agent: { status: 'online', lastSeenAt: new Date(now), lastReport: { incus: { available: true }, instances: { available: false } } } },
  { ...online, agent: { status: 'online', lastSeenAt: new Date(now - 121_000), lastReport: {} } },
  { ...online, agent: { status: 'online', lastSeenAt: new Date(now), lastReport: { incus: { available: false } } } }
]) {
  const service = evaluateHostService(host, now)
  assert.equal(service.available, false)
  for (const status of ['running', 'stopped', 'error']) assert.equal(instanceServiceStatus(status, service), 'error')
  for (const status of ['creating', 'suspended', 'deleted']) assert.equal(instanceServiceStatus(status, service), status)
}
assert.equal(evaluateHostService({ ...online, status: 'offline', nodeSecurity: { report: { locked: true } } }).status, 'locked')
assert.equal(instanceServiceStatus('running', undefined), 'error')
assert.equal(instanceServiceStatus('running', evaluateHostService(online)), 'running')
const recovering = evaluateHostService({ ...online, nodeSecurity: { report: { locked: false, resumeInstances: [{ project: 'default', name: 'pending' }] } } })
assert.equal(instanceServiceStatus('stopped', recovering, 'pending'), 'error')
assert.equal(instanceServiceStatus('running', recovering, 'finished'), 'running')
const startupPending = evaluateHostService({ ...online, nodeSecurity: { report: { locked: false, recoveryPending: true, resumeInstances: [] } } })
assert.equal(startupPending.status, 'online', 'a normal restart fence is separate from an alarm lock')
assert.equal(startupPending.available, false, 'recoveryPending nodes must be excluded from delivery')
assert.equal(instanceServiceStatus('stopped', startupPending), 'error')
const hosts = new Map([[77, { status: 'offline' as const, available: false }]])
assert.deepEqual(instanceServiceStatusFilter('running', hosts), { status: 'running', NOT: { OR: [{ hostId: 77 }] } })
assert.deepEqual(instanceServiceStatusFilter('error', hosts), { OR: [{ status: 'error' }, { status: { in: ['running', 'stopped'] }, OR: [{ hostId: 77 }] }] })
assert.deepEqual(instanceServiceStatusFilter('stopped', new Map()), { status: 'stopped' })
let queries = 0
Object.assign(prisma.host, { findMany: async ({ where, select }: any) => {
 queries++; assert.deepEqual(where.id.in, [77]); assert.ok(select.nodeSecurity && select.agent); return [online]
} })
assert.equal((await loadHostServiceStatuses([77, 77])).get(77)?.available, true)
assert.equal((await loadHostServiceStatuses([])).size, 0)
assert.equal(queries, 1)
await prisma.$disconnect()
console.log('Host availability, lock priority, recovery and business-state separation passed')
