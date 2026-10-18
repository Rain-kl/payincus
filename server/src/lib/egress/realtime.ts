import { prisma } from '../../db/prisma.js'
import { hostTunnelManager, TunnelControlError, type ControlMethod } from '../incus/tunnel-manager.js'
import { networkInstruction, recordNetworkStatus, sanitizeNetworkStatus } from './sync.js'

export function requireNetworkControl(hostId: number, method: ControlMethod) {
  if (!hostTunnelManager.isTunnelOnline(hostId)) throw new TunnelControlError('OFFLINE')
  if (!hostTunnelManager.supportsControl(hostId, method)) throw new TunnelControlError('UPGRADE_REQUIRED')
}
export function controlFailure(error: unknown): { status: number; error: string } {
  const code = error instanceof TunnelControlError ? error.code : 'REMOTE_ERROR'
  const failures = {
    OFFLINE: [503, '节点隧道未连接，请确认 Agent 在线后重试'],
    UPGRADE_REQUIRED: [409, '请升级 Agent 后使用实时配置与检测'],
    BUSY: [409, '节点正在执行检测或配置，请稍后重试'],
    TIMEOUT: [504, '等待节点响应超时，请检查节点状态'],
    DISCONNECTED: [503, '节点隧道已断开，请重新连接后重试'],
    CANCELLED: [499, '请求已取消'],
    INVALID_RESPONSE: [502, '节点返回的数据无效'],
    REMOTE_ERROR: [502, '节点执行失败，请查看 Agent 日志']
  } as const
  const [status, message] = failures[code]
  return { status, error: message }
}
// The backend is single-process. Serialize control requests per host so two
// concurrent provisions cannot supersede each other's in-flight revision.
const networkPushes = new Map<number, Promise<unknown>>()
export function pushNetworkPolicy(hostId: number) {
  const previous = networkPushes.get(hostId) ?? Promise.resolve()
  const pending = previous.catch(() => {}).then(() => applyNetworkPolicy(hostId))
  networkPushes.set(hostId, pending)
  void pending.finally(() => { if (networkPushes.get(hostId) === pending) networkPushes.delete(hostId) }).catch(() => {})
  return pending
}
async function applyNetworkPolicy(hostId: number) {
  const agent = await prisma.hostAgent.findUnique({ where: { hostId }, select: { enabled: true } })
  if (!agent?.enabled) throw new TunnelControlError('OFFLINE')
  const instruction = await networkInstruction(hostId)
  if (!instruction.revision) return null
  const method = instruction.bindings.length || hostTunnelManager.supportsControl(hostId, 'network.apply.v2') ? 'network.apply.v2' : 'network.apply'
  requireNetworkControl(hostId, method)
  const raw = await hostTunnelManager.requestControl<Record<string, unknown>>(hostId, method, instruction, 100_000)
  let status: ReturnType<typeof sanitizeNetworkStatus>
  try { status = sanitizeNetworkStatus(raw) } catch { throw new TunnelControlError('INVALID_RESPONSE') }
  if (Number(raw.appliedRevision) > instruction.revision) throw new TunnelControlError('INVALID_RESPONSE')
  await recordNetworkStatus(hostId, raw, instruction.revision)
  return { revision: instruction.revision, appliedRevision: Number(raw.appliedRevision), status, success: status.state === 'applied' && Number(raw.appliedRevision) === instruction.revision }
}
export function sanitizeProbeResults(raw: unknown, nodeIds: number[]) {
  const items = (raw as any)?.items as Array<Record<string, unknown>>
  if (!Array.isArray(items) || items.length !== nodeIds.length || new Set(items.map(i => i?.nodeId)).size !== nodeIds.length || items.some(i => !i || !Number.isSafeInteger(i.nodeId) || !nodeIds.includes(Number(i.nodeId)))) throw new TunnelControlError('INVALID_RESPONSE')
  const reasons = ['command_failed', 'timeout', 'permission_denied', 'missing_file', 'config_invalid', 'reality_key_invalid', 'reality_id_invalid', 'tls_config_invalid', 'unsupported_transport']
  return nodeIds.map(id => {
    const i = items.find(i => i.nodeId === id)!
    return { nodeId: id, success: i.success === true, latencyMs: typeof i.latencyMs === 'number' && Number.isFinite(i.latencyMs) ? Math.max(0, Math.min(120000, Math.round(i.latencyMs))) : null, error: i.success === true ? '' : ['timeout', 'handshake', 'http', 'xray', 'unsupported', 'network'].includes(String(i.error)) ? String(i.error) : 'network', errorDetail: i.success !== true && reasons.includes(String(i.errorDetail)) ? String(i.errorDetail) : '' }
  })
}
