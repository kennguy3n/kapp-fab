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

	// OpenTelemetry tracing init runs BEFORE buildDeps so the pgx
	// tracer installed by NewPool inside buildDeps sees the global
	// TracerProvider this call sets. When KAPP_OTEL_ENDPOINT is
	// unset, InitTracing installs a no-op provider so call sites can
	// emit spans unconditionally; the hot path is a nil-check per
	// query and ~0 wire cost.
	tracingShutdown, err := platform.InitTracing(ctx, platform.LoadTracingConfig("kapp-api", cfg.Env))
	if err != nil {
		return fmt.Errorf("api: init tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown", slog.String("err", err.Error()))
		}
	}()

	d, cleanup, err := buildDeps(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	r := registerRoutes(d, logger)

	// HTTP timeout policy depends on whether SSE is being served on a
	// dedicated port (KAPP_SSE_ADDR set) or co-mounted on the main
	// router (legacy single-listener mode).
	//
	// Single-listener mode (SSEAddr empty): the main router carries
	// /api/v1/events/stream so WriteTimeout MUST be 0 — a non-zero
	// value is a hard per-connection kill that fires from end of
	// request headers, not idle time, and would terminate every SSE
	// subscription after at most that window. LongStreamTimeouts
	// gives us ReadHeader / Read / Idle / MaxHeaderBytes intact
	// while keeping Write=0. The slow-write surface across every
	// non-streaming route is bounded by chi's middleware.Timeout
	// (mounted per route group in registerRoutes) and TCP back-
	// pressure on the socket buffer.
	//
	// Dual-listener mode (SSEAddr set): SSE moves off the main
	// router (skipped by registerRoutes) onto its own http.Server
	// below, leaving the main router with no streaming endpoints.
	// We therefore adopt DefaultHTTPTimeouts (Write=120s) so the
	// strict slow-write defense applies to every other API route.
	var mainTimeouts HTTPTimeouts
	if cfg.SSEAddr != "" {
		mainTimeouts = platform.LoadHTTPTimeouts(platform.DefaultHTTPTimeouts())
	} else {
		mainTimeouts = platform.LoadHTTPTimeouts(platform.LongStreamTimeouts())
	}
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: r,
	}
	mainTimeouts.Apply(srv)

	// Optional dedicated SSE listener. When KAPP_SSE_ADDR is set the
	// /api/v1/events/stream endpoint is served from this dedicated
	// http.Server under LongStreamTimeouts (Write=0) so the main
	// API listener can adopt the strict 120s WriteTimeout above
	// without killing client subscriptions. When unset, this block
	// is skipped and SSE stays co-mounted on the main router.
	var sseSrv *http.Server
	if cfg.SSEAddr != "" {
		sseRouter := registerSSERoutes(d, logger)
		// SSE listener uses its own KAPP_SSE_* env namespace so an
		// operator tuning KAPP_HTTP_WRITE_TIMEOUT for the main API
		// listener does not inadvertently kill SSE streams. The
		// LongStreamTimeouts() base keeps Write=0 unless
		// KAPP_SSE_WRITE_TIMEOUT is explicitly set.
		sseTimeouts := platform.LoadHTTPTimeoutsWithPrefix("KAPP_SSE", platform.LongStreamTimeouts())
		sseSrv = &http.Server{
			Addr:    cfg.SSEAddr,
			Handler: sseRouter,
		}
		sseTimeouts.Apply(sseSrv)
		go func() {
			logger.Info("sse listening", slog.String("addr", cfg.SSEAddr))
			if err := sseSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("sse listener", slog.String("err", err.Error()))
			}
		}()
	}

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
		// cycles with tiny headers; MetricsHTTPTimeouts uses tighter
		// values than the user-facing main server. The metrics
		// listener uses its own KAPP_METRICS_* env namespace so an
		// operator tuning KAPP_HTTP_WRITE_TIMEOUT for the main API
		// listener cannot inadvertently slow down the Prometheus
		// scrape path (or vice versa).
		metricsTimeouts := platform.LoadHTTPTimeoutsWithPrefix("KAPP_METRICS", platform.MetricsHTTPTimeouts())
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
	if sseSrv != nil {
		if err := sseSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("sse shutdown", slog.String("err", err.Error()))
		}
	}
	logger.Info("shutdown complete")
	return nil
}

// HTTPTimeouts is a local alias used by the conditional in run() so
// the switch between Default and LongStream policies is a single var
// declaration rather than a generic interface. Keeping the alias
// inside the package avoids leaking an api-binary-only type into
// internal/platform.
type HTTPTimeouts = platform.HTTPTimeouts
