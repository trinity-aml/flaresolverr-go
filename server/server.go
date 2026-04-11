package flaresolverr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

type Server struct {
	cfg        Config
	service    *Service
	httpServer *http.Server
	metrics    *metricsRegistry
	startOnce  sync.Once
	wg         sync.WaitGroup
}

func NewServer(cfg Config) *Server {
	cfg = cfg.withDefaults()
	metrics, err := newMetricsRegistry(cfg)
	if err != nil {
		panic(err)
	}
	server := &Server{
		cfg:     cfg,
		service: NewService(cfg),
		metrics: metrics,
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
		s.metrics.Start(&s.wg, s.cfg.Logger)
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
		if req.Proxy == nil && s.cfg.DefaultProxy != nil {
			req.Proxy = s.cfg.DefaultProxy
		}

		res, status := s.service.ControllerV1(r.Context(), &req)
		writeJSON(w, status, res)
	})

	var handler http.Handler = mux
	handler = s.errorPlugin(handler)
	handler = s.prometheusPlugin(handler)
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
		s.cfg.Logger.Info("http request", "remote_addr", r.RemoteAddr, "method", r.Method, "url", r.URL.String(), "status", status)
	})
}

func (s *Server) errorPlugin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.cfg.Logger.Error("panic", "err", recovered)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": fmt.Sprint(recovered),
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) prometheusPlugin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = requestWithV1Body(r)
		writer := &captureWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)

		if !s.cfg.PrometheusEnabled || r.URL.Path != "/v1" {
			return
		}

		var res V1Response
		if err := json.Unmarshal(writer.body.Bytes(), &res); err != nil {
			s.cfg.Logger.Warn("error exporting metrics", "err", err)
			return
		}
		s.metrics.Observe(v1RequestFromContext(r.Context()), &res)
	})
}
