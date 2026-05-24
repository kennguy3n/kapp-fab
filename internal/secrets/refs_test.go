package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseRef_Matrix pins the 1:1 mapping between the validator
// in helpdesk/mailboxes/mailbox.go and the resolver here. If
// either side drifts, the mailbox CRUD path will accept refs
// the resolver cannot dispatch (or vice versa).
func TestParseRef_Matrix(t *testing.T) {
	cases := []struct {
		ref    string
		scheme string
		value  string
		err    error
	}{
		// env:NAME
		{"env:KAPP_IMAP_PW", "env", "KAPP_IMAP_PW", nil},
		// vault:// path
		{"vault://kapp/imap/mailbox-a", "vault", "kapp/imap/mailbox-a", nil},
		// aws:// name
		{"aws://kapp/imap/mailbox-a", "aws", "kapp/imap/mailbox-a", nil},
		// aws:arn:... (value retains the arn: marker)
		{"aws:arn:aws:secretsmanager:us-east-1:123:secret:kapp/imap/a-A1b2", "aws",
			"arn:aws:secretsmanager:us-east-1:123:secret:kapp/imap/a-A1b2", nil},
		// gcp:// path
		{"gcp://projects/kapp/secrets/imap-a/versions/latest", "gcp",
			"projects/kapp/secrets/imap-a/versions/latest", nil},
		// gcp:projects/... (value retains the projects/ marker)
		{"gcp:projects/kapp/secrets/imap-a", "gcp",
			"projects/kapp/secrets/imap-a", nil},
		// file scheme — triple slash for absolute path.
		{"file:///etc/kapp/imap.pw", "file", "/etc/kapp/imap.pw", nil},
		// file scheme — single slash form, marker retained.
		{"file:/etc/kapp/imap.pw", "file", "/etc/kapp/imap.pw", nil},
		// Errors
		{"", "", "", ErrUnknownScheme},
		{"plaintextpassword", "", "", ErrUnknownScheme},
		{"vauly://typo", "", "", ErrUnknownScheme},
		{"https://oops.com", "", "", ErrUnknownScheme},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			scheme, value, err := ParseRef(tc.ref)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Errorf("err: got %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if scheme != tc.scheme {
				t.Errorf("scheme: got %q, want %q", scheme, tc.scheme)
			}
			if value != tc.value {
				t.Errorf("value: got %q, want %q", value, tc.value)
			}
		})
	}
}

// TestParseRef_LongerPrefixFirst pins the order-sensitive
// dispatch: "file:///abs" must match "file://" not "file:/"
// because the longer prefix has stricter semantics.
func TestParseRef_LongerPrefixFirst(t *testing.T) {
	scheme, value, err := ParseRef("file:///etc/kapp/imap.pw")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if scheme != "file" {
		t.Errorf("scheme: %q", scheme)
	}
	// "file://" strip leaves "/etc/kapp/imap.pw" (NOT
	// "//etc/kapp/imap.pw" which is what "file:/" + marker
	// would give).
	if value != "/etc/kapp/imap.pw" {
		t.Errorf("value: %q (expected /etc/kapp/imap.pw)", value)
	}
}

// fakeProvider is a counting in-memory Provider for resolver
// dispatch tests. The test asserts that the resolver calls the
// right provider with the parsed value, and that backend-level
// errors are surfaced unchanged.
type fakeProvider struct {
	name   string
	values map[string][]byte
	err    error
	calls  atomic.Int64
}

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) GetSecret(_ context.Context, key string) (SecretValue, error) {
	p.calls.Add(1)
	if p.err != nil {
		return SecretValue{}, p.err
	}
	v, ok := p.values[key]
	if !ok {
		return SecretValue{}, ErrSecretNotFound
	}
	return SecretValue{Bytes: v}, nil
}

// TestRefResolver_DispatchMatrix pins that each scheme routes
// to the right provider with the right value.
func TestRefResolver_DispatchMatrix(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("vault-pw"),
	}}
	aws := &fakeProvider{name: "aws", values: map[string][]byte{
		"kapp/imap/a":                                                 []byte("aws-pw"),
		"arn:aws:secretsmanager:us-east-1:123:secret:kapp/imap/a-A1b2": []byte("aws-arn-pw"),
	}}
	gcp := &fakeProvider{name: "gcp", values: map[string][]byte{
		"projects/kapp/secrets/imap-a/versions/latest": []byte("gcp-pw"),
		"projects/kapp/secrets/imap-a":                 []byte("gcp-short-pw"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault, AWS: aws, GCP: gcp})

	// Vault
	got, err := r.Resolve(context.Background(), "vault://kapp/imap/a")
	if err != nil || string(got) != "vault-pw" {
		t.Errorf("vault: got %q err %v", got, err)
	}
	if vault.calls.Load() != 1 {
		t.Errorf("vault.calls = %d, want 1", vault.calls.Load())
	}

	// AWS short
	got, err = r.Resolve(context.Background(), "aws://kapp/imap/a")
	if err != nil || string(got) != "aws-pw" {
		t.Errorf("aws://: got %q err %v", got, err)
	}
	// AWS ARN — value retains the arn: marker
	got, err = r.Resolve(context.Background(), "aws:arn:aws:secretsmanager:us-east-1:123:secret:kapp/imap/a-A1b2")
	if err != nil || string(got) != "aws-arn-pw" {
		t.Errorf("aws:arn: got %q err %v", got, err)
	}
	if aws.calls.Load() != 2 {
		t.Errorf("aws.calls = %d, want 2", aws.calls.Load())
	}

	// GCP //
	got, err = r.Resolve(context.Background(), "gcp://projects/kapp/secrets/imap-a/versions/latest")
	if err != nil || string(got) != "gcp-pw" {
		t.Errorf("gcp://: got %q err %v", got, err)
	}
	// GCP short
	got, err = r.Resolve(context.Background(), "gcp:projects/kapp/secrets/imap-a")
	if err != nil || string(got) != "gcp-short-pw" {
		t.Errorf("gcp: got %q err %v", got, err)
	}
	if gcp.calls.Load() != 2 {
		t.Errorf("gcp.calls = %d, want 2", gcp.calls.Load())
	}
}

// TestRefResolver_EnvScheme pins env: dispatch (no provider).
func TestRefResolver_EnvScheme(t *testing.T) {
	r := NewRefResolver(RefResolverOptions{})
	t.Setenv("KAPP_TEST_REFS_ENV", "env-pw")
	got, err := r.Resolve(context.Background(), "env:KAPP_TEST_REFS_ENV")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != "env-pw" {
		t.Errorf("value: %q", got)
	}

	// Unset env var → ErrSecretNotFound.
	t.Setenv("KAPP_TEST_REFS_ENV", "")
	if _, err := r.Resolve(context.Background(), "env:KAPP_TEST_REFS_ENV"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("unset env: got %v, want ErrSecretNotFound", err)
	}
}

// TestRefResolver_FileScheme pins file:// dispatch (no provider).
// Uses a tmp file the test owns end-to-end.
func TestRefResolver_FileScheme(t *testing.T) {
	dir := t.TempDir()
	pwPath := filepath.Join(dir, "imap.pw")
	if err := os.WriteFile(pwPath, []byte("file-pw\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := NewRefResolver(RefResolverOptions{})
	got, err := r.Resolve(context.Background(), "file://"+pwPath)
	if err != nil {
		t.Fatalf("file://: %v", err)
	}
	// Trailing newline must be trimmed.
	if string(got) != "file-pw" {
		t.Errorf("file://: %q", got)
	}
	got, err = r.Resolve(context.Background(), "file:"+pwPath)
	if err != nil {
		t.Fatalf("file:: %v", err)
	}
	if string(got) != "file-pw" {
		t.Errorf("file:: %q", got)
	}

	// Missing file → ErrSecretNotFound.
	if _, err := r.Resolve(context.Background(), "file://"+filepath.Join(dir, "missing.pw")); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("missing file: got %v, want ErrSecretNotFound", err)
	}
}

// TestRefResolver_FileSchemeRejectsRelative pins the
// path-traversal guard: relative paths and ".." segments are
// refused.
func TestRefResolver_FileSchemeRejectsRelative(t *testing.T) {
	r := NewRefResolver(RefResolverOptions{})
	cases := []string{
		"file:relative.pw",
		"file:./relative.pw",
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			_, err := r.Resolve(context.Background(), ref)
			if err == nil {
				t.Fatalf("expected error for ref %q", ref)
			}
		})
	}
	// And the resolver rejects ".." even when surfaced as a
	// "file://absolute" with embedded segments.
	if _, err := r.Resolve(context.Background(), "file:///etc/../etc/passwd"); err == nil {
		t.Errorf("expected error for traversal ref")
	}
}

// TestRefResolver_NotConfigured pins that schemes without a
// wired provider return ErrProviderNotConfigured (not a panic).
func TestRefResolver_NotConfigured(t *testing.T) {
	r := NewRefResolver(RefResolverOptions{})
	for _, ref := range []string{
		"vault://kapp/imap/a",
		"aws://kapp/imap/a",
		"gcp://projects/kapp/secrets/imap-a",
	} {
		_, err := r.Resolve(context.Background(), ref)
		if !errors.Is(err, ErrProviderNotConfigured) {
			t.Errorf("%s: got %v, want ErrProviderNotConfigured", ref, err)
		}
	}
}

// TestRefResolver_UnknownScheme pins that a typo'd ref returns
// ErrUnknownScheme rather than (e.g.) silently falling through
// to env or to a generic error.
func TestRefResolver_UnknownScheme(t *testing.T) {
	r := NewRefResolver(RefResolverOptions{})
	cases := []string{
		"",
		"plaintext",
		"vauly://typo",
		"file",
		"file:",
		"https://oops",
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			_, err := r.Resolve(context.Background(), ref)
			if !errors.Is(err, ErrUnknownScheme) {
				t.Errorf("%q: got %v, want ErrUnknownScheme", ref, err)
			}
		})
	}
}

// TestPasswordCache_HitMiss pins the cache behaviour: a first
// Resolve calls the resolver; a second Resolve (within TTL)
// returns the cached value without calling the resolver.
func TestPasswordCache_HitMiss(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw1"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 5*time.Minute)
	const scope = "mailbox-A"
	const ref = "vault://kapp/imap/a"

	got, err := cache.Resolve(context.Background(), scope, ref)
	if err != nil || string(got) != "pw1" {
		t.Fatalf("first: %q %v", got, err)
	}
	if vault.calls.Load() != 1 {
		t.Errorf("after first: calls=%d, want 1", vault.calls.Load())
	}

	got, err = cache.Resolve(context.Background(), scope, ref)
	if err != nil || string(got) != "pw1" {
		t.Fatalf("second: %q %v", got, err)
	}
	// Cache must have absorbed the second call.
	if vault.calls.Load() != 1 {
		t.Errorf("after second: calls=%d, want 1 (cache miss)", vault.calls.Load())
	}
}

// TestPasswordCache_Expiry pins that the cache calls the
// resolver again once the entry's TTL expires.
func TestPasswordCache_Expiry(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw1"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 1*time.Minute)
	// Pin the clock so we don't sleep in the test.
	clock := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return clock }

	if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if vault.calls.Load() != 1 {
		t.Errorf("calls=%d", vault.calls.Load())
	}

	// Within TTL — still cached.
	clock = clock.Add(30 * time.Second)
	if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
		t.Fatalf("within-ttl: %v", err)
	}
	if vault.calls.Load() != 1 {
		t.Errorf("within-ttl: calls=%d, want 1", vault.calls.Load())
	}

	// Past TTL — resolver fires again.
	clock = clock.Add(2 * time.Minute)
	if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
		t.Fatalf("past-ttl: %v", err)
	}
	if vault.calls.Load() != 2 {
		t.Errorf("past-ttl: calls=%d, want 2", vault.calls.Load())
	}
}

// TestPasswordCache_RefChangeBypasses pins that changing the
// ref (e.g. operator rotated to a new Vault path) produces a
// cache miss even within the same scope and TTL window.
func TestPasswordCache_RefChangeBypasses(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw-A"),
		"kapp/imap/b": []byte("pw-B"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 5*time.Minute)

	if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
		t.Fatalf("ref A: %v", err)
	}
	// Same scope, different ref → cache miss.
	got, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/b")
	if err != nil {
		t.Fatalf("ref B: %v", err)
	}
	if string(got) != "pw-B" {
		t.Errorf("ref B: got %q, want pw-B", got)
	}
	if vault.calls.Load() != 2 {
		t.Errorf("calls=%d, want 2", vault.calls.Load())
	}
}

// TestPasswordCache_Invalidate pins explicit invalidation.
func TestPasswordCache_Invalidate(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw1"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 5*time.Minute)

	if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	cache.Invalidate("mb", "vault://kapp/imap/a")
	// Next call must miss the cache.
	if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
		t.Fatalf("after invalidate: %v", err)
	}
	if vault.calls.Load() != 2 {
		t.Errorf("calls=%d, want 2 (invalidate forced refetch)", vault.calls.Load())
	}
}

// TestPasswordCache_InvalidateScope pins the scope-level drop
// used when a mailbox row is hard-deleted.
func TestPasswordCache_InvalidateScope(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw-A"),
		"kapp/imap/b": []byte("pw-B"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 5*time.Minute)

	_, _ = cache.Resolve(context.Background(), "mb-1", "vault://kapp/imap/a")
	_, _ = cache.Resolve(context.Background(), "mb-1", "vault://kapp/imap/b")
	_, _ = cache.Resolve(context.Background(), "mb-2", "vault://kapp/imap/a")
	if vault.calls.Load() != 3 {
		t.Fatalf("warm: calls=%d, want 3", vault.calls.Load())
	}

	cache.InvalidateScope("mb-1")

	// mb-1's two entries are gone — re-resolving fires the
	// resolver again.
	_, _ = cache.Resolve(context.Background(), "mb-1", "vault://kapp/imap/a")
	_, _ = cache.Resolve(context.Background(), "mb-1", "vault://kapp/imap/b")
	if vault.calls.Load() != 5 {
		t.Errorf("after scope-invalidate: calls=%d, want 5", vault.calls.Load())
	}

	// mb-2 still warm.
	_, _ = cache.Resolve(context.Background(), "mb-2", "vault://kapp/imap/a")
	if vault.calls.Load() != 5 {
		t.Errorf("mb-2 should be warm: calls=%d, want 5", vault.calls.Load())
	}
}

// TestPasswordCache_TTLZeroDisablesCache pins the disable-the-
// cache mode: ttl<=0 means every call goes straight to the
// resolver.
func TestPasswordCache_TTLZeroDisablesCache(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw1"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 0)

	for range 3 {
		if _, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a"); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	if vault.calls.Load() != 3 {
		t.Errorf("calls=%d, want 3 (cache disabled)", vault.calls.Load())
	}
}

// TestPasswordCache_DefensiveCopy pins that mutating the
// returned slice does NOT corrupt the cached entry.
func TestPasswordCache_DefensiveCopy(t *testing.T) {
	vault := &fakeProvider{name: "vault", values: map[string][]byte{
		"kapp/imap/a": []byte("pw1234"),
	}}
	r := NewRefResolver(RefResolverOptions{Vault: vault})
	cache := NewPasswordCache(r, 5*time.Minute)

	first, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Caller mutates the returned slice.
	for i := range first {
		first[i] = 'X'
	}

	// Second call must return the original cached snapshot,
	// not the mutated copy.
	second, err := cache.Resolve(context.Background(), "mb", "vault://kapp/imap/a")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(second) != "pw1234" {
		t.Errorf("cache mutated by caller: got %q", second)
	}
}
