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

// 1. 静态断言检查 terminal-proxy.ts 源码中对 WebSocket 隧道的适配
const proxySource = readRepoFile('server/src/lib/terminal-proxy.ts')

assert.ok(
  proxySource.includes("import { hostTunnelManager } from './incus/tunnel-manager.js'"),
  'terminal-proxy must import hostTunnelManager'
)
assert.ok(
  proxySource.includes('hostTunnelManager.isTunnelOnline('),
  'terminal-proxy must check isTunnelOnline for tunnel-enabled hosts'
)
assert.ok(
  proxySource.includes('hostTunnelManager.createDuplexStream('),
  'terminal-proxy must create duplex streams using hostTunnelManager'
)
assert.ok(
  proxySource.includes('tls.connect({') && proxySource.includes('socket: duplex'),
  'terminal-proxy must wrap duplex streams in tls.connect'
)
assert.ok(
  proxySource.includes('dataWsOptions.createConnection = createTunnelSocket'),
  'terminal-proxy must provide createConnection for dataWs in tunnel mode'
)
assert.ok(
  proxySource.includes('controlWsOptions.createConnection = createTunnelSocket'),
  'terminal-proxy must provide createConnection for controlWs in tunnel mode'
)

// 2. 行为验证：当穿透开启但 Agent 离线时，应抛出离线错误
const { createIncusConsoleConnection } = await import('../src/lib/terminal-proxy.js')
const dummyHostOffline: any = {
  id: 999999,
  name: 'offline-tunnel-host',
  url: 'https://127.0.0.1:8443',
  tunnelEnabled: true,
  targetHost: '127.0.0.1',
  targetPort: 8443,
  cert_path: '/dummy/cert.pem',
  key_path: '/dummy/key.pem'
}

await assert.rejects(
  async () => {
    await createIncusConsoleConnection(dummyHostOffline, 'test-instance')
  },
  /宿主机内网穿透通道未连接（Agent 离线）/,
  'createIncusConsoleConnection must reject when tunnel agent is offline'
)

console.log('terminal tunnel guard tests passed')
