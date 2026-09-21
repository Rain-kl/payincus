import assert from 'node:assert/strict'
import { Prisma } from '@prisma/client'
import {
  getPromoCodeByCode,
  getPromoCodeById,
  incrementPromoUsageWithLock,
  getInstancePromoBinding,
  bindPromoToInstance,
  updateInstancePromoBindingCycles,
  unbindPromoFromInstance
} from '../src/db/promo-codes.js'
import {
  createPromoRedemptionLog,
  getPromoRedemptionLogsByInstance,
  countUserPromoRedemptions
} from '../src/db/promo-redemptions.js'

console.log('Testing Promo Engine Database Operations & Concurrency Protection...')

// 1. Export checks
{
  assert.equal(typeof getPromoCodeByCode, 'function', 'getPromoCodeByCode must be a function')
  assert.equal(typeof getPromoCodeById, 'function', 'getPromoCodeById must be a function')
  assert.equal(typeof incrementPromoUsageWithLock, 'function', 'incrementPromoUsageWithLock must be a function')
  assert.equal(typeof getInstancePromoBinding, 'function', 'getInstancePromoBinding must be a function')
  assert.equal(typeof bindPromoToInstance, 'function', 'bindPromoToInstance must be a function')
  assert.equal(typeof updateInstancePromoBindingCycles, 'function', 'updateInstancePromoBindingCycles must be a function')
  assert.equal(typeof unbindPromoFromInstance, 'function', 'unbindPromoFromInstance must be a function')
  assert.equal(typeof createPromoRedemptionLog, 'function', 'createPromoRedemptionLog must be a function')
  assert.equal(typeof getPromoRedemptionLogsByInstance, 'function', 'getPromoRedemptionLogsByInstance must be a function')
}

// 2. getPromoCodeByCode & getPromoCodeById
async function testPromoCodeQueries() {
  const mockPromo = {
    id: 1,
    code: 'DISCOUNT50',
    enabled: true,
    discountType: 'PERCENTAGE',
    discountValue: new Prisma.Decimal(0.5),
    scopes: [],
    _count: { bindings: 0, redemptionLogs: 0 }
  }

  let capturedWhere: any = null
  let capturedInclude: any = null
  const mockTx: any = {
    promoCode: {
      findUnique: async ({ where, include }: any) => {
        capturedWhere = where
        capturedInclude = include
        if (where.code === 'DISCOUNT50' || where.id === 1) {
          return mockPromo
        }
        return null
      }
    }
  }

  // by code with whitespace trim
  const resCode = await getPromoCodeByCode('  DISCOUNT50  ', mockTx)
  assert.equal(resCode?.id, 1, 'Should find promo by trimmed code')
  assert.equal(capturedWhere.code, 'DISCOUNT50', 'Code should be trimmed')
  assert.equal(capturedInclude.scopes, true, 'Include should include scopes')
  assert.equal(capturedInclude._count.select.bindings, true, 'Include should count bindings')

  // by id
  const resId = await getPromoCodeById(1, mockTx)
  assert.equal(resId?.code, 'DISCOUNT50', 'Should find promo by id')
  assert.equal(capturedWhere.id, 1, 'Where should match id')

  // not found
  const notFound = await getPromoCodeByCode('NON_EXISTENT', mockTx)
  assert.equal(notFound, null, 'Should return null for non-existent promo')
}

// 3. incrementPromoUsageWithLock and error boundaries
async function testIncrementPromoUsageWithLock() {
  const basePromo = {
    id: 10,
    code: 'LIMITED1',
    enabled: true,
    startsAt: null,
    expiresAt: null,
    maxTotalUses: 1,
    usedTotalCount: 0,
    totalDiscountAmount: new Prisma.Decimal(0)
  }

  // 3.1 Lock acquisition check
  let lockAcquired = false
  const createMockTx = (promoOverride: Partial<typeof basePromo> | null) => {
    return {
      $queryRaw: async () => {
        lockAcquired = true
        return [{ id: basePromo.id }]
      },
      promoCode: {
        findUnique: async () => {
          return promoOverride ? { ...basePromo, ...promoOverride } : null
        },
        update: async ({ data }: any) => {
          return {
            ...basePromo,
            ...promoOverride,
            usedTotalCount: ((promoOverride?.usedTotalCount ?? basePromo.usedTotalCount) + 1),
            totalDiscountAmount: new Prisma.Decimal(25)
          }
        }
      }
    } as any
  }

  // Error boundary 1: PROMO_NOT_FOUND
  {
    lockAcquired = false
    const tx = createMockTx(null)
    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 999),
      (err: Error) => err.message === 'PROMO_NOT_FOUND',
      'Should throw PROMO_NOT_FOUND when record is missing'
    )
    assert.equal(lockAcquired, true, 'Should have attempted row lock')
  }

  // Error boundary 2: PROMO_DISABLED
  {
    const tx = createMockTx({ enabled: false })
    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 10),
      (err: Error) => err.message === 'PROMO_DISABLED',
      'Should throw PROMO_DISABLED when enabled is false'
    )
  }

  // Error boundary 3: PROMO_NOT_STARTED
  {
    const futureDate = new Date(Date.now() + 86400000)
    const tx = createMockTx({ startsAt: futureDate })
    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 10),
      (err: Error) => err.message === 'PROMO_NOT_STARTED',
      'Should throw PROMO_NOT_STARTED when startsAt is in the future'
    )
  }

  // Error boundary 4: PROMO_EXPIRED
  {
    const pastDate = new Date(Date.now() - 1000)
    const tx = createMockTx({ expiresAt: pastDate })
    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 10),
      (err: Error) => err.message === 'PROMO_EXPIRED',
      'Should throw PROMO_EXPIRED when expiresAt is in the past'
    )
  }

  // Error boundary 5: PROMO_QUOTA_EXCEEDED
  {
    const tx = createMockTx({ maxTotalUses: 5, usedTotalCount: 5 })
    await assert.rejects(
      () => incrementPromoUsageWithLock(tx, 10),
      (err: Error) => err.message === 'PROMO_QUOTA_EXCEEDED',
      'Should throw PROMO_QUOTA_EXCEEDED when usedTotalCount >= maxTotalUses'
    )
  }

  // Success: within quota limit
  {
    const tx = createMockTx({ maxTotalUses: 2, usedTotalCount: 1 })
    const updated = await incrementPromoUsageWithLock(tx, 10, 25)
    assert.equal(updated.usedTotalCount, 2, 'Should increment usedTotalCount')
  }

  // Success: unlimited quota (maxTotalUses is null)
  {
    const tx = createMockTx({ maxTotalUses: null, usedTotalCount: 99 })
    const updated = await incrementPromoUsageWithLock(tx, 10, 10)
    assert.equal(updated.usedTotalCount, 100, 'Should increment usedTotalCount when maxTotalUses is null')
  }
}

// 4. InstancePromoBinding operations
async function testInstancePromoBindings() {
  let upsertArg: any = null
  let updateArg: any = null
  let deleteArg: any = null
  let findUniqueArg: any = null

  const mockTx: any = {
    instancePromoBinding: {
      findUnique: async (arg: any) => {
        findUniqueArg = arg
        if (arg.where.instanceId === 101) {
          return {
            id: 1,
            instanceId: 101,
            promoCodeId: 5,
            durationType: 'REPEATING',
            totalCycles: 6,
            usedCycles: 2,
            remainingCycles: 4,
            promoCode: {
              id: 5,
              code: 'PROMO6M',
              scopes: []
            }
          }
        }
        return null
      },
      upsert: async (arg: any) => {
        upsertArg = arg
        return {
          id: 1,
          ...arg.create
        }
      },
      update: async (arg: any) => {
        updateArg = arg
        return {
          id: 1,
          instanceId: arg.where.instanceId,
          usedCycles: 3,
          remainingCycles: arg.data.remainingCycles
        }
      },
      deleteMany: async (arg: any) => {
        deleteArg = arg
        if (arg.where.instanceId === 101) {
          return { count: 1 }
        }
        return { count: 0 }
      }
    }
  }

  // getInstancePromoBinding
  const binding = await getInstancePromoBinding(101, mockTx)
  assert.equal(binding?.instanceId, 101)
  assert.equal(binding?.promoCode.code, 'PROMO6M')
  assert.equal(findUniqueArg.where.instanceId, 101)
  assert.equal(findUniqueArg.include.promoCode.include.scopes, true)

  const emptyBinding = await getInstancePromoBinding(999, mockTx)
  assert.equal(emptyBinding, null)

  // bindPromoToInstance
  const bound = await bindPromoToInstance(mockTx, {
    instanceId: 202,
    promoCodeId: 5,
    durationType: 'REPEATING',
    totalCycles: 6,
    usedCycles: 1,
    remainingCycles: 5
  })
  assert.equal(bound.instanceId, 202)
  assert.equal(upsertArg.where.instanceId, 202)
  assert.equal(upsertArg.create.promoCodeId, 5)
  assert.equal(upsertArg.create.totalCycles, 6)
  assert.equal(upsertArg.create.usedCycles, 1)
  assert.equal(upsertArg.create.remainingCycles, 5)

  // updateInstancePromoBindingCycles
  const updatedBinding = await updateInstancePromoBindingCycles(mockTx, 101, {
    consumedCycles: 1,
    newRemainingCycles: 3
  })
  assert.equal(updatedBinding.remainingCycles, 3)
  assert.equal(updateArg.where.instanceId, 101)
  assert.deepEqual(updateArg.data.usedCycles, { increment: 1 })
  assert.equal(updateArg.data.remainingCycles, 3)

  // unbindPromoFromInstance
  const unbindSuccess = await unbindPromoFromInstance(mockTx, 101)
  assert.equal(unbindSuccess.success, true)
  assert.equal(deleteArg.where.instanceId, 101)

  const unbindFail = await unbindPromoFromInstance(mockTx, 999)
  assert.equal(unbindFail.success, false)
}

// 5. PromoRedemptionLog operations
async function testPromoRedemptionLogs() {
  let createArg: any = null
  let findManyArg: any = null
  let countArg: any = null

  const mockTx: any = {
    promoRedemptionLog: {
      create: async (arg: any) => {
        createArg = arg
        return {
          id: 50,
          ...arg.data,
          createdAt: new Date()
        }
      },
      findMany: async (arg: any) => {
        findManyArg = arg
        return [
          {
            id: 1,
            instanceId: arg.where.instanceId,
            actionType: 'create',
            cycleIndex: 1,
            discountAmount: new Prisma.Decimal(20)
          }
        ]
      },
      count: async (arg: any) => {
        countArg = arg
        if (arg.where.userId === 42 && arg.where.promoCodeId === 10) {
          return 2
        }
        return 0
      }
    }
  }

  // createPromoRedemptionLog
  const created = await createPromoRedemptionLog(mockTx, {
    promoCodeId: 10,
    instanceId: 101,
    userId: 42,
    actionType: 'create',
    cycleIndex: 1,
    months: 1,
    originalPrice: 100,
    discountAmount: 20,
    finalPrice: 80,
    commissionAmount: 5
  })
  assert.equal(created.id, 50)
  assert.equal(createArg.data.promoCodeId, 10)
  assert.equal(createArg.data.instanceId, 101)
  assert.equal(createArg.data.actionType, 'create')
  assert(createArg.data.originalPrice instanceof Prisma.Decimal)
  assert(createArg.data.discountAmount instanceof Prisma.Decimal)
  assert(createArg.data.finalPrice instanceof Prisma.Decimal)
  assert(createArg.data.commissionAmount instanceof Prisma.Decimal)
  assert.equal(Number(createArg.data.originalPrice), 100)
  assert.equal(Number(createArg.data.discountAmount), 20)
  assert.equal(Number(createArg.data.finalPrice), 80)
  assert.equal(Number(createArg.data.commissionAmount), 5)

  // getPromoRedemptionLogsByInstance
  const logs = await getPromoRedemptionLogsByInstance(101, mockTx)
  assert.equal(logs.length, 1)
  assert.equal(logs[0].instanceId, 101)
  assert.equal(findManyArg.where.instanceId, 101)
  assert.equal(findManyArg.orderBy.createdAt, 'desc')

  // countUserPromoRedemptions
  const count = await countUserPromoRedemptions(10, 42, mockTx)
  assert.equal(count, 2)
  assert.equal(countArg.where.promoCodeId, 10)
  assert.equal(countArg.where.userId, 42)
}

async function run() {
  await testPromoCodeQueries()
  await testIncrementPromoUsageWithLock()
  await testInstancePromoBindings()
  await testPromoRedemptionLogs()
  console.log('✅ All promo db operations & concurrency lock tests passed!')
}

run().catch((err) => {
  console.error('❌ Test failed:', err)
  process.exit(1)
})
