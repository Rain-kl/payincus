// Wiring for the port-mapping data layer.
//
// lib/instance_port_mapping.go exposes InstancePortMappingBackend as a seam because
// server/src/db/port-mappings.ts had not been ported when it landed. That file now
// exists as db/port_mappings.go, and this file installs it.
//
// Why the registration lives here instead of in port_mappings.go's init(): package
// db must not import lib — lib imports db (see db/billing_operations.go and
// db/ip_addresses.go, which both note the same rule). The backend interface's
// CreatePortMapping takes lib.InstancePortMappingCreateInput, so a db type cannot
// satisfy it at all, let alone register itself. Registering from the lib side is the
// established direction for every other db→lib seam in this codebase
// (lib/network_payload_builder.go sets db.IpAddressesNatIpv4PoolSource,
// lib/tunnel_manager.go sets db.InstanceEgressHostSupportsControl).
package lib

import (
	"context"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// dbInstancePortMappingBackend adapts db/port_mappings.go to the seam. Each method
// is a 1:1 call — no extra locking, retry or validation is added here, because the
// TypeScript's addInstancePortMapping is what owns that logic and it is already
// ported in instance_port_mapping.go.
type dbInstancePortMappingBackend struct{}

// The adapter must keep satisfying the seam; a mismatch is a compile error rather
// than a runtime surprise in the instance creation path.
var _ InstancePortMappingBackend = dbInstancePortMappingBackend{}

// dbInstancePortMappingAllocatePort is the adapter's AllocatePort. The seam has no
// excludedPorts parameter because the TypeScript call site
// (addInstancePortMapping) never passes one.
func (dbInstancePortMappingBackend) AllocatePort(ctx context.Context, hostID int32, protocol InstancePortMappingProtocol) (int32, bool, error) {
	return db.PortMappingsAllocatePort(ctx, hostID, protocol, nil)
}

func (dbInstancePortMappingBackend) CheckPortInUse(ctx context.Context, hostID int32, port int32, protocol InstancePortMappingProtocol) (bool, error) {
	mapping, err := db.PortMappingsCheckPortInUse(ctx, hostID, port, protocol)
	if err != nil {
		return false, err
	}
	return mapping != nil, nil
}

func (dbInstancePortMappingBackend) CreatePortMapping(ctx context.Context, input InstancePortMappingCreateInput) (int32, error) {
	return db.PortMappingsCreate(ctx, db.PortMappingCreateInput{
		InstanceID:    input.InstanceID,
		HostID:        input.HostID,
		Protocol:      input.Protocol,
		PublicPort:    input.PublicPort,
		PrivatePort:   input.PrivatePort,
		Remark:        input.Remark,
		SystemManaged: input.SystemManaged,
	})
}

func (dbInstancePortMappingBackend) DeletePortMapping(ctx context.Context, id int32) error {
	return db.PortMappingsDelete(ctx, id)
}

// instancePortMappingWiredBackend records what init installed. It exists only so a
// test can prove the registration happened: other tests in this package reset the
// seam to nil (TestAddInstancePortMappingRequiresBackend), and the registration must
// not depend on test order.
var instancePortMappingWiredBackend InstancePortMappingBackend

// init installs the database-backed port-mapping layer, which is what makes
// AddInstancePortMapping return real allocations instead of
// ErrInstancePortMappingBackendUnavailable.
func init() {
	backend := dbInstancePortMappingBackend{}
	instancePortMappingWiredBackend = backend
	RegisterInstancePortMappingBackend(backend)
}
