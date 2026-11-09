<script setup lang="ts">
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  Activity,
  AlertCircle,
  CheckCircle2,
  Clock,
  Globe,
  Info,
  Plus,
  RotateCw,
  Search,
  Server,
  ShieldCheck,
  Sliders,
  Trash2,
  UploadCloud,
  X
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useTabQuery } from '@/composables/useTabQuery'
import { useReveal } from '@/composables/useReveal'
import type { ProxyImportResult, ProxyNode, ProxyProbeResponse, ProxyProbeResult } from '@/api/egress-types'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Select,
  type SelectOption
} from '@/components/ui'
import DrawerModal from '@/components/DrawerModal.vue'
import EgressProfilesPanel from '@/components/EgressProfilesPanel.vue'

const { t } = useI18n()
const route = useRoute()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const PROBE_URL_STORAGE_KEY = 'egress.probeUrl'
const PROBE_INTERVAL_STORAGE_KEY = 'egress.probeIntervalSeconds'
const DEFAULT_PROBE_URL = 'https://1.1.1.1/cdn-cgi/trace'
const DEFAULT_PROBE_INTERVAL_SECONDS = 30

function storedProbeInterval() {
  const value = Number(localStorage.getItem(PROBE_INTERVAL_STORAGE_KEY))
  return Number.isInteger(value) && value >= 10 && value <= 3600 ? value : DEFAULT_PROBE_INTERVAL_SECONDS
}

const nodes = ref<ProxyNode[]>([])
const selected = ref<number[]>([])
const hosts = ref<Array<{ id: number; name: string }>>([])
const probeResult = ref<ProxyProbeResponse | null>(null)

const content = ref('')
const subscriptionUrl = ref('')
const source = ref('content')
const busy = ref(false)
const error = ref('')
const result = ref<ProxyImportResult | null>(null)

const requestedHost = Number(route.query.hostId)
const probeHost = ref(Number.isSafeInteger(requestedHost) && requestedHost > 0 ? requestedHost : 0)
const probeUrl = ref(localStorage.getItem(PROBE_URL_STORAGE_KEY) || DEFAULT_PROBE_URL)
const probeInterval = ref(storedProbeInterval())
const probeIntervalSeconds = computed(() =>
  Number.isInteger(probeInterval.value) && probeInterval.value >= 10 && probeInterval.value <= 3600
    ? probeInterval.value
    : DEFAULT_PROBE_INTERVAL_SECONDS
)

watch(probeUrl, value => localStorage.setItem(PROBE_URL_STORAGE_KEY, value))
watch(probeIntervalSeconds, value => localStorage.setItem(PROBE_INTERVAL_STORAGE_KEY, String(value)))

const testing = ref(false)
const showImport = ref(false)
const searchQuery = ref('')
const tab = useTabQuery<'nodes' | 'profiles' | 'settings'>({ keys: ['nodes', 'profiles', 'settings'], fallback: 'nodes' })

const selectedHostIdString = computed({
  get: () => (probeHost.value > 0 ? String(probeHost.value) : ''),
  set: (val: string | number | boolean) => {
    probeHost.value = Number(val) || 0
  }
})

const hostOptions = computed<SelectOption[]>(() =>
  hosts.value.map(host => ({
    value: String(host.id),
    label: host.name
  }))
)

const selectedHostName = computed(() => {
  const current = hosts.value.find(h => h.id === probeHost.value)
  return current ? current.name : (hosts.value[0]?.name || '-')
})

const healthyCount = computed(() => {
  if (!probeResult.value?.items?.length) return 0
  return probeResult.value.items.filter(item => item.success).length
})

const tabs = computed(() => [
  {
    key: 'nodes' as const,
    label: t('egress.proxyNodes'),
    icon: Globe,
    count: nodes.value.length
  },
  {
    key: 'profiles' as const,
    label: t('packageEgress.profiles'),
    icon: ShieldCheck
  },
  {
    key: 'settings' as const,
    label: t('egress.settings'),
    icon: Sliders
  }
])

const sourceOptions = computed<SelectOption[]>(() => [
  { value: 'content', label: t('egress.rawOrUri') },
  { value: 'url', label: t('egress.subscriptionUrl') }
])

const filteredNodes = computed(() => {
  const q = searchQuery.value.trim().toLowerCase()
  if (!q) return nodes.value
  return nodes.value.filter(n =>
    n.name.toLowerCase().includes(q) ||
    n.protocol.toLowerCase().includes(q) ||
    n.server.toLowerCase().includes(q) ||
    String(n.port).includes(q)
  )
})

const allSelected = computed(() =>
  filteredNodes.value.length > 0 &&
  filteredNodes.value.every(node => selected.value.includes(node.id))
)

const isIndeterminate = computed(() => {
  const selectedInCurrent = filteredNodes.value.filter(node => selected.value.includes(node.id)).length
  return selectedInCurrent > 0 && selectedInCurrent < filteredNodes.value.length
})

const latency = computed(() => new Map(probeResult.value?.items.map(item => [item.nodeId, item]) ?? []))

function toggleAll() {
  if (allSelected.value) {
    const currentIds = new Set(filteredNodes.value.map(n => n.id))
    selected.value = selected.value.filter(id => !currentIds.has(id))
  } else {
    const set = new Set(selected.value)
    filteredNodes.value.forEach(node => set.add(node.id))
    selected.value = Array.from(set)
  }
}

function protocolVariant(protocol: string): 'default' | 'info' | 'success' | 'warning' | 'error' {
  switch (protocol.toLowerCase()) {
    case 'vless':
    case 'vmess':
      return 'info'
    case 'trojan':
    case 'hysteria2':
      return 'warning'
    case 'ss':
    case 'shadowsocks':
      return 'success'
    case 'socks':
    case 'socks5':
    default:
      return 'default'
  }
}

let generation = 0
let active = false
let probeController: AbortController | undefined

async function load() {
  const current = generation
  try {
    const [n, h] = await Promise.all([api.network.nodes(), api.network.probeHosts()])
    if (current !== generation) return
    nodes.value = n.nodes
    hosts.value = h.hosts
    if (!hosts.value.some(host => host.id === probeHost.value)) {
      probeHost.value = hosts.value[0]?.id || 0
    }
    selected.value = selected.value.filter(id => n.nodes.some(node => node.id === id))
  } catch (e: any) {
    if (current === generation) error.value = e.message
  }
}

async function importNodes(confirm: boolean) {
  busy.value = true
  error.value = ''
  try {
    result.value = await api.network.importNodes({
      ...(source.value === 'url' ? { subscriptionUrl: subscriptionUrl.value } : { content: content.value }),
      confirm
    })
    if (confirm) {
      showImport.value = false
      content.value = ''
      subscriptionUrl.value = ''
      await load()
    }
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function upload(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0]
  if (!file) return
  if (file.size > 512 * 1024) {
    error.value = t('egress.fileTooLarge')
    return
  }
  content.value = await file.text()
  result.value = null
}

async function remove(id: number) {
  busy.value = true
  error.value = ''
  try {
    await api.network.deleteNode(id)
    await load()
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function probe() {
  const current = generation
  const controller = new AbortController()
  probeController = controller
  testing.value = true
  error.value = ''
  probeResult.value = null
  try {
    const ids = [...selected.value]
    const hostId = probeHost.value
    const url = probeUrl.value
    const items: ProxyProbeResult[] = []
    for (let index = 0; index < ids.length; index += 10) {
      const response = await api.network.probe(hostId, ids.slice(index, index + 10), url, controller.signal)
      if (current !== generation) return
      items.push(...response.items)
      probeResult.value = { host: response.host, items: [...items] }
    }
  } catch (e: any) {
    if (current === generation) error.value = e.message
  } finally {
    if (current === generation) {
      testing.value = false
      probeController = undefined
    }
  }
}

watch([content, subscriptionUrl, source], () => {
  result.value = null
})

watch(probeHost, () => {
  if (testing.value) {
    generation++
    probeController?.abort()
    probeController = undefined
    testing.value = false
  }
  probeResult.value = null
})

function selectRequestedHost() {
  const id = Number(route.query.hostId)
  if (Number.isSafeInteger(id) && id > 0) probeHost.value = id
}

function activate() {
  if (active) return
  active = true
  selectRequestedHost()
  void load()
}

function deactivate() {
  active = false
  generation++
  probeController?.abort()
  probeController = undefined
  testing.value = false
  probeResult.value = null
}

watch(() => route.query.hostId, () => {
  if (active) selectRequestedHost()
})

onMounted(activate)
onActivated(activate)
onDeactivated(deactivate)
onUnmounted(deactivate)
</script>

<template>
  <div ref="revealRoot" class="space-y-6">
    <!-- Apple HIG PageHeader -->
    <PageHeader
      data-reveal
      :title="t('egress.pageTitle')"
      :description="t('egress.pageDescription')"
    >
      <template #actions>
        <Button
          v-if="tab === 'nodes'"
          variant="secondary"
          size="sm"
          :disabled="busy || testing"
          @click="load"
        >
          <template #icon>
            <RotateCw class="w-3.5 h-3.5" :class="{ 'animate-spin': busy }" />
          </template>
          {{ t('common.refresh') }}
        </Button>
        <Button
          v-if="tab === 'nodes'"
          variant="primary"
          size="sm"
          :disabled="busy || testing"
          @click="showImport = true"
        >
          <template #icon>
            <Plus class="w-3.5 h-3.5 stroke-[2.5]" />
          </template>
          {{ t('egress.importNodes') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Apple 风格分段控制器 (Segmented Tab Control) -->
    <div
      data-reveal
      class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full"
    >
      <button
        v-for="tItem in tabs"
        :key="tItem.key"
        type="button"
        role="tab"
        :aria-selected="tab === tItem.key"
        class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none cursor-pointer"
        :class="tab === tItem.key
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="tab = tItem.key"
      >
        <component :is="tItem.icon" class="w-4 h-4 stroke-[1.8]" />
        <span>{{ tItem.label }}</span>
        <Badge
          v-if="tItem.count !== undefined"
          size="sm"
          :variant="tab === tItem.key ? 'info' : 'default'"
        >
          {{ tItem.count }}
        </Badge>
      </button>
    </div>

    <!-- 顶部错误通知横幅 -->
    <div
      v-if="error && !showImport"
      data-reveal
      role="alert"
      class="flex items-center justify-between gap-3 p-4 rounded-2xl bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_25%,var(--bg-surface))] text-[var(--error)] text-sm"
    >
      <div class="flex items-center gap-2.5 min-w-0">
        <AlertCircle class="w-4 h-4 shrink-0 stroke-[2]" />
        <span class="break-words font-medium">{{ error }}</span>
      </div>
      <button
        type="button"
        class="text-[var(--error)] hover:opacity-75 transition-opacity shrink-0 p-1"
        @click="error = ''"
      >
        <X class="w-4 h-4" />
      </button>
    </div>

    <!-- Tab 1: 代理节点列表 -->
    <template v-if="tab === 'nodes'">
      <!-- 概览指标卡片 (Apple Stat Cards) -->
      <div data-reveal class="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <Card hoverable padding="sm" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              {{ t('egress.proxyNodes') }}
            </span>
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
              <Globe class="w-4 h-4" />
            </div>
          </div>
          <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
            {{ nodes.length }}
          </div>
        </Card>

        <Card hoverable padding="sm" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              已选待测
            </span>
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
              <CheckCircle2 class="w-4 h-4" />
            </div>
          </div>
          <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--accent)]">
            {{ selected.length }}
          </div>
        </Card>

        <Card hoverable padding="sm" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              {{ t('egress.probeResults') }}
            </span>
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
              <Activity class="w-4 h-4" />
            </div>
          </div>
          <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--success)]">
            {{ healthyCount }}
          </div>
        </Card>

        <Card hoverable padding="sm" class="group">
          <div class="flex items-center justify-between">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] truncate">
              {{ t('egress.probeHost') }}
            </span>
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)] group-hover:scale-105 transition-transform duration-300">
              <Server class="w-4 h-4" />
            </div>
          </div>
          <div class="mt-3 text-base lg:text-lg font-semibold tracking-tight truncate text-[var(--text-primary)]">
            {{ selectedHostName }}
          </div>
        </Card>
      </div>

      <!-- 操作与过滤工具栏 -->
      <Card data-reveal padding="sm">
        <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
          <div class="flex flex-col gap-3 sm:flex-row sm:items-center flex-1 min-w-0">
            <!-- 搜索框 -->
            <div class="w-full sm:w-64">
              <Input
                v-model="searchQuery"
                size="sm"
                placeholder="搜索名称 / 协议 / 服务器..."
                clearable
              >
                <template #prefix>
                  <Search class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                </template>
              </Input>
            </div>

            <!-- 宿主机选择 -->
            <div class="w-full sm:w-56 flex items-center gap-2">
              <Select
                v-model="selectedHostIdString"
                :options="hostOptions"
                :disabled="testing"
                size="sm"
                placeholder="选择检测宿主机"
              />
            </div>
          </div>

          <!-- 检测按钮与操作 -->
          <div class="flex items-center gap-2.5 flex-wrap shrink-0">
            <Button
              variant="secondary"
              size="sm"
              :disabled="busy || testing || !selected.length || !probeHost"
              :loading="testing"
              @click="probe"
            >
              <template #icon>
                <Activity class="w-3.5 h-3.5 text-[var(--accent)]" />
              </template>
              {{ t(testing ? 'egress.testing' : 'egress.testSelected') }}
            </Button>
            <Button
              variant="primary"
              size="sm"
              :disabled="busy || testing"
              @click="showImport = true"
            >
              <template #icon>
                <Plus class="w-3.5 h-3.5 stroke-[2.5]" />
              </template>
              {{ t('egress.importNodes') }}
            </Button>
          </div>
        </div>
      </Card>

      <!-- 节点数据容器 -->
      <div data-reveal class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] overflow-hidden">
        <!-- 空状态 -->
        <div v-if="!nodes.length" class="py-12">
          <EmptyState
            :title="t('egress.noNodes')"
            :description="t('egress.pageDescription')"
          >
            <template #action>
              <Button
                variant="primary"
                size="sm"
                :disabled="busy || testing"
                @click="showImport = true"
              >
                <template #icon>
                  <Plus class="w-3.5 h-3.5 stroke-[2.5]" />
                </template>
                {{ t('egress.importNodes') }}
              </Button>
            </template>
          </EmptyState>
        </div>

        <template v-else>
          <!-- 移动端响应式卡片 (lg:hidden) -->
          <div class="space-y-3 p-4 lg:hidden">
            <div class="flex items-center justify-between pb-2 border-b border-[var(--border-color)] text-xs text-[var(--text-secondary)]">
              <label class="flex items-center gap-2 cursor-pointer select-none">
                <input
                  type="checkbox"
                  class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                  :checked="allSelected"
                  :disabled="testing"
                  @change="toggleAll"
                />
                <span>{{ t('packageEgress.selectAll') }} ({{ selected.length }}/{{ filteredNodes.length }})</span>
              </label>
            </div>

            <div
              v-for="node in filteredNodes"
              :key="node.id"
              class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-2.5 transition-colors duration-150"
            >
              <div class="flex items-start justify-between gap-2">
                <div class="flex items-center gap-2.5 min-w-0">
                  <input
                    v-model="selected"
                    type="checkbox"
                    :disabled="testing"
                    :value="node.id"
                    :aria-label="node.name"
                    class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] shrink-0"
                  />
                  <div class="min-w-0 truncate">
                    <div class="font-medium text-sm text-[var(--text-primary)] truncate">
                      {{ node.name }}
                    </div>
                  </div>
                </div>
                <Badge :variant="protocolVariant(node.protocol)" size="sm" class="shrink-0">
                  {{ node.protocol }}
                </Badge>
              </div>

              <div class="font-mono text-xs text-[var(--text-secondary)] break-all pl-6">
                {{ node.server }}:{{ node.port }}
              </div>

              <div class="flex items-center justify-between border-t border-[var(--border-color)] pt-2.5 pl-6">
                <div>
                  <Badge
                    v-if="testing && selected.includes(node.id) && !latency.has(node.id)"
                    variant="warning"
                    size="sm"
                    pulse
                    dot
                  >
                    {{ t('egress.testing') }}
                  </Badge>
                  <template v-else-if="latency.has(node.id)">
                    <Badge
                      v-if="latency.get(node.id)!.success"
                      variant="success"
                      size="sm"
                      dot
                    >
                      {{ latency.get(node.id)!.latencyMs }} ms
                    </Badge>
                    <Badge
                      v-else
                      variant="error"
                      size="sm"
                      dot
                      :title="latency.get(node.id)?.errorDetail"
                    >
                      {{ latency.get(node.id)!.error || 'Failed' }}
                    </Badge>
                  </template>
                  <span v-else class="text-xs text-[var(--text-tertiary)] font-mono">—</span>
                </div>

                <Button
                  variant="ghost"
                  size="sm"
                  class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] !p-1.5 h-auto min-h-0"
                  :disabled="busy || testing"
                  :title="t('common.delete')"
                  @click="remove(node.id)"
                >
                  <Trash2 class="w-3.5 h-3.5" />
                </Button>
              </div>
            </div>
          </div>

          <!-- PC 端定宽表格 (hidden lg:block) -->
          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed text-sm border-collapse">
              <colgroup>
                <col class="w-[5%]" />
                <col class="w-[24%]" />
                <col class="w-[14%]" />
                <col class="w-[26%]" />
                <col class="w-[18%]" />
                <col class="w-[13%]" />
              </colgroup>
              <thead class="bg-[var(--bg-surface-soft)] text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] select-none">
                <tr class="border-b border-[var(--border-color)]">
                  <th scope="col" class="px-4 py-3 text-center">
                    <input
                      type="checkbox"
                      :checked="allSelected"
                      :indeterminate="isIndeterminate"
                      :disabled="testing"
                      :aria-label="t('packageEgress.selectAll')"
                      class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
                      @change="toggleAll"
                    />
                  </th>
                  <th scope="col" class="px-4 py-3 text-left">
                    {{ t('packageEgress.name') }}
                  </th>
                  <th scope="col" class="px-4 py-3 text-left">
                    {{ t('egress.protocol') }}
                  </th>
                  <th scope="col" class="px-4 py-3 text-left">
                    {{ t('packageEgress.endpoint') }}
                  </th>
                  <th scope="col" class="px-4 py-3 text-left">
                    {{ t('packageEgress.latency') }}
                  </th>
                  <th scope="col" class="px-4 py-3 text-right">
                    {{ t('common.actions') }}
                  </th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr
                  v-for="node in filteredNodes"
                  :key="node.id"
                  class="hover:bg-[var(--bg-surface-soft)] transition-colors duration-150"
                >
                  <td class="px-4 py-3.5 text-center">
                    <input
                      v-model="selected"
                      type="checkbox"
                      :disabled="testing"
                      :value="node.id"
                      :aria-label="node.name"
                      class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
                    />
                  </td>
                  <td class="px-4 py-3.5 break-words font-medium text-[var(--text-primary)] pr-2">
                    {{ node.name }}
                  </td>
                  <td class="px-4 py-3.5 pr-2">
                    <Badge :variant="protocolVariant(node.protocol)" size="sm">
                      {{ node.protocol }}
                    </Badge>
                  </td>
                  <td class="px-4 py-3.5 font-mono text-xs text-[var(--text-secondary)] break-all pr-2">
                    {{ node.server }}:{{ node.port }}
                  </td>
                  <td class="px-4 py-3.5 pr-2" aria-live="polite">
                    <Badge
                      v-if="testing && selected.includes(node.id) && !latency.has(node.id)"
                      variant="warning"
                      size="sm"
                      pulse
                      dot
                    >
                      {{ t('egress.testing') }}
                    </Badge>
                    <template v-else-if="latency.has(node.id)">
                      <Badge
                        v-if="latency.get(node.id)!.success"
                        variant="success"
                        size="sm"
                        dot
                      >
                        {{ latency.get(node.id)!.latencyMs }} ms
                      </Badge>
                      <Badge
                        v-else
                        variant="error"
                        size="sm"
                        dot
                        :title="latency.get(node.id)?.errorDetail"
                      >
                        {{ latency.get(node.id)!.error || 'Failed' }}
                      </Badge>
                    </template>
                    <span v-else class="text-xs text-[var(--text-tertiary)] font-mono select-none">—</span>
                  </td>
                  <td class="px-4 py-3.5 text-right">
                    <div class="flex items-center justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] !p-1.5 h-auto min-h-0"
                        :disabled="busy || testing"
                        :title="t('common.delete')"
                        @click="remove(node.id)"
                      >
                        <Trash2 class="w-3.5 h-3.5" />
                      </Button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 表格底部提示与说明 -->
          <div class="px-4 py-3 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between text-xs text-[var(--text-tertiary)]">
            <div class="flex items-center gap-1.5">
              <Info class="w-3.5 h-3.5 shrink-0" />
              <span>{{ t('egress.probeHint') }}</span>
            </div>
            <div class="font-mono">
              Total: {{ nodes.length }}
            </div>
          </div>
        </template>
      </div>
    </template>

    <!-- Tab 2: 出口策略面板 -->
    <EgressProfilesPanel
      v-else-if="tab === 'profiles'"
      data-reveal
      :nodes="nodes"
      :probe-url="probeUrl"
      :probe-interval-seconds="probeIntervalSeconds"
    />

    <!-- Tab 3: 检测与参数设置 -->
    <Card v-else-if="tab === 'settings'" data-reveal padding="md" class="space-y-6">
      <template #header>
        <div class="flex items-center gap-2.5">
          <div class="flex h-8 w-8 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
            <Sliders class="w-4 h-4" />
          </div>
          <div>
            <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('egress.settings') }}
            </h2>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ t('egress.pageDescription') }}
            </p>
          </div>
        </div>
      </template>

      <div class="space-y-5 max-w-2xl">
        <!-- Probe URL -->
        <Input
          v-model="probeUrl"
          :label="t('egress.probeUrl')"
          :hint="t('egress.probeUrlHint')"
          :disabled="testing"
          type="url"
          placeholder="https://1.1.1.1/cdn-cgi/trace"
        >
          <template #prefix>
            <Globe class="w-4 h-4 text-[var(--text-tertiary)]" />
          </template>
        </Input>

        <!-- Probe Interval -->
        <Input
          v-model.number="probeInterval"
          :label="t('egress.probeInterval')"
          :hint="t('egress.probeSettingsHint')"
          type="number"
          min="10"
          max="3600"
          placeholder="30"
        >
          <template #prefix>
            <Clock class="w-4 h-4 text-[var(--text-tertiary)]" />
          </template>
        </Input>

        <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-start gap-2.5 text-xs text-[var(--text-secondary)] leading-relaxed">
          <Info class="w-4 h-4 text-[var(--accent)] shrink-0 mt-0.5" />
          <div>
            探测参数修改后会自动同步至本地存储，并作为新创建或同步出口策略的默认检测基线。
          </div>
        </div>
      </div>
    </Card>

    <!-- 导入节点抽屉 (DrawerModal) -->
    <DrawerModal v-model="showImport" :title="t('egress.importNodes')">
      <div class="space-y-4">
        <!-- 抽屉内错误反馈 -->
        <div
          v-if="error"
          role="alert"
          class="flex items-center gap-2 p-3 rounded-xl bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_25%,var(--bg-surface))] text-[var(--error)] text-xs"
        >
          <AlertCircle class="w-4 h-4 shrink-0" />
          <span class="break-words font-medium">{{ error }}</span>
        </div>

        <!-- 导入来源选择 -->
        <div class="space-y-1.5">
          <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
            {{ t('egress.importNodes') }}
          </label>
          <Select
            v-model="source"
            :options="sourceOptions"
            size="sm"
          />
        </div>

        <!-- 订阅地址输入 -->
        <div v-if="source === 'url'" class="space-y-1.5">
          <Input
            v-model="subscriptionUrl"
            type="url"
            :label="t('egress.subscriptionUrl')"
            placeholder="https://example.com/subscription"
            :disabled="busy"
          />
        </div>

        <!-- 配置文件或分享链接 -->
        <template v-else>
          <div class="space-y-1.5">
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
              {{ t('egress.upload') }}
            </label>
            <div class="relative flex items-center justify-center p-4 rounded-xl border border-dashed border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:border-[var(--border-hover)] transition-all cursor-pointer">
              <input
                type="file"
                accept=".json,.txt,application/json,text/plain"
                class="absolute inset-0 opacity-0 cursor-pointer w-full h-full"
                :aria-label="t('egress.upload')"
                :disabled="busy"
                @change="upload"
              />
              <div class="flex items-center gap-2 text-xs text-[var(--text-secondary)]">
                <UploadCloud class="w-4 h-4 text-[var(--accent)]" />
                <span>{{ t('egress.upload') }} (.json, .txt)</span>
              </div>
            </div>
          </div>

          <div class="space-y-1.5">
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
              {{ t('egress.rawOrUri') }}
            </label>
            <textarea
              v-model="content"
              rows="6"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-primary)] p-3 font-mono text-xs focus:ring-2 focus:ring-[var(--accent)] focus:border-[var(--accent)] focus:outline-none transition-all placeholder-[var(--text-tertiary)]"
              :aria-label="t('egress.rawOrUri')"
              :placeholder="t('egress.importHint')"
              :disabled="busy"
            />
          </div>
        </template>

        <!-- 解析结果预览区 -->
        <div
          v-if="result"
          class="space-y-3 p-3.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]"
        >
          <div class="flex items-center justify-between text-xs font-semibold text-[var(--text-primary)]">
            <span class="flex items-center gap-1.5">
              <CheckCircle2 class="w-3.5 h-3.5 text-[var(--success)]" />
              {{ t('egress.imported') }}: {{ result.nodes?.length || 0 }}
            </span>
            <span v-if="result.errors?.length" class="text-[var(--error)] flex items-center gap-1">
              <AlertCircle class="w-3.5 h-3.5" />
              {{ result.errors.length }} 个解析异常
            </span>
          </div>

          <div v-if="result.nodes && result.nodes.length > 0" class="max-h-44 overflow-y-auto space-y-1.5 pr-1">
            <div
              v-for="(n, idx) in result.nodes"
              :key="idx"
              class="flex items-center justify-between gap-2 p-2 rounded-lg bg-[var(--bg-surface)] text-xs border border-[var(--border-color)]"
            >
              <div class="min-w-0 flex items-center gap-2 truncate">
                <span class="font-medium text-[var(--text-primary)] truncate">{{ n.name }}</span>
                <Badge size="sm" :variant="protocolVariant(n.protocol)">{{ n.protocol }}</Badge>
              </div>
              <span class="font-mono text-[11px] text-[var(--text-secondary)] shrink-0">{{ n.server }}:{{ n.port }}</span>
            </div>
          </div>

          <div v-if="result.errors?.length" class="max-h-28 overflow-y-auto space-y-1 pr-1 text-xs text-[var(--error)]">
            <p v-for="item in result.errors" :key="item.index" class="break-all font-mono text-[11px]">
              #{{ item.index }}: {{ item.error }}
            </p>
          </div>
        </div>

        <!-- 抽屉底部操作 -->
        <div class="flex items-center justify-end gap-3 pt-2">
          <Button
            variant="secondary"
            size="sm"
            :disabled="busy || (!content.trim() && !subscriptionUrl.trim())"
            :loading="busy"
            @click="importNodes(false)"
          >
            {{ t('egress.preview') }}
          </Button>
          <Button
            variant="primary"
            size="sm"
            :disabled="busy || !result?.nodes?.length"
            :loading="busy"
            @click="importNodes(true)"
          >
            {{ t('egress.confirmImport') }}
          </Button>
        </div>
      </div>
    </DrawerModal>
  </div>
</template>
