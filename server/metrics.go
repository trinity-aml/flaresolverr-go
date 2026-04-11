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
	enabled bool
	server  *http.Server

	requestCounter  *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

func newMetricsRegistry(cfg Config) (*metricsRegistry, error) {
	metrics := &metricsRegistry{enabled: cfg.PrometheusEnabled}
	if !cfg.PrometheusEnabled {
		return metrics, nil
	}

	reg := prometheus.NewRegistry()
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

	metrics.server = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", cfg.PrometheusPort),
		Handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
	}
	return metrics, nil
}

func (m *metricsRegistry) Observe(req *V1Request, res *V1Response) {
	if !m.enabled || res == nil || res.StartTimestamp == 0 || res.EndTimestamp == 0 {
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
	if !m.enabled || m.server == nil {
		return
	}
	logger.Info("serving prometheus exporter", "addr", m.server.Addr)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("prometheus server failed", "err", err)
		}
	}()
}

func (m *metricsRegistry) Shutdown(ctx context.Context) error {
	if !m.enabled || m.server == nil {
		return nil
	}
	return m.server.Shutdown(ctx)
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
