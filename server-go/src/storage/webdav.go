// Port of server/src/storage/providers/WebDavProvider.ts over
// lib.SafeOutboundHTTPClient (connect-time SSRF revalidation).
package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

type webDavProvider struct {
	rawHost  string
	baseURL  string
	basePath string
	username string
	password string
	client   *http.Client
}

func newWebDavProvider(config ConfigData) (*webDavProvider, error) {
	basePath, err := NormalizeStorageBasePath(derefString(config.BasePath))
	if err != nil {
		return nil, err
	}
	portSuffix := ""
	if config.Port != nil && *config.Port > 0 {
		portSuffix = ":" + strconv.FormatInt(int64(*config.Port), 10)
	}
	rawHost := strings.TrimRight(strings.TrimSpace(config.Host), "/")
	baseURL := "https://" + rawHost + portSuffix
	if strings.Contains(rawHost, "://") {
		baseURL = rawHost + portSuffix
	}
	return &webDavProvider{
		rawHost:  config.Host,
		baseURL:  baseURL,
		basePath: basePath,
		username: derefString(config.Username),
		password: derefString(config.Password),
		client:   lib.SafeOutboundHTTPClient(),
	}, nil
}

func (p *webDavProvider) buildTargetURL(remotePath string) string {
	trimmed := strings.TrimLeft(remotePath, "/")
	if trimmed == "" {
		return p.baseURL + "/"
	}
	return p.baseURL + "/" + trimmed
}

func (p *webDavProvider) do(ctx context.Context, method, remotePath string, body io.Reader, headers map[string]string) (*http.Response, error) {
	if err := storageAssertSafeTarget(ctx, "WEBDAV", p.rawHost); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, p.buildTargetURL(remotePath), body)
	if err != nil {
		return nil, err
	}
	if p.username != "" || p.password != "" {
		req.SetBasicAuth(p.username, p.password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return p.client.Do(req)
}

func (p *webDavProvider) ensureDir(ctx context.Context, dirPath string) {
	trimmed := strings.Trim(dirPath, "/")
	if trimmed == "" {
		return
	}
	parts := strings.Split(trimmed, "/")
	cur := ""
	for _, seg := range parts {
		cur += "/" + seg
		resp, err := p.do(ctx, "MKCOL", cur, nil, nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}
}

func (p *webDavProvider) UploadStream(ctx context.Context, reader io.Reader, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	if p.basePath != "" && p.basePath != "/" {
		p.ensureDir(ctx, p.basePath)
	}
	resp, err := p.do(ctx, http.MethodPut, remotePath, reader, map[string]string{
		"Content-Type": "application/octet-stream",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("WebDAV PUT failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (p *webDavProvider) DownloadStream(ctx context.Context, filename string) (io.ReadCloser, error) {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, http.MethodGet, remotePath, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("WebDAV GET failed (%d)", resp.StatusCode)
	}
	return resp.Body, nil
}

func (p *webDavProvider) TestConnection(ctx context.Context) error {
	resp, err := p.do(ctx, "PROPFIND", "/", nil, map[string]string{"Depth": "0"})
	if err != nil {
		return fmt.Errorf("WebDAV 连接测试失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("WebDAV 连接测试失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *webDavProvider) DeleteFile(ctx context.Context, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, http.MethodDelete, remotePath, nil, nil)
	if err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("删除文件失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *webDavProvider) ListFiles(ctx context.Context, subPath string) ([]string, error) {
	if err := storageAssertSafeTarget(ctx, "WEBDAV", p.rawHost); err != nil {
		return nil, err
	}
	targetPath, err := JoinStorageRemoteDirectory(p.basePath, subPath)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, "PROPFIND", targetPath, nil, map[string]string{"Depth": "1"})
	if err != nil {
		return []string{}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return []string{}, nil
	}
	var multiStatus struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&multiStatus); err != nil {
		return []string{}, nil
	}
	targetTrimmed := strings.TrimRight(targetPath, "/")
	out := make([]string, 0, len(multiStatus.Responses))
	for _, r := range multiStatus.Responses {
		href := strings.TrimRight(r.Href, "/")
		if href == "" || href == targetTrimmed || strings.HasSuffix(targetTrimmed, href) {
			continue
		}
		base := path.Base(href)
		if base != "" && base != "/" && base != "." {
			out = append(out, base)
		}
	}
	return out, nil
}
