package tenant

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/hkdf"
)

// EnvelopeKeyManager implements the same interfaces as KeyManager
// (FieldEncryptor, AuditHasher, BlindIndexer) but uses per-tenant DEKs
// stored in the tenant_keys table instead of deriving keys via HKDF on
// every access. The DEKs are wrapped (encrypted) by a KEK derived from
// the master key, so the database never stores plaintext DEKs.
//
// Benefits over the HKDF-only KeyManager:
//   - Per-tenant key rotation without changing the master key.
//   - Key versioning: retired DEKs remain available for decrypting
//     existing ciphertext during the re-encryption backfill window.
//   - KMS-ready: the KEK source can be switched from HKDF to a KMS
//     for business/regulated tiers without changing the DEK format.
//
// The manager is backward-compatible: if no tenant_keys row exists for
// a tenant, it generates one on first access (generating a random DEK,
// wrapping it with the KEK, and storing the wrapped form). This means
// existing tenants are transparently migrated on first encrypted write
// after the feature is enabled.
type EnvelopeKeyManager struct {
	masterKey []byte
	pool      *pgxpool.Pool
	ttl       time.Duration
	kekSource string

	mu    sync.Mutex
	cache map[uuid.UUID]envelopeCacheEntry
	// prevCache caches retired DEKs for dual-key decrypt during
	// rotation. Keyed by (tenantID, keyVersion).
	prevCache map[tenantKeyVersion][]byte
	// createLocks serializes DEK creation per tenant so concurrent
	// goroutines don't race to insert duplicate keys. Each tenant gets
	// its own mutex, created on first access.
	createLocks   map[uuid.UUID]*sync.Mutex
	createLocksMu sync.Mutex

	// auditEntries and blindIndexEntries mirror the KeyManager's
	// caches. They are derived from the DEK (not the master key) so
	// rotating the DEK also rotates the audit/blind-index keys.
	auditEntries      map[uuid.UUID]keyCacheEntry
	blindIndexEntries map[uuid.UUID]keyCacheEntry
}

type envelopeCacheEntry struct {
	dek     []byte
	version int
	expires time.Time
}

type tenantKeyVersion struct {
	tenantID uuid.UUID
	version  int
}

// kekLabel is the HKDF context for KEK derivation. Domain-separated
// from the field-encryption, audit-HMAC, and blind-index labels.
var kekLabel = []byte("kapp.kek.v1")

// wrappedDEKPrefix marks a wrapped DEK value.
const wrappedDEKPrefix = "kapp:wrap:v1:"

// NewEnvelopeKeyManager constructs an EnvelopeKeyManager backed by the
// given admin pool. The pool must connect as kapp_admin (or a role
// with SELECT + INSERT on tenant_keys). ttl controls how long cached
// DEKs are retained before re-reading from the database.
func NewEnvelopeKeyManager(masterKey []byte, pool *pgxpool.Pool, ttl time.Duration) (*EnvelopeKeyManager, error) {
	if len(masterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	return &EnvelopeKeyManager{
		masterKey:         masterKey,
		pool:              pool,
		ttl:               ttl,
		kekSource:         "hkdf",
		cache:             make(map[uuid.UUID]envelopeCacheEntry),
		prevCache:         make(map[tenantKeyVersion][]byte),
		createLocks:       make(map[uuid.UUID]*sync.Mutex),
		auditEntries:      make(map[uuid.UUID]keyCacheEntry),
		blindIndexEntries: make(map[uuid.UUID]keyCacheEntry),
	}, nil
}

// deriveKEK derives the key-encryption key from the master key for a
// given tenant. The KEK is used to wrap/unwrap DEKs stored in the
// tenant_keys table. It is NOT used for field encryption directly.
func (e *EnvelopeKeyManager) deriveKEK(tenantID uuid.UUID) ([]byte, error) {
	if len(e.masterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	salt := tenantID[:]
	r := hkdf.New(sha256.New, e.masterKey, salt, kekLabel)
	out := make([]byte, keySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("tenant: kek hkdf read: %w", err)
	}
	return out, nil
}

// wrapDEK encrypts a DEK with the KEK using AES-256-GCM.
func wrapDEK(kek, dek []byte) (string, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return "", fmt.Errorf("tenant: wrap dek cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("tenant: wrap dek gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("tenant: wrap dek nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, dek, nil)
	envelope := append(nonce, ct...)
	return wrappedDEKPrefix + base64.StdEncoding.EncodeToString(envelope), nil
}

// unwrapDEK decrypts a wrapped DEK with the KEK.
func unwrapDEK(kek []byte, wrapped string) ([]byte, error) {
	if !strings.HasPrefix(wrapped, wrappedDEKPrefix) {
		return nil, fmt.Errorf("tenant: invalid wrapped DEK prefix")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(wrapped, wrappedDEKPrefix))
	if err != nil {
		return nil, fmt.Errorf("tenant: decode wrapped DEK: %w", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("tenant: unwrap dek cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("tenant: unwrap dek gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(raw) < ns+gcm.Overhead() {
		return nil, fmt.Errorf("tenant: wrapped DEK too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	dek, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("tenant: unwrap dek gcm open: %w", err)
	}
	return dek, nil
}

// generateDEK creates a random 256-bit DEK.
func generateDEK() ([]byte, error) {
	dek := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("tenant: generate DEK: %w", err)
	}
	return dek, nil
}

// tenantKeyRow mirrors a row in the tenant_keys table.
type tenantKeyRow struct {
	TenantID   uuid.UUID
	KeyVersion int
	WrappedDEK string
	KEKSource  string
	Status     string
}

// getOrCreateDEK returns the active DEK for the tenant, generating and
// storing one if none exists. The DEK is cached for ttl duration.
// A per-tenant mutex serializes DEK creation so concurrent goroutines
// don't race to insert duplicate keys.
func (e *EnvelopeKeyManager) getOrCreateDEK(ctx context.Context, tenantID uuid.UUID) ([]byte, int, error) {
	if tenantID == uuid.Nil {
		return nil, 0, errors.New("tenant: tenant id required for DEK")
	}
	// Fast path: check cache under the global lock.
	e.mu.Lock()
	if entry, ok := e.cache[tenantID]; ok {
		if e.ttl == 0 || time.Now().Before(entry.expires) {
			e.mu.Unlock()
			return entry.dek, entry.version, nil
		}
	}
	e.mu.Unlock()

	// Slow path: acquire per-tenant lock to serialize DB access + DEK
	// creation. This prevents two goroutines from both seeing no active
	// DEK and each inserting a new one.
	e.createLocksMu.Lock()
	tLock, ok := e.createLocks[tenantID]
	if !ok {
		tLock = &sync.Mutex{}
		e.createLocks[tenantID] = tLock
	}
	e.createLocksMu.Unlock()

	tLock.Lock()
	defer tLock.Unlock()

	// Re-check cache after acquiring the per-tenant lock — another
	// goroutine may have created the DEK while we were waiting.
	e.mu.Lock()
	if entry, ok := e.cache[tenantID]; ok {
		if e.ttl == 0 || time.Now().Before(entry.expires) {
			e.mu.Unlock()
			return entry.dek, entry.version, nil
		}
	}
	e.mu.Unlock()

	kek, err := e.deriveKEK(tenantID)
	if err != nil {
		return nil, 0, err
	}

	// Try to load the active DEK from the database.
	row, err := e.loadActiveDEK(ctx, tenantID)
	if err == nil {
		dek, err := unwrapDEK(kek, row.WrappedDEK)
		if err != nil {
			return nil, 0, fmt.Errorf("tenant: unwrap active DEK: %w", err)
		}
		e.cacheDEK(tenantID, dek, row.KeyVersion)
		return dek, row.KeyVersion, nil
	}

	// No active DEK — generate one and store it.
	dek, err := generateDEK()
	if err != nil {
		return nil, 0, err
	}
	wrapped, err := wrapDEK(kek, dek)
	if err != nil {
		return nil, 0, err
	}
	version, err := e.storeDEK(ctx, tenantID, wrapped)
	if err != nil {
		return nil, 0, fmt.Errorf("tenant: store DEK: %w", err)
	}
	e.cacheDEK(tenantID, dek, version)
	return dek, version, nil
}

// loadActiveDEK reads the active tenant_keys row for a tenant.
func (e *EnvelopeKeyManager) loadActiveDEK(ctx context.Context, tenantID uuid.UUID) (*tenantKeyRow, error) {
	var row tenantKeyRow
	err := e.pool.QueryRow(ctx,
		`SELECT tenant_id, key_version, wrapped_dek, kek_source, status
		   FROM tenant_keys
		  WHERE tenant_id = $1 AND status = 'active'
		  LIMIT 1`,
		tenantID,
	).Scan(&row.TenantID, &row.KeyVersion, &row.WrappedDEK, &row.KEKSource, &row.Status)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// storeDEK inserts a new tenant_keys row with status='active' and
// retires any previous active row. Returns the new key_version.
func (e *EnvelopeKeyManager) storeDEK(ctx context.Context, tenantID uuid.UUID, wrapped string) (int, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("tenant: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Get the max version + 1 for this tenant.
	var maxVersion int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(key_version), 0) FROM tenant_keys WHERE tenant_id = $1`,
		tenantID,
	).Scan(&maxVersion)
	if err != nil {
		return 0, fmt.Errorf("tenant: query max version: %w", err)
	}
	newVersion := maxVersion + 1

	// Retire the previous active row.
	_, err = tx.Exec(ctx,
		`UPDATE tenant_keys SET status = 'retired', retired_at = now()
		  WHERE tenant_id = $1 AND status = 'active'`,
		tenantID,
	)
	if err != nil {
		return 0, fmt.Errorf("tenant: retire old DEK: %w", err)
	}

	// Insert the new active row.
	_, err = tx.Exec(ctx,
		`INSERT INTO tenant_keys (tenant_id, key_version, wrapped_dek, kek_source, status)
		 VALUES ($1, $2, $3, $4, 'active')`,
		tenantID, newVersion, wrapped, e.kekSource,
	)
	if err != nil {
		return 0, fmt.Errorf("tenant: insert DEK: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("tenant: commit DEK: %w", err)
	}
	return newVersion, nil
}

// cacheDEK stores a DEK in the in-memory cache with TTL.
func (e *EnvelopeKeyManager) cacheDEK(tenantID uuid.UUID, dek []byte, version int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var expires time.Time
	if e.ttl > 0 {
		expires = time.Now().Add(e.ttl)
	}
	e.cache[tenantID] = envelopeCacheEntry{dek: dek, version: version, expires: expires}
}

// EncryptString encrypts plaintext with the tenant's active DEK.
func (e *EnvelopeKeyManager) EncryptString(tenantID uuid.UUID, plaintext string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dek, _, err := e.getOrCreateDEK(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return encryptWithKey(dek, plaintext)
}

// DecryptString reverses EncryptString. It tries the active DEK first,
// then falls back to retired DEKs (for the rotation window).
func (e *EnvelopeKeyManager) DecryptString(tenantID uuid.UUID, value string) (string, error) {
	if !strings.HasPrefix(value, ciphertextPrefix) {
		return value, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dek, _, err := e.getOrCreateDEK(ctx, tenantID)
	if err != nil {
		return "", err
	}
	out, err := decryptWithKey(dek, value)
	if err == nil {
		return out, nil
	}
	// Fall back to retired DEKs. Load all retired rows for this tenant
	// and try each one. This is the rotation window path.
	retired, rerr := e.loadRetiredDEKs(ctx, tenantID)
	if rerr != nil || len(retired) == 0 {
		return "", err
	}
	kek, kerr := e.deriveKEK(tenantID)
	if kerr != nil {
		return "", kerr
	}
	for _, row := range retired {
		dek, err := unwrapDEK(kek, row.WrappedDEK)
		if err != nil {
			continue
		}
		out, err := decryptWithKey(dek, value)
		if err == nil {
			return out, nil
		}
	}
	return "", err
}

// loadRetiredDEKs reads all retired tenant_keys rows for a tenant.
func (e *EnvelopeKeyManager) loadRetiredDEKs(ctx context.Context, tenantID uuid.UUID) ([]tenantKeyRow, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT tenant_id, key_version, wrapped_dek, kek_source, status
		   FROM tenant_keys
		  WHERE tenant_id = $1 AND status = 'retired'
		  ORDER BY key_version DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenantKeyRow
	for rows.Next() {
		var row tenantKeyRow
		if err := rows.Scan(&row.TenantID, &row.KeyVersion, &row.WrappedDEK, &row.KEKSource, &row.Status); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// HMACString computes an audit HMAC using a key derived from the DEK
// (not the master key), so rotating the DEK also rotates the audit key.
func (e *EnvelopeKeyManager) HMACString(tenantID uuid.UUID, value string) (string, error) {
	key, err := e.derivedSubKey(tenantID, auditHMACLabel)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// BlindIndex computes a blind index digest using a key derived from
// the DEK (not the master key).
func (e *EnvelopeKeyManager) BlindIndex(tenantID uuid.UUID, value string) (string, error) {
	key, err := e.derivedSubKey(tenantID, blindIndexLabel)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	sum := mac.Sum(nil)
	return base64.StdEncoding.EncodeToString(sum[:16]), nil
}

// derivedSubKey derives a sub-key from the tenant's DEK via HKDF under
// the given label. This keeps the audit-HMAC and blind-index keys
// cryptographically independent of the field-encryption DEK while
// still being per-tenant and rotatable with the DEK.
func (e *EnvelopeKeyManager) derivedSubKey(tenantID uuid.UUID, label []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dek, _, err := e.getOrCreateDEK(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	r := hkdf.New(sha256.New, dek, tenantID[:], label)
	out := make([]byte, keySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("tenant: sub-key hkdf read: %w", err)
	}
	return out, nil
}

// RotateKey generates a new DEK for the tenant, stores it as the new
// active version, and retires the previous one. The old DEK remains
// in the cache for decrypting existing ciphertext until the re-
// encryption backfill completes.
func (e *EnvelopeKeyManager) RotateKey(ctx context.Context, tenantID uuid.UUID) (int, error) {
	kek, err := e.deriveKEK(tenantID)
	if err != nil {
		return 0, err
	}
	dek, err := generateDEK()
	if err != nil {
		return 0, err
	}
	wrapped, err := wrapDEK(kek, dek)
	if err != nil {
		return 0, err
	}
	version, err := e.storeDEK(ctx, tenantID, wrapped)
	if err != nil {
		return 0, err
	}
	e.cacheDEK(tenantID, dek, version)
	return version, nil
}

// KeyVersion returns the active key version for a tenant. Returns 0
// if no key has been provisioned yet.
func (e *EnvelopeKeyManager) KeyVersion(ctx context.Context, tenantID uuid.UUID) (int, error) {
	e.mu.Lock()
	if entry, ok := e.cache[tenantID]; ok {
		if e.ttl == 0 || time.Now().Before(entry.expires) {
			e.mu.Unlock()
			return entry.version, nil
		}
	}
	e.mu.Unlock()
	row, err := e.loadActiveDEK(ctx, tenantID)
	if err != nil {
		return 0, nil // no key provisioned yet
	}
	return row.KeyVersion, nil
}

// KeyInfo describes a tenant's key state for the privacy dashboard.
type KeyInfo struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	Version   int       `json:"key_version"`
	KEKSource string    `json:"kek_source"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ListKeyVersions returns all key versions for a tenant (active +
// retired), ordered by version descending. Used by the privacy
// dashboard and admin key-rotation UI.
func (e *EnvelopeKeyManager) ListKeyVersions(ctx context.Context, tenantID uuid.UUID) ([]KeyInfo, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT tenant_id, key_version, kek_source, status, created_at
		   FROM tenant_keys
		  WHERE tenant_id = $1
		  ORDER BY key_version DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInfo
	for rows.Next() {
		var ki KeyInfo
		if err := rows.Scan(&ki.TenantID, &ki.Version, &ki.KEKSource, &ki.Status, &ki.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ki)
	}
	return out, rows.Err()
}

// EnvelopeKeyManager satisfies the record.FieldEncryptor,
// record.AuditHasher, and record.BlindIndexer interfaces
// structurally. The compile-time assertions live in the record
// package's wiring (deps_build.go) to avoid an import cycle.
