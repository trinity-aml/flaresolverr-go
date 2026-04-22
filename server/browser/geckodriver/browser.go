// Package geckodriverbackend implements the browser.Client interface on top of
// geckodriver + Firefox (typically daijro/camoufox). Camoufox ships with
// randomised fingerprints, patched navigator.webdriver, TLS noise and WebRTC
// leak prevention, so we can drive it through the standard W3C WebDriver
// protocol without any CDP-specific stealth layer.
package geckodriverbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
)

type Config = browserpkg.Config
type Proxy = browserpkg.Proxy
type Cookie = browserpkg.Cookie
type ChallengeResolutionResult = browserpkg.ChallengeResolutionResult
type Logger = browserpkg.Logger
type Request = browserpkg.Request
type Result = browserpkg.Result
type Client = browserpkg.Client

var (
	appendWithEnv        = browserpkg.AppendWithEnv
	buildPostFormHTML    = browserpkg.BuildPostFormHTML
	scrubUserAgent       = browserpkg.ScrubUserAgent
	firstCookiePath      = browserpkg.FirstCookiePath
	sleepContext         = browserpkg.SleepContext
	accessDeniedTitles   = browserpkg.AccessDeniedTitles
	accessDeniedSelectors = browserpkg.AccessDeniedSelectors
	challengeTitles      = browserpkg.ChallengeTitles
	challengeSelectors   = browserpkg.ChallengeSelectors
	createTransientDir   = browserpkg.CreateTransientDir
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type geckoBrowser struct {
	cfg    Config
	logger Logger
	proxy  *Proxy

	httpClient        *http.Client
	baseURL           string
	sessionID         string
	effectiveHeadless bool

	driverCmd       *exec.Cmd
	driverRuntimeDir string
	driverLogPath   string
	cachedUserAgent string

	xvfbCmd         *exec.Cmd
	previousDisplay string
	profileDir      string
	keepProfileDir  bool
	downloadDir     string

	mu sync.Mutex
}

// NewGeckoDriver starts a geckodriver process bound to the configured Firefox
// / Camoufox binary and returns a ready-to-use Client.
func NewGeckoDriver(cfg Config, proxy *Proxy) (Client, error) {
	b := &geckoBrowser{
		cfg:             cfg,
		logger:          cfg.Logger,
		proxy:           proxy,
		previousDisplay: os.Getenv("DISPLAY"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
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
	if err := b.startDriver(); err != nil {
		_ = b.Close()
		return nil, err
	}
	if err := b.createSession(); err != nil {
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
	b.deleteSession(ctx)
	cancel()
	b.stopDriver()
	if b.driverRuntimeDir != "" {
		_ = os.RemoveAll(b.driverRuntimeDir)
		b.driverRuntimeDir = ""
	}
	b.cleanupProfileDir()
	b.stopDisplay()
	return nil
}

// stopDriver waits for geckodriver to exit on its own (triggered by DELETE
// /session) for a short grace period, then escalates to SIGTERM and finally
// SIGKILL. Killing immediately orphans the Firefox child and strands the
// profile directory on disk.
func (b *geckoBrowser) stopDriver() {
	if b.driverCmd == nil || b.driverCmd.Process == nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- b.driverCmd.Wait() }()

	select {
	case <-done:
		b.driverCmd = nil
		return
	case <-time.After(3 * time.Second):
	}

	_ = b.driverCmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		b.driverCmd = nil
		return
	case <-time.After(2 * time.Second):
	}

	_ = b.driverCmd.Process.Kill()
	<-done
	b.driverCmd = nil
}

// ---------- driver lifecycle ----------

func (b *geckoBrowser) startDriver() error {
	port, err := freeLocalPort()
	if err != nil {
		return fmt.Errorf("reserve geckodriver port: %w", err)
	}

	effectiveHeadless, display, err := b.prepareHeadlessMode()
	if err != nil {
		return err
	}
	b.effectiveHeadless = effectiveHeadless

	env := os.Environ()
	if display != "" {
		env = appendWithEnv(env, "DISPLAY", display)
	}

	cmdArgs := []string{
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--binary", b.cfg.BrowserPath,
	}
	b.driverLogPath = ""
	if b.cfg.DebugLogging {
		if err := b.prepareDriverRuntimeDir(); err != nil {
			return err
		}
		b.driverLogPath = filepath.Join(b.driverRuntimeDir, "geckodriver.log")
		cmdArgs = append(cmdArgs, "--log", "trace")
	}

	cmd := exec.Command(b.cfg.DriverPath, cmdArgs...)
	cmd.Env = env
	if b.driverLogPath != "" {
		logFile, err := os.Create(b.driverLogPath)
		if err != nil {
			return fmt.Errorf("create geckodriver log: %w", err)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start geckodriver: %w", err)
	}

	b.driverCmd = cmd
	b.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(15 * time.Second)
	waitFor := 50 * time.Millisecond
	for time.Now().Before(deadline) {
		statusCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := b.webDriverRequest(statusCtx, http.MethodGet, "/status", nil)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(waitFor)
		if waitFor < 500*time.Millisecond {
			waitFor *= 2
		}
	}
	return fmt.Errorf("geckodriver did not become ready")
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

func (b *geckoBrowser) createSession() error {
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
		"browserName":             "firefox",
		"acceptInsecureCerts":     true,
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
		} else if err != nil {
			b.logger.Warn("ignoring proxy: %s", err)
		}
	}

	payload := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": capabilities,
		},
	}

	raw, topSessionID, err := b.webDriverRequest(context.Background(), http.MethodPost, "/session", payload)
	if err != nil {
		if tail := b.driverLogTail(); tail != "" {
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
	b.sessionID = sessionID
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
		"dom.webdriver.enabled":                        false,
		"useAutomationExtension":                       false,
		"dom.webnotifications.enabled":                 false,
		"app.update.enabled":                           false,
		"datareporting.healthreport.uploadEnabled":     false,
		"datareporting.policy.dataSubmissionEnabled":   false,
		"browser.startup.homepage_override.mstone":     "ignore",
		"browser.startup.page":                         0,
		"browser.newtabpage.enabled":                   false,
		"browser.shell.checkDefaultBrowser":            false,
		"network.cookie.cookieBehavior":                0, // accept all
		"privacy.trackingprotection.enabled":           false,
		"security.OCSP.enabled":                        0,
		// Download handling: auto-save to our profile's downloads dir without
		// a dialog for common attachment types. Without these, navigating to a
		// URL that serves Content-Disposition: attachment (e.g. a .torrent)
		// opens a modal save dialog that webdriver cannot dismiss, hanging the
		// session until the outer timeout.
		"browser.download.folderList":                   2,
		"browser.download.useDownloadDir":               true,
		"browser.download.manager.showWhenStarting":     false,
		"browser.download.alwaysOpenPanel":              false,
		"browser.helperApps.alwaysAsk.force":            false,
		"browser.helperApps.neverAsk.saveToDisk":        "application/x-bittorrent,application/octet-stream,application/x-msdownload,application/zip,application/x-zip-compressed",
		"pdfjs.disabled":                                true,
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
	if err := b.navigate(ctx, req); err != nil {
		return nil, "", fmt.Errorf("navigate: %w", err)
	}

	if len(req.Cookies) > 0 {
		if err := b.setCookies(ctx, req.URL, req.Cookies); err != nil {
			return nil, "", fmt.Errorf("set cookies: %w", err)
		}
		if err := b.navigate(ctx, req); err != nil {
			return nil, "", fmt.Errorf("reload after cookies: %w", err)
		}
	}

	title, err := b.pageTitle(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read page title: %w", err)
	}
	for _, accessTitle := range accessDeniedTitles {
		if strings.HasPrefix(title, accessTitle) {
			return nil, "", fmt.Errorf("Cloudflare has blocked this request. Probably your IP is banned for this site, check in your web browser.")
		}
	}
	for _, selector := range accessDeniedSelectors {
		exists, err := b.selectorExists(ctx, selector)
		if err != nil {
			return nil, "", fmt.Errorf("check access denied selector %q: %w", selector, err)
		}
		if exists {
			return nil, "", fmt.Errorf("Cloudflare has blocked this request. Probably your IP is banned for this site, check in your web browser.")
		}
	}

	message := "Challenge not detected!"
	challengeFound, err := b.challengePresent(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("detect challenge: %w", err)
	}
	if challengeFound {
		if err := b.solveChallenge(ctx); err != nil {
			return nil, "", fmt.Errorf("solve challenge: %w", err)
		}
		message = "Challenge solved!"
	}

	currentURL, err := b.currentURL(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read current url: %w", err)
	}
	userAgent, err := b.userAgent(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read user agent: %w", err)
	}
	cookies, err := b.currentCookies(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read cookies: %w", err)
	}

	result := &ChallengeResolutionResult{
		URL:       currentURL,
		Status:    200, // geckodriver has no cheap way to read the real HTTP status
		Cookies:   cookies,
		UserAgent: userAgent,
	}

	if !req.ReturnOnlyCookies {
		if req.WaitInSeconds > 0 {
			if err := sleepContext(ctx, time.Duration(req.WaitInSeconds)*time.Second); err != nil {
				return nil, "", fmt.Errorf("wait after challenge: %w", err)
			}
		}
		htmlDoc, err := b.pageHTML(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("read response html: %w", err)
		}
		result.Response = htmlDoc
	}

	if req.ReturnScreenshot {
		shot, err := b.screenshot(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("capture screenshot: %w", err)
		}
		result.Screenshot = shot
	}

	return result, message, nil
}

func (b *geckoBrowser) navigate(ctx context.Context, req Request) error {
	targetURL := req.URL
	if strings.EqualFold(req.Method, "POST") {
		htmlDoc := buildPostFormHTML(req.URL, req.PostData)
		targetURL = "data:text/html;charset=utf-8," + url.PathEscape(htmlDoc)
	}
	_, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/url"), map[string]any{"url": targetURL})
	return err
}

func (b *geckoBrowser) setCookies(ctx context.Context, rawURL string, cookies []Cookie) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	domain := parsed.Hostname()
	secure := strings.EqualFold(parsed.Scheme, "https")

	for _, cookie := range cookies {
		if cookie.Name == "" {
			continue
		}
		cookiePayload := map[string]any{
			"name":     cookie.Name,
			"value":    cookie.Value,
			"path":     firstCookiePath(cookie.Path),
			"domain":   firstNonEmpty(cookie.Domain, domain),
			"secure":   cookie.Secure || secure,
			"httpOnly": cookie.HTTPOnly,
		}
		if strings.TrimSpace(cookie.SameSite) != "" {
			cookiePayload["sameSite"] = cookie.SameSite
		}
		if cookie.Expires > 0 {
			cookiePayload["expiry"] = int64(cookie.Expires)
		}
		payload := map[string]any{"cookie": cookiePayload}
		if _, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/cookie"), payload); err != nil {
			return err
		}
	}
	return nil
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
	_, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/actions"), map[string]any{
		"actions": actions,
	})
	if err != nil {
		// Release any half-started action chain.
		_, _, _ = b.webDriverRequest(ctx, http.MethodDelete, b.sessionPath("/actions"), nil)
		return err
	}
	_, _, _ = b.webDriverRequest(ctx, http.MethodDelete, b.sessionPath("/actions"), nil)
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
	title, err := b.pageTitle(ctx)
	if err != nil {
		return false, err
	}
	for _, challengeTitle := range challengeTitles {
		if strings.EqualFold(title, challengeTitle) {
			return true, nil
		}
	}
	for _, selector := range challengeSelectors {
		exists, err := b.selectorExists(ctx, selector)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (b *geckoBrowser) pageTitle(ctx context.Context) (string, error) {
	raw, _, err := b.webDriverRequest(ctx, http.MethodGet, b.sessionPath("/title"), nil)
	if err != nil {
		return "", err
	}
	var title string
	if err := json.Unmarshal(raw, &title); err != nil {
		return "", err
	}
	return title, nil
}

func (b *geckoBrowser) currentURL(ctx context.Context) (string, error) {
	raw, _, err := b.webDriverRequest(ctx, http.MethodGet, b.sessionPath("/url"), nil)
	if err != nil {
		return "", err
	}
	var current string
	if err := json.Unmarshal(raw, &current); err != nil {
		return "", err
	}
	return current, nil
}

func (b *geckoBrowser) pageHTML(ctx context.Context) (string, error) {
	return b.executeString(ctx, `document.documentElement ? document.documentElement.outerHTML : ''`)
}

func (b *geckoBrowser) selectorExists(ctx context.Context, selector string) (bool, error) {
	return b.executeBool(ctx, fmt.Sprintf(`document.querySelector(%q) !== null`, selector))
}

func (b *geckoBrowser) currentCookies(ctx context.Context) ([]Cookie, error) {
	raw, _, err := b.webDriverRequest(ctx, http.MethodGet, b.sessionPath("/cookie"), nil)
	if err != nil {
		return nil, err
	}
	type wdCookie struct {
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Domain   string  `json:"domain"`
		Path     string  `json:"path"`
		HTTPOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
		SameSite string  `json:"sameSite"`
		Expiry   float64 `json:"expiry"`
	}
	var entries []wdCookie
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	result := make([]Cookie, 0, len(entries))
	for _, entry := range entries {
		result = append(result, Cookie{
			Name:     entry.Name,
			Value:    entry.Value,
			Domain:   entry.Domain,
			Path:     entry.Path,
			HTTPOnly: entry.HTTPOnly,
			Secure:   entry.Secure,
			SameSite: entry.SameSite,
			Expires:  entry.Expiry,
		})
	}
	return result, nil
}

func (b *geckoBrowser) screenshot(ctx context.Context) (string, error) {
	raw, _, err := b.webDriverRequest(ctx, http.MethodGet, b.sessionPath("/screenshot"), nil)
	if err != nil {
		return "", err
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return "", err
	}
	return encoded, nil
}

func (b *geckoBrowser) userAgent(ctx context.Context) (string, error) {
	if strings.TrimSpace(b.cachedUserAgent) != "" {
		return b.cachedUserAgent, nil
	}
	ua, err := b.executeString(ctx, `navigator.userAgent`)
	if err != nil {
		return "", err
	}
	b.cachedUserAgent = scrubUserAgent(ua)
	return b.cachedUserAgent, nil
}

// ---------- execute helpers ----------

func (b *geckoBrowser) executeString(ctx context.Context, script string) (string, error) {
	raw, err := b.executeScript(ctx, script)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func (b *geckoBrowser) executeBool(ctx context.Context, script string) (bool, error) {
	raw, err := b.executeScript(ctx, script)
	if err != nil {
		return false, err
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, err
	}
	return value, nil
}

func (b *geckoBrowser) executeScript(ctx context.Context, script string) (json.RawMessage, error) {
	script = strings.TrimSpace(script)
	script = strings.TrimSuffix(script, ";")
	if script == "" {
		script = "null"
	}
	wrapped := "return (" + script + ")"
	raw, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/execute/sync"), map[string]any{
		"script": wrapped,
		"args":   []any{},
	})
	return raw, err
}

// ---------- low-level plumbing ----------

func (b *geckoBrowser) sessionPath(path string) string {
	return "/session/" + b.sessionID + path
}

func (b *geckoBrowser) webDriverRequest(ctx context.Context, method, path string, payload any) (json.RawMessage, string, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	var envelope struct {
		Value     json.RawMessage `json:"value"`
		SessionID string          `json:"sessionId"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &envelope); err != nil {
			if resp.StatusCode >= 400 {
				return nil, "", fmt.Errorf("geckodriver http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
			}
			return nil, "", err
		}
	}

	if resp.StatusCode >= 400 {
		var wdErr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if len(envelope.Value) > 0 && json.Unmarshal(envelope.Value, &wdErr) == nil && strings.TrimSpace(wdErr.Message) != "" {
			return nil, envelope.SessionID, fmt.Errorf("%s", wdErr.Message)
		}
		return nil, envelope.SessionID, fmt.Errorf("geckodriver http %d", resp.StatusCode)
	}

	return envelope.Value, envelope.SessionID, nil
}

func (b *geckoBrowser) deleteSession(ctx context.Context) {
	if strings.TrimSpace(b.sessionID) == "" || strings.TrimSpace(b.baseURL) == "" {
		return
	}
	_, _, _ = b.webDriverRequest(ctx, http.MethodDelete, b.sessionPath(""), nil)
	b.sessionID = ""
}

func (b *geckoBrowser) driverLogTail() string {
	if b.driverLogPath == "" {
		return ""
	}
	data, err := os.ReadFile(b.driverLogPath)
	if err != nil {
		return ""
	}
	const maxTail = 4 * 1024
	if len(data) > maxTail {
		data = data[len(data)-maxTail:]
	}
	return strings.TrimSpace(string(data))
}

// ---------- headless / display ----------

func (b *geckoBrowser) prepareHeadlessMode() (bool, string, error) {
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
	cmd, display, err := browserpkg.StartXvfb(xvfbPath)
	if err != nil {
		return false, "", err
	}
	b.xvfbCmd = cmd
	return false, display, nil
}

func (b *geckoBrowser) stopDisplay() {
	if b.xvfbCmd != nil && b.xvfbCmd.Process != nil {
		_ = b.xvfbCmd.Process.Kill()
		_, _ = b.xvfbCmd.Process.Wait()
		b.xvfbCmd = nil
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

func freeLocalPort() (int, error) {
	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer lst.Close()
	return lst.Addr().(*net.TCPAddr).Port, nil
}

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
			"proxyType":       "manual",
			"httpProxy":       hostPort,
			"sslProxy":        hostPort,
			"noProxy":         []string{"localhost", "127.0.0.1"},
		}, nil
	case "socks5", "socks4":
		version := 5
		if scheme == "socks4" {
			version = 4
		}
		return map[string]any{
			"proxyType":     "manual",
			"socksProxy":    hostPort,
			"socksVersion":  version,
			"noProxy":       []string{"localhost", "127.0.0.1"},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}
}
