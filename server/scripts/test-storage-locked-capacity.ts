import assert from 'node:assert/strict'
import { prisma } from '../src/db/prisma.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { closeAllClients } from '../src/lib/incus/incus-pool.js'
import { getStorageCapacity, invalidateHostStorageCapacity } from '../src/lib/storage-capacity.js'
let available=true,manual=false,locked=false,failed=false,pending=false
Object.assign(prisma.storagePool,{findMany:async()=>[{name:'data',driver:'zfs',config:{source:'pool/incus',...(pending?{_payincusImport:{source:'pool/incus'}}:{})},updatedAt:new Date(0)}]})
Object.assign(prisma.instance,{findMany:async()=>[]})
Object.assign(prisma.host,{findUnique:async()=>({id:4,url:'https://node.invalid',diskUsed:0})})
Object.assign(IncusClient.prototype,{
 connect:async function(this:IncusClient){this.connected=true;return {}},
 request:async(_method:string,path:string)=>{
  if(path==='/1.0/payincus/storage'){
   if(failed)throw new Error('fixture unavailable')
   return {locked,datasets:[{dataset:'pool/incus',encryptionRoot:'pool/incus',algorithm:'aes-256-gcm',keyStatus:available?'available':'unavailable'}],locks:{'pool/incus':{locked:manual}}}
  }
  if(path.endsWith('/resources'))return {space:{total:100*1024**3,used:0}}
  throw new Error('unexpected path')
 }
})
const read=async()=>{invalidateHostStorageCapacity(4);return getStorageCapacity(4)}
assert.equal((await read()).reason,null)
available=false;assert.equal((await read()).reason,'storage_unavailable')
available=true;manual=true;assert.equal((await read()).reason,'storage_unavailable')
manual=false;locked=true;assert.equal((await read()).reason,'storage_unavailable')
locked=false;failed=true;assert.equal((await read()).reason,'storage_unavailable')
failed=false;pending=true;assert.equal((await read()).reason,'storage_unavailable')
pending=false;assert.equal((await read()).reason,null)
await closeAllClients()
console.log('ZFS allocation excludes unloaded keys, manual/global locks, unknown state and pending imports; unlocked capacity recovers')
