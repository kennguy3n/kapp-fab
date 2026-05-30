package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// Engine is the top-level orchestrator that wires Registrar +
// LifecycleHooks + Dispatcher into the install / uninstall /
// invoke flows.
//
// Engine.Install:
//
//  1. Validate the InstallRequest + the resolved bundle.
//  2. Load the extension version row (catalog) and verify it
//     is publishable: extension status = approved AND version
//     is not yanked.
//  3. Generate a fresh signing secret.
//  4. Dispatch pre_install (BLOCKING). 4xx/5xx/transport
//     failure → return ErrPreInstallRejected without touching
//     the database. The dispatch_log records the attempt.
//  5. Open a tenant-scoped tx. Inside the tx:
//     a. INSERT the install row (status='installing',
//     signing_secret = step 3).
//     b. Registrar.RegisterAll for every KType/workflow/tool/
//     webhook in the resolved bundle.
//     c. UPDATE install row to status='active'.
//     Commit. If any step fails, the whole tx rolls back and
//     the engine returns the error with NO partial registration.
//  6. Dispatch post_install (BEST-EFFORT). A failed post_install
//     is logged but does NOT roll back the install — the
//     extension is registered and operational; the publisher's
//     side just won't see the "you have a new install"
//     notification. The dispatch_log records the attempt.
//
// Engine.Uninstall is symmetric:
//
//  1. Validate request and load the install row.
//  2. Dispatch pre_uninstall (BLOCKING unless req.SkipHooks).
//  3. Open tenant tx, run Registrar.UnregisterAll + UPDATE
//     status='uninstalled'.
//  4. Dispatch post_uninstall (BEST-EFFORT).
//
// Both flows are idempotent against retry — a second Install with
// the same (tenant, extension) hits the marketplace_extension_
// installations unique constraint and returns ErrConflict. A
// second Uninstall on an already-uninstalled row is rejected by
// the InstallStatus transition graph.
type Engine struct {
	pool      *pgxpool.Pool
	store     *marketplace.Store
	registrar *Registrar
	hooks     LifecycleHooks
	// now is the clock the engine uses for signing-timestamp and
	// dispatch-log timestamps. Tests override.
	now func() time.Time
	// generateSecret is the signing-secret factory. Tests can
	// pin this to a deterministic value to assert against. Nil
	// uses GenerateSigningSecret.
	generateSecret func() (SigningSecret, error)
}

// EngineOptions configures an Engine at construction time.
type EngineOptions struct {
	// Pool is the database pool. Required.
	Pool *pgxpool.Pool
	// Store is the marketplace catalog repository. Required —
	// the engine reads ExtensionVersion rows for manifest +
	// status checks.
	Store *marketplace.Store
	// Registrar handles INSERTs into the runtime tables. Defaults
	// to NewRegistrar() if nil.
	Registrar *Registrar
	// Hooks is the lifecycle hook dispatcher. Defaults to
	// NoopHooks() if nil — handy for tests that don't exercise
	// hook semantics.
	Hooks LifecycleHooks
	// Now is the clock. Defaults to time.Now if nil.
	Now func() time.Time
	// GenerateSecret is the signing-secret factory. Defaults to
	// GenerateSigningSecret if nil.
	GenerateSecret func() (SigningSecret, error)
}

// NewEngine constructs an Engine from EngineOptions. Returns an
// error if a required option is missing.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.Pool == nil {
		return nil, errors.New("runtime: engine: pool required")
	}
	if opts.Store == nil {
		return nil, errors.New("runtime: engine: store required")
	}
	e := &Engine{
		pool:           opts.Pool,
		store:          opts.Store,
		registrar:      opts.Registrar,
		hooks:          opts.Hooks,
		now:            opts.Now,
		generateSecret: opts.GenerateSecret,
	}
	if e.registrar == nil {
		e.registrar = NewRegistrar()
	}
	if e.hooks == nil {
		e.hooks = NoopHooks()
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.generateSecret == nil {
		e.generateSecret = GenerateSigningSecret
	}
	return e, nil
}

// InstallResult is the return value of Engine.Install.
type InstallResult struct {
	// Installation is the freshly-created install row. Status is
	// 'active' on success; the engine never returns a partially-
	// installed row — failures roll back the entire tx and
	// return a nil installation.
	Installation *marketplace.Installation
	// SigningSecret is the generated per-install HMAC key. The
	// caller (B6 API handler) is expected to return this to the
	// operator who initiated the install so they can configure
	// the extension's webhook server with the matching key.
	// After this Install call, the secret is also persisted in
	// marketplace_extension_installations.signing_secret — the
	// runtime can recover it on subsequent dispatches.
	SigningSecret SigningSecret
	// PreInstallResult is the dispatch_log-equivalent shape from
	// the pre_install hook dispatch. Nil if the hooks were
	// NoopHooks.
	PreInstallResult *LifecycleResult
	// PostInstallResult is the dispatch_log-equivalent shape from
	// the post_install hook dispatch. Nil if hooks were
	// NoopHooks.
	PostInstallResult *LifecycleResult
}

// Install runs the install lifecycle. See Engine doc comment for
// the step-by-step flow.
func (e *Engine) Install(ctx context.Context, req *InstallRequest, bundle *ResolvedBundle) (*InstallResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if bundle == nil || bundle.Manifest == nil {
		return nil, errors.New("runtime: engine: install: nil bundle/manifest")
	}

	// Step 2: catalog-side preconditions.
	ext, err := e.store.GetExtension(ctx, req.ExtensionID)
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: get extension: %w", err)
	}
	if ext.Status != marketplace.ExtensionStatusListed {
		return nil, fmt.Errorf("runtime: engine: extension status %q does not permit install (need 'listed')", ext.Status)
	}
	ver, err := e.store.GetVersion(ctx, req.VersionID)
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: get version: %w", err)
	}
	if ver.ExtensionID != req.ExtensionID {
		return nil, fmt.Errorf("runtime: engine: version %s does not belong to extension %s", req.VersionID, req.ExtensionID)
	}
	if ver.Yanked {
		return nil, fmt.Errorf("%w: version %s is yanked", marketplace.ErrYanked, req.VersionID)
	}

	// Step 3: generate signing secret.
	secret, err := e.generateSecret()
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: generate signing secret: %w", err)
	}

	webhookBase := req.NormalizedWebhookBase()

	// Step 4: pre_install (BLOCKING).
	preBody, err := MarshalLifecyclePayload(map[string]any{
		"phase":          string(PhasePreInstall),
		"tenant_id":      req.TenantID.String(),
		"extension_id":   req.ExtensionID.String(),
		"version_id":     req.VersionID.String(),
		"webhook_base":   webhookBase,
		"settings":       req.Settings,
		"installed_by":   uuidOrNilString(&req.InstalledBy),
		"signing_secret": string(secret),
	})
	if err != nil {
		return nil, err
	}
	preResult, preErr := e.hooks.Dispatch(ctx, &LifecycleDispatch{
		TenantID:           req.TenantID,
		InstallationID:     uuid.Nil, // not yet known
		ExtensionID:        req.ExtensionID,
		ExtensionVersionID: req.VersionID,
		Phase:              PhasePreInstall,
		WebhookBase:        webhookBase,
		SigningSecret:      secret,
		Body:               preBody,
	})
	if preErr != nil {
		return nil, preErr
	}
	if preResult != nil && preResult.Aborted {
		return nil, fmt.Errorf("%w: %s", ErrPreInstallRejected, preResult.AbortReason)
	}

	// Step 5: transactional registration.
	var (
		installation marketplace.Installation
	)
	settingsJSON := []byte("{}")
	if len(req.Settings) > 0 {
		b, err := json.Marshal(req.Settings)
		if err != nil {
			return nil, fmt.Errorf("runtime: engine: marshal settings: %w", err)
		}
		settingsJSON = b
	}

	// installed_by → NULL when caller didn't supply a user id, so
	// the FK to users(id) is satisfied for system/bootstrap installs
	// (e.g. tenant-provisioning scripts) that have no operator
	// associated with them. Passing uuid.Nil literally would insert
	// the all-zero UUID and trip the FK.
	var installedByArg interface{}
	if req.InstalledBy != uuid.Nil {
		installedByArg = req.InstalledBy
	}
	txErr := dbutil.WithTenantTx(ctx, e.pool, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// INSERT install row (status='installing', signing_secret).
		row := tx.QueryRow(ctx,
			`INSERT INTO marketplace_extension_installations (
				tenant_id, extension_id, extension_version_id, status, settings,
				webhook_base, installed_by, signing_secret
			) VALUES (
				$1, $2, $3, 'installing', $4::jsonb, $5, $6, $7
			)
			RETURNING id, tenant_id, extension_id, extension_version_id, status, settings::text,
			          webhook_base, installed_by, installed_at, updated_at`,
			req.TenantID, req.ExtensionID, req.VersionID, string(settingsJSON), webhookBase, installedByArg, string(secret),
		)
		var installedBy *uuid.UUID
		var settingsTxt string
		if err := row.Scan(
			&installation.ID, &installation.TenantID, &installation.ExtensionID, &installation.ExtensionVersionID,
			&installation.Status, &settingsTxt, &installation.WebhookBase,
			&installedBy, &installation.InstalledAt, &installation.UpdatedAt,
		); err != nil {
			if isPGUniqueViolation(err) {
				return fmt.Errorf("%w: tenant has already installed this extension", marketplace.ErrConflict)
			}
			return fmt.Errorf("runtime: engine: insert install row: %w", err)
		}
		installation.Settings = []byte(settingsTxt)
		installation.InstalledBy = installedBy

		// Atomic registration of every resource declared in the
		// resolved bundle.
		if err := e.registrar.RegisterAll(ctx, tx, req.TenantID, installation.ID, webhookBase, bundle); err != nil {
			return err
		}

		// Promote to 'active'.
		_, err := tx.Exec(ctx,
			`UPDATE marketplace_extension_installations
			   SET status = 'active', updated_at = now()
			 WHERE tenant_id = $1 AND id = $2`,
			req.TenantID, installation.ID)
		if err != nil {
			return fmt.Errorf("runtime: engine: promote to active: %w", err)
		}
		installation.Status = marketplace.InstallStatusActive
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	// Step 6: post_install (BEST-EFFORT).
	postBody, err := MarshalLifecyclePayload(map[string]any{
		"phase":           string(PhasePostInstall),
		"tenant_id":       req.TenantID.String(),
		"installation_id": installation.ID.String(),
		"extension_id":    req.ExtensionID.String(),
		"version_id":      req.VersionID.String(),
		"webhook_base":    webhookBase,
		"installed_at":    installation.InstalledAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	postResult, postErr := e.hooks.Dispatch(ctx, &LifecycleDispatch{
		TenantID:           req.TenantID,
		InstallationID:     installation.ID,
		ExtensionID:        req.ExtensionID,
		ExtensionVersionID: req.VersionID,
		Phase:              PhasePostInstall,
		WebhookBase:        webhookBase,
		SigningSecret:      secret,
		Body:               postBody,
	})
	// post_install errors are NOT propagated — best-effort. The
	// dispatch_log captures the attempt. We do return the
	// LifecycleResult so the caller can surface a warning to
	// the operator.
	if postErr != nil && postResult == nil {
		postResult = &LifecycleResult{Err: postErr}
	}

	return &InstallResult{
		Installation:      &installation,
		SigningSecret:     secret,
		PreInstallResult:  preResult,
		PostInstallResult: postResult,
	}, nil
}

// UninstallResult is the return value of Engine.Uninstall.
type UninstallResult struct {
	// Installation is the row after the status flip. Status is
	// 'uninstalled'.
	Installation *marketplace.Installation
	// PreUninstallResult / PostUninstallResult mirror the install
	// shape. Nil if hooks were NoopHooks or req.SkipHooks.
	PreUninstallResult  *LifecycleResult
	PostUninstallResult *LifecycleResult
}

// Uninstall runs the uninstall lifecycle. See Engine doc comment.
func (e *Engine) Uninstall(ctx context.Context, req *UninstallRequest) (*UninstallResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	// Load the install row to fetch signing_secret + webhook_base
	// for the hook dispatch and to verify the row is in a state
	// where uninstall is permitted (not already 'uninstalled').
	install, err := e.store.GetInstallation(ctx, req.TenantID, req.InstallationID)
	if err != nil {
		return nil, fmt.Errorf("runtime: engine: load install: %w", err)
	}
	if install.Status == marketplace.InstallStatusUninstalled {
		return nil, fmt.Errorf("%w: installation %s already uninstalled", marketplace.ErrConflict, req.InstallationID)
	}

	// Fetch the signing secret directly — Store does not expose it
	// on Installation (it's runtime-only).
	secret, err := e.loadSigningSecret(ctx, req.TenantID, req.InstallationID)
	if err != nil {
		return nil, err
	}

	var preResult, postResult *LifecycleResult

	// pre_uninstall (BLOCKING unless skipped).
	if !req.SkipHooks {
		body, err := MarshalLifecyclePayload(map[string]any{
			"phase":           string(PhasePreUninstall),
			"tenant_id":       req.TenantID.String(),
			"installation_id": req.InstallationID.String(),
			"extension_id":    install.ExtensionID.String(),
			"version_id":      install.ExtensionVersionID.String(),
			"webhook_base":    install.WebhookBase,
			"uninstalled_by":  uuidOrNilString(&req.UninstalledBy),
		})
		if err != nil {
			return nil, err
		}
		res, err := e.hooks.Dispatch(ctx, &LifecycleDispatch{
			TenantID:           req.TenantID,
			InstallationID:     req.InstallationID,
			ExtensionID:        install.ExtensionID,
			ExtensionVersionID: install.ExtensionVersionID,
			Phase:              PhasePreUninstall,
			WebhookBase:        install.WebhookBase,
			SigningSecret:      secret,
			Body:               body,
		})
		if err != nil {
			return nil, err
		}
		if res != nil && res.Aborted {
			return nil, fmt.Errorf("%w: %s", ErrPreUninstallRejected, res.AbortReason)
		}
		preResult = res
	}

	// Transactional teardown: unregister runtime tables + flip
	// status. The install row itself is retained for audit.
	txErr := dbutil.WithTenantTx(ctx, e.pool, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := e.registrar.UnregisterAll(ctx, tx, req.TenantID, req.InstallationID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE marketplace_extension_installations
			   SET status = 'uninstalled', updated_at = now()
			 WHERE tenant_id = $1 AND id = $2`,
			req.TenantID, req.InstallationID)
		if err != nil {
			return fmt.Errorf("runtime: engine: flip to uninstalled: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	install.Status = marketplace.InstallStatusUninstalled

	// post_uninstall (BEST-EFFORT, skipped if SkipHooks).
	if !req.SkipHooks {
		body, err := MarshalLifecyclePayload(map[string]any{
			"phase":           string(PhasePostUninstall),
			"tenant_id":       req.TenantID.String(),
			"installation_id": req.InstallationID.String(),
			"extension_id":    install.ExtensionID.String(),
			"version_id":      install.ExtensionVersionID.String(),
		})
		if err != nil {
			return nil, err
		}
		res, postErr := e.hooks.Dispatch(ctx, &LifecycleDispatch{
			TenantID:           req.TenantID,
			InstallationID:     req.InstallationID,
			ExtensionID:        install.ExtensionID,
			ExtensionVersionID: install.ExtensionVersionID,
			Phase:              PhasePostUninstall,
			WebhookBase:        install.WebhookBase,
			SigningSecret:      secret,
			Body:               body,
		})
		// Best-effort; an error here is captured but not propagated.
		if postErr != nil && res == nil {
			res = &LifecycleResult{Err: postErr}
		}
		postResult = res
	}

	return &UninstallResult{
		Installation:        install,
		PreUninstallResult:  preResult,
		PostUninstallResult: postResult,
	}, nil
}

// loadSigningSecret reads the per-install HMAC secret directly. The
// marketplace.Store does NOT expose this column (the secret never
// flows through Installation JSON). The engine has its own reader
// because hook dispatch needs it.
func (e *Engine) loadSigningSecret(ctx context.Context, tenantID, installID uuid.UUID) (SigningSecret, error) {
	var secret string
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT signing_secret FROM marketplace_extension_installations
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, installID)
		if err := row.Scan(&secret); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: install %s", marketplace.ErrNotFound, installID)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if secret == "" {
		return "", fmt.Errorf("runtime: engine: install %s has empty signing secret", installID)
	}
	return SigningSecret(secret), nil
}

// uuidOrNilString returns the string form of u or "" for uuid.Nil
// (or a nil *uuid.UUID). Used to serialise InstalledBy /
// UninstalledBy into the lifecycle payload — the extension's hook
// receives an empty string when the install was system-initiated.
func uuidOrNilString(u *uuid.UUID) string {
	if u == nil || *u == uuid.Nil {
		return ""
	}
	return u.String()
}
