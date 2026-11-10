// Port of server/src/lib/egress/attachments.ts and server/src/lib/egress/provision.ts.
//
// These two files are the network-security half of instance creation: before the
// POST that creates an instance, they pin the NIC host names, install the
// per-instance ACLs and write the egress binding row the node agent enforces. A
// failure here must abort the creation — the alternative is an instance that is
// reachable but expected to be filtered.
//
// The policy push itself (pushNetworkPolicy in lib/egress/realtime.ts, implemented
// in routes/egress.go) is not in this package: routes imports lib, so lib cannot
// import it back. It is reachable through EgressAttachmentPushNetworkPolicy, which
// is nil by default. When a caller actually needs a push and the hook is unwired it
// fails closed with an explicit error instead of creating an unprotected instance.
// Instances whose egressMode is 'host' (the schema default) never reach that path,
// which is why normal creation works without the hook.
package lib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// EgressAttachmentBinding mirrors one entry of the pushed policy's `bindings`.
type EgressAttachmentBinding struct {
	InstanceName string
	Generation   int32
}

// EgressAttachmentGroup mirrors one entry of the pushed policy's `groups`.
type EgressAttachmentGroup struct {
	Key   string
	State string
	Error string
}

// EgressAttachmentApplied mirrors the subset of pushNetworkPolicy's result that
// the readiness check reads.
type EgressAttachmentApplied struct {
	Bindings []EgressAttachmentBinding
	Groups   []EgressAttachmentGroup
}

// EgressAttachmentPushNetworkPolicy is the seam for
// `pushNetworkPolicy(hostId)` (lib/egress/realtime.ts). routes/egress.go owns the
// only implementation; nil means "not wired", and callers fail closed.
var EgressAttachmentPushNetworkPolicy func(ctx context.Context, hostID int32) (*EgressAttachmentApplied, error)

// EgressAttachmentTunnelSupportsControl is the seam for
// `hostTunnelManager.supportsControl(hostId, 'network.apply.v2')`. It defaults to
// the real tunnel manager in this package and stays overridable for tests.
var EgressAttachmentTunnelSupportsControl = func(hostID int32, method string) bool {
	return TunnelManagerInstance().SupportsControl(hostID, TunnelControlMethod(method))
}

// egressAttachmentInstance is the projection the preparation needs.
type egressAttachmentInstance struct {
	ID              int32
	HostID          int32
	IncusName       string
	EgressMode      string
	EgressProfileID *int32
	NetworkMode     string
}

// egressAttachmentNIC mirrors one entry of the nics JSON column.
type egressAttachmentNIC struct {
	HostName string `json:"hostName"`
	IPv4     string `json:"ipv4"`
	Bridge   string `json:"bridge"`
}

// EgressAttachmentPreparePackageEgress mirrors preparePackageEgress.
//
// It attaches the package egress binding to an instance create body: it validates
// that the target can actually enforce the policy, rewrites every bridged NIC with
// a deterministic host name plus IPv4/MAC filtering, upserts the attachment row
// (generation+1) inside the 74201/74202 advisory-lock pair, and finally requires the
// node to confirm the binding before the instance is allowed to start.
func EgressAttachmentPreparePackageEgress(ctx context.Context, client *IncusClient, value any, instanceID *int32) (any, error) {
	prepared, _, err := egressAttachmentPrepare(ctx, client, value, instanceID)
	return prepared, err
}

// egressAttachmentPrepare is the internal form. `changed` is the TypeScript's
// `value !== originalValue`: it tells protectProvisionedNetwork whether this
// instance is package-managed.
func egressAttachmentPrepare(ctx context.Context, client *IncusClient, value any, instanceID *int32) (any, bool, error) {
	if client == nil || client.options.HostID == 0 || value == nil {
		return value, false, nil
	}
	original, ok := value.(map[string]any)
	if !ok {
		return value, false, nil
	}
	name, _ := original["name"].(string)
	if name == "" {
		return value, false, nil
	}

	hostID := int32(client.options.HostID)
	instance, err := egressAttachmentFindInstance(ctx, hostID, name, instanceID)
	if err != nil {
		return value, false, err
	}
	if instance == nil || instance.EgressMode == "host" {
		return value, false, nil
	}

	if original["type"] == "virtual-machine" ||
		egressAttachmentConfigString(original, "security.privileged") == "true" {
		return value, false, errors.New("Package egress requires an unprivileged container")
	}
	if instance.NetworkMode != "nat" {
		return value, false, errors.New("Package egress currently requires IPv4 NAT")
	}
	if !EgressAttachmentTunnelSupportsControl(hostID, "network.apply.v2") {
		return value, false, errors.New("Upgrade the online host Agent to support package egress")
	}
	if instance.EgressProfileID != nil {
		profiles, err := db.EgressProfilesByIDs(ctx, []int32{*instance.EgressProfileID})
		if err != nil {
			return value, false, err
		}
		permitted := false
		for _, profile := range profiles {
			if profile.ID != *instance.EgressProfileID {
				continue
			}
			if profile.Active && egressAttachmentContainsInt32(profile.AllowedHostIDs, hostID) {
				permitted = true
			}
		}
		if !permitted {
			return value, false, errors.New("Instance egress is not permitted on this host")
		}
	}

	config, err := egressAttachmentClone(original)
	if err != nil {
		return value, false, err
	}

	settings, _ := config["config"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}
	settings["user.incudal.egress-instance"] = strconv.FormatInt(int64(instance.ID), 10)
	config["config"] = settings

	devices, _ := config["devices"].(map[string]any)
	if devices == nil {
		devices = map[string]any{}
	}

	expanded, err := egressAttachmentExpandDevices(ctx, client, config, devices)
	if err != nil {
		return value, false, err
	}

	nics := make([]egressAttachmentNIC, 0, 4)
	for key, source := range expanded {
		device, ok := source.(map[string]any)
		if !ok {
			continue
		}
		deviceType, _ := device["type"].(string)
		if deviceType != "nic" {
			continue
		}
		if nicType, _ := device["nictype"].(string); nicType != "" && nicType != "bridged" {
			return value, false, errors.New("Package egress only supports bridged NAT NICs")
		}
		ipv4, _ := device["ipv4.address"].(string)
		if parsed := net.ParseIP(ipv4); parsed == nil || parsed.To4() == nil {
			return value, false, errors.New("A static IPv4 allocation is required for package egress")
		}

		hostName := "ipg" + egressAttachmentHash(name+"/"+key, 11)
		rewritten := make(map[string]any, len(device)+4)
		for field, fieldValue := range device {
			rewritten[field] = fieldValue
		}
		rewritten["host_name"] = hostName
		rewritten["security.ipv4_filtering"] = "true"
		rewritten["security.mac_filtering"] = "true"
		rewritten["ipv6.address"] = "none"
		devices[key] = rewritten

		bridge, _ := device["parent"].(string)
		if bridge == "" {
			bridge, _ = device["network"].(string)
		}
		nics = append(nics, egressAttachmentNIC{HostName: hostName, IPv4: ipv4, Bridge: bridge})
	}
	config["devices"] = devices

	if len(nics) == 0 || len(nics) > 4 {
		return value, false, errors.New("Invalid package egress NIC inventory")
	}

	generation, err := egressAttachmentBind(ctx, hostID, instance, nics)
	if err != nil {
		return value, false, err
	}
	TaskLog(ctx, "info", fmt.Sprintf("应用实例出口绑定 %s generation=%d", name, generation))

	applied, err := egressAttachmentPush(ctx, hostID)
	if err != nil {
		return value, false, err
	}
	if !egressAttachmentBindingReady(applied, name, generation) {
		detail := ""
		if group := egressAttachmentFindGroup(applied, instance.EgressProfileID); group != nil {
			detail = fmt.Sprintf(" (%s: %s/%s)", group.Key, group.State, group.Error)
		}
		return value, false, fmt.Errorf("The instance egress binding is not ready%s; outbound access remains blocked", detail)
	}

	return config, true, nil
}

// EgressAttachmentEnsureBeforeStart mirrors ensurePackageEgressBeforeStart: the
// binding must be applied to the live instance body before the guest can send its
// first packet.
func EgressAttachmentEnsureBeforeStart(ctx context.Context, client *IncusClient, name string) error {
	if client == nil || client.options.HostID == 0 {
		return nil
	}
	hostID := int32(client.options.HostID)

	exact, err := egressAttachmentFindInstance(ctx, hostID, name, nil)
	if err != nil {
		return err
	}
	if exact != nil && exact.EgressMode == "host" {
		return nil
	}

	current, err := incusInstanceFetchRaw(ctx, client, name)
	if err != nil {
		return err
	}

	var instanceID int32
	if exact != nil {
		instanceID = exact.ID
	} else {
		instanceID = egressAttachmentInstanceIDFromConfig(current)
	}
	if instanceID <= 0 {
		return nil
	}

	input := map[string]any{
		"name":     name,
		"type":     current["type"],
		"config":   current["config"],
		"devices":  current["devices"],
		"profiles": current["profiles"],
	}
	next, changed, err := egressAttachmentPrepare(ctx, client, input, &instanceID)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	nextConfig, _ := next.(map[string]any)
	body := map[string]any{
		"config":   nextConfig["config"],
		"devices":  nextConfig["devices"],
		"profiles": nextConfig["profiles"],
	}
	_, err = client.Request(ctx, http.MethodPut, "/1.0/instances/"+url.PathEscape(name),
		body, incusInstanceOperationTimeout, body)
	return err
}

// EgressAttachmentRelease mirrors releasePackageEgress.
//
// The TypeScript swallows every error and logs one warning: the deletion is
// best-effort and the node re-syncs on its next reconnect. Keeping that contract
// matters because deleteInstance must stay idempotent.
func EgressAttachmentRelease(ctx context.Context, client *IncusClient, name string) error {
	if client == nil || client.options.HostID == 0 {
		return nil
	}
	hostID := int32(client.options.HostID)

	changed := int64(0)
	if err := func() error {
		if db.Pool() == nil {
			return errors.New("database not initialized")
		}
		firewall, err := json.Marshal(DefaultFirewall())
		if err != nil {
			return err
		}
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

		// Single-argument lock 74201 then two-argument (74202, hostId): different
		// lock spaces, see WAVE_D_SPEC §0.4.
		if _, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock(74201)"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock(74202, $1)", hostID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM instance_egress_attachments WHERE host_id = $1 AND incus_name = $2`, hostID, name)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected()
		if changed > 0 {
			if err := db.EgressHostNetworkPolicyBump(ctx, tx, hostID, firewall); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}(); err != nil {
		TaskLog(ctx, "warn", "出口绑定清理等待节点重连后同步")
		return nil
	}

	if changed > 0 {
		if _, err := egressAttachmentPush(ctx, hostID); err != nil {
			TaskLog(ctx, "warn", "出口绑定清理等待节点重连后同步")
		}
	}
	return nil
}

// EgressAttachmentProtectProvisionedNetwork mirrors protectProvisionedNetwork.
//
// Network ACLs cover Internet traffic; the per-NIC ACLs added here additionally
// cover same-bridge traffic. A host whose policy is not applied aborts the create:
// creating a NIC before the ACL exists would let the first packets through.
func EgressAttachmentProtectProvisionedNetwork(ctx context.Context, client *IncusClient, value any, instanceID *int32) (any, error) {
	if client == nil || client.options.HostID == 0 || value == nil {
		return value, nil
	}
	if _, ok := value.(map[string]any); !ok {
		return value, nil
	}

	prepared, packageManaged, err := egressAttachmentPrepare(ctx, client, value, instanceID)
	if err != nil {
		return value, err
	}

	hostID := int32(client.options.HostID)
	policy, err := db.EgressHostNetworkPolicyGet(ctx, hostID)
	if err != nil {
		return value, err
	}
	if policy == nil {
		return prepared, nil
	}

	var firewall FirewallPolicy
	if len(policy.Firewall) > 0 {
		if err := json.Unmarshal(policy.Firewall, &firewall); err != nil {
			return value, err
		}
	}
	if !firewall.Enabled && (policy.EgressMode == "off" || packageManaged) {
		return prepared, nil
	}

	var status map[string]any
	if len(policy.Status) > 0 {
		if err := json.Unmarshal(policy.Status, &status); err != nil {
			return value, err
		}
	}
	if policy.AppliedRevision != policy.Revision || status["state"] != "applied" {
		return value, errors.New("Host network policy is pending or failed; retry after the Agent confirms application")
	}

	original, _ := prepared.(map[string]any)
	config, err := egressAttachmentClone(original)
	if err != nil {
		return value, err
	}
	devices, _ := config["devices"].(map[string]any)
	if devices == nil {
		devices = map[string]any{}
	}
	settings, _ := config["config"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}

	expanded, err := egressAttachmentExpandDevices(ctx, client, config, devices)
	if err != nil {
		return value, err
	}

	for key, source := range expanded {
		device, ok := source.(map[string]any)
		if !ok {
			continue
		}
		if deviceType, _ := device["type"].(string); deviceType != "nic" {
			continue
		}

		rewritten := make(map[string]any, len(device)+4)
		for field, fieldValue := range device {
			rewritten[field] = fieldValue
		}

		nicType, _ := rewritten["nictype"].(string)
		network, _ := rewritten["network"].(string)
		switch {
		case nicType == "routed":
			rewritten["host_name"] = "iceg" + egressAttachmentHash(fmt.Sprintf("%v/%s", config["name"], key), 10)
		case network != "" || nicType == "bridged":
			if firewall.Enabled {
				acls := egressAttachmentAppendACL(rewritten["security.acls"], "incudal-host-firewall")
				rewritten["security.acls"] = acls
				for _, direction := range []string{"ingress", "egress"} {
					field := "security.acls.default." + direction + ".action"
					action := "unset"
					if value, ok := rewritten[field].(string); ok && value != "" {
						action = value
					}
					settings["user.incudal.acl.nic."+key+"."+direction] = action
					if direction == "ingress" {
						rewritten[field] = firewall.IngressAction
					} else {
						rewritten[field] = firewall.EgressAction
					}
				}
			}
		default:
			return value, errors.New("Unsupported NIC type for managed host network protection")
		}

		devices[key] = rewritten
	}

	config["devices"] = devices
	config["config"] = settings
	return config, nil
}

// ==================== helpers ====================

// egressAttachmentFindInstance mirrors the two lookup forms of preparePackageEgress:
// by id when the caller has one, otherwise by (hostId, incusId).
func egressAttachmentFindInstance(ctx context.Context, hostID int32, name string, instanceID *int32) (*egressAttachmentInstance, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}

	var row *egressAttachmentInstance
	scan := func(scanner interface{ Scan(dest ...any) error }) error {
		var instance egressAttachmentInstance
		if err := scanner.Scan(&instance.ID, &instance.HostID, &instance.IncusName,
			&instance.EgressMode, &instance.EgressProfileID, &instance.NetworkMode); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		row = &instance
		return nil
	}

	const columns = `id, host_id, incus_id, egress_mode, egress_profile_id, network_mode`
	if instanceID != nil {
		if err := scan(db.Pool().QueryRow(ctx,
			`SELECT `+columns+` FROM instances WHERE id = $1`, *instanceID)); err != nil {
			return nil, err
		}
		return row, nil
	}
	if err := scan(db.Pool().QueryRow(ctx,
		`SELECT `+columns+` FROM instances WHERE host_id = $1 AND incus_id = $2 LIMIT 1`, hostID, name)); err != nil {
		return nil, err
	}
	return row, nil
}

// egressAttachmentBind mirrors the transaction: lock, capacity check, upsert,
// policy bump. It returns the new generation.
func egressAttachmentBind(ctx context.Context, hostID int32, instance *egressAttachmentInstance, nics []egressAttachmentNIC) (int32, error) {
	if db.Pool() == nil {
		return 0, errors.New("database not initialized")
	}
	nicsJSON, err := json.Marshal(nics)
	if err != nil {
		return 0, err
	}
	firewall, err := json.Marshal(DefaultFirewall())
	if err != nil {
		return 0, err
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock(74201)"); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock(74202, $1)", hostID); err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx, `SELECT DISTINCT a.profile_id
		FROM instance_egress_attachments a
		JOIN instances i ON i.id = a.instance_id
		WHERE a.host_id = $1 AND a.profile_id IS NOT NULL AND i.status <> 'deleted'`, hostID)
	if err != nil {
		return 0, err
	}
	groups := make([]int32, 0, 16)
	for rows.Next() {
		var profileID int32
		if err := rows.Scan(&profileID); err != nil {
			rows.Close()
			return 0, err
		}
		groups = append(groups, profileID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if instance.EgressProfileID != nil &&
		!egressAttachmentContainsInt32(groups, *instance.EgressProfileID) &&
		len(groups) >= EgressPackageMaxGroups {
		return 0, errors.New("Host package egress group capacity exceeded")
	}

	var generation int32
	err = tx.QueryRow(ctx, `INSERT INTO instance_egress_attachments
			(host_id, instance_id, incus_name, mode, profile_id, nics)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (host_id, incus_name) DO UPDATE SET
			instance_id = EXCLUDED.instance_id,
			mode = EXCLUDED.mode,
			profile_id = EXCLUDED.profile_id,
			nics = EXCLUDED.nics,
			generation = instance_egress_attachments.generation + 1
		RETURNING generation`,
		hostID, instance.ID, instance.IncusName, instance.EgressMode, instance.EgressProfileID, string(nicsJSON)).
		Scan(&generation)
	if err != nil {
		return 0, err
	}

	if err := db.EgressHostNetworkPolicyBump(ctx, tx, hostID, firewall); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return generation, nil
}

// egressAttachmentPush invokes the policy-push seam, failing closed when it is not
// wired. Reaching this with a nil hook means an instance is *intended* to be
// filtered by package egress, which is precisely the case where silently skipping
// the push would be a security regression.
func egressAttachmentPush(ctx context.Context, hostID int32) (*EgressAttachmentApplied, error) {
	if EgressAttachmentPushNetworkPolicy == nil {
		return nil, errors.New("出口策略下发未接线（pushNetworkPolicy）：拒绝在未确认出口过滤生效前创建实例")
	}
	return EgressAttachmentPushNetworkPolicy(ctx, hostID)
}

// egressAttachmentBindingReady mirrors the module filter of the push result.
func egressAttachmentBindingReady(applied *EgressAttachmentApplied, name string, generation int32) bool {
	if applied == nil {
		return false
	}
	for _, binding := range applied.Bindings {
		if binding.InstanceName == name && binding.Generation == generation {
			return true
		}
	}
	return false
}

// egressAttachmentFindGroup mirrors
// `groups.find(group => group.key === "p" + profileId)`.
func egressAttachmentFindGroup(applied *EgressAttachmentApplied, profileID *int32) *EgressAttachmentGroup {
	if applied == nil || profileID == nil {
		return nil
	}
	key := "p" + strconv.FormatInt(int64(*profileID), 10)
	for index := range applied.Groups {
		if applied.Groups[index].Key == key {
			return &applied.Groups[index]
		}
	}
	return nil
}

// egressAttachmentExpandDevices mirrors the profile-expansion loop used by both
// files: every profile's devices first (default profile when the list is empty),
// then the instance's own devices.
func egressAttachmentExpandDevices(ctx context.Context, client *IncusClient, config, devices map[string]any) (map[string]any, error) {
	// `config.profiles ?? ['default']`: an explicit empty array expands nothing.
	profiles := []string{"default"}
	switch raw := config["profiles"].(type) {
	case nil:
		profiles = []string{"default"}
	case []any:
		profiles = make([]string, 0, len(raw))
		for _, entry := range raw {
			if name, ok := entry.(string); ok {
				profiles = append(profiles, name)
			}
		}
	case []string:
		profiles = raw
	default:
		profiles = []string{"default"}
	}

	expanded := map[string]any{}
	for _, profile := range profiles {
		raw, err := client.Request(ctx, http.MethodGet, "/1.0/profiles/"+url.PathEscape(profile), nil,
			incusInstanceOperationTimeout, nil)
		if err != nil {
			return nil, err
		}
		var data struct {
			Devices map[string]any `json:"devices"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, NewIncusAPIError("节点返回的 profile 数据格式无效", 502, nil)
		}
		for key, value := range data.Devices {
			expanded[key] = value
		}
	}
	for key, value := range devices {
		expanded[key] = value
	}
	return expanded, nil
}

// egressAttachmentConfigString mirrors `config.config?.[key]`.
func egressAttachmentConfigString(config map[string]any, key string) string {
	settings, _ := config["config"].(map[string]any)
	if settings == nil {
		return ""
	}
	value, _ := settings[key].(string)
	return value
}

// egressAttachmentClone mirrors structuredClone for the JSON-shaped create body.
func egressAttachmentClone(value map[string]any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var cloned map[string]any
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil, err
	}
	if cloned == nil {
		cloned = map[string]any{}
	}
	return cloned, nil
}

// egressAttachmentHash mirrors `createHash('sha256').update(seed).digest('hex').slice(0, length)`.
func egressAttachmentHash(seed string, length int) string {
	sum := sha256.Sum256([]byte(seed))
	encoded := hex.EncodeToString(sum[:])
	if len(encoded) < length {
		return encoded
	}
	return encoded[:length]
}

// egressAttachmentAppendACL mirrors the `[...new Set([...existing.split(','), acl])]`
// de-duplication, preserving the original order. Only a strictly empty entry is
// dropped — the TypeScript's `.filter(Boolean)` keeps whitespace-only entries.
func egressAttachmentAppendACL(raw any, acl string) string {
	existing, _ := raw.(string)
	values := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, entry := range strings.Split(existing, ",") {
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		values = append(values, entry)
	}
	if !seen[acl] {
		values = append(values, acl)
	}
	return strings.Join(values, ",")
}

// egressAttachmentInstanceIDFromConfig mirrors
// `Number(current.config?.['user.incudal.egress-instance'])` with the
// safe-integer/positive guard.
func egressAttachmentInstanceIDFromConfig(instance map[string]any) int32 {
	settings, _ := instance["config"].(map[string]any)
	if settings == nil {
		return 0
	}
	raw, _ := settings["user.incudal.egress-instance"].(string)
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || parsed <= 0 || parsed > 2147483647 {
		return 0
	}
	return int32(parsed)
}

// egressAttachmentContainsInt32 mirrors Array.prototype.includes.
func egressAttachmentContainsInt32(values []int32, candidate int32) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
