// Tests for routes/transfers.go (server/src/routes/transfers.ts).
//
// Pure tests pin the request plumbing: route mounting (Gin must accept the
// static/param sibling routes), the JS `Number()` id grammar, body normalization,
// the new-name builder, the nanoid alphabet and the invalid-list-type envelope.
//
// The state-machine tests run against PostgreSQL (skipped unless
// PAYINCUS_TEST_DATABASE_URL is set) because the ownership change, the atomic
// claim and the refund are SQL. Incus, Caddy and the clock are replaced through
// the package seams, so no node is contacted. That combination is what proves:
//
//   - one accept moves the owner exactly once (a second accept is 400 and writes
//     nothing);
//   - accept refuses a running instance / a failed rename and rolls the claim back;
//   - reject and cancel refund the fee exactly once and then refuse accept;
//   - push moves ownership with the recipient's incus id;
//   - the create guards (owner, status, host flag, self, banned, pending) answer
//     the same statuses/codes as the TypeScript.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== pure tests ====================

// TestTransfersRouteMount pins every registered path and that Gin accepts the
// static/param siblings (`/users/search`, `/pending-count` next to `/:id`).
func TestTransfersRouteMount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterTransfers(engine)

	wanted := map[string]bool{
		"GET /api/transfers/users/search":                    false,
		"POST /api/transfers/instances/:instanceId/transfer": false,
		"GET /api/transfers":                                 false,
		"GET /api/transfers/":                                false,
		"GET /api/transfers/pending-count":                   false,
		"GET /api/transfers/:id":                             false,
		"POST /api/transfers/:id/accept":                     false,
		"POST /api/transfers/:id/reject":                     false,
		"POST /api/transfers/:id/cancel":                     false,
		"POST /api/transfers/:id/push":                       false,
	}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := wanted[key]; !ok {
			t.Errorf("unexpected route %s", key)
			continue
		}
		wanted[key] = true
	}
	for key, seen := range wanted {
		if !seen {
			t.Errorf("missing route %s", key)
		}
	}
}

// TestTransfersParseRouteID pins the JS Number() grammar of parsePositiveInteger.
func TestTransfersParseRouteID(t *testing.T) {
	valid := map[string]int32{
		"1":          1,
		"01":         1,
		"1.0":        1,
		"1e3":        1000,
		"0x10":       16,
		"0b11":       3,
		"0o17":       15,
		" 1 ":        1,
		"2147483647": 2147483647,
	}
	for raw, want := range valid {
		got, ok := transfersParseRouteID(raw)
		if !ok || got != want {
			t.Errorf("parseRouteID(%q) = %d,%v want %d,true", raw, got, ok, want)
		}
	}
	invalid := []string{"", "0", "-1", "-0", "1.5", "abc", "1a", "NaN", "Infinity",
		"9007199254740992", "2147483648", "1_000", "0x", "+", "."}
	for _, raw := range invalid {
		if got, ok := transfersParseRouteID(raw); ok {
			t.Errorf("parseRouteID(%q) = %d,true want invalid", raw, got)
		}
	}
}

// TestTransfersNormalizeCreateInput pins every message the handler can return.
func TestTransfersNormalizeCreateInput(t *testing.T) {
	input, message := transfersNormalizeCreateInput(map[string]any{
		"targetUsername": "  bob  ",
		"remark":         "  hello  ",
	})
	if message != "" || input.TargetUsername != "bob" || input.Remark == nil || *input.Remark != "hello" {
		t.Fatalf("normalize = %#v message %q", input, message)
	}

	input, message = transfersNormalizeCreateInput(map[string]any{"targetUsername": "bob", "remark": "   "})
	if message != "" || input.Remark != nil {
		t.Fatalf("blank remark = %#v message %q", input.Remark, message)
	}

	cases := []struct {
		body    any
		message string
	}{
		{"not an object", "Invalid request body"},
		{[]any{}, "Invalid request body"},
		{map[string]any{}, "Target username is required"},
		{map[string]any{"targetUsername": 12}, "Target username is required"},
		{map[string]any{"targetUsername": "a"}, "Target username must be 2-64 characters"},
		{map[string]any{"targetUsername": "   "}, "Target username must be 2-64 characters"},
		{map[string]any{"targetUsername": strings.Repeat("a", 65)}, "Target username must be 2-64 characters"},
		{map[string]any{"targetUsername": "bob", "remark": 5}, "Remark must be a string"},
		{map[string]any{"targetUsername": "bob", "remark": strings.Repeat("r", 201)}, "Remark must be no longer than 200 characters"},
	}
	for _, testCase := range cases {
		if _, message := transfersNormalizeCreateInput(testCase.body); message != testCase.message {
			t.Errorf("normalize(%#v) message = %q, want %q", testCase.body, message, testCase.message)
		}
	}
}

// TestTransfersTransferredName pins the shared accept/push name builder.
func TestTransfersTransferredName(t *testing.T) {
	suffix := "（alice转）"
	if got := transfersTransferredName("box", suffix); got != "box（alice转）" {
		t.Fatalf("name = %q", got)
	}
	if got := transfersTransferredName("box（alice转）", suffix); got != "box（alice转）" {
		t.Fatalf("already suffixed name = %q", got)
	}

	longName := strings.Repeat("b", 60)
	got := transfersTransferredName(longName, suffix)
	if transfersJSStringLength(got) != transfersInstanceNameMaxLength {
		t.Fatalf("long name = %q (len %d), want 64 units", got, transfersJSStringLength(got))
	}
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("long name lost the suffix: %q", got)
	}

	// The TypeScript uses substring(0, 64 - suffix.length); a suffix longer than 64
	// units clamps to an empty prefix, so only the suffix survives.
	hugeSuffix := "（" + strings.Repeat("x", 70) + "转）"
	if got := transfersTransferredName("box", hugeSuffix); got != hugeSuffix {
		t.Fatalf("huge suffix name = %q", got)
	}
}

// TestTransfersRandomIDShape pins the nanoid contract the Incus id depends on.
func TestTransfersRandomIDShape(t *testing.T) {
	seen := map[string]bool{}
	for index := 0; index < 200; index++ {
		id := transfersRandomID()
		if len(id) != transfersNanoidLength {
			t.Fatalf("id = %q, want %d chars", id, transfersNanoidLength)
		}
		for _, char := range id {
			if !strings.ContainsRune(transfersNanoidAlphabet, char) {
				t.Fatalf("id = %q contains %q", id, char)
			}
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Fatalf("only %d unique ids out of 200; the generator is not random", len(seen))
	}
}

// TestTransfersListRejectsInvalidType pins the 400 envelope before any DB access.
func TestTransfersListRejectsInvalidType(t *testing.T) {
	engine := transfersRouteTestRouter()
	for _, path := range []string{
		"/api/transfers?type=bogus",
		"/api/transfers?type=SENT",
		"/api/transfers",
		"/api/transfers?type=sent&type=received",
	} {
		status, body := transfersRouteTestCall(t, engine, http.MethodGet, path, 995_002, "transfer-route-to", "")
		if status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, status)
		}
		if body["code"] != "INVALID_PARAMS" || body["details"] != `type must be "sent" or "received"` {
			t.Fatalf("%s body = %#v", path, body)
		}
	}
}

// ==================== database fixtures ====================

const (
	transfersRouteFromUser  int32 = 995_001
	transfersRouteToUser    int32 = 995_002
	transfersRouteThirdUser int32 = 995_003
	transfersRouteBanned    int32 = 995_004

	transfersRouteHost         int32 = 995_001
	transfersRouteHostDisabled int32 = 995_002

	transfersRouteInstance     int32 = 995_001
	transfersRouteRunning      int32 = 995_002
	transfersRouteSuspended    int32 = 995_003
	transfersRouteDisabledHost int32 = 995_004
	transfersRouteOtherOwned   int32 = 995_005

	transfersRouteRangeLow  int32 = 995_000
	transfersRouteRangeHigh int32 = 995_999
)

var (
	transfersRouteDBOnce sync.Once
	transfersRouteDBErr  error
)

// transfersRouteTestDatabase seeds the fixture set and returns the context.
func transfersRouteTestDatabase(t *testing.T) context.Context {
	t.Helper()

	dsn := os.Getenv("PAYINCUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PAYINCUS_TEST_DATABASE_URL to a throwaway database with the Prisma schema")
	}
	transfersRouteDBOnce.Do(func() {
		_ = os.Setenv("DATABASE_URL", dsn)
		transfersRouteDBErr = db.Init(context.Background())
	})
	if transfersRouteDBErr != nil {
		t.Fatalf("db.Init: %v", transfersRouteDBErr)
	}

	ctx := context.Background()
	transfersRouteCleanup(t, ctx)
	t.Cleanup(func() { transfersRouteCleanup(t, ctx) })
	transfersRouteSeed(t, ctx)
	return ctx
}

func transfersRouteCleanup(t *testing.T, ctx context.Context) {
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
		`DELETE FROM logs WHERE user_id BETWEEN $1 AND $2`,
		`DELETE FROM instances WHERE id BETWEEN $1 AND $2`,
		`DELETE FROM hosts WHERE id BETWEEN $1 AND $2`,
		`DELETE FROM users WHERE id BETWEEN $1 AND $2`,
	}
	for _, statement := range statements {
		if _, err := db.Pool().Exec(ctx, statement, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
			t.Fatalf("cleanup %q: %v", statement, err)
		}
	}
}

func transfersRouteSeed(t *testing.T, ctx context.Context) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)

	users := []struct {
		id       int32
		username string
		status   models.UserStatus
		balance  string
	}{
		{transfersRouteFromUser, "transfer-route-from", models.UserStatusActive, "20.00"},
		{transfersRouteToUser, "transfer-route-to", models.UserStatusActive, "0.00"},
		{transfersRouteThirdUser, "transfer-route-third", models.UserStatusActive, "0.00"},
		{transfersRouteBanned, "transfer-route-banned", models.UserStatusBanned, "0.00"},
	}
	for _, user := range users {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO users (id, username, password_hash, balance, status, updated_at)
			 VALUES ($1, $2, 'x', $3::numeric, $4, $5)`,
			user.id, user.username, user.balance, string(user.status), now); err != nil {
			t.Fatalf("insert user %d: %v", user.id, err)
		}
	}

	hosts := []struct {
		id              int32
		transferEnabled bool
	}{
		{transfersRouteHost, true},
		{transfersRouteHostDisabled, false},
	}
	for _, host := range hosts {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO hosts (id, user_id, name, url, transfer_enabled, caddy_enabled, updated_at)
			 VALUES ($1, $2, $3, $4, $5, false, $6)`,
			host.id, transfersRouteFromUser, "transfer-route-host-"+strconv.Itoa(int(host.id)),
			"https://127.0.0.1:8443", host.transferEnabled, now); err != nil {
			t.Fatalf("insert host %d: %v", host.id, err)
		}
	}

	instances := []struct {
		id      int32
		incusID string
		name    string
		owner   int32
		hostID  int32
		status  models.InstanceStatus
		suspend *string
	}{
		{transfersRouteInstance, "transfer-route-incus-995001", "route-box", transfersRouteFromUser, transfersRouteHost, models.InstanceStatusStopped, nil},
		{transfersRouteRunning, "transfer-route-incus-995002", "route-running", transfersRouteFromUser, transfersRouteHost, models.InstanceStatusRunning, nil},
		{transfersRouteSuspended, "transfer-route-incus-995003", "route-suspended", transfersRouteFromUser, transfersRouteHost, models.InstanceStatusSuspended, transferTestStringPtrRoute("expired")},
		{transfersRouteOtherOwned, "transfer-route-incus-995005", "route-other", transfersRouteToUser, transfersRouteHost, models.InstanceStatusStopped, nil},
		{transfersRouteDisabledHost, "transfer-route-incus-995004", "route-disabled-host", transfersRouteFromUser, transfersRouteHostDisabled, models.InstanceStatusStopped, nil},
	}
	for _, instance := range instances {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO instances
			   (id, incus_id, name, user_id, host_id, image, status, suspend_reason, cpu, memory, disk, updated_at)
			 VALUES ($1, $2, $3, $4, $5, 'debian/12', $6, $7, 1, 512, 5120, $8)`,
			instance.id, instance.incusID, instance.name, instance.owner, instance.hostID,
			string(instance.status), instance.suspend, now); err != nil {
			t.Fatalf("insert instance %d: %v", instance.id, err)
		}
	}
}

func transferTestStringPtrRoute(value string) *string { return &value }

// transfersRouteReset clears transfer state between scenarios and resets the
// instance owner/name so a previous accept cannot leak into the next test.
func transfersRouteReset(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM instance_transfers WHERE instance_id BETWEEN $1 AND $2`,
		transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete transfers: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM instance_billing_records WHERE instance_id BETWEEN $1 AND $2`,
		transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete billing: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM balance_logs WHERE user_id BETWEEN $1 AND $2`,
		transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete balance logs: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM logs WHERE user_id BETWEEN $1 AND $2`,
		transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete logs: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM snapshot_policies WHERE instance_id BETWEEN $1 AND $2`, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete snapshot policies: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM backup_policies WHERE instance_id BETWEEN $1 AND $2`, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete backup policies: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM snapshots WHERE instance_id BETWEEN $1 AND $2`, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete snapshots: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM backups WHERE instance_id BETWEEN $1 AND $2`, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete backups: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM port_mappings WHERE instance_id BETWEEN $1 AND $2`, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete port mappings: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM proxy_sites WHERE instance_id BETWEEN $1 AND $2`, transfersRouteRangeLow, transfersRouteRangeHigh); err != nil {
		t.Fatalf("delete proxy sites: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE users SET balance = CASE WHEN id = $3 THEN 20.00 ELSE 0 END WHERE id BETWEEN $1 AND $2`,
		transfersRouteRangeLow, transfersRouteRangeHigh, transfersRouteFromUser); err != nil {
		t.Fatalf("reset balances: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE instances SET user_id = $2, name = $3, incus_id = $4, auto_renew = true, display_order = 1
		  WHERE id = $1`,
		transfersRouteInstance, transfersRouteFromUser, "route-box", "transfer-route-incus-995001"); err != nil {
		t.Fatalf("reset instance: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE instances SET user_id = $2, name = $3, incus_id = $4 WHERE id = $1`,
		transfersRouteRunning, transfersRouteFromUser, "route-running", "transfer-route-incus-995002"); err != nil {
		t.Fatalf("reset running instance: %v", err)
	}
}

// transfersRouteInsertTransfer inserts a transfer row for the route tests.
func transfersRouteInsertTransfer(
	t *testing.T,
	ctx context.Context,
	id, instanceID, fromUserID, toUserID int32,
	status models.TransferStatus,
	fee *string,
) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO instance_transfers
		   (id, instance_id, from_user_id, to_user_id, status, snapshot, fee, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6::numeric, $7, $7)`,
		id, instanceID, fromUserID, toUserID, string(status), fee, now); err != nil {
		t.Fatalf("insert transfer %d: %v", id, err)
	}
}

// transfersRouteInsertAttachments creates one row in every table accept/push must
// clean up.
func transfersRouteInsertAttachments(t *testing.T, ctx context.Context, instanceID int32) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO proxy_sites (id, instance_id, host_id, domain, target_port, updated_at)
		 VALUES ($1, $2, $3, $4, 80, $5)`,
		instanceID+100_000, instanceID, transfersRouteHost, "route-"+strconv.Itoa(int(instanceID))+".example", now); err != nil {
		t.Fatalf("insert proxy site: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO port_mappings (id, instance_id, host_id, protocol, public_port, private_port)
		 VALUES ($1, $2, $3, 'tcp', $4, 80)`,
		instanceID+100_000, instanceID, transfersRouteHost, 25_000+int(instanceID%1_000)); err != nil {
		t.Fatalf("insert port mapping: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO snapshots (id, instance_id, incus_name, name) VALUES ($1, $2, $3, $4)`,
		instanceID+100_000, instanceID, "snap-"+strconv.Itoa(int(instanceID)), "snap"); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO backups (id, instance_id, incus_name, name, status) VALUES ($1, $2, $3, $4, 'ready')`,
		instanceID+100_000, instanceID, "bak-"+strconv.Itoa(int(instanceID)), "bak"); err != nil {
		t.Fatalf("insert backup: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO snapshot_policies (id, instance_id, updated_at) VALUES ($1, $2, $3)`,
		instanceID+100_000, instanceID, now); err != nil {
		t.Fatalf("insert snapshot policy: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO backup_policies (id, instance_id, updated_at) VALUES ($1, $2, $3)`,
		instanceID+100_000, instanceID, now); err != nil {
		t.Fatalf("insert backup policy: %v", err)
	}
}

// ==================== route harness ====================

// transfersRouteTestRouter mounts the transfer routes behind a header-driven fake
// auth layer, so the handlers run without JWT/DB token plumbing.
func transfersRouteTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("/api/transfers")
	group.Use(func(c *gin.Context) {
		rawID := c.GetHeader("X-Test-User")
		if rawID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			return
		}
		id, _ := strconv.Atoi(rawID)
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{
			ID:       int32(id),
			Username: c.GetHeader("X-Test-Username"),
			Role:     "user",
			Status:   "active",
		})
		c.Next()
	})
	transfersMount(group)
	return engine
}

// transfersRouteTestCall performs one request and decodes a JSON object body.
func transfersRouteTestCall(
	t *testing.T,
	engine *gin.Engine,
	method, path string,
	userID int32,
	username, body string,
) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if userID != 0 {
		request.Header.Set("X-Test-User", strconv.Itoa(int(userID)))
		request.Header.Set("X-Test-Username", username)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder.Code, decoded
}

// transfersRouteTestSeams replaces the Incus/Caddy seams and records the rename
// calls. The returned slices are read after the request.
func transfersRouteTestSeams(t *testing.T) (renames *[]string, removedDevices *[]string, deletedSnapshots *[]string, deletedBackups *[]string) {
	t.Helper()

	previousClient := TransfersGetIncusClient
	previousRename := TransfersRenameInstance
	previousRemove := TransfersRemoveDevice
	previousSnapshot := TransfersDeleteSnapshot
	previousBackup := TransfersDeleteBackup
	previousCaddy := TransfersGetCaddyClient
	previousNanoid := TransfersNanoid

	renameCalls := make([]string, 0, 4)
	deviceCalls := make([]string, 0, 4)
	snapshotCalls := make([]string, 0, 4)
	backupCalls := make([]string, 0, 4)

	TransfersGetIncusClient = func(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
		return &lib.IncusClient{}, nil
	}
	TransfersRenameInstance = func(ctx context.Context, client *lib.IncusClient, oldName, newName string) error {
		renameCalls = append(renameCalls, oldName+"->"+newName)
		return nil
	}
	TransfersRemoveDevice = func(ctx context.Context, client *lib.IncusClient, instanceName, deviceName string) (json.RawMessage, error) {
		deviceCalls = append(deviceCalls, instanceName+"/"+deviceName)
		return nil, nil
	}
	TransfersDeleteSnapshot = func(ctx context.Context, client *lib.IncusClient, instanceName, snapshotName string) (json.RawMessage, error) {
		snapshotCalls = append(snapshotCalls, instanceName+"/"+snapshotName)
		return nil, nil
	}
	TransfersDeleteBackup = func(ctx context.Context, client *lib.IncusClient, instanceName, backupName string) (json.RawMessage, error) {
		backupCalls = append(backupCalls, instanceName+"/"+backupName)
		return nil, nil
	}
	TransfersGetCaddyClient = func(host *db.HostsRecord) (transfersCaddySiteDeleter, error) {
		return nil, errors.New("caddy unavailable in tests")
	}
	TransfersNanoid = func() string { return "abcd1234" }

	t.Cleanup(func() {
		TransfersGetIncusClient = previousClient
		TransfersRenameInstance = previousRename
		TransfersRemoveDevice = previousRemove
		TransfersDeleteSnapshot = previousSnapshot
		TransfersDeleteBackup = previousBackup
		TransfersGetCaddyClient = previousCaddy
		TransfersNanoid = previousNanoid
	})

	return &renameCalls, &deviceCalls, &snapshotCalls, &backupCalls
}

// ==================== create guards ====================

// TestTransfersCreateGuards pins the create route's ordered guard ladder.
func TestTransfersCreateGuards(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	engine := transfersRouteTestRouter()

	create := func(userID int32, username, path, body string) (int, map[string]any) {
		return transfersRouteTestCall(t, engine, http.MethodPost, path, userID, username, body)
	}
	validBody := `{"targetUsername":"transfer-route-to"}`
	createPath := "/api/transfers/instances/995001/transfer"

	if status, body := create(0, "", createPath, validBody); status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d body %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", "/api/transfers/instances/0/transfer", validBody); status != http.StatusBadRequest || body["code"] != "INVALID_ID" {
		t.Fatalf("invalid id = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, `{"targetUsername":"a"}`); status != http.StatusBadRequest || body["code"] != "VALIDATION_ERROR" || body["error"] != "targetUsername is too short" {
		t.Fatalf("short username = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, `{"targetUsername":"  "}`); status != http.StatusBadRequest || body["error"] != "Target username must be 2-64 characters" {
		t.Fatalf("blank username = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", "/api/transfers/instances/999999/transfer", validBody); status != http.StatusNotFound || body["code"] != "INSTANCE_NOT_FOUND" {
		t.Fatalf("missing instance = %d %#v", status, body)
	}
	if status, body := create(transfersRouteToUser, "transfer-route-to", createPath, validBody); status != http.StatusForbidden || body["code"] != "FORBIDDEN" {
		t.Fatalf("non-owner = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", "/api/transfers/instances/995003/transfer", validBody); status != http.StatusForbidden || body["code"] != "INSTANCE_SUSPENDED_EXPIRED" {
		t.Fatalf("suspended = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", "/api/transfers/instances/995004/transfer", validBody); status != http.StatusBadRequest || body["code"] != "TRANSFER_HOST_DISABLED" {
		t.Fatalf("disabled host = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, `{"targetUsername":"transfer-route-from"}`); status != http.StatusBadRequest || body["code"] != "TRANSFER_TO_SELF" {
		t.Fatalf("self = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, `{"targetUsername":"transfer-route-banned"}`); status != http.StatusBadRequest || body["code"] != "TRANSFER_TO_BANNED" {
		t.Fatalf("banned = %d %#v", status, body)
	}
	if status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, `{"targetUsername":"transfer-route-nobody"}`); status != http.StatusNotFound || body["code"] != "USER_NOT_FOUND" {
		t.Fatalf("missing target = %d %#v", status, body)
	}

	status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, `{"targetUsername":"transfer-route-to","remark":"  hi  "}`)
	if status != http.StatusOK || body["message"] != "Transfer request created" {
		t.Fatalf("create = %d %#v", status, body)
	}
	transfer := body["transfer"].(map[string]any)
	if transfer["status"] != "pending" {
		t.Fatalf("created transfer = %#v", transfer)
	}
	transferID := int32(transfer["id"].(float64))

	var remark *string
	var snapshotJSON []byte
	if err := db.Pool().QueryRow(ctx,
		`SELECT remark, snapshot FROM instance_transfers WHERE id = $1`, transferID).Scan(&remark, &snapshotJSON); err != nil {
		t.Fatalf("created row: %v", err)
	}
	if remark == nil || *remark != "hi" {
		t.Fatalf("remark = %v, want hi", remark)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot["originalName"] != "route-box" || snapshot["hostId"].(float64) != 995001 ||
		snapshot["hostName"] != "transfer-route-host-995001" || snapshot["backupsCount"].(float64) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	// A second create while the first is pending is rejected.
	if status, body := create(transfersRouteFromUser, "transfer-route-from", createPath, validBody); status != http.StatusBadRequest || body["code"] != "TRANSFER_ALREADY_PENDING" {
		t.Fatalf("duplicate = %d %#v", status, body)
	}

	_ = ctx
}

// ==================== accept ====================

// TestTransfersAcceptMovesOwnershipOnce is the central ownership test: the first
// accept renames and moves the instance, the second is rejected without side
// effects.
func TestTransfersAcceptMovesOwnershipOnce(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	transfersRouteInsertAttachments(t, ctx, transfersRouteInstance)
	fee := "5.00"
	transfersRouteInsertTransfer(t, ctx, 995101, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, &fee)

	renames, devices, snapshots, backups := transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995101/accept",
		transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusOK || body["message"] != "Transfer accepted" {
		t.Fatalf("accept = %d %#v", status, body)
	}
	if len(*renames) != 1 || (*renames)[0] != "transfer-route-incus-995001->u995002-abcd1234" {
		t.Fatalf("renames = %#v", *renames)
	}

	var owner int32
	var name, incusID string
	var autoRenew bool
	var displayOrder int32
	if err := db.Pool().QueryRow(ctx,
		`SELECT user_id, name, incus_id, auto_renew, display_order FROM instances WHERE id = $1`,
		transfersRouteInstance).Scan(&owner, &name, &incusID, &autoRenew, &displayOrder); err != nil {
		t.Fatalf("instance: %v", err)
	}
	if owner != transfersRouteToUser || name != "route-box（transfer-route-from转）" ||
		incusID != "u995002-abcd1234" || autoRenew || displayOrder != 0 {
		t.Fatalf("instance = owner %d name %q incus %q autoRenew %v order %d",
			owner, name, incusID, autoRenew, displayOrder)
	}

	var transferStatus models.TransferStatus
	var acceptedAt *time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT status, accepted_at FROM instance_transfers WHERE id = 995101`).Scan(&transferStatus, &acceptedAt); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if transferStatus != models.TransferStatusAccepted || acceptedAt == nil {
		t.Fatalf("transfer = %s acceptedAt %v", transferStatus, acceptedAt)
	}

	var billingCount int64
	var billingAmount models.Decimal
	var billingMonths int32
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MAX(amount), 0), COALESCE(MAX(months), 0)
		   FROM instance_billing_records WHERE instance_id = $1`, transfersRouteInstance).
		Scan(&billingCount, &billingAmount, &billingMonths); err != nil {
		t.Fatalf("billing: %v", err)
	}
	if billingCount != 1 || billingAmount.String() != "5" || billingMonths != 0 {
		t.Fatalf("billing = %d %s %d", billingCount, billingAmount, billingMonths)
	}

	// Attachments cleaned up with the NEW incus id.
	if len(*devices) != 1 || (*devices)[0] != "u995002-abcd1234/proxy-tcp-25001" {
		t.Fatalf("devices = %#v", *devices)
	}
	if len(*snapshots) != 1 || (*snapshots)[0] != "u995002-abcd1234/snap-995001" {
		t.Fatalf("snapshots = %#v", *snapshots)
	}
	if len(*backups) != 1 || (*backups)[0] != "u995002-abcd1234/bak-995001" {
		t.Fatalf("backups = %#v", *backups)
	}
	for table, want := range map[string]int64{
		"proxy_sites":       0,
		"port_mappings":     0,
		"snapshots":         0,
		"snapshot_policies": 0,
		"backup_policies":   0,
	} {
		var count int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE instance_id = $1`, transfersRouteInstance).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
	var availableBackups int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM backups WHERE instance_id = $1 AND status <> 'deleted'`,
		transfersRouteInstance).Scan(&availableBackups); err != nil {
		t.Fatalf("backups count: %v", err)
	}
	if availableBackups != 0 {
		t.Fatalf("live backups = %d, want 0", availableBackups)
	}

	// Second accept: 400 TRANSFER_NOT_PENDING, nothing renamed or billed again.
	status, body = transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995101/accept",
		transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusBadRequest || body["code"] != "TRANSFER_NOT_PENDING" {
		t.Fatalf("second accept = %d %#v", status, body)
	}
	if len(*renames) != 1 {
		t.Fatalf("second accept renamed again: %#v", *renames)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM instance_billing_records WHERE instance_id = $1`, transfersRouteInstance).
		Scan(&billingCount); err != nil {
		t.Fatalf("billing after second accept: %v", err)
	}
	if billingCount != 1 {
		t.Fatalf("billing rows after second accept = %d, want 1", billingCount)
	}
}

// TestTransfersAcceptRefusesWrongPartyAndBadID pins the 404/403/400 split after a
// lost claim.
func TestTransfersAcceptRefusesWrongPartyAndBadID(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	transfersRouteInsertTransfer(t, ctx, 995111, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, nil)
	transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/0/accept",
		transfersRouteToUser, "transfer-route-to", ""); status != http.StatusBadRequest || body["code"] != "INVALID_ID" {
		t.Fatalf("invalid id = %d %#v", status, body)
	}
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995999/accept",
		transfersRouteToUser, "transfer-route-to", ""); status != http.StatusNotFound || body["code"] != "TRANSFER_NOT_FOUND" {
		t.Fatalf("missing = %d %#v", status, body)
	}
	// The initiator is not the recipient.
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995111/accept",
		transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusForbidden || body["code"] != "FORBIDDEN" {
		t.Fatalf("wrong party = %d %#v", status, body)
	}
	// Still pending after the rejected attempt.
	var statusValue models.TransferStatus
	if err := db.Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = 995111`).Scan(&statusValue); err != nil {
		t.Fatalf("status: %v", err)
	}
	if statusValue != models.TransferStatusPending {
		t.Fatalf("status = %s, want pending", statusValue)
	}
}

// TestTransfersAcceptInstanceGuards pins the running-instance and deleted-instance
// branches, including the claim rollback.
func TestTransfersAcceptInstanceGuards(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	transfersRouteInsertTransfer(t, ctx, 995121, transfersRouteRunning, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, nil)
	transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995121/accept",
		transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusBadRequest || body["code"] != "INSTANCE_MUST_BE_STOPPED" {
		t.Fatalf("running = %d %#v", status, body)
	}
	var transferStatus models.TransferStatus
	if err := db.Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = 995121`).Scan(&transferStatus); err != nil {
		t.Fatalf("status: %v", err)
	}
	if transferStatus != models.TransferStatusPending {
		t.Fatalf("status = %s, want pending (rolled back)", transferStatus)
	}

	// A deleted instance cancels the transfer instead of rolling it back.
	transfersRouteInsertTransfer(t, ctx, 995122, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, nil)
	if _, err := db.Pool().Exec(ctx, `UPDATE instances SET status = 'deleted' WHERE id = $1`, transfersRouteInstance); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	status, body = transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995122/accept",
		transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusBadRequest || body["code"] != "INSTANCE_NOT_FOUND" {
		t.Fatalf("deleted instance = %d %#v", status, body)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = 995122`).Scan(&transferStatus); err != nil {
		t.Fatalf("status: %v", err)
	}
	if transferStatus != models.TransferStatusCancelled {
		t.Fatalf("status = %s, want cancelled", transferStatus)
	}
}

// TestTransfersAcceptRenameFailureRollsBack pins the compensation path: a failed
// Incus rename leaves ownership, name and status untouched.
func TestTransfersAcceptRenameFailureRollsBack(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	fee := "5.00"
	transfersRouteInsertTransfer(t, ctx, 995131, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, &fee)
	transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	previousRename := TransfersRenameInstance
	TransfersRenameInstance = func(ctx context.Context, client *lib.IncusClient, oldName, newName string) error {
		return errors.New("node refused")
	}
	t.Cleanup(func() { TransfersRenameInstance = previousRename })

	status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995131/accept",
		transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusInternalServerError || body["code"] != "INCUS_RENAME_FAILED" {
		t.Fatalf("rename failure = %d %#v", status, body)
	}

	var owner int32
	var transferStatus models.TransferStatus
	if err := db.Pool().QueryRow(ctx,
		`SELECT user_id FROM instances WHERE id = $1`, transfersRouteInstance).Scan(&owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if owner != transfersRouteFromUser {
		t.Fatalf("owner = %d, want the original owner", owner)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = 995131`).Scan(&transferStatus); err != nil {
		t.Fatalf("status: %v", err)
	}
	if transferStatus != models.TransferStatusPending {
		t.Fatalf("status = %s, want pending", transferStatus)
	}
	var billingCount int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM instance_billing_records WHERE instance_id = $1`, transfersRouteInstance).
		Scan(&billingCount); err != nil {
		t.Fatalf("billing: %v", err)
	}
	if billingCount != 0 {
		t.Fatalf("billing rows = %d, want 0", billingCount)
	}
}

// ==================== reject / cancel ====================

// TestTransfersRejectRefundsOnce pins reject + refund + idempotency.
func TestTransfersRejectRefundsOnce(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	fee := "5.00"
	transfersRouteInsertTransfer(t, ctx, 995141, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, &fee)
	transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	// A non-recipient cannot reject.
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995141/reject",
		transfersRouteFromUser, "transfer-route-from", `{"reason":"nope"}`); status != http.StatusForbidden || body["code"] != "FORBIDDEN" {
		t.Fatalf("wrong party reject = %d %#v", status, body)
	}
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995141/reject",
		transfersRouteToUser, "transfer-route-to", `{"reason":"no thanks"}`); status != http.StatusOK || body["message"] != "Transfer rejected" {
		t.Fatalf("reject = %d %#v", status, body)
	}

	balance := transfersRouteBalance(t, ctx, transfersRouteFromUser)
	if balance != 25 {
		t.Fatalf("balance = %v, want 25 (20 + 5 refund)", balance)
	}
	var transferStatus models.TransferStatus
	var rejectReason *string
	if err := db.Pool().QueryRow(ctx,
		`SELECT status, reject_reason FROM instance_transfers WHERE id = 995141`).
		Scan(&transferStatus, &rejectReason); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if transferStatus != models.TransferStatusRejected || rejectReason == nil || *rejectReason != "no thanks" {
		t.Fatalf("transfer = %s reason %v", transferStatus, rejectReason)
	}

	// Second reject: 400 and the balance does not move again.
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995141/reject",
		transfersRouteToUser, "transfer-route-to", `{}`); status != http.StatusBadRequest || body["code"] != "TRANSFER_NOT_PENDING" {
		t.Fatalf("second reject = %d %#v", status, body)
	}
	if balance := transfersRouteBalance(t, ctx, transfersRouteFromUser); balance != 25 {
		t.Fatalf("balance after second reject = %v, want 25", balance)
	}

	// The rejected transfer cannot be accepted either.
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995141/accept",
		transfersRouteToUser, "transfer-route-to", ""); status != http.StatusBadRequest || body["code"] != "TRANSFER_NOT_PENDING" {
		t.Fatalf("accept after reject = %d %#v", status, body)
	}
}

// TestTransfersCancelRefundsAndBlocksAccept pins cancel + refund + the accept gate.
func TestTransfersCancelRefundsAndBlocksAccept(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	fee := "3.00"
	transfersRouteInsertTransfer(t, ctx, 995151, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, &fee)
	transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995151/cancel",
		transfersRouteToUser, "transfer-route-to", ""); status != http.StatusForbidden || body["code"] != "FORBIDDEN" {
		t.Fatalf("recipient cancel = %d %#v", status, body)
	}
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995151/cancel",
		transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusOK || body["message"] != "Transfer cancelled" {
		t.Fatalf("cancel = %d %#v", status, body)
	}
	if balance := transfersRouteBalance(t, ctx, transfersRouteFromUser); balance != 23 {
		t.Fatalf("balance = %v, want 23", balance)
	}
	var transferStatus models.TransferStatus
	if err := db.Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = 995151`).Scan(&transferStatus); err != nil {
		t.Fatalf("status: %v", err)
	}
	if transferStatus != models.TransferStatusCancelled {
		t.Fatalf("status = %s, want cancelled", transferStatus)
	}

	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995151/accept",
		transfersRouteToUser, "transfer-route-to", ""); status != http.StatusBadRequest || body["code"] != "TRANSFER_NOT_PENDING" {
		t.Fatalf("accept after cancel = %d %#v", status, body)
	}
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995151/cancel",
		transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusBadRequest || body["code"] != "TRANSFER_NOT_PENDING" {
		t.Fatalf("second cancel = %d %#v", status, body)
	}
}

// ==================== push ====================

// TestTransfersPushMovesOwnership pins the initiator-only push path.
func TestTransfersPushMovesOwnership(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	transfersRouteInsertTransfer(t, ctx, 995161, transfersRouteInstance, transfersRouteFromUser, transfersRouteThirdUser,
		models.TransferStatusPending, nil)
	renames, _, _, _ := transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	// The recipient cannot push.
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995161/push",
		transfersRouteThirdUser, "transfer-route-third", ""); status != http.StatusForbidden || body["code"] != "FORBIDDEN" {
		t.Fatalf("recipient push = %d %#v", status, body)
	}
	if status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995161/push",
		transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusOK || body["message"] != "Transfer pushed successfully" {
		t.Fatalf("push = %d %#v", status, body)
	}
	if len(*renames) != 1 || (*renames)[0] != "transfer-route-incus-995001->u995003-abcd1234" {
		t.Fatalf("renames = %#v", *renames)
	}
	var owner int32
	var name string
	if err := db.Pool().QueryRow(ctx,
		`SELECT user_id, name FROM instances WHERE id = $1`, transfersRouteInstance).Scan(&owner, &name); err != nil {
		t.Fatalf("instance: %v", err)
	}
	if owner != transfersRouteThirdUser || name != "route-box（transfer-route-from转）" {
		t.Fatalf("instance = owner %d name %q", owner, name)
	}

	// A running instance blocks the push before any Incus call.
	transfersRouteInsertTransfer(t, ctx, 995162, transfersRouteRunning, transfersRouteFromUser, transfersRouteThirdUser,
		models.TransferStatusPending, nil)
	status, body := transfersRouteTestCall(t, engine, http.MethodPost, "/api/transfers/995162/push",
		transfersRouteFromUser, "transfer-route-from", "")
	if status != http.StatusBadRequest || body["code"] != "INSTANCE_MUST_BE_STOPPED" {
		t.Fatalf("running push = %d %#v", status, body)
	}
	if len(*renames) != 1 {
		t.Fatalf("running push renamed: %#v", *renames)
	}
	var transferStatus models.TransferStatus
	if err := db.Pool().QueryRow(ctx,
		`SELECT status FROM instance_transfers WHERE id = 995162`).Scan(&transferStatus); err != nil {
		t.Fatalf("status: %v", err)
	}
	if transferStatus != models.TransferStatusPending {
		t.Fatalf("status = %s, want pending", transferStatus)
	}
}

// ==================== read endpoints ====================

// TestTransfersReadEndpoints pins list/detail/pending-count against the fixtures,
// including the party-only detail rule and the list pagination envelope.
func TestTransfersReadEndpoints(t *testing.T) {
	ctx := transfersRouteTestDatabase(t)
	transfersRouteReset(t, ctx)
	remark := "read me"
	transfersRouteInsertTransfer(t, ctx, 995171, transfersRouteInstance, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusPending, nil)
	if _, err := db.Pool().Exec(ctx,
		`UPDATE instance_transfers SET remark = $1 WHERE id = 995171`, remark); err != nil {
		t.Fatalf("set remark: %v", err)
	}
	transfersRouteInsertTransfer(t, ctx, 995172, transfersRouteRunning, transfersRouteFromUser, transfersRouteToUser,
		models.TransferStatusRejected, nil)
	transfersRouteInsertTransfer(t, ctx, 995173, transfersRouteInstance, transfersRouteFromUser, transfersRouteThirdUser,
		models.TransferStatusAccepted, nil)
	transfersRouteTestSeams(t)
	engine := transfersRouteTestRouter()

	status, body := transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers?type=sent", transfersRouteFromUser, "transfer-route-from", "")
	if status != http.StatusOK {
		t.Fatalf("sent list = %d %#v", status, body)
	}
	if body["total"].(float64) != 3 || body["page"].(float64) != 1 || body["pageSize"].(float64) != 20 || body["totalPages"].(float64) != 1 {
		t.Fatalf("sent envelope = %#v", body)
	}
	items := body["transfers"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %#v", items)
	}
	first := items[0].(map[string]any)
	if first["id"].(float64) != 995173 {
		t.Fatalf("first item = %#v, want the newest transfer 995173", first)
	}
	if first["canPush"] != false {
		t.Fatalf("accepted transfer must not be pushable: %#v", first)
	}
	if first["fromUser"] != nil {
		t.Fatalf("sent list must not include fromUser: %#v", first["fromUser"])
	}
	var pendingItem map[string]any
	for _, entry := range items {
		candidate := entry.(map[string]any)
		if candidate["id"].(float64) == 995171 {
			pendingItem = candidate
		}
	}
	if pendingItem == nil || pendingItem["canPush"] != true || pendingItem["remark"] != remark {
		t.Fatalf("pending item = %#v", pendingItem)
	}

	status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers?type=received", transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusOK {
		t.Fatalf("received list = %d %#v", status, body)
	}
	items = body["transfers"].([]any)
	if len(items) != 2 {
		t.Fatalf("received items = %#v", items)
	}
	if item := items[0].(map[string]any); item["toUser"] != nil || item["fromUser"] == nil {
		t.Fatalf("received item = %#v", item)
	}

	status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/pending-count", transfersRouteToUser, "transfer-route-to", "")
	if status != http.StatusOK || body["count"].(float64) != 1 {
		t.Fatalf("pending count = %d %#v", status, body)
	}

	// Detail: both parties may read, a third user may not, a missing row is 404.
	for _, user := range []struct {
		id   int32
		name string
	}{{transfersRouteFromUser, "transfer-route-from"}, {transfersRouteToUser, "transfer-route-to"}} {
		status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/995171", user.id, user.name, "")
		if status != http.StatusOK || body["transfer"].(map[string]any)["id"].(float64) != 995171 {
			t.Fatalf("detail for %s = %d %#v", user.name, status, body)
		}
	}
	detail := body["transfer"].(map[string]any)
	if detail["fromUser"] == nil || detail["toUser"] == nil || detail["instanceName"] != "route-box" {
		t.Fatalf("detail = %#v", detail)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/995171", transfersRouteThirdUser, "transfer-route-third", ""); status != http.StatusForbidden || body["code"] != "FORBIDDEN" {
		t.Fatalf("third-party detail = %d %#v", status, body)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/995999", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusNotFound || body["code"] != "TRANSFER_NOT_FOUND" {
		t.Fatalf("missing detail = %d %#v", status, body)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/0", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusBadRequest || body["code"] != "INVALID_ID" {
		t.Fatalf("invalid detail id = %d %#v", status, body)
	}

	// The user search answers self/banned/unknown before anything else.
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/users/search?username=transfer-route-to", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusOK || body["user"].(map[string]any)["username"] != "transfer-route-to" {
		t.Fatalf("search = %d %#v", status, body)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/users/search?username=transfer-route-from", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusBadRequest || body["code"] != "TRANSFER_TO_SELF" {
		t.Fatalf("search self = %d %#v", status, body)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/users/search?username=transfer-route-banned", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusBadRequest || body["code"] != "TRANSFER_TO_BANNED" {
		t.Fatalf("search banned = %d %#v", status, body)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/users/search?username=a", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusBadRequest || body["code"] != "INVALID_PARAMS" {
		t.Fatalf("search short = %d %#v", status, body)
	}
	if status, body = transfersRouteTestCall(t, engine, http.MethodGet, "/api/transfers/users/search?username=transfer-route-nobody", transfersRouteFromUser, "transfer-route-from", ""); status != http.StatusNotFound || body["code"] != "USER_NOT_FOUND" {
		t.Fatalf("search missing = %d %#v", status, body)
	}
}

// transfersRouteBalance reads a user's balance.
func transfersRouteBalance(t *testing.T, ctx context.Context, userID int32) float64 {
	t.Helper()
	var balance models.Decimal
	if err := db.Pool().QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("balance: %v", err)
	}
	return balance.InexactFloat64()
}
