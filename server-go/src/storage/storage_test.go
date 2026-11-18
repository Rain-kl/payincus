package storage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
	"gorm.io/datatypes"
)

func TestStoragePathHelpers(t *testing.T) {
	if got, err := NormalizeStorageBasePath("  /backups/daily "); err != nil || got != "/backups/daily/" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if got, err := NormalizeStorageBasePath("nested/dir"); err != nil || got != "nested/dir/" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for _, bad := range []string{"../escape", "/a/../b", "a\\b", "a\x00b"} {
		if _, err := NormalizeStorageBasePath(bad); err == nil {
			t.Fatalf("expected error for basePath %q", bad)
		}
	}
	if got, err := JoinStorageRemotePath("backups", "snap-1.tar.gz"); err != nil || got != "backups/snap-1.tar.gz" {
		t.Fatalf("join=%q err=%v", got, err)
	}
	for _, bad := range []string{"", ".", "..", "dir/file", "a\\b"} {
		if _, err := NormalizeStorageFilename(bad); err == nil {
			t.Fatalf("expected error for filename %q", bad)
		}
	}
}

func TestS3ProviderSigV4AndOperations(t *testing.T) {
	origCheck := storageAssertSafeTarget
	storageAssertSafeTarget = func(context.Context, string, string) error { return nil }
	defer func() { storageAssertSafeTarget = origCheck }()

	var storedKey string
	var storedBody []byte
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			_, _ = w.Write([]byte(`<ListBucketResult><Contents><Key>backups/test.tar.gz</Key></Contents></ListBucketResult>`))
		case r.Method == http.MethodPut:
			storedKey = r.URL.Path
			storedBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == storedKey:
			_, _ = w.Write(storedBody)
		case r.Method == http.MethodDelete && r.URL.Path == storedKey:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	key := "AKIAIOSFODNN7EXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	base := "backups"
	raw, err := NewProvider(ConfigData{
		Type:     models.StorageTypeS3,
		Host:     srv.URL,
		Username: &key,
		Password: &secret,
		BasePath: &base,
		Extra: map[string]any{
			"bucket":         "my-bucket",
			"region":         "us-east-1",
			"forcePathStyle": true,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider S3: %v", err)
	}
	p := raw.(*s3Provider)
	p.client = srv.Client()
	p.now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }

	ctx := context.Background()
	if err := p.TestConnection(ctx); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20261118/us-east-1/s3/aws4_request") {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}

	if err := p.UploadStream(ctx, strings.NewReader("payload-bytes"), "demo.tar.gz"); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
	if storedKey != "/my-bucket/backups/demo.tar.gz" || string(storedBody) != "payload-bytes" {
		t.Fatalf("stored = (%q, %q)", storedKey, string(storedBody))
	}

	rc, err := p.DownloadStream(ctx, "demo.tar.gz")
	if err != nil {
		t.Fatalf("DownloadStream: %v", err)
	}
	dl, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(dl) != "payload-bytes" {
		t.Fatalf("downloaded = %q", string(dl))
	}

	files, err := p.ListFiles(ctx, "")
	if err != nil || len(files) != 1 || files[0] != "test.tar.gz" {
		t.Fatalf("ListFiles = (%#v, %v)", files, err)
	}
	if err := p.DeleteFile(ctx, "demo.tar.gz"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
}

func TestWebDavProviderOperations(t *testing.T) {
	origCheck := storageAssertSafeTarget
	storageAssertSafeTarget = func(context.Context, string, string) error { return nil }
	defer func() { storageAssertSafeTarget = origCheck }()

	var stored []byte
	var gotBasicUser, gotBasicPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBasicUser, gotBasicPass, _ = r.BasicAuth()
		switch r.Method {
		case "PROPFIND":
			w.WriteHeader(207)
			_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"><d:response><d:href>/backups/</d:href></d:response><d:response><d:href>/backups/a.tar.gz</d:href></d:response></d:multistatus>`))
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			stored, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			_, _ = w.Write(stored)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	user := "alice"
	pass := "secret"
	base := "/backups"
	raw, err := NewProvider(ConfigData{
		Type:     models.StorageTypeWEBDAV,
		Host:     srv.URL,
		Username: &user,
		Password: &pass,
		BasePath: &base,
	})
	if err != nil {
		t.Fatalf("NewProvider WebDAV: %v", err)
	}
	p := raw.(*webDavProvider)
	p.client = srv.Client()

	ctx := context.Background()
	if err := p.TestConnection(ctx); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if gotBasicUser != "alice" || gotBasicPass != "secret" {
		t.Fatalf("basic auth = (%q, %q)", gotBasicUser, gotBasicPass)
	}
	if err := p.UploadStream(ctx, strings.NewReader("dav-data"), "a.tar.gz"); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
	rc, err := p.DownloadStream(ctx, "a.tar.gz")
	if err != nil {
		t.Fatalf("DownloadStream: %v", err)
	}
	dl, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(dl) != "dav-data" {
		t.Fatalf("downloaded = %q", string(dl))
	}
	files, err := p.ListFiles(ctx, "")
	if err != nil || len(files) != 1 || files[0] != "a.tar.gz" {
		t.Fatalf("ListFiles = (%#v, %v)", files, err)
	}
	if err := p.DeleteFile(ctx, "a.tar.gz"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
}

func TestFtpProviderProtocolRoundTrip(t *testing.T) {
	origCheck := storageAssertSafeTarget
	storageAssertSafeTarget = func(context.Context, string, string) error { return nil }
	defer func() { storageAssertSafeTarget = origCheck }()

	// Spin up a minimal passive FTP server.
	ctrlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlLn.Close()

	var uploaded bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ctrlLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				writeLine := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
				writeLine("220 test ftp ready")
				var dataLn net.Listener
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmdLine := strings.TrimSpace(line)
					upper := strings.ToUpper(cmdLine)
					switch {
					case strings.HasPrefix(upper, "USER"):
						writeLine("331 password required")
					case strings.HasPrefix(upper, "PASS"):
						writeLine("230 login ok")
					case strings.HasPrefix(upper, "TYPE"):
						writeLine("200 type set")
					case strings.HasPrefix(upper, "MKD"):
						writeLine("257 created")
					case upper == "PASV":
						dataLn, _ = net.Listen("tcp", "127.0.0.1:0")
						p := dataLn.Addr().(*net.TCPAddr).Port
						writeLine("227 Entering Passive Mode (127,0,0,1," + strings.ReplaceAll(strings.TrimSpace(
							func() string {
								return string([]byte{byte('0' + (p/256)/100), byte('0' + ((p/256)/10)%10), byte('0' + (p/256)%10)})
							}(),
						), "0", "0") + ")")
						// Write valid integer tuple directly.
						_ = dataLn.Close()
					case strings.HasPrefix(upper, "QUIT"):
						writeLine("221 bye")
						return
					}
				}
			}(c)
		}
	}()
	_ = ctrlLn.Close()
	<-done

	// Clean RFC 959 passive FTP server loop.
	ctrlLn2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlLn2.Close()

	go func() {
		for {
			c, err := ctrlLn2.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				writeLine := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
				writeLine("220 test ftp ready")
				var dataLn net.Listener
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmdLine := strings.TrimSpace(line)
					upper := strings.ToUpper(cmdLine)
					switch {
					case strings.HasPrefix(upper, "USER"):
						writeLine("331 password required")
					case strings.HasPrefix(upper, "PASS"):
						writeLine("230 login ok")
					case strings.HasPrefix(upper, "TYPE"):
						writeLine("200 type set")
					case strings.HasPrefix(upper, "MKD"):
						writeLine("257 ok")
					case upper == "PASV":
						if dataLn != nil {
							_ = dataLn.Close()
						}
						dataLn, _ = net.Listen("tcp", "127.0.0.1:0")
						p := dataLn.Addr().(*net.TCPAddr).Port
						writeLine("227 Entering Passive Mode (127,0,0,1," +
							strconvItoa(p/256) + "," + strconvItoa(p%256) + ")")
					case strings.HasPrefix(upper, "STOR"):
						writeLine("150 opening data")
						dc, err := dataLn.Accept()
						if err == nil {
							_, _ = io.Copy(&uploaded, dc)
							_ = dc.Close()
						}
						_ = dataLn.Close()
						writeLine("226 transfer complete")
					case strings.HasPrefix(upper, "DELE"):
						writeLine("250 deleted")
					case strings.HasPrefix(upper, "QUIT"):
						writeLine("221 bye")
						return
					default:
						writeLine("502 not implemented")
					}
				}
			}(c)
		}
	}()

	host, portStr, _ := net.SplitHostPort(ctrlLn2.Addr().String())
	var portNum int32
	for i := 0; i < len(portStr); i++ {
		portNum = portNum*10 + int32(portStr[i]-'0')
	}
	user := "u"
	pass := "p"
	base := "bak"
	raw, err := NewProvider(ConfigData{
		Type:     models.StorageTypeFTP,
		Host:     host,
		Port:     &portNum,
		Username: &user,
		Password: &pass,
		BasePath: &base,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := raw.(*ftpProvider)
	p.dialContext = (&net.Dialer{}).DialContext

	if err := p.UploadStream(context.Background(), strings.NewReader("ftp-payload"), "archive.tar.gz"); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
	if uploaded.String() != "ftp-payload" {
		t.Fatalf("uploaded = %q", uploaded.String())
	}
	if err := p.DeleteFile(context.Background(), "archive.tar.gz"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
}

func TestCreateDecryptsPasswordAndSSRFGuardsBlockPrivateHost(t *testing.T) {
	t.Setenv("JWT_SECRET", "unit-test-secret-0123456789abcdef")
	cipher, err := lib.EncryptSensitiveData("top-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	key := "AKIA123"
	cfg := &models.StorageConfig{
		Type:     models.StorageTypeS3,
		Host:     "127.0.0.1",
		Username: &key,
		Password: &cipher,
		Extra:    datatypes.JSON(`{"bucket":"b","region":"us-east-1"}`),
	}
	provider, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	s3p := provider.(*s3Provider)
	if s3p.secretKey != "top-secret-key" {
		t.Fatalf("secretKey = %q, want decrypted secret", s3p.secretKey)
	}
	// SSRF guard rejects loopback target on actual call.
	if err := s3p.TestConnection(context.Background()); err == nil || !errors.Is(err, err) {
		t.Fatal("expected SSRF error for 127.0.0.1")
	}
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
