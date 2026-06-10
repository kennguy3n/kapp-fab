package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// OIDCLoginState is the transient per-login state the Authorization-Code
// flow stashes between the /login redirect and the /callback. It is
// confidentiality- AND integrity-sensitive: Verifier is the PKCE secret
// and a tampered State/Nonce would defeat the CSRF / replay bindings.
// It is therefore always carried in an AEAD-sealed cookie produced by
// OIDCStateCodec, never in plaintext.
type OIDCLoginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	// ReturnTo is an app-relative path the callback redirects to after
	// a successful login. It is validated by the handler to be a
	// same-site absolute path (leading "/", no "//") before use so it
	// cannot be turned into an open redirect.
	ReturnTo string `json:"r,omitempty"`
	// IssuedAt is the unix-seconds timestamp the state was minted, used
	// to reject stale login attempts independent of the cookie's own
	// Max-Age (defence in depth against a replayed cookie).
	IssuedAt int64 `json:"t"`
}

// OIDCStateCodec seals/opens OIDCLoginState with AES-256-GCM. The same
// codec instance is shared by the /login and /callback handlers; it is
// safe for concurrent use (the AEAD is stateless and a fresh nonce is
// drawn per Seal).
type OIDCStateCodec struct {
	aead cipher.AEAD
	// maxAge bounds how long a sealed state is accepted by Open,
	// independent of the cookie Max-Age the browser enforces.
	maxAge time.Duration
}

// NewOIDCStateCodec builds a codec from a 32-byte key. Keys shorter or
// longer than 32 bytes are rejected so callers cannot accidentally
// weaken the cipher; derive a 32-byte key with
// DeriveOIDCStateKey when starting from an arbitrary secret.
func NewOIDCStateCodec(key []byte, maxAge time.Duration) (*OIDCStateCodec, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("auth: oidc state key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc state cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc state gcm: %w", err)
	}
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	return &OIDCStateCodec{aead: aead, maxAge: maxAge}, nil
}

// DeriveOIDCStateKey derives a stable 32-byte AES key from an arbitrary
// secret (e.g. the deployment's existing JWT secret) via SHA-256. This
// lets a deployment reuse an already-managed secret rather than
// provisioning a dedicated IAM_CORE_COOKIE_KEY, keeping the integration
// zero-ops while still allowing an explicit dedicated key. The secret
// must be non-empty.
func DeriveOIDCStateKey(secret string) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("auth: cannot derive oidc state key from empty secret")
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

// GenerateOIDCStateKey returns a fresh random 32-byte key. Used as the
// dev-only fallback when no key is configured; it is ephemeral (a
// restart invalidates in-flight logins) and NOT safe for multi-replica
// deployments, which must configure a shared key.
func GenerateOIDCStateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("auth: generate oidc state key: %w", err)
	}
	return key, nil
}

// Seal marshals and encrypts the state, returning a URL-safe base64
// string suitable for a cookie value. A random 12-byte GCM nonce is
// prepended to the ciphertext.
func (c *OIDCStateCodec) Seal(st OIDCLoginState) (string, error) {
	if st.IssuedAt == 0 {
		st.IssuedAt = time.Now().Unix()
	}
	plaintext, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("auth: marshal oidc state: %w", err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("auth: oidc state nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts and validates a sealed state string. It returns an
// error when the value is malformed, fails authentication (tampered or
// wrong key), or is older than the codec's maxAge.
func (c *OIDCStateCodec) Open(value string) (*OIDCLoginState, error) {
	if value == "" {
		return nil, errors.New("auth: empty oidc state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("auth: decode oidc state: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("auth: oidc state too short")
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: open oidc state: %w", err)
	}
	var st OIDCLoginState
	if err := json.Unmarshal(plaintext, &st); err != nil {
		return nil, fmt.Errorf("auth: unmarshal oidc state: %w", err)
	}
	if st.IssuedAt > 0 {
		age := time.Since(time.Unix(st.IssuedAt, 0))
		if age > c.maxAge {
			return nil, fmt.Errorf("auth: oidc state expired (age %s > %s)", age, c.maxAge)
		}
	}
	return &st, nil
}
