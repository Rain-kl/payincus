<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
const props = defineProps<{ mode: string; versionId: number | null; hostIds: number[]; networkMode: string; instanceType: string; privileged: boolean }>()
const emit = defineEmits<{ 'update:mode': [value: string]; 'update:versionId': [value: number | null] }>()
const { t } = useI18n()
const profiles = ref<Awaited<ReturnType<typeof api.egress.profiles>>['profiles']>([])
const error = ref('')
const supported = computed(() => props.networkMode === 'nat' && props.instanceType === 'container' && !props.privileged)
const permitted = computed(() => profiles.value.filter(profile => profile.active && props.hostIds.every(hostId => profile.hostIds.includes(hostId))))
const selectedAvailable = computed(() => props.mode !== 'profile' || permitted.value.some(profile => profile.id === props.versionId))
onMounted(async () => { try { profiles.value = (await api.egress.profiles()).profiles } catch { error.value = t('packageEgress.loadFailed') } })
function modeChange(event: Event) { const mode = (event.target as HTMLSelectElement).value; emit('update:mode', mode); if (mode !== 'profile') emit('update:versionId', null) }
</script>
<template>
  <div class="space-y-2">
    <label class="block text-sm font-medium text-themed-secondary" for="package-egress-mode">{{ t('packageEgress.title') }}</label>
    <select id="package-egress-mode" class="input" :value="mode" @change="modeChange"><option value="legacy">{{ t('packageEgress.legacy') }}</option><option value="direct">{{ t('packageEgress.direct') }}</option><option value="profile">{{ t('packageEgress.profile') }}</option></select>
    <select v-if="mode === 'profile'" class="input" :value="versionId ?? ''" required :aria-label="t('packageEgress.select')" @change="emit('update:versionId', Number(($event.target as HTMLSelectElement).value) || null)"><option value="">{{ t('packageEgress.select') }}</option><option v-for="profile in permitted" :key="profile.id" :value="profile.id">{{ profile.name }} · v{{ profile.version }}</option></select>
    <p v-if="mode !== 'legacy' && !supported" class="text-error text-sm" role="alert">{{ t('packageEgress.unsupported') }}</p>
    <p v-if="!selectedAvailable" class="text-warning text-sm" role="alert">{{ t('packageEgress.unavailable') }}</p>
    <p v-if="error" class="text-error text-sm">{{ error }}</p>
    <p class="text-xs text-themed-muted">{{ t('packageEgress.newOnly') }}</p>
  </div>
</template>
