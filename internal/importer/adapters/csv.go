// Package adapters contains source-system-specific implementations of
// the importer.Adapter surface. Each file handles Discover + Export for
// exactly one source family (CSV/JSON, Frappe REST, …) and emits
// NormalizedRow values into the pipeline's streaming callback.
package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// CSVConfig is the JSON shape expected in ImportJob.Config for CSV/JSON
// sources. Either `entities` (preferred, multi-sheet) is provided, or a
// single `entity`+`payload` pair is used for the common one-sheet case.
//
// The payload is supplied inline (base64/raw text) rather than a remote
// URL because the importer service runs in-cell and should never fan
// out to arbitrary URLs from inside RLS-protected handlers.
type CSVConfig struct {
	// Format is "csv" (default) or "json" (one JSON array of objects
	// per entity). Controls how Payload is decoded.
	Format string `json:"format,omitempty"`
	// Entity is the logical name the operator maps to a target KType.
	Entity string `json:"entity,omitempty"`
	// TargetKType provides a sensible default when the caller has not
	// submitted a mapping yet; the pipeline's resolveTarget still
	// prefers the explicit mapping when one exists.
	TargetKType string `json:"target_ktype,omitempty"`
	// Payload holds the raw file contents. For CSV, the first line is
	// treated as the header row.
	Payload string `json:"payload"`
	// IDField names the source-side unique identifier column so the
	// staging row can record an originating source id (optional).
	IDField string `json:"id_field,omitempty"`
	// Entities is the multi-sheet variant: one logical entity per
	// payload. When set, Entity/Payload/TargetKType on the parent are
	// ignored.
	Entities []CSVEntity `json:"entities,omitempty"`
}

// CSVEntity describes one sheet inside a multi-entity CSV/JSON config.
type CSVEntity struct {
	Entity      string `json:"entity"`
	TargetKType string `json:"target_ktype,omitempty"`
	Format      string `json:"format,omitempty"`
	Payload     string `json:"payload"`
	IDField     string `json:"id_field,omitempty"`
}

// CSVAdapter handles CSV and JSON-array payloads supplied inline on the
// import job config.
type CSVAdapter struct{}

// NewCSVAdapter returns the CSV/JSON source adapter.
func NewCSVAdapter() *CSVAdapter { return &CSVAdapter{} }

// SourceType discriminates the adapter for registry lookup.
func (*CSVAdapter) SourceType() string { return importer.SourceTypeCSV }

// JSONAdapter is a thin alias that registers the CSV adapter logic
// under the "json" source type so the wizard's JSON selection maps to
// a valid adapter. The underlying CSVAdapter already dispatches on the
// per-entity `format` field, so the behaviour is identical to the CSV
// path when callers pass `format: "json"` in their config.
type JSONAdapter struct{ CSVAdapter }

// NewJSONAdapter returns the JSON alias for the CSV/JSON adapter.
func NewJSONAdapter() *JSONAdapter { return &JSONAdapter{} }

// SourceType reports "json" so Pipeline.RegisterAdapter can file this
// instance under the json key without colliding with the CSV instance.
func (*JSONAdapter) SourceType() string { return importer.SourceTypeJSON }

// Discover parses the config, counts rows per entity, and stamps a
// SHA-256 checksum over the payload bytes so the reconciler can detect
// truncation in long-running uploads.
func (a *CSVAdapter) Discover(_ context.Context, raw json.RawMessage) (importer.DiscoverResult, error) {
	cfg, err := a.load(raw)
	if err != nil {
		return importer.DiscoverResult{}, err
	}
	result := importer.DiscoverResult{}
	for _, ent := range cfg.Entities {
		rows, err := parseEntity(ent)
		if err != nil {
			return importer.DiscoverResult{}, fmt.Errorf("discover entity %q: %w", ent.Entity, err)
		}
		result.Entities = append(result.Entities, importer.DiscoveredEntity{
			Name:     ent.Entity,
			RowCount: int64(len(rows)),
			TargetKT: ent.TargetKType,
			Checksum: checksumBytes([]byte(ent.Payload)),
		})
		result.TotalRows += int64(len(rows))
	}
	return result, nil
}

// Export streams each entity's rows into the emit callback. Rows carry
// their source-side id when IDField is configured.
func (a *CSVAdapter) Export(_ context.Context, raw json.RawMessage, emit func(importer.NormalizedRow) error) error {
	cfg, err := a.load(raw)
	if err != nil {
		return err
	}
	for _, ent := range cfg.Entities {
		rows, err := parseEntity(ent)
		if err != nil {
			return fmt.Errorf("export entity %q: %w", ent.Entity, err)
		}
		for _, row := range rows {
			sourceID := ""
			if ent.IDField != "" {
				if v, ok := row[ent.IDField]; ok {
					sourceID = fmt.Sprintf("%v", v)
				}
			}
			if err := emit(importer.NormalizedRow{
				Entity:   ent.Entity,
				SourceID: sourceID,
				Data:     row,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// load parses the config blob and normalizes the single-entity shortcut
// into the Entities slice so Discover/Export can be uniform.
func (a *CSVAdapter) load(raw json.RawMessage) (CSVConfig, error) {
	var cfg CSVConfig
	if len(raw) == 0 {
		return cfg, errors.New("csv: config required")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("csv: parse config: %w", err)
	}
	if len(cfg.Entities) == 0 {
		if cfg.Entity == "" || cfg.Payload == "" {
			return cfg, errors.New("csv: entity + payload (or entities[]) required")
		}
		cfg.Entities = []CSVEntity{{
			Entity:      cfg.Entity,
			TargetKType: cfg.TargetKType,
			Format:      cfg.Format,
			Payload:     cfg.Payload,
			IDField:     cfg.IDField,
		}}
	}
	for i := range cfg.Entities {
		if cfg.Entities[i].Format == "" {
			cfg.Entities[i].Format = "csv"
		}
	}
	return cfg, nil
}

// parseEntity parses a single entity's payload into a slice of
// map[string]any rows. `csv` uses encoding/csv; `json` expects an array
// of objects.
func parseEntity(ent CSVEntity) ([]map[string]any, error) {
	switch strings.ToLower(ent.Format) {
	case "json":
		var rows []map[string]any
		if err := json.Unmarshal([]byte(ent.Payload), &rows); err != nil {
			return nil, err
		}
		return rows, nil
	case "", "csv":
		reader := csv.NewReader(strings.NewReader(ent.Payload))
		reader.TrimLeadingSpace = true
		header, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return nil, err
		}
		var rows []map[string]any
		for {
			rec, err := reader.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, err
			}
			row := make(map[string]any, len(header))
			for i, col := range header {
				if i < len(rec) {
					row[col] = rec[i]
				}
			}
			rows = append(rows, row)
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("csv: unsupported format %q", ent.Format)
	}
}

// checksumBytes returns the hex SHA-256 digest of the input bytes.
// Used to stamp a stable checksum on each entity for reconciliation.
func checksumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
