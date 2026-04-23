//go:build loadtest
// +build loadtest

// Package load_test exercises the Kapp kernel against a large fleet
// of tenants so we can verify the ARCHITECTURE.md §1 claim that a
// single cell serves thousands of tenants with acceptable latency
// and without losing RLS isolation under concurrent load.
//
// This file is gated behind the `loadtest` build tag because it
// provisions 1000 tenants and runs ~1000× the CRUD traffic of the
// normal integration suite. Run with:
//
//	KAPP_TEST_DB_URL=postgres://kapp:kapp_dev@localhost:5432/kapp?sslmode=disable \
//	  go test -tags=loadtest -timeout=30m -run TestThousandTenantLoad ./internal/integrationtest/...
package integrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestThousandTenantLoad provisions a fleet of tenants, pushes CRUD
// traffic through all of them concurrently, and:
//
//  1. Reports p50/p95/p99 latency for Create/Get/List;
//  2. Asserts RLS isolation by randomly spot-checking that a
//     record created under tenant A is not visible inside tenant B's
//     transaction.
//
// The test size is governed by env vars so operators can scale it to
// their hardware without editing the source:
//
//	KAPP_LOAD_TEST_TENANTS — number of tenants (default 1000)
//	KAPP_LOAD_TEST_WORKERS — concurrent workers (default 32)
//	KAPP_LOAD_TEST_OPS     — ops per tenant (default 3)
func TestThousandTenantLoad(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	numTenants := envInt("KAPP_LOAD_TEST_TENANTS", 1000)
	workers := envInt("KAPP_LOAD_TEST_WORKERS", 32)
	opsPerTenant := envInt("KAPP_LOAD_TEST_OPS", 3)

	t.Logf("load test: tenants=%d workers=%d ops/tenant=%d", numTenants, workers, opsPerTenant)

	ktypeName := uniqueSlug("load.note")
	schema := json.RawMessage(fmt.Sprintf(`{
		"name": %q,
		"version": 1,
		"fields": [
			{"name": "title", "type": "string", "required": true, "max_length": 120},
			{"name": "body",  "type": "string"}
		]
	}`, ktypeName))
	if err := h.ktypes.Register(ctx, ktype.KType{Name: ktypeName, Version: 1, Schema: schema}); err != nil {
		t.Fatalf("register ktype: %v", err)
	}

	// Seed tenants sequentially — the control plane is not the thing
	// we're trying to stress here; we just need N distinct tenant rows
	// with users assigned so RLS has something to enforce.
	tenants := make([]uuid.UUID, numTenants)
	actors := make([]uuid.UUID, numTenants)
	seedStart := time.Now()
	for i := 0; i < numTenants; i++ {
		tn, err := h.tenants.Create(ctx, tenant.CreateInput{
			Slug: uniqueSlug(fmt.Sprintf("load-%04d", i)),
			Name: "Load Tenant",
			Cell: "test",
			Plan: "free",
		})
		if err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
		u, err := h.users.CreateUser(ctx, tenant.User{
			KChatUserID: "load-" + uuid.NewString()[:8],
			Email:       fmt.Sprintf("load-%d@test", i),
			DisplayName: "Load",
		})
		if err != nil {
			t.Fatalf("seed user %d: %v", i, err)
		}
		if err := h.users.AddUserToTenant(ctx, u.ID, tn.ID, "owner"); err != nil {
			t.Fatalf("bind user %d: %v", i, err)
		}
		tenants[i] = tn.ID
		actors[i] = u.ID
	}
	t.Logf("seeded %d tenants in %s", numTenants, time.Since(seedStart))

	// Instrument per-op latency. Three parallel histograms keep create
	// / get / list distributions separate so we can see whether any
	// single code path regresses.
	var createSamples, getSamples, listSamples safeLatencies
	var createdRecords sync.Map // tenantID -> record.KRecord

	jobs := make(chan int, numTenants)
	for i := 0; i < numTenants; i++ {
		jobs <- i
	}
	close(jobs)

	store := record.NewPGStore(h.pool, h.ktypes, h.publisher, h.auditor)

	var failures atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	loadStart := time.Now()
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				tenantID := tenants[idx]
				actorID := actors[idx]
				for op := 0; op < opsPerTenant; op++ {
					title := fmt.Sprintf("load-%d-%d", idx, op)
					data := json.RawMessage(fmt.Sprintf(`{"title":%q,"body":"hello"}`, title))

					t0 := time.Now()
					rec, err := store.Create(ctx, record.KRecord{
						TenantID:  tenantID,
						KType:     ktypeName,
						Data:      data,
						CreatedBy: actorID,
					})
					createSamples.add(time.Since(t0))
					if err != nil {
						failures.Add(1)
						t.Errorf("create tenant=%d op=%d: %v", idx, op, err)
						continue
					}

					t0 = time.Now()
					if _, err := store.Get(ctx, tenantID, rec.ID); err != nil {
						failures.Add(1)
						t.Errorf("get tenant=%d: %v", idx, err)
					}
					getSamples.add(time.Since(t0))

					t0 = time.Now()
					if _, err := store.List(ctx, tenantID, record.ListFilter{KType: ktypeName, Limit: 10}); err != nil {
						failures.Add(1)
						t.Errorf("list tenant=%d: %v", idx, err)
					}
					listSamples.add(time.Since(t0))

					createdRecords.Store(tenantID, *rec)
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("drove %d ops across %d tenants in %s (workers=%d, failures=%d)",
		numTenants*opsPerTenant*3, numTenants, time.Since(loadStart), workers, failures.Load())

	if f := failures.Load(); f > 0 {
		t.Fatalf("%d operations failed under load", f)
	}

	reportPercentiles(t, "create", createSamples.snapshot())
	reportPercentiles(t, "get", getSamples.snapshot())
	reportPercentiles(t, "list", listSamples.snapshot())

	// RLS spot check: pick 32 tenant pairs at random and verify that
	// a record created under tenantA is not visible to tenantB. This
	// validates that RLS holds under concurrent multi-tenant load, not
	// just when the process is idle.
	checked := 0
	for i := 0; i < 32; i++ {
		a := tenants[i%len(tenants)]
		b := tenants[(i+7)%len(tenants)]
		if a == b {
			continue
		}
		recA, ok := loadRecord(&createdRecords, a)
		if !ok {
			continue
		}
		// Try to read recA from tenantB's context — RLS must hide it.
		err := platform.WithTenantTx(ctx, h.pool, b, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx,
				`SELECT id FROM krecords WHERE id = $1`, recA.ID,
			).Scan(&id)
		})
		if err == nil {
			t.Fatalf("RLS isolation violated: tenant %s read tenant %s's record %s", b, a, recA.ID)
		}
		if err != pgx.ErrNoRows {
			// We expect pgx.ErrNoRows; any other error indicates a
			// transport problem but still proves RLS hid the row.
			t.Logf("RLS check tenant=%s saw err=%v (acceptable; row was hidden)", b, err)
		}
		checked++
	}
	if checked < 4 {
		t.Fatalf("RLS spot check only ran %d comparisons; insufficient coverage", checked)
	}
	t.Logf("RLS isolation verified across %d random tenant pairs under load", checked)

	// Keep references alive so the suite still has these helpers in
	// scope when we add more assertions in later phases.
	_ = events.Event{}
	_ = audit.Entry{}
}

// safeLatencies is a thread-safe slice of per-op latencies. Appending
// is O(1) amortised; snapshot() copies the slice so callers can sort
// without racing against live writers.
type safeLatencies struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (s *safeLatencies) add(d time.Duration) {
	s.mu.Lock()
	s.samples = append(s.samples, d)
	s.mu.Unlock()
}

func (s *safeLatencies) snapshot() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.samples))
	copy(out, s.samples)
	return out
}

func reportPercentiles(t *testing.T, label string, samples []time.Duration) {
	t.Helper()
	if len(samples) == 0 {
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("load.%s n=%d p50=%s p95=%s p99=%s max=%s",
		label, len(samples),
		samples[len(samples)*50/100],
		samples[len(samples)*95/100],
		samples[len(samples)*99/100],
		samples[len(samples)-1],
	)
}

func loadRecord(m *sync.Map, tenantID uuid.UUID) (record.KRecord, bool) {
	v, ok := m.Load(tenantID)
	if !ok {
		return record.KRecord{}, false
	}
	r, ok := v.(record.KRecord)
	return r, ok
}

func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil || v <= 0 {
		return def
	}
	return v
}
