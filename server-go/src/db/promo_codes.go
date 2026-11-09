// Port of server/src/db/promo-codes.ts.
//
// This file owns the promo-code read path plus the mutations that the promo
// engine performs inside a caller-supplied transaction. Names are prefixed with
// `promo` / `Promo` because the db package is shared by parallel module ports.
//
// Concurrency
// -----------
// IncrementPromoUsageWithLock mirrors incrementPromoUsageWithLock: it takes a
// row-level `SELECT ... FOR UPDATE` on promo_codes *inside the caller's
// transaction* before re-reading the row and incrementing the counters. The lock
// must be the first lock the transaction takes (same ordering as Node), otherwise
// a concurrent redemption could pass the quota check between the read and the
// write. Everything therefore runs on the same *gorm.DB transaction handle that
// the caller passes in.
//
// Money
// -----
// Prisma stores Decimal(10,2); the Go models keep models.Decimal so reads stay
// exact. Values that cross into the TypeScript `number` domain go through
// PromoMoneyFixed2, which reproduces `Number(value.toFixed(2))` exactly (JS rounds
// the exact binary value, ties away from zero).
package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Promo state-machine errors, mirroring the `throw new Error('CODE')` calls in
// promo-codes.ts. The engine/route layer maps them onto API error codes.
var (
	ErrPromoNotFound       = errors.New("PROMO_NOT_FOUND")
	ErrPromoDisabled       = errors.New("PROMO_DISABLED")
	ErrPromoNotStarted     = errors.New("PROMO_NOT_STARTED")
	ErrPromoExpired        = errors.New("PROMO_EXPIRED")
	ErrPromoQuotaExceeded  = errors.New("PROMO_QUOTA_EXCEEDED")
	ErrPromoBindingMissing = errors.New("PROMO_BINDING_NOT_FOUND")
)

// PromoCodeWithDetails mirrors PromoCodeWithDetails:
//
//	include: { scopes: true, _count: { select: { bindings, redemptionLogs } } }
//
// Scopes shadows the embedded models.PromoCode relation field (which is gorm:"-"
// and therefore always nil) with the actually loaded scope rows.
type PromoCodeWithDetails struct {
	models.PromoCode
	Scopes              []models.PromoCodeScope `json:"scopes"`
	BindingsCount       int64                   `json:"bindingsCount"`
	RedemptionLogsCount int64                   `json:"redemptionLogsCount"`
}

// PromoInstanceBindingWithCode mirrors InstancePromoBindingWithCode:
//
//	include: { promoCode: { include: { scopes: true } } }
type PromoInstanceBindingWithCode struct {
	ID              int32
	InstanceID      int32
	PromoCodeID     int32
	DurationType    models.PromoDurationType
	TotalCycles     *int32
	UsedCycles      int32
	RemainingCycles *int32
	BoundAt         time.Time
	LastRedeemedAt  time.Time
	PromoCode       PromoCodeWithDetails
}

// PromoBindParams mirrors the bindPromoToInstance parameter object. A nil
// TotalCycles / RemainingCycles means SQL NULL (FOREVER).
type PromoBindParams struct {
	InstanceID      int32
	PromoCodeID     int32
	DurationType    models.PromoDurationType
	TotalCycles     *int32
	UsedCycles      int32
	RemainingCycles *int32
}

// PromoBindingCycles mirrors updateInstancePromoBindingCycles parameters.
type PromoBindingCycles struct {
	ConsumedCycles     int32
	NewRemainingCycles *int32
}

// ==================== money helpers ====================

// PromoMoneyFixed2 mirrors `Number(value.toFixed(2))`.
//
// ECMAScript's toFixed rounds the *exact* binary value of the double, half away
// from zero, while Go's strconv/decimal helpers use the shortest decimal
// representation (and strconv ties-to-even). big.Rat exposes the exact value, so
// this reproduces the JS result for every input, including 0.125 -> 0.13.
func PromoMoneyFixed2(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1e21 {
		// toFixed switches to exponential notation at 1e21; Number() then yields
		// the original double again.
		return value
	}

	exact := new(big.Rat).SetFloat64(value)
	if exact == nil {
		return value
	}
	exact.Mul(exact, big.NewRat(100, 1))

	quotient, remainder := new(big.Int).QuoRem(exact.Num(), exact.Denom(), new(big.Int))
	twiceRemainder := new(big.Int).Abs(remainder)
	twiceRemainder.Lsh(twiceRemainder, 1)
	if twiceRemainder.Cmp(exact.Denom()) >= 0 {
		if exact.Num().Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}

	result, _ := new(big.Rat).SetFrac(quotient, big.NewInt(100)).Float64()
	return result
}

// promoDecimal2 mirrors `new Prisma.Decimal(value.toFixed(2))`.
func promoDecimal2(value float64) models.Decimal {
	text := strconv.FormatFloat(PromoMoneyFixed2(value), 'f', 2, 64)
	parsed, err := decimal.NewFromString(text)
	if err != nil {
		return models.Decimal{Decimal: decimal.Zero}
	}
	return models.Decimal{Decimal: parsed}
}

// ==================== client plumbing ====================

// promoClient returns the caller's transaction when one was supplied, otherwise
// the shared handle. Mirrors `const client = tx ?? prisma`.
func promoClient(ctx context.Context, tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx.WithContext(ctx)
	}
	return DB.WithContext(ctx)
}

// ==================== reads ====================

// PromoGetCodeByCode mirrors getPromoCodeByCode (`where: { code: code.trim() }`).
func PromoGetCodeByCode(ctx context.Context, code string, tx *gorm.DB) (*PromoCodeWithDetails, error) {
	return promoLoadCode(ctx, tx, "code = ?", code)
}

// PromoGetCodeByID mirrors getPromoCodeById.
func PromoGetCodeByID(ctx context.Context, id int32, tx *gorm.DB) (*PromoCodeWithDetails, error) {
	return promoLoadCode(ctx, tx, "id = ?", id)
}

func promoLoadCode(ctx context.Context, tx *gorm.DB, where string, args ...any) (*PromoCodeWithDetails, error) {
	client := promoClient(ctx, tx)

	var row models.PromoCode
	err := client.Where(where, args...).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	details := &PromoCodeWithDetails{PromoCode: row, Scopes: []models.PromoCodeScope{}}

	var scopes []models.PromoCodeScope
	if err := client.Where("promo_code_id = ?", row.ID).Order("id ASC").Find(&scopes).Error; err != nil {
		return nil, err
	}
	if scopes != nil {
		details.Scopes = scopes
	}

	if err := client.Model(&models.InstancePromoBinding{}).
		Where("promo_code_id = ?", row.ID).
		Count(&details.BindingsCount).Error; err != nil {
		return nil, err
	}
	if err := client.Model(&models.PromoRedemptionLog{}).
		Where("promo_code_id = ?", row.ID).
		Count(&details.RedemptionLogsCount).Error; err != nil {
		return nil, err
	}

	return details, nil
}

// promoBindingReturning pins the projection used by every binding mutation so the
// enum column comes back as text (GORM's typed string scan needs that).
const promoBindingReturning = `id, instance_id, promo_code_id,
	duration_type::text AS duration_type, total_cycles, used_cycles, remaining_cycles,
	bound_at, last_redeemed_at`

// PromoGetInstanceBinding mirrors getInstancePromoBinding (unique on instance_id).
func PromoGetInstanceBinding(ctx context.Context, instanceID int32, tx *gorm.DB) (*PromoInstanceBindingWithCode, error) {
	client := promoClient(ctx, tx)

	var row struct {
		ID              int32     `gorm:"column:id"`
		InstanceID      int32     `gorm:"column:instance_id"`
		PromoCodeID     int32     `gorm:"column:promo_code_id"`
		DurationType    string    `gorm:"column:duration_type"`
		TotalCycles     *int32    `gorm:"column:total_cycles"`
		UsedCycles      int32     `gorm:"column:used_cycles"`
		RemainingCycles *int32    `gorm:"column:remaining_cycles"`
		BoundAt         time.Time `gorm:"column:bound_at"`
		LastRedeemedAt  time.Time `gorm:"column:last_redeemed_at"`
	}
	err := client.Raw(
		`SELECT `+promoBindingReturning+` FROM instance_promo_bindings WHERE instance_id = ?`, instanceID,
	).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}

	promo, err := PromoGetCodeByID(ctx, row.PromoCodeID, tx)
	if err != nil {
		return nil, err
	}
	if promo == nil {
		// The relation is mandatory in Prisma; a dangling binding cannot occur
		// (FK + cascade), so treat it as "no binding" rather than panicking.
		return nil, nil
	}

	return &PromoInstanceBindingWithCode{
		ID:              row.ID,
		InstanceID:      row.InstanceID,
		PromoCodeID:     row.PromoCodeID,
		DurationType:    models.PromoDurationType(row.DurationType),
		TotalCycles:     row.TotalCycles,
		UsedCycles:      row.UsedCycles,
		RemainingCycles: row.RemainingCycles,
		BoundAt:         row.BoundAt,
		LastRedeemedAt:  row.LastRedeemedAt,
		PromoCode:       *promo,
	}, nil
}

// ==================== writes ====================

// PromoIncrementUsageWithLock mirrors incrementPromoUsageWithLock.
//
// The FOR UPDATE lock is taken on the caller's transaction; the subsequent read
// and write must use the same handle (they do). `discountAmount` is rounded with
// toFixed(2) semantics and only added when it is finite and > 0.
func PromoIncrementUsageWithLock(ctx context.Context, tx *gorm.DB, promoCodeID int32, discountAmount float64) (models.PromoCode, error) {
	if tx == nil {
		return models.PromoCode{}, errors.New("PromoIncrementUsageWithLock requires a transaction")
	}
	client := tx.WithContext(ctx)

	// 1. Row-level exclusive lock, exactly like the $queryRaw in TypeScript.
	if err := client.Exec(`SELECT id FROM "promo_codes" WHERE id = ? FOR UPDATE`, promoCodeID).Error; err != nil {
		return models.PromoCode{}, err
	}

	// 2. Re-read under the lock.
	var promo models.PromoCode
	err := client.Where("id = ?", promoCodeID).Take(&promo).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.PromoCode{}, ErrPromoNotFound
	}
	if err != nil {
		return models.PromoCode{}, err
	}

	if !promo.Enabled {
		return models.PromoCode{}, ErrPromoDisabled
	}
	now := time.Now()
	if promo.StartsAt != nil && promo.StartsAt.After(now) {
		return models.PromoCode{}, ErrPromoNotStarted
	}
	if promo.ExpiresAt != nil && !promo.ExpiresAt.After(now) {
		return models.PromoCode{}, ErrPromoExpired
	}
	if promo.MaxTotalUses != nil && promo.UsedTotalCount >= *promo.MaxTotalUses {
		return models.PromoCode{}, ErrPromoQuotaExceeded
	}

	// 3. Atomic increment.
	validDiscount := 0.0
	if !math.IsNaN(discountAmount) && !math.IsInf(discountAmount, 0) && discountAmount > 0 {
		validDiscount = PromoMoneyFixed2(discountAmount)
	}

	var updated models.PromoCode
	if validDiscount > 0 {
		err = client.Raw(
			`UPDATE promo_codes
			    SET used_total_count = used_total_count + 1,
			        total_discount_amount = total_discount_amount + ?,
			        updated_at = ?
			  WHERE id = ?
			  RETURNING *`,
			promoDecimal2(validDiscount), now, promoCodeID,
		).Scan(&updated).Error
	} else {
		err = client.Raw(
			`UPDATE promo_codes
			    SET used_total_count = used_total_count + 1,
			        updated_at = ?
			  WHERE id = ?
			  RETURNING *`,
			now, promoCodeID,
		).Scan(&updated).Error
	}
	if err != nil {
		return models.PromoCode{}, err
	}
	return updated, nil
}

// PromoBindToInstance mirrors bindPromoToInstance: an upsert keyed on the unique
// instance_id. bound_at is only written on insert (Prisma's update branch leaves
// it untouched).
func PromoBindToInstance(ctx context.Context, tx *gorm.DB, params PromoBindParams) (models.InstancePromoBinding, error) {
	client := promoClient(ctx, tx)
	now := time.Now()

	var binding models.InstancePromoBinding
	err := client.Raw(
		`INSERT INTO instance_promo_bindings
		     (instance_id, promo_code_id, duration_type, total_cycles, used_cycles,
		      remaining_cycles, bound_at, last_redeemed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (instance_id) DO UPDATE SET
		     promo_code_id = EXCLUDED.promo_code_id,
		     duration_type = EXCLUDED.duration_type,
		     total_cycles = EXCLUDED.total_cycles,
		     used_cycles = EXCLUDED.used_cycles,
		     remaining_cycles = EXCLUDED.remaining_cycles,
		     last_redeemed_at = EXCLUDED.last_redeemed_at
		 RETURNING `+promoBindingReturning,
		params.InstanceID,
		params.PromoCodeID,
		string(params.DurationType),
		params.TotalCycles,
		params.UsedCycles,
		params.RemainingCycles,
		now,
		now,
	).Scan(&binding).Error
	if err != nil {
		return models.InstancePromoBinding{}, err
	}
	return binding, nil
}

// PromoUpdateBindingCycles mirrors updateInstancePromoBindingCycles.
func PromoUpdateBindingCycles(ctx context.Context, tx *gorm.DB, instanceID int32, cycles PromoBindingCycles) (models.InstancePromoBinding, error) {
	client := promoClient(ctx, tx)

	var binding models.InstancePromoBinding
	err := client.Raw(
		`UPDATE instance_promo_bindings
		    SET used_cycles = used_cycles + ?,
		        remaining_cycles = ?,
		        last_redeemed_at = ?
		  WHERE instance_id = ?
		  RETURNING `+promoBindingReturning,
		cycles.ConsumedCycles,
		cycles.NewRemainingCycles,
		time.Now(),
		instanceID,
	).Scan(&binding).Error
	if err != nil {
		return models.InstancePromoBinding{}, err
	}
	if binding.ID == 0 {
		// Prisma's update throws P2025 for a missing row.
		return models.InstancePromoBinding{}, ErrPromoBindingMissing
	}
	return binding, nil
}

// PromoUnbindFromInstance mirrors unbindPromoFromInstance: deleteMany + count > 0.
func PromoUnbindFromInstance(ctx context.Context, tx *gorm.DB, instanceID int32) (bool, error) {
	client := promoClient(ctx, tx)
	result := client.Exec(`DELETE FROM instance_promo_bindings WHERE instance_id = ?`, instanceID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// PromoIsUniqueViolation reports whether err is a PostgreSQL unique violation,
// used by callers that need to distinguish an upsert race.
func PromoIsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// promoCompilerCheck keeps fmt imported for the documented error format below.
var _ = fmt.Sprintf
