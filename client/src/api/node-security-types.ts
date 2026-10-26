export interface NodeSecurityState {
  enabled: boolean
  locked: boolean
  revision: number
  reason?: string
  targets?: Array<{ dataset: string; fingerprint: string }>
  results?: Array<{ target: string; success: boolean; detail: string }>
}
export interface NodeSecurityOverview {
  serverFingerprint?: string | null
  state: NodeSecurityState | null
  offline?: boolean
  reportedAt?: string
  keys: Array<{ id: number; dataset: string; fingerprint: string; version: number }>
  events: Array<{ eventId: string; command: string; uid: number; pid: number; critical: boolean; blocked: boolean; occurredAt: string }>
}
