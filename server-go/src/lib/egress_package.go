// Port of the missing halves of server/src/lib/egress/package.ts.
//
// The TypeScript file exports five functions. Three of them are already ported
// elsewhere and are deliberately NOT duplicated here — two sources of truth for
// the same firewall instruction document would drift:
//
//	resolveProfile             → routes/egress.go  egressResolveProfile
//	packageEgressInstruction   → routes/egress.go  egressPackageEgressInstruction
//	bumpEgressHost             → db/egress.go      EgressHostNetworkPolicyBump
//
// What was genuinely missing, and is ported below:
//
//	validatePackageEgress  — the package admission rule
//	bindPackageEgress      — the binding copied onto a new instance
//
// Both are security controls. A package may only use host/direct/profile egress;
// only `profile` may name a profile; and any non-host mode requires an
// unprivileged IPv4 NAT container, because the proxy path is implemented with
// nftables rules that only exist for that shape. bindPackageEgress additionally
// requires the target node's Agent to advertise `network.apply.v2`, otherwise a
// binding would be recorded that the node cannot enforce.
//
// Errors are *NetworkInputError (HTTP 400), which the routes translate verbatim.
package lib

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// EgressPackageMaxGroups mirrors MAX_EGRESS_GROUPS, which
// server/src/lib/egress/package.ts exports. It aliases egressMaxGroups
// (lib/egress_policy.go) so the two copies of the literal cannot drift.
const EgressPackageMaxGroups = egressMaxGroups

// EgressBinding mirrors `EgressBinding = { egressMode: string; egressProfileId: number | null }`.
type EgressBinding struct {
	EgressMode      string `json:"egressMode"`
	EgressProfileID *int32 `json:"egressProfileId"`
}

// EgressPackageValidationInput mirrors the object passed to validatePackageEgress.
// The optional fields keep the TypeScript's `?? default` semantics: nil means
// "not provided".
type EgressPackageValidationInput struct {
	EgressMode      *string
	EgressProfileID *int32
	NetworkMode     *string
	InstanceType    *string
	Privileged      bool
	HostIDs         []int32
}

// EgressPackageTunnelSupportsControl is the seam for
// `hostTunnelManager.supportsControl(hostId, 'network.apply.v2')`. It defaults to
// the real tunnel manager (lib/tunnel_manager.go, same package) and stays
// overridable for tests.
//
// A node without a live, capability-advertising tunnel is reported as needing an
// Agent upgrade instead of silently accepting a binding it cannot enforce.
var EgressPackageTunnelSupportsControl = func(hostID int32, method string) bool {
	return TunnelManagerInstance().SupportsControl(hostID, TunnelControlMethod(method))
}

// egressPackageClient mirrors `client: Prisma.TransactionClient | typeof prisma`.
func egressPackageClient(client *gorm.DB) (*gorm.DB, error) {
	if client != nil {
		return client, nil
	}
	if db.DB == nil {
		return nil, errors.New("package egress: database not initialized")
	}
	return db.DB, nil
}

// egressPackageAdvisoryLock takes the SINGLE-argument advisory lock 74201 that
// the TypeScript uses for every egress binding write. It must run on the same
// transaction as the write; otherwise PostgreSQL scopes it to the implicit
// single-statement transaction and releases it immediately.
//
// Note this is the one-argument form (`pg_advisory_xact_lock(bigint)`), a
// different lock space from db.AdvisoryTransactionLock's two-int form.
func egressPackageAdvisoryLock(tx *gorm.DB) error {
	return tx.Exec("SELECT 1 FROM pg_advisory_xact_lock(?::bigint)", db.EgressAdvisoryLockNamespace).Error
}

// init installs the validator into db/packages.go (db must not import lib).
func init() {
	db.RegisterPackageEgressValidator(func(ctx context.Context, client *gorm.DB, input db.PackageEgressValidationInput) error {
		return ValidatePackageEgress(ctx, client, EgressPackageValidationInput{
			EgressMode:      input.EgressMode,
			EgressProfileID: input.EgressProfileID,
			NetworkMode:     input.NetworkMode,
			InstanceType:    input.InstanceType,
			Privileged:      input.Privileged,
			HostIDs:         input.HostIDs,
		})
	})
}

// ValidatePackageEgress mirrors validatePackageEgress.
func ValidatePackageEgress(ctx context.Context, client *gorm.DB, input EgressPackageValidationInput) error {
	mode := "host"
	if input.EgressMode != nil {
		mode = *input.EgressMode
	}
	if mode != EgressPackageModeHost && mode != EgressPackageModeDirect && mode != EgressPackageModeProfile {
		return networkError("Invalid package egress mode")
	}
	if mode != EgressPackageModeProfile && input.EgressProfileID != nil {
		return networkError("Only proxy mode may select an egress profile")
	}
	if mode == EgressPackageModeHost {
		return nil
	}

	networkMode := "nat"
	if input.NetworkMode != nil {
		networkMode = *input.NetworkMode
	}
	instanceType := "container"
	if input.InstanceType != nil {
		instanceType = *input.InstanceType
	}
	if networkMode != "nat" || instanceType != "container" || input.Privileged {
		return networkError("Package egress currently requires an unprivileged IPv4 NAT container")
	}

	if mode != EgressPackageModeProfile {
		return nil
	}

	// `input.egressProfileId ? findUnique(...) : null` — 0 is falsy in JS.
	if input.EgressProfileID == nil || *input.EgressProfileID == 0 {
		return networkError("Egress profile is inactive or not permitted on the selected hosts")
	}

	query, err := egressPackageClient(client)
	if err != nil {
		return err
	}
	var profile struct {
		ID             int32   `gorm:"column:id"`
		Active         bool    `gorm:"column:active"`
		AllowedHostIDs *string `gorm:"column:allowed_host_ids_json"`
	}
	err = query.WithContext(ctx).Raw(
		`SELECT id, active, array_to_json(allowed_host_ids)::text AS allowed_host_ids_json
		   FROM egress_profiles WHERE id = ? LIMIT 1`, *input.EgressProfileID,
	).Scan(&profile).Error
	if err != nil {
		return err
	}
	if profile.ID == 0 || !profile.Active {
		return networkError("Egress profile is inactive or not permitted on the selected hosts")
	}

	allowed := egressPackageInt32Array(profile.AllowedHostIDs)
	for _, hostID := range input.HostIDs {
		if !egressPackageContains(allowed, hostID) {
			return networkError("Egress profile is inactive or not permitted on the selected hosts")
		}
	}
	return nil
}

// BindPackageEgress mirrors bindPackageEgress: resolve the binding a new
// instance inherits (from its source instance when cloning/migrating, otherwise
// from its package), validate it against the target host, and return it.
//
// The advisory lock is taken before any read so two concurrent deliveries cannot
// interleave a policy change with this decision.
func BindPackageEgress(ctx context.Context, client *gorm.DB, packageID *int32, hostID int32, sourceID *int32) (EgressBinding, error) {
	query, err := egressPackageClient(client)
	if err != nil {
		return EgressBinding{}, err
	}
	if err := egressPackageAdvisoryLock(query); err != nil {
		return EgressBinding{}, err
	}

	type bindingRow struct {
		ID              int32  `gorm:"column:id"`
		EgressMode      string `gorm:"column:egress_mode"`
		EgressProfileID *int32 `gorm:"column:egress_profile_id"`
	}

	// `sourceId ? ... : null` — 0 is falsy in JS.
	var source *bindingRow
	if sourceID != nil && *sourceID != 0 {
		var row bindingRow
		err := query.WithContext(ctx).Raw(
			`SELECT id, egress_mode, egress_profile_id FROM instances WHERE id = ? LIMIT 1`, *sourceID,
		).Scan(&row).Error
		if err != nil {
			return EgressBinding{}, err
		}
		if row.ID != 0 {
			source = &row
		}
	}

	// `packageId ? ... : null` — 0 is falsy in JS.
	type packageRow struct {
		ID              int32  `gorm:"column:id"`
		EgressMode      string `gorm:"column:egress_mode"`
		EgressProfileID *int32 `gorm:"column:egress_profile_id"`
		NetworkMode     string `gorm:"column:network_mode"`
		InstanceType    string `gorm:"column:instance_type"`
		Privileged      bool   `gorm:"column:privileged"`
	}
	var pkg *packageRow
	if packageID != nil && *packageID != 0 {
		var row packageRow
		err := query.WithContext(ctx).Raw(
			`SELECT id, egress_mode, egress_profile_id, network_mode::text AS network_mode,
			        instance_type::text AS instance_type, privileged
			   FROM packages WHERE id = ? LIMIT 1`, *packageID,
		).Scan(&row).Error
		if err != nil {
			return EgressBinding{}, err
		}
		if row.ID != 0 {
			pkg = &row
		}
	}

	if (sourceID != nil && *sourceID != 0 && source == nil) || (packageID != nil && *packageID != 0 && pkg == nil && source == nil) {
		return EgressBinding{}, networkError("Egress binding source no longer exists")
	}

	binding := EgressBinding{EgressMode: "host"}
	switch {
	case source != nil:
		binding = EgressBinding{EgressMode: source.EgressMode, EgressProfileID: source.EgressProfileID}
	case pkg != nil:
		binding = EgressBinding{EgressMode: pkg.EgressMode, EgressProfileID: pkg.EgressProfileID}
	}

	if binding.EgressMode == "host" {
		return binding, nil
	}

	if !EgressPackageTunnelSupportsControl(hostID, "network.apply.v2") {
		return EgressBinding{}, networkError("The host needs an online Agent supporting package egress")
	}
	if source == nil {
		// `{ ...pkg!, hostIds: [hostId] }`
		networkMode := pkg.NetworkMode
		instanceType := pkg.InstanceType
		if err := ValidatePackageEgress(ctx, query, EgressPackageValidationInput{
			EgressMode:      &pkg.EgressMode,
			EgressProfileID: pkg.EgressProfileID,
			NetworkMode:     &networkMode,
			InstanceType:    &instanceType,
			Privileged:      pkg.Privileged,
			HostIDs:         []int32{hostID},
		}); err != nil {
			return EgressBinding{}, err
		}
	}
	if source != nil && source.EgressProfileID != nil && *source.EgressProfileID != 0 {
		var profile struct {
			ID             int32   `gorm:"column:id"`
			Active         bool    `gorm:"column:active"`
			AllowedHostIDs *string `gorm:"column:allowed_host_ids_json"`
		}
		err := query.WithContext(ctx).Raw(
			`SELECT id, active, array_to_json(allowed_host_ids)::text AS allowed_host_ids_json
			   FROM egress_profiles WHERE id = ? LIMIT 1`, *source.EgressProfileID,
		).Scan(&profile).Error
		if err != nil {
			return EgressBinding{}, err
		}
		if profile.ID == 0 || !profile.Active || !egressPackageContains(egressPackageInt32Array(profile.AllowedHostIDs), hostID) {
			return EgressBinding{}, networkError("The instance egress profile is not permitted on the target host")
		}
	}
	return binding, nil
}

// egressPackageInt32Array parses `array_to_json(integer[])::text`.
func egressPackageInt32Array(raw *string) []int32 {
	if raw == nil || *raw == "" || *raw == "null" {
		return []int32{}
	}
	var values []int32
	if err := json.Unmarshal([]byte(*raw), &values); err != nil || values == nil {
		return []int32{}
	}
	return values
}

// egressPackageContains mirrors Array.prototype.includes.
func egressPackageContains(values []int32, candidate int32) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
