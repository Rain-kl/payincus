-- Package-module fixture rows for test/conformance/requests-packages.json.
--
-- The harness replays the same request list against the Node and the Go backend
-- on the *same* database and diffs the responses, so the package cases need rows
-- that already exist before either server starts.
--
-- Apply order is handled by parity.sh (it runs `test/conformance/seed` first, then
-- every seed-*.sql). Manual equivalent:
--
--   PARITY_DATABASE_URL=postgresql://.../payincus_goparity \
--     ./test/conformance/reset-parity-db.sh
--   PARITY_DATABASE_URL=postgresql://.../payincus_goparity \
--     ./test/conformance/parity.sh requests-packages.json
--
-- Every row uses an id >= 930001 (the packages segment; hosts/packages/plans/
-- bindings/instances all stay inside it) so it cannot collide with the
-- auto-increment sequence or the 900001/920001 segments other modules use. The
-- sequences are advanced past the seeded range at the end.
--
-- Idempotency: ON CONFLICT (id) DO NOTHING everywhere, so re-running the file
-- never duplicates fixtures. Users are resolved by username (admin /
-- parityuser), never by a hard-coded id.
--
-- The rows are chosen so the read-only and negative cases are deterministic:
--   * 930001 is an admin-owned PUBLIC package (global_shared) bound to the online
--     host 930001, with three plans (two active, one disabled) -> /public,
--     /public/regions, GET /, GET /:id for a non-owner and GET /:id/plans;
--   * 930002 is an admin-owned PRIVATE package -> non-owner GET/PATCH 403;
--   * 930003 is parityuser's own package -> owner branches, share endpoints,
--     an idempotent PATCH {} and an empty plan list;
--   * one running instance on 930001 makes DELETE /930001 return
--     PACKAGE_HAS_INSTANCES instead of mutating the fixture.

BEGIN;

-- The host must look schedulable: online, not tunnel-gated, no agent row (an
-- absent agent is "no health data" on both backends) and a ready system-disk pool.
INSERT INTO hosts (id, user_id, name, url, location, country_code, architecture, status,
                   cpu_used, memory_used, disk_used, cpu_allowance_max, memory_max,
                   instance_type, tunnel_enabled, enable_api,
                   created_at, updated_at)
SELECT 930001, u.id, 'pkg-parity-host-930001', 'https://pkg-parity-host-930001.invalid:8443',
       'Parity Region One', 'us', 'x86_64', 'online'::"HostStatus",
       100, 1024, 10240, 1600, 32768,
       'container'::"InstanceType", false, true,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO storage_pools (id, host_id, name, driver, purpose, description, config,
                           status, total_bytes, used_bytes, telemetry, reported_at,
                           created_at, updated_at)
VALUES (930001, 930001, 'pkg-pool', 'dir', 'instance_data'::"StoragePurpose",
        'Parity package pool', '{}'::jsonb, 'ready', 107374182400, 0, '{}'::jsonb,
        now(), now() - interval '40 days', now() - interval '1 day')
ON CONFLICT (id) DO NOTHING;

INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      bandwidth_max, network_mode, instance_type, privileged, nested, active,
                      port_limit, snapshot_limit, backup_limit, site_limit,
                      monthly_traffic_limit, traffic_reset_price,
                      global_shared, global_max_instances, allow_instance_deletion,
                      created_at)
SELECT 930001, u.id, 'Parity Public Package', 'Fixture package for public endpoints',
       200, 2048, 20480, NULL, 'nat'::"NetworkMode", 'container'::"InstanceType",
       false, false, true, 20, 5, 3, 10, 1073741824, 5.00, true, 2, true,
       now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      bandwidth_max, network_mode, instance_type, privileged, nested, active,
                      port_limit, snapshot_limit, backup_limit, site_limit,
                      monthly_traffic_limit, traffic_reset_price,
                      global_shared, global_max_instances, allow_instance_deletion,
                      created_at)
SELECT 930002, u.id, 'Parity Private Package', 'Fixture private package for 403 branches',
       100, 1024, 10240, NULL, 'nat'::"NetworkMode", 'container'::"InstanceType",
       false, false, true, 20, 5, 3, 10, NULL, 0.00, false, NULL, true,
       now() - interval '39 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      bandwidth_max, network_mode, instance_type, privileged, nested, active,
                      port_limit, snapshot_limit, backup_limit, site_limit,
                      monthly_traffic_limit, traffic_reset_price,
                      global_shared, global_max_instances, allow_instance_deletion,
                      created_at)
SELECT 930003, u.id, 'Parity User Package', 'Fixture package owned by the non-admin user',
       150, 1536, 15360, NULL, 'nat'::"NetworkMode", 'container'::"InstanceType",
       false, false, true, 20, 5, 3, 10, 2147483648, 3.50, false, NULL, true,
       now() - interval '38 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- package_plans.updated_at is NOT NULL with no database default (Prisma fills it
-- client-side), so every row names it explicitly.
INSERT INTO package_plans (id, package_id, name, description,
                           cpu, memory, disk, port_limit, snapshot_limit, backup_limit, site_limit,
                           swap_size, traffic_limit, traffic_limit_speed,
                           price, billing_cycle, setup_fee, traffic_reset_price,
                           is_active, is_sold_out, sort_order, sla_guarantee,
                           created_at, updated_at)
VALUES
  (930001, 930001, 'Parity Basic', 'Basic parity plan',
   100, 1024, 10240, 20, 5, 3, 10, 0, 1073741824, '100Mbit',
   10.00, 1, 0, NULL, true, false, 0, 99.90, now() - interval '40 days', now() - interval '1 day'),
  (930002, 930001, 'Parity Pro', 'Pro parity plan',
   200, 2048, 20480, 20, 5, 3, 10, 512, 2147483648, '200Mbit',
   20.00, 1, 0, 2.50, true, false, 1, NULL, now() - interval '39 days', now() - interval '1 day'),
  (930003, 930002, 'Private Plan', 'Private package plan',
   100, 1024, 10240, 20, 5, 3, 10, 0, 1073741824, '100Mbit',
   15.00, 2, 0, NULL, true, false, 0, 95.00, now() - interval '38 days', now() - interval '1 day'),
  (930004, 930001, 'Parity Disabled Plan', 'Inactive plan, filtered by activeOnly',
   300, 3072, 30720, 20, 5, 3, 10, 0, 3221225472, '300Mbit',
   30.00, 1, 0, NULL, false, false, 2, NULL, now() - interval '37 days', now() - interval '1 day')
ON CONFLICT (id) DO NOTHING;

INSERT INTO package_hosts (id, package_id, host_id, storage_pool_name, traffic_multiplier, created_at)
VALUES
  (930001, 930001, 930001, 'pkg-pool', 1.000, now() - interval '40 days'),
  (930002, 930002, 930001, 'pkg-pool', 1.000, now() - interval '39 days'),
  (930003, 930003, 930001, 'pkg-pool', 1.500, now() - interval '38 days')
ON CONFLICT (id) DO NOTHING;

-- A running instance bound to 930001 keeps DELETE /930001 on the
-- PACKAGE_HAS_INSTANCES branch (no fixture mutation) and exercises the
-- instance-count field of the list/detail payloads.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id,
                       storage_pool_name, image, status, cpu, memory, disk,
                       updated_at, created_at)
SELECT 930001, 'pkg-parity-instance-930001', 'pkg-parity-instance-930001',
       u.id, 930001, 930001, 'pkg-pool', 'debian/12', 'running'::"InstanceStatus",
       100, 1024, 10240, now() - interval '1 day', now() - interval '30 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- Refresh the pool report on every apply. storage-capacity treats a report older
-- than max(120s, heartbeat*3) as unavailable, and ON CONFLICT DO NOTHING would
-- otherwise freeze reported_at at the first run, silently flipping the package
-- create/update storage checks to HOST_STORAGE_POOL_UNAVAILABLE minutes later.
UPDATE storage_pools
   SET status = 'ready', total_bytes = 107374182400, used_bytes = 0,
       reported_at = now(), updated_at = now()
 WHERE id = 930001;

SELECT setval(pg_get_serial_sequence('hosts', 'id'), 930010, true);
SELECT setval(pg_get_serial_sequence('storage_pools', 'id'), 930010, true);
SELECT setval(pg_get_serial_sequence('packages', 'id'), 930010, true);
SELECT setval(pg_get_serial_sequence('package_plans', 'id'), 930010, true);
SELECT setval(pg_get_serial_sequence('package_hosts', 'id'), 930010, true);
SELECT setval(pg_get_serial_sequence('instances', 'id'), 930010, true);

COMMIT;
