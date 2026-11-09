package db

import (
	"reflect"
	"testing"
)

func TestEgressProfilesUniqueSorted(t *testing.T) {
	got := egressProfilesUniqueSorted([]int32{5, 2, 5, 1, 2})
	want := []int32{1, 2, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uniqueSorted = %v, want %v", got, want)
	}
	// `[...new Set([])]` is an empty array, not nil.
	empty := egressProfilesUniqueSorted(nil)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %#v", empty)
	}
	// The union used for host targets keeps newly authorized and revoked hosts.
	targets := egressProfilesUniqueSorted(append(append([]int32{}, 9, 4), 7, 4, 2))
	if !reflect.DeepEqual(targets, []int32{2, 4, 7, 9}) {
		t.Fatalf("union = %v", targets)
	}
}

func TestEgressProfilesEgressJSON(t *testing.T) {
	if string(EgressProfilesEgressJSON([]byte(`{"mode":"proxy"}`))) != `{"mode":"proxy"}` {
		t.Fatalf("raw value changed")
	}
	if string(EgressProfilesEgressJSON(nil)) != "null" {
		t.Fatalf("nil value = %q", EgressProfilesEgressJSON(nil))
	}
}
