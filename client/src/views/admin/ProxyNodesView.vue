<script setup lang="ts">
import DrawerModal from '@/components/DrawerModal.vue'
import EgressProfilesPanel from '@/components/EgressProfilesPanel.vue'
import IconButton from '@/components/IconButton.vue'
import { Trash2 } from 'lucide-vue-next'
import { computed, ref, watch, onMounted, onUnmounted, onActivated, onDeactivated } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import { useTabQuery } from '@/composables/useTabQuery'
import type { ProxyNode, ProxyImportResult, ProxyProbeResponse, ProxyProbeResult } from '@/api/egress-types'
const { t } = useI18n()
const route = useRoute()
const PROBE_URL_STORAGE_KEY = 'egress.probeUrl'
const PROBE_INTERVAL_STORAGE_KEY = 'egress.probeIntervalSeconds'
const DEFAULT_PROBE_URL = 'https://1.1.1.1/cdn-cgi/trace'
const DEFAULT_PROBE_INTERVAL_SECONDS = 30
function storedProbeInterval() {
  const value = Number(localStorage.getItem(PROBE_INTERVAL_STORAGE_KEY))
  return Number.isInteger(value) && value >= 10 && value <= 3600 ? value : DEFAULT_PROBE_INTERVAL_SECONDS
}
const nodes = ref<ProxyNode[]>([]), selected = ref<number[]>([]), hosts = ref<Array<{ id: number; name: string }>>([]), probeResult = ref<ProxyProbeResponse | null>(null)
const content = ref(''), subscriptionUrl = ref(''), source = ref('content'), busy = ref(false), error = ref(''), result = ref<ProxyImportResult | null>(null)
const requestedHost = Number(route.query.hostId)
const probeHost = ref(Number.isSafeInteger(requestedHost) && requestedHost > 0 ? requestedHost : 0)
const probeUrl = ref(localStorage.getItem(PROBE_URL_STORAGE_KEY) || DEFAULT_PROBE_URL)
const probeInterval = ref(storedProbeInterval())
const probeIntervalSeconds = computed(() => Number.isInteger(probeInterval.value) && probeInterval.value >= 10 && probeInterval.value <= 3600 ? probeInterval.value : DEFAULT_PROBE_INTERVAL_SECONDS)
watch(probeUrl, value => localStorage.setItem(PROBE_URL_STORAGE_KEY, value))
watch(probeIntervalSeconds, value => localStorage.setItem(PROBE_INTERVAL_STORAGE_KEY, String(value)))
const testing = ref(false), showImport = ref(false)
const tab = useTabQuery<'nodes' | 'profiles' | 'settings'>({ keys: ['nodes', 'profiles', 'settings'], fallback: 'nodes' })
const allSelected = computed(() => nodes.value.length > 0 && nodes.value.every(node => selected.value.includes(node.id)))
const latency = computed(() => new Map(probeResult.value?.items.map(item => [item.nodeId, item]) ?? []))
function toggleAll() { selected.value = allSelected.value ? [] : nodes.value.map(node => node.id) }
let generation = 0
let active = false
let probeController: AbortController | undefined
async function load() {
  const current = generation
  try {
    const [n, h] = await Promise.all([api.network.nodes(), api.network.probeHosts()])
    if (current !== generation) return
    nodes.value = n.nodes; hosts.value = h.hosts
    if (!hosts.value.some(host => host.id === probeHost.value)) probeHost.value = hosts.value[0]?.id || 0
    selected.value = selected.value.filter(id => n.nodes.some(node => node.id === id))
  } catch (e: any) { if (current === generation) error.value = e.message }
}
async function importNodes(confirm: boolean) {
  busy.value = true; error.value = ''
  try {
    result.value = await api.network.importNodes({ ...(source.value === 'url' ? { subscriptionUrl: subscriptionUrl.value } : { content: content.value }), confirm })
    if (confirm) { showImport.value = false; content.value = ''; subscriptionUrl.value = ''; await load() }
  } catch (e: any) { error.value = e.message }
  finally { busy.value = false }
}
async function upload(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0]
  if (!file) return
  if (file.size > 512 * 1024) { error.value = t('egress.fileTooLarge'); return }
  content.value = await file.text(); result.value = null
}
async function remove(id: number) {
  busy.value = true; error.value = ''
  try { await api.network.deleteNode(id); await load() } catch (e: any) { error.value = e.message }
  finally { busy.value = false }
}
async function probe() {
  const current = generation
  const controller = new AbortController()
  probeController = controller
  testing.value = true; error.value = ''; probeResult.value = null
  try {
    const ids = [...selected.value], hostId = probeHost.value, url = probeUrl.value
    const items: ProxyProbeResult[] = []
    for (let index = 0; index < ids.length; index += 10) {
      const response = await api.network.probe(hostId, ids.slice(index, index + 10), url, controller.signal)
      if (current !== generation) return
      items.push(...response.items)
      probeResult.value = { host: response.host, items: [...items] }
    }
  } catch (e: any) { if (current === generation) error.value = e.message }
  finally { if (current === generation) { testing.value = false; probeController = undefined } }
}
watch([content, subscriptionUrl, source], () => { result.value = null })
watch(probeHost, () => {
  if (testing.value) { generation++; probeController?.abort(); probeController = undefined; testing.value = false }
  probeResult.value = null
})
function selectRequestedHost() {
  const id = Number(route.query.hostId)
  if (Number.isSafeInteger(id) && id > 0) probeHost.value = id
}
function activate() {
  if (active) return
  active = true; selectRequestedHost(); void load()
}
function deactivate() {
  active = false; generation++
  probeController?.abort(); probeController = undefined; testing.value = false; probeResult.value = null
}
watch(() => route.query.hostId, () => { if (active) selectRequestedHost() })
onMounted(activate)
onActivated(activate)
onDeactivated(deactivate)
onUnmounted(deactivate)
</script>
<template>
  <div class="space-y-6">
    <header><h1 class="text-2xl font-semibold text-themed">{{ t('egress.pageTitle') }}</h1><p class="mt-1 text-sm text-themed-muted">{{ t('egress.pageDescription') }}</p></header>
    <div class="border-b border-themed overflow-x-auto">
      <nav class="flex flex-nowrap gap-1 sm:gap-2 min-w-max" role="tablist">
        <button type="button" role="tab" :aria-selected="tab === 'nodes'" :class="['-mb-px flex-shrink-0 whitespace-nowrap border-b-2 px-4 py-2.5 text-sm font-medium transition-colors', tab === 'nodes' ? 'border-primary-600 text-primary-600 dark:border-primary-400 dark:text-primary-400' : 'border-transparent text-themed-muted hover:text-themed']" @click="tab = 'nodes'">{{ t('egress.proxyNodes') }}</button>
        <button type="button" role="tab" :aria-selected="tab === 'profiles'" :class="['-mb-px flex-shrink-0 whitespace-nowrap border-b-2 px-4 py-2.5 text-sm font-medium transition-colors', tab === 'profiles' ? 'border-primary-600 text-primary-600 dark:border-primary-400 dark:text-primary-400' : 'border-transparent text-themed-muted hover:text-themed']" @click="tab = 'profiles'">{{ t('packageEgress.profiles') }}</button>
        <button type="button" role="tab" :aria-selected="tab === 'settings'" :class="['-mb-px flex-shrink-0 whitespace-nowrap border-b-2 px-4 py-2.5 text-sm font-medium transition-colors', tab === 'settings' ? 'border-primary-600 text-primary-600 dark:border-primary-400 dark:text-primary-400' : 'border-transparent text-themed-muted hover:text-themed']" @click="tab = 'settings'">{{ t('egress.settings') }}</button>
      </nav>
    </div>
    <p v-if="error && !showImport" class="text-error break-words" role="alert">{{ error }}</p>
    <EgressProfilesPanel v-if="tab === 'profiles'" :nodes="nodes" :probe-url="probeUrl" :probe-interval-seconds="probeIntervalSeconds" />
    <section v-else-if="tab === 'settings'" class="card space-y-4 p-5 sm:p-6">
      <h2 class="font-semibold text-themed">{{ t('egress.settings') }}</h2>
      <label class="flex flex-wrap items-center gap-3 text-sm text-themed-secondary">{{ t('egress.probeUrl') }}<input v-model="probeUrl" :disabled="testing" class="input flex-1" type="url" /></label>
      <p class="text-xs text-themed-muted">{{ t('egress.probeUrlHint') }}</p>
      <label class="flex flex-wrap items-center gap-3 text-sm text-themed-secondary">{{ t('egress.probeInterval') }}<input v-model.number="probeInterval" class="input w-40" type="number" min="10" max="3600" /></label>
      <p class="text-xs text-themed-muted">{{ t('egress.probeSettingsHint') }}</p>
    </section>
    <section v-else class="card space-y-4 p-5 sm:p-6">
      <div class="flex flex-wrap items-center gap-3">
        <h2 class="mr-auto font-semibold text-themed">{{ t('egress.proxyNodes') }}</h2>
        <label class="flex items-center gap-2 text-sm text-themed">{{ t('egress.probeHost') }}<select v-model="probeHost" :disabled="testing" class="input w-auto"><option v-for="host in hosts" :key="host.id" :value="host.id">{{ host.name }}</option></select></label>
        <button class="btn-secondary" :disabled="busy || testing || !selected.length || !probeHost" @click="probe">{{ t(testing ? 'egress.testing' : 'egress.testSelected') }}</button>
        <button class="btn-primary" :disabled="busy || testing" @click="showImport = true">{{ t('egress.importNodes') }}</button>
      </div>
      <p v-if="!nodes.length" class="text-sm text-themed-muted">{{ t('egress.noNodes') }}</p>
      <div v-else class="overflow-hidden"><table class="w-full table-fixed text-sm"><thead><tr class="border-b border-themed text-left"><th class="w-[6%] py-3"><input type="checkbox" :checked="allSelected" :disabled="testing" :aria-label="t('packageEgress.selectAll')" @change="toggleAll" /></th><th class="w-[25%]">{{ t('packageEgress.name') }}</th><th class="w-[16%]">{{ t('egress.protocol') }}</th><th class="w-[28%]">{{ t('packageEgress.endpoint') }}</th><th class="w-[15%]">{{ t('packageEgress.latency') }}</th><th class="w-[10%] text-right">{{ t('common.actions') }}</th></tr></thead><tbody><tr v-for="node in nodes" :key="node.id" class="border-b border-themed"><td class="py-4"><input v-model="selected" type="checkbox" :disabled="testing" :value="node.id" :aria-label="node.name" /></td><td class="break-words font-medium pr-2">{{ node.name }}</td><td class="break-words pr-2">{{ node.protocol }}</td><td class="break-all pr-2">{{ node.server }}:{{ node.port }}</td><td class="break-words pr-2" aria-live="polite"><span v-if="testing && selected.includes(node.id) && !latency.has(node.id)" class="text-themed-muted">{{ t('egress.testing') }}</span><span v-else-if="latency.has(node.id)" :class="latency.get(node.id)!.success ? 'text-success' : 'text-error'" :title="latency.get(node.id)?.errorDetail">{{ latency.get(node.id)!.success ? `${latency.get(node.id)!.latencyMs} ms` : latency.get(node.id)!.error }}</span><span v-else class="text-themed-muted">—</span></td><td class="text-right"><div class="flex items-center justify-end gap-1"><IconButton :label="t('common.delete')" variant="danger" :disabled="busy || testing" @click="remove(node.id)"><Trash2 /></IconButton></div></td></tr></tbody></table></div>
      <p class="text-xs text-themed-muted">{{ t('egress.probeHint') }}</p>
    </section>
    <DrawerModal v-model="showImport" :title="t('egress.importNodes')">
      <div class="space-y-4">
        <p v-if="error" class="text-error" role="alert">{{ error }}</p>
        <select v-model="source" class="input" :aria-label="t('egress.importNodes')"><option value="content">{{ t('egress.rawOrUri') }}</option><option value="url">{{ t('egress.subscriptionUrl') }}</option></select>
        <input v-if="source === 'url'" v-model="subscriptionUrl" type="url" class="input" :aria-label="t('egress.subscriptionUrl')" placeholder="https://example.com/subscription" />
        <template v-else><input type="file" accept=".json,.txt,application/json,text/plain" :aria-label="t('egress.upload')" @change="upload" /><textarea v-model="content" class="input font-mono text-xs" rows="7" :aria-label="t('egress.rawOrUri')" :placeholder="t('egress.importHint')" /></template>
        <div v-if="result" class="space-y-2 text-sm"><p v-for="(node, index) in result.nodes" :key="index" class="break-all text-themed-secondary">{{ node.name }} · {{ node.protocol }} · {{ node.server }}:{{ node.port }}</p><p v-for="item in result.errors" :key="item.index" class="text-error">{{ item.index }}: {{ item.error }}</p></div>
        <div class="flex flex-wrap justify-end gap-3"><button class="btn-secondary" :disabled="busy" @click="importNodes(false)">{{ t('egress.preview') }}</button><button class="btn-primary" :disabled="busy || !result?.nodes?.length" @click="importNodes(true)">{{ t('egress.confirmImport') }}</button></div>
      </div>
    </DrawerModal>
  </div>
</template>
