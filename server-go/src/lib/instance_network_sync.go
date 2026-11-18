// Port of server/src/lib/instance-network-sync.ts.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// InstanceNetworkSyncTarget mirrors InstanceNetworkSyncTarget.
type InstanceNetworkSyncTarget struct {
	ID          int32
	Name        string
	HostID      *int32
	NetworkMode string
	IPv4        *string
	IPv6        *string
}

// ResolvedInstanceNetworkAddresses mirrors ResolvedInstanceNetworkAddresses.
type ResolvedInstanceNetworkAddresses struct {
	IPv4           *string
	IPv6           *string
	IPv4Device     string
	IPv6Device     string
	ConfiguredIPv6 *string
	ObservedIPv6   *string
}

// PersistedInstanceNetworkAddresses mirrors PersistedInstanceNetworkAddresses.
type PersistedInstanceNetworkAddresses struct {
	IPv4Changed bool    `json:"ipv4Changed"`
	OldIPv4     *string `json:"oldIpv4"`
	NewIPv4     *string `json:"newIpv4"`
	IPv6Changed bool    `json:"ipv6Changed"`
	OldIPv6     *string `json:"oldIpv6"`
	NewIPv6     *string `json:"newIpv6"`
}

// PersistNetworkAddressesOptions mirrors `{ updateLastSyncedAt?: boolean }`.
type PersistNetworkAddressesOptions struct {
	UpdateLastSyncedAt bool
}

type networkStateAddress struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Scope   string `json:"scope"`
}

type networkStateInterface struct {
	Addresses []networkStateAddress `json:"addresses"`
}

type networkStatePayload struct {
	Status  string                           `json:"status"`
	Network map[string]networkStateInterface `json:"network"`
}

type networkDevicePayload struct {
	Devices         map[string]map[string]any `json:"devices"`
	ExpandedDevices map[string]map[string]any `json:"expanded_devices"`
}

var skippedNetworkInterfaces = []string{"lo", "docker", "br-", "veth", "warp", "CloudflareWARP"}

func normalizeNetworkSyncIPv6(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	first := strings.TrimSpace(strings.SplitN(trimmed, ",", 2)[0])
	return strings.TrimSpace(strings.SplitN(first, "/", 2)[0])
}

func isGlobalRoutableIPv6(value string) bool {
	normalized := normalizeNetworkSyncIPv6(value)
	if normalized == "" {
		return false
	}
	lower := strings.ToLower(normalized)
	return !strings.HasPrefix(lower, "fd") &&
		!strings.HasPrefix(lower, "fc") &&
		!strings.HasPrefix(lower, "fe80:") &&
		lower != "::1" &&
		lower != "::"
}

func shouldSkipNetworkInterface(iface string) bool {
	for _, prefix := range skippedNetworkInterfaces {
		if iface == prefix || strings.HasPrefix(iface, prefix) {
			return true
		}
	}
	return false
}

func pickGlobalIPv6FromAddresses(addresses []networkStateAddress) *string {
	for _, addr := range addresses {
		if addr.Family != "inet6" || addr.Scope != "global" {
			continue
		}
		normalized := normalizeNetworkSyncIPv6(addr.Address)
		if isGlobalRoutableIPv6(normalized) {
			return &normalized
		}
	}
	return nil
}

// ExtractIPv4FromState mirrors extractIpv4FromState from instance-network-sync.ts.
func ExtractIPv4FromState(stateRaw json.RawMessage) *string {
	if len(stateRaw) == 0 {
		return nil
	}
	var state networkStatePayload
	if err := json.Unmarshal(stateRaw, &state); err != nil || len(state.Network) == 0 {
		return nil
	}
	if eth0, ok := state.Network["eth0"]; ok {
		for _, addr := range eth0.Addresses {
			if addr.Family == "inet" && addr.Scope == "global" && addr.Address != "" {
				ip := addr.Address
				return &ip
			}
		}
	}
	names := make([]string, 0, len(state.Network))
	for name := range state.Network {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, iface := range names {
		if shouldSkipNetworkInterface(iface) {
			continue
		}
		for _, addr := range state.Network[iface].Addresses {
			if addr.Family == "inet" && addr.Scope == "global" && addr.Address != "" {
				ip := addr.Address
				return &ip
			}
		}
	}
	return nil
}

// ExtractIPv6FromState mirrors extractIpv6FromState from instance-network-sync.ts.
func ExtractIPv6FromState(stateRaw json.RawMessage) *string {
	if len(stateRaw) == 0 {
		return nil
	}
	var state networkStatePayload
	if err := json.Unmarshal(stateRaw, &state); err != nil || len(state.Network) == 0 {
		return nil
	}
	for _, iface := range []string{"eth1", "enp5s0", "ens5", "eno1"} {
		if info, ok := state.Network[iface]; ok {
			if ip := pickGlobalIPv6FromAddresses(info.Addresses); ip != nil {
				return ip
			}
		}
	}
	names := make([]string, 0, len(state.Network))
	for name := range state.Network {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, iface := range names {
		if shouldSkipNetworkInterface(iface) {
			continue
		}
		if ip := pickGlobalIPv6FromAddresses(state.Network[iface].Addresses); ip != nil {
			return ip
		}
	}
	return nil
}

// ExtractConfiguredPrimaryIPv6 mirrors extractConfiguredPrimaryIpv6.
func ExtractConfiguredPrimaryIPv6(instanceRaw json.RawMessage) (*string, string) {
	if len(instanceRaw) == 0 {
		return nil, "eth1"
	}
	var payload networkDevicePayload
	if err := json.Unmarshal(instanceRaw, &payload); err != nil {
		return nil, "eth1"
	}
	devices := payload.ExpandedDevices
	if len(devices) == 0 {
		devices = payload.Devices
	}
	if len(devices) == 0 {
		return nil, "eth1"
	}

	keys := make([]string, 0, len(devices))
	hasEth1 := false
	for k := range devices {
		if k == "eth1" {
			hasEth1 = true
		} else {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if hasEth1 {
		keys = append([]string{"eth1"}, keys...)
	}

	for _, fallbackDevice := range keys {
		dev := devices[fallbackDevice]
		rawIPv6, _ := dev["ipv6.address"].(string)
		if rawIPv6 == "" {
			continue
		}
		for _, part := range strings.Split(rawIPv6, ",") {
			norm := normalizeNetworkSyncIPv6(part)
			if isGlobalRoutableIPv6(norm) {
				deviceName := fallbackDevice
				if custom, ok := dev["name"].(string); ok && strings.TrimSpace(custom) != "" {
					deviceName = strings.TrimSpace(custom)
				}
				if deviceName == "" {
					deviceName = "eth1"
				}
				return &norm, deviceName
			}
		}
	}
	return nil, "eth1"
}

// ResolveInstanceNetworkAddresses mirrors resolveInstanceNetworkAddresses.
func ResolveInstanceNetworkAddresses(
	target InstanceNetworkSyncTarget,
	incusInstanceRaw json.RawMessage,
	stateRaw json.RawMessage,
) ResolvedInstanceNetworkAddresses {
	observedIPv4 := ExtractIPv4FromState(stateRaw)
	observedIPv6 := ExtractIPv6FromState(stateRaw)
	configuredIPv6, ipv6Device := ExtractConfiguredPrimaryIPv6(incusInstanceRaw)

	resolvedIPv4 := target.IPv4
	if observedIPv4 != nil {
		resolvedIPv4 = observedIPv4
	}

	resolvedIPv6 := target.IPv6
	switch target.NetworkMode {
	case "nat_ipv6", "ipv6_only", "public_ipv4_ipv6":
		if configuredIPv6 != nil {
			resolvedIPv6 = configuredIPv6
		} else if observedIPv6 != nil {
			resolvedIPv6 = observedIPv6
		}
	case "nat_ipv6_nat", "ipv6_nat":
		if observedIPv6 != nil {
			resolvedIPv6 = observedIPv6
		}
	}

	return ResolvedInstanceNetworkAddresses{
		IPv4:           resolvedIPv4,
		IPv6:           resolvedIPv6,
		IPv4Device:     "eth0",
		IPv6Device:     ipv6Device,
		ConfiguredIPv6: configuredIPv6,
		ObservedIPv6:   observedIPv6,
	}
}

func sameStringPtr(left, right *string) bool {
	if left == nil && right == nil {
		return true
	}
	if left == nil || right == nil {
		return false
	}
	return *left == *right
}

func cloneStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	copyVal := *v
	return &copyVal
}

func syncPrimaryIPAddressTx(
	ctx context.Context,
	tx *gorm.DB,
	target InstanceNetworkSyncTarget,
	addrType models.IpType,
	address *string,
	device string,
) error {
	if address == nil || *address == "" {
		return tx.WithContext(ctx).
			Where("instance_id = ? AND type = ? AND is_primary = true AND is_custom = false", target.ID, addrType).
			Delete(&models.IpAddress{}).Error
	}

	var hostID int32
	if target.HostID != nil && *target.HostID != 0 {
		hostID = *target.HostID
	} else {
		if err := tx.WithContext(ctx).Raw(`SELECT host_id FROM instances WHERE id = ? LIMIT 1`, target.ID).Scan(&hostID).Error; err != nil {
			return err
		}
	}
	if hostID == 0 {
		return fmt.Errorf("Unable to resolve host scope for instance %d", target.ID)
	}

	if addrType == models.IpTypeInet6 {
		if err := db.AdvisoryTransactionLockStringTx(tx, db.IPAddressAllocationLockNamespace, *address); err != nil {
			return err
		}
	}

	var conflictID int32
	if addrType == models.IpTypeInet4 {
		if err := tx.WithContext(ctx).Raw(
			`SELECT id FROM instances i
			 WHERE i.id <> ? AND i.status <> 'deleted' AND i.host_id = ?
			   AND (i.ipv4 = ? OR EXISTS (SELECT 1 FROM ip_addresses a WHERE a.instance_id = i.id AND a.address = ? AND a.type = 'inet4'))
			 LIMIT 1`,
			target.ID, hostID, *address, *address,
		).Scan(&conflictID).Error; err != nil {
			return err
		}
	} else {
		if err := tx.WithContext(ctx).Raw(
			`SELECT id FROM instances i
			 WHERE i.id <> ? AND i.status <> 'deleted'
			   AND (i.ipv6 = ? OR EXISTS (SELECT 1 FROM ip_addresses a WHERE a.instance_id = i.id AND a.address = ? AND a.type = 'inet6'))
			 LIMIT 1`,
			target.ID, *address, *address,
		).Scan(&conflictID).Error; err != nil {
			return err
		}
	}
	if conflictID != 0 {
		return fmt.Errorf("IP address %s is already assigned to instance %d", *address, conflictID)
	}

	var existingPrimary models.IpAddress
	err := tx.WithContext(ctx).
		Where("instance_id = ? AND type = ? AND is_primary = true AND is_custom = false", target.ID, addrType).
		Order("id ASC").First(&existingPrimary).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	var primaryID int32
	if err == nil {
		primaryID = existingPrimary.ID
		if err := tx.WithContext(ctx).Model(&models.IpAddress{}).Where("id = ?", primaryID).Updates(map[string]any{
			"address":    *address,
			"type":       addrType,
			"is_primary": true,
			"is_custom":  false,
			"device":     device,
			"host_id":    hostID,
		}).Error; err != nil {
			return err
		}
	} else {
		rec := models.IpAddress{
			HostID:     hostID,
			InstanceID: target.ID,
			Address:    *address,
			Type:       addrType,
			Device:     device,
			IsPrimary:  true,
			IsCustom:   false,
		}
		if err := tx.WithContext(ctx).Create(&rec).Error; err != nil {
			return err
		}
		primaryID = rec.ID
	}

	return tx.WithContext(ctx).
		Where("instance_id = ? AND type = ? AND is_primary = true AND is_custom = false AND id <> ?", target.ID, addrType, primaryID).
		Delete(&models.IpAddress{}).Error
}

// PersistResolvedInstanceNetworkAddresses mirrors persistResolvedInstanceNetworkAddresses.
func PersistResolvedInstanceNetworkAddresses(
	ctx context.Context,
	target InstanceNetworkSyncTarget,
	resolved ResolvedInstanceNetworkAddresses,
	options PersistNetworkAddressesOptions,
) (PersistedInstanceNetworkAddresses, error) {
	oldIPv4 := cloneStringPtr(target.IPv4)
	oldIPv6 := cloneStringPtr(target.IPv6)
	newIPv4 := cloneStringPtr(resolved.IPv4)
	newIPv6 := cloneStringPtr(resolved.IPv6)
	ipv4Changed := !sameStringPtr(oldIPv4, newIPv4)
	ipv6Changed := !sameStringPtr(oldIPv6, newIPv6)

	result := PersistedInstanceNetworkAddresses{
		IPv4Changed: ipv4Changed,
		OldIPv4:     oldIPv4,
		NewIPv4:     newIPv4,
		IPv6Changed: ipv6Changed,
		OldIPv6:     oldIPv6,
		NewIPv6:     newIPv6,
	}

	if db.DB == nil {
		return result, errors.New("数据库未初始化")
	}

	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var status string
		if err := tx.Raw(`SELECT status::text FROM instances WHERE id = ? LIMIT 1`, target.ID).Scan(&status).Error; err != nil {
			return err
		}
		if status == "" || status == "deleted" {
			return nil
		}

		updates := map[string]any{}
		if ipv4Changed {
			updates["ipv4"] = newIPv4
		}
		if ipv6Changed {
			updates["ipv6"] = newIPv6
		}
		if options.UpdateLastSyncedAt {
			updates["last_synced_at"] = time.Now().UTC()
		}
		if len(updates) > 0 {
			if err := tx.Model(&models.Instance{}).
				Where("id = ? AND status <> 'deleted'", target.ID).
				Updates(updates).Error; err != nil {
				return err
			}
		}
		if err := syncPrimaryIPAddressTx(ctx, tx, target, models.IpTypeInet4, newIPv4, resolved.IPv4Device); err != nil {
			return err
		}
		return syncPrimaryIPAddressTx(ctx, tx, target, models.IpTypeInet6, newIPv6, resolved.IPv6Device)
	})
	return result, err
}
