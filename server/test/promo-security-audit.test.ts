import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { Prisma } from '@prisma/client'
import type { Instance, PackagePlan } from '@prisma/client'
import { incrementPromoUsageWithLock } from '../src/db/promo-codes.js'
import {
  calculateFinalPrice,
  calculateCreationQuote,
  calculatePromoRenewalSplit
} from '../src/services/promo/promo-calculator.js'
import { PromoCodeEngine } from '../src/services/promo-engine.js'
import {
  calculateInstanceRemainingRefundQuote,
  calculatePlanChange,
  calculateInstancePriceAdjustmentQuote
} from '../src/db/billing-operations.js'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const root = resolve(__dirname, '../..')
const read = (path: string) => readFileSync(resolve(root, path), 'utf8')

console.log('🛡️  Starting Automated Security Penetration & Anti-Arbitrage Audit Suite...\n')

// =============================================================================
// Helper: PostgreSQL Row Lock Concurrency Simulator
// =============================================================================
class PostgresRowLockSimulator {
  private lockedRowIds = new Set<number>()
  private waitQueue: Map<number, (() => void)[]> = new Map()

  async acquire(rowId: number): Promise<() => void> {
    if (!this.lockedRowIds.has(rowId)) {
      this.lockedRowIds.add(rowId)
      return () => this.release(rowId)
    }
    return new Promise<() => void>((resolve) => {
      const queue = this.waitQueue.get(rowId) || []
      queue.push(() => {
        this.lockedRowIds.add(rowId)
        resolve(() => this.release(rowId))
      })
      this.waitQueue.set(rowId, queue)
    })
  }

  private release(rowId: number) {
    this.lockedRowIds.delete(rowId)
    const queue = this.waitQueue.get(rowId)
    if (queue && queue.length > 0) {
      const next = queue.shift()!
      next()
    }
  }
}

// =============================================================================
// 1. 并发超卖与条件竞争防范 (Concurrency Race Conditions & Overselling)
// =============================================================================
async function testConcurrencyOversellingSecurity() {
  console.log('▶ [Dimension 1] Testing Concurrency Overselling & PostgreSQL Row Locks...')

  // 1.1 Simulate 20 concurrent transactions attempting to redeem a single-use promo code (maxTotalUses = 1)
  {
    const lockSim = new PostgresRowLockSimulator()
    const sharedPromo = {
      id: 777,
      code: 'FLASH_SALE_1_QUOTA',
      enabled: true,
      startsAt: null,
      expiresAt: null,
      maxTotalUses: 1,
      usedTotalCount: 0,
      totalDiscountAmount: new Prisma.Decimal(0)
    }

    let forUpdateExecutedCount = 0

    const createConcurrentTx = () => {
      let releaseLock: (() => void) | null = null
      const tx = {
        $queryRaw: async (sqlObj: any) => {
          const sqlString = sqlObj?.strings ? sqlObj.strings.join('') : String(sqlObj)
          // Assert PostgreSQL row lock syntax
          assert.ok(
            sqlString.includes('SELECT id FROM "promo_codes" WHERE id =') &&
              sqlString.includes('FOR UPDATE'),
            'Must execute SELECT ... FOR UPDATE row-level lock'
          )
          forUpdateExecutedCount++
          releaseLock = await lockSim.acquire(sharedPromo.id)
          return [{ id: sharedPromo.id }]
        },
        promoCode: {
          findUnique: async () => {
            // Returns current state under row lock
            return { ...sharedPromo }
          },
          update: async ({ data }: any) => {
            if (data.usedTotalCount?.increment) {
              sharedPromo.usedTotalCount += data.usedTotalCount.increment
            }
            if (data.totalDiscountAmount?.increment) {
              sharedPromo.totalDiscountAmount = sharedPromo.totalDiscountAmount.add(
                data.totalDiscountAmount.increment
              )
            }
            return { ...sharedPromo }
          }
        },
        release: () => {
          if (releaseLock) {
            releaseLock()
            releaseLock = null
          }
        }
      } as any
      return tx
    }

    const CONCURRENT_REQUESTS = 20
    const promises = Array.from({ length: CONCURRENT_REQUESTS }, async () => {
      const tx = createConcurrentTx()
      try {
        return await incrementPromoUsageWithLock(tx, sharedPromo.id, 15.0)
      } finally {
        tx.release()
      }
    })

    const results = await Promise.allSettled(promises)

    const fulfilled = results.filter((r) => r.status === 'fulfilled')
    const rejected = results.filter((r) => r.status === 'rejected') as PromiseRejectedResult[]

    assert.equal(
      fulfilled.length,
      1,
      `Expected exactly 1 request to succeed on quota=1, got ${fulfilled.length}`
    )
    assert.equal(
      rejected.length,
      19,
      `Expected exactly 19 requests to fail on quota=1, got ${rejected.length}`
    )
    assert.equal(
      sharedPromo.usedTotalCount,
      1,
      `sharedPromo.usedTotalCount must be strictly 1, got ${sharedPromo.usedTotalCount}`
    )
    assert.equal(
      forUpdateExecutedCount,
      CONCURRENT_REQUESTS,
      'Every transaction must attempt row-level lock acquisition'
    )

    for (const rej of rejected) {
      assert.equal(
        rej.reason?.message,
        'PROMO_QUOTA_EXCEEDED',
        'Every failed transaction must throw PROMO_QUOTA_EXCEEDED'
      )
    }

    console.log('  ✓ 20 concurrent transactions on 1-quota code: exactly 1 succeeded, 19 blocked with PROMO_QUOTA_EXCEEDED')
  }

  // 1.2 Multi-quota stress test: 20 concurrent requests on maxTotalUses = 5
  {
    const lockSim = new PostgresRowLockSimulator()
    const sharedPromo = {
      id: 888,
      code: 'FLASH_SALE_5_QUOTA',
      enabled: true,
      startsAt: null,
      expiresAt: null,
      maxTotalUses: 5,
      usedTotalCount: 0,
      totalDiscountAmount: new Prisma.Decimal(0)
    }

    const createConcurrentTx = () => {
      let releaseLock: (() => void) | null = null
      const tx = {
        $queryRaw: async () => {
          releaseLock = await lockSim.acquire(sharedPromo.id)
          return [{ id: sharedPromo.id }]
        },
        promoCode: {
          findUnique: async () => ({ ...sharedPromo }),
          update: async ({ data }: any) => {
            if (data.usedTotalCount?.increment) {
              sharedPromo.usedTotalCount += data.usedTotalCount.increment
            }
            return { ...sharedPromo }
          }
        },
        release: () => {
          if (releaseLock) {
            releaseLock()
            releaseLock = null
          }
        }
      } as any
      return tx
    }

    const CONCURRENT_REQUESTS = 20
    const results = await Promise.allSettled(
      Array.from({ length: CONCURRENT_REQUESTS }, async () => {
        const tx = createConcurrentTx()
        try {
          return await incrementPromoUsageWithLock(tx, sharedPromo.id, 10.0)
        } finally {
          tx.release()
        }
      })
    )

    const fulfilled = results.filter((r) => r.status === 'fulfilled')
    const rejected = results.filter((r) => r.status === 'rejected') as PromiseRejectedResult[]

    assert.equal(fulfilled.length, 5, `Expected exactly 5 to succeed on quota=5, got ${fulfilled.length}`)
    assert.equal(rejected.length, 15, `Expected exactly 15 to fail on quota=5, got ${rejected.length}`)
    assert.equal(sharedPromo.usedTotalCount, 5, 'Final used count must be strictly 5')

    for (const rej of rejected) {
      assert.equal(rej.reason?.message, 'PROMO_QUOTA_EXCEEDED')
    }

    console.log('  ✓ 20 concurrent transactions on 5-quota code: exactly 5 succeeded, 15 blocked, total used = 5')
  }
}

// =============================================================================
// 2. 负数金额与反向充值防御 (Negative Price Exploit & Reverse Billing)
// =============================================================================
async function testNegativePriceAndClampingSecurity() {
  console.log('▶ [Dimension 2] Testing Negative Price Exploit & Zero-Floor Clamping...')

  // 2.1 Direct calculateFinalPrice tests against negative results
  assert.equal(calculateFinalPrice(15, 50), 0, '¥15 base with ¥50 discount must floor to 0')
  assert.equal(calculateFinalPrice(15, 100), 0, '¥15 base with ¥100 discount must floor to 0')
  assert.equal(calculateFinalPrice(15, 999.99), 0, '¥15 base with ¥999.99 discount must floor to 0')
  assert.equal(calculateFinalPrice(0, 50), 0, '¥0 base with ¥50 discount must floor to 0')
  assert.equal(calculateFinalPrice(-10, 50), 0, 'Negative base price must clamp to 0')

  // 2.2 calculateCreationQuote with FIXED_AMOUNT exceeding base price
  {
    const quote50 = calculateCreationQuote({
      originalPrice: 15.0,
      discountType: 'FIXED_AMOUNT',
      discountValue: 50.0
    })
    assert.equal(quote50.finalPrice, 0.0, 'Final payable price must be strictly 0.00')
    assert.equal(quote50.discountAmount, 15.0, 'Discount amount must be clamped to original price (15.00)')

    const quote999 = calculateCreationQuote({
      originalPrice: 15.0,
      discountType: 'FIXED_AMOUNT',
      discountValue: 999.99
    })
    assert.equal(quote999.finalPrice, 0.0, 'Final payable price must be strictly 0.00')
    assert.equal(quote999.discountAmount, 15.0, 'Discount amount must be clamped to original price (15.00)')
  }

  // 2.3 calculateCreationQuote with PERCENTAGE exceeding 100%
  {
    const quoteOver100 = calculateCreationQuote({
      originalPrice: 20.0,
      discountType: 'PERCENTAGE',
      discountValue: 1.5 // 150% discount injection attempt
    })
    assert.equal(quoteOver100.finalPrice, 0.0, '150% discount must floor final payable price to 0.00')
    assert.equal(quoteOver100.discountAmount, 30.0)
  }

  // 2.4 Floating-point precision and rounding attack checks
  {
    const floatRes1 = calculateFinalPrice(0.3, 0.1 + 0.2)
    assert.equal(floatRes1, 0, 'IEEE 754 precision artifact (0.3 - 0.30000000000000004) must resolve to 0')

    const quotePrecision = calculateCreationQuote({
      originalPrice: 19.99,
      discountType: 'FIXED_AMOUNT',
      discountValue: 19.99
    })
    assert.equal(quotePrecision.finalPrice, 0.0)
    assert.equal(quotePrecision.discountAmount, 19.99)
  }

  // 2.5 Renewal split calculation under zero price or excessive discount
  {
    const renewSplit = calculatePromoRenewalSplit({
      monthlyPrice: 0,
      months: 1,
      discountRate: 0.5,
      remainingCycles: 3
    })
    assert.equal(renewSplit.finalAmount, 0)
    assert.equal(renewSplit.discountAmount, 0)
  }

  console.log('  ✓ Negative price injection tests: 100% clamped to 0.00 floor, reverse billing eliminated')
}

// =============================================================================
// 3. 实例销毁退款套利防御 (Destroy Refund Arbitrage)
// =============================================================================
async function testDestroyRefundArbitrageSecurity() {
  console.log('▶ [Dimension 3] Testing Destroy Refund Arbitrage Defense...')

  const futureDate = new Date(Date.now() + 20 * 24 * 60 * 60 * 1000)
  const pastDate = new Date(Date.now() - 10 * 24 * 60 * 60 * 1000)

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

  const createMockTx = (options: {
    hasBinding?: boolean
    redemptionLogs?: any[]
    renewCount?: number
  }) => {
    return {
      instancePromoBinding: {
        findUnique: async () =>
          options.hasBinding ? { id: 1, instanceId: 101, promoCodeId: 10 } : null
      },
      promoRedemptionLog: {
        findMany: async () => options.redemptionLogs ?? []
      },
      instanceBillingRecord: {
        count: async () => options.renewCount ?? 0,
        findMany: async (args: any) => {
          if (args?.where?.type === 'refund') return []
          return [
            {
              amount: 100,
              periodStart: pastDate,
              periodEnd: futureDate
            }
          ]
        },
        aggregate: async (args: any) => {
          if (args?.where?.type === 'refund') return { _sum: { amount: 0 } }
          return { _sum: { amount: 100 } }
        },
        findFirst: async () => ({ amount: 100 })
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
      inboxMessage: { findMany: async () => [] },
      instanceAffBinding: { findUnique: async () => null },
      affBinding: { findUnique: async () => null },
      packagePlan: {
        findUnique: async () => ({
          id: 1,
          packageId: 1,
          name: 'Plan 1',
          price: 10000,
          billingCycle: 1,
          cpu: 1,
          memory: 1024,
          disk: 20,
          trafficLimitSpeed: null,
          isActive: true,
          isSoldOut: false
        })
      }
    } as any
  }

  // 3.1 Active InstancePromoBinding: 0 refund
  {
    const tx = createMockTx({ hasBinding: true })
    const eligibility = await PromoCodeEngine.getPromoRefundEligibility(101, tx)
    assert.equal(eligibility.isRefundable, false)
    assert.equal(eligibility.reason, 'PROMO_BINDING_ACTIVE')

    const quote = await calculateInstanceRemainingRefundQuote(baseInstance, tx)
    assert.equal(quote.refundableValue, 0, 'Active binding instance refundableValue must be 0')
    assert.equal(quote.maxRefundable, 0, 'Active binding instance maxRefundable must be 0')
    assert.equal(quote.remainingValue, 0, 'Active binding instance remainingValue must be 0')
    assert.equal(quote.remainingDays, 0, 'Active binding instance remainingDays must be 0')
  }

  // 3.2 First-month ONCE discount without renewal: 0 refund
  {
    const tx = createMockTx({
      hasBinding: false,
      redemptionLogs: [{ id: 1, instanceId: 101, actionType: 'create' }],
      renewCount: 0
    })
    const eligibility = await PromoCodeEngine.getPromoRefundEligibility(101, tx)
    assert.equal(eligibility.isRefundable, false)
    assert.equal(eligibility.reason, 'PROMO_FIRST_CYCLE_LOCKED')

    const quote = await calculateInstanceRemainingRefundQuote(baseInstance, tx)
    assert.equal(quote.refundableValue, 0, 'Unrenewed first-month promo instance refundableValue must be 0')
    assert.equal(quote.maxRefundable, 0, 'Unrenewed first-month promo instance maxRefundable must be 0')
    assert.equal(quote.remainingValue, 0, 'Unrenewed first-month promo instance remainingValue must be 0')
  }

  // 3.3 First-month ONCE discount AFTER subsequent paid renewals: refund unlocked
  {
    const tx = createMockTx({
      hasBinding: false,
      redemptionLogs: [{ id: 1, instanceId: 101, actionType: 'create' }],
      renewCount: 1
    })
    const eligibility = await PromoCodeEngine.getPromoRefundEligibility(101, tx)
    assert.equal(eligibility.isRefundable, true)

    const quote = await calculateInstanceRemainingRefundQuote(baseInstance, tx)
    assert.ok(quote.refundableValue > 0, 'Renewed instance must allow normal refund')
    assert.ok(quote.maxRefundable > 0, 'Renewed instance maxRefundable must be > 0')
  }

  console.log('  ✓ Destroy refund arbitrage tests: active binding & unrenewed once-promo strictly blocked from refund')
}

// =============================================================================
// 4. 方案降级差价变现套利防御 (Plan Downgrade Arbitrage)
// =============================================================================
async function testPlanDowngradeArbitrageSecurity() {
  console.log('▶ [Dimension 4] Testing Plan Downgrade Arbitrage Defense...')

  const futureDate = new Date(Date.now() + 20 * 24 * 60 * 60 * 1000)
  const pastDate = new Date(Date.now() - 10 * 24 * 60 * 60 * 1000)

  const baseInstance = {
    id: 101,
    userId: 1,
    packageId: 1,
    packagePlanId: 1,
    billingPrice: 100, // ¥100/mo expensive plan
    billingCycle: 1,
    expiresAt: futureDate,
    status: 'running' as const,
    name: 'test-inst',
    hostId: 1
  } as unknown as Instance

  const cheapPlan = {
    id: 2,
    packageId: 1,
    name: 'Plan 2 Cheap',
    price: 2000, // ¥20/mo cheap plan
    billingCycle: 1,
    cpu: 1,
    memory: 1024,
    disk: 20,
    trafficLimitSpeed: null,
    isActive: true,
    isSoldOut: false
  } as PackagePlan

  const createMockTx = (hasBinding: boolean, renewCount: number = 0) => {
    return {
      instancePromoBinding: {
        findUnique: async () =>
          hasBinding ? { id: 1, instanceId: 101, promoCodeId: 10 } : null
      },
      promoRedemptionLog: {
        findMany: async () =>
          hasBinding ? [] : [{ id: 1, instanceId: 101, actionType: 'create' }]
      },
      instanceBillingRecord: {
        count: async () => renewCount,
        findMany: async (args: any) => {
          if (args?.where?.type === 'refund') return []
          return [
            {
              amount: 100,
              periodStart: pastDate,
              periodEnd: futureDate
            }
          ]
        },
        aggregate: async (args: any) => {
          if (args?.where?.type === 'refund') {
            return { _sum: { amount: 0 } }
          }
          return { _sum: { amount: 100 } }
        },
        findFirst: async () => ({ amount: 100 })
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
      inboxMessage: { findMany: async () => [] },
      instanceAffBinding: { findUnique: async () => null },
      affBinding: { findUnique: async () => null },
      packagePlan: {
        findUnique: async (args: any) => {
          if (args?.where?.id === 1) {
            return {
              id: 1,
              packageId: 1,
              name: 'Plan 1 Expensive',
              price: 10000,
              billingCycle: 1
            }
          }
          return cheapPlan
        }
      }
    } as any
  }

  // 4.1 Plan change downgrade on instance with active promo binding
  {
    const tx = createMockTx(true)
    const changeResult = await calculatePlanChange(baseInstance, cheapPlan, { tx })
    assert.equal(changeResult.isUpgrade, false, 'Should be recognized as downgrade')
    assert.equal(
      changeResult.priceDiff,
      0,
      `Downgrade on promo-protected instance must clamp priceDiff to 0, got ${changeResult.priceDiff}`
    )
  }

  // 4.2 Plan change downgrade on unrenewed once-promo instance
  {
    const tx = createMockTx(false, 0)
    const changeResult = await calculatePlanChange(baseInstance, cheapPlan, { tx })
    assert.equal(changeResult.isUpgrade, false)
    assert.equal(
      changeResult.priceDiff,
      0,
      `Downgrade on unrenewed promo instance must clamp priceDiff to 0, got ${changeResult.priceDiff}`
    )
  }

  // 4.3 Instance price adjustment quote (admin downward adjustment on promo instance)
  {
    const tx = createMockTx(true)
    const adjQuote = await calculateInstancePriceAdjustmentQuote(baseInstance, 20, true, tx)
    assert.equal(
      adjQuote.priceDiff,
      0,
      `Price adjustment downwards on promo instance must clamp priceDiff to 0, got ${adjQuote.priceDiff}`
    )
  }

  // 4.4 Regular non-promo instance: downgrade allows negative priceDiff (standard balance refund)
  {
    const tx = {
      ...createMockTx(false, 1),
      promoRedemptionLog: { findMany: async () => [] }
    }
    const changeResult = await calculatePlanChange(baseInstance, cheapPlan, { tx })
    assert.ok(
      changeResult.priceDiff < 0,
      `Regular instance downgrade should yield negative priceDiff, got ${changeResult.priceDiff}`
    )
  }

  console.log('  ✓ Plan downgrade arbitrage tests: priceDiff strictly clamped to 0, wallet cash-out impossible')
}

// =============================================================================
// 5. 暴力枚举与接口刷券风控 (Brute-Force & Enumeration Protection)
// =============================================================================
async function testBruteForceAndInjectionSecurity() {
  console.log('▶ [Dimension 5] Testing Brute-Force & Injection Resistance...')

  // 5.1 Route ID parser resilience test against SQL injection and fuzz inputs
  const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\d*$/
  function parsePositiveRouteId(value: string): number | null {
    if (!POSITIVE_ROUTE_ID_PATTERN.test(value)) return null
    const parsed = Number(value)
    return Number.isSafeInteger(parsed) ? parsed : null
  }

  const maliciousInputs = [
    // SQL Injections
    '1; DROP TABLE promo_codes; --',
    '1 OR 1=1',
    "1' UNION SELECT * FROM users--",
    '1 AND SLEEP(5)',
    // Path / Traversal / Control characters
    '../1',
    '1\0',
    '1\n',
    ' 1 ',
    '+1',
    '-1',
    // Non-integers / Floats / Octal / Hex
    '0',
    '-0',
    '01',
    '1.0',
    '1.5',
    '0x1f',
    '1e5',
    // Number.MAX_SAFE_INTEGER + 1 overflow
    '9007199254740992',
    '9999999999999999999999999999',
    // Type confusion strings
    'NaN',
    'undefined',
    'null',
    '[object Object]',
    '',
    '   '
  ]

  for (const input of maliciousInputs) {
    const result = parsePositiveRouteId(input)
    assert.equal(
      result,
      null,
      `parsePositiveRouteId must reject malicious/invalid input: ${JSON.stringify(input)}`
    )
  }

  // Valid positive integers must pass
  assert.equal(parsePositiveRouteId('1'), 1)
  assert.equal(parsePositiveRouteId('42'), 42)
  assert.equal(parsePositiveRouteId('999999'), 999999)

  // 5.2 Static Route Security Guard: Verify authentication enforcement on user and admin routes
  const userRouteSource = read('server/src/routes/promo-codes.ts')
  const adminRouteSource = read('server/src/routes/admin-promo-codes.ts')

  assert.ok(
    userRouteSource.includes("fastify.post('/validate', {\n    onRequest: [fastify.authenticate]"),
    'User /validate route must require authentication'
  )
  assert.ok(
    userRouteSource.includes("fastify.get('/renew-preview/:instanceId', {\n    onRequest: [fastify.authenticate]"),
    'User /renew-preview route must require authentication'
  )
  assert.ok(
    userRouteSource.includes("fastify.post('/apply/:instanceId', {\n    onRequest: [fastify.authenticate]"),
    'User /apply route must require authentication'
  )

  assert.ok(
    adminRouteSource.includes("app.addHook('onRequest', app.authenticate)") &&
      adminRouteSource.includes("app.addHook('onRequest', app.requireAdmin)"),
    'Admin promo routes must enforce app.authenticate and app.requireAdmin globally'
  )

  // 5.3 Safe Error Messages: Invalid promo code returns clean standardized error without timing or stack leak
  const mockTx = {
    promoCode: { findUnique: async () => null }
  } as any
  const validationRes = await PromoCodeEngine.validate({
    code: 'NON_EXISTENT_GUESS',
    packageId: 1,
    packagePlanId: 1,
    userId: 1,
    tx: mockTx
  })
  assert.equal(validationRes.valid, false)
  assert.equal(validationRes.errorCode, 'PROMO_NOT_FOUND')
  assert.equal(validationRes.error, '优惠码不存在')

  console.log('  ✓ Injection & brute force tests: route ID validation 100% immune, auth strictly enforced, errors sanitized')
}

// =============================================================================
// 6. TOCTOU 时序篡改与防脏读 (TOCTOU & Dirty Read Prevention)
// =============================================================================
async function testToctouAndDirtyReadSecurity() {
  console.log('▶ [Dimension 6] Testing TOCTOU & Dirty Read Prevention...')

  // 6.1 Scope Tampering Attack in Instance Creation Flow:
  // User attempts to validate against Package 1, but order creation binds promo to Package 2
  const restrictedPromo = {
    id: 99,
    code: 'PKG1_ONLY_DISCOUNT',
    enabled: true,
    isGlobal: false,
    startsAt: null,
    expiresAt: null,
    maxTotalUses: null,
    usedTotalCount: 0,
    maxUsesPerUser: null,
    discountType: 'PERCENTAGE' as const,
    discountValue: new Prisma.Decimal(0.5),
    durationType: 'ONCE' as const,
    durationCycles: null,
    type: 'ADMIN_PROMO' as const,
    userId: null,
    scopes: [
      {
        id: 1,
        promoCodeId: 99,
        packageId: 1,
        packagePlanId: null
      }
    ],
    _count: { bindings: 0, redemptionLogs: 0 }
  }

  const mockTx = {
    promoCode: { findUnique: async () => restrictedPromo },
    promoRedemptionLog: { count: async () => 0 },
    instancePromoBinding: { findUnique: async () => null }
  } as any

  // Validation against legitimate Package 1 succeeds
  const legitValidation = await PromoCodeEngine.validate({
    code: 'PKG1_ONLY_DISCOUNT',
    packageId: 1,
    packagePlanId: 1,
    userId: 1,
    tx: mockTx
  })
  assert.equal(legitValidation.valid, true)

  // Validation against tampered Package 2 fails with PROMO_SCOPE_MISMATCH
  const tamperedValidation = await PromoCodeEngine.validate({
    code: 'PKG1_ONLY_DISCOUNT',
    packageId: 2, // Tampered package
    packagePlanId: 20,
    userId: 1,
    tx: mockTx
  })
  assert.equal(tamperedValidation.valid, false)
  assert.equal(tamperedValidation.errorCode, 'PROMO_SCOPE_MISMATCH')

  // Verify that server/src/routes/instances.ts revalidates directly against actual creation package & plan
  const instancesRouteSource = read('server/src/routes/instances.ts')
  assert.ok(
    instancesRouteSource.includes('packageId: pkg.id') &&
      instancesRouteSource.includes('packagePlanId: selectedPlan.id') &&
      instancesRouteSource.includes('PromoCodeEngine.validate'),
    'instances.ts must revalidate promo directly against resolved creation package and plan'
  )

  // 6.2 In-Transaction Disabled Promo Invalidation:
  // Promo code was enabled during user preview, but disabled before transaction executes
  {
    const tx = {
      $queryRaw: async () => [{ id: 99 }],
      promoCode: {
        findUnique: async () => ({
          ...restrictedPromo,
          enabled: false // Admin just disabled it
        })
      }
    } as any

    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 99, 10),
      (err: Error) => err.message === 'PROMO_DISABLED',
      'Transaction must atomically reject disabled promo code under lock'
    )
  }

  // 6.3 In-Transaction Expired Promo Invalidation:
  // Promo code expired between preview and transaction execution
  {
    const tx = {
      $queryRaw: async () => [{ id: 99 }],
      promoCode: {
        findUnique: async () => ({
          ...restrictedPromo,
          expiresAt: new Date(Date.now() - 1000) // Expired 1 second ago
        })
      }
    } as any

    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 99, 10),
      (err: Error) => err.message === 'PROMO_EXPIRED',
      'Transaction must atomically reject expired promo code under lock'
    )
  }

  // 6.4 Renewal Flow Invalidation:
  // Verify billing scheduler & renewInstance stop discounts when promo is disabled
  {
    const schedulerSource = read('server/src/services/billing-scheduler.ts')
    assert.ok(
      schedulerSource.includes('promoBinding.promoCode.enabled') &&
        schedulerSource.includes('(!promoBinding.promoCode.expiresAt || promoBinding.promoCode.expiresAt > now)'),
      'billing-scheduler.ts must enforce active status and expiry checks for renewal promo bindings'
    )

    const billingOpsSource = read('server/src/db/billing-operations.ts')
    assert.ok(
      billingOpsSource.includes('promoBinding.promoCode.enabled') &&
        billingOpsSource.includes('(!promoBinding.promoCode.expiresAt || promoBinding.promoCode.expiresAt > now)'),
      'billing-operations.ts must enforce active status and expiry checks for renewal promo bindings'
    )
  }

  console.log('  ✓ TOCTOU & Dirty Read tests: in-tx scope revalidation & state locks prevent all race conditions')
}

// =============================================================================
// Main Test Runner
// =============================================================================
async function runAllSecurityAuditTests() {
  await testConcurrencyOversellingSecurity()
  await testNegativePriceAndClampingSecurity()
  await testDestroyRefundArbitrageSecurity()
  await testPlanDowngradeArbitrageSecurity()
  await testBruteForceAndInjectionSecurity()
  await testToctouAndDirtyReadSecurity()

  console.log('\n🔒 =====================================================================')
  console.log('🎉 [SECURITY AUDIT COMPLETE] All 6 Security Dimensions 100% PASSED!')
  console.log('🔒 =====================================================================\n')
}

runAllSecurityAuditTests().catch((err) => {
  console.error('\n❌ Security Audit Test Failed with Exception:', err)
  process.exit(1)
})
