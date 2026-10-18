import { preparePackageEgress } from './attachments.js'
import { createHash } from 'node:crypto'
import { prisma } from '../../db/prisma.js'
import type { IncusClient } from '../incus/incus-client.js'
import type { EgressPolicy, FirewallPolicy } from './policy.js'

// Apply protection before a new NIC can send its first packet. Network ACLs
// cover Internet traffic; per-NIC ACLs also cover same-bridge communication.
export async function protectProvisionedNetwork(client: IncusClient, value: unknown, instanceId?: number): Promise<unknown> {
  if (!client.options.hostId || !value || typeof value !== 'object') return value
  const originalValue = value
  value = await preparePackageEgress(client, value, instanceId)
  const packageManaged = value !== originalValue
  const policy = await prisma.hostNetworkPolicy.findUnique({ where: { hostId: client.options.hostId } })
  if (!policy) return value
  const firewall = policy.firewall as unknown as FirewallPolicy, egress = policy.egress as unknown as EgressPolicy
  if (!firewall.enabled && (egress.mode === 'off' || packageManaged)) return value
  const status = policy.status as Record<string, unknown>
  if (policy.appliedRevision !== policy.revision || status.state !== 'applied') throw new Error('Host network policy is pending or failed; retry after the Agent confirms application')
  const config = structuredClone(value) as { name?: string; config?: Record<string, string>; devices?: Record<string, Record<string, string>>; profiles?: string[] }
  const devices = config.devices ?? {}
  const expanded: Record<string, Record<string, string>> = {}
  for (const name of config.profiles ?? ['default']) {
    const profile = await client.request<{ devices: Record<string, Record<string, string>> }>('GET', `/1.0/profiles/${encodeURIComponent(name)}`)
    Object.assign(expanded, profile.devices)
  }
  Object.assign(expanded, devices)
  for (const [name, original] of Object.entries(expanded)) {
    if (original.type !== 'nic') continue
    const device = { ...original }
    if (device.nictype === 'routed') {
      device.host_name = `iceg${createHash('sha256').update(`${config.name}/${name}`).digest('hex').slice(0, 10)}`
    } else if (device.network || device.nictype === 'bridged') {
      if (firewall.enabled) {
        device['security.acls'] = [...new Set([...(device['security.acls'] ?? '').split(',').filter(Boolean), 'incudal-host-firewall'])].join(',')
        config.config ??= {}
        for (const direction of ['ingress', 'egress'] as const) {
          const key = `security.acls.default.${direction}.action`
          config.config[`user.incudal.acl.nic.${name}.${direction}`] = device[key] || 'unset'
          device[key] = direction === 'ingress' ? firewall.ingressAction : firewall.egressAction
        }
      }
    } else throw new Error('Unsupported NIC type for managed host network protection')
    devices[name] = device
  }
  config.devices = devices
  return config
}
