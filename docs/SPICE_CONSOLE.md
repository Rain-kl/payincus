# KVM graphical console

Running KVM instances have an **Actions → Graphical console** entry on the shared
customer/admin instance details page. The drawer supports keyboard, mouse,
Ctrl+Alt+Del, full screen, and explicit reconnect. Closing it disconnects the
session. LXC retains its text terminal.

## Connection path

Browser SPICE client → authenticated Panel WebSocket → Incus console operation.
For tunnel-enabled hosts, both the mTLS HTTP request and every mTLS WebSocket use
the existing Agent reverse tunnel. No SPICE TCP port, guest RDP service, or guest
Incus Agent is needed. Panel's existing WebSocket proxy configuration is reused.

POST `/api/ws/instances/:id/spice-session` requests Incus `type: vga`. One control
WebSocket keeps the operation alive. Every SPICE channel opens fd `0` again on
that **same operation**, following Incus `ConsoleInstanceDynamic`; creating a new
operation per display/input channel would disconnect the other channels.

The browser receives an opaque console capability, not Incus certificate paths,
operation IDs, or fd secrets. It is bound to the instance and originating login
session, supports only that console's channels, expires after two hours, and is
revoked when the main channel closes. A capability without an attached main
channel is cleaned within 75 seconds. It is never stored in browser storage.
Channels carry the capability in the WebSocket protocol request header, rather
than the URL, to keep it out of standard reverse-proxy access logs. The selected
response protocol is `binary`, not the capability. Auxiliary channels inherit it.

## Authorization and lifecycle

- Creation requires normal authenticated access, a current unbanned user, a
  running VM, and instance-owner, host-owner, or administrator permission.
- Every channel checks Origin and rechecks current permission/session validity.
  The backend target is pinned; a host migration or recreation invalidates it.
- Active sessions revalidate every 25 seconds. Existing logout, user revocation,
  ban, instance deletion, and shutdown hooks also close graphical sessions.
- Limit: one graphical session per instance, two per user, twelve channels per
  session, and one hundred sessions process-wide, including pending setup.
  General IP/user WebSocket limits count a console as one logical connection.
- Initial backend setup and channel setup are bounded to 20 seconds, with
  heartbeat and buffer limits. Disconnect during setup cancels pending work and
  closes backends that finish late. DELETE is idempotent after main-channel close.
- Capability headers and legacy query tickets are redacted by the request logger. Audit logs identify only the
  user and instance. Frame payloads and Incus responses are not logged.

The client uses native guest resolution and scrolls when needed. Browser keyboard
shortcuts may remain reserved by the browser. USB redirection, file transfer,
multi-monitor support, and full desktop-client audio parity are not promised.
The console does not implement Windows image/password/network provisioning.

## Client source and updates

`client/public/vendor/spice/` contains the SPICE HTML5 source used by the Incus web
UI, pinned to the commit in `UPSTREAM.txt`. Both frontend builds ship those ES
modules locally. Copyright notices and LGPL license files are retained; source
files remain directly available in the distributed frontend.

Local patches inherit WebSocket capability protocol headers across channels,
remove connection URLs from logs, keep playback separate from the
cursor, and clean all channels on stop. When updating the pinned source, retain
these fixes, review upstream changes, and repeat protocol and browser checks.

## Verification

`pnpm --filter server test:spice-console` runs a local TLS Incus fixture plus the
actual Fastify routes, real WebSockets, and actual transport code. It checks mTLS,
direct/tunnel connections, shared-operation channels, early handshake bytes,
ownership, Origin, login revocation, concurrent session limits, cancellation,
and capability redaction. DB access is replaced with controlled dependencies;
no production DB or host is accessed. OpenSSL is required for temporary test TLS.

Before release, also run type checks, terminal/auth/origin guards, frontend guards,
both frontend builds, and an actual VM console check. A local QEMU SPICE display
test verifies the browser protocol/rendering/input path; an Incus HTTP fixture
does **not** replace acceptance against an actual target Incus daemon.
