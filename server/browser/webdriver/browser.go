package webdriverbackend

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
	"time"

	browserpkg "flaresolverr-go/server/browser"
)

type Config = browserpkg.Config
type Proxy = browserpkg.Proxy
type Cookie = browserpkg.Cookie
type ChallengeResolutionResult = browserpkg.ChallengeResolutionResult
type Logger = browserpkg.Logger
type Request = browserpkg.Request
type Result = browserpkg.Result
type Client = browserpkg.Client
type clickTarget = browserpkg.ClickTarget
type point = browserpkg.Point

var appendWithEnv = browserpkg.AppendWithEnv
var buildPostFormHTML = browserpkg.BuildPostFormHTML
var scrubUserAgent = browserpkg.ScrubUserAgent
var blockedURLs = browserpkg.BlockedURLs
var normalizeBlockedPattern = browserpkg.NormalizeBlockedPattern
var accessDeniedTitles = browserpkg.AccessDeniedTitles
var accessDeniedSelectors = browserpkg.AccessDeniedSelectors
var challengeTitles = browserpkg.ChallengeTitles
var challengeSelectors = browserpkg.ChallengeSelectors
var turnstileSelectors = browserpkg.TurnstileSelectors
var firstCookiePath = browserpkg.FirstCookiePath
var sleepContext = browserpkg.SleepContext
var tabbableTargets = browserpkg.TabbableTargets
var summarizeClickTarget = browserpkg.SummarizeClickTarget
var clickPointsForTarget = browserpkg.ClickPointsForTarget
var isVerifyButtonTarget = browserpkg.IsVerifyButtonTarget
var isChallengeIframeTarget = browserpkg.IsChallengeIframeTarget
var summarizeCandidateTargets = browserpkg.SummarizeCandidateTargets
var relevantChallengeTargets = browserpkg.RelevantChallengeTargets
var fallbackChallengeTargets = browserpkg.FallbackChallengeTargets
var chromeArgValue = browserpkg.ChromeArgValue

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

const webdriverTabKey = "\uE004"

type webDriverBrowser struct {
	cfg    Config
	logger Logger
	proxy  *Proxy

	httpClient        *http.Client
	baseURL           string
	sessionID         string
	effectiveHeadless bool

	driverCmd         *exec.Cmd
	patchedDriverDir  string
	patchedDriverPath string
	driverLogPath     string

	xvfbCmd         *exec.Cmd
	previousDisplay string
	userDataDir     string
	keepUserDataDir bool
	proxyExtDir     string

	mu sync.Mutex
}

func NewWebDriver(cfg Config, proxy *Proxy) (Client, error) {
	b := &webDriverBrowser{
		cfg:             cfg,
		logger:          cfg.Logger,
		proxy:           proxy,
		previousDisplay: os.Getenv("DISPLAY"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	if err := b.prepareUserDataDir(); err != nil {
		return nil, err
	}
	if err := b.prepareProxyExtension(); err != nil {
		_ = b.Close()
		return nil, err
	}
	if err := b.preparePatchedDriver(); err != nil {
		_ = b.Close()
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
	if err := b.installStealth(); err != nil {
		_ = b.Close()
		return nil, err
	}

	return b, nil
}

func (b *webDriverBrowser) UserAgent(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	return b.userAgent(runCtx)
}

func (b *webDriverBrowser) Resolve(ctx context.Context, req Request) (*Result, error) {
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

func (b *webDriverBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.deleteSession(context.Background())
	if b.driverCmd != nil && b.driverCmd.Process != nil {
		_ = b.driverCmd.Process.Kill()
		_, _ = b.driverCmd.Process.Wait()
		b.driverCmd = nil
	}
	if b.patchedDriverDir != "" {
		_ = os.RemoveAll(b.patchedDriverDir)
		b.patchedDriverDir = ""
	}
	if b.proxyExtDir != "" {
		_ = os.RemoveAll(b.proxyExtDir)
		b.proxyExtDir = ""
	}
	b.cleanupUserDataDir()
	b.stopDisplay()
	return nil
}

func (b *webDriverBrowser) preparePatchedDriver() error {
	driverPath := b.cfg.DriverPath
	if strings.TrimSpace(driverPath) == "" {
		return fmt.Errorf("chromedriver executable not found")
	}

	patchedPath, tempDir, err := patchChromeDriverBinary(driverPath)
	if err != nil {
		return err
	}
	b.patchedDriverPath = patchedPath
	b.patchedDriverDir = tempDir
	return nil
}

func (b *webDriverBrowser) startDriver() error {
	port, err := freeLocalPort()
	if err != nil {
		return fmt.Errorf("reserve chromedriver port: %w", err)
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

	b.driverLogPath = filepath.Join(b.patchedDriverDir, "chromedriver.log")
	cmd := exec.Command(
		b.patchedDriverPath,
		fmt.Sprintf("--port=%d", port),
		"--allowed-origins=*",
		"--verbose",
		"--log-path="+b.driverLogPath,
	)
	cmd.Env = env
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chromedriver: %w", err)
	}

	b.driverCmd = cmd
	b.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		statusCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := b.webDriverRequest(statusCtx, http.MethodGet, "/status", nil)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("chromedriver did not become ready")
}

func (b *webDriverBrowser) createSession() error {
	args := b.chromeArgs()
	b.logger.Debug("creating webdriver session", "browser_path", b.cfg.BrowserPath, "headless", b.cfg.Headless, "effective_headless", b.effectiveHeadless, "display", os.Getenv("DISPLAY"), "args", args)

	chromeOptions := map[string]any{
		"args":                   args,
		"excludeSwitches":        []string{"enable-automation"},
		"useAutomationExtension": false,
	}
	if b.cfg.BrowserPath != "" {
		chromeOptions["binary"] = b.cfg.BrowserPath
	}

	payload := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": map[string]any{
				"browserName":             "chrome",
				"acceptInsecureCerts":     true,
				"pageLoadStrategy":        "normal",
				"unhandledPromptBehavior": "ignore",
				"goog:chromeOptions":      chromeOptions,
			},
		},
	}

	raw, topSessionID, err := b.webDriverRequest(context.Background(), http.MethodPost, "/session", payload)
	if err != nil {
		if tail := b.driverLogTail(); tail != "" {
			return fmt.Errorf("create webdriver session: %w | chromedriver log: %s", err, tail)
		}
		return fmt.Errorf("create webdriver session: %w", err)
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
		return fmt.Errorf("webdriver session id missing")
	}

	b.sessionID = sessionID
	return nil
}

func (b *webDriverBrowser) chromeArgs() []string {
	args := []string{
		"--no-sandbox",
		"--window-size=1920,1080",
		"--disable-search-engine-choice-screen",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--no-zygote",
		"--ignore-certificate-errors",
		"--ignore-ssl-errors",
		"--disable-blink-features=AutomationControlled",
		"--remote-allow-origins=*",
		"--start-maximized",
	}
	if !b.effectiveHeadless && runtime.GOOS != "windows" {
		args = append(args, "--window-position=-2400,-2400")
	}
	if b.effectiveHeadless {
		args = append(args, "--headless=new")
	}
	if lang := os.Getenv("LANG"); strings.TrimSpace(lang) != "" {
		args = append(args, "--accept-lang="+lang, "--lang="+lang)
	}
	if ua := strings.TrimSpace(b.cfg.StartupUserAgent); ua != "" {
		args = append(args, "--user-agent="+ua)
	}
	if b.userDataDir != "" {
		args = append(args, "--user-data-dir="+b.userDataDir)
	}
	if b.proxyExtDir != "" {
		args = append(args,
			"--disable-features=DisableLoadExtensionCommandLineSwitch",
			"--load-extension="+b.proxyExtDir,
		)
	} else if b.proxy != nil && strings.TrimSpace(b.proxy.URL) != "" {
		args = append(args, "--proxy-server="+b.proxy.URL)
	}
	args = append(args, splitChromeArgs(os.Getenv("CHROME_ARGS"))...)
	return args
}

func (b *webDriverBrowser) installStealth() error {
	const stealthScript = `(() => {
		const safeDefine = (obj, prop, getter) => {
			if (!obj) return;
			try {
				Object.defineProperty(obj, prop, { get: getter, configurable: true });
			} catch (_) {}
		};

		try {
			Object.defineProperty(window, 'navigator', {
				value: new Proxy(navigator, {
					has: (target, key) => (key === 'webdriver' ? false : key in target),
					get: (target, key) => {
						if (key === 'webdriver') return false;
						const value = target[key];
						return typeof value === 'function' ? value.bind(target) : value;
					},
				}),
				configurable: true,
			});
		} catch (_) {}

		safeDefine(navigator, 'maxTouchPoints', () => 1);
		if (navigator.connection) {
			safeDefine(navigator.connection, 'rtt', () => 100);
		}
		safeDefine(navigator, 'languages', () => navigator.languages && navigator.languages.length ? navigator.languages : ['en-US', 'en']);
		safeDefine(navigator, 'plugins', () => navigator.plugins && navigator.plugins.length ? navigator.plugins : [1, 2, 3, 4, 5]);

		window.chrome = window.chrome || {
			app: { isInstalled: false },
			runtime: {},
		};
		if (!window.Notification) {
			window.Notification = { permission: 'denied' };
		}
		if (navigator.permissions && navigator.permissions.query) {
			const originalQuery = navigator.permissions.query.bind(navigator.permissions);
			navigator.permissions.__proto__.query = (parameters) =>
				parameters && parameters.name === 'notifications'
					? Promise.resolve({ state: window.Notification.permission })
					: originalQuery(parameters);
		}
	})();`

	if _, err := b.executeCDP(context.Background(), "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": stealthScript,
	}); err != nil {
		return err
	}

	currentUA, err := b.userAgent(context.Background())
	if err != nil {
		return err
	}
	overrideUA := strings.TrimSpace(b.cfg.StartupUserAgent)
	if overrideUA == "" {
		overrideUA = scrubUserAgent(currentUA)
	}
	if overrideUA != "" {
		_, _ = b.executeCDP(context.Background(), "Emulation.setUserAgentOverride", map[string]any{
			"userAgent":      overrideUA,
			"acceptLanguage": firstNonEmpty(os.Getenv("LANG"), "en-US"),
			"platform":       runtime.GOOS,
		})
	}

	return nil
}

func (b *webDriverBrowser) resolve(ctx context.Context, req Request) (*ChallengeResolutionResult, string, error) {
	if req.DisableMedia {
		_, _ = b.executeCDP(ctx, "Network.enable", map[string]any{})
		urls := make([]string, 0, len(blockedURLs))
		for _, pattern := range blockedURLs {
			urls = append(urls, normalizeBlockedPattern(pattern))
		}
		_, _ = b.executeCDP(ctx, "Network.setBlockedURLs", map[string]any{"urls": urls})
	}

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

	if req.LogHTML || b.cfg.LogHTML {
		htmlDoc, err := b.pageHTML(ctx)
		if err == nil {
			b.logger.Debug("response html", "html", htmlDoc)
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
		Status:    200,
		Cookies:   cookies,
		UserAgent: userAgent,
	}

	if req.TabsTillVerify != nil {
		token, err := b.resolveTurnstileToken(ctx, max(*req.TabsTillVerify, 1))
		if err != nil {
			return nil, "", fmt.Errorf("read turnstile token: %w", err)
		}
		result.TurnstileToken = token
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
		result.Headers = map[string]string{}
		result.Response = htmlDoc
	}

	if req.ReturnScreenshot {
		screenshot, err := b.screenshot(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("capture screenshot: %w", err)
		}
		result.Screenshot = screenshot
	}

	return result, message, nil
}

func (b *webDriverBrowser) navigate(ctx context.Context, req Request) error {
	targetURL := req.URL
	if strings.EqualFold(req.Method, "POST") {
		htmlDoc := buildPostFormHTML(req.URL, req.PostData)
		targetURL = "data:text/html;charset=utf-8," + url.PathEscape(htmlDoc)
	}
	_, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/url"), map[string]any{"url": targetURL})
	return err
}

func (b *webDriverBrowser) setCookies(ctx context.Context, rawURL string, cookies []Cookie) error {
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
		payload := map[string]any{
			"cookie": map[string]any{
				"name":     cookie.Name,
				"value":    cookie.Value,
				"path":     firstCookiePath(cookie.Path),
				"domain":   firstNonEmpty(cookie.Domain, domain),
				"secure":   cookie.Secure || secure,
				"httpOnly": cookie.HTTPOnly,
			},
		}
		if strings.TrimSpace(cookie.SameSite) != "" {
			payload["cookie"].(map[string]any)["sameSite"] = cookie.SameSite
		}
		if cookie.Expires > 0 {
			payload["cookie"].(map[string]any)["expiry"] = int64(cookie.Expires)
		}
		if _, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/cookie"), payload); err != nil {
			return err
		}
	}
	return nil
}

func (b *webDriverBrowser) solveChallenge(ctx context.Context) error {
	b.debugChallengeState(ctx, "challenge-detected")

	attempt := 0
	for {
		found, err := b.challengePresent(ctx)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}

		attempt++
		cleared, err := b.waitChallengeGone(ctx, time.Second)
		if err != nil {
			return err
		}
		if cleared {
			return nil
		}

		b.logger.Debug("timeout waiting for challenge to clear", "attempt", attempt)
		_ = b.clickVerify(ctx, 1)
	}
}

func (b *webDriverBrowser) resolveTurnstileToken(ctx context.Context, tabs int) (string, error) {
	for _, selector := range turnstileSelectors {
		exists, err := b.selectorExists(ctx, selector)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}

		for {
			token, err := b.readInputValue(ctx, selector)
			if err != nil {
				return "", err
			}
			if token != "" {
				return token, nil
			}
			if err := b.clickVerify(ctx, tabs); err != nil {
				return "", err
			}
			if err := b.focusHelperButton(ctx); err != nil {
				return "", err
			}
			if err := sleepContext(ctx, time.Second); err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

func (b *webDriverBrowser) clickVerify(ctx context.Context, tabs int) error {
	b.debugChallengeState(ctx, "before-click-verify")
	_ = b.switchToDefaultContent(ctx)

	if err := b.runWebDriverKeySequence(ctx, tabs); err != nil {
		b.logger.Debug("cloudflare verify key sequence failed", "err", err)
	}

	buttonClicked, err := b.clickVerifyHumanButton(ctx)
	if err != nil {
		b.logger.Debug("cloudflare verify human button click failed", "err", err)
	}
	if buttonClicked {
		b.logger.Debug("cloudflare verify human button clicked")
	}

	fallbackClicked, err := b.clickTabbableChallengeTarget(ctx, tabs)
	if err != nil {
		b.logger.Debug("webdriver tabbable target fallback failed", "err", err)
	}
	if fallbackClicked {
		b.logger.Debug("webdriver tabbable fallback clicked")
	}

	buttonClicked, err = b.clickVerifyButtons(ctx)
	if err != nil {
		b.logger.Debug("cloudflare verify button click failed", "err", err)
	}
	if buttonClicked {
		b.logger.Debug("cloudflare generic verify button clicked")
	}

	iframeClicked, err := b.clickChallengeIframes(ctx)
	if err != nil {
		b.logger.Debug("cloudflare challenge iframe click failed", "err", err)
	}
	if iframeClicked {
		b.logger.Debug("cloudflare iframe clicked")
	}

	_ = b.switchToDefaultContent(ctx)
	b.debugChallengeState(ctx, "after-click-verify")
	return sleepContext(ctx, 2*time.Second)
}

func (b *webDriverBrowser) runWebDriverKeySequence(ctx context.Context, tabs int) error {
	if err := b.prepareWebDriverInput(ctx); err != nil {
		return err
	}

	keyActions := make([]map[string]any, 0, 2*max(tabs, 1)+3)
	keyActions = append(keyActions, map[string]any{"type": "pause", "duration": 5000})
	for i := 0; i < max(tabs, 1); i++ {
		keyActions = append(keyActions,
			map[string]any{"type": "keyDown", "value": webdriverTabKey},
			map[string]any{"type": "keyUp", "value": webdriverTabKey},
			map[string]any{"type": "pause", "duration": 100},
		)
	}
	keyActions = append(keyActions,
		map[string]any{"type": "pause", "duration": 1000},
		map[string]any{"type": "keyDown", "value": " "},
		map[string]any{"type": "keyUp", "value": " "},
	)

	if err := b.performActions(ctx, []map[string]any{
		{
			"type":    "key",
			"id":      "keyboard",
			"actions": keyActions,
		},
	}); err != nil {
		return err
	}
	return b.releaseActions(ctx)
}

func (b *webDriverBrowser) prepareWebDriverInput(ctx context.Context) error {
	_, _ = b.executeCDP(ctx, "Page.bringToFront", map[string]any{})
	_, _ = b.executeCDP(ctx, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	_, _ = b.executeScript(ctx, `(() => {
		try { window.focus(); } catch (_) {}
		try {
			if (document.body && typeof document.body.focus === 'function') {
				document.body.focus();
			}
		} catch (_) {}
		return document.hasFocus ? document.hasFocus() : true;
	})()`)
	return nil
}

func (b *webDriverBrowser) clickTabbableChallengeTarget(ctx context.Context, tabs int) (bool, error) {
	targets, err := b.clickTargets(ctx)
	if err != nil {
		return false, err
	}

	tabbables := tabbableTargets(targets)
	if len(tabbables) == 0 {
		return false, nil
	}

	index := max(tabs, 1) - 1
	if index >= len(tabbables) {
		index = len(tabbables) - 1
	}
	target := tabbables[index]
	b.logger.Debug("webdriver tabbable fallback target", "target", summarizeClickTarget(target))

	if err := b.focusTarget(ctx, target); err != nil {
		b.logger.Debug("focus target failed", "target", summarizeClickTarget(target), "err", err)
	}

	if target.Visible && target.Width > 0 && target.Height > 0 {
		for _, candidate := range clickPointsForTarget(target) {
			if err := b.clickAt(ctx, candidate); err == nil {
				return true, nil
			}
		}
	}

	if err := b.performActions(ctx, []map[string]any{
		{
			"type": "key",
			"id":   "keyboard",
			"actions": []map[string]any{
				{"type": "keyDown", "value": " "},
				{"type": "keyUp", "value": " "},
			},
		},
	}); err != nil {
		return false, err
	}
	_ = b.releaseActions(ctx)
	return true, nil
}

func (b *webDriverBrowser) focusTarget(ctx context.Context, target clickTarget) error {
	script := fmt.Sprintf(`(() => {
		const want = {
			tag: %q,
			id: %q,
			name: %q,
			title: %q,
			role: %q,
			ariaLabel: %q,
			className: %q,
			left: %f,
			top: %f,
			width: %f,
			height: %f
		};

		const visible = (el) => {
			if (!el || !el.getBoundingClientRect) return false;
			const rect = el.getBoundingClientRect();
			const style = window.getComputedStyle(el);
			return rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
		};

		const matches = (el) => {
			if (!el || !el.getBoundingClientRect) return false;
			const rect = el.getBoundingClientRect();
			if ((el.tagName || '').toLowerCase() !== want.tag) return false;
			if (want.id && el.id !== want.id) return false;
			if (want.name && (el.getAttribute('name') || '') !== want.name) return false;
			if (want.title && (el.getAttribute('title') || '') !== want.title) return false;
			if (want.role && (el.getAttribute('role') || '') !== want.role) return false;
			if (want.ariaLabel && (el.getAttribute('aria-label') || '') !== want.ariaLabel) return false;
			if (want.className && (el.className || '') !== want.className) return false;
			return Math.abs(rect.left - want.left) < 2 &&
				Math.abs(rect.top - want.top) < 2 &&
				Math.abs(rect.width - want.width) < 2 &&
				Math.abs(rect.height - want.height) < 2;
		};

		const visited = new Set();
		const walk = (root) => {
			if (!root || visited.has(root) || !root.querySelectorAll) return null;
			visited.add(root);
			for (const el of root.querySelectorAll('*')) {
				if (el.shadowRoot) {
					const found = walk(el.shadowRoot);
					if (found) return found;
				}
				if (!visible(el)) continue;
				if (matches(el)) return el;
			}
			return null;
		};

		const found = walk(document);
		if (!found || typeof found.focus !== 'function') return false;
		found.focus();
		return true;
	})()`, target.Tag, target.ID, target.Name, target.Title, target.Role, target.AriaLabel, target.ClassName, target.Left, target.Top, target.Width, target.Height)

	_, err := b.executeScript(ctx, script)
	return err
}

func (b *webDriverBrowser) focusHelperButton(ctx context.Context) error {
	_, err := b.executeScript(ctx, `(() => {
		let el = document.getElementById('__flaresolverr-focus');
		if (!el) {
			el = document.createElement('button');
			el.id = '__flaresolverr-focus';
			el.style.position = 'fixed';
			el.style.top = '0';
			el.style.left = '0';
			document.body.prepend(el);
		}
		el.focus();
		return true;
	})()`)
	return err
}

func (b *webDriverBrowser) clickVerifyButtons(ctx context.Context) (bool, error) {
	targets, err := b.clickTargets(ctx)
	if err != nil {
		return false, err
	}

	clicked := false
	for _, target := range targets {
		if !isVerifyButtonTarget(target) {
			continue
		}
		for _, candidate := range clickPointsForTarget(target) {
			if err := b.clickAt(ctx, candidate); err != nil {
				b.logger.Debug("verify button click attempt failed", "target", summarizeClickTarget(target), "err", err)
				continue
			}
			clicked = true
		}
	}
	return clicked, nil
}

func (b *webDriverBrowser) clickVerifyHumanButton(ctx context.Context) (bool, error) {
	const script = `(() => {
		const node = document.evaluate(
			"//input[@type='button' and @value='Verify you are human']",
			document,
			null,
			XPathResult.FIRST_ORDERED_NODE_TYPE,
			null
		).singleNodeValue;
		if (!node || !node.getBoundingClientRect) return null;
		const rect = node.getBoundingClientRect();
		return {
			kind: "input",
			tag: (node.tagName || "").toLowerCase(),
			type: node.getAttribute ? (node.getAttribute("type") || "") : "",
			text: node.value || "",
			id: node.id || "",
			name: node.getAttribute ? (node.getAttribute("name") || "") : "",
			title: node.getAttribute ? (node.getAttribute("title") || "") : "",
			ariaLabel: node.getAttribute ? (node.getAttribute("aria-label") || "") : "",
			role: node.getAttribute ? (node.getAttribute("role") || "") : "",
			className: typeof node.className === "string" ? node.className : "",
			tabIndex: typeof node.tabIndex === "number" ? node.tabIndex : -1,
			disabled: !!node.disabled || (node.getAttribute && node.getAttribute("aria-disabled") === "true"),
			src: "",
			left: rect.left,
			top: rect.top,
			width: rect.width,
			height: rect.height,
			visible: rect.width > 0 && rect.height > 0
		};
	})()`

	raw, err := b.executeScript(ctx, script)
	if err != nil {
		return false, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}

	var target clickTarget
	if err := json.Unmarshal(raw, &target); err != nil {
		return false, err
	}
	for _, candidate := range clickPointsForTarget(target) {
		if err := b.clickAt(ctx, candidate); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (b *webDriverBrowser) clickChallengeIframes(ctx context.Context) (bool, error) {
	targets, err := b.clickTargets(ctx)
	if err != nil {
		return false, err
	}

	clicked := false
	for _, target := range targets {
		if !isChallengeIframeTarget(target) {
			continue
		}
		for _, candidate := range clickPointsForTarget(target) {
			if err := b.clickAt(ctx, candidate); err != nil {
				b.logger.Debug("iframe click attempt failed", "target", summarizeClickTarget(target), "err", err)
				continue
			}
			clicked = true
			cleared, err := b.waitChallengeClear(ctx, 1500*time.Millisecond)
			if err == nil && cleared {
				return true, nil
			}
		}
	}
	return clicked, nil
}

func (b *webDriverBrowser) clickAt(ctx context.Context, p point) error {
	return b.performActions(ctx, []map[string]any{
		{
			"type": "pointer",
			"id":   "mouse",
			"parameters": map[string]any{
				"pointerType": "mouse",
			},
			"actions": []map[string]any{
				{
					"type":     "pointerMove",
					"duration": 0,
					"x":        int(p.X),
					"y":        int(p.Y),
					"origin":   "viewport",
				},
				{"type": "pause", "duration": 150},
				{"type": "pointerDown", "button": 0},
				{"type": "pointerUp", "button": 0},
			},
		},
	})
}

func (b *webDriverBrowser) waitChallengeClear(ctx context.Context, d time.Duration) (bool, error) {
	if err := sleepContext(ctx, d); err != nil {
		return false, err
	}
	found, err := b.challengePresent(ctx)
	if err != nil {
		return false, err
	}
	return !found, nil
}

func (b *webDriverBrowser) waitChallengeGone(ctx context.Context, d time.Duration) (bool, error) {
	deadline := time.Now().Add(d)
	for {
		found, err := b.challengePresent(ctx)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
			return false, err
		}
	}
}

func (b *webDriverBrowser) clickTargets(ctx context.Context) ([]clickTarget, error) {
	raw, err := b.executeScript(ctx, `(() => {
		const results = [];
		const visited = new Set();

		const pushTarget = (el, kind) => {
			if (!el || !el.getBoundingClientRect) return;
			const rect = el.getBoundingClientRect();
			const style = window.getComputedStyle(el);
			const text = kind === 'input' ? (el.value || '') : (el.innerText || el.textContent || '');
			const tag = (el.tagName || '').toLowerCase();
			results.push({
				kind,
				tag,
				type: el.getAttribute ? (el.getAttribute('type') || '') : '',
				text: (text || '').trim(),
				id: el.id || '',
				name: el.getAttribute ? (el.getAttribute('name') || '') : '',
				title: el.getAttribute ? (el.getAttribute('title') || '') : '',
				ariaLabel: el.getAttribute ? (el.getAttribute('aria-label') || '') : '',
				role: el.getAttribute ? (el.getAttribute('role') || '') : '',
				className: typeof el.className === 'string' ? el.className : '',
				tabIndex: typeof el.tabIndex === 'number' ? el.tabIndex : -1,
				disabled: !!el.disabled || (el.getAttribute && el.getAttribute('aria-disabled') === 'true'),
				src: kind === 'iframe' ? (el.src || (el.getAttribute && el.getAttribute('src')) || '') : '',
				left: rect.left,
				top: rect.top,
				width: rect.width,
				height: rect.height,
				visible: rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0'
			});
		};

		const walk = (root) => {
			if (!root || visited.has(root) || !root.querySelectorAll) return;
			visited.add(root);
			for (const el of root.querySelectorAll('*')) {
				if (el.shadowRoot) walk(el.shadowRoot);
				const tag = (el.tagName || '').toLowerCase();
				if (tag === 'iframe' || tag === 'button' || tag === 'input' || tag === 'textarea' || tag === 'select') {
					pushTarget(el, tag);
					continue;
				}
				if (tag === 'a' && el.href) {
					pushTarget(el, 'anchor');
					continue;
				}
				if (typeof el.tabIndex === 'number' && el.tabIndex >= 0) {
					pushTarget(el, 'tabindex');
					continue;
				}
				if (el.getAttribute && el.getAttribute('role') === 'button') {
					pushTarget(el, 'role_button');
				}
			}
		};

		walk(document);
		return results;
	})()`)
	if err != nil {
		return nil, err
	}

	var targets []clickTarget
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func (b *webDriverBrowser) debugChallengeState(ctx context.Context, stage string) {
	activeElement, _ := b.executeString(ctx, `(() => {
		const el = document.activeElement;
		if (!el) return '';
		const bits = [el.tagName || '', el.id ? ('#' + el.id) : '', el.getAttribute ? (el.getAttribute('name') || '') : ''];
		return bits.filter(Boolean).join(' ');
	})()`)
	hasFocus, _ := b.executeBool(ctx, `document.hasFocus ? document.hasFocus() : true`)

	targets, err := b.clickTargets(ctx)
	if err != nil {
		b.logger.Debug("challenge state", "stage", stage, "activeElement", activeElement, "hasFocus", hasFocus, "targetsErr", err)
		return
	}

	relevant := summarizeCandidateTargets(relevantChallengeTargets(targets))
	fallback := summarizeCandidateTargets(fallbackChallengeTargets(targets))
	tabs := summarizeCandidateTargets(tabbableTargets(targets))

	b.logger.Debug("challenge state", "stage", stage, "activeElement", activeElement, "hasFocus", hasFocus, "targets", relevant, "controls", fallback, "tabStops", tabs)
}

func (b *webDriverBrowser) challengePresent(ctx context.Context) (bool, error) {
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

func (b *webDriverBrowser) pageTitle(ctx context.Context) (string, error) {
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

func (b *webDriverBrowser) currentURL(ctx context.Context) (string, error) {
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

func (b *webDriverBrowser) pageHTML(ctx context.Context) (string, error) {
	return b.executeString(ctx, `document.documentElement ? document.documentElement.outerHTML : ''`)
}

func (b *webDriverBrowser) selectorExists(ctx context.Context, selector string) (bool, error) {
	return b.executeBool(ctx, fmt.Sprintf(`document.querySelector(%q) !== null`, selector))
}

func (b *webDriverBrowser) readInputValue(ctx context.Context, selector string) (string, error) {
	return b.executeString(ctx, fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		return el ? (el.value || '') : '';
	})()`, selector))
}

func (b *webDriverBrowser) currentCookies(ctx context.Context) ([]Cookie, error) {
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

func (b *webDriverBrowser) screenshot(ctx context.Context) (string, error) {
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

func (b *webDriverBrowser) userAgent(ctx context.Context) (string, error) {
	ua, err := b.executeString(ctx, `navigator.userAgent`)
	if err != nil {
		return "", err
	}
	return scrubUserAgent(ua), nil
}

func (b *webDriverBrowser) executeString(ctx context.Context, script string) (string, error) {
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

func (b *webDriverBrowser) executeBool(ctx context.Context, script string) (bool, error) {
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

func (b *webDriverBrowser) executeScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	raw, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/execute/sync"), map[string]any{
		"script": script,
		"args":   args,
	})
	return raw, err
}

func (b *webDriverBrowser) executeCDP(ctx context.Context, cmd string, params map[string]any) (json.RawMessage, error) {
	raw, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/goog/cdp/execute"), map[string]any{
		"cmd":    cmd,
		"params": params,
	})
	return raw, err
}

func (b *webDriverBrowser) performActions(ctx context.Context, actions []map[string]any) error {
	_, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/actions"), map[string]any{
		"actions": actions,
	})
	return err
}

func (b *webDriverBrowser) releaseActions(ctx context.Context) error {
	_, _, err := b.webDriverRequest(ctx, http.MethodDelete, b.sessionPath("/actions"), nil)
	return err
}

func (b *webDriverBrowser) sessionPath(path string) string {
	return "/session/" + b.sessionID + path
}

func (b *webDriverBrowser) switchToDefaultContent(ctx context.Context) error {
	_, _, err := b.webDriverRequest(ctx, http.MethodPost, b.sessionPath("/frame"), map[string]any{
		"id": nil,
	})
	return err
}

func (b *webDriverBrowser) driverLogTail() string {
	if strings.TrimSpace(b.driverLogPath) == "" {
		return ""
	}
	data, err := os.ReadFile(b.driverLogPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	start := 0
	if len(lines) > 12 {
		start = len(lines) - 12
	}
	return strings.Join(lines[start:], " | ")
}

func (b *webDriverBrowser) webDriverRequest(ctx context.Context, method, path string, payload any) (json.RawMessage, string, error) {
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
				return nil, "", fmt.Errorf("webdriver http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
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
		return nil, envelope.SessionID, fmt.Errorf("webdriver http %d", resp.StatusCode)
	}

	return envelope.Value, envelope.SessionID, nil
}

func (b *webDriverBrowser) deleteSession(ctx context.Context) {
	if strings.TrimSpace(b.sessionID) == "" || strings.TrimSpace(b.baseURL) == "" {
		return
	}
	_, _, _ = b.webDriverRequest(ctx, http.MethodDelete, b.sessionPath(""), nil)
	b.sessionID = ""
}

func (b *webDriverBrowser) prepareHeadlessMode() (bool, string, error) {
	if !b.cfg.Headless || runtime.GOOS == "windows" {
		return b.cfg.Headless, "", nil
	}
	if display := os.Getenv("DISPLAY"); display != "" {
		return false, display, nil
	}
	xvfbPath, err := exec.LookPath("Xvfb")
	if err != nil {
		b.logger.Warn("HEADLESS=true without DISPLAY or Xvfb; falling back to Chrome headless mode")
		return true, "", nil
	}
	display, err := b.startDisplay(xvfbPath)
	if err != nil {
		return false, "", err
	}
	return false, display, nil
}

func (b *webDriverBrowser) startDisplay(xvfbPath string) (string, error) {
	for displayNumber := 99; displayNumber < 120; displayNumber++ {
		socketPath := fmt.Sprintf("/tmp/.X11-unix/X%d", displayNumber)
		if _, err := os.Stat(socketPath); err == nil {
			continue
		}

		display := fmt.Sprintf(":%d", displayNumber)
		cmd := exec.Command(xvfbPath, display, "-screen", "0", "1920x1080x24", "-nolisten", "tcp")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			continue
		}

		for range 50 {
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				break
			}
			if _, err := os.Stat(socketPath); err == nil {
				b.xvfbCmd = cmd
				return display, nil
			}
			time.Sleep(100 * time.Millisecond)
		}

		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	return "", fmt.Errorf("start Xvfb: no usable display found")
}

func (b *webDriverBrowser) stopDisplay() {
	if b.xvfbCmd != nil && b.xvfbCmd.Process != nil {
		_ = b.xvfbCmd.Process.Kill()
		_, _ = b.xvfbCmd.Process.Wait()
		b.xvfbCmd = nil
	}
	if b.previousDisplay == "" {
		_ = os.Unsetenv("DISPLAY")
	} else {
		_ = os.Setenv("DISPLAY", b.previousDisplay)
	}
}

func (b *webDriverBrowser) prepareUserDataDir() error {
	customDir := chromeArgValue(os.Getenv("CHROME_ARGS"), "user-data-dir")
	if customDir != "" {
		b.userDataDir = customDir
		b.keepUserDataDir = true
		return nil
	}

	dir, err := os.MkdirTemp("", "flaresolverr-go-profile-*")
	if err != nil {
		return fmt.Errorf("create browser profile dir: %w", err)
	}
	b.userDataDir = dir
	b.keepUserDataDir = false

	defaultDir := filepath.Join(dir, "Default")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("prepare browser profile dir: %w", err)
	}
	return nil
}

func (b *webDriverBrowser) cleanupUserDataDir() {
	if b.keepUserDataDir || strings.TrimSpace(b.userDataDir) == "" {
		return
	}
	_ = os.RemoveAll(b.userDataDir)
	b.userDataDir = ""
}

func (b *webDriverBrowser) prepareProxyExtension() error {
	if b.proxy == nil || strings.TrimSpace(b.proxy.URL) == "" || strings.TrimSpace(b.proxy.Username) == "" {
		return nil
	}

	parsed, err := url.Parse(b.proxy.URL)
	if err != nil {
		return fmt.Errorf("parse proxy url: %w", err)
	}
	if parsed.Hostname() == "" || parsed.Port() == "" {
		return fmt.Errorf("proxy url must include host and port")
	}

	dir, err := os.MkdirTemp("", "flaresolverr-go-proxy-ext-*")
	if err != nil {
		return fmt.Errorf("create proxy extension dir: %w", err)
	}

	manifest := `{
  "version": "1.0.0",
  "manifest_version": 3,
  "name": "Chrome Proxy",
  "permissions": ["proxy", "tabs", "storage", "webRequest", "webRequestAuthProvider"],
  "host_permissions": ["<all_urls>"],
  "background": { "service_worker": "background.js" },
  "minimum_chrome_version": "76.0.0"
}`

	background := fmt.Sprintf(`var config = {
  mode: "fixed_servers",
  rules: {
    singleProxy: {
      scheme: %q,
      host: %q,
      port: %s
    },
    bypassList: ["localhost"]
  }
};

chrome.proxy.settings.set({value: config, scope: "regular"}, function() {});

function callbackFn(details) {
  return {
    authCredentials: {
      username: %q,
      password: %q
    }
  };
}

chrome.webRequest.onAuthRequired.addListener(
  callbackFn,
  { urls: ["<all_urls>"] },
  ["blocking"]
);`, parsed.Scheme, parsed.Hostname(), parsed.Port(), b.proxy.Username, b.proxy.Password)

	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "background.js"), []byte(background), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}

	b.proxyExtDir = dir
	return nil
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func splitChromeArgs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.Fields(raw)
	args := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if strings.HasPrefix(field, "--") {
			args = append(args, field)
		}
	}
	return args
}
