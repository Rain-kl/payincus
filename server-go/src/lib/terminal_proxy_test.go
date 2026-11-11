// Terminal-ticket and terminal-session invariants that live outside the route
// package. The test names start with TestWebSocket so the Wave E acceptance
// filter (`-run 'TestWebSocket|TestSpice'`) covers them.
package lib

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

func terminalTicketPtr(value int32) *int32 { return &value }

// TestWebSocketTerminalTicketSingleUse pins the one-shot contract: the second
// consume of the same ticket is rejected, and a ticket bound to one instance
// cannot be replayed against another (the mismatch burns it).
func TestWebSocketTerminalTicketSingleUse(t *testing.T) {
	token := GenerateTerminalAccessTicket(11, 900001, 1_700_000_000, nil)
	first := ConsumeTerminalAccessTicket(token, terminalTicketPtr(900001))
	if !first.Valid || first.UserID != 11 || first.InstanceID != 900001 {
		t.Fatalf("first consume = %+v", first)
	}
	replay := ConsumeTerminalAccessTicket(token, terminalTicketPtr(900001))
	if replay.Valid || replay.Error != "Ticket not found or already used" {
		t.Fatalf("replay = %+v", replay)
	}
}

// TestWebSocketTerminalTicketInstanceBinding pins the bound-instance rule.
func TestWebSocketTerminalTicketInstanceBinding(t *testing.T) {
	token := GenerateTerminalAccessTicket(12, 900002, 1_700_000_000, nil)
	mismatch := ConsumeTerminalAccessTicket(token, terminalTicketPtr(900003))
	if mismatch.Valid || mismatch.Error != "Instance mismatch" {
		t.Fatalf("instance mismatch = %+v", mismatch)
	}
	if again := ConsumeTerminalAccessTicket(token, terminalTicketPtr(900002)); again.Valid {
		t.Fatal("a mismatched ticket must be burned, not reusable for the right instance")
	}
}

// TestWebSocketTerminalTicketTypeIsolation pins that other action tickets cannot
// open a terminal.
func TestWebSocketTerminalTicketTypeIsolation(t *testing.T) {
	oauthTicket := GenerateOAuthBindTicket(13, 1_700_000_000, nil)
	if result := ConsumeTerminalAccessTicket(oauthTicket, terminalTicketPtr(900004)); result.Valid {
		t.Fatal("an oauth-bind ticket must not be accepted as a terminal ticket")
	}
}

// TestWebSocketTerminalSessionCountersAndCloseHelpers pins the process-local
// session accounting used by the per-user / per-instance limits.
func TestWebSocketTerminalSessionCountersAndCloseHelpers(t *testing.T) {
	ResetTerminalSessionsForTest()
	defer ResetTerminalSessionsForTest()

	AddTerminalSessionForTest(&TerminalSession{ID: "term_a", UserID: 21, InstanceID: 7001})
	AddTerminalSessionForTest(&TerminalSession{ID: "term_b", UserID: 21, InstanceID: 7001})
	AddTerminalSessionForTest(&TerminalSession{ID: "term_c", UserID: 22, InstanceID: 7002})

	stats := GetActiveSessionStats()
	if stats.Total != 3 || stats.ByUser[21] != 2 || stats.ByInstance[7001] != 2 || stats.ByUser[22] != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	if closed := CloseUserSessions(21, "test"); closed != 2 {
		t.Fatalf("CloseUserSessions(21) = %d, want 2", closed)
	}
	if stats := GetActiveSessionStats(); stats.Total != 1 || stats.ByUser[22] != 1 {
		t.Fatalf("stats after close = %+v", stats)
	}

	AddTerminalSessionForTest(&TerminalSession{ID: "term_d", UserID: 23, InstanceID: 7003})
	if closed := CloseInstanceSessions(7002, "test"); closed != 1 {
		t.Fatalf("CloseInstanceSessions(7002) = %d, want 1", closed)
	}
	if closed := CloseAllSessions("test"); closed != 1 {
		t.Fatalf("CloseAllSessions = %d, want 1", closed)
	}
	if stats := GetActiveSessionStats(); stats.Total != 0 {
		t.Fatalf("sessions after CloseAllSessions = %+v", stats)
	}
}

// TestWebSocketTerminalSessionIDShape pins the term_<ms>_<rand> identifier.
func TestWebSocketTerminalSessionIDShape(t *testing.T) {
	id := terminalGenerateSessionID()
	if len(id) < len("term_0_x") || id[:5] != "term_" {
		t.Fatalf("session id = %q", id)
	}
}

// TestWebSocketTerminalResizeBounds pins the resize clamp of terminal-proxy.ts:
// cols in [1,500], rows in [1,200], falsy/NaN → 80/24.
func TestWebSocketTerminalResizeBounds(t *testing.T) {
	cases := []struct {
		cols, rows         any
		wantCols, wantRows float64
	}{
		{nil, nil, 80, 24},
		{float64(0), float64(0), 80, 24},
		{float64(120), float64(40), 120, 40},
		{float64(-5), float64(-5), 1, 1},
		{float64(9999), float64(9999), 500, 200},
		{"100", "50", 100, 50},
		{"not-a-number", "nope", 80, 24},
		{"NaN", "NaN", 80, 24}, // Number("NaN") is NaN → falsy → default
		{true, false, 1, 24},   // Number(false) is 0 → `|| 24` default
	}
	for _, tc := range cases {
		cols, rows := terminalResizeBounds(tc.cols, tc.rows)
		if cols != tc.wantCols || rows != tc.wantRows {
			t.Errorf("terminalResizeBounds(%v,%v) = (%v,%v), want (%v,%v)", tc.cols, tc.rows, cols, rows, tc.wantCols, tc.wantRows)
		}
	}
}

// terminalTestCertificates writes a throwaway self-signed certificate/key pair,
// because CreateIncusConsoleConnection insists on a readable pair (the Node code
// reads the same files).
func terminalTestCertificates(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "payincus-terminal-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	directory := t.TempDir()
	certPath := filepath.Join(directory, "client.crt")
	keyPath := filepath.Join(directory, "client.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// TestWebSocketTerminalSessionProxiesData drives CreateTerminalSession against a
// fake Incus node: exec payload → fd websockets → browser bytes forwarded and
// echoed, resize translated onto the control channel, and the session drops out of
// the stats when the browser closes.
func TestWebSocketTerminalSessionProxiesData(t *testing.T) {
	ResetTerminalSessionsForTest()
	defer ResetTerminalSessionsForTest()

	const operationID = "op-terminal-test"
	dataSockets := make(chan *WebSocket, 2)
	controlMessages := make(chan []byte, 4)
	var execBody map[string]any

	fakeIncus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/instances/test-instance/exec":
			var decoded map[string]any
			_ = json.NewDecoder(r.Body).Decode(&decoded)
			execBody = decoded
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"type":"async","status":"Operation created","operation":"/1.0/operations/%s","metadata":{"id":"%s","metadata":{"fds":{"0":"data-secret","control":"control-secret"}}}}`,
				operationID, operationID)
		case IsWebSocketUpgrade(r) && strings.HasSuffix(r.URL.Path, "/operations/"+operationID+"/websocket"):
			socket, err := UpgradeWebSocket(w, r, nil)
			if err != nil {
				return
			}
			if r.URL.Query().Get("secret") == "control-secret" {
				for {
					payload, _, readErr := socket.ReadMessage()
					if readErr != nil {
						return
					}
					controlMessages <- payload
				}
			}
			dataSockets <- socket
			for {
				payload, _, readErr := socket.ReadMessage()
				if readErr != nil {
					return
				}
				_ = socket.SendBinary([]byte(strings.ToUpper(string(payload))))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fakeIncus.Close()

	certPath, keyPath := terminalTestCertificates(t)
	host := &db.HostsRecord{
		ID:         1,
		Name:       "fake-incus",
		URL:        fakeIncus.URL,
		CertPath:   &certPath,
		KeyPath:    &keyPath,
		TargetHost: "127.0.0.1",
		TargetPort: 8443,
	}

	browserServer := newSpiceTestServer(t)
	browserClient := browserServer.dial(t, "/api/ws/instances/1/terminal")
	browserSide := browserServer.accept(t)

	session, err := CreateTerminalSession(browserSide, host, 1, "test-instance", 5, "container", nil)
	if err != nil {
		t.Fatalf("CreateTerminalSession: %v", err)
	}
	if session.ConnectionMode() != "exec" {
		t.Fatalf("mode = %q, want exec", session.ConnectionMode())
	}
	if execBody["wait-for-websocket"] != true || execBody["interactive"] != true {
		t.Fatalf("exec payload = %v", execBody)
	}
	if command, ok := execBody["command"].([]any); !ok || len(command) < 3 || command[0] != "/bin/sh" {
		t.Fatalf("exec command = %v", execBody["command"])
	}

	// The first browser message is the `connected` notification.
	payload, isBinary, err := readWSMessage(t, browserClient)
	if err != nil || isBinary || !strings.Contains(string(payload), `"type":"connected"`) {
		t.Fatalf("connected message = (%q,%t,%v)", payload, isBinary, err)
	}

	// Browser → Incus → browser.
	dataSide := <-dataSockets
	if err := browserClient.SendBinary([]byte("hi")); err != nil {
		t.Fatalf("browser send: %v", err)
	}
	payload, isBinary, err = readWSMessage(t, browserClient)
	if err != nil || !isBinary || string(payload) != "HI" {
		t.Fatalf("echo = (%q,%t,%v)", payload, isBinary, err)
	}

	// Resize goes to the control channel, not the data channel.
	if err := browserClient.SendText(`{"type":"resize","cols":100,"rows":50}`); err != nil {
		t.Fatalf("resize send: %v", err)
	}
	select {
	case resize := <-controlMessages:
		if !strings.Contains(string(resize), `"window-resize"`) || !strings.Contains(string(resize), `"width":100`) {
			t.Fatalf("resize command = %s", resize)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resize command reached the control channel")
	}

	// Closing the browser removes the session.
	browserClient.Terminate()
	deadline := time.Now().Add(5 * time.Second)
	for GetActiveSessionStats().Total != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session not cleaned up: %+v", GetActiveSessionStats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = dataSide
}
