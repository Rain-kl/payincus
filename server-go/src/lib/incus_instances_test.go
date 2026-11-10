package lib

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ==================== fake node ====================

// incusInstanceTestRequest is one request the fake node received.
type incusInstanceTestRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// incusInstanceTestNode is a scripted Incus API: the handler decides the status and
// body for every request and the node records what actually arrived. No socket ever
// leaves the test process.
type incusInstanceTestNode struct {
	mu       sync.Mutex
	requests []incusInstanceTestRequest
	handler  func(request incusInstanceTestRequest) (int, string)
}

func (n *incusInstanceTestNode) Requests() []incusInstanceTestRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]incusInstanceTestRequest(nil), n.requests...)
}

func (n *incusInstanceTestNode) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requests)
}

// incusInstanceTestClient builds a client whose transport answers with the handler
// instead of dialling. The unexported fields are reachable because this file is in
// package lib.
func incusInstanceTestClient(t *testing.T, handler func(request incusInstanceTestRequest) (int, string)) (*IncusClient, *incusInstanceTestNode) {
	t.Helper()
	node := &incusInstanceTestNode{handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		record := incusInstanceTestRequest{Method: request.Method, Path: request.URL.Path}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &record.Body)
		}
		if request.URL.RawQuery != "" {
			record.Path += "?" + request.URL.RawQuery
		}

		node.mu.Lock()
		node.requests = append(node.requests, record)
		node.mu.Unlock()

		status, body := node.handler(record)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return &IncusClient{baseURL: server.URL, http: server.Client()}, node
}

// incusSyncEnvelope is a successful sync response carrying `metadata`.
func incusSyncEnvelope(metadata string) string {
	return `{"type":"sync","status":"Success","status_code":200,"metadata":` + metadata + `}`
}

// incusErrorEnvelope is a node-side error response.
func incusErrorEnvelope(message string, code int) string {
	encoded, _ := json.Marshal(message)
	return `{"type":"error","error":` + string(encoded) + `,"error_code":` + itoaSmall(code) + `}`
}

// incusAsyncEnvelope starts an async operation.
func incusAsyncEnvelope(operation string) string {
	return `{"type":"async","status":"Operation created","status_code":100,"operation":"` + operation + `"}`
}

// incusOperationEnvelope is a sync response whose metadata is an operation object.
func incusOperationEnvelope(status string, statusCode int, metadata string) string {
	return `{"type":"sync","status":"Success","status_code":200,"metadata":{"status":"` + status +
		`","status_code":` + itoaSmall(statusCode) + `,"metadata":` + metadata + `}}`
}

// incusAssertJSONEqual compares two JSON documents semantically, so map order and
// number representation cannot produce a false failure.
func incusAssertJSONEqual(t *testing.T, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("bad expected JSON: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var gotValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !reflect.DeepEqual(gotValue, want) {
		prettyGot, _ := json.MarshalIndent(gotValue, "", "  ")
		prettyWant, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", prettyGot, prettyWant)
	}
}

func incusStringPtr(value string) *string { return &value }
func incusInt32Value(value int32) *int32  { return &value }
func incusBoolPtr(value bool) *bool       { return &value }

// ==================== buildInstanceConfig ====================

// TestIncusInstanceBuildConfigContainerNAT pins the whole container NAT body,
// including the swap limit, the IOPS fallback for the write direction and the
// bridged eth0 device.
func TestIncusInstanceBuildConfigContainerNAT(t *testing.T) {
	config, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:         "vm-test01",
		Image:        "ubuntu/24.04",
		CPU:          200,
		Memory:       2048,
		Disk:         20480,
		SwapEnabled:  incusBoolPtr(true),
		SwapSize:     incusInt32Value(512),
		Nested:       true,
		NetworkMode:  "nat",
		InstanceType: "container",
		StoragePool:  incusStringPtr("default"),
		IPv4Address:  incusStringPtr("10.10.1.5"),
		CloudInitConfig: &IncusConfigPayload{
			UserUserData: "#cloud-config\nhostname: vm-test01",
		},
		LimitsRead:            incusStringPtr("100MB"),
		LimitsWriteIops:       incusInt32Value(500),
		LimitsIngress:         incusStringPtr("300Mbit"),
		LimitsProcesses:       incusInt32Value(500),
		LimitsCPUPriority:     incusInt32Value(5),
		BootAutostart:         incusBoolPtr(true),
		BootAutostartPriority: incusInt32Value(10),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	incusAssertJSONEqual(t, config, `{
		"name": "vm-test01",
		"type": "container",
		"source": {"type": "image", "alias": "ubuntu/24.04"},
		"config": {
			"limits.cpu": "2",
			"limits.cpu.allowance": "200%",
			"limits.memory": "2048MB",
			"limits.memory.swap": "512MiB",
			"limits.processes": "500",
			"limits.cpu.priority": "5",
			"boot.autostart": "true",
			"boot.autostart.priority": "10",
			"security.nesting": "true",
			"user.user-data": "#cloud-config\nhostname: vm-test01"
		},
		"devices": {
			"root": {
				"path": "/",
				"pool": "default",
				"type": "disk",
				"size": "20480MB",
				"limits.read": "100MB",
				"limits.write": "500iops"
			},
			"eth0": {
				"type": "nic",
				"name": "eth0",
				"nictype": "bridged",
				"parent": "incusbr0",
				"ipv4.address": "10.10.1.5",
				"limits.ingress": "300Mbit"
			}
		}
	}`)
}

// TestIncusInstanceBuildConfigRoutedIPv6 pins the dual-NIC architecture: eth0
// bridged with the legacy Gbit→Mbit conversion, eth1 routed with the full address
// list and the subnet route, and the v2 network-config override.
func TestIncusInstanceBuildConfigRoutedIPv6(t *testing.T) {
	config, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:          "vm-test02",
		Image:         "debian/12",
		CPU:           100,
		Memory:        1024,
		Disk:          10240,
		SwapEnabled:   incusBoolPtr(false),
		NetworkMode:   "nat_ipv6",
		InstanceType:  "container",
		IPv4Address:   incusStringPtr("10.10.1.6"),
		IPv6Address:   incusStringPtr("2a01:db8::5"),
		IPv6Config:    &NetworkPayloadIPv6Config{PrimaryIP: "2a01:db8::5", ExtraIPs: []string{"2a01:db8::6"}, Subnet: incusStringPtr("2a01:db8::/112")},
		HostInterface: incusStringPtr("ens3"),
		LimitsIngress: incusStringPtr("1Gbit"),
		LimitsEgress:  incusStringPtr("500Mbit"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	incusAssertJSONEqual(t, config, `{
		"name": "vm-test02",
		"type": "container",
		"source": {"type": "image", "alias": "debian/12"},
		"config": {
			"limits.cpu": "1",
			"limits.cpu.allowance": "100%",
			"limits.memory": "1024MB",
			"limits.memory.swap": "false",
			"user.user-data": "#cloud-config\nhostname: vm-test02\nmanage_etc_hosts: true\ndisable_root: false\nruncmd:\n  - sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config\n  - sed -i 's/PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config\n  - systemctl restart sshd || service ssh restart || true",
			"user.network-config": "version: 2\nethernets:\n  eth0:\n    dhcp4: false\n    dhcp6: false\n    accept-ra: false\n    addresses:\n      - 10.10.1.6/22\n    routes:\n      - to: 0.0.0.0/0\n        via: 10.10.0.1\n    nameservers:\n      addresses:\n        - 10.10.0.1\n  eth1:\n    dhcp4: false\n    dhcp6: false\n    accept-ra: false\n    addresses:\n      - 2a01:db8::5/128\n      - 2a01:db8::6/128\n    gateway6: fe80::1\n    nameservers:\n      addresses:\n        - 10.10.0.1\n"
		},
		"devices": {
			"root": {"path": "/", "pool": "default", "type": "disk", "size": "10240MB"},
			"eth0": {
				"type": "nic",
				"name": "eth0",
				"nictype": "bridged",
				"parent": "incusbr0",
				"ipv4.address": "10.10.1.6",
				"limits.ingress": "1000Mbit",
				"limits.egress": "500Mbit"
			},
			"eth1": {
				"type": "nic",
				"nictype": "routed",
				"parent": "ens3",
				"name": "eth1",
				"ipv6.address": "2a01:db8::5,2a01:db8::6",
				"ipv6.routes": "2a01:db8::/112"
			}
		}
	}`)
}

// TestIncusInstanceBuildConfigVM pins the VM differences: the agent ISO device, the
// deterministic MACs, security.secureboot, the remote simplestreams source and the
// deliberate absence of the network-config override.
func TestIncusInstanceBuildConfigVM(t *testing.T) {
	config, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:         "vm-test01",
		Image:        "images:debian/12",
		CPU:          100,
		Memory:       2048,
		Disk:         20480,
		NetworkMode:  "nat",
		InstanceType: "vm",
		IPv4Address:  incusStringPtr("10.10.1.7"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	incusAssertJSONEqual(t, config, `{
		"name": "vm-test01",
		"type": "virtual-machine",
		"source": {
			"type": "image",
			"mode": "pull",
			"server": "https://images.linuxcontainers.org",
			"protocol": "simplestreams",
			"alias": "debian/12"
		},
		"config": {
			"limits.cpu": "1",
			"limits.cpu.allowance": "100%",
			"limits.memory": "2048MB",
			"security.secureboot": "false",
			"user.user-data": "#cloud-config\nhostname: vm-test01\nmanage_etc_hosts: true\ndisable_root: false\nruncmd:\n  - sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config\n  - sed -i 's/PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config\n  - systemctl restart sshd || service ssh restart || true"
		},
		"devices": {
			"root": {"path": "/", "pool": "default", "type": "disk", "size": "20480MB"},
			"agent": {"type": "disk", "source": "agent:config"},
			"eth0": {
				"type": "nic",
				"name": "eth0",
				"nictype": "bridged",
				"parent": "incusbr0",
				"ipv4.address": "10.10.1.7",
				"hwaddr": "02:86:d7:0f:df:20"
			}
		}
	}`)
}

// TestIncusInstanceBuildConfigVMKeepsGeneratedNetworkConfig pins that a VM's
// generator-produced network-config (MAC matched) is never overwritten by the
// routed-IPv6 override, which only applies to containers.
func TestIncusInstanceBuildConfigVMKeepsGeneratedNetworkConfig(t *testing.T) {
	generated := "version: 2\nethernets:\n  enp5s0:\n    match:\n      macaddress: 02:86:d7:0f:df:20\n"
	config, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:          "vm-test01",
		Image:         "images:debian/12",
		CPU:           100,
		Memory:        2048,
		Disk:          20480,
		NetworkMode:   "nat_ipv6",
		InstanceType:  "vm",
		IPv4Address:   incusStringPtr("10.10.1.7"),
		IPv6Address:   incusStringPtr("2a01:db8::7"),
		HostInterface: incusStringPtr("ens3"),
		CloudInitConfig: &IncusConfigPayload{
			UserUserData:      "#cloud-config",
			UserNetworkConfig: generated,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	settings := config["config"].(map[string]any)
	if settings["user.network-config"] != generated {
		t.Fatalf("VM network-config was overwritten: %q", settings["user.network-config"])
	}
}

// TestIncusInstanceBuildConfigGPU pins the shared-GPU projection.
func TestIncusInstanceBuildConfigGPU(t *testing.T) {
	config, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:            "gpu-1",
		Image:           "ubuntu/24.04",
		CPU:             100,
		Memory:          1024,
		Disk:            10240,
		NetworkMode:     "nat",
		InstanceType:    "container",
		GPUPCIAddresses: []string{"0000:01:00.0"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	settings := config["config"].(map[string]any)
	if settings["nvidia.runtime"] != "true" {
		t.Fatalf("nvidia.runtime = %v", settings["nvidia.runtime"])
	}
	if settings["nvidia.driver.capabilities"] != "compute,utility" {
		t.Fatalf("nvidia.driver.capabilities = %v", settings["nvidia.driver.capabilities"])
	}
	profiles, ok := config["profiles"].([]any)
	if !ok || len(profiles) != 0 {
		t.Fatalf("profiles must be cleared, got %#v", config["profiles"])
	}
	devices := config["devices"].(map[string]any)
	gpu, ok := devices["gpu0"].(map[string]any)
	if !ok || gpu["pci"] != "0000:01:00.0" || gpu["type"] != "gpu" {
		t.Fatalf("gpu device = %#v", devices["gpu0"])
	}
	if _, ok := devices["root"]; !ok {
		t.Fatal("applyGpuDevice must not drop the root device")
	}
}

// TestIncusInstanceBuildConfigRejectsPrivilegedGPU pins the isolation rule: shared
// GPUs are only for unprivileged LXC containers.
func TestIncusInstanceBuildConfigRejectsPrivilegedGPU(t *testing.T) {
	_, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:            "gpu-2",
		Image:           "ubuntu/24.04",
		CPU:             100,
		Memory:          1024,
		Disk:            10240,
		NetworkMode:     "nat",
		InstanceType:    "container",
		Privileged:      true,
		GPUPCIAddresses: []string{"0000:01:00.0"},
	})
	if err == nil {
		t.Fatal("expected a privileged GPU rejection")
	}
	var gpuErr *GpuError
	if !errors.As(err, &gpuErr) {
		t.Fatalf("expected *GpuError, got %T (%v)", err, err)
	}
}

// TestIncusInstanceBuildConfigLegacyNetworkFallback pins the backwards-compatible
// branch: no static IPv4, no limits and networkMode 'nat' means no explicit NIC at
// all (the default profile provides it).
func TestIncusInstanceBuildConfigLegacyNetworkFallback(t *testing.T) {
	config, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		Name:         "legacy-1",
		Image:        "alpine/3.20",
		CPU:          50,
		Memory:       256,
		Disk:         1024,
		NetworkMode:  "nat",
		InstanceType: "container",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	devices := config["devices"].(map[string]any)
	if _, ok := devices["eth0"]; ok {
		t.Fatal("legacy NAT without addresses/limits must not declare eth0")
	}
	if _, ok := devices["root"]; !ok {
		t.Fatal("root device missing")
	}
	settings := config["config"].(map[string]any)
	if _, ok := settings["limits.memory.swap"]; ok {
		t.Fatal("swap limit requires swapEnabled or swapSize to be provided")
	}
}

// TestIncusInstanceNormalizeBandwidth pins the legacy Gbit conversion and the
// passthrough for every other value.
func TestIncusInstanceNormalizeBandwidth(t *testing.T) {
	cases := map[string]string{
		"1Gbit":    "1000Mbit",
		"1.5Gbit":  "1500Mbit",
		"0.25Gbit": "250Mbit",
		"300Mbit":  "300Mbit",
		"100MB":    "100MB",
	}
	for input, want := range cases {
		got := incusInstanceNormalizeBandwidth(incusStringPtr(input))
		if got == nil || *got != want {
			t.Errorf("normalizeBandwidth(%q) = %v, want %q", input, got, want)
		}
	}
	if incusInstanceNormalizeBandwidth(nil) != nil {
		t.Error("nil must stay nil")
	}
	if incusInstanceNormalizeBandwidth(incusStringPtr("")) != nil {
		t.Error("empty must normalize to nil (falsy)")
	}
}

// ==================== create ====================

// TestIncusInstanceCreateSuccess pins the happy path and that the built body is what
// reaches the node.
func TestIncusInstanceCreateSuccess(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodPost && request.Path == "/1.0/instances" {
			return http.StatusOK, incusSyncEnvelope(`{"name":"vm-test01"}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	result, err := IncusInstanceCreate(t.Context(), client, map[string]any{
		"name": "vm-test01",
		"type": "container",
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if decoded["name"] != "vm-test01" {
		t.Fatalf("result name = %v", decoded["name"])
	}
	requests := node.Requests()
	if len(requests) != 1 || requests[0].Body["name"] != "vm-test01" {
		t.Fatalf("requests = %#v", requests)
	}
}

// TestIncusInstanceCreateExistingInstanceReturnsNodeError pins the "already exists"
// path: the node's 409 is propagated verbatim (the compensation path keys off it) and
// the existence re-check sees the instance, so the egress binding is retained.
func TestIncusInstanceCreateExistingInstanceReturnsNodeError(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			return http.StatusConflict, incusErrorEnvelope(`Instance "vm-test01" already exists`, 409)
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01":
			return http.StatusOK, incusSyncEnvelope(`{"name":"vm-test01","status":"Running"}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := IncusInstanceCreate(t.Context(), client, map[string]any{"name": "vm-test01"}, nil)
	if err == nil {
		t.Fatal("expected the node's error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *IncusApiError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, "already exists") {
		t.Fatalf("message = %q", apiErr.Message)
	}
	if node.Count() != 2 {
		t.Fatalf("expected POST + existence check, got %#v", node.Requests())
	}
}

// TestIncusInstanceCreateFailurePropagatesNodeError pins the generic failure path:
// the POST error wins over the existence-check error.
func TestIncusInstanceCreateFailurePropagatesNodeError(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			return http.StatusInternalServerError, incusErrorEnvelope("node exploded", 500)
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01":
			return http.StatusNotFound, incusErrorEnvelope("Instance not found", 404)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := IncusInstanceCreate(t.Context(), client, map[string]any{"name": "vm-test01"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected a 500 IncusApiError, got %T (%v)", err, err)
	}
	if node.Count() != 2 {
		t.Fatalf("expected POST + existence check, got %#v", node.Requests())
	}
}

// TestIncusInstanceCreateWaitsForAsyncOperation pins the async path: the POST returns
// an operation, Request follows it to /wait?timeout=10 and the operation metadata is
// the result.
func TestIncusInstanceCreateWaitsForAsyncOperation(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			return http.StatusAccepted, incusAsyncEnvelope("/1.0/operations/create-1")
		case request.Method == http.MethodGet && request.Path == "/1.0/operations/create-1/wait?timeout=10":
			return http.StatusOK, incusOperationEnvelope("Success", 200, `{"name":"vm-test01"}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	result, err := IncusInstanceCreate(t.Context(), client, map[string]any{"name": "vm-test01"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if decoded["name"] != "vm-test01" {
		t.Fatalf("result = %v", decoded)
	}
	if node.Count() != 2 {
		t.Fatalf("expected POST + wait, got %#v", node.Requests())
	}
}

// TestIncusInstanceCreateAsyncFailurePropagatesNodeError pins the failure branch of the
// async waiter: a Failure operation becomes an error with the node's status code.
func TestIncusInstanceCreateAsyncFailurePropagatesNodeError(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			return http.StatusAccepted, incusAsyncEnvelope("/1.0/operations/create-2")
		case request.Method == http.MethodGet && request.Path == "/1.0/operations/create-2/wait?timeout=10":
			return http.StatusOK, `{"type":"sync","status":"Success","status_code":200,"metadata":{"status":"Failure","status_code":500,"err":"unpack failed"}}`
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01":
			return http.StatusNotFound, incusErrorEnvelope("Instance not found", 404)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := IncusInstanceCreate(t.Context(), client, map[string]any{"name": "vm-test01"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected a 500 IncusApiError, got %T (%v)", err, err)
	}
	if !strings.Contains(apiErr.Message, "unpack failed") {
		t.Fatalf("message = %q", apiErr.Message)
	}
}

// ==================== operation waiting ====================

// TestIncusInstanceWaitForOperationSuccess pins the direct wait helper.
func TestIncusInstanceWaitForOperationSuccess(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Path == "/1.0/operations/op-1/wait?timeout=10" {
			return http.StatusOK, incusOperationEnvelope("Success", 200, `{"result":"ok"}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	result, err := IncusInstanceWaitForOperation(t.Context(), client, "/1.0/operations/op-1", 2*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	incusAssertJSONEqual(t, result, `{"result":"ok"}`)
}

// TestIncusInstanceWaitForOperationCancelled pins the 409 mapping.
func TestIncusInstanceWaitForOperationCancelled(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		return http.StatusOK, incusOperationEnvelope("Cancelled", 200, `{}`)
	})

	_, err := IncusInstanceWaitForOperation(t.Context(), client, "/1.0/operations/op-2", 2*time.Second, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected a 409 IncusApiError, got %T (%v)", err, err)
	}
}

// TestIncusInstanceWaitForOperationRejectsMalformedResponse pins the 502 on a response
// without status/status_code.
func TestIncusInstanceWaitForOperationRejectsMalformedResponse(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		return http.StatusOK, `{"type":"sync","status":"Success","status_code":200,"metadata":{"foo":"bar"}}`
	})

	_, err := IncusInstanceWaitForOperation(t.Context(), client, "/1.0/operations/op-3", 2*time.Second, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected a 502 IncusApiError, got %T (%v)", err, err)
	}
}

// TestIncusInstanceWaitForOperationTimeout pins the bounded wait: a forever-running
// operation must surface as a 504, never as a hung call.
func TestIncusInstanceWaitForOperationTimeout(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		return http.StatusOK, incusOperationEnvelope("Running", 100, `{}`)
	})

	started := time.Now()
	_, err := IncusInstanceWaitForOperation(t.Context(), client, "/1.0/operations/op-4",
		300*time.Millisecond, nil)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected a 504 IncusApiError, got %T (%v)", err, err)
	}
	if !strings.Contains(apiErr.Message, "Operation timeout") {
		t.Fatalf("message = %q", apiErr.Message)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("wait took %s; the timeout is not bounded", elapsed)
	}
	if node.Count() == 0 {
		t.Fatal("expected at least one poll")
	}
}

// TestIncusInstanceWaitForOperationLongDelegates pins the 10-minute-default helper.
func TestIncusInstanceWaitForOperationLongDelegates(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Path == "/1.0/operations/op-5/wait?timeout=10" {
			return http.StatusOK, incusOperationEnvelope("Success", 200, `{"ok":true}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	result, err := IncusInstanceWaitForOperationLong(t.Context(), client, "/1.0/operations/op-5", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	incusAssertJSONEqual(t, result, `{"ok":true}`)
}

// TestIncusClientWaitForOperationTimeout pins the same bound on the client's own
// auto-wait, which is what every Request on an async envelope uses.
func TestIncusClientWaitForOperationTimeout(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		return http.StatusOK, incusOperationEnvelope("Running", 100, `{}`)
	})

	_, err := client.WaitForOperation(t.Context(), "/1.0/operations/slow", 300*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected a 504 IncusApiError, got %T (%v)", err, err)
	}
}

// ==================== delete ====================

// TestIncusInstanceDeleteIsIdempotent pins the 404 branch: an already-absent instance
// is a success, not an error.
func TestIncusInstanceDeleteIsIdempotent(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01/logs/qemu.log":
			return http.StatusNotFound, incusErrorEnvelope("not found", 404)
		case request.Method == http.MethodDelete && request.Path == "/1.0/instances/vm-test01":
			return http.StatusNotFound, incusErrorEnvelope("Instance not found", 404)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	result, err := IncusInstanceDelete(t.Context(), client, "vm-test01")
	if err != nil {
		t.Fatalf("delete must be idempotent, got %v", err)
	}
	if result != nil {
		t.Fatalf("result = %s, want nil", string(result))
	}
	if node.Count() != 2 {
		t.Fatalf("requests = %#v", node.Requests())
	}
}

// TestIncusInstanceDeleteSucceeds pins the normal delete and the pre-delete log read.
func TestIncusInstanceDeleteSucceeds(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01/logs/qemu.log":
			return http.StatusOK, "qemu: boot error\n"
		case request.Method == http.MethodDelete && request.Path == "/1.0/instances/vm-test01":
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	if _, err := IncusInstanceDelete(t.Context(), client, "vm-test01"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Count() != 2 {
		t.Fatalf("requests = %#v", node.Requests())
	}
}

// TestIncusInstanceDeletePropagatesNon404 pins that a real node failure is not
// swallowed by the idempotency branch.
func TestIncusInstanceDeletePropagatesNon404(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01/logs/qemu.log":
			return http.StatusNotFound, incusErrorEnvelope("not found", 404)
		case request.Method == http.MethodDelete && request.Path == "/1.0/instances/vm-test01":
			return http.StatusInternalServerError, incusErrorEnvelope("storage busy", 500)
		}
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := IncusInstanceDelete(t.Context(), client, "vm-test01")
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *IncusApiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected a 500 IncusApiError, got %T (%v)", err, err)
	}
}

// TestIncusInstanceCleanupDelegatesToDelete pins that the compensation cleanup is the
// idempotent delete.
func TestIncusInstanceCleanupDelegatesToDelete(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodDelete {
			return http.StatusNotFound, incusErrorEnvelope("Instance not found", 404)
		}
		return http.StatusNotFound, incusErrorEnvelope("not found", 404)
	})

	if err := IncusInstanceCleanup(t.Context(), client, "vm-test01"); err != nil {
		t.Fatalf("cleanup must be idempotent, got %v", err)
	}
}

// ==================== start / stop ====================

// TestIncusInstanceStartPutsState pins the start body (action + 60s timeout).
func TestIncusInstanceStartPutsState(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodPut && request.Path == "/1.0/instances/vm-test01/state" {
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	if _, err := IncusInstanceStart(t.Context(), client, "vm-test01"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	requests := node.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Body["action"] != "start" {
		t.Fatalf("action = %v", requests[0].Body["action"])
	}
	if requests[0].Body["timeout"] != float64(60) {
		t.Fatalf("timeout = %v", requests[0].Body["timeout"])
	}
}

// TestIncusInstanceStopSkipsStoppedInstance pins the Alpine workaround: a stopped
// instance is not sent a second stop.
func TestIncusInstanceStopSkipsStoppedInstance(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01/state" {
			return http.StatusOK, incusSyncEnvelope(`{"status":"stopped"}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	if _, err := IncusInstanceStop(t.Context(), client, "vm-test01", false, IncusInstanceBusyRetryOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Count() != 1 {
		t.Fatalf("expected only the state read, got %#v", node.Requests())
	}
}

// TestIncusInstanceStopRetriesBusyOperation pins the busy-retry contract: a
// `busy running a "..."` error is retried, and the retry observes the now-stopped
// state instead of sending a second stop.
func TestIncusInstanceStopRetriesBusyOperation(t *testing.T) {
	var stateReads int
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodGet && request.Path == "/1.0/operations?recursion=1":
			// Best-effort diagnostics for the busy retry.
			return http.StatusOK, incusSyncEnvelope(`[]`)
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01/state":
			stateReads++
			if stateReads == 1 {
				return http.StatusOK, incusSyncEnvelope(`{"status":"Running"}`)
			}
			return http.StatusOK, incusSyncEnvelope(`{"status":"Stopped"}`)
		case request.Method == http.MethodPut && request.Path == "/1.0/instances/vm-test01/state":
			return http.StatusInternalServerError, incusErrorEnvelope(`Instance "vm-test01" is busy running a "update" operation`, 500)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := IncusInstanceStop(t.Context(), client, "vm-test01", false, IncusInstanceBusyRetryOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stateReads != 2 {
		t.Fatalf("state reads = %d, want 2 (retry after the busy error)", stateReads)
	}
	stopAttempts := 0
	for _, request := range node.Requests() {
		if request.Method == http.MethodPut && request.Body["action"] == "stop" {
			stopAttempts++
		}
	}
	if stopAttempts != 1 {
		t.Fatalf("stop attempts = %d, want 1 (the retry must see the stopped state)", stopAttempts)
	}
}

// TestIncusInstanceExtractBusyOperationName pins the regex, including the
// non-busy passthrough.
func TestIncusInstanceExtractBusyOperationName(t *testing.T) {
	cases := map[string]string{
		`Instance "vm-1" is busy running a "update" operation`: "update",
		`instance is BUSY RUNNING A "snapshot" OPERATION`:      "snapshot",
		`some other failure`: "",
	}
	for message, want := range cases {
		if got := incusInstanceExtractBusyOperationName(errors.New(message)); got != want {
			t.Errorf("extract(%q) = %q, want %q", message, got, want)
		}
	}
	if got := incusInstanceExtractBusyOperationName(nil); got != "" {
		t.Errorf("nil error = %q", got)
	}
}

// ==================== devices ====================

// TestIncusInstanceAddDevicePreservesInstance pins that the whole instance object is
// spread back, so a PUT cannot erase description or unrelated devices.
func TestIncusInstanceAddDevicePreservesInstance(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch request.Method {
		case http.MethodGet:
			return http.StatusOK, incusSyncEnvelope(`{
				"name":"vm-test01","type":"container","status":"Running","status_code":103,
				"description":"keep me",
				"config":{"limits.cpu":"1"},
				"devices":{"root":{"path":"/","pool":"default","type":"disk"}}
			}`)
		case http.MethodPut:
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := IncusInstanceAddDevice(t.Context(), client, "vm-test01", "proxy-22",
		map[string]any{"type": "proxy", "listen": "tcp:0.0.0.0:22022"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requests := node.Requests()
	if len(requests) != 2 || requests[1].Method != http.MethodPut {
		t.Fatalf("requests = %#v", requests)
	}
	body := requests[1].Body
	if body["description"] != "keep me" {
		t.Fatalf("description was dropped: %#v", body)
	}
	devices := body["devices"].(map[string]any)
	if _, ok := devices["root"]; !ok {
		t.Fatalf("root device was dropped: %#v", devices)
	}
	if _, ok := devices["proxy-22"]; !ok {
		t.Fatalf("new device missing: %#v", devices)
	}
}

// TestIncusInstanceRemoveMissingDeviceIsNoOp pins the early return: no PUT at all.
func TestIncusInstanceRemoveMissingDeviceIsNoOp(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		if request.Method == http.MethodGet {
			return http.StatusOK, incusSyncEnvelope(`{"name":"vm-test01","devices":{"root":{"path":"/"}}}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	result, err := IncusInstanceRemoveDevice(t.Context(), client, "vm-test01", "proxy-22")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("result = %s, want nil", string(result))
	}
	if node.Count() != 1 {
		t.Fatalf("expected only the GET, got %#v", node.Requests())
	}
}

// ==================== update ====================

// TestIncusInstanceUpdateMergesConfig pins updateInstance: config is merged (never
// replaced), the rest of the instance object survives and a nil update is dropped.
func TestIncusInstanceUpdateMergesConfig(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch request.Method {
		case http.MethodGet:
			return http.StatusOK, incusSyncEnvelope(`{
				"name":"vm-test01","type":"container","description":"keep me",
				"config":{"limits.cpu":"1","limits.memory":"1024MB"},
				"devices":{"root":{"path":"/"}}
			}`)
		case http.MethodPut:
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	_, err := UpdateInstance(t.Context(), client, "vm-test01", map[string]any{
		"config": map[string]any{"limits.memory": "2048MB"},
		"pool":   nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requests := node.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %#v", requests)
	}
	body := requests[1].Body
	if body["description"] != "keep me" {
		t.Fatalf("description was dropped: %#v", body)
	}
	settings := body["config"].(map[string]any)
	if settings["limits.cpu"] != "1" {
		t.Fatalf("existing config was not merged: %#v", settings)
	}
	if settings["limits.memory"] != "2048MB" {
		t.Fatalf("config update missing: %#v", settings)
	}
	if _, ok := body["pool"]; ok {
		t.Fatalf("nil update must be dropped: %#v", body)
	}
}

// ==================== provision seam ====================

// TestIncusInstanceProvisionCreatesThenStarts pins the seam body: one POST and one
// state PUT, in that order, with the built config.
func TestIncusInstanceProvisionCreatesThenStarts(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			return http.StatusOK, incusSyncEnvelope(`{"name":"vm-test01"}`)
		case request.Method == http.MethodPut && request.Path == "/1.0/instances/vm-test01/state":
			return http.StatusOK, incusSyncEnvelope(`{}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	swapSize := int32(0)
	err := IncusInstanceProvision(t.Context(), client, IncusInstanceProvisionInput{
		Config: IncusInstanceProvisionConfig{
			Name:          "vm-test01",
			Image:         "ubuntu/24.04",
			CPU:           200,
			Memory:        2048,
			Disk:          20480,
			SwapEnabled:   false,
			SwapSize:      &swapSize,
			NetworkMode:   "nat",
			InstanceType:  "container",
			IPv4Address:   incusStringPtr("10.10.1.5"),
			HostInterface: "eth0",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requests := node.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Method != http.MethodPost || requests[0].Body["name"] != "vm-test01" {
		t.Fatalf("create request = %#v", requests[0])
	}
	settings := requests[0].Body["config"].(map[string]any)
	if settings["limits.cpu"] != "2" || settings["limits.memory"] != "2048MB" {
		t.Fatalf("config = %#v", settings)
	}
	if requests[1].Method != http.MethodPut || requests[1].Body["action"] != "start" {
		t.Fatalf("start request = %#v", requests[1])
	}
}

// TestIncusInstanceProvisionStopsWhenCreateFails pins that a failed create never
// reaches the start call.
func TestIncusInstanceProvisionStopsWhenCreateFails(t *testing.T) {
	client, node := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch {
		case request.Method == http.MethodPost && request.Path == "/1.0/instances":
			return http.StatusInternalServerError, incusErrorEnvelope("no space left", 500)
		case request.Method == http.MethodGet && request.Path == "/1.0/instances/vm-test01":
			return http.StatusNotFound, incusErrorEnvelope("Instance not found", 404)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	err := IncusInstanceProvision(t.Context(), client, IncusInstanceProvisionInput{
		Config: IncusInstanceProvisionConfig{
			Name:          "vm-test01",
			Image:         "ubuntu/24.04",
			CPU:           100,
			Memory:        1024,
			Disk:          10240,
			NetworkMode:   "nat",
			InstanceType:  "container",
			IPv4Address:   incusStringPtr("10.10.1.5"),
			HostInterface: "eth0",
		},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, request := range node.Requests() {
		if request.Method == http.MethodPut {
			t.Fatalf("start must not run after a failed create: %#v", request)
		}
	}
}

// TestIncusInstanceProvisionRequiresClient pins the nil guard.
func TestIncusInstanceProvisionRequiresClient(t *testing.T) {
	if err := IncusInstanceProvision(t.Context(), nil, IncusInstanceProvisionInput{}); err == nil {
		t.Fatal("expected an error for a nil client")
	}
}

// TestIncusInstanceListFollowsURLs pins that the URL list is resolved into instances.
func TestIncusInstanceListFollowsURLs(t *testing.T) {
	client, _ := incusInstanceTestClient(t, func(request incusInstanceTestRequest) (int, string) {
		switch request.Path {
		case "/1.0/instances":
			return http.StatusOK, incusSyncEnvelope(`["/1.0/instances/a","/1.0/instances/b"]`)
		case "/1.0/instances/a":
			return http.StatusOK, incusSyncEnvelope(`{"name":"a","status":"Running","status_code":103}`)
		case "/1.0/instances/b":
			return http.StatusOK, incusSyncEnvelope(`{"name":"b","status":"Stopped","status_code":102}`)
		}
		t.Errorf("unexpected request %s %s", request.Method, request.Path)
		return http.StatusInternalServerError, incusErrorEnvelope("unexpected", 500)
	})

	instances, err := IncusInstanceList(t.Context(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(instances) != 2 || instances[0].Name != "a" || instances[1].Name != "b" {
		t.Fatalf("instances = %#v", instances)
	}
}
