package platform

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// MeteringBufferConfig controls how aggressively the metering
// middleware batches increments before flushing them to the DB.
// Defaults picked for a steady-state of ~1000 req/s:
//
//   - BatchInterval 5s → at worst a crashed pod loses ~5s of
//     metering, which is acceptable for monthly billing granularity.
//   - BatchThreshold 500 → a hot tenant flushes early rather than
//     waiting the full 5s.
type MeteringBufferConfig struct {
	BatchInterval  time.Duration
	BatchThreshold int
}

// DefaultMeteringBufferConfig returns the config new callers should
// pass to NewMeteringBuffer.
func DefaultMeteringBufferConfig() MeteringBufferConfig {
	return MeteringBufferConfig{
		BatchInterval:  5 * time.Second,
		BatchThreshold: 500,
	}
}

// MeteringBuffer accumulates (tenant, metric) → delta pairs in
// memory and flushes them to the backing tenant.MeteringStore on a
// time- or count-based schedule. The buffer deliberately owns its
// own goroutine so the hot HTTP path pays only a map write +
// threshold check.
type MeteringBuffer struct {
	store *tenant.MeteringStore
	cfg   MeteringBufferConfig

	mu      sync.Mutex
	batch   map[meterKey]int64
	pending int

	stopCh chan struct{}
	nudge  chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

type meterKey struct {
	tenantID uuid.UUID
	metric   string
}

// NewMeteringBuffer constructs a buffer and starts its background
// flush loop. Callers must call Close on shutdown to drain pending
// increments; Close is safe to call multiple times.
func NewMeteringBuffer(store *tenant.MeteringStore, cfg MeteringBufferConfig) *MeteringBuffer {
	if cfg.BatchInterval <= 0 {
		cfg = DefaultMeteringBufferConfig()
	}
	if cfg.BatchThreshold <= 0 {
		cfg.BatchThreshold = DefaultMeteringBufferConfig().BatchThreshold
	}
	b := &MeteringBuffer{
		store:  store,
		cfg:    cfg,
		batch:  map[meterKey]int64{},
		stopCh: make(chan struct{}),
		nudge:  make(chan struct{}, 1),
		doneCh: make(chan struct{}),
	}
	go b.loop()
	return b
}

// Increment queues a delta. Never blocks on the DB; the background
// loop handles the actual write. A nil store turns Increment into a
// no-op so tests and local dev don't need a live DB.
func (b *MeteringBuffer) Increment(tenantID uuid.UUID, metric string, delta int64) {
	if b == nil || b.store == nil || tenantID == uuid.Nil || metric == "" || delta == 0 {
		return
	}
	b.mu.Lock()
	key := meterKey{tenantID: tenantID, metric: metric}
	b.batch[key] += delta
	b.pending++
	threshold := b.cfg.BatchThreshold
	b.mu.Unlock()
	if b.pending >= threshold {
		// Non-blocking nudge — a back-pressured flusher never
		// stalls the request path. nudge is buffered to 1, so
		// multiple threshold hits between flushes coalesce.
		select {
		case b.nudge <- struct{}{}:
		default:
		}
	}
}

// Close stops the background goroutine and flushes anything
// remaining in the buffer. Safe to call on a nil receiver.
func (b *MeteringBuffer) Close(ctx context.Context) {
	if b == nil {
		return
	}
	b.once.Do(func() {
		close(b.stopCh)
	})
	select {
	case <-b.doneCh:
	case <-ctx.Done():
		return
	}
}

func (b *MeteringBuffer) loop() {
	ticker := time.NewTicker(b.cfg.BatchInterval)
	defer ticker.Stop()
	defer close(b.doneCh)
	for {
		select {
		case <-ticker.C:
			b.flush(context.Background())
		case <-b.nudge:
			b.flush(context.Background())
		case <-b.stopCh:
			b.flush(context.Background())
			return
		}
	}
}

func (b *MeteringBuffer) flush(ctx context.Context) {
	b.mu.Lock()
	if len(b.batch) == 0 {
		b.mu.Unlock()
		return
	}
	toFlush := b.batch
	b.batch = map[meterKey]int64{}
	b.pending = 0
	b.mu.Unlock()
	for k, delta := range toFlush {
		if err := b.store.Increment(ctx, k.tenantID, k.metric, delta); err != nil {
			log.Printf("metering: flush tenant=%s metric=%s delta=%d: %v",
				k.tenantID, k.metric, delta, err)
		}
	}
}

// APICallMiddleware increments the api_calls metric once per
// request after the downstream handler runs. The write is buffered;
// the hot path cost is a mutex-protected map update.
func APICallMiddleware(buf *MeteringBuffer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if buf == nil {
				return
			}
			t := TenantFromContext(r.Context())
			if t == nil {
				return
			}
			buf.Increment(t.ID, tenant.MetricAPICalls, 1)
		})
	}
}
