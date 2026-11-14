/** NVIDIA sharing is per physical device. It does not partition VRAM or compute. */
export const GPU_REPORT_MAX_AGE_MS = 180_000
export const GPU_SHARING_NOTICE = '共享物理 GPU；显存和算力由同池实例共同使用，不提供独立显存或算力保证。'
export class GpuError extends Error {
  statusCode: number
  code: string
  constructor(message: string, statusCode = 400, code = 'GPU_CONFIGURATION_INVALID') {
    super(message)
    this.statusCode = statusCode
    this.code = code
  }
}
export interface GpuPolicy {
  gpuShared: boolean
  gpuDeviceIds: number[]
}
export function normalizeGpuInput(input: Record<string, unknown>, nullable = false): { gpuShared?: boolean | null; gpuDeviceIds?: number[] } {
  const result: { gpuShared?: boolean | null; gpuDeviceIds?: number[] } = {}
  if (input.gpuShared !== undefined) {
    if (typeof input.gpuShared !== 'boolean' && !(nullable && input.gpuShared === null)) throw new GpuError('GPU sharing must be a boolean')
    result.gpuShared = input.gpuShared as boolean | null
  }
  if (input.gpuDeviceIds !== undefined) {
    if (!Array.isArray(input.gpuDeviceIds) || input.gpuDeviceIds.length > 1000 || input.gpuDeviceIds.some(id => !Number.isSafeInteger(id) || id <= 0 || id > 2147483647)) throw new GpuError('Invalid GPU device selection')
    result.gpuDeviceIds = [...new Set(input.gpuDeviceIds as number[])]
  }
  return result
}
export function resolveGpuPolicy(pkg: GpuPolicy, plan?: { gpuShared: boolean | null; gpuDeviceIds: number[] } | null): GpuPolicy {
  return plan && plan.gpuShared !== null
    ? { gpuShared: plan.gpuShared, gpuDeviceIds: plan.gpuDeviceIds }
    : pkg
}
export function isGpuFresh(gpu: { available: boolean; runtimeReady: boolean; lastSeenAt: Date }, now = Date.now()): boolean {
  return gpu.available && gpu.runtimeReady && now - gpu.lastSeenAt.getTime() <= GPU_REPORT_MAX_AGE_MS && gpu.lastSeenAt.getTime() <= now + 30_000
}
export interface GpuModelSummary { name: string; memoryTotalMiB: number; count: number }
export interface GpuConfiguration { hostIds: number[]; devices: GpuModelSummary[]; count: number }

export function summarizeGpuModels(devices: Array<{ name: string; memoryTotalMiB: number }>): GpuModelSummary[] {
  const groups = new Map<string, GpuModelSummary>()
  for (const device of devices) {
    const key = JSON.stringify([device.name, device.memoryTotalMiB])
    const group = groups.get(key)
    if (group) group.count++
    else groups.set(key, { name: device.name, memoryTotalMiB: device.memoryTotalMiB, count: 1 })
  }
  return [...groups.values()].sort((a, b) => a.name.localeCompare(b.name) || a.memoryTotalMiB - b.memoryTotalMiB)
}

// Each host provides its entire selected bundle. Equal bundles across hosts are
// alternatives for scheduling, not additional GPUs in the same container.
export function summarizeGpuConfigurations(devices: Array<{ hostId: number; name: string; memoryTotalMiB: number }>): GpuConfiguration[] {
  const byHost = new Map<number, typeof devices>()
  for (const device of devices) byHost.set(device.hostId, [...(byHost.get(device.hostId) || []), device])
  const groups = new Map<string, GpuConfiguration>()
  for (const [hostId, cards] of byHost) {
    const models = summarizeGpuModels(cards)
    const key = JSON.stringify(models)
    const group = groups.get(key)
    if (group) group.hostIds.push(hostId)
    else groups.set(key, { hostIds: [hostId], devices: models, count: cards.length })
  }
  return [...groups.values()]
}

const GPU_CONFIG_KEYS = ['nvidia.runtime', 'nvidia.driver.capabilities'] as const

export interface IncusGpuDeviceInfo {
  name: string
  pci: string | null
  gputype: string | null
}

export function listIncusGpuDevices(config: Record<string, any> | null | undefined): IncusGpuDeviceInfo[] {
  if (!config?.devices || typeof config.devices !== 'object') return []
  const result: IncusGpuDeviceInfo[] = []
  for (const [name, device] of Object.entries(config.devices)) {
    if ((device as any)?.type !== 'gpu') continue
    const rawPci = typeof (device as any)?.pci === 'string' ? (device as any).pci.trim() : ''
    const rawType = typeof (device as any)?.gputype === 'string' ? (device as any).gputype.trim() : ''
    result.push({ name, pci: rawPci || null, gputype: rawType || null })
  }
  return result.sort((a, b) => a.name.localeCompare(b.name))
}

export function extractResourceGpuPciAddresses(resources: unknown): Set<string> | null {
  if (!resources || typeof resources !== 'object') return null
  const cards = (resources as { gpu?: { cards?: unknown } }).gpu?.cards
  if (!Array.isArray(cards)) return null
  const addresses = new Set<string>()
  for (const card of cards) {
    if (!card || typeof card !== 'object') continue
    const raw = (card as { pci_address?: unknown; pciAddress?: unknown }).pci_address ?? (card as { pciAddress?: unknown }).pciAddress
    if (typeof raw === 'string' && raw.trim()) addresses.add(raw.trim().toLowerCase())
  }
  return addresses
}

export function clearGpuDevices(config: Record<string, any>): boolean {
  let changed = false
  if (config.devices && typeof config.devices === 'object') {
    const devices = { ...config.devices }
    for (const [name, device] of Object.entries(devices)) {
      if ((device as any)?.type === 'gpu') {
        delete devices[name]
        changed = true
      }
    }
    if (changed) config.devices = devices
  }
  if (config.config && typeof config.config === 'object') {
    const nextConfig = { ...config.config }
    let configChanged = false
    for (const key of GPU_CONFIG_KEYS) {
      if (key in nextConfig) {
        delete nextConfig[key]
        configChanged = true
      }
    }
    if (configChanged) {
      config.config = nextConfig
      changed = true
    }
  }
  return changed
}

export function sanitizeMissingHostGpuDevices(config: Record<string, any>, hostPciAddresses: Set<string>): string[] {
  if (!config.devices || typeof config.devices !== 'object') return []
  const devices = { ...config.devices }
  const removed: string[] = []
  let remainingGpuCount = 0
  for (const name of Object.keys(devices).sort()) {
    const device = devices[name]
    if ((device as any)?.type !== 'gpu') continue
    const rawPci = typeof (device as any)?.pci === 'string' ? (device as any).pci.trim() : ''
    const normalizedPci = rawPci.toLowerCase()
    if ((normalizedPci && !hostPciAddresses.has(normalizedPci)) || (!normalizedPci && hostPciAddresses.size === 0)) {
      delete devices[name]
      removed.push(rawPci ? `${name}(${rawPci})` : name)
    } else {
      remainingGpuCount++
    }
  }
  let configChanged = false
  if (remainingGpuCount === 0 && config.config && typeof config.config === 'object') {
    const nextConfig = { ...config.config }
    for (const key of GPU_CONFIG_KEYS) {
      if (key in nextConfig) {
        delete nextConfig[key]
        configChanged = true
      }
    }
    if (configChanged) config.config = nextConfig
  }
  if (removed.length > 0) config.devices = devices
  return removed
}

export function applyGpuDevice(config: Record<string, any>, pci: string | string[]): void {
  const addresses = typeof pci === 'string' ? [pci] : pci
  if (config.type === 'virtual-machine' || config.config?.['security.privileged'] === 'true') throw new GpuError('共享 GPU 仅支持非特权 LXC 容器')
  if (!addresses.length || new Set(addresses).size !== addresses.length || addresses.some(address => !/^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$/.test(address))) throw new GpuError('Invalid GPU PCI address')
  // Do not inherit a default profile that might expose every GPU on the host.
  config.profiles = []
  config.config = { ...config.config, 'nvidia.runtime': 'true', 'nvidia.driver.capabilities': 'compute,utility' }
  const devices = { ...config.devices }
  for (const [name, device] of Object.entries(devices)) if ((device as any)?.type === 'gpu') delete devices[name]
  addresses.forEach((pciAddress, index) => { devices[`gpu${index}`] = { type: 'gpu', gputype: 'physical', pci: pciAddress } })
  config.devices = devices
}
