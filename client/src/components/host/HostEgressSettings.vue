<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import type { EgressSelection, EgressProfile, NetworkPolicy } from '@/api/egress-types'
import NetworkPolicyStatus from './NetworkPolicyStatus.vue'
const props = defineProps<{ hostId: number }>()
const { t } = useI18n()
const policy = ref<NetworkPolicy | null>(null), form = ref<EgressSelection | null>(null), profiles = ref<EgressProfile[]>([])
const error = ref(''), busy = ref(false), dirty = ref(false), agentReady = ref<boolean | null>(null), formRevision = ref(0)
let generation = 0
async function load(reset = false) {
  const current = generation
  try {
    const [response, catalog] = await Promise.all([api.network.policy(props.hostId), api.egress.profiles()])
    if (current !== generation) return
    policy.value = response.policy
    profiles.value = catalog.profiles.filter(profile => profile.active && profile.hostIds.includes(props.hostId))
    agentReady.value = response.networkControlReady === true
    if (!form.value || reset) { form.value = { egressMode: response.policy.egressMode, egressProfileId: response.policy.egressProfileId }; formRevision.value = response.policy.revision; dirty.value = false }
  } catch (e: any) { if (current === generation) error.value = e.message }
}
async function save() {
  if (!form.value) return
  busy.value = true; error.value = ''
  try { await api.network.updateEgress(props.hostId, formRevision.value, { ...form.value, egressProfileId: form.value.egressMode === 'profile' ? form.value.egressProfileId : null }); await load(true) }
  catch (e: any) { error.value = e.message; await load(Number.isSafeInteger(e.revision)) }
  finally { busy.value = false }
}
watch(() => props.hostId, () => { generation++; agentReady.value = null; form.value = null; policy.value = null; error.value = ''; void load() }, { immediate: true })
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
      <p v-if="agentReady === false" class="text-sm text-warning">{{ t('egress.agentRequired') }}</p>
      <div v-if="form" class="space-y-4" @change="dirty = true">
        <label class="block text-sm text-themed">{{ t('egress.mode') }}<select v-model="form.egressMode" class="input mt-1"><option value="off">{{ t('egress.off') }}</option><option value="direct">{{ t('egress.direct') }}</option><option value="profile">{{ t('packageEgress.profile') }}</option></select></label>
        <label v-if="form.egressMode === 'profile'" class="block text-sm text-themed">{{ t('packageEgress.select') }}<select v-model="form.egressProfileId" class="input mt-1"><option :value="null">{{ t('packageEgress.select') }}</option><option v-for="profile in profiles" :key="profile.id" :value="profile.id">{{ profile.name }}</option></select></label>
        <p class="text-xs text-themed-muted">{{ t('packageEgress.newOnly') }}</p>
        <button type="button" class="btn-primary" :disabled="busy || !dirty || (form.egressMode === 'profile' && !form.egressProfileId)" @click="save">{{ t(busy ? 'egress.applying' : 'egress.saveEgress') }}</button>
      </div>
    </div>
  </section>
</template>
