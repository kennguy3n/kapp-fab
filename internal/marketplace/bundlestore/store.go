// Package bundlestore implements the marketplace-hosted bundle
// storage backend introduced in Phase B8 (publisher developer
// experience).
//
// Until B8, every published bundle had to live on a publisher-
// owned CDN; the marketplace stored a bundle_url + bundle_hash on
// the version row and the install-time HTTPResolver fetched bytes
// from the publisher. That model works for organisations with
// existing infra but is a hard prerequisite for smaller publishers
// and for the kapp-publish CLI we ship in this phase — the
// publisher experience cannot require "first, go set up a CDN
// somewhere with HTTPS and a stable URL."
//
// bundlestore.Store is the alternative: a content-addressed object
// store keyed by SHA-256, fronted by a metadata table
// (marketplace_bundle_uploads, see migrations/000077). Publishers
// POST tar.gz bytes; we compute the hash, dedup against the
// metadata table, store the bytes, and return a marketplace-hosted
// bundle_url of the form
//
//	https://<host>/api/v1/marketplace/bundles/<hash>.tar.gz
//
// that the publisher then passes to PublishVersion. The install-
// time HTTPResolver fetches from that URL just like any other
// publisher-hosted bundle — the resolver does not care whether the
// bytes live on R2 or on us. Marketplace-hosted bundles are exactly
// as immutable and content-checked as publisher-hosted ones.
//
// The ObjectStore interface is intentionally narrow (Put/Get/Exists)
// so a swap from the MemoryStore used in tests to S3 in production
// is a one-line wiring change in deps_build. Put MUST be idempotent
// on the key — duplicate uploads of the same content are a no-op
// at the storage layer (the metadata table provides the same
// guarantee at the SQL layer via UNIQUE(content_hash)).
//
// The package is intentionally minimal: no signed URLs, no presigned
// PUTs, no streaming-multipart hand-off. v1 buffers the bundle into
// memory (capped at marketplace.MaxBundleSizeBytes = 10 MiB) so the
// upload handler can hash + validate + store in one shot without
// holding a half-completed object in S3 on a client disconnect. A
// future B8.1 may add presigned PUTs for the large-bundle case once
// we lift the 10 MiB cap.
package bundlestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// DefaultContentType is the only content type accepted by v1. The
// CHECK constraint on marketplace_bundle_uploads.content_type
// enforces the same set; this constant is here so the upload
// handler, the metadata Store, and the migration cannot drift.
const DefaultContentType = "application/gzip"

// OrphanRetention is how long an unreferenced upload row sticks
// around before GC may reclaim it. A publisher who uploads but
// never references the bundle in a PublishVersion call gets a
// week to either commit or be cleaned up.
//
// This is a soft default; the GCUnreferenced caller (e.g. a cron
// or admin endpoint) supplies its own minAge to tune. Bumping the
// default here is the right knob if support repeatedly reports
// "I uploaded my bundle yesterday and it disappeared."
const OrphanRetention = 7 * 24 * time.Hour

// ErrBundleNotFound mirrors files.ErrNotFound for callers that want
// to distinguish "the upload metadata row does not exist" from
// generic SQL errors. The bundle-serve handler maps this to 404.
var ErrBundleNotFound = errors.New("bundlestore: bundle not found")

// ErrBundleTooLarge wraps marketplace.ErrBundleTooLarge for callers
// that interact with this package directly without importing the
// parent marketplace package. The upload handler also returns this
// directly so the size-cap-exceeded path is uniform across the API
// surface.
var ErrBundleTooLarge = marketplace.ErrBundleTooLarge

// ObjectStore is the minimal byte-level interface this package
// needs from its storage backend. Put MUST be idempotent — a put
// of the same key MUST NOT overwrite (the storage_key is content-
// addressed; identical key = identical bytes). Get MUST return
// ErrBundleNotFound on absence; any other error indicates an
// IO failure.
//
// Backends:
//   - MemoryStore: in-process map for tests.
//   - DiskStore: filesystem-backed for single-binary deploys with
//     persistent storage (e.g. an EBS volume, NFS mount, or a
//     local dev box). Survives process restart, which MemoryStore
//     does not. Selected via KAPP_MARKETPLACE_BUNDLE_DIR.
//   - S3Store (TODO B8.1): wraps internal/files/s3.go for the
//     multi-replica production rollout. Until that lands, the
//     production posture is DiskStore on a persistent volume; the
//     MemoryStore is dev/test only.
type ObjectStore interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
}

// MemoryStore is the in-process ObjectStore used for unit tests
// and for bootstrap single-binary deploys where no external object
// store is available. Safe for concurrent use; Put is idempotent
// on the key.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
	mime map[string]string
}

// NewMemoryStore constructs an empty MemoryStore. Use this in tests
// and in dev-stack wiring; production should swap in S3Store once
// B8.1 lands.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string][]byte{}, mime: map[string]string{}}
}

// Put stores data under the key. A second Put with the same key is
// a no-op so the caller's "dedup-by-hash" contract holds.
func (m *MemoryStore) Put(_ context.Context, key, contentType string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[key]; ok {
		return nil
	}
	// Copy so a subsequent mutation of the caller's slice does
	// not corrupt our stored bytes. The Resolver tests rely on
	// this invariant when they reuse a builder buffer across
	// multiple Put calls.
	m.data[key] = append([]byte(nil), data...)
	m.mime[key] = contentType
	return nil
}

// Get returns a ReadCloser over the stored payload, or
// ErrBundleNotFound. The returned reader holds a defensive copy so
// the caller can iterate without holding the store-wide lock.
func (m *MemoryStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrBundleNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

// Exists reports whether key is present in the store. Used by the
// upload handler to skip a redundant Put when the metadata row says
// the row already exists but we want to assert the bytes do too
// (defense-in-depth against a metadata-only insert that failed
// mid-flight).
func (m *MemoryStore) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.data[key]
	return ok, nil
}

// Delete removes the key from the store. Used by the GC sweeper
// after the metadata row is deleted. Missing key is a no-op.
func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	delete(m.mime, key)
	return nil
}

// DiskStore is the filesystem-backed ObjectStore used by
// production single-binary deploys that mount a persistent volume
// (EBS / NFS / local SSD) under KAPP_MARKETPLACE_BUNDLE_DIR. It
// survives process restart, which MemoryStore does not — the
// motivating Devin Review finding flagged that swapping a deploy
// process would silently lose every uploaded bundle.
//
// Layout: bytes are written under {root}/{StorageKeyForHash(hash)},
// e.g. {root}/bundles/sha256/ab/abcd….tar.gz. The two-byte
// prefix sharding keeps directory listings bounded for filesystems
// where listing a million-entry directory is expensive.
//
// Concurrency: Put is implemented as write-to-tmp-then-rename so
// a partial write never produces a half-formed object visible to
// Get. Two writers racing on the same key both produce the same
// bytes (content-addressed) so the rename is safe.
//
// This is intentionally simple — no compression, no encryption,
// no signed URLs. v1 is "write the bytes to a file, read them
// back later." S3Store remains the multi-replica answer.
type DiskStore struct {
	root string
}

// NewDiskStore constructs a DiskStore rooted at dir. The directory
// is created (with 0o750 perms) if it does not exist; if dir is
// empty the constructor returns an error so the operator does not
// silently write bundles to the process CWD.
func NewDiskStore(dir string) (*DiskStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("bundlestore: DiskStore requires a non-empty root directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("bundlestore: resolve disk root %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("bundlestore: mkdir disk root %q: %w", abs, err)
	}
	return &DiskStore{root: abs}, nil
}

// Root returns the absolute directory the DiskStore writes under.
// Exposed for observability / dashboard surfaces; never used to
// build user-visible URLs.
func (d *DiskStore) Root() string { return d.root }

// keyPath joins the key onto the store root. The key is
// canonicalised by StorageKeyForHash so it does not contain `..`
// or absolute prefixes, but Clean is applied defensively and the
// result is asserted to remain inside root so a future key-format
// change cannot escape the directory.
func (d *DiskStore) keyPath(key string) (string, error) {
	joined := filepath.Join(d.root, filepath.Clean("/"+key))
	rel, err := filepath.Rel(d.root, joined)
	if err != nil {
		return "", fmt.Errorf("bundlestore: resolve disk key %q: %w", key, err)
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("bundlestore: disk key %q escapes root", key)
	}
	return joined, nil
}

// Put writes data under key using a write-to-tmp + rename for
// atomicity. A second Put with the same key is treated as a no-op
// to match the MemoryStore contract — content-addressed bytes are
// immutable so the bytes on disk and the bytes in `data` are
// guaranteed identical.
//
// Devin Review
// ANALYSIS_pr-review-job-20b9bdccfe6d463c9a4d6ac7f0fea816_0005
// noted a Stat-then-Rename TOCTOU window: two concurrent Puts of
// the same key could both pass the os.Stat check at line below
// and both attempt the os.Rename. This is BENIGN by the content-
// addressing invariant — every key is sha256(bytes), so the bytes
// being renamed in are byte-identical to any bytes already at dst
// or being raced into dst by a concurrent writer. The os.Rename
// is atomic on POSIX filesystems, so the final on-disk state
// after both renames is one of the two identical-bytes tmp files
// — observably indistinguishable from a serialized execution.
// The unused tmp file is removed by the loser's `cleanup` defer.
// We deliberately do NOT add an O_EXCL-style "fail if exists"
// check before the rename: that would convert benign loser-races
// into spurious errors that the upload pipeline would have to
// retry-or-collapse, paying coordination cost for zero
// correctness gain.
func (d *DiskStore) Put(_ context.Context, key, _ string, data []byte) error {
	dst, err := d.keyPath(key)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		return nil // idempotent
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("bundlestore: stat disk key %q: %w", key, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("bundlestore: mkdir disk shard: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return fmt.Errorf("bundlestore: create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bundlestore: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bundlestore: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bundlestore: close temp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("bundlestore: rename to %q: %w", dst, err)
	}
	cleanup = false
	return nil
}

// Get opens the file under key for streaming read. Returns
// ErrBundleNotFound if the file does not exist.
func (d *DiskStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	dst, err := d.keyPath(key)
	if err != nil {
		return nil, err
	}
	// gosec G304 — dst is constrained by keyPath's root assertion
	// above. The caller cannot supply an arbitrary path.
	f, err := os.Open(dst) // #nosec G304
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrBundleNotFound
		}
		return nil, fmt.Errorf("bundlestore: open disk key %q: %w", key, err)
	}
	return f, nil
}

// Exists probes whether key is present. Used by the upload
// handler's metadata-row + bytes consistency check.
func (d *DiskStore) Exists(_ context.Context, key string) (bool, error) {
	dst, err := d.keyPath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(dst)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("bundlestore: stat disk key %q: %w", key, err)
}

// Delete removes the file under key. Missing file is a no-op so
// GC re-runs are idempotent. The parent shard directory is left
// in place (cheap, avoids racing with concurrent Puts of other
// hashes that happen to share a prefix).
func (d *DiskStore) Delete(_ context.Context, key string) error {
	dst, err := d.keyPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bundlestore: remove disk key %q: %w", key, err)
	}
	return nil
}

// BundleUpload is the metadata row for one stored bundle. ID is
// generated server-side; ContentHash is the SHA-256 hex of the
// bytes and is the primary natural key (UNIQUE in SQL).
// StorageKey is opaque to clients — the bundle-serve endpoint
// translates ContentHash to a streaming GET. ReferencedAt is set
// the first time a PublishVersion call references this hash; until
// then the row is GC-eligible after OrphanRetention.
type BundleUpload struct {
	ID           uuid.UUID  `json:"id"`
	ContentHash  string     `json:"content_hash"`
	SizeBytes    int64      `json:"size_bytes"`
	ContentType  string     `json:"content_type"`
	StorageKey   string     `json:"-"` // server-internal, never returned in API
	PublisherID  *uuid.UUID `json:"publisher_id,omitempty"`
	UploadedBy   *uuid.UUID `json:"uploaded_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ReferencedAt *time.Time `json:"referenced_at,omitempty"`
}

// UploadInput is the parameter block for Store.Upload. Bytes is the
// raw bundle payload (the upload handler has already validated
// gzip + tar + manifest at this point). PublisherID and UploadedBy
// are the audit-trail fields. ContentType is optional and defaults
// to DefaultContentType.
type UploadInput struct {
	Bytes       []byte
	PublisherID uuid.UUID
	UploadedBy  uuid.UUID
	ContentType string
}

// Store wraps an ObjectStore with the metadata table that lets us
// dedup, GC, and audit uploads. Construct one per process via
// NewStore. The optional admin pool, set via WithAdminPool, is
// used only by GCUnreferenced — the read/insert/update path goes
// through the primary pool.
type Store struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool // optional, required by GCUnreferenced
	objs      ObjectStore
	nower     func() time.Time
}

// NewStore wires a Store over the shared pool and an ObjectStore.
// The pool can be the kapp_app pool — marketplace_bundle_uploads
// has no tenant_id column (platform-global table, see
// migrations/000077) so RLS does not apply to it. SELECT / INSERT
// / UPDATE all run through this pool.
//
// DELETE requires kapp_admin (granted in migrations/000077:166)
// and is therefore routed through an optional admin pool wired by
// WithAdminPool — calling GCUnreferenced without that is rejected
// with ErrAdminPoolRequired rather than failing with an opaque
// "permission denied" SQL error inside the sweep.
func NewStore(pool *pgxpool.Pool, objs ObjectStore) *Store {
	if pool == nil || objs == nil {
		panic("bundlestore: NewStore requires non-nil pool and ObjectStore")
	}
	return &Store{pool: pool, objs: objs, nower: time.Now}
}

// WithAdminPool wires the BYPASSRLS / kapp_admin pool used by
// GCUnreferenced for DELETE. Idempotent; passing nil clears the
// wired pool. Returns s so the caller can chain.
func (s *Store) WithAdminPool(adminPool *pgxpool.Pool) *Store {
	if s == nil {
		return nil
	}
	s.adminPool = adminPool
	return s
}

// ErrAdminPoolRequired is returned by GCUnreferenced when the
// admin pool has not been wired via WithAdminPool. The migration
// grants DELETE on marketplace_bundle_uploads to kapp_admin only
// (see migrations/000077:166); running the sweep through the
// regular kapp_app pool would fail with an opaque "permission
// denied" SQL error. Surface a clear failure at the entrypoint
// instead, so the operator wires the admin pool intentionally.
var ErrAdminPoolRequired = errors.New("bundlestore: admin pool required for GC (only kapp_admin has DELETE)")

// SetClock swaps the internal clock for tests. nil restores time.Now.
func (s *Store) SetClock(f func() time.Time) {
	if f == nil {
		s.nower = time.Now
		return
	}
	s.nower = f
}

// HashBytes returns the lowercase-hex SHA-256 of data. Convenience
// for tests and for the upload handler — the canonical hash format
// matches marketplace.IsValidBundleHash so the value flows
// unchanged into PublishVersion.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// StorageKeyForHash returns the canonical object-store key for a
// SHA-256 hex hash. The two-byte prefix sharding (sha256/ab/abcd…)
// keeps directory listings on a filesystem backend bounded; S3 does
// not care but the prefix is harmless.
func StorageKeyForHash(hash string) string {
	if len(hash) < 4 {
		// Defensive: callers always pass a valid 64-char hash, but
		// a short-hash bug should not produce an unkeyed object.
		return "bundles/sha256/_short/" + hash
	}
	return "bundles/sha256/" + hash[:2] + "/" + hash + ".tar.gz"
}

// Upload stores the bundle bytes in the object store and records
// the metadata row, returning the BundleUpload. Idempotent on
// content_hash: a second Upload of identical bytes returns the
// existing row (with the original PublisherID / UploadedBy
// preserved — the rationale is documented in
// migrations/000077). Returns ErrBundleTooLarge when len(Bytes)
// exceeds marketplace.MaxBundleSizeBytes.
func (s *Store) Upload(ctx context.Context, in UploadInput) (*BundleUpload, error) {
	if len(in.Bytes) == 0 {
		return nil, fmt.Errorf("bundlestore: empty bundle payload")
	}
	if int64(len(in.Bytes)) > marketplace.MaxBundleSizeBytes {
		return nil, ErrBundleTooLarge
	}
	if in.PublisherID == uuid.Nil {
		return nil, fmt.Errorf("bundlestore: publisher id required")
	}
	if in.UploadedBy == uuid.Nil {
		return nil, fmt.Errorf("bundlestore: uploader id required")
	}
	ct := in.ContentType
	if ct == "" {
		ct = DefaultContentType
	}
	if ct != DefaultContentType {
		// The DB CHECK would catch this but the SQL error message
		// is opaque; surface a clear failure before round-tripping.
		return nil, fmt.Errorf("bundlestore: content type %q not accepted (only %q)", ct, DefaultContentType)
	}

	hash := HashBytes(in.Bytes)
	key := StorageKeyForHash(hash)

	// Check for an existing metadata row first. If one exists the
	// bytes are also already in the object store (the two writes
	// are issued in order: object first, then metadata, so a
	// metadata row implies a successful Put). Idempotent dedup
	// of the same hash from a different publisher returns the
	// original row unchanged.
	existing, err := s.GetByHash(ctx, hash)
	if err != nil && !errors.Is(err, ErrBundleNotFound) {
		return nil, err
	}
	if existing != nil {
		// Defensive: assert the bytes are present. If a previous
		// run inserted the metadata row but the object Put failed
		// (or the object was GC'd manually), re-Put so future
		// fetches succeed. ObjectStore.Put is idempotent so this
		// is cheap on the happy path.
		ok, exErr := s.objs.Exists(ctx, existing.StorageKey)
		if exErr != nil {
			return nil, fmt.Errorf("bundlestore: object exists probe: %w", exErr)
		}
		if !ok {
			if pErr := s.objs.Put(ctx, existing.StorageKey, existing.ContentType, in.Bytes); pErr != nil {
				return nil, fmt.Errorf("bundlestore: object re-put: %w", pErr)
			}
		}
		return existing, nil
	}

	// Fresh upload. Object store first so a metadata row never
	// dangles without bytes; the unique-on-content_hash insert
	// below is the serialisation point if two concurrent uploads
	// race on the same hash.
	if err := s.objs.Put(ctx, key, ct, in.Bytes); err != nil {
		return nil, fmt.Errorf("bundlestore: object put: %w", err)
	}

	out := &BundleUpload{
		ID:          uuid.New(),
		ContentHash: hash,
		SizeBytes:   int64(len(in.Bytes)),
		ContentType: ct,
		StorageKey:  key,
		CreatedAt:   s.nower().UTC(),
	}
	pubID := in.PublisherID
	out.PublisherID = &pubID
	uplBy := in.UploadedBy
	out.UploadedBy = &uplBy

	err = s.pool.QueryRow(ctx, `
		INSERT INTO marketplace_bundle_uploads
			(id, content_hash, size_bytes, content_type, storage_key,
			 publisher_id, uploaded_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (content_hash) DO NOTHING
		RETURNING id, created_at`,
		out.ID, out.ContentHash, out.SizeBytes, out.ContentType, out.StorageKey,
		out.PublisherID, out.UploadedBy, out.CreatedAt,
	).Scan(&out.ID, &out.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Race: a concurrent uploader of the same hash won the
			// insert. Re-read the row and return that — the
			// content is identical so the caller is satisfied.
			row, getErr := s.GetByHash(ctx, hash)
			if getErr != nil {
				return nil, fmt.Errorf("bundlestore: race rescue read: %w", getErr)
			}
			return row, nil
		}
		return nil, fmt.Errorf("bundlestore: insert metadata: %w", err)
	}
	return out, nil
}

// GetByHash returns the metadata row by content_hash, or
// ErrBundleNotFound. The hash MUST be the canonical lowercase-hex
// form; the SQL lookup is case-sensitive (the column is hex-
// constrained on insert so this is invariant-preserving).
func (s *Store) GetByHash(ctx context.Context, hash string) (*BundleUpload, error) {
	if hash == "" {
		return nil, fmt.Errorf("bundlestore: hash required")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT id, content_hash, size_bytes, content_type, storage_key,
		       publisher_id, uploaded_by, created_at, referenced_at
		  FROM marketplace_bundle_uploads
		 WHERE content_hash = $1`, hash)
	out := &BundleUpload{}
	var pubID, uplBy *uuid.UUID
	if err := row.Scan(
		&out.ID, &out.ContentHash, &out.SizeBytes, &out.ContentType, &out.StorageKey,
		&pubID, &uplBy, &out.CreatedAt, &out.ReferencedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBundleNotFound
		}
		return nil, fmt.Errorf("bundlestore: get by hash: %w", err)
	}
	out.PublisherID = pubID
	out.UploadedBy = uplBy
	return out, nil
}

// Fetch returns a ReadCloser over the bundle bytes for the given
// hash, plus the metadata row. The caller MUST Close the reader.
// Returns ErrBundleNotFound if no metadata row exists OR if the
// object store reports no bytes (the two failure modes collapse
// into one because either is "the publisher's bundle URL is
// effectively dead").
func (s *Store) Fetch(ctx context.Context, hash string) (*BundleUpload, io.ReadCloser, error) {
	row, err := s.GetByHash(ctx, hash)
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.objs.Get(ctx, row.StorageKey)
	if err != nil {
		if errors.Is(err, ErrBundleNotFound) {
			return nil, nil, ErrBundleNotFound
		}
		return nil, nil, fmt.Errorf("bundlestore: object get: %w", err)
	}
	return row, rc, nil
}

// MarkReferenced records that a PublishVersion call has consumed
// this upload by referring to its hash on a version row. After
// this point the row is no longer GC-eligible. Idempotent:
// re-marking an already-referenced row is a no-op (the column is
// only set once, capturing the first reference).
//
// The publisher-version handler calls this after a successful
// PublishVersion insert; a failed PublishVersion leaves the row
// unreferenced and the GC sweeper reclaims it after
// OrphanRetention.
//
// Returns marketplace.ErrNotFound when no upload row matches the
// hash — this happens legitimately when the publisher hosts the
// bundle on their own CDN (no marketplace upload was ever
// recorded). Callers should treat that case as best-effort and
// continue; an unknown hash is not an error from the publish
// pipeline's perspective because the version row's FK to the
// extension is the real GC anchor, not referenced_at on a
// possibly-absent upload row.
func (s *Store) MarkReferenced(ctx context.Context, hash string) error {
	if hash == "" {
		return fmt.Errorf("bundlestore: hash required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE marketplace_bundle_uploads
		   SET referenced_at = COALESCE(referenced_at, now())
		 WHERE content_hash = $1`, hash)
	if err != nil {
		return fmt.Errorf("bundlestore: mark referenced: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Bundle was not uploaded through us — publisher hosts it
		// on their own CDN. The caller (publisher-version handler)
		// is already swallowing this as best-effort; surfacing
		// ErrNotFound makes the contract honest and lets a future
		// caller (e.g. an admin tool) distinguish.
		return marketplace.ErrNotFound
	}
	return nil
}

// ListPublisherUploads returns up to limit upload rows for the
// publisher, newest first. limit <=0 selects an internal default
// of 100; values above 500 are capped to 500. Used by the
// publisher dashboard's "bundle history" view.
func (s *Store) ListPublisherUploads(ctx context.Context, publisherID uuid.UUID, limit int) ([]BundleUpload, error) {
	if publisherID == uuid.Nil {
		return nil, fmt.Errorf("bundlestore: publisher id required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, content_hash, size_bytes, content_type, storage_key,
		       publisher_id, uploaded_by, created_at, referenced_at
		  FROM marketplace_bundle_uploads
		 WHERE publisher_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, publisherID, limit)
	if err != nil {
		return nil, fmt.Errorf("bundlestore: list publisher uploads: %w", err)
	}
	defer rows.Close()
	out := make([]BundleUpload, 0, 32)
	for rows.Next() {
		var row BundleUpload
		var pubID, uplBy *uuid.UUID
		if err := rows.Scan(
			&row.ID, &row.ContentHash, &row.SizeBytes, &row.ContentType, &row.StorageKey,
			&pubID, &uplBy, &row.CreatedAt, &row.ReferencedAt,
		); err != nil {
			return nil, fmt.Errorf("bundlestore: scan upload row: %w", err)
		}
		row.PublisherID = pubID
		row.UploadedBy = uplBy
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bundlestore: iterate upload rows: %w", err)
	}
	return out, nil
}

// GCResult summarises one GCUnreferenced sweep.
type GCResult struct {
	Scanned          int
	DeletedRows      int
	DeletedObjects   int
	OrphanedObjects  int // metadata gone, object delete failed
	StorageReclaimed int64
}

// GCUnreferenced deletes upload rows older than minAge that have
// never been referenced by a PublishVersion call, and removes the
// backing object-store bytes. minAge <=0 falls back to
// OrphanRetention. Returns a per-sweep summary.
//
// Failure mode: if the metadata row is deleted but the object
// delete fails, the GCResult's OrphanedObjects counter is bumped
// and the sweep continues. An operator can re-run the sweep (or
// invoke a manual object delete) — the metadata is gone so the
// next sweep skips this row. We tolerate this rather than rolling
// back the metadata delete because re-inserting metadata for an
// object that may not exist would mislead future readers.
func (s *Store) GCUnreferenced(ctx context.Context, minAge time.Duration) (*GCResult, error) {
	// Migration 000077 grants DELETE only to kapp_admin; the
	// primary pool is kapp_app so a DELETE here would fail with
	// "permission denied" mid-sweep. Refuse upfront so the
	// operator wires the admin pool explicitly. The select query
	// is also routed through the admin pool so the scan and the
	// delete observe identical visibility.
	if s.adminPool == nil {
		return nil, ErrAdminPoolRequired
	}
	if minAge <= 0 {
		minAge = OrphanRetention
	}
	cutoff := s.nower().UTC().Add(-minAge)

	rows, err := s.adminPool.Query(ctx, `
		SELECT id, storage_key, size_bytes
		  FROM marketplace_bundle_uploads
		 WHERE referenced_at IS NULL
		   AND created_at < $1
		 ORDER BY created_at ASC
		 LIMIT 1000`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("bundlestore: gc scan: %w", err)
	}
	defer rows.Close()
	type cand struct {
		id   uuid.UUID
		key  string
		size int64
	}
	candidates := make([]cand, 0, 64)
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.key, &c.size); err != nil {
			return nil, fmt.Errorf("bundlestore: gc scan row: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bundlestore: gc iterate: %w", err)
	}

	result := &GCResult{Scanned: len(candidates)}
	for _, c := range candidates {
		tag, delErr := s.adminPool.Exec(ctx, `
			DELETE FROM marketplace_bundle_uploads
			 WHERE id = $1
			   AND referenced_at IS NULL
			   AND created_at < $2`, c.id, cutoff)
		if delErr != nil {
			return result, fmt.Errorf("bundlestore: gc delete metadata: %w", delErr)
		}
		if tag.RowsAffected() == 0 {
			// Raced with a concurrent PublishVersion that
			// marked it referenced between scan and delete. Skip.
			continue
		}
		result.DeletedRows++
		result.StorageReclaimed += c.size
		if err := s.objs.Delete(ctx, c.key); err != nil {
			result.OrphanedObjects++
			continue
		}
		result.DeletedObjects++
	}
	return result, nil
}
