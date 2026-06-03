package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// noopTenantLookup satisfies TenantLookup. The fail-closed tests never
// reach a lookup (production short-circuits with 403; the dev case
// returns 400 on the missing header before any lookup), so both
// methods are unreachable stubs.
type noopTenantLookup struct{}

func (noopTenantLookup) Get(context.Context, uuid.UUID) (*tenant.Tenant, error) {
	return nil, tenant.ErrNotFound
}
func (noopTenantLookup) GetBySlug(context.Context, string) (*tenant.Tenant, error) {
	return nil, tenant.ErrNotFound
}

// TestLoadConfig_SecureDefaults locks in the Workstream 2 flip: with
// only DB_URL set (the minimal dev boot) the security-critical toggles
// default to their SECURE posture — authz enforced, JWT required —
// while RequireRedis stays false so a local boot works without Redis.
func TestLoadConfig_SecureDefaults(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_ENV", "")
	t.Setenv("KAPP_AUTHZ_ENFORCE", "")
	t.Setenv("KAPP_REQUIRE_JWT", "")
	t.Setenv("KAPP_REQUIRE_REDIS", "")
	t.Setenv("REDIS_URL", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AuthzEnforce {
		t.Error("AuthzEnforce defaulted false; want true (secure default)")
	}
	if !cfg.RequireJWT {
		t.Error("RequireJWT defaulted false; want true (secure default)")
	}
	if cfg.RequireRedis {
		t.Error("RequireRedis defaulted true in dev; want false so local boots without Redis")
	}
	if cfg.CSPHeader != DefaultCSPHeader {
		t.Errorf("CSPHeader = %q, want DefaultCSPHeader", cfg.CSPHeader)
	}
	if !cfg.IsDevelopment() || cfg.IsProduction() {
		t.Errorf("posture wrong for empty KAPP_ENV: dev=%v prod=%v", cfg.IsDevelopment(), cfg.IsProduction())
	}
}

// TestLoadConfig_RequireRedisProductionDefault verifies the posture-
// dependent RequireRedis default: true in a non-dev environment so a
// production deploy cannot silently fall back to per-pod limiting, but
// still overridable by an explicit KAPP_REQUIRE_REDIS=0.
func TestLoadConfig_RequireRedisProductionDefault(t *testing.T) {
	// staging is non-dev but not production, so the production secret
	// gate does not fire — this isolates the RequireRedis default.
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_ENV", "staging")
	t.Setenv("KAPP_REQUIRE_REDIS", "")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.RequireRedis {
		t.Error("RequireRedis defaulted false in staging; want true (non-dev hardening)")
	}

	// Explicit opt-out still wins.
	t.Setenv("KAPP_REQUIRE_REDIS", "0")
	t.Setenv("REDIS_URL", "")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with explicit opt-out: %v", err)
	}
	if cfg.RequireRedis {
		t.Error("KAPP_REQUIRE_REDIS=0 did not override the non-dev default")
	}
}

// TestLoadConfig_ProductionMissingSecretsListsAll is the core fail-
// closed test: KAPP_ENV=production with none of the critical secrets
// set must fail the boot with a SINGLE error naming EVERY missing
// variable (not just the first), so an operator fixes the whole set in
// one pass.
func TestLoadConfig_ProductionMissingSecretsListsAll(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_ENV", "production")
	t.Setenv("KAPP_JWT_SECRET", "")
	t.Setenv("KAPP_MASTER_KEY", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("KAPP_SECRET_PROVIDER", "")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("production boot with no secrets succeeded; want fatal error")
	}
	for _, want := range []string{"KAPP_JWT_SECRET", "KAPP_MASTER_KEY", "REDIS_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("production error %q does not mention %q (must list ALL missing vars)", err.Error(), want)
		}
	}
}

// TestLoadConfig_ProductionAllSecretsPresent verifies the gate passes
// once every critical secret is set.
func TestLoadConfig_ProductionAllSecretsPresent(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_ENV", "production")
	t.Setenv("KAPP_JWT_SECRET", "a-sufficiently-long-production-jwt-secret-value")
	t.Setenv("KAPP_MASTER_KEY", "a-master-key-for-field-encryption-1234567890")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("production boot with all secrets failed: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction() false for KAPP_ENV=production")
	}
	if len(cfg.MissingProductionEnv()) != 0 {
		t.Errorf("MissingProductionEnv = %v, want empty", cfg.MissingProductionEnv())
	}
}

// TestMissingProductionEnv_NonEnvSecretBackendSkipsJWTSecret verifies
// that when the operator selects a non-env secrets backend the gate
// does not demand KAPP_JWT_SECRET (the secret legitimately lives in
// the external provider), while still requiring the master key and
// Redis.
func TestMissingProductionEnv_NonEnvSecretBackendSkipsJWTSecret(t *testing.T) {
	cfg := &Config{
		Env:              "production",
		SecretProvider:   "vault",
		JWTSecretPresent: false,
		MasterKeyPresent: true,
		RedisURL:         "redis://localhost:6379",
	}
	if missing := cfg.MissingProductionEnv(); len(missing) != 0 {
		t.Errorf("MissingProductionEnv = %v, want empty (vault backend supplies JWT secret)", missing)
	}
}

// TestSecurityHeadersMiddleware_SetsHeaders verifies the baseline
// hardening headers are present and that HSTS is omitted unless
// explicitly configured.
func TestSecurityHeadersMiddleware_SetsHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("defaults without HSTS", func(t *testing.T) {
		h := SecurityHeadersMiddleware(SecurityHeadersConfig{})(next)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
		res := rec.Result()
		if got := res.Header.Get(headerCSP); got != DefaultCSPHeader {
			t.Errorf("CSP = %q, want default", got)
		}
		if got := res.Header.Get(headerFrameOptions); got != DefaultFrameOptions {
			t.Errorf("X-Frame-Options = %q, want %q", got, DefaultFrameOptions)
		}
		if got := res.Header.Get(headerContentTypeOptions); got != DefaultContentTypeOptions {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, DefaultContentTypeOptions)
		}
		if got := res.Header.Get(headerHSTS); got != "" {
			t.Errorf("HSTS = %q, want empty (disabled outside production)", got)
		}
	})

	t.Run("production config emits HSTS and custom CSP", func(t *testing.T) {
		cfg := &Config{Env: "production", CSPHeader: "default-src 'none'"}
		h := SecurityHeadersMiddleware(SecurityHeadersConfigFromConfig(cfg))(next)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
		res := rec.Result()
		if got := res.Header.Get(headerCSP); got != "default-src 'none'" {
			t.Errorf("CSP = %q, want operator override", got)
		}
		if got := res.Header.Get(headerHSTS); got != DefaultHSTS {
			t.Errorf("HSTS = %q, want %q in production", got, DefaultHSTS)
		}
	})
}

// TestRedisRateLimiter_FailClosedWhenBackendDown is the rate-limiter
// fail-closed test. We point a limiter at a miniredis, then Close the
// miniredis to simulate an outage. In fail-closed mode the limiter
// reports RateUnavailable (the middleware turns this into 503); in the
// default fail-open mode it reports RateAllowed.
func TestRedisRateLimiter_FailClosedWhenBackendDown(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	lim, err := NewRedisRateLimiter(ctx, "redis://"+mr.Addr(), RateLimitConfig{
		RequestsPerMinute: 60,
		BurstSize:         3,
		IdleTimeout:       time.Minute,
	})
	if err != nil {
		t.Fatalf("new redis rate limiter: %v", err)
	}
	t.Cleanup(func() { _ = lim.Close() })
	tenantID := uuid.New()

	// Healthy backend: within budget → allowed.
	if got := lim.Check(ctx, tenantID, 60, 3); got != RateAllowed {
		t.Fatalf("healthy backend Check = %v, want RateAllowed", got)
	}

	// Simulate the outage.
	mr.Close()

	t.Run("fail-open returns allowed", func(t *testing.T) {
		lim.WithFailClosed(false)
		if got := lim.Check(ctx, tenantID, 60, 3); got != RateAllowed {
			t.Errorf("fail-open Check on outage = %v, want RateAllowed", got)
		}
	})
	t.Run("fail-closed returns unavailable", func(t *testing.T) {
		lim.WithFailClosed(true)
		if got := lim.Check(ctx, tenantID, 60, 3); got != RateUnavailable {
			t.Errorf("fail-closed Check on outage = %v, want RateUnavailable", got)
		}
	})
}

// TestTenantMiddleware_ProductionRejectsHeaderPath verifies the
// X-Tenant-ID header fallback is failed CLOSED in production: every
// request is refused with 403 regardless of the header, while
// development keeps honouring it.
func TestTenantMiddleware_ProductionRejectsHeaderPath(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	t.Run("production rejects with 403", func(t *testing.T) {
		// Posture is captured when TenantMiddleware is constructed, so
		// build it AFTER setting the env to mirror a production boot.
		t.Setenv("KAPP_ENV", "production")
		mw := TenantMiddleware(noopTenantLookup{})
		reached = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		mw(next).ServeHTTP(rec, req)
		if rec.Result().StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403 in production", rec.Result().StatusCode)
		}
		if reached {
			t.Error("handler was reached in production; header path must be failed closed")
		}
	})

	t.Run("development still requires the header", func(t *testing.T) {
		t.Setenv("KAPP_ENV", "development")
		mw := TenantMiddleware(noopTenantLookup{})
		reached = false
		rec := httptest.NewRecorder()
		// No header → 400 (the legacy dev behaviour), proving the
		// middleware did NOT short-circuit on the production path.
		mw(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
		if rec.Result().StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (header required) in development", rec.Result().StatusCode)
		}
	})
}
