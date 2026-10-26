<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import api from '@/api/admin'
import type { NodeSecurityOverview } from '@/api/node-security-types'
import DrawerModal from '@/components/DrawerModal.vue'

const props = defineProps<{ hostId: number }>()
const overview = ref<NodeSecurityOverview | null>(null)
const busy = ref(false)
const error = ref('')
const message = ref('')
const drawer = ref(false)
const action = ref<'identity' | 'policy' | 'recover' | 'lock' | 'save' | 'reveal' | 'unlock'>('policy')
const dataset = ref('')
const key = ref('')
const revealed = ref('')
const savedFingerprint = ref('')
const state = computed(() => overview.value?.state)
const title = computed(() => ({ identity: '核对节点证书指纹', policy: '切换关键命令诱捕', recover: '批准节点恢复', lock: '立即锁定节点', save: '托管 ZFS 密钥', reveal: '领取 ZFS 密钥', unlock: '加载 ZFS 密钥' })[action.value])
let generation = 0
let loading = false
async function refresh() {
  if (loading || busy.value) return
  loading = true
  const current = generation
  try {
    const result = await api.nodeSecurity.status(props.hostId)
    if (current === generation) overview.value = result
  } catch { if (current === generation) error.value = '无法读取节点管理状态，请检查权限和连接' }
  finally { loading = false }
}
function open(next: typeof action.value, name = '') {
  action.value = next
  dataset.value = name
  key.value = ''
  revealed.value = ''
  savedFingerprint.value = ''
  error.value = ''
  message.value = ''
  drawer.value = true
}
function close() {
  if (busy.value) return
  drawer.value = false
  key.value = ''
  revealed.value = ''
  savedFingerprint.value = ''
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
    } else if (action.value === 'save') {
      const result = await api.nodeSecurity.saveKey(hostId, dataset.value.trim(), key.value.trim())
      if (current !== generation) return
      key.value = ''
      savedFingerprint.value = result.fingerprint
      message.value = '已保存。请将此指纹复制到 TUI 核对后继续创建。'
    } else if (action.value === 'reveal') {
      const result = await api.nodeSecurity.revealKey(hostId, dataset.value.trim())
      if (current !== generation) return
      revealed.value = result.key
      message.value = '密钥已领取并记录审计；关闭抽屉后清除页面显示。注意剪贴板历史可能保存副本。'
    } else if (action.value === 'unlock') {
      await api.nodeSecurity.unlock(hostId, dataset.value.trim())
      message.value = '已加载存储密钥；仍需批准节点恢复，再启动容器。'
      drawer.value = false
    } else {
      if (action.value !== 'lock' && (revision === undefined || overview.value?.offline)) throw new Error('请先刷新在线状态')
      await api.nodeSecurity.action(hostId, action.value, action.value === 'policy' ? { enabled: !state.value?.enabled, revision } : action.value === 'recover' ? { revision } : {})
      drawer.value = false
      message.value = '操作完成；节点恢复不会自动启动全部容器。'
    }
  } catch { if (current === generation) error.value = '操作未完成。请检查密钥、节点连接与最新状态；未自动解除锁定。' }
  finally { busy.value = false; await refresh() }
}
watch(() => props.hostId, () => { generation++; overview.value = null; drawer.value = false; key.value = ''; revealed.value = ''; savedFingerprint.value = ''; message.value = ''; error.value = ''; void refresh() }, { immediate: true })
const timer = setInterval(() => { void refresh() }, 5000)
onUnmounted(() => { generation++; clearInterval(timer); key.value = ''; revealed.value = '' })
</script>

<template>
  <section class="card p-5 space-y-4">
    <div>
      <h3 class="text-lg font-semibold text-themed">节点安全模式</h3>
      <p class="text-sm text-themed-muted mt-1">所有受管 CLI 调用上报审计；关键命令诱捕默认关闭。API 锁定独立于 ZFS，关闭诱捕不会解除已有锁定。</p>
    </div>
    <p v-if="error" class="text-sm text-themed" role="alert">{{ error }}</p>
    <p v-if="message" class="text-sm text-themed" role="status">{{ message }}</p>
    <div class="flex flex-wrap gap-3 text-sm text-themed">
      <span>诱捕：{{ state?.enabled ? '已开启' : '未开启' }}</span>
      <span>API：{{ !state ? '未知' : state.locked ? '已锁定' : '未锁定' }}</span>
      <span v-if="overview?.offline">节点离线，显示上次上报</span>
      <span v-if="state?.reason">原因：{{ state.reason }}</span>
    </div>
    <div class="flex flex-wrap gap-2">
      <button class="btn-secondary btn-sm" :disabled="busy" @click="refresh">刷新</button>
      <button class="btn-secondary btn-sm" :disabled="busy" @click="open('identity')">核对证书指纹</button>
      <button class="btn-secondary btn-sm" :disabled="busy || !state || overview?.offline" @click="open('policy')">{{ state?.enabled ? '关闭诱捕' : '开启诱捕' }}</button>
      <button class="btn-secondary btn-sm" :disabled="busy || overview?.offline" @click="open('lock')">立即锁定</button>
      <button class="btn-primary btn-sm" :disabled="busy || !state?.locked || overview?.offline" @click="open('recover')">批准恢复</button>
      <button class="btn-secondary btn-sm" :disabled="busy" @click="open('save')">保存 ZFS 密钥</button>
    </div>
    <p class="text-xs text-themed-muted">锁定将停止本节点容器并尝试卸载已注册的加密存储；可能中断业务。存储动作失败时 API 仍保持锁定。</p>
    <p class="text-xs text-themed-muted break-all">节点证书 SHA256：{{ overview?.serverFingerprint || '尚未保存，请先通过可信终端核对' }}</p>
    <div v-for="result in state?.results || []" :key="result.target" class="rounded-lg border border-themed p-3 text-sm text-themed">
      {{ result.target }}：{{ result.detail === 'not_configured' ? '未配置加密存储，附加锁定不适用' : result.success ? '完成' : '失败，请检查占用或存储状态' }}
    </div>
    <div v-for="stored in overview?.keys || []" :key="stored.id" class="rounded-lg border border-themed p-3 space-y-2">
      <p class="text-sm text-themed break-all">{{ stored.dataset }} · 版本 {{ stored.version }}</p>
      <p class="text-xs text-themed-muted break-all">SHA256：{{ stored.fingerprint }}</p>
      <div class="flex flex-wrap gap-2">
        <button class="btn-secondary btn-sm" :disabled="busy" @click="open('reveal', stored.dataset)">领取密钥</button>
        <button class="btn-secondary btn-sm" :disabled="busy || overview?.offline" @click="open('unlock', stored.dataset)">加载密钥</button>
      </div>
    </div>
    <details class="text-sm text-themed">
      <summary>最近 CLI 审计与告警（最多 100 条）</summary>
      <div v-for="event in overview?.events || []" :key="event.eventId" class="border-b border-themed py-2 break-words">
        {{ new Date(event.occurredAt).toLocaleString() }} · UID {{ event.uid }} · PID {{ event.pid }} · {{ event.command }} · {{ event.blocked ? '已拒绝' : '已记录' }}
      </div>
    </details>
    <DrawerModal :show="drawer" :title="title" :closable="!busy" :close-on-backdrop="!busy" @close="close">
      <form class="space-y-4" @submit.prevent="submit">
        <p class="text-sm text-themed-muted">请确认本次管理操作。原始密钥仅用于托管、领取或存储解锁，不会写入操作日志。</p>
        <p v-if="action === 'lock'" class="text-sm text-themed">此操作将中断本节点容器，之后需要人工恢复。</p>
        <p v-if="action === 'recover'" class="text-sm text-themed">请先确认告警原因已处理，并加载全部已注册存储的密钥。批准后不自动启动容器。</p>
        <label v-if="action === 'identity'" class="block text-sm text-themed">
          从节点 TUI 可信终端核对服务端证书 SHA256；更换证书时必须重新核对
          <input v-model="dataset" class="input mt-1 w-full" required autocomplete="off" />
        </label>
        <label v-if="['save', 'reveal', 'unlock'].includes(action)" class="block text-sm text-themed">
          ZFS 加密根数据集
          <input v-model="dataset" class="input mt-1 w-full" required :readonly="action !== 'save'" autocomplete="off" placeholder="tank/incus" />
        </label>
        <label v-if="action === 'save'" class="block text-sm text-themed">
          TUI 生成的 64 位十六进制密钥
          <input v-model="key" class="input mt-1 w-full" type="password" autocomplete="off" required pattern="[0-9a-f]{64}" />
        </label>
        <p v-if="savedFingerprint" class="text-sm text-themed break-all">保存成功，SHA256：{{ savedFingerprint }}</p>
        <textarea v-if="revealed" class="input w-full font-mono" :value="revealed" readonly aria-label="领取的 ZFS 密钥" autocomplete="off" spellcheck="false" />
        <p v-if="message" class="text-sm text-themed">{{ message }}</p>
        <p v-if="error" class="text-sm text-themed" role="alert">{{ error }}</p>
        <div class="flex justify-end gap-2">
          <button type="button" class="btn-secondary" :disabled="busy" @click="close">关闭</button>
          <button v-if="!revealed && !savedFingerprint" type="submit" class="btn-primary" :disabled="busy">{{ busy ? '正在处理…' : '确认操作' }}</button>
        </div>
      </form>
    </DrawerModal>
  </section>
</template>
