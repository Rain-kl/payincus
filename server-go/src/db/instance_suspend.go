// Port of server/src/db/instance-suspend.ts.
//
// The module is the state machine behind expiry handling. Every write is a
// conditional `updateMany` whose WHERE clause repeats the eligibility rules, so a
// manual suspend, a renewal or a second replica racing the scheduler cannot be
// overwritten. The returned bool is "this caller won the transition", and the
// scheduler uses it to decide whether to notify.
package db

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ExpiryNotificationClaimGuard mirrors ExpiryNotificationClaimGuard.
type ExpiryNotificationClaimGuard struct {
	MinExpiresAt             time.Time
	MaxExpiresAt             time.Time
	RecentNotificationCutoff time.Time
}

// AutoRenewAttemptGuard mirrors AutoRenewAttemptGuard.
type AutoRenewAttemptGuard struct {
	Now             time.Time
	ExpiryThreshold time.Time
	Version         int32
	CurrentAttempts int32
}

// SuspendInstanceByExpiry mirrors suspendInstanceByExpiry: only a paid instance
// that is past `expiresAt` and not already suspended/deleted transitions. nil means
// "somebody else got there first".
func SuspendInstanceByExpiry(ctx context.Context, instanceID int32) (*models.Instance, error) {
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	now := time.Now().UTC()
	result := DB.WithContext(ctx).Model(&models.Instance{}).
		Where("id = ?", instanceID).
		Where("package_plan_id IS NOT NULL").
		Where("expires_at IS NOT NULL AND expires_at < ?", now).
		Where("status NOT IN ?", []string{
			string(models.InstanceStatusSuspended), string(models.InstanceStatusDeleted),
		}).
		Updates(map[string]any{
			"status":         string(models.InstanceStatusSuspended),
			"suspended_at":   now,
			"suspended_by":   nil,
			"suspend_reason": "expired",
			"version":        gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}

	var instance models.Instance
	if err := DB.WithContext(ctx).Where("id = ?", instanceID).Take(&instance).Error; err != nil {
		return nil, err
	}
	return &instance, nil
}

// IsInstanceSuspended mirrors isInstanceSuspended.
func IsInstanceSuspended(ctx context.Context, instanceID int32) (bool, error) {
	var rows []struct {
		Status string `gorm:"column:status"`
	}
	err := DB.WithContext(ctx).Table("instances").Select("status::text AS status").
		Where("id = ?", instanceID).Limit(1).Find(&rows).Error
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].Status == string(models.InstanceStatusSuspended), nil
}

// GetExpiredUnsuspendedPaidInstances mirrors getExpiredUnsuspendedInstances: paid
// instances only (packagePlanId not null), so free instances are never suspended by
// the billing job.
//
// It is deliberately NOT db.GetExpiredUnsuspendedInstances from db/instances.go:
// that helper predates the billing scheduler, omits the packagePlanId filter and
// projects a narrow row. Reusing it would suspend free instances, which the Node
// scheduler never does.
func GetExpiredUnsuspendedPaidInstances(ctx context.Context) ([]models.Instance, error) {
	now := time.Now().UTC()
	instances := make([]models.Instance, 0, 16)
	err := DB.WithContext(ctx).
		Where("package_plan_id IS NOT NULL").
		Where("expires_at IS NOT NULL AND expires_at < ?", now).
		Where("status NOT IN ?", []string{
			string(models.InstanceStatusSuspended), string(models.InstanceStatusDeleted),
		}).
		Order("id ASC").
		Find(&instances).Error
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// GetAutoRenewInstances mirrors getAutoRenewInstances.
func GetAutoRenewInstances(ctx context.Context, hoursBeforeExpiry int) ([]models.Instance, error) {
	now := time.Now().UTC()
	threshold := now.Add(time.Duration(hoursBeforeExpiry) * time.Hour)
	instances := make([]models.Instance, 0, 16)
	err := DB.WithContext(ctx).
		Where("auto_renew = ?", true).
		Where("expires_at IS NOT NULL AND expires_at <= ? AND expires_at > ?", threshold, now).
		Where("status NOT IN ?", []string{
			string(models.InstanceStatusSuspended), string(models.InstanceStatusDeleted),
		}).
		Order("id ASC").
		Find(&instances).Error
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// UpdateAutoRenewAttempt mirrors updateAutoRenewAttempt: the guard makes the
// attempt counter a compare-and-swap, so two workers cannot both "spend" an attempt.
func UpdateAutoRenewAttempt(
	ctx context.Context,
	instanceID int32,
	attempt int32,
	disableAutoRenew bool,
	guard *AutoRenewAttemptGuard,
) (bool, error) {
	now := time.Now().UTC()
	updates := map[string]any{
		"auto_renew_attempts":        attempt,
		"last_auto_renew_attempt_at": now,
	}
	if disableAutoRenew {
		updates["auto_renew"] = false
	}

	query := DB.WithContext(ctx).Model(&models.Instance{}).Where("id = ?", instanceID)
	if guard != nil {
		query = query.
			Where("auto_renew = ?", true).
			Where("expires_at IS NOT NULL AND expires_at > ? AND expires_at <= ?", guard.Now.UTC(), guard.ExpiryThreshold.UTC()).
			Where("status NOT IN ?", []string{
				string(models.InstanceStatusSuspended), string(models.InstanceStatusDeleted),
			}).
			Where("version = ?", guard.Version).
			Where("auto_renew_attempts = ?", guard.CurrentAttempts)
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// ResetAutoRenewAttempts mirrors resetAutoRenewAttempts.
func ResetAutoRenewAttempts(ctx context.Context, instanceID int32) error {
	return DB.WithContext(ctx).Model(&models.Instance{}).Where("id = ?", instanceID).
		Updates(map[string]any{"auto_renew_attempts": int32(0), "last_auto_renew_attempt_at": nil}).Error
}

// UpdateExpiryNotifiedAt mirrors updateExpiryNotifiedAt. The guarded form is the
// notification claim: exactly one caller can stamp the instance inside the window.
func UpdateExpiryNotifiedAt(ctx context.Context, instanceID int32, guard *ExpiryNotificationClaimGuard) (bool, error) {
	query := DB.WithContext(ctx).Model(&models.Instance{}).Where("id = ?", instanceID)
	if guard != nil {
		query = query.
			Where("expires_at IS NOT NULL AND expires_at >= ? AND expires_at < ?", guard.MinExpiresAt.UTC(), guard.MaxExpiresAt.UTC()).
			Where("auto_renew = ?", false).
			Where("status NOT IN ?", []string{
				string(models.InstanceStatusSuspended), string(models.InstanceStatusDeleted),
			}).
			Where("(expiry_notified_at IS NULL OR expiry_notified_at < ?)", guard.RecentNotificationCutoff.UTC())
	}
	result := query.Update("expiry_notified_at", time.Now().UTC())
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// GetSuspendedInstancesOlderThan mirrors getSuspendedInstancesOlderThan.
//
// JavaScript's setDate is local-calendar arithmetic; production runs UTC, so UTC
// calendar arithmetic is the faithful equivalent there and the only deterministic
// choice everywhere else.
func GetSuspendedInstancesOlderThan(ctx context.Context, days int) ([]models.Instance, error) {
	threshold := time.Now().UTC().AddDate(0, 0, -days)
	instances := make([]models.Instance, 0, 16)
	err := DB.WithContext(ctx).
		Where("status = ?", string(models.InstanceStatusSuspended)).
		Where("suspended_at IS NOT NULL AND suspended_at < ?", threshold).
		Order("id ASC").
		Find(&instances).Error
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// RestoreExpiryDeleteClaim mirrors restoreExpiryDeleteClaim: the compensation used
// when the host/Incus step of the expiry-delete job fails. `status = 'deleted'` in
// the WHERE clause makes it a no-op if someone else already advanced the row.
func RestoreExpiryDeleteClaim(ctx context.Context, instanceID int32) error {
	return DB.WithContext(ctx).Model(&models.Instance{}).
		Where("id = ? AND status = ?", instanceID, string(models.InstanceStatusDeleted)).
		Updates(map[string]any{
			"status":  string(models.InstanceStatusSuspended),
			"version": gorm.Expr("version + 1"),
		}).Error
}

// ExpiryDeleteClaim mirrors the conditional UPDATE that claims an instance for the
// expiry-delete job:
//
//	where { id, version, status: 'suspended', suspendReason: 'expired',
//	        suspendedAt: { not: null, lt: deleteThreshold } }
//	data  { status: 'deleted', version: { increment: 1 } }
//
// Returns true when this caller owns the deletion and must either finish it or
// call RestoreExpiryDeleteClaim.
func ExpiryDeleteClaim(ctx context.Context, instanceID int32, version int32, deleteThreshold time.Time) (bool, error) {
	result := DB.WithContext(ctx).Model(&models.Instance{}).
		Where("id = ? AND version = ?", instanceID, version).
		Where("status = ?", string(models.InstanceStatusSuspended)).
		Where("suspend_reason = ?", "expired").
		Where("suspended_at IS NOT NULL AND suspended_at < ?", deleteThreshold.UTC()).
		Updates(map[string]any{
			"status":  string(models.InstanceStatusDeleted),
			"version": gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// ExpiryNotifyCandidate is the id-only projection of the expiry-notify page.
type ExpiryNotifyCandidate struct {
	ID int32
}

// InstanceListExpiryNotifyCandidates mirrors the findMany at the top of
// notifyExpiringInstances:
//
//	expiresAt: { gte: min, lt: max }, autoRenew: false,
//	status: { notIn: ['suspended','deleted'] },
//	OR: [{ expiryNotifiedAt: null }, { expiryNotifiedAt: { lt: cutoff } }]
func InstanceListExpiryNotifyCandidates(
	ctx context.Context,
	minThreshold, maxThreshold, recentNotificationCutoff time.Time,
) ([]ExpiryNotifyCandidate, error) {
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	var ids []int32
	err := DB.WithContext(ctx).Table("instances").
		Select("id").
		Where("expires_at IS NOT NULL AND expires_at >= ? AND expires_at < ?", minThreshold.UTC(), maxThreshold.UTC()).
		Where("auto_renew = ?", false).
		Where("status NOT IN ?", []string{
			string(models.InstanceStatusSuspended), string(models.InstanceStatusDeleted),
		}).
		Where("(expiry_notified_at IS NULL OR expiry_notified_at < ?)", recentNotificationCutoff.UTC()).
		Order("id ASC").
		Pluck("id", &ids).Error
	if err != nil {
		return nil, err
	}
	candidates := make([]ExpiryNotifyCandidate, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, ExpiryNotifyCandidate{ID: id})
	}
	return candidates, nil
}
