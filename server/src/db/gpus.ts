import { Prisma } from '@prisma/client'
import { prisma } from './prisma.js'
import { GpuError, GPU_SHARING_NOTICE, isGpuFresh, resolveGpuPolicy, type GpuPolicy } from '../lib/gpu.js'
type Client = Prisma.TransactionClient | typeof prisma

export async function getGpuPolicy(client: Client, packageId?: number | null, planId?: number | null): Promise<GpuPolicy> {
  if (!packageId) return { gpuShared: false, gpuDeviceIds: [] }
  const pkg = await client.package.findUnique({ where: { id: packageId } })
  if (!pkg) throw new GpuError('套餐不存在')
  const plan = planId ? await client.packagePlan.findUnique({ where: { id: planId } }) : null
  if (planId && (!plan || plan.packageId !== packageId)) throw new GpuError('方案不属于该套餐')
  const policy = resolveGpuPolicy(pkg, plan)
  if (policy.gpuShared && (pkg.instanceType !== 'container' || pkg.privileged || !policy.gpuDeviceIds.length)) throw new GpuError('共享 GPU 必须使用非特权 LXC 套餐并选择显卡')
  return policy
}

export async function validateGpuSelection(client: Client, input: GpuPolicy & { instanceType: string; privileged: boolean; hostIds: number[] }): Promise<void> {
  if (!input.gpuShared) return
  if (input.instanceType !== 'container' || input.privileged) throw new GpuError('共享 GPU 仅支持非特权 LXC 容器')
  if (!input.gpuDeviceIds.length) throw new GpuError('请选择至少一张共享 GPU')
  const devices = await client.hostGpu.findMany({ where: { id: { in: input.gpuDeviceIds }, hostId: { in: input.hostIds }, sharingEnabled: true } })
  if (devices.length !== input.gpuDeviceIds.length) throw new GpuError('GPU 不属于套餐绑定的宿主机，或尚未开放共享')
}

export async function findAvailableGpu(client: Client, hostId: number, policy: GpuPolicy, excludeInstanceId?: number) {
  if (!policy.gpuShared) return null
  const devices = await client.hostGpu.findMany({ where: { hostId, id: { in: policy.gpuDeviceIds }, sharingEnabled: true }, orderBy: { id: 'asc' } })
  for (const device of devices) {
    if (!isGpuFresh(device)) continue
    const used = await client.instance.count({ where: { gpuDeviceId: device.id, gpuReleasedAt: null, ...(excludeInstanceId ? { id: { not: excludeInstanceId } } : {}) } })
    if (used < device.maxInstances) return device
  }
  return null
}

export async function reserveInstanceGpu(client: Client, input: { hostId: number; packageId?: number | null; planId?: number | null }): Promise<{ gpuDeviceId?: number }> {
  const policy = await getGpuPolicy(client, input.packageId, input.planId)
  if (!policy.gpuShared) return {}
  await client.$queryRaw`SELECT id FROM host_gpus WHERE host_id = ${input.hostId} ORDER BY id FOR UPDATE`
  const gpu = await findAvailableGpu(client, input.hostId, policy)
  if (!gpu) throw new GpuError('GPU_RESOURCES_INSUFFICIENT: 所选 GPU 不可用、监控已过期或共享名额已满')
  return { gpuDeviceId: gpu.id }
}

// A stopped/suspended/expired instance still owns its seat. Recreate uses its
// original board, never silently changes to the other card or falls back to CPU.
export async function getInstanceGpuPci(instanceId: number, hostId: number): Promise<string | undefined> {
  return prisma.$transaction(async tx => {
    const instance = await tx.instance.findUnique({ where: { id: instanceId } })
    if (!instance?.gpuDeviceId) return undefined
    await tx.$queryRaw`SELECT id FROM host_gpus WHERE id = ${instance.gpuDeviceId} FOR UPDATE`
    const gpu = await tx.hostGpu.findUnique({ where: { id: instance.gpuDeviceId } })
    if (!gpu || gpu.hostId !== hostId || !isGpuFresh(gpu)) throw new GpuError('绑定的 GPU 已离线或不在目标宿主机上，请检查宿主 GPU 状态')
    if (instance.gpuReleasedAt) {
      const used = await tx.instance.count({ where: { gpuDeviceId: gpu.id, gpuReleasedAt: null, id: { not: instanceId } } })
      if (!gpu.sharingEnabled || used >= gpu.maxInstances) throw new GpuError('GPU 共享名额不足，无法重新创建实例')
      await tx.instance.update({ where: { id: instanceId }, data: { gpuReleasedAt: null } })
    }
    return gpu.pciAddress
  })
}

export async function releaseFailedGpu(instanceId: number): Promise<void> {
  await prisma.instance.updateMany({ where: { id: instanceId, status: { in: ['error', 'deleted'] }, gpuReleasedAt: null }, data: { gpuReleasedAt: new Date() } })
}

export async function assertGpuPlanChange(client: Client, instanceId: number, planId: number): Promise<void> {
  const instance = await client.instance.findUnique({ where: { id: instanceId } })
  if (!instance) return
  const policy = await getGpuPolicy(client, instance.packageId, planId)
  if (policy.gpuShared !== Boolean(instance.gpuDeviceId) || (instance.gpuDeviceId && !policy.gpuDeviceIds.includes(instance.gpuDeviceId))) throw new GpuError('该方案会改变 GPU 绑定，请新购所需 GPU 方案；已有实例仅支持保留原显卡的方案变更')
}

export async function assertGpuMigration(instanceId: number): Promise<void> {
  const instance = await prisma.instance.findUnique({ where: { id: instanceId }, select: { gpuDeviceId: true } })
  if (instance?.gpuDeviceId) throw new GpuError('共享 GPU 实例不支持跨宿主迁移或克隆，请在目标宿主新购 GPU 实例')
}

function measurement(value: unknown, max = 1e9): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= max ? value : null
}

export async function syncHostGpuReport(hostId: number, input: unknown): Promise<void> {
  if (!input || typeof input !== 'object' || Array.isArray(input)) return // old agents remain compatible
  const report = input as Record<string, unknown>
  if (!['ready', 'error', 'unavailable'].includes(String(report.status))) return
  const cards = report.status === 'ready' && Array.isArray(report.cards) ? report.cards.slice(0, 64) : []
  const seen = new Set<string>()
  const pciSeen = new Set<string>()
  const valid: Array<{ uuid: string; pciAddress: string; name: string; memoryTotalMiB: number; metrics: Prisma.InputJsonObject }> = []
  for (const value of cards) {
    if (!value || typeof value !== 'object') continue
    const card = value as Record<string, unknown>
    if (typeof card.uuid !== 'string' || !/^GPU-[a-fA-F0-9-]{16,64}$/.test(card.uuid) || typeof card.pciAddress !== 'string' || !/^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$/.test(card.pciAddress) || seen.has(card.uuid) || pciSeen.has(card.pciAddress)) continue
    const total = measurement(card.memoryTotalMiB, 10_000_000)
    if (!total || !Number.isSafeInteger(total)) continue
    seen.add(card.uuid); pciSeen.add(card.pciAddress)
    valid.push({ uuid: card.uuid, pciAddress: card.pciAddress, name: String(card.name || 'NVIDIA GPU').slice(0, 128), memoryTotalMiB: total, metrics: {
      memoryUsedMiB: measurement(card.memoryUsedMiB, total), gpuUtilization: measurement(card.gpuUtilization, 100), memoryUtilization: measurement(card.memoryUtilization, 100), temperature: measurement(card.temperature, 150), powerDrawWatts: measurement(card.powerDrawWatts), powerLimitWatts: measurement(card.powerLimitWatts), driverVersion: String(card.driverVersion || '').slice(0, 80), computeMode: String(card.computeMode || '').slice(0, 80)
    } })
  }
  await prisma.$transaction(async tx => {
    await tx.hostGpu.updateMany({ where: { hostId }, data: { available: false, runtimeReady: false } })
    for (const card of valid) await tx.hostGpu.upsert({ where: { hostId_uuid: { hostId, uuid: card.uuid } }, create: { hostId, ...card, available: true, runtimeReady: report.runtimeReady === true && card.metrics.computeMode === 'Default', lastSeenAt: new Date() }, update: { ...card, available: true, runtimeReady: report.runtimeReady === true && card.metrics.computeMode === 'Default', lastSeenAt: new Date() } })
  })
}

export async function listHostGpus(hostId: number) {
  const devices = await prisma.hostGpu.findMany({ where: { hostId }, orderBy: { id: 'asc' } })
  return Promise.all(devices.map(async gpu => {
    const usedInstances = await prisma.instance.count({ where: { gpuDeviceId: gpu.id, gpuReleasedAt: null } })
    return { ...gpu, ready: isGpuFresh(gpu), usedInstances, availableSlots: Math.max(0, gpu.maxInstances - usedInstances), sharingNotice: GPU_SHARING_NOTICE }
  }))
}

export async function getGpuOffersForPackages(packageIds: number[]): Promise<Map<number, GpuPolicy[]>> {
  const packages = await prisma.package.findMany({ where: { id: { in: packageIds } }, select: { id: true, gpuShared: true, gpuDeviceIds: true, plans: { where: { isActive: true }, select: { gpuShared: true, gpuDeviceIds: true, isSoldOut: true } } } })
  return new Map(packages.map(pkg => [pkg.id, pkg.plans.length ? pkg.plans.filter(plan => !plan.isSoldOut).map(plan => resolveGpuPolicy(pkg, plan)) : [pkg]]))
}
export async function gpuOffersAvailableOnHost(hostId: number, offers: GpuPolicy[]): Promise<boolean> {
  for (const policy of offers) if (!policy.gpuShared || await findAvailableGpu(prisma, hostId, policy)) return true
  return false
}
export async function gpuPlanAvailable(packageId: number, planId: number): Promise<boolean> {
  const policy = await getGpuPolicy(prisma, packageId, planId)
  if (!policy.gpuShared) return true
  const bindings = await prisma.packageHost.findMany({ where: { packageId, host: { status: 'online' } }, select: { hostId: true } })
  for (const binding of bindings) if (await findAvailableGpu(prisma, binding.hostId, policy)) return true
  return false
}

/** Only call before the provisioner is launched: no Incus object can exist yet. */
export async function compensateGpuPreparationFailure(instanceId: number): Promise<void> {
  const instance = await prisma.instance.findUnique({ where: { id: instanceId } })
  if (!instance?.gpuDeviceId) return
  const claimed = await prisma.instance.updateMany({ where: { id: instanceId, status: 'creating' }, data: { status: 'error' } })
  if (!claimed.count) return
  const { rollbackResources } = await import('./quota-operations.js')
  const { releasePublicIpv4ForInstance } = await import('./public-ipv4.js')
  const { compensateFailedInstancePurchase } = await import('./instance-provisioning-compensation.js')
  const { networkModeAllowsPortMapping } = await import('../lib/network-modes.js')
  const operations = await Promise.allSettled([
    rollbackResources({ hostId: instance.hostId, cpu: instance.cpu, memory: instance.memory, disk: instance.disk, portCount: networkModeAllowsPortMapping(instance.networkMode) ? (instance.portLimit ?? 0) : 0 }),
    prisma.$transaction(tx => releasePublicIpv4ForInstance(tx, instanceId)),
    compensateFailedInstancePurchase(instanceId, instance.userId, instance.hostId),
    releaseFailedGpu(instanceId)
  ])
  for (const result of operations) if (result.status === 'rejected') console.error('[GPU] preparation compensation failed', { instanceId, error: result.reason })
}
