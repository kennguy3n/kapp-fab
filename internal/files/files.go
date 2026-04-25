// Package files hosts the Phase F attachment layer. It stores binary
// blobs in an object store (S3/MinIO) keyed by SHA-256 so that two
// uploads of the same content share one physical object, and records
// per-tenant metadata in the `files` table so RLS keeps attachments
// tenant-isolated regardless of de-duplication across tenants.
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ErrNotFound is returned when a file metadata row does not exist for
// the requesting tenant. Used to map to HTTP 404.
var ErrNotFound = errors.New("files: not found")

// File is the metadata row for a stored attachment. The storage key
// is a content-addressable path inside the object store; the same
// content_hash is reused across tenants but each tenant owns its own
// metadata row, so RLS keeps visibility tenant-local.
type File struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	StorageKey  string    `json:"storage_key"`
	ContentHash string    `json:"content_hash"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	UploadedBy  uuid.UUID `json:"uploaded_by"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

// Blob represents a raw byte payload with its declared content type.
// Callers build one from multipart/form-data or the request body and
// pass it to Store.Upload.
type Blob struct {
	ContentType string
	Data        []byte
}

// ObjectStore is the minimal put/get interface over the backing
// object storage. Concrete implementations target S3, MinIO, or an
// in-memory buffer for unit tests. Put MUST be idempotent on the
// storage key — a Put of the same content is a no-op.
type ObjectStore interface {
	Put(ctx context.Context, key string, contentType string, data []byte) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// MemoryStore is the in-process ObjectStore used for tests and for
// bootstrap deployments where no external object store is available.
// Safe for concurrent use.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
	mime map[string]string
}

// NewMemoryStore constructs an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string][]byte{}, mime: map[string]string{}}
}

// Put stores data under the key. A subsequent Put with the same key
// is a no-op so the caller can safely dedup by content hash.
func (m *MemoryStore) Put(_ context.Context, key, contentType string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[key]; ok {
		return nil
	}
	m.data[key] = append([]byte(nil), data...)
	m.mime[key] = contentType
	return nil
}

// Get returns a ReadCloser over the stored payload.
func (m *MemoryStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(&byteReader{b: append([]byte(nil), b...)}), nil
}

// Store persists file uploads and their metadata. It combines the
// object-store Put with an `INSERT INTO files` under tenant context
// so a concurrent put from a second tenant with the same content
// dedups at the storage layer but still gets its own metadata row.
type Store struct {
	pool  *pgxpool.Pool
	objs  ObjectStore
	nower func() time.Time
}

// NewStore wires a Store over the shared pool and object store.
func NewStore(pool *pgxpool.Pool, objs ObjectStore) *Store {
	return &Store{pool: pool, objs: objs, nower: time.Now}
}

// Upload stores the blob in the object store (keyed by SHA-256),
// writes the per-tenant metadata row, and returns the hydrated File.
func (s *Store) Upload(
	ctx context.Context,
	tenantID uuid.UUID,
	uploaderID uuid.UUID,
	blob Blob,
) (*File, error) {
	if tenantID == uuid.Nil || uploaderID == uuid.Nil {
		return nil, errors.New("files: tenant_id and uploader required")
	}
	if len(blob.Data) == 0 {
		return nil, errors.New("files: empty payload")
	}
	sum := sha256.Sum256(blob.Data)
	hash := hex.EncodeToString(sum[:])
	key := "sha256/" + hash[:2] + "/" + hash
	// Thread the tenant id through so the per-tenant ZK fabric
	// router can resolve to this tenant's bucket. Stores that
	// don't care (MemoryStore, the global S3Store) ignore it.
	objCtx := WithTenant(ctx, tenantID)
	if err := s.objs.Put(objCtx, key, blob.ContentType, blob.Data); err != nil {
		return nil, fmt.Errorf("files: put: %w", err)
	}

	out := &File{
		ID:          uuid.New(),
		TenantID:    tenantID,
		StorageKey:  key,
		ContentHash: hash,
		ContentType: fallback(blob.ContentType, "application/octet-stream"),
		SizeBytes:   int64(len(blob.Data)),
		UploadedBy:  uploaderID,
		UploadedAt:  s.nower().UTC(),
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO files
			     (id, tenant_id, storage_key, content_hash, content_type,
			      size_bytes, uploaded_by, uploaded_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			 RETURNING uploaded_at`,
			out.ID, out.TenantID, out.StorageKey, out.ContentHash,
			out.ContentType, out.SizeBytes, out.UploadedBy, out.UploadedAt,
		).Scan(&out.UploadedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("files: insert metadata: %w", err)
	}
	return out, nil
}

// Get returns the metadata row for the file or ErrNotFound.
func (s *Store) Get(ctx context.Context, tenantID, fileID uuid.UUID) (*File, error) {
	var f File
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, storage_key, content_hash, content_type,
			        size_bytes, uploaded_by, uploaded_at
			   FROM files
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, fileID,
		).Scan(&f.ID, &f.TenantID, &f.StorageKey, &f.ContentHash,
			&f.ContentType, &f.SizeBytes, &f.UploadedBy, &f.UploadedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &f, nil
}

// Read returns the metadata row plus a ReadCloser over the backing
// object bytes. Callers are responsible for closing the reader.
func (s *Store) Read(ctx context.Context, tenantID, fileID uuid.UUID) (*File, io.ReadCloser, error) {
	f, err := s.Get(ctx, tenantID, fileID)
	if err != nil {
		return nil, nil, err
	}
	objCtx := WithTenant(ctx, tenantID)
	rc, err := s.objs.Get(objCtx, f.StorageKey)
	if err != nil {
		return nil, nil, err
	}
	return f, rc, nil
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
