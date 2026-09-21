import assert from 'node:assert/strict'
import { isProductEligibleForPromo } from '../src/services/promo/scope-matcher.js'
import type { PromoCodeWithScopes, PromoTarget } from '../src/services/promo/types.js'

console.log('Testing Promo Hierarchical Bubble-up Scope Matcher...')

// 1. Global Promo (isGlobal = true)
{
  const globalPromo: PromoCodeWithScopes = {
    isGlobal: true,
    scopes: []
  }
  const targetA: PromoTarget = { packageId: 1, packagePlanId: 10 }
  const targetB: PromoTarget = { packageId: 99, packagePlanId: 999 }

  assert.equal(isProductEligibleForPromo(globalPromo, targetA), true, 'Global promo should match any target A')
  assert.equal(isProductEligibleForPromo(globalPromo, targetB), true, 'Global promo should match any target B')

  // Global promo even if scopes has elements
  const globalPromoWithScopes: PromoCodeWithScopes = {
    isGlobal: true,
    scopes: [{ packageId: 1, packagePlanId: null }]
  }
  assert.equal(isProductEligibleForPromo(globalPromoWithScopes, targetB), true, 'Global promo should match even with non-matching scopes')
}

// 2. Leaf (Plan) exact matching
{
  const planPromo: PromoCodeWithScopes = {
    isGlobal: false,
    scopes: [
      { packageId: 1, packagePlanId: 101 }
    ]
  }

  const matchingTarget: PromoTarget = { packageId: 1, packagePlanId: 101 }
  const nonMatchingTargetPlan: PromoTarget = { packageId: 1, packagePlanId: 102 }
  const nonMatchingTargetPackage: PromoTarget = { packageId: 2, packagePlanId: 201 }

  assert.equal(isProductEligibleForPromo(planPromo, matchingTarget), true, 'Should match exact packagePlanId')
  assert.equal(isProductEligibleForPromo(planPromo, nonMatchingTargetPlan), false, 'Should not match different plan in same package')
  assert.equal(isProductEligibleForPromo(planPromo, nonMatchingTargetPackage), false, 'Should not match different package')
}

// 3. Parent (Package) bubble-up matching
{
  const packagePromo: PromoCodeWithScopes = {
    isGlobal: false,
    scopes: [
      { packageId: 10, packagePlanId: null }
    ]
  }

  const planATarget: PromoTarget = { packageId: 10, packagePlanId: 501 }
  const planBTarget: PromoTarget = { packageId: 10, packagePlanId: 502 }
  const otherPackageTarget: PromoTarget = { packageId: 20, packagePlanId: 501 }

  assert.equal(isProductEligibleForPromo(packagePromo, planATarget), true, 'Should bubble up match target under package 10 (plan A)')
  assert.equal(isProductEligibleForPromo(packagePromo, planBTarget), true, 'Should bubble up match target under package 10 (plan B)')
  assert.equal(isProductEligibleForPromo(packagePromo, otherPackageTarget), false, 'Should not match target under package 20')
}

// 4. Multiple mixed scopes (Multiple packages & specific plans)
{
  const mixedPromo: PromoCodeWithScopes = {
    isGlobal: false,
    scopes: [
      { packageId: 1, packagePlanId: 101 }, // Package 1, specific Plan 101
      { packageId: 2, packagePlanId: null }  // Package 2, entire package
    ]
  }

  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 1, packagePlanId: 101 }), true, 'Should match specific plan 101')
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 1, packagePlanId: 102 }), false, 'Should not match other plan 102 in package 1')
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 2, packagePlanId: 201 }), true, 'Should bubble up match any plan in package 2')
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 2, packagePlanId: 202 }), true, 'Should bubble up match another plan in package 2')
  assert.equal(isProductEligibleForPromo(mixedPromo, { packageId: 3, packagePlanId: 301 }), false, 'Should not match package 3')
}

// 5. Empty scopes & non-global
{
  const emptyPromo: PromoCodeWithScopes = {
    isGlobal: false,
    scopes: []
  }
  assert.equal(isProductEligibleForPromo(emptyPromo, { packageId: 1, packagePlanId: 101 }), false, 'Empty scopes on non-global promo should not match')
}

console.log('✅ All promo scope matcher tests passed!')
