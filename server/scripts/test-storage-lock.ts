import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import Fastify from 'fastify'
import routes from '../src/routes/storage-encryption.js'
import { prisma } from '../src/db/prisma.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { fingerprint, seal } from '../src/lib/node-security/vault.js'

process.env.ENCRYPTION_KEY=randomBytes(48).toString('base64')
const key=randomBytes(32).toString('hex'),dataset='pool/incus'
const saved={id:1,hostId:4,dataset,fingerprint:fingerprint(key),version:1,ciphertext:seal(key,{hostId:4,dataset,version:1})}
let available=true,manual=false,complete=false,globalLocked=false,failAudit=false,failStart=false,moved=false
let revision=1,lockWrites=0
const identities=[{project:'default',name:'running',uuid:'one',frozen:false},{project:'default',name:'frozen',uuid:'two',frozen:true},{project:'default',name:'vm',uuid:'three',frozen:false},{project:'default',name:'suspended',uuid:'four',frozen:false}]
let pending=identities.map(i=>({...i}))
const statuses:Record<string,string>={running:'Stopped',frozen:'Stopped',vm:'Stopped',suspended:'Stopped'}
const starts:string[]=[]
Object.assign(prisma.storagePool, { findUnique: async () => null })
Object.assign(prisma.host,{findUnique:async()=>({id:4,userId:10,url:'https://node.invalid'})})
Object.assign(prisma.nodeSecurityState,{findUnique:async()=>({serverFingerprint:'a'.repeat(64)})})
Object.assign(prisma.nodeStorageKey,{findUnique:async()=>saved,upsert:async()=>saved,updateMany:async()=>({count:1})})
Object.assign(prisma.log,{create:async()=>{if(failAudit)throw new Error('audit unavailable');return {id:1}}})
Object.assign(prisma.instance,{findFirst:async({where}:any)=>({status:where.incusId==='suspended'?'suspended':'stopped',expiresAt:null})})
Object.assign(IncusClient.prototype,{
 connect:async()=>{},close:async()=>{},
 request:async(method:string,path:string,body?:any)=>{
  if(path==='/1.0/payincus')return {api_extensions:['payincus_storage_encryption_v1']}
  if(path==='/1.0/storage-pools/data')return {driver:'zfs',config:{'zfs.pool_name':dataset}}
  if(path==='/1.0/storage-pools/moved')return {driver:'zfs',config:{'zfs.pool_name':'other/incus'}}
  if(path==='/1.0/payincus/storage')return {datasets:[{dataset,encryptionRoot:dataset,algorithm:'aes-256-gcm',keyStatus:available?'available':'unavailable',keyFormat:'hex'}],locks:{[dataset]:{locked:manual,complete,resumeInstances:pending}}}
  if(path==='/1.0/payincus/security')return {locked:globalLocked,revision,targets:[{dataset,guid:'123',fingerprint:saved.fingerprint}],storageLocks:{[dataset]:{locked:manual,complete,resumeInstances:pending}}}
  if(path==='/1.0/payincus/storage/lock-pool'){
   assert.equal(body.dataset,dataset);lockWrites++;manual=true;complete=true;available=false;revision++;return {}
  }
  if(['/1.0/payincus/storage/unlock-pool','/1.0/payincus/storage/unlock'].includes(path)){
   assert.equal(body.key,key);assert.equal(body.dataset,dataset)
   if(manual)assert.equal(path,'/1.0/payincus/storage/unlock-pool')
   manual=false;complete=false;available=true;revision++;return {}
  }
  if(path==='/1.0/payincus/storage/resume'){
   assert.equal(body.dataset,dataset);assert.equal(body.revision,revision)
   pending=pending.filter(p=>p.uuid!==body.instance.uuid);revision++
   return {locked:false,revision,resumeInstances:pending}
  }
  const match=/^\/1\.0\/instances\/([^/?]+)(\/state)?\?project=default$/.exec(path)
  if(match){
   const name=match[1],item=identities.find(i=>i.name===name)!
   assert.ok(item,'recovery touched an instance outside the captured list')
   if(!match[2])return {type:name==='vm'?'virtual-machine':'container',config:{'volatile.uuid':item.uuid},expanded_devices:{root:{type:'disk',pool:moved?'moved':'data',path:'/'}}}
   if(method==='GET')return {status:statuses[name]}
   if(body.action==='start'){
    if(failStart && name==='running')throw new Error('fixture start failure')
    starts.push(name);statuses[name]='Running'
   }else if(body.action==='freeze'){statuses[name]='Frozen'}else throw new Error('unexpected action')
   return {}
  }
  throw new Error('unexpected endpoint')
 }
})
const app=Fastify({logger:false})
app.decorate('authenticate',async(req:any)=>{req.user={id:10,role:'user'}})
await app.register(routes)
const url='/api/hosts/4/storage-pools/data/encryption'
failAudit=true
assert.equal((await app.inject({method:'PUT',url,payload:{}})).statusCode,409)
assert.equal(lockWrites,0);assert.equal(available,true)
failAudit=false
const locked=await app.inject({method:'PUT',url,payload:{}})
assert.equal(locked.statusCode,200);assert.equal(locked.json().manualLocked,true);assert.equal(locked.json().state,'locked')
assert.equal(locked.body.includes(key),false)
failStart=true
const partial=await app.inject({method:'POST',url,payload:{}})
assert.equal(partial.statusCode,409);assert.match(partial.json().error,/磁盘已解锁.*尚未恢复/)
assert.deepEqual(pending.map(p=>p.name),['running'])
assert.deepEqual(starts.sort(),['frozen','vm']);assert.equal(statuses.frozen,'Frozen');assert.equal(statuses.suspended,'Stopped')
failStart=false
const restored=await app.inject({method:'POST',url,payload:{}})
assert.equal(restored.statusCode,200);assert.equal(restored.json().pendingRecovery,0)
assert.deepEqual(starts.sort(),['frozen','running','vm'])
pending=[{...identities[0]}];statuses.running='Stopped';moved=true
assert.equal((await app.inject({method:'POST',url,payload:{}})).statusCode,409)
assert.equal(pending.length,1);assert.equal(statuses.running,'Stopped')
moved=false;globalLocked=true
const nodeLock=await app.inject({method:'POST',url,payload:{}})
assert.equal(nodeLock.statusCode,423);assert.equal(statuses.running,'Stopped');assert.equal(globalLocked,true)
await app.close()
console.log('Storage lock/unlock: audit before stop, scoped exact-identity recovery, VM/frozen restoration, original stopped/suspended protection, retry and node-lock boundary passed')
