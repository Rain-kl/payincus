# 管理端内置 Incus UI 源码集成与内网穿透代理架构设计（Spec）

- **日期**：2026-10-28
- **状态**：已确认（Approved）
- **目标**：将 Incus Web UI 完整源码直接集成至本仓库，仅在控制端（管理端）构建与暴露；管理员在节点详情页右上角点击「IncusUI」按钮后在新标签页打开该节点的原生 UI 控制台，复用管理端现有单 Session 鉴权并通过反向隧道 + 面板 mTLS 证书免二次鉴权直达宿主机 `incusd`。

---

## 1. 架构总览与设计原则

1. **源码本地化与可二开性**：
   - 不依赖构建期外部拉取，也不内置黑盒预编译产物，直接将 Incus UI 源码作为 pnpm workspace 独立子包 `incus-ui/`（`@payincus/incus-ui`）纳入仓库管理。
   - 保持 `incus-ui/` 与现有 `client/`（Vue 3）目录物理隔离，避免 React/Vanilla SCSS 触发 `client/src` 下的 `test:frontend-color-tokens` 守卫，同时确保用户端构建产物 `client/dist/user` 零污染（100% 通过 `test:frontend-dist-boundary-guards`）。
2. **单 Session 统一鉴权（零双 Session）**：
   - 新标签页打开 `/incus-ui/:hostId/ui` 时，与管理端处于同一源（`admin.payincus.com` 或本地管理端端口 `3002`）。
   - 直接读取同源 `localStorage.getItem('token')` 中的管理员 JWT Access Token，并通过 `window.addEventListener('storage')` 实时同步登录/登出状态，避免并发调用 `/api/auth/refresh` 引发 Refresh Token 轮换冲突。
   - 后端所有代理路由强制校验 `fastify.authenticateAdmin`（要求 `role === 'admin'` 且账号状态为 `active`、会话未失效）。
3. **纯内网穿透通道 + 服务端代持 mTLS（仅走反向隧道）**：
   - 浏览器端无需安装任何客户端证书，也无需配置 OIDC。
   - **仅支持内网穿透模式（不再兼容直连 fallback）**：后端收到管理员的代理请求后，直接校验 `hostTunnelManager.isTunnelOnline(hostId)`，强制通过 `hostTunnelManager.createDuplexStream(hostId, targetHost, targetPort)` 建立虚拟流，挂载控制端持有的客户端证书与私钥，与宿主机 `incusd`（默认 `127.0.0.1:8443`）建立端到端 TLS 连接。若该节点隧道未连接，直接拒绝并提示隧道离线。

---

## 2. 模块详细设计

### 2.1 仓库目录与构建流水线（Monorepo 集成）

```text
payincus/
├── pnpm-workspace.yaml           # 新增 'incus-ui'
├── incus-ui/                     # 本地化 Incus UI 源码包 (@payincus/incus-ui)
│   ├── package.json
│   ├── tsconfig.json
│   ├── vite.config.ts            # base: '/incus-ui/'
│   └── src/
│       ├── lib/
│       │   └── payincus-bridge.ts # 统一请求重写、Token 注入、WS 票据续期与下载桥接
│       └── ...                   # Incus UI 核心业务代码（移除无用的 OIDC/证书引导页）
├── scripts/
│   └── build-incus-ui.mjs        # 基于源码内容 Hash 的增量构建脚本 -> client/dist/admin/incus-ui
├── client/
│   └── src/views/resources/MyHostDetailView.vue # 右上角新增「IncusUI」按钮（v-if="isAdmin"）
└── server/
    └── src/routes/admin-incus-ui.ts # 管理员专属 Incus UI HTTP 流式代理与 WebSocket 穿透桥接
```

#### 构建与开发联动：
- **增量构建（`scripts/build-incus-ui.mjs`）**：
  - 计算 `incus-ui/` 下源码文件与配置文件的 SHA-256 摘要。
  - 若摘要未变且构建缓存存在，直接同步至 `client/dist/admin/incus-ui/`；若源码发生修改（二次开发），自动执行 `pnpm --filter @payincus/incus-ui build`。
- **`client/package.json`**：
  - `build:user`：仅构建用户端 Vue 应用至 `client/dist/user`（不含 `incus-ui`）。
  - `build:admin`：构建管理端 Vue 应用至 `client/dist/admin`，随后调用 `node ../scripts/build-incus-ui.mjs` 将 Incus UI 嵌入 `client/dist/admin/incus-ui`。
- **Nginx 路由（`deploy/nginx-split-intranet.conf.example`）**：
  - 在 `admin.payincus.com` 的 `server` 块中：
    - `/incus-ui/` 路径配置 `try_files $uri $uri/ /incus-ui/index.html;`，支持 `/incus-ui/:hostId/ui/...` 前端路由刷新。
    - `/api/admin/hosts/` 下的 `incus-proxy` 接口关闭请求缓冲（`proxy_request_buffering off; proxy_buffering off;`）并放宽超时，支持镜像/备份流式传输。

---

### 2.2 前端运行时桥接层（`incus-ui/src/lib/payincus-bridge.ts`）

为了将对 Incus UI 业务源码的侵入降到最低（方便后续二次开发），在 `incus-ui` 启动入口最先加载 `payincus-bridge.ts`：

1. **上下文提取**：
   - 从 URL 路径 `/incus-ui/:hostId/ui/...` 解析当前 `hostId`（正整数校验），并将 React Router 的 `basename` 动态设为 `/incus-ui/${hostId}`。
   - 若 `localStorage.getItem('token')` 不存在，直接重定向回管理端登录页 `/login?redirect=...`。
2. **`window.fetch` 与 `XMLHttpRequest` 统一重写**：
   - 当请求 URL 为相对路径 `/1.0/...` 或同源 `/1.0/...` 时，自动重写为：
     `/api/admin/hosts/${hostId}/incus-proxy/1.0/...`
   - 自动附加 `Authorization: Bearer ${localStorage.getItem('token')}`。
   - 针对 `/1.0` 的服务端信息探测请求（Incus UI 用 `GET /1.0` 判断 `auth: "trusted"`），由于后端通过 mTLS 证书请求宿主机 `incusd`，宿主机原生返回 `auth: "trusted"`，Incus UI 自动跳过所有登录/证书引导，直接进入主控台。
3. **`window.WebSocket` 同步重写与子协议票据传递**：
   - `payincus-bridge.ts` 初始化时调用 `POST /api/admin/hosts/${hostId}/incus-ui-ticket` 预取一个绑定当前 `(adminUserId, sessionId, hostId)` 的短期多路复用票据 `wsTicket`（TTL 5 分钟，每 3 分钟自动静默刷新）。
   - 拦截 `new WebSocket(url, protocols)`：当 `url` 指向 `/1.0/events` 或 `/1.0/operations/.../websocket` 时，同步将地址重写为：
     `ws(s)://<current-host>/api/ws/hosts/${hostId}/incus-proxy/1.0/...`
   - 将票据注入 `Sec-WebSocket-Protocol`（格式：`incudal-incus-ui.${wsTicket}`，同时保留原有的 `binary` 等子协议），避免票据出现在 URL 和反向代理日志中。
4. **原生文件下载（`<a href="/1.0/.../export">`）桥接**：
   - 监听全局点击或重写下载 URL，为 `/1.0/` 下的导出/文件下载链接附加单次短期下载票据，确保浏览器原生下载流正常工作。

---

### 2.3 后端内网穿透流式代理（`server/src/routes/admin-incus-ui.ts`）

1. **安全与权限边界**：
   - 严格校验 `hostId` 为合法正整数（复用 `POSITIVE_ROUTE_ID_PATTERN`）。
   - HTTP 路由强制挂载 `onRequest: [fastify.authenticateAdmin]`；WebSocket 与原生下载路由通过 `action-ticket.ts` 验签后，二次校验 `isAccessTokenInvalidated` 及数据库实时用户状态（必须为 `active` 且 `role === 'admin'`）。
   - 受演示环境只读保护（`shouldBlockDemoMutation`）：在 Demo 模式下拦截所有非 `GET/HEAD` 的代理写请求。
   - 路径白名单：仅允许代理 `/1.0` 及其子路径（如 `/1.0/instances`、`/1.0/events`、`/1.0/operations/...`），严禁代理 `/1.0/payincus` 内部控制扩展或包含 `..` 的路径穿越请求。

2. **零拷贝 HTTP 流式转发（`ALL /api/admin/hosts/:hostId/incus-proxy/*`）**：
   - 强制要求该宿主机反向隧道在线（`hostTunnelManager.isTunnelOnline(hostId)`），不再走直连分支。
   - 在该路由插件作用域内注册流式 Content-Type 解析器，不将请求体读入内存 Buffer，直接将 `request.raw` 流式转发给基于 `hostTunnelManager.createDuplexStream(hostId, targetHost, targetPort)` 创建的隧道 mTLS Dispatcher。
   - 透传状态码、必要响应头（过滤掉宿主机可能返回的冲突安全头与 Hop-by-Hop 头），并将 `response.body` 直接 pipe 到 `reply.raw`。
   - **审计日志**：当管理员通过 Incus UI 发起变更请求（`POST` / `PUT` / `PATCH` / `DELETE`）时，自动写入系统日志（`LogModule.HOST`，动作 `host.incus_ui_mutation`，记录 `method`、`path` 与 `hostId`，不记录敏感载荷）。

3. **WebSocket 全双工隧道桥接（`GET /api/ws/hosts/:hostId/incus-proxy/*`）**：
   - 校验 Origin（`validateWebSocketOrigin`）、IP/用户连接数限制及 `Sec-WebSocket-Protocol` 中的 `incudal-incus-ui.<ticket>`。
   - 强制要求隧道在线，通过 `hostTunnelManager.createDuplexStream(hostId, targetHost, targetPort)` + `tls.connect({ cert, key })` 连接宿主机 `incusd` 对应的 `/1.0/events` 或 `/1.0/operations/:uuid/websocket?secret=...`。
   - 在浏览器 WebSocket 与宿主机 WebSocket 之间建立双向消息/二进制帧转发与背压控制；任一端关闭或报错时立即清理对端连接与虚拟流。

---

### 2.4 控制端节点详情页入口（`MyHostDetailView.vue`）

- **按钮位置**：在 `client/src/views/resources/MyHostDetailView.vue` 右上角操作区（“批量延期”按钮旁）新增 `IncusUI` 按钮。
- **显示与可用条件**：
  - 仅管理员可见：`v-if="isAdmin"`。
  - 仅当节点反向隧道在线（`host.tunnelOnline`）且已完成验证时可点击；隧道离线时按钮置灰禁用并提示需隧道在线。
- **点击行为**：
  - 点击后调用 `window.open(`/incus-ui/${host.value.id}/ui`, '_blank', 'noopener,noreferrer')`，在新标签页直接进入该节点的 Incus UI。
- **国际化与主题**：
  - 在 `zh-CN.ts`、`zh-TW.ts`、`en.ts` 中补齐对应文案 key（通过 `test:frontend-i18n-keys` 守卫）。
  - 按钮使用现有语义类名 `btn-secondary btn-sm sm:btn`，不引入任何硬编码颜色或半透明写法（通过 `test:frontend-color-tokens` 与 `test:frontend-route-guards` 守卫）。

---

## 3. 守卫测试与 OrbStack `debian-test` 真机 E2E 验证计划

1. **新增守卫测试 `server/scripts/test-incus-ui-proxy-guards.ts`**：
   - 验证非管理员、未登录、封禁账号、已注销会话访问 `/api/admin/hosts/:hostId/incus-proxy/*` 与 WebSocket 端点均被 401/403 拦截；
   - 验证隧道未连接（`!hostTunnelManager.isTunnelOnline(hostId)`）时直接拒绝访问；
   - 验证非法 `hostId`、路径穿越（`..`）、访问内部扩展路径 `/1.0/payincus` 均被 400/403 拒绝；
   - 验证 HTTP 读写流式转发与 WebSocket 双向桥接在反向隧道模式下正常工作。
2. **既有守卫回归**：
   - `pnpm --filter server test:frontend-route-guards`
   - `pnpm --filter server test:frontend-color-tokens`
   - `pnpm --filter server test:frontend-i18n-keys`
   - `pnpm --filter server test:frontend-dist-boundary-guards`
   - 双端构建与类型检查：`pnpm --filter client type-check`、`pnpm --filter server type-check`、`pnpm build`。
3. **OrbStack 虚拟机 `debian-test` 端到端真机实测（E2E）**：
   - 对接本地 OrbStack 中的 `debian-test` 虚拟机（运行 `incusd` 与反向隧道连接器）；
   - 通过管理端点击 `debian-test` 节点的「IncusUI」按钮打开新标签页；
   - 实测免鉴权直接加载 Incus UI 首页、查看 `debian-test` 上的实例/存储/网络列表、实时事件流（`/1.0/events` WebSocket）以及在 Incus UI 中打开实例终端（`/1.0/operations/.../websocket`）全链路穿透连通。
