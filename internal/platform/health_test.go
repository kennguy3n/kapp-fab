package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dummyPool returns a pool whose config is valid but points at an
// unreachable host. pgxpool.New is lazy (MinConns=0), so no
// connection is attempted until a query runs — which lets tests
// exercise non-DB code paths (config defaults, probe selection,
// aggregation) without a live Postgres.
func dummyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("dummy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestNewHealthChecker_RequiresPool(t *testing.T) {
	t.Parallel()
	if _, err := NewHealthChecker(HealthConfig{}); err == nil {
		t.Fatal("expected error when Pool is nil")
	}
}

func TestNewHealthChecker_Defaults(t *testing.T) {
	t.Parallel()
	hc, err := NewHealthChecker(HealthConfig{Pool: dummyPool(t)})
	if err != nil {
		t.Fatalf("new checker: %v", err)
	}
	if hc.cfg.ProbeTimeout != 2*time.Second {
		t.Errorf("ProbeTimeout default = %v, want 2s", hc.cfg.ProbeTimeout)
	}
	if hc.cfg.OutboxBacklogWarn != 1000 || hc.cfg.OutboxBacklogCritical != 10000 {
		t.Errorf("outbox defaults = %d/%d", hc.cfg.OutboxBacklogWarn, hc.cfg.OutboxBacklogCritical)
	}
	if hc.cfg.HTTPClient == nil || hc.cfg.now == nil {
		t.Error("expected HTTPClient and now to be defaulted")
	}
}

func TestProbes_SelectionMirrorsConfig(t *testing.T) {
	t.Parallel()
	pool := dummyPool(t)

	// Only Pool → just the postgres probe.
	hc, _ := NewHealthChecker(HealthConfig{Pool: pool})
	if got := probeNames(hc); len(got) != 1 || got[0] != "postgres" {
		t.Fatalf("minimal probe set = %v, want [postgres]", got)
	}

	// Everything wired → every probe present.
	hc, _ = NewHealthChecker(HealthConfig{
		Pool:             pool,
		AdminPool:        pool,
		RedisURL:         "redis://localhost:6379",
		NATSURL:          "nats://localhost:4222",
		ZKFabricEndpoint: "http://localhost:8081",
	})
	want := map[string]bool{
		"postgres": true, "redis": true, "nats": true,
		"zk_object_fabric": true, "outbox": true, "worker": true,
	}
	got := probeNames(hc)
	if len(got) != len(want) {
		t.Fatalf("full probe set = %v, want %d entries", got, len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected probe %q", n)
		}
	}
}

func probeNames(hc *HealthChecker) []string {
	probes := hc.probes()
	names := make([]string, len(probes))
	for i, p := range probes {
		names[i] = p.name
	}
	return names
}

func TestAggregateStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		statuses []ComponentStatus
		critical []bool
		want     ComponentStatus
	}{
		{
			name:     "all operational",
			statuses: []ComponentStatus{StatusOperational, StatusOperational},
			critical: []bool{true, false},
			want:     StatusOperational,
		},
		{
			name:     "critical down → system down",
			statuses: []ComponentStatus{StatusDown, StatusOperational},
			critical: []bool{true, false},
			want:     StatusDown,
		},
		{
			name:     "non-critical down → only degraded",
			statuses: []ComponentStatus{StatusOperational, StatusDown},
			critical: []bool{true, false},
			want:     StatusDegraded,
		},
		{
			name:     "degraded component → degraded system",
			statuses: []ComponentStatus{StatusOperational, StatusDegraded},
			critical: []bool{true, false},
			want:     StatusDegraded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := make([]ComponentHealth, len(tc.statuses))
			for i, s := range tc.statuses {
				results[i] = ComponentHealth{Status: s}
			}
			if got := aggregateStatus(results, tc.critical); got != tc.want {
				t.Errorf("aggregateStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckZKFabric(t *testing.T) {
	t.Parallel()
	// Any HTTP response — even 404 — proves reachability.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	hc, _ := NewHealthChecker(HealthConfig{Pool: dummyPool(t), ZKFabricEndpoint: srv.URL})
	got := hc.checkZKFabric(context.Background())
	if got.Status != StatusOperational {
		t.Fatalf("status = %q, want operational (got err %q)", got.Status, got.Error)
	}
	if got.Detail["http_status"] != http.StatusNotFound {
		t.Errorf("http_status detail = %v, want 404", got.Detail["http_status"])
	}

	// Unreachable endpoint → down.
	hc, _ = NewHealthChecker(HealthConfig{
		Pool:             dummyPool(t),
		ZKFabricEndpoint: "http://127.0.0.1:1",
		ProbeTimeout:     500 * time.Millisecond,
	})
	got = hc.checkZKFabric(context.Background())
	if got.Status != StatusDown {
		t.Errorf("unreachable status = %q, want down", got.Status)
	}
}

func TestCheckRedis(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	hc, _ := NewHealthChecker(HealthConfig{Pool: dummyPool(t), RedisURL: "redis://" + mr.Addr()})
	if got := hc.checkRedis(context.Background()); got.Status != StatusOperational {
		t.Fatalf("redis status = %q, want operational (err %q)", got.Status, got.Error)
	}

	// Point at a closed port → down.
	mr.Close()
	if got := hc.checkRedis(context.Background()); got.Status != StatusDown {
		t.Errorf("closed-redis status = %q, want down", got.Status)
	}
}

// TestElapsedMS_UsesInjectableClock pins the latency helper to the
// checker's injectable clock. Under a faked clock the measured
// duration must be derived from h.cfg.now (here a deterministic 250ms
// jump), not the real wall clock — the old time.Since(start) version
// would report years, so this guards against that regression.
func TestElapsedMS_UsesInjectableClock(t *testing.T) {
	t.Parallel()
	hc, err := NewHealthChecker(HealthConfig{Pool: dummyPool(t)})
	if err != nil {
		t.Fatalf("new checker: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base
	hc.cfg.now = func() time.Time { return current }

	start := hc.cfg.now()
	current = base.Add(250 * time.Millisecond)
	if got := hc.elapsedMS(start); got != 250 {
		t.Errorf("elapsedMS = %v, want 250 (must measure via injectable clock)", got)
	}
}

func TestCheckNATS_Unreachable(t *testing.T) {
	t.Parallel()
	hc, _ := NewHealthChecker(HealthConfig{
		Pool:         dummyPool(t),
		NATSURL:      "nats://127.0.0.1:1",
		ProbeTimeout: 500 * time.Millisecond,
	})
	if got := hc.checkNATS(context.Background()); got.Status != StatusDown {
		t.Errorf("unreachable nats status = %q, want down", got.Status)
	}
}
