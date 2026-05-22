package main

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// registerSSERoutes builds the chi router served by the dedicated
// Server-Sent-Events listener when KAPP_SSE_ADDR is set. It is the
// counterpart to registerRoutes: the main router serves every non-
// streaming route under DefaultHTTPTimeouts (Write=120s) while this
// router serves /api/v1/events/stream under LongStreamTimeouts
// (Write=0). The split exists so the SSE-shaped Write=0 surface is
// confined to a single port instead of widening every API route's
// slow-write attack surface.
//
// Middleware stack mirrors registerRoutes — RealIP, Recoverer,
// RequestIDMiddleware, and (when wired) MetricsMiddleware — so SSE
// connections carry the same request_id propagation, panic recovery,
// real-IP attribution, and Prometheus counter coverage as every
// other route. The tenantChain + apiCallMW pair matches the legacy
// single-listener mount inside registerRoutes; consumers of the SSE
// stream see the same auth + metering surface in either deployment
// mode.
//
// The /healthz probe is mounted so a load balancer health-checking
// the SSE port (without forwarding clients there) sees a 200 without
// having to traverse the auth chain. The probe shares its handler
// with the main router so the dependency on the database pool is
// identical.
func registerSSERoutes(d *apiDeps, logger *slog.Logger) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(platform.RequestIDMiddleware(logger))
	r.Use(platform.TracingMiddleware("kapp-api-sse"))
	if d.metrics != nil {
		r.Use(platform.MetricsMiddleware(d.metrics))
	}

	r.Get("/healthz", healthHandler(d.pool))

	// The auth chain + per-tenant metering mirror the legacy mount
	// inside registerRoutes (the if-block under "Phase F event
	// stream"). Splitting this off does NOT relax any defense —
	// every middleware that ran on the legacy route runs here too;
	// only the surrounding http.Server's timeout policy differs.
	r.Route("/api/v1/events", func(r chi.Router) {
		d.tenantChain(r)
		r.Use(d.apiCallMW)
		r.Get("/stream", d.eh.stream)
	})

	// Catch-all 404 so a curious operator hitting the SSE port at
	// /api/v1/records (or any other main-router path) gets a clean
	// "Not Found" instead of chi's default handler. This keeps the
	// SSE listener visibly single-purpose.
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this listener only serves SSE streams; see KAPP_SSE_ADDR", http.StatusNotFound)
	})

	return r
}
