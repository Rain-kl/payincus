// Port of server/src/lib/traffic-multiplier.ts.
//
// A package is sold per host, and each package↔host binding carries a traffic
// multiplier that scales the plan's monthly traffic allowance on that host. The
// multiplier is a *resource promise*: it is stored in `package_hosts.traffic_multiplier`
// (Decimal(8,3)) and copied onto instances, so the normalisation below is a
// contract rather than a formatting choice:
//
//   - absent / null / ”  → 1 (the Prisma column default)
//   - anything outside [0.001, 100] or non-numeric → an error with the exact
//     TypeScript message; the routes surface it as a 400
//   - the value is rounded to 3 decimals with JavaScript's `Math.round`
//     (half away from zero on the exact double; Go's math.Round matches for the
//     whole positive range this function accepts)
//
// `applyTrafficMultiplier` reproduces the BigInt arithmetic exactly:
// `(limit * BigInt(Math.round(normalized * 1000))) / 1000n`. BigInt division
// truncates toward zero, so this uses math/big instead of int64 arithmetic (the
// product of a 63-bit limit and a 100000 scale overflows int64).
//
// db/traffic.go carries a private duplicate of Normalize/ApplyTrafficMultiplier
// because the db package must not import lib (lib imports db). The two
// implementations are pinned to the same cases by tests in both packages; keep
// them in lock-step.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TrafficMultiplierScale mirrors MULTIPLIER_SCALE (1000n).
const TrafficMultiplierScale = int64(1000)

// ErrTrafficMultiplierRange mirrors the TypeScript `throw new Error(...)` text.
var ErrTrafficMultiplierRange = errors.New("Traffic multiplier must be at least 0.001 and no more than 100")

// TrafficMultiplierResolveParams mirrors the object passed to
// resolveInstanceTrafficLimitForHost.
type TrafficMultiplierResolveParams struct {
	// PackageID is `packageId: number | null | undefined`; nil and 0 are both
	// "no package" (the TypeScript guard is `if (!params.packageId)`).
	PackageID *int32
	HostID    int32
	// BaseTrafficLimit is `baseTrafficLimit: bigint | null`.
	BaseTrafficLimit *int64
}

// trafficMultiplierClient mirrors `const client = ...` handling in the
// TypeScript: a nil client means the package-level pool.
func trafficMultiplierClient(client *gorm.DB) *gorm.DB {
	if client == nil {
		return db.DB
	}
	return client
}

// NormalizeTrafficMultiplier mirrors normalizeTrafficMultiplier(value: unknown).
//
// `value` is `unknown` in the TypeScript because it comes from the database
// (Prisma Decimal), from JSON (number/string/null) or from an already-normalised
// float. Every shape the callers can produce is handled explicitly.
func NormalizeTrafficMultiplier(value any) (float64, error) {
	switch typed := value.(type) {
	case nil:
		// `value === null || value === undefined || value === ''`
		return 1, nil
	case string:
		if typed == "" {
			return 1, nil
		}
		numeric, ok := trafficMultiplierJSNumber(typed)
		if !ok {
			return 0, ErrTrafficMultiplierRange
		}
		return trafficMultiplierRound(numeric)
	case float64:
		return trafficMultiplierRound(typed)
	case float32:
		return trafficMultiplierRound(float64(typed))
	case int:
		return trafficMultiplierRound(float64(typed))
	case int32:
		return trafficMultiplierRound(float64(typed))
	case int64:
		return trafficMultiplierRound(float64(typed))
	case uint:
		return trafficMultiplierRound(float64(typed))
	case uint32:
		return trafficMultiplierRound(float64(typed))
	case uint64:
		return trafficMultiplierRound(float64(typed))
	case bool:
		// Number(true) === 1, Number(false) === 0; false is then rejected by the
		// range check, exactly like the TypeScript.
		if typed {
			return 1, nil
		}
		return 0, ErrTrafficMultiplierRange
	case json.Number:
		numeric, ok := trafficMultiplierJSNumber(typed.String())
		if !ok {
			return 0, ErrTrafficMultiplierRange
		}
		return trafficMultiplierRound(numeric)
	case models.Decimal:
		// `Number(decimal)` goes through Decimal.valueOf() → the double value.
		numeric, _ := typed.Decimal.Float64()
		return trafficMultiplierRound(numeric)
	case *models.Decimal:
		if typed == nil {
			return 1, nil
		}
		numeric, _ := typed.Decimal.Float64()
		return trafficMultiplierRound(numeric)
	default:
		// Number(object) is NaN in JavaScript; so is anything else unhandled.
		return 0, ErrTrafficMultiplierRange
	}
}

// trafficMultiplierRound mirrors `Math.round(numeric * 1000) / 1000` plus the
// range guard. The guard runs *before* rounding, exactly like the TypeScript.
func trafficMultiplierRound(numeric float64) (float64, error) {
	if math.IsNaN(numeric) || math.IsInf(numeric, 0) || numeric < 0.001 || numeric > 100 {
		return 0, ErrTrafficMultiplierRange
	}
	return math.Round(numeric*float64(TrafficMultiplierScale)) / float64(TrafficMultiplierScale), nil
}

// trafficMultiplierJSNumber mirrors `Number(text)` for a string: JavaScript
// trims whitespace, accepts decimal/scientific notation, the Infinity literals
// and the 0x/0b/0o integer prefixes. It returns ok=false for NaN so the caller
// raises the range error (the TypeScript's `!Number.isFinite(numeric)`).
func trafficMultiplierJSNumber(text string) (float64, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		// Number('') and Number('   ') are 0.
		return 0, true
	}
	switch trimmed {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}

	lower := strings.ToLower(trimmed)
	sign := 1.0
	digits := lower
	if strings.HasPrefix(digits, "+") {
		digits = digits[1:]
	} else if strings.HasPrefix(digits, "-") {
		sign = -1
		digits = digits[1:]
	}
	for _, prefix := range []struct {
		text string
		base int
	}{{"0x", 16}, {"0b", 2}, {"0o", 8}} {
		if strings.HasPrefix(digits, prefix.text) {
			parsed, err := strconv.ParseUint(digits[len(prefix.text):], prefix.base, 64)
			if err != nil {
				return 0, false
			}
			return sign * float64(parsed), true
		}
	}

	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// ApplyTrafficMultiplier mirrors applyTrafficMultiplier(limit, multiplier):
// `(limit * scaled) / MULTIPLIER_SCALE` with BigInt truncation toward zero.
// A nil limit stays unlimited (nil), and the multiplier is normalised first.
func ApplyTrafficMultiplier(limit *int64, multiplier any) (*int64, error) {
	if limit == nil {
		return nil, nil
	}

	normalized, err := NormalizeTrafficMultiplier(multiplier)
	if err != nil {
		return nil, err
	}
	scaled := int64(math.Round(normalized * float64(TrafficMultiplierScale)))

	product := new(big.Int).Mul(big.NewInt(*limit), big.NewInt(scaled))
	quotient := new(big.Int).Quo(product, big.NewInt(TrafficMultiplierScale))
	if !quotient.IsInt64() {
		// BigInt has no overflow; an int64 column does. Fail loudly instead of
		// silently truncating a traffic promise.
		return nil, errors.New("Traffic multiplier result out of range")
	}
	result := quotient.Int64()
	return &result, nil
}

// GetPackageHostTrafficMultiplier mirrors getPackageHostTrafficMultiplier: a
// missing binding normalises to 1, exactly like `binding?.trafficMultiplier`.
func GetPackageHostTrafficMultiplier(ctx context.Context, client *gorm.DB, packageID, hostID int32) (float64, error) {
	var row struct {
		TrafficMultiplier *models.Decimal `gorm:"column:traffic_multiplier"`
	}
	err := trafficMultiplierClient(client).WithContext(ctx).Raw(
		`SELECT traffic_multiplier FROM package_hosts WHERE package_id = ? AND host_id = ? LIMIT 1`,
		packageID, hostID,
	).Scan(&row).Error
	if err != nil {
		return 0, err
	}
	return NormalizeTrafficMultiplier(row.TrafficMultiplier)
}

// ResolveInstanceTrafficLimitForHost mirrors resolveInstanceTrafficLimitForHost.
// An absent/zero package id leaves the plan's own limit untouched.
func ResolveInstanceTrafficLimitForHost(ctx context.Context, client *gorm.DB, params TrafficMultiplierResolveParams) (*int64, error) {
	if params.PackageID == nil || *params.PackageID == 0 {
		return params.BaseTrafficLimit, nil
	}
	multiplier, err := GetPackageHostTrafficMultiplier(ctx, client, *params.PackageID, params.HostID)
	if err != nil {
		return nil, err
	}
	return ApplyTrafficMultiplier(params.BaseTrafficLimit, multiplier)
}
