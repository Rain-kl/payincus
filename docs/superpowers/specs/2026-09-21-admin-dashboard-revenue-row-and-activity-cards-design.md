# 管理员大盘布局重构与营销/开机记录卡片设计规格 (Spec)

## 1. 概述与背景
管理后台运营大盘（`AdminDashboardView.vue`）当前采用双列网格布局，将「营收与消费趋势」与其他三个业务卡片并列放置。在展示 30 天日颗粒度或 12 个月月颗粒度的柱状图时，空间较为紧凑。
同时，管理员需要更直观地监控：
1. **营销活动态势**：哪些抽奖活动正在进行中、每日签到是否开启及今日参与热度；
2. **实例交付动向**：系统最近开机的实例记录、涉及的用户、节点与健康状态。

本设计将「营收与消费趋势」调整为**通栏独占一行**，并新增两个卡片：**【营销（进行中的活动）】** 与 **【最近开机记录（系统实例创建情况）】**，保持单请求聚合加载与 30s 无感知刷新。

---

## 2. 后端数据聚合设计 (`server/src/routes/admin-statistics.ts`)

在接口 `GET /api/admin/statistics/overview` 返回体中扩充两个字段：`marketing` 与 `recentInstances`。

### 2.1 营销数据 (`marketing`)
```ts
interface ActiveLotteryItem {
  id: number
  name: string
  costPoints: number
  totalDraws: number
  startAt: string | null
  endAt: string | null
}

interface MarketingOverview {
  activeLotteries: ActiveLotteryItem[]
  totalActiveLotteries: number
  checkin: {
    enabled: boolean
    minPoints: number
    maxPoints: number
    requireInstance: boolean
    todayCheckins: number
  }
}
```
- **数据抓取逻辑**：
  - `activeLotteries`：查询 `prisma.lottery`，条件为 `isActive: true` 且（`endAt` 为空 或 `endAt >= now()`），按 `createdAt: 'desc'` 取前 5 个；
  - `totalActiveLotteries`：同样条件的 `count()`；
  - `checkin`：
    - 读取 `systemConfig` 中的签到设置（或默认配置：`enabled: true, minPoints: 1, maxPoints: 500, requireInstance: false`）；
    - 统计今日签到量：查询 `prisma.dailyCheckin.count({ where: { dateKey: todayDateKey } })`。

### 2.2 最近开机记录 (`recentInstances`)
```ts
interface RecentInstanceItem {
  id: number
  name: string
  status: string
  createdAt: string
  user: {
    id: number
    username: string
    email: string
  }
  host: {
    id: number
    name: string
    countryCode: string | null
  }
  packagePlan: {
    name: string
    cpu: number
    memory: number
    disk: number
  } | null
}
```
- **数据抓取逻辑**：
  - 查询 `prisma.instance`，条件 `status: { not: 'deleted' }`；
  - 关联查询 `user` (id, username, email)、`host` (id, name, countryCode)、`packagePlan` (name, cpu, memory, disk)；
  - 按 `createdAt: 'desc'` 取前 5 台。

---

## 3. 前端布局与组件设计 (`client/src/views/admin/AdminDashboardView.vue`)

### 3.1 页面网格结构
- **第一层**：现有关键指标（4 张统计卡片，维持不变）；
- **第二层**：运维就绪与健康雷达（维持不变）；
- **第三层（独立通栏）**：
  - **「营收与消费走势」卡片（通栏 100% 宽度）**：
    - 图表区域具有更宽广的时间轴视野，各柱体间距与标签显示更为清晰舒展；
    - 保留充值/消费指标切换、日/月周期切换与峰值明细。
- **第四层（双列平衡网格 `grid grid-cols-1 xl:grid-cols-2 gap-5`）**：
  - **左列（业务与活动主线）**：
    1. 今日订单履约漏斗卡片（维持现有展示与漏斗条）；
    2. **【新增】营销（进行中的活动）卡片**。
  - **右列（资源与交付主线）**：
    1. **【新增】最近开机记录卡片**；
    2. 实例构成与到期分布卡片；
    3. 平台运行事实卡片（Platform Pulse）。

### 3.2 营销卡片展示细节
- **卡片头部**：
  - 标题：`t('admin.dashboard.marketingTitle')`（进行中的活动）；
  - 副标：`t('admin.dashboard.marketingDescription')`（抽奖活动、签到福利及参与热度）；
  - 徽章：`N 个进行中`。
- **卡片主体**：
  - **签到活动状态条**：
    - 状态指示（开启/关闭）、随机积分区间、今日已签到人数；
  - **抽奖活动列表**（最多展示 5 项）：
    - 活动名称、每次所需积分、累计抽奖次数、有效期标签；
  - **空状态**：无有效活动时展示友好占位文案。
- **卡片底栏**：
  - 快捷跳转按钮：`t('admin.dashboard.manageMarketing')` ➔ `/admin/entertainment`。

### 3.3 最近开机记录卡片展示细节
- **卡片头部**：
  - 标题：`t('admin.dashboard.recentInstancesTitle')`（最近开机记录）；
  - 副标：`t('admin.dashboard.recentInstancesDescription')`（系统最新创建与交付的实例情况）；
  - 徽章：`今日新开 +X 台`。
- **卡片主体**：
  - 紧凑列表展示最近 5 台实例：
    - 运行状态指示点（绿/琥珀/红，纯色主题 token，严格禁止透明度和硬编码 hex）；
    - 实例名称（粗体等宽）及配置方案摘要（如 `1C 1G · 低配`）；
    - 所属用户（`#id 用户名`）及所在宿主机；
    - 易读相对时间（刚刚、X分钟前、X小时前）。
- **卡片底栏**：
  - 快捷跳转按钮：`t('admin.dashboard.viewAllInstances')` ➔ `/admin/instances`。

---

## 4. 主题与设计系统规范 (遵守 `AGENTS.md` 与 `THEME.md`)
1. **纯黑白单色风格**：
   - 浅色模式为白底黑字，深色模式为黑底白字；
   - 状态指示色只保留绿（`bg-emerald-600`）、琥珀（`bg-amber-600`）、红（`bg-rose-600`）三种纯色；
   - 严禁任何硬编码十六进制颜色（如 `#161513` 等）；
   - 严禁任何透明度简写（如 `/50`、`/40`、`rgba()` 等），所有背景边框均采用实心语义 token（`bg-themed-surface`、`border-themed` 等）。
2. **多语言规范**：
   - 在 `zh-CN.ts`、`en.ts`、`zh-TW.ts` 中补齐所有新文案词条。

---

## 5. 验证与质量保证
1. **守卫验证**：
   - `pnpm --filter server test:frontend-route-guards`：验证大盘路由与跳转正确性；
   - `pnpm --filter server test:frontend-color-tokens`：强制拦截任何非法色值与透明度；
   - `pnpm --filter server test:frontend-i18n-keys`：验证三语字典完整性；
   - `pnpm --filter server type-check` 与 `pnpm --filter client type-check`：类型检查；
   - `make check`：代码与 Agent 哈希检查。
2. **UI 体验验证**：
   - 30秒无闪烁智能刷新验证；
   - 移动端与 PC 宽屏下的响应式表现验证。
