// Maintenance writes owned by services/billing-scheduler.ts.
//
// Both are inline `prisma.*` calls in the TypeScript module (no db/ export), so
// they live here rather than in a route-facing db file.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// CancelStalePendingRechargeOrders mirrors cleanExpiredRechargeOrders:
//
//	where: { status: 'pending', expiredAt: { lt: now - grace } }
//	data:  { status: 'cancelled' }
//
// The grace period is load-bearing: a gateway callback can arrive minutes after the
// local expiry and must still be credited, so the order is only cancelled six hours
// after it expired.
func CancelStalePendingRechargeOrders(ctx context.Context, grace time.Duration) (int64, error) {
	if DB == nil {
		return 0, errors.New("database not initialized")
	}
	cutoff := time.Now().UTC().Add(-grace)
	result := DB.WithContext(ctx).Table("recharge_records").
		Where("status = ?", string(models.RechargeStatusPending)).
		Where("expired_at < ?", cutoff).
		Update("status", string(models.RechargeStatusCancelled))
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// DeleteOldPaymentCallbacks mirrors cleanOldPaymentCallbacks: the replay-protection
// rows are only needed for 30 days.
func DeleteOldPaymentCallbacks(ctx context.Context, olderThan time.Time) (int64, error) {
	if DB == nil {
		return 0, errors.New("database not initialized")
	}
	result := DB.WithContext(ctx).Exec(`DELETE FROM payment_callbacks WHERE created_at < ?`, olderThan.UTC())
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// BillingSchedulerInstanceRow is the fresh instance read used by every billing
// sub-job before it acts: the row may have changed since the candidate query.
//
// A full models.Instance is used because the renewal path needs version,
// billing_price/cycle, suspend_reason and the resource fields.
func BillingSchedulerInstanceRow(ctx context.Context, instanceID int32) (*models.Instance, error) {
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	var instance models.Instance
	result := DB.WithContext(ctx).Where("id = ?", instanceID).Limit(1).Find(&instance)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &instance, nil
}
