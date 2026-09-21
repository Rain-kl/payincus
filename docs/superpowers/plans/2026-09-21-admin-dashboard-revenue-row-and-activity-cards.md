# 管理员大盘营收趋势通栏与营销/开机记录卡片实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将管理员大盘（`AdminDashboardView.vue`）的「营收与消费趋势」调整为独占一行的通栏卡片，并在下方双列平衡网格中新增【营销（进行中的活动）】卡片与【最近开机记录（系统实例创建情况）】卡片。

**Architecture:** 
1. 服务端在 `GET /api/admin/statistics/overview` 响应中并行聚合营销数据（活跃抽奖活动、签到配置与今日签到量）和最新创建实例流（最近 5 台实例基本信息），保持大盘数据单次请求原子加载与 30s 自动无闪烁刷新；
2. 客户端重构 `AdminDashboardView.vue` 结构，将图表提升为通栏展示，并在下方双列网格左列放置订单漏斗与营销卡片，右列放置最近开机、实例构成与平台运行事实卡片；
3. 严格遵循苹果黑白单色风格、绿/琥珀/红三色状态指示、零硬编码 hex 与零透明度规范，补齐中英繁三语。

**Tech Stack:** Fastify 5, Prisma 7, PostgreSQL, Vue 3, Vite, Tailwind CSS (Tokenized), vue-i18n.

## Global Constraints
- 纯黑白单色设计（浅色白底黑字，深色黑底白字），无二次元，状态色仅保留绿、琥珀、红；
- 严禁硬编码 hex 颜色（由 `test:frontend-color-tokens` 守卫强制拦截）；
- 严禁透明/半透明（无 `rgba()`、无 `/50`、无 `bg-transparent` 等）；
- 严禁触碰或还原工作区正在施工的文件；
- 严禁 `git push` 到远程仓库；
- 提交前必须通过 `make check`。

---

### Task 1: 后端数据契约扩展与测试守卫
**Files:**
- Modify: `server/src/routes/admin-statistics.ts:160-220,460-537`
- Create: `server/scripts/test-admin-dashboard-overview-guards.ts`
- Modify: `server/package.json`

**Interfaces:**
- Consumes: `prisma.lottery`, `prisma.dailyCheckin`, `prisma.systemConfig`, `prisma.instance`
- Produces: `GET /api/admin/statistics/overview` 扩充 `marketing` 与 `recentInstances` 字段：
  ```ts
  marketing: {
    activeLotteries: Array<{
      id: number
      name: string
      costPoints: number
      totalDraws: number
      startAt: string | null
      endAt: string | null
    }>
    totalActiveLotteries: number
    checkin: {
      enabled: boolean
      minPoints: number
      maxPoints: number
      requireInstance: boolean
      todayCheckins: number
    }
  }
  recentInstances: Array<{
    id: number
    name: string
    status: string
    createdAt: string
    user: { id: number; username: string; email: string }
    host: { id: number; name: string; countryCode: string | null }
    packagePlan: { name: string; cpu: number; memory: number; disk: number } | null
  }>
  ```

- [ ] **Step 1: 编写守卫测试 `server/scripts/test-admin-dashboard-overview-guards.ts`**
  断言 `server/src/routes/admin-statistics.ts` 源码中包含 `activeLotteries`、`todayCheckins`、`recentInstances` 以及对应的返回字段映射。
- [ ] **Step 2: 运行守卫测试验证失败**
  `pnpm --filter server test:admin-dashboard-overview-guards`（应失败，提示尚未包含相应字段）。
- [ ] **Step 3: 修改 `server/src/routes/admin-statistics.ts` 实现并行聚合**
  在 `Promise.all` 中增加活跃抽奖活动、有效抽奖总数、今日签到数、签到系统配置、最新 5 台开机实例的查询与返回映射。
- [ ] **Step 4: 重新运行守卫测试验证通过**
  `pnpm --filter server test:admin-dashboard-overview-guards`（PASS）。
- [ ] **Step 5: 验证服务端类型检查**
  `pnpm --filter server type-check`。
- [ ] **Step 6: Git 提交 Task 1**
  `git add server/src/routes/admin-statistics.ts server/scripts/test-admin-dashboard-overview-guards.ts server/package.json`
  `git commit -m "feat(statistics): aggregate marketing and recent instances in admin overview"`

---

### Task 2: 前端三语多语言字典补齐
**Files:**
- Modify: `client/src/locales/zh-CN.ts`
- Modify: `client/src/locales/en.ts`
- Modify: `client/src/locales/zh-TW.ts`

**Interfaces:**
- 补充 `admin.dashboard` 下营销与最近开机相关键名：
  - `marketingTitle`
  - `marketingDescription`
  - `activeCampaignsBadge`
  - `dailyCheckin`
  - `checkinEnabled`
  - `checkinDisabled`
  - `checkinRule`
  - `todayCheckinsCount`
  - `costPointsPerDraw`
  - `drawsCount`
  - `longTermActive`
  - `noActiveLotteries`
  - `manageMarketing`
  - `recentInstancesTitle`
  - `recentInstancesDescription`
  - `newInstancesTodayBadge`
  - `noRecentInstances`
  - `viewAllInstances`

- [ ] **Step 1: 在 `client/src/locales/zh-CN.ts`、`en.ts`、`zh-TW.ts` 添加词条**
- [ ] **Step 2: 运行 i18n 守卫验证**
  `pnpm --filter server test:frontend-i18n-keys`
- [ ] **Step 3: Git 提交 Task 2**
  `git add client/src/locales/zh-CN.ts client/src/locales/en.ts client/src/locales/zh-TW.ts`
  `git commit -m "feat(i18n): add marketing and recent provisioning keys for admin dashboard"`

---

### Task 3: 前端大盘视图重构与卡片装配
**Files:**
- Modify: `client/src/views/admin/AdminDashboardView.vue`

**Interfaces:**
- 消费：`stats.marketing`、`stats.recentInstances`、新增 i18n 键名
- 产出：
  - 顶层通栏卡片：营收与消费趋势（`col-span-full` 或独立全宽 section）
  - 双列平衡网格：
    - 左列：今日订单履约漏斗卡片 + 营销卡片
    - 右列：最近开机记录卡片 + 实例构成与到期分布卡片 + 平台运行事实卡片
  - 辅助方法：`formatRelativeTime(dateStr)`、`getInstanceStatusColor(status)`

- [ ] **Step 1: 在 `AdminDashboardView.vue` 补充营销与最近实例的类型与计算属性**
  - 定义 `activeLotteries`、`checkinInfo`、`recentInstances`；
  - 编写简洁的相对时间格式化工具函数 `formatRelativeTime`。
- [ ] **Step 2: 调整布局结构将「营收与消费走势」移为通栏卡片**
  - 独立成完整通栏 `w-full` 卡片，使柱状图在宽屏下有完整视野。
- [ ] **Step 3: 编写【营销（进行中的活动）】卡片组件模板**
  - 签到状态条 + 抽奖活动列表 + 空状态 + 跳转按钮。
  - 严格遵守颜色守卫（无 `#hex`，无 `/50`，纯语义 token）。
- [ ] **Step 4: 编写【最近开机记录】卡片组件模板**
  - 紧凑列表（状态指示圆点、实例名、规格、用户、节点、相对时间）+ 跳转按钮。
- [ ] **Step 5: 验证前端路由守卫与颜色 Token 守卫**
  - `pnpm --filter server test:frontend-route-guards`
  - `pnpm --filter server test:frontend-color-tokens`
- [ ] **Step 6: Git 提交 Task 3**
  `git add client/src/views/admin/AdminDashboardView.vue`
  `git commit -m "feat(admin): refactor revenue trend to full row and add marketing and recent instances cards"`

---

### Task 4: 端到端全量验证与构建验收
**Files:**
- None (verification only)

- [ ] **Step 1: 运行服务端与客户端类型检查**
  - `pnpm --filter server type-check`
  - `pnpm --filter client type-check`
- [ ] **Step 2: 运行前端双端完整构建**
  - `pnpm build:client`
- [ ] **Step 3: 运行全量核心守卫测试**
  - `pnpm --filter server test:admin-dashboard-overview-guards`
  - `pnpm --filter server test:frontend-route-guards`
  - `pnpm --filter server test:frontend-color-tokens`
  - `pnpm --filter server test:frontend-i18n-keys`
  - `make check`
- [ ] **Step 4: 更新 Walkthrough 总结**
