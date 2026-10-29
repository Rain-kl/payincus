import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { prisma, closePrismaDatabase } from '../src/db/prisma.js'
import { processAgentStorageReport } from '../src/services/agent-storage-report.js'
import { hasStoragePool, updateStoragePool } from '../src/db/storage-pools.js'
import { getStorageCapacity, getStorageOverview } from '../src/lib/storage-capacity.js'
const target = new URL(process.env.DATABASE_URL || '')
assert.ok(['127.0.0.1','localhost'].includes(target.hostname))
assert.ok(target.pathname.endsWith('_test'), 'Use a disposable local database ending in _test')
const owner=await prisma.user.create({data:{username:`storage-report-${randomUUID()}`,passwordHash:'fixture'}})
const MiB=1024**2
try {
 // Generating an install command must not revoke a running connector.
 const {issueHostAgentInstallToken,consumeHostAgentInstallToken,rotateHostAgentCredentials}=await import('../src/lib/host-agent-credentials.js')
 const enrollment=await prisma.host.create({data:{userId:owner.id,name:'install-token-lifecycle',url:'https://node.invalid'}})
 const first=await rotateHostAgentCredentials(enrollment.id)
 const retainedReport={incus:{available:true},metrics:{heartbeatIntervalSeconds:10}}
 const seenAt=new Date()
 await prisma.hostAgent.update({where:{hostId:enrollment.id},data:{status:'online',lastSeenAt:seenAt,lastReport:retainedReport}})
 const token1=await issueHostAgentInstallToken(enrollment.id)
 const preserved=await prisma.hostAgent.findUniqueOrThrow({where:{hostId:enrollment.id}})
 assert.equal(preserved.agentId,first.agentId,'install command revoked active Agent ID')
 assert.equal(preserved.secretHash,first.agent.secretHash,'install command revoked active secret')
 assert.equal(preserved.status,'online')
 assert.equal(preserved.lastSeenAt?.getTime(),seenAt.getTime())
 assert.deepEqual(preserved.lastReport,retainedReport)
 const token2=await issueHostAgentInstallToken(enrollment.id)
 await assert.rejects(consumeHostAgentInstallToken(token1.installToken),/AGENT_INSTALL_TOKEN_INVALID/)
 const claims=await Promise.allSettled([consumeHostAgentInstallToken(token2.installToken),consumeHostAgentInstallToken(token2.installToken)])
 assert.equal(claims.filter(r=>r.status==='fulfilled').length,1,'token was consumed more than once')
 const claimed=claims.find(r=>r.status==='fulfilled') as PromiseFulfilledResult<Awaited<ReturnType<typeof consumeHostAgentInstallToken>>>
 assert.equal(claimed.value.agent.agentId,first.agentId)
 assert.equal(claimed.value.agentSecret===first.agentSecret,true)
 await prisma.hostAgent.update({where:{hostId:enrollment.id},data:{enabled:false}})
 await issueHostAgentInstallToken(enrollment.id,true)
 assert.equal((await prisma.hostAgent.findUniqueOrThrow({where:{hostId:enrollment.id}})).enabled,false,'command generation silently enabled a disabled agent')
 const rotated=await rotateHostAgentCredentials(enrollment.id,true)
 assert.notEqual(rotated.agentId,first.agentId,'explicit revocation did not rotate identity')
 assert.equal(rotated.agent.enabled,true)
 const host=await prisma.host.create({data:{userId:owner.id,name:'storage-report',url:'https://node.invalid',status:'online'}})
 let pool=await prisma.storagePool.create({data:{hostId:host.id,name:'data',driver:'dir',purpose:'instance_data',createdAt:new Date(Date.now()-10000)}})
 const now=new Date()
 const sample=(at:Date,total=100*1024*MiB)=>({available:true,complete:true,reportedAt:at.toISOString(),pools:[{name:'data',driver:'dir',status:'ready',config:{source:'/data'},space:{total,used:10*MiB},usedBy:0,filesystemId:'diskA'}]})
 await processAgentStorageReport(host.id,sample(now),now)
 const stored=()=>prisma.storagePool.findUniqueOrThrow({where:{id:pool.id}})
 assert.equal((await stored()).totalBytes,100n*1024n*BigInt(MiB))
 assert.equal((await getStorageCapacity(host.id,{requiredMB:1024})).reason,null)
 assert.equal(await processAgentStorageReport(host.id,sample(new Date(now.getTime()-1000),1*MiB),now),false)
 assert.equal((await stored()).totalBytes,100n*1024n*BigInt(MiB),'old heartbeat replaced capacity')
 const failedAt=new Date(now.getTime()+1000)
 await processAgentStorageReport(host.id,{available:false,reportedAt:failedAt.toISOString()},failedAt)
 assert.equal((await stored()).status,'unavailable')
 assert.equal((await getStorageOverview(host.id)).totalMB,102400,'failed inventory erased last known capacity')
 assert.equal((await getStorageCapacity(host.id)).reason,'storage_unavailable')
 const next=new Date(now.getTime()+2000)
 await processAgentStorageReport(host.id,sample(next),next)
 assert.equal((await stored()).status,'ready')
 const invalid=sample(new Date(now.getTime()+3000));invalid.pools[0].space.used=-1
 await processAgentStorageReport(host.id,invalid,new Date(now.getTime()+3000))
 assert.equal((await stored()).totalBytes,100n*1024n*BigInt(MiB),'invalid space erased capacity')
 assert.equal((await stored()).status,'unavailable')
 const missingAt=new Date(now.getTime()+4000)
 await processAgentStorageReport(host.id,{available:true,complete:true,reportedAt:missingAt.toISOString(),pools:[]},missingAt)
 assert.equal((await stored()).status,'missing')
 await prisma.storagePool.delete({where:{id:pool.id}})
 await processAgentStorageReport(host.id,sample(new Date(now.getTime()+5000)),new Date(now.getTime()+5000))
 assert.equal(await prisma.storagePool.count({where:{hostId:host.id}}),0,'heartbeat resurrected deleted record')
 pool=await prisma.storagePool.create({data:{hostId:host.id,name:'data',driver:'dir',purpose:'instance_data',createdAt:new Date(now.getTime()+6000)}})
 await processAgentStorageReport(host.id,sample(new Date(now.getTime()+5000)),new Date(now.getTime()+7000))
 assert.equal((await stored()).status,'pending','previous incarnation heartbeat overwrote recreated pool')
 await processAgentStorageReport(host.id,sample(new Date(now.getTime()+7000),200*1024*MiB),new Date(now.getTime()+7000))
 assert.equal((await stored()).totalBytes,200n*1024n*BigInt(MiB))
 const pkg=await prisma.package.create({data:{userId:owner.id,name:'bound',cpuMax:100,memoryMax:1024,diskMax:102400,packageHosts:{create:{hostId:host.id,storagePoolName:'data'}}}})
 await prisma.storagePool.create({data:{hostId:host.id,name:'huge',driver:'dir',purpose:'instance_data',status:'ready',reportedAt:now,totalBytes:1000n*1024n*BigInt(MiB),telemetry:{filesystemId:'diskB'}}})
 assert.equal((await getStorageCapacity(host.id,{packageId:pkg.id,requiredMB:300*1024})).reason,'disk_full','bound pool spilled into a larger pool')
 await prisma.instance.create({data:{userId:owner.id,hostId:host.id,incusId:'other-disk',name:'other-disk',image:'fixture',status:'running',cpu:100,memory:1024,disk:950*1024,storagePoolName:'huge'}})
 assert.equal((await getStorageCapacity(host.id,{packageId:pkg.id,requiredMB:150*1024})).reason,null,'unrelated disk reservations reduced bound pool capacity')
 assert.equal(await hasStoragePool(host.id,'default'),false,'default bypassed database registration')
 await updateStoragePool(host.id,'data',{purpose:'instance_storage'})
 assert.equal((await getStorageCapacity(host.id,{packageId:pkg.id})).reason,'storage_pool_missing','pool purpose was not saved')
 await updateStoragePool(host.id,'data',{purpose:'instance_data'})
 for(const name of ['sparse-a','sparse-b']) await prisma.storagePool.create({data:{hostId:host.id,name,driver:'zfs',config:{source:`${name}/incus`},purpose:'instance_data',status:'ready',reportedAt:now,totalBytes:100n*1024n*BigInt(MiB),telemetry:{backing:{id:'shared-root',sizeBytes:100*1024*MiB,availableBytes:20*1024*MiB}}}})
 await prisma.instance.create({data:{userId:owner.id,hostId:host.id,incusId:'sparse-reserved',name:'sparse-reserved',image:'fixture',status:'creating',cpu:100,memory:1024,disk:15*1024,storagePoolName:'sparse-b'}})
 assert.equal((await getStorageCapacity(host.id,{poolName:'sparse-a',requiredMB:10*1024})).reason,'disk_full','sparse vdevs promised their shared backing space twice')
 await processAgentStorageReport(host.id,{available:true,complete:false,reportedAt:new Date().toISOString(),pools:[{name:'sparse-a',driver:'zfs',config:{source:'replacement/incus'},status:'ready',space:{total:200*1024*MiB,used:0},encryption:{encrypted:true,state:'unlocked'}}]})
 assert.equal((await getStorageCapacity(host.id,{poolName:'sparse-a'})).reason,'storage_unavailable','changed dataset identity remained allocatable')
 await prisma.host.update({where:{id:host.id},data:{status:'offline'}})
 assert.equal((await getStorageCapacity(host.id,{packageId:pkg.id})).reason,'storage_unavailable')
 assert.equal((await getStorageOverview(host.id)).totalMB,1400*1024,'offline host erased pool totals')
 // Exercise the real signed heartbeat route, including enrollment completion and retained disk inventory.
 const {default:Fastify}=await import('fastify')
 const {default:agentRoutes,caddyUninstallRequested}=await import('../src/routes/agent.js')
 const {generateAgentId,generateAgentSecret,createAgentBodyHash,createAgentSignature}=await import('../src/lib/agent-auth.js')
 const {encryptSensitiveData}=await import('../src/lib/security.js')
 const {createHash}=await import('node:crypto')
 const live=await prisma.host.create({data:{userId:owner.id,name:'signed-heartbeat',url:'https://node.invalid',status:'offline'}})
 const signedPool=await prisma.storagePool.create({data:{hostId:live.id,name:'data',driver:'dir',purpose:'instance_data',createdAt:new Date(Date.now()-10000)}})
 const agentId=generateAgentId(), secret=generateAgentSecret()
 await prisma.hostAgent.create({data:{hostId:live.id,agentId,secretHash:createHash('sha256').update(secret).digest('hex'),secretEncrypted:encryptSensitiveData(secret)}})
 const app=Fastify()
 app.decorate('authenticate',async()=>{})
 app.decorate('authenticateUser',async()=>{})
 app.decorate('authenticateAdmin',async()=>{})
 await app.register(agentRoutes,{prefix:'/api/agent'})
 const send=(body:any)=>{
  const timestamp=String(Date.now()),nonce=randomUUID(),bodyHash=createAgentBodyHash(body)
  return app.inject({method:'POST',url:'/api/agent/heartbeat',payload:body,headers:{'x-incudal-agent-id':agentId,'x-incudal-timestamp':timestamp,'x-incudal-nonce':nonce,'x-incudal-body-sha256':bodyHash,'x-incudal-signature':createAgentSignature(secret,{method:'POST',path:'/api/agent/heartbeat',timestamp,nonce,bodyHash})}})
 }
 try {
  const oldId=generateAgentId()
  const timestamp=String(Date.now()), nonce=randomUUID(), unknownBody={nodeApi:'payincus_node_v1'}, hash=createAgentBodyHash(unknownBody)
  const unknown=await app.inject({method:'POST',url:'/api/agent/heartbeat',payload:unknownBody,headers:{'x-incudal-agent-id':oldId,'x-incudal-timestamp':timestamp,'x-incudal-nonce':nonce,'x-incudal-body-sha256':hash,'x-incudal-signature':createAgentSignature(secret,{method:'POST',path:'/api/agent/heartbeat',timestamp,nonce,bodyHash:hash})}})
  assert.equal(unknown.statusCode,401)
  assert.equal(unknown.json().code,'AGENT_NOT_REGISTERED')
  await prisma.hostAgent.update({where:{hostId:live.id},data:{enabled:false}})
  const disabled=await send({nodeApi:'payincus_node_v1'})
  assert.equal(disabled.statusCode,401)
  assert.equal(disabled.json().code,'AGENT_DISABLED')
  await prisma.hostAgent.update({where:{hostId:live.id},data:{enabled:true}})
  const payload={nodeApi:'payincus_node_v1',capabilities:['network-control'],runtime:{goarch:'arm64'},incus:{available:true},instances:{available:true,items:[]},metrics:{heartbeatIntervalSeconds:10},resources:{storageAvailable:true,storage:{disks:[{id:'disk0',size:100*1024*MiB}]}},storage:sample(new Date())}
  const result=await send(payload)
  assert.equal(result.statusCode,200,result.body)
  assert.equal((await prisma.storagePool.findUniqueOrThrow({where:{id:signedPool.id}})).status,'ready')
  const installed=await prisma.host.findUniqueOrThrow({where:{id:live.id}})
  assert.equal(installed.isInstalled,true)
  assert.equal(installed.architecture,'aarch64')
  assert.ok(installed.certPath&&installed.keyPath)
  const lost=await send({...payload,resources:{storageAvailable:false},storage:{available:false,reportedAt:new Date().toISOString()}})
  assert.equal(lost.statusCode,200,lost.body)
  const report=(await prisma.hostAgent.findUniqueOrThrow({where:{hostId:live.id}})).lastReport as any
  assert.equal(report.resources.storageAvailable,false)
  assert.equal(report.resources.storage.disks[0].id,'disk0','temporary sampling failure erased disk inventory')
  assert.equal((await prisma.storagePool.findUniqueOrThrow({where:{id:signedPool.id}})).totalBytes,100n*1024n*BigInt(MiB))
  await prisma.host.update({where:{id:live.id},data:{caddyEnabled:true}})
  caddyUninstallRequested.add(live.id)
  const daemonDown=await send({...payload,incus:{available:false},instances:{available:false},resources:{storageAvailable:false},storage:{available:false,reportedAt:new Date().toISOString()}})
  assert.equal(daemonDown.statusCode,200,daemonDown.body)
  const {loadHostServiceStatuses}=await import('../src/lib/host-service-status.js')
  assert.equal((await loadHostServiceStatuses([live.id])).get(live.id)?.available,false)
  assert.equal((await prisma.host.findUniqueOrThrow({where:{id:live.id}})).caddyEnabled,true,'absent telemetry was treated as successful uninstall')
  caddyUninstallRequested.delete(live.id)
  const {hostTunnelManager}=await import('../src/lib/incus/tunnel-manager.js')
  const {encodeFrame,decodeFrame,TunnelFrameType}=await import('../src/lib/incus/tunnel-protocol.js')
  const {EventEmitter}=await import('node:events')
  const {storedPoolEncryption}=await import('../src/lib/storage/read-model.js')
  const zfs=await prisma.storagePool.create({data:{hostId:live.id,name:'default',driver:'zfs',config:{source:'default/incus'},status:'locked',createdAt:new Date(Date.now()-10000)}})
  const frames:Buffer[]=[]
  const socket=Object.assign(new EventEmitter(),{readyState:1,send:(frame:Buffer)=>frames.push(frame),close:()=>{}})
  hostTunnelManager.registerTunnel(live.id,socket)
  socket.emit('message',encodeFrame(TunnelFrameType.CONTROL_HELLO,0,Buffer.from(JSON.stringify({capabilities:['heartbeat-confirm-v1']}))))
  try {
   let completed=false
   const confirmed=hostTunnelManager.requestHeartbeatAndWait(live.id,1000).then(()=>{completed=true})
   const requestId=JSON.parse(decodeFrame(frames.at(-1)!).payload.toString()).requestId
   const storage={available:true,complete:true,collectionComplete:true,reportedAt:new Date().toISOString(),pools:[{name:'data',driver:'dir',status:'locked',config:{source:'/data'}},{name:'default',driver:'zfs',status:'locked',config:{source:'default/incus'},encryption:{encrypted:true,nodeLocked:true,state:'locked',source:'default/incus',encryptionRoot:'default/incus',keyFormat:'hex',manualLocked:false}}]}
   await send({nodeApi:'payincus_node_v1',nodeSecurity:{state:{locked:true}},storage})
   assert.equal(completed,false,'unrelated periodic heartbeat completed a node test')
   const lockedReport=await send({heartbeatRequestId:requestId,nodeApi:'payincus_node_v1',nodeSecurity:{state:{locked:true}},storage:{...storage,reportedAt:new Date().toISOString()}})
   assert.equal(lockedReport.statusCode,200,lockedReport.body)
   await confirmed
   const encryption=await storedPoolEncryption(live.id,'default')
   assert.equal(encryption.encrypted,true)
   assert.equal(encryption.state,'locked')
   assert.equal(encryption.encryptionRoot,'default/incus')
   assert.equal(encryption.nodeLocked,true)
   assert.equal((await prisma.storagePool.findUniqueOrThrow({where:{id:zfs.id}})).status,'locked')
   const failed=assert.rejects(hostTunnelManager.requestHeartbeatAndWait(live.id,1000),/采集|保存/)
   const failedId=JSON.parse(decodeFrame(frames.at(-1)!).payload.toString()).requestId
   await send({heartbeatRequestId:failedId,nodeApi:'payincus_node_v1',incus:{available:false},storage:{available:false,reportedAt:new Date().toISOString()}})
   await failed
   for (const kind of ['partial','stale','instances']) {
    const refused=assert.rejects(hostTunnelManager.requestHeartbeatAndWait(live.id,1000),/采集|保存/)
    const id=JSON.parse(decodeFrame(frames.at(-1)!).payload.toString()).requestId
    const at=kind==='stale' ? (await prisma.storagePool.findUniqueOrThrow({where:{id:zfs.id}})).reportedAt!.toISOString() : new Date().toISOString()
    await send({heartbeatRequestId:id,nodeApi:'payincus_node_v1',...(kind==='instances'?{incus:{available:true},instances:{available:false},resources:{storageAvailable:true}}:{nodeSecurity:{state:{locked:true}}}),storage:{...storage,collectionComplete:kind!=='partial',reportedAt:at}})
    await refused
   }
   // Persist failure must reject the test, even though the agent record was already updated.
   const original=prisma.storagePool.updateMany
   const dbFailed=assert.rejects(hostTunnelManager.requestHeartbeatAndWait(live.id,1000),/未能保存/)
   const dbId=JSON.parse(decodeFrame(frames.at(-1)!).payload.toString()).requestId
   try {
    prisma.storagePool.updateMany=async()=>{throw new Error('fixture storage persistence failed')}
    const bad=await send({heartbeatRequestId:dbId,nodeApi:'payincus_node_v1',incus:{available:true},storage:{...storage,reportedAt:new Date().toISOString()}})
    assert.equal(bad.statusCode,500)
    await dbFailed
   } finally {prisma.storagePool.updateMany=original}
  } finally {hostTunnelManager.unregisterTunnel(live.id,socket)}
  const beforeLegacy=await prisma.storagePool.findUniqueOrThrow({where:{id:signedPool.id}})
  const locked=await send({nodeApi:'payincus_node_v1',nodeSecurity:{state:{locked:true}}})
  assert.equal(locked.statusCode,200,locked.body)
  assert.equal((await prisma.storagePool.findUniqueOrThrow({where:{id:signedPool.id}})).status,beforeLegacy.status,'unversioned legacy lock heartbeat overwrote a newer storage sample')
 } finally { await app.close() }
 console.log('Storage heartbeat DB integration passed: ordered reports, failures, deletion/recreation, retained totals, strict package binding and offline rejection')
} finally {
 await prisma.instance.deleteMany({where:{userId:owner.id}})
 await prisma.package.deleteMany({where:{userId:owner.id}})
 await prisma.host.deleteMany({where:{userId:owner.id}})
 await prisma.user.delete({where:{id:owner.id}})
 await closePrismaDatabase()
}
