# Hosts and Resource Pools

Hosts and resource pools connect host capacity to PayIncus so the system can sell and deliver instances by host, plan and capacity.

> The hosting subsystem (user-hosted nodes, hosted packages, hosting earnings, hosting zones) has been removed entirely. Hosts are managed by administrators only.

## Resource Model

| Object | Description |
| --- | --- |
| Host | A physical or virtual host running Incus and the Agent. |
| Plan | CPU, memory, disk, traffic, price and billing cycle offered to users. |
| Image | OS image selectable during instance creation. |
| Resource pool | Aggregated capacity and supply. |

For new hosts, use Ubuntu 22.04+ or Debian 12/13. Debian 11 remains a compatibility path only; new hosts should prefer Debian 12/13.

KVM plans can only be bound to hosts that support virtual machines. The host must expose a usable `/dev/kvm` and be marked as `vm` or `both` in the admin host type. Hosts without KVM support should be used as container hosts only. The backend rejects VM plans bound to container-only hosts and rejects changing a host to container-only while VM plans are still bound to it, so users do not create instances that immediately enter an error state.

## Network Delivery Modes

PayIncus currently treats these network modes as supported sales and delivery targets:

- `nat`: IPv4 NAT. Instances receive an internal IPv4 address and port mappings use the host NAT port pool.
- `nat_ipv6`: IPv4 NAT plus dedicated IPv6. Instances receive an internal NAT IPv4 address and a routed dedicated IPv6 address.
- `ipv6_only`: dedicated IPv6 only. This mode does not consume NAT IPv4 ports.
- `public_ipv4`: dedicated IPv4. Instances bind a public IPv4 address through an Incus routed NIC.
- `public_ipv4_ipv6`: dedicated IPv4 plus dedicated IPv6. Instances receive both a public routed IPv4 address and a routed dedicated IPv6 address.

`nat_ipv6_nat` and `ipv6_nat` remain only for legacy data compatibility and should not be used for newly published plans.

Dedicated IPv4 plans require a public IPv4 pool on the host first. Configure it from the host detail page under the "Dedicated IPv4" tab. A pool stores gateway, prefix length, DNS and address entries. During instance creation PayIncus reserves one free IPv4 address after locked host capacity reservation; failed or timed-out provisioning releases the address. Hosts without free dedicated IPv4 addresses are excluded from dedicated IPv4 delivery.

## Admin Capabilities

- Inspect global host capacity, online status and Agent heartbeats.
- Manage plans, images, resource pools and supply state.
- Troubleshoot Agent reporting, Incus connectivity and resource statistics.

## Agent Relationship

The Agent is the runtime data source for hosts:

- Host heartbeat.
- CPU, memory, disk and traffic reporting.
- Instance status reporting.
- Agent version and binary download proxying.
- Signature verification and replay protection.

## Key Backend Modules

- `hosts`: host management.
- `resource-pool`: resource pool requests, capacity and consistency.
- `packages`: package management.
- `images`: image management.
- `agent`: heartbeat, reporting and Agent releases.

## Risks

- Host capacity must be validated against both real Agent reports and admin configuration.
- Publishing or unpublishing plans must not break renewal and billing for already purchased instances.
- Do not keep allocating resources blindly while the Agent is offline.
- VM/KVM plans, host type and visible images must stay consistent. If Incus returns `KVM support is missing (no /dev/kvm)`, the host cannot deliver KVM instances; enable nested virtualization, replace the host with one that supports KVM, or republish the plan as a container plan.
- When Incus returns `not authorized`, first check whether the host trusts the current panel certificate; regenerate the node install command and run it on the host to refresh the `panel` trust entry.
- On Debian/cloud kernels, prefer LVM when creating storage pools; only choose ZFS after confirming `modprobe zfs` works. If creating a ZFS pool returns `modprobe: FATAL: Module zfs not found`, the host has no usable ZFS kernel module. Fix `linux-headers-$(uname -r)` / `zfs-dkms`, or switch to LVM, Btrfs or a DIR pool.
- Resource deletion and billing settlement must keep audit records.

## Verification Checklist

- Agent heartbeat and resource reporting work.
- The admin panel can read host storage pools and can create or link storage pools.
- Dedicated IPv4 hosts have address pools configured and available addresses are recognized by plan capacity checks.
- Host capacity changes are reflected in the admin panel.
- Instance delivery never targets an unavailable host.
