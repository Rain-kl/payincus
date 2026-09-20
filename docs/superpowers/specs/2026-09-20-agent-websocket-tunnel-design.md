# 宿主机 Agent 基于 WebSocket 的内网穿透与双模式架构设计

## 1. 概述与背景

### 1.1 现状与痛点
PayIncus 面板管理宿主机上的 Incus 容器与虚拟机时，控制面采用主动直连模式（面板直连宿主机 `https://<host-ip>:<api-port>`，默认 8443，mTLS 客户端证书握手认证）。
但在以下场景中存在明显痛点：
1. **NAT 内网宿主机**：宿主机处于家用宽带、内网虚拟化集群或严格 NAT 后面，没有独立公网 IP，无法开放入站端口。
2. **防火墙与安全管控**：很多机房阻断非标端口入站，或管理员出于安全考量禁止在公网暴露虚拟化底座管理端口。
3. **跨境通信网络稳定性**：使用非标准端口或原生 VPN / 裸 FRP 穿透时，长期挂载的长连接极易被运营商或防火墙阻断。

### 1.2 目标与设计原则
- **内网穿透模式（WebSocket Reverse Tunnel）**：宿主机 Agent 启动后主动向 Server 建立安全的出站 WebSocket 长连接，Server 通过该反向隧道转发 Incus 请求到宿主机本地指定端口（默认 `127.0.0.1:8443`）。
- **双模式并存（Strict A 方案）**：系统完整保留「主动直连」与「内网穿透」双模式，前端提供明确开关，严格按照配置执行。在穿透模式下，用户无需输入连接地址/公网 IP，直接指定本地转发端口（默认 8443）。
- **服务端集中驱动（Server-Driven Control Plane）**：
  - 宿主机端安装命令 100% 统一，**无需指定任何穿透环境变量或端口**。
  - 何时开启/关闭穿透、转发至哪个本地 IP 和端口，全部由 Server 在心跳响应中下发或在长连接中动态热更新，**Agent 零本地端口配置、零重启**。
- **业务零侵入**：Server 层的 `IncusClient` 继续保持原生的 mTLS 证书校验与完整 Incus API 调用逻辑，端到端证书认证不变，所有实例创建、启停、监控业务代码零破坏。

---

## 2. 总体架构设计

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                           PayIncus Server (Fastify)                         │
│                                                                             │
│  [前端管理界面]                                                             │
│       │                                                                     │
│       ▼                                                                     │
│  [Host 业务层 / IncusClient]                                                │
│       │                                                                     │
│       ├── (主动直连模式) ──> net.connect(公网 IP:Port) ──────────────────┐  │
│       │                                                                 │  │
│       └── (内网穿透模式) ──> HostTunnelManager (内存流式多路复用)         │  │
│                                      │                                  │  │
│                                      ▼                                  │  │
│                   WebSocket 端点: /api/agent/tunnel                     │  │
└──────────────────────────────────────┬──────────────────────────────────────┘
                                       │ 
                 出站 WSS 连接 (HTTPS 443 / Cloudflare CDN 兼容)
                 携带 HMAC 签名头、30s 心跳保活、自动指数退避重连
                                       │
┌──────────────────────────────────────▼──────────────────────────────────────┐
│                         宿主机 Agent (incudal-agent)                        │
│                                                                             │
│  [心跳采集器] ── 60s ──> POST /api/agent/heartbeat (接收 tunnel 开关与配置)  │
│                                                                             │
│  [WebSocket Tunnel Worker] <────────────────────────────────────────────────┘
│         │
│         │ (收到 STREAM_OPEN 帧)
│         ▼
│    net.Dial("tcp", targetHost + ":" + targetPort) [默认 127.0.0.1:8443]
│         │
│         ▼
│    Incus Daemon ([::]:8443 / unix.socket)
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 3. 详细设计

### 3.1 数据模型扩展（`server/prisma/schema.prisma`）
在 `Host` 模型中新增字段：
```prisma
model Host {
  // ... 现有字段保持不变 ...
  tunnelEnabled     Boolean  @default(false) @map("tunnel_enabled") // 是否开启内网穿透模式
  targetHost        String   @default("127.0.0.1") @map("target_host") // 穿透本地目标地址（默认 127.0.0.1）
  targetPort        Int      @default(8443) @map("target_port")       // 穿透本地目标端口（默认 8443）
}
```
*在穿透模式下，`Host.url` 字段存为内部虚拟统一标识 `tunnel://host-{id}`，既满足数据库 `url` 非空约束，又明确标记通道类别。*

### 3.2 前端界面与交互设计（`MyHostCreateView.vue` / `MyHostConfigTab.vue`）
1. **连接模式切换**：
   - 增加 Radio / Tab 模式切换：`[主动直连 (Direct)]` 与 `[内网穿透 (Agent Tunnel)]`。
2. **内网穿透模式下的表单行为**：
   - **连接地址（IP / 域名）输入框**：自动隐藏并展示友好的引导提示卡片：
     > 💡 “已启用 Agent WebSocket 反向穿透模式。宿主机无需独立公网 IP，无需防火墙放行入站端口。”
   - **目标端口**：默认填入 `8443`，支持用户按需修改为其他本地自定义端口。
   - **高级设置（折叠）**：展示「本地转发目标」，默认锁定为 `127.0.0.1`，支持微调修改（如特定容器网桥 IP）。
3. **状态展示**：
   - 在宿主机列表与详情页中，直观展示连接方式与在线状态：
     - 直连：`主动直连 (https://1.2.3.4:8443)`
     - 穿透已连接：`🟢 穿透隧道已连接 (127.0.0.1:8443)`
     - 穿透离线：`🔴 穿透隧道未建立 (Agent 离线)`

### 3.3 WebSocket 隧道与轻量多路复用流协议

#### 3.3.1 端点与鉴权
- **端点**：`GET /api/agent/tunnel`（由 `@fastify/websocket` 处理）。
- **握手鉴权**：Agent 在 HTTP 升级请求的 Header 中携带标准 HMAC-SHA256 签名：
  - `x-incudal-agent-id`: 宿主机绑定的 Agent ID
  - `x-incudal-timestamp`: 毫秒级时间戳
  - `x-incudal-nonce`: 随机防重放 Nonce
  - `x-incudal-signature`: 对请求路径和 Nonce 进行 HMAC-SHA256 计算所得的签名
- Server 验证签名有效、对应宿主机 `tunnelEnabled == true` 且 Nonce 未被重放后，完成 101 Switching Protocols 升级。

#### 3.3.2 5 字节轻量分帧协议（Multiplexed Stream Framing）
单条 WebSocket 连接通过二进制流帧承载多个并发连接：
```text
+────────────+─────────────────────+─────────────────────────────+
| Type (1B)  |   Stream ID (4B)    |        Payload (变长)       |
+────────────+─────────────────────+─────────────────────────────+
```
- **Type 字节定义**：
  - `0x01 (STREAM_OPEN)`：Server ➔ Agent，携带由 Server 动态决定的 `targetHost` 与 `targetPort`，通知 Agent 在本地开启拨号。
  - `0x02 (STREAM_DATA)`：双向数据传输（包含 mTLS 握手报文及 HTTP 原始载荷）。
  - `0x03 (STREAM_CLOSE)`：双向优雅关闭半连接（EOF / FIN）。
  - `0x04 (STREAM_RESET)`：双向异常中断连接（RST / 拨号失败）。
  - `0x05 (CONFIG_UPDATE)`：Server ➔ Agent，用于长连接在线期间热推配置更新。

### 3.4 Server 端内存隧道管理与 IncusClient 适配
根据 `AGENTS.md` 规定，私有后端为**单实例进程**，因此在 `server/src/lib/incus/tunnel-manager.ts` 中维护单例 `HostTunnelManager`：
1. **连接注册与心跳维护**：
   - 维护 `hostId ➔ WebSocket` 实例映射。
   - 支持 Agent 掉线自动清理与其关联的虚拟 Stream。
2. **IncusClient 虚拟 Duplex 流**：
   - 在穿透模式下，`IncusClient` 通过 `HostTunnelManager.createDuplexStream(hostId)` 获取 Node.js 原生 `stream.Duplex`。
   - 将该流直接送入 Node.js 原生 `tls.connect({ socket: duplexStream, cert, key, rejectUnauthorized: false })`。
   - `undici` 的 HTTP 客户端无缝跑在该 TLS Socket 之上，上层所有 API 请求、超时管理、连接池完全透明。

### 3.5 服务端动态生命周期驱动（Server-Driven Lifecycle）
1. **安装命令 100% 统一**：
   - 无论是否开启穿透，生成的安装命令均为原生标准命令：
     ```bash
     curl -fsSL "$PANEL_URL/api/agent/install.sh" | sudo env \
       INCUDAL_PANEL_URL="$PANEL_URL" \
       INCUDAL_AGENT_INSTALL_TOKEN="$TOKEN" \
       bash
     ```
2. **心跳下发与自动启停**：
   - Server 在 `/api/agent/heartbeat` 响应中下发 `tunnel` 配置对象：
     ```json
     {
       "tunnel": {
         "enabled": true,
         "targetHost": "127.0.0.1",
         "targetPort": 8443
       }
     }
     ```
   - Agent 检测到 `enabled: true`，在后台启动 WebSocket 隧道连接并保持。
   - 若在 Web 端关闭内网穿透，下一次心跳响应返回 `enabled: false`，Agent 自动切断 WebSocket 隧道进入休眠，不消耗系统资源。
3. **秒级配置热生效**：
   - 若在穿透连接期间修改了 `targetPort`，Server 在已建立的 WebSocket 隧道中发送 `CONFIG_UPDATE` 帧，Agent 立即热更新转发目标，无需重启进程。

---

## 4. 安全防护与隔离机制

1. **严格的本地转发目标白名单**：
   - Agent 仅允许连接 Server 明确授权下发的 `targetHost` 与 `targetPort`，严禁客户端透传任意 IP 端口，防止宿主机被当作内网渗透跳板。
2. **重放攻击防护**：
   - WebSocket 握手时强制校验 Nonce 唯一性与 5 分钟时间戳窗口，与现有心跳鉴权规范保持同等级别安全性。
3. **mTLS 端到端加密透传**：
   - 隧道只传输加密的 TLS 字节流，不解密任何应用层流量，Incus 的双向证书认证防伪造特性得到完整保留。
4. **资源约束**：
   - systemd 保持现有防护约束：`CPUQuota=20%`、`MemoryMax=256M`、`TasksMax=128`。

---

## 5. 测试与验证计划

### 5.1 自动化测试守卫（Server 端）
- 新建 `server/scripts/test-agent-tunnel-guards.ts`：
  - 断言 WebSocket 路由 `/api/agent/tunnel` 存在且包含严格的 HMAC 鉴权校验。
  - 断言 `HostTunnelManager` 支持 Stream 多路复用分帧与异常 Reset 处理。
  - 断言 `IncusClient` 支持根据 `tunnelEnabled` 动态切换直连与穿透传输层。
  - 跑通 `pnpm --filter server test:agent-tunnel-guards`。

### 5.2 Agent 单元与集成测试（Go 端）
- 新建 `agent/internal/tunnel/tunnel_test.go`：
  - 测试二进制分帧（`STREAM_OPEN`, `STREAM_DATA`, `STREAM_CLOSE`, `STREAM_RESET`）的编解码一致性。
  - 测试本地 TCP 拨号失败时能准确发送 `STREAM_RESET`。
  - 跑通 `pnpm test:agent` 与 `go test ./internal/tunnel/...`。

### 5.3 静态类型与前端守卫检查
- `pnpm --filter server type-check`
- `pnpm --filter client type-check`
- `pnpm --filter server test:frontend-route-guards`
- `pnpm --filter server test:frontend-i18n-keys`
- `pnpm build:client`
