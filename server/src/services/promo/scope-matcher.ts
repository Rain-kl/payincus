import type { PromoCodeWithScopes, PromoTarget } from './types.js'

/**
 * 判断指定产品是否在优惠码的适用范围树中
 * 匹配路径：Leaf (Plan) -> Parent (Package) -> Root (Global)
 */
export function isProductEligibleForPromo(
  promo: PromoCodeWithScopes,
  target: PromoTarget
): boolean {
  if (promo.isGlobal) {
    return true
  }

  // 1. 检查具体方案（叶子节点）
  const matchPlan = promo.scopes.some(
    scope => scope.packagePlanId !== null && scope.packagePlanId === target.packagePlanId
  )
  if (matchPlan) {
    return true
  }

  // 2. 向上冒泡检查所属套餐（父节点）
  const matchPackage = promo.scopes.some(
    scope => scope.packageId !== null && scope.packageId === target.packageId && scope.packagePlanId === null
  )
  if (matchPackage) {
    return true
  }

  return false
}
