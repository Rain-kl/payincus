// Port of server/src/db/ipv6-subnets.ts.
//
// Allocation is a correctness boundary here too, but with a different failure
// mode than single addresses: a subnet must not overlap ANY allocated subnet, so
// the whole-table overlap scan and the insert run inside one transaction holding
// the transaction-scoped advisory lock (namespace 4202, key 1). Namespace 4202 is
// a second, independent namespace from IP address allocation (4119) — keeping them
// separate means a slow address allocation never blocks subnet allocation and vice
// versa, exactly like the TypeScript.
//
// CIDRs are canonicalised to `<network address>/<prefix>` before they are compared
// or stored, and ranges are compared as inclusive 128-bit integers, mirroring
// Address6.startAddress()/endAddress() and the library's correctForm() rendering
// (RFC 5952: lowercase, no leading zeros, first longest zero run compressed, and a
// single zero group never compressed).
package db

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Ipv6SubnetAllowedPrefixes mirrors IPV6_SUBNET_ALLOWED_PREFIXES.
var Ipv6SubnetAllowedPrefixes = []int{112, 120, 124}

// Ipv6SubnetOverlapError mirrors IPV6_SUBNET_OVERLAP.
const Ipv6SubnetOverlapError = "IPV6_SUBNET_OVERLAP"

// Ipv6SubnetAllocationLockNamespace mirrors IPV6_SUBNET_ALLOCATION_LOCK_NAMESPACE.
const Ipv6SubnetAllocationLockNamespace = 4202

// ipv6SubnetsPrefixPattern mirrors /^\d{1,3}$/.
var ipv6SubnetsPrefixPattern = regexp.MustCompile(`^\d{1,3}$`)

// Ipv6SubnetsIsOverlapError mirrors isIpv6SubnetOverlapError: it matches the
// sentinel message only.
func Ipv6SubnetsIsOverlapError(err error) bool {
	return err != nil && err.Error() == Ipv6SubnetOverlapError
}

// Ipv6CidrRange mirrors Ipv6CidrRange: the canonical CIDR string plus its
// inclusive start/end as 128-bit integers.
type Ipv6CidrRange struct {
	CIDR   string
	Prefix int
	Start  *big.Int
	End    *big.Int
}

// ipv6SubnetsToBigInt converts a 16-byte address to its integer value.
func ipv6SubnetsToBigInt(address netip.Addr) *big.Int {
	bytes := address.As16()
	return new(big.Int).SetBytes(bytes[:])
}

// ipv6SubnetsFromBigInt mirrors Address6.fromBigInt(value).correctForm().
func ipv6SubnetsFromBigInt(value *big.Int) string {
	modulus := new(big.Int).Lsh(big.NewInt(1), 128)
	reduced := new(big.Int).Mod(value, modulus)
	bytes := make([]byte, 16)
	reduced.FillBytes(bytes)

	var array [16]byte
	copy(array[:], bytes)
	return ipv6SubnetsFormatCorrect(array)
}

// ipv6SubnetsFormatCorrect mirrors Address6#correctForm().
//
// The compression rule differs from netip.Addr.String() only for IPv4-mapped
// addresses (netip renders the dotted quad, correctForm renders two hex groups),
// but reproducing the algorithm keeps the stored CIDR byte-identical to Node.
func ipv6SubnetsFormatCorrect(address [16]byte) string {
	groups := make([]int, 8)
	for index := 0; index < 8; index++ {
		groups[index] = int(address[index*2])<<8 | int(address[index*2+1])
	}

	// First longest run of two or more zero groups. A single zero group is never
	// compressed (RFC 5952, and correctForm only records zeroCounter > 1).
	bestStart, bestLength := -1, 0
	for index := 0; index < 8; {
		if groups[index] != 0 {
			index++
			continue
		}
		runEnd := index
		for runEnd < 8 && groups[runEnd] == 0 {
			runEnd++
		}
		if runEnd-index > 1 && runEnd-index > bestLength {
			bestStart, bestLength = index, runEnd-index
		}
		index = runEnd
	}

	var builder strings.Builder
	wroteGroup := false
	for index := 0; index < 8; {
		if index == bestStart {
			builder.WriteString("::")
			wroteGroup = false
			index += bestLength
			continue
		}
		if wroteGroup {
			builder.WriteString(":")
		}
		builder.WriteString(fmt.Sprintf("%x", groups[index]))
		wroteGroup = true
		index++
	}
	formatted := builder.String()
	if formatted == "" {
		return "::"
	}
	return formatted
}

// Ipv6SubnetsNormalizeCidrRange mirrors normalizeIpv6CidrRange: canonical network
// address plus the inclusive range. Any malformed input is an error, which the
// route turns into 400 "Invalid IPv6 subnet CIDR format".
func Ipv6SubnetsNormalizeCidrRange(cidr string) (Ipv6CidrRange, error) {
	trimmed := strings.TrimSpace(cidr)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || !ipv6SubnetsPrefixPattern.MatchString(parts[1]) {
		return Ipv6CidrRange{}, errors.New("Invalid IPv6 subnet CIDR")
	}

	prefix := 0
	for _, digit := range parts[1] {
		prefix = prefix*10 + int(digit-'0')
	}
	if prefix < 0 || prefix > 128 {
		return Ipv6CidrRange{}, errors.New("Invalid IPv6 subnet CIDR")
	}

	address, err := netip.ParseAddr(parts[0])
	if err != nil || !address.Is6() {
		return Ipv6CidrRange{}, errors.New("Invalid IPv6 subnet CIDR")
	}
	// Address6 strips the zone before parsing; netip keeps it, so drop it here.
	address = address.WithZone("")

	masked, err := address.Prefix(prefix)
	if err != nil {
		return Ipv6CidrRange{}, errors.New("Invalid IPv6 subnet CIDR")
	}
	start := ipv6SubnetsToBigInt(masked.Addr())
	size := new(big.Int).Lsh(big.NewInt(1), uint(128-prefix))
	end := new(big.Int).Sub(new(big.Int).Add(start, size), big.NewInt(1))

	return Ipv6CidrRange{
		CIDR:   ipv6SubnetsFromBigInt(start) + "/" + fmt.Sprintf("%d", prefix),
		Prefix: prefix,
		Start:  start,
		End:    end,
	}, nil
}

// Ipv6SubnetsRangesOverlap mirrors ipv6CidrRangesOverlap.
func Ipv6SubnetsRangesOverlap(a, b Ipv6CidrRange) bool {
	return a.Start.Cmp(b.End) <= 0 && b.Start.Cmp(a.End) <= 0
}

// Ipv6SubnetsCreateData mirrors CreateIpv6SubnetData.
type Ipv6SubnetsCreateData struct {
	CIDR       string
	PrimaryIP  string
	Device     *string
	InstanceID int32
}

// Ipv6SubnetsCreate mirrors createIpv6Subnet.
func Ipv6SubnetsCreate(ctx context.Context, data Ipv6SubnetsCreateData) (*models.Ipv6Subnet, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}

	candidate, err := Ipv6SubnetsNormalizeCidrRange(data.CIDR)
	if err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// `pg_advisory_xact_lock(4202, 1)` — the same two-int form as the TypeScript.
	if err := AdvisoryTransactionLock(ctx, tx, Ipv6SubnetAllocationLockNamespace, 1); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `SELECT cidr FROM ipv6_subnets`)
	if err != nil {
		return nil, err
	}
	overlaps := false
	for rows.Next() {
		var allocated string
		if err := rows.Scan(&allocated); err != nil {
			rows.Close()
			return nil, err
		}
		existing, err := Ipv6SubnetsNormalizeCidrRange(allocated)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if Ipv6SubnetsRangesOverlap(candidate, existing) {
			overlaps = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if overlaps {
		return nil, errors.New(Ipv6SubnetOverlapError)
	}

	device := "eth1"
	if data.Device != nil && *data.Device != "" {
		device = *data.Device
	}

	var record models.Ipv6Subnet
	err = tx.QueryRow(ctx, `INSERT INTO ipv6_subnets (cidr, primary_ip, device, instance_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, cidr, primary_ip, device, instance_id, created_at`,
		candidate.CIDR, data.PrimaryIP, device, data.InstanceID,
	).Scan(&record.ID, &record.CIDR, &record.PrimaryIP, &record.Device, &record.InstanceID, &record.CreatedAt)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &record, nil
}

// Ipv6SubnetsGetByInstanceID mirrors getIpv6SubnetsByInstanceId.
func Ipv6SubnetsGetByInstanceID(ctx context.Context, instanceID int32) ([]models.Ipv6Subnet, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT id, cidr, primary_ip, device, instance_id, created_at
		FROM ipv6_subnets WHERE instance_id = $1 ORDER BY created_at ASC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	subnets := make([]models.Ipv6Subnet, 0, 2)
	for rows.Next() {
		var subnet models.Ipv6Subnet
		if err := rows.Scan(&subnet.ID, &subnet.CIDR, &subnet.PrimaryIP, &subnet.Device,
			&subnet.InstanceID, &subnet.CreatedAt); err != nil {
			return nil, err
		}
		subnets = append(subnets, subnet)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return subnets, nil
}

// Ipv6SubnetsInstance is the instance projection getIpv6SubnetById includes.
type Ipv6SubnetsInstance struct {
	ID          int32
	IncusID     string
	Name        string
	UserID      int32
	HostID      int32
	Status      models.InstanceStatus
	NetworkMode string
	Host        *IpAddressesHost
}

// Ipv6SubnetsRecord mirrors getIpv6SubnetById's `include: { instance: { include: {
// host: true } } }`.
type Ipv6SubnetsRecord struct {
	ID         int32
	CIDR       string
	PrimaryIP  string
	Device     string
	InstanceID int32
	CreatedAt  time.Time
	Instance   *Ipv6SubnetsInstance
}

// Ipv6SubnetsGetByID mirrors getIpv6SubnetById.
func Ipv6SubnetsGetByID(ctx context.Context, id int32) (*Ipv6SubnetsRecord, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}

	var record Ipv6SubnetsRecord
	var instance Ipv6SubnetsInstance
	var status, networkMode string
	var host IpAddressesHost
	err := pool.QueryRow(ctx, `SELECT s.id, s.cidr, s.primary_ip, s.device, s.instance_id, s.created_at,
			i.id, i.incus_id, i.name, i.user_id, i.host_id, i.status::text, i.network_mode::text,
			h.id, h.user_id, h.name, h.url, h.cert_path, h.key_path, h.tunnel_enabled,
			h.target_host, h.target_port, h.ipv6_subnet, h.ipv6_gateway, h.ipv6_parent_interface
		FROM ipv6_subnets s
		JOIN instances i ON i.id = s.instance_id
		JOIN hosts h ON h.id = i.host_id
		WHERE s.id = $1
		LIMIT 1`, id).Scan(
		&record.ID, &record.CIDR, &record.PrimaryIP, &record.Device, &record.InstanceID, &record.CreatedAt,
		&instance.ID, &instance.IncusID, &instance.Name, &instance.UserID, &instance.HostID,
		&status, &networkMode,
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

	instance.Status = models.InstanceStatus(status)
	instance.NetworkMode = networkMode
	instance.Host = &host
	record.Instance = &instance
	return &record, nil
}

// Ipv6SubnetsGetByCidr mirrors getIpv6SubnetByCidr. Prisma's findUnique compares
// the raw string, not the normalized range.
func Ipv6SubnetsGetByCidr(ctx context.Context, cidr string) (*models.Ipv6Subnet, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return nil, err
	}
	var subnet models.Ipv6Subnet
	err := pool.QueryRow(ctx, `SELECT id, cidr, primary_ip, device, instance_id, created_at
		FROM ipv6_subnets WHERE cidr = $1 LIMIT 1`, cidr).Scan(
		&subnet.ID, &subnet.CIDR, &subnet.PrimaryIP, &subnet.Device, &subnet.InstanceID, &subnet.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &subnet, nil
}

// Ipv6SubnetsDelete mirrors deleteIpv6Subnet. Prisma's delete raises P2025 when
// the row is gone, which surfaces as a 500.
func Ipv6SubnetsDelete(ctx context.Context, id int32) error {
	if err := ipAddressesDatabaseReady(); err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `DELETE FROM ipv6_subnets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("记录不存在或已被删除")
	}
	return nil
}

// Ipv6SubnetsExists mirrors isIpv6SubnetExists: any overlap with an allocated
// subnet counts as "exists" (that is what makes the pool allocation retry loop
// terminate).
func Ipv6SubnetsExists(ctx context.Context, cidr string) (bool, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return false, err
	}
	candidate, err := Ipv6SubnetsNormalizeCidrRange(cidr)
	if err != nil {
		return false, err
	}

	rows, err := pool.Query(ctx, `SELECT cidr FROM ipv6_subnets`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var allocated string
		if err := rows.Scan(&allocated); err != nil {
			return false, err
		}
		existing, err := Ipv6SubnetsNormalizeCidrRange(allocated)
		if err != nil {
			return false, err
		}
		if Ipv6SubnetsRangesOverlap(candidate, existing) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// Ipv6SubnetsCount mirrors countIpv6Subnets.
func Ipv6SubnetsCount(ctx context.Context, instanceID int32) (int64, error) {
	if err := ipAddressesDatabaseReady(); err != nil {
		return 0, err
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM ipv6_subnets WHERE instance_id = $1`,
		instanceID).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}
