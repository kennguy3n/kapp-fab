package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// Adapter is the source-system-specific surface the pipeline calls into
// during Discover → Export → Normalize. Each adapter is responsible for
// turning whatever the source speaks (CSV rows, Frappe DocType pages)
// into a slice of NormalizedRow instances that the mapping + staging
// stages can consume.
//
// Adapters MUST respect the caller's tenant context — anything they
// stash in the pipeline's staging table needs to carry the invoking
// tenant's id so RLS is authoritative.
type Adapter interface {
	// SourceType returns the discriminator stored in `import_jobs.source_type`.
	SourceType() string
	// Discover inventories the source entities (e.g. DocTypes, CSV
	// headers) and returns a per-entity count + optional checksum so
	// the reconciler can compare against the staged output. It must
	// not mutate source data.
	Discover(ctx context.Context, config json.RawMessage) (DiscoverResult, error)
	// Export pulls the actual rows. Implementations stream results via
	// the emit callback so that pagination and memory use stay bounded.
	Export(ctx context.Context, config json.RawMessage, emit func(NormalizedRow) error) error
}

// DiscoverResult is the adapter's snapshot of what the source looks
// like: total rows per entity + a stable checksum so the reconciler
// can detect silent truncation.
type DiscoverResult struct {
	Entities  []DiscoveredEntity `json:"entities"`
	TotalRows int64              `json:"total_rows"`
	Notes     []string           `json:"notes,omitempty"`
}

// DiscoveredEntity is one source-side collection (DocType, CSV sheet).
type DiscoveredEntity struct {
	Name      string   `json:"name"`
	RowCount  int64    `json:"row_count"`
	Fields    []string `json:"fields,omitempty"`
	Checksum  string   `json:"checksum,omitempty"`
	TargetKT  string   `json:"target_ktype,omitempty"`
}

// NormalizedRow is the intermediate shape the pipeline stages on.
// `Entity` is the source collection (DocType name, CSV sheet); `Data`
// is the already-normalized JSON that should land in
// `import_staging.data`; `SourceID` is the source-side identifier.
type NormalizedRow struct {
	Entity   string
	SourceID string
	Data     map[string]any
}

// Pipeline coordinates the full Discover → … → Cutover sequence. It
// does NOT own any goroutines — callers drive it stage by stage so an
// import can be paused between stages for operator review (the
// canonical flow: Discover+Export+Stage runs on POST /imports,
// Validate runs on POST /imports/{id}/validate, Accept runs on
// POST /imports/{id}/accept).
type Pipeline struct {
	adapters  map[string]Adapter
	jobs      *JobStore
	staging   *StagingStore
	validator *Validator
	recon     *Reconciler
	records   *record.PGStore
}

// NewPipeline wires the orchestrator. The record store is used by the
// final Accept stage to promote staged rows into the live `krecords`
// table.
func NewPipeline(
	jobs *JobStore,
	staging *StagingStore,
	validator *Validator,
	recon *Reconciler,
	records *record.PGStore,
) *Pipeline {
	return &Pipeline{
		adapters:  make(map[string]Adapter),
		jobs:      jobs,
		staging:   staging,
		validator: validator,
		recon:     recon,
		records:   records,
	}
}

// RegisterAdapter adds an adapter under its SourceType discriminator.
// Duplicate registrations overwrite to keep the API idempotent.
func (p *Pipeline) RegisterAdapter(a Adapter) {
	p.adapters[a.SourceType()] = a
}

// StartStaging drives the Discover → Export → Normalize → Stage
// sub-pipeline in one call. The job transitions through
// discovering → exporting → normalizing → mapping → staging and
// stops in `staging` state waiting for the operator to submit a
// field-mapping or trigger validation.
//
// Returns the number of staged rows + the discovery summary that the
// reconciler will read later.
func (p *Pipeline) StartStaging(ctx context.Context, job *ImportJob) (int64, DiscoverResult, error) {
	adapter, ok := p.adapters[job.SourceType]
	if !ok {
		return 0, DiscoverResult{}, fmt.Errorf("importer: no adapter for %q", job.SourceType)
	}

	// Stage 1: Discover.
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageDiscovering, nil, nil); err != nil {
		return 0, DiscoverResult{}, err
	}
	disco, err := adapter.Discover(ctx, job.Config)
	if err != nil {
		p.fail(ctx, job, fmt.Errorf("discover: %w", err))
		return 0, DiscoverResult{}, err
	}

	// Stage 2+3: Export + Normalize, streamed into staging.
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageExporting, marshalProgress(map[string]any{"source": map[string]any{"count": disco.TotalRows}}), nil); err != nil {
		return 0, DiscoverResult{}, err
	}
	var staged int64
	err = adapter.Export(ctx, job.Config, func(row NormalizedRow) error {
		target := resolveTarget(job, row)
		if target == "" {
			// Without a mapping we cannot stage. Persist the row
			// anyway so the validator can surface a useful error.
			target = row.Entity
		}
		data, mErr := json.Marshal(row.Data)
		if mErr != nil {
			return mErr
		}
		_, sErr := p.staging.Insert(ctx, StagingRow{
			TenantID:    job.TenantID,
			JobID:       job.ID,
			SourceType:  job.SourceType,
			SourceID:    row.SourceID,
			TargetKType: target,
			Data:        data,
			Status:      StagingPending,
		})
		if sErr != nil {
			return sErr
		}
		staged++
		return nil
	})
	if err != nil {
		p.fail(ctx, job, fmt.Errorf("export: %w", err))
		return staged, disco, err
	}

	// Stage 4: Mapping — transient bookkeeping state; the operator
	// fills in the mapping next via UpdateMapping before validation.
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageStaging, marshalProgress(map[string]any{
		"source":  map[string]any{"count": disco.TotalRows, "entities": disco.Entities},
		"staging": map[string]any{"count": staged},
	}), nil); err != nil {
		return staged, disco, err
	}
	return staged, disco, nil
}

// Validate walks every staging row for the job, classifies it as
// `valid` or `invalid` via the configured Validator, and transitions
// the job to `reconciling` on completion.
func (p *Pipeline) Validate(ctx context.Context, job *ImportJob) (validCount, invalidCount int64, err error) {
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageValidating, job.Progress, nil); err != nil {
		return 0, 0, err
	}
	offset := 0
	for {
		rows, err := p.staging.ListByJob(ctx, job.TenantID, job.ID, StagingPending, 200, offset)
		if err != nil {
			return validCount, invalidCount, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			errs, err := p.validator.ValidateRow(ctx, row)
			if err != nil {
				return validCount, invalidCount, err
			}
			status := StagingValid
			if len(errs) > 0 {
				status = StagingInvalid
				invalidCount++
			} else {
				validCount++
			}
			if err := p.staging.MarkValidated(ctx, job.TenantID, row.ID, status, errs); err != nil {
				return validCount, invalidCount, err
			}
		}
		if len(rows) < 200 {
			break
		}
		offset += len(rows)
	}
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageReconciling, marshalProgress(map[string]any{
		"valid":   validCount,
		"invalid": invalidCount,
	}), nil); err != nil {
		return validCount, invalidCount, err
	}
	return validCount, invalidCount, nil
}

// Reconcile compares staged row counts to the adapter's discovery
// summary and persists a Reconciliation blob on the job.
func (p *Pipeline) Reconcile(ctx context.Context, job *ImportJob, source SourceSummary) (Reconciliation, error) {
	rec, err := p.recon.Reconcile(ctx, *job, source)
	if err != nil {
		return Reconciliation{}, err
	}
	if _, err := p.jobs.UpdateReconciliation(ctx, job.TenantID, job.ID, rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// Accept promotes every `valid` staging row to a live KRecord. Invalid
// rows are skipped; the operator is expected to fix the source and
// rerun the import. On success the job transitions through
// accepting → cutting_over → completed.
func (p *Pipeline) Accept(ctx context.Context, job *ImportJob, actor uuid.UUID) (int64, error) {
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageAccepting, job.Progress, nil); err != nil {
		return 0, err
	}
	imported := int64(0)
	offset := 0
	for {
		rows, err := p.staging.ListByJob(ctx, job.TenantID, job.ID, StagingValid, 200, offset)
		if err != nil {
			return imported, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			rec, err := p.records.Create(ctx, record.KRecord{
				TenantID:  row.TenantID,
				KType:     row.TargetKType,
				Data:      row.Data,
				CreatedBy: actor,
			})
			if err != nil {
				return imported, fmt.Errorf("importer: accept row %d: %w", row.ID, err)
			}
			if err := p.staging.MarkImported(ctx, row.TenantID, row.ID, rec.ID); err != nil {
				return imported, err
			}
			imported++
		}
		if len(rows) < 200 {
			break
		}
		offset += len(rows)
	}
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageCuttingOver, marshalProgress(map[string]any{"imported": imported}), nil); err != nil {
		return imported, err
	}
	if _, err := p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageCompleted, marshalProgress(map[string]any{"imported": imported}), nil); err != nil {
		return imported, err
	}
	return imported, nil
}

// fail marks the job failed and records the error in the `errors`
// column. Used by the stage helpers when an adapter or validator
// returns a hard error.
func (p *Pipeline) fail(ctx context.Context, job *ImportJob, cause error) {
	blob, _ := json.Marshal([]map[string]any{{"message": cause.Error()}})
	_, _ = p.jobs.UpdateStatus(ctx, job.TenantID, job.ID, StageFailed, job.Progress, blob)
}

// marshalProgress is a tiny helper so callers don't have to juggle
// json.Marshal + error handling for the Progress column.
func marshalProgress(payload map[string]any) json.RawMessage {
	if payload == nil {
		return json.RawMessage("{}")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// resolveTarget picks the target KType for a normalized row. The
// operator-submitted mapping wins; otherwise adapters can carry a
// default via the row's entity label.
func resolveTarget(job *ImportJob, row NormalizedRow) string {
	var mapping struct {
		Entities map[string]struct {
			TargetKType string `json:"target_ktype"`
		} `json:"entities"`
	}
	if len(job.Mapping) > 0 {
		if err := json.Unmarshal(job.Mapping, &mapping); err == nil {
			if m, ok := mapping.Entities[row.Entity]; ok && m.TargetKType != "" {
				return m.TargetKType
			}
		}
	}
	return ""
}

// ErrAdapterMissing is surfaced when the HTTP layer asks the pipeline
// to run a source type it doesn't know about.
var ErrAdapterMissing = errors.New("importer: adapter missing for source type")
