import type { Prisma } from '@prisma/client'
import { prisma } from '../../db/prisma.js'

const commands = new Set(['help', 'help_or_version', 'list', 'ls', 'info', 'version', 'completion', 'exec', 'shell', 'console', 'file', 'export', 'publish', 'copy', 'cp', 'move', 'mv', 'config', 'profile', 'storage', 'network', 'query', 'start', 'restart', 'restore', 'snapshot', 'delete', 'launch', 'init', 'admin', 'operation', 'stop', 'pause', 'import', 'image', 'project', 'remote', 'alias', 'alias_or_unknown', 'unknown_option', 'info_log', 'config_read', 'profile_read', 'storage_read', 'network_read', 'image_read', 'project_read', 'remote_read', 'operation_read'])
export interface CliEvent { id: string; time: string; uid: number; pid: number; command: string; critical: boolean; blocked: boolean }
export function parseReport(input: unknown): { state: Prisma.InputJsonObject; events: CliEvent[] } {
  if (!input || typeof input !== 'object') throw new Error('invalid report')
  const value = input as Record<string, unknown>
  const state = value.state as Record<string, unknown> | undefined
  if (!state || typeof state.enabled !== 'boolean' || typeof state.locked !== 'boolean' || !Number.isSafeInteger(state.revision) || Number(state.revision) < 0) throw new Error('invalid state')
  if (!Array.isArray(value.events) || value.events.length > 128) throw new Error('invalid events')
  const events = value.events.map((raw: unknown) => {
    const e = raw as CliEvent
    if (!e || !/^[0-9a-f]{32}$/.test(e.id) || !commands.has(e.command) || typeof e.critical !== 'boolean' || typeof e.blocked !== 'boolean' || !Number.isInteger(e.uid) || e.uid < 0 || e.uid > 4294967295 || !Number.isInteger(e.pid) || e.pid < 0 || e.pid > 2147483647 || typeof e.time !== 'string' || !Number.isFinite(Date.parse(e.time))) throw new Error('invalid event')
    return { id: e.id, time: e.time, uid: e.uid, pid: e.pid, command: e.command, critical: e.critical, blocked: e.blocked }
  })
  // Status is allowlisted; never retain arbitrary agent-provided fields or command payloads.
  const results = Array.isArray(state.results) ? state.results.slice(0, 256).map((r: any) => ({ target: String(r.target || '').slice(0, 200), success: r.success === true, detail: r.detail === 'complete' || r.detail === 'not_configured' ? r.detail : 'failed' })) : []
  return { state: { enabled: state.enabled, locked: state.locked, revision: Number(state.revision), results, reason: typeof state.reason === 'string' && (state.reason === 'daemon_restart' || state.reason === 'panel_request' || state.reason === 'state_unavailable' || state.reason === 'audit_unavailable' || /^cli:[a-z_]+$/.test(state.reason)) ? state.reason.slice(0, 64) : '' }, events }
}
export async function recordReport(hostId: number, input: unknown): Promise<string[]> {
  const { state, events } = parseReport(input)
  await prisma.$transaction(async tx => {
    // Serialize this node's reports and preserve a newer durable lock revision during delayed delivery.
    await tx.$executeRaw`SELECT pg_advisory_xact_lock(700260, ${hostId})`
    const previous = await tx.nodeSecurityState.findUnique({ where: { hostId } })
    const previousRevision = previous?.report && typeof previous.report === 'object' && !Array.isArray(previous.report) ? Number(previous.report.revision ?? 0) : 0
    if (Number(state.revision) >= previousRevision || state.reason === 'state_unavailable' || state.reason === 'audit_unavailable') {
      await tx.nodeSecurityState.upsert({ where: { hostId }, create: { hostId, report: state }, update: { report: state, reportedAt: new Date() } })
    }
    await tx.nodeCliEvent.deleteMany({ where: { hostId, receivedAt: { lt: new Date(Date.now() - 30 * 86400000) } } })
    if (events.length) {
    const inserted = await tx.nodeCliEvent.createMany({ data: events.map(e => ({ hostId, eventId: e.id, command: e.command, uid: BigInt(e.uid), pid: e.pid, critical: e.critical, blocked: e.blocked, occurredAt: new Date(e.time) })), skipDuplicates: true })
    if (inserted.count > 0 && events.some(e => e.blocked)) await tx.log.create({ data: { userId: null, module: 'security', action: 'node.cli_alarm', content: `节点 #${hostId} 上报关键命令告警，请检查节点锁定与存储状态`, result: 'warning' } })
    }
  })
  return events.map(e => e.id)
}
