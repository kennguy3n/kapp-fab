package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// BulkResult is the summary returned by BulkPatch / BulkDelete. The
// handler surfaces failed ids so the UI can mark individual rows for
// retry rather than pretending a partial-ok outcome succeeded.
type BulkResult struct {
	Succeeded []uuid.UUID `json:"succeeded"`
	Failed    []BulkError `json:"failed"`
}

// BulkError pairs a failing record id with its error message so the
// UI can render per-row diagnostics.
type BulkError struct {
	ID    uuid.UUID `json:"id"`
	Error string    `json:"error"`
}

// BulkPatch shallow-merges patch onto every (tenantID, ktype, id)
// record in a single transaction so either all rows commit or none
// do. Each successful row emits the canonical krecord.updated event
// and a matching audit entry. Rows that 404 (already deleted, wrong
// tenant under RLS) are rolled into Failed; the transaction still
// commits the rows that succeeded so one typo does not poison the
// whole batch.
func (s *PGStore) BulkPatch(
	ctx context.Context,
	tenantID uuid.UUID,
	ktypeName string,
	ids []uuid.UUID,
	patch json.RawMessage,
	actor uuid.UUID,
) (BulkResult, error) {
	if tenantID == uuid.Nil {
		return BulkResult{}, errors.New("record: bulk: tenant id required")
	}
	if ktypeName == "" {
		return BulkResult{}, errors.New("record: bulk: ktype required")
	}
	if len(ids) == 0 {
		return BulkResult{}, nil
	}
	if len(patch) == 0 {
		patch = json.RawMessage("{}")
	}
	result := BulkResult{Succeeded: []uuid.UUID{}, Failed: []BulkError{}}
	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		a := actor
		actorPtr = &a
	}
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range ids {
			if err := s.bulkPatchOne(ctx, tx, tenantID, ktypeName, id, patch, actorPtr, &result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return BulkResult{}, err
	}
	return result, nil
}

func (s *PGStore) bulkPatchOne(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	ktypeName string,
	id uuid.UUID,
	patch json.RawMessage,
	actor *uuid.UUID,
	result *BulkResult,
) error {
	var existing KRecord
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
		        created_by, created_at, updated_by, updated_at, deleted_at
		 FROM krecords
		 WHERE tenant_id = $1 AND id = $2 AND ktype = $3
		 FOR UPDATE`,
		tenantID, id, ktypeName,
	).Scan(
		&existing.ID, &existing.TenantID, &existing.KType, &existing.KTypeVersion,
		&existing.Data, &existing.Status, &existing.Version,
		&existing.CreatedBy, &existing.CreatedAt,
		&existing.UpdatedBy, &existing.UpdatedAt, &existing.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			result.Failed = append(result.Failed, BulkError{ID: id, Error: ErrNotFound.Error()})
			return nil
		}
		return fmt.Errorf("record: bulk select: %w", err)
	}

	existingPlain, err := s.decryptRecord(ctx, &existing)
	if err != nil {
		return fmt.Errorf("record: bulk decrypt: %w", err)
	}
	merged, err := mergeJSON(existingPlain, patch)
	if err != nil {
		return fmt.Errorf("record: bulk merge: %w", err)
	}
	kt, err := s.registry.Get(ctx, existing.KType, existing.KTypeVersion)
	if err != nil {
		return err
	}
	if err := ktype.ValidateData(kt.Schema, merged); err != nil {
		result.Failed = append(result.Failed, BulkError{ID: id, Error: err.Error()})
		return nil
	}
	mergedEncrypted, err := s.encryptFields(tenantID, kt.Schema, merged)
	if err != nil {
		return fmt.Errorf("record: bulk encrypt: %w", err)
	}

	var updated KRecord
	err = tx.QueryRow(ctx,
		`UPDATE krecords
		    SET data = $1, updated_by = $2, updated_at = now(), version = version + 1
		  WHERE tenant_id = $3 AND id = $4
		  RETURNING id, tenant_id, ktype, ktype_version, data, status, version,
		            created_by, created_at, updated_by, updated_at, deleted_at`,
		mergedEncrypted, actor, tenantID, id,
	).Scan(
		&updated.ID, &updated.TenantID, &updated.KType, &updated.KTypeVersion,
		&updated.Data, &updated.Status, &updated.Version,
		&updated.CreatedBy, &updated.CreatedAt,
		&updated.UpdatedBy, &updated.UpdatedAt, &updated.DeletedAt,
	)
	if err != nil {
		return fmt.Errorf("record: bulk update: %w", err)
	}
	updated.Data = merged
	diff := audit.Diff(existingPlain, merged)
	if err := s.emit(ctx, tx, updated, "krecord.updated", audit.Entry{
		TenantID:    updated.TenantID,
		ActorID:     updated.UpdatedBy,
		ActorKind:   audit.ActorUser,
		Action:      fmt.Sprintf("%s.bulk_update", updated.KType),
		TargetKType: updated.KType,
		TargetID:    &updated.ID,
		Before:      existingPlain,
		After:       merged,
		Context:     diff,
	}); err != nil {
		return err
	}
	result.Succeeded = append(result.Succeeded, id)
	return nil
}

// BulkDelete soft-deletes every (tenantID, ktype, id) row in a single
// transaction. Missing or already-deleted rows are surfaced on Failed
// rather than rolling the whole batch back so one stale selection in
// the UI does not block the rest of the delete.
func (s *PGStore) BulkDelete(
	ctx context.Context,
	tenantID uuid.UUID,
	ktypeName string,
	ids []uuid.UUID,
	actor uuid.UUID,
) (BulkResult, error) {
	if tenantID == uuid.Nil {
		return BulkResult{}, errors.New("record: bulk: tenant id required")
	}
	if ktypeName == "" {
		return BulkResult{}, errors.New("record: bulk: ktype required")
	}
	if len(ids) == 0 {
		return BulkResult{}, nil
	}
	result := BulkResult{Succeeded: []uuid.UUID{}, Failed: []BulkError{}}
	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		a := actor
		actorPtr = &a
	}
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range ids {
			if err := s.bulkDeleteOne(ctx, tx, tenantID, ktypeName, id, actorPtr, &result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return BulkResult{}, err
	}
	return result, nil
}

func (s *PGStore) bulkDeleteOne(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	ktypeName string,
	id uuid.UUID,
	actor *uuid.UUID,
	result *BulkResult,
) error {
	var existing KRecord
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
		        created_by, created_at, updated_by, updated_at, deleted_at
		 FROM krecords
		 WHERE tenant_id = $1 AND id = $2 AND ktype = $3
		 FOR UPDATE`,
		tenantID, id, ktypeName,
	).Scan(
		&existing.ID, &existing.TenantID, &existing.KType, &existing.KTypeVersion,
		&existing.Data, &existing.Status, &existing.Version,
		&existing.CreatedBy, &existing.CreatedAt,
		&existing.UpdatedBy, &existing.UpdatedAt, &existing.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			result.Failed = append(result.Failed, BulkError{ID: id, Error: ErrNotFound.Error()})
			return nil
		}
		return fmt.Errorf("record: bulk select: %w", err)
	}
	if existing.Status == "deleted" {
		result.Failed = append(result.Failed, BulkError{ID: id, Error: ErrNotFound.Error()})
		return nil
	}

	var deleted KRecord
	err = tx.QueryRow(ctx,
		`UPDATE krecords
		    SET status = 'deleted', deleted_at = now(), updated_at = now(),
		        updated_by = $3, version = version + 1
		  WHERE tenant_id = $1 AND id = $2
		  RETURNING id, tenant_id, ktype, ktype_version, data, status, version,
		            created_by, created_at, updated_by, updated_at, deleted_at`,
		tenantID, id, actor,
	).Scan(
		&deleted.ID, &deleted.TenantID, &deleted.KType, &deleted.KTypeVersion,
		&deleted.Data, &deleted.Status, &deleted.Version,
		&deleted.CreatedBy, &deleted.CreatedAt,
		&deleted.UpdatedBy, &deleted.UpdatedAt, &deleted.DeletedAt,
	)
	if err != nil {
		return fmt.Errorf("record: bulk delete: %w", err)
	}

	existingPlain, err := s.decryptRecord(ctx, &existing)
	if err != nil {
		return fmt.Errorf("record: bulk decrypt: %w", err)
	}
	deleted.Data = existingPlain
	if err := s.emit(ctx, tx, deleted, "krecord.deleted", audit.Entry{
		TenantID:    deleted.TenantID,
		ActorID:     actor,
		ActorKind:   audit.ActorUser,
		Action:      fmt.Sprintf("%s.bulk_delete", deleted.KType),
		TargetKType: deleted.KType,
		TargetID:    &deleted.ID,
		Before:      existingPlain,
		After:       existingPlain,
	}); err != nil {
		return err
	}
	result.Succeeded = append(result.Succeeded, id)
	return nil
}

// BulkFetch returns the records for the supplied ids as a
// tenant-scoped read. Callers use this to stream a CSV export off
// the selected rows without re-implementing the decrypt path.
func (s *PGStore) BulkFetch(
	ctx context.Context,
	tenantID uuid.UUID,
	ktypeName string,
	ids []uuid.UUID,
) ([]KRecord, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("record: bulk: tenant id required")
	}
	if ktypeName == "" {
		return nil, errors.New("record: bulk: ktype required")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]KRecord, 0, len(ids))
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords
			 WHERE tenant_id = $1 AND ktype = $2 AND id = ANY($3)
			 ORDER BY updated_at DESC, id DESC`,
			tenantID, ktypeName, ids,
		)
		if err != nil {
			return fmt.Errorf("record: bulk fetch: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r KRecord
			if err := rows.Scan(
				&r.ID, &r.TenantID, &r.KType, &r.KTypeVersion,
				&r.Data, &r.Status, &r.Version,
				&r.CreatedBy, &r.CreatedAt,
				&r.UpdatedBy, &r.UpdatedAt, &r.DeletedAt,
			); err != nil {
				return fmt.Errorf("record: bulk fetch scan: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		decrypted, err := s.decryptRecord(ctx, &out[i])
		if err != nil {
			return nil, err
		}
		out[i].Data = decrypted
	}
	return out, nil
}
