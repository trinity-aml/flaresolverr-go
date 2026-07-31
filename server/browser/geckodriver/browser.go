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
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := sleepContext(ctx, tick); err != nil {
			return err
		}
		found, err := b.challengePresent(ctx)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if attempt%5 == 4 {
			_ = b.mouseWiggle(ctx)
		}
	}
	return fmt.Errorf("cloudflare challenge did not clear within %d seconds", maxAttempts)
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

// clickTurnstileCheckbox sends a real pointer click at the screen-space
// center of the Turnstile widget. Cloudflare hosts the actual checkbox
// inside a cross-origin iframe (challenges.cloudflare.com), so we can't
// dispatch synthetic events at it from page JS — but Firefox forwards a
// WebDriver Actions pointer click at the right (x, y) coordinates into
// the iframe just fine. Returns true if a widget was found and clicked.
func (b *geckoBrowser) clickTurnstileCheckbox(ctx context.Context) (bool, string) {
	// First locate the widget. We try several candidate selectors and
	// scroll the chosen element into view before clicking, otherwise the
	// pointer click lands on background pixels and Cloudflare never sees
	// the gesture.
	const locate = `
        (function() {
            var selectors = [
                'iframe[src*="challenges.cloudflare.com"]',
                '.cf-turnstile iframe',
                '.cf-turnstile',
                '[data-sitekey]'
            ];
            var found = null;
            for (var i = 0; i < selectors.length; i++) {
                var el = document.querySelector(selectors[i]);
                if (el) {
                    var r0 = el.getBoundingClientRect();
                    if (r0.width >= 4 && r0.height >= 4) {
                        found = {el: el, sel: selectors[i]};
                        break;
                    }
                }
            }
            if (!found) {
                // Count widgets to help diagnose
                var cnt = document.querySelectorAll('.cf-turnstile, [data-sitekey], iframe').length;
                return {error: 'no-widget', iframe_total: cnt};
            }
            try {
                found.el.scrollIntoView({block: 'center', inline: 'center'});
            } catch (_) {}
            var r = found.el.getBoundingClientRect();
            var cx = Math.round(r.left + Math.min(30, r.width / 2));
            var cy = Math.round(r.top + r.height / 2);
            return {selector: found.sel, x: cx, y: cy, w: Math.round(r.width), h: Math.round(r.height),
                    viewportW: window.innerWidth, viewportH: window.innerHeight};
        })()
    `
	raw, err := b.sess.Execute(ctx, locate)
	if err != nil {
		return false, fmt.Sprintf("locate js err: %v", err)
	}
	var pos struct {
		Selector    string `json:"selector"`
		Error       string `json:"error"`
		IframeTotal int    `json:"iframe_total"`
		X           int    `json:"x"`
		Y           int    `json:"y"`
		W           int    `json:"w"`
		H           int    `json:"h"`
		ViewportW   int    `json:"viewportW"`
		ViewportH   int    `json:"viewportH"`
	}
	if err := json.Unmarshal(raw, &pos); err != nil {
		return false, fmt.Sprintf("locate json err: %v raw=%s", err, string(raw))
	}
	if pos.Error != "" || (pos.W == 0 && pos.H == 0) {
		return false, fmt.Sprintf("widget not located: err=%q iframe_total=%d", pos.Error, pos.IframeTotal)
	}
	diag := fmt.Sprintf("sel=%s x=%d y=%d w=%d h=%d viewport=%dx%d", pos.Selector, pos.X, pos.Y, pos.W, pos.H, pos.ViewportW, pos.ViewportH)

	// Brief settle after scroll before the click.
	_ = sleepContext(ctx, 300*time.Millisecond)

	actions := []map[string]any{
		{
			"id":         "turnstile-mouse",
			"type":       "pointer",
			"parameters": map[string]any{"pointerType": "mouse"},
			"actions": []map[string]any{
				{"type": "pointerMove", "duration": 150, "x": pos.X, "y": pos.Y, "origin": "viewport"},
				{"type": "pause", "duration": 200},
				{"type": "pointerDown", "button": 0},
				{"type": "pause", "duration": 120},
				{"type": "pointerUp", "button": 0},
			},
		},
	}
	if _, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/actions"), map[string]any{
		"actions": actions,
	}); err != nil {
		_, _, _ = b.sess.Do(ctx, http.MethodDelete, b.sess.Path("/actions"), nil)
		return false, diag + " click-actions err: " + err.Error()
	}
	_, _, _ = b.sess.Do(ctx, http.MethodDelete, b.sess.Path("/actions"), nil)
	return true, diag
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
