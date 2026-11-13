export interface HostInstanceBackupItem {
  project: string
  name: string
  status: 'pending' | 'running' | 'success' | 'failed' | 'interrupted'
  file?: string
  bytes: number
  error?: string
}
export interface HostInstanceBackupTask {
  id: string
  directory: string
  status: 'running' | 'success' | 'failed' | 'partial' | 'interrupted'
  createdAt: string
  finishedAt?: string
  items: HostInstanceBackupItem[]
  error?: string
}
