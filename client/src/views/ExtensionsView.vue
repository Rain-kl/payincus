<script setup lang="ts">
/**
 * ExtensionsView - 扩展与自定义初始化命令管理中心
 * 
 * 遵循 Apple HIG 设计风格与 THEME.md 规范：
 * - 深度复用 @/components/ui (PageHeader, Card, Button, Badge, EmptyState, Input, Select, Pagination)
 * - 语义色彩 Token (杜绝硬编码 hex 色值)
 * - 胶囊药丸按钮、微阴影发丝边框、18px/rounded-2xl 容器卡片、微交互
 * - 响应式插件卡片网格布局，杜绝横向滚动溢出
 */
import { ref, computed, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Terminal,
  Plus,
  RefreshCw,
  Search,
  RotateCcw,
  Eye,
  Edit3,
  Trash2,
  Copy,
  Layers,
  FileCode,
  Check,
  Power
} from 'lucide-vue-next'
import api from '@/api'
import { useToast } from '@/stores/toast'
import InitCommandModal from '@/components/extensions/InitCommandModal.vue'
import InitCommandDetailModal from '@/components/extensions/InitCommandDetailModal.vue'
import DistroIcon from '@/components/icons/DistroIcon.vue'
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

defineOptions({ name: 'ExtensionsView' })

const { t, locale } = useI18n()
const toast = useToast()

// 命令列表接口
interface InitCommand {
  id: number
  name: string
  commandPreview: string
  commandLineCount: number
  distros: string[]
  description: string | null
  enabled: boolean
  createdAt: string
  updatedAt: string
}

const commands = ref<InitCommand[]>([])
const loading = ref(false)

// 筛选与搜索
const searchQuery = ref('')
const selectedStatus = ref('')
const selectedDistro = ref('')

// 分页
const currentPage = ref(1)
const pageSize = ref(12)

// 发行版图标映射
const distroIcons: Record<string, string> = {
  ubuntu: 'ubuntu',
  debian: 'debian',
  rhel: 'almalinux',
  alpine: 'alpine',
  arch: 'arch',
  suse: 'opensuse',
  all: 'linux'
}

// 发行版选项
const distroOptions = computed(() => [
  { value: '', label: `${t('common.all')} ${t('extensions.initCommands.distros')}` },
  { value: 'all', label: t('extensions.initCommands.distroNames.all') },
  { value: 'ubuntu', label: 'Ubuntu' },
  { value: 'debian', label: 'Debian' },
  { value: 'rhel', label: 'RHEL / CentOS' },
  { value: 'alpine', label: 'Alpine' },
  { value: 'arch', label: 'Arch Linux' },
  { value: 'suse', label: 'openSUSE' }
])

// 状态选项
const statusOptions = computed(() => [
  { value: '', label: `${t('common.all')} ${t('common.status')}` },
  { value: 'enabled', label: t('extensions.initCommands.statusEnabled') },
  { value: 'disabled', label: t('extensions.initCommands.statusDisabled') }
])

// 统计数据
const totalCount = computed(() => commands.value.length)
const enabledCount = computed(() => commands.value.filter(c => c.enabled).length)
const distroCount = computed(() => {
  const set = new Set<string>()
  for (const c of commands.value) {
    for (const d of c.distros || []) {
      set.add(d)
    }
  }
  return set.size
})

// 筛选结果
const filteredCommands = computed(() => {
  return commands.value.filter(cmd => {
    if (searchQuery.value.trim()) {
      const q = searchQuery.value.trim().toLowerCase()
      const matchName = cmd.name?.toLowerCase().includes(q)
      const matchDesc = cmd.description?.toLowerCase().includes(q)
      const matchPreview = cmd.commandPreview?.toLowerCase().includes(q)
      if (!matchName && !matchDesc && !matchPreview) {
        return false
      }
    }
    if (selectedStatus.value === 'enabled' && !cmd.enabled) return false
    if (selectedStatus.value === 'disabled' && cmd.enabled) return false
    if (selectedDistro.value) {
      if (!cmd.distros || !cmd.distros.includes(selectedDistro.value)) {
        return false
      }
    }
    return true
  })
})

const paginatedCommands = computed(() => {
  const start = (currentPage.value - 1) * pageSize.value
  return filteredCommands.value.slice(start, start + pageSize.value)
})

watch([searchQuery, selectedStatus, selectedDistro], () => {
  currentPage.value = 1
})

function resetFilters(): void {
  searchQuery.value = ''
  selectedStatus.value = ''
  selectedDistro.value = ''
  currentPage.value = 1
}

function onPaginationChange(payload: { page: number; pageSize: number }): void {
  currentPage.value = payload.page
  pageSize.value = payload.pageSize
}

// 加载命令列表
async function loadCommands(): Promise<void> {
  loading.value = true
  try {
    const response = await api.initCommands.list()
    commands.value = response.commands
  } catch (error: any) {
    toast.error(t('extensions.initCommands.loadFailed') + ': ' + (error?.message || String(error)))
  } finally {
    loading.value = false
  }
}

// 新增/编辑弹窗
const showCommandModal = ref(false)
const editingCommand = ref<InitCommand | null>(null)

function openAddModal(): void {
  editingCommand.value = null
  showCommandModal.value = true
}

function openEditModal(cmd: InitCommand): void {
  editingCommand.value = cmd
  showCommandModal.value = true
}

function onModalClose(): void {
  showCommandModal.value = false
  editingCommand.value = null
}

async function onModalSave(): Promise<void> {
  await loadCommands()
  onModalClose()
}

// 详情弹窗
const showDetailModal = ref(false)
const viewingCommand = ref<InitCommand | null>(null)

function openDetailModal(cmd: InitCommand): void {
  viewingCommand.value = cmd
  showDetailModal.value = true
}

function closeDetailModal(): void {
  showDetailModal.value = false
  viewingCommand.value = null
}

// 删除命令
async function deleteCommand(cmd: InitCommand): Promise<void> {
  if (!confirm(t('extensions.initCommands.confirmDelete', { name: cmd.name }))) return
  
  try {
    await api.initCommands.delete(cmd.id)
    toast.success(t('extensions.initCommands.deleteSuccess'))
    await loadCommands()
    if (paginatedCommands.value.length === 0 && currentPage.value > 1) {
      currentPage.value--
    }
  } catch (error: any) {
    toast.error(t('extensions.initCommands.deleteFailed') + ': ' + (error?.message || String(error)))
  }
}

// 切换启用状态
async function toggleEnabled(cmd: InitCommand): Promise<void> {
  const nextState = !cmd.enabled
  try {
    await api.initCommands.update(cmd.id, { enabled: nextState })
    cmd.enabled = nextState
    toast.success(cmd.enabled ? t('extensions.initCommands.enabled') : t('extensions.initCommands.disabled'))
  } catch (error: any) {
    toast.error(t('extensions.initCommands.toggleFailed') + ': ' + (error?.message || String(error)))
  }
}

// 复制预览命令
async function copyCommandPreview(cmd: InitCommand): Promise<void> {
  const content = cmd.commandPreview || ''
  if (!content) return
  try {
    await navigator.clipboard.writeText(content)
    toast.success(t('common.copied'))
  } catch {
    toast.error(t('common.copyFailed'))
  }
}

// 获取发行版显示名称
function getDistroName(distro: string): string {
  const key = `extensions.initCommands.distroNames.${distro}`
  const translated = t(key)
  return translated !== key ? translated : distro
}

function formatDate(dateStr?: string | null): string {
  if (!dateStr) return ''
  try {
    return new Date(dateStr).toLocaleDateString(locale.value, {
      year: 'numeric',
      month: 'short',
      day: 'numeric'
    })
  } catch {
    return dateStr
  }
}

onMounted(() => {
  loadCommands()
})
</script>

<template>
  <div class="kawaii-page space-y-6 md:space-y-8 animate-fade-in max-w-7xl mx-auto pb-12">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('nav.extensions')"
      :description="t('extensions.initCommands.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadCommands"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5 stroke-[2]" />
          </template>
          {{ loading ? t('common.loading') : t('common.refresh') }}
        </Button>
        <Button
          variant="primary"
          size="sm"
          @click="openAddModal"
        >
          <template #icon>
            <Plus class="w-3.5 h-3.5 stroke-[2]" />
          </template>
          {{ t('extensions.initCommands.add') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Quick Stats Overview -->
    <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
      <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 md:p-5 shadow-[0_2px_8px_rgba(0,0,0,0.04)] flex items-center gap-4 transition-all duration-300 hover:border-[var(--border-hover)]">
        <div class="w-11 h-11 rounded-2xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] flex items-center justify-center shrink-0">
          <FileCode class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <p class="text-xs font-medium text-[var(--text-secondary)]">{{ t('extensions.initCommands.title') }}</p>
          <p class="text-xl md:text-2xl font-semibold tracking-tight text-[var(--text-primary)] mt-0.5 tabular-nums">{{ totalCount }}</p>
        </div>
      </div>

      <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 md:p-5 shadow-[0_2px_8px_rgba(0,0,0,0.04)] flex items-center gap-4 transition-all duration-300 hover:border-[var(--border-hover)]">
        <div class="w-11 h-11 rounded-2xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] flex items-center justify-center shrink-0">
          <Check class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <p class="text-xs font-medium text-[var(--text-secondary)]">{{ t('extensions.initCommands.statusEnabled') }}</p>
          <p class="text-xl md:text-2xl font-semibold tracking-tight text-[var(--text-primary)] mt-0.5 tabular-nums">{{ enabledCount }}</p>
        </div>
      </div>

      <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 md:p-5 shadow-[0_2px_8px_rgba(0,0,0,0.04)] flex items-center gap-4 transition-all duration-300 hover:border-[var(--border-hover)]">
        <div class="w-11 h-11 rounded-2xl bg-[color-mix(in_srgb,var(--chart-1)_12%,var(--bg-surface))] text-[var(--chart-1)] border border-[color-mix(in_srgb,var(--chart-1)_24%,var(--bg-surface))] flex items-center justify-center shrink-0">
          <Layers class="w-5 h-5 stroke-[2]" />
        </div>
        <div class="min-w-0">
          <p class="text-xs font-medium text-[var(--text-secondary)]">{{ t('extensions.initCommands.distros') }}</p>
          <p class="text-xl md:text-2xl font-semibold tracking-tight text-[var(--text-primary)] mt-0.5 tabular-nums">{{ distroCount }}</p>
        </div>
      </div>
    </div>

    <!-- Filter & Search Toolbar -->
    <Card padding="sm">
      <div class="flex flex-col md:flex-row md:items-center justify-between gap-3">
        <div class="flex-1 min-w-0">
          <Input
            v-model="searchQuery"
            :placeholder="t('common.search') + '...'"
            size="sm"
            clearable
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>

        <div class="flex items-center gap-2.5 shrink-0 flex-wrap sm:flex-nowrap">
          <div class="w-full sm:w-44">
            <Select
              v-model="selectedStatus"
              :options="statusOptions"
              size="sm"
            />
          </div>

          <div class="w-full sm:w-52">
            <Select
              v-model="selectedDistro"
              :options="distroOptions"
              size="sm"
            />
          </div>

          <Button
            v-if="searchQuery || selectedStatus || selectedDistro"
            variant="ghost"
            size="sm"
            class="shrink-0"
            @click="resetFilters"
          >
            <template #icon>
              <RotateCcw class="w-3.5 h-3.5 stroke-[2]" />
            </template>
            {{ t('common.reset') }}
          </Button>
        </div>
      </div>
    </Card>

    <!-- Loading Skeleton Grid -->
    <div v-if="loading && commands.length === 0" class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
      <div
        v-for="i in 6"
        :key="i"
        class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 space-y-4 animate-pulse shadow-[0_2px_8px_rgba(0,0,0,0.04)]"
      >
        <div class="flex items-center gap-3">
          <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)]"></div>
          <div class="flex-1 space-y-2">
            <div class="h-4 w-3/4 rounded bg-[var(--bg-surface-soft)]"></div>
            <div class="h-3 w-1/2 rounded bg-[var(--bg-surface-soft)]"></div>
          </div>
        </div>
        <div class="h-20 rounded-xl bg-[var(--bg-surface-soft)]"></div>
        <div class="flex gap-2">
          <div class="h-5 w-16 rounded-full bg-[var(--bg-surface-soft)]"></div>
          <div class="h-5 w-16 rounded-full bg-[var(--bg-surface-soft)]"></div>
        </div>
      </div>
    </div>

    <!-- Empty State: No Commands at all -->
    <Card v-else-if="!loading && commands.length === 0" padding="lg">
      <EmptyState
        :title="t('extensions.initCommands.empty')"
        :description="t('extensions.initCommands.emptyHint')"
      >
        <template #icon>
          <Terminal class="w-7 h-7 stroke-[1.5]" />
        </template>
        <template #action>
          <Button variant="primary" size="md" @click="openAddModal">
            <template #icon>
              <Plus class="w-4 h-4 stroke-[2]" />
            </template>
            {{ t('extensions.initCommands.addFirst') }}
          </Button>
        </template>
      </EmptyState>
    </Card>

    <!-- Empty State: Filter Returned Zero Results -->
    <Card v-else-if="!loading && filteredCommands.length === 0" padding="lg">
      <EmptyState
        :title="t('common.noSearchResults')"
        :description="t('common.noData')"
      >
        <template #icon>
          <Search class="w-7 h-7 stroke-[1.5]" />
        </template>
        <template #action>
          <Button variant="secondary" size="sm" @click="resetFilters">
            <template #icon>
              <RotateCcw class="w-3.5 h-3.5 stroke-[2]" />
            </template>
            {{ t('common.reset') }}
          </Button>
        </template>
      </EmptyState>
    </Card>

    <!-- Commands Card Grid -->
    <div v-else class="space-y-6">
      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
        <div
          v-for="cmd in paginatedCommands"
          :key="cmd.id"
          class="group rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:shadow-[0_8px_24px_rgba(0,0,0,0.08)] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] p-5 md:p-6 flex flex-col justify-between"
          :class="{ 'opacity-65': !cmd.enabled }"
        >
          <!-- Top Info -->
          <div>
            <div class="flex items-start justify-between gap-3 mb-3">
              <div class="flex items-center gap-3 min-w-0 flex-1">
                <div class="w-10 h-10 rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] flex items-center justify-center shrink-0 group-hover:scale-105 transition-transform duration-300">
                  <Terminal class="w-5 h-5 stroke-[2]" />
                </div>
                <div class="min-w-0 flex-1">
                  <h3 class="font-semibold tracking-tight text-base text-[var(--text-primary)] truncate" :title="cmd.name">
                    {{ cmd.name }}
                  </h3>
                  <p v-if="cmd.description" class="text-xs text-[var(--text-secondary)] truncate mt-0.5" :title="cmd.description">
                    {{ cmd.description }}
                  </p>
                  <p v-else class="text-xs text-[var(--text-tertiary)] mt-0.5">
                    {{ formatDate(cmd.updatedAt || cmd.createdAt) }}
                  </p>
                </div>
              </div>

              <Badge
                :variant="cmd.enabled ? 'success' : 'default'"
                size="sm"
                dot
                class="shrink-0"
              >
                {{ cmd.enabled ? t('extensions.initCommands.statusEnabled') : t('extensions.initCommands.statusDisabled') }}
              </Badge>
            </div>

            <!-- Terminal Preview -->
            <div class="mt-3.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs font-mono relative overflow-hidden group/terminal">
              <div class="flex items-center justify-between text-[10px] text-[var(--text-tertiary)] mb-1.5 pb-1 border-b border-[var(--border-color)]/50 select-none">
                <div class="flex items-center gap-1.5">
                  <span class="w-2 h-2 rounded-full bg-[var(--error)]/70"></span>
                  <span class="w-2 h-2 rounded-full bg-[var(--warning)]/70"></span>
                  <span class="w-2 h-2 rounded-full bg-[var(--success)]/70"></span>
                  <span class="ml-1 font-mono uppercase">sh</span>
                </div>
                <div class="flex items-center gap-2">
                  <span>{{ t('extensions.initCommands.lineCount', { count: cmd.commandLineCount }) }}</span>
                  <button
                    v-if="cmd.commandPreview"
                    type="button"
                    class="opacity-0 group-hover/terminal:opacity-100 hover:text-[var(--text-primary)] transition-opacity"
                    :title="t('common.copy')"
                    @click.stop="copyCommandPreview(cmd)"
                  >
                    <Copy class="w-3 h-3" />
                  </button>
                </div>
              </div>
              <div class="font-mono text-[var(--text-secondary)] whitespace-pre-wrap break-all line-clamp-3 select-all leading-relaxed">
                {{ cmd.commandPreview || t('extensions.initCommands.noContent') }}
              </div>
            </div>

            <!-- Distros Badges -->
            <div class="mt-4 flex flex-wrap items-center gap-1.5">
              <span
                v-for="distro in cmd.distros"
                :key="distro"
                class="inline-flex items-center gap-1.5 text-xs px-2.5 py-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-secondary)]"
              >
                <DistroIcon :distro="distroIcons[distro] || 'linux'" :size="13" />
                <span>{{ getDistroName(distro) }}</span>
              </span>
            </div>
          </div>

          <!-- Bottom Footer Actions -->
          <div class="mt-5 pt-3.5 border-t border-[var(--border-color)] flex items-center justify-between gap-2">
            <!-- Toggle status capsule button -->
            <button
              type="button"
              class="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-full text-xs font-medium transition-all duration-200 active:scale-95"
              :class="cmd.enabled
                ? 'bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_28%,var(--bg-surface))] hover:brightness-105'
                : 'bg-[var(--bg-surface-soft)] text-[var(--text-tertiary)] border border-[var(--border-color)] hover:text-[var(--text-secondary)]'"
              :title="cmd.enabled ? t('extensions.initCommands.clickToDisable') : t('extensions.initCommands.clickToEnable')"
              @click="toggleEnabled(cmd)"
            >
              <Power class="w-3.5 h-3.5 stroke-[2]" :class="cmd.enabled ? 'text-[var(--success)]' : 'text-[var(--text-tertiary)]'" />
              <span>{{ cmd.enabled ? t('extensions.initCommands.statusEnabled') : t('extensions.initCommands.statusDisabled') }}</span>
            </button>

            <!-- Action buttons -->
            <div class="flex items-center gap-1">
              <button
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-90"
                :title="t('extensions.initCommands.viewDetail')"
                @click="openDetailModal(cmd)"
              >
                <Eye class="w-4 h-4 stroke-[1.8]" />
              </button>

              <button
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-90"
                :title="t('common.edit')"
                @click="openEditModal(cmd)"
              >
                <Edit3 class="w-4 h-4 stroke-[1.8]" />
              </button>

              <button
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_12%,transparent)] transition-all duration-200 active:scale-90"
                :title="t('common.delete')"
                @click="deleteCommand(cmd)"
              >
                <Trash2 class="w-4 h-4 stroke-[1.8]" />
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- Pagination component -->
      <Pagination
        :page="currentPage"
        :page-size="pageSize"
        :total="filteredCommands.length"
        :show-size-changer="true"
        :page-size-options="[6, 12, 24, 48]"
        @change="onPaginationChange"
      >
        <template #total="{ total, page, totalPages }">
          {{ $t('common.pageInfo', { current: page, total: totalPages, count: total }) }}
        </template>
      </Pagination>
    </div>

    <!-- Modals -->
    <InitCommandModal
      v-if="showCommandModal"
      :command="editingCommand"
      @close="onModalClose"
      @save="onModalSave"
    />

    <InitCommandDetailModal
      v-if="showDetailModal && viewingCommand"
      :command="viewingCommand"
      @close="closeDetailModal"
    />
  </div>
</template>
