package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/secrets"
)

// SignerFromProvider builds a *Signer backed by a KeyRing whose
// primary (and optional verifier) keys are loaded from the
// supplied secrets.Provider. It is the PR-6 successor to
// SignerFromEnv: where SignerFromEnv reads KAPP_JWT_SECRET
// directly from os.Getenv and builds a single-key signer,
// SignerFromProvider resolves PrimaryRef (and VerifyRefs) via
// the Provider, populates a KeyRing, and starts a refresh loop
// (when ctx is non-nil) that detects upstream rotations and
// reloads without a restart.
//
// The function is intentionally orthogonal to SignerFromEnv:
// existing services that haven't migrated to the Provider model
// keep calling SignerFromEnv; services that have migrated call
// SignerFromProvider. Both produce a *Signer whose API surface
// is identical, so downstream Issue / Verify call sites do not
// change.
//
// # Refresh lifecycle
//
// When refreshCtx is non-nil, this function starts a goroutine
// that polls the provider every refreshInterval and reloads the
// keyring when the upstream version changes. The goroutine is
// stopped by cancelling refreshCtx. Pass a nil context to disable
// auto-refresh (useful for unit tests and one-shot CLI utilities).
//
// # Dev-placeholder guard
//
// SignerFromEnv refuses to boot when KAPP_JWT_SECRET equals the
// recognised dev placeholder unless KAPP_ALLOW_DEV_JWT_SECRET=1.
// The same guard applies here: a Provider that returns the dev
// placeholder string (which would happen if the operator stored
// the example value in their secret store) is rejected unless
// the opt-in flag is set. The guard is at this layer because
// the Provider implementations are intentionally
// content-agnostic.
func SignerFromProvider(refreshCtx context.Context, provider secrets.Provider, opts SignerProviderOptions) (*Signer, error) {
	if provider == nil {
		return nil, errors.New("auth: SignerFromProvider requires non-nil provider")
	}
	if opts.PrimaryRef == "" {
		return nil, errors.New("auth: SignerFromProvider requires non-empty PrimaryRef")
	}
	algorithm := opts.Algorithm
	if algorithm == "" {
		algorithm = AlgHS256
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Boot context: a finite-deadline child of the caller's
	// context so a hung secret store at boot doesn't wedge the
	// whole startup forever. A 30s ceiling is generous enough
	// for SDK init + a remote secret fetch but bounded so a
	// misbehaving provider cannot strand the API forever.
	bootCtx, bootCancel := bootContext(refreshCtx)
	defer bootCancel()

	primary, err := loadKey(bootCtx, provider, opts.PrimaryRef, algorithm)
	if err != nil {
		return nil, fmt.Errorf("auth: load primary key %s: %w", opts.PrimaryRef, err)
	}
	if err := checkDevPlaceholder(primary, algorithm); err != nil {
		return nil, err
	}
	ring, err := NewKeyRing(primary)
	if err != nil {
		return nil, fmt.Errorf("auth: build keyring: %w", err)
	}
	// Track each verify ref's boot-time version so the refresher
	// can skip a redundant first-tick re-fetch for refs whose
	// upstream version hasn't moved. Failed loads do NOT seed the
	// map — leaving the entry unset means the refresher will
	// retry the missing ref on its first tick (which is the
	// recoverable-failure behaviour we want for a transient
	// provider blip during boot).
	verifyVersions := make(map[string]string, len(opts.VerifyRefs))
	for _, ref := range opts.VerifyRefs {
		v, err := loadKey(bootCtx, provider, ref, algorithm)
		if err != nil {
			// Verify-only references are best-effort: a
			// stale verifier in the operator's config should
			// not block boot. Log and continue.
			logger.Warn("auth: skipping verifier key",
				slog.String("ref", ref),
				slog.String("error", err.Error()))
			continue
		}
		if err := ring.AddVerifier(v); err != nil {
			logger.Warn("auth: add verifier failed",
				slog.String("ref", ref),
				slog.String("error", err.Error()))
			continue
		}
		verifyVersions[ref] = v.Version
	}

	signer, err := NewSigner(SignerConfig{
		Algorithm:  algorithm,
		KeyRing:    ring,
		Issuer:     opts.Issuer,
		Audience:   opts.Audience,
		AccessTTL:  opts.AccessTTL,
		RefreshTTL: opts.RefreshTTL,
		Leeway:     opts.Leeway,
	})
	if err != nil {
		return nil, err
	}

	if refreshCtx != nil && opts.RefreshInterval > 0 {
		refresher := &KeyRingRefresher{
			Ring:       ring,
			Provider:   provider,
			PrimaryRef: opts.PrimaryRef,
			VerifyRefs: opts.VerifyRefs,
			Interval:   opts.RefreshInterval,
			Algorithm:  algorithm,
			Logger:     logger,
		}
		// Seed the refresher's current-version map for the
		// primary ref AND every successfully-loaded verify ref
		// so the first tick doesn't burn one provider round-trip
		// per ref re-fetching material that hasn't rotated yet.
		// Unset entries (failed verify loads) intentionally fall
		// through to the refresher's normal lookup path.
		refresher.current = make(map[string]string, 1+len(verifyVersions))
		refresher.current[opts.PrimaryRef] = primary.Version
		for ref, version := range verifyVersions {
			refresher.current[ref] = version
		}
		// Wire the refresher's exit into the signer so callers
		// can join it during shutdown (see Signer.RefresherDone).
		// Buffer is zero — close is the signal, no value is sent.
		done := make(chan struct{})
		signer.refresherDone = done
		go func() {
			defer close(done)
			if err := refresher.Run(refreshCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("auth: keyring refresher exited", slog.String("error", err.Error()))
			}
		}()
	}

	logger.Info("auth: signer initialised from provider",
		slog.String("provider", provider.Name()),
		slog.String("primary_ref", opts.PrimaryRef),
		slog.String("primary_kid", primary.KID),
		slog.String("primary_version", primary.Version),
		slog.Int("verify_refs", len(opts.VerifyRefs)),
		slog.String("algorithm", string(algorithm)))

	return signer, nil
}

// SignerProviderOptions is the construction-time configuration
// for SignerFromProvider. PrimaryRef is the only mandatory
// field; all others have sensible defaults.
type SignerProviderOptions struct {
	// PrimaryRef is the Provider key for the issuing material
	// (e.g. "jwt/primary" against Vault, "kapp/jwt/primary"
	// against AWS Secrets Manager). Required.
	PrimaryRef string
	// VerifyRefs is the optional set of additional Provider
	// keys to load as verify-only entries. Each one is fetched
	// at boot and registered as a verifier; rotation later
	// keeps them current.
	VerifyRefs []string
	// Algorithm is the JWS algorithm. Default AlgHS256.
	Algorithm Algorithm
	// Issuer / Audience are the JWT iss / aud claims. Default
	// "kapp" / "kapp" matching SignerFromEnv.
	Issuer   string
	Audience string
	// AccessTTL / RefreshTTL govern token lifetimes. Defaults
	// match SignerFromEnv (15m / 24h).
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// Leeway absorbs clock skew between issuer and validator.
	// Default 30s.
	Leeway time.Duration
	// RefreshInterval is the cadence at which the refresher
	// re-polls the provider. Default 60s. Set to <=0 to
	// disable the refresher (one-shot load, no rotation).
	RefreshInterval time.Duration
	// Logger receives boot and rotation events.
	Logger *slog.Logger
}

// bootContext returns a finite-deadline context for the boot
// path. When refreshCtx is nil (one-shot CLI callers) the
// timeout is grafted onto context.Background; otherwise it's a
// child of the caller's context so a cancellation propagates.
func bootContext(refreshCtx context.Context) (context.Context, context.CancelFunc) {
	if refreshCtx == nil {
		return context.WithTimeout(context.Background(), 30*time.Second) //nolint:contextcheck // intentional: refreshCtx is nil
	}
	return context.WithTimeout(refreshCtx, 30*time.Second)
}

func loadKey(ctx context.Context, provider secrets.Provider, ref string, algorithm Algorithm) (SigningKey, error) {
	val, err := provider.GetSecret(ctx, ref)
	if err != nil {
		return SigningKey{}, err
	}
	kid := deriveKID(ref, val.Version)
	key := SigningKey{
		KID:       kid,
		Algorithm: algorithm,
		Version:   val.Version,
	}
	switch algorithm {
	case AlgHS256:
		key.HMACKey = val.Bytes
	case AlgRS256:
		priv, err := ParsePrivateKeyPEM(val.Bytes)
		if err != nil {
			return SigningKey{}, fmt.Errorf("parse RS256 PEM: %w", err)
		}
		key.RSAPrivate = priv
		key.RSAPublic = &priv.PublicKey
	default:
		return SigningKey{}, fmt.Errorf("unsupported algorithm %q", algorithm)
	}
	return key, nil
}

// checkDevPlaceholder applies the same guard as SignerFromEnv:
// refuse to mint tokens against the recognised dev secret unless
// KAPP_ALLOW_DEV_JWT_SECRET=1 is set. The check is here (rather
// than inside the Provider) because the Provider implementations
// are intentionally content-agnostic.
func checkDevPlaceholder(k SigningKey, algorithm Algorithm) error {
	if algorithm != AlgHS256 {
		return nil
	}
	if string(k.HMACKey) != DevPlaceholderJWTSecret {
		return nil
	}
	if os.Getenv("KAPP_ALLOW_DEV_JWT_SECRET") == "1" {
		return nil
	}
	return errors.New(
		"auth: resolved JWT signing key is the literal dev-only placeholder from .env.example; " +
			"rotate the upstream secret to a freshly generated value (e.g. `openssl rand -base64 48`) " +
			"or set KAPP_ALLOW_DEV_JWT_SECRET=1 to acknowledge a dev deployment",
	)
}

// GenerateHMACKey returns a fresh base64-encoded 32-byte secret
// suitable for HS256 signing. The caller is responsible for
// writing this to the upstream secret store; the helper is here
// so operator runbooks have a stable invocation surface
// (`go run cmd/keygen` style) regardless of the Provider in
// use.
func GenerateHMACKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate hmac key: %w", err)
	}
	// Use URL-safe base64 so the secret can be safely passed
	// through environment variables or HTTP-style stores
	// without escaping headaches. The trailing "=" padding is
	// stripped because some secret stores reject it.
	encoded := base64.URLEncoding.EncodeToString(buf)
	return strings.TrimRight(encoded, "="), nil
}
