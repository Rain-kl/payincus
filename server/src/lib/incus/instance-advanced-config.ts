import type { IncusClient } from './incus-client.js'

export const advancedFields = {
  limitsRead: 'limits_read', limitsWrite: 'limits_write',
  limitsReadIops: 'limits_read_iops', limitsWriteIops: 'limits_write_iops',
  limitsIngress: 'limits_ingress', limitsEgress: 'limits_egress',
  limitsProcesses: 'limits_processes', limitsCpuPriority: 'limits_cpu_priority',
  bootAutostart: 'boot_autostart', bootAutostartPriority: 'boot_autostart_priority',
  bootAutostartDelay: 'boot_autostart_delay', bootHostShutdownTimeout: 'boot_host_shutdown_timeout'
} as const
export type AdvancedConfig = Partial<Record<keyof typeof advancedFields, string | number | boolean | null>>
export const advancedDefaults: Record<string, string | number | boolean> = {
  limits_read: '100MB', limits_write: '100MB', limits_read_iops: 500, limits_write_iops: 500,
  limits_ingress: '300Mbit', limits_egress: '300Mbit', limits_processes: 500, limits_cpu_priority: 10,
  boot_autostart: true, boot_autostart_priority: 20, boot_autostart_delay: 15, boot_host_shutdown_timeout: 30
}
const configKeys: Partial<Record<keyof typeof advancedFields, string>> = {
  limitsProcesses: 'limits.processes', limitsCpuPriority: 'limits.cpu.priority',
  bootAutostart: 'boot.autostart', bootAutostartPriority: 'boot.autostart.priority',
  bootAutostartDelay: 'boot.autostart.delay', bootHostShutdownTimeout: 'boot.host_shutdown_timeout'
}
type Devices = Record<string, Record<string, string>>
function validDevices(value: unknown): value is Devices {
  return !!value && typeof value === 'object' && !Array.isArray(value) && Object.values(value).every(device =>
    !!device && typeof device === 'object' && !Array.isArray(device) && Object.values(device).every(entry => typeof entry === 'string'))
}

// Incus 7.0 LTS PATCH merges configuration keys and whole named device definitions.
export async function applyAdvancedConfig(
  client: Pick<IncusClient, 'request'>, name: string, changes: AdvancedConfig,
  defaults: Record<string, unknown>, signal: AbortSignal
): Promise<void> {
  signal.throwIfAborted()
  const path = `/1.0/instances/${encodeURIComponent(name)}`
  const current = await client.request<{ type: string; devices: Devices; expanded_devices: Devices }>('GET', path, null, 30000, signal)
  if (!current || !['container', 'virtual-machine'].includes(current.type) || !validDevices(current.devices) || !validDevices(current.expanded_devices)) {
    throw new Error('Invalid Incus instance response')
  }
  if (current.type === 'virtual-machine' && changes.limitsProcesses !== undefined) throw new Error('Process limits require a container')
  const config: Record<string, string> = {}
  const devices: Devices = {}
  const value = (key: keyof typeof advancedFields) => changes[key] ?? defaults[advancedFields[key]] ?? advancedDefaults[advancedFields[key]]
  for (const [key, target] of Object.entries(configKeys)) {
    if (changes[key as keyof AdvancedConfig] !== undefined) config[target!] = String(value(key as keyof AdvancedConfig))
  }
  const device = (kind: 'disk' | 'nic'): Record<string, string> => {
    const candidates = Object.entries(current.expanded_devices).filter(([, d]) => d.type === kind && (kind !== 'disk' || d.path === '/'))
    const selected = kind === 'nic' ? candidates.find(([name]) => name === 'eth0') ?? candidates.find(([, d]) => d.name === 'eth0') ?? (candidates.length === 1 ? candidates[0] : undefined) : candidates.length === 1 ? candidates[0] : undefined
    if (!selected) throw new Error(`Cannot identify instance ${kind} device`)
    const [key, expanded] = selected
    devices[key] ??= { ...expanded, ...current.devices[key] }
    return devices[key]
  }
  for (const [throughput, iops, target] of [['limitsRead', 'limitsReadIops', 'limits.read'], ['limitsWrite', 'limitsWriteIops', 'limits.write']] as const) {
    if (changes[throughput] !== undefined || changes[iops] !== undefined) {
      if (changes[throughput] !== undefined && changes[iops] !== undefined) throw new Error('Choose throughput or IOPS, not both')
      const key = changes[throughput] !== undefined ? throughput : iops
      device('disk')[target] = `${value(key)}${key === iops ? 'iops' : ''}`
    }
  }
  for (const [key, target] of [['limitsIngress', 'limits.ingress'], ['limitsEgress', 'limits.egress']] as const) {
    if (changes[key] !== undefined) device('nic')[target] = String(value(key)).replace(/Mbps$/i, 'Mbit').replace(/Gbps$/i, 'Gbit').replace(/Kbps$/i, 'kbit')
  }
  signal.throwIfAborted()
  await client.request('PATCH', path, { config, ...(Object.keys(devices).length ? { devices } : {}) }, 30000, signal)
}
