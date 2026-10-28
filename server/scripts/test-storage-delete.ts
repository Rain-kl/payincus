import assert from 'node:assert/strict'
import { prisma } from '../src/db/prisma.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { deleteManagedZfsPool } from '../src/lib/storage/delete.js'

let keys: any[]=[]
let record: any=null
let source='pool/incus'
let actualSource=source
let exists=true
let remoteFailure=false
let cleanupFailure=false
let transactionFailure=false
let changeKey=false
let deletes=0
let forgotten:string[]=[]
Object.assign(prisma.host,{findUnique:async()=>({id:4,url:'https://node.invalid'})})
Object.assign(prisma.nodeSecurityState,{findUnique:async()=>({serverFingerprint:'a'.repeat(64)})})
const pools={
 updateMany:async()=>({count:1}),
 findUnique:async()=>record,
 upsert:async({create,update}:any)=>{record=record?{...record,...update}:create;return record},
 deleteMany:async()=>{record=null;return {count:1}}
}
const keyDb={
 findUnique:async({where}:any)=>keys.find(k=>k.id===where.id)||null,
 findMany:async()=>keys.filter(k=>k.dataset===source||k.dataset.startsWith(source+'/')).map(({id,dataset,fingerprint})=>({id,dataset,fingerprint})),
 delete:async({where}:any)=>{keys=keys.filter(k=>k.id!==where.id)}
}
Object.assign(prisma.storagePool,pools)
Object.assign(prisma.nodeStorageKey,keyDb)
Object.assign(prisma,{$transaction:async(fn:any)=>{
 if(transactionFailure)throw new Error('fixture database unavailable')
 const snapshot=structuredClone({keys,record})
 try{return await fn({storagePool:pools,nodeStorageKey:keyDb})}catch(e){keys=snapshot.keys;record=snapshot.record;throw e}
}})
Object.assign(IncusClient.prototype,{
 connect:async function(this:IncusClient){assert.equal(this.options.serverFingerprint,'a'.repeat(64))},close:async()=>{},
 request:async(method:string,path:string,body?:any)=>{
  if(path==='/1.0/payincus')return {api_extensions:['payincus_storage_encryption_v1']}
  if(path==='/1.0/storage-pools')return exists?['/1.0/storage-pools/data']:[]
  if(path==='/1.0/storage-pools/data'&&method==='GET')return {driver:'zfs',config:{'zfs.pool_name':actualSource}}
  if(path==='/1.0/payincus/security')return {targets:[{dataset:source,guid:'123'},{dataset:'pool/other',guid:'456'}]}
  if(method==='DELETE'){
   assert.ok(record.config._payincusDeletion,'remote delete preceded durable checkpoint')
   deletes++;if(remoteFailure)throw new Error('fixture deletion refused')
   exists=false;return {}
  }
  if(path==='/1.0/payincus/storage/forget'){
   assert.equal(exists,false,'registration removed before remote deletion')
   assert.equal(body.dataset,source,'sibling or ancestor root was unregistered')
   assert.equal(body.guid,'123')
   if(cleanupFailure)throw new Error('fixture verification failure')
   forgotten.push(body.dataset)
   if(changeKey)keys[0].fingerprint='changed'
   return {}
  }
  throw new Error('unexpected endpoint')
 }
})
function reset(){
 source='pool/incus';actualSource=source;exists=true;remoteFailure=false;cleanupFailure=false;transactionFailure=false;changeKey=false;deletes=0;forgotten=[]
 keys=[{id:1,hostId:4,dataset:source,fingerprint:'fp1'},{id:2,hostId:4,dataset:'pool/other',fingerprint:'fp2'}]
 record={hostId:4,name:'data',driver:'zfs',purpose:'instance_data',config:{source}}
}
reset();remoteFailure=true
await assert.rejects(deleteManagedZfsPool(4,'data'))
assert.equal(keys.length,2);assert.equal(forgotten.length,0);assert.ok(record.config._payincusDeletion)
remoteFailure=false;await deleteManagedZfsPool(4,'data');assert.equal(keys.length,1);assert.equal(keys[0].dataset,'pool/other');assert.equal(record,null)
reset();cleanupFailure=true
await assert.rejects(deleteManagedZfsPool(4,'data'),/节点存储已删除/)
assert.equal(keys.length,2);assert.equal(exists,false);assert.ok(record)
cleanupFailure=false;await deleteManagedZfsPool(4,'data');assert.equal(deletes,1,'cleanup retry deleted twice');assert.equal(keys.length,1)
reset();transactionFailure=true
await assert.rejects(deleteManagedZfsPool(4,'data'));assert.equal(keys.length,2);assert.ok(record)
transactionFailure=false;await deleteManagedZfsPool(4,'data');assert.equal(deletes,1);assert.equal(record,null)
reset();remoteFailure=true;await assert.rejects(deleteManagedZfsPool(4,'data'))
remoteFailure=false;actualSource='other/new';await assert.rejects(deleteManagedZfsPool(4,'data'),/来源已变化/);assert.equal(deletes,1);assert.equal(keys.length,2)
reset();changeKey=true
await assert.rejects(deleteManagedZfsPool(4,'data'));assert.equal(keys[0].fingerprint,'changed');assert.ok(record)
reset();source='pool/private/child';actualSource=source;keys=[{id:3,hostId:4,dataset:'pool/private',fingerprint:'parent'}]
await deleteManagedZfsPool(4,'data');assert.equal(keys.length,1,'deleting child removed ancestor encryption key')
console.log('Storage deletion: checkpoint-before-delete, key retention on failure, retry without repeated disk deletion, sibling/ancestor preservation and concurrent-key protection passed')

reset();exists=false;record.config._payincusImport={source,encryptionRoot:source}
const removed=await deleteManagedZfsPool(4,'data');assert.match(removed.message,/外部数据集/);assert.equal(record,null);assert.equal(deletes,0);assert.equal(keys.length,2)
