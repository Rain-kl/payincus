import { createHash } from 'node:crypto'
import { NetworkInputError, object } from './policy.js'

export interface ImportedProxy { name: string; protocol: string; server: string; port: number; outbound: Record<string, any>; fingerprint: string }
const supported = ['socks', 'http', 'vless', 'vmess', 'trojan', 'shadowsocks']
function text(value: unknown, label: string, max = 1024): string {
  if (typeof value !== 'string' || !value.length || value.length > max || /[\x00-\x1f]/.test(value)) throw new NetworkInputError(`Invalid ${label}`)
  return value
}
function endpoint(address: unknown, port: unknown) {
  const server = text(address, 'server', 253).replace(/^\[|\]$/g, '')
  if (!/^[\w.:[\]-]+$/.test(server)) throw new NetworkInputError('Invalid proxy server')
  const n = Number(port)
  if (!Number.isInteger(n) || n < 1 || n > 65535) throw new NetworkInputError('Invalid proxy port')
  return { server, port: n }
}
function pickStrings(input: Record<string, any>, keys: string[]): Record<string, any> {
  const result: Record<string, any> = {}
  for (const key of keys) if (input[key] !== undefined && input[key] !== '') result[key] = text(input[key], key, 2048)
  return result
}
function streamSettings(value: unknown): Record<string, any> | undefined {
  if (value === undefined) return undefined
  const v = object(value), result: Record<string, any> = {}
  if (Object.keys(v).some(k => !['network', 'security', 'tlsSettings', 'realitySettings', 'wsSettings', 'grpcSettings', 'httpupgradeSettings', 'xhttpSettings', 'tcpSettings'].includes(k))) throw new NetworkInputError('Unsupported stream option (local files and socket overrides are not imported)')
  const network = v.network ?? 'tcp', security = v.security ?? 'none'
  if (!['tcp', 'raw', 'ws', 'grpc', 'httpupgrade', 'xhttp'].includes(network) || !['none', 'tls', 'reality'].includes(security)) throw new NetworkInputError('Unsupported Xray transport or security')
  result.network = network; result.security = security
  if (v.tcpSettings && Object.keys(object(v.tcpSettings)).length) throw new NetworkInputError('TCP header settings are not supported')
  if (security === 'tls') {
    const tls = object(v.tlsSettings ?? {})
    if (Object.keys(tls).some(k => !['serverName', 'fingerprint', 'alpn', 'allowInsecure'].includes(k))) throw new NetworkInputError('Unsupported TLS option')
    result.tlsSettings = pickStrings(tls, ['serverName', 'fingerprint'])
    if (tls.allowInsecure !== undefined) { if (typeof tls.allowInsecure !== 'boolean') throw new NetworkInputError('Invalid TLS verification setting'); result.tlsSettings.allowInsecure = tls.allowInsecure }
    if (tls.alpn !== undefined) { if (!Array.isArray(tls.alpn) || tls.alpn.length > 8) throw new NetworkInputError('Invalid ALPN'); result.tlsSettings.alpn = tls.alpn.map((s: unknown) => text(s, 'ALPN', 64)) }
  }
  if (security === 'reality') {
    const reality = object(v.realitySettings)
    if (Object.keys(reality).some(k => !['serverName', 'fingerprint', 'publicKey', 'shortId', 'spiderX', 'password'].includes(k))) throw new NetworkInputError('Unsupported Reality option')
    result.realitySettings = pickStrings(reality, ['serverName', 'fingerprint', 'publicKey', 'shortId', 'spiderX', 'password'])
    if (!reality.publicKey && !reality.password) throw new NetworkInputError('Reality public key is required')
  }
  for (const [transport, field, keys] of [['ws', 'wsSettings', ['path']], ['grpc', 'grpcSettings', ['serviceName', 'authority']], ['httpupgrade', 'httpupgradeSettings', ['path', 'host']], ['xhttp', 'xhttpSettings', ['path', 'host', 'mode']]] as const) {
    if (network !== transport) continue
    const input = object(v[field] ?? {})
    if (Object.keys(input).some(k => !(keys as readonly string[]).includes(k) && !(transport === 'ws' && k === 'headers'))) throw new NetworkInputError(`Unsupported ${transport} option`)
    result[field] = pickStrings(input, [...keys])
    if (transport === 'ws' && input.headers) {
      const headers = object(input.headers)
      if (Object.keys(headers).some(k => k.toLowerCase() !== 'host')) throw new NetworkInputError('Only the WebSocket Host header is supported')
      result[field].headers = Object.fromEntries(Object.entries(headers).map(([k, val]) => [k, text(val, 'Host header', 253)]))
    }
  }
  return result
}
function stable(value: any): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(',')}]`
  if (value && typeof value === 'object') return `{${Object.keys(value).sort().map(k => `${JSON.stringify(k)}:${stable(value[k])}`).join(',')}}`
  return JSON.stringify(value)
}
export function normalizeOutbound(value: unknown, name?: string): ImportedProxy {
  const v = object(value), protocol = v.protocol
  if (!supported.includes(protocol)) throw new NetworkInputError('Unsupported proxy protocol')
  if (Object.keys(v).some(k => !['tag', 'protocol', 'settings', 'streamSettings'].includes(k))) throw new NetworkInputError('Unsupported outbound option')
  const s = object(v.settings)
  const source = ['vless', 'vmess'].includes(protocol) ? s.vnext : s.servers
  if (!Array.isArray(source) || source.length !== 1) throw new NetworkInputError('Each outbound must contain exactly one server')
  const entry = object(source[0]), ep = endpoint(entry.address, entry.port)
  const server: Record<string, any> = { address: ep.server, port: ep.port }
  if (['vless', 'vmess'].includes(protocol)) {
    if (!Array.isArray(entry.users) || entry.users.length !== 1) throw new NetworkInputError('Exactly one proxy credential is required')
    const user = object(entry.users[0]), id = text(user.id, 'UUID', 64)
    if (!/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i.test(id)) throw new NetworkInputError('Invalid proxy UUID')
    if (protocol === 'vless' && user.encryption && user.encryption !== 'none') throw new NetworkInputError('Unsupported VLESS encryption')
    if (protocol === 'vmess' && user.alterId && user.alterId !== 0) throw new NetworkInputError('Legacy VMess alterId is unsupported')
    server.users = [{ id, ...(protocol === 'vless' ? { encryption: 'none', ...(user.flow ? { flow: text(user.flow, 'flow', 64) } : {}) } : { security: user.security ?? 'auto', alterId: 0 }) }]
  } else if (['trojan', 'shadowsocks'].includes(protocol)) {
    server.password = text(entry.password, 'password')
    if (protocol === 'shadowsocks') {
      if (!['aes-128-gcm', 'aes-256-gcm', 'chacha20-poly1305', 'chacha20-ietf-poly1305', '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305'].includes(entry.method)) throw new NetworkInputError('Unsupported Shadowsocks cipher')
      server.method = entry.method
    }
  } else if (entry.users !== undefined) {
    if (!Array.isArray(entry.users) || entry.users.length !== 1) throw new NetworkInputError('Exactly one proxy credential is supported')
    const user = object(entry.users[0]); server.users = [{ user: text(user.user, 'username', 255), pass: typeof user.pass === 'string' && user.pass.length <= 1024 ? user.pass : text(user.pass, 'password') }]
  }
  // Reject options we cannot preserve instead of silently changing transport/authentication.
  const allowed = ['address', 'port', ...(['vless', 'vmess', 'socks', 'http'].includes(protocol) ? ['users'] : protocol === 'trojan' ? ['password'] : ['password', 'method'])]
  if (Object.keys(entry).some(k => !allowed.includes(k)) || Object.keys(s).some(k => !(['vless', 'vmess'].includes(protocol) ? ['vnext'] : ['servers']).includes(k))) throw new NetworkInputError('Unsupported proxy settings')
  const stream = streamSettings(v.streamSettings)
  const outbound = { protocol, settings: { [['vless', 'vmess'].includes(protocol) ? 'vnext' : 'servers']: [server] }, ...(stream ? { streamSettings: stream } : {}) }
  if (JSON.stringify(outbound).length > 8192) throw new NetworkInputError('Proxy configuration is too large')
  return { name: text(name || v.tag || `${protocol} ${ep.server}:${ep.port}`, 'name', 200), protocol, ...ep, outbound, fingerprint: createHash('sha256').update(stable(outbound)).digest('hex') }
}
function singbox(value: unknown): ImportedProxy {
  const v = object(value)
  if (Object.keys(v).some(k => !['type', 'tag', 'server', 'server_port', 'username', 'password', 'uuid', 'flow', 'security', 'alter_id', 'method', 'tls', 'transport', 'version', 'packet_encoding'].includes(k))) throw new NetworkInputError('Unsupported sing-box outbound option')
  const protocol = v.type === 'socks' ? 'socks' : v.type
  if (v.type === 'socks' && v.version && v.version !== '5') throw new NetworkInputError('Only SOCKS5 is supported')
  if (v.packet_encoding) throw new NetworkInputError('Unsupported sing-box packet encoding')
  const server: Record<string, any> = { address: v.server, port: v.server_port }
  if (['vless', 'vmess'].includes(protocol)) server.users = [{ id: v.uuid, flow: v.flow, security: v.security, alterId: v.alter_id }]
  else if (['socks', 'http'].includes(protocol)) { if (v.username) server.users = [{ user: v.username, pass: v.password ?? '' }] }
  else { server.password = v.password; if (protocol === 'shadowsocks') server.method = v.method }
  const stream: Record<string, any> = { network: 'tcp', security: 'none' }
  if (v.tls?.enabled) {
    const tls = object(v.tls)
    if (Object.keys(tls).some(k => !['enabled', 'server_name', 'insecure', 'alpn', 'utls', 'reality'].includes(k))) throw new NetworkInputError('Unsupported sing-box TLS option')
    stream.security = tls.reality?.enabled ? 'reality' : 'tls'
    const common = { serverName: tls.server_name, fingerprint: tls.utls?.enabled ? tls.utls.fingerprint : undefined }
    stream[stream.security === 'reality' ? 'realitySettings' : 'tlsSettings'] = stream.security === 'reality' ? { ...common, publicKey: tls.reality.public_key, shortId: tls.reality.short_id } : { ...common, allowInsecure: tls.insecure, alpn: tls.alpn }
  }
  if (v.transport) {
    const tr = object(v.transport)
    if (tr.type === 'ws' && Object.keys(tr).every(k => ['type', 'path', 'headers'].includes(k))) { stream.network = 'ws'; stream.wsSettings = { path: tr.path, headers: tr.headers } }
    else if (tr.type === 'grpc' && Object.keys(tr).every(k => ['type', 'service_name'].includes(k))) { stream.network = 'grpc'; stream.grpcSettings = { serviceName: tr.service_name } }
    else if (tr.type === 'httpupgrade' && Object.keys(tr).every(k => ['type', 'path', 'host'].includes(k))) { stream.network = 'httpupgrade'; stream.httpupgradeSettings = { path: tr.path, host: tr.host } }
    else throw new NetworkInputError('Unsupported sing-box transport')
  }
  // JSON removes absent optional fields before applying the strict Xray validator.
  return normalizeOutbound(JSON.parse(JSON.stringify({ protocol, tag: v.tag, settings: { [['vless', 'vmess'].includes(protocol) ? 'vnext' : 'servers']: [server] }, streamSettings: stream })))
}
function decode64(value: string): string {
  if (!/^[A-Za-z0-9+/_=\s-]+$/.test(value)) throw new NetworkInputError('Invalid base64 data')
  return Buffer.from(value.trim(), 'base64url').toString('utf8')
}
function uri(input: string): ImportedProxy {
  if (input.startsWith('vmess://')) {
    const v = object(JSON.parse(decode64(input.slice(8))))
    if (v.net && !['tcp', 'ws', 'grpc'].includes(v.net)) throw new NetworkInputError('Unsupported VMess transport')
    if (v.type && v.type !== 'none') throw new NetworkInputError('Unsupported VMess header type')
    return normalizeOutbound({ protocol: 'vmess', tag: v.ps, settings: { vnext: [{ address: v.add, port: v.port, users: [{ id: v.id, alterId: Number(v.aid ?? 0), security: v.scy ?? 'auto' }] }] }, streamSettings: { network: v.net ?? 'tcp', security: v.tls === 'tls' ? 'tls' : 'none', ...(v.tls === 'tls' ? { tlsSettings: { serverName: v.sni || v.host || v.add } } : {}), ...(v.net === 'ws' ? { wsSettings: { path: v.path || '/', headers: v.host ? { Host: v.host } : {} } } : {}), ...(v.net === 'grpc' ? { grpcSettings: { serviceName: v.path || '' } } : {}) } })
  }
  let value = input
  if (value.startsWith('ss://') && !value.slice(5).split('#')[0].includes('@')) { const [encoded, name] = value.slice(5).split('#'); value = `ss://${decode64(encoded)}${name ? `#${name}` : ''}` }
  const u = new URL(value), protocol = ({ 'socks5:': 'socks', 'socks:': 'socks', 'https:': 'http', 'ss:': 'shadowsocks' } as Record<string, string>)[u.protocol] ?? u.protocol.slice(0, -1)
  if (!supported.includes(protocol)) throw new NetworkInputError('Unsupported URI protocol')
  const q = u.searchParams
  for (const key of q.keys()) if (!['type', 'security', 'sni', 'peer', 'fp', 'pbk', 'sid', 'spx', 'alpn', 'host', 'path', 'serviceName', 'mode', 'flow', 'encryption', 'allowInsecure'].includes(key)) throw new NetworkInputError(`Unsupported URI parameter: ${key.replace(/[^\w]/g, '').slice(0, 40)}`)
  const server: Record<string, any> = { address: u.hostname, port: u.port || (u.protocol === 'https:' ? 443 : protocol === 'http' ? 80 : 1080) }
  if (['vless', 'vmess'].includes(protocol)) server.users = [{ id: decodeURIComponent(u.username), encryption: q.get('encryption') ?? 'none', ...(q.get('flow') ? { flow: q.get('flow') } : {}) }]
  else if (protocol === 'trojan') server.password = decodeURIComponent(u.username)
  else if (protocol === 'shadowsocks') {
    const auth = u.password ? `${decodeURIComponent(u.username)}:${decodeURIComponent(u.password)}` : decode64(decodeURIComponent(u.username))
    const i = auth.indexOf(':'); if (i < 1) throw new NetworkInputError('Invalid Shadowsocks credential'); server.method = auth.slice(0, i); server.password = auth.slice(i + 1)
  } else if (u.username) server.users = [{ user: decodeURIComponent(u.username), pass: decodeURIComponent(u.password) }]
  const network = q.get('type') || 'tcp', security = q.get('security') || (protocol === 'trojan' || u.protocol === 'https:' ? 'tls' : 'none')
  const stream: Record<string, any> = { network, security }
  if (security === 'tls') stream.tlsSettings = { serverName: q.get('sni') || q.get('peer') || u.hostname, ...(q.get('fp') ? { fingerprint: q.get('fp') } : {}), ...(q.get('alpn') ? { alpn: q.get('alpn')!.split(',') } : {}), allowInsecure: q.get('allowInsecure') === '1' }
  if (security === 'reality') stream.realitySettings = { serverName: q.get('sni') || u.hostname, fingerprint: q.get('fp') || 'chrome', publicKey: q.get('pbk') || '', ...(q.get('sid') ? { shortId: q.get('sid') } : {}), ...(q.get('spx') ? { spiderX: q.get('spx') } : {}) }
  if (network === 'ws') stream.wsSettings = { path: q.get('path') || '/', ...(q.get('host') ? { headers: { Host: q.get('host') } } : {}) }
  if (network === 'grpc') stream.grpcSettings = { ...(q.get('serviceName') ? { serviceName: q.get('serviceName') } : {}) }
  if (network === 'httpupgrade' || network === 'xhttp') stream[`${network}Settings`] = { path: q.get('path') || '/', ...(q.get('host') ? { host: q.get('host') } : {}), ...(network === 'xhttp' && q.get('mode') ? { mode: q.get('mode') } : {}) }
  return normalizeOutbound({ protocol, ...(u.hash ? { tag: decodeURIComponent(u.hash.slice(1)) } : {}), settings: { [['vless', 'vmess'].includes(protocol) ? 'vnext' : 'servers']: [server] }, streamSettings: stream })
}
export function parseProxyImport(input: string): { nodes: ImportedProxy[]; errors: Array<{ index: number; error: string }> } {
  if (Buffer.byteLength(input) > 512 * 1024) throw new NetworkInputError('Import exceeds 512 KiB')
  let content = input.trim(), entries: unknown[]
  if (!content) throw new NetworkInputError('Import is empty')
  if (!/^[{[]/.test(content) && !content.includes('://')) content = decode64(content).trim()
  if (/^[{[]/.test(content)) {
    let parsed: any
    try { parsed = JSON.parse(content) } catch { throw new NetworkInputError('Invalid configuration JSON') }
    entries = Array.isArray(parsed) ? parsed : parsed.outbounds ?? [parsed]
  } else entries = content.split(/\r?\n/).map(s => s.trim()).filter(s => s && !s.startsWith('#'))
  if (!Array.isArray(entries) || entries.length > 200) throw new NetworkInputError('Import must contain at most 200 nodes')
  const nodes: ImportedProxy[] = [], errors: Array<{ index: number; error: string }> = []
  for (let i = 0; i < entries.length; i++) {
    try { const item = entries[i]; nodes.push(typeof item === 'string' ? uri(item) : object(item).type ? singbox(item) : normalizeOutbound(item)) }
    catch (e) { errors.push({ index: i + 1, error: e instanceof NetworkInputError ? e.message : 'Invalid node configuration' }) }
  }
  return { nodes, errors }
}
