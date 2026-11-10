-- Hosts-module fixture rows for test/conformance/requests-hosts.json.
--
-- The conformance harness replays the same request list against the Node and the
-- Go backend on the *same* database and diffs the responses, so the host cases
-- need rows that already exist before either server starts.
--
-- Apply once AFTER the parity database exists (reset-parity-db.sh wipes
-- everything, so this file has to run again after it); parity.sh applies every
-- test/conformance/seed-*.sql automatically and this file is idempotent.
--
-- Every row uses an id >= 900001 so it cannot collide with the auto-increment
-- sequence. Hosts are owned by the admin account.
--
-- The connection URL points at https://127.0.0.1:1 — TCP port 1 refuses
-- immediately, so every Incus path fails fast and deterministically on both
-- backends instead of waiting for a timeout.
--
-- ipv6_mode = 1 (routed) requires subnet + parent interface: PATCH /api/hosts/:id
-- with an empty body still enforces HOST_IPV6_ROUTE_REQUIRES_CONFIG, so the
-- fixture hosts carry both values.
--
-- Install tokens: `is_installed = true` is the "token already used" case
-- (403 Host already installed), a past expiry is the "expired" case, and one
-- fresh token yields the script (status-only comparison).

BEGIN;

INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  nat_public_ip, nat_port_start, nat_port_end, cpu_used, memory_used, disk_used,
  tags, nat_ports_used_count, cpu_allowance_max, memory_max, instance_type,
  storage_driver, storage_type, storage_size, ipv6_mode, ipv6_subnet,
  ipv6_gateway, ipv6_parent_interface, enable_api, tunnel_enabled,
  target_host, target_port, install_token, install_token_expire, is_installed,
  cert_download_count, caddy_enabled, caddy_port, transfer_enabled,
  traffic_reset_day, announcement, notify_purchase, notify_renew,
  notify_destroy, enable_resource_pool, created_at, updated_at
)
SELECT 900001, u.id, 'parity-host-online', 'https://127.0.0.1:1', 'Parity DC', 'us', 'x86_64', 'online',
        '203.0.113.10', 20000, 30000, 0, 0, 0,
        '[]'::jsonb, 0, 0, 0, 'container',
        'zfs', 'loop', 60, 1, 'fd00:9001:1::/64',
        'fd00:9001:1::1', 'eth0', true, false,
        '127.0.0.1', 8443, NULL, NULL, false,
        0, false, 8444, true,
        1, NULL, true, true,
        false, true, now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  cpu_used, memory_used, disk_used, tags, instance_type, storage_driver,
  storage_type, storage_size, ipv6_mode, ipv6_subnet, ipv6_parent_interface,
  enable_api, tunnel_enabled, target_host, target_port, is_installed,
  cert_download_count, caddy_enabled, caddy_port, transfer_enabled,
  traffic_reset_day, notify_purchase, notify_renew, notify_destroy,
  enable_resource_pool, created_at, updated_at
)
SELECT 900002, u.id, 'parity-host-offline', 'https://127.0.0.1:1', NULL, 'us', 'x86_64', 'offline',
        0, 0, 0, '[]'::jsonb, 'container', 'zfs',
        'loop', 60, 2, NULL, NULL,
        true, false, '127.0.0.1', 8443, false,
        0, false, 8444, true,
        1, true, true, false,
        true, now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  cpu_used, memory_used, disk_used, tags, instance_type, storage_driver,
  storage_type, storage_size, ipv6_mode, ipv6_subnet, ipv6_parent_interface,
  enable_api, tunnel_enabled, target_host, target_port, is_installed,
  cert_download_count, caddy_enabled, caddy_port, transfer_enabled,
  traffic_reset_day, notify_purchase, notify_renew, notify_destroy,
  enable_resource_pool, created_at, updated_at
)
SELECT 900003, u.id, 'parity-host-maintenance', 'https://127.0.0.1:1', 'Parity Maint', 'de', 'x86_64', 'maintenance',
        0, 0, 0, '["parity","maint"]'::jsonb, 'container', 'zfs',
        'loop', 60, 2, NULL, NULL,
        true, false, '127.0.0.1', 8443, false,
        0, false, 8444, true,
        1, true, true, false,
        true, now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Already-used install token: is_installed = true  -> 403 "Host already installed".
INSERT INTO hosts (
  id, user_id, name, url, country_code, architecture, status, tags,
  instance_type, storage_driver, storage_type, storage_size, ipv6_mode,
  enable_api, tunnel_enabled, target_host, target_port,
  install_token, install_token_expire, is_installed, cert_download_count,
  caddy_enabled, caddy_port, transfer_enabled, traffic_reset_day,
  notify_purchase, notify_renew, notify_destroy, enable_resource_pool,
  created_at, updated_at
)
SELECT 900004, u.id, 'parity-host-token-used', 'https://127.0.0.1:1', 'us', 'x86_64', 'offline', '[]'::jsonb,
        'container', 'zfs', 'loop', 60, 2,
        true, false, '127.0.0.1', 8443,
        'parity-install-token-used-900004', now() + interval '1 day', true, 0,
        false, 8444, true, 1,
        true, true, false, true,
        now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Expired install token: is_installed = false, expiry in the past -> 403 expired.
INSERT INTO hosts (
  id, user_id, name, url, country_code, architecture, status, tags,
  instance_type, storage_driver, storage_type, storage_size, ipv6_mode,
  enable_api, tunnel_enabled, target_host, target_port,
  install_token, install_token_expire, is_installed, cert_download_count,
  caddy_enabled, caddy_port, transfer_enabled, traffic_reset_day,
  notify_purchase, notify_renew, notify_destroy, enable_resource_pool,
  created_at, updated_at
)
SELECT 900005, u.id, 'parity-host-token-expired', 'https://127.0.0.1:1', 'us', 'x86_64', 'offline', '[]'::jsonb,
        'container', 'zfs', 'loop', 60, 2,
        true, false, '127.0.0.1', 8443,
        'parity-install-token-expired-900005', now() - interval '2 days', false, 0,
        false, 8444, true, 1,
        true, true, false, true,
        now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Fresh install token: is_installed = false, expiry in the future -> 200 script.
INSERT INTO hosts (
  id, user_id, name, url, country_code, architecture, status, tags,
  instance_type, storage_driver, storage_type, storage_size, ipv6_mode,
  enable_api, tunnel_enabled, target_host, target_port,
  install_token, install_token_expire, is_installed, cert_download_count,
  caddy_enabled, caddy_port, transfer_enabled, traffic_reset_day,
  notify_purchase, notify_renew, notify_destroy, enable_resource_pool,
  created_at, updated_at
)
SELECT 900006, u.id, 'parity-host-token-fresh', 'https://127.0.0.1:1', 'us', 'x86_64', 'offline', '[]'::jsonb,
        'container', 'zfs', 'loop', 60, 2,
        true, false, '127.0.0.1', 8443,
        'parity-install-token-fresh-900006', now() + interval '1 day', false, 0,
        false, 8444, true, 1,
        true, true, false, true,
        now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- A package bound to the online host (GET /api/hosts/:id/plans reads it).
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max, active, created_at)
SELECT 900001, u.id, 'parity-package', 'Conformance fixture package', 100, 2048, 10240, true, now() - interval '30 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO package_hosts (id, package_id, host_id, storage_pool_name, traffic_multiplier, created_at)
VALUES (900001, 900001, 900001, NULL, 1.000, now() - interval '30 days')
ON CONFLICT (id) DO NOTHING;

INSERT INTO package_plans (
  id, package_id, name, description, cpu, memory, disk, port_limit,
  snapshot_limit, backup_limit, site_limit, traffic_limit, traffic_limit_speed,
  price, billing_cycle, is_active, is_sold_out, sort_order, created_at, updated_at
)
VALUES (900001, 900001, 'parity-plan-monthly', NULL, 50, 1024, 5120, 20,
        5, 3, 10, 0, '300Mbit',
        12.34, 1, true, false, 0, now() - interval '30 days', now() - interval '1 day')
ON CONFLICT (id) DO NOTHING;

COMMIT;
