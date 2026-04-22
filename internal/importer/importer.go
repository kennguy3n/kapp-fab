// Package importer implements the Phase F data onboarding pipeline:
// Discover → Export → Normalize → Map → Validate → Stage → Reconcile →
// Accept → Cutover. The pipeline is tenant-scoped end to end: every
// stage runs inside platform.WithTenantTx so RLS on `import_jobs` and
// `import_staging` is enforced and per-tenant quotas are honored.
//
// The core type is ImportJob — a persistent state machine whose
// status field tracks the current pipeline stage. Pipeline walks a
// job through its stages by delegating to a source Adapter (CSV,
// Frappe REST, …) for the early stages and to the generic record
// store for the final Accept stage.
package importer

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Stage names mirror the `import_jobs.status` CHECK constraint in
// migrations/000008_importer.sql. Callers flip through them in order;
// the terminal values are `completed` and `failed`.
const (
	StagePending      = "pending"
	StageDiscovering  = "discovering"
	StageExporting    = "exporting"
	StageNormalizing  = "normalizing"
	StageMapping      = "mapping"
	StageValidating   = "validating"
	StageStaging      = "staging"
	StageReconciling  = "reconciling"
	StageAccepting    = "accepting"
	StageCuttingOver  = "cutting_over"
	StageCompleted    = "completed"
	StageFailed       = "failed"
)

// Source types recognized by the HTTP surface and the adapter registry.
const (
	SourceTypeCSV    = "csv"
	SourceTypeJSON   = "json"
	SourceTypeFrappe = "frappe"
)

// StagingStatus values for individual rows in `import_staging`.
const (
	StagingPending  = "pending"
	StagingValid    = "valid"
	StagingInvalid  = "invalid"
	StagingImported = "imported"
)

// ImportJob mirrors one row in the `import_jobs` table. `Config`,
// `Mapping`, `Progress`, `Errors`, and `Reconciliation` are opaque JSON
// documents; higher-level code typed-decodes them as needed.
type ImportJob struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	SourceType     string          `json:"source_type"`
	Status         string          `json:"status"`
	Config         json.RawMessage `json:"config"`
	Mapping        json.RawMessage `json:"mapping"`
	Progress       json.RawMessage `json:"progress"`
	Errors         json.RawMessage `json:"errors"`
	Reconciliation json.RawMessage `json:"reconciliation"`
	CreatedBy      uuid.UUID       `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

// StagingRow mirrors one row in the `import_staging` table.
type StagingRow struct {
	ID                int64           `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	JobID             uuid.UUID       `json:"job_id"`
	SourceType        string          `json:"source_type"`
	SourceID          string          `json:"source_id,omitempty"`
	TargetKType       string          `json:"target_ktype"`
	Data              json.RawMessage `json:"data"`
	ValidationErrors  json.RawMessage `json:"validation_errors"`
	Status            string          `json:"status"`
	ImportedRecordID  *uuid.UUID      `json:"imported_record_id,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// ValidationError is one structured row-level error surfaced by the
// validator. The collection is stored in `import_staging.validation_errors`
// as a JSON array so the UI can render a row-level error report.
type ValidationError struct {
	Field   string `json:"field,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Reconciliation is the summary produced by the reconciler: total rows
// read from source, total staged, totals that must match across the
// two, and an optional checksum for numeric columns (e.g. invoice
// totals). Discrepancies are surfaced as a list of messages so the
// operator sees exactly which aggregates failed.
type Reconciliation struct {
	SourceCount     int64             `json:"source_count"`
	StagedCount     int64             `json:"staged_count"`
	ValidCount      int64             `json:"valid_count"`
	InvalidCount    int64             `json:"invalid_count"`
	Checksums       map[string]string `json:"checksums,omitempty"`
	Discrepancies   []string          `json:"discrepancies,omitempty"`
}

// ErrJobNotFound is returned when the requested import job does not
// exist in the caller's tenant.
var ErrJobNotFound = errors.New("importer: job not found")

// ErrInvalidTransition is returned when a caller tries to advance a
// job to a stage that is not reachable from its current state.
var ErrInvalidTransition = errors.New("importer: invalid stage transition")
