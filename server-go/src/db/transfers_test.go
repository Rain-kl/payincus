// Tests for db/transfers.go (server/src/db/transfers.ts).
//
// Pure tests pin the helpers whose behaviour is invisible in SQL (pagination,
// status normalisation, the UTF-16 search slice, the snapshot wire shape, the retry
// schedule). The database-backed tests are skipped unless
// PAYINCUS_TEST_DATABASE_URL points at a throwaway database with the Prisma schema
// (never production/development); they prove what only PostgreSQL can:
//
//   - the create transaction takes the NON-BLOCKING 4103 lock, retries on the
//     increasing 100ms*(attempt+1) schedule and charges the fee atomically;
//   - a busy balance lock (4110) surfaces as BALANCE_CONFLICT, not as a hang;
//   - the accept claim is the idempotency gate (pending -> processing exactly once);
//   - executeTransfer moves ownership and writes the transfer_fee billing row;
//   - refunds restore the balance with a transfer_refund snapshot log;
//   - the list/pending-count queries filter and paginate exactly.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== pure tests ====================

func TestTransferCreateLockRetryDefaults(t *testing.T) {
	if transferCreateLockRetryLimit != 20 {
		t.Fatalf("retry limit = %d, want 20", transferCreateLockRetryLimit)
	}
	if transferCreateLockRetryDelay != 100*time.Millisecond {
		t.Fatalf("retry delay = %v, want 100ms", transferCreateLockRetryDelay)
	}
	if TransferCreateLockNamespace != 4103 {
		t.Fatalf("TransferCreateLockNamespace = %d, want 4103", TransferCreateLockNamespace)
	}
	if transferTransactionTimeout != 10*time.Second {
		t.Fatalf("transaction timeout = %v, want 10s", transferTransactionTimeout)
	}
}

func TestTransferClampPagination(t *testing.T) {
	cases := []struct {
		page, pageSize     int
		wantPage, wantSize int
	}{
		{0, 0, 1, 20},
		{-1, -5, 1, 20},
		{5, 0, 5, 20},
		{2, 100, 2, 100},
		{3, 1000, 3, 100},
		{1, 1, 1, 1},
	}
	for _, testCase := range cases {
		page, size := transferClampPagination(testCase.page, testCase.pageSize)
		if page != testCase.wantPage || size != testCase.wantSize {
			t.Errorf("clamp(%d,%d) = (%d,%d), want (%d,%d)",
				testCase.page, testCase.pageSize, page, size, testCase.wantPage, testCase.wantSize)
		}
	}
}

func TestTransferNormalizeStatus(t *testing.T) {
	for _, status := range []string{"pending", "processing", "accepted", "rejected", "cancelled"} {
		if got := transferNormalizeStatus(status); string(got) != status {
			t.Errorf("normalizeStatus(%q) = %q", status, got)
		}
	}
	for _, status := range []string{"", "PENDING", "bogus", "deleted"} {
		if got := transferNormalizeStatus(status); got != "" {
			t.Errorf("normalizeStatus(%q) = %q, want empty", status, got)
		}
	}
}

func TestTransferNormalizeSearch(t *testing.T) {
	if got := transferNormalizeSearch("  "); got != "" {
		t.Errorf("blank search = %q, want empty", got)
	}
	if got := transferNormalizeSearch("  box  "); got != "box" {
		t.Errorf("search = %q, want box", got)
	}
	// ECMAScript whitespace: U+FEFF is trimmed, U+0085 is not.
	if got := transferNormalizeSearch("\uFEFFbox\uFEFF"); got != "box" {
		t.Errorf("BOM search = %q, want box", got)
	}
	if got := transferNormalizeSearch("\u0085box"); got != "\u0085box" {
		t.Errorf("NEL search = %q, want it preserved", got)
	}

	long := make([]byte, 200)
	for index := range long {
		long[index] = 'a'
	}
	if got := transferNormalizeSearch(string(long)); len(got) != transferListSearchMaxLen {
		t.Errorf("search length = %d, want %d", len(got), transferListSearchMaxLen)
	}

	// The slice counts UTF-16 code units: each emoji is two units, so a limit of
	// three keeps the first emoji and drops the split surrogate pair.
	if got := transferSliceUTF16("😀😀😀", 3); got != "😀" {
		t.Errorf("emoji slice = %q, want one emoji", got)
	}
	if got := transferSliceUTF16("😀😀😀", 6); got != "😀😀😀" {
		t.Errorf("full emoji slice = %q", got)
	}
	// A long enough search goes through the same UTF-16 path.
	longEmoji := ""
	for index := 0; index < 100; index++ {
		longEmoji += "😀"
	}
	if got := transferNormalizeSearch(longEmoji); len(utf16.Encode([]rune(got))) != transferListSearchMaxLen {
		t.Errorf("emoji search units = %d, want %d", len(utf16.Encode([]rune(got))), transferListSearchMaxLen)
	}
}

func TestTransferSnapshotJSONShape(t *testing.T) {
	packageID := int32(5)
	packageName := "plan"
	location := "us-1"
	ipv4 := "203.0.113.7"

	snapshot := TransferSnapshot{
		PackageID:         &packageID,
		PackageName:       &packageName,
		HostID:            9,
		HostName:          "host",
		HostLocation:      &location,
		HostCountryCode:   "us",
		OriginalName:      "box",
		CPU:               2,
		Memory:            2048,
		Disk:              20000,
		NetworkMode:       "nat",
		PortMappingsCount: 3,
		SnapshotsCount:    1,
		BackupsCount:      2,
		IPv4:              &ipv4,
		IPv6:              nil,
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"packageId":5,"packageName":"plan","hostId":9,"hostName":"host","hostLocation":"us-1",` +
		`"hostCountryCode":"us","originalName":"box","cpu":2,"memory":2048,"disk":20000,` +
		`"networkMode":"nat","portMappingsCount":3,"snapshotsCount":1,"backupsCount":2,` +
		`"ipv4":"203.0.113.7","ipv6":null}`
	if string(encoded) != want {
		t.Fatalf("snapshot JSON = %s\nwant %s", encoded, want)
	}
}

// ==================== database fixtures ====================

const (
	transferTestFromUser  int32 = 994_001
	transferTestToUser    int32 = 994_002
	transferTestOtherUser int32 = 994_003
	transferTestHost      int32 = 994_001
	transferTestInstance  int32 = 994_001
	transferTestInstance2 int32 = 994_002
	transferTestInstance3 int32 = 994_003

	// transferTestRangeLow/High bound every fixture row so cleanup never touches the
	// conformance seeds (993001+) or another module's ids.
	transferTestRangeLow  int32 = 994_000
	transferTestRangeHigh int32 = 994_999
)

var (
	transferTestInitOnce sync.Once
	transferTestInitErr  error
)

// transferTestDatabase initialises the pool against PAYINCUS_TEST_DATABASE_URL and
// installs a deterministic fixture set. It skips (never fails) when the DSN is not
// provided.
func transferTestDatabase(t *testing.T) context.Context {
	t.Helper()

	dsn := os.Getenv("PAYINCUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PAYINCUS_TEST_DATABASE_URL to a throwaway database with the Prisma schema")
	}
	transferTestInitOnce.Do(func() {
		_ = os.Setenv("DATABASE_URL", dsn)
		transferTestInitErr = Init(context.Background())
	})
	if transferTestInitErr != nil {
		t.Fatalf("db.Init: %v", transferTestInitErr)
	}

	ctx := context.Background()
	transferTestCleanup(t, ctx)
	t.Cleanup(func() { transferTestCleanup(t, ctx) })
	transferTestSeed(t, ctx)
	return ctx
}

// transferTestPlayer deletes every fixture row. Order respects the foreign keys.
func transferTestCleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	statements := []string{
		`DELETE FROM instance_billing_records WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM balance_logs WHERE user_id BETWEEN $1 AND $2`,
		`DELETE FROM instance_transfers WHERE instance_id BETWEEN $1 AND $2 OR from_user_id BETWEEN $1 AND $2 OR to_user_id BETWEEN $1 AND $2`,
		`DELETE FROM snapshot_policies WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM backup_policies WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM snapshots WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM backups WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM port_mappings WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM proxy_sites WHERE instance_id BETWEEN $1 AND $2`,
		`DELETE FROM instances WHERE id BETWEEN $1 AND $2`,
		`DELETE FROM hosts WHERE id BETWEEN $1 AND $2`,
		`DELETE FROM users WHERE id BETWEEN $1 AND $2`,
	}
	for _, statement := range statements {
		if _, err := Pool().Exec(ctx, statement, transferTestRangeLow, transferTestRangeHigh); err != nil {
			t.Fatalf("cleanup %q: %v", statement, err)
		}
	}
}

// transferTestSeed inserts the three users, one host and three instances.
func transferTestSeed(t *testing.T, ctx context.Context) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)

	for _, fixture := range []struct {
		id       int32
		username string
		balance  string
	}{
		{transferTestFromUser, "transfer-db-from", "20.00"},
		{transferTestToUser, "transfer-db-to", "0.00"},
		{transferTestOtherUser, "transfer-db-other", "0.00"},
	} {
		if _, err := Pool().Exec(ctx,
			`INSERT INTO users (id, username, password_hash, balance, updated_at)
			 VALUES ($1, $2, 'x', $3::numeric, $4)`,
			fixture.id, fixture.username, fixture.balance, now); err != nil {
			t.Fatalf("insert user %d: %v", fixture.id, err)
		}
	}

	if _, err := Pool().Exec(ctx,
		`INSERT INTO hosts (id, user_id, name, url, updated_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		transferTestHost, transferTestFromUser, "transfer-db-host",
		"https://127.0.0.1:8443", now); err != nil {
		t.Fatalf("insert host: %v", err)
	}

	for index, fixture := range []struct {
		id     int32
		name   string
		status models.InstanceStatus
	}{
		{transferTestInstance, "transfer-db-box", models.InstanceStatusStopped},
		{transferTestInstance2, "transfer-db-box-two", models.InstanceStatusRunning},
		{transferTestInstance3, "transfer-db-box-three", models.InstanceStatusStopped},
	} {
		if _, err := Pool().Exec(ctx,
			`INSERT INTO instances
			   (id, incus_id, name, user_id, host_id, image, status, cpu, memory, disk, updated_at)
			 VALUES ($1, $2, $3, $4, $5, 'debian/12', $6, 1, 512, 5120, $7)`,
			fixture.id, "transfer-db-incus-"+string(rune('a'+index)), fixture.name,
			transferTestFromUser, transferTestHost, string(fixture.status), now); err != nil {
			t.Fatalf("insert instance %d: %v", fixture.id, err)
		}
	}
}

// transferTestResetTransfer removes transfer-related state between sub-scenarios.
func transferTestResetTransfer(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := Pool().Exec(ctx,
		`DELETE FROM instance_transfers WHERE instance_id BETWEEN $1 AND $2`,
		transferTestRangeLow, transferTestRangeHigh); err != nil {
		t.Fatalf("delete transfers: %v", err)
	}
	if _, err := Pool().Exec(ctx,
		`DELETE FROM instance_billing_records WHERE instance_id BETWEEN $1 AND $2`,
		transferTestRangeLow, transferTestRangeHigh); err != nil {
		t.Fatalf("delete billing: %v", err)
	}
	if _, err := Pool().Exec(ctx,
		`DELETE FROM balance_logs WHERE user_id BETWEEN $1 AND $2`,
		transferTestRangeLow, transferTestRangeHigh); err != nil {
		t.Fatalf("delete balance logs: %v", err)
	}
	if _, err := Pool().Exec(ctx,
		`UPDATE users SET balance = CASE WHEN id = $3 THEN 20.00 ELSE 0 END WHERE id BETWEEN $1 AND $2`,
		transferTestRangeLow, transferTestRangeHigh, transferTestFromUser); err != nil {
		t.Fatalf("reset balances: %v", err)
	}
	if _, err := Pool().Exec(ctx,
		`UPDATE instances SET user_id = $2, name = 'transfer-db-box', incus_id = 'transfer-db-incus-a',
		        auto_renew = true, display_order = 1
		  WHERE id = $1`, transferTestInstance, transferTestFromUser); err != nil {
		t.Fatalf("reset instance: %v", err)
	}
}

// transferTestInsertTransfer inserts one transfer row and returns its id.
func transferTestInsertTransfer(
	t *testing.T,
	ctx context.Context,
	id, instanceID, fromUserID, toUserID int32,
	status models.TransferStatus,
	fee *string,
	createdAt time.Time,
	remark *string,
) {
	t.Helper()
	updatedAt := createdAt
	var acceptedAt, rejectedAt, cancelledAt *time.Time
	switch status {
	case models.TransferStatusAccepted:
		acceptedAt = &updatedAt
	case models.TransferStatusRejected:
		rejectedAt = &updatedAt
	case models.TransferStatusCancelled:
		cancelledAt = &updatedAt
	}
	if _, err := Pool().Exec(ctx,
		`INSERT INTO instance_transfers
		   (id, instance_id, from_user_id, to_user_id, status, snapshot, remark, fee,
		    created_at, updated_at, accepted_at, rejected_at, cancelled_at)
		 VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $7::numeric, $8, $8, $9, $10, $11)`,
		id, instanceID, fromUserID, toUserID, string(status), remark, fee,
		createdAt, acceptedAt, rejectedAt, cancelledAt); err != nil {
		t.Fatalf("insert transfer %d: %v", id, err)
	}
}

func transferTestBalance(t *testing.T, ctx context.Context, userID int32) float64 {
	t.Helper()
	var balance models.Decimal
	if err := Pool().QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("balance: %v", err)
	}
	return balance.InexactFloat64()
}

// ==================== database tests ====================

// TestTransferDatabaseCreateWithFeeAndExecute is the end-to-end money + ownership
// proof: charge atomically, claim exactly once, move ownership, write the billing
// row.
func TestTransferDatabaseCreateWithFeeAndExecute(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	result, err := CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID:   transferTestInstance,
		FromUserID:   transferTestFromUser,
		ToUserID:     transferTestToUser,
		Snapshot:     TransferSnapshot{HostID: transferTestHost, HostName: "host", HostCountryCode: "us", OriginalName: "transfer-db-box"},
		Remark:       transferTestStringPtr("please take it"),
		Fee:          5.50,
		InstanceName: "transfer-db-box",
	})
	if err != nil {
		t.Fatalf("CreateTransferWithFee: %v", err)
	}
	if !result.FeeDeducted || result.TransferID == 0 {
		t.Fatalf("result = %#v", result)
	}
	if balance := transferTestBalance(t, ctx, transferTestFromUser); balance != 14.50 {
		t.Fatalf("balance = %v, want 14.50", balance)
	}

	var log struct {
		Type          models.BalanceLogType
		Amount        models.Decimal
		BalanceBefore models.Decimal
		BalanceAfter  models.Decimal
		Remark        *string
	}
	if err := Pool().QueryRow(ctx,
		`SELECT type, amount, balance_before, balance_after, remark FROM balance_logs
		  WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, transferTestFromUser).
		Scan(&log.Type, &log.Amount, &log.BalanceBefore, &log.BalanceAfter, &log.Remark); err != nil {
		t.Fatalf("balance log: %v", err)
	}
	if log.Type != models.BalanceLogTypeTransferFee {
		t.Fatalf("log type = %s", log.Type)
	}
	if log.Amount.String() != "-5.5" || log.BalanceBefore.String() != "20" || log.BalanceAfter.String() != "14.5" {
		t.Fatalf("log amounts = %s %s %s", log.Amount, log.BalanceBefore, log.BalanceAfter)
	}
	if log.Remark == nil || *log.Remark != `转移实例 "transfer-db-box" 手续费` {
		t.Fatalf("log remark = %v", log.Remark)
	}

	var fee *string
	var status models.TransferStatus
	var updatedAt time.Time
	if err := Pool().QueryRow(ctx,
		`SELECT fee::text, status, updated_at FROM instance_transfers WHERE id = $1`, result.TransferID).
		Scan(&fee, &status, &updatedAt); err != nil {
		t.Fatalf("transfer row: %v", err)
	}
	if fee == nil || *fee != "5.50" || status != models.TransferStatusPending {
		t.Fatalf("transfer = fee %v status %s", fee, status)
	}
	if delta := time.Since(updatedAt); delta < -5*time.Second || delta > 5*time.Second {
		t.Fatalf("updated_at = %v is not UTC now (delta %v)", updatedAt, delta)
	}

	// A second create must be rejected before any second charge.
	if _, err := CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID: transferTestInstance, FromUserID: transferTestFromUser, ToUserID: transferTestToUser,
		Snapshot: TransferSnapshot{}, Fee: 5.50, InstanceName: "transfer-db-box",
	}); !errors.Is(err, ErrTransferAlreadyPending) {
		t.Fatalf("second create err = %v, want ErrTransferAlreadyPending", err)
	}
	if balance := transferTestBalance(t, ctx, transferTestFromUser); balance != 14.50 {
		t.Fatalf("balance after duplicate = %v, want 14.50", balance)
	}

	// Idempotency gate: the claim wins exactly once.
	claimed, err := ClaimTransferForAccept(ctx, result.TransferID, transferTestToUser)
	if err != nil {
		t.Fatalf("ClaimTransferForAccept: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claim = %d, want 1", claimed)
	}
	claimedAgain, err := ClaimTransferForAccept(ctx, result.TransferID, transferTestToUser)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimedAgain != 0 {
		t.Fatalf("second claim = %d, want 0", claimedAgain)
	}

	if err := ExecuteTransfer(ctx, result.TransferID, transferTestInstance, transferTestFromUser,
		transferTestToUser, "transfer-db-box（from转）", "u-994002-abcd1234"); err != nil {
		t.Fatalf("ExecuteTransfer: %v", err)
	}

	var owner int32
	var name, incusID string
	var autoRenew bool
	var displayOrder int32
	if err := Pool().QueryRow(ctx,
		`SELECT user_id, name, incus_id, auto_renew, display_order FROM instances WHERE id = $1`,
		transferTestInstance).Scan(&owner, &name, &incusID, &autoRenew, &displayOrder); err != nil {
		t.Fatalf("instance after transfer: %v", err)
	}
	if owner != transferTestToUser || name != "transfer-db-box（from转）" ||
		incusID != "u-994002-abcd1234" || autoRenew || displayOrder != 0 {
		t.Fatalf("instance = owner %d name %q incus %q autoRenew %v order %d",
			owner, name, incusID, autoRenew, displayOrder)
	}

	if err := Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = $1`, result.TransferID).Scan(&status); err != nil {
		t.Fatalf("transfer status: %v", err)
	}
	if status != models.TransferStatusAccepted {
		t.Fatalf("transfer status = %s, want accepted", status)
	}

	var billingCount int64
	var billingAmount models.Decimal
	var billingMonths int32
	var billingType string
	if err := Pool().QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MAX(amount), 0), COALESCE(MAX(months), 0), COALESCE(MAX(type::text), '')
		   FROM instance_billing_records WHERE instance_id = $1`, transferTestInstance).
		Scan(&billingCount, &billingAmount, &billingMonths, &billingType); err != nil {
		t.Fatalf("billing record: %v", err)
	}
	if billingCount != 1 || billingAmount.String() != "5.5" || billingMonths != 0 ||
		billingType != string(models.BillingRecordTypeTransferFee) {
		t.Fatalf("billing = count %d amount %s months %d type %s",
			billingCount, billingAmount, billingMonths, billingType)
	}
}

// TestTransferDatabaseInsufficientBalanceAndConflicts pins the three failure modes
// of the create transaction.
func TestTransferDatabaseInsufficientBalanceAndConflicts(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	if _, err := Pool().Exec(ctx, `UPDATE users SET balance = 1.00 WHERE id = $1`, transferTestFromUser); err != nil {
		t.Fatalf("set balance: %v", err)
	}
	_, err := CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID: transferTestInstance, FromUserID: transferTestFromUser, ToUserID: transferTestToUser,
		Snapshot: TransferSnapshot{}, Fee: 5, InstanceName: "transfer-db-box",
	})
	if !errors.Is(err, ErrTransferInsufficientBalance) {
		t.Fatalf("err = %v, want ErrTransferInsufficientBalance", err)
	}
	if balance := transferTestBalance(t, ctx, transferTestFromUser); balance != 1 {
		t.Fatalf("balance = %v, want 1", balance)
	}
	var count int64
	if err := Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM instance_transfers WHERE instance_id = $1`, transferTestInstance).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("transfer rows = %d, want 0", count)
	}

	// A held 4110 lock must answer BALANCE_CONFLICT immediately, never block.
	if _, err := Pool().Exec(ctx, `UPDATE users SET balance = 20.00 WHERE id = $1`, transferTestFromUser); err != nil {
		t.Fatalf("restore balance: %v", err)
	}
	holder, err := Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	locked, err := TryAdvisoryTransactionLock(ctx, holder, UserBalanceLockNamespace, transferTestFromUser)
	if err != nil || !locked {
		t.Fatalf("hold balance lock: locked=%v err=%v", locked, err)
	}

	start := time.Now()
	_, err = CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID: transferTestInstance, FromUserID: transferTestFromUser, ToUserID: transferTestToUser,
		Snapshot: TransferSnapshot{}, Fee: 5, InstanceName: "transfer-db-box",
	})
	if !errors.Is(err, ErrTransferBalanceConflict) {
		t.Fatalf("err = %v, want ErrTransferBalanceConflict", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("balance conflict took %v; the lock must be non-blocking", elapsed)
	}
	_ = holder.Rollback(ctx)

	// Fee 0: no charge, no balance log, fee column NULL.
	result, err := CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID: transferTestInstance, FromUserID: transferTestFromUser, ToUserID: transferTestToUser,
		Snapshot: TransferSnapshot{}, Fee: 0, InstanceName: "transfer-db-box",
	})
	if err != nil {
		t.Fatalf("free create: %v", err)
	}
	if result.FeeDeducted {
		t.Fatal("fee 0 must not report a deduction")
	}
	var fee *string
	if err := Pool().QueryRow(ctx,
		`SELECT fee::text FROM instance_transfers WHERE id = $1`, result.TransferID).Scan(&fee); err != nil {
		t.Fatalf("fee: %v", err)
	}
	if fee != nil {
		t.Fatalf("fee = %v, want NULL", *fee)
	}
	if balance := transferTestBalance(t, ctx, transferTestFromUser); balance != 20 {
		t.Fatalf("balance = %v, want 20", balance)
	}
	if err := Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM balance_logs WHERE user_id = $1`, transferTestFromUser).Scan(&count); err != nil {
		t.Fatalf("balance log count: %v", err)
	}
	if count != 0 {
		t.Fatalf("balance logs = %d, want 0", count)
	}
}

// TestTransferDatabaseCreateLockRetry pins the non-blocking retry loop: while
// another transaction holds (4103, instanceId), every attempt returns "busy", the
// loop sleeps 100ms*(attempt+1) and finally answers TRANSFER_ALREADY_PENDING.
func TestTransferDatabaseCreateLockRetry(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	previousLimit := transferCreateLockRetryLimit
	previousDelay := transferCreateLockRetryDelay
	transferCreateLockRetryLimit = 3
	transferCreateLockRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		transferCreateLockRetryLimit = previousLimit
		transferCreateLockRetryDelay = previousDelay
	})

	holder, err := Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	locked, err := TryAdvisoryTransactionLock(ctx, holder, TransferCreateLockNamespace, transferTestInstance)
	if err != nil || !locked {
		t.Fatalf("hold instance lock: locked=%v err=%v", locked, err)
	}

	start := time.Now()
	_, err = CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID: transferTestInstance, FromUserID: transferTestFromUser, ToUserID: transferTestToUser,
		Snapshot: TransferSnapshot{}, Fee: 0, InstanceName: "transfer-db-box",
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTransferAlreadyPending) {
		t.Fatalf("err = %v, want ErrTransferAlreadyPending", err)
	}
	// 3 attempts -> sleeps 10ms + 20ms + 30ms (the TS sleeps after the last too).
	if elapsed < 55*time.Millisecond {
		t.Fatalf("elapsed = %v; the retry schedule was not honoured", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v; a busy lock must not block", elapsed)
	}
	_ = holder.Rollback(ctx)

	// Once the holder releases, the very next call succeeds (first attempt).
	if _, err := CreateTransferWithFee(ctx, TransferCreateWithFeeInput{
		InstanceID: transferTestInstance, FromUserID: transferTestFromUser, ToUserID: transferTestToUser,
		Snapshot: TransferSnapshot{}, Fee: 0, InstanceName: "transfer-db-box",
	}); err != nil {
		t.Fatalf("create after release: %v", err)
	}
}

// TestTransferDatabaseStateMachine pins claim/reject/cancel/rollback semantics.
func TestTransferDatabaseStateMachine(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	now := time.Now().UTC().Truncate(time.Millisecond)
	transferTestInsertTransfer(t, ctx, 994_101, transferTestInstance, transferTestFromUser, transferTestToUser,
		models.TransferStatusPending, nil, now, nil)

	if claimed, err := ClaimTransferForAccept(ctx, 994_101, transferTestOtherUser); err != nil || claimed != 0 {
		t.Fatalf("wrong-recipient claim = %d err %v, want 0", claimed, err)
	}
	if claimed, err := ClaimTransferForPush(ctx, 994_101, transferTestToUser); err != nil || claimed != 0 {
		t.Fatalf("wrong-sender push claim = %d err %v, want 0", claimed, err)
	}
	if claimed, err := ClaimTransferForPush(ctx, 994_101, transferTestFromUser); err != nil || claimed != 1 {
		t.Fatalf("sender push claim = %d err %v, want 1", claimed, err)
	}
	if err := RollbackTransferToPending(ctx, 994_101); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if rejected, err := RejectPendingTransfer(ctx, 994_101, transferTestFromUser, nil); err != nil || rejected != 0 {
		t.Fatalf("wrong-recipient reject = %d err %v, want 0", rejected, err)
	}
	reason := "not interested"
	if rejected, err := RejectPendingTransfer(ctx, 994_101, transferTestToUser, &reason); err != nil || rejected != 1 {
		t.Fatalf("reject = %d err %v, want 1", rejected, err)
	}
	if rejected, err := RejectPendingTransfer(ctx, 994_101, transferTestToUser, &reason); err != nil || rejected != 0 {
		t.Fatalf("second reject = %d err %v, want 0", rejected, err)
	}
	if claimed, err := ClaimTransferForAccept(ctx, 994_101, transferTestToUser); err != nil || claimed != 0 {
		t.Fatalf("claim after reject = %d err %v, want 0", claimed, err)
	}

	transferTestInsertTransfer(t, ctx, 994_102, transferTestInstance2, transferTestFromUser, transferTestToUser,
		models.TransferStatusPending, nil, now, nil)
	if cancelled, err := CancelPendingTransfer(ctx, 994_102, transferTestToUser); err != nil || cancelled != 0 {
		t.Fatalf("wrong-sender cancel = %d err %v, want 0", cancelled, err)
	}
	if cancelled, err := CancelPendingTransfer(ctx, 994_102, transferTestFromUser); err != nil || cancelled != 1 {
		t.Fatalf("cancel = %d err %v, want 1", cancelled, err)
	}
	if claimed, err := ClaimTransferForAccept(ctx, 994_102, transferTestToUser); err != nil || claimed != 0 {
		t.Fatalf("claim after cancel = %d err %v, want 0", claimed, err)
	}

	// The unconditional helpers mirror Prisma update() and report a missing row.
	if err := CancelTransfer(ctx, 994_102); err != nil {
		t.Fatalf("unconditional cancel: %v", err)
	}
	if err := AcceptTransfer(ctx, 994_999); !errors.Is(err, ErrTransferRecordNotFound) {
		t.Fatalf("accept missing err = %v, want ErrTransferRecordNotFound", err)
	}
	if err := RejectTransfer(ctx, 994_999, nil); !errors.Is(err, ErrTransferRecordNotFound) {
		t.Fatalf("reject missing err = %v, want ErrTransferRecordNotFound", err)
	}
	if err := CancelTransfer(ctx, 994_999); !errors.Is(err, ErrTransferRecordNotFound) {
		t.Fatalf("cancel missing err = %v, want ErrTransferRecordNotFound", err)
	}
}

// TestTransferDatabaseRefund pins the refund credit + snapshot log for both reasons.
func TestTransferDatabaseRefund(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	if _, err := Pool().Exec(ctx, `UPDATE users SET balance = 10.00 WHERE id = $1`, transferTestFromUser); err != nil {
		t.Fatalf("set balance: %v", err)
	}
	if success, message := RefundTransferFee(ctx, TransferRefundInput{
		TransferID: 994_201, FromUserID: transferTestFromUser, Fee: 4,
		InstanceID: transferTestInstance, InstanceName: "box", Reason: TransferRefundReasonRejected,
	}); !success {
		t.Fatalf("refund failed: %s", message)
	}
	if balance := transferTestBalance(t, ctx, transferTestFromUser); balance != 14 {
		t.Fatalf("balance = %v, want 14", balance)
	}

	var log struct {
		Type          models.BalanceLogType
		Amount        models.Decimal
		BalanceBefore models.Decimal
		BalanceAfter  models.Decimal
		Remark        *string
	}
	if err := Pool().QueryRow(ctx,
		`SELECT type, amount, balance_before, balance_after, remark FROM balance_logs
		  WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, transferTestFromUser).
		Scan(&log.Type, &log.Amount, &log.BalanceBefore, &log.BalanceAfter, &log.Remark); err != nil {
		t.Fatalf("balance log: %v", err)
	}
	if log.Type != models.BalanceLogTypeTransferRefund || log.Amount.String() != "4" ||
		log.BalanceBefore.String() != "10" || log.BalanceAfter.String() != "14" {
		t.Fatalf("log = %s %s %s %s", log.Type, log.Amount, log.BalanceBefore, log.BalanceAfter)
	}
	if log.Remark == nil || *log.Remark != `转移实例 "box" 被拒绝，退还手续费` {
		t.Fatalf("remark = %v", log.Remark)
	}

	if success, message := RefundTransferFee(ctx, TransferRefundInput{
		TransferID: 994_202, FromUserID: transferTestFromUser, Fee: 1,
		InstanceID: transferTestInstance, InstanceName: "box", Reason: TransferRefundReasonCancelled,
	}); !success {
		t.Fatalf("cancel refund failed: %s", message)
	}
	if err := Pool().QueryRow(ctx,
		`SELECT remark FROM balance_logs WHERE user_id = $1 ORDER BY id DESC LIMIT 1`,
		transferTestFromUser).Scan(&log.Remark); err != nil {
		t.Fatalf("cancel remark: %v", err)
	}
	if log.Remark == nil || *log.Remark != `取消转移实例 "box"，退还手续费` {
		t.Fatalf("cancel remark = %v", log.Remark)
	}

	// A balance lock conflict is reported, not swallowed as success.
	holder, err := Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if locked, err := TryAdvisoryTransactionLock(ctx, holder, UserBalanceLockNamespace, transferTestFromUser); err != nil || !locked {
		t.Fatalf("hold balance lock: %v %v", locked, err)
	}
	success, message := RefundTransferFee(ctx, TransferRefundInput{
		TransferID: 994_203, FromUserID: transferTestFromUser, Fee: 1,
		InstanceID: transferTestInstance, InstanceName: "box", Reason: TransferRefundReasonRejected,
	})
	if success || message != "BALANCE_CONFLICT" {
		t.Fatalf("conflict refund = success %v message %q", success, message)
	}
	_ = holder.Rollback(ctx)
}

// TestTransferDatabaseListsAndPendingCount pins the query filters, ordering and
// pagination metadata.
func TestTransferDatabaseListsAndPendingCount(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	remarkAlpha := "alpha note"
	remarkBeta := "beta note"
	remarkGamma := "gamma note"
	transferTestInsertTransfer(t, ctx, 994_301, transferTestInstance, transferTestFromUser, transferTestToUser,
		models.TransferStatusPending, nil, base, &remarkAlpha)
	transferTestInsertTransfer(t, ctx, 994_302, transferTestInstance2, transferTestFromUser, transferTestToUser,
		models.TransferStatusRejected, nil, base.Add(time.Minute), &remarkBeta)
	transferTestInsertTransfer(t, ctx, 994_303, transferTestInstance3, transferTestFromUser, transferTestOtherUser,
		models.TransferStatusAccepted, nil, base.Add(2*time.Minute), &remarkGamma)

	sent, err := GetTransfersSent(ctx, transferTestFromUser, TransferListOptions{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("GetTransfersSent: %v", err)
	}
	if sent.Total != 3 || sent.TotalPages != 2 || len(sent.Items) != 2 || sent.Page != 1 || sent.PageSize != 2 {
		t.Fatalf("sent = total %d pages %d items %d page %d size %d",
			sent.Total, sent.TotalPages, len(sent.Items), sent.Page, sent.PageSize)
	}
	if sent.Items[0].ID != 994_303 || sent.Items[1].ID != 994_302 {
		t.Fatalf("sent order = %d,%d, want 994303,994302", sent.Items[0].ID, sent.Items[1].ID)
	}
	if sent.Items[0].Instance == nil || sent.Items[0].Instance.Name != "transfer-db-box-three" {
		t.Fatalf("instance projection = %#v", sent.Items[0].Instance)
	}
	if sent.Items[0].ToUser == nil || sent.Items[0].ToUser.Username != "transfer-db-other" {
		t.Fatalf("toUser projection = %#v", sent.Items[0].ToUser)
	}

	pageTwo, err := GetTransfersSent(ctx, transferTestFromUser, TransferListOptions{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("page two: %v", err)
	}
	if len(pageTwo.Items) != 1 || pageTwo.Items[0].ID != 994_301 {
		t.Fatalf("page two items = %#v", pageTwo.Items)
	}

	pending, err := GetTransfersSent(ctx, transferTestFromUser, TransferListOptions{Page: 1, PageSize: 20, Status: "pending"})
	if err != nil {
		t.Fatalf("pending filter: %v", err)
	}
	if pending.Total != 1 || pending.Items[0].ID != 994_301 {
		t.Fatalf("pending filter = %#v", pending.Items)
	}

	byRemark, err := GetTransfersSent(ctx, transferTestFromUser, TransferListOptions{Page: 1, PageSize: 20, Search: "beta"})
	if err != nil {
		t.Fatalf("remark search: %v", err)
	}
	if byRemark.Total != 1 || byRemark.Items[0].ID != 994_302 {
		t.Fatalf("remark search = %#v", byRemark.Items)
	}

	// Sent search matches the recipient's username.
	byRecipient, err := GetTransfersSent(ctx, transferTestFromUser, TransferListOptions{Page: 1, PageSize: 20, Search: "transfer-db-to"})
	if err != nil {
		t.Fatalf("recipient search: %v", err)
	}
	if byRecipient.Total != 2 {
		t.Fatalf("recipient search total = %d, want 2", byRecipient.Total)
	}

	received, err := GetTransfersReceived(ctx, transferTestToUser, TransferListOptions{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("GetTransfersReceived: %v", err)
	}
	if received.Total != 2 {
		t.Fatalf("received total = %d, want 2", received.Total)
	}
	// Received includes the sender, not the recipient.
	if received.Items[0].FromUser == nil || received.Items[0].FromUser.Username != "transfer-db-from" {
		t.Fatalf("fromUser projection = %#v", received.Items[0].FromUser)
	}

	// Received search matches the sender's username.
	receivedBySender, err := GetTransfersReceived(ctx, transferTestToUser, TransferListOptions{Page: 1, PageSize: 20, Search: "transfer-db-from"})
	if err != nil {
		t.Fatalf("sender search: %v", err)
	}
	if receivedBySender.Total != 2 {
		t.Fatalf("sender search total = %d, want 2", receivedBySender.Total)
	}

	if count, err := GetPendingReceivedCount(ctx, transferTestToUser); err != nil || count != 1 {
		t.Fatalf("pending count = %d err %v, want 1", count, err)
	}
	if count, err := GetPendingReceivedCount(ctx, transferTestOtherUser); err != nil || count != 0 {
		t.Fatalf("other pending count = %d err %v, want 0", count, err)
	}

	clamped, err := GetTransfersSent(ctx, transferTestFromUser, TransferListOptions{Page: 0, PageSize: 1000})
	if err != nil {
		t.Fatalf("clamped: %v", err)
	}
	if clamped.Page != 1 || clamped.PageSize != 100 || clamped.TotalPages != 1 {
		t.Fatalf("clamped = page %d size %d pages %d", clamped.Page, clamped.PageSize, clamped.TotalPages)
	}

	if pendingFlag, err := HasPendingTransfer(ctx, transferTestInstance); err != nil || !pendingFlag {
		t.Fatalf("HasPendingTransfer = %v err %v, want true", pendingFlag, err)
	}
	transfer, err := GetPendingTransferByInstanceID(ctx, transferTestInstance)
	if err != nil || transfer == nil || transfer.ID != 994_301 {
		t.Fatalf("GetPendingTransferByInstanceID = %#v err %v", transfer, err)
	}
	if transfer.FromUser == nil || transfer.ToUser == nil {
		t.Fatalf("pending transfer users = %#v %#v", transfer.FromUser, transfer.ToUser)
	}
	if transfer, err := GetTransferByIDWithFee(ctx, 994_301); err != nil || transfer == nil || transfer.ID != 994_301 {
		t.Fatalf("GetTransferByIDWithFee = %#v err %v", transfer, err)
	}
}

// TestTransferDatabaseCleanup pins both cleanup paths and the 30-minute window.
func TestTransferDatabaseCleanup(t *testing.T) {
	ctx := transferTestDatabase(t)
	transferTestResetTransfer(t, ctx)

	now := time.Now().UTC().Truncate(time.Millisecond)
	transferTestInsertTransfer(t, ctx, 994_401, transferTestInstance, transferTestFromUser, transferTestToUser,
		models.TransferStatusProcessing, nil, now, nil)
	oldTimestamp := now.Add(-31 * time.Minute)
	transferTestInsertTransfer(t, ctx, 994_402, transferTestInstance2, transferTestFromUser, transferTestToUser,
		models.TransferStatusProcessing, nil, oldTimestamp, nil)
	if _, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET updated_at = $1 WHERE id = 994_401`, now); err != nil {
		t.Fatalf("touch recent: %v", err)
	}
	if _, err := Pool().Exec(ctx,
		`UPDATE instance_transfers SET updated_at = $1 WHERE id = 994_402`, oldTimestamp); err != nil {
		t.Fatalf("touch old: %v", err)
	}

	cleaned, err := CleanupTimeoutTransfers(ctx)
	if err != nil {
		t.Fatalf("CleanupTimeoutTransfers: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("timeout cleanup = %d, want 1", cleaned)
	}
	var status models.TransferStatus
	if err := Pool().QueryRow(ctx, `SELECT status FROM instance_transfers WHERE id = 994_401`).Scan(&status); err != nil {
		t.Fatalf("recent status: %v", err)
	}
	if status != models.TransferStatusProcessing {
		t.Fatalf("recent transfer status = %s, want processing", status)
	}
	if err := Pool().QueryRow(ctx, `SELECT status FROM instance_transfers WHERE id = 994_402`).Scan(&status); err != nil {
		t.Fatalf("old status: %v", err)
	}
	if status != models.TransferStatusPending {
		t.Fatalf("old transfer status = %s, want pending", status)
	}

	stale, err := CleanupStaleTransfers(ctx)
	if err != nil {
		t.Fatalf("CleanupStaleTransfers: %v", err)
	}
	if stale != 1 {
		t.Fatalf("stale cleanup = %d, want 1", stale)
	}
	if err := Pool().QueryRow(ctx, `SELECT status FROM instance_transfers WHERE id = 994_401`).Scan(&status); err != nil {
		t.Fatalf("recent status: %v", err)
	}
	if status != models.TransferStatusPending {
		t.Fatalf("recent transfer status = %s, want pending", status)
	}
}

func transferTestStringPtr(value string) *string { return &value }
