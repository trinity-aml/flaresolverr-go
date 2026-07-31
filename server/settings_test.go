package flaresolverr

import (
	"net/http"
	"strings"
	"testing"
)

func validPayload() settingsPayload {
	return settingsPayload{
		Host:                "0.0.0.0",
		Port:                8191,
		BrowserBackend:      "auto",
		LogLevel:            "info",
		PrometheusPort:      8192,
		ChromeForTestingURL: "https://googlechromelabs.github.io/chrome-for-testing/",
	}
}

func TestToConfigRejectsInvalidPorts(t *testing.T) {
	tests := []struct {
		name           string
		port, promPort int
		wantErr        string
	}{
		{"zero port", 0, 8192, "port must be between"},
		{"negative port", -1, 8192, "port must be between"},
		{"port above range", 70000, 8192, "port must be between"},
		{"zero prometheus port", 8191, 0, "prometheus port must be between"},
		{"prometheus port above range", 8191, 70000, "prometheus port must be between"},
		{"colliding ports", 8191, 8191, "must differ"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload()
			p.Port = tc.port
			p.PrometheusPort = tc.promPort

			_, err := p.toConfig(Config{})
			if err == nil {
				t.Fatalf("expected an error for port=%d prometheusPort=%d", tc.port, tc.promPort)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestToConfigRequiresHost(t *testing.T) {
	p := validPayload()
	p.Host = "   "

	if _, err := p.toConfig(Config{}); err == nil {
		t.Fatal("expected an error for a blank host")
	}
}

// The driver fetched from this URL is chmod 0755'd and executed, so a plaintext
// or relative URL must not be accepted.
func TestToConfigRequiresHTTPSChromeForTestingURL(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/chrome-for-testing/",
		"ftp://example.com/",
		"not-a-url",
		"/relative/path",
	} {
		t.Run(raw, func(t *testing.T) {
			p := validPayload()
			p.ChromeForTestingURL = raw

			if _, err := p.toConfig(Config{}); err == nil {
				t.Errorf("expected %q to be rejected", raw)
			}
		})
	}
}

func TestToConfigAcceptsEmptyChromeForTestingURL(t *testing.T) {
	p := validPayload()
	p.ChromeForTestingURL = ""

	if _, err := p.toConfig(Config{}); err != nil {
		t.Errorf("an empty URL should fall back to the default, got %v", err)
	}
}

func TestToConfigRejectsUnknownLogLevel(t *testing.T) {
	p := validPayload()
	p.LogLevel = "chatty"

	if _, err := p.toConfig(Config{}); err == nil {
		t.Fatal("expected an error for an unsupported log level")
	}
}

func TestToConfigNormalizesWarningLogLevel(t *testing.T) {
	p := validPayload()
	p.LogLevel = "warning"

	cfg, err := p.toConfig(Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "warn")
	}
}

func TestToConfigRejectsUnknownBackend(t *testing.T) {
	p := validPayload()
	p.BrowserBackend = "webkit"

	if _, err := p.toConfig(Config{}); err == nil {
		t.Fatal("expected an error for an unsupported browser backend")
	}
}

func TestToConfigProxyHandling(t *testing.T) {
	t.Run("blank url clears the proxy", func(t *testing.T) {
		p := validPayload()
		p.ProxyURL = "  "

		cfg, err := p.toConfig(Config{DefaultProxy: &Proxy{URL: "http://old:3128"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DefaultProxy != nil {
			t.Errorf("expected the proxy to be cleared, got %+v", cfg.DefaultProxy)
		}
	})

	t.Run("relative url is rejected", func(t *testing.T) {
		p := validPayload()
		p.ProxyURL = "proxy:3128"

		if _, err := p.toConfig(Config{}); err == nil {
			t.Fatal("expected a relative proxy URL to be rejected")
		}
	})

	t.Run("credentials are preserved", func(t *testing.T) {
		p := validPayload()
		p.ProxyURL = "http://proxy.internal:3128"
		p.ProxyUsername = " user "
		p.ProxyPassword = "  pass with spaces  "

		cfg, err := p.toConfig(Config{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DefaultProxy == nil {
			t.Fatal("expected a proxy to be set")
		}
		if cfg.DefaultProxy.Username != "user" {
			t.Errorf("Username = %q, want %q", cfg.DefaultProxy.Username, "user")
		}
		// The password is deliberately not trimmed — leading/trailing spaces
		// can be part of it.
		if cfg.DefaultProxy.Password != "  pass with spaces  " {
			t.Errorf("Password was altered: %q", cfg.DefaultProxy.Password)
		}
	})
}

func TestSettingsPayloadRoundTrip(t *testing.T) {
	original := validPayload()
	original.BrowserPath = "/usr/bin/chromium"
	original.DriverPath = "/usr/bin/chromedriver"
	original.DriverCacheDir = "/var/cache/drivers"
	original.DriverAutoDownload = true
	original.Headless = true
	original.StartupUserAgent = "Mozilla/5.0"
	original.LogHTML = true
	original.DisableMedia = true
	original.PrometheusEnabled = true
	original.ProxyURL = "http://proxy.internal:3128"
	original.ProxyUsername = "user"
	original.ProxyPassword = "pass"

	cfg, err := original.toConfig(Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := settingsPayloadFromConfig(cfg)
	if got != original {
		t.Errorf("round trip changed the payload:\n got: %+v\nwant: %+v", got, original)
	}
}

func TestGuardStateChangingRequest(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		origin      string
		fetchSite   string
		host        string
		wantErr     bool
	}{
		{name: "curl-style json", contentType: "application/json", host: "127.0.0.1:8191"},
		{name: "json with charset", contentType: "application/json; charset=utf-8", host: "127.0.0.1:8191"},
		{name: "same-origin browser", contentType: "application/json", origin: "http://127.0.0.1:8191", fetchSite: "same-origin", host: "127.0.0.1:8191"},
		{name: "address bar navigation", contentType: "application/json", fetchSite: "none", host: "127.0.0.1:8191"},

		// The CSRF vector: a simple request needs no preflight.
		{name: "text/plain simple request", contentType: "text/plain", host: "127.0.0.1:8191", wantErr: true},
		{name: "form encoded simple request", contentType: "application/x-www-form-urlencoded", host: "127.0.0.1:8191", wantErr: true},
		{name: "multipart simple request", contentType: "multipart/form-data; boundary=x", host: "127.0.0.1:8191", wantErr: true},
		{name: "missing content type", contentType: "", host: "127.0.0.1:8191", wantErr: true},

		{name: "cross-site fetch metadata", contentType: "application/json", fetchSite: "cross-site", host: "127.0.0.1:8191", wantErr: true},
		{name: "same-site fetch metadata", contentType: "application/json", fetchSite: "same-site", host: "127.0.0.1:8191", wantErr: true},
		{name: "foreign origin", contentType: "application/json", origin: "http://evil.example", host: "127.0.0.1:8191", wantErr: true},
		{name: "origin with a different port", contentType: "application/json", origin: "http://127.0.0.1:9999", host: "127.0.0.1:8191", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodPost, "http://"+tc.host+"/api/settings", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			r.Host = tc.host
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}

			gotErr := guardStateChangingRequest(r) != nil
			if gotErr != tc.wantErr {
				t.Errorf("guardStateChangingRequest error = %v, want error = %v", gotErr, tc.wantErr)
			}
		})
	}
}
