import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { prisma } from '../src/db/prisma.js'
import { IncusClient, IncusApiError } from '../src/lib/incus/incus-client.js'
import { provisionEncryptedPool } from '../src/lib/storage/provision.js'
import { fingerprint } from '../src/lib/node-security/vault.js'

const master = randomBytes(48).toString('base64')
process.env.ENCRYPTION_KEY = master
let saved: any = null
let failSave = false
let failNode = false
let failBind = false
let denyNode = false
let bound = false
let locked = false
let supported = true
let firstKey = ''
let nodeWrites = 0
let dbPools = 0
Object.assign(prisma.host, { findUnique: async () => ({ id: 4, userId: 10, url: 'https://node.invalid' }) })
Object.assign(prisma.nodeSecurityState, { findUnique: async () => ({ serverFingerprint: 'a'.repeat(64) }) })
Object.assign(prisma.nodeStorageKey, {
 findUnique: async () => saved,
 upsert: async ({create}: any) => { if (failSave) throw new Error('fixture'); saved ??= create; return saved },
 updateMany: async ({data}: any) => { saved.ciphertext = data.ciphertext; return {count:1} }
})
Object.assign(prisma.storagePool, { upsert: async ({create}: any) => { assert.equal(create.config.source,'test/incus'); dbPools++; return create } })
Object.assign(IncusClient.prototype, {
 connect: async function(this: IncusClient) { assert.equal(this.options.serverFingerprint,'a'.repeat(64)) }, close: async () => {},
 request: async (method: string, path: string, body?: any) => {
  if (path === '/1.0/payincus') return {api_extensions:supported?['payincus_storage_encryption_v1']:[]}
  if (path === '/1.0/payincus/security') return {locked}
  if (path === '/1.0/storage-pools' && method === 'GET') return bound?['/1.0/storage-pools/data']:[]
  if (path === '/1.0/payincus/storage/pool') {
   nodeWrites++
   assert.ok(saved,'node write preceded durable key custody')
   assert.equal(body.name,'test'); assert.equal(body.sizeGiB,2); assert.equal(body.device,'')
   assert.equal(saved.fingerprint,fingerprint(body.key))
   if (firstKey) assert.equal(body.key,firstKey,'retry rotated the only usable key')
   firstKey=body.key
   if(denyNode) throw new IncusApiError(`dataset creation permission denied: ${body.key}`,403,body)
   if(failNode) throw new Error('fixture lost response')
   return {targets:[{dataset:'test/incus',fingerprint:fingerprint(firstKey)}]}
  }
  if(path === '/1.0/storage-pools' && method === 'POST') {
   nodeWrites++; assert.equal(body.driver,'zfs'); assert.deepEqual(body.config,{source:'test/incus'})
   assert.equal(JSON.stringify(body).includes(firstKey),false)
   if(failBind)throw new Error('fixture binding failure')
   bound=true; return {}
  }
  if(path === '/1.0/storage-pools/data') return {driver:'zfs',config:{'zfs.pool_name':'test/incus'}}
  if(path === '/1.0/payincus/storage') return {datasets:[{dataset:'test/incus',encryptionRoot:'test/incus',algorithm:'aes-256-gcm',keyStatus:'available',keyFormat:'hex'}]}
  throw new Error('unexpected endpoint')
 }
})
const options={name:'data',zfsPoolName:'test',size:'2GiB',useLoop:true}
await assert.rejects(provisionEncryptedPool(4,{...options,size:'2GiB',source:'/dev/vdb'}))
assert.equal(nodeWrites,0)
supported=false; await assert.rejects(provisionEncryptedPool(4,options)); assert.equal(saved,null)
supported=true; locked=true; await assert.rejects(provisionEncryptedPool(4,options)); assert.equal(saved,null)
locked=false; process.env.ENCRYPTION_KEY='weak'; await assert.rejects(provisionEncryptedPool(4,options)); assert.equal(nodeWrites,0)
process.env.ENCRYPTION_KEY=master
failSave=true; await assert.rejects(provisionEncryptedPool(4,options)); assert.equal(nodeWrites,0)
failSave=false; denyNode=true
await assert.rejects(provisionEncryptedPool(4,options),(error:any)=>error.statusCode===403&&error.message.includes('dataset creation permission denied')&&!error.message.includes(firstKey)&&!error.message.includes('database disk image'))
denyNode=false; failNode=true; await assert.rejects(provisionEncryptedPool(4,options),/密钥已保留/)
assert.ok(saved); assert.equal(dbPools,0)
failNode=false; failBind=true; await assert.rejects(provisionEncryptedPool(4,options),/密钥已保留/); assert.equal(dbPools,0)
failBind=false
const result=await provisionEncryptedPool(4,options)
assert.equal(result.encrypted,true); assert.equal(result.source,'test/incus'); assert.equal(result.keySaved,true)
assert.equal(JSON.stringify(result).includes(firstKey),false)
const writes=nodeWrites
await provisionEncryptedPool(4,options)
assert.equal(nodeWrites,writes,'completed retry repeated remote writes')
console.log('Encrypted storage provisioning: save-before-write, unsupported/locked refusal, interrupted creation/binding, same-key retry and confirmed Incus source passed')
