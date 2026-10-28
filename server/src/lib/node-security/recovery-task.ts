import { prisma } from '../../db/prisma.js'
import { runOperationTask, taskLog } from '../operation-task-log.js'

export const NODE_RECOVERY_KIND = 'node_recover'

// Persist acceptance before scheduling any node request; a failed insert must never unlock a node.
export async function enqueueNodeRecovery(hostId: number, action: () => Promise<void>): Promise<{ taskId: number; status: 'running' }> {
  const accepted = await prisma.$transaction(async tx => {
    await tx.$executeRaw`SELECT pg_advisory_xact_lock(700261, ${hostId})`
    const existing = await tx.operationTask.findFirst({ where: { hostId, kind: NODE_RECOVERY_KIND, status: 'running' }, orderBy: { id: 'desc' } })
    if (existing) return { id: existing.id, created: false }
    const task = await tx.operationTask.create({ data: { hostId, instanceName: `节点 #${hostId}`, kind: NODE_RECOVERY_KIND } })
    return { id: task.id, created: true }
  })
  if (accepted.created) {
    setImmediate(() => {
      void runOperationTask(hostId, `节点 #${hostId}`, NODE_RECOVERY_KIND, action, { id: accepted.id })
        .catch(() => { taskLog('error', `节点 #${hostId} 恢复任务未完成，请检查任务日志并重试`) })
    })
  }
  return { taskId: accepted.id, status: 'running' }
}

export function latestNodeRecovery(hostId: number) {
  return prisma.operationTask.findFirst({
    where: { hostId, kind: NODE_RECOVERY_KIND }, orderBy: { id: 'desc' },
    select: { id: true, status: true, error: true, createdAt: true, finishedAt: true }
  })
}
