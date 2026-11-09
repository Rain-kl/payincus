// Port of the database-facing halves of server/src/routes/egress.ts and
// server/src/lib/egress/{package,sync,realtime}.ts.
//
// This module owns the network-policy rows that decide which firewall/ACL a node
// applies, so every statement below is a security control:
//
//   - the whole read-modify-write of HostNetworkPolicy runs under the same
//     transaction-scoped advisory lock (74201) the TypeScript uses
//   - the host row is locked FOR UPDATE so two admins cannot interleave a
//     revision check and an upsert
//   - a proxy node may only be deleted when no profile still names it
//   - a policy may only bind an ACTIVE profile that authorizes that host
//
// The instruction DOCUMENT builder (networkInstruction / packageEgressInstruction)
// lives in routes/egress.go instead, because it needs lib's egress validators and
// the db package must not import lib (lib -> db would cycle).
package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// EgressAdvisoryLockNamespace is the network-configuration advisory lock key.
//
// The TypeScript uses the SINGLE-argument form `pg_advisory_xact_lock(74201)`
// (a bigint key). db.AdvisoryTransactionLock takes the TWO-argument form, which
// PostgreSQL keeps in a different lock space, so this module must issue the
// one-argument statement itself rather than reusing that helper — otherwise the
// Go lock would not exclude the Node lock (or vice versa) during the parallel-run
// window.
const EgressAdvisoryLockNamespace int64 = 74201

// egressAdvisoryLock takes the transaction-scoped network lock. It must run on the
// same pgx.Tx as the work it guards; otherwise the lock is released immediately.
func egressAdvisoryLock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock($1::bigint)", EgressAdvisoryLockNamespace)
	return err
}

// EgressInputError mirrors lib.NetworkInputError at the database boundary: the
// routes translate it back into a 400 with the message verbatim.
type EgressInputError struct {
	Message string
}

// Error implements error.
func (e *EgressInputError) Error() string { return e.Message }

func newEgressInputError(message string) *EgressInputError {
	return &EgressInputError{Message: message}
}

// EgressProxyNode is the projection every egress query needs from proxy_nodes.
type EgressProxyNode struct {
	ID              int32
	Name            string
	Protocol        string
	Server          string
	Port            int32
	Fingerprint     string
	ConfigEncrypted string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EgressHost is the projection the egress and storage routes need from hosts.
type EgressHost struct {
	ID            int32
	Name          string
	UserID        int32
	URL           string
	CertPath      *string
	KeyPath       *string
	TunnelEnabled bool
	TargetHost    string
	TargetPort    int32
}

// EgressHostAgent is the projection read by the network-control checks.
type EgressHostAgent struct {
	HostID       int32
	Enabled      bool
	Version      *string
	Capabilities []byte
	LastSeenAt   *time.Time
}

// EgressHostNetworkPolicy mirrors the HostNetworkPolicy row without the relation
// fields. Times are kept raw so the caller can format them for the wire.
type EgressHostNetworkPolicy struct {
	HostID          int32
	Revision        int32
	EgressMode      string
	EgressProfileID *int32
	Firewall        []byte
	AppliedRevision int32
	Status          []byte
	ReportedAt      *time.Time
	UpdatedAt       time.Time
}

// EgressHostRef mirrors `{ id, name }`.
type EgressHostRef struct {
	ID   int32
	Name string
}

// EgressAttachmentRow is one instance_egress_attachments row joined with the
// instance status, which is all packageEgressInstruction reads.
type EgressAttachmentRow struct {
	ID             int32
	InstanceID     int32
	IncusName      string
	Mode           string
	ProfileID      *int32
	Generation     int32
	Nics           []byte
	InstanceStatus string
}

// EgressProxyNodeList mirrors `proxyNode.findMany({ orderBy: { id: 'asc' }, take })`.
func EgressProxyNodeList(ctx context.Context, take int) ([]EgressProxyNode, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := pool.Query(ctx, `SELECT id, name, protocol, server, port, fingerprint,
		config_encrypted, created_at, updated_at
		FROM proxy_nodes ORDER BY id ASC LIMIT $1`, take)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]EgressProxyNode, 0, 16)
	for rows.Next() {
		var node EgressProxyNode
		if err := rows.Scan(&node.ID, &node.Name, &node.Protocol, &node.Server, &node.Port,
			&node.Fingerprint, &node.ConfigEncrypted, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// EgressProxyNodesByIDs mirrors `proxyNode.findMany({ where: { id: { in: ids } } })`.
func EgressProxyNodesByIDs(ctx context.Context, ids []int32) ([]EgressProxyNode, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := pool.Query(ctx, `SELECT id, name, protocol, server, port, fingerprint,
		config_encrypted, created_at, updated_at
		FROM proxy_nodes WHERE id = ANY($1::int4[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]EgressProxyNode, 0, len(ids))
	for rows.Next() {
		var node EgressProxyNode
		if err := rows.Scan(&node.ID, &node.Name, &node.Protocol, &node.Server, &node.Port,
			&node.Fingerprint, &node.ConfigEncrypted, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// EgressProxyNodeInsert mirrors `proxyNode.createMany({ data: [row], skipDuplicates: true })`
// and returns the number of rows actually inserted (0 or 1).
func EgressProxyNodeInsert(ctx context.Context, node EgressProxyNode) (int64, error) {
	if pool == nil {
		return 0, errors.New("database not initialized")
	}
	tag, err := pool.Exec(ctx, `INSERT INTO proxy_nodes
		(name, protocol, server, port, fingerprint, config_encrypted, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT DO NOTHING`,
		node.Name, node.Protocol, node.Server, node.Port, node.Fingerprint, node.ConfigEncrypted)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// EgressProxyNodeDelete mirrors the delete transaction in
// DELETE /api/admin/proxy-nodes/:id. It returns "used", "ok" or "missing".
func EgressProxyNodeDelete(ctx context.Context, id int32) (string, error) {
	if pool == nil {
		return "", errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := egressAdvisoryLock(ctx, tx); err != nil {
		return "", err
	}

	// Prisma: egressProfile.count({ where: { egress: { path: ['proxyNodeIds'], array_contains: [id] } } })
	var referenced int64
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*)::bigint FROM egress_profiles
		 WHERE (egress -> 'proxyNodeIds') @> jsonb_build_array($1::int4)`, id).Scan(&referenced); err != nil {
		return "", err
	}
	if referenced > 0 {
		return "used", nil
	}

	tag, err := tx.Exec(ctx, `DELETE FROM proxy_nodes WHERE id = $1`, id)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "missing", nil
	}
	return "ok", nil
}

// EgressHostsWithAgentCapability mirrors
// `host.findMany({ where: { agent: { enabled: true, capabilities: { array_contains: [capability] } } },
// select: { id, name }, orderBy: { id: 'asc' }, take })`.
func EgressHostsWithAgentCapability(ctx context.Context, capability string, take int) ([]EgressHostRef, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := pool.Query(ctx, `SELECT h.id, h.name
		FROM hosts h JOIN host_agents a ON a.host_id = h.id
		WHERE a.enabled = true AND a.capabilities @> jsonb_build_array($1::text)
		ORDER BY h.id ASC LIMIT $2`, capability, take)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := make([]EgressHostRef, 0, 16)
	for rows.Next() {
		var host EgressHostRef
		if err := rows.Scan(&host.ID, &host.Name); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// EgressHostByID mirrors `host.findUnique({ where: { id } })` for the columns the
// egress and storage modules read.
func EgressHostByID(ctx context.Context, id int32) (*EgressHost, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	var host EgressHost
	err := pool.QueryRow(ctx, `SELECT id, name, user_id, url, cert_path, key_path,
		tunnel_enabled, target_host, target_port
		FROM hosts WHERE id = $1`, id).
		Scan(&host.ID, &host.Name, &host.UserID, &host.URL, &host.CertPath, &host.KeyPath,
			&host.TunnelEnabled, &host.TargetHost, &host.TargetPort)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &host, nil
}

// EgressHostAgentGet mirrors `hostAgent.findUnique({ where: { hostId } })`.
func EgressHostAgentGet(ctx context.Context, hostID int32) (*EgressHostAgent, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	var agent EgressHostAgent
	var capabilities []byte
	err := pool.QueryRow(ctx, `SELECT host_id, enabled, version, capabilities, last_seen_at
		FROM host_agents WHERE host_id = $1`, hostID).
		Scan(&agent.HostID, &agent.Enabled, &agent.Version, &capabilities, &agent.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	agent.Capabilities = capabilities
	return &agent, nil
}

// egressScanHostNetworkPolicy reads the policy columns in model order.
func egressScanHostNetworkPolicy(scanner interface {
	Scan(dest ...any) error
}) (*EgressHostNetworkPolicy, error) {
	var policy EgressHostNetworkPolicy
	err := scanner.Scan(&policy.HostID, &policy.Revision, &policy.EgressMode, &policy.EgressProfileID,
		&policy.Firewall, &policy.AppliedRevision, &policy.Status, &policy.ReportedAt, &policy.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

const egressHostNetworkPolicyColumns = `host_id, revision, egress_mode, egress_profile_id,
	firewall, applied_revision, status, reported_at, updated_at`

// EgressHostNetworkPolicyGet mirrors `hostNetworkPolicy.findUnique({ where: { hostId } })`.
func EgressHostNetworkPolicyGet(ctx context.Context, hostID int32) (*EgressHostNetworkPolicy, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return egressScanHostNetworkPolicy(pool.QueryRow(ctx,
		`SELECT `+egressHostNetworkPolicyColumns+` FROM host_network_policies WHERE host_id = $1`, hostID))
}

// egressHostNetworkPolicyTx is the same read inside a transaction.
func egressHostNetworkPolicyTx(ctx context.Context, tx pgx.Tx, hostID int32) (*EgressHostNetworkPolicy, error) {
	return egressScanHostNetworkPolicy(tx.QueryRow(ctx,
		`SELECT `+egressHostNetworkPolicyColumns+` FROM host_network_policies WHERE host_id = $1`, hostID))
}

// EgressNetworkBinding mirrors the `{ egressMode, egressProfileId }` selection.
type EgressNetworkBinding struct {
	EgressMode      string
	EgressProfileID *int32
}

// EgressNetworkPolicyUpdateInput mirrors the PUT /hosts/:id/network-policy/:section
// transaction input. Exactly one of Selection / Firewall is used.
type EgressNetworkPolicyUpdateInput struct {
	HostID           int32
	ExpectedRevision int32
	Selection        *EgressNetworkBinding
	Firewall         []byte
	DefaultFirewall  []byte
}

// EgressNetworkPolicyUpdate mirrors the transaction of
// PUT /api/hosts/:id/network-policy/{egress,firewall}. It returns
// "missing", "conflict", "nodes" or "ok".
func EgressNetworkPolicyUpdate(ctx context.Context, input EgressNetworkPolicyUpdateInput) (string, error) {
	if pool == nil {
		return "", errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := egressAdvisoryLock(ctx, tx); err != nil {
		return "", err
	}

	// `SELECT id FROM hosts WHERE id = ${request.params.id} FOR UPDATE`
	var lockedHostID int32
	err = tx.QueryRow(ctx, `SELECT id FROM hosts WHERE id = $1 FOR UPDATE`, input.HostID).Scan(&lockedHostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}

	current, err := egressHostNetworkPolicyTx(ctx, tx, input.HostID)
	if err != nil {
		return "", err
	}
	currentRevision := int32(0)
	if current != nil {
		currentRevision = current.Revision
	}
	// `(current?.revision ?? 0) !== request.body.revision`
	if currentRevision != input.ExpectedRevision {
		return "conflict", nil
	}

	if input.Selection != nil && input.Selection.EgressProfileID != nil && *input.Selection.EgressProfileID != 0 {
		profile, err := egressProfileByIDTx(ctx, tx, *input.Selection.EgressProfileID)
		if err != nil {
			return "", err
		}
		// `if (!profile?.active || !profile.allowedHostIds.includes(request.params.id)) return 'nodes'`
		if profile == nil || !profile.Active || !egressProfilesContainsHost(profile.AllowedHostIDs, input.HostID) {
			return "nodes", nil
		}
	}

	if input.Selection != nil {
		_, err = tx.Exec(ctx, `INSERT INTO host_network_policies
			(host_id, firewall, egress_mode, egress_profile_id, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (host_id) DO UPDATE
			SET egress_mode = EXCLUDED.egress_mode,
			    egress_profile_id = EXCLUDED.egress_profile_id,
			    revision = host_network_policies.revision + 1,
			    updated_at = NOW()`,
			input.HostID, string(input.DefaultFirewall), input.Selection.EgressMode, input.Selection.EgressProfileID)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO host_network_policies (host_id, firewall, updated_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (host_id) DO UPDATE
			SET firewall = EXCLUDED.firewall,
			    revision = host_network_policies.revision + 1,
			    updated_at = NOW()`, input.HostID, string(input.Firewall))
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return "ok", nil
}

// EgressHostNetworkPolicyBump mirrors bumpEgressHost(tx, hostId): it creates the
// row (revision falls back to the schema default of 1) or increments the revision
// so the node re-syncs its policy.
func EgressHostNetworkPolicyBump(ctx context.Context, tx pgx.Tx, hostID int32, defaultFirewall []byte) error {
	_, err := tx.Exec(ctx, `INSERT INTO host_network_policies (host_id, firewall, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (host_id) DO UPDATE
		SET revision = host_network_policies.revision + 1,
		    updated_at = NOW()`, hostID, string(defaultFirewall))
	return err
}

// EgressAttachmentsForHost mirrors
// `instanceEgressAttachment.findMany({ where: { hostId }, include: { instance: { select: { status } } },
// orderBy: { id: 'asc' } })`.
func EgressAttachmentsForHost(ctx context.Context, hostID int32) ([]EgressAttachmentRow, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := pool.Query(ctx, `SELECT a.id, a.instance_id, a.incus_name, a.mode, a.profile_id,
		a.generation, a.nics, i.status
		FROM instance_egress_attachments a
		JOIN instances i ON i.id = a.instance_id
		WHERE a.host_id = $1
		ORDER BY a.id ASC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]EgressAttachmentRow, 0, 16)
	for rows.Next() {
		var item EgressAttachmentRow
		if err := rows.Scan(&item.ID, &item.InstanceID, &item.IncusName, &item.Mode, &item.ProfileID,
			&item.Generation, &item.Nics, &item.InstanceStatus); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// EgressAuthorizedProfilesForHost mirrors
// `egressProfile.findMany({ where: { active: true, allowedHostIds: { has: hostId } }, orderBy: { id: 'asc' } })`.
func EgressAuthorizedProfilesForHost(ctx context.Context, hostID int32) ([]EgressProfilesRow, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return egressProfilesQuery(ctx, `SELECT `+egressProfilesColumns+`
		FROM egress_profiles
		WHERE active = true AND $1 = ANY(allowed_host_ids)
		ORDER BY id ASC`, hostID)
}

// EgressRecordNetworkStatus mirrors recordNetworkStatus(): every guard returns
// nil (nothing to write) exactly like the TypeScript's bare `return`.
func EgressRecordNetworkStatus(
	ctx context.Context,
	hostID int32,
	appliedRevision int32,
	status []byte,
	expectedRevision *int32,
) error {
	if pool == nil {
		return errors.New("database not initialized")
	}
	current, err := EgressHostNetworkPolicyGet(ctx, hostID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if expectedRevision != nil && current.Revision != *expectedRevision {
		return nil
	}
	if appliedRevision > current.Revision || appliedRevision < current.AppliedRevision {
		return nil
	}
	_, err = pool.Exec(ctx, `UPDATE host_network_policies
		SET applied_revision = $2, status = $3, reported_at = NOW(), updated_at = NOW()
		WHERE host_id = $1 AND revision = $4`, hostID, appliedRevision, string(status), current.Revision)
	return err
}

// egressProfilesContainsHost mirrors Array.prototype.includes.
func egressProfilesContainsHost(hostIDs []int32, hostID int32) bool {
	for _, candidate := range hostIDs {
		if candidate == hostID {
			return true
		}
	}
	return false
}
