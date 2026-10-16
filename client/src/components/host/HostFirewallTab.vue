<script setup lang="ts">
import { ref, watch, onUnmounted, toRaw } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import DrawerModal from '@/components/DrawerModal.vue'
import NetworkPolicyStatus from './NetworkPolicyStatus.vue'
import type { FirewallPolicy, FirewallRule, NetworkPolicy } from '@/api/egress-types'
const props = defineProps<{ hostId: number }>()
const { t } = useI18n()
const policy = ref<NetworkPolicy | null>(null), form = ref<FirewallPolicy | null>(null), error = ref(''), busy = ref(false), dirty = ref(false), showRule = ref(false), editing = ref(-1), formRevision = ref(0)
const emptyRule = (): FirewallRule => ({ direction: 'egress', action: 'drop', protocol: 'tcp', source: '', destination: '', sourcePort: '', destinationPort: '', description: '', enabled: true })
const draft = ref(emptyRule())
let generation = 0
async function load(reset = false) {
  const current = generation
  try { const response = await api.network.policy(props.hostId); if (current !== generation) return; policy.value = response.policy; if (!form.value || reset) { form.value = structuredClone(response.policy.firewall); formRevision.value = response.policy.revision; dirty.value = false } }
  catch (e: any) { if (current === generation) error.value = e.message }
}
function edit(index = -1) { editing.value = index; draft.value = index < 0 ? emptyRule() : structuredClone(toRaw(form.value!.rules[index])); showRule.value = true }
function acceptRule() { if (!form.value) return; if (editing.value < 0) form.value.rules.push(structuredClone(toRaw(draft.value))); else form.value.rules[editing.value] = structuredClone(toRaw(draft.value)); showRule.value = false; dirty.value = true }
function remove(index: number) { form.value?.rules.splice(index, 1); dirty.value = true }
async function save() {
  if (!form.value) return
  busy.value = true; error.value = ''
  try { await api.network.updateFirewall(props.hostId, formRevision.value, form.value); await load(true) } catch (e: any) { error.value = e.message }
  finally { busy.value = false }
}
watch(() => props.hostId, () => { generation++; policy.value = null; form.value = null; error.value = ''; showRule.value = false; void load() }, { immediate: true })
const timer = setInterval(() => { void load() }, 15_000)
onUnmounted(() => { generation++; clearInterval(timer) })
</script>
<template>
  <section class="space-y-5">
    <div><h2 class="text-lg font-semibold text-themed">{{ t('egress.firewall') }}</h2><p class="mt-1 text-sm text-themed-muted">{{ t('egress.firewallHint') }}</p></div>
    <NetworkPolicyStatus :policy="policy" />
    <p v-if="error" class="text-error" role="alert">{{ error }}</p>
    <div v-if="form" class="card space-y-5 p-5" @input="dirty = true" @change="dirty = true">
      <label class="flex items-center gap-2 text-sm text-themed"><input v-model="form.enabled" type="checkbox" />{{ t('egress.enableFirewall') }}</label>
      <div class="grid gap-4 sm:grid-cols-2">
        <label class="text-sm text-themed">{{ t('egress.defaultIngress') }}<select v-model="form.ingressAction" class="input mt-1"><option value="allow">{{ t('egress.allow') }}</option><option value="drop">{{ t('egress.drop') }}</option><option value="reject">{{ t('egress.reject') }}</option></select></label>
        <label class="text-sm text-themed">{{ t('egress.defaultEgress') }}<select v-model="form.egressAction" class="input mt-1"><option value="allow">{{ t('egress.allow') }}</option><option value="drop">{{ t('egress.drop') }}</option><option value="reject">{{ t('egress.reject') }}</option></select></label>
      </div>
      <p class="text-xs text-themed-muted">{{ t('egress.aclOrder') }}</p>
      <div class="space-y-3">
        <article v-for="(rule, index) in form.rules" :key="index" class="flex flex-wrap items-start gap-3 rounded-lg border border-themed p-3 text-sm">
          <input v-model="rule.enabled" type="checkbox" :aria-label="t('egress.enableRule')" />
          <div class="min-w-0 flex-1 break-words text-themed"><p class="font-medium">{{ t(`egress.${rule.direction}`) }} · {{ t(`egress.${rule.action}`) }} · {{ rule.protocol || t('egress.any') }}</p><p class="break-all font-mono text-xs text-themed-muted">{{ rule.source || '*' }}:{{ rule.sourcePort || '*' }} → {{ rule.destination || '*' }}:{{ rule.destinationPort || '*' }}</p><p v-if="rule.description" class="text-themed-muted">{{ rule.description }}</p></div>
          <button class="btn-ghost" @click="edit(index)">{{ t('egress.editRule') }}</button><button class="btn-ghost text-error" @click="remove(index)">{{ t('common.delete') }}</button>
        </article>
      </div>
      <div class="flex flex-wrap gap-2"><button class="btn-secondary" :disabled="form.rules.length >= 200" @click="edit()">{{ t('egress.addRule') }}</button><button class="btn-primary" :disabled="busy || !dirty" @click="save">{{ t('egress.saveFirewall') }}</button><button class="btn-secondary" :disabled="busy" @click="load(true)">{{ t('egress.reload') }}</button></div>
    </div>
    <DrawerModal :show="showRule" :title="t('egress.editRule')" @close="showRule = false">
      <form class="space-y-4" @submit.prevent="acceptRule">
        <label class="block text-sm text-themed">{{ t('egress.direction') }}<select v-model="draft.direction" class="input mt-1"><option value="egress">{{ t('egress.egress') }}</option><option value="ingress">{{ t('egress.ingress') }}</option></select></label>
        <label class="block text-sm text-themed">{{ t('egress.action') }}<select v-model="draft.action" class="input mt-1"><option value="drop">{{ t('egress.drop') }}</option><option value="reject">{{ t('egress.reject') }}</option><option value="allow">{{ t('egress.allow') }}</option></select></label>
        <label class="block text-sm text-themed">{{ t('egress.protocol') }}<select v-model="draft.protocol" class="input mt-1"><option value="">{{ t('egress.any') }}</option><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp4">ICMPv4</option><option value="icmp6">ICMPv6</option></select></label>
        <label class="block text-sm text-themed">{{ t('egress.source') }}<input v-model="draft.source" class="input mt-1" placeholder="192.0.2.0/24,198.51.100.1" /></label>
        <label class="block text-sm text-themed">{{ t('egress.destination') }}<input v-model="draft.destination" class="input mt-1" placeholder="203.0.113.0/24" /></label>
        <label class="block text-sm text-themed">{{ t('egress.sourcePort') }}<input v-model="draft.sourcePort" class="input mt-1" placeholder="1000-2000" /></label>
        <label class="block text-sm text-themed">{{ t('egress.destinationPort') }}<input v-model="draft.destinationPort" class="input mt-1" placeholder="25,465,587" /></label>
        <label class="block text-sm text-themed">{{ t('egress.description') }}<input v-model="draft.description" class="input mt-1" maxlength="200" /></label>
        <div class="flex gap-2"><button type="button" class="btn-secondary" @click="showRule = false">{{ t('common.cancel') }}</button><button type="submit" class="btn-primary">{{ t('common.confirm') }}</button></div>
      </form>
    </DrawerModal>
  </section>
</template>
