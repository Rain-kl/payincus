// Port of server/src/db/instance-provisioning-compensation.ts.
//
// The only caller in Node is the instance-create timeout sweeper in app.ts. The Go
// route layer has its own unexported copy for the request path; the sweeper needs
// the same transaction, so it is ported here.
//
// Serializable transaction, 15s timeout, in the TypeScript. GORM cannot request an
// isolation level per transaction without dropping to database/sql, and PostgreSQL's
// procedure here does not depend on serializable retries: every money move is a
// conditional/increment UPDATE under the 4110/4112 advisory locks. The 15s timeout
// is preserved through the context.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// InstanceCompensationResult mirrors
// `{ refunded, refundAmount, reason? }`.
type InstanceCompensationResult struct {
	Refunded     bool
	RefundAmount float64
	Reason       string
}

// CompensateFailedInstancePurchase mirrors compensateFailedInstancePurchase.
//
// Idempotent: a second call after a successful refund returns
// `already_refunded`, and the refund marker is the Chinese remark string the
// TypeScript matches on.
func CompensateFailedInstancePurchase(ctx context.Context, instanceID, userID, hostID int32) (InstanceCompensationResult, error) {
	if DB == nil {
		return InstanceCompensationResult{}, errors.New("database not initialized")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	result := InstanceCompensationResult{Reason: ""}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := AdvisoryTransactionLockTx(tx, UserBalanceLockNamespace, userID); err != nil {
			return err
		}

		var instance struct {
			ID     int32  `gorm:"column:id"`
			Name   string `gorm:"column:name"`
			UserID int32  `gorm:"column:user_id"`
			HostID int32  `gorm:"column:host_id"`
		}
		found := tx.Raw(`SELECT id, name, user_id, host_id FROM instances WHERE id = ?`, instanceID).
			Scan(&instance)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected == 0 || instance.UserID != userID || instance.HostID != hostID {
			result = InstanceCompensationResult{Reason: "instance_not_found_or_mismatch"}
			return nil
		}

		var purchase struct {
			ID     int32  `gorm:"column:id"`
			Amount string `gorm:"column:amount"`
		}
		if err := tx.Raw(`SELECT id, amount::text AS amount FROM instance_billing_records
		                   WHERE instance_id = ? AND user_id = ? AND type = 'newPurchase'
		                   ORDER BY created_at ASC, id ASC LIMIT 1`, instanceID, userID).
			Scan(&purchase).Error; err != nil {
			return err
		}
		if purchase.ID == 0 {
			result = InstanceCompensationResult{Reason: "not_paid_purchase"}
			return nil
		}

		var existingRefund struct {
			ID int32 `gorm:"column:id"`
		}
		if err := tx.Raw(`SELECT id FROM instance_billing_records
		                   WHERE instance_id = ? AND user_id = ? AND type = 'refund'
		                     AND remark LIKE '%实例创建失败自动退款%'
		                   LIMIT 1`, instanceID, userID).Scan(&existingRefund).Error; err != nil {
			return err
		}
		if existingRefund.ID != 0 {
			result = InstanceCompensationResult{Reason: "already_refunded"}
			return nil
		}

		refundAmount, parseErr := decimal.NewFromString(purchase.Amount)
		if parseErr != nil || !refundAmount.IsPositive() {
			result = InstanceCompensationResult{Reason: "invalid_purchase_amount"}
			return nil
		}

		balanceBeforeText, err := userBalanceText(tx, userID)
		if err != nil {
			return err
		}
		if balanceBeforeText == nil {
			return errors.New("用户不存在")
		}
		if err := tx.Exec(`UPDATE users SET balance = balance + ? WHERE id = ?`, refundAmount.String(), userID).Error; err != nil {
			return err
		}
		balanceAfterText, err := userBalanceText(tx, userID)
		if err != nil {
			return err
		}
		balanceBefore := instanceCompensationMustDecimal(balanceBeforeText)
		balanceAfter := instanceCompensationMustDecimal(balanceAfterText)

		remark := fmt.Sprintf("实例创建失败自动退款：%s", instance.Name)
		var balanceLogID int32
		row := tx.Raw(`INSERT INTO balance_logs (user_id, type, amount, balance_before, balance_after, instance_id, remark, created_at)
		                VALUES (?, 'refund', ?, ?, ?, ?, ?, NOW()) RETURNING id`,
			userID, refundAmount.String(), balanceBefore, balanceAfter, instanceID, remark).Row()
		if err := row.Scan(&balanceLogID); err != nil {
			return err
		}

		now := time.Now().UTC()
		if err := tx.Exec(`INSERT INTO instance_billing_records
			(instance_id, user_id, type, amount, months, period_start, period_end, balance_log_id, remark, created_at)
			VALUES (?, ?, 'refund', ?, 0, ?, ?, ?, ?, NOW())`,
			instanceID, userID, refundAmount.Neg().String(), now, now, balanceLogID, remark).Error; err != nil {
			return err
		}

		var affLogs []struct {
			ID             int32  `gorm:"column:id"`
			UserID         int32  `gorm:"column:user_id"`
			AffCodeID      *int32 `gorm:"column:aff_code_id"`
			Amount         string `gorm:"column:amount"`
			OriginalAmount string `gorm:"column:original_amount"`
		}
		if err := tx.Raw(`SELECT id, user_id, aff_code_id, amount::text AS amount, original_amount::text AS original_amount
		                   FROM aff_logs
		                  WHERE instance_id = ? AND type = 'new_purchase' AND amount > 0
		                  ORDER BY id ASC`, instanceID).Scan(&affLogs).Error; err != nil {
			return err
		}

		for _, affLog := range affLogs {
			commission, parseErr := decimal.NewFromString(affLog.Amount)
			if parseErr != nil || !commission.IsPositive() {
				continue
			}
			if err := AdvisoryTransactionLockTx(tx, UserAffBalanceLockNamespace, affLog.UserID); err != nil {
				return err
			}
			affBeforeText, err := userAffBalanceText(tx, affLog.UserID)
			if err != nil {
				return err
			}
			if affBeforeText == nil {
				continue
			}
			if err := tx.Exec(`UPDATE users SET aff_balance = aff_balance - ? WHERE id = ?`, commission.String(), affLog.UserID).Error; err != nil {
				return err
			}
			affAfterText, err := userAffBalanceText(tx, affLog.UserID)
			if err != nil {
				return err
			}
			affBefore := instanceCompensationMustDecimal(affBeforeText)
			affAfter := instanceCompensationMustDecimal(affAfterText)

			if err := tx.Exec(`INSERT INTO aff_logs
				(user_id, type, amount, aff_code_id, instance_id, original_amount, balance_before, balance_after, remark, created_at)
				VALUES (?, 'new_purchase', ?, ?, ?, ?, ?, ?, ?, NOW())`,
				affLog.UserID, commission.Neg().String(), affLog.AffCodeID, instanceID,
				affLog.OriginalAmount, affBefore, affAfter,
				fmt.Sprintf("实例创建失败返利冲正：%s", instance.Name)).Error; err != nil {
				return err
			}

			if affLog.AffCodeID != nil {
				if err := tx.Exec(`UPDATE aff_codes SET used_count = used_count - 1 WHERE id = ? AND used_count > 0`,
					*affLog.AffCodeID).Error; err != nil {
					return err
				}
				if err := tx.Exec(`UPDATE aff_codes SET total_earnings = total_earnings - ? WHERE id = ?`,
					commission.String(), *affLog.AffCodeID).Error; err != nil {
					return err
				}
			}
		}

		result = InstanceCompensationResult{Refunded: true, RefundAmount: refundAmount.InexactFloat64()}
		return nil
	})
	if err != nil {
		return InstanceCompensationResult{}, err
	}
	return result, nil
}

func userBalanceText(tx *gorm.DB, userID int32) (*string, error) {
	var rows []struct {
		Balance *string `gorm:"column:balance"`
	}
	if err := tx.Raw(`SELECT balance::text AS balance FROM users WHERE id = ?`, userID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].Balance, nil
}

func userAffBalanceText(tx *gorm.DB, userID int32) (*string, error) {
	var rows []struct {
		AffBalance *string `gorm:"column:aff_balance"`
	}
	if err := tx.Raw(`SELECT aff_balance::text AS aff_balance FROM users WHERE id = ?`, userID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].AffBalance, nil
}

// mustDecimal mirrors `Number(null)` → 0? No: `Number(null)` is 0, and the
// TypeScript reads a non-null column there, so a nil can only come from a schema
// change. Returning 0 keeps the insert total.
func instanceCompensationMustDecimal(value *string) string {
	if value == nil {
		return "0"
	}
	return *value
}
