<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Copy, Eye, EyeOff, Gift, PlusCircle, RefreshCw, ShieldCheck, Sparkles } from 'lucide-vue-next'
import api from '@/api'
import { useToast } from '@/stores/toast'
import { useThemeStore } from '@/stores/theme'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select,
  type BadgeVariant
} from '@/components/ui'
import type { GiftCardRecord, GiftCardStatus } from '@/types/api'

const { t, locale } = useI18n()
const toast = useToast()
const themeStore = useThemeStore()

const redeemCode = ref('')
const generateAmount = ref<number | string>('')
const generateRemark = ref('')
const cards = ref<GiftCardRecord[]>([])
const loading = ref(false)
const redeeming = ref(false)
const generating = ref(false)
const statusFilter = ref<GiftCardStatus | ''>('')
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const lastGeneratedCode = ref('')
const revealedCodeIds = ref<Set<number>>(new Set())
const turnstileEnabled = ref(false)
const turnstileSiteKey = ref('')
const turnstileToken = ref('')
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const isTurnstileChallengeAvailable = computed<boolean>(() => turnstileEnabled.value && Boolean(turnstileSiteKey.value))

const statusOptions = computed<Array<{ value: GiftCardStatus | ''; label: string }>>(() => [
  { value: '', label: t('giftCards.status.all') },
  { value: 'active', label: t('giftCards.status.active') },
  { value: 'used', label: t('giftCards.status.used') },
  { value: 'disabled', label: t('giftCards.status.disabled') },
  { value: 'expired', label: t('giftCards.status.expired') }
])

const totalActiveValue = computed(() =>
  cards.value
    .filter(card => card.status === 'active')
    .reduce((sum, card) => sum + card.balanceValue, 0)
)

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

function badgeVariant(status: GiftCardStatus): BadgeVariant {
  switch (status) {
    case 'active':
      return 'success'
    case 'used':
      return 'default'
    case 'disabled':
      return 'warning'
    case 'expired':
      return 'error'
    default:
      return 'default'
  }
}

function maskGiftCardCode(code: string): string {
  if (!code) return ''
  if (code.length <= 12) return `${code.slice(0, 3)}...`
  return `${code.slice(0, 6)}...${code.slice(-4)}`
}

function isCodeRevealed(card: GiftCardRecord): boolean {
  return revealedCodeIds.value.has(card.id)
}

function displayCardCode(card: GiftCardRecord): string {
  return isCodeRevealed(card) ? card.code : (card.codeMasked || maskGiftCardCode(card.code))
}

function toggleCodeReveal(card: GiftCardRecord): void {
  const next = new Set(revealedCodeIds.value)
  if (next.has(card.id)) {
    next.delete(card.id)
  } else {
    next.add(card.id)
  }
  revealedCodeIds.value = next
}

async function loadTurnstileConfig(): Promise<void> {
  try {
    const config = await api.systemConfig.getPublic()
    turnstileEnabled.value = config.turnstileEnabled || false
    turnstileSiteKey.value = config.turnstileSiteKey || ''
  } catch (err) {
    console.error('[GiftCardsView] Failed to load Turnstile config:', err)
    turnstileEnabled.value = false
    turnstileSiteKey.value = ''
  }
}

async function loadCards(): Promise<void> {
  loading.value = true
  try {
    const response = await api.giftCards.mine({
      page: page.value,
      pageSize: pageSize.value,
      status: statusFilter.value || undefined
    })
    cards.value = response.records
    total.value = response.total
    revealedCodeIds.value = new Set()
  } catch (err: any) {
    toast.error(t('giftCards.toast.loadFailed', { message: err?.message || String(err) }))
  } finally {
    loading.value = false
  }
}

function getTurnstileToken(): string | undefined {
  if (!isTurnstileChallengeAvailable.value) return undefined
  const widgetToken = turnstileRef.value?.getToken?.()
  if (widgetToken) {
    turnstileToken.value = widgetToken
    return widgetToken
  }

  if (turnstileToken.value) return turnstileToken.value

  const domToken = document
    .querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]')
    ?.value
    ?.trim()
  if (domToken) {
    turnstileToken.value = domToken
    return domToken
  }

  return undefined
}

function resetTurnstile(): void {
  turnstileToken.value = ''
  turnstileRef.value?.reset?.()
}

function requireTurnstileToken(): string | undefined | null {
  const token = getTurnstileToken()
  if (isTurnstileChallengeAvailable.value && !token) {
    toast.warning(t('giftCards.toast.turnstileRequired'))
    return null
  }
  return token
}

function onTurnstileExpire(): void {
  turnstileToken.value = ''
}

function onTurnstileError(): void {
  turnstileToken.value = ''
  toast.error(t('giftCards.toast.turnstileFailed'))
}

function onTurnstileVerify(token: string): void {
  turnstileToken.value = token
}

async function redeemGiftCard(): Promise<void> {
  const code = redeemCode.value.trim()
  if (!code) {
    toast.warning(t('giftCards.toast.redeemCodeRequired'))
    return
  }
  redeeming.value = true
  try {
    const token = requireTurnstileToken()
    if (token === null) return
    const response = await api.giftCards.redeem(code, token)
    toast.success(t('giftCards.toast.redeemSuccess', { amount: formatMoney(response.amount) }))
    redeemCode.value = ''
    page.value = 1
    await loadCards()
  } catch (err: any) {
    toast.error(t('giftCards.toast.redeemFailed', { message: err?.message || String(err) }))
  } finally {
    resetTurnstile()
    redeeming.value = false
  }
}

async function generateGiftCard(): Promise<void> {
  const amount = Number(generateAmount.value)
  if (!Number.isFinite(amount) || amount <= 0) {
    toast.warning(t('giftCards.toast.generateAmountRequired'))
    return
  }
  generating.value = true
  try {
    const token = requireTurnstileToken()
    if (token === null) return
    const response = await api.giftCards.generate(amount, generateRemark.value.trim() || undefined, token)
    lastGeneratedCode.value = response.giftCard.code
    toast.success(t('giftCards.toast.generateSuccess', { balance: formatMoney(response.newBalance) }))
    generateAmount.value = ''
    generateRemark.value = ''
    page.value = 1
    await loadCards()
  } catch (err: any) {
    toast.error(t('giftCards.toast.generateFailed', { message: err?.message || String(err) }))
  } finally {
    resetTurnstile()
    generating.value = false
  }
}

async function copyCode(code: string): Promise<void> {
  await navigator.clipboard.writeText(code)
  toast.success(t('giftCards.toast.copied'))
}

function onStatusFilterChange(): void {
  page.value = 1
  loadCards()
}

function onPageChange(newPage: number): void {
  page.value = newPage
  loadCards()
}

function onPageSizeChange(newSize: number): void {
  pageSize.value = newSize
  page.value = 1
  loadCards()
}

function onPaginationChange(payload: { page: number; pageSize: number }): void {
  page.value = payload.page
  pageSize.value = payload.pageSize
  loadCards()
}

onMounted(async () => {
  await Promise.all([loadTurnstileConfig(), loadCards()])
})
</script>

<template>
  <div class="kawaii-page space-y-6 md:space-y-8 animate-fade-in max-w-7xl mx-auto pb-12">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('nav.giftCards')"
      :description="t('giftCards.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadCards"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5 stroke-[2]" />
          </template>
          {{ loading ? t('giftCards.loading') : t('giftCards.refresh') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Last Generated Code Notice -->
    <div
      v-if="lastGeneratedCode"
      class="rounded-2xl border border-[color-mix(in_srgb,var(--warning)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--warning)_8%,var(--bg-surface))] p-5 shadow-[0_2px_8px_rgba(0,0,0,0.04)] transition-all duration-300"
    >
      <div class="flex items-center gap-2.5 text-sm font-semibold text-[var(--warning)]">
        <Sparkles class="h-4 w-4 shrink-0 stroke-[2]" />
        <span>{{ t('giftCards.lastCodeNotice') }}</span>
      </div>
      <div class="mt-3 flex flex-col gap-2.5 sm:flex-row sm:items-center">
        <code class="min-w-0 flex-1 break-all rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2.5 font-mono text-xs tabular-nums text-[var(--text-primary)] shadow-sm">
          {{ lastGeneratedCode }}
        </code>
        <Button
          variant="secondary"
          size="sm"
          class="shrink-0"
          @click="copyCode(lastGeneratedCode)"
        >
          <template #icon>
            <Copy class="w-3.5 h-3.5" />
          </template>
          {{ t('giftCards.copy') }}
        </Button>
      </div>
    </div>

    <!-- Turnstile Challenge Card (if required) -->
    <Card v-if="isTurnstileChallengeAvailable" padding="md">
      <div class="flex items-start gap-3.5">
        <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
          <ShieldCheck class="h-5 w-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
            {{ t('giftCards.turnstileTitle') }}
          </h2>
          <p class="mt-1 text-sm text-[var(--text-secondary)]">
            {{ t('giftCards.turnstileDescription') }}
          </p>
        </div>
      </div>
      <div class="mt-4">
        <TurnstileWidget
          ref="turnstileRef"
          v-model="turnstileToken"
          :site-key="turnstileSiteKey"
          :theme="themeStore.isDark ? 'dark' : 'light'"
          action="gift_card"
          @verify="onTurnstileVerify"
          @expire="onTurnstileExpire"
          @error="onTurnstileError"
        />
      </div>
    </Card>

    <!-- Action Cards Grid -->
    <div class="grid gap-6 lg:grid-cols-2">
      <!-- Redeem Gift Card Card -->
      <Card padding="md">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
            <Gift class="h-5 w-5 stroke-[2]" />
          </div>
          <div>
            <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('giftCards.redeemTitle') }}
            </h2>
          </div>
        </div>
        <div class="mt-5 flex flex-col gap-3 sm:flex-row sm:items-center">
          <div class="flex-1">
            <Input
              v-model="redeemCode"
              class="font-mono text-sm"
              :placeholder="t('giftCards.redeemPlaceholder')"
              clearable
              @enter="redeemGiftCard"
            />
          </div>
          <Button
            variant="primary"
            class="shrink-0"
            :loading="redeeming"
            :disabled="redeeming || !redeemCode.trim()"
            @click="redeemGiftCard"
          >
            {{ redeeming ? t('giftCards.redeeming') : t('giftCards.redeem') }}
          </Button>
        </div>
      </Card>

      <!-- Generate Gift Card Card -->
      <Card padding="md">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
            <PlusCircle class="h-5 w-5 stroke-[2]" />
          </div>
          <div>
            <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('giftCards.generateTitle') }}
            </h2>
          </div>
        </div>
        <div class="mt-5 grid gap-3 sm:grid-cols-[160px_minmax(0,1fr)_auto]">
          <Input
            v-model="generateAmount"
            type="number"
            class="font-mono tabular-nums text-sm"
            :placeholder="t('giftCards.amountPlaceholder')"
          />
          <Input
            v-model="generateRemark"
            :maxlength="200"
            class="text-sm"
            :placeholder="t('giftCards.remarkPlaceholder')"
            @enter="generateGiftCard"
          />
          <Button
            variant="primary"
            class="shrink-0"
            :loading="generating"
            :disabled="generating || !generateAmount || Number(generateAmount) <= 0"
            @click="generateGiftCard"
          >
            {{ generating ? t('giftCards.generating') : t('giftCards.generate') }}
          </Button>
        </div>
      </Card>
    </div>

    <!-- Gift Cards History Card -->
    <Card padding="md">
      <div class="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
        <div>
          <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
            {{ t('giftCards.mineTitle') }}
          </h2>
          <p class="mt-1 text-sm font-mono tabular-nums text-[var(--text-secondary)]">
            {{ t('giftCards.activeTotal', { amount: formatMoney(totalActiveValue) }) }}
          </p>
        </div>
        <div class="w-full md:w-48">
          <Select
            v-model="statusFilter"
            :options="statusOptions"
            size="sm"
            @change="onStatusFilterChange"
          />
        </div>
      </div>

      <!-- Mobile List View -->
      <div class="mt-5 space-y-3 lg:hidden">
        <div
          v-for="card in cards"
          :key="card.id"
          class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.02)] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] hover:border-[var(--border-hover)]"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="text-xs uppercase tracking-wider text-[var(--text-tertiary)] font-medium">
                {{ t('giftCards.code') }}
              </div>
              <code class="mt-1 block break-all font-mono text-xs text-[var(--text-primary)]">
                {{ displayCardCode(card) }}
              </code>
            </div>
            <Badge :variant="badgeVariant(card.status)" :dot="card.status === 'active'" size="sm">
              {{ statusLabel(card.status) }}
            </Badge>
          </div>
          <div class="mt-3.5 grid grid-cols-2 gap-3 text-sm border-t border-[var(--border-color)] pt-3">
            <div>
              <div class="text-xs uppercase tracking-wider text-[var(--text-tertiary)] font-medium">
                {{ t('giftCards.amount') }}
              </div>
              <div class="mt-0.5 font-mono font-semibold tabular-nums text-[var(--text-primary)]">
                {{ formatMoney(card.balanceValue) }}
              </div>
            </div>
            <div>
              <div class="text-xs uppercase tracking-wider text-[var(--text-tertiary)] font-medium">
                {{ t('giftCards.expiresAt') }}
              </div>
              <div class="mt-0.5 text-xs text-[var(--text-secondary)]">
                {{ formatDate(card.expiresAt) }}
              </div>
            </div>
          </div>
          <div class="mt-4 flex gap-2">
            <Button
              variant="secondary"
              size="sm"
              class="flex-1"
              @click="toggleCodeReveal(card)"
            >
              <template #icon>
                <EyeOff v-if="isCodeRevealed(card)" class="w-3.5 h-3.5" />
                <Eye v-else class="w-3.5 h-3.5" />
              </template>
              {{ isCodeRevealed(card) ? t('giftCards.hideCode') : t('giftCards.showCode') }}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              class="flex-1"
              @click="copyCode(card.code)"
            >
              <template #icon>
                <Copy class="w-3.5 h-3.5" />
              </template>
              {{ t('giftCards.copy') }}
            </Button>
          </div>
        </div>

        <div v-if="!loading && cards.length === 0" class="rounded-2xl border border-dashed border-[var(--border-color)] bg-[var(--bg-surface-soft)]/50">
          <EmptyState :title="t('giftCards.empty')" />
        </div>
      </div>

      <!-- Desktop Table View -->
      <div class="mt-5 hidden overflow-hidden lg:block">
        <div class="overflow-hidden rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)]">
          <table class="w-full table-fixed text-sm">
            <colgroup>
              <col style="width: 28%" />
              <col style="width: 16%" />
              <col style="width: 14%" />
              <col style="width: 24%" />
              <col style="width: 18%" />
            </colgroup>
            <thead class="border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-left text-xs uppercase tracking-wider text-[var(--text-tertiary)] select-none">
              <tr>
                <th class="py-3.5 px-4 font-semibold">{{ t('giftCards.code') }}</th>
                <th class="py-3.5 px-4 font-semibold">{{ t('giftCards.amount') }}</th>
                <th class="py-3.5 px-4 font-semibold">{{ t('giftCards.statusTitle') }}</th>
                <th class="py-3.5 px-4 font-semibold">{{ t('giftCards.expiresAt') }}</th>
                <th class="py-3.5 px-4 font-semibold">{{ t('giftCards.action') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="card in cards"
                :key="card.id"
                class="transition-colors duration-150 hover:bg-[var(--bg-surface-soft)]"
              >
                <td class="py-3.5 px-4 font-mono text-xs text-[var(--text-primary)]">
                  <span class="block truncate">{{ displayCardCode(card) }}</span>
                </td>
                <td class="py-3.5 px-4 font-mono font-medium tabular-nums text-[var(--text-primary)]">
                  {{ formatMoney(card.balanceValue) }}
                </td>
                <td class="py-3.5 px-4">
                  <Badge :variant="badgeVariant(card.status)" :dot="card.status === 'active'" size="sm">
                    {{ statusLabel(card.status) }}
                  </Badge>
                </td>
                <td class="py-3.5 px-4 text-xs text-[var(--text-secondary)]">
                  {{ formatDate(card.expiresAt) }}
                </td>
                <td class="py-3.5 px-4 whitespace-nowrap">
                  <div class="flex items-center gap-1.5">
                    <Button
                      variant="secondary"
                      size="sm"
                      class="!min-h-[32px] !h-8 !w-8 !p-0 rounded-full"
                      :title="isCodeRevealed(card) ? t('giftCards.hideCode') : t('giftCards.showCode')"
                      :aria-label="isCodeRevealed(card) ? t('giftCards.hideCode') : t('giftCards.showCode')"
                      @click="toggleCodeReveal(card)"
                    >
                      <EyeOff v-if="isCodeRevealed(card)" class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                      <Eye v-else class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                    </Button>
                    <Button
                      variant="secondary"
                      size="sm"
                      class="!min-h-[32px] !h-8 !w-8 !p-0 rounded-full"
                      :title="t('giftCards.copy')"
                      :aria-label="t('giftCards.copy')"
                      @click="copyCode(card.code)"
                    >
                      <Copy class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                    </Button>
                  </div>
                </td>
              </tr>
              <tr v-if="!loading && cards.length === 0">
                <td colspan="5" class="py-12 text-center">
                  <EmptyState :title="t('giftCards.empty')" />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Pagination -->
      <div v-if="total > 0" class="mt-6 border-t border-[var(--border-color)] pt-4">
        <Pagination
          :page="page"
          :page-size="pageSize"
          :total="total"
          :page-size-options="[10, 20, 50, 100]"
          @update:page="onPageChange"
          @update:page-size="onPageSizeChange"
          @change="onPaginationChange"
        />
      </div>
    </Card>
  </div>
</template>
