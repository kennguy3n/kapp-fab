package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// tenantKTypes, when non-nil, is consulted for KType names in
	// the `custom.*` namespace (Phase N8b low-code KTypes). When
	// nil, the record store falls back to platform-only behaviour
	// — every name resolves through the global ktypes table. The
	// store is wired via WithTenantKTypes on the boot path.
	tenantKTypes *ktype.TenantStore
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

// WithTenantKTypes returns the store with a tenant-scoped KType
// store wired in. With it, names matching `custom.*` resolve
// against the per-tenant `tenant_ktypes` table; everything else
// falls through to the global `ktypes` registry. The store is
// rejected if it is nil — callers that explicitly want to disable
// custom KType lookup should simply not call WithTenantKTypes.
func (s *PGStore) WithTenantKTypes(store *ktype.TenantStore) *PGStore {
	if store != nil {
		s.tenantKTypes = store
	}
	return s
}

// ktypeResolveMode tells resolveKType which custom-KType statuses
// are allowed for the calling operation. The status gate exists
// because tenant-authored KTypes have a lifecycle (draft → active
// → archived) and the operation that consults a KType must opt
// into the right subset:
//
//   - resolveForCreate: only `active`. A KType in `draft` is still
//     being authored in the builder and isn't promoted; an
//     `archived` KType is frozen — no new records on either.
//
//   - resolveForUpdate: `active` OR `archived`. Existing records on
//     an archived schema must remain editable (data-entry
//     corrections, status workflow transitions, etc.) — the
//     archival freezes the schema, not the rows that were already
//     created against it. `draft` is still rejected because no
//     records should exist there in the first place.
//
//   - resolveForRead: any status. Reads / decryptions of historical
//     records work regardless of the current status of the schema
//     they reference.
type ktypeResolveMode int

const (
	resolveForCreate ktypeResolveMode = iota
	resolveForUpdate
	resolveForRead
)

// resolveKType is the single lookup helper every record-store path
// uses to fetch a KType's schema. For names in the custom.*
// namespace it consults the tenant_ktypes table (Phase N8b); for
// everything else it falls through to the global `ktypes` registry.
// Custom-KType status enforcement is applied per the supplied
// `mode` (see ktypeResolveMode for the per-mode rules); platform
// KTypes don't have the same lifecycle and bypass the status gate
// entirely.
//
// This pool-based entry point opens its own tenant transaction for
// the custom-KType branch. Call sites that already hold an outer
// tx (record.Update, record.Delete) must use resolveKTypeInTx
// instead so the lookup reuses the existing connection rather than
// acquiring a second one from the pool — the nested form is the
// performance hot path under load.
func (s *PGStore) resolveKType(ctx context.Context, tenantID uuid.UUID, name string, version int, mode ktypeResolveMode) (*ktype.KType, error) {
	if s.tenantKTypes != nil && ktype.IsCustomName(name) {
		tkt, err := s.tenantKTypes.Get(ctx, tenantID, name, version)
		if err != nil {
			return nil, err
		}
		return materializeCustomKType(tkt, mode)
	}
	return s.registry.Get(ctx, name, version)
}

// resolveKTypeInTx is the tx-aware variant of resolveKType. When
// called inside an outer dbutil.WithTenantTx (Update / Delete) it
// reuses the existing connection so the per-record overhead is one
// pool connection regardless of whether the KType is platform-
// supplied or tenant-authored. Platform-KType lookups bypass the tx
// entirely because PGRegistry is an in-memory map — no DB I/O.
func (s *PGStore) resolveKTypeInTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name string, version int, mode ktypeResolveMode) (*ktype.KType, error) {
	if s.tenantKTypes != nil && ktype.IsCustomName(name) {
		tkt, err := s.tenantKTypes.GetInTx(ctx, tx, tenantID, name, version)
		if err != nil {
			return nil, err
		}
		return materializeCustomKType(tkt, mode)
	}
	return s.registry.Get(ctx, name, version)
}

// materializeCustomKType is the shared post-fetch step used by both
// resolveKType and resolveKTypeInTx so the status gate and KType
// projection stay in lock-step across the two entry points.
func materializeCustomKType(tkt *ktype.TenantKType, mode ktypeResolveMode) (*ktype.KType, error) {
	if err := checkCustomKTypeStatus(tkt, mode); err != nil {
		return nil, err
	}
	return &ktype.KType{
		Name:      tkt.Name,
		Version:   tkt.Version,
		Schema:    tkt.Schema,
		CreatedAt: tkt.CreatedAt,
	}, nil
}

// checkCustomKTypeStatus enforces the per-mode rules for a custom
// KType's status. Split out from resolveKType so the BulkPatch /
// Delete paths can share the exact same matrix.
func checkCustomKTypeStatus(tkt *ktype.TenantKType, mode ktypeResolveMode) error {
	switch mode {
	case resolveForCreate:
		if tkt.Status != ktype.CustomStatusActive {
			return fmt.Errorf("record: custom ktype %q is %s, only active types back record creates", tkt.Name, tkt.Status)
		}
	case resolveForUpdate:
		if tkt.Status != ktype.CustomStatusActive && tkt.Status != ktype.CustomStatusArchived {
			return fmt.Errorf("record: custom ktype %q is %s, only active or archived types accept updates", tkt.Name, tkt.Status)
		}
	case resolveForRead:
		// every status is readable
	}
	return nil
}

// dbNow returns Postgres's current timestamp. Used to capture the
// keyset-walk snapshot ceiling in ListAll / ListByField / ForEach in
// the same clock domain that assigns `updated_at` values on commit,
// so app-server clock skew (NTP jitter, container pause) cannot move
// the ceiling backwards relative to row timestamps. clock_timestamp()
// is preferred over now() / transaction_timestamp() because it is
// not frozen at the start of the surrounding transaction — we want
// the wall-clock instant this call is made, not the instant the
// outer scheduler's transaction began.
//
// Returns timestamptz directly (no `AT TIME ZONE 'UTC'` cast) so the
// result type carries its own UTC anchor rather than relying on
// pgx's scan-default location for `timestamp without time zone`.
// pgx v5 always normalises timestamptz to UTC on scan regardless of
// the connection's TimeZone setting; the previous formulation only
// produced UTC because pgx's plain-timestamp default *happens* to
// be UTC. The explicit .UTC() below is kept as belt-and-suspenders
// in case a future scan layer normalises timestamptz to the local
// zone, but the SQL itself no longer depends on the driver's
// timezone defaults.
func (s *PGStore) dbNow(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("record: dbNow: %w", err)
	}
	return t.UTC(), nil
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
	kt, err := s.resolveKType(ctx, r.TenantID, r.KType, r.KTypeVersion, resolveForCreate)
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
//
// This is the HTTP-facing list method: Limit is capped at 500 and
// defaults to 50 to protect the API from unbounded pulls. Server-side
// batch callers that need to walk every row of a KType should use
// ListAll / ForEach instead — see payroll engine + scheduler sweepers.
//
// Pagination strategy: when filter.Cursor is non-empty the query uses
// keyset pagination (`WHERE (updated_at, id) < (cursor_ts, cursor_id)`)
// which is stable under concurrent inserts. Otherwise, when filter.Offset
// is > 0, the legacy `OFFSET` path runs (kept for backward compat).
// When neither is set the query starts from the newest row.
func (s *PGStore) List(ctx context.Context, tenantID uuid.UUID, filter ListFilter) ([]KRecord, error) {
	page, err := s.ListPage(ctx, tenantID, filter)
	if err != nil {
		return nil, err
	}
	return page.Records, nil
}

// ListPage is the cursor-aware variant of List: in addition to the
// page of records it returns the opaque NextCursor token for the
// next page, or "" when the page is the last one. New callers (the
// HTTP list handler, the Rust SDK, the agent tools that paginate
// large KTypes) should use this method directly so the cursor is
// not lost. The legacy List helper drops the cursor for callers
// that only need the page slice.
func (s *PGStore) ListPage(ctx context.Context, tenantID uuid.UUID, filter ListFilter) (*ListPage, error) {
	if filter.KType == "" {
		return nil, errors.New("record: ktype filter required")
	}
	// Default to 50 when unset; clamp at the documented cap of 500 instead
	// of falling back to the default. Falling back to 50 on `?limit=501`
	// was surprising — callers expect a hard cap, not a silent drop.
	switch {
	case filter.Limit <= 0:
		filter.Limit = 50
	case filter.Limit > 500:
		filter.Limit = 500
	}
	status := filter.Status
	if status == "" {
		status = "active"
	}
	cursorTS, cursorID, err := DecodeCursor(filter.Cursor)
	if err != nil {
		return nil, err
	}

	// Preallocate an empty (non-nil) slice so the JSON response is `[]`
	// rather than `null` when no rows match — consistent with the OpenAPI
	// list response contract.
	out := make([]KRecord, 0, filter.Limit)
	err = platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			rows pgx.Rows
			err  error
		)
		switch {
		case filter.Cursor != "":
			// Keyset pagination: row comparison handles ties on
			// updated_at by using the id as the secondary sort key.
			rows, err = tx.Query(ctx,
				`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				        created_by, created_at, updated_by, updated_at, deleted_at
				 FROM krecords
				 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
				   AND (updated_at, id) < ($4, $5)
				 ORDER BY updated_at DESC, id DESC
				 LIMIT $6`,
				tenantID, filter.KType, status, cursorTS, cursorID, filter.Limit,
			)
		case filter.Offset > 0:
			// Legacy OFFSET path. New callers should switch to
			// cursor-based pagination — see the deprecation
			// notice surfaced by the HTTP list handler.
			rows, err = tx.Query(ctx,
				`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				        created_by, created_at, updated_by, updated_at, deleted_at
				 FROM krecords
				 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
				 ORDER BY updated_at DESC, id DESC
				 LIMIT $4 OFFSET $5`,
				tenantID, filter.KType, status, filter.Limit, filter.Offset,
			)
		default:
			// First page, no cursor — keyset with no lower bound.
			rows, err = tx.Query(ctx,
				`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				        created_by, created_at, updated_by, updated_at, deleted_at
				 FROM krecords
				 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
				 ORDER BY updated_at DESC, id DESC
				 LIMIT $4`,
				tenantID, filter.KType, status, filter.Limit,
			)
		}
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
	page := &ListPage{Records: out}
	// Only emit a next cursor when we filled the page — otherwise
	// the caller has reached the end and an empty NextCursor lets
	// them stop iterating without an extra round trip.
	if len(out) == filter.Limit {
		last := out[len(out)-1]
		page.NextCursor = EncodeCursor(last.UpdatedAt, last.ID)
	}
	return page, nil
}

// ListAll is the server-side batch variant of List: it walks every row
// matching filter.KType/Status for the tenant, without the 500-row
// HTTP-facing cap. Callers like the payroll engine that need to process
// every employee / structure / payslip for a pay_run use this to avoid
// the silent clamp on List. Rows are paginated internally via keyset
// (the same `(updated_at, id) < cursor` pattern that the public List
// uses) so concurrent inserts cannot shift a row from one chunk to the
// next and we never re-scan rows we've already returned.
//
// filter.Limit and filter.Offset are ignored — ListAll always returns
// every match. To page over a subset, pair List with explicit offsets
// or use the cursor-aware ListPage. filter.KType is required and behaves
// identically to List; filter.Status defaults to "active".
//
// Safety cap: ListAll aborts with ErrListAllExceedsCap once it has
// accumulated more than ListAllMaxRows rows. This is a defensive
// guard against unbounded memory growth on huge tenants. For callers
// that need to walk arbitrarily large KTypes without materialising
// every row, use PGStore.ForEach: it iterates the same keyset and
// snapshot but invokes a callback per row, bounding peak memory to
// one chunk (≤500 rows) and lifting the row cap entirely.
//
// ListAll is now a thin wrapper around ForEach + slice accumulation;
// the streaming path is the source of truth for the SQL and
// consistency contract — see PGStore.ForEach for the full
// description of the snapshot ceiling, DB-clock rationale, and
// per-chunk transaction semantics.
func (s *PGStore) ListAll(ctx context.Context, tenantID uuid.UUID, filter ListFilter) ([]KRecord, error) {
	out := make([]KRecord, 0)
	err := s.ForEach(ctx, tenantID, filter, func(r KRecord) error {
		// Tight cap: refuse to grow `out` past ListAllMaxRows even
		// by a single row. Returning a typed error here surfaces
		// the overflow to the caller (and the test suite) with
		// enough context (ktype, rows-so-far, cap) to migrate them
		// to ForEach.
		if len(out)+1 > ListAllMaxRows {
			return fmt.Errorf("%w: ktype=%s rows=%d cap=%d",
				ErrListAllExceedsCap, filter.KType, len(out)+1, ListAllMaxRows)
		}
		// Honour the ForEachFunc contract (record.go): the slice
		// backing r.Data is owned by the store and may be reused
		// after the callback returns. ListAll retains every row
		// beyond the callback boundary, so copy r.Data before
		// appending. Today's foreachKeyset allocates fresh buffers
		// per scan so the copy is functionally a no-op, but this
		// keeps ListAll safe under any future scan-buffer pooling
		// optimisation in the store layer.
		if r.Data != nil {
			cp := make(json.RawMessage, len(r.Data))
			copy(cp, r.Data)
			r.Data = cp
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ForEach walks every record matching filter, invoking fn for each
// decrypted KRecord. Memory stays bounded to a single chunk (≤500
// records) regardless of total row count, so this is the right
// primitive for any sweep / batch caller that does not need a
// materialised slice — recurring engine, summarize_pipeline, payroll
// repost, exporter, certificate worker, etc.
//
// Behavioural contract:
//
//  1. Rows are visited in the same (updated_at DESC, id DESC) order
//     as ListAll and ListPage, paginated internally via keyset.
//  2. Snapshot consistency: a wall-clock ceiling is captured from
//     Postgres clock_timestamp() before the first chunk. Every chunk
//     filters `updated_at <= snapshot`. A row whose updated_at is
//     bumped by a concurrent Update mid-walk is excluded from this
//     walk and picked up by the next sweep. The contract is "every
//     row whose state was committed before walk start, exactly
//     once", not "every row that ever existed during the walk". See
//     PGStore.dbNow for the DB-clock vs app-clock rationale.
//  3. Each chunk runs in its own per-tenant transaction
//     (platform.WithTenantTx). The walk does NOT hold a long-lived
//     transaction, so it does not block autovacuum and cannot fail
//     with a serialization-near-end-of-walk error.
//  4. Decryption happens after the SQL result is read but before fn
//     is called. fn receives plaintext.
//  5. fn errors propagate up unchanged, EXCEPT the sentinel
//     ErrStopForEach which is trapped and causes ForEach to return
//     nil. Use ErrStopForEach for non-error early termination ("I
//     found what I was looking for, stop walking"); return any
//     other non-nil error to abort and propagate.
//  6. Unlike ListAll, ForEach has NO ListAllMaxRows cap — peak
//     memory is bounded by one chunk regardless of total row count.
//
// filter.Limit and filter.Offset are ignored. filter.KType is
// required; filter.Status defaults to "active".
func (s *PGStore) ForEach(ctx context.Context, tenantID uuid.UUID, filter ListFilter, fn ForEachFunc) error {
	if filter.KType == "" {
		return errors.New("record: ktype filter required")
	}
	if fn == nil {
		return errors.New("record: ForEach callback required")
	}
	status := filter.Status
	if status == "" {
		status = "active"
	}
	return s.foreachKeyset(ctx, tenantID, func(haveLower bool, cursorTS time.Time, cursorID uuid.UUID, snapshot time.Time, chunk int) (string, []any) {
		if haveLower {
			return `SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				        created_by, created_at, updated_by, updated_at, deleted_at
				 FROM krecords
				 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
				   AND updated_at <= $4
				   AND (updated_at, id) < ($5, $6)
				 ORDER BY updated_at DESC, id DESC
				 LIMIT $7`,
				[]any{tenantID, filter.KType, status, snapshot, cursorTS, cursorID, chunk}
		}
		return `SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords
			 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
			   AND updated_at <= $4
			 ORDER BY updated_at DESC, id DESC
			 LIMIT $5`,
			[]any{tenantID, filter.KType, status, snapshot, chunk}
	}, fn)
}

// foreachKeyset is the shared internal driver for ForEach and
// ForEachByField. It captures the snapshot, loops per-chunk under
// platform.WithTenantTx, decrypts each row, invokes fn, and handles
// the ErrStopForEach sentinel. The caller supplies the SQL+args via
// queryBuilder: a function that, given the cursor state and chunk
// size, returns the SQL string and bind arguments for the next
// chunk's query.
//
// Splitting the SQL-building out of the loop keeps ForEach and
// ForEachByField from each having to carry their own copy of the
// snapshot + cursor + transaction + decrypt loop — the loop is
// non-trivial and was previously duplicated between ListAll and
// ListByField, which is exactly the kind of duplication that drifts
// over time (one branch gains a cap check, the other does not, etc.).
func (s *PGStore) foreachKeyset(
	ctx context.Context,
	tenantID uuid.UUID,
	queryBuilder func(haveLower bool, cursorTS time.Time, cursorID uuid.UUID, snapshot time.Time, chunk int) (string, []any),
	fn ForEachFunc,
) error {
	const chunk = 500
	snapshot, err := s.dbNow(ctx)
	if err != nil {
		return err
	}
	var (
		cursorTS  time.Time
		cursorID  uuid.UUID
		haveLower bool
	)
	for {
		page := make([]KRecord, 0, chunk)
		err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			sql, args := queryBuilder(haveLower, cursorTS, cursorID, snapshot, chunk)
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return fmt.Errorf("record: foreach: %w", err)
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
				page = append(page, r)
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
		for i := range page {
			decrypted, err := s.decryptRecord(ctx, &page[i])
			if err != nil {
				return err
			}
			page[i].Data = decrypted
			if err := fn(page[i]); err != nil {
				if errors.Is(err, ErrStopForEach) {
					return nil
				}
				return err
			}
		}
		if len(page) < chunk {
			return nil
		}
		last := page[len(page)-1]
		cursorTS, cursorID, haveLower = last.UpdatedAt, last.ID, true
	}
}

// ListByField returns every record for the tenant matching
// filter.KType whose top-level JSONB field `field` equals `value`
// (case-insensitively). Used by surfaces like the customer portal
// where the caller must be restricted to rows they own
// (helpdesk.ticket.customer_email = claims.Email) — pushing the
// predicate into SQL avoids loading and decrypting the entire
// KType for the tenant just to filter client-side.
//
// Only plain-text JSONB fields are safe targets: values on fields
// marked `encrypted: true` in the schema are stored as ciphertext
// and would never match. Callers must validate that `field` is a
// public/indexable attribute before passing it in — this method is
// not exposed to end users directly.
//
// Now a thin wrapper around PGStore.ForEachByField + slice
// accumulation with the same ListAllMaxRows cap as ListAll. Callers
// that walk filtered sets too large to materialise should use
// ForEachByField directly. The streaming primitive is the source of
// truth for the SQL and consistency contract — see ForEachByField.
func (s *PGStore) ListByField(ctx context.Context, tenantID uuid.UUID, filter ListFilter, field, value string) ([]KRecord, error) {
	out := make([]KRecord, 0)
	err := s.ForEachByField(ctx, tenantID, filter, field, value, func(r KRecord) error {
		if len(out)+1 > ListAllMaxRows {
			return fmt.Errorf("%w: ktype=%s rows=%d cap=%d",
				ErrListAllExceedsCap, filter.KType, len(out)+1, ListAllMaxRows)
		}
		// Honour the ForEachFunc contract — same defensive copy as
		// ListAll above. ListByField retains every row beyond the
		// callback boundary in `out`; the store-owned slice may be
		// reused under future scan-buffer pooling, so copy r.Data
		// before appending.
		if r.Data != nil {
			cp := make(json.RawMessage, len(r.Data))
			copy(cp, r.Data)
			r.Data = cp
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ForEachByField is the streaming variant of ListByField. It walks
// every record matching filter.KType whose JSONB field `field`
// equals `value` (case-insensitive), invoking fn for each decrypted
// KRecord. Same consistency, snapshot, and per-chunk-tx semantics as
// ForEach — see ForEach for the full contract. Memory stays bounded
// to one chunk regardless of how many rows match.
//
// Use this over ListByField when the matching set is potentially
// large (e.g. all payslips across a multi-year history when filtering
// by pay_run_id is no longer enough to keep the result set small).
// Use ListByField when the matching set is bounded and the caller
// wants a slice for downstream processing.
func (s *PGStore) ForEachByField(ctx context.Context, tenantID uuid.UUID, filter ListFilter, field, value string, fn ForEachFunc) error {
	if filter.KType == "" {
		return errors.New("record: ktype filter required")
	}
	if field == "" {
		return errors.New("record: field required")
	}
	if fn == nil {
		return errors.New("record: ForEachByField callback required")
	}
	status := filter.Status
	if status == "" {
		status = "active"
	}
	return s.foreachKeyset(ctx, tenantID, func(haveLower bool, cursorTS time.Time, cursorID uuid.UUID, snapshot time.Time, chunk int) (string, []any) {
		if haveLower {
			return `SELECT id, tenant_id, ktype, ktype_version, data, status, version,
				        created_by, created_at, updated_by, updated_at, deleted_at
				 FROM krecords
				 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
				   AND lower(data->>$4) = lower($5)
				   AND updated_at <= $6
				   AND (updated_at, id) < ($7, $8)
				 ORDER BY updated_at DESC, id DESC
				 LIMIT $9`,
				[]any{tenantID, filter.KType, status, field, value, snapshot, cursorTS, cursorID, chunk}
		}
		return `SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			 FROM krecords
			 WHERE tenant_id = $1 AND ktype = $2 AND status = $3
			   AND lower(data->>$4) = lower($5)
			   AND updated_at <= $6
			 ORDER BY updated_at DESC, id DESC
			 LIMIT $7`,
			[]any{tenantID, filter.KType, status, field, value, snapshot, chunk}
	}, fn)
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

		// Resolve the KType once, on the outer transaction's
		// connection, and reuse it for decrypt + validate + encrypt.
		// Doing both of the following — using resolveKTypeInTx (not
		// the pool-based resolveKType) and decryptRecordWith (not
		// the pool-based decryptRecord) — guarantees that the
		// entire Update touches a single pooled connection even when
		// the KType is tenant-authored. Without this, a custom-KType
		// Update would acquire the outer tx's connection plus a
		// nested one for the decrypt-time KType lookup AND a third
		// nested one for the validate-time KType lookup, exhausting
		// the connection pool under modest write concurrency.
		kt, err := s.resolveKTypeInTx(ctx, tx, r.TenantID, existing.KType, existing.KTypeVersion, resolveForUpdate)
		if err != nil {
			return err
		}

		// Decrypt existing.Data before merging so the plaintext patch
		// in r.Data is shallow-merged onto plaintext, not onto ciphertext.
		// Without this the encrypted fields would be overwritten with
		// whatever ciphertext survives the merge and become unreadable.
		existingPlain, err := s.decryptRecordWith(&existing, kt)
		if err != nil {
			return fmt.Errorf("record: decrypt existing: %w", err)
		}

		merged, err := mergeJSON(existingPlain, r.Data)
		if err != nil {
			return fmt.Errorf("record: merge: %w", err)
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
		//
		// resolveKTypeInTx + decryptRecordWith keep the entire Delete
		// on the outer transaction's pool connection — see the
		// matching note in Update for the rationale.
		kt, err := s.resolveKTypeInTx(ctx, tx, tenantID, existing.KType, existing.KTypeVersion, resolveForRead)
		if err != nil {
			return fmt.Errorf("record: resolve ktype for delete: %w", err)
		}
		existingPlain, err := s.decryptRecordWith(&existing, kt)
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
		// Always encrypt what we receive: callers only pass plaintext
		// (validated user input on Create, decrypted-then-merged payload
		// on Update), so a kapp:enc:v1: prefix here is user-supplied and
		// must not be allowed to bypass encryption — doing so would
		// store garbage verbatim and break decryption on read.
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
// Runs in resolveForRead mode so historical records on draft /
// archived custom KTypes remain decryptable for the caller.
//
// This pool-based entry point opens its own tenant transaction for
// the custom-KType lookup. Call sites that already hold an outer
// tx (record.Update, record.Delete) AND have already resolved the
// KType for their own validation work must call decryptRecordWith
// instead so the per-record overhead is one pool connection rather
// than three (outer tx + KType lookup + decryption KType re-lookup).
func (s *PGStore) decryptRecord(ctx context.Context, r *KRecord) (json.RawMessage, error) {
	if s.encryptor == nil {
		return r.Data, nil
	}
	kt, err := s.resolveKType(ctx, r.TenantID, r.KType, r.KTypeVersion, resolveForRead)
	if err != nil {
		return nil, err
	}
	return s.decryptRecordWith(r, kt)
}

// decryptRecordWith is the pre-resolved variant of decryptRecord. The
// caller supplies the kt for r's (kind, version) — typically because
// they already needed it for ValidateData or encryptFields and want
// to avoid a second tenant_ktypes lookup. Beyond eliminating the
// lookup, this also lets the outer transaction's connection do all
// the work, removing the nested WithTenantTx footprint observed on
// the Update / Delete paths under load.
func (s *PGStore) decryptRecordWith(r *KRecord, kt *ktype.KType) (json.RawMessage, error) {
	if s.encryptor == nil {
		return r.Data, nil
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

// FilterFields strips fields the actor's roles are not allowed to read,
// based on a "field_permissions" block on the KType schema. A KType
// schema may carry an optional block:
//
//	"field_permissions": {
//	    "salary":  {"read": ["hr.admin","owner"], "write": ["hr.admin"]},
//	    "ssn":     {"read": ["hr.admin"],         "write": ["hr.admin"]}
//	}
//
// For each field in the block, if the actor holds none of the listed
// `read` roles, the field is removed from the response. A nil or empty
// block is a no-op (every field is returned). Fields not listed in the
// block are unrestricted by default.
//
// userRoles can be supplied directly or pulled from
// platform.UserRolesFromContext(ctx) before calling.
func FilterFields(data, schema json.RawMessage, userRoles []string) json.RawMessage {
	if len(data) == 0 || len(schema) == 0 {
		return data
	}
	rules := parseFieldPermissions(schema)
	if len(rules) == 0 {
		return data
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	roleSet := make(map[string]struct{}, len(userRoles))
	for _, r := range userRoles {
		roleSet[r] = struct{}{}
	}
	for field, rule := range rules {
		if len(rule.Read) == 0 {
			continue
		}
		if !roleSetMatchesAny(roleSet, rule.Read) {
			delete(doc, field)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

// FieldsForbiddenForWrite returns the list of fields in `data` the actor
// is not permitted to write according to the schema's
// `field_permissions` block. Returns an empty slice when every field is
// allowed (the common case) so callers can do `len(...) == 0` to detect
// success.
func FieldsForbiddenForWrite(data, schema json.RawMessage, userRoles []string) []string {
	if len(data) == 0 || len(schema) == 0 {
		return nil
	}
	rules := parseFieldPermissions(schema)
	if len(rules) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	roleSet := make(map[string]struct{}, len(userRoles))
	for _, r := range userRoles {
		roleSet[r] = struct{}{}
	}
	forbidden := make([]string, 0)
	for field := range doc {
		rule, ok := rules[field]
		if !ok {
			continue
		}
		if len(rule.Write) == 0 {
			continue
		}
		if !roleSetMatchesAny(roleSet, rule.Write) {
			forbidden = append(forbidden, field)
		}
	}
	return forbidden
}

type fieldRule struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
}

func parseFieldPermissions(schema json.RawMessage) map[string]fieldRule {
	var envelope struct {
		FieldPermissions map[string]fieldRule `json:"field_permissions"`
	}
	if err := json.Unmarshal(schema, &envelope); err != nil {
		return nil
	}
	return envelope.FieldPermissions
}

func roleSetMatchesAny(have map[string]struct{}, want []string) bool {
	for _, r := range want {
		if _, ok := have[r]; ok {
			return true
		}
	}
	return false
}
