package workers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func testHeavyBaseInstance(hostID int32) *db.InstanceDetailRecord {
	pkgID := int32(10)
	pool := "default"
	ipv4 := "10.10.0.5"
	return &db.InstanceDetailRecord{
		ID:              101,
		IncusID:         "u1-oldinst",
		Name:            "my-vps",
		UserID:          7,
		HostID:          hostID,
		PackageID:       &pkgID,
		StoragePoolName: &pool,
		Image:           "debian-12",
		Status:          models.InstanceStatusRunning,
		CPU:             2,
		Memory:          1024,
		Disk:            10240,
		IPv4:            &ipv4,
		NetworkMode:     models.NetworkModeNat,
	}
}

func testHeavyBasePackage() *db.PackageDetailRow {
	return &db.PackageDetailRow{
		ID:           10,
		Name:         "Starter",
		InstanceType: "container",
		IOLimitMode:  "throughput",
		CPUMax:       2,
		MemoryMax:    1024,
		DiskMax:      10240,
		PortLimit:    20,
		HostIDs:      []int32{1, 2},
	}
}

func testHeavyBaseHost(id int32, name string) *db.HostsRecord {
	pool := "default"
	return &db.HostsRecord{
		ID:              id,
		Name:            name,
		URL:             "https://127.0.0.1:8443",
		Status:          "online",
		StorageDriver:   "dir",
		StoragePath:     &pool,
		IPv6Mode:        0,
		InstanceType:    "container",
		CPUAllowanceMax: 100,
		MemoryMax:       8192,
		StorageSize:     102400,
	}
}

func testHeavyBaseSeams(instance *db.InstanceDetailRecord, host *db.HostsRecord, pkg *db.PackageDetailRow) *instanceTaskHeavySeams {
	return &instanceTaskHeavySeams{
		updateProgress:       func(context.Context, int32, string) error { return nil },
		setTaskNewInstanceID: func(context.Context, int32, int32) error { return nil },
		getInstance:          func(context.Context, int32) (*db.InstanceDetailRecord, error) { return instance, nil },
		getHost:              func(context.Context, int32) (*db.HostsRecord, error) { return host, nil },
		getClientForHost:     func(context.Context, *db.HostsRecord) (*lib.IncusClient, error) { return &lib.IncusClient{}, nil },
		getPackage:           func(context.Context, int32) (*db.PackageDetailRow, error) { return pkg, nil },
		getNodeSelectors:     func(context.Context, int32) (json.RawMessage, error) { return json.RawMessage("[]"), nil },
		getPlan:              func(context.Context, int32) (*models.PackagePlan, error) { return nil, nil },
		getSSHKey:            func(context.Context, int32) (*db.SshKeyData, error) { return nil, nil },
		getUserSSHKeys:       func(context.Context, int32) ([]db.SshKeyData, error) { return nil, nil },
		getCommands:          func(context.Context, []int32) ([]db.CustomInitCommandData, error) { return nil, nil },
		allocateNatIPv4: func(context.Context, int32, *int32) (*string, error) {
			ip := "10.10.0.88"
			return &ip, nil
		},
		ipv6Exists:        func(context.Context, string) (bool, error) { return false, nil },
		randomIPv6:        func(string) (string, error) { return "", nil },
		allocatePort:      func(context.Context, int32, models.Protocol, []int32) (int32, bool, error) { return 20001, true, nil },
		createPort:        func(context.Context, db.PortMappingCreateInput) (int32, error) { return 1, nil },
		listPorts:         func(context.Context, int32) ([]db.PortMappingRecord, error) { return nil, nil },
		createIP:          func(context.Context, db.IpAddressesCreateData) error { return nil },
		reserveResources:  func(context.Context, db.ReserveResourcesOptions) error { return nil },
		rollbackResources: func(context.Context, db.RollbackResourcesOptions) error { return nil },
		selectAndReserve: func(context.Context, db.HostsReserveOptions) (*db.HostsSelectionRecord, error) {
			return &db.HostsSelectionRecord{ID: host.ID, Name: host.Name}, nil
		},
		calculateHostRes: func(context.Context, int32) (db.HostsInstanceResourceSum, error) {
			return db.HostsInstanceResourceSum{}, nil
		},
		updateHostRes:    func(context.Context, int32, db.HostsResourceUpdate) error { return nil },
		setHostNATPorts:  func(context.Context, int32, int32) error { return nil },
		hostNATPorts:     func(context.Context, int32) (int32, bool) { return 0, true },
		countSourcePorts: func(context.Context, int32) (int32, error) { return 0, nil },
		hasStoragePool:   func(context.Context, int32, string) (bool, error) { return true, nil },
		resolveNewPool: func(context.Context, int32, *int32) (*string, error) {
			p := "default"
			return &p, nil
		},
		imageAvailability: func(context.Context, string, int32, db.SystemImageAvailabilityOptions) (db.HostImageAvailability, error) {
			return db.HostImageAvailability{OK: true}, nil
		},
		resolveTraffic: func(context.Context, *gorm.DB, lib.TrafficMultiplierResolveParams) (*int64, error) {
			return nil, nil
		},
		createInstanceRow:     func(context.Context, db.InstanceCreateInput) (int32, error) { return 202, nil },
		setInstanceIPv4:       func(context.Context, int32, string) error { return nil },
		setInstanceIPv6:       func(context.Context, int32, string) error { return nil },
		setInstanceStopped:    func(context.Context, int32) error { return nil },
		findInstanceByIncusID: func(context.Context, string) (int32, error) { return 0, nil },
		deleteInstanceRow:     func(context.Context, int32) error { return nil },
		cleanupRecreated:      func(context.Context, int32, int32) (int32, error) { return 0, nil },
		deleteOldIPv4Rows:     func(context.Context, int32, string) error { return nil },
		deleteAllIPs:          func(context.Context, int32) error { return nil },
		deleteIPv6Subnets:     func(context.Context, int32) error { return nil },
		markInstanceError:     func(context.Context, int32) error { return nil },
		resetCloudInit:        func(context.Context, int32) error { return nil },
		deleteSnapshotRows:    func(context.Context, int32) error { return nil },
		switchRecreated:       func(context.Context, int32, instanceTaskRecreateSwitch) error { return nil },
		switchHost:            func(context.Context, int32, instanceTaskHostSwitch) (int32, error) { return 0, nil },
		listProxySites:        func(context.Context, int32) ([]instanceTaskProxySite, error) { return nil, nil },
		deleteCaddySite:       func(context.Context, *db.HostsRecord, []string) error { return nil },
		createLog:             func(context.Context, int32, string, string, string, int32) {},
		sendNotification:      func(context.Context, int32, lib.EventType, lib.EventData) {},
		incusGetInstance: func(context.Context, *lib.IncusClient, string) (*lib.IncusInstance, error) {
			return &lib.IncusInstance{}, nil
		},
		incusGetState: func(context.Context, *lib.IncusClient, string) (incusStateNetwork, error) {
			return incusStateNetwork{Status: "Stopped"}, nil
		},
		incusCreate: func(context.Context, *lib.IncusClient, map[string]any, *int32) error { return nil },
		incusStart:  func(context.Context, *lib.IncusClient, string) error { return nil },
		incusStop: func(context.Context, *lib.IncusClient, string, bool, lib.IncusInstanceBusyRetryOptions) error {
			return nil
		},
		incusDelete:         func(context.Context, *lib.IncusClient, string) error { return nil },
		incusUpdate:         func(context.Context, *lib.IncusClient, string, map[string]any) error { return nil },
		incusCopy:           func(context.Context, *lib.IncusClient, map[string]any) error { return nil },
		incusListSnapshots:  func(context.Context, *lib.IncusClient, string) ([]lib.IncusSnapshot, error) { return nil, nil },
		incusDeleteSnapshot: func(context.Context, *lib.IncusClient, string, string) error { return nil },
		encryptPassword:     func(plain string) (string, error) { return "enc:" + plain, nil },
		decryptPassword:     func(cipher string) string { return strings.TrimPrefix(cipher, "enc:") },
		sleep:               func(context.Context, time.Duration) error { return nil },
		newIncusID:          func(int32) string { return "u7-newinst1" },
		now:                 func() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC) },
	}
}

func TestHeavyChangeHostReleasesDeletedPortsAndReconciles(t *testing.T) {
	sourceHost := testHeavyBaseHost(1, "src-host")
	targetHost := testHeavyBaseHost(2, "dst-host")
	instance := testHeavyBaseInstance(sourceHost.ID)
	pkg := testHeavyBasePackage()

	seams := testHeavyBaseSeams(instance, sourceHost, pkg)
	seams.getHost = func(_ context.Context, id int32) (*db.HostsRecord, error) {
		if id == targetHost.ID {
			return targetHost, nil
		}
		return sourceHost, nil
	}
	seams.selectAndReserve = func(context.Context, db.HostsReserveOptions) (*db.HostsSelectionRecord, error) {
		pool := "default"
		return &db.HostsSelectionRecord{ID: targetHost.ID, Name: targetHost.Name, StoragePoolName: &pool}, nil
	}
	seams.getSSHKey = func(_ context.Context, id int32) (*db.SshKeyData, error) {
		return &db.SshKeyData{ID: id, UserID: instance.UserID, PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey"}, nil
	}
	seams.switchHost = func(context.Context, int32, instanceTaskHostSwitch) (int32, error) {
		return 3, nil
	}
	seams.countSourcePorts = func(context.Context, int32) (int32, error) { return 4, nil }
	seams.hostNATPorts = func(context.Context, int32) (int32, bool) { return 2, true }

	var rollbackCall db.RollbackResourcesOptions
	seams.rollbackResources = func(_ context.Context, opts db.RollbackResourcesOptions) error {
		rollbackCall = opts
		return nil
	}
	var updatedNATPorts int32
	seams.setHostNATPorts = func(_ context.Context, hostID, count int32) error {
		if hostID == sourceHost.ID {
			updatedNATPorts = count
		}
		return nil
	}

	restore := setInstanceTaskHeavySeams(seams)
	defer restore()

	targetID := targetHost.ID
	sshKeyID := int32(9)
	rt := &instanceTaskRuntime{
		Task: &instanceTaskRecord{
			ID:           50,
			InstanceID:   instance.ID,
			HostID:       targetHost.ID,
			UserID:       instance.UserID,
			TaskType:     string(models.InstanceTaskTypeChangeHost),
			TargetHostID: &targetID,
			SSHKeyID:     &sshKeyID,
		},
		Instance: instance,
		Host:     targetHost,
		Client:   &lib.IncusClient{},
	}

	if err := executeInstanceChangeHostTask(t.Context(), rt); err != nil {
		t.Fatalf("executeInstanceChangeHostTask failed: %v", err)
	}
	if rollbackCall.HostID != sourceHost.ID || rollbackCall.PortCount != 3 {
		t.Fatalf("rollback = %+v, want HostID=%d PortCount=3", rollbackCall, sourceHost.ID)
	}
	if updatedNATPorts != 4 {
		t.Fatalf("setHostNATPorts = %d, want 4", updatedNATPorts)
	}
}

func TestHeavyChangeHostCompensatesTargetOnPreSwitchFailure(t *testing.T) {
	sourceHost := testHeavyBaseHost(1, "src-host")
	targetHost := testHeavyBaseHost(2, "dst-host")
	instance := testHeavyBaseInstance(sourceHost.ID)
	pkg := testHeavyBasePackage()

	seams := testHeavyBaseSeams(instance, sourceHost, pkg)
	seams.getHost = func(_ context.Context, id int32) (*db.HostsRecord, error) {
		if id == targetHost.ID {
			return targetHost, nil
		}
		return sourceHost, nil
	}
	seams.selectAndReserve = func(context.Context, db.HostsReserveOptions) (*db.HostsSelectionRecord, error) {
		pool := "default"
		return &db.HostsSelectionRecord{ID: targetHost.ID, Name: targetHost.Name, StoragePoolName: &pool}, nil
	}
	seams.getSSHKey = func(_ context.Context, id int32) (*db.SshKeyData, error) {
		return &db.SshKeyData{ID: id, UserID: instance.UserID, PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey"}, nil
	}
	seams.incusStart = func(context.Context, *lib.IncusClient, string) error {
		return errors.New("start failed")
	}

	var deletedName string
	seams.incusDelete = func(_ context.Context, _ *lib.IncusClient, name string) error {
		deletedName = name
		return nil
	}
	var rolledBackHost int32
	seams.rollbackResources = func(_ context.Context, opts db.RollbackResourcesOptions) error {
		rolledBackHost = opts.HostID
		return nil
	}

	restore := setInstanceTaskHeavySeams(seams)
	defer restore()

	targetID := targetHost.ID
	sshKeyID := int32(9)
	rt := &instanceTaskRuntime{
		Task: &instanceTaskRecord{
			ID:           51,
			InstanceID:   instance.ID,
			HostID:       targetHost.ID,
			UserID:       instance.UserID,
			TaskType:     string(models.InstanceTaskTypeChangeHost),
			TargetHostID: &targetID,
			SSHKeyID:     &sshKeyID,
		},
		Instance: instance,
		Host:     targetHost,
		Client:   &lib.IncusClient{},
	}

	if err := executeInstanceChangeHostTask(t.Context(), rt); err == nil {
		t.Fatal("expected error from incusStart")
	}
	if deletedName != "u7-newinst1" {
		t.Fatalf("compensated delete = %q, want u7-newinst1", deletedName)
	}
	if rolledBackHost != targetHost.ID {
		t.Fatalf("rolledBackHost = %d, want %d", rolledBackHost, targetHost.ID)
	}
}

func TestHeavyCloneCompensatesOnDBFailure(t *testing.T) {
	host := testHeavyBaseHost(1, "host-a")
	instance := testHeavyBaseInstance(host.ID)
	pkg := testHeavyBasePackage()

	seams := testHeavyBaseSeams(instance, host, pkg)
	seams.createPort = func(context.Context, db.PortMappingCreateInput) (int32, error) {
		return 0, errors.New("port insert failed")
	}
	seams.listPorts = func(context.Context, int32) ([]db.PortMappingRecord, error) {
		return []db.PortMappingRecord{{Protocol: models.ProtocolTcp, PublicPort: 20001, PrivatePort: 22}}, nil
	}
	seams.incusGetInstance = func(context.Context, *lib.IncusClient, string) (*lib.IncusInstance, error) {
		return &lib.IncusInstance{
			Devices: map[string]map[string]any{
				"eth0":    {"type": "nic"},
				"port-22": {"type": "proxy", "listen": "tcp:0.0.0.0:20001", "connect": "tcp:10.10.0.5:22"},
			},
		}, nil
	}

	var deletedClone string
	var deletedRowID int32
	var createdIncusID string
	seams.createInstanceRow = func(_ context.Context, input db.InstanceCreateInput) (int32, error) {
		createdIncusID = input.IncusID
		return 202, nil
	}
	seams.findInstanceByIncusID = func(_ context.Context, incusID string) (int32, error) {
		if incusID == createdIncusID {
			return 202, nil
		}
		return 0, nil
	}
	seams.incusDelete = func(_ context.Context, _ *lib.IncusClient, name string) error {
		deletedClone = name
		return nil
	}
	seams.deleteInstanceRow = func(_ context.Context, id int32) error {
		deletedRowID = id
		return nil
	}

	restore := setInstanceTaskHeavySeams(seams)
	defer restore()

	rt := &instanceTaskRuntime{
		Task: &instanceTaskRecord{
			ID:         52,
			InstanceID: instance.ID,
			HostID:     host.ID,
			UserID:     instance.UserID,
			TaskType:   string(models.InstanceTaskTypeClone),
		},
		Instance: instance,
		Host:     host,
		Client:   &lib.IncusClient{},
	}

	if err := executeInstanceCloneTask(t.Context(), rt); err == nil {
		t.Fatal("expected error from createPort")
	}
	if deletedClone != "u7-newinst1" {
		t.Fatalf("deletedClone = %q, want u7-newinst1", deletedClone)
	}
	if deletedRowID != 202 {
		t.Fatalf("deletedRowID = %d, want 202", deletedRowID)
	}
}
