package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/events"
)

// stockAlertEventType is the outbox event the notification router
// fans out to KChat. Carrying a `notification` envelope alongside the
// structured alert body lets the existing router deliver the user-
// visible card without stock-alert code touching KChat directly.
const stockAlertEventType = "inventory.low_stock_alert"

// stockAlertInterval governs how often the worker sweeps for low
// stock. A tight cadence is wasted work because inventory moves do
// not fire high-frequency price-tick style updates; a 60-second cycle
// is more than responsive enough for replenishment decisions and
// keeps the query volume on stock_levels negligible.
const stockAlertInterval = 60 * time.Second

// stockAlertDedupeWindow is the minimum gap between two alerts for
// the same (tenant, item, warehouse) tuple. Without it, a continuously
// below-threshold SKU would produce one KChat card per sweep. The
// worker keeps the last-alert timestamp in memory only — on restart
// the first cycle re-emits, which is a deliberate safety valve so
// operators who miss alerts during a deploy still see them afterwards.
const stockAlertDedupeWindow = 6 * time.Hour

// maxAlertEntries caps the in-memory dedupe map so a widespread
// shortage across thousands of tenants cannot grow lastSent without
// bound between dedupe-window evictions. When the cap is hit the
// worker drops the oldest entries — re-emitting a duplicate alert on
// the next sweep is preferable to an OOM.
const maxAlertEntries = 100_000

// stockAlertWorker runs a periodic sweep over stock_levels joined
// against inventory_items.reorder_level and emits one
// `inventory.low_stock_alert` outbox event per SKU that fell below
// its configured reorder point.
//
// The sweep query must run on a BYPASSRLS pool because it
// legitimately spans tenants — kapp_app sessions have no
// `app.tenant_id` GUC set at sweep time and RLS default-denies,
// which would silently return zero rows. The per-tenant emit below
// uses the regular pool + SET LOCAL so the outbox insert remains
// tenant-scoped. When adminPool is nil the sweeper short-circuits
// and logs a warning; the worker process stays up so the outbox
// drain keeps running.
type stockAlertWorker struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
	publisher events.Publisher
	setTenant func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error
	interval  time.Duration
	lastSent  map[alertKey]time.Time
	now       func() time.Time
}

// alertKey scopes the dedupe map to the exact SKU+warehouse pair the
// alert is about so two items tripping their thresholds around the
// same time still each produce their own card.
type alertKey struct {
	tenantID    uuid.UUID
	itemID      uuid.UUID
	warehouseID uuid.UUID
}

// newStockAlertWorker wires a worker that will use the platform's
// tenant-GUC setter so the publisher's INSERT respects RLS on the
// events table. setTenant is injected (rather than imported directly)
// to keep this file's imports compatible with `services/worker` which
// otherwise does not depend on platform/txn.go helpers.
func newStockAlertWorker(
	pool *pgxpool.Pool,
	adminPool *pgxpool.Pool,
	publisher events.Publisher,
	setTenant func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error,
) *stockAlertWorker {
	return &stockAlertWorker{
		pool:      pool,
		adminPool: adminPool,
		publisher: publisher,
		setTenant: setTenant,
		interval:  stockAlertInterval,
		lastSent:  make(map[alertKey]time.Time),
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// Run blocks until ctx is cancelled, sweeping at interval cadence.
// Errors from a single tick are logged and swallowed — a transient
// DB blip must not take the worker process down, the next tick will
// retry. Matches the error-handling contract of the outbox drain loop.
func (w *stockAlertWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	// Fire an immediate pass so freshly-deployed workers start
	// surfacing alerts without waiting a full interval.
	if err := w.sweep(ctx); err != nil {
		log.Printf("worker: stock alerts initial sweep: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.sweep(ctx); err != nil {
				log.Printf("worker: stock alerts sweep: %v", err)
			}
		}
	}
}

// lowStockRow is the projection we pull in a single multi-tenant scan.
// qty + reorder_level are kept as decimal.Decimal to match how every
// other inventory codepath accounts for stock (NUMERIC(20,4) in Postgres).
type lowStockRow struct {
	TenantID      uuid.UUID
	ItemID        uuid.UUID
	ItemSKU       string
	ItemName      string
	WarehouseID   uuid.UUID
	WarehouseCode string
	Qty           decimal.Decimal
	ReorderLevel  decimal.Decimal
}

// sweep runs one pass: query every SKU below its reorder level across
// all tenants, then emit a low-stock alert per row that isn't still
// inside the per-SKU dedupe window.
func (w *stockAlertWorker) sweep(ctx context.Context) error {
	if w.adminPool == nil {
		// No admin pool configured — the cross-tenant sweep would
		// default-deny under RLS and silently return zero rows. Skip
		// the sweep entirely and log once per tick so the operator
		// sees the feature is disabled rather than quietly broken.
		log.Printf("worker: stock alerts sweep skipped: ADMIN_DB_URL not set")
		return nil
	}
	// stock_levels is derived from inventory_moves, so items that have
	// never been moved do not appear there at all. The LEFT JOIN +
	// COALESCE(qty, 0) below lets a newly-created item with reorder_level>0
	// and no moves surface as zero-stock — arguably the most urgent case.
	// The CROSS JOIN is bounded by the per-tenant AND i.tenant_id=wh.tenant_id
	// predicate so it does not fan out across tenants.
	rows, err := w.adminPool.Query(ctx,
		`SELECT i.tenant_id, i.id, i.sku, i.name,
		        wh.id, wh.code,
		        COALESCE(sl.qty, 0), i.reorder_level
		   FROM inventory_items i
		   CROSS JOIN inventory_warehouses wh
		   LEFT JOIN stock_levels sl
		     ON sl.tenant_id = i.tenant_id
		    AND sl.item_id = i.id
		    AND sl.warehouse_id = wh.id
		  WHERE i.tenant_id = wh.tenant_id
		    AND i.active = TRUE
		    AND i.reorder_level > 0
		    AND COALESCE(sl.qty, 0) < i.reorder_level`,
	)
	if err != nil {
		return fmt.Errorf("query low stock: %w", err)
	}
	defer rows.Close()

	var alerts []lowStockRow
	for rows.Next() {
		var r lowStockRow
		if err := rows.Scan(
			&r.TenantID, &r.ItemID, &r.ItemSKU, &r.ItemName,
			&r.WarehouseID, &r.WarehouseCode,
			&r.Qty, &r.ReorderLevel,
		); err != nil {
			return fmt.Errorf("scan low stock row: %w", err)
		}
		alerts = append(alerts, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	now := w.now()
	for _, a := range alerts {
		key := alertKey{tenantID: a.TenantID, itemID: a.ItemID, warehouseID: a.WarehouseID}
		if last, ok := w.lastSent[key]; ok && now.Sub(last) < stockAlertDedupeWindow {
			continue
		}
		if err := w.emit(ctx, a); err != nil {
			log.Printf("worker: emit low stock alert tenant=%s item=%s: %v",
				a.TenantID, a.ItemID, err)
			continue
		}
		w.lastSent[key] = now
	}
	// Evict dedupe entries whose window has expired so the map stays
	// bounded by the number of currently-active low-stock situations
	// rather than growing with every alert ever fired. Mirrors the
	// idle-eviction pattern in RateLimiter.evictIdle.
	for k, last := range w.lastSent {
		if now.Sub(last) >= stockAlertDedupeWindow {
			delete(w.lastSent, k)
		}
	}
	// Hard cap so a widespread shortage across tens of thousands of
	// tenants cannot grow lastSent without bound between dedupe-window
	// evictions. When over the cap, drop the oldest quarter of entries
	// — re-emitting on the next sweep is preferable to an OOM and the
	// worker logs a warning so the operator notices.
	if len(w.lastSent) > maxAlertEntries {
		log.Printf("worker: stock alerts dedupe map exceeded cap (%d > %d); evicting oldest entries",
			len(w.lastSent), maxAlertEntries)
		w.evictOldest(len(w.lastSent) - maxAlertEntries*3/4)
	}
	return nil
}

// evictOldest removes the n oldest entries from the dedupe map. The
// single linear scan is fine at the map sizes we cap to — the map is
// never scanned for lookups in the hot path, only on sweep ticks.
func (w *stockAlertWorker) evictOldest(n int) {
	if n <= 0 || len(w.lastSent) == 0 {
		return
	}
	type kv struct {
		k alertKey
		t time.Time
	}
	entries := make([]kv, 0, len(w.lastSent))
	for k, t := range w.lastSent {
		entries = append(entries, kv{k, t})
	}
	// Partial sort by age ascending: we only need the first n keys.
	for i := 0; i < n && i < len(entries); i++ {
		min := i
		for j := i + 1; j < len(entries); j++ {
			if entries[j].t.Before(entries[min].t) {
				min = j
			}
		}
		entries[i], entries[min] = entries[min], entries[i]
		delete(w.lastSent, entries[i].k)
	}
}

// emit writes a single low-stock event to the outbox. The event body
// carries both the structured inventory fields (for programmatic
// consumers) and a `notification` envelope the worker's router maps
// to a KChat DM card — no new channel plumbing needed.
func (w *stockAlertWorker) emit(ctx context.Context, a lowStockRow) error {
	body := map[string]any{
		"tenant_id":       a.TenantID,
		"item_id":         a.ItemID,
		"item_sku":        a.ItemSKU,
		"item_name":       a.ItemName,
		"warehouse_id":    a.WarehouseID,
		"warehouse_code":  a.WarehouseCode,
		"qty":             a.Qty.String(),
		"reorder_level":   a.ReorderLevel.String(),
		"notification": map[string]any{
			"channel": "kchat",
			"title":   fmt.Sprintf("Low stock: %s @ %s", a.ItemSKU, a.WarehouseCode),
			"body": fmt.Sprintf("On-hand %s is below reorder level %s for %s at %s.",
				a.Qty.String(), a.ReorderLevel.String(), a.ItemName, a.WarehouseCode),
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := w.setTenant(ctx, tx, a.TenantID); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	if err := w.publisher.EmitTx(ctx, tx, events.Event{
		TenantID: a.TenantID,
		Type:     stockAlertEventType,
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("emit: %w", err)
	}
	return tx.Commit(ctx)
}
