import { prisma } from './prisma.js'
import { Prisma, type PromoRedemptionLog } from '@prisma/client'

/**
 * 记录优惠码核销明细流水
 */
export async function createPromoRedemptionLog(
  tx: Prisma.TransactionClient,
  data: {
    promoCodeId: number
    instanceId: number
    userId: number
    actionType: 'create' | 'renew'
    cycleIndex: number
    months?: number
    originalPrice: number
    discountAmount: number
    finalPrice: number
    commissionAmount?: number
  }
): Promise<PromoRedemptionLog> {
  return tx.promoRedemptionLog.create({
    data: {
      promoCodeId: data.promoCodeId,
      instanceId: data.instanceId,
      userId: data.userId,
      actionType: data.actionType,
      cycleIndex: data.cycleIndex,
      months: data.months ?? 1,
      originalPrice: new Prisma.Decimal(data.originalPrice.toFixed(2)),
      discountAmount: new Prisma.Decimal(data.discountAmount.toFixed(2)),
      finalPrice: new Prisma.Decimal(data.finalPrice.toFixed(2)),
      commissionAmount: new Prisma.Decimal((data.commissionAmount ?? 0).toFixed(2))
    }
  })
}

/**
 * 获取指定实例的所有优惠码核销记录（按创建时间倒序）
 */
export async function getPromoRedemptionLogsByInstance(
  instanceId: number,
  tx?: Prisma.TransactionClient
): Promise<PromoRedemptionLog[]> {
  const client = tx ?? prisma
  return client.promoRedemptionLog.findMany({
    where: { instanceId },
    orderBy: { createdAt: 'desc' }
  })
}

/**
 * 统计指定用户对指定优惠码的已使用次数（用于单用户限用次数校验）
 */
export async function countUserPromoRedemptions(
  promoCodeId: number,
  userId: number,
  actionType?: 'create' | 'renew',
  tx?: Prisma.TransactionClient
): Promise<number>
export async function countUserPromoRedemptions(
  promoCodeId: number,
  userId: number,
  tx?: Prisma.TransactionClient
): Promise<number>
export async function countUserPromoRedemptions(
  promoCodeId: number,
  userId: number,
  actionTypeOrTx?: 'create' | 'renew' | Prisma.TransactionClient,
  tx?: Prisma.TransactionClient
): Promise<number> {
  let actionType: 'create' | 'renew' | undefined
  let client: Prisma.TransactionClient | typeof prisma

  if (typeof actionTypeOrTx === 'string') {
    actionType = actionTypeOrTx
    client = tx ?? prisma
  } else if (actionTypeOrTx && typeof actionTypeOrTx === 'object') {
    actionType = undefined
    client = actionTypeOrTx
  } else {
    actionType = undefined
    client = tx ?? prisma
  }

  return client.promoRedemptionLog.count({
    where: {
      promoCodeId,
      userId,
      ...(actionType ? { actionType } : {})
    }
  })
}

