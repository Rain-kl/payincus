<script setup lang="ts">
import { computed, ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  ChevronDown,
  ChevronUp,
  Download,
  FileText,
  RefreshCw,
  RotateCcw,
  Search,
  Shield,
  ShieldAlert
} from 'lucide-vue-next'
import { useAuthStore } from '@/stores/auth'
import api, { type RiskOperationDefinition } from '@/api'
import { useToast } from '@/stores/toast'
import { useReveal } from '@/composables/useReveal'
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
import type { BadgeVariant } from '@/components/ui'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import type { Log } from '@/types/api'
import {
  formatLogAction,
  formatLogContent,
  formatLogModule,
  formatLogResult
} from '@/utils/log-format'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'LogsView' })

const { t, locale, tm } = useI18n()
const toast = useToast()
const authStore = useAuthStore()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 数据
const logs = ref<Log[]>([])
const loading = ref<boolean>(true)   // 首次加载
const refreshing = ref<boolean>(false)  // 后台刷新
const exportingAudit = ref<boolean>(false)
const modules = ref<string[]>([])
const riskDefinitions = ref<RiskOperationDefinition[]>([])

// 分页
const page = ref<number>(1)
const pageSize = ref<number>(100)
const total = ref<number>(0)
const totalPages = ref<number>(0)

// 筛选
const selectedModule = ref<string>('')
const searchQuery = ref<string>('')

// 展开的内容行
const expandedRows = ref<Set<number>>(new Set())
const canAccessAudit = computed(() => authStore.isAdmin)

// 风险统计
const highRiskCount = computed(() =>
  logs.value.filter(l => l.risk_level === 'high' || l.risk_level === 'critical').length
)
const approvalRequiredCount = computed(() =>
  logs.value.filter(l => l.approval_required).length
)
const verificationRequiredCount = computed(() =>
  logs.value.filter(l => l.verification_required).length
)

// 加载日志
async function loadLogs(silent = false): Promise<void> {
  if (!silent) loading.value = true
  else refreshing.value = true
  try {
    const params: { page: number; pageSize: number; module?: string; search?: string } = {
      page: page.value,
      pageSize: pageSize.value
    }
    if (selectedModule.value) {
      params.module = selectedModule.value
    }
    if (searchQuery.value) {
      params.search = searchQuery.value
    }

    const response = await api.logs.list(params)
    const data = response as { logs?: Log[]; total?: number; totalPages?: number }
    logs.value = data.logs || []
    total.value = data.total || 0
    totalPages.value = data.totalPages || 0
  } catch (err: any) {
    toast.error(t('logs.loadFailed') + ': ' + (err?.message || String(err)))
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

// 加载模块
async function loadModules(): Promise<void> {
  try {
    const response = await api.logs.getModules()
    modules.value = (response as { modules?: string[] }).modules || []
  } catch {
    // 静默失败
  }
}

async function loadRiskDefinitions(): Promise<void> {
  if (!canAccessAudit.value) return
  try {
    const response = await api.logs.getRiskDefinitions()
    riskDefinitions.value = response.definitions || []
  } catch {
    riskDefinitions.value = []
  }
}

// 搜索
function handleSearch(): void {
  page.value = 1
  loadLogs(true)
}

function handleModuleChange(val: string): void {
  selectedModule.value = val
  page.value = 1
  loadLogs(true)
}

// 重置筛选
function resetFilters(): void {
  selectedModule.value = ''
  searchQuery.value = ''
  page.value = 1
  loadLogs(true)
}

// 分页变更
function handlePageChange(newPage: number): void {
  page.value = newPage
  loadLogs(true)
}

function handlePageSizeUpdate(newSize: number): void {
  pageSize.value = newSize
  page.value = 1
  loadLogs(true)
}

// 格式化日期
function formatDate(dateString: string | null | undefined): string {
  if (!dateString) return '-'
  const date = new Date(dateString)
  // 支援 zh-CN, zh-TW, en 三種語言
  const localeMap: Record<string, string> = {
    'zh-CN': 'zh-CN',
    'zh-TW': 'zh-TW',
    'en': 'en-US'
  }
  const localeCode = localeMap[locale.value] || 'en-US'
  return date.toLocaleString(localeCode, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit'
  })
}

// UI Badge Variant 助手
function getResultBadgeVariant(result: string): BadgeVariant {
  if (result === 'success') return 'success'
  if (result === 'failed') return 'error'
  return 'warning'
}

function getRiskBadgeVariant(level: string | undefined): BadgeVariant {
  if (level === 'critical') return 'error'
  if (level === 'high') return 'warning'
  if (level === 'medium') return 'default'
  return 'success'
}

function formatRiskLevel(level: string | undefined): string {
  const map: Record<string, string> = {
    low: t('logs.risk.low'),
    medium: t('logs.risk.medium'),
    high: t('logs.risk.high'),
    critical: t('logs.risk.critical')
  }
  return map[level || 'low'] || map.low
}

async function exportAuditCsv(): Promise<void> {
  if (!canAccessAudit.value || exportingAudit.value) return
  exportingAudit.value = true
  try {
    const params: Record<string, unknown> = {
      limit: 1000
    }
    if (selectedModule.value) params.module = selectedModule.value
    if (searchQuery.value) params.search = searchQuery.value

    const blob = await api.logs.exportAuditCsv(params)
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `payincus-audit-logs-${Date.now()}.csv`
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
    toast.success(t('logs.auditExportSuccess'))
  } catch (err: any) {
    toast.error(t('logs.auditExportFailed') + ': ' + (err?.message || String(err)))
  } finally {
    exportingAudit.value = false
  }
}

function formatResult(result: string): string {
  return formatLogResult(result, tm('logResults') as Record<string, unknown>)
}

function formatModule(module: string): string {
  return formatLogModule(module, tm('logModules') as Record<string, unknown>)
}

function formatAction(action: string): string {
  return formatLogAction(action, tm('logActions') as Record<string, unknown>)
}

function formatContent(content: string): string {
  return formatLogContent(content)
}

// 切换内容展开/收起
function toggleContent(logId: number): void {
  if (expandedRows.value.has(logId)) {
    expandedRows.value.delete(logId)
  } else {
    expandedRows.value.add(logId)
  }
}

// 检查内容是否需要截断
function isContentLong(content: string): boolean {
  return !!content && content.length > 60
}

onMounted(async () => {
  await Promise.all([loadModules(), loadRiskDefinitions(), loadLogs()])
})
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- Page Header -->
    <PageHeader
      data-reveal
      :title="$t('logs.title')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="refreshing"
          @click="loadLogs(true)"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" :class="{ 'animate-spin': refreshing }" />
          </template>
          {{ $t('common.refresh') }}
        </Button>
        <Button
          v-if="canAccessAudit"
          variant="primary"
          size="sm"
          :loading="exportingAudit"
          @click="exportAuditCsv"
        >
          <template #icon>
            <Download class="w-4 h-4" />
          </template>
          {{ $t('logs.exportAudit') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Risk Audit Summary (Admin Only) -->
    <div
      v-if="canAccessAudit && riskDefinitions.length > 0"
      data-reveal
      class="grid grid-cols-2 sm:grid-cols-4 gap-3"
    >
      <Card padding="sm" class="flex items-center gap-3">
        <div class="w-10 h-10 rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0">
          <Shield class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)] truncate">
            {{ $t('logs.auditSummary.riskDefinitions') }}
          </div>
          <div class="text-lg font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ riskDefinitions.length }}
          </div>
        </div>
      </Card>

      <Card padding="sm" class="flex items-center gap-3">
        <div class="w-10 h-10 rounded-xl bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] flex items-center justify-center shrink-0">
          <ShieldAlert class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)] truncate">
            {{ $t('logs.auditSummary.highRiskCurrentPage') }}
          </div>
          <div class="text-lg font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ highRiskCount }}
          </div>
        </div>
      </Card>

      <Card padding="sm" class="flex items-center gap-3">
        <div class="w-10 h-10 rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] flex items-center justify-center shrink-0">
          <ShieldAlert class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)] truncate">
            {{ $t('logs.auditSummary.approvalRequired') }}
          </div>
          <div class="text-lg font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ approvalRequiredCount }}
          </div>
        </div>
      </Card>

      <Card padding="sm" class="flex items-center gap-3">
        <div class="w-10 h-10 rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0">
          <Shield class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)] truncate">
            {{ $t('logs.auditSummary.verificationRequired') }}
          </div>
          <div class="text-lg font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ verificationRequiredCount }}
          </div>
        </div>
      </Card>
    </div>

    <!-- Filters Card -->
    <Card data-reveal padding="sm">
      <div class="grid grid-cols-1 md:grid-cols-12 gap-3 items-end">
        <!-- Module filter -->
        <div class="md:col-span-4">
          <Select
            v-model="selectedModule"
            :label="$t('logs.module')"
            size="sm"
            @change="handleModuleChange"
          >
            <option value="">{{ $t('logs.allModules') }}</option>
            <option v-for="m in modules" :key="m" :value="m">{{ formatModule(m) }}</option>
          </Select>
        </div>

        <!-- Search -->
        <div class="md:col-span-5">
          <Input
            v-model="searchQuery"
            :label="$t('logs.search')"
            size="sm"
            clearable
            :placeholder="$t('logs.searchPlaceholder')"
            @enter="handleSearch"
            @clear="handleSearch"
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>

        <!-- Actions -->
        <div class="md:col-span-3 flex items-center gap-2">
          <Button
            variant="primary"
            size="sm"
            class="flex-1"
            @click="handleSearch"
          >
            <template #icon>
              <Search class="w-3.5 h-3.5" />
            </template>
            {{ $t('logs.search') }}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            class="flex-1"
            @click="resetFilters"
          >
            <template #icon>
              <RotateCcw class="w-3.5 h-3.5" />
            </template>
            {{ $t('logs.reset') }}
          </Button>
        </div>
      </div>
    </Card>

    <!-- Logs Content -->
    <SkeletonLoader v-if="loading" type="table" />

    <template v-else>
      <!-- Empty State -->
      <Card v-if="logs.length === 0" data-reveal padding="none">
        <EmptyState :title="$t('logs.noLogs')">
          <template #icon>
            <FileText class="w-7 h-7 stroke-[1.5]" />
          </template>
        </EmptyState>
      </Card>

      <!-- Mobile Cards View -->
      <div v-else class="space-y-3 lg:hidden">
        <div
          v-for="log in logs"
          :key="log.id"
          class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <div class="truncate text-sm font-semibold text-themed">{{ formatAction(log.action) }}</div>
              <div class="mt-1 font-mono text-xs text-themed-muted">{{ formatDate(log.created_at) }}</div>
            </div>
            <Badge :variant="getResultBadgeVariant(log.result)" size="sm" class="shrink-0 whitespace-nowrap">
              {{ formatResult(log.result) }}
            </Badge>
          </div>

          <div class="mt-4 grid grid-cols-2 gap-2 text-sm">
            <div class="rounded-lg bg-themed-secondary px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-themed-muted">{{ $t('logs.user') }}</div>
              <div class="mt-1 truncate text-themed">{{ log.username || $t('logs.system') }}</div>
            </div>
            <div class="rounded-lg bg-themed-secondary px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-themed-muted">{{ $t('logs.module') }}</div>
              <div class="mt-1 truncate text-themed">{{ formatModule(log.module) }}</div>
            </div>
          </div>

          <div class="mt-3 flex flex-wrap items-center gap-2">
            <Badge :variant="getRiskBadgeVariant(log.risk_level)" size="sm" class="whitespace-nowrap">
              {{ formatRiskLevel(log.risk_level) }}
            </Badge>
            <Badge
              v-if="log.approval_required"
              variant="warning"
              size="sm"
              class="whitespace-nowrap"
              :title="$t('logs.auditSummary.approvalRequired')"
            >
              {{ log.approval_required ? $t('logs.auditSummary.approvalRequired') : '' }}
            </Badge>
            <Badge
              v-if="log.verification_required"
              variant="info"
              size="sm"
              class="whitespace-nowrap"
              :title="$t('logs.auditSummary.verificationRequired')"
            >
              {{ log.verification_required ? $t('logs.auditSummary.verificationRequired') : '' }}
            </Badge>
            <span v-if="log.risk_title" class="min-w-0 truncate text-xs text-themed-muted">
              {{ log.risk_title }}
            </span>
          </div>

          <div class="mt-3 rounded-lg bg-themed-secondary px-3 py-2 text-sm">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-themed-muted">{{ $t('logs.content') }}</div>
            <span v-if="!isContentLong(formatContent(log.content))" class="text-themed-secondary break-words">
              {{ formatContent(log.content) }}
            </span>
            <template v-else>
              <div v-if="!expandedRows.has(log.id)" class="flex min-w-0 items-center gap-2">
                <span class="min-w-0 flex-1 truncate text-themed-secondary">{{ formatContent(log.content).slice(0, 72) }}...</span>
                <button
                  type="button"
                  class="shrink-0 text-xs text-[var(--accent)] hover:underline inline-flex items-center gap-0.5"
                  @click="toggleContent(log.id)"
                >
                  <ChevronDown class="w-3.5 h-3.5" />
                  {{ $t('logs.expand') }}
                </button>
              </div>
              <div v-else class="space-y-2">
                <p class="whitespace-pre-wrap break-words text-themed-secondary text-xs leading-relaxed">{{ formatContent(log.content) }}</p>
                <button
                  type="button"
                  class="text-xs text-[var(--accent)] hover:underline inline-flex items-center gap-0.5"
                  @click="toggleContent(log.id)"
                >
                  <ChevronUp class="w-3.5 h-3.5" />
                  {{ $t('logs.collapse') }}
                </button>
              </div>
            </template>
          </div>
        </div>

        <!-- Mobile Pagination -->
        <Card v-if="total > 0" padding="sm">
          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            show-size-changer
            :page-size-options="[10, 30, 50, 100]"
            @update:page="handlePageChange"
            @update:page-size="handlePageSizeUpdate"
          >
            <template #total="{ total: tTotal, page: cPage, totalPages: tPages }">
              <span>{{ $t('logs.totalRecords', { total: tTotal, page: cPage, totalPages: tPages }) }}</span>
            </template>
          </Pagination>
        </Card>
      </div>

      <!-- Desktop Table View -->
      <div v-if="logs.length > 0" class="card hidden overflow-hidden lg:block">
        <table class="w-full table-fixed">
          <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
            <tr class="text-left text-xs font-medium uppercase tracking-wider text-[var(--text-tertiary)]">
              <th class="w-[16%] py-3.5 px-4 whitespace-nowrap">{{ $t('logs.time') }}</th>
              <th class="w-[12%] py-3.5 px-4 whitespace-nowrap">{{ $t('logs.user') }}</th>
              <th class="w-[13%] py-3.5 px-4 whitespace-nowrap">{{ $t('logs.module') }}</th>
              <th class="w-[14%] py-3.5 px-4 whitespace-nowrap">{{ $t('logs.action') }}</th>
              <th class="w-[12%] py-3.5 px-4 whitespace-nowrap">{{ $t('logs.riskLevel') }}</th>
              <th class="w-[25%] py-3.5 px-4 whitespace-nowrap">{{ $t('logs.content') }}</th>
              <th class="w-[8%] py-3.5 px-4 whitespace-nowrap text-center">{{ $t('logs.result') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--border-color)]">
            <tr
              v-for="log in logs"
              :key="log.id"
              class="hover:bg-[var(--bg-surface-soft)]/60 transition-colors duration-150"
            >
              <td class="py-3 px-4 font-mono text-xs text-[var(--text-secondary)] whitespace-nowrap">
                {{ formatDate(log.created_at) }}
              </td>
              <td class="py-3 px-4 text-sm text-[var(--text-primary)]">
                <div class="truncate font-medium" :title="log.username || $t('logs.system')">
                  {{ log.username || $t('logs.system') }}
                </div>
              </td>
              <td class="py-3 px-4 text-sm">
                <Badge variant="default" size="sm" class="max-w-full truncate whitespace-nowrap">
                  {{ formatModule(log.module) }}
                </Badge>
              </td>
              <td class="py-3 px-4 text-sm text-[var(--text-primary)]">
                <div class="truncate font-mono text-xs text-[var(--text-secondary)]" :title="formatAction(log.action)">
                  {{ formatAction(log.action) }}
                </div>
              </td>
              <td class="py-3 px-4 text-sm whitespace-nowrap">
                <div class="flex flex-col gap-1">
                  <div class="flex items-center gap-1.5 flex-wrap">
                    <Badge :variant="getRiskBadgeVariant(log.risk_level)" size="sm" class="whitespace-nowrap">
                      {{ formatRiskLevel(log.risk_level) }}
                    </Badge>
                    <Badge
                      v-if="log.approval_required"
                      variant="warning"
                      size="sm"
                      class="whitespace-nowrap"
                      :title="$t('logs.auditSummary.approvalRequired')"
                    >
                      {{ $t('logs.auditSummary.approvalRequired') }}
                    </Badge>
                    <Badge
                      v-if="log.verification_required"
                      variant="info"
                      size="sm"
                      class="whitespace-nowrap"
                      :title="$t('logs.auditSummary.verificationRequired')"
                    >
                      {{ $t('logs.auditSummary.verificationRequired') }}
                    </Badge>
                  </div>
                  <div v-if="log.risk_title" class="max-w-[140px] truncate text-xs text-[var(--text-tertiary)]" :title="log.risk_title">
                    {{ log.risk_title }}
                  </div>
                </div>
              </td>
              <td class="py-3 px-4 text-sm">
                <div class="max-w-full">
                  <!-- 短内容直接显示 -->
                  <span v-if="!isContentLong(formatContent(log.content))" class="text-[var(--text-secondary)]">
                    {{ formatContent(log.content) }}
                  </span>
                  <!-- 长内容可展开 -->
                  <template v-else>
                    <div v-if="!expandedRows.has(log.id)" class="flex items-center gap-1.5">
                      <span class="text-[var(--text-secondary)] truncate flex-1">{{ formatContent(log.content).slice(0, 50) }}...</span>
                      <button
                        type="button"
                        class="flex-shrink-0 p-1 rounded-md text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                        :title="$t('logs.expand')"
                        @click="toggleContent(log.id)"
                      >
                        <ChevronDown class="w-4 h-4" />
                      </button>
                    </div>
                    <div v-else class="space-y-2">
                      <p class="text-[var(--text-secondary)] whitespace-pre-wrap break-words text-xs leading-relaxed">{{ formatContent(log.content) }}</p>
                      <button
                        type="button"
                        class="text-xs flex items-center gap-1 text-[var(--accent)] hover:underline transition-colors"
                        @click="toggleContent(log.id)"
                      >
                        <ChevronUp class="w-3.5 h-3.5" />
                        {{ $t('logs.collapse') }}
                      </button>
                    </div>
                  </template>
                </div>
              </td>
              <td class="py-3 px-4 whitespace-nowrap text-center">
                <Badge :variant="getResultBadgeVariant(log.result)" size="sm" class="whitespace-nowrap">
                  {{ formatResult(log.result) }}
                </Badge>
              </td>
            </tr>
          </tbody>
        </table>

        <!-- Desktop Pagination -->
        <div
          v-if="total > 0"
          class="border-t border-[var(--border-color)] px-4 py-3"
        >
          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            show-size-changer
            :page-size-options="[10, 30, 50, 100]"
            @update:page="handlePageChange"
            @update:page-size="handlePageSizeUpdate"
          >
            <template #total="{ total: tTotal, page: cPage, totalPages: tPages }">
              <span>{{ $t('logs.totalRecords', { total: tTotal, page: cPage, totalPages: tPages }) }}</span>
            </template>
          </Pagination>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
/* Apple/Linear finish: uppercase micro-label audit-table headers */
thead th {
  text-transform: uppercase;
  letter-spacing: 0.05em;
}

@media (prefers-reduced-motion: reduce) {
  * {
    transition: none !important;
    animation: none !important;
  }
}
</style>
