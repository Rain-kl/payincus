-- OAuth (login-provider) fixture rows for test/conformance/requests-oauth.json.
--
-- parity.sh replays one request list against the Node and the Go backend on the
-- SAME database and diffs status + JSON body, so the read cases need rows that
-- already exist before either server starts. This file is idempotent and only
-- touches its own id range (992001+).
--
-- Id ranges already in use: hosts 900001, instance-billing 920001, packages
-- 930001, backups 940001, instance-destroy 950001, users/oauth-apps 960001,
-- public-api 980001, agent 991001. This module: 992001+.
--
-- Credentials: the client ids/secrets below are obviously fake values that exist
-- only in this throwaway parity database. They are never valid at GitHub/Google,
-- so no real OAuth flow can ever complete with them, and the request list never
-- calls /callback with a valid state (that would leave the process).
--
-- The PUT cases in the request list rewrite client_id/enabled/updated_at and
-- re-encrypt client_secret (the backends share ENCRYPTION_KEY, so each can read
-- what the other wrote). This seed restores the exact starting values on every
-- run so repeated comparisons stay deterministic; the secret is deliberately
-- stored as *plaintext* here so `clientSecretMasked` is stable regardless of
-- which backend wrote the row last (decryptSensitiveData passes plaintext
-- through unchanged).
--
-- The single user_oauth_bindings row (google, disabled provider) makes
-- GET /bindings non-empty and deterministic. DELETE /bindings/google is NOT in
-- the request list: the Node run would delete the row and the Go run would then
-- see "not bound", which the harness cannot reconcile. Success-path deletes are
-- covered by the Go DB-gated tests instead.

BEGIN;

-- ---------------------------------------------------------------------------
-- Provider configs: github enabled, google disabled.
-- ---------------------------------------------------------------------------
INSERT INTO oauth_configs (id, provider, client_id, client_secret, enabled, created_at, updated_at)
VALUES (
  992001, 'github', 'parity-fake-github-client-id', 'parity-fake-github-secret-0001', true,
  timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
)
ON CONFLICT (provider) DO UPDATE SET
  client_id = EXCLUDED.client_id,
  client_secret = EXCLUDED.client_secret,
  enabled = EXCLUDED.enabled,
  updated_at = EXCLUDED.updated_at;

INSERT INTO oauth_configs (id, provider, client_id, client_secret, enabled, created_at, updated_at)
VALUES (
  992002, 'google', 'parity-fake-google-client-id', 'parity-fake-google-secret-0002', false,
  timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
)
ON CONFLICT (provider) DO UPDATE SET
  client_id = EXCLUDED.client_id,
  client_secret = EXCLUDED.client_secret,
  enabled = EXCLUDED.enabled,
  updated_at = EXCLUDED.updated_at;

-- ---------------------------------------------------------------------------
-- One binding for the non-admin parity account (google, so the enabled-provider
-- list is unaffected). Timestamps are fixed so `boundAt` is stable.
-- ---------------------------------------------------------------------------
INSERT INTO user_oauth_bindings (
  id, user_id, provider, provider_user_id, provider_username, provider_email,
  provider_avatar, access_token, refresh_token, created_at, updated_at
)
SELECT 992101, u.id, 'google', 'parity-google-sub-992101', 'parity-google-user',
       'parity-google@example.com', 'https://avatars.example.com/parity-google',
       NULL, NULL,
       timestamp '2026-01-02 00:00:00.000', timestamp '2026-01-02 00:00:00.000'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (user_id, provider) DO UPDATE SET
  provider_user_id = EXCLUDED.provider_user_id,
  provider_username = EXCLUDED.provider_username,
  provider_email = EXCLUDED.provider_email,
  provider_avatar = EXCLUDED.provider_avatar,
  access_token = EXCLUDED.access_token,
  refresh_token = EXCLUDED.refresh_token,
  created_at = EXCLUDED.created_at,
  updated_at = EXCLUDED.updated_at;

-- Keep future auto-increment ids above the fixture range.
SELECT setval(pg_get_serial_sequence('oauth_configs', 'id'), 992010, true);
SELECT setval(pg_get_serial_sequence('user_oauth_bindings', 'id'), 992110, true);

COMMIT;
