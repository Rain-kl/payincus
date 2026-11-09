// Port of server/src/db/promo-redemptions.ts.
//
// The promo redemption ledger: one row per redemption (create or renew) with the
// price breakdown. Names are Promo-prefixed because the db package is shared by
// parallel module ports.
//
// Money
// -----
// Every price column is Decimal(10,2) and the TypeScript passes
// `new Prisma.Decimal(value.toFixed(2))`. PromoMoneyFixed2 (promo-codes.go)
// reproduces `Number(value.toFixed(2))` exactly, so the values are rounded with
// the same tie-breaking rule before they reach the column.
package db

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Promo redemption action types, mirroring the `'create' | 'renew'` union in the
// TypeScript signatures. The column itself is a plain VarChar(32).
const (
	PromoRedemptionActionCreate = "create"
	PromoRedemptionActionRenew  = "renew"
)

// PromoRedemptionLogData mirrors createPromoRedemptionLog's `data` argument.
// Months and CommissionAmount are pointers because the TypeScript applies
// `data.months ?? 1` and `(data.commissionAmount ?? 0)`; nil means "not
// provided".
type PromoRedemptionLogData struct {
	PromoCodeID      int32
	InstanceID       int32
	UserID           int32
	ActionType       string
	CycleIndex       int32
	Months           *int32
	OriginalPrice    float64
	DiscountAmount   float64
	FinalPrice       float64
	CommissionAmount *float64
}

// PromoCreateRedemptionLog mirrors createPromoRedemptionLog.
//
// The TypeScript signature requires a transaction (`tx.promoRedemptionLog.create`)
// and every caller passes one, so a nil tx is rejected instead of silently
// running outside the caller's transaction.
func PromoCreateRedemptionLog(ctx context.Context, tx *gorm.DB, data PromoRedemptionLogData) (models.PromoRedemptionLog, error) {
	if tx == nil {
		return models.PromoRedemptionLog{}, errors.New("PromoCreateRedemptionLog requires a transaction")
	}

	row := promoRedemptionLogRow(data)
	// Returning{} reads the row back so createdAt and every default come from the
	// database exactly like Prisma's create().
	if err := tx.WithContext(ctx).Clauses(clause.Returning{}).Create(&row).Error; err != nil {
		return models.PromoRedemptionLog{}, err
	}
	return row, nil
}

// promoRedemptionLogRow applies the TypeScript defaults and the toFixed(2)
// rounding. Kept separate so the mapping can be pinned without a database.
func promoRedemptionLogRow(data PromoRedemptionLogData) models.PromoRedemptionLog {
	months := int32(1)
	if data.Months != nil {
		months = *data.Months
	}
	commissionAmount := 0.0
	if data.CommissionAmount != nil {
		commissionAmount = *data.CommissionAmount
	}

	return models.PromoRedemptionLog{
		PromoCodeID:      data.PromoCodeID,
		InstanceID:       data.InstanceID,
		UserID:           data.UserID,
		ActionType:       data.ActionType,
		CycleIndex:       data.CycleIndex,
		Months:           months,
		OriginalPrice:    promoDecimal2(data.OriginalPrice),
		DiscountAmount:   promoDecimal2(data.DiscountAmount),
		FinalPrice:       promoDecimal2(data.FinalPrice),
		CommissionAmount: promoDecimal2(commissionAmount),
	}
}

// PromoGetRedemptionLogsByInstance mirrors getPromoRedemptionLogsByInstance:
// `where: { instanceId }, orderBy: { createdAt: 'desc' }`.
func PromoGetRedemptionLogsByInstance(ctx context.Context, instanceID int32, tx *gorm.DB) ([]models.PromoRedemptionLog, error) {
	client := promoClient(ctx, tx)

	rows := []models.PromoRedemptionLog{}
	if err := client.Where("instance_id = ?", instanceID).Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// PromoCountUserRedemptions mirrors countUserPromoRedemptions.
//
// The TypeScript declares three overloads only to work around optional
// parameters sitting before the transaction; in Go a nil actionType means
// "count every action", which is what the `(promoCodeId, userId, tx?)` overload
// does.
func PromoCountUserRedemptions(ctx context.Context, promoCodeID, userID int32, actionType *string, tx *gorm.DB) (int64, error) {
	client := promoClient(ctx, tx)

	query := client.Model(&models.PromoRedemptionLog{}).
		Where("promo_code_id = ?", promoCodeID).
		Where("user_id = ?", userID)
	if actionType != nil {
		query = query.Where("action_type = ?", *actionType)
	}

	var count int64
	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
