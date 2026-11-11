-- Terminal / SPICE fixture rows for test/conformance/requests-terminal-spice.json.
--
-- The conformance harness replays the same request list against the Node and the
-- Go backend on the *same* database and diffs status + JSON body, so the cases need
-- rows that already exist before either server starts.
--
-- Every id is >= 994001 so it cannot collide with the auto-increment sequence or
-- with the other fixtures (900001 .. 960001 ranges). The file is idempotent
-- (ON CONFLICT DO NOTHING), so re-running never duplicates rows.
--
-- The host URL points at https://127.0.0.1:1, so any console attempt fails fast and
-- deterministically on both backends; that is what makes the
-- "running VM on an unreachable node" case comparable (both answer 502 with the
-- same generic body).
--
-- Nothing in the request list mutates a row: the ticket POST only mints a
-- process-local capability (body skipped), and every other case is a rejection.

BEGIN;

-- 994001: online container host owned by admin. parityuser is neither the owner nor
-- the instance owner, which is what the 403 cases assert.
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
SELECT 994001, u.id, 'parity-terminal-spice-host', 'https://127.0.0.1:1', 'Parity DC 994001',
       'us', 'x86_64', 'online',
       '203.0.113.94', 29400, 29499, 0, 0, 0,
       '[]'::jsonb, 0, 10000, 1048576, 'container',
       'zfs', 'loop', 1000, 1, 'fd00:9941:1::/64',
       'fd00:9941:1::1', 'eth0', true, false,
       '127.0.0.1', 8443, true, 0, false,
       8444, true, 1, 'parity terminal/spice fixture',
       true, true, false, true,
       now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 994001: container package. 994002: VM package (SPICE only accepts VMs).
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, created_at)
SELECT 994001, u.id, 'Parity Terminal Container', 'terminal/spice fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       true, now() - interval '20 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, created_at)
SELECT 994002, u.id, 'Parity Spice VM', 'terminal/spice fixture',
       800, 4096, 60000, 'nat', 'vm', 20, 5, 3, 10, 1099511627776,
       true, now() - interval '20 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO package_hosts (id, package_id, host_id, storage_pool_name, traffic_multiplier, created_at)
VALUES (994001, 994001, 994001, 'parity-pool', 1.000, now() - interval '20 days')
ON CONFLICT (id) DO NOTHING;

-- 994001: parityuser's RUNNING container (terminal ticket success; SPICE forbidden
-- because the package is not a VM).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 994001, 'parity-terminal-994001', 'parity-terminal-container', u.id, 994001, 994001, NULL,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 994002: admin's RUNNING container — parityuser gets 403 from the ticket endpoint.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 994002, 'parity-terminal-994002', 'parity-terminal-admin-container', u.id, 994001, 994001, NULL,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 994003: parityuser's RUNNING VM on an unreachable node — the SPICE session
-- creation passes every check and then fails to reach Incus (502 on both sides).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 994003, 'parity-terminal-994003', 'parity-terminal-vm', u.id, 994001, 994002, NULL,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 994004: parityuser's STOPPED container — the terminal ticket must answer
-- INSTANCE_NOT_RUNNING.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 994004, 'parity-terminal-994004', 'parity-terminal-stopped', u.id, 994001, 994001, NULL,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 994005: admin's RUNNING VM — parityuser must get CONSOLE_FORBIDDEN before any
-- Incus call.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode, display_order,
                       port_limit, snapshot_limit, backup_limit, site_limit,
                       auto_renew, version, created_at, updated_at)
SELECT 994005, 'parity-terminal-994005', 'parity-terminal-admin-vm', u.id, 994001, 994002, NULL,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 0,
       20, 5, 3, 10, false, 0, now() - interval '20 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

SELECT setval(pg_get_serial_sequence('hosts', 'id'), 994010, true);
SELECT setval(pg_get_serial_sequence('packages', 'id'), 994010, true);
SELECT setval(pg_get_serial_sequence('package_hosts', 'id'), 994010, true);
SELECT setval(pg_get_serial_sequence('instances', 'id'), 994010, true);

COMMIT;
