// Port of server/src/lib/instance-port-mapping.ts, plus the three collaborators it
// pulls in that had no Go home yet:
//
//   - server/src/lib/remote-port.ts      → InstancePortMappingResolveRemotePort
//   - server/src/lib/proxy/*             → the LXC/KVM proxy-device strategies
//   - the pure half of db/port-mappings.ts allocatePort / allocatePorts
//
// The split follows WAVE_D_SPEC §4.1 "占位-执行-确认": every decision that can be
// made without I/O (which ports are candidates, which Incus device config a
// mapping becomes, whether a mapping should be created at all) is a pure function
// tested here; the DB and Incus calls sit behind seams at the edge.
//
// Why the allocation helpers live in lib and not in db: server/src/db/port-mappings.ts
// is not ported yet, and the instances wave will own that file. These helpers take
// the already-loaded used-port set, so db/port_mappings.go can call them without a
// second copy of the boundary logic. Nothing here touches local time — allocation is
// ordered purely by the port range, so a UTC+8 host and a UTC host produce the same
// candidate list.
package lib

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// InstancePortMappingProtocol mirrors PortMappingProtocol.
type InstancePortMappingProtocol = models.Protocol

// Remote port constants, mirroring REMOTE_PORT_SSH / REMOTE_PORT_RDP.
const (
	InstancePortMappingRemotePortSSH int32 = 22
	InstancePortMappingRemotePortRDP int32 = 3389
)

var instancePortMappingWindowsImagePattern = regexp.MustCompile(`(?i)windows|win(?:10|11|server|2019|2022|2025)`)

// InstancePortMappingResolveRemotePort mirrors resolveRemotePrivatePort.
//
// Unrecognised images fall back to SSH on purpose: adding port 22 to a Linux image
// is at worst redundant, while sending 3389 to a Linux box is a mapping that can
// never work.
func InstancePortMappingResolveRemotePort(image string) int32 {
	if instancePortMappingWindowsImagePattern.MatchString(image) {
		return InstancePortMappingRemotePortRDP
	}
	return InstancePortMappingRemotePortSSH
}

// ==================== Port allocation (db/port-mappings.ts) ====================

// InstancePortMappingAllocateCandidates mirrors the candidate collection shared by
// allocatePort and allocatePorts: walk [portStart, portEnd] inclusive, ascending,
// and keep every port that is not already used. Excluded ports are merged into the
// used set first, which is how the TypeScript applies `excludedPorts`.
//
// A nil/empty range (start or end unset, or start > end) yields no candidates, which
// is what `if (!host.nat_port_start || !host.nat_port_end) return null` does upstream.
func InstancePortMappingAllocateCandidates(portStart, portEnd int32, used map[int32]struct{}, excluded []int32) []int32 {
	candidates := make([]int32, 0)
	if portStart == 0 || portEnd == 0 || portEnd < portStart {
		return candidates
	}

	blocked := make(map[int32]struct{}, len(used)+len(excluded))
	for port := range used {
		blocked[port] = struct{}{}
	}
	for _, port := range excluded {
		blocked[port] = struct{}{}
	}

	for port := portStart; port <= portEnd; port++ {
		if _, isUsed := blocked[port]; isUsed {
			continue
		}
		candidates = append(candidates, port)
	}
	return candidates
}

// InstancePortMappingRandomIndex mirrors `Math.floor(Math.random() * length)`.
//
// random is injected so tests can pin the selection; nil means math/rand. The
// clamp keeps a misbehaving generator (a test returning 1.0) inside the slice.
func InstancePortMappingRandomIndex(length int, random func() float64) int {
	if length <= 0 {
		return -1
	}
	if random == nil {
		random = rand.Float64
	}
	index := int(math.Floor(random() * float64(length)))
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}

// InstancePortMappingSelectPort mirrors the tail of allocatePort: pick one of the
// candidates at random. ok is false when the pool is exhausted (allocatePort
// returns null).
func InstancePortMappingSelectPort(candidates []int32, random func() float64) (int32, bool) {
	index := InstancePortMappingRandomIndex(len(candidates), random)
	if index < 0 {
		return 0, false
	}
	return candidates[index], true
}

// InstancePortMappingShuffle mirrors the in-place Fisher-Yates loop the TypeScript
// uses (`for (let i = len - 1; i > 0; i--)`), including the descending direction so
// a pinned RNG produces the same permutation.
func InstancePortMappingShuffle(values []int32, random func() float64) {
	if random == nil {
		random = rand.Float64
	}
	for i := len(values) - 1; i > 0; i-- {
		j := int(math.Floor(random() * float64(i+1)))
		if j < 0 {
			j = 0
		}
		if j > i {
			j = i
		}
		values[i], values[j] = values[j], values[i]
	}
}

// InstancePortMappingAllocatePorts mirrors db.allocatePorts.
//
// When fewer candidates than `count` exist, the TypeScript returns all of them
// (shuffled); otherwise it returns the first `count` of a shuffled copy.
func InstancePortMappingAllocatePorts(candidates []int32, count int, random func() float64) []int32 {
	if count <= 0 {
		return []int32{}
	}
	if len(candidates) < count {
		shuffled := append([]int32{}, candidates...)
		InstancePortMappingShuffle(shuffled, random)
		return shuffled
	}
	shuffled := append([]int32{}, candidates...)
	InstancePortMappingShuffle(shuffled, random)
	return shuffled[:count]
}

// ==================== Proxy device strategies ====================

// InstancePortMappingDeviceConfig mirrors ProxyDeviceConfig. The field order is the
// JSON order the TypeScript object literals produce (type, listen, connect, nat).
type InstancePortMappingDeviceConfig struct {
	Type    string `json:"type"`
	Listen  string `json:"listen"`
	Connect string `json:"connect"`
	Nat     string `json:"nat,omitempty"`
}

// InstancePortMappingNamedDevice mirrors NamedProxyDeviceConfig.
type InstancePortMappingNamedDevice struct {
	// NameSuffix distinguishes the IPv6 device of a dual-stack mapping ("-v6").
	NameSuffix   string
	DeviceConfig InstancePortMappingDeviceConfig
}

// InstancePortMappingProxyResult mirrors ProxyDeviceResult.
type InstancePortMappingProxyResult struct {
	Success       bool
	ErrorMessage  string
	DeviceConfig  *InstancePortMappingDeviceConfig
	DeviceConfigs []InstancePortMappingNamedDevice
}

// Error messages returned by the strategies. They are surfaced verbatim to the API
// caller (the route passes detail through), so they are not translated.
const (
	instancePortMappingLxcMissingIPv6Error = "当前所在节点暂无任何公网 IPv6 记录！请通知管理员在节点设置中补充【公网 IPv6 或 NAT IPv6 地址】，否则无法穿透映射。"
	instancePortMappingKvmNoAddressError   = "Current host has no usable NAT public IPv4 or IPv6 address for KVM port mapping."
	instancePortMappingKvmMissingIPv6Error = "Current host has no public IPv6/NAT IPv6 configured for KVM port mapping."
	instancePortMappingKvmMissingIPv4Error = "当前宿主机没有可用于 KVM 端口映射的本机 IPv4 监听地址。请在节点 NAT 设置中配置本机监听 IPv4（nat_bind_ip），通常填写宿主机网卡上的公网或内网可绑定 IPv4。"
	instancePortMappingFactoryTruncated    = "底层代理生成异常被工厂截断"
)

// instancePortMappingIPv4NatNetworkModes mirrors IPV4_NAT_NETWORK_MODES.
var instancePortMappingIPv4NatNetworkModes = map[string]bool{
	"nat":          true,
	"nat_ipv6":     true,
	"nat_ipv6_nat": true,
}

// InstancePortMappingIsConcreteHostIPv4 mirrors isConcreteHostIpv4: an address that
// exists and is not the wildcard. nat=true in Incus requires a concrete listener.
func InstancePortMappingIsConcreteHostIPv4(address *string) bool {
	return address != nil && *address != "" && *address != "0.0.0.0"
}

// InstancePortMappingCreateProxyDevice mirrors ProxyStrategyFactory.getStrategy(...)
// .createProxyDevice(...).
//
// instanceType is 'virtual-machine'/'kvm' for KVM, anything else for LXC.
// hostNatIP is the *bindable* IPv4 the caller resolved with
// SelectBindableIpv4ListenAddress (the TypeScript passes that as hostNatIp).
func InstancePortMappingCreateProxyDevice(
	instanceType string,
	hostNatIP *string,
	hostIPv6 *string,
	networkMode string,
	protocol InstancePortMappingProtocol,
	publicPort int32,
	privatePort int32,
) InstancePortMappingProxyResult {
	if instanceType == "virtual-machine" || instanceType == "kvm" {
		return instancePortMappingKvmCreateProxyDevice(hostNatIP, hostIPv6, networkMode, protocol, publicPort, privatePort)
	}
	return instancePortMappingLxcCreateProxyDevice(hostNatIP, hostIPv6, networkMode, protocol, publicPort, privatePort)
}

// instancePortMappingLxcCreateProxyDevice mirrors LxcProxyStrategy.
func instancePortMappingLxcCreateProxyDevice(
	hostNatIP *string,
	hostIPv6 *string,
	networkMode string,
	protocol InstancePortMappingProtocol,
	publicPort int32,
	privatePort int32,
) InstancePortMappingProxyResult {
	p := string(protocol)

	if networkMode == "nat_ipv6_nat" {
		// Dual stack: IPv4 via kernel NAT (no forkproxy process), IPv6 via forkproxy.
		if InstancePortMappingIsConcreteHostIPv4(hostNatIP) {
			deviceConfigs := []InstancePortMappingNamedDevice{{
				DeviceConfig: InstancePortMappingDeviceConfig{
					Type:    "proxy",
					Listen:  fmt.Sprintf("%s:%s:%d", p, *hostNatIP, publicPort),
					Connect: fmt.Sprintf("%s:0.0.0.0:%d", p, privatePort),
					Nat:     "true",
				},
			}}
			if hostIPv6 != nil && *hostIPv6 != "" {
				deviceConfigs = append(deviceConfigs, InstancePortMappingNamedDevice{
					NameSuffix: "-v6",
					DeviceConfig: InstancePortMappingDeviceConfig{
						Type:    "proxy",
						Listen:  fmt.Sprintf("%s:[%s]:%d", p, *hostIPv6, publicPort),
						Connect: fmt.Sprintf("%s:0.0.0.0:%d", p, privatePort),
					},
				})
			}
			return InstancePortMappingProxyResult{Success: true, DeviceConfigs: deviceConfigs}
		}

		// No concrete IPv4: fall back to the single [::] dual-stack listener.
		return InstancePortMappingProxyResult{
			Success: true,
			DeviceConfig: &InstancePortMappingDeviceConfig{
				Type:    "proxy",
				Listen:  fmt.Sprintf("%s:[::]:%d", p, publicPort),
				Connect: fmt.Sprintf("%s:0.0.0.0:%d", p, privatePort),
			},
		}
	}

	var listenAddr string
	if networkMode == "ipv6_only" || networkMode == "ipv6_nat" {
		if hostIPv6 == nil || *hostIPv6 == "" {
			return InstancePortMappingProxyResult{Success: false, ErrorMessage: instancePortMappingLxcMissingIPv6Error}
		}
		listenAddr = "[" + *hostIPv6 + "]"
	} else {
		if hostNatIP != nil && *hostNatIP != "" {
			listenAddr = *hostNatIP
		} else {
			listenAddr = "0.0.0.0"
		}
	}

	connectAddr := "0.0.0.0"
	if networkMode == "ipv6_only" || networkMode == "ipv6_nat" {
		connectAddr = "[::]"
	}

	deviceConfig := InstancePortMappingDeviceConfig{
		Type:    "proxy",
		Listen:  fmt.Sprintf("%s:%s:%d", p, listenAddr, publicPort),
		Connect: fmt.Sprintf("%s:%s:%d", p, connectAddr, privatePort),
	}
	if instancePortMappingIPv4NatNetworkModes[networkMode] && InstancePortMappingIsConcreteHostIPv4(hostNatIP) {
		deviceConfig.Nat = "true"
	}

	return InstancePortMappingProxyResult{Success: true, DeviceConfig: &deviceConfig}
}

// instancePortMappingKvmCreateProxyDevice mirrors KvmProxyStrategy.
func instancePortMappingKvmCreateProxyDevice(
	hostNatIP *string,
	hostIPv6 *string,
	networkMode string,
	protocol InstancePortMappingProtocol,
	publicPort int32,
	privatePort int32,
) InstancePortMappingProxyResult {
	p := string(protocol)

	if networkMode == "nat_ipv6_nat" {
		deviceConfigs := make([]InstancePortMappingNamedDevice, 0, 2)
		if hostNatIP != nil && *hostNatIP != "" && *hostNatIP != "0.0.0.0" {
			deviceConfigs = append(deviceConfigs, InstancePortMappingNamedDevice{
				DeviceConfig: InstancePortMappingDeviceConfig{
					Type:    "proxy",
					Listen:  fmt.Sprintf("%s:%s:%d", p, *hostNatIP, publicPort),
					Connect: fmt.Sprintf("%s:0.0.0.0:%d", p, privatePort),
					Nat:     "true",
				},
			})
		}
		if hostIPv6 != nil && *hostIPv6 != "" {
			deviceConfigs = append(deviceConfigs, InstancePortMappingNamedDevice{
				NameSuffix: "-v6",
				DeviceConfig: InstancePortMappingDeviceConfig{
					Type:    "proxy",
					Listen:  fmt.Sprintf("%s:[%s]:%d", p, *hostIPv6, publicPort),
					Connect: fmt.Sprintf("%s:0.0.0.0:%d", p, privatePort),
				},
			})
		}
		if len(deviceConfigs) == 0 {
			return InstancePortMappingProxyResult{Success: false, ErrorMessage: instancePortMappingKvmNoAddressError}
		}
		return InstancePortMappingProxyResult{Success: true, DeviceConfigs: deviceConfigs}
	}

	var listenAddr string
	if networkMode == "ipv6_only" || networkMode == "ipv6_nat" {
		if hostIPv6 == nil || *hostIPv6 == "" {
			return InstancePortMappingProxyResult{Success: false, ErrorMessage: instancePortMappingKvmMissingIPv6Error}
		}
		listenAddr = "[" + *hostIPv6 + "]"
	} else {
		if hostNatIP == nil || *hostNatIP == "" || *hostNatIP == "0.0.0.0" {
			return InstancePortMappingProxyResult{Success: false, ErrorMessage: instancePortMappingKvmMissingIPv4Error}
		}
		listenAddr = *hostNatIP
	}

	connectAddr := "0.0.0.0"
	if networkMode == "ipv6_only" {
		connectAddr = "[::]"
	}

	deviceConfig := InstancePortMappingDeviceConfig{
		Type:    "proxy",
		Listen:  fmt.Sprintf("%s:%s:%d", p, listenAddr, publicPort),
		Connect: fmt.Sprintf("%s:%s:%d", p, connectAddr, privatePort),
	}
	if networkMode != "ipv6_nat" {
		deviceConfig.Nat = "true"
	}

	return InstancePortMappingProxyResult{Success: true, DeviceConfig: &deviceConfig}
}

// ==================== Seams: DB writes and the Incus device calls ====================

// ErrInstancePortMappingBackendUnavailable is returned by the default backend. It
// exists because server/src/db/port-mappings.ts (allocatePort, checkPortInUse,
// createPortMapping, deletePortMapping) has not been ported: the instances wave owns
// that file. Failing loudly is the only honest option — inventing a local copy would
// fork the port-reservation rules from the code that owns them.
var ErrInstancePortMappingBackendUnavailable = errors.New("db/port-mappings.ts 尚未移植：端口预留在 InstancePortMappingBackend 上注册后可用")

// InstancePortMappingCreateInput mirrors the createPortMapping argument object.
type InstancePortMappingCreateInput struct {
	InstanceID    int32
	HostID        int32
	Protocol      InstancePortMappingProtocol
	PublicPort    int32
	PrivatePort   int32
	Remark        *string
	SystemManaged bool
}

// InstancePortMappingBackend is the four DB writes the mapping flow needs.
type InstancePortMappingBackend interface {
	// AllocatePort mirrors db.allocatePort: ok=false when the host pool is exhausted.
	AllocatePort(ctx context.Context, hostID int32, protocol InstancePortMappingProtocol) (int32, bool, error)
	// CheckPortInUse mirrors db.checkPortInUse.
	CheckPortInUse(ctx context.Context, hostID int32, port int32, protocol InstancePortMappingProtocol) (bool, error)
	// CreatePortMapping mirrors db.createPortMapping (returns the new row id).
	CreatePortMapping(ctx context.Context, input InstancePortMappingCreateInput) (int32, error)
	// DeletePortMapping mirrors db.deletePortMapping.
	DeletePortMapping(ctx context.Context, id int32) error
}

type unavailableInstancePortMappingBackend struct{}

func (unavailableInstancePortMappingBackend) AllocatePort(context.Context, int32, InstancePortMappingProtocol) (int32, bool, error) {
	return 0, false, ErrInstancePortMappingBackendUnavailable
}

func (unavailableInstancePortMappingBackend) CheckPortInUse(context.Context, int32, int32, InstancePortMappingProtocol) (bool, error) {
	return false, ErrInstancePortMappingBackendUnavailable
}

func (unavailableInstancePortMappingBackend) CreatePortMapping(context.Context, InstancePortMappingCreateInput) (int32, error) {
	return 0, ErrInstancePortMappingBackendUnavailable
}

func (unavailableInstancePortMappingBackend) DeletePortMapping(context.Context, int32) error {
	return ErrInstancePortMappingBackendUnavailable
}

var instancePortMappingBackend InstancePortMappingBackend = unavailableInstancePortMappingBackend{}

// RegisterInstancePortMappingBackend installs the port-mapping data layer. Passing
// nil restores the failing default (used by tests).
func RegisterInstancePortMappingBackend(backend InstancePortMappingBackend) {
	if backend == nil {
		instancePortMappingBackend = unavailableInstancePortMappingBackend{}
		return
	}
	instancePortMappingBackend = backend
}

// InstancePortMappingClient is the slice of the Incus instance API the mapping flow
// uses (addDevice / removeDevice of incus-instances.ts).
type InstancePortMappingClient interface {
	AddDevice(ctx context.Context, instanceName, deviceName string, device map[string]string) error
	RemoveDevice(ctx context.Context, instanceName, deviceName string) error
}

// InstancePortMappingClientFactory builds the client for a host. It is a variable so
// tests can record device calls instead of talking to a node.
var InstancePortMappingClientFactory = defaultInstancePortMappingClient

// defaultInstancePortMappingClient mirrors `getIncusClient(host)`.
func defaultInstancePortMappingClient(ctx context.Context, host *InstancePortMappingHost) (InstancePortMappingClient, error) {
	client, err := GetIncusClient(ctx, IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
	if err != nil {
		return nil, err
	}
	return incusInstancePortMappingClient{client: client}, nil
}

// incusInstancePortMappingClient mirrors addDevice / removeDevice from
// lib/incus/incus-instances.ts: read the instance, edit one device key, PUT the
// whole device map back. Reusing UpdateInstance keeps the config-merge behaviour in
// one place.
type incusInstancePortMappingClient struct {
	client *IncusClient
}

func (c incusInstancePortMappingClient) AddDevice(ctx context.Context, instanceName, deviceName string, device map[string]string) error {
	instance, err := GetInstance(ctx, c.client, instanceName)
	if err != nil {
		return err
	}
	devices := instance.Devices
	if devices == nil {
		devices = map[string]map[string]any{}
	}
	entry := make(map[string]any, len(device))
	for key, value := range device {
		entry[key] = value
	}
	devices[deviceName] = entry
	_, err = UpdateInstance(ctx, c.client, instanceName, map[string]any{"devices": devices})
	return err
}

func (c incusInstancePortMappingClient) RemoveDevice(ctx context.Context, instanceName, deviceName string) error {
	instance, err := GetInstance(ctx, c.client, instanceName)
	if err != nil {
		return err
	}
	if instance.Devices == nil {
		return nil
	}
	if _, ok := instance.Devices[deviceName]; !ok {
		return nil
	}
	devices := make(map[string]map[string]any, len(instance.Devices))
	for name, config := range instance.Devices {
		if name == deviceName {
			continue
		}
		devices[name] = config
	}
	_, err = UpdateInstance(ctx, c.client, instanceName, map[string]any{"devices": devices})
	return err
}

// ==================== Seams: read-side lookups ====================

// InstancePortMappingHost is the host projection the mapping logic needs. It
// mirrors the columns the TypeScript reads off the Host row, and keeps the mapping
// logic independent of the db record shape so it can be tested without a database.
type InstancePortMappingHost struct {
	ID            int32
	URL           string
	CertPath      *string
	KeyPath       *string
	TargetHost    string
	TargetPort    int32
	TunnelEnabled bool
	IPAddress     *string
	IPv6Gateway   *string
	NATPublicIP   *string
	NATPublicIPv6 *string
	NATBindIP     *string
	NATBindIPv6   *string
	NATPortStart  *int32
	NATPortEnd    *int32
}

// InstancePortMappingHostFromRecord projects a db host row.
func InstancePortMappingHostFromRecord(record *db.HostsRecord) *InstancePortMappingHost {
	if record == nil {
		return nil
	}
	return &InstancePortMappingHost{
		ID:            record.ID,
		URL:           record.URL,
		CertPath:      record.CertPath,
		KeyPath:       record.KeyPath,
		TargetHost:    record.TargetHost,
		TargetPort:    record.TargetPort,
		TunnelEnabled: record.TunnelEnabled,
		IPAddress:     record.IPAddress,
		IPv6Gateway:   record.IPv6Gateway,
		NATPublicIP:   record.NATPublicIP,
		NATPublicIPv6: record.NATPublicIPv6,
		NATBindIP:     record.NATBindIP,
		NATBindIPv6:   record.NATBindIPv6,
		NATPortStart:  record.NATPortStart,
		NATPortEnd:    record.NATPortEnd,
	}
}

// InstancePortMappingInstance is the instance projection (id, host_id, incus_id,
// network_mode, package_id).
type InstancePortMappingInstance struct {
	ID          int32
	HostID      int32
	IncusID     string
	NetworkMode string
	PackageID   *int32
}

// InstancePortMappingInstanceFromDetail projects db.GetInstanceById.
func InstancePortMappingInstanceFromDetail(record *db.InstanceDetailRecord) *InstancePortMappingInstance {
	if record == nil {
		return nil
	}
	return &InstancePortMappingInstance{
		ID:          record.ID,
		HostID:      record.HostID,
		IncusID:     record.IncusID,
		NetworkMode: string(record.NetworkMode),
		PackageID:   record.PackageID,
	}
}

// InstancePortMappingGetHostByID defaults to db.getHostById.
var InstancePortMappingGetHostByID = func(ctx context.Context, hostID int32) (*InstancePortMappingHost, error) {
	record, err := db.HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	return InstancePortMappingHostFromRecord(record), nil
}

// InstancePortMappingGetInstanceByID defaults to db.getInstanceById.
var InstancePortMappingGetInstanceByID = func(ctx context.Context, instanceID int32) (*InstancePortMappingInstance, error) {
	record, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	return InstancePortMappingInstanceFromDetail(record), nil
}

// InstancePortMappingPackageIsVirtualMachine mirrors the inline package lookup in
// addInstancePortMapping: the package row decides whether the instance is a VM
// (params.type is not guaranteed to be in sync).
var InstancePortMappingPackageIsVirtualMachine = func(ctx context.Context, packageID int32) (bool, error) {
	if db.DB == nil {
		return false, errors.New("database not initialised")
	}
	var pkg models.Package
	err := db.DB.WithContext(ctx).Select("instance_type").Where("id = ?", packageID).Take(&pkg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// `!!pkg && ...` — a missing package means "not a VM".
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return pkg.InstanceType == models.InstanceTypeVm, nil
}

// InstancePortMappingHostIPv6Alias mirrors the hostAddressAlias.findFirst fallback.
var InstancePortMappingHostIPv6Alias = func(ctx context.Context, hostID int32) (*string, error) {
	if db.DB == nil {
		return nil, errors.New("database not initialised")
	}
	var alias models.HostAddressAlias
	err := db.DB.WithContext(ctx).Where("host_id = ? AND kind = ?", hostID, string(models.HostAddressKindIpv6)).Take(&alias).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &alias.Address, nil
}

// InstancePortMappingIsAutoRemotePortEnabled mirrors db.isInstanceAutoRemotePortEnabled
// (system config `instance_auto_remote_port`, default true).
var InstancePortMappingIsAutoRemotePortEnabled = func(ctx context.Context) (bool, error) {
	return db.GetSystemConfigBoolean(ctx, "instance_auto_remote_port", true)
}

// ==================== The mapping flow ====================

// InstancePortMappingMapping is the success payload.
type InstancePortMappingMapping struct {
	ID          int32                       `json:"id"`
	Protocol    InstancePortMappingProtocol `json:"protocol"`
	PublicPort  int32                       `json:"publicPort"`
	PrivatePort int32                       `json:"privatePort"`
	Remark      *string                     `json:"remark"`
}

// InstancePortMappingAddResult mirrors AddPortMappingResult: either {ok:true,
// mapping} or {ok:false, code, detail?, message?}.
type InstancePortMappingAddResult struct {
	Ok      bool                        `json:"ok"`
	Mapping *InstancePortMappingMapping `json:"mapping,omitempty"`
	Code    ErrorCode                   `json:"code,omitempty"`
	Detail  *string                     `json:"detail,omitempty"`
	Message *string                     `json:"message,omitempty"`
}

// InstancePortMappingAddInput mirrors AddPortMappingInput.
type InstancePortMappingAddInput struct {
	Instance      InstancePortMappingInstance
	Host          *InstancePortMappingHost
	Protocol      InstancePortMappingProtocol
	PrivatePort   int32
	PublicPort    *int32
	Remark        *string
	SystemManaged bool
}

func instancePortMappingFailure(code ErrorCode, detail *string) InstancePortMappingAddResult {
	return InstancePortMappingAddResult{Ok: false, Code: code, Detail: detail}
}

// instancePortMappingIsUniqueViolation mirrors isUniqueConstraintError (Prisma
// P2002 becomes PostgreSQL 23505 on the wire).
func instancePortMappingIsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key value") || strings.Contains(message, "unique constraint")
}

// instancePortMappingFirstNonEmpty mirrors the `a || b || c` chain over optional
// strings: the first value that is present and non-empty wins.
func instancePortMappingFirstNonEmpty(values ...*string) *string {
	for _, value := range values {
		if value != nil && *value != "" {
			return value
		}
	}
	return nil
}

// instancePortMappingTrimmedRemark mirrors `remark?.trim() || null`.
func instancePortMappingTrimmedRemark(remark *string) *string {
	if remark == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*remark)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// AddInstancePortMapping mirrors addInstancePortMapping.
//
// Safety sequence (pinned by server/src/test-port-mapping-safety):
//  1. reserve in the DB only after checkPortInUse says the port is free;
//  2. on a device failure remove only the device names this call created, never a
//     generic name that may belong to another request;
//  3. the DB reservation is deleted during rollback, leaving no dangling mapping.
//
// The caller owns the per-instance port lock and, for manual adds, the quota check.
//
// Errors: the TypeScript lets the allocate/check failures reject (they sit outside
// its try/catch), and so does this port — everything from the Incus call onwards is
// folded into the result, exactly like the catch block.
func AddInstancePortMapping(ctx context.Context, input InstancePortMappingAddInput) (InstancePortMappingAddResult, error) {
	instance := input.Instance
	host := input.Host
	backend := instancePortMappingBackend

	if host == nil {
		return instancePortMappingFailure(ErrHostNotFound, nil), nil
	}
	if instance.NetworkMode == "ipv6_only" {
		detail := "IPv6 Only instances do not require port mappings"
		return instancePortMappingFailure(ErrInvalidParams, &detail), nil
	}

	var allocatedPort int32
	if input.PublicPort != nil && *input.PublicPort != 0 {
		allocatedPort = *input.PublicPort
		if host.NATPortStart != nil && *host.NATPortStart != 0 && host.NATPortEnd != nil && *host.NATPortEnd != 0 {
			if allocatedPort < *host.NATPortStart || allocatedPort > *host.NATPortEnd {
				detail := fmt.Sprintf("%d-%d", *host.NATPortStart, *host.NATPortEnd)
				return instancePortMappingFailure(ErrPortRangeInvalid, &detail), nil
			}
		}
	} else {
		port, ok, err := backend.AllocatePort(ctx, instance.HostID, input.Protocol)
		if err != nil {
			return InstancePortMappingAddResult{}, err
		}
		if !ok {
			return instancePortMappingFailure(ErrPortNoAvailable, nil), nil
		}
		allocatedPort = port
	}

	inUse, err := backend.CheckPortInUse(ctx, instance.HostID, allocatedPort, input.Protocol)
	if err != nil {
		return InstancePortMappingAddResult{}, err
	}
	if inUse {
		return instancePortMappingFailure(ErrPortInUse, nil), nil
	}

	createdDeviceNames := make([]string, 0, 2)
	var reservedMappingID *int32
	mappingRemark := instancePortMappingTrimmedRemark(input.Remark)

	rollback := func() {
		// The TypeScript re-reads the host before rolling back: the caller's row may
		// be stale, and the device removal has to reach the node the instance is on.
		rollbackHost, hostErr := InstancePortMappingGetHostByID(ctx, instance.HostID)
		if hostErr != nil {
			return
		}
		if rollbackHost != nil {
			rollbackClient, clientErr := InstancePortMappingClientFactory(ctx, rollbackHost)
			if clientErr != nil {
				// Same as the TypeScript: a failed client acquisition aborts the rest
				// of the rollback, including the DB cleanup below.
				return
			}
			for _, createdDeviceName := range createdDeviceNames {
				if removeErr := rollbackClient.RemoveDevice(ctx, instance.IncusID, createdDeviceName); removeErr != nil {
					return
				}
			}
		}
		if reservedMappingID != nil {
			_ = backend.DeletePortMapping(ctx, *reservedMappingID)
		}
	}

	earlyResult, caughtErr := instancePortMappingProvision(ctx, instance, host, input, allocatedPort, mappingRemark, backend, &createdDeviceNames, &reservedMappingID)
	if earlyResult != nil {
		// A proxy-device generation failure is reported from inside the "try" block of
		// the TypeScript, i.e. before the reservation, so it surfaces as INVALID_PARAMS
		// and nothing needs to be rolled back.
		return *earlyResult, nil
	}
	if caughtErr != nil {
		rollback()
		if instancePortMappingIsUniqueViolation(caughtErr) {
			return instancePortMappingFailure(ErrPortInUse, nil), nil
		}
		message := caughtErr.Error()
		return InstancePortMappingAddResult{Ok: false, Code: ErrInternalError, Message: &message}, nil
	}

	return InstancePortMappingAddResult{
		Ok: true,
		Mapping: &InstancePortMappingMapping{
			ID:          *reservedMappingID,
			Protocol:    input.Protocol,
			PublicPort:  allocatedPort,
			PrivatePort: input.PrivatePort,
			Remark:      mappingRemark,
		},
	}, nil
}

// instancePortMappingProvision is the body of the TypeScript try block.
//
// It returns (earlyResult, err): earlyResult carries the INVALID_PARAMS branch that
// the TypeScript also returns from inside the try (no rollback), err carries anything
// else (rollback + INTERNAL_ERROR/PORT_IN_USE). Both nil means success.
func instancePortMappingProvision(
	ctx context.Context,
	instance InstancePortMappingInstance,
	host *InstancePortMappingHost,
	input InstancePortMappingAddInput,
	allocatedPort int32,
	mappingRemark *string,
	backend InstancePortMappingBackend,
	createdDeviceNames *[]string,
	reservedMappingID **int32,
) (*InstancePortMappingAddResult, error) {
	client, err := InstancePortMappingClientFactory(ctx, host)
	if err != nil {
		return nil, err
	}

	deviceName := fmt.Sprintf("proxy-%s-%d", input.Protocol, allocatedPort)

	// The real instance category comes from the package (params.type is not
	// guaranteed to be in sync).
	isVM := false
	if instance.PackageID != nil {
		isVM, err = InstancePortMappingPackageIsVirtualMachine(ctx, *instance.PackageID)
		if err != nil {
			return nil, err
		}
	}
	actualInstanceType := "container"
	if isVM {
		actualInstanceType = "virtual-machine"
	}

	var ipv6FromHost *string
	if host.IPAddress != nil && strings.Contains(*host.IPAddress, ":") {
		ipv6FromHost = host.IPAddress
	}
	explicitIPv6 := instancePortMappingFirstNonEmpty(host.NATBindIPv6, host.NATPublicIPv6, host.IPv6Gateway, ipv6FromHost)
	if explicitIPv6 == nil {
		// The TypeScript wraps this lookup in try/catch: a failure just keeps the
		// fallback computed above.
		alias, aliasErr := InstancePortMappingHostIPv6Alias(ctx, host.ID)
		if aliasErr == nil && alias != nil && *alias != "" {
			explicitIPv6 = alias
		}
	}

	bindableIPv4 := SelectBindableIpv4ListenAddress(host.NATBindIP, host.NATPublicIP, &host.URL, host.IPAddress)
	proxyDeviceRes := InstancePortMappingCreateProxyDevice(
		actualInstanceType, &bindableIPv4, explicitIPv6, instance.NetworkMode,
		input.Protocol, allocatedPort, input.PrivatePort,
	)

	deviceConfigs := proxyDeviceRes.DeviceConfigs
	if len(deviceConfigs) == 0 && proxyDeviceRes.DeviceConfig != nil {
		deviceConfigs = []InstancePortMappingNamedDevice{{DeviceConfig: *proxyDeviceRes.DeviceConfig}}
	}
	if !proxyDeviceRes.Success || len(deviceConfigs) == 0 {
		detail := proxyDeviceRes.ErrorMessage
		if detail == "" {
			detail = instancePortMappingFactoryTruncated
		}
		return &InstancePortMappingAddResult{Ok: false, Code: ErrInvalidParams, Detail: &detail}, nil
	}

	id, err := backend.CreatePortMapping(ctx, InstancePortMappingCreateInput{
		InstanceID:    instance.ID,
		HostID:        instance.HostID,
		Protocol:      input.Protocol,
		PublicPort:    allocatedPort,
		PrivatePort:   input.PrivatePort,
		Remark:        mappingRemark,
		SystemManaged: input.SystemManaged,
	})
	if err != nil {
		return nil, err
	}
	*reservedMappingID = &id

	for _, entry := range deviceConfigs {
		resolvedDeviceName := deviceName + entry.NameSuffix
		if addErr := client.AddDevice(ctx, instance.IncusID, resolvedDeviceName, entry.DeviceConfig.asMap()); addErr != nil {
			return nil, addErr
		}
		*createdDeviceNames = append(*createdDeviceNames, resolvedDeviceName)
	}

	return nil, nil
}

// asMap flattens a device config into the Incus device map.
func (c InstancePortMappingDeviceConfig) asMap() map[string]string {
	device := map[string]string{
		"type":    c.Type,
		"listen":  c.Listen,
		"connect": c.Connect,
	}
	if c.Nat != "" {
		device["nat"] = c.Nat
	}
	return device
}

// ==================== Auto remote port on instance creation ====================

// InstancePortMappingAutoRemotePortInput mirrors provisionAutoRemotePort's argument.
type InstancePortMappingAutoRemotePortInput struct {
	InstanceID  int32
	Image       string
	NetworkMode string
	// AutoRemotePort is a three-state flag: nil means "not supplied" and behaves like
	// true; only an explicit false suppresses the mapping.
	AutoRemotePort *bool
}

// ProvisionAutoRemotePort mirrors provisionAutoRemotePort.
//
// Creates the automatic remote mapping for a new instance: Linux → 22 (SSH),
// Windows → 3389 (RDP). Three owner-decided constraints:
//
//   - it does not count against the user's port quota (systemManaged=true);
//   - only new instances get it, existing ones are never backfilled;
//   - it needs both the global `instance_auto_remote_port` switch and the create-time
//     checkbox, so either one being off suppresses it.
//
// Failures must never affect instance creation — the instance is already running and
// the user can add a mapping by hand. That is why this returns nothing and only logs.
func ProvisionAutoRemotePort(ctx context.Context, input InstancePortMappingAutoRemotePortInput) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[AutoRemotePort] failed for instance %d: %v", input.InstanceID, recovered)
		}
	}()

	if input.AutoRemotePort != nil && !*input.AutoRemotePort {
		return
	}
	if !NetworkModeAllowsPortMapping(input.NetworkMode) {
		return
	}

	enabled, err := InstancePortMappingIsAutoRemotePortEnabled(ctx)
	if err != nil {
		log.Printf("[AutoRemotePort] failed for instance %d: %v", input.InstanceID, err)
		return
	}
	if !enabled {
		return
	}

	instance, err := InstancePortMappingGetInstanceByID(ctx, input.InstanceID)
	if err != nil {
		log.Printf("[AutoRemotePort] failed for instance %d: %v", input.InstanceID, err)
		return
	}
	if instance == nil {
		return
	}

	host, err := InstancePortMappingGetHostByID(ctx, instance.HostID)
	if err != nil {
		log.Printf("[AutoRemotePort] failed for instance %d: %v", input.InstanceID, err)
		return
	}
	if host == nil {
		return
	}

	privatePort := InstancePortMappingResolveRemotePort(input.Image)
	result, err := AddInstancePortMapping(ctx, InstancePortMappingAddInput{
		Instance:      *instance,
		Host:          host,
		Protocol:      models.ProtocolTcp,
		PrivatePort:   privatePort,
		SystemManaged: true,
	})
	if err != nil {
		log.Printf("[AutoRemotePort] failed for instance %d: %v", input.InstanceID, err)
		return
	}

	if result.Ok && result.Mapping != nil {
		log.Printf("[AutoRemotePort] instance %d: %d -> %d", input.InstanceID, result.Mapping.PublicPort, privatePort)
		return
	}
	log.Printf("[AutoRemotePort] skipped for instance %d: %s", input.InstanceID, result.Code)
}
