package flaresolverr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shape init.yaml actually ships in: a header, per-key explanations and a
// commented-out proxy block. A save that loses any of it is the bug this file
// exists to catch.
const commentedConfig = `# Defaults for flaresolverr-go.
# Lookup order:
# 1. ./init.yaml from current working directory

host: 0.0.0.0
port: 8191

# auto | chromedriver | geckodriver
browser_backend: auto

# Empty = detect automatically, per backend.
browser_path: ""
driver_path: ""
driver_auto_download: true
chrome_for_testing_url: https://googlechromelabs.github.io/chrome-for-testing

headless: true
log_level: info
log_html: false
disable_media: false

prometheus_enabled: false
prometheus_port: 8192

# proxy:
#   url: http://proxy.example:8080
`

func saveInto(t *testing.T, initial string, cfg Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "init.yaml")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveConfigToPath(path, cfg); err != nil {
		t.Fatalf("saveConfigToPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSaveConfigKeepsComments(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogLevel = "debug"
	cfg.DisableMedia = true

	got := saveInto(t, commentedConfig, cfg)

	for _, want := range []string{
		"# Defaults for flaresolverr-go.",
		"# 1. ./init.yaml from current working directory",
		"# auto | chromedriver | geckodriver",
		"# Empty = detect automatically, per backend.",
		"# proxy:",
		"#   url: http://proxy.example:8080",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment lost from init.yaml: %q\n--- got ---\n%s", want, got)
		}
	}

	if !strings.Contains(got, "log_level: debug") {
		t.Errorf("log_level not updated:\n%s", got)
	}
	if !strings.Contains(got, "disable_media: true") {
		t.Errorf("disable_media not updated:\n%s", got)
	}
}

func TestSaveConfigKeepsLayoutByteForByte(t *testing.T) {
	// Saving without changing anything must not touch the file at all: blank
	// lines group the config visually, and yaml.v3 drops them on a re-marshal
	// unless they are re-attached.
	cfg, warnings := loadConfigFromData(t, commentedConfig)
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}

	got := saveInto(t, commentedConfig, cfg)
	if got != commentedConfig {
		t.Errorf("save changed the file layout\n--- want ---\n%s\n--- got ---\n%s", commentedConfig, got)
	}
}

func TestSaveConfigKeepsEmptyKeysRatherThanDroppingThem(t *testing.T) {
	// browser_path is omitempty, so a plain marshal deletes the line when the
	// value is empty — and takes the comment above it along.
	cfg := DefaultConfig()
	cfg.BrowserPath = ""

	got := saveInto(t, commentedConfig, cfg)

	if !strings.Contains(got, "browser_path:") {
		t.Errorf("browser_path key dropped:\n%s", got)
	}
	if !strings.Contains(got, "# Empty = detect automatically, per backend.") {
		t.Errorf("comment above browser_path dropped:\n%s", got)
	}
}

func TestSaveConfigClearsAValueThatWasSet(t *testing.T) {
	withPath := strings.Replace(commentedConfig,
		`browser_path: ""`, `browser_path: /usr/bin/google-chrome`, 1)

	cfg := DefaultConfig()
	cfg.BrowserPath = ""

	got := saveInto(t, withPath, cfg)

	if strings.Contains(got, "google-chrome") {
		t.Errorf("cleared browser_path still carries the old value:\n%s", got)
	}
	var round configFile
	if err := yaml.Unmarshal([]byte(got), &round); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	if round.BrowserPath != "" {
		t.Errorf("BrowserPath = %q, want empty", round.BrowserPath)
	}
}

func TestSaveConfigAppendsKeysMissingFromTheFile(t *testing.T) {
	// A config written before a knob existed must gain it, not silently ignore
	// whatever the user set in the UI.
	minimal := "host: 0.0.0.0\nport: 8191\n"

	cfg := DefaultConfig()
	cfg.PrometheusEnabled = true
	cfg.PrometheusPort = 9999

	got := saveInto(t, minimal, cfg)

	var round configFile
	if err := yaml.Unmarshal([]byte(got), &round); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	if round.PrometheusEnabled == nil || !*round.PrometheusEnabled {
		t.Errorf("prometheus_enabled not written:\n%s", got)
	}
	if round.PrometheusPort != 9999 {
		t.Errorf("prometheus_port = %d, want 9999:\n%s", round.PrometheusPort, got)
	}
}

func TestSaveConfigRoundTripsEveryField(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 9191
	cfg.BrowserBackend = "geckodriver"
	cfg.BrowserPath = "/usr/bin/camoufox"
	cfg.DriverPath = "/usr/bin/geckodriver"
	cfg.DriverCacheDir = "/tmp/cache"
	cfg.DriverAutoDownload = false
	cfg.ChromeForTestingURL = "https://example.invalid/cft"
	cfg.Headless = false
	cfg.StartupUserAgent = "probe/1.0"
	cfg.LogLevel = "debug"
	cfg.LogHTML = true
	cfg.DisableMedia = true
	cfg.PrometheusEnabled = true
	cfg.PrometheusPort = 9999
	cfg.DefaultProxy = &Proxy{URL: "http://p:8080", Username: "u", Password: "p"}

	got := saveInto(t, commentedConfig, cfg)

	loaded, warnings := loadConfigFromData(t, got)
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}

	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"Host", loaded.Host, cfg.Host},
		{"Port", loaded.Port, cfg.Port},
		{"BrowserBackend", loaded.BrowserBackend, cfg.BrowserBackend},
		{"BrowserPath", loaded.BrowserPath, cfg.BrowserPath},
		{"DriverPath", loaded.DriverPath, cfg.DriverPath},
		{"DriverCacheDir", loaded.DriverCacheDir, cfg.DriverCacheDir},
		{"DriverAutoDownload", loaded.DriverAutoDownload, cfg.DriverAutoDownload},
		{"ChromeForTestingURL", loaded.ChromeForTestingURL, cfg.ChromeForTestingURL},
		{"Headless", loaded.Headless, cfg.Headless},
		{"StartupUserAgent", loaded.StartupUserAgent, cfg.StartupUserAgent},
		{"LogLevel", loaded.LogLevel, cfg.LogLevel},
		{"LogHTML", loaded.LogHTML, cfg.LogHTML},
		{"DisableMedia", loaded.DisableMedia, cfg.DisableMedia},
		{"PrometheusEnabled", loaded.PrometheusEnabled, cfg.PrometheusEnabled},
		{"PrometheusPort", loaded.PrometheusPort, cfg.PrometheusPort},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if loaded.DefaultProxy == nil || loaded.DefaultProxy.URL != "http://p:8080" ||
		loaded.DefaultProxy.Username != "u" || loaded.DefaultProxy.Password != "p" {
		t.Errorf("DefaultProxy = %+v, want the saved one", loaded.DefaultProxy)
	}
}

func TestSaveConfigFallsBackWhenTheFileIsUnusable(t *testing.T) {
	cases := map[string]string{
		"malformed":  "host: [unterminated\n",
		"not a map":  "- one\n- two\n",
		"empty file": "",
	}
	for name, initial := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "init.yaml")
			if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.LogLevel = "debug"
			if err := saveConfigToPath(path, cfg); err != nil {
				t.Fatalf("save must not fail on a bad config file: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var round configFile
			if err := yaml.Unmarshal(data, &round); err != nil {
				t.Fatalf("fallback produced unparseable yaml: %v\n%s", err, data)
			}
			if round.LogLevel != "debug" {
				t.Errorf("log_level = %q, want debug\n%s", round.LogLevel, data)
			}
		})
	}
}

func TestSaveConfigCreatesAFileThatDoesNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "init.yaml")
	cfg := DefaultConfig()
	cfg.LogLevel = "warn"
	if err := saveConfigToPath(path, cfg); err != nil {
		t.Fatalf("saveConfigToPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var round configFile
	if err := yaml.Unmarshal(data, &round); err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if round.LogLevel != "warn" {
		t.Errorf("log_level = %q, want warn", round.LogLevel)
	}
}

// TestConfigFileKeysMatchTheStruct guards the reflection helper: a knob added to
// configFile without a yaml tag would silently stop being merge-aware.
func TestConfigFileKeysMatchTheStruct(t *testing.T) {
	keys := configFileKeys()
	for _, want := range []string{
		"host", "port", "browser_backend", "browser_path", "driver_path",
		"driver_cache_dir", "driver_auto_download", "chrome_for_testing_url",
		"headless", "startup_user_agent", "log_level", "log_html",
		"disable_media", "prometheus_enabled", "prometheus_port", "proxy",
	} {
		if _, ok := keys[want]; !ok {
			t.Errorf("configFileKeys() is missing %q", want)
		}
	}
	if got, want := len(keys), 16; got != want {
		t.Errorf("configFileKeys() has %d keys, want %d — update this test with the new knob", got, want)
	}
}

func loadConfigFromData(t *testing.T, data string) (Config, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "init.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadConfig([]string{path})
}
