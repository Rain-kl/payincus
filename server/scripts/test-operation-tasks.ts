import assert from 'node:assert/strict'
import { setTimeout as delay } from 'node:timers/promises'
import Fastify from 'fastify'

// Isolated integration test. No production database or node is contacted.
process.env.DATABASE_URL = 'postgresql://test:test@127.0.0.1:1/test'
const { prisma, closePrismaDatabase } = await import('../src/db/prisma.js')
const { hostTunnelManager } = await import('../src/lib/incus/tunnel-manager.js')
const { runOperationTask, taskLog, redactTaskLog, recoverInterruptedOperationTasks, operationTaskControl, stopOperationTask } = await import('../src/lib/operation-task-log.js')
const { default: routes } = await import('../src/routes/admin-tasks.js')
const { TunnelFrameType } = await import('../src/lib/incus/tunnel-protocol.js')
const frameTypes = Object.values(TunnelFrameType).filter(value => typeof value === 'number')
assert.equal(new Set(frameTypes).size, frameTypes.length, 'Task logs and network control must use distinct frame types')
const tasks: any[] = []
const logs: any[] = []
let nextTask = 0
let nextLog = 0
const matches = (task: any, where: any) => Object.entries(where || {}).every(([key, value]) => task[key] === value)
Object.assign(prisma.operationTask, {
  create: async ({ data }: any) => { const task = { id: ++nextTask, status: 'running', error: null, createdAt: new Date(), finishedAt: null, ...data }; tasks.push(task); return task },
  findUnique: async ({ where }: any) => tasks.find(task => task.id === where.id) || null,
  findMany: async ({ where, skip = 0, take }: any = {}) => tasks.filter(task => matches(task, where)).slice().reverse().slice(skip, take ? skip + take : undefined),
  count: async ({ where }: any) => tasks.filter(task => matches(task, where)).length,
  updateMany: async ({ where, data }: any) => { const found = tasks.filter(task => matches(task, where)); found.forEach(task => Object.assign(task, data)); return { count: found.length } },
  update: async ({ where, data }: any) => Object.assign(tasks.find(task => task.id === where.id), data)
})
Object.assign(prisma.operationTaskLog, {
  create: async ({ data }: any) => { const log = { id: ++nextLog, createdAt: new Date(), ...data }; logs.push(log); return log },
  findMany: async ({ where, take }: any) => logs.filter(log => log.taskId === where.taskId && log.id > where.id.gt).slice(0, take)
})
Object.assign(prisma.host, { findMany: async () => [{ id: 1, name: 'node-test' }] })
;(prisma as any).$transaction = async (operations: any[]) => Promise.all(operations)
hostTunnelManager.sendTaskLogControl = () => false
assert.doesNotMatch(redactTaskLog('password="private-value" token=secret-value https://name:pass@example.test/path'), /private-value|secret-value|name:pass/)
const { waitForOperation } = await import('../src/lib/incus/incus-utils.js')
let polls = 0
await runOperationTask(1, 'progress-vm', 'create', async () => {
  const result = await waitForOperation({ request: async () => ++polls === 1
    ? { status: 'Running', status_code: 103, description: 'Creating instance', metadata: { download_progress: '50%', secret: 'must-not-log' } }
    : { status: 'Success', status_code: 200, metadata: { done: true } }
  } as any, '/1.0/operations/test', 1000)
  assert.deepEqual(result, { done: true })
})
assert(logs.some(log => /download_progress: 50%/.test(log.message)))
assert(!logs.some(log => /must-not-log/.test(log.message)))
const progressSecret = 'fixture-private-progress-material'
await runOperationTask(1, 'redacted-progress', 'create', async () => {
  await waitForOperation({ request: async () => ({ status: 'Success', status_code: 200, description: `progress ${progressSecret}`, metadata: { progress: 'x'.repeat(990) + progressSecret } }) } as any, '/1.0/operations/redaction', 1000, undefined, { key: progressSecret })
})
assert(!logs.some(log => log.message.includes(progressSecret) || log.message.includes('fixture-pr')), 'progress was truncated before key redaction')
for (const name of ['AbortError', 'TimeoutError']) {
  for (const alreadyAborted of [false, true]) {
    const controller = new AbortController()
    const reason = new DOMException(`fixture stop ${progressSecret}`, name)
    let calls = 0
    if (alreadyAborted) controller.abort(reason)
    await assert.rejects(waitForOperation({ request: async () => {
      calls++
      controller.abort(reason)
      throw new DOMException('request aborted', 'AbortError')
    } } as any, '/abort', 1000, controller.signal, { key: progressSecret }), (error: unknown) => {
      assert.ok(error instanceof Error)
      assert.equal(error.name, name)
      assert.equal(error.message, 'fixture stop [REDACTED]')
      return true
    })
    assert.equal(calls, alreadyAborted ? 0 : 1)
  }
}
await assert.rejects(waitForOperation({ request: async () => ({}) } as any, '/invalid', 1000), /节点异步操作响应格式无效/)
const { IncusApiError } = await import('../src/lib/incus/incus-error.js')
const concurrentAbort = new AbortController()
await assert.rejects(waitForOperation({ request: async () => {
  concurrentAbort.abort()
  throw new IncusApiError('node permission denied', 403)
} } as any, '/concurrent-abort', 1000, concurrentAbort.signal), (error: unknown) => error instanceof IncusApiError && error.statusCode === 403 && error.message === 'node permission denied')

// Reset only test fixtures before the parallel isolation checks.
tasks.length = 0; logs.length = 0; nextTask = 0; nextLog = 0
await Promise.all([
  runOperationTask(1, 'vm-a', 'create', async () => {
    taskLog('info', 'a-first'); await delay(10); taskLog('info', 'a-second')
    hostTunnelManager.emit('task_log', 999, 1, 'WRONG-HOST')
    hostTunnelManager.emit('task_log', 1, 1, 'node-a')
    await runOperationTask(1, 'vm-a', 'destroy', async () => taskLog('warn', 'rollback-a'))
  }),
  runOperationTask(2, 'vm-b', 'destroy', async () => { await delay(1); taskLog('info', 'b-only') })
])
assert.equal(tasks.length, 2, 'nested cleanup belongs to the parent task')
assert.equal(tasks[0].status, 'success')
assert(logs.some(log => log.taskId === 1 && log.source === 'node' && log.message === 'node-a'))
assert(!logs.some(log => log.message === 'WRONG-HOST'))
assert(!logs.some(log => log.taskId === 2 && /a-first|a-second|node-a/.test(log.message)), 'parallel tasks must not mix logs')
await assert.rejects(runOperationTask(1, 'failed-vm', 'create', async () => { throw new Error('KVM missing password=hidden') }), /KVM missing/)
assert.equal(tasks[2].status, 'failed')
assert.doesNotMatch(tasks[2].error, /hidden/)
await runOperationTask(1, 'timeout-vm', 'create', async () => { tasks[3].status = 'failed'; tasks[3].error = 'create timeout' })
assert.equal(tasks[3].status, 'failed', 'late success cannot overwrite timeout cleanup')
const interrupted = await prisma.operationTask.create({ data: { hostId: 1, instanceName: 'interrupted-vm', kind: 'create' } })
await recoverInterruptedOperationTasks()
assert.equal(tasks.find(task => task.id === interrupted.id).status, 'interrupted')
for (let i = 0; i < 205; i++) await prisma.operationTaskLog.create({ data: { taskId: 1, source: 'server', level: 'info', message: `history-${i}` } })
const app = Fastify()
app.decorate('authenticate', async (req: any, reply: any) => {
  if (!req.headers.authorization) return reply.code(401).send({ error: 'unauthorized' })
  req.user = { id: 1, role: req.headers.authorization === 'admin' ? 'admin' : 'user' }
})
app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user.role !== 'admin') return reply.code(403).send({ error: 'forbidden' }) })
await app.register(routes)
// Cancellation reaches the actual node operation and cannot cancel cleanup.
assert.equal((await app.inject({ method: 'POST', url: '/api/admin/tasks/1/stop' })).statusCode, 401)
assert.equal((await app.inject({ method: 'POST', url: '/api/admin/tasks/1/stop', headers: { authorization: 'user' } })).statusCode, 403)
assert.equal((await app.inject({ method: 'POST', url: '/api/admin/tasks/0/stop', headers: { authorization: 'admin' } })).statusCode, 400)
assert.equal((await app.inject({ method: 'POST', url: '/api/admin/tasks/999/stop', headers: { authorization: 'admin' } })).statusCode, 404)
assert.equal((await app.inject({ method: 'POST', url: '/api/admin/tasks/1/stop', headers: { authorization: 'admin' } })).statusCode, 409)
let allowCancel = false
let cancelCalls = 0
let releasePoll!: (value: any) => void
let ready!: () => void
const attached = new Promise<void>(resolve => { ready = resolve })
let cancellationTaskId = 0
const cancellation = runOperationTask(1, 'cancel-vm', 'create', async () => {
  cancellationTaskId = tasks.at(-1).id
  try {
    await waitForOperation({ request: async (method: string, path: string) => {
      if (method === 'DELETE') { cancelCalls++; releasePoll({ status: 'Cancelled', status_code: 401 }); return {} }
      if (path.includes('/wait?')) return new Promise(resolve => { releasePoll = resolve; ready() })
      return { status: 'Running', status_code: 103, may_cancel: allowCancel }
    } } as any, '/1.0/operations/cancel-test')
    assert.fail('cancelled node operation must reject')
  } catch (error) {
    assert.equal(operationTaskControl(cancellationTaskId).canStop, false, 'cleanup cannot be cancelled')
    throw error
  }
})
const rejectedCancellation = assert.rejects(cancellation, /节点操作已取消/)
await attached
assert.equal(operationTaskControl(cancellationTaskId).canStop, true)
const stopUrl = `/api/admin/tasks/${cancellationTaskId}/stop`
assert.equal((await app.inject({ method: 'POST', url: stopUrl, headers: { authorization: 'admin' } })).statusCode, 409, 'unsupported node cancellation is reported without marking the task finished')
assert.equal(tasks.at(-1).status, 'running')
assert.equal(cancelCalls, 0)
allowCancel = true
assert.equal((await app.inject({ method: 'POST', url: stopUrl, headers: { authorization: 'admin' } })).statusCode, 200)
await rejectedCancellation
assert.equal(cancelCalls, 1)
assert.equal(tasks.at(-1).status, 'cancelled')
await assert.rejects(stopOperationTask(cancellationTaskId), /无法停止/)
assert.equal((await app.inject('/api/admin/tasks')).statusCode, 401)
assert.equal((await app.inject({ url: '/api/admin/tasks', headers: { authorization: 'user' } })).statusCode, 403)
for (const url of ['/api/admin/tasks/0/logs/stream', '/api/admin/tasks/1/logs/stream?after=-1', '/api/admin/tasks?page=-2', '/api/admin/tasks?status=invalid']) {
  assert.equal((await app.inject({ url, headers: { authorization: 'admin' } })).statusCode, 400, url)
}
assert.equal((await app.inject({ url: '/api/admin/tasks/999/logs/stream', headers: { authorization: 'admin' } })).statusCode, 404)
const response = await app.inject({ url: '/api/admin/tasks/1/logs/stream', headers: { authorization: 'admin' } })
assert.equal(response.statusCode, 200)
assert.match(response.headers['content-type'] as string, /text\/event-stream/)
assert.match(response.body, /history-204/)
assert.match(response.body, /event: end/)
assert.doesNotMatch(response.body, /b-only/)
const last = logs.filter(log => log.taskId === 1).at(-1)!.id
const resumed = await app.inject({ url: `/api/admin/tasks/1/logs/stream?after=${last - 1}`, headers: { authorization: 'admin' } })
assert.equal((resumed.body.match(/event: log/g) || []).length, 1)
const { createServer } = await import('node:http')
const { Agent } = await import('undici')
const { IncusClient } = await import('../src/lib/incus/incus-client.js')
// Exercise the default request deadline through a real HTTP Incus envelope.
// Advance the clock beyond the old 2/5-minute limits without a slow test.
const realNow = Date.now
let clock = realNow()
let waits = 0
const operationNode = createServer((req, res) => {
  res.setHeader('Content-Type', 'application/json')
  if (req.url === '/create') res.end(JSON.stringify({ type: 'async', operation: '/op' }))
  else {
    clock += 6 * 60 * 1000
    res.end(JSON.stringify({ type: 'sync', metadata: ++waits === 1 ? { status: 'Running', status_code: 103 } : { status: 'Success', status_code: 200, metadata: { done: true } } }))
  }
})
await new Promise<void>(resolve => operationNode.listen(0, '127.0.0.1', resolve))
const operationClient = new IncusClient({ url: `http://127.0.0.1:${(operationNode.address() as { port: number }).port}`, certPath: null, keyPath: null })
operationClient.agent = new Agent()
try {
  Date.now = () => clock
  assert.deepEqual(await operationClient.request('POST', '/create'), { done: true }, 'default operation deadline must allow downloads longer than five minutes')
  await assert.rejects(waitForOperation({ request: async () => { clock += 31 * 60 * 1000; return { status: 'Running', status_code: 103 } } } as any, '/timeout'), /1800 seconds/, 'deadline must expire after thirty minutes with an accurate message')
} finally {
  Date.now = realNow
  await operationClient.close()
  await new Promise<void>(resolve => operationNode.close(() => resolve()))
}
let range = ''
const node = createServer((req, res) => { range = req.headers.range || ''; res.end('x'.repeat(100000)) })
await new Promise<void>(resolve => node.listen(0, '127.0.0.1', resolve))
const address = node.address() as { port: number }
const client = new IncusClient({ url: `http://127.0.0.1:${address.port}`, certPath: null, keyPath: null })
client.agent = new Agent()
const bounded = await client.readFile('/qemu.log', 65536)
assert.equal(bounded.content.length, 65536)
assert.equal(range, 'bytes=-65536')
const unbounded = await client.readFile('/normal-file')
assert.equal(unbounded.content.length, 100000, 'normal file reads retain their previous behavior')
await client.close()
await new Promise<void>(resolve => node.close(() => resolve()))
await app.close()
await closePrismaDatabase()
console.log('operation task lifecycle, isolation, redaction, auth, validation, SSE replay and resume checks passed')
