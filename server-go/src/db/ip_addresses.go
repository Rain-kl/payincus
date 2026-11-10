// Port of server/src/db/ip-addresses.ts.
//
// IP allocation is a correctness boundary: two instances must never end up with
// the same address. Every write that can introduce an IPv6 address therefore runs
// inside a transaction that first takes the PostgreSQL transaction-scoped advisory
// lock IPAddressAllocationLockNamespace (4119) keyed on the address string —
// exactly the namespace and ordering the TypeScript uses — and only then re-checks
// both uniqueness sources (ip_addresses rows and instances.ipv6) while the lock is
// still held. The lock key is the address itself, so unrelated allocations keep
// running concurrently.
//
// The NAT IPv4 allocator takes the same namespace with a per-host key
// (`nat-ipv4:host:<id>`), serializes every allocation for that host, and writes the
// reservation row inside the lock. That is the whole point: a candidate that only
// exists in memory is invisible to the next concurrent allocation, and the pool is
// 766 addresses wide, so the old "random retry until free" loop would eventually
// never terminate.
//
// db must not import lib (lib imports db), so the canonical listNatIpv4Pool is
// installed into IpAddressesNatIpv4PoolSource from the lib side (see
// lib/network_payload_builder.go). The fallback below is a byte-for-byte copy of
// the pool bounds so this package still behaves correctly when lib is not linked.
package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== errors ====================

// IpAddressesErrAlreadyExists mirrors the IP_ADDRESS_ALREADY_EXISTS sentinel the
// TypeScript throws from assertIpAddressAvailable.
var IpAddressesErrAlreadyExists = errors.New("IP_ADDRESS_ALREADY_EXISTS")

// IpAddressesErrNotFound mirrors the IP_ADDRESS_NOT_FOUND error thrown when
// updateIpAddressAddress targets a row that no longer exists.
var IpAddressesErrNotFound = errors.New("IP_ADDRESS_NOT_FOUND")

// IpAddressesIsAlreadyExistsError mirrors isIpAddressAlreadyExistsError. The
// TypeScript compares error.message, so both the sentinel and any same-message
// error match; nothing else does.
func IpAddressesIsAlreadyExistsError(err error) bool {
	return err != nil && (errors.Is(err, IpAddressesErrAlreadyExists) || err.Error() == IpAddressesErrAlreadyExists.Error())
}

// ipAddressesIsUniqueViolation reports PostgreSQL's unique_violation (SQLSTATE
// 23505). The TypeScript never reaches it because ip_addresses has only
// non-unique indexes, but the database remains the final authority on uniqueness:
// if a migration ever adds the constraint, a concurrent duplicate surfaces here
// and is mapped to the same 409 the route already emits.
func ipAddressesIsUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func ipAddressesDatabaseReady() error {
	if pool == nil {
		return errors.New("database not initialized")
	}
	return nil
}

// ipAddressesRowQuerier is the small slice of pgx shared by *pgxpool.Pool and
// pgx.Tx, so the insert helper works inside and outside a transaction.
type ipAddressesRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ==================== types ====================

// IpAddressesCreateData mirrors CreateIpAddressData. IsCustom models
// `isCustom?: boolean` (nil means "not provided" → stored as false).
type IpAddressesCreateData struct {
	Address    string
	Type       models.IpType
	IsPrimary  bool
	IsCustom   *bool
	Device     string
	HostID     *int32
	InstanceID int32
}

// IpAddressesUpdateAddressData mirrors UpdateIpAddressAddressData.
type IpAddressesUpdateAddressData struct {
	ID       int32
	Address  string
	IsCustom *bool
}

// IpAddressesHost is the projection of getHostById this module reads: identity,
// ownership, the Incus connection fields getIncusClient needs, and the IPv6
// subnet/parent-interface configuration.
type IpAddressesHost struct {
	ID                  int32
	UserID              int32
	Name                string
	URL                 string
	CertPath            *string
	KeyPath             *string
	TunnelEnabled       bool
	TargetHost          string
	TargetPort          int32
	IPv6Subnet          *string
	IPv6Gateway         *string
	IPv6ParentInterface *string
}

// IpAddressesInstance is the projection of getInstanceById this module reads.
type IpAddressesInstance struct {
	ID          int32
	IncusID     string
	Name        string
	UserID      int32
	HostID      int32
	Status      models.InstanceStatus
	NetworkMode string
	// Host is populated by IpAddressesGetByID, mirroring
	// `include: { instance: { include: { host: true } } }`.
	Host *IpAddressesHost
}

// IpAddressesRecord is an ip_addresses row plus its instance (and that instance's
// host), mirroring getIpAddressById's include.
type IpAddressesRecord struct {
	ID         int32
	Address    string
	Type       models.IpType
	IsPrimary  bool
	IsCustom   bool
	Device     string
	HostID     int32
	InstanceID int32
	CreatedAt  time.Time
	Instance   *IpAddressesInstance
}

// ==================== reads ====================

const ipAddressesColumns = "id, address, type::text AS type, is_primary, is_custom, device, host_id, instance_id, created_at"

// ipAddressesScan reads the standard ip_addresses projection.
func ipAddressesScan(row pgx.Row, record *models.IpAddress) error {
	var ipType string
	if err := row.Scan(&record.ID, &record.Address, &ipType, &record.IsPrimary, &record.IsCustom,
		&record.Device, &record.HostID, &record.InstanceID, &record.CreatedAt); err != nil {
		return err
	}
	record.Type = models.IpType(ipType)
	return nil
}

// IpAddressesGetByInstanceID mirrors getIpAddressesByInstanceId: primary first,
// then oldest first.
func IpAddressesGetByInstanceID(ctx context.Context, instanceID int32) ([]models.IpAddress, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT `+ipAddressesColumns+`
		FROM ip_addresses WHERE instance_id = $1
		ORDER BY is_primary DESC, created_at ASC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]models.IpAddress, 0, 4)
	for rows.Next() {
		var record models.IpAddress
		if err := ipAddressesScan(rows, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// IpAddressesGetPrimary mirrors getPrimaryIpAddress.
func IpAddressesGetPrimary(ctx context.Context, instanceID int32, ipType models.IpType) (*models.IpAddress, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}
	var record models.IpAddress
	err := ipAddressesScan(pool.QueryRow(ctx, `SELECT `+ipAddressesColumns+`
		FROM ip_addresses
		WHERE instance_id = $1 AND type = $2 AND is_primary = true
		LIMIT 1`, instanceID, string(ipType)), &record)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// IpAddressesGetByID mirrors getIpAddressById, including the instance and its host.
func IpAddressesGetByID(ctx context.Context, id int32) (*IpAddressesRecord, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}

	var record IpAddressesRecord
	var ipType string
	var instance IpAddressesInstance
	var instanceStatus, networkMode string
	var host IpAddressesHost

	err := pool.QueryRow(ctx, `SELECT ia.id, ia.address, ia.type::text, ia.is_primary, ia.is_custom,
			ia.device, ia.host_id, ia.instance_id, ia.created_at,
			i.id, i.incus_id, i.name, i.user_id, i.host_id, i.status::text, i.network_mode::text,
			h.id, h.user_id, h.name, h.url, h.cert_path, h.key_path, h.tunnel_enabled,
			h.target_host, h.target_port, h.ipv6_subnet, h.ipv6_gateway, h.ipv6_parent_interface
		FROM ip_addresses ia
		JOIN instances i ON i.id = ia.instance_id
		JOIN hosts h ON h.id = i.host_id
		WHERE ia.id = $1
		LIMIT 1`, id).Scan(
		&record.ID, &record.Address, &ipType, &record.IsPrimary, &record.IsCustom,
		&record.Device, &record.HostID, &record.InstanceID, &record.CreatedAt,
		&instance.ID, &instance.IncusID, &instance.Name, &instance.UserID, &instance.HostID,
		&instanceStatus, &networkMode,
		&host.ID, &host.UserID, &host.Name, &host.URL, &host.CertPath, &host.KeyPath,
		&host.TunnelEnabled, &host.TargetHost, &host.TargetPort, &host.IPv6Subnet,
		&host.IPv6Gateway, &host.IPv6ParentInterface,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	record.Type = models.IpType(ipType)
	instance.Status = models.InstanceStatus(instanceStatus)
	instance.NetworkMode = networkMode
	instance.Host = &host
	record.Instance = &instance
	return &record, nil
}

// IpAddressesGetHostByID mirrors the `db.getHostById` projection the route reads.
func IpAddressesGetHostByID(ctx context.Context, hostID int32) (*IpAddressesHost, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}
	var host IpAddressesHost
	err := pool.QueryRow(ctx, `SELECT id, user_id, name, url, cert_path, key_path, tunnel_enabled,
			target_host, target_port, ipv6_subnet, ipv6_gateway, ipv6_parent_interface
		FROM hosts WHERE id = $1 LIMIT 1`, hostID).Scan(
		&host.ID, &host.UserID, &host.Name, &host.URL, &host.CertPath, &host.KeyPath,
		&host.TunnelEnabled, &host.TargetHost, &host.TargetPort, &host.IPv6Subnet,
		&host.IPv6Gateway, &host.IPv6ParentInterface,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &host, nil
}

// IpAddressesGetInstanceByID mirrors the db.getInstanceById projection the route
// reads (identity, ownership, status and network mode).
func IpAddressesGetInstanceByID(ctx context.Context, instanceID int32) (*IpAddressesInstance, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}
	var instance IpAddressesInstance
	var status, networkMode string
	err := pool.QueryRow(ctx, `SELECT id, incus_id, name, user_id, host_id, status::text, network_mode::text
		FROM instances WHERE id = $1 LIMIT 1`, instanceID).Scan(
		&instance.ID, &instance.IncusID, &instance.Name, &instance.UserID, &instance.HostID,
		&status, &networkMode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	instance.Status = models.InstanceStatus(status)
	instance.NetworkMode = networkMode
	return &instance, nil
}

// ==================== writes ====================

// ipAddressesResolveInstanceHostID mirrors resolveInstanceHostId.
func ipAddressesResolveInstanceHostID(ctx context.Context, instanceID int32) (int32, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return 0, err
	}
	var hostID int32
	err := pool.QueryRow(ctx, `SELECT host_id FROM instances WHERE id = $1 LIMIT 1`, instanceID).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("Instance %d not found while creating IP address record", instanceID)
	}
	if err != nil {
		return 0, err
	}
	return hostID, nil
}

// ipAddressesAssertAvailable mirrors assertIpAddressAvailable: both uniqueness
// sources, checked while the caller holds the address-scoped advisory lock.
func ipAddressesAssertAvailable(ctx context.Context, tx pgx.Tx, input struct {
	Address            string
	Type               models.IpType
	InstanceID         int32
	IsPrimary          bool
	ExcludeIPAddressID *int32
}) error {
	if input.Type != models.IpTypeInet6 {
		return nil
	}

	existingIPSQL := `SELECT ia.id FROM ip_addresses ia
		JOIN instances i ON i.id = ia.instance_id
		WHERE ia.address = $1 AND ia.type = $2 AND i.status::text <> 'deleted'`
	args := []any{input.Address, string(input.Type)}
	if input.ExcludeIPAddressID != nil {
		args = append(args, *input.ExcludeIPAddressID)
		existingIPSQL += fmt.Sprintf(" AND ia.id <> $%d", len(args))
	}
	existingIPSQL += " LIMIT 1"

	var existingIPID int32
	err := tx.QueryRow(ctx, existingIPSQL, args...).Scan(&existingIPID)
	switch {
	case err == nil:
		return IpAddressesErrAlreadyExists
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to the instance.ipv6 check
	default:
		return err
	}

	existingInstanceSQL := `SELECT id FROM instances WHERE ipv6 = $1 AND status::text <> 'deleted'`
	args = []any{input.Address}
	if input.IsPrimary {
		args = append(args, input.InstanceID)
		existingInstanceSQL += " AND id <> $2"
	}
	existingInstanceSQL += " LIMIT 1"

	var existingInstanceID int32
	err = tx.QueryRow(ctx, existingInstanceSQL, args...).Scan(&existingInstanceID)
	switch {
	case err == nil:
		return IpAddressesErrAlreadyExists
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		return err
	}
}

// ipAddressesInsert mirrors tx.ipAddress.create. The host and instance are set by
// their foreign keys directly (Prisma's `connect`).
func ipAddressesInsert(ctx context.Context, q ipAddressesRowQuerier, data IpAddressesCreateData, hostID int32) (*models.IpAddress, error) {
	isCustom := false
	if data.IsCustom != nil {
		isCustom = *data.IsCustom
	}

	var record models.IpAddress
	var ipType string
	err := q.QueryRow(ctx, `INSERT INTO ip_addresses
			(address, type, is_primary, is_custom, device, host_id, instance_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+ipAddressesColumns,
		data.Address, string(data.Type), data.IsPrimary, isCustom, data.Device, hostID, data.InstanceID,
	).Scan(&record.ID, &record.Address, &ipType, &record.IsPrimary, &record.IsCustom,
		&record.Device, &record.HostID, &record.InstanceID, &record.CreatedAt)
	if err != nil {
		return nil, err
	}
	record.Type = models.IpType(ipType)
	return &record, nil
}

// IpAddressesCreate mirrors createIpAddress. inet6 writes are serialized per
// address by the advisory lock; every other type inserts directly, like the
// TypeScript.
func IpAddressesCreate(ctx context.Context, data IpAddressesCreateData) (*models.IpAddress, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}

	hostID := int32(0)
	if data.HostID != nil {
		hostID = *data.HostID
	} else {
		resolved, err := ipAddressesResolveInstanceHostID(ctx, data.InstanceID)
		if err != nil {
			return nil, err
		}
		hostID = resolved
	}

	if data.Type != models.IpTypeInet6 {
		record, err := ipAddressesInsert(ctx, pool, data, hostID)
		if err != nil && ipAddressesIsUniqueViolation(err) {
			return nil, IpAddressesErrAlreadyExists
		}
		return record, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same namespace and key as the TypeScript: the address string.
	if err := AdvisoryTransactionLockString(ctx, tx, IPAddressAllocationLockNamespace, data.Address); err != nil {
		return nil, err
	}
	if err := ipAddressesAssertAvailable(ctx, tx, struct {
		Address            string
		Type               models.IpType
		InstanceID         int32
		IsPrimary          bool
		ExcludeIPAddressID *int32
	}{
		Address:    data.Address,
		Type:       data.Type,
		InstanceID: data.InstanceID,
		IsPrimary:  data.IsPrimary,
	}); err != nil {
		return nil, err
	}

	record, err := ipAddressesInsert(ctx, tx, data, hostID)
	if err != nil {
		if ipAddressesIsUniqueViolation(err) {
			return nil, IpAddressesErrAlreadyExists
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return record, nil
}

// IpAddressesUpdateAddress mirrors updateIpAddressAddress. A primary inet6 row
// also updates instances.ipv6 so the denormalized column cannot drift from
// ip_addresses.
func IpAddressesUpdateAddress(ctx context.Context, data IpAddressesUpdateAddressData) (*models.IpAddress, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var recordID, instanceID int32
	var ipType string
	var isPrimary bool
	err = tx.QueryRow(ctx, `SELECT id, type::text, instance_id, is_primary
		FROM ip_addresses WHERE id = $1 LIMIT 1`, data.ID).
		Scan(&recordID, &ipType, &instanceID, &isPrimary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, IpAddressesErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if models.IpType(ipType) == models.IpTypeInet6 {
		if err := AdvisoryTransactionLockString(ctx, tx, IPAddressAllocationLockNamespace, data.Address); err != nil {
			return nil, err
		}
		if err := ipAddressesAssertAvailable(ctx, tx, struct {
			Address            string
			Type               models.IpType
			InstanceID         int32
			IsPrimary          bool
			ExcludeIPAddressID *int32
		}{
			Address:            data.Address,
			Type:               models.IpType(ipType),
			InstanceID:         instanceID,
			IsPrimary:          isPrimary,
			ExcludeIPAddressID: &recordID,
		}); err != nil {
			return nil, err
		}
	}

	var updated models.IpAddress
	var updatedType string
	if data.IsCustom != nil {
		err = tx.QueryRow(ctx, `UPDATE ip_addresses SET address = $2, is_custom = $3
			WHERE id = $1 RETURNING `+ipAddressesColumns,
			data.ID, data.Address, *data.IsCustom).
			Scan(&updated.ID, &updated.Address, &updatedType, &updated.IsPrimary, &updated.IsCustom,
				&updated.Device, &updated.HostID, &updated.InstanceID, &updated.CreatedAt)
	} else {
		err = tx.QueryRow(ctx, `UPDATE ip_addresses SET address = $2
			WHERE id = $1 RETURNING `+ipAddressesColumns,
			data.ID, data.Address).
			Scan(&updated.ID, &updated.Address, &updatedType, &updated.IsPrimary, &updated.IsCustom,
				&updated.Device, &updated.HostID, &updated.InstanceID, &updated.CreatedAt)
	}
	if err != nil {
		if ipAddressesIsUniqueViolation(err) {
			return nil, IpAddressesErrAlreadyExists
		}
		return nil, err
	}
	updated.Type = models.IpType(updatedType)

	if models.IpType(ipType) == models.IpTypeInet6 && isPrimary {
		// Prisma's instance.update also refreshes @updatedAt.
		if _, err := tx.Exec(ctx, `UPDATE instances SET ipv6 = $2, updated_at = NOW() WHERE id = $1`,
			instanceID, data.Address); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &updated, nil
}

// IpAddressesDelete mirrors deleteIpAddress. Prisma's delete raises P2025 when the
// row is gone, which surfaces as a 500; the routes always read the row first.
func IpAddressesDelete(ctx context.Context, id int32) error {
	if err := ipAddressesDatabaseReady(); err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `DELETE FROM ip_addresses WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("记录不存在或已被删除")
	}
	return nil
}

// ==================== lookups ====================

// ipAddressesDevicePattern mirrors /^eth(\d+)$/.
var ipAddressesDevicePattern = regexp.MustCompile(`^eth(\d+)$`)

// IpAddressesNextDeviceName mirrors getNextDeviceName: the lowest ethN not in use.
func IpAddressesNextDeviceName(ctx context.Context, instanceID int32) (string, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return "", err
	}
	rows, err := pool.Query(ctx, `SELECT device FROM ip_addresses WHERE instance_id = $1`, instanceID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	used := map[int]bool{}
	for rows.Next() {
		var device string
		if err := rows.Scan(&device); err != nil {
			return "", err
		}
		match := ipAddressesDevicePattern.FindStringSubmatch(device)
		if match == nil {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil || index < 0 {
			continue
		}
		used[index] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	next := 0
	for used[next] {
		next++
	}
	return "eth" + strconv.Itoa(next), nil
}

// IpAddressesExists mirrors isIpAddressExists: both the ip_addresses table and the
// denormalized instances.ipv6 column, excluding deleted instances.
func IpAddressesExists(ctx context.Context, address string) (bool, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return false, err
	}

	var existingID int32
	err := pool.QueryRow(ctx, `SELECT ia.id FROM ip_addresses ia
		JOIN instances i ON i.id = ia.instance_id
		WHERE ia.address = $1 AND ia.type = 'inet6' AND i.status::text <> 'deleted'
		LIMIT 1`, address).Scan(&existingID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	err = pool.QueryRow(ctx, `SELECT id FROM instances
		WHERE ipv6 = $1 AND status::text <> 'deleted' LIMIT 1`, address).Scan(&existingID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// IpAddressesExistsOnHost mirrors isIpAddressExistsOnHost: the same two sources,
// scoped to one host (private IPv4 space is per-host).
func IpAddressesExistsOnHost(ctx context.Context, address string, hostID int32) (bool, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return false, err
	}

	var existingID int32
	err := pool.QueryRow(ctx, `SELECT ia.id FROM ip_addresses ia
		JOIN instances i ON i.id = ia.instance_id
		WHERE ia.address = $1 AND ia.type = 'inet4' AND ia.host_id = $2
		  AND i.status::text <> 'deleted'
		LIMIT 1`, address, hostID).Scan(&existingID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	err = pool.QueryRow(ctx, `SELECT id FROM instances
		WHERE host_id = $1 AND ipv4 = $2 AND status::text <> 'deleted' LIMIT 1`,
		hostID, address).Scan(&existingID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// IpAddressesCount mirrors countIpAddresses.
func IpAddressesCount(ctx context.Context, instanceID int32) (int64, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return 0, err
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM ip_addresses WHERE instance_id = $1`,
		instanceID).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// ==================== NAT IPv4 reservation ====================

// Nat IPv4 pool bounds and reservation TTL, mirroring lib/ip-calculator.ts'
// IPV4_CONFIG and NAT_IPV4_RESERVATION_TTL_MS.
const (
	ipAddressesNatPoolStart = "10.10.1.1"
	ipAddressesNatPoolEnd   = "10.10.3.254"
	ipAddressesNatTTL       = 60 * 60 * 1000
)

// IpAddressesNatIpv4PoolSource mirrors listNatIpv4Pool(). The canonical
// implementation lives in lib (lib.ListNatIpv4Pool) and is installed from
// lib/network_payload_builder.go's init(); this fallback keeps the allocator
// correct when the lib package is not linked in (e.g. db package tests).
var IpAddressesNatIpv4PoolSource = func() []string { return ipAddressesFallbackNatPool() }

// ipAddressesRandomFloat64 mirrors Math.random() for the free-set pick.
var ipAddressesRandomFloat64 = rand.Float64

// ipAddressesFallbackNatPool enumerates 10.10.1.1–10.10.3.254 in order.
func ipAddressesFallbackNatPool() []string {
	start, ok := ipAddressesIPv4ToUint32(ipAddressesNatPoolStart)
	if !ok {
		return nil
	}
	end, ok := ipAddressesIPv4ToUint32(ipAddressesNatPoolEnd)
	if !ok {
		return nil
	}
	addresses := make([]string, 0, end-start+1)
	for value := start; value <= end; value++ {
		addresses = append(addresses, ipAddressesIPv4FromUint32(value))
	}
	return addresses
}

func ipAddressesIPv4ToUint32(address string) (uint32, bool) {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() {
		return 0, false
	}
	bytes := parsed.As4()
	return uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3]), true
}

func ipAddressesIPv4FromUint32(value uint32) string {
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}).String()
}

// IpAddressesAllocateAndReserveNatIpv4 mirrors allocateAndReserveNatIpv4.
//
// Everything happens inside one transaction holding the per-host advisory lock
// (namespace 4119, key `nat-ipv4:host:<id>`), and the reservation row is written
// before the lock is released. That is what makes a candidate visible to the next
// concurrent allocation; the reservation is a standalone row rather than
// instance.ipv4 because clone flows need an address before the instance exists and
// host migrations must not write to the old host's instance row.
func IpAddressesAllocateAndReserveNatIpv4(ctx context.Context, hostID int32, instanceID *int32) (*string, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// One lock per host: private IPv4 only needs to be unique inside a host.
	if err := AdvisoryTransactionLockString(ctx, tx, IPAddressAllocationLockNamespace,
		fmt.Sprintf("nat-ipv4:host:%d", hostID)); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	// Clear expired reservations: a failed delivery must not hold an address forever.
	if _, err := tx.Exec(ctx, `DELETE FROM nat_ipv4_reservations WHERE expires_at < $1`, now); err != nil {
		return nil, err
	}

	used := map[string]bool{}

	// 1. ip_addresses rows on this host belonging to a live instance.
	rows, err := tx.Query(ctx, `SELECT ia.address FROM ip_addresses ia
		JOIN instances i ON i.id = ia.instance_id
		WHERE ia.host_id = $1 AND ia.type = 'inet4' AND i.status::text <> 'deleted'`, hostID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			rows.Close()
			return nil, err
		}
		used[address] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. the denormalized instances.ipv4 column (rows that never got an
	// ip_addresses record).
	rows, err = tx.Query(ctx, `SELECT ipv4 FROM instances
		WHERE host_id = $1 AND ipv4 IS NOT NULL AND status::text <> 'deleted'`, hostID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var address *string
		if err := rows.Scan(&address); err != nil {
			rows.Close()
			return nil, err
		}
		if address != nil {
			used[*address] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3. live reservations.
	rows, err = tx.Query(ctx, `SELECT address FROM nat_ipv4_reservations
		WHERE host_id = $1 AND expires_at >= $2`, hostID, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			rows.Close()
			return nil, err
		}
		used[address] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	free := make([]string, 0, len(used)+64)
	for _, address := range IpAddressesNatIpv4PoolSource() {
		if !used[address] {
			free = append(free, address)
		}
	}
	if len(free) == 0 {
		return nil, nil
	}

	// `free[Math.floor(Math.random() * free.length)]`
	index := int(ipAddressesRandomFloat64() * float64(len(free)))
	if index < 0 {
		index = 0
	}
	if index >= len(free) {
		index = len(free) - 1
	}
	candidate := free[index]

	expiresAt := now.Add(ipAddressesNatTTL * time.Millisecond)
	if _, err := tx.Exec(ctx, `INSERT INTO nat_ipv4_reservations (host_id, address, expires_at)
		VALUES ($1, $2, $3)`, hostID, candidate, expiresAt); err != nil {
		return nil, err
	}

	// When the instance already lives on this host, mirror the address onto
	// instance.ipv4 so the panel shows it immediately. The hostId predicate keeps
	// the migration case (instance still on the old host) untouched.
	if instanceID != nil {
		if _, err := tx.Exec(ctx, `UPDATE instances SET ipv4 = $3, updated_at = NOW()
			WHERE id = $1 AND host_id = $2`, *instanceID, hostID, candidate); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &candidate, nil
}
