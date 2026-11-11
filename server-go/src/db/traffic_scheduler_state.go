// Scheduler-owned traffic state transitions.
//
// These are the three writes traffic-scheduler.ts performs with inline
// `prisma.*.updateMany` calls rather than through db/traffic.ts:
//
//   - markInstanceTrafficWarningIfNeeded   (instance → WARNING, conditional)
//   - releaseUserTrafficWarningLease       (roll the claim back on delivery failure)
//   - releaseInstanceTrafficWarningLease   (same for the instance claim)
//
// They live in their own file so the traffic module stays the single owner of the
// tables' read paths and no shared file needs to change.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// errSchedulerTrafficDatabaseMissing is returned before any SQL when the pool is
// not initialised (the conformance harness runs the package without a database).
var errSchedulerTrafficDatabaseMissing = errors.New("database not initialized")

// MarkInstanceTrafficWarningIfNeeded mirrors markInstanceTrafficWarningIfNeeded:
// only a NORMAL instance can claim the warning, so concurrent replicas send it
// once.
func MarkInstanceTrafficWarningIfNeeded(ctx context.Context, instanceID int32) (bool, error) {
	if Pool() == nil {
		return false, errSchedulerTrafficDatabaseMissing
	}
	tag, err := Pool().Exec(ctx, `
		UPDATE instances SET traffic_status = 'WARNING', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted' AND traffic_status = 'NORMAL'`, instanceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseUserTrafficWarningLease mirrors releaseUserTrafficWarningLease.
//
// The WHERE clause repeats the claim timestamp: if another tick already claimed the
// warning again, this rollback must not clobber the newer claim.
func ReleaseUserTrafficWarningLease(
	ctx context.Context,
	userID int32,
	claimedAt time.Time,
	previousSentAt *time.Time,
	previousStatus models.TrafficStatus,
) error {
	if Pool() == nil {
		return errSchedulerTrafficDatabaseMissing
	}
	_, err := Pool().Exec(ctx, `
		UPDATE user_quotas
		   SET traffic_warning_sent_at = $3, traffic_status = $4
		 WHERE user_id = $1 AND traffic_warning_sent_at = $2 AND traffic_status = 'WARNING'`,
		userID, claimedAt.UTC(), previousSentAt, string(previousStatus))
	return err
}

// ReleaseInstanceTrafficWarningLease mirrors releaseInstanceTrafficWarningLease.
func ReleaseInstanceTrafficWarningLease(ctx context.Context, instanceID int32) error {
	if Pool() == nil {
		return errSchedulerTrafficDatabaseMissing
	}
	_, err := Pool().Exec(ctx, `
		UPDATE instances SET traffic_status = 'NORMAL', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted' AND traffic_status = 'WARNING'`, instanceID)
	return err
}

// TrafficResetHost mirrors the {id, name} projection of the daily reset query.
type TrafficResetHost struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// TrafficNotificationContextRow mirrors one element of the projection in
// resolveTrafficNotificationContext.
type TrafficNotificationContextRow struct {
	PackageLimitsIngress  *string
	PackageLimitsEgress   *string
	PlanTrafficLimitSpeed *string
	TrafficResetDay       int
}

// TrafficNotificationContextRows mirrors
// `prisma.instance.findMany({ where: { userId, status: { not: 'deleted' }, name?,
// host: { name? } }, select: { package, packagePlan, host }, orderBy: { id: 'asc' } })`.
//
// The Prisma relations are `host` (required → INNER JOIN) and
// `package`/`packagePlan` (optional → LEFT JOIN). Empty filter strings mean "not
// provided", matching the TypeScript's conditional spread.
func TrafficNotificationContextRows(
	ctx context.Context,
	userID int32,
	instanceName, hostName string,
) ([]TrafficNotificationContextRow, error) {
	if Pool() == nil {
		return nil, errSchedulerTrafficDatabaseMissing
	}

	query := `
		SELECT p.limits_ingress, p.limits_egress, pp.traffic_limit_speed, h.traffic_reset_day
		  FROM instances i
		  JOIN hosts h ON h.id = i.host_id
		  LEFT JOIN packages p ON p.id = i.package_id
		  LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		 WHERE i.user_id = $1 AND i.status <> 'deleted'`
	args := []any{userID}
	if instanceName != "" {
		query += ` AND i.name = $2`
		args = append(args, instanceName)
	}
	if hostName != "" {
		if instanceName != "" {
			query += ` AND h.name = $3`
		} else {
			query += ` AND h.name = $2`
		}
		args = append(args, hostName)
	}
	query += ` ORDER BY i.id ASC`

	rows, err := Pool().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]TrafficNotificationContextRow, 0, 4)
	for rows.Next() {
		var row TrafficNotificationContextRow
		if err := rows.Scan(&row.PackageLimitsIngress, &row.PackageLimitsEgress,
			&row.PlanTrafficLimitSpeed, &row.TrafficResetDay); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// TrafficHostsByResetDay mirrors
// `prisma.host.findMany({ where: { trafficResetDay: today }, select: { id, name } })`.
func TrafficHostsByResetDay(ctx context.Context, day int) ([]TrafficResetHost, error) {
	if Pool() == nil {
		return nil, errSchedulerTrafficDatabaseMissing
	}
	rows, err := Pool().Query(ctx, `SELECT id, name FROM hosts WHERE traffic_reset_day = $1 ORDER BY id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := make([]TrafficResetHost, 0, 4)
	for rows.Next() {
		var host TrafficResetHost
		if err := rows.Scan(&host.ID, &host.Name); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}
