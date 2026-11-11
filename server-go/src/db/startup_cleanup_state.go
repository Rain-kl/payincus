// Writes owned by the instance-create timeout sweeper (app.ts inline cleanup).
package db

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ClaimCreatingInstanceAsError mirrors
//
//	prisma.instance.updateMany({ where: { id, status: 'creating' },
//	                            data: { status: 'error' } })
//
// The `status = 'creating'` predicate is the atomic claim that keeps the sweeper
// from racing `createInstanceAsync`'s own rollback.
func ClaimCreatingInstanceAsError(ctx context.Context, instanceID int32) (bool, error) {
	if DB == nil {
		return false, errors.New("database not initialized")
	}
	result := DB.WithContext(ctx).Exec(
		`UPDATE instances SET status = 'error', updated_at = ? WHERE id = ? AND status = 'creating'`,
		time.Now().UTC(), instanceID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// FailRunningCreateOperationTasks mirrors the operationTask.updateMany in the
// create-timeout cleaner:
//
//	where: { hostId, instanceName, kind: 'create', status: 'running' }
//	data:  { status: 'failed', error, finishedAt: now }
func FailRunningCreateOperationTasks(ctx context.Context, hostID int32, instanceName, reason string) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	return DB.WithContext(ctx).Exec(
		`UPDATE operation_tasks SET status = 'failed', error = ?, finished_at = ?
		  WHERE host_id = ? AND instance_name = ? AND kind = 'create' AND status = 'running'`,
		reason, time.Now().UTC(), hostID, instanceName).Error
}

// DBTx is re-exported so scheduler helpers can name the transaction type without
// importing gorm directly.
type DBTx = gorm.DB
