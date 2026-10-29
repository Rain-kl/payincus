import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { Prisma } from '@prisma/client'
import { prisma } from '../src/db/prisma.js'
import { grantStartupKeys, openStartup, sealStartup, startupWindowMs, verifyStartupProof } from '../src/lib/node-security/startup.js'
import { seal } from '../src/lib/node-security/vault.js'

const secret = randomBytes(32).toString('hex')
const agentId = 'agent-startup-fixture'
const base = { token: secret, timestamp: Date.now(), nonce: 'nonce-fixture-0001' }
const request = sealStartup(secret, agentId, 'request', base)
assert.deepEqual(openStartup(secret, agentId, 'request', request), base)
assert.deepEqual(verifyStartupProof(secret, agentId, request, base.timestamp + startupWindowMs), { nonce: base.nonce, timestamp: base.timestamp })
assert.deepEqual(verifyStartupProof(secret, agentId, request, base.timestamp - startupWindowMs), { nonce: base.nonce, timestamp: base.timestamp })
for (const invalid of [
  () => openStartup(secret, 'other-agent', 'request', request),
  () => openStartup(secret, agentId, 'response', request),
  () => openStartup(`${secret}x`, agentId, 'request', request),
  () => openStartup(secret, agentId, 'request', { ...request, ciphertext: request.ciphertext.slice(0, 10) + (request.ciphertext[10] === 'A' ? 'B' : 'A') + request.ciphertext.slice(11) }),
  () => verifyStartupProof(secret, agentId, request, base.timestamp + startupWindowMs + 1),
  () => verifyStartupProof(secret, agentId, request, base.timestamp - startupWindowMs - 1),
  () => verifyStartupProof(secret, agentId, sealStartup(secret, agentId, 'request', { ...base, token: 'wrong-token' }), base.timestamp),
  () => verifyStartupProof(secret, agentId, sealStartup(secret, agentId, 'request', { ...base, nonce: 'short' }), base.timestamp),
]) assert.throws(invalid)
assert.equal(startupWindowMs, 180_000)

const hostId = 778899
const dataset = 'tank/private'
const storageKey = randomBytes(32).toString('hex')
const master = randomBytes(48).toString('base64')
process.env.ENCRYPTION_KEY = master
let nodeState: any = { recoveryBlocked: false, recoveryGeneration: 0, report: { locked: false } }
let keyReads = 0
const used = new Set<string>()
const logRows: unknown[] = []
Object.assign(prisma, {
  $transaction: async (callback: (tx: any) => unknown) => callback(prisma),
  $executeRaw: async () => 1,
})
Object.assign(prisma.nodeSecurityState, {
  findUnique: async () => nodeState,
  upsert: async ({ create, update }: any) => {
    const changes = update ?? create
    const currentGeneration = nodeState?.recoveryGeneration ?? 0
    nodeState = { ...nodeState, ...changes }
    if (typeof changes.recoveryGeneration === 'object' && typeof changes.recoveryGeneration.increment === 'number') nodeState.recoveryGeneration = currentGeneration + changes.recoveryGeneration.increment
    return nodeState
  },
})
Object.assign(prisma.hostAgentNonce, {
  create: async ({ data }: any) => {
    if (used.has(data.nonce)) throw new Prisma.PrismaClientKnownRequestError('duplicate fixture nonce', { code: 'P2002', clientVersion: 'test' })
    used.add(data.nonce)
    return data
  },
})
Object.assign(prisma.nodeStorageKey, {
  findMany: async () => {
    keyReads++
    return [{ dataset, version: 1, ciphertext: seal(storageKey, { hostId, dataset, version: 1 }, master) }]
  },
})
Object.assign(prisma.log, { create: async ({ data }: any) => { logRows.push(data); return data } })

const agent = { hostId, agentId }
const grant = await grantStartupKeys(agent, secret, request)
assert.ok('envelope' in grant)
assert.deepEqual(openStartup(secret, agentId, 'response', grant.envelope!), {
  nonce: base.nonce, locked: false, generation: 0, keys: [{ dataset, key: storageKey }],
})
assert.equal(keyReads, 1)
assert.ok(!JSON.stringify(grant).includes(storageKey), 'plaintext key was not returned outside the sealed envelope')
assert.deepEqual(await grantStartupKeys(agent, secret, request), { replay: true })
assert.equal(keyReads, 1, 'replayed proof fetched keys again')

nodeState = { recoveryBlocked: false, recoveryGeneration: 0, report: { locked: true } }
const lockedProof = sealStartup(secret, agentId, 'request', { ...base, nonce: 'nonce-fixture-0002' })
const lockedGrant = await grantStartupKeys(agent, secret, lockedProof)
assert.ok('envelope' in lockedGrant)
assert.deepEqual(openStartup(secret, agentId, 'response', lockedGrant.envelope!), { nonce: 'nonce-fixture-0002', locked: true, generation: 0, keys: [] })
assert.equal(keyReads, 1, 'server revealed keys to an already locked node')

nodeState = { recoveryBlocked: false, recoveryGeneration: 0, report: { locked: false } }
const absent = await grantStartupKeys(agent, secret, sealStartup(secret, agentId, 'request', { ...base, nonce: 'nonce-fixture-0003' }))
assert.ok('envelope' in absent)

nodeState = null
assert.deepEqual(await grantStartupKeys(agent, secret, sealStartup(secret, agentId, 'request', { ...base, nonce: 'nonce-fixture-0004' })), { unavailable: true })

nodeState = { recoveryBlocked: false, recoveryGeneration: 0, report: { locked: false, reason: 'existing' } }
const invalid = await grantStartupKeys(agent, secret, { ...request, ciphertext: 'broken' })
assert.deepEqual(invalid, { invalid: true, generation: 1 })
assert.equal(nodeState.recoveryGeneration, 1, 'bad-proof lock generation was not returned from the committed upsert')
assert.equal(nodeState.recoveryBlocked, true, 'authenticated invalid inner proof did not stick the recovery lock')
assert.equal((nodeState.report as any).locked, true)
assert.equal((nodeState.report as any).reason, 'startup_verification')
assert.equal(logRows.length, 1)
assert.equal(JSON.stringify(logRows).includes(secret), false)
assert.equal(JSON.stringify(logRows).includes(storageKey), false)
const afterFailure = await grantStartupKeys(agent, secret, sealStartup(secret, agentId, 'request', { ...base, nonce: 'nonce-fixture-0005' }))
assert.ok('envelope' in afterFailure)
assert.deepEqual(openStartup(secret, agentId, 'response', afterFailure.envelope!), { nonce: 'nonce-fixture-0005', locked: true, generation: 1, keys: [] })

const route = readFileSync(new URL('../src/routes/agent.ts', import.meta.url), 'utf8')
const startupRoute = route.slice(route.indexOf("('/startup-keys'"), route.indexOf("('/security-report'"))
assert.ok(startupRoute.indexOf('authenticateAgentRequest') < startupRoute.indexOf('grantStartupKeys'), 'inner proof verification ran before outer signed agent authentication')
assert.match(startupRoute, /Startup service unavailable/)
assert.match(startupRoute, /Request could not be completed/)
console.log('Startup AES-GCM domain separation, ±180s freshness, replay handling, lock policy, and sealed key grant passed')
