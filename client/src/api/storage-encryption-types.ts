export interface StorageEncryptionState {
  encrypted: boolean
  state: 'plain' | 'locked' | 'unlocked'
  dataset: string
  encryptionRoot: string
  keySaved: boolean
  keyFormat?: string
  manualLocked?: boolean
  lockPending?: boolean
  pendingRecovery?: number
  pendingImport?: boolean
  nodeLocked?: boolean
}
