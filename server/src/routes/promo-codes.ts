import type { FastifyInstance, FastifyRequest, FastifyReply } from 'fastify'
import { prisma } from '../db/prisma.js'
import { PromoCodeEngine } from '../services/promo-engine.js'
import { getInstancePromoBinding, bindPromoToInstance } from '../db/promo-codes.js'
import { calculateCreateBilling, calculateMonthlyPrice } from '../db/billing-operations.js'
import { apiError, ErrorCode } from '../lib/errors.js'

const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\d*$/

function parsePositiveRouteId(value: string): number | null {
  if (!POSITIVE_ROUTE_ID_PATTERN.test(value)) return null
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) ? parsed : null
}

export default async function promoCodesRoutes(fastify: FastifyInstance) {
  // ==================== 1. POST /validate ====================
  fastify.post('/validate', {
    onRequest: [fastify.authenticate]
  }, async (request: FastifyRequest, _reply: FastifyReply) => {
    const body = (request.body || {}) as {
      code?: string
      packageId?: number
      packagePlanId?: number
    }

    const trimmedCode = (body.code || '').trim()
    if (!trimmedCode) {
      return {
        valid: false,
        error: '优惠码不能为空',
        errorCode: 'PROMO_NOT_FOUND'
      }
    }

    const packageId = Number(body.packageId)
    const packagePlanId = Number(body.packagePlanId)
    if (!packageId || !Number.isSafeInteger(packageId) || !packagePlanId || !Number.isSafeInteger(packagePlanId)) {
      return {
        valid: false,
        error: '套餐或方案参数无效',
        errorCode: 'INVALID_PARAMS'
      }
    }

    const plan = await prisma.packagePlan.findUnique({
      where: { id: packagePlanId }
    })

    if (!plan || plan.packageId !== packageId) {
      return {
        valid: false,
        error: '套餐方案不存在',
        errorCode: 'PLAN_NOT_FOUND'
      }
    }

    const user = (request as any).user
    const validation = await PromoCodeEngine.validate({
      code: trimmedCode,
      packageId,
      packagePlanId,
      userId: user.id
    })

    if (!validation.valid || !validation.promoCode) {
      return {
        valid: false,
        error: validation.error || '优惠码无效',
        errorCode: validation.errorCode || 'PROMO_INVALID'
      }
    }

    const billing = calculateCreateBilling(plan)
    const quote = PromoCodeEngine.calculateCreationQuote(billing.totalPrice, validation.promoCode)

    return {
      valid: true,
      code: validation.promoCode.code,
      discountType: validation.discountType,
      discountValue: Number(validation.discountValue),
      durationType: validation.durationType,
      durationCycles: validation.durationCycles,
      estimatedDiscount: quote.discountAmount,
      finalPrice: quote.finalPrice
    }
  })

  // ==================== 2. GET /renew-preview/:instanceId ====================
  fastify.get('/renew-preview/:instanceId', {
    onRequest: [fastify.authenticate]
  }, async (request: FastifyRequest, reply: FastifyReply) => {
    const params = request.params as { instanceId: string }
    const instanceId = parsePositiveRouteId(params.instanceId)
    if (!instanceId) {
      return reply.code(400).send(apiError(ErrorCode.INVALID_ID, '实例 ID 无效'))
    }

    const query = (request.query || {}) as { months?: string }
    let parsedMonths: number | null = null
    if (query.months !== undefined) {
      parsedMonths = parsePositiveRouteId(String(query.months))
      if (!parsedMonths || parsedMonths <= 0) {
        return reply.code(400).send(apiError(ErrorCode.INVALID_PARAMS, '续费月数无效'))
      }
    }

    const instance = await prisma.instance.findUnique({
      where: { id: instanceId },
      include: { packagePlan: true }
    })

    if (!instance) {
      return reply.code(404).send(apiError(ErrorCode.INSTANCE_NOT_FOUND, '实例不存在'))
    }

    const user = (request as any).user
    if (user.role !== 'admin' && instance.userId !== user.id) {
      return reply.code(403).send(apiError(ErrorCode.FORBIDDEN, '无权访问该实例'))
    }

    let monthlyPrice = calculateMonthlyPrice(instance)
    if (monthlyPrice <= 0 && instance.packagePlan) {
      monthlyPrice = (Number(instance.packagePlan.price) / 100) / Math.max(instance.packagePlan.billingCycle || 1, 1)
    }

    const binding = await getInstancePromoBinding(instanceId)
    const now = new Date()
    const isPromoActive = Boolean(
      binding &&
      binding.promoCode.enabled &&
      (!binding.promoCode.expiresAt || binding.promoCode.expiresAt > now)
    )

    let bindingStatus: 'ACTIVE' | 'DISABLED' | 'EXPIRED' | null = null
    if (binding) {
      if (!binding.promoCode.enabled) {
        bindingStatus = 'DISABLED'
      } else if (binding.promoCode.expiresAt && binding.promoCode.expiresAt <= now) {
        bindingStatus = 'EXPIRED'
      } else {
        bindingStatus = 'ACTIVE'
      }
    }

    const monthList = parsedMonths ? [parsedMonths] : [1, 3, 6, 12]

    const options = monthList.map(months => {
      if (isPromoActive && binding) {
        const quote = PromoCodeEngine.calculateRenewalQuote(monthlyPrice, months, binding)
        return {
          months,
          originalPrice: Number((monthlyPrice * months).toFixed(2)),
          discountAmount: quote.discountAmount,
          finalPrice: quote.finalAmount,
          discountedMonths: quote.discountedMonths,
          regularMonths: quote.regularMonths,
          willUnbind: quote.willUnbind
        }
      } else {
        const originalPrice = Number((monthlyPrice * months).toFixed(2))
        return {
          months,
          originalPrice,
          discountAmount: 0,
          finalPrice: originalPrice,
          discountedMonths: 0,
          regularMonths: months,
          willUnbind: false
        }
      }
    })

    return {
      instanceId,
      hasBinding: !!binding,
      isPromoActive,
      bindingStatus,
      binding: binding ? {
        code: binding.promoCode.code,
        discountValue: Number(binding.promoCode.discountValue),
        durationType: binding.durationType,
        remainingCycles: binding.remainingCycles,
        enabled: binding.promoCode.enabled,
        expiresAt: binding.promoCode.expiresAt
      } : null,
      options
    }
  })

  // ==================== 3. POST /apply/:instanceId ====================
  fastify.post('/apply/:instanceId', {
    onRequest: [fastify.authenticate]
  }, async (request: FastifyRequest, reply: FastifyReply) => {
    const params = request.params as { instanceId: string }
    const instanceId = parsePositiveRouteId(params.instanceId)
    if (!instanceId) {
      return reply.code(400).send(apiError(ErrorCode.INVALID_ID, '实例 ID 无效'))
    }

    const body = (request.body || {}) as { code?: string }
    const trimmedCode = (body.code || '').trim()
    if (!trimmedCode) {
      return reply.code(400).send({
        success: false,
        error: '优惠码不能为空',
        errorCode: 'PROMO_NOT_FOUND'
      })
    }

    const instance = await prisma.instance.findUnique({
      where: { id: instanceId },
      include: { host: true }
    })

    if (!instance || instance.status === 'deleted') {
      return reply.code(404).send(apiError(ErrorCode.INSTANCE_NOT_FOUND, '实例不存在'))
    }

    const user = (request as any).user
    if (user.role !== 'admin' && instance.userId !== user.id) {
      return reply.code(403).send(apiError(ErrorCode.FORBIDDEN, '无权操作该实例'))
    }

    // 用户托管节点不允许使用优惠码（托管节点命名前四位固定为 peer）
    if (instance.host && instance.host.name.toLowerCase().startsWith('peer')) {
      return reply.code(400).send({
        success: false,
        error: '用户托管节点不支持使用优惠码',
        errorCode: 'NOT_ELIGIBLE'
      })
    }

    if (!instance.packageId || !instance.packagePlanId) {
      return reply.code(400).send({
        success: false,
        error: '当前实例不支持绑定优惠码',
        errorCode: 'NOT_ELIGIBLE'
      })
    }

    const existingBinding = await getInstancePromoBinding(instanceId)
    if (existingBinding) {
      return reply.code(400).send({
        success: false,
        error: '该实例已绑定其他优惠码',
        errorCode: 'PROMO_BINDING_ACTIVE'
      })
    }

    const validation = await PromoCodeEngine.validate({
      code: trimmedCode,
      packageId: instance.packageId,
      packagePlanId: instance.packagePlanId,
      userId: user.id,
      targetInstanceId: instanceId
    })

    if (!validation.valid || !validation.promoCode) {
      return reply.code(400).send({
        success: false,
        error: validation.error || '优惠码无效',
        errorCode: validation.errorCode || 'PROMO_INVALID'
      })
    }

    if (validation.discountType !== 'PERCENTAGE') {
      return reply.code(400).send({
        success: false,
        error: '仅支持百分比折扣优惠码绑定续费',
        errorCode: 'INVALID_DISCOUNT_TYPE'
      })
    }

    const durationType = validation.durationType!
    const totalCycles = durationType === 'REPEATING' ? (validation.durationCycles || 1) : (durationType === 'ONCE' ? 1 : null)
    const remainingCycles = totalCycles

    const binding = await bindPromoToInstance(prisma, {
      instanceId,
      promoCodeId: validation.promoCode.id,
      durationType,
      totalCycles,
      usedCycles: 0,
      remainingCycles
    })

    return {
      success: true,
      binding: {
        id: binding.id,
        instanceId: binding.instanceId,
        promoCodeId: binding.promoCodeId,
        durationType: binding.durationType,
        totalCycles: binding.totalCycles,
        usedCycles: binding.usedCycles,
        remainingCycles: binding.remainingCycles
      }
    }
  })
}
