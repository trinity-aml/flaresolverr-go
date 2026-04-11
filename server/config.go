package flaresolverr

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"flaresolverr-go/internal/buildinfo"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Host                string
	Port                int
	BrowserPath         string
	DriverPath          string
	DriverCacheDir      string
	DriverAutoDownload  bool
	ChromeForTestingURL string
	Headless            bool
	StartupUserAgent    string
	LogHTML             bool
	DisableMedia        bool
	PrometheusEnabled   bool
	PrometheusPort      int
	DefaultProxy        *Proxy
	Version             string
	Logger              *slog.Logger
}

type configFile struct {
	Host                string           `yaml:"host"`
	Port                int              `yaml:"port"`
	BrowserPath         string           `yaml:"browser_path"`
	DriverPath          string           `yaml:"driver_path"`
	DriverCacheDir      string           `yaml:"driver_cache_dir"`
	DriverAutoDownload  *bool            `yaml:"driver_auto_download"`
	ChromeForTestingURL string           `yaml:"chrome_for_testing_url"`
	Headless            *bool            `yaml:"headless"`
	StartupUserAgent    string           `yaml:"startup_user_agent"`
	LogLevel            string           `yaml:"log_level"`
	LogHTML             *bool            `yaml:"log_html"`
	DisableMedia        *bool            `yaml:"disable_media"`
	PrometheusEnabled   *bool            `yaml:"prometheus_enabled"`
	PrometheusPort      int              `yaml:"prometheus_port"`
	DefaultProxy        *configFileProxy `yaml:"proxy"`
}

type configFileProxy struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

func DefaultConfig() Config {
	cfg, _ := LoadConfig()
	return cfg
}

func LoadConfig() (Config, []string) {
	return loadConfig(defaultInitConfigPaths())
}

func loadConfig(searchPaths []string) (Config, []string) {
	cfg := Config{
		Host:                "0.0.0.0",
		Port:                8191,
		BrowserPath:         findChromeBinary(),
		DriverAutoDownload:  true,
		ChromeForTestingURL: defaultChromeForTestingBaseURL,
		Headless:            true,
		PrometheusPort:      8192,
		Version:             firstNonEmpty(buildinfo.Version, "dev"),
	}

	logLevel := "info"
	var warnings []string

	if fileCfg, path, err := readConfigFile(searchPaths); err != nil {
		warnings = append(warnings, "init.yaml ignored: "+err.Error())
	} else if fileCfg != nil {
		applyConfigFile(&cfg, *fileCfg)
		if strings.TrimSpace(fileCfg.LogLevel) != "" {
			logLevel = fileCfg.LogLevel
		}
		_ = path
	}

	applyEnvConfig(&cfg, &logLevel)

	level := parseLogLevel(logLevel)
	cfg.Logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	return cfg, warnings
}

func (c Config) withDefaults() Config {
	base := DefaultConfig()
	if c.Host == "" {
		c.Host = base.Host
	}
	if c.Port == 0 {
		c.Port = base.Port
	}
	if c.BrowserPath == "" {
		c.BrowserPath = base.BrowserPath
	}
	if c.DriverPath == "" {
		c.DriverPath = base.DriverPath
	}
	if c.DriverCacheDir == "" {
		c.DriverCacheDir = base.DriverCacheDir
	}
	if c.ChromeForTestingURL == "" {
		c.ChromeForTestingURL = base.ChromeForTestingURL
	}
	if c.PrometheusPort == 0 {
		c.PrometheusPort = base.PrometheusPort
	}
	if c.Version == "" {
		c.Version = base.Version
	}
	if c.Logger == nil {
		c.Logger = base.Logger
	}
	if c.DefaultProxy == nil {
		c.DefaultProxy = base.DefaultProxy
	}
	return c
}

func defaultInitConfigPaths() []string {
	paths := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)

	if cwd, err := os.Getwd(); err == nil && strings.TrimSpace(cwd) != "" {
		path := filepath.Join(cwd, "init.yaml")
		paths = appendIfMissingPath(paths, seen, path)
	}

	if executable, err := os.Executable(); err == nil && strings.TrimSpace(executable) != "" {
		path := filepath.Join(filepath.Dir(executable), "init.yaml")
		paths = appendIfMissingPath(paths, seen, path)
	}

	return paths
}

func appendIfMissingPath(paths []string, seen map[string]struct{}, path string) []string {
	clean := filepath.Clean(path)
	if _, ok := seen[clean]; ok {
		return paths
	}
	seen[clean] = struct{}{}
	return append(paths, clean)
}

func readConfigFile(paths []string) (*configFile, string, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, path, err
		}

		var cfg configFile
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, path, err
		}
		return &cfg, path, nil
	}
	return nil, "", nil
}

func applyConfigFile(cfg *Config, fileCfg configFile) {
	if strings.TrimSpace(fileCfg.Host) != "" {
		cfg.Host = fileCfg.Host
	}
	if fileCfg.Port > 0 {
		cfg.Port = fileCfg.Port
	}
	if strings.TrimSpace(fileCfg.BrowserPath) != "" {
		cfg.BrowserPath = fileCfg.BrowserPath
	}
	if strings.TrimSpace(fileCfg.DriverPath) != "" {
		cfg.DriverPath = fileCfg.DriverPath
	}
	if strings.TrimSpace(fileCfg.DriverCacheDir) != "" {
		cfg.DriverCacheDir = fileCfg.DriverCacheDir
	}
	if fileCfg.DriverAutoDownload != nil {
		cfg.DriverAutoDownload = *fileCfg.DriverAutoDownload
	}
	if strings.TrimSpace(fileCfg.ChromeForTestingURL) != "" {
		cfg.ChromeForTestingURL = fileCfg.ChromeForTestingURL
	}
	if fileCfg.Headless != nil {
		cfg.Headless = *fileCfg.Headless
	}
	if strings.TrimSpace(fileCfg.StartupUserAgent) != "" {
		cfg.StartupUserAgent = fileCfg.StartupUserAgent
	}
	if fileCfg.LogHTML != nil {
		cfg.LogHTML = *fileCfg.LogHTML
	}
	if fileCfg.DisableMedia != nil {
		cfg.DisableMedia = *fileCfg.DisableMedia
	}
	if fileCfg.PrometheusEnabled != nil {
		cfg.PrometheusEnabled = *fileCfg.PrometheusEnabled
	}
	if fileCfg.PrometheusPort > 0 {
		cfg.PrometheusPort = fileCfg.PrometheusPort
	}
	if fileCfg.DefaultProxy != nil && strings.TrimSpace(fileCfg.DefaultProxy.URL) != "" {
		cfg.DefaultProxy = &Proxy{
			URL:      fileCfg.DefaultProxy.URL,
			Username: fileCfg.DefaultProxy.Username,
			Password: fileCfg.DefaultProxy.Password,
		}
	}
}

func applyEnvConfig(cfg *Config, logLevel *string) {
	cfg.Host = getenv("HOST", cfg.Host)
	cfg.Port = getenvInt("PORT", cfg.Port)
	cfg.BrowserPath = firstNonEmpty(os.Getenv("BROWSER_PATH"), cfg.BrowserPath)
	cfg.DriverPath = firstNonEmpty(os.Getenv("DRIVER_PATH"), cfg.DriverPath)
	cfg.DriverCacheDir = firstNonEmpty(os.Getenv("DRIVER_CACHE_DIR"), cfg.DriverCacheDir)
	cfg.DriverAutoDownload = getenvBool("DRIVER_AUTO_DOWNLOAD", cfg.DriverAutoDownload)
	cfg.ChromeForTestingURL = firstNonEmpty(os.Getenv("CHROME_FOR_TESTING_URL"), cfg.ChromeForTestingURL)
	cfg.Headless = getenvBool("HEADLESS", cfg.Headless)
	cfg.StartupUserAgent = firstNonEmpty(os.Getenv("STARTUP_USER_AGENT"), cfg.StartupUserAgent)
	cfg.LogHTML = getenvBool("LOG_HTML", cfg.LogHTML)
	cfg.DisableMedia = getenvBool("DISABLE_MEDIA", cfg.DisableMedia)
	cfg.PrometheusEnabled = getenvBool("PROMETHEUS_ENABLED", cfg.PrometheusEnabled)
	cfg.PrometheusPort = getenvInt("PROMETHEUS_PORT", cfg.PrometheusPort)

	if proxy := proxyFromEnv(); proxy != nil {
		cfg.DefaultProxy = proxy
	}
	if raw := os.Getenv("LOG_LEVEL"); strings.TrimSpace(raw) != "" {
		*logLevel = raw
	}
}

func (c Config) addr() string {
	return c.Host + ":" + strconv.Itoa(c.Port)
}

func proxyFromEnv() *Proxy {
	url := os.Getenv("PROXY_URL")
	if url == "" {
		return nil
	}
	return &Proxy{
		URL:      url,
		Username: os.Getenv("PROXY_USERNAME"),
		Password: os.Getenv("PROXY_PASSWORD"),
	}
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
