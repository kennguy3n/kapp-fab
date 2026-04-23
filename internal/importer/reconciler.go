package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// Reconciler compares source-side totals against staging-side totals
// and flags any discrepancies. It is intentionally narrow: it does
// not pull data back from the source a second time — the source
// counts/checksums come from the adapter's Discover stage and are
// persisted in the job's `config.discovered` or `progress.source`
// subtree. The reconciler reads those and compares them to the real
// counts from `import_staging`.
type Reconciler struct {
	staging *StagingStore
}

// NewReconciler wires a reconciler over the StagingStore.
func NewReconciler(staging *StagingStore) *Reconciler {
	return &Reconciler{staging: staging}
}

// SourceSummary is the shape the reconciler expects under
// `job.progress.source`. Adapters populate it during the Discover /
// Export stages so the reconciler can compare apples to apples
// without calling the source again.
type SourceSummary struct {
	Count     int64             `json:"count"`
	Checksums map[string]string `json:"checksums,omitempty"`
}

// Reconcile computes per-status counts from `import_staging`,
// compares them to the adapter's discovered counts, and returns a
// full Reconciliation summary. Any mismatch lands in Discrepancies.
func (r *Reconciler) Reconcile(ctx context.Context, job ImportJob, source SourceSummary) (Reconciliation, error) {
	counts, err := r.staging.CountsByStatus(ctx, job.TenantID, job.ID)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("importer: reconcile counts: %w", err)
	}
	total := int64(0)
	for _, c := range counts {
		total += c
	}
	var rec Reconciliation
	rec.SourceCount = source.Count
	rec.StagedCount = total
	rec.ValidCount = counts[StagingValid] + counts[StagingImported]
	rec.InvalidCount = counts[StagingInvalid]
	rec.Checksums = source.Checksums

	if source.Count > 0 && source.Count != total {
		rec.Discrepancies = append(rec.Discrepancies, fmt.Sprintf(
			"source count %d != staged count %d", source.Count, total,
		))
	}
	if rec.InvalidCount > 0 {
		rec.Discrepancies = append(rec.Discrepancies, fmt.Sprintf(
			"%d staging rows failed validation", rec.InvalidCount,
		))
	}
	return rec, nil
}

// Checksum computes a stable SHA-256 hex digest of a list of records.
// Adapters use this during Discover to publish a source-side digest
// that the reconciler can compare against the staged set. Each record
// is serialized with sorted keys so the digest is deterministic.
func Checksum(records []map[string]any) (string, error) {
	h := sha256.New()
	for _, r := range records {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, err := json.Marshal(r[k])
			if err != nil {
				return "", err
			}
			_, _ = h.Write([]byte(k))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write(b)
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ComputeChecksumsPerKType produces a deterministic per-KType digest
// over the staged rows so the operator can spot an accidental column
// truncation that wouldn't change the row count. Exposed so the
// importer service can surface it from GET /api/v1/imports/{id}.
func (r *Reconciler) ComputeChecksumsPerKType(
	ctx context.Context, tenantID, jobID uuid.UUID,
) (map[string]string, error) {
	rows, err := r.staging.ListByJob(ctx, tenantID, jobID, "", 5000, 0)
	if err != nil {
		return nil, err
	}
	byKType := make(map[string][]map[string]any)
	for _, row := range rows {
		var data map[string]any
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return nil, err
		}
		byKType[row.TargetKType] = append(byKType[row.TargetKType], data)
	}
	out := make(map[string]string, len(byKType))
	for kt, recs := range byKType {
		digest, err := Checksum(recs)
		if err != nil {
			return nil, err
		}
		out[kt] = digest
	}
	return out, nil
}
