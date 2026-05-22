package platform

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestMetricsMiddlewareNoChiContext exercises the exposed middleware
// outside a chi router so chi.RouteContext returns nil. Before the
// guard in MetricsMiddleware this panicked with a nil-pointer
// dereference the moment a scrape hit /metrics or /health; now the
// middleware must fall back to the raw URL path and still emit
// counters/histograms.
func TestMetricsMiddlewareNoChiContext(t *testing.T) {
	reg := NewMetricsRegistry()
	h := MetricsMiddleware(reg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req) // must not panic
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d", rr.Code, http.StatusOK)
	}
	body := scrape(t, reg)
	if !strings.Contains(body, `path="/metrics"`) {
		t.Fatalf("expected raw URL fallback path=/metrics in exposition; got:\n%s", body)
	}
	if !strings.Contains(body, `kapp_request_total`) {
		t.Fatalf("counter family missing from exposition; got:\n%s", body)
	}
}

// TestMetricsMiddlewareWithChiRouter is the counterpart that proves
// the normal-path branch still records the chi route pattern rather
// than the concrete URL — otherwise high-cardinality IDs in the path
// would balloon the series count.
func TestMetricsMiddlewareWithChiRouter(t *testing.T) {
	reg := NewMetricsRegistry()
	r := chi.NewRouter()
	r.Use(MetricsMiddleware(reg))
	r.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d", rr.Code, http.StatusOK)
	}
	body := scrape(t, reg)
	if !strings.Contains(body, `path="/users/{id}"`) {
		t.Fatalf("expected chi route pattern /users/{id} in exposition; got:\n%s", body)
	}
	if strings.Contains(body, `path="/users/42"`) {
		t.Fatalf("concrete URL leaked into labels; high-cardinality risk. got:\n%s", body)
	}
}

// TestMetricsMiddleware_404UsesUnmatchedSentinel pins the cardinality
// guard added alongside the TracingMiddleware chi-RoutePattern fix.
// Before the sentinel, an attacker hitting /api/v1/scanner-junk-
// <random-uuid> would walk the chi router unmatched, fall back to
// r.URL.Path, and emit one new Prometheus series per probe. The fix
// buckets every unmatched chi request under a single "<unmatched>"
// path label so the series count is bounded by (tenant × method ×
// status) and NOT by attacker creativity.
func TestMetricsMiddleware_404UsesUnmatchedSentinel(t *testing.T) {
	reg := NewMetricsRegistry()
	r := chi.NewRouter()
	r.Use(MetricsMiddleware(reg))
	r.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Probe a couple of attacker-shaped paths that chi will NOT
	// match (no route registered).
	for _, probe := range []string{
		"/api/v1/junk/abc-123-def-456",
		"/api/v1/junk/xyz-987",
		"/wp-admin/setup-config.php",
	} {
		req := httptest.NewRequest(http.MethodGet, probe, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s: status got %d, want %d (chi default 404)", probe, rr.Code, http.StatusNotFound)
		}
	}

	body := scrape(t, reg)
	if !strings.Contains(body, `path="<unmatched>"`) {
		t.Fatalf("expected unmatched-chi requests bucketed under path=\"<unmatched>\" sentinel; got:\n%s", body)
	}
	for _, leaked := range []string{
		`path="/api/v1/junk/abc-123-def-456"`,
		`path="/api/v1/junk/xyz-987"`,
		`path="/wp-admin/setup-config.php"`,
	} {
		if strings.Contains(body, leaked) {
			t.Fatalf("attacker-controlled URL leaked into Prometheus label %q; cardinality risk. body:\n%s", leaked, body)
		}
	}
}

func scrape(t *testing.T, reg *MetricsRegistry) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	reg.Handler()(rr, req)
	return rr.Body.String()
}
