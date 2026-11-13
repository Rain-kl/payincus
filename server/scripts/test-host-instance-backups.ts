import assert from 'node:assert/strict'
import Fastify from 'fastify'
import routes from '../src/routes/host-instance-backups.js'
import { prisma } from '../src/db/prisma.js'
import { IncusClient, IncusApiError } from '../src/lib/incus/incus-client.js'

let support = true
let auditFails = false
let calls = 0
let audited = 0
let pin: string | null = 'a'.repeat(64)
Object.assign(prisma.host, { findUnique: async () => ({ id: 3, url: 'https://fixture.invalid:8443' }) })
Object.assign(prisma.nodeSecurityState, { findUnique: async () => ({ serverFingerprint: pin }) })
Object.assign(prisma.log, { create: async () => { if (auditFails) throw Error('audit unavailable'); audited++; return {} } })
const originals = { connect: IncusClient.prototype.connect, close: IncusClient.prototype.close, request: IncusClient.prototype.request }
IncusClient.prototype.connect = async () => ({} as any)
IncusClient.prototype.close = async () => {}
IncusClient.prototype.request = async function(method, path, body) {
  if (path === '/1.0/agent') return { api_extensions: support ? ['incus_instance_backup_v1'] : [] } as any
  calls++
  if (method === 'GET') return { tasks: [] } as any
  if ((body as any).directory === '/locked') throw new IncusApiError('node is locked', 409)
  return { ...(body as object), status: 'running' } as any
}
const app = Fastify()
app.decorate('authenticate', async (req: any, reply: any) => {
  if (!req.headers.authorization) return reply.code(401).send({ error: 'unauthorized' })
  req.user = { id: 1, role: req.headers.authorization }
})
app.decorate('requireAdmin', async (req: any, reply: any) => { if (req.user.role !== 'admin') return reply.code(403).send({ error: 'forbidden' }) })
try {
  await app.register(routes)
  const url = '/api/admin/hosts/3/instance-backups'
  const headers = { authorization: 'admin' }
  const payload = { id: 'a'.repeat(32), directory: '/mnt/backup' }
  for (const method of ['GET', 'POST'] as const) {
    assert.equal((await app.inject({ method, url, ...(method === 'POST' ? { payload } : {}) })).statusCode, 401)
    assert.equal((await app.inject({ method, url, headers: { authorization: 'user' }, ...(method === 'POST' ? { payload } : {}) })).statusCode, 403)
  }
  for (const bad of [{ ...payload, directory: '../relative' }, { ...payload, id: 'bad' }]) assert.equal((await app.inject({ method: 'POST', url, headers, payload: bad })).statusCode, 400)
  assert.equal(calls, 0)
  support = false
  assert.equal((await app.inject({ method: 'POST', url, headers, payload })).statusCode, 409)
  support = true
  pin = null
  assert.equal((await app.inject({ method: 'POST', url, headers, payload })).statusCode, 409)
  pin = 'a'.repeat(64)
  auditFails = true
  assert.equal((await app.inject({ method: 'POST', url, headers, payload })).statusCode, 502)
  assert.equal(calls, 0)
  auditFails = false
  const accepted = await app.inject({ method: 'POST', url, headers, payload })
  assert.equal(accepted.statusCode, 200)
  assert.equal(accepted.json().status, 'running')
  assert.equal(audited, 1)
  const listed = await app.inject({ method: 'GET', url, headers })
  assert.equal(listed.statusCode, 200)
  assert.equal(listed.headers['cache-control'], 'no-store')
  assert.deepEqual(listed.json(), { tasks: [] })
  assert.equal((await app.inject({ method: 'POST', url, headers, payload: { ...payload, directory: '/locked' } })).statusCode, 409)
  console.log('Host backup admin authorization, input, capability, audit and node status checks passed')
} finally {
  Object.assign(IncusClient.prototype, originals)
  await app.close()
  await prisma.$disconnect()
}
