package flaresolverr

import (
	"fmt"
	"strings"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
	chromedpbackend "github.com/trinity-aml/flaresolverr-go/server/browser/chromedp"
	geckodriverbackend "github.com/trinity-aml/flaresolverr-go/server/browser/geckodriver"
	webdriverbackend "github.com/trinity-aml/flaresolverr-go/server/browser/webdriver"
)

type defaultBrowserFactory struct{}

func (defaultBrowserFactory) New(cfg Config, proxy *Proxy) (browserClient, error) {
	cfg = cfg.withDefaults()
	browserCfg := browserpkg.Config{
		BrowserPath:         cfg.BrowserPath,
		DriverPath:          cfg.DriverPath,
		Headless:            cfg.Headless,
		StartupUserAgent:    cfg.StartupUserAgent,
		LogHTML:             cfg.LogHTML,
		DebugLogging:        cfg.DebugLogging,
		DisableMedia:        cfg.DisableMedia,
		DriverCacheDir:      cfg.DriverCacheDir,
		DriverAutoDownload:  cfg.DriverAutoDownload,
		ChromeForTestingURL: cfg.ChromeForTestingURL,
		Logger:              cfg.Logger,
	}

	backend := resolveBrowserBackend(cfg)
	cfg.Logger.Info("selected browser backend", "backend", backend)

	switch backend {
	case "geckodriver":
		return newGeckoDriverBackend(cfg, browserCfg, proxy)
	case "chromedriver":
		return newChromeDriverBackend(cfg, browserCfg, proxy)
	}
	return nil, fmt.Errorf("unknown browser backend %q", backend)
}

// resolveBrowserBackend picks a concrete backend for the current config.
// Explicit `browser_backend` wins; in auto mode we infer from the configured
// browser binary name.
func resolveBrowserBackend(cfg Config) string {
	switch canonicalBrowserBackend(cfg.BrowserBackend) {
	case "geckodriver":
		return "geckodriver"
	case "chromedriver":
		return "chromedriver"
	}
	if isFirefoxBinary(cfg.BrowserPath) {
		return "geckodriver"
	}
	if strings.TrimSpace(cfg.BrowserPath) == "" && findFirefoxBinary() != "" && findChromeBinary() == "" {
		return "geckodriver"
	}
	return "chromedriver"
}

func newGeckoDriverBackend(cfg Config, browserCfg browserpkg.Config, proxy *Proxy) (browserClient, error) {
	if strings.TrimSpace(browserCfg.BrowserPath) == "" {
		if detected := findFirefoxBinary(); detected != "" {
			browserCfg.BrowserPath = detected
			cfg.Logger.Info("detected firefox/camoufox binary", "path", browserCfg.BrowserPath)
		}
	}
	if strings.TrimSpace(browserCfg.DriverPath) == "" {
		path, err := resolveGeckoDriverPath(cfg)
		if err != nil {
			cfg.Logger.Warn("geckodriver auto-download failed", "err", err)
		}
		if strings.TrimSpace(path) != "" {
			browserCfg.DriverPath = path
			cfg.Logger.Info("using geckodriver", "path", browserCfg.DriverPath)
		}
	}
	if strings.TrimSpace(browserCfg.DriverPath) == "" {
		return nil, fmt.Errorf("geckodriver not found; set driver_path or enable driver_auto_download")
	}
	if strings.TrimSpace(browserCfg.BrowserPath) == "" {
		return nil, fmt.Errorf("firefox/camoufox binary not found; set browser_path or install camoufox")
	}
	return geckodriverbackend.NewGeckoDriver(browserCfg, proxy)
}

func newChromeDriverBackend(cfg Config, browserCfg browserpkg.Config, proxy *Proxy) (browserClient, error) {
	driverPath, err := resolveChromeDriverPath(cfg)
	if err != nil {
		cfg.Logger.Warn("webdriver backend unavailable; falling back to chromedp backend", "err", err)
		return chromedpbackend.NewChromedp(browserCfg, proxy)
	}
	if driverPath != "" {
		browserCfg.DriverPath = driverPath
		cfg.Logger.Info("using webdriver backend with patched chromedriver", "driver_path", browserCfg.DriverPath)
		return webdriverbackend.NewWebDriver(browserCfg, proxy)
	}
	cfg.Logger.Warn("chromedriver not found; falling back to chromedp backend")
	return chromedpbackend.NewChromedp(browserCfg, proxy)
}
