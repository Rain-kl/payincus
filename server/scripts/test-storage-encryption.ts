import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import Fastify from 'fastify'
import routes from '../src/routes/storage-encryption.js'
import { prisma } from '../src/db/prisma.js'
import { IncusClient, IncusApiError } from '../src/lib/incus/incus-client.js'
import { fingerprint } from '../src/lib/node-security/vault.js'

process.env.ENCRYPTION_KEY = randomBytes(48).toString('base64')
const key = randomBytes(32).toString('hex')
let stored: any = null
let registered = false
let available = false
let supported = true
let nodeLocked = false
let failSave = false
let requests = 0
let managementDenied = false
Object.assign(prisma.storagePool, { updateMany: async () => ({count:1}), findUnique: async () => ({driver:'zfs',config:{},telemetry:{encryption:{encrypted:true,state:available?'unlocked':'locked',dataset:'test/incus',encryptionRoot:'test/incus',keyFormat:'hex'}}}) })
Object.assign(prisma.host, { findUnique: async () => ({ id: 4, userId: 10, url: 'https://node.invalid', tunnelEnabled: false }) })
Object.assign(prisma.nodeSecurityState, { findUnique: async () => ({ serverFingerprint: 'a'.repeat(64) }) })
Object.assign(prisma.nodeStorageKey, {
  findUnique: async () => stored,
  upsert: async ({ create }: any) => { if (failSave) throw new Error('fixture database failure'); stored ??= create; return stored },
  updateMany: async ({ where, data }: any) => { assert.equal(where.fingerprint, stored.fingerprint); stored.ciphertext = data.ciphertext; return { count: 1 } }
})
Object.assign(IncusClient.prototype, {
  connect: async function (this: IncusClient) { assert.equal(this.options.serverFingerprint, 'a'.repeat(64)) }, close: async () => {},
  request: async (method: string, path: string, body?: any) => {
    requests++
    if (path === '/1.0/payincus') return { api_extensions: supported ? ['payincus_storage_encryption_v1'] : [] }
    if (path === '/1.0/storage-pools/data') return { driver: 'zfs', config: { 'zfs.pool_name': 'test/incus', source: '/some/loop.img' } }
    if (path === '/1.0/payincus/storage') return { datasets: [{ dataset: 'test/incus', encryptionRoot: 'test/incus', algorithm: 'aes-256-gcm', keyFormat: 'hex', keyStatus: available ? 'available' : 'unavailable' }] }
    if (path === '/1.0/payincus/security') return { locked: nodeLocked, targets: registered ? [{ dataset: 'test/incus', fingerprint: fingerprint(key) }] : [] }
    if (method === 'POST' && ['/1.0/payincus/storage/register', '/1.0/payincus/storage/unlock'].includes(path)) {
      if (managementDenied) throw new IncusApiError('storage registration permission denied', 403, body)
      assert.equal(body.dataset, 'test/incus')
      assert.equal(body.key, key)
      if (nodeLocked) assert.equal(path, '/1.0/payincus/storage/unlock')
      available = true; registered = true; return {}
    }
    throw new Error('unexpected endpoint')
  }
})
const app = Fastify({ logger: false })
app.decorate('authenticate', async (req: any, reply: any) => {
  if (!req.headers.authorization) return reply.code(401).send({ error: 'unauthorized' })
  req.user = { id: req.headers.authorization === 'other' ? 11 : 10, role: req.headers.authorization === 'admin' ? 'admin' : 'user' }
})
await app.register(routes)
const url = '/api/hosts/4/storage-pools/data/encryption'
const call = (method: 'GET' | 'POST', payload?: any, authorization = 'owner') => app.inject({ method, url, headers: { authorization }, ...(payload ? { payload } : {}) })
assert.equal((await app.inject({ url })).statusCode, 401)
assert.equal((await call('GET', undefined, 'other')).statusCode, 403)
assert.equal(requests, 0)
for (const id of ['0', '0x4', '1.5', '04', '9999999999']) assert.equal((await app.inject({ url: url.replace('/4/', `/${id}/`), headers: { authorization: 'admin' } })).statusCode, 400)
const state = await call('GET')
assert.equal(state.statusCode, 200)
assert.equal(state.json().state, 'locked')
assert.equal(state.json().encrypted, true)
assert.equal(state.json().keySaved, false)
assert.equal((await call('POST', {})).statusCode, 409)
assert.equal((await call('POST', { key: 'bad' })).statusCode, 400)
nodeLocked = true
assert.equal((await call('POST', { key })).statusCode, 423)
assert.equal(registered, false)
nodeLocked = false
managementDenied = true
const deniedManagement = await call('POST', { key })
assert.equal(deniedManagement.statusCode, 403)
assert.match(deniedManagement.json().error, /storage registration permission denied/)
assert.equal(deniedManagement.body.includes('database disk image'), false)
managementDenied = false
const saved = await call('POST', { key })
assert.equal(saved.statusCode, 200)
assert.equal(saved.json().state, 'unlocked')
assert.equal(saved.json().keySaved, true)
assert.equal(saved.body.includes(key), false)
assert.equal(stored.ciphertext.includes(key), false)
assert.equal(saved.headers['cache-control'], 'no-store')
available = false; nodeLocked = true
const reuse = await call('POST', {})
assert.equal(reuse.statusCode, 200)
assert.equal(available, true)
assert.equal(nodeLocked, true)
const oldCiphertext = stored.ciphertext
assert.equal((await call('POST', { key: randomBytes(32).toString('hex') })).statusCode, 409)
assert.equal(stored.ciphertext, oldCiphertext)
failSave = true
const failedSave = await call('POST', { key })
assert.equal(failedSave.statusCode, 503)
assert.match(failedSave.json().error, /已加载密钥.*保存失败/)
assert.equal(failedSave.body.includes(key), false)
failSave = false
supported = false
const beforeRead = requests
assert.equal((await call('GET')).statusCode, 200)
assert.equal(requests, beforeRead, 'Cached encryption reads must not probe node capabilities')
assert.equal((await call('POST', {key})).statusCode, 409)
await app.close()
console.log('Storage encryption ownership, discovery, pinned adapter, custody, failed-save truthfulness and saved-key unlock passed')
