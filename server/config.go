package flaresolverr

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/trinity-aml/flaresolverr-go/internal/buildinfo"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Host                string
	Port                int
	BrowserBackend      string // "auto" | "chromedriver" | "geckodriver"
	BrowserPath         string
	DriverPath          string
	DriverCacheDir      string
	DriverAutoDownload  bool
	ChromeForTestingURL string
	Headless            bool
	StartupUserAgent    string
	LogLevel            string
	LogHTML             bool
	DebugLogging        bool
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
	BrowserBackend      string           `yaml:"browser_backend,omitempty"`
	BrowserPath         string           `yaml:"browser_path,omitempty"`
	DriverPath          string           `yaml:"driver_path,omitempty"`
	DriverCacheDir      string           `yaml:"driver_cache_dir,omitempty"`
	DriverAutoDownload  *bool            `yaml:"driver_auto_download"`
	ChromeForTestingURL string           `yaml:"chrome_for_testing_url,omitempty"`
	Headless            *bool            `yaml:"headless"`
	StartupUserAgent    string           `yaml:"startup_user_agent,omitempty"`
	LogLevel            string           `yaml:"log_level,omitempty"`
	LogHTML             *bool            `yaml:"log_html"`
	DisableMedia        *bool            `yaml:"disable_media"`
	PrometheusEnabled   *bool            `yaml:"prometheus_enabled"`
	PrometheusPort      int              `yaml:"prometheus_port"`
	DefaultProxy        *configFileProxy `yaml:"proxy,omitempty"`
}

type configFileProxy struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

func DefaultConfig() Config {
	return PrepareConfig(defaultConfigValues())
}

func LoadConfig() (Config, []string) {
	return loadConfig(defaultInitConfigPaths())
}

func loadConfig(searchPaths []string) (Config, []string) {
	cfg := defaultConfigValues()
	var warnings []string

	if fileCfg, _, err := readConfigFile(searchPaths); err != nil {
		warnings = append(warnings, "init.yaml ignored: "+err.Error())
	} else if fileCfg != nil {
		applyConfigFile(&cfg, *fileCfg)
	}

	applyEnvConfig(&cfg)

	return PrepareConfig(cfg), warnings
}

func PrepareConfig(cfg Config) Config {
	cfg = cfg.withDefaults()
	cfg.LogLevel = canonicalLogLevel(cfg.LogLevel)
	cfg.DebugLogging = parseLogLevel(cfg.LogLevel) <= slog.LevelDebug
	cfg.Logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))
	return cfg
}

func (c Config) withDefaults() Config {
	base := defaultConfigValues()
	if c.Host == "" {
		c.Host = base.Host
	}
	if c.Port == 0 {
		c.Port = base.Port
	}
	if c.BrowserBackend == "" {
		c.BrowserBackend = base.BrowserBackend
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
	if c.LogLevel == "" {
		c.LogLevel = base.LogLevel
	}
	if c.PrometheusPort == 0 {
		c.PrometheusPort = base.PrometheusPort
	}
	if c.Version == "" {
		c.Version = base.Version
	}
	if c.DefaultProxy == nil {
		c.DefaultProxy = base.DefaultProxy
	}
	return c
}

func defaultConfigValues() Config {
	return Config{
		Host:           "0.0.0.0",
		Port:           8191,
		BrowserBackend: "auto",
		// Deliberately empty: which browser to look for depends on the backend,
		// and only the backend knows that. Defaulting to findChromeBinary() here
		// meant geckodriver was handed Chrome's path on any machine that had
		// both, and refused it with "binary is not a Firefox executable" — while
		// the camoufox detection in newGeckoDriverBackend never ran, because the
		// path it guards on was no longer empty.
		BrowserPath:         "",
		DriverAutoDownload:  true,
		ChromeForTestingURL: defaultChromeForTestingBaseURL,
		Headless:            true,
		LogLevel:            "info",
		PrometheusPort:      8192,
		Version:             firstNonEmpty(buildinfo.Version, "dev"),
	}
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

func ResolveConfigPath() (string, error) {
	paths := defaultInitConfigPaths()
	if len(paths) == 0 {
		return "", fmt.Errorf("init.yaml path could not be resolved")
	}
	if _, path, err := readConfigFile(paths); path != "" {
		return path, nil
	} else if err != nil {
		return paths[0], nil
	}
	return paths[0], nil
}

func SaveConfig(cfg Config) (string, error) {
	path, err := ResolveConfigPath()
	if err != nil {
		return "", err
	}
	if err := saveConfigToPath(path, cfg); err != nil {
		return "", err
	}
	return path, nil
}

func saveConfigToPath(path string, cfg Config) error {
	fileCfg := configFileFromConfig(cfg)
	data, err := renderConfigYAML(path, fileCfg)
	if err != nil {
		return fmt.Errorf("marshal init.yaml: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create init.yaml directory: %w", err)
	}
	// A unique temp file in the target directory, not a fixed path+".tmp":
	// two concurrent saves sharing one temp name can interleave truncate and
	// write, and whichever renames second publishes a half-written init.yaml.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write init.yaml: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write init.yaml: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write init.yaml: %w", err)
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return fmt.Errorf("write init.yaml: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace init.yaml: %w", err)
	}
	return nil
}

// renderConfigYAML produces the bytes to write, editing the existing file's node
// tree in place rather than re-marshalling from scratch.
//
// Marshalling the struct is lossy in a way that matters: init.yaml ships with a
// header, an explanation of every browser_backend value and a commented-out
// proxy block, and a plain yaml.Marshal reduced all of that to eleven bare
// lines. Saving once from /settings therefore destroyed the documentation in the
// user's own config — including the keys whose value is empty, which omitempty
// drops entirely.
//
// Anything unparseable, missing or shaped unexpectedly falls back to the plain
// render: saving settings must not fail because a config file is malformed.
func renderConfigYAML(path string, fileCfg configFile) ([]byte, error) {
	desired, err := mappingNodeOf(fileCfg)
	if err != nil {
		return nil, err
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		return yaml.Marshal(&fileCfg)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return yaml.Marshal(&fileCfg)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return yaml.Marshal(&fileCfg)
	}

	mergeMapping(doc.Content[0], desired, configFileKeys())
	preserveBlankLines(doc.Content[0], string(existing))
	return yaml.Marshal(&doc)
}

// preserveBlankLines re-attaches the blank lines that separate groups of keys.
//
// yaml.v3 records comments but not blank lines, so a re-marshal runs the whole
// file together and loses the grouping. It does emit one for a HeadComment that
// starts with a newline, which is the only lever available — so find the keys
// that had a blank line above them in the source and mark them.
func preserveBlankLines(mapping *yaml.Node, source string) {
	lines := strings.Split(source, "\n")

	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if i == 0 {
			// A blank above the very first key belongs to the document's own
			// head comment, which already emits one. Adding another doubles it.
			continue
		}
		key := mapping.Content[i]
		if key.Line <= 0 || strings.HasPrefix(key.HeadComment, "\n") {
			continue
		}

		// key.Line is the key itself; its head comment sits on the lines above.
		above := key.Line - 2
		for above >= 0 && above < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[above]), "#") {
			above--
		}
		if above >= 0 && above < len(lines) && strings.TrimSpace(lines[above]) == "" {
			key.HeadComment = "\n" + key.HeadComment
		}
	}
}

// mappingNodeOf marshals v and returns its top-level mapping node.
func mappingNodeOf(v any) (*yaml.Node, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config did not marshal to a mapping")
	}
	return doc.Content[0], nil
}

// configFileKeys is the set of yaml keys configFile owns, read off the struct
// tags so it cannot drift from them.
func configFileKeys() map[string]struct{} {
	keys := map[string]struct{}{}
	t := reflect.TypeOf(configFile{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			keys[name] = struct{}{}
		}
	}
	return keys
}

// mergeMapping writes src's values into dst, keeping dst's key order, comments
// and any keys it does not own.
func mergeMapping(dst, src *yaml.Node, owned map[string]struct{}) {
	index := map[string]int{}
	for i := 0; i+1 < len(dst.Content); i += 2 {
		index[dst.Content[i].Value] = i
	}

	seen := map[string]struct{}{}
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		seen[key.Value] = struct{}{}

		at, ok := index[key.Value]
		if !ok {
			// A key the file does not carry yet — a knob added since it was
			// written. Append it with the comment the marshalled struct had.
			dst.Content = append(dst.Content, key, value)
			index[key.Value] = len(dst.Content) - 2
			continue
		}

		old := dst.Content[at+1]
		if old.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			mergeMapping(old, value, owned)
			continue
		}
		// Carry the old node's comments onto the new value: they annotate the
		// setting, not the particular value that happened to be there.
		value.HeadComment = old.HeadComment
		value.LineComment = old.LineComment
		value.FootComment = old.FootComment
		dst.Content[at+1] = value
	}

	// Keys the file has that the struct dropped. Every omitempty field in
	// configFile is either a string or the proxy mapping, so an absent one means
	// "empty" — write that rather than deleting the line, which would take the
	// key's comments with it. A dropped mapping really is gone, though.
	for i := 0; i+1 < len(dst.Content); {
		key, value := dst.Content[i], dst.Content[i+1]
		_, isOwned := owned[key.Value]
		_, isSet := seen[key.Value]
		switch {
		case !isOwned || isSet:
			i += 2
		case value.Kind == yaml.ScalarNode:
			value.SetString("")
			i += 2
		default:
			dst.Content = append(dst.Content[:i], dst.Content[i+2:]...)
		}
	}
}

func configFileFromConfig(cfg Config) configFile {
	fileCfg := configFile{
		Host:                cfg.Host,
		Port:                cfg.Port,
		BrowserBackend:      cfg.BrowserBackend,
		BrowserPath:         cfg.BrowserPath,
		DriverPath:          cfg.DriverPath,
		DriverCacheDir:      cfg.DriverCacheDir,
		DriverAutoDownload:  boolPtr(cfg.DriverAutoDownload),
		ChromeForTestingURL: cfg.ChromeForTestingURL,
		Headless:            boolPtr(cfg.Headless),
		StartupUserAgent:    cfg.StartupUserAgent,
		LogLevel:            canonicalLogLevel(cfg.LogLevel),
		LogHTML:             boolPtr(cfg.LogHTML),
		DisableMedia:        boolPtr(cfg.DisableMedia),
		PrometheusEnabled:   boolPtr(cfg.PrometheusEnabled),
		PrometheusPort:      cfg.PrometheusPort,
	}
	if cfg.DefaultProxy != nil && strings.TrimSpace(cfg.DefaultProxy.URL) != "" {
		fileCfg.DefaultProxy = &configFileProxy{
			URL:      cfg.DefaultProxy.URL,
			Username: cfg.DefaultProxy.Username,
			Password: cfg.DefaultProxy.Password,
		}
	}
	return fileCfg
}

func boolPtr(value bool) *bool {
	v := value
	return &v
}

func applyConfigFile(cfg *Config, fileCfg configFile) {
	if strings.TrimSpace(fileCfg.Host) != "" {
		cfg.Host = fileCfg.Host
	}
	if fileCfg.Port > 0 {
		cfg.Port = fileCfg.Port
	}
	if strings.TrimSpace(fileCfg.BrowserBackend) != "" {
		cfg.BrowserBackend = canonicalBrowserBackend(fileCfg.BrowserBackend)
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
	// log_level was written by SaveConfig but never read back here, so setting
	// the level from /settings appeared to work and was silently lost on the
	// next start.
	if strings.TrimSpace(fileCfg.LogLevel) != "" {
		cfg.LogLevel = canonicalLogLevel(fileCfg.LogLevel)
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

func applyEnvConfig(cfg *Config) {
	cfg.Host = getenv("HOST", cfg.Host)
	cfg.Port = getenvInt("PORT", cfg.Port)
	if envBackend := strings.TrimSpace(os.Getenv("BROWSER_BACKEND")); envBackend != "" {
		cfg.BrowserBackend = canonicalBrowserBackend(envBackend)
	}
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
	cfg.LogLevel = firstNonEmpty(os.Getenv("LOG_LEVEL"), cfg.LogLevel)

	if proxy := proxyFromEnv(); proxy != nil {
		cfg.DefaultProxy = proxy
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
	switch canonicalLogLevel(raw) {
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

// canonicalBrowserBackend normalises user-supplied backend names.
func canonicalBrowserBackend(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "geckodriver", "firefox", "camoufox":
		return "geckodriver"
	case "chromedriver", "chrome", "chromium":
		return "chromedriver"
	case "", "auto":
		return "auto"
	default:
		return "auto"
	}
}

func canonicalLogLevel(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	default:
		return "info"
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
