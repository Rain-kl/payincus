import assert from 'node:assert/strict'
import { readFileSync, mkdtempSync, writeFileSync, rmSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import { buildNodeInstallCommand } from '../src/lib/node-install.js'
import './test-node-distribution.js'
const hostAgentCredentialsSource=readFileSync(new URL('../src/lib/host-agent-credentials.ts',import.meta.url),'utf8')
assert.ok(
  hostAgentCredentialsSource.includes("const agentInstallTokenPrefix = 'ait_'") &&
    hostAgentCredentialsSource.includes('const agentInstallTokenTtlMs = 30 * 60 * 1000'),
  'Agent install tokens must use a distinct prefix and bounded 30-minute TTL'
)
assert.ok(
  hostAgentCredentialsSource.includes("return createHash('sha256').update(token).digest('hex')") &&
    hostAgentCredentialsSource.includes('installTokenHash: hashAgentInstallToken(installToken)'),
  'Agent install tokens must be hashed before persistence'
)
assert.ok(
  hostAgentCredentialsSource.includes('where: { installTokenHash: tokenHash }') &&
    hostAgentCredentialsSource.includes('installTokenUsedAt: null') &&
    hostAgentCredentialsSource.includes('installTokenExpiresAt: { gte: new Date() }') &&
    hostAgentCredentialsSource.includes('if (updated.count !== 1)'),
  'Agent install-token consumption must be conditional on unused and unexpired state'
)
assert.ok(
  hostAgentCredentialsSource.includes('installTokenHash: null') &&
    hostAgentCredentialsSource.includes('installTokenExpiresAt: null') &&
    hostAgentCredentialsSource.includes('installTokenUsedAt: new Date()'),
  'Agent install-token consumption must clear the token hash and mark used-at'
)

// Exercise the generated shell command with inert curl/sudo stubs. No package is installed.
const directory = mkdtempSync(join(tmpdir(), 'incus-node-command-'))
try {
  writeFileSync(join(directory, 'curl'), `#!/bin/sh
printf '%s\\n' "$@" > curl-args
[ "\${FAIL_DOWNLOAD:-}" != 1 ] || exit 22
cat > incus-node-install.sh <<'SCRIPT'
#!/bin/sh
printf '%s\\n' "$INCUDAL_PANEL_URL" "$INCUDAL_AGENT_INSTALL_TOKEN" "$INCUS_NODE_CHANNEL"
SCRIPT
`, { mode: 0o755 })
  writeFileSync(join(directory, 'sudo'), '#!/bin/sh\nexec "$@"\n', { mode: 0o755 })
  const panelUrl = "https://panel.example.com/a'$(touch injected)"
  const installToken = "ait_fixture'$(touch injected)_not_a_real_secret"
  for (const channel of ['latest', 'canary'] as const) {
    const command = buildNodeInstallCommand({ panelUrl: `${panelUrl}/`, installToken, channel })
    const env = { ...process.env, PATH: `${directory}:${process.env.PATH}` }
    assert.equal(execFileSync('bash', ['-c', command], { cwd: directory, env, encoding: 'utf8' }), `${panelUrl}\n${installToken}\n${channel}\n`)
    assert.deepEqual(readFileSync(join(directory, 'curl-args'), 'utf8').trim().split('\n'), ['-fsSL', `${panelUrl}/api/agent/install.sh`, '-o', 'incus-node-install.sh'])
    assert.equal(existsSync(join(directory, 'injected')), false, 'install inputs must never become shell commands')
    const failed = spawnSync('bash', ['-c', command], { cwd: directory, env: { ...env, FAIL_DOWNLOAD: '1' }, encoding: 'utf8' })
    assert.equal(failed.status, 22, 'failed downloads must stop before sudo/bash execution')
    assert.equal(failed.stdout, '')
  }
} finally {
  rmSync(directory, { recursive: true, force: true })
}

console.log('agent install command guard tests passed')
