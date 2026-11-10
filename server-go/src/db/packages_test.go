package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// packageDryRunDB builds an offline GORM handle. sql.Open does not dial and
// DisableAutomaticPing keeps gorm.Open from probing the server, so the SQL
// builder can be inspected without a database.
func packageDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	handle, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=127.0.0.1 user=test dbname=test sslmode=disable",
	}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
		Logger:               gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("open dry-run gorm: %v", err)
	}
	return handle
}

func packageInt32Ptr(value int32) *int32 { return &value }

func TestPackagePrerequisiteLockSQLShape(t *testing.T) {
	if PackagePrerequisiteLockNamespace != 4201 {
		t.Fatalf("PackagePrerequisiteLockNamespace = %d, want 4201", PackagePrerequisiteLockNamespace)
	}

	handle := packageDryRunDB(t)
	sql := handle.ToSQL(func(tx *gorm.DB) *gorm.DB {
		if err := packagePrerequisiteLock(tx); err != nil {
			t.Fatalf("lock: %v", err)
		}
		return tx
	})
	// GORM inlines the namespace argument, so this pins both the namespace and the
	// constant key 1 as the database will see them.
	if !strings.Contains(sql, "pg_advisory_xact_lock(4201, 1)") {
		t.Fatalf("SQL = %q, want pg_advisory_xact_lock(4201, 1)", sql)
	}
	if !strings.HasPrefix(sql, "WITH acquired_lock AS (SELECT pg_advisory_xact_lock(") {
		t.Fatalf("SQL = %q, want the CTE shape the TypeScript uses", sql)
	}
}

func TestPackageEgressLockSQLShape(t *testing.T) {
	if EgressAdvisoryLockNamespace != 74201 {
		t.Fatalf("EgressAdvisoryLockNamespace = %d, want 74201", EgressAdvisoryLockNamespace)
	}
	handle := packageDryRunDB(t)
	sql := handle.ToSQL(func(tx *gorm.DB) *gorm.DB {
		if err := packageEgressAdvisoryLock(tx); err != nil {
			t.Fatalf("lock: %v", err)
		}
		return tx
	})
	// The SINGLE-argument form: pg_advisory_xact_lock(74201), not (74201, key).
	if !strings.Contains(sql, "pg_advisory_xact_lock(74201::bigint)") {
		t.Fatalf("SQL = %q, want the single-argument 74201 lock", sql)
	}
}

// fakePrerequisiteReader models the prerequisite graph without a database.
type fakePrerequisiteReader struct {
	rows map[int32]struct {
		required *int32
		owner    int32
	}
	calls []int32
}

func (reader *fakePrerequisiteReader) packageRequiredIDAndOwner(_ context.Context, packageID int32) (*int32, int32, bool, error) {
	reader.calls = append(reader.calls, packageID)
	row, ok := reader.rows[packageID]
	if !ok {
		return nil, 0, false, nil
	}
	return row.required, row.owner, true, nil
}

func TestValidatePrerequisiteReferenceBranches(t *testing.T) {
	reader := &fakePrerequisiteReader{rows: map[int32]struct {
		required *int32
		owner    int32
	}{
		10: {required: nil, owner: 1},
		11: {required: packageInt32Ptr(10), owner: 1},
		12: {required: packageInt32Ptr(11), owner: 1},
		13: {required: packageInt32Ptr(14), owner: 1},
		14: {required: packageInt32Ptr(13), owner: 1},
		20: {required: nil, owner: 2},
	}}
	ownerOne := int32(1)

	cases := []struct {
		name        string
		packageID   *int32
		required    *int32
		owner       *int32
		wantValid   bool
		wantCode    string
		wantMessage string
	}{
		{"absent requirement", nil, nil, nil, true, "", ""},
		{"zero is invalid", nil, packageInt32Ptr(0), nil, false, "PACKAGE_PREREQUISITE_INVALID", "Required package is invalid"},
		{"negative is invalid", nil, packageInt32Ptr(-3), nil, false, "PACKAGE_PREREQUISITE_INVALID", "Required package is invalid"},
		{"self reference", packageInt32Ptr(10), packageInt32Ptr(10), nil, false, "PACKAGE_PREREQUISITE_SELF", "Package cannot require itself"},
		{"missing package", nil, packageInt32Ptr(99), nil, false, "PACKAGE_PREREQUISITE_INVALID", "Required package does not exist"},
		{"wrong owner", nil, packageInt32Ptr(20), &ownerOne, false, "PACKAGE_PREREQUISITE_SCOPE", "Required package must belong to the same owner"},
		{"direct cycle back to self", packageInt32Ptr(10), packageInt32Ptr(11), nil, false, "PACKAGE_PREREQUISITE_CYCLE", "Package prerequisite cannot form a cycle"},
		{"chain ending at the edited package", packageInt32Ptr(10), packageInt32Ptr(12), nil, false, "PACKAGE_PREREQUISITE_CYCLE", "Package prerequisite cannot form a cycle"},
		{"stored cycle without the edited package", nil, packageInt32Ptr(13), nil, false, "PACKAGE_PREREQUISITE_CYCLE", "Package prerequisite cannot form a cycle"},
		{"valid chain", packageInt32Ptr(5), packageInt32Ptr(12), nil, true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := packageValidatePrerequisiteReferenceWithClient(t.Context(), reader, tc.packageID, tc.required, tc.owner)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v (result %+v)", result.Valid, tc.wantValid, result)
			}
			if result.Code != tc.wantCode || result.Message != tc.wantMessage {
				t.Fatalf("code/message = %q/%q, want %q/%q", result.Code, result.Message, tc.wantCode, tc.wantMessage)
			}
		})
	}
}

func TestPackageNormalizeHostStoragePools(t *testing.T) {
	space := "   "
	named := "  data  "
	normalized, err := packageNormalizeHostStoragePools(map[string]*string{
		"1":  &named,
		"01": nil,
		"2":  &space,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(normalized) != 2 {
		t.Fatalf("len = %d, want 2 (01 canonicalises onto 1): %#v", len(normalized), normalized)
	}
	// Measured from the TypeScript: Object.entries() visits the canonical index
	// keys first ("1", "2") and the non-canonical "01" last, so "01" overwrites
	// host 1 with null. The Go side must be deterministic here, not depend on map
	// iteration order.
	if normalized["1"] != nil {
		t.Fatalf("host 1 pool = %q, want null (the \"01\" key canonicalises onto it last)", *normalized["1"])
	}
	if normalized["2"] != nil {
		t.Fatalf("whitespace-only pool must become null, got %q", *normalized["2"])
	}

	// Determinism: 200 runs must agree, otherwise the map iteration order leaks.
	for i := 0; i < 200; i++ {
		again, err := packageNormalizeHostStoragePools(map[string]*string{
			"1":  &named,
			"01": nil,
			"2":  &space,
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if again["1"] != nil {
			t.Fatalf("run %d: nondeterministic result for host 1: %q", i, *again["1"])
		}
	}

	if _, err := packageNormalizeHostStoragePools(map[string]*string{"abc": nil}); err == nil {
		t.Fatal("a non-numeric host key must be rejected")
	} else if err.Error() != "Invalid hostStoragePools key: abc" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestPackageNormalizeHostTrafficMultipliers(t *testing.T) {
	normalized, err := packageNormalizeHostTrafficMultipliers(map[string]any{
		"7":     1.5,
		"8":     "2.25",
		"9":     nil,
		"10":    "",
		"00012": 100.0,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := map[string]float64{"7": 1.5, "8": 2.25, "9": 1, "10": 1, "12": 100}
	for key, value := range want {
		if normalized[key] != value {
			t.Fatalf("multiplier[%s] = %v, want %v", key, normalized[key], value)
		}
	}

	if _, err := packageNormalizeHostTrafficMultipliers(map[string]any{"7": 0.0001}); err == nil {
		t.Fatal("an out-of-range multiplier must be rejected")
	}
	if _, err := packageNormalizeHostTrafficMultipliers(map[string]any{"nope": 1}); err == nil {
		t.Fatal("a non-numeric host key must be rejected")
	}
}

func TestPackageValidateHostTrafficMultipliers(t *testing.T) {
	hostIDs := []int32{1, 2}
	if err := packageValidateHostTrafficMultipliers(hostIDs, map[string]float64{"1": 1, "2": 2}); err != nil {
		t.Fatalf("bound hosts must pass: %v", err)
	}
	err := packageValidateHostTrafficMultipliers(hostIDs, map[string]float64{"3": 1})
	if err == nil || err.Error() != "Traffic multiplier mapping references an unbound host: 3" {
		t.Fatalf("error = %v", err)
	}
}

func TestPackageResolveHostStoragePool(t *testing.T) {
	requested := "requested"
	preferred := "preferred"
	current := "current"

	pool, err := packageResolveHostStoragePool(1, true, &requested, &current, &preferred, true)
	if err != nil || pool == nil || *pool != "requested" {
		t.Fatalf("requested must win: %v %v", pool, err)
	}

	empty := ""
	pool, err = packageResolveHostStoragePool(1, true, &empty, &current, &preferred, true)
	if err != nil || pool == nil || *pool != "preferred" {
		t.Fatalf("an empty requested pool must fall back to the preferred pool: %v %v", pool, err)
	}

	pool, err = packageResolveHostStoragePool(1, false, &requested, &current, &preferred, true)
	if err != nil || pool == nil || *pool != "current" {
		t.Fatalf("an untouched host must keep its current pool: %v %v", pool, err)
	}

	if _, err := packageResolveHostStoragePool(7, false, nil, nil, nil, true); err == nil {
		t.Fatal("an active package without a pool must be rejected")
	} else if err.Error() != "Active package must bind a system disk storage pool on host 7" {
		t.Fatalf("error = %q", err.Error())
	}

	pool, err = packageResolveHostStoragePool(7, false, nil, nil, nil, false)
	if err != nil || pool != nil {
		t.Fatalf("an inactive package may leave the pool null: %v %v", pool, err)
	}
}

func TestPackageInt32ArrayLiteral(t *testing.T) {
	cases := []struct {
		values []int32
		want   string
	}{
		{nil, "{}"},
		{[]int32{}, "{}"},
		{[]int32{1}, "{1}"},
		{[]int32{1, 2, 3}, "{1,2,3}"},
		{[]int32{-5}, "{-5}"},
	}
	for _, tc := range cases {
		if got := packageInt32ArrayLiteral(tc.values); got != tc.want {
			t.Fatalf("packageInt32ArrayLiteral(%v) = %q, want %q", tc.values, got, tc.want)
		}
	}
}

func TestPackageJSONInt32Array(t *testing.T) {
	raw := func(value string) *string { return &value }
	if got := packageJSONInt32Array(raw("[3,1,2]")); len(got) != 3 || got[0] != 3 {
		t.Fatalf("got %v", got)
	}
	for _, value := range []*string{nil, raw(""), raw("null"), raw("garbage")} {
		if got := packageJSONInt32Array(value); len(got) != 0 {
			t.Fatalf("got %v, want an empty selection", got)
		}
	}
}

func TestPackageSoldOutMinimums(t *testing.T) {
	// Free package: 15% CPU / 128MB / 512MB.
	minCPU, minMemory, minDisk, soldOut := packageSoldOutMinimums(nil)
	if soldOut || minCPU != 15 || minMemory != 128 || minDisk != 512 {
		t.Fatalf("free defaults = %d/%d/%d soldOut=%v", minCPU, minMemory, minDisk, soldOut)
	}

	// Paid package: the smallest available plan per resource.
	plans := []packageSoldOutPlan{
		{PackageID: 1, CPU: 300, Memory: 4096, Disk: 30720},
		{PackageID: 1, CPU: 100, Memory: 8192, Disk: 10240},
		{PackageID: 1, CPU: 50, Memory: 2048, Disk: 20480, IsSoldOut: true},
	}
	minCPU, minMemory, minDisk, soldOut = packageSoldOutMinimums(plans)
	if soldOut || minCPU != 100 || minMemory != 4096 || minDisk != 10240 {
		t.Fatalf("paid minimums = %d/%d/%d soldOut=%v", minCPU, minMemory, minDisk, soldOut)
	}

	// Every active plan manually sold out.
	allSoldOut := []packageSoldOutPlan{{CPU: 1, Memory: 1, Disk: 1, IsSoldOut: true}}
	if _, _, _, soldOut := packageSoldOutMinimums(allSoldOut); !soldOut {
		t.Fatal("all plans sold out must mark the package sold out")
	}
}

func TestPackageFieldTriState(t *testing.T) {
	var data PackageUpdateData
	payload := `{"description":"hello","bandwidthMax":null,"portLimit":20}`
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !data.Description.Present || data.Description.Value == nil || *data.Description.Value != "hello" {
		t.Fatalf("description = %+v", data.Description)
	}
	if !data.BandwidthMax.Present || data.BandwidthMax.Value != nil {
		t.Fatalf("explicit null must be Present with a nil Value: %+v", data.BandwidthMax)
	}
	if data.MemoryMax.Present {
		t.Fatalf("an absent key must not be Present: %+v", data.MemoryMax)
	}
	if !data.PortLimit.Present || data.PortLimit.Value == nil || *data.PortLimit.Value != 20 {
		t.Fatalf("portLimit = %+v", data.PortLimit)
	}

	if _, err := json.Marshal(PackageNull[string]()); err != nil {
		t.Fatalf("marshal null: %v", err)
	}
	encoded, _ := json.Marshal(PackageValue("x"))
	if string(encoded) != `"x"` {
		t.Fatalf("marshal value = %s", encoded)
	}
}

func TestPackageListRowJSONShape(t *testing.T) {
	row := PackageListRow{
		ID:                     1,
		UserID:                 2,
		Name:                   "pkg",
		CPUMax:                 100,
		GPUDeviceIDs:           []int32{},
		MonthlyTrafficLimit:    packageBigIntString(nil),
		RequiredPackageName:    nil,
		GlobalQuotaMultiplier:  nil,
		GlobalMaxInstances:     nil,
		CreatedAt:              packageISOTime(packageTestTime()),
		HostIDs:                []int32{},
		HostStoragePools:       map[string]*string{},
		HostTrafficMultipliers: map[string]float64{},
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, present := decoded["global_max_instances"]; present {
		t.Fatal("global_max_instances must be omitted when null")
	}
	if value, present := decoded["global_quota_multiplier"]; !present || value != nil {
		t.Fatalf("global_quota_multiplier = %v present=%v, want null", value, present)
	}
	if value, present := decoded["required_package_id"]; !present || value != nil {
		t.Fatalf("required_package_id = %v present=%v, want null", value, present)
	}
	if value, present := decoded["owner_email"]; present {
		t.Fatalf("owner_email must be omitted when the owner was not included: %v", value)
	}
	if value, present := decoded["monthly_traffic_limit"]; !present || value != nil {
		t.Fatalf("monthly_traffic_limit = %v, want null", value)
	}
	if decoded["created_at"] != "2026-11-10T12:00:00.000Z" {
		t.Fatalf("created_at = %v", decoded["created_at"])
	}

	// An included owner with a null email must emit null, not omit the key.
	row.OwnerEmail = (*string)(nil)
	row.OwnerAvatarStyle = "adventurer"
	encoded, err = json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded = nil
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if value, present := decoded["owner_email"]; !present || value != nil {
		t.Fatalf("owner_email = %v present=%v, want null", value, present)
	}
	if decoded["owner_avatar_style"] != "adventurer" {
		t.Fatalf("owner_avatar_style = %v", decoded["owner_avatar_style"])
	}
	if _, present := decoded["owner_avatar_badge_id"]; present {
		t.Fatal("owner_avatar_badge_id must be omitted when the user has none")
	}
}

func TestPackageHostsMinimumBindingWithoutDatabase(t *testing.T) {
	// The list check runs before Prisma in the TypeScript, so it must not depend
	// on a connection.
	if err := SetPackageHosts(t.Context(), 1, nil); !errors.Is(err, ErrPackageHostRequired) {
		t.Fatalf("error = %v, want ErrPackageHostRequired", err)
	}
	if err := SetPackageHosts(t.Context(), 1, []int32{}); !errors.Is(err, ErrPackageHostRequired) {
		t.Fatalf("error = %v, want ErrPackageHostRequired", err)
	}
}

// packageTestTime is the fixed timestamp used by the JSON shape assertions.
func packageTestTime() (value time.Time) {
	return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
}

// ==================== database-backed proofs ====================

// packageTestDatabaseURL returns the DSN for the opt-in database tests. It
// refuses anything that is not a disposable local database, mirroring
// server/scripts/test-storage-capacity.ts.
func packageTestDatabaseURL(t *testing.T) string {
	t.Helper()
	if os.Getenv("PAYINCUS_GO_DB_TESTS") != "1" {
		t.Skip("set PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL to run the database-backed package tests")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	lower := strings.ToLower(dsn)
	if !strings.Contains(lower, "@127.0.0.1") && !strings.Contains(lower, "@localhost") {
		t.Fatalf("refusing to run against a non-local database: %s", dsn)
	}
	if !strings.Contains(lower, "_test") {
		t.Fatalf("refusing to run against a database whose name does not end in _test: %s", dsn)
	}
	return dsn
}

// TestPackagePrerequisiteLockMutualExclusion proves the lock really excludes a
// second transaction on the (4201, 1) key, and that the single-argument lock
// space (74201-style) is different.
func TestPackagePrerequisiteLockMutualExclusion(t *testing.T) {
	packageTestDatabaseURL(t)
	ctx := t.Context()
	if err := Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer Close()

	tx := DB.Begin()
	if tx.Error != nil {
		t.Fatalf("begin: %v", tx.Error)
	}
	defer func() { _ = tx.Rollback() }()
	if err := packagePrerequisiteLock(tx); err != nil {
		t.Fatalf("lock: %v", err)
	}

	var twoArgHeld bool
	if err := DB.Raw(`SELECT pg_try_advisory_xact_lock(?, 1)`, PackagePrerequisiteLockNamespace).Scan(&twoArgHeld).Error; err != nil {
		t.Fatalf("try two-arg lock: %v", err)
	}
	if twoArgHeld {
		t.Fatal("a second session acquired (4201, 1) while the first transaction holds it")
	}

	var oneArgHeld bool
	if err := DB.Raw(`SELECT pg_try_advisory_xact_lock(?::bigint)`, EgressAdvisoryLockNamespace).Scan(&oneArgHeld).Error; err != nil {
		t.Fatalf("try one-arg lock: %v", err)
	}
	if !oneArgHeld {
		t.Fatal("the single-argument lock space must be independent of the two-int space")
	}

	if err := tx.Rollback().Error; err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := DB.Raw(`SELECT pg_try_advisory_xact_lock(?, 1)`, PackagePrerequisiteLockNamespace).Scan(&twoArgHeld).Error; err != nil {
		t.Fatalf("try two-arg lock after rollback: %v", err)
	}
	if !twoArgHeld {
		t.Fatal("the lock must be released when the transaction ends")
	}
}

// TestPackageHostBindingUniqueness proves the composite key deduplicates the
// same (package_id, host_id) pair and that the last binding cannot be removed.
func TestPackageHostBindingUniqueness(t *testing.T) {
	packageTestDatabaseURL(t)
	ctx := t.Context()
	if err := Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer Close()

	var ownerID int32
	if err := DB.Raw(`SELECT id FROM users ORDER BY id ASC LIMIT 1`).Scan(&ownerID).Error; err != nil || ownerID == 0 {
		t.Skip("no user row to attach the fixture to")
	}
	hostOne := packageEnsureTestHost(t, ctx, ownerID, "pkg-test-host-a")
	hostTwo := packageEnsureTestHost(t, ctx, ownerID, "pkg-test-host-b")
	hostThree := packageEnsureTestHost(t, ctx, ownerID, "pkg-test-host-c")

	var packageID int32
	if err := DB.Raw(`INSERT INTO packages (user_id, name, cpu_max, memory_max, disk_max)
		VALUES (?, ?, 100, 1024, 10240) RETURNING id`,
		ownerID, "pkg-host-uniqueness-"+packageTestSuffix()).Scan(&packageID).Error; err != nil {
		t.Fatalf("create package: %v", err)
	}
	defer func() { DB.Exec(`DELETE FROM packages WHERE id = ?`, packageID) }()

	if err := SetPackageHosts(ctx, packageID, []int32{hostOne, hostOne, hostTwo}); err != nil {
		t.Fatalf("set hosts: %v", err)
	}
	var count int64
	if err := DB.Raw(`SELECT count(*) FROM package_hosts WHERE package_id = ?`, packageID).Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("bindings = %d, want 2 (the duplicate host must collapse)", count)
	}

	// addPackageHost upserts, so a repeat is a no-op.
	if err := AddPackageHost(ctx, packageID, hostOne); err != nil {
		t.Fatalf("add host: %v", err)
	}
	if err := DB.Raw(`SELECT count(*) FROM package_hosts WHERE package_id = ?`, packageID).Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("bindings = %d, want 2 after an idempotent add", count)
	}

	// A binding that does not exist is P2025, not a silent success.
	if err := RemovePackageHost(ctx, packageID, hostThree); !errors.Is(err, ErrPackageHostBindingNotFound) {
		t.Fatalf("removing a missing binding = %v, want ErrPackageHostBindingNotFound", err)
	}

	if err := RemovePackageHost(ctx, packageID, hostTwo); err != nil {
		t.Fatalf("remove host: %v", err)
	}
	if err := RemovePackageHost(ctx, packageID, hostOne); !errors.Is(err, ErrPackageHostRequired) {
		t.Fatalf("removing the last binding = %v, want ErrPackageHostRequired", err)
	}
}

func packageEnsureTestHost(t *testing.T, ctx context.Context, ownerID int32, name string) int32 {
	t.Helper()
	var hostID int32
	if err := DB.Raw(`SELECT id FROM hosts WHERE name = ? LIMIT 1`, name).Scan(&hostID).Error; err != nil {
		t.Fatalf("find host: %v", err)
	}
	if hostID != 0 {
		return hostID
	}
	if err := DB.Raw(`INSERT INTO hosts (user_id, name, url, status, instance_type, storage_driver, storage_type, target_host)
		VALUES (?, ?, ?, 'online', 'both', 'dir', 'local', '127.0.0.1') RETURNING id`,
		ownerID, name, "https://127.0.0.1:58443").Scan(&hostID).Error; err != nil {
		t.Fatalf("create host: %v", err)
	}
	return hostID
}

func packageTestSuffix() string {
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
}

// TestPackagePrerequisiteLockOrderingGuard pins the call order inside the two
// serializable transactions. The TypeScript takes the egress lock (74201), then
// the prerequisite lock (4201,1), then validates the chain, then writes; taking
// the lock after validation would let two concurrent updates each pass and create
// a cycle. This is a source-level guard, the same technique the Node guard suite
// uses for packages.ts.
func TestPackagePrerequisiteLockOrderingGuard(t *testing.T) {
	source, err := os.ReadFile("packages.go")
	if err != nil {
		t.Fatalf("read packages.go: %v", err)
	}
	text := string(source)

	section := func(start, end string) string {
		t.Helper()
		startIndex := strings.Index(text, start)
		if startIndex < 0 {
			t.Fatalf("missing section start: %s", start)
		}
		endIndex := strings.Index(text[startIndex:], end)
		if endIndex < 0 {
			t.Fatalf("missing section end %q after %q", end, start)
		}
		return text[startIndex : startIndex+endIndex]
	}

	create := section("func CreatePackage(", "// packageCreateUnchecked mirrors")
	for _, ordered := range []struct {
		first, second string
	}{
		{"packageEgressAdvisoryLock(tx)", "packagePrerequisiteLock(tx)"},
		{"packagePrerequisiteLock(tx)", "packageValidatePrerequisiteReferenceWithClient("},
		{"packageValidatePrerequisiteReferenceWithClient(", "packageCreateUnchecked("},
	} {
		firstIndex := strings.Index(create, ordered.first)
		secondIndex := strings.Index(create, ordered.second)
		if firstIndex < 0 || secondIndex < 0 {
			t.Fatalf("CreatePackage must call %q and %q", ordered.first, ordered.second)
		}
		if firstIndex > secondIndex {
			t.Fatalf("CreatePackage calls %q before %q; the lock must come first", ordered.second, ordered.first)
		}
	}
	if !strings.Contains(create, "sql.LevelSerializable") {
		t.Fatal("CreatePackage's prerequisite path must run at SERIALIZABLE isolation")
	}

	update := section("func UpdatePackage(", "// packageCurrentPackage is the projection")
	for _, ordered := range []struct {
		first, second string
	}{
		{"packageEgressAdvisoryLock(tx)", "packagePrerequisiteLock(tx)"},
		{"packagePrerequisiteLock(tx)", "packageValidatePrerequisiteReferenceWithClient("},
		{"packageValidatePrerequisiteReferenceWithClient(", "packageUpdateUnchecked("},
	} {
		firstIndex := strings.Index(update, ordered.first)
		secondIndex := strings.Index(update, ordered.second)
		if firstIndex < 0 || secondIndex < 0 {
			t.Fatalf("UpdatePackage must call %q and %q", ordered.first, ordered.second)
		}
		if firstIndex > secondIndex {
			t.Fatalf("UpdatePackage calls %q before %q; the lock must come first", ordered.second, ordered.first)
		}
	}
	if !strings.Contains(update, "sql.LevelSerializable") {
		t.Fatal("UpdatePackage's prerequisite path must run at SERIALIZABLE isolation")
	}
}

// TestPackageHostsInsertStatement pins the conflict target of the binding insert:
// the @@unique([packageId, hostId]) key, with DO NOTHING so a duplicate host in
// the caller's list collapses (Prisma's skipDuplicates).
func TestPackageHostsInsertStatement(t *testing.T) {
	statement, args := packageHostsInsertStatement(7, []int32{1, 1, 2})
	if !strings.Contains(statement, "ON CONFLICT (package_id, host_id) DO NOTHING") {
		t.Fatalf("statement = %q, want the composite-key conflict target", statement)
	}
	if !strings.HasPrefix(statement, "INSERT INTO package_hosts (package_id, host_id) VALUES (?, ?), (?, ?), (?, ?)") {
		t.Fatalf("statement = %q", statement)
	}
	wantArgs := []any{int32(7), int32(1), int32(7), int32(1), int32(7), int32(2)}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
	for index := range wantArgs {
		if args[index] != wantArgs[index] {
			t.Fatalf("args[%d] = %#v, want %#v", index, args[index], wantArgs[index])
		}
	}

	if !strings.Contains(packageHostAddStatement, "ON CONFLICT (package_id, host_id) DO NOTHING") {
		t.Fatalf("add statement = %q", packageHostAddStatement)
	}
}

// TestPackageInsertStatement pins the column/placeholder alignment: every column
// in packageInsertColumns must have exactly one placeholder, and gpu_device_ids
// must carry the int4[] cast.
func TestPackageInsertStatement(t *testing.T) {
	if len(packageInsertColumns) != 40 {
		t.Fatalf("packageInsertColumns = %d, want the 40 columns createPackage writes", len(packageInsertColumns))
	}
	valuesIndex := strings.Index(packageInsertStatement, ") VALUES (")
	if valuesIndex < 0 {
		t.Fatalf("statement = %q", packageInsertStatement)
	}
	values := packageInsertStatement[valuesIndex+len(") VALUES ("):]
	values = strings.TrimSuffix(values, ") RETURNING id")
	if got := strings.Count(values, "?"); got != len(packageInsertColumns) {
		t.Fatalf("placeholders = %d, want %d", got, len(packageInsertColumns))
	}
	if !strings.Contains(values, "?::int4[]") {
		t.Fatalf("gpu_device_ids must be cast to int4[]: %q", values)
	}
	for _, column := range packageInsertColumns {
		if !strings.Contains(packageInsertStatement, column) {
			t.Fatalf("column %q missing from the statement", column)
		}
	}
}

// TestPackageGpuAdmissionBranches covers the GPU half of createPackage's
// admission rules. The host-membership check needs a database; every other
// rejection is decided before it and is pinned here.
func TestPackageGpuAdmissionBranches(t *testing.T) {
	// A non-shared package is never gated on GPU shape.
	if err := GpusValidateSelection(t.Context(), nil, GpusSelectionInput{}); err != nil {
		t.Fatalf("non-shared selection must pass: %v", err)
	}

	cases := []struct {
		name  string
		input GpusSelectionInput
		want  string
	}{
		{
			name:  "shared GPU on a VM",
			input: GpusSelectionInput{GpuShared: true, InstanceType: "vm", GpuDeviceIDs: []int32{1}},
			want:  "共享 GPU 仅支持非特权 LXC 容器",
		},
		{
			name:  "shared GPU on a privileged container",
			input: GpusSelectionInput{GpuShared: true, InstanceType: "container", Privileged: true, GpuDeviceIDs: []int32{1}},
			want:  "共享 GPU 仅支持非特权 LXC 容器",
		},
		{
			name:  "shared GPU without a device",
			input: GpusSelectionInput{GpuShared: true, InstanceType: "container"},
			want:  "请选择至少一张共享 GPU",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GpusValidateSelection(t.Context(), nil, tc.input)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
