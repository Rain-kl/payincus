<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import api from '@/api/admin'
import type { NodeSecurityOverview } from '@/api/node-security-types'
import DrawerModal from '@/components/DrawerModal.vue'

const props = defineProps<{ hostId: number }>()
const emit = defineEmits<{ refresh: [] }>()
const overview = ref<NodeSecurityOverview | null>(null)
const busy = ref(false)
const error = ref('')
const message = ref('')
const drawer = ref(false)
const action = ref<'identity' | 'policy' | 'recover' | 'lock'>('policy')
const dataset = ref('')
const state = computed(() => overview.value?.state)
const recovering = computed(() => overview.value?.recoveryTask?.status === 'running')
const recoveryStatus = computed(() => ({ running: '后台恢复中', success: '恢复完成', failed: '恢复失败，可重试', cancelled: '恢复已停止', interrupted: '恢复已中断，可重试' })[overview.value?.recoveryTask?.status || 'running'])
const title = computed(() => ({ identity: '核对节点证书指纹', policy: '切换安全模式', recover: '批准节点恢复', lock: '立即锁定节点' })[action.value])
let generation = 0
let loading = false
async function refresh() {
  if (loading || busy.value) return
  loading = true
  const current = generation
  try {
    const result = await api.nodeSecurity.status(props.hostId)
    if (current === generation) {
      const changed = overview.value?.state?.locked !== result.state?.locked
      overview.value = result
      if (changed) emit('refresh')
    }
  } catch { if (current === generation) error.value = '无法读取节点管理状态，请检查权限和连接' }
  finally { loading = false }
}
function open(next: typeof action.value) {
  action.value = next
  dataset.value = ''
  error.value = ''
  message.value = ''
  drawer.value = true
}
function close() {
  if (busy.value) return
  drawer.value = false
}
async function submit() {
  if (busy.value) return
  const hostId = props.hostId
  const current = generation
  const revision = state.value?.revision
  busy.value = true
  error.value = ''
  try {
    if (action.value === 'identity') {
      await api.nodeSecurity.identity(hostId, dataset.value.replaceAll(':', '').toLowerCase().trim())
      if (current !== generation) return
      drawer.value = false
      message.value = '节点证书指纹已保存；后续连接将严格校验。'
    } else if (action.value === 'recover') {
      if (revision === undefined || overview.value?.offline) throw new Error('请先刷新在线状态')
      const accepted = await api.nodeSecurity.recover(hostId, revision)
      if (current !== generation) return
      drawer.value = false
      message.value = `恢复任务 #${accepted.taskId} 已提交，将在后台恢复实例，可查看任务日志。`
    } else {
      if (action.value !== 'lock' && (revision === undefined || overview.value?.offline)) throw new Error('请先刷新在线状态')
      await api.nodeSecurity.action(hostId, action.value, action.value === 'policy' ? { enabled: !state.value?.enabled, revision } : {})
      drawer.value = false
      message.value = '操作完成；授权恢复后自动恢复锁定前运行的实例。'
    }
  } catch {
    if (current === generation) error.value = action.value === 'recover'
      ? '恢复未完成，请查看任务记录并刷新状态后重试。'
      : '节点操作未完成，请刷新状态并检查连接。'
  }
  finally { busy.value = false; await refresh() }
}
watch(() => props.hostId, () => { generation++; overview.value = null; drawer.value = false; message.value = ''; error.value = ''; void refresh() }, { immediate: true })
const timer = setInterval(() => { void refresh() }, 5000)
onUnmounted(() => { generation++; clearInterval(timer) })
</script>

<template>
  <section class="card p-5 space-y-4">
    <div>
      <h3 class="text-lg font-semibold text-themed">节点安全模式</h3>
      <p class="text-sm text-themed-muted mt-1">所有受管 CLI 调用上报审计；安全模式默认关闭。</p>
    </div>
    <p v-if="error" class="text-sm text-themed" role="alert">{{ error }}</p>
    <p v-if="message" class="text-sm text-themed" role="status">{{ message }}</p>
    <div v-if="overview?.recoveryTask" class="rounded-lg border border-themed p-3 space-y-1 text-sm text-themed" role="status">
      <p>恢复任务 #{{ overview.recoveryTask.id }}：{{ recoveryStatus }}</p>
      <p v-if="overview.recoveryTask.error">{{ overview.recoveryTask.error }}</p>
      <RouterLink class="text-themed underline" :to="{ path: '/admin/tasks', query: { task: overview.recoveryTask.id } }">查看恢复任务日志</RouterLink>
    </div>
    <div class="flex flex-wrap gap-3 text-sm text-themed">
      <span>安全模式：{{ state?.enabled ? '已开启' : '未开启' }}</span>
      <span>API：{{ !state ? '未知' : state.locked ? '已锁定' : '未锁定' }}</span>
      <span v-if="overview?.offline">节点离线，显示上次上报</span>
      <span v-if="state?.reason">原因：{{ state.reason }}</span>
    </div>
    <div class="flex flex-wrap gap-2">
      <button class="btn-secondary btn-sm" :disabled="busy" @click="refresh">刷新</button>
      <button class="btn-secondary btn-sm" :disabled="busy" @click="open('identity')">核对证书指纹</button>
      <button class="btn-secondary btn-sm" :disabled="busy || !state || overview?.offline" @click="open('policy')">{{ state?.enabled ? '关闭安全模式' : '开启安全模式' }}</button>
      <button class="btn-secondary btn-sm" :disabled="busy || overview?.offline" @click="open('lock')">立即锁定</button>
      <button class="btn-primary btn-sm" :disabled="busy || recovering || (!state?.locked && !state?.resumeInstances?.length) || overview?.offline" @click="open('recover')">批准恢复</button>
    </div>
    <p class="text-xs text-themed-muted">锁定将停止本节点实例并尝试卸载已注册的加密存储；可能中断业务。存储动作失败时 API 仍保持锁定。</p>
    <p class="text-xs text-themed-muted break-all">节点证书 SHA256：{{ overview?.serverFingerprint || '尚未保存，请先通过可信终端核对' }}</p>
    <div v-for="result in state?.results || []" :key="result.target" class="rounded-lg border border-themed p-3 text-sm text-themed">
      {{ result.target }}：{{ result.detail === 'not_configured' ? '未配置加密存储，附加锁定不适用' : result.success ? '完成' : '失败，请检查占用或存储状态' }}
    </div>
    <details class="text-sm text-themed">
      <summary>最近 CLI 审计与告警（最多 100 条）</summary>
      <div v-for="event in overview?.events || []" :key="event.eventId" class="border-b border-themed py-2 break-words">
        {{ new Date(event.occurredAt).toLocaleString() }} · UID {{ event.uid }} · PID {{ event.pid }} · {{ event.command }} · {{ event.blocked ? '已拒绝' : '已记录' }}
      </div>
    </details>
    <DrawerModal :show="drawer" :title="title" :closable="!busy" :close-on-backdrop="!busy" @close="close">
      <form class="space-y-4" @submit.prevent="submit">
        <p class="text-sm text-themed-muted">请确认本次管理操作。存储磁盘的加密与解锁请在“存储”页面管理。</p>
        <p v-if="action === 'lock'" class="text-sm text-themed">此操作将中断本节点实例，之后需要人工恢复。</p>
        <p v-if="action === 'recover'" class="text-sm text-themed">请先确认告警原因已处理，并在“存储”页面解密相关磁盘。批准后创建后台任务，自动恢复锁定前运行的实例。</p>
        <label v-if="action === 'identity'" class="block text-sm text-themed">
          从节点 TUI 可信终端核对服务端证书 SHA256；更换证书时必须重新核对
          <input v-model="dataset" class="input mt-1 w-full" required autocomplete="off" />
        </label>
        <p v-if="message" class="text-sm text-themed">{{ message }}</p>
        <p v-if="error" class="text-sm text-themed" role="alert">{{ error }}</p>
        <div class="flex justify-end gap-2">
          <button type="button" class="btn-secondary" :disabled="busy" @click="close">关闭</button>
          <button type="submit" class="btn-primary" :disabled="busy">{{ busy ? '正在处理…' : '确认操作' }}</button>
        </div>
      </form>
    </DrawerModal>
  </section>
</template>
