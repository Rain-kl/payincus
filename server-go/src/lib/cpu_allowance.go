// Port of server/src/lib/cpu-allowance.ts.
//
// Incus expresses CPU as a percentage allowance ("limits.cpu.allowance"), not as a
// core count. These helpers convert between the two and validate the 5%-step
// granularity the UI and the package editor use.
package lib

import "math"

// AllowanceToCores mirrors allowanceToCores.
//
//	allowance <= 0    → 1
//	allowance <= 100  → 1
//	otherwise         → ceil(allowance / 100)
//
// So 150% gets 2 cores and 250% gets 3. Note this is NOT cores = allowance/100:
// a 101% allowance already needs 2 cores because a container cannot be limited to
// a fraction of a core through limits.cpu.
func AllowanceToCores(allowance int) int {
	if allowance <= 0 {
		return 1
	}
	if allowance <= 100 {
		return 1
	}
	return int(math.Ceil(float64(allowance) / 100))
}

// IsValidAllowance mirrors isValidAllowance: inside the range AND a multiple of 5.
//
// maxAllowance < 0 means unlimited, mirroring the TypeScript default of Infinity
// (Go has no Infinity for an int, and a negative bound is unambiguous here).
func IsValidAllowance(allowance, minAllowance, maxAllowance int) bool {
	if allowance < minAllowance {
		return false
	}
	if maxAllowance >= 0 && allowance > maxAllowance {
		return false
	}
	return allowance%5 == 0
}

// NormalizeAllowance mirrors normalizeAllowance: clamp into range, floor to the
// nearest multiple of 5, then clamp to the minimum again.
//
// The final clamp matters: with minAllowance=15 a raw value of 12 floors to 10,
// which is below the minimum, so the result must be 15.
func NormalizeAllowance(allowance, minAllowance, maxAllowance int) int {
	if allowance < minAllowance {
		allowance = minAllowance
	}
	if maxAllowance >= 0 && allowance > maxAllowance {
		allowance = maxAllowance
	}
	allowance = (allowance / 5) * 5
	if allowance < minAllowance {
		allowance = minAllowance
	}
	return allowance
}
