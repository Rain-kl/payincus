import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { parseProxyImport } from '../src/lib/egress/import.js'
import { cidr, defaultEgress, defaultFirewall, validateEgress, validateFirewall } from '../src/lib/egress/policy.js'

const uuid = '00000000-0000-4000-8000-000000000001'
const input = `socks5://user:password@example.com:1080#Socks\nvless://${uuid}@example.com:443?security=tls&type=ws&host=example.com&path=%2Fproxy#Vless`
const parsed = parseProxyImport(input)
assert.equal(parsed.nodes.length, 2); assert.deepEqual(parsed.errors, [])
assert.equal(parsed.nodes[0].outbound.settings.servers[0].users[0].pass, 'password')
assert.equal(parsed.nodes[1].outbound.streamSettings.wsSettings.path, '/proxy')
assert.equal(parseProxyImport(Buffer.from(input).toString('base64')).nodes.length, 2)
assert.equal(parseProxyImport('socks5://example.com:1080').nodes[0].fingerprint, parseProxyImport('socks5://example.com:1080#DifferentName').nodes[0].fingerprint)
const raw = parseProxyImport(JSON.stringify({ outbounds: [{ protocol: 'socks', tag: 'native', settings: { servers: [{ address: 'example.com', port: 1080 }] } }, { protocol: 'freedom' }] }))
assert.equal(raw.nodes.length, 1); assert.equal(raw.errors.length, 1)
const sb = parseProxyImport(JSON.stringify({ outbounds: [{ type: 'vless', tag: 'converted', server: 'example.com', server_port: 443, uuid, tls: { enabled: true, server_name: 'example.com' }, transport: { type: 'ws', path: '/ws' } }, { type: 'hysteria2', server: 'example.com' }, { type: 'socks', server: 'example.com', server_port: 1080, detour: 'another' }] }))
assert.equal(sb.nodes.length, 1); assert.equal(sb.errors.length, 2)
assert.equal(sb.nodes[0].outbound.streamSettings.security, 'tls')
assert.equal(parseProxyImport('ss://' + Buffer.from('aes-128-gcm:secret').toString('base64') + '@example.com:8388#SS').nodes.length, 1)
assert.equal(parseProxyImport(`vless://${uuid}@example.com:443?security=reality&pbk=public-key&sni=example.com`).nodes.length, 1)
const dangerous = parseProxyImport(JSON.stringify({ protocol: 'socks', settings: { servers: [{ address: 'example.com', port: 1080 }] }, streamSettings: { sockopt: { dialerProxy: 'direct' } } }))
assert.equal(dangerous.nodes.length, 0)
assert.equal(parseProxyImport('socks5://user:do-not-echo@example.com:99999').errors[0].error.includes('do-not-echo'), false)
assert.throws(() => parseProxyImport('a'.repeat(524289)))
assert.throws(() => cidr('0.0.0.0/33')); assert.throws(() => cidr('::/129')); assert.equal(cidr('::/0'), '::/0')
assert.throws(() => validateEgress({ ...defaultEgress, mode: 'proxy' }))
assert.throws(() => validateEgress({ ...defaultEgress, proxyNodeIds: [1, 1] }))
assert.throws(() => validateEgress({ ...defaultEgress, probeUrl: 'http://example.com' }))
assert.deepEqual(validateFirewall(defaultFirewall), defaultFirewall)
const rule = { direction: 'egress', action: 'drop', protocol: 'tcp', source: '', destination: '203.0.113.0/24', sourcePort: '', destinationPort: '25,465,1000-2000', description: 'block', enabled: true }
assert.equal(validateFirewall({ ...defaultFirewall, enabled: true, rules: [rule] }).rules.length, 1)
for (const patch of [{ destinationPort: '65536' }, { destinationPort: '100-1' }, { protocol: 'icmp4' }, { source: '::/0' }, { destination: '1.2.3.4; drop' }]) assert.throws(() => validateFirewall({ ...defaultFirewall, rules: [{ ...rule, ...patch }] }))
const route = readFileSync(new URL('../src/routes/egress.ts', import.meta.url), 'utf8')
assert.ok(route.includes('const admin = [app.authenticate, app.requireAdmin]'))
assert.ok(route.includes('revision: { type: \'integer\''))
assert.ok(route.includes('pg_advisory_xact_lock(74201)'))
assert.ok(!route.includes('snapshotEncrypted: true'))
const provision = readFileSync(new URL('../src/lib/egress/provision.ts', import.meta.url), 'utf8')
assert.ok(provision.includes('policy.appliedRevision !== policy.revision'))
assert.ok(provision.includes('device.host_name = `iceg'))
console.log('egress import, validation, secret boundaries and provisioning guards passed')
