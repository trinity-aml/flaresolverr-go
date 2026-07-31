package flaresolverr

import (
	"context"
	"net"
	"sync"
	"testing"
)

func TestPrometheusResult(t *testing.T) {
	tests := []struct {
		message, want string
	}{
		{"Challenge solved!", "solved"},
		{"Challenge not detected!", "not_detected"},
		{"Error solving the challenge. Timeout after 60 seconds.", "error"},
		{"Error", "error"},
		{"", "unknown"},
		{"Session created successfully.", "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.message, func(t *testing.T) {
			if got := prometheusResult(tc.message); got != tc.want {
				t.Errorf("prometheusResult(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

func TestParseDomainURL(t *testing.T) {
	tests := []struct {
		raw, want string
	}{
		{"https://example.com/path?q=1", "example.com"},
		{"https://sub.example.com:8443/", "sub.example.com"},
		{"http://127.0.0.1:8191/", "127.0.0.1"},
		{"not a url", ""},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			if got := parseDomainURL(tc.raw); got != tc.want {
				t.Errorf("parseDomainURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestObserveIgnoresIncompleteResponses(t *testing.T) {
	metrics, err := newMetricsRegistry(Config{PrometheusEnabled: true, PrometheusPort: 0})
	if err != nil {
		t.Fatalf("newMetricsRegistry: %v", err)
	}

	// None of these must panic or record anything.
	metrics.Observe(nil, nil)
	metrics.Observe(&V1Request{URL: "https://example.com"}, &V1Response{})
	metrics.Observe(&V1Request{URL: "https://example.com"}, &V1Response{StartTimestamp: 1})
}

func TestObserveIsANoOpWhenDisabled(t *testing.T) {
	metrics, err := newMetricsRegistry(Config{PrometheusEnabled: false, PrometheusPort: 9999})
	if err != nil {
		t.Fatalf("newMetricsRegistry: %v", err)
	}

	metrics.Observe(
		&V1Request{URL: "https://example.com"},
		&V1Response{Message: "Challenge solved!", StartTimestamp: 1000, EndTimestamp: 2000},
	)
}

// A failed exporter restart must not leave the registry believing a server is
// running — that used to disable metrics permanently until a process restart.
func TestMetricsApplyConfigRecoversAfterAFailedRestart(t *testing.T) {
	metrics, err := newMetricsRegistry(Config{PrometheusEnabled: false, PrometheusPort: 19301})
	if err != nil {
		t.Fatalf("newMetricsRegistry: %v", err)
	}

	var wg sync.WaitGroup
	logger := PrepareConfig(Config{LogLevel: "error"}).Logger
	defer func() {
		_ = metrics.Shutdown(context.Background())
		wg.Wait()
	}()

	port := freePortForTest(t)
	cfg := Config{PrometheusEnabled: true, PrometheusPort: port}
	if err := metrics.ApplyConfig(context.Background(), &wg, logger, cfg); err != nil {
		t.Fatalf("enable exporter: %v", err)
	}

	metrics.mu.RLock()
	server := metrics.server
	metrics.mu.RUnlock()
	if server == nil {
		t.Fatal("expected a server to be published after a successful start")
	}

	// Re-applying the same config must be a no-op, not a restart.
	if err := metrics.ApplyConfig(context.Background(), &wg, logger, cfg); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	metrics.mu.RLock()
	same := metrics.server == server
	metrics.mu.RUnlock()
	if !same {
		t.Error("an unchanged config must not restart the exporter")
	}

	// Move to a new port: the old server is shut down and a new one published.
	newPort := freePortForTest(t)
	if err := metrics.ApplyConfig(context.Background(), &wg, logger, Config{PrometheusEnabled: true, PrometheusPort: newPort}); err != nil {
		t.Fatalf("move port: %v", err)
	}
	metrics.mu.RLock()
	moved := metrics.server != nil && metrics.server != server
	metrics.mu.RUnlock()
	if !moved {
		t.Error("expected a fresh server after a port change")
	}
}

func TestMetricsShutdownIsIdempotent(t *testing.T) {
	metrics, err := newMetricsRegistry(Config{PrometheusEnabled: false, PrometheusPort: 19302})
	if err != nil {
		t.Fatalf("newMetricsRegistry: %v", err)
	}

	if err := metrics.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown with no server running: %v", err)
	}
	if err := metrics.Shutdown(context.Background()); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
}

func freePortForTest(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
