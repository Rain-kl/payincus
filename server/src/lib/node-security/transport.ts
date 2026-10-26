import tls from 'node:tls'
import net from 'node:net'
import { timingSafeEqual } from 'node:crypto'
import type { ConnectionOptions } from 'node:tls'
import type { IncusClientOptions } from '../../types/incus.js'
import { hostTunnelManager } from '../incus/tunnel-manager.js'

// A trusted terminal supplies this pin; an unverified network response never enrolls it.
export function connectPinned(options: IncusClientOptions, credentials: Pick<ConnectionOptions, 'cert' | 'key'>, callback: (error: Error | null, socket: any) => void): void {
  const expected = options.serverFingerprint || ''
  if (!/^[0-9a-f]{64}$/.test(expected)) { callback(new Error('Invalid node identity'), null); return }
  let socket: tls.TLSSocket | undefined
  let done = false
  const finish = (error: Error | null) => {
    if (done) return
    done = true
    if (error) socket?.destroy()
    else socket?.setTimeout(0)
    callback(error, error ? null : socket)
  }
  try {
    const url = new URL(options.url)
    if (url.protocol !== 'https:') throw new Error('HTTPS required')
    const host = url.hostname.replace(/^\[|\]$/g, '')
    const transport = options.tunnelEnabled && options.hostId
      ? { socket: hostTunnelManager.createDuplexStream(options.hostId, options.targetHost || '127.0.0.1', options.targetPort || 8443) }
      : { host, port: Number(url.port || 443) }
    socket = tls.connect({ ...transport, ...credentials, rejectUnauthorized: false, minVersion: 'TLSv1.2', servername: net.isIP(host) ? undefined : host })
    socket.setTimeout(10_000, () => finish(new Error('Node connection timed out')))
    socket.once('error', () => finish(new Error('Node connection failed')))
    socket.once('secureConnect', () => {
      const actual = socket?.getPeerCertificate().fingerprint256?.replaceAll(':', '').toLowerCase() || ''
      if (!/^[0-9a-f]{64}$/.test(actual) || !timingSafeEqual(Buffer.from(actual, 'hex'), Buffer.from(expected, 'hex'))) { finish(new Error('Node identity mismatch')); return }
      // No HTTP headers or secret body can be sent until the pin has matched.
      finish(null)
    })
  } catch { finish(new Error('Node connection failed')) }
}
