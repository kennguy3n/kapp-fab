package tenant

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

// Env var consulted by LoadMasterKey. The value must be a 32+-byte
// secret — the deployment rotation policy documents how to roll it.
const MasterKeyEnvVar = "KAPP_MASTER_KEY"

// PrevMasterKeyEnvVar optionally holds the retiring master key during
// a rotation window. When set, DecryptString will try the primary
// key first and fall back to the previous key so encrypted fields
// written before the rotation remain readable until the backfill job
// re-encrypts them. EncryptString never uses the previous key.
const PrevMasterKeyEnvVar = "KAPP_MASTER_KEY_PREV"

// keySize is the per-tenant AES-256-GCM key length.
const keySize = 32

// infoLabel anchors the HKDF context. Including a constant label means
// a derived key cannot be interpreted as one for a different purpose
// even if the tenant id salt accidentally collides.
var infoLabel = []byte("kapp.krecord.field.v1")

// auditHMACLabel is a separate HKDF context for the audit/event redaction
// digest. Domain-separating it from the field-encryption label means a
// per-tenant audit HMAC key can never be confused with the field DEK, so
// leaking one does not compromise the other.
var auditHMACLabel = []byte("kapp.audit.hmac.v1")

// blindIndexLabel is a third HKDF context for the blind-index HMAC key.
// Domain-separating it from both the field-encryption and audit-HMAC
// labels means a blind-index key leak does not compromise field
// encryption or audit integrity, and vice versa.
var blindIndexLabel = []byte("kapp.blind.index.v1")

// ciphertextPrefix marks values that have been encrypted at the field
// level. An opaque prefix lets us distinguish legacy plaintext values
// from ciphertext without a schema migration — anything that does not
// start with this prefix is returned to callers verbatim.
const ciphertextPrefix = "kapp:enc:v1:"

// ErrMasterKeyMissing is returned by LoadMasterKey when the env var is
// unset or too short. Callers may choose to operate without per-tenant
// encryption (encryptor set to nil) in development; production deploys
// are expected to fail fast.
var ErrMasterKeyMissing = errors.New("tenant: master key missing or too short; set " + MasterKeyEnvVar)

// LoadMasterKey reads the master key from the environment. The raw
// value can be either 32+ raw bytes or a base64-encoded secret; the
// base64 path exists because most secret managers emit base64. Any
// value shorter than 32 bytes after decoding is rejected.
func LoadMasterKey() ([]byte, error) {
	return loadKeyFromEnv(MasterKeyEnvVar)
}

// FailClosedOnMissingMasterKey enforces the production posture for the
// per-tenant field-encryption master key. Pass the error returned by
// LoadMasterKey. In a production deployment (KAPP_ENV=production|prod)
// a missing/short master key is fatal: the service cannot decrypt
// tenant credentials (bank feeds, marketplace signing secrets) or
// krecord encrypted fields, so booting would silently degrade into a
// broken state. Outside production the helper returns nil so dev /
// staging keep the plaintext fallback.
//
// Call sites that already treat ErrMasterKeyMissing as "encryption
// disabled" should route that branch through this helper so production
// fails loudly:
//
//	key, err := tenant.LoadMasterKey()
//	if err != nil {
//	    if err := tenant.FailClosedOnMissingMasterKey(err); err != nil {
//	        return err
//	    }
//	    // dev/staging: proceed without encryption
//	}
func FailClosedOnMissingMasterKey(err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrMasterKeyMissing) {
		// A non-sentinel error (e.g. malformed base64) is always fatal,
		// regardless of environment — it means the operator set the var
		// to something the loader cannot parse.
		return err
	}
	if isProductionEnv() {
		return fmt.Errorf("tenant: %w in production; set %s before boot", ErrMasterKeyMissing, MasterKeyEnvVar)
	}
	return nil
}

// isProductionEnv mirrors platform.IsProductionEnv without importing
// platform (the tenant package must not depend on platform/ — see the
// KeyManager import-cycle note). It reads KAPP_ENV directly so a single
// env var drives every posture decision consistently.
func isProductionEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAPP_ENV"))) {
	case "production", "prod":
		return true
	default:
		return false
	}
}

// LoadPrevMasterKey reads the retiring master key if present. Returns
// (nil, nil) when the env var is unset — rotation is opt-in and the
// absence of a previous key is the steady state. A malformed value
// (present but shorter than 32 bytes after base64 decode) returns
// ErrMasterKeyMissing so operators notice misconfiguration.
func LoadPrevMasterKey() ([]byte, error) {
	if os.Getenv(PrevMasterKeyEnvVar) == "" {
		return nil, nil
	}
	return loadKeyFromEnv(PrevMasterKeyEnvVar)
}

func loadKeyFromEnv(env string) ([]byte, error) {
	raw := os.Getenv(env)
	if raw == "" {
		return nil, ErrMasterKeyMissing
	}
	// Accept either a raw long secret or a base64 blob. base64.StdEncoding
	// rejects non-base64 characters, so we fall back to the raw bytes on
	// decode failure.
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) >= keySize {
		return decoded, nil
	}
	if len(raw) < keySize {
		return nil, ErrMasterKeyMissing
	}
	return []byte(raw), nil
}

// DeriveKey returns a per-tenant 32-byte key derived from masterKey
// via HKDF-SHA256, using the tenant uuid as the salt. The returned
// slice is newly allocated so callers can zero it after use.
//
// The derivation is deterministic: the same (masterKey, tenantID)
// pair always produces the same key. That is the invariant the
// KeyManager relies on to re-derive keys on cache miss without
// coordinating with any other component.
func DeriveKey(masterKey []byte, tenantID uuid.UUID) ([]byte, error) {
	if len(masterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: tenant id required for key derivation")
	}
	salt := tenantID[:]
	r := hkdf.New(sha256.New, masterKey, salt, infoLabel)
	out := make([]byte, keySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("tenant: hkdf read: %w", err)
	}
	return out, nil
}

// KeyManager caches derived per-tenant keys so the HKDF round is
// paid once per tenant per TTL window. Entries for tenants that go
// silent for longer than ttl age out on next access, keeping the
// manager in line with the zero-idle-cost invariant.
//
// The cache lives entirely inside this package so the tenant layer
// does not depend on platform/ — avoiding an import cycle with
// platform/middleware.go (which imports tenant).
type KeyManager struct {
	masterKey []byte
	// prevMasterKey is the retiring key during a rotation window.
	// Non-nil enables dual-key decrypt: EncryptString always uses
	// masterKey, DecryptString tries masterKey and falls back to
	// prevMasterKey on GCM open failure.
	prevMasterKey []byte
	ttl           time.Duration
	mu            sync.Mutex
	entries       map[uuid.UUID]keyCacheEntry
	prevEntries   map[uuid.UUID][]byte
	// auditEntries caches per-tenant audit HMAC keys, derived under a
	// separate HKDF label so they are independent of the field DEKs.
	auditEntries map[uuid.UUID]keyCacheEntry
	// blindIndexEntries caches per-tenant blind-index HMAC keys,
	// derived under a third HKDF label so they are independent of both
	// the field DEKs and the audit HMAC keys.
	blindIndexEntries map[uuid.UUID]keyCacheEntry
}

type keyCacheEntry struct {
	key     []byte
	expires time.Time
}

// NewKeyManager wires a key manager around the provided master key.
// ttl == 0 disables TTL-based eviction; entries then stay cached for
// the lifetime of the process (still bounded by active tenants).
func NewKeyManager(masterKey []byte, ttl time.Duration) (*KeyManager, error) {
	return NewKeyManagerWithPrev(masterKey, nil, ttl)
}

// NewKeyManagerWithPrev is NewKeyManager with an additional retiring
// master key for the rotation window. Pass nil for prevMasterKey when
// no rotation is in progress.
func NewKeyManagerWithPrev(masterKey, prevMasterKey []byte, ttl time.Duration) (*KeyManager, error) {
	if len(masterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	if prevMasterKey != nil && len(prevMasterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	return &KeyManager{
		masterKey:         masterKey,
		prevMasterKey:     prevMasterKey,
		ttl:               ttl,
		entries:           make(map[uuid.UUID]keyCacheEntry),
		prevEntries:       make(map[uuid.UUID][]byte),
		auditEntries:      make(map[uuid.UUID]keyCacheEntry),
		blindIndexEntries: make(map[uuid.UUID]keyCacheEntry),
	}, nil
}

// prevKey derives and caches the tenant's previous-master-key. Called
// only from DecryptString after the primary key fails. Cache is not
// TTL'd — rotation windows are short and the number of tenants active
// during one is bounded by the same ceiling as the primary cache.
func (k *KeyManager) prevKey(tenantID uuid.UUID) ([]byte, error) {
	if k.prevMasterKey == nil {
		return nil, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.prevEntries[tenantID]; ok {
		return key, nil
	}
	derived, err := DeriveKey(k.prevMasterKey, tenantID)
	if err != nil {
		return nil, err
	}
	k.prevEntries[tenantID] = derived
	return derived, nil
}

// Key returns the AES-256 key for tenantID, deriving and caching it
// on first use. Safe for concurrent callers; derivation is
// serialised per-manager to avoid thundering herds on cold start.
func (k *KeyManager) Key(tenantID uuid.UUID) ([]byte, error) {
	if k == nil {
		return nil, errors.New("tenant: nil key manager")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if entry, ok := k.entries[tenantID]; ok {
		if k.ttl == 0 || time.Now().Before(entry.expires) {
			return entry.key, nil
		}
		// TTL expired — fall through to re-derive.
	}
	derived, err := DeriveKey(k.masterKey, tenantID)
	if err != nil {
		return nil, err
	}
	var expires time.Time
	if k.ttl > 0 {
		expires = time.Now().Add(k.ttl)
	}
	k.entries[tenantID] = keyCacheEntry{key: derived, expires: expires}
	return derived, nil
}

// Len reports the number of cached per-tenant keys. Primarily a test
// hook — in production the cache size mirrors the count of tenants
// that have performed at least one encrypted field op within the TTL
// window.
func (k *KeyManager) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}

// EncryptString encrypts plaintext with the tenant's derived key and
// returns a compact base64 envelope that carries the GCM nonce + ct.
// The returned string is prefixed with ciphertextPrefix so callers
// can recognise an already-encrypted value on round-trip.
func (k *KeyManager) EncryptString(tenantID uuid.UUID, plaintext string) (string, error) {
	key, err := k.Key(tenantID)
	if err != nil {
		return "", err
	}
	return encryptWithKey(key, plaintext)
}

// DecryptString reverses EncryptString. Values missing the prefix
// are returned verbatim so mixed plaintext/ciphertext columns (e.g.
// during a rollout) degrade gracefully. When a previous master key
// is configured and the current key fails GCM auth (the canonical
// signal that the ciphertext was written under the retiring key),
// DecryptString falls back to the previous key so the rotation
// window does not break reads.
func (k *KeyManager) DecryptString(tenantID uuid.UUID, value string) (string, error) {
	if !strings.HasPrefix(value, ciphertextPrefix) {
		return value, nil
	}
	key, err := k.Key(tenantID)
	if err != nil {
		return "", err
	}
	out, err := decryptWithKey(key, value)
	if err == nil {
		return out, nil
	}
	prev, perr := k.prevKey(tenantID)
	if perr != nil || prev == nil {
		return "", err
	}
	return decryptWithKey(prev, value)
}

// ReencryptString re-encrypts a value that was originally encrypted
// under the previous master key so it can be written back under the
// current key. Returns the original value unchanged when it does not
// carry the ciphertext prefix or when no previous key is configured.
// The rotation tool (scripts/rotate_master_key.sh + the cmd helper)
// uses this to migrate krecord field payloads in batches.
func (k *KeyManager) ReencryptString(tenantID uuid.UUID, value string) (string, error) {
	if !strings.HasPrefix(value, ciphertextPrefix) {
		return value, nil
	}
	plain, err := k.DecryptString(tenantID, value)
	if err != nil {
		return "", err
	}
	return k.EncryptString(tenantID, plain)
}

// IsEncrypted reports whether a value carries the envelope prefix.
// Helpful for schema-agnostic tooling (e.g. the importer reconciler)
// that wants to avoid re-encrypting an already-encrypted column.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, ciphertextPrefix)
}

// logSentinels is the set of substrings SanitizeForLog strips from any
// string destined for a log line, error message, or other non-data-plane
// surface. Operators can register additional sentinels (e.g. a tenant-
// specific token rotated during an incident) via RegisterLogSentinel.
var (
	logSentinelsMu sync.RWMutex
	logSentinels   []string
)

// RegisterLogSentinel adds a substring that SanitizeForLog will replace
// with "<redacted>". Registration is process-global and intended for
// incident-time use (e.g. "a live API key was pasted into a support
// ticket; suppress it from logs until rotation completes"). It is safe
// for concurrent use.
func RegisterLogSentinel(s string) {
	if s == "" {
		return
	}
	logSentinelsMu.Lock()
	defer logSentinelsMu.Unlock()
	logSentinels = append(logSentinels, s)
}

// SanitizeForLog returns a copy of `s` safe to write to a log line, error
// message, or any other non-data-plane surface:
//
//   - substrings carrying the field-encryption ciphertext envelope
//     ("kapp:enc:v1:...") are replaced with "<ciphertext>" so a logged
//     record value does not expose the (already-encrypted) blob, which
//     keeps logs free of opaque noise and avoids any chance of a future
//     decrypt path being pointed at harvested log text;
//   - any substring registered via RegisterLogSentinel is replaced with
//     "<redacted>".
//
// SanitizeForLog is the single chokepoint for the P1-7 log-redaction
// posture (docs/SECURITY_HARDENING_PLAN.md). Call sites that format
// record data, request bodies, or error details into log/error text
// should route the sensitive fragments through it. It is intentionally
// conservative: it only redacts known-sensitive patterns and does not
// attempt to infer sensitive values heuristically.
func SanitizeForLog(s string) string {
	if s == "" {
		return s
	}
	out := s
	// Replace ciphertext envelopes. A value carrying the prefix runs to
	// the next whitespace or string end; replacing the whole blob keeps
	// logs readable.
	if strings.Contains(out, ciphertextPrefix) {
		out = stripCiphertextBlobs(out)
	}
	logSentinelsMu.RLock()
	sentinels := logSentinels
	logSentinelsMu.RUnlock()
	for _, sent := range sentinels {
		if sent != "" && strings.Contains(out, sent) {
			out = strings.ReplaceAll(out, sent, "<redacted>")
		}
	}
	return out
}

// stripCiphertextBlobs replaces every "kapp:enc:v1:..." run (up to the
// next whitespace) with "<ciphertext>". The envelope is base64, which
// contains no whitespace, so the whitespace boundary reliably delimits
// the blob in practice.
func stripCiphertextBlobs(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], ciphertextPrefix) {
			// Skip to the end of the base64 run.
			j := i + len(ciphertextPrefix)
			for j < len(s) && !isWhitespace(s[j]) {
				j++
			}
			b.WriteString("<ciphertext>")
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', ']', '}', ',', ')':
		return true
	default:
		return false
	}
}

// HMACString returns a base64-encoded HMAC-SHA256 of `value` keyed with a
// per-tenant key derived from the master key under the audit HMAC label.
// The digest is meant for audit/event redaction: it lets the audit trail
// record that a sensitive field changed (and to which value, verifiably)
// without storing the plaintext value itself. The digest is deterministic
// for a given (master key, tenant, value) triple, so two audit rows that
// reference the same value produce the same digest and a verifier can
// detect "no change" without ever seeing the value.
//
// Returns ("", nil) when no master key is configured — callers treat the
// empty digest as "hash unavailable" and omit it from the audit context
// rather than failing the mutation. Redaction itself does not depend on
// the digest, so dev environments without a master key still redact.
func (k *KeyManager) HMACString(tenantID uuid.UUID, value string) (string, error) {
	if k == nil {
		return "", nil
	}
	key, err := k.auditHMACKey(tenantID)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// auditHMACKey derives the per-tenant audit HMAC key. It mirrors the
// field-encryption derivation but under auditHMACLabel so the two keys
// are cryptographically independent. The result is cached alongside the
// field-encryption entries so the HKDF round is paid once per tenant.
func (k *KeyManager) auditHMACKey(tenantID uuid.UUID) ([]byte, error) {
	if k == nil {
		return nil, errors.New("tenant: nil key manager")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if entry, ok := k.auditEntries[tenantID]; ok {
		if k.ttl == 0 || time.Now().Before(entry.expires) {
			return entry.key, nil
		}
	}
	if len(k.masterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	salt := tenantID[:]
	r := hkdf.New(sha256.New, k.masterKey, salt, auditHMACLabel)
	out := make([]byte, keySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("tenant: audit hkdf read: %w", err)
	}
	var expires time.Time
	if k.ttl > 0 {
		expires = time.Now().Add(k.ttl)
	}
	k.auditEntries[tenantID] = keyCacheEntry{key: out, expires: expires}
	return out, nil
}

// BlindIndex returns a base64-encoded HMAC-SHA256 of `value` keyed with
// a per-tenant key derived from the master key under the blind-index
// label. The digest is truncated to 16 bytes (128 bits) before base64
// encoding — enough collision resistance for equality lookup while
// keeping the stored index compact. The digest is deterministic for a
// given (master key, tenant, value) triple so ListByField can match on
// it without decrypting the field.
//
// Returns ("", nil) when no master key is configured — callers treat
// the empty digest as "blind index unavailable" and skip writing the
// index entry. Queries against the index simply won't match rows
// written without a master key, which is the correct dev-environment
// degradation.
func (k *KeyManager) BlindIndex(tenantID uuid.UUID, value string) (string, error) {
	if k == nil {
		return "", nil
	}
	key, err := k.blindIndexKey(tenantID)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	sum := mac.Sum(nil)
	// Truncate to 16 bytes (128 bits) for compact storage. 128 bits
	// of HMAC output is well above the birthday bound for any
	// realistic tenant record count.
	return base64.StdEncoding.EncodeToString(sum[:16]), nil
}

// blindIndexKey derives the per-tenant blind-index HMAC key. It mirrors
// the audit-HMAC derivation but under blindIndexLabel so the two keys
// are cryptographically independent. The result is cached alongside
// the other per-tenant entries so the HKDF round is paid once per
// tenant.
func (k *KeyManager) blindIndexKey(tenantID uuid.UUID) ([]byte, error) {
	if k == nil {
		return nil, errors.New("tenant: nil key manager")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if entry, ok := k.blindIndexEntries[tenantID]; ok {
		if k.ttl == 0 || time.Now().Before(entry.expires) {
			return entry.key, nil
		}
	}
	if len(k.masterKey) < keySize {
		return nil, ErrMasterKeyMissing
	}
	salt := tenantID[:]
	r := hkdf.New(sha256.New, k.masterKey, salt, blindIndexLabel)
	out := make([]byte, keySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("tenant: blind index hkdf read: %w", err)
	}
	var expires time.Time
	if k.ttl > 0 {
		expires = time.Now().Add(k.ttl)
	}
	k.blindIndexEntries[tenantID] = keyCacheEntry{key: out, expires: expires}
	return out, nil
}

func encryptWithKey(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("tenant: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("tenant: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("tenant: nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	blob := append(nonce, ct...)
	return ciphertextPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

func decryptWithKey(key []byte, value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, ciphertextPrefix))
	if err != nil {
		return "", fmt.Errorf("tenant: base64 decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("tenant: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("tenant: gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("tenant: ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("tenant: gcm open: %w", err)
	}
	return string(pt), nil
}
