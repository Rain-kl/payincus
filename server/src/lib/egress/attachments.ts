import { createHash } from 'node:crypto'
import { isIP } from 'node:net'
import type { Prisma } from '@prisma/client'
import { prisma } from '../../db/prisma.js'
import type { IncusClient } from '../incus/incus-client.js'
import { hostTunnelManager } from '../incus/tunnel-manager.js'
import { bumpEgressHost, MAX_EGRESS_GROUPS } from './package.js'
import { pushNetworkPolicy } from './realtime.js'
import { taskLog } from '../operation-task-log.js'

interface Config { type?: string; name?: string; config?: Record<string, string>; devices?: Record<string, Record<string, string>>; profiles?: string[] }
export async function preparePackageEgress(client: IncusClient, value: unknown, instanceId?: number): Promise<unknown> {
  if (!client.options.hostId || !value || typeof value !== 'object') return value
  const original = value as Config
  if (!original.name) return value
  const instance = instanceId
    ? await prisma.instance.findUnique({ where: { id: instanceId } })
    : await prisma.instance.findFirst({ where: { hostId: client.options.hostId, incusId: original.name } })
  if (!instance || instance.egressMode === 'host') return value
  const hostId = client.options.hostId
  if (original.type === 'virtual-machine' || original.config?.['security.privileged'] === 'true') throw new Error('Package egress requires an unprivileged container')
  if (instance.networkMode !== 'nat') throw new Error('Package egress currently requires IPv4 NAT')
  if (!hostTunnelManager.supportsControl(hostId, 'network.apply.v2')) throw new Error('Upgrade the online host Agent to support package egress')
  if (instance.egressProfileId) {
    const profile = await prisma.egressProfile.findUnique({ where: { id: instance.egressProfileId } })
    if (!profile?.active || !profile.allowedHostIds.includes(hostId)) throw new Error('Instance egress is not permitted on this host')
  }
  const config = structuredClone(original)
  if (!config.name) throw new Error('Missing instance name for egress binding')
  config.config ??= {}
  config.config['user.incudal.egress-instance'] = String(instance.id)
  config.devices ??= {}
  const expanded: Record<string, Record<string, string>> = {}
  for (const profile of config.profiles ?? ['default']) {
    const data = await client.request<{ devices?: Record<string, Record<string, string>> }>('GET', `/1.0/profiles/${encodeURIComponent(profile)}`)
    Object.assign(expanded, data.devices)
  }
  Object.assign(expanded, config.devices)
  const nics: Array<{ hostName: string; ipv4: string; bridge: string }> = []
  for (const [key, source] of Object.entries(expanded)) {
    if (source.type !== 'nic') continue
    if (source.nictype && source.nictype !== 'bridged') throw new Error('Package egress only supports bridged NAT NICs')
    if (isIP(source['ipv4.address'] || '') !== 4) throw new Error('A static IPv4 allocation is required for package egress')
    const hostName = `ipg${createHash('sha256').update(`${config.name}/${key}`).digest('hex').slice(0, 11)}`
    config.devices[key] = { ...source, host_name: hostName, 'security.ipv4_filtering': 'true', 'security.mac_filtering': 'true', 'ipv6.address': 'none' }
    nics.push({ hostName, ipv4: source['ipv4.address'], bridge: source.parent || source.network })
  }
  if (!nics.length || nics.length > 4) throw new Error('Invalid package egress NIC inventory')
  const attachment = await prisma.$transaction(async tx => {
    await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
    await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74202, ${hostId})`
    const groups = await tx.instanceEgressAttachment.findMany({ where: { hostId, profileId: { not: null }, instance: { status: { not: 'deleted' } } }, select: { profileId: true }, distinct: ['profileId'] })
    if (instance.egressProfileId && !groups.some(group => group.profileId === instance.egressProfileId) && groups.length >= MAX_EGRESS_GROUPS) throw new Error('Host package egress group capacity exceeded')
    const binding = await tx.instanceEgressAttachment.upsert({ where: { hostId_incusName: { hostId, incusName: config.name! } }, create: { hostId, instanceId: instance.id, incusName: config.name!, mode: instance.egressMode, profileId: instance.egressProfileId, nics: nics as unknown as Prisma.InputJsonValue }, update: { instanceId: instance.id, mode: instance.egressMode, profileId: instance.egressProfileId, nics: nics as unknown as Prisma.InputJsonValue, generation: { increment: 1 } } })
    await bumpEgressHost(tx, hostId)
    return binding
  })
  taskLog('info', `应用实例出口绑定 ${config.name} generation=${attachment.generation}`)
  const applied = await pushNetworkPolicy(hostId)
  const bindings = applied?.status.bindings as Array<{ instanceName: string; generation: number }> | undefined
  const groups = applied?.status.groups as Array<{ key: string; state: string; error: string }> | undefined
  const group = groups?.find(group => group.key === `p${attachment.profileId}`)
  if (!bindings?.some(binding => binding.instanceName === config.name && binding.generation === attachment.generation) || (attachment.profileId && !groups?.some(group => group.key === `p${attachment.profileId}` && group.state === 'ready'))) {
    throw new Error(`The instance egress binding is not ready${group ? ` (${group.key}: ${group.state}/${group.error})` : ''}; outbound access remains blocked`)
  }
  return config
}

export async function ensurePackageEgressBeforeStart(client: IncusClient, name: string): Promise<void> {
  if (!client.options?.hostId) return
  const exact = await prisma.instance.findFirst({ where: { hostId: client.options.hostId, incusId: name }, select: { id: true, egressMode: true } })
  if (exact?.egressMode === 'host') return
  const current = await client.request<Config>('GET', `/1.0/instances/${encodeURIComponent(name)}`)
  const id = exact?.id ?? Number(current.config?.['user.incudal.egress-instance'])
  if (!Number.isSafeInteger(id) || id <= 0) return
  const input = { name, type: current.type, config: current.config, devices: current.devices, profiles: current.profiles }
  const next = await preparePackageEgress(client, input, id) as Config
  if (next === input) return
  await client.request('PUT', `/1.0/instances/${encodeURIComponent(name)}`, { config: next.config, devices: next.devices, profiles: next.profiles })
}

export async function releasePackageEgress(client: IncusClient, name: string): Promise<void> {
  if (!client.options.hostId) return
  try {
    const hostId = client.options.hostId
    const changed = await prisma.$transaction(async tx => {
      await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74201)`
    await tx.$queryRaw`SELECT 1 AS locked FROM pg_advisory_xact_lock(74202, ${hostId})`
      const result = await tx.instanceEgressAttachment.deleteMany({ where: { hostId, incusName: name } })
      if (result.count) await bumpEgressHost(tx, hostId)
      return result.count
    })
    if (changed) await pushNetworkPolicy(hostId)
  } catch { taskLog('warn', '出口绑定清理等待节点重连后同步') }
}
