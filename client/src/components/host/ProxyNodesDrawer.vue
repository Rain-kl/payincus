<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import DrawerModal from '@/components/DrawerModal.vue'
import type { ProxyNode, ProxyImportResult, ProbeJob } from '@/api/egress-types'
const props = defineProps<{ show: boolean; hostId: number }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const { t } = useI18n()
const nodes = ref<ProxyNode[]>([]), selected = ref<number[]>([]), hosts = ref<Array<{ id: number; name: string }>>([]), jobs = ref<ProbeJob[]>([])
const content = ref(''), subscriptionUrl = ref(''), source = ref('content'), busy = ref(false), error = ref(''), result = ref<ProxyImportResult | null>(null), probeHost = ref(props.hostId)
const probeUrl = ref('https://www.gstatic.com/generate_204')
let generation = 0
let timer: ReturnType<typeof setInterval> | undefined
async function load() {
  const current = generation
  try {
    const [n, p, h] = await Promise.all([api.network.nodes(), api.network.probes(), api.network.probeHosts()])
    if (current !== generation) return
    nodes.value = n.nodes; jobs.value = p.jobs; hosts.value = h.hosts
    selected.value = selected.value.filter(id => n.nodes.some(node => node.id === id))
  } catch (e: any) { if (current === generation) error.value = e.message }
}
async function importNodes(confirm: boolean) {
  busy.value = true; error.value = ''
  try {
    result.value = await api.network.importNodes({ ...(source.value === 'url' ? { subscriptionUrl: subscriptionUrl.value } : { content: content.value }), confirm })
    if (confirm) { content.value = ''; subscriptionUrl.value = ''; await load(); emit('changed') }
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
  try { await api.network.deleteNode(id); await load(); emit('changed') } catch (e: any) { error.value = e.message }
  finally { busy.value = false }
}
async function probe() {
  busy.value = true; error.value = ''
  try { await api.network.probe(probeHost.value, selected.value, probeUrl.value); await load() } catch (e: any) { error.value = e.message }
  finally { busy.value = false }
}
watch([content, subscriptionUrl, source], () => { result.value = null })
watch(() => props.show, show => {
  generation++
  if (timer) clearInterval(timer)
  if (show) { probeHost.value = props.hostId; void load(); timer = setInterval(() => { void load() }, 10_000) }
  else { content.value = ''; subscriptionUrl.value = ''; result.value = null; error.value = '' }
}, { immediate: true })
onUnmounted(() => { generation++; if (timer) clearInterval(timer) })
</script>
<template>
  <DrawerModal :show="show" :title="t('egress.proxyNodes')" max-width="max-w-3xl" @close="emit('close')">
    <div class="space-y-6">
      <p v-if="error" class="text-error break-words" role="alert">{{ error }}</p>
      <section class="space-y-3">
        <h3 class="font-semibold text-themed">{{ t('egress.importNodes') }}</h3>
        <select v-model="source" class="input"><option value="content">{{ t('egress.rawOrUri') }}</option><option value="url">{{ t('egress.subscriptionUrl') }}</option></select>
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
      <section class="space-y-3">
        <h3 class="font-semibold text-themed">{{ t('egress.proxyNodes') }}</h3>
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
      <section class="space-y-3">
        <h3 class="font-semibold text-themed">{{ t('egress.probeResults') }}</h3>
        <article v-for="job in jobs" :key="job.id" class="space-y-2 rounded-lg border border-themed p-3 text-sm">
          <p class="text-themed">{{ job.host.name }} · {{ t(`egress.jobs.${job.state}`) }} · {{ new Date(job.createdAt).toLocaleString() }}</p>
          <p v-for="item in job.result" :key="item.nodeId" :class="item.success ? 'text-success' : 'text-error'">{{ nodes.find(n => n.id === item.nodeId)?.name || item.nodeId }}: {{ item.success ? `${item.latencyMs} ms` : item.error }}</p>
        </article>
      </section>
    </div>
  </DrawerModal>
</template>
