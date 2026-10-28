# Admin Built-in Incus UI & Reverse Tunnel Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Integrate the full Incus Web UI source code (`incus-ui/`) into the PayIncus monorepo, build and expose it exclusively on the Admin panel (`/incus-ui/:hostId/ui`), add an admin-only `IncusUI` launch button on the host detail page, and proxy all Incus UI HTTP/WebSocket traffic through the reverse tunnel (`HostTunnelManager`) using the panel's mTLS certificate with single-session admin JWT authentication.

**Architecture:**
1. **Backend Tunnel-Only mTLS Proxy (`server/src/routes/admin-incus-ui.ts`)**: Exposes `POST /api/admin/hosts/:hostId/incus-ui-ticket`, `ALL /api/admin/hosts/:hostId/incus-proxy/*`, and WebSocket `GET /api/ws/hosts/:hostId/incus-proxy/*`. Enforces `role === 'admin'`, active session checks, demo read-only guards, path allowlisting (`/1.0` only, blocking `/1.0/payincus` and `..`), and strictly routes via `hostTunnelManager.createDuplexStream(hostId, targetHost, targetPort)` wrapped in mTLS.
2. **Local Source Integration (`incus-ui/`)**: Integrates `incus-ui-canonical` source code as `@payincus/incus-ui` in the pnpm workspace with a lightweight runtime bridge (`incus-ui/src/lib/payincus-bridge.ts`) that reads the shared same-origin admin `localStorage.getItem('token')`, rewrites `/1.0/*` `fetch`/`XHR` calls to `/api/admin/hosts/:hostId/incus-proxy/1.0/*`, and rewrites `WebSocket` connections to `/api/ws/hosts/:hostId/incus-proxy/1.0/*` with `Sec-WebSocket-Protocol: incudal-incus-ui.<ticket>`.
3. **Admin Host Detail Entry & Build Pipeline**: Adds the `IncusUI` button to `client/src/views/resources/MyHostDetailView.vue` (visible only when `isAdmin`, enabled when `host.tunnelOnline && !needsVerification`, opens `/incus-ui/${host.id}/ui` in a new tab) and wires `scripts/build-incus-ui.mjs` into `build:admin` (never `build:user`).

**Tech Stack:** Fastify 5, Node.js `tls` / `undici` / `ws`, Vue 3 + TypeScript (`client/`), React 18 + TypeScript + Vite (`incus-ui/`), OrbStack `debian-test` VM for E2E verification.

## Global Constraints

- **Tunnel-Only Transport**: Do NOT implement direct connection fallback for Incus UI proxy; strictly require `hostTunnelManager.isTunnelOnline(hostId)` and connect via `hostTunnelManager.createDuplexStream(hostId, targetHost, targetPort)`.
- **Single Session (No Dual Session)**: Reuse the admin panel's existing `localStorage.getItem('token')` JWT and `action-ticket.ts` short-lived WebSocket/download capability tickets.
- **Admin-Only Boundary**: Customer build (`client/dist/user`) must NEVER contain `/api/admin` or `incus-ui` assets (`test:frontend-dist-boundary-guards`).
- **Color & Route Guards**: Do NOT introduce hardcoded `#hex` or `rgba()`/`opacity` in `client/src/**` (`test:frontend-color-tokens`, `test:frontend-route-guards`, `test:frontend-i18n-keys`).
- **Git Discipline**: Commit after each completed logical unit using Conventional Commits (`feat(incus-ui): ...`); never push without owner authorization.

---

### Task 1: Action Ticket Support for Incus UI WebSocket & Native Downloads

**Files:**
- Modify: `server/src/lib/action-ticket.ts`
- Test: `server/scripts/test-incus-ui-proxy-guards.ts`

**Interfaces:**
- Produces:
  - `generateIncusUiAccessTicket(userId: number, hostId: number, issuedAt: number, sessionId?: string): string`
  - `verifyIncusUiAccessTicket(token: string, expectedHostId: number): IncusUiTicketVerifyResult`

- [ ] **Step 1: Write the failing unit test for Incus UI ticket lifecycle**

Create `server/scripts/test-incus-ui-proxy-guards.ts` with initial ticket verification tests:

```typescript
import assert from 'node:assert/strict'
import {
  generateIncusUiAccessTicket,
  verifyIncusUiAccessTicket,
  revokeActionTicketsForSession
} from '../src/lib/action-ticket.js'

const token = generateIncusUiAccessTicket(1, 42, 1700000000, 'sid-admin-1')
assert.equal(typeof token, 'string')
assert.ok(token.length >= 24)

// Multi-use within TTL for the same hostId (events WS + exec FDs + downloads)
const firstCheck = verifyIncusUiAccessTicket(token, 42)
assert.equal(firstCheck.valid, true)
assert.equal(firstCheck.userId, 1)
assert.equal(firstCheck.hostId, 42)
assert.equal(firstCheck.sessionId, 'sid-admin-1')

const secondCheck = verifyIncusUiAccessTicket(token, 42)
assert.equal(secondCheck.valid, true)

// Mismatched hostId must be rejected without crashing
const wrongHost = verifyIncusUiAccessTicket(token, 99)
assert.equal(wrongHost.valid, false)

// Session revocation must immediately invalidate the ticket
revokeActionTicketsForSession('sid-admin-1')
const afterRevoke = verifyIncusUiAccessTicket(token, 42)
assert.equal(afterRevoke.valid, false)

console.log('incus ui ticket unit tests passed')
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pnpm --filter server exec tsx scripts/test-incus-ui-proxy-guards.ts`
Expected: FAIL with `SyntaxError: The requested module '../src/lib/action-ticket.js' does not provide an export named 'generateIncusUiAccessTicket'`

- [ ] **Step 3: Implement `incus-ui` ticket support in `server/src/lib/action-ticket.ts`**

Update `server/src/lib/action-ticket.ts`:
- Add `'incus-ui'` to `ActionTicketType`
- Add `IncusUiTicket` interface with `type: 'incus-ui'`, `hostId: number`, TTL `5 * 60 * 1000` (5 minutes), and `maxUsage: 256` (so a single page session can open `/1.0/events` + multiple exec/console WebSocket channels and file downloads without exhausting the ticket)
- Export `generateIncusUiAccessTicket` and `verifyIncusUiAccessTicket(token: string, expectedHostId: number)`

```typescript
export interface IncusUiTicketVerifyResult {
  valid: boolean
  userId?: number
  hostId?: number
  issuedAt?: number
  sessionId?: string
  error?: string
}

export function generateIncusUiAccessTicket(
  userId: number,
  hostId: number,
  issuedAt: number,
  sessionId?: string
): string {
  const now = Date.now()
  return createTicket({
    type: 'incus-ui',
    userId,
    hostId,
    issuedAt,
    sessionId,
    expiresAt: now + ACTION_TICKET_TTL_MS['incus-ui'],
    createdAt: now,
    usageCount: 0,
    maxUsage: 256
  })
}

export function verifyIncusUiAccessTicket(
  token: string,
  expectedHostId?: number
): IncusUiTicketVerifyResult {
  const ticket = actionTickets.get(token)
  if (!ticket || ticket.type !== 'incus-ui') {
    return { valid: false, error: 'Ticket not found or expired' }
  }

  if (Date.now() > ticket.expiresAt) {
    actionTickets.delete(token)
    return { valid: false, error: 'Ticket expired' }
  }

  if (expectedHostId !== undefined && ticket.hostId !== expectedHostId) {
    return { valid: false, error: 'Host mismatch' }
  }

  ticket.usageCount += 1
  if (ticket.usageCount >= ticket.maxUsage) {
    actionTickets.delete(token)
  } else {
    actionTickets.set(token, ticket)
  }

  return {
    valid: true,
    userId: ticket.userId,
    hostId: ticket.hostId,
    issuedAt: ticket.issuedAt,
    sessionId: ticket.sessionId
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `pnpm --filter server exec tsx scripts/test-incus-ui-proxy-guards.ts`
Expected: PASS (`incus ui ticket unit tests passed`)

---

### Task 2: Backend Reverse Tunnel HTTP & WebSocket Proxy Route (`admin-incus-ui.ts`)

**Files:**
- Create: `server/src/routes/admin-incus-ui.ts`
- Modify: `server/src/app.ts`
- Modify: `server/src/config/rate-limit.ts`
- Modify: `server/package.json`
- Test: `server/scripts/test-incus-ui-proxy-guards.ts`

**Interfaces:**
- Consumes:
  - `hostTunnelManager.isTunnelOnline(hostId)` and `hostTunnelManager.createDuplexStream(hostId, targetHost, targetPort)` from `server/src/lib/incus/tunnel-manager.ts`
  - `resolveCertificatePair(certPath, keyPath)` from `server/src/lib/incus/certificate-paths.ts`
  - `generateIncusUiAccessTicket`, `verifyIncusUiAccessTicket` from `server/src/lib/action-ticket.ts`
- Produces:
  - `POST /api/admin/hosts/:hostId/incus-ui-ticket` -> `{ ticket: string, expiresIn: 300 }`
  - `ALL /api/admin/hosts/:hostId/incus-proxy/*` -> Streaming HTTP proxy to host's `incusd` over reverse tunnel
  - `GET /api/ws/hosts/:hostId/incus-proxy/*` -> Bidirectional WebSocket bridge to host's `incusd` over reverse tunnel

- [ ] **Step 1: Expand `server/scripts/test-incus-ui-proxy-guards.ts` with route & security assertions**

Add tests covering:
1. Path validation helper `normalizeIncusProxyTargetPath(wildcardPath, rawSearch)`:
   - Allows `/1.0`, `/1.0/instances`, `/1.0/events?type=operation`, `/1.0/operations/abc/websocket?secret=xyz`
   - Rejects `/1.0/payincus`, `/1.0/payincus/network`, `/1.0/../etc/passwd`, `/ui/`, empty paths
2. End-to-end mock tunnel HTTP + WebSocket proxy verification using a local self-signed TLS server connected through a mock `hostTunnelManager` duplex stream.

- [ ] **Step 2: Run test to verify it fails**

Run: `pnpm --filter server exec tsx scripts/test-incus-ui-proxy-guards.ts`
Expected: FAIL because `server/src/routes/admin-incus-ui.ts` does not exist yet.

- [ ] **Step 3: Implement `server/src/routes/admin-incus-ui.ts` and register in `server/src/app.ts` & `server/src/config/rate-limit.ts`**

Key implementation requirements in `server/src/routes/admin-incus-ui.ts`:
- **Path Normalization & Security Guard**:
  ```typescript
  export function normalizeIncusProxyTargetPath(wildcard: string, search = ''): string | null {
    const clean = ('/' + String(wildcard || '').replace(/^\/+/, '')).replace(/\/+/g, '/')
    if (clean !== '/1.0' && !clean.startsWith('/1.0/')) return null
    if (clean.includes('..') || clean.includes('\\')) return null
    if (clean === '/1.0/payincus' || clean.startsWith('/1.0/payincus/')) return null
    return `${clean}${search}`
  }
  ```
- **Tunnel-Only Requirement**:
  - Check `hostTunnelManager.isTunnelOnline(host.id)`. If offline, return `503 { error: '宿主机反向隧道未连接，无法打开 Incus UI', code: 'TUNNEL_OFFLINE' }`.
- **Streaming HTTP Proxy (`ALL /api/admin/hosts/:hostId/incus-proxy/*`)**:
  - Support both `Authorization: Bearer <jwt>` (via `fastify.authenticateAdmin`) AND `?ticket=<incusUiTicket>` for native browser `GET` file downloads (e.g. backup/image export).
  - Register custom content-type parser `fastify.addContentTypeParser('*', (req, payload, done) => done(null, payload))` inside the encapsulated route plugin so raw upload streams (ISO/backup uploads) are piped directly without buffering into memory or hitting the 10MB JSON body limit.
  - Use `undici.Agent` configured with `connect` creating `hostTunnelManager.createDuplexStream(host.id, targetHost, targetPort)` + `tls.connect({ socket: duplex, cert, key, rejectUnauthorized: false })`.
  - Forward status code, safe response headers (filtering out hop-by-hop headers and conflicting `content-security-policy`/`x-frame-options`), and stream `response.body` to `reply.raw`.
  - Audit mutations (`POST`/`PUT`/`PATCH`/`DELETE`) via `createLog(userId, LogModule.HOST, 'host.incus_ui_mutation', ...)`.
- **WebSocket Bridge (`GET /api/ws/hosts/:hostId/incus-proxy/*`)**:
  - Extract ticket from `Sec-WebSocket-Protocol` (`incudal-incus-ui.<ticket>`) or query `?ticket=<ticket>`.
  - Verify `validateWebSocketOrigin(request)`, `verifyIncusUiAccessTicket(ticket, hostId)`, `isAccessTokenInvalidated(...)`, and active `admin` role in DB.
  - Open upstream `ws.WebSocket` over `hostTunnelManager.createDuplexStream(host.id, targetHost, targetPort)` + `tls.connect({ socket: duplex, cert, key, rejectUnauthorized: false })` to `wss://127.0.0.1:8443${targetPath}`.
  - Buffer any initial client frames received before upstream `open`, then bridge binary/text messages and close events bidirectionally.
- **Whitelist in `server/src/config/rate-limit.ts`**:
  - Add `'/api/admin/hosts/*/incus-proxy/*'`, `'/api/ws/hosts/*/incus-proxy/*'`, and `'/api/incus-ui/*'` to `rateLimitWhitelist.pathPrefixes`.

- [ ] **Step 4: Run guard test and server type-check**

Run: `pnpm --filter server type-check && pnpm --filter server test:incus-ui-proxy-guards`
Expected: PASS

---

### Task 3: Integrate `incus-ui` Source Code into Monorepo & Implement `payincus-bridge.ts`

**Files:**
- Modify: `pnpm-workspace.yaml`
- Create: `incus-ui/` (local source integration of `incus-ui-canonical` tailored for PayIncus)
- Create: `incus-ui/src/lib/payincus-bridge.ts`
- Create: `scripts/build-incus-ui.mjs`
- Modify: `client/package.json`
- Modify: `client/vite.config.ts`
- Modify: `deploy/nginx-split-intranet.conf.example`

**Interfaces:**
- Consumes:
  - `POST /api/admin/hosts/:hostId/incus-ui-ticket`
  - `ALL /api/admin/hosts/:hostId/incus-proxy/1.0/*`
  - `GET /api/ws/hosts/:hostId/incus-proxy/1.0/*`
  - Admin JWT from `localStorage.getItem('token')`
- Produces:
  - Built static SPA in `client/dist/admin/incus-ui/` served at `/incus-ui/:hostId/ui/...`

- [ ] **Step 1: Fetch `canonical/incus-ui-canonical` source tree into `incus-ui/` and clean up upstream CI/packaging bloat**

Clone the upstream source tree into a temporary directory, strip `.git`, `.github`, snap/debian packaging, and non-essential test fixtures, and place the clean React + TypeScript + Vite source tree into `incus-ui/`. Add `'incus-ui'` to `pnpm-workspace.yaml`.

- [ ] **Step 2: Create `incus-ui/src/lib/payincus-bridge.ts` and wire into `incus-ui` entry point**

Implement `incus-ui/src/lib/payincus-bridge.ts`:
- Parse `hostId` from `/incus-ui/:hostId/ui` (or query `?hostId=`), expose `getPayIncusHostId()` and `getPayIncusRouterBase()` (`/incus-ui/${hostId}`).
- Read `localStorage.getItem('token')` and listen to `window.addEventListener('storage')`. If missing, redirect to `/admin/login`.
- Maintain a cached WebSocket/download ticket via `POST /api/admin/hosts/${hostId}/incus-ui-ticket` (refreshed every 3 minutes and on demand).
- Patch `window.fetch` and `XMLHttpRequest.prototype.open`/`send`:
  - Any request to `/1.0` or `/1.0/...` is rewritten to `/api/admin/hosts/${hostId}/incus-proxy/1.0...` with `Authorization: Bearer ${token}`.
- Patch `window.WebSocket`:
  - Any WebSocket connection to `/1.0/...` (such as `/1.0/events` or `/1.0/operations/.../websocket?secret=...`) is synchronously rewritten to `ws(s)://${location.host}/api/ws/hosts/${hostId}/incus-proxy/1.0/...` with `incudal-incus-ui.${wsTicket}` injected into the `protocols` array.
- Intercept click events on `<a href="/1.0/...">` download links to append `?ticket=${wsTicket}`.

- [ ] **Step 3: Configure `incus-ui/vite.config.ts` and `scripts/build-incus-ui.mjs`**

- Configure `incus-ui` Vite build with `base: '/incus-ui/'`.
- Create `scripts/build-incus-ui.mjs` to compute a content hash of `incus-ui/src`, `incus-ui/index.html`, `incus-ui/package.json`, and `incus-ui/vite.config.ts`, cache the compiled output in `incus-ui/.build-cache`, and copy it into `client/dist/admin/incus-ui` during `pnpm --filter client build:admin`.
- Add dev middleware in `client/vite.config.ts` (only when `appEntry === 'admin'`) and Fastify static/dev fallback so `/incus-ui/:hostId/ui` works seamlessly in both `pnpm dev:admin` (port 3002) and production Nginx (`deploy/nginx-split-intranet.conf.example`).

- [ ] **Step 4: Build `incus-ui` and verify `test:frontend-dist-boundary-guards`**

Run: `pnpm --filter client build && pnpm --filter server test:frontend-dist-boundary-guards`
Expected: PASS (`client/dist/admin/incus-ui/index.html` exists, `client/dist/user` has zero `incus-ui` files, boundary guard passes).

---

### Task 4: Admin Host Detail `IncusUI` Button & Frontend Guards

**Files:**
- Modify: `client/src/views/resources/MyHostDetailView.vue`
- Modify: `client/src/locales/zh-CN.ts`
- Modify: `client/src/locales/zh-TW.ts`
- Modify: `client/src/locales/en.ts`
- Modify: `server/scripts/test-incus-ui-proxy-guards.ts`

**Interfaces:**
- Consumes:
  - `isAdmin`, `host.value.id`, `host.value.tunnelOnline`, `needsVerification` in `MyHostDetailView.vue`
- Produces:
  - `openIncusUi()` action opening `/incus-ui/${host.value.id}/ui` in a new browser tab

- [ ] **Step 1: Add static guard assertions for `MyHostDetailView.vue` in `server/scripts/test-incus-ui-proxy-guards.ts`**

Assert that `MyHostDetailView.vue` defines `function openIncusUi()`, renders the button with `v-if="isAdmin"`, checks `host.tunnelOnline`, and opens `/incus-ui/${host.value.id}/ui` in a new tab.

- [ ] **Step 2: Add i18n keys and `IncusUI` button to `MyHostDetailView.vue`**

- In `client/src/locales/zh-CN.ts`, `zh-TW.ts`, and `en.ts` under `admin.hosts`:
  - `incusUi`: `'IncusUI'`
  - `incusUiTunnelOffline`: `'节点反向隧道未连接，无法打开 IncusUI'` (and corresponding zh-TW / en translations)
- In `client/src/views/resources/MyHostDetailView.vue`:
  - Add `function openIncusUi()`:
    ```typescript
    function openIncusUi() {
      if (!host.value || !isAdmin.value) return
      if (!host.value.tunnelOnline) {
        toast.error(t('admin.hosts.incusUiTunnelOffline'))
        return
      }
      window.open(`/incus-ui/${host.value.id}/ui`, '_blank', 'noopener,noreferrer')
    }
    ```
  - Add the button in the top-right action bar next to `openExtendModal`:
    ```vue
    <button
      v-if="isAdmin"
      :disabled="!!actionLoading || needsVerification || !host.tunnelOnline"
      :title="!host.tunnelOnline ? t('admin.hosts.incusUiTunnelOffline') : 'IncusUI'"
      class="btn-secondary btn-sm sm:btn"
      @click="openIncusUi"
    >
      <span>{{ t('admin.hosts.incusUi') }}</span>
    </button>
    ```

- [ ] **Step 3: Run all frontend & backend guard tests**

Run:
```bash
pnpm --filter client type-check
pnpm --filter server type-check
pnpm --filter server test:frontend-route-guards
pnpm --filter server test:frontend-color-tokens
pnpm --filter server test:frontend-i18n-keys
pnpm --filter server test:incus-ui-proxy-guards
pnpm build
```
Expected: ALL PASS.

---

### Task 5: End-to-End Verification on OrbStack `debian-test` VM

**Files:**
- Verify against live local environment (`debian-test` OrbStack VM + local PayIncus server & admin panel)

- [ ] **Step 1: Inspect `debian-test` VM Incus & tunnel status**

Check `orb -m debian-test exec sudo incus info` and verify `debian-test` is registered and connected to the local PayIncus server via reverse tunnel (`hostTunnelManager.isTunnelOnline(hostId) === true`).

- [ ] **Step 2: Verify end-to-end IncusUI flow**

1. Start local PayIncus backend (`3001`) and admin frontend (`3002`).
2. Log in as admin on the admin panel, navigate to the `debian-test` host detail page (`/admin/resources/hosts/:id`).
3. Click the top-right `IncusUI` button -> verify it opens `/incus-ui/:id/ui` in a new tab.
4. Verify passwordless/certless login directly into the Incus UI dashboard (`GET /api/admin/hosts/:id/incus-proxy/1.0` returns `auth: "trusted"`).
5. Verify listing instances, storage pools, networks, profiles, `/1.0/events` WebSocket stream, and opening an instance terminal WebSocket through the reverse tunnel.
6. Run `make check` / guard suite and commit changes with Conventional Commits.
