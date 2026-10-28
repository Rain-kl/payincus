import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  generateIncusUiAccessTicket,
  verifyIncusUiAccessTicket,
  revokeActionTicketsForSession
} from '../src/lib/action-ticket.js'
import {
  normalizeIncusProxyTargetPath,
  extractIncusUiTicketFromSubprotocols
} from '../src/routes/admin-incus-ui.js'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '../..')

// 1. Ticket unit tests
const token = generateIncusUiAccessTicket(1, 42, 1700000000, 'sid-admin-1')
assert.equal(typeof token, 'string')
assert.ok(token.length >= 24)

const firstCheck = verifyIncusUiAccessTicket(token, 42)
assert.equal(firstCheck.valid, true)
assert.equal(firstCheck.userId, 1)
assert.equal(firstCheck.hostId, 42)
assert.equal(firstCheck.sessionId, 'sid-admin-1')

const secondCheck = verifyIncusUiAccessTicket(token, 42)
assert.equal(secondCheck.valid, true)

const wrongHost = verifyIncusUiAccessTicket(token, 99)
assert.equal(wrongHost.valid, false)

revokeActionTicketsForSession('sid-admin-1')
const afterRevoke = verifyIncusUiAccessTicket(token, 42)
assert.equal(afterRevoke.valid, false)

// 2. Path normalization & security boundary tests
assert.equal(normalizeIncusProxyTargetPath('1.0', ''), '/1.0')
assert.equal(normalizeIncusProxyTargetPath('/1.0', ''), '/1.0')
assert.equal(normalizeIncusProxyTargetPath('1.0/instances', '?recursion=2'), '/1.0/instances?recursion=2')
assert.equal(normalizeIncusProxyTargetPath('1.0/events', '?type=operation,logging'), '/1.0/events?type=operation,logging')
assert.equal(
  normalizeIncusProxyTargetPath('1.0/operations/op-123/websocket', '?secret=sec-456'),
  '/1.0/operations/op-123/websocket?secret=sec-456'
)

// Forbidden paths must return null
assert.equal(normalizeIncusProxyTargetPath('', ''), null)
assert.equal(normalizeIncusProxyTargetPath('ui/assets/app.js', ''), null)
assert.equal(normalizeIncusProxyTargetPath('1.0/payincus', ''), null)
assert.equal(normalizeIncusProxyTargetPath('1.0/payincus/network', ''), null)
assert.equal(normalizeIncusProxyTargetPath('1.0/../etc/passwd', ''), null)
assert.equal(normalizeIncusProxyTargetPath('1.0/instances/..%2f..', ''), null)
assert.equal(normalizeIncusProxyTargetPath('1.0\\instances', ''), null)

// 3. Subprotocol extraction tests
const extracted = extractIncusUiTicketFromSubprotocols('binary, incudal-incus-ui.ticket_abc_123')
assert.equal(extracted.ticket, 'ticket_abc_123')
assert.deepEqual(extracted.forwardProtocols, ['binary'])

// 4. Static route guard assertions on server/src/routes/admin-incus-ui.ts
const routeSource = readFileSync(resolve(repoRoot, 'server/src/routes/admin-incus-ui.ts'), 'utf8')
assert.ok(
  routeSource.includes('hostTunnelManager.isTunnelOnline(host.id)') &&
    routeSource.includes('hostTunnelManager.createDuplexStream('),
  'admin-incus-ui.ts must strictly require online reverse tunnel and use createDuplexStream'
)
assert.ok(
  !routeSource.includes('connectPinned('),
  'admin-incus-ui.ts must only use reverse tunnel transport without direct connectPinned fallback'
)
assert.ok(
  routeSource.includes('validateWebSocketOrigin(request)'),
  'admin-incus-ui.ts must validate WebSocket Origin'
)
assert.ok(
  routeSource.includes("if (user.role !== 'admin')"),
  'admin-incus-ui.ts must enforce inline admin role check'
)

// 5. Frontend button & bridge assertions
const hostDetailView = readFileSync(
  resolve(repoRoot, 'client/src/views/resources/MyHostDetailView.vue'),
  'utf8'
)
assert.ok(
  hostDetailView.includes('openIncusUi') &&
    hostDetailView.includes("window.open(`/incus-ui/${host.value.id}/ui`, '_blank', 'noopener,noreferrer')") &&
    hostDetailView.includes("t('admin.hosts.incusUi')"),
  'MyHostDetailView.vue must expose the admin IncusUI button opening /incus-ui/:hostId/ui in a new tab'
)

const bridgeSource = readFileSync(
  resolve(repoRoot, 'incus-ui/src/lib/payincus-bridge.ts'),
  'utf8'
)
assert.ok(
  bridgeSource.includes('/api/admin/hosts/${hostId}/incus-proxy') &&
    bridgeSource.includes('/api/ws/hosts/${hostId}/incus-proxy') &&
    bridgeSource.includes('/api/admin/hosts/${hostId}/incus-ui-ticket') &&
    bridgeSource.includes('incudal-incus-ui.'),
  'incus-ui payincus-bridge.ts must rewrite REST and WebSocket traffic to the admin tunnel proxy endpoints'
)

console.log('incus ui proxy guard tests passed')
