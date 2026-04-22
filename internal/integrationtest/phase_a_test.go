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
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// harness bundles the kernel collaborators against a live pool.
type harness struct {
	pool      *pgxpool.Pool
	tenants   *tenant.PGStore
	ktypes    *ktype.PGRegistry
	publisher *events.PGPublisher
	auditor   *audit.PGLogger
	records   *record.PGStore
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

	cache := platform.NewLRUCache(64, time.Minute)
	h := &harness{
		pool:      pool,
		tenants:   tenant.NewPGStore(pool),
		ktypes:    ktype.NewPGRegistry(pool, cache),
		publisher: events.NewPGPublisher(pool),
		auditor:   audit.NewPGLogger(pool),
	}
	h.records = record.NewPGStore(pool, h.ktypes, h.publisher, h.auditor)
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
		{"name":"stage","type":"enum","enum":["lead","qualified","won","lost"],"required":true}
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
			{"name":"stage","type":"enum","enum":["lead","won"],"required":true}
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
