package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/secrets"
)

// SigningKey is a single signing-key entry within a KeyRing.
// Each key carries a stable identifier (KID) plus the actual
// material; the validator picks the entry whose KID matches the
// JWT header. Multiple keys are kept alive simultaneously during
// a rotation so freshly-minted tokens (signed by the new key)
// AND in-flight tokens (signed by the old key, still
// pre-expiry) both validate.
type SigningKey struct {
	// KID is the key-identifier stamped into the JWT header so
	// the validator can pick the right verification material.
	// Must be globally unique across all keys in a KeyRing.
	KID string
	// Algorithm is the signing algorithm for this key. All
	// keys in a single KeyRing must share the algorithm class
	// (HS256 / RS256) -- the per-deployment choice is set at
	// build time and a mixed-class ring is a configuration
	// error.
	Algorithm Algorithm
	// HMACKey is consulted when Algorithm == AlgHS256.
	HMACKey []byte
	// RSAPrivate / RSAPublic are consulted when Algorithm ==
	// AlgRS256. RSAPrivate is required on the primary
	// (issuing) key; verify-only keys may carry RSAPublic
	// only, which lets a deployment rotate to asymmetric
	// signing without the new public key needing to land
	// simultaneously on every verifier.
	RSAPrivate *rsa.PrivateKey
	RSAPublic  *rsa.PublicKey
	// Version is the rotation tag from the upstream secret
	// provider. Used by the refresh loop to detect upstream
	// rotations without re-fetching the material on every
	// tick.
	Version string
}

// KeyRing is the multi-key signing+verification surface. The
// primary key is used for issuance; the full set is used for
// verification (the validator picks by KID). A deployment that
// is NOT mid-rotation has exactly one entry; a deployment
// mid-rotation has the freshly-staged key as primary plus the
// previous key (verify-only) still listed.
//
// Mutations are atomic via the SwapPrimary / AddVerifier APIs.
// Lookups are wait-free (a single atomic.Pointer load) so the
// signing path stays allocation-free.
type KeyRing struct {
	state atomic.Pointer[keyringState]
}

type keyringState struct {
	primaryKID string
	byKID      map[string]SigningKey
}

// NewKeyRing returns a KeyRing seeded with the supplied primary
// key. The primary is the one Issue / IssueWithTTL / IssueRefresh
// will use; until AddVerifier is called, it is also the only
// key Verify will accept.
//
// Returns an error when the supplied key is missing required
// material (e.g. AlgHS256 without HMACKey, or an HMAC key under
// 32 bytes).
func NewKeyRing(primary SigningKey) (*KeyRing, error) {
	if err := validateKey(primary); err != nil {
		return nil, fmt.Errorf("auth: keyring primary: %w", err)
	}
	r := &KeyRing{}
	r.state.Store(&keyringState{
		primaryKID: primary.KID,
		byKID:      map[string]SigningKey{primary.KID: primary},
	})
	return r, nil
}

// AddVerifier registers an additional key for verification only.
// Use this during a rotation: stage the new key with
// SwapPrimary, then keep the old key listed via AddVerifier so
// in-flight tokens minted under it still validate until they
// expire. After the longest-lived refresh token has expired the
// operator can remove the old key with Remove(kid).
func (r *KeyRing) AddVerifier(key SigningKey) error {
	if err := validateKey(key); err != nil {
		return fmt.Errorf("auth: keyring verifier: %w", err)
	}
	for {
		cur := r.state.Load()
		next := cloneState(cur)
		next.byKID[key.KID] = key
		if r.state.CompareAndSwap(cur, next) {
			return nil
		}
	}
}

// SwapPrimary atomically promotes the key with the supplied
// KID to primary (must be already present via AddVerifier or
// passed verbatim if not yet in the ring). If the key is not
// present and full material is supplied via the optional
// SigningKey argument, it is added at the same time. Returns
// an error if the supplied KID is not findable.
//
// The previous primary remains in the ring as a verifier so
// in-flight tokens still validate. Operators should clear it
// via Remove() after the longest refresh-TTL has elapsed.
func (r *KeyRing) SwapPrimary(kid string, optKey ...SigningKey) error {
	if kid == "" {
		return errors.New("auth: keyring SwapPrimary requires non-empty kid")
	}
	if len(optKey) > 1 {
		return errors.New("auth: keyring SwapPrimary accepts at most one key arg")
	}
	for {
		cur := r.state.Load()
		next := cloneState(cur)
		if _, ok := next.byKID[kid]; !ok {
			if len(optKey) == 0 {
				return fmt.Errorf("auth: keyring SwapPrimary unknown kid %q", kid)
			}
			key := optKey[0]
			if key.KID != kid {
				return fmt.Errorf("auth: keyring SwapPrimary kid mismatch %q vs %q", kid, key.KID)
			}
			if err := validateKey(key); err != nil {
				return fmt.Errorf("auth: keyring SwapPrimary: %w", err)
			}
			next.byKID[kid] = key
		}
		next.primaryKID = kid
		if r.state.CompareAndSwap(cur, next) {
			return nil
		}
	}
}

// Remove drops the key with the supplied KID from the ring.
// Refuses to remove the current primary (the operator must
// SwapPrimary first). Use this AFTER the refresh-TTL has
// elapsed for the longest-lived token minted under the key, so
// in-flight tokens have all expired naturally.
func (r *KeyRing) Remove(kid string) error {
	for {
		cur := r.state.Load()
		if cur.primaryKID == kid {
			return fmt.Errorf("auth: keyring Remove refuses primary kid %q; SwapPrimary first", kid)
		}
		if _, ok := cur.byKID[kid]; !ok {
			return fmt.Errorf("auth: keyring Remove unknown kid %q", kid)
		}
		next := cloneState(cur)
		delete(next.byKID, kid)
		if r.state.CompareAndSwap(cur, next) {
			return nil
		}
	}
}

// Primary returns the current issuing key (a snapshot copy so
// callers can't mutate the ring through the returned value).
func (r *KeyRing) Primary() SigningKey {
	s := r.state.Load()
	return s.byKID[s.primaryKID]
}

// Get returns the key with the supplied KID and a boolean
// indicating presence. Used by the verifier to pick a key by
// the JWT header's kid claim.
func (r *KeyRing) Get(kid string) (SigningKey, bool) {
	s := r.state.Load()
	k, ok := s.byKID[kid]
	return k, ok
}

// All returns every key currently registered in the ring (primary
// + verifiers) in deterministic order. Used by the no-kid legacy-
// token verification path: tokens minted before keyring rotation
// was enabled carry no `kid` header, so the verifier has to try
// each registered key in turn rather than dispatching by kid.
// Order: primary first, then verifiers in sorted-KID order, so
// the common case (most recently rotated token) hits the most
// likely key first.
func (r *KeyRing) All() []SigningKey {
	s := r.state.Load()
	out := make([]SigningKey, 0, len(s.byKID))
	if primary, ok := s.byKID[s.primaryKID]; ok {
		out = append(out, primary)
	}
	// Collect non-primary KIDs for stable iteration.
	others := make([]string, 0, len(s.byKID))
	for kid := range s.byKID {
		if kid == s.primaryKID {
			continue
		}
		others = append(others, kid)
	}
	sortStrings(others)
	for _, kid := range others {
		out = append(out, s.byKID[kid])
	}
	return out
}

// KIDs returns the set of currently-registered key identifiers
// in deterministic order (sorted). Boot logging uses this so the
// operator can see at a glance which keys are live.
func (r *KeyRing) KIDs() []string {
	s := r.state.Load()
	ids := make([]string, 0, len(s.byKID))
	for k := range s.byKID {
		ids = append(ids, k)
	}
	// Sort for stable boot logs. The atomic snapshot is a
	// copy-on-write map, so iteration order is non-deterministic.
	sortStrings(ids)
	return ids
}

func cloneState(s *keyringState) *keyringState {
	next := &keyringState{
		primaryKID: s.primaryKID,
		byKID:      make(map[string]SigningKey, len(s.byKID)+1),
	}
	for k, v := range s.byKID {
		next.byKID[k] = v
	}
	return next
}

func validateKey(k SigningKey) error {
	if k.KID == "" {
		return errors.New("KID required")
	}
	switch k.Algorithm {
	case AlgHS256:
		if len(k.HMACKey) < 32 {
			return errors.New("HS256 requires >=32-byte HMAC key")
		}
	case AlgRS256:
		if k.RSAPrivate == nil && k.RSAPublic == nil {
			return errors.New("RS256 requires a key")
		}
	default:
		return fmt.Errorf("unsupported algorithm %q", k.Algorithm)
	}
	return nil
}

// sortStrings is a tiny in-place selection sort. The dependency
// graph for this package has zero stdlib sort users and pulling
// in "sort" just for KIDs() would inflate the auth package's
// import surface for a single use site. KeyRings carry single
// digits of entries in practice (current + one rotating-out
// previous), so the O(n^2) cost is unmeasurable.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// KeyRingRefresher is the loop that periodically re-resolves
// the configured secret references via a secrets.Provider and
// rotates the ring when the upstream version changes. Operators
// drive a rotation by changing the upstream value (in AWS
// Secrets Manager, in Vault, etc.); the refresher detects the
// new Version on the next tick and AddVerifier+SwapPrimary the
// new material into the ring without a restart.
//
// The refresher does NOT delete old keys automatically -- a
// freshly-minted refresh token may live for hours and must
// continue to validate. The Reaper field is wired below the
// refresh loop and clears keys older than RefreshTTL.
type KeyRingRefresher struct {
	Ring     *KeyRing
	Provider secrets.Provider
	// PrimaryRef is the Provider key for the issuing material
	// (e.g. "jwt/primary" against Vault, "kapp/jwt/primary"
	// against AWS).
	PrimaryRef string
	// VerifyRefs is the optional list of additional Provider
	// keys to keep loaded as verify-only entries. Use when an
	// operator wants to keep N historical keys around for
	// JWT-chain validation -- a typical deployment lists the
	// "previous" key here.
	VerifyRefs []string
	// Interval is the poll cadence. Default 1 minute.
	Interval time.Duration
	// Algorithm is the signing algorithm for newly-discovered
	// keys (must match the existing ring's algorithm class).
	Algorithm Algorithm
	// Logger receives boot + rotation events. Use slog.Default()
	// when not wired explicitly.
	Logger *slog.Logger

	mu      sync.Mutex
	current map[string]string // ref -> version last seen
}

// Run blocks until ctx is cancelled, polling the provider every
// Interval. Errors are logged at WARN level but do not abort the
// loop -- a transient provider outage should not bring the JWT
// signer down with it (the ring continues to use the last known
// good keys). Returns ctx.Err() on cancellation.
func (kr *KeyRingRefresher) Run(ctx context.Context) error {
	if kr.Ring == nil || kr.Provider == nil {
		return errors.New("auth: refresher requires Ring and Provider")
	}
	if kr.Interval <= 0 {
		kr.Interval = time.Minute
	}
	if kr.Logger == nil {
		kr.Logger = slog.Default()
	}
	if kr.current == nil {
		kr.current = make(map[string]string)
	}
	// Eager first refresh so the boot log records the
	// resolved keys before the first request hits the signer.
	if err := kr.refreshOnce(ctx); err != nil {
		kr.Logger.Warn("auth: initial key refresh failed", slog.String("error", err.Error()))
	}
	tick := time.NewTicker(kr.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := kr.refreshOnce(ctx); err != nil {
				kr.Logger.Warn("auth: key refresh failed", slog.String("error", err.Error()))
			}
		}
	}
}

func (kr *KeyRingRefresher) refreshOnce(ctx context.Context) error {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if kr.Logger == nil {
		kr.Logger = slog.Default()
	}
	if kr.current == nil {
		kr.current = make(map[string]string)
	}

	if err := kr.checkOne(ctx, kr.PrimaryRef, true); err != nil {
		return fmt.Errorf("primary %s: %w", kr.PrimaryRef, err)
	}
	for _, ref := range kr.VerifyRefs {
		if err := kr.checkOne(ctx, ref, false); err != nil {
			kr.Logger.Warn("auth: verify-only refresh failed",
				slog.String("ref", ref),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

func (kr *KeyRingRefresher) checkOne(ctx context.Context, ref string, isPrimary bool) error {
	val, err := kr.Provider.GetSecret(ctx, ref)
	if err != nil {
		return err
	}
	prev := kr.current[ref]
	if val.Version != "" && val.Version == prev {
		// Same upstream version we already loaded -- no
		// action required. Empty version (env provider)
		// always re-applies; that is correct because env
		// vars don't carry a version and would otherwise
		// never refresh.
		return nil
	}
	kid := deriveKID(ref, val.Version)
	key := SigningKey{
		KID:       kid,
		Algorithm: kr.Algorithm,
		Version:   val.Version,
	}
	switch kr.Algorithm {
	case AlgHS256:
		key.HMACKey = val.Bytes
	case AlgRS256:
		priv, err := ParsePrivateKeyPEM(val.Bytes)
		if err != nil {
			return fmt.Errorf("parse RS256 PEM: %w", err)
		}
		key.RSAPrivate = priv
		key.RSAPublic = &priv.PublicKey
	default:
		return fmt.Errorf("unsupported algorithm %q", kr.Algorithm)
	}
	if isPrimary {
		if err := kr.Ring.SwapPrimary(kid, key); err != nil {
			return fmt.Errorf("swap primary: %w", err)
		}
		kr.Logger.Info("auth: primary key rotated",
			slog.String("ref", ref),
			slog.String("kid", kid),
			slog.String("version", val.Version))
	} else {
		if err := kr.Ring.AddVerifier(key); err != nil {
			return fmt.Errorf("add verifier: %w", err)
		}
		kr.Logger.Info("auth: verifier key loaded",
			slog.String("ref", ref),
			slog.String("kid", kid),
			slog.String("version", val.Version))
	}
	kr.current[ref] = val.Version
	return nil
}

// deriveKID builds a stable kid from the secret reference and
// the upstream version. Format: <ref-with-slashes-flattened>.<version>.
// Distinct upstream versions produce distinct kids, which is
// the contract callers rely on to detect rotations from the
// JWT header alone.
//
// Empty version (env provider, file provider on first read)
// falls back to the ref alone so the kid stays stable across
// refreshes of an env-sourced secret.
func deriveKID(ref, version string) string {
	flat := strings.ReplaceAll(ref, "/", "_")
	flat = strings.ReplaceAll(flat, ".", "_")
	if version == "" {
		return flat
	}
	return flat + "." + version
}
