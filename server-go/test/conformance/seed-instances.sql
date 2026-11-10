-- Instances-module fixture rows for test/conformance/requests-instances.json.
--
-- The conformance harness replays the same request list against the Node and the
-- Go backend on the *same* database and diffs status + JSON body, so the read
-- cases need real rows before either server starts.
--
-- Every id is >= 960001 so it cannot collide with the auto-increment sequence or
-- with the 900001 / 920001 / 930001 / 940001 / 950001 ranges of the other
-- fixtures. The file is idempotent (ON CONFLICT DO NOTHING), so re-running never
-- duplicates rows.
--
-- Writes performed by the request list (all idempotent / restorable):
--   PATCH /api/instances/960001/order {...}           (display_order only)
--   PATCH /api/instances/960001/quota {}              (no-op update)
--   PATCH /api/instances/960001/rename {"name": ...}  (renamed back by a 2nd case)
-- No instance status, balance or task row is mutated by any case.
--
-- status avoids 'creating' on purpose: Node's stuck-creating sweeper rewrites
-- those rows and the two backends would then race the fixture.
--
-- The host connection URL points at https://127.0.0.1:1 so any Incus path fails
-- fast and deterministically on both backends (nothing in the request list relies
-- on reaching a node).

BEGIN;

-- 960001: online container host owned by admin.
INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  nat_public_ip, nat_port_start, nat_port_end, cpu_used, memory_used, disk_used,
  tags, nat_ports_used_count, cpu_allowance_max, memory_max, instance_type,
  storage_driver, storage_type, storage_size, ipv6_mode, ipv6_subnet,
  ipv6_gateway, ipv6_parent_interface, enable_api, tunnel_enabled,
  target_host, target_port, is_installed, cert_download_count, caddy_enabled,
  caddy_port, transfer_enabled, traffic_reset_day, announcement,
  notify_purchase, notify_renew, notify_destroy, enable_resource_pool,
  created_at, updated_at
)
SELECT 960001, u.id, 'parity-instances-host', 'https://127.0.0.1:1', 'Parity DC 960001',
       'us', 'x86_64', 'online',
       '203.0.113.60', 26000, 26999, 0, 0, 0,
       '[]'::jsonb, 0, 10000, 1048576, 'container',
       'zfs', 'loop', 1000, 1, 'fd00:9601:1::/64',
       'fd00:9601:1::1', 'eth0', true, false,
       '127.0.0.1', 8443, true, 0, false,
       8444, true, 1, 'parity instances fixture',
       true, true, false, true,
       now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 960001: parityuser-owned free package bound to host 960001.
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, privileged, nested,
                      allow_instance_deletion, io_limit_mode, limits_read, limits_write,
                      limits_read_iops, limits_write_iops, limits_ingress, limits_egress,
                      limits_processes, limits_cpu_priority, boot_autostart,
                      boot_autostart_priority, boot_autostart_delay,
                      boot_host_shutdown_timeout, created_at)
SELECT 960001, u.id, 'Parity Instances Package', 'instances fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       true, false, false, true, 'throughput', '100MB', '100MB', 500, 500,
       '300Mbit', '300Mbit', 500, 10, true, 20, 15, 30,
       now() - interval '20 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960002: admin-owned package (NOT shared) — parityuser must get 403.
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, created_at)
SELECT 960002, u.id, 'Parity Instances Admin Package', 'instances fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       true, now() - interval '20 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 960003: admin-owned package that forbids instance deletion.
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, allow_instance_deletion,
                      created_at)
SELECT 960003, u.id, 'Parity Instances No-Delete Package', 'instances fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       true, false, now() - interval '20 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO package_hosts (id, package_id, host_id, storage_pool_name, traffic_multiplier, created_at)
VALUES (960001, 960001, 960001, 'parity-pool', 1.000, now() - interval '20 days')
ON CONFLICT (id) DO NOTHING;

-- Plan prices are CENTS (¥30.00 -> 3000.00).
INSERT INTO package_plans (id, package_id, name, cpu, memory, disk, port_limit, snapshot_limit,
                           backup_limit, site_limit, traffic_limit, traffic_limit_speed, price,
                           billing_cycle, is_active, is_sold_out, sort_order, swap_size,
                           created_at, updated_at)
VALUES (960001, 960001, 'Parity Instances Plan', 1, 1024, 10240, 20, 5, 3, 10,
        1099511627776, '1Mbit', 3000.00, 1, true, false, 0, 0,
        now() - interval '20 days', now() - interval '20 days')
ON CONFLICT (id) DO NOTHING;

-- 960001: parityuser's free stopped instance (the main read fixture).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       ssh_port, root_password, monthly_traffic_limit, monthly_traffic_used,
                       auto_renew, version, created_at, updated_at)
SELECT 960001, 'parity-instances-960001', 'parity-instances-free', u.id, 960001, 960001, NULL,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, 22, NULL, 1099511627776, 0,
       false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960002: admin's instance — parityuser is not the owner and not the host owner.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       ssh_port, auto_renew, version, created_at, updated_at)
SELECT 960002, 'parity-instances-960002', 'parity-instances-admin', u.id, 960001, 960002, NULL,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, 22, false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 960003: parityuser's suspended instance (manual reason, not 'expired').
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       suspended_at, suspend_reason, auto_renew, version,
                       created_at, updated_at)
SELECT 960003, 'parity-instances-960003', 'parity-instances-suspended', u.id, 960001, 960001, NULL,
       'images:debian/12', 'suspended', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, now() - interval '2 days', 'manual', false, 0,
       now() - interval '20 days', now() - interval '2 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960006: parityuser's stopped instance that carries the pending task fixture.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 960006, 'parity-instances-960006', 'parity-instances-task', u.id, 960001, 960001, NULL,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'nat', 1,
       20, 5, 3, 10, false, 0, now() - interval '19 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960007: parityuser's PAID instance (paid instances cannot be deleted by the owner).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 960007, 'parity-instances-960007', 'parity-instances-paid', u.id, 960001, 960001, 960001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 2,
       20, 5, 3, 10, 30.00, 1, now() + interval '30 days', false, 0,
       now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960008: parityuser's RUNNING free instance (start/rebuild/clone guards).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       ipv4, auto_renew, version, created_at, updated_at)
SELECT 960008, 'parity-instances-960008', 'parity-instances-running', u.id, 960001, 960001, NULL,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 3,
       20, 5, 3, 10, '10.10.0.60', false, 0, now() - interval '18 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960009: ipv6_only instance — port mapping is rejected with PORT_MAPPING_NAT_ONLY.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 960009, 'parity-instances-960009', 'parity-instances-ipv6only', u.id, 960001, 960001, NULL,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'ipv6_only', 4,
       20, 5, 3, 10, false, 0, now() - interval '17 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960010: instance whose package forbids deletion.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 960010, 'parity-instances-960010', 'parity-instances-nodelete', u.id, 960001, 960003, NULL,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'nat', 5,
       20, 5, 3, 10, false, 0, now() - interval '16 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960001: PENDING start task for instance 960006 (GET /:id/task, GET /tasks/:id).
INSERT INTO instance_tasks (id, instance_id, host_id, user_id, task_type, status,
                            progress, retry_count, created_at)
SELECT 960001, 960006, 960001, u.id, 'start', 'PENDING',
       NULL, 0, now() - interval '10 minutes'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 960002: COMPLETED task (DELETE /tasks/:id must answer TASK_CANNOT_CANCEL).
INSERT INTO instance_tasks (id, instance_id, host_id, user_id, task_type, status,
                            progress, retry_count, created_at, started_at, finished_at)
SELECT 960002, 960001, 960001, u.id, 'start', 'COMPLETED',
       NULL, 0, now() - interval '3 hours', now() - interval '3 hours', now() - interval '2 hours'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- A manual mapping on 960001 (kept in sync by the detail route's quota counters).
INSERT INTO port_mappings (id, instance_id, host_id, protocol, public_port, private_port,
                           remark, system_managed, created_at)
VALUES (960001, 960001, 960001, 'tcp', 26001, 22, 'parity fixture', false, now() - interval '10 days')
ON CONFLICT (id) DO NOTHING;

-- A mapping on admin's instance (the foreign-delete case).
INSERT INTO port_mappings (id, instance_id, host_id, protocol, public_port, private_port,
                           remark, system_managed, created_at)
VALUES (960002, 960002, 960001, 'tcp', 26002, 22, NULL, false, now() - interval '10 days')
ON CONFLICT (id) DO NOTHING;

INSERT INTO snapshots (id, instance_id, incus_name, name, description, stateful, size, created_at)
VALUES (960001, 960001, 'parity-snap-960001', 'parity-snap', NULL, false, 0, '2024-02-01 00:00:00.000')
ON CONFLICT (id) DO NOTHING;

INSERT INTO backups (id, instance_id, incus_name, name, description, size, status, created_at, expires_at)
VALUES (960001, 960001, 'parity-bk-960001', 'parity-bk', NULL, 1024, 'ready', '2024-02-02 00:00:00.000', NULL)
ON CONFLICT (id) DO NOTHING;

COMMIT;
