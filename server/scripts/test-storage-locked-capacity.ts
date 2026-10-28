import assert from 'node:assert/strict'
import { prisma } from '../src/db/prisma.js'
import { IncusClient } from '../src/lib/incus/incus-client.js'
import { getStorageCapacity, getStorageOverview } from '../src/lib/storage-capacity.js'
const GiB = 1024n ** 3n
const pool = { id: 1, name: 'data', driver: 'zfs', config: { source: 'pool/incus' }, status: 'ready', totalBytes: 100n * GiB, usedBytes: 0n, reportedAt: new Date(), telemetry: {} }
const host = { id: 4, status: 'online', diskUsed: 0, agent: null, nodeSecurity: null as any }
Object.assign(prisma.storagePool, { findMany: async () => [pool] })
Object.assign(prisma.instance, { findMany: async () => [] })
Object.assign(prisma.host, { findUnique: async () => host })
Object.assign(IncusClient.prototype, { connect: async () => { throw new Error('capacity read contacted a node') }, request: async () => { throw new Error('capacity read contacted a node') } })
const read = () => getStorageCapacity(4)
assert.equal((await read()).reason, null)
for (const status of ['locked', 'unavailable', 'missing', 'pending']) {
 pool.status = status
 assert.equal((await read()).reason, 'storage_unavailable')
 assert.equal((await getStorageOverview(4)).totalMB, 102400, 'Last known capacity must survive failures')
}
pool.status = 'ready'
host.nodeSecurity = { report: { locked: true } }
assert.equal((await read()).reason, 'storage_unavailable')
host.nodeSecurity = null
host.status = 'offline'
assert.equal((await read()).reason, 'storage_unavailable')
host.status = 'online'
pool.reportedAt = new Date(Date.now() - 121000)
assert.equal((await read()).reason, 'storage_unavailable')
pool.reportedAt = new Date()
assert.equal((await read()).reason, null)
console.log('Stored capacity rejects offline/locked/unreported/stale pools, preserves totals and performs no node requests')
