package chromedpbackend

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	browserpkg "flaresolverr-go/server/browser"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

type Config = browserpkg.Config
type Proxy = browserpkg.Proxy
type Cookie = browserpkg.Cookie
type ChallengeResolutionResult = browserpkg.ChallengeResolutionResult
type Logger = browserpkg.Logger
type Request = browserpkg.Request
type Result = browserpkg.Result
type Client = browserpkg.Client

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var (
	headlessUserAgentRE = regexp.MustCompile(`(?i)HEADLESS`)

	accessDeniedTitles = []string{
		"Access denied",
		"Attention Required! | Cloudflare",
	}
	accessDeniedSelectors = []string{
		"div.cf-error-title span.cf-code-label span",
		"#cf-error-details div.cf-error-overview h1",
	}
	challengeTitles = []string{
		"Just a moment...",
		"DDoS-Guard",
	}
	challengeSelectors = []string{
		"#cf-challenge-running",
		".ray_id",
		".attack-box",
		"#cf-please-wait",
		"#challenge-spinner",
		"#trk_jschal_js",
		"#turnstile-wrapper",
		".lds-ring",
		"td.info #js_info",
		"div.vc div.text-box h2",
	}
	turnstileSelectors = []string{
		"input[name='cf-turnstile-response']",
	}
	blockedURLs = []string{
		"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.bmp", "*.svg", "*.ico",
		"*.PNG", "*.JPG", "*.JPEG", "*.GIF", "*.WEBP", "*.BMP", "*.SVG", "*.ICO",
		"*.tiff", "*.tif", "*.jpe", "*.apng", "*.avif", "*.heic", "*.heif",
		"*.TIFF", "*.TIF", "*.JPE", "*.APNG", "*.AVIF", "*.HEIC", "*.HEIF",
		"*.css", "*.CSS",
		"*.woff", "*.woff2", "*.ttf", "*.otf", "*.eot",
		"*.WOFF", "*.WOFF2", "*.TTF", "*.OTF", "*.EOT",
	}
)

type chromedpBrowser struct {
	cfg    Config
	logger Logger
	proxy  *Proxy

	allocCtx    context.Context
	allocCancel context.CancelFunc
	browserCtx  context.Context
	browserStop context.CancelFunc

	xvfbCmd         *exec.Cmd
	previousDisplay string
	userDataDir     string
	keepUserDataDir bool

	mu sync.Mutex
}

type clickTarget struct {
	Kind      string  `json:"kind"`
	Tag       string  `json:"tag"`
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Title     string  `json:"title"`
	AriaLabel string  `json:"ariaLabel"`
	Role      string  `json:"role"`
	ClassName string  `json:"className"`
	TabIndex  int64   `json:"tabIndex"`
	Disabled  bool    `json:"disabled"`
	Src       string  `json:"src"`
	Left      float64 `json:"left"`
	Top       float64 `json:"top"`
	Width     float64 `json:"width"`
	Height    float64 `json:"height"`
	Visible   bool    `json:"visible"`
}

type point struct {
	X float64
	Y float64
}

func NewChromedp(cfg Config, proxy *Proxy) (Client, error) {
	b := &chromedpBrowser{
		cfg:             cfg,
		logger:          cfg.Logger,
		proxy:           proxy,
		previousDisplay: os.Getenv("DISPLAY"),
	}

	if err := b.prepareUserDataDir(); err != nil {
		return nil, err
	}

	opts, err := b.execAllocatorOptions()
	if err != nil {
		b.cleanupUserDataDir()
		b.stopDisplay()
		return nil, err
	}

	b.allocCtx, b.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	b.browserCtx, b.browserStop = chromedp.NewContext(b.allocCtx)

	if err := chromedp.Run(b.browserCtx); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("start chrome: %w", err)
	}

	if err := b.installStealth(); err != nil {
		_ = b.Close()
		return nil, err
	}

	if err := b.enableProxyAuth(); err != nil {
		_ = b.Close()
		return nil, err
	}

	return b, nil
}

func (b *chromedpBrowser) execAllocatorOptions() ([]chromedp.ExecAllocatorOption, error) {
	env := os.Environ()
	effectiveHeadless, display, err := b.prepareHeadlessMode()
	if err != nil {
		return nil, err
	}
	if display != "" {
		env = appendWithEnv(env, "DISPLAY", display)
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.WindowSize(1920, 1080),
		chromedp.Flag("disable-search-engine-choice-screen", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-zygote", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.Flag("ignore-ssl-errors", true),
		chromedp.Flag("remote-allow-origins", "*"),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("start-maximized", true),
		chromedp.Env(env...),
	}

	if b.cfg.BrowserPath != "" {
		opts = append(opts, chromedp.ExecPath(b.cfg.BrowserPath))
	}
	if lang := os.Getenv("LANG"); lang != "" {
		opts = append(opts, chromedp.Flag("accept-lang", lang))
		opts = append(opts, chromedp.Flag("lang", lang))
	}
	if !effectiveHeadless && runtime.GOOS != "windows" {
		opts = append(opts, chromedp.Flag("window-position", "-2400,-2400"))
	}
	if effectiveHeadless {
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	if ua := strings.TrimSpace(b.cfg.StartupUserAgent); ua != "" {
		opts = append(opts, chromedp.Flag("user-agent", ua))
	}
	if b.userDataDir != "" {
		opts = append(opts, chromedp.Flag("user-data-dir", b.userDataDir))
	}
	if b.proxy != nil && b.proxy.URL != "" {
		opts = append(opts, chromedp.Flag("proxy-server", b.proxy.URL))
	}
	opts = append(opts, parseChromeArgs(os.Getenv("CHROME_ARGS"))...)

	return opts, nil
}

func (b *chromedpBrowser) installStealth() error {
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
			app: {
				isInstalled: false,
				InstallState: {
					DISABLED: 'disabled',
					INSTALLED: 'installed',
					NOT_INSTALLED: 'not_installed',
				},
				RunningState: {
					CANNOT_RUN: 'cannot_run',
					READY_TO_RUN: 'ready_to_run',
					RUNNING: 'running',
				},
			},
			runtime: {
				OnInstalledReason: {
					CHROME_UPDATE: 'chrome_update',
					INSTALL: 'install',
					SHARED_MODULE_UPDATE: 'shared_module_update',
					UPDATE: 'update',
				},
				OnRestartRequiredReason: {
					APP_UPDATE: 'app_update',
					OS_UPDATE: 'os_update',
					PERIODIC: 'periodic',
				},
				PlatformArch: {
					ARM: 'arm',
					ARM64: 'arm64',
					MIPS: 'mips',
					MIPS64: 'mips64',
					X86_32: 'x86-32',
					X86_64: 'x86-64',
				},
				PlatformNaclArch: {
					ARM: 'arm',
					MIPS: 'mips',
					MIPS64: 'mips64',
					X86_32: 'x86-32',
					X86_64: 'x86-64',
				},
				PlatformOs: {
					ANDROID: 'android',
					CROS: 'cros',
					LINUX: 'linux',
					MAC: 'mac',
					OPENBSD: 'openbsd',
					WIN: 'win',
				},
				RequestUpdateCheckStatus: {
					NO_UPDATE: 'no_update',
					THROTTLED: 'throttled',
					UPDATE_AVAILABLE: 'update_available',
				},
			},
		};
		window.chrome.app = window.chrome.app || { isInstalled: false };
		window.chrome.runtime = window.chrome.runtime || {};

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

		const oldCall = Function.prototype.call;
		function patchedCall() {
			return oldCall.apply(this, arguments);
		}
		Function.prototype.call = patchedCall;

		const nativeToStringFunctionString = Error.toString().replace(/Error/g, 'toString');
		const oldToString = Function.prototype.toString;
		function functionToString() {
			if (this === navigator.permissions.query) {
				return 'function query() { [native code] }';
			}
			if (this === functionToString) {
				return nativeToStringFunctionString;
			}
			return oldCall.call(oldToString, this);
		}
		Function.prototype.toString = functionToString;

		const patchWebGL = (ctor) => {
			if (!ctor || !ctor.prototype || !ctor.prototype.getParameter) return;
			const original = ctor.prototype.getParameter;
			ctor.prototype.getParameter = function(parameter) {
				if (parameter === 37445) return 'Intel Inc.';
				if (parameter === 37446) return 'Intel Iris OpenGL Engine';
				return original.apply(this, arguments);
			};
		};

		patchWebGL(window.WebGLRenderingContext);
		patchWebGL(window.WebGL2RenderingContext);
	})();`

	return chromedp.Run(
		b.browserCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx)
			return err
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var currentUA string
			if err := chromedp.Evaluate(`navigator.userAgent`, &currentUA).Do(ctx); err != nil {
				return err
			}

			overrideUA := strings.TrimSpace(b.cfg.StartupUserAgent)
			if overrideUA == "" {
				overrideUA = scrubUserAgent(currentUA)
			}
			if overrideUA == "" || overrideUA == currentUA {
				return nil
			}

			if err := emulation.SetFocusEmulationEnabled(true).Do(ctx); err != nil {
				b.logger.Debug("focus emulation setup failed", "err", err)
			}
			if err := emulation.SetUserAgentOverride(overrideUA).WithAcceptLanguage(firstNonEmpty(os.Getenv("LANG"), "en-US")).WithPlatform(runtime.GOOS).Do(ctx); err != nil {
				b.logger.Debug("user agent override failed", "err", err)
				return nil
			}
			return nil
		}),
	)
}

func (b *chromedpBrowser) prepareHeadlessMode() (bool, string, error) {
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

func (b *chromedpBrowser) startDisplay(xvfbPath string) (string, error) {
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

func (b *chromedpBrowser) enableProxyAuth() error {
	if b.proxy == nil || b.proxy.Username == "" {
		return nil
	}

	chromedp.ListenTarget(b.browserCtx, func(ev any) {
		switch e := ev.(type) {
		case *fetch.EventRequestPaused:
			go func() {
				ctx, cancel := context.WithTimeout(b.browserCtx, 5*time.Second)
				defer cancel()
				_ = chromedp.Run(ctx, fetch.ContinueRequest(e.RequestID))
			}()
		case *fetch.EventAuthRequired:
			go func() {
				ctx, cancel := context.WithTimeout(b.browserCtx, 5*time.Second)
				defer cancel()
				response := &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseDefault}
				if e.AuthChallenge != nil && e.AuthChallenge.Source == fetch.AuthChallengeSourceProxy {
					response = &fetch.AuthChallengeResponse{
						Response: fetch.AuthChallengeResponseResponseProvideCredentials,
						Username: b.proxy.Username,
						Password: b.proxy.Password,
					}
				}
				_ = chromedp.Run(ctx, fetch.ContinueWithAuth(e.RequestID, response))
			}()
		}
	})

	return chromedp.Run(
		b.browserCtx,
		fetch.Enable().WithHandleAuthRequests(true).WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}),
	)
}

func (b *chromedpBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.browserStop != nil {
		b.browserStop()
		b.browserStop = nil
	}
	if b.allocCancel != nil {
		b.allocCancel()
		b.allocCancel = nil
	}
	b.cleanupUserDataDir()
	b.stopDisplay()
	return nil
}

func (b *chromedpBrowser) stopDisplay() {
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

func (b *chromedpBrowser) prepareUserDataDir() error {
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

func (b *chromedpBrowser) cleanupUserDataDir() {
	if b.keepUserDataDir || strings.TrimSpace(b.userDataDir) == "" {
		return
	}
	_ = os.RemoveAll(b.userDataDir)
	b.userDataDir = ""
}

func (b *chromedpBrowser) UserAgent(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	runCtx, cancel := b.newRunContext(ctx, 15*time.Second)
	defer cancel()

	return b.userAgent(runCtx)
}

func (b *chromedpBrowser) userAgent(ctx context.Context) (string, error) {
	var ua string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &ua)); err != nil {
		return "", err
	}
	return scrubUserAgent(ua), nil
}

func (b *chromedpBrowser) Resolve(ctx context.Context, req Request) (*Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	timeout := time.Duration(max(req.MaxTimeoutMS, 1)) * time.Millisecond
	runCtx, cancel := b.newRunContext(ctx, timeout)
	defer cancel()

	result, message, err := b.resolve(runCtx, req)
	if err != nil {
		return nil, err
	}
	return &Result{Result: result, Message: message}, nil
}

func (b *chromedpBrowser) newRunContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	runCtx, runCancel := context.WithCancel(b.browserCtx)
	stopForward := make(chan struct{})
	if parent != nil {
		go func() {
			select {
			case <-parent.Done():
				runCancel()
			case <-stopForward:
			}
		}()
	}
	if timeout > 0 {
		timeoutCtx, timeoutCancel := context.WithTimeout(runCtx, timeout)
		return timeoutCtx, func() {
			close(stopForward)
			timeoutCancel()
			runCancel()
		}
	}
	return runCtx, func() {
		close(stopForward)
		runCancel()
	}
}

func (b *chromedpBrowser) resolve(ctx context.Context, req Request) (*ChallengeResolutionResult, string, error) {
	if req.DisableMedia {
		patterns := make([]*network.BlockPattern, 0, len(blockedURLs))
		for _, pattern := range blockedURLs {
			patterns = append(patterns, &network.BlockPattern{
				URLPattern: normalizeBlockedPattern(pattern),
				Block:      true,
			})
		}
		if err := chromedp.Run(ctx,
			network.Enable(),
			network.SetBlockedURLs().WithURLPatterns(patterns),
		); err != nil {
			b.logger.Debug("Network.setBlockedURLs failed", "err", err)
		}
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
	cookies, err := b.currentCookies(ctx, currentURL)
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
		var screenshot []byte
		if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&screenshot)); err != nil {
			return nil, "", fmt.Errorf("capture screenshot: %w", err)
		}
		result.Screenshot = encodeBase64(screenshot)
	}

	return result, message, nil
}

func (b *chromedpBrowser) navigate(ctx context.Context, req Request) error {
	if strings.EqualFold(req.Method, "POST") {
		htmlDoc := buildPostFormHTML(req.URL, req.PostData)
		return chromedp.Run(ctx, chromedp.Navigate("data:text/html;charset=utf-8,"+url.PathEscape(htmlDoc)))
	}
	return chromedp.Run(ctx, chromedp.Navigate(req.URL))
}

func (b *chromedpBrowser) setCookies(ctx context.Context, rawURL string, cookies []Cookie) error {
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
		action := network.SetCookie(cookie.Name, cookie.Value).
			WithPath(firstCookiePath(cookie.Path)).
			WithSecure(cookie.Secure || secure).
			WithHTTPOnly(cookie.HTTPOnly)
		if cookie.Domain != "" {
			action = action.WithDomain(cookie.Domain)
		} else if domain != "" {
			action = action.WithDomain(domain)
		}
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(inner context.Context) error {
			return action.WithURL(rawURL).Do(inner)
		})); err != nil {
			return err
		}
	}

	return nil
}

func (b *chromedpBrowser) solveChallenge(ctx context.Context) error {
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
		targets, err := b.clickTargets(ctx)
		if err != nil {
			b.logger.Debug("challenge target collection failed", "attempt", attempt, "err", err)
			targets = nil
		}
		relevant := relevantChallengeTargets(targets)
		waitFor := challengeAutoWaitDuration(relevant)

		cleared, err := b.waitChallengeGone(ctx, waitFor)
		if err != nil {
			return err
		}
		if cleared {
			return nil
		}

		if len(relevant) == 0 {
			b.logger.Debug("challenge still present without verify targets; falling back to keyboard verify", "attempt", attempt, "wait", waitFor.String(), "controls", summarizeCandidateTargets(targets))
		} else {
			b.logger.Debug("timeout waiting for challenge to clear", "attempt", attempt, "targets", summarizeCandidateTargets(relevant))
		}
		_ = b.clickVerify(ctx, 1)
	}
}

func (b *chromedpBrowser) resolveTurnstileToken(ctx context.Context, tabs int) (string, error) {
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

func (b *chromedpBrowser) clickVerify(ctx context.Context, tabs int) error {
	b.debugChallengeState(ctx, "before-click-verify")

	if err := b.runWebDriverKeySequence(ctx, tabs); err != nil {
		b.logger.Debug("cloudflare verify key sequence failed", "err", err)
	}

	cleared, err := b.waitChallengeClear(ctx, 1500*time.Millisecond)
	if err == nil && cleared {
		return nil
	}

	fallbackClicked, err := b.clickTabbableChallengeTarget(ctx, tabs)
	if err != nil {
		b.logger.Debug("webdriver tabbable target fallback failed", "err", err)
	}
	if fallbackClicked {
		cleared, err = b.waitChallengeClear(ctx, 2*time.Second)
		if err == nil && cleared {
			return nil
		}
	}

	buttonClicked, err := b.clickVerifyButtons(ctx)
	if err != nil {
		b.logger.Debug("cloudflare verify button click failed", "err", err)
	}
	if buttonClicked {
		cleared, err = b.waitChallengeClear(ctx, 2*time.Second)
		if err == nil && cleared {
			return nil
		}
	}

	iframeClicked, err := b.clickChallengeIframes(ctx)
	if err != nil {
		b.logger.Debug("cloudflare challenge iframe click failed", "err", err)
	}
	if iframeClicked {
		cleared, err = b.waitChallengeClear(ctx, 3*time.Second)
		if err == nil && cleared {
			return nil
		}
	}

	b.debugChallengeState(ctx, "after-click-verify")
	return sleepContext(ctx, 2*time.Second)
}

func (b *chromedpBrowser) focusHelperButton(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.Evaluate(`(() => {
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
	})()`, nil))
}

func (b *chromedpBrowser) runWebDriverKeySequence(ctx context.Context, tabs int) error {
	if err := b.prepareWebDriverInput(ctx); err != nil {
		return err
	}
	if err := sleepContext(ctx, 5*time.Second); err != nil {
		return err
	}
	for i := 0; i < max(tabs, 1); i++ {
		if err := b.dispatchKeyStroke(ctx, kb.Tab); err != nil {
			return err
		}
		if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	if err := sleepContext(ctx, time.Second); err != nil {
		return err
	}
	return b.dispatchKeyStroke(ctx, " ")
}

func (b *chromedpBrowser) prepareWebDriverInput(ctx context.Context) error {
	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(inner context.Context) error { return page.BringToFront().Do(inner) }),
		chromedp.ActionFunc(func(inner context.Context) error { return emulation.SetFocusEmulationEnabled(true).Do(inner) }),
		chromedp.Evaluate(`(() => {
			try { window.focus(); } catch (_) {}
			try {
				if (document.body && typeof document.body.focus === 'function') {
					document.body.focus();
				}
			} catch (_) {}
			return document.hasFocus ? document.hasFocus() : true;
		})()`, nil),
	); err != nil {
		return err
	}

	return b.clickAt(ctx, point{X: 12, Y: 12})
}

func (b *chromedpBrowser) dispatchKeyStroke(ctx context.Context, keys string) error {
	for _, r := range keys {
		for _, event := range kb.Encode(r) {
			if err := event.Do(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *chromedpBrowser) clickTabbableChallengeTarget(ctx context.Context, tabs int) (bool, error) {
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

	switch {
	case target.Kind == "iframe":
		for _, candidate := range clickPointsForTarget(target) {
			if err := b.clickAt(ctx, candidate); err == nil {
				return true, nil
			}
		}
	case target.Visible && target.Width > 0 && target.Height > 0:
		for _, candidate := range clickPointsForTarget(target) {
			if err := b.clickAt(ctx, candidate); err == nil {
				return true, nil
			}
		}
	}

	if err := b.dispatchKeyStroke(ctx, " "); err != nil {
		return false, err
	}
	return true, nil
}

func (b *chromedpBrowser) focusTarget(ctx context.Context, target clickTarget) error {
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

	return chromedp.Run(ctx, chromedp.Evaluate(script, nil))
}

func (b *chromedpBrowser) clickVerifyButtons(ctx context.Context) (bool, error) {
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

func (b *chromedpBrowser) clickChallengeIframes(ctx context.Context) (bool, error) {
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

func (b *chromedpBrowser) clickAt(ctx context.Context, p point) error {
	return chromedp.Run(
		ctx,
		chromedp.MouseEvent(input.MouseMoved, p.X, p.Y),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.MouseClickXY(p.X, p.Y),
	)
}

func (b *chromedpBrowser) waitChallengeClear(ctx context.Context, d time.Duration) (bool, error) {
	if err := sleepContext(ctx, d); err != nil {
		return false, err
	}
	found, err := b.challengePresent(ctx)
	if err != nil {
		return false, err
	}
	return !found, nil
}

func (b *chromedpBrowser) waitChallengeGone(ctx context.Context, d time.Duration) (bool, error) {
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

func (b *chromedpBrowser) clickTargets(ctx context.Context) ([]clickTarget, error) {
	var targets []clickTarget
	const script = `(() => {
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
	})()`

	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &targets)); err != nil {
		return nil, err
	}
	return targets, nil
}

func (b *chromedpBrowser) debugChallengeState(ctx context.Context, stage string) {
	var activeElement string
	var hasFocus bool
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const el = document.activeElement;
		if (!el) return '';
		const bits = [el.tagName || '', el.id ? ('#' + el.id) : '', el.getAttribute ? (el.getAttribute('name') || '') : ''];
		return bits.filter(Boolean).join(' ');
	})()`, &activeElement))
	_ = chromedp.Run(ctx, chromedp.Evaluate(`document.hasFocus ? document.hasFocus() : true`, &hasFocus))

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

func (b *chromedpBrowser) challengePresent(ctx context.Context) (bool, error) {
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

func (b *chromedpBrowser) pageTitle(ctx context.Context) (string, error) {
	var title string
	err := chromedp.Run(ctx, chromedp.Title(&title))
	return title, err
}

func (b *chromedpBrowser) currentURL(ctx context.Context) (string, error) {
	var current string
	err := chromedp.Run(ctx, chromedp.Evaluate(`window.location.href`, &current))
	return current, err
}

func (b *chromedpBrowser) pageHTML(ctx context.Context) (string, error) {
	var htmlDoc string
	err := chromedp.Run(ctx, chromedp.OuterHTML("html", &htmlDoc, chromedp.ByQuery))
	return htmlDoc, err
}

func (b *chromedpBrowser) selectorExists(ctx context.Context, selector string) (bool, error) {
	var exists bool
	js := fmt.Sprintf(`document.querySelector(%q) !== null`, selector)
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &exists))
	return exists, err
}

func (b *chromedpBrowser) readInputValue(ctx context.Context, selector string) (string, error) {
	var value string
	js := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		return el ? (el.value || '') : '';
	})()`, selector)
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &value))
	return value, err
}

func (b *chromedpBrowser) currentCookies(ctx context.Context, currentURL string) ([]Cookie, error) {
	var entries []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(inner context.Context) error {
		var innerErr error
		entries, innerErr = network.GetCookies().WithURLs([]string{currentURL}).Do(inner)
		return innerErr
	}))
	if err != nil {
		return nil, err
	}

	result := make([]Cookie, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		result = append(result, Cookie{
			Name:     entry.Name,
			Value:    entry.Value,
			Domain:   entry.Domain,
			Path:     entry.Path,
			HTTPOnly: entry.HTTPOnly,
			Secure:   entry.Secure,
			SameSite: string(entry.SameSite),
		})
	}
	return result, nil
}

func buildPostFormHTML(targetURL, postData string) string {
	queryString := strings.TrimPrefix(postData, "?")
	pairs, _ := url.ParseQuery(queryString)

	var builder strings.Builder
	builder.WriteString(`<!DOCTYPE html><html><body><form id="hackForm" action="`)
	builder.WriteString(html.EscapeString(targetURL))
	builder.WriteString(`" method="POST">`)
	for name, values := range pairs {
		if name == "submit" {
			continue
		}
		for _, value := range values {
			builder.WriteString(`<input type="text" name="`)
			builder.WriteString(html.EscapeString(url.QueryEscape(name)))
			builder.WriteString(`" value="`)
			builder.WriteString(html.EscapeString(url.QueryEscape(value)))
			builder.WriteString(`"><br>`)
		}
	}
	builder.WriteString(`</form><script>document.getElementById("hackForm").submit();</script></body></html>`)
	return builder.String()
}

func appendWithEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, item)
	}
	if !replaced {
		result = append(result, prefix+value)
	}
	return result
}

func normalizeBlockedPattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return pattern
	}
	if strings.Contains(pattern, "://") {
		return pattern
	}
	if strings.HasPrefix(pattern, "*") {
		return "*://*:*/*" + strings.TrimPrefix(pattern, "*")
	}
	return "*://*:*/*" + pattern
}

func isVerifyButtonTarget(target clickTarget) bool {
	if !target.Visible {
		return false
	}

	haystack := strings.ToLower(strings.Join([]string{
		target.Text,
		target.AriaLabel,
		target.Title,
		target.Name,
		target.ID,
	}, " "))

	if strings.Contains(haystack, "verify you are human") {
		return true
	}
	return strings.Contains(haystack, "verify") && strings.Contains(haystack, "human")
}

func isChallengeIframeTarget(target clickTarget) bool {
	if target.Kind != "iframe" || !target.Visible {
		return false
	}

	haystack := strings.ToLower(strings.Join([]string{
		target.Src,
		target.Title,
		target.Name,
		target.ID,
		target.AriaLabel,
	}, " "))

	for _, needle := range []string{"cloudflare", "challenge", "turnstile", "cf-chl", "captcha", "widget"} {
		if strings.Contains(haystack, needle) {
			return true
		}
	}

	return target.Width >= 240 && target.Width <= 420 && target.Height >= 40 && target.Height <= 120
}

func relevantChallengeTargets(targets []clickTarget) []clickTarget {
	relevant := make([]clickTarget, 0, len(targets))
	for _, target := range targets {
		if isVerifyButtonTarget(target) || isChallengeIframeTarget(target) {
			relevant = append(relevant, target)
		}
	}
	return relevant
}

func fallbackChallengeTargets(targets []clickTarget) []clickTarget {
	candidates := make([]clickTarget, 0, len(targets))
	for _, target := range targets {
		if !target.Visible || target.ID == "__flaresolverr-focus" {
			continue
		}
		switch target.Kind {
		case "iframe":
			if target.Width >= 20 && target.Width <= 900 && target.Height >= 20 && target.Height <= 400 {
				candidates = append(candidates, target)
			}
		case "button", "input", "role_button":
			if target.Width >= 40 && target.Width <= 500 && target.Height >= 20 && target.Height <= 180 {
				candidates = append(candidates, target)
			}
		}
	}
	return candidates
}

func tabbableTargets(targets []clickTarget) []clickTarget {
	candidates := make([]clickTarget, 0, len(targets))
	for _, target := range targets {
		if !target.Visible || target.Disabled || target.ID == "__flaresolverr-focus" {
			continue
		}
		if target.Width <= 0 || target.Height <= 0 {
			continue
		}
		if target.TabIndex >= 0 {
			candidates = append(candidates, target)
			continue
		}
		switch target.Tag {
		case "iframe", "button", "input", "textarea", "select":
			candidates = append(candidates, target)
		case "a":
			if target.Src == "" {
				candidates = append(candidates, target)
			}
		}
		if target.Role == "button" {
			candidates = append(candidates, target)
		}
	}
	return dedupeTargets(candidates)
}

func dedupeTargets(targets []clickTarget) []clickTarget {
	if len(targets) < 2 {
		return targets
	}
	seen := make(map[string]struct{}, len(targets))
	unique := make([]clickTarget, 0, len(targets))
	for _, target := range targets {
		key := fmt.Sprintf("%s|%s|%s|%s|%.1f|%.1f|%.1f|%.1f", target.Kind, target.Tag, target.ID, target.Name, target.Left, target.Top, target.Width, target.Height)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, target)
	}
	return unique
}

func summarizeCandidateTargets(targets []clickTarget) []string {
	if len(targets) == 0 {
		return nil
	}

	limit := min(len(targets), 6)
	summary := make([]string, 0, limit)
	for _, target := range targets[:limit] {
		summary = append(summary, summarizeClickTarget(target))
	}
	return summary
}

func challengeAutoWaitDuration(relevant []clickTarget) time.Duration {
	if len(relevant) == 0 {
		return 1500 * time.Millisecond
	}
	return time.Second
}

func clickPointsForTarget(target clickTarget) []point {
	left := target.Left
	top := target.Top
	width := target.Width
	height := target.Height
	center := point{X: left + width/2, Y: top + height/2}

	if target.Kind != "iframe" {
		return []point{
			center,
			{X: center.X + min(width*0.05, 8), Y: center.Y + min(height*0.05, 6)},
		}
	}

	leftBias := left + min(max(width*0.2, 18), 42)
	verticalCenter := top + height/2
	return []point{
		{X: leftBias, Y: verticalCenter},
		center,
		{X: left + min(max(width*0.33, 28), max(width-10, 10)), Y: top + min(max(height*0.5, 18), max(height-6, 6))},
	}
}

func summarizeClickTarget(target clickTarget) string {
	return fmt.Sprintf("%s/%s text=%q role=%q title=%q src=%q tabIndex=%d rect=(%.1f,%.1f %.1fx%.1f)", target.Kind, target.Tag, target.Text, target.Role, target.Title, target.Src, target.TabIndex, target.Left, target.Top, target.Width, target.Height)
}

func chromeArgValue(raw, name string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}

	flagPrefix := "--" + strings.TrimSpace(name)
	for i := 0; i < len(fields); i++ {
		field := strings.TrimSpace(fields[i])
		if field == "" {
			continue
		}
		if value, ok := strings.CutPrefix(field, flagPrefix+"="); ok {
			return strings.TrimSpace(value)
		}
		if field == flagPrefix && i+1 < len(fields) {
			return strings.TrimSpace(fields[i+1])
		}
	}
	return ""
}

func parseChromeArgs(raw string) []chromedp.ExecAllocatorOption {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	fields := strings.Fields(raw)
	opts := make([]chromedp.ExecAllocatorOption, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if !strings.HasPrefix(field, "--") {
			continue
		}
		field = strings.TrimPrefix(field, "--")
		if name, value, ok := strings.Cut(field, "="); ok {
			opts = append(opts, chromedp.Flag(name, value))
			continue
		}
		opts = append(opts, chromedp.Flag(field, true))
	}
	return opts
}

func firstCookiePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "/"
	}
	return path
}

func scrubUserAgent(value string) string {
	return strings.TrimSpace(headlessUserAgentRE.ReplaceAllString(value, ""))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func encodeBase64(payload []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	if len(payload) == 0 {
		return ""
	}
	out := make([]byte, 0, ((len(payload)+2)/3)*4)
	for i := 0; i < len(payload); i += 3 {
		var b0 = payload[i]
		var b1 byte
		var b2 byte
		remaining := len(payload) - i
		if remaining > 1 {
			b1 = payload[i+1]
		}
		if remaining > 2 {
			b2 = payload[i+2]
		}

		out = append(out, table[b0>>2])
		out = append(out, table[((b0&0x03)<<4)|(b1>>4)])
		if remaining > 1 {
			out = append(out, table[((b1&0x0f)<<2)|(b2>>6)])
		} else {
			out = append(out, '=')
		}
		if remaining > 2 {
			out = append(out, table[b2&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
