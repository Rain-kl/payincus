<script setup lang="ts">
import { ref, onMounted, onActivated, computed, watch, type Component } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import HostGpuTab from '@/components/host/HostGpuTab.vue'
import HostFirewallTab from '@/components/host/HostFirewallTab.vue'
import HostInfoTab from '@/components/host/HostInfoTab.vue'
import HostImagesTab from '@/components/host/HostImagesTab.vue'
import MyHostConfigTab from '@/components/host/MyHostConfigTab.vue'
import HostInstancesTab from '@/components/host/HostInstancesTab.vue'
import HostStorageTab from '@/components/host/HostStorageTab.vue'
import HostPublicIpv4Tab from '@/components/host/HostPublicIpv4Tab.vue'
import HostCaddyTab from '@/components/host/HostCaddyTab.vue'
import HostAgentLogsTab from '@/components/host/HostAgentLogsTab.vue'
import HostRedeemCodesTab from '@/components/host/HostRedeemCodesTab.vue'
import HostCreateInstanceTab from '@/components/host/HostCreateInstanceTab.vue'
import HostOpsTab from '@/components/host/HostOpsTab.vue'
import FlagIcon from '@/components/FlagIcon.vue'
import { hostsPath } from '@/utils/app-paths'
import { useTabQuery } from '@/composables/useTabQuery'
import { useReveal } from '@/composables/useReveal'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  type BadgeVariant
} from '@/components/ui'
import {
  Activity,
  AlertCircle,
  AlertTriangle,
  Calendar,
  Check,
  Copy,
  Cpu,
  ExternalLink,
  FileText,
  Gift,
  Globe,
  HardDrive,
  Info,
  Layers,
  Plus,
  Server,
  Shield,
  ShieldCheck,
  Sliders,
  Terminal,
  Trash2,
  X
} from 'lucide-vue-next'

// 为 KeepAlive exclude 匹配定义组件名称
defineOptions({
  name: 'MyHostDetailView'
})

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToast()
const authStore = useAuthStore()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 是否是管理员
const isAdmin = computed(() => authStore.user?.role === 'admin')

type TabType = 'firewall' | 'gpu' | 'info' | 'config' | 'images' | 'instances' | 'storage' | 'publicIpv4' | 'caddy' | 'agentLogs' | 'redeemCodes' | 'ops' | 'create'
const activeTab = useTabQuery<TabType>({
  keys: ['firewall', 'gpu', 'info', 'config', 'images', 'instances', 'storage', 'publicIpv4', 'caddy', 'agentLogs', 'redeemCodes', 'ops', 'create'],
  fallback: 'info'
})

interface Host {
  id: number
  name: string
  url: string
  location?: string
  countryCode: string
  status: 'online' | 'offline' | 'maintenance' | 'locked'
  instanceCount?: number
  certPath?: string
  keyPath?: string
  cpuAllowanceMax?: number
  memoryMax?: number
  instanceType?: 'container' | 'vm' | 'both'
  notifyPurchase?: boolean
  notifyRenew?: boolean
  notifyDestroy?: boolean
  resources?: {
    cpuUsed: number
    memoryUsed: number
    diskTotal: number // 实例存储池容量（MB）
    diskUsed: number
  }
  natConfig?: {
    publicIp: string | null
    publicIpv6?: string | null
    bindIp?: string | null
    bindIpv6?: string | null
    portRangeStart: number | null
    portRangeEnd: number | null
    portsUsedCount?: number
  }
  tunnelEnabled?: boolean
  tunnelOnline?: boolean
  createdAt?: string
  updatedAt?: string
}

const host = ref<Host | null>(null)
const loading = ref(true)
const actionLoading = ref('')

// 删除确认弹窗
const showDeleteModal = ref(false)
const deleteConfirmName = ref('')

// 批量延期弹窗（仅管理员）
const showExtendModal = ref(false)
const extendDays = ref<number | undefined>(undefined)
const paidInstanceCount = ref(0)

// Incus 节点安装 / 配置抽屉
const showReinstallModal = ref(false)
const reinstallCommand = ref('')
const installTokenExpiresAt = ref('')

const reinstallStatus = ref<'waiting' | 'verifying' | 'success' | 'error'>('waiting')
const reinstallError = ref('')

const statusInfo = computed(() => {
  if (!host.value) return { label: '', class: '', dot: '', variant: 'default' as BadgeVariant }
  const map: Record<string, { label: string; class: string; dot: string; variant: BadgeVariant }> = {
    locked: { label: '已锁定', class: 'badge-error', dot: 'bg-[var(--error)]', variant: 'error' },
    online: { label: t('admin.hosts.statusOnline'), class: 'badge-success', dot: 'bg-[var(--success)]', variant: 'success' },
    offline: { label: t('admin.hosts.statusOffline'), class: 'badge-default', dot: 'bg-[var(--text-tertiary)]', variant: 'default' },
    maintenance: { label: t('admin.hosts.statusMaintenance'), class: 'badge-warning', dot: 'bg-[var(--warning)]', variant: 'warning' }
  }
  let currentStatus = host.value.status
  if (currentStatus !== 'locked' && currentStatus !== 'maintenance' && host.value.tunnelEnabled && !host.value.tunnelOnline) {
    currentStatus = 'offline'
  }
  return map[currentStatus] || map.offline
})

// 本组件被用户端和管理端共用，两侧的路由名不同：
// 用户端 = 'my-host-detail'，管理端 = 'admin-my-host-detail'。
// 之前只比较 'my-host-detail'，导致管理端的守卫永远提前 return、从不重新加载数据。
const HOST_DETAIL_ROUTE_NAMES = ['my-host-detail', 'admin-my-host-detail']
const isOnHostDetailRoute = () => HOST_DETAIL_ROUTE_NAMES.includes(String(route.name ?? ''))

onMounted(async () => {
  await loadHost()
})

// 组件可能被外层 <KeepAlive> 缓存后复用 —— 这种情况下再次进入本页时 onMounted 不会执行。
// 若此时缓存里 host 为空（或路由 id 已变），页面会停在「既不 loading、host 又为 null」的
// 状态，模板两个分支都不成立 → 整页白屏（刷新后因为是全新挂载才恢复正常）。
// 因此在 activated 时按需重新加载。
onActivated(async () => {
  if (!isOnHostDetailRoute()) return
  const currentId = Number.parseInt(String(route.params.id ?? ''), 10)
  if (!Number.isFinite(currentId) || currentId <= 0) return
  if (!host.value || (host.value as any).id !== currentId) {
    loading.value = true
    host.value = null
    await loadHost()
  }
})

// 监听路由参数变化，重新加载数据
watch(() => route.params.id, async (newId, oldId) => {
  // 检查是否仍在节点详情页，防止路由切换到其他页面时误触发（需同时认用户端/管理端两个路由名）
  if (!isOnHostDetailRoute()) return

  if (newId && newId !== oldId) {
    // 重置所有状态
    loading.value = true
    activeTab.value = 'info'
    host.value = null
    actionLoading.value = ''
    
    // 关闭所有弹窗并重置其状态
    showDeleteModal.value = false
    deleteConfirmName.value = ''
    
    showExtendModal.value = false
    extendDays.value = undefined
    paidInstanceCount.value = 0
    
    showReinstallModal.value = false
    reinstallCommand.value = ''
    installTokenExpiresAt.value = ''
    reinstallStatus.value = 'waiting'
    reinstallError.value = ''
    
    await loadHost()
  }
})

async function loadHost() {
  const hostId = parseInt(route.params.id as string)
  if (isNaN(hostId) || hostId <= 0) {
    toast.error(t('admin.hosts.invalidId'))
    router.replace(hostsPath())
    return
  }

  try {
    const res = await api.hosts.get(hostId)
    host.value = (res as any).host || res
  } catch (err: any) {
    toast.error(t('admin.hosts.loadFailed') + ': ' + (err?.message || String(err)))
    router.replace(hostsPath())
  } finally {
    loading.value = false
  }
}

// 兜底态里的「重试」：重新拉一次节点详情
async function retryLoadHost() {
  loading.value = true
  host.value = null
  await loadHost()
}

async function testConnection() {
  if (!host.value) return
  actionLoading.value = 'test'
  try {
    const res = await api.hosts.test(host.value.id)
    if (res.success) {
      toast.success(res.message || t('admin.hosts.testSuccess'))
    }
    await loadHost()
  } catch (err: any) {
    toast.error(t('admin.hosts.testFailed') + ': ' + (err?.message || String(err)))
  } finally {
    actionLoading.value = ''
  }
}

// 验证并连接宿主机（用于未完成安装的宿主机）
async function verifyHost() {
  if (!host.value) return
  actionLoading.value = 'verify'
  try {
    const res = await api.hosts.verify(host.value.id)
    if (res.success) {
      toast.success(res.message || t('admin.hosts.verifySuccess'))
    }
    await loadHost()
  } catch (err: any) {
    toast.error(t('admin.hosts.verifyFailed') + ': ' + (err?.message || String(err)))
  } finally {
    actionLoading.value = ''
  }
}

// 是否需要验证（证书路径未配置）
const needsVerification = computed(() => !host.value?.certPath)

async function openDeleteModal() {
  if (!host.value) return
  
  actionLoading.value = 'delete'
  try {
    const res = await api.instances.list({
      hostId: host.value.id,
      pageSize: 1
    })
    const instanceCount = (res as any).total || 0
    
    if (instanceCount > 0) {
      toast.error(t('admin.hosts.hasInstances', { count: instanceCount }))
      return
    }
    
    deleteConfirmName.value = ''
    showDeleteModal.value = true
  } catch (err: any) {
    toast.error(t('admin.hosts.checkFailed') + ': ' + (err?.message || String(err)))
  } finally {
    actionLoading.value = ''
  }
}

async function confirmDeleteHost() {
  if (!host.value) return
  if (deleteConfirmName.value !== host.value.name) {
    toast.error(t('admin.hosts.deleteNameMismatch'))
    return
  }

  actionLoading.value = 'delete'
  showDeleteModal.value = false
  try {
    await api.hosts.delete(host.value.id)
    toast.success(t('admin.hosts.hostDeleted'))
    router.push(hostsPath())
  } catch (err: any) {
    toast.error(t('admin.hosts.deleteFailed') + ': ' + (err?.message || String(err)))
  } finally {
    actionLoading.value = ''
  }
}

const canDelete = computed(() => {
  return deleteConfirmName.value === host.value?.name
})

function onConfigSaved() {
  loadHost()
}

// 打开批量延期弹窗（仅管理员）
async function openExtendModal() {
  if (!host.value || !isAdmin.value) return

  paidInstanceCount.value = 0
  extendDays.value = undefined
  showExtendModal.value = true
}

// 确认批量延期
async function confirmExtendAll() {
  if (!host.value || !extendDays.value || extendDays.value < 1 || extendDays.value > 365) {
    toast.error(t('admin.hosts.extendDaysInvalid'))
    return
  }
  
  actionLoading.value = 'extend'
  showExtendModal.value = false
  try {
    const res = await api.hosts.batchExtendAll(host.value.id, extendDays.value)
    if (res.success) {
      toast.success(t('admin.hosts.extendSuccess', { count: res.extendedCount, days: extendDays.value }))
    } else {
      toast.info(res.message)
    }
  } catch (err: any) {
    toast.error(t('admin.hosts.extendFailed') + ': ' + (err?.message || String(err)))
  } finally {
    actionLoading.value = ''
  }
}

function openIncusUi() {
  if (!host.value || !isAdmin.value) return
  if (!host.value.tunnelOnline) {
    toast.error(t('admin.hosts.incusUiTunnelOffline'))
    return
  }
  window.open(`/incus-ui/${host.value.id}/ui`, '_blank', 'noopener,noreferrer')
}

function openNodeInstall() {
  showReinstallModal.value = true
}

// 用户确认后再生成命令，打开抽屉不会轮换节点凭据。
async function regenerateInstall() {
  if (!host.value || actionLoading.value) return
  
  actionLoading.value = 'reinstall'
  try {
    const res = await api.hosts.generateAgentInstallCommand(host.value.id)
    reinstallCommand.value = res.installCommand
    installTokenExpiresAt.value = res.installTokenExpiresAt
    reinstallStatus.value = 'waiting'
    reinstallError.value = ''
  } catch (err: any) {
    toast.error(t('admin.hosts.agentInstallCommandFailed') + ': ' + (err?.message || String(err)))
  } finally {
    actionLoading.value = ''
  }
}

// 重新安装后验证
async function verifyAfterReinstall() {
  if (!host.value) return
  reinstallStatus.value = 'verifying'
  reinstallError.value = ''
  
  try {
    const res = needsVerification.value
      ? await api.hosts.verify(host.value.id)
      : await api.hosts.test(host.value.id)
    if (res.success) {
      reinstallStatus.value = 'success'
      toast.success(t('admin.hosts.verifySuccess'))
      await loadHost()
    }
  } catch (err: any) {
    reinstallStatus.value = 'error'
    reinstallError.value = err?.message || String(err)
    toast.error(t('admin.hosts.verifyFailed') + ': ' + reinstallError.value)
  }
}

// 复制重新安装命令
function copyReinstallCommand() {
  navigator.clipboard.writeText(reinstallCommand.value)
  toast.success(t('common.copied'))
}

// 关闭重新安装弹窗
function closeReinstallModal() {
  showReinstallModal.value = false
  loadHost()
}

interface TabItem {
  key: TabType
  labelKey: string
  icon: string
  iconComponent: Component
}

const tabDefinitions: TabItem[] = [
  { key: 'info', labelKey: 'admin.hosts.tabInfo', icon: 'M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z', iconComponent: Info },
  { key: 'ops', labelKey: 'admin.hosts.tabOps', icon: 'M5 13l4 4L19 7M12 6v6l4 2', iconComponent: Activity },
  { key: 'gpu', labelKey: 'gpu.title', icon: 'M5 5h14v14H5zM9 9h6v6H9zM9 2v3m6-3v3M9 19v3m6-3v3M2 9h3m14 0h3M2 15h3m14 0h3', iconComponent: Cpu },
  { key: 'config', labelKey: 'admin.hosts.tabConfig', icon: 'M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z', iconComponent: Sliders },
  { key: 'images', labelKey: 'admin.hosts.tabImages', icon: 'M4 16l4.586-4.586a2 2 0 012.828 0L16 16m-2-2l1.586-1.586a2 2 0 012.828 0L20 14m-6-6h.01M6 20h12a2 2 0 002-2V6a2 2 0 00-2-2H6a2 2 0 00-2 2v12a2 2 0 002 2z', iconComponent: Layers },
  { key: 'storage', labelKey: 'admin.hosts.tabStorage', icon: 'M4 7v10c0 2.21 3.582 4 8 4s8-1.79 8-4V7M4 7c0 2.21 3.582 4 8 4s8-1.79 8-4M4 7c0-2.21 3.582-4 8-4s8 1.79 8 4m0 5c0 2.21-3.582 4-8 4s-8-1.79-8-4', iconComponent: HardDrive },
  { key: 'publicIpv4', labelKey: 'admin.hosts.tabPublicIpv4', icon: 'M12 11c0 1.657-1.79 3-4 3s-4-1.343-4-3 1.79-3 4-3 4 1.343 4 3zm0 0c0-1.657 1.79-3 4-3s4 1.343 4 3-1.79 3-4 3-4-1.343-4-3zm-4 3v4m8-4v4M8 6V3m8 3V3', iconComponent: Globe },
  { key: 'caddy', labelKey: 'host.caddy.title', icon: 'M21 12a9 9 0 01-9 9m9-9a9 9 0 00-9-9m9 9H3m9 9a9 9 0 01-9-9m9 9c1.657 0 3-4.03 3-9s-1.343-9-3-9m0 18c-1.657 0-3-4.03-3-9s1.343-9 3-9m-9 9a9 9 0 019-9', iconComponent: Terminal },
  { key: 'agentLogs', labelKey: 'host.agentLogs.title', icon: 'M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z', iconComponent: FileText },
  { key: 'redeemCodes', labelKey: 'redeemCodes.title', icon: 'M15 5v2m0 4v2m0 4v2M5 5a2 2 0 00-2 2v3a2 2 0 110 4v3a2 2 0 002 2h14a2 2 0 002-2v-3a2 2 0 110-4V7a2 2 0 00-2-2H5z', iconComponent: Gift },
  { key: 'instances', labelKey: 'admin.hosts.tabInstances', icon: 'M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10', iconComponent: Server },
  { key: 'create', labelKey: 'admin.hosts.tabCreate', icon: 'M12 4v16m8-8H4', iconComponent: Plus }
]

const tabs = computed<TabItem[]>(() =>
  tabDefinitions.flatMap(tab =>
    tab.key === 'config' && isAdmin.value
      ? [
          tab,
          {
            key: 'firewall' as TabType,
            labelKey: 'egress.firewall',
            icon: 'M12 3l8 4v5c0 5-8 9-8 9s-8-4-8-9V7l8-4z',
            iconComponent: Shield
          }
        ]
      : [tab]
  )
)

// 非管理员没有 firewall tab；URL 直接落到该 tab（如分享链接）时回退到 info，避免空白
watch(tabs, (list) => {
  if (!list.some(tab => tab.key === activeTab.value)) {
    activeTab.value = 'info'
  }
}, { immediate: true })
</script>

<template>
  <div ref="revealRoot" class="kawaii-page space-y-6 animate-fade-in">
    <div v-if="loading">
      <SkeletonLoader type="detail" />
    </div>

    <template v-else-if="host">
      <!-- Apple Style PageHeader -->
      <PageHeader
        data-reveal
        :title="host.name"
        :description="host.location || host.url"
        back
        @back="router.replace(hostsPath())"
      >
        <template #title>
          <div class="flex flex-wrap items-center gap-2.5">
            <FlagIcon v-if="host.countryCode" :code="host.countryCode" size="md" />
            <h1 class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)] uppercase truncate">
              {{ host.name }}
            </h1>
            <Badge :variant="statusInfo.variant" dot pulse>
              {{ statusInfo.label }}
            </Badge>
          </div>
        </template>

        <template #description>
          <p class="text-sm font-mono text-[var(--text-secondary)] mt-1 truncate">
            {{ host.location || host.url }}
          </p>
        </template>

        <template #actions>
          <!-- IncusUI 内网穿透控制面板入口（仅管理员可见） -->
          <Button
            v-if="isAdmin"
            variant="secondary"
            size="sm"
            :disabled="!!actionLoading || needsVerification || !host.tunnelOnline"
            :title="!host.tunnelOnline ? t('admin.hosts.incusUiTunnelOffline') : t('admin.hosts.incusUi')"
            @click="openIncusUi"
          >
            <template #icon>
              <ExternalLink class="w-4 h-4" />
            </template>
            <span>{{ t('admin.hosts.incusUi') }}</span>
          </Button>

          <!-- 批量延期按钮（仅管理员可见） -->
          <Button
            v-if="isAdmin"
            variant="secondary"
            size="sm"
            :disabled="!!actionLoading"
            :loading="actionLoading === 'extend'"
            @click="openExtendModal"
          >
            <template #icon>
              <Calendar class="w-4 h-4" />
            </template>
            <span>{{ t('admin.hosts.batchExtend') }}</span>
          </Button>

          <!-- 唯一的节点安装 / 配置入口 -->
          <Button
            variant="secondary"
            size="sm"
            :disabled="!!actionLoading"
            :loading="actionLoading === 'reinstall'"
            @click="openNodeInstall"
          >
            <template #icon>
              <Terminal class="w-4 h-4" />
            </template>
            <span>{{ t('admin.hosts.nodeInstallConfigure') }}</span>
          </Button>

          <!-- 未验证的宿主机显示验证按钮 -->
          <Button
            v-if="needsVerification"
            variant="primary"
            size="sm"
            :disabled="!!actionLoading"
            :loading="actionLoading === 'verify'"
            @click="verifyHost"
          >
            <template #icon>
              <ShieldCheck class="w-4 h-4" />
            </template>
            <span>{{ t('admin.hosts.verifyAndConnect') }}</span>
          </Button>

          <!-- 已验证的宿主机显示测试连接按钮 -->
          <Button
            v-else
            variant="secondary"
            size="sm"
            :disabled="!!actionLoading"
            :loading="actionLoading === 'test'"
            @click="testConnection"
          >
            <template #icon>
              <Activity class="w-4 h-4" />
            </template>
            <span>{{ t('admin.hosts.test') }}</span>
          </Button>

          <!-- 删除宿主机 -->
          <Button
            variant="danger"
            size="sm"
            :disabled="!!actionLoading"
            :loading="actionLoading === 'delete'"
            @click="openDeleteModal"
          >
            <template #icon>
              <Trash2 class="w-4 h-4" />
            </template>
            <span>{{ t('common.delete') }}</span>
          </Button>
        </template>
      </PageHeader>

      <!-- Apple Segmented Tab 导航 -->
      <div data-reveal class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full scrollbar-hide">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          type="button"
          class="flex items-center gap-2 px-3.5 py-2 rounded-xl text-xs sm:text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] shrink-0 select-none"
          :class="activeTab === tab.key
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/50'"
          @click="activeTab = tab.key as TabType"
        >
          <component :is="tab.iconComponent" v-if="tab.iconComponent" class="w-4 h-4 shrink-0 stroke-[1.8]" />
          <span>{{ t(tab.labelKey) }}</span>
        </button>
      </div>

      <!-- Tab 内容区 -->
      <div class="transition-opacity duration-200">
        <HostGpuTab v-if="activeTab === 'gpu'" :host-id="host.id" />
        <HostInfoTab v-if="activeTab === 'info'" :host="host" @refresh="loadHost" />
        <MyHostConfigTab v-if="activeTab === 'config'" :host="host" @saved="onConfigSaved" />
        <HostFirewallTab v-if="activeTab === 'firewall' && isAdmin" :host-id="host.id" />
        <HostImagesTab v-if="activeTab === 'images'" :host-id="host.id" :host-name="host.name" />
        <HostStorageTab v-if="activeTab === 'storage'" :host-id="host.id" @updated="loadHost" />
        <HostPublicIpv4Tab v-if="activeTab === 'publicIpv4'" :host-id="host.id" />
        <HostCaddyTab v-if="activeTab === 'caddy'" :host-id="host.id" />
        <HostAgentLogsTab v-if="activeTab === 'agentLogs'" :host-id="host.id" />
        <HostRedeemCodesTab v-if="activeTab === 'redeemCodes'" :host-id="host.id" />
        <HostInstancesTab v-if="activeTab === 'instances'" :host-id="host.id" :host-name="host.name" />
        <HostOpsTab v-if="activeTab === 'ops'" :host-id="host.id" :host-name="host.name" @refresh="loadHost" />
        <HostCreateInstanceTab v-if="activeTab === 'create'" :host-id="host.id" :host-name="host.name" :instance-type="host.instanceType" />
      </div>

      <!-- 批量延期弹窗 -->
      <Teleport to="body">
        <Transition name="modal">
          <div v-if="showExtendModal" class="modal-overlay">
            <div class="modal-backdrop" @click="showExtendModal = false"></div>
            <div class="modal-content max-w-md">
              <div class="modal-header">
                <div class="flex items-center gap-3">
                  <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_25%,var(--bg-surface))]">
                    <Calendar class="w-5 h-5 stroke-[2]" />
                  </div>
                  <div>
                    <h3 class="modal-title">{{ t('admin.hosts.batchExtend') }}</h3>
                    <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ host.name }}</p>
                  </div>
                </div>
                <button
                  type="button"
                  class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
                  @click="showExtendModal = false"
                >
                  <X class="w-4 h-4 stroke-[2]" />
                </button>
              </div>
              <div class="modal-body space-y-4">
                <div class="flex items-start gap-2.5 rounded-2xl border border-[color-mix(in_srgb,var(--accent)_20%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))] p-3.5 text-sm text-[var(--text-secondary)]">
                  <Info class="w-5 h-5 shrink-0 text-[var(--accent)] stroke-[2] mt-0.5" />
                  <p class="leading-relaxed">
                    {{ t('admin.hosts.extendHint', { count: paidInstanceCount }) }}
                  </p>
                </div>
                <div class="space-y-1.5">
                  <Input
                    :model-value="extendDays ?? ''"
                    type="number"
                    min="1"
                    max="365"
                    :label="t('admin.hosts.extendDaysLabel')"
                    :placeholder="t('admin.hosts.extendDaysPlaceholder')"
                    @update:model-value="v => extendDays = v === '' ? undefined : Number(v)"
                  />
                </div>
              </div>
              <div class="modal-footer flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface)]">
                <Button variant="secondary" @click="showExtendModal = false">{{ t('common.cancel') }}</Button>
                <Button
                  variant="primary"
                  :disabled="!extendDays || extendDays < 1 || extendDays > 365"
                  @click="confirmExtendAll"
                >
                  {{ t('admin.hosts.confirmExtendBtn') }}
                </Button>
              </div>
            </div>
          </div>
        </Transition>
      </Teleport>

      <!-- 删除确认弹窗 -->
      <Teleport to="body">
        <Transition name="modal">
          <div v-if="showDeleteModal" class="modal-overlay">
            <div class="modal-backdrop" @click="showDeleteModal = false"></div>
            <div class="modal-content max-w-md">
              <div class="modal-header">
                <div class="flex items-center gap-3">
                  <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] border border-[color-mix(in_srgb,var(--error)_25%,var(--bg-surface))]">
                    <Trash2 class="w-5 h-5 stroke-[2]" />
                  </div>
                  <div>
                    <h3 class="modal-title text-[var(--error)]">{{ t('admin.hosts.deleteHost') }}</h3>
                    <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ host.name }}</p>
                  </div>
                </div>
                <button
                  type="button"
                  class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
                  @click="showDeleteModal = false"
                >
                  <X class="w-4 h-4 stroke-[2]" />
                </button>
              </div>
              <div class="modal-body space-y-4">
                <div class="p-3.5 rounded-2xl bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] text-[var(--error)] border border-[color-mix(in_srgb,var(--error)_25%,var(--bg-surface))] flex items-start gap-2.5">
                  <AlertTriangle class="w-5 h-5 shrink-0 stroke-[2] mt-0.5" />
                  <p class="text-sm leading-relaxed">{{ t('admin.hosts.deleteWarning') }}</p>
                </div>
                <p class="text-sm text-[var(--text-secondary)] leading-relaxed">
                  {{ t('admin.hosts.deleteConfirmHint', { name: host.name }) }}
                </p>
                <div class="space-y-1.5">
                  <Input
                    v-model="deleteConfirmName"
                    :label="t('admin.hosts.enterHostName')"
                    :placeholder="host.name"
                    autocomplete="off"
                  />
                </div>
              </div>
              <div class="modal-footer flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface)]">
                <Button variant="secondary" @click="showDeleteModal = false">{{ t('common.cancel') }}</Button>
                <Button
                  variant="danger"
                  :disabled="!canDelete"
                  @click="confirmDeleteHost"
                >
                  {{ t('admin.hosts.confirmDeleteBtn') }}
                </Button>
              </div>
            </div>
          </div>
        </Transition>
      </Teleport>

      <!-- 统一的 Incus 节点安装 / 配置抽屉 -->
      <Teleport to="body">
        <Transition name="modal">
          <div v-if="showReinstallModal" class="modal-overlay">
            <div class="modal-backdrop" @click="closeReinstallModal"></div>
            <div class="modal-content">
              <div class="modal-header">
                <div class="flex items-center gap-3">
                  <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_25%,var(--bg-surface))]">
                    <Terminal class="w-5 h-5 stroke-[2]" />
                  </div>
                  <div>
                    <h3 class="modal-title">{{ t('admin.hosts.nodeInstallConfigure') }}</h3>
                    <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ host.name }}</p>
                  </div>
                </div>
                <button
                  type="button"
                  class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
                  @click="closeReinstallModal"
                >
                  <X class="w-4 h-4 stroke-[2]" />
                </button>
              </div>
              <div class="modal-body space-y-5">
                <p class="text-sm text-[var(--text-secondary)] leading-relaxed">{{ t('admin.hosts.nodeInstallUnifiedHint') }}</p>
                <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-sm text-[var(--text-primary)] leading-relaxed">
                  {{ t('admin.hosts.agentInstallCommandConfirm') }}
                </div>
                
                <!-- 步骤 1: 执行安装命令 -->
                <div v-if="reinstallCommand" class="space-y-3 pt-2">
                  <div class="flex items-center gap-2.5">
                    <span
                      class="flex items-center justify-center w-6 h-6 rounded-full text-xs font-semibold transition-colors"
                      :class="reinstallStatus === 'success' ? 'bg-[var(--success)] text-white' : 'bg-[var(--accent)] text-white'"
                    >
                      1
                    </span>
                    <span class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.hosts.step1RunScript') }}</span>
                  </div>
                  <p class="text-xs text-[var(--text-secondary)] ml-8">{{ t('admin.hosts.runOnHost') }}</p>
                  <p v-if="installTokenExpiresAt" class="text-xs text-[var(--text-secondary)] ml-8">
                    {{ t('admin.hosts.agentInstallTokenExpiresAt', { time: new Date(installTokenExpiresAt).toLocaleString() }) }}
                  </p>
                  <div class="ml-8 overflow-x-auto rounded-2xl border border-[var(--border-color)] bg-[var(--bg-elevated)] p-4 shadow-inner">
                    <code class="text-[var(--success)] text-xs sm:text-sm break-all whitespace-pre-wrap font-mono select-all">{{ reinstallCommand }}</code>
                  </div>
                  <div class="ml-8">
                    <Button
                      variant="secondary"
                      size="sm"
                      @click="copyReinstallCommand"
                    >
                      <template #icon>
                        <Copy class="w-3.5 h-3.5" />
                      </template>
                      {{ t('admin.hosts.copyCommand') }}
                    </Button>
                  </div>
                </div>

                <!-- 步骤 2: 验证并连接 -->
                <div v-if="reinstallCommand" class="space-y-3 pt-4 border-t border-[var(--border-color)]">
                  <div class="flex items-center gap-2.5">
                    <span
                      class="flex items-center justify-center w-6 h-6 rounded-full text-xs font-semibold transition-colors"
                      :class="reinstallStatus === 'success' ? 'bg-[var(--success)] text-white' : 'bg-[var(--text-tertiary)] text-white'"
                    >
                      2
                    </span>
                    <span class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.hosts.step2Verify') }}</span>
                  </div>
                  <p class="text-xs text-[var(--text-secondary)] ml-8">{{ t('admin.hosts.verifyHint') }}</p>
                
                  <!-- 状态显示 -->
                  <div class="ml-8 flex items-center gap-3">
                    <Button
                      variant="secondary"
                      size="sm"
                      :disabled="reinstallStatus === 'verifying' || reinstallStatus === 'success'"
                      :loading="reinstallStatus === 'verifying'"
                      @click="verifyAfterReinstall"
                    >
                      {{ reinstallStatus === 'verifying' ? t('admin.hosts.verifying') : t('admin.hosts.verifyAndConnect') }}
                    </Button>
                  
                    <!-- 成功状态 -->
                    <div v-if="reinstallStatus === 'success'" class="flex items-center gap-2 text-[var(--success)]">
                      <Check class="w-5 h-5 stroke-[2.5]" />
                      <span class="text-sm font-medium">{{ t('admin.hosts.verifySuccess') }}</span>
                    </div>
                  </div>
                
                  <!-- 错误提示 -->
                  <div
                    v-if="reinstallStatus === 'error' && reinstallError"
                    class="ml-8 p-3.5 rounded-2xl bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] text-[var(--error)] border border-[color-mix(in_srgb,var(--error)_25%,var(--bg-surface))] text-sm leading-relaxed"
                  >
                    {{ reinstallError }}
                  </div>
                </div>
              </div>
              <div class="modal-footer flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface)]">
                <Button variant="secondary" @click="closeReinstallModal">{{ t('common.close') }}</Button>
                <Button
                  v-if="reinstallStatus !== 'success'"
                  variant="primary"
                  :disabled="!!actionLoading || reinstallStatus === 'verifying'"
                  :loading="actionLoading === 'reinstall'"
                  @click="regenerateInstall"
                >
                  {{ t('admin.hosts.nodeGenerateCommand') }}
                </Button>
                <Button
                  v-if="reinstallStatus === 'success'"
                  variant="primary"
                  @click="closeReinstallModal"
                >
                  {{ t('common.done') }}
                </Button>
              </div>
            </div>
          </div>
        </Transition>
      </Teleport>
    </template>

    <!-- 兜底态：既不在 loading、host 又为空时给出明确状态与重试操作 -->
    <Card v-else padding="lg" class="text-center">
      <EmptyState
        :title="t('admin.hosts.loadFailed')"
        :description="t('admin.hosts.invalidId')"
      >
        <template #icon>
          <AlertCircle class="w-7 h-7 text-[var(--error)] stroke-[1.5]" />
        </template>
        <template #action>
          <div class="flex items-center gap-3">
            <Button variant="primary" size="sm" @click="retryLoadHost">
              {{ t('common.retry') }}
            </Button>
            <Button variant="secondary" size="sm" @click="router.replace(hostsPath())">
              {{ t('common.back') }}
            </Button>
          </div>
        </template>
      </EmptyState>
    </Card>
  </div>
</template>
