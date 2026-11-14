import { http } from './index'
import type { HostInstanceBackupTask } from './host-instance-backup-types'

export interface InstanceTransferTask extends HostInstanceBackupTask {
  kind?: 'export' | 'import'
  instance?: string
  filePath?: string
  operationTaskId?: number | null
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

export interface ImportPackagePreset {
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

export interface PackageBindingPayload {
  packageId?: number | null
  planId?: number | null
  validDays?: number | null
  expiresAt?: string | null
}

export const PACKAGE_NONE_SENTINEL = '__none__'

export function buildPackageBindingPayload(
  packageChoice: string,
  planChoice: string,
  validDaysText: string
): PackageBindingPayload {
  if (!packageChoice || packageChoice === PACKAGE_NONE_SENTINEL) {
    return { packageId: null, planId: null, validDays: null }
  }
  const packageId = Number(packageChoice)
  if (!Number.isFinite(packageId) || packageId <= 0) {
    return { packageId: null, planId: null, validDays: null }
  }
  const hasPlan = Boolean(planChoice && planChoice !== PACKAGE_NONE_SENTINEL && Number(planChoice) > 0)
  const planId = hasPlan ? Number(planChoice) : null
  const parsedDays = Number(validDaysText.trim())
  const validDays = hasPlan && validDaysText.trim() !== '' && Number.isInteger(parsedDays) && parsedDays >= 1
    ? parsedDays
    : null
  return { packageId, planId, validDays }
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
  preset?: ImportPackagePreset | null
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

  getImportPackages: (hostId: number): Promise<{ packages: ImportPackageOption[] }> =>
    http.get(`/admin/hosts/${hostId}/import-packages`),

  importInstance: (
    hostId: number,
    data: {
      id?: string
      filePath: string
      name?: string
      storagePool?: string
      userId?: number | null
      displayName?: string | null
    } & PackageBindingPayload
  ): Promise<InstanceTransferTask> =>
    http.post(`/admin/hosts/${hostId}/instances/import`, data, { timeout: 0 }),

  getUnassignedInstances: (hostId: number): Promise<{ instances: UnassignedHostInstance[] }> =>
    http.get(`/admin/hosts/${hostId}/unassigned-instances`),

  assignUnassignedInstance: (
    hostId: number,
    incusName: string,
    data: { userId: number; name?: string } & PackageBindingPayload
  ): Promise<{
    instance: {
      id: number
      name: string
      incusId: string
      userId: number
      packageId?: number | null
      packagePlanId?: number | null
      packageName?: string | null
      planName?: string | null
      expiresAt?: string | null
      billingPrice?: number | null
    }
  }> =>
    http.post(`/admin/hosts/${hostId}/unassigned-instances/${encodeURIComponent(incusName)}/assign`, data),

  bindInstancePackage: (
    hostId: number,
    instanceId: number,
    data: PackageBindingPayload
  ): Promise<{
    instance: {
      id: number
      packageId: number | null
      packagePlanId: number | null
      packageName: string | null
      planName: string | null
      billingPrice: number | null
      billingCycle: number | null
      expiresAt: string | null
    }
  }> =>
    http.put(`/admin/hosts/${hostId}/instances/${instanceId}/package`, data),

  searchUsers: (params?: { search?: string; pageSize?: number }): Promise<{
    users: Array<{ id: number; username: string; email: string }>
    total: number
  }> => http.get('/users', { params })
}
