# Go 后端迁移蓝图（GO_MIGRATION.md）

> 目标：把控制面板后端（`server/`，Node + Fastify + Prisma，约 **145,000 行 TS**、**311 个文件**、**130 个 Prisma 模型 / 66 个枚举**、**65 个路由文件**）**逐行翻译**为 Go 程序。
> 策略：**两版并行** —— Node 版继续承载生产，Go 版在 `server-go/` 独立开发；**全部接口行为对齐后**再替换。
> 顺序：**先对齐模型 → 再做接口 → 最后实现逻辑**。
> 本文件是团队的施工依据；与 `AGENTS.md`（统一规矩）、`docs/TEAM.md`（角色分诊）配套。

---

## 0. 现状（已落地，可运行）

`server-go/` 已完成「地基 + 模型对齐 + 一个端到端参考模块」，`go build ./...`、`go vet ./...`、`go test ./...` 全绿。

| 产物 | 路径 | 说明 |
|------|------|------|
| 模型对齐 | `server-go/src/models/models_gen.go` | **130 模型 / 66 枚举**，由 `tools/prismagen` 从 `server/prisma/schema.prisma` 生成，`json` tag = Prisma 字段名（camelCase）→ API 字段零成本对齐 |
| 生成器 | `server-go/tools/prismagen/main.go` | `schema.prisma` → Go struct；`@map`→列名、`@id`→primaryKey、关系字段→`gorm:"-"` |
| Decimal | `server-go/src/models/decimal.go` | Prisma Decimal 语义（JSON 序列化为**字符串**） |
| 错误目录 | `server-go/src/lib/errors_gen.go` | **254 个错误码 + 文案**，从 `lib/errors.ts` 生成；`APIErrorFor` 复刻 `apiError()`（内部错误不泄详情、details 截断 256） |
| 限流配置 | `server-go/src/config/rate_limit_gen.go` | **80 条路由规则 + 全局 300/min + 白名单**，从 `config/rate-limit.ts` 生成 |
| 基础设施 | `src/config`、`src/lib`、`src/db`、`src/middleware` | env 加载、日志脱敏、demo 安全、trust-proxy、cookie/origin 配置、JWT、token 失效、CORS、安全头、限流、demo 只读/脱敏、Recovery |
| 入口 | `server-go/src/app.go`、`cmd/server/main.go` | Gin 路由 + 中间件链（顺序对齐 `app.ts`）+ `/api/health` + Fastify 同款 404 |
| 参考模块 | `src/routes/announcements.go`、`src/db/announcements.go` | 端到端样例：鉴权、管理员校验、分页、路由 ID 严格解析、错误码 |
| 对拍工具 | `server-go/test/conformance/` | **双跑对拍**：Node 与 Go 连同一 DB，逐请求 diff 状态码与 JSON |
| 单元测试 | `src/lib/lib_test.go`、`src/models/models_test.go`、`src/app_test.go` | 错误目录/脱敏/demo/限流/模型计数/健康检查/鉴权/404 |
| **Wave 1 地基（已完成）** | `src/lib/log_localization.go`、`src/lib/risk_audit.go`、`src/db/logs.go`、`src/db/advisory_locks.go`、`src/db/system_config.go`、`src/lib/config_cache.go`、`src/db/balance.go`、`src/lib/operation_verification.go` | 共享地基 + 单测（`go test ./...` 全绿） |
| **Wave 2 地基（已完成）** | `src/lib/security.go`（1753 行，含 AES-GCM 加解密/2FA/登录锁/审计/refresh token）、`src/lib/security_config.go`、`src/lib/runtime_settings.go`、`src/lib/turnstile.go`、`src/lib/distributed_lock.go`、`src/lib/outbound_security.go`、`src/lib/public_api_auth.go` | 鉴权与安全地基，`go test -race ./...` 全绿 |
| **Wave 2 首批模块（已完成）** | `src/services/vip_levels.go` + `src/routes/vip_levels.go`、`src/db/images.go` + `src/routes/images.go`、`src/db/terminal_saved_commands.go` + `src/routes/terminal_saved_commands.go` | 端到端路由模块，含对拍用例 |

### 已确认的关键事实（勿再重复踩）

- **Fastify AJV 默认值**（`@fastify/ajv-compiler/lib/default-ajv-options.js`，已实测核对）：`coerceTypes:'array'`、`useDefaults:true`、`removeAdditional:true`、`allErrors:false`。含义：**未知字段被静默丢弃而非报错**、标量会被字符串化、单元素数组会解包、**只报第一个校验错误**。带 `schema.body` 的路由必须复刻这套语义（见 `routes/terminal_saved_commands.go`）。
- **AES-GCM 互通已实证**：key=`sha256(ENCRYPTION_KEY || (prod ? "" : JWT_SECRET || 'default-encryption-key'))`，16B IV，密文 `iv:tag:ct`（hex）；Go 加密 → Node 解密已实测通过。
- **TOTP/otpauth URI 已实测比对** otplib v12（`window:0`，拒绝上一周期令牌）。

### 待接线 / 已知缺口（下一步）

- `lib/mailer.go`、`lib/notifier.go` 未移植 → `lib/operation_verification.go` 预留了 `OperationVerificationEmailSender` / `OperationVerificationNotificationSender` seam（默认返回显式失败，不静默成功），落地后由主程接线。
- `lib/invite_pricing.go` 待办（依赖 `db/advisory_locks.go` + `db/system_config.go`，二者已就绪）。
- `lib/operation_verification.go` 内有一份**未导出的** advisory-lock 副本，待与 `db.AdvisoryTransactionLockString` 归一。
- `vip-levels` 的每路由限流（60/30/20 per minute）在 TS 里是路由内联声明，**不在** `config/rate-limit.ts`，所以 Go 侧目前只受全局 300/min 约束——需在 `app.go` 的路由注册处按 TS 补齐。
- 静态托管（生产 `SERVE_STATIC_CLIENT`：index.html / 预览机器人降级 / 缓存头）尚未移植。

**参考模块就是「翻译模板」**：新增模块请照 `announcements` 的四层结构写（route → db → 复用 lib/middleware）。

---

## 1. 技术选型（已定，附理由）

| 关注点 | 选型 | 理由 |
|--------|------|------|
| HTTP 框架 | **Gin** | 生态最成熟、中间件/分组模型与 Fastify 的 `onRequest`/`preHandler` 一一对应 |
| ORM / 驱动 | **GORM v2 + pgx/v5（`stdlib.OpenDBFromPool`，单池）** | Prisma 的链式 API 最易逐行翻译；pgx 池同时服务 GORM 与手写 SQL，复刻 Node「一个 pg Pool 服务 Prisma + 原生 SQL」 |
| 模型来源 | `schema.prisma`（生成器） | 模型唯一事实来源仍是 Prisma；DB 迁移仍由 Prisma 负责，**Go 不做 AutoMigrate** |
| 金额 | `shopspring/decimal` + 自定义 `Decimal`（JSON 字符串） | 对齐 Prisma Decimal 的 JSON 形态 |
| JWT | `golang-jwt/jwt/v5` | 对齐 fastify-jwt（HS256 + `JWT_SECRET`，claims：`id/username/role/status/sid/iat/exp`） |
| 口令 | `golang.org/x/crypto/bcrypt` | 与 bcryptjs 哈希互相兼容 |
| 定时 | `robfig/cron/v3` | 对齐 node-cron |
| WebSocket | `gorilla/websocket` | 成熟 |
| 校验 | `go-playground/validator` + `santhosh-tekuri/jsonschema/v6` | 对齐 Fastify JSON Schema 校验 |
| 限流 | 进程内固定窗口（自研 ~80 行） | **架构不变量是单实例**（AGENTS.md），内存态即正确；对齐 `@fastify/rate-limit` 的 `ip` / `ip:rulePath` 键 |

> 依赖已固定：Gin 1.12 · GORM 1.31 · pgx 5.11 · decimal 1.6 · jwt 5.3 · zerolog。

---

## 2. 目录映射（保持结构/文件名不变）

Node 的 `server/src/**/*.ts` 一一映射到 `server-go/src/**/*.go`：

```
server/src/app.ts               →  server-go/src/app.go              （package app，import 时别名 app）
server/src/routes/users.ts      →  server-go/src/routes/users.go
server/src/db/users.ts          →  server-go/src/db/users.go
server/src/services/vip-levels.ts → server-go/src/services/vip_levels.go
server/src/lib/security.ts      →  server-go/src/lib/security.go
server/src/config/rate-limit.ts →  server-go/src/config/rate_limit_gen.go（生成）+ rate_limit.go（helper）
```

- Go 不允许 `kebab-case` 文件名（非测试文件），统一改成 `snake_case`（`vip-levels.ts` → `vip_levels.go`）。
- `src/` 根目录的包名是 `app`（目录名与包名不一致，import 时用别名 `app "…/server-go/src"`）。
- 生成文件（`*_gen.go`）**不得手改**；改 TS 源后重新生成。

---

## 3. 每个模块的施工配方（必须照做）

1. **先读后写**：读 Node 源文件 + 对应 `test-*-guards.ts`（把守卫里的**业务不变量**（如严格正整数 ID 解析）翻译成 Go 行为）。
2. **对齐接口**：把该模块的请求/响应形态加入 `test/conformance/requests*.json`（**先钉契约，再写实现**）。
3. **实现**：`routes/X.go`（鉴权中间件、参数解析、错误码、状态码）+ `db/X.go`（查询，必要时用 `db.DB.Raw` 手写 SQL 以保证字段投影与 Prisma `select` 一致）+ 需要时 `services/X.go`。
4. **本地测试**：新增 `*_test.go`（不依赖 DB 的部分）+ 需要 DB 的部分走对拍。
5. **对拍**：起 Node（3001）+ Go（3002）连同一 DB，跑 `test/conformance`，**逐接口 diff 必须为零**。
6. **完成定义**：`go build ./... && go vet ./... && go test ./...` 全绿 **且** 该模块对拍用例全绿。

### 关键等价规则（易踩）

- **路由 ID**：`lib.ParsePositiveRouteID` 严格匹配 `^[1-9]\d*$`（拒绝 `0`、`-1`、`01`）——对应守卫 `test-user-route-id-guards` 等。**禁止** `strconv.Atoi` 裸解析。
- **错误响应**：一律 `lib.RespondAPIError(c, status, code, details)`；文案取自 `ErrorMessages`（生成，**不得手写**）。
- **Decimal**：金额字段用 `models.Decimal`，JSON 必须是字符串。
- **JSON 形态必须与 TS 逐字对齐（最高频的坑）**：
  - TS 里**直接返回 Prisma 模型/实体** → 其中的 Decimal 字段在 JSON 里是**字符串** → Go 用 `models.Decimal`。
  - TS 里显式 `.toNumber()` / `Number(x)` / `parseFloat(...)` 出来的 → JSON 里是**数字** → Go 用 `float64`。
  - 判据：看 TS 函数的**返回类型标注**（`Promise<number>` vs `Promise<SomeModel>`）。例：`db/balance.ts` 的 `getUserBalance`/`getUserConsumeStats`/`getUsersTotalConsumeMap` 全是 number。
- **禁止 import cycle（db ↛ lib）**：`lib` 已经 import `db`（`token_invalidation.go` 等），所以 **`db` 包不得 import `lib`**。若 `db` 层需要 `lib` 的能力（日志归一化/风险分级/配置缓存失效），用**注册钩子**模式：在 `db` 侧声明 `var xxxHook func(...)` + `RegisterXxxHook(...)`，由 `lib` 侧在 `init()` 里注册实现（已用于 `db/logs.go`、`db/system_config.go`）。
- **Host**：Go 把 Host 放进 `c.Request.Host`（不在 header map）——demo 判定已据此实现，勿改回 `GetHeader("Host")`。
- **分页**：`{items,total,page,pageSize,totalPages}`，`totalPages = ceil(total/pageSize)`。
- **404**：`/api/*` → `{"error":"API 路由不存在"}`；`/cdn-cgi/*` → 204（已在 `app.go` 实现）。
- **禁止**在 Go 里做 AutoMigrate / 改 schema；schema 由 Prisma 迁移管理。

---

## 4. 验证策略（重点：Node 守卫**不能**验证 Go）

**事实**：`server/scripts/test-*.ts` 中 200+ 个「守卫」绝大多数是**静态扫描 TS 源码的字符串断言**（读 `server/src/routes/users.ts` 断言正则片段），**无法**验证 Go 二进制。因此：

| 证据层 | 手段 |
|--------|------|
| 模型对齐 | `prismagen` 生成 + `models_test.go` 计数（130/66）+ 字段抽样 |
| 配置对齐 | `errors_gen.go`（254）、`rate_limit_gen.go`（80）从 TS 源生成，天然同源 |
| 接口行为对齐 | **双跑对拍**（`test/conformance`）：同 DB、同请求、diff 状态码与 JSON —— **这是兼容性的唯一硬证据** |
| 业务不变量 | 把守卫里的不变量翻译成 Go 单测（如 ID 解析、内部错误不泄详情、demo 只读） |
| 上线前 | 全量对拍 + 真机跑通受影响流程（AGENTS.md §8） |

> 现有 Node 守卫**继续保留并继续通过**（Node 版仍在跑）；Go 版不要求、也无法「跑通」这些 TS 守卫。新增 Go 侧测试才是 Go 版的完成依据。

---

## 5. 风险清单（高危，须架构师把关）

1. **单实例不变量**：登录失败锁、OAuth state nonce、一次性登录码防重放、导出任务状态、交割 finalize 去重、config 缓存**均为进程内态**（`server/src/lib/security.ts`、`workers/exchangeDeliveryWorker.ts`、`lib/config-cache.ts`）。Go 版必须同样单实例，且这些内存态语义要逐一复刻。
2. ~~**金额/Decimal 序列化**~~ **已实测确认（2026-10-29）**：用真实 `@prisma/client` 实测 `JSON.stringify(new Prisma.Decimal('100.00'))` → `"100"`、`'12.50'` → `"12.5"`、`'0'` → `"0"` —— **Prisma 的 Decimal 序列化为「去尾零的 JSON 字符串」**，与 `shopspring/decimal` 的 `String()` 行为一致，`models.Decimal` 无需改动。**但**：TS 里凡是 `.toNumber()`/`Number(...)` 出来的（如 `getUserBalance(): Promise<number>`、`UserConsumeStats`、`getUsersTotalConsumeMap`）必须返回 Go `float64`（JSON 数字），**不能**返回 `models.Decimal`。
3. **校验文案**：Fastify schema 校验错误的文案映射（`app.ts` setErrorHandler 的 `minLength/pattern/format/required` 分支）需逐条复刻。
4. **鉴权面**：三套鉴权（Web JWT、Public API `pat_/poa_`、host agent HMAC）+ cookie 语义 + token 失效表语义。
5. **WebSocket**：终端/SPICE/Incus UI 隧道（`terminal-proxy`、`spice-sessions`、`websocket-security`、`maxPayload`、单 IP 连接数）。
6. **调度/worker**：15 个 scheduler/worker + PG advisory lock 语义（`db/advisory-locks.ts`）。
7. **静态托管**：生产 `SERVE_STATIC_CLIENT` 的 index.html/预览机器人降级/缓存头（`app.ts` 尾部）尚未移植。
8. **Prisma 原生 SQL**：105 处 `$queryRaw/$executeRaw` 需逐条翻译并核对结果类型。

---

## 6. 模块清单与波次（按依赖排序）

**Wave 1 — 共享地基**（被绝大多数路由依赖，必须最先做）
`db/logs`（`createLog`，~30 路由依赖）、`lib/security`（剩余：口令校验/登录锁/审计/加解密/2FA/OAuth state）、`db/system-config` + `lib/config-cache`、`db/balance`、`lib/permission`、`db/advisory-locks`、`lib/notifier`、`lib/mailer`、`lib/operation-verification`、`lib/audit`。

**Wave 2 — 叶子模块**（低/中危、依赖少、可并行）
`vip-levels`(81)、`vip-benefits`(126)、`egress-profiles`(89)、`storage-encryption`(42)、`admin-tasks`(104)、`terminal-saved-commands`(174)、`admin-oauth-apps`(177)、`api-tokens`(178)、`images`(221)、`logs`(226)、`system-update`(275)、`admin-notification-channels`(286)、`promo-codes`(307)、`custom-init-commands`(313)、`verification`(331)、`ssh-keys`(361)、`storage-configs`(414)、`resource-pool`(482)、`snapshots`(491)、`checkin`(507)、`friends`(544)、`help`(666)、`ip-addresses`(886)、`balance`(779)、`entertainment`(782)、`notifications`(607)、`inbox`(578)、`gift-cards`(559)、`redeem-codes`(610)、`orders`(814)、`tickets`(1014)、`proxy-sites`(986)、`telegram`(1000)、`transfers`(1152)、`user-invites`(240)、`admin-statistics`(686)、`batch-config`(682)、`aff`(581)、`terminal`(527)。

**Wave 3 — 高危**（先出 spec 再实现；鉴权/支付/权限/风控/资源交付/兑换/实例创建/OTA）
`auth`(1436)、`users`(1572)、`instances`(6894)、`hosts`(7987)、`packages`(2876)、`public-api`(2396)、`recharge`(3027)、`admin-billing`(4786)、`agent`(1042)、`mail`(2361)、`instance-billing`(1080)、`instance-destroy`(1337)、`backups`(1467)、`oauth`(748)、`oauth-provider`(328)、`node-security`(162)、`spice`(123)、`admin-incus-ui`(723)、`traffic`(609)、`system-config`(941)、`admin-entertainment`(1568)、`admin-promo-codes`(680)、`egress`(155)。

---

## 7. 团队与文件所有权（避免冲突）

沿用 `docs/TEAM.md` 的角色分诊。**硬性规则**：

- **同一文件同一时刻只允许一个 agent 修改**。共享文件（`app.go`、`lib/errors*`、`config/*`、`models/*_gen.go`、生成器）由**主程独占**，模块 agent 不得改。
- 模块 agent 只允许**新增**自己的 `routes/X.go`、`db/X.go`、`services/X.go`、`*_test.go` 与 `test/conformance/requests-<module>.json`；需要公共 helper 时**在 PR 里提需求**，由主程加进共享文件。
- 生成物（`*_gen.go`）只能由生成器产出。
- 每个 agent 交付前必须自跑：`cd server-go && gofmt -l src cmd tools`（应为空）、`go build ./...`、`go vet ./...`、`go test ./...`。

### 角色映射

| 角色 | 职责 |
|------|------|
| 技术架构师（主程） | 定选型/不变量/spec、守共享文件、审 diff、对拍与真机验证、管 git |
| 后端开发（子代理） | 按配方实现 Wave 1/2 模块（低中危，可并行） |
| QA | 维护对拍用例、把 Node 守卫不变量翻译成 Go 测试、对抗式复核假阳性 |
| 运维 | 部署/systemd/端口编排（Go 版先跑 3002，与 Node 并存） |

---

## 8. 完成定义（DoD）

1. `go build ./... && go vet ./... && go test ./...` 全绿；
2. 该模块的**对拍用例全绿**（同 DB、同请求，状态码与 JSON 逐字段一致）；
3. 涉及高危路径者已出 spec 并由主程审过；
4. 真机跑过受影响流程；
5. 报告：改了哪些文件 / 为什么 / 风险 / 怎么验证的。

以上任一未满足 = **未完成**。
