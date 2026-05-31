// Package marketplace is the typed data layer for the Kapp extension
// marketplace, modelling the contracts pinned by docs/EXTENSION_SPEC.md
// (B1, PR #125).
//
// The package owns:
//
//   - Domain types for the four marketplace tables (000068 migration):
//     Extension, ExtensionVersion, ReviewState, Installation.
//   - The manifest parser + validator that turns a raw
//     kapp-extension.yaml byte slice into a typed Manifest with all
//     the hard limits from EXTENSION_SPEC §2 enforced.
//   - The Store repository the upcoming B6 API endpoints,  B4 webhook
//     dispatcher, and B5 UI extension surfacer call into.
//
// What this package intentionally does NOT do:
//
//   - Bundle extraction / file inspection. The marketplace upload
//     handler (B6) is responsible for tarball walking; this package
//     takes already-parsed manifest bytes and validates them.
//   - Webhook signing / dispatch. That is B4 — this package only
//     stores the per-tenant webhook_base.
//   - UI iframe rendering. That is B5 — this package surfaces
//     ui_extensions_count so the UI shell can decide whether to
//     hydrate the slot, but the iframe lifecycle is owned elsewhere.
//   - Review pipeline execution. That is B7 — this package surfaces
//     the typed ReviewState column for B7 to populate.
//
// All cross-tenant authorisation (e.g. "is this user allowed to
// publish under this publisher prefix?") lives at the B6 HTTP boundary
// — this package's only multi-tenant guard is the RLS policy on
// marketplace_extension_installations (the only tenant-scoped table).
package marketplace

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ExtensionStatus is the publisher-side listing state for an extension.
// Drives marketplace catalog visibility.
type ExtensionStatus string

const (
	// ExtensionStatusUnpublished — extension row exists (e.g. the
	// publisher reserved the name) but no version has yet been
	// approved for listing. Marketplace browse hides these.
	ExtensionStatusUnpublished ExtensionStatus = "unpublished"

	// ExtensionStatusListed — extension is publicly browsable and
	// installable. At least one version has cleared review.
	ExtensionStatusListed ExtensionStatus = "listed"

	// ExtensionStatusDeprecated — existing installs keep working but
	// the marketplace shows a deprecation banner and refuses new
	// installs. Publishers move here when superseded by a different
	// extension; if a future version of the same extension supersedes
	// the deprecated one, the status flips back to `listed`.
	ExtensionStatusDeprecated ExtensionStatus = "deprecated"

	// ExtensionStatusRemoved — operator-side hard takedown (e.g.
	// security incident, ToS violation). All installs are forced
	// into status='disabled' on next health check and the marketplace
	// catalogue hides the row.
	ExtensionStatusRemoved ExtensionStatus = "removed"
)

// Valid reports whether the status is one of the four well-known
// values. Surfaces 400 from API handlers.
func (s ExtensionStatus) Valid() bool {
	switch s {
	case ExtensionStatusUnpublished,
		ExtensionStatusListed,
		ExtensionStatusDeprecated,
		ExtensionStatusRemoved:
		return true
	}
	return false
}

// ReviewStatus tracks the per-version operator review pipeline that
// B7 drives. Transitions are intentionally directional — once a
// version is approved or rejected, the only way out is to publish a
// new version, never to mutate the existing review row back to an
// earlier state. (`withdrawn` is the publisher-initiated terminal
// state; `approved`/`rejected` are operator-initiated terminals.)
type ReviewStatus string

const (
	// ReviewStatusSubmitted — version uploaded, automated checks have
	// not yet started.
	ReviewStatusSubmitted ReviewStatus = "submitted"

	// ReviewStatusAutomatedPassed — every automated check that B7
	// runs (SAST, manifest schema, bundle size sanity) passed.
	// Awaiting human reviewer.
	ReviewStatusAutomatedPassed ReviewStatus = "automated_passed"

	// ReviewStatusManualReview — a human reviewer has the version
	// open. Set by the review queue UI when a reviewer claims the
	// item.
	ReviewStatusManualReview ReviewStatus = "manual_review"

	// ReviewStatusApproved — reviewer accepted the version. The
	// marketplace catalog promotes the version (or the parent
	// extension transitions unpublished→listed if this is the first
	// approved version).
	ReviewStatusApproved ReviewStatus = "approved"

	// ReviewStatusRejected — reviewer rejected the version. The
	// publisher must upload a new version with the issues addressed;
	// the rejected row stays as the audit trail.
	ReviewStatusRejected ReviewStatus = "rejected"

	// ReviewStatusWithdrawn — publisher withdrew the version before
	// review concluded (e.g. found a self-spotted bug). The version
	// row is left in place but is treated as not-installable.
	ReviewStatusWithdrawn ReviewStatus = "withdrawn"

	// ReviewStatusDeadLetter — the review worker tried and failed
	// MaxReviewAttempts times in a row (CDN unreachable, bundle
	// parser exception, etc.). B7.2 terminal state. The version
	// stays installable-blocked (not listed) until the admin
	// Rescan endpoint moves it back to `submitted` with a fresh
	// attempt budget. last_attempt_error on the row carries the
	// last failure message so the operator can decide whether to
	// rescan (transient) or investigate the bundle (persistent).
	ReviewStatusDeadLetter ReviewStatus = "dead_letter"
)

// IsTerminal returns true for the four states (`approved`, `rejected`,
// `withdrawn`, `dead_letter`) that the review pipeline never
// transitions out of without explicit admin intervention. Note that
// `dead_letter` is recoverable via the admin Rescan endpoint — same
// mechanism that re-runs a stuck `submitted` row — so terminal here
// means "the worker won't move it on its own", not "immutable for
// all time".
func (s ReviewStatus) IsTerminal() bool {
	switch s {
	case ReviewStatusApproved,
		ReviewStatusRejected,
		ReviewStatusWithdrawn,
		ReviewStatusDeadLetter:
		return true
	}
	return false
}

// Valid reports whether the status matches one of the seven well-known
// values.
func (s ReviewStatus) Valid() bool {
	switch s {
	case ReviewStatusSubmitted,
		ReviewStatusAutomatedPassed,
		ReviewStatusManualReview,
		ReviewStatusApproved,
		ReviewStatusRejected,
		ReviewStatusWithdrawn,
		ReviewStatusDeadLetter:
		return true
	}
	return false
}

// InstallStatus is the per-tenant lifecycle state for a marketplace
// extension install. Distinct from ExtensionStatus (which is global)
// and ReviewStatus (which is per-version operator review).
type InstallStatus string

const (
	// InstallStatusPending — install row created, runtime has not yet
	// completed first-time setup (settings validation, secrets
	// wiring, webhook handshake).
	InstallStatusPending InstallStatus = "pending"

	// InstallStatusInstalling — first-time setup in progress. The
	// install worker (B4) advances this to `active` on success.
	InstallStatusInstalling InstallStatus = "installing"

	// InstallStatusActive — extension is live for the tenant. Agents,
	// webhooks, UI surfaces all respect this gate.
	InstallStatusActive InstallStatus = "active"

	// InstallStatusDisabled — tenant administrator paused the
	// extension (or the marketplace flipped it off due to upstream
	// removal). Existing data is preserved; webhooks / UI / agent
	// tools are suppressed.
	InstallStatusDisabled InstallStatus = "disabled"

	// InstallStatusFailed — first-time setup failed; failure_reason
	// is populated (CHECK constraint enforces).
	InstallStatusFailed InstallStatus = "failed"

	// InstallStatusUninstalled — the row remains for audit purposes
	// but the runtime treats it as fully torn down.
	InstallStatusUninstalled InstallStatus = "uninstalled"
)

// Valid reports whether the status matches one of the six well-known
// values.
func (s InstallStatus) Valid() bool {
	switch s {
	case InstallStatusPending,
		InstallStatusInstalling,
		InstallStatusActive,
		InstallStatusDisabled,
		InstallStatusFailed,
		InstallStatusUninstalled:
		return true
	}
	return false
}

// Extension is the publisher-level listing row. One per (publisher,
// slug). The default install version is named in ListedVersion; the
// per-version rows live in ExtensionVersion.
type Extension struct {
	ID            uuid.UUID       `json:"id"`
	Name          string          `json:"name"` // "<publisher>.<slug>"
	Publisher     string          `json:"publisher"`
	Slug          string          `json:"slug"`
	DisplayName   string          `json:"display_name"`
	Description   string          `json:"description"`
	Author        string          `json:"author"`
	License       string          `json:"license"` // SPDX identifier
	Homepage      string          `json:"homepage,omitempty"`
	SupportEmail  string          `json:"support_email,omitempty"`
	IconURL       string          `json:"icon_url,omitempty"`
	Status        ExtensionStatus `json:"status"`
	ListedVersion string          `json:"listed_version,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ExtensionVersion is the per-(extension, version) bundle record. The
// row is write-once via the BEFORE UPDATE trigger in 000068 — only
// the Yanked / YankedReason fields are mutable post-publish.
type ExtensionVersion struct {
	ID                  uuid.UUID `json:"id"`
	ExtensionID         uuid.UUID `json:"extension_id"`
	Version             string    `json:"version"`     // SemVer 2.0.0
	BundleHash          string    `json:"bundle_hash"` // SHA-256 hex
	BundleSizeBytes     int64     `json:"bundle_size_bytes"`
	BundleURL           string    `json:"bundle_url"`
	Manifest            []byte    `json:"-"` // raw JSONB
	MinKappVersion      string    `json:"min_kapp_version"`
	MaxKappVersion      string    `json:"max_kapp_version,omitempty"`
	FeaturesRequired    []string  `json:"features_required"`
	PermissionsRequired []string  `json:"permissions_required"`
	KtypesCount         int       `json:"ktypes_count"`
	WorkflowsCount      int       `json:"workflows_count"`
	AgentToolsCount     int       `json:"agent_tools_count"`
	UIExtensionsCount   int       `json:"ui_extensions_count"`
	WebhooksCount       int       `json:"webhooks_count"`
	Yanked              bool      `json:"yanked"`
	YankedReason        string    `json:"yanked_reason,omitempty"`
	PublishedAt         time.Time `json:"published_at"`

	// Signature columns. Populated when the publisher attaches an
	// ed25519 signature at submit time; otherwise all three remain
	// nil/empty. The DB CHECK on marketplace_extension_versions
	// enforces the all-or-nothing invariant. The three fields are
	// kept on this struct (rather than a nested pointer) so the
	// JSON shape stays flat for clients — Signature() returns a
	// typed view when callers want one.
	BundleSignature        string     `json:"bundle_signature,omitempty"`
	BundleSignatureKeyID   string     `json:"bundle_signature_key_id,omitempty"`
	SignedAt               *time.Time `json:"signed_at,omitempty"`
}

// Signature returns the typed BundleSignature view, or nil if the
// version is unsigned. Convenience for callers that prefer the
// struct over the flat columns.
func (v ExtensionVersion) Signature() *BundleSignature {
	if v.BundleSignature == "" || v.BundleSignatureKeyID == "" || v.SignedAt == nil {
		return nil
	}
	return &BundleSignature{
		SignatureB64: v.BundleSignature,
		KeyID:        v.BundleSignatureKeyID,
		SignedAt:     *v.SignedAt,
	}
}

// ReviewState is the per-version operator review record. B7 owns the
// transitions; this package's Store reads/writes the row.
//
// AttemptCount, LastAttemptError, and LastAttemptAt are B7.2's
// dead-letter accounting. They are zero/empty for a freshly-created
// row and are bumped by the worker via
// ReviewStateStore.RecordAttemptFailure when a pipeline run errors.
// Reset to zero/empty by ResetReviewStateForRescan.
type ReviewState struct {
	ExtensionVersionID uuid.UUID    `json:"extension_version_id"`
	Status             ReviewStatus `json:"status"`
	AutomatedChecks    []byte       `json:"-"` // raw JSONB (B7 schema)
	ManualReviewNotes  string       `json:"manual_review_notes,omitempty"`
	Reviewer           string       `json:"reviewer,omitempty"`
	ReviewedAt         *time.Time   `json:"reviewed_at,omitempty"`
	AttemptCount       int          `json:"attempt_count"`
	LastAttemptError   string       `json:"last_attempt_error,omitempty"`
	LastAttemptAt      *time.Time   `json:"last_attempt_at,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

// BundleSignature is the optional ed25519 signature attached to a
// version row by the publisher at submit time. The fields are
// all-or-nothing (the DB constraint enforces this). The pipeline's
// SignatureCheck looks the KeyID up against marketplace_publisher_keys
// to find the public key and runs sign.Verify against the raw bundle
// bytes. The SignedAt timestamp is the wall-clock at submit time —
// useful for replay-window reasoning but not load-bearing for the
// verification logic.
type BundleSignature struct {
	SignatureB64 string
	KeyID        string
	SignedAt     time.Time
}

// Installation is the tenant-scoped per-install record. RLS isolates
// rows per tenant; the Store's Install*/List*ForTenant methods all
// run under dbutil.WithTenantTx so the GUC is set.
type Installation struct {
	ID                    uuid.UUID     `json:"id"`
	TenantID              uuid.UUID     `json:"tenant_id"`
	ExtensionID           uuid.UUID     `json:"extension_id"`
	ExtensionVersionID    uuid.UUID     `json:"extension_version_id"`
	Status                InstallStatus `json:"status"`
	Settings              []byte        `json:"-"` // raw JSONB (validated against the version's settings_schema)
	WebhookBase           string        `json:"webhook_base"`
	InstalledBy           *uuid.UUID    `json:"installed_by,omitempty"`
	InstalledAt           time.Time     `json:"installed_at"`
	UpdatedAt             time.Time     `json:"updated_at"`
	LastHealthCheckAt     *time.Time    `json:"last_health_check_at,omitempty"`
	LastHealthCheckStatus string        `json:"last_health_check_status,omitempty"`
	FailureReason         string        `json:"failure_reason,omitempty"`
}

// Sentinel errors returned by Store / manifest parser. Callers (B6
// API handlers) translate these into HTTP statuses:
//
//	ErrConflict             → 409
//	ErrNotFound             → 404
//	ErrInvalidManifest      → 400 (with the wrapped detail)
//	ErrBundleTooLarge       → 413
//	ErrPermissionScopeUnknown → 400
//	ErrImmutableVersion     → 409
//	ErrYanked               → 409
//	ErrForbidden            → 403
//	ErrLastOwnerRemoval     → 409
var (
	// ErrConflict signals a unique-constraint hit — either a
	// duplicate (publisher, slug) extension insert or a duplicate
	// (extension_id, version) version publish.
	ErrConflict = errors.New("marketplace: row already exists")

	// ErrNotFound is the catch-all "no row" sentinel for Get*
	// lookups.
	ErrNotFound = errors.New("marketplace: not found")

	// ErrInvalidManifest wraps any manifest validation failure
	// (missing required field, malformed name, count limit exceeded,
	// disallowed placeholder, etc.). Callers may type-assert to
	// *ManifestError to extract the per-field detail.
	ErrInvalidManifest = errors.New("marketplace: invalid manifest")

	// ErrBundleTooLarge means the bundle size exceeded the 10 MiB
	// hard cap defined in EXTENSION_SPEC §2. Returned both by the
	// manifest layer (when the size is supplied at parse time) and
	// by the DB CHECK constraint at INSERT.
	ErrBundleTooLarge = errors.New("marketplace: bundle exceeds 10 MiB hard limit")

	// ErrPermissionScopeUnknown means a permission named in
	// permissions_required[] is not one of the known platform
	// permission scopes. The list of valid scopes lives in
	// manifest.go.
	ErrPermissionScopeUnknown = errors.New("marketplace: permission scope not recognised")

	// ErrImmutableVersion is returned when an UPDATE attempts to
	// mutate a write-once column on marketplace_extension_versions.
	// The DB trigger raises pgerror P0001 with a specific message; the
	// repository translates that into this sentinel.
	ErrImmutableVersion = errors.New("marketplace: version row is immutable; publish a new version instead")

	// ErrYanked is returned when an operation requires a
	// non-yanked version but the target version is yanked. Today
	// it is raised by SetListedVersion (a yanked version cannot
	// be the listed/recommended one because B6's install endpoint
	// refuses to install yanked versions, spec §10). B6 will
	// surface this as 409 Conflict so the operator UI can show a
	// clear "version is yanked" message rather than a generic
	// "invalid transition" error.
	ErrYanked = errors.New("marketplace: version is yanked")

	// ErrInvalidSignature is the catch-all sentinel for a registered
	// ed25519 key failing to verify a bundle's signature. B7's
	// automated review pipeline raises this both at the structured-
	// finding layer (severity=error, code=signature.invalid) and at
	// the publisher submit endpoint when the early-reject path runs.
	ErrInvalidSignature = errors.New("marketplace: invalid bundle signature")

	// ErrPublisherNotVerified is returned by admin endpoints that
	// require the auto-approve-patch flag (a B7.1 feature) when the
	// publisher has not yet been operator-verified. Today the
	// pipeline does not depend on it, but the verify/unverify
	// admin endpoints surface this for the patch fast-path.
	ErrPublisherNotVerified = errors.New("marketplace: publisher is not verified")

	// ErrClaimLost is the B7 review pipeline's signal that an
	// admin Rescan landed between a worker's claim and its Persist
	// call. The atomic claim guard on UpdateReviewState refuses
	// the transition; the worker logs + drops the result and the
	// next poll re-claims the freshly-reset row to re-run the
	// pipeline against the same version. See
	// services/worker/review_worker.go and Pipeline.Persist for
	// the full TOCTOU rationale.
	ErrClaimLost = errors.New("marketplace: review claim lost (concurrent rescan)")

	// ErrForbidden is returned by the B7.1 self-service publisher
	// surface when the authenticated user is not a member of the
	// publisher (or holds a role insufficient for the requested
	// action — e.g. a `member` trying to invoke an owner-only
	// member-management endpoint). Mapped to 403 by writeError.
	// Distinct from ErrNotFound (404) so the engine can tell a
	// non-member apart from a missing publisher; the HTTP layer
	// can choose to collapse both into 404 to avoid leaking
	// publisher existence to outsiders.
	ErrForbidden = errors.New("marketplace: forbidden")

	// ErrLastOwnerRemoval is the invariant guard on the
	// publisher_members surface: "any publisher with members must
	// have ≥1 owner." Returned by SetMemberRole(owner→member) and
	// RemoveMember(last owner) when other (non-owner) members
	// would remain after the change. Admin override endpoints
	// bypass this guard explicitly. Mapped to 409 by writeError —
	// the request itself was authorised but the desired state
	// transition would violate the invariant.
	ErrLastOwnerRemoval = errors.New("marketplace: removing this owner would leave the publisher without an owner")

	// ErrReviewMaxAttemptsExceeded is the dead-letter signal
	// returned by ReviewStateStore.RecordAttemptFailure when the
	// incremented attempt_count would meet or exceed
	// MaxReviewAttempts. The worker catches this sentinel and
	// transitions the row to ReviewStatusDeadLetter with a
	// synthetic finding row recording the final failure. Mapped to
	// 409 by writeError so admin endpoints that try to transition
	// a dead-lettered row get a clear conflict.
	ErrReviewMaxAttemptsExceeded = errors.New("marketplace: review max attempts exceeded")
)

// Publisher is the publisher identity row. Backfilled at migration
// time from the distinct publisher column on marketplace_extensions.
// The verified_at + verified_by columns are the audit trail for the
// operator's verification decision; auto_approve_patch is the gate
// for the future fast-path (B7.1) that lets verified publishers'
// patch-version bumps skip the manual_review step.
type Publisher struct {
	ID                 uuid.UUID  `json:"id"`
	Slug               string     `json:"slug"`
	DisplayName        string     `json:"display_name"`
	ContactEmail       string     `json:"contact_email"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	VerifiedBy         string     `json:"verified_by,omitempty"`
	VerificationNotes  string     `json:"verification_notes,omitempty"`
	AutoApprovePatch   bool       `json:"auto_approve_patch"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// PublisherMemberRole is the role of a user in the publisher
// membership table. Two-role RBAC: owner manages members + keys,
// member manages keys only. See migrations/000075 for the
// rationale on why we stopped at two roles.
type PublisherMemberRole string

const (
	// PublisherMemberRoleOwner can add/remove other members and
	// can manage keys. There must be ≥1 owner whenever a
	// publisher has any members.
	PublisherMemberRoleOwner PublisherMemberRole = "owner"

	// PublisherMemberRoleMember can list members and manage keys
	// (register / list / revoke) but cannot add or remove other
	// members. Used to delegate key-rotation duty without
	// granting full publisher control.
	PublisherMemberRoleMember PublisherMemberRole = "member"
)

// Valid reports whether r is one of the recognised roles. Used
// by the handler-side input validator before passing to the
// store.
func (r PublisherMemberRole) Valid() bool {
	switch r {
	case PublisherMemberRoleOwner, PublisherMemberRoleMember:
		return true
	}
	return false
}

// AtLeast returns true if r is at least as privileged as other.
// owner > member. Used by RequireMemberRole to gate owner-only
// endpoints.
func (r PublisherMemberRole) AtLeast(other PublisherMemberRole) bool {
	rank := func(x PublisherMemberRole) int {
		switch x {
		case PublisherMemberRoleOwner:
			return 2
		case PublisherMemberRoleMember:
			return 1
		}
		return 0
	}
	return rank(r) >= rank(other)
}

// PublisherMember is one (publisher, user, role) row. AddedBy
// is the user_id of whoever added this member (nil for
// admin-added rows since the admin acts on behalf of the
// platform, not as a publisher member themselves). UserEmail
// and UserDisplayName are populated by ListMembers / GetMember
// via a JOIN with the users table so the API surface doesn't
// need a second round-trip to render member rows; they are zero
// values for store methods that do not need them (e.g.
// RequireMemberRole, which only needs the role).
type PublisherMember struct {
	PublisherID     uuid.UUID           `json:"publisher_id"`
	UserID          uuid.UUID           `json:"user_id"`
	Role            PublisherMemberRole `json:"role"`
	AddedBy         *uuid.UUID          `json:"added_by,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	UserEmail       string              `json:"user_email,omitempty"`
	UserDisplayName string              `json:"user_display_name,omitempty"`
}

// PublisherWithMembership pairs a Publisher row with the role of
// the authenticated caller. Used by the "list publishers I'm a
// member of" endpoint so the UI can render a per-row "You are an
// owner / member" badge without a second round-trip.
type PublisherWithMembership struct {
	Publisher Publisher           `json:"publisher"`
	Role      PublisherMemberRole `json:"role"`
}

// PublisherKey is one ed25519 public key registered by a publisher.
// Multiple keys per publisher supports rotation: register the new
// key, sign new uploads with it, then revoke the old key. The
// pipeline considers any non-revoked key a valid signer; revoked
// keys remain in the table so we can still verify signatures on
// already-uploaded immutable version rows.
type PublisherKey struct {
	ID            uuid.UUID  `json:"id"`
	PublisherID   uuid.UUID  `json:"publisher_id"`
	KeyID         string     `json:"key_id"`
	Algorithm     string     `json:"algorithm"`
	PublicKeyB64  string     `json:"public_key_b64"`
	Label         string     `json:"label,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Severity tags structured findings. The pipeline interprets the
// severity as follows:
//
//	error — blocks the version; pipeline transitions to rejected.
//	warn  — surfaces on the listing detail page as advisory output;
//	        does NOT block.
//	info  — recorded for forensic detail but not surfaced in the UI;
//	        used e.g. by the SignatureCheck to note "publisher
//	        unsigned" without flagging.
type Severity string

// The three severity tiers the pipeline emits — see the Severity
// godoc for what each one signals to the marketplace UI and the
// review state machine.
const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warn"
	SeverityInfo  Severity = "info"
)

// Valid reports whether s is one of the three defined severities.
func (s Severity) Valid() bool {
	switch s {
	case SeverityError, SeverityWarn, SeverityInfo:
		return true
	}
	return false
}

// ReviewFinding is one structured output of an automated check. The
// natural key is (extension_version_id, check_name, code, location)
// so a re-scan replaces rather than duplicates findings.
type ReviewFinding struct {
	ID                 uuid.UUID `json:"id"`
	ExtensionVersionID uuid.UUID `json:"extension_version_id"`
	CheckName          string    `json:"check_name"`
	Severity           Severity  `json:"severity"`
	Code               string    `json:"code"`
	Message            string    `json:"message"`
	Location           string    `json:"location,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}
