// Port of server/src/services/promo/scope-matcher.ts.
//
// The promo scope tree is three levels deep: Leaf (package plan) -> Parent
// (package) -> Root (global). A scope row that names a plan matches that plan;
// a row that names only a package matches every plan of that package. A row that
// names both is a plan row (the package check requires packagePlanId === null),
// exactly like the TypeScript.
//
// Exports are Promo-prefixed because the services package is shared by parallel
// module ports.
package services

import "github.com/rain-kl/payincus/server-go/src/db"

// PromoScopeNode mirrors PromoScopeNode (both ids are nullable columns).
type PromoScopeNode struct {
	PackageID     *int32
	PackagePlanID *int32
}

// PromoTarget mirrors PromoTarget (the product being priced).
type PromoTarget struct {
	PackageID     int32
	PackagePlanID int32
}

// PromoCodeWithScopes mirrors PromoCodeWithScopes: the two fields the matcher
// reads off a promo code.
type PromoCodeWithScopes struct {
	IsGlobal bool
	Scopes   []PromoScopeNode
}

// PromoScopesOf adapts a loaded promo code to the matcher's structural input.
func PromoScopesOf(promo *db.PromoCodeWithDetails) PromoCodeWithScopes {
	scopes := make([]PromoScopeNode, 0, len(promo.Scopes))
	for _, scope := range promo.Scopes {
		scopes = append(scopes, PromoScopeNode{
			PackageID:     scope.PackageID,
			PackagePlanID: scope.PackagePlanID,
		})
	}
	return PromoCodeWithScopes{IsGlobal: promo.IsGlobal, Scopes: scopes}
}

// PromoIsProductEligibleForPromo mirrors isProductEligibleForPromo.
func PromoIsProductEligibleForPromo(promo PromoCodeWithScopes, target PromoTarget) bool {
	if promo.IsGlobal {
		return true
	}

	// 1. Leaf match: a scope row pinned to this exact plan.
	for _, scope := range promo.Scopes {
		if scope.PackagePlanID != nil && *scope.PackagePlanID == target.PackagePlanID {
			return true
		}
	}

	// 2. Parent match: a package-level row (packagePlanId === null) for this package.
	for _, scope := range promo.Scopes {
		if scope.PackageID != nil &&
			*scope.PackageID == target.PackageID &&
			scope.PackagePlanID == nil {
			return true
		}
	}

	return false
}
