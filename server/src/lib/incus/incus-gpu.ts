import type { IncusClient } from './incus-client.js'
import { execWithOutput } from './incus-instances.js'
import { GpuError } from '../gpu.js'

/** Verify the injected userspace binary/loader, NVML and all assigned devices. */
export async function verifyGpuRuntime(client: IncusClient, name: string, expectedCount: number): Promise<void> {
  try {
    const result = await execWithOutput(client, name, ['/usr/bin/nvidia-smi', '--query-gpu=uuid', '--format=csv,noheader'], {}, 20_000)
    const code = (result.metadata as { return?: number } | null)?.return
    if (code !== 0) throw new Error(result.stderr.trim().slice(0, 400) || `nvidia-smi exit=${code ?? 'unknown'}`)
    const uuids = result.stdout.trim().split(/\r?\n/).map(value => value.trim()).filter(Boolean)
    if (uuids.length !== expectedCount || new Set(uuids).size !== expectedCount || uuids.some(uuid => !/^GPU-[a-fA-F0-9-]{16,64}$/.test(uuid))) {
      throw new Error(`nvidia-smi 可见 GPU 数量与分配不符（预期 ${expectedCount}）`)
    }
  } catch (error) {
    const reason = error instanceof Error ? error.message.slice(0, 400) : 'unknown error'
    throw new GpuError(`NVIDIA GPU 运行时校验失败：请使用带 glibc 的 GPU 镜像（如 Ubuntu/Debian），并检查宿主驱动与 NVIDIA Container Toolkit。${reason}`)
  }
}
