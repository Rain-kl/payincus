import { EventEmitter } from 'node:events'
import { prisma } from '../db/prisma.js'
import { hostTunnelManager } from './incus/tunnel-manager.js'

/**
 * Agent 运行日志流会话管理器（进程内单例）
 *
 * 数据路径：
 *   浏览器 SSE ──> 面板 ──LOG_CTL(start/stop)──> Agent
 *   浏览器 SSE <── 面板 <──LOG_DATA(逐行)────── Agent（经其出站 WS 主动推送）
 *
 * 面板收到浏览器的日志订阅后用 LOG_CTL 帧请求 Agent 启动 journalctl 流；
 * Agent 通过其既有出站 WebSocket（agent 主动连接面板的常驻通道）逐行回传 LOG_DATA；
 * 管理器把每行扇出给该宿主机的全部 SSE 订阅者。
 *
 * 状态语义：隧道在线（tunnelOnline）是日志流的必要不充分条件 ——
 * 心跳在线但隧道未开启（宿主机配置为直连模式）时，订阅者会收到
 * { agentOnline: true, tunnelOnline: false }，前端据此给出可操作提示，
 * 而不是笼统显示「Agent 离线」。
 */

export interface AgentLogStreamClient {
  /** 向浏览器连接写入一段 SSE 文本；返回 false 表示连接已不可写 */
  write(chunk: string): boolean
}

interface AgentLogSession {
  clients: Set<AgentLogStreamClient>
  keepalive: ReturnType<typeof setInterval> | null
}

const SSE_KEEPALIVE_MS = 15 * 1000
const AGENT_HEARTBEAT_STALE_MS = 3 * 60 * 1000

class AgentLogStreamManager extends EventEmitter {
  private sessions = new Map<number, AgentLogSession>()
  private attached = false

  private ensureAttached(): void {
    if (this.attached) return
    this.attached = true
    hostTunnelManager.on('tunnel_connected', (hostId: number) => {
      const session = this.sessions.get(hostId)
      if (!session || session.clients.size === 0) return
      // Agent 隧道重建后自动恢复日志流，浏览器无需重连
      hostTunnelManager.sendLogControl(hostId, 'start', 100)
      this.broadcastStatus(session, { tunnelOnline: true })
    })
    hostTunnelManager.on('tunnel_disconnected', (hostId: number) => {
      const session = this.sessions.get(hostId)
      if (!session || session.clients.size === 0) return
      // 保持连接打开：等待 Agent 重连后自动恢复
      this.broadcastStatus(session, { tunnelOnline: false })
    })
    hostTunnelManager.on('log_data', (hostId: number, line: string | null) => {
      const session = this.sessions.get(hostId)
      if (!session || session.clients.size === 0) return
      if (line === null) {
        // 空 Payload = Agent 日志流结束
        for (const client of session.clients) {
          client.write('event: end\ndata: {}\n\n')
        }
        return
      }
      const payload = line.length > 8000 ? line.slice(0, 8000) + '…' : line
      const data = JSON.stringify(payload)
      for (const client of session.clients) {
        if (!client.write(`event: log\ndata: ${data}\n\n`)) {
          // 连接已断开，延迟到下一次广播统一清理
          this.scheduleDeferredCleanup(hostId, client)
        }
      }
    })
  }

  private deferredCleanup = new Map<number, Set<AgentLogStreamClient>>()

  private scheduleDeferredCleanup(hostId: number, client: AgentLogStreamClient): void {
    let set = this.deferredCleanup.get(hostId)
    if (!set) {
      set = new Set()
      this.deferredCleanup.set(hostId, set)
    }
    set.add(client)
    queueMicrotask(() => {
      const pending = this.deferredCleanup.get(hostId)
      if (!pending) return
      this.deferredCleanup.delete(hostId)
      for (const dead of pending) {
        this.unsubscribe(hostId, dead)
      }
    })
  }

  private sseEvent(event: string, payload: unknown): string {
    return `event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`
  }

  private broadcastStatus(session: AgentLogSession, status: Record<string, boolean>): void {
    const chunk = this.sseEvent('status', status)
    for (const client of session.clients) {
      client.write(chunk)
    }
  }

  /**
   * 注册一个浏览器 SSE 订阅者；首个订阅者会触发 Agent 端开始推送日志。
   * 订阅时探测心跳在线与隧道在线，把真实状态回给前端。
   */
  async subscribe(hostId: number, client: AgentLogStreamClient): Promise<void> {
    this.ensureAttached()

    let session = this.sessions.get(hostId)
    if (!session) {
      session = { clients: new Set(), keepalive: null }
      this.sessions.set(hostId, session)
    }
    session.clients.add(client)

    if (session.keepalive === null) {
      session.keepalive = setInterval(() => {
        const current = this.sessions.get(hostId)
        if (!current || current.clients.size === 0) return
        for (const c of current.clients) {
          c.write(': ping\n\n')
        }
      }, SSE_KEEPALIVE_MS)
    }

    const tunnelOnline = hostTunnelManager.isTunnelOnline(hostId)

    let agentOnline = false
    try {
      const agent = await prisma.hostAgent.findFirst({
        where: { hostId, enabled: true },
        select: { status: true, lastSeenAt: true }
      })
      agentOnline = !!agent && agent.status === 'online' &&
        !!agent.lastSeenAt && (Date.now() - agent.lastSeenAt.getTime()) < AGENT_HEARTBEAT_STALE_MS
    } catch {
      // DB 读取失败不影响订阅流程，仅以隧道状态为准
    }

    client.write(this.sseEvent('status', { agentOnline, tunnelOnline }))
    if (tunnelOnline) {
      hostTunnelManager.sendLogControl(hostId, 'start', 100)
    }
  }

  /**
   * 注销一个浏览器 SSE 订阅者；最后一个订阅者退出时通知 Agent 停止推送。
   */
  unsubscribe(hostId: number, client: AgentLogStreamClient): void {
    const session = this.sessions.get(hostId)
    if (!session) return
    session.clients.delete(client)
    if (session.clients.size > 0) return

    if (session.keepalive !== null) {
      clearInterval(session.keepalive)
      session.keepalive = null
    }
    this.sessions.delete(hostId)
    if (hostTunnelManager.isTunnelOnline(hostId)) {
      hostTunnelManager.sendLogControl(hostId, 'stop')
    }
  }
}

/**
 * 全局单例
 */
export const agentLogStreamManager = new AgentLogStreamManager()
