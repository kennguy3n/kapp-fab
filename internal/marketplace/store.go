package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// pgUniqueViolation is the SQLSTATE for 23505 — same constant the
// other repository packages (internal/tenant/store.go,
// internal/ktype/registry.go) use to detect duplicate-row inserts.
const pgUniqueViolation = "23505"

// Store is the marketplace repository. It owns Postgres access for
// the four 000068 tables and is the only package writing them.
//
// Two pool roles matter:
//
//   - The catalog tables (extensions, versions, review_state) are
//     global and accessed via direct pool.QueryRow / pool.Exec —
//     they have no RLS policy because the marketplace catalog is a
//     shared product surface (see migration comment).
//
//   - marketplace_extension_installations is tenant-scoped via RLS;
//     every method that touches it runs inside dbutil.WithTenantTx
//     so the `app.tenant_id` GUC is set before the query.
//
// Per the EXTENSION_SPEC.md package doc, B6 (API endpoints), B4
// (webhook dispatcher), B5 (UI extensions), and B7 (review pipeline)
// all call into this Store rather than touching SQL directly. Adding
// a new query? It belongs here, not in a callsite-side helper.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store bound to the given pool. The pool MUST be
// the application pool (kapp_app role) — privileged pools (kapp_admin
// / BYPASSRLS) would skip the installations RLS policy and silently
// expose cross-tenant rows.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateExtension inserts a publisher-level listing row. Returns
// ErrConflict if (publisher, slug) is already taken, which the B6
// API translates to 409. The display_name / description / etc. are
// the publisher-visible header fields; status starts at
// `unpublished` and only flips to `listed` once a version clears
// review (B7).
func (s *Store) CreateExtension(ctx context.Context, in CreateExtensionInput) (*Extension, error) {
	if in.Publisher == "" || in.Slug == "" {
		return nil, fmt.Errorf("%w: publisher and slug required", ErrInvalidManifest)
	}
	if in.DisplayName == "" {
		return nil, fmt.Errorf("%w: display_name required", ErrInvalidManifest)
	}
	if in.Description == "" {
		return nil, fmt.Errorf("%w: description required", ErrInvalidManifest)
	}
	if in.Author == "" {
		return nil, fmt.Errorf("%w: author required", ErrInvalidManifest)
	}
	if in.License == "" {
		return nil, fmt.Errorf("%w: license required", ErrInvalidManifest)
	}
	name := in.Publisher + "." + in.Slug
	id := uuid.New()
	var out Extension
	err := s.pool.QueryRow(ctx,
		`INSERT INTO marketplace_extensions (
			id, name, publisher, slug, display_name, description, author,
			license, homepage, support_email, icon_url, status, listed_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,''), NULLIF($10,''), NULLIF($11,''), 'unpublished', NULL
		)
		RETURNING id, name, publisher, slug, display_name, description, author,
		          license, COALESCE(homepage,''), COALESCE(support_email,''), COALESCE(icon_url,''),
		          status, COALESCE(listed_version,''), created_at, updated_at`,
		id, name, in.Publisher, in.Slug, in.DisplayName, in.Description, in.Author,
		in.License, in.Homepage, in.SupportEmail, in.IconURL,
	).Scan(
		&out.ID, &out.Name, &out.Publisher, &out.Slug, &out.DisplayName, &out.Description, &out.Author,
		&out.License, &out.Homepage, &out.SupportEmail, &out.IconURL,
		&out.Status, &out.ListedVersion, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("marketplace: insert extension: %w", err)
	}
	return &out, nil
}

// CreateExtensionInput is the publisher-facing parameter block for
// CreateExtension. Required: Publisher, Slug, DisplayName,
// Description, Author, License. The publisher/slug pair MUST already
// have been validated against the spec §3 regex by the caller — the
// DB CHECK is a backstop but the validator at the manifest layer
// returns better error messages.
type CreateExtensionInput struct {
	Publisher    string
	Slug         string
	DisplayName  string
	Description  string
	Author       string
	License      string
	Homepage     string
	SupportEmail string
	IconURL      string
}

// GetExtension returns the extension row for an id. Returns
// ErrNotFound if the id does not exist.
func (s *Store) GetExtension(ctx context.Context, id uuid.UUID) (*Extension, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: id required", ErrNotFound)
	}
	var out Extension
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, publisher, slug, display_name, description, author,
		        license, COALESCE(homepage,''), COALESCE(support_email,''), COALESCE(icon_url,''),
		        status, COALESCE(listed_version,''), created_at, updated_at
		 FROM marketplace_extensions WHERE id = $1`, id,
	).Scan(
		&out.ID, &out.Name, &out.Publisher, &out.Slug, &out.DisplayName, &out.Description, &out.Author,
		&out.License, &out.Homepage, &out.SupportEmail, &out.IconURL,
		&out.Status, &out.ListedVersion, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get extension: %w", err)
	}
	return &out, nil
}

// GetExtensionByName returns the extension row for the dotted
// `<publisher>.<slug>` name. Convenience wrapper for B6 endpoints
// that take the name from the request URL.
func (s *Store) GetExtensionByName(ctx context.Context, name string) (*Extension, error) {
	if name == "" || !strings.Contains(name, ".") {
		return nil, fmt.Errorf("%w: name must be <publisher>.<slug>", ErrNotFound)
	}
	var out Extension
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, publisher, slug, display_name, description, author,
		        license, COALESCE(homepage,''), COALESCE(support_email,''), COALESCE(icon_url,''),
		        status, COALESCE(listed_version,''), created_at, updated_at
		 FROM marketplace_extensions WHERE name = $1`, name,
	).Scan(
		&out.ID, &out.Name, &out.Publisher, &out.Slug, &out.DisplayName, &out.Description, &out.Author,
		&out.License, &out.Homepage, &out.SupportEmail, &out.IconURL,
		&out.Status, &out.ListedVersion, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get extension by name: %w", err)
	}
	return &out, nil
}

// ListExtensionsFilter narrows ListExtensions to a subset of the
// catalog. Zero-value filters mean "all rows".
type ListExtensionsFilter struct {
	// Status selects rows with the given listing status. Empty
	// means "all statuses including unpublished".
	Status ExtensionStatus
	// Publisher restricts to a single publisher's extensions.
	Publisher string
	// Limit caps the number of rows returned. <=0 means "no
	// explicit limit"; the implementation enforces a hard ceiling
	// of 500 to keep one runaway listing query from buffering the
	// entire catalog into memory.
	Limit int
}

// ListExtensions returns the catalog rows matching filter, ordered
// by name. Filtering is done in SQL (so the DB's
// marketplace_extensions_status_idx is used) — the caller does not
// have to post-filter the result slice.
func (s *Store) ListExtensions(ctx context.Context, filter ListExtensionsFilter) ([]Extension, error) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if filter.Status != "" {
		if !filter.Status.Valid() {
			return nil, fmt.Errorf("%w: unknown status %q", ErrInvalidManifest, filter.Status)
		}
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.Publisher != "" {
		args = append(args, filter.Publisher)
		conditions = append(conditions, fmt.Sprintf("publisher = $%d", len(args)))
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, name, publisher, slug, display_name, description, author,
		       license, COALESCE(homepage,''), COALESCE(support_email,''), COALESCE(icon_url,''),
		       status, COALESCE(listed_version,''), created_at, updated_at
		FROM marketplace_extensions %s
		ORDER BY name ASC
		LIMIT $%d`, where, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list extensions: %w", err)
	}
	defer rows.Close()
	out := make([]Extension, 0, 16)
	for rows.Next() {
		var e Extension
		if err := rows.Scan(
			&e.ID, &e.Name, &e.Publisher, &e.Slug, &e.DisplayName, &e.Description, &e.Author,
			&e.License, &e.Homepage, &e.SupportEmail, &e.IconURL,
			&e.Status, &e.ListedVersion, &e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list extensions: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketplace: list extensions: rows: %w", err)
	}
	return out, nil
}

// UpdateExtensionStatus transitions the listing.status. Returns
// ErrNotFound if the row does not exist.
//
// Valid transitions (enforced by this method, not the DB CHECK):
//
//	unpublished → listed       (first version approved)
//	listed      → deprecated   (publisher request)
//	deprecated  → listed       (publisher un-deprecates with newer version)
//	any         → removed      (operator hard takedown)
//
// removed is terminal — once an extension is removed, the publisher
// must re-create it under a different slug. This mirrors the
// security-incident playbook (do not re-list a compromised name).
func (s *Store) UpdateExtensionStatus(ctx context.Context, id uuid.UUID, status ExtensionStatus) error {
	if id == uuid.Nil {
		return fmt.Errorf("%w: id required", ErrNotFound)
	}
	if !status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidManifest, status)
	}
	current, err := s.GetExtension(ctx, id)
	if err != nil {
		return err
	}
	if !extensionStatusTransitionAllowed(current.Status, status) {
		return fmt.Errorf("%w: cannot transition extension status from %q to %q",
			ErrInvalidManifest, current.Status, status)
	}
	// Optimistic concurrency: the UPDATE asserts the status the
	// transition graph was checked against. If a concurrent caller
	// flipped the row in the gap between GetExtension and this
	// Exec, RowsAffected==0 and the loop below re-reads the latest
	// row to decide whether the new state is consistent with the
	// requested target (idempotent re-issue of a converging
	// transition) or a true conflict to surface. Without the
	// status guard, a `listed→deprecated` and `listed→removed`
	// race could both UPDATE successfully and last-writer-wins.
	for attempt := 0; attempt < 3; attempt++ {
		tag, err := s.pool.Exec(ctx,
			`UPDATE marketplace_extensions
			   SET status = $2, updated_at = now()
			 WHERE id = $1 AND status = $3`,
			id, string(status), string(current.Status),
		)
		if err != nil {
			return fmt.Errorf("marketplace: update extension status: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		// Re-read — either the row was concurrently transitioned
		// (status no longer == current.Status) or the row was
		// deleted (ErrNotFound). Recompute the transition decision
		// against the fresh state.
		latest, err := s.GetExtension(ctx, id)
		if err != nil {
			return err
		}
		if latest.Status == status {
			// Concurrent caller landed the same target — idempotent
			// success. (Spec: status transitions are convergent.)
			return nil
		}
		if !extensionStatusTransitionAllowed(latest.Status, status) {
			return fmt.Errorf("%w: cannot transition extension status from %q to %q (concurrent change)",
				ErrInvalidManifest, latest.Status, status)
		}
		current = latest
	}
	return fmt.Errorf("marketplace: update extension status: gave up after 3 contended retries on id %s", id)
}

func extensionStatusTransitionAllowed(from, to ExtensionStatus) ExtensionStatusTransition {
	if from == to {
		return true
	}
	if from == ExtensionStatusRemoved {
		return false
	}
	switch to {
	case ExtensionStatusListed:
		return from == ExtensionStatusUnpublished || from == ExtensionStatusDeprecated
	case ExtensionStatusDeprecated:
		return from == ExtensionStatusListed
	case ExtensionStatusRemoved:
		return true
	case ExtensionStatusUnpublished:
		return false
	}
	return false
}

// ExtensionStatusTransition is a type alias purely so the package
// godoc surface is self-documenting; the value is a bool.
type ExtensionStatusTransition = bool

// SetListedVersion marks `version` as the default install target for
// the extension. Called by B7 when a version transitions to
// approved. The version MUST belong to the extension (FK +
// extension_id match) and MUST exist in marketplace_extension_versions.
func (s *Store) SetListedVersion(ctx context.Context, extensionID uuid.UUID, version string) error {
	if extensionID == uuid.Nil || version == "" {
		return fmt.Errorf("%w: extension id and version required", ErrNotFound)
	}
	// Verify the version row exists and belongs to the extension —
	// the marketplace_extensions table has no FK to versions (would
	// be a circular dependency: extensions.listed_version → versions
	// → extensions.id), so the integrity check lives here.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM marketplace_extension_versions
		               WHERE extension_id = $1 AND version = $2)`,
		extensionID, version,
	).Scan(&exists); err != nil {
		return fmt.Errorf("marketplace: set listed version: lookup: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: version %q does not exist for extension %s",
			ErrNotFound, version, extensionID)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE marketplace_extensions
		   SET listed_version = $2, updated_at = now()
		 WHERE id = $1`,
		extensionID, version,
	)
	if err != nil {
		return fmt.Errorf("marketplace: set listed version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PublishVersionInput is the parameter block for PublishVersion. The
// caller is responsible for having already parsed + validated the
// manifest and hashed the bundle — see ParseManifest and HashBundle.
// PublishVersion is intentionally narrow: it does NOT re-parse the
// manifest. The validator runs in the B6 API layer because rejection
// produces a 400 with structured per-field detail, which is best
// rendered at the HTTP boundary; once we reach the persistence call,
// the bundle is known good.
type PublishVersionInput struct {
	ExtensionID  uuid.UUID
	Manifest     *Manifest
	BundleHash   string
	BundleSize   int64
	BundleURL    string
	ManifestJSON []byte // serialised representation, persisted in JSONB column
}

// PublishVersion inserts a new immutable per-version row. Returns
// ErrConflict if (extension_id, version) is already taken (i.e. the
// same version was uploaded before — spec §3.2 "(name, version) is
// immutable"). Returns ErrBundleTooLarge if BundleSize exceeds the
// 10 MiB cap (the DB CHECK is the source of truth; this is the
// pre-flight check so we don't waste a round-trip).
func (s *Store) PublishVersion(ctx context.Context, in PublishVersionInput) (*ExtensionVersion, error) {
	if in.ExtensionID == uuid.Nil {
		return nil, fmt.Errorf("%w: extension id required", ErrNotFound)
	}
	if in.Manifest == nil {
		return nil, fmt.Errorf("%w: manifest required", ErrInvalidManifest)
	}
	if in.BundleHash == "" {
		return nil, fmt.Errorf("%w: bundle hash required", ErrInvalidManifest)
	}
	if in.BundleSize <= 0 {
		return nil, fmt.Errorf("%w: bundle size must be positive", ErrInvalidManifest)
	}
	if in.BundleSize > MaxBundleSizeBytes {
		return nil, ErrBundleTooLarge
	}
	if in.BundleURL == "" {
		return nil, fmt.Errorf("%w: bundle URL required", ErrInvalidManifest)
	}
	// Verify the extension exists — surfaces a clearer error than
	// a FK violation if a caller (e.g. a stale CLI cache) calls
	// PublishVersion with a deleted extension id.
	if _, err := s.GetExtension(ctx, in.ExtensionID); err != nil {
		return nil, err
	}
	manifestJSON := in.ManifestJSON
	if len(manifestJSON) == 0 {
		// Default-marshal the parsed manifest to JSON for the JSONB
		// column. The original YAML bytes aren't always available
		// (e.g. when the API layer parsed and discarded them); the
		// JSON form is what the catalog UI renders anyway.
		b, err := json.Marshal(in.Manifest)
		if err != nil {
			return nil, fmt.Errorf("marketplace: marshal manifest: %w", err)
		}
		manifestJSON = b
	}

	out := ExtensionVersion{
		ExtensionID:         in.ExtensionID,
		Version:             in.Manifest.Version,
		BundleHash:          in.BundleHash,
		BundleSizeBytes:     in.BundleSize,
		BundleURL:           in.BundleURL,
		Manifest:            manifestJSON,
		MinKappVersion:      in.Manifest.MinKappVersion,
		MaxKappVersion:      in.Manifest.MaxKappVersion,
		FeaturesRequired:    append([]string{}, in.Manifest.FeaturesRequired...),
		PermissionsRequired: append([]string{}, in.Manifest.PermissionsRequired...),
		KtypesCount:         len(in.Manifest.KTypes),
		WorkflowsCount:      len(in.Manifest.Workflows),
		AgentToolsCount:     len(in.Manifest.AgentTools),
		UIExtensionsCount:   len(in.Manifest.UIExtensions),
		WebhooksCount:       len(in.Manifest.WebhooksConsumed),
	}
	if out.FeaturesRequired == nil {
		out.FeaturesRequired = []string{}
	}
	if out.PermissionsRequired == nil {
		out.PermissionsRequired = []string{}
	}

	// Atomic publish: the version INSERT and the review_state
	// seed-row INSERT MUST land in the same transaction. Two
	// separate auto-committed Exec calls could leave an orphan
	// version row whose review_state row failed to insert (transient
	// DB error, process crash between the two Execs). A retry of
	// PublishVersion would then hit the (extension_id, version)
	// UNIQUE and return ErrConflict, and B7's polling LEFT-JOIN
	// (which the code comment below promises will always find a row)
	// would silently skip the orphan forever. The seed-row write is
	// idempotent (ON CONFLICT DO NOTHING) so it's safe inside the
	// same transaction as the version write.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("marketplace: publish version: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := tx.QueryRow(ctx,
		`INSERT INTO marketplace_extension_versions (
			extension_id, version, bundle_hash, bundle_size_bytes, bundle_url, manifest,
			min_kapp_version, max_kapp_version, features_required, permissions_required,
			ktypes_count, workflows_count, agent_tools_count, ui_extensions_count, webhooks_count
		) VALUES (
			$1, $2, $3, $4, $5, $6::jsonb,
			$7, NULLIF($8,''), $9, $10,
			$11, $12, $13, $14, $15
		)
		RETURNING id, published_at, yanked, COALESCE(yanked_reason,'')`,
		out.ExtensionID, out.Version, out.BundleHash, out.BundleSizeBytes, out.BundleURL, string(out.Manifest),
		out.MinKappVersion, out.MaxKappVersion, out.FeaturesRequired, out.PermissionsRequired,
		out.KtypesCount, out.WorkflowsCount, out.AgentToolsCount, out.UIExtensionsCount, out.WebhooksCount,
	).Scan(&out.ID, &out.PublishedAt, &out.Yanked, &out.YankedReason); err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: version %s already published for extension", ErrConflict, out.Version)
		}
		return nil, fmt.Errorf("marketplace: publish version: %w", err)
	}
	// Auto-create the review_state row so B7's polling queries can
	// LEFT JOIN against a guaranteed-present row instead of needing
	// COALESCE / NULL handling at every read site. The default
	// status is `submitted` (per migration default). The version
	// INSERT above and this seed INSERT are atomic via the enclosing
	// transaction — see the begin/defer-rollback at the top of this
	// branch.
	if _, err := tx.Exec(ctx,
		`INSERT INTO marketplace_extension_review_state (extension_version_id)
		 VALUES ($1)
		 ON CONFLICT (extension_version_id) DO NOTHING`,
		out.ID,
	); err != nil {
		return nil, fmt.Errorf("marketplace: seed review state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("marketplace: publish version: commit: %w", err)
	}
	committed = true
	return &out, nil
}

// GetVersion returns the version row by id. Returns ErrNotFound if
// the id does not exist.
func (s *Store) GetVersion(ctx context.Context, id uuid.UUID) (*ExtensionVersion, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: id required", ErrNotFound)
	}
	var out ExtensionVersion
	err := s.pool.QueryRow(ctx,
		`SELECT id, extension_id, version, bundle_hash, bundle_size_bytes, bundle_url, manifest::text,
		        min_kapp_version, COALESCE(max_kapp_version,''),
		        features_required, permissions_required,
		        ktypes_count, workflows_count, agent_tools_count, ui_extensions_count, webhooks_count,
		        yanked, COALESCE(yanked_reason,''), published_at
		 FROM marketplace_extension_versions WHERE id = $1`, id,
	).Scan(
		&out.ID, &out.ExtensionID, &out.Version, &out.BundleHash, &out.BundleSizeBytes, &out.BundleURL,
		scanJSONB(&out.Manifest),
		&out.MinKappVersion, &out.MaxKappVersion,
		&out.FeaturesRequired, &out.PermissionsRequired,
		&out.KtypesCount, &out.WorkflowsCount, &out.AgentToolsCount, &out.UIExtensionsCount, &out.WebhooksCount,
		&out.Yanked, &out.YankedReason, &out.PublishedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get version: %w", err)
	}
	return &out, nil
}

// GetVersionByExtensionAndVersion returns the version row for
// (extension_id, version). Returns ErrNotFound if no such row.
func (s *Store) GetVersionByExtensionAndVersion(ctx context.Context, extensionID uuid.UUID, version string) (*ExtensionVersion, error) {
	if extensionID == uuid.Nil || version == "" {
		return nil, fmt.Errorf("%w: extension id and version required", ErrNotFound)
	}
	var out ExtensionVersion
	err := s.pool.QueryRow(ctx,
		`SELECT id, extension_id, version, bundle_hash, bundle_size_bytes, bundle_url, manifest::text,
		        min_kapp_version, COALESCE(max_kapp_version,''),
		        features_required, permissions_required,
		        ktypes_count, workflows_count, agent_tools_count, ui_extensions_count, webhooks_count,
		        yanked, COALESCE(yanked_reason,''), published_at
		 FROM marketplace_extension_versions
		 WHERE extension_id = $1 AND version = $2`, extensionID, version,
	).Scan(
		&out.ID, &out.ExtensionID, &out.Version, &out.BundleHash, &out.BundleSizeBytes, &out.BundleURL,
		scanJSONB(&out.Manifest),
		&out.MinKappVersion, &out.MaxKappVersion,
		&out.FeaturesRequired, &out.PermissionsRequired,
		&out.KtypesCount, &out.WorkflowsCount, &out.AgentToolsCount, &out.UIExtensionsCount, &out.WebhooksCount,
		&out.Yanked, &out.YankedReason, &out.PublishedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketplace: get version by (ext,version): %w", err)
	}
	return &out, nil
}

// ListVersions returns the version rows for an extension ordered by
// published_at DESC (newest first), excluding yanked entries unless
// includeYanked is true. Used by B6's `GET /extensions/{name}/versions`
// listing and by the install dialog's version picker.
func (s *Store) ListVersions(ctx context.Context, extensionID uuid.UUID, includeYanked bool) ([]ExtensionVersion, error) {
	if extensionID == uuid.Nil {
		return nil, fmt.Errorf("%w: extension id required", ErrNotFound)
	}
	q := `SELECT id, extension_id, version, bundle_hash, bundle_size_bytes, bundle_url, manifest::text,
	             min_kapp_version, COALESCE(max_kapp_version,''),
	             features_required, permissions_required,
	             ktypes_count, workflows_count, agent_tools_count, ui_extensions_count, webhooks_count,
	             yanked, COALESCE(yanked_reason,''), published_at
	      FROM marketplace_extension_versions
	      WHERE extension_id = $1`
	if !includeYanked {
		q += ` AND yanked = FALSE`
	}
	q += ` ORDER BY published_at DESC`
	rows, err := s.pool.Query(ctx, q, extensionID)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list versions: %w", err)
	}
	defer rows.Close()
	out := make([]ExtensionVersion, 0, 8)
	for rows.Next() {
		var v ExtensionVersion
		if err := rows.Scan(
			&v.ID, &v.ExtensionID, &v.Version, &v.BundleHash, &v.BundleSizeBytes, &v.BundleURL,
			scanJSONB(&v.Manifest),
			&v.MinKappVersion, &v.MaxKappVersion,
			&v.FeaturesRequired, &v.PermissionsRequired,
			&v.KtypesCount, &v.WorkflowsCount, &v.AgentToolsCount, &v.UIExtensionsCount, &v.WebhooksCount,
			&v.Yanked, &v.YankedReason, &v.PublishedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list versions: scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketplace: list versions: rows: %w", err)
	}
	return out, nil
}

// YankVersion soft-removes a version. Existing installations keep
// running but new installs are refused (B6's install endpoint checks
// `yanked = FALSE` before proceeding). reason MUST be non-empty —
// the DB CHECK enforces this; the early check here gives a better
// error message than the constraint violation.
func (s *Store) YankVersion(ctx context.Context, versionID uuid.UUID, reason string) error {
	if versionID == uuid.Nil {
		return fmt.Errorf("%w: version id required", ErrNotFound)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: yank reason required", ErrInvalidManifest)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE marketplace_extension_versions
		   SET yanked = TRUE, yanked_reason = $2
		 WHERE id = $1 AND yanked = FALSE`,
		versionID, reason,
	)
	if err != nil {
		return s.translateImmutabilityError(err)
	}
	if tag.RowsAffected() == 0 {
		// Either the version doesn't exist or it was already yanked.
		// Distinguish for callers via a separate lookup.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM marketplace_extension_versions WHERE id = $1)`,
			versionID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("marketplace: yank version: lookup: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		return fmt.Errorf("%w: version already yanked", ErrConflict)
	}
	return nil
}

// translateImmutabilityError maps a Postgres P0001 raise from the
// marketplace_extension_versions_immutable_trg trigger into the
// repository's ErrImmutableVersion sentinel. Other errors pass
// through unchanged.
func (s *Store) translateImmutabilityError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "P0001" && strings.Contains(pgErr.Message, "immutable") {
			return ErrImmutableVersion
		}
	}
	return fmt.Errorf("marketplace: %w", err)
}

// InstallInput is the parameter block for Install. WebhookBase must
// be an https:// URL (DB CHECK enforces; the early validation here
// gives a clearer error).
type InstallInput struct {
	TenantID           uuid.UUID
	ExtensionID        uuid.UUID
	ExtensionVersionID uuid.UUID
	Settings           []byte // JSONB; nil → '{}'
	WebhookBase        string
	InstalledBy        *uuid.UUID
}

// Install inserts an installation row for a tenant. Runs inside
// dbutil.WithTenantTx so the RLS policy admits the row. Returns
// ErrConflict if the tenant has already installed the extension
// (regardless of version) — uninstall + reinstall is required to
// change versions. The B6 endpoint surfaces this as a 409 with
// guidance to call `PATCH /installations/{id}/version` instead.
func (s *Store) Install(ctx context.Context, in InstallInput) (*Installation, error) {
	if in.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrNotFound)
	}
	if in.ExtensionID == uuid.Nil || in.ExtensionVersionID == uuid.Nil {
		return nil, fmt.Errorf("%w: extension and version ids required", ErrNotFound)
	}
	if in.WebhookBase == "" {
		return nil, fmt.Errorf("%w: webhook_base required", ErrInvalidManifest)
	}
	if !strings.HasPrefix(strings.ToLower(in.WebhookBase), "https://") {
		return nil, fmt.Errorf("%w: webhook_base must be https://", ErrInvalidManifest)
	}
	settings := in.Settings
	if len(settings) == 0 {
		settings = []byte("{}")
	}
	var out Installation
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO marketplace_extension_installations (
				tenant_id, extension_id, extension_version_id, status, settings, webhook_base, installed_by
			) VALUES (
				$1, $2, $3, 'pending', $4::jsonb, $5, $6
			)
			RETURNING id, tenant_id, extension_id, extension_version_id, status, settings::text,
			          webhook_base, installed_by, installed_at, updated_at,
			          last_health_check_at, COALESCE(last_health_check_status,''), COALESCE(failure_reason,'')`,
			in.TenantID, in.ExtensionID, in.ExtensionVersionID,
			string(settings), in.WebhookBase, in.InstalledBy,
		)
		return row.Scan(
			&out.ID, &out.TenantID, &out.ExtensionID, &out.ExtensionVersionID, &out.Status,
			scanJSONB(&out.Settings),
			&out.WebhookBase, &out.InstalledBy, &out.InstalledAt, &out.UpdatedAt,
			&out.LastHealthCheckAt, &out.LastHealthCheckStatus, &out.FailureReason,
		)
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: tenant has already installed this extension", ErrConflict)
		}
		return nil, fmt.Errorf("marketplace: install: %w", err)
	}
	return &out, nil
}

// UpdateInstallStatus advances the lifecycle. Caller is responsible
// for setting failureReason when status = failed (CHECK enforces).
// Other transitions ignore failureReason.
//
// Transition graph (enforced by this method, not the DB CHECK):
//
//	pending     → installing | failed | uninstalled
//	installing  → active     | failed | uninstalled
//	active      → disabled   | failed | uninstalled
//	disabled    → active     | uninstalled
//	failed      → installing | uninstalled       (operator retry path)
//	uninstalled → Ø                              (terminal)
//
// uninstalled is terminal — once a tenant uninstalls, the install
// row is retained for audit (linkage to past audit events / webhook
// signatures) but the lifecycle cannot re-activate. A fresh Install
// call creates a new row with a new ID. Same posture as
// UpdateExtensionStatus's `removed` and UpdateReviewState's
// terminals — we want the at-least-once worker re-issue path to be
// safely idempotent (self-loop) and out-of-order transitions to be
// caught at the store boundary rather than corrupt downstream
// dashboards / billing.
func (s *Store) UpdateInstallStatus(ctx context.Context, tenantID, installID uuid.UUID, status InstallStatus, failureReason string) error {
	if tenantID == uuid.Nil || installID == uuid.Nil {
		return fmt.Errorf("%w: tenant id and install id required", ErrNotFound)
	}
	if !status.Valid() {
		return fmt.Errorf("%w: unknown install status %q", ErrInvalidManifest, status)
	}
	if status == InstallStatusFailed && strings.TrimSpace(failureReason) == "" {
		return fmt.Errorf("%w: failure_reason required when status=failed", ErrInvalidManifest)
	}
	var reason any
	if status == InstallStatusFailed {
		reason = failureReason
	} else {
		reason = nil
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Read current status FOR UPDATE so a concurrent transition
		// can't slip between the graph check and the UPDATE. The
		// row lock is short-lived (single round-trip) and scoped to
		// the install row; no risk of holding it across the
		// callback's network boundary.
		var currentRaw string
		if err := tx.QueryRow(ctx,
			`SELECT status
			   FROM marketplace_extension_installations
			  WHERE id = $1
			  FOR UPDATE`,
			installID,
		).Scan(&currentRaw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("marketplace: update install status: read current: %w", err)
		}
		current := InstallStatus(currentRaw)
		if !installStatusTransitionAllowed(current, status) {
			return fmt.Errorf("%w: cannot transition install status from %q to %q",
				ErrInvalidManifest, current, status)
		}
		tag, err := tx.Exec(ctx,
			`UPDATE marketplace_extension_installations
			   SET status = $2,
			       failure_reason = $3,
			       updated_at = now()
			 WHERE id = $1`,
			installID, string(status), reason,
		)
		if err != nil {
			return fmt.Errorf("marketplace: update install status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func installStatusTransitionAllowed(from, to InstallStatus) bool {
	if from == to {
		return true
	}
	if from == InstallStatusUninstalled {
		return false
	}
	switch from {
	case InstallStatusPending:
		switch to {
		case InstallStatusInstalling, InstallStatusFailed, InstallStatusUninstalled:
			return true
		}
	case InstallStatusInstalling:
		switch to {
		case InstallStatusActive, InstallStatusFailed, InstallStatusUninstalled:
			return true
		}
	case InstallStatusActive:
		switch to {
		case InstallStatusDisabled, InstallStatusFailed, InstallStatusUninstalled:
			return true
		}
	case InstallStatusDisabled:
		switch to {
		case InstallStatusActive, InstallStatusUninstalled:
			return true
		}
	case InstallStatusFailed:
		// Operator-driven retry: failed installs can re-enter the
		// installing state once the underlying cause is fixed.
		// Going directly back to active without re-installing would
		// skip the handshake/secrets validation step.
		switch to {
		case InstallStatusInstalling, InstallStatusUninstalled:
			return true
		}
	}
	return false
}

// RecordInstallHealthCheck stamps the last_health_check_* columns.
// Called from the periodic webhook health-check sweep (B4 follow-up).
// status is a free-form string but the platform's health checker
// emits {"ok","degraded","unreachable","unauthorized"} consistently.
func (s *Store) RecordInstallHealthCheck(ctx context.Context, tenantID, installID uuid.UUID, status string) error {
	if tenantID == uuid.Nil || installID == uuid.Nil {
		return fmt.Errorf("%w: tenant id and install id required", ErrNotFound)
	}
	if strings.TrimSpace(status) == "" {
		return fmt.Errorf("%w: health check status required", ErrInvalidManifest)
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE marketplace_extension_installations
			   SET last_health_check_at = now(),
			       last_health_check_status = $2,
			       updated_at = now()
			 WHERE id = $1`,
			installID, status,
		)
		if err != nil {
			return fmt.Errorf("marketplace: record health check: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetInstallation returns the install row for an id within the
// tenant. RLS ensures cross-tenant reads return ErrNotFound.
func (s *Store) GetInstallation(ctx context.Context, tenantID, installID uuid.UUID) (*Installation, error) {
	if tenantID == uuid.Nil || installID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and install id required", ErrNotFound)
	}
	var out Installation
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT id, tenant_id, extension_id, extension_version_id, status, settings::text,
			        webhook_base, installed_by, installed_at, updated_at,
			        last_health_check_at, COALESCE(last_health_check_status,''), COALESCE(failure_reason,'')
			 FROM marketplace_extension_installations
			 WHERE id = $1`, installID,
		)
		err := row.Scan(
			&out.ID, &out.TenantID, &out.ExtensionID, &out.ExtensionVersionID, &out.Status,
			scanJSONB(&out.Settings),
			&out.WebhookBase, &out.InstalledBy, &out.InstalledAt, &out.UpdatedAt,
			&out.LastHealthCheckAt, &out.LastHealthCheckStatus, &out.FailureReason,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("marketplace: get install: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListInstallationsForTenant returns every install row visible to
// the tenant (RLS-filtered). Ordered by installed_at DESC.
func (s *Store) ListInstallationsForTenant(ctx context.Context, tenantID uuid.UUID) ([]Installation, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrNotFound)
	}
	out := make([]Installation, 0, 8)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, extension_id, extension_version_id, status, settings::text,
			        webhook_base, installed_by, installed_at, updated_at,
			        last_health_check_at, COALESCE(last_health_check_status,''), COALESCE(failure_reason,'')
			 FROM marketplace_extension_installations
			 ORDER BY installed_at DESC`,
		)
		if err != nil {
			return fmt.Errorf("marketplace: list installs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var inst Installation
			if err := rows.Scan(
				&inst.ID, &inst.TenantID, &inst.ExtensionID, &inst.ExtensionVersionID, &inst.Status,
				scanJSONB(&inst.Settings),
				&inst.WebhookBase, &inst.InstalledBy, &inst.InstalledAt, &inst.UpdatedAt,
				&inst.LastHealthCheckAt, &inst.LastHealthCheckStatus, &inst.FailureReason,
			); err != nil {
				return fmt.Errorf("marketplace: list installs: scan: %w", err)
			}
			out = append(out, inst)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListInstallationsByVersion returns every install of a specific
// version, across tenants. Used by the operator-side B7 "removed"
// transition that needs to force-disable every install. Bypasses RLS
// by virtue of running the query with no tenant GUC set — only the
// operator pool (kapp_admin) should call this; the application pool
// will return zero rows because the RLS policy's USING expression
// returns false when `app.tenant_id` is unset.
//
// pool MUST be the admin pool (BYPASSRLS); calling with the
// application pool returns an empty slice silently.
func (s *Store) ListInstallationsByVersion(ctx context.Context, adminPool *pgxpool.Pool, versionID uuid.UUID) ([]Installation, error) {
	if adminPool == nil {
		return nil, errors.New("marketplace: admin pool required for cross-tenant query")
	}
	if versionID == uuid.Nil {
		return nil, fmt.Errorf("%w: version id required", ErrNotFound)
	}
	rows, err := adminPool.Query(ctx,
		`SELECT id, tenant_id, extension_id, extension_version_id, status, settings::text,
		        webhook_base, installed_by, installed_at, updated_at,
		        last_health_check_at, COALESCE(last_health_check_status,''), COALESCE(failure_reason,'')
		 FROM marketplace_extension_installations
		 WHERE extension_version_id = $1
		 ORDER BY installed_at DESC`, versionID,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list installs by version: %w", err)
	}
	defer rows.Close()
	out := make([]Installation, 0, 8)
	for rows.Next() {
		var inst Installation
		if err := rows.Scan(
			&inst.ID, &inst.TenantID, &inst.ExtensionID, &inst.ExtensionVersionID, &inst.Status,
			scanJSONB(&inst.Settings),
			&inst.WebhookBase, &inst.InstalledBy, &inst.InstalledAt, &inst.UpdatedAt,
			&inst.LastHealthCheckAt, &inst.LastHealthCheckStatus, &inst.FailureReason,
		); err != nil {
			return nil, fmt.Errorf("marketplace: list installs by version: scan: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// jsonbScanner is a pgx Scanner that captures a JSONB column as
// []byte. We use this rather than the standard *[]byte target
// because pgx returns JSONB as a string by default unless explicitly
// cast to text; scanJSONB does the cast on the SQL side
// (`column::text`) and then wraps the destination here so the
// Installation.Settings / ExtensionVersion.Manifest fields end up as
// the raw JSON bytes ready for re-emit.
type jsonbScanner struct {
	dest *[]byte
}

func scanJSONB(dest *[]byte) *jsonbScanner {
	return &jsonbScanner{dest: dest}
}

// Scan implements sql.Scanner. The pgx driver hands us either a
// string (the cast-to-text path) or a []byte; we copy the bytes into
// the destination so the caller owns a stable slice.
func (s *jsonbScanner) Scan(src any) error {
	if src == nil {
		*s.dest = nil
		return nil
	}
	switch v := src.(type) {
	case string:
		b := make([]byte, len(v))
		copy(b, v)
		*s.dest = b
	case []byte:
		b := make([]byte, len(v))
		copy(b, v)
		*s.dest = b
	default:
		return fmt.Errorf("marketplace: unexpected JSONB scan source %T", src)
	}
	return nil
}

// isUniqueViolation returns true iff err is a pgx unique-constraint
// violation. Used to translate pre-existing-row INSERTs into the
// ErrConflict sentinel.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
