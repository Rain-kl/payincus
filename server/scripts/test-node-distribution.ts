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
assert.equal(existsSync(resolve(root, 'server/templates/agent-install.sh')), false, 'separate Agent installer must be retired')
for (const path of ['server/templates/install.sh']) {
 const body = read(path)
 assert.match(body, /https:\/\/raw\.githubusercontent\.com\/Rain-kl\/incus/)
 assert.match(body, /latest\) branch=main/)
 assert.match(body, /canary\) branch=canary/)
 assert.match(body, /--proto '=https'/)
 assert.doesNotMatch(body, /agent\/binary|manifest\.json|ExecStart|systemctl|apt-get|mv .*agent|cp .*agent/)
 execFileSync('bash', ['-n', resolve(root, path)])
}
const routes = read('server/src/routes/agent.ts')
assert.match(routes, /templates\/install\.sh/)
assert.doesNotMatch(routes, /templates\/agent-install\.sh/)
assert.doesNotMatch(routes, /getLatestAgentVersion|buildAgentUpgradeInstruction|downloadAgentReleaseBinary|INCUDAL_AGENT_RELEASE_/)
assert.match(routes, /nodeApi !== 'payincus_node_v1'/)
assert.match(routes, /managedBy: 'apt'/)
const tunnel = read('server/src/lib/incus/tunnel-manager.ts')
assert.doesNotMatch(tunnel, /encodeFrame\(TunnelFrameType\.CONTROL_REQUEST|case TunnelFrameType\.CONTROL_RESPONSE/)
const hosts = read('server/src/routes/hosts.ts')
assert.match(hosts, /buildNodeInstallCommand\(/)
assert.doesNotMatch(hosts, /function buildHostInstallCommand|installToken = randomUUID\(\)/)
const regenerate = hosts.slice(hosts.indexOf("'/:id/regenerate-install'"), hosts.indexOf('// 测试宿主机连接（支持节点所有者）'))
assert.match(regenerate, /issueHostAgentInstallToken\(hostId, true\)/)
assert.doesNotMatch(regenerate, /prisma\.host\.update|certPath: null|keyPath: null|isInstalled: false/)

// A target added to the build matrix without the installer and the publish
// steps fails at release time, after a full build, with a duplicate-asset error.
// The node source lives in its own repository, so these checks only run where a
// checkout is present; the incus repository's own CI is the authoritative gate.
const nodeRoot = resolve(root, 'incus')
if (existsSync(nodeRoot)) {
  const integration = read('incus/.github/workflows/payincus.yml')
  const publish = read('incus/.github/workflows/payincus-publish.yml')
  const installer = read('incus/scripts/payincus/install.sh')
  const matrixTargets = [...new Set([...integration.matchAll(/^\s+target: (\S+)$/gm)].map(m => m[1]))].sort()
  assert.ok(matrixTargets.length > 0, 'integration matrix must declare at least one package target')
  for (const target of matrixTargets) {
    const distro = target.replace('-', ':')
    assert.ok(installer.includes(distro), `install.sh must accept the matrix target ${target}`)
  }
  // Publish steps must not enumerate targets by hand: every extra target that a
  // hard-coded list forgets leaves two identically named SHA256SUMS assets.
  for (const [name, body] of [['payincus.yml', integration], ['payincus-publish.yml', publish]]) {
    // Only the final publish step is inspected: verification steps may legitimately
    // enumerate targets, and the workflow name itself contains "Publish".
    const rename = body.slice(body.lastIndexOf('Publish'))
    assert.doesNotMatch(rename, /for target in\s+(?:ubuntu|debian)-\S+/, `${name} must derive release targets from the artifacts, not a fixed list`)
    assert.match(rename, /for folder in release\/payincus-linux-\*/, `${name} must iterate the downloaded artifacts`)
  }
}
console.log('Incus-only node source, installer, distribution and transport boundaries passed')
