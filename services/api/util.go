package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// healthHandler is the /healthz probe used by Kubernetes, load
// balancers, and CI smoke tests. It returns 200 when the database
// pool can be pinged within 2s and 503 otherwise. The 2s timeout is
// short enough that a stuck connection cannot mask a real outage but
// long enough to survive a transient TCP blip during failover.
func healthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// rootHandler answers the bare /api/v1/ probe used to confirm the
// service is reachable without touching the database. Useful for
// load balancer health checks that are configured separately from
// the deeper /healthz probe.
func rootHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON encodes body as JSON with the supplied status code. It
// is the single point at which the API surface emits a JSON
// envelope; centralising it here keeps the Content-Type header
// consistent across every handler and avoids per-handler
// boilerplate.
func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// tenantCountryResolver adapts *tenant.PGStore to the
// hr.CountryResolver shape so the payroll engine can fetch a
// tenant's ISO 3166-1 alpha-2 country code without importing the
// tenant package directly. Lookup failures collapse to "" + nil
// because the engine treats both as "no statutory pack" and we'd
// rather fail-soft a slip than block payroll on a control-plane
// hiccup.
func tenantCountryResolver(svc *tenant.PGStore) hr.CountryResolver {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		t, err := svc.Get(ctx, tenantID)
		if err != nil {
			return "", nil
		}
		return t.Country, nil
	}
}
