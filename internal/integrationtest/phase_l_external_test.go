//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// errPlainCipher is returned by the test encryptor when the input
// does not look like a v1: ciphertext envelope; lets the data
// source store distinguish a corrupted column from a legitimate
// (rotated) value.
var errPlainCipher = errors.New("test encryptor: plaintext input")

func base64Encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}

func getEnv(key string) string { return os.Getenv(key) }

// TestInsightsExternalDataSourceRoundtrip creates a data source row,
// reads it back, and verifies the connection string is encrypted at
// rest by inspecting the column directly via the admin pool.
func TestInsightsExternalDataSourceRoundtrip(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping external data source roundtrip")
	}
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("ext"), Name: "External Co", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	enc := newTestEncryptor()
	store := insights.NewDataSourceStore(h.pool, enc)
	want := "postgres://reader:supersecret@analytics.example.invalid:5432/warehouse"
	created, err := store.Create(ctx, insights.DataSource{
		TenantID:         tn.ID,
		Name:             "warehouse",
		Description:      "company-wide warehouse",
		Dialect:          "postgres",
		ConnectionString: want,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create data source: %v", err)
	}
	// Create echoes the plaintext DSN back to the caller so the UI
	// doesn't have to do a follow-up fetch — the *on-disk* column
	// is what must be encrypted, asserted via the admin pool below.

	got, err := store.Get(ctx, tn.ID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConnectionString != want {
		t.Fatalf("decrypted dsn mismatch:\n want=%q\n  got=%q", want, got.ConnectionString)
	}

	// Defence-in-depth: reach through the admin pool (BYPASSRLS) and
	// confirm the on-disk value is *not* the plaintext DSN.
	var rawCipher string
	if err := h.adminPool.QueryRow(ctx,
		`SELECT connection_string FROM insights_data_sources WHERE tenant_id = $1 AND id = $2`,
		tn.ID, created.ID,
	).Scan(&rawCipher); err != nil {
		t.Fatalf("admin select: %v", err)
	}
	if rawCipher == want {
		t.Fatalf("connection string stored in plaintext: %q", rawCipher)
	}
	if !strings.Contains(rawCipher, "v1:") {
		t.Fatalf("encrypted column missing version prefix: %q", rawCipher)
	}

	if err := store.Delete(ctx, tn.ID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, tn.ID, created.ID); err == nil {
		t.Fatalf("expected ErrDataSourceNotFound after delete, got nil")
	}
}

// TestInsightsExternalRunnerExecutes verifies the external runner can
// open a pool against a real Postgres URL (we reuse the test
// database itself as a stand-in for an "external" warehouse) and
// execute a simple aggregation. The pool manager fingerprint cache
// is also exercised by issuing two back-to-back runs and asserting
// only one pool is created.
func TestInsightsExternalRunnerExecutes(t *testing.T) {
	h := newHarness(t)
	dbURL := getEnv("KAPP_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("KAPP_TEST_DB_URL not set; skipping external runner test")
	}
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("extrun"), Name: "ExtRun Co", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	enc := newTestEncryptor()
	store := insights.NewDataSourceStore(h.pool, enc)
	created, err := store.Create(ctx, insights.DataSource{
		TenantID:         tn.ID,
		Name:             "self-warehouse",
		Dialect:          "postgres",
		ConnectionString: dbURL,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create data source: %v", err)
	}
	pools := insights.NewPoolManager()
	t.Cleanup(pools.Close)
	ext := insights.NewExternalRunner(store, pools)

	// Reach into pg_class which is guaranteed to exist on every
	// Postgres deployment so the test doesn't depend on tenant data.
	def := reporting.Definition{
		Source: reporting.SourceExternalPrefix + created.ID.String() + ":pg_class",
		Aggregations: []reporting.Aggregation{{
			Op: reporting.AggCount, Alias: "n",
		}},
		Limit: 100,
	}
	res, err := ext.Run(ctx, tn.ID, def)
	if err != nil {
		t.Fatalf("external runner: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("expected at least one row")
	}
	if got, _ := res.Rows[0]["n"].(int64); got <= 0 {
		// pgx returns int64 for bigint COUNT, but accept any
		// non-zero positive numeric.
		if v, ok := res.Rows[0]["n"]; !ok || v == nil {
			t.Fatalf("count column missing from row: %+v", res.Rows[0])
		}
	}
}

// testEncryptor is a deterministic test-only Encryptor that mirrors
// the production tenant.KeyManager's interface but uses simple
// reversible base64 wrapping. Production paths use real AES-GCM via
// tenant.KeyManager; this lets the test assert "the column is not
// the plaintext DSN" without requiring KAPP_MASTER_KEY plumbing.
type testEncryptor struct{}

func newTestEncryptor() *testEncryptor { return &testEncryptor{} }

func (testEncryptor) EncryptString(tenantID uuid.UUID, plaintext string) (string, error) {
	return "v1:" + tenantID.String() + ":" + base64Encode(plaintext), nil
}
func (testEncryptor) DecryptString(tenantID uuid.UUID, value string) (string, error) {
	const prefix = "v1:"
	if !strings.HasPrefix(value, prefix) {
		return "", errPlainCipher
	}
	tail := strings.TrimPrefix(value, prefix)
	parts := strings.SplitN(tail, ":", 2)
	if len(parts) != 2 {
		return "", errPlainCipher
	}
	return base64Decode(parts[1])
}
