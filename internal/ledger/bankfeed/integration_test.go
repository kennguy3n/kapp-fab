//go:build integration
// +build integration

// These tests exercise the DB-backed bankfeed stores against a live
// Postgres connected as the non-superuser kapp_app role, so RLS is
// enforced exactly as in production. They are gated behind the
// `integration` build tag and skip when KAPP_TEST_DB_URL is unset, the
// same convention as internal/integrationtest.
//
//	make test-integration            # runs the whole integration suite
//	KAPP_TEST_DB_URL=... go test -tags=integration ./internal/ledger/bankfeed/
package bankfeed

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("KAPP_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("KAPP_TEST_DB_URL not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := platform.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return pool
}

// seedTenantAndAccount creates a fresh tenant + bank account and returns
// their ids. Each call uses a unique slug so tests can run repeatedly
// against a shared DB.
func seedTenantAndAccount(t *testing.T, pool *pgxpool.Pool) (tenantID, bankAccountID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenants := tenant.NewPGStore(pool)
	tn, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "bf-" + uuid.NewString()[:8], Name: "BankFeed Test", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	ledgerStore := ledger.NewPGStore(pool, events.NewPGPublisher(pool), audit.NewPGLogger(pool))
	acct, err := ledgerStore.UpsertBankAccount(ctx, ledger.BankAccount{
		TenantID: tn.ID, Name: "Operating", AccountCode: "1000", Currency: "USD", Active: true,
	})
	if err != nil {
		t.Fatalf("create bank account: %v", err)
	}
	return tn.ID, acct.ID
}

func keyManager(t *testing.T) *tenant.KeyManager {
	t.Helper()
	// 32-byte test master key; never used outside tests.
	km, err := tenant.NewKeyManager([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	return km
}

func TestConnectionStoreCRUDEncryptsAndIsolates(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	tenantID, acctID := seedTenantAndAccount(t, pool)
	auditor := audit.NewPGLogger(pool)
	store := NewConnectionStore(pool, keyManager(t), auditor)

	conn, err := store.UpsertConnection(ctx, Connection{
		TenantID:      tenantID,
		BankAccountID: acctID,
		Provider:      ProviderPlaid,
		AccessToken:   "access-secret-123",
		RefreshToken:  "refresh-secret-456",
		ExternalID:    "item-1",
		Status:        StatusActive,
	})
	if err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}

	// Round-trip read decrypts transparently.
	got, err := store.GetConnection(ctx, tenantID, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.AccessToken != "access-secret-123" || got.RefreshToken != "refresh-secret-456" {
		t.Fatalf("decrypted tokens mismatch: %+v", got)
	}

	// Ciphertext at rest must not equal the plaintext. The GUC set by
	// set_config(..., is_local=true) only persists inside a transaction,
	// so the raw read runs in its own tx with the tenant context set.
	var rawAccess []byte
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		t.Fatalf("set tenant guc: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT access_token_enc FROM bank_feed_connections WHERE tenant_id=$1 AND id=$2`,
		tenantID, conn.ID).Scan(&rawAccess); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if string(rawAccess) == "access-secret-123" {
		t.Fatal("access token stored in plaintext at rest")
	}
	_ = tx.Rollback(ctx)

	// Cursor + last_sync advance.
	syncedAt := time.Now().UTC().Truncate(time.Second)
	if err := store.AdvanceCursor(ctx, tenantID, conn.ID, "cursor-9", syncedAt); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	after, _ := store.GetConnection(ctx, tenantID, conn.ID)
	if after.Cursor != "cursor-9" || after.LastSyncAt == nil {
		t.Fatalf("cursor/last_sync not advanced: %+v", after)
	}

	// A credential refresh via UpsertConnection (LastSyncAt left nil) must
	// NOT clobber the established sync position: COALESCE preserves it.
	refreshed, err := store.UpsertConnection(ctx, Connection{
		TenantID:      tenantID,
		ID:            conn.ID,
		BankAccountID: acctID,
		Provider:      ProviderPlaid,
		AccessToken:   "access-secret-rotated",
		ExternalID:    "item-1",
		Status:        StatusActive,
		// LastSyncAt intentionally nil (caller updated credentials only).
	})
	if err != nil {
		t.Fatalf("UpsertConnection (refresh): %v", err)
	}
	if refreshed.LastSyncAt == nil || !refreshed.LastSyncAt.Equal(*after.LastSyncAt) {
		t.Fatalf("last_sync_at not preserved across credential refresh: got %v want %v",
			refreshed.LastSyncAt, after.LastSyncAt)
	}
	if reread, _ := store.GetConnection(ctx, tenantID, conn.ID); reread.LastSyncAt == nil {
		t.Fatal("last_sync_at nulled at rest after credential refresh")
	}

	// Active listing includes it; MarkError + SetStatus drive lifecycle.
	active, err := store.ListActiveConnections(ctx, tenantID)
	if err != nil || len(active) != 1 {
		t.Fatalf("ListActiveConnections = %v, %v; want 1", active, err)
	}
	if err := store.MarkError(ctx, tenantID, conn.ID, "token expired"); err != nil {
		t.Fatalf("MarkError: %v", err)
	}
	if err := store.SetStatus(ctx, tenantID, conn.ID, StatusRevoked); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	active, _ = store.ListActiveConnections(ctx, tenantID)
	if len(active) != 0 {
		t.Fatalf("revoked connection still active: %v", active)
	}

	// RLS isolation: a different tenant cannot see this connection.
	otherTenant, _ := seedTenantAndAccount(t, pool)
	if _, err := store.GetConnection(ctx, otherTenant, conn.ID); err == nil {
		t.Fatal("cross-tenant GetConnection should fail under RLS")
	}
}

// TestCSVConnectionDedupesUnderConcurrentCreate proves the deterministic
// CSV id closes the ensureCSVConnection TOCTOU race: two UpsertConnection
// calls with a nil id for the same (tenant, account) CSV feed — the shape
// two concurrent first-time statement uploads produce — must converge on
// a single row rather than inserting two.
func TestCSVConnectionDedupesUnderConcurrentCreate(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	tenantID, acctID := seedTenantAndAccount(t, pool)
	store := NewConnectionStore(pool, keyManager(t), audit.NewPGLogger(pool))

	first, err := store.UpsertConnection(ctx, Connection{
		TenantID: tenantID, BankAccountID: acctID, Provider: ProviderCSV, Status: StatusActive,
	})
	if err != nil {
		t.Fatalf("first CSV upsert: %v", err)
	}
	second, err := store.UpsertConnection(ctx, Connection{
		TenantID: tenantID, BankAccountID: acctID, Provider: ProviderCSV, Status: StatusActive,
	})
	if err != nil {
		t.Fatalf("second CSV upsert: %v", err)
	}
	if first.ID != second.ID || first.ID != CSVConnectionID(tenantID, acctID) {
		t.Fatalf("CSV ids diverged: %s vs %s (want deterministic %s)",
			first.ID, second.ID, CSVConnectionID(tenantID, acctID))
	}
	conns, err := store.ListConnectionsByAccount(ctx, tenantID, acctID)
	if err != nil {
		t.Fatalf("ListConnectionsByAccount: %v", err)
	}
	csvCount := 0
	for _, c := range conns {
		if c.Provider == ProviderCSV {
			csvCount++
		}
	}
	if csvCount != 1 {
		t.Fatalf("got %d CSV connections for the account; want exactly 1", csvCount)
	}
}

func TestRuleStoreCRUDAndOrdering(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	tenantID, acctID := seedTenantAndAccount(t, pool)
	store := NewRuleStore(pool, audit.NewPGLogger(pool))

	// Two tenant-wide rules + one account-scoped, inserted out of order.
	_, err := store.UpsertRule(ctx, Rule{
		TenantID: tenantID, Priority: 20, ConditionType: CondDescriptionContains,
		ConditionValue: "aws", TargetAccountCode: "6000", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	high, err := store.UpsertRule(ctx, Rule{
		TenantID: tenantID, Priority: 10, ConditionType: CondDescriptionContains,
		ConditionValue: "uber", TargetAccountCode: "6100", AutoApprove: true, Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	_, err = store.UpsertRule(ctx, Rule{
		TenantID: tenantID, BankAccountID: &acctID, Priority: 5,
		ConditionType: CondCounterparty, ConditionValue: "acme", TargetAccountCode: "6200", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertRule (account-scoped): %v", err)
	}

	// ListRules for the account returns account-scoped + tenant-wide,
	// priority-ordered ascending.
	rules, err := store.ListRules(ctx, tenantID, &acctID)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules; want 3", len(rules))
	}
	if rules[0].Priority > rules[1].Priority || rules[1].Priority > rules[2].Priority {
		t.Fatalf("rules not priority-ordered: %+v", rules)
	}

	// Delete the high-priority rule.
	if err := store.DeleteRule(ctx, tenantID, high.ID); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	rules, _ = store.ListRules(ctx, tenantID, &acctID)
	if len(rules) != 2 {
		t.Fatalf("after delete got %d; want 2", len(rules))
	}

	// RLS: another tenant sees none of these rules.
	other, _ := seedTenantAndAccount(t, pool)
	otherRules, _ := store.ListAllRules(ctx, other)
	if len(otherRules) != 0 {
		t.Fatalf("cross-tenant rules leaked: %v", otherRules)
	}
}

func TestSmartMatcherEndToEnd(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	tenantID, acctID := seedTenantAndAccount(t, pool)
	ledgerStore := ledger.NewPGStore(pool, events.NewPGPublisher(pool), audit.NewPGLogger(pool))

	// Ingest a feed line via the idempotent sync path.
	vd := time.Now().UTC().Truncate(24 * time.Hour)
	lines := []ledger.BankTransaction{{
		BankAccountID: acctID, ValueDate: vd, Description: "ACME CORP",
		Amount: decimal.RequireFromString("-100.00"), Currency: "USD", ExternalRef: "ext-1",
	}}
	inserted, err := ledgerStore.SyncBankTransactions(ctx, tenantID, acctID, lines)
	if err != nil {
		t.Fatalf("SyncBankTransactions: %v", err)
	}
	if len(inserted) != 1 {
		t.Fatalf("inserted %d; want 1", len(inserted))
	}

	// Re-sync the same external_ref is a no-op (idempotency index).
	again, err := ledgerStore.SyncBankTransactions(ctx, tenantID, acctID, lines)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-sync inserted %d; want 0 (deduped)", len(again))
	}

	// With no journal entries there are no suggestions, but the call must
	// succeed and persist nothing.
	matcher := ledger.NewSmartMatcher(ledgerStore)
	sugs, err := matcher.SuggestMatches(ctx, tenantID, inserted[0].ID, ledger.MatchOptions{})
	if err != nil {
		t.Fatalf("SuggestMatches: %v", err)
	}
	if len(sugs) != 0 {
		t.Fatalf("got %d suggestions with no candidates; want 0", len(sugs))
	}
}
