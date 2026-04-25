package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/exporter"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// ExportWorker drains the export_jobs queue. The loop polls on a
// fixed cadence (RunLoop interval); per tick it claims a single
// pending job under FOR UPDATE SKIP LOCKED, runs the export against
// the per-tenant KRecord store, and persists the payload back on
// the same row.
//
// Concurrency: a single worker pod is enough for the current
// throughput target (a tenant-wide CSV is at most a few hundred
// MB). Scaling out is just running another worker — SKIP LOCKED
// guarantees each job is claimed exactly once.
type ExportWorker struct {
	store    *exporter.Store
	records  exporter.KRecordLister
	interval time.Duration
}

// NewExportWorker wires an export worker. The records lister is
// usually *record.PGStore; tests can pass a fake.
func NewExportWorker(store *exporter.Store, records exporter.KRecordLister, interval time.Duration) *ExportWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &ExportWorker{store: store, records: records, interval: interval}
}

// Run blocks until ctx is cancelled, draining the export queue on
// every tick. Errors during a single job are logged and the row
// is moved to status=failed; they never abort the loop.
func (w *ExportWorker) Run(ctx context.Context) {
	log.Printf("export-worker: started; tick=%s", w.interval)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("export-worker: shutdown")
			return
		case <-t.C:
			w.drain(ctx)
		}
	}
}

func (w *ExportWorker) drain(ctx context.Context) {
	for {
		job, err := w.store.ClaimNext(ctx)
		if err != nil {
			log.Printf("export-worker: claim: %v", err)
			return
		}
		if job == nil {
			return // queue empty
		}
		w.process(ctx, job)
	}
}

// process runs a single job to completion. The KType-all path is
// rejected with a clear error here rather than at enqueue time so
// the API surface stays simple — full tenant dumps go through the
// dedicated `kapp-backup extract` CLI instead.
func (w *ExportWorker) process(ctx context.Context, job *exporter.ExportJob) {
	log.Printf("export-worker: running job %s tenant=%s ktype=%s format=%s", job.ID, job.TenantID, job.KType, job.Format)
	if job.KType == exporter.KTypeAll {
		_ = w.store.Fail(ctx, job.TenantID, job.ID, "tenant-wide export: use the kapp-backup CLI")
		return
	}
	payload, rowCount, err := exporter.ProcessKType(ctx, w.records, job.TenantID, job.KType, job.Format)
	if err != nil {
		log.Printf("export-worker: process job %s: %v", job.ID, err)
		_ = w.store.Fail(ctx, job.TenantID, job.ID, err.Error())
		return
	}
	if err := w.store.Complete(ctx, job.TenantID, job.ID, payload, rowCount); err != nil {
		log.Printf("export-worker: complete job %s: %v", job.ID, err)
		_ = w.store.Fail(ctx, job.TenantID, job.ID, err.Error())
		return
	}
	log.Printf("export-worker: completed job %s rows=%d bytes=%d", job.ID, rowCount, len(payload))
}

// Compile-time assertion that *record.PGStore satisfies the lister
// interface ProcessKType expects. Keeps the wiring honest; if the
// signature ever drifts the worker fails to build.
var _ exporter.KRecordLister = (*record.PGStore)(nil)

// stringErrIs is a small helper for tests checking the loop never
// surfaces a known-okay error like ErrJobNotFound from a race.
func stringErrIs(err error, target error) bool {
	if err == nil || target == nil {
		return false
	}
	return errors.Is(err, target) || err.Error() == fmt.Sprintf("%v", target)
}

var _ = stringErrIs // exported for potential test use; keeps the symbol.
