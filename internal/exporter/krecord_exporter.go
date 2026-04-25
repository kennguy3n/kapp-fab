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

// KRecordLister is the minimum surface ProcessKType needs from the
// KRecord store. Defined here so the exporter does not pull in the
// full *record.PGStore interface and so unit tests can supply a
// lightweight fake.
type KRecordLister interface {
	ListAll(ctx context.Context, tenantID uuid.UUID, filter record.ListFilter) ([]record.KRecord, error)
}

// ProcessKType runs a per-KType export by paging through ListAll and
// rendering rows in the requested format. Returns the encoded
// payload alongside the row count so the worker can persist both
// in one Complete call.
//
// The caller is responsible for the surrounding job state machine
// (claim → run → complete). This function is pure: same inputs
// produce the same output, so a re-run after a transient worker
// crash never duplicates data.
func ProcessKType(ctx context.Context, lister KRecordLister, tenantID uuid.UUID, ktype, format string) ([]byte, int64, error) {
	if lister == nil {
		return nil, 0, fmt.Errorf("%w: krecord lister required", ErrInvalidInput)
	}
	if tenantID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	if ktype == "" || ktype == KTypeAll {
		return nil, 0, fmt.Errorf("%w: krecord exporter requires a concrete ktype", ErrInvalidInput)
	}
	rows, err := lister.ListAll(ctx, tenantID, record.ListFilter{
		KType:  ktype,
		Status: "active",
	})
	if err != nil {
		return nil, 0, fmt.Errorf("exporter: list %s: %w", ktype, err)
	}
	switch format {
	case FormatCSV:
		return renderCSV(rows)
	case FormatJSON:
		return renderJSON(rows)
	default:
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
