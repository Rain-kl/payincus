// Port of the daily-checkin settings reader from server/src/db/checkin.ts.
//
// Only getDailyCheckinSettings (plus its defaults and normaliser) lives here.
// The rest of checkin.ts — performCheckin, checkin records, stats, admin logs —
// belongs to the checkin module port and is expected to extend this file, so
// every package-level identifier is prefixed with `Checkin`/`checkin` to stay
// collision-free in the flat db package.
package db

import "context"

// The checkin settings constants mirror checkin.ts. They are exported because the
// settings contract (defaults + hard limit) is shared with the future checkin
// routes, which return minPoints/maxPoints to the client.
const (
	// CheckinMinPointsDefault mirrors CHECKIN_MIN_POINTS_DEFAULT.
	CheckinMinPointsDefault = 1
	// CheckinMaxPointsDefault mirrors CHECKIN_MAX_POINTS_DEFAULT.
	CheckinMaxPointsDefault = 500
	// CheckinMaxPointsLimit mirrors CHECKIN_MAX_POINTS_LIMIT.
	CheckinMaxPointsLimit = 100_000
)

// checkinMaxSafeInteger mirrors Number.MAX_SAFE_INTEGER (2^53-1). checkin.ts uses
// Number.isSafeInteger, which rejects anything outside this range.
const checkinMaxSafeInteger = 9007199254740991

// CheckinDailySettings mirrors DailyCheckinSettings. It is also part of the admin
// statistics payload (marketing.checkin), so the JSON names match the TS object.
type CheckinDailySettings struct {
	Enabled         bool `json:"enabled"`
	MinPoints       int  `json:"minPoints"`
	MaxPoints       int  `json:"maxPoints"`
	RequireInstance bool `json:"requireInstance"`
}

// checkinIsSafeInteger mirrors Number.isSafeInteger.
func checkinIsSafeInteger(value int) bool {
	return value >= -checkinMaxSafeInteger && value <= checkinMaxSafeInteger
}

// checkinClampPoints mirrors clampCheckinPoints: unsafe integers fall back to the
// default, everything else is clamped into [1, CHECKIN_MAX_POINTS_LIMIT].
func checkinClampPoints(value, fallback int) int {
	if !checkinIsSafeInteger(value) {
		return fallback
	}
	if value < 1 {
		return 1
	}
	if value > CheckinMaxPointsLimit {
		return CheckinMaxPointsLimit
	}
	return value
}

// checkinNormalizeSettings mirrors normalizeCheckinSettings: the max is never
// allowed below the min.
func checkinNormalizeSettings(settings CheckinDailySettings) CheckinDailySettings {
	minPoints := checkinClampPoints(settings.MinPoints, CheckinMinPointsDefault)
	maxPoints := checkinClampPoints(settings.MaxPoints, CheckinMaxPointsDefault)
	if maxPoints < minPoints {
		maxPoints = minPoints
	}
	return CheckinDailySettings{
		Enabled:         settings.Enabled,
		MinPoints:       minPoints,
		MaxPoints:       maxPoints,
		RequireInstance: settings.RequireInstance,
	}
}

// CheckinGetDailyCheckinSettings mirrors getDailyCheckinSettings.
//
// The TS version issues its four config reads through Promise.all; go's config
// getter is cache-backed and per-key, so sequential reads observe exactly the
// same values. Any read error fails the whole call, like Promise.all rejecting.
func CheckinGetDailyCheckinSettings(ctx context.Context) (CheckinDailySettings, error) {
	enabled, err := GetSystemConfigBoolean(ctx, "checkin_enabled", true)
	if err != nil {
		return CheckinDailySettings{}, err
	}
	minPoints, err := GetSystemConfigNumber(ctx, "checkin_min_points", CheckinMinPointsDefault)
	if err != nil {
		return CheckinDailySettings{}, err
	}
	maxPoints, err := GetSystemConfigNumber(ctx, "checkin_max_points", CheckinMaxPointsDefault)
	if err != nil {
		return CheckinDailySettings{}, err
	}
	requireInstance, err := GetSystemConfigBoolean(ctx, "checkin_require_instance", false)
	if err != nil {
		return CheckinDailySettings{}, err
	}

	return checkinNormalizeSettings(CheckinDailySettings{
		Enabled:         enabled,
		MinPoints:       minPoints,
		MaxPoints:       maxPoints,
		RequireInstance: requireInstance,
	}), nil
}
