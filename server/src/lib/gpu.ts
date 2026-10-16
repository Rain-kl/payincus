/** NVIDIA sharing is per physical device. It does not partition VRAM or compute. */
export const GPU_REPORT_MAX_AGE_MS = 180_000
export const GPU_SHARING_NOTICE = '共享物理 GPU；显存和算力由同池实例共同使用，不提供独立显存或算力保证。'
export class GpuError extends Error {
  statusCode = 400
  code = 'GPU_CONFIGURATION_INVALID'
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
export function applyGpuDevice(config: Record<string, any>, pciAddress: string): void {
  if (config.type === 'virtual-machine' || config.config?.['security.privileged'] === 'true') throw new GpuError('共享 GPU 仅支持非特权 LXC 容器')
  if (!/^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$/.test(pciAddress)) throw new GpuError('Invalid GPU PCI address')
  // Do not inherit a default profile that might expose every GPU on the host.
  config.profiles = []
  config.config = { ...config.config, 'nvidia.runtime': 'true', 'nvidia.driver.capabilities': 'compute,utility' }
  const devices = { ...config.devices }
  for (const [name, device] of Object.entries(devices)) if ((device as any)?.type === 'gpu') delete devices[name]
  devices.gpu0 = { type: 'gpu', gputype: 'physical', pci: pciAddress }
  config.devices = devices
}
