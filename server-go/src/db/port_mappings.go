// Port of server/src/db/port-mappings.ts (10 exported functions).
//
// Port reservation is a correctness boundary: two instances must never be handed
// the same public port. The database is the final authority — port_mappings has
// UNIQUE (host_id, protocol, public_port) — but the TypeScript also narrows the
// window by checking `checkPortInUse` right before every manual mapping, and the
// route layer serialises per-instance additions.
//
// Faithfulness notes (all verified against the TypeScript, which is authoritative):
//
//   - allocatePort / allocatePorts are NOT wrapped in a transaction and take no row
//     lock. The TypeScript reads the host, reads the used ports, and picks a
//     candidate; adding a transaction here would change call semantics (the
//     function is called from outside any transaction, and a candidate that only
//     exists in memory is invisible to a concurrent allocation either way). The
//     unique index is what actually prevents duplicates, exactly as in Node.
//   - A falsy port range (NULL or 0 at either end) means "no pool": every allocator
//     returns null/[] instead of inventing ports.
//   - getHostPortStats counts DISTINCT public ports (Prisma groupBy + .length), not
//     rows, so one port mapped as both TCP and UDP counts once.
//   - suggestAvailablePorts returns ports in ascending order and uses `<=` (an exact
//     match still returns everything it found); allocatePorts shuffles.
//   - deletePortMapping throws when the row is gone (Prisma P2025 → 500 upstream);
//     the Go port returns PortMappingsErrNotFound rather than a silent success.
//
// package db must not import lib (lib imports db), so the candidate / shuffle /
// select helpers below are a second copy of lib/instance_port_mapping.go's pure
// allocation helpers. lib/port_mapping_backend_test.go asserts the two produce
// identical output for identical input, and the arithmetic is a direct translation
// of the same TypeScript, so they cannot drift silently.
package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// PortMappingsErrDatabaseNotInitialized is returned when the shared pool is not
// open (the package is used before db.Init, or a unit test forgot to skip).
var PortMappingsErrDatabaseNotInitialized = errors.New("database not initialised")

// PortMappingsErrNotFound reproduces Prisma's P2025 on delete: the row existed when
// the caller listed it and is gone now. The TypeScript lets the rejection escape, so
// the route answers 500; silently succeeding would hide a double-delete.
var PortMappingsErrNotFound = errors.New("port mapping not found")

// ==================== Records ====================

// PortMappingRecord mirrors the `PortMapping` interface in
// server/src/types/database.ts: the snake_case projection every db/port-mappings.ts
// function returns, with created_at already formatted as an ISO string. The routes
// return these objects verbatim, so the JSON tags are part of the wire contract.
type PortMappingRecord struct {
	ID            int32           `json:"id"`
	InstanceID    int32           `json:"instance_id"`
	HostID        int32           `json:"host_id"`
	Protocol      models.Protocol `json:"protocol"`
	PublicPort    int32           `json:"public_port"`
	PrivatePort   int32           `json:"private_port"`
	Remark        *string         `json:"remark"`
	CreatedAt     string          `json:"created_at"`
	SystemManaged bool            `json:"system_managed"`
}

// PortMappingCreateInput mirrors the createPortMapping argument object.
type PortMappingCreateInput struct {
	InstanceID  int32
	HostID      int32
	Protocol    models.Protocol
	PublicPort  int32
	PrivatePort int32
	Remark      *string
	// SystemManaged marks the automatic remote port created with the instance. It
	// does not count against the user's port quota; manual mappings are always false.
	SystemManaged bool
}

// PortMappingHostPortStats mirrors the getHostPortStats return object.
type PortMappingHostPortStats struct {
	Total      int32 `json:"total"`
	Used       int32 `json:"used"`
	Available  int32 `json:"available"`
	RangeStart int32 `json:"rangeStart"`
	RangeEnd   int32 `json:"rangeEnd"`
}

// PortMappingConflict mirrors one element of checkPortsInUse's result: which port is
// taken and by which instance.
type PortMappingConflict struct {
	Port       int32 `json:"port"`
	InstanceID int32 `json:"instanceId"`
}

// portMappingColumns is the Prisma `select`/model projection in table order.
const portMappingColumns = `id, instance_id, host_id, protocol, public_port, private_port, remark, created_at, system_managed`

// portMappingsScanner is satisfied by both pgx.Row and pgx.Rows.
type portMappingsScanner interface {
	Scan(dest ...any) error
}

// portMappingISOString mirrors `date.toISOString()`: always UTC, always exactly
// three fractional digits (the column is timestamp(3)).
func portMappingISOString(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// portMappingRecord scans one row of portMappingColumns and formats it the way the
// TypeScript mapper does.
func portMappingRecord(scanner portMappingsScanner) (PortMappingRecord, error) {
	var (
		record    PortMappingRecord
		protocol  string
		createdAt time.Time
	)
	if err := scanner.Scan(&record.ID, &record.InstanceID, &record.HostID, &protocol,
		&record.PublicPort, &record.PrivatePort, &record.Remark, &createdAt, &record.SystemManaged); err != nil {
		return PortMappingRecord{}, err
	}
	record.Protocol = models.Protocol(protocol)
	record.CreatedAt = portMappingISOString(createdAt)
	return record, nil
}

func portMappingsRequireDB() error {
	if Pool() == nil {
		return PortMappingsErrDatabaseNotInitialized
	}
	return nil
}

// portMappingRemark mirrors `remark || null`: an absent or empty remark is stored as
// NULL, never an empty string.
func portMappingRemark(remark *string) *string {
	if remark == nil || *remark == "" {
		return nil
	}
	return remark
}

// portMappingHostPortRange mirrors the `if (!host || !host.nat_port_start ||
// !host.nat_port_end) return null` guard shared by every allocator.
func portMappingHostPortRange(host *HostsRecord) (int32, int32, bool) {
	if host == nil || host.NATPortStart == nil || host.NATPortEnd == nil {
		return 0, 0, false
	}
	start, end := *host.NATPortStart, *host.NATPortEnd
	if start == 0 || end == 0 {
		return 0, 0, false
	}
	return start, end, true
}

// ==================== Pure allocation helpers ====================

// PortMappingsAllocateCandidates mirrors the candidate collection shared by
// allocatePort / allocatePorts / suggestAvailablePorts: walk [portStart, portEnd]
// inclusive, ascending, keep every port that is neither in `used` nor in
// `excluded`.
//
// An impossible range (start or end 0, or start > end) yields no candidates,
// matching the falsy-range guard in the TypeScript callers. The TypeScript's
// `Number.isInteger` filter on excludedPorts is unnecessary here because the
// parameter is already []int32.
func PortMappingsAllocateCandidates(portStart, portEnd int32, used map[int32]struct{}, excluded []int32) []int32 {
	candidates := make([]int32, 0)
	if portStart == 0 || portEnd == 0 || portEnd < portStart {
		return candidates
	}

	blocked := make(map[int32]struct{}, len(used)+len(excluded))
	for port := range used {
		blocked[port] = struct{}{}
	}
	for _, port := range excluded {
		blocked[port] = struct{}{}
	}

	for port := portStart; port <= portEnd; port++ {
		if _, isUsed := blocked[port]; isUsed {
			continue
		}
		candidates = append(candidates, port)
	}
	return candidates
}

// PortMappingsRandomIndex mirrors `Math.floor(Math.random() * length)`.
//
// random is injected for tests; nil means math/rand. The clamp keeps a misbehaving
// generator (a test returning 1.0) inside the slice.
func PortMappingsRandomIndex(length int, random func() float64) int {
	if length <= 0 {
		return -1
	}
	if random == nil {
		random = rand.Float64
	}
	index := int(math.Floor(random() * float64(length)))
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}

// PortMappingsSelectPort mirrors the tail of allocatePort: pick one candidate at
// random. ok=false means the pool is exhausted (allocatePort returns null).
func PortMappingsSelectPort(candidates []int32, random func() float64) (int32, bool) {
	index := PortMappingsRandomIndex(len(candidates), random)
	if index < 0 {
		return 0, false
	}
	return candidates[index], true
}

// PortMappingsShuffle mirrors the in-place Fisher-Yates loop of the TypeScript
// (`for (let i = len - 1; i > 0; i--)`), including the descending direction so a
// pinned RNG produces the same permutation.
func PortMappingsShuffle(values []int32, random func() float64) {
	if random == nil {
		random = rand.Float64
	}
	for i := len(values) - 1; i > 0; i-- {
		j := int(math.Floor(random() * float64(i+1)))
		if j < 0 {
			j = 0
		}
		if j > i {
			j = i
		}
		values[i], values[j] = values[j], values[i]
	}
}

// PortMappingsAllocatePortsFromCandidates mirrors the tail of allocatePorts: when
// fewer candidates exist than requested, every candidate is returned (shuffled);
// otherwise the first `count` entries of a shuffled copy are returned.
//
// count <= 0 yields an empty slice (the route always passes count >= 1; the
// TypeScript's negative-slice quirk is not reproduced).
func PortMappingsAllocatePortsFromCandidates(candidates []int32, count int, random func() float64) []int32 {
	if count <= 0 {
		return []int32{}
	}
	shuffled := append([]int32{}, candidates...)
	PortMappingsShuffle(shuffled, random)
	if len(candidates) < count {
		return shuffled
	}
	return shuffled[:count]
}

// PortMappingsSuggestFromCandidates mirrors the tail of suggestAvailablePorts:
// ascending order, and `availablePorts.length <= count` returns everything.
func PortMappingsSuggestFromCandidates(candidates []int32, count int) []int32 {
	if len(candidates) <= count {
		return candidates
	}
	return candidates[:count]
}

// ==================== Reads ====================

// PortMappingsList mirrors getPortMappings: every mapping of an instance, oldest
// first. `id ASC` is appended as a tiebreaker for the millisecond-precision
// created_at — Node's order on a tie is unspecified, so this only removes
// non-determinism.
func PortMappingsList(ctx context.Context, instanceID int32) ([]PortMappingRecord, error) {
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	rows, err := Pool().Query(ctx,
		"SELECT "+portMappingColumns+" FROM port_mappings WHERE instance_id = $1 ORDER BY created_at ASC, id ASC",
		instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mappings := make([]PortMappingRecord, 0)
	for rows.Next() {
		record, scanErr := portMappingRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		mappings = append(mappings, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mappings, nil
}

// PortMappingsGetByID mirrors getPortMappingById. A missing row is (nil, nil), not
// an error — exactly like `findUnique` returning null.
func PortMappingsGetByID(ctx context.Context, id int32) (*PortMappingRecord, error) {
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	record, err := portMappingRecord(Pool().QueryRow(ctx,
		"SELECT "+portMappingColumns+" FROM port_mappings WHERE id = $1", id))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// PortMappingsCheckPortInUse mirrors checkPortInUse: the first mapping that holds
// (host, port, protocol), or nil when the port is free.
func PortMappingsCheckPortInUse(ctx context.Context, hostID, port int32, protocol models.Protocol) (*PortMappingRecord, error) {
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	record, err := portMappingRecord(Pool().QueryRow(ctx,
		"SELECT "+portMappingColumns+" FROM port_mappings WHERE host_id = $1 AND public_port = $2 AND protocol = $3 LIMIT 1",
		hostID, port, string(protocol)))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// PortMappingsCheckPortsInUse mirrors checkPortsInUse: the subset of `ports` that is
// already taken on the host, with the instance holding each one. An empty input
// returns an empty slice without touching the database.
func PortMappingsCheckPortsInUse(ctx context.Context, hostID int32, ports []int32, protocol models.Protocol) ([]PortMappingConflict, error) {
	conflicts := make([]PortMappingConflict, 0)
	if len(ports) == 0 {
		return conflicts, nil
	}
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	rows, err := Pool().Query(ctx,
		`SELECT public_port, instance_id FROM port_mappings
		 WHERE host_id = $1 AND protocol = $2 AND public_port = ANY($3)`,
		hostID, string(protocol), ports)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var conflict PortMappingConflict
		if err := rows.Scan(&conflict.Port, &conflict.InstanceID); err != nil {
			return nil, err
		}
		conflicts = append(conflicts, conflict)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return conflicts, nil
}

// PortMappingsHostPortStats mirrors getHostPortStats.
//
// `used` is the number of DISTINCT public ports (Prisma groupBy by publicPort), so a
// port mapped as both TCP and UDP counts once; the TypeScript never subtracts that
// subtlety, and neither does this port.
func PortMappingsHostPortStats(ctx context.Context, hostID int32) (*PortMappingHostPortStats, error) {
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	host, err := HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	start, end, ok := portMappingHostPortRange(host)
	if !ok {
		return nil, nil
	}

	var used int32
	if err := Pool().QueryRow(ctx,
		"SELECT COUNT(DISTINCT public_port) FROM port_mappings WHERE host_id = $1", hostID).Scan(&used); err != nil {
		return nil, err
	}

	total := end - start + 1
	return &PortMappingHostPortStats{
		Total:      total,
		Used:       used,
		Available:  total - used,
		RangeStart: start,
		RangeEnd:   end,
	}, nil
}

// portMappingsUsedPorts loads the ports already taken on a host for one protocol.
func portMappingsUsedPorts(ctx context.Context, hostID int32, protocol models.Protocol) (map[int32]struct{}, error) {
	rows, err := Pool().Query(ctx,
		"SELECT public_port FROM port_mappings WHERE host_id = $1 AND protocol = $2",
		hostID, string(protocol))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	used := make(map[int32]struct{})
	for rows.Next() {
		var port int32
		if err := rows.Scan(&port); err != nil {
			return nil, err
		}
		used[port] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return used, nil
}

// portMappingsUsedPortsForMode mirrors the checkBothProtocols branch: both protocols
// share the same numeric pool, so a port taken by UDP must not be handed out for TCP.
func portMappingsUsedPortsForMode(ctx context.Context, hostID int32, protocol models.Protocol, checkBothProtocols bool) (map[int32]struct{}, error) {
	if !checkBothProtocols {
		return portMappingsUsedPorts(ctx, hostID, protocol)
	}

	tcpPorts, err := portMappingsUsedPorts(ctx, hostID, models.ProtocolTcp)
	if err != nil {
		return nil, err
	}
	udpPorts, err := portMappingsUsedPorts(ctx, hostID, models.ProtocolUdp)
	if err != nil {
		return nil, err
	}
	for port := range udpPorts {
		tcpPorts[port] = struct{}{}
	}
	return tcpPorts, nil
}

// ==================== Allocation ====================

// PortMappingsAllocatePort mirrors allocatePort. ok=false means "no port available"
// and is the Go spelling of the TypeScript's `null`.
func PortMappingsAllocatePort(ctx context.Context, hostID int32, protocol models.Protocol, excludedPorts []int32) (int32, bool, error) {
	if err := portMappingsRequireDB(); err != nil {
		return 0, false, err
	}

	host, err := HostsGetByID(ctx, hostID)
	if err != nil {
		return 0, false, err
	}
	start, end, ok := portMappingHostPortRange(host)
	if !ok {
		return 0, false, nil
	}

	used, err := portMappingsUsedPorts(ctx, hostID, protocol)
	if err != nil {
		return 0, false, err
	}

	candidates := PortMappingsAllocateCandidates(start, end, used, excludedPorts)
	port, found := PortMappingsSelectPort(candidates, nil)
	if !found {
		return 0, false, nil
	}
	return port, true, nil
}

// PortMappingsAllocatePorts mirrors allocatePorts.
func PortMappingsAllocatePorts(ctx context.Context, hostID int32, count int, protocol models.Protocol, checkBothProtocols bool) ([]int32, error) {
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	host, err := HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	start, end, ok := portMappingHostPortRange(host)
	if !ok {
		return []int32{}, nil
	}

	used, err := portMappingsUsedPortsForMode(ctx, hostID, protocol, checkBothProtocols)
	if err != nil {
		return nil, err
	}

	candidates := PortMappingsAllocateCandidates(start, end, used, nil)
	return PortMappingsAllocatePortsFromCandidates(candidates, count, nil), nil
}

// PortMappingsSuggestAvailablePorts mirrors suggestAvailablePorts.
func PortMappingsSuggestAvailablePorts(ctx context.Context, hostID int32, count int, protocol models.Protocol, excludePorts []int32, checkBothProtocols bool) ([]int32, error) {
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	host, err := HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	start, end, ok := portMappingHostPortRange(host)
	if !ok {
		return []int32{}, nil
	}

	used, err := portMappingsUsedPortsForMode(ctx, hostID, protocol, checkBothProtocols)
	if err != nil {
		return nil, err
	}

	candidates := PortMappingsAllocateCandidates(start, end, used, excludePorts)
	return PortMappingsSuggestFromCandidates(candidates, count), nil
}

// ==================== Writes ====================

// portMappingsInsertQuery stores created_at through the column default
// (CURRENT_TIMESTAMP), exactly like Prisma's @default(now()); port_mappings has no
// updated_at column, so nothing else has to be supplied.
const portMappingsInsertQuery = `INSERT INTO port_mappings
	(instance_id, host_id, protocol, public_port, private_port, remark, system_managed)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	RETURNING id`

// PortMappingsCreate mirrors createPortMapping: it returns the new row id.
//
// A duplicate (host, protocol, public_port) surfaces as a PostgreSQL unique
// violation, which is how lib/instance-port-mapping.go maps P2002 to PORT_IN_USE.
func PortMappingsCreate(ctx context.Context, input PortMappingCreateInput) (int32, error) {
	if err := portMappingsRequireDB(); err != nil {
		return 0, err
	}

	var id int32
	if err := Pool().QueryRow(ctx, portMappingsInsertQuery,
		input.InstanceID, input.HostID, string(input.Protocol), input.PublicPort, input.PrivatePort,
		portMappingRemark(input.Remark), input.SystemManaged,
	).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// PortMappingsCreateBatch mirrors createPortMappingsBatch: Prisma's array
// $transaction runs the creates sequentially in one transaction, so a failure
// halfway leaves nothing behind.
func PortMappingsCreateBatch(ctx context.Context, items []PortMappingCreateInput) ([]int32, error) {
	ids := make([]int32, 0, len(items))
	if len(items) == 0 {
		return ids, nil
	}
	if err := portMappingsRequireDB(); err != nil {
		return nil, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Rollback after a successful commit is a no-op in pgx.
		_ = tx.Rollback(ctx)
	}()

	for index, item := range items {
		var id int32
		if err := tx.QueryRow(ctx, portMappingsInsertQuery,
			item.InstanceID, item.HostID, string(item.Protocol), item.PublicPort, item.PrivatePort,
			portMappingRemark(item.Remark), item.SystemManaged,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("port mapping %d: %w", index, err)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

// PortMappingsDelete mirrors deletePortMapping.
//
// Prisma's delete throws P2025 when the row is missing; this returns
// PortMappingsErrNotFound for the same reason (a silent no-op would let two
// concurrent deletes both report success).
func PortMappingsDelete(ctx context.Context, id int32) error {
	if err := portMappingsRequireDB(); err != nil {
		return err
	}

	tag, err := Pool().Exec(ctx, "DELETE FROM port_mappings WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return PortMappingsErrNotFound
	}
	return nil
}
