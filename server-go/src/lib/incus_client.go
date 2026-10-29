// Port of server/src/lib/incus/incus-client.ts and the parts of
// incus-pool.ts / certificate-paths.ts / incus-utils.ts needed to talk to a node.
//
// This is the substrate six Phase-5 modules share (checkin, ip-addresses,
// snapshots, resource-pool, batch-config, transfers). It is deliberately kept
// minimal and boring: mTLS request, async-operation polling, health bookkeeping.
//
// The pinned-fingerprint transport (lib/node-security/transport.ts) and the
// tunnel branch (hostTunnelManager) are NOT ported here: they belong to the
// node-security and WebSocket waves. A host that requires either is rejected
// with an explicit error instead of silently connecting without the pin, because
// falling back would downgrade a security control.
package lib

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// IncusClientOptions mirrors the TypeScript IncusClientOptions.
type IncusClientOptions struct {
	URL               string
	CertPath          *string
	KeyPath           *string
	TunnelEnabled     bool
	HostID            int64
	TargetHost        string
	TargetPort        int
	ServerFingerprint string
}

// IncusAPIType mirrors the `type` discriminator on an Incus API envelope.
type IncusAPIType string

const (
	IncusAPITypeSync  IncusAPIType = "sync"
	IncusAPITypeAsync IncusAPIType = "async"
	IncusAPITypeError IncusAPIType = "error"
)

// IncusAPIResponse mirrors IncusApiResponse<T>.
type IncusAPIResponse struct {
	Type       IncusAPIType    `json:"type"`
	Status     string          `json:"status,omitempty"`
	StatusCode int             `json:"status_code,omitempty"`
	Operation  string          `json:"operation,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Error      string          `json:"error,omitempty"`
	ErrorCode  int             `json:"error_code,omitempty"`
	Err        string          `json:"err,omitempty"`
}

// IncusOperation mirrors IncusOperation.
type IncusOperation struct {
	ID          string          `json:"id"`
	Class       string          `json:"class"`
	Description string          `json:"description,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Status      string          `json:"status"`
	StatusCode  int             `json:"status_code"`
	MayCancel   bool            `json:"may_cancel,omitempty"`
	Location    string          `json:"location,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Err         string          `json:"err,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// IncusClient mirrors the TypeScript class. It is safe for concurrent use.
type IncusClient struct {
	baseURL   string
	certPath  string
	keyPath   string
	options   IncusClientOptions
	http      *http.Client
	connected bool
	mu        sync.RWMutex
}

// Default operation timeouts, mirroring the TS constructor / request defaults.
const (
	incusDefaultHeadersTimeout = 120 * time.Second
	incusDefaultBodyTimeout    = 300 * time.Second
	incusDefaultOperationWait  = 30 * time.Minute
	incusConnectionExpire      = 30 * time.Minute
	incusPoolCleanupInterval   = 5 * time.Minute
)

// NormalizeIncusURL mirrors the private static normalizeUrl: a bare IPv6 address
// without brackets is repaired so url.Parse accepts it.
//
//	https://2a06:de04:10:1250:::8443 → https://[2a06:de04:10:1250::]:8443
func NormalizeIncusURL(raw string) string {
	if _, err := url.Parse(raw); err == nil {
		return raw
	}

	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	scheme := raw[:schemeEnd+3]
	rest := raw[schemeEnd+3:]

	lastColon := strings.LastIndex(rest, ":")
	if lastColon < 0 {
		return raw
	}
	afterLastColon := rest[lastColon+1:]
	if afterLastColon == "" || !isAllDigits(afterLastColon) {
		return raw
	}
	addr := rest[:lastColon]
	if !strings.Contains(addr, ":") {
		return raw
	}
	return scheme + "[" + addr + "]:" + afterLastColon
}

func isAllDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}

// ResolveCertificatePath mirrors resolveCertificatePath: an existing path wins,
// otherwise fall back to the stable install-directory location.
func ResolveCertificatePath(certPath *string) *string {
	if certPath == nil || *certPath == "" {
		return nil
	}
	if fileExists(*certPath) {
		return certPath
	}
	if stable := stableCertificatePath(*certPath); stable != nil {
		return stable
	}
	return certPath
}

// ResolveCertificatePair mirrors resolveCertificatePair.
func ResolveCertificatePair(certPath, keyPath *string) (*string, *string) {
	return ResolveCertificatePath(certPath), ResolveCertificatePath(keyPath)
}

// stableCertificatePath mirrors stableCertificatePath: PANEL_CRT_PATH /
// PANEL_KEY_PATH first, then INCUDAL_INSTALL_DIR/server/certs, then the default
// install root.
func stableCertificatePath(filePath string) *string {
	fileName := filepath.Base(filePath)
	installDir := os.Getenv("INCUDAL_INSTALL_DIR")
	if installDir == "" {
		installDir = "/opt/incudal"
	}

	explicit := ""
	if fileName == "client.crt" {
		explicit = os.Getenv("PANEL_CRT_PATH")
	} else {
		explicit = os.Getenv("PANEL_KEY_PATH")
	}

	candidates := make([]string, 0, 3)
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	candidates = append(candidates,
		filepath.Join(installDir, "server", "certs", fileName),
		filepath.Join("/opt/incudal", "server", "certs", fileName),
	)

	for _, candidate := range candidates {
		if fileExists(candidate) {
			value := candidate
			return &value
		}
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// NewIncusClient mirrors `new IncusClient(options)`. It does not connect; call
// Connect before Request.
func NewIncusClient(options IncusClientOptions) *IncusClient {
	certPath, keyPath := ResolveCertificatePair(options.CertPath, options.KeyPath)
	client := &IncusClient{
		baseURL: NormalizeIncusURL(options.URL),
		options: options,
	}
	if certPath != nil {
		client.certPath = *certPath
	}
	if keyPath != nil {
		client.keyPath = *keyPath
	}
	if options.TargetHost == "" {
		client.options.TargetHost = "127.0.0.1"
	}
	if options.TargetPort == 0 {
		client.options.TargetPort = 8443
	}
	return client
}

// Connect mirrors connect(): load the mTLS pair, build the transport and verify
// the node answers by fetching /1.0.
func (c *IncusClient) Connect(ctx context.Context) (json.RawMessage, error) {
	transport, err := c.buildTransport()
	if err != nil {
		return nil, c.wrapConnectError(err)
	}

	c.mu.Lock()
	c.http = &http.Client{
		Transport: transport,
		Timeout:   incusDefaultHeadersTimeout + incusDefaultBodyTimeout,
	}
	c.connected = true
	c.mu.Unlock()

	info, err := c.serverInfo(ctx)
	if err != nil {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		return nil, c.wrapConnectError(err)
	}
	return info, nil
}

// wrapConnectError mirrors the `无法连接到 Incus: ${message}` wrapper.
func (c *IncusClient) wrapConnectError(err error) error {
	message := err.Error()
	if aborted, ok := IsNodeOperationAborted(err); ok {
		message = aborted.Message
	}
	return fmt.Errorf("无法连接到 Incus: %s", message)
}

// buildTransport mirrors the three branches of connect() minus the tunnel one.
func (c *IncusClient) buildTransport() (*http.Transport, error) {
	if c.certPath == "" || c.keyPath == "" {
		return nil, errors.New("Certificate or key path is missing")
	}

	// A pinned fingerprint is a security control: refuse rather than connect
	// without verifying it. /lib/node-security/transport.ts owns that path and is
	// part of the node-security wave.
	if c.options.ServerFingerprint != "" {
		return nil, errors.New("节点身份固定校验（serverFingerprint）尚未移植")
	}
	// The tunnel branch depends on hostTunnelManager (WebSocket wave).
	if c.options.TunnelEnabled && c.options.HostID != 0 {
		return nil, errors.New("宿主机内网穿透通道未移植，请等待节点隧道模块")
	}

	certificate, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		return nil, err
	}

	// Incus uses a self-signed certificate, so verification is disabled —
	// matching `rejectUnauthorized: false` in the TypeScript.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{certificate},
			InsecureSkipVerify: true,
			// The node's identity is established by the client certificate, not by
			// validating its self-signed chain. Verification is either a pinned
			// fingerprint (not ported) or absent, exactly as in Node.
			MinVersion: tls.VersionTLS12,
		},
		DisableKeepAlives:   false,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	return transport, nil
}

// Request mirrors `request(method, path, body, timeout, signal, errorBody)`.
//
// body is marshalled as JSON when non-nil. errorBody is the original request used
// for redaction; pass the same value as body unless the caller rewrote it.
func (c *IncusClient) Request(ctx context.Context, method, path string, body any, timeout time.Duration, errorBody any) (json.RawMessage, error) {
	c.mu.RLock()
	httpClient := c.http
	c.mu.RUnlock()
	if httpClient == nil {
		return nil, errors.New("Incus 客户端未连接")
	}
	if timeout <= 0 {
		timeout = incusDefaultOperationWait
	}
	if errorBody == nil {
		errorBody = body
	}

	requestURL := c.baseURL + path
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, NewIncusAPIError(err.Error(), 502, errorBody)
		}
		reader = strings.NewReader(string(encoded))
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, NewIncusAPIError(err.Error(), 502, errorBody)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := httpClient.Do(request)
	if err != nil {
		// A cancelled context surfaces as an AbortError in Node.
		if ctx.Err() != nil {
			return nil, &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
		}
		// Node's undici raises HeadersTimeoutError / BodyTimeoutError; both map to
		// the TimeoutError kind downstream.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &NodeOperationAborted{Kind: "TimeoutError", Message: "操作超时"}
		}
		return nil, NewIncusAPIError(err.Error(), 502, errorBody)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, NewIncusAPIError(err.Error(), 502, errorBody)
	}

	var envelope IncusAPIResponse
	if err := json.Unmarshal(payload, &envelope); err != nil {
		text := strings.TrimSpace(string(payload))
		if text == "" {
			text = fmt.Sprintf("节点 API 返回了非 JSON 响应（HTTP %d）", response.StatusCode)
		}
		status := response.StatusCode
		if status < 400 {
			status = 502
		}
		return nil, NewIncusAPIError(text, status, errorBody)
	}

	if response.StatusCode >= 400 || envelope.Type == IncusAPITypeError {
		message := envelope.Error
		if message == "" {
			message = fmt.Sprintf("Incus API HTTP %d", response.StatusCode)
		}
		status := response.StatusCode
		if status < 400 {
			status = envelope.ErrorCode
		}
		return nil, NewIncusAPIError(message, status, errorBody)
	}

	if envelope.Type == IncusAPITypeAsync && envelope.Operation != "" {
		return c.WaitForOperation(ctx, envelope.Operation, timeout, errorBody)
	}

	if len(envelope.Metadata) > 0 {
		return envelope.Metadata, nil
	}
	// `(data.metadata || data)`: when metadata is absent the whole envelope is the
	// result. Re-marshal so callers see a single consistent shape.
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, NewIncusAPIError("节点 API 响应格式无效", 502, errorBody)
	}
	return encoded, nil
}

// serverInfo mirrors getServerInfo(): GET /1.0.
func (c *IncusClient) serverInfo(ctx context.Context) (json.RawMessage, error) {
	return c.Request(ctx, http.MethodGet, "/1.0", nil, incusDefaultOperationWait, nil)
}

// WaitForOperation mirrors waitForOperation: poll `{operation}/wait?timeout=10`
// until the operation leaves the running state.
func (c *IncusClient) WaitForOperation(ctx context.Context, operationURL string, timeout time.Duration, errorBody any) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = incusDefaultOperationWait
	}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		raw, err := c.Request(ctx, http.MethodGet, operationURL+"/wait?timeout=10", nil, timeout, errorBody)
		if err != nil {
			if aborted, ok := IsNodeOperationAborted(err); ok {
				return nil, aborted
			}
			return nil, OperationError(err, errorBody)
		}

		var operation IncusOperation
		if err := json.Unmarshal(raw, &operation); err != nil || operation.Status == "" {
			return nil, NewIncusAPIError("节点异步操作响应格式无效", 502, errorBody)
		}

		switch operation.Status {
		case "Cancelled":
			return nil, NewIncusAPIError("节点操作已取消，执行失败清理", 409, errorBody)
		case "Success":
			return operation.Metadata, nil
		case "Failure":
			message := operation.Err
			if message == "" {
				message = operation.Error
			}
			if message == "" {
				message = "操作失败"
			}
			return nil, NewIncusAPIError(message, operation.StatusCode, errorBody)
		}

		if operation.StatusCode >= 400 {
			message := operation.Err
			if message == "" {
				message = operation.Error
			}
			if message == "" {
				message = "节点操作失败"
			}
			return nil, NewIncusAPIError(message, operation.StatusCode, errorBody)
		}

		// A short sleep keeps the loop from spinning if the node answers instantly.
		select {
		case <-ctx.Done():
			return nil, &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
		case <-time.After(200 * time.Millisecond):
		}
	}

	return nil, NewIncusAPIError(fmt.Sprintf("Operation timeout (%d seconds)", int(timeout.Seconds())), 504, errorBody)
}

// ReadFile mirrors readFile(): the Incus Files API returns raw bytes, not JSON.
// When limit > 0 only the trailing `limit` bytes are requested and returned.
func (c *IncusClient) ReadFile(ctx context.Context, path string, limit int) (string, int, error) {
	c.mu.RLock()
	httpClient := c.http
	c.mu.RUnlock()
	if httpClient == nil {
		return "", 0, errors.New("Incus 客户端未连接")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", 0, NewIncusAPIError(err.Error(), 502, nil)
	}
	request.Header.Set("Content-Type", "application/json")
	if limit > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=-%d", limit))
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return "", 0, NewIncusAPIError(err.Error(), 502, nil)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return "", response.StatusCode, NewIncusAPIError(err.Error(), 502, nil)
	}
	if limit > 0 && len(payload) > limit {
		payload = payload[:limit]
	}
	return string(payload), response.StatusCode, nil
}

// CheckFileExists mirrors checkFileExists().
func (c *IncusClient) CheckFileExists(ctx context.Context, path string) (bool, int, error) {
	_, statusCode, err := c.ReadFile(ctx, path, 0)
	return statusCode == http.StatusOK, statusCode, err
}

// Connected mirrors the `connected` field.
func (c *IncusClient) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Close mirrors close().
func (c *IncusClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http != nil {
		if transport, ok := c.http.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	c.http = nil
	c.connected = false
	return nil
}

// ==================== connection pool ====================

type incusPooledClient struct {
	client           *IncusClient
	lastUsed         time.Time
	lastResponseTime time.Duration
	errorCount       int
}

var (
	incusPoolMu      sync.Mutex
	incusPool        = map[string]*incusPooledClient{}
	incusConnecting  = map[string]*incusConnectCall{}
	incusPoolStarted bool
)

// incusConnectCall is the Go analogue of the `connectingLocks` promise: every
// caller that arrives during a connect waits on the same channel.
type incusConnectCall struct {
	done   chan struct{}
	client *IncusClient
	err    error
}

// GetIncusClient mirrors getIncusClient(host): reuse a live pooled client,
// reconnect a dead one, or create and connect a new one. Concurrent callers for
// the same host share a single connect attempt.
func GetIncusClient(ctx context.Context, options IncusClientOptions) (*IncusClient, error) {
	startIncusPoolCleanup()

	key := fmt.Sprintf("%d", options.HostID)

	for {
		incusPoolMu.Lock()
		if call, connecting := incusConnecting[key]; connecting {
			incusPoolMu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				return nil, &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
			}
			if call.err != nil {
				return nil, call.err
			}
			return call.client, nil
		}

		if pooled, ok := incusPool[key]; ok {
			pooled.lastUsed = time.Now()
			if pooled.client.Connected() {
				incusPoolMu.Unlock()
				return pooled.client, nil
			}
		}

		call := &incusConnectCall{done: make(chan struct{})}
		incusConnecting[key] = call
		incusPoolMu.Unlock()

		go connectIncusPooled(ctx, key, options, call)
	}
}

// connectIncusPooled performs the single shared connect attempt.
func connectIncusPooled(ctx context.Context, key string, options IncusClientOptions, call *incusConnectCall) {
	defer func() {
		incusPoolMu.Lock()
		delete(incusConnecting, key)
		incusPoolMu.Unlock()
		close(call.done)
	}()

	client := NewIncusClient(options)
	if _, err := client.Connect(ctx); err != nil {
		incusPoolMu.Lock()
		if pooled, ok := incusPool[key]; ok {
			pooled.errorCount++
		}
		incusPoolMu.Unlock()
		call.err = err
		return
	}

	incusPoolMu.Lock()
	incusPool[key] = &incusPooledClient{client: client, lastUsed: time.Now()}
	incusPoolMu.Unlock()

	call.client = client
}

// UpdateIncusClientResponseTime mirrors updateClientResponseTime.
func UpdateIncusClientResponseTime(hostID int64, responseTime time.Duration) {
	key := fmt.Sprintf("%d", hostID)
	incusPoolMu.Lock()
	if pooled, ok := incusPool[key]; ok {
		pooled.lastResponseTime = responseTime
		pooled.lastUsed = time.Now()
		pooled.errorCount = 0
	}
	incusPoolMu.Unlock()
}

// RecordIncusClientError mirrors recordClientError.
func RecordIncusClientError(hostID int64) {
	key := fmt.Sprintf("%d", hostID)
	incusPoolMu.Lock()
	if pooled, ok := incusPool[key]; ok {
		pooled.errorCount++
	}
	incusPoolMu.Unlock()
}

// IncusPoolStatus is one entry of the pool status map.
type IncusPoolStatus struct {
	Connected        bool
	LastUsed         time.Time
	LastResponseTime time.Duration
	ErrorCount       int
}

// GetIncusPoolStatus mirrors getPoolStatus.
func GetIncusPoolStatus() map[string]IncusPoolStatus {
	incusPoolMu.Lock()
	defer incusPoolMu.Unlock()

	status := make(map[string]IncusPoolStatus, len(incusPool))
	for key, pooled := range incusPool {
		status[key] = IncusPoolStatus{
			Connected:        pooled.client.Connected(),
			LastUsed:         pooled.lastUsed,
			LastResponseTime: pooled.lastResponseTime,
			ErrorCount:       pooled.errorCount,
		}
	}
	return status
}

// RemoveIncusClient mirrors removeIncusClient.
func RemoveIncusClient(hostID int64) {
	key := fmt.Sprintf("%d", hostID)
	incusPoolMu.Lock()
	pooled, ok := incusPool[key]
	delete(incusPool, key)
	incusPoolMu.Unlock()

	if ok {
		_ = pooled.client.Close()
	}
}

// CloseAllIncusClients mirrors closeAllClients.
func CloseAllIncusClients() {
	incusPoolMu.Lock()
	clients := make([]*IncusClient, 0, len(incusPool))
	for key, pooled := range incusPool {
		clients = append(clients, pooled.client)
		delete(incusPool, key)
	}
	incusPoolMu.Unlock()

	for _, client := range clients {
		_ = client.Close()
	}
}

// startIncusPoolCleanup mirrors ensureCleanupStarted: drop connections idle for
// more than 30 minutes, checked every 5 minutes.
func startIncusPoolCleanup() {
	incusPoolMu.Lock()
	if incusPoolStarted {
		incusPoolMu.Unlock()
		return
	}
	incusPoolStarted = true
	incusPoolMu.Unlock()

	go func() {
		ticker := time.NewTicker(incusPoolCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			expired := make([]*IncusClient, 0)
			incusPoolMu.Lock()
			for key, pooled := range incusPool {
				if time.Since(pooled.lastUsed) > incusConnectionExpire {
					expired = append(expired, pooled.client)
					delete(incusPool, key)
				}
			}
			incusPoolMu.Unlock()
			for _, client := range expired {
				_ = client.Close()
			}
		}
	}()
}

// MapInstanceStatus mirrors mapInstanceStatus: Incus state → the four DB statuses
// plus 'creating'. Frozen maps to stopped (unusable either way) and Stopping maps
// to running (not fully stopped yet).
func MapInstanceStatus(incusStatus string) string {
	switch incusStatus {
	case "Running":
		return "running"
	case "Stopped", "Frozen":
		return "stopped"
	case "Error":
		return "error"
	case "Starting":
		return "creating"
	case "Stopping":
		return "running"
	default:
		return "unknown"
	}
}
