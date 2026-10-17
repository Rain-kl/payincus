import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { InstanceStatusObservations } from '../src/lib/instance-status-observation.js'

const observations = new InstanceStatusObservations()
assert.equal(observations.shouldApply(1, 'running', 'error', 0), false)
assert.equal(observations.shouldApply(1, 'running', 'error', 29_999), false)
assert.equal(observations.shouldApply(1, 'running', 'error', 30_000), true)
assert.equal(observations.shouldApply(1, 'error', 'running', 30_001), true, 'error must be recoverable')
assert.equal(observations.shouldApply(1, 'running', 'error', 30_002), false, 'successful recovery resets error confirmation')
observations.forget(1) // A failed/unknown query or an active workflow breaks the observation window.
assert.equal(observations.shouldApply(1, 'running', 'error', 100_000), false)
assert.equal(observations.shouldApply(1, 'running', 'unknown', 100_001), false)
assert.equal(observations.shouldApply(1, 'running', 'error', 200_000), false)
assert.equal(observations.shouldApply(1, 'running', 'error', 1_200_001), false, 'old observations expire')
for (const current of ['creating', 'deleted', 'suspended']) {
  for (const observed of ['running', 'stopped', 'error']) {
    assert.equal(observations.shouldApply(2, current, observed), false, `${current} must remain protected`)
  }
}
assert.equal(observations.shouldApply(3, 'running', 'creating'), false, 'Starting must not strand a provisioned VM in creating')
assert.equal(observations.shouldApply(3, 'error', 'stopped'), true)

const scheduler = readFileSync(new URL('../src/services/status-scheduler.ts', import.meta.url), 'utf8')
const routes = readFileSync(new URL('../src/routes/instances.ts', import.meta.url), 'utf8')
assert.ok(scheduler.includes("in: ['running', 'stopped', 'error']"), 'scheduler must recheck error instances')
assert.ok(scheduler.includes("if (instance.status === 'error') return { changed: false, error: errorMessage }"),
  'failed provisioning records must not lose error/refund semantics when Incus returns not found')
for (const source of [scheduler, routes]) {
  assert.ok(source.includes('instanceStatusObservations.shouldApply('))
  assert.ok(source.includes('instanceStatusObservations.forget('))
  assert.ok(source.includes('await hasActiveInstanceOperation('), 'active workflows must be excluded')
  assert.ok(source.includes('status: instance.status'), 'writes must compare current status to avoid overwriting a concurrent suspension/deletion')
}
console.log('instance status observation tests passed')
