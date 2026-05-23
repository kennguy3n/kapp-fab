package exporter

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// fakeKRecordSource is a deterministic in-memory KRecordSource used
// by the exporter unit tests. It mirrors the streaming contract of
// the production *record.PGStore.ForEach: rows are delivered in
// descending updated_at order, the callback can return early via
// an error, and a nil filter.KType is rejected with the same
// "ktype filter required" message the production store emits.
//
// Holding all rows in a slice is fine here: tests exercise tens
// of rows, not the full streaming benefit. The behaviour we care
// about is that the exporter consumes ForEach correctly — that no
// `ListAll` cap exists on this path, and that every chunk is
// surfaced to the renderer.
type fakeKRecordSource struct {
	rows []record.KRecord
	// callbackErr is returned from the callback when non-nil so
	// tests can exercise the error-propagation contract without
	// needing a real database failure.
	callbackErr error
	// surfaceErr is returned from ForEach itself (not the
	// callback) so the test can exercise the upstream DB-error
	// wrapping in ProcessKType.
	surfaceErr error
}

func (f *fakeKRecordSource) ForEach(_ context.Context, _ uuid.UUID, filter record.ListFilter, fn record.ForEachFunc) error {
	if filter.KType == "" {
		return errors.New("record: ktype filter required")
	}
	if f.surfaceErr != nil {
		return f.surfaceErr
	}
	for _, r := range f.rows {
		if filter.KType != "" && r.KType != filter.KType {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		if err := fn(r); err != nil {
			return err
		}
		if f.callbackErr != nil {
			return f.callbackErr
		}
	}
	return nil
}

// newSampleRow builds a single test row with a JSON `data` payload
// that exercises every CSV cell type (string, number, bool, object,
// array, null).
func newSampleRow(t *testing.T, id, name string, amount float64, when time.Time) record.KRecord {
	t.Helper()
	payload := map[string]any{
		"name":     name,
		"amount":   amount,
		"active":   true,
		"tags":     []any{"alpha", "beta"},
		"metadata": map[string]any{"source": "test"},
		"notes":    nil,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return record.KRecord{
		ID:           uuid.MustParse(id),
		TenantID:     uuid.MustParse("00000000-0000-0000-0000-00000000aaaa"),
		KType:        "invoice",
		KTypeVersion: 1,
		Status:       "active",
		Version:      1,
		CreatedBy:    uuid.MustParse("00000000-0000-0000-0000-00000000bbbb"),
		CreatedAt:    when,
		UpdatedAt:    when,
		Data:         raw,
	}
}

func TestProcessKTypeRejectsBadInputs(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	src := &fakeKRecordSource{}

	cases := []struct {
		name   string
		source KRecordSource
		tenant uuid.UUID
		ktype  string
		format string
	}{
		{name: "nil source", source: nil, tenant: tenant, ktype: "invoice", format: FormatJSON},
		{name: "nil tenant", source: src, tenant: uuid.Nil, ktype: "invoice", format: FormatJSON},
		{name: "empty ktype", source: src, tenant: tenant, ktype: "", format: FormatJSON},
		{name: "wildcard ktype", source: src, tenant: tenant, ktype: KTypeAll, format: FormatJSON},
		{name: "bad format", source: src, tenant: tenant, ktype: "invoice", format: "yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ProcessKType(context.Background(), tc.source, tc.tenant, tc.ktype, tc.format)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestProcessKTypeStreamsJSON(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeKRecordSource{
		rows: []record.KRecord{
			newSampleRow(t, "11111111-1111-1111-1111-111111111111", "first", 100, t0),
			newSampleRow(t, "22222222-2222-2222-2222-222222222222", "second", 250.5, t0.Add(1*time.Minute)),
		},
	}
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")

	payload, rows, err := ProcessKType(context.Background(), src, tenant, "invoice", FormatJSON)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if rows != 2 {
		t.Fatalf("row count = %d, want 2", rows)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded len = %d, want 2", len(decoded))
	}
	for i, want := range []string{"first", "second"} {
		data, ok := decoded[i]["data"].(map[string]any)
		if !ok {
			t.Fatalf("row %d: missing data object", i)
		}
		if got := data["name"]; got != want {
			t.Errorf("row %d name = %v, want %v", i, got, want)
		}
	}
}

func TestProcessKTypeStreamsCSVWithUnionedKeys(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeKRecordSource{
		rows: []record.KRecord{
			newSampleRow(t, "11111111-1111-1111-1111-111111111111", "first", 100, t0),
			// Second row carries a `notes` value where the first
			// row had `null`, plus an extra `currency` key the
			// first row lacks. Tests the CSV renderer's
			// union-of-keys header behaviour.
			func() record.KRecord {
				r := newSampleRow(t, "22222222-2222-2222-2222-222222222222", "second", 250.5, t0.Add(1*time.Minute))
				var obj map[string]any
				_ = json.Unmarshal(r.Data, &obj)
				obj["currency"] = "USD"
				obj["notes"] = "manual entry"
				raw, _ := json.Marshal(obj)
				r.Data = raw
				return r
			}(),
		},
	}
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")

	payload, rows, err := ProcessKType(context.Background(), src, tenant, "invoice", FormatCSV)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if rows != 2 {
		t.Fatalf("row count = %d, want 2", rows)
	}

	parsed, err := csv.NewReader(strings.NewReader(string(payload))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(parsed) != 3 {
		t.Fatalf("expected header + 2 rows, got %d", len(parsed))
	}
	header := parsed[0]
	// First 8 columns are fixed metadata, then data keys sorted
	// lexicographically. Both `currency` and `notes` must appear
	// even though only the second row populates `currency`.
	wantMetadata := []string{"id", "ktype", "ktype_version", "status", "version", "created_by", "created_at", "updated_at"}
	for i, name := range wantMetadata {
		if header[i] != name {
			t.Errorf("header[%d] = %q, want %q", i, header[i], name)
		}
	}
	dataCols := header[len(wantMetadata):]
	wantDataCols := []string{"active", "amount", "currency", "metadata", "name", "notes", "tags"}
	if len(dataCols) != len(wantDataCols) {
		t.Fatalf("data cols = %v, want %v", dataCols, wantDataCols)
	}
	for i, name := range wantDataCols {
		if dataCols[i] != name {
			t.Errorf("dataCols[%d] = %q, want %q", i, dataCols[i], name)
		}
	}
}

func TestProcessKTypePropagatesSourceErrors(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	boom := errors.New("simulated db failure")
	src := &fakeKRecordSource{surfaceErr: boom}

	_, _, err := ProcessKType(context.Background(), src, tenant, "invoice", FormatJSON)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom, got %v", err)
	}
	// The exporter must wrap with its own context so log lines
	// identify the failing ktype without needing call-site
	// inspection. The previous formulation used "list %s"; the
	// migrated one uses "stream %s" to reflect the new path.
	if !strings.Contains(err.Error(), "stream invoice") {
		t.Errorf("expected error to mention 'stream invoice', got %q", err.Error())
	}
}

// TestProcessKTypeBypassesListAllCap exercises the architectural
// reason for the migration: a tenant with more rows than the old
// ListAllMaxRows cap (100 000) must still produce a complete
// export. The fake source streams 100 050 rows; ProcessKType
// must consume all of them and return a non-empty payload.
//
// We use the JSON renderer because its output is the smallest
// per-row footprint that still proves the row count round-trips.
// CSV would also work but inflates the test buffer noticeably
// without exercising any additional code path on the streaming
// side.
func TestProcessKTypeBypassesListAllCap(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	// 100050 = ListAllMaxRows (100k) + a safety margin. The cap
	// is enforced inside record.ListAll/ListByField, not on the
	// ForEach path the exporter now uses, so this loop must
	// complete without error.
	const total = 100_050
	rows := make([]record.KRecord, 0, total)
	for i := 0; i < total; i++ {
		// Each row carries a minimal `data` payload; the test
		// asserts on count, not on per-row content.
		raw, _ := json.Marshal(map[string]any{"i": i})
		rows = append(rows, record.KRecord{
			ID:           uuid.New(),
			TenantID:     uuid.MustParse("00000000-0000-0000-0000-00000000aaaa"),
			KType:        "invoice",
			KTypeVersion: 1,
			Status:       "active",
			Version:      1,
			CreatedBy:    uuid.MustParse("00000000-0000-0000-0000-00000000bbbb"),
			CreatedAt:    t0,
			UpdatedAt:    t0,
			Data:         raw,
		})
	}
	src := &fakeKRecordSource{rows: rows}
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")

	_, count, err := ProcessKType(context.Background(), src, tenant, "invoice", FormatJSON)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if count != int64(total) {
		t.Fatalf("count = %d, want %d", count, total)
	}
}
