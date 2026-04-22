package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Sentinel errors.
var (
	ErrNotFound        = errors.New("record: not found")
	ErrVersionConflict = errors.New("record: version conflict")
)

// PGStore implements Store against PostgreSQL. Every mutation runs inside a
// single transaction that:
//
//  1. Sets the tenant GUC via platform.WithTenantTx
//  2. Validates the payload against the KType schema
//  3. Executes the DB operation (INSERT / UPDATE / soft-DELETE)
//  4. Emits an outbox event via events.Publisher.EmitTx
//  5. Writes an audit entry via audit.Logger.LogTx
//
// Step 4 and 5 participate in the same transaction as step 3 so a failure
// anywhere in the pipeline rolls the whole mutation back — no silent writes.
type PGStore struct {
	pool      *pgxpool.Pool
	registry  *ktype.PGRegistry
	publisher events.Publisher
	auditor   audit.Logger
}

// NewPGStore wires a PGStore from the shared pool and its collaborators.
func NewPGStore(
	pool *pgxpool.Pool,
	registry *ktype.PGRegistry,
	publisher events.Publisher,
	auditor audit.Logger,
) *PGStore {
	return &PGStore{
		pool:      pool,
		registry:  registry,
		publisher: publisher,
		auditor:   auditor,
	}
}

// Create inserts a new KRecord. The KType is looked up at version 0 ("latest")
// if the caller did not specify one explicitly.
func (s *PGStore) Create(ctx context.Context, r KRecord) (*KRecord, error) {
	if r.KType == "" {
		return nil, errors.New("record: ktype required")
	}
	if r.TenantID == uuid.Nil {
		return nil, errors.New("record: tenant id required")
	}
	if r.CreatedBy == uuid.Nil {
		return nil, errors.New("record: created_by required")
	}
	kt, err := s.registry.Get(ctx, r.KType, r.KTypeVersion)
	if err != nil {
		return nil, err
	}
	if err := ktype.ValidateData(kt.Schema, r.Data); err != nil {
		return nil, err
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.KTypeVersion = kt.Version
	if r.Status == "" {
		r.Status = "active"
	}
	if r.Version == 0 {
		r.Version = 1
	}

	var created KRecord
	err = platform.WithTenantTx(ctx, s.pool, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO krecords
			     (id, tenant_id, ktype, ktype_version, data, status, version, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING id, tenant_id, ktype, ktype_version, data, status, version,
			           created_by, created_at, updated_by, updated_at, deleted_at`,
			r.ID, r.TenantID, r.KType, r.KTypeVersion, r.Data, r.Status, r.Version, r.CreatedBy,
		).Scan(
			&created.ID, &created.TenantID, &created.KType, &created.KTypeVersion,
			&created.Data, &created.Status, &created.Version,
			&created.CreatedBy, &created.CreatedAt,
			&created.UpdatedBy, &created.UpdatedAt, &created.DeletedAt,
		)
		if err != nil {
			return fmt.Errorf("record: insert: %w", err)
		}
		return s.emit(ctx, tx, created, "krecord.created", audit.Entry{
			TenantID:    created.TenantID,
			ActorID:     &created.CreatedBy,
			ActorKind:   audit.ActorUser,
			Action:      fmt.Sprintf("%s.create", created.KType),
			TargetKType: created.KType,
			TargetID:    &created.ID,
			After:       created.Data,
		})
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// Get returns a single record. RLS filters cross-tenant access.
func (s *PGStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*KRecord, error) {
	var out KRecord
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&out.ID, &out.TenantID, &out.KType, &out.KTypeVersion,
			&out.Data, &out.Status, &out.Version,
			&out.CreatedBy, &out.CreatedAt,
			&out.UpdatedBy, &out.UpdatedAt, &out.DeletedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("record: get: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns records for the tenant, filtered by ktype/status.
func (s *PGStore) List(ctx context.Context, tenantID uuid.UUID, filter ListFilter) ([]KRecord, error) {
	if filter.KType == "" {
		return nil, errors.New("record: ktype filter required")
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 50
	}
	status := filter.Status
	if status == "" {
		status = "active"
	}

	var out []KRecord
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords
			 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
			 ORDER BY updated_at DESC, id DESC
			 LIMIT $4 OFFSET $5`,
			tenantID, filter.KType, status, filter.Limit, filter.Offset,
		)
		if err != nil {
			return fmt.Errorf("record: list: %w", err)
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
				return fmt.Errorf("record: scan: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Update applies a patch to a record. The incoming r.Data is merged shallowly
// onto the existing row so callers can submit only the fields they want to
// change. Optimistic concurrency is enforced by matching on the current
// version; an outdated version returns ErrVersionConflict.
func (s *PGStore) Update(ctx context.Context, r KRecord) (*KRecord, error) {
	if r.TenantID == uuid.Nil || r.ID == uuid.Nil {
		return nil, errors.New("record: tenant and id required")
	}

	var updated KRecord
	err := platform.WithTenantTx(ctx, s.pool, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var existing KRecord
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords
			 WHERE tenant_id = $1 AND id = $2
			 FOR UPDATE`,
			r.TenantID, r.ID,
		).Scan(
			&existing.ID, &existing.TenantID, &existing.KType, &existing.KTypeVersion,
			&existing.Data, &existing.Status, &existing.Version,
			&existing.CreatedBy, &existing.CreatedAt,
			&existing.UpdatedBy, &existing.UpdatedAt, &existing.DeletedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("record: select for update: %w", err)
		}
		if r.Version != 0 && r.Version != existing.Version {
			return ErrVersionConflict
		}

		merged, err := mergeJSON(existing.Data, r.Data)
		if err != nil {
			return fmt.Errorf("record: merge: %w", err)
		}

		kt, err := s.registry.Get(ctx, existing.KType, existing.KTypeVersion)
		if err != nil {
			return err
		}
		if err := ktype.ValidateData(kt.Schema, merged); err != nil {
			return err
		}

		updatedBy := r.UpdatedBy
		err = tx.QueryRow(ctx,
			`UPDATE krecords
			    SET data = $1, updated_by = $2, updated_at = now(), version = version + 1
			  WHERE tenant_id = $3 AND id = $4
			  RETURNING id, tenant_id, ktype, ktype_version, data, status, version,
			            created_by, created_at, updated_by, updated_at, deleted_at`,
			merged, updatedBy, r.TenantID, r.ID,
		).Scan(
			&updated.ID, &updated.TenantID, &updated.KType, &updated.KTypeVersion,
			&updated.Data, &updated.Status, &updated.Version,
			&updated.CreatedBy, &updated.CreatedAt,
			&updated.UpdatedBy, &updated.UpdatedAt, &updated.DeletedAt,
		)
		if err != nil {
			return fmt.Errorf("record: update: %w", err)
		}

		diff := audit.Diff(existing.Data, updated.Data)
		return s.emit(ctx, tx, updated, "krecord.updated", audit.Entry{
			TenantID:    updated.TenantID,
			ActorID:     updated.UpdatedBy,
			ActorKind:   audit.ActorUser,
			Action:      fmt.Sprintf("%s.update", updated.KType),
			TargetKType: updated.KType,
			TargetID:    &updated.ID,
			Before:      existing.Data,
			After:       updated.Data,
			Context:     diff,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// Delete soft-deletes a record (status=deleted, deleted_at=now()) and emits
// krecord.deleted. Hard deletes are reserved for tenant purge operations.
func (s *PGStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var existing KRecord
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id,
		).Scan(
			&existing.ID, &existing.TenantID, &existing.KType, &existing.KTypeVersion,
			&existing.Data, &existing.Status, &existing.Version,
			&existing.CreatedBy, &existing.CreatedAt,
			&existing.UpdatedBy, &existing.UpdatedAt, &existing.DeletedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("record: select for delete: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE krecords
			    SET status = 'deleted', deleted_at = now(), updated_at = now(), version = version + 1
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		); err != nil {
			return fmt.Errorf("record: soft delete: %w", err)
		}
		return s.emit(ctx, tx, existing, "krecord.deleted", audit.Entry{
			TenantID:    existing.TenantID,
			ActorKind:   audit.ActorUser,
			Action:      fmt.Sprintf("%s.delete", existing.KType),
			TargetKType: existing.KType,
			TargetID:    &existing.ID,
			Before:      existing.Data,
		})
	})
}

// emit fires the event + audit side-effects in the same transaction as the
// state change.
func (s *PGStore) emit(ctx context.Context, tx pgx.Tx, r KRecord, eventType string, entry audit.Entry) error {
	payload, err := json.Marshal(map[string]any{
		"id":       r.ID,
		"tenant":   r.TenantID,
		"ktype":    r.KType,
		"version":  r.Version,
		"status":   r.Status,
		"data":     r.Data,
		"updated":  r.UpdatedAt,
		"created":  r.CreatedAt,
		"actor":    entry.ActorID,
		"kind":     string(entry.ActorKind),
		"snapshot": snapshotTypeFor(eventType),
	})
	if err != nil {
		return fmt.Errorf("record: marshal event payload: %w", err)
	}
	if err := s.publisher.EmitTx(ctx, tx, events.Event{
		TenantID: r.TenantID,
		Type:     eventType,
		Payload:  payload,
	}); err != nil {
		return err
	}
	return s.auditor.LogTx(ctx, tx, entry)
}

func snapshotTypeFor(eventType string) string {
	switch {
	case strings.HasSuffix(eventType, ".created"):
		return "create"
	case strings.HasSuffix(eventType, ".updated"):
		return "update"
	case strings.HasSuffix(eventType, ".deleted"):
		return "delete"
	default:
		return "unknown"
	}
}

// mergeJSON performs a shallow merge of patch onto base. Keys present in
// patch overwrite keys in base; keys absent from patch are preserved.
func mergeJSON(base, patch json.RawMessage) (json.RawMessage, error) {
	var baseMap, patchMap map[string]any
	if len(base) == 0 {
		base = json.RawMessage("{}")
	}
	if len(patch) == 0 {
		return base, nil
	}
	if err := json.Unmarshal(base, &baseMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, err
	}
	if baseMap == nil {
		baseMap = map[string]any{}
	}
	for k, v := range patchMap {
		baseMap[k] = v
	}
	return json.Marshal(baseMap)
}
