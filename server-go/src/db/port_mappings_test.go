package db

// Pure tests for the port-mapping allocator. They need no database: the boundary
// arithmetic is exactly what decides whether an instance gets a port, and it is the
// part that must match server/src/db/port-mappings.ts value for value.
//
// The DB-backed functions are thin wrappers around these helpers plus one query, so
// the behaviour pinned here is the behaviour of the port pool.

import (
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func portMappingsUsed(ports ...int32) map[int32]struct{} {
	used := make(map[int32]struct{}, len(ports))
	for _, port := range ports {
		used[port] = struct{}{}
	}
	return used
}

// sequenceRNG returns the given values in order, then repeats the last one.
func sequenceRNG(values ...float64) func() float64 {
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

func TestPortMappingsAllocateCandidates(t *testing.T) {
	cases := []struct {
		name     string
		start    int32
		end      int32
		used     map[int32]struct{}
		excluded []int32
		want     []int32
	}{
		// Inclusive bounds: both the first and the last port of the pool.
		{name: "empty pool", start: 20_000, end: 20_003, want: []int32{20_000, 20_001, 20_002, 20_003}},
		{name: "single port", start: 20_005, end: 20_005, want: []int32{20_005}},
		// Occupied ports are skipped, including the range endpoints.
		{name: "used endpoints skipped", start: 20_000, end: 20_003, used: portMappingsUsed(20_000, 20_003), want: []int32{20_001, 20_002}},
		{name: "used middle skipped", start: 20_000, end: 20_003, used: portMappingsUsed(20_002), want: []int32{20_000, 20_001, 20_003}},
		// Exclusions merge with the used set.
		{name: "excluded merged", start: 20_000, end: 20_003, used: portMappingsUsed(20_001), excluded: []int32{20_002}, want: []int32{20_000, 20_003}},
		{name: "excluded outside range ignored", start: 20_000, end: 20_001, excluded: []int32{40_000}, want: []int32{20_000, 20_001}},
		// Exhaustion.
		{name: "pool exhausted", start: 20_000, end: 20_002, used: portMappingsUsed(20_000, 20_001, 20_002), want: []int32{}},
		// A released port becomes a candidate again.
		{name: "released port is reusable", start: 20_000, end: 20_001, used: portMappingsUsed(), excluded: nil, want: []int32{20_000, 20_001}},
		// Falsy / inverted ranges, mirroring `if (!host.nat_port_start || ...)`.
		{name: "zero start", start: 0, end: 20_003, want: []int32{}},
		{name: "zero end", start: 20_000, end: 0, want: []int32{}},
		{name: "inverted range", start: 20_003, end: 20_000, want: []int32{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PortMappingsAllocateCandidates(tc.start, tc.end, tc.used, tc.excluded)
			if len(got) != len(tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
			for index := range got {
				if got[index] != tc.want[index] {
					t.Fatalf("candidates = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPortMappingsRandomIndexClamps(t *testing.T) {
	if got := PortMappingsRandomIndex(0, nil); got != -1 {
		t.Errorf("empty slice index = %d, want -1", got)
	}
	// Math.random() < 1, but a pinned generator can return 1.0; stay inside.
	if got := PortMappingsRandomIndex(4, func() float64 { return 1 }); got != 3 {
		t.Errorf("index(4, 1.0) = %d, want 3", got)
	}
	if got := PortMappingsRandomIndex(4, func() float64 { return -0.5 }); got != 0 {
		t.Errorf("index(4, -0.5) = %d, want 0", got)
	}
	// floor(0.5 * 4) == 2.
	if got := PortMappingsRandomIndex(4, func() float64 { return 0.5 }); got != 2 {
		t.Errorf("index(4, 0.5) = %d, want 2", got)
	}
}

func TestPortMappingsSelectPort(t *testing.T) {
	if _, ok := PortMappingsSelectPort(nil, nil); ok {
		t.Error("an empty candidate list must not yield a port")
	}

	candidates := []int32{20_010, 20_011, 20_012}
	port, ok := PortMappingsSelectPort(candidates, func() float64 { return 0.99 })
	if !ok || port != 20_012 {
		t.Errorf("port = %d, ok = %v; want the last candidate", port, ok)
	}
	port, ok = PortMappingsSelectPort(candidates, func() float64 { return 0 })
	if !ok || port != 20_010 {
		t.Errorf("port = %d, ok = %v; want the first candidate", port, ok)
	}
}

func TestPortMappingsShuffleMatchesTheTypeScriptLoop(t *testing.T) {
	// A generator that always returns 0 makes every iteration swap with index 0, so
	// the expected result documents both the descending direction and the swap order.
	values := []int32{1, 2, 3, 4}
	PortMappingsShuffle(values, func() float64 { return 0 })
	want := []int32{2, 3, 4, 1}
	for index := range values {
		if values[index] != want[index] {
			t.Fatalf("shuffle = %v, want %v", values, want)
		}
	}
}

func TestPortMappingsAllocatePortsFromCandidates(t *testing.T) {
	all := []int32{20_000, 20_001, 20_002, 20_003, 20_004}

	if got := PortMappingsAllocatePortsFromCandidates(all, 0, nil); len(got) != 0 {
		t.Errorf("count 0 = %v, want empty", got)
	}
	if got := PortMappingsAllocatePortsFromCandidates(all, -3, nil); len(got) != 0 {
		t.Errorf("negative count = %v, want empty", got)
	}

	// Fewer candidates than requested: everything is returned.
	got := PortMappingsAllocatePortsFromCandidates(all[:2], 5, func() float64 { return 0 })
	if len(got) != 2 {
		t.Fatalf("insufficient pool = %v, want both candidates", got)
	}

	// Exact match: everything, still shuffled.
	got = PortMappingsAllocatePortsFromCandidates(all[:3], 3, func() float64 { return 0.999 })
	if len(got) != 3 {
		t.Fatalf("exact count = %v, want 3", got)
	}

	// More candidates than requested: count entries, all distinct and all in range.
	got = PortMappingsAllocatePortsFromCandidates(all, 3, sequenceRNG(0.1, 0.5, 0.9, 0.2))
	if len(got) != 3 {
		t.Fatalf("count 3 = %v, want 3 entries", got)
	}
	seen := map[int32]bool{}
	for _, port := range got {
		if port < 20_000 || port > 20_004 {
			t.Errorf("allocated port %d is outside the pool", port)
		}
		if seen[port] {
			t.Errorf("allocated port %d twice", port)
		}
		seen[port] = true
	}

	// The input slice is not mutated (the TypeScript copies before shuffling).
	if all[0] != 20_000 || all[4] != 20_004 {
		t.Errorf("input slice was mutated: %v", all)
	}
}

func TestPortMappingsSuggestFromCandidates(t *testing.T) {
	ascending := []int32{20_000, 20_001, 20_002, 20_003}

	// `availablePorts.length <= count` returns everything it has (the <= is upstream).
	got := PortMappingsSuggestFromCandidates(ascending, 4)
	if len(got) != 4 {
		t.Fatalf("exact count = %v, want all", got)
	}
	got = PortMappingsSuggestFromCandidates(ascending, 2)
	if len(got) != 2 || got[0] != 20_000 || got[1] != 20_001 {
		t.Fatalf("count 2 = %v; suggestions must stay ascending", got)
	}
	got = PortMappingsSuggestFromCandidates(ascending, 9)
	if len(got) != 4 {
		t.Fatalf("oversized count = %v, want all", got)
	}
	if got := PortMappingsSuggestFromCandidates(nil, 3); len(got) != 0 {
		t.Errorf("empty pool = %v, want empty", got)
	}
}

func TestPortMappingISOStringMatchesToISOString(t *testing.T) {
	// JavaScript's toISOString always emits UTC with exactly three fractional digits.
	instant := time.Date(2026, 11, 10, 8, 30, 0, 123_000_000, time.UTC)
	if got := portMappingISOString(instant); got != "2026-11-10T08:30:00.123Z" {
		t.Errorf("iso = %q", got)
	}
	// A non-UTC location is normalised, not printed with an offset.
	shanghai := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2026, 11, 10, 16, 30, 0, 123_000_000, shanghai)
	if got := portMappingISOString(local); got != "2026-11-10T08:30:00.123Z" {
		t.Errorf("iso for a UTC+8 time = %q, want the UTC wall clock", got)
	}
	// Whole seconds still carry .000.
	whole := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := portMappingISOString(whole); got != "2026-01-02T03:04:05.000Z" {
		t.Errorf("iso = %q", got)
	}
}

func TestPortMappingRemarkNormalisation(t *testing.T) {
	// Prisma gets `remark || null`: nil and "" both become NULL.
	if got := portMappingRemark(nil); got != nil {
		t.Errorf("nil remark = %v, want nil", *got)
	}
	empty := ""
	if got := portMappingRemark(&empty); got != nil {
		t.Errorf("empty remark = %v, want nil", *got)
	}
	// Whitespace is NOT trimmed here (the TypeScript keeps it; the caller trims).
	spaced := "  keep me  "
	if got := portMappingRemark(&spaced); got == nil || *got != spaced {
		t.Errorf("remark = %v, want %q", got, spaced)
	}
}

func TestPortMappingHostPortRange(t *testing.T) {
	zero, negative := int32(0), int32(-1)
	start, end := int32(20_000), int32(20_099)

	if _, _, ok := portMappingHostPortRange(nil); ok {
		t.Error("a missing host has no range")
	}
	if _, _, ok := portMappingHostPortRange(&HostsRecord{}); ok {
		t.Error("unset columns mean no pool")
	}
	if _, _, ok := portMappingHostPortRange(&HostsRecord{NATPortStart: &zero, NATPortEnd: &end}); ok {
		t.Error("nat_port_start = 0 means no pool")
	}
	if _, _, ok := portMappingHostPortRange(&HostsRecord{NATPortStart: &start, NATPortEnd: &zero}); ok {
		t.Error("nat_port_end = 0 means no pool")
	}
	// The TS guard only rejects falsy bounds; an inverted range passes it and then
	// produces no candidates (see TestPortMappingsAllocateCandidates).
	if invStart, invEnd, ok := portMappingHostPortRange(&HostsRecord{NATPortStart: &start, NATPortEnd: &negative}); !ok || invStart != start || invEnd != negative {
		t.Errorf("inverted range = %d-%d, ok = %v", invStart, invEnd, ok)
	}
	if got := PortMappingsAllocateCandidates(start, negative, nil, nil); len(got) != 0 {
		t.Errorf("inverted range candidates = %v, want empty", got)
	}
	gotStart, gotEnd, ok := portMappingHostPortRange(&HostsRecord{NATPortStart: &start, NATPortEnd: &end})
	if !ok || gotStart != start || gotEnd != end {
		t.Errorf("range = %d-%d, ok = %v", gotStart, gotEnd, ok)
	}
}

func TestPortMappingProtocolsAreTheModelConstants(t *testing.T) {
	// The column is the PostgreSQL enum "Protocol"; both members must be reachable
	// through models or the allocator would query the wrong pool.
	if models.ProtocolTcp != "tcp" || models.ProtocolUdp != "udp" {
		t.Fatalf("protocol constants changed: %q %q", models.ProtocolTcp, models.ProtocolUdp)
	}
}
