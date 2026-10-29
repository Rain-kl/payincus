# Dual-run conformance harness

The Node backend's `test-*-guards.ts` suites are **static source scans** of the
TypeScript files (they read `server/src/...` and assert string patterns). They
cannot validate a Go binary, so behavioural compatibility must be proven
differently: run **both** servers against the **same PostgreSQL database** and
diff their HTTP responses.

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
- For authenticated cases, generate a real access token once (log in against the
  Node server) and paste it into the `token` field. Tokens are HMAC-signed with the
  shared `JWT_SECRET`, so the same token is accepted by both backends.
- Add a case here **before** porting a module, so the module's contract is pinned
  from day one.
