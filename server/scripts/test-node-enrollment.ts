import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createHash, X509Certificate } from 'node:crypto'
import { getPanelCertificatePaths, readPanelClientCertificate } from '../src/lib/panel-certificate.js'

const directory = mkdtempSync(join(tmpdir(), 'incus-enrollment-certificate-'))
const envKeys = ['PANEL_CRT_PATH', 'PANEL_KEY_PATH', 'INCUDAL_INSTALL_DIR', 'INSTALL_DIR', 'INCUDAL_APP_DIR'] as const
const previous = Object.fromEntries(envKeys.map(key => [key, process.env[key]]))
try {
  const certificatePath = join(directory, 'client.crt')
  const privateKeyPath = join(directory, 'client.key')
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=enrollment-fixture', '-keyout', privateKeyPath, '-out', certificatePath], { stdio: 'pipe' })
  process.env.PANEL_CRT_PATH = certificatePath
  process.env.PANEL_KEY_PATH = privateKeyPath
  assert.deepEqual(getPanelCertificatePaths(), { certPath: certificatePath, keyPath: privateKeyPath })
  const publicPem = readFileSync(certificatePath, 'utf8')
  const result = readPanelClientCertificate()
  assert.ok(result.certificate.includes('BEGIN CERTIFICATE'))
  assert.ok(!result.certificate.includes('PRIVATE KEY'))
  assert.equal(result.fingerprint, createHash('sha256').update(new X509Certificate(publicPem).raw).digest('hex'))
  writeFileSync(certificatePath, publicPem + readFileSync(privateKeyPath, 'utf8'))
  assert.throws(() => readPanelClientCertificate(), /without private key/)
  writeFileSync(certificatePath, 'invalid certificate')
  assert.throws(() => readPanelClientCertificate(), /Expected one PEM/)
  process.env.PANEL_CRT_PATH = join(directory, 'missing.crt')
  assert.throws(() => readPanelClientCertificate(), /ENOENT/)
  const source = readFileSync(new URL('../src/routes/agent.ts', import.meta.url), 'utf8')
  const start = source.indexOf("'/install-config/:token'")
  const end = source.indexOf("'/hosts/:hostId/status'", start)
  const route = source.slice(start, end)
  assert.ok(route.indexOf('panelCertificate = readPanelClientCertificate()') < route.indexOf('await consumeHostAgentInstallToken(token)'), 'unavailable certificate must not burn a token')
  assert.ok(route.includes('schemaVersion: 1') && route.includes('panelClientCertificate: panelCertificate.certificate'))
  assert.ok(route.includes('buildAgentInstallConfig({'), 'legacy text exchange must remain available')
  console.log('node enrollment public certificate and protocol guards passed')
} finally {
  for (const key of envKeys) {
    if (previous[key] === undefined) delete process.env[key]
    else process.env[key] = previous[key]
  }
  rmSync(directory, { recursive: true, force: true })
}
