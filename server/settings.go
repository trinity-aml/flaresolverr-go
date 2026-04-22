package flaresolverr

import (
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
)

type settingsPayload struct {
	Host                string `json:"host"`
	Port                int    `json:"port"`
	BrowserBackend      string `json:"browserBackend"`
	BrowserPath         string `json:"browserPath"`
	DriverPath          string `json:"driverPath"`
	DriverCacheDir      string `json:"driverCacheDir"`
	DriverAutoDownload  bool   `json:"driverAutoDownload"`
	ChromeForTestingURL string `json:"chromeForTestingURL"`
	Headless            bool   `json:"headless"`
	StartupUserAgent    string `json:"startupUserAgent"`
	LogLevel            string `json:"logLevel"`
	LogHTML             bool   `json:"logHTML"`
	DisableMedia        bool   `json:"disableMedia"`
	PrometheusEnabled   bool   `json:"prometheusEnabled"`
	PrometheusPort      int    `json:"prometheusPort"`
	ProxyURL            string `json:"proxyURL"`
	ProxyUsername       string `json:"proxyUsername"`
	ProxyPassword       string `json:"proxyPassword"`
}

type settingsResponse struct {
	Status          string          `json:"status"`
	Message         string          `json:"message,omitempty"`
	Config          settingsPayload `json:"config"`
	ConfigPath      string          `json:"configPath,omitempty"`
	RestartRequired []string        `json:"restartRequired,omitempty"`
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error":       http.StatusText(http.StatusMethodNotAllowed),
			"status_code": http.StatusMethodNotAllowed,
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(settingsPageHTML))
}

func (s *Server) handleSettingsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		writeJSON(w, http.StatusOK, settingsResponse{
			Status:     StatusOK,
			Config:     settingsPayloadFromConfig(cfg),
			ConfigPath: s.currentConfigPath(),
		})
	case http.MethodPost:
		defer r.Body.Close()

		var payload settingsPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":       "invalid json body",
				"status_code": http.StatusBadRequest,
			})
			return
		}

		cfg, err := payload.toConfig(s.currentConfig())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":       err.Error(),
				"status_code": http.StatusBadRequest,
			})
			return
		}

		path, err := SaveConfig(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":       err.Error(),
				"status_code": http.StatusInternalServerError,
			})
			return
		}

		s.cfgMu.Lock()
		s.configPath = path
		s.cfgMu.Unlock()

		restartRequired, err := s.applyConfig(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":       err.Error(),
				"status_code": http.StatusInternalServerError,
			})
			return
		}

		writeJSON(w, http.StatusOK, settingsResponse{
			Status:          StatusOK,
			Message:         "Settings saved and applied.",
			Config:          settingsPayloadFromConfig(s.currentConfig()),
			ConfigPath:      path,
			RestartRequired: restartRequired,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error":       http.StatusText(http.StatusMethodNotAllowed),
			"status_code": http.StatusMethodNotAllowed,
		})
	}
}

func (s *Server) currentConfigPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.configPath
}

func settingsPayloadFromConfig(cfg Config) settingsPayload {
	payload := settingsPayload{
		Host:                cfg.Host,
		Port:                cfg.Port,
		BrowserBackend:      canonicalBrowserBackend(cfg.BrowserBackend),
		BrowserPath:         cfg.BrowserPath,
		DriverPath:          cfg.DriverPath,
		DriverCacheDir:      cfg.DriverCacheDir,
		DriverAutoDownload:  cfg.DriverAutoDownload,
		ChromeForTestingURL: cfg.ChromeForTestingURL,
		Headless:            cfg.Headless,
		StartupUserAgent:    cfg.StartupUserAgent,
		LogLevel:            canonicalLogLevel(cfg.LogLevel),
		LogHTML:             cfg.LogHTML,
		DisableMedia:        cfg.DisableMedia,
		PrometheusEnabled:   cfg.PrometheusEnabled,
		PrometheusPort:      cfg.PrometheusPort,
	}
	if cfg.DefaultProxy != nil {
		payload.ProxyURL = cfg.DefaultProxy.URL
		payload.ProxyUsername = cfg.DefaultProxy.Username
		payload.ProxyPassword = cfg.DefaultProxy.Password
	}
	return payload
}

func (p settingsPayload) toConfig(base Config) (Config, error) {
	base = base.withDefaults()

	host := strings.TrimSpace(p.Host)
	if host == "" {
		return Config{}, fmt.Errorf("host is required")
	}
	if p.Port <= 0 {
		return Config{}, fmt.Errorf("port must be greater than zero")
	}
	if p.PrometheusPort <= 0 {
		return Config{}, fmt.Errorf("prometheus port must be greater than zero")
	}

	logLevel := canonicalLogLevel(p.LogLevel)
	if strings.TrimSpace(p.LogLevel) != "" && logLevel != strings.ToLower(strings.TrimSpace(p.LogLevel)) {
		switch strings.ToLower(strings.TrimSpace(p.LogLevel)) {
		case "warning":
			logLevel = "warn"
		case "debug", "info", "warn", "error":
		default:
			return Config{}, fmt.Errorf("unsupported log level")
		}
	}

	backend := canonicalBrowserBackend(p.BrowserBackend)
	if strings.TrimSpace(p.BrowserBackend) != "" && backend == "auto" && strings.ToLower(strings.TrimSpace(p.BrowserBackend)) != "auto" {
		return Config{}, fmt.Errorf("unsupported browser backend")
	}

	cfg := base
	cfg.Host = host
	cfg.Port = p.Port
	cfg.BrowserBackend = backend
	cfg.BrowserPath = strings.TrimSpace(p.BrowserPath)
	cfg.DriverPath = strings.TrimSpace(p.DriverPath)
	cfg.DriverCacheDir = strings.TrimSpace(p.DriverCacheDir)
	cfg.DriverAutoDownload = p.DriverAutoDownload
	cfg.ChromeForTestingURL = strings.TrimSpace(p.ChromeForTestingURL)
	cfg.Headless = p.Headless
	cfg.StartupUserAgent = strings.TrimSpace(p.StartupUserAgent)
	cfg.LogLevel = logLevel
	cfg.LogHTML = p.LogHTML
	cfg.DisableMedia = p.DisableMedia
	cfg.PrometheusEnabled = p.PrometheusEnabled
	cfg.PrometheusPort = p.PrometheusPort
	cfg.Logger = nil

	proxyURL := strings.TrimSpace(p.ProxyURL)
	if proxyURL == "" {
		cfg.DefaultProxy = nil
	} else {
		parsed, err := neturl.Parse(proxyURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Config{}, fmt.Errorf("proxy url must be an absolute URL")
		}
		cfg.DefaultProxy = &Proxy{
			URL:      proxyURL,
			Username: strings.TrimSpace(p.ProxyUsername),
			Password: p.ProxyPassword,
		}
	}

	return cfg, nil
}
