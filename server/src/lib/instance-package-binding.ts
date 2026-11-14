import { NetworkMode, Prisma } from '@prisma/client'
import { calculateCreateBilling } from '../db/billing-operations.js'
import { createLog, LogModule, LogResult } from '../db/logs.js'
import { calculateHostResourcesFromInstances, updateHostResources } from '../db/hosts.js'
import { prisma } from '../db/prisma.js'
import type { IncusClient } from './incus/incus-client.js'
import { getInstance, getInstanceState } from './incus/incus-instances.js'
import { bindPackageEgress } from './egress/package.js'
import { resolveInstanceTrafficLimitForHost } from './traffic-multiplier.js'
import { normalizePlanTrafficLimitSpeed } from '../services/traffic-bandwidth.js'
import type { IncusInstance } from '../types/incus.js'

export const IMPORT_PRESET_CONFIG_KEY = 'instance_import_package_presets'
const IMPORT_PRESET_LOCK_KEY = 74208
export const IMPORT_PRESET_TTL_MS = 30 * 24 * 60 * 60 * 1000
const MAX_VALID_DAYS = 36500

export class PackageBindingError extends Error {
  statusCode: number
  code: string

  constructor(message: string, statusCode = 400, code = 'PACKAGE_BINDING_INVALID') {
    super(message)
    this.name = 'PackageBindingError'
    this.statusCode = statusCode
    this.code = code
  }
}

export interface PackageBindingInput {
  packageId?: number | null
  planId?: number | null
  validDays?: number | null
  expiresAt?: string | null
}

export interface ImportPackagePlanOption {
  id: number
  name: string
  description: string | null
  billingCycle: number
  /** 单周期价格（元） */
  price: number
  cpu: number
  memory: number
  disk: number
  trafficLimit: string | null
  trafficLimitSpeed: string | null
  portLimit: number
  snapshotLimit: number
  backupLimit: number
  siteLimit: number
  isActive: boolean
  sortOrder: number
}

export interface ImportPackageOption {
  id: number
  name: string
  description: string | null
  instanceType: string
  networkMode: string
  cpuMax: number
  memoryMax: number
  diskMax: number
  monthlyTrafficLimit: string | null
  portLimit: number
  snapshotLimit: number
  backupLimit: number
  siteLimit: number
  isActive: boolean
  boundToHost: boolean
  plans: ImportPackagePlanOption[]
}

export interface PendingImportPreset {
  packageId?: number | null
  planId?: number | null
  packageName?: string | null
  planName?: string | null
  validDays?: number | null
  expiresAt?: string | null
  userId?: number | null
  displayName?: string | null
  createdAt: string
}

interface StoredTaskPreset {
  hostId: number
  incusName?: string | null
  filePath?: string | null
  preset: PendingImportPreset
}

export interface ImportPresetStore {
  version: 1
  byTask: Record<string, StoredTaskPreset>
  byUnassigned: Record<string, PendingImportPreset>
}

export interface ExtractedIncusMetrics {
  cpu: number
  memory: number
  disk: number
  storagePoolName: string | null
  ipv4: string | null
  ipv6: string | null
  networkMode: NetworkMode
  image: string
  status: 'running' | 'stopped'
  instanceType: 'container' | 'vm'
}

export interface ResolvedPackageBinding {
  packageId: number | null
  packagePlanId: number | null
  packageName: string | null
  planName: string | null
  billingPrice: Prisma.Decimal | null
  billingCycle: number | null
  expiresAt: Date | null
  portLimit: number
  snapshotLimit: number
  backupLimit: number
  siteLimit: number
  monthlyTrafficLimit: bigint | null
  limitsRead: string | null
  limitsWrite: string | null
  limitsReadIops: number | null
  limitsWriteIops: number | null
  limitsIngress: string | null
  limitsEgress: string | null
  limitsProcesses: number | null
  limitsCpuPriority: number | null
  bootAutostart: boolean
  bootAutostartPriority: number | null
  bootAutostartDelay: number | null
  bootHostShutdownTimeout: number | null
  swapEnabled: boolean
  swapSize: number | null
  egressMode: string
  egressProfileId: number | null
  hasLimitsTrafficSpeed: boolean
}

function emptyPresetStore(): ImportPresetStore {
  return { version: 1, byTask: {}, byUnassigned: {} }
}

function unassignedPresetKey(hostId: number, incusName: string): string {
  return `${hostId}:${incusName}`
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}

function parseOptionalPositiveInt(value: unknown): number | null {
  if (value === null || value === undefined) return null
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value <= 0) return null
  return value
}

function normalizePendingPreset(raw: unknown): PendingImportPreset | null {
  if (!isRecord(raw)) return null
  const createdAt = typeof raw.createdAt === 'string' && !Number.isNaN(Date.parse(raw.createdAt))
    ? raw.createdAt
    : new Date().toISOString()

  return {
    packageId: parseOptionalPositiveInt(raw.packageId),
    planId: parseOptionalPositiveInt(raw.planId),
    packageName: typeof raw.packageName === 'string' ? raw.packageName : null,
    planName: typeof raw.planName === 'string' ? raw.planName : null,
    validDays: parseOptionalPositiveInt(raw.validDays),
    expiresAt: typeof raw.expiresAt === 'string' && raw.expiresAt.trim() ? raw.expiresAt.trim() : null,
    userId: parseOptionalPositiveInt(raw.userId),
    displayName: typeof raw.displayName === 'string' && raw.displayName.trim() ? raw.displayName.trim() : null,
    createdAt
  }
}

/**
 * 规范化并清理过期的导入套餐预设存储（超过 30 天自动清理）。
 */
export function pruneAndNormalizePresetStore(raw: unknown, nowMs = Date.now()): ImportPresetStore {
  if (!isRecord(raw)) return emptyPresetStore()
  const store = emptyPresetStore()
  const cutoff = nowMs - IMPORT_PRESET_TTL_MS

  if (isRecord(raw.byTask)) {
    for (const [taskId, entry] of Object.entries(raw.byTask)) {
      if (!taskId || !isRecord(entry)) continue
      const hostId = parseOptionalPositiveInt(entry.hostId)
      const preset = normalizePendingPreset(entry.preset)
      if (!hostId || !preset) continue
      const createdMs = Date.parse(preset.createdAt)
      if (Number.isFinite(createdMs) && createdMs < cutoff) continue
      store.byTask[taskId] = {
        hostId,
        incusName: typeof entry.incusName === 'string' && entry.incusName.trim() ? entry.incusName.trim() : null,
        filePath: typeof entry.filePath === 'string' && entry.filePath.trim() ? entry.filePath.trim() : null,
        preset
      }
    }
  }

  if (isRecord(raw.byUnassigned)) {
    for (const [key, entry] of Object.entries(raw.byUnassigned)) {
      if (!key) continue
      const preset = normalizePendingPreset(entry)
      if (!preset) continue
      const createdMs = Date.parse(preset.createdAt)
      if (Number.isFinite(createdMs) && createdMs < cutoff) continue
      store.byUnassigned[key] = preset
    }
  }

  return store
}

/**
 * 解析并校验导入/绑定时的自定义到期时间或有效天数。
 */
export function resolveExplicitExpiresAt(options: {
  validDays?: number | null
  expiresAt?: string | null
  defaultExpiresAt: Date | null
  now?: Date
}): Date | null {
  const now = options.now ?? new Date()
  if (typeof options.expiresAt === 'string' && options.expiresAt.trim() !== '') {
    const parsed = new Date(options.expiresAt.trim())
    if (Number.isNaN(parsed.getTime())) {
      throw new PackageBindingError('到期时间格式无效', 400, 'INVALID_EXPIRES_AT')
    }
    return parsed
  }

  if (options.validDays !== undefined && options.validDays !== null) {
    if (!Number.isInteger(options.validDays) || options.validDays < 1 || options.validDays > MAX_VALID_DAYS) {
      throw new PackageBindingError(`有效天数必须为 1 到 ${MAX_VALID_DAYS} 之间的整数`, 400, 'INVALID_VALID_DAYS')
    }
    return new Date(now.getTime() + options.validDays * 24 * 60 * 60 * 1000)
  }

  return options.defaultExpiresAt
}

function normalizeIncusSizeToMB(raw: unknown, fallbackMB: number): number {
  if (typeof raw !== 'string') return fallbackMB
  const match = raw.trim().match(/^(\d+(?:\.\d+)?)\s*([kmgt]i?b?)?$/i)
  if (!match) return fallbackMB
  const num = Number(match[1])
  if (!Number.isFinite(num) || num <= 0) return fallbackMB
  const unit = (match[2] || 'b').toLowerCase()
  const factors: Record<string, number> = {
    b: 1 / (1024 * 1024),
    k: 1 / 1024,
    kb: 1 / 1000,
    kib: 1 / 1024,
    m: 1,
    mb: 1,
    mib: 1,
    g: 1024,
    gb: 1000,
    gib: 1024,
    t: 1024 * 1024,
    tb: 1000 * 1000,
    tib: 1024 * 1024
  }
  const factor = factors[unit] ?? 1
  return Math.max(1, Math.round(num * factor))
}

function normalizeIncusCpuAllowance(config: Record<string, unknown>): number {
  const rawAllowance = typeof config['limits.cpu.allowance'] === 'string' ? config['limits.cpu.allowance'].trim() : ''
  if (rawAllowance.endsWith('%')) {
    const pct = Number(rawAllowance.slice(0, -1))
    if (Number.isFinite(pct) && pct > 0) return Math.max(1, Math.round(pct))
  }
  const rawCpu = typeof config['limits.cpu'] === 'string' ? config['limits.cpu'].trim() : ''
  const cores = Number(rawCpu)
  if (Number.isFinite(cores) && cores > 0) return Math.max(1, Math.round(cores * 100))
  return 100
}

/**
 * 从 Incus 实例详情与运行时状态提取资源与网络规格。
 */
export async function inspectIncusInstanceMetrics(
  client: IncusClient,
  incusName: string
): Promise<ExtractedIncusMetrics> {
  const [incusInst, incusState] = await Promise.all([
    getInstance(client, incusName),
    getInstanceState(client, incusName).catch(() => null)
  ])
  if (!incusInst) {
    throw new PackageBindingError(`节点上不存在实例 ${incusName}`, 404, 'INCUS_INSTANCE_NOT_FOUND')
  }

  const config = (incusInst.expanded_config || incusInst.config || {}) as Record<string, unknown>
  const devices = (incusInst.expanded_devices || incusInst.devices || {}) as Record<string, Record<string, unknown>>
  const rootDevice = devices.root
  const storagePoolName = typeof rootDevice?.pool === 'string' && rootDevice.pool.trim()
    ? rootDevice.pool.trim()
    : null

  let ipv4: string | null = null
  let ipv6: string | null = null
  const networkMap = (incusState as IncusInstance['state'] | null)?.network
  if (networkMap && typeof networkMap === 'object') {
    for (const [iface, info] of Object.entries(networkMap)) {
      if (iface === 'lo') continue
      for (const addr of info.addresses || []) {
        if (addr.scope && addr.scope !== 'global') continue
        if (addr.family === 'inet' && !ipv4 && !addr.address.startsWith('169.254.')) {
          ipv4 = addr.address
        } else if (addr.family === 'inet6' && !ipv6 && !addr.address.startsWith('fe80:')) {
          ipv6 = addr.address
        }
      }
    }
  }

  const networkMode: NetworkMode = ipv4 && ipv6
    ? NetworkMode.nat_ipv6
    : ipv6
      ? NetworkMode.ipv6_only
      : NetworkMode.nat
  const status = incusInst.status?.toLowerCase() === 'running' ? 'running' : 'stopped'
  const instanceType = incusInst.type === 'virtual-machine' ? 'vm' : 'container'

  return {
    cpu: normalizeIncusCpuAllowance(config),
    memory: normalizeIncusSizeToMB(config['limits.memory'], 512),
    disk: normalizeIncusSizeToMB(rootDevice?.size, 5120),
    storagePoolName,
    ipv4,
    ipv6,
    networkMode,
    image: String(config['image.description'] || config['image.os'] || 'imported'),
    status,
    instanceType
  }
}

/**
 * 列出指定宿主机可用于导入/挂载绑定的套餐及方案（优先排列已关联该宿主机的套餐）。
 */
export async function listImportablePackagesForHost(hostId: number): Promise<ImportPackageOption[]> {
  const host = await prisma.host.findUnique({
    where: { id: hostId },
    select: { id: true, instanceType: true }
  })
  if (!host) {
    throw new PackageBindingError('宿主机不存在', 404, 'HOST_NOT_FOUND')
  }

  const packages = await prisma.package.findMany({
    where: host.instanceType && host.instanceType !== 'both'
      ? { instanceType: host.instanceType }
      : undefined,
    include: {
      packageHosts: {
        where: { hostId },
        select: { hostId: true }
      },
      plans: {
        orderBy: [{ sortOrder: 'asc' }, { id: 'asc' }]
      }
    },
    orderBy: [{ active: 'desc' }, { id: 'desc' }]
  })

  const mapped: ImportPackageOption[] = packages.map(pkg => ({
    id: pkg.id,
    name: pkg.name,
    description: pkg.description ?? null,
    instanceType: pkg.instanceType,
    networkMode: pkg.networkMode,
    cpuMax: pkg.cpuMax,
    memoryMax: pkg.memoryMax,
    diskMax: pkg.diskMax,
    monthlyTrafficLimit: pkg.monthlyTrafficLimit !== null ? pkg.monthlyTrafficLimit.toString() : null,
    portLimit: pkg.portLimit,
    snapshotLimit: pkg.snapshotLimit,
    backupLimit: pkg.backupLimit,
    siteLimit: pkg.siteLimit,
    isActive: pkg.active,
    boundToHost: pkg.packageHosts.length > 0,
    plans: pkg.plans.map(plan => ({
      id: plan.id,
      name: plan.name,
      description: plan.description ?? null,
      billingCycle: plan.billingCycle,
      price: Number((Number(plan.price) / 100).toFixed(2)),
      cpu: plan.cpu,
      memory: plan.memory,
      disk: plan.disk,
      trafficLimit: plan.trafficLimit.toString(),
      trafficLimitSpeed: plan.trafficLimitSpeed ?? null,
      portLimit: plan.portLimit,
      snapshotLimit: plan.snapshotLimit,
      backupLimit: plan.backupLimit,
      siteLimit: plan.siteLimit,
      isActive: plan.isActive,
      sortOrder: plan.sortOrder
    }))
  }))

  // 已关联该节点的套餐优先展示，启用套餐次之
  return mapped.sort((a, b) => {
    if (a.boundToHost !== b.boundToHost) return a.boundToHost ? -1 : 1
    if (a.isActive !== b.isActive) return a.isActive ? -1 : 1
    return a.id - b.id
  })
}

/**
 * 提交导入任务前快速校验套餐、方案、到期参数与目标用户，避免异步任务跑完才失败。
 */
export async function validatePackageBindingSelection(options: {
  hostId: number
  packageId?: number | null
  planId?: number | null
  validDays?: number | null
  expiresAt?: string | null
  instanceType?: 'container' | 'vm' | null
  userId?: number | null
}): Promise<{
  packageName: string | null
  planName: string | null
  resolvedPlanId: number | null
}> {
  if (options.userId !== undefined && options.userId !== null) {
    const targetUser = await prisma.user.findUnique({
      where: { id: options.userId },
      select: { id: true }
    })
    if (!targetUser) {
      throw new PackageBindingError('目标用户不存在', 404, 'USER_NOT_FOUND')
    }
  }

  if (!options.packageId) {
    if (options.planId) {
      throw new PackageBindingError('请先选择套餐再选择计费方案', 400, 'PACKAGE_REQUIRED_FOR_PLAN')
    }
    resolveExplicitExpiresAt({
      validDays: options.validDays,
      expiresAt: options.expiresAt,
      defaultExpiresAt: null
    })
    return { packageName: null, planName: null, resolvedPlanId: null }
  }

  const pkg = await prisma.package.findUnique({
    where: { id: options.packageId },
    include: {
      plans: {
        orderBy: [{ sortOrder: 'asc' }, { id: 'asc' }]
      }
    }
  })
  if (!pkg) {
    throw new PackageBindingError('所选套餐不存在', 404, 'PACKAGE_NOT_FOUND')
  }

  if (options.instanceType && pkg.instanceType !== options.instanceType) {
    throw new PackageBindingError(
      `套餐类型为 ${pkg.instanceType}，与实例类型 ${options.instanceType} 不匹配`,
      400,
      'PACKAGE_INSTANCE_TYPE_MISMATCH'
    )
  }

  let selectedPlan = null as (typeof pkg.plans)[number] | null
  if (typeof options.planId === 'number') {
    selectedPlan = pkg.plans.find(p => p.id === options.planId) ?? null
    if (!selectedPlan) {
      throw new PackageBindingError('所选方案不属于当前套餐', 400, 'PLAN_NOT_IN_PACKAGE')
    }
  } else if (options.planId === undefined && pkg.plans.length > 0) {
    // 调用方未显式传入 planId: null 时默认挂到第一个启用方案，避免付费套餐误变成免费实例
    selectedPlan = pkg.plans.find(p => p.isActive) ?? pkg.plans[0] ?? null
  }

  if (selectedPlan) {
    const normalizedSpeed = normalizePlanTrafficLimitSpeed(selectedPlan.trafficLimitSpeed)
    if (normalizedSpeed === undefined) {
      throw new PackageBindingError('所选方案的限速配置无效', 400, 'INVALID_PLAN_SPEED')
    }
  }

  resolveExplicitExpiresAt({
    validDays: options.validDays,
    expiresAt: options.expiresAt,
    defaultExpiresAt: selectedPlan ? calculateCreateBilling(selectedPlan).expiresAt : null
  })

  return {
    packageName: pkg.name,
    planName: selectedPlan?.name ?? null,
    resolvedPlanId: selectedPlan?.id ?? null
  }
}

/**
 * 在事务内解析目标套餐/方案配额、节点流量倍率与出网绑定，并返回实例写入字段。
 */
export async function resolvePackageBindingForHost(
  tx: Prisma.TransactionClient,
  options: {
    hostId: number
    packageId?: number | null
    planId?: number | null
    validDays?: number | null
    expiresAt?: string | null
    instanceType?: 'container' | 'vm' | null
    now?: Date
  }
): Promise<ResolvedPackageBinding> {
  const now = options.now ?? new Date()

  if (!options.packageId) {
    if (options.planId) {
      throw new PackageBindingError('请先选择套餐再选择计费方案', 400, 'PACKAGE_REQUIRED_FOR_PLAN')
    }
    return {
      packageId: null,
      packagePlanId: null,
      packageName: null,
      planName: null,
      billingPrice: null,
      billingCycle: null,
      expiresAt: null,
      portLimit: 10,
      snapshotLimit: 3,
      backupLimit: 2,
      siteLimit: 5,
      monthlyTrafficLimit: null,
      limitsRead: null,
      limitsWrite: null,
      limitsReadIops: null,
      limitsWriteIops: null,
      limitsIngress: null,
      limitsEgress: null,
      limitsProcesses: null,
      limitsCpuPriority: null,
      bootAutostart: false,
      bootAutostartPriority: null,
      bootAutostartDelay: null,
      bootHostShutdownTimeout: null,
      swapEnabled: false,
      swapSize: null,
      egressMode: 'direct',
      egressProfileId: null,
      hasLimitsTrafficSpeed: false
    }
  }

  const pkg = await tx.package.findUnique({
    where: { id: options.packageId },
    include: {
      plans: {
        orderBy: [{ sortOrder: 'asc' }, { id: 'asc' }]
      }
    }
  })
  if (!pkg) {
    throw new PackageBindingError('所选套餐不存在', 404, 'PACKAGE_NOT_FOUND')
  }

  if (options.instanceType && pkg.instanceType !== options.instanceType) {
    throw new PackageBindingError(
      `套餐类型为 ${pkg.instanceType}，与实例类型 ${options.instanceType} 不匹配`,
      400,
      'PACKAGE_INSTANCE_TYPE_MISMATCH'
    )
  }

  let selectedPlan = null as (typeof pkg.plans)[number] | null
  if (typeof options.planId === 'number') {
    selectedPlan = pkg.plans.find(p => p.id === options.planId) ?? null
    if (!selectedPlan) {
      throw new PackageBindingError('所选方案不属于当前套餐', 400, 'PLAN_NOT_IN_PACKAGE')
    }
  } else if (options.planId === undefined && pkg.plans.length > 0) {
    selectedPlan = pkg.plans.find(p => p.isActive) ?? pkg.plans[0] ?? null
  }

  let normalizedSpeed: string | null = null
  if (selectedPlan) {
    const parsedSpeed = normalizePlanTrafficLimitSpeed(selectedPlan.trafficLimitSpeed)
    if (parsedSpeed === undefined) {
      throw new PackageBindingError('所选方案的限速配置无效', 400, 'INVALID_PLAN_SPEED')
    }
    normalizedSpeed = parsedSpeed
  }

  const billing = selectedPlan ? calculateCreateBilling(selectedPlan) : null
  const resolvedExpiresAt = selectedPlan
    ? resolveExplicitExpiresAt({
        validDays: options.validDays,
        expiresAt: options.expiresAt,
        defaultExpiresAt: billing?.expiresAt ?? null,
        now
      })
    : null

  const baseTrafficLimit = selectedPlan?.trafficLimit ?? pkg.monthlyTrafficLimit ?? null
  const effectiveMonthlyTrafficLimit = await resolveInstanceTrafficLimitForHost(tx, {
    packageId: pkg.id,
    hostId: options.hostId,
    baseTrafficLimit
  })

  const egressBinding = await bindPackageEgress(tx, pkg.id, options.hostId)
  const limitsIngress = normalizedSpeed ?? pkg.limitsIngress ?? null
  const limitsEgress = normalizedSpeed ?? pkg.limitsEgress ?? null
  const planSwapSize = selectedPlan?.swapSize ?? 0

  return {
    packageId: pkg.id,
    packagePlanId: selectedPlan?.id ?? null,
    packageName: pkg.name,
    planName: selectedPlan?.name ?? null,
    billingPrice: billing ? new Prisma.Decimal(billing.price.toFixed(2)) : null,
    billingCycle: billing ? billing.billingCycle : null,
    expiresAt: resolvedExpiresAt,
    portLimit: selectedPlan?.portLimit ?? pkg.portLimit,
    snapshotLimit: selectedPlan?.snapshotLimit ?? pkg.snapshotLimit,
    backupLimit: selectedPlan?.backupLimit ?? pkg.backupLimit,
    siteLimit: selectedPlan?.siteLimit ?? pkg.siteLimit,
    monthlyTrafficLimit: effectiveMonthlyTrafficLimit,
    limitsRead: pkg.limitsRead ?? null,
    limitsWrite: pkg.limitsWrite ?? null,
    limitsReadIops: pkg.limitsReadIops ?? null,
    limitsWriteIops: pkg.limitsWriteIops ?? null,
    limitsIngress,
    limitsEgress,
    limitsProcesses: pkg.limitsProcesses ?? null,
    limitsCpuPriority: pkg.limitsCpuPriority ?? null,
    bootAutostart: pkg.bootAutostart,
    bootAutostartPriority: pkg.bootAutostartPriority ?? null,
    bootAutostartDelay: pkg.bootAutostartDelay ?? null,
    bootHostShutdownTimeout: pkg.bootHostShutdownTimeout ?? null,
    swapEnabled: planSwapSize > 0,
    swapSize: planSwapSize > 0 ? planSwapSize : null,
    egressMode: egressBinding.egressMode,
    egressProfileId: egressBinding.egressProfileId,
    hasLimitsTrafficSpeed: normalizedSpeed !== null
  }
}

async function mutateImportPresetStore<T>(
  mutator: (store: ImportPresetStore) => T | Promise<T>
): Promise<T> {
  return prisma.$transaction(async (tx) => {
    await tx.$executeRaw`SELECT pg_advisory_xact_lock(${IMPORT_PRESET_LOCK_KEY})`
    const existing = await tx.systemConfig.findUnique({ where: { key: IMPORT_PRESET_CONFIG_KEY } })
    let parsedRaw: unknown = null
    if (existing?.value) {
      try {
        parsedRaw = JSON.parse(existing.value)
      } catch {
        parsedRaw = null
      }
    }

    const store = pruneAndNormalizePresetStore(parsedRaw)
    const result = await mutator(store)
    const serialized = JSON.stringify(store)

    await tx.systemConfig.upsert({
      where: { key: IMPORT_PRESET_CONFIG_KEY },
      create: {
        key: IMPORT_PRESET_CONFIG_KEY,
        value: serialized,
        type: 'json',
        description: '实例异步导入待分配套餐/用户预设（内部状态）'
      },
      update: {
        value: serialized,
        type: 'json'
      }
    })
    return result
  })
}

async function readImportPresetStore(): Promise<ImportPresetStore> {
  const existing = await prisma.systemConfig.findUnique({ where: { key: IMPORT_PRESET_CONFIG_KEY } })
  if (!existing?.value) return emptyPresetStore()
  try {
    return pruneAndNormalizePresetStore(JSON.parse(existing.value))
  } catch {
    return emptyPresetStore()
  }
}

/**
 * 保存异步导入任务的套餐/用户预设，供导入完成时自动绑定或展示在未分配列表。
 */
export async function savePendingImportTaskPreset(options: {
  taskId: string
  hostId: number
  incusName?: string | null
  filePath?: string | null
  preset: PendingImportPreset
}): Promise<void> {
  if (!options.taskId) return
  await mutateImportPresetStore((store) => {
    store.byTask[options.taskId] = {
      hostId: options.hostId,
      incusName: options.incusName?.trim() || null,
      filePath: options.filePath?.trim() || null,
      preset: {
        ...options.preset,
        createdAt: options.preset.createdAt || new Date().toISOString()
      }
    }
  })
}

/**
 * 消费指定导入任务的套餐预设（如果存在）。
 */
export async function consumePendingImportTaskPreset(taskId: string): Promise<PendingImportPreset | null> {
  if (!taskId) return null
  return mutateImportPresetStore((store) => {
    const found = store.byTask[taskId]
    if (!found) return null
    delete store.byTask[taskId]
    return found.preset
  })
}

/**
 * 为尚未分配用户的导入实例保存推荐套餐/方案预设。
 */
export async function saveUnassignedInstancePreset(options: {
  hostId: number
  incusName: string
  preset: PendingImportPreset
}): Promise<void> {
  if (!options.incusName.trim()) return
  await mutateImportPresetStore((store) => {
    store.byUnassigned[unassignedPresetKey(options.hostId, options.incusName.trim())] = options.preset
  })
}

/**
 * 读取未分配实例上的套餐预设。
 */
export async function getUnassignedInstancePreset(
  hostId: number,
  incusName: string
): Promise<PendingImportPreset | null> {
  const store = await readImportPresetStore()
  return store.byUnassigned[unassignedPresetKey(hostId, incusName)] ?? null
}

/**
 * 消费并删除未分配实例上的套餐预设。
 */
export async function consumeUnassignedInstancePreset(
  hostId: number,
  incusName: string
): Promise<PendingImportPreset | null> {
  return mutateImportPresetStore((store) => {
    const key = unassignedPresetKey(hostId, incusName)
    const found = store.byUnassigned[key] ?? null
    if (found) delete store.byUnassigned[key]
    return found
  })
}

/**
 * 将同宿主机上与未分配实例名匹配的残留任务预设迁移到 unassigned 映射，确保前端列表可见。
 */
export async function listUnassignedInstancePresetsForHost(
  hostId: number,
  incusNames: string[]
): Promise<Record<string, PendingImportPreset>> {
  const nameSet = new Set(incusNames)
  const prefix = `${hostId}:`

  return mutateImportPresetStore((store) => {
    for (const [taskId, entry] of Object.entries(store.byTask)) {
      if (entry.hostId === hostId && entry.incusName && nameSet.has(entry.incusName)) {
        const key = unassignedPresetKey(hostId, entry.incusName)
        if (!store.byUnassigned[key]) {
          store.byUnassigned[key] = entry.preset
        }
        delete store.byTask[taskId]
      }
    }

    const result: Record<string, PendingImportPreset> = {}
    for (const [key, preset] of Object.entries(store.byUnassigned)) {
      if (!key.startsWith(prefix)) continue
      const name = key.slice(prefix.length)
      if (nameSet.has(name)) {
        result[name] = preset
      }
    }
    return result
  })
}

/**
 * 将节点上导入/未纳管的 Incus 实例注册进数据库并绑定指定套餐与方案，同步更新宿主机已用资源与日志。
 */
export async function registerImportedHostInstance(options: {
  hostId: number
  client: IncusClient
  incusName: string
  userId: number
  displayName?: string
  binding: PackageBindingInput
  metrics?: ExtractedIncusMetrics
  auditUserId?: number | null
  auditSource?: 'import_auto' | 'manual_assign'
  auditNote?: string
  throwIfAlreadyAssigned?: boolean
}): Promise<{
  alreadyAssigned: boolean
  instance: {
    id: number
    incusId: string
    name: string
    userId: number
    hostId: number
    packageId: number | null
    packagePlanId: number | null
    status: string
    expiresAt: string | null
    billingPrice: number | null
    billingCycle: number | null
    packageName: string | null
    planName: string | null
  } | null
}> {
  const targetUser = await prisma.user.findUnique({
    where: { id: options.userId },
    select: { id: true, username: true }
  })
  if (!targetUser) {
    throw new PackageBindingError('目标用户不存在', 404, 'USER_NOT_FOUND')
  }

  const metrics = options.metrics ?? await inspectIncusInstanceMetrics(options.client, options.incusName)
  const displayName = options.displayName?.trim() || options.incusName

  const txResult = await prisma.$transaction(async (tx) => {
    const dup = await tx.instance.findFirst({
      where: { hostId: options.hostId, incusId: options.incusName, status: { not: 'deleted' } }
    })
    if (dup) {
      if (options.throwIfAlreadyAssigned !== false) {
        throw new PackageBindingError('该实例已在数据库中纳管', 409, 'ALREADY_ASSIGNED')
      }
      return { alreadyAssigned: true as const, createdInstance: null, resolved: null }
    }

    const resolved = await resolvePackageBindingForHost(tx, {
      hostId: options.hostId,
      packageId: options.binding.packageId,
      planId: options.binding.planId,
      validDays: options.binding.validDays,
      expiresAt: options.binding.expiresAt,
      instanceType: metrics.instanceType
    })

    const createdInstance = await tx.instance.create({
      data: {
        incusId: options.incusName,
        name: displayName,
        userId: targetUser.id,
        hostId: options.hostId,
        packageId: resolved.packageId,
        packagePlanId: resolved.packagePlanId,
        storagePoolName: metrics.storagePoolName,
        image: metrics.image,
        status: metrics.status,
        cpu: metrics.cpu,
        memory: metrics.memory,
        disk: metrics.disk,
        ipv4: metrics.ipv4,
        ipv6: metrics.ipv6,
        networkMode: metrics.networkMode,
        portLimit: resolved.portLimit,
        snapshotLimit: resolved.snapshotLimit,
        backupLimit: resolved.backupLimit,
        siteLimit: resolved.siteLimit,
        monthlyTrafficLimit: resolved.monthlyTrafficLimit,
        limitsRead: resolved.limitsRead,
        limitsWrite: resolved.limitsWrite,
        limitsReadIops: resolved.limitsReadIops,
        limitsWriteIops: resolved.limitsWriteIops,
        limitsIngress: resolved.limitsIngress,
        limitsEgress: resolved.limitsEgress,
        limitsProcesses: resolved.limitsProcesses,
        limitsCpuPriority: resolved.limitsCpuPriority,
        bootAutostart: resolved.bootAutostart,
        bootAutostartPriority: resolved.bootAutostartPriority,
        bootAutostartDelay: resolved.bootAutostartDelay,
        bootHostShutdownTimeout: resolved.bootHostShutdownTimeout,
        swapEnabled: resolved.swapEnabled,
        swapSize: resolved.swapSize,
        egressMode: resolved.egressMode,
        egressProfileId: resolved.egressProfileId,
        billingPrice: resolved.billingPrice,
        billingCycle: resolved.billingCycle,
        expiresAt: resolved.expiresAt
      }
    })

    return { alreadyAssigned: false as const, createdInstance, resolved }
  })

  if (txResult.alreadyAssigned || !txResult.createdInstance || !txResult.resolved) {
    return { alreadyAssigned: true, instance: null }
  }

  const usedResources = await calculateHostResourcesFromInstances(options.hostId)
  await updateHostResources(options.hostId, usedResources)

  const pkgDesc = txResult.resolved.packageName
    ? `，挂载套餐 ${txResult.resolved.packageName}${txResult.resolved.planName ? ` / ${txResult.resolved.planName}` : ''}`
    : '（未挂载套餐，按免费实例处理）'
  const sourceLabel = options.auditSource === 'import_auto' ? '导入后自动分配实例' : '分配未纳管实例'
  const noteSuffix = options.auditNote ? ` ${options.auditNote.trim()}` : ''

  await createLog(
    options.auditUserId ?? targetUser.id,
    LogModule.INSTANCE,
    options.auditSource === 'import_auto' ? 'host.instance.import_assign' : 'host.instance.assign',
    `节点 #${options.hostId} ${sourceLabel} ${options.incusName} (#${txResult.createdInstance.id}) 给用户 ${targetUser.username} (#${targetUser.id})${pkgDesc}${noteSuffix}`,
    LogResult.SUCCESS,
    { instanceId: txResult.createdInstance.id }
  )

  return {
    alreadyAssigned: false,
    instance: {
      id: txResult.createdInstance.id,
      incusId: txResult.createdInstance.incusId,
      name: txResult.createdInstance.name,
      userId: txResult.createdInstance.userId,
      hostId: txResult.createdInstance.hostId,
      packageId: txResult.createdInstance.packageId,
      packagePlanId: txResult.createdInstance.packagePlanId,
      status: txResult.createdInstance.status,
      expiresAt: txResult.createdInstance.expiresAt?.toISOString() ?? null,
      billingPrice: txResult.createdInstance.billingPrice !== null ? Number(txResult.createdInstance.billingPrice) : null,
      billingCycle: txResult.createdInstance.billingCycle ?? null,
      packageName: txResult.resolved.packageName,
      planName: txResult.resolved.planName
    }
  }
}

/**
 * 为已纳管实例重新挂载或调整套餐/方案（不扣费），同步配额、流量限额、出网策略与到期时间。
 */
export async function rebindInstancePackage(options: {
  hostId: number
  instanceId: number
  binding: PackageBindingInput
  auditUserId?: number | null
  incusInstanceType?: 'container' | 'vm' | null
}) {
  const existing = await prisma.instance.findUnique({
    where: { id: options.instanceId },
    select: {
      id: true,
      hostId: true,
      name: true,
      incusId: true,
      status: true,
      userId: true,
      packageId: true
    }
  })

  if (!existing || existing.status === 'deleted' || existing.hostId !== options.hostId) {
    throw new PackageBindingError('实例不存在或不属于当前宿主机', 404, 'INSTANCE_NOT_FOUND')
  }

  let inferredType = options.incusInstanceType ?? null
  if (!inferredType) {
    const host = await prisma.host.findUnique({
      where: { id: options.hostId },
      select: { instanceType: true }
    })
    if (host?.instanceType === 'container' || host?.instanceType === 'vm') {
      inferredType = host.instanceType
    }
  }

  const now = new Date()
  const updatedResult = await prisma.$transaction(async (tx) => {
    const resolved = await resolvePackageBindingForHost(tx, {
      hostId: options.hostId,
      packageId: options.binding.packageId,
      planId: options.binding.planId,
      validDays: options.binding.validDays,
      expiresAt: options.binding.expiresAt,
      instanceType: inferredType,
      now
    })

    const updateData: Prisma.InstanceUncheckedUpdateInput = {
      packageId: resolved.packageId,
      packagePlanId: resolved.packagePlanId,
      portLimit: resolved.portLimit,
      snapshotLimit: resolved.snapshotLimit,
      backupLimit: resolved.backupLimit,
      siteLimit: resolved.siteLimit,
      monthlyTrafficLimit: resolved.monthlyTrafficLimit,
      billingPrice: resolved.billingPrice,
      billingCycle: resolved.billingCycle,
      expiresAt: resolved.expiresAt,
      autoRenew: false,
      egressMode: resolved.egressMode,
      egressProfileId: resolved.egressProfileId
    }

    if (resolved.hasLimitsTrafficSpeed || !resolved.packageId) {
      updateData.limitsIngress = resolved.limitsIngress
      updateData.limitsEgress = resolved.limitsEgress
    }

    const updated = await tx.instance.update({
      where: { id: options.instanceId },
      data: updateData
    })

    return { updated, resolved }
  })

  const pkgDesc = updatedResult.resolved.packageName
    ? `${updatedResult.resolved.packageName}${updatedResult.resolved.planName ? ` / ${updatedResult.resolved.planName}` : ''}`
    : '无套餐（免费实例）'

  await createLog(
    options.auditUserId ?? existing.userId,
    LogModule.INSTANCE,
    'instance.bind_package',
    `为实例 ${existing.name} (#${existing.id}) 调整挂载套餐：${pkgDesc}`,
    LogResult.SUCCESS,
    { instanceId: existing.id }
  )

  return {
    id: updatedResult.updated.id,
    packageId: updatedResult.updated.packageId,
    packagePlanId: updatedResult.updated.packagePlanId,
    packageName: updatedResult.resolved.packageName,
    planName: updatedResult.resolved.planName,
    billingPrice: updatedResult.updated.billingPrice !== null ? Number(updatedResult.updated.billingPrice) : null,
    billingCycle: updatedResult.updated.billingCycle ?? null,
    expiresAt: updatedResult.updated.expiresAt?.toISOString() ?? null
  }
}
