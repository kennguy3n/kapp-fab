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
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// FieldEncryptor is the subset of *tenant.KeyManager consumed by the
// store. The interface exists so tests can swap in deterministic or
// no-op implementations without wiring a real master key.
type FieldEncryptor interface {
	EncryptString(tenantID uuid.UUID, plaintext string) (string, error)
	DecryptString(tenantID uuid.UUID, value string) (string, error)
}

// compile-time assertion that *tenant.KeyManager satisfies FieldEncryptor.
var _ FieldEncryptor = (*tenant.KeyManager)(nil)

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
	// encryptor, when non-nil, transparently encrypts fields marked
	// {"encrypted": true} in the KType schema before INSERT and
	// decrypts them after SELECT. Leaving it nil preserves legacy
	// behaviour so rolling the feature on is a pure add.
	encryptor FieldEncryptor
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

// WithEncryptor returns the store with per-tenant field encryption
// enabled. The encryptor is applied on Create/Get/List/Update/Delete
// for any KType field whose schema carries {"encrypted": true}.
func (s *PGStore) WithEncryptor(e FieldEncryptor) *PGStore {
	s.encryptor = e
	return s
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
	// Preserve the plaintext payload for the audit entry and event
	// envelope. AES-GCM picks a fresh random nonce on every encrypt,
	// so audit diffs against ciphertext would be false positives and
	// the audit trail itself would be unreadable.
	plaintext := r.Data
	// Encrypt fields the schema marks as sensitive. This happens after
	// validation so validators still see plaintext (max_length, pattern,
	// etc. are meaningful only against the real value).
	encrypted, err := s.encryptFields(r.TenantID, kt.Schema, r.Data)
	if err != nil {
		return nil, err
	}
	r.Data = encrypted
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
		// Swap the RETURNING ciphertext for the original plaintext so the
		// event envelope and audit After are both human-readable.
		created.Data = plaintext
		return s.emit(ctx, tx, created, "krecord.created", audit.Entry{
			TenantID:    created.TenantID,
			ActorID:     &created.CreatedBy,
			ActorKind:   audit.ActorUser,
			Action:      fmt.Sprintf("%s.create", created.KType),
			TargetKType: created.KType,
			TargetID:    &created.ID,
			After:       plaintext,
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
	decrypted, err := s.decryptRecord(ctx, &out)
	if err != nil {
		return nil, err
	}
	out.Data = decrypted
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

	// Preallocate an empty (non-nil) slice so the JSON response is `[]`
	// rather than `null` when no rows match — consistent with the OpenAPI
	// list response contract.
	out := make([]KRecord, 0)
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
	// Decrypt encrypted fields on the way out so list responses carry
	// plaintext to callers. decryptRecord is a no-op when the store
	// has no encryptor or the KType schema has no encrypted fields.
	for i := range out {
		decrypted, err := s.decryptRecord(ctx, &out[i])
		if err != nil {
			return nil, err
		}
		out[i].Data = decrypted
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

		// Decrypt existing.Data before merging so the plaintext patch
		// in r.Data is shallow-merged onto plaintext, not onto ciphertext.
		// Without this the encrypted fields would be overwritten with
		// whatever ciphertext survives the merge and become unreadable.
		existingPlain, err := s.decryptRecord(ctx, &existing)
		if err != nil {
			return fmt.Errorf("record: decrypt existing: %w", err)
		}

		merged, err := mergeJSON(existingPlain, r.Data)
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

		// Re-encrypt the merged payload before writing so encrypted
		// fields round-trip through the DB as ciphertext.
		mergedEncrypted, err := s.encryptFields(r.TenantID, kt.Schema, merged)
		if err != nil {
			return fmt.Errorf("record: encrypt merged: %w", err)
		}

		updatedBy := r.UpdatedBy
		err = tx.QueryRow(ctx,
			`UPDATE krecords
			    SET data = $1, updated_by = $2, updated_at = now(), version = version + 1
			  WHERE tenant_id = $3 AND id = $4
			  RETURNING id, tenant_id, ktype, ktype_version, data, status, version,
			            created_by, created_at, updated_by, updated_at, deleted_at`,
			mergedEncrypted, updatedBy, r.TenantID, r.ID,
		).Scan(
			&updated.ID, &updated.TenantID, &updated.KType, &updated.KTypeVersion,
			&updated.Data, &updated.Status, &updated.Version,
			&updated.CreatedBy, &updated.CreatedAt,
			&updated.UpdatedBy, &updated.UpdatedAt, &updated.DeletedAt,
		)
		if err != nil {
			return fmt.Errorf("record: update: %w", err)
		}

		// Substitute plaintext for audit diff + event envelope. Comparing
		// ciphertext would flag every encrypted field as changed on every
		// update (fresh GCM nonces) and leave the audit trail unreadable.
		updated.Data = merged
		diff := audit.Diff(existingPlain, merged)
		return s.emit(ctx, tx, updated, "krecord.updated", audit.Entry{
			TenantID:    updated.TenantID,
			ActorID:     updated.UpdatedBy,
			ActorKind:   audit.ActorUser,
			Action:      fmt.Sprintf("%s.update", updated.KType),
			TargetKType: updated.KType,
			TargetID:    &updated.ID,
			Before:      existingPlain,
			After:       merged,
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
// actorID attributes the deletion in the audit log; pass uuid.Nil to leave
// actor unattributed.
func (s *PGStore) Delete(ctx context.Context, tenantID, id, actorID uuid.UUID) error {
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
		// Treat an already-deleted record as absent so a replayed DELETE
		// does not emit a second `krecord.deleted` event or audit entry.
		if existing.Status == "deleted" {
			return ErrNotFound
		}

		var updatedBy *uuid.UUID
		if actorID != uuid.Nil {
			a := actorID
			updatedBy = &a
		}

		var deleted KRecord
		err = tx.QueryRow(ctx,
			`UPDATE krecords
			    SET status = 'deleted', deleted_at = now(), updated_at = now(),
			        updated_by = $3, version = version + 1
			  WHERE tenant_id = $1 AND id = $2
			  RETURNING id, tenant_id, ktype, ktype_version, data, status, version,
			            created_by, created_at, updated_by, updated_at, deleted_at`,
			tenantID, id, updatedBy,
		).Scan(
			&deleted.ID, &deleted.TenantID, &deleted.KType, &deleted.KTypeVersion,
			&deleted.Data, &deleted.Status, &deleted.Version,
			&deleted.CreatedBy, &deleted.CreatedAt,
			&deleted.UpdatedBy, &deleted.UpdatedAt, &deleted.DeletedAt,
		)
		if err != nil {
			return fmt.Errorf("record: soft delete: %w", err)
		}

		// Soft delete does not touch the data column, so existing and
		// deleted carry the same ciphertext; decrypt once and reuse for
		// both the audit envelope and the event payload.
		existingPlain, err := s.decryptRecord(ctx, &existing)
		if err != nil {
			return fmt.Errorf("record: decrypt existing: %w", err)
		}
		deleted.Data = existingPlain
		return s.emit(ctx, tx, deleted, "krecord.deleted", audit.Entry{
			TenantID:    deleted.TenantID,
			ActorID:     updatedBy,
			ActorKind:   audit.ActorUser,
			Action:      fmt.Sprintf("%s.delete", deleted.KType),
			TargetKType: deleted.KType,
			TargetID:    &deleted.ID,
			Before:      existingPlain,
			After:       existingPlain,
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

// encryptFields transforms data in-place, encrypting every top-level
// field that the schema flags {"encrypted": true}. When no encryptor
// is wired up, or the schema has no encrypted fields, data is
// returned unchanged — the feature is opt-in and cheap when off.
func (s *PGStore) encryptFields(tenantID uuid.UUID, schema, data json.RawMessage) (json.RawMessage, error) {
	if s.encryptor == nil {
		return data, nil
	}
	fields, err := encryptedFieldNames(schema)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return data, nil
	}
	payload, err := unmarshalData(data)
	if err != nil {
		return nil, err
	}
	changed := false
	for name := range fields {
		v, ok := payload[name]
		if !ok || v == nil {
			continue
		}
		plaintext, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("record: encrypted field %q must be a string", name)
		}
		if tenant.IsEncrypted(plaintext) {
			continue
		}
		ct, err := s.encryptor.EncryptString(tenantID, plaintext)
		if err != nil {
			return nil, fmt.Errorf("record: encrypt field %q: %w", name, err)
		}
		payload[name] = ct
		changed = true
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(payload)
}

// decryptRecord produces the caller-visible version of r.Data, with
// encrypted fields decrypted transparently. r is not mutated — the
// caller substitutes the returned payload into the outgoing record.
func (s *PGStore) decryptRecord(ctx context.Context, r *KRecord) (json.RawMessage, error) {
	_ = ctx
	if s.encryptor == nil {
		return r.Data, nil
	}
	kt, err := s.registry.Get(ctx, r.KType, r.KTypeVersion)
	if err != nil {
		return nil, err
	}
	fields, err := encryptedFieldNames(kt.Schema)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return r.Data, nil
	}
	payload, err := unmarshalData(r.Data)
	if err != nil {
		return nil, err
	}
	changed := false
	for name := range fields {
		v, ok := payload[name]
		if !ok || v == nil {
			continue
		}
		ct, ok := v.(string)
		if !ok {
			continue
		}
		pt, err := s.encryptor.DecryptString(r.TenantID, ct)
		if err != nil {
			return nil, fmt.Errorf("record: decrypt field %q: %w", name, err)
		}
		if pt != ct {
			payload[name] = pt
			changed = true
		}
	}
	if !changed {
		return r.Data, nil
	}
	return json.Marshal(payload)
}

// encryptedFieldNames extracts the set of field names in schema that
// carry {"encrypted": true}. Returns an empty set (not nil) when none
// are present so callers can range over the result unconditionally.
func encryptedFieldNames(schema json.RawMessage) (map[string]struct{}, error) {
	var s ktype.Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, fmt.Errorf("record: parse schema for encryption: %w", err)
	}
	out := make(map[string]struct{})
	for _, f := range s.Fields {
		if f.Encrypted {
			out[f.Name] = struct{}{}
		}
	}
	return out, nil
}

// unmarshalData decodes a JSONB payload into a map. An empty payload
// yields an empty map so the caller can treat it uniformly.
func unmarshalData(data json.RawMessage) (map[string]any, error) {
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("record: parse data: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
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
