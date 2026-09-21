# 统一优惠码处理引擎（Unified Promo Code Engine）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建高内聚、防套利的统一优惠码处理引擎，支持树形适用范围冒泡匹配、续费按月精细切片计费、退款/降级 0 退还风控、管理端全生命周期运维与站内信通知。

**Architecture:** 基于 PostgreSQL + Prisma 7 + Fastify 5 架构。采用单一事实来源抽象统一引擎 `PromoCodeEngine`，通过独立数据表（主表、树形范围表、实例绑定表、核销流水表）承载数据。计费核心接入切片拆分算法与退款保护守卫，结合 PG 事务与 Advisory Lock 防止并发超卖。

**Tech Stack:** Node.js, TypeScript, Fastify 5, Prisma 7, PostgreSQL 16, Vitest / Node Test Runner

## Global Constraints

- 私有后端 `127.0.0.1:3001` 必须且只能运行 1 个 Node.js 进程实例（严格维持单实例不变量）。
- 无 Redis，并发与排他保护使用 PostgreSQL Advisory Lock 与事务行级锁。
- 价格与折扣必须使用 `Math.max(0, ...)` 严格保底，金额运算保留两位小数。
- 遵循 Conventional Commits：`<type>(<scope>): <subject>`，提交前运行 `make check`。
- 严禁硬编码 hex 颜色；本计划仅涉及后端引擎、API 与测试。

---

## File Structure & Responsibilities

```
server/
├── prisma/
│   └── schema.prisma                           # 新增 PromoCode, PromoCodeScope, InstancePromoBinding, PromoRedemptionLog 模型
├── src/
│   ├── services/
│   │   ├── promo/
│   │   │   ├── scope-matcher.ts                # 树形适用范围冒泡匹配算法 (Root -> Package -> Plan)
│   │   │   ├── promo-calculator.ts             # 价格计算与按月切片计算 (Billing Slicing)
│   │   │   └── types.ts                        # 引擎专有类型定义
│   │   └── promo-engine.ts                     # 统一优惠码处理门面引擎 (PromoCodeEngine)
│   ├── db/
│   │   ├── promo-codes.ts                      # 优惠码主表、适用范围表与绑定表的原子 DB 操作
│   │   └── promo-redemptions.ts                # 核销明细日志写入与统计查询
│   └── routes/
│       ├── promo-codes.ts                      # 用户端 API (验码、续费切片预览、存量补绑)
│       └── admin-promo-codes.ts                # 管理端 API (发券、列表、启停、删除全量解绑、指定解绑)
├── scripts/
│   └── test-promo-engine-guards.ts             # 核心业务不变量与防套利守卫测试
└── test/
    ├── promo-scope-matcher.test.ts
    ├── promo-calculator.test.ts
    ├── promo-engine.test.ts
    └── promo-refund-guards.test.ts
```

---

### Task 1: Prisma Schema & 数据库模型定义

**Files:**
- Modify: `server/prisma/schema.prisma:2840-2880`
- Test: `server/test/promo-schema.test.ts`

**Interfaces:**
- Produces: Prisma Models (`PromoCode`, `PromoCodeScope`, `InstancePromoBinding`, `PromoRedemptionLog`) 和 Enums (`PromoCodeType`, `PromoDiscountType`, `PromoDurationType`).

- [ ] **Step 1: 编写 Schema 结构测试**

```typescript
// server/test/promo-schema.test.ts
import { describe, it, expect } from 'vitest'
import { prisma } from '../src/db/prisma.js'

describe('Promo Code Schema Enums & Models', () => {
  it('should have promo code models defined in Prisma Client', () => {
    expect(prisma.promoCode).toBeDefined()
    expect(prisma.promoCodeScope).toBeDefined()
    expect(prisma.instancePromoBinding).toBeDefined()
    expect(prisma.promoRedemptionLog).toBeDefined()
  })
})
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-schema.test.ts`  
Expected: FAIL with "Cannot read properties of undefined (reading 'promoCode')"

- [ ] **Step 3: 在 `schema.prisma` 中添加模型定义**

在 `server/prisma/schema.prisma` 添加：
* `enum PromoCodeType { AFF_USER ADMIN_PROMO }`
* `enum PromoDiscountType { PERCENTAGE FIXED_AMOUNT }`
* `enum PromoDurationType { ONCE REPEATING FOREVER }`
* `model PromoCode`（含 `isGlobal`, `scopes`, `bindings`, `redemptionLogs`）
* `model PromoCodeScope`（含 `packageId`, `packagePlanId`）
* `model InstancePromoBinding`（含 `instanceId @unique`, `remainingCycles`, `usedCycles`）
* `model PromoRedemptionLog`（含 `actionType`, `discountAmount`, `finalPrice`）

执行客户端生成与模型推送：
```bash
pnpm --filter server prisma generate
pnpm --filter server prisma db push
```

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-schema.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/prisma/schema.prisma server/test/promo-schema.test.ts
git commit -m "feat(promo): define promo code schema models and enums"
```

---

### Task 2: 树形适用范围冒泡匹配算法（Scope Matcher）

**Files:**
- Create: `server/src/services/promo/scope-matcher.ts`
- Create: `server/src/services/promo/types.ts`
- Test: `server/test/promo-scope-matcher.test.ts`

**Interfaces:**
- Produces: `isProductEligibleForPromo(promo, target): boolean`

- [ ] **Step 1: 编写冒泡匹配单元测试**

```typescript
// server/test/promo-scope-matcher.test.ts
import { describe, it, expect } from 'vitest'
import { isProductEligibleForPromo } from '../src/services/promo/scope-matcher.js'

describe('Hierarchical Scope Bubble-Up Matcher', () => {
  it('should match global root node regardless of product', () => {
    const promo = { isGlobal: true, scopes: [] }
    expect(isProductEligibleForPromo(promo, { packageId: 1, packagePlanId: 10 })).toBe(true)
  })

  it('should match exact plan leaf node', () => {
    const promo = { isGlobal: false, scopes: [{ packageId: 1, packagePlanId: 10 }] }
    expect(isProductEligibleForPromo(promo, { packageId: 1, packagePlanId: 10 })).toBe(true)
    expect(isProductEligibleForPromo(promo, { packageId: 1, packagePlanId: 11 })).toBe(false)
  })

  it('should match parent package node for all child plans', () => {
    const promo = { isGlobal: false, scopes: [{ packageId: 1, packagePlanId: null }] }
    expect(isProductEligibleForPromo(promo, { packageId: 1, packagePlanId: 10 })).toBe(true)
    expect(isProductEligibleForPromo(promo, { packageId: 1, packagePlanId: 20 })).toBe(true)
    expect(isProductEligibleForPromo(promo, { packageId: 2, packagePlanId: 30 })).toBe(false)
  })
})
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-scope-matcher.test.ts`  
Expected: FAIL with "Cannot find module ... scope-matcher.js"

- [ ] **Step 3: 编写 `scope-matcher.ts` 纯函数实现**

在 `server/src/services/promo/scope-matcher.ts` 实现冒泡判定：
1. `promo.isGlobal === true` 直接返回 `true`；
2. 命中 `packagePlanId === target.packagePlanId` 返回 `true`；
3. 命中 `packageId === target.packageId && packagePlanId === null` 返回 `true`；
4. 否则返回 `false`。

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-scope-matcher.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/services/promo/ server/test/promo-scope-matcher.test.ts
git commit -m "feat(promo): implement hierarchical bubble-up scope matcher"
```

---

### Task 3: 计费切片计算与防负数保底引擎（Calculator）

**Files:**
- Create: `server/src/services/promo/promo-calculator.ts`
- Test: `server/test/promo-calculator.test.ts`

**Interfaces:**
- Produces:
  - `calculateFinalPrice(originalPrice, discountAmount): number`
  - `calculateCreationQuote(params): CreationQuoteResult`
  - `calculatePromoRenewalSplit(params): RenewalSplitResult`

- [ ] **Step 1: 编写切片与保底单测**

```typescript
// server/test/promo-calculator.test.ts
import { describe, it, expect } from 'vitest'
import { calculateFinalPrice, calculatePromoRenewalSplit } from '../src/services/promo/promo-calculator.js'

describe('Promo Calculator & Billing Slicing', () => {
  it('should clamp negative amount discounts to zero', () => {
    expect(calculateFinalPrice(15, 20)).toBe(0)
    expect(calculateFinalPrice(50, 20)).toBe(30)
  })

  it('should correctly slice annual renewal with limited cycles (4 remaining out of 12 months)', () => {
    const result = calculatePromoRenewalSplit({
      monthlyPrice: 100,
      months: 12,
      discountRate: 0.10, // 9 折
      remainingCycles: 4
    })
    expect(result.discountedMonths).toBe(4)
    expect(result.regularMonths).toBe(8)
    expect(result.discountAmount).toBe(40)
    expect(result.finalAmount).toBe(1160)
    expect(result.consumedCycles).toBe(4)
    expect(result.newRemainingCycles).toBe(0)
    expect(result.willUnbind).toBe(true)
  })

  it('should not unbind forever discounts', () => {
    const result = calculatePromoRenewalSplit({
      monthlyPrice: 100,
      months: 12,
      discountRate: 0.10,
      remainingCycles: null
    })
    expect(result.discountedMonths).toBe(12)
    expect(result.discountAmount).toBe(120)
    expect(result.finalAmount).toBe(1080)
    expect(result.willUnbind).toBe(false)
  })
})
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-calculator.test.ts`  
Expected: FAIL

- [ ] **Step 3: 编写 `promo-calculator.ts` 实现**

实现金额保底函数与切片拆分函数，严格保留两位小数，返回 `willUnbind` 标记。

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-calculator.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/services/promo/promo-calculator.ts server/test/promo-calculator.test.ts
git commit -m "feat(promo): implement billing slicing and zero clamp calculator"
```

---

### Task 4: 数据库原子操作层（DB CRUD & Concurrency Locks）

**Files:**
- Create: `server/src/db/promo-codes.ts`
- Create: `server/src/db/promo-redemptions.ts`
- Test: `server/test/promo-db.test.ts`

**Interfaces:**
- Produces:
  - `incrementPromoUsageWithLock(tx, promoCodeId)`
  - `bindPromoToInstance(tx, params)`
  - `unbindPromoFromInstance(tx, instanceId)`
  - `recordRedemptionLog(tx, params)`

- [ ] **Step 1: 编写原子递增与绑定操作测试**

```typescript
// server/test/promo-db.test.ts
import { describe, it, expect } from 'vitest'
import { prisma } from '../src/db/prisma.js'
import { incrementPromoUsageWithLock } from '../src/db/promo-codes.js'

describe('Promo Code DB Atomic Operations', () => {
  it('should fail increment when quota is reached', async () => {
    const promo = await prisma.promoCode.create({
      data: {
        code: `TEST-${Date.now()}`,
        discountType: 'PERCENTAGE',
        discountValue: 0.1,
        maxTotalUses: 1,
        usedTotalCount: 1
      }
    })

    await expect(
      prisma.$transaction(tx => incrementPromoUsageWithLock(tx, promo.id))
    ).rejects.toThrow('PROMO_QUOTA_EXCEEDED')
  })
})
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-db.test.ts`  
Expected: FAIL

- [ ] **Step 3: 实现原子扣减、绑定与流水落库**

在 `server/src/db/promo-codes.ts` 中通过 PostgreSQL 条件更新实现原子扣增：
```typescript
const result = await tx.promoCode.updateMany({
  where: {
    id: promoCodeId,
    enabled: true,
    OR: [{ maxTotalUses: null }, { usedTotalCount: { lt: prisma.raw('max_total_uses') } }]
  },
  data: { usedTotalCount: { increment: 1 } }
})
if (result.count === 0) throw new Error('PROMO_QUOTA_EXCEEDED')
```

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-db.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/db/promo-codes.ts server/src/db/promo-redemptions.ts server/test/promo-db.test.ts
git commit -m "feat(promo): implement atomic db operations and concurrency protection"
```

---

### Task 5: 统一门面引擎（PromoCodeEngine）

**Files:**
- Create: `server/src/services/promo-engine.ts`
- Test: `server/test/promo-engine.test.ts`

**Interfaces:**
- Consumes: `scope-matcher.ts`, `promo-calculator.ts`, `db/promo-codes.ts`
- Produces:
  - `PromoCodeEngine.validate(params)`
  - `PromoCodeEngine.settleOnCreate(tx, params)`
  - `PromoCodeEngine.settleOnRenew(tx, params)`
  - `PromoCodeEngine.getPromoRefundEligibility(instanceId, tx)`

- [ ] **Step 1: 编写引擎编排单测**

```typescript
// server/test/promo-engine.test.ts
import { describe, it, expect } from 'vitest'
import { PromoCodeEngine } from '../src/services/promo-engine.js'

describe('PromoCodeEngine Orchestration', () => {
  it('should validate and refuse self-referred AFF codes', async () => {
    // 验证逻辑测试
  })
})
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-engine.test.ts`  
Expected: FAIL

- [ ] **Step 3: 编写 `PromoCodeEngine` 完整服务实现**

封装开购、续费核销切片与退款资损判别逻辑。

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-engine.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/services/promo-engine.ts server/test/promo-engine.test.ts
git commit -m "feat(promo): implement PromoCodeEngine orchestration service"
```

---

### Task 6: 计费核心退款与方案降级风控改造

**Files:**
- Modify: `server/src/db/billing-operations.ts:280-340,420-510`
- Test: `server/test/promo-refund-guards.test.ts`

**Interfaces:**
- Consumes: `PromoCodeEngine.getPromoRefundEligibility`
- Modifies: `calculateInstanceRemainingRefundQuote`, `calculatePlanChangeDetails`

- [ ] **Step 1: 编写退款风控守卫测试**

```typescript
// server/test/promo-refund-guards.test.ts
import { describe, it, expect } from 'vitest'
import { calculateInstanceRemainingRefundQuote } from '../src/db/billing-operations.js'

describe('Refund & Downgrade Promo Guards', () => {
  it('should return 0 refundable value if instance has active promo binding', async () => {
    // 绑定中实例断言 refundableValue === 0
  })

  it('should return 0 refundable value in first month for once-discount instance without renew', async () => {
    // 首月立减未续费实例断言 refundableValue === 0
  })
})
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-refund-guards.test.ts`  
Expected: FAIL

- [ ] **Step 3: 注入风控判定至 `billing-operations.ts`**

在退款计算中，拦截活跃绑定与首月未续费记录，强制置 0；方案降级时将负差价强制截断为 0。

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-refund-guards.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/db/billing-operations.ts server/test/promo-refund-guards.test.ts
git commit -m "feat(promo): enforce zero refund and zero downgrade arbitrage guards"
```

---

### Task 7: 用户端 API 路由与开机/续费集成

**Files:**
- Create: `server/src/routes/promo-codes.ts`
- Modify: `server/src/routes/instances.ts:1380-1420`
- Modify: `server/src/services/billing-scheduler.ts:170-200`
- Test: `server/test/promo-user-routes.test.ts`

- [ ] **Step 1: 编写用户端验码与切片预览路由测试**

```typescript
// server/test/promo-user-routes.test.ts
import { describe, it, expect } from 'vitest'
// 测试 POST /api/v1/promos/validate 与 GET /instances/:id/renew-preview
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-user-routes.test.ts`  
Expected: FAIL

- [ ] **Step 3: 实现用户端路由并在 Fastify 注册**

注册 `POST /validate`、`GET /instances/:id/renew-preview` 与 `POST /instances/:id/apply-promo`；在开机事务中调用 `settleOnCreate`；在定时续费调度器调用 `settleOnRenew`。

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-user-routes.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/routes/promo-codes.ts server/src/routes/instances.ts server/src/services/billing-scheduler.ts server/test/promo-user-routes.test.ts
git commit -m "feat(promo): add user promo routes and integrate creation and renewal flow"
```

---

### Task 8: 管理端 API 路由与站内信通知

**Files:**
- Create: `server/src/routes/admin-promo-codes.ts`
- Test: `server/test/promo-admin-routes.test.ts`

- [ ] **Step 1: 编写管理端 CRUD、解绑与发信测试**

```typescript
// server/test/promo-admin-routes.test.ts
import { describe, it, expect } from 'vitest'
// 测试创建券、查看绑定实例、解绑实例触发 inbox 消息
```

- [ ] **Step 2: 运行测试以确认失败**

Run: `pnpm --filter server exec vitest run test/promo-admin-routes.test.ts`  
Expected: FAIL

- [ ] **Step 3: 实现管理端路由与异步站内信分发**

实现发券（树形范围多选）、启停、删除（全量解绑发信）、指定解绑（单独发信）。使用 `createInboxMessage` 投递 `promo_binding_exhausted`、`promo_binding_unbound_by_admin`、`promo_code_deleted` 事件。

- [ ] **Step 4: 运行测试以确认通过**

Run: `pnpm --filter server exec vitest run test/promo-admin-routes.test.ts`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/src/routes/admin-promo-codes.ts server/test/promo-admin-routes.test.ts
git commit -m "feat(promo): implement admin promo management and inbox notifications"
```

---

### Task 9: 全套守卫测试与系统完整性校验

**Files:**
- Create: `server/scripts/test-promo-engine-guards.ts`
- Modify: `server/package.json`

- [ ] **Step 1: 编写全量业务不变量守卫脚本**

在 `server/scripts/test-promo-engine-guards.ts` 编写包含并发、保底、树形冒泡、切片核销、退款 0 保护的全链路自动化守卫。

- [ ] **Step 2: 在 `package.json` 注册并在本地跑通守卫**

```bash
pnpm --filter server test:promo-engine-guards
```
Expected: All guards PASS (100% assertions green).

- [ ] **Step 3: 运行全局类型检查与规范校验**

```bash
pnpm --filter server type-check
make check
```
Expected: PASS with 0 errors.

- [ ] **Step 4: Commit**

```bash
git add server/scripts/test-promo-engine-guards.ts server/package.json
git commit -m "test(promo): add promo engine guard test suite"
```

---

### Task 10: 功能审查（Feature Consistency & Spec Adherence Audit）

**Files:**
- Create: `docs/superpowers/audits/feature-consistency-review.md`
- Audit Targets:
  - `docs/superpowers/specs/2026-09-22-unified-promo-code-engine-design.md`
  - `server/src/services/promo/`
  - `server/src/db/promo-codes.ts`
  - `server/src/routes/promo-codes.ts`
  - `server/src/routes/admin-promo-codes.ts`

**Review Checklist:**
- [ ] **Step 1: 对照规格书第 2 章执行管理员配置全景审查**
  - 核查 `PromoCode` 表及创建 API 是否完全支持 6 大维度所有字段：`code`, `name`, `discountType`, `discountValue`, `isGlobal`, `scopes`, `durationType`, `durationCycles`, `maxTotalUses`, `maxUsesPerUser`, `startsAt`, `expiresAt`, `enabled`。
  - 核查金额立减是否严格联动锁定为首期一次性（`ONCE`）。
- [ ] **Step 2: 对照规格书第 3 章执行树形适用范围冒泡匹配审查**
  - 核查 Root（全场通用）是否在 `isGlobal=true` 时放行所有产品。
  - 核查 Level 1 套餐节点是否能覆盖名下所有子方案。
  - 核查 Level 2 方案叶子节点是否能精准匹配单个方案且不干扰同套餐其他方案。
  - 核查混合勾选（例如套餐 A + 方案 B-1）是否正常解析生效。
- [ ] **Step 3: 对照规格书第 5 章执行切片计费与周期衰减审查**
  - 核查长周期（按季/年付）续费是否严格按照 $M_{\text{discounted}} = \min(M, R)$ 与 $M_{\text{regular}} = M - M_{\text{discounted}}$ 拆解。
  - 核查期数归 0 时是否在事务内完成物理删除解绑，并异步触发站内信。
  - 核查 `FOREVER` 永久码是否无论续费多久均按比例打折且不解绑。
- [ ] **Step 4: 对照规格书第 8 章执行管理运维与站内信规范审查**
  - 核查 3 大站内信事件（`promo_binding_exhausted`、`promo_binding_unbound_by_admin`、`promo_code_deleted`）的事件名、标题与文案是否 100% 吻合规格书。
  - 核查管理员删除优惠码时，是否将名下所有绑定实例解除并批量发送通知。
  - 核查停用操作是否为静默生效，新购不可用且续费不打折。
- [ ] **Step 5: 输出功能审查报告并 Commit**
  - 生成审查报告 `docs/superpowers/audits/feature-consistency-review.md`，记录对照核验结果。
  ```bash
  git add docs/superpowers/audits/feature-consistency-review.md
  git commit -m "docs(promo): record feature consistency audit report"
  ```

---

### Task 11: 安全审查（Security & Anti-Arbitrage Vulnerability Audit）

**Files:**
- Create: `docs/superpowers/audits/security-vulnerability-review.md`
- Test: `server/test/promo-security-audit.test.ts`
- Audit Targets:
  - `server/src/db/billing-operations.ts`
  - `server/src/db/promo-codes.ts`
  - `server/src/services/promo-engine.ts`
  - `server/src/routes/promo-codes.ts`

**Review Checklist:**
- [ ] **Step 1: 并发超卖与条件竞争审查（Race Conditions）**
  - 编写并发自动化测试模拟 20 个并发请求抢购限量 1 张的优惠码。
  - 核查 PostgreSQL 原子更新语句 `used_total_count < max_total_uses` 与事务行级锁是否彻底杜绝超卖。
- [ ] **Step 2: 负数金额与保底穿透审查（Negative Price Exploit）**
  - 测试当固定立减金额（如 ¥50）大于套餐价格（如 ¥15）时，实付金额是否强制截断为 0，余额扣款逻辑是否严禁产生“反向加款”。
- [ ] **Step 3: 销毁退款套利漏洞审查（Refund Arbitrage）**
  - 审计 `calculateInstanceRemainingRefundQuote`：
    - 活跃绑定中的实例申请销毁退款，必须断言 `refundableValue === 0`。
    - 首购享受一次性优惠立减且未曾续费的实例在首月内申请退款，必须断言 `refundableValue === 0`。
    - 验证发生过续费进入次月后的实例是否恢复正常退款。
- [ ] **Step 4: 方案降级差价变现套利审查（Plan Downgrade Arbitrage）**
  - 审计 `calculatePlanChangeDetails`：
    - 享有优惠的实例选择降级至更便宜方案时，计算出的负差价必须强制截断为 0，严禁向用户钱包退回任何差价。
- [ ] **Step 5: 暴力枚举与接口刷券风控审查（Brute-Force & Enumeration）**
  - 核查 Fastify 路由配置是否包含 `@fastify/rate-limit` 频率限制。
  - 核查自动发码是否采用高熵随机算法 `nanoid(8).toUpperCase()`。
- [ ] **Step 6: TOCTOU 时序篡改与异步调度脏读审查**
  - 核查在 `POST /instances` 提交开机时，事务内部是否重新全量执行适用范围树匹配，杜绝前端篡改套餐 ID。
  - 核查后台异步续费调度器在续费瞬间是否检查 `enabled === true` 与有效期，杜绝停用码的脏读打折。
- [ ] **Step 7: 输出安全审查报告并 Commit**
  - 生成安全报告 `docs/superpowers/audits/security-vulnerability-review.md`。
  ```bash
  git add docs/superpowers/audits/security-vulnerability-review.md server/test/promo-security-audit.test.ts
  git commit -m "docs(promo): record security and anti-arbitrage audit report"
  ```

