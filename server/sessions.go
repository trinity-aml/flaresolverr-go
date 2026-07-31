package flaresolverr

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
)

type browserRequest = browserpkg.Request
type browserResult = browserpkg.Result
type browserClient = browserpkg.Client

type browserFactory interface {
	New(Config, *Proxy) (browserClient, error)
}

// errSessionClosed is returned when a session is destroyed between the lookup
// that handed it out and the moment the caller acquired it.
var errSessionClosed = errors.New("The session doesn't exist.")

type session struct {
	id        string
	createdAt time.Time

	// ready is closed once construction finished. browser and err are only
	// safe to read after a receive on it; the close establishes the
	// happens-before edge for both.
	ready   chan struct{}
	browser browserClient
	err     error

	mu     sync.Mutex // serializes Resolve calls against Close on this session
	closed bool
}

func (s *session) lifetime() time.Duration {
	return time.Since(s.createdAt)
}

// wait blocks until construction finished and reports its outcome.
func (s *session) wait() error {
	<-s.ready
	return s.err
}

// close is idempotent and safe to call concurrently with an in-flight
// construction: it waits for the browser to exist before closing it, so a
// session destroyed mid-creation still gets cleaned up rather than orphaned.
func (s *session) close() error {
	<-s.ready
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.browser == nil {
		return nil
	}
	return s.browser.Close()
}

type sessionStore struct {
	cfg         Config
	factory     browserFactory
	userAgentFn func() string
	mu          sync.RWMutex
	sessions    map[string]*session
}

func newSessionStore(cfg Config, factory browserFactory, userAgentFn func() string) *sessionStore {
	return &sessionStore{
		cfg:         cfg,
		factory:     factory,
		userAgentFn: userAgentFn,
		sessions:    make(map[string]*session),
	}
}

// create returns the session for sessionID, launching a browser if there isn't
// one yet.
//
// The store lock is deliberately *not* held across factory.New. Launching a
// browser means starting Xvfb, patching and spawning a driver and, on a cold
// cache, downloading a multi-megabyte driver archive over the network — holding
// the write lock for that blocks every other create/get/destroy/list on the
// store. Instead the map entry is reserved first, the lock released, and the
// browser built outside it; concurrent callers for the same id block on
// item.ready rather than on the store.
func (s *sessionStore) create(sessionID string, proxy *Proxy, forceNew bool) (*session, bool, error) {
	s.mu.Lock()

	if sessionID == "" {
		sessionID = newSessionID()
	}

	if forceNew {
		if existing, ok := s.sessions[sessionID]; ok {
			delete(s.sessions, sessionID)
			s.mu.Unlock()
			_ = existing.close()
			s.mu.Lock()
		}
	}

	if existing, ok := s.sessions[sessionID]; ok {
		s.mu.Unlock()
		if err := existing.wait(); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	item := &session{
		id:        sessionID,
		createdAt: time.Now(),
		ready:     make(chan struct{}),
	}
	s.sessions[sessionID] = item

	cfg := s.cfg.withDefaults()
	userAgentFn := s.userAgentFn
	s.mu.Unlock()

	if cfg.StartupUserAgent == "" && userAgentFn != nil {
		cfg.StartupUserAgent = userAgentFn()
	}

	browser, err := s.factory.New(cfg, proxy)
	item.browser = browser
	if err != nil {
		item.err = fmt.Errorf("create browser session: %w", err)
	}
	close(item.ready)

	if item.err != nil {
		s.mu.Lock()
		if s.sessions[sessionID] == item {
			delete(s.sessions, sessionID)
		}
		s.mu.Unlock()
		return nil, false, item.err
	}

	// destroy/destroyAll may have drained the store while we were building.
	// Their close() call is already waiting on item.ready, so the browser will
	// be shut down — we just must not hand the caller a dead session.
	s.mu.Lock()
	_, present := s.sessions[sessionID]
	s.mu.Unlock()
	if !present {
		_ = item.close()
		return nil, false, errSessionClosed
	}

	return item, true, nil
}

func (s *sessionStore) get(sessionID string, ttl time.Duration) (*session, bool, error) {
	item, fresh, err := s.create(sessionID, nil, false)
	if err != nil {
		return nil, false, err
	}
	if ttl <= 0 || fresh || item.lifetime() <= ttl {
		return item, fresh, nil
	}
	return s.create(sessionID, nil, true)
}

func (s *sessionStore) applyConfig(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

func (s *sessionStore) destroy(sessionID string) bool {
	s.mu.Lock()
	item, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.sessions, sessionID)
	s.mu.Unlock()

	_ = item.close()
	return true
}

func (s *sessionStore) destroyAll() {
	s.mu.Lock()
	items := make([]*session, 0, len(s.sessions))
	for key, item := range s.sessions {
		items = append(items, item)
		delete(s.sessions, key)
	}
	s.mu.Unlock()

	for _, item := range items {
		_ = item.close()
	}
}

func (s *sessionStore) ids() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.sessions))
	for key := range s.sessions {
		ids = append(ids, key)
	}
	sort.Strings(ids)
	return ids
}
