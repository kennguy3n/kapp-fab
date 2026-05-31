// Package runtime is the marketplace extension RUNTIME engine —
// the live counterpart to the catalog the B2 internal/marketplace
// package builds. Where the catalog tracks *which* extension
// versions exist, the runtime owns *what happens* when an operator
// installs an extension into a tenant and what happens when an
// agent invokes one of that extension's registered tools.
//
// The engine is exposed via two top-level entry points:
//
//	Engine.Install   — transactional registration of the manifest's
//	                   KTypes / workflows / agent tools / webhook
//	                   subscriptions, sandwiched between a blocking
//	                   pre_install lifecycle hook and a best-effort
//	                   post_install lifecycle hook. Either the
//	                   whole install commits, or none of it does.
//
//	Engine.Uninstall — symmetric teardown. Cascade deletes the
//	                   registration tables (via FK ON DELETE
//	                   CASCADE) and dispatches best-effort
//	                   pre_uninstall + post_uninstall hooks.
//
// Tool dispatch is exposed via Dispatcher.Invoke, which performs
// a signed HMAC-SHA256 HTTPS POST to the extension's webhook with
// retry/backoff per the manifest descriptor. Every attempt — successful
// or not — is recorded in marketplace_dispatch_log so the audit
// trail survives uninstall.
//
// Lifecycle hooks are derived from the ${EXTENSION_WEBHOOK_BASE}
// placeholder using a fixed path convention:
//
//	POST {EXTENSION_WEBHOOK_BASE}/lifecycle/pre_install
//	POST {EXTENSION_WEBHOOK_BASE}/lifecycle/post_install
//	POST {EXTENSION_WEBHOOK_BASE}/lifecycle/pre_uninstall
//	POST {EXTENSION_WEBHOOK_BASE}/lifecycle/post_uninstall
//
// A 404 response on any lifecycle endpoint is treated as
// "extension does not implement this phase" — the engine logs INFO
// and moves on. This keeps the manifest schema v1 frozen while
// still letting extensions opt into lifecycle observation.
//
// Receive-side signature verification (the extension's own webhook
// server verifying the X-Kapp-Signature header) lives in the
// runtime/verify subpackage so extension authors can vendor just
// the verification half without pulling the dispatch half.
package runtime

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// LifecyclePhase is one of the four blocking points around install
// and uninstall where the engine dispatches a signed POST to the
// extension's webhook. Mapped to the URL path suffix on
// ${EXTENSION_WEBHOOK_BASE}/lifecycle/{phase}.
type LifecyclePhase string

// LifecyclePhase enum values. Each maps to a URL path suffix
// (/lifecycle/<value>) on the extension's webhook_base AND to a
// DispatchKind enum value via DispatchKindForPhase.
const (
	PhasePreInstall    LifecyclePhase = "pre_install"
	PhasePostInstall   LifecyclePhase = "post_install"
	PhasePreUninstall  LifecyclePhase = "pre_uninstall"
	PhasePostUninstall LifecyclePhase = "post_uninstall"
)

// LifecyclePath returns the URL path component for this phase as it
// will be appended to EXTENSION_WEBHOOK_BASE.
func (p LifecyclePhase) LifecyclePath() string {
	return "/lifecycle/" + string(p)
}

// DispatchKind is the marketplace_dispatch_log.kind column. One
// value per audit-log row type.
type DispatchKind string

// DispatchKind enum values. Each is a string written verbatim to
// marketplace_dispatch_log.kind. The lifecycle_* values correspond
// one-to-one to LifecyclePhase via DispatchKindForPhase; the
// remaining values are written by the agent-tool dispatcher
// (tool_invoke), the event delivery worker (event_delivery), and
// the health-check probe (health_check).
const (
	KindToolInvoke             DispatchKind = "tool_invoke"
	KindLifecyclePreInstall    DispatchKind = "lifecycle_pre_install"
	KindLifecyclePostInstall   DispatchKind = "lifecycle_post_install"
	KindLifecyclePreUninstall  DispatchKind = "lifecycle_pre_uninstall"
	KindLifecyclePostUninstall DispatchKind = "lifecycle_post_uninstall"
	KindEventDelivery          DispatchKind = "event_delivery"
	KindHealthCheck            DispatchKind = "health_check"
)

// DispatchKindForPhase maps a lifecycle phase to its dispatch log
// kind. Both enums exist because the dispatch log also stores rows
// for non-lifecycle events (tool_invoke, event_delivery,
// health_check) that have no phase.
func DispatchKindForPhase(p LifecyclePhase) DispatchKind {
	switch p {
	case PhasePreInstall:
		return KindLifecyclePreInstall
	case PhasePostInstall:
		return KindLifecyclePostInstall
	case PhasePreUninstall:
		return KindLifecyclePreUninstall
	case PhasePostUninstall:
		return KindLifecyclePostUninstall
	default:
		return ""
	}
}

// Errors used across the engine. Catalogued here so callers can
// distinguish operator-fixable conditions (permissions / features)
// from engine-internal failures (signing-secret generation, RNG
// exhaustion). errors.Is matches by value.
var (
	// ErrPreInstallRejected is returned when the extension's
	// pre_install lifecycle hook responds with a non-2xx status.
	// The Install transaction has not been started yet — the
	// installation row is not written.
	ErrPreInstallRejected = errors.New("runtime: pre_install hook rejected install")

	// ErrPreUninstallRejected is the uninstall counterpart. The
	// extension's pre_uninstall hook returned a non-2xx,
	// non-404 response; the engine refused to proceed with the
	// uninstall. Operators can force-uninstall by setting
	// UninstallRequest.SkipHooks = true.
	ErrPreUninstallRejected = errors.New("runtime: pre_uninstall hook rejected uninstall")

	// ErrInvalidWebhookBase is returned when the operator-supplied
	// EXTENSION_WEBHOOK_BASE fails the same https:// + URL-parse
	// sanity check that marketplace.Store.CreateInstall already
	// applies, but the runtime re-verifies before dispatching to
	// catch any case where a row was created via direct SQL (e.g.
	// the kapp-backup restore path skirts the engine).
	ErrInvalidWebhookBase = errors.New("runtime: invalid webhook_base")

	// ErrDispatchTimeout is returned when an outbound HTTP request
	// hits the descriptor's timeout. Each retry attempt has its
	// own timeout — this error is only surfaced after all retries
	// are exhausted.
	ErrDispatchTimeout = errors.New("runtime: dispatch timed out")

	// ErrToolNotRegistered is returned when Dispatcher.Invoke is
	// called with a tool name that has no row in
	// marketplace_extension_agent_tools for the (tenant,
	// installation) pair.
	ErrToolNotRegistered = errors.New("runtime: tool not registered")

	// ErrInstallationNotActive is returned when a dispatch is
	// requested against an installation whose status is not
	// 'active'. Pending / installing / disabled / failed /
	// uninstalled all block dispatch.
	ErrInstallationNotActive = errors.New("runtime: installation not active")

	// ErrLifecycleAbort is returned by EngineHooks.Dispatch when a
	// caller (test) explicitly requests aborting the lifecycle —
	// used in tests to assert that pre_install failure short-
	// circuits the install path.
	ErrLifecycleAbort = errors.New("runtime: lifecycle aborted")
)

// SigningSecret is the per-install 32-byte HMAC key, base64url-
// encoded without padding (43 chars). Generated at install time
// by GenerateSigningSecret. The encoded form is what's persisted
// in marketplace_extension_installations.signing_secret — never the
// raw bytes — so an ops dump of the column gives the operator a
// directly usable string. base64url (not standard base64) is used
// because the column may surface in HTTP headers in B4 (the receive
// side) where '+' and '/' would need additional escaping.
type SigningSecret string

// GenerateSigningSecret returns a fresh 32-byte signing secret
// drawn from crypto/rand. The 32-byte width matches the recommended
// HMAC-SHA256 key length (no advantage to longer; shorter weakens
// the construction). base64url encoding is URL- and header-safe.
func GenerateSigningSecret() (SigningSecret, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("runtime: rng exhausted: %w", err)
	}
	return SigningSecret(base64.RawURLEncoding.EncodeToString(buf)), nil
}

// Bytes returns the raw 32-byte HMAC key. Returns an error if the
// stored secret fails to decode — defensive against direct SQL
// writes that bypass the runtime's GenerateSigningSecret path.
func (s SigningSecret) Bytes() ([]byte, error) {
	if s == "" {
		return nil, errors.New("runtime: empty signing secret")
	}
	if len(s) != 43 {
		return nil, fmt.Errorf("runtime: signing secret length %d, want 43", len(s))
	}
	return base64.RawURLEncoding.DecodeString(string(s))
}

// IsValidWebhookBase performs the same https:// + parseable-URL
// check the marketplace_installations_webhook_base_https CHECK
// constraint applies, but in Go so the engine can reject early
// (before the lifecycle hook dispatch) with a structured error
// instead of a Postgres constraint violation surfacing through the
// driver. Mirrors marketplace.IsValidWebhookBase but kept here so
// the runtime doesn't depend on package-private knowledge.
func IsValidWebhookBase(base string) error {
	if base == "" {
		return ErrInvalidWebhookBase
	}
	if !strings.HasPrefix(base, "https://") {
		return ErrInvalidWebhookBase
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWebhookBase, err)
	}
	if u.Host == "" {
		return ErrInvalidWebhookBase
	}
	return nil
}

// InstallRequest captures the operator-supplied parameters of an
// Engine.Install call. The engine validates each field before
// touching the database.
type InstallRequest struct {
	// TenantID identifies the tenant receiving the install.
	TenantID uuid.UUID

	// ExtensionID + VersionID identify the catalog row being
	// installed. The engine looks up the version row to fetch its
	// manifest_yaml.
	ExtensionID uuid.UUID
	VersionID   uuid.UUID

	// WebhookBase is the operator-supplied ${EXTENSION_WEBHOOK_BASE}
	// the manifest's endpoint placeholders are resolved against.
	// MUST be https:// and parseable. Trailing slashes are
	// tolerated and stripped by the engine before persisting.
	WebhookBase string

	// Settings is the operator-supplied per-install config as a
	// JSON-serialisable map (matches the JSON schema declared by
	// the extension at manifest.SettingsSchema). The engine does
	// NOT validate against that schema in B3 — that's a B6 API
	// concern (the install endpoint should fail validation
	// before reaching the engine).
	Settings map[string]interface{}

	// InstalledBy is the user (operator) initiating the install,
	// for audit-log purposes. May be uuid.Nil for system
	// installs (e.g. a tenant-bootstrap script).
	InstalledBy uuid.UUID
}

// Validate runs cheap field-level sanity checks. Engine.Install
// calls this before dispatching the pre_install hook so a malformed
// request fails fast.
func (r *InstallRequest) Validate() error {
	if r == nil {
		return errors.New("runtime: nil install request")
	}
	if r.TenantID == uuid.Nil {
		return errors.New("runtime: tenant_id required")
	}
	if r.ExtensionID == uuid.Nil {
		return errors.New("runtime: extension_id required")
	}
	if r.VersionID == uuid.Nil {
		return errors.New("runtime: version_id required")
	}
	if err := IsValidWebhookBase(r.WebhookBase); err != nil {
		return err
	}
	return nil
}

// NormalizedWebhookBase returns WebhookBase with any trailing
// slashes removed. The dispatcher always builds lifecycle/tool URLs
// by concatenating "/lifecycle/..." or the manifest's endpoint
// (which already begins with "/"), so a trailing slash in the base
// would produce a double-slash URL — semantically equivalent under
// most HTTP servers but a defensive normalisation here keeps the
// audit log endpoints clean.
func (r *InstallRequest) NormalizedWebhookBase() string {
	return strings.TrimRight(r.WebhookBase, "/")
}

// UninstallRequest captures the parameters of an Engine.Uninstall
// call.
type UninstallRequest struct {
	TenantID       uuid.UUID
	InstallationID uuid.UUID
	// UninstalledBy is the operator initiating the uninstall.
	UninstalledBy uuid.UUID
	// SkipHooks short-circuits both pre_ and post_uninstall hook
	// dispatch. Used when the extension's webhook server is known
	// to be unreachable (e.g. publisher domain expired) and the
	// operator wants a forced removal. The audit log captures
	// "skipped" entries for the missed hooks so the forensic
	// trail is preserved.
	SkipHooks bool
}

// Validate runs field-level sanity checks.
func (r *UninstallRequest) Validate() error {
	if r == nil {
		return errors.New("runtime: nil uninstall request")
	}
	if r.TenantID == uuid.Nil {
		return errors.New("runtime: tenant_id required")
	}
	if r.InstallationID == uuid.Nil {
		return errors.New("runtime: installation_id required")
	}
	return nil
}

// DispatchRequest is the shape passed to Transport.Send.
//
// The signer derives the canonical request from this struct, so
// changing any field of DispatchRequest changes the signature and
// the receiver will reject the request. This is the by-design
// "everything is signed" property — there is no "metadata" field
// that flows around outside the HMAC.
type DispatchRequest struct {
	// TenantID + InstallationID are required for the dispatch log
	// row.
	TenantID       uuid.UUID
	InstallationID uuid.UUID
	// ExtensionID + ExtensionVersionID survive the installation
	// row's deletion via ON DELETE SET NULL on installation_id, so
	// the dispatch log row remains correlatable after uninstall.
	ExtensionID        uuid.UUID
	ExtensionVersionID uuid.UUID
	// Kind is the dispatch log row's kind column.
	Kind DispatchKind
	// URL is the absolute target URL (webhook_base + path).
	URL string
	// Body is the JSON-encoded request body. Empty body is allowed
	// (the body_sha256 is then the SHA-256 of zero bytes).
	Body []byte
	// Timeout is the per-attempt HTTP timeout. Total dispatch time
	// for N retries is up to N * Timeout + backoff between
	// attempts.
	Timeout time.Duration
	// Retry is the retry policy. Nil means single attempt.
	Retry *RetryPolicy
	// SigningSecret is the per-install HMAC key.
	SigningSecret SigningSecret
	// RequestID groups retry attempts in the dispatch log. The
	// engine generates one UUID per logical request and reuses it
	// across attempts.
	RequestID uuid.UUID
}

// RetryPolicy describes the per-dispatch retry behaviour. Mirrors
// marketplace.RetryRule but is the runtime-side, "already-validated"
// shape so the dispatcher doesn't have to re-parse "exponential"
// strings on every invoke.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts INCLUDING the
	// initial try. 1 means "no retry".
	MaxAttempts int
	// Backoff is "linear" or "exponential". Linear waits 1s, 2s,
	// 3s, ... between attempts. Exponential waits 1s, 2s, 4s, 8s,
	// 16s. The first attempt has no delay.
	Backoff string
}

// BackoffDelay returns the wait duration before attempt N
// (1-indexed). Attempt 1 always returns 0. Attempts beyond
// MaxAttempts return 0 — caller is expected to gate retries
// against MaxAttempts itself.
func (r *RetryPolicy) BackoffDelay(attempt int) time.Duration {
	if r == nil || attempt <= 1 {
		return 0
	}
	switch r.Backoff {
	case "linear":
		return time.Duration(attempt-1) * time.Second
	case "exponential":
		// 1s, 2s, 4s, 8s, 16s — cap at 16s so a pathological
		// MaxAttempts (which the validator caps at 5 anyway)
		// doesn't blow up to 32m.
		delay := time.Second
		for i := 1; i < attempt-1 && delay < 16*time.Second; i++ {
			delay *= 2
		}
		if delay > 16*time.Second {
			delay = 16 * time.Second
		}
		return delay
	default:
		return 0
	}
}

// FromManifestRetry constructs a RetryPolicy from the manifest's
// AgentToolRef.Retry pointer. Nil retry → single-attempt policy.
func FromManifestRetry(r *marketplace.RetryRule) *RetryPolicy {
	if r == nil {
		return &RetryPolicy{MaxAttempts: 1, Backoff: "exponential"}
	}
	return &RetryPolicy{
		MaxAttempts: r.MaxAttempts,
		Backoff:     r.Backoff,
	}
}

// DispatchResponse is the result returned by Transport.Send for a
// successful HTTP round-trip. A non-2xx response_status is still a
// "successful round-trip" — the dispatcher classifies 4xx as
// terminal and 5xx as retryable.
type DispatchResponse struct {
	// Status is the HTTP status code (e.g. 200, 404, 502).
	Status int
	// Body is the response body. Capped at 1 MiB by the transport
	// — larger responses are truncated and an X-Kapp-Truncated:
	// true header is added on read-back (but the audit log stores
	// only the SHA-256 of the truncated body, not the body).
	Body []byte
	// Header is a flat copy of the response headers. Used by
	// callers that need to inspect Content-Type or other metadata.
	Header map[string]string
	// Latency is the wall-clock time from request start to
	// response read completion.
	Latency time.Duration
	// Attempt is the 1-indexed attempt number that produced this
	// response. Used by the dispatch log so the audit trail
	// records the per-attempt latency, not the cumulative one.
	Attempt int
}
