import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import assert from 'node:assert/strict'

const routePath = resolve(process.cwd(), 'src/routes/proxy-sites.ts')
const source = readFileSync(routePath, 'utf8')
const caddyClientPath = resolve(process.cwd(), 'src/lib/caddy-client.ts')
const caddyClientSource = readFileSync(caddyClientPath, 'utf8')
const outboundSecurityPath = resolve(process.cwd(), 'src/lib/outbound-security.ts')
const outboundSecuritySource = readFileSync(outboundSecurityPath, 'utf8')

assert.ok(
  source.includes('function parsePositiveRouteId(value: string): number | null') &&
    source.includes('const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\\d*$/') &&
    source.includes('Number.isSafeInteger(parsed)'),
  'proxy site routes must use a strict positive safe-integer route ID parser'
)

assert.equal(
  source.match(/const instanceId = parsePositiveRouteId\(request\.params\.id\)/g)?.length ?? 0,
  8,
  'all proxy site instance path IDs must use the strict parser'
)

assert.equal(
  source.match(/const siteId = parsePositiveRouteId\(request\.params\.siteId\)/g)?.length ?? 0,
  6,
  'all proxy site path IDs must use the strict parser'
)

const forbiddenPatterns = [
  'parseInt(request.params.id, 10)',
  'parseInt(request.params.siteId, 10)',
  'isNaN(instanceId)',
  'isNaN(siteId)'
]

for (const pattern of forbiddenPatterns) {
  assert.ok(!source.includes(pattern), `proxy site route ID guard must not use ${pattern}`)
}

const certificateResolution = 'const [certificateAddress] = await resolvePublicHostname(site.domain)'
const certificateConnection = 'host: certificateAddress.address'
assert.ok(
  source.includes("import { resolvePublicHostname } from '../lib/outbound-security.js'") &&
    source.includes(certificateResolution) &&
    source.includes(certificateConnection) &&
    source.includes('servername: site.domain'),
  'certificate lookup must validate DNS through outbound security, pin the TLS host to that IP, and preserve SNI'
)
assert.ok(
  source.indexOf(certificateResolution) < source.indexOf('const socket = tls.connect({'),
  'certificate DNS validation must complete before opening the TLS connection'
)
assert.doesNotMatch(
  source,
  /tls\.connect\(\{[\s\S]*?host:\s*site\.domain/,
  'certificate TLS must not resolve the tenant-controlled hostname again at connect time'
)
assert.ok(
  outboundSecuritySource.includes('export async function resolvePublicHostname') &&
    outboundSecuritySource.includes('for (const record of records)') &&
    outboundSecuritySource.includes('isIpPrivateOrReserved(record.address)') &&
    outboundSecuritySource.includes('return records'),
  'outbound hostname resolution must reject every private or reserved result before returning pinned addresses'
)

assert.ok(
  caddyClientSource.includes('class CaddyApiError extends Error') &&
    caddyClientSource.includes('readonly statusCode: number') &&
    caddyClientSource.includes('new CaddyApiError(response.statusCode, text)'),
  'Caddy failures must preserve the HTTP status code for exact 404 handling'
)

assert.ok(
  caddyClientSource.includes(
    'if (!(postError instanceof CaddyApiError) || postError.statusCode !== 404)'
  ) && caddyClientSource.includes('throw postError'),
  'non-404 route POST failures must be rethrown instead of triggering server creation'
)

assert.ok(
  caddyClientSource.includes("await this.request('POST', routesPath, route)") &&
    caddyClientSource.includes(
      "existingServer = await this.request<{ routes?: CaddyRoute[] }>('GET', serverPath)"
    ) &&
    caddyClientSource.includes('if (Array.isArray(existingServer.routes))') &&
    caddyClientSource.includes("await this.request('PUT', routesPath, [route])"),
  'route writes must use array append and re-read sites before any conditional creation'
)

assert.ok(
  caddyClientSource.indexOf(
    "existingServer = await this.request<{ routes?: CaddyRoute[] }>('GET', serverPath)"
  ) <
    caddyClientSource.indexOf("await this.request('PUT', serverPath, serverConfig)"),
  'sites must be read and confirmed missing before its server config is created'
)

assert.ok(
  caddyClientSource.includes("await this.request('GET', httpAppPath)") &&
    caddyClientSource.indexOf("await this.request('GET', httpAppPath)") <
      caddyClientSource.indexOf("await this.request('PUT', httpAppPath, {"),
  'the HTTP app must be read and confirmed missing before conditional creation'
)

for (const forbiddenPattern of [
  "this.request('PATCH', '/config/apps/http'",
  "postErrorMsg.includes('404')",
  'POST 失败，可能服务器不存在'
]) {
  assert.ok(
    !caddyClientSource.includes(forbiddenPattern),
    `Caddy route guard forbids unsafe fallback: ${forbiddenPattern}`
  )
}

// 安全模型：Caddy Admin 仅绑定宿主机回环（127.0.0.1:<port>），面板经
// Agent 反向隧道（HMAC 鉴权）访问，不再依赖公网端口、TLS 证书或管理凭据。
assert.ok(
  caddyClientSource.includes('hostTunnelManager.createDuplexStream(config.hostId, targetHost, port)') &&
    caddyClientSource.includes("config.hostId"),
  'Caddy management must reach the host Admin API through the authenticated Agent tunnel (hostId-scoped)'
)

// 隧道目标必须固定为回环地址，绝不允许面板指定任意公网目标。
assert.ok(
  caddyClientSource.includes("targetHost || '127.0.0.1'") &&
    caddyClientSource.includes('config.port || 2019'),
  'Caddy tunnel target must default to loopback 127.0.0.1 / admin port 2019, never a public address'
)

// 新架构零管理凭据：不再存在 Basic Auth 用户名/密码或公网 TLS 证书。
for (const forbidden of ['caddy_username', 'caddy_password', 'CADDY_CA_PATH', 'servername', 'basicauth']) {
  assert.ok(
    !caddyClientSource.includes(forbidden),
    `Caddy loopback-tunnel model forbids legacy credential/TLS markers: ${forbidden}`
  )
}

console.log('proxy site route ID guard tests passed')
