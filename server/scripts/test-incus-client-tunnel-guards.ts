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

// Check types
const incusTypes = readRepoFile('server/src/types/incus.ts')
assert.ok(incusTypes.includes('tunnelEnabled?: boolean'), 'IncusClientOptions must support tunnelEnabled')
assert.ok(incusTypes.includes('hostId?: number'), 'IncusClientOptions must support hostId')
assert.ok(incusTypes.includes('targetHost?: string'), 'IncusClientOptions must support targetHost')
assert.ok(incusTypes.includes('targetPort?: number'), 'IncusClientOptions must support targetPort')

// Check incus-client.ts
const clientSource = readRepoFile('server/src/lib/incus/incus-client.ts')
assert.ok(clientSource.includes('hostTunnelManager'), 'IncusClient should import and use hostTunnelManager')
assert.ok(clientSource.includes('createDuplexStream'), 'IncusClient should call createDuplexStream')
assert.ok(clientSource.includes('tls.connect'), 'IncusClient should wrap duplex stream in tls.connect')

// Check incus-pool.ts
const poolSource = readRepoFile('server/src/lib/incus/incus-pool.ts')
assert.ok(poolSource.includes('tunnelEnabled'), 'incus-pool should pass tunnelEnabled to IncusClient')

console.log('incus client tunnel guard tests passed')
