#!/bin/bash
# Dual-run conformance runner for the Go migration.
#
#   PARITY_DATABASE_URL=postgresql://user:pass@127.0.0.1:5432/payincus_goparity \
#     ./test/conformance/parity.sh requests-users.json [more-case-files...]
#
# What it does
#   1. starts a THROWAWAY Node backend on 3101 (the reference implementation)
#      against the parity database — never the production 3001 instance;
#   2. logs in for real access tokens and fills the `REPLACE_WITH_*` placeholders
#      that the committed case files carry (see README.md);
#   3. builds and starts the current Go tree on 3102 against the same database;
#   4. replays every case file and diffs status + JSON body, one line per case.
#
# The two backends MUST share one database, otherwise the responses cannot be
# compared. Runs are serialised with a lock so concurrent users queue instead of
# fighting over ports or the database.
#
# Required environment
#   PARITY_DATABASE_URL   throwaway database both backends talk to
# Optional environment
#   PARITY_ADMIN_USER / PARITY_ADMIN_PASSWORD   default admin / admin123
#   PARITY_USER_USER   / PARITY_USER_PASSWORD   default parityuser / admin123
#                                               (role=user; empty disables)
set -u

GODIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REPO=$(cd "$GODIR/.." && pwd)

: "${PARITY_DATABASE_URL:?set PARITY_DATABASE_URL to the throwaway parity database}"
ADMIN_USER=${PARITY_ADMIN_USER:-admin}
ADMIN_PASS=${PARITY_ADMIN_PASSWORD:-admin123}
USER_USER=${PARITY_USER_USER-parityuser}
USER_PASS=${PARITY_USER_PASSWORD:-admin123}

NODE_PORT=${PARITY_NODE_PORT:-3101}
GO_PORT=${PARITY_GO_PORT:-3102}
LOCK=${TMPDIR:-/tmp}/payincus-parity.lock
FILLED=$(mktemp -d "${TMPDIR:-/tmp}/payincus-parity-filled.XXXXXX")

if [ $# -eq 0 ]; then
  echo "usage: PARITY_DATABASE_URL=... $0 requests-<group>.json [...]" >&2
  exit 2
fi

while ! mkdir "$LOCK" 2>/dev/null; do
  echo "waiting for another parity run to finish..."
  sleep 3
done
cleanup() {
  lsof -ti:$NODE_PORT | xargs -r kill -9 2>/dev/null
  lsof -ti:$GO_PORT   | xargs -r kill -9 2>/dev/null
  rm -rf "$FILLED"
  rmdir "$LOCK" 2>/dev/null
}
trap cleanup EXIT

common_env=(
  NODE_ENV=development
  JWT_SECRET=dev-secret-12345678
  COOKIE_SECRET=dev-secret-12345678
  ENCRYPTION_KEY=dev-secret-12345678
  DATABASE_URL="$PARITY_DATABASE_URL"
  HOST=127.0.0.1
)

wait_health() { # $1 port, $2 logfile, $3 label
  for _ in $(seq 1 180); do
    curl -sf -m 1 "http://127.0.0.1:$1/api/health" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "FATAL: $3 did not become healthy; see $2" >&2
  return 1
}

login() { # $1 username, $2 password -> token on stdout (never logged)
  curl -s -m 10 -X POST "http://127.0.0.1:$NODE_PORT/api/auth/login" \
    -H 'content-type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}" |
    python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("token") or "")
except Exception:
    print("")'
}

# --- Make sure both identities exist (a reset parity DB has none) ---
( cd "$GODIR" && go run ./test/conformance/seed -dsn "$PARITY_DATABASE_URL" \
    -admin-user "$ADMIN_USER" -admin-password "$ADMIN_PASS" \
    -user-user "$USER_USER" -user-password "$USER_PASS" ) || {
  echo "FATAL: could not seed the parity database; see the error above" >&2
  exit 7
}

# --- Apply the SQL fixtures some groups depend on (idempotent by design) ---
if command -v psql >/dev/null 2>&1; then
  for fixture in "$GODIR"/test/conformance/seed-*.sql; do
    [ -e "$fixture" ] || continue
    echo "fixture: $(basename "$fixture")"
    psql "$PARITY_DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$fixture" >/dev/null || {
      echo "FATAL: fixture $(basename "$fixture") failed" >&2
      exit 8
    }
  done
else
  echo "WARNING: psql not found; seed-*.sql fixtures were not applied" >&2
fi

# --- Node reference server ---
lsof -ti:$NODE_PORT | xargs -r kill -9 2>/dev/null
( cd "$REPO" && env "${common_env[@]}" PORT=$NODE_PORT \
    pnpm --filter server exec tsx src/app.ts > /tmp/parity-node.log 2>&1 & )
wait_health $NODE_PORT /tmp/parity-node.log "Node reference server" || exit 3

ADMIN_TOKEN=$(login "$ADMIN_USER" "$ADMIN_PASS")
USER_TOKEN=""
[ -n "$USER_USER" ] && USER_TOKEN=$(login "$USER_USER" "$USER_PASS")
if [ -z "$ADMIN_TOKEN" ]; then
  echo "FATAL: could not log in as $ADMIN_USER against the parity database; see /tmp/parity-node.log" >&2
  exit 6
fi
[ -z "$USER_TOKEN" ] && echo "WARNING: no non-admin token; NON_ADMIN cases will compare 401 vs 401" >&2

# --- Mint a public API token (pat_) and store only its digest ------------------
# Web tokens can be signed on the fly because both backends share JWT_SECRET; a
# pat_ token has to exist in the database. The plaintext is generated here, used
# to fill REPLACE_WITH_A_REAL_PUBLIC_API_TOKEN, and never written to the repo.
PUBLIC_TOKEN=""
if command -v psql >/dev/null 2>&1; then
  read -r PUBLIC_TOKEN PUBLIC_TOKEN_HASH PUBLIC_TOKEN_PREFIX <<<"$(python3 - <<'PY'
import base64, hashlib, secrets
token = "pat_" + base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode()
print(token, hashlib.sha256(token.encode()).hexdigest(), token[:16])
PY
)"
  psql "$PARITY_DATABASE_URL" -v ON_ERROR_STOP=1 -q \
    -v tok_prefix="$PUBLIC_TOKEN_PREFIX" -v tok_hash="$PUBLIC_TOKEN_HASH" >/dev/null <<'SQL'
DELETE FROM public_api_tokens WHERE name = 'conformance-harness';
INSERT INTO public_api_tokens (user_id, name, token_prefix, token_hash, scopes, created_at, updated_at)
SELECT u.id, 'conformance-harness', :'tok_prefix', :'tok_hash',
       '["profile:read","profile:write","balance:read","balance:write","billing:read",
         "products:read","services:read","services:operate","services:billing","orders:read",
         "tickets:read","tickets:write","notifications:read","notifications:send"]'::jsonb,
       now() at time zone 'utc', now() at time zone 'utc'
  FROM users u WHERE u.username = 'parityuser';
SQL
else
  echo "WARNING: psql not found; public API cases will not be filled" >&2
fi

# --- Fill token placeholders per group (real tokens never touch the repo) ---
filled=()
for f in "$@"; do
  src="$GODIR/test/conformance/$f"
  dst="$FILLED/$f"
  python3 - "$src" "$dst" "$ADMIN_TOKEN" "$USER_TOKEN" "$PUBLIC_TOKEN" <<'PY'
import sys
src, dst, admin, user, public = sys.argv[1:6]
s = open(src, encoding='utf-8').read()
s = s.replace('REPLACE_WITH_A_REAL_ADMIN_ACCESS_TOKEN', admin)
s = s.replace('REPLACE_WITH_A_REAL_NON_ADMIN_ACCESS_TOKEN', user or admin)
s = s.replace('REPLACE_WITH_A_REAL_PUBLIC_API_TOKEN', public)
s = s.replace('REPLACE_WITH_A_REAL_ACCESS_TOKEN', admin)
open(dst, 'w', encoding='utf-8').write(s)
PY
  filled+=("$dst")
done

# --- Go server under test ---
lsof -ti:$GO_PORT | xargs -r kill -9 2>/dev/null
( cd "$GODIR" && go build -o "$FILLED/server" ./cmd/server ) || { echo "FATAL: go build failed" >&2; exit 4; }
# Run the Go server with the repository as cwd, like the Node reference does
# (getProjectRoot()). Some endpoints resolve files relative to the working
# directory — /version reads version.json, install.sh serves templates — so the
# comparison must not depend on where the caller happened to stand.
( cd "$REPO" && env "${common_env[@]}" PORT=$GO_PORT "$FILLED/server" > /tmp/parity-go.log 2>&1 & )
wait_health $GO_PORT /tmp/parity-go.log "Go server" || exit 5

rc=0
for dst in "${filled[@]}"; do
  echo "### $(basename "$dst")"
  log="$FILLED/$(basename "$dst").log"
  ( cd "$GODIR" && go run ./test/conformance \
      -node "http://127.0.0.1:$NODE_PORT" \
      -go   "http://127.0.0.1:$GO_PORT" \
      -file "$dst" >"$log" 2>&1 )
  status=$?
  # Show the tail either way, but never let the pipe hide the real exit status.
  tail -60 "$log"
  if [ $status -ne 0 ]; then
    rc=1
    # The summary sits at the end, so a FAIL line can fall outside the tail.
    grep -E '^(FAIL|skip)' "$log" >&2 || true
    echo "FAIL: $(basename "$dst") exited $status; full output kept at $log" >&2
  fi
done
exit $rc
