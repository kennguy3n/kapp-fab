package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RefResolver dispatches "scheme-prefixed" secret references to
// the matching Provider. It is the layer that turns operator-
// facing strings — like the Helpdesk mailbox table's PasswordRef
// column — into the actual secret bytes.
//
// # Why a separate dispatcher
//
// The existing Provider interface answers a single backend per
// process: factory.NewFromConfig picks one of env / file / aws /
// vault / gcp at boot and every GetSecret call goes through it.
// That is the right shape for system-wide secrets (the JWT
// signing key, the captcha secret) where the operator runs one
// secret store.
//
// User-supplied refs are the opposite case. Each Helpdesk
// mailbox row carries its own PasswordRef and the value can be
// in a different backend per mailbox — Mailbox A's password
// might live in Vault, Mailbox B's in AWS Secrets Manager, and
// Mailbox C's may be a plain env var for the local-dev
// install. RefResolver matches the scheme prefix in the ref
// (env:, vault://, aws://, gcp://, file://) and routes to the
// corresponding pre-built Provider.
//
// # Scheme matrix
//
// The scheme prefixes mirror the validation list in
// internal/helpdesk/mailboxes/mailbox.go:looksLikePlaintextPassword.
// The mailbox row stores the ref verbatim; the resolver here is
// the matching consumer.
//
//   - env:NAME                              os.Getenv(NAME)
//   - vault://path/to/secret                Vault KV v2 read of path/to/secret
//   - aws://name-or-arn                     AWS SM read of name-or-arn
//   - aws:arn:aws:secretsmanager:…          AWS SM read of arn:…
//   - gcp://projects/PROJECT/secrets/NAME/versions/V
//     gcp:projects/PROJECT/secrets/NAME     Google SM read of projects/...
//   - file:///etc/kapp/imap.pw              os.ReadFile of /etc/kapp/imap.pw
//   - file:/etc/kapp/imap.pw                os.ReadFile of /etc/kapp/imap.pw
//
// # Caching
//
// Operators frequently keep IMAP passwords in Vault / AWS SM with
// rotation policies measured in days. The Helpdesk worker's
// supervisor converge loop fires every 60 seconds and resolves
// the password on every Start. Without a cache the resolver
// would burn 60+ Vault / AWS SM round-trips per hour per
// mailbox on top of the actual IMAP polling.
//
// PasswordCache wraps a RefResolver with a process-local cache
// keyed on (mailboxID, ref). The TTL is set by the caller (5
// minutes is a reasonable default — short enough that a rotation
// is picked up within one TTL, long enough that the converge
// loop's per-tick cost stays in-process). The cache invalidates
// when the operator changes the ref via the mailbox CRUD path
// (the supervisor's Stop on disabled-row drops the cache key
// because the mailboxID is no longer in the active set).
type RefResolver struct {
	// Pre-built providers per scheme. The fields are nil when
	// the operator did not configure the matching backend at
	// boot; resolves of refs in that scheme return
	// ErrProviderNotConfigured so the user surfaces a clear
	// error rather than the resolver silently falling through
	// to env.
	vault Provider
	aws   Provider
	gcp   Provider
}

// RefResolverOptions wires the per-scheme providers into a new
// RefResolver. Each provider is optional — passing nil for a
// provider means refs in that scheme resolve to a clear
// "not configured" error.
//
// env and file are NOT included: those backends are stateless
// (a process env lookup / a file read) and the resolver handles
// them in-line. File refs carry their own absolute path; env
// refs carry their own variable name. There is no operator-
// configurable prefix or root to inject.
type RefResolverOptions struct {
	Vault Provider
	AWS   Provider
	GCP   Provider
}

// NewRefResolver returns a RefResolver wired with the supplied
// per-scheme providers. The function never fails — the per-
// scheme nil-check fires at resolve time, not construction
// time, so a worker can still boot when the operator opted out
// of (e.g.) the Vault backend.
func NewRefResolver(opts RefResolverOptions) *RefResolver {
	return &RefResolver{
		vault: opts.Vault,
		aws:   opts.AWS,
		gcp:   opts.GCP,
	}
}

// Resolve dispatches the supplied ref to the matching scheme
// handler and returns the resolved secret bytes. The returned
// slice MUST NOT be retained by the caller across the call
// boundary — it is owned by the upstream Provider and may be
// zeroed after use by the file backend's defer-cleanup. The
// supervisor's password cache holds an immutable copy in its
// own buffer.
//
// Errors:
//
//   - ErrUnknownScheme when the ref does not match any known
//     prefix. The caller must surface this as an operator-
//     facing error so a typo in the mailbox CRUD payload fails
//     loudly at converge time rather than at first-IMAP-LOGIN
//     time.
//   - ErrProviderNotConfigured when the scheme matches but the
//     resolver was constructed without the corresponding
//     provider. Same surfacing rules.
//   - ErrSecretNotFound when the backend confirms the key is
//     missing. Operators who legitimately mean "no password"
//     should configure the mailbox row as disabled instead.
//   - any other error: transient. The supervisor's per-mailbox
//     backoff is the correct retry layer.
func (r *RefResolver) Resolve(ctx context.Context, ref string) ([]byte, error) {
	scheme, value, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "env":
		return resolveEnvRef(value)
	case "file":
		return resolveFileRef(value)
	case "vault":
		if r.vault == nil {
			return nil, fmt.Errorf("%w: vault provider", ErrProviderNotConfigured)
		}
		sv, err := r.vault.GetSecret(ctx, value)
		if err != nil {
			return nil, err
		}
		return sv.Bytes, nil
	case "aws":
		if r.aws == nil {
			return nil, fmt.Errorf("%w: aws provider", ErrProviderNotConfigured)
		}
		sv, err := r.aws.GetSecret(ctx, value)
		if err != nil {
			return nil, err
		}
		return sv.Bytes, nil
	case "gcp":
		if r.gcp == nil {
			return nil, fmt.Errorf("%w: gcp provider", ErrProviderNotConfigured)
		}
		sv, err := r.gcp.GetSecret(ctx, value)
		if err != nil {
			return nil, err
		}
		return sv.Bytes, nil
	default:
		// ParseRef would have returned ErrUnknownScheme; this
		// is unreachable unless ParseRef changes shape.
		return nil, fmt.Errorf("%w: %s", ErrUnknownScheme, scheme)
	}
}

// ErrUnknownScheme indicates that the ref string did not match
// any known scheme prefix. The caller must surface this as a
// user-facing validation error so a typo (e.g. "vauly://...")
// is caught at the mailbox CRUD layer before it reaches the
// IMAP fleet.
var ErrUnknownScheme = errors.New("secrets: unknown ref scheme")

// ParseRef extracts (scheme, value) from a ref string. The
// scheme list matches the validation in
// internal/helpdesk/mailboxes/mailbox.go:looksLikePlaintextPassword
// 1:1 — if these lists drift the validator will accept refs the
// resolver cannot dispatch (or vice versa).
//
// Returns ErrUnknownScheme when none of the known prefixes match.
//
// The longer prefix MUST be tested before the shorter one so
// "file:///abs" matches "file://" not "file:/". The order of
// the prefix list below honours that constraint.
func ParseRef(ref string) (scheme, value string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("%w: empty ref", ErrUnknownScheme)
	}
	// (prefix, scheme, preserveMarker) — when preserveMarker is
	// true the value retains the marker substring (e.g.
	// "aws:arn:..." → value="arn:..."); when false the prefix
	// is stripped wholesale.
	type entry struct {
		prefix         string
		scheme         string
		preserveMarker string
	}
	// Order matters: longer/more-specific prefixes first.
	entries := []entry{
		{prefix: "vault://", scheme: "vault"},
		{prefix: "aws://", scheme: "aws"},
		{prefix: "aws:arn:", scheme: "aws", preserveMarker: "arn:"},
		{prefix: "gcp://", scheme: "gcp"},
		{prefix: "gcp:projects/", scheme: "gcp", preserveMarker: "projects/"},
		{prefix: "file://", scheme: "file"},
		{prefix: "file:/", scheme: "file", preserveMarker: "/"},
		{prefix: "env:", scheme: "env"},
	}
	for _, e := range entries {
		if !strings.HasPrefix(ref, e.prefix) {
			continue
		}
		if e.preserveMarker != "" {
			return e.scheme, e.preserveMarker + strings.TrimPrefix(ref, e.prefix), nil
		}
		return e.scheme, strings.TrimPrefix(ref, e.prefix), nil
	}
	return "", "", fmt.Errorf("%w: %q", ErrUnknownScheme, ref)
}

// resolveEnvRef reads the named env var. Returns
// ErrSecretNotFound on unset / empty so a fat-fingered .env
// gives the same surfacing as a typo'd ref.
func resolveEnvRef(name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: env: ref missing variable name", ErrSecretNotFound)
	}
	v := os.Getenv(name)
	if v == "" {
		return nil, fmt.Errorf("%w: env %s unset", ErrSecretNotFound, name)
	}
	return []byte(v), nil
}

// resolveFileRef reads the file at the supplied absolute path.
// Unlike FileProvider this is NOT rooted at a pre-configured
// rootDir — the operator names the file directly in the ref.
// The caller is responsible for ensuring the mailbox CRUD layer
// rejects refs that name paths outside the operator's intended
// secret directory (the orchestrator's filesystem perms are the
// gate here; the resolver only enforces "exists + non-empty").
func resolveFileRef(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: file: ref missing path", ErrSecretNotFound)
	}
	// Defence in depth: refuse relative paths and refuse paths
	// containing ".." segments. file:// refs must point to a
	// known location the operator chose; we do not chase
	// symlinks or resolve ../.. tricks.
	// Refuse ".." segments on the RAW input — filepath.Clean
	// would resolve them and hide the operator's intent. A ref
	// like file:///etc/../etc/passwd is almost certainly a
	// traversal attempt and we want it surfaced loudly.
	if strings.Contains(path, "..") {
		return nil, fmt.Errorf("secrets: file ref %q contains '..'", path)
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("secrets: file ref %q must be absolute", path)
	}
	f, err := os.Open(cleaned) //nolint:gosec // G304 cleaned path validated above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: file %s missing", ErrSecretNotFound, cleaned)
		}
		return nil, fmt.Errorf("%w: open %s: %w", ErrProviderUnavailable, cleaned, err)
	}
	defer func() { _ = f.Close() }()
	// 1 MiB cap — same rationale as FileProvider: any password
	// bigger than this is almost certainly a misconfigured
	// mount.
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrProviderUnavailable, cleaned, err)
	}
	trimmed := []byte(strings.TrimRight(string(raw), "\r\n\t "))
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: file %s empty", ErrSecretNotFound, cleaned)
	}
	return trimmed, nil
}

// PasswordCache amortises remote-backend round-trips across
// multiple supervisor converge ticks. The supervisor's loop
// fires every 60 seconds and resolves the password for each
// active mailbox on every Start — without a cache that becomes
// 60+ Vault / AWS SM hits per hour per mailbox.
//
// The cache key is the (mailbox-scope, ref) tuple. The scope is
// the caller's choice — the supervisor uses the mailbox UUID
// string so a ref change in the CRUD path invalidates only the
// affected mailbox, leaving siblings warm. The ref is part of
// the key so a CRUD-level ref change is picked up immediately
// (the new ref produces a cache miss).
//
// TTL is set at construction time. 5 minutes is the default;
// operators with faster rotation cadences can shorten it via
// the worker's secrets config. The cache uses lazy eviction
// (no background goroutine) — expired entries are removed on
// the next Get attempt that touches them.
type PasswordCache struct {
	resolver *RefResolver
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]passwordCacheEntry
}

type passwordCacheEntry struct {
	value     []byte
	expiresAt time.Time
}

// NewPasswordCache returns a cache wrapping resolver. ttl <= 0
// is treated as "no caching" (every Get goes straight to the
// resolver) — useful in tests that want to disable the cache
// without changing the call shape.
func NewPasswordCache(resolver *RefResolver, ttl time.Duration) *PasswordCache {
	return &PasswordCache{
		resolver: resolver,
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]passwordCacheEntry),
	}
}

// Resolve returns the resolved bytes for ref, hitting the
// underlying resolver only when the cache entry for (scope, ref)
// is missing or expired. The scope MUST be unique per logical
// caller — the supervisor uses the mailbox UUID — so an
// invalidation in one scope does not blow away siblings.
//
// The returned slice is a fresh copy on every call so the
// caller is free to zero / mutate it without corrupting the
// cached entry. The cost is one allocation per Resolve, which
// is dwarfed by the converge tick's 60-second cadence.
func (c *PasswordCache) Resolve(ctx context.Context, scope, ref string) ([]byte, error) {
	if c == nil || c.resolver == nil {
		return nil, errors.New("secrets: password cache not initialised")
	}
	key := scope + "\x00" + ref
	if c.ttl > 0 {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok {
			if c.now().Before(e.expiresAt) {
				out := make([]byte, len(e.value))
				copy(out, e.value)
				c.mu.Unlock()
				return out, nil
			}
			// Expired — remove now so a concurrent
			// resolver call does not stale-fill from
			// the same expired entry.
			delete(c.entries, key)
		}
		c.mu.Unlock()
	}
	bytes, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if c.ttl > 0 {
		// Defensive copy — the resolver may have returned a
		// slice owned by an upstream backend (file mmap,
		// Vault response body buffer). Copying ensures the
		// cache holds an immutable snapshot independent of
		// the caller's slice.
		stored := make([]byte, len(bytes))
		copy(stored, bytes)
		c.mu.Lock()
		c.entries[key] = passwordCacheEntry{
			value:     stored,
			expiresAt: c.now().Add(c.ttl),
		}
		c.mu.Unlock()
		// Return another fresh copy so the caller's slice is
		// distinct from the cached one too.
		out := make([]byte, len(stored))
		copy(out, stored)
		return out, nil
	}
	return bytes, nil
}

// Invalidate removes the cache entry for (scope, ref) so the
// next Resolve call re-fetches from the backend. The supervisor
// calls this when a mailbox row is removed (the operator
// disabled it) so a stopped Poller's password is not held in
// memory longer than necessary. Idempotent — calling on a
// missing key is a no-op.
func (c *PasswordCache) Invalidate(scope, ref string) {
	if c == nil {
		return
	}
	key := scope + "\x00" + ref
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// InvalidateScope drops every entry that starts with scope. The
// supervisor uses this when a mailbox row is hard-deleted (so
// every ref ever observed for the mailbox is forgotten, not
// just the current one) and when a tenant is purged.
func (c *PasswordCache) InvalidateScope(scope string) {
	if c == nil {
		return
	}
	prefix := scope + "\x00"
	c.mu.Lock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}
