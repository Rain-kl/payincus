import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '../..')

function readRepoFile(path: string): string {
  return readFileSync(resolve(repoRoot, path), 'utf8')
}

// 1. 静态断言：server/src/db/host-addresses.ts
const dbHostAddresses = readRepoFile('server/src/db/host-addresses.ts')

assert.ok(
  dbHostAddresses.includes('tunnelEnabled: false'),
  'getHostsForAddressBackfill and getHostsWithDomainInputAlias must filter tunnelEnabled: false'
)
assert.ok(
  dbHostAddresses.includes('export async function removeHostAddressAliases('),
  'host-addresses.ts must export removeHostAddressAliases'
)
assert.ok(
  dbHostAddresses.includes('export async function cleanupTunnelHostAddressAliases('),
  'host-addresses.ts must export cleanupTunnelHostAddressAliases'
)

// 2. 静态断言：server/src/services/host-address-monitor.ts
const monitorSource = readRepoFile('server/src/services/host-address-monitor.ts')

assert.ok(
  monitorSource.includes('cleanupTunnelHostAddressAliases'),
  'host-address-monitor.ts must import and use cleanupTunnelHostAddressAliases'
)
assert.ok(
  monitorSource.includes('if (host?.tunnelEnabled)') &&
  monitorSource.includes('await removeHostAddressAliases(hostId, tx)'),
  'syncExistingHostAddressState must guard against tunnelEnabled and remove any stale aliases'
)
assert.ok(
  monitorSource.includes('cleanupTunnelHostAddressAliases(tx)'),
  'runHostAddressBackfillJob must invoke cleanupTunnelHostAddressAliases'
)

// 3. 静态断言：server/src/routes/hosts.ts
const hostsRouteSource = readRepoFile('server/src/routes/hosts.ts')

assert.ok(
  hostsRouteSource.includes('tunnelModeEffective') &&
  hostsRouteSource.includes('await db.removeHostAddressAliases(hostId, tx)'),
  'updateHost route must remove aliases when updating a tunnel host'
)
assert.ok(
  hostsRouteSource.includes('if (updates.tunnelEnabled === true) {') &&
  hostsRouteSource.includes('await db.removeHostAddressAliases(hostId, tx)'),
  'updateHost route must remove aliases when toggling tunnelEnabled to true'
)

// 4. 逻辑单元测试：removeHostAddressAliases 和 cleanupTunnelHostAddressAliases
const { removeHostAddressAliases, cleanupTunnelHostAddressAliases } = await import('../src/db/host-addresses.js')

let deletedHostId: number | null = null
let deletedTunnelHosts = false
let resolvedConflictId: number | null = null
let syncedAddresses: string[] = []

const mockDbClient: any = {
  host: {
    findMany: async ({ where }: any) => {
      if (where?.tunnelEnabled === true) {
        return [{ id: 888 }, { id: 999 }]
      }
      return []
    }
  },
  hostAddressAlias: {
    findMany: async ({ where }: any) => {
      if (where?.hostId === 888) {
        return [{ address: '127.0.0.1' }]
      }
      if (where?.hostId?.in?.includes(888)) {
        return [{ address: '127.0.0.1' }]
      }
      return []
    },
    deleteMany: async ({ where }: any) => {
      if (where?.hostId === 888) {
        deletedHostId = where.hostId
        return { count: 1 }
      }
      if (where?.hostId?.in?.includes(888)) {
        deletedTunnelHosts = true
        return { count: 1 }
      }
      return { count: 0 }
    }
  },
  hostAddressConflict: {
    findMany: async ({ where }: any) => {
      if (where?.address?.in?.includes('127.0.0.1')) {
        return [{ id: 101, hostAId: 888, hostBId: 999, address: '127.0.0.1', status: 'active' }]
      }
      return []
    },
    update: async ({ where, data }: any) => {
      if (where?.id === 101 && data?.status === 'resolved') {
        resolvedConflictId = where.id
        syncedAddresses.push('127.0.0.1')
      }
      return { id: 101, ...data }
    }
  }
}

// 4.1 测试 removeHostAddressAliases
const removed = await removeHostAddressAliases(888, mockDbClient)
assert.deepEqual(removed, ['127.0.0.1'], 'removeHostAddressAliases should return removed addresses')
assert.equal(deletedHostId, 888, 'removeHostAddressAliases must delete aliases by hostId')

// 4.2 测试 cleanupTunnelHostAddressAliases
const conflicts = await cleanupTunnelHostAddressAliases(mockDbClient)
assert.ok(Array.isArray(conflicts), 'cleanupTunnelHostAddressAliases should return conflict info array')
assert.equal(deletedTunnelHosts, true, 'cleanupTunnelHostAddressAliases must delete aliases where host.tunnelEnabled is true')
assert.equal(resolvedConflictId, 101, 'cleanupTunnelHostAddressAliases must resolve conflict with id 101')
assert.ok(syncedAddresses.includes('127.0.0.1'), 'cleanupTunnelHostAddressAliases must sync conflicts for affected addresses')

console.log('host address tunnel guard tests passed')
