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

const agentRoutes = readRepoFile('server/src/routes/agent.ts')
assert.ok(agentRoutes.includes("'/tunnel'"), 'agent routes should define /tunnel endpoint')
assert.ok(agentRoutes.includes('hostTunnelManager.registerTunnel'), 'agent /tunnel should register tunnel to hostTunnelManager')
assert.ok(agentRoutes.includes('tunnel: {'), 'agent heartbeat should deliver tunnel config')

const hostsRoutes = readRepoFile('server/src/routes/hosts.ts')
assert.ok(hostsRoutes.includes('hostTunnelManager'), 'hosts routes should reference hostTunnelManager for dynamic config')

console.log('agent tunnel route guard tests passed')
