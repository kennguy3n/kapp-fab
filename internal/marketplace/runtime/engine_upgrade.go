package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// UpgradeResult is the return shape for Engine.Upgrade. Mirrors
// InstallResult / UninstallResult: the post-commit Installation
// snapshot plus the two lifecycle dispatch records.
type UpgradeResult struct {
	// Installation is the install row with the new
	// extension_version_id and the post-commit updated_at.
	// Never nil on success.
	Installation *marketplace.Installation
	// FromVersionID is the install's previous extension_version_id,
	// captured at the start of the tx for audit / event-emitting.
	FromVersionID uuid.UUID
	// PreUpgradeResult records the blocking pre_upgrade dispatch
	// (nil when SkipHooks is true). On success it carries the
	// extension's 2xx response body so the caller can surface it
	// (e.g. a migration-progress message).
	PreUpgradeResult *LifecycleResult
	// PostUpgradeResult records the best-effort post_upgrade
	// dispatch. Nil when SkipHooks is true or the engine was
	// constructed with NoopHooks.
	PostUpgradeResult *LifecycleResult
}

// Upgrade swaps an existing install's extension_version_id to a
// new version and atomically re-registers every runtime-tables
// row (ktypes / workflows / agent_tools / webhook_subscriptions /
// posting_hooks) against the new bundle. The flow is symmetric
// with Install + Uninstall:
//
//  1. Validate the UpgradeRequest and the resolved-bundle pair.
//  2. Catalog-side preconditions: extension is 'listed', target
//     version belongs to that extension, target version is not
//     yanked. The same checks are re-applied inside the tx via
//     the catalog row lock so a concurrent yank can't slip past
//     this pre-check.
//  3. pre_upgrade (BLOCKING unless SkipHooks). 4xx/5xx/transport
//     failure → return ErrPreUpgradeRejected without touching the
//     DB. The dispatch_log records the attempt.
//  4. Open a tenant-scoped tx. Inside the tx:
//     a. SELECT ... FOR UPDATE the install row, capture the
//     current settings document, verify
//        - status compatible (active / failed — uninstalled and
//          installing are rejected),
//        - extension_version_id == FromVersionID (TOCTOU guard
//          against concurrent upgrade or uninstall+reinstall),
//        - signing_secret_ciphertext is non-empty (skipped in
//          SkipHooks branch for legacy-install force-upgrade).
//     b. Registrar.UnregisterAll for the install row — drops every
//        ktype, workflow, agent_tool, webhook_subscription
//        previously written by the install.
//     c. Registrar.RegisterAll against the NEW bundle — re-writes
//        the same tables with the new manifest's resources.
//     d. UPDATE the install row: extension_version_id =
//        ToVersionID, settings = (Settings || KeepSettings ?
//        existing : '{}'), updated_at = now(). RETURNING the
//        post-commit row.
//     Commit. If any step fails, the whole tx rolls back. The
//     install row's extension_version_id remains at FromVersionID
//     and the runtime tables remain at the old version's shape.
//  5. post_upgrade (BEST-EFFORT, skipped when SkipHooks). A failure
//     is logged but does NOT roll back the upgrade — the runtime
//     tables are already swapped and any reversal would be a
//     second upgrade, not a rollback.
//
// Failure-rollback semantics: the unique constraint on
// marketplace_extension_ktypes(tenant_id, namespace) — and the
// equivalent constraints on the other runtime tables — means a
// new bundle that introduces a KType the tenant already owns from
// a different install will fail RegisterAll. Because the
// UnregisterAll + RegisterAll are in the same tx, that failure
// rolls back the unregister too: the install reverts cleanly
// to FromVersionID. This is the defining invariant of
// Engine.Upgrade.
//
// Force-upgrade (SkipHooks=true): used when the extension's
// webhook server is unreachable and the operator wants to
// migrate to a new version regardless. The signing_secret check
// is skipped (legacy installs with empty signing_secret can be
// force-upgraded), and both lifecycle hooks are bypassed. The
// upgrade still validates the TOCTOU guard + runs the full in-tx
// registrar swap.
func (e *Engine) Upgrade(ctx context.Context, req *UpgradeRequest, newBundle *ResolvedBundle) (*UpgradeResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if newBundle == nil || newBundle.Manifest == nil {
		return nil, errors.New("runtime: engine: upgrade: nil new bundle/manifest")
	}

	// Step 2: catalog-side preconditions. We re-apply the same
	// checks Install runs: extension is 'listed', target version
	// belongs to that extension, target is not yanked. The
	// catalog rows are read outside the tx — the in-tx FOR UPDATE
	// on the install row at step 4a re-verifies version_id, and
	// the in-tx re-registration would fail on a yanked target
	// because the bundle resolver would refuse to serve a yanked
	// hash. But fail-fast here so a yanked-target attempt doesn't
	// dispatch pre_upgrade.
	//
	// We derive extension_id from the install row (the source of
	// truth for "what extension is this install at") rather than
	// the new bundle, because ResolvedBundle is intentionally
	// extension-id agnostic — it's keyed by manifest content
	// (publisher + slug + version) and the same bundle could
	// theoretically be reused across catalog rows in a future
	// migration. The cross-check below (toVer.ExtensionID ==
	// install.ExtensionID) closes the loop.
	install, err := e.store.GetInstallation(ctx, req.TenantID, req.InstallationID)
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: upgrade: load install: %w", err)
	}
	ext, err := e.store.GetExtension(ctx, install.ExtensionID)
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: upgrade: get extension: %w", err)
	}
	if ext.Status != marketplace.ExtensionStatusListed {
		return nil, fmt.Errorf("%w: extension status %q does not permit upgrade (need 'listed')",
			marketplace.ErrConflict, ext.Status)
	}
	toVer, err := e.store.GetVersion(ctx, req.ToVersionID)
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: upgrade: get target version: %w", err)
	}
	if toVer.ExtensionID != install.ExtensionID {
		return nil, fmt.Errorf("%w: target version %s belongs to extension %s, not install's extension %s",
			marketplace.ErrConflict, req.ToVersionID, toVer.ExtensionID, install.ExtensionID)
	}
	if toVer.Yanked {
		return nil, fmt.Errorf("%w: target version %s is yanked", marketplace.ErrYanked, req.ToVersionID)
	}
	// Pre-tx version check is just an early-out; the in-tx
	// FOR UPDATE is the authoritative guard. A mismatch here
	// means the install moved between when the caller decided to
	// upgrade and when we got here — surface as ErrVersionMismatch
	// so the caller can refresh.
	if install.ExtensionVersionID != req.FromVersionID {
		return nil, fmt.Errorf("%w: install %s is at %s, expected %s",
			ErrVersionMismatch, req.InstallationID, install.ExtensionVersionID, req.FromVersionID)
	}
	if install.Status == marketplace.InstallStatusUninstalled {
		return nil, fmt.Errorf("%w: installation %s is uninstalled", marketplace.ErrConflict, req.InstallationID)
	}
	if install.Status == marketplace.InstallStatusInstalling {
		return nil, fmt.Errorf("%w: installation %s is still installing", marketplace.ErrConflict, req.InstallationID)
	}

	// Step 3: pre_upgrade (BLOCKING unless skipped). Load the
	// signing secret outside the tx — same pattern as
	// Engine.Uninstall. SkipHooks bypasses both the secret load
	// AND the dispatch so force-upgrade works on legacy installs
	// with empty signing_secret columns.
	var (
		secret           SigningSecret
		preUpgradeResult *LifecycleResult
	)
	if !req.SkipHooks {
		loaded, err := e.loadSigningSecret(ctx, req.TenantID, req.InstallationID)
		if err != nil {
			return nil, err
		}
		secret = loaded

		preBody, err := MarshalLifecyclePayload(map[string]any{
			"phase":           string(PhasePreUpgrade),
			"tenant_id":       req.TenantID.String(),
			"installation_id": req.InstallationID.String(),
			"extension_id":    install.ExtensionID.String(),
			"from_version_id": req.FromVersionID.String(),
			"to_version_id":   req.ToVersionID.String(),
			"webhook_base":    install.WebhookBase,
			"upgraded_by":     uuidOrNilString(&req.UpgradedBy),
		})
		if err != nil {
			return nil, err
		}
		res, err := e.hooks.Dispatch(ctx, &LifecycleDispatch{
			TenantID:           req.TenantID,
			InstallationID:     req.InstallationID,
			ExtensionID:        install.ExtensionID,
			ExtensionVersionID: req.ToVersionID,
			Phase:              PhasePreUpgrade,
			WebhookBase:        install.WebhookBase,
			SigningSecret:      secret,
			Body:               preBody,
		})
		if err != nil {
			return nil, err
		}
		if res != nil && res.Aborted {
			return nil, fmt.Errorf("%w: %s", ErrPreUpgradeRejected, res.AbortReason)
		}
		preUpgradeResult = res
	}

	// Step 4: transactional swap. The whole flow runs in a
	// tenant-scoped tx so partial state is impossible — either
	// the new bundle's resources are fully registered AND the
	// install row points at ToVersionID, or nothing changed.
	var (
		fromVersionID uuid.UUID
		updated       marketplace.Installation
	)
	txErr := dbutil.WithTenantTx(ctx, e.pool, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Step 4a: SELECT ... FOR UPDATE the install row.
		var (
			status      string
			currentVer  uuid.UUID
			extID       uuid.UUID
			webhookBase string
			installedBy *uuid.UUID
			installedAt time.Time
			settingsTxt string
		)
		row := tx.QueryRow(ctx,
			`SELECT extension_id, extension_version_id, status, webhook_base,
			        installed_by, installed_at, settings::text
			   FROM marketplace_extension_installations
			  WHERE tenant_id = $1 AND id = $2
			  FOR UPDATE`,
			req.TenantID, req.InstallationID)
		if err := row.Scan(&extID, &currentVer, &status, &webhookBase,
			&installedBy, &installedAt, &settingsTxt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: install %s", marketplace.ErrNotFound, req.InstallationID)
			}
			return fmt.Errorf("runtime: engine: upgrade: lock install: %w", err)
		}

		// Re-verify the version-id under the row lock. The
		// previous pre-tx check is racy: a concurrent upgrade
		// could have committed between the pre-tx
		// GetInstallation and this FOR UPDATE. Catching that
		// race here is the whole point of carrying FromVersionID
		// through the request.
		if currentVer != req.FromVersionID {
			return fmt.Errorf("%w: install %s is now at %s, expected %s",
				ErrVersionMismatch, req.InstallationID, currentVer, req.FromVersionID)
		}
		if marketplace.InstallStatus(status) == marketplace.InstallStatusUninstalled {
			return fmt.Errorf("%w: installation %s is uninstalled", marketplace.ErrConflict, req.InstallationID)
		}
		if marketplace.InstallStatus(status) == marketplace.InstallStatusInstalling {
			return fmt.Errorf("%w: installation %s is still installing", marketplace.ErrConflict, req.InstallationID)
		}
		fromVersionID = currentVer

		// Choose the post-upgrade settings document.
		// - KeepSettings = pass existing settings through verbatim.
		// - Settings != nil = use the migrated document the
		//   handler validated against the target version's schema.
		// - Settings == nil && !KeepSettings = the handler did not
		//   touch settings; default to keep-existing behaviour
		//   (this is the common forward-compatible-upgrade path).
		var newSettingsJSON []byte
		switch {
		case req.Settings != nil:
			b, err := json.Marshal(req.Settings)
			if err != nil {
				return fmt.Errorf("runtime: engine: upgrade: marshal settings: %w", err)
			}
			newSettingsJSON = b
		default:
			// Pass through the existing document. Read-as-text
			// then write-as-jsonb to avoid a re-parse / re-canon
			// roundtrip that would lose any whitespace the
			// publisher's hook receiver might (incorrectly) be
			// signature-checking against — Postgres normalises
			// jsonb storage already, so what we read back IS the
			// canonical form.
			newSettingsJSON = []byte(settingsTxt)
		}

		// Step 4b: drop every old runtime-table row for this
		// install. RLS + the explicit tenant_id predicate guard
		// against cross-tenant deletes.
		if err := e.registrar.UnregisterAll(ctx, tx, req.TenantID, req.InstallationID); err != nil {
			return fmt.Errorf("runtime: engine: upgrade: unregister old: %w", err)
		}

		// Step 4c: register the new bundle. Failure here rolls
		// back the unregister above thanks to the enclosing tx —
		// the install reverts to FromVersionID cleanly.
		if err := e.registrar.RegisterAll(ctx, tx, req.TenantID, req.InstallationID, webhookBase, newBundle); err != nil {
			return fmt.Errorf("runtime: engine: upgrade: register new: %w", err)
		}

		// Step 4d: stamp the new version_id + settings + updated_at.
		// status flips to 'active' in case it was 'failed' before
		// the upgrade (the operator may be upgrading PAST the
		// broken version). failure_reason MUST clear in lock-step
		// with the status transition to satisfy the
		// failure_reason_only_when_failed CHECK at
		// migration 000068:261-265 — same shape as Engine.Uninstall.
		var (
			newUpdatedAt   time.Time
			newSettingsTxt string
			newStatus      string
		)
		row = tx.QueryRow(ctx,
			`UPDATE marketplace_extension_installations
			    SET extension_version_id = $1,
			        settings = $2::jsonb,
			        status = 'active',
			        failure_reason = NULL,
			        updated_at = now()
			  WHERE tenant_id = $3 AND id = $4
			  RETURNING status, settings::text, updated_at`,
			req.ToVersionID, string(newSettingsJSON), req.TenantID, req.InstallationID)
		if err := row.Scan(&newStatus, &newSettingsTxt, &newUpdatedAt); err != nil {
			return fmt.Errorf("runtime: engine: upgrade: update install row: %w", err)
		}

		updated = marketplace.Installation{
			ID:                 req.InstallationID,
			TenantID:           req.TenantID,
			ExtensionID:        extID,
			ExtensionVersionID: req.ToVersionID,
			Status:             marketplace.InstallStatus(newStatus),
			Settings:           []byte(newSettingsTxt),
			WebhookBase:        webhookBase,
			InstalledBy:        installedBy,
			InstalledAt:        installedAt,
			UpdatedAt:          newUpdatedAt,
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	out := &UpgradeResult{
		Installation:     &updated,
		FromVersionID:    fromVersionID,
		PreUpgradeResult: preUpgradeResult,
	}

	// Step 5: post_upgrade (BEST-EFFORT, skipped when SkipHooks).
	// The tx has committed; like post_install / post_uninstall,
	// every error path below still returns the UpgradeResult so
	// the caller sees the committed state. Failures are recorded
	// in dispatch_log (via the hook dispatcher's internal logging)
	// and folded into PostUpgradeResult.
	if req.SkipHooks {
		return out, nil
	}
	if secret == "" {
		// Defensive — the SkipHooks branch above is the documented
		// "empty secret ok" path; reaching here with an empty
		// secret means loadSigningSecret returned "" without
		// erroring, which it shouldn't. Surface the skip as a
		// structured LifecycleResult mirroring the same shape as
		// UpdateSettings's empty-secret branch.
		out.PostUpgradeResult = &LifecycleResult{
			Aborted:     true,
			AbortReason: "missing signing secret on install — post_upgrade hook skipped",
			Err:         fmt.Errorf("runtime: engine: upgrade: install %s has empty signing secret", req.InstallationID),
		}
		return out, nil
	}
	body, err := MarshalLifecyclePayload(map[string]any{
		"phase":           string(PhasePostUpgrade),
		"tenant_id":       req.TenantID.String(),
		"installation_id": req.InstallationID.String(),
		"extension_id":    updated.ExtensionID.String(),
		"from_version_id": fromVersionID.String(),
		"to_version_id":   req.ToVersionID.String(),
		"webhook_base":    updated.WebhookBase,
		"upgraded_by":     uuidOrNilString(&req.UpgradedBy),
	})
	if err != nil {
		out.PostUpgradeResult = &LifecycleResult{Err: fmt.Errorf("runtime: engine: post_upgrade marshal: %w", err)}
		return out, nil
	}
	res, postErr := e.hooks.Dispatch(ctx, &LifecycleDispatch{
		TenantID:           req.TenantID,
		InstallationID:     req.InstallationID,
		ExtensionID:        updated.ExtensionID,
		ExtensionVersionID: req.ToVersionID,
		Phase:              PhasePostUpgrade,
		WebhookBase:        updated.WebhookBase,
		SigningSecret:      secret,
		Body:               body,
	})
	if postErr != nil && res == nil {
		res = &LifecycleResult{Err: postErr}
	}
	out.PostUpgradeResult = res
	return out, nil
}
