import { createCipheriv, createDecipheriv, createHash, hkdfSync, randomBytes } from 'node:crypto'

export interface KeyIdentity { hostId: number; dataset: string; version: number }
export const datasetPattern = /^[A-Za-z][A-Za-z0-9_.:-]*(\/[A-Za-z0-9_.:-]+)+$/
export function fingerprint(key: string): string {
  if (!/^[0-9a-f]{64}$/.test(key)) throw new Error('需要 64 位十六进制随机 ZFS 密钥')
  return createHash('sha256').update(Buffer.from(key, 'hex')).digest('hex')
}
function identity(value: KeyIdentity): Buffer {
  if (!Number.isSafeInteger(value.hostId) || value.hostId < 1 || !Number.isSafeInteger(value.version) || value.version < 1 || !datasetPattern.test(value.dataset) || value.dataset.includes('..') || value.dataset.length > 200) throw new Error('无效的存储身份')
  return Buffer.from(JSON.stringify([value.hostId, value.dataset, value.version]))
}
function master(secret: string): Buffer {
  if (!secret || secret.length < 32) throw new Error('控制端 ENCRYPTION_KEY 未配置或强度不足')
  return Buffer.from(hkdfSync('sha256', secret, 'payincus-node-storage', 'vault-v1', 32))
}
export function seal(key: string, value: KeyIdentity, secret = process.env.ENCRYPTION_KEY || ''): string {
  fingerprint(key)
  const material = master(secret)
  try {
    const iv = randomBytes(12)
    const cipher = createCipheriv('aes-256-gcm', material, iv)
    cipher.setAAD(identity(value))
    const encrypted = Buffer.concat([cipher.update(Buffer.from(key, 'hex')), cipher.final()])
    return ['v1', iv.toString('hex'), cipher.getAuthTag().toString('hex'), encrypted.toString('hex')].join(':')
  } finally { material.fill(0) }
}
export function unseal(envelope: string, value: KeyIdentity, secret = process.env.ENCRYPTION_KEY || ''): string {
  const parts = envelope.split(':')
  if (parts.length !== 4 || parts[0] !== 'v1' || !/^[0-9a-f]{24}$/.test(parts[1]) || !/^[0-9a-f]{32}$/.test(parts[2]) || !/^[0-9a-f]{64}$/.test(parts[3])) throw new Error('存储密钥格式不兼容')
  const material = master(secret)
  try {
    const decipher = createDecipheriv('aes-256-gcm', material, Buffer.from(parts[1], 'hex'))
    decipher.setAAD(identity(value))
    decipher.setAuthTag(Buffer.from(parts[2], 'hex'))
    const plain = Buffer.concat([decipher.update(Buffer.from(parts[3], 'hex')), decipher.final()])
    try { return plain.toString('hex') } finally { plain.fill(0) }
  } catch { throw new Error('存储密钥校验失败') } finally { material.fill(0) }
}
