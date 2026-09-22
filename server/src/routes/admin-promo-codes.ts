import type { FastifyInstance } from 'fastify'
import { Prisma } from '@prisma/client'
import { prisma } from '../db/prisma.js'
import { unbindPromoFromInstance } from '../db/promo-codes.js'
import {
  sendPromoUnboundByAdminNotification,
  sendPromoCodeDeletedNotification
} from '../services/promo/notifications.js'

const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\d*$/

function parsePositiveRouteId(value: string): number | null {
  if (!POSITIVE_ROUTE_ID_PATTERN.test(value)) return null
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) ? parsed : null
}

function parsePositiveInteger(value: unknown, fallback: number): number {
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : fallback
}

function parseClampedPositiveInteger(value: unknown, fallback: number, max: number): number {
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed) || parsed < 1) return fallback
  return Math.min(parsed, max)
}

interface ListPromosQuery {
  page?: string
  pageSize?: string
  search?: string
  type?: string
  enabled?: string
}

interface CreatePromoBody {
  code?: string
  name?: string | null
  discountType?: 'PERCENTAGE' | 'FIXED_AMOUNT'
  discountValue?: number
  durationType?: 'ONCE' | 'REPEATING' | 'FOREVER'
  durationCycles?: number | null
  isGlobal?: boolean
  scopes?: Array<{ packageId: number; packagePlanId?: number | null }>
  maxTotalUses?: number | null
  maxUsesPerUser?: number | null
  startsAt?: string | null
  expiresAt?: string | null
  enabled?: boolean
}

interface UpdatePromoBody {
  name?: string | null
  maxTotalUses?: number | null
  maxUsesPerUser?: number | null
  startsAt?: string | null
  expiresAt?: string | null
  enabled?: boolean
  code?: string
  discountType?: string
  discountValue?: number
  durationType?: string
  durationCycles?: number | null
  isGlobal?: boolean
  scopes?: Array<{ packageId: number; packagePlanId?: number | null }>
}

interface TogglePromoBody {
  enabled?: boolean
}

export default async function adminPromoCodesRoutes(app: FastifyInstance): Promise<void> {
  app.addHook('onRequest', app.authenticate)
  app.addHook('onRequest', app.requireAdmin)

  // ==================== 1. GET / (and /api/admin/promos) ====================
  app.get<{
    Querystring: ListPromosQuery
  }>('/', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, _reply) => {
    const page = parsePositiveInteger(request.query.page, 1)
    const pageSize = parseClampedPositiveInteger(request.query.pageSize, 20, 100)

    const where: Prisma.PromoCodeWhereInput = {}
    const search = (request.query.search || '').trim()
    if (search) {
      where.OR = [
        { code: { contains: search, mode: 'insensitive' } },
        { name: { contains: search, mode: 'insensitive' } }
      ]
    }

    if (request.query.type === 'AFF_USER' || request.query.type === 'ADMIN_PROMO') {
      where.type = request.query.type
    }

    if (request.query.enabled === 'true') {
      where.enabled = true
    } else if (request.query.enabled === 'false') {
      where.enabled = false
    }

    const [total, promos] = await Promise.all([
      prisma.promoCode.count({ where }),
      prisma.promoCode.findMany({
        where,
        skip: (page - 1) * pageSize,
        take: pageSize,
        orderBy: { createdAt: 'desc' },
        include: {
          scopes: true,
          _count: { select: { bindings: true } }
        }
      })
    ])

    const items = promos.map(promo => ({
      id: promo.id,
      code: promo.code,
      name: promo.name,
      type: promo.type,
      discountType: promo.discountType,
      discountValue: Number(promo.discountValue),
      durationType: promo.durationType,
      durationCycles: promo.durationCycles,
      isGlobal: promo.isGlobal,
      scopes: promo.scopes.map(s => ({
        id: s.id,
        packageId: s.packageId,
        packagePlanId: s.packagePlanId
      })),
      maxTotalUses: promo.maxTotalUses,
      usedTotalCount: promo.usedTotalCount,
      maxUsesPerUser: promo.maxUsesPerUser,
      startsAt: promo.startsAt ? promo.startsAt.toISOString() : null,
      expiresAt: promo.expiresAt ? promo.expiresAt.toISOString() : null,
      enabled: promo.enabled,
      createdAt: promo.createdAt.toISOString(),
      totalDiscountAmount: Number(promo.totalDiscountAmount),
      activeBindingsCount: promo._count?.bindings ?? 0
    }))

    return {
      promos: items,
      items,
      total,
      page,
      pageSize,
      totalPages: Math.ceil(total / pageSize) || 0
    }
  })

  // ==================== 2. POST / (and /api/admin/promos) ====================
  app.post<{
    Body: CreatePromoBody
  }>('/', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, reply) => {
    const body = request.body || {}

    // Code validation
    if (!body.code || typeof body.code !== 'string') {
      return reply.code(400).send({ error: '优惠码不能为空', code: 'INVALID_PARAMS' })
    }
    const code = body.code.trim().toUpperCase()
    if (code.length < 3 || code.length > 32) {
      return reply.code(400).send({ error: '优惠码长度需在 3-32 字符之间', code: 'INVALID_PARAMS' })
    }

    const name = typeof body.name === 'string' && body.name.trim() ? body.name.trim() : null
    const discountType = body.discountType === 'FIXED_AMOUNT' ? 'FIXED_AMOUNT' : 'PERCENTAGE'

    // Discount value validation
    if (typeof body.discountValue !== 'number' || !Number.isFinite(body.discountValue) || body.discountValue <= 0) {
      return reply.code(400).send({ error: '折扣数值必须大于 0', code: 'INVALID_PARAMS' })
    }
    if (discountType === 'PERCENTAGE' && body.discountValue > 1.0) {
      return reply.code(400).send({ error: '百分比折扣数值必须在 0 到 1.0 之间', code: 'INVALID_PARAMS' })
    }

    // Duration type & cycles
    let durationType: 'ONCE' | 'REPEATING' | 'FOREVER'
    if (discountType === 'FIXED_AMOUNT') {
      durationType = 'ONCE'
    } else {
      if (body.durationType === 'ONCE' || body.durationType === 'REPEATING' || body.durationType === 'FOREVER') {
        durationType = body.durationType
      } else {
        durationType = 'FOREVER'
      }
    }

    let durationCycles: number | null = null
    if (durationType === 'REPEATING') {
      const cycles = Number(body.durationCycles)
      if (!Number.isSafeInteger(cycles) || cycles < 1) {
        return reply.code(400).send({ error: '重复周期数必须为大于等于 1 的整数', code: 'INVALID_PARAMS' })
      }
      durationCycles = cycles
    }

    const isGlobal = Boolean(body.isGlobal)
    const validatedScopes: Array<{ packageId: number; packagePlanId: number | null }> = []
    if (!isGlobal) {
      if (!Array.isArray(body.scopes) || body.scopes.length === 0) {
        return reply.code(400).send({ error: '非全局优惠码必须指定至少一个适用范围', code: 'INVALID_PARAMS' })
      }
      for (const scope of body.scopes) {
        const packageId = Number(scope.packageId)
        if (!Number.isSafeInteger(packageId) || packageId <= 0) {
          return reply.code(400).send({ error: '适用范围套餐 ID 无效', code: 'INVALID_PARAMS' })
        }
        const packagePlanId = scope.packagePlanId !== undefined && scope.packagePlanId !== null
          ? Number(scope.packagePlanId)
          : null
        if (packagePlanId !== null && (!Number.isSafeInteger(packagePlanId) || packagePlanId <= 0)) {
          return reply.code(400).send({ error: '适用范围套餐方案 ID 无效', code: 'INVALID_PARAMS' })
        }
        validatedScopes.push({ packageId, packagePlanId })
      }
    }

    let maxTotalUses: number | null = null
    if (body.maxTotalUses !== undefined && body.maxTotalUses !== null) {
      const parsed = Number(body.maxTotalUses)
      if (!Number.isSafeInteger(parsed) || parsed <= 0) {
        return reply.code(400).send({ error: '总使用次数必须为正整数', code: 'INVALID_PARAMS' })
      }
      maxTotalUses = parsed
    }

    let maxUsesPerUser: number | null = 1
    if (body.maxUsesPerUser !== undefined && body.maxUsesPerUser !== null) {
      const parsed = Number(body.maxUsesPerUser)
      if (!Number.isSafeInteger(parsed) || parsed <= 0) {
        return reply.code(400).send({ error: '单用户最大使用次数必须为正整数', code: 'INVALID_PARAMS' })
      }
      maxUsesPerUser = parsed
    }

    let startsAt: Date | null = null
    let expiresAt: Date | null = null
    if (body.startsAt) {
      const d = new Date(body.startsAt)
      if (isNaN(d.getTime())) {
        return reply.code(400).send({ error: '开始时间格式无效', code: 'INVALID_PARAMS' })
      }
      startsAt = d
    }
    if (body.expiresAt) {
      const d = new Date(body.expiresAt)
      if (isNaN(d.getTime())) {
        return reply.code(400).send({ error: '过期时间格式无效', code: 'INVALID_PARAMS' })
      }
      expiresAt = d
    }
    if (startsAt && expiresAt && startsAt >= expiresAt) {
      return reply.code(400).send({ error: '开始时间必须早于过期时间', code: 'INVALID_PARAMS' })
    }

    const enabled = body.enabled !== undefined ? Boolean(body.enabled) : true

    // Check code uniqueness
    const existing = await prisma.promoCode.findUnique({ where: { code } })
    if (existing) {
      return reply.code(409).send({ error: '优惠码已存在', code: 'PROMO_CODE_EXISTS' })
    }

    const adminId = request.user?.id ?? null
    const created = await prisma.$transaction(async (tx) => {
      return tx.promoCode.create({
        data: {
          code,
          name,
          type: 'ADMIN_PROMO',
          adminId,
          isGlobal,
          discountType,
          discountValue: new Prisma.Decimal(body.discountValue!),
          durationType,
          durationCycles,
          maxTotalUses,
          maxUsesPerUser,
          startsAt,
          expiresAt,
          enabled,
          scopes: validatedScopes.length > 0 ? {
            create: validatedScopes.map(s => ({
              packageId: s.packageId,
              packagePlanId: s.packagePlanId
            }))
          } : undefined
        },
        include: {
          scopes: true
        }
      })
    })

    return reply.code(201).send({
      id: created.id,
      code: created.code,
      name: created.name,
      type: created.type,
      discountType: created.discountType,
      discountValue: Number(created.discountValue),
      durationType: created.durationType,
      durationCycles: created.durationCycles,
      isGlobal: created.isGlobal,
      scopes: created.scopes.map(s => ({
        id: s.id,
        packageId: s.packageId,
        packagePlanId: s.packagePlanId
      })),
      maxTotalUses: created.maxTotalUses,
      usedTotalCount: created.usedTotalCount,
      maxUsesPerUser: created.maxUsesPerUser,
      startsAt: created.startsAt ? created.startsAt.toISOString() : null,
      expiresAt: created.expiresAt ? created.expiresAt.toISOString() : null,
      enabled: created.enabled,
      createdAt: created.createdAt.toISOString(),
      totalDiscountAmount: Number(created.totalDiscountAmount)
    })
  })

  // ==================== 3. PUT /:id (Edit Promo Code) ====================
  app.put<{
    Params: { id: string }
    Body: UpdatePromoBody
  }>('/:id', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, reply) => {
    const id = parsePositiveRouteId(request.params.id)
    if (!id) {
      return reply.code(400).send({ error: '无效的优惠码 ID', code: 'INVALID_ID' })
    }

    const existing = await prisma.promoCode.findUnique({
      where: { id },
      include: { scopes: true, _count: { select: { bindings: true } } }
    })
    if (!existing) {
      return reply.code(404).send({ error: '优惠码不存在', code: 'PROMO_NOT_FOUND' })
    }

    const body = request.body || {}

    // 1. 严格校验不可修改字段：折扣百分比/数值、折扣类型、有效周期模式、优惠券码、适用范围
    if (body.code !== undefined && typeof body.code === 'string') {
      const trimmed = body.code.trim().toUpperCase()
      if (trimmed !== existing.code) {
        return reply.code(400).send({ error: '优惠券码创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
      }
    }

    if (body.discountType !== undefined && body.discountType !== existing.discountType) {
      return reply.code(400).send({ error: '折扣类型创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
    }

    if (body.discountValue !== undefined && Number(body.discountValue) !== Number(existing.discountValue)) {
      return reply.code(400).send({ error: '折扣百分比/数值创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
    }

    if (body.durationType !== undefined && body.durationType !== existing.durationType) {
      return reply.code(400).send({ error: '有效周期模式创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
    }

    if (body.durationCycles !== undefined) {
      const newCycles = body.durationCycles === null ? null : Number(body.durationCycles)
      const oldCycles = existing.durationCycles === null ? null : Number(existing.durationCycles)
      if (newCycles !== oldCycles) {
        return reply.code(400).send({ error: '有效周期模式创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
      }
    }

    if (body.isGlobal !== undefined && Boolean(body.isGlobal) !== existing.isGlobal) {
      return reply.code(400).send({ error: '适用范围创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
    }

    if (body.scopes !== undefined) {
      const existingScopesSorted = existing.scopes
        .map(s => `${s.packageId}:${s.packagePlanId ?? ''}`)
        .sort()
      const newScopesSorted = body.scopes
        .map(s => `${s.packageId}:${s.packagePlanId ?? ''}`)
        .sort()
      if (
        existingScopesSorted.length !== newScopesSorted.length ||
        existingScopesSorted.some((val, idx) => val !== newScopesSorted[idx])
      ) {
        return reply.code(400).send({ error: '适用范围创建后不允许修改', code: 'PROMO_FIELD_IMMUTABLE' })
      }
    }

    // 2. 校验与处理允许修改的字段
    const updateData: Prisma.PromoCodeUpdateInput = {}

    if (body.name !== undefined) {
      updateData.name = typeof body.name === 'string' && body.name.trim() ? body.name.trim() : null
    }

    if (body.maxTotalUses !== undefined) {
      if (body.maxTotalUses === null) {
        updateData.maxTotalUses = null
      } else {
        const parsed = Number(body.maxTotalUses)
        if (!Number.isSafeInteger(parsed) || parsed <= 0) {
          return reply.code(400).send({ error: '总使用次数必须为正整数', code: 'INVALID_PARAMS' })
        }
        updateData.maxTotalUses = parsed
      }
    }

    if (body.maxUsesPerUser !== undefined) {
      if (body.maxUsesPerUser === null) {
        updateData.maxUsesPerUser = null
      } else {
        const parsed = Number(body.maxUsesPerUser)
        if (!Number.isSafeInteger(parsed) || parsed <= 0) {
          return reply.code(400).send({ error: '单用户最大使用次数必须为正整数', code: 'INVALID_PARAMS' })
        }
        updateData.maxUsesPerUser = parsed
      }
    }

    let startsAt: Date | null | undefined = undefined
    let expiresAt: Date | null | undefined = undefined

    if (body.startsAt !== undefined) {
      if (body.startsAt === null || body.startsAt === '') {
        startsAt = null
      } else {
        const d = new Date(body.startsAt)
        if (isNaN(d.getTime())) {
          return reply.code(400).send({ error: '开始时间格式无效', code: 'INVALID_PARAMS' })
        }
        startsAt = d
      }
    }

    if (body.expiresAt !== undefined) {
      if (body.expiresAt === null || body.expiresAt === '') {
        expiresAt = null
      } else {
        const d = new Date(body.expiresAt)
        if (isNaN(d.getTime())) {
          return reply.code(400).send({ error: '过期时间格式无效', code: 'INVALID_PARAMS' })
        }
        expiresAt = d
      }
    }

    const effectiveStartsAt = startsAt !== undefined ? startsAt : existing.startsAt
    const effectiveExpiresAt = expiresAt !== undefined ? expiresAt : existing.expiresAt
    if (effectiveStartsAt && effectiveExpiresAt && effectiveStartsAt >= effectiveExpiresAt) {
      return reply.code(400).send({ error: '开始时间必须早于过期时间', code: 'INVALID_PARAMS' })
    }

    if (startsAt !== undefined) updateData.startsAt = startsAt
    if (expiresAt !== undefined) updateData.expiresAt = expiresAt

    if (body.enabled !== undefined) {
      updateData.enabled = Boolean(body.enabled)
    }

    const updated = await prisma.promoCode.update({
      where: { id },
      data: updateData,
      include: {
        scopes: true,
        _count: { select: { bindings: true } }
      }
    })

    return reply.code(200).send({
      id: updated.id,
      code: updated.code,
      name: updated.name,
      type: updated.type,
      discountType: updated.discountType,
      discountValue: Number(updated.discountValue),
      durationType: updated.durationType,
      durationCycles: updated.durationCycles,
      isGlobal: updated.isGlobal,
      scopes: updated.scopes.map(s => ({
        id: s.id,
        packageId: s.packageId,
        packagePlanId: s.packagePlanId
      })),
      maxTotalUses: updated.maxTotalUses,
      usedTotalCount: updated.usedTotalCount,
      maxUsesPerUser: updated.maxUsesPerUser,
      startsAt: updated.startsAt ? updated.startsAt.toISOString() : null,
      expiresAt: updated.expiresAt ? updated.expiresAt.toISOString() : null,
      enabled: updated.enabled,
      createdAt: updated.createdAt.toISOString(),
      totalDiscountAmount: Number(updated.totalDiscountAmount),
      activeBindingsCount: updated._count?.bindings ?? 0
    })
  })

  // ==================== 4. PATCH /:id/toggle ====================
  app.patch<{
    Params: { id: string }
    Body: TogglePromoBody
  }>('/:id/toggle', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, reply) => {
    const id = parsePositiveRouteId(request.params.id)
    if (!id) {
      return reply.code(400).send({ error: '无效的优惠码 ID', code: 'INVALID_ID' })
    }

    if (typeof request.body?.enabled !== 'boolean') {
      return reply.code(400).send({ error: 'enabled 字段必须为布尔值', code: 'INVALID_PARAMS' })
    }

    const existing = await prisma.promoCode.findUnique({ where: { id } })
    if (!existing) {
      return reply.code(404).send({ error: '优惠码不存在', code: 'PROMO_NOT_FOUND' })
    }

    const updated = await prisma.promoCode.update({
      where: { id },
      data: { enabled: request.body.enabled }
    })

    return { success: true, enabled: updated.enabled }
  })

  // ==================== 4. DELETE /:id ====================
  app.delete<{
    Params: { id: string }
  }>('/:id', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, reply) => {
    const id = parsePositiveRouteId(request.params.id)
    if (!id) {
      return reply.code(400).send({ error: '无效的优惠码 ID', code: 'INVALID_ID' })
    }

    const promo = await prisma.promoCode.findUnique({ where: { id } })
    if (!promo) {
      return reply.code(404).send({ error: '优惠码不存在', code: 'PROMO_NOT_FOUND' })
    }

    const activeBindings = await prisma.$transaction(async (tx) => {
      const bindings = await tx.instancePromoBinding.findMany({
        where: { promoCodeId: id },
        include: {
          instance: { select: { id: true, name: true, userId: true } }
        }
      })
      await tx.instancePromoBinding.deleteMany({ where: { promoCodeId: id } })
      await tx.promoCodeScope.deleteMany({ where: { promoCodeId: id } })
      await tx.promoCode.delete({ where: { id } })
      return bindings
    })

    // After transaction commits, notify affected instances
    for (const binding of activeBindings) {
      if (binding.instance) {
        sendPromoCodeDeletedNotification({
          userId: binding.instance.userId,
          instanceId: binding.instance.id,
          instanceName: binding.instance.name,
          promoCode: promo.code
        }).catch(err => console.error('[Promo] Failed to send promo deleted notification:', err))
      }
    }

    return { success: true, affectedInstances: activeBindings.length }
  })

  // ==================== 5. GET /:id/instances ====================
  app.get<{
    Params: { id: string }
    Querystring: { page?: string; pageSize?: string }
  }>('/:id/instances', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, reply) => {
    const id = parsePositiveRouteId(request.params.id)
    if (!id) {
      return reply.code(400).send({ error: '无效的优惠码 ID', code: 'INVALID_ID' })
    }

    const promo = await prisma.promoCode.findUnique({ where: { id } })
    if (!promo) {
      return reply.code(404).send({ error: '优惠码不存在', code: 'PROMO_NOT_FOUND' })
    }

    const page = parsePositiveInteger(request.query.page, 1)
    const pageSize = parseClampedPositiveInteger(request.query.pageSize, 20, 100)

    const [total, bindings] = await Promise.all([
      prisma.instancePromoBinding.count({ where: { promoCodeId: id } }),
      prisma.instancePromoBinding.findMany({
        where: { promoCodeId: id },
        skip: (page - 1) * pageSize,
        take: pageSize,
        orderBy: { boundAt: 'desc' },
        include: {
          instance: {
            select: {
              id: true,
              name: true,
              userId: true,
              user: { select: { username: true } }
            }
          }
        }
      })
    ])

    const instances = bindings.map(b => ({
      instanceId: b.instanceId,
      instanceName: b.instance?.name ?? '',
      userId: b.instance?.userId ?? 0,
      username: b.instance?.user?.username ?? '',
      totalCycles: b.totalCycles,
      usedCycles: b.usedCycles,
      remainingCycles: b.remainingCycles,
      boundAt: b.boundAt.toISOString()
    }))

    return {
      instances,
      items: instances,
      total,
      page,
      pageSize,
      totalPages: Math.ceil(total / pageSize) || 0
    }
  })

  // ==================== 6. POST /:id/unbind/:instanceId ====================
  app.post<{
    Params: { id: string; instanceId: string }
  }>('/:id/unbind/:instanceId', {
    onRequest: [app.authenticate, app.requireAdmin]
  }, async (request, reply) => {
    const id = parsePositiveRouteId(request.params.id)
    const instanceId = parsePositiveRouteId(request.params.instanceId)
    if (!id || !instanceId) {
      return reply.code(400).send({ error: '无效的路由参数 ID', code: 'INVALID_ID' })
    }

    const binding = await prisma.$transaction(async (tx) => {
      const target = await tx.instancePromoBinding.findFirst({
        where: { promoCodeId: id, instanceId },
        include: {
          instance: { select: { id: true, name: true, userId: true } },
          promoCode: { select: { code: true } }
        }
      })
      if (!target) {
        return null
      }
      await unbindPromoFromInstance(tx, instanceId)
      return target
    })

    if (!binding) {
      return reply.code(404).send({ error: '未找到该实例的优惠码绑定', code: 'BINDING_NOT_FOUND' })
    }

    if (binding.instance) {
      sendPromoUnboundByAdminNotification({
        userId: binding.instance.userId,
        instanceId: binding.instance.id,
        instanceName: binding.instance.name,
        promoCode: binding.promoCode.code
      }).catch(err => console.error('[Promo] Failed to send admin unbind notification:', err))
    }

    return { success: true, message: '实例已解绑，已发送站内信通知' }
  })
}
