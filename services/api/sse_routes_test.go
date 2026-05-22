package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// TestRegisterSSERoutes_MountsStreamRoute pins the contract that the
// dedicated SSE listener serves /api/v1/events/stream and rejects every
// other path with a 404 carrying the SSE-specific hint. The handler
// itself is exercised only as far as the auth chain — pollEvents is
// never reached because the request carries no Authorization header so
// tenantChain shortcircuits with 401. That is sufficient to prove the
// route is registered AND that the tenantChain mount is in effect.
func TestRegisterSSERoutes_MountsStreamRoute(t *testing.T) {
	d := newSSETestDeps()
	r := registerSSERoutes(d, platform.NewLogger(platform.LoggerConfig{}, discardWriter{}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/events/stream without auth: got status %d, want 401 (tenantChain reject); body=%q", rec.Code, rec.Body.String())
	}
}

// TestRegisterSSERoutes_RejectsNonSSEPaths pins the catch-all 404 that
// keeps the SSE listener visibly single-purpose. A curious operator
// hitting /api/v1/records on the SSE port should see a self-describing
// 404 rather than chi's default body.
func TestRegisterSSERoutes_RejectsNonSSEPaths(t *testing.T) {
	d := newSSETestDeps()
	r := registerSSERoutes(d, platform.NewLogger(platform.LoggerConfig{}, discardWriter{}))

	for _, path := range []string{"/api/v1/records", "/api/v1/forms", "/api/v1/insights/queries", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got status %d, want 404", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "this listener only serves SSE streams") {
			t.Fatalf("%s: 404 body missing SSE-listener hint: %q", path, rec.Body.String())
		}
	}
}

// TestRegisterRoutes_OmitsSSEWhenSSEAddrSet pins the inverse half of
// the split: when KAPP_SSE_ADDR is set the SSE block in registerRoutes
// is skipped so the main router does NOT carry /api/v1/events/stream.
// Walks the router via chi.Walk so the assertion is independent of the
// request-handling path.
func TestRegisterRoutes_OmitsSSEWhenSSEAddrSet(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sseAddr    string
		expectSSE  bool
	}{
		{name: "empty SSEAddr keeps SSE on main router", sseAddr: "", expectSSE: true},
		{name: "non-empty SSEAddr moves SSE off main router", sseAddr: ":8081", expectSSE: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			has := mainRouterMountsStreamRoute(t, tc.sseAddr)
			if has != tc.expectSSE {
				t.Fatalf("SSEAddr=%q: main router mounts /api/v1/events/stream = %v, want %v", tc.sseAddr, has, tc.expectSSE)
			}
		})
	}
}

// mainRouterMountsStreamRoute builds a minimal apiDeps for the supplied
// SSE address, calls the production mountEventStreamOnMainRouter helper
// against a fresh chi router, and reports whether /api/v1/events/stream
// was registered. The test deliberately calls the same function the
// production registerRoutes calls so any refactor of the mount block
// (predicate flip, route shape change, middleware reordering) is
// caught here rather than silently drifting from a duplicated copy of
// the logic.
//
// Building a full registerRoutes router would require wiring dozens
// of handler stores the SSE-mount decision has no semantic dependency
// on, so we exercise the extracted helper directly. Both paths share
// the single source of truth in services/api/sse_routes.go.
func mainRouterMountsStreamRoute(t *testing.T, sseAddr string) bool {
	t.Helper()
	d := newSSETestDeps()
	d.cfg.SSEAddr = sseAddr

	r := chi.NewRouter()
	mountEventStreamOnMainRouter(r, d)

	found := false
	walkErr := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && route == "/api/v1/events/stream" {
			found = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("chi.Walk: %v", walkErr)
	}
	return found
}

// newSSETestDeps builds the minimum apiDeps the SSE-router tests need.
// Handlers that would otherwise touch the DB or auth signer are stubbed
// with no-op closures; the actual handler bodies are exercised only as
// far as the auth chain rejection so a nil pool / nil signer never
// causes a panic in the request path the tests hit.
func newSSETestDeps() *apiDeps {
	return &apiDeps{
		cfg: &platform.Config{},
		eh:  &eventsHandlers{pool: nil},
		tenantChain: func(r chi.Router) {
			// Stand-in for the real tenantChain in deps_build.go.
			// Real implementation mounts auth.Middleware which 401s
			// every request that does not carry a JWT; we mimic that
			// shape so the test request flows through the same
			// rejection path.
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				})
			})
		},
		apiCallMW: func(next http.Handler) http.Handler {
			return next
		},
	}
}

// discardWriter satisfies io.Writer for slog handlers without
// polluting the test output. The router only writes when a request
// completes; the test rejects every request at the auth chain so the
// MetricsMiddleware writes one structured line per call.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
