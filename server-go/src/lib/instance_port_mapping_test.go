package lib

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== remote port ====================

func TestInstancePortMappingResolveRemotePort(t *testing.T) {
	cases := []struct {
		image string
		want  int32
	}{
		{"ubuntu/noble", 22},
		{"debian/12", 22},
		{"alpine/3.20", 22},
		{"widows2022-typo", 22},
		{"windows/2022", 3389},
		{"Windows10", 3389},
		{"win11-golden", 3389},
		{"WIN2025", 3389},
		{"win2019", 3389},
		{"win10", 3389},
		{"win-server-2016", 22}, // "win" + "server" only matches adjacently
		{"", 22},
	}
	for _, tc := range cases {
		if got := InstancePortMappingResolveRemotePort(tc.image); got != tc.want {
			t.Errorf("InstancePortMappingResolveRemotePort(%q) = %d, want %d", tc.image, got, tc.want)
		}
	}
	if InstancePortMappingRemotePortSSH != 22 || InstancePortMappingRemotePortRDP != 3389 {
		t.Error("remote port constants must stay 22/3389")
	}
}

// ==================== allocation ====================

func TestInstancePortMappingAllocateCandidates(t *testing.T) {
	used := func(ports ...int32) map[int32]struct{} {
		set := make(map[int32]struct{}, len(ports))
		for _, port := range ports {
			set[port] = struct{}{}
		}
		return set
	}
	equal := func(t *testing.T, name string, got, want []int32) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}

	t.Run("empty pool keeps the whole range in ascending order", func(t *testing.T) {
		equal(t, "empty", InstancePortMappingAllocateCandidates(20_000, 20_005, nil, nil),
			[]int32{20_000, 20_001, 20_002, 20_003, 20_004, 20_005})
	})
	t.Run("first port of the segment is allocated", func(t *testing.T) {
		equal(t, "first", InstancePortMappingAllocateCandidates(20_000, 20_002, used(0), nil),
			[]int32{20_000, 20_001, 20_002})
	})
	t.Run("last port of the segment is allocated", func(t *testing.T) {
		equal(t, "last", InstancePortMappingAllocateCandidates(20_000, 20_002, used(20_001), nil),
			[]int32{20_000, 20_002})
	})
	t.Run("occupied ports are skipped", func(t *testing.T) {
		equal(t, "skip", InstancePortMappingAllocateCandidates(20_000, 20_004, used(20_000, 20_002, 20_004), nil),
			[]int32{20_001, 20_003})
	})
	t.Run("a fully occupied segment has no candidates", func(t *testing.T) {
		equal(t, "full", InstancePortMappingAllocateCandidates(20_000, 20_002, used(20_000, 20_001, 20_002), nil),
			[]int32{})
	})
	t.Run("excluded ports behave like occupied ones", func(t *testing.T) {
		equal(t, "excluded", InstancePortMappingAllocateCandidates(20_000, 20_003, nil, []int32{20_000, 20_003}),
			[]int32{20_001, 20_002})
	})
	t.Run("ports outside the segment are ignored", func(t *testing.T) {
		equal(t, "outside", InstancePortMappingAllocateCandidates(20_000, 20_001, used(19_999, 20_002), nil),
			[]int32{20_000, 20_001})
	})
	t.Run("a released port is reusable at its original position", func(t *testing.T) {
		before := InstancePortMappingAllocateCandidates(20_000, 20_003, used(20_000, 20_001, 20_002, 20_003), nil)
		if len(before) != 0 {
			t.Fatalf("exhausted pool should be empty, got %v", before)
		}
		after := InstancePortMappingAllocateCandidates(20_000, 20_003, used(20_000, 20_002, 20_003), nil)
		equal(t, "reuse", after, []int32{20_001})
	})
	t.Run("single port segment", func(t *testing.T) {
		equal(t, "single", InstancePortMappingAllocateCandidates(20_000, 20_000, nil, nil), []int32{20_000})
		equal(t, "single used", InstancePortMappingAllocateCandidates(20_000, 20_000, used(20_000), nil), []int32{})
	})
	t.Run("invalid ranges", func(t *testing.T) {
		// A host without nat_port_start/nat_port_end answers null upstream.
		equal(t, "zero start", InstancePortMappingAllocateCandidates(0, 20_000, nil, nil), []int32{})
		equal(t, "zero end", InstancePortMappingAllocateCandidates(20_000, 0, nil, nil), []int32{})
		equal(t, "inverted", InstancePortMappingAllocateCandidates(20_005, 20_000, nil, nil), []int32{})
	})
}

func TestInstancePortMappingSelectPort(t *testing.T) {
	candidates := []int32{20_000, 20_001, 20_002, 20_003, 20_004}
	cases := []struct {
		random float64
		want   int32
	}{
		{0, 20_000},
		{0.0001, 20_000},
		{0.5, 20_002},    // floor(0.5 * 5) = 2
		{0.9999, 20_004}, // floor(0.9999 * 5) = 4
	}
	for _, tc := range cases {
		got, ok := InstancePortMappingSelectPort(candidates, func() float64 { return tc.random })
		if !ok || got != tc.want {
			t.Errorf("random=%v → (%d, %v), want (%d, true)", tc.random, got, ok, tc.want)
		}
	}
	if _, ok := InstancePortMappingSelectPort(nil, func() float64 { return 0 }); ok {
		t.Error("an empty pool must not allocate")
	}
	if got := InstancePortMappingRandomIndex(0, func() float64 { return 0 }); got != -1 {
		t.Errorf("RandomIndex(0) = %d, want -1", got)
	}
	// A misbehaving generator is clamped instead of panicking.
	if got := InstancePortMappingRandomIndex(3, func() float64 { return 1 }); got != 2 {
		t.Errorf("RandomIndex with 1.0 = %d, want 2", got)
	}
}

func TestInstancePortMappingAllocatePorts(t *testing.T) {
	candidates := func() []int32 { return []int32{20_000, 20_001, 20_002, 20_003} }
	equal := func(t *testing.T, got, want []int32) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	// Fisher-Yates runs from the end: [1,2,3] with rng 0 twice → [2,3,1].
	zeros := func() float64 { return 0 }
	equal(t, InstancePortMappingAllocatePorts([]int32{1, 2, 3}, 3, zeros), []int32{2, 3, 1})
	equal(t, InstancePortMappingAllocatePorts([]int32{1, 2, 3}, 2, zeros), []int32{2, 3})

	// Fewer candidates than requested: everything comes back, shuffled.
	equal(t, InstancePortMappingAllocatePorts([]int32{1, 2, 3}, 5, zeros), []int32{2, 3, 1})

	// Exactly enough candidates.
	equal(t, InstancePortMappingAllocatePorts([]int32{1, 2}, 2, zeros), []int32{2, 1})

	// count <= 0 yields an empty slice, and the input slice is never mutated.
	equal(t, InstancePortMappingAllocatePorts([]int32{1, 2}, 0, zeros), []int32{})
	input := candidates()
	_ = InstancePortMappingAllocatePorts(input, 2, zeros)
	equal(t, input, candidates())

	// An empty pool is not an error.
	equal(t, InstancePortMappingAllocatePorts(nil, 3, zeros), []int32{})
}

// ==================== proxy device generation ====================

func TestInstancePortMappingCreateProxyDevice(t *testing.T) {
	s := func(value string) *string { return &value }
	nat := "true"

	cases := []struct {
		name         string
		instanceType string
		hostNatIP    *string
		hostIPv6     *string
		networkMode  string
		want         InstancePortMappingProxyResult
	}{
		{
			name:      "lxc nat with concrete ipv4 uses kernel NAT",
			hostNatIP: s("203.0.113.7"), networkMode: "nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat,
			}},
		},
		{
			name:      "lxc nat without a concrete ipv4 keeps forkproxy",
			hostNatIP: s("0.0.0.0"), networkMode: "nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:0.0.0.0:20022", Connect: "tcp:0.0.0.0:22",
			}},
		},
		{
			name:      "lxc nat_ipv6 also uses kernel NAT",
			hostNatIP: s("203.0.113.7"), networkMode: "nat_ipv6",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat,
			}},
		},
		{
			name:      "lxc public_ipv4 does not set nat",
			hostNatIP: s("203.0.113.7"), networkMode: "public_ipv4",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22",
			}},
		},
		{
			name:      "lxc dual stack splits into ipv4 NAT plus ipv6 forkproxy",
			hostNatIP: s("203.0.113.7"), hostIPv6: s("2001:db8::1"), networkMode: "nat_ipv6_nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfigs: []InstancePortMappingNamedDevice{
				{DeviceConfig: InstancePortMappingDeviceConfig{Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat}},
				{NameSuffix: "-v6", DeviceConfig: InstancePortMappingDeviceConfig{Type: "proxy", Listen: "tcp:[2001:db8::1]:20022", Connect: "tcp:0.0.0.0:22"}},
			}},
		},
		{
			name:      "lxc dual stack without ipv6 emits only the ipv4 device",
			hostNatIP: s("203.0.113.7"), networkMode: "nat_ipv6_nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfigs: []InstancePortMappingNamedDevice{
				{DeviceConfig: InstancePortMappingDeviceConfig{Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat}},
			}},
		},
		{
			name:      "lxc dual stack without ipv4 falls back to [::]",
			hostNatIP: s("0.0.0.0"), hostIPv6: s("2001:db8::1"), networkMode: "nat_ipv6_nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:[::]:20022", Connect: "tcp:0.0.0.0:22",
			}},
		},
		{
			name:     "lxc ipv6_only binds the v6 address",
			hostIPv6: s("2001:db8::1"), networkMode: "ipv6_only",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:[2001:db8::1]:20022", Connect: "tcp:[::]:22",
			}},
		},
		{
			name:        "lxc ipv6_nat without ipv6 fails",
			networkMode: "ipv6_nat",
			want:        InstancePortMappingProxyResult{Success: false, ErrorMessage: "当前所在节点暂无任何公网 IPv6 记录！请通知管理员在节点设置中补充【公网 IPv6 或 NAT IPv6 地址】，否则无法穿透映射。"},
		},
		{
			name:         "kvm nat requires a concrete ipv4",
			instanceType: "virtual-machine", networkMode: "nat",
			want: InstancePortMappingProxyResult{Success: false, ErrorMessage: "当前宿主机没有可用于 KVM 端口映射的本机 IPv4 监听地址。请在节点 NAT 设置中配置本机监听 IPv4（nat_bind_ip），通常填写宿主机网卡上的公网或内网可绑定 IPv4。"},
		},
		{
			name:         "kvm nat with concrete ipv4 sets nat",
			instanceType: "kvm", hostNatIP: s("203.0.113.7"), networkMode: "nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat,
			}},
		},
		{
			name:         "kvm ipv6_nat does not set nat",
			instanceType: "virtual-machine", hostIPv6: s("2001:db8::1"), networkMode: "ipv6_nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:[2001:db8::1]:20022", Connect: "tcp:0.0.0.0:22",
			}},
		},
		{
			name:         "kvm dual stack needs one of the addresses",
			instanceType: "virtual-machine", networkMode: "nat_ipv6_nat",
			want: InstancePortMappingProxyResult{Success: false, ErrorMessage: "Current host has no usable NAT public IPv4 or IPv6 address for KVM port mapping."},
		},
		{
			name:         "kvm dual stack with both addresses",
			instanceType: "virtual-machine", hostNatIP: s("203.0.113.7"), hostIPv6: s("2001:db8::1"), networkMode: "nat_ipv6_nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfigs: []InstancePortMappingNamedDevice{
				{DeviceConfig: InstancePortMappingDeviceConfig{Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat}},
				{NameSuffix: "-v6", DeviceConfig: InstancePortMappingDeviceConfig{Type: "proxy", Listen: "tcp:[2001:db8::1]:20022", Connect: "tcp:0.0.0.0:22"}},
			}},
		},
		{
			name:         "container type takes the lxc path",
			instanceType: "container", hostNatIP: s("203.0.113.7"), networkMode: "nat",
			want: InstancePortMappingProxyResult{Success: true, DeviceConfig: &InstancePortMappingDeviceConfig{
				Type: "proxy", Listen: "tcp:203.0.113.7:20022", Connect: "tcp:0.0.0.0:22", Nat: nat,
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InstancePortMappingCreateProxyDevice(tc.instanceType, tc.hostNatIP, tc.hostIPv6, tc.networkMode, models.ProtocolTcp, 20022, 22)
			assertProxyResult(t, got, tc.want)
		})
	}

	// UDP keeps the protocol in both addresses.
	got := InstancePortMappingCreateProxyDevice("container", s("203.0.113.7"), nil, "nat", models.ProtocolUdp, 20022, 53)
	if got.DeviceConfig == nil || got.DeviceConfig.Listen != "udp:203.0.113.7:20022" || got.DeviceConfig.Connect != "udp:0.0.0.0:53" {
		t.Errorf("udp config = %+v", got.DeviceConfig)
	}
}

func assertProxyResult(t *testing.T, got, want InstancePortMappingProxyResult) {
	t.Helper()
	if got.Success != want.Success {
		t.Fatalf("success = %v, want %v", got.Success, want.Success)
	}
	if got.ErrorMessage != want.ErrorMessage {
		t.Errorf("errorMessage = %q, want %q", got.ErrorMessage, want.ErrorMessage)
	}
	if (got.DeviceConfig == nil) != (want.DeviceConfig == nil) {
		t.Fatalf("deviceConfig = %+v, want %+v", got.DeviceConfig, want.DeviceConfig)
	}
	if got.DeviceConfig != nil && *got.DeviceConfig != *want.DeviceConfig {
		t.Errorf("deviceConfig = %+v, want %+v", *got.DeviceConfig, *want.DeviceConfig)
	}
	if len(got.DeviceConfigs) != len(want.DeviceConfigs) {
		t.Fatalf("deviceConfigs = %+v, want %+v", got.DeviceConfigs, want.DeviceConfigs)
	}
	for index := range want.DeviceConfigs {
		if got.DeviceConfigs[index].NameSuffix != want.DeviceConfigs[index].NameSuffix ||
			got.DeviceConfigs[index].DeviceConfig != want.DeviceConfigs[index].DeviceConfig {
			t.Errorf("deviceConfigs[%d] = %+v, want %+v", index, got.DeviceConfigs[index], want.DeviceConfigs[index])
		}
	}
}

// ==================== the mapping flow ====================

type fakePortMappingBackend struct {
	allocatedPort int32
	allocatedOK   bool
	allocateErr   error
	allocateCalls int

	portInUse  bool
	inUseErr   error
	inUseCalls int

	createErr   error
	createCalls []InstancePortMappingCreateInput
	nextID      int32

	deleteErr  error
	deletedIDs []int32
}

func (f *fakePortMappingBackend) AllocatePort(context.Context, int32, InstancePortMappingProtocol) (int32, bool, error) {
	f.allocateCalls++
	return f.allocatedPort, f.allocatedOK, f.allocateErr
}

func (f *fakePortMappingBackend) CheckPortInUse(context.Context, int32, int32, InstancePortMappingProtocol) (bool, error) {
	f.inUseCalls++
	return f.portInUse, f.inUseErr
}

func (f *fakePortMappingBackend) CreatePortMapping(_ context.Context, input InstancePortMappingCreateInput) (int32, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return 0, f.createErr
	}
	if f.nextID == 0 {
		return 900, nil
	}
	return f.nextID, nil
}

func (f *fakePortMappingBackend) DeletePortMapping(_ context.Context, id int32) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return f.deleteErr
}

type fakePortMappingClient struct {
	addErr     error
	added      []string
	addConfigs []map[string]string
	removeErr  error
	removed    []string
}

func (f *fakePortMappingClient) AddDevice(_ context.Context, _, deviceName string, device map[string]string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, deviceName)
	f.addConfigs = append(f.addConfigs, device)
	return nil
}

func (f *fakePortMappingClient) RemoveDevice(_ context.Context, _, deviceName string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, deviceName)
	return nil
}

// withPortMappingSeams installs fake seams for the duration of a test.
func withPortMappingSeams(t *testing.T, backendInstancePortMappingBackend InstancePortMappingBackend, client InstancePortMappingClient) {
	t.Helper()
	originalBackend := instancePortMappingBackend
	originalFactory := InstancePortMappingClientFactory
	originalGetHost := InstancePortMappingGetHostByID
	originalGetInstance := InstancePortMappingGetInstanceByID
	originalPackage := InstancePortMappingPackageIsVirtualMachine
	originalAlias := InstancePortMappingHostIPv6Alias
	originalFlag := InstancePortMappingIsAutoRemotePortEnabled

	RegisterInstancePortMappingBackend(backendInstancePortMappingBackend)
	InstancePortMappingClientFactory = func(context.Context, *InstancePortMappingHost) (InstancePortMappingClient, error) {
		if client == nil {
			return nil, errors.New("no fake client")
		}
		return client, nil
	}

	t.Cleanup(func() {
		instancePortMappingBackend = originalBackend
		InstancePortMappingClientFactory = originalFactory
		InstancePortMappingGetHostByID = originalGetHost
		InstancePortMappingGetInstanceByID = originalGetInstance
		InstancePortMappingPackageIsVirtualMachine = originalPackage
		InstancePortMappingHostIPv6Alias = originalAlias
		InstancePortMappingIsAutoRemotePortEnabled = originalFlag
	})
}

func testHost() *InstancePortMappingHost {
	start, end := int32(20_000), int32(20_010)
	bind := "203.0.113.7"
	publicIP := "203.0.113.7"
	return &InstancePortMappingHost{
		ID: 7, URL: "https://10.0.0.7:8443", NATBindIP: &bind, NATPublicIP: &publicIP,
		NATPortStart: &start, NATPortEnd: &end,
	}
}

func testInstance() InstancePortMappingInstance {
	return InstancePortMappingInstance{ID: 11, HostID: 7, IncusID: "pi-abcd", NetworkMode: string(models.NetworkModeNat)}
}

func TestAddInstancePortMappingEarlyFailures(t *testing.T) {
	cases := []struct {
		name       string
		input      InstancePortMappingAddInput
		wantCode   ErrorCode
		wantDetail string
	}{
		{
			name:     "host missing",
			input:    InstancePortMappingAddInput{Instance: testInstance(), Host: nil, Protocol: models.ProtocolTcp, PrivatePort: 22},
			wantCode: ErrHostNotFound,
		},
		{
			name: "ipv6_only does not need mappings",
			input: InstancePortMappingAddInput{
				Instance: InstancePortMappingInstance{ID: 11, HostID: 7, IncusID: "pi", NetworkMode: string(models.NetworkModeIpv6Only)},
				Host:     testHost(),
				Protocol: models.ProtocolTcp, PrivatePort: 22,
			},
			wantCode:   ErrInvalidParams,
			wantDetail: "IPv6 Only instances do not require port mappings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakePortMappingBackend{}
			withPortMappingSeams(t, backend, &fakePortMappingClient{})
			result, err := AddInstancePortMapping(context.Background(), tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Ok || result.Code != tc.wantCode {
				t.Fatalf("result = %+v, want code %s", result, tc.wantCode)
			}
			if tc.wantDetail != "" && (result.Detail == nil || *result.Detail != tc.wantDetail) {
				t.Errorf("detail = %v, want %q", result.Detail, tc.wantDetail)
			}
			if backend.allocateCalls != 0 || len(backend.createCalls) != 0 {
				t.Error("early failures must not touch the port pool")
			}
		})
	}
}

func TestAddInstancePortMappingPortSelection(t *testing.T) {
	host := testHost()

	t.Run("explicit port below the range", func(t *testing.T) {
		backend := &fakePortMappingBackend{}
		withPortMappingSeams(t, backend, &fakePortMappingClient{})
		port := int32(19_999)
		result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
			Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp,
			PrivatePort: 22, PublicPort: &port,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Ok || result.Code != ErrPortRangeInvalid {
			t.Fatalf("result = %+v, want PORT_RANGE_INVALID", result)
		}
		if result.Detail == nil || *result.Detail != "20000-20010" {
			t.Errorf("detail = %v, want 20000-20010", result.Detail)
		}
		if backend.allocateCalls != 0 {
			t.Error("an explicit port must not hit the allocator")
		}
	})

	t.Run("explicit port above the range", func(t *testing.T) {
		backend := &fakePortMappingBackend{}
		withPortMappingSeams(t, backend, &fakePortMappingClient{})
		port := int32(20_011)
		result, _ := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
			Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp,
			PrivatePort: 22, PublicPort: &port,
		})
		if result.Code != ErrPortRangeInvalid {
			t.Fatalf("result = %+v, want PORT_RANGE_INVALID", result)
		}
	})

	t.Run("explicit port on the boundaries is accepted", func(t *testing.T) {
		for _, boundary := range []int32{20_000, 20_010} {
			backend := &fakePortMappingBackend{}
			client := &fakePortMappingClient{}
			withPortMappingSeams(t, backend, client)
			port := boundary
			result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
				Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp,
				PrivatePort: 22, PublicPort: &port,
			})
			if err != nil {
				t.Fatalf("boundary %d: %v", boundary, err)
			}
			if !result.Ok {
				t.Fatalf("boundary %d rejected: %+v", boundary, result)
			}
			if backend.allocateCalls != 0 {
				t.Errorf("boundary %d must not allocate", boundary)
			}
			if len(client.added) != 1 || client.added[0] != fmt.Sprintf("proxy-tcp-%d", boundary) {
				t.Errorf("boundary %d devices = %v", boundary, client.added)
			}
		}
	})

	t.Run("pool exhausted", func(t *testing.T) {
		backend := &fakePortMappingBackend{allocatedOK: false}
		withPortMappingSeams(t, backend, &fakePortMappingClient{})
		result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
			Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp, PrivatePort: 22,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Ok || result.Code != ErrPortNoAvailable {
			t.Fatalf("result = %+v, want PORT_NO_AVAILABLE", result)
		}
	})

	t.Run("allocator error propagates", func(t *testing.T) {
		backend := &fakePortMappingBackend{allocateErr: errors.New("db down")}
		withPortMappingSeams(t, backend, &fakePortMappingClient{})
		_, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
			Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp, PrivatePort: 22,
		})
		if err == nil || err.Error() != "db down" {
			t.Fatalf("err = %v, want db down", err)
		}
	})

	t.Run("port already in use", func(t *testing.T) {
		port := int32(20_005)
		backend := &fakePortMappingBackend{portInUse: true}
		withPortMappingSeams(t, backend, &fakePortMappingClient{})
		result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
			Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp, PrivatePort: 22, PublicPort: &port,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Ok || result.Code != ErrPortInUse {
			t.Fatalf("result = %+v, want PORT_IN_USE", result)
		}
		if len(backend.createCalls) != 0 {
			t.Error("must not reserve a port that is in use")
		}
	})

	t.Run("check error propagates", func(t *testing.T) {
		backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true, inUseErr: errors.New("db down")}
		withPortMappingSeams(t, backend, &fakePortMappingClient{})
		_, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
			Instance: testInstance(), Host: host, Protocol: models.ProtocolTcp, PrivatePort: 22,
		})
		if err == nil || err.Error() != "db down" {
			t.Fatalf("err = %v, want db down", err)
		}
	})
}

func TestAddInstancePortMappingSuccess(t *testing.T) {
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true, nextID: 4242}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)

	remark := "  web  "
	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: testInstance(), Host: testHost(), Protocol: models.ProtocolTcp,
		PrivatePort: 80, Remark: &remark,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Fatalf("result = %+v", result)
	}
	if result.Mapping.ID != 4242 || result.Mapping.Protocol != models.ProtocolTcp ||
		result.Mapping.PublicPort != 20_005 || result.Mapping.PrivatePort != 80 {
		t.Errorf("mapping = %+v", *result.Mapping)
	}
	if result.Mapping.Remark == nil || *result.Mapping.Remark != "web" {
		t.Errorf("mapping remark = %v, want web", result.Mapping.Remark)
	}

	if len(backend.createCalls) != 1 {
		t.Fatalf("createCalls = %d", len(backend.createCalls))
	}
	created := backend.createCalls[0]
	if created.InstanceID != 11 || created.HostID != 7 || created.Protocol != models.ProtocolTcp ||
		created.PublicPort != 20_005 || created.PrivatePort != 80 || created.SystemManaged {
		t.Errorf("create input = %+v", created)
	}
	if created.Remark == nil || *created.Remark != "web" {
		t.Errorf("create remark = %v, want trimmed web", created.Remark)
	}

	if len(client.added) != 1 || client.added[0] != "proxy-tcp-20005" {
		t.Fatalf("devices = %v", client.added)
	}
	if got := client.addConfigs[0]; got["type"] != "proxy" || got["listen"] != "tcp:203.0.113.7:20005" ||
		got["connect"] != "tcp:0.0.0.0:80" || got["nat"] != "true" {
		t.Errorf("device config = %v", got)
	}
	if len(backend.deletedIDs) != 0 || len(client.removed) != 0 {
		t.Error("a successful mapping must not roll anything back")
	}
}

func TestAddInstancePortMappingBlankRemarkBecomesNull(t *testing.T) {
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true}
	withPortMappingSeams(t, backend, &fakePortMappingClient{})
	remark := "   \t "
	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: testInstance(), Host: testHost(), Protocol: models.ProtocolTcp,
		PrivatePort: 80, Remark: &remark,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mapping == nil || result.Mapping.Remark != nil {
		t.Errorf("mapping remark = %v, want nil", result.Mapping)
	}
	if backend.createCalls[0].Remark != nil {
		t.Errorf("create remark = %v, want nil", backend.createCalls[0].Remark)
	}
}

// TestAddInstancePortMappingRollback covers finding 2 and 3 of the safety sequence:
// only the device names created by *this* call are removed, and the DB reservation
// is deleted.
func TestAddInstancePortMappingRollback(t *testing.T) {
	// A dual-stack mapping creates two devices; the second one fails.
	host := testHost()
	host.NATBindIPv6 = incusConfigStrPtr("2001:db8::7")
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true, nextID: 777}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)
	InstancePortMappingGetHostByID = func(context.Context, int32) (*InstancePortMappingHost, error) {
		return host, nil
	}

	instance := testInstance()
	instance.NetworkMode = string(models.NetworkModeNatIpv6Nat)

	// Let the first AddDevice succeed, fail the second.
	failingClient := &sequentialFailingClient{inner: client, failOnCall: 2}
	InstancePortMappingClientFactory = func(context.Context, *InstancePortMappingHost) (InstancePortMappingClient, error) {
		return failingClient, nil
	}

	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: instance, Host: host, Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Ok || result.Code != ErrInternalError {
		t.Fatalf("result = %+v, want INTERNAL_ERROR", result)
	}
	if result.Message == nil || *result.Message != "device boom" {
		t.Errorf("message = %v, want device boom", result.Message)
	}

	// Rollback removes only the device this call created, never the -v6 one.
	if len(client.removed) != 1 || client.removed[0] != "proxy-tcp-20005" {
		t.Errorf("removed = %v, want [proxy-tcp-20005]", client.removed)
	}
	if len(backend.deletedIDs) != 1 || backend.deletedIDs[0] != 777 {
		t.Errorf("deleted = %v, want [777]", backend.deletedIDs)
	}
}

// sequentialFailingClient fails the n-th AddDevice call.
type sequentialFailingClient struct {
	inner      *fakePortMappingClient
	failOnCall int
	calls      int
}

func (c *sequentialFailingClient) AddDevice(ctx context.Context, instanceName, deviceName string, device map[string]string) error {
	c.calls++
	if c.calls == c.failOnCall {
		return errors.New("device boom")
	}
	return c.inner.AddDevice(ctx, instanceName, deviceName, device)
}

func (c *sequentialFailingClient) RemoveDevice(ctx context.Context, instanceName, deviceName string) error {
	return c.inner.RemoveDevice(ctx, instanceName, deviceName)
}

func TestAddInstancePortMappingUniqueViolationBecomesPortInUse(t *testing.T) {
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true, createErr: errors.New("ERROR: duplicate key value violates unique constraint \"port_mappings_host_id_public_port_protocol_key\" (SQLSTATE 23505)")}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)

	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: testInstance(), Host: testHost(), Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ok || result.Code != ErrPortInUse {
		t.Fatalf("result = %+v, want PORT_IN_USE", result)
	}
	if len(client.added) != 0 {
		t.Error("no device may be added when the reservation fails")
	}
}

func TestAddInstancePortMappingProxyFailureIsInvalidParams(t *testing.T) {
	// LXC ipv6_only without a host IPv6: the strategy refuses before any DB write.
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)

	instance := testInstance()
	instance.NetworkMode = string(models.NetworkModeIpv6Only)
	// ipv6_only would be rejected earlier, so use ipv6_nat instead (it allows mapping).
	instance.NetworkMode = string(models.NetworkModeIpv6Nat)

	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: instance, Host: testHost(), Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ok || result.Code != ErrInvalidParams {
		t.Fatalf("result = %+v, want INVALID_PARAMS", result)
	}
	if result.Detail == nil || !strings.Contains(*result.Detail, "公网 IPv6") {
		t.Errorf("detail = %v", result.Detail)
	}
	if len(backend.createCalls) != 0 {
		t.Error("a rejected proxy device must not reserve a port")
	}
}

func TestAddInstancePortMappingUsesPackageInstanceType(t *testing.T) {
	// The package says VM, so the KVM strategy runs and refuses a wildcard bind.
	host := testHost()
	host.NATBindIP = nil
	host.NATPublicIP = nil
	host.IPAddress = nil

	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)
	packageID := int32(3)
	instance := testInstance()
	instance.PackageID = &packageID

	var lookedUp int32
	InstancePortMappingPackageIsVirtualMachine = func(_ context.Context, id int32) (bool, error) {
		lookedUp = id
		return true, nil
	}

	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: instance, Host: host, Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookedUp != 3 {
		t.Errorf("package lookup id = %d, want 3", lookedUp)
	}
	if result.Ok || result.Code != ErrInvalidParams {
		t.Fatalf("result = %+v, want INVALID_PARAMS from the KVM strategy", result)
	}
	if result.Detail == nil || !strings.Contains(*result.Detail, "KVM") {
		t.Errorf("detail = %v, want the KVM message", result.Detail)
	}
}

func TestAddInstancePortMappingClientFactoryFailure(t *testing.T) {
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true}
	withPortMappingSeams(t, backend, &fakePortMappingClient{})
	InstancePortMappingClientFactory = func(context.Context, *InstancePortMappingHost) (InstancePortMappingClient, error) {
		return nil, errors.New("tunnel down")
	}
	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: testInstance(), Host: testHost(), Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ok || result.Code != ErrInternalError || result.Message == nil || *result.Message != "tunnel down" {
		t.Fatalf("result = %+v", result)
	}
}

// TestAddInstancePortMappingRollbackAbortsOnClientFailure pins the TypeScript quirk:
// if the rollback cannot rebuild a client, the DB reservation is left behind (the
// whole rollback sits in one try/catch). Behaviour is replicated, not "fixed", so a
// future divergence is a conscious decision.
func TestAddInstancePortMappingRollbackAbortsOnClientFailure(t *testing.T) {
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true, nextID: 555}
	withPortMappingSeams(t, backend, &fakePortMappingClient{})

	instance := testInstance()
	instance.NetworkMode = string(models.NetworkModeNatIpv6Nat)

	calls := 0
	InstancePortMappingClientFactory = func(context.Context, *InstancePortMappingHost) (InstancePortMappingClient, error) {
		calls++
		if calls == 1 {
			return &failingAddClient{}, nil
		}
		return nil, errors.New("rollback client unavailable")
	}

	result, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: instance, Host: testHost(), Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ok || result.Code != ErrInternalError {
		t.Fatalf("result = %+v", result)
	}
	if len(backend.deletedIDs) != 0 {
		t.Errorf("deleted = %v; the TypeScript skips the DB cleanup when the rollback client fails", backend.deletedIDs)
	}
}

type failingAddClient struct{}

func (failingAddClient) AddDevice(context.Context, string, string, map[string]string) error {
	return errors.New("add boom")
}

func (failingAddClient) RemoveDevice(context.Context, string, string) error { return nil }

func TestAddInstancePortMappingRequiresBackend(t *testing.T) {
	// Without a registered backend the write path fails loudly instead of pretending.
	RegisterInstancePortMappingBackend(nil)
	originalFactory := InstancePortMappingClientFactory
	InstancePortMappingClientFactory = func(context.Context, *InstancePortMappingHost) (InstancePortMappingClient, error) {
		return &fakePortMappingClient{}, nil
	}
	t.Cleanup(func() {
		RegisterInstancePortMappingBackend(nil)
		InstancePortMappingClientFactory = originalFactory
	})

	_, err := AddInstancePortMapping(context.Background(), InstancePortMappingAddInput{
		Instance: testInstance(), Host: testHost(), Protocol: models.ProtocolTcp, PrivatePort: 22,
	})
	if !errors.Is(err, ErrInstancePortMappingBackendUnavailable) {
		t.Fatalf("err = %v, want ErrInstancePortMappingBackendUnavailable", err)
	}
}

// ==================== provisionAutoRemotePort ====================

func TestProvisionAutoRemotePortSkips(t *testing.T) {
	cases := []struct {
		name        string
		auto        *bool
		networkMode string
		flag        bool
	}{
		{name: "explicit false", auto: instancePortMappingBoolPtr(false), networkMode: string(models.NetworkModeNat), flag: true},
		{name: "ipv6_only", auto: nil, networkMode: string(models.NetworkModeIpv6Only), flag: true},
		{name: "public ipv4", auto: nil, networkMode: string(models.NetworkModePublicIpv4), flag: true},
		{name: "global switch off", auto: nil, networkMode: string(models.NetworkModeNat), flag: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true}
			client := &fakePortMappingClient{}
			withPortMappingSeams(t, backend, client)

			lookups := 0
			InstancePortMappingIsAutoRemotePortEnabled = func(context.Context) (bool, error) {
				lookups++
				return tc.flag, nil
			}
			InstancePortMappingGetInstanceByID = func(context.Context, int32) (*InstancePortMappingInstance, error) {
				t.Fatal("must not load the instance when the mapping is suppressed")
				return nil, nil
			}

			ProvisionAutoRemotePort(context.Background(), InstancePortMappingAutoRemotePortInput{
				InstanceID: 11, Image: "ubuntu/noble", NetworkMode: tc.networkMode, AutoRemotePort: tc.auto,
			})
			if tc.name == "global switch off" && lookups != 1 {
				t.Errorf("lookups = %d, want 1", lookups)
			}
			if len(client.added) != 0 || len(backend.createCalls) != 0 {
				t.Error("nothing may be created when the mapping is suppressed")
			}
		})
	}
}

func TestProvisionAutoRemotePortCreatesMapping(t *testing.T) {
	backend := &fakePortMappingBackend{allocatedPort: 20_005, allocatedOK: true, nextID: 314}
	client := &fakePortMappingClient{}
	withPortMappingSeams(t, backend, client)

	InstancePortMappingIsAutoRemotePortEnabled = func(context.Context) (bool, error) { return true, nil }
	InstancePortMappingGetInstanceByID = func(_ context.Context, id int32) (*InstancePortMappingInstance, error) {
		instance := testInstance()
		instance.ID = id
		return &instance, nil
	}
	InstancePortMappingGetHostByID = func(context.Context, int32) (*InstancePortMappingHost, error) {
		return testHost(), nil
	}

	ProvisionAutoRemotePort(context.Background(), InstancePortMappingAutoRemotePortInput{
		InstanceID: 42, Image: "windows/2022", NetworkMode: string(models.NetworkModeNat),
	})

	if len(backend.createCalls) != 1 {
		t.Fatalf("createCalls = %d", len(backend.createCalls))
	}
	created := backend.createCalls[0]
	if created.InstanceID != 42 || created.PrivatePort != 3389 || !created.SystemManaged {
		t.Errorf("create input = %+v (want windows RDP 3389, systemManaged)", created)
	}
	if len(client.added) != 1 || client.added[0] != "proxy-tcp-20005" {
		t.Errorf("devices = %v", client.added)
	}
}

func TestProvisionAutoRemotePortSwallowsErrors(t *testing.T) {
	withPortMappingSeams(t, &fakePortMappingBackend{}, &fakePortMappingClient{})
	InstancePortMappingIsAutoRemotePortEnabled = func(context.Context) (bool, error) {
		return true, nil
	}
	InstancePortMappingGetInstanceByID = func(context.Context, int32) (*InstancePortMappingInstance, error) {
		return nil, errors.New("db down")
	}
	// Must not panic and must not propagate.
	ProvisionAutoRemotePort(context.Background(), InstancePortMappingAutoRemotePortInput{
		InstanceID: 1, Image: "debian/12", NetworkMode: string(models.NetworkModeNat),
	})
}

func instancePortMappingBoolPtr(value bool) *bool { return &value }
