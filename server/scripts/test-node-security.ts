import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { fingerprint, seal, unseal } from '../src/lib/node-security/vault.js'
import { parseReport } from '../src/lib/node-security/report.js'

const key = randomBytes(32).toString('hex')
const master = randomBytes(48).toString('base64')
const identity = { hostId: 7, dataset: 'tank/private', version: 1 }
const encrypted = seal(key, identity, master)
assert.notEqual(encrypted, key)
assert.equal(unseal(encrypted, identity, master), key)
assert.notEqual(seal(key, identity, master), encrypted)
assert.equal(fingerprint(key).length, 64)
for (const altered of [{ ...identity, hostId: 8 }, { ...identity, dataset: 'tank/other' }, { ...identity, version: 2 }]) assert.throws(() => unseal(encrypted, altered, master))
assert.throws(() => unseal(encrypted, identity, randomBytes(48).toString('base64')))
assert.throws(() => unseal(key, identity, master))
assert.throws(() => unseal(encrypted.slice(0, -2) + 'zz', identity, master))
assert.throws(() => seal(key, identity, 'weak'))
assert.throws(() => seal('123456', identity, master))
assert.throws(() => seal(key, { ...identity, dataset: 'tank/../secret' }, master))
const event = { id: 'a'.repeat(32), command: 'exec', uid: 0, pid: 10, time: new Date().toISOString(), critical: true, blocked: true }
const parsed = parseReport({ state: { locked: true, enabled: true, revision: 3, key }, events: [{ ...event, key, args: ['arbitrary-secret'] }] })
assert.deepEqual(parsed.events, [event])
assert.equal(JSON.stringify(parsed).includes(key), false)
assert.equal(JSON.stringify(parsed).includes('arbitrary-secret'), false)
for (const input of [null, {}, { state: { locked: true, enabled: true, revision: 1 }, events: [{ ...event, command: key }] }, { state: { locked: true, enabled: true, revision: 1 }, events: Array(129).fill(event) }]) assert.throws(() => parseReport(input))
const routes = readFileSync(new URL('../src/routes/node-security.ts', import.meta.url), 'utf8')
assert.match(routes, /app\.authenticate, app\.requireAdmin/)
assert.match(routes, /Cache-Control.*no-store/)
assert.match(routes, /additionalProperties: false/)
assert.match(routes, /secret|seal|unseal/)
assert.doesNotMatch(routes, /decryptSensitiveData/)
const agent = readFileSync(new URL('../src/routes/agent.ts', import.meta.url), 'utf8')
const reportRoute = agent.slice(agent.indexOf("('/security-report'"), agent.indexOf("('/heartbeat'"))
assert.match(reportRoute, /authenticateAgentRequest/)
assert.match(reportRoute, /recordReport/)
console.log('Node key envelope, audit validation and authorization guards passed')
