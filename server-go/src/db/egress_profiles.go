// Port of the database-facing halves of server/src/routes/egress-profiles.ts and
// server/src/lib/egress/{attachments,package}.ts.
//
// Egress profiles are shared policy identities: packages, instances and host
// bindings reference a profile id, never a copy of its contents. That is why the
// save path updates a profile in place, refuses to revoke a host that is still
// referenced, and bumps the revision of every host the change touches (added,
// kept AND revoked) so each node re-syncs instead of waiting for its first
// instance.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// EgressProfilesRow is the egress_profiles projection the routes and the
// instruction builder consume. AllowedHostIDs is a Postgres int4[] scanned
// through pgx, which keeps the order the column stores.
type EgressProfilesRow struct {
	ID             int32
	Name           string
	Active         bool
	Egress         []byte
	AllowedHostIDs []int32
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const egressProfilesColumns = `id, name, active, egress, allowed_host_ids, created_at, updated_at`

// egressProfilesQuery runs a profile SELECT and scans every row.
func egressProfilesQuery(ctx context.Context, query string, args ...any) ([]EgressProfilesRow, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	profiles := make([]EgressProfilesRow, 0, 16)
	for rows.Next() {
		profile, err := egressScanProfileRow(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, *profile)
	}
	return profiles, rows.Err()
}

func egressScanProfileRow(scanner interface {
	Scan(dest ...any) error
}) (*EgressProfilesRow, error) {
	var profile EgressProfilesRow
	var egress []byte
	if err := scanner.Scan(&profile.ID, &profile.Name, &profile.Active, &egress,
		&profile.AllowedHostIDs, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
		return nil, err
	}
	profile.Egress = egress
	return &profile, nil
}

// EgressProfilesByIDs mirrors `egressProfile.findMany({ where: { id: { in: ids } } })`.
func EgressProfilesByIDs(ctx context.Context, ids []int32) ([]EgressProfilesRow, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return egressProfilesQuery(ctx,
		`SELECT `+egressProfilesColumns+` FROM egress_profiles WHERE id = ANY($1::int4[])`, ids)
}

// EgressProfilesList mirrors `egressProfile.findMany({ orderBy: { id: 'desc' }, take })`.
func EgressProfilesList(ctx context.Context, take int) ([]EgressProfilesRow, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return egressProfilesQuery(ctx,
		`SELECT `+egressProfilesColumns+` FROM egress_profiles ORDER BY id DESC LIMIT $1`, take)
}

// EgressProfilesOwnedHostIDs mirrors
// `host.findMany({ where: { userId }, select: { id } }).map(host => host.id)`.
func EgressProfilesOwnedHostIDs(ctx context.Context, userID int32) ([]int32, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := pool.Query(ctx, `SELECT id FROM hosts WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int32, 0, 8)
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// EgressProfilesInstanceView is the projection of
// `instance.findUnique({ where: { id }, include: { egressProfile: true } })`.
type EgressProfilesInstanceView struct {
	ID                int32
	UserID            int32
	HostID            int32
	Status            string
	IncusID           string
	EgressMode        string
	EgressProfileID   *int32
	EgressProfileName *string
}

// EgressProfilesInstanceEgress mirrors the instance lookup of
// GET /api/instances/:id/egress.
func EgressProfilesInstanceEgress(ctx context.Context, id int32) (*EgressProfilesInstanceView, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	var view EgressProfilesInstanceView
	err := pool.QueryRow(ctx, `SELECT i.id, i.user_id, i.host_id, i.status, i.incus_id,
		i.egress_mode, i.egress_profile_id, p.name
		FROM instances i
		LEFT JOIN egress_profiles p ON p.id = i.egress_profile_id
		WHERE i.id = $1`, id).
		Scan(&view.ID, &view.UserID, &view.HostID, &view.Status, &view.IncusID,
			&view.EgressMode, &view.EgressProfileID, &view.EgressProfileName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &view, nil
}

// EgressProfilesAttachment is the projection of
// `instanceEgressAttachment.findUnique({ where: { hostId_incusName: { hostId, incusName } } })`.
type EgressProfilesAttachment struct {
	IncusName  string
	ProfileID  *int32
	Generation int32
}

// EgressProfilesAttachmentByHostIncusName reads the attachment row.
func EgressProfilesAttachmentByHostIncusName(ctx context.Context, hostID int32, incusName string) (*EgressProfilesAttachment, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	var attachment EgressProfilesAttachment
	err := pool.QueryRow(ctx, `SELECT incus_name, profile_id, generation
		FROM instance_egress_attachments WHERE host_id = $1 AND incus_name = $2`, hostID, incusName).
		Scan(&attachment.IncusName, &attachment.ProfileID, &attachment.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attachment, nil
}

// egressProfileByIDTx reads one profile inside a transaction.
func egressProfileByIDTx(ctx context.Context, tx pgx.Tx, id int32) (*EgressProfilesRow, error) {
	profile, err := egressScanProfileRow(tx.QueryRow(ctx,
		`SELECT `+egressProfilesColumns+` FROM egress_profiles WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// EgressProfilesSaveInput mirrors the POST/PUT /api/admin/egress-profiles body
// after validation (the route has already run validateEgress and name.trim()).
type EgressProfilesSaveInput struct {
	Editing         bool
	ID              int32
	Name            string
	HostIDs         []int32
	ProxyNodeIDs    []int32
	Egress          []byte
	DefaultFirewall []byte
}

// EgressProfilesSaveResult mirrors the transaction's `{ profile, targets }` or
// `null` when an edit names a profile that no longer exists.
type EgressProfilesSaveResult struct {
	Found   bool
	Profile *EgressProfilesRow
	Targets []int32
}

// EgressProfilesSave runs the whole create/update transaction:
//
//	advisory lock 74201
//	→ referenced proxy nodes and hosts must exist
//	→ load the profile being edited (404 when absent)
//	→ no host that still has a package, instance, host binding or attachment may
//	  be revoked
//	→ upsert the profile
//	→ bump every affected host revision (Node heartbeat retries a failed push, so
//	  the route never blocks on an offline node)
func EgressProfilesSave(ctx context.Context, input EgressProfilesSaveInput) (EgressProfilesSaveResult, error) {
	if pool == nil {
		return EgressProfilesSaveResult{}, errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return EgressProfilesSaveResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := egressAdvisoryLock(ctx, tx); err != nil {
		return EgressProfilesSaveResult{}, err
	}

	proxyNodeIDs := input.ProxyNodeIDs
	if proxyNodeIDs == nil {
		proxyNodeIDs = []int32{}
	}
	hostIDs := input.HostIDs
	if hostIDs == nil {
		hostIDs = []int32{}
	}

	// `if (await tx.proxyNode.count({ where: { id: { in: egress.proxyNodeIds } } }) !==
	//     egress.proxyNodeIds.length || await tx.host.count({ where: { id: { in: request.body.hostIds } } }) !==
	//     request.body.hostIds.length) throw new NetworkInputError('代理或授权宿主节点不存在')`
	var proxyCount, hostCount int64
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*)::bigint FROM proxy_nodes WHERE id = ANY($1::int4[])`, proxyNodeIDs).Scan(&proxyCount); err != nil {
		return EgressProfilesSaveResult{}, err
	}
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*)::bigint FROM hosts WHERE id = ANY($1::int4[])`, hostIDs).Scan(&hostCount); err != nil {
		return EgressProfilesSaveResult{}, err
	}
	if proxyCount != int64(len(proxyNodeIDs)) || hostCount != int64(len(hostIDs)) {
		return EgressProfilesSaveResult{}, newEgressInputError("代理或授权宿主节点不存在")
	}

	var existing *EgressProfilesRow
	if input.Editing {
		existing, err = egressProfileByIDTx(ctx, tx, input.ID)
		if err != nil {
			return EgressProfilesSaveResult{}, err
		}
		if existing == nil {
			return EgressProfilesSaveResult{Found: false}, nil
		}
	}

	used := make([]int32, 0, 8)
	if existing != nil {
		queries := []string{
			`SELECT host_id FROM instance_egress_attachments WHERE profile_id = $1`,
			`SELECT host_id FROM host_network_policies WHERE egress_profile_id = $1`,
			`SELECT ph.host_id FROM package_hosts ph
			 JOIN packages p ON p.id = ph.package_id
			 WHERE p.egress_profile_id = $1`,
			`SELECT host_id FROM instances WHERE egress_profile_id = $1 AND status <> 'deleted'`,
		}
		for _, query := range queries {
			rows, queryErr := tx.Query(ctx, query, existing.ID)
			if queryErr != nil {
				return EgressProfilesSaveResult{}, queryErr
			}
			for rows.Next() {
				var hostID int32
				if scanErr := rows.Scan(&hostID); scanErr != nil {
					rows.Close()
					return EgressProfilesSaveResult{}, scanErr
				}
				used = append(used, hostID)
			}
			rows.Close()
			if queryErr := rows.Err(); queryErr != nil {
				return EgressProfilesSaveResult{}, queryErr
			}
		}
	}

	affected := egressProfilesUniqueSorted(used)
	for _, hostID := range affected {
		if !egressProfilesContainsHost(hostIDs, hostID) {
			return EgressProfilesSaveResult{}, newEgressInputError("授权宿主范围仍有套餐、实例或节点引用，不能移除")
		}
	}

	// Targets: still-referenced hosts, the newly authorized ones, and the ones whose
	// authorization was revoked (existing.allowedHostIds).
	targets := affected
	if existing != nil {
		targets = egressProfilesUniqueSorted(append(append([]int32{}, affected...), existing.AllowedHostIDs...))
	}
	targets = egressProfilesUniqueSorted(append(targets, hostIDs...))

	var saved *EgressProfilesRow
	if existing != nil {
		saved, err = egressScanProfileRow(tx.QueryRow(ctx, `UPDATE egress_profiles
			SET name = $2, allowed_host_ids = $3, egress = $4, updated_at = NOW()
			WHERE id = $1
			RETURNING `+egressProfilesColumns, existing.ID, input.Name, hostIDs, string(input.Egress)))
	} else {
		saved, err = egressScanProfileRow(tx.QueryRow(ctx, `INSERT INTO egress_profiles
			(name, allowed_host_ids, egress, updated_at)
			VALUES ($1, $2, $3, NOW())
			RETURNING `+egressProfilesColumns, input.Name, hostIDs, string(input.Egress)))
	}
	if err != nil {
		return EgressProfilesSaveResult{}, err
	}

	for _, hostID := range targets {
		if err := EgressHostNetworkPolicyBump(ctx, tx, hostID, input.DefaultFirewall); err != nil {
			return EgressProfilesSaveResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return EgressProfilesSaveResult{}, err
	}
	return EgressProfilesSaveResult{Found: true, Profile: saved, Targets: targets}, nil
}

// EgressProfilesDelete mirrors the DELETE /api/admin/egress-profiles/:id
// transaction. referenced reports the `used === -1` branch (HTTP 409); otherwise
// count is the number of deleted rows (0 → 404).
func EgressProfilesDelete(ctx context.Context, id int32) (count int64, referenced bool, err error) {
	if pool == nil {
		return 0, false, errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := egressAdvisoryLock(ctx, tx); err != nil {
		return 0, false, err
	}

	// package + instance + hostNetworkPolicy + instanceEgressAttachment counts.
	counts := []string{
		`SELECT COUNT(*)::bigint FROM packages WHERE egress_profile_id = $1`,
		`SELECT COUNT(*)::bigint FROM instances WHERE egress_profile_id = $1`,
		`SELECT COUNT(*)::bigint FROM host_network_policies WHERE egress_profile_id = $1`,
		`SELECT COUNT(*)::bigint FROM instance_egress_attachments WHERE profile_id = $1`,
	}
	var used int64
	for _, query := range counts {
		var value int64
		if err := tx.QueryRow(ctx, query, id).Scan(&value); err != nil {
			return 0, false, err
		}
		used += value
	}
	if used > 0 {
		return 0, true, nil
	}

	tag, err := tx.Exec(ctx, `DELETE FROM egress_profiles WHERE id = $1`, id)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return tag.RowsAffected(), false, nil
}

// egressProfilesUniqueSorted mirrors `[...new Set(values)].sort((a, b) => a - b)`.
func egressProfilesUniqueSorted(values []int32) []int32 {
	seen := make(map[int32]bool, len(values))
	unique := make([]int32, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
}

// EgressProfilesEgressJSON normalises a stored profile egress document. It is
// exported for the routes that must echo the raw JSON value.
func EgressProfilesEgressJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(raw)
}
