import type { Prisma } from '@prisma/client'
import { prisma } from '../../db/prisma.js'
import { hostTunnelManager } from '../incus/tunnel-manager.js'
import { NetworkInputError, defaultFirewall, validateEgress } from './policy.js'
import { decodeProxyConfig } from './sync.js'

export type PackageEgressMode = 'host' | 'direct' | 'profile'
export type EgressBinding = { egressMode: string; egressProfileId: number | null }
type DB = Prisma.TransactionClient | typeof prisma
export const MAX_EGRESS_GROUPS = 16
export function normalizePackageEgress(input: Record<string, unknown>): Partial<EgressBinding> {
  const result: Partial<EgressBinding> = {}
  if (input.egressMode !== undefined) {
    if (!['host', 'direct', 'profile'].includes(String(input.egressMode))) throw new NetworkInputError('Invalid package egress mode')
    result.egressMode = String(input.egressMode)
  }
  if (input.egressProfileId !== undefined) {
    const id = input.egressProfileId
    if (id !== null && (!Number.isSafeInteger(id) || Number(id) <= 0 || Number(id) > 2147483647)) throw new NetworkInputError('Invalid egress profile')
    result.egressProfileId = id as number | null
  }
  return result
}
export async function validatePackageEgress(db: DB, input: Partial<EgressBinding> & { hostIds: number[]; networkMode?: string; instanceType?: string; privileged?: boolean }): Promise<void> {
  const mode = input.egressMode ?? 'host'
  if (!['host', 'direct', 'profile'].includes(mode)) throw new NetworkInputError('Invalid package egress mode')
  if (mode !== 'profile' && input.egressProfileId != null) throw new NetworkInputError('Only proxy mode may select an egress profile')
  if (mode === 'host') return
  if ((input.networkMode ?? 'nat') !== 'nat' || (input.instanceType ?? 'container') !== 'container' || input.privileged) throw new NetworkInputError('Package egress currently requires an unprivileged IPv4 NAT container')
  if (mode === 'profile') {
    const profile = input.egressProfileId ? await db.egressProfile.findUnique({ where: { id: input.egressProfileId } }) : null
    if (!profile?.active || input.hostIds.some(id => !profile.allowedHostIds.includes(id))) throw new NetworkInputError('Egress profile is inactive or not permitted on the selected hosts')
  }
}
// Bind the shared policy identity in the same transaction as resource delivery; never copy its contents.
export async function bindPackageEgress(db: DB, packageId: number | null, hostId: number, sourceId?: number): Promise<EgressBinding> {
  await db.$executeRaw`SELECT pg_advisory_xact_lock(74201)`
  const source = sourceId ? await db.instance.findUnique({ where: { id: sourceId }, select: { egressMode: true, egressProfileId: true } }) : null
  const pkg = packageId ? await db.package.findUnique({ where: { id: packageId }, select: { egressMode: true, egressProfileId: true, networkMode: true, instanceType: true, privileged: true } }) : null
  if ((sourceId && !source) || (packageId && !pkg && !source)) throw new NetworkInputError('Egress binding source no longer exists')
  const binding = source ?? pkg ?? { egressMode: 'host', egressProfileId: null }
  if (binding.egressMode !== 'host') {
    if (!hostTunnelManager.supportsControl(hostId, 'network.apply.v2')) throw new NetworkInputError('The host needs an online Agent supporting package egress')
    if (!source) await validatePackageEgress(db, { ...pkg!, hostIds: [hostId] })
    if (source?.egressProfileId) {
      const profile = await db.egressProfile.findUnique({ where: { id: source.egressProfileId } })
      if (!profile?.active || !profile.allowedHostIds.includes(hostId)) throw new NetworkInputError('The instance egress profile is not permitted on the target host')
    }
  }
  return { egressMode: binding.egressMode, egressProfileId: binding.egressProfileId }
}
export async function resolveProfile(profile: { egress: Prisma.JsonValue; active: boolean; allowedHostIds: number[] }, hostId: number) {
  if (!profile.active || !profile.allowedHostIds.includes(hostId)) throw new NetworkInputError('Egress policy unavailable for this host')
  const egress = validateEgress(profile.egress)
  const nodes = await prisma.proxyNode.findMany({ where: { id: { in: egress.proxyNodeIds } } })
  const byId = new Map(nodes.map(node => [node.id, node]))
  return { egress, nodes: egress.proxyNodeIds.map(id => {
    const node = byId.get(id)
    if (!node) throw new NetworkInputError('Referenced proxy node is missing')
    return { id, outbound: decodeProxyConfig(node.configEncrypted) }
  }) }
}
export async function packageEgressInstruction(hostId: number) {
  const attachments = await prisma.instanceEgressAttachment.findMany({ where: { hostId }, include: { instance: { select: { status: true } } }, orderBy: { id: 'asc' } })
  const active = attachments.filter(binding => binding.instance.status !== 'deleted')
  const profileIds = [...new Set(active.flatMap(binding => binding.profileId ? [binding.profileId] : []))]
  if (profileIds.length > MAX_EGRESS_GROUPS) throw new NetworkInputError('Host package egress group capacity exceeded')
  const profiles = await prisma.egressProfile.findMany({ where: { id: { in: profileIds } } })
  if (profiles.length !== profileIds.length) throw new NetworkInputError('Referenced egress policy is missing')
  return {
    schemaVersion: 2,
    groups: await Promise.all(profiles.map(async profile => ({ key: `p${profile.id}`, ...await resolveProfile(profile, hostId) }))),
    bindings: active.map(binding => ({ instanceName: binding.incusName, generation: binding.generation, mode: binding.mode, groupKey: binding.profileId ? `p${binding.profileId}` : '', nics: binding.nics }))
  }
}
export async function bumpEgressHost(db: DB, hostId: number): Promise<void> {
  await db.hostNetworkPolicy.upsert({ where: { hostId }, create: { hostId, firewall: defaultFirewall as unknown as Prisma.InputJsonValue }, update: { revision: { increment: 1 } } })
}
