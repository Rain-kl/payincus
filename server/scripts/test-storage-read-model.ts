import assert from 'node:assert/strict'
import Fastify from 'fastify'
import { prisma } from '../src/db/prisma.js'
import hostsRoutes from '../src/routes/hosts.js'
import storageEncryptionRoutes from '../src/routes/storage-encryption.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { hostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import { HostTunnelManager } from '../src/lib/incus/tunnel-manager.js'
import { encodeFrame, decodeFrame, TunnelFrameType } from '../src/lib/incus/tunnel-protocol.js'
import { EventEmitter } from 'node:events'
const pool = { id: 1, hostId: 4, name: 'data', driver: 'zfs', purpose: 'instance_data', description: 'saved', config: {source:'tank/incus'}, status:'ready', totalBytes:100n*1024n**3n, usedBytes:10n*1024n**3n, reportedAt:new Date(), telemetry:{encryption:{encrypted:true,state:'unlocked',encryptionRoot:'tank/incus'}}, createdAt:new Date() }
Object.assign(prisma.host, { findUnique: async () => ({id:4,userId:10,name:'fixture',url:'https://node.invalid',status:'online',certPath:'/fixture/cert',keyPath:'/fixture/key',createdAt:new Date(),updatedAt:new Date()}) })
Object.assign(prisma.host, { findMany: async () => [{id:4,status:'online',agent:null,nodeSecurity:null}] })
Object.assign(prisma.hostAgent, { findUnique: async () => null })
Object.assign(prisma.storagePool, { findMany: async () => [pool], findUnique: async () => pool })
Object.assign(prisma.nodeStorageKey, { findUnique: async () => ({ id:1 }) })
Object.assign(IncusClient.prototype, { connect: async () => { throw new Error('GET attempted a node connection') }, request: async () => { throw new Error('GET attempted a node request') } })
let online = true, notifications = 0, userId = 10
hostTunnelManager.requestHeartbeat = () => { notifications++; return online }
const app = Fastify()
const auth = async (request: any) => { request.user = {id:userId,role:'user'} }
app.decorate('authenticate', auth); app.decorate('authenticateUser', auth); app.decorate('authenticateAdmin', auth)
await app.register(hostsRoutes, {prefix:'/api/hosts'})
await app.register(storageEncryptionRoutes)
try {
 const list = await app.inject('/api/hosts/4/storage-pools')
 assert.equal(list.statusCode,200,list.body)
 assert.equal(list.json().pools[0].space.total,100*1024**3)
 const encryption = await app.inject('/api/hosts/4/storage-pools/data/encryption')
 assert.equal(encryption.statusCode,200,encryption.body)
 assert.equal(encryption.json().keySaved,true)
 for (const action of ['verify','test','sync']) {
  const response = await app.inject({method:'POST',url:`/api/hosts/4/${action}`})
  assert.equal(response.statusCode,202,response.body)
 }
 assert.equal(notifications,3)
 userId=20
 assert.equal((await app.inject('/api/hosts/4/storage-pools')).statusCode,403)
 assert.equal((await app.inject({method:'POST',url:'/api/hosts/4/verify'})).statusCode,403)
 assert.equal(notifications,3)
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
socket.emit('message',encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['payincus_node_v1','heartbeat-trigger-v1']}))))
assert.equal(manager.requestHeartbeat(4),true)
assert.equal(decodeFrame(sent.at(-1)!).type,TunnelFrameType.HEARTBEAT)
socket.readyState=3
assert.equal(manager.requestHeartbeat(4),false)
console.log('Storage reads are database-only; verification requests heartbeat asynchronously with authorization/capability/offline checks')
