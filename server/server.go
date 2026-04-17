package flaresolverr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Server struct {
	cfg        Config
	service    *Service
	httpServer *http.Server
	metrics    *metricsRegistry
	startOnce  sync.Once
	wg         sync.WaitGroup
	cfgMu      sync.RWMutex
	configPath string
}

func NewServer(cfg Config) *Server {
	cfg = PrepareConfig(cfg)
	metrics, err := newMetricsRegistry(cfg)
	if err != nil {
		panic(err)
	}
	configPath, _ := ResolveConfigPath()
	server := &Server{
		cfg:        cfg,
		service:    NewService(cfg),
		metrics:    metrics,
		configPath: configPath,
	}
	server.httpServer = &http.Server{
		Addr:    cfg.addr(),
		Handler: server.routes(),
	}
	return server
}

func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

func (s *Server) ListenAndServe() error {
	s.startOnce.Do(func() {
		s.metrics.Start(&s.wg, s.currentLogger())
	})
	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return err
	}
	if err := s.metrics.Shutdown(ctx); err != nil {
		return err
	}
	s.wg.Wait()
	return s.service.Close()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":       http.StatusText(http.StatusNotFound),
				"status_code": http.StatusNotFound,
			})
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"error":       http.StatusText(http.StatusMethodNotAllowed),
				"status_code": http.StatusMethodNotAllowed,
			})
			return
		}
		writeJSON(w, http.StatusOK, s.service.Index(r.Context()))
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"error":       http.StatusText(http.StatusMethodNotAllowed),
				"status_code": http.StatusMethodNotAllowed,
			})
			return
		}
		writeJSON(w, http.StatusOK, s.service.Health(r.Context()))
	})

	mux.HandleFunc("/settings", s.handleSettingsPage)
	mux.HandleFunc("/api/settings", s.handleSettingsAPI)

	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"error":       http.StatusText(http.StatusMethodNotAllowed),
				"status_code": http.StatusMethodNotAllowed,
			})
			return
		}

		defer r.Body.Close()

		var req V1Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":       "invalid json body",
				"status_code": http.StatusBadRequest,
			})
			return
		}
		cfg := s.currentConfig()
		if req.Proxy == nil && cfg.DefaultProxy != nil {
			req.Proxy = cfg.DefaultProxy
		}

		res, status := s.service.ControllerV1(r.Context(), &req)
		if cfg.PrometheusEnabled {
			s.metrics.Observe(&req, &res)
		}
		writeJSON(w, status, res)
	})

	var handler http.Handler = mux
	handler = s.errorPlugin(handler)
	handler = s.loggerPlugin(handler)
	return handler
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) loggerPlugin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		if r.URL.Path == "/health" {
			return
		}
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		s.currentLogger().Info("http request", "remote_addr", r.RemoteAddr, "method", r.Method, "url", r.URL.String(), "status", status)
	})
}

func (s *Server) errorPlugin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.currentLogger().Error("panic", "err", recovered)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": fmt.Sprint(recovered),
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) currentConfig() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) currentLogger() Logger {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Logger
}

func (s *Server) applyConfig(cfg Config) ([]string, error) {
	cfg = PrepareConfig(cfg)
	current := s.currentConfig()
	restartRequired := make([]string, 0, 1)
	if current.Host != cfg.Host || current.Port != cfg.Port {
		restartRequired = append(restartRequired, "main HTTP listen address")
	}

	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()

	s.service.ApplyConfig(cfg)

	applyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.metrics.ApplyConfig(applyCtx, &s.wg, cfg.Logger, cfg); err != nil {
		return restartRequired, err
	}

	return restartRequired, nil
}
