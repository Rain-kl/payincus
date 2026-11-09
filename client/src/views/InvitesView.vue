<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  ArrowLeft,
  CheckCircle2,
  Copy,
  Link,
  Percent,
  Plus,
  RefreshCw,
  Sparkles,
  Ticket
} from 'lucide-vue-next'
import api from '@/api'
import type { InviteCostOption, UserInvite, UserInviteSummary } from '@/types/api'
import { useToast } from '@/stores/toast'
import { dashboardPath } from '@/utils/app-paths'
import UserAvatar from '@/components/UserAvatar.vue'
import { Badge, Button, Card, EmptyState, PageHeader, Select } from '@/components/ui'
import { useReveal } from '@/composables/useReveal'

defineOptions({ name: 'InvitesView' })

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const toast = useToast()
const { t } = useI18n()

const loading = ref(true)
const listLoading = ref(false)
const generating = ref(false)
const summary = ref<UserInviteSummary | null>(null)
const invites = ref<UserInvite[]>([])
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const totalPages = ref(1)
const selectedCostResource = ref('')
const generatedInvites = ref<UserInvite[]>([])

const pageSizeOptions = [
  { label: '20 条/页', value: 20 },
  { label: '50 条/页', value: 50 },
  { label: '100 条/页', value: 100 }
]

const enabledCostOptions = computed(() => {
  return (summary.value?.costOptions || []).filter(option => option.enabled)
})

const costSelectOptions = computed(() => {
  return enabledCostOptions.value.map(option => ({
    label: `${option.label} (${option.displayAmount}) - ${getBalanceLabel(option.resource)}`,
    value: option.resource
  }))
})

const selectedCostOption = computed(() => {
  return enabledCostOptions.value.find(option => option.resource === selectedCostResource.value) || null
})

const canGenerate = computed(() => {
  const option = selectedCostOption.value
  if (!option || generating.value) return false
  return hasEnoughResource(option)
})

onMounted(loadPage)

async function loadPage(): Promise<void> {
  loading.value = true
  try {
    await Promise.all([loadSummary(), loadInvites()])
  } finally {
    loading.value = false
  }
}

async function loadSummary(): Promise<void> {
  summary.value = await api.userInvites.summary()
  if (!selectedCostResource.value && enabledCostOptions.value.length > 0) {
    selectedCostResource.value = enabledCostOptions.value[0].resource
  }
  if (selectedCostResource.value && !enabledCostOptions.value.some(option => option.resource === selectedCostResource.value)) {
    selectedCostResource.value = enabledCostOptions.value[0]?.resource || ''
  }
}

async function loadInvites(): Promise<void> {
  listLoading.value = true
  try {
    const res = await api.userInvites.list({ page: page.value, pageSize: pageSize.value })
    invites.value = res.invites || []
    total.value = res.total || 0
    totalPages.value = res.totalPages || 1
    if (page.value > totalPages.value && totalPages.value > 0) {
      page.value = totalPages.value
      await loadInvites()
    }
  } finally {
    listLoading.value = false
  }
}

async function generateInvite(): Promise<void> {
  if (!selectedCostResource.value || !canGenerate.value) return

  generating.value = true
  try {
    const res = await api.userInvites.generate({ costResource: selectedCostResource.value })
    generatedInvites.value = res.invites || []
    toast.success(t('invites.generateSuccess'))
    page.value = 1
    await Promise.all([loadSummary(), loadInvites()])
  } catch (err: any) {
    toast.error(err?.message || t('invites.generateFailed'))
  } finally {
    generating.value = false
  }
}

function hasEnoughResource(option: InviteCostOption): boolean {
  if (!summary.value) return false
  if (option.amount <= 0) return true
  if (option.resource === 'balance') return summary.value.balances.balance >= option.amount
  if (option.resource === 'points') return summary.value.balances.points >= option.amount
  return false
}

function getBalanceLabel(resource: string): string {
  if (!summary.value) return '-'
  if (resource === 'balance') return `¥${summary.value.balances.balance.toFixed(2)}`
  if (resource === 'points') return t('invites.pointsBalance', { points: summary.value.balances.points })
  return '-'
}

function getStatusVariant(invite: UserInvite): 'success' | 'default' {
  return invite.usedBy ? 'success' : 'default'
}

function getStatusLabel(invite: UserInvite): string {
  if (invite.usedBy) {
    return t('invites.status.used')
  }
  if (invite.expiresAt && new Date(invite.expiresAt) < new Date()) {
    return t('invites.status.expired')
  }
  return t('invites.status.unused')
}

function formatDate(value: string | null): string {
  if (!value) return '-'
  return new Date(value).toLocaleString()
}

function getInviteLink(invite: UserInvite): string {
  const origin = window.location.origin
  return `${origin}${invite.registerUrl}`
}

async function copyText(text: string, successMessage: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(successMessage)
  } catch {
    toast.error(t('common.copyFailed'))
  }
}

function previousPage(): void {
  if (page.value <= 1) return
  page.value -= 1
  void loadInvites()
}

function nextPage(): void {
  if (page.value >= totalPages.value) return
  page.value += 1
  void loadInvites()
}

function onPageSizeChange(val: any): void {
  const next = Number(val) || 20
  if (next !== pageSize.value) {
    pageSize.value = next
    page.value = 1
    void loadInvites()
  }
}
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <PageHeader
      data-reveal
      :title="t('invites.title')"
      :description="t('invites.description')"
    >
      <template #actions>
        <RouterLink v-slot="{ navigate }" :to="dashboardPath()" custom>
          <Button variant="secondary" class="w-full sm:w-auto" @click="navigate">
            <template #icon>
              <ArrowLeft class="h-4 w-4" />
            </template>
            {{ t('invites.backToOverview') }}
          </Button>
        </RouterLink>
      </template>
    </PageHeader>

    <div v-if="loading" class="space-y-6">
      <div class="grid gap-4 md:grid-cols-3">
        <Card v-for="i in 3" :key="i" loading padding="md" />
      </div>
      <Card loading padding="md" />
      <Card loading padding="md" />
    </div>

    <template v-else>
      <!-- 统计指标卡 -->
      <div data-reveal class="grid gap-4 md:grid-cols-3">
        <Card hoverable padding="md" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              {{ t('invites.stats.generated') }}
            </span>
            <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--accent)] group-hover:scale-105 transition-transform duration-300">
              <Ticket class="h-5 w-5 stroke-[1.8]" />
            </div>
          </div>
          <div class="mt-4 flex items-baseline justify-between">
            <div class="font-mono text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
              {{ summary?.stats.total || 0 }}
            </div>
          </div>
        </Card>

        <Card hoverable padding="md" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              {{ t('invites.stats.used') }}
            </span>
            <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--success)] group-hover:scale-105 transition-transform duration-300">
              <CheckCircle2 class="h-5 w-5 stroke-[1.8]" />
            </div>
          </div>
          <div class="mt-4 flex items-baseline justify-between">
            <div class="font-mono text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
              {{ summary?.stats.used || 0 }}
            </div>
          </div>
        </Card>

        <Card hoverable padding="md" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              {{ t('invites.stats.usageRate') }}
            </span>
            <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--accent)] group-hover:scale-105 transition-transform duration-300">
              <Percent class="h-5 w-5 stroke-[1.8]" />
            </div>
          </div>
          <div class="mt-4 flex items-baseline justify-between">
            <div class="font-mono text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
              {{ summary?.stats.usageRate || 0 }}%
            </div>
          </div>
        </Card>
      </div>

      <!-- 生成邀请码卡片 -->
      <Card data-reveal padding="md">
        <div class="flex flex-col gap-5 lg:flex-row lg:items-end lg:justify-between">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-3">
              <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--accent)]">
                <Sparkles class="h-5 w-5 stroke-[1.8]" />
              </div>
              <div class="min-w-0">
                <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('invites.generateTitle') }}</h2>
                <p class="mt-0.5 text-sm text-[var(--text-secondary)]">{{ t('invites.generateDescription') }}</p>
              </div>
            </div>

            <!-- 移动端便捷下拉选择框 (Select) -->
            <div v-if="costSelectOptions.length > 1" class="mt-4 sm:hidden">
              <Select
                v-model="selectedCostResource"
                :label="t('invites.columns.cost')"
                :options="costSelectOptions"
              />
            </div>

            <!-- 资源消耗选项卡片 (Card) -->
            <div v-if="enabledCostOptions.length > 0" class="mt-4 grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              <Card
                v-for="option in enabledCostOptions"
                :key="option.resource"
                hoverable
                padding="sm"
                class="transition-all duration-300"
                :class="selectedCostResource === option.resource
                  ? 'ring-2 ring-[var(--accent)] border-[var(--accent)] bg-[var(--bg-surface-soft)] shadow-sm'
                  : ''"
                @click="selectedCostResource = option.resource"
              >
                <div class="flex items-start justify-between gap-3">
                  <div class="min-w-0">
                    <p class="text-sm font-semibold text-[var(--text-primary)]">{{ option.label }}</p>
                    <p class="mt-1 font-mono text-2xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
                      {{ option.displayAmount }}
                    </p>
                  </div>
                  <Badge
                    :variant="hasEnoughResource(option) ? 'success' : 'error'"
                    class="shrink-0"
                  >
                    {{ getBalanceLabel(option.resource) }}
                  </Badge>
                </div>
              </Card>
            </div>

            <div v-else class="mt-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-sm text-[var(--text-secondary)]">
              {{ t('invites.noCostOptions') }}
            </div>
          </div>

          <Button
            size="md"
            variant="primary"
            :loading="generating"
            :disabled="!canGenerate"
            class="w-full justify-center lg:w-auto shrink-0"
            @click="generateInvite"
          >
            <template #icon>
              <Plus class="h-4 w-4 stroke-[2]" />
            </template>
            <span>{{ generating ? t('invites.generating') : t('invites.generate') }}</span>
          </Button>
        </div>

        <!-- 刚生成的邀请码 -->
        <div v-if="generatedInvites.length > 0" class="mt-6 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 md:p-5">
          <div class="flex items-center justify-between mb-3">
            <p class="text-sm font-semibold text-[var(--text-primary)]">{{ t('invites.justGenerated') }}</p>
            <Badge variant="success" size="sm">{{ generatedInvites.length }}</Badge>
          </div>
          <div class="space-y-2">
            <div
              v-for="invite in generatedInvites"
              :key="invite.id"
              class="flex flex-col gap-2 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3.5 sm:flex-row sm:items-center sm:justify-between shadow-sm"
            >
              <code class="font-mono text-sm font-semibold text-[var(--text-primary)] select-all">{{ invite.code }}</code>
              <div class="flex items-center gap-2">
                <Button
                  size="sm"
                  variant="ghost"
                  @click="copyText(invite.code, t('invites.codeCopied'))"
                >
                  <template #icon>
                    <Copy class="h-3.5 w-3.5" />
                  </template>
                  {{ t('invites.copyCode') }}
                </Button>
                <Button
                  size="sm"
                  variant="secondary"
                  @click="copyText(getInviteLink(invite), t('invites.linkCopied'))"
                >
                  <template #icon>
                    <Link class="h-3.5 w-3.5" />
                  </template>
                  {{ t('invites.copyLink') }}
                </Button>
              </div>
            </div>
          </div>
        </div>
      </Card>

      <!-- 我的邀请码列表卡片 -->
      <Card data-reveal padding="none" class="overflow-hidden">
        <template #header>
          <div class="flex items-center justify-between w-full">
            <div>
              <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('invites.myInvites') }}</h2>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('invites.myInvitesDescription') }}</p>
            </div>
            <Button
              size="sm"
              variant="ghost"
              :disabled="listLoading"
              @click="loadInvites"
            >
              <template #icon>
                <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': listLoading }" />
              </template>
              {{ t('common.refresh') }}
            </Button>
          </div>
        </template>

        <div v-if="listLoading" class="p-8 text-center text-themed-muted">
          {{ t('common.loading') }}
        </div>
        <EmptyState
          v-else-if="invites.length === 0"
          :title="t('invites.empty')"
        >
          <template #icon>
            <Ticket class="w-7 h-7 stroke-[1.5]" />
          </template>
        </EmptyState>
        <div v-else class="space-y-3 p-4 lg:hidden">
          <div
            v-for="invite in invites"
            :key="invite.id"
            class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <code class="block truncate font-mono text-sm font-semibold text-themed-secondary" :title="invite.code">
                  {{ invite.code }}
                </code>
                <div class="mt-1 text-xs text-themed-muted">
                  {{ invite.costSnapshot?.displayAmount || t('invites.adminGenerated') }}
                </div>
              </div>
              <Badge :variant="getStatusVariant(invite)" class="shrink-0 whitespace-nowrap">
                {{ getStatusLabel(invite) }}
              </Badge>
            </div>

            <div class="mt-4 rounded-lg bg-themed-secondary px-3 py-2">
              <div class="mb-2 text-[11px] font-medium uppercase tracking-wide text-themed-muted">{{ t('invites.usedBy') }}</div>
              <div v-if="invite.usedByUser" class="flex min-w-0 items-center gap-2">
                <UserAvatar
                  :username="invite.usedByUser.username"
                  :email="invite.usedByUser.email"
                  :avatar-style="invite.usedByUser.avatarStyle"
                  :badge-id="invite.usedByUser.avatarBadgeId"
                  :size="32"
                />
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-themed">{{ invite.usedByUser.username }}</p>
                  <p class="truncate text-xs text-themed-muted">{{ invite.usedByUser.email || '-' }}</p>
                </div>
              </div>
              <span v-else class="text-sm text-themed-muted">-</span>
            </div>

            <div class="mt-3 rounded-lg bg-themed-secondary px-3 py-2 text-sm text-themed-muted">
              <div>{{ t('invites.time.generated', { time: formatDate(invite.createdAt) }) }}</div>
              <div v-if="invite.usedAt">{{ t('invites.time.used', { time: formatDate(invite.usedAt) }) }}</div>
              <div v-else-if="invite.expiresAt">{{ t('invites.time.expires', { time: formatDate(invite.expiresAt) }) }}</div>
            </div>

            <div class="mt-4 grid grid-cols-2 gap-2">
              <Button
                size="sm"
                variant="ghost"
                class="justify-center"
                @click="copyText(invite.code, t('invites.codeCopied'))"
              >
                <template #icon>
                  <Copy class="h-3.5 w-3.5" />
                </template>
                {{ t('invites.copyCode') }}
              </Button>
              <Button
                size="sm"
                variant="secondary"
                class="justify-center"
                @click="copyText(getInviteLink(invite), t('invites.linkCopied'))"
              >
                <template #icon>
                  <Link class="h-3.5 w-3.5" />
                </template>
                {{ t('invites.copyLink') }}
              </Button>
            </div>
          </div>
        </div>
        <div v-if="invites.length > 0" class="hidden overflow-hidden lg:block">
          <table class="w-full table-fixed divide-y divide-themed">
            <thead class="bg-themed-tertiary">
              <tr class="text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">
                <th class="w-[24%] px-4 py-3">{{ t('invites.columns.code') }}</th>
                <th class="w-[12%] px-4 py-3">{{ t('common.status') }}</th>
                <th class="w-[20%] px-4 py-3">{{ t('invites.usedBy') }}</th>
                <th class="w-[14%] px-4 py-3">{{ t('invites.columns.cost') }}</th>
                <th class="w-[18%] px-4 py-3">{{ t('invites.columns.time') }}</th>
                <th class="w-[12%] px-4 py-3 text-right">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-themed">
              <tr v-for="invite in invites" :key="invite.id" class="text-sm transition-colors hover:bg-themed-hover">
                <td class="px-4 py-3">
                  <code class="block truncate font-mono text-sm font-semibold text-themed-secondary" :title="invite.code">{{ invite.code }}</code>
                </td>
                <td class="px-4 py-3 whitespace-nowrap">
                  <Badge :variant="getStatusVariant(invite)">{{ getStatusLabel(invite) }}</Badge>
                </td>
                <td class="px-4 py-3">
                  <div v-if="invite.usedByUser" class="flex items-center gap-2">
                    <UserAvatar
                      :username="invite.usedByUser.username"
                      :email="invite.usedByUser.email"
                      :avatar-style="invite.usedByUser.avatarStyle"
                      :badge-id="invite.usedByUser.avatarBadgeId"
                      :size="32"
                    />
                    <div class="min-w-0">
                      <p class="truncate text-sm font-medium text-themed">{{ invite.usedByUser.username }}</p>
                      <p class="truncate text-xs text-themed-muted">{{ invite.usedByUser.email || '-' }}</p>
                    </div>
                  </div>
                  <span v-else class="text-sm text-themed-muted">-</span>
                </td>
                <td class="px-4 py-3 text-sm text-themed-muted">
                  <div class="truncate">{{ invite.costSnapshot?.displayAmount || t('invites.adminGenerated') }}</div>
                </td>
                <td class="px-4 py-3 text-sm text-themed-muted">
                  <div>{{ t('invites.time.generated', { time: formatDate(invite.createdAt) }) }}</div>
                  <div v-if="invite.usedAt">{{ t('invites.time.used', { time: formatDate(invite.usedAt) }) }}</div>
                  <div v-else-if="invite.expiresAt">{{ t('invites.time.expires', { time: formatDate(invite.expiresAt) }) }}</div>
                </td>
                <td class="px-4 py-3 text-right whitespace-nowrap">
                  <div class="inline-flex items-center justify-end gap-1.5">
                    <Button
                      size="sm"
                      variant="ghost"
                      :title="t('invites.copyCode')"
                      @click="copyText(invite.code, t('invites.codeCopied'))"
                    >
                      <template #icon>
                        <Copy class="h-3.5 w-3.5" />
                      </template>
                      <span>{{ t('invites.copyCode') }}</span>
                    </Button>
                    <Button
                      size="sm"
                      variant="secondary"
                      :title="t('invites.copyLink')"
                      @click="copyText(getInviteLink(invite), t('invites.linkCopied'))"
                    >
                      <template #icon>
                        <Link class="h-3.5 w-3.5" />
                      </template>
                      <span>{{ t('invites.copyLink') }}</span>
                    </Button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div v-if="total > 0" class="flex flex-col gap-3 border-t border-themed px-5 py-4 sm:flex-row sm:items-center sm:justify-between">
          <div class="flex items-center gap-3 text-sm text-themed-muted">
            <span>{{ t('invites.total', { total }) }}</span>
            <div class="hidden sm:flex items-center gap-1.5 text-xs">
              <Select
                :model-value="pageSize"
                size="sm"
                :block="false"
                :options="pageSizeOptions"
                @update:model-value="onPageSizeChange"
              />
            </div>
          </div>
          <div class="grid grid-cols-[1fr_auto_1fr] items-center gap-2 sm:flex">
            <Button
              size="sm"
              variant="secondary"
              class="justify-center"
              :disabled="page <= 1 || listLoading"
              @click="previousPage"
            >
              {{ t('common.prevPage') }}
            </Button>
            <span class="min-w-[72px] text-center font-mono text-sm tabular-nums text-themed-muted">
              {{ page }} / {{ totalPages }}
            </span>
            <Button
              size="sm"
              variant="secondary"
              class="justify-center"
              :disabled="page >= totalPages || listLoading"
              @click="nextPage"
            >
              {{ t('common.nextPage') }}
            </Button>
          </div>
        </div>
      </Card>
    </template>
  </div>
</template>
