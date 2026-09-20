# 宿主机 Agent WebSocket 内网穿透与双模式 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 PayIncus 宿主机节点提供「主动直连」与「WebSocket 内网穿透」双模式，使没有公网 IP 的宿主机能够通过 Agent 主动建立的 WebSocket 隧道透明转发 Incus REST API，并支持服务端集中控制与动态热配置。

**Architecture:** 宿主机 Agent 主动向 Server 建立出站 WebSocket 连接，Server 端基于 `@fastify/websocket` 与 Node.js 内存流（Duplex）在 WebSocket 上实现 5 字节轻量多路复用隧道；Server 的 `IncusClient` 将 `undici.Agent` 桥接到该虚拟流上完成端到端 mTLS 握手。何时开启穿透、转发目标端口由 Server 动态下发，Agent 零本地配置，前端提供双模式无缝切换开关。

**Tech Stack:** Fastify 5, `@fastify/websocket`, Node.js `stream.Duplex`, `undici`, Prisma 7, PostgreSQL, Go 1.19+ (`gorilla/websocket`), Vue 3, TypeScript.

## Global Constraints

- **私有后端必须单实例运行**：`HostTunnelManager` 在 Node.js 进程内存中维护连接，不依赖外部缓存。
- **高危路径防回归**：不修改实例交付、计费账务或现有公网直连核心逻辑。
- **表格布局与前端守卫锁定**：涉及节点管理前端视图时，必须保持受锁定表格的 `table-fixed` 与百分比列宽之和为 100%。
- **双模式严格按开关执行**：开启穿透时只走 WebSocket 隧道；关闭穿透时只走公网直连。
- **服务端集中驱动**：宿主机一键安装命令 100% 统一，无需任何穿透环境变量或端口配置。
- **Git 提交纪律**：每个任务完成后进行原子 Commit，遵循 Conventional Commits，严禁 `git push`。

---

### Task 1: 数据库模型与类型扩展

**Files:**
- Create: `server/prisma/migrations/20260920080000_add_host_tunnel_fields/migration.sql`
- Modify: `server/prisma/schema.prisma:670-685`
- Modify: `server/src/db/hosts.ts:320-350, 460-500`
- Modify: `client/src/types/api.ts:1100-1120, 1240-1260`
- Test: `server/scripts/test-host-tunnel-schema-guards.ts`

**Interfaces:**
- Produces:
  - `Host.tunnelEnabled: boolean` (default false)
  - `Host.targetHost: string` (default '127.0.0.1')
  - `Host.targetPort: number` (default 8443)
  - TypeScript types `CreateHostInput` and `UpdateHostInput` with optional `tunnelEnabled`, `targetHost`, `targetPort`.

- [ ] **Step 1: 编写测试守卫 `server/scripts/test-host-tunnel-schema-guards.ts`**

```typescript
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const schema = readFileSync(resolve(process.cwd(), 'server/prisma/schema.prisma'), 'utf8')
assert.ok(schema.includes('tunnelEnabled     Boolean  @default(false) @map("tunnel_enabled")'))
assert.ok(schema.includes('targetHost        String   @default("127.0.0.1") @map("target_host")'))
assert.ok(schema.includes('targetPort        Int      @default(8443) @map("target_port")'))
console.log('host tunnel schema guard tests passed')
```

- [ ] **Step 2: 运行测试验证失败**

Run: `node --import tsx server/scripts/test-host-tunnel-schema-guards.ts`
Expected: FAIL (assertion fails because fields do not yet exist)

- [ ] **Step 3: 更新 `server/prisma/schema.prisma` 与创建迁移 SQL**

在 `model Host` 中添加：
```prisma
  tunnelEnabled     Boolean      @default(false) @map("tunnel_enabled")
  targetHost        String       @default("127.0.0.1") @map("target_host")
  targetPort        Int          @default(8443) @map("target_port")
```

创建 `server/prisma/migrations/20260920080000_add_host_tunnel_fields/migration.sql`:
```sql
-- 新增宿主机内网穿透字段
ALTER TABLE "hosts" ADD COLUMN IF NOT EXISTS "tunnel_enabled" BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE "hosts" ADD COLUMN IF NOT EXISTS "target_host" TEXT NOT NULL DEFAULT '127.0.0.1';
ALTER TABLE "hosts" ADD COLUMN IF NOT EXISTS "target_port" INTEGER NOT NULL DEFAULT 8443;
```

执行生成客户端：
`pnpm --filter server exec prisma generate`

- [ ] **Step 4: 更新 `server/src/db/hosts.ts` 与 `client/src/types/api.ts`**

在 `server/src/db/hosts.ts` 的 `createHost` 和 `updateHost` 函数与参数接口中加入 `tunnelEnabled?: boolean`, `targetHost?: string`, `targetPort?: number`。
在 `client/src/types/api.ts` 的 `Host` 及其创建/更新请求类型中同步加入这 3 个字段。

- [ ] **Step 5: 运行测试并验证通过**

Run: `node --import tsx server/scripts/test-host-tunnel-schema-guards.ts && pnpm --filter server type-check && pnpm --filter client type-check`
Expected: PASS

- [ ] **Step 6: 提交 Commit**

```bash
git add server/prisma/schema.prisma server/prisma/migrations/ server/scripts/test-host-tunnel-schema-guards.ts server/src/db/hosts.ts client/src/types/api.ts
git commit -m "feat(host): add tunnel_enabled, target_host and target_port fields to host schema"
```

---

### Task 2: Server 端 WebSocket 隧道协议与多路复用管理器（TunnelManager）

**Files:**
- Create: `server/src/lib/incus/tunnel-protocol.ts`
- Create: `server/src/lib/incus/tunnel-manager.ts`
- Create: `server/scripts/test-agent-tunnel-protocol-guards.ts`

**Interfaces:**
- Produces:
  - `enum TunnelFrameType { OPEN = 0x01, DATA = 0x02, CLOSE = 0x03, RESET = 0x04, CONFIG = 0x05 }`
  - `encodeFrame(type: TunnelFrameType, streamId: number, payload?: Buffer): Buffer`
  - `decodeFrame(buf: Buffer): { type: TunnelFrameType, streamId: number, payload: Buffer }`
  - `class HostTunnelManager`:
    - `registerTunnel(hostId: number, ws: WebSocket): void`
    - `unregisterTunnel(hostId: number, ws: WebSocket): void`
    - `isTunnelOnline(hostId: number): boolean`
    - `createDuplexStream(hostId: number, targetHost: string, targetPort: number): stream.Duplex`
    - `broadcastConfig(hostId: number, config: { targetHost: string, targetPort: number }): void`

- [ ] **Step 1: 编写测试守卫 `server/scripts/test-agent-tunnel-protocol-guards.ts`**

```typescript
import assert from 'node:assert/strict'
import { encodeFrame, decodeFrame, TunnelFrameType } from '../src/lib/incus/tunnel-protocol.js'

const payload = Buffer.from('hello-mTLS-bytes')
const frame = encodeFrame(TunnelFrameType.DATA, 101, payload)
assert.equal(frame.length, 5 + payload.length)
const decoded = decodeFrame(frame)
assert.equal(decoded.type, TunnelFrameType.DATA)
assert.equal(decoded.streamId, 101)
assert.deepEqual(decoded.payload, payload)
console.log('tunnel protocol guard tests passed')
```

- [ ] **Step 2: 运行测试验证失败**

Run: `node --import tsx server/scripts/test-agent-tunnel-protocol-guards.ts`
Expected: FAIL (module not found)

- [ ] **Step 3: 实现 `server/src/lib/incus/tunnel-protocol.ts`**

实现标准的 5 字节头部编解码（1 字节 Type + 4 字节大端 uint32 Stream ID + 变长 Payload），支持 `encodeFrame` 与 `decodeFrame`。

- [ ] **Step 4: 实现 `server/src/lib/incus/tunnel-manager.ts`**

实现 `HostTunnelManager` 单例：
1. 维护 `activeTunnels: Map<number, WebSocket>` 与 `streamMap: Map<string, stream.Duplex>`。
2. `createDuplexStream(hostId, targetHost, targetPort)`：
   - 生成唯一递增 `streamId`。
   - 创建 `stream.Duplex`，其 `_write(chunk, enc, cb)` 打包为 `FRAME_DATA` 写入 WebSocket。
   - 向 WebSocket 发送 `STREAM_OPEN` 帧（携带 targetHost 和 targetPort）。
   - 监听 WebSocket 收到对应 `streamId` 的 `FRAME_DATA` 时执行 `duplex.push(payload)`。
   - 监听 `FRAME_CLOSE` 时执行 `duplex.push(null)`，监听 `FRAME_RESET` 时触发 `duplex.destroy(err)`。
   - 超时保护与流泄漏清理。

- [ ] **Step 5: 运行测试验证通过**

Run: `node --import tsx server/scripts/test-agent-tunnel-protocol-guards.ts && pnpm --filter server type-check`
Expected: PASS

- [ ] **Step 6: 提交 Commit**

```bash
git add server/src/lib/incus/tunnel-protocol.ts server/src/lib/incus/tunnel-manager.ts server/scripts/test-agent-tunnel-protocol-guards.ts
git commit -m "feat(tunnel): implement server-side multiplexed tunnel protocol and manager"
```

---

### Task 3: Server 端 WebSocket 路由 `/api/agent/tunnel` 与心跳下发

**Files:**
- Modify: `server/src/routes/agent.ts:1-120, 800-860`
- Modify: `server/src/routes/hosts.ts:2400-2600`
- Create: `server/scripts/test-agent-tunnel-route-guards.ts`

**Interfaces:**
- Consumes: `HostTunnelManager` from Task 2, `validateAgentHeaders`, `verifyAgentSignature`
- Produces:
  - WebSocket 路由 `GET /api/agent/tunnel`（带 HMAC 鉴权）
  - 心跳响应包含 `tunnel: { enabled: boolean, targetHost: string, targetPort: number }`
  - 宿主机配置更新时触发 `hostTunnelManager.broadcastConfig(hostId, ...)`

- [ ] **Step 1: 编写路由守卫 `server/scripts/test-agent-tunnel-route-guards.ts`**

```typescript
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const agentRoutes = readFileSync(resolve(process.cwd(), 'server/src/routes/agent.ts'), 'utf8')
assert.ok(agentRoutes.includes('/tunnel'))
assert.ok(agentRoutes.includes('hostTunnelManager.registerTunnel'))
assert.ok(agentRoutes.includes('tunnel: {'))
console.log('agent tunnel route guard tests passed')
```

- [ ] **Step 2: 运行测试验证失败**

Run: `node --import tsx server/scripts/test-agent-tunnel-route-guards.ts`
Expected: FAIL

- [ ] **Step 3: 在 `server/src/routes/agent.ts` 中实现 `/tunnel` WebSocket 路由**

1. 接入 `@fastify/websocket` 处理：
   ```typescript
   fastify.get('/tunnel', { websocket: true }, (connection, request) => {
     // 1. 读取并校验 x-incudal-agent-id / x-incudal-signature 等 HMAC 头
     // 2. 查找关联 host，校验 tunnelEnabled
     // 3. 注册到 hostTunnelManager.registerTunnel(host.id, connection.socket)
     // 4. 处理 connection.socket 消息与断开事件
   })
   ```
2. 在心跳处理逻辑（`POST /api/agent/heartbeat`）响应中添加 `tunnel` 字段：
   ```typescript
   tunnel: {
     enabled: host.tunnelEnabled,
     targetHost: host.targetHost,
     targetPort: host.targetPort
   }
   ```
3. 在 `server/src/routes/hosts.ts` 中，当管理员更新 host 的 `tunnelEnabled`、`targetHost` 或 `targetPort` 时，调用 `hostTunnelManager.broadcastConfig` 实时推送热更新帧。

- [ ] **Step 4: 运行测试验证通过**

Run: `node --import tsx server/scripts/test-agent-tunnel-route-guards.ts && pnpm --filter server type-check`
Expected: PASS

- [ ] **Step 5: 提交 Commit**

```bash
git add server/src/routes/agent.ts server/src/routes/hosts.ts server/scripts/test-agent-tunnel-route-guards.ts
git commit -m "feat(agent): register /tunnel websocket endpoint and deliver tunnel config in heartbeat"
```

---

### Task 4: IncusClient 适配内网穿透虚拟流传输

**Files:**
- Modify: `server/src/lib/incus/incus-client.ts:15-100`
- Modify: `server/src/types/incus.ts:10-30`
- Modify: `server/src/routes/hosts.ts:1600-1670`
- Test: `server/scripts/test-incus-client-tunnel-guards.ts`

**Interfaces:**
- Consumes: `hostTunnelManager` from Task 2
- Produces:
  - `IncusClientOptions.tunnelEnabled?: boolean`
  - `IncusClientOptions.hostId?: number`
  - `IncusClientOptions.targetHost?: string`
  - `IncusClientOptions.targetPort?: number`

- [ ] **Step 1: 编写测试守卫 `server/scripts/test-incus-client-tunnel-guards.ts`**

```typescript
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const clientSource = readFileSync(resolve(process.cwd(), 'server/src/lib/incus/incus-client.ts'), 'utf8')
assert.ok(clientSource.includes('tunnelEnabled'))
assert.ok(clientSource.includes('hostTunnelManager'))
assert.ok(clientSource.includes('tls.connect'))
console.log('incus client tunnel guard tests passed')
```

- [ ] **Step 2: 运行测试验证失败**

Run: `node --import tsx server/scripts/test-incus-client-tunnel-guards.ts`
Expected: FAIL

- [ ] **Step 3: 更新 `IncusClient` 传输层**

在 `IncusClient.connect()` 中：
```typescript
if (this.options.tunnelEnabled && this.options.hostId) {
  if (!hostTunnelManager.isTunnelOnline(this.options.hostId)) {
    throw new Error('宿主机内网穿透通道未连接（Agent 离线），无法连接到 Incus API')
  }
  this.agent = new Agent({
    connect: (opts, cb) => {
      try {
        const duplex = hostTunnelManager.createDuplexStream(
          this.options.hostId!,
          this.options.targetHost || '127.0.0.1',
          this.options.targetPort || 8443
        )
        const tlsSocket = tls.connect({
          socket: duplex,
          cert,
          key,
          rejectUnauthorized: false
        })
        cb(null, tlsSocket)
      } catch (err: any) {
        cb(err, null as any)
      }
    },
    headersTimeout: 120000,
    bodyTimeout: 300000
  })
} else {
  // 保持原有直连 Agent
  this.agent = new Agent({
    connect: { cert, key, rejectUnauthorized: false },
    headersTimeout: 120000,
    bodyTimeout: 300000
  })
}
```

并在 `server/src/routes/hosts.ts` 的 `api.hosts.verify` 以及所有创建 `IncusClient` 的工厂函数处，传入 host 的 `tunnelEnabled`、`targetHost`、`targetPort` 与 `hostId`。

- [ ] **Step 4: 运行测试验证通过**

Run: `node --import tsx server/scripts/test-incus-client-tunnel-guards.ts && pnpm --filter server type-check`
Expected: PASS

- [ ] **Step 5: 提交 Commit**

```bash
git add server/src/lib/incus/incus-client.ts server/src/types/incus.ts server/src/routes/hosts.ts server/scripts/test-incus-client-tunnel-guards.ts
git commit -m "feat(incus): adapt IncusClient to route requests over tunnel virtual duplex streams"
```

---

### Task 5: Agent Go 实现（WebSocket 隧道 Worker 与本地转发 Dialer）

**Files:**
- Create: `agent/internal/tunnel/framing.go`
- Create: `agent/internal/tunnel/tunnel.go`
- Create: `agent/internal/tunnel/tunnel_test.go`
- Modify: `agent/cmd/incudal-agent/main.go:1-120`
- Modify: `agent/internal/panel/client.go:30-80`

**Interfaces:**
- Produces:
  - `tunnel.Worker`: 基于服务端下发的 `tunnel.enabled` 动态启停 WebSocket 连接。
  - 收到 `STREAM_OPEN` 时执行 `net.Dial("tcp", targetHost:targetPort)`。
  - 双向 `io.Copy` 数据并封装为 `STREAM_DATA`。
  - 收到 `CONFIG_UPDATE` 时更新本地转发目标。

- [ ] **Step 1: 编写 Go 单元测试 `agent/internal/tunnel/tunnel_test.go`**

测试 5 字节帧的编解码，模拟本地 TCP Echo Server 验证收到 `STREAM_OPEN` 后成功转发 `STREAM_DATA`。

- [ ] **Step 2: 运行测试验证失败**

Run: `cd agent && go test ./internal/tunnel/...`
Expected: FAIL (package does not exist)

- [ ] **Step 3: 实现 `agent/internal/tunnel/framing.go` 与 `agent/internal/tunnel/tunnel.go`**

1. 引入标准轻量 WebSocket 客户端连接（使用 `gorilla/websocket` 或 `nhooyr.io/websocket`，并更新 `go.mod`）。
2. 实现帧编解码 `EncodeFrame` / `DecodeFrame`。
3. 实现 `Worker.Start(ctx)`：
   - 构造 WSS URL（`ws://` 或 `wss://` 由 `panel_url` 协议决定）。
   - 计算 HMAC Header（复用 `internal/protocol` 逻辑）。
   - 拨号并建立长连接，启动后台 30s Ping-Pong。
   - 循环接收帧，分配虚拟流，`net.DialTimeout` 连向本地端口，并发转发。
   - 连接断开时指数退避重连。

- [ ] **Step 4: 在 `panel/client.go` 与 `main.go` 中接入**

心跳响应解析 `tunnel` 字段。若 `tunnel.enabled == true`，通知 `tunnelWorker` 启动并更新目标；若为 `false`，通知 `tunnelWorker` 优雅休眠断开。

- [ ] **Step 5: 运行测试验证通过**

Run: `cd agent && go test ./... && bash scripts/build-release.sh`
Expected: PASS (测试通过且双架构二进制编译成功)

- [ ] **Step 6: 提交 Commit**

```bash
git add agent/
git commit -m "feat(agent): implement websocket reverse tunnel worker and local dialer"
```

---

### Task 6: 前端宿主机添加与配置页面（双模式切换交互）

**Files:**
- Modify: `client/src/views/resources/MyHostCreateView.vue`
- Modify: `client/src/components/host/MyHostConfigTab.vue`
- Modify: `client/src/components/host/HostInfoTab.vue`
- Modify: `client/src/views/resources/MyHostsView.vue`
- Modify: `client/src/locales/zh-CN.ts`
- Modify: `client/src/locales/zh-TW.ts`
- Modify: `client/src/locales/en.ts`

**Interfaces:**
- Produces:
  - 模式切换单选组：`[主动直连] / [内网穿透]`
  - 穿透模式下隐藏公网 IP，默认指定端口 8443，折叠高级本地目标 `127.0.0.1`
  - 状态胶囊展示：`穿透已连接 / 穿透离线`
  - 多语言文案完整映射

- [ ] **Step 1: 在 `client/src/locales/{zh-CN,zh-TW,en}.ts` 中添加国际化文案**

```typescript
// zh-CN.ts:
tunnelMode: '内网穿透 (Agent 反向隧道)',
directMode: '主动直连 (公网 IP)',
connectionMode: '连接方式',
tunnelNotice: '已启用 Agent 反向穿透模式。宿主机无需独立公网 IP，无需防火墙放行入站端口。',
targetPort: '通信端口',
targetHost: '本地转发地址',
tunnelConnected: '穿透已连接',
tunnelDisconnected: '穿透未连接'
```
在 `zh-TW.ts` 和 `en.ts` 中同步添加对应翻译。

- [ ] **Step 2: 验证 i18n 完整性**

Run: `pnpm --filter server test:frontend-i18n-keys`
Expected: PASS

- [ ] **Step 3: 修改 `MyHostCreateView.vue`**

1. 在表单顶部添加模式切换 Radio。
2. 当 `form.tunnelEnabled == true` 时：
   - 隐藏 `hostAddress` 必填验证，将其自动置空或设为 internal。
   - 呈现穿透说明卡片与 `targetPort`（默认 8443）。
   - 高级设置中提供 `targetHost`（默认 127.0.0.1）。
3. 提交请求时将 `tunnelEnabled`, `targetHost`, `targetPort` 发送给后端。

- [ ] **Step 4: 修改 `MyHostConfigTab.vue`**

在宿主机配置标签页中支持同样的开关与端口/本地目标编辑，支持管理员随时保存切换。

- [ ] **Step 5: 修改 `MyHostsView.vue` 与 `HostInfoTab.vue`**

在宿主机卡片与详情页中增加穿透模式标记。
严格保持受锁定表格的 `table-fixed` 与百分比总和 100%。

- [ ] **Step 6: 验证前端守卫与类型检查**

Run: `pnpm --filter client type-check && pnpm --filter server test:frontend-route-guards`
Expected: PASS

- [ ] **Step 7: 提交 Commit**

```bash
git add client/src/views/resources/MyHostCreateView.vue client/src/components/host/MyHostConfigTab.vue client/src/components/host/HostInfoTab.vue client/src/views/resources/MyHostsView.vue client/src/locales/
git commit -m "feat(ui): support direct and websocket tunnel modes in host management views"
```

---

### Task 7: 全流程测试与双端生产构建验证

**Files:**
- Verify all modified server, client, and agent files.

- [ ] **Step 1: 运行全套守卫测试**

Run: `pnpm --filter server test:host-tunnel-schema-guards && pnpm --filter server test:agent-tunnel-protocol-guards && pnpm --filter server test:agent-tunnel-route-guards && pnpm --filter server test:incus-client-tunnel-guards && pnpm --filter server test:frontend-route-guards && pnpm --filter server test:frontend-i18n-keys`
Expected: ALL PASS

- [ ] **Step 2: 运行 Go agent 测试与编译**

Run: `cd agent && go test ./... && bash scripts/build-release.sh`
Expected: ALL PASS

- [ ] **Step 3: 运行前后端类型检查**

Run: `pnpm --filter server type-check && pnpm --filter client type-check`
Expected: ALL PASS

- [ ] **Step 4: 运行前端双端客户端构建**

Run: `pnpm build:client`
Expected: SUCCESS

- [ ] **Step 5: 整理最终提交与文档**

检查 `git status`，确保工作区干净，所有改动原子提交完成。
