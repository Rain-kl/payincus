package db

import (
	"testing"
	"time"
)

// TestSnapshotsAddMinutesLocal pins the JS `d.setMinutes(d.getMinutes() + n)`
// behaviour: wall-clock arithmetic that rolls over days (time.Date normalises DST
// the same way JavaScript's local Date setters do).
func TestSnapshotsAddMinutesLocal(t *testing.T) {
	from := time.Date(2026, 1, 1, 23, 30, 0, 0, time.Local)

	if got, want := snapshotsAddMinutesLocal(from, 0), from; !got.Equal(want) {
		t.Errorf("adding 0 minutes = %v, want %v", got, want)
	}
	if got, want := snapshotsAddMinutesLocal(from, 90), time.Date(2026, 1, 2, 1, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("adding 90 minutes = %v, want %v", got, want)
	}
	if got, want := snapshotsAddMinutesLocal(from, 1440), time.Date(2026, 1, 2, 23, 30, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("adding 1440 minutes = %v, want %v", got, want)
	}
	if got, want := snapshotsAddMinutesLocal(from, 60), time.Date(2026, 1, 2, 0, 30, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("adding 60 minutes = %v, want %v", got, want)
	}
}

// TestSnapshotsQuotaDenialMessage pins the exact quota messages (they surface to
// the client as apiError details).
func TestSnapshotsQuotaDenialMessage(t *testing.T) {
	if got := snapshotsQuotaDenialMessage(0, 0); got != "Snapshot quota not allocated for this instance" {
		t.Errorf("not-allocated message = %q", got)
	}
	if got := snapshotsQuotaDenialMessage(3, 3); got != "Instance snapshot quota full (3/3)" {
		t.Errorf("full message = %q", got)
	}
	if got := snapshotsQuotaDenialMessage(9, 5); got != "Instance snapshot quota full (9/5)" {
		t.Errorf("over-quota message = %q", got)
	}
}
