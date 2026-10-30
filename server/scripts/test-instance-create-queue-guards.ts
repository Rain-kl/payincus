import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

// Guards for the queued-instance-create contract:
// 1. A create task that hits a transient node failure returns to PENDING and
//    stops after a bounded number of attempts.
// 2. Business failures never requeue, so a bad request cannot spin forever.
// 3. The heartbeat exposes the pending queue so a returning node resumes work.

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const read = (path: string) => readFileSync(resolve(__dirname, '..', path), 'utf8')

const worker = read('src/workers/instanceTaskWorker.ts')
const agentRoute = read('src/routes/agent.ts')
const instanceRoute = read('src/routes/instances.ts')
const schema = read('prisma/schema.prisma')
const migration = read('prisma/migrations/20261030120000_instance_create_queue/migration.sql')

assert.ok(schema.includes('create    // 创建'), 'InstanceTaskType must declare the create variant')
assert.ok(schema.includes('payload              Json?'), 'InstanceTask must persist a create payload snapshot')
assert.ok(migration.includes(`ADD VALUE IF NOT EXISTS 'create'`), 'migration must add the create enum value')
assert.ok(migration.includes('ADD COLUMN IF NOT EXISTS "payload" JSONB'), 'migration must add the payload column')

assert.ok(worker.includes('const CREATE_TASK_MAX_RETRIES = 5'), 'worker must bound create retries at five attempts')
assert.ok(
  worker.includes(`case 'create': {`) && worker.includes('createTaskExecutor(task.instanceId, task.hostId, payload)'),
  'worker must replay a create task through the injected executor'
)
assert.ok(
  worker.includes('if (task?.taskType === \'create\' && isRetryableCreateError(errorMessage) && task.retryCount < CREATE_TASK_MAX_RETRIES)'),
  'worker must requeue only retryable create failures below the retry cap'
)
assert.ok(
  worker.includes('status: \'PENDING\'') && worker.includes('retryCount: { increment: 1 }'),
  'requeued create tasks must return to PENDING with an incremented retry counter'
)
assert.ok(
  worker.includes('export function nudgeInstanceTaskWorker()'),
  'worker must expose a nudge so a returning node resumes queued work immediately'
)

// Business errors must not be in the retryable set, or a permanent failure
// would be retried five times and mask the real cause from the customer.
for (const businessError of ['BALANCE_INSUFFICIENT', 'PROMO_', 'HOST_RESOURCES_INSUFFICIENT', 'SHARE_QUOTA_']) {
  assert.ok(
    !worker.includes(`'${businessError}`),
    `retryable create patterns must not include the business error ${businessError}`
  )
}

assert.ok(
  instanceRoute.includes('enqueueCreateTaskIfNodeUnavailable('),
  'instance creation must queue when the node is unavailable'
)
assert.ok(
  instanceRoute.includes('taskType: \'create\' as InstanceTaskType') &&
    instanceRoute.includes('payload: { config, userId, resources }'),
  'queued create must persist the replay snapshot'
)
assert.ok(
  instanceRoute.includes('setCreateTaskExecutor('),
  'route module must register the create executor consumed by the worker'
)

assert.ok(
  agentRoute.includes('pendingCreateTasks,') && agentRoute.includes(`taskType: 'create', status: 'PENDING'`),
  'heartbeat response must report the pending create queue'
)
assert.ok(
  agentRoute.includes('nudgeInstanceTaskWorker()'),
  'heartbeat must nudge the worker when queued creates exist'
)

console.log('Queued instance create guards passed')
