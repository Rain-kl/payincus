<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import { useToast } from '@/stores/toast'
import { useReveal } from '@/composables/useReveal'
import { PageHeader, Card, Badge, Button, EmptyState } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import {
  RefreshCw,
  Plus,
  ShoppingCart,
  Server,
  Ticket,
  BarChart3,
  TrendingUp,
  TrendingDown,
  CircleDollarSign,
  Activity,
  Users,
  ShieldCheck,
  Sparkles,
  CheckCircle2,
  ChevronRight
} from 'lucide-vue-next'

defineOptions({ name: 'AdminDashboardView' })

type StatisticsOverview = Awaited<ReturnType<typeof api.admin.getStatisticsOverview>>
type PeriodKey = 'daily' | 'monthly'
type TrendMetricKey = 'recharge' | 'consume'

interface StatPoint {
  label: string
  value: number
}

const router = useRouter()
const toast = useToast()
const { t, locale } = useI18n()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const loading = ref(true)
const stats = ref<StatisticsOverview | null>(null)
const lastUpdated = ref<string>('')
const timer = ref<ReturnType<typeof setInterval> | null>(null)

const trendPeriod = ref<PeriodKey>('daily')
const trendMetric = ref<TrendMetricKey>('recharge')

const moneyFormatter = computed(() =>
  new Intl.NumberFormat(locale.value, {
    style: 'currency',
    currency: 'CNY',
    maximumFractionDigits: 2
  })
)

const compactMoneyFormatter = computed(() =>
  new Intl.NumberFormat(locale.value, {
    style: 'currency',
    currency: 'CNY',
    notation: 'compact',
    maximumFractionDigits: 1
  })
)

const numberFormatter = computed(() => new Intl.NumberFormat(locale.value))

function formatMoney(value: number): string {
  return moneyFormatter.value.format(value)
}

function formatCompactMoney(value: number): string {
  return Math.abs(value) >= 10000
    ? compactMoneyFormatter.value.format(value)
    : moneyFormatter.value.format(value)
}

function formatNumber(value: number): string {
  return numberFormatter.value.format(value)
}

async function loadData(silent = false) {
  if (!silent) {
    loading.value = true
  }
  try {
    stats.value = await api.admin.getStatisticsOverview()
    const now = new Date()
    lastUpdated.value = now.toTimeString().slice(0, 8)
  } catch (error: any) {
    if (!silent) {
      toast.error(t('admin.statistics.loadFailed', {
        message: error?.message || t('admin.statistics.unknownError')
      }))
    }
  } finally {
    if (!silent) {
      loading.value = false
    }
  }
}

function startAutoRefresh() {
  stopAutoRefresh()
  timer.value = setInterval(() => {
    if (document.visibilityState === 'visible') {
      void loadData(true)
    }
  }, 30000)
}

function stopAutoRefresh() {
  if (timer.value) {
    clearInterval(timer.value)
    timer.value = null
  }
}

onMounted(() => {
  void loadData()
  startAutoRefresh()
})

onUnmounted(() => {
  stopAutoRefresh()
})

function jumpTo(path: string) {
  void router.push(path)
}

// 财务营收计算
const revenueToday = computed(() => stats.value?.operations.revenue.today || 0)
const revenueYesterday = computed(() => stats.value?.operations.revenue.yesterday || 0)
const revenue7Days = computed(() => stats.value?.operations.revenue.last7Days || 0)
const revenueDelta = computed(() => {
  const diff = revenueToday.value - revenueYesterday.value
  if (revenueYesterday.value <= 0) {
    return {
      hasBenchmark: false,
      text: revenueToday.value > 0 ? '+100%' : '0%',
      isPositive: revenueToday.value >= 0
    }
  }
  const pct = ((diff / revenueYesterday.value) * 100).toFixed(1)
  const isPositive = diff >= 0
  return {
    hasBenchmark: true,
    text: `${isPositive ? '+' : ''}${pct}%`,
    isPositive
  }
})

// 实例大盘计算
const totalInstances = computed(() => stats.value?.instances.total || 0)
const runningInstances = computed(() => stats.value?.operations.instances.running || 0)
const abnormalInstances = computed(() => stats.value?.operations.instances.abnormal || 0)
const expiringSoonInstances = computed(() => stats.value?.operations.instances.expiringSoon || 0)
const instanceHealthyRate = computed(() => {
  if (totalInstances.value <= 0) return '100'
  return ((runningInstances.value / totalInstances.value) * 100).toFixed(1)
})

// 用户与增长计算
const totalUsers = computed(() => stats.value?.users.total || 0)
const activeUsersToday = computed(() => stats.value?.operations.users.activeToday || 0)
const newUsersToday = computed(() => stats.value?.operations.users.newToday || 0)
const paidUsersTotal = computed(() => stats.value?.operations.users.paidTotal || 0)
const userPaidConversionRate = computed(() => {
  if (totalUsers.value <= 0) return '0'
  return ((paidUsersTotal.value / totalUsers.value) * 100).toFixed(1)
})

// 基础设施与节点在线率计算
const hostsTotal = computed(() => stats.value?.operations.infrastructure.hostsTotal || 0)
const hostsOnline = computed(() => stats.value?.operations.infrastructure.hostsOnline || 0)
const agentsOnline = computed(() => stats.value?.operations.infrastructure.agentsOnline || 0)
const agentsStale = computed(() => stats.value?.operations.infrastructure.agentsStale || 0)
const hostsOnlineRate = computed(() => {
  if (hostsTotal.value <= 0) return '100'
  return ((hostsOnline.value / hostsTotal.value) * 100).toFixed(0)
})

// 订单履约计算
const ordersTodayTotal = computed(() => stats.value?.operations.orders.todayTotal || 0)
const ordersTodaySuccess = computed(() => stats.value?.operations.orders.todaySuccess || 0)
const ordersTodayPending = computed(() => stats.value?.operations.orders.todayPending || 0)
const ordersTodayFailed = computed(() => stats.value?.operations.orders.todayFailed || 0)
const orderFulfillmentRate = computed(() => {
  if (ordersTodayTotal.value <= 0) return '100'
  return ((ordersTodaySuccess.value / ordersTodayTotal.value) * 100).toFixed(1)
})

const orderSuccessPct = computed(() => {
  if (ordersTodayTotal.value <= 0) return 0
  return Number(((ordersTodaySuccess.value / ordersTodayTotal.value) * 100).toFixed(1))
})

const orderPendingPct = computed(() => {
  if (ordersTodayTotal.value <= 0) return 0
  return Number(((ordersTodayPending.value / ordersTodayTotal.value) * 100).toFixed(1))
})

const orderFailedPct = computed(() => {
  if (ordersTodayTotal.value <= 0) return 0
  return Math.max(0, 100 - orderSuccessPct.value - orderPendingPct.value)
})

// 实例订购结构
const paidInstances = computed(() => stats.value?.instances.paid || 0)
const freeInstances = computed(() => stats.value?.instances.free || 0)
const paidFreeTotal = computed(() => paidInstances.value + freeInstances.value)
const paidInstancesPct = computed(() => {
  if (paidFreeTotal.value <= 0) return 0
  return Number(((paidInstances.value / paidFreeTotal.value) * 100).toFixed(1))
})
const freeInstancesPct = computed(() => {
  if (paidFreeTotal.value <= 0) return 0
  return Math.max(0, 100 - paidInstancesPct.value)
})

// 风险与待办事项映射
interface ActionableRiskItem {
  key: string
  severity: 'critical' | 'warning' | 'info'
  count: number
  title: string
  description: string
  targetPath: string
}

const activeRisks = computed<ActionableRiskItem[]>(() => {
  if (!stats.value?.operations.risks) return []
  return stats.value.operations.risks.map(r => {
    let targetPath = '/admin/settings'
    if (r.key === 'delivery_failed') {
      targetPath = '/admin/resources/hosts'
    } else if (r.key === 'payment_attention') {
      targetPath = '/admin/orders'
    } else if (r.key === 'host_offline' || r.key === 'agent_stale') {
      targetPath = '/admin/resources/hosts'
    } else if (r.key === 'ota_failed' || r.key === 'disk_update_error') {
      targetPath = '/admin/settings/update'
    }

    const title = t(`admin.statistics.operations.risks.${r.key}.title`, { count: r.count })
    const description = t(`admin.statistics.operations.risks.${r.key}.description`, { count: r.count })

    return {
      key: r.key,
      severity: r.severity,
      count: r.count,
      title,
      description,
      targetPath
    }
  })
})

// 走势图数据计算
const activeTrendSeries = computed<StatPoint[]>(() => {
  if (!stats.value) return []
  if (trendMetric.value === 'recharge') {
    return trendPeriod.value === 'daily'
      ? stats.value.billing.dailyRecharge
      : stats.value.billing.monthlyRecharge
  }
  return trendPeriod.value === 'daily'
    ? stats.value.billing.dailyConsume
    : stats.value.billing.monthlyConsume
})

const maxTrendValue = computed(() => {
  if (activeTrendSeries.value.length === 0) return 0
  return Math.max(0, ...activeTrendSeries.value.map(p => p.value))
})

function getBarHeight(point: StatPoint): number {
  if (maxTrendValue.value <= 0) return 0
  return Math.max((point.value / maxTrendValue.value) * 100, point.value > 0 ? 4 : 0)
}

function shouldShowTick(index: number, length: number): boolean {
  if (length <= 12) return true
  return index === 0 || index === length - 1 || index % 5 === 0
}

function formatTick(label: string): string {
  return label.length === 10 ? label.slice(5) : label
}

// 营销活动概览
const marketing = computed(() => stats.value?.marketing)
const activeLotteries = computed(() => marketing.value?.activeLotteries || [])
const totalActiveLotteries = computed(() => marketing.value?.totalActiveLotteries || 0)
const checkinInfo = computed(() => marketing.value?.checkin || {
  enabled: false,
  minPoints: 1,
  maxPoints: 500,
  requireInstance: false,
  todayCheckins: 0
})

// 最近开机记录
const recentInstances = computed(() => stats.value?.recentInstances || [])

function formatRelativeTime(dateStr: string): string {
  if (!dateStr) return ''
  const now = Date.now()
  const time = new Date(dateStr).getTime()
  const diffSeconds = Math.max(0, Math.floor((now - time) / 1000))
  if (diffSeconds < 60) return '刚刚'
  const diffMinutes = Math.floor(diffSeconds / 60)
  if (diffMinutes < 60) return `${diffMinutes} 分钟前`
  const diffHours = Math.floor(diffMinutes / 60)
  if (diffHours < 24) return `${diffHours} 小时前`
  const diffDays = Math.floor(diffHours / 24)
  if (diffDays < 30) return `${diffDays} 天前`
  return dateStr.slice(0, 10)
}

function getInstanceStatusDotClass(status: string): string {
  switch (status) {
    case 'running':
      return 'bg-[var(--success)]'
    case 'creating':
    case 'starting':
    case 'stopping':
    case 'rebuilding':
      return 'bg-[var(--warning)]'
    case 'error':
    case 'stopped':
    case 'frozen':
    default:
      return 'bg-[var(--error)]'
  }
}

function formatPlanSpecs(plan: { name: string; cpu: number; memory: number; disk: number } | null): string {
  if (!plan) return '基础实例'
  const ramGb = plan.memory >= 1024 ? `${(plan.memory / 1024).toFixed(0)}G` : `${plan.memory}M`
  const cores = Math.max(1, Math.round(plan.cpu / 100))
  return `${cores}C ${ramGb} · ${plan.name}`
}

// 待处理工单概览
const pendingTickets = computed(() => stats.value?.pendingTickets || [])
const pendingTicketsCount = computed(() => stats.value?.operations.support.openTickets ?? pendingTickets.value.length)

function getTicketPriorityBadgeVariant(priority: string): BadgeVariant {
  switch (priority) {
    case 'urgent':
      return 'error'
    case 'high':
      return 'warning'
    default:
      return 'default'
  }
}

function getTicketPriorityText(priority: string): string {
  switch (priority) {
    case 'urgent':
      return t('admin.dashboard.ticketPriorityUrgent')
    case 'high':
      return t('admin.dashboard.ticketPriorityHigh')
    case 'normal':
      return t('admin.dashboard.ticketPriorityNormal')
    case 'low':
      return t('admin.dashboard.ticketPriorityLow')
    default:
      return priority
  }
}
</script>

<template>
  <div ref="revealRoot" class="space-y-6 md:space-y-8 animate-fade-in">
    <!-- 顶部控制栏 (PageHeader) -->
    <PageHeader data-reveal>
      <template #title>
        <div class="flex items-center gap-3">
          <h1 class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ t('admin.dashboard.title') }}
          </h1>
          <Badge v-if="stats?.meta.timezone" variant="default" size="sm" class="font-mono">
            {{ stats.meta.timezone }}
          </Badge>
        </div>
      </template>
      <template #description>
        <p class="text-sm text-[var(--text-secondary)] mt-1 max-w-2xl">
          {{ t('admin.dashboard.description') }}
        </p>
      </template>
      <template #actions>
        <span v-if="lastUpdated" class="text-xs font-mono text-[var(--text-tertiary)] hidden sm:inline-block">
          {{ lastUpdated }}
        </span>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadData(false)"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5" :class="{ 'animate-spin': loading }" />
          </template>
          <span>{{ t('admin.statistics.refresh') }}</span>
        </Button>
      </template>
    </PageHeader>

    <!-- 快捷操作导航 (Apple 风格胶囊按钮群) -->
    <nav
      data-reveal
      aria-label="Quick Actions"
      class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-2.5 sm:p-3 flex flex-wrap items-center gap-2 shadow-[0_2px_8px_rgba(0,0,0,0.04)]"
    >
      <span class="text-xs font-medium text-[var(--text-tertiary)] uppercase tracking-wider mr-1 px-1">
        {{ t('admin.dashboard.quickActions') }}:
      </span>
      <Button
        variant="secondary"
        size="sm"
        @click="jumpTo('/admin/instances/create')"
      >
        <template #icon>
          <Plus class="w-3.5 h-3.5 text-[var(--accent)]" />
        </template>
        <span>{{ t('nav.create') }}</span>
      </Button>
      <Button
        variant="secondary"
        size="sm"
        @click="jumpTo('/admin/orders')"
      >
        <template #icon>
          <ShoppingCart class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
        </template>
        <span>{{ t('nav.orders') }}</span>
      </Button>
      <Button
        variant="secondary"
        size="sm"
        @click="jumpTo('/admin/resources/hosts')"
      >
        <template #icon>
          <Server class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
        </template>
        <span>{{ t('nav.hosts') }}</span>
      </Button>
      <Button
        variant="secondary"
        size="sm"
        @click="jumpTo('/admin/tickets')"
      >
        <template #icon>
          <Ticket class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
        </template>
        <span>{{ t('nav.tickets') }}</span>
      </Button>
      <Button
        variant="secondary"
        size="sm"
        class="sm:ml-auto"
        @click="jumpTo('/admin/statistics')"
      >
        <template #icon>
          <BarChart3 class="w-3.5 h-3.5 text-[var(--accent)]" />
        </template>
        <span>{{ t('nav.statistics') }}</span>
      </Button>
    </nav>

    <!-- 加载骨架 -->
    <div v-if="loading && !stats" class="space-y-6">
      <SkeletonLoader type="stats" />
      <SkeletonLoader type="card" />
    </div>

    <!-- 正文展示区 -->
    <div v-else-if="stats" class="space-y-6 md:space-y-8">
      <!-- Level 1: 四大顶层英雄卡片 (Headline KPI Cards) -->
      <section data-reveal aria-label="Headline KPIs" class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4 sm:gap-5">
        <!-- 1. 今日充值营收 -->
        <Card hoverable padding="sm" class="relative group">
          <div class="flex flex-col justify-between h-full min-h-[110px]">
            <div>
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.cards.todayRevenue') }}
                </span>
                <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)] group-hover:scale-105 transition-transform duration-300 shrink-0">
                  <CircleDollarSign class="w-5 h-5 stroke-[1.8]" />
                </div>
              </div>
              <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-[var(--text-primary)]">
                {{ formatCompactMoney(revenueToday) }}
              </p>
            </div>

            <div class="mt-3 pt-3 border-t border-[var(--border-color)] flex items-center justify-between text-xs">
              <span
                class="font-mono font-medium inline-flex items-center gap-1"
                :class="revenueDelta.isPositive ? 'text-[var(--success)]' : 'text-[var(--error)]'"
              >
                <TrendingUp v-if="revenueDelta.isPositive" class="w-3.5 h-3.5" />
                <TrendingDown v-else class="w-3.5 h-3.5" />
                <span>{{ revenueDelta.text }}</span>
                <span class="text-[var(--text-tertiary)] font-normal ml-0.5">vs 昨日</span>
              </span>
              <span class="text-[var(--text-tertiary)] font-mono tabular-nums">
                7日 {{ formatCompactMoney(revenue7Days) }}
              </span>
            </div>
          </div>
        </Card>

        <!-- 2. 运行中实例 -->
        <Card hoverable padding="sm" class="relative group">
          <div class="flex flex-col justify-between h-full min-h-[110px]">
            <div>
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.cards.runningInstances') }}
                </span>
                <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--success)] group-hover:scale-105 transition-transform duration-300 shrink-0">
                  <Activity class="w-5 h-5 stroke-[1.8]" />
                </div>
              </div>
              <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-[var(--text-primary)]">
                {{ formatNumber(runningInstances) }}
              </p>
            </div>

            <div class="mt-3 pt-3 border-t border-[var(--border-color)] flex items-center justify-between text-xs">
              <span class="text-[var(--text-secondary)]">
                {{ t('admin.dashboard.runningHealthyRate') }}:
                <strong class="font-mono font-semibold text-[var(--text-primary)]">{{ instanceHealthyRate }}%</strong>
              </span>
              <span
                class="font-mono tabular-nums"
                :class="abnormalInstances > 0 ? 'text-[var(--error)] font-semibold' : 'text-[var(--text-tertiary)]'"
              >
                异常: {{ abnormalInstances }}
              </span>
            </div>
          </div>
        </Card>

        <!-- 3. 今日活跃用户 -->
        <Card hoverable padding="sm" class="relative group">
          <div class="flex flex-col justify-between h-full min-h-[110px]">
            <div>
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.facts.activeUsers') }}
                </span>
                <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)] group-hover:scale-105 transition-transform duration-300 shrink-0">
                  <Users class="w-5 h-5 stroke-[1.8]" />
                </div>
              </div>
              <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-[var(--text-primary)]">
                {{ formatNumber(activeUsersToday) }}
              </p>
            </div>

            <div class="mt-3 pt-3 border-t border-[var(--border-color)] flex items-center justify-between text-xs">
              <span class="text-[var(--text-secondary)]">
                {{ t('admin.dashboard.userConversionRate') }}:
                <strong class="font-mono font-semibold text-[var(--text-primary)]">{{ userPaidConversionRate }}%</strong>
              </span>
              <span class="text-[var(--text-tertiary)] font-mono tabular-nums">
                新增 +{{ newUsersToday }}
              </span>
            </div>
          </div>
        </Card>

        <!-- 4. 节点与 Agent 在线率 -->
        <Card hoverable padding="sm" class="relative group">
          <div class="flex flex-col justify-between h-full min-h-[110px]">
            <div>
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.cards.onlineHosts') }}
                </span>
                <div
                  class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center group-hover:scale-105 transition-transform duration-300 shrink-0"
                  :class="agentsStale > 0 || (hostsTotal > 0 && hostsOnline < hostsTotal) ? 'text-[var(--error)]' : 'text-[var(--success)]'"
                >
                  <ShieldCheck class="w-5 h-5 stroke-[1.8]" />
                </div>
              </div>
              <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-[var(--text-primary)]">
                {{ hostsOnline }} <span class="text-base text-[var(--text-tertiary)] font-normal">/ {{ hostsTotal }}</span>
              </p>
            </div>

            <div class="mt-3 pt-3 border-t border-[var(--border-color)] flex items-center justify-between text-xs">
              <span class="text-[var(--text-secondary)]">
                在线率: <strong class="font-mono font-semibold text-[var(--text-primary)]">{{ hostsOnlineRate }}%</strong>
              </span>
              <span
                class="font-mono tabular-nums"
                :class="agentsStale > 0 ? 'text-[var(--error)] font-semibold' : 'text-[var(--text-tertiary)]'"
              >
                失联 Agent: {{ agentsStale }}
              </span>
            </div>
          </div>
        </Card>
      </section>

      <!-- Level 2: 重点待办与营销 (Campaigns & Pending Tickets) -->
      <section data-reveal aria-label="Campaigns and Pending Tickets" class="grid grid-cols-1 xl:grid-cols-2 gap-5">
        <!-- 1. 进行中的活动 (营销卡片) -->
        <Card padding="md" class="flex flex-col justify-between">
          <template #title>
            <div class="flex items-center gap-2">
              <Sparkles class="w-4 h-4 text-[var(--accent)]" />
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.marketingTitle') }}
              </h2>
            </div>
          </template>
          <template #subtitle>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ t('admin.dashboard.marketingDescription') }}
            </p>
          </template>
          <template #extra>
            <Badge variant="info" size="sm" class="font-mono">
              {{ t('admin.dashboard.activeCampaignsBadge', { count: totalActiveLotteries }) }}
            </Badge>
          </template>

          <div class="space-y-4">
            <!-- 签到活动状态条 -->
            <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 flex items-center justify-between">
              <div class="flex items-center gap-2.5">
                <span
                  class="w-2.5 h-2.5 rounded-full"
                  :class="checkinInfo.enabled ? 'bg-[var(--success)]' : 'bg-[var(--error)]'"
                />
                <div>
                  <span class="text-xs font-semibold text-[var(--text-primary)]">{{ t('admin.dashboard.dailyCheckin') }}</span>
                  <span class="text-[11px] text-[var(--text-secondary)] ml-2">
                    {{ checkinInfo.enabled ? t('admin.dashboard.checkinRule', { min: checkinInfo.minPoints, max: checkinInfo.maxPoints }) : t('admin.dashboard.checkinDisabled') }}
                  </span>
                </div>
              </div>
              <span class="text-xs font-mono tabular-nums text-[var(--text-secondary)]">
                {{ t('admin.dashboard.todayCheckinsCount', { count: checkinInfo.todayCheckins }) }}
              </span>
            </div>

            <!-- 生效中的抽奖活动列表 -->
            <div>
              <div v-if="activeLotteries.length > 0" class="divide-y divide-[var(--border-color)] border border-[var(--border-color)] rounded-xl overflow-hidden bg-[var(--bg-surface)]">
                <div
                  v-for="lottery in activeLotteries"
                  :key="lottery.id"
                  class="p-3 flex items-center justify-between hover:bg-[var(--bg-surface-soft)] transition-colors duration-200"
                >
                  <div class="min-w-0 pr-3">
                    <p class="text-xs font-medium text-[var(--text-primary)] truncate">
                      {{ lottery.name }}
                    </p>
                    <p class="text-[11px] text-[var(--text-secondary)] mt-0.5">
                      {{ t('admin.dashboard.costPointsPerDraw', { points: lottery.costPoints }) }}
                      <span class="mx-1 text-[var(--text-tertiary)]">·</span>
                      {{ t('admin.dashboard.drawsCount', { count: lottery.totalDraws }) }}
                    </p>
                  </div>
                  <Badge variant="default" size="sm" class="font-mono shrink-0">
                    {{ lottery.endAt ? lottery.endAt.slice(0, 10) : t('admin.dashboard.longTermActive') }}
                  </Badge>
                </div>
              </div>

              <EmptyState
                v-else
                :title="t('admin.dashboard.noActiveLotteries')"
                class="py-6 border border-[var(--border-color)] rounded-xl bg-[var(--bg-surface)]"
              />
            </div>
          </div>

          <template #footer>
            <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
              <span>{{ t('admin.dashboard.dailyCheckin') }}与抽奖活动</span>
              <button
                type="button"
                class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                @click="jumpTo('/admin/entertainment')"
              >
                <span>{{ t('admin.dashboard.manageMarketing') }}</span>
                <ChevronRight class="w-3.5 h-3.5" />
              </button>
            </div>
          </template>
        </Card>

        <!-- 2. 待处理工单 -->
        <Card padding="md" class="flex flex-col justify-between">
          <template #title>
            <div class="flex items-center gap-2">
              <Ticket class="w-4 h-4 text-[var(--accent)]" />
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.pendingTicketsTitle') }}
              </h2>
            </div>
          </template>
          <template #subtitle>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ t('admin.dashboard.pendingTicketsDescription') }}
            </p>
          </template>
          <template #extra>
            <Badge :variant="pendingTicketsCount > 0 ? 'warning' : 'default'" size="sm" class="font-mono">
              {{ t('admin.dashboard.pendingTicketsBadge', { count: pendingTicketsCount }) }}
            </Badge>
          </template>

          <!-- 工单列表 -->
          <div>
            <div v-if="pendingTickets.length > 0" class="divide-y divide-[var(--border-color)] border border-[var(--border-color)] rounded-xl overflow-hidden bg-[var(--bg-surface)]">
              <div
                v-for="ticket in pendingTickets"
                :key="ticket.id"
                class="p-3 flex items-center justify-between hover:bg-[var(--bg-surface-soft)] transition-colors duration-200 cursor-pointer active:scale-[0.99]"
                @click="jumpTo(`/admin/tickets?ticket=${ticket.id}`)"
              >
                <div class="min-w-0 pr-3">
                  <div class="flex items-center gap-2">
                    <Badge
                      :variant="getTicketPriorityBadgeVariant(ticket.priority)"
                      size="sm"
                      class="shrink-0 font-mono"
                    >
                      {{ getTicketPriorityText(ticket.priority) }}
                    </Badge>
                    <p class="text-xs font-medium text-[var(--text-primary)] truncate">
                      {{ ticket.subject }}
                    </p>
                  </div>
                  <p class="text-[11px] text-[var(--text-secondary)] mt-1 truncate">
                    {{ ticket.user.username }}
                    <span v-if="ticket.instance || ticket.host" class="mx-1 text-[var(--text-tertiary)]">·</span>
                    <span v-if="ticket.instance">{{ ticket.instance.name }}</span>
                    <span v-else-if="ticket.host">{{ ticket.host.name }}</span>
                    <span class="mx-1 text-[var(--text-tertiary)]">·</span>
                    <span class="capitalize">{{ ticket.category }}</span>
                  </p>
                </div>

                <span class="text-[11px] font-mono text-[var(--text-tertiary)] shrink-0">
                  {{ formatRelativeTime(ticket.createdAt) }}
                </span>
              </div>
            </div>

            <EmptyState
              v-else
              :title="t('admin.dashboard.noPendingTickets')"
              class="py-6 border border-[var(--border-color)] rounded-xl bg-[var(--bg-surface)]"
            />
          </div>

          <template #footer>
            <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
              <span>需管理员跟进与回复</span>
              <button
                type="button"
                class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                @click="jumpTo('/admin/tickets')"
              >
                <span>{{ t('admin.dashboard.manageTickets') }}</span>
                <ChevronRight class="w-3.5 h-3.5" />
              </button>
            </div>
          </template>
        </Card>
      </section>

      <!-- Level 3: 核心走势 (Revenue & Consumption Trend - Full Width) -->
      <section data-reveal aria-label="Revenue Trend" class="w-full">
        <Card padding="md" class="flex flex-col justify-between">
          <template #title>
            <div class="flex items-center gap-2">
              <BarChart3 class="w-4 h-4 text-[var(--accent)]" />
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.revenueTrend') }}
              </h2>
            </div>
          </template>
          <template #subtitle>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ trendPeriod === 'daily' ? t('admin.statistics.ranges.last30Days') : t('admin.statistics.ranges.last12Months') }}
            </p>
          </template>
          <template #extra>
            <div class="flex flex-wrap items-center gap-2">
              <!-- 指标切换 -->
              <div class="inline-flex rounded-full border border-[var(--border-color)] p-0.5 bg-[var(--bg-surface-soft)] text-xs">
                <button
                  type="button"
                  class="px-2.5 py-1 rounded-full transition-all duration-200"
                  :class="trendMetric === 'recharge' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] font-medium shadow-[0_1px_3px_rgba(0,0,0,0.06)]' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="trendMetric = 'recharge'"
                >
                  {{ t('admin.statistics.billingMetrics.recharge') }}
                </button>
                <button
                  type="button"
                  class="px-2.5 py-1 rounded-full transition-all duration-200"
                  :class="trendMetric === 'consume' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] font-medium shadow-[0_1px_3px_rgba(0,0,0,0.06)]' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="trendMetric = 'consume'"
                >
                  {{ t('admin.statistics.billingMetrics.consume') }}
                </button>
              </div>

              <!-- 周期切换 -->
              <div class="inline-flex rounded-full border border-[var(--border-color)] p-0.5 bg-[var(--bg-surface-soft)] text-xs">
                <button
                  type="button"
                  class="px-2.5 py-1 rounded-full transition-all duration-200"
                  :class="trendPeriod === 'daily' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] font-medium shadow-[0_1px_3px_rgba(0,0,0,0.06)]' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="trendPeriod = 'daily'"
                >
                  {{ t('admin.statistics.periods.daily') }}
                </button>
                <button
                  type="button"
                  class="px-2.5 py-1 rounded-full transition-all duration-200"
                  :class="trendPeriod === 'monthly' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] font-medium shadow-[0_1px_3px_rgba(0,0,0,0.06)]' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="trendPeriod = 'monthly'"
                >
                  {{ t('admin.statistics.periods.monthly') }}
                </button>
              </div>
            </div>
          </template>

          <!-- 柱状图表区 (原生 CSS 实现) -->
          <div class="chart-container mt-4">
            <div
              v-for="(point, index) in activeTrendSeries"
              :key="point.label"
              class="chart-bar-group group"
            >
              <div class="chart-track" :title="`${point.label}: ${formatMoney(point.value)}`">
                <span class="chart-popover">
                  {{ point.label }}: {{ formatCompactMoney(point.value) }}
                </span>
                <div
                  class="chart-bar-solid"
                  :class="trendMetric === 'recharge' ? 'chart-bar-accent' : 'chart-bar-emerald'"
                  :style="{ height: `${getBarHeight(point)}%` }"
                />
              </div>
              <span class="chart-label">
                {{ shouldShowTick(index, activeTrendSeries.length) ? formatTick(point.label) : '' }}
              </span>
            </div>
          </div>

          <template #footer>
            <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
              <span>峰值: <strong class="font-mono font-medium text-[var(--text-primary)]">{{ formatMoney(maxTrendValue) }}</strong></span>
              <button
                type="button"
                class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                @click="jumpTo('/admin/statistics')"
              >
                <span>查看财务明细</span>
                <ChevronRight class="w-3.5 h-3.5" />
              </button>
            </div>
          </template>
        </Card>
      </section>

      <!-- Level 4: 业务与运维多维分析 (Analytical Grid) -->
      <section data-reveal aria-label="Operations and Infrastructure Grid" class="grid grid-cols-1 xl:grid-cols-2 gap-5">
        <!-- 左列: 业务转化与营销主线 -->
        <div class="space-y-5">
          <!-- 1. 今日订单履约漏斗 -->
          <Card padding="md" class="flex flex-col justify-between">
            <template #title>
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.fulfillmentTitle') }}
              </h2>
            </template>
            <template #subtitle>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                今日创建 {{ ordersTodayTotal }} 笔订单履约转化分析
              </p>
            </template>
            <template #extra>
              <Badge variant="info" size="sm" class="font-mono">
                {{ t('admin.dashboard.fulfillmentRate', { rate: orderFulfillmentRate }) }}
              </Badge>
            </template>

            <!-- 分割进度条 -->
            <div class="space-y-4">
              <div class="h-2.5 rounded-full overflow-hidden bg-[var(--bg-surface-soft)] flex border border-[var(--border-color)]">
                <div
                  class="bg-[var(--success)] transition-all duration-500"
                  :style="{ width: `${orderSuccessPct}%` }"
                  :title="`成功: ${ordersTodaySuccess} (${orderSuccessPct}%)`"
                />
                <div
                  class="bg-[var(--warning)] transition-all duration-500"
                  :style="{ width: `${orderPendingPct}%` }"
                  :title="`待处理: ${ordersTodayPending} (${orderPendingPct}%)`"
                />
                <div
                  class="bg-[var(--error)] transition-all duration-500"
                  :style="{ width: `${orderFailedPct}%` }"
                  :title="`失败: ${ordersTodayFailed} (${orderFailedPct}%)`"
                />
              </div>

              <div class="grid grid-cols-3 gap-2 text-center">
                <div class="p-2.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                  <div class="flex items-center justify-center gap-1.5 text-xs text-[var(--text-secondary)]">
                    <span class="w-2 h-2 rounded-full bg-[var(--success)]" />
                    {{ t('admin.dashboard.fulfillmentSuccess') }}
                  </div>
                  <p class="mt-1 font-mono text-lg font-semibold tabular-nums text-[var(--text-primary)]">
                    {{ ordersTodaySuccess }}
                  </p>
                  <p class="text-[11px] font-mono text-[var(--text-tertiary)]">{{ orderSuccessPct }}%</p>
                </div>

                <div class="p-2.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                  <div class="flex items-center justify-center gap-1.5 text-xs text-[var(--text-secondary)]">
                    <span class="w-2 h-2 rounded-full bg-[var(--warning)]" />
                    {{ t('admin.dashboard.fulfillmentPending') }}
                  </div>
                  <p class="mt-1 font-mono text-lg font-semibold tabular-nums text-[var(--text-primary)]">
                    {{ ordersTodayPending }}
                  </p>
                  <p class="text-[11px] font-mono text-[var(--text-tertiary)]">{{ orderPendingPct }}%</p>
                </div>

                <div class="p-2.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                  <div class="flex items-center justify-center gap-1.5 text-xs text-[var(--text-secondary)]">
                    <span class="w-2 h-2 rounded-full bg-[var(--error)]" />
                    {{ t('admin.dashboard.fulfillmentFailed') }}
                  </div>
                  <p class="mt-1 font-mono text-lg font-semibold tabular-nums text-[var(--text-primary)]">
                    {{ ordersTodayFailed }}
                  </p>
                  <p class="text-[11px] font-mono text-[var(--text-tertiary)]">{{ orderFailedPct }}%</p>
                </div>
              </div>
            </div>

            <template #footer>
              <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
                <span>待关注阻断订单: <strong class="font-mono text-[var(--text-primary)]">{{ stats.operations.orders.needsAttention }}</strong> 笔</span>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                  @click="jumpTo('/admin/orders')"
                >
                  <span>进入订单中心</span>
                  <ChevronRight class="w-3.5 h-3.5" />
                </button>
              </div>
            </template>
          </Card>

          <!-- 2. 实例构成与到期分布 -->
          <Card padding="md" class="flex flex-col justify-between">
            <template #title>
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.instanceStructureTitle') }}
              </h2>
            </template>
            <template #subtitle>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                全平台 {{ totalInstances }} 台未删除实例的订购模式与健康状态
              </p>
            </template>
            <template #extra>
              <Badge variant="default" size="sm" class="font-mono">
                总量: {{ totalInstances }}
              </Badge>
            </template>

            <div class="space-y-4">
              <!-- 付费与免费占比条 -->
              <div>
                <div class="flex justify-between text-xs text-[var(--text-secondary)] mb-1.5">
                  <span class="inline-flex items-center gap-1.5">
                    <span class="w-2 h-2 rounded-full bg-[var(--accent)]" />
                    {{ t('admin.statistics.labels.paidInstances') }} ({{ paidInstances }}台)
                  </span>
                  <span class="inline-flex items-center gap-1.5">
                    <span class="w-2 h-2 rounded-full bg-[var(--success)]" />
                    {{ t('admin.statistics.labels.freeInstances') }} ({{ freeInstances }}台)
                  </span>
                </div>
                <div class="h-2.5 rounded-full overflow-hidden bg-[var(--bg-surface-soft)] flex border border-[var(--border-color)]">
                  <div class="bg-[var(--accent)] transition-all duration-500" :style="{ width: `${paidInstancesPct}%` }" />
                  <div class="bg-[var(--success)] transition-all duration-500" :style="{ width: `${freeInstancesPct}%` }" />
                </div>
              </div>

              <!-- 临期与健康概览 -->
              <div class="grid grid-cols-2 gap-3">
                <div class="p-3 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                  <p class="text-xs text-[var(--text-secondary)]">{{ t('admin.dashboard.expiringNotice') }}</p>
                  <p class="mt-1 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">
                    {{ expiringSoonInstances }}
                    <span class="text-xs text-[var(--text-tertiary)] font-normal ml-0.5">台</span>
                  </p>
                  <p class="text-[11px] text-[var(--text-tertiary)] mt-1">需关注续费留存</p>
                </div>

                <div class="p-3 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                  <p class="text-xs text-[var(--text-secondary)]">今日新交付实例</p>
                  <p class="mt-1 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">
                    +{{ stats.operations.instances.newToday }}
                    <span class="text-xs text-[var(--text-tertiary)] font-normal ml-0.5">台</span>
                  </p>
                  <p class="text-[11px] text-[var(--text-tertiary)] mt-1">生产环境在线增长</p>
                </div>
              </div>
            </div>

            <template #footer>
              <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
                <span>自营实例全部在线统计</span>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                  @click="jumpTo('/admin/resources/hosts')"
                >
                  <span>管理节点</span>
                  <ChevronRight class="w-3.5 h-3.5" />
                </button>
              </div>
            </template>
          </Card>
        </div>

        <!-- 右列: 资源交付与平台运行主线 -->
        <div class="space-y-5">
          <!-- 3. 最近开机记录卡片 -->
          <Card padding="md" class="flex flex-col justify-between">
            <template #title>
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.recentInstancesTitle') }}
              </h2>
            </template>
            <template #subtitle>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ t('admin.dashboard.recentInstancesDescription') }}
              </p>
            </template>
            <template #extra>
              <Badge variant="info" size="sm" class="font-mono">
                {{ t('admin.dashboard.newInstancesTodayBadge', { count: stats?.operations.instances.newToday || 0 }) }}
              </Badge>
            </template>

            <!-- 实例创建列表 -->
            <div>
              <div v-if="recentInstances.length > 0" class="divide-y divide-[var(--border-color)] border border-[var(--border-color)] rounded-xl overflow-hidden bg-[var(--bg-surface)]">
                <div
                  v-for="instance in recentInstances"
                  :key="instance.id"
                  class="p-3 flex items-center justify-between hover:bg-[var(--bg-surface-soft)] transition-colors duration-200"
                >
                  <div class="flex items-center gap-2.5 min-w-0 pr-3">
                    <span
                      class="w-2 h-2 rounded-full shrink-0"
                      :class="getInstanceStatusDotClass(instance.status)"
                      :title="instance.status"
                    />
                    <div class="min-w-0">
                      <div class="flex items-center gap-1.5">
                        <span class="text-xs font-mono font-semibold text-[var(--text-primary)] truncate">
                          {{ instance.name }}
                        </span>
                        <span class="text-[11px] font-mono text-[var(--text-tertiary)] truncate">
                          #{{ instance.id }}
                        </span>
                      </div>
                      <p class="text-[11px] text-[var(--text-secondary)] mt-0.5 truncate">
                        {{ instance.user.username }}
                        <span class="mx-1 text-[var(--text-tertiary)]">·</span>
                        {{ instance.host.name }}
                        <span class="mx-1 text-[var(--text-tertiary)]">·</span>
                        {{ formatPlanSpecs(instance.packagePlan) }}
                      </p>
                    </div>
                  </div>

                  <span class="text-[11px] font-mono text-[var(--text-tertiary)] shrink-0">
                    {{ formatRelativeTime(instance.createdAt) }}
                  </span>
                </div>
              </div>

              <EmptyState
                v-else
                :title="t('admin.dashboard.noRecentInstances')"
                class="py-6 border border-[var(--border-color)] rounded-xl bg-[var(--bg-surface)]"
              />
            </div>

            <template #footer>
              <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
                <span>全平台最新实例交付流</span>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                  @click="jumpTo('/admin/instances')"
                >
                  <span>{{ t('admin.dashboard.viewAllInstances') }}</span>
                  <ChevronRight class="w-3.5 h-3.5" />
                </button>
              </div>
            </template>
          </Card>

          <!-- 4. 平台运行事实 (Platform Pulse) -->
          <Card padding="md" class="flex flex-col justify-between">
            <template #title>
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.platformPulseTitle') }}
              </h2>
            </template>
            <template #subtitle>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                服务组件、工单、通知及底层状态
              </p>
            </template>
            <template #extra>
              <Badge variant="default" size="sm">
                实时状态
              </Badge>
            </template>

            <div class="divide-y divide-[var(--border-color)]">
              <!-- 待执行交付队列 -->
              <div class="py-2.5 flex items-center justify-between text-xs">
                <span class="text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.facts.pendingDelivery') }}
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="stats.operations.delivery.pendingTasks > 0 ? 'text-[var(--warning)]' : 'text-[var(--text-primary)]'"
                >
                  {{ stats.operations.delivery.pendingTasks }}
                </span>
              </div>

              <!-- 24h 失败交付 -->
              <div class="py-2.5 flex items-center justify-between text-xs">
                <span class="text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.facts.failedDelivery') }}
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="stats.operations.delivery.failedTasks24h > 0 ? 'text-[var(--error)]' : 'text-[var(--text-primary)]'"
                >
                  {{ stats.operations.delivery.failedTasks24h }}
                </span>
              </div>

              <!-- 待处理工单 -->
              <div class="py-2.5 flex items-center justify-between text-xs">
                <span class="text-[var(--text-secondary)]">
                  {{ t('admin.statistics.operations.cards.openTickets') }}
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="stats.operations.support.openTickets > 0 ? 'text-[var(--warning)]' : 'text-[var(--text-primary)]'"
                >
                  {{ stats.operations.support.openTickets }}
                </span>
              </div>

              <!-- 宿主机与在线 Agent -->
              <div class="py-2.5 flex items-center justify-between text-xs">
                <span class="text-[var(--text-secondary)]">
                  {{ t('admin.dashboard.hostsOverview') }}
                </span>
                <span class="font-mono text-xs font-semibold tabular-nums text-[var(--text-primary)]">
                  {{ hostsOnline }}/{{ hostsTotal }} 在线 ({{ agentsOnline }} Agent)
                </span>
              </div>

              <!-- 24h 通知 / 邮件失败 -->
              <div class="py-2.5 flex items-center justify-between text-xs">
                <span class="text-[var(--text-secondary)]">
                  24h 通知与邮件失败
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="(stats.operations.support.failedNotifications24h + stats.operations.support.failedEmails24h) > 0 ? 'text-[var(--warning)]' : 'text-[var(--text-primary)]'"
                >
                  {{ stats.operations.support.failedNotifications24h + stats.operations.support.failedEmails24h }}
                </span>
              </div>
            </div>

            <template #footer>
              <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
                <span>系统服务: 单实例运行正常</span>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline font-medium transition-colors"
                  @click="jumpTo('/admin/logs')"
                >
                  <span>审计日志</span>
                  <ChevronRight class="w-3.5 h-3.5" />
                </button>
              </div>
            </template>
          </Card>
        </div>
      </section>

      <!-- Level 5: 运维待办与风险雷达 (Action Hub) -->
      <section data-reveal aria-label="Operations Radar">
        <Card padding="md">
          <template #title>
            <div class="flex items-center gap-2">
              <span
                class="w-2.5 h-2.5 rounded-full"
                :class="activeRisks.length > 0 ? 'bg-[var(--warning)]' : 'bg-[var(--success)]'"
              />
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.dashboard.actionHubTitle') }}
              </h2>
            </div>
          </template>
          <template #subtitle>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ activeRisks.length > 0
                ? t('admin.dashboard.actionRequired') + ' - ' + t('admin.dashboard.actionCount', { count: activeRisks.length })
                : t('admin.dashboard.allClearDescription') }}
            </p>
          </template>
          <template #extra>
            <Badge
              :variant="activeRisks.length > 0 ? 'warning' : 'success'"
              size="sm"
              class="font-mono"
            >
              {{ activeRisks.length > 0 ? `${activeRisks.length} 待办` : '正常' }}
            </Badge>
          </template>

          <!-- 风险项列表 -->
          <div v-if="activeRisks.length > 0" class="grid grid-cols-1 md:grid-cols-2 gap-3">
            <div
              v-for="risk in activeRisks"
              :key="risk.key"
              class="p-3.5 rounded-xl border bg-[var(--bg-surface-soft)] flex items-start justify-between gap-3 transition-colors duration-200"
              :class="risk.severity === 'critical' ? 'border-[var(--error)]' : 'border-[var(--warning)]'"
            >
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <Badge
                    :variant="risk.severity === 'critical' ? 'error' : 'warning'"
                    size="sm"
                    class="shrink-0"
                  >
                    {{ risk.severity === 'critical' ? '紧急' : '预警' }}
                  </Badge>
                  <p class="text-sm font-semibold text-[var(--text-primary)] truncate">
                    {{ risk.title }}
                  </p>
                </div>
                <p class="text-xs text-[var(--text-secondary)] mt-1.5 leading-relaxed">
                  {{ risk.description }}
                </p>
              </div>

              <Button
                variant="secondary"
                size="sm"
                class="shrink-0 self-center"
                @click="jumpTo(risk.targetPath)"
              >
                {{ t('admin.dashboard.viewDetails') }}
              </Button>
            </div>
          </div>

          <!-- 全健康状态 -->
          <div v-else class="p-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between">
            <div class="flex items-center gap-3">
              <div class="w-8 h-8 rounded-full bg-[color-mix(in_srgb,var(--success)_15%,var(--bg-surface))] text-[var(--success)] flex items-center justify-center shrink-0">
                <CheckCircle2 class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <p class="text-sm font-medium text-[var(--text-primary)]">{{ t('admin.dashboard.allClearTitle') }}</p>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.dashboard.allClearDescription') }}</p>
              </div>
            </div>
            <span class="text-xs font-mono text-[var(--text-tertiary)] hidden sm:inline-block">
              {{ t('admin.dashboard.agentFreshnessNote') }}
            </span>
          </div>
        </Card>
      </section>
    </div>

    <!-- 空状态 (EmptyState) -->
    <EmptyState
      v-else
      :title="t('admin.statistics.noData')"
      class="py-16 border border-[var(--border-color)] rounded-2xl bg-[var(--bg-surface)]"
    >
      <template #action>
        <Button variant="primary" @click="loadData(false)">
          {{ t('admin.statistics.reload') }}
        </Button>
      </template>
    </EmptyState>
  </div>
</template>

<style scoped>
.chart-container {
  display: grid;
  grid-auto-flow: column;
  grid-auto-columns: minmax(24px, 1fr);
  align-items: end;
  gap: 0.35rem;
  min-height: 14rem;
  height: auto;
  overflow-x: auto;
  overflow-y: hidden;
  padding: 2.25rem 0.25rem 0.5rem;
  scrollbar-gutter: stable;
}

.chart-bar-group {
  min-width: 24px;
}

.chart-track {
  position: relative;
  display: flex;
  align-items: end;
  justify-content: center;
  height: 11.5rem;
  border-bottom: 1px solid var(--border-color);
}

.chart-bar-solid {
  width: min(75%, 1.5rem);
  min-height: 0;
  border-radius: 0.35rem 0.35rem 0 0;
  transform-origin: bottom;
  transition: height 0.25s ease;
}

.chart-bar-accent {
  background-color: var(--accent);
}

.chart-bar-emerald {
  background-color: var(--success);
}

.chart-popover {
  position: absolute;
  top: -2.25rem;
  left: 50%;
  transform: translateX(-50%);
  display: none;
  white-space: nowrap;
  font-size: 0.6875rem;
  font-family: ui-monospace, monospace;
  padding: 0.25rem 0.5rem;
  border-radius: 0.375rem;
  background-color: var(--bg-surface);
  color: var(--text-primary);
  border: 1px solid var(--border-color);
  box-shadow: 0 4px 6px -1px rgb(0 0 0 / 0.1);
  pointer-events: none;
  z-index: 10;
}

.chart-bar-group:hover .chart-popover {
  display: block;
}

.chart-bar-group:hover .chart-bar-solid {
  filter: brightness(1.1);
}

.chart-label {
  display: block;
  font-size: 0.6875rem;
  font-family: ui-monospace, monospace;
  color: var(--text-tertiary);
  text-align: center;
  margin-top: 0.5rem;
  height: 1rem;
}
</style>
