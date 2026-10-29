import { bindOperationCancellation, taskLog } from '../operation-task-log.js'
/**
 * Incus 客户端工具函数
 */

import { IncusApiError, operationError, redactNodeText } from './incus-error.js'
import type { IncusClient } from './incus-client.js'
import type { IncusOperation } from '../../types/incus.js'

/**
 * 等待异步操作完成
 * @param timeout 超时时间（毫秒），默认 30 分钟
 */
export async function waitForOperation(
  client: IncusClient,
  operationUrl: string,
  timeout: number = 30 * 60 * 1000,
  signal?: AbortSignal,
  requestBody?: unknown
): Promise<unknown> {
  const startTime = Date.now()
  taskLog('info', redactNodeText(`Incus 操作 ${operationUrl}，等待节点执行（超时 ${Math.round(timeout / 1000)} 秒）`, requestBody))
  let lastProgress = ''
  const unbind = bindOperationCancellation(async () => {
    try {
      const op = await client.request<IncusOperation>('GET', operationUrl, null, timeout, undefined, requestBody)
      if (op.status !== 'Running' && op.status !== 'Pending') throw new Error('节点操作已结束或正在取消，请刷新任务状态')
      if (!op.may_cancel) throw new Error('Incus 不支持取消当前操作，无法强行停止；请等待操作完成')
      await client.request('DELETE', operationUrl, null, timeout, undefined, requestBody)
    } catch (error) { throw operationError(error, requestBody) }
  })

  try {
    while (Date.now() - startTime < timeout) {
      signal?.throwIfAborted()
      const op = await client.request<IncusOperation>('GET', `${operationUrl}/wait?timeout=10`, null, timeout, signal, requestBody)

      if (!op || typeof op.status !== 'string' || typeof op.status_code !== 'number') throw new IncusApiError('节点异步操作响应格式无效', 502, requestBody)
      const progress = [op.description, op.status, ...Object.entries(op.metadata || {})
        .filter(([key]) => /^(download_progress|fs_progress|create_instance_from_image_unpack_progress|progress|unpack_progress)$/.test(key))
        .map(([key, value]) => `${key}: ${redactNodeText(String(value), requestBody).slice(0, 1000)}`)].filter(Boolean).map(value => redactNodeText(String(value), requestBody)).join(' · ')
      if (progress !== lastProgress) { taskLog('info', progress); lastProgress = progress }
      if (op.status === 'Cancelled') throw new IncusApiError('节点操作已取消，执行失败清理', 409, requestBody)
      if (op.status_code >= 400) throw new IncusApiError(op.err || op.error || '节点操作失败', op.status_code, requestBody)
      if (op.status === 'Success') {
        return op.metadata
      }

      if (op.status === 'Failure') {
        throw new IncusApiError(op.err || op.error || '操作失败', op.status_code, requestBody)
      }
    }

    throw new IncusApiError(`Operation timeout (${Math.round(timeout / 1000)} seconds)`, 504, requestBody)
  } catch (error) {
    if (signal?.aborted && (error === signal.reason || error instanceof Error && ['AbortError', 'TimeoutError'].includes(error.name))) {
      const reason = signal.reason
      throw new DOMException(redactNodeText(reason instanceof Error ? reason.message : String(reason), requestBody).slice(0, 4096), reason instanceof Error && reason.name === 'TimeoutError' ? 'TimeoutError' : 'AbortError')
    }
    throw operationError(error, requestBody)
  } finally { unbind() }
}

/**
 * 等待异步操作完成（长超时）
 */
export async function waitForOperationLong(
  client: IncusClient,
  operationUrl: string,
  timeout: number = 600000,
  requestBody?: unknown
): Promise<unknown> {
  return waitForOperation(client, operationUrl, timeout, undefined, requestBody)
}

/**
 * 将 Incus 实例状态转换为系统状态
 * 注意：返回的状态必须与数据库 Instance.status 类型兼容
 * 有效状态: 'creating' | 'running' | 'stopped' | 'error' | 'deleted'
 */
export function mapInstanceStatus(incusStatus: string): string {
  const statusMap: Record<string, string> = {
    'Running': 'running',
    'Stopped': 'stopped',
    'Frozen': 'stopped',     // Frozen 映射为 stopped（对用户来说都是不可用状态）
    'Error': 'error',
    'Starting': 'creating',  // Starting 映射为 creating（启动中）
    'Stopping': 'running'    // Stopping 映射为 running（还未完全停止，等下次同步）
  }
  return statusMap[incusStatus] || 'unknown'
}

