<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import DistroIcon from '@/components/icons/DistroIcon.vue'
import DrawerModal from '@/components/DrawerModal.vue'
import type { SystemImage, CreateSystemImageRequest, UpdateSystemImageRequest } from '@/types/api'
import {
  PageHeader,
  Card,
  Button,
  Input,
  Select,
  Badge,
  EmptyState,
  Pagination
} from '@/components/ui'
import {
  Plus,
  RefreshCw,
  Search,
  Layers,
  Eye,
  EyeOff,
  Edit3,
  Trash2,
  X,
  Loader2
} from 'lucide-vue-next'

const { t } = useI18n()
const toast = useToast()

// 状态
const loading = ref(true)
const saving = ref(false)
const images = ref<SystemImage[]>([])
const showModal = ref(false)
const editingImage = ref<SystemImage | null>(null)
const activeArchitecture = ref<'all' | 'x86_64' | 'aarch64'>('all')

// 过滤与搜索
const searchQuery = ref('')
const selectedOs = ref('all')
const selectedInstanceType = ref('all')

// 分页
const page = ref(1)
const pageSize = ref(15)

// 表单
interface ImageForm {
  name: string
  remoteAlias: string
  osType: string
  architecture: 'x86_64' | 'aarch64'
  instanceType: 'container' | 'vm' | 'both'
  icon: string
  sortOrder: number
  hidden: boolean
}

const form = ref<ImageForm>(getDefaultForm())

function getDefaultForm(): ImageForm {
  return {
    name: '',
    remoteAlias: '',
    osType: 'Linux',
    architecture: 'x86_64',
    instanceType: 'both',
    icon: '',
    sortOrder: 0,
    hidden: false
  }
}

// 常用图标列表
const iconOptions = [
  'almalinux', 'alpine', 'archlinux', 'centos', 'debian',
  'fedora', 'kali', 'opensuse', 'oracle', 'rockylinux', 'ubuntu'
]

const iconSelectOptions = computed(() =>
  iconOptions.map((icon) => ({
    value: icon,
    label: icon.charAt(0).toUpperCase() + icon.slice(1)
  }))
)

const architectureOptions: Array<{ value: 'x86_64' | 'aarch64'; label: string }> = [
  { value: 'x86_64', label: 'x86_64' },
  { value: 'aarch64', label: 'aarch64' }
]

const architectureTabs: Array<{ value: 'all' | 'x86_64' | 'aarch64'; labelKey?: string; label?: string }> = [
  { value: 'all', labelKey: 'common.all' },
  { value: 'x86_64', label: 'x86_64' },
  { value: 'aarch64', label: 'aarch64' }
]

const instanceTypeOptions = computed(() => [
  { value: 'both', label: t('admin.images.typeBoth') },
  { value: 'container', label: t('admin.images.typeContainer') },
  { value: 'vm', label: t('admin.images.typeVm') }
])

const availableOsList = computed(() => {
  const osSet = new Set<string>()
  for (const img of images.value) {
    if (img.osType) osSet.add(img.osType)
    if (img.icon) osSet.add(img.icon)
  }
  return Array.from(osSet).sort()
})

const osFilterOptions = computed(() => [
  { value: 'all', label: t('common.all') },
  ...availableOsList.value.map((os) => ({
    value: os,
    label: os.charAt(0).toUpperCase() + os.slice(1)
  }))
])

const instanceTypeFilterOptions = computed(() => [
  { value: 'all', label: t('common.all') },
  { value: 'both', label: t('admin.images.typeBoth') },
  { value: 'container', label: t('admin.images.typeContainer') },
  { value: 'vm', label: t('admin.images.typeVm') }
])

// 计算属性
const isEditMode = computed(() => editingImage.value !== null)
const modalTitle = computed(() =>
  isEditMode.value ? t('admin.images.edit') : t('admin.images.create')
)

const hasActiveFilters = computed(() => {
  return (
    searchQuery.value.trim() !== '' ||
    selectedOs.value !== 'all' ||
    selectedInstanceType.value !== 'all' ||
    activeArchitecture.value !== 'all'
  )
})

function resetFilters() {
  searchQuery.value = ''
  selectedOs.value = 'all'
  selectedInstanceType.value = 'all'
  activeArchitecture.value = 'all'
}

const filteredImages = computed(() => {
  return images.value.filter((image) => {
    // 架构过滤
    if (activeArchitecture.value !== 'all' && image.architecture !== activeArchitecture.value) {
      return false
    }
    // 实例类型过滤
    if (selectedInstanceType.value !== 'all' && image.instanceType !== selectedInstanceType.value) {
      return false
    }
    // 系统分类过滤
    if (selectedOs.value !== 'all') {
      const target = selectedOs.value.toLowerCase()
      const matchOs = image.osType?.toLowerCase() === target || image.icon?.toLowerCase() === target
      if (!matchOs) return false
    }
    // 搜索
    if (searchQuery.value.trim()) {
      const q = searchQuery.value.trim().toLowerCase()
      const matchName = image.name.toLowerCase().includes(q)
      const matchAlias = image.remoteAlias.toLowerCase().includes(q)
      const matchOs = image.osType?.toLowerCase().includes(q)
      if (!matchName && !matchAlias && !matchOs) return false
    }
    return true
  })
})

const paginatedImages = computed(() => {
  const start = (page.value - 1) * pageSize.value
  return filteredImages.value.slice(start, start + pageSize.value)
})

watch([activeArchitecture, selectedOs, selectedInstanceType, searchQuery], () => {
  page.value = 1
})

watch(filteredImages, (newVal) => {
  const maxPage = Math.max(1, Math.ceil(newVal.length / pageSize.value))
  if (page.value > maxPage) {
    page.value = maxPage
  }
})

// 批量操作与选中状态
const selectedIds = ref<Set<number>>(new Set())
const batchOperating = ref(false)

const selectedCount = computed(() => selectedIds.value.size)
const isAllSelected = computed(
  () =>
    filteredImages.value.length > 0 &&
    filteredImages.value.every((image) => selectedIds.value.has(image.id))
)
const isIndeterminate = computed(() => {
  const count = filteredImages.value.filter((image) => selectedIds.value.has(image.id)).length
  return count > 0 && count < filteredImages.value.length
})

function toggleSelect(id: number, checked: boolean): void {
  const next = new Set(selectedIds.value)
  if (checked) {
    next.add(id)
  } else {
    next.delete(id)
  }
  selectedIds.value = next
}

function toggleSelectAll(checked: boolean): void {
  const next = new Set(selectedIds.value)
  if (checked) {
    for (const image of filteredImages.value) {
      next.add(image.id)
    }
  } else {
    for (const image of filteredImages.value) {
      next.delete(image.id)
    }
  }
  selectedIds.value = next
}

function clearSelection(): void {
  selectedIds.value = new Set()
}

function getArchitectureCount(architecture: 'all' | 'x86_64' | 'aarch64'): number {
  if (architecture === 'all') {
    return images.value.length
  }
  return images.value.filter((image) => image.architecture === architecture).length
}

// 加载镜像列表
async function loadImages(): Promise<void> {
  loading.value = true
  try {
    const response = await api.images.list()
    images.value = response.images || []
    const validIds = new Set(images.value.map((image) => image.id))
    selectedIds.value = new Set([...selectedIds.value].filter((id) => validIds.has(id)))
  } catch (err: any) {
    toast.error(err.message || t('admin.images.loadFailed'))
  } finally {
    loading.value = false
  }
}

// 打开创建弹窗
function openCreateModal(): void {
  editingImage.value = null
  const defaultForm = getDefaultForm()
  if (activeArchitecture.value !== 'all') {
    defaultForm.architecture = activeArchitecture.value
  }
  form.value = defaultForm
  showModal.value = true
}

// 打开编辑弹窗
function openEditModal(image: SystemImage): void {
  editingImage.value = image
  form.value = {
    name: image.name,
    remoteAlias: image.remoteAlias,
    osType: image.osType || 'Linux',
    architecture: image.architecture || 'x86_64',
    instanceType: image.instanceType || 'both',
    icon: image.icon,
    sortOrder: image.sortOrder || 0,
    hidden: image.hidden || false
  }
  showModal.value = true
}

// 保存镜像
async function saveImage(): Promise<void> {
  if (!form.value.name || !form.value.remoteAlias || !form.value.icon) {
    toast.error(t('admin.images.validation.requiredFields'))
    return
  }

  saving.value = true
  try {
    if (isEditMode.value && editingImage.value) {
      const data: UpdateSystemImageRequest = {
        name: form.value.name,
        remoteAlias: form.value.remoteAlias,
        osType: form.value.osType,
        architecture: form.value.architecture,
        instanceType: form.value.instanceType,
        icon: form.value.icon,
        sortOrder: Number(form.value.sortOrder) || 0,
        hidden: form.value.hidden
      }
      await api.images.update(editingImage.value.id, data)
      toast.success(t('admin.images.updateSuccess'))
    } else {
      const data: CreateSystemImageRequest = {
        name: form.value.name,
        remoteAlias: form.value.remoteAlias,
        osType: form.value.osType,
        architecture: form.value.architecture,
        instanceType: form.value.instanceType,
        icon: form.value.icon,
        sortOrder: Number(form.value.sortOrder) || 0,
        hidden: form.value.hidden
      }
      await api.images.create(data)
      toast.success(t('admin.images.createSuccess'))
    }
    showModal.value = false
    await loadImages()
  } catch (err: any) {
    toast.error(err.message || t('admin.images.saveFailed'))
  } finally {
    saving.value = false
  }
}

// 切换隐藏状态
async function toggleHidden(image: SystemImage): Promise<void> {
  try {
    await api.images.update(image.id, { hidden: !image.hidden })
    toast.success(image.hidden ? t('admin.images.shown') : t('admin.images.hidden'))
    await loadImages()
  } catch (err: any) {
    toast.error(err.message)
  }
}

// 删除镜像
async function deleteImage(image: SystemImage): Promise<void> {
  if (!confirm(t('admin.images.confirmDelete', { name: image.name }))) return

  try {
    await api.images.delete(image.id)
    toast.success(t('admin.images.deleteSuccess'))
    await loadImages()
  } catch (err: any) {
    toast.error(err.message || t('admin.images.deleteFailed'))
  }
}

// 批量禁用
async function batchDisable(): Promise<void> {
  const ids = Array.from(selectedIds.value)
  if (ids.length === 0) return
  if (!confirm(t('admin.images.confirmBatchDisable', { count: ids.length }))) return

  batchOperating.value = true
  try {
    const results = await Promise.allSettled(
      ids.map((id) => api.images.update(id, { hidden: true }))
    )
    const successCount = results.filter((r) => r.status === 'fulfilled').length
    const failCount = results.length - successCount
    if (failCount === 0) {
      toast.success(t('admin.images.batchDisableSuccess', { count: successCount }))
    } else {
      toast.warning(`${t('admin.images.batchDisableSuccess', { count: successCount })}, ${failCount} failed`)
    }
    clearSelection()
    await loadImages()
  } catch (err: any) {
    toast.error(err.message || t('admin.images.saveFailed'))
  } finally {
    batchOperating.value = false
  }
}

// 批量启用
async function batchEnable(): Promise<void> {
  const ids = Array.from(selectedIds.value)
  if (ids.length === 0) return
  if (!confirm(t('admin.images.confirmBatchEnable', { count: ids.length }))) return

  batchOperating.value = true
  try {
    const results = await Promise.allSettled(
      ids.map((id) => api.images.update(id, { hidden: false }))
    )
    const successCount = results.filter((r) => r.status === 'fulfilled').length
    const failCount = results.length - successCount
    if (failCount === 0) {
      toast.success(t('admin.images.batchEnableSuccess', { count: successCount }))
    } else {
      toast.warning(`${t('admin.images.batchEnableSuccess', { count: successCount })}, ${failCount} failed`)
    }
    clearSelection()
    await loadImages()
  } catch (err: any) {
    toast.error(err.message || t('admin.images.saveFailed'))
  } finally {
    batchOperating.value = false
  }
}

// 批量删除
async function batchDelete(): Promise<void> {
  const ids = Array.from(selectedIds.value)
  if (ids.length === 0) return
  if (!confirm(t('admin.images.confirmBatchDelete', { count: ids.length }))) return

  batchOperating.value = true
  try {
    const results = await Promise.allSettled(
      ids.map((id) => api.images.delete(id))
    )
    const successCount = results.filter((r) => r.status === 'fulfilled').length
    const failCount = results.length - successCount
    if (failCount === 0) {
      toast.success(t('admin.images.batchDeleteSuccess', { count: successCount }))
    } else {
      toast.warning(`${t('admin.images.batchDeleteSuccess', { count: successCount })}, ${failCount} failed`)
    }
    clearSelection()
    await loadImages()
  } catch (err: any) {
    toast.error(err.message || t('admin.images.deleteFailed'))
  } finally {
    batchOperating.value = false
  }
}

onMounted(() => {
  loadImages()
})
</script>

<template>
  <div class="kawaii-page nimbus-view space-y-6 animate-fade-in">
    <!-- Header -->
    <PageHeader
      :title="t('admin.images.title')"
      :description="t('admin.images.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          :loading="loading"
          @click="loadImages"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" :class="loading ? 'animate-spin' : ''" />
          </template>
          {{ t('admin.images.refresh') }}
        </Button>
        <Button
          variant="primary"
          @click="openCreateModal"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2]" />
          </template>
          {{ t('admin.images.create') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Loading -->
    <Card v-if="loading" padding="lg" class="text-center">
      <div class="flex flex-col items-center justify-center py-12">
        <Loader2 class="w-8 h-8 animate-spin text-[var(--accent)]" />
        <p class="text-sm text-[var(--text-secondary)] mt-4 font-medium">{{ t('common.loading') }}</p>
      </div>
    </Card>

    <template v-else>
      <!-- Architecture Tabs & Filter Summary -->
      <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <!-- Architecture Segmented Control -->
        <div class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full w-fit">
          <button
            v-for="tab in architectureTabs"
            :key="tab.value"
            type="button"
            class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none"
            :class="activeArchitecture === tab.value
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
            @click="activeArchitecture = tab.value"
          >
            <span>{{ tab.labelKey ? t(tab.labelKey) : tab.label }}</span>
            <Badge
              size="sm"
              :variant="activeArchitecture === tab.value ? 'info' : 'default'"
            >
              {{ getArchitectureCount(tab.value) }}
            </Badge>
          </button>
        </div>

        <!-- Filter toggles / summary count -->
        <div class="text-xs text-[var(--text-secondary)] flex items-center gap-2">
          <span>共 {{ filteredImages.length }} 个镜像</span>
          <Button
            v-if="hasActiveFilters"
            variant="ghost"
            size="sm"
            class="text-[var(--text-secondary)]"
            @click="resetFilters"
          >
            重置筛选
          </Button>
        </div>
      </div>

      <!-- Filters and Search Toolbar -->
      <Card padding="sm">
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-12 gap-3">
          <!-- Search Input -->
          <div class="lg:col-span-6">
            <Input
              v-model="searchQuery"
              size="sm"
              placeholder="搜索镜像名称、远程别名或系统..."
              clearable
            >
              <template #prefix>
                <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>

          <!-- OS Filter -->
          <div class="lg:col-span-3">
            <Select
              v-model="selectedOs"
              size="sm"
              :options="osFilterOptions"
            />
          </div>

          <!-- Instance Type Filter -->
          <div class="lg:col-span-3">
            <Select
              v-model="selectedInstanceType"
              size="sm"
              :options="instanceTypeFilterOptions"
            />
          </div>
        </div>
      </Card>

      <!-- Batch Actions Toolbar -->
      <div
        v-if="selectedCount > 0"
        class="flex flex-wrap items-center justify-between gap-4 rounded-2xl border border-[var(--accent)]/30 bg-[color-mix(in_srgb,var(--accent)_5%,var(--bg-surface))] p-4 shadow-sm transition-all duration-300"
      >
        <div class="flex items-center gap-3.5">
          <label class="flex items-center gap-2.5 cursor-pointer text-sm font-medium text-[var(--text-primary)] select-none">
            <input
              type="checkbox"
              class="h-4 w-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
              :checked="isAllSelected"
              :indeterminate="isIndeterminate"
              @change="toggleSelectAll(($event.target as HTMLInputElement).checked)"
            />
            <span class="font-semibold">{{ t('admin.images.selectedCount', { count: selectedCount }) }}</span>
          </label>
          <Button
            variant="ghost"
            size="sm"
            @click="clearSelection"
          >
            {{ t('admin.images.clearSelection') }}
          </Button>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            :disabled="batchOperating"
            @click="batchEnable"
          >
            <template #icon>
              <Eye class="w-3.5 h-3.5" />
            </template>
            {{ t('admin.images.batchEnable') }}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            :disabled="batchOperating"
            @click="batchDisable"
          >
            <template #icon>
              <EyeOff class="w-3.5 h-3.5" />
            </template>
            {{ t('admin.images.batchDisable') }}
          </Button>
          <Button
            variant="danger"
            size="sm"
            :disabled="batchOperating"
            @click="batchDelete"
          >
            <template #icon>
              <Trash2 class="w-3.5 h-3.5" />
            </template>
            {{ t('admin.images.batchDelete') }}
          </Button>
        </div>
      </div>

      <!-- Images List Empty -->
      <Card v-if="filteredImages.length === 0" padding="none">
        <EmptyState
          :title="images.length === 0 ? t('admin.images.noImages') : t('admin.images.noImagesForArchitecture')"
          :description="hasActiveFilters ? '未找到符合当前筛选条件的镜像，请尝试更改搜索词或重置筛选' : '点击下方按钮开始添加第一个镜像'"
        >
          <template #icon>
            <Layers class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template #action>
            <Button
              v-if="hasActiveFilters"
              variant="secondary"
              size="sm"
              @click="resetFilters"
            >
              重置筛选
            </Button>
            <Button
              v-else
              variant="primary"
              size="sm"
              @click="openCreateModal"
            >
              <template #icon>
                <Plus class="w-4 h-4 stroke-[2]" />
              </template>
              {{ t('admin.images.create') }}
            </Button>
          </template>
        </EmptyState>
      </Card>

      <template v-else>
        <!-- Mobile Cards -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="image in paginatedImages"
            :key="image.id"
            padding="sm"
            class="transition-all duration-200"
            :class="[
              image.hidden ? 'opacity-70' : '',
              selectedIds.has(image.id) ? 'ring-2 ring-[var(--accent)]' : ''
            ]"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="flex min-w-0 items-center gap-3">
                <input
                  type="checkbox"
                  class="h-4 w-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer shrink-0"
                  :checked="selectedIds.has(image.id)"
                  @change="toggleSelect(image.id, ($event.target as HTMLInputElement).checked)"
                />
                <div class="w-10 h-10 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center shrink-0">
                  <DistroIcon :distro="image.icon" :size="28" />
                </div>
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ image.name }}</div>
                  <div class="truncate font-mono text-xs text-[var(--text-secondary)] mt-0.5">{{ image.remoteAlias }}</div>
                </div>
              </div>
              <Badge
                size="sm"
                :variant="image.hidden ? 'default' : 'success'"
                :dot="true"
              >
                {{ image.hidden ? t('admin.images.statusHidden') : t('admin.images.statusVisible') }}
              </Badge>
            </div>

            <div class="mt-4 grid grid-cols-3 gap-2 text-xs">
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.images.fields.architecture') }}</div>
                <Badge size="sm" variant="default">
                  {{ image.architecture }}
                </Badge>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.images.fields.instanceType') }}</div>
                <Badge size="sm" variant="info">
                  {{ image.instanceType === 'container' ? t('admin.images.typeContainer') : image.instanceType === 'vm' ? t('admin.images.typeVm') : t('admin.images.typeBoth') }}
                </Badge>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide mb-1">{{ t('admin.images.fields.sortOrder') }}</div>
                <span class="font-mono tabular-nums text-sm font-medium text-[var(--text-primary)]">{{ image.sortOrder }}</span>
              </div>
            </div>

            <div class="mt-4 flex items-center justify-end gap-2 border-t border-[var(--border-color)] pt-3">
              <Button
                variant="secondary"
                size="sm"
                :title="image.hidden ? t('admin.images.show') : t('admin.images.hide')"
                @click="toggleHidden(image)"
              >
                <template #icon>
                  <Eye v-if="image.hidden" class="w-3.5 h-3.5" />
                  <EyeOff v-else class="w-3.5 h-3.5" />
                </template>
                <span>{{ image.hidden ? t('admin.images.show') : t('admin.images.hide') }}</span>
              </Button>
              <Button
                variant="secondary"
                size="sm"
                :title="t('common.edit')"
                @click="openEditModal(image)"
              >
                <template #icon>
                  <Edit3 class="w-3.5 h-3.5" />
                </template>
                <span>{{ t('common.edit') }}</span>
              </Button>
              <Button
                variant="danger"
                size="sm"
                :title="t('common.delete')"
                @click="deleteImage(image)"
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
              <col class="w-[4%]" />
              <col class="w-[6%]" />
              <col class="w-[18%]" />
              <col class="w-[22%]" />
              <col class="w-[11%]" />
              <col class="w-[11%]" />
              <col class="w-[8%]" />
              <col class="w-[8%]" />
              <col class="w-[12%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider select-none">
                <th class="px-4 py-3.5">
                  <input
                    type="checkbox"
                    class="h-4 w-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
                    :checked="isAllSelected"
                    :indeterminate="isIndeterminate"
                    :aria-label="t('admin.images.selectAll')"
                    @change="toggleSelectAll(($event.target as HTMLInputElement).checked)"
                  />
                </th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.icon') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.name') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.remoteAlias') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.architecture') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.instanceType') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.sortOrder') }}</th>
                <th class="px-4 py-3.5">{{ t('admin.images.fields.status') }}</th>
                <th class="px-4 py-3.5 text-right">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="image in paginatedImages"
                :key="image.id"
                class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
                :class="[
                  image.hidden ? 'opacity-65' : '',
                  selectedIds.has(image.id) ? 'bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))]' : ''
                ]"
              >
                <td class="px-4 py-3.5">
                  <input
                    type="checkbox"
                    class="h-4 w-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
                    :checked="selectedIds.has(image.id)"
                    @change="toggleSelect(image.id, ($event.target as HTMLInputElement).checked)"
                  />
                </td>
                <td class="px-4 py-3.5">
                  <div class="w-9 h-9 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center">
                    <DistroIcon :distro="image.icon" :size="22" />
                  </div>
                </td>
                <td class="px-4 py-3.5 text-sm font-semibold text-[var(--text-primary)] truncate" :title="image.name">
                  {{ image.name }}
                </td>
                <td class="px-4 py-3.5 font-mono text-xs text-[var(--text-secondary)] truncate" :title="image.remoteAlias">
                  {{ image.remoteAlias }}
                </td>
                <td class="px-4 py-3.5">
                  <Badge size="sm" variant="default">
                    {{ image.architecture }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5">
                  <Badge size="sm" variant="info">
                    {{ image.instanceType === 'container' ? t('admin.images.typeContainer') : image.instanceType === 'vm' ? t('admin.images.typeVm') : t('admin.images.typeBoth') }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 font-mono tabular-nums text-xs text-[var(--text-secondary)]">
                  {{ image.sortOrder }}
                </td>
                <td class="px-4 py-3.5">
                  <Badge
                    size="sm"
                    :variant="image.hidden ? 'default' : 'success'"
                    :dot="true"
                  >
                    {{ image.hidden ? t('admin.images.statusHidden') : t('admin.images.statusVisible') }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 text-right">
                  <div class="flex items-center justify-end gap-1.5">
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
                      :title="image.hidden ? t('admin.images.show') : t('admin.images.hide')"
                      @click="toggleHidden(image)"
                    >
                      <Eye v-if="image.hidden" class="w-4 h-4" />
                      <EyeOff v-else class="w-4 h-4" />
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--accent)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
                      :title="t('common.edit')"
                      @click="openEditModal(image)"
                    >
                      <Edit3 class="w-4 h-4" />
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--error)]"
                      :title="t('common.delete')"
                      @click="deleteImage(image)"
                    >
                      <Trash2 class="w-4 h-4" />
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- Pagination -->
        <Card v-if="filteredImages.length > 0" padding="sm">
          <Pagination
            v-model:page="page"
            v-model:page-size="pageSize"
            :total="filteredImages.length"
            :show-size-changer="filteredImages.length > 10"
            :page-size-options="[10, 15, 20, 50]"
          />
        </Card>
      </template>
    </template>

    <!-- Drawer Modal -->
    <DrawerModal
      :show="showModal"
      max-width="max-w-lg"
      raw
      @close="showModal = false"
    >
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface)]">
        <div class="flex items-center gap-2.5 min-w-0">
          <div class="w-8 h-8 rounded-full bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0">
            <Layers class="w-4 h-4 stroke-[2]" />
          </div>
          <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">{{ modalTitle }}</h3>
        </div>
        <button
          type="button"
          class="w-8 h-8 inline-flex items-center justify-center rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
          :aria-label="t('common.cancel')"
          @click="showModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <form class="flex-1 flex flex-col min-h-0 bg-[var(--bg-surface)]" @submit.prevent="saveImage">
        <div class="p-6 space-y-4 overflow-y-auto flex-1">
          <!-- Name -->
          <Input
            v-model="form.name"
            :label="`${t('admin.images.fields.name')} *`"
            :placeholder="t('admin.images.placeholder.name')"
          />

          <!-- Remote Alias -->
          <Input
            v-model="form.remoteAlias"
            :label="`${t('admin.images.fields.remoteAlias')} *`"
            :placeholder="t('admin.images.placeholder.remoteAlias')"
            :hint="t('admin.images.hint.remoteAlias')"
          />

          <!-- OS Type & Architecture (2 cols) -->
          <div class="grid grid-cols-2 gap-4">
            <Input
              v-model="form.osType"
              :label="t('admin.images.fields.osType')"
              placeholder="Linux"
            />
            <Select
              v-model="form.architecture"
              :label="t('admin.images.fields.architecture')"
              :options="architectureOptions"
            />
          </div>

          <!-- Instance Type -->
          <Select
            v-model="form.instanceType"
            :label="t('admin.images.fields.instanceType')"
            :options="instanceTypeOptions"
            :hint="t('admin.images.hint.instanceType')"
          />

          <!-- Distro Icon with preview -->
          <div>
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between mb-1.5">
              <span>{{ t('admin.images.fields.icon') }} *</span>
            </label>
            <div class="flex items-center gap-3">
              <div v-if="form.icon" class="w-11 h-11 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-center shrink-0">
                <DistroIcon :distro="form.icon" :size="28" />
              </div>
              <div class="flex-1 min-w-0">
                <Select
                  v-model="form.icon"
                  :options="iconSelectOptions"
                  :placeholder="t('admin.images.placeholder.icon')"
                />
              </div>
            </div>
          </div>

          <!-- Sort Order -->
          <Input
            v-model.number="form.sortOrder"
            type="number"
            :label="t('admin.images.fields.sortOrder')"
            :hint="t('admin.images.hint.sortOrder')"
          />

          <!-- Hidden Checkbox / Switch -->
          <label class="flex items-start gap-3 p-3.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer hover:bg-[var(--bg-surface)] transition-colors select-none">
            <input
              id="hidden"
              v-model="form.hidden"
              type="checkbox"
              class="w-4 h-4 mt-0.5 rounded text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer shrink-0"
            />
            <div class="text-sm">
              <span class="font-medium text-[var(--text-primary)]">{{ t('admin.images.fields.hidden') }}</span>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">勾选后用户在新建实例时将无法看到并选择此镜像</p>
            </div>
          </label>
        </div>

        <div class="flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
          <Button type="button" variant="secondary" @click="showModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button type="submit" variant="primary" :loading="saving">
            {{ isEditMode ? t('common.save') : t('common.create') }}
          </Button>
        </div>
      </form>
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
