import assert from 'node:assert/strict'
import { Prisma } from '@prisma/client'
import { PromoCodeEngine } from '../src/services/promo-engine.js'
import * as db from '../src/db/index.js'

console.log('Testing PromoCodeEngine Facade Service & Orchestration...')

// ==================== 1. DB Re-export Checks ====================
{
  assert.equal(typeof (db as any).getPromoCodeByCode, 'function', 'getPromoCodeByCode must be re-exported in db/index')
  assert.equal(typeof (db as any).getInstancePromoBinding, 'function', 'getInstancePromoBinding must be re-exported in db/index')
  assert.equal(typeof (db as any).countUserPromoRedemptions, 'function', 'countUserPromoRedemptions must be re-exported in db/index')
  assert.equal(typeof (db as any).createPromoRedemptionLog, 'function', 'createPromoRedemptionLog must be re-exported in db/index')
}

// ==================== 2. PromoCodeEngine.validate Tests ====================
async function testValidate() {
  console.log('  Testing PromoCodeEngine.validate...')

  const basePromo = {
    id: 100,
    code: 'SAVE20',
    name: '20% OFF',
    type: 'ADMIN_PROMO' as const,
    userId: null,
    adminId: 1,
    isGlobal: true,
    discountType: 'PERCENTAGE' as const,
    discountValue: new Prisma.Decimal(0.2),
    commissionRate: new Prisma.Decimal(0),
    durationType: 'FOREVER' as const,
    durationCycles: null,
    maxTotalUses: 100,
    usedTotalCount: 5,
    maxUsesPerUser: 1,
    startsAt: null,
    expiresAt: null,
    enabled: true,
    totalDiscountAmount: new Prisma.Decimal(10),
    totalEarnings: new Prisma.Decimal(0),
    createdAt: new Date(),
    updatedAt: new Date(),
    scopes: [],
    _count: { bindings: 0, redemptionLogs: 0 }
  }

  const createMockTx = (promoOverride: any = {}, redemptionsCount = 0, existingBinding: any = null) => {
    return {
      promoCode: {
        findUnique: async () => {
          if (promoOverride === null) return null
          return { ...basePromo, ...promoOverride }
        }
      },
      instancePromoBinding: {
        findUnique: async () => existingBinding
      },
      promoRedemptionLog: {
        count: async () => redemptionsCount
      }
    } as any
  }

  // 2.1 Promo not found
  {
    const tx = createMockTx(null)
    const res = await PromoCodeEngine.validate({
      code: 'NON_EXISTENT',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_NOT_FOUND')
  }

  // 2.2 Promo disabled
  {
    const tx = createMockTx({ enabled: false })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_DISABLED')
  }

  // 2.3 Promo not started
  {
    const future = new Date(Date.now() + 86400000)
    const tx = createMockTx({ startsAt: future })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_NOT_STARTED')
  }

  // 2.4 Promo expired
  {
    const past = new Date(Date.now() - 86400000)
    const tx = createMockTx({ expiresAt: past })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_EXPIRED')
  }

  // 2.5 Total quota exceeded
  {
    const tx = createMockTx({ maxTotalUses: 10, usedTotalCount: 10 })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_QUOTA_EXCEEDED')
  }

  // 2.6 Per-user quota exceeded
  {
    const tx = createMockTx({ maxUsesPerUser: 1 }, 1)
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_USER_LIMIT_EXCEEDED')
  }

  // 2.7 Self-referral check: AFF_USER cannot use own code
  {
    const tx = createMockTx({
      type: 'AFF_USER',
      userId: 42
    })
    const res = await PromoCodeEngine.validate({
      code: 'MY_AFF_CODE',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'CANNOT_USE_OWN_AFF_CODE')
  }

  // 2.8 Target instance already has active binding
  {
    const tx = createMockTx({}, 0, { id: 1, instanceId: 99, promoCodeId: 100 })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 1,
      userId: 42,
      targetInstanceId: 99,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_BINDING_ACTIVE')
  }

  // 2.9 Scope mismatch: plan not matching scopes
  {
    const tx = createMockTx({
      isGlobal: false,
      scopes: [{ packageId: 2, packagePlanId: 20 }]
    })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 10,
      userId: 42,
      tx
    })
    assert.equal(res.valid, false)
    assert.equal(res.errorCode, 'PROMO_SCOPE_MISMATCH')
  }

  // 2.10 Valid promo code
  {
    const tx = createMockTx({
      isGlobal: false,
      scopes: [{ packageId: 1, packagePlanId: 10 }]
    })
    const res = await PromoCodeEngine.validate({
      code: 'SAVE20',
      packageId: 1,
      packagePlanId: 10,
      userId: 42,
      tx
    })
    assert.equal(res.valid, true)
    assert.equal(res.discountType, 'PERCENTAGE')
    assert.equal(res.discountValue, 0.2)
    assert.equal(res.durationType, 'FOREVER')
    assert.equal(res.description, '20% OFF')
    assert.ok(res.promoCode)
  }
}

// ==================== 3. Quote Calculation Tests ====================
function testQuotes() {
  console.log('  Testing PromoCodeEngine quotes...')

  // 3.1 calculateCreationQuote
  const quotePercent = PromoCodeEngine.calculateCreationQuote(100, {
    discountType: 'PERCENTAGE',
    discountValue: new Prisma.Decimal(0.15)
  } as any)
  assert.equal(quotePercent.originalPrice, 100)
  assert.equal(quotePercent.discountAmount, 15)
  assert.equal(quotePercent.finalPrice, 85)

  const quoteFixed = PromoCodeEngine.calculateCreationQuote(20, {
    discountType: 'FIXED_AMOUNT',
    discountValue: 30
  } as any)
  assert.equal(quoteFixed.discountAmount, 20)
  assert.equal(quoteFixed.finalPrice, 0)

  // 3.2 calculateRenewalQuote
  const renewSplit = PromoCodeEngine.calculateRenewalQuote(100, 12, {
    promoCode: {
      discountType: 'PERCENTAGE',
      discountValue: new Prisma.Decimal(0.10)
    },
    remainingCycles: 3
  } as any)
  assert.equal(renewSplit.discountedMonths, 3)
  assert.equal(renewSplit.regularMonths, 9)
  assert.equal(renewSplit.discountAmount, 30)
  assert.equal(renewSplit.finalAmount, 1170)
  assert.equal(renewSplit.willUnbind, true)
}

// ==================== 4. Settle On Create Tests ====================
async function testSettleOnCreate() {
  console.log('  Testing PromoCodeEngine.settleOnCreate...')

  let lockedPromoId: number | null = null
  let createdLog: any = null
  let boundParams: any = null

  const mockTx: any = {
    $queryRaw: async () => [{ id: 10 }],
    promoCode: {
      findUnique: async () => ({
        id: 10,
        enabled: true,
        startsAt: null,
        expiresAt: null,
        maxTotalUses: 100,
        usedTotalCount: 0,
        totalDiscountAmount: new Prisma.Decimal(0)
      }),
      update: async ({ where, data }: any) => {
        lockedPromoId = where.id
        return { id: where.id, ...data }
      }
    },
    promoRedemptionLog: {
      create: async ({ data }: any) => {
        createdLog = data
        return { id: 1, ...data }
      }
    },
    instancePromoBinding: {
      upsert: async ({ create }: any) => {
        boundParams = create
        return { id: 1, ...create }
      }
    }
  }

  // 4.1 ONCE promo: records log, does NOT bind
  {
    boundParams = null
    const res = await PromoCodeEngine.settleOnCreate(mockTx, {
      promoCodeId: 10,
      instanceId: 101,
      userId: 42,
      originalPrice: 100,
      discountAmount: 20,
      finalPrice: 80,
      durationType: 'ONCE'
    })
    assert.equal(lockedPromoId, 10)
    assert.equal(createdLog.actionType, 'create')
    assert.equal(createdLog.cycleIndex, 1)
    assert.equal(res.binding, null)
    assert.equal(boundParams, null, 'ONCE promo must not create instance binding')
  }

  // 4.2 REPEATING promo with 3 cycles -> remaining 2 cycles, binds
  {
    boundParams = null
    const res = await PromoCodeEngine.settleOnCreate(mockTx, {
      promoCodeId: 10,
      instanceId: 102,
      userId: 42,
      originalPrice: 100,
      discountAmount: 20,
      finalPrice: 80,
      durationType: 'REPEATING',
      durationCycles: 3
    })
    assert.ok(res.binding)
    assert.equal(boundParams.totalCycles, 3)
    assert.equal(boundParams.usedCycles, 1)
    assert.equal(boundParams.remainingCycles, 2)
  }

  // 4.3 REPEATING promo with 1 cycle -> remaining 0 cycles, does NOT bind
  {
    boundParams = null
    const res = await PromoCodeEngine.settleOnCreate(mockTx, {
      promoCodeId: 10,
      instanceId: 103,
      userId: 42,
      originalPrice: 100,
      discountAmount: 20,
      finalPrice: 80,
      durationType: 'REPEATING',
      durationCycles: 1
    })
    assert.equal(res.binding, null)
    assert.equal(boundParams, null, 'REPEATING promo with 1 cycle used at create must not bind')
  }

  // 4.4 FOREVER promo -> binds with totalCycles: null, remainingCycles: null
  {
    boundParams = null
    const res = await PromoCodeEngine.settleOnCreate(mockTx, {
      promoCodeId: 10,
      instanceId: 104,
      userId: 42,
      originalPrice: 100,
      discountAmount: 20,
      finalPrice: 80,
      durationType: 'FOREVER'
    })
    assert.ok(res.binding)
    assert.equal(boundParams.totalCycles, null)
    assert.equal(boundParams.usedCycles, 1)
    assert.equal(boundParams.remainingCycles, null)
  }
}

// ==================== 5. Settle On Renew Tests ====================
async function testSettleOnRenew() {
  console.log('  Testing PromoCodeEngine.settleOnRenew...')

  let updatedPromoDiscount: any = null
  let createdLog: any = null
  let updatedBindingCycles: any = null
  let unboundInstanceId: number | null = null

  const mockTx: any = {
    promoCode: {
      update: async ({ data }: any) => {
        updatedPromoDiscount = data.totalDiscountAmount
        return {}
      }
    },
    promoRedemptionLog: {
      create: async ({ data }: any) => {
        createdLog = data
        return { id: 2, ...data }
      }
    },
    instancePromoBinding: {
      update: async ({ where, data }: any) => {
        updatedBindingCycles = data
        return { instanceId: where.instanceId, ...data }
      },
      deleteMany: async ({ where }: any) => {
        unboundInstanceId = where.instanceId
        return { count: 1 }
      }
    }
  }

  // 5.1 Renew with REPEATING promo that has remaining cycles (not yet unbinding)
  {
    updatedBindingCycles = null
    unboundInstanceId = null
    const binding: any = {
      instanceId: 201,
      promoCodeId: 5,
      usedCycles: 1,
      remainingCycles: 3,
      promoCode: {
        discountType: 'PERCENTAGE',
        discountValue: new Prisma.Decimal(0.1)
      }
    }

    const res = await PromoCodeEngine.settleOnRenew(mockTx, {
      instanceId: 201,
      userId: 42,
      monthlyPrice: 50,
      months: 2,
      binding
    })

    assert.equal(res.unbound, false)
    assert.equal(res.quote.discountedMonths, 2)
    assert.equal(res.quote.discountAmount, 10)
    assert.equal(res.quote.newRemainingCycles, 1)
    assert.equal(createdLog.actionType, 'renew')
    assert.equal(createdLog.cycleIndex, 2)
    assert.equal(updatedBindingCycles.remainingCycles, 1)
    assert.equal(unboundInstanceId, null)
  }

  // 5.2 Renew with REPEATING promo that exhausts cycles -> unbinds
  {
    updatedBindingCycles = null
    unboundInstanceId = null
    const binding: any = {
      instanceId: 202,
      promoCodeId: 5,
      usedCycles: 3,
      remainingCycles: 1,
      promoCode: {
        discountType: 'PERCENTAGE',
        discountValue: new Prisma.Decimal(0.1)
      }
    }

    const res = await PromoCodeEngine.settleOnRenew(mockTx, {
      instanceId: 202,
      userId: 42,
      monthlyPrice: 50,
      months: 2,
      binding
    })

    assert.equal(res.unbound, true)
    assert.equal(res.quote.discountedMonths, 1)
    assert.equal(res.quote.regularMonths, 1)
    assert.equal(res.quote.discountAmount, 5)
    assert.equal(res.quote.finalAmount, 95)
    assert.equal(unboundInstanceId, 202, 'Should have unbound instance')
  }
}

// ==================== 6. Promo Refund Eligibility Tests ====================
async function testRefundEligibility() {
  console.log('  Testing PromoCodeEngine.getPromoRefundEligibility...')

  // 6.1 Active promo binding present -> NOT refundable (PROMO_BINDING_ACTIVE)
  {
    const mockTx: any = {
      instancePromoBinding: {
        findUnique: async () => ({ id: 1, instanceId: 301, promoCodeId: 10 })
      }
    }
    const res = await PromoCodeEngine.getPromoRefundEligibility(301, mockTx)
    assert.equal(res.isRefundable, false)
    assert.equal(res.reason, 'PROMO_BINDING_ACTIVE')
  }

  // 6.2 First-month once discount used, and no paid renew billing records exist -> PROMO_FIRST_CYCLE_LOCKED
  {
    const mockTx: any = {
      instancePromoBinding: {
        findUnique: async () => null
      },
      promoRedemptionLog: {
        findMany: async () => [{ id: 1, instanceId: 302, actionType: 'create' }]
      },
      instanceBillingRecord: {
        count: async () => 0
      }
    }
    const res = await PromoCodeEngine.getPromoRefundEligibility(302, mockTx)
    assert.equal(res.isRefundable, false)
    assert.equal(res.reason, 'PROMO_FIRST_CYCLE_LOCKED')
  }

  // 6.3 First-month once discount used, but has at least 1 renew record -> refundable
  {
    const mockTx: any = {
      instancePromoBinding: {
        findUnique: async () => null
      },
      promoRedemptionLog: {
        findMany: async () => [{ id: 1, instanceId: 303, actionType: 'create' }]
      },
      instanceBillingRecord: {
        count: async () => 1
      }
    }
    const res = await PromoCodeEngine.getPromoRefundEligibility(303, mockTx)
    assert.equal(res.isRefundable, true)
  }

  // 6.4 No promo used at all -> refundable
  {
    const mockTx: any = {
      instancePromoBinding: {
        findUnique: async () => null
      },
      promoRedemptionLog: {
        findMany: async () => []
      }
    }
    const res = await PromoCodeEngine.getPromoRefundEligibility(304, mockTx)
    assert.equal(res.isRefundable, true)
  }
}

async function runAllTests() {
  await testValidate()
  testQuotes()
  await testSettleOnCreate()
  await testSettleOnRenew()
  await testRefundEligibility()
  console.log('✅ All PromoCodeEngine orchestration tests passed!')
}

runAllTests().catch(err => {
  console.error('❌ Test failed:', err)
  process.exit(1)
})
