// Port of server/src/db/public-ipv4.ts (276 lines, 10 exports).
//
// Public IPv4 addresses are the second half of instance network delivery: the
// reservation runs INSIDE the instance-creation transaction (the instance id it
// stamps does not exist yet) and the release runs inside the destroy transaction.
// Two invariants therefore shape this file:
//
//  1. `FOR UPDATE SKIP LOCKED` on the free-address query is load-bearing. It is a
//     row lock, not an advisory lock: two concurrent creations take different rows
//     instead of queueing, and a row that another transaction is already holding
//     is skipped rather than waited on. Do not "simplify" it into an advisory lock
//     or a plain SELECT — the latter hands the same address to both callers.
//  2. Status is a state machine: free → assigned (assign, sets instance_id and
//     assigned_at, clears released_at), assigned → free (release, clears
//     instance_id and stamps released_at), free/disabled ↔ (admin set-status),
//     assigned → disabled is impossible (admin paths reject assigned rows), and a
//     free row whose gateway (own or pool) is empty is auto-disabled with an
//     explanatory note instead of being handed out.
//
// Every timestamp in the raw SQL is NOW() because the TypeScript wrote NOW(); no
// Go time value enters these statements except the UTC clock used where the
// TypeScript used a JS Date.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Sentinels mirror the string errors the TypeScript throws. routes/hosts.ts
// compares error.message, so the texts are part of the contract:
// PUBLIC_IPV4_POOL_NOT_FOUND → 404, PUBLIC_IPV4_ADDRESS_NOT_FOUND → 404,
// PUBLIC_IPV4_ADDRESS_ASSIGNED → 400.
var (
	ErrPublicIpv4PoolNotFound             = errors.New("PUBLIC_IPV4_POOL_NOT_FOUND")
	ErrPublicIpv4PoolHasAssignedAddresses = errors.New("PUBLIC_IPV4_POOL_HAS_ASSIGNED_ADDRESSES")
	ErrPublicIpv4AddressNotFound          = errors.New("PUBLIC_IPV4_ADDRESS_NOT_FOUND")
	ErrPublicIpv4AddressAssigned          = errors.New("PUBLIC_IPV4_ADDRESS_ASSIGNED")
)

// publicIpv4DefaultDNS mirrors the `['1.1.1.1', '8.8.8.8']` fallback.
var publicIpv4DefaultDNS = []string{"1.1.1.1", "8.8.8.8"}

// PublicIpv4Querier is satisfied by *pgxpool.Pool and pgx.Tx, so the free-count
// query can run on either. Reserve and release deliberately require a pgx.Tx: the
// row lock and the status write must commit together with the instance row.
type PublicIpv4Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var _ PublicIpv4Querier = (*pgxpool.Pool)(nil)
var _ PublicIpv4Querier = (pgx.Tx)(nil)

// PublicIpv4Assignment mirrors PublicIpv4Assignment: what provisioning needs to
// configure the interface inside the instance.
type PublicIpv4Assignment struct {
	ID           int32    `json:"id"`
	Address      string   `json:"address"`
	PrefixLength int32    `json:"prefixLength"`
	Gateway      string   `json:"gateway"`
	DNS          []string `json:"dns"`
}

// PublicIpv4ReserveInput mirrors reservePublicIpv4ForInstance's input.
type PublicIpv4ReserveInput struct {
	HostID     int32
	InstanceID int32
}

// PublicIpv4CreatePoolInput mirrors CreatePublicIpv4PoolInput.
type PublicIpv4CreatePoolInput struct {
	HostID       int32
	Name         string
	CIDR         *string
	Gateway      string
	PrefixLength *int32
	DNS          []string
	Notes        *string
	Addresses    []string
}

// PublicIpv4AddAddressesInput mirrors addPublicIpv4Addresses' input.
type PublicIpv4AddAddressesInput struct {
	HostID    int32
	PoolID    int32
	Addresses []string
}

// PublicIpv4PoolUpdate mirrors updatePublicIpv4Pool's data object. Every field is
// optional, so pointers are enough; `notes` is nullable and uses the tri-state
// `Set`/`Value` split because `notes: null` is a real clear.
type PublicIpv4PoolUpdate struct {
	Name         *string
	Gateway      *string
	PrefixLength *int32
	DNS          []string
	DNSSet       bool
	Enabled      *bool
	Notes        InstanceOptional[*string]
}

// PublicIpv4PoolAddressRecord mirrors the `addresses` select of
// listPublicIpv4Pools: only the listed columns, camelCase, ISO timestamps.
type PublicIpv4PoolAddressRecord struct {
	ID           int32                          `json:"id"`
	Address      string                         `json:"address"`
	PrefixLength int32                          `json:"prefixLength"`
	Gateway      *string                        `json:"gateway"`
	DNS          json.RawMessage                `json:"dns"`
	Status       models.PublicIpv4AddressStatus `json:"status"`
	InstanceID   *int32                         `json:"instanceId"`
	AssignedAt   *string                        `json:"assignedAt"`
	ReleasedAt   *string                        `json:"releasedAt"`
	Notes        *string                        `json:"notes"`
	CreatedAt    string                         `json:"createdAt"`
	UpdatedAt    string                         `json:"updatedAt"`
}

// PublicIpv4PoolRecord mirrors the Prisma PublicIpv4Pool model WITHOUT the
// `addresses` relation — that is exactly what create/update return. The host
// relation is not included by the TypeScript either, so it is absent.
type PublicIpv4PoolRecord struct {
	ID           int32           `json:"id"`
	HostID       int32           `json:"hostId"`
	Name         string          `json:"name"`
	CIDR         *string         `json:"cidr"`
	Gateway      string          `json:"gateway"`
	PrefixLength int32           `json:"prefixLength"`
	DNS          json.RawMessage `json:"dns"`
	RouteMode    string          `json:"routeMode"`
	Enabled      bool            `json:"enabled"`
	Notes        *string         `json:"notes"`
	CreatedAt    string          `json:"createdAt"`
	UpdatedAt    string          `json:"updatedAt"`
}

// PublicIpv4PoolWithAddresses mirrors listPublicIpv4Pools: the pool fields followed
// by `addresses`, always present (an empty array for a pool with no addresses).
// encoding/json flattens the embedded record, so the promotion matches Prisma's
// scalar-then-relation order.
type PublicIpv4PoolWithAddresses struct {
	PublicIpv4PoolRecord
	Addresses []PublicIpv4PoolAddressRecord `json:"addresses"`
}

// publicIpv4PoolColumns is the pool projection, in Prisma schema order.
const publicIpv4PoolColumns = `id, host_id, name, cidr, gateway, prefix_length, dns,
	route_mode, enabled, notes, created_at, updated_at`

// publicIpv4PoolAddressColumns is the address projection used by the pool list.
const publicIpv4PoolAddressColumns = `id, address, prefix_length, gateway, dns, status,
	instance_id, assigned_at, released_at, notes, created_at, updated_at`

// publicIpv4ScanPoolRow scans one pool row.
func publicIpv4ScanPoolRow(scanner interface {
	Scan(dest ...any) error
}) (*PublicIpv4PoolRecord, error) {
	var (
		record  PublicIpv4PoolRecord
		created time.Time
		updated time.Time
		dns     []byte
	)
	err := scanner.Scan(&record.ID, &record.HostID, &record.Name, &record.CIDR,
		&record.Gateway, &record.PrefixLength, &dns, &record.RouteMode, &record.Enabled,
		&record.Notes, &created, &updated)
	if err != nil {
		return nil, err
	}
	record.DNS = publicIpv4RawJSON(dns, "[]")
	record.CreatedAt = instancesISOTime(created)
	record.UpdatedAt = instancesISOTime(updated)
	return &record, nil
}

// publicIpv4RawJSON normalises a jsonb column (JSON null / empty → fallback).
func publicIpv4RawJSON(raw []byte, fallback string) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(fallback)
	}
	return json.RawMessage(trimmed)
}

// publicIpv4NormalizeDNS mirrors normalizeDns: keep only non-empty strings from a
// JSON array; anything else (wrong type, null, non-array) becomes [].
func publicIpv4NormalizeDNS(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return []string{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			result = append(result, text)
		}
	}
	return result
}

// publicIpv4UniqueTrimmed mirrors
// `[...new Set((items || []).map(item => item.trim()).filter(Boolean))]`.
func publicIpv4UniqueTrimmed(items []string) []string {
	result := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		result = append(result, trimmed)
	}
	return result
}

// publicIpv4AssignmentDNS mirrors
// `dns = normalizeDns(selected.dns).length > 0 ? normalizeDns(selected.dns) : normalizeDns(selected.poolDns)`
// followed by `dns.length > 0 ? dns : ['1.1.1.1','8.8.8.8']`.
func publicIpv4AssignmentDNS(addressDNS, poolDNS []string) []string {
	dns := addressDNS
	if len(dns) == 0 {
		dns = poolDNS
	}
	if len(dns) == 0 {
		return append([]string{}, publicIpv4DefaultDNS...)
	}
	return append([]string{}, dns...)
}

// PublicIpv4AvailablePredicate is the "this address may be handed out" predicate,
// shared by the free-count and the reservation statements. Sharing the exact text
// keeps the two from drifting: a count that disagrees with the allocator either
// promises addresses that do not exist or hides ones that do.
const PublicIpv4AvailablePredicate = "a.status = 'free' AND (a.pool_id IS NULL OR p.enabled = true)"

// publicIpv4FreeCountQuery mirrors countFreePublicIpv4ForHost. A standalone row
// (pool_id IS NULL) always counts; a pooled row only while its pool is enabled.
const publicIpv4FreeCountQuery = `SELECT COUNT(*)::bigint AS count
	FROM public_ipv4_addresses a
	LEFT JOIN public_ipv4_pools p ON p.id = a.pool_id
	WHERE a.host_id = $1
		AND ` + PublicIpv4AvailablePredicate

// publicIpv4ReserveSelect mirrors reservePublicIpv4ForInstance's query. The clause
// order is part of the contract: the filters, then ORDER BY id ASC (deterministic,
// lowest id first), then FOR UPDATE SKIP LOCKED (never block on a row another
// transaction already holds), then LIMIT 1.
const publicIpv4ReserveSelect = `SELECT
		a.id,
		a.address,
		a.prefix_length AS "prefixLength",
		a.gateway,
		p.gateway AS "poolGateway",
		a.dns,
		p.dns AS "poolDns"
	FROM public_ipv4_addresses a
	LEFT JOIN public_ipv4_pools p ON p.id = a.pool_id
	WHERE a.host_id = $1
		AND ` + PublicIpv4AvailablePredicate + `
	ORDER BY a.id ASC
	FOR UPDATE SKIP LOCKED
	LIMIT 1`

// publicIpv4ReleaseUpdate mirrors releasePublicIpv4ForInstance. `WHERE … status =
// 'assigned'` is what makes a double release a no-op instead of rewriting
// released_at, and clearing instance_id is what makes the address reservable again.
const publicIpv4ReleaseUpdate = `UPDATE public_ipv4_addresses
	SET status = 'free',
		instance_id = NULL,
		released_at = NOW(),
		updated_at = NOW()
	WHERE instance_id = $1
		AND status = 'assigned'`

// ==================== state machine ====================

// PublicIpv4AddressAction names the transitions the TypeScript supports.
type PublicIpv4AddressAction string

const (
	// PublicIpv4ActionReserve is reservePublicIpv4ForInstance's write.
	PublicIpv4ActionReserve PublicIpv4AddressAction = "reserve"
	// PublicIpv4ActionRelease is releasePublicIpv4ForInstance's write.
	PublicIpv4ActionRelease PublicIpv4AddressAction = "release"
	// PublicIpv4ActionDisable is the auto-disable of a gateway-less free row.
	PublicIpv4ActionDisable PublicIpv4AddressAction = "disable"
	// PublicIpv4ActionAdminFree is setPublicIpv4AddressStatus(..., 'free').
	PublicIpv4ActionAdminFree PublicIpv4AddressAction = "admin_free"
	// PublicIpv4ActionAdminDisable is setPublicIpv4AddressStatus(..., 'disabled').
	PublicIpv4ActionAdminDisable PublicIpv4AddressAction = "admin_disable"
	// PublicIpv4ActionDelete is deletePublicIpv4Address.
	PublicIpv4ActionDelete PublicIpv4AddressAction = "delete"
)

// PublicIpv4TransitionAllowed reports whether the action may touch a row in the
// given status. The statements above encode the same rules in SQL; this is their
// documented Go view and the table the unit tests drive:
//
//	rereserve  assigned --(release)--> free --(reserve)--> assigned is allowed
//	disabled rows are never reserved (only admin may move them)
//	assigned rows cannot be deleted or admin-restatused
func PublicIpv4TransitionAllowed(status models.PublicIpv4AddressStatus, action PublicIpv4AddressAction) bool {
	switch action {
	case PublicIpv4ActionReserve:
		return status == models.PublicIpv4AddressStatusFree
	case PublicIpv4ActionRelease:
		return status == models.PublicIpv4AddressStatusAssigned
	case PublicIpv4ActionDisable:
		return status == models.PublicIpv4AddressStatusFree
	case PublicIpv4ActionAdminFree, PublicIpv4ActionAdminDisable, PublicIpv4ActionDelete:
		return status != models.PublicIpv4AddressStatusAssigned
	default:
		return false
	}
}

// PublicIpv4AddressCountsAsAvailable mirrors PublicIpv4AvailablePredicate in Go so
// the counting rules are unit-testable without a database. It must stay in lockstep
// with the SQL constant, which both the free-count and the reservation statements
// embed (TestPublicIpv4FreeAddressCounting enforces the pairing).
func PublicIpv4AddressCountsAsAvailable(
	status models.PublicIpv4AddressStatus,
	poolID *int32,
	poolEnabled bool,
) bool {
	if status != models.PublicIpv4AddressStatusFree {
		return false
	}
	if poolID == nil {
		return true
	}
	return poolEnabled
}

// PublicIpv4NextStatus returns the status an allowed action leaves behind
// (release → free, reserve → assigned, disable → disabled, delete → none).
func PublicIpv4NextStatus(action PublicIpv4AddressAction) (models.PublicIpv4AddressStatus, bool) {
	switch action {
	case PublicIpv4ActionReserve:
		return models.PublicIpv4AddressStatusAssigned, true
	case PublicIpv4ActionRelease, PublicIpv4ActionAdminFree:
		return models.PublicIpv4AddressStatusFree, true
	case PublicIpv4ActionDisable, PublicIpv4ActionAdminDisable:
		return models.PublicIpv4AddressStatusDisabled, true
	default:
		return "", false
	}
}

// ==================== reads ====================

func publicIpv4RequireDB() error {
	if Pool() == nil || DB == nil {
		return errors.New("public ipv4: database not initialized")
	}
	return nil
}

// CountFreePublicIpv4ForHost mirrors countFreePublicIpv4ForHost: free addresses of
// the host whose pool (when set) is enabled. An address with pool_id IS NULL is a
// standalone row and always counts.
func CountFreePublicIpv4ForHost(ctx context.Context, q PublicIpv4Querier, hostID int32) (int64, error) {
	if q == nil {
		if err := publicIpv4RequireDB(); err != nil {
			return 0, err
		}
		q = Pool()
	}
	var count int64
	err := q.QueryRow(ctx, publicIpv4FreeCountQuery, hostID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ReservePublicIpv4ForInstance mirrors reservePublicIpv4ForInstance. It must run
// on the instance transaction: the row lock taken here has to be held until the
// instance row commits. Returns nil, nil when the host has no usable free address.
func ReservePublicIpv4ForInstance(
	ctx context.Context,
	tx pgx.Tx,
	input PublicIpv4ReserveInput,
) (*PublicIpv4Assignment, error) {
	var (
		id           int32
		address      string
		prefixLength int32
		gateway      *string
		poolGateway  *string
		dns          []byte
		poolDNS      []byte
	)
	err := tx.QueryRow(ctx, publicIpv4ReserveSelect, input.HostID).
		Scan(&id, &address, &prefixLength, &gateway, &poolGateway, &dns, &poolDNS)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// `selected.gateway || selected.poolGateway` — empty string falls through.
	resolvedGateway := ""
	if gateway != nil {
		resolvedGateway = *gateway
	}
	if resolvedGateway == "" && poolGateway != nil {
		resolvedGateway = *poolGateway
	}
	if resolvedGateway == "" {
		// Auto-disable instead of handing out an address that cannot route. The
		// note append keeps any previous note and adds a newline only when one
		// already exists (COALESCE + CASE with E'\n').
		if _, err := tx.Exec(ctx, `UPDATE public_ipv4_addresses
			SET status = 'disabled',
				notes = COALESCE(notes, '') || CASE WHEN notes IS NULL OR notes = '' THEN '' ELSE E'\n' END || 'Auto-disabled: missing gateway',
				updated_at = NOW()
			WHERE id = $1`, id); err != nil {
			return nil, err
		}
		return nil, nil
	}

	if _, err := tx.Exec(ctx, `UPDATE public_ipv4_addresses
		SET status = 'assigned',
			instance_id = $2,
			assigned_at = NOW(),
			released_at = NULL,
			updated_at = NOW()
		WHERE id = $1`, id, input.InstanceID); err != nil {
		return nil, err
	}

	return &PublicIpv4Assignment{
		ID:           id,
		Address:      address,
		PrefixLength: prefixLength,
		Gateway:      resolvedGateway,
		DNS:          publicIpv4AssignmentDNS(publicIpv4NormalizeDNS(dns), publicIpv4NormalizeDNS(poolDNS)),
	}, nil
}

// ReleasePublicIpv4ForInstance mirrors releasePublicIpv4ForInstance: only rows in
// 'assigned' are released, so a repeated release is a no-op (idempotent) and the
// address becomes reservable again.
func ReleasePublicIpv4ForInstance(ctx context.Context, tx pgx.Tx, instanceID int32) error {
	_, err := tx.Exec(ctx, publicIpv4ReleaseUpdate, instanceID)
	return err
}

// ListPublicIpv4Pools mirrors listPublicIpv4Pools: pools of the host ordered by
// id, each with its addresses ordered by id.
func ListPublicIpv4Pools(ctx context.Context, hostID int32) ([]PublicIpv4PoolWithAddresses, error) {
	if err := publicIpv4RequireDB(); err != nil {
		return nil, err
	}

	rows, err := Pool().Query(ctx, `SELECT `+publicIpv4PoolColumns+`
		FROM public_ipv4_pools WHERE host_id = $1 ORDER BY id ASC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pools := make([]PublicIpv4PoolWithAddresses, 0, 4)
	poolIDs := make([]int32, 0, 4)
	for rows.Next() {
		pool, err := publicIpv4ScanPoolRow(rows)
		if err != nil {
			return nil, err
		}
		pools = append(pools, PublicIpv4PoolWithAddresses{
			PublicIpv4PoolRecord: *pool,
			Addresses:            []PublicIpv4PoolAddressRecord{},
		})
		poolIDs = append(poolIDs, pool.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(poolIDs) == 0 {
		return pools, nil
	}

	// pool_id is selected first for grouping only: it is not part of the wire
	// record (the TypeScript select omits it).
	addressRows, err := Pool().Query(ctx, `SELECT pool_id, `+publicIpv4PoolAddressColumns+`
		FROM public_ipv4_addresses WHERE pool_id = ANY($1) ORDER BY id ASC`, poolIDs)
	if err != nil {
		return nil, err
	}
	defer addressRows.Close()

	byPoolID := make(map[int32][]PublicIpv4PoolAddressRecord, len(poolIDs))
	for addressRows.Next() {
		var (
			record     PublicIpv4PoolAddressRecord
			status     string
			poolID     *int32
			assignedAt *time.Time
			releasedAt *time.Time
			createdAt  time.Time
			updatedAt  time.Time
			dns        []byte
		)
		if err := addressRows.Scan(&poolID, &record.ID, &record.Address, &record.PrefixLength,
			&record.Gateway, &dns, &status, &record.InstanceID, &assignedAt, &releasedAt,
			&record.Notes, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		record.DNS = publicIpv4RawJSON(dns, "[]")
		record.Status = models.PublicIpv4AddressStatus(status)
		record.AssignedAt = instancesOptionalISOTime(assignedAt)
		record.ReleasedAt = instancesOptionalISOTime(releasedAt)
		record.CreatedAt = instancesISOTime(createdAt)
		record.UpdatedAt = instancesISOTime(updatedAt)
		if poolID != nil {
			byPoolID[*poolID] = append(byPoolID[*poolID], record)
		}
	}
	if err := addressRows.Err(); err != nil {
		return nil, err
	}

	for index := range pools {
		if addresses := byPoolID[pools[index].ID]; addresses != nil {
			pools[index].Addresses = addresses
		}
	}
	return pools, nil
}

// ==================== writes ====================

// CreatePublicIpv4Pool mirrors createPublicIpv4Pool: insert the pool, then bulk
// insert its addresses (duplicates skipped by the (host_id, address) unique
// index), all in one transaction. The returned record carries no addresses,
// exactly like the Prisma create result.
func CreatePublicIpv4Pool(ctx context.Context, input PublicIpv4CreatePoolInput) (*PublicIpv4PoolRecord, error) {
	if err := publicIpv4RequireDB(); err != nil {
		return nil, err
	}

	addresses := publicIpv4UniqueTrimmed(input.Addresses)
	prefixLength := int32(32)
	if input.PrefixLength != nil {
		prefixLength = *input.PrefixLength
	}

	dnsJSON, err := json.Marshal(input.DNS)
	if err != nil {
		return nil, err
	}
	if input.DNS == nil {
		dnsJSON = []byte("[]")
	}

	var name string
	if trimmed := strings.TrimSpace(input.Name); trimmed != "" {
		name = trimmed
	}
	gateway := strings.TrimSpace(input.Gateway)

	var cidr *string
	if input.CIDR != nil {
		if trimmed := strings.TrimSpace(*input.CIDR); trimmed != "" {
			cidr = &trimmed
		}
	}
	var notes *string
	if input.Notes != nil {
		if trimmed := strings.TrimSpace(*input.Notes); trimmed != "" {
			notes = &trimmed
		}
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()
	pool, err := publicIpv4ScanPoolRow(tx.QueryRow(ctx, `INSERT INTO public_ipv4_pools (
			host_id, name, cidr, gateway, prefix_length, dns, notes, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		RETURNING `+publicIpv4PoolColumns,
		input.HostID, name, cidr, gateway, prefixLength, string(dnsJSON), notes, now))
	if err != nil {
		return nil, err
	}

	if len(addresses) > 0 {
		if err := publicIpv4InsertAddresses(ctx, tx, input.HostID, pool.ID, addresses,
			prefixLength, gateway, string(dnsJSON)); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return pool, nil
}

// publicIpv4InsertAddresses performs the createMany(skipDuplicates) insert. The
// unique index is (host_id, address); ON CONFLICT DO NOTHING is the exact
// equivalent of skipDuplicates and keeps RowsAffected = number of new rows.
func publicIpv4InsertAddresses(
	ctx context.Context,
	tx pgx.Tx,
	hostID int32,
	poolID int32,
	addresses []string,
	prefixLength int32,
	gateway string,
	dnsJSON string,
) error {
	if len(addresses) == 0 {
		return nil
	}
	now := time.Now().UTC()
	placeholders := make([]string, 0, len(addresses))
	args := make([]any, 0, len(addresses)*8)
	for _, address := range addresses {
		base := len(args)
		args = append(args, poolID, hostID, address, prefixLength, gateway, dnsJSON, now)
		placeholders = append(placeholders, "($"+
			strconvItoa(base+1)+", $"+strconvItoa(base+2)+", $"+strconvItoa(base+3)+", $"+
			strconvItoa(base+4)+", $"+strconvItoa(base+5)+", $"+strconvItoa(base+6)+", $"+
			strconvItoa(base+7)+", $"+strconvItoa(base+7)+")")
	}
	_, err := tx.Exec(ctx, `INSERT INTO public_ipv4_addresses (
			pool_id, host_id, address, prefix_length, gateway, dns, created_at, updated_at
		) VALUES `+strings.Join(placeholders, ", ")+`
		ON CONFLICT (host_id, address) DO NOTHING`, args...)
	return err
}

// strconvItoa is a tiny local integer renderer for SQL placeholders.
func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	buffer := make([]byte, 0, 8)
	for value > 0 {
		buffer = append([]byte{byte('0' + value%10)}, buffer...)
		value /= 10
	}
	return string(buffer)
}

// AddPublicIpv4Addresses mirrors addPublicIpv4Addresses: look the pool up scoped
// to the host, then bulk insert with duplicates skipped. Returns the number of
// rows actually inserted (`{ count }`).
func AddPublicIpv4Addresses(ctx context.Context, input PublicIpv4AddAddressesInput) (int64, error) {
	if err := publicIpv4RequireDB(); err != nil {
		return 0, err
	}

	var (
		poolID       int32
		gateway      string
		prefixLength int32
		dns          []byte
	)
	err := Pool().QueryRow(ctx, `SELECT id, gateway, prefix_length, dns
		FROM public_ipv4_pools WHERE id = $1 AND host_id = $2`,
		input.PoolID, input.HostID).Scan(&poolID, &gateway, &prefixLength, &dns)
	if isNoRows(err) {
		return 0, ErrPublicIpv4PoolNotFound
	}
	if err != nil {
		return 0, err
	}

	addresses := publicIpv4UniqueTrimmed(input.Addresses)
	if len(addresses) == 0 {
		return 0, nil
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	dnsJSON := string(publicIpv4RawJSON(dns, "[]"))
	now := time.Now().UTC()
	inserted := int64(0)
	for _, address := range addresses {
		tag, err := tx.Exec(ctx, `INSERT INTO public_ipv4_addresses (
				pool_id, host_id, address, prefix_length, gateway, dns, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
			ON CONFLICT (host_id, address) DO NOTHING`,
			poolID, input.HostID, address, prefixLength, gateway, dnsJSON, now)
		if err != nil {
			return 0, err
		}
		inserted += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

// UpdatePublicIpv4Pool mirrors updatePublicIpv4Pool. Like Prisma's update with an
// empty data object, an empty patch still bumps updated_at.
func UpdatePublicIpv4Pool(ctx context.Context, hostID, poolID int32, data PublicIpv4PoolUpdate) (*PublicIpv4PoolRecord, error) {
	if err := publicIpv4RequireDB(); err != nil {
		return nil, err
	}

	var existingID int32
	err := Pool().QueryRow(ctx, `SELECT id FROM public_ipv4_pools WHERE id = $1 AND host_id = $2`,
		poolID, hostID).Scan(&existingID)
	if isNoRows(err) {
		return nil, ErrPublicIpv4PoolNotFound
	}
	if err != nil {
		return nil, err
	}

	sets := make([]string, 0, 8)
	args := []any{poolID}
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconvItoa(len(args)))
	}
	if data.Name != nil {
		add("name", strings.TrimSpace(*data.Name))
	}
	if data.Gateway != nil {
		add("gateway", strings.TrimSpace(*data.Gateway))
	}
	if data.PrefixLength != nil {
		add("prefix_length", *data.PrefixLength)
	}
	if data.DNSSet {
		encoded, err := json.Marshal(data.DNS)
		if err != nil {
			return nil, err
		}
		add("dns", string(encoded))
	}
	if data.Enabled != nil {
		add("enabled", *data.Enabled)
	}
	if data.Notes.Set {
		var notes *string
		if data.Notes.Value != nil {
			if trimmed := strings.TrimSpace(*data.Notes.Value); trimmed != "" {
				notes = &trimmed
			}
		}
		add("notes", notes)
	}
	sets = append(sets, "updated_at = NOW()")

	pool, err := publicIpv4ScanPoolRow(Pool().QueryRow(ctx, `UPDATE public_ipv4_pools SET `+
		strings.Join(sets, ", ")+` WHERE id = $1 RETURNING `+publicIpv4PoolColumns, args...))
	if isNoRows(err) {
		return nil, ErrPublicIpv4PoolNotFound
	}
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// DeletePublicIpv4Pool mirrors deletePublicIpv4Pool: refuse while any address is
// assigned, then delete scoped to the host (zero rows → P2025, surfaced as
// ErrPublicIpv4PoolNotFound).
func DeletePublicIpv4Pool(ctx context.Context, hostID, poolID int32) error {
	if err := publicIpv4RequireDB(); err != nil {
		return err
	}

	var busy int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM public_ipv4_addresses
		WHERE host_id = $1 AND pool_id = $2 AND status = 'assigned'`,
		hostID, poolID).Scan(&busy); err != nil {
		return err
	}
	if busy > 0 {
		return ErrPublicIpv4PoolHasAssignedAddresses
	}

	tag, err := Pool().Exec(ctx, `DELETE FROM public_ipv4_pools WHERE id = $1 AND host_id = $2`,
		poolID, hostID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrPublicIpv4PoolNotFound
	}
	return nil
}

// DeletePublicIpv4Address mirrors deletePublicIpv4Address: an assigned address can
// never be deleted (release it first).
func DeletePublicIpv4Address(ctx context.Context, hostID, addressID int32) error {
	if err := publicIpv4RequireDB(); err != nil {
		return err
	}

	var status string
	err := Pool().QueryRow(ctx, `SELECT status FROM public_ipv4_addresses
		WHERE id = $1 AND host_id = $2`, addressID, hostID).Scan(&status)
	if isNoRows(err) {
		return ErrPublicIpv4AddressNotFound
	}
	if err != nil {
		return err
	}
	if models.PublicIpv4AddressStatus(status) == models.PublicIpv4AddressStatusAssigned {
		return ErrPublicIpv4AddressAssigned
	}

	if _, err := Pool().Exec(ctx, `DELETE FROM public_ipv4_addresses WHERE id = $1`, addressID); err != nil {
		return err
	}
	return nil
}

// SetPublicIpv4AddressStatus mirrors setPublicIpv4AddressStatus: only free/disabled
// targets are accepted, and assigned rows are rejected before the write.
func SetPublicIpv4AddressStatus(
	ctx context.Context,
	hostID, addressID int32,
	status models.PublicIpv4AddressStatus,
) (*PublicIpv4PoolAddressRecord, error) {
	if err := publicIpv4RequireDB(); err != nil {
		return nil, err
	}
	if status != models.PublicIpv4AddressStatusFree && status != models.PublicIpv4AddressStatusDisabled {
		// The TypeScript type only admits 'free' | 'disabled'; a third value would
		// fail the enum at the driver instead. Reject it here with the same shape.
		return nil, ErrPublicIpv4AddressNotFound
	}

	var current string
	err := Pool().QueryRow(ctx, `SELECT status FROM public_ipv4_addresses
		WHERE id = $1 AND host_id = $2`, addressID, hostID).Scan(&current)
	if isNoRows(err) {
		return nil, ErrPublicIpv4AddressNotFound
	}
	if err != nil {
		return nil, err
	}
	if models.PublicIpv4AddressStatus(current) == models.PublicIpv4AddressStatusAssigned {
		return nil, ErrPublicIpv4AddressAssigned
	}

	var (
		record     PublicIpv4PoolAddressRecord
		rawStatus  string
		assignedAt *time.Time
		releasedAt *time.Time
		createdAt  time.Time
		updatedAt  time.Time
		dns        []byte
	)
	err = Pool().QueryRow(ctx, `UPDATE public_ipv4_addresses SET status = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING `+publicIpv4PoolAddressColumns,
		addressID, string(status)).Scan(&record.ID, &record.Address, &record.PrefixLength,
		&record.Gateway, &dns, &rawStatus, &record.InstanceID, &assignedAt, &releasedAt,
		&record.Notes, &createdAt, &updatedAt)
	if isNoRows(err) {
		return nil, ErrPublicIpv4AddressNotFound
	}
	if err != nil {
		return nil, err
	}
	record.DNS = publicIpv4RawJSON(dns, "[]")
	record.Status = models.PublicIpv4AddressStatus(rawStatus)
	record.AssignedAt = instancesOptionalISOTime(assignedAt)
	record.ReleasedAt = instancesOptionalISOTime(releasedAt)
	record.CreatedAt = instancesISOTime(createdAt)
	record.UpdatedAt = instancesISOTime(updatedAt)
	return &record, nil
}
