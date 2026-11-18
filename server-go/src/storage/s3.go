// Port of server/src/storage/providers/S3Provider.ts using AWS SigV4 over
// lib.SafeOutboundHTTPClient (connect-time SSRF revalidation).
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

type s3Provider struct {
	rawHost        string
	basePath       string
	bucket         string
	region         string
	endpoint       *url.URL
	accessKeyID    string
	secretKey      string
	forcePathStyle bool
	client         *http.Client
	now            func() time.Time
}

func newS3Provider(config ConfigData) (*s3Provider, error) {
	basePath, err := NormalizeStorageBasePath(derefString(config.BasePath))
	if err != nil {
		return nil, err
	}
	bucket, err := s3RequiredString(config.Extra["bucket"], "bucket")
	if err != nil {
		return nil, err
	}
	region := "auto"
	if rawRegion, ok := config.Extra["region"].(string); ok && strings.TrimSpace(rawRegion) != "" {
		region = strings.TrimSpace(rawRegion)
	}
	accessKeyID, err := s3RequiredString(derefString(config.Username), "access key")
	if err != nil {
		return nil, err
	}
	secretKey, err := s3RequiredString(derefString(config.Password), "secret key")
	if err != nil {
		return nil, err
	}
	endpointURL, err := s3BuildEndpoint(config.Host, config.Port)
	if err != nil {
		return nil, err
	}
	forcePathStyle, _ := config.Extra["forcePathStyle"].(bool)
	return &s3Provider{
		rawHost:        config.Host,
		basePath:       basePath,
		bucket:         bucket,
		region:         region,
		endpoint:       endpointURL,
		accessKeyID:    accessKeyID,
		secretKey:      secretKey,
		forcePathStyle: forcePathStyle,
		client:         lib.SafeOutboundHTTPClient(),
		now:            func() time.Time { return time.Now().UTC() },
	}, nil
}

func s3RequiredString(value any, field string) (string, error) {
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("S3 %s is required", field)
	}
	return strings.TrimSpace(s), nil
}

func s3BuildEndpoint(host string, port *int32) (*url.URL, error) {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return nil, errors.New("S3 endpoint host is required")
	}
	raw := trimmed
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if port != nil && *port > 0 {
		u.Host = u.Hostname() + ":" + strconv.FormatInt(int64(*port), 10)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func s3ObjectKey(path string) string {
	return strings.TrimLeft(path, "/")
}

func (p *s3Provider) objectURL(key string, query url.Values) string {
	u := *p.endpoint
	basePrefix := strings.TrimRight(p.endpoint.Path, "/")
	var segments []string
	if basePrefix != "" {
		segments = append(segments, strings.TrimLeft(basePrefix, "/"))
	}
	if p.forcePathStyle {
		segments = append(segments, p.bucket)
	} else {
		u.Host = p.bucket + "." + u.Host
	}
	encodedKey := s3EscapeObjectKey(key)
	if len(segments) > 0 {
		if encodedKey != "" {
			u.Path = "/" + strings.Join(segments, "/") + "/" + key
			u.RawPath = "/" + strings.Join(segments, "/") + "/" + encodedKey
		} else {
			u.Path = "/" + strings.Join(segments, "/") + "/"
			u.RawPath = u.Path
		}
	} else {
		u.Path = "/" + key
		u.RawPath = "/" + encodedKey
	}
	if len(query) > 0 {
		u.RawQuery = s3CanonicalQueryString(query)
	}
	return u.String()
}

func s3EscapeObjectKey(key string) string {
	if key == "" {
		return ""
	}
	parts := strings.Split(key, "/")
	for i, part := range parts {
		parts[i] = s3PercentEncode(part)
	}
	return strings.Join(parts, "/")
}

func s3PercentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func s3CanonicalQueryString(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, s3PercentEncode(k)+"="+s3PercentEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func s3HMAC(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func (p *s3Provider) signRequest(req *http.Request, payloadHash string) {
	now := p.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := s3CanonicalQueryString(req.URL.Query())
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		strings.ToLower(req.URL.Host), payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	sum := sha256.Sum256([]byte(canonicalRequest))
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, p.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	kDate := s3HMAC([]byte("AWS4"+p.secretKey), dateStamp)
	kRegion := s3HMAC(kDate, p.region)
	kService := s3HMAC(kRegion, "s3")
	kSigning := s3HMAC(kService, "aws4_request")
	signature := hex.EncodeToString(s3HMAC(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.accessKeyID, scope, signedHeaders, signature,
	))
}

func (p *s3Provider) doSigned(ctx context.Context, method, key string, query url.Values, body io.Reader, contentLength int64) (*http.Response, error) {
	if err := storageAssertSafeTarget(ctx, "S3", p.rawHost); err != nil {
		return nil, err
	}
	target := p.objectURL(key, query)
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	payloadHash := "UNSIGNED-PAYLOAD"
	if body == nil {
		empty := sha256.Sum256(nil)
		payloadHash = hex.EncodeToString(empty[:])
	}
	p.signRequest(req, payloadHash)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

func (p *s3Provider) UploadStream(ctx context.Context, reader io.Reader, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	resp, err := p.doSigned(ctx, http.MethodPut, s3ObjectKey(remotePath), nil, reader, -1)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (p *s3Provider) DownloadStream(ctx context.Context, filename string) (io.ReadCloser, error) {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return nil, err
	}
	resp, err := p.doSigned(ctx, http.MethodGet, s3ObjectKey(remotePath), nil, nil, 0)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (p *s3Provider) TestConnection(ctx context.Context) error {
	q := url.Values{
		"list-type": []string{"2"},
		"max-keys":  []string{"1"},
	}
	if prefix := s3ObjectKey(p.basePath); prefix != "" {
		q.Set("prefix", prefix)
	}
	resp, err := p.doSigned(ctx, http.MethodGet, "", q, nil, 0)
	if err != nil {
		return fmt.Errorf("S3 连接测试失败: %v", err)
	}
	_ = resp.Body.Close()
	return nil
}

func (p *s3Provider) DeleteFile(ctx context.Context, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	resp, err := p.doSigned(ctx, http.MethodDelete, s3ObjectKey(remotePath), nil, nil, 0)
	if err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	return resp.Body.Close()
}

func (p *s3Provider) ListFiles(ctx context.Context, path string) ([]string, error) {
	if err := storageAssertSafeTarget(ctx, "S3", p.rawHost); err != nil {
		return nil, err
	}
	dir, err := JoinStorageRemoteDirectory(p.basePath, path)
	if err != nil {
		return nil, err
	}
	prefix := s3ObjectKey(dir)
	q := url.Values{"list-type": []string{"2"}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	resp, err := p.doSigned(ctx, http.MethodGet, "", q, nil, 0)
	if err != nil {
		return []string{}, nil
	}
	defer resp.Body.Close()

	var listResp struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return []string{}, nil
	}
	out := make([]string, 0, len(listResp.Contents))
	for _, item := range listResp.Contents {
		key := item.Key
		if prefix != "" && strings.HasPrefix(key, prefix) {
			key = key[len(prefix):]
		}
		if key != "" {
			out = append(out, key)
		}
	}
	return out, nil
}
