package db

import "testing"

// TestCheckinClampPoints pins clampCheckinPoints: non-safe integers fall back to
// the supplied default, everything else is clamped into [1, CHECKIN_MAX_POINTS_LIMIT].
func TestCheckinClampPoints(t *testing.T) {
	cases := []struct {
		name     string
		value    int
		fallback int
		want     int
	}{
		{"zero is clamped up to one", 0, 1, 1},
		{"negative is clamped up to one", -25, 1, 1},
		{"one stays one", 1, 1, 1},
		{"inside range", 250, 1, 250},
		{"limit stays", CheckinMaxPointsLimit, 1, CheckinMaxPointsLimit},
		{"above limit is clamped", CheckinMaxPointsLimit + 1, 1, CheckinMaxPointsLimit},
		{"unsafe integer falls back", checkinMaxSafeInteger + 1, 7, 7},
		{"negative unsafe integer falls back", -checkinMaxSafeInteger - 1, 7, 7},
		{"safe maximum is clamped to the limit", checkinMaxSafeInteger, 1, CheckinMaxPointsLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkinClampPoints(tc.value, tc.fallback); got != tc.want {
				t.Fatalf("clampCheckinPoints(%d, %d) = %d, want %d", tc.value, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestCheckinClampPointsMatchesTypeScriptArithmetic(t *testing.T) {
	// Math.min(Math.max(value, 1), CHECKIN_MAX_POINTS_LIMIT)
	if got := checkinClampPoints(1, CheckinMinPointsDefault); got != 1 {
		t.Fatalf("lower bound = %d, want 1", got)
	}
	if got := checkinClampPoints(CheckinMaxPointsLimit+1, CheckinMaxPointsDefault); got != CheckinMaxPointsLimit {
		t.Fatalf("upper bound = %d, want %d", got, CheckinMaxPointsLimit)
	}
}

// TestCheckinNormalizeSettings pins normalizeCheckinSettings, including the rule
// that maxPoints can never be pushed below minPoints.
func TestCheckinNormalizeSettings(t *testing.T) {
	cases := []struct {
		name string
		in   CheckinDailySettings
		want CheckinDailySettings
	}{
		{
			name: "valid settings pass through",
			in:   CheckinDailySettings{Enabled: true, MinPoints: 5, MaxPoints: 100, RequireInstance: true},
			want: CheckinDailySettings{Enabled: true, MinPoints: 5, MaxPoints: 100, RequireInstance: true},
		},
		{
			// Node quirk worth pinning: the defaults are only used when
			// Number.isSafeInteger fails. 0 *is* a safe integer, so it clamps to 1
			// for both bounds instead of yielding (1, 500).
			name: "zeros clamp to one rather than taking the defaults",
			in:   CheckinDailySettings{Enabled: false, MinPoints: 0, MaxPoints: 0, RequireInstance: false},
			want: CheckinDailySettings{Enabled: false, MinPoints: 1, MaxPoints: 1, RequireInstance: false},
		},
		{
			name: "negative min is clamped to one",
			in:   CheckinDailySettings{Enabled: true, MinPoints: -5, MaxPoints: 10, RequireInstance: false},
			want: CheckinDailySettings{Enabled: true, MinPoints: 1, MaxPoints: 10, RequireInstance: false},
		},
		{
			name: "max below min is raised to min",
			in:   CheckinDailySettings{Enabled: true, MinPoints: 50, MaxPoints: 10, RequireInstance: false},
			want: CheckinDailySettings{Enabled: true, MinPoints: 50, MaxPoints: 50, RequireInstance: false},
		},
		{
			name: "max above the hard limit is clamped",
			in:   CheckinDailySettings{Enabled: true, MinPoints: 1, MaxPoints: 500000, RequireInstance: false},
			want: CheckinDailySettings{Enabled: true, MinPoints: 1, MaxPoints: CheckinMaxPointsLimit, RequireInstance: false},
		},
		{
			name: "min above the hard limit clamps both",
			in:   CheckinDailySettings{Enabled: true, MinPoints: 500000, MaxPoints: 500000, RequireInstance: false},
			want: CheckinDailySettings{Enabled: true, MinPoints: CheckinMaxPointsLimit, MaxPoints: CheckinMaxPointsLimit, RequireInstance: false},
		},
		{
			name: "unsafe integers fall back before capping",
			in:   CheckinDailySettings{Enabled: true, MinPoints: checkinMaxSafeInteger + 1, MaxPoints: checkinMaxSafeInteger + 1, RequireInstance: false},
			want: CheckinDailySettings{Enabled: true, MinPoints: CheckinMinPointsDefault, MaxPoints: CheckinMaxPointsDefault, RequireInstance: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkinNormalizeSettings(tc.in); got != tc.want {
				t.Fatalf("normalizeCheckinSettings(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCheckinDefaultsMatchTypeScript pins the constants used both by the config
// reads and by the public settings payload.
func TestCheckinDefaultsMatchTypeScript(t *testing.T) {
	if CheckinMinPointsDefault != 1 {
		t.Fatalf("CHECKIN_MIN_POINTS_DEFAULT = %d, want 1", CheckinMinPointsDefault)
	}
	if CheckinMaxPointsDefault != 500 {
		t.Fatalf("CHECKIN_MAX_POINTS_DEFAULT = %d, want 500", CheckinMaxPointsDefault)
	}
	if CheckinMaxPointsLimit != 100000 {
		t.Fatalf("CHECKIN_MAX_POINTS_LIMIT = %d, want 100000", CheckinMaxPointsLimit)
	}
	if checkinMaxSafeInteger != 9007199254740991 {
		t.Fatalf("Number.MAX_SAFE_INTEGER = %d, want 9007199254740991", checkinMaxSafeInteger)
	}
}
