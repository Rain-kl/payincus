export interface NodeSecurityState {
  enabled: boolean
  locked: boolean
  revision: number
  reason?: string
  resumeInstances?: Array<{ project: string; name: string; uuid: string; frozen: boolean }>
  targets?: Array<{ dataset: string; fingerprint: string }>
  results?: Array<{ target: string; success: boolean; detail: string }>
}
export interface NodeRecoveryTask {
  id: number
  status: 'running' | 'success' | 'failed' | 'cancelled' | 'interrupted'
  error: string | null
  createdAt: string
  finishedAt: string | null
}
export interface NodeSecurityOverview {
  recoveryTask?: NodeRecoveryTask | null
  serverFingerprint?: string | null
  state: NodeSecurityState | null
  offline?: boolean
  reportedAt?: string
  keys: Array<{ id: number; dataset: string; fingerprint: string; version: number }>
  events: Array<{ eventId: string; command: string; uid: number; pid: number; critical: boolean; blocked: boolean; occurredAt: string }>
}
