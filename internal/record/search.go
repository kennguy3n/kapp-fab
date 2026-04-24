package record

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// SearchResult is one row returned by Search. It mirrors KRecord plus
// a `rank` the client renders for ordering / deep-linking back to
// the record's list page.
type SearchResult struct {
	KRecord
	Rank float64 `json:"rank"`
}

// Search runs a plainto_tsquery against krecords.search_vector scoped
// to the supplied tenant. ktypes, when non-empty, restricts the scan
// to the named KTypes so the UI can surface per-domain result tabs
// without a post-filter. Soft-deleted records are excluded.
func (s *PGStore) Search(
	ctx context.Context,
	tenantID uuid.UUID,
	query string,
	ktypes []string,
	limit int,
) ([]SearchResult, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("record: search: tenant id required")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return []SearchResult{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	out := make([]SearchResult, 0, limit)
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		// The query uses plainto_tsquery so the user-supplied string
		// is safely lexeme-parsed — no risk of injecting operators.
		// ts_rank gives us a relevance score the UI can render
		// alongside the result row for transparency.
		if len(ktypes) == 0 {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				       created_by, created_at, updated_by, updated_at, deleted_at,
				       ts_rank(search_vector, plainto_tsquery('simple', $2)) AS rank
				  FROM krecords
				 WHERE tenant_id = $1
				   AND status <> 'deleted'
				   AND search_vector @@ plainto_tsquery('simple', $2)
				 ORDER BY rank DESC, updated_at DESC
				 LIMIT $3`,
				tenantID, query, limit,
			)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				       created_by, created_at, updated_by, updated_at, deleted_at,
				       ts_rank(search_vector, plainto_tsquery('simple', $2)) AS rank
				  FROM krecords
				 WHERE tenant_id = $1
				   AND status <> 'deleted'
				   AND ktype = ANY($4)
				   AND search_vector @@ plainto_tsquery('simple', $2)
				 ORDER BY rank DESC, updated_at DESC
				 LIMIT $3`,
				tenantID, query, limit, ktypes,
			)
		}
		if err != nil {
			return fmt.Errorf("record: search query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r SearchResult
			if err := rows.Scan(
				&r.ID, &r.TenantID, &r.KType, &r.KTypeVersion,
				&r.Data, &r.Status, &r.Version,
				&r.CreatedBy, &r.CreatedAt,
				&r.UpdatedBy, &r.UpdatedAt, &r.DeletedAt,
				&r.Rank,
			); err != nil {
				return fmt.Errorf("record: search scan: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// Decrypt any encrypted fields on the result rows so the UI
	// surface matches what the list page renders — otherwise
	// search would expose ciphertext the operator can't read.
	for i := range out {
		decrypted, err := s.decryptRecord(ctx, &out[i].KRecord)
		if err != nil {
			return nil, err
		}
		out[i].Data = decrypted
	}
	return out, nil
}
