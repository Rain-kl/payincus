import { nanoid } from 'nanoid'
import WebSocket, { type RawData } from 'ws'
import type { SpiceBackend } from './incus/spice-connection.js'

const MAX_BUFFER = 8 * 1024 * 1024
const MAX_CHANNELS = 12
const SESSION_LIFETIME = 2 * 60 * 60 * 1000

export interface SpiceAccess {
  userId: number
  instanceId: number
  issuedAt: number
  sessionId?: string
  hostId?: number
  instanceName?: string
}

interface SpiceSession extends SpiceAccess {
  token: string
  createdAt: number
  backend?: SpiceBackend
  opening?: Promise<SpiceBackend>
  abort: AbortController
  channels: Set<WebSocket>
  main?: WebSocket
  timer: NodeJS.Timeout
  checking: boolean
  controlAlive: boolean
  validate(): Promise<boolean>
}

/** Bounded, in-memory capabilities. Never expose Incus operation IDs or fd secrets. */
export class SpiceSessionManager {
  private sessions = new Map<string, SpiceSession>()
  private closing = new Map<number, Promise<void>>()
  private failedCleanup = new Map<number, SpiceBackend>()

  async waitForCleanup(instanceId: number): Promise<void> {
    await this.closing.get(instanceId)
    const failed = this.failedCleanup.get(instanceId)
    if (failed) {
      await failed.close()
      this.failedCleanup.delete(instanceId)
    }
  }

  async create(access: SpiceAccess, open: (signal: AbortSignal) => Promise<SpiceBackend>, validate: () => Promise<boolean>): Promise<string> {
    if (this.closing.has(access.instanceId) || this.failedCleanup.has(access.instanceId)) {
      await this.waitForCleanup(access.instanceId)
    }
    const all = [...this.sessions.values()]
    const reservedInstances = new Set([...all.map(s => s.instanceId), ...this.closing.keys(), ...this.failedCleanup.keys()])
    if (reservedInstances.size >= 100 || all.filter(s => s.userId === access.userId).length >= 2 || all.some(s => s.instanceId === access.instanceId)) {
      throw new Error('CONSOLE_LIMIT')
    }
    const token = nanoid(48)
    const session: SpiceSession = {
      ...access, token, createdAt: Date.now(), abort: new AbortController(), channels: new Set(),
      checking: false, controlAlive: true, validate,
      timer: setInterval(() => { void this.check(session) }, 25_000).unref()
    }
    // Reserve BEFORE asynchronous setup; pending sessions count towards both limits.
    this.sessions.set(token, session)
    const timeout = setTimeout(() => this.close(token), 20_000).unref()
    try {
      const opening = session.opening = open(session.abort.signal)
      // A pooled client's initial connect may not support AbortSignal. Bound the caller,
      // and close a backend that finishes after cancellation instead of retaining it.
      session.backend = await Promise.race([
        opening,
        new Promise<never>((_, reject) => session.abort.signal.addEventListener('abort',
          () => reject(new Error('Console setup aborted')), { once: true }))
      ])
      if (!this.sessions.has(token)) throw new Error('Console closed during setup')
      if (session.backend.control.readyState !== WebSocket.OPEN) throw new Error('Console control channel closed during setup')
      session.backend.control.on('close', () => this.close(token))
      session.backend.control.on('error', () => this.close(token))
      session.backend.control.on('pong', () => { session.controlAlive = true })
      return token
    } catch (error) {
      this.close(token)
      throw error
    } finally {
      clearTimeout(timeout)
    }
  }

  get(token: string, instanceId: number): SpiceAccess | undefined {
    const session = this.sessions.get(token)
    if (!session || session.instanceId !== instanceId || Date.now() - session.createdAt > SESSION_LIFETIME) return
    return session
  }

  /** Install message/close handlers synchronously, before DB permission checks or backend I/O. */
  attach(token: string, instanceId: number, socket: WebSocket, authorize: (main: boolean) => Promise<boolean>): void {
    const session = this.sessions.get(token)
    if (!session || !this.get(token, instanceId) || !session.backend || session.channels.size >= MAX_CHANNELS) {
      socket.close(4003, 'Console session unavailable')
      return
    }
    const isMain = !session.main
    if (isMain) session.main = socket
    session.channels.add(socket)
    let backend: WebSocket | undefined
    let pending: RawData[] = []
    let pendingBytes = 0
    let closed = false
    let alive = true
    const abort = new AbortController()
    const cleanup = () => {
      if (closed) return
      closed = true
      abort.abort()
      clearInterval(heartbeat)
      clearTimeout(timeout)
      pending = []
      session.channels.delete(socket)
      backend?.terminate()
      if (isMain) this.close(token)
    }
    const terminate = () => { socket.terminate(); cleanup() }
    const timeout = setTimeout(terminate, 20_000).unref()
    const heartbeat = setInterval(() => {
      if (!alive || socket.bufferedAmount > MAX_BUFFER || (backend?.bufferedAmount || 0) > MAX_BUFFER) return terminate()
      alive = false
      socket.ping()
      backend?.ping()
    }, 25_000).unref()
    socket.on('pong', () => { alive = true })
    socket.once('close', cleanup)
    socket.once('error', terminate)
    socket.on('message', (data, binary) => {
      const size = Array.isArray(data) ? data.reduce((n, b) => n + b.length, 0) : data.byteLength
      if (!binary || size > MAX_BUFFER) return terminate()
      if (!backend) {
        pendingBytes += size
        if (pendingBytes > MAX_BUFFER) return terminate()
        pending.push(data)
      } else if (backend.readyState === WebSocket.OPEN && backend.bufferedAmount < MAX_BUFFER) {
        backend.send(data, { binary: true }, error => { if (error) terminate() })
      } else terminate()
    })
    void (async () => {
      try {
        if (!await authorize(isMain) || closed || !this.sessions.has(token)) return terminate()
        const upstream = await session.backend!.openData(abort.signal)
        if (closed || !this.sessions.has(token) || socket.readyState !== WebSocket.OPEN) { upstream.terminate(); return terminate() }
        backend = upstream
        backend.on('message', (data, binary) => {
          if (!binary || socket.readyState !== WebSocket.OPEN || socket.bufferedAmount > MAX_BUFFER) return terminate()
          socket.send(data, { binary: true }, error => { if (error) terminate() })
        })
        backend.once('close', terminate)
        backend.once('error', terminate)
        for (const data of pending) backend.send(data, { binary: true })
        pending = []
        clearTimeout(timeout)
      } catch {
        console.warn('[SPICE] Console data channel failed', { instanceId })
        terminate()
      }
    })()
  }

  private async check(session: SpiceSession): Promise<void> {
    if (Date.now() - session.createdAt > SESSION_LIFETIME || (!session.main && Date.now() - session.createdAt > 60_000)) {
      return this.close(session.token)
    }
    if (session.checking) return
    session.checking = true
    try {
      if (!session.controlAlive || !await session.validate()) this.close(session.token)
      else {
        session.controlAlive = false
        session.backend?.control.ping()
      }
    } catch { this.close(session.token) }
    finally { session.checking = false }
  }

  close(token: string): Promise<void> {
    const session = this.sessions.get(token)
    if (!session) return Promise.resolve()
    this.sessions.delete(token)
    clearInterval(session.timer)
    session.abort.abort()
    for (const socket of session.channels) {
      socket.close(1000, 'Console closed')
      const force = setTimeout(() => socket.terminate(), 2000).unref()
      socket.once('close', () => clearTimeout(force))
    }
    const cleanup = Promise.resolve().then(async () => {
      // Keep the instance reserved until even a late setup result has been disposed.
      let backend = session.backend
      if (!backend && session.opening) {
        try { backend = await session.opening } catch { return }
      }
      if (!backend) return
      try {
        await backend.close()
      } catch {
        this.failedCleanup.set(session.instanceId, backend)
        console.warn('[SPICE] Console cleanup failed', { instanceId: session.instanceId })
      }
    }).finally(() => {
      if (this.closing.get(session.instanceId) === cleanup) this.closing.delete(session.instanceId)
    })
    this.closing.set(session.instanceId, cleanup)
    return cleanup
  }

  closeMatching(match: (session: SpiceAccess) => boolean): number {
    const tokens = [...this.sessions.values()].filter(match).map(s => s.token)
    for (const token of tokens) this.close(token)
    return tokens.length
  }

  async drain(): Promise<void> {
    this.closeMatching(() => true)
    await Promise.all([...this.closing.values()])
  }
}

export const spiceSessions = new SpiceSessionManager()
