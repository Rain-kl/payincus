// Port of server/src/lib/spice-sessions.ts.
//
// Bounded, in-process capabilities for the graphical console:
//
//   - the ticket returned to the browser is a random 48-character capability. It is
//     the only thing that authorises the WebSocket channels; the Incus operation id
//     and the fd secrets never leave the backend.
//   - at most 100 reserved instances, at most 2 sessions per user and at most one
//     session per instance (CONSOLE_LIMIT otherwise).
//   - a session lives at most 2 hours, and at most 60 s without a main channel.
//   - every 25 s the manager validates the session against the database and pings
//     the Incus control channel; two missed control pongs close it.
//   - at most 12 browser channels per session (main + display + input + …), each
//     capped at 8 MiB buffered per direction; the first channel is the main one and
//     closing it closes the session.
//
// Deployment invariant (AGENTS.md, WAVE_D_SPEC §0.1): the session map is process
// local. A second backend replica would not see tickets issued by the first. Do not
// move this to a shared store without an owner-approved architecture change.
package lib

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Limits mirror the constants at the top of spice-sessions.ts.
const (
	SpiceMaxBuffer            = 8 * 1024 * 1024
	SpiceMaxChannels          = 12
	SpiceSessionLifetime      = 2 * time.Hour
	spiceSessionCheckInterval = 25 * time.Second
	spiceSessionSetupTimeout  = 20 * time.Second
	spiceSessionNoMainGrace   = 60 * time.Second
)

// ErrSpiceConsoleLimit mirrors `new Error('CONSOLE_LIMIT')`; the route maps it to
// HTTP 409 with code CONSOLE_LIMIT.
var ErrSpiceConsoleLimit = errors.New("CONSOLE_LIMIT")

// SpiceAccess mirrors the SpiceAccess interface. HostID / InstanceName are set by
// the route after the permission check and bound the ticket to that host/instance.
type SpiceAccess struct {
	UserID       int32
	InstanceID   int32
	IssuedAt     int64
	SessionID    *string
	HostID       *int32
	InstanceName *string
}

type spiceOpenResult struct {
	done    chan struct{}
	backend *SpiceBackend
	err     error
}

type spiceSession struct {
	SpiceAccess
	token     string
	createdAt time.Time

	mu      sync.Mutex
	backend *SpiceBackend
	opening *spiceOpenResult

	ctx    context.Context
	cancel context.CancelFunc

	channels     map[*WebSocket]struct{}
	main         *WebSocket
	stopCheck    chan struct{}
	stopOnce     sync.Once
	checking     atomic.Bool
	controlAlive atomic.Bool

	validate func(ctx context.Context) bool
}

func (s *spiceSession) access() SpiceAccess { return s.SpiceAccess }

func (s *spiceSession) getBackend() *SpiceBackend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backend
}

func (s *spiceSession) setBackend(backend *SpiceBackend) {
	s.mu.Lock()
	s.backend = backend
	s.mu.Unlock()
}

func (s *spiceSession) getOpening() *spiceOpenResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opening
}

// SpiceSessionManager mirrors the class. The zero value is not usable; call
// NewSpiceSessionManager.
type SpiceSessionManager struct {
	mu            sync.Mutex
	sessions      map[string]*spiceSession
	closing       map[int32]chan struct{}
	failedCleanup map[int32]*SpiceBackend
	reconnecting  map[int32]bool
}

// NewSpiceSessionManager mirrors `new SpiceSessionManager()`.
func NewSpiceSessionManager() *SpiceSessionManager {
	return &SpiceSessionManager{
		sessions:      map[string]*spiceSession{},
		closing:       map[int32]chan struct{}{},
		failedCleanup: map[int32]*SpiceBackend{},
		reconnecting:  map[int32]bool{},
	}
}

// SpiceSessions is the process-wide manager (`export const spiceSessions`).
var SpiceSessions = NewSpiceSessionManager()

func spiceSessionToken() string {
	buf := make([]byte, 36) // 36 bytes → exactly 48 URL-safe characters, nanoid(48)
	if _, err := rand.Read(buf); err != nil {
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)[:36]))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// WaitForCleanup mirrors waitForCleanup(instanceId): wait for an in-flight close,
// then retry a failed backend cleanup once.
func (m *SpiceSessionManager) WaitForCleanup(instanceID int32) {
	m.mu.Lock()
	done := m.closing[instanceID]
	m.mu.Unlock()
	if done != nil {
		<-done
	}

	m.mu.Lock()
	failed := m.failedCleanup[instanceID]
	m.mu.Unlock()
	if failed != nil {
		_ = failed.Close()
		m.mu.Lock()
		if m.failedCleanup[instanceID] == failed {
			delete(m.failedCleanup, instanceID)
		}
		m.mu.Unlock()
	}
}

// Create mirrors create(access, open, validate, reconnect = false). The session
// lifetime is deliberately NOT derived from a request context: it must outlive the
// HTTP request that created it, exactly like the TypeScript AbortController.
func (m *SpiceSessionManager) Create(
	access SpiceAccess,
	open func(context.Context) (*SpiceBackend, error),
	validate func(context.Context) bool,
	reconnect bool,
) (string, error) {
	m.mu.Lock()
	if m.reconnecting[access.InstanceID] {
		m.mu.Unlock()
		return "", ErrSpiceConsoleLimit
	}
	if reconnect {
		m.reconnecting[access.InstanceID] = true
	}
	m.mu.Unlock()

	if reconnect {
		defer func() {
			m.mu.Lock()
			delete(m.reconnecting, access.InstanceID)
			m.mu.Unlock()
		}()
		m.CloseMatching(func(session SpiceAccess) bool { return session.InstanceID == access.InstanceID })
		m.WaitForCleanup(access.InstanceID)
	}

	return m.createSession(access, open, validate)
}

// createSession mirrors the private createSession.
func (m *SpiceSessionManager) createSession(
	access SpiceAccess,
	open func(context.Context) (*SpiceBackend, error),
	validate func(context.Context) bool,
) (string, error) {
	m.mu.Lock()
	_, closing := m.closing[access.InstanceID]
	_, failed := m.failedCleanup[access.InstanceID]
	m.mu.Unlock()
	if closing || failed {
		m.WaitForCleanup(access.InstanceID)
	}

	m.mu.Lock()
	reserved := map[int32]struct{}{}
	userSessions := 0
	instanceSessions := 0
	for _, session := range m.sessions {
		reserved[session.InstanceID] = struct{}{}
		if session.UserID == access.UserID {
			userSessions++
		}
		if session.InstanceID == access.InstanceID {
			instanceSessions++
		}
	}
	for instanceID := range m.closing {
		reserved[instanceID] = struct{}{}
	}
	for instanceID := range m.failedCleanup {
		reserved[instanceID] = struct{}{}
	}
	if len(reserved) >= 100 || userSessions >= 2 || instanceSessions > 0 {
		m.mu.Unlock()
		return "", ErrSpiceConsoleLimit
	}

	token := spiceSessionToken()
	sessionCtx, cancel := context.WithCancel(context.Background())
	session := &spiceSession{
		SpiceAccess: access,
		token:       token,
		createdAt:   time.Now().UTC(),
		ctx:         sessionCtx,
		cancel:      cancel,
		channels:    map[*WebSocket]struct{}{},
		stopCheck:   make(chan struct{}),
		validate:    validate,
	}
	session.controlAlive.Store(true)
	// Reserve BEFORE asynchronous setup; pending sessions count towards both limits.
	m.sessions[token] = session
	m.mu.Unlock()

	// The check loop mirrors `setInterval(() => void this.check(session), 25_000).unref()`.
	go m.runCheckLoop(session)

	setupTimer := time.AfterFunc(spiceSessionSetupTimeout, func() {
		go m.Close(token)
	})

	opening := &spiceOpenResult{done: make(chan struct{})}
	session.mu.Lock()
	session.opening = opening
	session.mu.Unlock()

	go func() {
		backend, err := open(session.ctx)
		opening.backend = backend
		opening.err = err
		close(opening.done)
	}()

	var backend *SpiceBackend
	select {
	case <-opening.done:
		backend = opening.backend
		if opening.err != nil {
			setupTimer.Stop()
			go m.Close(token)
			return "", opening.err
		}
	case <-sessionCtx.Done():
		setupTimer.Stop()
		go m.Close(token)
		return "", errors.New("Console setup aborted")
	}

	if !m.has(token) {
		setupTimer.Stop()
		go m.Close(token)
		return "", errors.New("Console closed during setup")
	}
	if backend == nil || backend.Control() == nil || !backend.Control().IsOpen() {
		setupTimer.Stop()
		go m.Close(token)
		return "", errors.New("Console control channel closed during setup")
	}

	session.setBackend(backend)
	backend.Control().OnClose(func() { go m.Close(token) })
	backend.Control().SetPongHandler(func() { session.controlAlive.Store(true) })
	setupTimer.Stop()
	return token, nil
}

func (m *SpiceSessionManager) has(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[token]
	return ok
}

// Get mirrors get(token, instanceId): the access record, or nil when the token is
// unknown, bound to another instance, or older than the lifetime.
func (m *SpiceSessionManager) Get(token string, instanceID int32) *SpiceAccess {
	m.mu.Lock()
	session := m.sessions[token]
	m.mu.Unlock()
	if session == nil || session.InstanceID != instanceID {
		return nil
	}
	if time.Since(session.createdAt) > SpiceSessionLifetime {
		return nil
	}
	access := session.access()
	return &access
}

// Has mirrors a membership test (used by tests and the attach path).
func (m *SpiceSessionManager) Has(token string) bool { return m.has(token) }

// ExpireSpiceSessionForTest ages a session past SpiceSessionLifetime (tests only).
func (m *SpiceSessionManager) ExpireSpiceSessionForTest(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session := m.sessions[token]; session != nil {
		session.createdAt = time.Now().UTC().Add(-SpiceSessionLifetime - time.Second)
	}
}

// Count returns the number of live sessions (tests only).
func (m *SpiceSessionManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// CheckSession runs one validation tick for a token (exported for tests). The
// production path is the 25 s loop started by createSession.
func (m *SpiceSessionManager) CheckSession(token string) {
	m.mu.Lock()
	session := m.sessions[token]
	m.mu.Unlock()
	if session == nil {
		return
	}
	m.check(session)
}

func (m *SpiceSessionManager) runCheckLoop(session *spiceSession) {
	ticker := time.NewTicker(spiceSessionCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-session.stopCheck:
			return
		case <-session.ctx.Done():
			return
		case <-ticker.C:
			m.check(session)
		}
	}
}

// check mirrors the private check(session).
func (m *SpiceSessionManager) check(session *spiceSession) {
	age := time.Since(session.createdAt)
	session.mu.Lock()
	hasMain := session.main != nil
	session.mu.Unlock()
	if age > SpiceSessionLifetime || (!hasMain && age > spiceSessionNoMainGrace) {
		go m.Close(session.token)
		return
	}
	if !session.checking.CompareAndSwap(false, true) {
		return
	}
	defer session.checking.Store(false)

	alive := session.controlAlive.Load()
	validateCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	valid := session.validate != nil && session.validate(validateCtx)
	cancel()
	if !alive || !valid {
		go m.Close(session.token)
		return
	}
	session.controlAlive.Store(false)
	if backend := session.getBackend(); backend != nil && backend.Control() != nil {
		_ = backend.Control().Ping()
	}
}

// SpiceChannel is one browser WebSocket attached to a session.
type SpiceChannel struct {
	manager     *SpiceSessionManager
	token       string
	session     *spiceSession
	socket      *WebSocket
	isMain      bool
	cleanupOnce sync.Once
	closed      atomic.Bool

	mu           sync.Mutex
	backend      *WebSocket
	pending      [][]byte
	pendingBytes int64
	alive        bool

	stopHeartbeat chan struct{}
	abort         context.CancelFunc
	ctx           context.Context
}

// Attach mirrors attach(token, instanceId, socket, authorize).
//
// The message/close handlers are installed synchronously, just like the
// TypeScript, so an early browser message is buffered rather than lost while the
// asynchronous authorisation and the data channel are opening.
func (m *SpiceSessionManager) Attach(token string, instanceID int32, socket *WebSocket, authorize func(main bool) bool) {
	m.mu.Lock()
	session := m.sessions[token]
	m.mu.Unlock()
	if session == nil || m.Get(token, instanceID) == nil {
		socket.Close(4003, "Console session unavailable")
		return
	}
	backend := session.getBackend()
	if backend == nil {
		socket.Close(4003, "Console session unavailable")
		return
	}

	session.mu.Lock()
	if len(session.channels) >= SpiceMaxChannels {
		session.mu.Unlock()
		socket.Close(4003, "Console session unavailable")
		return
	}
	isMain := session.main == nil
	if isMain {
		session.main = socket
	}
	session.channels[socket] = struct{}{}
	session.mu.Unlock()

	channelCtx, cancel := context.WithCancel(context.Background())
	channel := &SpiceChannel{
		manager:       m,
		token:         token,
		session:       session,
		socket:        socket,
		isMain:        isMain,
		alive:         true,
		abort:         cancel,
		ctx:           channelCtx,
		stopHeartbeat: make(chan struct{}),
	}

	terminate := func() { channel.terminate() }

	timeout := time.AfterFunc(spiceSessionSetupTimeout, terminate)
	go channel.heartbeat()

	socket.OnClose(func() { channel.cleanup() })
	socket.SetPongHandler(func() {
		channel.mu.Lock()
		channel.alive = true
		channel.mu.Unlock()
	})

	// Client → Incus (buffered until the data channel exists).
	go func() {
		for {
			payload, isBinary, err := socket.ReadMessage()
			if err != nil {
				return
			}
			size := int64(len(payload))
			if !isBinary || size > SpiceMaxBuffer {
				terminate()
				return
			}
			channel.mu.Lock()
			backendSocket := channel.backend
			if backendSocket == nil {
				channel.pendingBytes += size
				if channel.pendingBytes > SpiceMaxBuffer {
					channel.mu.Unlock()
					terminate()
					return
				}
				channel.pending = append(channel.pending, payload)
				channel.mu.Unlock()
				continue
			}
			channel.mu.Unlock()
			if backendSocket.IsOpen() && backendSocket.BufferedAmount() < SpiceMaxBuffer {
				if err := backendSocket.SendBinary(payload); err != nil {
					terminate()
					return
				}
			} else {
				terminate()
				return
			}
		}
	}()

	// Asynchronous authorisation, then the data channel.
	go func() {
		if !authorize(isMain) || channel.closed.Load() || !m.has(token) {
			terminate()
			return
		}
		upstream, err := backend.OpenData(channel.ctx)
		if err != nil {
			terminate()
			return
		}
		if channel.closed.Load() || !m.has(token) || !socket.IsOpen() {
			upstream.Terminate()
			terminate()
			return
		}
		channel.mu.Lock()
		channel.backend = upstream
		pending := channel.pending
		channel.pending = nil
		channel.pendingBytes = 0
		channel.mu.Unlock()

		upstream.OnClose(terminate)
		upstream.SetPongHandler(func() {
			channel.mu.Lock()
			channel.alive = true
			channel.mu.Unlock()
		})
		go func() {
			for {
				payload, isBinary, err := upstream.ReadMessage()
				if err != nil {
					terminate()
					return
				}
				if !isBinary || !socket.IsOpen() || socket.BufferedAmount() > SpiceMaxBuffer {
					terminate()
					return
				}
				if err := socket.SendBinary(payload); err != nil {
					terminate()
					return
				}
			}
		}()
		for _, data := range pending {
			_ = upstream.SendBinary(data)
		}
		timeout.Stop()
	}()
}

func (c *SpiceChannel) heartbeat() {
	ticker := time.NewTicker(spiceSessionCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopHeartbeat:
			return
		case <-ticker.C:
			c.mu.Lock()
			alive := c.alive
			backend := c.backend
			c.alive = false
			c.mu.Unlock()
			if !alive || c.socket.BufferedAmount() > SpiceMaxBuffer ||
				(backend != nil && backend.BufferedAmount() > SpiceMaxBuffer) {
				c.terminate()
				return
			}
			_ = c.socket.Ping()
			if backend != nil {
				_ = backend.Ping()
			}
		}
	}
}

// terminate mirrors `socket.terminate(); cleanup()`.
func (c *SpiceChannel) terminate() {
	c.socket.Terminate()
	c.cleanup()
}

func (c *SpiceChannel) cleanup() {
	c.cleanupOnce.Do(func() {
		c.closed.Store(true)
		c.abort()
		close(c.stopHeartbeat)
		c.mu.Lock()
		c.pending = nil
		c.pendingBytes = 0
		backend := c.backend
		c.mu.Unlock()
		if backend != nil {
			backend.Terminate()
		}

		c.session.mu.Lock()
		delete(c.session.channels, c.socket)
		c.session.mu.Unlock()

		if c.isMain {
			go c.manager.Close(c.token)
		}
	})
}

// Close mirrors close(token); the returned channel is closed when the backend has
// been disposed (the TypeScript Promise).
func (m *SpiceSessionManager) Close(token string) <-chan struct{} {
	m.mu.Lock()
	session := m.sessions[token]
	if session == nil {
		m.mu.Unlock()
		done := make(chan struct{})
		close(done)
		return done
	}
	delete(m.sessions, token)
	done := make(chan struct{})
	m.closing[session.InstanceID] = done
	m.mu.Unlock()

	session.cancel()
	session.stopOnce.Do(func() { close(session.stopCheck) })

	session.mu.Lock()
	channels := make([]*WebSocket, 0, len(session.channels))
	for socket := range session.channels {
		channels = append(channels, socket)
	}
	session.mu.Unlock()
	for _, socket := range channels {
		socket.Close(1000, "Console closed")
	}

	go func() {
		defer func() {
			m.mu.Lock()
			if m.closing[session.InstanceID] == done {
				delete(m.closing, session.InstanceID)
			}
			m.mu.Unlock()
			close(done)
		}()

		// Keep the instance reserved until even a late setup result has been disposed.
		backend := session.getBackend()
		if backend == nil {
			if opening := session.getOpening(); opening != nil {
				<-opening.done
				if opening.err != nil {
					return
				}
				backend = opening.backend
			}
		}
		if backend == nil {
			return
		}
		if err := backend.Close(); err != nil {
			m.mu.Lock()
			m.failedCleanup[session.InstanceID] = backend
			m.mu.Unlock()
		}
	}()
	return done
}

// CloseMatching mirrors closeMatching(match): close every matching session and
// report how many.
func (m *SpiceSessionManager) CloseMatching(match func(SpiceAccess) bool) int {
	m.mu.Lock()
	tokens := make([]string, 0, len(m.sessions))
	for token, session := range m.sessions {
		if match(session.access()) {
			tokens = append(tokens, token)
		}
	}
	m.mu.Unlock()
	for _, token := range tokens {
		m.Close(token)
	}
	return len(tokens)
}

// Drain closes every session and waits for their backend cleanup (onClose hook).
func (m *SpiceSessionManager) Drain() {
	m.CloseMatching(func(SpiceAccess) bool { return true })
	m.mu.Lock()
	pending := make([]chan struct{}, 0, len(m.closing))
	for _, done := range m.closing {
		pending = append(pending, done)
	}
	m.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}
