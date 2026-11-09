<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  AlertCircle,
  Banknote,
  CheckCircle2,
  Coins,
  CreditCard,
  Globe,
  Pencil,
  Plus,
  RotateCw,
  Sliders,
  Trash2,
  Wallet
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Select
} from '@/components/ui'
import DrawerModal from '@/components/DrawerModal.vue'
import { useReveal } from '@/composables/useReveal'

defineOptions({ name: 'AdminPaymentProvidersView' })

const props = withDefaults(defineProps<{
  embedded?: boolean
}>(), {
  embedded: false
})

const { t } = useI18n()
const toast = useToast()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 支付渠道列表
const providers = ref<any[]>([])
const loading = ref(true)

// 编辑/创建抽屉
const showModal = ref(false)
const isEditing = ref(false)
const modalLoading = ref(false)
const formData = ref({
  id: 0,
  name: '',
  type: 'yipay',
  config: {
    apiurl: '',
    pid: '',
    platform_public_key: '',
    merchant_private_key: ''
  } as Record<string, unknown>,
  methods: [] as string[],
  minAmount: 1,
  maxAmount: undefined as number | undefined,
  feeRate: 0,
  feeFixed: 0,
  sortOrder: 0
})

const DEFAULT_YIPAY_METHODS = ['alipay', 'wxpay']
const YIPAY_METHODS = ['alipay', 'wxpay', 'qqpay']
const IMPLEMENTED_PROVIDER_TYPES = new Set(['yipay', 'heleket', 'antom', 'manual'])

// 统计指标
const activeCount = computed(() => providers.value.filter(p => p.status === 'active').length)
const testingCount = computed(() => providers.value.filter(p => p.status === 'testing').length)
const disabledCount = computed(() => providers.value.filter(p => p.status === 'disabled').length)

function getConfigMethodFees(config: Record<string, unknown>): Record<string, { feeRate?: number; feeFixed?: number }> {
  const raw = (config as any).methodFees
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) {
    return {}
  }
  return raw as Record<string, { feeRate?: number; feeFixed?: number }>
}

function getYipayMethodFeePercent(method: string): number {
  const methodFees = getConfigMethodFees(formData.value.config)
  const feeRate = methodFees[method]?.feeRate !== undefined
    ? Number(methodFees[method]?.feeRate || 0)
    : Number(formData.value.feeRate || 0) / 100
  return Number.isFinite(feeRate) ? Number((feeRate * 100).toFixed(2)) : 0
}

function setYipayMethodFeePercent(method: string, value: string | number) {
  const percent = Number(value)
  const feeRate = Number.isFinite(percent) && percent > 0 ? percent / 100 : 0
  const methodFees = {
    ...getConfigMethodFees(formData.value.config),
    [method]: {
      ...getConfigMethodFees(formData.value.config)[method],
      feeRate
    }
  }
  formData.value.config = {
    ...formData.value.config,
    methodFees
  }
}

function getProviderMethodFeePercent(provider: any, method: string): number {
  const methodFees = getConfigMethodFees(provider.config || {})
  const feeRate = methodFees[method]?.feeRate !== undefined
    ? Number(methodFees[method]?.feeRate || 0)
    : Number(provider.feeRate || 0)
  return Number.isFinite(feeRate) ? feeRate * 100 : 0
}

function getProviderMethodSummary(provider: any): string {
  const methods = provider.methods || []
  if (provider.type !== 'yipay' || methods.length === 0) {
    return methods.join(', ') || '-'
  }

  return methods.map((method: string) => {
    const percent = getProviderMethodFeePercent(provider, method)
    const name = t(`wallet.paymentMethods.${method}`)
    const label = name === `wallet.paymentMethods.${method}` ? method : name
    return percent > 0 ? `${label} (${percent.toFixed(2)}%)` : label
  }).join(', ')
}

function getSecretPlaceholder(key: string, fallback: string): string {
  return isEditing.value && (formData.value.config as any)[`${key}Configured`]
    ? t('admin.paymentProviders.config.secretKeepPlaceholder')
    : fallback
}

function getSecretHint(key: string, fallback: string): string {
  const configuredHint = isEditing.value && (formData.value.config as any)[`${key}Configured`]
    ? `${t('admin.paymentProviders.config.secretKeepHint')} `
    : ''
  return `${configuredHint}${fallback}`.trim()
}

// 支付渠道类型选项
const providerTypes = computed(() => [
  { value: 'yipay', label: t('admin.paymentProviders.providerTypes.yipay') },
  { value: 'heleket', label: t('admin.paymentProviders.providerTypes.heleket') },
  { value: 'antom', label: t('admin.paymentProviders.providerTypes.antom') },
  { value: 'stripe', label: t('admin.paymentProviders.providerTypes.stripe') },
  { value: 'alipay_direct', label: t('admin.paymentProviders.providerTypes.alipayDirect') },
  { value: 'wechat_direct', label: t('admin.paymentProviders.providerTypes.wechatDirect') },
  { value: 'manual', label: t('admin.paymentProviders.providerTypes.manual') }
].map(option => {
  const implemented = IMPLEMENTED_PROVIDER_TYPES.has(option.value)
  return {
    ...option,
    disabled: !implemented,
    label: implemented ? option.label : `${option.label} (${t('admin.paymentProviders.notImplemented')})`
  }
}))

// 状态选项
const statusOptions = computed(() => [
  { value: 'active', label: t('admin.paymentProviders.statusActive') },
  { value: 'disabled', label: t('admin.paymentProviders.statusDisabled') },
  { value: 'testing', label: t('admin.paymentProviders.statusTesting') }
])

function getStatusVariant(status: string): 'success' | 'warning' | 'default' {
  switch (status) {
    case 'active':
      return 'success'
    case 'testing':
      return 'warning'
    case 'disabled':
    default:
      return 'default'
  }
}

function getTypeName(type: string): string {
  const typeKeyMap: Record<string, string> = {
    yipay: 'yipay',
    heleket: 'heleket',
    antom: 'antom',
    stripe: 'stripe',
    alipay_direct: 'alipayDirect',
    wechat_direct: 'wechatDirect',
    manual: 'manual'
  }
  const key = typeKeyMap[type]
  return key ? t(`admin.paymentProviders.providerTypes.${key}`) : type
}

function getProviderIcon(type: string) {
  switch (type) {
    case 'yipay':
      return Wallet
    case 'antom':
      return Globe
    case 'heleket':
      return Coins
    case 'manual':
      return Banknote
    default:
      return CreditCard
  }
}

// 删除确认弹窗
const showDeleteModal = ref(false)
const deleteTarget = ref<any>(null)
const deleteLoading = ref(false)

onMounted(() => {
  loadProviders()
})

async function loadProviders() {
  loading.value = true
  try {
    const res = await api.admin.getPaymentProviders()
    providers.value = res.providers || []
  } catch (err: any) {
    toast.error(t('admin.paymentProviders.loadFailed') + ': ' + (err?.response?.data?.message || err?.message || String(err)))
  } finally {
    loading.value = false
  }
}

// 根据渠道类型获取默认配置
function getDefaultConfig(type: string, version?: string): Record<string, unknown> {
  switch (type) {
    case 'yipay':
      if (version === 'v1') {
        return {
          version: 'v1',
          apiurl: '',
          pid: '',
          key: '',
          methodFees: {}
        }
      }
      return {
        version: 'v2',
        apiurl: '',
        pid: '',
        platform_public_key: '',
        merchant_private_key: '',
        methodFees: {}
      }
    case 'heleket':
      return {
        apiurl: 'https://api.heleket.com',
        merchant_uuid: '',
        api_key: '',
        currency: 'CNY',
        lifetime: 3600
      }
    case 'antom':
      return {
        environment: 'sandbox',
        apiurl: 'https://open-sea-global.alipay.com',
        clientId: '',
        antom_public_key: '',
        merchant_private_key: '',
        keyVersion: 1,
        currency: 'USD',
        currencyRate: 1,
        currencyExponent: 2,
        merchantRegion: '',
        settlementCurrency: '',
        merchantAccountId: '',
        locale: 'zh_CN',
        orderDescription: 'PayIncus account recharge',
        sessionExpiryMinutes: 30
      }
    case 'stripe':
      return {
        publishable_key: '',
        secret_key: '',
        webhook_secret: ''
      }
    case 'alipay_direct':
      return {
        app_id: '',
        private_key: '',
        public_key: ''
      }
    case 'wechat_direct':
      return {
        app_id: '',
        mch_id: '',
        api_key: '',
        cert_path: '',
        key_path: ''
      }
    case 'manual':
      return {
        instructions: ''
      }
    default:
      return {}
  }
}

function getDefaultMethods(type: string): string[] {
  switch (type) {
    case 'yipay':
      return [...DEFAULT_YIPAY_METHODS]
    default:
      return []
  }
}

function handleProviderTypeChange(type: string) {
  formData.value.config = getDefaultConfig(type)
  formData.value.methods = getDefaultMethods(type)
}

function openCreateModal() {
  isEditing.value = false
  formData.value = {
    id: 0,
    name: '',
    type: 'yipay',
    config: getDefaultConfig('yipay', 'v2'),
    methods: getDefaultMethods('yipay'),
    minAmount: 1,
    maxAmount: undefined,
    feeRate: 0,
    feeFixed: 0,
    sortOrder: 0
  }
  showModal.value = true
}

function openEditModal(provider: any) {
  isEditing.value = true
  const version = provider.config?.version || 'v2'
  const defaultConfig = getDefaultConfig(provider.type, version)
  const mergedConfig = { ...defaultConfig, ...(provider.config || {}) }

  formData.value = {
    id: provider.id,
    name: provider.name,
    type: provider.type,
    config: mergedConfig,
    methods: provider.methods?.length ? provider.methods : getDefaultMethods(provider.type),
    minAmount: provider.minAmount,
    maxAmount: provider.maxAmount !== null && provider.maxAmount !== undefined ? Number(provider.maxAmount) : undefined,
    feeRate: (Number(provider.feeRate) || 0) * 100, // 转换为百分比显示
    feeFixed: Number(provider.feeFixed) || 0,
    sortOrder: Number(provider.sortOrder) || 0
  }
  showModal.value = true
}

function buildConfigForSave(): Record<string, unknown> {
  if (formData.value.type !== 'yipay') {
    return formData.value.config
  }

  const sourceFees = getConfigMethodFees(formData.value.config)
  const methodFees: Record<string, { feeRate: number; feeFixed: number }> = {}
  const globalFallbackRate = Number(formData.value.feeRate || 0) / 100
  const globalFallbackFixed = Number(formData.value.feeFixed || 0)
  for (const method of formData.value.methods) {
    const feeRate = sourceFees[method]?.feeRate !== undefined
      ? Number(sourceFees[method]?.feeRate || 0)
      : globalFallbackRate
    const feeFixed = sourceFees[method]?.feeFixed !== undefined
      ? Number(sourceFees[method]?.feeFixed || 0)
      : globalFallbackFixed
    methodFees[method] = {
      feeRate: Number.isFinite(feeRate) && feeRate > 0 ? feeRate : 0,
      feeFixed: Number.isFinite(feeFixed) && feeFixed > 0 ? feeFixed : 0
    }
  }

  return {
    ...formData.value.config,
    methodFees
  }
}

async function saveProvider() {
  if (!formData.value.name.trim()) {
    toast.error(t('admin.paymentProviders.nameRequired'))
    return
  }

  modalLoading.value = true
  try {
    const minAmountNum = Number(formData.value.minAmount)
    const maxAmountNum = formData.value.maxAmount !== null && formData.value.maxAmount !== undefined && (formData.value.maxAmount as unknown as string) !== ''
      ? Number(formData.value.maxAmount)
      : null
    const sortOrderNum = Number(formData.value.sortOrder) || 0

    const data = {
      name: formData.value.name.trim(),
      type: formData.value.type,
      config: buildConfigForSave(),
      methods: formData.value.methods,
      minAmount: Number.isFinite(minAmountNum) ? minAmountNum : 1,
      maxAmount: maxAmountNum !== null && Number.isFinite(maxAmountNum) ? maxAmountNum : null,
      feeRate: formData.value.type === 'yipay' ? 0 : (Number(formData.value.feeRate) || 0) / 100, // 转换回小数
      feeFixed: formData.value.type === 'yipay' ? 0 : (Number(formData.value.feeFixed) || 0),
      sortOrder: Number.isFinite(sortOrderNum) ? sortOrderNum : 0
    }

    if (isEditing.value) {
      await api.admin.updatePaymentProvider(formData.value.id, data)
      toast.success(t('admin.paymentProviders.updateSuccess'))
    } else {
      await api.admin.createPaymentProvider(data)
      toast.success(t('admin.paymentProviders.createSuccess'))
    }

    showModal.value = false
    await loadProviders()
  } catch (err: any) {
    toast.error((isEditing.value ? t('admin.paymentProviders.updateFailed') : t('admin.paymentProviders.createFailed')) + ': ' + (err?.response?.data?.message || err?.message || String(err)))
  } finally {
    modalLoading.value = false
  }
}

async function updateStatus(provider: any, newStatus: string) {
  try {
    await api.admin.updatePaymentProviderStatus(provider.id, newStatus)
    provider.status = newStatus
    toast.success(t('admin.paymentProviders.statusUpdateSuccess'))
    await loadProviders()
  } catch (err: any) {
    toast.error(t('admin.paymentProviders.statusUpdateFailed') + ': ' + (err?.response?.data?.message || err?.message || String(err)))
  }
}

function confirmDelete(provider: any) {
  deleteTarget.value = provider
  showDeleteModal.value = true
}

async function deleteProvider() {
  if (!deleteTarget.value) return

  deleteLoading.value = true
  try {
    await api.admin.deletePaymentProvider(deleteTarget.value.id)
    toast.success(t('admin.paymentProviders.deleteSuccess'))
    showDeleteModal.value = false
    deleteTarget.value = null
    await loadProviders()
  } catch (err: any) {
    toast.error(t('admin.paymentProviders.deleteFailed') + ': ' + (err?.response?.data?.message || err?.message || String(err)))
  } finally {
    deleteLoading.value = false
  }
}

function togglePaymentMethod(method: string) {
  const index = formData.value.methods.indexOf(method)
  if (index === -1) {
    formData.value.methods.push(method)
  } else {
    formData.value.methods.splice(index, 1)
  }
}
</script>

<template>
  <div ref="revealRoot" :class="['animate-fade-in', props.embedded ? 'space-y-4' : 'space-y-6']">
    <!-- 页面头部 -->
    <PageHeader
      :title="t('admin.paymentProviders.title')"
      :description="t('admin.paymentProviders.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadProviders"
        >
          <template #icon>
            <RotateCw class="w-4 h-4" :class="{ 'animate-spin': loading }" />
          </template>
          {{ t('giftCards.refresh') }}
        </Button>
        <Button
          variant="primary"
          size="sm"
          @click="openCreateModal"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2.5]" />
          </template>
          {{ t('admin.paymentProviders.add') }}
        </Button>
      </template>
    </PageHeader>

    <!-- 概览指标卡 -->
    <div data-reveal class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.paymentProviders.title') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <CreditCard class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ providers.length }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.paymentProviders.statusActive') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <CheckCircle2 class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--success)]">
          {{ activeCount }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.paymentProviders.statusTesting') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Sliders class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--warning)]">
          {{ testingCount }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.paymentProviders.statusDisabled') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--text-tertiary)_12%,var(--bg-surface))] text-[var(--text-tertiary)] border border-[color-mix(in_srgb,var(--text-tertiary)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <AlertCircle class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-tertiary)]">
          {{ disabledCount }}
        </div>
      </Card>
    </div>

    <!-- 加载骨架屏 -->
    <div v-if="loading && providers.length === 0" class="space-y-4">
      <Card v-for="i in 3" :key="`skeleton-${i}`" loading padding="md" />
    </div>

    <!-- 支付渠道卡片列表 -->
    <div v-else-if="providers.length > 0" data-reveal class="space-y-4">
      <Card
        v-for="provider in providers"
        :key="provider.id"
        hoverable
        padding="md"
        class="group transition-all duration-300"
      >
        <div class="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
          <!-- 渠道基本信息与图标 -->
          <div class="min-w-0 flex-1">
            <div class="flex items-start gap-3.5">
              <div class="flex h-11 w-11 items-center justify-center rounded-2xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] shrink-0 group-hover:scale-105 transition-transform duration-300">
                <component :is="getProviderIcon(provider.type)" class="w-5 h-5 stroke-[2]" />
              </div>

              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <h3 class="font-semibold text-base md:text-lg tracking-tight text-[var(--text-primary)] truncate">
                    {{ provider.name }}
                  </h3>
                  <Badge :variant="getStatusVariant(provider.status)" dot size="sm">
                    {{ statusOptions.find(s => s.value === provider.status)?.label || provider.status }}
                  </Badge>
                  <Badge variant="info" size="sm">
                    {{ getTypeName(provider.type) }}
                  </Badge>
                  <span v-if="provider.sortOrder" class="font-mono text-xs text-[var(--text-tertiary)]">
                    #{{ provider.sortOrder }}
                  </span>
                </div>

                <div class="text-xs font-mono text-[var(--text-tertiary)] mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5">
                  <span>ID: {{ provider.id }}</span>
                  <span v-if="provider.config?.apiurl" class="font-sans text-[var(--text-secondary)] truncate max-w-sm">
                    {{ provider.config.apiurl }}
                  </span>
                </div>
              </div>
            </div>

            <!-- 参数配置网格 -->
            <div class="mt-4 grid grid-cols-2 md:grid-cols-4 gap-3 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
              <div>
                <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
                  {{ t('admin.paymentProviders.minAmount') }}
                </div>
                <div class="mt-1 font-mono text-sm font-semibold tabular-nums text-[var(--text-primary)]">
                  ¥{{ provider.minAmount }}
                </div>
              </div>
              <div>
                <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
                  {{ t('admin.paymentProviders.maxAmount') }}
                </div>
                <div class="mt-1 font-mono text-sm font-semibold tabular-nums text-[var(--text-primary)]">
                  {{ provider.maxAmount ? `¥${provider.maxAmount}` : t('common.unlimited') }}
                </div>
              </div>
              <div>
                <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
                  {{ t('admin.paymentProviders.feeRate') }}
                </div>
                <div class="mt-1 font-mono text-sm font-semibold tabular-nums text-[var(--accent)]">
                  {{ (provider.feeRate * 100).toFixed(2) }}%
                </div>
              </div>
              <div>
                <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
                  {{ t('admin.paymentProviders.feeFixed') }}
                </div>
                <div class="mt-1 font-mono text-sm font-semibold tabular-nums text-[var(--text-primary)]">
                  ¥{{ provider.feeFixed }}
                </div>
              </div>
            </div>

            <!-- 支持的支付方式及费率 -->
            <div class="mt-3.5 flex flex-wrap items-center justify-between gap-2 text-xs pt-0.5">
              <div class="flex items-center gap-2 min-w-0">
                <span class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-tertiary)] shrink-0">
                  {{ t('admin.paymentProviders.methods') }}:
                </span>
                <span class="text-[var(--text-secondary)] truncate">
                  {{ getProviderMethodSummary(provider) }}
                </span>
              </div>

              <div v-if="provider.type === 'manual' && provider.config?.instructions" class="flex items-center gap-1.5 text-xs text-[var(--text-tertiary)]">
                <span class="font-medium text-[var(--text-secondary)]">说明已配置</span>
              </div>
            </div>
          </div>

          <!-- 右侧操作栏 -->
          <div class="flex flex-wrap items-center gap-2 lg:ml-4 lg:justify-end shrink-0 pt-2 lg:pt-0 border-t lg:border-t-0 border-[var(--border-color)]">
            <div class="w-28">
              <Select
                :model-value="provider.status"
                :options="statusOptions"
                size="sm"
                @change="(val: any) => updateStatus(provider, String(val))"
              />
            </div>

            <Button
              variant="secondary"
              size="sm"
              @click="openEditModal(provider)"
            >
              <template #icon>
                <Pencil class="w-3.5 h-3.5" />
              </template>
              {{ t('admin.paymentProviders.edit') }}
            </Button>

            <Button
              variant="ghost"
              size="sm"
              class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
              @click="confirmDelete(provider)"
            >
              <template #icon>
                <Trash2 class="w-3.5 h-3.5" />
              </template>
              {{ t('admin.paymentProviders.delete') }}
            </Button>
          </div>
        </div>
      </Card>
    </div>

    <!-- 空状态 -->
    <Card v-else data-reveal padding="lg">
      <EmptyState
        :title="t('admin.paymentProviders.empty')"
        :description="t('admin.paymentProviders.noProviders')"
      >
        <template #icon>
          <CreditCard class="w-7 h-7 stroke-[1.5]" />
        </template>
        <template #action>
          <Button variant="primary" size="md" @click="openCreateModal">
            <template #icon>
              <Plus class="w-4 h-4 stroke-[2.5]" />
            </template>
            {{ t('admin.paymentProviders.add') }}
          </Button>
        </template>
      </EmptyState>
    </Card>

    <!-- 创建/编辑渠道抽屉（DrawerModal） -->
    <DrawerModal
      v-model="showModal"
      :title="isEditing ? t('admin.paymentProviders.edit') : t('admin.paymentProviders.add')"
      max-width="max-w-2xl"
    >
      <div class="space-y-4 text-sm">
        <div>
          <Input
            v-model="formData.name"
            :label="t('admin.paymentProviders.name') + ' *'"
            :placeholder="t('admin.paymentProviders.namePlaceholder')"
          />
        </div>

        <div>
          <Select
            v-model="formData.type"
            :label="t('admin.paymentProviders.type')"
            :options="providerTypes"
            :disabled="isEditing"
            @change="handleProviderTypeChange(formData.type)"
          />
        </div>

        <!-- 易支付配置 -->
        <template v-if="formData.type === 'yipay'">
          <div>
            <Select
              v-model="(formData.config as any).version"
              :label="t('admin.paymentProviders.config.sdkVersion') + ' *'"
              :options="[
                { value: 'v1', label: t('admin.paymentProviders.config.yipayVersionV1') },
                { value: 'v2', label: t('admin.paymentProviders.config.yipayVersionV2') }
              ]"
              :hint="(formData.config as any).version === 'v1'
                ? t('admin.paymentProviders.config.yipayVersionV1Hint')
                : t('admin.paymentProviders.config.yipayVersionV2Hint')"
              @change="formData.config = getDefaultConfig('yipay', (formData.config as any).version)"
            />
          </div>

          <div>
            <Input
              v-model="(formData.config as any).apiurl"
              :label="t('admin.paymentProviders.config.apiurl') + ' *'"
              :placeholder="t('admin.paymentProviders.config.yipayApiUrlPlaceholder')"
              :hint="t('admin.paymentProviders.config.yipayApiUrlHint')"
            />
          </div>

          <div>
            <Input
              v-model="(formData.config as any).pid"
              :label="t('admin.paymentProviders.config.pid') + ' *'"
              :placeholder="t('admin.paymentProviders.config.yipayPidPlaceholder')"
              class="font-mono"
            />
          </div>

          <!-- V1 版本: MD5密钥 -->
          <template v-if="(formData.config as any).version === 'v1'">
            <div>
              <Input
                v-model="(formData.config as any).key"
                type="password"
                :label="t('admin.paymentProviders.config.key') + ' *'"
                :placeholder="getSecretPlaceholder('key', t('admin.paymentProviders.config.yipayKeyPlaceholder'))"
                :hint="getSecretHint('key', t('admin.paymentProviders.config.yipayKeyHint'))"
                class="font-mono"
              />
            </div>
          </template>

          <!-- V2 版本: RSA密钥对 -->
          <template v-else>
            <div>
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
                {{ t('admin.paymentProviders.config.platformPublicKey') }} *
              </label>
              <textarea
                v-model="(formData.config as any).platform_public_key"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-xs font-mono focus:outline-none transition-all duration-200 resize-none h-24"
                :placeholder="t('admin.paymentProviders.config.platformPublicKeyPlaceholder')"
              ></textarea>
              <p class="text-xs text-[var(--text-tertiary)] mt-1">{{ t('admin.paymentProviders.config.platformPublicKeyHint') }}</p>
            </div>

            <div>
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
                {{ t('admin.paymentProviders.config.merchantPrivateKey') }} *
              </label>
              <textarea
                v-model="(formData.config as any).merchant_private_key"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-xs font-mono focus:outline-none transition-all duration-200 resize-none h-24"
                :placeholder="getSecretPlaceholder('merchant_private_key', t('admin.paymentProviders.config.merchantPrivateKeyPlaceholder'))"
              ></textarea>
              <p class="text-xs text-[var(--text-tertiary)] mt-1">{{ getSecretHint('merchant_private_key', t('admin.paymentProviders.config.merchantPrivateKeyHint')) }}</p>
            </div>
          </template>

          <!-- 支付方式选择（V1/V2通用） -->
          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-2">
              {{ t('admin.paymentProviders.paymentMethods') }}
            </label>
            <div class="space-y-2">
              <div
                v-for="method in YIPAY_METHODS"
                :key="method"
                class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 transition-colors duration-200"
              >
                <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                  <label class="flex items-center gap-2.5 cursor-pointer select-none">
                    <input
                      type="checkbox"
                      :checked="formData.methods.includes(method)"
                      class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                      @change="togglePaymentMethod(method)"
                    />
                    <span class="text-sm font-medium text-[var(--text-primary)]">{{ t(`wallet.paymentMethods.${method}`) }}</span>
                  </label>
                  <div class="flex items-center gap-2 sm:w-48">
                    <span class="whitespace-nowrap text-xs text-[var(--text-secondary)] shrink-0">{{ t('admin.paymentProviders.feeRate') }}</span>
                    <Input
                      :model-value="getYipayMethodFeePercent(method)"
                      type="number"
                      size="sm"
                      min="0"
                      max="100"
                      step="0.01"
                      class="font-mono tabular-nums flex-1"
                      :disabled="!formData.methods.includes(method)"
                      @update:model-value="(val: any) => setYipayMethodFeePercent(method, val)"
                    >
                      <template #suffix>
                        <span class="text-xs text-[var(--text-tertiary)] font-medium">%</span>
                      </template>
                    </Input>
                  </div>
                </div>
              </div>
            </div>
            <p class="text-xs text-[var(--text-tertiary)] mt-1.5">{{ t('admin.paymentProviders.config.yipayMethodsHint') }}</p>
            <p class="text-xs text-[var(--text-tertiary)] mt-0.5">{{ t('admin.paymentProviders.config.yipayMethodFeeHint') }}</p>
          </div>
        </template>

        <!-- Heleket 配置 -->
        <template v-else-if="formData.type === 'heleket'">
          <div>
            <Input
              v-model="(formData.config as any).apiurl"
              :label="t('admin.paymentProviders.config.apiurl') + ' *'"
              placeholder="https://api.heleket.com"
              :hint="t('admin.paymentProviders.config.heleketApiUrlHint')"
            />
          </div>
          <div>
            <Input
              v-model="(formData.config as any).merchant_uuid"
              :label="t('admin.paymentProviders.config.heleketMerchantUuid') + ' *'"
              :placeholder="t('admin.paymentProviders.config.heleketMerchantUuid')"
              class="font-mono"
            />
          </div>
          <div>
            <Input
              v-model="(formData.config as any).api_key"
              type="password"
              :label="t('admin.paymentProviders.config.heleketApiKey') + ' *'"
              :placeholder="getSecretPlaceholder('api_key', t('admin.paymentProviders.config.heleketApiKey'))"
              :hint="getSecretHint('api_key', '')"
              class="font-mono"
            />
          </div>
          <div>
            <Input
              v-model="(formData.config as any).currency"
              :label="t('admin.paymentProviders.config.heleketInvoiceCurrency')"
              class="uppercase"
              placeholder="CNY"
              :hint="t('admin.paymentProviders.config.heleketCurrencyHint')"
            />
          </div>
          <div>
            <Input
              v-model.number="(formData.config as any).lifetime"
              type="number"
              :label="t('admin.paymentProviders.config.heleketLifetime')"
              min="300"
              max="43200"
              step="1"
              placeholder="3600"
              :hint="t('admin.paymentProviders.config.heleketLifetimeHint')"
            />
          </div>
        </template>

        <!-- Antom Hosted Checkout 配置 -->
        <template v-else-if="formData.type === 'antom'">
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div>
              <Select
                v-model="(formData.config as any).environment"
                :label="t('admin.paymentProviders.config.antomEnvironment') + ' *'"
                :options="[
                  { value: 'sandbox', label: t('admin.paymentProviders.config.antomSandbox') },
                  { value: 'production', label: t('admin.paymentProviders.config.antomProduction') }
                ]"
              />
            </div>
            <div>
              <Select
                v-model="(formData.config as any).apiurl"
                :label="t('admin.paymentProviders.config.antomEndpoint') + ' *'"
                :options="[
                  { value: 'https://open-sea-global.alipay.com', label: 'Asia' },
                  { value: 'https://open-na-global.alipay.com', label: 'North America' },
                  { value: 'https://open.antglobal-us.com', label: 'US Merchant' },
                  { value: 'https://open-de-global.alipay.com', label: 'Europe' }
                ]"
              />
            </div>
          </div>
          <p class="text-xs text-[var(--text-tertiary)] -mt-2">{{ t('admin.paymentProviders.config.antomEnvironmentHint') }}</p>

          <div>
            <Input
              v-model="(formData.config as any).clientId"
              label="Client ID *"
              class="font-mono"
              placeholder="SANDBOX_..."
            />
          </div>

          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
              {{ t('admin.paymentProviders.config.antomPublicKey') }} *
            </label>
            <textarea
              v-model="(formData.config as any).antom_public_key"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-xs font-mono focus:outline-none transition-all duration-200 resize-none h-28"
              :placeholder="t('admin.paymentProviders.config.antomPublicKeyPlaceholder')"
            ></textarea>
            <p class="mt-1 text-xs text-[var(--text-tertiary)]">{{ t('admin.paymentProviders.config.antomPublicKeyHint') }}</p>
          </div>

          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
              {{ t('admin.paymentProviders.config.merchantPrivateKey') }} *
            </label>
            <textarea
              v-model="(formData.config as any).merchant_private_key"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-xs font-mono focus:outline-none transition-all duration-200 resize-none h-28"
              :placeholder="getSecretPlaceholder('merchant_private_key', t('admin.paymentProviders.config.merchantPrivateKeyPlaceholder'))"
            ></textarea>
            <p class="mt-1 text-xs text-[var(--text-tertiary)]">{{ getSecretHint('merchant_private_key', t('admin.paymentProviders.config.antomPrivateKeyHint')) }}</p>
          </div>

          <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
            <div>
              <Input
                v-model="(formData.config as any).currency"
                :label="t('admin.paymentProviders.config.antomCurrency') + ' *'"
                class="uppercase"
                placeholder="USD"
                :maxlength="3"
              />
            </div>
            <div>
              <Input
                v-model.number="(formData.config as any).currencyRate"
                type="number"
                :label="t('admin.paymentProviders.config.antomCurrencyRate') + ' *'"
                class="font-mono tabular-nums"
                min="0.000001"
                step="0.000001"
              />
            </div>
            <div>
              <Input
                v-model.number="(formData.config as any).currencyExponent"
                type="number"
                :label="t('admin.paymentProviders.config.antomCurrencyExponent') + ' *'"
                class="font-mono tabular-nums"
                min="0"
                max="3"
                step="1"
              />
            </div>
          </div>
          <p class="text-xs text-[var(--text-tertiary)] -mt-2">{{ t('admin.paymentProviders.config.antomCurrencyHint') }}</p>

          <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <Input
                v-model="(formData.config as any).merchantRegion"
                :label="t('admin.paymentProviders.config.antomMerchantRegion')"
                class="uppercase"
                placeholder="SG"
                :maxlength="2"
              />
            </div>
            <div>
              <Input
                v-model="(formData.config as any).settlementCurrency"
                :label="t('admin.paymentProviders.config.antomSettlementCurrency')"
                class="uppercase"
                placeholder="USD"
                :maxlength="3"
              />
            </div>
            <div>
              <Input
                v-model.number="(formData.config as any).keyVersion"
                type="number"
                label="Key Version"
                class="font-mono tabular-nums"
                min="0"
                max="999"
                step="1"
              />
            </div>
            <div>
              <Input
                v-model.number="(formData.config as any).sessionExpiryMinutes"
                type="number"
                :label="t('admin.paymentProviders.config.antomSessionExpiry')"
                class="font-mono tabular-nums"
                min="5"
                max="60"
                step="1"
              />
            </div>
          </div>

          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs text-[var(--text-secondary)] leading-relaxed">
            {{ t('admin.paymentProviders.config.antomCallbackHint') }}
          </div>
        </template>

        <!-- Stripe 配置 -->
        <template v-else-if="formData.type === 'stripe'">
          <div>
            <Input
              v-model="(formData.config as any).publishable_key"
              label="Publishable Key *"
              class="font-mono"
              placeholder="pk_live_..."
            />
          </div>
          <div>
            <Input
              v-model="(formData.config as any).secret_key"
              type="password"
              label="Secret Key *"
              class="font-mono"
              placeholder="sk_live_..."
            />
          </div>
          <div>
            <Input
              v-model="(formData.config as any).webhook_secret"
              type="password"
              label="Webhook Secret"
              class="font-mono"
              placeholder="whsec_..."
            />
          </div>
        </template>

        <!-- 支付宝直连配置 -->
        <template v-else-if="formData.type === 'alipay_direct'">
          <div>
            <Input
              v-model="(formData.config as any).app_id"
              label="App ID *"
              class="font-mono"
            />
          </div>
          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
              应用私钥 *
            </label>
            <textarea
              v-model="(formData.config as any).private_key"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-xs font-mono focus:outline-none transition-all duration-200 resize-none h-24"
            ></textarea>
          </div>
          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
              支付宝公钥 *
            </label>
            <textarea
              v-model="(formData.config as any).public_key"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-xs font-mono focus:outline-none transition-all duration-200 resize-none h-24"
            ></textarea>
          </div>
        </template>

        <!-- 微信直连配置 -->
        <template v-else-if="formData.type === 'wechat_direct'">
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <Input
                v-model="(formData.config as any).app_id"
                label="App ID *"
                class="font-mono"
              />
            </div>
            <div>
              <Input
                v-model="(formData.config as any).mch_id"
                label="商户号 *"
                class="font-mono"
              />
            </div>
          </div>
          <div>
            <Input
              v-model="(formData.config as any).api_key"
              type="password"
              label="API 密钥 *"
              class="font-mono"
            />
          </div>
        </template>

        <!-- 人工充值配置 -->
        <template v-else-if="formData.type === 'manual'">
          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
              {{ t('admin.paymentProviders.config.instructions') }}
            </label>
            <textarea
              v-model="(formData.config as any).instructions"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-3 text-sm focus:outline-none transition-all duration-200 resize-none h-28"
              :placeholder="t('admin.paymentProviders.config.instructionsPlaceholder')"
            ></textarea>
          </div>
        </template>

        <!-- 通用额度配置 -->
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div>
            <Input
              v-model.number="formData.minAmount"
              type="number"
              min="0"
              step="0.01"
              :label="t('admin.paymentProviders.minAmount')"
              class="font-mono tabular-nums"
            >
              <template #prefix><span class="text-xs text-[var(--text-tertiary)]">¥</span></template>
            </Input>
          </div>
          <div>
            <Input
              :model-value="formData.maxAmount ?? ''"
              type="number"
              min="0"
              step="0.01"
              :label="t('admin.paymentProviders.maxAmount')"
              :placeholder="t('common.unlimited')"
              class="font-mono tabular-nums"
              @update:model-value="(val: any) => formData.maxAmount = (val === '' || val === null || val === undefined) ? undefined : Number(val)"
            >
              <template #prefix><span class="text-xs text-[var(--text-tertiary)]">¥</span></template>
            </Input>
          </div>
        </div>

        <!-- 费率配置 -->
        <div v-if="formData.type !== 'yipay'" class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div>
            <Input
              v-model.number="formData.feeRate"
              type="number"
              min="0"
              max="100"
              step="0.01"
              :label="t('admin.paymentProviders.feeRate') + ' (%)'"
              class="font-mono tabular-nums"
            >
              <template #suffix><span class="text-xs text-[var(--text-tertiary)]">%</span></template>
            </Input>
          </div>
          <div>
            <Input
              v-model.number="formData.feeFixed"
              type="number"
              min="0"
              step="0.01"
              :label="t('admin.paymentProviders.feeFixed')"
              class="font-mono tabular-nums"
            >
              <template #prefix><span class="text-xs text-[var(--text-tertiary)]">¥</span></template>
            </Input>
          </div>
        </div>
        <div v-else class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-xs text-[var(--text-secondary)] leading-relaxed">
          {{ t('admin.paymentProviders.config.yipayFeeFieldHint') }}
        </div>

        <!-- 排序权重 -->
        <div>
          <Input
            v-model.number="formData.sortOrder"
            type="number"
            min="0"
            :label="t('admin.paymentProviders.sortOrder')"
            class="font-mono tabular-nums"
          />
        </div>
      </div>

      <template #footer>
        <div class="flex items-center justify-end gap-3">
          <Button
            variant="secondary"
            size="sm"
            :disabled="modalLoading"
            @click="showModal = false"
          >
            {{ t('common.cancel') }}
          </Button>
          <Button
            variant="primary"
            size="sm"
            :loading="modalLoading"
            :disabled="modalLoading"
            @click="saveProvider"
          >
            {{ modalLoading ? t('common.saving') : t('common.save') }}
          </Button>
        </div>
      </template>
    </DrawerModal>

    <!-- 删除确认抽屉（DrawerModal） -->
    <DrawerModal
      v-model="showDeleteModal"
      :title="t('admin.paymentProviders.deleteConfirm')"
      max-width="max-w-md"
    >
      <div class="py-2 space-y-3">
        <div class="flex items-start gap-3 p-3.5 rounded-xl border border-[color-mix(in_srgb,var(--error)_24%,var(--border-color))] bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))]">
          <div class="flex h-8 w-8 items-center justify-center rounded-lg bg-[color-mix(in_srgb,var(--error)_15%,var(--bg-surface))] text-[var(--error)] shrink-0">
            <Trash2 class="w-4 h-4" />
          </div>
          <div class="text-xs leading-relaxed text-[var(--text-secondary)]">
            {{ t('admin.paymentProviders.deleteWarning', { name: deleteTarget?.name }) }}
          </div>
        </div>
      </div>

      <template #footer>
        <div class="flex items-center justify-end gap-3">
          <Button
            variant="secondary"
            size="sm"
            :disabled="deleteLoading"
            @click="showDeleteModal = false"
          >
            {{ t('common.cancel') }}
          </Button>
          <Button
            variant="danger"
            size="sm"
            :loading="deleteLoading"
            :disabled="deleteLoading"
            @click="deleteProvider"
          >
            {{ deleteLoading ? t('common.deleting') : t('common.delete') }}
          </Button>
        </div>
      </template>
    </DrawerModal>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  .animate-fade-in {
    animation: none !important;
  }
}
</style>
