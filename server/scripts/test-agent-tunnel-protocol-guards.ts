import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { encodeFrame, decodeFrame, TunnelFrameType } from '../src/lib/incus/tunnel-protocol.js'
import { HostTunnelManager, WebSocketLike } from '../src/lib/incus/tunnel-manager.js'

// --- Protocol Framing Tests ---
const payload = Buffer.from('hello-mTLS-bytes')
const frame = encodeFrame(TunnelFrameType.DATA, 101, payload)
assert.equal(frame.length, 5 + payload.length)
const decoded = decodeFrame(frame)
assert.equal(decoded.type, TunnelFrameType.DATA)
assert.equal(decoded.streamId, 101)
assert.deepEqual(decoded.payload, payload)

const openPayload = Buffer.from(JSON.stringify({ targetHost: '127.0.0.1', targetPort: 8443 }))
const openFrame = encodeFrame(TunnelFrameType.OPEN, 1, openPayload)
const decodedOpen = decodeFrame(openFrame)
assert.equal(decodedOpen.type, TunnelFrameType.OPEN)
assert.equal(decodedOpen.streamId, 1)
assert.deepEqual(JSON.parse(decodedOpen.payload.toString('utf8')), { targetHost: '127.0.0.1', targetPort: 8443 })

const closeFrame = encodeFrame(TunnelFrameType.CLOSE, 101)
assert.equal(closeFrame.length, 5)
const decodedClose = decodeFrame(closeFrame)
assert.equal(decodedClose.type, TunnelFrameType.CLOSE)
assert.equal(decodedClose.streamId, 101)
assert.equal(decodedClose.payload.length, 0)

const configPayload = Buffer.from(JSON.stringify({ targetHost: 'localhost', targetPort: 9443 }))
const configFrame = encodeFrame(TunnelFrameType.CONFIG, 0, configPayload)
const decodedConfig = decodeFrame(configFrame)
assert.equal(decodedConfig.type, TunnelFrameType.CONFIG)
assert.equal(decodedConfig.streamId, 0)

assert.throws(() => decodeFrame(Buffer.from([0x01, 0x00])), /Frame too short/)

// --- HostTunnelManager Integration Tests ---
class MockWebSocket extends EventEmitter implements WebSocketLike {
  readyState = 1 // OPEN
  sentFrames: Buffer[] = []

  send(data: any, cb?: (err?: Error) => void): void {
    this.sentFrames.push(Buffer.isBuffer(data) ? data : Buffer.from(data))
    if (cb) process.nextTick(cb)
  }

  close(code?: number, reason?: string): void {
    this.readyState = 3 // CLOSED
    this.emit('close', code, reason)
  }

  simulateIncoming(buf: Buffer): void {
    this.emit('message', buf)
  }
}

async function testTunnelManager() {
  const manager = new HostTunnelManager()
  const mockWs = new MockWebSocket()
  const hostId = 777

  assert.equal(manager.isTunnelOnline(hostId), false)
  manager.registerTunnel(hostId, mockWs)
  assert.equal(manager.isTunnelOnline(hostId), true)

  assert.equal(decodeFrame(mockWs.sentFrames[0]).type, TunnelFrameType.CONTROL_HELLO)
  mockWs.sentFrames = []

  // 1. Broadcast config
  const broadcastOk = manager.broadcastConfig(hostId, { targetHost: '127.0.0.1', targetPort: 8443 })
  assert.equal(broadcastOk, true)
  assert.equal(mockWs.sentFrames.length, 1)
  const sentConfigFrame = decodeFrame(mockWs.sentFrames[0])
  assert.equal(sentConfigFrame.type, TunnelFrameType.CONFIG)
  assert.equal(sentConfigFrame.streamId, 0)

  // 2. Create Duplex Stream
  const duplex = manager.createDuplexStream(hostId, '127.0.0.1', 8443)
  assert.ok(duplex, 'duplex stream created')
  assert.equal(mockWs.sentFrames.length, 2)
  const sentOpenFrame = decodeFrame(mockWs.sentFrames[1])
  assert.equal(sentOpenFrame.type, TunnelFrameType.OPEN)
  const streamId = sentOpenFrame.streamId
  assert.ok(streamId > 0)

  // 3. Write data into Duplex stream -> should be framed into mockWs
  await new Promise<void>((resolve, reject) => {
    duplex.write(Buffer.from('client-request-payload'), (err) => {
      if (err) return reject(err)
      resolve()
    })
  })
  assert.equal(mockWs.sentFrames.length, 3)
  const sentDataFrame = decodeFrame(mockWs.sentFrames[2])
  assert.equal(sentDataFrame.type, TunnelFrameType.DATA)
  assert.equal(sentDataFrame.streamId, streamId)
  assert.equal(sentDataFrame.payload.toString('utf8'), 'client-request-payload')

  // 4. Simulate remote Agent sending response DATA frame
  const agentResponsePayload = Buffer.from('agent-response-payload')
  const incomingFrame = encodeFrame(TunnelFrameType.DATA, streamId, agentResponsePayload)
  
  const receivedChunks: Buffer[] = []
  duplex.on('data', (chunk) => {
    receivedChunks.push(chunk)
  })

  mockWs.simulateIncoming(incomingFrame)
  await new Promise((r) => setTimeout(r, 10))
  assert.equal(Buffer.concat(receivedChunks).toString('utf8'), 'agent-response-payload')

  // 5. Close duplex stream -> should send CLOSE frame
  await new Promise<void>((resolve) => {
    duplex.end(() => {
      resolve()
    })
  })
  assert.equal(mockWs.sentFrames.length, 4)
  const sentCloseFrame = decodeFrame(mockWs.sentFrames[3])
  assert.equal(sentCloseFrame.type, TunnelFrameType.CLOSE)
  assert.equal(sentCloseFrame.streamId, streamId)

  // 6. Remote agent closes stream
  const incomingCloseFrame = encodeFrame(TunnelFrameType.CLOSE, streamId)
  mockWs.simulateIncoming(incomingCloseFrame)
  await new Promise((r) => setTimeout(r, 10))

  // 7. WebSocket disconnection
  mockWs.close()
  assert.equal(manager.isTunnelOnline(hostId), false)
}

// Retired business-RPC advertisements cannot enable node control.
{
 const manager=new HostTunnelManager()
 const ws=new MockWebSocket()
 manager.registerTunnel(1,ws)
 ws.simulateIncoming(encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['network.apply','proxy.probe']}))))
 assert.equal(manager.supportsControl(1,'network.apply'),false)
 await assert.rejects(manager.requestControl(1,'network.apply',{},50),{code:'UPGRADE_REQUIRED'})
 ws.close()
}
await testTunnelManager()
console.log('tunnel protocol and manager guard tests passed')

// New transport-only nodes use native Incus extension endpoints, preserving JSON unchanged.
{
  const requests: Array<{ hostId: number; method: string; params: unknown; signal: AbortSignal }> = []
  const native = new HostTunnelManager(async (hostId, method, params, signal) => {
    requests.push({ hostId, method, params, signal })
    return { appliedRevision: 7 }
  })
  const socket = new MockWebSocket()
  native.registerTunnel(8101, socket)
  socket.emit('message', encodeFrame(TunnelFrameType.CONTROL_HELLO, 0, Buffer.from(JSON.stringify({ capabilities: ['payincus_node_v1'] }))))
  assert.equal(native.supportsControl(8101, 'network.apply.v2'), true)
  const input = { schemaVersion: 2, revision: 7 }
  assert.deepEqual(await native.requestControl(8101, 'network.apply.v2', input, 1000), { appliedRevision: 7 })
  assert.equal(requests[0].hostId, 8101)
  assert.deepEqual(requests[0].params, input)
  assert.equal(socket.sentFrames.some(frame => Number(decodeFrame(frame).type) === 0x08), false)
  native.unregisterTunnel(8101)
}
{
  let requestSignal: AbortSignal | undefined
  const native = new HostTunnelManager(async (_host, _method, _params, signal) => {
    requestSignal = signal
    return new Promise(() => {})
  })
  const socket = new MockWebSocket()
  native.registerTunnel(8102, socket)
  socket.emit('message', encodeFrame(TunnelFrameType.CONTROL_HELLO, 0, Buffer.from(JSON.stringify({ capabilities: ['payincus_node_v1'] }))))
  const request = native.requestControl(8102, 'proxy.probe', {}, 1000)
  const rejected = assert.rejects(request, { code: 'DISCONNECTED' })
  native.unregisterTunnel(8102)
  await rejected
  assert.equal(requestSignal?.aborted, true)
}
{
  const native = new HostTunnelManager(async () => { throw Object.assign(new Error('busy'), { statusCode: 409 }) })
  const socket = new MockWebSocket()
  native.registerTunnel(8103, socket)
  socket.emit('message', encodeFrame(TunnelFrameType.CONTROL_HELLO, 0, Buffer.from(JSON.stringify({ capabilities: ['payincus_node_v1'] }))))
  await assert.rejects(native.requestControl(8103, 'network.apply', {}, 1000), { code: 'BUSY' })
  native.unregisterTunnel(8103)
}
console.log('native Incus node API routing and disconnect cancellation passed')

// Native controls retain bounded concurrency, timeout, cancellation and socket replacement cleanup.
{
 const manager=new HostTunnelManager(async (_host,_method,_params,signal)=>new Promise((_resolve,reject)=>signal.addEventListener('abort',()=>reject(new Error('cancelled')))))
 const ws=new MockWebSocket()
 manager.registerTunnel(88,ws)
 ws.simulateIncoming(encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['payincus_node_v1']}))))
 await assert.rejects(manager.requestControl(88,'proxy.probe',{},5),{code:'TIMEOUT'})
 const abort=new AbortController()
 const pending=manager.requestControl(88,'proxy.probe',{},1000,abort.signal)
 abort.abort();await assert.rejects(pending,{code:'CANCELLED'})
 const calls=Array.from({length:4},()=>manager.requestControl(88,'proxy.probe',{},1000))
 const settled=Promise.allSettled(calls)
 await assert.rejects(manager.requestControl(88,'proxy.probe',{},1000),{code:'BUSY'})
 manager.registerTunnel(88,new MockWebSocket())
 for(const result of await settled){assert.equal(result.status,'rejected');if(result.status==='rejected')assert.equal(result.reason.code,'DISCONNECTED')}
 assert.equal((manager as any).pendingControls.size,0)
 manager.unregisterTunnel(88)
}
