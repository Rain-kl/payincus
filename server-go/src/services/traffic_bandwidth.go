// Port of server/src/services/traffic-bandwidth.ts.
//
// Single bandwididth arbitration point: the configured line speed, the traffic
// overage throttle and the resource guard all resolve to one value, and every place
// that writes bandwidth (traffic reset, plan change, manual limit edit, overage
// throttle) goes through it so the three sources cannot disagree.
//
// Fidelity notes that matter for parity:
//
//   - `normalizePlanTrafficLimitSpeed` has THREE outcomes: a normalised "NMbit"
//     string, null (input was empty/"0"/sub-MiB) and undefined (input was garbage).
//     Go spells that as TrafficBandwidthNormalizedSpeed{Value, Invalid}; the
//     distinction is observable because `??` only falls through on null/undefined
//     while `||` also falls through on "".
//   - the byte-valued branch divides with BigInt semantics (truncating), so
//     "999999999999999999999999" keeps full precision instead of overflowing int64.
//   - `reconcileEffectiveBandwidth` writes limits_ingress/limits_egress
//     unconditionally (NULL included), which is why it does not reuse
//     db.UpdateInstanceConfig: that helper skips nil effective values.
package services

import (
	"context"
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// TrafficBandwidthOverageThrottleConfigKey is the system-config key holding the
	// overage throttle speed. The column name for the package's normal line speed is
	// unchanged (`trafficLimitSpeed`); the overage throttle is separate.
	TrafficBandwidthOverageThrottleConfigKey = "traffic_overage_throttle_speed"
	// TrafficBandwidthDefaultOverageThrottleSpeed is the fallback throttle.
	TrafficBandwidthDefaultOverageThrottleSpeed = "1Mbit"
	// TrafficBandwidthInstanceLockPrefix is the withLock key prefix used by
	// reconcileEffectiveBandwidth (`bandwidth:instance:<id>`).
	TrafficBandwidthInstanceLockPrefix = "bandwidth:instance:"
)

// trafficBandwidthMiB is MB_IN_BYTES as a big.Int (1024 * 1024), matching the
// BigInt arithmetic in the TypeScript.
var trafficBandwidthMiB = big.NewInt(1024 * 1024)

var (
	trafficBandwidthUnitPattern   = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(mbit|gbit)$`)
	trafficBandwidthDigitsPattern = regexp.MustCompile(`^[0-9]+$`)
)

// TrafficBandwidthNormalizedSpeed is the three-state result of
// normalizePlanTrafficLimitSpeed:
//
//	Value != nil            → normalized "NMbit"
//	Value == nil, !Invalid  → null  (empty, "0", or sub-MiB byte count)
//	Invalid                 → undefined (unparseable / non-positive unit value)
type TrafficBandwidthNormalizedSpeed struct {
	Value   *string
	Invalid bool
}

// IsNull reports the TypeScript `null` outcome.
func (n TrafficBandwidthNormalizedSpeed) IsNull() bool { return n.Value == nil && !n.Invalid }

// TrafficBandwidthNormalizePlanTrafficLimitSpeed mirrors normalizePlanTrafficLimitSpeed.
//
// The TypeScript parameter is `unknown`; Go callers only ever have a database value
// (varchar), so the `typeof !== 'string'` → undefined branch is unreachable here.
// `nil` stands for SQL NULL.
func TrafficBandwidthNormalizePlanTrafficLimitSpeed(value *string) TrafficBandwidthNormalizedSpeed {
	if value == nil || *value == "" {
		return TrafficBandwidthNormalizedSpeed{}
	}

	trimmed := strings.TrimSpace(*value)
	if trimmed == "" || trimmed == "0" {
		return TrafficBandwidthNormalizedSpeed{}
	}

	if match := trafficBandwidthUnitPattern.FindStringSubmatch(trimmed); match != nil {
		numeric, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsNaN(numeric) || math.IsInf(numeric, 0) || numeric <= 0 {
			return TrafficBandwidthNormalizedSpeed{Invalid: true}
		}
		multiplier := 1.0
		if strings.EqualFold(match[2], "gbit") {
			multiplier = 1000
		}
		mbps := math.Round(numeric * multiplier)
		if mbps > 0 {
			normalized := trafficBandwidthFormatMbps(mbps)
			return TrafficBandwidthNormalizedSpeed{Value: &normalized}
		}
		return TrafficBandwidthNormalizedSpeed{Invalid: true}
	}

	if trafficBandwidthDigitsPattern.MatchString(trimmed) {
		rawBytes, ok := new(big.Int).SetString(trimmed, 10)
		if !ok {
			return TrafficBandwidthNormalizedSpeed{Invalid: true}
		}
		mbps := new(big.Int).Quo(rawBytes, trafficBandwidthMiB)
		if mbps.Sign() > 0 {
			normalized := mbps.String() + "Mbit"
			return TrafficBandwidthNormalizedSpeed{Value: &normalized}
		}
		return TrafficBandwidthNormalizedSpeed{}
	}

	return TrafficBandwidthNormalizedSpeed{Invalid: true}
}

// trafficBandwidthFormatMbps renders `${Math.round(value)}Mbit`.
//
// Values beyond int64 (a plan speed above ~9.2e18 Mbit) fall back to a plain
// decimal rendering; JavaScript would switch to exponential notation at 1e21, a
// difference that cannot be reached through the admin UI and is noted rather than
// emulated.
func trafficBandwidthFormatMbps(mbps float64) string {
	if mbps > math.MaxInt64 {
		return strconv.FormatFloat(mbps, 'f', 0, 64) + "Mbit"
	}
	return strconv.FormatInt(int64(mbps), 10) + "Mbit"
}

// trafficBandwidthMbps mirrors bandwidthMbps: normalized speed → number, or nil.
func trafficBandwidthMbps(value *string) *int64 {
	normalized := TrafficBandwidthNormalizePlanTrafficLimitSpeed(value)
	if normalized.Value == nil {
		return nil
	}
	number, err := strconv.ParseInt(strings.TrimSuffix(*normalized.Value, "Mbit"), 10, 64)
	if err != nil {
		return nil
	}
	return &number
}

// trafficBandwidthMostRestrictive mirrors mostRestrictiveBandwidth.
func trafficBandwidthMostRestrictive(values ...*string) *string {
	worst := (*int64)(nil)
	for _, value := range values {
		mbps := trafficBandwidthMbps(value)
		if mbps == nil {
			continue
		}
		if worst == nil || *mbps < *worst {
			worst = mbps
		}
	}
	if worst == nil {
		return nil
	}
	formatted := strconv.FormatInt(*worst, 10) + "Mbit"
	return &formatted
}

// TrafficBandwidthPackageLimits is the `package` include (limits ingress/egress).
type TrafficBandwidthPackageLimits struct {
	LimitsIngress *string
	LimitsEgress  *string
}

// TrafficBandwidthPlanSpeed is the `packagePlan` include (trafficLimitSpeed).
type TrafficBandwidthPlanSpeed struct {
	TrafficLimitSpeed *string
}

// TrafficBandwidthArbitrationSource mirrors BandwidthArbitrationSource.
type TrafficBandwidthArbitrationSource struct {
	ConfiguredLimitsIngress *string
	ConfiguredLimitsEgress  *string
	TrafficStatus           string
	Package                 *TrafficBandwidthPackageLimits
	PackagePlan             *TrafficBandwidthPlanSpeed
}

// TrafficBandwidthEffective mirrors EffectiveBandwidth.
type TrafficBandwidthEffective struct {
	Ingress *string `json:"ingress"`
	Egress  *string `json:"egress"`
}

// TrafficBandwidthComputeEffective mirrors computeEffectiveBandwidth.
//
// overageThrottleSpeed replaces the TypeScript default argument; callers pass
// TrafficBandwidthDefaultOverageThrottleSpeed (or the configured value).
func TrafficBandwidthComputeEffective(instance TrafficBandwidthArbitrationSource, overageThrottleSpeed string) TrafficBandwidthEffective {
	// The TypeScript normalizes the plan speed *before* the `??` chain, so an
	// invalid or empty value is undefined and falls through to the package.
	var normalLineSpeed *string
	if instance.PackagePlan != nil {
		normalLineSpeed = TrafficBandwidthNormalizePlanTrafficLimitSpeed(instance.PackagePlan.TrafficLimitSpeed).Value
	}
	var packageIngress, packageEgress *string
	if instance.Package != nil {
		packageIngress = instance.Package.LimitsIngress
		packageEgress = instance.Package.LimitsEgress
	}

	baselineIngress := trafficBandwidthFirstNonNull(instance.ConfiguredLimitsIngress, normalLineSpeed, packageIngress)
	baselineEgress := trafficBandwidthFirstNonNull(instance.ConfiguredLimitsEgress, normalLineSpeed, packageEgress)

	var trafficConstraint *string
	if instance.TrafficStatus == string(models.TrafficStatusLIMITED) {
		trafficConstraint = &overageThrottleSpeed
	}

	return TrafficBandwidthEffective{
		Ingress: trafficBandwidthMostRestrictive(baselineIngress, trafficConstraint),
		Egress:  trafficBandwidthMostRestrictive(baselineEgress, trafficConstraint),
	}
}

// trafficBandwidthFirstNonNull mirrors `a ?? b ?? c` (nullish, not truthiness).
func trafficBandwidthFirstNonNull(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// TrafficBandwidthSource mirrors TrafficBandwidthSource.
type TrafficBandwidthSource struct {
	LimitsIngress *string
	LimitsEgress  *string
	Package       *TrafficBandwidthPackageLimits
	PackagePlan   *TrafficBandwidthPlanSpeed
}

// TrafficBandwidthResolvedLimits mirrors ResolvedTrafficBandwidthLimits.
type TrafficBandwidthResolvedLimits struct {
	IncusIngress *string `json:"incusIngress"`
	IncusEgress  *string `json:"incusEgress"`
	DBIngress    *string `json:"dbIngress"`
	DBEgress     *string `json:"dbEgress"`
}

// TrafficBandwidthResolveOptions mirrors the options object. StripThrottleOverride
// is the `=== true` comparison (anything else, including absent, is false).
type TrafficBandwidthResolveOptions struct {
	StripThrottleOverride bool
	// OverageThrottleSpeed nil means "use the default".
	OverageThrottleSpeed *string
}

// TrafficBandwidthResolveTrafficBandwidthLimits mirrors resolveTrafficBandwidthLimits.
func TrafficBandwidthResolveTrafficBandwidthLimits(instance TrafficBandwidthSource, options TrafficBandwidthResolveOptions) TrafficBandwidthResolvedLimits {
	var normalLineSpeed *string
	if instance.PackagePlan != nil {
		normalLineSpeed = instance.PackagePlan.TrafficLimitSpeed
	}
	if normalized := TrafficBandwidthNormalizePlanTrafficLimitSpeed(normalLineSpeed); normalized.Value != nil {
		// The normal line speed wins outright: it is the plan's shape, not a
		// per-instance override.
		return TrafficBandwidthResolvedLimits{
			IncusIngress: normalized.Value,
			IncusEgress:  normalized.Value,
			DBIngress:    normalized.Value,
			DBEgress:     normalized.Value,
		}
	}

	overageThrottleSpeed := TrafficBandwidthDefaultOverageThrottleSpeed
	if options.OverageThrottleSpeed != nil {
		overageThrottleSpeed = *options.OverageThrottleSpeed
	}

	configuredIngress := instance.LimitsIngress
	if options.StripThrottleOverride && configuredIngress != nil && *configuredIngress == overageThrottleSpeed {
		configuredIngress = nil
	}
	configuredEgress := instance.LimitsEgress
	if options.StripThrottleOverride && configuredEgress != nil && *configuredEgress == overageThrottleSpeed {
		configuredEgress = nil
	}

	var packageIngress, packageEgress *string
	if instance.Package != nil {
		packageIngress = instance.Package.LimitsIngress
		packageEgress = instance.Package.LimitsEgress
	}

	return TrafficBandwidthResolvedLimits{
		IncusIngress: trafficBandwidthFirstNonNull(configuredIngress, packageIngress),
		IncusEgress:  trafficBandwidthFirstNonNull(configuredEgress, packageEgress),
		DBIngress:    configuredIngress,
		DBEgress:     configuredEgress,
	}
}

// TrafficBandwidthApplyLimits mirrors the restoreBandwidth device loop: every NIC
// device gets limits.ingress / limits.egress set when the value is truthy and the
// key removed otherwise (a leftover throttle must not survive a reset). The
// returned map is a copy; the input is not mutated.
func TrafficBandwidthApplyLimits(devices map[string]map[string]any, ingress, egress *string) map[string]map[string]any {
	updated := make(map[string]map[string]any, len(devices))
	for name, device := range devices {
		copied := make(map[string]any, len(device)+2)
		for key, value := range device {
			copied[key] = value
		}
		if deviceType, ok := copied["type"].(string); !ok || deviceType != "nic" {
			updated[name] = copied
			continue
		}
		if ingress != nil && *ingress != "" {
			copied["limits.ingress"] = *ingress
		} else {
			delete(copied, "limits.ingress")
		}
		if egress != nil && *egress != "" {
			copied["limits.egress"] = *egress
		} else {
			delete(copied, "limits.egress")
		}
		updated[name] = copied
	}
	return updated
}

// trafficBandwidthRestoreBandwidth mirrors restoreBandwidth from lib/incus/traffic.
func trafficBandwidthRestoreBandwidth(ctx context.Context, client *lib.IncusClient, instanceName string, ingress, egress *string) error {
	instance, err := lib.GetInstance(ctx, client, instanceName)
	if err != nil {
		return err
	}
	devices := instance.Devices
	if devices == nil {
		devices = map[string]map[string]any{}
	}
	_, err = lib.UpdateInstance(ctx, client, instanceName, map[string]any{"devices": TrafficBandwidthApplyLimits(devices, ingress, egress)})
	return err
}

// TrafficBandwidthReconcileEffective mirrors reconcileEffectiveBandwidth: recompute
// from the constraints that are still in effect and persist both the Incus devices
// and the effective DB columns, under `bandwidth:instance:<id>`.
func TrafficBandwidthReconcileEffective(ctx context.Context, instanceID int32, existingClient *lib.IncusClient) (TrafficBandwidthEffective, error) {
	locked := lib.WithLock(ctx, trafficBandwidthInstanceLockKey(instanceID), func() (TrafficBandwidthEffective, error) {
		if db.DB == nil {
			return TrafficBandwidthEffective{}, errors.New("database not initialised")
		}

		var instance models.Instance
		err := db.DB.WithContext(ctx).Where("id = ?", instanceID).Take(&instance).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || instance.Status == models.InstanceStatusDeleted {
			return TrafficBandwidthEffective{}, errors.New("instance " + strconv.FormatInt(int64(instanceID), 10) + " is unavailable for bandwidth arbitration")
		}
		if err != nil {
			return TrafficBandwidthEffective{}, err
		}

		// The TypeScript includes the relations; gorm:"-" means they are loaded here.
		var host models.Host
		if err := db.DB.WithContext(ctx).Where("id = ?", instance.HostID).Take(&host).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return TrafficBandwidthEffective{}, err
		}

		source := TrafficBandwidthArbitrationSource{
			ConfiguredLimitsIngress: instance.ConfiguredLimitsIngress,
			ConfiguredLimitsEgress:  instance.ConfiguredLimitsEgress,
			TrafficStatus:           string(instance.TrafficStatus),
		}

		if instance.PackageID != nil {
			var pkg models.Package
			err := db.DB.WithContext(ctx).Select("limits_ingress", "limits_egress").Where("id = ?", *instance.PackageID).Take(&pkg).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return TrafficBandwidthEffective{}, err
			}
			if err == nil {
				// packages.limits_ingress / limits_egress are non-null Strings with a
				// default, so Prisma hands back "" rather than null; mirror that instead
				// of inventing a NULL.
				source.Package = &TrafficBandwidthPackageLimits{
					LimitsIngress: trafficBandwidthStringPtr(pkg.LimitsIngress),
					LimitsEgress:  trafficBandwidthStringPtr(pkg.LimitsEgress),
				}
			}
		}

		if instance.PackagePlanID != nil {
			var plan models.PackagePlan
			err := db.DB.WithContext(ctx).Select("traffic_limit_speed").Where("id = ?", *instance.PackagePlanID).Take(&plan).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return TrafficBandwidthEffective{}, err
			}
			if err == nil {
				// package_plans.traffic_limit_speed is a non-null String (default
				// "1Mbit"), so the pointer is always set and "" stays observable.
				source.PackagePlan = &TrafficBandwidthPlanSpeed{TrafficLimitSpeed: trafficBandwidthStringPtr(plan.TrafficLimitSpeed)}
			}
		}

		configuredSpeed, err := db.GetSystemConfig(ctx, TrafficBandwidthOverageThrottleConfigKey, true)
		if err != nil {
			return TrafficBandwidthEffective{}, err
		}
		configuredOverageThrottleSpeed := TrafficBandwidthDefaultOverageThrottleSpeed
		if normalized := TrafficBandwidthNormalizePlanTrafficLimitSpeed(configuredSpeed); normalized.Value != nil {
			configuredOverageThrottleSpeed = *normalized.Value
		}

		effective := TrafficBandwidthComputeEffective(source, configuredOverageThrottleSpeed)

		hasCertificates := host.CertPath != nil && *host.CertPath != "" && host.KeyPath != nil && *host.KeyPath != ""
		if host.Status == models.HostStatusOnline && hasCertificates {
			client := existingClient
			if client == nil {
				client, err = lib.GetIncusClient(ctx, lib.IncusClientOptions{
					URL:           host.URL,
					CertPath:      host.CertPath,
					KeyPath:       host.KeyPath,
					TunnelEnabled: host.TunnelEnabled,
					HostID:        int64(host.ID),
					TargetHost:    host.TargetHost,
					TargetPort:    int(host.TargetPort),
				})
				if err != nil {
					return TrafficBandwidthEffective{}, err
				}
			}
			if err := trafficBandwidthRestoreBandwidth(ctx, client, instance.IncusID, effective.Ingress, effective.Egress); err != nil {
				return TrafficBandwidthEffective{}, err
			}
		}

		// Prisma's update writes both columns unconditionally, NULL included, so this
		// deliberately does not go through db.UpdateInstanceConfig (which drops nil
		// effective values).
		if err := db.DB.WithContext(ctx).Exec(
			"UPDATE instances SET limits_ingress = ?, limits_egress = ?, updated_at = NOW() WHERE id = ?",
			effective.Ingress, effective.Egress, instanceID,
		).Error; err != nil {
			return TrafficBandwidthEffective{}, err
		}

		return effective, nil
	}, trafficBandwidthLockOptions())

	if !locked.Success {
		if locked.Error != "" {
			return TrafficBandwidthEffective{}, errors.New(locked.Error)
		}
		return TrafficBandwidthEffective{}, errors.New("failed to arbitrate bandwidth for instance " + strconv.FormatInt(int64(instanceID), 10))
	}
	return locked.Result, nil
}

// trafficBandwidthLockOptions mirrors
// `{ expireMs: 30_000, waitTimeoutMs: 10_000, retryIntervalMs: 50 }`.
func trafficBandwidthLockOptions() lib.LockOptions {
	return lib.LockOptions{ExpireMs: trafficBandwidthIntPtr(30_000), WaitTimeoutMs: trafficBandwidthIntPtr(10_000), RetryIntervalMs: trafficBandwidthIntPtr(50)}
}

func trafficBandwidthIntPtr(value int) *int { return &value }

// trafficBandwidthInstanceLockKey mirrors `bandwidth:instance:${instanceId}`.
func trafficBandwidthInstanceLockKey(instanceID int32) string {
	return TrafficBandwidthInstanceLockPrefix + strconv.FormatInt(int64(instanceID), 10)
}

// trafficBandwidthStringPtr takes the address of a non-nullable database string.
func trafficBandwidthStringPtr(value string) *string { return &value }
