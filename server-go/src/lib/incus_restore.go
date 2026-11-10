// Port of server/src/lib/incus/incus-restore.ts (188 lines): restore an instance
// from a backup stream, rename an instance, and probe for its existence.
//
// The TypeScript builds a dedicated undici Agent per restore (60s connect/headers
// timeout, body timeout disabled) because a backup upload can run for many
// minutes. Go reuses the already-connected client's transport (CLONED, so the
// per-request header timeout does not leak into the pooled client) and leaves the
// overall timeout at zero, which is the faithful equivalent of `bodyTimeout: 0`.
// The mTLS certificate, the pinned-fingerprint dialer and the Agent-tunnel dialer
// all live in that transport, so the clone keeps every security control — the
// TypeScript's re-read of cert/key files only exists because undici needs its own
// agent; this port does not read them again.
//
// waitForRestoreOperation is deliberately NOT IncusClient.WaitForOperation: the
// restore loop uses the TypeScript's own budgets (5 minutes total, `timeout=N`
// computed from the remaining time, capped at 30s, and a 2s back-off only after a
// network error). Reusing the generic wait would change both the total budget and
// the failure messages.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// IncusRestoreTimeout mirrors the default `timeout: 300000` (5 minutes).
	IncusRestoreTimeout = 300 * time.Second
	// incusRestoreConnectTimeout mirrors `connect.timeout: 60000` /
	// `headersTimeout: 60000`.
	incusRestoreConnectTimeout = 60 * time.Second
	// incusRestorePollBackoff mirrors the 2s sleep after a polling error.
	incusRestorePollBackoff = 2 * time.Second
	// incusRestoreWaitCap mirrors `Math.min(30, ...)`.
	incusRestoreWaitCap = 30
)

// GenerateTempInstanceName mirrors generateTempInstanceName:
// `{originalName}-restore-{Date.now()}`.
func GenerateTempInstanceName(originalName string) string {
	return fmt.Sprintf("%s-restore-%d", originalName, time.Now().UTC().UnixMilli())
}

// RestoreFromStreamParams mirrors RestoreFromStreamParams.
type RestoreFromStreamParams struct {
	Client     *IncusClient
	Backup     io.Reader
	TargetName string
	PoolName   string
	// Timeout overrides IncusRestoreTimeout; zero uses the default. Exposed for
	// tests only (the TypeScript hard-codes 5 minutes).
	Timeout time.Duration
}

// RestoreFromStream mirrors restoreFromStream: POST the raw backup stream to
// /1.0/instances with the X-Incus-name / X-Incus-pool headers, then wait for the
// asynchronous restore operation.
func RestoreFromStream(ctx context.Context, params RestoreFromStreamParams) error {
	client := params.Client
	if client == nil {
		return errors.New("Incus 客户端未初始化")
	}
	// The TypeScript refuses before I/O when the client has no certificate pair;
	// the Go client's transport already carries the pair.
	if client.certPath == "" || client.keyPath == "" {
		return errors.New("Certificate path not configured")
	}

	restoreHTTP, transport, err := client.incusRestoreHTTPClient()
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/1.0/instances", params.Backup)
	if err != nil {
		return err
	}
	// Incus accepts the X-Incus-* headers (and the legacy X-LXD-* ones).
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Incus-name", params.TargetName)
	if params.PoolName != "" {
		request.Header.Set("X-Incus-pool", params.PoolName)
	}

	response, err := restoreHTTP.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}

	var envelope IncusAPIResponse
	if err := json.Unmarshal(payload, &envelope); err != nil {
		text := strings.TrimSpace(string(payload))
		log.Printf("[Incus Restore] 解析响应失败: %s", incusRestoreTruncate(text, 200))
		return fmt.Errorf("无效的响应: %s", incusRestoreTruncate(text, 200))
	}

	if envelope.Type == IncusAPITypeError {
		log.Printf("[Incus Restore] 恢复失败: %s", envelope.Error)
		if envelope.Error != "" {
			return errors.New(envelope.Error)
		}
		return errors.New("恢复失败")
	}

	if envelope.Type == IncusAPITypeAsync && envelope.Operation != "" {
		timeout := params.Timeout
		if timeout <= 0 {
			timeout = IncusRestoreTimeout
		}
		return waitForRestoreOperation(ctx, client.baseURL, envelope.Operation, restoreHTTP, timeout)
	}
	return nil
}

// incusRestoreHTTPClient clones the connected client's transport and disables the
// overall request timeout. The clone shares the TLS config, pinned dialer and the
// tunnel dialer, so the security posture is unchanged.
func (c *IncusClient) incusRestoreHTTPClient() (*http.Client, *http.Transport, error) {
	c.mu.RLock()
	baseClient := c.http
	c.mu.RUnlock()
	if baseClient == nil {
		return nil, nil, errors.New("Incus 客户端未连接")
	}

	transport, ok := baseClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		return nil, nil, errors.New("Incus 客户端传输层不支持恢复流")
	}
	clone := transport.Clone()
	// undici's headersTimeout: the node must respond within 60s; the body may then
	// take as long as it needs.
	clone.ResponseHeaderTimeout = incusRestoreConnectTimeout

	return &http.Client{Transport: clone, Timeout: 0}, clone, nil
}

// incusRestoreOperationPayload is the /wait metadata projection. Pointer fields
// preserve the TypeScript's `statusCode && statusCode >= 400` check (a missing or
// zero code is falsy).
type incusRestoreOperationPayload struct {
	Status     string `json:"status"`
	StatusCode *int   `json:"status_code"`
	Err        string `json:"err"`
	Error      string `json:"error"`
}

// waitForRestoreOperation mirrors waitForRestoreOperation.
func waitForRestoreOperation(ctx context.Context, baseURL, operationURL string, httpClient *http.Client, timeout time.Duration) error {
	start := time.Now()

	for time.Since(start) < timeout {
		remaining := timeout - time.Since(start)
		waitSeconds := int(remaining / time.Second)
		if waitSeconds > incusRestoreWaitCap {
			waitSeconds = incusRestoreWaitCap
		}
		if waitSeconds <= 0 {
			break
		}

		waitURL := fmt.Sprintf("%s%s/wait?timeout=%d", baseURL, operationURL, waitSeconds)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, waitURL, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")

		response, requestErr := httpClient.Do(request)
		if requestErr != nil {
			if ctx.Err() != nil {
				return &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
			}
			log.Printf("[Incus Restore] 轮询出错: %s", requestErr.Error())
			select {
			case <-ctx.Done():
				return &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
			case <-time.After(incusRestorePollBackoff):
			}
			continue
		}

		payload, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			log.Printf("[Incus Restore] 轮询出错: %s", readErr.Error())
			select {
			case <-ctx.Done():
				return &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
			case <-time.After(incusRestorePollBackoff):
			}
			continue
		}

		var envelope IncusAPIResponse
		if err := json.Unmarshal(payload, &envelope); err != nil {
			text := strings.TrimSpace(string(payload))
			log.Printf("[Incus Restore] 解析响应失败: %s", incusRestoreTruncate(text, 200))
			// The TypeScript throws 无效的响应 inside the try and then falls into its
			// own catch: the message does not match 操作失败/Failure, so it logs and
			// retries after 2s.
			select {
			case <-ctx.Done():
				return &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
			case <-time.After(incusRestorePollBackoff):
			}
			continue
		}

		var metadata incusRestoreOperationPayload
		if len(envelope.Metadata) > 0 {
			_ = json.Unmarshal(envelope.Metadata, &metadata)
		}
		status := metadata.Status
		if status == "" {
			status = "unknown"
		}

		if status == "Success" || (metadata.StatusCode != nil && *metadata.StatusCode == http.StatusOK) {
			return nil
		}

		if status == "Failure" || (metadata.StatusCode != nil && *metadata.StatusCode >= 400) {
			message := metadata.Err
			if message == "" {
				message = metadata.Error
			}
			if message == "" {
				message = "操作失败"
			}
			log.Printf("[Incus Restore] 操作失败: %s", message)
			return errors.New(message)
		}
	}

	return errors.New("恢复操作超时 (5 分钟)")
}

// RenameIncusInstance mirrors renameInstance: POST {name} to the instance path.
// The TypeScript does not URL-escape the name, and neither does this port.
func RenameIncusInstance(ctx context.Context, client *IncusClient, oldName, newName string) error {
	body := map[string]any{"name": newName}
	_, err := client.Request(ctx, http.MethodPost, "/1.0/instances/"+oldName, body, 0, nil)
	return err
}

// InstanceExists mirrors instanceExists: any error means "not reachable", which
// the TypeScript treats as "does not exist".
func InstanceExists(ctx context.Context, client *IncusClient, instanceName string) bool {
	_, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+instanceName, nil, 0, nil)
	return err == nil
}

// incusRestoreTruncate mirrors `text.substring(0, n)` for log/error text.
func incusRestoreTruncate(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}
