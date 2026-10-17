<script setup lang="ts">
import { ref, watch, nextTick, onMounted, onBeforeUnmount, useId } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import DrawerModal from '@/components/DrawerModal.vue'
import api from '@/api'
import { buildApiWebSocketUrl } from '@/utils/api-url'

const props = defineProps<{ visible: boolean; instanceId: number | null; instanceName?: string; standalone?: boolean }>()
const emit = defineEmits<{ (event: 'update:visible', value: boolean): void }>()
const { t } = useI18n()
const router = useRouter()
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
let popupWindow: Window | null = null

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

function openWindow() {
  if (!props.instanceId) return
  if (popupWindow && !popupWindow.closed) {
    popupWindow.focus()
    emit('update:visible', false)
    return
  }
  const url = router.resolve({ name: 'console-window', params: { id: props.instanceId },
    query: { name: props.instanceName || '' } }).href
  // Open synchronously from the click so popup blockers can report failure before disconnecting.
  popupWindow = window.open('', `incudal-console-${props.instanceId}`, 'popup,width=1100,height=760,resizable=yes,scrollbars=yes')
  if (!popupWindow) { error.value = t('spice.windowFailed'); return }
  popupWindow.opener = null
  disconnect()
  popupWindow.location.replace(url)
  popupWindow.focus()
  emit('update:visible', false)
}

watch(() => [props.visible, props.instanceId], () => {
  if (props.visible && props.instanceId) void connect(props.standalone === true)
  else disconnect()
}, { immediate: true })
onMounted(() => { window.addEventListener('pagehide', disconnect) })
onBeforeUnmount(() => {
  window.removeEventListener('pagehide', disconnect)
  disconnect()
})
</script>

<template>
  <component :is="standalone ? 'section' : DrawerModal"
    v-bind="standalone ? { class: 'spice-window p-4' } : { modelValue: visible, title: `${t('spice.title')} · ${instanceName || ''}`, maxWidth: 'max-w-7xl', closeOnBackdrop: false }"
    @update:model-value="emit('update:visible', $event)">
    <h1 v-if="standalone" class="text-lg font-semibold text-themed mb-4">{{ t('spice.title') }} · {{ instanceName }}</h1>
    <div class="flex flex-wrap items-center gap-3 mb-4">
      <span class="text-sm text-themed-secondary" role="status">{{ t(`spice.${status}`) }}</span>
      <button type="button" class="btn-secondary" :disabled="status === 'connecting'" @click="connect(true)">{{ t('spice.reconnect') }}</button>
      <button type="button" class="btn-secondary" :disabled="status !== 'connected'" @click="sendCtrlAltDel">Ctrl+Alt+Del</button>
      <button v-if="!standalone" type="button" class="btn-secondary" @click="openWindow">{{ t('spice.window') }}</button>
    </div>
    <p v-if="error" class="text-sm text-themed-secondary mb-3" role="alert">{{ error }}</p>
    <div class="spice-viewport rounded-lg border border-themed overflow-auto">
      <div :id="screenId" ref="screen" class="spice-screen" :aria-label="t('spice.title')"></div>
    </div>
    <p class="text-xs text-themed-muted mt-3">{{ t(standalone ? 'spice.windowHint' : 'spice.hint') }}</p>
  </component>
</template>

<style scoped>
.spice-viewport { min-height: 24rem; max-height: 72vh; background: var(--terminal-bg); }
.spice-screen { position: relative; width: max-content; min-width: 100%; min-height: 24rem; }
.spice-window { height: 100vh; display: flex; flex-direction: column; background: var(--bg-primary); }
.spice-window .spice-viewport { flex: 1; max-height: none; min-height: 0; }
.spice-screen :deep(canvas) { display: block; }
</style>
