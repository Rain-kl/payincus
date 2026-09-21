# 后端统一优惠码处理引擎（Unified Promo Code Engine）架构与设计规格书

> **文档状态**：已审定（Detailed Specification）  
> **文档版本**：v1.0.0  
> **创建日期**：2026-09-22  
> **适用范围**：PayIncus (Incudal) 计费核心、优惠引擎、防套利风控、管理后台与通知体系

---

## 目录
1. [背景与核心业务不变量](#1-背景与核心业务不变量)
2. [管理员优惠券配置全景与表单规则](#2-管理员优惠券配置全景与表单规则)
3. [树形适用范围与冒泡匹配（Bubble-Up Matching）](#3-树形适用范围与冒泡匹配bubble-up-matching)
4. [完整数据库数据模型（Prisma Schema DDL）](#4-完整数据库数据模型prisma-schema-ddl)
5. [计费切片算法与核销生命周期（Billing Slicing）](#5-计费切片算法与核销生命周期billing-slicing)
6. [安全防范与反套利风控开发细节](#6-安全防范与反套利风控开发细节)
7. [销毁退款与方案降级风控机制](#7-销毁退款与方案降级风控机制)
8. [管理功能、事务流程与站内信通知体系](#8-管理功能事务流程与站内信通知体系)
9. [前后端 API 接口契约与数据结构](#9-前后端-api-接口契约与数据结构)
10. [与现有系统核心模块的集成点](#10-与现有系统核心模块的集成点)
11. [自动化测试与守卫测试验收标准](#11-自动化测试与守卫测试验收标准)

---

## 1. 背景与核心业务不变量

PayIncus 平台历史设计中，仅存在挂载于推荐返利体系下的用户推广码（AFF Code，`aff_codes` 表）。为满足商业化运营、官方大促、精准定向补贴及周期折扣的需求，且杜绝并发超卖、刷单薅羊毛与退款套利等资损隐患，特构建全新的**统一优惠码处理引擎（`PromoCodeEngine`）**。

### 核心业务不变量（Invariants）
1. **单一事实来源（Single Source of Truth）**：所有优惠码（用户 AFF 推广码与官方管理员优惠码）统一由引擎抽象层处理。
2. **单实例单码约束**：任何一台实例在任意时刻，**最多且只能绑定 1 个生效中的优惠码**。
3. **金额立减首购一次性，比例折扣支持周期循环**：
   * **金额优惠码**：不常驻绑定，仅在首次支付/开机时立减结算，结算后实例保持未绑定态。
   * **比例折扣优惠码**：可以绑定实例，支持配置首月一次性（`ONCE`）、前 N 个月固定期数循环（`REPEATING`）或永久无限循环（`FOREVER`）。
4. **长周期续费按月精细切片（Billing Slicing）**：续费周期超过剩余优惠期数时，必须精细切分为“打折月”与“原价月”，防范期数放大套利。
5. **优惠期销毁与降级 0 退还**：享有有效优惠码绑定的实例，或首月享受过一次性折销的实例在首月内，销毁退款金额强制为 0，方案降级差价不予退还。

---

## 2. 管理员优惠券配置全景与表单规则

管理员在后台创建优惠码时，可配置 6 大维度的完整参数。

### 2.1 配置参数全景矩阵

| 模块 | 配置项 | 字段名 | 控件与类型 | 说明与约束 | 必填 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **基础信息** | **优惠码名称** | `name` | 文本输入 | 如「2026夏季闪购 85折」，用于台账统计与通知模板引用 | 选填 |
| | **优惠码代码** | `code` | 文本 + 自动生成 | 允许自定义英数（如 `SUMMER2026`）或一键生成 8 位大写高熵随机码（`nanoid(8)`） | **必填** |
| **优惠策略** | **折扣方式** | `discountType` | 单选 Button | 1. **比例折扣**（`PERCENTAGE`，默认）<br>2. **固定金额立减**（`FIXED_AMOUNT`） | **必填** |
| | **面额/折扣值** | `discountValue` | 数字步进器 | • 比例折扣：输入百分比（如 `15%`，对应存储 `0.15`，即 85 折）<br>• 金额立减：输入减免元数（如 `20.00`），严格保底不能为负 | **必填** |
| **适用范围** | **适用范围模式** | `isGlobal` | 单选 Radio | 1. **全场通用**（`isGlobal = true`）：所有产品通用<br>2. **自定义指定产品**（`isGlobal = false`） | **必填** |
| | **树形范围多选** | `scopes` | 树形复选框 Tree | 展开选择套餐（Package）或套餐下具体方案（Plan），支持多选与混合勾选 | 条件必填 |
| **周期控制** | **优惠周期模式** | `durationType` | 单选 Radio | 1. **仅首月/首期一次性**（`ONCE`）<br>2. **固定期数循环**（`REPEATING`）<br>3. **永久无限循环**（`FOREVER`） | **必填** |
| | **循环生效期数** | `durationCycles` | 整数输入框 | 当 `durationType = REPEATING` 时显示，设置享受优惠的周期总月数（如 `6`） | 条件必填 |
| **配额与有效期** | **总发行配额** | `maxTotalUses` | 数字 + 无限勾选 | • 限制该优惠码总共可被兑换/绑定的实例台数（如 100 台）<br>• 勾选不限制存储为 `null` | **必填** |
| | **单用户限用** | `maxUsesPerUser` | 整数输入 | 单个用户 ID 最多可使用的次数（默认 1 次，防止同一账号批量开机） | 选填 |
| | **生效时间** | `startsAt` | 日期时间选择器 | 优惠码开放使用的起始时间（留空代表立即生效） | 选填 |
| | **失效时间** | `expiresAt` | 日期时间选择器 | 优惠码截止兑换的时间（留空代表长期有效） | 选填 |
| **状态控制** | **启用开关** | `enabled` | 开关 Switch | 默认开启。停用后不可新核销绑定，绑定的存量实例续费时暂不打折 | **必填** |

### 2.2 表单联动与互斥约束
1. **金额减免联动锁定**：
   * 一旦管理员选中 `discountType = FIXED_AMOUNT`，前端表单自动将 `durationType` 强制锁定为 `ONCE` 并禁用切换（金额优惠码不支持循环，首购结算后不入绑定表）。
2. **树形范围联动展开**：
   * 选择“全场通用”时收起产品树；选择“自定义指定产品”时，从后端拉取完整的产品树形结构。

---

## 3. 树形适用范围与冒泡匹配（Bubble-Up Matching）

适用范围在数据结构上抽象为 3 层的继承树：

```
[Level 0 - 根节点]: 全场通用 (Global Root, isGlobal = true)
   │
   ├── [Level 1 - 套餐]: 香港 CN2 高防云 (Package #1)
   │      ├── [Level 2 - 方案]: 1C1G 500G (Plan #101)  <-- 叶子节点
   │      └── [Level 2 - 方案]: 2C2G 1000G (Plan #102) <-- 叶子节点
   │
   └── [Level 1 - 套餐]: 日本 BGP 弹性云 (Package #2)
          ├── [Level 2 - 方案]: 1C2G 500G (Plan #201)  <-- 叶子节点
          └── [Level 2 - 方案]: 4C4G 2000G (Plan #202)  <-- 叶子节点
```

### 3.1 管理员勾选模式
* **勾选套餐节点（Package #1）**：表示该套餐名下的所有现在及未来添加的方案均适用。
* **勾选方案节点（Plan #201）**：仅该具体方案适用，Package #2 下的其他方案不适用。
* **混合勾选**：允许同时勾选 Package #1 + Plan #201。

### 3.2 自底向上冒泡匹配算法（Bubble-Up Matching）
当核销具体的目标购买项（叶子节点 `target: { packageId, packagePlanId }`）时，核销引擎自底向上递归向上冒泡：

```typescript
export function isProductEligibleForPromo(
  promo: {
    isGlobal: boolean
    scopes: Array<{ packageId: number | null; packagePlanId: number | null }>
  },
  target: { packageId: number; packagePlanId: number }
): boolean {
  // 1. 检查根节点（Level 0）：全场通用直接命中
  if (promo.isGlobal) {
    return true
  }

  // 2. 检查叶子节点（Level 2 - Plan）：是否显式勾选了该具体方案
  const matchLeafPlan = promo.scopes.some(
    scope => scope.packagePlanId === target.packagePlanId
  )
  if (matchLeafPlan) {
    return true
  }

  // 3. 向上冒泡检查父节点（Level 1 - Package）：是否勾选了该方案所属的整个套餐
  const matchParentPackage = promo.scopes.some(
    scope => scope.packageId === target.packageId && scope.packagePlanId === null
  )
  if (matchParentPackage) {
    return true
  }

  // 4. 沿树向上未命中任何祖先节点，判定为不适用
  return false
}
```

---

## 4. 完整数据库数据模型（Prisma Schema DDL）

在 `server/prisma/schema.prisma` 中实现统一模型定义：

```prisma
// ==================== 统一优惠码系统 ====================

enum PromoCodeType {
  AFF_USER         // 用户生成的推广返利码
  ADMIN_PROMO      // 管理员官方发行的优惠码
}

enum PromoDiscountType {
  PERCENTAGE       // 比例折扣
  FIXED_AMOUNT     // 固定金额立减
}

enum PromoDurationType {
  ONCE             // 仅首期有效（不常驻绑定）
  REPEATING        // 固定期数循环
  FOREVER          // 永久无限循环
}

// 1. 统一优惠码主表
model PromoCode {
  id                  Int               @id @default(autoincrement())
  code                String            @unique @db.VarChar(64)   // 优惠码（如 AFF-1-ABCDEF 或 SUMMER2026）
  name                String?           @db.VarChar(128)          // 内部备注名称
  type                PromoCodeType     @default(ADMIN_PROMO)     // 来源分类
  
  userId              Int?              @map("user_id")           // 若为 AFF 码，记录推广人 ID
  adminId             Int?              @map("admin_id")          // 管理员操作人 ID
  
  isGlobal            Boolean           @default(false) @map("is_global") // 是否全场通用
  
  discountType        PromoDiscountType @default(PERCENTAGE) @map("discount_type")
  discountValue       Decimal           @db.Decimal(10, 2) @map("discount_value") // 比例（0.10）或金额（20.00）
  commissionRate      Decimal           @default(0) @db.Decimal(5, 4) @map("commission_rate") // AFF 返利率
  
  durationType        PromoDurationType @default(FOREVER) @map("duration_type")
  durationCycles      Int?              @map("duration_cycles")   // REPEATING 时的总期数（如 6）
  
  maxTotalUses        Int?              @map("max_total_uses")    // 全局发行总量（null 为无限制）
  usedTotalCount      Int               @default(0) @map("used_total_count") // 全局已使用/已绑定总数
  maxUsesPerUser      Int?              @default(1) @map("max_uses_per_user") // 单用户限用次数
  
  startsAt            DateTime?         @map("starts_at")         // 生效时间
  expiresAt           DateTime?         @map("expires_at")        // 过期时间
  enabled             Boolean           @default(true)            // 启用开关
  
  totalDiscountAmount Decimal           @default(0) @db.Decimal(10, 2) @map("total_discount_amount")
  totalEarnings       Decimal           @default(0) @db.Decimal(10, 2) @map("total_earnings")
  
  createdAt           DateTime          @default(now()) @map("created_at")
  updatedAt           DateTime          @updatedAt @map("updated_at")

  user                User?             @relation(fields: [userId], references: [id], onDelete: Cascade)
  scopes              PromoCodeScope[]
  bindings            InstancePromoBinding[]
  redemptionLogs      PromoRedemptionLog[]

  @@index([type, enabled])
  @@index([userId])
  @@map("promo_codes")
}

// 2. 适用产品范围层级表
model PromoCodeScope {
  id              Int               @id @default(autoincrement())
  promoCodeId     Int               @map("promo_code_id")
  packageId       Int?              @map("package_id")
  packagePlanId   Int?              @map("package_plan_id")
  
  promoCode       PromoCode         @relation(fields: [promoCodeId], references: [id], onDelete: Cascade)
  package         Package?          @relation(fields: [packageId], references: [id], onDelete: Cascade)
  packagePlan     PackagePlan?      @relation(fields: [packagePlanId], references: [id], onDelete: Cascade)

  @@index([promoCodeId])
  @@index([packageId])
  @@index([packagePlanId])
  @@map("promo_code_scopes")
}

// 3. 实例优惠码实时绑定表
model InstancePromoBinding {
  id              Int               @id @default(autoincrement())
  instanceId      Int               @unique @map("instance_id") // 强约束：单实例全局仅能绑定 1 个优惠码
  promoCodeId     Int               @map("promo_code_id")
  
  durationType    PromoDurationType @map("duration_type")
  totalCycles     Int?              @map("total_cycles")        // 初始总期数（如 6）
  usedCycles      Int               @default(0) @map("used_cycles") // 已核销期数（开通首期算第 1 期）
  remainingCycles Int?              @map("remaining_cycles")    // 剩余有效期待核销期数（到 0 触发解绑）
  
  boundAt         DateTime          @default(now()) @map("bound_at")
  lastRedeemedAt  DateTime          @default(now()) @map("last_redeemed_at")

  instance        Instance          @relation(fields: [instanceId], references: [id], onDelete: Cascade)
  promoCode       PromoCode         @relation(fields: [promoCodeId], references: [id], onDelete: Cascade)

  @@index([promoCodeId])
  @@map("instance_promo_bindings")
}

// 4. 优惠码核销明细流水表（财务与审计底层记录）
model PromoRedemptionLog {
  id               Int               @id @default(autoincrement())
  promoCodeId      Int               @map("promo_code_id")
  instanceId       Int               @map("instance_id")
  userId           Int               @map("user_id")
  
  actionType       String            @map("action_type") @db.VarChar(32) // 'create' | 'renew'
  cycleIndex       Int               @map("cycle_index")         // 处于第几个优惠周期
  months           Int               @default(1)                 // 本次核销折算时长
  
  originalPrice    Decimal           @db.Decimal(10, 2) @map("original_price")
  discountAmount   Decimal           @db.Decimal(10, 2) @map("discount_amount")
  finalPrice       Decimal           @db.Decimal(10, 2) @map("final_price")
  commissionAmount Decimal           @default(0) @db.Decimal(10, 2) @map("commission_amount")
  
  createdAt        DateTime          @default(now()) @map("created_at")

  promoCode        PromoCode         @relation(fields: [promoCodeId], references: [id], onDelete: Cascade)
  instance         Instance          @relation(fields: [instanceId], references: [id], onDelete: Cascade)
  user             User              @relation(fields: [userId], references: [id], onDelete: Cascade)

  @@index([promoCodeId])
  @@index([instanceId])
  @@index([userId])
  @@map("promo_redemption_logs")
}
```

---

## 5. 计费切片算法与核销生命周期（Billing Slicing）

当用户在实例续费时选择长周期（如季付 3 个月、年付 12 个月），若实例绑定的优惠码有期数限制，必须执行按月精细切片计算。

### 5.1 数学公式与切片逻辑
设实例月单价为 $P_{\text{monthly}}$，用户本次续费月数为 $M$，当前剩余可用优惠期数为 $R$（`remainingCycles`），折扣比例为 $D$（如 $0.10$ 代表 9 折）：

1. **若优惠码为无限循环（`FOREVER`）**：
   * $M_{\text{discounted}} = M$
   * $\text{discountAmount} = \text{round}(P_{\text{monthly}} \times M \times D, 2)$
   * $\text{finalAmount} = (P_{\text{monthly}} \times M) - \text{discountAmount}$
   * 剩余期数不变，不解绑。

2. **若优惠码为固定期数循环（`REPEATING`）**：
   * **享受打折月数**：$M_{\text{discounted}} = \min(M, R)$
   * **原价计费月数**：$M_{\text{regular}} = M - M_{\text{discounted}}$
   * **总折扣金额**：
     $$\text{discountAmount} = \text{round}((P_{\text{monthly}} \times M_{\text{discounted}}) \times D, 2)$$
   * **最终实付总额**：
     $$\text{finalAmount} = (P_{\text{monthly}} \times M) - \text{discountAmount}$$
   * **期数扣减与解绑状态**：
     $$R_{\text{new}} = R - M_{\text{discounted}}$$
     * 若 $R_{\text{new}} \le 0$：更新扣减后，**在本次续费事务内直接物理删除 `instance_promo_bindings`**，触发自动解绑并发送站内信通知。

### 5.2 场景推演实例（单价 ¥100/月，9折码）
* **场景 A：剩余 4 个月，用户续费 12 个月（年付）**
  * 打折月数：$\min(12, 4) = 4$ 个月，折后价 $4 \times 100 \times 0.9 = 360$ 元；
  * 原价月数：$12 - 4 = 8$ 个月，原价 $8 \times 100 = 800$ 元；
  * 实付总价：$360 + 800 = 1160$ 元（原价 1200 元，减免 40 元）；
  * 优惠期数：4 个月全部耗尽，**完成续费瞬间自动解绑**，实例延期 1 年，1 年后到期续费时为标准原价。
* **场景 B：剩余 6 个月，用户续费 3 个月（季付）**
  * 打折月数：$\min(3, 6) = 3$ 个月全部 9 折，实付 270 元（减免 30 元）；
  * 优惠期数：扣减 3 个月，更新 `remainingCycles = 3`，保持绑定。

---

## 6. 安全防范与反套利风控开发细节

针对计费和优惠券系统的 6 大典型资损隐患，系统在底层构建严格的防御机制：

### 6.1 并发竞态与超额核销防范
* **问题**：恶意用户使用并发脚本在同 1 毫秒发起多笔请求，绕过总配额或单人限用次数。
* **实现方案**：
  1. 使用 PostgreSQL 条件原子更新扣减发行量：
     ```sql
     UPDATE promo_codes 
     SET used_total_count = used_total_count + 1 
     WHERE id = $1 AND enabled = true 
       AND (max_total_uses IS NULL OR used_total_count < max_total_uses);
     ```
     若影响行数为 0，则说明已被抢光，立即中断并报错。
  2. 使用 PG Advisory Lock 防止单用户并发开机：
     ```typescript
     await advisoryTransactionLock(tx, PROMO_USER_LOCK_NAMESPACE, userId)
     ```

### 6.2 金额防负数穿透
* **问题**：立减金额高于方案总价导致负账单，若系统将负数扣款处理为加余额，将导致资金被盗。
* **实现方案**：
  ```typescript
  export function calculateFinalPrice(originalPrice: number, discountAmount: number): number {
    return Math.max(0, Number((originalPrice - discountAmount).toFixed(2)))
  }
  ```

### 6.3 暴力枚举与接口刷券
* **实现方案**：
  * 券码必须采用 `nanoid(8).toUpperCase()` 高熵算法，组合空间 $> 2 \times 10^{12}$。
  * Fastify 验证接口施加 `@fastify/rate-limit`：单 IP/单用户限制 1 分钟最多 15 次。
  * 连续输错 5 次触发 15 分钟临时风控封锁。

### 6.4 时序篡改（TOCTOU）防范
* **问题**：前端先对便宜方案 A 验证通过，购买接口提交时偷换成昂贵方案 B。
* **实现方案**：在真正执行 `POST /instances` 的 DB 事务内，**重新拉取优惠码实表数据并再次执行全套 `isProductEligibleForPromo` 树形校验**，绝对不依赖前端上传的折扣计算结果。

### 6.5 停用/删除与定时续费 Worker 事务一致性
* **问题**：管理员停用优惠码后，后台异步批量续费 Worker 仍在按缓存打折。
* **实现方案**：
  续费 Worker 在续费每个实例时，独立开启事务并加行级排他锁或实时查询 `InstancePromoBinding -> PromoCode`：
  若 `promoCode.enabled === false` 或当前时间超期，**立即取消打折，按标准原价扣款**。

---

## 7. 销毁退款与方案降级风控机制

为从源头杜绝“低价打折开机、申请退款套现”的羊毛套利，制定强制退款与降级风控规则：

### 7.1 判定状态机
```
实例触发「销毁退款」或「方案降级」
   │
   ├── 1. 检查是否存在活跃绑定 (InstancePromoBinding)
   │      └── 存在活跃绑定 (REPEATING / FOREVER)
   │             └── 判定：❌ 不予退还剩余价值 (Refund = ¥0.00，降级差价不退)
   │
   └── 2. 若无活跃绑定，检查是否曾享受「首月/首购优惠」
          ├── 查询该实例的首购核销记录 (PromoRedemptionLog, actionType='create')
          │
          ├── 若首购无任何优惠：
          │      └── 判定：✅ 正常实例，按标准天数比例退还剩余价值
          │
          └── 若首购享受过优惠 (金额立减 或 首月一次性比例券)：
                 ├── 检查当前是否处于首购周期（该实例未曾发生过续费）
                 │      └── 判定：❌ 处于首月周期内，不退还剩余价值 (Refund = ¥0.00)
                 │
                 └── 检查当前是否已进入次月/后续续费周期（已有续费记录）
                        └── 判定：✅ 已进入全额续费阶段，按续费周期的实付标准正常退还剩余价值
```

### 7.2 降级方案（Plan Downgrade）的处理
在 `calculatePlanChangeDetails` 中：
* 升级方案（需补差价）：正常补缴差额；
* 降级方案（原日租金 > 新日租金）：若处于上述优惠保护期内，计算出的负差价强制截断为 0（`priceDiff = 0`），**不向用户钱包退回任何金额**。

---

## 8. 管理功能、事务流程与站内信通知体系

管理员在后台可对优惠码及绑定实例执行全生命周期管理，涉及 3 类站内信通知（调用 `createInboxMessage`）。

### 8.1 操作流程与事务设计

| 管理操作 | 数据库事务处理流程 | 站内信事件 |
| :--- | :--- | :--- |
| **自然期满解绑** | 续费事务中检测到 `remainingCycles == 0`，物理删除 `InstancePromoBinding` 记录 | `promo_binding_exhausted` |
| **管理员手动解绑指定实例** | 开启事务 $\rightarrow$ 校验实例绑定 $\rightarrow$ 删除对应 `InstancePromoBinding` 行 $\rightarrow$ 写操作日志 $\rightarrow$ 提交事务 $\rightarrow$ 异步发信 | `promo_binding_unbound_by_admin` |
| **管理员删除优惠码** | 开启事务 $\rightarrow$ 查询该券所有绑定的实例及归属用户 $\rightarrow$ 批量删除绑定 $\rightarrow$ 删除 scopes 与主表 $\rightarrow$ 提交事务 $\rightarrow$ 异步批量发信 | `promo_code_deleted` |
| **管理员停用优惠码** | 将 `promo_codes.enabled` 置为 `false`。新开机被拦截；存量续费自动按原价扣费 | 静默处理，不打扰用户 |

### 8.2 站内信文案规范模板

#### (1) 自然期满耗尽自动解绑
* **事件名**：`promo_binding_exhausted`
* **标题**：`【优惠期满】您的实例优惠折扣已结束`
* **内容**：
  > 尊敬的用户：  
  > 您的实例 **{instanceName}**（ID: #{instanceId}）所绑定的优惠码 **{promoCode}** 设定的优惠期数（共 {totalCycles} 期）已全部使用完毕。  
  > 从下一个计费周期开始，该实例将恢复为标准资费续费。您也可以在实例管理页面随时绑定新的有效优惠码。

#### (2) 管理员手动解绑指定实例
* **事件名**：`promo_binding_unbound_by_admin`
* **标题**：`【优惠变更】您的实例已解除优惠码绑定`
* **内容**：
  > 尊敬的用户：  
  > 您的实例 **{instanceName}**（ID: #{instanceId}）绑定的优惠码 **{promoCode}** 已由系统管理员解除绑定。  
  > 即日起该实例将不再享受此优惠码的续费折扣，后续续费将按标准资费计费。如有疑问，请通过工单系统联系客服支持。

#### (3) 管理员删除优惠码（全量解绑下线）
* **事件名**：`promo_code_deleted`
* **标题**：`【优惠调整】您使用的优惠码已下线终止`
* **内容**：
  > 尊敬的用户：  
  > 您在实例 **{instanceName}**（ID: #{instanceId}）上所享受的优惠码 **{promoCode}** 已由平台官方正式下线并终止优惠策略。  
  > 该实例后续续费将恢复为标准官方价格。感谢您的理解与支持。

---

## 9. 前后端 API 接口契约与数据结构

### 9.1 用户端接口

#### 1. 验证优惠码
* **URL**: `POST /api/v1/promos/validate`
* **Request**:
  ```json
  {
    "code": "SUMMER2026",
    "packageId": 10,
    "packagePlanId": 101
  }
  ```
* **Response (Success)**:
  ```json
  {
    "valid": true,
    "code": "SUMMER2026",
    "discountType": "PERCENTAGE",
    "discountValue": 0.15,
    "durationType": "REPEATING",
    "durationCycles": 6,
    "estimatedDiscount": 15.00,
    "description": "享受前 6 个月 85 折优惠"
  }
  ```

#### 2. 获取续费切片预览
* **URL**: `GET /api/v1/instances/:id/renew-preview`
* **Response**:
  ```json
  {
    "options": [
      {
        "months": 1,
        "originalPrice": 100.00,
        "discountAmount": 10.00,
        "finalPrice": 90.00,
        "promoDetail": "优惠码生效中：剩余 4 个月"
      },
      {
        "months": 12,
        "originalPrice": 1200.00,
        "discountAmount": 40.00,
        "finalPrice": 1160.00,
        "promoDetail": "前 4 个月享受 9 折，后 8 个月按原价计费（本次续费将自动耗尽优惠期数并解绑）"
      }
    ]
  }
  ```

#### 3. 存量实例补绑优惠码
* **URL**: `POST /api/v1/instances/:id/apply-promo`
* **Request**: `{ "code": "VIP90" }`
* **Response**: `{ "success": true, "message": "优惠码绑定成功，将于下次续费生效" }`

---

### 9.2 管理端接口

#### 1. 创建优惠码
* **URL**: `POST /api/v1/admin/promos`
* **Request**:
  ```json
  {
    "name": "夏季全场大促",
    "code": "SUMMER2026",
    "discountType": "PERCENTAGE",
    "discountValue": 0.10,
    "durationType": "REPEATING",
    "durationCycles": 6,
    "isGlobal": false,
    "scopes": [
      { "packageId": 1 },
      { "packageId": 2, "packagePlanId": 201 }
    ],
    "maxTotalUses": 100,
    "maxUsesPerUser": 1,
    "startsAt": null,
    "expiresAt": "2026-09-30T23:59:59Z",
    "enabled": true
  }
  ```

#### 2. 优惠码列表与统计
* **URL**: `GET /api/v1/admin/promos?page=1&pageSize=20`
* **Response**: 包含码详情、已用次数、累计减免总金额、当前绑定的活跃实例数。

#### 3. 停用 / 启用优惠码
* **URL**: `PATCH /api/v1/admin/promos/:id/toggle`
* **Request**: `{ "enabled": false }`

#### 4. 查看优惠码绑定的实例列表
* **URL**: `GET /api/v1/admin/promos/:id/instances?page=1&pageSize=20`
* **Response**: 展示 `instanceId`, `instanceName`, `username`, `totalCycles`, `usedCycles`, `remainingCycles`, `boundAt`。

#### 5. 解绑指定实例
* **URL**: `POST /api/v1/admin/promos/:id/unbind/:instanceId`
* **Response**: `{ "success": true, "message": "实例已解绑，已发送站内信通知" }`

#### 6. 删除优惠码
* **URL**: `DELETE /api/v1/admin/promos/:id`
* **Response**: `{ "success": true, "affectedInstances": 12 }`

---

## 10. 与现有系统核心模块的集成点

1. **`server/src/routes/instances.ts`**：
   * 在开通实例创建订单处，用 `PromoCodeEngine.validate` 替换原有的单独 AFF 校验；
   * 在扣费创建事务内，调用 `PromoCodeEngine.settleOnCreate` 完成核销与绑定。
2. **`server/src/db/billing-operations.ts`**：
   * 续费计算逻辑升级为调用 `calculatePromoRenewalSplit`；
   * 退款计算 `calculateInstanceRemainingRefundQuote` 接入 `getPromoRefundEligibility`，对绑码实例和首月未续费实例截断为 0 退款；
   * 方案变更 `calculatePlanChangeDetails` 接入降级差价清零保护。
3. **`server/src/services/billing-scheduler.ts`**：
   * 自动续费循环中，查询实表的 `InstancePromoBinding`，执行切片续费与到期自动解绑。
4. **`server/src/services/vip-benefits.ts`**：
   * 价格仲裁时，VIP 优惠与统一优惠码进行最优价比较仲裁（不叠加，取单项最低折后价）。

---

## 11. 自动化测试与守卫测试验收标准

1. **树形适用范围冒泡测试**：
   * 验证全场通用命中所有产品；
   * 验证命中套餐节点时，其下所有 Plan 均可用；
   * 验证仅勾选具体 Plan 时，同套餐下的其他 Plan 校验拒绝。
2. **长周期切片计费测试**：
   * 实例剩余 4 个月优惠期，年付 12 个月，断言前 4 个月享受折扣、后 8 个月按原价计费，续费后绑定关系被自动物理删除。
3. **退款风控测试**：
   * 绑定优惠码期间调用退款接口，断言 `refundableValue === 0`；
   * 首月享受立减或一次性打折期间调用退款，断言退款为 0；次月发生过续费后调用，断言按续费实付价正常退款。
4. **并发抢券守卫测试**：
   * 限制发行 1 张的优惠码，并发发起 10 个开机请求，断言仅且仅有 1 个请求成功，其余 9 个被原子阻断。
