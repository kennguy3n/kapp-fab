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

// TestWarehouseSyncStartManualRunAsync exercises the API "run now"
// path: StartManualRun records a 'running' run and returns immediately
// while the caller drives the export on a detached context. It asserts
// (a) the returned run is observable as 'running' before finish, (b) a
// second StartManualRun is rejected with ErrRunInProgress while the
// first holds the per-config lock, and (c) finish lands the rows and
// finalizes the run to 'success'.
func TestWarehouseSyncStartManualRunAsync(t *testing.T) {
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
		Slug: uniqueSlug("wh"), Name: "Warehouse Async Co", Cell: "test", Plan: "enterprise",
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
		Name:                    "ondemand-contacts",
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

	run, finish, err := syncH.StartManualRun(ctx, tn.ID, cfg)
	if err != nil {
		t.Fatalf("start manual run: %v", err)
	}
	if run.Status != warehouse.StatusRunning {
		t.Fatalf("started run status = %q, want running", run.Status)
	}
	if run.Trigger != warehouse.TriggerManual {
		t.Fatalf("started run trigger = %q, want manual", run.Trigger)
	}

	// The run row is durable and observable as 'running' before finish.
	pending, err := runStore.List(ctx, tn.ID, cfg.ID, 10)
	if err != nil {
		t.Fatalf("list runs (pending): %v", err)
	}
	if len(pending) != 1 || pending[0].Status != warehouse.StatusRunning {
		t.Fatalf("pending run not observable as running: %+v", pending)
	}

	// While the first run holds the lock, a colliding run-now is
	// rejected rather than racing into a second concurrent export.
	if _, _, err := syncH.StartManualRun(ctx, tn.ID, cfg); err != warehouse.ErrRunInProgress {
		t.Fatalf("concurrent StartManualRun err = %v, want ErrRunInProgress", err)
	}

	if err := finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}

	mirror := fmt.Sprintf("%q.%q", destSchema, "ktype_crm_contact")
	assertCount(t, h, mirror, 2)

	done, err := runStore.List(ctx, tn.ID, cfg.ID, 10)
	if err != nil {
		t.Fatalf("list runs (done): %v", err)
	}
	if len(done) != 1 || done[0].Status != warehouse.StatusSuccess {
		t.Fatalf("run not finalized to success: %+v", done)
	}
	if done[0].RowsExported != 2 {
		t.Fatalf("finished run rows = %d, want 2", done[0].RowsExported)
	}

	// The lock released with finish, so a fresh run-now is accepted again.
	reloaded, err := configStore.Get(ctx, tn.ID, cfg.ID)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	run2, finish2, err := syncH.StartManualRun(ctx, tn.ID, reloaded)
	if err != nil {
		t.Fatalf("second start manual run: %v", err)
	}
	if run2.Status != warehouse.StatusRunning {
		t.Fatalf("second started run status = %q, want running", run2.Status)
	}
	if err := finish2(ctx); err != nil {
		t.Fatalf("finish2: %v", err)
	}
}

// seedMove inserts an inventory_moves row for a tenant via the admin
// pool (which can write any tenant_id). The append-only moves feed the
// stock_levels aggregate view.
func seedMove(t *testing.T, h *harness, tenantID, itemID, warehouseID uuid.UUID, qty string) {
	t.Helper()
	if _, err := h.adminPool.Exec(context.Background(),
		`INSERT INTO inventory_moves (tenant_id, item_id, warehouse_id, qty)
		 VALUES ($1, $2, $3, $4::numeric)`,
		tenantID, itemID, warehouseID, qty); err != nil {
		t.Fatalf("seed inventory move: %v", err)
	}
}

// TestWarehouseSyncStockLevelsTenantIsolation is the regression guard
// for the cross-tenant leak Devin Review flagged: ledger.stock_levels
// is a plain (non security_invoker) VIEW that executes as its owner and
// so BYPASSES row-level security on the underlying inventory_moves
// table (relforcerowsecurity = false). Relying on RLS alone would
// export every tenant's stock into one tenant's warehouse. The export's
// explicit tenant_id = $1 predicate must contain the read so only the
// running tenant's stock lands. This test seeds two tenants' moves and
// asserts tenant B's stock never reaches tenant A's destination.
func TestWarehouseSyncStockLevelsTenantIsolation(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping stock-levels isolation test")
	}
	adminURL := os.Getenv("KAPP_TEST_ADMIN_DB_URL")
	if adminURL == "" {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set")
	}
	ctx := context.Background()

	tnA, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("whSA"), Name: "Stock A", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("whSB"), Name: "Stock B", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}

	// Tenant A: two distinct (item, warehouse) positions. Tenant B:
	// three positions that must never leak. Multiple moves per position
	// also confirm the aggregate SUM(qty) is exported as one row.
	aItem1, aItem2, wh := uuid.New(), uuid.New(), uuid.New()
	seedMove(t, h, tnA.ID, aItem1, wh, "10")
	seedMove(t, h, tnA.ID, aItem1, wh, "5") // same position, aggregates to 15
	seedMove(t, h, tnA.ID, aItem2, wh, "7")
	for i := 0; i < 3; i++ {
		seedMove(t, h, tnB.ID, uuid.New(), wh, "99")
	}

	destSchema := "wh_stk_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = h.adminPool.Exec(context.Background(),
			fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, destSchema))
	})

	dsStore := insights.NewDataSourceStore(h.pool, newTestEncryptor())
	ds, err := dsStore.Create(ctx, insights.DataSource{
		TenantID: tnA.ID, Name: "stockDestA", Dialect: "postgres",
		ConnectionString: adminURL, Enabled: true,
	})
	if err != nil {
		t.Fatalf("datasource: %v", err)
	}
	pools := insights.NewPoolManager()
	t.Cleanup(pools.Close)
	configStore := warehouse.NewConfigStore(h.pool)
	syncH := warehouse.NewSyncHandler(
		configStore,
		warehouse.NewRunStore(h.pool),
		warehouse.NewExporter(h.pool, dsStore, pools),
	)
	cfg, err := configStore.Create(ctx, warehouse.Config{
		TenantID: tnA.ID, Name: "stock-iso", DestinationDataSourceID: ds.ID,
		DestinationSchema: destSchema, Sources: []string{"ledger.stock_levels"},
		CronExpression: "0 2 * * *", Mode: warehouse.ModeFull, Enabled: true,
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	run, err := syncH.RunConfig(ctx, tnA.ID, cfg, warehouse.TriggerManual)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != warehouse.StatusSuccess {
		t.Fatalf("run status = %q (%s), want success", run.Status, run.Error)
	}
	// Tenant A has exactly two aggregated positions; tenant B's three
	// must NOT leak through the RLS-bypassing view.
	if run.RowsExported != 2 {
		t.Fatalf("rows = %d, want 2 (tenant A positions only)", run.RowsExported)
	}
	mirror := fmt.Sprintf("%q.%q", destSchema, "ledger_stock_levels")
	assertCount(t, h, mirror, 2)

	// The aggregate for aItem1 must be the SUM (10+5=15), proving the
	// view's grouping is preserved and only A's moves contribute.
	var qty string
	if err := h.adminPool.QueryRow(ctx,
		fmt.Sprintf(`SELECT qty::text FROM %s WHERE item_id = $1`, mirror), aItem1).Scan(&qty); err != nil {
		t.Fatalf("read aggregated qty: %v", err)
	}
	if qty != "15.0000" {
		t.Fatalf("aItem1 qty = %q, want 15.0000", qty)
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
