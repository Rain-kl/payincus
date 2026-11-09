<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import { translateError } from '@/utils/errorHandler'
import FlagIcon from '@/components/FlagIcon.vue'
import UserAvatar from '@/components/UserAvatar.vue'
import DrawerModal from '@/components/DrawerModal.vue'
import { useTabQuery } from '@/composables/useTabQuery'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  EmptyState,
  Input,
  Select,
  Pagination
} from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import {
  Server,
  Package,
  Users,
  Globe,
  Plus,
  RefreshCw,
  Search,
  Edit3,
  Trash2,
  X,
  Loader2
} from 'lucide-vue-next'

const { t } = useI18n()
const toast = useToast()

// TAB 状态
type MailTab = 'sources' | 'plans' | 'subscriptions' | 'domains'
const activeTab = useTabQuery<MailTab>({
  keys: ['sources', 'plans', 'subscriptions', 'domains'],
  fallback: 'sources',
  onExternalChange: (tab) => loadTabData(tab)
})

// 加载状态
const loading = ref(true)
const actionLoading = ref('')

// 数据
const sources = ref<any[]>([])
const plans = ref<any[]>([])
const subscriptions = ref<any[]>([])
const domains = ref<any[]>([])

// 邮箱源弹窗
const showSourceModal = ref(false)
const editingSource = ref<any>(null)
const sourceForm = ref({
  name: '',
  code: 'us',
  apiUrl: '',
  apiKey: '',
  smarterMailUrl: ''
})

// 方案弹窗
const showPlanModal = ref(false)
const editingPlan = ref<any>(null)
const planForm = ref({
  sourceId: 0,
  name: '',
  description: '',
  domainLimit: 1,
  diskLimitGb: 10,
  price: 0,
  billingCycle: 'monthly' as 'monthly' | 'yearly'
})

// 分页
const subscriptionsPage = ref(1)
const domainsPage = ref(1)
const pageSize = ref(20)
const totalSubscriptions = ref(0)
const totalDomains = ref(0)

// 搜索
const subscriptionsSearch = ref('')
const domainsSearch = ref('')

// 分页计算属性
const subscriptionsTotalPages = computed(() => Math.ceil(totalSubscriptions.value / pageSize.value))
const domainsTotalPages = computed(() => Math.ceil(totalDomains.value / pageSize.value))

const regionSelectOptions = [
  { value: 'us', label: '美国 (US)' },
  { value: 'nl', label: '荷兰 (NL)' }
]

const sourceSelectOptions = computed(() =>
  sources.value.map((s) => ({
    value: s.id,
    label: `${s.name} (${s.code.toUpperCase()})`
  }))
)

const billingCycleOptions = computed(() => [
  { value: 'monthly', label: t('mail.monthly') },
  { value: 'yearly', label: t('mail.yearly') }
])

const tabItems = computed(() => [
  { id: 'sources' as const, label: t('admin.mail.tabs.sources'), icon: Server, count: sources.value.length },
  { id: 'plans' as const, label: t('admin.mail.tabs.plans'), icon: Package, count: plans.value.length },
  { id: 'subscriptions' as const, label: t('admin.mail.tabs.subscriptions'), icon: Users, count: totalSubscriptions.value },
  { id: 'domains' as const, label: t('admin.mail.tabs.domains'), icon: Globe, count: totalDomains.value }
])

onMounted(async () => {
  await loadTabData(activeTab.value)
})

async function loadTabData(tab: MailTab) {
  if (tab === 'sources' && sources.value.length === 0) {
    await loadSources()
  } else if (tab === 'plans') {
    if (sources.value.length === 0) {
      await loadSources()
    }
    if (plans.value.length === 0) {
      await loadPlans()
    }
  } else if (tab === 'subscriptions') {
    subscriptionsPage.value = 1
    subscriptionsSearch.value = ''
    await loadSubscriptions()
  } else if (tab === 'domains') {
    domainsPage.value = 1
    domainsSearch.value = ''
    await loadDomains()
  }
}

async function switchTab(tab: MailTab) {
  activeTab.value = tab
  await loadTabData(tab)
}

// ===== 邮箱源管理 =====
async function loadSources() {
  loading.value = true
  try {
    const res = await api.mail.adminGetSources()
    sources.value = res.sources || []
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    loading.value = false
  }
}

function openCreateSourceModal() {
  editingSource.value = null
  sourceForm.value = {
    name: '',
    code: 'us',
    apiUrl: '',
    apiKey: '',
    smarterMailUrl: ''
  }
  showSourceModal.value = true
}

function openEditSourceModal(source: any) {
  editingSource.value = source
  sourceForm.value = {
    name: source.name,
    code: source.code,
    apiUrl: source.apiUrl || '',
    apiKey: '',
    smarterMailUrl: source.smarterMailUrl || ''
  }
  showSourceModal.value = true
}

async function saveSource() {
  if (!sourceForm.value.name || !sourceForm.value.apiUrl || (!editingSource.value && !sourceForm.value.apiKey)) {
    toast.error(t('admin.mail.fillRequired'))
    return
  }

  actionLoading.value = 'save-source'
  try {
    if (editingSource.value) {
      const payload = { ...sourceForm.value }
      if (!payload.apiKey) {
        delete (payload as Partial<typeof sourceForm.value>).apiKey
      }
      await api.mail.adminUpdateSource(editingSource.value.id, payload)
      toast.success(t('admin.mail.sourceUpdated'))
    } else {
      await api.mail.adminCreateSource(sourceForm.value)
      toast.success(t('admin.mail.sourceCreated'))
    }
    showSourceModal.value = false
    await loadSources()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

async function deleteSource(source: any) {
  if (!confirm(t('admin.mail.confirmDeleteSource', { name: source.name }))) return

  actionLoading.value = `delete-source-${source.id}`
  try {
    await api.mail.adminDeleteSource(source.id)
    toast.success(t('admin.mail.sourceDeleted'))
    await loadSources()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// ===== 方案管理 =====
async function loadPlans() {
  loading.value = true
  try {
    const res = await api.mail.adminGetPlans()
    plans.value = res.plans || []
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    loading.value = false
  }
}

function openCreatePlanModal() {
  if (sources.value.length === 0) {
    toast.error(t('admin.mail.createSourceFirst'))
    return
  }
  editingPlan.value = null
  planForm.value = {
    sourceId: sources.value[0]?.id || 0,
    name: '',
    description: '',
    domainLimit: 1,
    diskLimitGb: 10,
    price: 0,
    billingCycle: 'monthly'
  }
  showPlanModal.value = true
}

function openEditPlanModal(plan: any) {
  editingPlan.value = plan
  planForm.value = {
    sourceId: plan.sourceId,
    name: plan.name,
    description: plan.description || '',
    domainLimit: plan.domainLimit,
    diskLimitGb: plan.diskLimitGb,
    price: plan.price,
    billingCycle: plan.billingCycle
  }
  showPlanModal.value = true
}

async function savePlan() {
  const priceNum = Number(planForm.value.price)
  const domainLimitNum = Number(planForm.value.domainLimit)
  const diskLimitGbNum = Number(planForm.value.diskLimitGb)
  if (!planForm.value.name || !planForm.value.sourceId || priceNum <= 0) {
    toast.error(t('admin.mail.fillRequired'))
    return
  }

  actionLoading.value = 'save-plan'
  try {
    const payload = {
      ...planForm.value,
      price: priceNum,
      domainLimit: domainLimitNum,
      diskLimitGb: diskLimitGbNum
    }
    if (editingPlan.value) {
      await api.mail.adminUpdatePlan(editingPlan.value.id, payload)
      toast.success(t('admin.mail.planUpdated'))
    } else {
      await api.mail.adminCreatePlan(payload)
      toast.success(t('admin.mail.planCreated'))
    }
    showPlanModal.value = false
    await loadPlans()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

async function deletePlan(plan: any) {
  if (!confirm(t('admin.mail.confirmDeletePlan', { name: plan.name }))) return

  actionLoading.value = `delete-plan-${plan.id}`
  try {
    await api.mail.adminDeletePlan(plan.id)
    toast.success(t('admin.mail.planDeleted'))
    await loadPlans()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// ===== 订阅管理 =====
async function loadSubscriptions() {
  loading.value = true
  try {
    const res = await api.mail.adminGetSubscriptions({
      page: subscriptionsPage.value,
      pageSize: pageSize.value,
      search: subscriptionsSearch.value || undefined
    })
    subscriptions.value = res.subscriptions || []
    totalSubscriptions.value = res.total || 0
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    loading.value = false
  }
}

function searchSubscriptions() {
  subscriptionsPage.value = 1
  loadSubscriptions()
}

function goToSubscriptionsPage(newPage: number) {
  if (newPage >= 1 && newPage <= subscriptionsTotalPages.value) {
    subscriptionsPage.value = newPage
    loadSubscriptions()
  }
}

// ===== 域名管理 =====
async function loadDomains() {
  loading.value = true
  try {
    const res = await api.mail.adminGetDomains({
      page: domainsPage.value,
      pageSize: pageSize.value,
      search: domainsSearch.value || undefined
    })
    domains.value = res.domains || []
    totalDomains.value = res.total || 0
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    loading.value = false
  }
}

function searchDomains() {
  domainsPage.value = 1
  loadDomains()
}

function goToDomainsPage(newPage: number) {
  if (newPage >= 1 && newPage <= domainsTotalPages.value) {
    domainsPage.value = newPage
    loadDomains()
  }
}

// ===== 退订管理 =====
const showUnsubModal = ref(false)
const unsubTarget = ref<any>(null)
const unsubForm = ref({
  refundType: 'none' as 'none' | 'full' | 'remaining',
  reason: ''
})

function openUnsubModal(sub: any) {
  unsubTarget.value = sub
  unsubForm.value = { refundType: 'none', reason: '' }
  showUnsubModal.value = true
}

async function handleUnsubscribe() {
  if (!unsubTarget.value) return
  if (unsubForm.value.refundType !== 'none' && !unsubForm.value.reason.trim()) {
    toast.error(t('admin.mail.unsub.reasonRequired'))
    return
  }

  actionLoading.value = 'unsub'
  try {
    const result = await api.mail.adminCancelSubscription(unsubTarget.value.id, {
      refundType: unsubForm.value.refundType,
      reason: unsubForm.value.refundType !== 'none' ? unsubForm.value.reason.trim() : undefined
    })
    if (result.refundAmount > 0) {
      toast.success(t('admin.mail.unsub.successWithRefund', { amount: result.refundAmount.toFixed(2) }))
    } else {
      toast.success(t('admin.mail.unsub.success'))
    }
    showUnsubModal.value = false
    await loadSubscriptions()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

function formatPrice(price: number | string | null | undefined) {
  const num = Number(price) || 0
  return `¥${num.toFixed(2)}`
}

function formatDate(dateStr: string | null | undefined) {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleDateString()
}

function getStatusBadge(status: string): BadgeVariant {
  switch (status) {
    case 'active':
    case 'verified':
      return 'success'
    case 'pending':
      return 'warning'
    case 'expired':
    case 'suspended':
      return 'error'
    default:
      return 'default'
  }
}
</script>

<template>
  <div class="kawaii-page nimbus-view space-y-6 animate-fade-in">
    <!-- Header -->
    <PageHeader
      :title="t('admin.mail.title')"
      :description="t('admin.mail.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadTabData(activeTab)"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" :class="loading ? 'animate-spin' : ''" />
          </template>
          <span>{{ t('common.refresh') || '刷新' }}</span>
        </Button>
        <Button
          v-if="activeTab === 'sources'"
          variant="primary"
          size="sm"
          @click="openCreateSourceModal"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2]" />
          </template>
          <span>{{ t('admin.mail.createSource') }}</span>
        </Button>
        <Button
          v-else-if="activeTab === 'plans'"
          variant="primary"
          size="sm"
          @click="openCreatePlanModal"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2]" />
          </template>
          <span>{{ t('admin.mail.createPlan') }}</span>
        </Button>
      </template>
    </PageHeader>

    <!-- TAB 导航 (Apple 风格分段控制器) -->
    <div class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full w-fit">
      <button
        v-for="tab in tabItems"
        :key="tab.id"
        type="button"
        class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none"
        :class="activeTab === tab.id
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
        @click="switchTab(tab.id)"
      >
        <component :is="tab.icon" class="w-4 h-4 stroke-[1.8]" />
        <span>{{ tab.label }}</span>
        <Badge
          v-if="tab.count !== undefined && tab.count > 0"
          size="sm"
          :variant="activeTab === tab.id ? 'info' : 'default'"
        >
          {{ tab.count }}
        </Badge>
      </button>
    </div>

    <!-- Loading -->
    <Card v-if="loading" padding="lg" class="text-center">
      <div class="flex flex-col items-center justify-center py-12">
        <Loader2 class="w-8 h-8 animate-spin text-[var(--accent)]" />
        <p class="text-sm text-[var(--text-secondary)] mt-4 font-medium">{{ t('common.loading') }}</p>
      </div>
    </Card>

    <!-- 邮箱源 TAB -->
    <div v-else-if="activeTab === 'sources'" class="space-y-4">
      <Card v-if="sources.length === 0" padding="none">
        <EmptyState
          :title="t('admin.mail.noSources')"
          description="暂未配置任何邮箱服务源，点击下方按钮添加第一个发信源"
        >
          <template #icon>
            <Server class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template #action>
            <Button variant="primary" size="sm" @click="openCreateSourceModal">
              <template #icon>
                <Plus class="w-4 h-4 stroke-[2]" />
              </template>
              <span>{{ t('admin.mail.createSource') }}</span>
            </Button>
          </template>
        </EmptyState>
      </Card>

      <template v-else>
        <!-- Mobile Cards -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="source in sources"
            :key="source.id"
            padding="sm"
            class="transition-all duration-200"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="flex min-w-0 items-center gap-2.5">
                <div class="w-9 h-9 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center shrink-0">
                  <FlagIcon :code="source.code" class="w-5 h-5 rounded-sm" />
                </div>
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ source.name }}</div>
                  <div class="flex items-center gap-1.5 mt-0.5">
                    <Badge size="sm" variant="default" shape="rounded">
                      {{ source.code.toUpperCase() }}
                    </Badge>
                  </div>
                </div>
              </div>
              <Badge size="sm" variant="info">
                {{ source.planCount || 0 }} {{ t('admin.mail.plans') }}
              </Badge>
            </div>

            <div class="mt-3.5 space-y-1.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs">
              <div>
                <span class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide block mb-0.5">{{ t('admin.mail.apiEndpoint') }}</span>
                <span class="font-mono text-[var(--text-secondary)] break-all">{{ source.apiUrl }}</span>
              </div>
              <div v-if="source.smarterMailUrl">
                <span class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide block mb-0.5">{{ t('admin.mail.webmailUrl') }}</span>
                <span class="font-mono text-[var(--text-secondary)] break-all">{{ source.smarterMailUrl }}</span>
              </div>
            </div>

            <div class="mt-3.5 flex items-center justify-end gap-2 border-t border-[var(--border-color)] pt-3">
              <Button
                variant="secondary"
                size="sm"
                @click="openEditSourceModal(source)"
              >
                <template #icon>
                  <Edit3 class="w-3.5 h-3.5" />
                </template>
                <span>{{ t('common.edit') }}</span>
              </Button>
              <Button
                variant="ghost"
                size="sm"
                class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                :loading="actionLoading === `delete-source-${source.id}`"
                @click="deleteSource(source)"
              >
                <template #icon>
                  <Trash2 class="w-3.5 h-3.5" />
                </template>
                <span>{{ t('common.delete') }}</span>
              </Button>
            </div>
          </Card>
        </div>

        <!-- Desktop Table -->
        <div class="card hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <colgroup>
              <col class="w-[14%]" />
              <col class="w-[22%]" />
              <col class="w-[36%]" />
              <col class="w-[12%]" />
              <col class="w-[16%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider select-none">
                <th class="px-4 py-3.5">{{ t('admin.mail.region') }}</th>
                <th class="px-4 py-3.5">{{ t('common.name') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.apiEndpoint') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.plans') }}</th>
                <th class="px-4 py-3.5 text-right">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="source in sources"
                :key="source.id"
                class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
              >
                <td class="px-4 py-3.5">
                  <div class="flex items-center gap-2">
                    <FlagIcon :code="source.code" class="w-5 h-5 rounded-sm" />
                    <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-primary)] font-mono">
                      {{ source.code.toUpperCase() }}
                    </span>
                  </div>
                </td>
                <td class="px-4 py-3.5 text-sm font-medium text-[var(--text-primary)] truncate" :title="source.name">
                  {{ source.name }}
                </td>
                <td class="px-4 py-3.5 font-mono text-xs text-[var(--text-secondary)] truncate" :title="source.apiUrl">
                  {{ source.apiUrl }}
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] tabular-nums">
                  <Badge size="sm" variant="info">
                    {{ source.planCount || 0 }} {{ t('admin.mail.plans') }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 text-right whitespace-nowrap">
                  <div class="inline-flex items-center justify-end gap-1">
                    <Button
                      variant="ghost"
                      size="sm"
                      @click="openEditSourceModal(source)"
                    >
                      <template #icon>
                        <Edit3 class="w-3.5 h-3.5" />
                      </template>
                      <span>{{ t('common.edit') }}</span>
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                      :loading="actionLoading === `delete-source-${source.id}`"
                      @click="deleteSource(source)"
                    >
                      <template #icon>
                        <Trash2 class="w-3.5 h-3.5" />
                      </template>
                      <span>{{ t('common.delete') }}</span>
                    </Button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>

    <!-- 方案 TAB -->
    <div v-else-if="activeTab === 'plans'" class="space-y-4">
      <Card v-if="plans.length === 0" padding="none">
        <EmptyState
          :title="t('admin.mail.noPlans')"
          description="暂未配置任何邮箱方案套餐，点击下方按钮添加"
        >
          <template #icon>
            <Package class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template #action>
            <Button variant="primary" size="sm" @click="openCreatePlanModal">
              <template #icon>
                <Plus class="w-4 h-4 stroke-[2]" />
              </template>
              <span>{{ t('admin.mail.createPlan') }}</span>
            </Button>
          </template>
        </EmptyState>
      </Card>

      <template v-else>
        <!-- Mobile Cards -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="plan in plans"
            :key="plan.id"
            padding="sm"
            class="transition-all duration-200"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ plan.name }}</div>
                <div v-if="plan.description" class="mt-1 line-clamp-2 text-xs text-[var(--text-secondary)]">{{ plan.description }}</div>
              </div>
              <div class="shrink-0 text-right text-sm font-semibold text-[var(--text-primary)]">
                {{ formatPrice(plan.price) }}<span class="text-xs font-normal text-[var(--text-secondary)]">/{{ plan.billingCycle === 'monthly' ? t('mail.month') : t('mail.year') }}</span>
              </div>
            </div>

            <div class="mt-3.5 grid grid-cols-3 gap-2 text-xs">
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.mail.source') }}</div>
                <div class="truncate text-[var(--text-primary)] font-medium">{{ plan.source?.name || '-' }}</div>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.mail.domainLimit') }}</div>
                <div class="text-[var(--text-primary)] font-medium">{{ plan.domainLimit }}</div>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.mail.diskLimit') }}</div>
                <div class="text-[var(--text-primary)] font-medium">{{ plan.diskLimitGb }} GB</div>
              </div>
            </div>

            <div class="mt-3.5 flex items-center justify-end gap-2 border-t border-[var(--border-color)] pt-3">
              <Button
                variant="secondary"
                size="sm"
                @click="openEditPlanModal(plan)"
              >
                <template #icon>
                  <Edit3 class="w-3.5 h-3.5" />
                </template>
                <span>{{ t('common.edit') }}</span>
              </Button>
              <Button
                variant="ghost"
                size="sm"
                class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                :loading="actionLoading === `delete-plan-${plan.id}`"
                @click="deletePlan(plan)"
              >
                <template #icon>
                  <Trash2 class="w-3.5 h-3.5" />
                </template>
                <span>{{ t('common.delete') }}</span>
              </Button>
            </div>
          </Card>
        </div>

        <!-- Desktop Table -->
        <div class="card hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <colgroup>
              <col class="w-[26%]" />
              <col class="w-[18%]" />
              <col class="w-[14%]" />
              <col class="w-[14%]" />
              <col class="w-[14%]" />
              <col class="w-[14%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider select-none">
                <th class="px-4 py-3.5">{{ t('common.name') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.source') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.domainLimit') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.diskLimit') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.price') }}</th>
                <th class="px-4 py-3.5 text-right">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="plan in plans"
                :key="plan.id"
                class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
              >
                <td class="px-4 py-3.5">
                  <div class="text-sm font-semibold text-[var(--text-primary)]">{{ plan.name }}</div>
                  <div v-if="plan.description" class="text-xs text-[var(--text-secondary)] truncate mt-0.5">{{ plan.description }}</div>
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] truncate">
                  {{ plan.source?.name || '-' }}
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] tabular-nums">
                  {{ plan.domainLimit }}
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] tabular-nums">
                  {{ plan.diskLimitGb }} GB
                </td>
                <td class="px-4 py-3.5 text-sm font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatPrice(plan.price) }}<span class="text-xs font-normal text-[var(--text-secondary)]">/{{ plan.billingCycle === 'monthly' ? t('mail.month') : t('mail.year') }}</span>
                </td>
                <td class="px-4 py-3.5 text-right whitespace-nowrap">
                  <div class="inline-flex items-center justify-end gap-1">
                    <Button
                      variant="ghost"
                      size="sm"
                      @click="openEditPlanModal(plan)"
                    >
                      <template #icon>
                        <Edit3 class="w-3.5 h-3.5" />
                      </template>
                      <span>{{ t('common.edit') }}</span>
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                      :loading="actionLoading === `delete-plan-${plan.id}`"
                      @click="deletePlan(plan)"
                    >
                      <template #icon>
                        <Trash2 class="w-3.5 h-3.5" />
                      </template>
                      <span>{{ t('common.delete') }}</span>
                    </Button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>

    <!-- 订阅 TAB -->
    <div v-else-if="activeTab === 'subscriptions'" class="space-y-4">
      <!-- 搜索框 -->
      <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div class="flex items-center gap-2 max-w-sm w-full">
          <Input
            v-model="subscriptionsSearch"
            :placeholder="t('admin.mail.searchSubscriptions')"
            clearable
            size="sm"
            class="flex-1"
            @enter="searchSubscriptions"
            @clear="searchSubscriptions"
          />
          <Button
            variant="secondary"
            size="sm"
            @click="searchSubscriptions"
          >
            <template #icon>
              <Search class="w-3.5 h-3.5" />
            </template>
            <span>{{ t('common.search') }}</span>
          </Button>
        </div>
      </div>

      <!-- 无数据/搜索无结果 -->
      <Card v-if="subscriptions.length === 0" padding="none">
        <EmptyState
          :title="subscriptionsSearch ? t('common.noSearchResults') : t('admin.mail.noSubscriptions')"
          :description="subscriptionsSearch ? '没有找到匹配的订阅记录，请尝试其他关键词' : '当前暂无用户邮箱订阅'"
        >
          <template #icon>
            <Users class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template v-if="subscriptionsSearch" #action>
            <Button variant="secondary" size="sm" @click="subscriptionsSearch = ''; searchSubscriptions()">
              重置搜索
            </Button>
          </template>
        </EmptyState>
      </Card>

      <template v-else>
        <!-- Mobile Cards -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="sub in subscriptions"
            :key="sub.id"
            padding="sm"
            class="transition-all duration-200"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="flex min-w-0 items-center gap-2.5">
                <UserAvatar
                  :username="sub.user?.username || ''"
                  :email="sub.user?.email"
                  :avatar-style="sub.user?.avatarStyle"
                  :badge-id="sub.user?.avatarBadgeId || null"
                  :size="36"
                />
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ sub.user?.username || '-' }}</div>
                  <div class="truncate text-xs text-[var(--text-secondary)] mt-0.5">ID: {{ sub.user?.id || '-' }} · {{ sub.user?.email || '-' }}</div>
                </div>
              </div>
              <Badge size="sm" :variant="getStatusBadge(sub.status)">
                {{ t('mail.status.' + sub.status) }}
              </Badge>
            </div>

            <div class="mt-3.5 grid grid-cols-3 gap-2 text-xs">
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.mail.plan') }}</div>
                <div class="truncate text-[var(--text-primary)] font-medium">{{ sub.plan?.name || '-' }}</div>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.mail.expiresAt') }}</div>
                <div class="text-[var(--text-primary)] font-mono text-[11px]">{{ formatDate(sub.expiresAt) }}</div>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('common.createdAt') }}</div>
                <div class="text-[var(--text-secondary)] font-mono text-[11px]">{{ formatDate(sub.createdAt) }}</div>
              </div>
            </div>

            <div class="mt-3.5 flex items-center justify-end border-t border-[var(--border-color)] pt-3">
              <Button
                variant="ghost"
                size="sm"
                class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                :loading="actionLoading === 'unsub'"
                @click="openUnsubModal(sub)"
              >
                <span>{{ t('admin.mail.unsub.button') }}</span>
              </Button>
            </div>
          </Card>
        </div>

        <!-- Desktop Table -->
        <div class="card hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <colgroup>
              <col class="w-[30%]" />
              <col class="w-[18%]" />
              <col class="w-[12%]" />
              <col class="w-[14%]" />
              <col class="w-[14%]" />
              <col class="w-[12%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider select-none">
                <th class="px-4 py-3.5">{{ t('admin.mail.user') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.plan') }}</th>
                <th class="px-4 py-3.5">{{ t('common.status') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.expiresAt') }}</th>
                <th class="px-4 py-3.5">{{ t('common.createdAt') }}</th>
                <th class="px-4 py-3.5 text-right">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="sub in subscriptions"
                :key="sub.id"
                class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
              >
                <td class="px-4 py-3.5">
                  <div class="flex items-center gap-3">
                    <UserAvatar
                      :username="sub.user?.username || ''"
                      :email="sub.user?.email"
                      :avatar-style="sub.user?.avatarStyle"
                      :badge-id="sub.user?.avatarBadgeId || null"
                      :size="32"
                    />
                    <div class="min-w-0">
                      <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ sub.user?.username || '-' }}</div>
                      <div class="truncate text-xs text-[var(--text-secondary)]">ID: {{ sub.user?.id || '-' }} · {{ sub.user?.email || '-' }}</div>
                    </div>
                  </div>
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] truncate">
                  {{ sub.plan?.name || '-' }}
                </td>
                <td class="px-4 py-3.5">
                  <Badge size="sm" :variant="getStatusBadge(sub.status)">
                    {{ t('mail.status.' + sub.status) }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] tabular-nums font-mono">
                  {{ formatDate(sub.expiresAt) }}
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-secondary)] tabular-nums font-mono">
                  {{ formatDate(sub.createdAt) }}
                </td>
                <td class="px-4 py-3.5 text-right whitespace-nowrap">
                  <Button
                    variant="ghost"
                    size="sm"
                    class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                    :loading="actionLoading === 'unsub'"
                    @click="openUnsubModal(sub)"
                  >
                    <span>{{ t('admin.mail.unsub.button') }}</span>
                  </Button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 订阅分页 -->
        <Card v-if="totalSubscriptions > 0" padding="sm">
          <Pagination
            :page="subscriptionsPage"
            :page-size="pageSize"
            :total="totalSubscriptions"
            :show-total="true"
            @update:page="goToSubscriptionsPage"
          />
        </Card>
      </template>
    </div>

    <!-- 域名 TAB -->
    <div v-else-if="activeTab === 'domains'" class="space-y-4">
      <!-- 搜索框 -->
      <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div class="flex items-center gap-2 max-w-sm w-full">
          <Input
            v-model="domainsSearch"
            :placeholder="t('admin.mail.searchDomains')"
            clearable
            size="sm"
            class="flex-1"
            @enter="searchDomains"
            @clear="searchDomains"
          />
          <Button
            variant="secondary"
            size="sm"
            @click="searchDomains"
          >
            <template #icon>
              <Search class="w-3.5 h-3.5" />
            </template>
            <span>{{ t('common.search') }}</span>
          </Button>
        </div>
      </div>

      <!-- 无数据/搜索无结果 -->
      <Card v-if="domains.length === 0" padding="none">
        <EmptyState
          :title="domainsSearch ? t('common.noSearchResults') : t('admin.mail.noDomains')"
          :description="domainsSearch ? '没有找到匹配的域名，请尝试其他关键词' : '当前暂无用户绑定的邮箱域名'"
        >
          <template #icon>
            <Globe class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template v-if="domainsSearch" #action>
            <Button variant="secondary" size="sm" @click="domainsSearch = ''; searchDomains()">
              重置搜索
            </Button>
          </template>
        </EmptyState>
      </Card>

      <template v-else>
        <!-- Mobile Cards -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="domain in domains"
            :key="domain.id"
            padding="sm"
            class="transition-all duration-200"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="break-all font-mono text-sm font-semibold text-[var(--text-primary)]">{{ domain.domain }}</div>
                <div class="mt-1 text-xs text-[var(--text-secondary)] font-mono">{{ t('common.createdAt') }}: {{ formatDate(domain.createdAt) }}</div>
              </div>
              <Badge size="sm" :variant="getStatusBadge(domain.status)">
                {{ t('mail.domainStatus.' + domain.status) }}
              </Badge>
            </div>

            <div class="mt-3.5 flex items-center justify-between gap-3 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
              <div class="flex min-w-0 items-center gap-2.5">
                <UserAvatar
                  :username="domain.subscription?.user?.username || ''"
                  :email="domain.subscription?.user?.email"
                  :avatar-style="domain.subscription?.user?.avatarStyle"
                  :badge-id="domain.subscription?.user?.avatarBadgeId || null"
                  :size="30"
                />
                <div class="min-w-0">
                  <div class="truncate text-xs font-semibold text-[var(--text-primary)]">{{ domain.subscription?.user?.username || '-' }}</div>
                  <div class="truncate text-[11px] text-[var(--text-secondary)]">ID: {{ domain.subscription?.user?.id || '-' }}</div>
                </div>
              </div>
              <Badge size="sm" variant="default" shape="rounded">
                {{ domain._count?.accounts || 0 }} {{ t('admin.mail.accounts') }}
              </Badge>
            </div>
          </Card>
        </div>

        <!-- Desktop Table -->
        <div class="card hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <colgroup>
              <col class="w-[28%]" />
              <col class="w-[30%]" />
              <col class="w-[14%]" />
              <col class="w-[12%]" />
              <col class="w-[16%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider select-none">
                <th class="px-4 py-3.5">{{ t('admin.mail.domain') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.user') }}</th>
                <th class="px-4 py-3.5">{{ t('common.status') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.mail.accounts') }}</th>
                <th class="px-4 py-3.5">{{ t('common.createdAt') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="domain in domains"
                :key="domain.id"
                class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
              >
                <td class="px-4 py-3.5 font-mono text-sm font-semibold text-[var(--text-primary)] truncate" :title="domain.domain">
                  {{ domain.domain }}
                </td>
                <td class="px-4 py-3.5">
                  <div class="flex items-center gap-3">
                    <UserAvatar
                      :username="domain.subscription?.user?.username || ''"
                      :email="domain.subscription?.user?.email"
                      :avatar-style="domain.subscription?.user?.avatarStyle"
                      :badge-id="domain.subscription?.user?.avatarBadgeId || null"
                      :size="32"
                    />
                    <div class="min-w-0">
                      <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ domain.subscription?.user?.username || '-' }}</div>
                      <div class="truncate text-xs text-[var(--text-secondary)]">ID: {{ domain.subscription?.user?.id || '-' }} · {{ domain.subscription?.user?.email || '-' }}</div>
                    </div>
                  </div>
                </td>
                <td class="px-4 py-3.5">
                  <Badge size="sm" :variant="getStatusBadge(domain.status)">
                    {{ t('mail.domainStatus.' + domain.status) }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-primary)] tabular-nums">
                  <Badge size="sm" variant="default" shape="rounded">
                    {{ domain._count?.accounts || 0 }} {{ t('admin.mail.accounts') }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 text-sm text-[var(--text-secondary)] tabular-nums font-mono">
                  {{ formatDate(domain.createdAt) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 域名分页 -->
        <Card v-if="totalDomains > 0" padding="sm">
          <Pagination
            :page="domainsPage"
            :page-size="pageSize"
            :total="totalDomains"
            :show-total="true"
            @update:page="goToDomainsPage"
          />
        </Card>
      </template>
    </div>

    <!-- 邮箱源弹窗 -->
    <DrawerModal
      :show="showSourceModal"
      max-width="max-w-lg"
      raw
      @close="showSourceModal = false"
    >
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface)]">
        <div class="flex items-center gap-2.5 min-w-0">
          <div class="w-8 h-8 rounded-full bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0">
            <Server class="w-4 h-4 stroke-[2]" />
          </div>
          <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ editingSource ? t('admin.mail.editSource') : t('admin.mail.createSource') }}
          </h3>
        </div>
        <button
          type="button"
          class="w-8 h-8 inline-flex items-center justify-center rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
          :aria-label="t('common.cancel')"
          @click="showSourceModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <form class="flex-1 flex flex-col min-h-0 bg-[var(--bg-surface)]" @submit.prevent="saveSource">
        <div class="p-6 space-y-4 overflow-y-auto flex-1">
          <Input
            v-model="sourceForm.name"
            :label="`${t('common.name')} *`"
            :placeholder="t('admin.mail.sourcePlaceholder')"
          />

          <Select
            v-model="sourceForm.code"
            :label="`${t('admin.mail.region')} *`"
            :options="regionSelectOptions"
          />

          <Input
            v-model="sourceForm.apiUrl"
            :label="`${t('admin.mail.apiEndpoint')} *`"
            placeholder="https://api.cranemail.com"
          />

          <Input
            v-model="sourceForm.apiKey"
            type="password"
            :label="t('admin.mail.apiKey')"
            :placeholder="editingSource ? '留空表示不更改 API Key' : ''"
          />

          <Input
            v-model="sourceForm.smarterMailUrl"
            :label="t('admin.mail.webmailUrl')"
            placeholder="https://smartermail.example.com"
          />
        </div>

        <div class="flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
          <Button type="button" variant="secondary" @click="showSourceModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button
            type="submit"
            variant="primary"
            :loading="actionLoading === 'save-source'"
          >
            {{ t('common.save') }}
          </Button>
        </div>
      </form>
    </DrawerModal>

    <!-- 方案弹窗 -->
    <DrawerModal
      :show="showPlanModal"
      max-width="max-w-lg"
      raw
      @close="showPlanModal = false"
    >
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface)]">
        <div class="flex items-center gap-2.5 min-w-0">
          <div class="w-8 h-8 rounded-full bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0">
            <Package class="w-4 h-4 stroke-[2]" />
          </div>
          <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ editingPlan ? t('admin.mail.editPlan') : t('admin.mail.createPlan') }}
          </h3>
        </div>
        <button
          type="button"
          class="w-8 h-8 inline-flex items-center justify-center rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
          :aria-label="t('common.cancel')"
          @click="showPlanModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <form class="flex-1 flex flex-col min-h-0 bg-[var(--bg-surface)]" @submit.prevent="savePlan">
        <div class="p-6 space-y-4 overflow-y-auto flex-1">
          <Select
            v-model="planForm.sourceId"
            :label="`${t('admin.mail.source')} *`"
            :options="sourceSelectOptions"
          />

          <Input
            v-model="planForm.name"
            :label="`${t('common.name')} *`"
            :placeholder="t('admin.mail.planPlaceholder')"
          />

          <div class="space-y-1.5">
            <label class="block text-xs font-medium text-[var(--text-secondary)]">
              {{ t('common.description') }}
            </label>
            <textarea
              v-model="planForm.description"
              rows="2"
              class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/20 transition-all resize-none"
            ></textarea>
          </div>

          <div class="grid grid-cols-2 gap-4">
            <Input
              v-model="planForm.domainLimit"
              type="number"
              :label="`${t('admin.mail.domainLimit')} *`"
            />
            <Input
              v-model="planForm.diskLimitGb"
              type="number"
              :label="`${t('admin.mail.diskLimitGb')} *`"
            />
          </div>

          <div class="grid grid-cols-2 gap-4">
            <Input
              v-model="planForm.price"
              type="number"
              :label="`${t('admin.mail.price')} (¥) *`"
            />
            <Select
              v-model="planForm.billingCycle"
              :label="`${t('admin.mail.billingCycle')} *`"
              :options="billingCycleOptions"
            />
          </div>
        </div>

        <div class="flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
          <Button type="button" variant="secondary" @click="showPlanModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button
            type="submit"
            variant="primary"
            :loading="actionLoading === 'save-plan'"
          >
            {{ t('common.save') }}
          </Button>
        </div>
      </form>
    </DrawerModal>

    <!-- 退订弹窗 -->
    <DrawerModal
      :show="showUnsubModal"
      max-width="max-w-lg"
      raw
      @close="showUnsubModal = false"
    >
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface)]">
        <div class="flex items-center gap-2.5 min-w-0">
          <div class="w-8 h-8 rounded-full bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] flex items-center justify-center shrink-0">
            <Users class="w-4 h-4 stroke-[2]" />
          </div>
          <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ t('admin.mail.unsub.title') }}
          </h3>
        </div>
        <button
          type="button"
          class="w-8 h-8 inline-flex items-center justify-center rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
          :aria-label="t('common.cancel')"
          @click="showUnsubModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div v-if="unsubTarget" class="p-6 space-y-4 overflow-y-auto flex-1 bg-[var(--bg-surface)]">
        <!-- 订阅信息 -->
        <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4">
          <div class="text-sm space-y-2.5">
            <div class="flex justify-between items-center">
              <span class="text-xs text-[var(--text-secondary)]">{{ t('admin.mail.user') }}</span>
              <span class="text-sm font-semibold text-[var(--text-primary)]">{{ unsubTarget.user?.username }} (ID: {{ unsubTarget.user?.id }})</span>
            </div>
            <div class="flex justify-between items-center">
              <span class="text-xs text-[var(--text-secondary)]">{{ t('admin.mail.plan') }}</span>
              <span class="text-sm text-[var(--text-primary)] font-medium">{{ unsubTarget.plan?.name }}</span>
            </div>
            <div class="flex justify-between items-center">
              <span class="text-xs text-[var(--text-secondary)]">{{ t('admin.mail.price') }}</span>
              <span class="text-sm text-[var(--text-primary)] font-semibold">{{ formatPrice(unsubTarget.plan?.price) }}</span>
            </div>
            <div class="flex justify-between items-center">
              <span class="text-xs text-[var(--text-secondary)]">{{ t('admin.mail.expiresAt') }}</span>
              <span class="text-sm text-[var(--text-primary)] font-mono">{{ formatDate(unsubTarget.expiresAt) }}</span>
            </div>
          </div>
        </div>

        <!-- 退款方式 -->
        <div>
          <span class="block text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider mb-2.5">{{ t('admin.mail.unsub.refundType') }}</span>
          <div class="space-y-2">
            <label
              class="flex items-start gap-3 p-3.5 rounded-xl border transition-all cursor-pointer select-none"
              :class="unsubForm.refundType === 'none'
                ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))]'
                : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:bg-[var(--bg-surface)]'"
            >
              <input v-model="unsubForm.refundType" type="radio" value="none" class="w-4 h-4 mt-0.5 text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer" />
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.mail.unsub.refundNone') }}</div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.mail.unsub.refundNoneDesc') }}</div>
              </div>
            </label>
            <label
              class="flex items-start gap-3 p-3.5 rounded-xl border transition-all cursor-pointer select-none"
              :class="unsubForm.refundType === 'full'
                ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))]'
                : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:bg-[var(--bg-surface)]'"
            >
              <input v-model="unsubForm.refundType" type="radio" value="full" class="w-4 h-4 mt-0.5 text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer" />
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.mail.unsub.refundFull') }}</div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.mail.unsub.refundFullDesc', { amount: formatPrice(unsubTarget.plan?.price) }) }}</div>
              </div>
            </label>
            <label
              class="flex items-start gap-3 p-3.5 rounded-xl border transition-all cursor-pointer select-none"
              :class="unsubForm.refundType === 'remaining'
                ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))]'
                : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:bg-[var(--bg-surface)]'"
            >
              <input v-model="unsubForm.refundType" type="radio" value="remaining" class="w-4 h-4 mt-0.5 text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer" />
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.mail.unsub.refundRemaining') }}</div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.mail.unsub.refundRemainingDesc') }}</div>
              </div>
            </label>
          </div>
        </div>

        <!-- 退款原因 -->
        <div v-if="unsubForm.refundType !== 'none'" class="space-y-1.5">
          <label class="block text-xs font-medium text-[var(--text-secondary)]">
            {{ t('admin.mail.unsub.reason') }} *
          </label>
          <textarea
            v-model="unsubForm.reason"
            rows="3"
            class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/20 transition-all resize-none"
            :placeholder="t('admin.mail.unsub.reasonPlaceholder')"
          ></textarea>
        </div>
      </div>

      <div class="flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
        <Button type="button" variant="secondary" @click="showUnsubModal = false">
          {{ t('common.cancel') }}
        </Button>
        <Button
          type="button"
          variant="danger"
          :loading="actionLoading === 'unsub'"
          @click="handleUnsubscribe"
        >
          {{ t('admin.mail.unsub.confirm') }}
        </Button>
      </div>
    </DrawerModal>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  .nimbus-view *,
  .nimbus-view *::before,
  .nimbus-view *::after {
    transition-duration: 0.001ms !important;
    animation-duration: 0.001ms !important;
  }
}
</style>
