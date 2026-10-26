import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer } from 'node:https'
import { X509Certificate } from 'node:crypto'
import { once } from 'node:events'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { hostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import net from 'node:net'

const dir = mkdtempSync(join(tmpdir(), 'node-transport-'))
let requests = 0
let lastBody = ''
try {
  const cert = join(dir, 'fixture.crt'), key = join(dir, 'fixture.key')
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-subj', '/CN=localhost', '-days', '1'], { stdio: 'ignore' })
  const pin = new X509Certificate(readFileSync(cert)).fingerprint256.replaceAll(':', '').toLowerCase()
  const server = createServer({ cert: readFileSync(cert), key: readFileSync(key) }, async (req, res) => {
    requests++
    lastBody = ''
    for await (const chunk of req) lastBody += chunk
    res.setHeader('Content-Type', 'application/json')
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
      } finally { await valid.close() }
      const before = requests
      const invalid = new IncusClient({ ...options, tunnelEnabled: tunnel, hostId: 1, serverFingerprint: '0'.repeat(64) })
      try { await assert.rejects(invalid.connect()) } finally { await invalid.close() }
      assert.equal(requests, before, 'HTTP request reached a node with the wrong certificate')
    } finally { hostTunnelManager.createDuplexStream = old }
  }
  await new Promise<void>((resolve, reject) => server.close(err => err ? reject(err) : resolve()))
  console.log('Pinned node TLS: direct and tunneled requests pass; wrong certificate cannot receive HTTP data')
} finally { rmSync(dir, { recursive: true, force: true }) }
