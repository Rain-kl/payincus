import assert from 'node:assert/strict'
import Fastify from 'fastify'
import { prisma } from '../src/db/prisma.js'
import hostsRoutes from '../src/routes/hosts.js'
import storageEncryptionRoutes from '../src/routes/storage-encryption.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { hostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import { HeartbeatTestError, HostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import { encodeFrame, decodeFrame, TunnelFrameType } from '../src/lib/incus/tunnel-protocol.js'
import { EventEmitter } from 'node:events'
const pool = { id: 1, hostId: 4, name: 'data', driver: 'zfs', purpose: 'instance_data', description: 'saved', config: {source:'tank/incus'}, status:'ready', totalBytes:100n*1024n**3n, usedBytes:10n*1024n**3n, reportedAt:new Date(Date.now() - 600_000), telemetry:{spaceAvailable:90*1024**3, encryption:{encrypted:true,state:'unlocked',encryptionRoot:'tank/incus'}}, createdAt:new Date() }
let hostStatus = 'online'
let tunnelOnline = true
let extraNodePool = false
let allowNodeCalls = false
const updatedStatuses: string[] = []
const agentRecord = { status: 'online', version: '2.0.1', lastSeenAt: new Date(), lastReport: { incus: { available: true }, instances: { available: true }, metrics: { heartbeatIntervalSeconds: 5 } } }
Object.assign(prisma.host, {
 findUnique: async () => ({ id:4, userId:10, name:'fixture', url:'https://node.invalid', status:hostStatus, tunnelEnabled:true, certPath:'/fixture/cert', keyPath:'/fixture/key', createdAt:new Date(), updatedAt:new Date(), agent: agentRecord, nodeSecurity: { serverFingerprint: 'a'.repeat(64), report: { enabled: false, locked: false, revision: 1 } }, storagePools: [pool] }),
 findMany: async () => [{ id:4, status:hostStatus, tunnelEnabled:true, agent: agentRecord, nodeSecurity: { report: { enabled: false, locked: false, revision: 1 } } }],
 update: async ({ data }: any) => { if (typeof data?.status === 'string') { hostStatus = data.status; updatedStatuses.push(data.status) } return { id:4, status:hostStatus } }
})
Object.assign(prisma, {
 $transaction: async (fn: any) => fn({
  $executeRaw: async () => 0,
  nodeSecurityState: { findUnique: async () => ({ report: { enabled: false, locked: false, revision: 1 } }), upsert: async () => ({}) },
  nodeCliEvent: { deleteMany: async () => ({ count: 0 }), createMany: async () => ({ count: 0 }) },
  log: { create: async () => ({}) }
 })
})
Object.assign(prisma.hostAgent, { findUnique: async () => null })
Object.assign(prisma.storagePool, { findMany: async () => [pool], findUnique: async () => pool })
Object.assign(prisma.nodeStorageKey, { findUnique: async () => ({ id:1 }) })
Object.assign(IncusClient.prototype, {
 connect: async () => { if (!allowNodeCalls) throw new Error('GET attempted a node connection') },
 close: async () => {},
 request: async (_method: string, path: string) => {
  if (!allowNodeCalls) throw new Error('GET attempted a node request')
  if (path === '/1.0/agent') return { api_extensions: ['incus_baseline_v1', 'incus_security_v1'] }
  if (path === '/1.0/agent/baseline') {
   const pools: any[] = [{ name: 'data', driver: 'zfs', status: 'ready', config: { source: 'tank/incus' } }]
   if (extraNodePool) pools.push({ name: 'extra', driver: 'dir', status: 'ready', config: { source: '/extra' } })
   return { nodeSecurity: { state: { enabled: false, locked: false, revision: 1 } }, storage: { available: true, complete: true, pools } }
  }
  throw new Error(`unexpected request ${path}`)
 }
})
let online = true, notifications = 0, userId = 10
hostTunnelManager.requestHeartbeat = () => { notifications++; return online }
hostTunnelManager.measureTunnelPing = async () => tunnelOnline ? { ok: true, latencyMs: 12 } : { ok: false, latencyMs: null, error: '反向隧道未连接' }
hostTunnelManager.isTunnelOnline = () => tunnelOnline
const app = Fastify()
const auth = async (request: any) => { request.user = {id:userId,role:'user'} }
app.decorate('authenticate', auth); app.decorate('authenticateUser', auth); app.decorate('authenticateAdmin', auth)
await app.register(hostsRoutes, {prefix:'/api/hosts'})
await app.register(storageEncryptionRoutes)
try {
 const list = await app.inject('/api/hosts/4/storage-pools')
 assert.equal(list.statusCode,200,list.body)
 assert.equal(list.json().pools[0].status,'ready','Option B must not expire ready status on 10m reportedAt')
 assert.equal(list.json().pools[0].space.total,100*1024**3)
 assert.equal(list.json().pools[0].space.available,90*1024**3)
 pool.status = 'missing'
 assert.equal((await app.inject('/api/hosts/4/storage-pools')).json().pools[0].status,'missing')
 pool.status = 'ready'
 const encryption = await app.inject('/api/hosts/4/storage-pools/data/encryption')
 assert.equal(encryption.statusCode,200,encryption.body)
 assert.equal(encryption.json().keySaved,true)
 for (const action of ['verify','sync']) {
  const response = await app.inject({method:'POST',url:`/api/hosts/4/${action}`})
  assert.equal(response.statusCode,202,response.body)
 }
 assert.equal(notifications,2)
 allowNodeCalls = true
 const tested = await app.inject({method:'POST',url:'/api/hosts/4/test'})
 assert.equal(tested.statusCode,200,tested.body)
 assert.equal(tested.json().queued,undefined)
 assert.equal(tested.json().success,true)
 assert.equal(tested.json().diagnostic.checks.reverseTunnel.ok,true)
 assert.equal(tested.json().diagnostic.checks.incusMtls.ok,true)
 assert.equal(tested.json().diagnostic.checks.storageBaseline.ok,true)
 extraNodePool = true
 const diagWarn = await app.inject({method:'POST',url:'/api/hosts/4/diagnose'})
 assert.equal(diagWarn.statusCode,200,diagWarn.body)
 assert.equal(diagWarn.json().needsBaselineSync,true)
 assert.equal(diagWarn.json().checks.storageBaseline.status,'warn')
 extraNodePool = false
 tunnelOnline = false
 const diagTunnelOnlyFail = await app.inject({method:'POST',url:'/api/hosts/4/diagnose'})
 assert.equal(diagTunnelOnlyFail.statusCode,200,diagTunnelOnlyFail.body)
 assert.equal(diagTunnelOnlyFail.json().success,false)
 assert.equal(diagTunnelOnlyFail.json().checks.reverseTunnel.ok,false)
 assert.equal(hostStatus,'online','must stay online while heartbeat is still healthy')
 agentRecord.lastSeenAt = new Date(Date.now() - 120_000)
 const diagFail = await app.inject({method:'POST',url:'/api/hosts/4/diagnose'})
 assert.equal(diagFail.statusCode,200,diagFail.body)
 assert.equal(diagFail.json().checks.agentHeartbeat.ok,false)
 assert.equal(hostStatus,'offline')
 agentRecord.lastSeenAt = new Date()
 tunnelOnline = true
 const diagRecover = await app.inject({method:'POST',url:'/api/hosts/4/diagnose'})
 assert.equal(diagRecover.statusCode,200,diagRecover.body)
 assert.equal(diagRecover.json().success,true)
 assert.equal(hostStatus,'online')
 assert.deepEqual(updatedStatuses,['offline','online'])

 userId=20
 assert.equal((await app.inject('/api/hosts/4/storage-pools')).statusCode,403)
 assert.equal((await app.inject({method:'POST',url:'/api/hosts/4/verify'})).statusCode,403)
 assert.equal(notifications,2)
 userId=10;online=false
 assert.equal((await app.inject({method:'POST',url:'/api/hosts/4/verify'})).statusCode,409)
 assert.equal((await app.inject({method:'POST',url:'/api/hosts/bad/verify'})).statusCode,400)
} finally { await app.close() }
const manager = new HostTunnelManager()
const sent: Buffer[]=[]
const socket=Object.assign(new EventEmitter(),{readyState:1,send:(frame:Buffer)=>sent.push(frame),close:()=>{}})
assert.equal(manager.requestHeartbeat(4),false)
manager.registerTunnel(4,socket)
assert.equal(manager.requestHeartbeat(4),false,'must not assume capability support')
socket.emit('message',encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['incus_node_v1','heartbeat-trigger-v1']}))))
assert.equal(manager.requestHeartbeat(4),true)
assert.equal(decodeFrame(sent.at(-1)!).type,TunnelFrameType.HEARTBEAT)
socket.readyState=3
assert.equal(manager.requestHeartbeat(4),false)
console.log('Storage reads are database-only; node test awaits confirmed reporting while verify/sync retain asynchronous behavior')

// A socket notification is never sufficient evidence for a successful test.
manager.registerTunnel(4,socket)
socket.readyState=1
socket.emit('message',encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['heartbeat-trigger-v1']}))))
await assert.rejects(manager.requestHeartbeatAndWait(4,50),/版本不支持/)
socket.emit('message',encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['heartbeat-confirm-v1']}))))
let finished=false
const confirmed=manager.requestHeartbeatAndWait(4,1000).then(()=>{finished=true})
const requestId=JSON.parse(decodeFrame(sent.at(-1)!).payload.toString()).requestId
manager.completeHeartbeat(5,requestId)
manager.completeHeartbeat(4,'unrelated')
await Promise.resolve()
assert.equal(finished,false,'unrelated/old heartbeats completed a test')
await assert.rejects(manager.requestHeartbeatAndWait(4,1000),/正在执行/)
manager.completeHeartbeat(4,requestId)
await confirmed
const failed=assert.rejects(manager.requestHeartbeatAndWait(4,1000),/保存失败/)
manager.completeHeartbeat(4,JSON.parse(decodeFrame(sent.at(-1)!).payload.toString()).requestId,'存储保存失败')
await failed
await assert.rejects(manager.requestHeartbeatAndWait(4,5),/上报超时/)
const disconnected=assert.rejects(manager.requestHeartbeatAndWait(4,1000),/连接已断开/)
manager.unregisterTunnel(4,socket)
await disconnected
console.log('Heartbeat confirmation requires the matching authenticated persisted report; busy, timeout and disconnect fail')
