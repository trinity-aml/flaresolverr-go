package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	flaresolverr "github.com/trinity-aml/flaresolverr-go/server"
)

// shutdownTimeout bounds the whole graceful stop. It has to outlast a typical
// in-flight /v1 solve, otherwise http.Server.Shutdown returns early with a
// deadline error and the browser cleanup behind it gets less time than it
// needs to kill the driver processes.
const shutdownTimeout = 30 * time.Second

func main() {
	cfg, warnings := flaresolverr.LoadConfig()
	for _, warning := range warnings {
		log.Printf("config warning: %s", warning)
	}

	flag.StringVar(&cfg.Host, "host", cfg.Host, "server host")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "server port")
	flag.StringVar(&cfg.BrowserPath, "browser", cfg.BrowserPath, "path to Chrome/Chromium")
	flag.StringVar(&cfg.DriverPath, "driver", cfg.DriverPath, "path to ChromeDriver")
	flag.BoolVar(&cfg.Headless, "headless", cfg.Headless, "run Chrome in headless mode")
	flag.BoolVar(&cfg.DisableMedia, "disable-media", cfg.DisableMedia, "block images, styles and fonts")
	flag.BoolVar(&cfg.PrometheusEnabled, "prometheus", cfg.PrometheusEnabled, "enable Prometheus metrics exporter")
	flag.IntVar(&cfg.PrometheusPort, "prometheus-port", cfg.PrometheusPort, "Prometheus exporter port")
	flag.Parse()

	server, err := flaresolverr.NewServer(cfg)
	if err != nil {
		log.Fatalf("start server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ListenAndServe runs in the goroutine and shutdown runs in main, not the
	// other way around. Shutdown closes the listener, which makes
	// ListenAndServe return immediately — so if main were the one blocked on
	// ListenAndServe it would return and kill the process before Shutdown got
	// as far as service.Close(), leaking every live browser, driver process
	// and scratch dir on each SIGTERM.
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("flaresolverr-go listening on %s:%d", cfg.Host, cfg.Port)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			log.Println(err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown failed: %v", err)
		}
		if err := <-serveErr; err != nil {
			log.Println(err)
			os.Exit(1)
		}
	}
}
