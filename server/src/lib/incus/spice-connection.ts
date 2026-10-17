import tls from 'node:tls'
import net from 'node:net'
import { readFileSync } from 'node:fs'
import WebSocket from 'ws'
import { request } from 'undici'
import { getIncusClient } from './incus-pool.js'
import { hostTunnelManager } from './tunnel-manager.js'
import type { Host } from '../../types/database.js'

export interface SpiceBackend {
  control: WebSocket
  openData(signal: AbortSignal): Promise<WebSocket>
  close(): void
}

/** Each SPICE channel reopens fd 0 of ONE console operation, like ConsoleInstanceDynamic. */
export async function createSpiceBackend(host: Host, instanceName: string, signal: AbortSignal): Promise<SpiceBackend> {
  const client = await getIncusClient(host)
  signal.throwIfAborted()
  if (!client.agent || !client.certPath || !client.keyPath) throw new Error('Incus connection unavailable')
  const response = await request(`${client.baseUrl}/1.0/instances/${encodeURIComponent(instanceName)}/console`, {
    method: 'POST', dispatcher: client.agent, signal,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ type: 'vga', 'wait-for-websocket': true }),
    headersTimeout: 15_000, bodyTimeout: 15_000
  })
  const result = await response.body.json() as {
    type: string; metadata?: { id?: string; metadata?: { fds?: Record<string, string> } }
  }
  const operationId = result.metadata?.id
  const fds = result.metadata?.metadata?.fds
  if (response.statusCode >= 400 || result.type !== 'async' || !operationId || !fds?.['0'] || !fds.control) {
    // Do not log Incus responses: operation metadata contains channel secrets.
    throw new Error(`Incus VGA console unavailable (HTTP ${response.statusCode})`)
  }
  const cert = readFileSync(client.certPath)
  const key = readFileSync(client.keyPath)
  const sockets = new Set<WebSocket>()
  let closed = false
  const close = () => {
    if (closed) return
    closed = true
    for (const socket of sockets) socket.terminate()
    sockets.clear()
  }
  const open = (secret: string, abortSignal: AbortSignal): Promise<WebSocket> => {
    abortSignal.throwIfAborted()
    if (closed) throw new Error('Console closed')
    const url = new URL(`/1.0/operations/${encodeURIComponent(operationId)}/websocket`, client.baseUrl)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    url.searchParams.set('secret', secret)
    const options: WebSocket.ClientOptions = { cert, key, rejectUnauthorized: false, handshakeTimeout: 15_000, maxPayload: 16 * 1024 * 1024 }
    if (client.options.tunnelEnabled) {
      options.createConnection = () => {
        const duplex = hostTunnelManager.createDuplexStream(host.id, client.options.targetHost || '127.0.0.1', client.options.targetPort || 8443)
        return tls.connect({ socket: duplex, cert, key, rejectUnauthorized: false,
          servername: net.isIP(url.hostname) ? undefined : url.hostname })
      }
    }
    const socket = new WebSocket(url, options)
    sockets.add(socket)
    socket.on('error', () => {}) // Errors must never print the secret-bearing URL.
    socket.once('close', () => sockets.delete(socket))
    return new Promise((resolve, reject) => {
      const abort = () => { socket.terminate(); finish(new Error('Console connection aborted')) }
      const fail = () => finish(new Error('Console channel unavailable'))
      const finish = (error?: Error) => {
        abortSignal.removeEventListener('abort', abort)
        socket.off('error', fail)
        socket.off('close', fail)
        if (error) reject(error)
        else resolve(socket)
      }
      socket.once('open', () => finish())
      socket.once('error', fail)
      socket.once('close', fail)
      abortSignal.addEventListener('abort', abort, { once: true })
      if (abortSignal.aborted) abort()
    })
  }
  try {
    const control = await open(fds.control, signal)
    return { control, openData: abortSignal => open(fds['0'], abortSignal), close }
  } catch (error) {
    close()
    throw error
  }
}
