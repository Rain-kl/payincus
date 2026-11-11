// Reads used by services/auto-policy-scheduler.ts.
//
// db/snapshots.go owns the route-facing snapshot CRUD (its list is ordered
// created_at DESC because the panel shows newest first). The auto-snapshot
// scheduler needs a different projection and the opposite order, so those two
// queries live here instead of forking the route contract.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// SnapshotPolicyHost mirrors the `instance.host` include of the snapshot policy
// query.
type SnapshotPolicyHost struct {
	ID       int32
	Name     string
	URL      string
	CertPath *string
	KeyPath  *string
	Status   models.HostStatus
}

// SnapshotPolicyInstance mirrors the `instance` include (plus host).
type SnapshotPolicyInstance struct {
	ID            int32
	IncusID       string
	Name          string
	UserID        int32
	HostID        int32
	Status        models.InstanceStatus
	SnapshotLimit *int32
	Host          SnapshotPolicyHost
}

// SnapshotPolicyDue mirrors one element of
// `prisma.snapshotPolicy.findMany({ where: { enabled: true, OR: [{ nextRunAt:
// null }, { nextRunAt: { lte: now } }] }, include: { instance: { include: { host
// } } } })`.
type SnapshotPolicyDue struct {
	PolicyID   int32
	InstanceID int32
	Enabled    bool
	NextRunAt  *time.Time
	Instance   SnapshotPolicyInstance
}

const snapshotPolicyJoin = `
	FROM snapshot_policies sp
	JOIN instances i ON i.id = sp.instance_id
	JOIN hosts h ON h.id = i.host_id`

const snapshotPolicyJoinColumns = `
	sp.id, sp.instance_id, sp.enabled, sp.next_run_at,
	i.id, i.incus_id, i.name, i.user_id, i.host_id, i.status::text, i.snapshot_limit,
	h.id, h.name, h.url, h.cert_path, h.key_path, h.status::text`

func scanSnapshotPolicyDue(scan func(...any) error) (SnapshotPolicyDue, error) {
	var (
		policy   SnapshotPolicyDue
		instance SnapshotPolicyInstance
		host     SnapshotPolicyHost
		status   string
		hostStat string
	)
	err := scan(
		&policy.PolicyID, &policy.InstanceID, &policy.Enabled, &policy.NextRunAt,
		&instance.ID, &instance.IncusID, &instance.Name, &instance.UserID, &instance.HostID, &status, &instance.SnapshotLimit,
		&host.ID, &host.Name, &host.URL, &host.CertPath, &host.KeyPath, &hostStat,
	)
	if err != nil {
		return SnapshotPolicyDue{}, err
	}
	instance.Status = models.InstanceStatus(status)
	host.Status = models.HostStatus(hostStat)
	instance.Host = host
	policy.Instance = instance
	return policy, nil
}

// SnapshotsListDuePolicies mirrors the scheduler's candidate query.
func SnapshotsListDuePolicies(ctx context.Context, now time.Time) ([]SnapshotPolicyDue, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx,
		`SELECT `+snapshotPolicyJoinColumns+snapshotPolicyJoin+`
		  WHERE sp.enabled = true AND (sp.next_run_at IS NULL OR sp.next_run_at <= $1)
		  ORDER BY sp.next_run_at ASC NULLS FIRST, sp.id ASC`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	policies := make([]SnapshotPolicyDue, 0, 8)
	for rows.Next() {
		policy, err := scanSnapshotPolicyDue(rows.Scan)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// SnapshotsGetPolicyWithInstance mirrors the re-read inside
// executeLockedAutoSnapshot (`findUnique({ where: { instanceId }, include: {
// instance: { include: { host } } } })`).
func SnapshotsGetPolicyWithInstance(ctx context.Context, instanceID int32) (*SnapshotPolicyDue, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	row := Pool().QueryRow(ctx,
		`SELECT `+snapshotPolicyJoinColumns+snapshotPolicyJoin+` WHERE sp.instance_id = $1`, instanceID)
	policy, err := scanSnapshotPolicyDue(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &policy, nil
}

// SnapshotsListByInstanceIDAsc mirrors the scheduler's
// `prisma.snapshot.findMany({ where: { instanceId }, orderBy: { createdAt: 'asc' } })`:
// the oldest row is the first candidate for rotation.
func SnapshotsListByInstanceIDAsc(ctx context.Context, instanceID int32) ([]SnapshotsSnapshot, error) {
	if Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := Pool().Query(ctx,
		`SELECT `+snapshotsColumns+` FROM snapshots WHERE instance_id = $1 ORDER BY created_at ASC, id ASC`,
		instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snapshots := make([]SnapshotsSnapshot, 0, 8)
	for rows.Next() {
		var (
			snapshot    SnapshotsSnapshot
			description *string
			stateful    bool
			createdAt   time.Time
		)
		if err := rows.Scan(&snapshot.ID, &snapshot.InstanceID, &snapshot.IncusName, &snapshot.Name,
			&description, &stateful, &snapshot.Size, &createdAt); err != nil {
			return nil, err
		}
		snapshot.Description = description
		snapshot.Stateful = stateful
		snapshot.CreatedAt = createdAt
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}
