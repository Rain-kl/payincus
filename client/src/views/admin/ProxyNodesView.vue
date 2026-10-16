<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted, onActivated, onDeactivated } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import type { ProxyNode, ProxyImportResult, ProbeJob } from '@/api/egress-types'
const { t } = useI18n()
const route = useRoute()
const nodes = ref<ProxyNode[]>([]), selected = ref<number[]>([]), hosts = ref<Array<{ id: number; name: string }>>([]), jobs = ref<ProbeJob[]>([])
const content = ref(''), subscriptionUrl = ref(''), source = ref('content'), busy = ref(false), error = ref(''), result = ref<ProxyImportResult | null>(null)
const requestedHost = Number(route.query.hostId)
const probeHost = ref(Number.isSafeInteger(requestedHost) && requestedHost > 0 ? requestedHost : 0)
const probeUrl = ref('https://www.gstatic.com/generate_204')
const refreshing = ref(false), refreshedAt = ref(''), refreshError = ref('')
let generation = 0
let active = false
let timer: ReturnType<typeof setInterval> | undefined
async function refreshJobs() {
  if (!active || refreshing.value) return
  const current = generation
  refreshing.value = true
  try {
    const response = await api.network.probes()
    if (current !== generation) return
    jobs.value = response.jobs; refreshedAt.value = new Date().toLocaleTimeString(); refreshError.value = ''
  } catch (e: any) { if (current === generation) refreshError.value = e.message }
  finally { if (current === generation) refreshing.value = false }
}
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
    if (confirm) { content.value = ''; subscriptionUrl.value = ''; await load() }
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
  busy.value = true; error.value = ''
  try { await api.network.probe(probeHost.value, selected.value, probeUrl.value); await refreshJobs() } catch (e: any) { error.value = e.message }
  finally { busy.value = false }
}
watch([content, subscriptionUrl, source], () => { result.value = null })
function selectRequestedHost() {
  const id = Number(route.query.hostId)
  if (Number.isSafeInteger(id) && id > 0) probeHost.value = id
}
function startPolling() {
  if (active) return
  active = true; selectRequestedHost(); void load(); void refreshJobs()
  timer = setInterval(() => { void refreshJobs() }, 3_000)
}
function stopPolling() {
  active = false; generation++; refreshing.value = false
  if (timer) clearInterval(timer)
  timer = undefined
}
watch(() => route.query.hostId, () => { if (active) selectRequestedHost() })
onMounted(startPolling)
onActivated(startPolling)
onDeactivated(stopPolling)
onUnmounted(stopPolling)
</script>
<template>
  <div class="space-y-6">
    <header>
      <h1 class="text-2xl font-semibold text-themed">{{ t('egress.pageTitle') }}</h1>
      <p class="mt-1 text-sm text-themed-muted">{{ t('egress.pageDescription') }}</p>
    </header>
    <p v-if="error" class="text-error break-words" role="alert">{{ error }}</p>
    <div class="grid items-start gap-6 lg:grid-cols-2">
      <section class="card space-y-3 p-5 sm:p-6">
        <h2 class="font-semibold text-themed">{{ t('egress.importNodes') }}</h2>
        <select v-model="source" class="input" :aria-label="t('egress.importNodes')"><option value="content">{{ t('egress.rawOrUri') }}</option><option value="url">{{ t('egress.subscriptionUrl') }}</option></select>
        <input v-if="source === 'url'" v-model="subscriptionUrl" type="url" class="input" :aria-label="t('egress.subscriptionUrl')" placeholder="https://example.com/subscription" />
        <template v-else>
          <input type="file" accept=".json,.txt,application/json,text/plain" :aria-label="t('egress.upload')" @change="upload" />
          <textarea v-model="content" class="input font-mono text-xs" rows="7" :aria-label="t('egress.rawOrUri')" :placeholder="t('egress.importHint')" />
        </template>
        <div class="flex flex-wrap gap-2"><button class="btn-secondary" :disabled="busy" @click="importNodes(false)">{{ t('egress.preview') }}</button><button class="btn-primary" :disabled="busy || !result?.nodes?.length" @click="importNodes(true)">{{ t('egress.confirmImport') }}</button></div>
        <div v-if="result" class="space-y-2 text-sm">
          <p v-if="result.added !== undefined">{{ t('egress.imported') }}: {{ result.added }}; {{ t('egress.duplicates') }}: {{ result.duplicates }}</p>
          <p v-for="(node, index) in result.nodes" :key="index" class="break-all text-themed-secondary">{{ node.name }} · {{ node.protocol }} · {{ node.server }}:{{ node.port }}</p>
          <p v-for="item in result.errors" :key="item.index" class="text-error">{{ item.index }}: {{ item.error }}</p>
        </div>
      </section>
      <section class="card space-y-3 p-5 sm:p-6">
        <h2 class="font-semibold text-themed">{{ t('egress.proxyNodes') }}</h2>
        <p v-if="!nodes.length" class="text-sm text-themed-muted">{{ t('egress.noNodes') }}</p>
        <div v-for="node in nodes" :key="node.id" class="flex items-start gap-3 rounded-lg border border-themed p-3">
          <input v-model="selected" type="checkbox" :value="node.id" :aria-label="node.name" />
          <div class="min-w-0 flex-1 break-all text-sm"><p class="font-medium text-themed">{{ node.name }}</p><p class="text-themed-muted">{{ node.protocol }} · {{ node.server }}:{{ node.port }}</p></div>
          <button class="btn-ghost text-error" :disabled="busy" @click="remove(node.id)">{{ t('common.delete') }}</button>
        </div>
        <label class="block text-sm text-themed">{{ t('egress.probeHost') }}<select v-model="probeHost" class="input mt-1"><option v-for="host in hosts" :key="host.id" :value="host.id">{{ host.name }}</option></select></label>
        <label class="block text-sm text-themed">{{ t('egress.probeUrl') }}<input v-model="probeUrl" class="input mt-1" type="url" /></label>
        <button class="btn-primary" :disabled="busy || !selected.length || selected.length > 10 || !probeHost" @click="probe">{{ t('egress.testSelected') }}</button>
        <p class="text-xs text-themed-muted">{{ t('egress.probeHint') }}</p>
      </section>
      <section class="card space-y-3 p-5 sm:p-6 lg:col-span-2">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h2 class="font-semibold text-themed">{{ t('egress.probeResults') }}</h2>
          <button type="button" class="btn-secondary" :disabled="refreshing" @click="refreshJobs">{{ t('common.refresh') }}</button>
        </div>
        <p v-if="refreshedAt" class="text-xs text-themed-muted">{{ t('egress.autoRefresh', { seconds: 3, time: refreshedAt }) }}</p>
        <p v-if="refreshError" class="text-error text-sm" role="alert">{{ refreshError }}</p>
        <p v-if="!jobs.length" class="text-sm text-themed-muted">{{ t('egress.noProbeResults') }}</p>
        <article v-for="job in jobs" :key="job.id" class="space-y-2 rounded-lg border border-themed p-3 text-sm" aria-live="polite">
          <p class="text-themed">{{ job.host.name }} · {{ t(`egress.jobs.${job.state}`) }} · {{ new Date(job.createdAt).toLocaleString() }}</p>
          <p v-if="job.host.agent" class="text-xs text-themed-muted">{{ t('egress.agentHeartbeat', { version: job.host.agent.version || '-', time: job.host.agent.lastSeenAt ? new Date(job.host.agent.lastSeenAt).toLocaleString() : '-' }) }}</p>
          <p v-if="job.state === 'pending'" class="text-themed-muted">{{ t('egress.awaitingHeartbeat') }}</p>
          <p v-else-if="job.state === 'running'" class="text-themed-muted">{{ t('egress.awaitingResult') }}</p>
          <p v-else-if="job.state === 'expired'" class="text-warning">{{ t('egress.probeExpired') }}</p>
          <p v-if="job.state === 'pending' || job.state === 'running'" class="text-xs text-themed-muted">{{ t('egress.probeDeadline', { time: new Date(job.expiresAt).toLocaleString() }) }}</p>
          <p v-for="item in job.result" :key="item.nodeId" :class="item.success ? 'text-success' : 'text-error'">{{ nodes.find(n => n.id === item.nodeId)?.name || item.nodeId }}: {{ item.success ? `${item.latencyMs} ms` : item.error }}<span v-if="item.errorDetail"> · {{ t(`egress.failureReasons.${item.errorDetail}`) }}</span></p>
        </article>
      </section>
    </div>
  </div>
</template>
