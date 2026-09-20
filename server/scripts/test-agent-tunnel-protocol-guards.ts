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

await testTunnelManager()
console.log('tunnel protocol and manager guard tests passed')
