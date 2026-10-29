import assert from 'node:assert/strict'
import { setTimeout as delay } from 'node:timers/promises'
import Fastify from 'fastify'

process.env.DATABASE_URL = 'postgresql://fixture:fixture@127.0.0.1:1/test'
const { prisma, closePrismaDatabase } = await import('../src/db/prisma.js')
const { IncusClient } = await import('../src/lib/incus/incus-client.js')
const { default: routes } = await import('../src/routes/node-security.js')
const { default: tasksRoutes } = await import('../src/routes/admin-tasks.js')
const { recoverInterruptedOperationTasks } = await import('../src/lib/operation-task-log.js')
const { hostTunnelManager } = await import('../src/lib/incus/tunnel-manager.js')
const entry = { name: 'original-running', project: 'default', uuid: 'identity', frozen: false }
let state: any = { enabled: true, locked: true, revision: 3, resumeInstances: [entry] }
let report: any = state
let nextId = 0
let failCreate = false
let failApproval = false
let holdRecovery = false
let beginHeldRecovery!: () => void
let releaseHeldRecovery!: () => void
let recoveryGeneration = 0
let recoveryBlocked = true
let starts = 0
let approvals = 0
let running = false
let releaseStart!: () => void
let started!: () => void
const starting = new Promise<void>(resolve => { started = resolve })
const heldStart = new Promise<void>(resolve => { releaseStart = resolve })
const heldRecovery = new Promise<void>(resolve => { releaseHeldRecovery = resolve })
const recoveryStarted = new Promise<void>(resolve => { beginHeldRecovery = resolve })
const tasks: any[] = [], logs: any[] = []
const matches = (task: any, where: any) => Object.entries(where ?? {}).every(([key, value]) => task[key] === value)
Object.assign(prisma.operationTask, {
  findFirst: async ({ where }: any) => tasks.filter(task => matches(task, where)).at(-1) ?? null,
  findMany: async ({ where }: any = {}) => tasks.filter(task => matches(task, where)),
  findUnique: async ({ where }: any) => tasks.find(task => task.id === where.id) ?? null,
  count: async ({ where }: any) => tasks.filter(task => matches(task, where)).length,
  create: async ({ data }: any) => {
    if (failCreate) throw new Error('database unavailable')
    const task = { id: ++nextId, status: 'running', error: null, createdAt: new Date(), finishedAt: null, ...data }
    tasks.push(task); return task
  },
  updateMany: async ({ where, data }: any) => { const found = tasks.filter(task => matches(task, where)); found.forEach(task => Object.assign(task, data)); return { count: found.length } },
  update: async ({ where, data }: any) => Object.assign(tasks.find(task => task.id === where.id), data)
})
Object.assign(prisma.operationTaskLog, { create: async ({ data }: any) => { const row = { id: logs.length + 1, ...data }; logs.push(row); return row } })
Object.assign(prisma.host, { findUnique: async () => ({ id: 9, name: 'node', url: 'https://fixture.invalid' }), findMany: async () => [{ id: 9, name: 'node' }] })
Object.assign(prisma.nodeSecurityState, {
  findUnique: async () => ({ report, serverFingerprint: 'a'.repeat(64), recoveryGeneration, recoveryBlocked }),
  upsert: async ({ update }: any) => { report = update.report; return {} },
  update: async ({ data }: any) => { report = data.report ?? report; return {} },
  updateMany: async ({ where, data }: any) => {
    if (where.recoveryGeneration !== recoveryGeneration) return { count: 0 }
    if (typeof data.recoveryBlocked === 'boolean') recoveryBlocked = data.recoveryBlocked
    return { count: 1 }
  }
})
Object.assign(prisma.nodeCliEvent, { deleteMany: async () => ({ count: 0 }), findMany: async () => [] })
Object.assign(prisma.nodeStorageKey, { findMany: async () => [] })
Object.assign(prisma.instance, { findFirst: async () => ({ status: 'running', expiresAt: null }) })
Object.assign(prisma.log, { create: async () => ({}) })
let transaction = Promise.resolve()
;(prisma as any).$transaction = (action: any) => {
  if (Array.isArray(action)) return Promise.all(action)
  const result = transaction.then(() => action({ ...prisma, $executeRaw: async () => 0 }))
  transaction = result.catch(() => {})
  return result
}
Object.assign(IncusClient.prototype, {
  connect: async () => {}, close: async () => {},
  request: async (method: string, path: string, body: any) => {
    if (path === '/1.0/payincus') return { api_extensions: ['payincus_security_v1', 'payincus_security_resume_v1'] }
    if (path.endsWith('/recover')) {
      approvals++
      if (failApproval) throw new Error('approval refused')
      if (holdRecovery) { beginHeldRecovery(); await heldRecovery; holdRecovery = false }
      assert.equal(body.revision, state.revision)
      state = { ...state, locked: false, revision: state.revision + 1 }
      return state
    }
    if (path.endsWith('/security')) return state
    if (path.endsWith('/resume')) {
      assert.equal(body.revision, state.revision)
      state = { ...state, revision: state.revision + 1, resumeInstances: [] }
      return state
    }
    if (method === 'PUT') { starts++; started(); await heldStart; running = true; return {} }
    if (path.includes('/state?')) return { status: running ? 'Running' : 'Stopped' }
    return { type: 'container', config: { 'volatile.uuid': 'identity' } }
  }
})
hostTunnelManager.sendTaskLogControl = () => { assert.fail('node recovery must not subscribe to instance diagnostic streams') }
const app = Fastify({ logger: false })
app.decorate('authenticate', async (req: any, reply: any) => {
  if (!req.headers.authorization) return reply.code(401).send({ error: 'unauthorized' })
  req.user = { id: 1, role: req.headers.authorization }
})
app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user?.role !== 'admin') return reply.code(403).send({ error: 'forbidden' }) })
await app.register(routes)
await app.register(tasksRoutes)
const base = '/api/admin/hosts/9/node-security'
const approve = () => app.inject({ method: 'POST', url: base + '/recover', headers: { authorization: 'admin' }, payload: { revision: state.revision } })
for (const authorization of [undefined, 'user']) {
 const denied = await app.inject({ method: 'POST', url: base + '/recover', headers: authorization ? { authorization } : {}, payload: { revision: 3 } })
 assert.equal(denied.statusCode, authorization ? 403 : 401)
}
assert.equal(tasks.length, 0)
assert.equal((await app.inject({ method: 'POST', url: base + '/recover', headers: { authorization: 'admin' }, payload: {} })).statusCode, 400)
const responses = await Promise.race([Promise.all([approve(), approve()]), delay(1500).then(() => { throw new Error('approval blocked on container startup') })])
const accepted = responses[0]!
assert.equal(responses[1]!.statusCode, 202)
assert.equal(responses[1]!.json().taskId, accepted.json().taskId, 'simultaneous approval must deduplicate')
assert.equal(accepted.statusCode, 202)
assert.deepEqual(accepted.json(), { taskId: 1, status: 'running' })
assert.equal(accepted.headers['cache-control'], 'no-store')
await Promise.race([starting, delay(1500).then(() => { throw new Error('task did not start') })])
assert.equal(tasks[0].status, 'running', 'HTTP acceptance must precede completion')
const duplicate = await approve()
assert.equal(duplicate.statusCode, 202)
assert.equal(duplicate.json().taskId, 1)
assert.equal(approvals, 1)
assert.equal(starts, 1)
const overview = await app.inject({ url: base, headers: { authorization: 'admin' } })
assert.equal(overview.json().recoveryTask.status, 'running')
assert.equal(overview.json().events.length, 0)
const filtered = await app.inject({ url: '/api/admin/tasks?kind=node_recover', headers: { authorization: 'admin' } })
assert.equal(filtered.statusCode, 200)
assert.equal(filtered.json().items[0].id, 1)
releaseStart()
async function finished(id: number) {
 for (let i = 0; i < 100; i++) { if (tasks.find(task => task.id === id)?.status !== 'running') return; await delay(10) }
 assert.fail('background task did not finish')
}
await finished(1)
assert.equal(tasks[0].status, 'success')
assert.equal(state.resumeInstances.length, 0)
assert.ok(logs.some(row => row.taskId === 1 && row.message.includes('恢复进度 1/1')))
failApproval = true
const failed = await approve()
assert.equal(failed.statusCode, 202, 'node failures are represented by the asynchronous task')
await finished(2)
assert.equal(tasks[1].status, 'failed')
failApproval = false
assert.equal((await approve()).statusCode, 202, 'failed tasks must not hold the per-node gate')
await finished(3)
assert.equal(tasks[2].status, 'success')
failCreate = true
const before = approvals
assert.equal((await approve()).statusCode, 409)
await delay(10)
assert.equal(approvals, before, 'no durable task means no node side effect')
failCreate = false
const orphan = await prisma.operationTask.create({ data: { hostId: 9, instanceName: 'node', kind: 'node_recover' } })
await recoverInterruptedOperationTasks()
assert.equal(tasks.find(task => task.id === orphan.id).status, 'interrupted')
assert.equal(approvals, before, 'restart must not replay control-plane approval')
recoveryBlocked = true
holdRecovery = true
assert.equal((await approve()).statusCode, 202)
await Promise.race([recoveryStarted, delay(1500).then(() => { throw new Error('recovery generation race did not start') })])
recoveryGeneration++ // A new bad proof arrived after this administrator's approval was captured.
releaseHeldRecovery()
await finished(5)
assert.equal(tasks[4].status, 'failed')
assert.equal(recoveryBlocked, true, 'a stale approval must not clear a newer sticky lock generation')
await app.close()
await closePrismaDatabase()
console.log('Async 202 acceptance, deduplication, progress, failure/retry, DB failure, authorization and restart interruption passed')
