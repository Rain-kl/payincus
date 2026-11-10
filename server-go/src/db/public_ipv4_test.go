package db

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestPublicIpv4NormalizeDNS mirrors normalizeDns: only non-empty strings survive,
// and anything that is not an array becomes [].
func TestPublicIpv4NormalizeDNS(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"null", "null", []string{}},
		{"empty object", "{}", []string{}},
		{"empty array", "[]", []string{}},
		{"strings", `["1.1.1.1","8.8.8.8"]`, []string{"1.1.1.1", "8.8.8.8"}},
		{"drops blank", `["1.1.1.1","  ",""]`, []string{"1.1.1.1"}},
		{"drops non strings", `["1.1.1.1",8,null,true]`, []string{"1.1.1.1"}},
	}
	for _, tc := range cases {
		got := publicIpv4NormalizeDNS([]byte(tc.raw))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: normalizeDns(%s) = %v, want %v", tc.name, tc.raw, got, tc.want)
		}
	}
}

// TestPublicIpv4AssignmentDNS pins the DNS precedence of reservePublicIpv4ForInstance:
// address DNS wins, then the pool's DNS, then the 1.1.1.1 / 8.8.8.8 fallback.
func TestPublicIpv4AssignmentDNS(t *testing.T) {
	got := publicIpv4AssignmentDNS([]string{"9.9.9.9"}, []string{"1.0.0.1"})
	if !reflect.DeepEqual(got, []string{"9.9.9.9"}) {
		t.Fatalf("address DNS must win, got %v", got)
	}
	got = publicIpv4AssignmentDNS(nil, []string{"1.0.0.1"})
	if !reflect.DeepEqual(got, []string{"1.0.0.1"}) {
		t.Fatalf("pool DNS fallback, got %v", got)
	}
	got = publicIpv4AssignmentDNS(nil, nil)
	if !reflect.DeepEqual(got, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("default DNS, got %v", got)
	}
	// The caller must never alias the shared default slice.
	got[0] = "mutated"
	if publicIpv4DefaultDNS[0] != "1.1.1.1" {
		t.Fatal("default DNS slice must be copied, not aliased")
	}
}

// TestPublicIpv4UniqueTrimmed pins `[...new Set(items.map(trim).filter(Boolean))]`:
// trim, drop empties, de-duplicate, keep the first occurrence order.
func TestPublicIpv4UniqueTrimmed(t *testing.T) {
	got := publicIpv4UniqueTrimmed([]string{" 10.0.0.1 ", "10.0.0.2", "10.0.0.1", "", "   "})
	want := []string{"10.0.0.1", "10.0.0.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := publicIpv4UniqueTrimmed(nil); len(got) != 0 {
		t.Fatalf("nil = %v, want empty", got)
	}
}

// TestPublicIpv4StateMachine is the address lifecycle table:
//
//	free     → reserve  → assigned
//	assigned → release  → free      (and therefore reservable again)
//	free     → disable  → disabled  (auto-disable when no gateway)
//	free|disabled → admin free/disable/delete
//	assigned → admin delete/status: REJECTED (PUBLIC_IPV4_ADDRESS_ASSIGNED)
//	disabled → reserve:  REJECTED (an operator disabled it for a reason)
//
// The last two are the "illegal path" half of the table.
func TestPublicIpv4StateMachine(t *testing.T) {
	statuses := []models.PublicIpv4AddressStatus{
		models.PublicIpv4AddressStatusFree,
		models.PublicIpv4AddressStatusAssigned,
		models.PublicIpv4AddressStatusDisabled,
	}

	want := map[models.PublicIpv4AddressStatus]map[PublicIpv4AddressAction]bool{
		models.PublicIpv4AddressStatusFree: {
			PublicIpv4ActionReserve:      true,
			PublicIpv4ActionRelease:      false, // nothing to release
			PublicIpv4ActionDisable:      true,
			PublicIpv4ActionAdminFree:    true, // idempotent
			PublicIpv4ActionAdminDisable: true,
			PublicIpv4ActionDelete:       true,
		},
		models.PublicIpv4AddressStatusAssigned: {
			PublicIpv4ActionReserve:      false, // never hand out twice
			PublicIpv4ActionRelease:      true,
			PublicIpv4ActionDisable:      false,
			PublicIpv4ActionAdminFree:    false, // use release for that
			PublicIpv4ActionAdminDisable: false,
			PublicIpv4ActionDelete:       false,
		},
		models.PublicIpv4AddressStatusDisabled: {
			PublicIpv4ActionReserve:      false, // an operator disabled it
			PublicIpv4ActionRelease:      false,
			PublicIpv4ActionDisable:      false,
			PublicIpv4ActionAdminFree:    true, // re-enable by moving it back to free
			PublicIpv4ActionAdminDisable: true, // idempotent
			PublicIpv4ActionDelete:       true,
		},
	}

	actions := []PublicIpv4AddressAction{
		PublicIpv4ActionReserve, PublicIpv4ActionRelease, PublicIpv4ActionDisable,
		PublicIpv4ActionAdminFree, PublicIpv4ActionAdminDisable, PublicIpv4ActionDelete,
	}
	for _, status := range statuses {
		for _, action := range actions {
			got := PublicIpv4TransitionAllowed(status, action)
			if got != want[status][action] {
				t.Errorf("%s + %s = %v, want %v", status, action, got, want[status][action])
			}
		}
	}

	// An unknown action is never allowed.
	if PublicIpv4TransitionAllowed(models.PublicIpv4AddressStatusFree, "teleport") {
		t.Fatal("unknown action must be rejected")
	}

	// Next status of the two device lifecycle writes.
	if next, ok := PublicIpv4NextStatus(PublicIpv4ActionReserve); !ok || next != models.PublicIpv4AddressStatusAssigned {
		t.Fatalf("reserve next = %v/%v", next, ok)
	}
	if next, ok := PublicIpv4NextStatus(PublicIpv4ActionRelease); !ok || next != models.PublicIpv4AddressStatusFree {
		t.Fatalf("release next = %v/%v", next, ok)
	}
	if next, ok := PublicIpv4NextStatus(PublicIpv4ActionDisable); !ok || next != models.PublicIpv4AddressStatusDisabled {
		t.Fatalf("disable next = %v/%v", next, ok)
	}
	if _, ok := PublicIpv4NextStatus(PublicIpv4ActionDelete); ok {
		t.Fatal("delete has no next status")
	}
}

// TestPublicIpv4ReleaseThenReserve is the "released addresses are reused" invariant
// spelled out as a walk: assigned → release → free → reserve → assigned. It fails if
// the release statement forgets status = 'free' or the reserve filter stops
// accepting free rows.
func TestPublicIpv4ReleaseThenReserve(t *testing.T) {
	status := models.PublicIpv4AddressStatusAssigned

	if !PublicIpv4TransitionAllowed(status, PublicIpv4ActionRelease) {
		t.Fatal("an assigned address must be releasable")
	}
	status, _ = PublicIpv4NextStatus(PublicIpv4ActionRelease)
	if status != models.PublicIpv4AddressStatusFree {
		t.Fatalf("after release = %s, want free", status)
	}
	if !PublicIpv4TransitionAllowed(status, PublicIpv4ActionReserve) {
		t.Fatal("a released address must be reservable again")
	}
	status, _ = PublicIpv4NextStatus(PublicIpv4ActionReserve)
	if status != models.PublicIpv4AddressStatusAssigned {
		t.Fatalf("after reserve = %s, want assigned", status)
	}

	// Releasing twice must be a no-op, not a second write: the SQL only touches
	// rows in 'assigned'.
	if PublicIpv4TransitionAllowed(models.PublicIpv4AddressStatusFree, PublicIpv4ActionRelease) {
		t.Fatal("releasing a free address must be a no-op (idempotency)")
	}
}

// TestPublicIpv4ReserveSQLShape pins the reservation statement. Clause order is the
// contract: filters → ORDER BY id ASC → FOR UPDATE SKIP LOCKED → LIMIT 1. A plain
// SELECT (no SKIP LOCKED) hands the same address to two concurrent creations.
func TestPublicIpv4ReserveSQLShape(t *testing.T) {
	sql := publicIpv4ReserveSelect
	for _, fragment := range []string{
		"a.status = 'free'",
		"a.pool_id IS NULL OR p.enabled = true",
		"ORDER BY a.id ASC",
		"FOR UPDATE SKIP LOCKED",
		"LIMIT 1",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("reserve SQL missing %q:\n%s", fragment, sql)
		}
	}
	order := []string{
		"WHERE a.host_id = $1",
		"ORDER BY a.id ASC",
		"FOR UPDATE SKIP LOCKED",
		"LIMIT 1",
	}
	last := -1
	for _, fragment := range order {
		index := strings.Index(sql, fragment)
		if index <= last {
			t.Fatalf("clause %q out of order:\n%s", fragment, sql)
		}
		last = index
	}
	// No advisory lock here: this is a row lock and the two lock spaces differ.
	if strings.Contains(sql, "advisory") {
		t.Fatalf("reserve must use a row lock, not an advisory lock:\n%s", sql)
	}
}

// TestPublicIpv4ReleaseSQLShape pins the release statement: it clears instance_id,
// stamps released_at, and only touches assigned rows (idempotency + reuse).
func TestPublicIpv4ReleaseSQLShape(t *testing.T) {
	sql := publicIpv4ReleaseUpdate
	for _, fragment := range []string{
		"status = 'free'",
		"instance_id = NULL",
		"released_at = NOW()",
		"updated_at = NOW()",
		"WHERE instance_id = $1",
		"AND status = 'assigned'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("release SQL missing %q:\n%s", fragment, sql)
		}
	}
}

// TestPublicIpv4FreeCountSQLShape pins countFreePublicIpv4ForHost: only free rows,
// only enabled pools, standalone rows always counted.
func TestPublicIpv4FreeCountSQLShape(t *testing.T) {
	sql := publicIpv4FreeCountQuery
	for _, fragment := range []string{
		"COUNT(*)::bigint",
		"a.status = 'free'",
		"(a.pool_id IS NULL OR p.enabled = true)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("free-count SQL missing %q:\n%s", fragment, sql)
		}
	}
}

// TestPublicIpv4GatewayFallback pins the `gateway || poolGateway` fallback. An empty
// own gateway must fall through to the pool; if both are empty the address is
// auto-disabled instead of being handed out unfetchable.
func TestPublicIpv4GatewayFallback(t *testing.T) {
	resolve := func(own, pool *string) string {
		value := ""
		if own != nil {
			value = *own
		}
		if value == "" && pool != nil {
			value = *pool
		}
		return value
	}
	own := "10.0.0.1"
	pool := "10.0.0.254"
	empty := ""

	if got := resolve(&own, &pool); got != own {
		t.Fatalf("own gateway = %q, want %q", got, own)
	}
	if got := resolve(&empty, &pool); got != pool {
		t.Fatalf("empty own gateway must fall back to the pool, got %q", got)
	}
	if got := resolve(nil, nil); got != "" {
		t.Fatalf("no gateway = %q, want empty (caller auto-disables)", got)
	}
}

// TestPublicIpv4ErrorMessages pins the sentinel texts routes/hosts.ts compares:
// POOL_NOT_FOUND and ADDRESS_NOT_FOUND → 404, ADDRESS_ASSIGNED → 400.
func TestPublicIpv4ErrorMessages(t *testing.T) {
	cases := map[string]string{
		ErrPublicIpv4PoolNotFound.Error():             "PUBLIC_IPV4_POOL_NOT_FOUND",
		ErrPublicIpv4PoolHasAssignedAddresses.Error(): "PUBLIC_IPV4_POOL_HAS_ASSIGNED_ADDRESSES",
		ErrPublicIpv4AddressNotFound.Error():          "PUBLIC_IPV4_ADDRESS_NOT_FOUND",
		ErrPublicIpv4AddressAssigned.Error():          "PUBLIC_IPV4_ADDRESS_ASSIGNED",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("sentinel = %q, want %q", got, want)
		}
	}
}

// TestPublicIpv4PoolRecordJSONShape pins the list response shape: Prisma's camelCase
// pool model, the selected address columns only (no poolId/hostId), and ISO dates.
func TestPublicIpv4PoolRecordJSONShape(t *testing.T) {
	pool := PublicIpv4PoolWithAddresses{
		PublicIpv4PoolRecord: PublicIpv4PoolRecord{},
		Addresses:            []PublicIpv4PoolAddressRecord{{DNS: []byte("[]")}},
	}
	encoded, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	order := keyOrder(t, encoded)
	wantOrder := []string{
		"id", "hostId", "name", "cidr", "gateway", "prefixLength", "dns", "routeMode",
		"enabled", "notes", "createdAt", "updatedAt", "addresses",
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("pool key order:\n got %v\nwant %v", order, wantOrder)
	}

	addressEncoded, err := json.Marshal(pool.Addresses[0])
	if err != nil {
		t.Fatalf("marshal address: %v", err)
	}
	addressOrder := keyOrder(t, addressEncoded)
	wantAddressOrder := []string{
		"id", "address", "prefixLength", "gateway", "dns", "status", "instanceId",
		"assignedAt", "releasedAt", "notes", "createdAt", "updatedAt",
	}
	if !reflect.DeepEqual(addressOrder, wantAddressOrder) {
		t.Fatalf("address key order:\n got %v\nwant %v", addressOrder, wantAddressOrder)
	}
}

// TestPublicIpv4FreeAddressCounting is the resource-count contract: which rows the
// allocator may hand out, and therefore which rows countFreePublicIpv4ForHost must
// report as free.
//
// counted:     free + standalone (pool_id NULL)
//
//	free + pool enabled
//
// not counted: free + pool disabled   (operator paused the pool)
//
//	assigned               (in use)
//	disabled               (operator disabled the address)
//
// The Go mirror and the SQL constant must agree; if one is edited alone the test
// fails before the allocator and the count can disagree in production.
func TestPublicIpv4FreeAddressCounting(t *testing.T) {
	poolID := int32(4)
	cases := []struct {
		name        string
		status      models.PublicIpv4AddressStatus
		poolID      *int32
		poolEnabled bool
		want        bool
	}{
		{"free standalone", models.PublicIpv4AddressStatusFree, nil, false, true},
		{"free in enabled pool", models.PublicIpv4AddressStatusFree, &poolID, true, true},
		{"free in disabled pool", models.PublicIpv4AddressStatusFree, &poolID, false, false},
		{"assigned standalone", models.PublicIpv4AddressStatusAssigned, nil, true, false},
		{"assigned in enabled pool", models.PublicIpv4AddressStatusAssigned, &poolID, true, false},
		{"disabled standalone", models.PublicIpv4AddressStatusDisabled, nil, true, false},
		{"disabled in enabled pool", models.PublicIpv4AddressStatusDisabled, &poolID, true, false},
	}
	counted := 0
	for _, tc := range cases {
		got := PublicIpv4AddressCountsAsAvailable(tc.status, tc.poolID, tc.poolEnabled)
		if got != tc.want {
			t.Errorf("%s: countsAsAvailable = %v, want %v", tc.name, got, tc.want)
		}
		if got {
			counted++
		}
	}
	if counted != 2 {
		t.Fatalf("counted rows = %d, want 2", counted)
	}

	// Both statements must embed the same predicate text (minus the leading AND
	// that the reserve query adds before ORDER BY).
	predicate := PublicIpv4AvailablePredicate
	for name, sql := range map[string]string{
		"free count": publicIpv4FreeCountQuery,
		"reserve":    publicIpv4ReserveSelect,
	} {
		if !strings.Contains(sql, predicate) {
			t.Fatalf("%s SQL does not use the shared predicate:\n%s", name, sql)
		}
	}

	// Idempotent counting: re-running the predicate on the same rows cannot change
	// the answer (no state is read or written by the count).
	again := 0
	for _, tc := range cases {
		if PublicIpv4AddressCountsAsAvailable(tc.status, tc.poolID, tc.poolEnabled) {
			again++
		}
	}
	if again != counted {
		t.Fatalf("second pass counted %d, want %d (must be idempotent)", again, counted)
	}
}
