<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import { useToast } from '@/stores/toast'

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
      targetPath = '/admin/hosting'
    } else if (r.key === 'payment_attention') {
      targetPath = '/admin/orders'
    } else if (r.key === 'host_offline' || r.key === 'agent_stale') {
      targetPath = '/admin/resources/hosts'
    } else if (r.key === 'ota_failed' || r.key === 'disk_update_error') {
      targetPath = '/admin/system-update'
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
</script>

<template>
  <div class="kawaii-page animate-fade-in space-y-6">
    <!-- 顶部控制栏 -->
    <header class="flex flex-col md:flex-row md:items-center justify-between gap-4">
      <div>
        <div class="flex items-center gap-3">
          <h1 class="page-title text-2xl font-bold tracking-tight text-themed">
            {{ t('admin.dashboard.title') }}
          </h1>
          <span v-if="stats?.meta.timezone" class="inline-flex items-center px-2 py-0.5 rounded text-xs font-mono border border-themed bg-themed-tertiary text-themed-secondary">
            {{ stats.meta.timezone }}
          </span>
        </div>
        <p class="page-description text-sm text-themed-muted mt-1">
          {{ t('admin.dashboard.description') }}
        </p>
      </div>

      <div class="flex flex-wrap items-center gap-2.5">

        <span v-if="lastUpdated" class="text-xs font-mono text-themed-faint hidden sm:inline-block">
          {{ lastUpdated }}
        </span>

        <button
          type="button"
          class="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
          :disabled="loading"
          @click="loadData(false)"
        >
          <svg
            class="w-4 h-4"
            :class="{ 'animate-spin': loading }"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
            aria-hidden="true"
          >
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M4 4v5h.58m15.36 2A8 8 0 0 0 5.07 8.11M20 20v-5h-.58m0 0A8 8 0 0 1 4.06 12.03" />
          </svg>
          {{ t('admin.statistics.refresh') }}
        </button>
      </div>
    </header>

    <!-- 快捷操作入口 -->
    <nav aria-label="Quick Actions" class="flex flex-wrap items-center gap-2 p-3 rounded-lg border border-themed bg-themed-surface">
      <span class="text-xs font-medium text-themed-faint uppercase tracking-wider mr-1">
        {{ t('admin.dashboard.quickActions') }}:
      </span>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-themed text-xs font-medium text-themed hover:border-themed-strong bg-themed-surface transition-colors"
        @click="jumpTo('/admin/instances/create')"
      >
        <svg class="w-3.5 h-3.5 text-primary-600 dark:text-primary-400" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4v16m8-8H4" />
        </svg>
        {{ t('nav.create') }}
      </button>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-themed text-xs font-medium text-themed hover:border-themed-strong bg-themed-surface transition-colors"
        @click="jumpTo('/admin/orders')"
      >
        <svg class="w-3.5 h-3.5 text-themed-secondary" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M3 10h18M7 15h1m4 0h1m-7 4h12a3 3 0 003-3V8a3 3 0 00-3-3H6a3 3 0 00-3 3v8a3 3 0 003 3z" />
        </svg>
        {{ t('nav.orders') }}
      </button>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-themed text-xs font-medium text-themed hover:border-themed-strong bg-themed-surface transition-colors"
        @click="jumpTo('/admin/resources/hosts')"
      >
        <svg class="w-3.5 h-3.5 text-themed-secondary" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M5 12h14M5 12a2 2 0 01-2-2V6a2 2 0 012-2h14a2 2 0 012 2v4a2 2 0 01-2 2M5 12a2 2 0 00-2 2v4a2 2 0 002 2h14a2 2 0 002-2v-4a2 2 0 00-2-2" />
        </svg>
        {{ t('nav.hosts') }}
      </button>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-themed text-xs font-medium text-themed hover:border-themed-strong bg-themed-surface transition-colors"
        @click="jumpTo('/admin/tickets')"
      >
        <svg class="w-3.5 h-3.5 text-themed-secondary" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M15 5v2m0 4v2m0 4v2M5 5a2 2 0 00-2 2v3a2 2 0 110 4v3a2 2 0 002 2h14a2 2 0 002-2v-3a2 2 0 110-4V7a2 2 0 00-2-2H5z" />
        </svg>
        {{ t('nav.tickets') }}
      </button>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-themed text-xs font-medium text-themed hover:border-themed-strong bg-themed-surface transition-colors ml-auto"
        @click="jumpTo('/admin/statistics')"
      >
        <svg class="w-3.5 h-3.5 text-primary-600 dark:text-primary-400" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
        </svg>
        {{ t('nav.statistics') }}
      </button>
    </nav>

    <!-- 加载骨架 -->
    <div v-if="loading && !stats" class="space-y-6">
      <SkeletonLoader type="stats" />
      <SkeletonLoader type="card" />
    </div>

    <!-- 正文展示区 -->
    <div v-else-if="stats" class="space-y-6">
      <!-- Level 1: 四大顶层英雄卡片 (Headline KPI Cards) -->
      <section aria-label="Headline KPIs" class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4">
        <!-- 1. 今日充值营收 -->
        <div class="card p-4 sm:p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-2">
              <span class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">
                {{ t('admin.statistics.operations.cards.todayRevenue') }}
              </span>
              <div class="w-7 h-7 rounded-md border border-themed bg-themed-tertiary flex items-center justify-center">
                <svg class="w-4 h-4 text-primary-600 dark:text-primary-400" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M12 8c-1.657 0-3 .895-3 2s1.343 2 3 2 3 .895 3 2-1.343 2-3 2m0-8c1.11 0 2.08.402 2.599 1M12 8V7m0 1v8m0 0v1m0-1c-1.11 0-2.08-.402-2.599-1M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                </svg>
              </div>
            </div>
            <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-themed">
              {{ formatCompactMoney(revenueToday) }}
            </p>
          </div>

          <div class="mt-3 pt-3 border-t border-themed flex items-center justify-between text-xs">
            <span
              class="font-mono font-medium inline-flex items-center gap-1"
              :class="revenueDelta.isPositive ? 'text-emerald-700 dark:text-emerald-300' : 'text-rose-700 dark:text-rose-300'"
            >
              <span>{{ revenueDelta.isPositive ? '▲' : '▼' }}</span>
              <span>{{ revenueDelta.text }}</span>
              <span class="text-themed-faint font-normal">vs 昨日</span>
            </span>
            <span class="text-themed-faint font-mono">
              7日 {{ formatCompactMoney(revenue7Days) }}
            </span>
          </div>
        </div>

        <!-- 2. 运行中实例 -->
        <div class="card p-4 sm:p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-2">
              <span class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">
                {{ t('admin.statistics.operations.cards.runningInstances') }}
              </span>
              <div class="w-7 h-7 rounded-md border border-themed bg-themed-tertiary flex items-center justify-center">
                <svg class="w-4 h-4 text-emerald-600" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M5 12h14M5 12a2 2 0 01-2-2V6a2 2 0 012-2h14a2 2 0 012 2v4a2 2 0 01-2 2M5 12a2 2 0 00-2 2v4a2 2 0 002 2h14a2 2 0 002-2v-4a2 2 0 00-2-2" />
                </svg>
              </div>
            </div>
            <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-themed">
              {{ formatNumber(runningInstances) }}
            </p>
          </div>

          <div class="mt-3 pt-3 border-t border-themed flex items-center justify-between text-xs">
            <span class="text-themed-secondary">
              {{ t('admin.dashboard.runningHealthyRate') }}:
              <strong class="font-mono font-semibold text-themed">{{ instanceHealthyRate }}%</strong>
            </span>
            <span
              class="font-mono"
              :class="abnormalInstances > 0 ? 'text-rose-700 dark:text-rose-300 font-semibold' : 'text-themed-faint'"
            >
              异常: {{ abnormalInstances }}
            </span>
          </div>
        </div>

        <!-- 3. 今日活跃用户 -->
        <div class="card p-4 sm:p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-2">
              <span class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">
                {{ t('admin.statistics.operations.facts.activeUsers') }}
              </span>
              <div class="w-7 h-7 rounded-md border border-themed bg-themed-tertiary flex items-center justify-center">
                <svg class="w-4 h-4 text-primary-600 dark:text-primary-400" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0zm6 3a2 2 0 11-4 0 2 2 0 014 0zM7 10a2 2 0 11-4 0 2 2 0 014 0z" />
                </svg>
              </div>
            </div>
            <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-themed">
              {{ formatNumber(activeUsersToday) }}
            </p>
          </div>

          <div class="mt-3 pt-3 border-t border-themed flex items-center justify-between text-xs">
            <span class="text-themed-secondary">
              {{ t('admin.dashboard.userConversionRate') }}:
              <strong class="font-mono font-semibold text-themed">{{ userPaidConversionRate }}%</strong>
            </span>
            <span class="text-themed-faint font-mono">
              新增 +{{ newUsersToday }}
            </span>
          </div>
        </div>

        <!-- 4. 节点与 Agent 在线率 -->
        <div class="card p-4 sm:p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-2">
              <span class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">
                {{ t('admin.statistics.operations.cards.onlineHosts') }}
              </span>
              <div class="w-7 h-7 rounded-md border border-themed bg-themed-tertiary flex items-center justify-center">
                <svg
                  class="w-4 h-4"
                  :class="agentsStale > 0 || (hostsTotal > 0 && hostsOnline < hostsTotal) ? 'text-rose-700 dark:text-rose-300' : 'text-emerald-600'"
                  fill="none"
                  stroke="currentColor"
                  viewBox="0 0 24 24"
                  aria-hidden="true"
                >
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z" />
                </svg>
              </div>
            </div>
            <p class="mt-3 font-mono text-2xl sm:text-3xl font-semibold tabular-nums text-themed">
              {{ hostsOnline }} <span class="text-base text-themed-muted font-normal">/ {{ hostsTotal }}</span>
            </p>
          </div>

          <div class="mt-3 pt-3 border-t border-themed flex items-center justify-between text-xs">
            <span class="text-themed-secondary">
              在线率: <strong class="font-mono font-semibold text-themed">{{ hostsOnlineRate }}%</strong>
            </span>
            <span
              class="font-mono"
              :class="agentsStale > 0 ? 'text-rose-700 dark:text-rose-300 font-semibold' : 'text-themed-faint'"
            >
              失联 Agent: {{ agentsStale }}
            </span>
          </div>
        </div>
      </section>

      <!-- Level 2: 运维待办与风险雷达 (Action Hub) -->
      <section aria-label="Operations Radar" class="card p-5">
        <div class="flex items-center justify-between gap-3 border-b border-themed pb-4">
          <div>
            <h2 class="text-base font-semibold tracking-tight text-themed flex items-center gap-2">
              <span
                class="w-2.5 h-2.5 rounded-full"
                :class="activeRisks.length > 0 ? 'bg-amber-600' : 'bg-emerald-600'"
              ></span>
              {{ t('admin.dashboard.actionHubTitle') }}
            </h2>
            <p class="text-xs text-themed-muted mt-0.5">
              {{ activeRisks.length > 0
                ? t('admin.dashboard.actionRequired') + ' - ' + t('admin.dashboard.actionCount', { count: activeRisks.length })
                : t('admin.dashboard.allClearDescription') }}
            </p>
          </div>

          <span
            class="px-2.5 py-1 rounded text-xs font-mono font-medium border"
            :class="activeRisks.length > 0
              ? 'border-amber-600 text-amber-700 dark:text-amber-300 bg-themed-secondary'
              : 'border-emerald-600 text-emerald-700 dark:text-emerald-300 bg-themed-secondary'"
          >
            {{ activeRisks.length > 0 ? `${activeRisks.length} 待办` : '正常' }}
          </span>
        </div>

        <!-- 风险项列表 -->
        <div v-if="activeRisks.length > 0" class="mt-4 grid grid-cols-1 md:grid-cols-2 gap-3">
          <div
            v-for="risk in activeRisks"
            :key="risk.key"
            class="p-3.5 rounded-lg border bg-themed-secondary flex items-start justify-between gap-3"
            :class="risk.severity === 'critical' ? 'border-rose-600' : 'border-amber-600'"
          >
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span
                  class="px-1.5 py-0.5 rounded text-[10px] font-medium uppercase tracking-wide border"
                  :class="risk.severity === 'critical'
                    ? 'border-rose-600 text-rose-700 dark:text-rose-300'
                    : 'border-amber-600 text-amber-700 dark:text-amber-300'"
                >
                  {{ risk.severity === 'critical' ? '紧急' : '预警' }}
                </span>
                <p class="text-sm font-semibold text-themed truncate">
                  {{ risk.title }}
                </p>
              </div>
              <p class="text-xs text-themed-muted mt-1 leading-relaxed">
                {{ risk.description }}
              </p>
            </div>

            <button
              type="button"
              class="btn btn-secondary btn-sm shrink-0 self-center text-xs"
              @click="jumpTo(risk.targetPath)"
            >
              {{ t('admin.dashboard.viewDetails') }}
            </button>
          </div>
        </div>

        <!-- 全健康状态 -->
        <div v-else class="mt-4 p-4 rounded-lg border border-themed bg-themed-tertiary flex items-center justify-between">
          <div class="flex items-center gap-3">
            <div class="w-8 h-8 rounded-full border border-emerald-600 bg-themed-secondary flex items-center justify-center text-emerald-600">
              <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
              </svg>
            </div>
            <div>
              <p class="text-sm font-medium text-themed">{{ t('admin.dashboard.allClearTitle') }}</p>
              <p class="text-xs text-themed-muted mt-0.5">{{ t('admin.dashboard.allClearDescription') }}</p>
            </div>
          </div>
          <span class="text-xs font-mono text-themed-faint hidden sm:inline-block">
            {{ t('admin.dashboard.agentFreshnessNote') }}
          </span>
        </div>
      </section>

      <!-- Level 3: 核心走势与业务构成 (Analytical Grid) -->
      <section aria-label="Analytics and Structure" class="grid grid-cols-1 xl:grid-cols-2 gap-5">
        <!-- 1. 营收与消费走势 -->
        <div class="card p-5 flex flex-col justify-between">
          <div>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 border-b border-themed pb-4">
              <div>
                <h2 class="text-base font-semibold tracking-tight text-themed">
                  {{ t('admin.dashboard.revenueTrend') }}
                </h2>
                <p class="text-xs text-themed-muted mt-0.5">
                  {{ trendPeriod === 'daily' ? t('admin.statistics.ranges.last30Days') : t('admin.statistics.ranges.last12Months') }}
                </p>
              </div>

              <div class="flex flex-wrap items-center gap-2">
                <!-- 指标切换 -->
                <div class="inline-flex rounded-md border border-themed p-0.5 bg-themed-tertiary text-xs">
                  <button
                    type="button"
                    class="px-2.5 py-1 rounded transition-colors"
                    :class="trendMetric === 'recharge' ? 'bg-themed-surface text-themed font-medium' : 'text-themed-muted hover:text-themed'"
                    @click="trendMetric = 'recharge'"
                  >
                    {{ t('admin.statistics.billingMetrics.recharge') }}
                  </button>
                  <button
                    type="button"
                    class="px-2.5 py-1 rounded transition-colors"
                    :class="trendMetric === 'consume' ? 'bg-themed-surface text-themed font-medium' : 'text-themed-muted hover:text-themed'"
                    @click="trendMetric = 'consume'"
                  >
                    {{ t('admin.statistics.billingMetrics.consume') }}
                  </button>
                </div>

                <!-- 周期切换 -->
                <div class="inline-flex rounded-md border border-themed p-0.5 bg-themed-tertiary text-xs">
                  <button
                    type="button"
                    class="px-2 py-1 rounded transition-colors"
                    :class="trendPeriod === 'daily' ? 'bg-themed-surface text-themed font-medium' : 'text-themed-muted hover:text-themed'"
                    @click="trendPeriod = 'daily'"
                  >
                    {{ t('admin.statistics.periods.daily') }}
                  </button>
                  <button
                    type="button"
                    class="px-2 py-1 rounded transition-colors"
                    :class="trendPeriod === 'monthly' ? 'bg-themed-surface text-themed font-medium' : 'text-themed-muted hover:text-themed'"
                    @click="trendPeriod = 'monthly'"
                  >
                    {{ t('admin.statistics.periods.monthly') }}
                  </button>
                </div>
              </div>
            </div>

            <!-- 柱状图表区 (原生 CSS 实现) -->
            <div class="chart-container mt-6">
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
                  ></div>
                </div>
                <span class="chart-label">
                  {{ shouldShowTick(index, activeTrendSeries.length) ? formatTick(point.label) : '' }}
                </span>
              </div>
            </div>
          </div>

          <div class="mt-4 pt-3 border-t border-themed flex items-center justify-between text-xs text-themed-muted">
            <span>峰值: {{ formatMoney(maxTrendValue) }}</span>
            <button
              type="button"
              class="text-primary-600 dark:text-primary-400 hover:underline font-medium"
              @click="jumpTo('/admin/statistics')"
            >
              查看财务明细 ➔
            </button>
          </div>
        </div>

        <!-- 2. 今日订单履约漏斗 -->
        <div class="card p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-3 border-b border-themed pb-4">
              <div>
                <h2 class="text-base font-semibold tracking-tight text-themed">
                  {{ t('admin.dashboard.fulfillmentTitle') }}
                </h2>
                <p class="text-xs text-themed-muted mt-0.5">
                  今日创建 {{ ordersTodayTotal }} 笔订单履约转化分析
                </p>
              </div>
              <span class="text-xs font-mono font-semibold px-2 py-1 rounded border border-themed bg-themed-tertiary text-themed">
                {{ t('admin.dashboard.fulfillmentRate', { rate: orderFulfillmentRate }) }}
              </span>
            </div>

            <!-- 分割进度条 -->
            <div class="mt-6">
              <div class="h-3 rounded-full overflow-hidden bg-themed-tertiary flex border border-themed">
                <div
                  class="bg-emerald-600"
                  :style="{ width: `${orderSuccessPct}%` }"
                  :title="`成功: ${ordersTodaySuccess} (${orderSuccessPct}%)`"
                ></div>
                <div
                  class="bg-amber-600"
                  :style="{ width: `${orderPendingPct}%` }"
                  :title="`待处理: ${ordersTodayPending} (${orderPendingPct}%)`"
                ></div>
                <div
                  class="bg-rose-600"
                  :style="{ width: `${orderFailedPct}%` }"
                  :title="`失败: ${ordersTodayFailed} (${orderFailedPct}%)`"
                ></div>
              </div>

              <div class="grid grid-cols-3 gap-2 mt-4 text-center">
                <div class="p-2.5 rounded-lg border border-themed bg-themed-surface">
                  <div class="flex items-center justify-center gap-1.5 text-xs text-themed-muted">
                    <span class="w-2 h-2 rounded-full bg-emerald-600"></span>
                    {{ t('admin.dashboard.fulfillmentSuccess') }}
                  </div>
                  <p class="mt-1 font-mono text-lg font-semibold tabular-nums text-themed">
                    {{ ordersTodaySuccess }}
                  </p>
                  <p class="text-[11px] font-mono text-themed-faint">{{ orderSuccessPct }}%</p>
                </div>

                <div class="p-2.5 rounded-lg border border-themed bg-themed-surface">
                  <div class="flex items-center justify-center gap-1.5 text-xs text-themed-muted">
                    <span class="w-2 h-2 rounded-full bg-amber-600"></span>
                    {{ t('admin.dashboard.fulfillmentPending') }}
                  </div>
                  <p class="mt-1 font-mono text-lg font-semibold tabular-nums text-themed">
                    {{ ordersTodayPending }}
                  </p>
                  <p class="text-[11px] font-mono text-themed-faint">{{ orderPendingPct }}%</p>
                </div>

                <div class="p-2.5 rounded-lg border border-themed bg-themed-surface">
                  <div class="flex items-center justify-center gap-1.5 text-xs text-themed-muted">
                    <span class="w-2 h-2 rounded-full bg-rose-600"></span>
                    {{ t('admin.dashboard.fulfillmentFailed') }}
                  </div>
                  <p class="mt-1 font-mono text-lg font-semibold tabular-nums text-themed">
                    {{ ordersTodayFailed }}
                  </p>
                  <p class="text-[11px] font-mono text-themed-faint">{{ orderFailedPct }}%</p>
                </div>
              </div>
            </div>
          </div>

          <div class="mt-4 pt-3 border-t border-themed flex items-center justify-between text-xs text-themed-muted">
            <span>待关注阻断订单: {{ stats.operations.orders.needsAttention }} 笔</span>
            <button
              type="button"
              class="text-primary-600 dark:text-primary-400 hover:underline font-medium"
              @click="jumpTo('/admin/orders')"
            >
              进入订单中心 ➔
            </button>
          </div>
        </div>

        <!-- 3. 实例构成与到期分布 -->
        <div class="card p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-3 border-b border-themed pb-4">
              <div>
                <h2 class="text-base font-semibold tracking-tight text-themed">
                  {{ t('admin.dashboard.instanceStructureTitle') }}
                </h2>
                <p class="text-xs text-themed-muted mt-0.5">
                  全平台 {{ totalInstances }} 台未删除实例的订购模式与健康状态
                </p>
              </div>
              <span class="text-xs font-mono text-themed-secondary">
                总量: {{ totalInstances }}
              </span>
            </div>

            <div class="mt-6 space-y-4">
              <!-- 付费与免费占比条 -->
              <div>
                <div class="flex justify-between text-xs text-themed-secondary mb-1.5">
                  <span class="inline-flex items-center gap-1.5">
                    <span class="w-2 h-2 rounded-full bg-blue-600"></span>
                    {{ t('admin.statistics.labels.paidInstances') }} ({{ paidInstances }}台)
                  </span>
                  <span class="inline-flex items-center gap-1.5">
                    <span class="w-2 h-2 rounded-full bg-emerald-600"></span>
                    {{ t('admin.statistics.labels.freeInstances') }} ({{ freeInstances }}台)
                  </span>
                </div>
                <div class="h-3 rounded-full overflow-hidden bg-themed-tertiary flex border border-themed">
                  <div class="bg-blue-600" :style="{ width: `${paidInstancesPct}%` }"></div>
                  <div class="bg-emerald-600" :style="{ width: `${freeInstancesPct}%` }"></div>
                </div>
              </div>

              <!-- 临期与健康概览 -->
              <div class="grid grid-cols-2 gap-3 mt-4">
                <div class="p-3 rounded-lg border border-themed bg-themed-surface">
                  <p class="text-xs text-themed-muted">{{ t('admin.dashboard.expiringNotice') }}</p>
                  <p class="mt-1 font-mono text-xl font-semibold tabular-nums text-themed">
                    {{ expiringSoonInstances }}
                    <span class="text-xs text-themed-faint font-normal">台</span>
                  </p>
                  <p class="text-[11px] text-themed-faint mt-1">需关注续费留存</p>
                </div>

                <div class="p-3 rounded-lg border border-themed bg-themed-surface">
                  <p class="text-xs text-themed-muted">今日新交付实例</p>
                  <p class="mt-1 font-mono text-xl font-semibold tabular-nums text-themed">
                    +{{ stats.operations.instances.newToday }}
                    <span class="text-xs text-themed-faint font-normal">台</span>
                  </p>
                  <p class="text-[11px] text-themed-faint mt-1">生产环境在线增长</p>
                </div>
              </div>
            </div>
          </div>

          <div class="mt-4 pt-3 border-t border-themed flex items-center justify-between text-xs text-themed-muted">
            <span>托管 / 自营实例全部在线统计</span>
            <button
              type="button"
              class="text-primary-600 dark:text-primary-400 hover:underline font-medium"
              @click="jumpTo('/admin/hosting')"
            >
              管理托管实例 ➔
            </button>
          </div>
        </div>

        <!-- 4. 平台运行事实 (Platform Pulse) -->
        <div class="card p-5 flex flex-col justify-between">
          <div>
            <div class="flex items-center justify-between gap-3 border-b border-themed pb-4">
              <div>
                <h2 class="text-base font-semibold tracking-tight text-themed">
                  {{ t('admin.dashboard.platformPulseTitle') }}
                </h2>
                <p class="text-xs text-themed-muted mt-0.5">
                  服务组件、工单、通知及底层状态
                </p>
              </div>
              <span class="text-xs font-mono text-themed-faint">
                实时状态
              </span>
            </div>

            <div class="mt-4 divide-y divide-themed">
              <!-- 待执行交付队列 -->
              <div class="py-2.5 flex items-center justify-between">
                <span class="text-xs text-themed-secondary">
                  {{ t('admin.statistics.operations.facts.pendingDelivery') }}
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="stats.operations.delivery.pendingTasks > 0 ? 'text-amber-700 dark:text-amber-300' : 'text-themed'"
                >
                  {{ stats.operations.delivery.pendingTasks }}
                </span>
              </div>

              <!-- 24h 失败交付 -->
              <div class="py-2.5 flex items-center justify-between">
                <span class="text-xs text-themed-secondary">
                  {{ t('admin.statistics.operations.facts.failedDelivery') }}
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="stats.operations.delivery.failedTasks24h > 0 ? 'text-rose-700 dark:text-rose-300' : 'text-themed'"
                >
                  {{ stats.operations.delivery.failedTasks24h }}
                </span>
              </div>

              <!-- 待处理工单 -->
              <div class="py-2.5 flex items-center justify-between">
                <span class="text-xs text-themed-secondary">
                  {{ t('admin.statistics.operations.cards.openTickets') }}
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="stats.operations.support.openTickets > 0 ? 'text-amber-700 dark:text-amber-300' : 'text-themed'"
                >
                  {{ stats.operations.support.openTickets }}
                </span>
              </div>

              <!-- 宿主机与在线 Agent -->
              <div class="py-2.5 flex items-center justify-between">
                <span class="text-xs text-themed-secondary">
                  {{ t('admin.dashboard.hostsOverview') }}
                </span>
                <span class="font-mono text-xs font-semibold tabular-nums text-themed">
                  {{ hostsOnline }}/{{ hostsTotal }} 在线 ({{ agentsOnline }} Agent)
                </span>
              </div>

              <!-- 24h 通知 / 邮件失败 -->
              <div class="py-2.5 flex items-center justify-between">
                <span class="text-xs text-themed-secondary">
                  24h 通知与邮件失败
                </span>
                <span
                  class="font-mono text-xs font-semibold tabular-nums"
                  :class="(stats.operations.support.failedNotifications24h + stats.operations.support.failedEmails24h) > 0 ? 'text-amber-700 dark:text-amber-300' : 'text-themed'"
                >
                  {{ stats.operations.support.failedNotifications24h + stats.operations.support.failedEmails24h }}
                </span>
              </div>
            </div>
          </div>

          <div class="mt-4 pt-3 border-t border-themed flex items-center justify-between text-xs text-themed-muted">
            <span>系统单实例: 127.0.0.1:3001 正常</span>
            <button
              type="button"
              class="text-primary-600 dark:text-primary-400 hover:underline font-medium"
              @click="jumpTo('/admin/logs')"
            >
              审计日志 ➔
            </button>
          </div>
        </div>
      </section>
    </div>

    <!-- 空状态 -->
    <div v-else class="card p-12 text-center">
      <p class="text-themed-muted">{{ t('admin.statistics.noData') }}</p>
      <button type="button" class="btn btn-primary mt-4" @click="loadData(false)">
        {{ t('admin.statistics.reload') }}
      </button>
    </div>
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
  overflow-x: auto;
  padding: 2rem 0.25rem 0.75rem;
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
