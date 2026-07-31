package flaresolverr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalBrowserBackend(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "auto"},
		{"auto", "auto"},
		{"AUTO", "auto"},
		{"  auto  ", "auto"},
		{"geckodriver", "geckodriver"},
		{"firefox", "geckodriver"},
		{"camoufox", "geckodriver"},
		{"Camoufox", "geckodriver"},
		{"chromedriver", "chromedriver"},
		{"chrome", "chromedriver"},
		{"chromium", "chromedriver"},
		{"nonsense", "auto"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := canonicalBrowserBackend(tc.in); got != tc.want {
				t.Errorf("canonicalBrowserBackend(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalLogLevel(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "info"},
		{"debug", "debug"},
		{"DEBUG", "debug"},
		{" warn ", "warn"},
		{"warning", "warn"},
		{"error", "error"},
		{"nonsense", "info"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := canonicalLogLevel(tc.in); got != tc.want {
				t.Errorf("canonicalLogLevel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPrepareConfigFillsDefaultsAndLogger(t *testing.T) {
	cfg := PrepareConfig(Config{})

	if cfg.Logger == nil {
		t.Error("PrepareConfig must always produce a usable Logger")
	}
	if cfg.Host == "" || cfg.Port == 0 {
		t.Errorf("host/port defaults not applied: %q:%d", cfg.Host, cfg.Port)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.DebugLogging {
		t.Error("DebugLogging must be false at the default log level")
	}
}

func TestPrepareConfigDerivesDebugLogging(t *testing.T) {
	cfg := PrepareConfig(Config{LogLevel: "DEBUG"})

	if !cfg.DebugLogging {
		t.Error("DebugLogging must be derived from LogLevel=debug")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want it canonicalized to %q", cfg.LogLevel, "debug")
	}
}

// SaveConfig -> load must be lossless for every knob exposed in the settings UI,
// otherwise saving from /settings silently drops configuration.
func TestSaveConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "init.yaml")

	original := Config{
		Host:                "127.0.0.1",
		Port:                9191,
		BrowserBackend:      "geckodriver",
		BrowserPath:         "/usr/bin/camoufox",
		DriverPath:          "/usr/bin/geckodriver",
		DriverCacheDir:      "/var/cache/drivers",
		DriverAutoDownload:  true,
		ChromeForTestingURL: "https://example.com/cft/",
		Headless:            true,
		StartupUserAgent:    "Mozilla/5.0 (X11; Linux x86_64)",
		LogLevel:            "debug",
		LogHTML:             true,
		DisableMedia:        true,
		PrometheusEnabled:   true,
		PrometheusPort:      9192,
		DefaultProxy:        &Proxy{URL: "http://proxy:3128", Username: "u", Password: "p"},
	}

	if err := saveConfigToPath(path, original); err != nil {
		t.Fatalf("saveConfigToPath: %v", err)
	}

	loaded, warnings := loadConfig([]string{path})
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"Host", loaded.Host, original.Host},
		{"Port", loaded.Port, original.Port},
		{"BrowserBackend", loaded.BrowserBackend, original.BrowserBackend},
		{"BrowserPath", loaded.BrowserPath, original.BrowserPath},
		{"DriverPath", loaded.DriverPath, original.DriverPath},
		{"DriverCacheDir", loaded.DriverCacheDir, original.DriverCacheDir},
		{"DriverAutoDownload", loaded.DriverAutoDownload, original.DriverAutoDownload},
		{"ChromeForTestingURL", loaded.ChromeForTestingURL, original.ChromeForTestingURL},
		{"Headless", loaded.Headless, original.Headless},
		{"StartupUserAgent", loaded.StartupUserAgent, original.StartupUserAgent},
		{"LogLevel", loaded.LogLevel, original.LogLevel},
		{"LogHTML", loaded.LogHTML, original.LogHTML},
		{"DisableMedia", loaded.DisableMedia, original.DisableMedia},
		{"PrometheusEnabled", loaded.PrometheusEnabled, original.PrometheusEnabled},
		{"PrometheusPort", loaded.PrometheusPort, original.PrometheusPort},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if loaded.DefaultProxy == nil {
		t.Fatal("proxy was dropped on the round trip")
	}
	if loaded.DefaultProxy.URL != original.DefaultProxy.URL ||
		loaded.DefaultProxy.Username != original.DefaultProxy.Username ||
		loaded.DefaultProxy.Password != original.DefaultProxy.Password {
		t.Errorf("proxy = %+v, want %+v", loaded.DefaultProxy, original.DefaultProxy)
	}
}

// The save must be atomic: no ".tmp" leftovers, and the target either has the
// old content or the new one.
func TestSaveConfigLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "init.yaml")

	for range 5 {
		if err := saveConfigToPath(path, Config{Host: "0.0.0.0", Port: 8191}); err != nil {
			t.Fatalf("saveConfigToPath: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "init.yaml" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only init.yaml to remain, got %v", names)
	}
}

// A missing init.yaml is explicitly non-fatal: the defaults must stand.
func TestLoadConfigFromMissingPathIsNotFatal(t *testing.T) {
	cfg, warnings := loadConfig([]string{filepath.Join(t.TempDir(), "absent.yaml")})

	if len(warnings) != 0 {
		t.Errorf("a missing config file must be silent, got warnings: %v", warnings)
	}
	if cfg.Port != defaultConfigValues().Port {
		t.Errorf("Port = %d, want the default %d", cfg.Port, defaultConfigValues().Port)
	}
}

// A malformed init.yaml is also non-fatal — it warns and falls back.
func TestLoadConfigFromMalformedPathWarnsButDoesNotFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "init.yaml")
	if err := os.WriteFile(path, []byte("port: [not an int]\n\tbad indent"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, warnings := loadConfig([]string{path})
	if len(warnings) == 0 {
		t.Error("expected a warning for a malformed init.yaml")
	}
	if cfg.Port != defaultConfigValues().Port {
		t.Errorf("Port = %d, want the default %d after a parse failure", cfg.Port, defaultConfigValues().Port)
	}
}

// Environment overrides the file, which overrides the defaults.
func TestLoadConfigPrecedenceEnvBeatsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "init.yaml")
	if err := saveConfigToPath(path, Config{Host: "10.0.0.1", Port: 1111, LogLevel: "warn"}); err != nil {
		t.Fatalf("saveConfigToPath: %v", err)
	}

	// Baseline: the file wins over the defaults.
	cfg, _ := loadConfig([]string{path})
	if cfg.Host != "10.0.0.1" || cfg.Port != 1111 {
		t.Fatalf("file values not applied: %s:%d", cfg.Host, cfg.Port)
	}

	t.Setenv("PORT", "2222")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, _ = loadConfig([]string{path})
	if cfg.Port != 2222 {
		t.Errorf("Port = %d, want the env override 2222", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want the env override %q", cfg.LogLevel, "debug")
	}
	if cfg.Host != "10.0.0.1" {
		t.Errorf("Host = %q, want the file value to survive when no env var is set", cfg.Host)
	}
}
