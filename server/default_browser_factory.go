package flaresolverr

import (
	browserpkg "flaresolverr-go/server/browser"
	chromedpbackend "flaresolverr-go/server/browser/chromedp"
	webdriverbackend "flaresolverr-go/server/browser/webdriver"
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
		DisableMedia:        cfg.DisableMedia,
		DriverCacheDir:      cfg.DriverCacheDir,
		DriverAutoDownload:  cfg.DriverAutoDownload,
		ChromeForTestingURL: cfg.ChromeForTestingURL,
		Logger:              cfg.Logger,
	}
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
