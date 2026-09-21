export interface PromoScopeNode {
  packageId: number | null
  packagePlanId: number | null
}

export interface PromoTarget {
  packageId: number
  packagePlanId: number
}

export interface PromoCodeWithScopes {
  isGlobal: boolean
  scopes: PromoScopeNode[]
}

export interface CreationQuoteParams {
  originalPrice: number
  discountType: 'PERCENTAGE' | 'FIXED_AMOUNT'
  discountValue: number
}

export interface CreationQuoteResult {
  originalPrice: number
  discountAmount: number
  finalPrice: number
}

export interface RenewalSplitParams {
  monthlyPrice: number
  months: number
  discountRate: number
  remainingCycles: number | null // null indicates FOREVER
}

export interface RenewalSplitResult {
  discountedMonths: number
  regularMonths: number
  discountAmount: number
  finalAmount: number
  consumedCycles: number
  newRemainingCycles: number | null
  willUnbind: boolean
}
