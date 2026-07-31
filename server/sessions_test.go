package flaresolverr

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBrowser records whether it was closed, so tests can assert that no
// browser is ever leaked.
type fakeBrowser struct {
	mu       sync.Mutex
	closed   int
	closeErr error
}

func (f *fakeBrowser) UserAgent(context.Context) (string, error) { return "fake-ua", nil }

func (f *fakeBrowser) Resolve(context.Context, browserRequest) (*browserResult, error) {
	return &browserResult{}, nil
}

func (f *fakeBrowser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func (f *fakeBrowser) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeFactory optionally blocks inside New to emulate a slow browser launch
// (Xvfb start, driver download, Chrome cold start).
type fakeFactory struct {
	mu       sync.Mutex
	created  []*fakeBrowser
	delay    time.Duration
	err      error
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (f *fakeFactory) New(ctx context.Context, _ Config, _ *Proxy) (browserClient, error) {
	n := f.inFlight.Add(1)
	for {
		seen := f.maxSeen.Load()
		if n <= seen || f.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}

	b := &fakeBrowser{}
	f.mu.Lock()
	f.created = append(f.created, b)
	f.mu.Unlock()
	return b, nil
}

func (f *fakeFactory) all() []*fakeBrowser {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeBrowser(nil), f.created...)
}

func newTestStore(factory browserFactory) *sessionStore {
	return newSessionStore(PrepareConfig(Config{}), factory, nil)
}

func TestSessionStoreCreateAndReuse(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	first, fresh, err := store.create(context.Background(), "s1", nil, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !fresh {
		t.Error("the first create must report a fresh session")
	}

	second, fresh, err := store.create(context.Background(), "s1", nil, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fresh {
		t.Error("the second create must reuse the existing session")
	}
	if first != second {
		t.Error("expected the same *session back")
	}
	if got := len(factory.all()); got != 1 {
		t.Errorf("expected exactly 1 browser to be built, got %d", got)
	}
}

func TestSessionStoreForceNewClosesTheOldBrowser(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	if _, _, err := store.create(context.Background(), "s1", nil, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := store.create(context.Background(), "s1", nil, true); err != nil {
		t.Fatalf("forced create: %v", err)
	}

	browsers := factory.all()
	if len(browsers) != 2 {
		t.Fatalf("expected 2 browsers, got %d", len(browsers))
	}
	if browsers[0].closeCount() != 1 {
		t.Errorf("the replaced browser must be closed exactly once, got %d", browsers[0].closeCount())
	}
	if browsers[1].closeCount() != 0 {
		t.Error("the replacement must stay open")
	}
}

func TestSessionStoreCreateFailureLeavesNoEntry(t *testing.T) {
	factory := &fakeFactory{err: errors.New("boom")}
	store := newTestStore(factory)

	if _, _, err := store.create(context.Background(), "s1", nil, false); err == nil {
		t.Fatal("expected create to fail")
	}
	if ids := store.ids(); len(ids) != 0 {
		t.Errorf("a failed create must not leave a session behind, got %v", ids)
	}

	// A later successful create for the same id must still work.
	factory.err = nil
	if _, fresh, err := store.create(context.Background(), "s1", nil, false); err != nil || !fresh {
		t.Errorf("retry after failure: fresh=%v err=%v", fresh, err)
	}
}

// The store lock must not be held across factory.New — otherwise a slow browser
// launch blocks every unrelated session operation.
func TestSessionStoreCreateDoesNotBlockOtherOperations(t *testing.T) {
	factory := &fakeFactory{delay: 300 * time.Millisecond}
	store := newTestStore(factory)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := store.create(context.Background(), "slow", nil, false); err != nil {
			t.Errorf("create: %v", err)
		}
	}()

	// Give the goroutine time to enter factory.New.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	_ = store.ids()
	_ = store.destroy("nonexistent")
	store.applyConfig(PrepareConfig(Config{}))
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("store operations blocked for %v behind a browser launch; the store lock is held across factory.New", elapsed)
	}

	<-done
}

// Concurrent creates for distinct ids must run in parallel, not serialize.
func TestSessionStoreConcurrentCreatesRunInParallel(t *testing.T) {
	factory := &fakeFactory{delay: 100 * time.Millisecond}
	store := newTestStore(factory)

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			if _, _, err := store.create(context.Background(), id, nil, false); err != nil {
				t.Errorf("create %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	if got := factory.maxSeen.Load(); got < 2 {
		t.Errorf("max concurrent factory.New calls = %d; creates are serialized behind the store lock", got)
	}
}

// Concurrent creates for the SAME id must build exactly one browser.
func TestSessionStoreConcurrentCreatesSameIDBuildOneBrowser(t *testing.T) {
	factory := &fakeFactory{delay: 50 * time.Millisecond}
	store := newTestStore(factory)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.create(context.Background(), "same", nil, false); err != nil {
				t.Errorf("create: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(factory.all()); got != 1 {
		t.Errorf("expected 1 browser for 8 concurrent creates of one id, got %d", got)
	}
}

// A session destroyed while it is still being constructed must still have its
// browser closed — otherwise the process leaks a Chrome and its scratch dir.
func TestSessionStoreDestroyDuringCreationClosesTheBrowser(t *testing.T) {
	factory := &fakeFactory{delay: 200 * time.Millisecond}
	store := newTestStore(factory)

	go func() {
		// Ignore the error: losing the race is the expected outcome here.
		_, _, _ = store.create(context.Background(), "racy", nil, false)
	}()

	time.Sleep(50 * time.Millisecond)
	store.destroyAll()

	// destroyAll waits for construction to finish before closing.
	browsers := factory.all()
	if len(browsers) != 1 {
		t.Fatalf("expected 1 browser to have been built, got %d", len(browsers))
	}
	if browsers[0].closeCount() != 1 {
		t.Errorf("browser closed %d times, want exactly 1 — a session destroyed mid-creation leaked", browsers[0].closeCount())
	}
	if ids := store.ids(); len(ids) != 0 {
		t.Errorf("store should be empty, got %v", ids)
	}
}

func TestSessionStoreDestroyIsIdempotentAndClosesOnce(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	if _, _, err := store.create(context.Background(), "s1", nil, false); err != nil {
		t.Fatalf("create: %v", err)
	}

	if !store.destroy("s1") {
		t.Error("the first destroy must report success")
	}
	if store.destroy("s1") {
		t.Error("the second destroy must report that there was nothing to remove")
	}

	if got := factory.all()[0].closeCount(); got != 1 {
		t.Errorf("browser closed %d times, want 1", got)
	}
}

// destroy() and a concurrent Resolve on the same session must not race, and the
// session must be observably closed afterwards.
func TestSessionStoreConcurrentDestroyAndUse(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	item, _, err := store.create(context.Background(), "s1", nil, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		store.destroy("s1")
	}()
	go func() {
		defer wg.Done()
		item.mu.Lock()
		defer item.mu.Unlock()
		// Reading item.closed under item.mu is the check resolveChallenge makes.
		_ = item.closed
	}()
	wg.Wait()

	item.mu.Lock()
	closed := item.closed
	item.mu.Unlock()
	if !closed {
		t.Error("the session must be marked closed after destroy")
	}
}

func TestSessionStoreGetRecreatesAfterTTL(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	first, _, err := store.get(context.Background(), "s1", time.Hour)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Backdate the session past the TTL.
	first.createdAt = time.Now().Add(-2 * time.Hour)

	second, _, err := store.get(context.Background(), "s1", time.Hour)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if first == second {
		t.Error("expected a session older than the TTL to be recreated")
	}
	if got := factory.all()[0].closeCount(); got != 1 {
		t.Errorf("the expired browser must be closed, closeCount = %d", got)
	}
}

func TestSessionStoreGetWithoutTTLKeepsSession(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	first, _, err := store.get(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	first.createdAt = time.Now().Add(-100 * time.Hour)

	second, _, err := store.get(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if first != second {
		t.Error("with no TTL the session must never be recreated")
	}
}

func TestSessionStoreIDsAreSorted(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	for _, id := range []string{"c", "a", "b"} {
		if _, _, err := store.create(context.Background(), id, nil, false); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	ids := store.ids()
	if len(ids) != 3 || ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Errorf("ids = %v, want [a b c]", ids)
	}
}

func TestSessionStoreCreateGeneratesID(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	item, _, err := store.create(context.Background(), "", nil, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if item.id == "" {
		t.Error("expected an id to be generated for an empty session id")
	}
}

func TestSessionStoreDestroyAllClosesEverything(t *testing.T) {
	factory := &fakeFactory{}
	store := newTestStore(factory)

	for _, id := range []string{"a", "b", "c"} {
		if _, _, err := store.create(context.Background(), id, nil, false); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	store.destroyAll()

	for i, b := range factory.all() {
		if b.closeCount() != 1 {
			t.Errorf("browser %d closed %d times, want 1", i, b.closeCount())
		}
	}
	if ids := store.ids(); len(ids) != 0 {
		t.Errorf("store not empty after destroyAll: %v", ids)
	}
}
