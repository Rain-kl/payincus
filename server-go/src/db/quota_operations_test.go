package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ==================== database gate ====================

// quotaTestDatabase initialises the shared pool against the opt-in test database.
// The guard is the same one src/db/packages_test.go uses: an explicit flag, a local
// host and a database name containing "_test" (never a parity/production DSN).
func quotaTestDatabase(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("PAYINCUS_GO_DB_TESTS") != "1" {
		t.Skip("set PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL to run the database-backed quota tests")
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
		t.Fatalf("refusing to run against a database whose name does not contain \"_test\": %s", dsn)
	}

	ctx := context.Background()
	quotaTestInit.Do(func() { quotaTestInitErr = Init(ctx) })
	if quotaTestInitErr != nil {
		t.Fatalf("db.Init: %v", quotaTestInitErr)
	}
	return ctx
}

var (
	quotaTestInit    sync.Once
	quotaTestInitErr error
)

// quotaTestUserID resolves the conformance user by name (never a hard-coded id).
func quotaTestUserID(t *testing.T) int32 {
	t.Helper()
	var id int32
	if err := DB.Raw("SELECT id FROM users WHERE username = 'parityuser'").Scan(&id).Error; err != nil {
		t.Fatalf("load parityuser: %v", err)
	}
	if id == 0 {
		t.Skip("parityuser is missing from the test database")
	}
	// The conformance seed only creates identity rows; the quota counters need their
	// user_quotas row. Insert defaults once so the counter tests can run (a fresh
	// install creates this row during registration).
	if err := DB.Exec(`INSERT INTO user_quotas (user_id, host_limit, friend_limit, package_limit)
		VALUES (?, 10, 10, 10) ON CONFLICT (user_id) DO NOTHING`, id).Error; err != nil {
		t.Fatalf("ensure user_quotas row: %v", err)
	}
	return id
}

// quotaTestFixture inserts a host and three instances (limit 3 / limit 0 / NULL
// limit) plus everything the instance-quota checks count, and removes them again.
// Ids are in the 94085x range so concurrent fixtures cannot collide.
type quotaTestFixture struct {
	HostID    int32
	Limited   int32
	NoQuota   int32
	NullLimit int32
}

func quotaTestInsertFixture(t *testing.T) quotaTestFixture {
	t.Helper()
	userID := quotaTestUserID(t)
	ctx := context.Background()

	fixture := quotaTestFixture{HostID: 940851, Limited: 940851, NoQuota: 940852, NullLimit: 940853}
	cleanup := func() {
		for _, statement := range []string{
			"DELETE FROM port_mappings WHERE instance_id BETWEEN 940851 AND 940859",
			"DELETE FROM snapshots WHERE instance_id BETWEEN 940851 AND 940859",
			"DELETE FROM backups WHERE instance_id BETWEEN 940851 AND 940859",
			"DELETE FROM instances WHERE id BETWEEN 940851 AND 940859",
			"DELETE FROM hosts WHERE id BETWEEN 940851 AND 940859",
		} {
			if err := DB.Exec(statement).Error; err != nil {
				t.Logf("quota fixture cleanup %q: %v", statement, err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
			cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
			created_at, updated_at)
			VALUES (?, ?, 'quota-test-host', 'https://127.0.0.1:1', 'online', 5, 0, 0,
			10000, 1048576, 1000, 'zfs', 'loop', ?, ?)`,
			[]any{fixture.HostID, userID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, 'quota-limited', 'quota-limited', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', 3, 2, 2, ?, ?)`,
			[]any{fixture.Limited, userID, fixture.HostID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, 'quota-none', 'quota-none', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', 0, 0, 0, ?, ?)`,
			[]any{fixture.NoQuota, userID, fixture.HostID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, 'quota-null', 'quota-null', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', NULL, NULL, NULL, ?, ?)`,
			[]any{fixture.NullLimit, userID, fixture.HostID, now, now}},
		{`INSERT INTO backups (id, instance_id, incus_name, name, status, created_at)
			VALUES (940851, ?, 'b-ready', 'b-ready', 'ready', ?),
			       (940852, ?, 'b-creating', 'b-creating', 'creating', ?),
			       (940853, ?, 'b-error', 'b-error', 'error', ?),
			       (940854, ?, 'b-deleted', 'b-deleted', 'deleted', ?)`,
			[]any{fixture.NullLimit, now, fixture.NullLimit, now, fixture.NullLimit, now,
				fixture.NullLimit, now}},
		{`INSERT INTO snapshots (id, instance_id, incus_name, name)
			VALUES (940851, ?, 's1', 's1'), (940852, ?, 's2', 's2')`,
			[]any{fixture.NullLimit, fixture.NullLimit}},
		{`INSERT INTO port_mappings (id, instance_id, host_id, protocol, public_port, private_port, system_managed)
			VALUES (940851, ?, ?, 'tcp', 20001, 80, false),
			       (940852, ?, ?, 'tcp', 20002, 443, false),
			       (940853, ?, ?, 'tcp', 20003, 22, true)`,
			[]any{fixture.NullLimit, fixture.HostID, fixture.NullLimit, fixture.HostID,
				fixture.NullLimit, fixture.HostID}},
	}
	for _, statement := range statements {
		if err := DB.WithContext(ctx).Exec(statement.sql, statement.args...).Error; err != nil {
			t.Fatalf("quota fixture insert failed: %v\n%s", err, statement.sql)
		}
	}
	return fixture
}

// ==================== pure tests ====================

// TestQuotaDenialMessages pins the exact User-facing texts. The route copies
// `message` into the QUOTA_BACKUP_EXCEEDED details, so a rewording is a wire change.
func TestQuotaDenialMessages(t *testing.T) {
	cases := []struct {
		function func(current, limit int32) string
		current  int32
		limit    int32
		want     string
	}{
		{BackupsQuotaDenialMessage, 0, 0, "Backup quota not allocated for this instance"},
		{BackupsQuotaDenialMessage, 2, 2, "Instance backup quota full (2/2)"},
		{BackupsQuotaDenialMessage, 5, 2, "Instance backup quota full (5/2)"},
	}
	for _, tc := range cases {
		if got := tc.function(tc.current, tc.limit); got != tc.want {
			t.Errorf("denial(%d,%d) = %q, want %q", tc.current, tc.limit, got, tc.want)
		}
	}
}

// TestQuotaCounterMessages pins the shared user-level message bodies.
func TestQuotaCounterMessages(t *testing.T) {
	if got := quotaCounterCheck(1, 1, "Host"); got.Allowed || got.Message != "Host quota full (1/1)" {
		t.Fatalf("host full = %+v", got)
	}
	if got := quotaCounterCheck(0, 0, "Friend"); got.Allowed ||
		got.Message != "Friend feature is not authorized. Please contact administrator." {
		t.Fatalf("friend unauthorized = %+v", got)
	}
	if got := quotaCounterCheck(0, 5, "Package"); !got.Allowed || got.Current != 0 || got.Limit != 5 {
		t.Fatalf("package allowed = %+v", got)
	}
}

// TestQuotaResourceAmountsMustBeNonNegative pins the guard both host-capacity
// functions run before touching the database.
func TestQuotaResourceAmountsMustBeNonNegative(t *testing.T) {
	ctx := context.Background()
	for _, options := range []ReserveResourcesOptions{
		{HostID: 1, CPU: -1},
		{HostID: 1, Memory: -1},
		{HostID: 1, Disk: -1},
		{HostID: 1, PortCount: -1},
	} {
		if err := ReserveResources(ctx, options); err == nil || err.Error() != "Resource amounts must be non-negative" {
			t.Fatalf("ReserveResources(%+v) = %v", options, err)
		}
	}
	for _, options := range []RollbackResourcesOptions{
		{HostID: 1, CPU: -1},
		{HostID: 1, Memory: -1},
		{HostID: 1, Disk: -1},
		{HostID: 1, PortCount: -1},
	} {
		if err := RollbackResources(ctx, options); err == nil || err.Error() != "Resource amounts must be non-negative" {
			t.Fatalf("RollbackResources(%+v) = %v", options, err)
		}
	}
}

// ==================== quota: backup reservations ====================

// TestQuotaBackupQuotaCountsOnlyActiveRows pins the counter: only creating/ready
// rows consume a slot, so error/deleted rows release it.
func TestQuotaBackupQuotaCountsOnlyActiveRows(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	// NULL backup_limit → limit 0 → denied, but `current` still reports the count.
	result, err := CheckBackupQuota(ctx, fixture.NullLimit)
	if err != nil {
		t.Fatalf("CheckBackupQuota: %v", err)
	}
	if result.Allowed || result.Limit != 0 || result.Current != 2 {
		t.Fatalf("null-limit result = %+v, want denied with current 2", result)
	}
	if result.Message != "Backup quota not allocated for this instance" {
		t.Fatalf("null-limit message = %q", result.Message)
	}

	// limit 3 with no active rows → allowed with the exact counters.
	result, err = CheckBackupQuota(ctx, fixture.Limited)
	if err != nil {
		t.Fatalf("CheckBackupQuota: %v", err)
	}
	if !result.Allowed || result.Limit != 3 || result.Current != 0 {
		t.Fatalf("limited result = %+v", result)
	}

	// Missing instance.
	result, err = CheckBackupQuota(ctx, 999999)
	if err != nil {
		t.Fatalf("CheckBackupQuota: %v", err)
	}
	if result.Allowed || result.Message != "Instance not found" {
		t.Fatalf("missing instance result = %+v", result)
	}
}

// TestQuotaBackupReservationDeniedBranches pins the two refusal texts returned by
// the atomic reservation (not just the read-only check).
func TestQuotaBackupReservationDeniedBranches(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	// limit 0.
	reservation, err := CreateBackupWithQuotaReservation(ctx, BackupsCreateData{
		InstanceID: fixture.NoQuota, IncusName: "atomic", Name: "atomic",
	})
	if err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if reservation.Allowed || reservation.Message != "Backup quota not allocated for this instance" {
		t.Fatalf("limit-0 reservation = %+v", reservation)
	}

	// limit 3, filled with three active rows.
	if err := DB.Exec(`INSERT INTO backups (id, instance_id, incus_name, name, status)
		VALUES (940855, ?, 'fill-1', 'fill-1', 'ready'),
		       (940856, ?, 'fill-2', 'fill-2', 'ready'),
		       (940857, ?, 'fill-3', 'fill-3', 'creating')`,
		fixture.Limited, fixture.Limited, fixture.Limited).Error; err != nil {
		t.Fatalf("fill quota: %v", err)
	}
	reservation, err = CreateBackupWithQuotaReservation(ctx, BackupsCreateData{
		InstanceID: fixture.Limited, IncusName: "atomic", Name: "atomic",
	})
	if err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if reservation.Allowed || reservation.Message != "Instance backup quota full (3/3)" {
		t.Fatalf("full reservation = %+v", reservation)
	}

	// A missing instance is reported as such, not as a quota denial.
	reservation, err = CreateBackupWithQuotaReservation(ctx, BackupsCreateData{
		InstanceID: 999999, IncusName: "atomic", Name: "atomic",
	})
	if err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if reservation.Allowed || reservation.Message != "Instance not found" {
		t.Fatalf("missing instance reservation = %+v", reservation)
	}
}

// TestQuotaBackupReservationConcurrency is the load-bearing test: N concurrent
// reservations against a limit of 3 must produce exactly 3 successes, because the
// count and the insert happen inside one advisory-locked transaction. A
// read-then-insert implementation would let several goroutines pass the pre-check.
func TestQuotaBackupReservationConcurrency(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	const workers = 12
	const limit = 3

	var wg sync.WaitGroup
	results := make([]BackupsQuotaReservationResult, workers)
	errs := make([]error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			reservation, err := CreateBackupWithQuotaReservation(ctx, BackupsCreateData{
				InstanceID: fixture.Limited,
				IncusName:  fmt.Sprintf("concurrent-%d", index),
				Name:       fmt.Sprintf("concurrent-%d", index),
			})
			results[index] = reservation
			errs[index] = err
		}(i)
	}
	close(start)
	wg.Wait()

	allowed := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i].Allowed {
			allowed++
			if results[i].BackupID == nil {
				t.Fatalf("worker %d allowed without a backup id", i)
			}
		} else if results[i].Message != fmt.Sprintf("Instance backup quota full (%d/%d)", limit, limit) {
			t.Fatalf("worker %d denial = %q", i, results[i].Message)
		}
	}
	if allowed != limit {
		t.Fatalf("allowed = %d, want exactly %d", allowed, limit)
	}

	var active int32
	if err := DB.Raw(`SELECT count(*)::int FROM backups
		WHERE instance_id = ? AND status IN ('creating','ready')`, fixture.Limited).Scan(&active).Error; err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != limit {
		t.Fatalf("active rows = %d, want %d", active, limit)
	}
}

// TestQuotaBackupReservationRepeatAfterFull pins the "repeat submission is denied,
// never double-inserted" invariant.
func TestQuotaBackupReservationRepeatAfterFull(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	for i := 0; i < 3; i++ {
		if err := DB.Exec(`INSERT INTO backups (id, instance_id, incus_name, name, status)
			VALUES (?, ?, ?, ?, 'ready')`, 940861+i, fixture.Limited,
			fmt.Sprintf("repeat-%d", i), fmt.Sprintf("repeat-%d", i)).Error; err != nil {
			t.Fatalf("seed full quota: %v", err)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		reservation, err := CreateBackupWithQuotaReservation(ctx, BackupsCreateData{
			InstanceID: fixture.Limited, IncusName: "repeat", Name: "repeat",
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if reservation.Allowed {
			t.Fatalf("attempt %d was allowed past the quota", attempt)
		}
	}
	var count int32
	if err := DB.Raw(`SELECT count(*)::int FROM backups WHERE instance_id = ?`, fixture.Limited).
		Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("row count = %d, want 3 (denied attempts must not insert)", count)
	}
}

// ==================== quota: instance-level counts ====================

// TestQuotaInstanceQuotaCounts pins the port/snapshot counters, including the
// system-managed port exclusion.
func TestQuotaInstanceQuotaCounts(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	// port_limit NULL → 0; two user mappings + one system mapping, only the user
	// ones count.
	result, err := CheckPortQuota(ctx, fixture.NullLimit)
	if err != nil {
		t.Fatalf("CheckPortQuota: %v", err)
	}
	if result.Allowed || result.Current != 2 || result.Message != "Port quota not allocated for this instance" {
		t.Fatalf("port result = %+v", result)
	}

	// snapshot_limit NULL → 0; two rows.
	result, err = CheckSnapshotQuota(ctx, fixture.NullLimit)
	if err != nil {
		t.Fatalf("CheckSnapshotQuota: %v", err)
	}
	if result.Allowed || result.Current != 2 || result.Message != "Snapshot quota not allocated for this instance" {
		t.Fatalf("snapshot result = %+v", result)
	}

	// Missing instance for both.
	if result, err = CheckPortQuota(ctx, 999999); err != nil || result.Message != "Instance not found" {
		t.Fatalf("missing port instance = %+v err=%v", result, err)
	}
	if result, err = CheckSnapshotQuota(ctx, 999999); err != nil || result.Message != "Instance not found" {
		t.Fatalf("missing snapshot instance = %+v err=%v", result, err)
	}
}

// TestQuotaUserLevelCounters pins host/friend/package checks against the stored
// counters, restoring the row afterwards.
func TestQuotaUserLevelCounters(t *testing.T) {
	ctx := quotaTestDatabase(t)
	userID := quotaTestUserID(t)

	var originalHostLimit, originalHostUsed int32
	var originalFriendLimit, originalFriendUsed int32
	var originalPackageLimit, originalPackageUsed int32
	if err := DB.Raw(`SELECT host_limit, host_used, friend_limit, friend_used,
		package_limit, package_used FROM user_quotas WHERE user_id = ?`, userID).
		Row().Scan(&originalHostLimit, &originalHostUsed, &originalFriendLimit,
		&originalFriendUsed, &originalPackageLimit, &originalPackageUsed); err != nil {
		t.Skipf("parityuser has no user_quotas row: %v", err)
	}
	t.Cleanup(func() {
		hostLimit, hostUsed := originalHostLimit, originalHostUsed
		friendLimit, friendUsed := originalFriendLimit, originalFriendUsed
		packageLimit, packageUsed := originalPackageLimit, originalPackageUsed
		_ = UpdateUserQuotaUsed(context.Background(), userID, UserQuotaUsedUpdate{
			HostUsed: &hostUsed, FriendUsed: &friendUsed, PackageUsed: &packageUsed,
		})
		_ = DB.Exec(`UPDATE user_quotas SET host_limit = ?, friend_limit = ?, package_limit = ?
			WHERE user_id = ?`, hostLimit, friendLimit, packageLimit, userID).Error
	})

	// limit 0 → "not authorized".
	if err := DB.Exec(`UPDATE user_quotas SET host_limit = 0, friend_limit = 0, package_limit = 0
		WHERE user_id = ?`, userID).Error; err != nil {
		t.Fatalf("set zero limits: %v", err)
	}
	if result, err := CheckHostQuota(ctx, userID); err != nil || result.Allowed ||
		result.Message != "Host feature is not authorized. Please contact administrator." {
		t.Fatalf("zero host limit = %+v err=%v", result, err)
	}
	if result, err := CheckFriendQuota(ctx, userID); err != nil || result.Allowed ||
		result.Message != "Friend feature is not authorized. Please contact administrator." {
		t.Fatalf("zero friend limit = %+v err=%v", result, err)
	}
	if result, err := CheckPackageQuota(ctx, userID); err != nil || result.Allowed ||
		result.Message != "Package feature is not authorized. Please contact administrator." {
		t.Fatalf("zero package limit = %+v err=%v", result, err)
	}

	// used >= limit → full.
	if err := DB.Exec(`UPDATE user_quotas SET host_limit = 1, host_used = 1,
		friend_limit = 2, friend_used = 3, package_limit = 1, package_used = 1
		WHERE user_id = ?`, userID).Error; err != nil {
		t.Fatalf("set full limits: %v", err)
	}
	if result, err := CheckHostQuota(ctx, userID); err != nil || result.Allowed ||
		result.Message != "Host quota full (1/1)" {
		t.Fatalf("full host = %+v err=%v", result, err)
	}
	if result, err := CheckFriendQuota(ctx, userID); err != nil || result.Allowed ||
		result.Message != "Friend quota full (3/2)" {
		t.Fatalf("full friend = %+v err=%v", result, err)
	}
	if result, err := CheckPackageQuota(ctx, userID); err != nil || result.Allowed ||
		result.Message != "Package quota full (1/1)" {
		t.Fatalf("full package = %+v err=%v", result, err)
	}

	// allowed branch.
	if err := DB.Exec(`UPDATE user_quotas SET host_limit = 5, host_used = 1
		WHERE user_id = ?`, userID).Error; err != nil {
		t.Fatalf("set allowed limits: %v", err)
	}
	if result, err := CheckHostQuota(ctx, userID); err != nil || !result.Allowed ||
		result.Current != 1 || result.Limit != 5 || result.Message != "" {
		t.Fatalf("allowed host = %+v err=%v", result, err)
	}

	// Missing quota row.
	if result, err := CheckHostQuota(ctx, 999999); err != nil || result.Allowed ||
		result.Message != "User quota not found" {
		t.Fatalf("missing quota row = %+v err=%v", result, err)
	}
}

// TestQuotaUpdateHelpers pins the absolute increment/decrement/overwrite helpers
// and the empty-update early returns. The row is restored afterwards.
func TestQuotaUpdateHelpers(t *testing.T) {
	ctx := quotaTestDatabase(t)
	userID := quotaTestUserID(t)

	var originalHostUsed, originalFriendUsed, originalPackageUsed int32
	if err := DB.Raw(`SELECT host_used, friend_used, package_used FROM user_quotas
		WHERE user_id = ?`, userID).Row().Scan(&originalHostUsed, &originalFriendUsed,
		&originalPackageUsed); err != nil {
		t.Skipf("parityuser has no user_quotas row: %v", err)
	}
	t.Cleanup(func() {
		host, friend, pkg := originalHostUsed, originalFriendUsed, originalPackageUsed
		_ = UpdateUserQuotaUsed(context.Background(), userID, UserQuotaUsedUpdate{
			HostUsed: &host, FriendUsed: &friend, PackageUsed: &pkg,
		})
	})

	// Absolute overwrite.
	zero := int32(0)
	if err := UpdateUserQuotaUsed(ctx, userID, UserQuotaUsedUpdate{HostUsed: &zero}); err != nil {
		t.Fatalf("UpdateUserQuotaUsed: %v", err)
	}
	// Empty update is a no-op (not an error).
	if err := UpdateUserQuotaUsed(ctx, userID, UserQuotaUsedUpdate{}); err != nil {
		t.Fatalf("empty UpdateUserQuotaUsed: %v", err)
	}

	// Increment then decrement.
	value := int32(4)
	if err := IncrementUserQuotaUsed(ctx, userID, UserQuotaUsedUpdate{HostUsed: &value}); err != nil {
		t.Fatalf("IncrementUserQuotaUsed: %v", err)
	}
	if err := DecrementUserQuotaUsed(ctx, userID, "host"); err != nil {
		t.Fatalf("DecrementUserQuotaUsed: %v", err)
	}
	// Unknown type is a no-op in the TypeScript switch (no default).
	if err := DecrementUserQuotaUsed(ctx, userID, "instance"); err != nil {
		t.Fatalf("unknown decrement: %v", err)
	}
	// The helpers do not clamp at zero (matching the TypeScript body, not its
	// header comment).
	for i := 0; i < 10; i++ {
		if err := DecrementUserQuotaUsed(ctx, userID, "host"); err != nil {
			t.Fatalf("decrement below zero: %v", err)
		}
	}
	var hostUsed int32
	if err := DB.Raw(`SELECT host_used FROM user_quotas WHERE user_id = ?`, userID).
		Scan(&hostUsed).Error; err != nil {
		t.Fatalf("read host_used: %v", err)
	}
	if hostUsed != -7 {
		t.Fatalf("host_used = %d, want -7 (4 - 1 - 10)", hostUsed)
	}

	// Update on a missing row reports Prisma's P2025 text.
	if err := UpdateUserQuotaUsed(ctx, 999999, UserQuotaUsedUpdate{HostUsed: &zero}); err == nil ||
		!strings.Contains(err.Error(), "record to update not found") {
		t.Fatalf("missing row update = %v", err)
	}
}

// TestQuotaSyncUserQuotaUsed pins sync = calculate + overwrite.
func TestQuotaSyncUserQuotaUsed(t *testing.T) {
	ctx := quotaTestDatabase(t)
	userID := quotaTestUserID(t)

	calculated, err := CalculateUserQuotaUsed(ctx, userID)
	if err != nil {
		t.Fatalf("CalculateUserQuotaUsed: %v", err)
	}
	if calculated.HostUsed == nil || calculated.FriendUsed == nil || calculated.PackageUsed == nil {
		t.Fatalf("calculate returned nil counters: %+v", calculated)
	}
	if err := SyncUserQuotaUsed(ctx, userID); err != nil {
		t.Fatalf("SyncUserQuotaUsed: %v", err)
	}

	var hostUsed, friendUsed, packageUsed int32
	if err := DB.Raw(`SELECT host_used, friend_used, package_used FROM user_quotas
		WHERE user_id = ?`, userID).Row().Scan(&hostUsed, &friendUsed, &packageUsed); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if hostUsed != *calculated.HostUsed || friendUsed != *calculated.FriendUsed ||
		packageUsed != *calculated.PackageUsed {
		t.Fatalf("synced counters = (%d,%d,%d), want (%d,%d,%d)", hostUsed, friendUsed,
			packageUsed, *calculated.HostUsed, *calculated.FriendUsed, *calculated.PackageUsed)
	}
}

// ==================== quota: host capacity ====================

// TestQuotaResourceReserveFailsClosedWithoutStorageProvider proves reserveResources
// refuses to oversell when the storage-capacity provider is not wired (the db test
// binary cannot link lib, so the hook stays nil exactly like a fail-closed host).
func TestQuotaResourceReserveFailsClosedWithoutStorageProvider(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	err := ReserveResources(ctx, ReserveResourcesOptions{
		HostID: fixture.HostID, CPU: 1, Memory: 1024, Disk: 1024,
	})
	if err == nil || !strings.Contains(err.Error(), "HOST_RESOURCES_INSUFFICIENT") {
		t.Fatalf("reserve = %v, want HOST_RESOURCES_INSUFFICIENT", err)
	}

	var cpuUsed, memoryUsed, diskUsed, portsUsed int32
	if err := DB.Raw(`SELECT cpu_used, memory_used, disk_used, nat_ports_used_count
		FROM hosts WHERE id = ?`, fixture.HostID).
		Row().Scan(&cpuUsed, &memoryUsed, &diskUsed, &portsUsed); err != nil {
		t.Fatalf("read host: %v", err)
	}
	if cpuUsed != 5 || memoryUsed != 0 || diskUsed != 0 || portsUsed != 0 {
		t.Fatalf("failed reservation mutated the host: cpu=%d mem=%d disk=%d ports=%d",
			cpuUsed, memoryUsed, diskUsed, portsUsed)
	}
}

// TestQuotaRollbackResourcesNeverNegative pins the GREATEST(...,0) clamp for both
// the port and the plain branch.
func TestQuotaRollbackResourcesNeverNegative(t *testing.T) {
	ctx := quotaTestDatabase(t)
	fixture := quotaTestInsertFixture(t)

	if err := RollbackResources(ctx, RollbackResourcesOptions{
		HostID: fixture.HostID, CPU: 100, Memory: 100, Disk: 100, PortCount: 5,
	}); err != nil {
		t.Fatalf("rollback with ports: %v", err)
	}
	var cpuUsed, memoryUsed, diskUsed, portsUsed int32
	if err := DB.Raw(`SELECT cpu_used, memory_used, disk_used, nat_ports_used_count
		FROM hosts WHERE id = ?`, fixture.HostID).
		Row().Scan(&cpuUsed, &memoryUsed, &diskUsed, &portsUsed); err != nil {
		t.Fatalf("read host: %v", err)
	}
	if cpuUsed != 0 || memoryUsed != 0 || diskUsed != 0 || portsUsed != 0 {
		t.Fatalf("rollback = cpu=%d mem=%d disk=%d ports=%d, want all zero",
			cpuUsed, memoryUsed, diskUsed, portsUsed)
	}

	// Plain branch (portCount == 0).
	if err := DB.Exec(`UPDATE hosts SET cpu_used = 2, memory_used = 3, disk_used = 4,
		nat_ports_used_count = 6 WHERE id = ?`, fixture.HostID).Error; err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if err := RollbackResources(ctx, RollbackResourcesOptions{
		HostID: fixture.HostID, CPU: 1, Memory: 1, Disk: 1,
	}); err != nil {
		t.Fatalf("rollback without ports: %v", err)
	}
	if err := DB.Raw(`SELECT cpu_used, memory_used, disk_used, nat_ports_used_count
		FROM hosts WHERE id = ?`, fixture.HostID).
		Row().Scan(&cpuUsed, &memoryUsed, &diskUsed, &portsUsed); err != nil {
		t.Fatalf("read host: %v", err)
	}
	if cpuUsed != 1 || memoryUsed != 2 || diskUsed != 3 || portsUsed != 6 {
		t.Fatalf("plain rollback = cpu=%d mem=%d disk=%d ports=%d", cpuUsed, memoryUsed, diskUsed, portsUsed)
	}
}
