import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'

process.env.ENCRYPTION_KEY = randomBytes(48).toString('base64')

const { prisma } = await import('../src/db/prisma.js')
const { IncusClient, IncusApiError } = await import('../src/lib/incus/incus-client.js')
const { hostTunnelManager } = await import('../src/lib/incus/tunnel-manager.js')
const { issueHostAgentInstallToken, consumeHostAgentInstallToken } = await import('../src/lib/host-agent-credentials.js')
const { recordHeartbeatSecurityState, recordReport } = await import('../src/lib/node-security/report.js')
const { syncDesiredSecurityPolicy, clearPolicySyncCooldown } = await import('../src/lib/node-security/policy-sync.js')
const { processAgentStorageReport } = await import('../src/services/agent-storage-report.js')
const { syncHostStorageBaseline } = await import('../src/services/storage-baseline-sync.js')

const hostId = 42
const createdAt = new Date('2026-01-01T00:00:00Z')

let agentRow: any = null
let securityRow: any = {
  hostId,
  serverFingerprint: 'f'.repeat(64),
  recoveryBlocked: true,
  recoveryGeneration: 3,
  report: { enabled: true, locked: true, revision: 9, reason: 'cli:exec', _desiredEnabled: true },
  reportedAt: new Date('2026-01-02T00:00:00Z')
}

let poolsState: any[] = [
  {
    id: 101,
    hostId,
    name: 'default',
    driver: 'zfs',
    purpose: 'instance_data',
    description: 'primary',
    config: { source: 'tank/incus' },
    status: 'ready',
    totalBytes: 100n * 1024n ** 3n,
    usedBytes: 20n * 1024n ** 3n,
    reportedAt: new Date(Date.now() - 300_000),
    telemetry: { usedBy: 1, spaceAvailable: 75 * 1024 ** 3 },
    createdAt
  },
  {
    id: 102,
    hostId,
    name: 'nvme',
    driver: 'dir',
    purpose: 'instance_storage',
    description: 'secondary',
    config: { source: '/var/lib/nvme' },
    status: 'ready',
    totalBytes: 200n * 1024n ** 3n,
    usedBytes: 40n * 1024n ** 3n,
    reportedAt: new Date(Date.now() - 300_000),
    telemetry: { usedBy: 0, spaceAvailable: 150 * 1024 ** 3 },
    createdAt
  },
  {
    id: 103,
    hostId,
    name: 'stale-empty',
    driver: 'dir',
    purpose: 'instance_storage',
    description: 'no instances',
    config: { source: '/var/lib/old' },
    status: 'ready',
    totalBytes: 50n * 1024n ** 3n,
    usedBytes: 0n,
    reportedAt: new Date(Date.now() - 300_000),
    telemetry: {},
    createdAt
  }
]

let activeInstancesState: Array<{ storagePoolName: string | null }> = [{ storagePoolName: 'default' }]
const hostRow = {
  id: hostId,
  userId: 10,
  name: 'us-s1',
  url: 'https://127.0.0.1:8443',
  status: 'online',
  tunnelEnabled: false,
  targetHost: null,
  targetPort: null,
  certPath: '/fixture/cert',
  keyPath: '/fixture/key',
  architecture: null as string | null,
  natPublicIpv6: null as string | null,
  agent: agentRow,
  nodeSecurity: securityRow,
  createdAt,
  updatedAt: createdAt
}

const nodeSecurityDelegate = {
  findUnique: async () => securityRow ? { ...securityRow } : null,
  upsert: async ({ create, update }: any) => {
    securityRow = securityRow ? { ...securityRow, ...update } : { hostId, recoveryBlocked: false, recoveryGeneration: 0, serverFingerprint: null, ...create }
    hostRow.nodeSecurity = securityRow
    return securityRow
  },
  updateMany: async ({ data }: any) => {
    if (!securityRow) return { count: 0 }
    securityRow = {
      ...securityRow,
      ...data,
      recoveryGeneration: typeof data?.recoveryGeneration?.increment === 'number'
        ? securityRow.recoveryGeneration + data.recoveryGeneration.increment
        : securityRow.recoveryGeneration
    }
    hostRow.nodeSecurity = securityRow
    return { count: 1 }
  }
}

Object.assign(prisma.host, {
  findUnique: async () => ({ ...hostRow, agent: agentRow, nodeSecurity: securityRow }),
  findMany: async () => [{ id: hostId, status: hostRow.status, tunnelEnabled: false, agent: agentRow, nodeSecurity: securityRow }],
  updateMany: async ({ data }: any) => {
    Object.assign(hostRow, data)
    return { count: 1 }
  }
})

Object.assign(prisma.hostAgent, {
  findUnique: async ({ where }: any) => {
    if (!agentRow) return null
    if (where.hostId !== undefined && agentRow.hostId !== where.hostId) return null
    if (where.agentId !== undefined && agentRow.agentId !== where.agentId) return null
    if (where.installTokenHash !== undefined && agentRow.installTokenHash !== where.installTokenHash) return null
    return { ...agentRow, host: { id: hostId, name: hostRow.name } }
  },
  upsert: async ({ create, update }: any) => {
    agentRow = agentRow ? { ...agentRow, ...update } : { id: 1, status: 'offline', enabled: true, ...create }
    hostRow.agent = agentRow
    return agentRow
  },
  update: async ({ data }: any) => {
    agentRow = { ...agentRow, ...data }
    hostRow.agent = agentRow
    return agentRow
  },
  updateMany: async ({ where, data }: any) => {
    if (!agentRow || (where.installTokenHash && agentRow.installTokenHash !== where.installTokenHash)) return { count: 0 }
    agentRow = { ...agentRow, ...data }
    hostRow.agent = agentRow
    return { count: 1 }
  }
})

Object.assign(prisma.nodeSecurityState, nodeSecurityDelegate)
Object.assign(prisma.storagePool, {
  findMany: async () => poolsState.map(p => ({ ...p })),
  updateMany: async ({ where, data }: any) => {
    const idx = poolsState.findIndex(p => p.id === where.id)
    if (idx < 0) return { count: 0 }
    poolsState[idx] = { ...poolsState[idx], ...data }
    return { count: 1 }
  }
})
Object.assign(prisma.instance, {
  findMany: async () => activeInstancesState
})
Object.assign(prisma, {
  $transaction: async (fn: any) => fn({
    $executeRaw: async () => 0,
    nodeSecurityState: nodeSecurityDelegate,
    nodeCliEvent: { deleteMany: async () => ({ count: 0 }), createMany: async () => ({ count: 0 }) },
    log: { create: async () => ({ id: 1 }) }
  })
})
hostTunnelManager.isTunnelOnline = () => true

// 1. Re-enrollment clears stale fingerprint, lock report & recoveryBlocked while preserving _desiredEnabled.
const issued = await issueHostAgentInstallToken(hostId)
const consumed = await consumeHostAgentInstallToken(issued.installToken)
assert.equal(consumed.agent.hostId, hostId)
assert.equal(securityRow.serverFingerprint, null, 'reinstall must clear stale serverFingerprint')
assert.equal(securityRow.recoveryBlocked, false, 'reinstall must clear recoveryBlocked')
assert.equal(securityRow.recoveryGeneration, 4, 'reinstall must advance recoveryGeneration')
assert.equal(securityRow.reportedAt.getTime(), 0, 'reinstall must reset reportedAt')
assert.equal(securityRow.report.locked, undefined, 'reinstall must clear stale locked badge')
assert.equal(securityRow.report.revision, undefined, 'reinstall must reset stale revision')
assert.equal(securityRow.report._desiredEnabled, true, 'reinstall must preserve _desiredEnabled policy')

// 2. Heartbeat reports node truth & detects policy mismatch only when unlocked.
const mismatchCheck = await recordHeartbeatSecurityState(hostId, { enabled: false, locked: false, revision: 1 })
assert.equal(mismatchCheck.policyMismatch, true)
assert.equal(securityRow.report.locked, false)
assert.equal(securityRow.report.enabled, false)

const lockedNoSync = await recordHeartbeatSecurityState(hostId, { enabled: false, locked: true, revision: 2 })
assert.equal(lockedNoSync.policyMismatch, false, 'must not push policy while node is locked')
assert.equal(securityRow.report.locked, true)

// Recover node back to unlocked revision=3, pin new fingerprint, verify policy push succeeds.
await recordReport(hostId, { state: { enabled: false, locked: false, revision: 3 }, events: [] }, { authoritative: true })
securityRow.serverFingerprint = 'b'.repeat(64)
clearPolicySyncCooldown(hostId)

let policyPostBody: any = null
let useFallbackBaseline = false
const baselineSeen = { fallbackPools: false, fallbackSecurity: false }

Object.assign(IncusClient.prototype, {
  connect: async () => {},
  close: async () => {},
  request: async (method: string, path: string, body?: any) => {
    if (path === '/1.0/agent/security' && method === 'GET') {
      baselineSeen.fallbackSecurity = true
      return { enabled: false, locked: false, revision: 3 }
    }
    if (path === '/1.0/agent/security/policy' && method === 'POST') {
      policyPostBody = body
      return { enabled: body.enabled, locked: false, revision: body.revision + 1 }
    }
    if (path === '/1.0/agent/baseline') {
      if (useFallbackBaseline) {
        throw new IncusApiError('not found', 404)
      }
      return {
        nodeSecurity: { state: { enabled: true, locked: false, revision: 5 } },
        runtime: { goarch: 'amd64' },
        resources: { system: { uuid: 'u1' } },
        storage: {
          available: true,
          complete: true,
          pools: [
            { name: 'default', driver: 'zfs', status: 'ready', config: { source: 'rebuilt/incus' }, space: { total: 100 * 1024 ** 3, used: 5 * 1024 ** 3, available: 80 * 1024 ** 3 } },
            { name: 'nvme', driver: 'dir', status: 'ready', config: { source: '/var/lib/nvme' }, space: { total: 200 * 1024 ** 3, used: 30 * 1024 ** 3, available: 145 * 1024 ** 3 } },
            { name: 'extra', driver: 'dir', status: 'ready', config: { source: '/var/lib/extra' }, space: { total: 50 * 1024 ** 3, used: 0, available: 50 * 1024 ** 3 } }
          ]
        }
      }
    }
    if (path === '/1.0/storage-pools?recursion=1') {
      baselineSeen.fallbackPools = true
      return [
        { name: 'default', driver: 'zfs', status: 'Created', config: { source: 'rebuilt/incus' } },
        { name: 'nvme', driver: 'dir', status: 'Created', config: { source: '/var/lib/nvme' } }
      ]
    }
    if (path === '/1.0/agent/storage') {
      return { datasets: [{ dataset: 'rebuilt/incus', encryptionRoot: 'rebuilt/incus', algorithm: 'aes-256-gcm', keyStatus: 'available', keyFormat: 'hex' }] }
    }
    if (path === '/1.0/storage-pools/nvme/resources' || path === '/1.0/storage-pools/default/resources') {
      return { space: { total: 200 * 1024 ** 3, used: 30 * 1024 ** 3, available: 160 * 1024 ** 3 } }
    }
    throw new Error(`unexpected request ${method} ${path}`)
  }
})

const synced = await syncDesiredSecurityPolicy(hostId, true)
assert.equal(synced, true)
assert.deepEqual(policyPostBody, { enabled: true, revision: 3 })
assert.equal(securityRow.report.enabled, true)
assert.equal(securityRow.report.revision, 4)

// 3. processAgentStorageReport marks rebuilt pool with instances as 'missing', empty wrong-driver pool as 'unavailable', and persists spaceAvailable.
const now = new Date()
await processAgentStorageReport(hostId, {
  available: true,
  complete: true,
  reportedAt: now.toISOString(),
  pools: [
    { name: 'default', driver: 'zfs', status: 'ready', config: { source: 'tank/other' }, space: { total: 100 * 1024 ** 3, used: 5 * 1024 ** 3, available: 80 * 1024 ** 3 }, encryption: { encrypted: true, state: 'unlocked', source: 'tank/other', encryptionRoot: 'tank/other' } },
    { name: 'nvme', driver: 'dir', status: 'ready', config: { source: '/var/lib/nvme' }, space: { total: 200 * 1024 ** 3, used: 30 * 1024 ** 3, available: 145 * 1024 ** 3 } },
    { name: 'stale-empty', driver: 'zfs', status: 'ready', config: { source: 'other/incus' } }
  ]
}, now)

assert.equal(poolsState[0].status, 'missing', 'source mismatch with attached instances must be marked missing')
assert.equal(poolsState[1].status, 'ready')
assert.equal(poolsState[1].telemetry.spaceAvailable, 145 * 1024 ** 3, 'real space.available must be persisted verbatim')
assert.equal(poolsState[2].status, 'unavailable', 'driver mismatch without instances stays unavailable')

// 4. syncHostStorageBaseline works on both primary /1.0/agent/baseline and 404 fallback.
const primaryResult = await syncHostStorageBaseline(hostId)
assert.equal(primaryResult.usedBaselineEndpoint, true)
assert.equal(primaryResult.nodeSecuritySynced, true)
assert.equal(securityRow.report.revision, 5)
assert.deepEqual(primaryResult.untrackedPools, ['extra'])
assert.ok(primaryResult.missingPools.includes('default'))
assert.ok(primaryResult.missingPools.includes('stale-empty'))
assert.equal(hostRow.architecture, 'x86_64')

useFallbackBaseline = true
const fallbackResult = await syncHostStorageBaseline(hostId)
assert.equal(fallbackResult.usedBaselineEndpoint, false)
assert.equal(baselineSeen.fallbackPools, true)
assert.equal(baselineSeen.fallbackSecurity, true)
assert.equal(poolsState[1].telemetry.spaceAvailable, 160 * 1024 ** 3)

console.log('Host reinstall, desired security policy sync, missing-pool instance guard and baseline fallback guards passed')
