import assert from 'node:assert/strict'
import Fastify from 'fastify'
import { Prisma } from '@prisma/client'
import { prisma } from '../src/db/prisma.js'
import adminPromoCodesRoutes from '../src/routes/admin-promo-codes.js'
import {
  sendPromoBindingExhaustedNotification,
  sendPromoUnboundByAdminNotification,
  sendPromoCodeDeletedNotification
} from '../src/services/promo/notifications.js'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

console.log('Testing Admin Promo Routes & In-Site Notifications (TDD)...')

// ==================== Setup Test Fastify App ====================
async function buildTestApp() {
  const app = Fastify({ logger: false })

  // Mock authenticate & requireAdmin decorators
  app.decorate('authenticate', async (request: any, reply: any) => {
    const mockUser = request.headers['x-mock-user']
    if (mockUser) {
      request.user = JSON.parse(mockUser)
    } else {
      return reply.code(401).send({ error: 'Unauthorized', code: 'UNAUTHORIZED' })
    }
  })

  app.decorate('requireAdmin', async (request: any, reply: any) => {
    if (!request.user) {
      return reply.code(401).send({ error: 'Unauthorized', code: 'UNAUTHORIZED' })
    }
    if (request.user.role !== 'admin') {
      return reply.code(403).send({ error: 'Admin privileges required', code: 'ADMIN_REQUIRED' })
    }
  })

  await app.register(adminPromoCodesRoutes, { prefix: '/api/admin/promos' })
  await app.ready()
  return app
}

async function runTests() {
  const app = await buildTestApp()

  const adminHeaders = {
    'x-mock-user': JSON.stringify({ id: 1, username: 'admin', role: 'admin' })
  }
  const userHeaders = {
    'x-mock-user': JSON.stringify({ id: 10, username: 'user1', role: 'user' })
  }

  // Save original prisma methods
  const origPromoCodeFindMany = prisma.promoCode?.findMany
  const origPromoCodeCount = prisma.promoCode?.count
  const origPromoCodeFindUnique = prisma.promoCode?.findUnique
  const origPromoCodeCreate = prisma.promoCode?.create
  const origPromoCodeUpdate = prisma.promoCode?.update
  const origPromoCodeDelete = prisma.promoCode?.delete
  const origBindingFindMany = prisma.instancePromoBinding?.findMany
  const origBindingFindFirst = prisma.instancePromoBinding?.findFirst
  const origBindingCount = prisma.instancePromoBinding?.count
  const origBindingDeleteMany = prisma.instancePromoBinding?.deleteMany
  const origScopeDeleteMany = prisma.promoCodeScope?.deleteMany
  const origInboxMessageCreate = prisma.inboxMessage?.create
  const origTransaction = prisma.$transaction

  try {
    // ==================== Suite 1: In-Site Notification Helpers ====================
    console.log('  1. Testing in-site notification helpers...')
    {
      const createdMessages: any[] = []
      ;(prisma as any).inboxMessage = {
        create: async (args: any) => {
          createdMessages.push(args.data)
          return { id: 123 }
        }
      }

      // 1.1 sendPromoBindingExhaustedNotification
      await sendPromoBindingExhaustedNotification({
        userId: 10,
        instanceId: 101,
        instanceName: 'test-vm-1',
        promoCode: 'RENEW20',
        totalCycles: 3
      })
      assert.equal(createdMessages.length, 1)
      assert.equal(createdMessages[0].userId, 10)
      assert.equal(createdMessages[0].eventType, 'promo_binding_exhausted')
      assert.equal(createdMessages[0].title, '【优惠期满】您的实例优惠折扣已结束')
      assert.ok(createdMessages[0].content.includes('test-vm-1'))
      assert.ok(createdMessages[0].content.includes('RENEW20'))
      assert.ok(createdMessages[0].content.includes('3'))
      assert.deepEqual(createdMessages[0].data, {
        instanceId: 101,
        instanceName: 'test-vm-1',
        promoCode: 'RENEW20',
        totalCycles: 3
      })

      // 1.2 sendPromoUnboundByAdminNotification
      await sendPromoUnboundByAdminNotification({
        userId: 10,
        instanceId: 102,
        instanceName: 'test-vm-2',
        promoCode: 'ADMIN50'
      })
      assert.equal(createdMessages.length, 2)
      assert.equal(createdMessages[1].userId, 10)
      assert.equal(createdMessages[1].eventType, 'promo_binding_unbound_by_admin')
      assert.equal(createdMessages[1].title, '【优惠变更】您的实例已解除优惠码绑定')
      assert.ok(createdMessages[1].content.includes('test-vm-2'))
      assert.ok(createdMessages[1].content.includes('ADMIN50'))
      assert.deepEqual(createdMessages[1].data, {
        instanceId: 102,
        instanceName: 'test-vm-2',
        promoCode: 'ADMIN50'
      })

      // 1.3 sendPromoCodeDeletedNotification
      await sendPromoCodeDeletedNotification({
        userId: 10,
        instanceId: 103,
        instanceName: 'test-vm-3',
        promoCode: 'EXPIRED10'
      })
      assert.equal(createdMessages.length, 3)
      assert.equal(createdMessages[2].userId, 10)
      assert.equal(createdMessages[2].eventType, 'promo_code_deleted')
      assert.equal(createdMessages[2].title, '【优惠调整】您使用的优惠码已下线终止')
      assert.ok(createdMessages[2].content.includes('test-vm-3'))
      assert.ok(createdMessages[2].content.includes('EXPIRED10'))
      assert.deepEqual(createdMessages[2].data, {
        instanceId: 103,
        instanceName: 'test-vm-3',
        promoCode: 'EXPIRED10'
      })

      // 1.4 Helper catches errors safely
      ;(prisma as any).inboxMessage = {
        create: async () => {
          throw new Error('Database connection failed')
        }
      }
      await assert.doesNotReject(async () => {
        await sendPromoBindingExhaustedNotification({
          userId: 10,
          instanceId: 101,
          instanceName: 'test-vm-1',
          promoCode: 'RENEW20',
          totalCycles: 3
        })
      })
    }

    // ==================== Suite 2: Admin Auth & Route ID Validation ====================
    console.log('  2. Testing auth & route ID validation...')
    {
      // 2.1 Unauthenticated -> 401
      const resUnauth = await app.inject({
        method: 'GET',
        url: '/api/admin/promos'
      })
      assert.equal(resUnauth.statusCode, 401)

      // 2.2 Non-admin user -> 403
      const resNonAdmin = await app.inject({
        method: 'GET',
        url: '/api/admin/promos',
        headers: userHeaders
      })
      assert.equal(resNonAdmin.statusCode, 403)

      // 2.3 Invalid route IDs
      for (const badId of ['abc', '0', '-5', '3.14', '99999999999999999999999']) {
        const resToggle = await app.inject({
          method: 'PATCH',
          url: `/api/admin/promos/${badId}/toggle`,
          headers: adminHeaders,
          payload: { enabled: false }
        })
        assert.equal(resToggle.statusCode, 400, `Expected 400 for bad ID ${badId}`)

        const resDelete = await app.inject({
          method: 'DELETE',
          url: `/api/admin/promos/${badId}`,
          headers: adminHeaders
        })
        assert.equal(resDelete.statusCode, 400, `Expected 400 for bad ID ${badId}`)

        const resInstances = await app.inject({
          method: 'GET',
          url: `/api/admin/promos/${badId}/instances`,
          headers: adminHeaders
        })
        assert.equal(resInstances.statusCode, 400, `Expected 400 for bad ID ${badId}`)

        const resUnbind = await app.inject({
          method: 'POST',
          url: `/api/admin/promos/${badId}/unbind/10`,
          headers: adminHeaders
        })
        assert.equal(resUnbind.statusCode, 400, `Expected 400 for bad ID ${badId}`)
      }

      const resBadInstanceId = await app.inject({
        method: 'POST',
        url: '/api/admin/promos/1/unbind/invalid',
        headers: adminHeaders
      })
      assert.equal(resBadInstanceId.statusCode, 400)
    }

    // ==================== Suite 3: GET /api/admin/promos ====================
    console.log('  3. Testing GET /api/admin/promos...')
    {
      const mockPromo = {
        id: 1,
        code: 'PROMO20',
        name: '20% OFF Summer',
        type: 'ADMIN_PROMO',
        userId: null,
        adminId: 1,
        isGlobal: true,
        discountType: 'PERCENTAGE',
        discountValue: new Prisma.Decimal(0.2),
        durationType: 'FOREVER',
        durationCycles: null,
        maxTotalUses: 100,
        usedTotalCount: 5,
        maxUsesPerUser: 1,
        startsAt: new Date('2026-06-01T00:00:00.000Z'),
        expiresAt: new Date('2026-12-31T23:59:59.000Z'),
        enabled: true,
        totalDiscountAmount: new Prisma.Decimal(150.5),
        createdAt: new Date('2026-06-01T00:00:00.000Z'),
        scopes: [],
        _count: { bindings: 3 }
      }

      ;(prisma as any).promoCode = {
        count: async (args: any) => 1,
        findMany: async (args: any) => [mockPromo]
      }

      const res = await app.inject({
        method: 'GET',
        url: '/api/admin/promos',
        headers: adminHeaders
      })
      assert.equal(res.statusCode, 200)
      const data = res.json()
      assert.equal(data.total, 1)
      assert.equal(data.page, 1)
      assert.equal(data.pageSize, 20)
      assert.equal(data.totalPages, 1)
      assert.ok(Array.isArray(data.promos || data.items))
      const item = (data.promos || data.items)[0]
      assert.equal(item.code, 'PROMO20')
      assert.equal(item.discountValue, 0.2)
      assert.equal(item.totalDiscountAmount, 150.5)
      assert.equal(item.activeBindingsCount, 3)
      assert.equal(item.isGlobal, true)
    }

    // ==================== Suite 4: POST /api/admin/promos ====================
    console.log('  4. Testing POST /api/admin/promos...')
    {
      // 4.1 Missing code
      const resNoCode = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: { discountValue: 0.1 }
      })
      assert.equal(resNoCode.statusCode, 400)

      // 4.2 Invalid code length (< 3 or > 32)
      const resShortCode = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: { code: 'AB', discountValue: 0.1 }
      })
      assert.equal(resShortCode.statusCode, 400)

      // 4.3 Invalid discountValue (<= 0 or percentage > 1.0)
      const resBadPct = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: { code: 'DISC150', discountType: 'PERCENTAGE', discountValue: 1.5 }
      })
      assert.equal(resBadPct.statusCode, 400)

      // 4.4 Non-global without scopes -> 400
      const resNoScopes = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: { code: 'LOCAL10', isGlobal: false, scopes: [], discountValue: 0.1 }
      })
      assert.equal(resNoScopes.statusCode, 400)

      // 4.5 REPEATING without valid durationCycles -> 400
      const resNoCycles = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: { code: 'REPEAT', isGlobal: true, discountValue: 0.1, durationType: 'REPEATING' }
      })
      assert.equal(resNoCycles.statusCode, 400)

      // 4.6 Duplicate code -> 409
      ;(prisma as any).promoCode = {
        findUnique: async (args: any) => {
          if (args.where.code === 'EXISTING') return { id: 99, code: 'EXISTING' }
          return null
        }
      }
      const resConflict = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: { code: 'existing', isGlobal: true, discountValue: 0.1 }
      })
      assert.equal(resConflict.statusCode, 409)

      // 4.7 Successful creation
      let createdData: any = null
      ;(prisma as any).promoCode = {
        findUnique: async () => null,
        create: async (args: any) => {
          createdData = args.data
          return {
            id: 10,
            ...args.data,
            discountValue: args.data.discountValue,
            totalDiscountAmount: new Prisma.Decimal(0),
            createdAt: new Date(),
            scopes: args.data.scopes?.create || []
          }
        }
      }
      ;(prisma as any).$transaction = async (cb: any) => cb(prisma)

      const resSuccess = await app.inject({
        method: 'POST',
        url: '/api/admin/promos',
        headers: adminHeaders,
        payload: {
          code: 'newcode',
          name: 'New Code',
          discountType: 'FIXED_AMOUNT',
          discountValue: 50,
          isGlobal: false,
          scopes: [{ packageId: 1, packagePlanId: 2 }]
        }
      })
      assert.equal(resSuccess.statusCode, 201)
      assert.equal(createdData.code, 'NEWCODE')
      assert.equal(createdData.durationType, 'ONCE') // FIXED_AMOUNT forces durationType = 'ONCE'
      assert.equal(createdData.isGlobal, false)
    }

    // ==================== Suite 5: PATCH /api/admin/promos/:id/toggle ====================
    console.log('  5. Testing PATCH /api/admin/promos/:id/toggle...')
    {
      // 5.1 Non-boolean body -> 400
      const resBadBody = await app.inject({
        method: 'PATCH',
        url: '/api/admin/promos/1/toggle',
        headers: adminHeaders,
        payload: { enabled: 'yes' }
      })
      assert.equal(resBadBody.statusCode, 400)

      // 5.2 Promo not found -> 404
      ;(prisma as any).promoCode = {
        findUnique: async () => null
      }
      const resNotFound = await app.inject({
        method: 'PATCH',
        url: '/api/admin/promos/999/toggle',
        headers: adminHeaders,
        payload: { enabled: false }
      })
      assert.equal(resNotFound.statusCode, 404)

      // 5.3 Toggle success
      ;(prisma as any).promoCode = {
        findUnique: async (args: any) => ({ id: args.where.id, enabled: true }),
        update: async (args: any) => ({ id: args.where.id, enabled: args.data.enabled })
      }
      const resToggle = await app.inject({
        method: 'PATCH',
        url: '/api/admin/promos/1/toggle',
        headers: adminHeaders,
        payload: { enabled: false }
      })
      assert.equal(resToggle.statusCode, 200)
      assert.equal(resToggle.json().enabled, false)
    }

    // ==================== Suite 6: DELETE /api/admin/promos/:id ====================
    console.log('  6. Testing DELETE /api/admin/promos/:id...')
    {
      // 6.1 Not found -> 404
      ;(prisma as any).promoCode = {
        findUnique: async () => null
      }
      const resNotFound = await app.inject({
        method: 'DELETE',
        url: '/api/admin/promos/999',
        headers: adminHeaders
      })
      assert.equal(resNotFound.statusCode, 404)

      // 6.2 Cascade delete & notification trigger
      const deletedNotifications: any[] = []
      ;(prisma as any).inboxMessage = {
        create: async (args: any) => {
          deletedNotifications.push(args.data)
          return { id: 1 }
        }
      }

      let deletedPromoId = 0
      let deletedBindingsPromoId = 0
      let deletedScopesPromoId = 0

      ;(prisma as any).promoCode = {
        findUnique: async (args: any) => ({ id: args.where.id, code: 'TODELETE' }),
        delete: async (args: any) => {
          deletedPromoId = args.where.id
          return { id: args.where.id }
        }
      }
      ;(prisma as any).instancePromoBinding = {
        findMany: async (args: any) => [
          {
            instanceId: 501,
            instance: { id: 501, name: 'vm-501', userId: 88 }
          }
        ],
        deleteMany: async (args: any) => {
          deletedBindingsPromoId = args.where.promoCodeId
          return { count: 1 }
        }
      }
      ;(prisma as any).promoCodeScope = {
        deleteMany: async (args: any) => {
          deletedScopesPromoId = args.where.promoCodeId
          return { count: 1 }
        }
      }
      ;(prisma as any).$transaction = async (cb: any) => cb(prisma)

      const resDel = await app.inject({
        method: 'DELETE',
        url: '/api/admin/promos/42',
        headers: adminHeaders
      })
      assert.equal(resDel.statusCode, 200)
      assert.equal(resDel.json().affectedInstances, 1)
      assert.equal(deletedPromoId, 42)
      assert.equal(deletedBindingsPromoId, 42)
      assert.equal(deletedScopesPromoId, 42)

      // Wait a tick for async notification
      await new Promise(r => setTimeout(r, 50))
      assert.equal(deletedNotifications.length, 1)
      assert.equal(deletedNotifications[0].eventType, 'promo_code_deleted')
      assert.equal(deletedNotifications[0].userId, 88)
    }

    // ==================== Suite 7: GET /api/admin/promos/:id/instances ====================
    console.log('  7. Testing GET /api/admin/promos/:id/instances...')
    {
      // 7.1 Promo not found -> 404
      ;(prisma as any).promoCode = {
        findUnique: async () => null
      }
      const resNotFound = await app.inject({
        method: 'GET',
        url: '/api/admin/promos/999/instances',
        headers: adminHeaders
      })
      assert.equal(resNotFound.statusCode, 404)

      // 7.2 Returns instances list
      ;(prisma as any).promoCode = {
        findUnique: async () => ({ id: 1, code: 'TEST' })
      }
      ;(prisma as any).instancePromoBinding = {
        count: async () => 1,
        findMany: async () => [
          {
            instanceId: 201,
            promoCodeId: 1,
            totalCycles: 3,
            usedCycles: 1,
            remainingCycles: 2,
            boundAt: new Date('2026-07-01T00:00:00.000Z'),
            instance: {
              id: 201,
              name: 'instance-alpha',
              userId: 55,
              user: { username: 'john_doe' }
            }
          }
        ]
      }

      const resList = await app.inject({
        method: 'GET',
        url: '/api/admin/promos/1/instances',
        headers: adminHeaders
      })
      assert.equal(resList.statusCode, 200)
      const data = resList.json()
      assert.equal(data.total, 1)
      assert.equal(data.page, 1)
      const instance = (data.instances || data.items)[0]
      assert.equal(instance.instanceId, 201)
      assert.equal(instance.instanceName, 'instance-alpha')
      assert.equal(instance.userId, 55)
      assert.equal(instance.username, 'john_doe')
      assert.equal(instance.remainingCycles, 2)
    }

    // ==================== Suite 8: POST /api/admin/promos/:id/unbind/:instanceId ====================
    console.log('  8. Testing POST /api/admin/promos/:id/unbind/:instanceId...')
    {
      // 8.1 Binding not found -> 404
      ;(prisma as any).instancePromoBinding = {
        findFirst: async () => null
      }
      ;(prisma as any).$transaction = async (cb: any) => cb(prisma)

      const resNotFound = await app.inject({
        method: 'POST',
        url: '/api/admin/promos/1/unbind/999',
        headers: adminHeaders
      })
      assert.equal(resNotFound.statusCode, 404)

      // 8.2 Unbind success and notification dispatch
      const unbindNotifications: any[] = []
      ;(prisma as any).inboxMessage = {
        create: async (args: any) => {
          unbindNotifications.push(args.data)
          return { id: 1 }
        }
      }

      let deletedWhere: any = null
      ;(prisma as any).instancePromoBinding = {
        findFirst: async () => ({
          id: 10,
          instanceId: 301,
          promoCodeId: 5,
          instance: { id: 301, name: 'vm-unbind', userId: 77 },
          promoCode: { code: 'UNBINDME' }
        }),
        deleteMany: async (args: any) => {
          deletedWhere = args.where
          return { count: 1 }
        }
      }

      const resUnbind = await app.inject({
        method: 'POST',
        url: '/api/admin/promos/5/unbind/301',
        headers: adminHeaders
      })
      assert.equal(resUnbind.statusCode, 200)
      assert.ok(resUnbind.json().success)
      assert.equal(deletedWhere.instanceId, 301)

      await new Promise(r => setTimeout(r, 50))
      assert.equal(unbindNotifications.length, 1)
      assert.equal(unbindNotifications[0].eventType, 'promo_binding_unbound_by_admin')
      assert.equal(unbindNotifications[0].userId, 77)
      assert.equal(unbindNotifications[0].data.promoCode, 'UNBINDME')
    }

    // ==================== Suite 9: Billing Operations Integration Verification ====================
    console.log('  9. Verifying renewal exhaustion notification integration...')
    {
      const billingOpsSource = readFileSync(resolve(process.cwd(), 'src/db/billing-operations.ts'), 'utf8')
      assert.ok(
        billingOpsSource.includes('sendPromoBindingExhaustedNotification'),
        'billing-operations.ts must import and call sendPromoBindingExhaustedNotification'
      )
      assert.ok(
        billingOpsSource.includes('renewResult.unbound'),
        'billing-operations.ts must check renewResult.unbound before sending exhaustion notification'
      )
    }

    console.log('All Admin Promo Routes & In-Site Notification Tests Passed!')
  } finally {
    // Restore original prisma methods
    ;(prisma as any).promoCode = {
      findMany: origPromoCodeFindMany,
      count: origPromoCodeCount,
      findUnique: origPromoCodeFindUnique,
      create: origPromoCodeCreate,
      update: origPromoCodeUpdate,
      delete: origPromoCodeDelete
    }
    ;(prisma as any).instancePromoBinding = {
      findMany: origBindingFindMany,
      findFirst: origBindingFindFirst,
      count: origBindingCount,
      deleteMany: origBindingDeleteMany
    }
    ;(prisma as any).promoCodeScope = {
      deleteMany: origScopeDeleteMany
    }
    if ((prisma as any).inboxMessage) {
      ;(prisma as any).inboxMessage.create = origInboxMessageCreate
    }
    prisma.$transaction = origTransaction
  }
}

runTests().catch(err => {
  console.error('Test failed:', err)
  process.exit(1)
})
