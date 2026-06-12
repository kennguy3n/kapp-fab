//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
	"github.com/kennguy3n/kapp-fab/internal/warehouse"
)

// seedContact inserts a crm.contact KRecord and returns its id.
func seedContact(t *testing.T, h *harness, tenantID, actor uuid.UUID, name string) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "email": name + "@example.invalid"})
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     crm.KTypeContact,
		Data:      body,
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("seed contact %q: %v", name, err)
	}
	return rec.ID
}

// TestWarehouseSyncExportsRowsToDestinationSchema is the end-to-end
// assertion required by the workstream: a tenant's krecords are
// exported into a second Postgres schema (the "external warehouse")
// and the rows are observed to have landed. It also exercises the
// incremental path: a second run after seeding more rows appends only
// the new rows and advances the watermark, never duplicating by PK.
func TestWarehouseSyncExportsRowsToDestinationSchema(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping warehouse export integration test")
	}
	adminURL := os.Getenv("KAPP_TEST_ADMIN_DB_URL")
	if adminURL == "" {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping warehouse export integration test")
	}
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("wh"), Name: "Warehouse Co", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := crm.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}
	actor := uuid.New()
	seedContact(t, h, tn.ID, actor, "alice")
	seedContact(t, h, tn.ID, actor, "bob")

	// The "external warehouse" is the same Postgres reached via the
	// admin role (which has DDL rights), written into a unique schema
	// so the mirror tables never collide with other tests.
	destSchema := "wh_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = h.adminPool.Exec(context.Background(),
			fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, destSchema))
	})

	enc := newTestEncryptor()
	dsStore := insights.NewDataSourceStore(h.pool, enc)
	ds, err := dsStore.Create(ctx, insights.DataSource{
		TenantID:         tn.ID,
		Name:             "warehouse-dest",
		Dialect:          "postgres",
		ConnectionString: adminURL,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create destination datasource: %v", err)
	}

	pools := insights.NewPoolManager()
	t.Cleanup(pools.Close)
	exporter := warehouse.NewExporter(h.pool, dsStore, pools)

	configStore := warehouse.NewConfigStore(h.pool)
	runStore := warehouse.NewRunStore(h.pool)
	syncH := warehouse.NewSyncHandler(configStore, runStore, exporter)

	cfg, err := configStore.Create(ctx, warehouse.Config{
		TenantID:                tn.ID,
		Name:                    "nightly-contacts",
		DestinationDataSourceID: ds.ID,
		DestinationSchema:       destSchema,
		Sources:                 []string{"ktype:crm.contact"},
		CronExpression:          "0 2 * * *",
		Mode:                    warehouse.ModeIncremental,
		Enabled:                 true,
		CreatedBy:               &actor,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// First run — manual trigger. Both seeded contacts must land.
	run, err := syncH.RunConfig(ctx, tn.ID, cfg, warehouse.TriggerManual)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if run.Status != warehouse.StatusSuccess {
		t.Fatalf("first run status = %q (%s), want success", run.Status, run.Error)
	}
	if run.RowsExported != 2 {
		t.Fatalf("first run rows = %d, want 2", run.RowsExported)
	}

	mirror := fmt.Sprintf("%q.%q", destSchema, "ktype_crm_contact")
	assertCount(t, h, mirror, 2)

	// The watermark must have advanced so the next run is incremental.
	reloaded, err := configStore.Get(ctx, tn.ID, cfg.ID)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(reloaded.Watermarks["ktype:crm.contact"]) == 0 {
		t.Fatalf("watermark not persisted after first run: %+v", reloaded.Watermarks)
	}
	if reloaded.LastStatus != warehouse.StatusSuccess {
		t.Fatalf("config last_status = %q, want success", reloaded.LastStatus)
	}

	// Seed one more contact and re-run. Only the new row should be
	// read+written this time, and the mirror should hold 3 (no dupes).
	seedContact(t, h, tn.ID, actor, "carol")
	run2, err := syncH.RunConfig(ctx, tn.ID, reloaded, warehouse.TriggerSchedule)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if run2.RowsExported != 1 {
		t.Fatalf("second (incremental) run rows = %d, want 1", run2.RowsExported)
	}
	assertCount(t, h, mirror, 3)

	// Run history must record both runs, newest first.
	runs, err := runStore.List(ctx, tn.ID, cfg.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("run history = %d rows, want 2", len(runs))
	}
	if runs[0].Trigger != warehouse.TriggerSchedule || runs[1].Trigger != warehouse.TriggerManual {
		t.Fatalf("run history order/trigger wrong: %q then %q", runs[0].Trigger, runs[1].Trigger)
	}
}

// assertCount checks the mirror table holds exactly want rows, read
// through the admin pool (the destination warehouse is not RLS-scoped).
func assertCount(t *testing.T, h *harness, fqTable string, want int64) {
	t.Helper()
	var got int64
	if err := h.adminPool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+fqTable).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", fqTable, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", fqTable, got, want)
	}
}

// TestWarehouseSyncTenantIsolation confirms the export only mirrors the
// running tenant's rows: a second tenant's contacts in the same source
// table never appear in the first tenant's destination schema. This is
// the core privacy guarantee for the 5000-tenant fleet — the source
// read runs under RLS on the app-role pool.
func TestWarehouseSyncTenantIsolation(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping isolation test")
	}
	adminURL := os.Getenv("KAPP_TEST_ADMIN_DB_URL")
	if adminURL == "" {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set")
	}
	ctx := context.Background()
	if err := crm.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}

	tnA, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("whA"), Name: "Tenant A", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("whB"), Name: "Tenant B", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}
	actor := uuid.New()
	seedContact(t, h, tnA.ID, actor, "a-only-1")
	seedContact(t, h, tnA.ID, actor, "a-only-2")
	seedContact(t, h, tnB.ID, actor, "b-secret-1")
	seedContact(t, h, tnB.ID, actor, "b-secret-2")
	seedContact(t, h, tnB.ID, actor, "b-secret-3")

	destSchema := "wh_iso_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = h.adminPool.Exec(context.Background(),
			fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, destSchema))
	})

	dsStore := insights.NewDataSourceStore(h.pool, newTestEncryptor())
	ds, err := dsStore.Create(ctx, insights.DataSource{
		TenantID: tnA.ID, Name: "destA", Dialect: "postgres",
		ConnectionString: adminURL, Enabled: true,
	})
	if err != nil {
		t.Fatalf("datasource: %v", err)
	}
	pools := insights.NewPoolManager()
	t.Cleanup(pools.Close)
	syncH := warehouse.NewSyncHandler(
		warehouse.NewConfigStore(h.pool),
		warehouse.NewRunStore(h.pool),
		warehouse.NewExporter(h.pool, dsStore, pools),
	)
	cfg, err := warehouse.NewConfigStore(h.pool).Create(ctx, warehouse.Config{
		TenantID: tnA.ID, Name: "iso", DestinationDataSourceID: ds.ID,
		DestinationSchema: destSchema, Sources: []string{"ktype:crm.contact"},
		CronExpression: "0 2 * * *", Mode: warehouse.ModeFull, Enabled: true,
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	run, err := syncH.RunConfig(ctx, tnA.ID, cfg, warehouse.TriggerManual)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Only tenant A's two contacts may land — never tenant B's three.
	if run.RowsExported != 2 {
		t.Fatalf("rows = %d, want 2 (tenant A only)", run.RowsExported)
	}
	assertCount(t, h, fmt.Sprintf("%q.%q", destSchema, "ktype_crm_contact"), 2)
}
