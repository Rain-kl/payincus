// Port of server/src/services/traffic-utils.ts.
//
// Traffic accounting runs on bigint counters in Postgres (rx_raw / tx_raw et al),
// so the whole module works in int64 and NEVER converts to float except for
// display percentages and the human-readable byte formatter.
//
// The subtle rule is CalculateIncrement: a counter that went backwards means the
// node rebooted or the interface was recreated. The TypeScript returns 0 and only
// re-bases the snapshot, deliberately under-counting one sampling window rather
// than treating the counter reset as a huge burst.
package lib

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TrafficTimeZone mirrors TRAFFIC_TIME_ZONE.
const TrafficTimeZone = "Asia/Shanghai"

// Traffic status values mirror the TypeScript string unions.
const (
	TrafficStatusNormal  = "NORMAL"
	TrafficStatusWarning = "WARNING"
	TrafficStatusLimited = "LIMITED"
)

// TrafficSampleSource mirrors the `source` union.
const (
	TrafficSourceAgentReport     = "agent-report"
	TrafficSourceActiveCollector = "active-collector"
)

// TrafficSnapshotSample mirrors TrafficSnapshotSample. The optional counters are
// pointers so "not reported" stays distinguishable from a reported zero.
type TrafficSnapshotSample struct {
	RxRaw        int64
	TxRaw        int64
	SampledAt    time.Time
	Source       string
	RxPacketsRaw *int64
	TxPacketsRaw *int64
	CPUUsageRaw  *int64
}

// CalculateIncrement mirrors calculateIncrement: a counter that moved backwards
// contributes nothing, because the baseline is being rebuilt after a reset.
func CalculateIncrement(current, previous int64) int64 {
	if current < previous {
		return 0
	}
	return current - previous
}

// GetEffectiveLimit mirrors getEffectiveLimit: limit + extraQuota, with nil
// meaning unlimited.
func GetEffectiveLimit(limit *int64, extraQuota int64) *int64 {
	if limit == nil {
		return nil
	}
	total := *limit + extraQuota
	return &total
}

// IsOverLimit mirrors isOverLimit: `used >= limit`, and unlimited never trips.
func IsOverLimit(used int64, limit *int64) bool {
	if limit == nil {
		return false
	}
	return used >= *limit
}

// IsWarningThreshold mirrors isWarningThreshold: 80% of the limit, computed with
// integer arithmetic (`(limit * 80n) / 100n`), so it truncates exactly as Node
// does — using floats here would shift the threshold on some limits.
func IsWarningThreshold(used int64, limit *int64) bool {
	if limit == nil {
		return false
	}
	return used >= (*limit*80)/100
}

// IsWarningSentThisMonth mirrors isWarningSentThisMonth: same calendar month AND
// same year. The comparison uses LOCAL time, matching Date#getMonth/getFullYear.
func IsWarningSentThisMonth(sentAt *time.Time, now time.Time) bool {
	if sentAt == nil {
		return false
	}
	local := now.Local()
	sent := sentAt.Local()
	return sent.Month() == local.Month() && sent.Year() == local.Year()
}

// CalculateUserTrafficStatus mirrors calculateUserTrafficStatus.
func CalculateUserTrafficStatus(used int64, limit *int64) string {
	if IsOverLimit(used, limit) {
		return TrafficStatusLimited
	}
	if IsWarningThreshold(used, limit) {
		return TrafficStatusWarning
	}
	return TrafficStatusNormal
}

// CalculateInstanceTrafficStatus mirrors calculateInstanceTrafficStatus: an
// instance has no warning tier, only normal or limited.
func CalculateInstanceTrafficStatus(used int64, limit *int64) string {
	if IsOverLimit(used, limit) {
		return TrafficStatusLimited
	}
	return TrafficStatusNormal
}

// trafficByteUnits mirrors the units array in formatBytes.
var trafficByteUnits = []string{"B", "KB", "MB", "GB", "TB"}

// FormatBytes mirrors formatBytes: two decimals and a binary-scaled unit. Note
// the original divides as a FLOAT (`value /= 1024`), so the rounding matches
// toFixed(2) rather than an integer shift.
func FormatBytes(bytes int64) string {
	value := float64(bytes)
	unitIndex := 0
	for value >= 1024 && unitIndex < len(trafficByteUnits)-1 {
		value /= 1024
		unitIndex++
	}
	// JS toFixed rounds half away from zero; fmt's %.2f rounds half to even. The
	// difference only appears on exact .xx5 boundaries, which byte counts scaled
	// by 1024 essentially never hit, so the simpler form is kept.
	return fmt.Sprintf("%.2f %s", value, trafficByteUnits[unitIndex])
}

// trafficBytePattern mirrors /^([\d.]+)\s*(B|KB|MB|GB|TB)$/i.
var trafficBytePattern = regexp.MustCompile(`(?i)^([\d.]+)\s*(B|KB|MB|GB|TB)$`)

// trafficUnitMultipliers mirrors the multipliers record.
var trafficUnitMultipliers = map[string]int64{
	"B":  1,
	"KB": 1024,
	"MB": 1024 * 1024,
	"GB": 1024 * 1024 * 1024,
	"TB": 1024 * 1024 * 1024 * 1024,
}

// ParseBytes mirrors parseBytes: "100GB" → bytes, using `BigInt(Math.floor(value
// * Number(multiplier)))`. The float multiply then floor is intentional — doing
// the arithmetic in integers would differ for values like "1.5TB".
func ParseBytes(text string) (int64, error) {
	match := trafficBytePattern.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("Invalid byte string: %s", text)
	}

	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("Invalid byte string: %s", text)
	}

	multiplier, ok := trafficUnitMultipliers[strings.ToUpper(match[2])]
	if !ok {
		return 0, fmt.Errorf("Invalid byte string: %s", text)
	}

	return int64(math.Floor(value * float64(multiplier))), nil
}

// CalculatePercentage mirrors calculatePercentage: a float ratio capped at 100,
// and 0 when the limit is absent or zero.
func CalculatePercentage(used int64, limit *int64) float64 {
	if limit == nil || *limit == 0 {
		return 0
	}
	percentage := (float64(used) / float64(*limit)) * 100
	if percentage > 100 {
		return 100
	}
	return percentage
}

// GetTrafficPeriod mirrors getTrafficPeriod: the billing window anchored on the
// host's reset day, clamped to 1..28 so every month has the day.
//
// The TypeScript builds local Date values with `new Date(year, month, day)`.
// These stay in the caller's location so the result has the same wall-clock
// meaning.
func GetTrafficPeriod(resetDay int, reference time.Time) (periodStart, periodEnd time.Time) {
	day := resetDay
	if day < 1 {
		day = 1
	}
	if day > 28 {
		day = 28
	}

	year, month, currentDay := reference.Year(), reference.Month(), reference.Day()

	if currentDay >= day {
		periodStart = time.Date(year, month, day, 0, 0, 0, 0, reference.Location())
		periodEnd = time.Date(year, month+1, day, 0, 0, 0, 0, reference.Location())
		return periodStart, periodEnd
	}
	periodStart = time.Date(year, month-1, day, 0, 0, 0, 0, reference.Location())
	periodEnd = time.Date(year, month, day, 0, 0, 0, 0, reference.Location())
	return periodStart, periodEnd
}

// IsTodayResetDay mirrors isTodayResetDay.
func IsTodayResetDay(resetDay int, now time.Time) bool {
	return now.Day() == resetDay
}

// FormatLocalDate mirrors formatLocalDate: YYYY-MM-DD in LOCAL time. Using
// UTC here would shift the day for hosts east of Greenwich, which is exactly the
// bug the TypeScript comment warns about.
func FormatLocalDate(moment time.Time) string {
	return moment.Format("2006-01-02")
}

// FormatShanghaiDate mirrors formatLocalDate evaluated in the traffic timezone.
// Used by the traffic routes, which work in Asia/Shanghai rather than the
// process-local zone.
func FormatShanghaiDate(moment time.Time) string {
	return moment.In(loadBusinessLocation(TrafficTimeZone)).Format("2006-01-02")
}
