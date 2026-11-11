-- Agent-module fixture rows for test/conformance/requests-agent.json.
--
-- Everything here is deterministic so the Node and Go serializers can be diffed
-- byte-for-byte: fixed ids (>= 990001), fixed agent ids, fixed timestamps and a
-- fixed `last_seen_at` far in the past (so deriveAgentStatus reports "offline"
-- on both backends instead of depending on wall-clock time).
--
-- Install tokens: the plaintext tokens below never leave this file — only their
-- sha256 digests are stored. One host carries an EXPIRED token, one a USED token;
-- both must answer 403 without mutating anything.
--
-- Idempotent: every INSERT uses ON CONFLICT DO NOTHING. parity.sh applies every
-- test/conformance/seed-*.sql automatically after the identities exist.

BEGIN;

-- Hosts owned by the admin account -------------------------------------------------
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
SELECT v.id, u.id, v.name, v.url, v.location, 'us', 'x86_64', 'online',
       v.nat_public_ip, 20000, 30000, 0, 0, 0,
       '[]'::jsonb, 0, 0, 0, 'container',
       'zfs', 'loop', 60, 2, NULL, NULL, NULL, true, v.tunnel_enabled,
       '127.0.0.1', 8443, true, 0, false,
       8444, true, 1, NULL,
       true, true, false, true,
       timestamptz '2026-01-01 00:00:00+00', timestamptz '2026-01-01 00:00:00+00'
  FROM users u, (VALUES
    (990001, 'parity-agent-host',            'https://127.0.0.1:1', 'Parity Agent DC',  '203.0.113.31', false),
    (990003, 'parity-agent-install-user',    'https://127.0.0.1:1', NULL,               '203.0.113.33', false),
    (990004, 'parity-agent-install-admin',   'https://127.0.0.1:1', NULL,               '203.0.113.34', false),
    (990005, 'parity-agent-rotate',          'https://127.0.0.1:1', NULL,               '203.0.113.35', false),
    (990006, 'parity-agent-token-expired',   'https://127.0.0.1:1', NULL,               '203.0.113.36', false),
    (990007, 'parity-agent-token-used',      'https://127.0.0.1:1', NULL,               '203.0.113.37', false)
  ) AS v(id, name, url, location, nat_public_ip, tunnel_enabled)
 WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- One host owned by the plain (non-admin) conformance user: proves the
-- owner-or-admin rule is not "admin only".
INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  cpu_used, memory_used, disk_used, tags, instance_type, storage_driver,
  storage_type, storage_size, ipv6_mode, enable_api, tunnel_enabled,
  target_host, target_port, is_installed, cert_download_count, caddy_enabled,
  caddy_port, transfer_enabled, traffic_reset_day, notify_purchase,
  notify_renew, notify_destroy, enable_resource_pool, created_at, updated_at
)
SELECT 990002, u.id, 'parity-agent-user-host', 'https://127.0.0.1:1', NULL, 'us', 'x86_64', 'online',
       0, 0, 0, '[]'::jsonb, 'container', 'zfs', 'loop', 60, 2,
       true, false, '127.0.0.1', 8443, true, 0, false, 8444, true, 1,
       true, true, false, true,
       timestamptz '2026-01-01 00:00:00+00', timestamptz '2026-01-01 00:00:00+00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- Deterministic agent record for GET /hosts/:hostId/status and the admin status
-- route. `last_seen_at` is old, so both backends derive "offline" from the same
-- stored "online" status (deriveAgentStatus).
INSERT INTO host_agents (
  id, host_id, agent_id, secret_hash, secret_encrypted,
  install_token_hash, install_token_expires_at, install_token_used_at,
  enabled, status, version, capabilities, last_report, last_seen_at,
  last_heartbeat_ip, created_at, updated_at
)
VALUES (
  990001, 990001, 'agt_parity_fixture_agent_0001',
  'd24f8b8f1f0e7f8f1e4a6d3f7c9b2a5e8d1f4c7a0b3e6d9f2a5c8b1e4d7f0a3c',
  'fixture-secret-not-decryptable',
  NULL, NULL, NULL,
  true, 'online', '1.4.0',
  '["incus_node_v1","network-control"]'::jsonb,
  '{"metrics":{"heartbeatIntervalSeconds":30}}'::jsonb,
  timestamptz '2020-01-01 00:00:00+00',
  '203.0.113.9',
  timestamptz '2026-01-01 00:00:00+00', timestamptz '2026-01-01 00:00:00+00'
)
ON CONFLICT (id) DO NOTHING;

-- Expired install token: sha256('ait_parity_expired_token_000000000000000000')
INSERT INTO host_agents (
  id, host_id, agent_id, secret_hash, secret_encrypted,
  install_token_hash, install_token_expires_at, install_token_used_at,
  enabled, status, version, capabilities, last_report, last_seen_at,
  created_at, updated_at
)
VALUES (
  990002, 990006, 'agt_parity_fixture_agent_0002',
  'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
  'fixture-secret-not-decryptable',
  '883b7eed907599b91e4ce7d9be36ca431f8fb838cab7716dff970fa7bc3a53ed',
  now() - interval '1 hour', NULL,
  true, 'offline', NULL, '[]'::jsonb, '{}'::jsonb, NULL,
  timestamptz '2026-01-01 00:00:00+00', timestamptz '2026-01-01 00:00:00+00'
)
ON CONFLICT (id) DO NOTHING;

-- Already-used install token: sha256('ait_parity_used_token_0000000000000000000000'),
-- expires in the future and used_at is set.
INSERT INTO host_agents (
  id, host_id, agent_id, secret_hash, secret_encrypted,
  install_token_hash, install_token_expires_at, install_token_used_at,
  enabled, status, version, capabilities, last_report, last_seen_at,
  created_at, updated_at
)
VALUES (
  990003, 990007, 'agt_parity_fixture_agent_0003',
  'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
  'fixture-secret-not-decryptable',
  '2629c1c25ced5abba05ad63d8075bec301ba6391899e453e23ab582051f525db',
  now() + interval '1 hour', now() - interval '10 minutes',
  true, 'offline', NULL, '[]'::jsonb, '{}'::jsonb, NULL,
  timestamptz '2026-01-01 00:00:00+00', timestamptz '2026-01-01 00:00:00+00'
)
ON CONFLICT (id) DO NOTHING;

COMMIT;
