package flaresolverr

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// cfgRecordingFactory records the Config each browser was launched with.
type cfgRecordingFactory struct {
	mu    sync.Mutex
	seen  []Config
	inner fakeFactory
}

func (f *cfgRecordingFactory) New(ctx context.Context, cfg Config, proxy *Proxy) (browserClient, error) {
	f.mu.Lock()
	f.seen = append(f.seen, cfg)
	f.mu.Unlock()
	return f.inner.New(ctx, cfg, proxy)
}

func (f *cfgRecordingFactory) configs() []Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Config(nil), f.seen...)
}

// TestTheCachedUserAgentNeverBecomesALaunchFlag is the regression guard for the
// defect behind "it only works right after a restart".
//
// The store used to fill StartupUserAgent from the UA the *first* browser
// reported, which put `--user-agent=<that string>` on every later browser's
// command line. The value equalled what the browser reports natively, so it
// changed nothing about the UA and broke the cold solve outright. Measured on
// CloakBrowser 146 against bitru.org, first request of a fresh process, the flag
// the only difference between the two runs:
//
//	no --user-agent    13 s  Challenge solved!
//	   --user-agent    20 s  challenge still present
//
// So the first browser of a process solved and no later one could, and only a
// new Service — or a service restart — cleared the cache. This is the
// command-line twin of the CDP guard in the webdriver backend.
func TestTheCachedUserAgentNeverBecomesALaunchFlag(t *testing.T) {
	factory := &cfgRecordingFactory{}
	service := newService(Config{}, factory)
	t.Cleanup(func() { _ = service.Close() })

	ctx := context.Background()
	for _, id := range []string{"first", "second", "third"} {
		if _, _, err := service.sessions.create(ctx, id, nil, false); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		// Mimic the real pipeline, which caches the UA off the first browser.
		service.storeUserAgent("fake-ua")
	}

	for i, cfg := range factory.configs() {
		if strings.TrimSpace(cfg.StartupUserAgent) != "" {
			t.Errorf("browser %d was launched with StartupUserAgent=%q; a cached UA must never reach the command line", i+1, cfg.StartupUserAgent)
		}
	}
	if got := service.peekUserAgent(); got == "" {
		t.Error("the service stopped caching a user agent at all — this test would then prove nothing")
	}
}

// TestAnExplicitStartupUserAgentIsStillHonoured keeps the fix from going too
// far: someone setting startup_user_agent (or STARTUP_USER_AGENT) is asking for
// a specific UA on purpose, and that still has to reach the browser.
func TestAnExplicitStartupUserAgentIsStillHonoured(t *testing.T) {
	const want = "Mozilla/5.0 (X11; Linux x86_64) TestAgent/1.0"
	factory := &cfgRecordingFactory{}
	service := newService(Config{StartupUserAgent: want}, factory)
	t.Cleanup(func() { _ = service.Close() })

	if _, _, err := service.sessions.create(context.Background(), "only", nil, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	configs := factory.configs()
	if len(configs) != 1 {
		t.Fatalf("expected one browser, got %d", len(configs))
	}
	if configs[0].StartupUserAgent != want {
		t.Errorf("explicit StartupUserAgent did not reach the browser: got %q, want %q", configs[0].StartupUserAgent, want)
	}
}
