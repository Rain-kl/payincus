# Wave D 高危模块架构 Spec

> 依据 `AGENTS.md` §2.3「高危路径先读后写」与 `docs/TEAM.md` 的角色分诊：鉴权 / 支付 / 权限 / 风控 / 资源交付 / 兑换 / 实例创建 / OTA 六大高危面在动工前必须先有本 Spec，实现必须逐条对齐。
> 本文件是 Wave D 的施工与验收依据；每完成一个模块，在主文档 `docs/GO_MIGRATION.md` §9 勾选并附对拍数字。

---

## 0. 全局不变量（所有 Wave D 模块共同约束）

### 0.1 单实例运行

`AGENTS.md` 明确：私有后端必须且只能运行 **1 个进程**。以下状态在 Node 版是**进程内**的，Go 版**同样如此**，不得改为多副本：

| 状态 | 位置 | Go 实现 |
|---|---|---|
| 登录失败锁 | `lib/security.ts` | `lib/security.go` 的进程内 map + mutex（已移植） |
| OAuth state nonce | `lib/security.ts` | 同上（已移植） |
| 一次性登录码防重放 | `lib/security.ts` | 同上（已移植） |
| 导出任务状态 | `lib/security.ts` | 同上（已移植） |
| 交割 finalize 去重 | `workers/exchangeDeliveryWorker.ts` | Wave E；同样进程内 |
| config 缓存 | `lib/config-cache.ts` | `lib/config_cache.go`（已移植） |

**验收动作**：任何「把上述状态挪到 PostgreSQL 以支持多副本」的改动都属于架构变更，必须先经 owner 批准。Go 版不得擅自"改进"。

### 0.2 三套鉴权面（互不通用）

| 面 | 凭据形态 | 校验方式 | 生效范围 |
|---|---|---|---|
| Web | `Authorization: Bearer <JWT>` + refresh cookie | HMAC-SHA256，`JWT_SECRET` | `/api/*`（除 public / agent / public-api） |
| Public API | `pat_` / `poa_` 前缀 token | **库内哈希比对**，不走 JWT | `/api/public/*` |
| Host Agent | HMAC 签名（method + path + body + timestamp） | 共享密钥 | `/api/agent/*` |

**红线**：三者不得互相回退。Web token 不能访问 Public API，Agent 签名不能访问 `/api/*`。每条都要有反向测试（用 A 面凭据打 B 面，断言 401）。

### 0.3 金额路径

- 所有余额变动必须在**同一事务**内完成，并先取 advisory lock（命名空间见下表）。
- 变动前后的余额都要落 `balance_logs`，`balanceBefore` / `balanceAfter` 是快照，不是增量。
- 幂等键：凡可重试的入账（交割、退款、礼品卡）必须带幂等标记，重复执行不得二次入账。
- Decimal 一律用 `models.Decimal`（shopspring），**不得**用 float64 参与计算；对外的 `number` 字段在最后一步转换。

### 0.4 Advisory lock 命名空间（已实证）

| 常量 | 值 | 用途 |
|---|---|---|
| `USER_BALANCE_LOCK_NAMESPACE` | 4110 | 主余额变动 |
| `USER_POINTS_LOCK_NAMESPACE` | 4111 | 积分变动 |
| `USER_AFF_BALANCE_LOCK_NAMESPACE` | 4112 | 推广余额 |
| `GIFT_CARD_LOCK_NAMESPACE` | 4120 | 礼品卡 |
| `REDEEM_CODE_LOCK_NAMESPACE` | 4104 | 兑换码 |
| `INSTANCE_OPERATION_LOCK_NAMESPACE` | 4106 | 实例操作 |
| `IP_ADDRESS_ALLOCATION_LOCK_NAMESPACE` | 4119 | IP 分配 |
| `INSTANCE_TRAFFIC_RESET_LOCK_NAMESPACE` | 4121 | 流量重置 |
| `USER_VIP_BENEFIT_CLAIM_LOCK_NAMESPACE` | 4115 | VIP 奖励领取 |
| `TRANSFER_CREATE_LOCK_NAMESPACE` | 4103 | 转移创建 |
| egress 包绑定 | 74201（**单参数形式**） | 出站策略绑定 |

> ⚠️ 74101/74201 类走的是**单参数** `pg_advisory_xact_lock(bigint)`，与两参数形式处于不同锁空间。文档/代码注释必须写明，否则后人"复用 helper"会静默失去互斥。

---

## 1. `db/users.go`（Wave D 第 1 步，其余模块的依赖）

**TS 源**：`server/src/db/users.ts`（862 行，22 个导出函数）

### 必须整文件移植，不得只挑需要的
理由：`users.ts` 的 2FA 状态机（`save2FASecret` → `enable2FA` → `save2FARecoveryCodes` → `enable2FAWithRecoveryCodes`）分四步写库，任何一步缺失都会让某条自助流程半途而废。历史上 Node 版正是因为补丁式添加而出现过「开了 2FA 但恢复码没落库」的窗口。

### 契约
- `findUserByUsername` / `findUserByUsernameOrEmail` / `findUserById` / `findUserByEmail` → 未命中返回 `nil, nil`（不是 error）。
- `createUser` / `createRegisteredUser` → 唯一约束冲突必须映射为可识别的 sentinel，供 routes 返回 **400 / `USER_EXISTS`**（实测：`auth.ts` 用的是 400 `USER_EXISTS`；仓库里不存在 `USERNAME_EXISTS` 这个码，本文件早期版本写错，已改正）。
- `getUserQuota` / `createUserQuota` / `updateUserQuotaLimits` → 配额字段含 bigint（流量），必须 int64。
- `updateUserStatus(id, status, banReason)` → 封禁时必须同时失效该用户全部 access token（Node 在 route 层做，Go 要保持同一位置，不要下沉）。
- `getAllAdminUserIds()` → `role='admin' AND status='active'`，顺序无关但要去重。
- 2FA 相关六个函数按上表状态机实现，**secret 与恢复码都存加密值**（复用 `lib/security.go` 的 AES-GCM）。

### 验收
- 单测覆盖状态机四步 + 回滚（`disable2FAComplete` 清空 secret 与恢复码）。
- 对拍：`/api/users/*` 未移植前，先移植 `db/users.go` 并给一个临时只读路由做自检，或等 `routes/users.go` 一起对拍。

---

## 2. `mail`（Wave D 第 2 步）

**TS 源**：`server/src/routes/mail.ts`（2361 行）

### 高危点
邮件发送涉及**外部服务 + 用户身份**，且 `mail.ts` 同时承载了三个不同的关注点：邮箱域名管理、订阅（subscription）、以及实际的收发。移植时必须按关注点分层，否则 2361 行会变成一个不可审的文件。

### 契约要点
- 域名所有权验证（DNS TXT）必须先查后写，且**验证失败不得落库为已验证**。
- 每个邮箱的配额与计费走 `MAIL_SUBSCRIPTION_LOCK_NAMESPACE = 4109`。
- 发信失败必须回滚对应的额度占用，不能"发失败但扣了额度"。

### Spec 先行要求
动工前需 owner 确认：**是否有生产邮件域名/凭据需要迁移**。若需要，属于 §0.1 的运维动作，必须先出迁移步骤再动代码。

---

## 3. `auth`（Wave D 第 3 步）

**TS 源**：`server/src/routes/auth.ts`（1436 行）

### 必须逐条复刻的行为（都是安全控制）
1. **登录失败锁**：同一 identifier 连续失败 N 次后锁定，锁是**进程内**的。Go 侧 `lib/security.go` 已实现，routes 必须调用而非另建。
2. **Turnstile**：`lib/turnstile.go` 已移植，含 `SetTurnstileClientIPResolver` 接线到 `middleware.ClientIP`。不得绕过。
3. **2FA**：TOTP 窗口 `window: 0`（拒绝上一周期令牌），已实证与 otplib v12 字节一致。
4. **刷新令牌**：refresh cookie 的 `httpOnly` / `sameSite` / `secure` 三属性由 `lib/cookie_config.go` 统一决定，不得在 route 里手写。
5. **OAuth state**：一次性 nonce，用后即焚。
6. **注册开关**：`isRegistrationEnabled()` + 邀请码双闸门。

### 反向测试（必做）
- 用 Public API token 打 `/api/auth/*` → 401
- 用过期的 Web token → 401
- 用已失效（`token_invalidations` 命中）的 token → 401
- 用 Turnstile 校验失败的请求 → 403

---

## 4. `hosts` → `packages` → `instances`（Wave D 主体，依赖链）

这是整个后端最大的三块（7987 + 2873 + 6894 行），必须按序做，因为：

```
hosts      ← 节点连接、健康、隧道控制面、存储池
  ↓
packages   ← 套餐绑定 host、容量策略、出站策略
  ↓
instances  ← 创建/销毁/迁移，全部资源交付动作
```

### 4.1 共同契约
- **资源交付必须是「先占位后执行再确认」**：DB 里先写意图（creating），再调 Incus，成功改 running，失败走补偿（`instance-provisioning-compensation.ts`）。
- **补偿必须幂等**：重复执行不得重复扣费或重复释放配额。
- 所有 Incus 调用走 `lib/incus_client.go`；带指纹的主机走 `lib/node_security_transport.go` 的固定拨号。

### 4.2 逐个模块的前置
| 模块 | 前置 | 必须读的 TS |
|---|---|---|
| `hosts` | 无 | `db/hosts.ts`、`lib/host-health.ts`、`lib/incus/tunnel-manager.ts` |
| `packages` | hosts | `db/packages.ts`、`db/package-hosts.ts`、`lib/storage-capacity.ts` |
| `instances` | hosts + packages + instance-billing | `db/instances.ts`、`lib/managed-instance-provision.ts`、`lib/instance-provisioning-compensation.ts` |

### 4.3 Spec 先行要求
动工前必须确认：**生产环境是否允许 Go 版做一次真实的实例创建**（真机验证是 DoD 第 4 条）。若不允许，则 Wave D 的 `instances` 只能做到「对拍通过 + 单测覆盖」，并在报告里显式标注「未真机验证」，不得声称完成。

> 默认按**不碰生产**执行：实例创建/销毁的真实 Incus 验证不做，只做双跑对拍 + 单测，报告必须写「未真机验证」。

### 4.4 `hosts` 施工切分（实测数据，2026-11-10）

`routes/hosts.ts` 共 **63 条路由**。按关注点分为 6 组，便于分批移植与审阅：

| 组 | 路由 | 行区间 | 说明 |
|---|---|---|---|
| A 安装与 CRUD | `GET /`、`POST /`、`GET /install.sh`、`GET /install.sh/:token`、`GET /cert/:token`、`POST /:id/verify`、`POST /:id/regenerate-install`、`POST /:id/test`、`GET /:id`、`GET|PUT /:id/images`、`PATCH /:id`、`DELETE /:id` | 679–1785, 2879 | 安装脚本/证书是**公开**端点，不做 JWT 鉴权但靠一次性 token |
| B 维护与批量 | `POST /:id/maintenance`、`POST /:id/sync`、`POST /:id/instances/batch-delete-preview`、`DELETE /:id/instances/batch` | 2280–2449 | 批量删除是资源释放路径，必须幂等 |
| C 存储池 | `/:id/storage-pools`（GET/POST/DELETE/PATCH） | 2930–3278 | 与 `lib/storage/*` 和 `instance-advanced-config` 共享语义 |
| D 公网 IPv4 + Caddy + agent 日志 | `/:id/public-ipv4/*`、`/:id/caddy/*`、`/:id/agent/logs/ticket`、`/:id/agent/logs/stream` | 3370–3897 | `agent/logs/stream` 是 **public**（一次性 ticket 授权）；Caddy 走 `lib/caddy_client.go` |
| E 容量与实例运维 | `/:id/instances/sync-status`、`/:id/recalculate-resources`、`/:id/instances/suspend|unsuspend`、`/:id/extend-all`、`/:id/plans`、`/:id/instances/migrate`、`/:id/users/lookup`、`/:id/instances/create-for-user`、`/:id/instances/gift-days`、`/:id/instances/:instanceId/renewal-price` | 3897–4676, 5303–6135 | `migrate`/`create-for-user` 是**资源交付**，走 §4.1 的占位-执行-确认 |
| F ops 与审计 | `/:id/ops/discover|optimize-port-mappings|baseline-sync|network-repair`、`/:id/ops/instances/*`、`/:id/ops/audit/*` | 6261–7847 | 20 条；`audit/scan`、`kill-process`、`dangerous-action` 属高危执行路径 |

`db/hosts.ts`（**1238 行、20 个导出函数**）是 22 个路由模块的共同依赖（`instances` 32 处、`ip-addresses` 10 处、`traffic` 8 处、`admin-billing` 7 处……），因此**必须先落地并稳定**，再动 `routes/hosts.go`。

已核实的关键事实：
- `db/hosts.ts` **不使用 advisory lock**。`selectAndReserveHostWithLock` 用的是 SQL 行锁 `FOR UPDATE SKIP LOCKED`（`lockQuery`，约 1057 行），选宿主机后在同一事务里预占资源。Go 侧必须保留同一段 SQL 语义（同一筛选条件、同一 SKIP LOCKED、同一事务边界），**不要替换成 advisory lock**——两者并发语义不同。
- `reservePlanUpgradeCapacityWithLock` 是另一条独立的锁路径（套餐升级容量预占），同样必须在事务内完成。
- `lib/incus/tunnel-manager.ts`（544 行）是隧道控制面，和 `lib/incus_client.go` 的拨号分支耦合；移植时先确认 `incus_client.go` 现有 seam（指纹固定传输层已在 `lib/node_security_transport.go`）。
- 公开端点（A 组的 install.sh/cert、D 组的 logs/stream）**没有 JWT**，对拍时不要塞 token；它们的安全性来自一次性 token/ticket，反向测试要覆盖「token 已用过 → 401/403」。

**交付顺序（不可颠倒）**：`db/hosts.go` → `lib/tunnel_manager.go` → `routes/hosts.go`（可分 6 组提交，但同一时间只允许一个 agent 写 `routes/hosts.go`）。

---

## 5. 支付类（`recharge` / `admin-billing`）

### 红线
- 任何金额计算必须可复算：给定相同输入必须得到相同结果（`lib/billing_calc.go` 已按此实现，含 `Number(x.toFixed(n))` 的精确复刻）。
- 支付回调必须验签，验签失败不得改单。
- 退款必须有上限（已付金额），且必须有幂等键。

### 必须的测试
- 同一回调重放两次 → 只入账一次
- 并发两笔退款 → 总额不超过已付
- `toFixed` 边界值（`1.005` / `2.675` / `-1.005`）逐值对拍（`lib/billing_calc_test.go` 已覆盖）

---

## 6. OTA（`system-update`）

### 红线
- **不碰生产**：`AGENTS.md` §2.1。任何 OTA 动作必须有 owner 明确指令。
- Go 版 `system-update` 只移植**只读查询 + 任务状态机**；实际执行 OTA 的脚本调用保留为 seam，默认显式失败。
- `RUN_DB_CHECKS=0` 旁路不得在 Go 版实现。

---

## 7. 验收清单（每个 Wave D 模块）

1. `gofmt -l` 空、`go build ./...`、`go vet ./...`、`go test ./...` 全绿
2. 该模块对拍全绿（同 DB、同请求，状态码 + JSON 逐字段）
3. 反向鉴权测试（§0.2）通过
4. 金额路径：并发 + 重放测试通过
5. 真机跑过受影响流程（或显式标注未验证及原因）
6. 报告：改了哪些文件 / 为什么 / 风险 / 怎么验证

---

## 8. 已知风险（Wave D 开工即接受）

| 风险 | 缓解 |
|---|---|
| 单实例不变量在 Go 版被误改 | §0.1 表格 + 代码注释 + 反向测试 |
| `Int[]` 列在 GORM 下不可解析 | 已修：`prismagen` 输出 `serializer:json`（提交 `36bdabe`），并有解析测试 |
| 单参数 advisory lock 与两参数锁空间不同 | §0.4 表格显式标注 74201 |
| Prisma 与 Go 的 `@updatedAt` 语义差异 | 每个 update 函数都有对应单测；空更新早返回逐处复刻 |
| 高危模块无真机验证 | §4.3 要求显式标注，不得声称完成 |
