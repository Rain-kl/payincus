import { Duplex } from 'node:stream'
import { EventEmitter } from 'node:events'
import { decodeFrame, encodeFrame, TunnelFrameType } from './tunnel-protocol.js'

export interface WebSocketLike {
  readyState: number
  send(data: any, cb?: (err?: Error) => void): void
  close(code?: number, reason?: string): void
  on(event: string, listener: (...args: any[]) => void): this
  removeListener?(event: string, listener: (...args: any[]) => void): this
}

export interface TunnelTargetConfig {
  targetHost: string
  targetPort: number
}

const WS_OPEN = 1

/**
 * 宿主机反向 WebSocket 隧道管理器（单例）
 *
 * 维护在线宿主机的出站 WebSocket 长连接，管理多路复用 Duplex 虚拟流，
 * 为 IncusClient 提供端到端 TLS 握手所需的全双工字节流通道。
 */
export class HostTunnelManager extends EventEmitter {
  private activeTunnels = new Map<number, WebSocketLike>()
  private streamMap = new Map<string, Duplex>()
  private nextStreamId = 0

  /**
   * 注册在线隧道的 WebSocket 连接
   */
  registerTunnel(hostId: number, ws: WebSocketLike): void {
    const existing = this.activeTunnels.get(hostId)
    if (existing && existing !== ws) {
      try {
        existing.close(1000, 'Replaced by newer agent tunnel connection')
      } catch {
        // ignore close error
      }
    }

    this.activeTunnels.set(hostId, ws)
    this.emit('tunnel_connected', hostId)

    const onMessage = (data: any) => {
      let buf: Buffer
      if (Buffer.isBuffer(data)) {
        buf = data
      } else if (data instanceof ArrayBuffer) {
        buf = Buffer.from(data)
      } else if (Array.isArray(data)) {
        buf = Buffer.concat(data)
      } else if (data?.buffer instanceof ArrayBuffer) {
        buf = Buffer.from(data.buffer, data.byteOffset, data.byteLength)
      } else {
        return
      }

      try {
        const frame = decodeFrame(buf)
        this.handleIncomingFrame(hostId, frame)
      } catch (err) {
        console.error(`[HostTunnelManager] Malformed frame from host ${hostId}:`, err)
      }
    }

    const onClose = () => {
      if (this.activeTunnels.get(hostId) === ws) {
        this.activeTunnels.delete(hostId)
        this.cleanupHostStreams(hostId, new Error(`Host ${hostId} tunnel closed`))
        this.emit('tunnel_disconnected', hostId)
      }
    }

    ws.on('message', onMessage)
    ws.on('close', onClose)
    ws.on('error', (err) => {
      console.warn(`[HostTunnelManager] Tunnel socket error for host ${hostId}:`, err?.message || err)
    })
  }

  /**
   * 注销指定宿主机的 WebSocket 隧道
   */
  unregisterTunnel(hostId: number, ws?: WebSocketLike): void {
    const current = this.activeTunnels.get(hostId)
    if (!ws || current === ws) {
      this.activeTunnels.delete(hostId)
      if (current) {
        try {
          current.close(1000, 'Unregistered')
        } catch {
          // ignore
        }
      }
      this.cleanupHostStreams(hostId, new Error(`Host ${hostId} tunnel unregistered`))
      this.emit('tunnel_disconnected', hostId)
    }
  }

  /**
   * 判断指定宿主机当前是否在线连接隧道
   */
  isTunnelOnline(hostId: number): boolean {
    const ws = this.activeTunnels.get(hostId)
    return !!ws && ws.readyState === WS_OPEN
  }

  /**
   * 获取指定宿主机的活动 WebSocket
   */
  getTunnel(hostId: number): WebSocketLike | undefined {
    return this.activeTunnels.get(hostId)
  }

  /**
   * 向指定宿主机广播实时配置更新帧
   */
  broadcastConfig(hostId: number, config: TunnelTargetConfig): boolean {
    const ws = this.activeTunnels.get(hostId)
    if (!ws || ws.readyState !== WS_OPEN) {
      return false
    }
    const payload = Buffer.from(JSON.stringify(config), 'utf8')
    const frame = encodeFrame(TunnelFrameType.CONFIG, 0, payload)
    try {
      ws.send(frame)
      return true
    } catch (err) {
      console.error(`[HostTunnelManager] Failed to send config frame to host ${hostId}:`, err)
      return false
    }
  }

  /**
   * 向指定宿主机下发日志流控制帧（启停 Agent 的 journalctl 推送）
   */
  sendLogControl(hostId: number, action: 'start' | 'stop', lines?: number): boolean {
    const ws = this.activeTunnels.get(hostId)
    if (!ws || ws.readyState !== WS_OPEN) {
      return false
    }
    const payload = Buffer.from(JSON.stringify({ action, ...(lines ? { lines } : {}) }), 'utf8')
    const frame = encodeFrame(TunnelFrameType.LOG_CTL, 0, payload)
    try {
      ws.send(frame)
      return true
    } catch (err) {
      console.error(`[HostTunnelManager] Failed to send log control frame to host ${hostId}:`, err)
      return false
    }
  }

  /**
   * 为指定宿主机创建并返回一个用于 Incus API 请求的多路复用 Duplex 虚拟流
   */
  createDuplexStream(hostId: number, targetHost: string = '127.0.0.1', targetPort: number = 8443): Duplex {
    const ws = this.activeTunnels.get(hostId)
    if (!ws || ws.readyState !== WS_OPEN) {
      throw new Error(`Host ${hostId} tunnel is not connected or offline`)
    }

    this.nextStreamId = (this.nextStreamId + 1) & 0x7fffffff
    if (this.nextStreamId === 0) this.nextStreamId = 1
    const streamId = this.nextStreamId
    const streamKey = `${hostId}:${streamId}`

    // 发送 STREAM_OPEN 帧通知宿主机 Agent 连接本地端口
    const openPayload = Buffer.from(JSON.stringify({ targetHost, targetPort }), 'utf8')
    const openFrame = encodeFrame(TunnelFrameType.OPEN, streamId, openPayload)
    ws.send(openFrame)

    let isDestroyed = false

    const duplex = new Duplex({
      read(_size) {
        // 数据由 WebSocket message 事件被动注入通过 duplex.push()
      },
      write(chunk: any, encoding: BufferEncoding, callback: (error?: Error | null) => void) {
        if (ws.readyState !== WS_OPEN) {
          return callback(new Error('Tunnel connection lost while writing'))
        }
        const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk, encoding)
        // 64KB 分块发送
        const CHUNK_SIZE = 64 * 1024
        let offset = 0
        const sendNextChunk = () => {
          if (offset >= buf.length) {
            return callback()
          }
          const end = Math.min(offset + CHUNK_SIZE, buf.length)
          const slice = buf.subarray(offset, end)
          offset = end
          const dataFrame = encodeFrame(TunnelFrameType.DATA, streamId, slice)
          ws.send(dataFrame, (err) => {
            if (err) return callback(err)
            sendNextChunk()
          })
        }
        sendNextChunk()
      },
      final(callback: (error?: Error | null) => void) {
        if (ws.readyState === WS_OPEN) {
          const closeFrame = encodeFrame(TunnelFrameType.CLOSE, streamId)
          ws.send(closeFrame, () => callback())
        } else {
          callback()
        }
      },
      destroy(err: Error | null, callback: (error: Error | null) => void) {
        if (!isDestroyed) {
          isDestroyed = true
          if (ws.readyState === WS_OPEN) {
            const resetFrame = encodeFrame(TunnelFrameType.RESET, streamId)
            try {
              ws.send(resetFrame)
            } catch {
              // ignore
            }
          }
        }
        callback(err)
      }
    })

    const cleanup = () => {
      this.streamMap.delete(streamKey)
    }

    duplex.once('close', cleanup)
    duplex.once('finish', cleanup)
    duplex.once('error', cleanup)

    this.streamMap.set(streamKey, duplex)
    return duplex
  }

  /**
   * 处理 Agent WebSocket 发来的二进制数据帧
   */
  private handleIncomingFrame(hostId: number, frame: { type: TunnelFrameType; streamId: number; payload: Buffer }): void {
    const { type, streamId, payload } = frame
    const streamKey = `${hostId}:${streamId}`
    const stream = this.streamMap.get(streamKey)

    switch (type) {
      case TunnelFrameType.DATA:
        if (stream && !stream.destroyed) {
          stream.push(payload)
        }
        break

      case TunnelFrameType.CLOSE:
        if (stream && !stream.destroyed) {
          stream.push(null) // EOF
        }
        break

      case TunnelFrameType.RESET:
        if (stream && !stream.destroyed) {
          stream.destroy(new Error(`Stream ${streamId} reset by remote agent`))
        }
        break

      case TunnelFrameType.OPEN:
      case TunnelFrameType.CONFIG:
      case TunnelFrameType.LOG_CTL:
        // 服务端目前仅发出 OPEN / CONFIG / LOG_CTL，忽略 Agent 错误发来的此类控制帧
        break

      case TunnelFrameType.LOG_DATA:
        // Agent 推送的日志行（空 Payload 表示日志流结束）
        this.emit('log_data', hostId, payload.length > 0 ? payload.toString('utf8') : null)
        break

      default:
        break
    }
  }

  /**
   * 宿主机断开时，清理该宿主机下的所有虚拟流
   */
  private cleanupHostStreams(hostId: number, err: Error): void {
    const prefix = `${hostId}:`
    for (const [key, stream] of this.streamMap.entries()) {
      if (key.startsWith(prefix)) {
        this.streamMap.delete(key)
        if (!stream.destroyed) {
          stream.destroy(err)
        }
      }
    }
  }
}

/**
 * 全局单例
 */
export const hostTunnelManager = new HostTunnelManager()
