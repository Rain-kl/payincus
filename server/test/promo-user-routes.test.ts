import assert from 'node:assert/strict'
import Fastify from 'fastify'
import { Prisma } from '@prisma/client'
import { prisma } from '../src/db/prisma.js'
import promoCodesRoutes from '../src/routes/promo-codes.js'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

console.log('Testing User Promo Routes & Instance Integration (TDD)...')

// ==================== Setup Test Fastify App ====================
async function buildTestApp() {
  const app = Fastify({ logger: false })

  // Mock authentication decorators
  app.decorate('authenticate', async (request: any, reply: any) => {
    const mockUser = request.headers['x-mock-user']
    if (mockUser) {
      request.user = JSON.parse(mockUser)
    } else {
      request.user = { id: 10, role: 'user' }
    }
  })

  app.decorate('authenticateUser', async (request: any, reply: any) => {
    const mockUser = request.headers['x-mock-user']
    if (mockUser) {
      request.user = JSON.parse(mockUser)
    } else {
      request.user = { id: 10, role: 'user' }
    }
  })

  await app.register(promoCodesRoutes, { prefix: '/api/promos' })
  await app.ready()
  return app
}

async function runTests() {
  const app = await buildTestApp()

  // Save original prisma methods
  const origPackagePlan = prisma.packagePlan?.findUnique
  const origInstance = prisma.instance?.findUnique
  const origPromoCode = prisma.promoCode?.findUnique
  const origInstancePromoBindingFindUnique = prisma.instancePromoBinding?.findUnique
  const origInstancePromoBindingUpsert = prisma.instancePromoBinding?.upsert
  const origRedemptionCount = prisma.promoRedemptionLog?.count

  try {
    // ==================== 1. POST /api/promos/validate Tests ====================
    console.log('  1. Testing POST /api/promos/validate...')

    // 1.1 Valid promo code
    {
      ;(prisma as any).packagePlan = {
        findUnique: async (args: any) => {
          if (args.where.id === 101) {
            return { id: 101, packageId: 1, price: new Prisma.Decimal(10000), billingCycle: 1 } // 100.00 CNY
          }
          return null
        }
      }
      ;(prisma as any).promoCode = {
        findUnique: async (args: any) => {
          if (args.where.code === 'DISCOUNT20') {
            return {
              id: 1,
              code: 'DISCOUNT20',
              name: '20% OFF',
              type: 'ADMIN_PROMO',
              userId: null,
              adminId: 1,
              isGlobal: true,
              discountType: 'PERCENTAGE',
              discountValue: new Prisma.Decimal(0.2),
              commissionRate: new Prisma.Decimal(0.05),
              durationType: 'REPEATING',
              durationCycles: 3,
              maxTotalUses: 100,
              usedTotalCount: 1,
              maxUsesPerUser: 1,
              startsAt: null,
              expiresAt: null,
              enabled: true,
              totalDiscountAmount: new Prisma.Decimal(20),
              totalEarnings: new Prisma.Decimal(0),
              scopes: [],
              _count: { bindings: 0, redemptionLogs: 0 }
            }
          }
          return null
        }
      }
      ;(prisma as any).promoRedemptionLog = {
        count: async () => 0
      }

      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/validate',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: {
          code: 'DISCOUNT20',
          packageId: 1,
          packagePlanId: 101
        }
      })

      assert.equal(res.statusCode, 200)
      const body = JSON.parse(res.payload)
      assert.equal(body.valid, true)
      assert.equal(body.code, 'DISCOUNT20')
      assert.equal(body.discountType, 'PERCENTAGE')
      assert.equal(body.discountValue, 0.2)
      assert.equal(body.durationType, 'REPEATING')
      assert.equal(body.durationCycles, 3)
      assert.equal(body.estimatedDiscount, 20)
      assert.equal(body.finalPrice, 80)
    }

    // 1.2 Invalid promo code (not found)
    {
      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/validate',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: {
          code: 'NON_EXISTENT',
          packageId: 1,
          packagePlanId: 101
        }
      })

      const body = JSON.parse(res.payload)
      assert.equal(body.valid, false)
      assert.equal(body.errorCode, 'PROMO_NOT_FOUND')
    }

    // 1.3 Missing required parameters
    {
      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/validate',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: {
          code: '',
          packageId: 1,
          packagePlanId: 101
        }
      })

      const body = JSON.parse(res.payload)
      assert.equal(body.valid, false)
      assert.equal(body.errorCode, 'PROMO_NOT_FOUND')
    }

    // ==================== 2. GET /api/promos/renew-preview/:instanceId Tests ====================
    console.log('  2. Testing GET /api/promos/renew-preview/:instanceId...')

    // 2.1 Instance with active binding (3 remaining cycles of 20% discount, monthly price 50)
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 201) {
            return {
              id: 201,
              userId: 10,
              billingPrice: new Prisma.Decimal(50),
              billingCycle: 1,
              packageId: 1,
              packagePlanId: 101
            }
          }
          return null
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findUnique: async (args: any) => {
          if (args.where.instanceId === 201) {
            return {
              id: 1,
              instanceId: 201,
              promoCodeId: 1,
              durationType: 'REPEATING',
              totalCycles: 3,
              usedCycles: 1,
              remainingCycles: 2,
              boundAt: new Date(),
              lastRedeemedAt: new Date(),
              promoCode: {
                id: 1,
                code: 'REPEAT3M',
                enabled: true,
                expiresAt: null,
                discountValue: new Prisma.Decimal(0.2),
                scopes: []
              }
            }
          }
          return null
        }
      }

      // Test multi-month standard options
      const res = await app.inject({
        method: 'GET',
        url: '/api/promos/renew-preview/201',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) }
      })

      assert.equal(res.statusCode, 200)
      const body = JSON.parse(res.payload)
      assert.equal(body.hasBinding, true)
      assert.equal(Array.isArray(body.options), true)
      assert.equal(body.options.length, 4) // 1, 3, 6, 12 months

      // Check 1 month option (fully discounted, remaining 2 -> 1, willUnbind: false)
      const opt1 = body.options.find((o: any) => o.months === 1)
      assert.equal(opt1.originalPrice, 50)
      assert.equal(opt1.discountAmount, 10)
      assert.equal(opt1.finalPrice, 40)
      assert.equal(opt1.discountedMonths, 1)
      assert.equal(opt1.regularMonths, 0)
      assert.equal(opt1.willUnbind, false)

      // Check 3 months option (2 discounted, 1 regular, willUnbind: true)
      const opt3 = body.options.find((o: any) => o.months === 3)
      assert.equal(opt3.originalPrice, 150)
      assert.equal(opt3.discountAmount, 20) // 2 * (50 * 0.2) = 20
      assert.equal(opt3.finalPrice, 130)
      assert.equal(opt3.discountedMonths, 2)
      assert.equal(opt3.regularMonths, 1)
      assert.equal(opt3.willUnbind, true)
    }

    // 2.2 Instance without active binding (0 discount)
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 201) {
            return {
              id: 201,
              userId: 10,
              billingPrice: new Prisma.Decimal(50),
              billingCycle: 1,
              packageId: 1,
              packagePlanId: 101
            }
          }
          if (args.where.id === 202) {
            return {
              id: 202,
              userId: 10,
              billingPrice: new Prisma.Decimal(50),
              billingCycle: 1,
              packageId: 1,
              packagePlanId: 101
            }
          }
          return null
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findUnique: async () => null
      }

      const res = await app.inject({
        method: 'GET',
        url: '/api/promos/renew-preview/202?months=6',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) }
      })

      assert.equal(res.statusCode, 200)
      const body = JSON.parse(res.payload)
      assert.equal(body.hasBinding, false)
      const opt6 = body.options.find((o: any) => o.months === 6)
      assert.equal(opt6.originalPrice, 300)
      assert.equal(opt6.discountAmount, 0)
      assert.equal(opt6.finalPrice, 300)
      assert.equal(opt6.regularMonths, 6)
      assert.equal(opt6.willUnbind, false)
    }

    // 2.3 Rejects unauthorized instance access
    {
      const res = await app.inject({
        method: 'GET',
        url: '/api/promos/renew-preview/201',
        headers: { 'x-mock-user': JSON.stringify({ id: 99, role: 'user' }) } // Different user
      })

      assert.equal(res.statusCode, 403)
    }

    // 2.4 Returns 404 when instance not found
    {
      const res = await app.inject({
        method: 'GET',
        url: '/api/promos/renew-preview/999',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) }
      })

      assert.equal(res.statusCode, 404)
    }

    // 2.5 Instance with disabled promo code in binding returns regular price with 0 discount
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 203) {
            return {
              id: 203,
              userId: 10,
              billingPrice: new Prisma.Decimal(50),
              billingCycle: 1,
              packageId: 1,
              packagePlanId: 101
            }
          }
          return null
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findUnique: async (args: any) => {
          if (args.where.instanceId === 203) {
            return {
              id: 2,
              instanceId: 203,
              promoCodeId: 2,
              durationType: 'FOREVER',
              totalCycles: null,
              usedCycles: 1,
              remainingCycles: null,
              boundAt: new Date(),
              lastRedeemedAt: new Date(),
              promoCode: {
                id: 2,
                code: 'DISABLEDPAY',
                enabled: false,
                expiresAt: null,
                discountValue: new Prisma.Decimal(0.5),
                scopes: []
              }
            }
          }
          return null
        }
      }

      const res = await app.inject({
        method: 'GET',
        url: '/api/promos/renew-preview/203?months=1',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) }
      })

      assert.equal(res.statusCode, 200)
      const body = JSON.parse(res.payload)
      assert.equal(body.hasBinding, true)
      assert.equal(body.isPromoActive, false)
      assert.equal(body.bindingStatus, 'DISABLED')
      assert.equal(body.options[0].discountAmount, 0)
      assert.equal(body.options[0].finalPrice, 50)
    }

    // ==================== 3. POST /api/promos/apply/:instanceId Tests ====================
    console.log('  3. Testing POST /api/promos/apply/:instanceId...')

    // 3.1 Binds valid PERCENTAGE promo code
    {
      let boundData: any = null
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 301) {
            return {
              id: 301,
              userId: 10,
              packageId: 1,
              packagePlanId: 101,
              status: 'running',
              host: { id: 1, name: 'regular-node-01' }
            }
          }
          return null
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findUnique: async () => null,
        upsert: async (args: any) => {
          boundData = args.create
          return { id: 1, ...args.create }
        }
      }
      ;(prisma as any).promoCode = {
        findUnique: async (args: any) => {
          if (args.where.code === 'PERCENT15') {
            return {
              id: 5,
              code: 'PERCENT15',
              type: 'ADMIN_PROMO',
              userId: null,
              isGlobal: true,
              discountType: 'PERCENTAGE',
              discountValue: new Prisma.Decimal(0.15),
              commissionRate: new Prisma.Decimal(0),
              durationType: 'FOREVER',
              durationCycles: null,
              maxTotalUses: 100,
              usedTotalCount: 0,
              maxUsesPerUser: 1,
              startsAt: null,
              expiresAt: null,
              enabled: true,
              scopes: []
            }
          }
          return null
        }
      }
      ;(prisma as any).promoRedemptionLog = {
        count: async () => 0
      }

      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/apply/301',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: { code: 'PERCENT15' }
      })

      assert.equal(res.statusCode, 200)
      const body = JSON.parse(res.payload)
      assert.equal(body.success, true)
      assert.ok(boundData)
      assert.equal(boundData.instanceId, 301)
      assert.equal(boundData.promoCodeId, 5)
      assert.equal(boundData.durationType, 'FOREVER')
    }

    // 3.2 Rejects if instance already has active binding
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 301) {
            return {
              id: 301,
              userId: 10,
              packageId: 1,
              packagePlanId: 101,
              status: 'running',
              host: { id: 1, name: 'regular-node-01' }
            }
          }
          return null
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findUnique: async (args: any) => {
          if (args.where.instanceId === 301) {
            return { id: 1, instanceId: 301, promoCodeId: 5 }
          }
          return null
        }
      }

      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/apply/301',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: { code: 'PERCENT15' }
      })

      assert.equal(res.statusCode, 400)
      const body = JSON.parse(res.payload)
      assert.equal(body.success, false)
      assert.equal(body.errorCode, 'PROMO_BINDING_ACTIVE')
    }

    // 3.3 Rejects non-PERCENTAGE promo code for binding
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 301) {
            return {
              id: 301,
              userId: 10,
              packageId: 1,
              packagePlanId: 101,
              status: 'running',
              host: { id: 1, name: 'regular-node-01' }
            }
          }
          return null
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findUnique: async () => null
      }
      ;(prisma as any).promoCode = {
        findUnique: async (args: any) => {
          if (args.where.code === 'FIXED20') {
            return {
              id: 6,
              code: 'FIXED20',
              type: 'ADMIN_PROMO',
              userId: null,
              isGlobal: true,
              discountType: 'FIXED_AMOUNT',
              discountValue: new Prisma.Decimal(20),
              commissionRate: new Prisma.Decimal(0),
              durationType: 'ONCE',
              durationCycles: null,
              maxTotalUses: 100,
              usedTotalCount: 0,
              maxUsesPerUser: 1,
              startsAt: null,
              expiresAt: null,
              enabled: true,
              scopes: []
            }
          }
          return null
        }
      }

      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/apply/301',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: { code: 'FIXED20' }
      })

      assert.equal(res.statusCode, 400)
      const body = JSON.parse(res.payload)
      assert.equal(body.success, false)
      assert.equal(body.errorCode, 'INVALID_DISCOUNT_TYPE')
    }

    // 3.4 Rejects apply on peer node instance
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 302) {
            return {
              id: 302,
              userId: 10,
              packageId: 1,
              packagePlanId: 101,
              status: 'running',
              host: { id: 1, name: 'peer-node-01' }
            }
          }
          return null
        }
      }

      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/apply/302',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: { code: 'PERCENT15' }
      })

      assert.equal(res.statusCode, 400)
      const body = JSON.parse(res.payload)
      assert.equal(body.success, false)
      assert.equal(body.errorCode, 'NOT_ELIGIBLE')
    }

    // 3.5 Rejects apply on deleted instance
    {
      ;(prisma as any).instance = {
        findUnique: async (args: any) => {
          if (args.where.id === 303) {
            return {
              id: 303,
              userId: 10,
              packageId: 1,
              packagePlanId: 101,
              status: 'deleted',
              host: { id: 1, name: 'node-01' }
            }
          }
          return null
        }
      }

      const res = await app.inject({
        method: 'POST',
        url: '/api/promos/apply/303',
        headers: { 'x-mock-user': JSON.stringify({ id: 10, role: 'user' }) },
        payload: { code: 'PERCENT15' }
      })

      assert.equal(res.statusCode, 404)
    }

    // ==================== 4. Instance Creation & Renewal Integration Check ====================
    console.log('  4. Testing instances.ts & billing integration structure...')
    const instancesSource = readFileSync(resolve(process.cwd(), 'src/routes/instances.ts'), 'utf8')

    assert.ok(
      instancesSource.includes('PromoCodeEngine.validate'),
      'instances.ts must call PromoCodeEngine.validate for promo codes'
    )
    assert.ok(
      instancesSource.includes('PromoCodeEngine.calculateCreationQuote'),
      'instances.ts must calculate creation quote using PromoCodeEngine'
    )
    assert.ok(
      instancesSource.includes('PromoCodeEngine.settleOnCreate'),
      'instances.ts must settle promo code on creation inside transaction'
    )
    assert.ok(
      instancesSource.includes("const promoDiscountAmount = pricingSource === 'aff' ? discountAmount : 0"),
      'instances.ts must attribute zero promo discount amount when VIP benefit is chosen'
    )
    assert.ok(
      instancesSource.includes("validation.error || pVal.error || '优惠码无效'"),
      'instances.ts must prioritize specific legacy validation error over generic not found error'
    )

    const billingOpsSource = readFileSync(resolve(process.cwd(), 'src/db/billing-operations.ts'), 'utf8')
    assert.ok(
      billingOpsSource.includes('PromoCodeEngine.settleOnRenew'),
      'billing-operations.ts performRenewal must call PromoCodeEngine.settleOnRenew'
    )

    const billingSchedulerSource = readFileSync(resolve(process.cwd(), 'src/services/billing-scheduler.ts'), 'utf8')
    assert.ok(
      billingSchedulerSource.includes('PromoCodeEngine.calculateRenewalQuote'),
      'billing-scheduler.ts must calculate promo renewal quote when checking auto-renew balance'
    )

    console.log('All Promo User Routes and Integration tests passed!')
  } finally {
    // Restore original prisma methods
    if (origPackagePlan) (prisma as any).packagePlan.findUnique = origPackagePlan
    if (origInstance) (prisma as any).instance.findUnique = origInstance
    if (origPromoCode) (prisma as any).promoCode.findUnique = origPromoCode
    if (origInstancePromoBindingFindUnique) (prisma as any).instancePromoBinding.findUnique = origInstancePromoBindingFindUnique
    if (origInstancePromoBindingUpsert) (prisma as any).instancePromoBinding.upsert = origInstancePromoBindingUpsert
    if (origRedemptionCount) (prisma as any).promoRedemptionLog.count = origRedemptionCount
  }
}

runTests().catch(err => {
  console.error('Test failed with error:', err)
  process.exit(1)
})
