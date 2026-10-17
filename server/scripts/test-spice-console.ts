import assert from 'node:assert/strict'
import { once } from 'node:events'
import https from 'node:https'
import net from 'node:net'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { execFileSync } from 'node:child_process'
import Fastify from 'fastify'
import websocket from '@fastify/websocket'
import WebSocket, { WebSocketServer } from 'ws'
import { SpiceSessionManager, spiceSessions } from '../src/lib/spice-sessions.js'
import { hostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import { logSerializers } from '../src/lib/log-sanitizer.js'
import type { Host } from '../src/types/database.js'

// Every DB-facing route dependency is replaced below. This URL is never connected.
process.env.DATABASE_URL ||= 'postgresql://test:test@127.0.0.1:1/incudal_spice_test'
const { default: spiceRoutes } = await import('../src/routes/spice.js')
const { createSpiceBackend } = await import('../src/lib/incus/spice-connection.js')
const { closeAllClients } = await import('../src/lib/incus/incus-pool.js')
const { closeUserSessions, closeSessionTerminalSessions } = await import('../src/lib/terminal-proxy.js')

const directory = mkdtempSync(join(tmpdir(), 'incudal-spice-test-'))
const keyPath = join(directory, 'key.pem')
const certPath = join(directory, 'cert.pem')
execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', keyPath, '-out', certPath, '-days', '1', '-subj', '/CN=localhost'], { stdio: 'ignore' })
let operations = 0
const activeOperations = new Set<string>()
const cancellations: string[] = []
let rejectControl = false
let missingDataFd = false
let rejectCancellation = false
let operationAlreadyFinished = false
let orphanedConsole = false
let forcedOpens = 0
const channels: string[] = []
const peers = new Set<WebSocket>()
const fakeIncus = https.createServer({ key: readFileSync(keyPath), cert: readFileSync(certPath), ca: readFileSync(certPath), requestCert: true }, (req, res) => {
  res.setHeader('content-type', 'application/json')
  if (req.method === 'GET' && req.url === '/1.0') return res.end(JSON.stringify({ type: 'sync', metadata: { api_extensions: [] } }))
  if (req.method === 'DELETE' && req.url?.startsWith('/1.0/operations/')) {
    if (rejectCancellation) { res.statusCode = 503; return res.end('{}') }
    if (operationAlreadyFinished) { res.statusCode = 400; return res.end('{}') }
    cancellations.push(req.url)
    const existed = activeOperations.delete(req.url.split('/').at(-1)!)
    res.statusCode = existed ? 200 : 404
    return res.end(JSON.stringify({ type: 'sync', metadata: {} }))
  }
  if (req.method === 'GET' && req.url?.startsWith('/1.0/operations/')) {
    res.statusCode = activeOperations.has(req.url.split('/').at(-1)!) ? 200 : 404
    return res.end(JSON.stringify({ type: 'sync', metadata: { status: operationAlreadyFinished ? 'Success' : 'Running' } }))
  }
  if (req.method === 'POST' && req.url === '/1.0/instances/vm-test/console') {
    let body = ''
    req.on('data', chunk => { body += chunk })
    req.on('end', () => {
      assert.equal((req.socket as any).getPeerCertificate().subject.CN, 'localhost', 'mTLS client certificate required')
      assert.equal(JSON.parse(body).type, 'vga')
      const reconnect = JSON.parse(body).force === true
      if (orphanedConsole && !reconnect) {
        res.statusCode = 500
        return res.end(JSON.stringify({ type: 'error', error: 'This console is already connected. Force is required to take it over.' }))
      }
      if (reconnect) { forcedOpens++; orphanedConsole = false }
      operations++
      activeOperations.add(`op-${operations}`)
      res.statusCode = 202
      res.end(JSON.stringify({ type: 'async', metadata: { id: `op-${operations}`, metadata: { fds: { ...(!missingDataFd ? { '0': 'data-secret' } : {}), control: 'control-secret' } } } }))
    })
  } else { res.statusCode = 404; res.end('{}') }
})
const wss = new WebSocketServer({ noServer: true })
fakeIncus.on('upgrade', (req, socket, head) => {
  if (rejectControl && new URL(req.url!, 'https://localhost').searchParams.get('secret') === 'control-secret') {
    socket.destroy()
    return
  }
  wss.handleUpgrade(req, socket, head, ws => {
  peers.add(ws)
  ws.on('close', () => peers.delete(ws))
  const url = new URL(req.url!, 'https://localhost')
  channels.push(url.pathname)
  if (url.searchParams.get('secret') === 'data-secret') ws.on('message', data => ws.send(data, { binary: true }))
  })
})
await new Promise<void>(resolve => fakeIncus.listen(0, '127.0.0.1', resolve))
const port = (fakeIncus.address() as net.AddressInfo).port
const host = { id: 96101, url: `https://127.0.0.1:${port}`, cert_path: certPath, key_path: keyPath, user_id: 3 } as Host
const originalDuplex = hostTunnelManager.createDuplexStream
const originalOnline = hostTunnelManager.isTunnelOnline
const app = Fastify()
let invalidated = false
let owner = 1
let running = true
let vm = true
const origin = { origin: 'http://localhost:3000' }
const settle = () => new Promise(resolve => setTimeout(resolve, 30))
const closedSocket = (socket: WebSocket): Promise<any[]> => socket.readyState === WebSocket.CLOSED ? Promise.resolve([(socket as any)._closeCode]) : once(socket, 'close')
try {
  // Exercise the real Incus mTLS transport, both direct and over the tunnel duplex.
  for (const tunnel of [false, true]) {
    let duplexCalls = 0
    hostTunnelManager.isTunnelOnline = () => true
    hostTunnelManager.createDuplexStream = (() => { duplexCalls++; return net.connect(port, '127.0.0.1') }) as any
    const backend = await createSpiceBackend({ ...host, id: tunnel ? 96102 : 96101, tunnel_enabled: tunnel } as any, 'vm-test', AbortSignal.timeout(5000))
    const a = await backend.openData(AbortSignal.timeout(5000))
    const b = await backend.openData(AbortSignal.timeout(5000))
    const reply = once(b, 'message')
    b.send(Buffer.from([0, 255, 128, 7]), { binary: true })
    assert.deepEqual((await reply)[0], Buffer.from([0, 255, 128, 7]))
    assert.equal(a.readyState, WebSocket.OPEN)
    assert.ok(channels.slice(-3).every(path => path === `/1.0/operations/op-${operations}/websocket`), 'all channels share one operation')
    if (tunnel) assert.ok(duplexCalls >= 4, 'HTTP + control + both data channels must use the tunnel')
    const beforeCancel = cancellations.length
    await Promise.all([backend.close(), backend.close()])
    assert.equal(cancellations.length, beforeCancel + 1, 'duplicate close cancels exactly its own operation once')
    assert.equal(activeOperations.size, 0, 'closing sockets must also cancel the Incus operation')
    orphanedConsole = true
    await assert.rejects(createSpiceBackend({ ...host, id: tunnel ? 96102 : 96101, tunnel_enabled: tunnel } as any,
      'vm-test', AbortSignal.timeout(5000)), /create/)
    assert.equal(orphanedConsole, true, 'ordinary opens must not take over an existing console')
    const takeover = await createSpiceBackend({ ...host, id: tunnel ? 96102 : 96101, tunnel_enabled: tunnel } as any,
      'vm-test', AbortSignal.timeout(5000), true)
    assert.equal(orphanedConsole, false, 'explicit reconnect must take over orphaned Incus consoles')
    await takeover.close()
    await settle()
    console.log(`SPICE transport passed (${tunnel ? "tunnel" : "direct"})`)
  }
  assert.equal(forcedOpens, 2, 'takeover must work both directly and through the reverse tunnel')
  hostTunnelManager.isTunnelOnline = originalOnline
  hostTunnelManager.createDuplexStream = originalDuplex
  for (const fail of ['control', 'metadata'] as const) {
    rejectControl = fail === 'control'
    missingDataFd = fail === 'metadata'
    await assert.rejects(createSpiceBackend(host, 'vm-test', AbortSignal.timeout(5000)), new RegExp(fail))
    assert.equal(activeOperations.size, 0, `${fail} setup failure must release the operation`)
  }
  rejectControl = missingDataFd = false
  const retryBackend = await createSpiceBackend(host, 'vm-test', AbortSignal.timeout(5000))
  rejectCancellation = true
  await assert.rejects(retryBackend.close(), /cleanup/)
  rejectCancellation = false
  await retryBackend.close()
  assert.equal(activeOperations.size, 0, 'failed cancellation can be retried with a fresh signal')
  const expiredBackend = await createSpiceBackend(host, 'vm-test', AbortSignal.timeout(5000))
  activeOperations.clear()
  await expiredBackend.close() // Incus may already have removed an ended operation.
  const setupAbort = new AbortController()
  const abortedBackend = await createSpiceBackend(host, 'vm-test', setupAbort.signal)
  setupAbort.abort()
  await abortedBackend.close()
  assert.equal(activeOperations.size, 0, 'cleanup must not reuse the aborted session signal')
  const finishedBackend = await createSpiceBackend(host, 'vm-test', AbortSignal.timeout(5000))
  operationAlreadyFinished = true
  await finishedBackend.close()
  activeOperations.clear()
  operationAlreadyFinished = false
  await app.register(websocket)
  app.decorate('authenticate', async (req: any, reply: any) => {
    if (!req.headers['x-user']) return reply.code(401).send({ error: 'Unauthorized' })
    req.user = { id: Number(req.headers['x-user']), iat: 100, sid: 'session-1' }
  })
  await app.register(spiceRoutes, { prefix: '/api/ws/instances', dependencies: {
    getInstance: (async (id: number) => id === 6 ? { id: 6, host_id: host.id, user_id: owner, package_id: 1, status: running ? 'running' : 'stopped', incus_id: 'vm-test' } : null) as any,
    getHost: (async () => host) as any,
    getPackage: (async () => ({ instance_type: vm ? 'vm' : 'container' })) as any,
    getUser: (async (id: number) => ({ id, role: id === 2 ? 'admin' : 'user', status: 'active' })) as any,
    invalidated: async () => invalidated,
    audit: (async () => ({})) as any
  } })
  await app.listen({ port: 0, host: '127.0.0.1' })
  const appPort = (app.server.address() as net.AddressInfo).port
  const connectWs = async (path: string, options: { headers: Record<string, string> }) => {
    const url = new URL(path, `ws://127.0.0.1:${appPort}`)
    const ticket = url.searchParams.get("ticket")
    url.search = ""
    const ws = new WebSocket(url, ticket ? ["binary", `incudal-spice.${ticket}`] : undefined, options)
    await once(ws, 'open')
    return ws
  }
  const post = (id = '6', user = '1', reconnect = false) => app.inject({ method: 'POST', url: `/api/ws/instances/${id}/spice-session`, headers: { 'x-user': user }, payload: { reconnect } })
  assert.equal((await app.inject({ method: 'POST', url: '/api/ws/instances/6/spice-session' })).statusCode, 401)
  for (const id of ['0', '6x', '-6', '9007199254740992']) assert.equal((await post(id)).statusCode, 400)
  assert.equal((await post('6', '4')).statusCode, 403, 'unrelated user denied')
  vm = false; assert.equal((await post()).statusCode, 403, 'LXC cannot open SPICE'); vm = true
  running = false; assert.equal((await post()).statusCode, 403); running = true
  invalidated = true; assert.equal((await post()).statusCode, 403); invalidated = false
  for (const user of ['2', '3']) {
    const response = await post('6', user)
    assert.equal(response.statusCode, 200, 'admin and host owner permitted')
    spiceSessions.close(response.json().ticket)
  }
  console.log("SPICE permission checks passed")
  const before = operations
  const responses = await Promise.all([post(), post()])
  assert.deepEqual(responses.map(r => r.statusCode).sort(), [200, 409], 'concurrent opens reserve the instance slot')
  assert.equal(operations, before + 1)
  const ticket = responses.find(r => r.statusCode === 200)!.json().ticket
  assert.equal(responses.find(r => r.statusCode === 200)!.headers['cache-control'], 'no-store')
  assert.ok(!responses.find(r => r.statusCode === 200)!.body.includes('data-secret'))
  assert.equal((await app.inject({ method: 'DELETE', url: '/api/ws/instances/6/spice-session', headers: { 'x-user': '4' }, payload: { ticket } })).statusCode, 403)
  console.log("SPICE concurrent session limits passed")
  const badOrigin = await connectWs(`/api/ws/instances/6/spice?ticket=${ticket}`, { headers: { origin: 'https://unrelated.invalid' } })
  assert.equal((await closedSocket(badOrigin))[0], 4003)
  const wrongInstance = await connectWs(`/api/ws/instances/7/spice?ticket=${ticket}`, { headers: origin })
  assert.equal((await closedSocket(wrongInstance))[0], 4001)
  console.log("SPICE origin and instance checks passed")
  const data = await connectWs(`/api/ws/instances/6/spice?ticket=${ticket}`, { headers: origin })
  assert.equal(data.protocol, 'binary', 'the negotiated protocol must not echo the console capability')
  // Immediate handshake bytes must survive asynchronous authorization/backend setup.
  const echoed = once(data, 'message')
  data.send(Buffer.from([82, 69, 68, 81, 0, 255]), { binary: true })
  assert.deepEqual((await echoed)[0], Buffer.from([82, 69, 68, 81, 0, 255]))
  console.log("SPICE main channel handshake passed")
  const aux = await connectWs(`/api/ws/instances/6/spice?ticket=${ticket}`, { headers: origin })
  const echoedAux = once(aux, 'message'); aux.send(Buffer.from('auxiliary'), { binary: true })
  assert.equal((await echoedAux)[0].toString(), 'auxiliary')
  assert.equal(operations, before + 1, 'auxiliary channels must not create another console')
  const closed = closedSocket(data)
  const auxClosed = closedSocket(aux)
  assert.equal(closeSessionTerminalSessions('session-1'), 1, 'logout must close graphics too')
  await Promise.all([closed, auxClosed])
  for (let i = 0; i < 2; i++) {
    assert.equal((await app.inject({ method: 'DELETE', url: '/api/ws/instances/6/spice-session', headers: { 'x-user': '1' }, payload: { ticket } })).statusCode, 200, 'close after main-channel cleanup is idempotent')
  }
  const replay = await connectWs(`/api/ws/instances/6/spice?ticket=${ticket}`, { headers: origin })
  assert.equal((await closedSocket(replay))[0], 4001)
  const previous = (await post()).json().ticket
  assert.equal((await post('6', '4', true)).statusCode, 403, 'unauthorized reconnect cannot close another user console')
  assert.ok(spiceSessions.get(previous, 6))
  invalidated = true
  assert.equal((await post('6', '1', true)).statusCode, 403)
  assert.ok(spiceSessions.get(previous, 6), 'revoked credentials cannot trigger takeover')
  invalidated = false
  const replaced = await post('6', '1', true)
  assert.equal(replaced.statusCode, 200)
  assert.equal(spiceSessions.get(previous, 6), undefined, 'reconnect must invalidate the previous local ticket')
  assert.ok(spiceSessions.get(replaced.json().ticket, 6))
  const reconnects = await Promise.all([post('6', '1', true), post('6', '1', true)])
  assert.deepEqual(reconnects.map(r => r.statusCode).sort(), [200, 409], 'concurrent reconnects must not repeatedly evict each other')
  await spiceSessions.close(reconnects.find(r => r.statusCode === 200)!.json().ticket)
  const revoked = (await post()).json().ticket
  invalidated = true
  const invalid = await connectWs(`/api/ws/instances/6/spice?ticket=${revoked}`, { headers: origin })
  await closedSocket(invalid)
  assert.equal(spiceSessions.get(revoked, 6), undefined)
  invalidated = false
  const earlyTicket = (await post()).json().ticket
  const early = await connectWs(`/api/ws/instances/6/spice?ticket=${earlyTicket}`, { headers: origin })
  early.close(); await closedSocket(early); await settle()
  assert.equal(spiceSessions.get(earlyTicket, 6), undefined, 'early disconnect releases pending console')
  const banned = (await post()).json().ticket
  assert.equal(closeUserSessions(1), 1)
  assert.equal(spiceSessions.get(banned, 6), undefined)
  owner = 4
  assert.equal((await post()).statusCode, 403, 'ownership changes immediately deny future sessions')
  const manager = new SpiceSessionManager()
  let finish: ((backend: any) => void) | undefined
  let lateClosed = 0
  const opening = manager.create({ userId: 1, instanceId: 6, issuedAt: 100 }, () => new Promise(resolve => { finish = resolve }), async () => true)
  manager.closeMatching(() => true)
  await assert.rejects(opening, /aborted/)
  finish!({ close: () => lateClosed++ })
  await settle()
  assert.equal(lateClosed, 1, 'late backend must be cleaned after cancellation')
  await manager.drain()

  let releaseCleanup!: () => void
  let reopenCount = 0
  const pendingCleanup = new Promise<void>(resolve => { releaseCleanup = resolve })
  const fakeBackend = (close: () => Promise<void>) => ({
    control: { readyState: WebSocket.OPEN, on() {} }, close
  }) as any
  const first = await manager.create({ userId: 1, instanceId: 6, issuedAt: 100 },
    async () => fakeBackend(() => pendingCleanup), async () => true)
  const closing = manager.close(first)
  const reopening = manager.create({ userId: 1, instanceId: 6, issuedAt: 100 }, async () => {
    reopenCount++
    return fakeBackend(async () => {})
  }, async () => true)
  await settle()
  assert.equal(reopenCount, 0, 'rapid reconnect must wait for Incus cancellation, not only websocket closure')
  releaseCleanup()
  await closing
  const reopened = await reopening
  assert.equal(reopenCount, 1)
  await manager.close(reopened)
  let cleanupAttempts = 0
  let failCleanup = true
  const failing = await manager.create({ userId: 1, instanceId: 6, issuedAt: 100 }, async () => fakeBackend(async () => {
    cleanupAttempts++
    if (failCleanup) throw new Error('offline')
  }), async () => true)
  await manager.close(failing)
  await assert.rejects(manager.create({ userId: 1, instanceId: 6, issuedAt: 100 }, async () => {
    throw new Error('must not open before cleanup')
  }, async () => true), /offline/)
  failCleanup = false
  await manager.waitForCleanup(6)
  assert.equal(cleanupAttempts, 3, 'failed cleanup is retried before allowing reconnection')
  assert.ok(!JSON.stringify(logSerializers.req({ url: '/api/ws/instances/6/spice?ticket=private-console-capability', headers: { 'sec-websocket-protocol': 'binary, incudal-spice.private-console-capability' } })).includes('private-console-capability'))
  owner = 1
  assert.equal((await post()).statusCode, 200)
  await app.close()
  assert.equal(activeOperations.size, 0, 'server shutdown must await Incus console cancellation')
  const appSource = readFileSync(new URL('../src/app.ts', import.meta.url), 'utf8')
  const shutdown = appSource.slice(appSource.indexOf('const gracefulShutdown ='))
  assert.ok(shutdown.indexOf('await spiceSessions.drain()') < shutdown.indexOf('await fastify.close()'),
    'shutdown must cancel consoles before websocket preClose disconnects reverse tunnels')
  console.log('SPICE tests passed: mTLS/direct/tunnel, multi-channel operation reuse, binary handshake, permissions, origin, revocation, limits, early disconnect, cancellation and redaction')
} finally {
  await spiceSessions.drain()
  await app.close()
  await closeAllClients()
  hostTunnelManager.createDuplexStream = originalDuplex
  hostTunnelManager.isTunnelOnline = originalOnline
  for (const peer of peers) peer.terminate()
  wss.close()
  await new Promise<void>(resolve => fakeIncus.close(() => resolve()))
  rmSync(directory, { recursive: true, force: true })
}
