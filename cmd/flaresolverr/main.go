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

	server := flaresolverr.NewServer(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown failed: %v", err)
		}
	}()

	log.Printf("flaresolverr-go listening on %s:%d", cfg.Host, cfg.Port)
	if err := server.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
