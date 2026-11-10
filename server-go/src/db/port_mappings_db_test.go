package db

// Database-backed verification of the hand-written SQL in port_mappings.go.
//
// It is skipped unless PAYINCUS_TEST_DATABASE_URL points at a THROWAWAY database
// that has the Prisma schema applied (never a production/development database — the
// ambient DATABASE_URL is deliberately not read). Everything it creates is keyed by
// fixed high ids and removed again, so it is safe to run against a shared scratch
// database and safe to re-run after an aborted run.
//
// What it proves that the pure tests cannot:
//   - the INSERT does not violate the bool/enum/NOT NULL columns, and created_at is
//     filled by the column default exactly like Prisma's @default(now());
//   - `protocol = $3` accepts a Go string for the PostgreSQL enum "Protocol";
//   - `public_port = ANY($3)` binds a []int32;
//   - COUNT(DISTINCT public_port) is what getHostPortStats reports;
//   - a duplicate (host, protocol, public_port) raises SQLSTATE 23505 — the
//     database-level guard the allocator relies on;
//   - a duplicate inside createPortMappingsBatch rolls the whole transaction back.

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Fixture ids, far above anything the application allocates from its sequences.
const (
	portMappingTestUserID     int32 = 990_001
	portMappingTestHostID     int32 = 990_001
	portMappingTestInstanceID int32 = 990_101
	portMappingTestInstance2  int32 = 990_102
)

func portMappingTestDatabase(t *testing.T) context.Context {
	t.Helper()

	dsn := os.Getenv("PAYINCUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PAYINCUS_TEST_DATABASE_URL to a throwaway database with the Prisma schema")
	}

	ctx := context.Background()
	// Only the explicit test DSN is ever used; an ambient DATABASE_URL is overwritten.
	t.Setenv("DATABASE_URL", dsn)
	if err := Init(ctx); err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() {
		Close()
		// Leave the package as the pure tests expect it (no half-open handle).
		pool = nil
		DB = nil
	})

	cleanup := func() {
		_, _ = Pool().Exec(ctx, "DELETE FROM port_mappings WHERE host_id = $1", portMappingTestHostID)
		_, _ = Pool().Exec(ctx, "DELETE FROM instances WHERE id = ANY($1)",
			[]int32{portMappingTestInstanceID, portMappingTestInstance2})
		_, _ = Pool().Exec(ctx, "DELETE FROM hosts WHERE id = $1", portMappingTestHostID)
		_, _ = Pool().Exec(ctx, "DELETE FROM users WHERE id = $1", portMappingTestUserID)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := Pool().Exec(ctx,
		`INSERT INTO users (id, username, password_hash, updated_at) VALUES ($1, $2, $3, $4)`,
		portMappingTestUserID, "portmap-test-user", "x", now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := Pool().Exec(ctx,
		`INSERT INTO hosts (id, user_id, name, url, nat_port_start, nat_port_end, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		portMappingTestHostID, portMappingTestUserID, "portmap-test-host", "https://127.0.0.1:8443",
		int32(20_000), int32(20_009), now); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	for _, instanceID := range []int32{portMappingTestInstanceID, portMappingTestInstance2} {
		if _, err := Pool().Exec(ctx,
			`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, cpu, memory, disk, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, 1, 512, 5120, $7)`,
			instanceID, "portmap-test-"+string(rune('a'+instanceID-990_101)), "portmap-test",
			portMappingTestUserID, portMappingTestHostID, "debian/12", now); err != nil {
			t.Fatalf("insert instance %d: %v", instanceID, err)
		}
	}

	return ctx
}

func TestPortMappingsDatabaseCRUD(t *testing.T) {
	ctx := portMappingTestDatabase(t)

	remark := "  web  "
	id, err := PortMappingsCreate(ctx, PortMappingCreateInput{
		InstanceID: portMappingTestInstanceID, HostID: portMappingTestHostID,
		Protocol: models.ProtocolTcp, PublicPort: 20_000, PrivatePort: 80,
		Remark: &remark, SystemManaged: true,
	})
	if err != nil {
		t.Fatalf("PortMappingsCreate: %v", err)
	}

	record, err := PortMappingsGetByID(ctx, id)
	if err != nil {
		t.Fatalf("PortMappingsGetByID: %v", err)
	}
	if record == nil {
		t.Fatal("created mapping is not readable")
	}
	if record.InstanceID != portMappingTestInstanceID || record.HostID != portMappingTestHostID ||
		record.Protocol != models.ProtocolTcp || record.PublicPort != 20_000 || record.PrivatePort != 80 {
		t.Errorf("record = %+v", record)
	}
	if record.Remark == nil || *record.Remark != remark {
		t.Errorf("remark = %v, want the untrimmed %q (the caller trims)", record.Remark, remark)
	}
	if !record.SystemManaged {
		t.Error("system_managed was not persisted")
	}
	// created_at is filled by the column default and formatted like toISOString().
	iso := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	if !iso.MatchString(record.CreatedAt) {
		t.Errorf("created_at = %q, want an ISO-8601 UTC string", record.CreatedAt)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", record.CreatedAt)
	if err != nil {
		t.Fatalf("created_at is not parseable: %v", err)
	}
	if delta := time.Since(parsed); delta < -time.Minute || delta > time.Minute {
		t.Errorf("created_at %q is not close to now (%v)", record.CreatedAt, delta)
	}

	// `remark || null`: an empty remark is stored as NULL.
	empty := ""
	nullRemarkID, err := PortMappingsCreate(ctx, PortMappingCreateInput{
		InstanceID: portMappingTestInstanceID, HostID: portMappingTestHostID,
		Protocol: models.ProtocolTcp, PublicPort: 20_001, PrivatePort: 443, Remark: &empty,
	})
	if err != nil {
		t.Fatalf("PortMappingsCreate (empty remark): %v", err)
	}
	nullRecord, err := PortMappingsGetByID(ctx, nullRemarkID)
	if err != nil || nullRecord == nil {
		t.Fatalf("read back: %v %v", nullRecord, err)
	}
	if nullRecord.Remark != nil {
		t.Errorf("empty remark = %q, want NULL", *nullRecord.Remark)
	}
	if nullRecord.SystemManaged {
		t.Error("system_managed must default to false")
	}

	// The list is oldest-first.
	second, err := PortMappingsCreate(ctx, PortMappingCreateInput{
		InstanceID: portMappingTestInstanceID, HostID: portMappingTestHostID,
		Protocol: models.ProtocolUdp, PublicPort: 20_002, PrivatePort: 53,
	})
	if err != nil {
		t.Fatalf("PortMappingsCreate (udp): %v", err)
	}
	mappings, err := PortMappingsList(ctx, portMappingTestInstanceID)
	if err != nil {
		t.Fatalf("PortMappingsList: %v", err)
	}
	if len(mappings) != 3 {
		t.Fatalf("list = %d rows, want 3", len(mappings))
	}
	if mappings[0].ID != id || mappings[1].ID != nullRemarkID || mappings[2].ID != second {
		t.Errorf("list order = %d,%d,%d; want insertion order", mappings[0].ID, mappings[1].ID, mappings[2].ID)
	}

	// A missing row is (nil, nil), like findUnique returning null.
	if missing, err := PortMappingsGetByID(ctx, 0); err != nil || missing != nil {
		t.Errorf("missing row = %v, %v", missing, err)
	}

	// checkPortInUse is protocol- and host-scoped.
	found, err := PortMappingsCheckPortInUse(ctx, portMappingTestHostID, 20_000, models.ProtocolTcp)
	if err != nil || found == nil || found.ID != id {
		t.Errorf("checkPortInUse tcp = %+v, %v", found, err)
	}
	if found, err := PortMappingsCheckPortInUse(ctx, portMappingTestHostID, 20_000, models.ProtocolUdp); err != nil || found != nil {
		t.Errorf("checkPortInUse udp = %+v, %v; the port is only taken for tcp", found, err)
	}

	// checkPortsInUse binds an int array through ANY($3).
	conflicts, err := PortMappingsCheckPortsInUse(ctx, portMappingTestHostID, []int32{20_000, 20_001, 20_005}, models.ProtocolTcp)
	if err != nil {
		t.Fatalf("PortMappingsCheckPortsInUse: %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("conflicts = %+v, want 2", conflicts)
	}
	for _, conflict := range conflicts {
		if conflict.Port == 20_000 && conflict.InstanceID != portMappingTestInstanceID {
			t.Errorf("conflict owner = %+v", conflict)
		}
	}
	if empty, err := PortMappingsCheckPortsInUse(ctx, portMappingTestHostID, nil, models.ProtocolTcp); err != nil || len(empty) != 0 {
		t.Errorf("empty port list = %v, %v", empty, err)
	}

	// Delete, then delete again: Prisma throws P2025 the second time.
	if err := PortMappingsDelete(ctx, second); err != nil {
		t.Fatalf("PortMappingsDelete: %v", err)
	}
	if err := PortMappingsDelete(ctx, second); !errors.Is(err, PortMappingsErrNotFound) {
		t.Errorf("second delete = %v, want PortMappingsErrNotFound", err)
	}
}

func TestPortMappingsDatabaseAllocation(t *testing.T) {
	ctx := portMappingTestDatabase(t)

	// Take 20000 (tcp), 20001 (tcp) and 20002 (udp).
	seed := []PortMappingCreateInput{
		{InstanceID: portMappingTestInstanceID, HostID: portMappingTestHostID, Protocol: models.ProtocolTcp, PublicPort: 20_000, PrivatePort: 80},
		{InstanceID: portMappingTestInstanceID, HostID: portMappingTestHostID, Protocol: models.ProtocolTcp, PublicPort: 20_001, PrivatePort: 443},
		{InstanceID: portMappingTestInstance2, HostID: portMappingTestHostID, Protocol: models.ProtocolUdp, PublicPort: 20_002, PrivatePort: 53},
	}
	ids, err := PortMappingsCreateBatch(ctx, seed)
	if err != nil {
		t.Fatalf("PortMappingsCreateBatch: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("batch ids = %v", ids)
	}

	// A single allocation never hands out a port the protocol already uses.
	for attempt := 0; attempt < 20; attempt++ {
		port, ok, err := PortMappingsAllocatePort(ctx, portMappingTestHostID, models.ProtocolTcp, nil)
		if err != nil {
			t.Fatalf("PortMappingsAllocatePort: %v", err)
		}
		if !ok {
			t.Fatalf("allocation %d reported an exhausted pool", attempt)
		}
		if port < 20_000 || port > 20_009 {
			t.Fatalf("allocated port %d is outside the host pool", port)
		}
		if port == 20_000 || port == 20_001 {
			t.Fatalf("allocated a port the tcp pool already holds: %d", port)
		}
		// 20002 is taken for UDP only; a tcp-only allocation may legitimately reuse the
		// number, exactly like the TypeScript (which filters by protocol).
	}

	// Exclusions are honoured. TCP holds 20000/20001, so excluding 20002..20008
	// leaves exactly 20009.
	exclusions := []int32{20_002, 20_003, 20_004, 20_005, 20_006, 20_007, 20_008}
	if port, ok, err := PortMappingsAllocatePort(ctx, portMappingTestHostID, models.ProtocolTcp, exclusions); err != nil || !ok || port != 20_009 {
		t.Errorf("allocation with exclusions = %d, %v, %v; want 20009", port, ok, err)
	}

	// Exhaust the pool for UDP and require ok=false.
	all := make([]PortMappingCreateInput, 0, 10)
	for port := int32(20_000); port <= 20_009; port++ {
		all = append(all, PortMappingCreateInput{
			InstanceID: portMappingTestInstance2, HostID: portMappingTestHostID,
			Protocol: models.ProtocolUdp, PublicPort: port, PrivatePort: port,
		})
	}
	// 20002 already exists for udp, so use the batch to fill the rest only.
	fill := all[:2]
	fill = append(fill, all[3:]...)
	if _, err := PortMappingsCreateBatch(ctx, fill); err != nil {
		t.Fatalf("filling the udp pool: %v", err)
	}
	if port, ok, err := PortMappingsAllocatePort(ctx, portMappingTestHostID, models.ProtocolUdp, nil); err != nil || ok {
		t.Errorf("exhausted pool = %d, %v, %v; want ok=false", port, ok, err)
	}

	// checkBothProtocols: TCP cannot take a port UDP holds.
	suggested, err := PortMappingsSuggestAvailablePorts(ctx, portMappingTestHostID, 100, models.ProtocolTcp, nil, true)
	if err != nil {
		t.Fatalf("PortMappingsSuggestAvailablePorts: %v", err)
	}
	if len(suggested) != 0 {
		t.Errorf("suggestions with both protocols = %v, want none", suggested)
	}

	// And with TCP-only scope the remaining TCP ports come back ascending.
	tcpOnly, err := PortMappingsSuggestAvailablePorts(ctx, portMappingTestHostID, 2, models.ProtocolTcp, nil, false)
	if err != nil {
		t.Fatalf("PortMappingsSuggestAvailablePorts: %v", err)
	}
	if len(tcpOnly) != 2 || tcpOnly[0] != 20_002 || tcpOnly[1] != 20_003 {
		t.Errorf("tcp suggestions = %v, want [20002 20003]", tcpOnly)
	}

	// AllocatePorts can house a multi-port instance, and skips both protocols.
	batch, err := PortMappingsAllocatePorts(ctx, portMappingTestHostID, 2, models.ProtocolTcp, true)
	if err != nil {
		t.Fatalf("PortMappingsAllocatePorts: %v", err)
	}
	if len(batch) != 0 {
		t.Errorf("allocation with both protocols = %v, want none", batch)
	}

	// hostPortStats counts DISTINCT ports: 20002 exists for udp only, so the union
	// over both protocols is 10 distinct ports → used 10.
	stats, err := PortMappingsHostPortStats(ctx, portMappingTestHostID)
	if err != nil {
		t.Fatalf("PortMappingsHostPortStats: %v", err)
	}
	if stats == nil {
		t.Fatal("stats are nil for a host with a pool")
	}
	if stats.Total != 10 || stats.RangeStart != 20_000 || stats.RangeEnd != 20_009 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.Used != 10 || stats.Available != 0 {
		t.Errorf("stats used/available = %d/%d, want 10/0", stats.Used, stats.Available)
	}
}

func TestPortMappingsDatabaseUniqueAndTransactionRollback(t *testing.T) {
	ctx := portMappingTestDatabase(t)

	if _, err := PortMappingsCreate(ctx, PortMappingCreateInput{
		InstanceID: portMappingTestInstanceID, HostID: portMappingTestHostID,
		Protocol: models.ProtocolTcp, PublicPort: 20_003, PrivatePort: 80,
	}); err != nil {
		t.Fatalf("PortMappingsCreate: %v", err)
	}

	// The same (host, protocol, port) again: PostgreSQL unique_violation. This is
	// what lib/instance-port-mapping.go turns into PORT_IN_USE.
	_, err := PortMappingsCreate(ctx, PortMappingCreateInput{
		InstanceID: portMappingTestInstance2, HostID: portMappingTestHostID,
		Protocol: models.ProtocolTcp, PublicPort: 20_003, PrivatePort: 81,
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate create = %v, want SQLSTATE 23505", err)
	}

	// A batch that clashes halfway must leave nothing behind (Prisma's array
	// $transaction is atomic in the same way).
	before, err := PortMappingsList(ctx, portMappingTestInstance2)
	if err != nil || len(before) != 0 {
		t.Fatalf("precondition: instance 2 has %d mappings, %v", len(before), err)
	}
	_, err = PortMappingsCreateBatch(ctx, []PortMappingCreateInput{
		{InstanceID: portMappingTestInstance2, HostID: portMappingTestHostID, Protocol: models.ProtocolTcp, PublicPort: 20_004, PrivatePort: 80},
		{InstanceID: portMappingTestInstance2, HostID: portMappingTestHostID, Protocol: models.ProtocolTcp, PublicPort: 20_003, PrivatePort: 81},
	})
	if err == nil {
		t.Fatal("a batch containing a duplicate must fail")
	}
	after, err := PortMappingsList(ctx, portMappingTestInstance2)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("batch left %d rows behind after a failure: %+v", len(after), after)
	}
}

// TestPortMappingsDatabaseHostWithoutPool pins the "no pool" contract against real
// NULL columns.
func TestPortMappingsDatabaseHostWithoutPool(t *testing.T) {
	ctx := portMappingTestDatabase(t)

	if _, err := Pool().Exec(ctx, "UPDATE hosts SET nat_port_start = NULL, nat_port_end = NULL WHERE id = $1", portMappingTestHostID); err != nil {
		t.Fatalf("clear pool: %v", err)
	}

	if port, ok, err := PortMappingsAllocatePort(ctx, portMappingTestHostID, models.ProtocolTcp, nil); err != nil || ok {
		t.Errorf("allocate on a pool-less host = %d, %v, %v", port, ok, err)
	}
	if ports, err := PortMappingsAllocatePorts(ctx, portMappingTestHostID, 3, models.ProtocolTcp, false); err != nil || len(ports) != 0 {
		t.Errorf("allocatePorts on a pool-less host = %v, %v", ports, err)
	}
	if stats, err := PortMappingsHostPortStats(ctx, portMappingTestHostID); err != nil || stats != nil {
		t.Errorf("stats on a pool-less host = %+v, %v", stats, err)
	}
}
