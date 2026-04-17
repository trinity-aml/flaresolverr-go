package flaresolverr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metricsRegistry struct {
	mu      sync.RWMutex
	enabled bool
	port    int
	server  *http.Server
	handler http.Handler

	requestCounter  *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

func newMetricsRegistry(cfg Config) (*metricsRegistry, error) {
	reg := prometheus.NewRegistry()
	metrics := &metricsRegistry{
		enabled: cfg.PrometheusEnabled,
		port:    cfg.PrometheusPort,
	}
	metrics.requestCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flaresolverr_request",
		Help: "Total requests with result",
	}, []string{"domain", "result"})
	metrics.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "flaresolverr_request_duration",
		Help:    "Request duration in seconds",
		Buckets: []float64{0, 10, 25, 50},
	}, []string{"domain"})

	if err := reg.Register(metrics.requestCounter); err != nil {
		return nil, fmt.Errorf("register request counter: %w", err)
	}
	if err := reg.Register(metrics.requestDuration); err != nil {
		return nil, fmt.Errorf("register request duration: %w", err)
	}
	metrics.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return metrics, nil
}

func (m *metricsRegistry) Observe(req *V1Request, res *V1Response) {
	m.mu.RLock()
	enabled := m.enabled
	m.mu.RUnlock()
	if !enabled || res == nil || res.StartTimestamp == 0 || res.EndTimestamp == 0 {
		return
	}

	domain := "unknown"
	switch {
	case res.Solution != nil && res.Solution.URL != "":
		domain = parseDomainURL(res.Solution.URL)
	case req != nil && req.URL != "":
		domain = parseDomainURL(req.URL)
	}
	if domain == "" {
		domain = "unknown"
	}

	runTime := float64(res.EndTimestamp-res.StartTimestamp) / 1000
	m.requestDuration.WithLabelValues(domain).Observe(runTime)
	m.requestCounter.WithLabelValues(domain, prometheusResult(res.Message)).Inc()
}

func (m *metricsRegistry) Start(wg *sync.WaitGroup, logger Logger) {
	_ = m.ApplyConfig(context.Background(), wg, logger, Config{
		PrometheusEnabled: m.isEnabled(),
		PrometheusPort:    m.currentPort(),
	})
}

func (m *metricsRegistry) ApplyConfig(ctx context.Context, wg *sync.WaitGroup, logger Logger, cfg Config) error {
	var oldServer *http.Server
	var newServer *http.Server

	m.mu.Lock()
	portChanged := cfg.PrometheusPort != m.port
	enabledChanged := cfg.PrometheusEnabled != m.enabled
	if (portChanged || enabledChanged) && m.server != nil {
		oldServer = m.server
		m.server = nil
	}
	m.enabled = cfg.PrometheusEnabled
	m.port = cfg.PrometheusPort
	if cfg.PrometheusEnabled && m.server == nil {
		newServer = &http.Server{
			Addr:    fmt.Sprintf("0.0.0.0:%d", cfg.PrometheusPort),
			Handler: m.handler,
		}
		m.server = newServer
	}
	m.mu.Unlock()

	if oldServer != nil {
		if err := oldServer.Shutdown(ctx); err != nil {
			return err
		}
	}
	if newServer != nil {
		m.startServer(wg, logger, newServer)
	}
	return nil
}

func (m *metricsRegistry) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	server := m.server
	m.server = nil
	m.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

func (m *metricsRegistry) startServer(wg *sync.WaitGroup, logger Logger, server *http.Server) {
	logger.Info("serving prometheus exporter", "addr", server.Addr)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("prometheus server failed", "err", err)
		}
	}()
}

func (m *metricsRegistry) isEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled
}

func (m *metricsRegistry) currentPort() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.port
}

func prometheusResult(message string) string {
	switch {
	case message == "Challenge solved!":
		return "solved"
	case message == "Challenge not detected!":
		return "not_detected"
	case strings.HasPrefix(message, "Error"):
		return "error"
	default:
		return "unknown"
	}
}

func parseDomainURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "unknown"
	}
	return parsed.Hostname()
}
