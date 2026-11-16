package flaresolverr

import (
	"context"
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
	New(context.Context, Config, *Proxy) (browserClient, error)
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
	cfg      Config
	factory  browserFactory
	mu       sync.RWMutex
	sessions map[string]*session
}

func newSessionStore(cfg Config, factory browserFactory) *sessionStore {
	return &sessionStore{
		cfg:      cfg,
		factory:  factory,
		sessions: make(map[string]*session),
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
func (s *sessionStore) create(ctx context.Context, sessionID string, proxy *Proxy, forceNew bool) (*session, bool, error) {
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
	s.mu.Unlock()

	// The cached user agent is deliberately NOT fed back in here, and it must
	// not be reintroduced.
	//
	// This block used to read the UA the first browser reported and hand it to
	// every later one, which put `--user-agent=<that string>` on their command
	// line. The value was identical to what the browser reports natively, so it
	// changed nothing about the UA — and broke the cold solve outright.
	//
	// Measured on CloakBrowser 146 against bitru.org, first request of a fresh
	// process, the flag the only difference:
	//
	//	no --user-agent    13 s  Challenge solved!
	//	   --user-agent    20 s  challenge still present
	//
	// That is the command-line twin of the trap the CDP path already guards
	// (`overrideUA == currentUA` in the webdriver backend, with its own note
	// about a no-op override being "the entire difference between clearing the
	// managed challenge in ~7s and looping on it forever"). Passing the flag
	// marks the UA as overridden whatever the value is, and the challenge reads
	// that.
	//
	// It is also the whole of "it only works right after a restart": the first
	// browser of a process had nothing cached and so launched clean, every one
	// after it carried the flag, and only a new Service — or a restart — cleared
	// the cache. An explicit StartupUserAgent from config or STARTUP_USER_AGENT
	// still applies: that is someone asking for a specific UA on purpose.

	browser, err := s.factory.New(ctx, cfg, proxy)
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

func (s *sessionStore) get(ctx context.Context, sessionID string, ttl time.Duration) (*session, bool, error) {
	item, fresh, err := s.create(ctx, sessionID, nil, false)
	if err != nil {
		return nil, false, err
	}
	if ttl <= 0 || fresh || item.lifetime() <= ttl {
		return item, fresh, nil
	}
	return s.create(ctx, sessionID, nil, true)
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
