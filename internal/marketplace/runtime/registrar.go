package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// ResolvedBundle is the install-time payload that B3 registers into
// the marketplace_extension_* tables. The fields here are the
// post-extraction shape: each KType / workflow / tool descriptor
// has been resolved from a bundle-relative file path into a
// concrete name + JSON body. The caller (B6's install API handler,
// or a test) is responsible for performing that resolution before
// invoking Engine.Install. This keeps B3 decoupled from the bundle
// archive format (zip/tar layout, manifest path-vs-name mapping)
// which is owned by the bundle extractor.
//
// The caller MUST guarantee that:
//
//   - Every Name field is already validated against the spec's
//     namespace regex (ext.<publisher>.<slug> for KTypes /
//     workflows / agent_tools).
//   - Every Schema / Definition / Descriptor is valid JSON
//     parseable by encoding/json.
//   - The slice ordering matches the manifest's slice ordering
//     (used only for error-attribution; the registrar uses Name
//     as the primary key, not slice index).
//
// The registrar re-asserts these invariants at insert time via
// the DB CHECK constraints declared in migrations/000069, so a
// bug in a caller cannot poison the registry.
type ResolvedBundle struct {
	// Manifest is the parsed manifest, retained as the source of
	// truth for retry policy / timeout / endpoint extraction on
	// agent_tools and webhook subscriptions. Required.
	Manifest *marketplace.Manifest

	// KTypes is one entry per manifest.ktypes[i]. Length and
	// ordering match Manifest.KTypes; the registrar uses Name +
	// SchemaJSON for the INSERT (the manifest's path is not
	// persisted — the resolved name is the identity).
	KTypes []ResolvedKType

	// Workflows is one entry per manifest.workflows[i].
	Workflows []ResolvedWorkflow

	// AgentTools is one entry per manifest.agent_tools[i].
	// AgentToolRef.{Endpoint, Timeout, Retry} are NOT copied
	// here — the registrar reads them from Manifest.AgentTools
	// at the same index so a stale or mismatched ResolvedAgentTool
	// can't cause endpoint/timeout drift between manifest and
	// runtime table. The registrar materialises those columns
	// from Manifest, validates them inside the tx, and rejects
	// the install if they fail.
	AgentTools []ResolvedAgentTool
}

// ResolvedKType is the install-time form of a manifest KType
// reference. The name and JSON schema body have been extracted
// from the bundle archive (B6's job) so the registrar can do a
// single INSERT.
type ResolvedKType struct {
	// Name is the canonical KType name (`ext.<publisher>.<slug>`).
	// Required.
	Name string
	// Version is the KType schema-document version. Defaults to
	// 1 for v1 bundles.
	Version int
	// SchemaJSON is the JSON Schema document body. Required.
	SchemaJSON json.RawMessage
}

// ResolvedWorkflow is the install-time form of a manifest workflow
// reference.
type ResolvedWorkflow struct {
	// Name is the canonical workflow name. Per spec §5 workflows
	// are namespaced by manifest publisher; the bundle extractor
	// derives the name from the bundle file body.
	Name string
	// Version mirrors ResolvedKType.Version.
	Version int
	// DefinitionJSON is the workflow definition (state-machine
	// graph) as a JSON document.
	DefinitionJSON json.RawMessage
}

// ResolvedAgentTool is the install-time form of a manifest agent-
// tool reference. The bundle archive holds a JSON descriptor file
// next to the manifest; the bundle extractor returns it here so
// the registrar can persist both the structured columns (from the
// manifest) AND the raw descriptor (for B4 introspection).
type ResolvedAgentTool struct {
	// Name is the canonical tool name. The bundle's descriptor
	// file declares the name; the bundle extractor surfaces it.
	Name string
	// DescriptorJSON is the raw tool descriptor body. Stored
	// verbatim in marketplace_extension_agent_tools.descriptor.
	DescriptorJSON json.RawMessage
}

// Registrar performs the atomic install-time registration of a
// resolved bundle into the marketplace_extension_* tables. The
// Engine wires a single transaction around pre_install + registrar
// so a hook failure rolls back every INSERT.
//
// Registrar deliberately does NOT own the pool; the Engine passes
// the tx in so the engine's pre_install + post_install lifecycle
// hooks see the same write-context. This also means Registrar is
// nil-safe at the package level — a test can construct one without
// a database for unit-shape verification.
type Registrar struct {
	// Now returns the registration timestamp. Tests override to
	// pin a deterministic now; production defaults to time.Now.
	Now func() time.Time
}

// NewRegistrar returns a Registrar with time.Now as its clock.
func NewRegistrar() *Registrar {
	return &Registrar{Now: time.Now}
}

// nowOrDefault returns r.Now() falling back to time.Now if Now is
// nil. Callers should not construct Registrar with a nil Now in
// practice; this is purely defensive.
func (r *Registrar) nowOrDefault() time.Time {
	if r == nil || r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// RegisterAll inserts every KType / workflow / agent-tool / webhook
// subscription declared by the resolved bundle into the runtime
// tables, all in a single tenant-scoped tx. Returns ErrConflict
// (wrapping marketplace.ErrConflict) on any duplicate-key collision
// against an already-registered name within the same installation.
//
// The transaction MUST already have app.tenant_id set
// (dbutil.SetTenantContext or dbutil.WithTenantTx have already
// run). The Engine wires this in; tests that call Registrar
// directly are responsible for the same.
//
// Insert order:
//
//  1. KTypes — must land before workflows / posting hooks which
//     reference them.
//  2. Workflows.
//  3. Agent tools.
//  4. Webhook subscriptions.
//
// Each call performs full-bundle re-registration: the caller is
// expected to wipe prior rows for the installation before invoking
// RegisterAll if the intent is "replace" (e.g., a version upgrade).
// For B3 the only call site is fresh-install; B6 upgrades / B7
// version changes are out of scope.
func (r *Registrar) RegisterAll(ctx context.Context, tx pgx.Tx, tenantID, installationID uuid.UUID, webhookBase string, bundle *ResolvedBundle) error {
	if tx == nil {
		return errors.New("runtime: registrar: nil tx")
	}
	if tenantID == uuid.Nil {
		return errors.New("runtime: registrar: tenant_id required")
	}
	if installationID == uuid.Nil {
		return errors.New("runtime: registrar: installation_id required")
	}
	if bundle == nil || bundle.Manifest == nil {
		return errors.New("runtime: registrar: nil bundle/manifest")
	}
	webhookBase = strings.TrimRight(strings.TrimSpace(webhookBase), "/")
	if webhookBase == "" {
		return errors.New("runtime: registrar: webhook_base required")
	}
	if !strings.HasPrefix(webhookBase, "https://") {
		return fmt.Errorf("runtime: registrar: webhook_base %q must use https://", webhookBase)
	}
	if got, want := len(bundle.KTypes), len(bundle.Manifest.KTypes); got != want {
		return fmt.Errorf("runtime: registrar: resolved KTypes count %d != manifest count %d", got, want)
	}
	if got, want := len(bundle.Workflows), len(bundle.Manifest.Workflows); got != want {
		return fmt.Errorf("runtime: registrar: resolved Workflows count %d != manifest count %d", got, want)
	}
	if got, want := len(bundle.AgentTools), len(bundle.Manifest.AgentTools); got != want {
		return fmt.Errorf("runtime: registrar: resolved AgentTools count %d != manifest count %d", got, want)
	}

	publisher := bundle.Manifest.Publisher

	if err := r.insertKTypes(ctx, tx, tenantID, installationID, publisher, bundle.KTypes); err != nil {
		return err
	}
	if err := r.insertWorkflows(ctx, tx, tenantID, installationID, bundle.Workflows); err != nil {
		return err
	}
	if err := r.insertAgentTools(ctx, tx, tenantID, installationID, webhookBase, bundle.AgentTools, bundle.Manifest.AgentTools); err != nil {
		return err
	}
	if err := r.insertWebhookSubscriptions(ctx, tx, tenantID, installationID, webhookBase, bundle.Manifest.WebhooksConsumed); err != nil {
		return err
	}
	return nil
}

// resolveEndpoint substitutes the install's webhook_base for the
// ${EXTENSION_WEBHOOK_BASE} placeholder declared in manifest
// endpoints. The manifest validator (B2) restricts endpoints to the
// `${EXTENSION_WEBHOOK_BASE}(/path)?` shape, so the only allowed
// outputs of this function are `<base>` or `<base>/<path>`. After
// resolution the result MUST be an https:// URL — that's how the
// DB CHECK constraint on agent_tools.endpoint /
// webhook_subscriptions.endpoint gates malformed installs.
func resolveEndpoint(endpoint, webhookBase string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errors.New("empty endpoint")
	}
	const placeholder = "${EXTENSION_WEBHOOK_BASE}"
	if !strings.HasPrefix(endpoint, placeholder) {
		return "", fmt.Errorf("endpoint %q must start with %s", endpoint, placeholder)
	}
	suffix := endpoint[len(placeholder):]
	resolved := webhookBase + suffix
	if !strings.HasPrefix(resolved, "https://") {
		return "", fmt.Errorf("resolved endpoint %q must use https://", resolved)
	}
	return resolved, nil
}

func (r *Registrar) insertKTypes(ctx context.Context, tx pgx.Tx, tenantID, installID uuid.UUID, publisher string, kts []ResolvedKType) error {
	if len(kts) == 0 {
		return nil
	}
	now := r.nowOrDefault()
	for i, kt := range kts {
		if err := marketplace.ValidateKTypeName(kt.Name, publisher); err != nil {
			return fmt.Errorf("runtime: registrar: ktypes[%d]: %w", i, err)
		}
		if len(kt.SchemaJSON) == 0 {
			return fmt.Errorf("runtime: registrar: ktypes[%d]: missing schema body", i)
		}
		if !json.Valid(kt.SchemaJSON) {
			return fmt.Errorf("runtime: registrar: ktypes[%d]: schema body is not valid JSON", i)
		}
		ver := kt.Version
		if ver == 0 {
			ver = 1
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO marketplace_extension_ktypes
				(tenant_id, installation_id, ktype_name, ktype_version, schema, registered_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, installID, kt.Name, ver, kt.SchemaJSON, now)
		if err != nil {
			if isPGUniqueViolation(err) {
				return fmt.Errorf("%w: ktype %q already registered for installation %s", marketplace.ErrConflict, kt.Name, installID)
			}
			return fmt.Errorf("runtime: registrar: insert ktype %q: %w", kt.Name, err)
		}
	}
	return nil
}

func (r *Registrar) insertWorkflows(ctx context.Context, tx pgx.Tx, tenantID, installID uuid.UUID, wfs []ResolvedWorkflow) error {
	if len(wfs) == 0 {
		return nil
	}
	now := r.nowOrDefault()
	for i, wf := range wfs {
		if strings.TrimSpace(wf.Name) == "" {
			return fmt.Errorf("runtime: registrar: workflows[%d]: missing name", i)
		}
		if len(wf.DefinitionJSON) == 0 {
			return fmt.Errorf("runtime: registrar: workflows[%d]: missing definition body", i)
		}
		if !json.Valid(wf.DefinitionJSON) {
			return fmt.Errorf("runtime: registrar: workflows[%d]: definition body is not valid JSON", i)
		}
		ver := wf.Version
		if ver == 0 {
			ver = 1
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO marketplace_extension_workflows
				(tenant_id, installation_id, workflow_name, workflow_version, definition, registered_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, installID, wf.Name, ver, wf.DefinitionJSON, now)
		if err != nil {
			if isPGUniqueViolation(err) {
				return fmt.Errorf("%w: workflow %q already registered for installation %s", marketplace.ErrConflict, wf.Name, installID)
			}
			return fmt.Errorf("runtime: registrar: insert workflow %q: %w", wf.Name, err)
		}
	}
	return nil
}

func (r *Registrar) insertAgentTools(ctx context.Context, tx pgx.Tx, tenantID, installID uuid.UUID, webhookBase string, resolved []ResolvedAgentTool, refs []marketplace.AgentToolRef) error {
	if len(refs) == 0 {
		return nil
	}
	now := r.nowOrDefault()
	for i, ref := range refs {
		res := resolved[i]
		if strings.TrimSpace(res.Name) == "" {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: missing name", i)
		}
		if len(res.DescriptorJSON) == 0 {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: missing descriptor body", i)
		}
		if !json.Valid(res.DescriptorJSON) {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: descriptor body is not valid JSON", i)
		}
		if ref.Handler != "webhook" {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: handler %q not supported (only 'webhook' in B3)", i, ref.Handler)
		}
		timeout, err := time.ParseDuration(ref.Timeout)
		if err != nil {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: parse timeout %q: %w", i, ref.Timeout, err)
		}
		if timeout <= 0 {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: non-positive timeout", i)
		}
		timeoutMs := int(timeout / time.Millisecond)

		resolvedEndpoint, resolveErr := resolveEndpoint(ref.Endpoint, webhookBase)
		if resolveErr != nil {
			return fmt.Errorf("runtime: registrar: agent_tools[%d]: %w", i, resolveErr)
		}

		// RetryRule defaults are already enforced by the manifest
		// validator; if a manifest reached here with a nil retry
		// block we treat that as "exponential x 2 attempts" which
		// matches the spec default. The validator MUST default the
		// Retry pointer before this point — this fallback is purely
		// defensive against the test path that constructs a manifest
		// in code without round-tripping through ParseManifest.
		maxAttempts := 2
		backoff := "exponential"
		if ref.Retry != nil {
			maxAttempts = ref.Retry.MaxAttempts
			backoff = ref.Retry.Backoff
			if backoff == "" {
				backoff = "exponential"
			}
		}

		_, execErr := tx.Exec(ctx, `
			INSERT INTO marketplace_extension_agent_tools
				(tenant_id, installation_id, tool_name, descriptor,
				 handler, endpoint, timeout_ms,
				 retry_max_attempts, retry_backoff, registered_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			tenantID, installID, res.Name, res.DescriptorJSON,
			ref.Handler, resolvedEndpoint, timeoutMs,
			maxAttempts, backoff, now)
		if execErr != nil {
			if isPGUniqueViolation(execErr) {
				return fmt.Errorf("%w: agent_tool %q already registered for installation %s", marketplace.ErrConflict, res.Name, installID)
			}
			return fmt.Errorf("runtime: registrar: insert agent_tool %q: %w", res.Name, execErr)
		}
	}
	return nil
}

func (r *Registrar) insertWebhookSubscriptions(ctx context.Context, tx pgx.Tx, tenantID, installID uuid.UUID, webhookBase string, subs []marketplace.WebhookRef) error {
	if len(subs) == 0 {
		return nil
	}
	now := r.nowOrDefault()
	for i, sub := range subs {
		if strings.TrimSpace(sub.Event) == "" {
			return fmt.Errorf("runtime: registrar: webhooks_consumed[%d]: missing event", i)
		}
		if strings.TrimSpace(sub.Endpoint) == "" {
			return fmt.Errorf("runtime: registrar: webhooks_consumed[%d]: missing endpoint", i)
		}
		resolvedEndpoint, resolveErr := resolveEndpoint(sub.Endpoint, webhookBase)
		if resolveErr != nil {
			return fmt.Errorf("runtime: registrar: webhooks_consumed[%d]: %w", i, resolveErr)
		}
		filterJSON := []byte(`{}`)
		if len(sub.Filter) > 0 {
			b, err := json.Marshal(sub.Filter)
			if err != nil {
				return fmt.Errorf("runtime: registrar: webhooks_consumed[%d]: marshal filter: %w", i, err)
			}
			filterJSON = b
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO marketplace_webhook_subscriptions
				(tenant_id, installation_id, event, filter, endpoint, registered_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, installID, sub.Event, filterJSON, resolvedEndpoint, now)
		if err != nil {
			return fmt.Errorf("runtime: registrar: insert webhook_sub (event=%q): %w", sub.Event, err)
		}
	}
	return nil
}

// UnregisterAll deletes every row from the runtime tables for the
// (tenant, installation) pair. Called from Engine.Uninstall *before*
// the installations row itself is updated to 'uninstalled' — the
// CASCADE on FK would handle the deletes if the install row were
// deleted, but we keep the install row and flip status instead so
// the audit history (installed_by / installed_at) survives.
//
// dispatch_log is NOT touched — it's append-only and survives via
// ON DELETE SET NULL on installation_id (configured in the
// migration's FK).
func (r *Registrar) UnregisterAll(ctx context.Context, tx pgx.Tx, tenantID, installID uuid.UUID) error {
	if tx == nil {
		return errors.New("runtime: registrar: nil tx")
	}
	if tenantID == uuid.Nil {
		return errors.New("runtime: registrar: tenant_id required")
	}
	if installID == uuid.Nil {
		return errors.New("runtime: registrar: installation_id required")
	}
	tables := []string{
		"marketplace_webhook_subscriptions",
		"marketplace_extension_agent_tools",
		"marketplace_extension_workflows",
		"marketplace_extension_ktypes",
	}
	for _, t := range tables {
		// RLS guards tenant_id but we include it explicitly so a
		// bug that misconfigures app.tenant_id can't sweep another
		// tenant's rows (belt + suspenders).
		_, err := tx.Exec(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE tenant_id = $1 AND installation_id = $2", t),
			tenantID, installID)
		if err != nil {
			return fmt.Errorf("runtime: registrar: delete from %s: %w", t, err)
		}
	}
	return nil
}

// RegisterAllInTx is a convenience wrapper that opens a tenant-
// scoped tx via dbutil.WithTenantTx and runs RegisterAll inside it.
// Used by tests that need the full registrar path without manually
// staging a tx. Production callers use the engine's own
// transaction.
func (r *Registrar) RegisterAllInTx(ctx context.Context, pool *pgxpool.Pool, tenantID, installationID uuid.UUID, webhookBase string, bundle *ResolvedBundle) error {
	return dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return r.RegisterAll(ctx, tx, tenantID, installationID, webhookBase, bundle)
	})
}

// isPGUniqueViolation reports whether err is a Postgres
// unique-constraint violation (SQLSTATE 23505). Used by the
// registrar to translate raw INSERT errors into the marketplace
// ErrConflict sentinel.
func isPGUniqueViolation(err error) bool {
	type pgErr interface {
		SQLState() string
	}
	var pe pgErr
	if errors.As(err, &pe) {
		return pe.SQLState() == "23505"
	}
	return false
}
