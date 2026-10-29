// Port of server/src/lib/timezone.ts.
//
// The TypeScript version leans on Intl.DateTimeFormat. Go's time.LoadLocation
// uses the IANA tzdata directly, so the helpers below are simpler while keeping
// the same observable contract: "today"/"this month" are evaluated in
// Asia/Shanghai and the returned instants are UTC.
//
// Business rule: "今日" statistics roll over at 00:00 China time, "本月" at the
// 1st 00:00 China time.
package lib

import (
	"time"

	// Embed the IANA database so the binary does not depend on the host's
	// /usr/share/zoneinfo (the production image is minimal).
	_ "time/tzdata"
)

// BusinessTimezone mirrors BUSINESS_TIMEZONE / DEFAULT_TIMEZONE.
const BusinessTimezone = "Asia/Shanghai"

// loadBusinessLocation returns the configured location. The timezone parsing in
// the TS original would throw on an unknown zone; there is no configuration
// surface for it in Go, so a failure here is a build/host bug and the fallback
// keeps callers total.
func loadBusinessLocation(timezone string) *time.Location {
	if timezone == "" {
		timezone = BusinessTimezone
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.UTC
	}
	return location
}

// GetDateStringInTimezone mirrors getDateStringInTimezone: YYYY-MM-DD in the
// given zone. en-CA formats as YYYY-MM-DD, which is what the original relies on.
func GetDateStringInTimezone(moment time.Time, timezone string) string {
	return moment.In(loadBusinessLocation(timezone)).Format("2006-01-02")
}

// GetMonthStringInTimezone mirrors getMonthStringInTimezone: YYYY-MM in the
// given zone.
func GetMonthStringInTimezone(moment time.Time, timezone string) string {
	return moment.In(loadBusinessLocation(timezone)).Format("2006-01")
}

// GetTodayRange mirrors getTodayRange: [today 00:00 in zone, tomorrow 00:00 in
// zone) as UTC instants.
//
// The TS version computes a fixed minute offset and then subtracts it, which
// silently assumes the offset does not change between the two boundaries. Using
// time.Date in the zone is exact and additionally handles DST zones correctly
// (Asia/Shanghai has none, so both agree there).
func GetTodayRange(now time.Time, timezone string) (start time.Time, end time.Time) {
	location := loadBusinessLocation(timezone)
	local := now.In(location)
	start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).UTC()
	return start, start.AddDate(0, 0, 1)
}

// GetThisMonthStart mirrors getThisMonthStart: the 1st of the current month at
// 00:00 in the zone, as a UTC instant.
func GetThisMonthStart(now time.Time, timezone string) time.Time {
	location := loadBusinessLocation(timezone)
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location).UTC()
}

// GetLastMonthRange mirrors getLastMonthRange: [first day of previous month,
// this month's start) in the zone. `end` is exclusive here; the TS version
// returns `end = thisMonthStart - 1ms` for a Prisma `lt` filter. Callers that
// need the inclusive form should subtract a millisecond themselves so the intent
// stays visible at the call site.
func GetLastMonthRange(now time.Time, timezone string) (start time.Time, end time.Time) {
	thisMonthStart := GetThisMonthStart(now, timezone)
	return thisMonthStart.AddDate(0, -1, 0), thisMonthStart
}

// TimezoneOffsetMinutes reproduces getTimezoneOffsetMinutes for callers that
// need the raw offset (positive = east of UTC). Asia/Shanghai is +480.
func TimezoneOffsetMinutes(moment time.Time, timezone string) int {
	location := loadBusinessLocation(timezone)
	_, offsetSeconds := moment.In(location).Zone()
	return offsetSeconds / 60
}

// ParseBusinessDate parses a YYYY-MM-DD string as midnight in the business zone.
// Used by the statistics modules, which round-trip the strings produced by
// GetDateStringInTimezone.
func ParseBusinessDate(value string) (time.Time, bool) {
	parsed, err := time.ParseInLocation("2006-01-02", value, loadBusinessLocation(BusinessTimezone))
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// FormatBusinessDate is the inverse of ParseBusinessDate for logging/debug use.
func FormatBusinessDate(moment time.Time) string {
	return moment.In(loadBusinessLocation(BusinessTimezone)).Format("2006-01-02")
}
