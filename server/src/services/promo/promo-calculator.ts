import type {
  CreationQuoteParams,
  CreationQuoteResult,
  RenewalSplitParams,
  RenewalSplitResult
} from './types.js'

/**
 * 计算扣减优惠后的最终金额，强制防负数保底 (>= 0) 并保留两位小数
 */
export function calculateFinalPrice(originalPrice: number, discountAmount: number): number {
  return Math.max(0, Number((originalPrice - discountAmount).toFixed(2)))
}

/**
 * 计算新购（创建订单）时的优惠金额与最终金额
 */
export function calculateCreationQuote(params: CreationQuoteParams): CreationQuoteResult {
  let discountAmount = 0

  if (params.discountType === 'PERCENTAGE') {
    discountAmount = Number((params.originalPrice * params.discountValue).toFixed(2))
  } else if (params.discountType === 'FIXED_AMOUNT') {
    discountAmount = Number(Math.min(params.originalPrice, params.discountValue).toFixed(2))
  }

  const finalPrice = calculateFinalPrice(params.originalPrice, discountAmount)

  return {
    originalPrice: params.originalPrice,
    discountAmount,
    finalPrice
  }
}

/**
 * 续费切片计算引擎
 * 处理 FOREVER 与 REPEATING（跨周期折算）的优惠扣减与剩余周期轮换
 */
export function calculatePromoRenewalSplit(params: RenewalSplitParams): RenewalSplitResult {
  const { monthlyPrice, months, discountRate, remainingCycles } = params

  if (remainingCycles === null) {
    // 永久优惠（FOREVER）
    const discountedMonths = months
    const regularMonths = 0
    const discountAmount = Number(((monthlyPrice * months) * discountRate).toFixed(2))
    const finalAmount = calculateFinalPrice(monthlyPrice * months, discountAmount)
    const consumedCycles = months
    const newRemainingCycles = null
    const willUnbind = false

    return {
      discountedMonths,
      regularMonths,
      discountAmount,
      finalAmount,
      consumedCycles,
      newRemainingCycles,
      willUnbind
    }
  }

  // 有限周期优惠（REPEATING）
  const discountedMonths = Math.min(months, Math.max(0, remainingCycles))
  const regularMonths = months - discountedMonths
  const discountAmount = Number(((monthlyPrice * discountedMonths) * discountRate).toFixed(2))
  const finalAmount = calculateFinalPrice(monthlyPrice * months, discountAmount)
  const consumedCycles = discountedMonths
  const newRemainingCycles = Math.max(0, remainingCycles - discountedMonths)
  const willUnbind = newRemainingCycles <= 0

  return {
    discountedMonths,
    regularMonths,
    discountAmount,
    finalAmount,
    consumedCycles,
    newRemainingCycles,
    willUnbind
  }
}
