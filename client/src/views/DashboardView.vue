<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, onActivated, onDeactivated } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useConfigStore } from '@/stores/config'
import api, { type VipLevelProgress, type VipProgressCondition, type VipProgressMetric } from '@/api'
import Skeleton from '@/components/Skeleton.vue'
import { formatMemory, formatDisk, getStatusInfo } from '@/utils/formatters'
import type { Instance, UserBalance } from '@/types/api'
import DistroIcon from '@/components/icons/DistroIcon.vue'
import FlagIcon from '@/components/FlagIcon.vue'
import InstanceDisplayIcon from '@/components/InstanceDisplayIcon.vue'
import { getRandomFunnyQuote } from '@/data/funnyQuotes'
import { getVipBadgeInlineStyle, normalizeVipBadgeStyle, type VipBadgeStyle } from '@/utils/vipBadge'
import { instanceDetailPath, instancesPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'
import { PageHeader, Card, Badge, Button, EmptyState } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import {
  Server,
  Activity,
  Wallet,
  Coins,
  RefreshCw,
  ChevronRight,
  AlertTriangle,
  Send
} from 'lucide-vue-next'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'DashboardView' })

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 遮蔽 IPv4 地址后两位，例如：1.2.3.4 -> 1.2.*.*
function maskIpv4(ip: string | null | undefined): string | null {
  if (!ip) return null
  const parts = ip.split('.')
  if (parts.length === 4) {
    return `${parts[0]}.${parts[1]}.*.*`
  }
  return ip
}

function isIpv4Address(ip: string | null | undefined): boolean {
  if (!ip) return false
  return /^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$/.test(ip)
}

// 获取显示的IP地址：有NAT的模式显示NAT公网IP，纯IPv6模式显示IPv6，其他显示内网IPv4
function getDisplayIp(instance: Instance): string | null {
  const networkMode = instance.networkMode || instance.network_mode
  // 有 NAT 端口映射的模式：显示 NAT 公网 IP
  const hasNat = networkMode === 'nat' || networkMode === 'nat_ipv6' || networkMode === 'nat_ipv6_nat'
  if (hasNat) {
    const natPublicIp = instance.natPublicIp || instance.host?.nat_public_ip
    if (isIpv4Address(natPublicIp)) {
      return maskIpv4(natPublicIp)
    }
    if (networkMode === 'nat_ipv6' && instance.ipv6) {
      return instance.ipv6
    }
    return maskIpv4(instance.ipv4)
  }
  // 纯 IPv6 模式：优先显示独立 IPv6
  if (networkMode === 'ipv6_only' && instance.ipv6) {
    return instance.ipv6
  }
  return maskIpv4(instance.ipv4)
}

// 从镜像名称提取发行版标识（用于 DistroIcon 组件）
function getDistroFromName(name: string): string {
  const lowerName = (name || '').toLowerCase()
  const distros = ['almalinux', 'alma', 'alpine', 'arch', 'centos', 'debian', 'fedora', 'gentoo', 'kali', 'mint', 'opensuse', 'suse', 'oracle', 'redhat', 'rhel', 'rockylinux', 'rocky', 'ubuntu', 'void']
  for (const distro of distros) {
    if (lowerName.includes(distro)) return distro
  }
  return 'linux'
}

// 获取付费实例的图标类型（根据节点名称判断）
// 返回 'pro' | 'prime' | null
function getPaidIconType(instance: Instance): 'pro' | 'prime' | null {
  if (!instance.packagePlanId) return null
  return instance.instanceType === 'vm' ? 'prime' : 'pro'
}

// 格式化镜像名称：优先使用 imageName，否则去掉 images: 前缀
function formatImageName(image: string, imageName?: string | null): string {
  if (imageName) return imageName
  return image?.replace(/^images:/, '') || ''
}

function getInstanceTypeLabel(instance: Instance): string {
  const key = `instance.instanceType.${instance.instanceType === 'vm' ? 'vm' : 'container'}`
  const translated = t(key)
  return translated === key ? (instance.instanceType === 'vm' ? 'KVM' : 'LXC') : translated
}

function getHostName(instance: Instance): string {
  return instance.host?.name || t('dashboard.unknownHost')
}

function getStatusKey(instance: Instance): string {
  return instance.status?.toLowerCase() || 'unknown'
}

function getInstanceStatusBadge(status: string): { variant: BadgeVariant; pulse: boolean } {
  const s = status?.toLowerCase() || ''
  if (s === 'running') return { variant: 'success', pulse: false }
  if (s === 'stopped') return { variant: 'default', pulse: false }
  if (['starting', 'stopping', 'restarting', 'creating'].includes(s)) return { variant: 'warning', pulse: true }
  if (['suspended', 'error', 'failed'].includes(s)) return { variant: 'error', pulse: false }
  return { variant: 'default', pulse: false }
}

const { t, locale } = useI18n()
const authStore = useAuthStore()
const configStore = useConfigStore()
void configStore.loadPublicConfig()

const footerTelegramLink = computed(() => {
  const link = configStore.footerTelegramLink?.trim()
  return link || null
})

// 随机骚话（页面加载时固定，不随刷新变化）
const funnyQuote = ref('')

function getTimeGreeting(): string {
  const hour = new Date().getHours()
  if (hour < 12) return t('dashboard.greeting.morning')
  if (hour < 18) return t('dashboard.greeting.afternoon')
  return t('dashboard.greeting.evening')
}

const instances = ref<Instance[]>([])
const balanceOverview = ref<UserBalance>({
  balance: 0,
  frozen: 0,
  totalRecharge: 0,
  totalConsume: 0
})
const userVipLevel = ref(0)
const userVipBadgeStyle = ref<VipBadgeStyle | null>(null)
const userVipProgress = ref<VipLevelProgress | null>(null)
const userPoints = ref(0)
const loading = ref(true)
const loadError = ref('')

let refreshInterval: ReturnType<typeof setInterval> | null = null

onMounted(async () => {
  // 初始化随机骚话
  funnyQuote.value = getRandomFunnyQuote(locale.value)
  await loadData()
  refreshInterval = setInterval(loadData, 15000)
})

onUnmounted(() => {
  if (refreshInterval) clearInterval(refreshInterval)
})

// 当组件被 KeepAlive 停用时，暂停定时器
onDeactivated(() => {
  if (refreshInterval) {
    clearInterval(refreshInterval)
    refreshInterval = null
  }
})

// 当组件从 KeepAlive 缓存中激活时，恢复定时器并刷新数据
onActivated(async () => {
  if (!refreshInterval) {
    refreshInterval = setInterval(loadData, 15000)
  }
  await loadData()
})

async function loadData(): Promise<void> {
  try {
    if (!authStore.isAuthenticated) {
      if (refreshInterval) {
        clearInterval(refreshInterval)
        refreshInterval = null
      }
      return
    }

    const [instancesRes, meRes, balanceRes, vipRes, pointsRes] = await Promise.all([
      api.instances.list({ pageSize: 1000 }),
      api.auth.me(),
      api.billing.getUserBalance().catch(() => null),
      api.vipLevels.getMyOverview().catch(() => null),
      api.entertainment.getPoints().catch(() => null)
    ])
    const res = instancesRes as { instances?: Instance[] }
    instances.value = res.instances || []
    loadError.value = ''
    if ((meRes as { user?: any }).user) {
      authStore.user = (meRes as { user: any }).user
    }
    if (balanceRes?.balance) {
      balanceOverview.value = balanceRes.balance
    }
    userVipLevel.value = normalizeVipLevel(vipRes?.userVipLevel)
    userVipBadgeStyle.value = userVipLevel.value > 0
      ? normalizeVipBadgeStyle(vipRes?.userVipBadgeStyle, userVipLevel.value)
      : null
    userVipProgress.value = vipRes?.userVipProgress || null
    userPoints.value = Number(pointsRes?.points || 0)
  } catch (error: any) {
    console.error('Failed to load dashboard:', error)
    if (error?.response?.status === 401 || error?.code === 'UNAUTHORIZED') {
      if (refreshInterval) {
        clearInterval(refreshInterval)
        refreshInterval = null
      }
    } else {
      loadError.value = error?.message || t('common.loadFailed')
    }
  } finally {
    loading.value = false
  }
}

const stats = computed(() => {
  const all = instances.value
  return {
    total: all.length,
    running: all.filter(i => getStatusKey(i) === 'running').length,
    stopped: all.filter(i => getStatusKey(i) === 'stopped').length,
    creating: all.filter(i => getStatusKey(i) === 'creating').length
  }
})

const recentInstances = computed(() => instances.value.slice(0, 5))

const instanceTypeStats = computed(() => {
  const vm = instances.value.filter(instance => instance.instanceType === 'vm').length
  return {
    vm,
    container: Math.max(instances.value.length - vm, 0)
  }
})

const balanceSummaryItems = computed(() => [
  {
    label: t('dashboard.totalRecharge'),
    value: formatCurrency(balanceOverview.value.totalRecharge || 0)
  },
  {
    label: t('dashboard.totalConsume'),
    value: formatCurrency(balanceOverview.value.totalConsume || 0)
  },
  {
    label: t('dashboard.userPoints'),
    value: formatPoints(userPoints.value)
  }
])

const accountPanelHint = computed(() => t('dashboard.walletHint'))

const memberBadgeStyle = computed(() => userVipLevel.value > 0 ? getVipBadgeInlineStyle(userVipBadgeStyle.value) : undefined)

const memberBadgeClass = computed(() => userVipLevel.value > 0
  ? 'border border-[var(--border-color)] shadow-sm'
  : 'border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-secondary)]'
)

const instanceListSummary = computed(() => t('dashboard.instanceListSummary', {
  count: Math.min(recentInstances.value.length, instances.value.length),
  total: instances.value.length
}))

const vipProgressPercent = computed(() => {
  const progress = Number(userVipProgress.value?.progress || 0)
  return Math.max(0, Math.min(100, Math.round(progress)))
})

const vipProgressStyle = computed(() => ({ width: `${vipProgressPercent.value}%` }))

const vipProgressTargetText = computed(() => {
  const progress = userVipProgress.value
  if (!progress) return t('dashboard.vipProgressUnavailable')
  if (progress.nextLevel) {
    return t('dashboard.vipProgressToNext', {
      current: formatUserVipLevel(progress.currentLevel),
      next: formatUserVipLevel(progress.nextLevel)
    })
  }
  return progress.isMaxLevel ? t('dashboard.vipProgressMaxed') : t('dashboard.vipProgressNoRule')
})

const vipProgressHint = computed(() => {
  const progress = userVipProgress.value
  if (!progress?.nextLevel) return t('dashboard.vipProgressStableHint')
  if (progress.userMetric) {
    const metric = progress.conditions[0]?.metric
      ? getVipProgressMetricLabel(progress.conditions[0].metric)
      : t('dashboard.memberLevel')
    return t('dashboard.vipProgressSingleMetricHint', {
      level: formatUserVipLevel(progress.nextLevel),
      metric
    })
  }
  const key = progress.conditionMode === 'all' ? 'dashboard.vipProgressAllHint' : 'dashboard.vipProgressAnyHint'
  return t(key, { level: formatUserVipLevel(progress.nextLevel) })
})

const vipProgressConditions = computed(() => userVipProgress.value?.conditions || [])

const vipProgressConditionGridClass = computed(() => vipProgressConditions.value.length === 1
  ? 'grid-cols-1'
  : 'sm:grid-cols-2'
)

function formatCurrency(amount: number): string {
  return `¥${amount.toFixed(2)}`
}

function formatPoints(points: number): string {
  const normalized = Number.isFinite(points) ? Math.max(points, 0) : 0
  return Math.floor(normalized).toLocaleString()
}

function getVipProgressMetricLabel(metric: VipProgressMetric): string {
  const keyMap: Record<VipProgressMetric, string> = {
    totalRecharge: 'dashboard.vipMetricTotalRecharge',
    totalConsume: 'dashboard.vipMetricTotalConsume',
    instanceCount: 'dashboard.vipMetricInstanceCount'
  }
  return t(keyMap[metric])
}

function isVipCountMetric(metric: VipProgressMetric): boolean {
  return metric === 'instanceCount'
}

function formatVipProgressValue(condition: VipProgressCondition, value: number): string {
  return isVipCountMetric(condition.metric) ? String(Math.floor(value)) : formatCurrency(value)
}

function formatVipProgressRemaining(condition: VipProgressCondition): string {
  if (condition.matched) return t('dashboard.vipProgressConditionMet')
  if (isVipCountMetric(condition.metric)) {
    return String(Math.ceil(condition.remaining))
  }
  return formatCurrency(condition.remaining)
}

function normalizeVipLevel(level: unknown): number {
  const parsed = Number(level)
  return Number.isFinite(parsed) && parsed > 0 ? Math.floor(parsed) : 0
}

function formatUserVipLevel(level: unknown): string {
  const normalizedLevel = normalizeVipLevel(level)
  return normalizedLevel > 0 ? `VIP${normalizedLevel}` : t('dashboard.memberLevelBasic')
}

const greetingTitle = computed(() => {
  const username = authStore.user?.username || t('common.user')
  const level = normalizeVipLevel(userVipLevel.value)
  if (level <= 0) {
    return t('dashboard.greeting.basicUser', { username })
  }

  return t('dashboard.greeting.memberUser', {
    level: formatUserVipLevel(level),
    username
  })
})

const greetingText = computed(() => t('dashboard.greeting.full', {
  greeting: getTimeGreeting(),
  member: greetingTitle.value
}))
</script>

<template>
  <div ref="revealRoot" class="max-w-7xl mx-auto space-y-6 sm:space-y-8 animate-fade-in">
    <!-- 页头：问候语 + 状态行 + 操作栏 -->
    <PageHeader data-reveal>
      <template #title>
        <h1 class="text-2xl sm:text-3xl font-semibold tracking-tight text-[var(--text-primary)] truncate">
          {{ greetingText }}
        </h1>
      </template>
      <template #description>
        <div class="flex items-center gap-2 text-sm text-[var(--text-secondary)] mt-1.5">
          <span class="inline-block w-2 h-2 rounded-full bg-[var(--success)] shrink-0" />
          <span class="truncate">{{ funnyQuote }}</span>
        </div>
      </template>
      <template #actions>
        <!-- 刷新数据 -->
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadData()"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5" :class="{ 'animate-spin': loading }" />
          </template>
          <span>{{ t('common.refresh') }}</span>
        </Button>

        <!-- 我的实例快捷入口 -->
        <RouterLink :to="instancesPath()">
          <Button variant="secondary" size="sm">
            <template #icon>
              <Server class="w-3.5 h-3.5" />
            </template>
            <span>{{ t('dashboard.myInstances') }}</span>
          </Button>
        </RouterLink>

        <!-- Telegram 官方群组链接 -->
        <a
          v-if="footerTelegramLink"
          :href="footerTelegramLink"
          target="_blank"
          rel="noopener noreferrer"
          class="inline-flex items-center justify-center w-9 h-9 rounded-full bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          title="Telegram"
          aria-label="Telegram"
        >
          <Send class="w-4 h-4 stroke-[1.8]" />
        </a>
      </template>
    </PageHeader>

    <!-- 核心指标卡：总实例 / 运行中 / 账户余额 / 积分 -->
    <section data-reveal class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4 sm:gap-5">
      <!-- 总实例数 -->
      <Card hoverable padding="sm" class="relative group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('dashboard.totalInstances') }}
          </span>
          <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)] group-hover:scale-105 transition-transform duration-300">
            <Server class="w-5 h-5 stroke-[1.8]" />
          </div>
        </div>
        <div class="mt-3">
          <div class="text-3xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ stats.total }}
          </div>
          <div class="mt-2 flex items-center gap-1.5 text-xs text-[var(--text-secondary)]">
            <span>{{ t('dashboard.stoppedInstances') }}</span>
            <span class="font-mono font-medium text-[var(--text-primary)] tabular-nums">{{ stats.stopped }}</span>
          </div>
        </div>
      </Card>

      <!-- 运行中实例 -->
      <Card hoverable padding="sm" class="relative group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('dashboard.runningInstances') }}
          </span>
          <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--success)] group-hover:scale-105 transition-transform duration-300">
            <Activity class="w-5 h-5 stroke-[1.8]" />
          </div>
        </div>
        <div class="mt-3">
          <div class="text-3xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ stats.running }}
          </div>
          <div class="mt-2 flex items-center gap-2 text-xs">
            <Badge variant="success" size="sm" :dot="true">
              {{ stats.running }} / {{ stats.total }}
            </Badge>
            <span class="text-[var(--text-secondary)]">{{ t('dashboard.runningInstances') }}</span>
          </div>
        </div>
      </Card>

      <!-- 账户余额 -->
      <Card hoverable padding="sm" class="relative group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('dashboard.accountOverview') }}
          </span>
          <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)] group-hover:scale-105 transition-transform duration-300">
            <Wallet class="w-5 h-5 stroke-[1.8]" />
          </div>
        </div>
        <div class="mt-3">
          <div class="text-3xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ formatCurrency(balanceOverview.balance) }}
          </div>
          <div class="mt-2 flex items-center gap-1.5 text-xs text-[var(--text-secondary)]">
            <span>{{ t('dashboard.totalRecharge') }}</span>
            <span class="font-mono font-medium text-[var(--text-primary)] tabular-nums">{{ formatCurrency(balanceOverview.totalRecharge || 0) }}</span>
          </div>
        </div>
      </Card>

      <!-- 用户积分 / 会员等级 -->
      <Card hoverable padding="sm" class="relative group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('dashboard.userPoints') }}
          </span>
          <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)] group-hover:scale-105 transition-transform duration-300">
            <Coins class="w-5 h-5 stroke-[1.8]" />
          </div>
        </div>
        <div class="mt-3">
          <div class="text-3xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ formatPoints(userPoints) }}
          </div>
          <div class="mt-2 flex items-center gap-1.5 text-xs text-[var(--text-secondary)]">
            <span>{{ t('dashboard.memberLevel') }}</span>
            <span class="font-medium text-[var(--text-primary)]">{{ formatUserVipLevel(userVipLevel) }}</span>
          </div>
        </div>
      </Card>
    </section>

    <!-- 两栏：账户总览（含会员进度） + 实例状态分布 -->
    <section data-reveal class="grid gap-6 lg:grid-cols-3">
      <!-- 左：账户总览与 VIP 成长进度 -->
      <Card class="lg:col-span-2">
        <template #header>
          <div class="flex items-center justify-between w-full">
            <div>
              <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('dashboard.accountOverview') }}
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ accountPanelHint }}
              </p>
            </div>
            <div>
              <span
                v-if="userVipLevel > 0"
                class="inline-flex items-center rounded-full px-3 py-1 text-xs font-semibold"
                :class="memberBadgeClass"
                :style="memberBadgeStyle"
              >
                {{ formatUserVipLevel(userVipLevel) }}
              </span>
              <Badge v-else variant="default" size="sm">
                {{ formatUserVipLevel(userVipLevel) }}
              </Badge>
            </div>
          </div>
        </template>

        <div>
          <!-- 主余额展示 -->
          <div class="text-3xl sm:text-4xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ formatCurrency(balanceOverview.balance) }}
          </div>

          <!-- 充值/消费/积分概况 -->
          <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mt-6">
            <div
              v-for="item in balanceSummaryItems"
              :key="item.label"
              class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5"
            >
              <div class="text-xs text-[var(--text-secondary)]">{{ item.label }}</div>
              <div class="mt-1 text-sm sm:text-base font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums truncate">
                {{ item.value }}
              </div>
            </div>
          </div>

          <!-- VIP 成长进度面板 -->
          <div class="mt-6 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 sm:p-5">
            <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
              <div class="min-w-0">
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ t('dashboard.vipProgressTitle') }}</div>
                <div class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ vipProgressTargetText }}</div>
              </div>
              <Badge variant="info" size="sm">
                {{ vipProgressPercent }}%
              </Badge>
            </div>

            <!-- 进度条 -->
            <div class="mt-4 h-2 w-full rounded-full bg-[var(--bg-tertiary)] overflow-hidden">
              <div
                class="h-full rounded-full bg-[var(--accent)] transition-all duration-500 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                :style="vipProgressStyle"
              />
            </div>
            <div class="mt-2 text-xs text-[var(--text-secondary)]">{{ vipProgressHint }}</div>

            <!-- 各项条件达标网格 -->
            <div v-if="vipProgressConditions.length > 0" class="mt-4 grid gap-2.5" :class="vipProgressConditionGridClass">
              <div
                v-for="condition in vipProgressConditions"
                :key="condition.metric"
                class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3.5"
              >
                <div class="flex items-center justify-between gap-2 mb-2.5">
                  <span class="text-xs font-medium text-[var(--text-secondary)]">{{ getVipProgressMetricLabel(condition.metric) }}</span>
                  <Badge :variant="condition.matched ? 'success' : 'default'" size="sm">
                    {{ condition.progress }}%
                  </Badge>
                </div>
                <div class="grid grid-cols-3 gap-2">
                  <div class="rounded-lg bg-[var(--bg-surface-soft)] p-2">
                    <div class="text-[11px] text-[var(--text-secondary)]">{{ t('dashboard.vipProgressCurrent') }}</div>
                    <div class="mt-0.5 text-xs font-semibold text-[var(--text-primary)] font-mono tabular-nums truncate">
                      {{ formatVipProgressValue(condition, condition.current) }}
                    </div>
                  </div>
                  <div class="rounded-lg bg-[var(--bg-surface-soft)] p-2">
                    <div class="text-[11px] text-[var(--text-secondary)]">{{ t('dashboard.vipProgressTarget') }}</div>
                    <div class="mt-0.5 text-xs font-semibold text-[var(--text-primary)] font-mono tabular-nums truncate">
                      {{ formatVipProgressValue(condition, condition.target) }}
                    </div>
                  </div>
                  <div class="rounded-lg bg-[var(--bg-surface-soft)] p-2">
                    <div class="text-[11px] text-[var(--text-secondary)]">{{ t('dashboard.vipProgressRemaining') }}</div>
                    <div
                      class="mt-0.5 text-xs font-semibold font-mono tabular-nums truncate"
                      :class="condition.matched ? 'text-[var(--success)]' : 'text-[var(--accent)]'"
                    >
                      {{ formatVipProgressRemaining(condition) }}
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </Card>

      <!-- 右：实例状态与分布 -->
      <Card class="flex flex-col">
        <template #header>
          <div class="flex items-center justify-between w-full">
            <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('dashboard.instanceStatusOverview') }}
            </h3>
            <RouterLink :to="instancesPath()" class="text-xs font-medium text-[var(--accent)] hover:underline inline-flex items-center gap-0.5">
              <span>{{ t('dashboard.viewAll') }}</span>
              <ChevronRight class="w-3.5 h-3.5" />
            </RouterLink>
          </div>
        </template>

        <div>
          <!-- 实例总数 -->
          <div class="flex items-baseline gap-2 mb-6">
            <span class="text-3xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums leading-none">
              {{ stats.total }}
            </span>
            <span class="text-xs text-[var(--text-secondary)]">{{ t('dashboard.totalInstances') }}</span>
          </div>

          <!-- 各状态占比条 -->
          <div class="space-y-4">
            <!-- 运行中 -->
            <div>
              <div class="flex items-center justify-between text-xs">
                <span class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <span class="w-2 h-2 rounded-full bg-[var(--success)] shrink-0" />
                  <span>{{ t('dashboard.runningInstances') }}</span>
                </span>
                <span class="font-mono font-medium text-[var(--text-primary)] tabular-nums">{{ stats.running }}</span>
              </div>
              <div class="h-2 w-full rounded-full bg-[var(--bg-surface-soft)] overflow-hidden mt-2">
                <div
                  class="h-full rounded-full bg-[var(--success)] transition-all duration-500 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                  :style="{ width: (stats.total ? Math.round((stats.running / stats.total) * 100) : 0) + '%' }"
                />
              </div>
            </div>

            <!-- 已停止 -->
            <div>
              <div class="flex items-center justify-between text-xs">
                <span class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <span class="w-2 h-2 rounded-full bg-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('dashboard.stoppedInstances') }}</span>
                </span>
                <span class="font-mono font-medium text-[var(--text-primary)] tabular-nums">{{ stats.stopped }}</span>
              </div>
              <div class="h-2 w-full rounded-full bg-[var(--bg-surface-soft)] overflow-hidden mt-2">
                <div
                  class="h-full rounded-full bg-[var(--text-tertiary)] transition-all duration-500 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                  :style="{ width: (stats.total ? Math.round((stats.stopped / stats.total) * 100) : 0) + '%' }"
                />
              </div>
            </div>

            <!-- 容器 (LXC) -->
            <div>
              <div class="flex items-center justify-between text-xs">
                <span class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <span class="w-2 h-2 rounded-full bg-[var(--accent)] shrink-0" />
                  <span>{{ t('dashboard.containerInstances') }}</span>
                </span>
                <span class="font-mono font-medium text-[var(--text-primary)] tabular-nums">{{ instanceTypeStats.container }}</span>
              </div>
              <div class="h-2 w-full rounded-full bg-[var(--bg-surface-soft)] overflow-hidden mt-2">
                <div
                  class="h-full rounded-full bg-[var(--accent)] transition-all duration-500 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                  :style="{ width: (stats.total ? Math.round((instanceTypeStats.container / stats.total) * 100) : 0) + '%' }"
                />
              </div>
            </div>

            <!-- 虚拟机 (KVM) -->
            <div>
              <div class="flex items-center justify-between text-xs">
                <span class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <span class="w-2 h-2 rounded-full bg-[var(--border-strong)] shrink-0" />
                  <span>{{ t('dashboard.vmInstances') }}</span>
                </span>
                <span class="font-mono font-medium text-[var(--text-primary)] tabular-nums">{{ instanceTypeStats.vm }}</span>
              </div>
              <div class="h-2 w-full rounded-full bg-[var(--bg-surface-soft)] overflow-hidden mt-2">
                <div
                  class="h-full rounded-full bg-[var(--border-strong)] transition-all duration-500 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                  :style="{ width: (stats.total ? Math.round((instanceTypeStats.vm / stats.total) * 100) : 0) + '%' }"
                />
              </div>
            </div>
          </div>
        </div>
      </Card>
    </section>

    <!-- 最近实例列表卡片 -->
    <Card data-reveal padding="none" class="overflow-hidden">
      <template #header>
        <div class="flex items-center justify-between w-full">
          <div>
            <h2 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('dashboard.myInstances') }}
            </h2>
            <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ instanceListSummary }}</p>
          </div>
          <RouterLink :to="instancesPath()">
            <Button variant="secondary" size="sm">
              <span>{{ t('dashboard.viewAll') }}</span>
              <template #suffix>
                <ChevronRight class="w-3.5 h-3.5" />
              </template>
            </Button>
          </RouterLink>
        </div>
      </template>

      <!-- 加载中骨架屏 -->
      <div v-if="loading" class="p-6 space-y-4">
        <div
          v-for="i in 3"
          :key="i"
          class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4"
        >
          <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <div class="flex items-center gap-3">
              <Skeleton type="circle" size="44px" />
              <div class="min-w-0 flex-1 space-y-2">
                <Skeleton type="line" width="150px" height="15px" />
                <Skeleton type="line" width="210px" height="11px" />
              </div>
            </div>
            <div class="flex flex-wrap gap-2 lg:justify-end">
              <Skeleton v-for="metric in 3" :key="metric" type="line" width="74px" height="28px" />
            </div>
          </div>
        </div>
      </div>

      <!-- 暂无实例空状态 -->
      <div v-else-if="instances.length === 0 && !loadError" class="py-12 px-6">
        <EmptyState
          :title="t('dashboard.noInstances')"
        >
          <template #icon>
            <Server class="w-7 h-7 stroke-[1.5]" />
          </template>
        </EmptyState>
      </div>

      <!-- 加载错误状态 -->
      <div v-else-if="loadError" class="py-12 px-6 text-center">
        <div class="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--error)]">
          <AlertTriangle class="h-7 w-7 stroke-[1.5]" />
        </div>
        <h3 class="mb-1 text-base font-semibold text-[var(--text-primary)]">{{ t('common.loadFailed') }}</h3>
        <p class="mb-5 text-sm text-[var(--text-secondary)] max-w-sm mx-auto break-words">{{ loadError }}</p>
        <Button variant="primary" size="sm" @click="loadData()">{{ t('common.retry') }}</Button>
      </div>

      <!-- 实例行 -->
      <div v-else class="divide-y divide-[var(--border-color)]">
        <RouterLink
          v-for="instance in recentInstances"
          :key="instance.id"
          :to="instanceDetailPath(instance.id)"
          class="group flex flex-col lg:flex-row lg:items-center lg:justify-between gap-4 p-4 sm:p-5 hover:bg-[var(--bg-surface-soft)] transition-colors duration-200"
        >
          <div class="flex items-start gap-3.5 min-w-0 flex-1">
            <div class="w-11 h-11 sm:w-12 sm:h-12 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center shrink-0 overflow-hidden group-hover:scale-105 transition-transform duration-300">
              <InstanceDisplayIcon
                v-if="instance.iconBadgeId || getPaidIconType(instance)"
                :badge-id="instance.iconBadgeId"
                :fallback-icon="getPaidIconType(instance)"
                :alt="instance.name"
                :size="48"
              />
              <DistroIcon
                v-else
                :distro="getDistroFromName(instance.image)"
                :size="26"
              />
            </div>

            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-semibold text-sm sm:text-base text-[var(--text-primary)] truncate max-w-full group-hover:text-[var(--accent)] transition-colors duration-200">
                  {{ instance.name }}
                </span>
                <Badge
                  :variant="getInstanceStatusBadge(instance.status).variant"
                  :dot="true"
                  :pulse="getInstanceStatusBadge(instance.status).pulse"
                  size="sm"
                >
                  {{ getStatusInfo(instance.status, t).label }}
                </Badge>
                <Badge variant="default" size="sm" shape="rounded">
                  {{ getInstanceTypeLabel(instance) }}
                </Badge>
              </div>

              <div class="mt-1.5 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-xs text-[var(--text-secondary)]">
                <span class="font-mono truncate max-w-[14rem] sm:max-w-[20rem]">{{ formatImageName(instance.image, (instance as any).imageName) }}</span>
                <span class="text-[var(--text-tertiary)]">•</span>
                <span class="font-mono text-[var(--text-primary)]">CPU {{ instance.cpu }}%</span>
                <span class="text-[var(--text-tertiary)]">•</span>
                <span class="font-mono text-[var(--text-primary)]">{{ formatMemory(instance.memory) }}</span>
                <span class="text-[var(--text-tertiary)]">•</span>
                <span class="font-mono text-[var(--text-primary)]">{{ formatDisk(instance.disk) }}</span>
              </div>

              <!-- CPU load bar -->
              <div class="mt-2.5 h-1.5 w-full max-w-xs rounded-full bg-[var(--bg-tertiary)] overflow-hidden">
                <div
                  class="h-full rounded-full transition-all duration-300"
                  :class="{
                    'bg-[var(--error)]': instance.cpu > 85,
                    'bg-[var(--warning)]': instance.cpu > 60 && instance.cpu <= 85,
                    'bg-[var(--accent)]': instance.cpu <= 60
                  }"
                  :style="{ width: Math.min(Math.max(instance.cpu, 0), 100) + '%' }"
                />
              </div>
            </div>
          </div>

          <!-- 右侧节点信息 & IP & 箭头 -->
          <div class="flex flex-wrap items-center gap-2.5 lg:justify-end lg:pl-4">
            <span class="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-xs font-medium bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)]">
              <FlagIcon :code="instance.host?.country_code || instance.hostCountryCode || 'us'" size="xs" />
              <span class="max-w-[8rem] truncate">{{ getHostName(instance) }}</span>
            </span>
            <span class="inline-flex items-center px-2.5 py-1 rounded-lg text-xs font-mono bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)]">
              {{ getDisplayIp(instance) || t('dashboard.noPublicIp') }}
            </span>
            <div class="hidden lg:flex w-8 h-8 rounded-full items-center justify-center text-[var(--text-tertiary)] group-hover:text-[var(--accent)] group-hover:translate-x-0.5 transition-all duration-200">
              <ChevronRight class="w-4 h-4" />
            </div>
          </div>
        </RouterLink>
      </div>

      <!-- 查看全部底栏 -->
      <template v-if="instances.length > 5" #footer>
        <div class="text-center py-1">
          <RouterLink :to="instancesPath()" class="text-sm font-medium text-[var(--accent)] hover:underline inline-flex items-center gap-1">
            <span>{{ t('dashboard.viewAllInstancesWithCount', { count: instances.length }) }}</span>
            <ChevronRight class="w-4 h-4" />
          </RouterLink>
        </div>
      </template>
    </Card>
  </div>
</template>
