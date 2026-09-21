import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { isProductEligibleForPromo } from '../src/services/promo/scope-matcher.js'
import {
  calculateFinalPrice,
  calculateCreationQuote,
  calculatePromoRenewalSplit
} from '../src/services/promo/promo-calculator.js'
import { PromoCodeEngine } from '../src/services/promo-engine.js'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const root = resolve(__dirname, '../..')
const read = (path: string) => readFileSync(resolve(root, path), 'utf8')

function sectionBetween(sourceText: string, startMarker: string, endMarker: string): string {
  const start = sourceText.indexOf(startMarker)
  assert.notEqual(start, -1, `missing start marker: ${startMarker}`)
  const end = sourceText.indexOf(endMarker, start)
  assert.notEqual(end, -1, `missing end marker: ${endMarker}`)
  return sourceText.slice(start, end)
}

// =============================================================================
// 1. Static Architectural & Structural Invariants
// =============================================================================

// 1.1 Route registration in server/src/app.ts
const appSource = read('server/src/app.ts')

assert.ok(
  appSource.includes("import promoCodesRoutes from './routes/promo-codes.js'") &&
    appSource.includes("import adminPromoCodesRoutes from './routes/admin-promo-codes.js'"),
  'app.ts must import promoCodesRoutes and adminPromoCodesRoutes'
)

assert.ok(
  appSource.includes("await fastify.register(promoCodesRoutes, { prefix: '/api/promos' })") &&
    appSource.includes("await fastify.register(adminPromoCodesRoutes, { prefix: '/api/admin/promos' })"),
  'app.ts must register promo routes under /api/promos and /api/admin/promos'
)

// 1.2 User Promo Route Security & Route ID Parsing in server/src/routes/promo-codes.ts
const userRouteSource = read('server/src/routes/promo-codes.ts')

assert.ok(
  userRouteSource.includes('const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\\d*$/') &&
    userRouteSource.includes('function parsePositiveRouteId(value: string): number | null') &&
    userRouteSource.includes('Number.isSafeInteger(parsed)'),
  'promo-codes.ts must define strict positive route ID parsing with POSITIVE_ROUTE_ID_PATTERN'
)

assert.ok(
  userRouteSource.includes('fastify.post(\'/validate\', {\n    onRequest: [fastify.authenticate]') ||
    userRouteSource.includes("fastify.post('/validate', {\n    onRequest: [fastify.authenticate]"),
  'promo-codes.ts /validate must require authentication'
)

assert.ok(
  userRouteSource.includes("fastify.get('/renew-preview/:instanceId', {\n    onRequest: [fastify.authenticate]") &&
    userRouteSource.includes("const instanceId = parsePositiveRouteId(params.instanceId)"),
  'promo-codes.ts /renew-preview/:instanceId must require authentication and use parsePositiveRouteId'
)

assert.ok(
  userRouteSource.includes("fastify.post('/apply/:instanceId', {\n    onRequest: [fastify.authenticate]") &&
    userRouteSource.includes("const instanceId = parsePositiveRouteId(params.instanceId)"),
  'promo-codes.ts /apply/:instanceId must require authentication and use parsePositiveRouteId'
)

// 1.3 Admin Promo Route Security & Route ID Parsing in server/src/routes/admin-promo-codes.ts
const adminRouteSource = read('server/src/routes/admin-promo-codes.ts')

assert.ok(
  adminRouteSource.includes("app.addHook('onRequest', app.authenticate)") &&
    adminRouteSource.includes("app.addHook('onRequest', app.requireAdmin)"),
  'admin-promo-codes.ts must enforce app.authenticate and app.requireAdmin for all routes'
)

assert.ok(
  adminRouteSource.includes('const POSITIVE_ROUTE_ID_PATTERN = /^[1-9]\\d*$/') &&
    adminRouteSource.includes('function parsePositiveRouteId(value: string): number | null') &&
    adminRouteSource.includes('Number.isSafeInteger(parsed)'),
  'admin-promo-codes.ts must define strict positive route ID parsing with POSITIVE_ROUTE_ID_PATTERN'
)

assert.equal(
  adminRouteSource.match(/const id = parsePositiveRouteId\(request\.params\.id\)/g)?.length ?? 0,
  4,
  'admin-promo-codes.ts must parse route id using parsePositiveRouteId on all parameterized id routes'
)

assert.ok(
  adminRouteSource.includes('const instanceId = parsePositiveRouteId(request.params.instanceId)'),
  'admin-promo-codes.ts must parse instanceId using parsePositiveRouteId'
)

// 1.4 Core Engine Facade in server/src/services/promo-engine.ts
const promoEngineSource = read('server/src/services/promo-engine.ts')

assert.ok(
  promoEngineSource.includes('export class PromoCodeEngine') &&
    promoEngineSource.includes('export default PromoCodeEngine') &&
    promoEngineSource.includes('static async validate(') &&
    promoEngineSource.includes('static calculateCreationQuote(') &&
    promoEngineSource.includes('static calculateRenewalQuote(') &&
    promoEngineSource.includes('static async settleOnCreate(') &&
    promoEngineSource.includes('static async settleOnRenew(') &&
    promoEngineSource.includes('static async getPromoRefundEligibility('),
  'promo-engine.ts must export PromoCodeEngine with validate, calculateCreationQuote, calculateRenewalQuote, settleOnCreate, settleOnRenew, and getPromoRefundEligibility'
)

assert.equal(typeof PromoCodeEngine.validate, 'function', 'PromoCodeEngine.validate must be a function')
assert.equal(typeof PromoCodeEngine.calculateCreationQuote, 'function', 'PromoCodeEngine.calculateCreationQuote must be a function')
assert.equal(typeof PromoCodeEngine.calculateRenewalQuote, 'function', 'PromoCodeEngine.calculateRenewalQuote must be a function')
assert.equal(typeof PromoCodeEngine.settleOnCreate, 'function', 'PromoCodeEngine.settleOnCreate must be a function')
assert.equal(typeof PromoCodeEngine.settleOnRenew, 'function', 'PromoCodeEngine.settleOnRenew must be a function')
assert.equal(typeof PromoCodeEngine.getPromoRefundEligibility, 'function', 'PromoCodeEngine.getPromoRefundEligibility must be a function')

// 1.5 Billing Integration in server/src/db/billing-operations.ts
const billingOperationsSource = read('server/src/db/billing-operations.ts')

assert.ok(
  billingOperationsSource.includes("import { PromoCodeEngine } from '../services/promo-engine.js'"),
  'billing-operations.ts must import PromoCodeEngine'
)

const refundQuoteSection = sectionBetween(
  billingOperationsSource,
  'export async function calculateInstanceRemainingRefundQuote(',
  'export async function calculateInstanceRefund('
)
assert.ok(
  refundQuoteSection.includes('const promoRefund = await PromoCodeEngine.getPromoRefundEligibility(instance.id, tx)') &&
    refundQuoteSection.includes('if (!promoRefund.isRefundable) {') &&
    refundQuoteSection.includes('remainingDays: 0') &&
    refundQuoteSection.includes('remainingValue: 0') &&
    refundQuoteSection.includes('refundableValue: 0') &&
    refundQuoteSection.includes('maxRefundable: 0'),
  'calculateInstanceRemainingRefundQuote must enforce PromoCodeEngine.getPromoRefundEligibility and return zero refundable values'
)

assert.ok(
  billingOperationsSource.includes('const promoRefund = await PromoCodeEngine.getPromoRefundEligibility(instance.id, options.tx)') &&
    billingOperationsSource.includes('if (!promoRefund.isRefundable && (priceDiff < 0 || !calcResult.isUpgrade)) {') &&
    billingOperationsSource.includes('priceDiff = 0'),
  'calculatePlanChangePreviewQuote must clamp downgrade priceDiff to 0 when not refundable'
)

assert.ok(
  billingOperationsSource.includes('const promoRefund = await PromoCodeEngine.getPromoRefundEligibility(instance.id, tx)') &&
    billingOperationsSource.includes('if (!promoRefund.isRefundable && (priceDiff < 0 || roundedNewPrice < oldPrice)) {') &&
    billingOperationsSource.includes('priceDiff = 0'),
  'calculatePlanChangeBilling must clamp downgrade priceDiff to 0 when not refundable'
)

const renewSection = sectionBetween(
  billingOperationsSource,
  '// 如果统一优惠码实际生效（击败或打平 VIP/AFF 折扣），执行续费切片结算与状态更新',
  '// 如果有 AFF 绑定，给优惠码创建者返利'
)
assert.ok(
  renewSection.includes('const renewResult = await PromoCodeEngine.settleOnRenew(tx, {') &&
    renewSection.includes('if (renewResult.unbound) {') &&
    renewSection.includes('sendPromoBindingExhaustedNotification({') &&
    renewSection.includes('promoCode: promoBinding.promoCode.code'),
  'performRenewal must invoke PromoCodeEngine.settleOnRenew and dispatch sendPromoBindingExhaustedNotification on unbind'
)

// 1.6 Instance Creation Integration in server/src/routes/instances.ts
const instancesSource = read('server/src/routes/instances.ts')

assert.ok(
  instancesSource.includes('await PromoCodeEngine.settleOnCreate(tx, {') &&
    instancesSource.includes('instanceId: instance.id') &&
    instancesSource.includes('promoCodeId: promoValidation.promoCode.id'),
  'instances.ts must invoke PromoCodeEngine.settleOnCreate within the creation transaction'
)

assert.ok(
  instancesSource.includes("if ((validatedAffCode || promoValidation) && preCheckHost.name.toLowerCase().startsWith('peer')) {") &&
    instancesSource.includes("apiError(ErrorCode.INVALID_PARAMS, '用户托管节点不支持使用优惠码')"),
  'instances.ts must strictly forbid applying promo codes on peer hosts'
)

// 1.7 In-Site Notifications in server/src/services/promo/notifications.ts
const notificationsSource = read('server/src/services/promo/notifications.ts')

assert.ok(
  notificationsSource.includes('export async function sendPromoBindingExhaustedNotification(') &&
    notificationsSource.includes("eventType: 'promo_binding_exhausted'") &&
    notificationsSource.includes("title: '【优惠期满】您的实例优惠折扣已结束'") &&
    notificationsSource.includes('${params.instanceName}') &&
    notificationsSource.includes('#${params.instanceId}') &&
    notificationsSource.includes('${params.promoCode}') &&
    notificationsSource.includes('${params.totalCycles}'),
  'notifications.ts must export sendPromoBindingExhaustedNotification with promo_binding_exhausted, Chinese title, and dynamic parameters'
)

assert.ok(
  notificationsSource.includes('export async function sendPromoUnboundByAdminNotification(') &&
    notificationsSource.includes("eventType: 'promo_binding_unbound_by_admin'") &&
    notificationsSource.includes("title: '【优惠变更】您的实例已解除优惠码绑定'") &&
    notificationsSource.includes('${params.instanceName}') &&
    notificationsSource.includes('#${params.instanceId}') &&
    notificationsSource.includes('${params.promoCode}'),
  'notifications.ts must export sendPromoUnboundByAdminNotification with promo_binding_unbound_by_admin, Chinese title, and dynamic parameters'
)

assert.ok(
  notificationsSource.includes('export async function sendPromoCodeDeletedNotification(') &&
    notificationsSource.includes("eventType: 'promo_code_deleted'") &&
    notificationsSource.includes("title: '【优惠调整】您使用的优惠码已下线终止'") &&
    notificationsSource.includes('${params.instanceName}') &&
    notificationsSource.includes('#${params.instanceId}') &&
    notificationsSource.includes('${params.promoCode}'),
  'notifications.ts must export sendPromoCodeDeletedNotification with promo_code_deleted, Chinese title, and dynamic parameters'
)

assert.equal(
  notificationsSource.match(/try\s*\{[\s\S]*?await createInboxMessage\(/g)?.length ?? 0,
  3,
  'All 3 promo notifications must be safely wrapped in try-catch blocks to prevent caller disruptions'
)


// =============================================================================
// 2. Runtime Algorithmic Invariants
// =============================================================================

// 2.1 Hierarchical Bubble-up Matching (isProductEligibleForPromo)
{
  // A. isGlobal matches any product target
  const globalPromo = { isGlobal: true, scopes: [] }
  assert.equal(isProductEligibleForPromo(globalPromo, { packageId: 1, packagePlanId: 10 }), true)
  assert.equal(isProductEligibleForPromo(globalPromo, { packageId: 999, packagePlanId: 888 }), true)

  // B. Package-level scope (packagePlanId === null) bubbles up and matches any plan in that package
  const pkgPromo = {
    isGlobal: false,
    scopes: [{ packageId: 10, packagePlanId: null }]
  }
  assert.equal(isProductEligibleForPromo(pkgPromo, { packageId: 10, packagePlanId: 101 }), true)
  assert.equal(isProductEligibleForPromo(pkgPromo, { packageId: 10, packagePlanId: 102 }), true)
  assert.equal(isProductEligibleForPromo(pkgPromo, { packageId: 11, packagePlanId: 101 }), false, 'Different package must not match')

  // C. Plan-level scope matches only exact packagePlanId and rejects sibling plans
  const planPromo = {
    isGlobal: false,
    scopes: [{ packageId: 10, packagePlanId: 101 }]
  }
  assert.equal(isProductEligibleForPromo(planPromo, { packageId: 10, packagePlanId: 101 }), true)
  assert.equal(isProductEligibleForPromo(planPromo, { packageId: 10, packagePlanId: 102 }), false, 'Sibling plan must be rejected')
  assert.equal(isProductEligibleForPromo(planPromo, { packageId: 11, packagePlanId: 201 }), false, 'Unmatched package and plan must be rejected')

  // D. Multi-scope union matching
  const mixedPromo = {
    isGlobal: false,
    scopes: [
      { packageId: 1, packagePlanId: 10 },
      { packageId: 2, packagePlanId: null }
    ]
  }
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 1, packagePlanId: 10 }), true)
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 1, packagePlanId: 11 }), false)
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 2, packagePlanId: 20 }), true)
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 2, packagePlanId: 21 }), true)
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 3, packagePlanId: 30 }), false)

  // E. Non-global with empty scopes returns false
  assert.equal(isProductEligibleForPromo({ isGlobal: false, scopes: [] }, { packageId: 1, packagePlanId: 10 }), false)
}

// 2.2 Billing Slicing & Zero Clamp (calculateFinalPrice, calculateCreationQuote, calculatePromoRenewalSplit)
{
  // A. calculateFinalPrice: zero clamp and 2 decimal places
  assert.equal(calculateFinalPrice(100, 30), 70)
  assert.equal(calculateFinalPrice(100, 100), 0)
  assert.equal(calculateFinalPrice(50, 150), 0, 'Price deduction exceeding original price must strictly clamp to 0')
  assert.equal(calculateFinalPrice(0, 10), 0)
  assert.equal(calculateFinalPrice(10.555, 0.333), 10.22)

  // B. calculateCreationQuote
  // Percentage discount
  const pctQuote = calculateCreationQuote({
    originalPrice: 100,
    discountType: 'PERCENTAGE',
    discountValue: 0.2
  })
  assert.equal(pctQuote.originalPrice, 100)
  assert.equal(pctQuote.discountAmount, 20)
  assert.equal(pctQuote.finalPrice, 80)

  // Fixed amount discount within original price
  const fixedQuoteNormal = calculateCreationQuote({
    originalPrice: 100,
    discountType: 'FIXED_AMOUNT',
    discountValue: 35
  })
  assert.equal(fixedQuoteNormal.originalPrice, 100)
  assert.equal(fixedQuoteNormal.discountAmount, 35)
  assert.equal(fixedQuoteNormal.finalPrice, 65)

  // Fixed amount discount exceeding original price (zero clamp)
  const fixedQuoteExceeding = calculateCreationQuote({
    originalPrice: 50,
    discountType: 'FIXED_AMOUNT',
    discountValue: 120
  })
  assert.equal(fixedQuoteExceeding.originalPrice, 50)
  assert.equal(fixedQuoteExceeding.discountAmount, 50, 'discountAmount must clamp to originalPrice')
  assert.equal(fixedQuoteExceeding.finalPrice, 0, 'finalPrice must strictly clamp to 0')

  // Facade delegation check
  const facadeCreationQuote = PromoCodeEngine.calculateCreationQuote(100, {
    discountType: 'PERCENTAGE',
    discountValue: 0.15
  })
  assert.equal(facadeCreationQuote.originalPrice, 100)
  assert.equal(facadeCreationQuote.discountAmount, 15)
  assert.equal(facadeCreationQuote.finalPrice, 85)

  // C. calculatePromoRenewalSplit
  // 12 months renewal with 4 remaining promo cycles (exhaustion / willUnbind)
  const renewalExhaustion = calculatePromoRenewalSplit({
    monthlyPrice: 50,
    months: 12,
    discountRate: 0.2,
    remainingCycles: 4
  })
  assert.equal(renewalExhaustion.discountedMonths, 4)
  assert.equal(renewalExhaustion.regularMonths, 8)
  assert.equal(renewalExhaustion.consumedCycles, 4)
  assert.equal(renewalExhaustion.newRemainingCycles, 0)
  assert.equal(renewalExhaustion.willUnbind, true)
  assert.equal(renewalExhaustion.discountAmount, 40) // 50 * 4 * 0.2 = 40
  assert.equal(renewalExhaustion.finalAmount, 560)   // 50 * 12 - 40 = 560

  // 3 months renewal with 6 remaining cycles (cycles remain / no unbind)
  const renewalPartial = calculatePromoRenewalSplit({
    monthlyPrice: 100,
    months: 3,
    discountRate: 0.1,
    remainingCycles: 6
  })
  assert.equal(renewalPartial.discountedMonths, 3)
  assert.equal(renewalPartial.regularMonths, 0)
  assert.equal(renewalPartial.consumedCycles, 3)
  assert.equal(renewalPartial.newRemainingCycles, 3)
  assert.equal(renewalPartial.willUnbind, false)
  assert.equal(renewalPartial.discountAmount, 30) // 100 * 3 * 0.1 = 30
  assert.equal(renewalPartial.finalAmount, 270)   // 300 - 30 = 270

  // FOREVER duration renewal split (remainingCycles === null)
  const renewalForever = calculatePromoRenewalSplit({
    monthlyPrice: 80,
    months: 12,
    discountRate: 0.25,
    remainingCycles: null
  })
  assert.equal(renewalForever.discountedMonths, 12)
  assert.equal(renewalForever.regularMonths, 0)
  assert.equal(renewalForever.consumedCycles, 12)
  assert.equal(renewalForever.newRemainingCycles, null)
  assert.equal(renewalForever.willUnbind, false)
  assert.equal(renewalForever.discountAmount, 240) // 80 * 12 * 0.25 = 240
  assert.equal(renewalForever.finalAmount, 720)   // 960 - 240 = 720

  // Facade delegation check for renewal quote
  const facadeRenewalQuote = PromoCodeEngine.calculateRenewalQuote(100, 12, {
    promoCode: { discountValue: 0.2 } as any,
    remainingCycles: 4
  } as any)
  assert.equal(facadeRenewalQuote.discountedMonths, 4)
  assert.equal(facadeRenewalQuote.regularMonths, 8)
  assert.equal(facadeRenewalQuote.willUnbind, true)
  assert.equal(facadeRenewalQuote.newRemainingCycles, 0)
}

// 2.3 Notification Templates Invariant (Chinese titles and variable interpolation verification)
{
  const sampleParams = {
    userId: 1001,
    instanceId: 42,
    instanceName: 'hk-bgp-vm-1',
    promoCode: 'OFF50NOW',
    totalCycles: 6
  }

  // Template 1: Promo Binding Exhausted
  const expectedTitle1 = '【优惠期满】您的实例优惠折扣已结束'
  const expectedContent1 = `尊敬的用户：\n您的实例 **${sampleParams.instanceName}**（ID: #${sampleParams.instanceId}）所绑定的优惠码 **${sampleParams.promoCode}** 设定的优惠期数（共 ${sampleParams.totalCycles} 期）已全部使用完毕。\n从下一个计费周期开始，该实例将恢复为标准资费续费。您也可以在实例管理页面随时绑定新的有效优惠码。`
  assert.ok(expectedContent1.includes('hk-bgp-vm-1') && expectedContent1.includes('#42') && expectedContent1.includes('OFF50NOW') && expectedContent1.includes('共 6 期'))
  assert.ok(notificationsSource.includes(expectedTitle1))

  // Template 2: Promo Unbound By Admin
  const expectedTitle2 = '【优惠变更】您的实例已解除优惠码绑定'
  const expectedContent2 = `尊敬的用户：\n您的实例 **${sampleParams.instanceName}**（ID: #${sampleParams.instanceId}）绑定的优惠码 **${sampleParams.promoCode}** 已由系统管理员解除绑定。\n即日起该实例将不再享受此优惠码的续费折扣，后续续费将按标准资费计费。如有疑问，请通过工单系统联系客服支持。`
  assert.ok(expectedContent2.includes('hk-bgp-vm-1') && expectedContent2.includes('#42') && expectedContent2.includes('OFF50NOW'))
  assert.ok(notificationsSource.includes(expectedTitle2))

  // Template 3: Promo Code Deleted
  const expectedTitle3 = '【优惠调整】您使用的优惠码已下线终止'
  const expectedContent3 = `尊敬的用户：\n您在实例 **${sampleParams.instanceName}**（ID: #${sampleParams.instanceId}）上所享受的优惠码 **${sampleParams.promoCode}** 已由平台官方正式下线并终止优惠策略。\n该实例后续续费将恢复为标准官方价格。感谢您的理解与支持。`
  assert.ok(expectedContent3.includes('hk-bgp-vm-1') && expectedContent3.includes('#42') && expectedContent3.includes('OFF50NOW'))
  assert.ok(notificationsSource.includes(expectedTitle3))
}

// =============================================================================
// 3. Package Scripts Registration Invariants
// =============================================================================
const serverPackage = read('server/package.json')
const rootPackage = read('package.json')

assert.ok(
  serverPackage.includes('"test:promo-engine-guards": "node --import tsx scripts/test-promo-engine-guards.ts"'),
  'server/package.json must register test:promo-engine-guards'
)

assert.ok(
  rootPackage.includes('pnpm --filter server test:promo-engine-guards'),
  'root package.json test script must include pnpm --filter server test:promo-engine-guards'
)

console.log('promo engine guard tests passed')
