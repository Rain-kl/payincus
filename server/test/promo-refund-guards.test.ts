import assert from 'node:assert/strict'
import {
  calculateInstanceRemainingRefundQuote,
  calculateInstanceRefund,
  calculateInstancePriceAdjustmentQuote,
  calculatePlanChange
} from '../src/db/billing-operations.js'
import type { Instance, PackagePlan } from '@prisma/client'

console.log('Testing Promo Refund & Downgrade Guards (TDD)...')

const futureDate = new Date(Date.now() + 20 * 24 * 60 * 60 * 1000)
const pastDate = new Date(Date.now() - 10 * 24 * 60 * 60 * 1000)

function createMockTx(options: {
  hasBinding?: boolean
  redemptionLogs?: any[]
  renewCount?: number
  hasPositiveRecords?: boolean
  oldPlanPrice?: number
  newPlanPrice?: number
}) {
  const {
    hasBinding = false,
    redemptionLogs = [],
    renewCount = 0,
    hasPositiveRecords = true
  } = options

  return {
    instancePromoBinding: {
      findUnique: async () => (hasBinding ? { id: 1, instanceId: 101, promoCodeId: 10 } : null)
    },
    promoRedemptionLog: {
      findMany: async () => redemptionLogs
    },
    instanceBillingRecord: {
      count: async () => renewCount,
      findMany: async (args: any) => {
        if (args?.where?.type === 'refund') {
          return []
        }
        if (!hasPositiveRecords) return []
        return [
          {
            amount: 100,
            periodStart: pastDate,
            periodEnd: futureDate
          }
        ]
      },
      aggregate: async () => ({
        _sum: { amount: hasPositiveRecords ? 100 : 0 }
      }),
      findFirst: async () => (hasPositiveRecords ? { amount: 100 } : null)
    },
    instance: {
      findUnique: async () => ({
        id: 101,
        userId: 1,
        hostId: 1,
        name: 'test-inst',
        expiresAt: futureDate,
        createdAt: pastDate,
        updatedAt: pastDate
      }),
      findMany: async () => []
    },
    inboxMessage: {
      findMany: async () => []
    },
    instanceAffBinding: {
      findUnique: async () => null
    },
    affBinding: {
      findUnique: async () => null
    },
    packagePlan: {
      findUnique: async (args: any) => {
        const id = args?.where?.id
        return {
          id,
          packageId: 1,
          name: `Plan ${id}`,
          price: id === 1 ? 10000 : 2000, // 分: 100 元 vs 20 元
          billingCycle: 1,
          cpu: 1,
          memory: 1024,
          disk: 20,
          trafficLimitSpeed: null,
          isActive: true,
          isSoldOut: false
        }
      }
    }
  } as any
}

const baseInstance = {
  id: 101,
  userId: 1,
  packageId: 1,
  packagePlanId: 1,
  billingPrice: 100,
  billingCycle: 1,
  expiresAt: futureDate,
  status: 'running' as const,
  name: 'test-inst',
  hostId: 1
} as unknown as Instance

// ==================== 1. Active Promo Binding Tests ====================
async function testActivePromoBindingRefund() {
  console.log('  1. Testing active promo binding zero-refund guard...')
  const mockTx = createMockTx({ hasBinding: true })

  const quote = await calculateInstanceRemainingRefundQuote(baseInstance, mockTx)
  assert.equal(quote.refundableValue, 0, 'Active promo binding instance must have refundableValue === 0')
  assert.equal(quote.maxRefundable, 0, 'Active promo binding instance must have maxRefundable === 0')
  assert.equal(quote.remainingValue, 0, 'Active promo binding instance must have remainingValue === 0')
}

// ==================== 2. First-Month Once Promo Without Renewal Tests ====================
async function testFirstMonthOncePromoRefund() {
  console.log('  2. Testing first-month once discount without renew zero-refund guard...')
  const mockTx = createMockTx({
    hasBinding: false,
    redemptionLogs: [{ id: 1, instanceId: 101, actionType: 'create' }],
    renewCount: 0
  })

  const quote = await calculateInstanceRemainingRefundQuote(baseInstance, mockTx)
  assert.equal(quote.refundableValue, 0, 'First-month once promo instance without renewal must have refundableValue === 0')
  assert.equal(quote.maxRefundable, 0, 'First-month once promo instance without renewal must have maxRefundable === 0')
  assert.equal(quote.remainingValue, 0, 'First-month once promo instance without renewal must have remainingValue === 0')
}

// ==================== 3. First-Month Once Promo With Renewal Tests ====================
async function testRenewedOncePromoRefund() {
  console.log('  3. Testing first-month once discount with renew allows normal refund...')
  const mockTx = createMockTx({
    hasBinding: false,
    redemptionLogs: [{ id: 1, instanceId: 101, actionType: 'create' }],
    renewCount: 1
  })

  const quote = await calculateInstanceRemainingRefundQuote(baseInstance, mockTx)
  assert.ok(quote.refundableValue > 0, `Renewed once-promo instance should allow refund, got ${quote.refundableValue}`)
  assert.ok(quote.maxRefundable > 0, `Renewed once-promo instance should have maxRefundable > 0, got ${quote.maxRefundable}`)
}

// ==================== 4. Normal Instance Without Promo Tests ====================
async function testNormalInstanceRefund() {
  console.log('  4. Testing regular instance without promo calculates normal refund...')
  const mockTx = createMockTx({
    hasBinding: false,
    redemptionLogs: [],
    renewCount: 0
  })

  const quote = await calculateInstanceRemainingRefundQuote(baseInstance, mockTx)
  assert.ok(quote.refundableValue > 0, `Normal instance should have refundableValue > 0, got ${quote.refundableValue}`)
  assert.ok(quote.maxRefundable > 0, `Normal instance should have maxRefundable > 0, got ${quote.maxRefundable}`)
}

// ==================== 5. Downgrade PriceDiff Clamping Guard Tests ====================
async function testDowngradePriceDiffClamping() {
  console.log('  5. Testing plan downgrade / price adjustment priceDiff clamping...')

  // 5.1 Price adjustment on instance under promo protection: price reduction must clamp priceDiff to 0
  {
    const mockTx = createMockTx({ hasBinding: true })
    const adjQuote = await calculateInstancePriceAdjustmentQuote(baseInstance, 20, true, mockTx)
    assert.equal(adjQuote.priceDiff, 0, `Price adjustment downgrade on promo instance must clamp priceDiff to 0, got ${adjQuote.priceDiff}`)
  }

  // 5.2 Price adjustment on once-discount without renew: must clamp priceDiff to 0
  {
    const mockTx = createMockTx({
      hasBinding: false,
      redemptionLogs: [{ id: 1, instanceId: 101, actionType: 'create' }],
      renewCount: 0
    })
    const adjQuote = await calculateInstancePriceAdjustmentQuote(baseInstance, 20, true, mockTx)
    assert.equal(adjQuote.priceDiff, 0, `Price adjustment downgrade on first-cycle promo instance must clamp priceDiff to 0, got ${adjQuote.priceDiff}`)
  }

  // 5.3 Price adjustment on normal instance: price reduction allows negative priceDiff (refund to balance)
  {
    const mockTx = createMockTx({
      hasBinding: false,
      redemptionLogs: [],
      renewCount: 0
    })
    const adjQuote = await calculateInstancePriceAdjustmentQuote(baseInstance, 20, true, mockTx)
    assert.ok(adjQuote.priceDiff < 0, `Normal instance price reduction should yield negative priceDiff, got ${adjQuote.priceDiff}`)
  }

  // 5.4 Plan change downgrade on instance under promo protection: priceDiff must clamp to 0
  {
    const mockTx = createMockTx({ hasBinding: true })
    const cheapPlan = {
      id: 2,
      packageId: 1,
      name: 'Plan 2',
      price: 2000,
      billingCycle: 1,
      cpu: 1,
      memory: 1024,
      disk: 20,
      trafficLimitSpeed: null,
      isActive: true,
      isSoldOut: false
    } as PackagePlan
    const planChangeRes = await calculatePlanChange(baseInstance, cheapPlan, { tx: mockTx })
    assert.equal(planChangeRes.priceDiff, 0, `Plan change downgrade on promo instance must clamp priceDiff to 0, got ${planChangeRes.priceDiff}`)
  }

  // 5.5 Plan change downgrade on normal instance without promo: allows negative priceDiff (refund to balance)
  {
    const mockTx = createMockTx({
      hasBinding: false,
      redemptionLogs: [],
      renewCount: 0
    })
    const cheapPlan = {
      id: 2,
      packageId: 1,
      name: 'Plan 2',
      price: 2000,
      billingCycle: 1,
      cpu: 1,
      memory: 1024,
      disk: 20,
      trafficLimitSpeed: null,
      isActive: true,
      isSoldOut: false
    } as PackagePlan
    const planChangeRes = await calculatePlanChange(baseInstance, cheapPlan, { tx: mockTx })
    assert.ok(planChangeRes.priceDiff < 0, `Normal instance plan downgrade should yield negative priceDiff, got ${planChangeRes.priceDiff}`)
  }
}

async function runAllTests() {
  await testActivePromoBindingRefund()
  await testFirstMonthOncePromoRefund()
  await testRenewedOncePromoRefund()
  await testNormalInstanceRefund()
  await testDowngradePriceDiffClamping()
  console.log('✅ All Promo Refund & Downgrade Guard tests passed!')
}

runAllTests().catch(err => {
  console.error('❌ Test failed:', err)
  process.exit(1)
})
