// Package loadtest provisions a large tenant fleet and drives concurrent
// CRUD + finance posting traffic through it so operators can measure
// p50 / p95 / p99 latency, error rate, and connection pool utilisation
// against a real PostgreSQL instance.
//
// Unlike the `loadtest`-tagged TestThousandTenantLoad in
// internal/integrationtest/load_test.go (which only exercises generic
// KRecord CRUD), this harness also posts balanced journal entries
// against the ledger store so the report reflects a mixed workload:
// record updates from CRM + inventory plus finance postings from AR /
// AP pipelines. It is intended to be invoked from a standalone binary
// (see cmd/kapp-loadtest) or from an ad-hoc `go test` so the fleet
// size can be scaled to the target cell's capacity.
package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// Config controls the load-test shape. Zero-values fall back to
// production-like defaults (1000 tenants, 32 workers, 5 CRUD ops and
// 2 ledger posts per tenant).
type Config struct {
	Tenants            int
	Workers            int
	CRUDOpsPerTenant   int
	LedgerOpsPerTenant int
	KTypeName          string
}

// Result is the summary emitted after a run. Latencies are reported
// per operation family so callers can see whether CRUD or finance
// postings regressed in isolation.
type Result struct {
	Tenants          int
	Workers          int
	TotalOperations  int64
	Failures         int64
	SeedDuration     time.Duration
	RunDuration      time.Duration
	Create           LatencySummary
	Get              LatencySummary
	List             LatencySummary
	PostJournal      LatencySummary
	PoolMaxConns     int32
	PoolPeakConns    int32
	PoolAcquiredPeak int32
}

// LatencySummary captures p50 / p95 / p99 / max of a latency sample
// set. All values are returned even when the sample set is empty so
// the result row is stable to print.
type LatencySummary struct {
	Count int
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Max   time.Duration
}

// Run executes one pass of the harness and returns the Result.
// Callers provide the app pool (the same RLS-enforcing pool the API
// gateway uses) so the numbers reflect the path every handler pays.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) (*Result, error) {
	cfg = cfg.withDefaults()

	tenantSvc := tenant.NewPGStore(pool)
	users := tenant.NewUserStore(pool)
	cache := platform.NewLRUCache(4096, time.Minute)
	registry := ktype.NewPGRegistry(pool, cache)
	publisher := events.NewPGPublisher(pool)
	auditor := audit.NewPGLogger(pool)
	records := record.NewPGStore(pool, registry, publisher, auditor)
	ledgerStore := ledger.NewPGStore(pool, publisher, auditor)

	// Register the synthetic KType the workers use so Create/List
	// doesn't block on the registry for every tenant.
	schema := json.RawMessage(fmt.Sprintf(`{
		"name": %q,
		"version": 1,
		"fields": [
			{"name": "title", "type": "string", "required": true, "max_length": 120},
			{"name": "body",  "type": "string"}
		]
	}`, cfg.KTypeName))
	if err := registry.Register(ctx, ktype.KType{Name: cfg.KTypeName, Version: 1, Schema: schema}); err != nil {
		return nil, fmt.Errorf("register ktype: %w", err)
	}

	seedStart := time.Now()
	tenants := make([]uuid.UUID, cfg.Tenants)
	actors := make([]uuid.UUID, cfg.Tenants)
	for i := 0; i < cfg.Tenants; i++ {
		tn, err := tenantSvc.Create(ctx, tenant.CreateInput{
			Slug: fmt.Sprintf("lt-%s", uuid.NewString()[:8]),
			Name: "LoadTest",
			Cell: "loadtest",
			Plan: "free",
		})
		if err != nil {
			return nil, fmt.Errorf("seed tenant %d: %w", i, err)
		}
		u, err := users.CreateUser(ctx, tenant.User{
			KChatUserID: "lt-" + uuid.NewString()[:8],
			Email:       fmt.Sprintf("lt-%d@loadtest", i),
			DisplayName: "LoadTest",
		})
		if err != nil {
			return nil, fmt.Errorf("seed user %d: %w", i, err)
		}
		if err := users.AddUserToTenant(ctx, u.ID, tn.ID, "owner"); err != nil {
			return nil, fmt.Errorf("bind user %d: %w", i, err)
		}
		if err := seedMinimalAccounts(ctx, ledgerStore, tn.ID); err != nil {
			return nil, fmt.Errorf("seed accounts %d: %w", i, err)
		}
		tenants[i] = tn.ID
		actors[i] = u.ID
	}
	seedDur := time.Since(seedStart)

	var createS, getS, listS, postS safeLatencies

	jobs := make(chan int, cfg.Tenants)
	for i := 0; i < cfg.Tenants; i++ {
		jobs <- i
	}
	close(jobs)

	var failures atomic.Int64
	var totalOps atomic.Int64
	var peakAcquired atomic.Int32
	var peakTotal atomic.Int32

	stopSampler := make(chan struct{})
	var samplerDone sync.WaitGroup
	samplerDone.Add(1)
	go func() {
		defer samplerDone.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-ticker.C:
				s := pool.Stat()
				if a := s.AcquiredConns(); a > peakAcquired.Load() {
					peakAcquired.Store(a)
				}
				if tc := s.TotalConns(); tc > peakTotal.Load() {
					peakTotal.Store(tc)
				}
			}
		}
	}()

	runStart := time.Now()
	var wg sync.WaitGroup
	wg.Add(cfg.Workers)
	for w := 0; w < cfg.Workers; w++ {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				tenantID := tenants[idx]
				actorID := actors[idx]
				for op := 0; op < cfg.CRUDOpsPerTenant; op++ {
					title := fmt.Sprintf("lt-%d-%d", idx, op)
					data := json.RawMessage(fmt.Sprintf(`{"title":%q,"body":"hi"}`, title))
					t0 := time.Now()
					rec, err := records.Create(ctx, record.KRecord{
						TenantID:  tenantID,
						KType:     cfg.KTypeName,
						Data:      data,
						CreatedBy: actorID,
					})
					createS.add(time.Since(t0))
					totalOps.Add(1)
					if err != nil {
						failures.Add(1)
						continue
					}
					t0 = time.Now()
					if _, err := records.Get(ctx, tenantID, rec.ID); err != nil {
						failures.Add(1)
					}
					getS.add(time.Since(t0))
					totalOps.Add(1)
					t0 = time.Now()
					if _, err := records.List(ctx, tenantID, record.ListFilter{KType: cfg.KTypeName, Limit: 10}); err != nil {
						failures.Add(1)
					}
					listS.add(time.Since(t0))
					totalOps.Add(1)
				}
				for op := 0; op < cfg.LedgerOpsPerTenant; op++ {
					t0 := time.Now()
					_, err := ledgerStore.PostJournalEntry(ctx, ledger.JournalEntry{
						TenantID:  tenantID,
						Memo:      fmt.Sprintf("lt-post-%d-%d", idx, op),
						PostedAt:  time.Now(),
						CreatedBy: actorID,
						Lines: []ledger.JournalLine{
							{AccountCode: "1000", Debit: decimal.NewFromInt(10), Currency: "USD"},
							{AccountCode: "4000", Credit: decimal.NewFromInt(10), Currency: "USD"},
						},
					})
					postS.add(time.Since(t0))
					totalOps.Add(1)
					if err != nil {
						failures.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	runDur := time.Since(runStart)
	close(stopSampler)
	samplerDone.Wait()

	return &Result{
		Tenants:          cfg.Tenants,
		Workers:          cfg.Workers,
		TotalOperations:  totalOps.Load(),
		Failures:         failures.Load(),
		SeedDuration:     seedDur,
		RunDuration:      runDur,
		Create:           summarize(createS.snapshot()),
		Get:              summarize(getS.snapshot()),
		List:             summarize(listS.snapshot()),
		PostJournal:      summarize(postS.snapshot()),
		PoolMaxConns:     pool.Config().MaxConns,
		PoolPeakConns:    peakTotal.Load(),
		PoolAcquiredPeak: peakAcquired.Load(),
	}, nil
}

// String renders the result in a one-block log-friendly layout so
// operators can eyeball p95/p99 deltas across runs.
func (r *Result) String() string {
	return fmt.Sprintf(
		"loadtest: tenants=%d workers=%d ops=%d failures=%d seed=%s run=%s\n"+
			"  create  n=%d p50=%s p95=%s p99=%s max=%s\n"+
			"  get     n=%d p50=%s p95=%s p99=%s max=%s\n"+
			"  list    n=%d p50=%s p95=%s p99=%s max=%s\n"+
			"  post_je n=%d p50=%s p95=%s p99=%s max=%s\n"+
			"  pool    max_conns=%d peak_conns=%d peak_acquired=%d",
		r.Tenants, r.Workers, r.TotalOperations, r.Failures, r.SeedDuration, r.RunDuration,
		r.Create.Count, r.Create.P50, r.Create.P95, r.Create.P99, r.Create.Max,
		r.Get.Count, r.Get.P50, r.Get.P95, r.Get.P99, r.Get.Max,
		r.List.Count, r.List.P50, r.List.P95, r.List.P99, r.List.Max,
		r.PostJournal.Count, r.PostJournal.P50, r.PostJournal.P95, r.PostJournal.P99, r.PostJournal.Max,
		r.PoolMaxConns, r.PoolPeakConns, r.PoolAcquiredPeak,
	)
}

func (c Config) withDefaults() Config {
	if c.Tenants <= 0 {
		c.Tenants = 1000
	}
	if c.Workers <= 0 {
		c.Workers = 32
	}
	if c.CRUDOpsPerTenant <= 0 {
		c.CRUDOpsPerTenant = 5
	}
	if c.LedgerOpsPerTenant <= 0 {
		c.LedgerOpsPerTenant = 2
	}
	if c.KTypeName == "" {
		c.KTypeName = "loadtest.note"
	}
	return c
}

func seedMinimalAccounts(ctx context.Context, store *ledger.PGStore, tenantID uuid.UUID) error {
	for _, a := range []ledger.Account{
		{TenantID: tenantID, Code: "1000", Name: "Cash", Type: ledger.AccountTypeAsset, Active: true},
		{TenantID: tenantID, Code: "4000", Name: "Sales", Type: ledger.AccountTypeRevenue, Active: true},
	} {
		if _, err := store.CreateAccount(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// safeLatencies is a thread-safe slice of per-op latencies.
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

func summarize(samples []time.Duration) LatencySummary {
	if len(samples) == 0 {
		return LatencySummary{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return LatencySummary{
		Count: len(samples),
		P50:   samples[len(samples)*50/100],
		P95:   samples[len(samples)*95/100],
		P99:   samples[len(samples)*99/100],
		Max:   samples[len(samples)-1],
	}
}
