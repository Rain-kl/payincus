import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createHash, X509Certificate } from 'node:crypto'

export function getPanelCertificatePaths(): { certPath: string; keyPath: string } {
  const moduleDir = dirname(fileURLToPath(import.meta.url))
  const appDir = process.env.INCUDAL_APP_DIR || process.cwd()
  const installDir = process.env.INCUDAL_INSTALL_DIR || process.env.INSTALL_DIR ||
    (appDir.endsWith('/current') ? dirname(appDir) : null)
  const stableCertPath = installDir ? join(installDir, 'server/certs/client.crt') : null
  const stableKeyPath = installDir ? join(installDir, 'server/certs/client.key') : null
  return {
    certPath: process.env.PANEL_CRT_PATH || (stableCertPath && existsSync(stableCertPath) ? stableCertPath : join(moduleDir, '../../certs/client.crt')),
    keyPath: process.env.PANEL_KEY_PATH || (stableKeyPath && existsSync(stableKeyPath) ? stableKeyPath : join(moduleDir, '../../certs/client.key'))
  }
}

/** Return only the public certificate used by the panel's Incus client. */
export function readPanelClientCertificate(): { certificate: string; fingerprint: string } {
  const data = readFileSync(getPanelCertificatePaths().certPath, 'utf8')
  if (data.length > 65536 || !/^\s*-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----\s*$/.test(data)) {
    throw new Error('Expected one PEM public certificate without private key material')
  }
  const cert = new X509Certificate(data)
  const now = Date.now()
  if (Date.parse(cert.validFrom) > now || Date.parse(cert.validTo) < now) {
    throw new Error('Panel client certificate is not currently valid')
  }
  return {
    certificate: cert.toString(),
    fingerprint: createHash('sha256').update(cert.raw).digest('hex')
  }
}
