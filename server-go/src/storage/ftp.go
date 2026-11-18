// Port of server/src/storage/providers/FtpProvider.ts using a lightweight
// RFC 959 passive-mode client over lib.SafeOutboundDialContext.
package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

const ftpTransferTimeout = 6 * time.Hour

type ftpProvider struct {
	rawHost     string
	host        string
	port        int
	username    string
	password    string
	basePath    string
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newFtpProvider(config ConfigData) (*ftpProvider, error) {
	basePath, err := NormalizeStorageBasePath(derefString(config.BasePath))
	if err != nil {
		return nil, err
	}
	host := strings.TrimPrefix(strings.TrimSpace(config.Host), "ftp://")
	port := 21
	if config.Port != nil && *config.Port > 0 {
		port = int(*config.Port)
	}
	username := derefString(config.Username)
	if username == "" {
		username = "anonymous"
	}
	return &ftpProvider{
		rawHost:     config.Host,
		host:        host,
		port:        port,
		username:    username,
		password:    derefString(config.Password),
		basePath:    basePath,
		dialContext: lib.SafeOutboundDialContext,
	}, nil
}

type ftpConn struct {
	provider *ftpProvider
	ctrl     net.Conn
	reader   *bufio.Reader
	host     string
}

func (p *ftpProvider) connect(ctx context.Context) (*ftpConn, error) {
	if err := storageAssertSafeTarget(ctx, "FTP", p.rawHost); err != nil {
		return nil, err
	}
	ctrlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	addr := net.JoinHostPort(p.host, strconv.Itoa(p.port))
	conn, err := p.dialContext(ctrlCtx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(ftpTransferTimeout))
	fc := &ftpConn{
		provider: p,
		ctrl:     conn,
		reader:   bufio.NewReader(conn),
		host:     p.host,
	}
	code, msg, err := fc.readResponse()
	if err != nil || code >= 400 {
		_ = fc.close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("FTP greeting failed (%d): %s", code, msg)
	}
	if code, msg, err := fc.cmd("USER %s", p.username); err != nil {
		_ = fc.close()
		return nil, err
	} else if code == 331 {
		if pCode, pMsg, pErr := fc.cmd("PASS %s", p.password); pErr != nil || pCode >= 400 {
			_ = fc.close()
			if pErr != nil {
				return nil, pErr
			}
			return nil, fmt.Errorf("FTP auth failed (%d): %s", pCode, pMsg)
		}
	} else if code >= 400 {
		_ = fc.close()
		return nil, fmt.Errorf("FTP USER failed (%d): %s", code, msg)
	}
	if code, msg, err := fc.cmd("TYPE I"); err != nil || code >= 400 {
		_ = fc.close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("FTP TYPE I failed (%d): %s", code, msg)
	}
	return fc, nil
}

func (c *ftpConn) close() error {
	if c.ctrl == nil {
		return nil
	}
	_, _, _ = c.cmd("QUIT")
	return c.ctrl.Close()
}

func (c *ftpConn) cmd(format string, args ...any) (int, string, error) {
	line := fmt.Sprintf(format, args...) + "\r\n"
	if _, err := io.WriteString(c.ctrl, line); err != nil {
		return 0, "", err
	}
	return c.readResponse()
}

func (c *ftpConn) readResponse() (int, string, error) {
	first, err := c.reader.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	first = strings.TrimRight(first, "\r\n")
	if len(first) < 3 {
		return 0, first, fmt.Errorf("malformed FTP response: %q", first)
	}
	code, err := strconv.Atoi(first[:3])
	if err != nil {
		return 0, first, err
	}
	var lines []string
	lines = append(lines, first)
	if len(first) >= 4 && first[3] == '-' {
		prefix := first[:3] + " "
		for {
			next, err := c.reader.ReadString('\n')
			if err != nil {
				return 0, "", err
			}
			next = strings.TrimRight(next, "\r\n")
			lines = append(lines, next)
			if strings.HasPrefix(next, prefix) {
				break
			}
		}
	}
	return code, strings.Join(lines, "\n"), nil
}

func (c *ftpConn) openPassiveData(ctx context.Context) (net.Conn, error) {
	// Try PASV first (maximally compatible with standard & NAT servers).
	code, msg, err := c.cmd("PASV")
	if err != nil {
		return nil, err
	}
	if code == 227 {
		start := strings.IndexByte(msg, '(')
		end := strings.LastIndexByte(msg, ')')
		if start < 0 || end <= start {
			return nil, fmt.Errorf("invalid PASV response: %s", msg)
		}
		parts := strings.Split(msg[start+1:end], ",")
		if len(parts) != 6 {
			return nil, fmt.Errorf("invalid PASV tuple: %s", msg)
		}
		p1, err1 := strconv.Atoi(strings.TrimSpace(parts[4]))
		p2, err2 := strconv.Atoi(strings.TrimSpace(parts[5]))
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid PASV port: %s", msg)
		}
		dataPort := p1*256 + p2
		return c.provider.dialContext(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(dataPort)))
	}
	// Fallback to EPSV.
	code, msg, err = c.cmd("EPSV")
	if err != nil {
		return nil, err
	}
	if code != 229 {
		return nil, fmt.Errorf("PASV/EPSV rejected (%d): %s", code, msg)
	}
	start := strings.IndexByte(msg, '(')
	end := strings.LastIndexByte(msg, ')')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("invalid EPSV response: %s", msg)
	}
	fields := strings.Split(msg[start+1:end], msg[start+1:start+2])
	if len(fields) < 4 {
		return nil, fmt.Errorf("invalid EPSV tuple: %s", msg)
	}
	dataPort, err := strconv.Atoi(strings.TrimSpace(fields[3]))
	if err != nil {
		return nil, err
	}
	return c.provider.dialContext(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(dataPort)))
}

func (c *ftpConn) ensureDir(dir string) {
	trimmed := strings.Trim(dir, "/")
	if trimmed == "" {
		return
	}
	cur := ""
	if strings.HasPrefix(dir, "/") {
		cur = ""
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" {
			continue
		}
		if cur == "" && strings.HasPrefix(dir, "/") {
			cur = "/" + seg
		} else if cur == "" {
			cur = seg
		} else {
			cur += "/" + seg
		}
		_, _, _ = c.cmd("MKD %s", cur)
	}
}

func (p *ftpProvider) UploadStream(ctx context.Context, reader io.Reader, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	fc, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer fc.close()

	if p.basePath != "" && p.basePath != "/" {
		fc.ensureDir(p.basePath)
	}
	dataConn, err := fc.openPassiveData(ctx)
	if err != nil {
		return err
	}
	code, msg, err := fc.cmd("STOR %s", remotePath)
	if err != nil || (code != 125 && code != 150) {
		_ = dataConn.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf("FTP STOR failed (%d): %s", code, msg)
	}
	_, copyErr := io.Copy(dataConn, reader)
	closeErr := dataConn.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	code, msg, err = fc.readResponse()
	if err != nil {
		return err
	}
	if code >= 400 {
		return fmt.Errorf("FTP upload completion failed (%d): %s", code, msg)
	}
	return nil
}

func (p *ftpProvider) DownloadStream(ctx context.Context, filename string) (io.ReadCloser, error) {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return nil, err
	}
	fc, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	dataConn, err := fc.openPassiveData(ctx)
	if err != nil {
		_ = fc.close()
		return nil, err
	}
	code, msg, err := fc.cmd("RETR %s", remotePath)
	if err != nil || (code != 125 && code != 150) {
		_ = dataConn.Close()
		_ = fc.close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("FTP RETR failed (%d): %s", code, msg)
	}
	return &ftpDataReadCloser{conn: dataConn, ctrl: fc}, nil
}

type ftpDataReadCloser struct {
	conn net.Conn
	ctrl *ftpConn
	once sync.Once
}

func (f *ftpDataReadCloser) Read(p []byte) (int, error) {
	return f.conn.Read(p)
}

func (f *ftpDataReadCloser) Close() error {
	var err error
	f.once.Do(func() {
		err = f.conn.Close()
		_, _, _ = f.ctrl.readResponse()
		_ = f.ctrl.close()
	})
	return err
}

func (p *ftpProvider) TestConnection(ctx context.Context) error {
	fc, err := p.connect(ctx)
	if err != nil {
		return fmt.Errorf("FTP 连接测试失败: %v", err)
	}
	defer fc.close()

	dataConn, err := fc.openPassiveData(ctx)
	if err != nil {
		return fmt.Errorf("FTP 连接测试失败: %v", err)
	}
	code, msg, err := fc.cmd("NLST /")
	if err != nil || (code != 125 && code != 150 && code != 226) {
		_ = dataConn.Close()
		if err != nil {
			return fmt.Errorf("FTP 连接测试失败: %v", err)
		}
		return fmt.Errorf("FTP 连接测试失败: (%d) %s", code, msg)
	}
	_, _ = io.Copy(io.Discard, dataConn)
	_ = dataConn.Close()
	if code == 125 || code == 150 {
		_, _, _ = fc.readResponse()
	}
	return nil
}

func (p *ftpProvider) DeleteFile(ctx context.Context, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	fc, err := p.connect(ctx)
	if err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	defer fc.close()

	code, msg, err := fc.cmd("DELE %s", remotePath)
	if err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	if code >= 400 {
		return fmt.Errorf("删除文件失败: (%d) %s", code, msg)
	}
	return nil
}

func (p *ftpProvider) ListFiles(ctx context.Context, subPath string) ([]string, error) {
	if err := storageAssertSafeTarget(ctx, "FTP", p.rawHost); err != nil {
		return nil, err
	}
	targetPath, err := JoinStorageRemoteDirectory(p.basePath, subPath)
	if err != nil {
		return nil, err
	}
	fc, err := p.connect(ctx)
	if err != nil {
		return []string{}, nil
	}
	defer fc.close()

	dataConn, err := fc.openPassiveData(ctx)
	if err != nil {
		return []string{}, nil
	}
	code, _, err := fc.cmd("NLST %s", targetPath)
	if err != nil || (code != 125 && code != 150) {
		_ = dataConn.Close()
		return []string{}, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(dataConn, 4*1024*1024))
	_ = dataConn.Close()
	_, _, _ = fc.readResponse()

	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "." || trimmed == ".." {
			continue
		}
		if idx := strings.LastIndexByte(trimmed, '/'); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return out, ctx.Err()
	}
	return out, nil
}
