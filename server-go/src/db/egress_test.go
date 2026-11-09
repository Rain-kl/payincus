package db

import (
	"encoding/json"
	"testing"
)

// TestEgressAdvisoryLockNamespace pins the cross-process contract with the Node
// backend: server/src/lib/egress/* and routes/egress*.ts all take
// pg_advisory_xact_lock(74201). Changing this value silently disables the mutual
// exclusion between the two runtimes during the parallel-run window.
func TestEgressAdvisoryLockNamespace(t *testing.T) {
	if EgressAdvisoryLockNamespace != 74201 {
		t.Fatalf("EgressAdvisoryLockNamespace = %d, want 74201", EgressAdvisoryLockNamespace)
	}
}

func TestEgressInputErrorMessage(t *testing.T) {
	err := newEgressInputError("代理或授权宿主节点不存在")
	if err.Error() != "代理或授权宿主节点不存在" {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestEgressProfilesContainsHost(t *testing.T) {
	hosts := []int32{3, 1}
	if !egressProfilesContainsHost(hosts, 1) {
		t.Fatalf("membership missed")
	}
	if egressProfilesContainsHost(hosts, 2) {
		t.Fatalf("false membership")
	}
	if egressProfilesContainsHost(nil, 1) {
		t.Fatalf("nil slice reported membership")
	}
}

func TestEgressJSONRawValueIsNotAString(t *testing.T) {
	encoded, err := json.Marshal(EgressProfilesEgressJSON([]byte(`{"mode":"off"}`)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `{"mode":"off"}` {
		t.Fatalf("raw json = %s", encoded)
	}
	encoded, err = json.Marshal(EgressProfilesEgressJSON(nil))
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if string(encoded) != "null" {
		t.Fatalf("nil raw json = %s", encoded)
	}
}
