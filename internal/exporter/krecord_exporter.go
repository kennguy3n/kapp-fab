package exporter

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// KRecordSource is the minimum surface ProcessKType needs from the
// KRecord store. Defined here so the exporter does not pull in the
// full *record.PGStore interface and so unit tests can supply a
// lightweight fake.
//
// ForEach is the streaming iterator (Pillar A2) that walks every
// matching row exactly once under a snapshot ceiling, without the
// `ListAll` 100k row safety cap. The exporter is a *typed* caller
// that knows it wants the full set for a single KType — unlike
// anonymous callers the cap protects from accidental unbounded
// reads — so consuming via ForEach is the architecturally correct
// path: a tenant with >100k rows of one KType can still produce a
// complete export without hitting `ErrListAllExceedsCap`.
type KRecordSource interface {
	ForEach(ctx context.Context, tenantID uuid.UUID, filter record.ListFilter, fn record.ForEachFunc) error
}

// ProcessKType runs a per-KType export by streaming rows through
// ForEach and rendering them in the requested format. Returns the
// encoded payload alongside the row count so the worker can
// persist both in one Complete call.
//
// The caller is responsible for the surrounding job state machine
// (claim → run → complete). This function is pure: same inputs
// produce the same output, so a re-run after a transient worker
// crash never duplicates data.
//
// Memory note: the JSON and CSV renderers still build their output
// in memory because they emit a single payload that the worker
// writes back to the export_jobs row in one transaction. ForEach
// streams *out of the database* (one chunk at a time, snapshot
// ceiling, no 100k cap) while the in-process accumulation builds
// the final payload. The end-state memory profile is the same as
// the previous ListAll formulation — the win is that the cap that
// could surface as `ErrListAllExceedsCap` on large tenants no
// longer applies to this code path. Switching the worker to a
// streaming chunk-write protocol (write CSV row-by-row to an
// object-storage upload, finalise on the last chunk) is a future
// improvement tracked in the export pipeline backlog.
func ProcessKType(ctx context.Context, source KRecordSource, tenantID uuid.UUID, ktype, format string) ([]byte, int64, error) {
	if source == nil {
		return nil, 0, fmt.Errorf("%w: krecord source required", ErrInvalidInput)
	}
	if tenantID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	if ktype == "" || ktype == KTypeAll {
		return nil, 0, fmt.Errorf("%w: krecord exporter requires a concrete ktype", ErrInvalidInput)
	}
	if format != FormatCSV && format != FormatJSON {
		return nil, 0, fmt.Errorf("%w: format %q", ErrInvalidInput, format)
	}

	// Collect rows via ForEach. The previous ListAll formulation
	// returned a []KRecord directly; collecting into the same slice
	// here preserves the renderer signatures (renderCSV / renderJSON
	// already take []KRecord) while letting the database side
	// stream chunk-by-chunk without the 100k row safety cap.
	//
	// Defensive copy of r.Data: the documented ForEachFunc contract
	// (`internal/record/record.go`) says "the slice backing the
	// KRecord's Data is owned by the store and may be reused after
	// the callback returns, so any data that must outlive the
	// callback should be copied out." Today's foreachKeyset
	// implementation allocates a fresh page slice per chunk and pgx
	// v5 allocates fresh byte buffers per scan, so no reuse actually
	// occurs — but the exporter retains every KRecord beyond the
	// callback boundary to feed the renderers downstream, which
	// would silently corrupt the export the moment the store layer
	// switches to a buffer-pool scan (e.g. pgx.RowToStructByPos with
	// a reusable buffer). Copying r.Data inside the callback honours
	// the contract regardless of what the store does internally.
	var rows []record.KRecord
	if err := source.ForEach(ctx, tenantID, record.ListFilter{
		KType:  ktype,
		Status: "active",
	}, func(r record.KRecord) error {
		if r.Data != nil {
			cp := make(json.RawMessage, len(r.Data))
			copy(cp, r.Data)
			r.Data = cp
		}
		rows = append(rows, r)
		return nil
	}); err != nil {
		return nil, 0, fmt.Errorf("exporter: stream %s: %w", ktype, err)
	}

	switch format {
	case FormatCSV:
		return renderCSV(rows)
	case FormatJSON:
		return renderJSON(rows)
	default:
		// Unreachable: format validated above. The explicit branch
		// keeps the switch exhaustive for the linter and guards
		// against silent renderer additions that skip validation.
		return nil, 0, fmt.Errorf("%w: format %q", ErrInvalidInput, format)
	}
}

// renderJSON emits a top-level JSON array of {id, version, status,
// created_at, updated_at, data: {...}} objects. The shape mirrors
// the API list response so the export round-trips with the import
// pipeline (`internal/importer`).
func renderJSON(rows []record.KRecord) ([]byte, int64, error) {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var dataObj any
		if len(r.Data) > 0 {
			if err := json.Unmarshal(r.Data, &dataObj); err != nil {
				return nil, 0, fmt.Errorf("exporter: decode row %s data: %w", r.ID, err)
			}
		}
		out = append(out, map[string]any{
			"id":            r.ID,
			"ktype":         r.KType,
			"ktype_version": r.KTypeVersion,
			"status":        r.Status,
			"version":       r.Version,
			"created_by":    r.CreatedBy,
			"created_at":    r.CreatedAt.Format(time.RFC3339Nano),
			"updated_at":    r.UpdatedAt.Format(time.RFC3339Nano),
			"data":          dataObj,
		})
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, 0, fmt.Errorf("exporter: marshal json: %w", err)
	}
	return body, int64(len(rows)), nil
}

// renderCSV emits one row per KRecord with a stable column order:
// fixed metadata columns first, then the union of all top-level
// keys in the data JSON, sorted lexicographically. Nested values
// are JSON-encoded so the cell is round-trippable.
func renderCSV(rows []record.KRecord) ([]byte, int64, error) {
	dataKeys := map[string]struct{}{}
	decoded := make([]map[string]any, len(rows))
	for i, r := range rows {
		obj := map[string]any{}
		if len(r.Data) > 0 {
			if err := json.Unmarshal(r.Data, &obj); err != nil {
				return nil, 0, fmt.Errorf("exporter: decode row %s data: %w", r.ID, err)
			}
		}
		for k := range obj {
			dataKeys[k] = struct{}{}
		}
		decoded[i] = obj
	}
	keys := make([]string, 0, len(dataKeys))
	for k := range dataKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	// Fixed metadata columns match the JSON exporter (renderJSON)
	// so CSV and JSON dumps of the same KType carry the same
	// attribution fields and can round-trip through the importer.
	header := append([]string{
		"id", "ktype", "ktype_version", "status", "version", "created_by", "created_at", "updated_at",
	}, keys...)
	if err := w.Write(header); err != nil {
		return nil, 0, fmt.Errorf("exporter: write header: %w", err)
	}
	for i, r := range rows {
		row := []string{
			r.ID.String(),
			r.KType,
			fmt.Sprintf("%d", r.KTypeVersion),
			r.Status,
			fmt.Sprintf("%d", r.Version),
			r.CreatedBy.String(),
			r.CreatedAt.Format(time.RFC3339Nano),
			r.UpdatedAt.Format(time.RFC3339Nano),
		}
		for _, k := range keys {
			row = append(row, csvCell(decoded[i][k]))
		}
		if err := w.Write(row); err != nil {
			return nil, 0, fmt.Errorf("exporter: write row %s: %w", r.ID, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, 0, fmt.Errorf("exporter: flush csv: %w", err)
	}
	return buf.Bytes(), int64(len(rows)), nil
}

// csvCell renders a JSON-decoded value into a single CSV cell.
// Strings pass through verbatim; everything else is JSON-encoded so
// the cell is unambiguous when re-imported.
func csvCell(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}
