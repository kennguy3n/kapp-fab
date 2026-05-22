// Command api is the Kapp HTTP gateway / BFF. It exposes REST endpoints for
// KType and KRecord operations, health probes, and (future) event streaming.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("api: %v", err)
	}
}

func run() error {
	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	// Install the structured logger BEFORE buildDeps so any not-yet-
	// migrated log.Printf call sites inside dependency wiring (pool
	// open, NATS connect, KType registration) flow through the same
	// pipeline as request-scoped lines. slog.Default() is also
	// updated so package-level code that does slog.Info(...) ends up
	// in the right place.
	logger := platform.NewLogger(platform.LoggerConfig{
		Format:  cfg.LogFormat,
		Level:   cfg.LogLevel,
		Service: "api",
		Env:     cfg.Env,
	}, os.Stderr)
	platform.InstallDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, cleanup, err := buildDeps(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	r := registerRoutes(d, logger)

	// /api/v1/events/stream is an SSE endpoint that holds the
	// response open for the lifetime of the client subscription.
	// http.Server.WriteTimeout is a hard, per-connection kill — it
	// fires from end of request headers, not idle time — so any
	// non-zero value would terminate every SSE stream after at most
	// that window. We therefore use LongStreamTimeouts for the main
	// API server which sets Write=0 while keeping every other
	// defense (ReadHeader / Read / Idle / MaxHeaderBytes) in force.
	// Slow-write attacks on non-streaming routes are bounded by
	// chi's middleware.Timeout (mounted in registerRoutes) and the
	// TCP socket buffer back-pressure.
	mainTimeouts := platform.LoadHTTPTimeouts(platform.LongStreamTimeouts())
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: r,
	}
	mainTimeouts.Apply(srv)

	// Optional dedicated /metrics listener. Production deployments
	// SHOULD set KAPP_METRICS_ADDR (e.g. ":9090") so the Prometheus
	// scraper can hit /metrics without contending with user-facing
	// HTTP latency or going through the auth chain. When unset, the
	// metrics handler stays mounted inside the main router (legacy
	// behaviour) so local dev keeps working unchanged.
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" && d.metrics != nil {
		metricsMux := http.NewServeMux()
		metricsMux.HandleFunc("/metrics", d.metrics.Handler())
		metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ok")
		})
		// Metrics scrape connections are short request-response
		// cycles with tiny headers; MetricsHTTPTimeouts uses
		// tighter values than the user-facing main server.
		metricsTimeouts := platform.LoadHTTPTimeouts(platform.MetricsHTTPTimeouts())
		metricsSrv = &http.Server{
			Addr:    cfg.MetricsAddr,
			Handler: metricsMux,
		}
		metricsTimeouts.Apply(metricsSrv)
		go func() {
			logger.Info("metrics listening", slog.String("addr", cfg.MetricsAddr))
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics listener", slog.String("err", err.Error()))
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", cfg.ListenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics shutdown", slog.String("err", err.Error()))
		}
	}
	logger.Info("shutdown complete")
	return nil
}
