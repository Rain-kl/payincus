<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
const props = defineProps<{ mode: string; profileId: number | null; hostIds: number[]; networkMode: string; instanceType: string; privileged: boolean }>()
const emit = defineEmits<{ 'update:mode': [value: string]; 'update:profileId': [value: number | null] }>()
const { t } = useI18n()
const profiles = ref<Awaited<ReturnType<typeof api.egress.profiles>>['profiles']>([])
const error = ref('')
const supported = computed(() => props.networkMode === 'nat' && props.instanceType === 'container' && !props.privileged)
const permitted = computed(() => profiles.value.filter(profile => profile.active && props.hostIds.every(hostId => profile.hostIds.includes(hostId))))
const selectedAvailable = computed(() => props.mode !== 'profile' || permitted.value.some(profile => profile.id === props.profileId))
onMounted(async () => { try { profiles.value = (await api.egress.profiles()).profiles } catch { error.value = t('packageEgress.loadFailed') } })
function modeChange(event: Event) { const mode = (event.target as HTMLSelectElement).value; emit('update:mode', mode); if (mode !== 'profile') emit('update:profileId', null) }
</script>
<template>
  <div class="space-y-2">
    <label class="block text-sm font-medium text-themed-secondary" for="package-egress-mode">{{ t('packageEgress.title') }}</label>
    <select id="package-egress-mode" class="input" :value="mode" @change="modeChange"><option value="host">{{ t('packageEgress.host') }}</option><option value="direct">{{ t('packageEgress.direct') }}</option><option value="profile">{{ t('packageEgress.profile') }}</option></select>
    <select v-if="mode === 'profile'" class="input" :value="profileId ?? ''" required :aria-label="t('packageEgress.select')" @change="emit('update:profileId', Number(($event.target as HTMLSelectElement).value) || null)"><option value="">{{ t('packageEgress.select') }}</option><option v-for="profile in permitted" :key="profile.id" :value="profile.id">{{ profile.name }}</option></select>
    <p v-if="mode !== 'host' && !supported" class="text-error text-sm" role="alert">{{ t('packageEgress.unsupported') }}</p>
    <p v-if="!selectedAvailable" class="text-warning text-sm" role="alert">{{ t('packageEgress.unavailable') }}</p>
    <p v-if="error" class="text-error text-sm">{{ error }}</p>
    <p class="text-xs text-themed-muted">{{ t('packageEgress.newOnly') }}</p>
  </div>
</template>
