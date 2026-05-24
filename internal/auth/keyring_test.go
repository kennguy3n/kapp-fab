package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kennguy3n/kapp-fab/internal/secrets"
)

func TestKeyRing_PrimaryAndVerifierLookups(t *testing.T) {
	primary := SigningKey{KID: "k1", Algorithm: AlgHS256, HMACKey: make([]byte, 32)}
	for i := range primary.HMACKey {
		primary.HMACKey[i] = 0x11
	}
	ring, err := NewKeyRing(primary)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	if ring.Primary().KID != "k1" {
		t.Fatalf("Primary.KID = %s want k1", ring.Primary().KID)
	}
	got, ok := ring.Get("k1")
	if !ok || got.KID != "k1" {
		t.Fatalf("Get(k1): ok=%v key=%v", ok, got)
	}
	if _, ok := ring.Get("nope"); ok {
		t.Fatalf("Get(nope) should be absent")
	}
}

func TestKeyRing_SwapPrimaryAndAddVerifier(t *testing.T) {
	k1 := SigningKey{KID: "k1", Algorithm: AlgHS256, HMACKey: make([]byte, 32)}
	k2 := SigningKey{KID: "k2", Algorithm: AlgHS256, HMACKey: make([]byte, 32)}
	for i := range k1.HMACKey {
		k1.HMACKey[i] = 0x11
	}
	for i := range k2.HMACKey {
		k2.HMACKey[i] = 0x22
	}
	ring, err := NewKeyRing(k1)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	if err := ring.AddVerifier(k2); err != nil {
		t.Fatalf("AddVerifier: %v", err)
	}
	if ring.Primary().KID != "k1" {
		t.Fatalf("Primary still k1 after AddVerifier")
	}
	if err := ring.SwapPrimary("k2"); err != nil {
		t.Fatalf("SwapPrimary: %v", err)
	}
	if ring.Primary().KID != "k2" {
		t.Fatalf("Primary should be k2 after swap")
	}
	// Old primary still present as verifier
	if _, ok := ring.Get("k1"); !ok {
		t.Fatalf("old primary k1 should still be present as verifier")
	}
}

func TestKeyRing_RemoveRefusesPrimary(t *testing.T) {
	k1 := SigningKey{KID: "k1", Algorithm: AlgHS256, HMACKey: make([]byte, 32)}
	for i := range k1.HMACKey {
		k1.HMACKey[i] = 0x11
	}
	ring, _ := NewKeyRing(k1)
	if err := ring.Remove("k1"); err == nil {
		t.Fatalf("Remove of primary should error")
	}
}

func TestKeyRing_KIDsSorted(t *testing.T) {
	k := SigningKey{Algorithm: AlgHS256, HMACKey: make([]byte, 32)}
	for i := range k.HMACKey {
		k.HMACKey[i] = 0x11
	}
	ring, _ := NewKeyRing(SigningKey{KID: "z1", Algorithm: AlgHS256, HMACKey: k.HMACKey})
	if err := ring.AddVerifier(SigningKey{KID: "a2", Algorithm: AlgHS256, HMACKey: k.HMACKey}); err != nil {
		t.Fatalf("AddVerifier: %v", err)
	}
	if err := ring.AddVerifier(SigningKey{KID: "m3", Algorithm: AlgHS256, HMACKey: k.HMACKey}); err != nil {
		t.Fatalf("AddVerifier: %v", err)
	}
	got := ring.KIDs()
	want := []string{"a2", "m3", "z1"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("KIDs() = %v want %v", got, want)
	}
}

// TestKeyRing_SwapPrimaryReplacesMaterialForExistingKID pins the
// contract that SwapPrimary("kid", newKey) overwrites the ring's
// stored material for "kid" when "kid" is already present, even
// though the previous version-check shape (`if !ok { insert }`)
// would have dropped newKey on the floor in that case. Required
// for versionless-provider refresh: env / dev backends produce
// the same KID on every tick (no version metadata) but the
// underlying bytes can change when the operator rotates the
// secret out-of-band. Without same-KID-overwrite, a versionless
// rotation silently no-ops — KeyRingRefresher.checkOne calls
// SwapPrimary with the new SecretValue but the ring's signing
// material remains stale.
func TestKeyRing_SwapPrimaryReplacesMaterialForExistingKID(t *testing.T) {
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = 0x11
	}
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = 0x22
	}
	ring, err := NewKeyRing(SigningKey{KID: "kid-1", Algorithm: AlgHS256, HMACKey: oldKey})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	// SwapPrimary("kid-1", newMaterial): KID already present,
	// new material supplied — must replace.
	if err := ring.SwapPrimary("kid-1", SigningKey{KID: "kid-1", Algorithm: AlgHS256, HMACKey: newKey}); err != nil {
		t.Fatalf("SwapPrimary: %v", err)
	}
	got := ring.Primary()
	if !bytes.Equal(got.HMACKey, newKey) {
		t.Fatalf("SwapPrimary did not replace material: got=%x want=%x", got.HMACKey, newKey)
	}
}

// TestKeyRing_SwapPrimaryRejectsMaterialKIDMismatch ensures
// passing a SigningKey whose KID disagrees with the swap target
// is still an error (no change to behaviour from the previous
// shape, but worth pinning to prevent regressions in either
// direction).
func TestKeyRing_SwapPrimaryRejectsMaterialKIDMismatch(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0x11
	}
	ring, _ := NewKeyRing(SigningKey{KID: "kid-a", Algorithm: AlgHS256, HMACKey: k})
	err := ring.SwapPrimary("kid-a", SigningKey{KID: "kid-b", Algorithm: AlgHS256, HMACKey: k})
	if err == nil {
		t.Fatalf("expected kid mismatch error, got nil")
	}
}

func TestKeyRing_RejectsShortHMACKey(t *testing.T) {
	_, err := NewKeyRing(SigningKey{KID: "k", Algorithm: AlgHS256, HMACKey: []byte("short")})
	if err == nil {
		t.Fatalf("expected error for short HMAC key")
	}
}

func TestSigner_KeyRing_RoundTrip_HS256(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xAA
	}
	ring, err := NewKeyRing(SigningKey{KID: "kid-1", Algorithm: AlgHS256, HMACKey: k})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	signer, err := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   ring,
		Issuer:    "kapp",
		Audience:  "kapp",
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, err := signer.Issue(Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Issuer != "kapp" {
		t.Fatalf("issuer not threaded: %s", claims.Issuer)
	}
}

func TestSigner_KeyRing_RotationPreservesInFlightTokens(t *testing.T) {
	k1 := make([]byte, 32)
	for i := range k1 {
		k1[i] = 0x11
	}
	k2 := make([]byte, 32)
	for i := range k2 {
		k2[i] = 0x22
	}
	ring, _ := NewKeyRing(SigningKey{KID: "kid-1", Algorithm: AlgHS256, HMACKey: k1})
	signer, _ := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   ring,
		Issuer:    "k",
		Audience:  "k",
	})
	oldToken, err := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue old: %v", err)
	}
	// Rotate to k2 while keeping k1 as verifier
	if err := ring.SwapPrimary("kid-2", SigningKey{KID: "kid-2", Algorithm: AlgHS256, HMACKey: k2}); err != nil {
		t.Fatalf("SwapPrimary: %v", err)
	}
	// Old token must still verify (signed under k1, k1 is still in the ring)
	if _, err := signer.Verify(oldToken); err != nil {
		t.Fatalf("old token should still verify under rotated keyring: %v", err)
	}
	// New token uses k2
	newToken, err := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue new: %v", err)
	}
	if _, err := signer.Verify(newToken); err != nil {
		t.Fatalf("new token should verify: %v", err)
	}
	// After old key is reaped, old token should fail with sig error
	if err := ring.Remove("kid-1"); err != nil {
		t.Fatalf("Remove kid-1: %v", err)
	}
	if _, err := signer.Verify(oldToken); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("expected ErrTokenSignature after old key removed, got %v", err)
	}
}

func TestSigner_KeyRing_UnknownKID_Rejected(t *testing.T) {
	k1 := make([]byte, 32)
	for i := range k1 {
		k1[i] = 0x11
	}
	ringA, _ := NewKeyRing(SigningKey{KID: "kid-A", Algorithm: AlgHS256, HMACKey: k1})
	signerA, _ := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   ringA,
		Issuer:    "k",
		Audience:  "k",
	})
	token, _ := signerA.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})

	// Build a signer with a different ring that doesn't know kid-A
	k2 := make([]byte, 32)
	for i := range k2 {
		k2[i] = 0x22
	}
	ringB, _ := NewKeyRing(SigningKey{KID: "kid-B", Algorithm: AlgHS256, HMACKey: k2})
	signerB, _ := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   ringB,
		Issuer:    "k",
		Audience:  "k",
	})
	_, err := signerB.Verify(token)
	if !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("expected ErrTokenSignature for unknown kid, got %v", err)
	}
}

func TestSigner_SingleKeyAndKeyRing_RejectsBoth(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xAA
	}
	ring, _ := NewKeyRing(SigningKey{KID: "k", Algorithm: AlgHS256, HMACKey: k})
	_, err := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		HMACKey:   k,
		KeyRing:   ring,
	})
	if err == nil || !strings.Contains(err.Error(), "either KeyRing or single-key fields") {
		t.Fatalf("expected mutex error, got %v", err)
	}
}

func TestSigner_BackwardsCompat_LegacyTokenWithoutKID(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xCC
	}
	// Mint a token with the legacy single-key path (no kid header)
	legacySigner, err := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		HMACKey:   k,
		Issuer:    "k",
		Audience:  "k",
	})
	if err != nil {
		t.Fatalf("legacy NewSigner: %v", err)
	}
	legacyToken, err := legacySigner.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("legacy Issue: %v", err)
	}
	// Now upgrade to keyring; legacy token (no kid header) should
	// still verify because the keyring verifier falls back to the
	// primary key when the header has no kid.
	ring, _ := NewKeyRing(SigningKey{KID: "kid-1", Algorithm: AlgHS256, HMACKey: k})
	upgraded, _ := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   ring,
		Issuer:    "k",
		Audience:  "k",
	})
	if _, err := upgraded.Verify(legacyToken); err != nil {
		t.Fatalf("legacy token should still verify after keyring upgrade: %v", err)
	}
}

// TestSigner_RS256_SingleKey_DerivesPublicFromPrivate pins the
// contract that a legacy single-key RS256 signer constructed with
// only RSAPrivate (RSAPublic nil) verifies tokens correctly — the
// public key is derived from the private key at NewSigner time.
// Without the derivation, verifySignature would silently skip the
// nil-RSAPublic candidate and return ErrTokenSignature on every
// verification, a regression from the pre-keyring behaviour that
// surfaced this as "auth: RS256 verifier missing public key".
func TestSigner_RS256_SingleKey_DerivesPublicFromPrivate(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := NewSigner(SignerConfig{
		Algorithm:  AlgRS256,
		RSAPrivate: priv,
		// RSAPublic deliberately nil to exercise the
		// derivation path.
		Issuer:   "k",
		Audience: "k",
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, err := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("Verify after derive-public: %v", err)
	}
}

func TestSigner_KeyRing_RS256_RoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ring, err := NewKeyRing(SigningKey{
		KID:        "rsa-1",
		Algorithm:  AlgRS256,
		RSAPrivate: priv,
		RSAPublic:  &priv.PublicKey,
	})
	if err != nil {
		t.Fatalf("NewKeyRing RS256: %v", err)
	}
	signer, err := NewSigner(SignerConfig{
		Algorithm: AlgRS256,
		KeyRing:   ring,
		Issuer:    "k",
		Audience:  "k",
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, err := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

type fakeProvider struct {
	store map[string]secrets.SecretValue
}

func (*fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) GetSecret(_ context.Context, key string) (secrets.SecretValue, error) {
	v, ok := f.store[key]
	if !ok {
		return secrets.SecretValue{}, secrets.ErrSecretNotFound
	}
	return v, nil
}

func TestSignerFromProvider_RoundTrip(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xCC
	}
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: k, Version: "v1"},
		},
	}
	signer, err := SignerFromProvider(context.TODO(), p, SignerProviderOptions{
		PrimaryRef: "jwt/primary",
		Algorithm:  AlgHS256,
		Issuer:     "k",
		Audience:   "k",
	})
	if err != nil {
		t.Fatalf("SignerFromProvider: %v", err)
	}
	token, err := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestSignerFromProvider_RefresherDoneClosesOnContextCancel
// pins the shutdown-join contract: a signer constructed with
// a refresher (non-nil refreshCtx + positive RefreshInterval)
// exposes a non-nil RefresherDone() channel, and that channel
// is closed once the refresher goroutine has fully exited
// after the context is cancelled. Callers rely on this for
// graceful shutdown — without the join, a provider Close()
// can race in-flight refresh RPCs.
func TestSignerFromProvider_RefresherDoneClosesOnContextCancel(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xAB
	}
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: k, Version: "v1"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	signer, err := SignerFromProvider(ctx, p, SignerProviderOptions{
		PrimaryRef:      "jwt/primary",
		Algorithm:       AlgHS256,
		Issuer:          "k",
		Audience:        "k",
		RefreshInterval: time.Hour, // long enough that we observe ctx-cancel exit, not a tick.
	})
	if err != nil {
		cancel()
		t.Fatalf("SignerFromProvider: %v", err)
	}
	done := signer.RefresherDone()
	if done == nil {
		cancel()
		t.Fatalf("RefresherDone must be non-nil when refresher is started")
	}
	select {
	case <-done:
		cancel()
		t.Fatalf("RefresherDone closed before context cancellation")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RefresherDone did not close within 2s of context cancellation")
	}
}

// TestSignerFromProvider_RefresherDoneNilWithoutRefresher pins
// the contract that a signer constructed without an auto-
// refresh loop (nil ctx OR zero RefreshInterval, as in one-
// shot CLI invocations) returns nil from RefresherDone. The
// caller's shutdown-join logic relies on the nil case meaning
// "no goroutine to wait on".
func TestSignerFromProvider_RefresherDoneNilWithoutRefresher(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xCD
	}
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: k, Version: "v1"},
		},
	}
	// Nil ctx — refresher loop intentionally not started.
	signer, err := SignerFromProvider(nil, p, SignerProviderOptions{
		PrimaryRef: "jwt/primary",
		Algorithm:  AlgHS256,
		Issuer:     "k",
		Audience:   "k",
	})
	if err != nil {
		t.Fatalf("SignerFromProvider: %v", err)
	}
	if signer.RefresherDone() != nil {
		t.Fatalf("RefresherDone must be nil when no refresher is started")
	}
}

func TestSignerFromProvider_RejectsDevPlaceholder(t *testing.T) {
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: []byte(DevPlaceholderJWTSecret)},
		},
	}
	_, err := SignerFromProvider(context.TODO(), p, SignerProviderOptions{PrimaryRef: "jwt/primary"})
	if err == nil {
		t.Fatalf("expected dev-placeholder refusal")
	}
	if !strings.Contains(err.Error(), "dev-only placeholder") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSignerFromProvider_AllowsDevPlaceholderWithOptIn(t *testing.T) {
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "1")
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: []byte(DevPlaceholderJWTSecret)},
		},
	}
	if _, err := SignerFromProvider(context.TODO(), p, SignerProviderOptions{PrimaryRef: "jwt/primary"}); err != nil {
		t.Fatalf("opt-in should allow dev placeholder: %v", err)
	}
}

func TestSignerFromProvider_VerifyRefs(t *testing.T) {
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	for i := range k1 {
		k1[i] = 0x11
	}
	for i := range k2 {
		k2[i] = 0x22
	}
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary":  {Bytes: k1, Version: "v1"},
			"jwt/previous": {Bytes: k2, Version: "v0"},
		},
	}
	signer, err := SignerFromProvider(context.TODO(), p, SignerProviderOptions{
		PrimaryRef: "jwt/primary",
		VerifyRefs: []string{"jwt/previous"},
		Algorithm:  AlgHS256,
		Issuer:     "k",
		Audience:   "k",
	})
	if err != nil {
		t.Fatalf("SignerFromProvider: %v", err)
	}
	// Primary token works.
	token, _ := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("primary token should verify: %v", err)
	}
	// Forge a token under "jwt/previous" (simulate a recently-rotated-out key)
	prevKID := deriveKID("jwt/previous", "v0")
	prevRing, _ := NewKeyRing(SigningKey{KID: prevKID, Algorithm: AlgHS256, HMACKey: k2})
	prevSigner, _ := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   prevRing,
		Issuer:    "k",
		Audience:  "k",
	})
	oldToken, _ := prevSigner.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if _, err := signer.Verify(oldToken); err != nil {
		t.Fatalf("token signed by previous key should verify under refreshed signer: %v", err)
	}
}

func TestKeyRingRefresher_DetectsRotation(t *testing.T) {
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	for i := range k1 {
		k1[i] = 0x11
	}
	for i := range k2 {
		k2[i] = 0x22
	}
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: k1, Version: "v1"},
		},
	}
	signer, err := SignerFromProvider(context.TODO(), p, SignerProviderOptions{
		PrimaryRef: "jwt/primary",
		Algorithm:  AlgHS256,
		Issuer:     "k",
		Audience:   "k",
	})
	if err != nil {
		t.Fatalf("SignerFromProvider: %v", err)
	}
	// Simulate the operator rotating the upstream secret.
	p.store["jwt/primary"] = secrets.SecretValue{Bytes: k2, Version: "v2"}

	refresher := &KeyRingRefresher{
		Ring:       signer.cfg.KeyRing,
		Provider:   p,
		PrimaryRef: "jwt/primary",
		Algorithm:  AlgHS256,
		current:    map[string]string{"jwt/primary": "v1"},
	}
	if err := refresher.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	// Primary should now point to the v2 kid.
	prim := signer.cfg.KeyRing.Primary()
	if !strings.HasSuffix(prim.KID, ".v2") {
		t.Fatalf("primary KID should end in .v2 after rotation; got %s", prim.KID)
	}
	// And the old v1 key should still be present as a verifier.
	if _, ok := signer.cfg.KeyRing.Get(deriveKID("jwt/primary", "v1")); !ok {
		t.Fatalf("previous primary should remain in ring as verifier")
	}
}

func TestKeyRingRefresher_RefreshContextCancellation(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xAB
	}
	p := &fakeProvider{
		store: map[string]secrets.SecretValue{
			"jwt/primary": {Bytes: k, Version: "v1"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	signer, err := SignerFromProvider(ctx, p, SignerProviderOptions{
		PrimaryRef:      "jwt/primary",
		Algorithm:       AlgHS256,
		Issuer:          "k",
		Audience:        "k",
		RefreshInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("SignerFromProvider: %v", err)
	}
	cancel()
	// Allow the refresher goroutine to observe the cancellation.
	time.Sleep(30 * time.Millisecond)
	// Should still be able to issue + verify after refresher exit.
	token, _ := signer.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("Verify after cancel: %v", err)
	}
}

func TestGenerateHMACKey_ProducesUniqueKeys(t *testing.T) {
	k1, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}
	k2, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}
	if k1 == k2 {
		t.Fatalf("two GenerateHMACKey calls collided")
	}
	if len(k1) < 32 {
		t.Fatalf("generated key %q too short", k1)
	}
}

// TestSigner_BackwardsCompat_LegacyTokenAfterRotation verifies the
// rotation-bridge case for ANALYSIS_pr-review-job-...-d967d70cf92e..._0002:
// a token minted before the keyring was enabled (no kid header)
// must continue to verify AFTER the operator rotates the primary
// key. Before the fix, selectVerificationKey unconditionally
// returned the current Primary() for empty kid, which meant the
// moment SwapPrimary moved kid-1 into the verifier set, every
// in-flight legacy token started failing signature verification.
//
// The fix made the empty-kid path enumerate every key in the ring
// (primary + verifiers) and try each. This test exercises that:
// mint a legacy-style token under kid-1, rotate primary to kid-2,
// confirm the legacy token still verifies via the kid-1 verifier
// slot.
func TestSigner_BackwardsCompat_LegacyTokenAfterRotation(t *testing.T) {
	k1 := make([]byte, 32)
	for i := range k1 {
		k1[i] = 0xCC
	}
	// Mint a legacy token (no kid header) before any keyring is in play.
	legacySigner, err := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		HMACKey:   k1,
		Issuer:    "k",
		Audience:  "k",
	})
	if err != nil {
		t.Fatalf("legacy NewSigner: %v", err)
	}
	legacyToken, err := legacySigner.Issue(Claims{UserID: uuid.New(), TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("legacy Issue: %v", err)
	}

	// Operator enables keyring with k1 as the initial primary.
	ring, err := NewKeyRing(SigningKey{KID: "kid-1", Algorithm: AlgHS256, HMACKey: k1})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	upgraded, err := NewSigner(SignerConfig{
		Algorithm: AlgHS256,
		KeyRing:   ring,
		Issuer:    "k",
		Audience:  "k",
	})
	if err != nil {
		t.Fatalf("upgraded NewSigner: %v", err)
	}
	if _, err := upgraded.Verify(legacyToken); err != nil {
		t.Fatalf("legacy token must verify before rotation: %v", err)
	}

	// Operator rotates: kid-2 becomes primary, kid-1 demoted to verifier.
	k2 := make([]byte, 32)
	for i := range k2 {
		k2[i] = 0xDD
	}
	if err := ring.SwapPrimary("kid-2", SigningKey{KID: "kid-2", Algorithm: AlgHS256, HMACKey: k2}); err != nil {
		t.Fatalf("SwapPrimary: %v", err)
	}

	// Legacy token (still no kid header) MUST still verify — it
	// was signed with k1, and k1 is now in the verifier slot.
	if _, err := upgraded.Verify(legacyToken); err != nil {
		t.Fatalf("legacy token must verify after rotation: %v", err)
	}
}

// TestNewSigner_RejectsKeyRingAlgMismatch verifies the defence
// against the operator-misconfiguration case described in
// ANALYSIS_pr-review-job-...-d967d70cf92e..._0004: a SignerConfig
// declaring AlgHS256 with a KeyRing whose primary key was built
// for AlgRS256. Without the guard, the boot would succeed and
// the verify path would call rsa.VerifyPKCS1v15 with a nil RSA
// pub key on every token, producing 500s rather than a clear
// boot-time error.
func TestNewSigner_RejectsKeyRingAlgMismatch(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0xEE
	}
	// Build a ring whose primary is HS256 but ask the signer to
	// declare itself RS256. The constructor must refuse.
	ring, err := NewKeyRing(SigningKey{KID: "kid-hs", Algorithm: AlgHS256, HMACKey: k})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	_, err = NewSigner(SignerConfig{
		Algorithm: AlgRS256,
		KeyRing:   ring,
	})
	if err == nil {
		t.Fatal("NewSigner accepted RS256 signer with HS256 ring primary; want refusal")
	}
	if !strings.Contains(err.Error(), "algorithm") {
		t.Fatalf("error should name the algorithm mismatch; got %v", err)
	}
}

// TestKeyRing_All_ReturnsPrimaryFirstThenSortedVerifiers verifies
// the iteration contract of KeyRing.All — primary appears first,
// verifiers follow in sorted-KID order. This guarantees the
// legacy-token verifier path tries the most-recently-rotated key
// first (the common case) without burning extra time on stale
// verifiers when the common case wins.
func TestKeyRing_All_ReturnsPrimaryFirstThenSortedVerifiers(t *testing.T) {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0x77
	}
	ring, err := NewKeyRing(SigningKey{KID: "kid-primary", Algorithm: AlgHS256, HMACKey: k})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	for _, kid := range []string{"kid-c", "kid-a", "kid-b"} {
		if err := ring.AddVerifier(SigningKey{KID: kid, Algorithm: AlgHS256, HMACKey: k}); err != nil {
			t.Fatalf("AddVerifier %s: %v", kid, err)
		}
	}
	all := ring.All()
	if len(all) != 4 {
		t.Fatalf("expected 4 keys, got %d", len(all))
	}
	got := make([]string, 0, len(all))
	for _, k := range all {
		got = append(got, k.KID)
	}
	want := []string{"kid-primary", "kid-a", "kid-b", "kid-c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("All[%d]: got %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
}
