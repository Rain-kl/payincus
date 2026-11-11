// Port of the streaming half of server/src/lib/incus/incus-backups.ts
// (getBackupExportStream), plus the small exported wrapper for the cloud-init
// network-config generator.
//
// Why it lives in lib: the IncusClient's base URL and certificate paths are
// unexported, so a caller outside this package cannot build the long-lived
// streaming request without duplicating the transport (and its pinned dialer /
// tunnel dialer). Keeping the stream here reuses incusRestoreHTTPClient, which is
// the same "headers within 60s, body for as long as it takes" policy the Node code
// applies to backup exports.
package lib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// BackupExportStream is the result of GetBackupExportStream. The caller owns
// Body and must close it; ContentLength is nil when the node did not send a
// Content-Length header (chunked transfer).
type BackupExportStream struct {
	Body          io.ReadCloser
	ContentLength *int64
}

// GetBackupExportStream mirrors getBackupExportStream: GET
// /1.0/instances/{instance}/backups/{backup}/export and hand the raw body back to
// the caller. The request itself has no body timeout (the transport clone has
// Timeout 0 and only a ResponseHeaderTimeout), so a multi-GB backup can stream.
func GetBackupExportStream(ctx context.Context, client *IncusClient, instanceName, backupName string) (*BackupExportStream, error) {
	if client == nil {
		return nil, errors.New("Incus 客户端未连接")
	}
	if client.certPath == "" || client.keyPath == "" {
		return nil, errors.New("Certificate path not configured")
	}

	exportHTTP, transport, err := client.incusRestoreHTTPClient()
	if err != nil {
		return nil, err
	}

	url := client.baseURL + "/1.0/instances/" + instanceName + "/backups/" + backupName + "/export"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}

	response, err := exportHTTP.Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		text := string(body)
		if text != "" {
			return nil, fmt.Errorf("导出备份失败: %s", text)
		}
		return nil, fmt.Errorf("导出备份失败: %d", response.StatusCode)
	}

	var contentLength *int64
	if header := response.Header.Get("Content-Length"); header != "" {
		if parsed, parseErr := strconv.ParseInt(header, 10, 64); parseErr == nil {
			contentLength = &parsed
		}
	}

	return &BackupExportStream{
		Body:          &backupExportBody{ReadCloser: response.Body, transport: transport},
		ContentLength: contentLength,
	}, nil
}

// backupExportBody closes the idle connections when the stream ends; undici's
// `agent.close()` is the Node equivalent.
type backupExportBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b *backupExportBody) Close() error {
	err := b.ReadCloser.Close()
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	return err
}

// GenerateCloudInitNetworkConfig exposes the network-config generator that
// network_payload_builder.go keeps unexported. Rebuild and clone need to inject
// this payload directly (the TS imports the same function from
// lib/network-payload-builder.js).
func GenerateCloudInitNetworkConfig(ipv4Address string, ipv6List []string) string {
	return networkPayloadGenerateCloudInitNetworkConfig(ipv4Address, ipv6List)
}

// NetworkPayloadHasIpv6Support mirrors hasIpv6Support from
// lib/network-payload-builder.ts: routed IPv6 needs eth1, so the rebuild path uses
// it to decide whether to force the dual-stack network-config.
func NetworkPayloadHasIpv6Support(networkMode string) bool {
	return NetworkModeNeedsRoutedIpv6(networkMode)
}
