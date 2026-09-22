import { prisma } from '../db/prisma.js'
import {
  Prisma,
  type PromoCode,
  type PromoDiscountType,
  type PromoDurationType,
  type InstancePromoBinding,
  type PromoRedemptionLog
} from '@prisma/client'
import {
  getPromoCodeByCode,
  getInstancePromoBinding,
  incrementPromoUsageWithLock,
  bindPromoToInstance,
  updateInstancePromoBindingCycles,
  unbindPromoFromInstance,
  type PromoCodeWithDetails,
  type InstancePromoBindingWithCode
} from '../db/promo-codes.js'
import {
  createPromoRedemptionLog,
  countUserPromoRedemptions,
  getPromoRedemptionLogsByInstance
} from '../db/promo-redemptions.js'
import {
  calculateCreationQuote,
  calculatePromoRenewalSplit
} from './promo/promo-calculator.js'
import { isProductEligibleForPromo } from './promo/scope-matcher.js'
import type {
  CreationQuoteResult,
  RenewalSplitResult
} from './promo/types.js'

export interface PromoValidateParams {
  code: string
  packageId: number
  packagePlanId: number
  userId: number
  targetInstanceId?: number
  tx?: Prisma.TransactionClient
}

export interface PromoValidationResult {
  valid: boolean
  error?: string
  errorCode?: string
  promoCode?: PromoCodeWithDetails
  discountType?: PromoDiscountType
  discountValue?: number
  durationType?: PromoDurationType
  durationCycles?: number | null
  description?: string
}

export interface PromoSettleCreateParams {
  promoCodeId: number
  instanceId: number
  userId: number
  originalPrice?: number
  discountAmount?: number
  finalPrice?: number
  durationType: PromoDurationType
  durationCycles?: number | null
  commissionAmount?: number
  commissionRate?: number
  quote?: {
    discountAmount: number
    finalPrice: number
    originalPrice?: number
  }
}

export interface PromoSettleCreateResult {
  promoCode: PromoCode
  redemptionLog: PromoRedemptionLog
  binding: InstancePromoBinding | null
}

export interface PromoSettleRenewParams {
  instanceId: number
  userId: number
  monthlyPrice: number
  months: number
  binding?: InstancePromoBindingWithCode | null
  commissionAmount?: number
}

export interface PromoSettleRenewResult {
  quote: RenewalSplitResult
  unbound: boolean
  redemptionLog: PromoRedemptionLog
}

export interface PromoRefundEligibilityResult {
  isRefundable: boolean
  reason?: string
}

/**
 * 统一优惠码编排门面服务 (PromoCodeEngine)
 */
export class PromoCodeEngine {
  /**
   * 校验优惠码合法性及适用性
   */
  static async validate(params: PromoValidateParams): Promise<PromoValidationResult> {
    const code = params.code?.trim()
    if (!code) {
      return {
        valid: false,
        error: '优惠码不能为空',
        errorCode: 'PROMO_NOT_FOUND'
      }
    }

    let promo = await getPromoCodeByCode(code, params.tx)
    if (!promo && code.toUpperCase() !== code) {
      promo = await getPromoCodeByCode(code.toUpperCase(), params.tx)
    }
    if (!promo) {
      return {
        valid: false,
        error: '优惠码不存在',
        errorCode: 'PROMO_NOT_FOUND'
      }
    }

    if (!promo.enabled) {
      return {
        valid: false,
        error: '优惠码已被禁用',
        errorCode: 'PROMO_DISABLED'
      }
    }

    const now = new Date()
    if (promo.startsAt && now < promo.startsAt) {
      return {
        valid: false,
        error: '优惠码活动尚未开始',
        errorCode: 'PROMO_NOT_STARTED'
      }
    }

    if (promo.expiresAt && now > promo.expiresAt) {
      return {
        valid: false,
        error: '优惠码已过期',
        errorCode: 'PROMO_EXPIRED'
      }
    }

    if (promo.maxTotalUses !== null && promo.usedTotalCount >= promo.maxTotalUses) {
      return {
        valid: false,
        error: '优惠码使用次数已达上限',
        errorCode: 'PROMO_QUOTA_EXCEEDED'
      }
    }

    if (promo.maxUsesPerUser !== null) {
      const userUses = await countUserPromoRedemptions(promo.id, params.userId, 'create', params.tx)
      if (userUses >= promo.maxUsesPerUser) {
        return {
          valid: false,
          error: '您已达到该优惠码的最大使用次数',
          errorCode: 'PROMO_USER_LIMIT_EXCEEDED'
        }
      }
    }

    // 推广码防自购互刷（Self-referral check）
    if (promo.type === 'AFF_USER' && promo.userId === params.userId) {
      return {
        valid: false,
        error: '不能使用自己的推广优惠码',
        errorCode: 'CANNOT_USE_OWN_AFF_CODE'
      }
    }

    // 检查目标实例是否已存在活跃绑定
    if (params.targetInstanceId) {
      const binding = await getInstancePromoBinding(params.targetInstanceId, params.tx)
      if (binding) {
        return {
          valid: false,
          error: '该实例已绑定其他优惠码',
          errorCode: 'PROMO_BINDING_ACTIVE'
        }
      }
    }

    // 树形适用范围冒泡匹配
    const eligible = isProductEligibleForPromo(promo, {
      packageId: params.packageId,
      packagePlanId: params.packagePlanId
    })
    if (!eligible) {
      return {
        valid: false,
        error: '该优惠码不适用于当前套餐或方案',
        errorCode: 'PROMO_SCOPE_MISMATCH'
      }
    }

    return {
      valid: true,
      promoCode: promo,
      discountType: promo.discountType,
      discountValue: Number(promo.discountValue),
      durationType: promo.durationType,
      durationCycles: promo.durationCycles,
      description: promo.name ?? undefined
    }
  }

  /**
   * 计算新购开机时的优惠与最终价格
   */
  static calculateCreationQuote(
    planPrice: number,
    promo: { discountType: PromoDiscountType; discountValue: Prisma.Decimal | number }
  ): CreationQuoteResult {
    return calculateCreationQuote({
      originalPrice: planPrice,
      discountType: promo.discountType,
      discountValue: Number(promo.discountValue)
    })
  }

  /**
   * 计算续费时的周期切片拆分与优惠明细
   */
  static calculateRenewalQuote(
    monthlyPrice: number,
    months: number,
    binding: InstancePromoBindingWithCode
  ): RenewalSplitResult {
    return calculatePromoRenewalSplit({
      monthlyPrice,
      months,
      discountRate: Number(binding.promoCode.discountValue),
      remainingCycles: binding.remainingCycles
    })
  }

  /**
   * 新购（创建实例）成功后的核销结算与实例绑定
   */
  static async settleOnCreate(
    tx: Prisma.TransactionClient,
    params: PromoSettleCreateParams
  ): Promise<PromoSettleCreateResult> {
    const discountAmount = params.quote?.discountAmount ?? params.discountAmount ?? 0
    const finalPrice = params.quote?.finalPrice ?? params.finalPrice ?? 0
    const originalPrice = params.quote?.originalPrice ?? params.originalPrice ?? Number((finalPrice + discountAmount).toFixed(2))
    const commissionAmount = params.commissionAmount ?? (params.commissionRate ? Number((originalPrice * params.commissionRate).toFixed(2)) : 0)

    // 1. 原子递增优惠码总核销数与减免总额（带行锁保护防并发超额）
    const updatedPromo = await incrementPromoUsageWithLock(tx, params.promoCodeId, discountAmount)

    // 2. 记录流水日志
    const redemptionLog = await createPromoRedemptionLog(tx, {
      promoCodeId: params.promoCodeId,
      instanceId: params.instanceId,
      userId: params.userId,
      actionType: 'create',
      cycleIndex: 1,
      originalPrice,
      discountAmount,
      finalPrice,
      commissionAmount
    })

    // 3. 实例绑定决策
    let binding: InstancePromoBinding | null = null

    if (discountAmount > 0 && params.durationType !== 'ONCE') {
      if (params.durationType === 'REPEATING') {
        const totalCycles = params.durationCycles || 1
        const remainingCycles = Math.max(0, totalCycles - 1)
        if (remainingCycles > 0) {
          binding = await bindPromoToInstance(tx, {
            instanceId: params.instanceId,
            promoCodeId: params.promoCodeId,
            durationType: params.durationType,
            totalCycles,
            usedCycles: 1,
            remainingCycles
          })
        }
      } else if (params.durationType === 'FOREVER') {
        binding = await bindPromoToInstance(tx, {
          instanceId: params.instanceId,
          promoCodeId: params.promoCodeId,
          durationType: params.durationType,
          totalCycles: null,
          usedCycles: 1,
          remainingCycles: null
        })
      }
    }

    return {
      promoCode: updatedPromo,
      redemptionLog,
      binding
    }
  }

  /**
   * 续费成功后的核销切片结算与绑定状态更新
   */
  static async settleOnRenew(
    tx: Prisma.TransactionClient,
    params: PromoSettleRenewParams
  ): Promise<PromoSettleRenewResult> {
    const binding = params.binding ?? (await getInstancePromoBinding(params.instanceId, tx))
    if (!binding) {
      throw new Error('PROMO_BINDING_NOT_FOUND')
    }

    const quote = calculatePromoRenewalSplit({
      monthlyPrice: params.monthlyPrice,
      months: params.months,
      discountRate: Number(binding.promoCode.discountValue),
      remainingCycles: binding.remainingCycles
    })

    // 更新优惠码累计减免金额
    if (quote.discountAmount > 0) {
      await tx.promoCode.update({
        where: { id: binding.promoCodeId },
        data: {
          totalDiscountAmount: {
            increment: new Prisma.Decimal(quote.discountAmount.toFixed(2))
          }
        }
      })
    }

    const originalPrice = Number((params.monthlyPrice * params.months).toFixed(2))
    const redemptionLog = await createPromoRedemptionLog(tx, {
      promoCodeId: binding.promoCodeId,
      instanceId: params.instanceId,
      userId: params.userId,
      actionType: 'renew',
      cycleIndex: binding.usedCycles + 1,
      months: params.months,
      originalPrice,
      discountAmount: quote.discountAmount,
      finalPrice: quote.finalAmount,
      commissionAmount: params.commissionAmount ?? 0
    })

    if (quote.willUnbind) {
      await unbindPromoFromInstance(tx, params.instanceId)
      return {
        quote,
        unbound: true,
        redemptionLog
      }
    } else {
      await updateInstancePromoBindingCycles(tx, params.instanceId, {
        consumedCycles: quote.consumedCycles,
        newRemainingCycles: quote.newRemainingCycles
      })
      return {
        quote,
        unbound: false,
        redemptionLog
      }
    }
  }

  /**
   * 判定实例当前是否允许退款（资损风控判定）
   */
  static async getPromoRefundEligibility(
    instanceId: number,
    tx?: Prisma.TransactionClient
  ): Promise<PromoRefundEligibilityResult> {
    const client = tx ?? prisma

    // 1. 检查活跃绑定：如果存在正在生效的绑定，不可退款
    const binding = await getInstancePromoBinding(instanceId, tx)
    if (binding) {
      return {
        isRefundable: false,
        reason: 'PROMO_BINDING_ACTIVE'
      }
    }

    // 2. 检查首月一次性优惠：如果存在 create 记录
    const logs = await getPromoRedemptionLogsByInstance(instanceId, tx)
    const hasCreatePromo = logs.some(log => log.actionType === 'create')

    if (hasCreatePromo) {
      // 检查是否存在已付费的续费计费记录
      let renewCount = 0
      if (typeof (client as any).billingRecord?.count === 'function') {
        renewCount = await (client as any).billingRecord.count({
          where: { instanceId, type: 'renew', status: 'paid' }
        })
      } else if (typeof (client as any).instanceBillingRecord?.count === 'function') {
        renewCount = await (client as any).instanceBillingRecord.count({
          where: { instanceId, type: 'renew' }
        })
      }

      if (renewCount === 0) {
        return {
          isRefundable: false,
          reason: 'PROMO_FIRST_CYCLE_LOCKED'
        }
      }
    }

    return {
      isRefundable: true
    }
  }
}

export default PromoCodeEngine
