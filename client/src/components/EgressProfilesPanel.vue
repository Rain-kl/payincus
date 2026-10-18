<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import DrawerModal from '@/components/DrawerModal.vue'
const props = defineProps<{ nodes: Array<{ id: number; name: string }> }>()
const { t } = useI18n()
const profiles = ref<Awaited<ReturnType<typeof api.egress.profiles>>['profiles']>([])
const hosts = ref<Array<{ id: number; name: string }>>([])
const show = ref(false), busy = ref(false), error = ref(''), name = ref(''), profileId = ref(0), proxyNodeId = ref(0), hostIds = ref<number[]>([])
const unique = computed(() => profiles.value.filter((profile, index, list) => list.findIndex(item => item.profileId === profile.profileId) === index))
async function load() { try { const [p,h] = await Promise.all([api.egress.profiles(),api.hosts.list({ all: true })]); profiles.value = p.profiles; hosts.value = h.hosts } catch (e: any) { error.value = e.message } }
function open(id = 0) { profileId.value = id; name.value = ''; proxyNodeId.value = 0; hostIds.value = []; error.value = ''; show.value = true }
async function save() {
  if (!proxyNodeId.value || !hostIds.value.length) { error.value = t('packageEgress.hostsRequired'); return }
  busy.value = true
  try { const data = { proxyNodeId: proxyNodeId.value, hostIds: hostIds.value }; if (profileId.value) await api.egress.version(profileId.value,data); else await api.egress.create({ ...data,name: name.value }); show.value = false; await load() } catch (e: any) { error.value = e.message } finally { busy.value = false }
}
onMounted(load)
</script>
<template>
  <section class="card space-y-4 p-5 sm:p-6">
    <div class="flex justify-between items-center gap-3"><h2 class="font-semibold text-themed">{{ t('packageEgress.profiles') }}</h2><button class="btn-secondary" type="button" @click="open()">{{ t('packageEgress.create') }}</button></div>
    <p class="text-xs text-themed-muted">{{ t('packageEgress.newOnly') }}</p>
    <p v-if="error && !show" class="text-error" role="alert">{{ error }}</p>
    <p v-if="!profiles.length" class="text-themed-muted text-sm">{{ t('packageEgress.empty') }}</p>
    <div v-for="profile in unique" :key="profile.profileId" class="border-t border-themed pt-3 flex flex-wrap items-center justify-between gap-3"><div class="text-sm break-words">{{ profile.name }} · {{ t('packageEgress.version') }} {{ profile.version }} · {{ t('packageEgress.hosts') }}: {{ profile.hostIds.map(id => hosts.find(host => host.id === id)?.name || id).join(', ') }}</div><button class="btn-ghost text-sm" @click="open(profile.profileId)">{{ t('packageEgress.newVersion') }}</button></div>
    <DrawerModal v-model="show" :title="t(profileId ? 'packageEgress.newVersion' : 'packageEgress.create')">
      <form class="space-y-4" @submit.prevent="save">
        <p v-if="error" class="text-error" role="alert">{{ error }}</p>
        <label v-if="!profileId" class="block space-y-2"><span>{{ t('packageEgress.name') }}</span><input v-model="name" class="input" required maxlength="80" /></label>
        <label class="block space-y-2"><span>{{ t('packageEgress.node') }}</span><select v-model="proxyNodeId" class="input" required><option :value="0">{{ t('packageEgress.node') }}</option><option v-for="node in props.nodes" :key="node.id" :value="node.id">{{ node.name }}</option></select></label>
        <fieldset class="space-y-2"><legend class="mb-2">{{ t('packageEgress.hosts') }}</legend><label v-for="host in hosts" :key="host.id" class="flex items-center gap-2"><input v-model="hostIds" type="checkbox" :value="host.id" /><span>{{ host.name }}</span></label></fieldset>
        <div class="flex justify-end gap-3"><button type="button" class="btn-secondary" @click="show = false">{{ t('packageEgress.cancel') }}</button><button type="submit" class="btn-primary" :disabled="busy">{{ t('packageEgress.save') }}</button></div>
      </form>
    </DrawerModal>
  </section>
</template>
