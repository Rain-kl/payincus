import assert from 'node:assert/strict'
import Fastify from 'fastify'
import websocket from '@fastify/websocket'
import agentRoutes from '../src/routes/agent.js'
import { encryptSensitiveData } from '../src/lib/security.js'
import { createAgentBodyHash, createAgentSignature, generateAgentId, generateAgentSecret } from '../src/lib/agent-auth.js'
import { randomBytes } from 'node:crypto'
import { prisma } from '../src/db/prisma.js'
import { recordReport } from '../src/lib/node-security/report.js'
import { fingerprint, seal, unseal } from '../src/lib/node-security/vault.js'
import { openStartup, sealStartup } from '../src/lib/node-security/startup.js'

if (!process.env.DATABASE_URL?.includes('/node_security_validation')) throw new Error('Requires dedicated node_security_validation database')
const suffix = randomBytes(8).toString('hex')
const key = randomBytes(32).toString('hex')
const master = randomBytes(48).toString('base64')
const user = await prisma.user.create({ data: { username: `node-fixture-${suffix}`, passwordHash: 'disabled-fixture-account' } })
const host = await prisma.host.create({ data: { userId: user.id, name: 'isolated node fixture', url: 'https://127.0.0.1:1' } })
process.env.ENCRYPTION_KEY = master
const app = Fastify({ logger: false })
app.decorate('authenticate', async () => {})
app.decorate('authenticateAdmin', async () => {})
await app.register(websocket)
await app.register(agentRoutes, { prefix: '/api/agent' })
try {
  const distribution = await app.inject({ url: '/api/agent/distribution' })
  assert.equal(distribution.statusCode, 200)
  assert.equal(distribution.json().repository, 'Rain-kl/incus')
  assert.equal(distribution.json().managedBy, 'apt')
  for (const url of ['/api/agent/manifest.json', '/api/agent/binary/incudal-agent-linux-amd64']) assert.equal((await app.inject({ url })).statusCode, 404)
  const identity = { hostId: host.id, dataset: 'tank/private', version: 1 }
  await prisma.nodeStorageKey.create({ data: { hostId: host.id, dataset: identity.dataset, fingerprint: fingerprint(key), ciphertext: seal(key, identity, master) } })
  const stored = await prisma.nodeStorageKey.findUniqueOrThrow({ where: { hostId_dataset: { hostId: host.id, dataset: identity.dataset } } })
  assert.equal(stored.ciphertext.includes(key), false)
  assert.equal(unseal(stored.ciphertext, identity, master) === key, true)
  const event = { id: randomBytes(16).toString('hex'), time: new Date().toISOString(), uid: 4294967294, pid: 7, command: 'exec', critical: true, blocked: true }
  const report = { state: { enabled: true, locked: true, revision: 3 }, events: [event] }
  await prisma.nodeSecurityState.create({ data: { hostId: host.id, report: {}, serverFingerprint: 'a'.repeat(64) } })
  const ack = await recordReport(host.id, report)
  assert.deepEqual(ack, [event.id])
  await recordReport(host.id, report)
  assert.equal(await prisma.nodeCliEvent.count({ where: { hostId: host.id } }), 1)
  const entry = await prisma.nodeCliEvent.findFirstOrThrow({ where: { hostId: host.id } })
  assert.equal(entry.uid, BigInt(event.uid))
  assert.equal((await prisma.nodeSecurityState.findUniqueOrThrow({ where: { hostId: host.id } })).report !== null, true)
  const logs = await prisma.log.findMany({ where: { action: 'node.cli_alarm', content: { contains: `#${host.id} ` } } })
  assert.equal(logs.length, 1)
  assert.equal(logs.some(l => l.content.includes(key)), false)
  await recordReport(host.id, { state: { enabled: false, locked: false, revision: 1 }, events: [] })
  const latest = await prisma.nodeSecurityState.findUniqueOrThrow({ where: { hostId: host.id } })
  assert.equal((latest.report as any).revision, 3)
  assert.equal(latest.serverFingerprint, 'a'.repeat(64))
  const agentId = generateAgentId(), agentSecret = generateAgentSecret()
  await prisma.hostAgent.create({ data: { hostId: host.id, agentId, secretHash: 'fixture-unused', secretEncrypted: encryptSensitiveData(agentSecret), lastReport: { preserved: true } } })
  function headers(body: unknown, path: string, nonce = randomBytes(16).toString('hex'), bodyHash = createAgentBodyHash(body)) {
    const timestamp = String(Date.now())
    return { 'x-incudal-agent-id': agentId, 'x-incudal-timestamp': timestamp, 'x-incudal-nonce': nonce, 'x-incudal-body-sha256': bodyHash, 'x-incudal-signature': createAgentSignature(agentSecret, { method: 'POST', path, timestamp, nonce, bodyHash }) }
  }
  const path = '/api/agent/security-report'
  const signed = headers(report, path)
  const accepted = await app.inject({ method: 'POST', url: path, headers: signed, payload: report })
  assert.equal(accepted.statusCode, 200)
  assert.deepEqual(accepted.json().ack, [event.id])
  assert.equal((await app.inject({ method: 'POST', url: path, headers: signed, payload: report })).statusCode, 401)
  assert.equal((await app.inject({ method: 'POST', url: path, headers: headers(report, path), payload: { ...report, state: { ...report.state, locked: false } } })).statusCode, 401)
  const malformed = { state: {}, events: [] }
  assert.equal((await app.inject({ method: 'POST', url: path, headers: headers(malformed, path), payload: malformed })).statusCode, 503)
  const heartbeat = { nodeApi: 'payincus_node_v1', nodeSecurity: { state: { locked: true } } }
  assert.equal((await app.inject({ method: 'POST', url: '/api/agent/heartbeat', headers: headers({}, '/api/agent/heartbeat'), payload: {} })).statusCode, 409)
  const heartbeatResponse = await app.inject({ method: 'POST', url: '/api/agent/heartbeat', headers: headers(heartbeat, '/api/agent/heartbeat'), payload: heartbeat })
  assert.equal(heartbeatResponse.statusCode, 200)
  const agent = await prisma.hostAgent.findUniqueOrThrow({ where: { agentId } })
  assert.deepEqual(agent.lastReport, { preserved: true })
  assert.equal(agent.status, 'online')
  const startupPath = '/api/agent/startup-keys'
  await prisma.nodeSecurityState.update({ where: { hostId: host.id }, data: { recoveryBlocked: false, report: { enabled: true, locked: false, revision: 4 } } })
  const innerNonce = `nonce-${randomBytes(12).toString('hex')}`
  const inner = sealStartup(agentSecret, agentId, 'request', { token: agentSecret, timestamp: Date.now(), nonce: innerNonce })
  const startup = await app.inject({ method: 'POST', url: startupPath, headers: headers(inner, startupPath), payload: inner })
  assert.equal(startup.statusCode, 200)
  assert.equal(startup.headers['cache-control'], 'no-store')
  assert.deepEqual(openStartup(agentSecret, agentId, 'response', startup.json()), { nonce: innerNonce, locked: false, generation: 0, keys: [{ dataset: identity.dataset, key }] })
  const replay = await app.inject({ method: 'POST', url: startupPath, headers: headers(inner, startupPath), payload: inner })
  assert.equal(replay.statusCode, 409)
  assert.equal((await prisma.nodeSecurityState.findUniqueOrThrow({ where: { hostId: host.id } })).recoveryBlocked, false)

  const badInner = { ...inner, ciphertext: 'invalid-ciphertext' }
  const badOuter = headers(badInner, startupPath)
  badOuter['x-incudal-signature'] = '0'.repeat(64)
  assert.equal((await app.inject({ method: 'POST', url: startupPath, headers: badOuter, payload: badInner })).statusCode, 401)
  assert.equal((await prisma.nodeSecurityState.findUniqueOrThrow({ where: { hostId: host.id } })).recoveryBlocked, false, 'outer identity failure must never lock a node')

  const authenticatedBadProof = await app.inject({ method: 'POST', url: startupPath, headers: headers(badInner, startupPath), payload: badInner })
  assert.equal(authenticatedBadProof.statusCode, 409)
  const blocked = await prisma.nodeSecurityState.findUniqueOrThrow({ where: { hostId: host.id } })
  assert.equal(blocked.recoveryBlocked, true)
  assert.equal(blocked.recoveryGeneration, 1)
  assert.equal((blocked.report as any).locked, true)
  const blockedReport = { state: blocked.report, events: [] }
  const generationReport = await app.inject({ method: 'POST', url: path, headers: headers(blockedReport, path), payload: blockedReport })
  assert.equal(generationReport.statusCode, 200)
  assert.deepEqual(generationReport.json().ack, [])
  assert.equal(generationReport.json().recoveryBlocked, true)
  assert.equal(generationReport.json().recoveryGeneration, 1, 'security report did not relay the committed lock generation')
  const lockedProof = sealStartup(agentSecret, agentId, 'request', { token: agentSecret, timestamp: Date.now(), nonce: `nonce-${randomBytes(12).toString('hex')}` })
  const lockedGrant = await app.inject({ method: 'POST', url: startupPath, headers: headers(lockedProof, startupPath), payload: lockedProof })
  assert.equal(lockedGrant.statusCode, 200)
  const lockedGrantPayload = openStartup(agentSecret, agentId, 'response', lockedGrant.json())
  assert.deepEqual(lockedGrantPayload.keys, [])
  assert.equal(lockedGrantPayload.generation, 1)
  console.log('PostgreSQL + real HMAC route: report ACK, sealed startup grant, nonce replay, outer-auth isolation, sticky inner-proof lock and locked key denial passed')

} finally {
  await app.close()
  await prisma.log.deleteMany({ where: { action: 'node.cli_alarm', content: { contains: `#${host.id} ` } } })
  await prisma.host.delete({ where: { id: host.id } })
  await prisma.user.delete({ where: { id: user.id } })
  await prisma.$disconnect()
}
