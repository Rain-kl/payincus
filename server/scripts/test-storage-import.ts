import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { prisma } from '../src/db/prisma.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { fingerprint } from '../src/lib/node-security/vault.js'
import { importEncryptedPool, nodeForPendingPool, unlockAndBindPool } from '../src/lib/storage/import.js'
import { poolEncryption, type StorageNode } from '../src/lib/storage/encryption.js'
import { autoLoadStoragePool } from '../src/services/storage-encryption-loader.js'
process.env.ENCRYPTION_KEY=randomBytes(48).toString('base64')
const key=randomBytes(32).toString('hex')
let record:any=null,saved:any=null,source='disk/incus',bound=false,loaded=false,registered=false,manual=false,globalLocked=false,failBind=false,changed=false,automaticWrites=0
const inventory=()=>[{dataset:'disk',encryptionRoot:'-',algorithm:'off',keyFormat:'-',keyStatus:'-'},{dataset:source,encryptionRoot:source,algorithm:'aes-256-gcm',keyFormat:'hex',keyStatus:loaded?'available':'unavailable'}]
Object.assign(prisma.host,{findUnique:async()=>({id:4,userId:10,url:'https://node.invalid'})})
Object.assign(prisma.nodeSecurityState,{findUnique:async()=>({serverFingerprint:'a'.repeat(64)})})
Object.assign(prisma.storagePool,{
 updateMany:async()=>({count:1}),
 findUnique:async()=>record,
 upsert:async({create}:any)=>{record??={id:1,...create};return record},
 update:async({where,data}:any)=>{assert.equal(where.id,record.id);Object.assign(record,data);return record}
})
Object.assign(prisma.nodeStorageKey,{
 findUnique:async()=>saved,
 upsert:async({create}:any)=>{saved??=create;return saved},
 updateMany:async({data}:any)=>{saved.ciphertext=data.ciphertext;return {count:1}}
})
const node:StorageNode={request:async<T>(method:string,path:string,body?:any):Promise<T>=>{
 let value:any
 if(path==='/1.0/payincus')value={api_extensions:['payincus_storage_encryption_v1']}
 else if(path==='/1.0/storage-pools'&&method==='GET')value=bound?['/1.0/storage-pools/data']:[]
 else if(path==='/1.0/storage-pools/data')value={driver:'zfs',config:{'zfs.pool_name':changed?'other/root':source}}
 else if(path==='/1.0/payincus/storage')value={datasets:inventory(),locks:{[source]:{locked:manual,complete:manual,resumeInstances:[]}}}
 else if(path==='/1.0/payincus/security')value={locked:globalLocked,targets:registered?[{dataset:source,fingerprint:fingerprint(key)}]:[]}
 else if(['/1.0/payincus/storage/register','/1.0/payincus/storage/unlock','/1.0/payincus/storage/auto-unlock'].includes(path)){
  if(body.key!==key)throw new Error('fixture wrong key')
  assert.equal(body.dataset,source)
  if(path.endsWith('/auto-unlock')){automaticWrites++;assert.equal(manual,false);assert.equal(globalLocked,false)}
  loaded=true;registered=true;value={}
 }else if(path==='/1.0/storage-pools'&&method==='POST'){
  assert.equal(loaded,true);assert.ok(saved,'native bind preceded verified custody')
  assert.equal(body.config.source,source);assert.equal(JSON.stringify(body).includes(key),false)
  if(failBind)throw new Error('fixture bind failed')
  bound=true;value={}
 }else throw new Error('unexpected request')
 return value as T
}}
Object.assign(IncusClient.prototype,{connect:async()=>{},close:async()=>{},request:node.request})
const vault={find:async()=>saved,save:async(r:any)=>{saved=r}}
const imported=await importEncryptedPool(4,{name:'data',source:'disk'})
assert.equal(imported?.pendingImport,true);assert.equal(record.config.source,source,'parent pool was not resolved to its encrypted child');assert.equal(bound,false)
const pending=await poolEncryption(await nodeForPendingPool(node,4,'data'),'data',vault)
assert.equal(pending.encrypted,true);assert.equal(pending.state,'locked');assert.equal(pending.pendingImport,true)
await assert.rejects(unlockAndBindPool(node,4,'data',vault,'0'.repeat(64)))
assert.equal(saved,null);assert.equal(bound,false)
failBind=true
await assert.rejects(unlockAndBindPool(node,4,'data',vault,key),/密钥已加载并保存.*绑定未完成/)
assert.ok(saved);assert.equal(record.config._payincusImport.source,source)
failBind=false
const ready=await unlockAndBindPool(node,4,'data',vault)
assert.equal(ready.state,'unlocked');assert.equal(ready.pendingImport,false);assert.equal(bound,true);assert.equal(record.config._payincusImport,undefined)
loaded=false;manual=true
assert.equal(await autoLoadStoragePool(node,4,'data',vault),'skipped');assert.equal(automaticWrites,0)
manual=false;globalLocked=true
assert.equal(await autoLoadStoragePool(node,4,'data',vault),'skipped');assert.equal(automaticWrites,0)
globalLocked=false
assert.equal(await autoLoadStoragePool(node,4,'data',vault),'loaded');assert.equal(automaticWrites,1);assert.equal(loaded,true)
assert.equal(await autoLoadStoragePool(node,4,'data',vault),'skipped');assert.equal(automaticWrites,1)
record.config._payincusImport={source,encryptionRoot:source};changed=true
await assert.rejects(poolEncryption(await nodeForPendingPool(node,4,'data'),'data',vault),/来源已变化/)
changed=false
await assert.rejects(importEncryptedPool(4,{name:'missing',source:'not-present'}),/暂未发现/)
// Existing external pool roots also use the same custody envelope and import path.
record=null;saved=null;bound=false;registered=false;loaded=false;source='foreign'
const root=await importEncryptedPool(4,{name:'data',source})
assert.equal(root?.pendingImport,true)
const rootReady=await unlockAndBindPool(node,4,'data',vault,key)
assert.equal(rootReady.encryptionRoot,'foreign');assert.equal(rootReady.keySaved,true)
assert.equal(JSON.stringify(rootReady).includes(key),false)
console.log('Encrypted external import: visible locked checkpoint, correct encrypted child selection, bad-key refusal, partial-bind retry, automatic custody reuse, lock-safe loader and external root support passed')
