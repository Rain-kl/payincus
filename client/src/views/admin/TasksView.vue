<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import {
  AlertCircle,
  AlertTriangle,
  Calendar,
  Clock,
  Download,
  FileText,
  Layers,
  Loader2,
  RefreshCw,
  RotateCcw,
  Search,
  Server,
  Square,
  Terminal
} from 'lucide-vue-next'
import api, { type OperationTask, type OperationTaskLog } from '@/api/admin'
import DrawerModal from '@/components/DrawerModal.vue'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select,
  Table
} from '@/components/ui'
import type { BadgeVariant, SelectOption, TableColumn } from '@/components/ui'
import { useReveal } from '@/composables/useReveal'
import { buildApiUrl } from '@/utils/api-url'

defineOptions({ name: 'AdminTasksView' })

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const items = ref<OperationTask[]>([])
const total = ref(0)
const page = ref(1)
const search = ref('')
const status = ref('')
const kind = ref('')
const loading = ref(false)
const listError = ref('')
const selected = ref<OperationTask | null>(null)
const logs = ref<OperationTaskLog[]>([])
const drawer = ref(false)
const connection = ref('connecting')
const streamError = ref('')
const follow = ref(true)
const logPanel = ref<HTMLElement | null>(null)
const now = ref(Date.now())
const stopTarget = ref<OperationTask | null>(null)
const stopping = ref(false)
const stopError = ref('')

let clockTimer: ReturnType<typeof setInterval> | undefined
let disposed = false
let refreshTimer: ReturnType<typeof setInterval> | undefined
let controller: AbortController | undefined
let session = 0

const key = (name: string) => `operationTasks.${name}`

const statusOptions = computed<SelectOption[]>(() => [
  { label: t(key('allStatuses')), value: '' },
  { label: t(key('running')), value: 'running' },
  { label: t(key('success')), value: 'success' },
  { label: t(key('failed')), value: 'failed' },
  { label: t(key('cancelled')), value: 'cancelled' },
  { label: t(key('interrupted')), value: 'interrupted' }
])

const kindOptions = computed<SelectOption[]>(() => [
  { label: t(key('allKinds')), value: '' },
  { label: t(key('create')), value: 'create' },
  { label: t(key('destroy')), value: 'destroy' },
  { label: t(key('node_recover')), value: 'node_recover' }
])

function getStatusBadgeVariant(taskStatus: string): BadgeVariant {
  switch (taskStatus) {
    case 'running':
      return 'warning'
    case 'success':
      return 'success'
    case 'failed':
    case 'interrupted':
      return 'error'
    case 'cancelled':
    default:
      return 'default'
  }
}

function getKindBadgeVariant(taskKind: string): BadgeVariant {
  switch (taskKind) {
    case 'create':
      return 'info'
    case 'destroy':
      return 'warning'
    case 'node_recover':
    default:
      return 'default'
  }
}

const columns = computed<TableColumn<OperationTask>[]>(() => [
  { key: 'instance', title: t(key('instance')), width: '28%' },
  { key: 'kind', title: t(key('allKinds')), width: '10%' },
  { key: 'status', title: t(key('allStatuses')), width: '14%' },
  { key: 'host', title: t(key('host')), width: '15%' },
  { key: 'created', title: t(key('created')), width: '16%' },
  { key: 'duration', title: t(key('duration')), width: '10%' },
  { key: 'actions', title: t(key('actions')), width: '7%', align: 'right' }
])

const date = (value: string | null) => (value ? new Date(value).toLocaleString() : '—')

function duration(task: OperationTask) {
  const end = task.finishedAt ? new Date(task.finishedAt).getTime() : task.status === 'running' ? now.value : NaN
  if (!Number.isFinite(end)) return '—'
  const seconds = Math.max(0, Math.floor((end - new Date(task.createdAt).getTime()) / 1000))
  return `${Math.floor(seconds / 3600).toString().padStart(2, '0')}:${Math.floor(seconds / 60 % 60).toString().padStart(2, '0')}:${(seconds % 60).toString().padStart(2, '0')}`
}

function askStop(task: OperationTask) {
  stopError.value = ''
  stopTarget.value = task
}

async function confirmStop() {
  if (!stopTarget.value || stopping.value) return
  stopping.value = true
  stopError.value = ''
  try {
    await api.operationTasks.stop(stopTarget.value.id)
    if (selected.value && selected.value.id === stopTarget.value.id) {
      selected.value.stopping = true
    }
    stopTarget.value = null
    await load()
  } catch (error) {
    stopError.value = error && typeof error === 'object' && 'message' in error && typeof error.message === 'string'
      ? error.message
      : t(key('stopFailed'))
  } finally {
    stopping.value = false
  }
}

async function load() {
  if (loading.value) return
  loading.value = true
  try {
    const data = await api.operationTasks.list({
      page: page.value,
      search: search.value || undefined,
      status: status.value || undefined,
      kind: kind.value || undefined
    })
    if (disposed) return
    items.value = data.items
    total.value = data.total
    listError.value = ''
  } catch (error) {
    if (!disposed) {
      listError.value = error instanceof Error ? error.message : t(key('loadFailed'))
    }
  } finally {
    loading.value = false
  }
}

async function filter() {
  page.value = 1
  await load()
}

async function resetFilter() {
  search.value = ''
  status.value = ''
  kind.value = ''
  page.value = 1
  await load()
}

function handlePageChange(newPage: number) {
  if (newPage === page.value || loading.value) return
  page.value = newPage
  void load()
}

function open(task: OperationTask) {
  void router.replace({ query: { ...route.query, task: task.id } })
}

function close() {
  const query = { ...route.query }
  delete query.task
  void router.replace({ query })
}

function stop() {
  session++
  controller?.abort()
  controller = undefined
}

async function scroll() {
  if (follow.value) {
    await nextTick()
    logPanel.value?.scrollTo({ top: logPanel.value.scrollHeight })
  }
}

async function stream(id: number) {
  stop()
  const ownSession = session
  const abort = new AbortController()
  controller = abort
  logs.value = []
  selected.value = items.value.find(item => item.id === id) || null
  drawer.value = true
  connection.value = 'connecting'
  streamError.value = ''
  let cursor = 0
  while (!abort.signal.aborted && ownSession === session) {
    try {
      // Refresh access tokens through the standard API interceptor before each SSE connection.
      await api.operationTasks.list({ page: 1 })
      if (abort.signal.aborted) break
      const token = localStorage.getItem('token')
      const response = await fetch(buildApiUrl(`/admin/tasks/${id}/logs/stream?after=${cursor}`), {
        headers: { Authorization: `Bearer ${token || ''}` }, signal: abort.signal, credentials: 'include'
      })
      if (!response.ok || !response.body) {
        if ([400, 401, 403, 404].includes(response.status)) {
          connection.value = 'loadFailed'
          streamError.value = `${t(key('loadFailed'))} (HTTP ${response.status})`
          break
        }
        throw new Error(`HTTP ${response.status}`)
      }
      connection.value = 'connected'
      streamError.value = ''
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      let ended = false
      try {
        while (!abort.signal.aborted) {
          const result = await reader.read()
          if (result.done) break
          buffer += decoder.decode(result.value, { stream: true }).replace(/\r\n/g, '\n')
          if (buffer.length > 1024 * 1024) throw new Error('Invalid log stream')
          let split: number
          while ((split = buffer.indexOf('\n\n')) >= 0) {
            const block = buffer.slice(0, split)
            buffer = buffer.slice(split + 2)
            const event = block.split('\n').find(line => line.startsWith('event:'))?.slice(6).trim()
            const payload = block.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')
            if (!payload) continue
            const data = JSON.parse(payload)
            if (ownSession !== session) return
            if (event === 'log' && data.id > cursor) {
              logs.value.push(data)
              cursor = data.id
            }
            if (event === 'task') selected.value = data
            if (event === 'stream-error') throw new Error(data.message)
            if (event === 'end') {
              ended = true
              connection.value = 'complete'
            }
          }
          await scroll()
        }
      } finally {
        await reader.cancel().catch(() => {})
        reader.releaseLock()
      }
      if (ended) {
        void load()
        break
      }
    } catch (error) {
      if (abort.signal.aborted || ownSession !== session) return
      streamError.value = error instanceof Error ? error.message : t(key('loadFailed'))
    }
    if (abort.signal.aborted || ownSession !== session) break
    connection.value = 'reconnecting'
    await new Promise<void>(resolve => {
      const done = () => {
        clearTimeout(timer)
        abort.signal.removeEventListener('abort', done)
        resolve()
      }
      const timer = setTimeout(done, 2000)
      abort.signal.addEventListener('abort', done, { once: true })
    })
  }
}

function download() {
  const content = logs.value.map(log => `${date(log.createdAt)} [${log.source}] [${log.level}] ${log.message}`).join('\n')
  const url = URL.createObjectURL(new Blob([content], { type: 'text/plain;charset=utf-8' }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `task-${route.query.task}.log`
  anchor.click()
  URL.revokeObjectURL(url)
}

watch(
  () => route.query.task,
  value => {
    const id = Number(value)
    if (Number.isSafeInteger(id) && id > 0) void stream(id)
    else {
      stop()
      drawer.value = false
      selected.value = null
    }
  },
  { immediate: true }
)

watch(drawer, value => {
  if (!value && route.query.task) close()
})

watch(follow, () => {
  void scroll()
})

onMounted(() => {
  void load()
  refreshTimer = setInterval(() => { void load() }, 5000)
  clockTimer = setInterval(() => { now.value = Date.now() }, 1000)
})

onUnmounted(() => {
  disposed = true
  stop()
  clearInterval(refreshTimer)
  clearInterval(clockTimer)
})
</script>

<template>
  <div ref="revealRoot" class="space-y-6">
    <!-- 页面标题与刷新操作 -->
    <PageHeader
      data-reveal
      :title="t(key('title'))"
      :description="t(key('description'))"
    >
      <template #actions>
        <div class="flex items-center gap-2.5">
          <Badge v-if="total > 0" variant="default" size="md">
            {{ total }}
          </Badge>
          <Button
            variant="secondary"
            size="sm"
            :loading="loading"
            class="shrink-0"
            @click="load"
          >
            <template #icon>
              <RefreshCw class="w-4 h-4" :class="{ 'animate-spin': loading }" />
            </template>
            {{ t(key('refresh')) }}
          </Button>
        </div>
      </template>
    </PageHeader>

    <!-- 筛选卡片 -->
    <Card data-reveal padding="sm">
      <form class="flex flex-wrap items-center gap-3" @submit.prevent="filter">
        <div class="w-full sm:w-64">
          <Input
            v-model="search"
            size="sm"
            :placeholder="t(key('instance'))"
            :aria-label="t(key('instance'))"
            :maxlength="100"
            clearable
            @enter="filter"
            @clear="filter"
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>
        <div class="w-full sm:w-44">
          <Select
            v-model="status"
            size="sm"
            :options="statusOptions"
            :aria-label="t(key('allStatuses'))"
            @change="filter"
          />
        </div>
        <div class="w-full sm:w-44">
          <Select
            v-model="kind"
            size="sm"
            :options="kindOptions"
            :aria-label="t(key('allKinds'))"
            @change="filter"
          />
        </div>
        <div class="flex items-center gap-2">
          <Button
            variant="primary"
            size="sm"
            type="submit"
          >
            <template #icon>
              <Search class="w-3.5 h-3.5" />
            </template>
            {{ t(key('search')) }}
          </Button>
          <Button
            v-if="search || status || kind"
            variant="secondary"
            size="sm"
            type="button"
            @click="resetFilter"
          >
            <template #icon>
              <RotateCcw class="w-3.5 h-3.5" />
            </template>
            {{ t('common.reset') }}
          </Button>
        </div>
      </form>
    </Card>

    <!-- 列表加载错误提示 -->
    <div
      v-if="listError"
      role="alert"
      class="flex items-center gap-2.5 rounded-2xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-4 text-sm text-[var(--error)] shadow-sm"
    >
      <AlertCircle class="h-4 w-4 shrink-0" />
      <span class="flex-1">{{ listError }}</span>
      <Button
        variant="ghost"
        size="sm"
        @click="load"
      >
        {{ t(key('refresh')) }}
      </Button>
    </div>

    <!-- 任务列表表格 (桌面端定宽无水平溢出 + 移动端自适应卡片) -->
    <div data-reveal>
      <Table
        :columns="columns"
        :data="items"
        :loading="loading && items.length === 0"
        :fixed="true"
        row-key="id"
      >
        <!-- 空状态 -->
        <template #empty>
          <EmptyState
            :title="t(key('empty'))"
            :description="search || status || kind ? '没有匹配当前筛选条件的任务' : undefined"
          />
        </template>

        <!-- 单元格：实例名称与错误 -->
        <template #cell-instance="{ row: task }">
          <div class="min-w-0 pr-2">
            <button
              type="button"
              class="text-left font-medium text-sm text-[var(--text-primary)] hover:text-[var(--accent)] hover:underline truncate block max-w-full transition-colors"
              @click="open(task)"
            >
              {{ task.instanceName }}
            </button>
            <div class="flex items-center gap-2 mt-0.5">
              <span class="font-mono text-xs text-[var(--text-tertiary)]">#{{ task.id }}</span>
            </div>
            <div v-if="task.error" class="mt-1">
              <button
                type="button"
                class="text-xs text-[var(--error)] text-left hover:underline break-words line-clamp-2 cursor-pointer inline-flex items-center gap-1"
                :title="task.error"
                @click="open(task)"
              >
                <AlertCircle class="w-3.5 h-3.5 shrink-0" />
                <span class="truncate">{{ task.error }}</span>
              </button>
            </div>
          </div>
        </template>

        <!-- 单元格：任务类型 -->
        <template #cell-kind="{ row: task }">
          <Badge :variant="getKindBadgeVariant(task.kind)" size="sm">
            {{ t(key(task.kind)) }}
          </Badge>
        </template>

        <!-- 单元格：状态与停止中提示 -->
        <template #cell-status="{ row: task }">
          <div class="flex flex-col items-start gap-1">
            <Badge
              :variant="getStatusBadgeVariant(task.status)"
              size="sm"
              :dot="true"
              :pulse="task.status === 'running'"
            >
              {{ t(key(task.status)) }}
            </Badge>
            <div v-if="task.stopping" class="flex items-center gap-1 text-[var(--warning)] text-xs font-medium">
              <Loader2 class="w-3 h-3 animate-spin" />
              <span>{{ t(key('stopping')) }}</span>
            </div>
          </div>
        </template>

        <!-- 单元格：节点 -->
        <template #cell-host="{ row: task }">
          <span class="text-xs font-mono text-[var(--text-secondary)] truncate block" :title="task.hostName || String(task.hostId)">
            {{ task.hostName || `#${task.hostId}` }}
          </span>
        </template>

        <!-- 单元格：创建时间 -->
        <template #cell-created="{ row: task }">
          <span class="text-xs font-mono tabular-nums text-[var(--text-secondary)] whitespace-nowrap">
            {{ date(task.createdAt) }}
          </span>
        </template>

        <!-- 单元格：耗时 -->
        <template #cell-duration="{ row: task }">
          <span class="font-mono text-xs tabular-nums text-[var(--text-secondary)]">
            {{ duration(task) }}
          </span>
        </template>

        <!-- 单元格：操作 -->
        <template #cell-actions="{ row: task }">
          <div class="flex items-center justify-end gap-1.5">
            <Button
              variant="secondary"
              size="sm"
              class="!h-8 !w-8 !p-0 !min-h-0 shrink-0"
              :title="t(key('view'))"
              :aria-label="t(key('view'))"
              @click="open(task)"
            >
              <FileText class="w-4 h-4 text-[var(--text-secondary)]" />
            </Button>
            <Button
              v-if="task.status === 'running'"
              variant="danger"
              size="sm"
              class="!h-8 !w-8 !p-0 !min-h-0 shrink-0"
              :disabled="!task.canStop || task.stopping"
              :title="t(key(task.stopping ? 'stopping' : 'stopTask'))"
              :aria-label="t(key('stopTask'))"
              @click="askStop(task)"
            >
              <Square class="w-3.5 h-3.5 fill-current" />
            </Button>
          </div>
        </template>

        <!-- 移动端自适应卡片视图 -->
        <template #mobile="{ row: task }">
          <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 space-y-3 shadow-sm transition-all duration-200 active:scale-[0.99]">
            <div class="flex items-start justify-between gap-2">
              <div class="min-w-0 flex-1">
                <button
                  type="button"
                  class="font-semibold text-sm text-[var(--text-primary)] hover:text-[var(--accent)] hover:underline truncate block text-left"
                  @click="open(task)"
                >
                  {{ task.instanceName }}
                </button>
                <span class="font-mono text-xs text-[var(--text-tertiary)]">#{{ task.id }}</span>
              </div>
              <Badge :variant="getStatusBadgeVariant(task.status)" size="sm" :dot="true" :pulse="task.status === 'running'">
                {{ t(key(task.status)) }}
              </Badge>
            </div>

            <div class="grid grid-cols-2 gap-2 text-xs text-[var(--text-secondary)]">
              <div class="flex items-center gap-1.5 truncate">
                <Layers class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span>{{ t(key(task.kind)) }}</span>
              </div>
              <div class="flex items-center gap-1.5 truncate">
                <Server class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span class="font-mono truncate">{{ task.hostName || `#${task.hostId}` }}</span>
              </div>
              <div class="flex items-center gap-1.5 truncate">
                <Clock class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span>{{ t(key('duration')) }}: <span class="font-mono tabular-nums">{{ duration(task) }}</span></span>
              </div>
              <div class="flex items-center gap-1.5 truncate">
                <Calendar class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
                <span class="font-mono tabular-nums truncate">{{ date(task.createdAt) }}</span>
              </div>
            </div>

            <div
              v-if="task.error"
              class="rounded-xl bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_20%,var(--bg-surface))] p-2.5 text-xs text-[var(--error)] break-words cursor-pointer"
              @click="open(task)"
            >
              {{ task.error }}
            </div>

            <div class="pt-2 flex items-center justify-between border-t border-[var(--border-color)]">
              <div v-if="task.stopping" class="flex items-center gap-1.5 text-xs text-[var(--warning)] font-medium">
                <Loader2 class="w-3.5 h-3.5 animate-spin" />
                <span>{{ t(key('stopping')) }}</span>
              </div>
              <div v-else class="text-xs text-[var(--text-tertiary)] font-mono">
                {{ date(task.createdAt) }}
              </div>

              <div class="flex items-center gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  @click="open(task)"
                >
                  <FileText class="w-3.5 h-3.5" />
                  {{ t(key('view')) }}
                </Button>
                <Button
                  v-if="task.status === 'running' && task.canStop"
                  variant="danger"
                  size="sm"
                  :disabled="task.stopping"
                  @click="askStop(task)"
                >
                  <Square class="w-3 h-3 fill-current" />
                  {{ t(key('stopTask')) }}
                </Button>
              </div>
            </div>
          </div>
        </template>

        <!-- 表格底部独立分页器 -->
        <template #pagination>
          <Pagination
            v-if="total > 0"
            :page="page"
            :page-size="20"
            :total="total"
            :show-size-changer="false"
            @update:page="handlePageChange"
          />
        </template>
      </Table>
    </div>

    <!-- 任务详情与实时日志抽屉 -->
    <DrawerModal
      v-model="drawer"
      :title="t(key('detail'))"
      max-width="max-w-4xl"
    >
      <div class="space-y-4">
        <!-- 任务概览卡片 -->
        <div v-if="selected" class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 sm:p-5 space-y-4 shadow-sm">
          <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="font-mono text-sm text-[var(--text-tertiary)]">#{{ selected.id }}</span>
                <h3 class="font-semibold text-base sm:text-lg text-[var(--text-primary)] truncate">
                  {{ selected.instanceName }}
                </h3>
              </div>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ t(key(selected.kind)) }} · {{ t(key('host')) }} #{{ selected.hostId }}
                <span v-if="selected.hostName">({{ selected.hostName }})</span>
              </p>
            </div>

            <div class="flex items-center gap-2 shrink-0">
              <Badge
                :variant="getStatusBadgeVariant(selected.status)"
                size="md"
                :dot="true"
                :pulse="selected.status === 'running'"
              >
                {{ t(key(selected.status)) }}
              </Badge>

              <Button
                v-if="selected.canStop"
                variant="danger"
                size="sm"
                :disabled="selected.stopping"
                @click="askStop(selected)"
              >
                <Square class="w-3.5 h-3.5 fill-current" />
                {{ t(key('stopTask')) }}
              </Button>
            </div>
          </div>

          <!-- 任务指标网格 -->
          <div class="grid grid-cols-2 sm:grid-cols-3 gap-3 pt-3 border-t border-[var(--border-color)] text-xs">
            <div>
              <span class="text-[var(--text-tertiary)] block">{{ t(key('created')) }}</span>
              <span class="font-mono tabular-nums text-[var(--text-secondary)] mt-0.5 block">
                {{ date(selected.createdAt) }}
              </span>
            </div>
            <div>
              <span class="text-[var(--text-tertiary)] block">{{ t(key('finished')) }}</span>
              <span class="font-mono tabular-nums text-[var(--text-secondary)] mt-0.5 block">
                {{ date(selected.finishedAt) }}
              </span>
            </div>
            <div>
              <span class="text-[var(--text-tertiary)] block">{{ t(key('duration')) }}</span>
              <span class="font-mono tabular-nums font-medium text-[var(--text-primary)] mt-0.5 block">
                {{ duration(selected) }}
              </span>
            </div>
          </div>

          <!-- 停止中提示 -->
          <div
            v-if="selected.stopping"
            class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-3 text-xs text-[var(--warning)] font-medium"
          >
            <Loader2 class="w-4 h-4 animate-spin shrink-0" />
            <span>{{ t(key('stopping')) }}</span>
          </div>

          <!-- 执行中断提示 -->
          <div
            v-if="selected.status === 'interrupted'"
            class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-3 text-xs text-[var(--warning)]"
          >
            <AlertCircle class="w-4 h-4 shrink-0" />
            <span>{{ t(key('interruptedHint')) }}</span>
          </div>

          <!-- 错误信息 -->
          <div
            v-if="selected.error"
            role="alert"
            class="flex items-start gap-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-3 text-xs text-[var(--error)] break-words"
          >
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1 font-mono">{{ selected.error }}</span>
          </div>
        </div>

        <!-- 骨架占位 -->
        <div
          v-else
          class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-5 animate-pulse space-y-3"
        >
          <div class="h-5 bg-[var(--bg-tertiary)] rounded w-1/3"></div>
          <div class="h-4 bg-[var(--bg-tertiary)] rounded w-1/2"></div>
        </div>

        <!-- 日志控制工具栏 -->
        <div class="flex flex-wrap items-center justify-between gap-3 text-xs pt-1">
          <!-- 连接状态 -->
          <div class="flex items-center gap-2">
            <span
              class="w-2 h-2 rounded-full inline-block"
              :class="{
                'bg-[var(--warning)] animate-ping': connection === 'connecting' || connection === 'reconnecting',
                'bg-[var(--success)]': connection === 'connected',
                'bg-[var(--accent)]': connection === 'complete',
                'bg-[var(--error)]': connection === 'loadFailed'
              }"
            />
            <span role="status" class="text-[var(--text-secondary)]">
              {{ t(key(connection)) }}
            </span>
            <button
              v-if="connection === 'loadFailed' && selected"
              type="button"
              class="inline-flex items-center gap-1 text-[var(--accent)] hover:underline ml-1 font-medium"
              @click="stream(selected.id)"
            >
              <RotateCcw class="w-3 h-3" />
              <span>{{ t('common.retry') }}</span>
            </button>
          </div>

          <!-- 选项与下载 -->
          <div class="flex items-center gap-3">
            <label class="flex items-center gap-1.5 cursor-pointer text-[var(--text-secondary)] hover:text-[var(--text-primary)] select-none">
              <input
                v-model="follow"
                type="checkbox"
                class="rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] focus:ring-offset-0 bg-[var(--bg-surface)]"
              />
              <span>{{ t(key('follow')) }}</span>
            </label>

            <Button
              variant="secondary"
              size="sm"
              :disabled="!logs.length"
              @click="download"
            >
              <template #icon>
                <Download class="w-3.5 h-3.5" />
              </template>
              {{ t(key('download')) }}
            </Button>
          </div>
        </div>

        <!-- 流错误提示 -->
        <div
          v-if="streamError"
          role="alert"
          class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-3 text-xs text-[var(--error)] break-words"
        >
          <AlertCircle class="w-4 h-4 shrink-0" />
          <span class="flex-1">{{ streamError }}</span>
          <Button
            v-if="selected"
            variant="ghost"
            size="sm"
            class="!text-xs !py-1 !px-2"
            @click="stream(selected.id)"
          >
            {{ t('common.retry') }}
          </Button>
        </div>

        <!-- 终端日志控制台 -->
        <div
          ref="logPanel"
          class="task-logs rounded-2xl border border-[var(--border-color)] bg-[var(--bg-tertiary)] p-4 font-mono text-xs leading-relaxed select-text shadow-sm"
          role="log"
          :aria-label="t(key('view'))"
        >
          <div
            v-if="!logs.length"
            class="h-full flex flex-col items-center justify-center text-center py-12 text-[var(--text-tertiary)] space-y-2"
          >
            <Loader2 v-if="connection === 'connecting'" class="w-6 h-6 animate-spin text-[var(--accent)]" />
            <Terminal v-else class="w-6 h-6 stroke-[1.5]" />
            <p>{{ t(key('waiting')) }}</p>
          </div>

          <div v-else class="space-y-1">
            <div
              v-for="log in logs"
              :key="log.id"
              class="whitespace-pre-wrap break-all py-0.5 hover:bg-[var(--bg-surface-soft)] rounded px-1 -mx-1 transition-colors"
              :class="{
                'text-[var(--error)]': log.level === 'error',
                'text-[var(--warning)]': log.level === 'warn',
                'text-[var(--text-primary)]': log.level !== 'error' && log.level !== 'warn'
              }"
            >
              <span class="text-[var(--text-tertiary)] mr-2 select-none">
                {{ date(log.createdAt) }} [{{ t(key(log.source)) }}]
              </span>
              <span>{{ log.message }}</span>
            </div>
          </div>
        </div>
      </div>
    </DrawerModal>

    <!-- 强行停止任务确认对话框 -->
    <DrawerModal
      :model-value="Boolean(stopTarget)"
      :title="t(key('stopTask'))"
      :closable="!stopping"
      :close-on-backdrop="!stopping"
      max-width="max-w-lg"
      @update:model-value="value => { if (!value && !stopping) stopTarget = null }"
    >
      <div class="space-y-4">
        <div class="flex items-start gap-3">
          <div class="w-10 h-10 rounded-2xl bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] flex items-center justify-center text-[var(--error)] shrink-0">
            <AlertTriangle class="w-5 h-5" />
          </div>
          <div class="min-w-0 flex-1">
            <p class="font-semibold text-base text-[var(--text-primary)] break-all">
              {{ stopTarget?.instanceName }}
            </p>
            <p class="text-xs text-[var(--text-tertiary)] font-mono mt-0.5">
              #{{ stopTarget?.id }} · {{ stopTarget?.kind ? t(key(stopTarget.kind)) : '' }}
            </p>
          </div>
        </div>

        <p class="text-xs text-[var(--text-secondary)] leading-relaxed">
          {{ t(key('stopHint')) }}
        </p>

        <div
          v-if="stopError"
          role="alert"
          class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-3 text-xs text-[var(--error)]"
        >
          <AlertCircle class="w-4 h-4 shrink-0" />
          <span>{{ stopError }}</span>
        </div>
      </div>

      <template #footer>
        <div class="flex items-center justify-end gap-2.5 w-full">
          <Button
            variant="secondary"
            size="sm"
            :disabled="stopping"
            @click="stopTarget = null"
          >
            {{ t(key('cancel')) }}
          </Button>
          <Button
            variant="danger"
            size="sm"
            :loading="stopping"
            @click="confirmStop"
          >
            {{ t(key(stopping ? 'stopping' : 'confirmStop')) }}
          </Button>
        </div>
      </template>
    </DrawerModal>
  </div>
</template>

<style scoped>
.task-logs {
  height: 55vh;
  overflow-y: auto;
}
</style>
