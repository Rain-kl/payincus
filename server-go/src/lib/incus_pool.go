// Port of server/src/lib/incus/incus-pool.ts (231 lines).
//
// The pool itself — reuse a live connection, single-flight reconnects, the
// 30-minute idle expiry and the 5-minute sweeper — is implemented in
// lib/incus_client.go (GetIncusClient / UpdateIncusClientResponseTime /
// RecordIncusClientError / GetIncusPoolStatus / RemoveIncusClient /
// CloseAllIncusClients). It lives there because every Phase-5 module imports the
// client and the pool from one file; moving it would be a pure rename with no
// behavioural change, so this file deliberately does NOT contain a second copy.
//
// What this file adds is the piece the TypeScript pool owns that Go needs
// explicitly: `getIncusClient(host)` takes a *host row*. Go has no structural
// typing, so every route must project db.HostsRecord onto IncusClientOptions.
// GetIncusClientForHost is that projection, in exactly one place:
//
//   - host.cert_path || host.certPath  → CertPath
//   - host.key_path  || host.keyPath   → KeyPath
//   - tunnelEnabled ?? tunnel_enabled  → TunnelEnabled
//   - targetHost ?? target_host ?? "127.0.0.1"
//   - targetPort ?? target_port ?? 8443
//   - hostId                           → HostID (also the pool key)
//
// A nil host is rejected before any I/O: the TypeScript would throw a TypeError on
// `host.url`, which surfaces as a 500; an explicit error is the same outcome with
// a usable message.
package lib

import (
	"context"
	"errors"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// Pool timing constants re-exported for monitoring/tests. The values (not the
// declarations) are the contract: CONNECTION_EXPIRE_MS = 30min, sweeper = 5min.
const (
	IncusConnectionExpire    = incusConnectionExpire
	IncusPoolCleanupInterval = incusPoolCleanupInterval
)

// ErrIncusPoolHostRequired mirrors the TypeError a missing host produces.
var ErrIncusPoolHostRequired = errors.New("incus pool: host is required")

// GetIncusClientForHost mirrors `getIncusClient(host)` from incus-pool.ts.
//
// It never blocks on a dead node longer than the dial/headers timeout carried by
// IncusClient, and concurrent callers for the same host share one connect attempt
// (incusConnecting in incus_client.go).
func GetIncusClientForHost(ctx context.Context, host *db.HostsRecord) (*IncusClient, error) {
	if host == nil {
		return nil, ErrIncusPoolHostRequired
	}
	return GetIncusClient(ctx, IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
}

// GetIncusClientFromPool mirrors the `getIncusClientFromPool` alias
// (`export const getIncusClientFromPool = getIncusClient`) used by the traffic
// scheduler. The TypeScript alias takes a host; the Go pool entry point takes
// options, so callers with a host row use GetIncusClientForHost.
func GetIncusClientFromPool(ctx context.Context, options IncusClientOptions) (*IncusClient, error) {
	return GetIncusClient(ctx, options)
}
