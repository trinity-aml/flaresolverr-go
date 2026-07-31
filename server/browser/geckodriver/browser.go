// Package geckodriverbackend implements the browser.Client interface on top of
// geckodriver + Firefox (typically daijro/camoufox). Camoufox ships with
// randomised fingerprints, patched navigator.webdriver, TLS noise and WebRTC
// leak prevention, so we can drive it through the standard W3C WebDriver
// protocol without any CDP-specific stealth layer.
package geckodriverbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
	"github.com/trinity-aml/flaresolverr-go/server/browser/w3c"
)

type Config = browserpkg.Config
type Proxy = browserpkg.Proxy
type Cookie = browserpkg.Cookie
type ChallengeResolutionResult = browserpkg.ChallengeResolutionResult
type Logger = browserpkg.Logger
type Request = browserpkg.Request
type Result = browserpkg.Result
type Client = browserpkg.Client
type documentResponse = browserpkg.DocumentResponse

var (
	appendWithEnv         = browserpkg.AppendWithEnv
	buildPostFormHTML     = browserpkg.BuildPostFormHTML
	scrubUserAgent        = browserpkg.ScrubUserAgent
	firstCookiePath       = browserpkg.FirstCookiePath
	sleepContext          = browserpkg.SleepContext
	accessDeniedTitles    = browserpkg.AccessDeniedTitles
	accessDeniedSelectors = browserpkg.AccessDeniedSelectors
	challengeTitles       = browserpkg.ChallengeTitles
	challengeSelectors    = browserpkg.ChallengeSelectors
	createTransientDir    = browserpkg.CreateTransientDir
	firstNonEmpty         = browserpkg.FirstNonEmpty
)

type geckoBrowser struct {
	cfg    Config
	logger Logger
	proxy  *Proxy

	sess              *w3c.Session
	effectiveHeadless bool

	driver           *w3c.DriverProcess
	driverRuntimeDir string
	cachedUserAgent  string
	mediaBlockWarned bool

	xvfbProc        *browserpkg.XvfbProcess
	previousDisplay string
	profileDir      string
	keepProfileDir  bool
	downloadDir     string

	mu sync.Mutex
}

// NewGeckoDriver starts a geckodriver process bound to the configured Firefox
// / Camoufox binary and returns a ready-to-use Client.
func NewGeckoDriver(ctx context.Context, cfg Config, proxy *Proxy) (Client, error) {
	b := &geckoBrowser{
		cfg:             cfg,
		logger:          cfg.Logger,
		proxy:           proxy,
		previousDisplay: os.Getenv("DISPLAY"),
		sess: &w3c.Session{
			HTTP:      &http.Client{Timeout: 30 * time.Second},
			ErrPrefix: "geckodriver",
		},
	}

	if strings.TrimSpace(cfg.DriverPath) == "" {
		return nil, fmt.Errorf("geckodriver executable not found")
	}
	if strings.TrimSpace(cfg.BrowserPath) == "" {
		return nil, fmt.Errorf("firefox/camoufox binary not configured")
	}

	if err := b.prepareProfileDir(); err != nil {
		return nil, err
	}
	if err := b.startDriver(ctx); err != nil {
		_ = b.Close()
		return nil, err
	}
	if err := b.createSession(ctx); err != nil {
		_ = b.Close()
		return nil, err
	}

	return b, nil
}

func (b *geckoBrowser) UserAgent(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	return b.userAgent(runCtx)
}

func (b *geckoBrowser) Resolve(ctx context.Context, req Request) (*Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	timeout := time.Duration(max(req.MaxTimeoutMS, 1)) * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, message, err := b.resolve(runCtx, req)
	if err != nil {
		return nil, err
	}
	return &Result{Result: result, Message: message}, nil
}

func (b *geckoBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// DELETE /session tells geckodriver to quit Firefox gracefully.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	b.sess.Delete(ctx)
	cancel()
	b.driver.Stop()
	if b.driverRuntimeDir != "" {
		_ = os.RemoveAll(b.driverRuntimeDir)
		b.driverRuntimeDir = ""
	}
	b.cleanupProfileDir()
	b.stopDisplay()
	return nil
}

// ---------- driver lifecycle ----------

func (b *geckoBrowser) startDriver(ctx context.Context) error {
	port, err := w3c.FreeLocalPort()
	if err != nil {
		return fmt.Errorf("reserve geckodriver port: %w", err)
	}

	effectiveHeadless, display, err := b.prepareHeadlessMode(ctx)
	if err != nil {
		return err
	}
	b.effectiveHeadless = effectiveHeadless

	env := os.Environ()
	if display != "" {
		env = appendWithEnv(env, "DISPLAY", display)
	}

	spec := w3c.DriverSpec{
		Name: "geckodriver",
		Path: b.cfg.DriverPath,
		Args: []string{
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
			"--binary", b.cfg.BrowserPath,
		},
		Env: env,
	}
	if b.cfg.DebugLogging {
		if err := b.prepareDriverRuntimeDir(); err != nil {
			return err
		}
		// geckodriver only logs to stdout/stderr, so it needs redirection.
		spec.LogPath = filepath.Join(b.driverRuntimeDir, "geckodriver.log")
		spec.RedirectOutput = true
		spec.Args = append(spec.Args, "--log", "trace")
	}

	b.sess.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	driver, err := w3c.StartDriver(ctx, spec, b.sess)
	if err != nil {
		return err
	}
	b.driver = driver
	return nil
}

func (b *geckoBrowser) prepareDriverRuntimeDir() error {
	if b.driverRuntimeDir != "" {
		return nil
	}
	dir, err := createTransientDir("flaresolverr-go-geckodriver-*")
	if err != nil {
		return fmt.Errorf("create geckodriver runtime dir: %w", err)
	}
	b.driverRuntimeDir = dir
	return nil
}

func (b *geckoBrowser) createSession(ctx context.Context) error {
	args := b.firefoxArgs()
	b.logger.Debug("creating geckodriver session",
		"browser_path", b.cfg.BrowserPath,
		"headless", b.cfg.Headless,
		"effective_headless", b.effectiveHeadless,
		"display", os.Getenv("DISPLAY"),
		"args", args)

	prefs := b.firefoxPrefs()

	firefoxOptions := map[string]any{
		"binary": b.cfg.BrowserPath,
		"args":   args,
		"prefs":  prefs,
	}

	capabilities := map[string]any{
		"browserName":         "firefox",
		"acceptInsecureCerts": true,
		// eager (vs normal): return from navigate() once DOMContentLoaded
		// fires, without waiting for the full load event. This matters when
		// the target URL is actually a file download (.torrent etc.) —
		// Firefox never fires load for a download, so "normal" strategy
		// would hang until the outer timeout and leave a stale session.
		"pageLoadStrategy":        "eager",
		"unhandledPromptBehavior": "dismiss and notify",
		"moz:firefoxOptions":      firefoxOptions,
	}

	if b.proxy != nil && strings.TrimSpace(b.proxy.URL) != "" {
		if proxyCap, err := buildProxyCapability(b.proxy); err == nil && proxyCap != nil {
			capabilities["proxy"] = proxyCap
			// W3C proxy capabilities carry no credentials, and Firefox answers
			// the 407 with a modal dialog no WebDriver call can dismiss. The
			// other two backends do handle auth (MV3 extension for chromedriver,
			// Fetch.authRequired for chromedp) — say so loudly rather than
			// letting the user debug a generic challenge timeout.
			if strings.TrimSpace(b.proxy.Username) != "" || strings.TrimSpace(b.proxy.Password) != "" {
				b.logger.Warn("geckodriver backend cannot pass proxy credentials; the proxy will be contacted unauthenticated. Use the chromedriver or chromedp backend for authenticated proxies.")
			}
		} else if err != nil {
			b.logger.Warn("ignoring proxy", "err", err)
		}
	}

	payload := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": capabilities,
		},
	}

	raw, topSessionID, err := b.sess.Do(context.Background(), http.MethodPost, "/session", payload)
	if err != nil {
		if tail := b.driver.LogTail(); tail != "" {
			return fmt.Errorf("create geckodriver session: %w | geckodriver log: %s", err, tail)
		}
		return fmt.Errorf("create geckodriver session: %w", err)
	}

	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &created)

	sessionID := strings.TrimSpace(topSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(created.SessionID)
	}
	if sessionID == "" {
		return fmt.Errorf("geckodriver session id missing")
	}
	b.sess.ID = sessionID
	return nil
}

func (b *geckoBrowser) firefoxArgs() []string {
	args := []string{}
	// Point Firefox at a profile directory we own so we can tear it down in
	// Close(). Without this, geckodriver creates its own rust_mozprofile* in
	// /tmp that survives us if cleanup races the process exit.
	if b.profileDir != "" {
		args = append(args, "-profile", b.profileDir)
	}
	// Firefox uses --headless (no "new" variant); only add when we truly want
	// no-display and don't have Xvfb.
	if b.effectiveHeadless {
		args = append(args, "-headless")
	}
	if extra := splitArgs(os.Getenv("FIREFOX_ARGS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	return args
}

func (b *geckoBrowser) firefoxPrefs() map[string]any {
	prefs := map[string]any{
		"dom.webdriver.enabled":                      false,
		"useAutomationExtension":                     false,
		"dom.webnotifications.enabled":               false,
		"app.update.enabled":                         false,
		"datareporting.healthreport.uploadEnabled":   false,
		"datareporting.policy.dataSubmissionEnabled": false,
		"browser.startup.homepage_override.mstone":   "ignore",
		"browser.startup.page":                       0,
		"browser.newtabpage.enabled":                 false,
		"browser.shell.checkDefaultBrowser":          false,
		"network.cookie.cookieBehavior":              0, // accept all
		"privacy.trackingprotection.enabled":         false,
		"security.OCSP.enabled":                      0,
		// Download handling: auto-save to our profile's downloads dir without
		// a dialog for common attachment types. Without these, navigating to a
		// URL that serves Content-Disposition: attachment (e.g. a .torrent)
		// opens a modal save dialog that webdriver cannot dismiss, hanging the
		// session until the outer timeout.
		"browser.download.folderList":               2,
		"browser.download.useDownloadDir":           true,
		"browser.download.manager.showWhenStarting": false,
		"browser.download.alwaysOpenPanel":          false,
		"browser.helperApps.alwaysAsk.force":        false,
		"browser.helperApps.neverAsk.saveToDisk":    "application/x-bittorrent,application/octet-stream,application/x-msdownload,application/zip,application/x-zip-compressed",
		"pdfjs.disabled":                            true,
	}
	if dir := strings.TrimSpace(b.downloadDir); dir != "" {
		prefs["browser.download.dir"] = dir
	}
	if ua := strings.TrimSpace(b.cfg.StartupUserAgent); ua != "" {
		prefs["general.useragent.override"] = ua
	}
	if b.cfg.DisableMedia {
		// 2 = disallow images; cheap way to skip media fetches.
		prefs["permissions.default.image"] = 2
		prefs["media.autoplay.default"] = 5
	}
	if lang := strings.TrimSpace(os.Getenv("LANG")); lang != "" {
		prefs["intl.accept_languages"] = lang
	}
	return prefs
}

// ---------- resolve flow ----------

func (b *geckoBrowser) resolve(ctx context.Context, req Request) (*ChallengeResolutionResult, string, error) {
	return browserpkg.ResolvePage(ctx, b, req)
}

// ---------- browser.Page ----------

// SetMediaBlocked is a no-op here. Firefox has no per-request equivalent of
// Network.setBlockedURLs; media blocking is applied once via firefoxPrefs()
// when the profile is created, so cfg.DisableMedia works but the per-request
// flag cannot. Reported once so the difference is visible rather than silent.
func (b *geckoBrowser) SetMediaBlocked(_ context.Context, blocked bool) error {
	if blocked && !b.cfg.DisableMedia && !b.mediaBlockWarned {
		b.mediaBlockWarned = true
		b.logger.Warn("per-request disableMedia is not supported by the geckodriver backend; set disable_media in the config to block media for the whole profile")
	}
	return nil
}

func (b *geckoBrowser) Navigate(ctx context.Context, req Request) error {
	return b.navigate(ctx, req)
}

func (b *geckoBrowser) SetPageCookies(ctx context.Context, rawURL string, cookies []Cookie) error {
	return b.sess.SetCookies(ctx, rawURL, cookies)
}

func (b *geckoBrowser) Title(ctx context.Context) (string, error) { return b.sess.Title(ctx) }

func (b *geckoBrowser) SelectorExists(ctx context.Context, selector string) (bool, error) {
	return b.sess.SelectorExists(ctx, selector)
}

func (b *geckoBrowser) HTML(ctx context.Context) (string, error) { return b.sess.HTML(ctx) }

func (b *geckoBrowser) CurrentURL(ctx context.Context) (string, error) { return b.sess.URL(ctx) }

func (b *geckoBrowser) PageUserAgent(ctx context.Context) (string, error) {
	return b.userAgent(ctx)
}

func (b *geckoBrowser) PageCookies(ctx context.Context, _ string) ([]Cookie, error) {
	return b.sess.Cookies(ctx)
}

func (b *geckoBrowser) Screenshot(ctx context.Context) (string, error) {
	return b.sess.Screenshot(ctx)
}

func (b *geckoBrowser) ChallengePresent(ctx context.Context) (bool, error) {
	return b.challengePresent(ctx)
}

func (b *geckoBrowser) SolveChallenge(ctx context.Context) error {
	return b.solveChallenge(ctx)
}

// DocumentResponse returns a zero value: geckodriver exposes no cheap way to
// read the real HTTP status, so the pipeline keeps its 200 default.
func (b *geckoBrowser) DocumentResponse(context.Context, string) (documentResponse, error) {
	return documentResponse{}, nil
}

// ApplyTurnstileToken keeps this backend's own two-stage strategy: Managed
// Challenge Invisible widgets populate cf-turnstile-response by themselves, so
// the value is read first with no interaction; only if that is empty (and the
// caller asked via tabs_till_verify) do we click the checkbox and re-read,
// refreshing the cookie jar because the click can mint new cookies.
func (b *geckoBrowser) ApplyTurnstileToken(ctx context.Context, req Request, result *ChallengeResolutionResult) error {
	if token, err := b.readTurnstileToken(ctx); err == nil && token != "" {
		result.TurnstileToken = token
		b.logger.Info("turnstile token captured without interaction", "len", len(token))
		return nil
	}
	if req.TabsTillVerify == nil {
		return nil
	}

	b.logger.Info("turnstile token not auto-populated, attempting interactive click")
	clicked, diag := b.clickTurnstileCheckbox(ctx)
	b.logger.Info("turnstile click result", "clicked", clicked, "diag", diag)
	if !clicked {
		return nil
	}

	_ = sleepContext(ctx, 5*time.Second)
	token, err := b.readTurnstileToken(ctx)
	if err != nil || token == "" {
		b.logger.Info("turnstile token still empty after click")
		return nil
	}
	result.TurnstileToken = token
	b.logger.Info("turnstile token captured after click", "len", len(token))
	if fresh, err := b.sess.Cookies(ctx); err == nil && len(fresh) > 0 {
		result.Cookies = fresh
	}
	return nil
}

func (b *geckoBrowser) PageLogger() Logger      { return b.logger }
func (b *geckoBrowser) LogHTMLConfigured() bool { return b.cfg.LogHTML }

func (b *geckoBrowser) navigate(ctx context.Context, req Request) error {
	targetURL := req.URL
	if strings.EqualFold(req.Method, "POST") {
		htmlDoc := buildPostFormHTML(req.URL, req.PostData)
		targetURL = "data:text/html;charset=utf-8," + url.PathEscape(htmlDoc)
	}
	_, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/url"), map[string]any{"url": targetURL})
	return err
}

// solveChallenge polls the page, waiting for Cloudflare's "Verifying..."
// interstitial to resolve itself. Camoufox passes most passive fingerprint
// checks silently, so the expected path is a short wait loop. We still provide
// a fail-fast exit if the challenge lingers after a handful of attempts with
// no user-visible cleared state — this mirrors the chromium backend's bailout.
func (b *geckoBrowser) solveChallenge(ctx context.Context) error {
	_ = b.mouseWiggle(ctx)

	const (
		maxAttempts = 30
		tick        = time.Second

		// An interactive Turnstile ("verify you are human") never clears on
		// its own — it needs a real click on the checkbox. This loop used to
		// only wait and wiggle the mouse, so such a challenge always ran out
		// the attempts and failed, even though clickTurnstileCheckbox already
		// existed: it was only reachable from ApplyTurnstileToken, which runs
		// *after* solveChallenge has succeeded.
		//
		// Give the passive path a few seconds first: a Managed Challenge often
		// clears by itself, and clicking before the widget script has wired up
		// its handler does nothing.
		firstClickAttempt = 3
		clickEvery        = 6
	)

	clickAttempts := 0
	for attempt := range maxAttempts {
		if err := sleepContext(ctx, tick); err != nil {
			return err
		}
		found, err := b.challengePresent(ctx)
		if err != nil {
			return err
		}
		if !found {
			b.awaitDocumentReady(ctx)
			return nil
		}

		if attempt >= firstClickAttempt && (attempt-firstClickAttempt)%clickEvery == 0 {
			clickAttempts++
			clicked, diag := b.clickTurnstileCheckbox(ctx)
			b.logger.Debug("turnstile checkbox click during solve",
				"attempt", attempt, "clicked", clicked, "diag", diag)
		}

		if attempt%5 == 4 {
			_ = b.mouseWiggle(ctx)
		}
	}
	return fmt.Errorf("cloudflare challenge did not clear within %d seconds (%d checkbox click attempts)", maxAttempts, clickAttempts)
}

// awaitDocumentReady waits for the destination page to finish parsing before
// the pipeline snapshots it.
//
// Clearing a challenge ends in a navigation to the real page, and the challenge
// stops being detectable the moment that navigation starts — so without this
// the snapshot can catch a document that has only got as far as </head>.
// Measured on a live interactive challenge: the response was 1.4 KB with no
// <body> at waitInSeconds=0 and the full 12 KB page at waitInSeconds=2. Waiting
// on the page's own state beats making callers guess a sleep.
//
// Every exit is a plain return: this only improves what the caller is about to
// read, so a driver that will not answer is a reason to stop waiting, never a
// reason to fail a solve that already succeeded.
func (b *geckoBrowser) awaitDocumentReady(ctx context.Context) {
	const (
		budget = 10 * time.Second
		poll   = 200 * time.Millisecond
	)

	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		// document.body is checked alongside readyState because a document that
		// has not started loading yet still reports the previous "complete".
		ready, err := b.sess.ExecuteBool(ctx, `document.readyState === 'complete' && !!document.body`)
		if err != nil {
			return
		}
		if ready {
			return
		}
		if err := sleepContext(ctx, poll); err != nil {
			return
		}
	}
	b.logger.Debug("page still loading when the challenge cleared", "waited", budget)
}

// mouseWiggle sends a short sequence of mouse moves via the W3C Actions API.
// Firefox's Actions implementation accepts the same shape as chromedriver so
// we don't need CDP here.
func (b *geckoBrowser) mouseWiggle(ctx context.Context) error {
	points := []struct{ x, y int }{
		{120, 180}, {260, 240}, {400, 300}, {540, 340},
		{620, 280}, {480, 220}, {340, 260}, {200, 320},
	}
	actions := []map[string]any{
		{
			"id":         "mouse-wiggle",
			"type":       "pointer",
			"parameters": map[string]any{"pointerType": "mouse"},
			"actions":    buildWiggleActions(points),
		},
	}
	_, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/actions"), map[string]any{
		"actions": actions,
	})
	if err != nil {
		// Release any half-started action chain.
		_, _, _ = b.sess.Do(ctx, http.MethodDelete, b.sess.Path("/actions"), nil)
		return err
	}
	_, _, _ = b.sess.Do(ctx, http.MethodDelete, b.sess.Path("/actions"), nil)
	return nil
}

func buildWiggleActions(points []struct{ x, y int }) []map[string]any {
	actions := make([]map[string]any, 0, len(points)*2)
	for _, p := range points {
		actions = append(actions, map[string]any{
			"type":     "pointerMove",
			"duration": 40,
			"x":        p.x,
			"y":        p.y,
			"origin":   "viewport",
		})
		actions = append(actions, map[string]any{"type": "pause", "duration": 40})
	}
	return actions
}

// ---------- DOM probes ----------

func (b *geckoBrowser) challengePresent(ctx context.Context) (bool, error) {
	title, err := b.sess.Title(ctx)
	if err != nil {
		return false, err
	}
	for _, challengeTitle := range challengeTitles {
		if strings.EqualFold(title, challengeTitle) {
			return true, nil
		}
	}
	for _, selector := range challengeSelectors {
		exists, err := b.sess.SelectorExists(ctx, selector)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (b *geckoBrowser) userAgent(ctx context.Context) (string, error) {
	if strings.TrimSpace(b.cachedUserAgent) != "" {
		return b.cachedUserAgent, nil
	}
	ua, err := b.sess.ExecuteString(ctx, `navigator.userAgent`)
	if err != nil {
		return "", err
	}
	b.cachedUserAgent = scrubUserAgent(ua)
	return b.cachedUserAgent, nil
}

// challengeFrameSelector matches the cross-origin iframe Cloudflare serves the
// Turnstile widget from.
const challengeFrameSelector = `iframe[src*="challenges.cloudflare.com"]`

const (
	// Each candidate node costs one round-trip to the driver, so the shadow
	// walk is bounded. A challenge interstitial is a few dozen elements; the
	// cap only bites if we somehow run it against a full-size page.
	shadowWalkMaxNodes = 400
	shadowWalkMaxDepth = 6
)

// clickTurnstileCheckbox locates the "verify you are human" checkbox and clicks
// it natively. Returns whether the click was accepted, plus a diagnostic string.
//
// The path to that checkbox crosses two closed shadow roots — one hiding the
// challenges.cloudflare.com iframe in the host page, one hiding the checkbox
// inside that iframe's body — so none of it is visible to page JavaScript. An
// earlier version of this function ran document.querySelector from the page and
// consequently reported "no widget" on every single attempt: not a click that
// missed, a widget that could not be seen. The WebDriver element endpoints do
// see it, because Firefox hands over closed shadow roots (see w3c.ShadowRoot).
func (b *geckoBrowser) clickTurnstileCheckbox(ctx context.Context) (bool, string) {
	frame, walked, err := b.findThroughShadowDOM(ctx, challengeFrameSelector)
	if err != nil {
		return false, fmt.Sprintf("locating the challenge frame failed after %d nodes: %v", walked, err)
	}
	if frame == "" {
		return false, fmt.Sprintf("no challenge frame on the page (%d nodes walked)", walked)
	}

	if err := b.sess.SwitchToFrame(ctx, frame); err != nil {
		return false, "switch into the challenge frame: " + err.Error()
	}
	// Everything below addresses the iframe, including any later call on this
	// session — the pipeline reads Title and HTML right after us.
	defer func() {
		if err := b.sess.SwitchToDefaultContent(ctx); err != nil {
			b.logger.Warn("could not return to the top-level browsing context", "err", err)
		}
	}()

	checkbox, walked, err := b.findThroughShadowDOM(ctx, "input[type=checkbox]")
	if err != nil {
		return false, fmt.Sprintf("locating the checkbox failed after %d nodes: %v", walked, err)
	}
	if checkbox == "" {
		// A passive challenge has no checkbox at all; that is not a failure.
		return false, fmt.Sprintf("challenge frame has no checkbox (%d nodes walked)", walked)
	}

	if err := b.sess.ClickElement(ctx, checkbox); err != nil {
		return false, "click the checkbox: " + err.Error()
	}

	// Only report success once the widget agrees the click landed. Cloudflare
	// swaps the widget out on acceptance, so a stale reference is a pass, not a
	// failure.
	_ = sleepContext(ctx, 500*time.Millisecond)
	selected, err := b.sess.ElementSelected(ctx, checkbox)
	if err != nil {
		return true, "clicked, widget replaced (" + err.Error() + ")"
	}
	return selected, fmt.Sprintf("clicked, checked=%t", selected)
}

// findThroughShadowDOM returns a reference to the first element matching
// selector in the current browsing context, descending into shadow roots when
// the light DOM has no match. It also reports how many nodes it probed, which
// is the number that tells "found nothing" apart from "gave up".
//
// The walk is breadth-first and deliberately unfiltered: a closed shadow host
// is indistinguishable from an ordinary element until the driver is asked, so
// every node is a candidate and the cost is capped instead of guessed at.
func (b *geckoBrowser) findThroughShadowDOM(ctx context.Context, selector string) (string, int, error) {
	ids, err := b.sess.FindElements(ctx, selector)
	if err != nil {
		return "", 0, err
	}
	if len(ids) > 0 {
		return ids[0], 0, nil
	}

	walked := 0
	roots := []string{""} // "" is the document itself
	for depth := 0; depth < shadowWalkMaxDepth && len(roots) > 0; depth++ {
		var deeper []string
		for _, root := range roots {
			hosts, err := b.findAllIn(ctx, root, "*")
			if err != nil {
				continue
			}
			for _, host := range hosts {
				if walked >= shadowWalkMaxNodes {
					return "", walked, fmt.Errorf("shadow walk hit its %d-node cap", shadowWalkMaxNodes)
				}
				walked++

				shadow, err := b.sess.ShadowRoot(ctx, host)
				if err != nil || shadow == "" {
					continue
				}
				if found, err := b.sess.FindElementsInShadow(ctx, shadow, selector); err == nil && len(found) > 0 {
					return found[0], walked, nil
				}
				deeper = append(deeper, shadow)
			}
		}
		roots = deeper
	}
	return "", walked, nil
}

// findAllIn searches a shadow root, or the document when root is "".
func (b *geckoBrowser) findAllIn(ctx context.Context, root, selector string) ([]string, error) {
	if root == "" {
		return b.sess.FindElements(ctx, selector)
	}
	return b.sess.FindElementsInShadow(ctx, root, selector)
}

// readTurnstileToken returns the value of any cf-turnstile-response input on
// the page. Returns "" when no such input exists or the challenge hasn't
// populated it yet. Walks both the main document and any non-cross-origin
// iframes (Cloudflare embeds the input in a friendly iframe in some flows).
func (b *geckoBrowser) readTurnstileToken(ctx context.Context) (string, error) {
	const script = `
        (function() {
            var sel = 'input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]';
            var el = document.querySelector(sel);
            if (el && el.value) return el.value;
            var frames = document.querySelectorAll('iframe');
            for (var i = 0; i < frames.length; i++) {
                try {
                    var doc = frames[i].contentDocument;
                    if (!doc) continue;
                    var inner = doc.querySelector(sel);
                    if (inner && inner.value) return inner.value;
                } catch (_) {}
            }
            return '';
        })()
    `
	return b.sess.ExecuteString(ctx, script)
}

// ---------- execute helpers ----------

// ---------- low-level plumbing ----------

// ---------- headless / display ----------

func (b *geckoBrowser) prepareHeadlessMode(ctx context.Context) (bool, string, error) {
	if !b.cfg.Headless || runtime.GOOS == "windows" {
		return b.cfg.Headless, "", nil
	}
	if display := os.Getenv("DISPLAY"); display != "" {
		return false, display, nil
	}
	xvfbPath, err := exec.LookPath("Xvfb")
	if err != nil {
		// Firefox -headless degrades Camoufox fingerprint somewhat, but it's
		// still more private than vanilla Chrome headless.
		b.logger.Warn("HEADLESS=true without DISPLAY or Xvfb; falling back to Firefox -headless mode")
		return true, "", nil
	}
	proc, display, err := browserpkg.StartXvfb(ctx, xvfbPath)
	if err != nil {
		return false, "", err
	}
	b.xvfbProc = proc
	return false, display, nil
}

func (b *geckoBrowser) stopDisplay() {
	if b.xvfbProc != nil {
		_ = b.xvfbProc.Stop()
		b.xvfbProc = nil
	}
}

// ---------- profile dir ----------

func (b *geckoBrowser) prepareProfileDir() error {
	if b.profileDir != "" {
		return nil
	}
	dir, err := createTransientDir("flaresolverr-go-geckoprofile-*")
	if err != nil {
		return fmt.Errorf("create firefox profile dir: %w", err)
	}
	b.profileDir = dir
	downloadDir := filepath.Join(dir, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err == nil {
		b.downloadDir = downloadDir
	}
	return nil
}

func (b *geckoBrowser) cleanupProfileDir() {
	if b.profileDir == "" || b.keepProfileDir {
		return
	}
	_ = os.RemoveAll(b.profileDir)
	b.profileDir = ""
}

// ---------- helpers ----------

func splitArgs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := strings.Fields(raw)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func buildProxyCapability(p *Proxy) (map[string]any, error) {
	raw := strings.TrimSpace(p.URL)
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return nil, fmt.Errorf("proxy url missing host:port")
	}
	hostPort := host + ":" + port

	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https", "":
		return map[string]any{
			"proxyType": "manual",
			"httpProxy": hostPort,
			"sslProxy":  hostPort,
			"noProxy":   []string{"localhost", "127.0.0.1"},
		}, nil
	case "socks5", "socks4":
		version := 5
		if scheme == "socks4" {
			version = 4
		}
		return map[string]any{
			"proxyType":    "manual",
			"socksProxy":   hostPort,
			"socksVersion": version,
			"noProxy":      []string{"localhost", "127.0.0.1"},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}
}
