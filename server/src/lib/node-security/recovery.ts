export interface ResumeInstance { project: string; name: string; uuid: string; frozen: boolean }
export interface RecoveryState { locked: boolean; revision: number; resumeInstances?: ResumeInstance[] }
export interface RecoveryClient { request<T = unknown>(method: string, path: string, body?: unknown): Promise<T> }

// The node retains pending identities until native async operations and their final state are verified.
export async function restoreNodeInstances(client: RecoveryClient, initial: RecoveryState, allowed: (item: ResumeInstance) => Promise<boolean>, options: { progress?: (completed: number, total: number, failed: number) => void; cancelled?: () => boolean } = {}): Promise<{ state: RecoveryState; failed: number }> {
  let state = initial
  let failed = 0
  if (state.locked || !Number.isSafeInteger(state.revision) || !Array.isArray(state.resumeInstances)) throw new Error('invalid recovery state')
  let completed = 0
  for (const item of initial.resumeInstances!) {
    if (options.cancelled?.()) throw new Error('节点恢复任务已取消')
    try {
      if (!item || typeof item.project !== 'string' || !item.project || typeof item.name !== 'string' || !item.name || typeof item.uuid !== 'string' || !item.uuid || typeof item.frozen !== 'boolean') throw new Error('invalid workload identity')
      const query = `?project=${encodeURIComponent(item.project)}`
      const base = `/1.0/instances/${encodeURIComponent(item.name)}`
      const acknowledged = async () => { state = await client.request<RecoveryState>('POST', '/1.0/payincus/security/resume', { revision: state.revision, instance: item }) }
      if (!await allowed(item)) { await acknowledged(); continue }
      const instance = await client.request<{ config?: Record<string, string>; type?: string }>('GET', base + query)
      if (instance.type !== 'container' || instance.config?.['volatile.uuid'] !== item.uuid) throw new Error('workload identity changed')
      const readState = () => client.request<{ status: string }>('GET', `${base}/state${query}`)
      let current = await readState()
      if (current.status === 'Stopped') {
        // IncusClient waits for the native asynchronous operation before returning.
        await client.request('PUT', `${base}/state${query}`, { action: 'start', timeout: 30, force: false, stateful: false })
        current = await readState()
      }
      if (item.frozen && current.status === 'Running') {
        await client.request('PUT', `${base}/state${query}`, { action: 'freeze', timeout: 30, force: false, stateful: false })
        current = await readState()
      }
      if (current.status !== (item.frozen ? 'Frozen' : 'Running')) throw new Error('workload did not recover')
      await acknowledged()
    } catch {
      failed++
      if (options.cancelled?.()) throw new Error('节点恢复任务已取消')
    } finally { options.progress?.(++completed, initial.resumeInstances!.length, failed) }
  }
  return { state, failed }
}
