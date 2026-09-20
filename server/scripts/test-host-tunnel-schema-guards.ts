import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '../..')

function readRepoFile(path: string): string {
  return readFileSync(resolve(repoRoot, path), 'utf8')
}

const schema = readRepoFile('server/prisma/schema.prisma')
assert.ok(schema.includes('tunnelEnabled     Boolean      @default(false) @map("tunnel_enabled")'), 'schema should define tunnelEnabled')
assert.ok(schema.includes('targetHost        String       @default("127.0.0.1") @map("target_host")'), 'schema should define targetHost')
assert.ok(schema.includes('targetPort        Int          @default(8443) @map("target_port")'), 'schema should define targetPort')

const migrationPath = resolve(repoRoot, 'server/prisma/migrations/20260920080000_add_host_tunnel_fields/migration.sql')
assert.ok(existsSync(migrationPath), 'migration SQL file must exist')
const migrationSql = readFileSync(migrationPath, 'utf8')
assert.ok(migrationSql.includes('tunnel_enabled'), 'migration SQL must alter tunnel_enabled')
assert.ok(migrationSql.includes('target_host'), 'migration SQL must alter target_host')
assert.ok(migrationSql.includes('target_port'), 'migration SQL must alter target_port')

const dbHosts = readRepoFile('server/src/db/hosts.ts')
assert.ok(dbHosts.includes('tunnelEnabled'), 'server/src/db/hosts.ts should handle tunnelEnabled')
assert.ok(dbHosts.includes('targetHost'), 'server/src/db/hosts.ts should handle targetHost')
assert.ok(dbHosts.includes('targetPort'), 'server/src/db/hosts.ts should handle targetPort')

const clientApi = readRepoFile('client/src/types/api.ts')
assert.ok(clientApi.includes('tunnelEnabled') || clientApi.includes('tunnel_enabled'), 'client/src/types/api.ts should define tunnel fields')

console.log('host tunnel schema guard tests passed')
