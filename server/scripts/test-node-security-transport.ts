import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer } from 'node:https'
import { X509Certificate } from 'node:crypto'
import { once } from 'node:events'
import { IncusClient, IncusApiError } from '../src/lib/incus/incus-client.js'
import { hostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import net from 'node:net'

const dir = mkdtempSync(join(tmpdir(), 'node-transport-'))
let requests = 0
let lastBody = ''
let failure: { status: number; message: string; plain?: boolean } | null = null
let asyncFailure = false
try {
  const cert = join(dir, 'fixture.crt'), key = join(dir, 'fixture.key')
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-subj', '/CN=localhost', '-days', '1'], { stdio: 'ignore' })
  const pin = new X509Certificate(readFileSync(cert)).fingerprint256.replaceAll(':', '').toLowerCase()
  const server = createServer({ cert: readFileSync(cert), key: readFileSync(key) }, async (req, res) => {
    requests++
    lastBody = ''
    for await (const chunk of req) lastBody += chunk
    res.setHeader('Content-Type', 'application/json')
    if (asyncFailure && req.url === '/1.0/instances') { res.end(JSON.stringify({ type: 'async', operation: '/1.0/operations/fixture' })); return }
    if (failure) { res.statusCode = failure.status; res.end(failure.plain ? failure.message : JSON.stringify({ type: 'error', error: failure.message })); return }
    res.end(JSON.stringify({ type: 'sync', metadata: { ok: true } }))
  })
  server.listen(0, '127.0.0.1'); await once(server, 'listening')
  const address = server.address(); assert.ok(address && typeof address !== 'string')
  const url = `https://127.0.0.1:${address.port}`
  const options = { url, certPath: cert, keyPath: key, serverFingerprint: pin }
  for (const tunnel of [false, true]) {
    const old = hostTunnelManager.createDuplexStream
    hostTunnelManager.createDuplexStream = (() => net.connect(address.port, '127.0.0.1')) as typeof old
    try {
      const valid = new IncusClient({ ...options, tunnelEnabled: tunnel, hostId: 1 })
      try {
        await valid.connect()
        assert.deepEqual(await valid.request('POST', '/1.0/payincus/security/unlock', { key: 'fixture-only' }), { ok: true })
        assert.equal(lastBody, JSON.stringify({ key: 'fixture-only' }))
        for (const scenario of [
          { status: 409, message: 'refresh recovery state' },
          { status: 403, message: 'You must be authenticated' },
          { status: 403, message: 'permission denied by node access handler', plain: true },
          { status: 500, message: 'ZFS property unavailable: keyformat' },
          { status: 500, message: 'Failed to begin transaction: sql: database disk image is malformed (11)' }
        ]) {
          failure = scenario
          for (const endpoint of ['/1.0/payincus/security/recover', '/1.0/instances', '/1.0/storage-pools']) {
            await assert.rejects(valid.request('POST', endpoint, { revision: 1 }), (error: unknown) => {
              assert.ok(error instanceof IncusApiError)
              assert.equal(error.message, scenario.message)
              assert.equal(error.statusCode, scenario.status)
              return true
            })
          }
        }
        failure = { status: 403, message: 'operation wait access denied' }
        asyncFailure = true
        await assert.rejects(valid.request('POST', '/1.0/instances', {}), (error: unknown) => error instanceof IncusApiError && error.statusCode === 403 && error.message === 'operation wait access denied')
        asyncFailure = false
        const secret = 'fixture-private-key-material'
        failure = { status: 409, message: `key rejected: ${secret}` }
        await assert.rejects(valid.request('POST', '/1.0/payincus/storage/create', { key: secret }), (error: unknown) => {
          assert.ok(error instanceof IncusApiError)
          assert.equal(error.message.includes(secret), false)
          assert.match(error.message, /key rejected: \[REDACTED\]/)
          return true
        })
        failure = null
      } finally { await valid.close() }
      const before = requests
      const invalid = new IncusClient({ ...options, tunnelEnabled: tunnel, hostId: 1, serverFingerprint: '0'.repeat(64) })
      try { await assert.rejects(invalid.connect()) } finally { await invalid.close() }
      assert.equal(requests, before, 'HTTP request reached a node with the wrong certificate')
    } finally { hostTunnelManager.createDuplexStream = old }
  }
  await new Promise<void>((resolve, reject) => server.close(err => err ? reject(err) : resolve()))
  console.log('Pinned TLS direct/tunnel: ordinary, management, non-JSON and async errors retain causes/status; explicit lock response preserved; request keys redacted and wrong certificate rejected')
} finally { rmSync(dir, { recursive: true, force: true }) }
