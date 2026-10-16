<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import type { EgressPolicy, NetworkPolicy, ProxyNode } from '@/api/egress-types'
import NetworkPolicyStatus from './NetworkPolicyStatus.vue'
const props = defineProps<{ hostId: number }>()
const { t } = useI18n()
const policy = ref<NetworkPolicy | null>(null), form = ref<EgressPolicy | null>(null), nodes = ref<ProxyNode[]>([])
const cidrs = ref(''), error = ref(''), busy = ref(false), dirty = ref(false), agentReady = ref(false)
const formRevision = ref(0)
let generation = 0
async function load(reset = false) {
  const current = generation
  try {
    const [response, catalog] = await Promise.all([api.network.policy(props.hostId), api.network.nodes()])
    if (current !== generation) return
    policy.value = response.policy; nodes.value = catalog.nodes; agentReady.value = Array.isArray(response.agent?.capabilities) && response.agent.capabilities.includes('network-control')
    if (!form.value || reset) { form.value = structuredClone(response.policy.egress); formRevision.value = response.policy.revision; cidrs.value = form.value.internalCidrs.join('\n'); dirty.value = false }
  } catch (e: any) { if (current === generation) error.value = e.message }
}
function toggle(id: number, checked: boolean) {
  if (!form.value) return
  form.value.proxyNodeIds = checked ? [...form.value.proxyNodeIds, id] : form.value.proxyNodeIds.filter(n => n !== id); dirty.value = true
}
function move(index: number, delta: number) {
  if (!form.value) return
  const list = [...form.value.proxyNodeIds], target = index + delta
  if (target < 0 || target >= list.length) return
  ;[list[index], list[target]] = [list[target], list[index]]; form.value.proxyNodeIds = list; dirty.value = true
}
async function save() {
  if (!form.value || !policy.value) return
  busy.value = true; error.value = ''
  try { form.value.internalCidrs = cidrs.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean); await api.network.updateEgress(props.hostId, formRevision.value, form.value); await load(true) }
  catch (e: any) { error.value = e.message; await load(Number.isSafeInteger(e.revision)) }
  finally { busy.value = false }
}
watch(() => props.hostId, () => { generation++; form.value = null; policy.value = null; error.value = ''; void load() }, { immediate: true })
const timer = setInterval(() => { void load() }, 15_000)
onUnmounted(() => { generation++; clearInterval(timer) })
</script>
<template>
  <section class="card">
    <div class="flex flex-wrap items-center justify-between gap-3 border-b border-themed px-5 py-4 sm:px-6">
      <h2 class="text-sm font-semibold text-themed">{{ t('egress.title') }}</h2>
      <RouterLink class="btn-secondary" :to="{ name: 'admin-proxy-nodes', query: { hostId } }">{{ t('egress.manageNodes') }}</RouterLink>
    </div>
    <div class="space-y-5 px-5 py-5 sm:px-6">
      <NetworkPolicyStatus :policy="policy" />
      <p v-if="error" class="text-error" role="alert">{{ error }}</p>
      <p v-if="!agentReady" class="text-sm text-warning">{{ t('egress.agentRequired') }}</p>
      <div v-if="form" class="space-y-4" @input="dirty = true" @change="dirty = true">
        <label class="block text-sm text-themed">{{ t('egress.mode') }}<select v-model="form.mode" class="input mt-1"><option value="off">{{ t('egress.off') }}</option><option value="direct">{{ t('egress.direct') }}</option><option value="proxy">{{ t('egress.proxy') }}</option></select></label>
        <template v-if="form.mode === 'proxy'">
          <label class="block text-sm text-themed">{{ t('egress.strategy') }}<select v-model="form.strategy" class="input mt-1"><option value="failover">{{ t('egress.failover') }}</option><option value="balance">{{ t('egress.balance') }}</option><option value="leastPing">{{ t('egress.leastPing') }}</option></select></label>
          <div class="grid gap-2 sm:grid-cols-2"><label v-for="node in nodes" :key="node.id" class="flex items-center gap-2 rounded-lg border border-themed p-2 text-sm text-themed"><input type="checkbox" :checked="form.proxyNodeIds.includes(node.id)" @change="toggle(node.id, ($event.target as HTMLInputElement).checked)" /><span class="break-all">{{ node.name }}</span></label></div>
          <p class="text-xs text-themed-muted">{{ t('egress.orderHint') }}</p>
          <div v-for="(id, index) in form.proxyNodeIds" :key="id" class="flex items-center gap-2 text-sm"><span class="min-w-0 flex-1 break-all text-themed">{{ index + 1 }}. {{ nodes.find(n => n.id === id)?.name || id }}</span><button type="button" class="btn-ghost" :disabled="index === 0" :aria-label="t('egress.moveUp')" @click="move(index, -1)">↑</button><button type="button" class="btn-ghost" :disabled="index === form.proxyNodeIds.length - 1" :aria-label="t('egress.moveDown')" @click="move(index, 1)">↓</button></div>
          <label class="block text-sm text-themed">{{ t('egress.probeUrl') }}<input v-model="form.probeUrl" class="input mt-1" type="url" /></label>
          <label class="block text-sm text-themed">{{ t('egress.probeInterval') }}<input v-model.number="form.probeIntervalSeconds" class="input mt-1" type="number" min="10" max="3600" /></label>
        </template>
        <label class="block text-sm text-themed">{{ t('egress.internalCidrs') }}<textarea v-model="cidrs" rows="3" class="input mt-1 font-mono text-xs" /></label>
        <p class="text-xs text-themed-muted">{{ t('egress.behaviorHint') }}</p>
        <button type="button" class="btn-primary" :disabled="busy || !dirty" @click="save">{{ t(busy ? 'egress.applying' : 'egress.saveEgress') }}</button>
      </div>
    </div>
  </section>
</template>
