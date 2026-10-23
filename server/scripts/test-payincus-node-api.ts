import assert from 'node:assert/strict'
import { callNodeAPI } from '../src/lib/incus/payincus-node-api.js'
import type { IncusClient } from '../src/lib/incus/incus-client.js'

for (const method of ['network.apply', 'network.apply.v2', 'proxy.probe'] as const) {
  const requests: unknown[][] = []
  const client: Pick<IncusClient, 'request'> = {
    async request<T>(...args: any[]) {
      requests.push(args)
      return (requests.length === 1 ? { api_extensions: ['payincus_node_v1'] } : { appliedRevision: 8 }) as T
    }
  }
  const controller = new AbortController()
  const input = { schemaVersion: 2, revision: 8 }
  assert.deepEqual(await callNodeAPI(client, method, input, controller.signal), { appliedRevision: 8 })
  assert.deepEqual(requests[0], ['GET', '/1.0/payincus', null, undefined, controller.signal])
  assert.deepEqual(requests[1], [method === 'proxy.probe' ? 'POST' : 'PUT', method === 'proxy.probe' ? '/1.0/payincus/proxy-probes' : '/1.0/payincus/network', input, undefined, controller.signal])
}
let writes = 0
const missing: Pick<IncusClient, 'request'> = {
  async request<T>(method: string) {
    if (method !== 'GET') writes++
    return { api_extensions: [] } as T
  }
}
await assert.rejects(callNodeAPI(missing, 'network.apply.v2', {}, new AbortController().signal), /extension unavailable/)
assert.equal(writes, 0)
const denied: Pick<IncusClient, 'request'> = { async request() { throw Object.assign(new Error('denied'), { statusCode: 403 }) } }
await assert.rejects(callNodeAPI(denied, 'network.apply', {}, new AbortController().signal), { statusCode: 403 })
const cancelled = new AbortController()
cancelled.abort()
await assert.rejects(callNodeAPI(missing, 'proxy.probe', {}, cancelled.signal), { name: 'AbortError' })
console.log('PayIncus node API capability, request, authorization and cancellation contracts passed')

const malformed: Pick<IncusClient, 'request'> = { async request<T>() { return { api_extensions: 'payincus_node_v1' } as T } }
await assert.rejects(callNodeAPI(malformed, 'network.apply.v2', {}, new AbortController().signal), /extension unavailable/)
