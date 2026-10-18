import tls from 'node:tls'
import net from 'node:net'
import { readFileSync } from 'node:fs'
import WebSocket from 'ws'
import { request } from 'undici'
import { setTimeout as delay } from 'node:timers/promises'
import { getIncusClient } from './incus-pool.js'
import { hostTunnelManager } from './tunnel-manager.js'
import type { Host } from '../../types/database.js'

export class SpiceConnectionError extends Error {
  constructor(public readonly stage: 'create' | 'metadata' | 'control' | 'cleanup', public readonly httpStatus?: number) {
    super(`Incus console ${stage} failed`)
  }
}

export interface SpiceBackend {
  control: WebSocket
  openData(signal: AbortSignal): Promise<WebSocket>
  close(): Promise<void>
}

/** Each SPICE channel reopens fd 0 of ONE console operation, like ConsoleInstanceDynamic. */
export async function createSpiceBackend(host: Host, instanceName: string, signal: AbortSignal, reconnect = false): Promise<SpiceBackend> {
  const client = await getIncusClient(host)
  signal.throwIfAborted()
  if (!client.agent || !client.certPath || !client.keyPath) throw new Error('Incus connection unavailable')
  const response = await request(`${client.baseUrl}/1.0/instances/${encodeURIComponent(instanceName)}/console`, {
    method: 'POST', dispatcher: client.agent, signal,
    headers: { 'Content-Type': 'application/json' },
    // Incus performs the takeover atomically for this VM, including sessions left by an old panel process.
    body: JSON.stringify({ type: 'vga', 'wait-for-websocket': true, force: reconnect }),
    headersTimeout: 15_000, bodyTimeout: 15_000
  })
  const result = await response.body.json() as {
    type: string; metadata?: { id?: string; metadata?: { fds?: Record<string, string> } }
  }
  const operationId = result.metadata?.id
  const fds = result.metadata?.metadata?.fds
  if (response.statusCode >= 400 || result.type !== 'async' || !operationId) {
    // Do not log Incus responses: operation metadata contains channel secrets.
    throw new SpiceConnectionError('create', response.statusCode)
  }
  const sockets = new Set<WebSocket>()
  let closed = false
  let cleanup: Promise<void> | undefined
  const close = (): Promise<void> => {
    if (cleanup) return cleanup
    closed = true
    for (const socket of sockets) socket.terminate()
    sockets.clear()
    // Closing the websocket alone can leave Incus' VGA operation holding the SPICE socket.
    // Cancellation must use its own signal: the setup/session signal is already aborted.
    cleanup = (async () => {
      const cleanupSignal = AbortSignal.timeout(5000)
      const operationUrl = `${client.baseUrl}/1.0/operations/${encodeURIComponent(operationId)}`
      const cancellation = await request(operationUrl, {
        method: 'DELETE', dispatcher: client.agent!, signal: cleanupSignal,
        headersTimeout: 5000, bodyTimeout: 5000
      })
      await cancellation.body.dump()
      if (cancellation.statusCode === 404) return
      // A normal socket close can finish the operation before cancellation reaches Incus.
      if (cancellation.statusCode >= 400 && ![400, 409].includes(cancellation.statusCode)) {
        console.warn('[SPICE] Console cancellation rejected', { instanceId: instanceName, httpStatus: cancellation.statusCode })
        throw new SpiceConnectionError('cleanup', cancellation.statusCode)
      }
      while (true) {
        const state = await request(operationUrl, {
          method: 'GET', dispatcher: client.agent!, signal: cleanupSignal,
          headersTimeout: 5000, bodyTimeout: 5000
        })
        if (state.statusCode === 404) { await state.body.dump(); return }
        const operation = await state.body.json() as { metadata?: { status?: string } }
        if (state.statusCode >= 400) throw new SpiceConnectionError('cleanup', state.statusCode)
        if (['Success', 'Failure', 'Cancelled'].includes(operation.metadata?.status || '')) return
        await delay(100, undefined, { signal: cleanupSignal })
      }
    })().catch(() => {
      // Never propagate transport errors containing fd secrets or URLs.
      cleanup = undefined // A later reconnect may retry cleanup after a transient transport failure.
      throw new SpiceConnectionError('cleanup')
    })
    return cleanup
  }
  let cert: Buffer
  let key: Buffer
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
    if (!fds?.['0'] || !fds.control) throw new SpiceConnectionError('metadata')
    cert = readFileSync(client.certPath)
    key = readFileSync(client.keyPath)
    const control = await open(fds.control, signal)
    return { control, openData: abortSignal => open(fds['0'], abortSignal), close }
  } catch (error) {
    await close()
    throw error instanceof SpiceConnectionError ? error : new SpiceConnectionError('control')
  }
}
