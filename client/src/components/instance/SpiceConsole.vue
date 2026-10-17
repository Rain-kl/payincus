<script setup lang="ts">
import { ref, watch, nextTick, onBeforeUnmount, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import DrawerModal from '@/components/DrawerModal.vue'
import api from '@/api'
import { buildApiWebSocketUrl } from '@/utils/api-url'

const props = defineProps<{ visible: boolean; instanceId: number | null; instanceName?: string }>()
const emit = defineEmits<{ (event: 'update:visible', value: boolean): void }>()
const { t } = useI18n()
const screen = ref<HTMLDivElement | null>(null)
const status = ref<'connecting' | 'connected' | 'error'>('connecting')
const error = ref('')
const screenId = `spice-screen-${useId()}`
interface SpiceClient { stop(): void; inputs?: { state: string } }
interface SpiceModule {
  SpiceMainConn: new (options: { uri: string; websocketProtocols: string[]; password: string; screen_id: string; onerror: () => void; onsuccess: () => void }) => SpiceClient
  sendCtrlAltDel(client: SpiceClient): void
}
let module: SpiceModule | undefined
let client: SpiceClient | undefined
let session: { id: number; ticket: string } | undefined
let generation = 0
let deadline: ReturnType<typeof setTimeout> | undefined
let readyPoll: ReturnType<typeof setInterval> | undefined

function disconnect() {
  generation++
  clearTimeout(deadline)
  clearInterval(readyPoll)
  const previous = client
  client = undefined
  try { previous?.stop() } catch { /* The server-side close still releases every channel. */ }
  screen.value?.replaceChildren()
  if (session) {
    const previousSession = session
    session = undefined
    void api.instances.closeSpiceSession(previousSession.id, previousSession.ticket).catch(() => {})
  }
}

async function connect(reconnect = false) {
  disconnect()
  if (!props.visible || !props.instanceId) return
  const current = generation
  const instanceId = props.instanceId
  status.value = 'connecting'
  error.value = ''
  const failed = () => {
    if (current !== generation) return
    disconnect()
    status.value = 'error'
    error.value = t('spice.connectionFailed')
  }
  try {
    await nextTick()
    // Vendored source is shipped with both builds; no third-party CDN or credentials.
    const moduleUrl = `${import.meta.env.BASE_URL}vendor/spice/src/main.js`
    module = await import(/* @vite-ignore */ moduleUrl) as SpiceModule
    if (current !== generation) return
    const result = await api.instances.createSpiceSession(instanceId, reconnect)
    if (current !== generation) {
      void api.instances.closeSpiceSession(instanceId, result.ticket).catch(() => {})
      return
    }
    session = { id: instanceId, ticket: result.ticket }
    deadline = setTimeout(failed, 30_000)
    const updateReady = () => {
      if (current !== generation || !screen.value?.querySelector('canvas') || client?.inputs?.state !== 'ready') return
      clearTimeout(deadline)
      clearInterval(readyPoll)
      status.value = 'connected'
    }
    client = new module.SpiceMainConn({
      uri: buildApiWebSocketUrl(`/ws/instances/${instanceId}/spice`),
      websocketProtocols: ['binary', `incudal-spice.${result.ticket}`],
      password: '', screen_id: screenId, onerror: failed,
      onsuccess: updateReady
    })
    readyPoll = setInterval(updateReady, 200)
  } catch (cause) {
    if (current !== generation) return
    failed()
    const failure = cause as { code?: string; response?: { data?: { code?: string } } }
    const code = failure?.code || failure?.response?.data?.code
    if (code === 'CONSOLE_LIMIT') error.value = t('spice.limit')
    if (code === 'CONSOLE_FORBIDDEN') error.value = t('spice.forbidden')
  }
}

function sendCtrlAltDel() {
  if (client && module && client.inputs?.state === 'ready') module.sendCtrlAltDel(client)
  screen.value?.querySelector('canvas')?.focus()
}

async function fullscreen() {
  try { await screen.value?.parentElement?.requestFullscreen() }
  catch { error.value = t('spice.fullscreenFailed') }
}

watch(() => [props.visible, props.instanceId], () => {
  if (props.visible && props.instanceId) void connect()
  else disconnect()
}, { immediate: true })
onBeforeUnmount(disconnect)
</script>

<template>
  <DrawerModal :model-value="visible" :title="`${t('spice.title')} · ${instanceName || ''}`" max-width="max-w-7xl" :close-on-backdrop="false" @update:model-value="emit('update:visible', $event)">
    <div class="flex flex-wrap items-center gap-3 mb-4">
      <span class="text-sm text-themed-secondary" role="status">{{ t(`spice.${status}`) }}</span>
      <button type="button" class="btn-secondary" :disabled="status === 'connecting'" @click="connect(true)">{{ t('spice.reconnect') }}</button>
      <button type="button" class="btn-secondary" :disabled="status !== 'connected'" @click="sendCtrlAltDel">Ctrl+Alt+Del</button>
      <button type="button" class="btn-secondary" :disabled="status !== 'connected'" @click="fullscreen">{{ t('spice.fullscreen') }}</button>
    </div>
    <p v-if="error" class="text-sm text-themed-secondary mb-3" role="alert">{{ error }}</p>
    <div class="spice-viewport rounded-lg border border-themed overflow-auto">
      <div :id="screenId" ref="screen" class="spice-screen" :aria-label="t('spice.title')"></div>
    </div>
    <p class="text-xs text-themed-muted mt-3">{{ t('spice.hint') }}</p>
  </DrawerModal>
</template>

<style scoped>
.spice-viewport { min-height: 24rem; max-height: 72vh; background: var(--terminal-bg); }
.spice-screen { position: relative; width: max-content; min-width: 100%; min-height: 24rem; }
.spice-viewport:fullscreen { max-height: none; width: 100%; height: 100%; }
.spice-screen :deep(canvas) { display: block; }
</style>
