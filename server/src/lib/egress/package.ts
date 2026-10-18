import type { Prisma } from '@prisma/client'
import { prisma } from '../../db/prisma.js'
import { hostTunnelManager } from '../incus/tunnel-manager.js'
import { NetworkInputError, defaultEgress, defaultFirewall } from './policy.js'
import { decodeProxyConfig } from './sync.js'

export type PackageEgressMode = 'legacy' | 'direct' | 'profile'
export type EgressSnapshot = { egressMode: string; egressProfileVersionId: number | null }
type DB = Prisma.TransactionClient | typeof prisma
export const MAX_EGRESS_GROUPS = 16

export function normalizePackageEgress(input: Record<string, unknown>): Partial<EgressSnapshot> {
  const result: Partial<EgressSnapshot> = {}
  if (input.egressMode !== undefined) {
    if (!['legacy', 'direct', 'profile'].includes(String(input.egressMode))) throw new NetworkInputError('Invalid package egress mode')
    result.egressMode = String(input.egressMode)
  }
  if (input.egressProfileVersionId !== undefined) {
    const id = input.egressProfileVersionId
    if (id !== null && (!Number.isSafeInteger(id) || Number(id) <= 0 || Number(id) > 2147483647)) throw new NetworkInputError('Invalid egress profile version')
    result.egressProfileVersionId = id as number | null
  }
  return result
}

export async function validatePackageEgress(db: DB, input: Partial<EgressSnapshot> & { hostIds: number[]; networkMode?: string; instanceType?: string; privileged?: boolean }): Promise<void> {
  const mode = input.egressMode ?? 'legacy'
  if (!['legacy', 'direct', 'profile'].includes(mode)) throw new NetworkInputError('Invalid package egress mode')
  if (mode !== 'profile' && input.egressProfileVersionId != null) throw new NetworkInputError('Only proxy mode may select an egress profile')
  if (mode === 'legacy') return
  if ((input.networkMode ?? 'nat') !== 'nat' || (input.instanceType ?? 'container') !== 'container' || input.privileged) {
    throw new NetworkInputError('Package egress currently requires an unprivileged IPv4 NAT container')
  }
  if (mode === 'profile') {
    const version = input.egressProfileVersionId ? await db.egressProfileVersion.findUnique({ where: { id: input.egressProfileVersionId }, include: { profile: true } }) : null
    if (!version?.profile.active || input.hostIds.some(id => !version.allowedHostIds.includes(id))) throw new NetworkInputError('Egress profile is inactive or not permitted on the selected hosts')
  }
}

// Read through the caller's transaction: charged purchases and their network snapshot commit together.
export async function snapshotPackageEgress(db: DB, packageId: number | null, hostId: number, sourceId?: number): Promise<EgressSnapshot> {
  const source = sourceId ? await db.instance.findUnique({ where: { id: sourceId }, select: { egressMode: true, egressProfileVersionId: true } }) : null
  const pkg = packageId ? await db.package.findUnique({ where: { id: packageId }, select: { egressMode: true, egressProfileVersionId: true, networkMode: true, instanceType: true, privileged: true } }) : null
  if ((sourceId && !source) || (packageId && !pkg && !source)) throw new NetworkInputError('Egress snapshot source no longer exists')
  const snapshot = source ?? pkg ?? { egressMode: 'legacy', egressProfileVersionId: null }
  if (snapshot.egressMode !== 'legacy') {
    if (!hostTunnelManager.supportsControl(hostId, 'network.apply.v2')) throw new NetworkInputError('The host needs an online Agent supporting package egress')
    if (!source) await validatePackageEgress(db, { ...pkg!, hostIds: [hostId] })
    if (source?.egressProfileVersionId) {
      const version = await db.egressProfileVersion.findUnique({ where: { id: source.egressProfileVersionId } })
      if (!version?.allowedHostIds.includes(hostId)) throw new NetworkInputError('The instance egress profile is not permitted on the target host')
    }
  }
  return { egressMode: snapshot.egressMode, egressProfileVersionId: snapshot.egressProfileVersionId }
}

export async function packageEgressInstruction(hostId: number) {
  const attachments = await prisma.instanceEgressAttachment.findMany({ where: { hostId }, include: { instance: { select: { status: true } } }, orderBy: { id: 'asc' } })
  const active = attachments.filter(binding => binding.instance.status !== 'deleted')
  const versionIds = [...new Set(active.flatMap(binding => binding.profileVersionId ? [binding.profileVersionId] : []))]
  if (versionIds.length > MAX_EGRESS_GROUPS) throw new NetworkInputError('Host package egress group capacity exceeded')
  const versions = await prisma.egressProfileVersion.findMany({ where: { id: { in: versionIds } }, include: { proxyNode: true } })
  return {
    schemaVersion: 2,
    groups: versions.map(version => ({ key: `v${version.id}`, egress: { ...defaultEgress, mode: 'proxy', strategy: 'balance', proxyNodeIds: [version.proxyNodeId] }, nodes: [{ id: version.proxyNodeId, outbound: decodeProxyConfig(version.proxyNode.configEncrypted) }] })),
    bindings: active.map(binding => ({ instanceName: binding.incusName, generation: binding.generation, mode: binding.mode, groupKey: binding.profileVersionId ? `v${binding.profileVersionId}` : '', nics: binding.nics }))
  }
}

export async function bumpEgressHost(db: DB, hostId: number): Promise<void> {
  await db.hostNetworkPolicy.upsert({ where: { hostId }, create: { hostId, egress: defaultEgress as unknown as Prisma.InputJsonValue, firewall: defaultFirewall as unknown as Prisma.InputJsonValue }, update: { revision: { increment: 1 } } })
}
