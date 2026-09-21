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

// 1. 静态断言：server/src/lib/incus/tunnel-manager.ts
const tunnelManagerSource = readRepoFile('server/src/lib/incus/tunnel-manager.ts')

assert.ok(
  tunnelManagerSource.includes("hostTunnelManager.on('tunnel_connected'") &&
  tunnelManagerSource.includes("hostTunnelManager.on('tunnel_disconnected'"),
  'tunnel-manager.ts must listen to tunnel_connected and tunnel_disconnected'
)
assert.ok(
  tunnelManagerSource.includes('async function syncTunnelHostStatus('),
  'tunnel-manager.ts must define syncTunnelHostStatus'
)
assert.ok(
  tunnelManagerSource.includes('export async function reconcileTunnelHostsStatus('),
  'tunnel-manager.ts must export reconcileTunnelHostsStatus'
)
assert.ok(
  tunnelManagerSource.includes("host.status === 'maintenance'"),
  'tunnel-manager.ts must preserve maintenance mode during auto-status sync'
)

// 2. 静态断言：server/src/routes/hosts.ts
const hostsRouteSource = readRepoFile('server/src/routes/hosts.ts')

assert.ok(
  hostsRouteSource.includes('deriveEffectiveHostStatus'),
  'hosts.ts must export and use deriveEffectiveHostStatus helper'
)
assert.ok(
  hostsRouteSource.includes('status: deriveEffectiveHostStatus(host)'),
  'GET / hosts list must use deriveEffectiveHostStatus'
)
assert.ok(
  hostsRouteSource.includes('status: effectiveStatus') &&
  hostsRouteSource.includes('const effectiveStatus = deriveEffectiveHostStatus(host)'),
  'GET /:id host detail must use deriveEffectiveHostStatus'
)
assert.ok(
  hostsRouteSource.includes("if (host.status === 'offline') {") &&
  hostsRouteSource.includes("await db.updateHostStatus(hostId, 'online')"),
  'POST /:id/test must set status online when connection succeeds'
)
assert.ok(
  hostsRouteSource.includes("if (host.status === 'online') {") &&
  hostsRouteSource.includes("await db.updateHostStatus(hostId, 'offline')"),
  'POST /:id/test must set status offline when connection fails'
)

// 3. 静态断言：server/src/app.ts
const appSource = readRepoFile('server/src/app.ts')

assert.ok(
  appSource.includes('reconcileTunnelHostsStatus'),
  'app.ts must invoke reconcileTunnelHostsStatus on startup'
)

// 4. 静态断言：client 前端防御
const hostInfoTabSource = readRepoFile('client/src/components/host/HostInfoTab.vue')
assert.ok(
  hostInfoTabSource.includes('props.host.tunnelEnabled && !props.host.tunnelOnline'),
  'HostInfoTab.vue must check tunnelEnabled && !tunnelOnline in statusInfo'
)

const myHostsViewSource = readRepoFile('client/src/views/resources/MyHostsView.vue')
assert.ok(
  myHostsViewSource.includes('host?.tunnelEnabled && !host?.tunnelOnline'),
  'MyHostsView.vue must check tunnelEnabled && !tunnelOnline in getStatusClass'
)

const myHostDetailViewSource = readRepoFile('client/src/views/resources/MyHostDetailView.vue')
assert.ok(
  myHostDetailViewSource.includes('host.value.tunnelEnabled && !host.value.tunnelOnline'),
  'MyHostDetailView.vue must check tunnelEnabled && !tunnelOnline in statusInfo'
)

// 5. 逻辑单元测试：deriveEffectiveHostStatus
const { deriveEffectiveHostStatus, hostTunnelManager } = await import('../src/lib/incus/tunnel-manager.js')

// 5.1 维护模式下的节点，无论穿透是否在线，均保持 maintenance
assert.equal(
  deriveEffectiveHostStatus({
    id: 1001,
    status: 'maintenance',
    tunnelEnabled: true
  }),
  'maintenance',
  'maintenance host must remain maintenance'
)

// 5.2 非穿透直连节点，返回原始 DB 状态
assert.equal(
  deriveEffectiveHostStatus({
    id: 1002,
    status: 'online',
    tunnelEnabled: false
  }),
  'online',
  'direct connection host returns DB online status'
)

// 5.3 穿透模式节点，隧道未连接时，状态强制衍生为 offline
assert.equal(
  deriveEffectiveHostStatus({
    id: 1003,
    status: 'online',
    tunnelEnabled: true
  }),
  'offline',
  'tunnel host with disconnected tunnel must derive offline status'
)

// 5.4 穿透模式节点，注册伪 WebSocket 模拟隧道在线后，状态为 online
const dummyWs: any = {
  readyState: 1, // WS_OPEN
  send: () => {},
  close: () => {},
  on: () => dummyWs
}
hostTunnelManager.registerTunnel(1004, dummyWs)

assert.equal(
  deriveEffectiveHostStatus({
    id: 1004,
    status: 'online',
    tunnelEnabled: true
  }),
  'online',
  'tunnel host with connected tunnel must derive online status'
)

// 清理模拟连接
hostTunnelManager.unregisterTunnel(1004)

assert.equal(
  deriveEffectiveHostStatus({
    id: 1004,
    status: 'online',
    tunnelEnabled: true
  }),
  'offline',
  'tunnel host must derive offline status after unregisterTunnel'
)

console.log('host tunnel status guard tests passed')
process.exit(0)
