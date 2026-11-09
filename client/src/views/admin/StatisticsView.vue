<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  RefreshCw,
  TrendingUp,
  Users,
  Server,
  CircleDollarSign,
  ShoppingCart,
  HardDrive,
  MessageSquare,
  BarChart3,
  ShieldAlert,
  AlertTriangle,
  CheckCircle2,
  Activity,
  Percent
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  EmptyState,
  Select
} from '@/components/ui'

defineOptions({ name: 'StatisticsView' })

type StatisticsOverview = Awaited<ReturnType<typeof api.admin.getStatisticsOverview>>
type TabKey = 'users' | 'instances' | 'billing'
type PeriodKey = 'daily' | 'monthly'
type BillingMetricKey = 'recharge' | 'consume' | 'aff' | 'destroyFee'
type ChartValueType = 'number' | 'money'
type Tone = 'blue' | 'emerald' | 'amber' | 'rose'

interface StatPoint {
  label: string
  value: number
}

interface SummaryCard {
  label: string
  value: number
  type: ChartValueType
  caption: string
  tone: Tone
}

interface OperationCard extends SummaryCard {
  detail?: string
}

const toast = useToast()
const { t, locale } = useI18n()
const loading = ref(true)
const stats = ref<StatisticsOverview | null>(null)
const activeTab = ref<TabKey>('users')
const userPeriod = ref<PeriodKey>('daily')
const instancePeriod = ref<PeriodKey>('daily')
const billingPeriod = ref<PeriodKey>('daily')
const billingMetric = ref<BillingMetricKey>('recharge')

const tabItems: Array<{ value: TabKey; labelKey: string }> = [
  { value: 'users', labelKey: 'admin.statistics.tabs.users' },
  { value: 'instances', labelKey: 'admin.statistics.tabs.instances' },
  { value: 'billing', labelKey: 'admin.statistics.tabs.billing' }
]

const periodItems: Array<{ value: PeriodKey; labelKey: string }> = [
  { value: 'daily', labelKey: 'admin.statistics.periods.daily' },
  { value: 'monthly', labelKey: 'admin.statistics.periods.monthly' }
]

const periodOptions = computed(() =>
  periodItems.map(item => ({
    value: item.value,
    label: t(item.labelKey)
  }))
)

const billingMetricItems: Array<{ value: BillingMetricKey; labelKey: string; tone: Tone }> = [
  { value: 'recharge', labelKey: 'admin.statistics.billingMetrics.recharge', tone: 'blue' },
  { value: 'consume', labelKey: 'admin.statistics.billingMetrics.consume', tone: 'emerald' },
  { value: 'aff', labelKey: 'admin.statistics.billingMetrics.aff', tone: 'amber' },
  { value: 'destroyFee', labelKey: 'admin.statistics.billingMetrics.destroyFee', tone: 'rose' }
]

const billingMetricOptions = computed(() =>
  billingMetricItems.map(item => ({
    value: item.value,
    label: t(item.labelKey)
  }))
)

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

const compactNumberFormatter = computed(() =>
  new Intl.NumberFormat(locale.value, {
    notation: 'compact',
    maximumFractionDigits: 1
  })
)

const userSeries = computed<StatPoint[]>(() => {
  if (!stats.value) return []
  return userPeriod.value === 'daily'
    ? stats.value.users.dailyNewUsers
    : stats.value.users.monthlyNewUsers
})

const instanceSeries = computed<StatPoint[]>(() => {
  if (!stats.value) return []
  return instancePeriod.value === 'daily'
    ? stats.value.instances.dailyCreatedInstances
    : stats.value.instances.monthlyCreatedInstances
})

const currentBillingMetric = computed(() =>
  billingMetricItems.find(item => item.value === billingMetric.value) || billingMetricItems[0]
)

const billingSeries = computed<StatPoint[]>(() => {
  if (!stats.value) return []

  if (billingMetric.value === 'consume') {
    return billingPeriod.value === 'daily'
      ? stats.value.billing.dailyConsume
      : stats.value.billing.monthlyConsume
  }

  if (billingMetric.value === 'aff') {
    return billingPeriod.value === 'daily'
      ? stats.value.billing.dailyAff
      : stats.value.billing.monthlyAff
  }

  if (billingMetric.value === 'destroyFee') {
    return billingPeriod.value === 'daily'
      ? stats.value.billing.dailyDestroyFee
      : stats.value.billing.monthlyDestroyFee
  }

  return billingPeriod.value === 'daily'
    ? stats.value.billing.dailyRecharge
    : stats.value.billing.monthlyRecharge
})

const userCards = computed<SummaryCard[]>(() => [
  {
    label: t('admin.statistics.cards.totalUsers'),
    value: stats.value?.users.total || 0,
    type: 'number',
    caption: t('admin.statistics.captions.currentTotal'),
    tone: 'blue'
  },
  {
    label: userPeriod.value === 'daily'
      ? t('admin.statistics.cards.recentDailyNewUsers')
      : t('admin.statistics.cards.recentMonthlyNewUsers'),
    value: sumSeries(userSeries.value),
    type: 'number',
    caption: userPeriod.value === 'daily'
      ? t('admin.statistics.captions.dailyAggregate')
      : t('admin.statistics.captions.monthlyAggregate'),
    tone: 'emerald'
  },
  {
    label: t('admin.statistics.cards.averageNewUsers'),
    value: averageSeries(userSeries.value),
    type: 'number',
    caption: userPeriod.value === 'daily'
      ? t('admin.statistics.captions.dailyAverage')
      : t('admin.statistics.captions.monthlyAverage'),
    tone: 'amber'
  }
])

const instanceCards = computed<SummaryCard[]>(() => [
  {
    label: t('admin.statistics.cards.totalInstances'),
    value: stats.value?.instances.total || 0,
    type: 'number',
    caption: t('admin.statistics.captions.nonDeletedInstances'),
    tone: 'blue'
  },
  {
    label: t('admin.statistics.cards.availableInstances'),
    value: stats.value?.instances.active || 0,
    type: 'number',
    caption: t('admin.statistics.captions.notDeletedOrSuspended'),
    tone: 'emerald'
  },
  {
    label: instancePeriod.value === 'daily'
      ? t('admin.statistics.cards.recentDailyCreatedInstances')
      : t('admin.statistics.cards.recentMonthlyCreatedInstances'),
    value: sumSeries(instanceSeries.value),
    type: 'number',
    caption: instancePeriod.value === 'daily'
      ? t('admin.statistics.captions.dailyAggregate')
      : t('admin.statistics.captions.monthlyAggregate'),
    tone: 'amber'
  }
])

const billingCards = computed<SummaryCard[]>(() => [
  {
    label: t('admin.statistics.cards.totalRecharge'),
    value: stats.value?.billing.totals.recharge || 0,
    type: 'money',
    caption: t('admin.statistics.captions.completedOrders'),
    tone: 'blue'
  },
  {
    label: t('admin.statistics.cards.totalConsume'),
    value: stats.value?.billing.totals.consume || 0,
    type: 'money',
    caption: t('admin.statistics.captions.totalScope'),
    tone: 'emerald'
  },
  {
    label: t('admin.statistics.cards.totalAff'),
    value: stats.value?.billing.totals.aff || 0,
    type: 'money',
    caption: t('admin.statistics.captions.affCommission'),
    tone: 'amber'
  },
  {
    label: t('admin.statistics.cards.totalDestroyFee'),
    value: stats.value?.billing.totals.destroyFee || 0,
    type: 'money',
    caption: t('admin.statistics.captions.userDestroyFee'),
    tone: 'rose'
  }
])

const operationCards = computed<OperationCard[]>(() => [
  {
    label: t('admin.statistics.operations.cards.todayRevenue'),
    value: stats.value?.operations.revenue.today || 0,
    type: 'money',
    caption: t('admin.statistics.operations.captions.yesterday', {
      value: formatCompactValue(stats.value?.operations.revenue.yesterday || 0, 'money')
    }),
    detail: t('admin.statistics.operations.captions.last30Days', {
      value: formatCompactValue(stats.value?.operations.revenue.last30Days || 0, 'money')
    }),
    tone: 'blue'
  },
  {
    label: t('admin.statistics.operations.cards.last7DaysRevenue'),
    value: stats.value?.operations.revenue.last7Days || 0,
    type: 'money',
    caption: t('admin.statistics.operations.captions.completedRevenue'),
    tone: 'emerald'
  },
  {
    label: t('admin.statistics.operations.cards.todayOrders'),
    value: stats.value?.operations.orders.todayTotal || 0,
    type: 'number',
    caption: t('admin.statistics.operations.captions.orderBreakdown', {
      success: stats.value?.operations.orders.todaySuccess || 0,
      failed: stats.value?.operations.orders.todayFailed || 0,
      pending: stats.value?.operations.orders.todayPending || 0
    }),
    tone: 'amber'
  },
  {
    label: t('admin.statistics.operations.cards.runningInstances'),
    value: stats.value?.operations.instances.running || 0,
    type: 'number',
    caption: t('admin.statistics.operations.captions.instanceBreakdown', {
      abnormal: stats.value?.operations.instances.abnormal || 0,
      expiring: stats.value?.operations.instances.expiringSoon || 0
    }),
    tone: (stats.value?.operations.instances.abnormal || 0) > 0 ? 'rose' : 'emerald'
  },
  {
    label: t('admin.statistics.operations.cards.onlineHosts'),
    value: stats.value?.operations.infrastructure.hostsOnline || 0,
    type: 'number',
    caption: t('admin.statistics.operations.captions.hostBreakdown', {
      total: stats.value?.operations.infrastructure.hostsTotal || 0,
      agents: stats.value?.operations.infrastructure.agentsOnline || 0
    }),
    tone: (stats.value?.operations.infrastructure.agentsStale || 0) > 0 ? 'rose' : 'blue'
  },
  {
    label: t('admin.statistics.operations.cards.openTickets'),
    value: stats.value?.operations.support.openTickets || 0,
    type: 'number',
    caption: t('admin.statistics.operations.captions.supportBreakdown', {
      unread: stats.value?.operations.support.unreadInboxMessages || 0,
      failed: (stats.value?.operations.support.failedNotifications24h || 0) + (stats.value?.operations.support.failedEmails24h || 0)
    }),
    tone: (stats.value?.operations.support.openTickets || 0) > 0 ? 'amber' : 'emerald'
  }
])

const operationFacts = computed(() => [
  {
    label: t('admin.statistics.operations.facts.newUsers'),
    value: formatValue(stats.value?.operations.users.newToday || 0, 'number')
  },
  {
    label: t('admin.statistics.operations.facts.activeUsers'),
    value: formatValue(stats.value?.operations.users.activeToday || 0, 'number')
  },
  {
    label: t('admin.statistics.operations.facts.paidUsers'),
    value: formatValue(stats.value?.operations.users.paidTotal || 0, 'number')
  },
  {
    label: t('admin.statistics.operations.facts.newInstances'),
    value: formatValue(stats.value?.operations.instances.newToday || 0, 'number')
  },
  {
    label: t('admin.statistics.operations.facts.pendingDelivery'),
    value: formatValue(stats.value?.operations.delivery.pendingTasks || 0, 'number')
  },
  {
    label: t('admin.statistics.operations.facts.failedDelivery'),
    value: formatValue(stats.value?.operations.delivery.failedTasks24h || 0, 'number')
  }
])

const operationRisks = computed(() => stats.value?.operations.risks || [])

const paidFreeTotal = computed(() =>
  (stats.value?.instances.paid || 0) + (stats.value?.instances.free || 0)
)

const paidPercent = computed(() => getPercent(stats.value?.instances.paid || 0, paidFreeTotal.value))
const freePercent = computed(() => getPercent(stats.value?.instances.free || 0, paidFreeTotal.value))

onMounted(() => {
  void loadStatistics()
})

async function loadStatistics() {
  loading.value = true
  try {
    stats.value = await api.admin.getStatisticsOverview()
  } catch (error: any) {
    toast.error(t('admin.statistics.loadFailed', {
      message: error?.message || t('admin.statistics.unknownError')
    }))
  } finally {
    loading.value = false
  }
}

function sumSeries(points: StatPoint[]): number {
  return points.reduce((sum, point) => sum + point.value, 0)
}

function averageSeries(points: StatPoint[]): number {
  if (points.length === 0) return 0
  return Number((sumSeries(points) / points.length).toFixed(2))
}

function maxSeries(points: StatPoint[]): number {
  return Math.max(0, ...points.map(point => point.value))
}

function barHeight(point: StatPoint, points: StatPoint[]): number {
  const max = maxSeries(points)
  if (max <= 0) return 0
  return Math.max((point.value / max) * 100, point.value > 0 ? 3 : 0)
}

function shouldShowTick(index: number, length: number): boolean {
  if (length <= 12) return true
  return index === 0 || index === length - 1 || index % 5 === 0
}

function formatTick(label: string): string {
  return label.length === 10 ? label.slice(5) : label
}

function formatValue(value: number, type: ChartValueType): string {
  if (type === 'money') return moneyFormatter.value.format(value)
  return numberFormatter.value.format(value)
}

function formatCompactValue(value: number, type: ChartValueType): string {
  if (type === 'money') return Math.abs(value) >= 10000
    ? compactMoneyFormatter.value.format(value)
    : moneyFormatter.value.format(value)
  return Math.abs(value) >= 10000
    ? compactNumberFormatter.value.format(value)
    : numberFormatter.value.format(value)
}

function getPercent(value: number, total: number): number {
  if (total <= 0) return 0
  return Number(((value / total) * 100).toFixed(1))
}

function cardToneClass(tone: Tone): string {
  switch (tone) {
    case 'emerald':
      return 'text-[var(--chart-2)] bg-[color-mix(in_srgb,var(--chart-2)_12%,var(--bg-surface))] border-[color-mix(in_srgb,var(--chart-2)_24%,var(--bg-surface))]'
    case 'amber':
      return 'text-[var(--chart-3)] bg-[color-mix(in_srgb,var(--chart-3)_12%,var(--bg-surface))] border-[color-mix(in_srgb,var(--chart-3)_24%,var(--bg-surface))]'
    case 'rose':
      return 'text-[var(--chart-4)] bg-[color-mix(in_srgb,var(--chart-4)_12%,var(--bg-surface))] border-[color-mix(in_srgb,var(--chart-4)_24%,var(--bg-surface))]'
    default:
      return 'text-[var(--chart-1)] bg-[color-mix(in_srgb,var(--chart-1)_12%,var(--bg-surface))] border-[color-mix(in_srgb,var(--chart-1)_24%,var(--bg-surface))]'
  }
}

function barToneClass(tone: Tone): string {
  switch (tone) {
    case 'emerald':
      return 'chart-bar-emerald'
    case 'amber':
      return 'chart-bar-amber'
    case 'rose':
      return 'chart-bar-rose'
    default:
      return 'chart-bar-blue'
  }
}

function riskToneClass(severity: string): string {
  if (severity === 'critical') {
    return 'text-[var(--error)] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] border-[color-mix(in_srgb,var(--error)_25%,var(--bg-surface))]'
  }
  if (severity === 'warning') {
    return 'text-[var(--warning)] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] border-[color-mix(in_srgb,var(--warning)_25%,var(--bg-surface))]'
  }
  return 'text-[var(--text-secondary)] bg-[var(--bg-surface-soft)] border-[var(--border-color)]'
}

function getOperationCardIcon(index: number) {
  switch (index) {
    case 0: return CircleDollarSign
    case 1: return TrendingUp
    case 2: return ShoppingCart
    case 3: return Server
    case 4: return HardDrive
    case 5: return MessageSquare
    default: return BarChart3
  }
}

function getUserCardIcon(index: number) {
  switch (index) {
    case 0: return Users
    case 1: return TrendingUp
    default: return Activity
  }
}

function getInstanceCardIcon(index: number) {
  switch (index) {
    case 0: return Server
    case 1: return CheckCircle2
    default: return TrendingUp
  }
}

function getBillingCardIcon(index: number) {
  switch (index) {
    case 0: return CircleDollarSign
    case 1: return ShoppingCart
    case 2: return Users
    default: return Percent
  }
}

function getTabIcon(tab: TabKey) {
  switch (tab) {
    case 'users': return Users
    case 'instances': return Server
    case 'billing': return CircleDollarSign
  }
}
</script>

<template>
  <div class="space-y-6 md:space-y-8 animate-fade-in">
    <!-- 顶部控制栏 (PageHeader) -->
    <PageHeader>
      <template #title>
        <div class="flex items-center gap-3">
          <h1 class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ t('admin.statistics.title') }}
          </h1>
          <Badge v-if="stats?.meta.timezone" variant="default" size="sm" class="font-mono">
            {{ t('admin.statistics.timezone', { timezone: stats.meta.timezone }) }}
          </Badge>
        </div>
      </template>

      <template #description>
        <p class="text-sm text-[var(--text-secondary)] mt-1 max-w-2xl leading-relaxed">
          {{ t('admin.statistics.description') }}
        </p>
      </template>

      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadStatistics"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5" :class="{ 'animate-spin': loading }" />
          </template>
          <span>{{ t('admin.statistics.refresh') }}</span>
        </Button>
      </template>
    </PageHeader>

    <!-- 加载骨架屏 -->
    <div v-if="loading" class="space-y-6">
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        <Card v-for="i in 6" :key="i" :loading="true" padding="sm" />
      </div>
      <div class="grid grid-cols-1 xl:grid-cols-[minmax(0,1fr)_380px] gap-6">
        <Card :loading="true" />
        <Card :loading="true" />
      </div>
    </div>

    <!-- 统计详情内容 -->
    <div v-else-if="stats" class="space-y-8">
      <!-- 运营大盘总览 (Commercial Operations Overview) -->
      <section class="space-y-5">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
          <div>
            <div class="flex items-center gap-2.5">
              <h2 class="text-xl font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('admin.statistics.operations.title') }}
              </h2>
              <Badge variant="info" size="sm" dot>Live</Badge>
            </div>
            <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
              {{ t('admin.statistics.operations.description') }}
            </p>
          </div>
          <div class="inline-flex items-center gap-1.5 px-3 py-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs font-mono text-[var(--text-tertiary)] self-start sm:self-auto select-none">
            <Activity class="w-3.5 h-3.5 text-[var(--accent)]" />
            <span>{{ t('admin.statistics.operations.agentFreshness', { minutes: 30 }) }}</span>
          </div>
        </div>

        <!-- 运营核心指标卡片 -->
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          <Card
            v-for="(card, idx) in operationCards"
            :key="card.label"
            padding="sm"
            class="transition-all duration-300 hover:shadow-md group"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl border transition-all duration-300 group-hover:scale-105"
                :class="cardToneClass(card.tone)"
              >
                <component :is="getOperationCardIcon(idx)" class="h-5 w-5" />
              </div>
              <p class="min-w-0 flex-1 text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)] truncate">
                {{ card.label }}
              </p>
            </div>
            <div class="mt-4">
              <p class="font-mono text-2xl sm:text-3xl font-bold tracking-tight tabular-nums text-[var(--text-primary)]">
                {{ formatCompactValue(card.value, card.type) }}
              </p>
              <p class="mt-1.5 text-xs text-[var(--text-secondary)] truncate">
                {{ card.caption }}
              </p>
              <p v-if="card.detail" class="mt-0.5 text-xs text-[var(--text-tertiary)] truncate">
                {{ card.detail }}
              </p>
            </div>
          </Card>
        </div>

        <!-- 风险雷达与今日运营脉搏 -->
        <div class="grid grid-cols-1 xl:grid-cols-[minmax(0,1fr)_380px] gap-6">
          <!-- 运营风险雷达 -->
          <Card class="transition-all duration-300">
            <template #header>
              <div class="flex items-center justify-between w-full">
                <div class="flex items-center gap-2.5">
                  <div class="w-8 h-8 rounded-xl flex items-center justify-center bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))]">
                    <ShieldAlert class="w-4 h-4" />
                  </div>
                  <div>
                    <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)]">
                      {{ t('admin.statistics.operations.riskTitle') }}
                    </h3>
                    <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                      {{ t('admin.statistics.operations.riskDescription') }}
                    </p>
                  </div>
                </div>
                <Badge
                  :variant="operationRisks.length ? 'warning' : 'success'"
                  :dot="true"
                  :pulse="operationRisks.length > 0"
                  size="sm"
                >
                  {{ operationRisks.length ? t('admin.statistics.operations.riskCount', { count: operationRisks.length }) : t('admin.statistics.operations.noRisk') }}
                </Badge>
              </div>
            </template>

            <div v-if="operationRisks.length" class="space-y-3">
              <div
                v-for="risk in operationRisks"
                :key="risk.key"
                :class="['border rounded-xl p-3.5 transition-all duration-200', riskToneClass(risk.severity)]"
              >
                <div class="flex items-start justify-between gap-3">
                  <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-2">
                      <AlertTriangle v-if="risk.severity === 'critical' || risk.severity === 'warning'" class="w-4 h-4 shrink-0" />
                      <p class="text-sm font-semibold tracking-tight">
                        {{ t(`admin.statistics.operations.risks.${risk.key}.title`, { count: risk.count }) }}
                      </p>
                    </div>
                    <p class="text-xs mt-1.5 opacity-85 leading-relaxed">
                      {{ t(`admin.statistics.operations.risks.${risk.key}.description`, { count: risk.count }) }}
                    </p>
                  </div>
                  <Badge
                    :variant="risk.severity === 'critical' ? 'error' : risk.severity === 'warning' ? 'warning' : 'info'"
                    size="sm"
                    class="shrink-0 uppercase font-mono text-[10px]"
                  >
                    {{ t(`admin.statistics.operations.severity.${risk.severity}`) }}
                  </Badge>
                </div>
              </div>
            </div>

            <div v-else class="rounded-xl border border-[color-mix(in_srgb,var(--success)_25%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--success)_8%,var(--bg-surface))] p-5 text-center">
              <div class="w-10 h-10 rounded-full mx-auto mb-2 flex items-center justify-center bg-[color-mix(in_srgb,var(--success)_16%,var(--bg-surface))] text-[var(--success)]">
                <CheckCircle2 class="w-5 h-5" />
              </div>
              <p class="text-sm font-semibold text-[var(--success)]">{{ t('admin.statistics.operations.healthyTitle') }}</p>
              <p class="text-xs text-[var(--text-secondary)] mt-1 max-w-sm mx-auto">{{ t('admin.statistics.operations.healthyDescription') }}</p>
            </div>
          </Card>

          <!-- 今日运营脉搏 -->
          <Card class="transition-all duration-300">
            <template #header>
              <div>
                <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)]">
                  {{ t('admin.statistics.operations.todayTitle') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.statistics.operations.todayDescription') }}
                </p>
              </div>
            </template>

            <div class="divide-y divide-[var(--border-color)]">
              <div
                v-for="fact in operationFacts"
                :key="fact.label"
                class="flex items-center justify-between gap-4 py-3 first:pt-0 last:pb-0"
              >
                <span class="text-sm text-[var(--text-secondary)]">{{ fact.label }}</span>
                <span class="font-mono text-sm font-semibold tabular-nums text-[var(--text-primary)]">{{ fact.value }}</span>
              </div>
            </div>
          </Card>
        </div>
      </section>

      <!-- 维度切换胶囊栏 (Apple Segmented Capsule Bar) -->
      <div class="flex items-center justify-between border-b border-[var(--border-color)] pb-3">
        <div class="inline-flex p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <button
            v-for="tab in tabItems"
            :key="tab.value"
            type="button"
            class="inline-flex items-center gap-2 px-4 sm:px-5 py-2 text-xs sm:text-sm font-medium rounded-full transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
            :class="activeTab === tab.value
              ? 'bg-[var(--accent)] text-white shadow-sm'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="activeTab = tab.value"
          >
            <component :is="getTabIcon(tab.value)" class="w-4 h-4" />
            <span>{{ t(tab.labelKey) }}</span>
          </button>
        </div>
      </div>

      <!-- 用户分析 Tab -->
      <section v-if="activeTab === 'users'" class="space-y-6">
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Card
            v-for="(card, idx) in userCards"
            :key="card.label"
            padding="sm"
            class="transition-all duration-300 hover:shadow-md group"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl border transition-all duration-300 group-hover:scale-105"
                :class="cardToneClass(card.tone)"
              >
                <component :is="getUserCardIcon(idx)" class="h-5 w-5" />
              </div>
              <p class="min-w-0 flex-1 text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)] truncate">
                {{ card.label }}
              </p>
            </div>
            <div class="mt-4">
              <p class="font-mono text-2xl sm:text-3xl font-bold tracking-tight tabular-nums text-[var(--text-primary)]">
                {{ formatValue(card.value, card.type) }}
              </p>
              <p class="mt-1.5 text-xs text-[var(--text-secondary)] truncate">{{ card.caption }}</p>
            </div>
          </Card>
        </div>

        <Card class="transition-all duration-300">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 w-full">
              <div>
                <div class="flex items-center gap-2.5">
                  <span class="w-2.5 h-2.5 rounded-full bg-[var(--chart-1)] shrink-0"></span>
                  <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)]">
                    {{ t('admin.statistics.sections.newUsers') }}
                  </h3>
                </div>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ userPeriod === 'daily' ? t('admin.statistics.ranges.last30Days') : t('admin.statistics.ranges.last12Months') }}
                </p>
              </div>

              <!-- 周期选择：桌面胶囊 / 移动端 Select -->
              <div class="hidden sm:inline-flex p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <button
                  v-for="period in periodItems"
                  :key="period.value"
                  type="button"
                  class="px-3.5 py-1 text-xs font-medium rounded-full transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
                  :class="userPeriod === period.value
                    ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
                    : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="userPeriod = period.value"
                >
                  {{ t(period.labelKey) }}
                </button>
              </div>
              <div class="sm:hidden w-28">
                <Select
                  v-model="userPeriod"
                  :options="periodOptions"
                  size="sm"
                />
              </div>
            </div>
          </template>

          <div class="chart-area mt-4">
            <div v-for="(point, index) in userSeries" :key="point.label" class="chart-column group">
              <div class="chart-value" :title="`${point.label}: ${formatValue(point.value, 'number')}`">
                <span class="chart-tooltip">{{ t('admin.statistics.tooltip', { label: point.label, value: formatValue(point.value, 'number') }) }}</span>
                <div
                  :class="['chart-bar', barToneClass('blue')]"
                  :style="{ height: `${barHeight(point, userSeries)}%` }"
                ></div>
              </div>
              <span class="chart-tick">{{ shouldShowTick(index, userSeries.length) ? formatTick(point.label) : '' }}</span>
            </div>
          </div>
        </Card>
      </section>

      <!-- 实例分析 Tab -->
      <section v-else-if="activeTab === 'instances'" class="space-y-6">
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Card
            v-for="(card, idx) in instanceCards"
            :key="card.label"
            padding="sm"
            class="transition-all duration-300 hover:shadow-md group"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl border transition-all duration-300 group-hover:scale-105"
                :class="cardToneClass(card.tone)"
              >
                <component :is="getInstanceCardIcon(idx)" class="h-5 w-5" />
              </div>
              <p class="min-w-0 flex-1 text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)] truncate">
                {{ card.label }}
              </p>
            </div>
            <div class="mt-4">
              <p class="font-mono text-2xl sm:text-3xl font-bold tracking-tight tabular-nums text-[var(--text-primary)]">
                {{ formatValue(card.value, card.type) }}
              </p>
              <p class="mt-1.5 text-xs text-[var(--text-secondary)] truncate">{{ card.caption }}</p>
            </div>
          </Card>
        </div>

        <div class="grid grid-cols-1 xl:grid-cols-[minmax(0,1fr)_360px] gap-6">
          <Card class="transition-all duration-300">
            <template #header>
              <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 w-full">
                <div>
                  <div class="flex items-center gap-2.5">
                    <span class="w-2.5 h-2.5 rounded-full bg-[var(--chart-2)] shrink-0"></span>
                    <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)]">
                      {{ t('admin.statistics.sections.createdInstances') }}
                    </h3>
                  </div>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ instancePeriod === 'daily' ? t('admin.statistics.ranges.last30Days') : t('admin.statistics.ranges.last12Months') }}
                  </p>
                </div>

                <!-- 周期选择：桌面胶囊 / 移动端 Select -->
                <div class="hidden sm:inline-flex p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                  <button
                    v-for="period in periodItems"
                    :key="period.value"
                    type="button"
                    class="px-3.5 py-1 text-xs font-medium rounded-full transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
                    :class="instancePeriod === period.value
                      ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
                      : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                    @click="instancePeriod = period.value"
                  >
                    {{ t(period.labelKey) }}
                  </button>
                </div>
                <div class="sm:hidden w-28">
                  <Select
                    v-model="instancePeriod"
                    :options="periodOptions"
                    size="sm"
                  />
                </div>
              </div>
            </template>

            <div class="chart-area mt-4">
              <div v-for="(point, index) in instanceSeries" :key="point.label" class="chart-column group">
                <div class="chart-value" :title="`${point.label}: ${formatValue(point.value, 'number')}`">
                  <span class="chart-tooltip">{{ t('admin.statistics.tooltip', { label: point.label, value: formatValue(point.value, 'number') }) }}</span>
                  <div
                    :class="['chart-bar', barToneClass('emerald')]"
                    :style="{ height: `${barHeight(point, instanceSeries)}%` }"
                  ></div>
                </div>
                <span class="chart-tick">{{ shouldShowTick(index, instanceSeries.length) ? formatTick(point.label) : '' }}</span>
              </div>
            </div>
          </Card>

          <!-- 付费/免费占比卡片 -->
          <Card class="transition-all duration-300">
            <template #header>
              <div>
                <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)]">
                  {{ t('admin.statistics.sections.paidFreeInstances') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.statistics.sections.paidFreeDescription') }}
                </p>
              </div>
            </template>

            <div class="space-y-6 pt-2">
              <!-- 分割比例进度条 -->
              <div class="h-3 rounded-full overflow-hidden bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex p-0.5">
                <div
                  class="stat-split-seg rounded-l-full bg-[var(--chart-1)]"
                  :style="{ width: `${paidPercent}%` }"
                  :title="`${t('admin.statistics.labels.paidInstances')}: ${paidPercent}%`"
                ></div>
                <div
                  class="stat-split-seg rounded-r-full bg-[var(--chart-2)]"
                  :style="{ width: `${freePercent}%` }"
                  :title="`${t('admin.statistics.labels.freeInstances')}: ${freePercent}%`"
                ></div>
              </div>

              <!-- 比例细分列表 -->
              <div class="space-y-3">
                <div class="flex items-center justify-between p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-colors">
                  <div class="flex items-center gap-3">
                    <span class="w-3 h-3 rounded-full bg-[var(--chart-1)] shrink-0"></span>
                    <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('admin.statistics.labels.paidInstances') }}</span>
                  </div>
                  <div class="text-right">
                    <p class="font-mono text-base font-semibold tabular-nums text-[var(--text-primary)]">{{ formatValue(stats.instances.paid, 'number') }}</p>
                    <p class="font-mono text-xs tabular-nums text-[var(--text-tertiary)]">{{ paidPercent }}%</p>
                  </div>
                </div>

                <div class="flex items-center justify-between p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-colors">
                  <div class="flex items-center gap-3">
                    <span class="w-3 h-3 rounded-full bg-[var(--chart-2)] shrink-0"></span>
                    <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('admin.statistics.labels.freeInstances') }}</span>
                  </div>
                  <div class="text-right">
                    <p class="font-mono text-base font-semibold tabular-nums text-[var(--text-primary)]">{{ formatValue(stats.instances.free, 'number') }}</p>
                    <p class="font-mono text-xs tabular-nums text-[var(--text-tertiary)]">{{ freePercent }}%</p>
                  </div>
                </div>
              </div>
            </div>
          </Card>
        </div>
      </section>

      <!-- 财务营收 Tab -->
      <section v-else class="space-y-6">
        <div class="grid grid-cols-2 lg:grid-cols-4 gap-4">
          <Card
            v-for="(card, idx) in billingCards"
            :key="card.label"
            padding="sm"
            class="transition-all duration-300 hover:shadow-md group"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl border transition-all duration-300 group-hover:scale-105"
                :class="cardToneClass(card.tone)"
              >
                <component :is="getBillingCardIcon(idx)" class="h-5 w-5" />
              </div>
              <p class="min-w-0 flex-1 text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)] truncate">
                {{ card.label }}
              </p>
            </div>
            <div class="mt-4">
              <p class="font-mono text-2xl sm:text-3xl font-bold tracking-tight tabular-nums text-[var(--text-primary)]">
                {{ formatCompactValue(card.value, card.type) }}
              </p>
              <p class="mt-1.5 text-xs text-[var(--text-secondary)] truncate">{{ card.caption }}</p>
            </div>
          </Card>
        </div>

        <Card class="transition-all duration-300">
          <template #header>
            <div class="flex flex-col lg:flex-row lg:items-center justify-between gap-4 w-full">
              <div>
                <div class="flex items-center gap-2.5">
                  <span class="chart-swatch" :class="barToneClass(currentBillingMetric.tone)"></span>
                  <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)]">
                    {{ t('admin.statistics.sections.metricTrend', { metric: t(currentBillingMetric.labelKey) }) }}
                  </h3>
                </div>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.statistics.sections.billingScope', {
                    range: billingPeriod === 'daily' ? t('admin.statistics.ranges.last30Days') : t('admin.statistics.ranges.last12Months')
                  }) }}
                </p>
              </div>

              <!-- 指标与周期选择器 -->
              <div class="flex flex-col sm:flex-row gap-3">
                <!-- 移动端 Select 选择器 -->
                <div class="flex sm:hidden items-center gap-2 w-full">
                  <div class="flex-1">
                    <Select
                      v-model="billingMetric"
                      :options="billingMetricOptions"
                      size="sm"
                    />
                  </div>
                  <div class="w-28">
                    <Select
                      v-model="billingPeriod"
                      :options="periodOptions"
                      size="sm"
                    />
                  </div>
                </div>

                <!-- 桌面端 Apple 胶囊药丸切换群 -->
                <div class="hidden sm:inline-flex flex-wrap p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] gap-1">
                  <button
                    v-for="metric in billingMetricItems"
                    :key="metric.value"
                    type="button"
                    class="px-3 py-1 text-xs font-medium rounded-full transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
                    :class="billingMetric === metric.value
                      ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
                      : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                    @click="billingMetric = metric.value"
                  >
                    {{ t(metric.labelKey) }}
                  </button>
                </div>

                <div class="hidden sm:inline-flex p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                  <button
                    v-for="period in periodItems"
                    :key="period.value"
                    type="button"
                    class="px-3.5 py-1 text-xs font-medium rounded-full transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
                    :class="billingPeriod === period.value
                      ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
                      : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                    @click="billingPeriod = period.value"
                  >
                    {{ t(period.labelKey) }}
                  </button>
                </div>
              </div>
            </div>
          </template>

          <div class="chart-area mt-4">
            <div v-for="(point, index) in billingSeries" :key="point.label" class="chart-column group">
              <div class="chart-value" :title="`${point.label}: ${formatValue(point.value, 'money')}`">
                <span class="chart-tooltip">{{ t('admin.statistics.tooltip', { label: point.label, value: formatCompactValue(point.value, 'money') }) }}</span>
                <div
                  :class="['chart-bar', barToneClass(currentBillingMetric.tone)]"
                  :style="{ height: `${barHeight(point, billingSeries)}%` }"
                ></div>
              </div>
              <span class="chart-tick">{{ shouldShowTick(index, billingSeries.length) ? formatTick(point.label) : '' }}</span>
            </div>
          </div>
        </Card>
      </section>
    </div>

    <!-- 空数据 / 错误状态 -->
    <Card v-else class="text-center py-16">
      <EmptyState
        :title="t('admin.statistics.noData')"
      >
        <template #action>
          <Button variant="primary" @click="loadStatistics">
            <template #icon>
              <RefreshCw class="w-4 h-4" />
            </template>
            {{ t('admin.statistics.reload') }}
          </Button>
        </template>
      </EmptyState>
    </Card>
  </div>
</template>

<style scoped>
.chart-area {
  display: grid;
  grid-auto-flow: column;
  grid-auto-columns: minmax(28px, 1fr);
  align-items: end;
  gap: 0.5rem;
  min-height: 18rem;
  overflow-x: auto;
  padding: 2.75rem 0.25rem 1.25rem;
  scrollbar-gutter: stable;
}

.chart-column {
  min-width: 28px;
}

.chart-value {
  position: relative;
  display: flex;
  align-items: end;
  justify-content: center;
  height: 15rem;
  border-bottom: 1px solid var(--border-color);
}

.chart-bar {
  width: min(72%, 1.6rem);
  min-height: 0;
  border-radius: 6px 6px 0 0;
  transform-origin: bottom;
  transition: height 0.35s cubic-bezier(0.25, 0.1, 0.25, 1), opacity 0.2s ease;
  animation: chart-grow 0.5s cubic-bezier(0.25, 0.1, 0.25, 1) both;
}

.chart-column:hover .chart-bar {
  opacity: 0.82;
}

@keyframes chart-grow {
  from {
    transform: scaleY(0);
    opacity: 0.5;
  }
  to {
    transform: scaleY(1);
    opacity: 1;
  }
}

.stat-split-seg {
  transform-origin: left;
  animation: stat-split-grow 0.6s cubic-bezier(0.25, 0.1, 0.25, 1) both;
}

@keyframes stat-split-grow {
  from {
    transform: scaleX(0);
  }
  to {
    transform: scaleX(1);
  }
}

.chart-bar-blue {
  background-color: var(--chart-1);
}

.chart-bar-emerald {
  background-color: var(--chart-2);
}

.chart-bar-amber {
  background-color: var(--chart-3);
}

.chart-bar-rose {
  background-color: var(--chart-4);
}

.chart-tooltip {
  position: absolute;
  top: -2.5rem;
  left: 50%;
  z-index: 10;
  transform: translateX(-50%);
  white-space: nowrap;
  border-radius: 0.5rem;
  padding: 0.35rem 0.6rem;
  font-size: 0.75rem;
  line-height: 1rem;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  opacity: 0;
  pointer-events: none;
  transition: opacity 0.15s ease, transform 0.15s ease;
  color: var(--text-primary);
  background-color: var(--bg-surface);
  border: 1px solid var(--border-color);
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.08);
  backdrop-filter: blur(8px);
}

.chart-column:hover .chart-tooltip {
  opacity: 1;
  transform: translateX(-50%) translateY(-2px);
}

.chart-tick {
  display: block;
  width: max-content;
  min-height: 1rem;
  margin: 0.6rem auto 0;
  text-align: center;
  font-size: 0.7rem;
  line-height: 1rem;
  white-space: nowrap;
  word-break: keep-all;
  overflow-wrap: normal;
  color: var(--text-tertiary);
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
}

.chart-swatch {
  display: inline-block;
  width: 0.625rem;
  height: 0.625rem;
  flex-shrink: 0;
  border-radius: 0.1875rem;
}

@media (prefers-reduced-motion: reduce) {
  *,
  ::before,
  ::after {
    animation-duration: 0.001ms !important;
    transition-duration: 0.001ms !important;
  }
}
</style>
