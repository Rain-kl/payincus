import assert from 'node:assert/strict'
import {
  calculateFinalPrice,
  calculateCreationQuote,
  calculatePromoRenewalSplit
} from '../src/services/promo/promo-calculator.js'
import type { CreationQuoteParams, RenewalSplitParams } from '../src/services/promo/types.js'

console.log('Testing Promo Calculator & Slicing Engine...')

// 1. calculateFinalPrice
{
  // Standard subtraction
  assert.equal(calculateFinalPrice(100, 20), 80, '100 - 20 should be 80')

  // Float precision handling
  assert.equal(calculateFinalPrice(10.99, 3.33), 7.66, '10.99 - 3.33 should be 7.66')

  // Zero final price when discount equals price
  assert.equal(calculateFinalPrice(50, 50), 0, '50 - 50 should be 0')

  // Zero clamp: discount exceeds price
  assert.equal(calculateFinalPrice(50, 80), 0, '50 - 80 should clamp to 0, not negative')

  // Zero original price
  assert.equal(calculateFinalPrice(0, 10), 0, '0 - 10 should clamp to 0')
}

// 2. calculateCreationQuote
{
  // PERCENTAGE discount
  const percentParams: CreationQuoteParams = {
    originalPrice: 100,
    discountType: 'PERCENTAGE',
    discountValue: 0.2
  }
  const percentResult = calculateCreationQuote(percentParams)
  assert.deepEqual(percentResult, {
    originalPrice: 100,
    discountAmount: 20,
    finalPrice: 80
  }, '20% off 100 should give 20 discount and 80 final price')

  // PERCENTAGE with rounding
  const roundParams: CreationQuoteParams = {
    originalPrice: 19.99,
    discountType: 'PERCENTAGE',
    discountValue: 0.15
  }
  const roundResult = calculateCreationQuote(roundParams)
  // 19.99 * 0.15 = 2.9985 -> 3.00, finalPrice = 19.99 - 3.00 = 16.99
  assert.deepEqual(roundResult, {
    originalPrice: 19.99,
    discountAmount: 3,
    finalPrice: 16.99
  }, '15% off 19.99 should round properly to 3.00 discount and 16.99 final price')

  // PERCENTAGE 100% off
  const freeParams: CreationQuoteParams = {
    originalPrice: 100,
    discountType: 'PERCENTAGE',
    discountValue: 1.0
  }
  assert.deepEqual(calculateCreationQuote(freeParams), {
    originalPrice: 100,
    discountAmount: 100,
    finalPrice: 0
  }, '100% off should give 0 final price')

  // FIXED_AMOUNT discount
  const fixedParams: CreationQuoteParams = {
    originalPrice: 100,
    discountType: 'FIXED_AMOUNT',
    discountValue: 30
  }
  assert.deepEqual(calculateCreationQuote(fixedParams), {
    originalPrice: 100,
    discountAmount: 30,
    finalPrice: 70
  }, 'Fixed 30 off 100 should give 30 discount and 70 final price')

  // FIXED_AMOUNT exceeds original price (zero clamp)
  const fixedExceedParams: CreationQuoteParams = {
    originalPrice: 50,
    discountType: 'FIXED_AMOUNT',
    discountValue: 80
  }
  assert.deepEqual(calculateCreationQuote(fixedExceedParams), {
    originalPrice: 50,
    discountAmount: 50,
    finalPrice: 0
  }, 'Fixed 80 off 50 should clamp discount to 50 and final price to 0')
}

// 3. calculatePromoRenewalSplit
{
  // FOREVER discount (remainingCycles: null)
  const foreverParams: RenewalSplitParams = {
    monthlyPrice: 100,
    months: 3,
    discountRate: 0.2,
    remainingCycles: null
  }
  const foreverResult = calculatePromoRenewalSplit(foreverParams)
  assert.deepEqual(foreverResult, {
    discountedMonths: 3,
    regularMonths: 0,
    discountAmount: 60,
    finalAmount: 240,
    consumedCycles: 3,
    newRemainingCycles: null,
    willUnbind: false
  }, 'FOREVER promo should apply discount to all months and retain null cycles')

  // REPEATING discount with ample remaining cycles
  const ampleParams: RenewalSplitParams = {
    monthlyPrice: 100,
    months: 2,
    discountRate: 0.2,
    remainingCycles: 5
  }
  const ampleResult = calculatePromoRenewalSplit(ampleParams)
  assert.deepEqual(ampleResult, {
    discountedMonths: 2,
    regularMonths: 0,
    discountAmount: 40,
    finalAmount: 160,
    consumedCycles: 2,
    newRemainingCycles: 3,
    willUnbind: false
  }, 'REPEATING promo with ample cycles should discount all months and decrement cycles')

  // REPEATING discount expiring exactly
  const exactParams: RenewalSplitParams = {
    monthlyPrice: 100,
    months: 3,
    discountRate: 0.1,
    remainingCycles: 3
  }
  const exactResult = calculatePromoRenewalSplit(exactParams)
  assert.deepEqual(exactResult, {
    discountedMonths: 3,
    regularMonths: 0,
    discountAmount: 30,
    finalAmount: 270,
    consumedCycles: 3,
    newRemainingCycles: 0,
    willUnbind: true
  }, 'REPEATING promo expiring exactly should set newRemainingCycles to 0 and willUnbind to true')

  // REPEATING discount slicing across remaining cycles (fewer cycles than months)
  const slicedParams: RenewalSplitParams = {
    monthlyPrice: 100,
    months: 6,
    discountRate: 0.5,
    remainingCycles: 2
  }
  const slicedResult = calculatePromoRenewalSplit(slicedParams)
  assert.deepEqual(slicedResult, {
    discountedMonths: 2,
    regularMonths: 4,
    discountAmount: 100,
    finalAmount: 500,
    consumedCycles: 2,
    newRemainingCycles: 0,
    willUnbind: true
  }, 'REPEATING promo slicing should discount 2 months and charge full for remaining 4 months')

  // REPEATING discount already exhausted (remainingCycles: 0)
  const exhaustedParams: RenewalSplitParams = {
    monthlyPrice: 100,
    months: 3,
    discountRate: 0.2,
    remainingCycles: 0
  }
  const exhaustedResult = calculatePromoRenewalSplit(exhaustedParams)
  assert.deepEqual(exhaustedResult, {
    discountedMonths: 0,
    regularMonths: 3,
    discountAmount: 0,
    finalAmount: 300,
    consumedCycles: 0,
    newRemainingCycles: 0,
    willUnbind: true
  }, 'Exhausted promo should provide 0 discount and unbind')

  // Decimal / float precision calculation in renewal split
  const floatParams: RenewalSplitParams = {
    monthlyPrice: 33.33,
    months: 3,
    discountRate: 0.15,
    remainingCycles: 2
  }
  const floatResult = calculatePromoRenewalSplit(floatParams)
  // discountedMonths: 2, regularMonths: 1
  // discountAmount = Number(((33.33 * 2) * 0.15).toFixed(2)) = Number((66.66 * 0.15).toFixed(2)) = 10.00
  // finalAmount = Number(((33.33 * 3) - 10.00).toFixed(2)) = Number((99.99 - 10.00).toFixed(2)) = 89.99
  assert.deepEqual(floatResult, {
    discountedMonths: 2,
    regularMonths: 1,
    discountAmount: 10,
    finalAmount: 89.99,
    consumedCycles: 2,
    newRemainingCycles: 0,
    willUnbind: true
  }, 'Float precision calculation should round discountAmount and finalAmount properly')
}

console.log('✅ All promo calculator & slicing engine tests passed!')
