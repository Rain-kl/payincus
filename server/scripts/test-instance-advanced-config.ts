import assert from 'node:assert/strict'
import Fastify from 'fastify'
import { applyAdvancedConfig } from '../src/lib/incus/instance-advanced-config.js'
import { registerAdvancedConfigRoute } from '../src/routes/instance-advanced-config.js'

const calls: any[][] = []
const fixture = {
  type: 'container', devices: { uplink: { type: 'nic', network: 'br-test', name: 'eth0', 'hwaddr': '00:16:3e:12:34:56' } },
  expanded_devices: {
    volume: { type: 'disk', path: '/', pool: 'private', size: '12GiB' },
    uplink: { type: 'nic', network: 'br-test', name: 'eth0', 'hwaddr': '00:16:3e:12:34:56' }
  }
}
const client = { async request<T>(...args: any[]): Promise<T> { calls.push(args); return structuredClone(fixture) as T } }
const signal = new AbortController().signal
await applyAdvancedConfig(client, 'test/name', {
  limitsRead: '100MB', limitsWrite: '150MB', limitsIngress: '300Mbps', limitsEgress: '500Mbit',
  limitsProcesses: 4000, limitsCpuPriority: 0, bootAutostart: false,
  bootAutostartPriority: 0, bootAutostartDelay: 0, bootHostShutdownTimeout: 30
}, {}, signal)
assert.equal(calls[0][1], '/1.0/instances/test%2Fname')
const patch = calls[1][2]
assert.equal(calls[1][0], 'PATCH')
assert.equal(calls[1][4], signal)
assert.deepEqual(patch.devices.volume, { ...fixture.expanded_devices.volume, 'limits.read': '100MB', 'limits.write': '150MB' })
assert.equal(patch.devices.uplink['hwaddr'], '00:16:3e:12:34:56')
assert.equal(patch.devices.uplink['limits.ingress'], '300Mbit')
assert.equal(patch.devices.uplink['limits.egress'], '500Mbit')
assert.equal(patch.config['limits.processes'], '4000')
assert.equal(patch.config['limits.cpu.priority'], '0')
assert.equal(patch.config['boot.autostart'], 'false')
assert.equal(patch.config['boot.autostart.delay'], '0')
calls.length = 0
await applyAdvancedConfig(client, 'demo', { limitsRead: null, limitsCpuPriority: null, bootAutostart: null }, { limits_read: '45MB', limits_cpu_priority: 0, boot_autostart: false }, signal)
assert.equal(calls[1][2].devices.volume['limits.read'], '45MB')
assert.equal(calls[1][2].config['boot.autostart'], 'false')
await assert.rejects(applyAdvancedConfig(client, 'demo', { limitsRead: '100MB', limitsReadIops: 20 }, {}, signal), /Choose throughput/)
calls.length = 0
await applyAdvancedConfig(client, 'demo', { limitsReadIops: 0 }, {}, signal)
assert.equal(calls[1][2].devices.volume['limits.read'], '0iops')
const aborted = new AbortController(); aborted.abort()
await assert.rejects(applyAdvancedConfig(client, 'demo', {}, {}, aborted.signal), { name: 'AbortError' })
for (const response of [null, {}, { type: 'container', devices: {} }, { type: 'container', devices: [], expanded_devices: {} }, { type: 'container', devices: {}, expanded_devices: { root: null } }]) {
  await assert.rejects(applyAdvancedConfig({ async request<T>() { return response as T } }, 'demo', {}, {}, signal), /Invalid Incus/)
}
for (const failure of ['denied', 'timeout', 'operation failure', 'cancelled']) {
  await assert.rejects(applyAdvancedConfig({ async request<T>(method: string) { if (method === 'GET') return fixture as T; throw new Error(failure) } }, 'demo', { limitsProcesses: 1000 }, {}, signal), new RegExp(failure))
}
let persisted = 0
let applied = 0
let failApply = false
let busy = false
let status = 'running'
let nodeChanges: any
let savedChanges: any
let savedEffective: any
let trafficStatus = 'NORMAL'
const app = Fastify({ logger: false })
app.decorate('authenticate', async (request: any, reply: any) => {
  if (!request.headers.authorization) return reply.code(401).send({ error: 'unauthorized' })
  request.user = { id: request.headers.authorization === 'owner' ? 8 : 1, role: request.headers.authorization }
})
registerAdvancedConfigRoute(app, async reply => { if (busy) { reply.code(409).send({ error: 'Task active' }); return true }; return false }, async () => false, {
  getInstanceById: async () => ({ id: 9, host_id: 3, status, incus_id: 'demo', name: 'Demo', package_id: null }),
  getHostById: async () => ({ id: 3, user_id: 8 }),
  getIncusClient: async () => client,
  applyAdvancedConfig: async (_client: unknown, _name: string, changes: unknown) => { nodeChanges = changes; applied++; if (failApply) throw new Error('native failed') },
  updateInstanceConfig: async (_id: number, changes: unknown, effective: unknown) => { persisted++; savedChanges = changes; savedEffective = effective },
  getBandwidthState: async () => ({ trafficStatus, configuredLimitsIngress: null, configuredLimitsEgress: null, packagePlan: { trafficLimitSpeed: '20Mbit' }, package: { limitsIngress: '300Mbit', limitsEgress: '300Mbit' } }),
  getSystemConfig: async () => '2Mbit',
  withLock: async (_key: string, fn: () => Promise<void>) => { await fn(); return { success: true } },
  createLog: async () => {},
} as unknown as NonNullable<Parameters<typeof registerAdvancedConfigRoute>[3]>)
const update = (role?: string, payload: any = { limitsProcesses: 4000 }, id = '9') => app.inject({ method: 'PATCH', url: `/${id}/advanced-config`, headers: role ? { authorization: role } : {}, payload })
assert.equal((await update()).statusCode, 401)
assert.equal((await update('user')).statusCode, 403)
assert.equal(persisted, 0)
assert.equal(applied, 0)
for (const payload of [{ limitsCpuPriority: 11 }, { bootAutostartPriority: 101 }, { limitsProcesses: -1 }, { limitsRead: 'invalid' }, { limitsIngress: 'x' }, { limitsIngress: '0Mbit' }, { limitsRead: '1MB', limitsReadIops: 50 }, { unrecognized: true }, {}]) {
  assert.equal((await update('admin', payload)).statusCode, 400)
}
assert.equal((await update('admin', undefined, '0')).statusCode, 400)
busy = true
assert.equal((await update('admin')).statusCode, 409)
busy = false; status = 'suspended'
assert.equal((await update('admin')).statusCode, 409)
status = 'running'
assert.equal((await update('admin')).statusCode, 200)
assert.equal((await update('owner')).statusCode, 200)
assert.equal(persisted, 2)
assert.equal((await update('admin', { limitsIngress: '500Mbps', limitsEgress: null })).statusCode, 200)
assert.equal(nodeChanges.limitsIngress, '500Mbit')
assert.equal(nodeChanges.limitsEgress, '20Mbit')
assert.equal(savedChanges.limitsIngress, '500Mbit')
assert.equal(savedChanges.limitsEgress, null)
trafficStatus = 'LIMITED'
assert.equal((await update('admin', { limitsIngress: '500Mbit' })).statusCode, 200)
assert.equal(nodeChanges.limitsIngress, '2Mbit')
assert.equal(savedChanges.limitsIngress, '500Mbit')
assert.equal(savedEffective.ingress, '2Mbit')
failApply = true
assert.equal((await update('admin')).statusCode, 502)
assert.equal(persisted, 4)
await app.close()
console.log('Advanced config native device, default reset, zero/false, validation, access, conflict and failure contracts passed')

// Exercise the real HTTP envelope/operation adapter, including asynchronous failures and aborts.
const { MockAgent } = await import('undici')
const { IncusClient } = await import('../src/lib/incus/incus-client.js')
const dispatcher = new MockAgent()
dispatcher.disableNetConnect()
const pool = dispatcher.get('https://node.example')
const native = new IncusClient({ url: 'https://node.example' })
native.agent = dispatcher as unknown as NonNullable<typeof native.agent>
const respond = (path: string, method: string, metadata: unknown) => pool.intercept({ path, method }).reply(200, { type: 'sync', metadata })
const prepare = (id: string, result: string) => {
  respond('/1.0/instances/demo', 'GET', fixture)
  pool.intercept({ path: '/1.0/instances/demo', method: 'PATCH' }).reply(202, { type: 'async', operation: `/1.0/operations/${id}` })
  respond(`/1.0/operations/${id}/wait?timeout=10`, 'GET', { status: result, status_code: result === 'Success' ? 200 : 400, err: result === 'Success' ? '' : 'operation rejected' })
}
prepare('success', 'Success')
await applyAdvancedConfig(native, 'demo', { limitsProcesses: 4000 }, {}, signal)
prepare('failure', 'Failure')
await assert.rejects(applyAdvancedConfig(native, 'demo', { limitsProcesses: 4000 }, {}, signal), /operation rejected/)
respond('/1.0/instances/demo', 'GET', fixture)
pool.intercept({ path: '/1.0/instances/demo', method: 'PATCH' }).reply(202, { type: 'async', operation: '/1.0/operations/slow' })
pool.intercept({ path: '/1.0/operations/slow/wait?timeout=10', method: 'GET' }).reply(200, { type: 'sync', metadata: { status: 'Success', status_code: 200 } }).delay(500)
await assert.rejects(applyAdvancedConfig(native, 'demo', { limitsProcesses: 4000 }, {}, AbortSignal.timeout(25)), /abort|timeout/i)
await dispatcher.close()
console.log('Native HTTP async operation success, failure and cancellation passed')
