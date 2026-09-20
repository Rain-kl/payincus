/**
 * Caddy API 客户端（纯隧道模式）
 *
 * 安全模型：
 * - Caddy Admin API 只在宿主机回环 127.0.0.1:<port> 上以明文 HTTP 监听
 *   （节点上由 agent 负责将其绑定到 localhost，绝不对外开放公网端口）。
 * - 面板通过现有 Agent 反向 WebSocket 隧道（HMAC 鉴权）创建一条到
 *   hostId 的对端虚拟 TCP 流，再在该流上发起 HTTP 请求。
 * - 不依赖任何管理凭据 / TLS 证书 / Basic Auth —— 信任链完全落在
 *   面板 <-> Agent 的 HMAC + 隧道通道上；Caddy 只信任同机 Agent。
 */

import http from 'node:http'
import type { Duplex } from 'node:stream'
import { hostTunnelManager } from './incus/tunnel-manager.js'

// 请求超时配置（毫秒）
const REQUEST_TIMEOUT = 30000 // 30秒

export interface CaddyClientConfig {
  hostId: number // 宿主机 ID（用于经 Agent 隧道建立到 Caddy Admin 的虚拟流）
  port?: number // Caddy Admin 端口（默认 2019，本地回环监听）
  targetHost?: string // 隧道对端地址，固定 127.0.0.1（可通过构造覆盖，仅测试用）
}

export interface CaddyRoute {
  '@id'?: string
  match?: Array<{ host?: string[]; protocol?: string }>
  handle?: Array<{
    handler: string
    upstreams?: Array<{ dial: string }>
    routes?: CaddyRoute[]
  }>
  terminal?: boolean
}

class CaddyApiError extends Error {
  constructor(
    readonly statusCode: number,
    responseBody: string
  ) {
    super(`Caddy API error: ${statusCode} ${responseBody}`)
    this.name = 'CaddyApiError'
  }
}

/**
 * 隧道 duplex 是 stream.Duplex，node:http 客户端在其上会调用一些
 * net.Socket 专属方法；补上空实现即可安全交由 http.Agent 使用。
 */
function patchDuplexForHttp(socket: Duplex): Duplex {
  const patched = socket as Duplex & {
    setNoDelay?: () => Duplex
    setKeepAlive?: () => Duplex
    setTimeout?: (ms: number, cb?: () => void) => Duplex
    ref?: () => Duplex
    unref?: () => Duplex
  }
  if (typeof patched.setNoDelay !== 'function') patched.setNoDelay = () => socket
  if (typeof patched.setKeepAlive !== 'function') patched.setKeepAlive = () => socket
  if (typeof patched.setTimeout !== 'function') {
    patched.setTimeout = (_ms: number, cb?: () => void) => {
      if (cb) socket.once('timeout', cb)
      return socket
    }
  }
  if (typeof patched.ref !== 'function') patched.ref = () => socket
  if (typeof patched.unref !== 'function') patched.unref = () => socket
  return socket
}

/**
 * Caddy API 客户端类
 */
export class CaddyClient {
  private baseUrl: string
  private agent: http.Agent

  constructor(config: CaddyClientConfig) {
    const port = config.port || 2019
    const targetHost = config.targetHost || '127.0.0.1'
    this.baseUrl = `http://127.0.0.1:${port}`

    // 每请求经隧道建立一条到宿主机回环 Caddy Admin 的虚拟 TCP 流。
    // keepAlive=false：流用完即断，避免长连接占用隧道流；http.Agent 会
    // 在请求结束后自行销毁 connect 返回的 socket。
    type CreateConnectionCallback = (err: Error | null, socket?: Duplex) => void
    type CreateConnectionFn = (options: Record<string, unknown>, callback: CreateConnectionCallback) => void
    const agentOptions: http.AgentOptions & { createConnection?: CreateConnectionFn } = {
      keepAlive: false,
      maxSockets: 32,
      createConnection: (_options, callback) => {
        try {
          const duplex = hostTunnelManager.createDuplexStream(config.hostId, targetHost, port)
          callback(null, patchDuplexForHttp(duplex))
        } catch (err) {
          callback(err instanceof Error ? err : new Error(String(err)))
        }
      }
    }
    this.agent = new http.Agent(agentOptions)
  }

  /**
   * 发起 API 请求
   */
  private request<T>(
    method: string,
    path: string,
    body?: unknown
  ): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const request = http.request({
        method,
        hostname: '127.0.0.1',
        port: new URL(this.baseUrl).port || 2019,
        path,
        agent: this.agent,
        headers: {
          'Content-Type': 'application/json'
        }
      }, (response) => {
        const chunks: Buffer[] = []
        response.on('data', (chunk: Buffer) => chunks.push(chunk))
        response.on('end', () => {
          const text = Buffer.concat(chunks).toString('utf8')
          if (response.statusCode !== undefined && response.statusCode >= 400) {
            reject(new CaddyApiError(response.statusCode, text))
            return
          }
          if (!text) {
            resolve({} as T)
            return
          }
          try {
            resolve(JSON.parse(text) as T)
          } catch (parseError) {
            reject(parseError instanceof Error ? parseError : new Error(String(parseError)))
          }
        })
      })

      const timeoutId = setTimeout(() => request.destroy(new Error('Caddy request timeout')), REQUEST_TIMEOUT)
      request.on('error', (err: Error) => {
        clearTimeout(timeoutId)
        reject(err)
      })
      request.on('response', () => clearTimeout(timeoutId))

      if (body !== undefined && body !== null) {
        request.write(JSON.stringify(body))
      }
      request.end()
    })
  }

  /**
   * 获取 Caddy 配置
   */
  async getConfig(): Promise<unknown> {
    return this.request('GET', '/config/')
  }

  /**
   * 测试连接
   */
  async testConnection(): Promise<boolean> {
    try {
      await this.getConfig()
      return true
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : String(error)
      console.error('[Caddy Client] 连接失败（隧道）', { error: errorMessage })
      return false
    }
  }

  /**
   * 添加反代站点
   *
   * 架构说明：
   * - 使用统一的 `sites` 服务器监听 :80 和 :443
   * - HTTPS 站点：正常配置，Caddy 自动处理证书和重定向
   * - HTTP 站点：添加 protocol 匹配，仅响应 HTTP 请求
   *
   * @param domain 域名
   * @param targetIp 目标 IP (实例内网 IP)
   * @param targetPort 目标端口
   * @param httpsEnabled 是否启用 HTTPS（自动申请 Let's Encrypt 证书）
   */
  async addSite(domain: string, targetIp: string, targetPort: number, httpsEnabled: boolean = true): Promise<void> {
    const routeId = `site-${domain.replace(/\./g, '-')}`

    // 构建路由配置
    const route: CaddyRoute = {
      '@id': routeId,
      match: [{ host: [domain] }],
      handle: [{
        handler: 'reverse_proxy',
        upstreams: [{ dial: `${targetIp}:${targetPort}` }]
      }],
      terminal: true
    }

    // HTTP-only 站点：添加协议匹配，仅响应 HTTP 请求
    if (!httpsEnabled) {
      route.match = [{ host: [domain], protocol: 'http' }]
    }

    // 统一使用 sites 服务器
    const serverName = 'sites'
    const serverPath = `/config/apps/http/servers/${serverName}`
    const routesPath = `${serverPath}/routes`

    // routes 是数组；POST 只追加当前路由，不会替换现有路由集合。
    try {
      await this.request('POST', routesPath, route)
      return
    } catch (postError) {
      // 网络、认证、5xx 等错误绝不能被当作服务器不存在。
      if (!(postError instanceof CaddyApiError) || postError.statusCode !== 404) {
        throw postError
      }
    }

    // POST 的 404 之后再读取复核，区分 server 不存在与 routes 字段不存在。
    let existingServer: { routes?: CaddyRoute[] } | undefined
    try {
      existingServer = await this.request<{ routes?: CaddyRoute[] }>('GET', serverPath)
    } catch (getServerError) {
      if (!(getServerError instanceof CaddyApiError) || getServerError.statusCode !== 404) {
        throw getServerError
      }
    }

    if (existingServer) {
      if (Array.isArray(existingServer.routes)) {
        await this.request('POST', routesPath, route)
      } else {
        // server 存在但 routes 字段不存在；只在精确字段路径严格创建数组。
        await this.request('PUT', routesPath, [route])
      }
      return
    }

    // 已明确确认 sites 不存在；PUT 在对象路径上是严格创建，不会替换已有 server。
    const serverConfig: Record<string, unknown> = {
      listen: [':80', ':443'],
      routes: [route],
      automatic_https: {
        disable_redirects: true
      }
    }

    try {
      await this.request('PUT', serverPath, serverConfig)
      return
    } catch (putError) {
      // 只有明确 404 才可能是父级 http 应用不存在。
      if (!(putError instanceof CaddyApiError) || putError.statusCode !== 404) {
        throw putError
      }
    }

    const httpAppPath = '/config/apps/http'
    try {
      await this.request('GET', httpAppPath)
    } catch (getHttpError) {
      if (!(getHttpError instanceof CaddyApiError) || getHttpError.statusCode !== 404) {
        throw getHttpError
      }

      // 父级也已明确确认不存在；严格创建，不覆盖任何现有 HTTP 配置。
      await this.request('PUT', httpAppPath, {
        servers: {
          [serverName]: serverConfig
        }
      })
      return
    }

    // http 在复核期间已由其他请求创建；回到精确 server 路径严格创建。
    await this.request('PUT', serverPath, serverConfig)
  }

  /**
   * 删除反代站点
   * @param domain 域名
   */
  async deleteSite(domain: string): Promise<void> {
    const routeId = `site-${domain.replace(/\./g, '-')}`

    try {
      await this.request('DELETE', `/id/${routeId}`)
    } catch (error) {
      // 如果路由不存在，忽略错误
      if (error instanceof Error && !error.message.includes('404')) {
        throw error
      }
    }
  }

  /**
   * 获取所有反代站点
   */
  async getSites(): Promise<string[]> {
    try {
      const config = await this.getConfig() as {
        apps?: {
          http?: {
            servers?: Record<string, {
              routes?: CaddyRoute[]
            }>
          }
        }
      }

      const servers = config?.apps?.http?.servers || {}
      const domains: string[] = []

      // 从所有服务器收集域名
      for (const serverName of Object.keys(servers)) {
        const routes = servers[serverName]?.routes || []
        for (const route of routes) {
          if (route.match?.[0]?.host) {
            domains.push(...route.match[0].host)
          }
        }
      }

      return domains
    } catch {
      return []
    }
  }

  /**
   * 重新加载配置
   */
  async reload(): Promise<void> {
    // Caddy 会自动应用配置更改，不需要显式 reload
    // 这个方法保留用于兼容性
  }
}

/**
 * 创建 Caddy 客户端实例（经 Agent 隧道访问宿主机回环 Caddy Admin）
 */
export function createCaddyClient(config: CaddyClientConfig): CaddyClient {
  return new CaddyClient(config)
}

/**
 * 宿主机的 Caddy 管理端口（该端口在节点上只绑定 localhost，由 Agent 隧道访问）。
 * 新安装统一使用 Caddy Admin 默认端口 2019；旧值 8444 仅用于兼容判断。
 */
export const CADDY_ADMIN_PORT = 2019

/**
 * 从 host 行提取 Caddy 管理端口（DB 未配置或为旧值 8444 时归一为 2019）。
 */
function resolveCaddyAdminPort(hostPort: number | null | undefined): number {
  const port = hostPort ?? 0
  // 8444 是旧"公网反代层"的端口；收紧 localhost 后管理口恒定 2019。
  return port === 8444 || port <= 0 ? CADDY_ADMIN_PORT : port
}

/**
 * 为宿主机创建经 Agent 隧道访问 Caddy Admin 的客户端。
 *
 * @param host 宿主机行（Prisma 或 db 层映射对象），需含 id / caddy_enabled 等信息
 * @param opts.port 显式指定管理端口（默认按 host.caddy_port 归一）
 * @throws 当隧道无法建立（Agent 离线）或 Caddy 未启用时
 */
export function getCaddyClientForHost(
  host: { id: number; caddy_enabled?: boolean; caddyEnabled?: boolean; caddy_port?: number | null; caddyPort?: number | null },
  opts: { port?: number } = {}
): CaddyClient {
  if (!host || !host.id) {
    throw new Error('宿主机无效，无法建立 Caddy 隧道连接')
  }
  const caddyEnabled = host.caddy_enabled ?? host.caddyEnabled
  if (caddyEnabled === false) {
    throw new Error('请先在宿主机安装 Caddy')
  }
  if (!hostTunnelManager.isTunnelOnline(host.id)) {
    throw new Error('宿主机 Agent 隧道未连接，无法管理 Caddy（请确认 Agent 在线）')
  }
  const port = host.caddy_port ?? host.caddyPort ?? 0
  return createCaddyClient({
    hostId: host.id,
    port: opts.port || resolveCaddyAdminPort(port)
  })
}
