import assert from 'node:assert/strict'
import { resizeStoragePool } from '../src/lib/storage/resize.js'
let supported = true, native = false, failure = false
const writes: Array<{method: string; path: string; body: unknown}> = []
const node = { request: async <T>(method: string, path: string, body?: unknown): Promise<T> => {
 if (method === 'GET' && path === '/1.0/storage-pools/data') return { driver: 'zfs', config: native ? {source: '/var/lib/incus/disks/data.img', size: '1GiB'} : {source: 'tank/incus'} } as T
 if (method === 'GET' && path === '/1.0/payincus') return { api_extensions: supported ? ['payincus_storage_pool_resize_v1'] : [] } as T
 writes.push({method, path, body})
 if (failure) throw new Error('fixture node error')
 return {} as T
} }
await resizeStoragePool(node, 'data', '2TiB')
assert.deepEqual(writes.pop(), { method: 'POST', path: '/1.0/payincus/storage/resize', body: {dataset: 'tank/incus', sizeGiB: 2048} })
for (const size of ['-1GiB', '0GiB', '1.5GiB', '65537GiB', 'bad']) await assert.rejects(resizeStoragePool(node, 'data', size))
assert.equal(writes.length, 0)
supported = false
await assert.rejects(resizeStoragePool(node, 'data', '2GiB'), /更新节点/)
assert.equal(writes.length, 0)
supported = true; failure = true
await assert.rejects(resizeStoragePool(node, 'data', '2GiB'), /fixture node error/)
native = true; failure = false
await resizeStoragePool(node, 'data', '3GiB')
assert.deepEqual(writes.pop(), { method: 'PATCH', path: '/1.0/storage-pools/data', body: {config: {size: '3GiB'}} })
console.log('Storage resize contracts passed: native/custom routing, extension checks, size validation and error propagation')
