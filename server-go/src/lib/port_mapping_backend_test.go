package lib

// Tests for the port-mapping data-layer wiring (lib/port_mapping_backend.go).
//
// Three things are pinned here:
//
//  1. init() really installs the database backend, so AddInstancePortMapping stops
//     failing with ErrInstancePortMappingBackendUnavailable in production.
//  2. The adapter methods are 1:1 calls into db/port_mappings.go — reachable without
//     a database because the db layer fails loudly when its pool is closed.
//  3. The allocation helpers db/port_mappings.go had to re-implement (package db
//     cannot import lib) produce exactly the same output as the ones in
//     instance_port_mapping.go, so the two copies cannot drift.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// portMappingSequenceRNG returns the given values in order, then repeats the last
// one. Both allocation implementations consume the generator in the same order, so a
// shared sequence is what makes their outputs comparable.
func portMappingSequenceRNG(values ...float64) func() float64 {
	index := 0
	return func() float64 {
		if index >= len(values) {
			return values[len(values)-1]
		}
		value := values[index]
		index++
		return value
	}
}

func TestInstancePortMappingDatabaseBackendIsWired(t *testing.T) {
	if instancePortMappingWiredBackend == nil {
		t.Fatal("init() did not install a port-mapping backend")
	}
	if _, isUnavailable := instancePortMappingWiredBackend.(unavailableInstancePortMappingBackend); isUnavailable {
		t.Fatal("init() installed the failing default instead of the database backend")
	}
	if _, isDatabase := instancePortMappingWiredBackend.(dbInstancePortMappingBackend); !isDatabase {
		t.Fatalf("init() installed %T, want dbInstancePortMappingBackend", instancePortMappingWiredBackend)
	}
}

// TestAddInstancePortMappingReachesTheDatabaseBackend proves the registered backend
// is the db one: the call gets past the seam and fails inside db because no pool is
// open, which is the only honest error without a database.
func TestAddInstancePortMappingReachesTheDatabaseBackend(t *testing.T) {
	if db.Pool() != nil {
		t.Skip("a live database is open; this assertion is only meaningful with the pool closed")
	}

	originalBackend := instancePortMappingBackend
	RegisterInstancePortMappingBackend(instancePortMappingWiredBackend)
	t.Cleanup(func() { instancePortMappingBackend = originalBackend })

	publicPort := int32(20_005)
	_, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance:    testInstance(),
		Host:        testHost(),
		Protocol:    models.ProtocolTcp,
		PrivatePort: 22,
		PublicPort:  &publicPort,
	})
	if errors.Is(err, ErrInstancePortMappingBackendUnavailable) {
		t.Fatalf("AddInstancePortMapping still returns ErrInstancePortMappingBackendUnavailable: %v", err)
	}
	if !errors.Is(err, db.PortMappingsErrDatabaseNotInitialized) {
		t.Fatalf("err = %v, want the db layer's not-initialised error", err)
	}
}

func TestDatabasePortMappingBackendMethodsReachDBLayer(t *testing.T) {
	if db.Pool() != nil {
		t.Skip("a live database is open; these assertions are only meaningful with the pool closed")
	}

	backend := dbInstancePortMappingBackend{}
	ctx := context.Background()

	if _, _, err := backend.AllocatePort(ctx, 7, models.ProtocolTcp); !errors.Is(err, db.PortMappingsErrDatabaseNotInitialized) {
		t.Errorf("AllocatePort err = %v", err)
	}
	if _, err := backend.CheckPortInUse(ctx, 7, 20_005, models.ProtocolTcp); !errors.Is(err, db.PortMappingsErrDatabaseNotInitialized) {
		t.Errorf("CheckPortInUse err = %v", err)
	}
	remark := "web"
	if _, err := backend.CreatePortMapping(ctx, InstancePortMappingCreateInput{
		InstanceID: 11, HostID: 7, Protocol: models.ProtocolTcp,
		PublicPort: 20_005, PrivatePort: 80, Remark: &remark, SystemManaged: true,
	}); !errors.Is(err, db.PortMappingsErrDatabaseNotInitialized) {
		t.Errorf("CreatePortMapping err = %v", err)
	}
	if err := backend.DeletePortMapping(ctx, 1); !errors.Is(err, db.PortMappingsErrDatabaseNotInitialized) {
		t.Errorf("DeletePortMapping err = %v", err)
	}
}

// TestPortMappingUniqueViolationFromPgError covers the pgconn branch of
// instancePortMappingIsUniqueViolation: the unique index
// (host_id, protocol, public_port) is what actually stops two instances sharing a
// port, and Prisma's P2002 surfaces on the wire as SQLSTATE 23505.
func TestPortMappingUniqueViolationFromPgError(t *testing.T) {
	if !instancePortMappingIsUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Error("23505 must be treated as a unique violation")
	}
	if instancePortMappingIsUniqueViolation(&pgconn.PgError{Code: "23503"}) {
		t.Error("a foreign-key violation must not be reported as PORT_IN_USE")
	}

	// End to end through the flow: the reservation fails, rollback runs, and the
	// caller sees PORT_IN_USE rather than INTERNAL_ERROR.
	backend := &fakePortMappingBackend{
		allocatedPort: 20_005, allocatedOK: true,
		createErr: &pgconn.PgError{Code: "23505", ConstraintName: "port_mappings_host_id_protocol_public_port_key"},
	}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)

	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: testInstance(), Host: testHost(), Protocol: models.ProtocolTcp, PrivatePort: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ok || result.Code != ErrPortInUse {
		t.Fatalf("result = %+v, want PORT_IN_USE", result)
	}
	if len(client.added) != 0 {
		t.Errorf("devices added on a failed reservation: %v", client.added)
	}
}

// TestPortMappingAllocationHelpersMatchDB pins the duplicated allocation arithmetic.
// db.PortMappingsAllocateCandidates/SelectPort/Shuffle/AllocatePortsFromCandidates are
// a second copy of these helpers (db cannot import lib), and a silent divergence
// would hand out ports the lib-side flow believes are free.
func TestPortMappingAllocationHelpersMatchDB(t *testing.T) {
	ranges := [][2]int32{{20_000, 20_010}, {1, 2}, {20_005, 20_005}, {0, 5}, {9, 3}}
	usedSets := []map[int32]struct{}{
		{},
		{20_000: struct{}{}},
		{20_005: struct{}{}, 20_006: struct{}{}},
		{20_010: struct{}{}},
	}
	excludedSets := [][]int32{nil, {}, {20_001}, {20_000, 20_010}, {40_000}}

	for _, portRange := range ranges {
		for _, used := range usedSets {
			for _, excluded := range excludedSets {
				libCandidates := InstancePortMappingAllocateCandidates(portRange[0], portRange[1], used, excluded)
				dbCandidates := db.PortMappingsAllocateCandidates(portRange[0], portRange[1], used, excluded)
				if len(libCandidates) != len(dbCandidates) {
					t.Fatalf("candidates differ for range %v used %v excluded %v: %v vs %v",
						portRange, used, excluded, libCandidates, dbCandidates)
				}
				for index := range libCandidates {
					if libCandidates[index] != dbCandidates[index] {
						t.Fatalf("candidates differ for range %v used %v excluded %v: %v vs %v",
							portRange, used, excluded, libCandidates, dbCandidates)
					}
				}

				// Selection: the same pinned RNG must pick the same candidate.
				for _, rngValue := range []float64{0, 0.25, 0.5, 0.99, 1} {
					random := func() float64 { return rngValue }
					libPort, libOK := InstancePortMappingSelectPort(libCandidates, random)
					dbPort, dbOK := db.PortMappingsSelectPort(dbCandidates, random)
					if libOK != dbOK || libPort != dbPort {
						t.Fatalf("select differs for %v at rng %v: (%d,%v) vs (%d,%v)",
							libCandidates, rngValue, libPort, libOK, dbPort, dbOK)
					}
					if got := InstancePortMappingRandomIndex(len(libCandidates), random); got != db.PortMappingsRandomIndex(len(dbCandidates), random) {
						t.Fatalf("random index differs for length %d at rng %v", len(libCandidates), rngValue)
					}
				}

				// Shuffle: identical permutation for an identical RNG sequence.
				for _, count := range []int{0, 1, 2, 5, len(dbCandidates)} {
					libCopy := append([]int32{}, libCandidates...)
					dbCopy := append([]int32{}, dbCandidates...)
					libAllocated := InstancePortMappingAllocatePorts(libCopy, count, portMappingSequenceRNG(0.1, 0.7, 0.3, 0.9, 0.2))
					dbAllocated := db.PortMappingsAllocatePortsFromCandidates(dbCopy, count, portMappingSequenceRNG(0.1, 0.7, 0.3, 0.9, 0.2))
					if len(dbAllocated) == 0 && libAllocated == nil {
						continue
					}
					if len(libAllocated) != len(dbAllocated) {
						t.Fatalf("allocated count differs for %v count %d: %v vs %v",
							dbCandidates, count, libAllocated, dbAllocated)
					}
					for index := range libAllocated {
						if libAllocated[index] != dbAllocated[index] {
							t.Fatalf("allocation differs for %v count %d: %v vs %v",
								dbCandidates, count, libAllocated, dbAllocated)
						}
					}
				}

				// Shuffle in place, same sequence, same result.
				libShuffled := append([]int32{}, libCandidates...)
				dbShuffled := append([]int32{}, dbCandidates...)
				InstancePortMappingShuffle(libShuffled, portMappingSequenceRNG(0.4, 0.8, 0.1))
				db.PortMappingsShuffle(dbShuffled, portMappingSequenceRNG(0.4, 0.8, 0.1))
				for index := range libShuffled {
					if libShuffled[index] != dbShuffled[index] {
						t.Fatalf("shuffle differs for %v: %v vs %v", dbCandidates, libShuffled, dbShuffled)
					}
				}
			}
		}
	}
}
