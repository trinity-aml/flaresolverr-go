package browser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakePage records the pipeline's calls in order so tests can assert on the
// sequence, which is what actually matters here: several steps are ordered the
// way they are for non-obvious reasons.
type fakePage struct {
	calls []string

	title        string
	deniedSel    map[string]bool
	challenge    bool
	challengeErr error
	solveErr     error
	html         string
	url          string
	ua           string
	cookies      []Cookie
	docResp      DocumentResponse
	docRespErr   error
	logHTML      bool
	mediaBlocked []bool
	turnstile    string
	navCount     int
}

func newFakePage() *fakePage {
	return &fakePage{
		title:     "Example",
		deniedSel: map[string]bool{},
		html:      "<html>ok</html>",
		url:       "https://example.com/",
		ua:        "UA/1.0",
		cookies:   []Cookie{{Name: "c", Value: "1"}},
	}
}

func (p *fakePage) record(name string) { p.calls = append(p.calls, name) }

func (p *fakePage) SetMediaBlocked(_ context.Context, blocked bool) error {
	p.record("SetMediaBlocked")
	p.mediaBlocked = append(p.mediaBlocked, blocked)
	return nil
}
func (p *fakePage) Navigate(context.Context, Request) error {
	p.record("Navigate")
	p.navCount++
	return nil
}
func (p *fakePage) SetPageCookies(context.Context, string, []Cookie) error {
	p.record("SetPageCookies")
	return nil
}
func (p *fakePage) Title(context.Context) (string, error) {
	p.record("Title")
	return p.title, nil
}
func (p *fakePage) SelectorExists(_ context.Context, sel string) (bool, error) {
	p.record("SelectorExists")
	return p.deniedSel[sel], nil
}
func (p *fakePage) HTML(context.Context) (string, error) {
	p.record("HTML")
	return p.html, nil
}
func (p *fakePage) CurrentURL(context.Context) (string, error) {
	p.record("CurrentURL")
	return p.url, nil
}
func (p *fakePage) PageUserAgent(context.Context) (string, error) {
	p.record("PageUserAgent")
	return p.ua, nil
}
func (p *fakePage) PageCookies(context.Context, string) ([]Cookie, error) {
	p.record("PageCookies")
	return p.cookies, nil
}
func (p *fakePage) Screenshot(context.Context) (string, error) {
	p.record("Screenshot")
	return "c2hvdA==", nil
}
func (p *fakePage) ChallengePresent(context.Context) (bool, error) {
	p.record("ChallengePresent")
	return p.challenge, p.challengeErr
}
func (p *fakePage) SolveChallenge(context.Context) error {
	p.record("SolveChallenge")
	return p.solveErr
}
func (p *fakePage) DocumentResponse(context.Context, string) (DocumentResponse, error) {
	p.record("DocumentResponse")
	return p.docResp, p.docRespErr
}
func (p *fakePage) ApplyTurnstileToken(_ context.Context, _ Request, result *ChallengeResolutionResult) error {
	p.record("ApplyTurnstileToken")
	result.TurnstileToken = p.turnstile
	return nil
}
func (p *fakePage) PageLogger() Logger      { return discardLogger{} }
func (p *fakePage) LogHTMLConfigured() bool { return p.logHTML }

type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}

func indexOf(calls []string, name string) int { return slices.Index(calls, name) }

func TestResolvePageHappyPath(t *testing.T) {
	p := newFakePage()
	p.docResp = DocumentResponse{Status: 418, Headers: map[string]string{"x": "y"}}

	result, message, err := ResolvePage(context.Background(), p, Request{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if message != "Challenge not detected!" {
		t.Errorf("message = %q", message)
	}
	if result.URL != "https://example.com/" || result.UserAgent != "UA/1.0" {
		t.Errorf("snapshot wrong: %+v", result)
	}
	if result.Status != 418 || result.Headers["x"] != "y" {
		t.Errorf("document response not applied: status=%d headers=%v", result.Status, result.Headers)
	}
	if result.Response != "<html>ok</html>" {
		t.Errorf("response = %q", result.Response)
	}
	if len(result.Cookies) != 1 {
		t.Errorf("cookies = %v", result.Cookies)
	}
}

// The block list must be applied on every request, including when media is
// allowed — otherwise disableMedia:true sticks for the rest of a session.
func TestResolvePageAlwaysAppliesMediaBlockList(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		p := newFakePage()
		if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u", DisableMedia: blocked}); err != nil {
			t.Fatalf("ResolvePage: %v", err)
		}
		if len(p.mediaBlocked) != 1 || p.mediaBlocked[0] != blocked {
			t.Errorf("DisableMedia=%v: SetMediaBlocked calls = %v", blocked, p.mediaBlocked)
		}
		if i := indexOf(p.calls, "SetMediaBlocked"); i != 0 {
			t.Errorf("SetMediaBlocked must run first, got index %d in %v", i, p.calls)
		}
	}
}

// Supplying cookies must set them and re-navigate so the page loads with them.
func TestResolvePageRenavigatesAfterCookies(t *testing.T) {
	p := newFakePage()
	_, _, err := ResolvePage(context.Background(), p, Request{
		URL:     "https://example.com",
		Cookies: []Cookie{{Name: "a", Value: "1"}},
	})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if p.navCount != 2 {
		t.Errorf("navigate called %d times, want 2 (initial + reload after cookies)", p.navCount)
	}
	if indexOf(p.calls, "SetPageCookies") > indexOf(p.calls, "Title") {
		t.Error("cookies must be set before the access-denied check")
	}
}

func TestResolvePageSkipsCookieReloadWhenNoCookies(t *testing.T) {
	p := newFakePage()
	if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u"}); err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if p.navCount != 1 {
		t.Errorf("navigate called %d times, want 1", p.navCount)
	}
	if slices.Contains(p.calls, "SetPageCookies") {
		t.Error("SetPageCookies must not be called without cookies")
	}
}

func TestResolvePageDetectsAccessDeniedByTitle(t *testing.T) {
	p := newFakePage()
	p.title = AccessDeniedTitles[0] + " — extra"

	_, _, err := ResolvePage(context.Background(), p, Request{URL: "u"})
	if err == nil {
		t.Fatal("expected an access-denied error")
	}
	if !strings.Contains(err.Error(), "Cloudflare has blocked this request") {
		t.Errorf("err = %q", err)
	}
	if slices.Contains(p.calls, "ChallengePresent") {
		t.Error("must abort before challenge detection")
	}
}

func TestResolvePageDetectsAccessDeniedBySelector(t *testing.T) {
	p := newFakePage()
	p.deniedSel[AccessDeniedSelectors[0]] = true

	_, _, err := ResolvePage(context.Background(), p, Request{URL: "u"})
	if err == nil || !strings.Contains(err.Error(), "Cloudflare has blocked this request") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePageSolvesChallenge(t *testing.T) {
	p := newFakePage()
	p.challenge = true

	_, message, err := ResolvePage(context.Background(), p, Request{URL: "u"})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if message != "Challenge solved!" {
		t.Errorf("message = %q, want %q", message, "Challenge solved!")
	}
	if !slices.Contains(p.calls, "SolveChallenge") {
		t.Error("SolveChallenge was not called")
	}
}

func TestResolvePageDoesNotSolveWhenAbsent(t *testing.T) {
	p := newFakePage()
	if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u"}); err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if slices.Contains(p.calls, "SolveChallenge") {
		t.Error("SolveChallenge must not run when no challenge is present")
	}
}

func TestResolvePagePropagatesSolveError(t *testing.T) {
	p := newFakePage()
	p.challenge = true
	p.solveErr = errors.New("boom")

	if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u"}); err == nil {
		t.Fatal("expected the solve error to propagate")
	}
}

// The snapshot must come after the challenge is solved — reading the URL or the
// cookie jar earlier catches the data: page for request.post.
func TestResolvePageSnapshotsAfterSolving(t *testing.T) {
	p := newFakePage()
	p.challenge = true

	if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u"}); err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	solve := indexOf(p.calls, "SolveChallenge")
	for _, after := range []string{"CurrentURL", "PageCookies", "PageUserAgent"} {
		if indexOf(p.calls, after) < solve {
			t.Errorf("%s must be read after SolveChallenge; calls: %v", after, p.calls)
		}
	}
}

func TestResolvePageReturnOnlyCookiesSkipsHTML(t *testing.T) {
	p := newFakePage()
	result, _, err := ResolvePage(context.Background(), p, Request{URL: "u", ReturnOnlyCookies: true})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if result.Response != "" {
		t.Errorf("response must be empty, got %q", result.Response)
	}
	if slices.Contains(p.calls, "HTML") {
		t.Error("HTML must not be read for returnOnlyCookies")
	}
}

// req.LogHTML forces an HTML read even when the caller does not want it back.
func TestResolvePageLogHTMLReadsHTMLEarly(t *testing.T) {
	p := newFakePage()
	_, _, err := ResolvePage(context.Background(), p, Request{URL: "u", LogHTML: true, ReturnOnlyCookies: true})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if !slices.Contains(p.calls, "HTML") {
		t.Error("LogHTML must trigger an HTML read")
	}
	if indexOf(p.calls, "HTML") > indexOf(p.calls, "Title") {
		t.Error("the LogHTML read happens before the access-denied check")
	}
}

// The configured log_html must have the same effect as the per-request flag.
// geckodriver used to honour neither.
func TestResolvePageConfiguredLogHTML(t *testing.T) {
	p := newFakePage()
	p.logHTML = true
	if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u", ReturnOnlyCookies: true}); err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if !slices.Contains(p.calls, "HTML") {
		t.Error("LogHTMLConfigured must trigger an HTML read")
	}
}

func TestResolvePageScreenshot(t *testing.T) {
	p := newFakePage()
	result, _, err := ResolvePage(context.Background(), p, Request{URL: "u", ReturnScreenshot: true})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if result.Screenshot != "c2hvdA==" {
		t.Errorf("screenshot = %q", result.Screenshot)
	}
}

func TestResolvePageNoScreenshotByDefault(t *testing.T) {
	p := newFakePage()
	if _, _, err := ResolvePage(context.Background(), p, Request{URL: "u"}); err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if slices.Contains(p.calls, "Screenshot") {
		t.Error("Screenshot must not be captured unless asked for")
	}
}

// A backend with no way to read the status (geckodriver) returns a zero value
// and the 200 default must stand.
func TestResolvePageKeepsDefaultStatusWhenUnavailable(t *testing.T) {
	p := newFakePage()
	result, _, err := ResolvePage(context.Background(), p, Request{URL: "u"})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if result.Status != 200 {
		t.Errorf("status = %d, want the 200 default", result.Status)
	}
}

// A failure to read the real status is non-fatal.
func TestResolvePageDocumentResponseErrorIsNonFatal(t *testing.T) {
	p := newFakePage()
	p.docRespErr = errors.New("no perf log")

	result, _, err := ResolvePage(context.Background(), p, Request{URL: "u"})
	if err != nil {
		t.Fatalf("a document-response error must not fail the request: %v", err)
	}
	if result.Status != 200 {
		t.Errorf("status = %d, want 200", result.Status)
	}
}

func TestResolvePageAppliesTurnstileToken(t *testing.T) {
	p := newFakePage()
	p.turnstile = "tok"

	result, _, err := ResolvePage(context.Background(), p, Request{URL: "u"})
	if err != nil {
		t.Fatalf("ResolvePage: %v", err)
	}
	if result.TurnstileToken != "tok" {
		t.Errorf("token = %q", result.TurnstileToken)
	}
}

func TestResolvePageRespectsCancelledContext(t *testing.T) {
	p := newFakePage()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := ResolvePage(ctx, p, Request{URL: "u", WaitInSeconds: 5}); err == nil {
		t.Error("expected the cancelled context to abort the wait")
	}
}
