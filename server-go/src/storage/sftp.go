// Port of server/src/storage/providers/SftpProvider.ts using pure-Go SFTP v3
// framing over golang.org/x/crypto/ssh and lib.SafeOutboundDialContext.
package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

const (
	sftpProtoVersion  = 3
	sftpChunkSize     = 32 * 1024
	sftpConnectWindow = 30 * time.Second

	sshFXPInit    byte = 1
	sshFXPVersion byte = 2
	sshFXPOpen    byte = 3
	sshFXPClose   byte = 4
	sshFXPRead    byte = 5
	sshFXPWrite   byte = 6
	sshFXPOpenDir byte = 11
	sshFXPReadDir byte = 12
	sshFXPRemove  byte = 13
	sshFXPMkdir   byte = 14
	sshFXPStatus  byte = 101
	sshFXPHandle  byte = 102
	sshFXPData    byte = 103
	sshFXPName    byte = 104

	sshFXFRead  uint32 = 0x00000001
	sshFXFWrite uint32 = 0x00000002
	sshFXFCreat uint32 = 0x00000008
	sshFXFTrunc uint32 = 0x00000010

	sshFXOK  uint32 = 0
	sshFXEOF uint32 = 1
)

type sftpProvider struct {
	rawHost     string
	host        string
	port        int
	username    string
	password    string
	basePath    string
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newSftpProvider(config ConfigData) (*sftpProvider, error) {
	basePath, err := NormalizeStorageBasePath(derefString(config.BasePath))
	if err != nil {
		return nil, err
	}
	host := strings.TrimPrefix(strings.TrimSpace(config.Host), "sftp://")
	port := 22
	if config.Port != nil && *config.Port > 0 {
		port = int(*config.Port)
	}
	return &sftpProvider{
		rawHost:     config.Host,
		host:        host,
		port:        port,
		username:    derefString(config.Username),
		password:    derefString(config.Password),
		basePath:    basePath,
		dialContext: lib.SafeOutboundDialContext,
	}, nil
}

type sftpSession struct {
	sshClient *ssh.Client
	session   *ssh.Session
	stdin     io.WriteCloser
	stdout    io.Reader
	reqID     uint32
}

func (p *sftpProvider) connect(ctx context.Context) (*sftpSession, error) {
	if err := storageAssertSafeTarget(ctx, "SFTP", p.rawHost); err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, sftpConnectWindow)
	defer cancel()

	addr := net.JoinHostPort(p.host, strconv.Itoa(p.port))
	rawConn, err := p.dialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	sshCfg := &ssh.ClientConfig{
		User: p.username,
		Auth: []ssh.AuthMethod{
			ssh.Password(p.password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = p.password
				}
				return answers, nil
			}),
		},
		// Custom SSH backup targets are user-configured without host-key pins in DB.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         sftpConnectWindow,
	}
	c, chans, reqs, err := ssh.NewClientConn(rawConn, addr, sshCfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	sess, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, err
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, err
	}

	sc := &sftpSession{
		sshClient: client,
		session:   sess,
		stdin:     stdin,
		stdout:    stdout,
	}
	var initBody [4]byte
	binary.BigEndian.PutUint32(initBody[:], sftpProtoVersion)
	if err := sc.writePacket(sshFXPInit, initBody[:]); err != nil {
		_ = sc.close()
		return nil, err
	}
	pType, _, err := sc.readPacket()
	if err != nil {
		_ = sc.close()
		return nil, err
	}
	if pType != sshFXPVersion {
		_ = sc.close()
		return nil, fmt.Errorf("unexpected SFTP init response type %d", pType)
	}
	return sc, nil
}

func (s *sftpSession) close() error {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.session != nil {
		_ = s.session.Close()
	}
	if s.sshClient != nil {
		return s.sshClient.Close()
	}
	return nil
}

func (s *sftpSession) writePacket(pktType byte, payload []byte) error {
	var header [5]byte
	binary.BigEndian.PutUint32(header[:4], uint32(1+len(payload)))
	header[4] = pktType
	var frame bytes.Buffer
	frame.Grow(5 + len(payload))
	frame.Write(header[:])
	frame.Write(payload)
	_, err := s.stdin.Write(frame.Bytes())
	return err
}

func (s *sftpSession) readPacket() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(s.stdout, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:4])
	if length == 0 || length > 16*1024*1024 {
		return 0, nil, fmt.Errorf("invalid SFTP packet length %d", length)
	}
	payload := make([]byte, length-1)
	if _, err := io.ReadFull(s.stdout, payload); err != nil {
		return 0, nil, err
	}
	return header[4], payload, nil
}

func sftpPutUint32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func sftpPutUint64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

func sftpPutString(buf *bytes.Buffer, s string) {
	sftpPutUint32(buf, uint32(len(s)))
	buf.WriteString(s)
}

func sftpReadString(b []byte) (string, []byte, error) {
	if len(b) < 4 {
		return "", nil, io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(b[:4])
	if int(4+n) > len(b) {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(b[4 : 4+n]), b[4+n:], nil
}

func sftpParseStatusError(payload []byte) (uint32, error) {
	if len(payload) < 8 {
		return 0, io.ErrUnexpectedEOF
	}
	code := binary.BigEndian.Uint32(payload[4:8])
	if code == sshFXOK {
		return code, nil
	}
	msg, _, err := sftpReadString(payload[8:])
	if err != nil || msg == "" {
		return code, fmt.Errorf("SFTP status code %d", code)
	}
	return code, fmt.Errorf("SFTP error (%d): %s", code, msg)
}

func (s *sftpSession) nextID() uint32 {
	s.reqID++
	return s.reqID
}

func (s *sftpSession) expectHandleOrErr() ([]byte, error) {
	pType, payload, err := s.readPacket()
	if err != nil {
		return nil, err
	}
	if pType == sshFXPStatus {
		_, stErr := sftpParseStatusError(payload)
		return nil, stErr
	}
	if pType != sshFXPHandle {
		return nil, fmt.Errorf("unexpected SFTP response %d", pType)
	}
	if len(payload) < 4 {
		return nil, io.ErrUnexpectedEOF
	}
	handle, _, err := sftpReadString(payload[4:])
	return []byte(handle), err
}

func (s *sftpSession) expectOK() error {
	pType, payload, err := s.readPacket()
	if err != nil {
		return err
	}
	if pType != sshFXPStatus {
		return fmt.Errorf("unexpected SFTP response %d", pType)
	}
	_, stErr := sftpParseStatusError(payload)
	return stErr
}

func (s *sftpSession) closeHandle(handle []byte) {
	var buf bytes.Buffer
	sftpPutUint32(&buf, s.nextID())
	sftpPutString(&buf, string(handle))
	_ = s.writePacket(sshFXPClose, buf.Bytes())
	_, _, _ = s.readPacket()
}

func (s *sftpSession) mkdirAll(dir string) {
	trimmed := strings.Trim(dir, "/")
	if trimmed == "" {
		return
	}
	cur := ""
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
		var buf bytes.Buffer
		sftpPutUint32(&buf, s.nextID())
		sftpPutString(&buf, cur)
		sftpPutUint32(&buf, 0) // attrs flags = 0
		if err := s.writePacket(sshFXPMkdir, buf.Bytes()); err == nil {
			_ = s.expectOK()
		}
	}
}

func (s *sftpSession) openFile(remotePath string, pflags uint32) ([]byte, error) {
	var buf bytes.Buffer
	sftpPutUint32(&buf, s.nextID())
	sftpPutString(&buf, remotePath)
	sftpPutUint32(&buf, pflags)
	sftpPutUint32(&buf, 0) // attrs flags = 0
	if err := s.writePacket(sshFXPOpen, buf.Bytes()); err != nil {
		return nil, err
	}
	return s.expectHandleOrErr()
}

func (p *sftpProvider) UploadStream(ctx context.Context, reader io.Reader, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	sess, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer sess.close()

	if p.basePath != "" && p.basePath != "/" {
		sess.mkdirAll(p.basePath)
	}
	handle, err := sess.openFile(remotePath, sshFXFWrite|sshFXFCreat|sshFXFTrunc)
	if err != nil {
		return err
	}
	defer sess.closeHandle(handle)

	chunk := make([]byte, sftpChunkSize)
	var offset uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := reader.Read(chunk)
		if n > 0 {
			var buf bytes.Buffer
			sftpPutUint32(&buf, sess.nextID())
			sftpPutString(&buf, string(handle))
			sftpPutUint64(&buf, offset)
			sftpPutUint32(&buf, uint32(n))
			buf.Write(chunk[:n])
			if err := sess.writePacket(sshFXPWrite, buf.Bytes()); err != nil {
				return err
			}
			if err := sess.expectOK(); err != nil {
				return err
			}
			offset += uint64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (p *sftpProvider) DownloadStream(ctx context.Context, filename string) (io.ReadCloser, error) {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return nil, err
	}
	sess, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	handle, err := sess.openFile(remotePath, sshFXFRead)
	if err != nil {
		_ = sess.close()
		return nil, err
	}
	return &sftpReadCloser{
		ctx:    ctx,
		sess:   sess,
		handle: handle,
	}, nil
}

type sftpReadCloser struct {
	ctx    context.Context
	sess   *sftpSession
	handle []byte
	offset uint64
	buf    []byte
	eof    bool
	once   sync.Once
}

func (r *sftpReadCloser) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		var req bytes.Buffer
		sftpPutUint32(&req, r.sess.nextID())
		sftpPutString(&req, string(r.handle))
		sftpPutUint64(&req, r.offset)
		sftpPutUint32(&req, sftpChunkSize)
		if err := r.sess.writePacket(sshFXPRead, req.Bytes()); err != nil {
			return 0, err
		}
		pType, payload, err := r.sess.readPacket()
		if err != nil {
			return 0, err
		}
		if pType == sshFXPStatus {
			code, stErr := sftpParseStatusError(payload)
			if code == sshFXEOF {
				r.eof = true
				return 0, io.EOF
			}
			return 0, stErr
		}
		if pType != sshFXPData || len(payload) < 4 {
			return 0, fmt.Errorf("unexpected SFTP read response %d", pType)
		}
		data, _, err := sftpReadString(payload[4:])
		if err != nil {
			return 0, err
		}
		r.offset += uint64(len(data))
		r.buf = []byte(data)
		if len(data) == 0 {
			r.eof = true
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *sftpReadCloser) Close() error {
	r.once.Do(func() {
		r.sess.closeHandle(r.handle)
		_ = r.sess.close()
	})
	return nil
}

func (s *sftpSession) listDir(dir string) ([]string, error) {
	var openBuf bytes.Buffer
	sftpPutUint32(&openBuf, s.nextID())
	sftpPutString(&openBuf, dir)
	if err := s.writePacket(sshFXPOpenDir, openBuf.Bytes()); err != nil {
		return nil, err
	}
	handle, err := s.expectHandleOrErr()
	if err != nil {
		return nil, err
	}
	defer s.closeHandle(handle)

	out := []string{}
	for {
		var rdBuf bytes.Buffer
		sftpPutUint32(&rdBuf, s.nextID())
		sftpPutString(&rdBuf, string(handle))
		if err := s.writePacket(sshFXPReadDir, rdBuf.Bytes()); err != nil {
			return out, err
		}
		pType, payload, err := s.readPacket()
		if err != nil {
			return out, err
		}
		if pType == sshFXPStatus {
			code, stErr := sftpParseStatusError(payload)
			if code == sshFXEOF {
				return out, nil
			}
			return out, stErr
		}
		if pType != sshFXPName || len(payload) < 8 {
			return out, fmt.Errorf("unexpected SFTP readdir response %d", pType)
		}
		count := binary.BigEndian.Uint32(payload[4:8])
		rest := payload[8:]
		for i := uint32(0); i < count; i++ {
			var name string
			name, rest, err = sftpReadString(rest)
			if err != nil {
				return out, err
			}
			_, rest, err = sftpReadString(rest) // longname
			if err != nil {
				return out, err
			}
			// Skip attrs: uint32 flags + optional attributes (read minimal prefix)
			if len(rest) < 4 {
				return out, io.ErrUnexpectedEOF
			}
			flags := binary.BigEndian.Uint32(rest[:4])
			rest = rest[4:]
			if flags&0x00000001 != 0 && len(rest) >= 8 { // size
				rest = rest[8:]
			}
			if flags&0x00000002 != 0 && len(rest) >= 8 { // uid/gid
				rest = rest[8:]
			}
			if flags&0x00000004 != 0 && len(rest) >= 4 { // permissions
				rest = rest[4:]
			}
			if flags&0x00000008 != 0 && len(rest) >= 8 { // acmodtime
				rest = rest[8:]
			}
			if flags&0x80000000 != 0 && len(rest) >= 4 { // extended
				extCount := binary.BigEndian.Uint32(rest[:4])
				rest = rest[4:]
				for j := uint32(0); j < extCount; j++ {
					_, rest, _ = sftpReadString(rest)
					_, rest, _ = sftpReadString(rest)
				}
			}
			if name != "." && name != ".." && name != "" {
				out = append(out, name)
			}
		}
	}
}

func (p *sftpProvider) TestConnection(ctx context.Context) error {
	sess, err := p.connect(ctx)
	if err != nil {
		return fmt.Errorf("SFTP 连接测试失败: %v", err)
	}
	defer sess.close()

	if _, err := sess.listDir("/"); err != nil {
		return fmt.Errorf("SFTP 连接测试失败: %v", err)
	}
	return nil
}

func (p *sftpProvider) DeleteFile(ctx context.Context, filename string) error {
	remotePath, err := JoinStorageRemotePath(p.basePath, filename)
	if err != nil {
		return err
	}
	sess, err := p.connect(ctx)
	if err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	defer sess.close()

	var buf bytes.Buffer
	sftpPutUint32(&buf, sess.nextID())
	sftpPutString(&buf, remotePath)
	if err := sess.writePacket(sshFXPRemove, buf.Bytes()); err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	if err := sess.expectOK(); err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}
	return nil
}

func (p *sftpProvider) ListFiles(ctx context.Context, subPath string) ([]string, error) {
	if err := storageAssertSafeTarget(ctx, "SFTP", p.rawHost); err != nil {
		return nil, err
	}
	dir, err := JoinStorageRemoteDirectory(p.basePath, subPath)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = "/"
	}
	sess, err := p.connect(ctx)
	if err != nil {
		return []string{}, nil
	}
	defer sess.close()

	files, err := sess.listDir(dir)
	if err != nil {
		return []string{}, nil
	}
	return files, nil
}
