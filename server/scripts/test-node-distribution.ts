import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
const root = resolve(fileURLToPath(new URL('../..', import.meta.url)))
const read = (path: string) => readFileSync(resolve(root, path), 'utf8')
assert.equal(existsSync(resolve(root, 'agent/cmd/incudal-agent/main.go')), false)
assert.equal(existsSync(resolve(root, '.github/workflows/agent-release.yml')), false)
assert.equal(existsSync(resolve(root, 'server/scripts/smoke-agent-release.ts')), false)
assert.doesNotMatch(read('Dockerfile'), /agent-builder|COPY agent|agent-dist/)
for (const path of ['server/templates/install.sh', 'server/templates/agent-install.sh']) {
 const body = read(path)
 assert.match(body, /https:\/\/raw\.githubusercontent\.com\/Rain-kl\/incus/)
 assert.match(body, /latest\) branch=main/)
 assert.match(body, /canary\) branch=canary/)
 assert.match(body, /--proto '=https'/)
 assert.doesNotMatch(body, /agent\/binary|manifest\.json|ExecStart|systemctl|apt-get|mv .*agent|cp .*agent/)
 execFileSync('bash', ['-n', resolve(root, path)])
}
const routes = read('server/src/routes/agent.ts')
assert.doesNotMatch(routes, /getLatestAgentVersion|buildAgentUpgradeInstruction|downloadAgentReleaseBinary|INCUDAL_AGENT_RELEASE_/)
assert.match(routes, /nodeApi !== 'payincus_node_v1'/)
assert.match(routes, /managedBy: 'apt'/)
const tunnel = read('server/src/lib/incus/tunnel-manager.ts')
assert.doesNotMatch(tunnel, /encodeFrame\(TunnelFrameType\.CONTROL_REQUEST|case TunnelFrameType\.CONTROL_RESPONSE/)
console.log('Incus-only node source, installer, distribution and transport boundaries passed')
