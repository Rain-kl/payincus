import { prisma } from './prisma.js'
import { Prisma, type PromoCode, type InstancePromoBinding, type PromoDurationType } from '@prisma/client'

export type PromoCodeWithDetails = Prisma.PromoCodeGetPayload<{
  include: {
    scopes: true
    _count: { select: { bindings: true; redemptionLogs: true } }
  }
}>

export type InstancePromoBindingWithCode = Prisma.InstancePromoBindingGetPayload<{
  include: {
    promoCode: {
      include: { scopes: true }
    }
  }
}>

/**
 * 根据优惠码字符串查询优惠码详情（含适用范围与统计信息）
 */
export async function getPromoCodeByCode(
  code: string,
  tx?: Prisma.TransactionClient
): Promise<PromoCodeWithDetails | null> {
  const client = tx ?? prisma
  return client.promoCode.findUnique({
    where: { code: code.trim() },
    include: {
      scopes: true,
      _count: { select: { bindings: true, redemptionLogs: true } }
    }
  })
}

/**
 * 根据 ID 查询优惠码详情（含适用范围与统计信息）
 */
export async function getPromoCodeById(
  id: number,
  tx?: Prisma.TransactionClient
): Promise<PromoCodeWithDetails | null> {
  const client = tx ?? prisma
  return client.promoCode.findUnique({
    where: { id },
    include: {
      scopes: true,
      _count: { select: { bindings: true, redemptionLogs: true } }
    }
  })
}

/**
 * 原子检查约束并递增优惠码使用次数与累计减免金额（带行级排他锁保护）
 */
export async function incrementPromoUsageWithLock(
  tx: Prisma.TransactionClient,
  promoCodeId: number,
  discountAmount: number = 0
): Promise<PromoCode> {
  // 1. 若支持 $queryRaw（PostgreSQL 事务客户端），先加 FOR UPDATE 行级排他锁，防止并发超额核销
  if (typeof (tx as any).$queryRaw === 'function') {
    await tx.$queryRaw(Prisma.sql`
      SELECT id FROM "promo_codes" WHERE id = ${promoCodeId} FOR UPDATE
    `)
  }

  // 2. 读取锁定状态下的最新优惠码数据
  const promo = await tx.promoCode.findUnique({
    where: { id: promoCodeId }
  })

  if (!promo) {
    throw new Error('PROMO_NOT_FOUND')
  }

  if (!promo.enabled) {
    throw new Error('PROMO_DISABLED')
  }

  const now = new Date()
  if (promo.startsAt && promo.startsAt > now) {
    throw new Error('PROMO_NOT_STARTED')
  }

  if (promo.expiresAt && promo.expiresAt <= now) {
    throw new Error('PROMO_EXPIRED')
  }

  if (promo.maxTotalUses !== null && promo.usedTotalCount >= promo.maxTotalUses) {
    throw new Error('PROMO_QUOTA_EXCEEDED')
  }

  // 3. 执行原子递增
  const validDiscount = typeof discountAmount === 'number' && Number.isFinite(discountAmount) && discountAmount > 0
    ? Number(discountAmount.toFixed(2))
    : 0

  return tx.promoCode.update({
    where: { id: promoCodeId },
    data: {
      usedTotalCount: { increment: 1 },
      ...(validDiscount > 0
        ? { totalDiscountAmount: { increment: new Prisma.Decimal(validDiscount.toFixed(2)) } }
        : {})
    }
  })
}

/**
 * 获取指定实例当前活跃的优惠码绑定（含优惠码详情与适用范围）
 */
export async function getInstancePromoBinding(
  instanceId: number,
  tx?: Prisma.TransactionClient
): Promise<InstancePromoBindingWithCode | null> {
  const client = tx ?? prisma
  return client.instancePromoBinding.findUnique({
    where: { instanceId },
    include: {
      promoCode: {
        include: { scopes: true }
      }
    }
  })
}

/**
 * 将优惠码绑定到指定实例（若已绑定则更新）
 */
export async function bindPromoToInstance(
  tx: Prisma.TransactionClient,
  params: {
    instanceId: number
    promoCodeId: number
    durationType: PromoDurationType
    totalCycles?: number | null
    usedCycles?: number
    remainingCycles?: number | null
  }
): Promise<InstancePromoBinding> {
  const {
    instanceId,
    promoCodeId,
    durationType,
    totalCycles = null,
    usedCycles = 0,
    remainingCycles = null
  } = params

  return tx.instancePromoBinding.upsert({
    where: { instanceId },
    create: {
      instanceId,
      promoCodeId,
      durationType,
      totalCycles,
      usedCycles,
      remainingCycles,
      boundAt: new Date(),
      lastRedeemedAt: new Date()
    },
    update: {
      promoCodeId,
      durationType,
      totalCycles,
      usedCycles,
      remainingCycles,
      lastRedeemedAt: new Date()
    }
  })
}

/**
 * 续费时更新实例绑定的已核销期数与剩余期数
 */
export async function updateInstancePromoBindingCycles(
  tx: Prisma.TransactionClient,
  instanceId: number,
  params: {
    consumedCycles: number
    newRemainingCycles: number | null
  }
): Promise<InstancePromoBinding> {
  return tx.instancePromoBinding.update({
    where: { instanceId },
    data: {
      usedCycles: { increment: params.consumedCycles },
      remainingCycles: params.newRemainingCycles,
      lastRedeemedAt: new Date()
    }
  })
}

/**
 * 解除实例与优惠码的绑定关系
 */
export async function unbindPromoFromInstance(
  tx: Prisma.TransactionClient,
  instanceId: number
): Promise<{ success: boolean }> {
  const result = await tx.instancePromoBinding.deleteMany({
    where: { instanceId }
  })
  return { success: result.count > 0 }
}
