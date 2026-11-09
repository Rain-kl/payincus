<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Check,
  CheckCircle2,
  Coins,
  Copy,
  CreditCard,
  Gift,
  Plus,
  Power,
  PowerOff,
  RotateCw,
  Sparkles,
  Trash2
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import type { GiftCardRecord, GiftCardStats, GiftCardStatus } from '@/types/api'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select
} from '@/components/ui'
import { useReveal } from '@/composables/useReveal'

defineOptions({ name: 'AdminGiftCardsView' })

const { t, locale } = useI18n()
const toast = useToast()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const records = ref<GiftCardRecord[]>([])
const stats = ref<GiftCardStats | null>(null)
const loading = ref(false)
const saving = ref(false)
const selectedIds = ref<Set<number>>(new Set())
const statusFilter = ref<GiftCardStatus | ''>('')
const revealCode = ref(false)
const batchResult = ref<{ batchId: string; codes: GiftCardRecord[] } | null>(null)
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

const copiedId = ref<number | null>(null)
let copyTimer: ReturnType<typeof setTimeout> | null = null

const form = ref({
  faceValue: 10,
  balanceValue: undefined as number | undefined,
  count: 1,
  expiresAt: '',
  remark: ''
})

const selectedCount = computed(() => selectedIds.value.size)

const statusOptions = computed<Array<{ value: GiftCardStatus | ''; label: string }>>(() => [
  { value: '', label: t('giftCards.status.all') },
  { value: 'active', label: t('giftCards.status.active') },
  { value: 'used', label: t('giftCards.status.used') },
  { value: 'disabled', label: t('giftCards.status.disabled') },
  { value: 'expired', label: t('giftCards.status.expired') }
])

const isAllSelected = computed(() => {
  return records.value.length > 0 && records.value.every(card => selectedIds.value.has(card.id))
})

const isIndeterminate = computed(() => {
  const selectedInCurrent = records.value.filter(card => selectedIds.value.has(card.id)).length
  return selectedInCurrent > 0 && selectedInCurrent < records.value.length
})

function formatMoney(value: number): string {
  return new Intl.NumberFormat(locale.value, {
    style: 'currency',
    currency: 'CNY',
    maximumFractionDigits: 2
  }).format(value || 0)
}

function formatDate(value?: string | null): string {
  if (!value) return t('giftCards.neverExpires')
  return new Date(value).toLocaleString(locale.value)
}

function statusLabel(status: GiftCardStatus): string {
  return t(`giftCards.status.${status}`)
}

function statusVariant(status: GiftCardStatus): 'success' | 'warning' | 'error' | 'default' {
  switch (status) {
    case 'active':
      return 'success'
    case 'disabled':
      return 'warning'
    case 'expired':
      return 'error'
    case 'used':
    default:
      return 'default'
  }
}

async function loadData(): Promise<void> {
  loading.value = true
  try {
    const [listResponse, statsResponse] = await Promise.all([
      api.giftCards.list({
        page: page.value,
        pageSize: pageSize.value,
        status: statusFilter.value || undefined,
        revealCode: revealCode.value
      }),
      api.giftCards.stats()
    ])
    records.value = listResponse.records
    total.value = listResponse.total
    stats.value = statsResponse
    selectedIds.value = new Set()
  } catch (err: any) {
    toast.error(t('giftCardsAdmin.toast.loadFailed', { message: err?.message || String(err) }))
  } finally {
    loading.value = false
  }
}

async function handleFilterChange(): Promise<void> {
  page.value = 1
  await loadData()
}

async function createCards(): Promise<void> {
  const count = Number(form.value.count)
  const faceValue = Number(form.value.faceValue)
  const balanceValue = form.value.balanceValue === null || form.value.balanceValue === undefined || (form.value.balanceValue as unknown as string) === ''
    ? undefined
    : Number(form.value.balanceValue)
  if (!Number.isFinite(faceValue) || faceValue <= 0 || !Number.isSafeInteger(count) || count < 1) {
    toast.warning(t('giftCardsAdmin.toast.invalidCreateForm'))
    return
  }

  saving.value = true
  try {
    const payload = {
      faceValue,
      balanceValue,
      expiresAt: form.value.expiresAt ? new Date(form.value.expiresAt).toISOString() : null,
      remark: form.value.remark.trim() || undefined
    }
    if (count === 1) {
      const response = await api.giftCards.generate(payload)
      batchResult.value = { batchId: '', codes: [response.giftCard] }
      toast.success(t('giftCardsAdmin.toast.created'))
    } else {
      const response = await api.giftCards.batch({ ...payload, count })
      batchResult.value = { batchId: response.batchId, codes: response.codes }
      toast.success(t('giftCardsAdmin.toast.batchCreated', { count: response.count }))
    }
    page.value = 1
    await loadData()
  } catch (err: any) {
    toast.error(t('giftCardsAdmin.toast.createFailed', { message: err?.message || String(err) }))
  } finally {
    saving.value = false
  }
}

async function updateCardStatus(card: GiftCardRecord): Promise<void> {
  try {
    if (card.status === 'disabled') {
      await api.giftCards.enable(card.id)
      toast.success(t('giftCardsAdmin.toast.enabled'))
    } else {
      await api.giftCards.disable(card.id)
      toast.success(t('giftCardsAdmin.toast.disabled'))
    }
    await loadData()
  } catch (err: any) {
    toast.error(t('giftCardsAdmin.toast.updateFailed', { message: err?.message || String(err) }))
  }
}

async function deleteCard(card: GiftCardRecord): Promise<void> {
  if (!confirm(t('giftCardsAdmin.confirm.deleteOne', { code: card.codeMasked || card.code }))) return
  try {
    await api.giftCards.delete(card.id)
    toast.success(t('giftCardsAdmin.toast.deleted'))
    await loadData()
  } catch (err: any) {
    toast.error(t('giftCardsAdmin.toast.deleteFailed', { message: err?.message || String(err) }))
  }
}

async function batchDisable(): Promise<void> {
  const ids = Array.from(selectedIds.value)
  if (ids.length === 0 || !confirm(t('giftCardsAdmin.confirm.disableSelected', { count: ids.length }))) return
  try {
    const response = await api.giftCards.batchDisable(ids)
    toast.success(t('giftCardsAdmin.toast.batchDisabled', { count: response.count }))
    await loadData()
  } catch (err: any) {
    toast.error(t('giftCardsAdmin.toast.batchDisableFailed', { message: err?.message || String(err) }))
  }
}

async function batchDelete(): Promise<void> {
  const ids = Array.from(selectedIds.value)
  if (ids.length === 0 || !confirm(t('giftCardsAdmin.confirm.deleteSelected', { count: ids.length }))) return
  try {
    const response = await api.giftCards.batchDelete(ids)
    toast.success(t('giftCardsAdmin.toast.batchDeleted', { count: response.count }))
    await loadData()
  } catch (err: any) {
    toast.error(t('giftCardsAdmin.toast.batchDeleteFailed', { message: err?.message || String(err) }))
  }
}

function toggleSelected(id: number, checked: boolean): void {
  const next = new Set(selectedIds.value)
  if (checked) next.add(id)
  else next.delete(id)
  selectedIds.value = next
}

function toggleSelectAll(checked: boolean): void {
  const next = new Set(selectedIds.value)
  if (checked) {
    for (const card of records.value) {
      next.add(card.id)
    }
  } else {
    for (const card of records.value) {
      next.delete(card.id)
    }
  }
  selectedIds.value = next
}

async function copySingleCode(code: string, id: number): Promise<void> {
  try {
    await navigator.clipboard.writeText(code)
    copiedId.value = id
    if (copyTimer) clearTimeout(copyTimer)
    copyTimer = setTimeout(() => {
      copiedId.value = null
    }, 2000)
    toast.success(t('giftCards.toast.copied'))
  } catch {
    toast.error('复制失败')
  }
}

async function copyCodes(codes: GiftCardRecord[]): Promise<void> {
  await navigator.clipboard.writeText(codes.map(card => card.code).join('\n'))
  toast.success(t('giftCards.toast.copied'))
}

onMounted(loadData)
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- 页面头部 -->
    <PageHeader
      :title="t('giftCardsAdmin.title')"
      :description="t('giftCardsAdmin.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadData"
        >
          <template #icon>
            <RotateCw class="w-4 h-4" :class="{ 'animate-spin': loading }" />
          </template>
          {{ loading ? t('giftCards.loading') : t('giftCards.refresh') }}
        </Button>
      </template>
    </PageHeader>

    <!-- 统计指标卡 -->
    <div data-reveal class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('giftCardsAdmin.stats.active') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <CheckCircle2 class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--success)]">
          {{ stats ? stats.active : 0 }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('giftCardsAdmin.stats.used') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Gift class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ stats ? stats.used : 0 }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('giftCardsAdmin.stats.outstanding') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <CreditCard class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--warning)]">
          {{ formatMoney(stats ? stats.outstandingValue : 0) }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('giftCardsAdmin.stats.redeemed') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Coins class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ formatMoney(stats ? stats.totalRedeemedValue : 0) }}
        </div>
      </Card>
    </div>

    <!-- 生成礼品卡表单 -->
    <Card data-reveal padding="md">
      <template #header>
        <div class="flex items-center gap-2.5">
          <div class="flex h-8 w-8 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
            <Sparkles class="w-4 h-4" />
          </div>
          <div>
            <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('giftCardsAdmin.generateTitle') }}
            </h2>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ t('giftCards.description') }}
            </p>
          </div>
        </div>
      </template>

      <div class="space-y-4">
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
          <div>
            <Input
              v-model.number="form.faceValue"
              type="number"
              :label="t('giftCardsAdmin.faceValueLabel')"
              :placeholder="t('giftCardsAdmin.faceValuePlaceholder')"
              class="font-mono"
            />
          </div>
          <div>
            <Input
              v-model.number="form.balanceValue"
              type="number"
              :label="t('giftCardsAdmin.balanceValueLabel')"
              :placeholder="t('giftCardsAdmin.balanceValuePlaceholder')"
              class="font-mono"
            />
          </div>
          <div>
            <Input
              v-model.number="form.count"
              type="number"
              :label="t('giftCardsAdmin.countLabel')"
              :placeholder="t('giftCardsAdmin.countPlaceholder')"
              class="font-mono"
            />
          </div>
          <div>
            <Input
              v-model="form.expiresAt"
              type="datetime-local"
              :label="t('giftCardsAdmin.expiresAtLabel')"
            />
          </div>
        </div>

        <div class="flex flex-col sm:flex-row items-end gap-4">
          <div class="flex-1 w-full">
            <Input
              v-model="form.remark"
              :label="t('giftCardsAdmin.remarkLabel')"
              :placeholder="t('giftCards.remarkPlaceholder')"
              :maxlength="200"
            />
          </div>
          <div class="w-full sm:w-auto shrink-0">
            <Button
              variant="primary"
              class="w-full sm:w-auto min-w-[140px]"
              :disabled="saving"
              :loading="saving"
              @click="createCards"
            >
              <template #icon>
                <Plus class="w-4 h-4 stroke-[2.5]" />
              </template>
              {{ saving ? t('giftCards.generating') : t('giftCards.generate') }}
            </Button>
          </div>
        </div>
      </div>
    </Card>

    <!-- 生成结果展示卡片 -->
    <Card
      v-if="batchResult"
      data-reveal
      padding="sm"
      class="border-[color-mix(in_srgb,var(--success)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--success)_6%,var(--bg-surface))]"
    >
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div class="flex items-center gap-2.5">
          <div class="flex h-8 w-8 items-center justify-center rounded-full bg-[color-mix(in_srgb,var(--success)_15%,var(--bg-surface))] text-[var(--success)] shrink-0">
            <CheckCircle2 class="w-4 h-4 stroke-[2.5]" />
          </div>
          <div class="min-w-0">
            <div class="font-semibold text-sm text-[var(--text-primary)]">
              {{ t('giftCardsAdmin.batchResultTitle') }}
            </div>
            <div v-if="batchResult.batchId" class="font-mono text-xs text-[var(--text-secondary)] mt-0.5 truncate">
              {{ t('giftCardsAdmin.batchId', { batchId: batchResult.batchId }) }}
            </div>
          </div>
        </div>
        <div class="flex items-center gap-2 shrink-0">
          <Button variant="secondary" size="sm" @click="copyCodes(batchResult.codes)">
            <template #icon>
              <Copy class="w-3.5 h-3.5" />
            </template>
            {{ t('giftCardsAdmin.copyAll') }}
          </Button>
          <Button variant="ghost" size="sm" @click="batchResult = null">
            关闭
          </Button>
        </div>
      </div>
      <pre class="mt-3 max-h-48 overflow-auto rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3 font-mono text-xs leading-relaxed text-[var(--text-secondary)] select-all">{{ batchResult.codes.map(card => card.code).join('\n') }}</pre>
    </Card>

    <!-- 过滤器与数据容器 -->
    <Card data-reveal padding="sm">
      <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center">
          <div class="w-full sm:w-48">
            <Select
              v-model="statusFilter"
              :options="statusOptions"
              size="sm"
              @change="handleFilterChange"
            />
          </div>
          <label class="flex h-9 items-center gap-2 text-sm text-[var(--text-primary)] cursor-pointer select-none px-2 rounded-xl hover:bg-[var(--bg-surface-soft)] transition-colors">
            <input
              v-model="revealCode"
              type="checkbox"
              class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
              @change="loadData"
            />
            <span>{{ t('giftCardsAdmin.revealCode') }}</span>
          </label>
        </div>

        <!-- 批量操作按钮 -->
        <div class="flex items-center gap-2 flex-wrap">
          <span v-if="selectedCount > 0" class="text-xs font-semibold text-[var(--text-secondary)] px-2">
            已选择 {{ selectedCount }} 项
          </span>
          <Button
            variant="secondary"
            size="sm"
            :disabled="selectedCount === 0"
            @click="batchDisable"
          >
            <template #icon>
              <PowerOff class="w-3.5 h-3.5 text-[var(--warning)]" />
            </template>
            {{ t('giftCardsAdmin.batchDisable') }}
          </Button>
          <Button
            variant="danger"
            size="sm"
            :disabled="selectedCount === 0"
            @click="batchDelete"
          >
            <template #icon>
              <Trash2 class="w-3.5 h-3.5" />
            </template>
            {{ t('giftCardsAdmin.batchDelete') }}
          </Button>
        </div>
      </div>
    </Card>

    <!-- 数据列表：PC 表格与移动端卡片容器 -->
    <div data-reveal class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] overflow-hidden">
      <!-- 移动端响应式卡片 (space-y-3 p-4 lg:hidden) -->
      <div class="space-y-3 p-4 lg:hidden">
        <!-- 移动端骨架屏 -->
        <div v-if="loading && records.length === 0" class="space-y-3">
          <Card v-for="i in 3" :key="`mob-loading-${i}`" loading padding="sm" />
        </div>

        <!-- 移动端空数据 -->
        <div v-else-if="records.length === 0" class="py-8">
          <EmptyState :title="t('giftCards.empty')" />
        </div>

        <!-- 移动端卡片列表 -->
        <div
          v-for="card in records"
          :key="card.id"
          class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 space-y-3"
        >
          <div class="flex items-start gap-3">
            <input
              type="checkbox"
              class="mt-1 h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] shrink-0"
              :checked="selectedIds.has(card.id)"
              @change="toggleSelected(card.id, ($event.target as HTMLInputElement).checked)"
            />
            <div class="min-w-0 flex-1">
              <div class="flex items-start justify-between gap-2">
                <div class="min-w-0 flex items-center gap-1.5">
                  <code class="break-all font-mono text-xs font-semibold text-[var(--text-primary)]">{{ card.code }}</code>
                  <button
                    type="button"
                    class="text-[var(--text-tertiary)] hover:text-[var(--accent)] transition-colors p-1 rounded active:scale-95 shrink-0"
                    :title="t('giftCards.copy')"
                    @click="copySingleCode(card.code, card.id)"
                  >
                    <Check v-if="copiedId === card.id" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <Copy v-else class="w-3.5 h-3.5" />
                  </button>
                </div>
                <Badge :variant="statusVariant(card.status)" dot size="sm" class="shrink-0">
                  {{ statusLabel(card.status) }}
                </Badge>
              </div>

              <div v-if="card.remark" class="text-xs text-[var(--text-tertiary)] mt-1">
                {{ card.remark }}
              </div>

              <div class="mt-3 grid grid-cols-2 gap-3 text-sm">
                <div>
                  <div class="text-[11px] font-medium text-[var(--text-tertiary)] uppercase">{{ t('giftCards.amount') }}</div>
                  <div class="font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(card.balanceValue) }}</div>
                  <div class="text-xs text-[var(--text-tertiary)]">{{ t('giftCardsAdmin.faceValue', { amount: formatMoney(card.faceValue) }) }}</div>
                </div>
                <div>
                  <div class="text-[11px] font-medium text-[var(--text-tertiary)] uppercase">{{ t('giftCards.expiresAt') }}</div>
                  <div class="text-xs text-[var(--text-secondary)]">{{ formatDate(card.expiresAt) }}</div>
                </div>
              </div>

              <div class="mt-3 space-y-1 text-xs text-[var(--text-tertiary)] border-t border-[var(--border-color)] pt-2.5">
                <div>{{ t('giftCardsAdmin.createdBy', { user: card.createdBy?.username || '-' }) }}</div>
                <div v-if="card.owner?.username">{{ t('giftCardsAdmin.owner', { user: card.owner.username }) }}</div>
                <div v-if="card.usedBy?.username">{{ t('giftCardsAdmin.usedBy', { user: card.usedBy.username }) }}</div>
                <div v-if="card.batchId" class="font-mono text-[10px]">Batch: {{ card.batchId }}</div>
              </div>

              <div class="mt-4 flex flex-wrap gap-2">
                <Button
                  v-if="card.status === 'active' || card.status === 'disabled'"
                  variant="secondary"
                  size="sm"
                  @click="updateCardStatus(card)"
                >
                  <template #icon>
                    <Power v-if="card.status === 'disabled'" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <PowerOff v-else class="w-3.5 h-3.5 text-[var(--warning)]" />
                  </template>
                  {{ card.status === 'disabled' ? t('giftCardsAdmin.enable') : t('giftCardsAdmin.disable') }}
                </Button>
                <Button
                  v-if="card.status !== 'used'"
                  variant="ghost"
                  size="sm"
                  class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                  @click="deleteCard(card)"
                >
                  <template #icon>
                    <Trash2 class="w-3.5 h-3.5" />
                  </template>
                  {{ t('giftCardsAdmin.delete') }}
                </Button>
              </div>
            </div>
          </div>
        </div>

        <!-- 移动端分页 -->
        <div v-if="total > pageSize" class="pt-2">
          <Pagination
            v-model:page="page"
            v-model:page-size="pageSize"
            :total="total"
            @change="loadData"
          />
        </div>
      </div>

      <!-- PC 端定宽表格 (hidden overflow-hidden lg:block) -->
      <div class="hidden overflow-hidden lg:block">
        <table class="w-full table-fixed text-sm">
          <colgroup>
            <col class="w-[5%]" />
            <col class="w-[26%]" />
            <col class="w-[15%]" />
            <col class="w-[12%]" />
            <col class="w-[18%]" />
            <col class="w-[12%]" />
            <col class="w-[12%]" />
          </colgroup>
          <thead class="bg-[var(--bg-surface-soft)] text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] select-none">
            <tr class="border-b border-[var(--border-color)]">
              <th scope="col" class="px-4 py-3 text-center">
                <input
                  type="checkbox"
                  class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                  :checked="isAllSelected"
                  :indeterminate="isIndeterminate"
                  @change="toggleSelectAll(($event.target as HTMLInputElement).checked)"
                />
              </th>
              <th scope="col" class="px-4 py-3 text-left">{{ t('giftCards.code') }}</th>
              <th scope="col" class="px-4 py-3 text-left">{{ t('giftCards.amount') }}</th>
              <th scope="col" class="px-4 py-3 text-left">{{ t('giftCards.statusTitle') }}</th>
              <th scope="col" class="px-4 py-3 text-left">{{ t('giftCardsAdmin.createdAndUsed') }}</th>
              <th scope="col" class="px-4 py-3 text-left">{{ t('giftCards.expiresAt') }}</th>
              <th scope="col" class="px-4 py-3 text-right">{{ t('giftCards.action') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--border-color)]">
            <!-- 加载骨架屏 -->
            <template v-if="loading && records.length === 0">
              <tr v-for="i in 5" :key="`loading-${i}`" class="animate-pulse">
                <td colspan="7" class="px-4 py-4">
                  <div class="h-4 bg-[var(--bg-surface-soft)] rounded w-3/4"></div>
                </td>
              </tr>
            </template>

            <!-- 空数据状态 -->
            <tr v-else-if="records.length === 0">
              <td colspan="7" class="py-12">
                <EmptyState :title="t('giftCards.empty')" />
              </td>
            </tr>

            <!-- 列表行 -->
            <tr
              v-for="card in records"
              :key="card.id"
              class="hover:bg-[var(--bg-surface-soft)] transition-colors duration-150"
            >
              <!-- 复选框 -->
              <td class="px-4 py-3.5 text-center">
                <input
                  type="checkbox"
                  class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                  :checked="selectedIds.has(card.id)"
                  @change="toggleSelected(card.id, ($event.target as HTMLInputElement).checked)"
                />
              </td>

              <!-- 兑换码 -->
              <td class="px-4 py-3.5 truncate">
                <div class="flex items-center gap-1.5 min-w-0">
                  <code class="font-mono font-semibold text-xs text-[var(--text-primary)] truncate">{{ card.code }}</code>
                  <button
                    type="button"
                    class="text-[var(--text-tertiary)] hover:text-[var(--accent)] transition-colors shrink-0 p-1 rounded-md active:scale-95"
                    :title="t('giftCards.copy')"
                    @click="copySingleCode(card.code, card.id)"
                  >
                    <Check v-if="copiedId === card.id" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <Copy v-else class="w-3.5 h-3.5" />
                  </button>
                  <span
                    v-if="card.batchId"
                    class="hidden xl:inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-mono text-[var(--text-tertiary)] bg-[var(--bg-surface-soft)] border border-[var(--border-color)] truncate max-w-[120px]"
                    :title="`Batch: ${card.batchId}`"
                  >
                    #{{ card.batchId.slice(0, 8) }}
                  </span>
                </div>
                <div v-if="card.remark" class="text-xs text-[var(--text-tertiary)] truncate mt-0.5">
                  {{ card.remark }}
                </div>
              </td>

              <!-- 金额 -->
              <td class="px-4 py-3.5 text-[var(--text-primary)]">
                <div class="font-semibold tabular-nums text-sm">{{ formatMoney(card.balanceValue) }}</div>
                <div class="text-xs text-[var(--text-tertiary)]">{{ t('giftCardsAdmin.faceValue', { amount: formatMoney(card.faceValue) }) }}</div>
              </td>

              <!-- 状态标签 -->
              <td class="px-4 py-3.5">
                <Badge :variant="statusVariant(card.status)" dot size="sm">
                  {{ statusLabel(card.status) }}
                </Badge>
              </td>

              <!-- 创建与使用者 -->
              <td class="px-4 py-3.5 text-xs text-[var(--text-secondary)]">
                <div class="truncate">{{ t('giftCardsAdmin.createdBy', { user: card.createdBy?.username || '-' }) }}</div>
                <div v-if="card.owner?.username" class="truncate text-[var(--text-tertiary)]">{{ t('giftCardsAdmin.owner', { user: card.owner.username }) }}</div>
                <div v-if="card.usedBy?.username" class="truncate text-[var(--text-tertiary)]">{{ t('giftCardsAdmin.usedBy', { user: card.usedBy.username }) }}</div>
              </td>

              <!-- 有效期 -->
              <td class="px-4 py-3.5 text-xs text-[var(--text-secondary)] whitespace-nowrap">
                {{ formatDate(card.expiresAt) }}
              </td>

              <!-- 操作列 -->
              <td class="px-4 py-3.5 text-right">
                <div class="flex items-center justify-end gap-1">
                  <Button
                    v-if="card.status === 'active' || card.status === 'disabled'"
                    variant="ghost"
                    size="sm"
                    class="h-7 px-2 text-xs"
                    @click="updateCardStatus(card)"
                  >
                    <template #icon>
                      <Power v-if="card.status === 'disabled'" class="w-3.5 h-3.5 text-[var(--success)]" />
                      <PowerOff v-else class="w-3.5 h-3.5 text-[var(--warning)]" />
                    </template>
                    {{ card.status === 'disabled' ? t('giftCardsAdmin.enable') : t('giftCardsAdmin.disable') }}
                  </Button>
                  <Button
                    v-if="card.status !== 'used'"
                    variant="ghost"
                    size="sm"
                    class="h-7 px-2 text-xs text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                    @click="deleteCard(card)"
                  >
                    <template #icon>
                      <Trash2 class="w-3.5 h-3.5" />
                    </template>
                    {{ t('giftCardsAdmin.delete') }}
                  </Button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <!-- PC 端分页栏 -->
        <div v-if="total > pageSize" class="p-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
          <Pagination
            v-model:page="page"
            v-model:page-size="pageSize"
            :total="total"
            @change="loadData"
          />
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  * {
    transition: none !important;
    animation: none !important;
  }
}
</style>
