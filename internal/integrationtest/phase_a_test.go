//go:build integration
// +build integration

// Package integrationtest exercises the Phase A kernel end-to-end against a
// real PostgreSQL instance. Run with:
//
//	KAPP_TEST_DB_URL=postgres://kapp:kapp_dev@localhost:5432/kapp?sslmode=disable \
//	  go test -tags=integration ./internal/integrationtest/...
//
// The tests are skipped when KAPP_TEST_DB_URL is unset, keeping `go test ./...`
// fast and hermetic.
package integrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// harness bundles the kernel collaborators against a live pool.
//
// The pool connects as `kapp_app` — the same non-superuser role the
// application uses in production — so RLS is enforced during tests.
// adminPool connects as `kapp_admin` (BYPASSRLS) and is optional; it is
// used by the RLS default-deny test and by TestUserStoreGetUserTenants to
// exercise the control-plane cross-tenant lookup.
type harness struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
	tenants   *tenant.PGStore
	ktypes    *ktype.PGRegistry
	publisher *events.PGPublisher
	auditor   *audit.PGLogger
	records   *record.PGStore
	users     *tenant.UserStore
}

func newHarness(t *testing.T) *harness {
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

	var adminPool *pgxpool.Pool
	if adminURL := os.Getenv("KAPP_TEST_ADMIN_DB_URL"); adminURL != "" {
		adminPool, err = platform.NewPool(ctx, adminURL)
		if err != nil {
			t.Fatalf("open admin pool: %v", err)
		}
		t.Cleanup(func() { adminPool.Close() })
	}

	cache := platform.NewLRUCache(64, time.Minute)
	h := &harness{
		pool:      pool,
		adminPool: adminPool,
		tenants:   tenant.NewPGStore(pool),
		ktypes:    ktype.NewPGRegistry(pool, cache),
		publisher: events.NewPGPublisher(pool),
		auditor:   audit.NewPGLogger(pool),
	}
	h.records = record.NewPGStore(pool, h.ktypes, h.publisher, h.auditor)
	h.users = tenant.NewUserStore(pool).WithAdminPool(adminPool)
	return h
}

// uniqueSlug keeps tests from colliding when run against a shared DB.
func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString()[:8])
}

func TestTenantLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	slug := uniqueSlug("tenant")
	created, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: slug, Name: "Integration Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Status != tenant.StatusActive {
		t.Fatalf("expected active, got %q", created.Status)
	}

	got, err := h.tenants.Get(ctx, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("get: %v (got=%+v)", err, got)
	}
	bySlug, err := h.tenants.GetBySlug(ctx, slug)
	if err != nil || bySlug.ID != created.ID {
		t.Fatalf("get by slug: %v (got=%+v)", err, bySlug)
	}

	list, err := h.tenants.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !containsTenant(list, created.ID) {
		t.Fatalf("list did not include created tenant id=%s", created.ID)
	}

	// Allowed transitions (per internal/tenant/store.go):
	//   active    → suspended  (Suspend)
	//   suspended → active     (Activate)
	//   suspended → archived   (Archive)
	//   any ≠ deleting → deleting (Delete)
	// So to exercise every lifecycle op we suspend twice.
	for _, tc := range []struct {
		name string
		op   func(context.Context, uuid.UUID) error
		want tenant.Status
	}{
		{"suspend", h.tenants.Suspend, tenant.StatusSuspended},
		{"activate", h.tenants.Activate, tenant.StatusActive},
		{"suspend again", h.tenants.Suspend, tenant.StatusSuspended},
		{"archive", h.tenants.Archive, tenant.StatusArchived},
		{"delete", h.tenants.Delete, tenant.StatusDeleting},
	} {
		if err := tc.op(ctx, created.ID); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, err := h.tenants.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("%s: refetch: %v", tc.name, err)
		}
		if got.Status != tc.want {
			t.Fatalf("%s: got status %q, want %q", tc.name, got.Status, tc.want)
		}
	}
}

func TestKTypeRegistry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := uniqueSlug("crm.deal")

	schema := json.RawMessage(`{"fields":[
		{"name":"title","type":"string","required":true,"max_length":120},
		{"name":"amount","type":"number","required":true,"min":0},
		{"name":"stage","type":"enum","values":["lead","qualified","won","lost"],"required":true}
	]}`)
	if err := h.ktypes.Register(ctx, ktype.KType{Name: name, Version: 1, Schema: schema}); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := h.ktypes.Get(ctx, name, 0)
	if err != nil || got.Name != name || got.Version != 1 {
		t.Fatalf("get latest: %v (got=%+v)", err, got)
	}

	// Cache hit path.
	got2, err := h.ktypes.Get(ctx, name, 1)
	if err != nil || got2.Version != 1 {
		t.Fatalf("get v1 (cache): %v (got=%+v)", err, got2)
	}

	list, err := h.ktypes.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, kt := range list {
		if kt.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("list did not include %q", name)
	}
}

func TestRecordCRUDEmitsEventsAndAudit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("rec"), Name: "Rec Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	kname := uniqueSlug("crm.deal")
	if err := h.ktypes.Register(ctx, ktype.KType{
		Name: kname, Version: 1,
		Schema: json.RawMessage(`{"fields":[
			{"name":"title","type":"string","required":true},
			{"name":"amount","type":"number","required":true},
			{"name":"stage","type":"enum","values":["lead","won"],"required":true}
		]}`),
	}); err != nil {
		t.Fatalf("register ktype: %v", err)
	}

	actor := uuid.New()
	created, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     kname,
		Data:      json.RawMessage(`{"title":"Alpha","amount":1,"stage":"lead"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("want version=1, got %d", created.Version)
	}

	// Validation rejects an unknown enum value.
	if _, err := h.records.Create(ctx, record.KRecord{
		TenantID: tn.ID, KType: kname, CreatedBy: actor,
		Data: json.RawMessage(`{"title":"Bad","amount":2,"stage":"bogus"}`),
	}); err == nil {
		t.Fatalf("expected validation error for bogus enum")
	}

	got, err := h.records.Get(ctx, tn.ID, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("get: %v (got=%+v)", err, got)
	}

	list, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: kname})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: got %d (err=%v)", len(list), err)
	}

	updatedBy := uuid.New()
	updated, err := h.records.Update(ctx, record.KRecord{
		TenantID: tn.ID, ID: created.ID,
		Data:      json.RawMessage(`{"stage":"won"}`),
		Version:   1,
		UpdatedBy: &updatedBy,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("want version=2, got %d", updated.Version)
	}
	var merged map[string]any
	if err := json.Unmarshal(updated.Data, &merged); err != nil {
		t.Fatalf("unmarshal updated data: %v", err)
	}
	if merged["stage"] != "won" || merged["title"] != "Alpha" {
		t.Fatalf("shallow merge failed: %+v", merged)
	}

	// Optimistic concurrency: stale version.
	if _, err := h.records.Update(ctx, record.KRecord{
		TenantID: tn.ID, ID: created.ID,
		Data:      json.RawMessage(`{"stage":"lead"}`),
		Version:   1,
		UpdatedBy: &updatedBy,
	}); err == nil {
		t.Fatalf("expected version conflict")
	}

	if err := h.records.Delete(ctx, tn.ID, created.ID, updatedBy); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := h.records.Delete(ctx, tn.ID, created.ID, updatedBy); err == nil {
		t.Fatalf("expected ErrNotFound on double-delete")
	}

	// Events: three undelivered entries in the outbox for this tenant.
	types, err := eventTypesForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("fetch events: %v", err)
	}
	want := []string{"krecord.created", "krecord.updated", "krecord.deleted"}
	if !sameSet(types, want) {
		t.Fatalf("event types mismatch: got=%v want=%v", types, want)
	}

	// Audit: one entry per mutation with tenant + actor set.
	entries, err := auditActionsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("fetch audit: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 audit entries, got %d (%v)", len(entries), entries)
	}
}

func TestRLSIsolatesTenants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	a, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("a"), Name: "A", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant a: %v", err)
	}
	b, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("b"), Name: "B", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant b: %v", err)
	}
	kname := uniqueSlug("note")
	if err := h.ktypes.Register(ctx, ktype.KType{
		Name: kname, Version: 1,
		Schema: json.RawMessage(`{"fields":[{"name":"body","type":"string","required":true}]}`),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	actor := uuid.New()
	if _, err := h.records.Create(ctx, record.KRecord{
		TenantID: a.ID, KType: kname,
		Data:      json.RawMessage(`{"body":"secret-a"}`),
		CreatedBy: actor,
	}); err != nil {
		t.Fatalf("create a: %v", err)
	}

	// Tenant B cannot list tenant A's records.
	list, err := h.records.List(ctx, b.ID, record.ListFilter{KType: kname})
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("RLS leak: tenant b saw %d records from tenant a", len(list))
	}
}

// --- helpers ---

func containsTenant(ts []tenant.Tenant, id uuid.UUID) bool {
	for _, t := range ts {
		if t.ID == id {
			return true
		}
	}
	return false
}

func eventTypesForTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT type FROM events WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func auditActionsForTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT action FROM audit_log WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// _ keeps the pgx import tidy if future tests use tx helpers directly.
var _ = pgx.TxOptions{}

// TestFirstCodingSlice exercises the end-to-end scenario from
// PROGRESS.md ("First Coding Slice — Acceptance Test Checklist"): two
// tenants `acme` and `globex`, one user each, one demo.note record each,
// and a strict RLS check that the default role sees zero rows without a
// tenant context. This is the scenario that elevates Phase A from "code
// written" to "kernel proven end-to-end".
func TestFirstCodingSlice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 1. Create two tenants `acme` and `globex`.
	acme, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("acme"), Name: "Acme Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create acme: %v", err)
	}
	globex, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("globex"), Name: "Globex Corp", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create globex: %v", err)
	}

	// 2. Register KType demo.note with fields title (required), body.
	kname := uniqueSlug("demo.note")
	if err := h.ktypes.Register(ctx, ktype.KType{
		Name: kname, Version: 1,
		Schema: json.RawMessage(`{"fields":[
			{"name":"title","type":"string","required":true,"max_length":120},
			{"name":"body","type":"text"}
		]}`),
	}); err != nil {
		t.Fatalf("register demo.note: %v", err)
	}

	// 3. Create users alice (in acme) and bob (in globex).
	alice, err := h.users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-alice-" + uuid.NewString()[:8],
		Email:       "alice@acme.test",
		DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := h.users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-bob-" + uuid.NewString()[:8],
		Email:       "bob@globex.test",
		DisplayName: "Bob",
	})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if err := h.users.AddUserToTenant(ctx, alice.ID, acme.ID, "owner"); err != nil {
		t.Fatalf("bind alice->acme: %v", err)
	}
	if err := h.users.AddUserToTenant(ctx, bob.ID, globex.ID, "owner"); err != nil {
		t.Fatalf("bind bob->globex: %v", err)
	}

	// 4. Alice creates a note in acme; Bob creates a note in globex.
	aliceNote, err := h.records.Create(ctx, record.KRecord{
		TenantID:  acme.ID, KType: kname,
		Data:      json.RawMessage(`{"title":"Acme launch plan","body":"Q1 roadmap"}`),
		CreatedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("alice create note: %v", err)
	}
	bobNote, err := h.records.Create(ctx, record.KRecord{
		TenantID:  globex.ID, KType: kname,
		Data:      json.RawMessage(`{"title":"Globex offsite","body":"Week of Nov 14"}`),
		CreatedBy: bob.ID,
	})
	if err != nil {
		t.Fatalf("bob create note: %v", err)
	}

	// 5 & 6. Each tenant's list returns only its own records.
	aList, err := h.records.List(ctx, acme.ID, record.ListFilter{KType: kname})
	if err != nil || len(aList) != 1 || aList[0].ID != aliceNote.ID {
		t.Fatalf("acme list: got len=%d err=%v want 1 (alice's note %s)",
			len(aList), err, aliceNote.ID)
	}
	gList, err := h.records.List(ctx, globex.ID, record.ListFilter{KType: kname})
	if err != nil || len(gList) != 1 || gList[0].ID != bobNote.ID {
		t.Fatalf("globex list: got len=%d err=%v want 1 (bob's note %s)",
			len(gList), err, bobNote.ID)
	}

	// 7. Direct DB query under acme's tenant context returns only acme rows.
	var acmeRows int
	if err := dbutil.WithTenantTx(ctx, h.pool, acme.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM krecords WHERE ktype = $1`, kname,
		).Scan(&acmeRows)
	}); err != nil {
		t.Fatalf("acme scoped scan: %v", err)
	}
	if acmeRows != 1 {
		t.Fatalf("acme scoped rows = %d; want 1", acmeRows)
	}

	// 8. Direct DB query with NO tenant context returns zero rows (RLS
	// default-deny). The query runs on the shared app pool without setting
	// app.tenant_id.
	var nakedRows int
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM krecords WHERE ktype = $1`, kname,
	).Scan(&nakedRows); err != nil {
		t.Fatalf("naked scan: %v", err)
	}
	if nakedRows != 0 {
		t.Fatalf("RLS default-deny breach: saw %d rows without tenant context", nakedRows)
	}

	// 9. Every create produced exactly one event and one audit entry per tenant.
	for name, tnID := range map[string]uuid.UUID{"acme": acme.ID, "globex": globex.ID} {
		types, err := eventTypesForTenant(ctx, h.pool, tnID)
		if err != nil {
			t.Fatalf("%s: events: %v", name, err)
		}
		if len(types) != 1 || types[0] != "krecord.created" {
			t.Fatalf("%s: events = %v; want [krecord.created]", name, types)
		}
		actions, err := auditActionsForTenant(ctx, h.pool, tnID)
		if err != nil {
			t.Fatalf("%s: audit: %v", name, err)
		}
		if len(actions) != 1 {
			t.Fatalf("%s: audit len = %d; want 1 (%v)", name, len(actions), actions)
		}
	}
}

// TestRateLimitKicksIn confirms that per-tenant rate limiting rejects
// requests past the configured burst. Covered via the platform.RateLimiter
// directly — the middleware wraps this same call.
func TestRateLimitKicksIn(t *testing.T) {
	_ = newHarness(t) // ensure we still skip when the DB is absent
	limiter := platform.NewRateLimiter(platform.RateLimitConfig{
		RequestsPerMinute: 60,
		BurstSize:         3,
		IdleTimeout:       time.Minute,
	})
	tenantID := uuid.New()
	for i := 0; i < 3; i++ {
		if !limiter.Allow(tenantID, 0, 0) {
			t.Fatalf("call %d should be allowed within burst", i)
		}
	}
	if limiter.Allow(tenantID, 0, 0) {
		t.Fatalf("call past burst should be rejected")
	}
}

// TestUserStoreGetUserTenants confirms that the admin-pool pathway sees
// memberships for a user across multiple tenants, satisfying the login
// flow requirement. Skips when the admin pool is unavailable — that
// configuration is documented and not a hard error.
func TestUserStoreGetUserTenants(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping cross-tenant lookup test")
	}
	ctx := context.Background()

	u, err := h.users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-multi-" + uuid.NewString()[:8],
		Email:       "multi@test",
		DisplayName: "Multi",
	})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	a, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("ua"), Name: "UA", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant ua: %v", err)
	}
	b, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("ub"), Name: "UB", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant ub: %v", err)
	}
	if err := h.users.AddUserToTenant(ctx, u.ID, a.ID, "owner"); err != nil {
		t.Fatalf("bind ua: %v", err)
	}
	if err := h.users.AddUserToTenant(ctx, u.ID, b.ID, "member"); err != nil {
		t.Fatalf("bind ub: %v", err)
	}
	memberships, err := h.users.GetUserTenants(ctx, u.ID)
	if err != nil {
		t.Fatalf("get memberships: %v", err)
	}
	if len(memberships) != 2 {
		t.Fatalf("memberships len = %d; want 2 (%+v)", len(memberships), memberships)
	}
}

// BenchmarkTenantContextSwitch measures the cost of setting
// app.tenant_id on a fresh transaction — the primary per-request
// overhead that the Kapp kernel imposes on top of plain pgx. The target
// per ARCHITECTURE.md is sub-millisecond.
func BenchmarkTenantContextSwitch(b *testing.B) {
	dbURL := os.Getenv("KAPP_TEST_DB_URL")
	if dbURL == "" {
		b.Skip("KAPP_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := platform.NewPool(ctx, dbURL)
	if err != nil {
		b.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Pre-seed a tenant so the benchmark is meaningful on a clean DB.
	store := tenant.NewPGStore(pool)
	tn, err := store.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("bench"), Name: "Bench", Cell: "test", Plan: "free",
	})
	if err != nil {
		b.Fatalf("seed tenant: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := dbutil.WithTenantTx(ctx, pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
			// A trivial read so the tx has something to commit.
			var one int
			return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
		}); err != nil {
			b.Fatalf("tx %d: %v", i, err)
		}
	}
}
