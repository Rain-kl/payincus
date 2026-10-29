import { createCipheriv, createDecipheriv, createHash, randomBytes, timingSafeEqual } from 'node:crypto'
import { Prisma } from '@prisma/client'
import { prisma } from '../../db/prisma.js'
import { unseal } from './vault.js'

export const startupWindowMs = 180_000
export interface StartupEnvelope { iv: string; ciphertext: string }
const aad = (agentId: string, direction: 'request' | 'response') => Buffer.from(`payincus-startup-v1:${direction}:${agentId}`)
const key = (secret: string) => createHash('sha256').update(`payincus-startup-v1\0${secret}`).digest()
export function sealStartup(secret: string, agentId: string, direction: 'request' | 'response', value: unknown): StartupEnvelope {
  const iv = randomBytes(12), material = key(secret)
  try {
    const cipher = createCipheriv('aes-256-gcm', material, iv)
    cipher.setAAD(aad(agentId, direction))
    const plaintext = Buffer.from(JSON.stringify(value))
    try { return { iv: iv.toString('base64'), ciphertext: Buffer.concat([cipher.update(plaintext), cipher.final(), cipher.getAuthTag()]).toString('base64') } }
    finally { plaintext.fill(0) }
  } finally { material.fill(0) }
}
export function openStartup(secret: string, agentId: string, direction: 'request' | 'response', input: unknown): any {
  const value = input as StartupEnvelope
  if (!value || typeof value.iv !== 'string' || typeof value.ciphertext !== 'string' || value.ciphertext.length > 90_000) throw new Error('invalid envelope')
  const iv = Buffer.from(value.iv, 'base64'), data = Buffer.from(value.ciphertext, 'base64'), material = key(secret)
  try {
    if (iv.length !== 12 || data.length < 16) throw new Error('invalid envelope')
    const cipher = createDecipheriv('aes-256-gcm', material, iv)
    cipher.setAAD(aad(agentId, direction)); cipher.setAuthTag(data.subarray(-16))
    const plaintext = Buffer.concat([cipher.update(data.subarray(0, -16)), cipher.final()])
    try { return JSON.parse(plaintext.toString()) } finally { plaintext.fill(0) }
  } finally { material.fill(0) }
}
export function verifyStartupProof(secret: string, agentId: string, envelope: unknown, now = Date.now()): { nonce: string; timestamp: number } {
  const value = openStartup(secret, agentId, 'request', envelope)
  const actual = typeof value?.token === 'string' ? Buffer.from(value.token) : Buffer.alloc(0), expected = Buffer.from(secret)
  if (actual.length !== expected.length || !timingSafeEqual(actual, expected) || !Number.isSafeInteger(value.timestamp) || Math.abs(now - value.timestamp) > startupWindowMs || typeof value.nonce !== 'string' || !/^[A-Za-z0-9._:-]{8,128}$/.test(value.nonce)) throw new Error('invalid proof')
  return { nonce: value.nonce, timestamp: value.timestamp }
}

// Called only after ordinary node HMAC identity and its outer nonce have been verified.
export async function grantStartupKeys(agent: { hostId: number; agentId: string }, secret: string, input: unknown) {
  let proof: ReturnType<typeof verifyStartupProof>
  try { proof = verifyStartupProof(secret, agent.agentId, input) }
  catch {
    const generation = await prisma.$transaction(async tx => {
      await tx.$executeRaw`SELECT pg_advisory_xact_lock(700260, ${agent.hostId})`
      const previous = await tx.nodeSecurityState.findUnique({ where: { hostId: agent.hostId } })
      const report = { ...(previous?.report as Record<string, unknown> ?? {}), locked: true, reason: 'startup_verification' }
      const locked = await tx.nodeSecurityState.upsert({ where: { hostId: agent.hostId }, create: { hostId: agent.hostId, recoveryBlocked: true, recoveryGeneration: 1, report: { locked: true, enabled: true, revision: 0, reason: 'startup_verification', results: [] } }, update: { recoveryBlocked: true, recoveryGeneration: { increment: 1 }, report: report as Prisma.InputJsonObject } })
      await tx.log.create({ data: { userId: null, module: 'security', action: 'node.startup.rejected', content: `节点 #${agent.hostId} 启动凭据验证失败，已锁定并等待管理员核验`, result: 'warning' } })
      return locked.recoveryGeneration
    })
    return { invalid: true as const, generation }
  }
  return prisma.$transaction(async tx => {
    await tx.$executeRaw`SELECT pg_advisory_xact_lock(700260, ${agent.hostId})`
    const state = await tx.nodeSecurityState.findUnique({ where: { hostId: agent.hostId } })
    const report = state?.report as { locked?: boolean } | null
    if (!state || typeof report?.locked !== 'boolean') return { unavailable: true as const }
    if (state.recoveryBlocked || report?.locked !== false) return { envelope: sealStartup(secret, agent.agentId, 'response', { nonce: proof.nonce, locked: true, generation: state.recoveryGeneration, keys: [] }) }
    try {
      await tx.hostAgentNonce.create({ data: { agentId: agent.agentId, nonce: `startup:${proof.nonce}`, expiresAt: new Date(Date.now() + 2 * startupWindowMs) } })
    } catch (error) {
      if (error instanceof Prisma.PrismaClientKnownRequestError && error.code === 'P2002') return { replay: true as const }
      throw error
    }
    const records = await tx.nodeStorageKey.findMany({ where: { hostId: agent.hostId }, take: 257 })
    if (records.length > 256) return { unavailable: true as const }
    const keys = records.map(record => ({ dataset: record.dataset, key: unseal(record.ciphertext, { hostId: agent.hostId, dataset: record.dataset, version: record.version }) }))
    try { return { envelope: sealStartup(secret, agent.agentId, 'response', { nonce: proof.nonce, locked: false, generation: state.recoveryGeneration, keys }) } }
    finally { for (const item of keys) item.key = '' }
  })
}
