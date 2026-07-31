package webdriverbackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
type clickTarget = browserpkg.ClickTarget
type point = browserpkg.Point
type documentResponse = browserpkg.DocumentResponse

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
var normalizeResponseHeaders = browserpkg.NormalizeResponseHeaders
var urlsEquivalent = browserpkg.URLsEquivalent
var createTransientDir = browserpkg.CreateTransientDir
var firstNonEmpty = browserpkg.FirstNonEmpty

var proxyExtensionCacheMu sync.Mutex

const webdriverTabKey = "\uE004"

type webDriverBrowser struct {
	cfg    Config
	logger Logger
	proxy  *Proxy

	sess              *w3c.Session
	effectiveHeadless bool

	driver            *w3c.DriverProcess
	patchedDriverDir  string
	patchedDriverPath string
	driverRuntimeDir  string
	cachedUserAgent   string

	xvfbProc        *browserpkg.XvfbProcess
	previousDisplay string
	userDataDir     string
	keepUserDataDir bool
	proxyExtDir     string
	keepProxyExtDir bool
	perfLogPath     string

	mu sync.Mutex
}

func NewWebDriver(ctx context.Context, cfg Config, proxy *Proxy) (Client, error) {
	b := &webDriverBrowser{
		cfg:             cfg,
		logger:          cfg.Logger,
		proxy:           proxy,
		previousDisplay: os.Getenv("DISPLAY"),
		sess: &w3c.Session{
			HTTP:      &http.Client{Timeout: 30 * time.Second},
			ErrPrefix: "webdriver",
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
	if err := b.startDriver(ctx); err != nil {
		_ = b.Close()
		return nil, err
	}
	if err := b.createSession(ctx); err != nil {
		_ = b.Close()
		return nil, err
	}
	if err := b.installStealth(ctx); err != nil {
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

	// Bounded: this runs under b.mu, and destroyAll closes sessions serially, so
	// a wedged chromedriver would otherwise stall shutdown for the full 30s
	// httpClient timeout per session.
	deleteCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	b.sess.Delete(deleteCtx)
	cancel()
	b.driver.Stop()
	if b.patchedDriverDir != "" {
		_ = os.RemoveAll(b.patchedDriverDir)
		b.patchedDriverDir = ""
	}
	if b.driverRuntimeDir != "" {
		_ = os.RemoveAll(b.driverRuntimeDir)
		b.driverRuntimeDir = ""
	}
	if b.proxyExtDir != "" {
		if !b.keepProxyExtDir {
			_ = os.RemoveAll(b.proxyExtDir)
		}
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

func (b *webDriverBrowser) startDriver(ctx context.Context) error {
	port, err := w3c.FreeLocalPort()
	if err != nil {
		return fmt.Errorf("reserve chromedriver port: %w", err)
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
		Name: "chromedriver",
		Path: b.patchedDriverPath,
		Args: []string{
			fmt.Sprintf("--port=%d", port),
			"--allowed-origins=*",
		},
		Env: env,
	}
	if b.cfg.DebugLogging {
		if err := b.prepareDriverRuntimeDir(); err != nil {
			return err
		}
		// chromedriver writes the log itself, so no output redirection.
		spec.LogPath = filepath.Join(b.driverRuntimeDir, "chromedriver.log")
		spec.Args = append(spec.Args, "--verbose", "--log-path="+spec.LogPath)
	}

	b.sess.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	driver, err := w3c.StartDriver(ctx, spec, b.sess)
	if err != nil {
		return err
	}
	b.driver = driver
	return nil
}

func (b *webDriverBrowser) prepareDriverRuntimeDir() error {
	if b.driverRuntimeDir != "" {
		return nil
	}
	dir, err := createTransientDir("flaresolverr-go-webdriver-*")
	if err != nil {
		return fmt.Errorf("create chromedriver runtime dir: %w", err)
	}
	b.driverRuntimeDir = dir
	return nil
}

func (b *webDriverBrowser) createSession(ctx context.Context) error {
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
				"goog:loggingPrefs": map[string]any{
					"performance": "ALL",
				},
				"goog:chromeOptions": chromeOptions,
			},
		},
	}

	raw, topSessionID, err := b.sess.Do(context.Background(), http.MethodPost, "/session", payload)
	if err != nil {
		if tail := b.driver.LogTail(); tail != "" {
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

	b.sess.ID = sessionID
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
		// Cloudflare renders the Turnstile widget inside a *closed* shadow
		// root, which querySelectorAll and el.shadowRoot cannot see. Stealth
		// Chromium builds expose it as el.fakeShadowRoot behind this flag;
		// stock Chrome ignores the unknown feature name.
		"--enable-blink-features=FakeShadowRoot",
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

func (b *webDriverBrowser) installStealth(ctx context.Context) error {
	// Stealth patches applied on every new document to reduce automation
	// fingerprint signals that CF Managed Challenge uses for botting decisions.
	// Covers: navigator.webdriver, real PluginArray/MimeTypeArray mimic,
	// Canvas/WebGL/Audio fingerprint noise, window.chrome, permissions quirk,
	// Notification stub, and matching sec-ch-ua userAgentData.
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

		safeDefine(navigator, 'maxTouchPoints', () => 0);
		safeDefine(navigator, 'hardwareConcurrency', () => 8);
		safeDefine(navigator, 'deviceMemory', () => 8);
		if (navigator.connection) {
			safeDefine(navigator.connection, 'rtt', () => 100);
			safeDefine(navigator.connection, 'downlink', () => 10);
			safeDefine(navigator.connection, 'effectiveType', () => '4g');
		}
		safeDefine(navigator, 'languages', () => navigator.languages && navigator.languages.length ? navigator.languages : ['en-US', 'en']);

		// PluginArray / MimeTypeArray — mimic real Chrome PDF plugins so that
		// CF fingerprinting sees proper Plugin objects (not a number array).
		try {
			const makeMime = (type, suffixes, description) => {
				const mime = Object.create(MimeType.prototype);
				Object.defineProperty(mime, 'type', { get: () => type });
				Object.defineProperty(mime, 'suffixes', { get: () => suffixes });
				Object.defineProperty(mime, 'description', { get: () => description });
				return mime;
			};
			const makePlugin = (name, filename, description, mimes) => {
				const plugin = Object.create(Plugin.prototype);
				Object.defineProperty(plugin, 'name', { get: () => name });
				Object.defineProperty(plugin, 'filename', { get: () => filename });
				Object.defineProperty(plugin, 'description', { get: () => description });
				Object.defineProperty(plugin, 'length', { get: () => mimes.length });
				for (let i = 0; i < mimes.length; i++) {
					Object.defineProperty(plugin, i, { get: () => mimes[i] });
					Object.defineProperty(plugin, mimes[i].type, { get: () => mimes[i] });
					Object.defineProperty(mimes[i], 'enabledPlugin', { get: () => plugin });
				}
				return plugin;
			};
			const pdfMime = makeMime('application/pdf', 'pdf', 'Portable Document Format');
			const pluginPdfMime = makeMime('application/x-google-chrome-pdf', 'pdf', 'Portable Document Format');
			const pdfViewer = makePlugin('PDF Viewer', 'internal-pdf-viewer', 'Portable Document Format', [pdfMime, pluginPdfMime]);
			const chromePdf = makePlugin('Chrome PDF Viewer', 'internal-pdf-viewer', 'Portable Document Format', [pdfMime, pluginPdfMime]);
			const chromiumPdf = makePlugin('Chromium PDF Viewer', 'internal-pdf-viewer', 'Portable Document Format', [pdfMime, pluginPdfMime]);
			const microsoftEdgePdf = makePlugin('Microsoft Edge PDF Viewer', 'internal-pdf-viewer', 'Portable Document Format', [pdfMime, pluginPdfMime]);
			const webkitPdf = makePlugin('WebKit built-in PDF', 'internal-pdf-viewer', 'Portable Document Format', [pdfMime, pluginPdfMime]);
			const plugins = [pdfViewer, chromePdf, chromiumPdf, microsoftEdgePdf, webkitPdf];
			const pluginArray = Object.create(PluginArray.prototype);
			Object.defineProperty(pluginArray, 'length', { get: () => plugins.length });
			for (let i = 0; i < plugins.length; i++) {
				Object.defineProperty(pluginArray, i, { get: () => plugins[i] });
				Object.defineProperty(pluginArray, plugins[i].name, { get: () => plugins[i] });
			}
			pluginArray.item = (i) => plugins[i] || null;
			pluginArray.namedItem = (name) => plugins.find(p => p.name === name) || null;
			pluginArray.refresh = () => {};
			safeDefine(navigator, 'plugins', () => pluginArray);

			const mimes = [pdfMime, pluginPdfMime];
			const mimeArray = Object.create(MimeTypeArray.prototype);
			Object.defineProperty(mimeArray, 'length', { get: () => mimes.length });
			for (let i = 0; i < mimes.length; i++) {
				Object.defineProperty(mimeArray, i, { get: () => mimes[i] });
				Object.defineProperty(mimeArray, mimes[i].type, { get: () => mimes[i] });
			}
			mimeArray.item = (i) => mimes[i] || null;
			mimeArray.namedItem = (name) => mimes.find(m => m.type === name) || null;
			safeDefine(navigator, 'mimeTypes', () => mimeArray);
		} catch (_) {}

		// userAgentData — return a plausible sec-ch-ua profile (Chrome)
		try {
			const brands = [
				{ brand: 'Not)A;Brand', version: '99' },
				{ brand: 'Google Chrome', version: '147' },
				{ brand: 'Chromium', version: '147' },
			];
			const uaData = {
				brands: brands,
				mobile: false,
				platform: 'Linux',
				getHighEntropyValues: (hints) => Promise.resolve({
					brands: brands,
					mobile: false,
					platform: 'Linux',
					platformVersion: '6.17.0',
					architecture: 'x86',
					bitness: '64',
					model: '',
					uaFullVersion: '147.0.7727.101',
					fullVersionList: brands,
					wow64: false,
				}),
				toJSON: () => ({ brands: brands, mobile: false, platform: 'Linux' }),
			};
			safeDefine(navigator, 'userAgentData', () => uaData);
		} catch (_) {}

		// Canvas fingerprint: inject tiny, deterministic-per-document noise into
		// getImageData so CF can't get a stable canvas hash from our browser.
		try {
			const noisify = (canvas, context) => {
				if (!context || typeof context.getImageData !== 'function') return;
				const orig = context.getImageData;
				const seed = (Math.random() * 1e6) | 0;
				context.getImageData = function(x, y, w, h) {
					const img = orig.apply(this, arguments);
					const data = img.data;
					for (let i = 0; i < data.length; i += 4) {
						const v = (seed + i) & 0x7;
						data[i] = data[i] ^ (v & 1);
						data[i+1] = data[i+1] ^ ((v >> 1) & 1);
						data[i+2] = data[i+2] ^ ((v >> 2) & 1);
					}
					return img;
				};
			};
			const origGetContext = HTMLCanvasElement.prototype.getContext;
			HTMLCanvasElement.prototype.getContext = function(type) {
				const ctx = origGetContext.apply(this, arguments);
				if (type === '2d') noisify(this, ctx);
				return ctx;
			};
			const origToDataURL = HTMLCanvasElement.prototype.toDataURL;
			HTMLCanvasElement.prototype.toDataURL = function() {
				try {
					const ctx = this.getContext('2d');
					if (ctx) noisify(this, ctx);
				} catch (_) {}
				return origToDataURL.apply(this, arguments);
			};
		} catch (_) {}

		// WebGL: override renderer/vendor unmasked values to look like a real
		// Intel UHD Graphics rig instead of headless SwiftShader/LLVM.
		try {
			const patchGL = (proto) => {
				if (!proto || !proto.getParameter) return;
				const orig = proto.getParameter;
				proto.getParameter = function(parameter) {
					// UNMASKED_VENDOR_WEBGL = 0x9245
					if (parameter === 0x9245) return 'Intel Inc.';
					// UNMASKED_RENDERER_WEBGL = 0x9246
					if (parameter === 0x9246) return 'Intel(R) UHD Graphics 630';
					return orig.apply(this, arguments);
				};
			};
			if (typeof WebGLRenderingContext !== 'undefined') patchGL(WebGLRenderingContext.prototype);
			if (typeof WebGL2RenderingContext !== 'undefined') patchGL(WebGL2RenderingContext.prototype);
		} catch (_) {}

		// AudioContext fingerprint noise
		try {
			if (typeof AnalyserNode !== 'undefined') {
				const orig = AnalyserNode.prototype.getFloatFrequencyData;
				AnalyserNode.prototype.getFloatFrequencyData = function(array) {
					orig.apply(this, arguments);
					for (let i = 0; i < array.length; i++) {
						array[i] = array[i] + (Math.random() - 0.5) * 1e-7;
					}
				};
			}
		} catch (_) {}

		window.chrome = window.chrome || {
			app: { isInstalled: false, InstallState: { DISABLED: 'disabled', INSTALLED: 'installed', NOT_INSTALLED: 'not_installed' }, RunningState: { CANNOT_RUN: 'cannot_run', READY_TO_RUN: 'ready_to_run', RUNNING: 'running' } },
			runtime: { OnInstalledReason: {}, OnRestartRequiredReason: {}, PlatformArch: {}, PlatformNaclArch: {}, PlatformOs: {}, RequestUpdateCheckStatus: {} },
			csi: () => ({ onloadT: Date.now(), startE: Date.now(), pageT: 0, tran: 15 }),
			loadTimes: () => ({ requestTime: Date.now() / 1000, startLoadTime: Date.now() / 1000, commitLoadTime: Date.now() / 1000, finishDocumentLoadTime: Date.now() / 1000, finishLoadTime: Date.now() / 1000, firstPaintTime: Date.now() / 1000, firstPaintAfterLoadTime: 0, navigationType: 'Other', wasFetchedViaSpdy: true, wasNpnNegotiated: true, npnNegotiatedProtocol: 'h2', wasAlternateProtocolAvailable: false, connectionInfo: 'h2' }),
		};
		if (!window.Notification) {
			window.Notification = { permission: 'default' };
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
	// Only override when it actually changes the UA — the chromedp backend has
	// always had this guard and the webdriver one did not. The call is far from
	// free: with no userAgentMetadata, Chrome re-derives its high-entropy client
	// hints from the UA string alone, and Cloudflare compares those against the
	// UA. Measured on rutracker with a stealth Chromium, this no-op override
	// (same UA in, same UA out) was the entire difference between clearing the
	// managed challenge in ~7s and looping on it forever.
	if overrideUA == "" || overrideUA == currentUA {
		return nil
	}
	if _, err := b.executeCDP(context.Background(), "Emulation.setUserAgentOverride", map[string]any{
		"userAgent":      overrideUA,
		"acceptLanguage": firstNonEmpty(os.Getenv("LANG"), "en-US"),
		"platform":       runtime.GOOS,
	}); err != nil {
		// Not fatal — the browser is still usable — but per the note above this
		// is what makes managed challenges clear, so a silent failure looks like
		// "challenges suddenly stopped solving" with nothing in the log.
		b.logger.Warn("user agent override failed; managed challenges may loop until timeout", "err", err)
	}

	return nil
}

func (b *webDriverBrowser) resolve(ctx context.Context, req Request) (*ChallengeResolutionResult, string, error) {
	return browserpkg.ResolvePage(ctx, b, req)
}

// ---------- browser.Page ----------

func (b *webDriverBrowser) SetMediaBlocked(ctx context.Context, blocked bool) error {
	urls := make([]string, 0, len(blockedURLs))
	if blocked {
		for _, pattern := range blockedURLs {
			urls = append(urls, normalizeBlockedPattern(pattern))
		}
	}
	if _, err := b.executeCDP(ctx, "Network.enable", map[string]any{}); err != nil {
		return err
	}
	_, err := b.executeCDP(ctx, "Network.setBlockedURLs", map[string]any{"urls": urls})
	return err
}

func (b *webDriverBrowser) Navigate(ctx context.Context, req Request) error {
	return b.navigate(ctx, req)
}

func (b *webDriverBrowser) SetPageCookies(ctx context.Context, rawURL string, cookies []Cookie) error {
	return b.sess.SetCookies(ctx, rawURL, cookies)
}

func (b *webDriverBrowser) Title(ctx context.Context) (string, error) { return b.sess.Title(ctx) }

func (b *webDriverBrowser) SelectorExists(ctx context.Context, selector string) (bool, error) {
	return b.sess.SelectorExists(ctx, selector)
}

func (b *webDriverBrowser) HTML(ctx context.Context) (string, error) { return b.sess.HTML(ctx) }

func (b *webDriverBrowser) CurrentURL(ctx context.Context) (string, error) { return b.sess.URL(ctx) }

func (b *webDriverBrowser) PageUserAgent(ctx context.Context) (string, error) {
	return b.userAgent(ctx)
}

func (b *webDriverBrowser) PageCookies(ctx context.Context, _ string) ([]Cookie, error) {
	return b.sess.Cookies(ctx)
}

func (b *webDriverBrowser) Screenshot(ctx context.Context) (string, error) {
	return b.sess.Screenshot(ctx)
}

func (b *webDriverBrowser) ChallengePresent(ctx context.Context) (bool, error) {
	return b.challengePresent(ctx)
}

func (b *webDriverBrowser) SolveChallenge(ctx context.Context) error {
	return b.solveChallenge(ctx)
}

func (b *webDriverBrowser) DocumentReady(ctx context.Context) (bool, error) {
	return b.sess.ExecuteBool(ctx, browserpkg.DocumentReadyScript)
}

func (b *webDriverBrowser) DocumentResponse(ctx context.Context, currentURL string) (documentResponse, error) {
	return b.documentResponse(ctx, currentURL)
}

func (b *webDriverBrowser) ApplyTurnstileToken(ctx context.Context, req Request, result *ChallengeResolutionResult) error {
	if req.TabsTillVerify == nil {
		return nil
	}
	token, err := b.resolveTurnstileToken(ctx, max(*req.TabsTillVerify, 1))
	if err != nil {
		return err
	}
	result.TurnstileToken = token
	return nil
}

func (b *webDriverBrowser) PageLogger() Logger      { return b.logger }
func (b *webDriverBrowser) LogHTMLConfigured() bool { return b.cfg.LogHTML }

func (b *webDriverBrowser) navigate(ctx context.Context, req Request) error {
	targetURL := req.URL
	if strings.EqualFold(req.Method, "POST") {
		htmlDoc := buildPostFormHTML(req.URL, req.PostData)
		targetURL = "data:text/html;charset=utf-8," + url.PathEscape(htmlDoc)
	}
	_, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/url"), map[string]any{"url": targetURL})
	return err
}

func (b *webDriverBrowser) solveChallenge(ctx context.Context) error {
	b.debugChallengeState(ctx, "challenge-detected")

	// Seed some mouse activity up front. CF Managed Challenge requires
	// human-like mouse movement signals before it will auto-resolve.
	_ = b.mouseWiggle(ctx)

	// Managed Challenge Invisible: page shows "Verifying..." with no
	// interactive Turnstile/checkbox — only helper links (refresh, docs).
	// Clicking those just refreshes the page and restarts the loop. If we
	// see this pattern for N consecutive attempts, bail out fast so the
	// client can fall back rather than burn the full 90s timeout.
	const passiveBailout = 3
	attempt := 0
	passiveStreak := 0
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

		targets, _ := b.clickTargets(ctx)
		relevant := relevantChallengeTargets(targets)
		fallback := fallbackChallengeTargets(targets)
		if len(relevant) == 0 && len(fallback) == 0 {
			passiveStreak++
			b.logger.Debug("managed challenge: no interactive controls",
				"attempt", attempt, "passive_streak", passiveStreak)
			if passiveStreak >= passiveBailout {
				return fmt.Errorf("managed challenge has no interactive controls after %d attempts; browser fingerprint likely blocked", passiveStreak)
			}
			// Passive managed challenge: give CF more time with mouse
			// activity instead of clicking unrelated page links.
			_ = b.mouseWiggle(ctx)
			if err := sleepContext(ctx, 3*time.Second); err != nil {
				return err
			}
			continue
		}
		passiveStreak = 0

		b.logger.Debug("timeout waiting for challenge to clear", "attempt", attempt)
		_ = b.clickVerify(ctx, 1)
	}
}

// mouseWiggle dispatches a short sequence of mouseMoved CDP events to
// simulate human-like mouse activity. CF Managed Challenge checks for mouse
// movement before auto-resolving; a purely static browser gets stuck.
func (b *webDriverBrowser) mouseWiggle(ctx context.Context) error {
	pts := []struct{ x, y float64 }{
		{120, 180}, {260, 240}, {400, 300}, {540, 340},
		{620, 280}, {480, 220}, {340, 260}, {200, 320},
	}
	for _, p := range pts {
		if _, err := b.executeCDP(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type":   "mouseMoved",
			"x":      p.x,
			"y":      p.y,
			"button": "none",
		}); err != nil {
			return err
		}
		if err := sleepContext(ctx, 40*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (b *webDriverBrowser) resolveTurnstileToken(ctx context.Context, tabs int) (string, error) {
	for _, selector := range turnstileSelectors {
		exists, err := b.sess.SelectorExists(ctx, selector)
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

	if _, err := b.clickVerifyHumanButton(ctx); err != nil {
		b.logger.Debug("cloudflare verify human button click failed", "err", err)
	}

	if _, err := b.clickTabbableChallengeTarget(ctx, tabs); err != nil {
		b.logger.Debug("webdriver tabbable target fallback failed", "err", err)
	}

	if _, err := b.clickVerifyButtons(ctx); err != nil {
		b.logger.Debug("cloudflare verify button click failed", "err", err)
	}

	if _, err := b.clickChallengeIframes(ctx); err != nil {
		b.logger.Debug("cloudflare challenge iframe click failed", "err", err)
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
	_, _ = b.sess.Execute(ctx, `(() => {
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
	script := browserpkg.FocusTargetScript(target)

	_, err := b.sess.Execute(ctx, script)
	return err
}

func (b *webDriverBrowser) focusHelperButton(ctx context.Context) error {
	_, err := b.sess.Execute(ctx, browserpkg.FocusHelperScript)
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

	raw, err := b.sess.Execute(ctx, script)
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
	raw, err := b.sess.Execute(ctx, browserpkg.ClickTargetsScript)
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
	activeElement, _ := b.sess.ExecuteString(ctx, browserpkg.ActiveElementScript)
	hasFocus, _ := b.sess.ExecuteBool(ctx, browserpkg.HasFocusScript)

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

func (b *webDriverBrowser) readInputValue(ctx context.Context, selector string) (string, error) {
	return b.sess.ExecuteString(ctx, fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		return el ? (el.value || '') : '';
	})()`, selector))
}

func (b *webDriverBrowser) userAgent(ctx context.Context) (string, error) {
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

func (b *webDriverBrowser) executeCDP(ctx context.Context, cmd string, params map[string]any) (json.RawMessage, error) {
	raw, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/goog/cdp/execute"), map[string]any{
		"cmd":    cmd,
		"params": params,
	})
	return raw, err
}

func (b *webDriverBrowser) performActions(ctx context.Context, actions []map[string]any) error {
	_, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/actions"), map[string]any{
		"actions": actions,
	})
	return err
}

func (b *webDriverBrowser) releaseActions(ctx context.Context) error {
	_, _, err := b.sess.Do(ctx, http.MethodDelete, b.sess.Path("/actions"), nil)
	return err
}

func (b *webDriverBrowser) switchToDefaultContent(ctx context.Context) error {
	_, _, err := b.sess.Do(ctx, http.MethodPost, b.sess.Path("/frame"), map[string]any{
		"id": nil,
	})
	return err
}

func (b *webDriverBrowser) documentResponse(ctx context.Context, currentURL string) (documentResponse, error) {
	entries, err := b.performanceLog(ctx)
	if err != nil {
		return documentResponse{}, err
	}

	type responsePayload struct {
		URL     string         `json:"url"`
		Status  int            `json:"status"`
		Headers map[string]any `json:"headers"`
	}
	type paramsEnvelope struct {
		Type     string          `json:"type"`
		Response responsePayload `json:"response"`
	}
	type messageEnvelope struct {
		Message struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		} `json:"message"`
	}
	type performanceEntry struct {
		Message string `json:"message"`
	}

	for i := len(entries) - 1; i >= 0; i-- {
		var item performanceEntry
		if err := json.Unmarshal(entries[i], &item); err != nil || strings.TrimSpace(item.Message) == "" {
			continue
		}

		var message messageEnvelope
		if err := json.Unmarshal([]byte(item.Message), &message); err != nil {
			continue
		}
		if message.Message.Method != "Network.responseReceived" {
			continue
		}

		var params paramsEnvelope
		if err := json.Unmarshal(message.Message.Params, &params); err != nil {
			continue
		}
		if !strings.EqualFold(params.Type, "Document") {
			continue
		}
		if currentURL != "" && params.Response.URL != "" && !urlsEquivalent(currentURL, params.Response.URL) {
			continue
		}

		return documentResponse{
			URL:     params.Response.URL,
			Status:  params.Response.Status,
			Headers: normalizeResponseHeaders(params.Response.Headers),
		}, nil
	}

	return documentResponse{}, nil
}

func (b *webDriverBrowser) performanceLog(ctx context.Context) ([]json.RawMessage, error) {
	paths := b.performanceLogPaths()

	var lastErr error
	for _, path := range paths {
		raw, _, err := b.sess.Do(ctx, http.MethodPost, path, map[string]any{"type": "performance"})
		if err != nil {
			lastErr = err
			continue
		}
		b.perfLogPath = path

		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		return entries, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func (b *webDriverBrowser) performanceLogPaths() []string {
	defaults := []string{
		b.sess.Path("/se/log"),
		b.sess.Path("/log"),
	}
	if strings.TrimSpace(b.perfLogPath) == "" {
		return defaults
	}

	paths := make([]string, 0, len(defaults))
	paths = append(paths, b.perfLogPath)
	for _, path := range defaults {
		if path != b.perfLogPath {
			paths = append(paths, path)
		}
	}
	return paths
}

func (b *webDriverBrowser) prepareHeadlessMode(ctx context.Context) (bool, string, error) {
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
	display, err := b.startDisplay(ctx, xvfbPath)
	if err != nil {
		return false, "", err
	}
	return false, display, nil
}

func (b *webDriverBrowser) startDisplay(ctx context.Context, xvfbPath string) (string, error) {
	proc, display, err := browserpkg.StartXvfb(ctx, xvfbPath)
	if err != nil {
		return "", err
	}
	b.xvfbProc = proc
	return display, nil
}

func (b *webDriverBrowser) stopDisplay() {
	if b.xvfbProc != nil {
		_ = b.xvfbProc.Stop()
		b.xvfbProc = nil
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

	dir, err := createTransientDir("flaresolverr-go-profile-*")
	if err != nil {
		return fmt.Errorf("create browser profile dir: %w", err)
	}
	b.userDataDir = dir
	b.keepUserDataDir = false
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

	dir, err := ensureProxyExtensionDir(parsed, b.proxy.Username, b.proxy.Password)
	if err != nil {
		return err
	}

	b.proxyExtDir = dir
	b.keepProxyExtDir = true
	return nil
}

func ensureProxyExtensionDir(parsed *url.URL, username, password string) (string, error) {
	cacheRoot, err := proxyExtensionCacheRoot()
	if err != nil {
		return "", err
	}

	key := proxyExtensionCacheKey(parsed, username, password)
	dir := filepath.Join(cacheRoot, key)
	manifestPath := filepath.Join(dir, "manifest.json")
	backgroundPath := filepath.Join(dir, "background.js")
	if fileExists(manifestPath) && fileExists(backgroundPath) {
		return dir, nil
	}

	proxyExtensionCacheMu.Lock()
	defer proxyExtensionCacheMu.Unlock()

	if fileExists(manifestPath) && fileExists(backgroundPath) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create proxy extension cache dir: %w", err)
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
);`, parsed.Scheme, parsed.Hostname(), parsed.Port(), username, password)

	if err := writeProxyExtensionFile(manifestPath, []byte(manifest)); err != nil {
		return "", err
	}
	if err := writeProxyExtensionFile(backgroundPath, []byte(background)); err != nil {
		return "", err
	}
	return dir, nil
}

func proxyExtensionCacheRoot() (string, error) {
	baseDir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(baseDir) == "" {
		baseDir = os.TempDir()
	}
	dir := filepath.Join(baseDir, "flaresolverr-go", "proxy-extension")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create proxy extension cache root: %w", err)
	}
	return dir, nil
}

func proxyExtensionCacheKey(parsed *url.URL, username, password string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(parsed.Scheme),
		strings.TrimSpace(parsed.Hostname()),
		strings.TrimSpace(parsed.Port()),
		strings.TrimSpace(username),
		password,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func writeProxyExtensionFile(path string, content []byte) error {
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, content, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
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
