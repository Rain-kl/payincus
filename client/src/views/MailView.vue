<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import api from '@/api'
import { useToast } from '@/stores/toast'
import { useConfigStore } from '@/stores/config'
import { translateError } from '@/utils/errorHandler'
import FlagIcon from '@/components/FlagIcon.vue'
import { freeSiteCopy } from '@/utils/freeSiteFun'
import { mailDomainPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'
import {
  PageHeader,
  Card,
  Button,
  Input,
  Select,
  Badge,
  Table,
  EmptyState
} from '@/components/ui'
import type { TableColumn, SelectOption, BadgeVariant } from '@/components/ui'
import {
  Mail,
  ShoppingCart,
  Clock,
  Globe,
  HardDrive,
  Users,
  Zap,
  Sparkles,
  Shield,
  Plus,
  RefreshCw,
  Check,
  CheckCircle2,
  AlertCircle,
  ChevronRight,
  Info,
  HelpCircle,
  X,
  Loader2
} from 'lucide-vue-next'

const { t } = useI18n()
const router = useRouter()
const toast = useToast()
const configStore = useConfigStore()
void configStore.loadPublicConfig()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// TAB 状态
const activeTab = ref<'my' | 'buy'>('my')

// 加载状态
const loading = ref(true)
const buyLoading = ref(false)

// 订阅数据
const subscription = ref<any>(null)

// 购买相关
const sources = ref<any[]>([])
const selectedSourceId = ref<number | null>(null)
const selectedPlanId = ref<number | null>(null)

// 优惠码相关
const affCode = ref('')
const affCodeValid = ref<boolean | null>(null)
const affCodeDiscount = ref(0)
const affCodeCommissionRate = ref(0)
const affCodeError = ref('')
const affCodeVerifying = ref(false)

// 计算属性
const selectedSource = computed(() => sources.value.find(s => s.id === selectedSourceId.value))
const selectedPlan = computed(() => selectedSource.value?.plans.find((p: any) => p.id === selectedPlanId.value))
const checkoutPriceInfo = computed(() => {
  if (!selectedPlan.value) {
    return null
  }

  const originalPrice = Number(selectedPlan.value.price)
  const discountAmount = affCodeValid.value
    ? Number((originalPrice * affCodeDiscount.value).toFixed(2))
    : 0
  const finalPrice = Number((originalPrice - discountAmount).toFixed(2))

  return {
    originalPrice,
    discountAmount,
    finalPrice
  }
})

// 检查选中地区是否已有订阅
const hasSubscriptionInSelectedRegion = computed(() => {
  if (!subscription.value || !selectedSourceId.value) return false
  return subscription.value.source.id === selectedSourceId.value
})

// 域名列表表格配置
const domainColumns = computed<TableColumn[]>(() => [
  { key: 'domain', title: t('mail.domainName'), width: '50%' },
  { key: 'status', title: t('common.status'), width: '28%' },
  { key: 'actions', title: t('common.actions'), width: '22%', align: 'right' }
])

// 域名添加弹窗
const showAddDomainModal = ref(false)
const addDomainLoading = ref(false)
const newDomain = ref('')

// 续费弹窗
const showRenewModal = ref(false)
const renewLoading = ref(false)
const renewMonths = ref(1)
const autoRenewLoading = ref(false)

const renewOptions = computed<SelectOption[]>(() => [
  { label: `1 ${t('mail.month')}`, value: 1 },
  { label: `3 ${t('mail.months')}`, value: 3 },
  { label: `6 ${t('mail.months')}`, value: 6 },
  { label: `12 ${t('mail.months')}`, value: 12 }
])

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [subRes, srcRes] = await Promise.all([
      api.mail.getSubscription(),
      api.mail.getSources()
    ])
    subscription.value = subRes.subscription
    sources.value = srcRes.sources

    // 默认选择第一个源和方案
    if (sources.value.length > 0 && !selectedSourceId.value) {
      selectedSourceId.value = sources.value[0].id
      if (sources.value[0].plans.length > 0) {
        selectedPlanId.value = sources.value[0].plans[0].id
      }
    }

    // 如果没有订阅，自动切换到购买 TAB
    if (!subscription.value) {
      activeTab.value = 'buy'
    }
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    loading.value = false
  }
}

function selectSource(id: number) {
  selectedSourceId.value = id
  const source = sources.value.find(s => s.id === id)
  if (source?.plans.length > 0) {
    selectedPlanId.value = source.plans[0].id
  } else {
    selectedPlanId.value = null
  }
}

function selectPlan(id: number) {
  selectedPlanId.value = id
  // 重置优惠码状态
  resetAffCode()
}

function resetAffCode() {
  affCode.value = ''
  affCodeValid.value = null
  affCodeDiscount.value = 0
  affCodeCommissionRate.value = 0
  affCodeError.value = ''
}

async function verifyAffCode() {
  if (!affCode.value.trim()) {
    resetAffCode()
    return
  }

  affCodeVerifying.value = true
  affCodeError.value = ''

  try {
    const res = await api.mail.validateAffCode(affCode.value.trim())
    if (res.valid) {
      affCodeValid.value = true
      affCodeDiscount.value = res.discountRate || 0
      affCodeCommissionRate.value = res.commissionRate || 0
    } else {
      affCodeValid.value = false
      affCodeError.value = res.error || t('aff.promoCodeInvalid')
    }
  } catch (err: any) {
    affCodeValid.value = false
    affCodeError.value = err.message || t('aff.promoCodeInvalid')
  } finally {
    affCodeVerifying.value = false
  }
}

async function purchaseSubscription() {
  if (!selectedPlanId.value) {
    toast.error(t('mail.selectPlanFirst'))
    return
  }

  buyLoading.value = true
  try {
    await api.mail.purchaseSubscription(
      selectedPlanId.value,
      affCodeValid.value ? affCode.value.trim() : undefined
    )
    toast.success(t('mail.purchaseSuccess'))
    resetAffCode()
    await loadData()
    activeTab.value = 'my'
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    buyLoading.value = false
  }
}

// 添加域名
function openAddDomainModal() {
  newDomain.value = ''
  showAddDomainModal.value = true
}

async function addDomain() {
  if (!newDomain.value.trim()) {
    toast.error(t('mail.domainRequired'))
    return
  }

  addDomainLoading.value = true
  try {
    await api.mail.addDomain(newDomain.value.trim())
    toast.success(t('mail.domainAdded'))
    showAddDomainModal.value = false
    await loadData()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    addDomainLoading.value = false
  }
}

function goToDomain(id: number) {
  router.push(mailDomainPath(id))
}

// 续费相关
function openRenewModal() {
  // 根据计费周期设置默认值
  if (subscription.value?.plan?.billingCycle === 'yearly') {
    renewMonths.value = 12
  } else {
    renewMonths.value = 1
  }
  showRenewModal.value = true
}

const renewMonthlyPrice = computed(() => {
  if (!subscription.value?.plan) return 0
  const plan = subscription.value.plan
  return plan.billingCycle === 'monthly'
    ? Number(plan.price)
    : Number(plan.price) / 12
})

const renewOriginalPrice = computed(() => {
  return Math.round(renewMonthlyPrice.value * Number(renewMonths.value) * 100) / 100
})

const renewDiscountRate = computed(() => {
  return subscription.value?.affBinding?.affCode?.discountRate || 0
})

const renewDiscountAmount = computed(() => {
  return Math.round(renewOriginalPrice.value * renewDiscountRate.value * 100) / 100
})

const renewFinalPrice = computed(() => {
  return renewOriginalPrice.value - renewDiscountAmount.value
})

async function renewSubscription() {
  renewLoading.value = true
  try {
    await api.mail.renewSubscription(Number(renewMonths.value))
    toast.success(t('mail.renewSuccess'))
    showRenewModal.value = false
    await loadData()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    renewLoading.value = false
  }
}

async function toggleAutoRenew() {
  if (!subscription.value || autoRenewLoading.value) return

  const nextValue = !subscription.value.autoRenew
  autoRenewLoading.value = true
  try {
    const result = await api.mail.setAutoRenew(nextValue)
    subscription.value.autoRenew = result.autoRenew
    toast.success(t(result.autoRenew ? 'mail.autoRenewEnabled' : 'mail.autoRenewDisabled'))
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    autoRenewLoading.value = false
  }
}

function getStatusBadgeVariant(status: string): BadgeVariant {
  switch (status) {
    case 'verified':
    case 'active':
      return 'success'
    case 'pending':
      return 'warning'
    case 'suspended':
    case 'expired':
    case 'cancelled':
      return 'error'
    default:
      return 'default'
  }
}

function formatDate(dateStr: string) {
  return new Date(dateStr).toLocaleDateString()
}

function formatPrice(price: number | string | null | undefined) {
  if (configStore.freeSiteMode) return freeSiteCopy.mailPrice
  const num = Number(price) || 0
  return `¥${num.toFixed(2)}`
}

function getBillingCycleLabel(cycle: string | null | undefined) {
  if (configStore.freeSiteMode) return freeSiteCopy.mailBillingCycle
  return cycle === 'monthly' ? t('mail.monthly') : t('mail.yearly')
}

function getBillingCycleSuffix(cycle: string | null | undefined) {
  if (configStore.freeSiteMode) return ''
  return `/${cycle === 'monthly' ? t('mail.month') : t('mail.year')}`
}
</script>

<template>
  <div ref="revealRoot" class="kawaii-page animate-fade-in space-y-6">
    <!-- 页面标题 -->
    <PageHeader
      :title="t('mail.title')"
      :description="t('mail.description')"
    />

    <!-- Apple 风格分段导航 (Segmented Tabs) -->
    <div
      data-reveal
      class="flex items-center overflow-x-auto p-1.5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] w-fit max-w-full shadow-[0_2px_8px_rgba(0,0,0,0.02)]"
    >
      <div class="flex items-center gap-1 shrink-0" role="tablist" :aria-label="t('mail.title')">
        <button
          type="button"
          role="tab"
          :aria-selected="activeTab === 'my'"
          class="whitespace-nowrap rounded-xl px-4 py-2 text-sm font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="activeTab === 'my'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
          @click="activeTab = 'my'"
        >
          {{ t('mail.tabs.my') }}
        </button>
        <button
          type="button"
          role="tab"
          :aria-selected="activeTab === 'buy'"
          class="whitespace-nowrap rounded-xl px-4 py-2 text-sm font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="activeTab === 'buy'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
          @click="activeTab = 'buy'"
        >
          {{ t('mail.tabs.buy') }}
        </button>
      </div>
    </div>

    <!-- 加载状态 -->
    <Card v-if="loading" padding="lg" class="text-center">
      <div class="flex flex-col items-center justify-center py-12">
        <Loader2 class="w-8 h-8 animate-spin text-[var(--accent)]" />
        <p class="text-sm text-[var(--text-secondary)] mt-4 font-medium">{{ t('common.loading') }}</p>
      </div>
    </Card>

    <!-- 我的域名邮箱 TAB -->
    <div v-else-if="activeTab === 'my'" class="space-y-6">
      <!-- 无订阅状态 -->
      <Card v-if="!subscription" padding="none">
        <EmptyState
          :title="t('mail.noSubscription')"
          :description="t('mail.buyNowHint')"
        >
          <template #icon>
            <Mail class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template #action>
            <Button variant="primary" @click="activeTab = 'buy'">
              <template #icon>
                <ShoppingCart class="w-4 h-4 stroke-[2]" />
              </template>
              <span>{{ t('mail.buyNow') }}</span>
            </Button>
          </template>
        </EmptyState>
      </Card>

      <!-- 有订阅状态 -->
      <template v-else>
        <!-- 订阅概览卡片 -->
        <Card data-reveal>
          <template #header>
            <div class="flex items-center justify-between w-full">
              <h3 class="text-base md:text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('mail.subscriptionOverview') }}
              </h3>
              <Badge :variant="getStatusBadgeVariant(subscription.status)" dot>
                {{ t('mail.status.' + subscription.status) }}
              </Badge>
            </div>
          </template>

          <!-- 指标卡片网格 -->
          <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mb-6">
            <div class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
              <div class="text-xs text-[var(--text-secondary)] mb-1 flex items-center gap-1.5">
                <Clock class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span>{{ t('mail.expiresAt') }}</span>
              </div>
              <div class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ formatDate(subscription.expiresAt) }}
              </div>
            </div>
            <div class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
              <div class="text-xs text-[var(--text-secondary)] mb-1 flex items-center gap-1.5">
                <Globe class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span>{{ t('mail.domainsUsed') }}</span>
              </div>
              <div class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ subscription.usage.domainCount }} / {{ subscription.plan.domainLimit }}
              </div>
            </div>
            <div class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
              <div class="text-xs text-[var(--text-secondary)] mb-1 flex items-center gap-1.5">
                <HardDrive class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span>{{ t('mail.totalSpace') }}</span>
              </div>
              <div class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ subscription.plan.diskLimitGb }} GB
              </div>
            </div>
          </div>

          <!-- 方案信息与自动续费行 -->
          <div class="pt-4 border-t border-[var(--border-color)] flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
            <div class="min-w-0">
              <div class="text-sm text-[var(--text-secondary)] truncate">
                <span class="font-medium text-[var(--text-primary)]">{{ t('mail.plan') }}</span>：{{ subscription.plan.name }} ({{ subscription.source.name }}) ·
                {{ subscription.plan.domainLimit }} {{ t('mail.domains') }} / {{ subscription.plan.diskLimitGb }}GB ·
                <span class="font-semibold text-[var(--text-primary)]">{{ formatPrice(subscription.plan.price) }}{{ getBillingCycleSuffix(subscription.plan.billingCycle) }}</span>
              </div>
              <div class="mt-1 text-xs text-[var(--text-tertiary)]">{{ t('mail.autoRenewHint') }}</div>
            </div>
            <div class="flex shrink-0 items-center gap-3">
              <button
                type="button"
                role="switch"
                :aria-checked="subscription.autoRenew"
                :aria-label="t('mail.autoRenew')"
                :disabled="autoRenewLoading"
                :class="[
                  'relative h-6 w-11 rounded-full transition-colors duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] disabled:cursor-not-allowed disabled:opacity-60',
                  subscription.autoRenew ? 'bg-[var(--success)]' : 'bg-[var(--bg-tertiary)] border border-[var(--border-color)]'
                ]"
                @click="toggleAutoRenew"
              >
                <span
                  :class="[
                    'absolute top-0.5 h-5 w-5 rounded-full bg-white shadow-sm transition-transform duration-200',
                    subscription.autoRenew ? 'translate-x-5' : 'translate-x-0.5'
                  ]"
                />
              </button>
              <span class="text-xs font-medium text-[var(--text-secondary)]">
                {{ t(subscription.autoRenew ? 'mail.autoRenewOn' : 'mail.autoRenewOff') }}
              </span>
              <Button variant="secondary" size="sm" @click="openRenewModal">
                <template #icon>
                  <RefreshCw class="w-3.5 h-3.5" />
                </template>
                <span>{{ t('mail.renew') }}</span>
              </Button>
            </div>
          </div>
        </Card>

        <!-- 我的域名列表 -->
        <div data-reveal class="space-y-3 pt-2">
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2">
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('mail.myDomains') }}</h3>
              <Badge v-if="subscription.domains.length > 0" size="sm" variant="default" shape="pill">
                {{ subscription.domains.length }}
              </Badge>
            </div>
            <Button
              variant="primary"
              size="sm"
              :disabled="subscription.usage.domainCount >= subscription.plan.domainLimit"
              @click="openAddDomainModal"
            >
              <template #icon>
                <Plus class="w-3.5 h-3.5" />
              </template>
              <span>{{ t('mail.addDomain') }}</span>
            </Button>
          </div>

          <Table
            :columns="domainColumns"
            :data="subscription.domains"
            row-key="id"
            :empty-text="t('mail.noDomains')"
            empty-description=""
          >
            <template #empty>
              <EmptyState :title="t('mail.noDomains')" description="">
                <template #icon>
                  <Globe class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
                </template>
                <template #action>
                  <Button
                    variant="primary"
                    size="sm"
                    :disabled="subscription.usage.domainCount >= subscription.plan.domainLimit"
                    @click="openAddDomainModal"
                  >
                    <template #icon>
                      <Plus class="w-3.5 h-3.5" />
                    </template>
                    <span>{{ t('mail.addDomain') }}</span>
                  </Button>
                </template>
              </EmptyState>
            </template>
            <template #cell-domain="{ row }">
              <div class="flex items-center gap-2.5 font-medium text-[var(--text-primary)]">
                <div class="w-8 h-8 rounded-lg bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center shrink-0 text-[var(--text-secondary)]">
                  <Globe class="w-4 h-4 stroke-[1.5]" />
                </div>
                <span class="truncate font-semibold">{{ row.domain }}</span>
              </div>
            </template>
            <template #cell-status="{ row }">
              <Badge :variant="getStatusBadgeVariant(row.status)" size="sm" dot>
                {{ t('mail.domainStatus.' + row.status) }}
              </Badge>
            </template>
            <template #cell-actions="{ row }">
              <Button variant="ghost" size="sm" @click="goToDomain(row.id)">
                <span>{{ t('common.detail') }}</span>
                <template #suffix>
                  <ChevronRight class="w-3.5 h-3.5" />
                </template>
              </Button>
            </template>
            <template #mobile="{ row }">
              <div
                class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.02)] hover:border-[var(--border-hover)] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] cursor-pointer active:scale-[0.99] flex items-center justify-between gap-3"
                @click="goToDomain(row.id)"
              >
                <div class="flex items-center gap-3 min-w-0">
                  <div class="w-9 h-9 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center shrink-0 text-[var(--text-secondary)]">
                    <Globe class="w-4 h-4 stroke-[1.5]" />
                  </div>
                  <div class="min-w-0">
                    <div class="font-semibold text-sm text-[var(--text-primary)] truncate">{{ row.domain }}</div>
                    <div class="mt-1">
                      <Badge :variant="getStatusBadgeVariant(row.status)" size="sm" dot>
                        {{ t('mail.domainStatus.' + row.status) }}
                      </Badge>
                    </div>
                  </div>
                </div>
                <ChevronRight class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
              </div>
            </template>
          </Table>
        </div>
      </template>
    </div>

    <!-- 购买域名邮箱 TAB -->
    <div v-else-if="activeTab === 'buy'" class="lg:flex lg:gap-8">
      <!-- 左侧主内容区域 -->
      <div class="flex-1 min-w-0 space-y-6">
        <!-- Step 1: 选择服务地区 -->
        <Card data-reveal>
          <template #header>
            <div class="flex items-center gap-2.5">
              <span class="w-6 h-6 rounded-full bg-[var(--accent)] text-white text-xs font-semibold flex items-center justify-center shrink-0">1</span>
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('mail.selectRegion') }}</h3>
            </div>
          </template>

          <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div
              v-for="source in sources"
              :key="source.id"
              :class="[
                'group relative p-4 rounded-2xl border transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] cursor-pointer active:scale-[0.98]',
                selectedSourceId === source.id
                  ? 'border-[var(--accent)] ring-2 ring-[var(--accent)]/20 bg-[var(--bg-surface)] shadow-[0_4px_16px_rgba(0,0,0,0.06)]'
                  : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)]'
              ]"
              @click="selectSource(source.id)"
            >
              <div class="flex items-center gap-3">
                <!-- 国旗 -->
                <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center overflow-hidden shrink-0">
                  <FlagIcon :code="source.code" size="lg" />
                </div>
                <div class="min-w-0 flex-1">
                  <div class="font-semibold text-sm md:text-base text-[var(--text-primary)] truncate">{{ source.name }}</div>
                  <div class="text-xs text-[var(--warning)] font-medium mt-0.5 flex items-center gap-1">
                    <span class="w-1.5 h-1.5 rounded-full bg-[var(--warning)] shrink-0" />
                    <span>{{ t('mail.nodeStatus.limited') }}</span>
                  </div>
                </div>
                <!-- 选中指示器 -->
                <div
                  :class="[
                    'w-5 h-5 rounded-full border flex items-center justify-center transition-all duration-200 shrink-0',
                    selectedSourceId === source.id
                      ? 'border-[var(--accent)] bg-[var(--accent)] text-white'
                      : 'border-[var(--border-color)] group-hover:border-[var(--border-hover)] bg-[var(--bg-surface-soft)]'
                  ]"
                >
                  <Check v-if="selectedSourceId === source.id" class="w-3 h-3 stroke-[3]" />
                </div>
              </div>
            </div>
          </div>
        </Card>

        <!-- 已购买该地区提示 -->
        <Card
          v-if="hasSubscriptionInSelectedRegion"
          data-reveal
          class="border-[var(--warning)] bg-[color-mix(in_srgb,var(--warning)_8%,var(--bg-surface))]"
          padding="sm"
        >
          <div class="flex items-start gap-3">
            <AlertCircle class="w-5 h-5 text-[var(--warning)] shrink-0 mt-0.5" />
            <div class="min-w-0 flex-1">
              <h4 class="font-semibold text-sm text-[var(--text-primary)]">{{ t('mail.alreadyPurchased') }}</h4>
              <p class="text-xs md:text-sm text-[var(--text-secondary)] mt-1 leading-relaxed">{{ t('mail.alreadyPurchasedDesc') }}</p>
              <Button
                variant="secondary"
                size="sm"
                class="mt-3"
                @click="activeTab = 'my'"
              >
                {{ t('mail.viewMySubscription') }}
              </Button>
            </div>
          </div>
        </Card>

        <!-- Step 2: 方案配置详情 -->
        <Card v-if="selectedSource && !hasSubscriptionInSelectedRegion" data-reveal>
          <template #header>
            <div class="flex items-center gap-2.5">
              <span class="w-6 h-6 rounded-full bg-[var(--accent)] text-white text-xs font-semibold flex items-center justify-center shrink-0">2</span>
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('mail.planDetails') }}</h3>
            </div>
          </template>

          <div class="space-y-4">
            <div
              v-for="plan in selectedSource.plans"
              :key="plan.id"
              :class="[
                'relative p-5 rounded-2xl border transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] cursor-pointer active:scale-[0.99]',
                selectedPlanId === plan.id
                  ? 'border-[var(--accent)] ring-2 ring-[var(--accent)]/20 bg-[var(--bg-surface)] shadow-[0_4px_16px_rgba(0,0,0,0.06)]'
                  : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)]'
              ]"
              @click="selectPlan(plan.id)"
            >
              <!-- 方案标题和价格 -->
              <div class="flex items-start justify-between gap-4 mb-4">
                <div class="flex-1 min-w-0">
                  <div class="flex items-center gap-2">
                    <h4 class="text-base md:text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">{{ plan.name }}</h4>
                    <Badge v-if="selectedPlanId === plan.id" variant="info" size="sm">已选</Badge>
                  </div>
                  <!-- 方案描述（从后端获取）-->
                  <p v-if="plan.description" class="text-xs md:text-sm text-[var(--text-secondary)] mt-1 leading-relaxed">{{ plan.description }}</p>
                </div>
                <div class="text-right shrink-0">
                  <div class="text-2xl font-bold tracking-tight text-[var(--text-primary)]">
                    {{ formatPrice(plan.price) }}
                    <span class="text-xs font-normal text-[var(--text-secondary)]">{{ getBillingCycleSuffix(plan.billingCycle) }}</span>
                  </div>
                </div>
              </div>

              <!-- 配额信息 + 功能特性 -->
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 pt-4 border-t border-[var(--border-color)] text-xs md:text-sm">
                <div class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <HardDrive class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('mail.feature.domainStorage', { count: plan.domainLimit, storage: plan.diskLimitGb }) }}</span>
                </div>
                <div class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <Mail class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('mail.feature.unlimitedAliases') }}</span>
                </div>
                <div class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <Users class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('mail.feature.unlimitedMailboxes') }}</span>
                </div>
                <div class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <Zap class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('mail.feature.emailLimit') }}</span>
                </div>
                <div class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <Sparkles class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('mail.feature.emClientPro') }}</span>
                </div>
                <div class="flex items-center gap-2 text-[var(--text-secondary)]">
                  <Shield class="w-4 h-4 text-[var(--text-tertiary)] shrink-0" />
                  <span>{{ t('mail.feature.catchAll') }}</span>
                </div>
              </div>

              <!-- 选中指示器 -->
              <div
                v-if="selectedPlanId === plan.id"
                class="absolute bottom-4 right-4 w-6 h-6 rounded-full bg-[var(--accent)] text-white flex items-center justify-center shadow-sm"
              >
                <Check class="w-3.5 h-3.5 stroke-[3]" />
              </div>
            </div>
          </div>
        </Card>

        <!-- Step 3: 其他选项 (优惠码) -->
        <Card v-if="selectedPlan && !hasSubscriptionInSelectedRegion" data-reveal>
          <template #header>
            <div class="flex items-center gap-2.5">
              <span class="w-6 h-6 rounded-full bg-[var(--accent)] text-white text-xs font-semibold flex items-center justify-center shrink-0">3</span>
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('mail.otherOptions') }}</h3>
            </div>
          </template>

          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-1.5 block">
              {{ t('aff.promoCodeOptional') }}
            </label>
            <div class="flex gap-2">
              <div class="flex-1 min-w-0">
                <Input
                  v-model="affCode"
                  :placeholder="t('aff.promoCodeInputPlaceholder')"
                  :disabled="affCodeVerifying"
                  @enter="verifyAffCode"
                />
              </div>
              <Button
                variant="secondary"
                :disabled="affCodeVerifying || !affCode.trim()"
                :loading="affCodeVerifying"
                @click="verifyAffCode"
              >
                <span>{{ t('mail.verify') }}</span>
              </Button>
            </div>
            <div v-if="affCodeValid === true" class="text-xs text-[var(--success)] font-medium mt-2 flex items-center gap-1.5">
              <CheckCircle2 class="w-3.5 h-3.5 shrink-0" />
              <span>{{ t('aff.promoCodeValid', { rate: (affCodeDiscount * 100).toFixed(0) + '%' }) }}</span>
            </div>
            <div v-else-if="affCodeValid === false" class="text-xs text-[var(--error)] font-medium mt-2 flex items-center gap-1.5">
              <AlertCircle class="w-3.5 h-3.5 shrink-0" />
              <span>{{ affCodeError }}</span>
            </div>
          </div>
        </Card>

        <!-- 移动端结算汇总（仅在小屏幕显示） -->
        <Card v-if="selectedPlan && !hasSubscriptionInSelectedRegion" class="lg:hidden">
          <template #header>
            <div class="flex items-center gap-2">
              <ShoppingCart class="w-5 h-5 text-[var(--text-primary)]" />
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                {{ configStore.freeSiteMode ? freeSiteCopy.mailCheckoutTitle : t('mail.checkout.title') }}
              </h3>
            </div>
          </template>

          <div class="space-y-3 text-sm">
            <div class="flex justify-between items-center">
              <span class="text-[var(--text-secondary)]">{{ t('mail.checkout.region') }}</span>
              <span class="text-[var(--text-primary)] font-medium">{{ selectedSource?.name }}</span>
            </div>
            <div class="flex justify-between items-center">
              <span class="text-[var(--text-secondary)]">{{ configStore.freeSiteMode ? freeSiteCopy.mailBillingCycle : t('mail.billingCycle') }}</span>
              <span class="text-[var(--text-primary)] font-medium">{{ getBillingCycleLabel(selectedPlan.billingCycle) }}</span>
            </div>
            <div class="flex justify-between items-center">
              <span class="text-[var(--text-secondary)]">{{ t('mail.checkout.serviceStatus') }}</span>
              <Badge variant="success" size="sm" dot>{{ t('mail.checkout.instant') }}</Badge>
            </div>

            <div class="border-t border-[var(--border-color)] pt-3 mt-3 space-y-2">
              <div v-if="affCodeValid && checkoutPriceInfo" class="flex justify-between text-[var(--text-secondary)]">
                <span>{{ t('aff.originalPrice') }}</span>
                <span>¥{{ checkoutPriceInfo.originalPrice.toFixed(2) }}</span>
              </div>
              <div v-if="affCodeValid && checkoutPriceInfo" class="flex justify-between text-[var(--success)] font-medium">
                <span>{{ t('aff.discountAmount') }}</span>
                <span>-¥{{ checkoutPriceInfo.discountAmount.toFixed(2) }}</span>
              </div>
              <div class="flex justify-between items-baseline pt-1">
                <span class="text-[var(--text-secondary)] font-medium">{{ configStore.freeSiteMode ? freeSiteCopy.mailCheckoutAmount : t('mail.checkout.amount') }}</span>
                <span class="text-2xl font-bold tracking-tight text-[var(--text-primary)]">{{ formatPrice(checkoutPriceInfo?.finalPrice ?? selectedPlan.price) }}</span>
              </div>
            </div>
          </div>

          <Button
            variant="primary"
            size="lg"
            block
            class="mt-5"
            :loading="buyLoading"
            :disabled="buyLoading"
            @click="purchaseSubscription"
          >
            <span>{{ configStore.freeSiteMode ? freeSiteCopy.mailCheckoutConfirm : t('mail.checkout.confirm') }}</span>
            <template #suffix>
              <ChevronRight class="w-4 h-4" />
            </template>
          </Button>

          <p class="text-xs text-[var(--text-tertiary)] text-center mt-3 flex items-center justify-center gap-1.5">
            <Info class="w-3.5 h-3.5 shrink-0" />
            <span>{{ configStore.freeSiteMode ? freeSiteCopy.mailBalanceRequired : t('mail.checkout.balanceRequired') }}</span>
          </p>
        </Card>
      </div>

      <!-- 右侧结算汇总卡片（大屏幕显示） -->
      <div v-if="selectedPlan && !hasSubscriptionInSelectedRegion" class="hidden lg:block w-80 shrink-0">
        <div class="sticky top-6 space-y-4">
          <Card data-reveal>
            <template #header>
              <div class="flex items-center gap-2">
                <ShoppingCart class="w-5 h-5 text-[var(--text-primary)]" />
                <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ configStore.freeSiteMode ? freeSiteCopy.mailCheckoutTitle : t('mail.checkout.title') }}
                </h3>
              </div>
            </template>

            <div class="space-y-3 text-sm">
              <div class="flex justify-between items-center">
                <span class="text-[var(--text-secondary)]">{{ t('mail.checkout.region') }}</span>
                <span class="text-[var(--text-primary)] font-medium">{{ selectedSource?.name }}</span>
              </div>
              <div class="flex justify-between items-center">
                <span class="text-[var(--text-secondary)]">{{ configStore.freeSiteMode ? freeSiteCopy.mailBillingCycle : t('mail.billingCycle') }}</span>
                <span class="text-[var(--text-primary)] font-medium">{{ getBillingCycleLabel(selectedPlan.billingCycle) }}</span>
              </div>
              <div class="flex justify-between items-center">
                <span class="text-[var(--text-secondary)]">{{ t('mail.checkout.serviceStatus') }}</span>
                <Badge variant="success" size="sm" dot>{{ t('mail.checkout.instant') }}</Badge>
              </div>

              <div class="border-t border-[var(--border-color)] pt-3 mt-3 space-y-2">
                <div v-if="affCodeValid && checkoutPriceInfo" class="flex justify-between text-[var(--text-secondary)]">
                  <span>{{ t('aff.originalPrice') }}</span>
                  <span>¥{{ checkoutPriceInfo.originalPrice.toFixed(2) }}</span>
                </div>
                <div v-if="affCodeValid && checkoutPriceInfo" class="flex justify-between text-[var(--success)] font-medium">
                  <span>{{ t('aff.discountAmount') }}</span>
                  <span>-¥{{ checkoutPriceInfo.discountAmount.toFixed(2) }}</span>
                </div>
                <div class="flex justify-between items-baseline pt-1">
                  <span class="text-[var(--text-secondary)] font-medium">{{ configStore.freeSiteMode ? freeSiteCopy.mailCheckoutAmount : t('mail.checkout.amount') }}</span>
                  <span class="text-2xl font-bold tracking-tight text-[var(--text-primary)]">{{ formatPrice(checkoutPriceInfo?.finalPrice ?? selectedPlan.price) }}</span>
                </div>
              </div>
            </div>

            <Button
              variant="primary"
              size="lg"
              block
              class="mt-5"
              :loading="buyLoading"
              :disabled="buyLoading"
              @click="purchaseSubscription"
            >
              <span>{{ configStore.freeSiteMode ? freeSiteCopy.mailCheckoutConfirm : t('mail.checkout.confirm') }}</span>
              <template #suffix>
                <ChevronRight class="w-4 h-4" />
              </template>
            </Button>

            <p class="text-xs text-[var(--text-tertiary)] text-center mt-3 flex items-center justify-center gap-1.5">
              <Info class="w-3.5 h-3.5 shrink-0" />
              <span>{{ configStore.freeSiteMode ? freeSiteCopy.mailBalanceRequired : t('mail.checkout.balanceRequired') }}</span>
            </p>
          </Card>

          <!-- 帮助提示 -->
          <Card class="bg-[var(--bg-surface-soft)]" padding="sm">
            <div class="flex items-start gap-3">
              <HelpCircle class="w-5 h-5 text-[var(--text-tertiary)] shrink-0 mt-0.5" />
              <div class="min-w-0">
                <h4 class="text-sm font-semibold text-[var(--text-primary)]">{{ t('mail.help.title') }}</h4>
                <p class="text-xs text-[var(--text-secondary)] mt-1 leading-relaxed">{{ t('mail.help.desc') }}</p>
              </div>
            </div>
          </Card>
        </div>
      </div>
    </div>

    <!-- 添加域名弹窗 (Apple Modal Sheet) -->
    <Teleport to="body">
      <Transition
        enter-active-class="transition duration-200 ease-out"
        enter-from-class="opacity-0 scale-95"
        enter-to-class="opacity-100 scale-100"
        leave-active-class="transition duration-150 ease-in"
        leave-from-class="opacity-100 scale-100"
        leave-to-class="opacity-0 scale-95"
      >
        <div
          v-if="showAddDomainModal"
          class="fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-6"
          role="dialog"
          aria-modal="true"
          :aria-label="t('mail.addDomain')"
        >
          <!-- 背景遮罩磨砂 -->
          <div
            class="fixed inset-0 bg-black/40 backdrop-blur-sm transition-opacity"
            @click="showAddDomainModal = false"
          />

          <!-- 对话框主体 -->
          <div
            class="relative w-full max-w-md rounded-2xl bg-[var(--bg-surface)] border border-[var(--border-color)] shadow-[0_8px_32px_rgba(0,0,0,0.12)] overflow-hidden flex flex-col z-10"
            @click.stop
          >
            <div class="flex items-center justify-between px-6 py-4 border-b border-[var(--border-color)]">
              <h3 class="text-base md:text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('mail.addDomain') }}
              </h3>
              <button
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
                :title="t('common.cancel')"
                :aria-label="t('common.cancel')"
                @click="showAddDomainModal = false"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>
            <div class="p-6 space-y-4">
              <Input
                v-model="newDomain"
                :label="t('mail.domainName')"
                :placeholder="t('mail.domainPlaceholder')"
                :hint="t('mail.domainHint')"
                @enter="addDomain"
              />
            </div>
            <div class="flex items-center justify-end gap-3 px-6 py-3.5 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
              <Button variant="ghost" size="sm" @click="showAddDomainModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                size="sm"
                :disabled="addDomainLoading || !newDomain.trim()"
                :loading="addDomainLoading"
                @click="addDomain"
              >
                {{ t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 续费弹窗 (Apple Modal Sheet) -->
    <Teleport to="body">
      <Transition
        enter-active-class="transition duration-200 ease-out"
        enter-from-class="opacity-0 scale-95"
        enter-to-class="opacity-100 scale-100"
        leave-active-class="transition duration-150 ease-in"
        leave-from-class="opacity-100 scale-100"
        leave-to-class="opacity-0 scale-95"
      >
        <div
          v-if="showRenewModal"
          class="fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-6"
          role="dialog"
          aria-modal="true"
          :aria-label="t('mail.renewSubscription')"
        >
          <!-- 背景遮罩磨砂 -->
          <div
            class="fixed inset-0 bg-black/40 backdrop-blur-sm transition-opacity"
            @click="showRenewModal = false"
          />

          <!-- 对话框主体 -->
          <div
            class="relative w-full max-w-sm rounded-2xl bg-[var(--bg-surface)] border border-[var(--border-color)] shadow-[0_8px_32px_rgba(0,0,0,0.12)] overflow-hidden flex flex-col z-10"
            @click.stop
          >
            <div class="flex items-center justify-between px-6 py-4 border-b border-[var(--border-color)]">
              <h3 class="text-base md:text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('mail.renewSubscription') }}
              </h3>
              <button
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
                :title="t('common.cancel')"
                :aria-label="t('common.cancel')"
                @click="showRenewModal = false"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>
            <div class="p-6 space-y-4">
              <!-- 续费时长选择 -->
              <div>
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-1.5 block">
                  {{ t('mail.renewDuration') }}
                </label>
                <!-- 按年计费：只能续费1年 -->
                <div
                  v-if="subscription?.plan?.billingCycle === 'yearly'"
                  class="min-h-[44px] px-3.5 flex items-center rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-sm text-[var(--text-primary)] font-medium"
                >
                  1 {{ t('mail.year') }}
                </div>
                <!-- 按月计费：可选择1/3/6/12个月 -->
                <Select
                  v-else
                  :model-value="renewMonths"
                  :options="renewOptions"
                  @update:model-value="(val: any) => renewMonths = Number(val)"
                />
              </div>

              <!-- 费用明细卡片 -->
              <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-4 space-y-2.5">
                <!-- 按年计费 -->
                <template v-if="subscription?.plan?.billingCycle === 'yearly'">
                  <div class="flex justify-between text-sm">
                    <span class="text-[var(--text-secondary)]">{{ t('mail.yearlyPrice') }}</span>
                    <span class="text-[var(--text-primary)] font-medium">¥{{ Number(subscription.plan.price).toFixed(2) }}/{{ t('mail.year') }}</span>
                  </div>
                </template>
                <!-- 按月计费 -->
                <template v-else>
                  <div class="flex justify-between text-sm">
                    <span class="text-[var(--text-secondary)]">{{ t('mail.monthlyPrice') }}</span>
                    <span class="text-[var(--text-primary)] font-medium">¥{{ renewMonthlyPrice.toFixed(2) }}/{{ t('mail.month') }}</span>
                  </div>
                  <div class="flex justify-between text-sm">
                    <span class="text-[var(--text-secondary)]">{{ t('mail.renewDuration') }}</span>
                    <span class="text-[var(--text-primary)] font-medium">{{ renewMonths }} {{ renewMonths > 1 ? t('mail.months') : t('mail.month') }}</span>
                  </div>
                </template>
                <div v-if="renewDiscountRate > 0" class="flex justify-between text-sm text-[var(--success)] font-medium">
                  <span>{{ t('aff.promoDiscount') }} ({{ (renewDiscountRate * 100).toFixed(0) }}%)</span>
                  <span>-¥{{ renewDiscountAmount.toFixed(2) }}</span>
                </div>
                <div class="border-t border-[var(--border-color)] pt-2.5 flex justify-between items-baseline font-medium">
                  <span class="text-[var(--text-secondary)]">{{ t('mail.totalPrice') }}</span>
                  <span class="text-[var(--text-primary)] text-xl font-bold tracking-tight">¥{{ renewFinalPrice.toFixed(2) }}</span>
                </div>
              </div>
            </div>
            <div class="flex items-center justify-end gap-3 px-6 py-3.5 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
              <Button variant="ghost" size="sm" @click="showRenewModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                size="sm"
                :disabled="renewLoading"
                :loading="renewLoading"
                @click="renewSubscription"
              >
                {{ t('mail.confirmRenew') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
