//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestTenantRecordCounts_BumpAndReconcile is the end-to-end check for
// the Phase O denormalised KRecord counter (A2 in the roadmap):
//
//  1. After Create, tenant_record_counts.record_count is incremented
//     by 1 inside the same WithTenantTx — the row exists even though
//     no caller seeded it (UPSERT path of bumpTenantRecordCount).
//  2. After multiple Creates the counter equals the active row count.
//  3. After Delete, the counter is decremented by 1 — and the
//     re-delete short-circuit (ErrNotFound) does NOT double-decrement.
//  4. QuotaEnforcer.CheckRecordCount reads from the counter table
//     (returns ErrQuotaExceeded when counter >= max_records) and
//     never falls back to a krecords COUNT(*).
//  5. RecordCountReconciler corrects an injected drift back to the
//     authoritative krecords scan.
func TestTenantRecordCounts_BumpAndReconcile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("trc"), Name: "Trc Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	kname := uniqueSlug("custom.trc_item")
	if err := h.ktypes.Register(ctx, ktype.KType{
		Name: kname, Version: 1,
		Schema: json.RawMessage(`{"fields":[{"name":"name","type":"string","required":true}]}`),
	}); err != nil {
		t.Fatalf("register ktype: %v", err)
	}
	actor := uuid.New()

	// (0) Fresh tenant — no counter row exists yet. CheckRecordCount
	// must treat absence as zero and allow the first insert without
	// falling back to the old O(n) COUNT(*).
	enforcer := platform.NewQuotaEnforcer(h.pool)
	if err := enforcer.CheckRecordCount(ctx, tn.ID, platform.Quota{MaxRecords: 5}); err != nil {
		t.Fatalf("CheckRecordCount on fresh tenant: %v", err)
	}
	if got := readCount(t, h.pool, tn.ID); got != -1 {
		t.Fatalf("fresh tenant should have no counter row, got %d", got)
	}

	// (1+2) Three creates → counter = 3.
	created := make([]uuid.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		r, err := h.records.Create(ctx, record.KRecord{
			TenantID:  tn.ID,
			KType:     kname,
			Data:      json.RawMessage(`{"name":"a"}`),
			CreatedBy: actor,
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		created = append(created, r.ID)
	}
	if got := readCount(t, h.pool, tn.ID); got != 3 {
		t.Fatalf("after 3 creates: want counter=3, got %d", got)
	}

	// (3) Delete one. Counter should drop to 2.
	if err := h.records.Delete(ctx, tn.ID, created[0], actor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := readCount(t, h.pool, tn.ID); got != 2 {
		t.Fatalf("after 1 delete: want counter=2, got %d", got)
	}
	// Re-deleting the same id is a no-op (ErrNotFound) and MUST NOT
	// decrement again — the bump only fires after the
	// "already deleted" early-return in store.Delete.
	if err := h.records.Delete(ctx, tn.ID, created[0], actor); err == nil {
		t.Fatalf("re-delete should have returned ErrNotFound")
	}
	if got := readCount(t, h.pool, tn.ID); got != 2 {
		t.Fatalf("after re-delete: counter must stay at 2, got %d", got)
	}

	// (4) Quota enforcement reads from the counter.
	if err := enforcer.CheckRecordCount(ctx, tn.ID, platform.Quota{MaxRecords: 2}); err == nil {
		t.Fatalf("expected ErrQuotaExceeded at MaxRecords=2 with counter=2")
	}
	if err := enforcer.CheckRecordCount(ctx, tn.ID, platform.Quota{MaxRecords: 5}); err != nil {
		t.Fatalf("CheckRecordCount(MaxRecords=5) on counter=2: %v", err)
	}

	// (5) Inject drift: corrupt the counter to a wrong value and
	// confirm the reconciler restores the authoritative scan.
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE tenant_record_counts
			    SET record_count = 999
			  WHERE tenant_id = $1`,
			tn.ID)
		return err
	}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	if got := readCount(t, h.pool, tn.ID); got != 999 {
		t.Fatalf("drift inject failed: want 999, got %d", got)
	}

	reconciler := platform.NewRecordCountReconciler(h.pool)
	if err := reconciler.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: platform.ActionTypeRecordCountRecount,
		TenantID:   tn.ID,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readCount(t, h.pool, tn.ID); got != 2 {
		t.Fatalf("after reconcile: want counter=2 (matches 2 active rows), got %d", got)
	}
}

// readCount returns tenant_record_counts.record_count for the tenant
// or -1 when no row exists. The query runs through WithTenantTx
// because tenant_record_counts has RLS enabled — a plain admin pool
// query would either be filtered out or require a separate BYPASSRLS
// connection just for the test. WithTenantTx sets app.tenant_id so the
// RLS USING clause passes; the value we read is still the same single
// row keyed by tenant_id.
//
// The reason this helper exists at all (instead of calling
// QuotaEnforcer.CheckRecordCount) is that the production read path
// conflates "row absent" with "row present with value 0" — both yield
// count=0 and "allow the insert". The test needs to assert the
// stronger property that the fresh-tenant code path never created a
// counter row, which only a raw SELECT distinguishing pgx.ErrNoRows
// from a zero scan can express.
func readCount(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) int64 {
	t.Helper()
	var n int64
	err := dbutil.WithTenantTx(context.Background(), pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT record_count FROM tenant_record_counts WHERE tenant_id = $1`,
			tenantID,
		).Scan(&n)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return -1
		}
		t.Fatalf("readCount: %v", err)
	}
	return n
}
