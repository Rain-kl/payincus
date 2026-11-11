-- Admin Incus UI fixture rows for test/conformance/requests-admin-incus-ui.json.
--
-- Only the pre-upgrade branches can be compared against Node without a node agent
-- connected: the ticket endpoint and the proxy routes need a host row that EXISTS so
-- the "host found, tunnel offline -> 503 TUNNEL_OFFLINE" branch is reachable. Without
-- this row every host id would stop at 404 HOST_NOT_FOUND and the tunnel check would
-- never run on either backend.
--
-- id >= 995001 keeps clear of the other fixtures. parity.sh re-applies every
-- seed-*.sql on each run, so the statement is idempotent (ON CONFLICT DO UPDATE).
--
-- The host deliberately has no cert_path/key_path: with the tunnel offline the
-- certificate branch (502 INCUS_CERT_UNAVAILABLE) is never reached, and a missing
-- file must not turn into an accidental failure in another module's cases.
BEGIN;

INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  cpu_used, memory_used, disk_used, tags, instance_type, storage_driver,
  storage_type, storage_size, ipv6_mode, enable_api, tunnel_enabled,
  target_host, target_port, is_installed, cert_download_count, caddy_enabled,
  caddy_port, transfer_enabled, traffic_reset_day, notify_purchase,
  notify_renew, notify_destroy, enable_resource_pool, created_at, updated_at
)
SELECT 995001, u.id, 'parity-incus-ui-host', 'https://127.0.0.1:1', 'Parity Incus UI', 'us', 'x86_64', 'offline',
       0, 0, 0, '["parity","incus-ui"]'::jsonb, 'container', 'zfs',
       'loop', 60, 1, true, false,
       '127.0.0.1', 8443, false, 0, false,
       8444, true, 1, true,
       true, false, false, now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
-- ON CONFLICT keeps the file idempotent. The DO UPDATE pins the two columns this
-- fixture owns (tunnel_enabled / enable_resource_pool) so a re-run converges instead
-- of leaving an older row behind; every other column is left untouched.
ON CONFLICT (id) DO UPDATE
  SET tunnel_enabled = EXCLUDED.tunnel_enabled,
      enable_resource_pool = EXCLUDED.enable_resource_pool;

COMMIT;
