import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
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

console.log('agent install command guard tests passed')
