// Tests for the host-agent route module (server/src/routes/agent.ts, 12 routes).
//
// The suite proves the parts that need no database:
//
//   - the complete route inventory and each route's authentication class
//     (public / web user / web admin / HMAC);
//   - the HMAC verifier end to end: valid signature, wrong signature, stale
//     timestamp, body-hash mismatch, unregistered/disabled identity, nonce replay;
//   - the reverse-auth red line (WAVE_D_SPEC §0.2): a Web JWT or a pat_ token sent
//     to an agent endpoint is rejected, and a Web JWT cannot open /tunnel;
//   - the public one-time-token branches (invalid token, certificate-before-token
//     ordering, query-schema rejection);
//   - the heartbeat state machine (locked node, caddy install/uninstall, report
//     error precedence, pending create queue, storage snapshot preservation).
//
// Everything that needs rows (host ownership 403, real credential rotation, real
// token consumption) is covered by test/conformance/requests-agent.json, which
// runs Node and Go against the same parity database.
package routes

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

func agentTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAgent(router)
	return router
}

func agentDoRequest(t *testing.T, router *gin.Engine, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func agentDecodeJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

// ==================== route inventory + auth classes ====================

func TestAgentRouteInventory(t *testing.T) {
	router := agentTestRouter()
	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		"GET /api/agent/install.sh",
		"GET /api/agent/distribution",
		"GET /api/agent/install-config/:token",
		"GET /api/agent/hosts/:hostId/status",
		"POST /api/agent/hosts/:hostId/install-command",
		"GET /api/agent/admin/hosts/:hostId/status",
		"POST /api/agent/admin/hosts/:hostId/credentials",
		"POST /api/agent/admin/hosts/:hostId/install-command",
		"POST /api/agent/startup-keys",
		"POST /api/agent/security-report",
		"POST /api/agent/heartbeat",
		"GET /api/agent/tunnel",
	}
	if len(registered) != len(want) {
		t.Fatalf("registered %d routes, want exactly %d: %v", len(registered), len(want), registered)
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("missing route %s", route)
		}
	}
}

func TestAgentAnonymousAuthClasses(t *testing.T) {
	router := agentTestRouter()
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		status int
		code   string
	}{
		{"install.sh is public", http.MethodGet, "/api/agent/install.sh", nil, http.StatusOK, ""},
		{"distribution is public", http.MethodGet, "/api/agent/distribution", nil, http.StatusOK, ""},
		{"install-config rejects a malformed token", http.MethodGet, "/api/agent/install-config/not-a-token", nil, http.StatusForbidden, ""},
		{"user status needs a JWT", http.MethodGet, "/api/agent/hosts/1/status", nil, http.StatusUnauthorized, ""},
		{"user install-command needs a JWT", http.MethodPost, "/api/agent/hosts/1/install-command", []byte(`{}`), http.StatusUnauthorized, ""},
		{"admin status needs a JWT", http.MethodGet, "/api/agent/admin/hosts/1/status", nil, http.StatusUnauthorized, ""},
		{"admin credentials needs a JWT", http.MethodPost, "/api/agent/admin/hosts/1/credentials", []byte(`{}`), http.StatusUnauthorized, ""},
		{"admin install-command needs a JWT", http.MethodPost, "/api/agent/admin/hosts/1/install-command", []byte(`{}`), http.StatusUnauthorized, ""},
		{"startup-keys needs HMAC", http.MethodPost, "/api/agent/startup-keys", []byte(`{}`), http.StatusUnauthorized, "AGENT_AUTH_REQUIRED"},
		{"security-report needs HMAC", http.MethodPost, "/api/agent/security-report", []byte(`{}`), http.StatusUnauthorized, ""},
		{"heartbeat needs HMAC", http.MethodPost, "/api/agent/heartbeat", []byte(`{}`), http.StatusUnauthorized, "AGENT_AUTH_REQUIRED"},
		{"tunnel plain GET is 404", http.MethodGet, "/api/agent/tunnel", nil, http.StatusNotFound, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := agentDoRequest(t, router, testCase.method, testCase.path, testCase.body, nil)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, testCase.status, recorder.Body.String())
			}
			if testCase.code != "" {
				decoded := agentDecodeJSON(t, recorder)
				if decoded["code"] != testCase.code {
					t.Fatalf("code = %v, want %s (body %s)", decoded["code"], testCase.code, recorder.Body.String())
				}
			}
		})
	}
}

// TestAgentWebCredentialsAreRejectedOnEveryHMACEndpoint is the WAVE_D_SPEC §0.2
// reverse test: a Web JWT and a pat_ token must never satisfy the agent plane.
func TestAgentWebCredentialsAreRejectedOnEveryHMACEndpoint(t *testing.T) {
	router := agentTestRouter()
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 1, Username: "admin", Role: "admin", Status: "active", SID: "session-1",
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	credentials := map[string]string{
		"web-jwt":   "Bearer " + token,
		"pat-token": "Bearer pat_" + strings.Repeat("a", 43),
		"poa-token": "Bearer poa_" + strings.Repeat("b", 43),
	}
	endpoints := []string{"/api/agent/startup-keys", "/api/agent/security-report", "/api/agent/heartbeat"}
	for credentialName, header := range credentials {
		for _, endpoint := range endpoints {
			recorder := agentDoRequest(t, router, http.MethodPost, endpoint, []byte(`{}`),
				map[string]string{"Authorization": header})
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("%s on %s: status = %d, want 401 (body %s)",
					credentialName, endpoint, recorder.Code, recorder.Body.String())
			}
			if strings.Contains(strings.ToLower(recorder.Body.String()), "signature verification failed") {
				t.Fatalf("%s reached HMAC verification instead of being rejected as unauthenticated", credentialName)
			}
		}
	}
}

// ==================== HMAC verifier ====================

// agentProbeRouter exposes the authenticator only (no handler side effects).
func agentProbeRouter() *gin.Engine {
	router := gin.New()
	probe := func(c *gin.Context) {
		if agentAuthenticateRequest(c) == nil {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	router.GET("/probe", probe)
	router.POST("/probe", probe)
	return router
}

func agentSignedHeaders(t *testing.T, secret, agentID, method, path string, body []byte, nonce, timestamp string) map[string]string {
	t.Helper()
	var decoded any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode signed body: %v", err)
		}
	}
	bodyHash := lib.AgentBodyHashForRequest(method, len(body) > 0, decoded)
	signature := lib.CreateAgentSignature(secret, lib.AgentSigningPayload{
		Method:    method,
		Path:      path,
		Timestamp: timestamp,
		Nonce:     nonce,
		BodyHash:  bodyHash,
	})
	return map[string]string{
		"X-Incus-Agent-Id":    agentID,
		"X-Incus-Timestamp":   timestamp,
		"X-Incus-Nonce":       nonce,
		"X-Incus-Body-Sha256": bodyHash,
		"X-Incus-Signature":   signature,
		"Content-Type":          "application/json",
	}
}

func TestAgentHMACAuthenticationMatrix(t *testing.T) {
	const encryptionEnv = "test-encryption-key-for-agent-tests"
	t.Setenv("ENCRYPTION_KEY", encryptionEnv)

	secret := lib.GenerateAgentSecret()
	encrypted, err := lib.EncryptSensitiveData(secret)
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	agent := &models.HostAgent{
		ID:              7,
		HostID:          42,
		AgentID:         lib.GenerateAgentID(),
		SecretHash:      lib.HashAgentSecret(secret),
		SecretEncrypted: encrypted,
		Enabled:         true,
		Status:          "online",
	}
	disabled := *agent
	disabled.ID = 8
	disabled.AgentID = lib.GenerateAgentID()
	disabled.Enabled = false

	originalLoad, originalRecord := agentLoadByIdentity, agentRecordNonce
	defer func() { agentLoadByIdentity, agentRecordNonce = originalLoad, originalRecord }()

	agentLoadByIdentity = func(_ context.Context, agentID string) *models.HostAgent {
		if agentID == agent.AgentID {
			return agent
		}
		if agentID == disabled.AgentID {
			return &disabled
		}
		return nil
	}
	usedNonces := map[string]bool{}
	agentRecordNonce = func(_ context.Context, agentID, nonce string) error {
		key := agentID + "\x00" + nonce
		if usedNonces[key] {
			return lib.ErrAgentNonceReplay
		}
		usedNonces[key] = true
		return nil
	}

	router := agentProbeRouter()
	now := time.Now().UTC()
	freshTimestamp := fmt.Sprintf("%d", now.UnixMilli())
	body := []byte(`{"version":"1.2.3"}`)

	t.Run("valid signature is accepted", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/probe", body, "nonce-valid-0001", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("replayed nonce is rejected", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/probe", body, "nonce-replay-01", freshTimestamp)
		first := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if first.Code != http.StatusOK {
			t.Fatalf("first request status = %d (body %s)", first.Code, first.Body.String())
		}
		second := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if second.Code != http.StatusUnauthorized {
			t.Fatalf("replay status = %d, want 401", second.Code)
		}
		if decoded := agentDecodeJSON(t, second); decoded["code"] != "AGENT_NONCE_REPLAY" {
			t.Fatalf("replay code = %v, want AGENT_NONCE_REPLAY", decoded["code"])
		}
	})

	t.Run("wrong signature is rejected", func(t *testing.T) {
		headers := agentSignedHeaders(t, lib.GenerateAgentSecret(), agent.AgentID, http.MethodPost, "/probe", body, "nonce-wrongsig1", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if decoded := agentDecodeJSON(t, recorder); decoded["code"] != "AGENT_SIGNATURE_INVALID" {
			t.Fatalf("code = %v, want AGENT_SIGNATURE_INVALID", decoded["code"])
		}
	})

	t.Run("tampered path is rejected", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/other-path", body, "nonce-path00001", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
	})

	t.Run("stale timestamp is rejected", func(t *testing.T) {
		stale := fmt.Sprintf("%d", now.Add(-6*time.Minute).UnixMilli())
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/probe", body, "nonce-stale0001", stale)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if decoded := agentDecodeJSON(t, recorder); decoded["code"] != "AGENT_AUTH_EXPIRED" {
			t.Fatalf("code = %v, want AGENT_AUTH_EXPIRED", decoded["code"])
		}
	})

	t.Run("future timestamp is rejected", func(t *testing.T) {
		future := fmt.Sprintf("%d", now.Add(6*time.Minute).UnixMilli())
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/probe", body, "nonce-future001", future)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
	})

	t.Run("body hash mismatch is rejected", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/probe", body, "nonce-bodyhash1", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", []byte(`{"version":"9.9.9"}`), headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if decoded := agentDecodeJSON(t, recorder); decoded["code"] != "AGENT_BODY_HASH_MISMATCH" {
			t.Fatalf("code = %v, want AGENT_BODY_HASH_MISMATCH", decoded["code"])
		}
	})

	t.Run("unregistered identity is rejected", func(t *testing.T) {
		unknownID := lib.GenerateAgentID()
		headers := agentSignedHeaders(t, secret, unknownID, http.MethodPost, "/probe", body, "nonce-unknown01", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if decoded := agentDecodeJSON(t, recorder); decoded["code"] != "AGENT_NOT_REGISTERED" {
			t.Fatalf("code = %v, want AGENT_NOT_REGISTERED", decoded["code"])
		}
	})

	t.Run("disabled identity is rejected", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, disabled.AgentID, http.MethodPost, "/probe", body, "nonce-disabled1", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if decoded := agentDecodeJSON(t, recorder); decoded["code"] != "AGENT_DISABLED" {
			t.Fatalf("code = %v, want AGENT_DISABLED", decoded["code"])
		}
	})

	t.Run("invalid header shapes are rejected before any lookup", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/probe", body, "nonce-badheader", freshTimestamp)
		headers["X-Incus-Agent-Id"] = "not-an-agent-id"
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, headers)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		decoded := agentDecodeJSON(t, recorder)
		if decoded["code"] != "AGENT_AUTH_INVALID" {
			t.Fatalf("code = %v, want AGENT_AUTH_INVALID", decoded["code"])
		}
		if decoded["details"] != "invalid agent id" {
			t.Fatalf("details = %v, want \"invalid agent id\"", decoded["details"])
		}
	})

	t.Run("missing headers are rejected", func(t *testing.T) {
		recorder := agentDoRequest(t, router, http.MethodPost, "/probe", body, map[string]string{"Content-Type": "application/json"})
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
		if decoded := agentDecodeJSON(t, recorder); decoded["code"] != "AGENT_AUTH_REQUIRED" {
			t.Fatalf("code = %v, want AGENT_AUTH_REQUIRED", decoded["code"])
		}
	})

	t.Run("GET without a body hashes the empty string", func(t *testing.T) {
		headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodGet, "/probe", nil, "nonce-get000001", freshTimestamp)
		recorder := agentDoRequest(t, router, http.MethodGet, "/probe", nil, headers)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}
	})
}

// TestAgentJWTCannotOpenTunnel proves the WebSocket route authenticates with HMAC
// only: a browser/Web JWT gets an upgraded socket and then close code 4001.
func TestAgentJWTCannotOpenTunnel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAgent(router)
	server := httptest.NewServer(router)
	defer server.Close()

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 1, Username: "admin", Role: "admin", Status: "active", SID: "session-1",
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	for name, header := range map[string]string{
		"anonymous": "",
		"web-jwt":   "Bearer " + token,
		"pat-token": "Bearer pat_" + strings.Repeat("a", 43),
	} {
		t.Run(name, func(t *testing.T) {
			client, response := agentTunnelDial(t, server.URL, header)
			defer client.close()
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status = %d, want 101 (the route upgrades before authenticating)", response.StatusCode)
			}
			code, _ := agentTunnelReadClose(t, client)
			if code != 4001 {
				t.Fatalf("close code = %d, want 4001 (agent authentication failed)", code)
			}
		})
	}
}

// TestAgentSignedSecurityReportAndHeartbeat covers the authenticated path of both
// HMAC report endpoints through the real router (body hash -> signature ->
// nonce -> handler -> response), with the DB touch points stubbed.
func TestAgentSignedSecurityReportAndHeartbeat(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "test-encryption-key-for-agent-tests")
	secret := lib.GenerateAgentSecret()
	encrypted, err := lib.EncryptSensitiveData(secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	agent := &models.HostAgent{
		ID: 7, HostID: 42, AgentID: lib.GenerateAgentID(),
		SecretEncrypted: encrypted, Enabled: true, Status: "online",
	}

	originalLoad, originalNonce := agentLoadByIdentity, agentRecordNonce
	originalReport, originalState := agentRecordSecurityReport, agentReadSecurityState
	originalDeps := agentHeartbeatDepsFor
	defer func() {
		agentLoadByIdentity, agentRecordNonce = originalLoad, originalNonce
		agentRecordSecurityReport, agentReadSecurityState = originalReport, originalState
		agentHeartbeatDepsFor = originalDeps
	}()

	agentLoadByIdentity = func(_ context.Context, agentID string) *models.HostAgent {
		if agentID == agent.AgentID {
			return agent
		}
		return nil
	}
	agentRecordNonce = func(_ context.Context, _, _ string) error { return nil }
	agentRecordSecurityReport = func(_ context.Context, hostID int32, _ any) ([]string, error) {
		if hostID != agent.HostID {
			t.Fatalf("recordReport host = %d", hostID)
		}
		return []string{"0123456789abcdef0123456789abcdef"}, nil
	}
	generation := int32(7)
	agentReadSecurityState = func(_ context.Context, hostID int32) (*models.NodeSecurityState, error) {
		if hostID != agent.HostID {
			t.Fatalf("state host = %d", hostID)
		}
		return &models.NodeSecurityState{HostID: hostID, RecoveryBlocked: true, RecoveryGeneration: generation}, nil
	}
	capture := &agentHeartbeatCapture{}
	agentHeartbeatDepsFor = func() *agentHeartbeatDeps { return agentFakeHeartbeatDeps(capture) }

	router := agentTestRouter()
	timestamp := fmt.Sprintf("%d", time.Now().UTC().UnixMilli())
	reportBody := []byte(`{"state":{"enabled":true,"locked":false,"revision":1},"events":[]}`)
	headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/api/agent/security-report",
		reportBody, "nonce-report-001", timestamp)
	recorder := agentDoRequest(t, router, http.MethodPost, "/api/agent/security-report", reportBody, headers)
	if recorder.Code != http.StatusOK {
		t.Fatalf("security-report status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := agentDecodeJSON(t, recorder)
	if body["recoveryBlocked"] != true || body["recoveryGeneration"] != float64(7) {
		t.Fatalf("security-report body = %v", body)
	}
	ack, ok := body["ack"].([]any)
	if !ok || len(ack) != 1 || ack[0] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("ack = %v", body["ack"])
	}

	heartbeatBody := []byte(`{"nodeApi":"incus_node_v1","incus":{"available":true},"instances":{"available":true},"resources":{"storageAvailable":true,"network":{"cards":[]}},"storage":{"available":true,"collectionComplete":true},"caddy":{"available":false}}`)
	headers = agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/api/agent/heartbeat",
		heartbeatBody, "nonce-heartbeat-1", timestamp)
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/heartbeat", heartbeatBody, headers)
	if recorder.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	heartbeat := agentDecodeJSON(t, recorder)
	if heartbeat["ok"] != true || heartbeat["taskPollIntervalSeconds"] != float64(15) {
		t.Fatalf("heartbeat body = %v", heartbeat)
	}
	if len(capture.completed) != 1 || capture.completed[0] != "" {
		t.Fatalf("complete calls = %v", capture.completed)
	}

	// A replay must be rejected before the handler runs.
	agentRecordNonce = func(_ context.Context, _, _ string) error { return lib.ErrAgentNonceReplay }
	headers = agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/api/agent/heartbeat",
		heartbeatBody, "nonce-heartbeat-2", timestamp)
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/heartbeat", heartbeatBody, headers)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["code"] != "AGENT_NONCE_REPLAY" {
		t.Fatalf("replay code = %v", body["code"])
	}
}

func TestAgentSecurityReportFailureMapping(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "test-encryption-key-for-agent-tests")
	secret := lib.GenerateAgentSecret()
	encrypted, err := lib.EncryptSensitiveData(secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	agent := &models.HostAgent{ID: 7, HostID: 42, AgentID: lib.GenerateAgentID(), SecretEncrypted: encrypted, Enabled: true}

	originalLoad, originalNonce := agentLoadByIdentity, agentRecordNonce
	originalReport, originalState := agentRecordSecurityReport, agentReadSecurityState
	defer func() {
		agentLoadByIdentity, agentRecordNonce = originalLoad, originalNonce
		agentRecordSecurityReport, agentReadSecurityState = originalReport, originalState
	}()
	agentLoadByIdentity = func(context.Context, string) *models.HostAgent { return agent }
	agentRecordNonce = func(context.Context, string, string) error { return nil }

	router := agentTestRouter()
	body := []byte(`{"state":{"enabled":true,"locked":false,"revision":1},"events":[]}`)
	timestamp := fmt.Sprintf("%d", time.Now().UTC().UnixMilli())

	agentRecordSecurityReport = func(context.Context, int32, any) ([]string, error) {
		return nil, errors.New("report failed")
	}
	headers := agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/api/agent/security-report", body, "nonce-report-fail1", timestamp)
	recorder := agentDoRequest(t, router, http.MethodPost, "/api/agent/security-report", body, headers)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}

	agentRecordSecurityReport = func(context.Context, int32, any) ([]string, error) { return []string{}, nil }
	agentReadSecurityState = func(context.Context, int32) (*models.NodeSecurityState, error) {
		return nil, errors.New("state failed")
	}
	headers = agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/api/agent/security-report", body, "nonce-report-fail2", timestamp)
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/security-report", body, headers)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}

	// No state row: recoveryBlocked=false, generation=0 (the TypeScript's `?? 0`).
	agentReadSecurityState = func(context.Context, int32) (*models.NodeSecurityState, error) { return nil, nil }
	headers = agentSignedHeaders(t, secret, agent.AgentID, http.MethodPost, "/api/agent/security-report", body, "nonce-report-fail3", timestamp)
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/security-report", body, headers)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	decoded := agentDecodeJSON(t, recorder)
	if decoded["recoveryBlocked"] != false || decoded["recoveryGeneration"] != float64(0) {
		t.Fatalf("body = %v", decoded)
	}
	if ack, ok := decoded["ack"].([]any); !ok || len(ack) != 0 {
		t.Fatalf("ack = %v, want []", decoded["ack"])
	}
}

// ==================== tunnel helpers ====================

type agentTunnelClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (c *agentTunnelClient) close() { _ = c.conn.Close() }

func agentTunnelDial(t *testing.T, serverURL, authorization string) (*agentTunnelClient, *http.Response) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("random: %v", err)
	}
	request := "GET /api/agent/tunnel HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(keyBytes) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	if authorization != "" {
		request += "Authorization: " + authorization + "\r\n"
	}
	request += "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	return &agentTunnelClient{conn: conn, reader: reader}, response
}

func agentTunnelReadClose(t *testing.T, client *agentTunnelClient) (int, string) {
	t.Helper()
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(client.reader, header); err != nil {
		t.Fatalf("read close header: %v", err)
	}
	opcode := header[0] & 0x0F
	length := uint64(header[1] & 0x7F)
	if length == 126 {
		extended := make([]byte, 2)
		if _, err := io.ReadFull(client.reader, extended); err != nil {
			t.Fatalf("read length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(client.reader, payload); err != nil {
		t.Fatalf("read close payload: %v", err)
	}
	if opcode != 0x8 {
		t.Fatalf("opcode = %d, want close (0x8)", opcode)
	}
	if len(payload) < 2 {
		t.Fatalf("close payload too short: %v", payload)
	}
	return int(binary.BigEndian.Uint16(payload)), string(payload[2:])
}

// ==================== public endpoints ====================

func TestAgentInstallScriptServesTemplate(t *testing.T) {
	router := agentTestRouter()
	recorder := agentDoRequest(t, router, http.MethodGet, "/api/agent/install.sh", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/x-shellscript; charset=utf-8" {
		t.Fatalf("content type = %q", contentType)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("cache control = %q", cache)
	}
	template, ok := hostsReadInstallScript()
	if !ok {
		t.Skip("install.sh template not present in this checkout")
	}
	if recorder.Body.String() != template {
		t.Fatalf("served script differs from server/templates/install.sh (%d vs %d bytes)",
			recorder.Body.Len(), len(template))
	}
}

func TestAgentDistributionDescriptor(t *testing.T) {
	router := agentTestRouter()
	recorder := agentDoRequest(t, router, http.MethodGet, "/api/agent/distribution", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := agentDecodeJSON(t, recorder)
	want := map[string]any{
		"repository": "Rain-kl/incus",
		"package":    "incus-payincus",
		"installer":  "/api/agent/install.sh",
		"managedBy":  "apt",
	}
	for key, value := range want {
		if body[key] != value {
			t.Fatalf("%s = %v, want %v", key, body[key], value)
		}
	}
	channels, ok := body["channels"].([]any)
	if !ok || len(channels) != 2 || channels[0] != "latest" || channels[1] != "canary" {
		t.Fatalf("channels = %v", body["channels"])
	}
}

func TestAgentInstallConfigTokenBranches(t *testing.T) {
	router := agentTestRouter()

	// Unknown/short token: the plaintext branch answers the exact shell comment.
	recorder := agentDoRequest(t, router, http.MethodGet, "/api/agent/install-config/ait_short", nil, nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if recorder.Body.String() != "# Error: Invalid Agent install token" {
		t.Fatalf("body = %q", recorder.Body.String())
	}

	// The certificate is validated BEFORE the token is consumed: with an
	// unavailable certificate the JSON format answers 503, never 403.
	t.Setenv("PANEL_CRT_PATH", filepath.Join(t.TempDir(), "missing.crt"))
	recorder = agentDoRequest(t, router, http.MethodGet, "/api/agent/install-config/ait_short?format=json", nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (certificate checked first)", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["code"] != "PANEL_CERTIFICATE_UNAVAILABLE" {
		t.Fatalf("code = %v", body["code"])
	}

	// The query schema rejects any other format value without touching the token.
	recorder = agentDoRequest(t, router, http.MethodGet, "/api/agent/install-config/ait_short?format=yaml", nil, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["code"] != "VALIDATION_ERROR" {
		t.Fatalf("code = %v", body["code"])
	}
}

// ==================== heartbeat state machine ====================

type agentHeartbeatCapture struct {
	updates          []agentHeartbeatAgentUpdate
	storageReports   int
	instanceReports  int
	gpuSyncs         int
	onlineMarks      int
	installedMarks   []string
	certificateSets  int
	ipv6Values       []string
	successRecords   int
	caddyStates      []bool
	nudges           int
	networkStatuses  int
	completed        []string
	completeRequests []any
}

func agentFakeHeartbeatDeps(capture *agentHeartbeatCapture) *agentHeartbeatDeps {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	return &agentHeartbeatDeps{
		UpdateAgent: func(_ context.Context, _ string, update agentHeartbeatAgentUpdate) error {
			capture.updates = append(capture.updates, update)
			return nil
		},
		ProcessStorage: func(_ context.Context, _ int32, _ any, _ time.Time) (bool, error) {
			capture.storageReports++
			return true, nil
		},
		ProcessInstances: func(_ context.Context, _ int32, _ any) (services.AgentInstanceReportResult, error) {
			capture.instanceReports++
			return services.AgentInstanceReportResult{Received: 2, Matched: 2, StatusUpdated: 1, TrafficUpdated: 1, TrafficDeltaBytes: "1024"}, nil
		},
		SyncGPU: func(_ context.Context, _ int32, _ any) error {
			capture.gpuSyncs++
			return nil
		},
		HostConnection: func(_ context.Context, _ int32) (*agentHostConnection, error) {
			return &agentHostConnection{CaddyEnabled: false, TunnelEnabled: true, TargetHost: "10.0.0.5", TargetPort: 8444}, nil
		},
		MarkHostOnline: func(_ context.Context, _ int32) error {
			capture.onlineMarks++
			return nil
		},
		MarkHostInstalled: func(_ context.Context, _ int32, architecture string) error {
			capture.installedMarks = append(capture.installedMarks, architecture)
			return nil
		},
		SetHostCertificates: func(_ context.Context, _ int32) error {
			capture.certificateSets++
			return nil
		},
		SetHostIPv6: func(_ context.Context, _ int32, ipv6 string) error {
			capture.ipv6Values = append(capture.ipv6Values, ipv6)
			return nil
		},
		RecordHostSuccess: func(_ int32) {
			capture.successRecords++
		},
		SetCaddyEnabled: func(_ context.Context, _ int32, enabled bool) error {
			capture.caddyStates = append(capture.caddyStates, enabled)
			return nil
		},
		ListPendingCreates: func(_ context.Context, _ int32) ([]agentPendingCreateTask, error) {
			instanceID := int32(99)
			return []agentPendingCreateTask{{
				TaskID: 5, InstanceID: &instanceID, RetryCount: 1,
				CreatedAt: now.Format("2006-01-02T15:04:05.000Z"),
			}}, nil
		},
		NudgeWorker: func() { capture.nudges++ },
		RecordNetworkStatus: func(_ context.Context, _ int32, _ any) error {
			capture.networkStatuses++
			return nil
		},
		CompleteHeartbeat: func(_ int32, requestID any, message string) {
			capture.completed = append(capture.completed, message)
			capture.completeRequests = append(capture.completeRequests, requestID)
		},
		Now: func() time.Time { return now },
	}
}

func agentHeartbeatTestContext(t *testing.T, payload map[string]any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", strings.NewReader(string(encoded)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.RemoteAddr = "127.0.0.1:5555"
	return c, recorder
}

func agentResetCaddyState() {
	agentCaddyState.Lock()
	agentCaddyState.install = map[int32]bool{}
	agentCaddyState.uninstall = map[int32]bool{}
	agentCaddyState.Unlock()
}

func agentTestAgent() *models.HostAgent {
	return &models.HostAgent{
		ID:           7,
		HostID:       42,
		AgentID:      "agt_0123456789abcdefghijklmn",
		Enabled:      true,
		Status:       "online",
		Capabilities: datatypes.JSON(`["incus_node_v1"]`),
		LastReport:   datatypes.JSON(`{}`),
		CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestAgentHeartbeatRequiresNodeAPI(t *testing.T) {
	agentResetCaddyState()
	capture := &agentHeartbeatCapture{}
	c, recorder := agentHeartbeatTestContext(t, map[string]any{"heartbeatRequestId": "req-1"})
	agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: map[string]any{"heartbeatRequestId": "req-1"}}, agentFakeHeartbeatDeps(capture))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["code"] != "NODE_API_REQUIRED" {
		t.Fatalf("code = %v", body["code"])
	}
	if len(capture.completed) != 1 || capture.completed[0] != "本次节点上报未能保存，测试失败" {
		t.Fatalf("completed = %v", capture.completed)
	}
	if capture.completeRequests[0] != "req-1" {
		t.Fatalf("request id = %v", capture.completeRequests[0])
	}
}

func TestAgentHeartbeatLockedNodeStaysOnline(t *testing.T) {
	agentResetCaddyState()
	capture := &agentHeartbeatCapture{}
	payload := map[string]any{
		"nodeApi":      "incus_node_v1",
		"version":      "1.4.0",
		"instances":    map[string]any{"available": true, "items": []any{}},
		"storage":      map[string]any{"available": true, "collectionComplete": true, "reportedAt": "2026-11-11T12:00:00.000Z", "pools": []any{}},
		"nodeSecurity": map[string]any{"state": map[string]any{"locked": true}},
		"capabilities": []any{"incus_node_v1", "network-control"},
	}
	c, recorder := agentHeartbeatTestContext(t, payload)
	agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, agentFakeHeartbeatDeps(capture))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if capture.instanceReports != 0 {
		t.Fatal("locked node must not process the instance inventory")
	}
	if len(capture.updates) != 1 || capture.updates[0].IncludeLastReport {
		t.Fatalf("locked update = %+v (must not write lastReport)", capture.updates)
	}
	body := agentDecodeJSON(t, recorder)
	tunnel, ok := body["tunnel"].(map[string]any)
	if !ok || tunnel["enabled"] != true || tunnel["targetHost"] != "10.0.0.5" || tunnel["targetPort"] != float64(8444) {
		t.Fatalf("tunnel = %v", body["tunnel"])
	}
	if len(capture.completed) != 1 || capture.completed[0] != "" {
		t.Fatalf("completed = %v, want success", capture.completed)
	}
}

func TestAgentHeartbeatLockedNodeStorageFailure(t *testing.T) {
	agentResetCaddyState()
	capture := &agentHeartbeatCapture{}
	deps := agentFakeHeartbeatDeps(capture)
	deps.ProcessStorage = func(_ context.Context, _ int32, _ any, _ time.Time) (bool, error) {
		return false, nil
	}
	payload := map[string]any{
		"nodeApi":      "incus_node_v1",
		"nodeSecurity": map[string]any{"state": map[string]any{"locked": true}},
		"storage":      map[string]any{"available": true, "collectionComplete": true},
	}
	c, recorder := agentHeartbeatTestContext(t, payload)
	agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, deps)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if len(capture.completed) != 1 || capture.completed[0] != "节点已连接，但锁定存储状态采集或保存未完成，测试失败" {
		t.Fatalf("completed = %v", capture.completed)
	}
}

func agentFullHeartbeatPayload() map[string]any {
	return map[string]any{
		"nodeApi":      "incus_node_v1",
		"version":      "1.4.0",
		"capabilities": []any{"incus_node_v1", "network-control"},
		"runtime":      map[string]any{"goarch": "arm64"},
		"incus":        map[string]any{"available": true},
		"instances":    map[string]any{"available": true, "items": []any{}},
		"resources": map[string]any{
			"storageAvailable": true,
			"network": map[string]any{
				"cards": []any{
					map[string]any{"addresses": []any{
						map[string]any{"family": "inet6", "scope": "global", "address": "2001:db8::5"},
					}},
				},
			},
		},
		"metrics":       map[string]any{"heartbeatIntervalSeconds": float64(1)},
		"storage":       map[string]any{"available": true, "collectionComplete": true},
		"caddy":         map[string]any{"available": false},
		"networkStatus": map[string]any{"appliedRevision": float64(0), "state": "idle"},
	}
}

func TestAgentHeartbeatSuccessPath(t *testing.T) {
	agentResetCaddyState()
	capture := &agentHeartbeatCapture{}
	payload := agentFullHeartbeatPayload()
	c, recorder := agentHeartbeatTestContext(t, payload)
	agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, agentFakeHeartbeatDeps(capture))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(capture.completed) != 1 || capture.completed[0] != "" {
		t.Fatalf("completed = %v, want success", capture.completed)
	}
	if capture.instanceReports != 1 || capture.storageReports != 1 || capture.gpuSyncs != 1 {
		t.Fatalf("reports = instances %d storage %d gpu %d", capture.instanceReports, capture.storageReports, capture.gpuSyncs)
	}
	if capture.onlineMarks != 1 || capture.successRecords != 1 || capture.certificateSets != 1 {
		t.Fatalf("host marks = online %d success %d certs %d", capture.onlineMarks, capture.successRecords, capture.certificateSets)
	}
	if len(capture.installedMarks) != 1 || capture.installedMarks[0] != "aarch64" {
		t.Fatalf("architecture = %v, want aarch64 from runtime.goarch=arm64", capture.installedMarks)
	}
	if len(capture.ipv6Values) != 1 || capture.ipv6Values[0] != "2001:db8::5" {
		t.Fatalf("ipv6 = %v", capture.ipv6Values)
	}
	if capture.networkStatuses != 1 {
		t.Fatalf("network status records = %d", capture.networkStatuses)
	}
	if capture.nudges != 1 {
		t.Fatalf("nudges = %d, want 1 when the create queue is non-empty", capture.nudges)
	}

	// The stored lastReport keeps the raw sections, clamps the interval to 5s and
	// records instances.available.
	if len(capture.updates) != 1 || !capture.updates[0].IncludeLastReport {
		t.Fatalf("updates = %+v", capture.updates)
	}
	report := capture.updates[0].LastReport
	metrics, ok := report["metrics"].(map[string]any)
	if !ok || metrics["heartbeatIntervalSeconds"] != 5 {
		t.Fatalf("metrics = %v, want heartbeatIntervalSeconds clamped to 5", report["metrics"])
	}
	instances, ok := report["instances"].(map[string]any)
	if !ok || instances["available"] != true {
		t.Fatalf("instances = %v", report["instances"])
	}
	if runtime, ok := report["runtime"].(map[string]any); !ok || runtime["goarch"] != "arm64" {
		t.Fatalf("runtime = %v", report["runtime"])
	}

	body := agentDecodeJSON(t, recorder)
	if body["ok"] != true || body["taskPollIntervalSeconds"] != float64(15) {
		t.Fatalf("response = %v", body)
	}
	if reportValue, ok := body["instanceReport"].(map[string]any); !ok || reportValue["received"] != float64(2) {
		t.Fatalf("instanceReport = %v", body["instanceReport"])
	}
	tasks, ok := body["pendingCreateTasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("pendingCreateTasks = %v", body["pendingCreateTasks"])
	}
	task := tasks[0].(map[string]any)
	if task["taskId"] != float64(5) || task["instanceId"] != float64(99) || task["retryCount"] != float64(1) {
		t.Fatalf("task = %v", task)
	}
	caddy, ok := body["caddy"].(map[string]any)
	if !ok || caddy["command"] != "idle" || caddy["port"] != float64(0) {
		t.Fatalf("caddy = %v", body["caddy"])
	}
	tunnel, ok := body["tunnel"].(map[string]any)
	if !ok || tunnel["enabled"] != true || tunnel["targetHost"] != "10.0.0.5" || tunnel["targetPort"] != float64(8444) {
		t.Fatalf("tunnel = %v", body["tunnel"])
	}
}

func TestAgentHeartbeatPreservesStorageOnIncompleteCollection(t *testing.T) {
	agentResetCaddyState()
	capture := &agentHeartbeatCapture{}
	payload := agentFullHeartbeatPayload()
	delete(payload, "storage")
	resources := payload["resources"].(map[string]any)
	resources["storageAvailable"] = false
	delete(resources, "network")

	agent := agentTestAgent()
	agent.LastReport = datatypes.JSON(`{"resources":{"storage":{"pools":[{"name":"tank"}]}},"metrics":{"heartbeatIntervalSeconds":30}}`)
	// make the stale metrics visible in the report we assert below
	c, recorder := agentHeartbeatTestContext(t, payload)
	agentHeartbeatStateMachine(c, agent, agentHeartbeatBody{Raw: payload}, agentFakeHeartbeatDeps(capture))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	report := capture.updates[0].LastReport
	reportResources, ok := report["resources"].(map[string]any)
	if !ok {
		t.Fatalf("resources = %v", report["resources"])
	}
	storage, ok := reportResources["storage"].(map[string]any)
	if !ok {
		t.Fatalf("storage snapshot not preserved: %v", reportResources)
	}
	pools, ok := storage["pools"].([]any)
	if !ok || len(pools) != 1 {
		t.Fatalf("preserved storage = %v", storage)
	}
	// A missing storage report fails the storage gate first (the TypeScript's
	// `request.body.storage?.available !== true` also wins over the hardware gate).
	if len(capture.completed) != 1 || capture.completed[0] != "节点存储状态采集或保存失败，测试失败" {
		t.Fatalf("completed = %v", capture.completed)
	}
}

func TestAgentHeartbeatReportErrorPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(payload map[string]any, deps *agentHeartbeatDeps)
		message string
	}{
		{
			name: "incus unavailable wins",
			mutate: func(payload map[string]any, _ *agentHeartbeatDeps) {
				payload["incus"] = map[string]any{"available": false}
			},
			message: "节点已连接，但 Incus 状态采集失败，测试失败",
		},
		{
			name: "storage rejection",
			mutate: func(_ map[string]any, deps *agentHeartbeatDeps) {
				deps.ProcessStorage = func(_ context.Context, _ int32, _ any, _ time.Time) (bool, error) {
					return false, nil
				}
			},
			message: "节点存储状态采集或保存失败，测试失败",
		},
		{
			name: "instance report failure",
			mutate: func(_ map[string]any, deps *agentHeartbeatDeps) {
				deps.ProcessInstances = func(_ context.Context, _ int32, _ any) (services.AgentInstanceReportResult, error) {
					return services.AgentInstanceReportResult{}, errors.New("boom")
				}
			},
			message: "节点实例状态采集或保存失败，测试失败",
		},
		{
			name: "instance report errors counted",
			mutate: func(_ map[string]any, deps *agentHeartbeatDeps) {
				deps.ProcessInstances = func(_ context.Context, _ int32, _ any) (services.AgentInstanceReportResult, error) {
					return services.AgentInstanceReportResult{Received: 1, Errors: 1}, nil
				}
			},
			message: "节点实例状态采集或保存失败，测试失败",
		},
		{
			name: "hardware storage flag missing",
			mutate: func(payload map[string]any, _ *agentHeartbeatDeps) {
				payload["resources"].(map[string]any)["storageAvailable"] = false
			},
			message: "节点硬件状态采集或保存失败，测试失败",
		},
		{
			name: "gpu sync failure",
			mutate: func(_ map[string]any, deps *agentHeartbeatDeps) {
				deps.SyncGPU = func(_ context.Context, _ int32, _ any) error { return errors.New("gpu") }
			},
			message: "节点硬件状态采集或保存失败，测试失败",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			agentResetCaddyState()
			capture := &agentHeartbeatCapture{}
			deps := agentFakeHeartbeatDeps(capture)
			payload := agentFullHeartbeatPayload()
			testCase.mutate(payload, deps)
			c, recorder := agentHeartbeatTestContext(t, payload)
			agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, deps)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			if len(capture.completed) != 1 || capture.completed[0] != testCase.message {
				t.Fatalf("completed = %v, want %q", capture.completed, testCase.message)
			}
		})
	}
}

func TestAgentHeartbeatCaddyCommands(t *testing.T) {
	hostID := int32(42)

	t.Run("install requested", func(t *testing.T) {
		agentResetCaddyState()
		agentCaddyRequestInstall(hostID)
		capture := &agentHeartbeatCapture{}
		payload := agentFullHeartbeatPayload()
		payload["caddy"] = map[string]any{"available": false, "port": float64(3080)}
		c, recorder := agentHeartbeatTestContext(t, payload)
		agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, agentFakeHeartbeatDeps(capture))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		body := agentDecodeJSON(t, recorder)
		caddy := body["caddy"].(map[string]any)
		if caddy["command"] != "install" || caddy["port"] != float64(3080) {
			t.Fatalf("caddy = %v", caddy)
		}
		if !agentCaddyHas(agentCaddyState.install, hostID) {
			t.Fatal("install request must survive until the agent reports Caddy available")
		}
	})

	t.Run("caddy reported available auto-enables", func(t *testing.T) {
		agentResetCaddyState()
		agentCaddyRequestInstall(hostID)
		capture := &agentHeartbeatCapture{}
		payload := agentFullHeartbeatPayload()
		payload["caddy"] = map[string]any{"available": true, "port": float64(3080)}
		c, recorder := agentHeartbeatTestContext(t, payload)
		agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, agentFakeHeartbeatDeps(capture))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if len(capture.caddyStates) != 1 || capture.caddyStates[0] != true {
			t.Fatalf("caddy states = %v, want [true]", capture.caddyStates)
		}
		if agentCaddyHas(agentCaddyState.install, hostID) {
			t.Fatal("install request must be cleared once Caddy is available")
		}
		caddy := agentDecodeJSON(t, recorder)["caddy"].(map[string]any)
		if caddy["command"] != "idle" || caddy["port"] != float64(0) {
			t.Fatalf("caddy = %v", caddy)
		}
	})

	t.Run("uninstall requested", func(t *testing.T) {
		agentResetCaddyState()
		agentCaddyRequestUninstall(hostID)
		capture := &agentHeartbeatCapture{}
		deps := agentFakeHeartbeatDeps(capture)
		deps.HostConnection = func(_ context.Context, _ int32) (*agentHostConnection, error) {
			return &agentHostConnection{CaddyEnabled: true, TunnelEnabled: false}, nil
		}
		payload := agentFullHeartbeatPayload()
		payload["caddy"] = map[string]any{"available": false}
		c, recorder := agentHeartbeatTestContext(t, payload)
		agentHeartbeatStateMachine(c, agentTestAgent(), agentHeartbeatBody{Raw: payload}, deps)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		caddy := agentDecodeJSON(t, recorder)["caddy"].(map[string]any)
		if caddy["command"] != "uninstall" {
			t.Fatalf("caddy = %v", caddy)
		}
		if len(capture.caddyStates) != 1 || capture.caddyStates[0] != false {
			t.Fatalf("caddy states = %v, want [false]", capture.caddyStates)
		}
		if agentCaddyHas(agentCaddyState.uninstall, hostID) || agentCaddyHas(agentCaddyState.install, hostID) {
			t.Fatal("uninstall request must be cleared")
		}
	})
}

// ==================== serialisation / pure helpers ====================

func TestAgentDeriveStatus(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-10 * time.Second)
	stale := now.Add(-3 * time.Minute)
	veryStale := now.Add(-2 * time.Hour)

	agent := agentTestAgent()
	agent.Status = "online"
	agent.LastSeenAt = &recent
	if got := agentDeriveStatus(agent, now); got != "online" {
		t.Fatalf("recent agent status = %s", got)
	}
	agent.LastSeenAt = &stale
	if got := agentDeriveStatus(agent, now); got != "offline" {
		t.Fatalf("stale agent status = %s (threshold 120s)", got)
	}

	// A longer heartbeat interval raises the offline threshold: 3600s interval →
	// 10800s threshold, so a two-hour-old report is still online.
	agent.LastSeenAt = &veryStale
	agent.LastReport = datatypes.JSON(`{"metrics":{"heartbeatIntervalSeconds":3600}}`)
	if got := agentDeriveStatus(agent, now); got != "online" {
		t.Fatalf("long-interval status = %s", got)
	}

	agent.Status = "maintenance"
	agent.LastSeenAt = nil
	if got := agentDeriveStatus(agent, now); got != "maintenance" {
		t.Fatalf("non-online status = %s", got)
	}
	agent.Status = "online"
	if got := agentDeriveStatus(agent, now); got != "offline" {
		t.Fatalf("missing lastSeenAt status = %s", got)
	}
	agent.LastSeenAt = &recent
	agent.Enabled = false
	if got := agentDeriveStatus(agent, now); got != "online" {
		t.Fatalf("disabled agent status = %s (stored status is returned as-is)", got)
	}
}

func TestAgentSerializeShape(t *testing.T) {
	agent := agentTestAgent()
	agent.Version = nil
	agent.Capabilities = datatypes.JSON(`null`)
	agent.LastReport = datatypes.JSON(`null`)
	agent.LastSeenAt = nil

	serialized := agentSerialize(agent)
	if capabilities, ok := serialized["capabilities"].([]any); !ok || len(capabilities) != 0 {
		t.Fatalf("capabilities = %#v, want []", serialized["capabilities"])
	}
	if report, ok := serialized["lastReport"].(map[string]any); !ok || len(report) != 0 {
		t.Fatalf("lastReport = %#v, want {}", serialized["lastReport"])
	}
	if serialized["lastSeenAt"] != nil {
		t.Fatalf("lastSeenAt = %v, want null", serialized["lastSeenAt"])
	}
	if serialized["versionStatus"] != "unknown" {
		t.Fatalf("versionStatus = %v", serialized["versionStatus"])
	}
	if serialized["createdAt"] != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("createdAt = %v", serialized["createdAt"])
	}

	agent.Capabilities = datatypes.JSON(`["incus_node_v1","network-control"]`)
	agent.LastReport = datatypes.JSON(`{"metrics":{"heartbeatIntervalSeconds":30}}`)
	seen := time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)
	agent.LastSeenAt = &seen
	serialized = agentSerialize(agent)
	if serialized["versionStatus"] != "managed" {
		t.Fatalf("versionStatus = %v", serialized["versionStatus"])
	}
	if serialized["lastSeenAt"] != "2026-11-11T11:00:00.000Z" {
		t.Fatalf("lastSeenAt = %v", serialized["lastSeenAt"])
	}
}

func TestAgentParsePositiveID(t *testing.T) {
	cases := []struct {
		value string
		want  int32
		ok    bool
	}{
		{"1", 1, true},
		{"990001", 990001, true},
		{"2147483647", 2147483647, true},
		{"0", 0, false},
		{"01", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"1.5", 0, false},
		{" 1", 0, false},
		{"2147483648", 0, false},
		{"9007199254740993", 0, false},
	}
	for _, testCase := range cases {
		got, ok := agentParsePositiveID(testCase.value)
		if ok != testCase.ok || got != testCase.want {
			t.Errorf("agentParsePositiveID(%q) = (%d,%v), want (%d,%v)", testCase.value, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestAgentClampHeartbeatInterval(t *testing.T) {
	cases := []struct {
		value any
		want  int
	}{
		{nil, 5},
		{float64(0), 5},
		{float64(1), 5},
		{float64(30), 30},
		{"30", 30},
		{"abc", 5},
		{float64(999999), 3600},
		{1.5, 5},
	}
	for _, testCase := range cases {
		if got := agentClampHeartbeatInterval(testCase.value); got != testCase.want {
			t.Errorf("clamp(%v) = %d, want %d", testCase.value, got, testCase.want)
		}
	}
}

func TestAgentNormalizeCapabilities(t *testing.T) {
	got := agentNormalizeCapabilities([]any{"a", " a ", "b", float64(1), nil})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("capabilities = %v", got)
	}
	if got := agentNormalizeCapabilities("nope"); len(got) != 0 {
		t.Fatalf("non-array capabilities = %v", got)
	}
	many := make([]any, 0, 100)
	for index := 0; index < 100; index++ {
		many = append(many, fmt.Sprintf("cap-%d", index))
	}
	if got := agentNormalizeCapabilities(many); len(got) != 64 {
		t.Fatalf("capability cap = %d, want 64", len(got))
	}
	long := strings.Repeat("x", 100)
	got = agentNormalizeCapabilities([]any{long})
	if len(got) != 1 || len(got[0]) != 80 {
		t.Fatalf("capability truncation = %d chars", len(got[0]))
	}
}

func TestAgentNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://panel.example.com", "https://panel.example.com", true},
		{"https://panel.example.com:443/agent", "https://panel.example.com", true},
		{"http://panel.example.com:80/x", "http://panel.example.com", true},
		{"HTTPS://Panel.Example.COM/", "https://panel.example.com", true},
		{"ftp://panel.example.com", "", false},
		{"panel.example.com", "", false},
		{"", "", false},
		{"https://" + strings.Repeat("a", 2100) + ".com", "", false},
	}
	for _, testCase := range cases {
		got := agentNormalizeBaseURL(&testCase.in)
		if testCase.ok {
			if got == nil || *got != testCase.want {
				t.Errorf("normalizeBaseUrl(%q) = %v, want %q", testCase.in, got, testCase.want)
			}
		} else if got != nil {
			t.Errorf("normalizeBaseUrl(%q) = %q, want nil", testCase.in, *got)
		}
	}
}

func TestAgentDerivePanelURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(headers map[string]string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/agent/hosts/1/install-command", strings.NewReader("{}"))
		for name, value := range headers {
			c.Request.Header.Set(name, value)
		}
		return c
	}

	explicit := "https://explicit.example.com"
	if got := agentDerivePanelURL(newContext(nil), &explicit); got != explicit {
		t.Fatalf("explicit base url = %q", got)
	}
	if got := agentDerivePanelURL(newContext(map[string]string{"Origin": "https://origin.example.com"}), nil); got != "https://origin.example.com" {
		t.Fatalf("origin = %q", got)
	}
	if got := agentDerivePanelURL(newContext(map[string]string{"Referer": "https://referer.example.com/x"}), nil); got != "https://referer.example.com" {
		t.Fatalf("referer = %q", got)
	}
	if got := agentDerivePanelURL(newContext(map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "forwarded.example.com",
	}), nil); got != "https://forwarded.example.com" {
		t.Fatalf("forwarded = %q", got)
	}
	if got := agentDerivePanelURL(newContext(map[string]string{"Host": "plain.example.com"}), nil); got != "http://plain.example.com" {
		t.Fatalf("host fallback = %q", got)
	}
}

func TestAgentBuildNodeInstallCommand(t *testing.T) {
	channel := "latest"
	got := agentBuildNodeInstallCommand("https://panel.example.com/", "ait_token'quote", &channel)
	want := "curl -fsSL 'https://panel.example.com/api/agent/install.sh' -o incus-node-install.sh && " +
		"sudo env INCUS_PANEL_URL='https://panel.example.com' " +
		"INCUS_AGENT_INSTALL_TOKEN='ait_token'\\''quote' INCUS_NODE_CHANNEL='latest' bash incus-node-install.sh"
	if got != want {
		t.Fatalf("install command =\n%s\nwant\n%s", got, want)
	}
	defaultCommand := agentBuildNodeInstallCommand("https://panel.example.com", "tok", nil)
	if !strings.Contains(defaultCommand, "INCUS_NODE_CHANNEL='canary'") {
		t.Fatalf("default channel missing: %s", defaultCommand)
	}
}

func TestAgentBuildInstallConfig(t *testing.T) {
	got := agentBuildAgentInstallConfig("agt_x", "ias_y'z")
	want := "INCUS_AGENT_ID='agt_x'\nINCUS_AGENT_SECRET='ias_y'\\''z'\n"
	if got != want {
		t.Fatalf("install config = %q, want %q", got, want)
	}
}

func TestAgentExtractHostPublicIPv6(t *testing.T) {
	resources := map[string]any{
		"network": map[string]any{
			"cards": []any{
				map[string]any{"addresses": []any{
					map[string]any{"family": "inet6", "scope": "link", "address": "fe80::1"},
					map[string]any{"family": "inet6", "scope": "global", "address": "fd00::1"},
				}},
				map[string]any{"ports": []any{
					map[string]any{"addresses": []any{
						map[string]any{"family": "inet6", "scope": "global", "address": "2001:db8::7"},
					}},
				}},
			},
		},
	}
	if got := agentExtractHostPublicIPv6(resources); got == nil || *got != "2001:db8::7" {
		t.Fatalf("ipv6 = %v", got)
	}
	if got := agentExtractHostPublicIPv6(map[string]any{}); got != nil {
		t.Fatalf("empty resources ipv6 = %v", got)
	}
	if got := agentExtractHostPublicIPv6("nope"); got != nil {
		t.Fatalf("scalar resources ipv6 = %v", got)
	}
}

func TestAgentBuildHeartbeatReportNormalizesInstances(t *testing.T) {
	body := &agentHeartbeatBody{Raw: map[string]any{
		"instances": map[string]any{"available": "yes"},
		"metrics":   map[string]any{"heartbeatIntervalSeconds": float64(600), "cpu": float64(3)},
		"resources": map[string]any{"storageAvailable": false},
	}}
	report := agentBuildHeartbeatReport(body, map[string]any{
		"resources": map[string]any{"storage": map[string]any{"pools": []any{}}},
	})
	instances := report["instances"].(map[string]any)
	if instances["available"] != false {
		t.Fatalf("instances.available = %v", instances["available"])
	}
	metrics := report["metrics"].(map[string]any)
	if metrics["heartbeatIntervalSeconds"] != 600 || metrics["cpu"] != float64(3) {
		t.Fatalf("metrics = %v", metrics)
	}
	resources := report["resources"].(map[string]any)
	if _, present := resources["storage"]; !present {
		t.Fatalf("previous storage not preserved: %v", resources)
	}
	if _, ok := report["caddy"].(map[string]any); !ok {
		t.Fatalf("caddy = %v, want {}", report["caddy"])
	}
}

func TestAgentBodyParsingRulesApplyBeforeHMAC(t *testing.T) {
	router := agentTestRouter()
	// Fastify parses the body before the handler: an empty JSON body is 400, an
	// unsupported media type is 415, both before any HMAC check.
	request := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", strings.NewReader(""))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Length", "0")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty JSON body status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", strings.NewReader("<xml/>"))
	request.Header.Set("Content-Type", "application/xml")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("xml body status = %d, want 415", recorder.Code)
	}
}

// TestAgentSecurityReportBodyBinding proves /security-report rejects a signature
// that was produced for a different body before running any handler logic.
func TestAgentHeartbeatSchemaValidationPrecedesHMAC(t *testing.T) {
	router := agentTestRouter()

	// A declared object property with a non-object value is a schema error, and
	// Fastify reports it before the handler (and therefore before HMAC).
	recorder := agentDoRequest(t, router, http.MethodPost, "/api/agent/heartbeat",
		[]byte(`{"instances":"not-an-object"}`), nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["code"] != "VALIDATION_ERROR" || body["error"] != "must be object" {
		t.Fatalf("body = %v", body)
	}

	// maxLength is reported with the TypeScript's field prefix.
	longVersion, err := json.Marshal(map[string]any{"version": strings.Repeat("v", 129)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/heartbeat", longVersion, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["error"] != "version is too long" {
		t.Fatalf("error = %v", body["error"])
	}

	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/heartbeat",
		[]byte(`{"capabilities":`+strings.Repeat("[", 65)), nil)
	// malformed JSON still answers 400 from the body parser
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d", recorder.Code)
	}

	// A valid schema with a scalar capability is coerced to an array and then
	// reaches the HMAC check (401), not a 400.
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/heartbeat",
		[]byte(`{"capabilities":"incus_node_v1"}`), nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (schema accepted, HMAC missing)", recorder.Code)
	}
}

func TestAgentOptionalBooleanSchemaCoercion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validate := func(object map[string]any) (map[string]any, bool, int) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		filtered, ok := agentValidateOptionalBoolean(c, object, "baseUrl", "channel")
		return filtered, ok, recorder.Code
	}

	filtered, ok, _ := validate(map[string]any{"unexpected": true, "enabled": true})
	if !ok {
		t.Fatal("unknown field must be dropped, not rejected")
	}
	if _, present := filtered["unexpected"]; present {
		t.Fatal("unknown field survived removeAdditional")
	}
	if filtered["enabled"] != true {
		t.Fatalf("enabled = %v", filtered["enabled"])
	}

	if filtered, ok, _ := validate(map[string]any{"enabled": "false"}); !ok || filtered["enabled"] != false {
		t.Fatalf("string boolean coercion = %v (ok=%v)", filtered["enabled"], ok)
	}
	if filtered, ok, _ := validate(map[string]any{"enabled": float64(0)}); !ok || filtered["enabled"] != false {
		t.Fatalf("numeric boolean coercion = %v (ok=%v)", filtered["enabled"], ok)
	}
	if _, ok, status := validate(map[string]any{"enabled": "yes"}); ok || status != http.StatusBadRequest {
		t.Fatalf("invalid boolean accepted (status %d)", status)
	}
}

func TestAgentRouteBodyLimits(t *testing.T) {
	router := agentTestRouter()
	largeReport := []byte(`{"padding":"` + strings.Repeat("x", agentSecurityReportLimit) + `"}`)
	recorder := agentDoRequest(t, router, http.MethodPost, "/api/agent/security-report", largeReport, nil)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("security-report status = %d, want 413", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["error"] != "Request body is too large" {
		t.Fatalf("error = %v", body["error"])
	}

	largeStartup := []byte(`{"padding":"` + strings.Repeat("y", agentStartupKeysBodyLimit) + `"}`)
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/startup-keys", largeStartup, nil)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("startup-keys status = %d, want 413", recorder.Code)
	}

	// Exactly at the limit is allowed through to the HMAC check.
	atLimit := []byte(`{"p":"` + strings.Repeat("z", agentStartupKeysBodyLimit-len(`{"p":""}`)) + `"}`)
	recorder = agentDoRequest(t, router, http.MethodPost, "/api/agent/startup-keys", atLimit, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("at-limit status = %d, want 401", recorder.Code)
	}
}

func TestAgentSecurityReportBodyBinding(t *testing.T) {
	router := agentTestRouter()
	signedBody := []byte(`{"state":{"enabled":true,"locked":false,"revision":1},"events":[]}`)
	headers := agentSignedHeaders(t, lib.GenerateAgentSecret(), lib.GenerateAgentID(),
		http.MethodPost, "/api/agent/security-report", signedBody, "nonce-report001",
		fmt.Sprintf("%d", time.Now().UTC().UnixMilli()))
	recorder := agentDoRequest(t, router, http.MethodPost, "/api/agent/security-report", []byte(`{}`), headers)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if body := agentDecodeJSON(t, recorder); body["error"] != "Report unavailable" {
		t.Fatalf("error = %v", body["error"])
	}
}

func TestAgentCaddyRequestSeams(t *testing.T) {
	agentResetCaddyState()
	defer agentResetCaddyState()
	if err := HostsCaddyInstallRequested(123); err != nil {
		t.Fatalf("install seam: %v", err)
	}
	if err := HostsCaddyUninstallRequested(123); err != nil {
		t.Fatalf("uninstall seam: %v", err)
	}
	if !agentCaddyHas(agentCaddyState.install, 123) || !agentCaddyHas(agentCaddyState.uninstall, 123) {
		t.Fatal("caddy seams did not record the requests")
	}
}
