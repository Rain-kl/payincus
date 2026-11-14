import { http } from './index'
import type { HostInstanceBackupTask } from './host-instance-backup-types'

export interface InstanceTransferTask extends HostInstanceBackupTask {
  kind?: 'export' | 'import'
  instance?: string
  filePath?: string
  operationTaskId?: number | null
}

export interface UnassignedHostInstance {
  incusName: string
  instanceType: 'container' | 'vm'
  status: 'running' | 'stopped'
  cpu: number
  memory: number
  disk: number
  storagePoolName: string | null
  ipv4: string | null
  ipv6: string | null
  networkMode: string
  image: string
  createdAt: string | null
}

export interface ImportStoragePoolOption {
  name: string
  driver: string
  purpose?: 'instance_data' | 'instance_storage' | null
  status?: string
}

export const hostTransferApi = {
  getTransferTasks: (hostId: number): Promise<{ supported: boolean; tasks: InstanceTransferTask[] }> =>
    http.get(`/admin/hosts/${hostId}/instance-transfer`, { timeout: 0 }),

  getImportStoragePools: (hostId: number): Promise<{ pools: ImportStoragePoolOption[] }> =>
    http.get(`/admin/hosts/${hostId}/import-storage-pools`),

  exportInstance: (
    hostId: number,
    instanceId: number,
    data: { id?: string; targetPath: string; mode?: 'directory' | 'file' }
  ): Promise<InstanceTransferTask> =>
    http.post(`/admin/hosts/${hostId}/instances/${instanceId}/export`, data, { timeout: 0 }),

  importInstance: (
    hostId: number,
    data: { id?: string; filePath: string; name?: string; storagePool?: string }
  ): Promise<InstanceTransferTask> =>
    http.post(`/admin/hosts/${hostId}/instances/import`, data, { timeout: 0 }),

  getUnassignedInstances: (hostId: number): Promise<{ instances: UnassignedHostInstance[] }> =>
    http.get(`/admin/hosts/${hostId}/unassigned-instances`),

  assignUnassignedInstance: (
    hostId: number,
    incusName: string,
    data: { userId: number; name?: string }
  ): Promise<{ instance: { id: number; name: string; incusId: string; userId: number } }> =>
    http.post(`/admin/hosts/${hostId}/unassigned-instances/${encodeURIComponent(incusName)}/assign`, data),

  searchUsers: (params?: { search?: string; pageSize?: number }): Promise<{
    users: Array<{ id: number; username: string; email: string }>
    total: number
  }> => http.get('/users', { params })
}
