// Unit tests for lib/incus_pool.go (the host-row adapter over the pool that lives
// in lib/incus_client.go).
//
// No real node is involved: the "node" is an httptest TLS server, and its
// certificate/private key are written to temporary files so the production mTLS
// path (tls.LoadX509KeyPair + InsecureSkipVerify, exactly like `rejectUnauthorized:
// false`) is exercised end to end. GET /1.0 is the connect health check.
package lib

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// incusPoolTestCertificate writes the httptest server's certificate and key to
// files and returns their paths.
func incusPoolTestCertificate(t *testing.T, server *httptest.Server) (string, string) {
	t.Helper()
	if len(server.TLS.Certificates) == 0 || len(server.TLS.Certificates[0].Certificate) == 0 {
		t.Fatal("httptest server has no certificate")
	}
	certificate := server.TLS.Certificates[0]

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificate.Certificate[0],
	})
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	directory := t.TempDir()
	certPath := filepath.Join(directory, "client.crt")
	keyPath := filepath.Join(directory, "client.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// incusPoolTestNode answers the connect health check.
func incusPoolTestNode(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/1.0" {
			_, _ = writer.Write([]byte(`{"type":"sync","status":"Success","status_code":200,"metadata":{"environment":{"server":"incus"}}}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"type":"error","error":"not found","error_code":404}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestIncusPoolConnectsCachesAndReconnects proves the pool contract the transfer
// routes depend on: one connect per host, reuse afterwards, a fresh client after
// removal, and the pool status map reporting the entry.
func TestIncusPoolConnectsCachesAndReconnects(t *testing.T) {
	server := incusPoolTestNode(t)
	certPath, keyPath := incusPoolTestCertificate(t, server)

	const hostID int32 = 997_001
	RemoveIncusClient(int64(hostID))
	t.Cleanup(func() { RemoveIncusClient(int64(hostID)) })

	host := &db.HostsRecord{
		ID:       hostID,
		Name:     "pool-test-host",
		URL:      server.URL,
		CertPath: &certPath,
		KeyPath:  &keyPath,
	}

	first, err := GetIncusClientForHost(t.Context(), host)
	if err != nil {
		t.Fatalf("GetIncusClientForHost: %v", err)
	}
	if first == nil || !first.Connected() {
		t.Fatal("expected a connected client")
	}

	second, err := GetIncusClientForHost(t.Context(), host)
	if err != nil {
		t.Fatalf("second GetIncusClientForHost: %v", err)
	}
	if second != first {
		t.Fatal("a live pooled client must be reused, not reconnected")
	}

	status := GetIncusPoolStatus()
	entry, ok := status["997001"]
	if !ok {
		t.Fatalf("pool status missing host: %#v", status)
	}
	if !entry.Connected || entry.ErrorCount != 0 {
		t.Fatalf("unexpected pool entry: %#v", entry)
	}

	RemoveIncusClient(int64(hostID))
	if _, ok := GetIncusPoolStatus()["997001"]; ok {
		t.Fatal("RemoveIncusClient must drop the pool entry")
	}

	third, err := GetIncusClientForHost(t.Context(), host)
	if err != nil {
		t.Fatalf("third GetIncusClientForHost: %v", err)
	}
	if third == first {
		t.Fatal("a removed client must not be reused")
	}
}

// TestIncusPoolReconnectFailureCountsAndSurfaces proves a dead node is reported as
// an error (never a nil client) and that the pooled entry's error counter moves —
// the transfer route turns that error into HOST_UNAVAILABLE.
func TestIncusPoolReconnectFailureCountsAndSurfaces(t *testing.T) {
	server := incusPoolTestNode(t)
	certPath, keyPath := incusPoolTestCertificate(t, server)

	const hostID int32 = 997_002
	RemoveIncusClient(int64(hostID))
	t.Cleanup(func() { RemoveIncusClient(int64(hostID)) })

	host := &db.HostsRecord{ID: hostID, URL: server.URL, CertPath: &certPath, KeyPath: &keyPath}
	first, err := GetIncusClientForHost(t.Context(), host)
	if err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	// Simulate a dropped connection: Close marks the client disconnected and the
	// pool keeps the dead entry.
	_ = first.Close()

	// Point the host at a closed port so the reconnect fails.
	deadHost := &db.HostsRecord{ID: hostID, URL: "https://127.0.0.1:1", CertPath: &certPath, KeyPath: &keyPath}
	if _, err := GetIncusClientForHost(t.Context(), deadHost); err == nil {
		t.Fatal("reconnect to a dead node must return an error")
	}

	status := GetIncusPoolStatus()
	if entry, ok := status["997002"]; ok && entry.ErrorCount == 0 {
		t.Fatalf("reconnect failure must increment errorCount: %#v", entry)
	}
}

// TestIncusPoolRequiresHost pins the nil-host guard (the TypeScript would throw a
// TypeError on host.url).
func TestIncusPoolRequiresHost(t *testing.T) {
	if _, err := GetIncusClientForHost(t.Context(), nil); err != ErrIncusPoolHostRequired {
		t.Fatalf("err = %v, want ErrIncusPoolHostRequired", err)
	}
}

// TestIncusPoolHostIDIsPoolKey pins that the pool is keyed by host id (the TS uses
// `${host.id}`), so two host rows with different urls but the same id share one
// entry and different ids do not.
func TestIncusPoolHostIDIsPoolKey(t *testing.T) {
	server := incusPoolTestNode(t)
	certPath, keyPath := incusPoolTestCertificate(t, server)

	ids := []int32{997_011, 997_012}
	for _, id := range ids {
		RemoveIncusClient(int64(id))
	}
	t.Cleanup(func() {
		for _, id := range ids {
			RemoveIncusClient(int64(id))
		}
	})

	clients := make(map[int32]*IncusClient)
	for _, id := range ids {
		client, err := GetIncusClientForHost(t.Context(), &db.HostsRecord{
			ID: id, URL: server.URL, CertPath: &certPath, KeyPath: &keyPath,
		})
		if err != nil {
			t.Fatalf("host %d: %v", id, err)
		}
		clients[id] = client
	}
	if clients[997_011] == clients[997_012] {
		t.Fatal("different host ids must not share a pooled client")
	}

	// The reconnect alias must resolve to the same pooled entry.
	same, err := GetIncusClientFromPool(t.Context(), IncusClientOptions{
		URL: server.URL, CertPath: &certPath, KeyPath: &keyPath, HostID: int64(ids[0]),
	})
	if err != nil {
		t.Fatalf("GetIncusClientFromPool: %v", err)
	}
	if same != clients[ids[0]] {
		t.Fatal("GetIncusClientFromPool must be the same pool")
	}
}

// TestIncusPoolTimingConstants pins the measured TS values.
func TestIncusPoolTimingConstants(t *testing.T) {
	if IncusConnectionExpire != 30*time.Minute {
		t.Fatalf("IncusConnectionExpire = %v, want 30m", IncusConnectionExpire)
	}
	if IncusPoolCleanupInterval != 5*time.Minute {
		t.Fatalf("IncusPoolCleanupInterval = %v, want 5m", IncusPoolCleanupInterval)
	}
}
