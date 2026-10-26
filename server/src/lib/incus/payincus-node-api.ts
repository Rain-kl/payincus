import type { IncusClient } from './incus-client.js'
import type { ControlMethod } from './tunnel-manager.js'

export type NodeAPIRequester = (hostId: number, method: ControlMethod, params: unknown, signal: AbortSignal) => Promise<unknown>

// All node controls use the same mTLS Incus client as instance operations.
export const requestNodeAPI: NodeAPIRequester = async (hostId, method, params, signal) => {
  const [{ prisma }, { getIncusClient }] = await Promise.all([
    import('../../db/prisma.js'), import('./incus-pool.js')
  ])
  signal.throwIfAborted()
  const host = await prisma.host.findUnique({ where: { id: hostId } })
  if (!host) throw new Error('Node not found')
  const client = await getIncusClient(host)
  signal.throwIfAborted()
  return callNodeAPI(client, method, params, signal)
}

export async function callNodeAPI(client: Pick<IncusClient, 'request'>, method: ControlMethod, params: unknown, signal: AbortSignal) {
  signal.throwIfAborted()
  const info = await client.request<{ api_extensions: string[] }>('GET', '/1.0/payincus', null, undefined, signal)
  if (!Array.isArray(info?.api_extensions) || !info.api_extensions.includes('payincus_node_v1')) throw new Error('Node API extension unavailable')
  return client.request(method === 'proxy.probe' ? 'POST' : 'PUT', method === 'proxy.probe' ? '/1.0/payincus/proxy-probes' : '/1.0/payincus/network', params, undefined, signal)
}
