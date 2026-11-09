// Port of the create-quote slice of server/src/db/billing-operations.ts.
//
// billing-operations.ts is ~1200 lines and belongs to the high-risk instance /
// admin billing waves: plan changes, prorated refunds, renewals, auto-renew. Only
// ONE of its functions is needed before then — `calculateCreateBilling`, the pure
// price quote used by the promo-code and AFF flows — so it lands here on its own.
// Keeping it separate means the billing wave can add the rest of that module
// without this file becoming a stub it has to fight.
//
// calculateCreateBilling is pure: it reads the plan's price and billing cycle and
// returns a quote. It moves no money, so porting it early cannot affect payments.
package db

import (
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// CreateBillingResult mirrors CreateBillingResult.
//
// ExpiresAt is the instant a new subscription would end: `addMonths(now,
// plan.billingCycle)` where a month is 31 days (see lib.AddMonths), NOT a calendar
// month.
type CreateBillingResult struct {
	Plan         models.PackagePlan
	Price        float64
	BillingCycle int32
	SetupFee     float64
	TotalPrice   float64
	ExpiresAt    time.Time
}

// CalculateCreateBilling mirrors calculateCreateBilling:
//
//	const price = Number(plan.price) / 100  // 分转元
//	const totalPrice = price                // 开通费已废弃
//	const expiresAt = addMonths(new Date(), plan.billingCycle)
//
// The setup fee is deliberately 0: it was retired, and the plan's setup_fee column
// is no longer charged. `price` is the plan price in yuan (the column stores cents).
func CalculateCreateBilling(plan models.PackagePlan, now time.Time) CreateBillingResult {
	// Number(plan.price) / 100 — the column is an integer cent amount, so the
	// division is exact for every representable price.
	price := decimalToFloat64(plan.Price) / 100

	return CreateBillingResult{
		Plan:         plan,
		Price:        price,
		BillingCycle: plan.BillingCycle,
		SetupFee:     0,
		TotalPrice:   price,
		ExpiresAt:    addMonths31(now, int(plan.BillingCycle)),
	}
}

// addMonths31 is the 31-day month used everywhere in the billing tables. It is a
// local copy rather than lib.AddMonths because package db cannot import lib: lib
// imports db (log sanitizer, token invalidation, notification delivery), so the
// reverse edge would be a cycle. lib/billing_calc_test.go pins the same rule on
// both sides, so they cannot drift silently.
func addMonths31(date time.Time, months int) time.Time {
	return date.AddDate(0, 0, months*31)
}

// init wires the seam that routes/promo_codes.go declares. That route needs the
// quote's totalPrice; leaving the seam unassigned would make every promo apply
// fail with "not ported". Assigning it here keeps the dependency pointing from the
// route to the data layer, which is the direction the rest of the codebase uses.
func init() {
	RegisterCreateBillingQuote(func(plan models.PackagePlan) (float64, error) {
		return CalculateCreateBilling(plan, time.Now()).TotalPrice, nil
	})
}

// CreateBillingQuote is the signature the route seam expects.
type CreateBillingQuote func(plan models.PackagePlan) (float64, error)

// createBillingQuote holds the registered implementation. It is a hook rather than
// a direct call so package routes never has to import package db's billing
// internals, and so a test can substitute a fixed quote.
var createBillingQuote CreateBillingQuote

// RegisterCreateBillingQuote installs the quote provider. Called from this
// package's init, and from tests that need a deterministic price.
func RegisterCreateBillingQuote(quote CreateBillingQuote) {
	createBillingQuote = quote
}

// GetCreateBillingQuote returns the installed provider, or nil when none is
// registered yet (for example in a unit test that never links the db package).
func GetCreateBillingQuote() CreateBillingQuote {
	return createBillingQuote
}
