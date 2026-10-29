<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api/admin'
import DrawerModal from '@/components/DrawerModal.vue'
import type { EgressPolicy, EgressProfile } from '@/api/egress-types'
const props = defineProps<{ nodes: Array<{ id: number; name: string }> }>()
const { t } = useI18n()
const profiles = ref<EgressProfile[]>([]), hosts = ref<Array<{ id: number; name: string }>>([])
const show = ref(false), busy = ref(false), error = ref(''), name = ref(''), profileId = ref(0), hostIds = ref<number[]>([]), cidrs = ref('')
const defaultPolicy = (): EgressPolicy => ({ mode: 'proxy', strategy: 'failover', proxyNodeIds: [], internalCidrs: [], probeUrl: 'https://1.1.1.1/cdn-cgi/trace', probeIntervalSeconds: 30 })
const form = ref(defaultPolicy())
async function load() { try { const [p,h] = await Promise.all([api.egress.profiles(), api.hosts.list({ all: true })]); profiles.value = p.profiles; hosts.value = h.hosts } catch (e: any) { error.value = e.message } }
function open(profile?: EgressProfile) { profileId.value = profile?.id ?? 0; name.value = profile?.name ?? ''; form.value = profile?.egress ? structuredClone(profile.egress) : defaultPolicy(); hostIds.value = [...profile?.hostIds ?? []]; cidrs.value = form.value.internalCidrs.join('\n'); error.value = ''; show.value = true }
function move(index: number, delta: number) {
  const list = [...form.value.proxyNodeIds], target = index + delta
  if (target < 0 || target >= list.length) return
  ;[list[index], list[target]] = [list[target], list[index]]
  form.value.proxyNodeIds = list
}
async function save() {
  if (!form.value.proxyNodeIds.length || !hostIds.value.length) { error.value = t('packageEgress.hostsRequired'); return }
  busy.value = true
  try {
    const egress = { ...form.value, internalCidrs: cidrs.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean) }
    const data = { name: name.value, hostIds: hostIds.value, egress }
    if (profileId.value) await api.egress.update(profileId.value, data); else await api.egress.create(data)
    show.value = false; await load()
  } catch (e: any) { error.value = e.message } finally { busy.value = false }
}
async function remove(id: number) { busy.value = true; try { await api.egress.remove(id); await load() } catch (e: any) { error.value = e.message } finally { busy.value = false } }
onMounted(load)
</script>
<template>
  <section class="card space-y-4 p-5 sm:p-6">
    <div class="flex flex-wrap justify-between items-center gap-3"><h2 class="font-semibold text-themed">{{ t('packageEgress.profiles') }}</h2><button class="btn-primary" type="button" @click="open()">{{ t('packageEgress.create') }}</button></div>
    <p class="text-xs text-themed-muted">{{ t('packageEgress.newOnly') }}</p>
    <p v-if="error && !show" class="text-error" role="alert">{{ error }}</p>
    <p v-if="!profiles.length" class="text-themed-muted text-sm">{{ t('packageEgress.empty') }}</p>
    <div class="overflow-hidden"><table v-if="profiles.length" class="w-full table-fixed text-sm"><thead><tr class="border-b border-themed text-left"><th class="w-1/4 py-3">{{ t('packageEgress.name') }}</th><th class="w-1/4">{{ t('egress.strategy') }}</th><th class="w-1/4">{{ t('packageEgress.node') }}</th><th class="w-1/4 text-right">{{ t('common.actions') }}</th></tr></thead><tbody><tr v-for="profile in profiles" :key="profile.id" class="border-b border-themed"><td class="py-3 break-words">{{ profile.name }}</td><td class="break-words">{{ profile.egress ? t(`egress.${profile.egress.strategy}`) : '' }}</td><td class="break-words">{{ profile.egress?.proxyNodeIds.map(id => nodes.find(node => node.id === id)?.name || id).join(', ') }}</td><td class="text-right"><button class="btn-ghost" @click="open(profile)">{{ t('common.edit') }}</button><button class="btn-ghost text-error" :disabled="busy" @click="remove(profile.id)">{{ t('common.delete') }}</button></td></tr></tbody></table></div>
    <DrawerModal v-model="show" :title="t(profileId ? 'packageEgress.edit' : 'packageEgress.create')">
      <form class="space-y-4" @submit.prevent="save">
        <p v-if="error" class="text-error" role="alert">{{ error }}</p>
        <label class="block space-y-2"><span>{{ t('packageEgress.name') }}</span><input v-model="name" class="input" required maxlength="80" /></label>
        <label class="block space-y-2"><span>{{ t('egress.strategy') }}</span><select v-model="form.strategy" class="input"><option value="failover">{{ t('egress.failover') }}</option><option value="balance">{{ t('egress.balance') }}</option><option value="leastPing">{{ t('egress.leastPing') }}</option></select></label>
        <fieldset class="space-y-2"><legend>{{ t('packageEgress.node') }}</legend><label v-for="node in props.nodes" :key="node.id" class="flex items-center gap-2"><input v-model="form.proxyNodeIds" type="checkbox" :value="node.id" /><span>{{ node.name }}</span></label></fieldset>
        <p class="text-xs text-themed-muted">{{ t('egress.orderHint') }}</p>
        <div v-for="(id, index) in form.proxyNodeIds" :key="id" class="flex items-center gap-2 text-sm"><span class="min-w-0 flex-1 break-all">{{ index + 1 }}. {{ nodes.find(node => node.id === id)?.name || id }}</span><button type="button" class="btn-ghost" :disabled="index === 0" :aria-label="t('egress.moveUp')" @click="move(index, -1)">↑</button><button type="button" class="btn-ghost" :disabled="index === form.proxyNodeIds.length - 1" :aria-label="t('egress.moveDown')" @click="move(index, 1)">↓</button></div>
        <fieldset class="space-y-2"><legend>{{ t('packageEgress.hosts') }}</legend><label v-for="host in hosts" :key="host.id" class="flex items-center gap-2"><input v-model="hostIds" type="checkbox" :value="host.id" /><span>{{ host.name }}</span></label></fieldset>
        <label class="block space-y-2"><span>{{ t('egress.probeUrl') }}</span><input v-model="form.probeUrl" class="input" type="url" required /></label>
        <label class="block space-y-2"><span>{{ t('egress.probeInterval') }}</span><input v-model.number="form.probeIntervalSeconds" class="input" type="number" min="10" max="3600" required /></label>
        <label class="block space-y-2"><span>{{ t('egress.internalCidrs') }}</span><textarea v-model="cidrs" class="input" rows="3" /></label>
        <div class="flex justify-end gap-3"><button type="button" class="btn-secondary" @click="show = false">{{ t('packageEgress.cancel') }}</button><button type="submit" class="btn-primary" :disabled="busy">{{ t('packageEgress.save') }}</button></div>
      </form>
    </DrawerModal>
  </section>
</template>
