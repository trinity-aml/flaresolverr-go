package flaresolverr

import (
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

type session struct {
	id        string
	browser   browserClient
	createdAt time.Time
	mu        sync.Mutex
}

func (s *session) lifetime() time.Duration {
	return time.Since(s.createdAt)
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

func (s *sessionStore) create(sessionID string, proxy *Proxy, forceNew bool) (*session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sessionID == "" {
		sessionID = newSessionID()
	}

	if forceNew {
		if existing, ok := s.sessions[sessionID]; ok {
			_ = existing.browser.Close()
			delete(s.sessions, sessionID)
		}
	}

	if existing, ok := s.sessions[sessionID]; ok {
		return existing, false, nil
	}

	cfg := s.cfg.withDefaults()
	if cfg.StartupUserAgent == "" && s.userAgentFn != nil {
		cfg.StartupUserAgent = s.userAgentFn()
	}

	browser, err := s.factory.New(cfg, proxy)
	if err != nil {
		return nil, false, fmt.Errorf("create browser session: %w", err)
	}

	item := &session{
		id:        sessionID,
		browser:   browser,
		createdAt: time.Now(),
	}
	s.sessions[sessionID] = item
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

func (s *sessionStore) destroy(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	item, ok := s.sessions[sessionID]
	if !ok {
		return false
	}
	_ = item.browser.Close()
	delete(s.sessions, sessionID)
	return true
}

func (s *sessionStore) destroyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, item := range s.sessions {
		_ = item.browser.Close()
		delete(s.sessions, key)
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
