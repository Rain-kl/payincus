<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  AlertTriangle,
  ArrowUpCircle,
  Calendar,
  Clock,
  Cpu,
  DownloadCloud,
  GitBranch,
  History,
  Layers,
  Package,
  RefreshCw,
  RotateCcw,
  Sparkles,
  Terminal
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import { systemSettingsNavigationItems } from '@/constants/adminSettings'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  PageHeader
} from '@/components/ui'
import type {
  AvailableSystemUpdate,
  SystemUpdateCheckResult,
  SystemUpdateTask,
  VersionMetadata
} from '@/types/api'

const toast = useToast()
const route = useRoute()
const { t } = useI18n()

const loading = ref(true)
const checking = ref(false)
const starting = ref(false)
const rollingBackTaskId = ref<number | null>(null)
const version = ref<VersionMetadata | null>(null)
const updateCheck = ref<SystemUpdateCheckResult | null>(null)
const tasks = ref<SystemUpdateTask[]>([])
const selectedTaskId = ref<number | null>(null)
const selectedTaskLogs = ref('')
const logsLoading = ref(false)
const taskPage = ref(1)
const TASKS_PER_PAGE = 7
let pollTimer: number | null = null

const selectedTask = computed(() =>
  tasks.value.find(task => task.id === selectedTaskId.value) || null
)

const latestUpdate = computed<AvailableSystemUpdate | null>(() =>
  updateCheck.value?.latest || null
)

const displayLatestUpdate = computed<AvailableSystemUpdate | null>(() => {
  if (latestUpdate.value) return latestUpdate.value
  if (!currentVersion.value) return null
  return {
    version: currentVersion.value.version,
    commit: currentVersion.value.gitCommit,
    date: currentVersion.value.buildTime || currentVersion.value.deployedAt,
    changelog: currentVersion.value.changelog || [],
    ota: {
      manifestAvailable: false,
      manifestUrl: null,
      artifacts: [],
      error: updateCheck.value
        ? '当前版本已是最新版本，未发现新的 OTA 发行包。'
        : '检查更新后显示 OTA manifest 和包校验信息。'
    }
  }
})

const latestOtaArtifacts = computed(() =>
  displayLatestUpdate.value?.ota?.artifacts || []
)

const repositoryUnavailable = computed(() =>
  updateCheck.value?.repositoryAvailable === false
)

const hasRunningTask = computed(() =>
  tasks.value.some(task => task.status === 'pending' || task.status === 'running')
)

const currentVersion = computed(() => updateCheck.value?.current || version.value)

const canManageUpdates = computed(() => updateCheck.value?.canManageUpdates !== false)

const totalTaskPages = computed(() =>
  Math.max(1, Math.ceil(tasks.value.length / TASKS_PER_PAGE))
)

const paginatedTasks = computed(() => {
  const start = (taskPage.value - 1) * TASKS_PER_PAGE
  return tasks.value.slice(start, start + TASKS_PER_PAGE)
})

const updateActionLabel = computed(() => {
  if (starting.value) return '启动中...'
  if (hasRunningTask.value) return '已有更新任务执行中'
  if (repositoryUnavailable.value) return '无法在线更新'
  if (updateCheck.value && !updateCheck.value.updateAvailable) return '已更新至最新版本'
  if (updateCheck.value && !canManageUpdates.value) return '仅超管可更新'
  if (!updateCheck.value) return '检查更新后可操作'
  return '更新到最新版本'
})

const updateActionDisabled = computed(() =>
  repositoryUnavailable.value ||
  !canManageUpdates.value ||
  !latestUpdate.value ||
  !updateCheck.value?.updateAvailable ||
  starting.value ||
  hasRunningTask.value
)

function formatDate(value: string | null | undefined): string {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

function formatBytes(value: number | null | undefined): string {
  if (!value || value < 0) return '-'
  const units = ['B', 'KB', 'MB', 'GB']
  let size = value
  let index = 0
  while (size >= 1024 && index < units.length - 1) {
    size /= 1024
    index += 1
  }
  return `${size.toFixed(index === 0 ? 0 : 1)} ${units[index]}`
}

function shortSha(value: string | null | undefined): string {
  return value ? `${value.slice(0, 12)}...` : '-'
}

function statusLabel(status: SystemUpdateTask['status']): string {
  const labels: Record<SystemUpdateTask['status'], string> = {
    pending: '等待中',
    running: '执行中',
    success: '成功',
    failed: '失败',
    rolled_back: '已回滚'
  }
  return labels[status]
}

function statusBadgeVariant(status: SystemUpdateTask['status']): 'default' | 'info' | 'success' | 'error' | 'warning' {
  switch (status) {
    case 'running':
      return 'info'
    case 'success':
      return 'success'
    case 'failed':
      return 'error'
    case 'rolled_back':
      return 'warning'
    case 'pending':
    default:
      return 'default'
  }
}

async function loadVersion() {
  const response = await api.systemUpdate.getVersion()
  version.value = response.version
}

async function loadTasks() {
  const response = await api.systemUpdate.listTasks()
  tasks.value = response.tasks
  if (selectedTaskId.value && !response.tasks.some(task => task.id === selectedTaskId.value)) {
    selectedTaskId.value = null
  }
  if (!selectedTaskId.value && response.tasks.length > 0) {
    selectedTaskId.value = response.tasks[0].id
  }
  ensureSelectedTaskVisible()
}

async function loadSelectedTaskLogs() {
  if (!selectedTaskId.value) {
    selectedTaskLogs.value = ''
    return
  }
  logsLoading.value = true
  try {
    const response = await api.systemUpdate.getTaskLogs(selectedTaskId.value)
    selectedTaskLogs.value = response.logs
  } catch (err: any) {
    selectedTaskLogs.value = ''
    toast.error('加载更新日志失败：' + (err?.message || String(err)))
  } finally {
    logsLoading.value = false
  }
}

async function refreshAll() {
  loading.value = true
  try {
    await Promise.all([loadVersion(), loadTasks(), loadUpdateCheck(false)])
    await loadSelectedTaskLogs()
  } catch (err: any) {
    toast.error('加载版本信息失败：' + (err?.message || String(err)))
  } finally {
    loading.value = false
  }
}

async function loadUpdateCheck(notify: boolean) {
  checking.value = true
  try {
    updateCheck.value = await api.systemUpdate.check()
    if (notify) {
      if (repositoryUnavailable.value) {
        toast.error(updateCheck.value.repositoryError || '当前部署目录不是 Git 工作区，无法在线更新')
      } else if (updateCheck.value.updateAvailable) {
        toast.success('发现可更新版本')
      } else {
        toast.success('当前已经是最新版本')
      }
    }
  } catch (err: any) {
    if (notify) toast.error('检查更新失败：' + (err?.message || String(err)))
  } finally {
    checking.value = false
  }
}

async function checkUpdates() {
  await loadUpdateCheck(true)
}

async function startUpdate(targetVersion: string) {
  if (!targetVersion || hasRunningTask.value) return
  const confirmed = window.confirm(`确认更新到 ${targetVersion}？更新会备份当前版本、执行数据库迁移并重启服务。`)
  if (!confirmed) return

  starting.value = true
  try {
    const response = await api.systemUpdate.start(targetVersion)
    selectedTaskId.value = response.task.id
    toast.success('更新任务已创建')
    await loadTasks()
    await loadSelectedTaskLogs()
  } catch (err: any) {
    toast.error('启动更新失败：' + (err?.message || String(err)))
  } finally {
    starting.value = false
  }
}

async function rollbackTask(task: SystemUpdateTask) {
  if (!task.backupPath || hasRunningTask.value) return
  const confirmed = window.confirm(`确认回滚任务 #${task.id} 的备份？该操作会重启服务。`)
  if (!confirmed) return

  rollingBackTaskId.value = task.id
  try {
    await api.systemUpdate.rollback(task.id)
    selectedTaskId.value = task.id
    toast.success('回滚任务已启动')
    await loadTasks()
    await loadSelectedTaskLogs()
  } catch (err: any) {
    toast.error('启动回滚失败：' + (err?.message || String(err)))
  } finally {
    rollingBackTaskId.value = null
  }
}

async function selectTask(task: SystemUpdateTask) {
  selectedTaskId.value = task.id
  ensureSelectedTaskVisible()
  await loadSelectedTaskLogs()
}

function setTaskPage(page: number) {
  taskPage.value = Math.min(Math.max(page, 1), totalTaskPages.value)
}

function ensureSelectedTaskVisible() {
  if (!selectedTaskId.value) {
    setTaskPage(taskPage.value)
    return
  }
  const index = tasks.value.findIndex(task => task.id === selectedTaskId.value)
  if (index >= 0) {
    taskPage.value = Math.floor(index / TASKS_PER_PAGE) + 1
  } else {
    setTaskPage(taskPage.value)
  }
}

function startPolling() {
  if (pollTimer !== null) return
  pollTimer = window.setInterval(async () => {
    if (!hasRunningTask.value && !selectedTask.value) return
    await loadTasks()
    await loadSelectedTaskLogs()
  }, 5000)
}

onMounted(async () => {
  await refreshAll()
  startPolling()
})

onUnmounted(() => {
  if (pollTimer !== null) {
    window.clearInterval(pollTimer)
  }
})
</script>

<template>
  <div class="max-w-6xl mx-auto space-y-6 pb-12">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('admin.system.title') || '系统设置'"
      description="通过 Git release tag 与 OTA 发行包进行受控在线升级，更新前自动执行备份、数据迁移与健康自检。"
    >
      <template #actions>
        <div class="flex items-center gap-2.5">
          <Button
            variant="secondary"
            size="sm"
            :loading="loading"
            class="shrink-0"
            @click="refreshAll"
          >
            <template #icon>
              <RefreshCw class="w-3.5 h-3.5" :class="{ 'animate-spin': loading }" />
            </template>
            刷新
          </Button>
          <Button
            variant="primary"
            size="sm"
            :loading="checking"
            :disabled="checking || hasRunningTask"
            class="shrink-0"
            @click="checkUpdates"
          >
            <template #icon>
              <DownloadCloud class="w-3.5 h-3.5" />
            </template>
            {{ checking ? '检查中...' : '检查更新' }}
          </Button>
        </div>
      </template>
    </PageHeader>

    <!-- Segmented Navigation Bar -->
    <nav class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto no-scrollbar shadow-sm">
      <router-link
        v-for="item in systemSettingsNavigationItems"
        :key="item.path"
        :to="item.path"
        class="shrink-0 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98] select-none"
        :class="route.path === item.path
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold border border-[var(--border-color)]/60'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/50 border border-transparent'"
      >
        {{ t(item.labelKey) }}
      </router-link>
    </nav>

    <!-- Skeleton Loading State -->
    <div v-if="loading" class="space-y-6 animate-pulse">
      <div class="grid gap-6 xl:grid-cols-2">
        <Card v-for="i in 2" :key="i" padding="md">
          <div class="space-y-4">
            <div class="h-5 w-32 bg-[var(--bg-surface-soft)] rounded-md"></div>
            <div class="h-10 w-44 bg-[var(--bg-surface-soft)] rounded-xl"></div>
            <div class="h-28 bg-[var(--bg-surface-soft)] rounded-xl"></div>
          </div>
        </Card>
      </div>
      <div class="grid gap-6 xl:grid-cols-[minmax(320px,420px)_1fr]">
        <Card padding="md">
          <div class="space-y-3">
            <div class="h-5 w-28 bg-[var(--bg-surface-soft)] rounded-md"></div>
            <div v-for="i in 4" :key="i" class="h-16 bg-[var(--bg-surface-soft)] rounded-xl"></div>
          </div>
        </Card>
        <Card padding="md">
          <div class="space-y-3">
            <div class="h-5 w-32 bg-[var(--bg-surface-soft)] rounded-md"></div>
            <div class="h-[420px] bg-[var(--bg-surface-soft)] rounded-xl"></div>
          </div>
        </Card>
      </div>
    </div>

    <!-- Main Content -->
    <template v-else>
      <!-- Dual Version Overview Cards -->
      <section class="grid gap-6 xl:grid-cols-2">
        <!-- Current Version Card -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div class="flex items-center gap-2">
                <Layers class="w-4 h-4 text-[var(--accent)]" />
                <h2 class="text-sm font-semibold uppercase tracking-wider text-[var(--text-secondary)]">当前运行版本</h2>
              </div>
              <Badge variant="info" size="sm" dot>当前运行</Badge>
            </div>
          </template>

          <div class="space-y-6">
            <div>
              <div class="font-mono text-3xl sm:text-4xl font-bold tracking-tight text-[var(--text-primary)] tabular-nums">
                {{ currentVersion?.version || '-' }}
              </div>
              <p class="text-xs text-[var(--text-tertiary)] mt-1">当前主进程加载并运行的生产版本</p>
            </div>

            <dl class="grid grid-cols-1 sm:grid-cols-2 gap-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-xs">
              <div class="space-y-1">
                <dt class="text-[var(--text-tertiary)] flex items-center gap-1.5">
                  <GitBranch class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                  <span>Git Tag</span>
                </dt>
                <dd class="font-mono font-medium text-[var(--text-primary)] truncate">
                  {{ currentVersion?.gitTag || '-' }}
                </dd>
              </div>
              <div class="space-y-1">
                <dt class="text-[var(--text-tertiary)] flex items-center gap-1.5">
                  <Cpu class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                  <span>Git Commit</span>
                </dt>
                <dd class="font-mono text-[var(--text-primary)] truncate" :title="currentVersion?.gitCommit || ''">
                  {{ currentVersion?.gitCommit || '-' }}
                </dd>
              </div>
              <div class="space-y-1">
                <dt class="text-[var(--text-tertiary)] flex items-center gap-1.5">
                  <Calendar class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                  <span>构建时间</span>
                </dt>
                <dd class="text-[var(--text-primary)]">
                  {{ formatDate(currentVersion?.buildTime) }}
                </dd>
              </div>
              <div class="space-y-1">
                <dt class="text-[var(--text-tertiary)] flex items-center gap-1.5">
                  <Clock class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                  <span>部署时间</span>
                </dt>
                <dd class="text-[var(--text-primary)]">
                  {{ formatDate(currentVersion?.deployedAt) }}
                </dd>
              </div>
            </dl>

            <div class="border-t border-[var(--border-color)] pt-4 space-y-2">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">当前版本更新内容</h3>
              <ul v-if="currentVersion?.changelog?.length" class="space-y-1.5 pl-1 text-xs text-[var(--text-secondary)]">
                <li v-for="item in currentVersion.changelog" :key="item" class="flex items-start gap-2">
                  <span class="w-1.5 h-1.5 rounded-full bg-[var(--accent)] mt-1.5 shrink-0"></span>
                  <span class="leading-relaxed">{{ item }}</span>
                </li>
              </ul>
              <p v-else class="text-xs text-[var(--text-tertiary)]">暂无版本说明。</p>
            </div>
          </div>
        </Card>

        <!-- Latest Available Version Card -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div class="flex items-center gap-2">
                <ArrowUpCircle class="w-4 h-4 text-[var(--accent)]" />
                <h2 class="text-sm font-semibold uppercase tracking-wider text-[var(--text-secondary)]">最新可用版本</h2>
              </div>
              <Badge
                :variant="updateCheck ? (updateCheck.updateAvailable ? 'info' : 'success') : 'default'"
                :dot="updateCheck?.updateAvailable"
                :pulse="updateCheck?.updateAvailable"
                size="sm"
              >
                {{ updateCheck ? (updateCheck.updateAvailable ? '可更新' : '已最新') : '未检查' }}
              </Badge>
            </div>
          </template>

          <div class="space-y-6">
            <div>
              <div class="font-mono text-3xl sm:text-4xl font-bold tracking-tight text-[var(--text-primary)] tabular-nums">
                {{ displayLatestUpdate?.version || '-' }}
              </div>
              <p class="text-xs text-[var(--text-tertiary)] mt-1">官方远程仓库 / OTA 发布的最新版本</p>
            </div>

            <!-- Repository Unavailable Notice -->
            <div
              v-if="repositoryUnavailable"
              class="flex items-start gap-3 rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-4 text-xs text-[var(--warning)]"
            >
              <AlertTriangle class="w-4 h-4 shrink-0 mt-0.5" />
              <div class="leading-relaxed font-medium">
                {{ updateCheck?.repositoryError || '当前部署目录不是 Git 工作区，无法在线更新。' }}
              </div>
            </div>

            <dl class="grid grid-cols-1 sm:grid-cols-2 gap-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-xs">
              <div class="space-y-1">
                <dt class="text-[var(--text-tertiary)] flex items-center gap-1.5">
                  <Cpu class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                  <span>Git Commit</span>
                </dt>
                <dd class="font-mono text-[var(--text-primary)] truncate" :title="displayLatestUpdate?.commit || ''">
                  {{ displayLatestUpdate?.commit || '-' }}
                </dd>
              </div>
              <div class="space-y-1">
                <dt class="text-[var(--text-tertiary)] flex items-center gap-1.5">
                  <Calendar class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                  <span>发布时间</span>
                </dt>
                <dd class="text-[var(--text-primary)]">
                  {{ formatDate(displayLatestUpdate?.date) }}
                </dd>
              </div>
            </dl>

            <div class="space-y-2">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">最新版本更新内容</h3>
              <ul v-if="displayLatestUpdate?.changelog?.length" class="space-y-1.5 pl-1 text-xs text-[var(--text-secondary)]">
                <li v-for="item in displayLatestUpdate.changelog" :key="item" class="flex items-start gap-2">
                  <span class="w-1.5 h-1.5 rounded-full bg-[var(--accent)] mt-1.5 shrink-0"></span>
                  <span class="leading-relaxed">{{ item }}</span>
                </li>
              </ul>
              <p v-else class="text-xs text-[var(--text-tertiary)]">检查更新后显示 release tag 说明。</p>
            </div>

            <!-- OTA Manifest & Artifacts -->
            <div class="border-t border-[var(--border-color)] pt-4 space-y-3">
              <div class="flex items-center justify-between gap-3 flex-wrap">
                <div class="flex items-center gap-2">
                  <Package class="w-4 h-4 text-[var(--text-secondary)]" />
                  <h3 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">OTA 发行包</h3>
                </div>
                <Badge
                  :variant="displayLatestUpdate?.ota?.manifestAvailable ? 'success' : 'warning'"
                  size="sm"
                >
                  {{ displayLatestUpdate?.ota?.manifestAvailable ? '已校验 manifest' : 'Git 更新兼容模式' }}
                </Badge>
              </div>

              <div v-if="latestOtaArtifacts.length" class="space-y-2">
                <div
                  v-for="artifact in latestOtaArtifacts"
                  :key="artifact.name"
                  class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs"
                >
                  <div class="flex items-center justify-between gap-2">
                    <span class="font-mono font-medium text-[var(--text-primary)] truncate">{{ artifact.name }}</span>
                    <Badge variant="default" size="sm" class="font-mono shrink-0">
                      {{ artifact.platform }}/{{ artifact.arch }}
                    </Badge>
                  </div>
                  <div class="mt-2 flex items-center justify-between text-[var(--text-secondary)] font-mono text-[11px] gap-2">
                    <span class="tabular-nums">{{ formatBytes(artifact.size) }}</span>
                    <span class="truncate text-[var(--text-tertiary)]" :title="artifact.sha256">{{ shortSha(artifact.sha256) }}</span>
                  </div>
                </div>
              </div>
              <p v-else class="text-xs text-[var(--text-tertiary)] leading-relaxed">
                {{ displayLatestUpdate?.ota?.error || '检查更新后显示 OTA manifest 和包校验信息。' }}
              </p>
            </div>

            <!-- Upgrade Trigger Button -->
            <Button
              variant="primary"
              size="lg"
              block
              :disabled="updateActionDisabled"
              :loading="starting"
              @click="latestUpdate && startUpdate(latestUpdate.version)"
            >
              <template #icon>
                <Sparkles class="w-4 h-4" />
              </template>
              {{ updateActionLabel }}
            </Button>
          </div>
        </Card>
      </section>

      <!-- Available Versions Section -->
      <section v-if="updateCheck?.updates?.length">
        <Card padding="none">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div class="flex items-center gap-2.5">
                <History class="w-4 h-4 text-[var(--accent)]" />
                <div>
                  <h2 class="text-base font-semibold text-[var(--text-primary)]">可更新版本列表</h2>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">发现可升级版本，支持按需直接跨步升级</p>
                </div>
              </div>
              <Badge variant="info" size="sm">
                {{ updateCheck.updates.length }} 个版本
              </Badge>
            </div>
          </template>

          <div class="divide-y divide-[var(--border-color)]">
            <div
              v-for="item in updateCheck.updates"
              :key="item.version"
              class="flex flex-col gap-4 p-5 md:flex-row md:items-start md:justify-between hover:bg-[var(--bg-surface-soft)] transition-colors"
            >
              <div class="min-w-0 flex-1 space-y-2">
                <div class="flex items-center gap-2.5 flex-wrap">
                  <span class="font-mono text-base font-bold tabular-nums text-[var(--text-primary)]">{{ item.version }}</span>
                  <Badge
                    :variant="item.ota.manifestAvailable ? 'success' : 'warning'"
                    size="sm"
                  >
                    {{ item.ota.manifestAvailable ? `OTA manifest · ${item.ota.artifacts.length} 个包` : (item.ota.error || '未发现 OTA manifest') }}
                  </Badge>
                </div>
                <div class="flex items-center gap-2 text-xs text-[var(--text-secondary)] flex-wrap">
                  <span>{{ formatDate(item.date) }}</span>
                  <span>·</span>
                  <span class="font-mono">{{ item.commit || '-' }}</span>
                </div>
                <ul v-if="item.changelog.length" class="space-y-1 pl-1 text-xs text-[var(--text-secondary)] pt-1">
                  <li v-for="line in item.changelog.slice(0, 6)" :key="line" class="flex items-start gap-2">
                    <span class="w-1.5 h-1.5 rounded-full bg-[var(--text-tertiary)] mt-1.5 shrink-0"></span>
                    <span class="leading-relaxed">{{ line }}</span>
                  </li>
                </ul>
              </div>
              <div class="shrink-0 self-start md:self-center">
                <Button
                  variant="secondary"
                  size="sm"
                  :disabled="starting || hasRunningTask"
                  @click="startUpdate(item.version)"
                >
                  更新到此版本
                </Button>
              </div>
            </div>
          </div>
        </Card>
      </section>

      <!-- Tasks and Logs Section -->
      <section class="grid gap-6 xl:grid-cols-[minmax(320px,420px)_1fr]">
        <!-- Tasks Column -->
        <Card padding="none" class="flex flex-col h-full">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div>
                <h2 class="text-base font-semibold text-[var(--text-primary)]">更新任务</h2>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">每页最多显示 7 条，旧任务翻页查看</p>
              </div>
              <Badge variant="default" size="sm">
                {{ tasks.length }} 条
              </Badge>
            </div>
          </template>

          <div v-if="tasks.length === 0" class="p-6">
            <EmptyState
              title="暂无更新任务"
              description="当前系统尚未发起过任何更新或回滚任务。"
            />
          </div>

          <div v-else class="divide-y divide-[var(--border-color)] flex-1 overflow-y-auto">
            <button
              v-for="task in paginatedTasks"
              :key="task.id"
              type="button"
              class="group relative block w-full text-left p-4 transition-all duration-200 active:scale-[0.99] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
              :class="selectedTaskId === task.id
                ? 'bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))] text-[var(--text-primary)]'
                : 'hover:bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
              @click="selectTask(task)"
            >
              <span
                v-if="selectedTaskId === task.id"
                class="absolute left-0 top-2 bottom-2 w-1 rounded-r-full bg-[var(--accent)]"
              />
              <div class="flex items-center justify-between gap-2 min-w-0">
                <span class="truncate font-mono text-sm font-semibold text-[var(--text-primary)]">
                  #{{ task.id }} {{ task.fromVersion || '-' }} &rarr; {{ task.targetVersion }}
                </span>
                <Badge
                  :variant="statusBadgeVariant(task.status)"
                  :dot="task.status === 'running'"
                  :pulse="task.status === 'running'"
                  size="sm"
                  class="shrink-0"
                >
                  {{ statusLabel(task.status) }}
                </Badge>
              </div>
              <div class="mt-1.5 flex items-center justify-between text-xs text-[var(--text-tertiary)] gap-2">
                <span class="truncate">{{ task.startedByUsername || '-' }}</span>
                <span class="shrink-0 tabular-nums">{{ formatDate(task.createdAt) }}</span>
              </div>
              <p v-if="task.errorMessage" class="mt-1.5 truncate text-xs text-[var(--error)]">
                {{ task.errorMessage }}
              </p>
            </button>
          </div>

          <template v-if="tasks.length > 0" #footer>
            <div class="flex items-center justify-between gap-3 text-xs text-[var(--text-secondary)]">
              <span>第 {{ taskPage }} / {{ totalTaskPages }} 页 · 共 {{ tasks.length }} 个任务</span>
              <div class="flex items-center gap-1.5">
                <Button
                  variant="secondary"
                  size="sm"
                  :disabled="taskPage <= 1"
                  @click="setTaskPage(taskPage - 1)"
                >
                  上一页
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  :disabled="taskPage >= totalTaskPages"
                  @click="setTaskPage(taskPage + 1)"
                >
                  下一页
                </Button>
              </div>
            </div>
          </template>
        </Card>

        <!-- Task Logs Column -->
        <Card padding="none" class="flex flex-col h-full">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h2 class="text-base font-semibold text-[var(--text-primary)]">任务日志</h2>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5 truncate">
                  {{ selectedTask ? `任务 #${selectedTask.id} · ${statusLabel(selectedTask.status)} · ${formatDate(selectedTask.createdAt)}` : '请在左侧列表中选择任务' }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 flex-wrap">
                <Button
                  variant="secondary"
                  size="sm"
                  :loading="logsLoading"
                  :disabled="!selectedTask || logsLoading"
                  @click="loadSelectedTaskLogs"
                >
                  <template #icon>
                    <RefreshCw class="w-3.5 h-3.5" />
                  </template>
                  {{ logsLoading ? '加载中...' : '刷新日志' }}
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  :loading="rollingBackTaskId === selectedTask?.id"
                  :disabled="!selectedTask?.backupPath || hasRunningTask || rollingBackTaskId === selectedTask?.id"
                  @click="selectedTask && rollbackTask(selectedTask)"
                >
                  <template #icon>
                    <RotateCcw class="w-3.5 h-3.5" />
                  </template>
                  {{ rollingBackTaskId === selectedTask?.id ? '回滚中...' : '回滚' }}
                </Button>
              </div>
            </div>
          </template>

          <div class="p-4 md:p-6 flex-1 flex flex-col">
            <div v-if="!selectedTask" class="py-12 flex-1 flex items-center justify-center">
              <EmptyState
                title="请选择任务"
                description="在左侧任务列表中点击选中更新任务以查看对应的详细输出日志。"
              />
            </div>
            <div v-else class="rounded-2xl border border-[var(--border-color)] bg-[var(--terminal-bg)] overflow-hidden shadow-sm flex-1 flex flex-col">
              <!-- macOS Terminal Header Bar -->
              <div class="flex items-center justify-between px-4 py-2.5 bg-[var(--bg-surface-soft)]/90 border-b border-[var(--border-color)] backdrop-blur-sm shrink-0">
                <div class="flex items-center gap-2">
                  <span class="w-3 h-3 rounded-full bg-[var(--error)] opacity-80 inline-block shadow-sm"></span>
                  <span class="w-3 h-3 rounded-full bg-[var(--warning)] opacity-80 inline-block shadow-sm"></span>
                  <span class="w-3 h-3 rounded-full bg-[var(--success)] opacity-80 inline-block shadow-sm"></span>
                  <div class="flex items-center gap-1.5 ml-2 text-xs font-mono text-[var(--text-secondary)] font-medium">
                    <Terminal class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                    <span>update.log</span>
                  </div>
                </div>
                <div class="flex items-center gap-2">
                  <Badge :variant="statusBadgeVariant(selectedTask.status)" size="sm">
                    {{ statusLabel(selectedTask.status) }}
                  </Badge>
                  <span class="text-xs text-[var(--text-tertiary)] font-mono">
                    #{{ selectedTask.id }}
                  </span>
                </div>
              </div>
              <!-- Log Output Container -->
              <pre class="min-h-[420px] max-h-[640px] flex-1 overflow-auto p-4 md:p-5 font-mono text-xs leading-relaxed text-[var(--text-primary)] whitespace-pre-wrap select-text selection:bg-[var(--accent)] selection:text-white">{{ selectedTaskLogs || (logsLoading ? '日志读取中...' : '暂无日志输出。') }}</pre>
            </div>
          </div>
        </Card>
      </section>
    </template>
  </div>
</template>
