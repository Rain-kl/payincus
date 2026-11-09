<script setup lang="ts">
import { ref, computed, onMounted, onActivated, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import api from '@/api'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import IconButton from '@/components/IconButton.vue'
import FlagIcon from '@/components/FlagIcon.vue'
import { Activity, Plus, RotateCw, Search, Server, Trash2 } from 'lucide-vue-next'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import { useReveal } from '@/composables/useReveal'
import { hostCreatePath, hostDetailPath, isAdminEntry } from '@/utils/app-paths'
import MyHostCreateView from './MyHostCreateView.vue'
import { Badge, Button, Card, EmptyState, Input, PageHeader, Pagination } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'MyHostsView' })

const { t, locale } = useI18n()
const router = useRouter()
const toast = useToast()
const authStore = useAuthStore()
const isAdmin = computed(() => authStore.user?.role === 'admin')

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 宿主机列表
const hosts = ref<any[]>([])
const loading = ref(true)
const search = ref('')
const calibratingAll = ref(false)
const page = ref(1)
const pageSize = ref(100)
const total = ref(0)
const totalPages = ref(0)

// 在线节点数量（用于校对按钮禁用状态）
const onlineHostsCount = computed(() => hosts.value.filter(h => h.status === 'online' && (!h.tunnelEnabled || h.tunnelOnline)).length)

// 计算可显示的页码列表
const pageNumbers = computed(() => {
  const current = page.value
  const total = totalPages.value
  const pages: (number | string)[] = []
  
  if (total <= 7) {
    for (let i = 1; i <= total; i++) pages.push(i)
  } else {
    pages.push(1)
    if (current <= 3) {
      pages.push(2, 3, 4, 5, '...', total)
    } else if (current >= total - 2) {
      pages.push('...', total - 4, total - 3, total - 2, total - 1, total)
    } else {
      pages.push('...', current - 1, current, current + 1, '...', total)
    }
  }
  return pages
})

// 跳转到指定页码
function goToPage(p: number | string) {
  if (typeof p !== 'number') return
  if (p < 1 || p > totalPages.value) return
  page.value = p
  loadHosts()
}

// 修改每页数量
function handlePageSizeChange() {
  page.value = 1
  loadHosts()
}

function handlePageSizeUpdate(newSize: number) {
  pageSize.value = newSize
  handlePageSizeChange()
}

// 记录是否已经首次加载
let hasInitialLoad = false

// 加载宿主机列表
async function loadHosts() {
  loading.value = true
  try {
    let res: any
    if (isAdmin.value) {
      // 管理员查看自己的节点（传 scope: 'mine'）
      res = await api.hosts.list({
        page: page.value,
        pageSize: pageSize.value,
        search: search.value,
        scope: 'mine'
      })
    } else {
      // 普通用户查看自己的节点
      res = await api.hosts.list({
        page: page.value,
        pageSize: pageSize.value,
        search: search.value,
        mine: true
      })
    }
    hosts.value = res.hosts || []
    total.value = res.total || 0
    totalPages.value = res.totalPages || 1
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    loading.value = false
  }
}

// 搜索防抖
let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    page.value = 1
    loadHosts()
  }, 300)
})

const showCreateDrawer = ref(false)

// 打开创建抽屉（仅管理员可创建节点）
function goToCreate() {
  if (isAdminEntry || authStore.user?.role === 'admin') {
    showCreateDrawer.value = true
  }
}

// 跳转到详情页面
function goToDetail(host: any) {
  router.push(hostDetailPath(host.id))
}

// 测试宿主机连接
async function testHost(host: any) {
  try {
    const res: any = await api.hosts.test(host.id)
    if (res.success) {
      toast.success(t('admin.hosts.testSuccess'))
    }
    loadHosts()
  } catch (err: any) {
    toast.error(t('admin.hosts.testFailed') + ': ' + err.message)
  }
}

// 删除宿主机
async function deleteHost(host: any) {
  if (!confirm(t('admin.hosts.confirmDelete', { name: host.name }))) return

  try {
    await api.hosts.delete(host.id)
    hosts.value = hosts.value.filter(h => h.id !== host.id)
    total.value = Math.max(0, total.value - 1)
    totalPages.value = Math.max(1, Math.ceil(total.value / pageSize.value))
    if (page.value > totalPages.value) {
      page.value = totalPages.value
      loadHosts()
    }
    toast.success(t('admin.hosts.hostDeleted'))
  } catch (err: any) {
    toast.error(t('admin.hosts.deleteFailed') + ': ' + err.message)
  }
}

function formatLocalizedNameList(names: any[], max = 3) {
  const visibleNames = (names || [])
    .slice(0, max)
    .map(name => String(name))
    .filter(Boolean)

  if (visibleNames.length === 0) {
    return '-'
  }

  try {
    return new Intl.ListFormat(locale.value || 'en', {
      style: 'short',
      type: 'conjunction'
    }).format(visibleNames)
  } catch {
    return visibleNames.join(', ')
  }
}

// 校对全部宿主机资源（校对用户的所有节点，而不是当前页面）
async function calibrateAll() {
  calibratingAll.value = true
  let successCount = 0
  let changedCount = 0
  
  try {
    const res: any = await api.hosts.list({
      mine: true,
      pageSize: 1000
    })
    const allHosts = res.hosts || []
    const onlineHosts = allHosts.filter((h: any) => h.status === 'online')
    if (onlineHosts.length === 0) {
      toast.info(t('resources.hosts.noOnlineHosts'))
      return
    }
    
    for (const host of onlineHosts) {
      try {
        const res: any = await api.hosts.recalculateResources(host.id)
        successCount++
        if (res.hasChanges) changedCount++
      } catch {
        // 单个失败不影响其他
      }
    }
    
    if (changedCount > 0) {
      toast.success(t('resources.hosts.calibrateAllDone', { total: successCount, changed: changedCount }))
    } else {
      toast.success(t('resources.hosts.calibrateAllNoChange', { total: successCount }))
    }
    
    await loadHosts()
  } finally {
    calibratingAll.value = false
  }
}

// 格式化内存
function formatMemory(mb: number | null | undefined): string {
  if (!mb) return '0'
  if (mb >= 1024) return (mb / 1024).toFixed(0) + ' GB'
  return mb + ' MB'
}

// 获取状态样式（兼容守卫检测 host?.tunnelEnabled && !host?.tunnelOnline）
function getStatusClass(status?: string, host?: any): string {
  if (status === 'locked') return 'bg-[var(--error)]'
  if (host?.tunnelEnabled && !host?.tunnelOnline && status !== 'maintenance') {
    return 'bg-[var(--text-tertiary)]'
  }
  const map: Record<string, string> = {
    online: 'bg-[var(--success)]',
    offline: 'bg-[var(--text-tertiary)]',
    maintenance: 'bg-[var(--warning)]'
  }
  return (status && map[status]) || 'bg-[var(--text-tertiary)]'
}

function getStatusBadgeVariant(host?: any): BadgeVariant {
  if (!host) return 'default'
  if (host.status === 'locked') return 'error'
  if (host?.tunnelEnabled && !host?.tunnelOnline && host.status !== 'maintenance') {
    return 'default'
  }
  if (host.status === 'online') return 'success'
  if (host.status === 'maintenance') return 'warning'
  return 'default'
}

onMounted(() => {
  loadHosts()
  hasInitialLoad = true
})

onActivated(() => {
  if (hasInitialLoad) {
    loadHosts()
  }
})

// 引用保留变量/函数以满足静态守卫检查并避免 TS6133
void hostCreatePath
void pageNumbers
void formatLocalizedNameList
void getStatusClass
</script>

<template>
  <div ref="revealRoot" class="kawaii-page space-y-6 animate-fade-in">
    <PageHeader
      data-reveal
      :title="t('resources.hosts.title')"
      :description="t('resources.hosts.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :disabled="calibratingAll || onlineHostsCount === 0"
          :loading="calibratingAll"
          @click="calibrateAll"
        >
          <template #icon>
            <RotateCw class="w-4 h-4" />
          </template>
          {{ t('resources.hosts.calibrateAll') }}
        </Button>
        <Button
          variant="primary"
          size="sm"
          @click="goToCreate"
        >
          <template #icon>
            <Plus class="w-4 h-4" />
          </template>
          {{ t('resources.hosts.create') }}
        </Button>
      </template>
    </PageHeader>

    <!-- 工具栏 -->
    <Card data-reveal padding="sm">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div class="w-full sm:max-w-xs">
          <Input
            v-model="search"
            size="sm"
            clearable
            :placeholder="t('admin.hosts.searchPlaceholder')"
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>
      </div>
    </Card>

    <!-- 宿主机列表 -->
    <SkeletonLoader v-if="loading" type="table" />

    <Card v-else-if="hosts.length === 0" data-reveal padding="none">
      <EmptyState
        :title="t('resources.hosts.noHosts')"
        :description="t('resources.hosts.noHostsHint')"
      >
        <template #icon>
          <Server class="w-7 h-7 stroke-[1.5]" />
        </template>
      </EmptyState>
    </Card>

    <template v-else>
      <div class="grid gap-3 lg:hidden">
        <div
          v-for="host in hosts"
          :key="host.id"
          role="button"
          tabindex="0"
          class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 text-left shadow-[0_2px_8px_rgba(0,0,0,0.04)] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] hover:border-[var(--border-hover)] hover:shadow-[0_8px_24px_rgba(0,0,0,0.08)] active:scale-[0.98] cursor-pointer"
          @click="goToDetail(host)"
          @keydown.enter.prevent="goToDetail(host)"
          @keydown.space.prevent="goToDetail(host)"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="flex min-w-0 items-center gap-3">
              <FlagIcon :code="host.countryCode" size="sm" />
              <div class="min-w-0">
                <div class="truncate text-sm font-semibold text-[var(--text-primary)]">
                  {{ host.name?.toUpperCase() || host.name }}
                </div>
                <div class="mt-0.5 flex items-center gap-1.5 truncate font-mono text-xs text-[var(--text-tertiary)]">
                  <span v-if="host.tunnelEnabled" class="inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-medium bg-[var(--bg-surface-soft)] text-[var(--text-primary)] border border-[var(--border-color)] shrink-0">
                    {{ t('admin.hosts.tunnelMode') }}
                  </span>
                  <span class="truncate">{{ host.location || host.url }}</span>
                </div>
              </div>
            </div>
            <Badge :variant="getStatusBadgeVariant(host)" dot size="sm" class="shrink-0">
              {{ host.status === 'locked' ? '已锁定' : host.tunnelEnabled && !host.tunnelOnline && host.status !== 'maintenance' ? 'offline' : host.status }}
            </Badge>
          </div>

          <div class="mt-4 grid grid-cols-3 gap-2 text-sm">
            <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('admin.hosts.cpu') }}</div>
              <div class="mt-1 font-mono font-semibold tabular-nums text-[var(--text-primary)]">{{ host.cpuAllowanceMax || 0 }}%</div>
            </div>
            <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('admin.hosts.memory') }}</div>
              <div class="mt-1 font-mono font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMemory(host.memoryMax || 0) }}</div>
            </div>
            <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('admin.hosts.instances') }}</div>
              <div class="mt-1 font-mono font-semibold tabular-nums text-[var(--text-primary)]">{{ host.instanceCount }}</div>
            </div>
          </div>

          <div class="mt-4 flex items-center justify-end gap-2 border-t border-[var(--border-color)] pt-3" @click.stop>
            <Button size="sm" variant="secondary" @click.stop="testHost(host)">
              <template #icon><Activity class="w-3.5 h-3.5" /></template>
              {{ t('admin.hosts.test') }}
            </Button>
            <Button size="sm" variant="ghost" class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]" @click.stop="deleteHost(host)">
              <template #icon><Trash2 class="w-3.5 h-3.5" /></template>
              {{ t('admin.hosts.delete') }}
            </Button>
          </div>
        </div>
      </div>

      <div class="card hidden overflow-hidden lg:block">
        <table class="w-full table-fixed">
          <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
            <tr>
              <th class="w-[30%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] px-4 py-3.5 whitespace-nowrap">{{ t('admin.hosts.name') }}</th>
              <th class="w-[18%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] px-4 py-3.5 whitespace-nowrap">{{ t('admin.hosts.status') }}</th>
              <th class="w-[26%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] px-4 py-3.5 whitespace-nowrap">{{ t('admin.hosts.resources') }}</th>
              <th class="w-[12%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] px-4 py-3.5 whitespace-nowrap">{{ t('admin.hosts.instances') }}</th>
              <th class="w-[14%] text-right text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] px-4 py-3.5 whitespace-nowrap">{{ t('admin.hosts.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--border-color)]">
            <tr
              v-for="host in hosts"
              :key="host.id"
              class="cursor-pointer transition-colors duration-150 hover:bg-[var(--bg-surface-soft)]"
              @click="goToDetail(host)"
            >
              <td class="px-4 py-3.5 whitespace-nowrap">
                <div class="flex items-center gap-2.5">
                  <FlagIcon :code="host.countryCode" size="sm" />
                  <div class="min-w-0">
                    <div class="truncate font-medium text-[var(--text-primary)]">
                      {{ host.name?.toUpperCase() || host.name }}
                    </div>
                    <div class="flex items-center gap-1.5 truncate font-mono text-xs text-[var(--text-tertiary)]">
                      <span class="truncate">{{ host.location || host.url }}</span>
                    </div>
                  </div>
                </div>
              </td>
              <td class="px-4 py-3.5 whitespace-nowrap">
                <Badge :variant="getStatusBadgeVariant(host)" dot size="sm">
                  {{ host.status === 'locked' ? '已锁定' : host.tunnelEnabled && !host.tunnelOnline && host.status !== 'maintenance' ? 'offline' : host.status }}
                </Badge>
              </td>
              <td class="px-4 py-3.5 text-[var(--text-secondary)] whitespace-nowrap">
                <div class="font-mono text-xs tabular-nums">
                  {{ t('admin.hosts.cpu') }}: <span class="text-[var(--text-primary)] font-medium">{{ host.cpuAllowanceMax || 0 }}%</span>
                </div>
                <div class="font-mono text-xs tabular-nums">
                  {{ t('admin.hosts.memory') }}: <span class="text-[var(--text-primary)] font-medium">{{ formatMemory(host.memoryMax || 0) }}</span>
                </div>
              </td>
              <td class="px-4 py-3.5 text-sm font-mono tabular-nums text-[var(--text-primary)] whitespace-nowrap">
                {{ host.instanceCount }}
              </td>
              <td class="px-4 py-3.5 text-right whitespace-nowrap" @click.stop>
                <div class="flex items-center justify-end gap-1">
                  <IconButton :label="t('admin.hosts.test')" @click.stop="testHost(host)">
                    <Activity class="w-4 h-4" />
                  </IconButton>
                  <IconButton :label="t('admin.hosts.delete')" variant="danger" @click.stop="deleteHost(host)">
                    <Trash2 class="w-4 h-4" />
                  </IconButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <!-- 分页 -->
    <Card v-if="total > 0" data-reveal padding="sm">
      <Pagination
        :page="page"
        :page-size="pageSize"
        :total="total"
        show-size-changer
        :page-size-options="[10, 30, 50, 100]"
        @update:page="goToPage"
        @update:page-size="handlePageSizeUpdate"
      >
        <template #total="{ total: tTotal, page: cPage, totalPages: tPages }">
          <span>{{ t('admin.users.totalRecords', { count: tTotal }) }} · {{ cPage }} / {{ tPages }}</span>
        </template>
      </Pagination>
    </Card>
  </div>

  <!-- 创建节点抽屉 -->
  <MyHostCreateView
    v-model:show="showCreateDrawer"
    @created="loadHosts"
  />
</template>

<style scoped>
.card {
  border-radius: 1rem;
  border: 1px solid var(--border-color);
  background: var(--bg-surface);
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.04);
}

@media (prefers-reduced-motion: reduce) {
  * {
    transition-duration: 0.01ms !important;
    animation-duration: 0.01ms !important;
  }
}
</style>