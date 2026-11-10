# Dual-run conformance harness

The Node backend's `test-*-guards.ts` suites are **static source scans** of the
TypeScript files (they read `server/src/...` and assert string patterns). They
cannot validate a Go binary, so behavioural compatibility must be proven
differently: run **both** servers against the **same PostgreSQL database** and
diff their HTTP responses.

## Quick start: `parity.sh`

`parity.sh` does the whole cycle (reference Node on 3101, Go under test on 3102,
token filling, replay, teardown) and serialises concurrent runs with a lock:

```bash
PARITY_DATABASE_URL=postgresql://user:pass@127.0.0.1:5432/payincus_goparity \
  ./test/conformance/parity.sh requests-users.json requests-auth.json
```

It needs one throwaway database that **both** backends point at, plus two login
accounts inside it: an admin (`PARITY_ADMIN_USER`, default `admin`) and a plain
user (`PARITY_USER_USER`, default `parityuser`, `role=user`) so admin-only 403
paths are exercised for real instead of collapsing into 401-vs-401. Credentials
come from the environment only — never commit them.

`parity.sh` runs `test/conformance/seed` first, so the two identities exist even
on a freshly migrated database (it only inserts missing rows; existing rows are
never rewritten). For write-heavy modules, reset the database to a known state
before a comparison run:

```bash
PARITY_DATABASE_URL=... ./test/conformance/reset-parity-db.sh
```

That drops and re-migrates the parity database, so both runs see identical
tables. It refuses to run unless the database name contains `goparity`.

The manual steps below are what the script automates; keep them for debugging a
single case.

## Token placeholders (important)

Committed case files must **never** contain a real token. They carry one of three
placeholders, which `parity.sh` replaces with freshly minted tokens before replay:

| Placeholder | Replaced with |
|---|---|
| `REPLACE_WITH_A_REAL_ACCESS_TOKEN` | admin token (historical default) |
| `REPLACE_WITH_A_REAL_ADMIN_ACCESS_TOKEN` | admin token |
| `REPLACE_WITH_A_REAL_NON_ADMIN_ACCESS_TOKEN` | non-admin (`role=user`) token |

A case whose token is still an unreplaced placeholder proves nothing: both
backends reject the literal string with 401, so the case "matches" while the
handler was never executed. Always run through `parity.sh`, and when adding a
group by hand check that no case still contains `REPLACE_WITH`.

### Cookie attributes

`skipBody` cases (login, logout, refresh) otherwise compare nothing but the
status code, leaving `httpOnly` / `secure` / `sameSite` / `maxAge` unverified.
Set `"compareCookieAttrs": true` to diff the `Set-Cookie` attributes as well. The
cookie value is ignored — it is a fresh random token on every request — and so is
`Expires`, which is absolute time.

```json
{ "name": "auth-logout-no-credentials", "method": "POST", "path": "/api/auth/logout",
  "skipBody": true, "compareCookieAttrs": true }
```

## How to run

1. Start PostgreSQL and apply migrations (Prisma owns the schema):
   ```bash
   cd server && pnpm exec prisma migrate deploy
   ```
2. Start the Node backend on its normal port:
   ```bash
   NODE_ENV=development JWT_SECRET=dev-secret-12345678 HOST=127.0.0.1 PORT=3001 \
     pnpm --filter server exec tsx src/app.ts
   ```
3. Start the Go backend on a different port, pointing at the same `DATABASE_URL`:
   ```bash
   cd server-go
   NODE_ENV=development JWT_SECRET=dev-secret-12345678 HOST=127.0.0.1 PORT=3002 \
     DATABASE_URL=postgresql://... go run ./cmd/server
   ```
4. Replay the request list and diff:
   ```bash
   go run ./test/conformance \
     -node http://127.0.0.1:3001 \
     -go   http://127.0.0.1:3002 \
     -file ./test/conformance/requests.example.json
   ```

The command exits non-zero if any case differs in status code or JSON body.
Volatile keys (default: `timestamp`) are dropped before comparison; override with
`-ignore-keys`.

## Notes

- Use **read-only** requests, or a throwaway database, so the two servers do not
  fight over writes.
- For authenticated cases, use the `REPLACE_WITH_*` placeholders described above
  and let `parity.sh` mint the tokens. Tokens are HMAC-signed with the shared
  `JWT_SECRET`, so one token is accepted by both backends.
- Add a case here **before** porting a module, so the module's contract is pinned
  from day one.
