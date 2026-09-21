import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import * as PrismaClientPkg from '@prisma/client'

const __dirname = dirname(fileURLToPath(import.meta.url))
const root = resolve(__dirname, '../..')
const schemaPath = resolve(root, 'server/prisma/schema.prisma')
const schema = readFileSync(schemaPath, 'utf8')

console.log('Testing Promo Engine Prisma Schema & Model Definitions...')

// 1. Enums definition checks
assert(
  schema.includes('enum PromoCodeType {\n  AFF_USER\n  ADMIN_PROMO\n}'),
  'schema must define PromoCodeType enum with AFF_USER and ADMIN_PROMO'
)

assert(
  schema.includes('enum PromoDiscountType {\n  PERCENTAGE\n  FIXED_AMOUNT\n}'),
  'schema must define PromoDiscountType enum with PERCENTAGE and FIXED_AMOUNT'
)

assert(
  schema.includes('enum PromoDurationType {\n  ONCE\n  REPEATING\n  FOREVER\n}'),
  'schema must define PromoDurationType enum with ONCE, REPEATING, FOREVER'
)

// 2. Models definition checks
assert(schema.includes('model PromoCode {'), 'schema must define PromoCode model')
assert(schema.includes('@@map("promo_codes")'), 'PromoCode model must map to promo_codes table')

assert(schema.includes('model PromoCodeScope {'), 'schema must define PromoCodeScope model')
assert(schema.includes('@@map("promo_code_scopes")'), 'PromoCodeScope model must map to promo_code_scopes table')

assert(schema.includes('model InstancePromoBinding {'), 'schema must define InstancePromoBinding model')
assert(schema.includes('@@map("instance_promo_bindings")'), 'InstancePromoBinding model must map to instance_promo_bindings table')

assert(schema.includes('model PromoRedemptionLog {'), 'schema must define PromoRedemptionLog model')
assert(schema.includes('@@map("promo_redemption_logs")'), 'PromoRedemptionLog model must map to promo_redemption_logs table')

// 3. Reverse relations in User, Instance, Package, PackagePlan
assert(
  /model User \{[\s\S]*?\bpromoCodes\s+PromoCode\[\][\s\S]*?\bpromoRedemptionLogs\s+PromoRedemptionLog\[\][\s\S]*?\}/.test(schema),
  'User model must define promoCodes and promoRedemptionLogs reverse relations'
)

assert(
  /model Instance \{[\s\S]*?\bpromoBinding\s+InstancePromoBinding\?[\s\S]*?\bpromoRedemptionLogs\s+PromoRedemptionLog\[\][\s\S]*?\}/.test(schema),
  'Instance model must define promoBinding and promoRedemptionLogs reverse relations'
)

assert(
  /model Package \{[\s\S]*?\bpromoCodeScopes\s+PromoCodeScope\[\][\s\S]*?\}/.test(schema),
  'Package model must define promoCodeScopes reverse relation'
)

assert(
  /model PackagePlan \{[\s\S]*?\bpromoCodeScopes\s+PromoCodeScope\[\][\s\S]*?\}/.test(schema),
  'PackagePlan model must define promoCodeScopes reverse relation'
)

// 4. Runtime Prisma Client checks (generated client must export enums and model names)
const { PromoCodeType, PromoDiscountType, PromoDurationType, Prisma } = PrismaClientPkg as any

assert(PromoCodeType && PromoCodeType.AFF_USER === 'AFF_USER' && PromoCodeType.ADMIN_PROMO === 'ADMIN_PROMO', 'Prisma client must export PromoCodeType enum')
assert(PromoDiscountType && PromoDiscountType.PERCENTAGE === 'PERCENTAGE' && PromoDiscountType.FIXED_AMOUNT === 'FIXED_AMOUNT', 'Prisma client must export PromoDiscountType enum')
assert(PromoDurationType && PromoDurationType.FOREVER === 'FOREVER', 'Prisma client must export PromoDurationType enum')

assert(Prisma?.ModelName?.PromoCode === 'PromoCode', 'Prisma client ModelName must include PromoCode')
assert(Prisma?.ModelName?.PromoCodeScope === 'PromoCodeScope', 'Prisma client ModelName must include PromoCodeScope')
assert(Prisma?.ModelName?.InstancePromoBinding === 'InstancePromoBinding', 'Prisma client ModelName must include InstancePromoBinding')
assert(Prisma?.ModelName?.PromoRedemptionLog === 'PromoRedemptionLog', 'Prisma client ModelName must include PromoRedemptionLog')

console.log('✅ All promo schema checks passed!')
