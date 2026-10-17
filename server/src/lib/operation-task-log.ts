import { AsyncLocalStorage } from 'node:async_hooks'
import { format } from 'node:util'
import { prisma } from '../db/prisma.js'
import { sanitizeObject, sanitizeTokensInString } from './log-sanitizer.js'
import { hostTunnelManager } from './incus/tunnel-manager.js'

interface TaskContext { id: number; hostId: number; name: string; queue: Promise<void>; count: number; nodeAck: boolean; pending: number; dropped: number }
const context = new AsyncLocalStorage<TaskContext>()
const active = new Map<number, TaskContext>()
const MAX_LOGS = 5000
export const currentOperationTask = () => context.getStore()

export function redactTaskLog(message: string): string {
  return sanitizeTokensInString(message)
    .replace(/-----BEGIN [^-]*PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----/g, '[REDACTED_KEY]')
    .replace(/((?:password|passwd|secret|token|authorization|cookie|user-data|cloud-init)["']?\s*[:=]\s*)(?:"[^"\n]*"|'[^'\n]*'|[^\s,;]+)/gi, '$1[REDACTED]')
    .replace(/(https?:\/\/)[^\s/@]+:[^\s/@]+@/gi, '$1[REDACTED]@')
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '').slice(0, 8000)
}

function enqueue(ctx: TaskContext, message: string, level = 'info', source = 'server'): void {
  if (!ctx.id || ctx.count >= MAX_LOGS) return
  if (ctx.pending >= 100) { ctx.dropped++; return }
  ctx.pending++
  const text = ctx.count++ === MAX_LOGS - 1 ? '日志已达到上限，后续仅保留任务最终状态。' : redactTaskLog(message)
  ctx.queue = ctx.queue.then(async () => {
    await prisma.operationTaskLog.create({ data: { taskId: ctx.id, message: text, level, source } })
  }).catch(err => { console.error('[OperationTask] 写入日志失败', err instanceof Error ? err.message : 'database error') }).finally(() => { ctx.pending-- })
}

export function taskLog(level: 'info' | 'warn' | 'error', ...args: unknown[]): void {
  const message = redactTaskLog(format(...args.map(arg => arg instanceof Error ? arg.message : sanitizeObject(arg))))
  console[level === 'info' ? 'log' : level](message)
  const ctx = context.getStore()
  if (ctx) enqueue(ctx, message, level)
}

hostTunnelManager.on('task_log', (hostId: number, taskId: number, line: string) => {
  const ctx = active.get(taskId)
  if (ctx && ctx.hostId === hostId) { ctx.nodeAck = true; enqueue(ctx, line, 'info', 'node') }
})
hostTunnelManager.on('tunnel_connected', (hostId: number) => {
  for (const ctx of active.values()) if (ctx.hostId === hostId) {
    enqueue(ctx, '节点通道重连，恢复采集（断线期间日志可能不完整）。', 'warn')
    hostTunnelManager.sendTaskLogControl(hostId, ctx.id, ctx.name, 'start')
  }
})
hostTunnelManager.on('tunnel_disconnected', (hostId: number) => {
  for (const ctx of active.values()) if (ctx.hostId === hostId) enqueue(ctx, '节点日志通道断开，等待重连。', 'warn')
})

export async function runOperationTask<T>(hostId: number, name: string, kind: 'create' | 'destroy', action: () => Promise<T>): Promise<T> {
  if (context.getStore()) return action()
  const task = await prisma.operationTask.create({ data: { hostId, instanceName: name, kind } }).catch(() => null)
  if (!task) {
    console.error('[OperationTask] 无法创建诊断任务，继续执行原业务流程；请检查数据库迁移和连接')
    return context.run({ id: 0, hostId, name, queue: Promise.resolve(), count: 0, nodeAck: false, pending: 0, dropped: 0 }, action)
  }
  const ctx: TaskContext = { id: task.id, hostId, name, queue: Promise.resolve(), count: 0, nodeAck: false, pending: 0, dropped: 0 }
  active.set(task.id, ctx)
  return context.run(ctx, async () => {
    enqueue(ctx, kind === 'create' ? '开始创建实例' : '开始销毁实例')
    const collecting = hostTunnelManager.sendTaskLogControl(hostId, task.id, name, 'start')
    enqueue(ctx, collecting ? '已请求节点采集 Incus / QEMU 日志（需要支持任务日志的 Agent）。' : '节点日志通道未连接，仅记录服务端步骤和 Incus 操作进度。', collecting ? 'info' : 'warn')
    const ackTimer = setTimeout(() => {
      if (collecting && !ctx.nodeAck) enqueue(ctx, '节点未确认日志采集，请检查 Agent 版本和日志读取权限；服务端步骤继续记录。', 'warn')
    }, 5000)
    let detached = false
    const detach = () => {
      if (detached) return
      detached = true
      clearTimeout(ackTimer)
      active.delete(task.id)
      hostTunnelManager.sendTaskLogControl(hostId, task.id, name, 'stop')
    }
    // Allow the node's last periodic QEMU sample to arrive before closing a task.
    const drain = async () => {
      if (collecting && !ctx.nodeAck) enqueue(ctx, '未收到节点日志确认，本任务仅有服务端日志；请核对 Agent 版本和权限。', 'warn')
      if (collecting && ctx.nodeAck) await new Promise(resolve => setTimeout(resolve, 2100))
      await ctx.queue
      if (ctx.dropped) { enqueue(ctx, `日志写入繁忙，已丢弃 ${ctx.dropped} 条节点日志。`, 'warn'); await ctx.queue }
    }
    try {
      const result = await action()
      await drain()
      detach()
      enqueue(ctx, '任务执行完成')
      await ctx.queue
      await prisma.operationTask.updateMany({ where: { id: task.id, status: 'running' }, data: { status: 'success', finishedAt: new Date() } }).catch(() => { console.error('[OperationTask] 无法写入成功状态') })
      return result
    } catch (error) {
      const message = redactTaskLog(error instanceof Error ? error.message : String(error))
      await drain()
      detach()
      enqueue(ctx, message, 'error')
      await ctx.queue
      await prisma.operationTask.updateMany({ where: { id: task.id, status: 'running' }, data: { status: 'failed', error: message, finishedAt: new Date() } }).catch(() => { console.error('[OperationTask] 无法写入失败状态') })
      throw error
    } finally {
      detach()
      await ctx.queue
    }
  })
}

// 进程重启时不能将中断任务继续显示成运行中，也不能假定节点操作已停止。
export async function recoverInterruptedOperationTasks(): Promise<void> {
  const tasks = await prisma.operationTask.findMany({ where: { status: 'running' }, select: { id: true } })
  for (const task of tasks) await prisma.$transaction([
    prisma.operationTaskLog.create({ data: { taskId: task.id, source: 'server', level: 'error', message: '后端重启，任务执行结果未知；请检查节点实际状态后再操作。' } }),
    prisma.operationTask.update({ where: { id: task.id }, data: { status: 'interrupted', error: '后端重启，执行结果未知', finishedAt: new Date() } })
  ])
}
