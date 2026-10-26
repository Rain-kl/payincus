import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import Fastify from 'fastify'
import routes from '../src/routes/node-security.js'
import { prisma } from '../src/db/prisma.js'
import { seal, fingerprint } from '../src/lib/node-security/vault.js'

process.env.ENCRYPTION_KEY = randomBytes(48).toString('base64')
const key = randomBytes(32).toString('hex')
let row: any = null
let audit = 0
let failAudit = false
Object.assign(prisma.host, { findUnique: async () => ({ id: 9 }) })
Object.assign(prisma.nodeStorageKey, {
  findUnique: async () => row,
  create: async ({ data }: any) => { row = { id: 1, version: 1, ...data }; return { id: row.id, version: row.version, dataset: row.dataset, fingerprint: row.fingerprint } }
})
Object.assign(prisma.log, { create: async () => { if (failAudit) throw new Error('database unavailable'); audit++; return {} } })
const app = Fastify({ logger: false })
app.decorate('authenticate', async (req: any, reply: any) => {
  if (!req.headers.authorization) return reply.code(401).send({ error: 'unauthorized' })
  req.user = { id: 1, role: req.headers.authorization }
})
app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user?.role !== 'admin') return reply.code(403).send({ error: 'forbidden' }) })
await app.register(routes)
const base = '/api/admin/hosts/9/node-security'
for (const action of ['/identity', '/keys', '/keys/reveal', '/keys/unlock', '/policy', '/lock', '/recover']) {
  const body = action === '/identity' ? { fingerprint: 'a'.repeat(64) } : action === '/keys' ? { dataset: 'tank/private', key } : action.includes('/keys/') ? { dataset: 'tank/private' } : action === '/policy' ? { enabled: true, revision: 0 } : action === '/recover' ? { revision: 0 } : {}
  assert.equal((await app.inject({ method: 'POST', url: base + action, payload: body })).statusCode, 401)
  assert.equal((await app.inject({ method: 'POST', url: base + action, headers: { authorization: 'user' }, payload: body })).statusCode, 403)
}
assert.equal(audit, 0)
assert.equal(row, null)
const saved = await app.inject({ method: 'POST', url: base + '/keys', headers: { authorization: 'admin' }, payload: { dataset: 'tank/private', key } })
assert.equal(saved.statusCode, 200)
assert.equal(saved.json().fingerprint, fingerprint(key))
assert.equal(saved.body.includes(key), false)
assert.equal(row.ciphertext.includes(key), false)
const reveal = await app.inject({ method: 'POST', url: base + '/keys/reveal', headers: { authorization: 'admin' }, payload: { dataset: 'tank/private' } })
assert.equal(reveal.statusCode, 200)
assert.equal(reveal.json().key === key, true)
assert.equal(reveal.headers['cache-control'], 'no-store')
assert.ok(audit >= 2)
failAudit = true
const denied = await app.inject({ method: 'POST', url: base + '/keys/reveal', headers: { authorization: 'admin' }, payload: { dataset: 'tank/private' } })
assert.equal(denied.statusCode, 503)
assert.equal(denied.body.includes(key), false)
failAudit = false
row.ciphertext = seal(key, { hostId: 10, dataset: 'tank/private', version: 1 })
assert.equal((await app.inject({ method: 'POST', url: base + '/keys/reveal', headers: { authorization: 'admin' }, payload: { dataset: 'tank/private' } })).statusCode, 409)
assert.equal((await app.inject({ method: 'POST', url: base + '/keys', headers: { authorization: 'admin' }, payload: { dataset: 'tank/private', key } })).statusCode, 409)
assert.equal((await app.inject({ method: 'POST', url: '/api/admin/hosts/0/node-security/lock', headers: { authorization: 'admin' }, payload: {} })).statusCode, 400)
await app.close()
console.log('Node administration authorization, key enrollment, strict recovery and audit-before-reveal passed')
