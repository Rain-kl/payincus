#!/bin/bash
# Reset the throwaway parity database to a known state.
#
#   PARITY_DATABASE_URL=postgresql://user:pass@127.0.0.1:5432/payincus_goparity \
#     ./test/conformance/reset-parity-db.sh
#
# Wave D modules write real rows (hosts, packages, instances), so parity runs are
# only comparable when they start from the same state. This drops and re-migrates
# the parity database, then re-creates the two conformance identities via
# ./test/conformance/seed.
#
# The destructive part is guard-railed twice: the URL must name a *_goparity
# database, and PARITY_DATABASE_URL must be set explicitly (there is no fallback
# to DATABASE_URL, so a stray production DSN cannot be picked up).
set -eu

: "${PARITY_DATABASE_URL:?set PARITY_DATABASE_URL to the throwaway parity database}"
case "$PARITY_DATABASE_URL" in
  *goparity*) ;;
  *) echo "refusing to reset a database whose name does not contain \"goparity\"" >&2; exit 1 ;;
esac

GODIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REPO=$(cd "$GODIR/.." && pwd)

echo "resetting parity database..."
( cd "$REPO/server" && DATABASE_URL="$PARITY_DATABASE_URL" \
    pnpm exec prisma migrate reset --force --skip-seed --skip-generate )

echo "seeding conformance identities..."
( cd "$GODIR" && go run ./test/conformance/seed -dsn "$PARITY_DATABASE_URL" )

echo "done."
