import assert from 'node:assert/strict'
import { restoreNodeInstances, type ResumeInstance, type RecoveryState } from '../src/lib/node-security/recovery.js'
const first: ResumeInstance = { project: 'project with spaces', name: 'running', uuid: 'identity', frozen: false }
const frozen = { ...first, name: 'frozen', frozen: true }
const suspended = { ...first, name: 'suspended' }
let pending = [first, frozen, suspended]
let revision = 10
const states = new Map([['running', 'Stopped'], ['frozen', 'Stopped']])
let operations = 0
let fail = true
const request = async <T>(method: string, path: string, body?: any): Promise<T> => {
 if (path.endsWith('/resume')) {
  assert.equal(body.revision, revision++)
  pending = pending.filter(item => item.name !== body.instance.name)
  return { locked: false, revision, resumeInstances: pending } as T
 }
 assert.ok(path.endsWith('?project=project%20with%20spaces'))
 const name = path.split('/')[3]!
 if (method === 'GET' && !path.includes('/state')) return { type: 'container', config: { 'volatile.uuid': 'identity' } } as T
 if (method === 'GET') return { status: states.get(name) } as T
 assert.equal(method, 'PUT')
 operations++
 if (name === 'running' && fail) throw new Error('native operation failed')
 states.set(name, body.action === 'start' ? 'Running' : 'Frozen')
 return {} as T
}
const state = (): RecoveryState => ({ locked: false, revision, resumeInstances: [...pending] })
const result = await restoreNodeInstances({ request }, state(), async item => item.name !== 'suspended')
assert.equal(result.failed, 1)
assert.deepEqual(pending, [first], 'failed startup remains durable; suspension is not restarted')
assert.equal(states.get('frozen'), 'Frozen')
fail = false
const retried = await restoreNodeInstances({ request }, state(), async () => true)
assert.equal(retried.failed, 0)
assert.equal(pending.length, 0)
assert.equal(states.get('running'), 'Running')
assert.equal(operations, 4)
// Lost ACK must not cause a second successful start.
pending = [first]
const before = operations
assert.equal((await restoreNodeInstances({ request }, state(), async () => true)).failed, 0)
assert.equal(operations, before)
const changed = { ...first, uuid: 'new-instance' }
assert.equal((await restoreNodeInstances({ request }, { locked: false, revision, resumeInstances: [changed] }, async () => true)).failed, 1)
assert.equal(operations, before)
await assert.rejects(restoreNodeInstances({ request }, { locked: true, revision, resumeInstances: [first] }, async () => true))
assert.equal((await restoreNodeInstances({ request }, { locked: false, revision, resumeInstances: [] }, async () => true)).failed, 0)
console.log('Native recovery identity, async failure, frozen state, partial retry and lost ACK passed')

await assert.rejects(restoreNodeInstances({ request }, { locked: false, revision, resumeInstances: [first] }, async () => true, { cancelled: () => true }), /已取消/)
