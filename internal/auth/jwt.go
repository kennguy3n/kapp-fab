// Package auth implements Kapp's authentication layer: JWT issuance and
// validation, KChat SSO exchange, session storage with revocation, and
// an HTTP middleware that replaces the Phase A X-Tenant-ID / X-User-ID
// header scheme with claims decoded from a signed Bearer token.
//
// The JWT signer is HS256-only for Phase H. RS256 is supported for
// validation so a deployment can rotate to asymmetric keys without a
// breaking migration — the configured algorithm is declared at
// SignerConfig construction time and callers swap the Signer in place.
package auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Algorithm identifies the JWS signing algorithm. Kept as a string so
// the header value is the canonical wire form — "HS256" / "RS256".
type Algorithm string

const (
	// AlgHS256 is HMAC-SHA256 over the signing input, keyed by a
	// shared symmetric secret. Used by the default local dev signer.
	AlgHS256 Algorithm = "HS256"
	// AlgRS256 is RSA-SHA256 asymmetric signing. Used when a
	// deployment wants to hand public keys to external validators.
	AlgRS256 Algorithm = "RS256"
)

// Claims is the Kapp-specific JWT claim set. TenantID is the single
// load-bearing claim for multi-tenancy: the auth middleware will
// SET LOCAL app.tenant_id = Claims.TenantID on every request, so a
// forged or mismatched claim cannot reach another tenant's rows.
type Claims struct {
	UserID    uuid.UUID `json:"uid"`
	TenantID  uuid.UUID `json:"tid"`
	Roles     []string  `json:"roles,omitempty"`
	SessionID uuid.UUID `json:"sid,omitempty"`
	// Scope narrows the token's surface. Empty (default) means a
	// standard user session with full KApp access; "portal" means
	// an external customer session that may only hit the /portal
	// endpoints and only rows scoped to its Email.
	Scope string `json:"scope,omitempty"`
	// Email is set for portal tokens so downstream handlers can
	// filter helpdesk tickets by customer_email without a second
	// portal_users lookup on every request.
	Email string `json:"email,omitempty"`
	// IsPlatformAdmin grants access to control-plane routes
	// (/api/v1/tenants/*, /api/v1/admin/*, KType registration). It
	// is issued only by the SSO path when users.is_platform_admin
	// is TRUE for the resolved user, OR the bootstrap
	// KAPP_PLATFORM_ADMIN_USERS env var lists the user's *KChat ID*
	// (see internal/auth/sso.go::bootstrapAdmin — keyed on the
	// SSO-provider-stable identifier the operator knows in advance,
	// NOT on the Kapp internal UUID). Tenant member roles cannot
	// grant this claim — it is a property of the Kapp install, not
	// of any single tenant.
	IsPlatformAdmin bool `json:"platform_admin,omitempty"`
	// Standard JWT claims (subset we actually use).
	Issuer    string `json:"iss,omitempty"`
	Audience  string `json:"aud,omitempty"`
	IssuedAt  int64  `json:"iat,omitempty"`
	ExpiresAt int64  `json:"exp,omitempty"`
	NotBefore int64  `json:"nbf,omitempty"`
	JWTID     string `json:"jti,omitempty"`
}

// Valid returns nil when the claim set is well-formed, has not
// expired, and the NotBefore window has opened. Leeway is applied in
// opposite directions for the two time checks:
//
//   - For ExpiresAt, we reject when now-leeway >= exp (more lenient:
//     still accept a token that expired within the leeway window).
//   - For NotBefore, we reject when now+leeway < nbf (also more
//     lenient: accept a token whose nbf is at most leeway in the
//     future so a freshly-issued token doesn't bounce under clock
//     skew).
//
// Subtracting leeway from a single "now" and using it for both checks
// — as an earlier revision did — silently inverted the NotBefore
// direction and caused every fresh token to fail for the first
// leeway-worth of seconds.
func (c *Claims) Valid(now time.Time, leeway time.Duration) error {
	if c.UserID == uuid.Nil {
		return errors.New("auth: claim uid missing")
	}
	if c.TenantID == uuid.Nil {
		return errors.New("auth: claim tid missing")
	}
	if c.ExpiresAt > 0 && now.Add(-leeway).Unix() >= c.ExpiresAt {
		return ErrTokenExpired
	}
	if c.NotBefore > 0 && now.Add(leeway).Unix() < c.NotBefore {
		return errors.New("auth: token not yet valid")
	}
	return nil
}

// Sentinel errors the API surface maps to 401/403.
var (
	ErrTokenInvalid   = errors.New("auth: token invalid")
	ErrTokenExpired   = errors.New("auth: token expired")
	ErrTokenSignature = errors.New("auth: token signature invalid")
)

// SignerConfig is the static configuration for a Signer. TTL governs
// both access-token and refresh-token expiry; ARCHITECTURE.md §9 calls
// for short-lived access tokens plus a longer refresh window, so we
// model them as two TTLs and the refresh path reuses the same signer.
//
// Single-key vs keyring: callers may populate either {Algorithm,
// HMACKey | RSAPrivate | RSAPublic} (the pre-PR-6 form, preserved
// for backwards compatibility with every existing test and call
// site) OR KeyRing (the PR-6 rotation form). Both fields populated
// is a configuration error and NewSigner refuses. The Algorithm
// field is still required when KeyRing is set so the encoder knows
// which JWS algorithm header to stamp.
type SignerConfig struct {
	Algorithm Algorithm
	// HMACKey is consulted when Algorithm == AlgHS256.
	HMACKey []byte
	// RSAPrivate is consulted when Algorithm == AlgRS256 for
	// issuance; RSAPublic is used for verification only.
	RSAPrivate *rsa.PrivateKey
	RSAPublic  *rsa.PublicKey
	Issuer     string
	Audience   string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// Leeway absorbs clock skew between issuer and validator. Small
	// (30s–60s) values are typical; 0 disables the grace window.
	Leeway time.Duration
	// KeyRing, when non-nil, supersedes the single-key fields:
	// the encoder picks material from KeyRing.Primary() and stamps
	// the kid header; the verifier picks material by kid header,
	// falling back to the primary when the token carries no kid
	// (legacy access tokens minted before rotation was enabled).
	KeyRing *KeyRing
}

// Signer issues and validates JWTs under a single static config. A
// deployment that rotates keys keeps one Signer per key and picks the
// live one at issuance time; rollback is a swap of the pointer.
type Signer struct {
	cfg SignerConfig
	now func() time.Time
	// refresherDone, when non-nil, is closed by the keyring
	// refresher goroutine started by SignerFromProvider once
	// it has fully exited. Callers can wait on this channel
	// during shutdown to join the goroutine before tearing
	// down resources the refresher depends on (e.g. the
	// secrets.Provider's gRPC connection), avoiding a race
	// where the refresher is still mid-RPC when the provider
	// is closed. Nil on signers built without a refresher
	// (SignerFromEnv, NewSigner with no provider).
	refresherDone <-chan struct{}
}

// RefresherDone returns a channel that is closed once the
// background keyring refresher (if any) has fully exited. Use
// during shutdown to wait for the refresher to drain in-flight
// provider RPCs before closing the provider — without this
// join, a provider Close() can race with an in-flight refresh
// RPC and produce shutdown-time errors. Returns nil for signers
// without a refresher (legacy single-key path, env path,
// one-shot CLI use): callers should handle the nil case as
// "no goroutine to wait on".
func (s *Signer) RefresherDone() <-chan struct{} {
	return s.refresherDone
}

// NewSigner validates the config and returns a Signer ready to issue
// and verify tokens. Missing required fields return an error so bad
// boots fail fast rather than minting invalid tokens.
func NewSigner(cfg SignerConfig) (*Signer, error) {
	switch cfg.Algorithm {
	case AlgHS256:
		if cfg.KeyRing == nil && len(cfg.HMACKey) < 32 {
			return nil, errors.New("auth: HS256 requires >=32-byte key")
		}
	case AlgRS256:
		if cfg.KeyRing == nil && cfg.RSAPrivate == nil && cfg.RSAPublic == nil {
			return nil, errors.New("auth: RS256 requires a key")
		}
		// Derive RSAPublic from RSAPrivate when only the private
		// key is supplied. Without this, an operator who configures
		// "RS256 with only a private key" (a reasonable shape —
		// the public key IS the private key's PublicKey field) hits
		// a silent-failure mode where every Verify call returns
		// ErrTokenSignature because verifySignature skips
		// candidates with nil RSAPublic. The pre-keyring code
		// surfaced this as "auth: RS256 verifier missing public
		// key" at construction time; deriving is strictly better
		// because the deployment works correctly instead of
		// failing loudly. Operators who want a verify-only signer
		// (RSAPublic without RSAPrivate) are unaffected — that
		// path was already valid and stays valid.
		if cfg.KeyRing == nil && cfg.RSAPrivate != nil && cfg.RSAPublic == nil {
			cfg.RSAPublic = &cfg.RSAPrivate.PublicKey
		}
	default:
		return nil, fmt.Errorf("auth: unsupported algorithm %q", cfg.Algorithm)
	}
	if cfg.KeyRing != nil && (len(cfg.HMACKey) > 0 || cfg.RSAPrivate != nil || cfg.RSAPublic != nil) {
		return nil, errors.New("auth: SignerConfig must use either KeyRing or single-key fields, not both")
	}
	if cfg.KeyRing != nil {
		// Reject a ring whose primary or any verifier was
		// declared with a different algorithm class than the
		// signer. Without this guard, a misconfigured operator
		// could ship an HS256 primary with an RS256 verifier in
		// the ring — the verify path would call
		// rsa.VerifyPKCS1v15 with a nil public key and the boot
		// would succeed, only to fail every token at runtime.
		// We re-check every member of the ring rather than
		// trusting AddVerifier to have done it: KeyRing exposes
		// SwapPrimary / Remove and could be mutated in tests
		// to bypass that check.
		primary := cfg.KeyRing.Primary()
		if primary.Algorithm != "" && primary.Algorithm != cfg.Algorithm {
			return nil, fmt.Errorf("auth: KeyRing primary algorithm %q does not match signer algorithm %q",
				primary.Algorithm, cfg.Algorithm)
		}
		for _, k := range cfg.KeyRing.All() {
			if k.Algorithm != "" && k.Algorithm != cfg.Algorithm {
				return nil, fmt.Errorf("auth: KeyRing verifier %q algorithm %q does not match signer algorithm %q",
					k.KID, k.Algorithm, cfg.Algorithm)
			}
		}
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = 15 * time.Minute
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 24 * time.Hour
	}
	return &Signer{cfg: cfg, now: time.Now}, nil
}

// Algorithm reports the signature algorithm the signer was
// constructed with — useful for boot diagnostics that want to log
// the actual crypto posture rather than a configured-but-maybe-
// ignored value. (SignerFromEnv unconditionally builds HS256
// regardless of what KAPP_JWT_ALGORITHM is set to; callers that
// log the configured algorithm without consulting this accessor
// can mislead operators into thinking their tokens are
// asymmetrically signed when they are not.)
func (s *Signer) Algorithm() Algorithm {
	return s.cfg.Algorithm
}

// Leeway reports the validation leeway the signer was constructed
// with — analogous diagnostic to Algorithm() above. SignerFromEnv
// hardcodes 30s regardless of cfg.JWTLeeway.
func (s *Signer) Leeway() time.Duration {
	return s.cfg.Leeway
}

// Issue mints an access token from the supplied claim set. The
// Issuer, Audience, IssuedAt, and ExpiresAt fields are populated
// from the signer config; callers fill in UserID/TenantID/Roles.
func (s *Signer) Issue(base Claims) (string, error) {
	now := s.now().UTC()
	c := base
	if c.Issuer == "" {
		c.Issuer = s.cfg.Issuer
	}
	if c.Audience == "" {
		c.Audience = s.cfg.Audience
	}
	c.IssuedAt = now.Unix()
	c.NotBefore = now.Unix()
	c.ExpiresAt = now.Add(s.cfg.AccessTTL).Unix()
	if c.JWTID == "" {
		c.JWTID = uuid.NewString()
	}
	return s.encode(c)
}

// IssueWithTTL mints an access-scoped token with a caller-supplied
// lifetime. It is functionally equivalent to Issue except that the
// ExpiresAt claim is stamped off `ttl` rather than the signer's
// configured AccessTTL, so scopes with a non-default session length
// (portal customers, short-lived machine tokens, etc.) don't have
// to post-process the token they just signed. A zero or negative
// ttl falls back to the signer's AccessTTL.
func (s *Signer) IssueWithTTL(base Claims, ttl time.Duration) (string, error) {
	now := s.now().UTC()
	c := base
	if c.Issuer == "" {
		c.Issuer = s.cfg.Issuer
	}
	if c.Audience == "" {
		c.Audience = s.cfg.Audience
	}
	if ttl <= 0 {
		ttl = s.cfg.AccessTTL
	}
	c.IssuedAt = now.Unix()
	c.NotBefore = now.Unix()
	c.ExpiresAt = now.Add(ttl).Unix()
	if c.JWTID == "" {
		c.JWTID = uuid.NewString()
	}
	return s.encode(c)
}

// IssueRefresh mints a long-lived refresh token. A refresh token
// carries the same UserID + TenantID but a distinct JWTID and a
// "refresh" audience so the validator can reject it on the access
// path.
func (s *Signer) IssueRefresh(base Claims) (string, error) {
	now := s.now().UTC()
	c := base
	c.Issuer = s.cfg.Issuer
	c.Audience = s.cfg.Audience + ".refresh"
	c.IssuedAt = now.Unix()
	c.NotBefore = now.Unix()
	c.ExpiresAt = now.Add(s.cfg.RefreshTTL).Unix()
	if c.JWTID == "" {
		c.JWTID = uuid.NewString()
	}
	return s.encode(c)
}

// Verify decodes and validates an access-token compact-JWS, returning
// the carried claims. The signature is checked under the signer's
// algorithm; mismatch returns ErrTokenSignature without leaking why.
// The audience claim must equal the signer's configured Audience —
// refresh tokens (aud = Audience + ".refresh") are rejected on this
// path. Callers handling refresh tokens must use VerifyRefresh.
func (s *Signer) Verify(token string) (*Claims, error) {
	return s.verify(token, s.cfg.Audience)
}

// VerifyRefresh decodes and validates a refresh-token compact-JWS.
// Separated from Verify so a refresh token cannot be used on the
// access path (or vice versa): the audience claim must equal the
// configured Audience + ".refresh".
func (s *Signer) VerifyRefresh(token string) (*Claims, error) {
	return s.verify(token, s.cfg.Audience+".refresh")
}

func (s *Signer) verify(token, expectedAudience string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenInvalid
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenSignature, err)
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header decode: %w", ErrTokenInvalid, err)
	}
	var header struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hdrJSON, &header); err != nil {
		return nil, fmt.Errorf("%w: header parse: %w", ErrTokenInvalid, err)
	}
	candidates, err := s.selectVerificationCandidates(header.Alg, header.KID)
	if err != nil {
		return nil, err
	}
	if !s.verifySignature(signingInput, sig, candidates) {
		return nil, ErrTokenSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	if expectedAudience != "" && c.Audience != expectedAudience {
		return nil, ErrTokenInvalid
	}
	if err := c.Valid(s.now(), s.cfg.Leeway); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Signer) encode(c Claims) (string, error) {
	header := map[string]string{
		"alg": string(s.cfg.Algorithm),
		"typ": "JWT",
	}
	hmacKey, rsaPriv, kid := s.selectSigningKey()
	if kid != "" {
		header["kid"] = kid
	}
	hdrJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimJSON, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimJSON)
	var sig []byte
	switch s.cfg.Algorithm {
	case AlgHS256:
		mac := hmac.New(sha256.New, hmacKey)
		mac.Write([]byte(signingInput))
		sig = mac.Sum(nil)
	case AlgRS256:
		if rsaPriv == nil {
			return "", errors.New("auth: RS256 signer missing private key")
		}
		hashed := sha256.Sum256([]byte(signingInput))
		signed, err := rsa.SignPKCS1v15(rand.Reader, rsaPriv, crypto.SHA256, hashed[:])
		if err != nil {
			return "", fmt.Errorf("auth: rsa sign: %w", err)
		}
		sig = signed
	default:
		return "", fmt.Errorf("auth: unsupported algorithm %q", s.cfg.Algorithm)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// selectSigningKey returns the active signing material plus the
// kid to stamp on the JWS header. When the signer is configured
// via the legacy single-key fields, the kid is empty (header is
// omitted) so existing tokens stay byte-for-byte identical.
// When configured via KeyRing, the kid is the primary key's KID.
func (s *Signer) selectSigningKey() (hmacKey []byte, rsaPriv *rsa.PrivateKey, kid string) {
	if s.cfg.KeyRing != nil {
		k := s.cfg.KeyRing.Primary()
		return k.HMACKey, k.RSAPrivate, k.KID
	}
	return s.cfg.HMACKey, s.cfg.RSAPrivate, ""
}

// selectVerificationCandidates returns the candidate keys that
// could validate a JWS with the supplied (alg, kid) header.
//
// Dispatch by case:
//
//   - alg mismatch → fail closed with ErrTokenSignature regardless
//     of kid; we never verify a token claiming a different signature
//     algorithm than the signer is configured for.
//   - KeyRing not configured (legacy single-key signer) → single
//     candidate from the cfg's HMACKey / RSAPublic field. Behaviour
//     identical to pre-keyring builds.
//   - KeyRing configured, kid present → exactly the key with that
//     kid. Unknown kid is a fail-closed signature error.
//   - KeyRing configured, kid empty → every key in the ring (primary
//     first, then verifiers in stable order). This is the legacy-
//     token-after-rotation case: a token minted before keyring was
//     enabled (no `kid` header) must still validate after the operator
//     rotates the primary, which moves the original signing key into
//     the verifier set. Without this fanout, the legacy token would
//     fail signature check the moment Primary() changes.
//
// Returning a slice keeps the API uniform across these branches —
// the verify path iterates the slice and tries each, short-
// circuiting on the first match.
func (s *Signer) selectVerificationCandidates(alg, kid string) ([]SigningKey, error) {
	if alg != "" && alg != string(s.cfg.Algorithm) {
		return nil, fmt.Errorf("%w: algorithm mismatch", ErrTokenSignature)
	}
	if s.cfg.KeyRing == nil {
		return []SigningKey{{
			Algorithm:  s.cfg.Algorithm,
			HMACKey:    s.cfg.HMACKey,
			RSAPublic:  s.cfg.RSAPublic,
			RSAPrivate: s.cfg.RSAPrivate,
		}}, nil
	}
	if kid == "" {
		return s.cfg.KeyRing.All(), nil
	}
	k, ok := s.cfg.KeyRing.Get(kid)
	if !ok {
		return nil, fmt.Errorf("%w: unknown kid %q", ErrTokenSignature, kid)
	}
	return []SigningKey{k}, nil
}

// verifySignature returns true when at least one of the candidate
// keys produces a signature matching sig over signingInput. The
// per-candidate check is constant-time (hmac.Equal /
// rsa.VerifyPKCS1v15 both compare in O(n) without leaking timing
// for valid-length inputs); iterating multiple candidates does
// leak "which key matched" via wall-clock timing, but only at the
// granularity of one HMAC / one RSA verify each. We accept that
// timing surface in exchange for legacy-token compatibility — an
// attacker who learns "key #2 matched" learns nothing useful
// because all candidate keys are equally privileged.
func (s *Signer) verifySignature(signingInput string, sig []byte, candidates []SigningKey) bool {
	switch s.cfg.Algorithm {
	case AlgHS256:
		matched := false
		for _, k := range candidates {
			if len(k.HMACKey) == 0 {
				continue
			}
			mac := hmac.New(sha256.New, k.HMACKey)
			mac.Write([]byte(signingInput))
			if hmac.Equal(mac.Sum(nil), sig) {
				matched = true
				// Do NOT break early: iterating every
				// candidate keeps wall-clock timing
				// proportional to (ring size) rather
				// than (ring size up to the match).
			}
		}
		return matched
	case AlgRS256:
		hashed := sha256.Sum256([]byte(signingInput))
		matched := false
		for _, k := range candidates {
			if k.RSAPublic == nil {
				continue
			}
			if err := rsa.VerifyPKCS1v15(k.RSAPublic, crypto.SHA256, hashed[:], sig); err == nil {
				matched = true
			}
		}
		return matched
	default:
		return false
	}
}

// ParsePrivateKeyPEM parses a PEM-encoded RSA private key. Accepts
// PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") envelopes.
func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("auth: no PEM block in key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: parse key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("auth: not an RSA private key")
	}
	return rsaKey, nil
}
