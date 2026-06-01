import { useCallback, useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import {
  Badge,
  Button,
  ControlledModal,
  Input,
} from "@kapp/ui";
import { api } from "../../lib/api";
import type {
  InstallMarketplaceExtensionResponse,
  MarketplaceExtension,
  MarketplaceExtensionVersion,
} from "@kapp/client";
import {
  defaultWebhookBase,
  formatBundleSize,
  formatTimestamp,
} from "./lib";
import { SettingsForm, validateAgainstSchema } from "./SettingsForm";

/**
 * InstallExtensionDialog drives the POST /api/v1/marketplace/installations
 * flow:
 *
 *   1. Show the version metadata + required permissions for
 *      review (the user clicked Install but should still be able
 *      to back out after seeing the full ask).
 *   2. Render a settings form against the version's
 *      manifest-declared settings_schema. If the manifest didn't
 *      ship a schema, the body becomes a free-form JSON editor
 *      so power users can still pass key/value pairs the
 *      extension might read out-of-band.
 *   3. POST with idempotency key; on success surface the install
 *      row + signing_secret to the caller via onInstalled so the
 *      detail page can route to the install management view.
 *
 * Settings validation runs both client-side (instant UX) and
 * server-side (engine.Install re-validates against the same
 * schema). The client-side check is best-effort — the server is
 * still the source of truth.
 */
export function InstallExtensionDialog({
  extension,
  version,
  onClose,
  onInstalled,
}: {
  extension: MarketplaceExtension;
  version: MarketplaceExtensionVersion;
  onClose: () => void;
  onInstalled: (res: InstallMarketplaceExtensionResponse) => void;
}) {
  const [webhookBase, setWebhookBase] = useState(defaultWebhookBase());
  const [settings, setSettings] = useState<Record<string, unknown>>({});
  const [validationError, setValidationError] = useState<string | null>(null);
  // settingsInvalidKeys mirrors the same per-editor-key validity
  // map InstallationDetailPage uses (round-4 ANALYSIS_0001). The
  // dialog has the same exact failure mode: the
  // FreeformJsonEditor (and any future NestedJsonEditor under a
  // B6.2 schema) only fires onChange when its text buffer parses
  // cleanly, so the parent's `settings` state retains the LAST
  // valid value when the buffer is mid-stream unparseable. Pre-
  // round-5 the dialog had no validity signal threaded at all,
  // which meant clicking Install while the textarea showed
  // "{not valid json" silently submitted the previous valid
  // value instead of the bytes on screen — confusing UX ("the
  // install succeeded but it didn't use the keys I just typed").
  // The fix mirrors the SettingsForm.tsx contract: each editor
  // identifies itself with a stable key, the parent tracks the
  // set of currently-invalid editors, and the Install button is
  // disabled iff the set is non-empty. Today only the freeform
  // editor is mounted (the dialog has no schema wired yet — see
  // the `useMemo(() => null, [])` below), so this is
  // architecturally over-built for the present case; under B6.2
  // it will compose with multiple object-typed schema fields
  // exactly the way InstallationDetailPage does.
  const [settingsInvalidKeys, setSettingsInvalidKeys] = useState<Set<string>>(
    () => new Set(),
  );
  const settingsFormValid = settingsInvalidKeys.size === 0;
  const handleSettingsValidity = useCallback(
    (key: string, valid: boolean) => {
      setSettingsInvalidKeys((prev) => {
        if (valid) {
          if (!prev.has(key)) return prev;
          const next = new Set(prev);
          next.delete(key);
          return next;
        }
        if (prev.has(key)) return prev;
        const next = new Set(prev);
        next.add(key);
        return next;
      });
    },
    [],
  );

  // Fetch the version's settings_schema from the manifest. The
  // backend doesn't currently expose a dedicated schema endpoint
  // (the schema rides on the install resolver), so we surface a
  // raw key/value JSON editor and rely on the engine's
  // validateInstallSettings to reject malformed input.
  //
  // When B6.2 ships GET /marketplace/extensions/{id}/versions/{id}/schema,
  // wire it through `schemaQuery` here and feed the result to
  // SettingsForm — the component already accepts a schema prop.
  const schema = useMemo(() => null, []);

  // Reset state when the dialog is shown for a different version.
  // Includes the validity set: per-editor-key signals from the
  // previously-mounted SettingsForm refer to keys that may no
  // longer be in the new schema, and the previous version's
  // unmount-cleanup will already have cleared its own keys. We
  // start from an empty set to avoid any chance of carrying
  // stale invalid-key entries across the version swap.
  //
  // Round-6 ANALYSIS_0001: the reset MUST happen during render,
  // not in a useEffect. React's standard derived-state-from-
  // props pattern: hold previous version.id in state, compare
  // during render, and if it changed, schedule the resets
  // SYNCHRONOUSLY before children reconcile. Pre-fix we used
  // useEffect, which runs post-commit — so on a version swap
  // the new SettingsForm subtree (keyed on version.id) would
  // mount during the render with the STALE `settings` prop
  // still in scope, the FreeformJsonEditor's lazy init would
  // seed its text buffer from the previous version's settings,
  // and only AFTER the commit would the effect run and the
  // parent settings finally clear. By then the textarea had
  // already painted the wrong bytes. Doing the reset in render
  // via setSettings({}) is React's official guidance for
  // "derive state from prop change" (see the React docs page
  // "You Might Not Need an Effect"). The setState calls during
  // render are queued; React re-renders before painting, so
  // the new SettingsForm subtree mounts against the freshly-
  // empty settings as a single atomic remount.
  const [prevVersionId, setPrevVersionId] = useState(version.id);
  if (version.id !== prevVersionId) {
    setPrevVersionId(version.id);
    setSettings({});
    setValidationError(null);
    setWebhookBase(defaultWebhookBase());
    setSettingsInvalidKeys(new Set());
  }

  const install = useMutation({
    mutationFn: (input: { settings: Record<string, unknown> }) =>
      api.installMarketplaceExtension({
        extension_id: extension.id,
        version_id: version.id,
        webhook_base: webhookBase.trim(),
        settings: input.settings,
      }),
    onSuccess: (res) => onInstalled(res),
  });

  const onConfirm = () => {
    setValidationError(null);
    // Round-7 ANALYSIS_0002: defense-in-depth re-check of the
    // settings-form validity signal at the top of onConfirm,
    // mirroring the webhook + schema gates below. The Install
    // button's `disabled={pending || !settingsFormValid}` prop
    // (line ~336) already prevents the click in standard
    // browsers, but `<button disabled>` is not a hard wall:
    //   * Some accessibility tools can fire synthetic click
    //     events that bypass the disabled attribute.
    //   * Programmatic invocation (e.g. an e2e test calling
    //     `onConfirm()` directly, or a third-party script
    //     wiring its own keyboard shortcut) routes around the
    //     button entirely.
    //   * A future refactor could swap the disabled prop for
    //     a styling-only "looks-disabled" class without
    //     realising the validity gate is purely visual.
    // Since the parent's settings draft retains the last valid
    // parsed object while the textarea shows mid-stream
    // unparseable text, submitting through any of those side
    // doors would silently send the stale-but-valid draft
    // instead of the bytes on screen — same UX failure mode
    // the validity-signal lift was designed to close, and now
    // closed at the data path too. Cost is two lines; benefit
    // is no longer relying on a single UI gate for a data-
    // integrity invariant.
    if (!settingsFormValid) {
      setValidationError(
        "Fix the settings JSON before installing — the textarea contents are not parseable.",
      );
      return;
    }
    if (!webhookBase.trim()) {
      setValidationError("Webhook base URL is required.");
      return;
    }
    try {
      const u = new URL(webhookBase.trim());
      if (u.protocol !== "https:" && u.protocol !== "http:") {
        setValidationError("Webhook base must be an http(s) URL.");
        return;
      }
    } catch {
      setValidationError("Webhook base must be a valid URL.");
      return;
    }
    if (schema) {
      const err = validateAgainstSchema(schema, settings);
      if (err) {
        setValidationError(err);
        return;
      }
    }
    install.mutate({ settings });
  };

  // Round-10 ANALYSIS_0003: a single requestClose helper used by
  // BOTH the modal's backdrop/ESC handler AND the explicit Cancel
  // button, so the in-flight-install guard can't be bypassed via
  // the disabled-attribute side door the same way the round-7
  // ANALYSIS_0002 (Install button) and round-8 BUG_0001 (Save
  // settings button) defense-in-depth patterns close their own
  // disabled-bypass vectors. Pre-fix, the Cancel button passed
  // `onClick={onClose}` directly while only the modal's onClose
  // wrapped it with the isPending check — meaning a synthetic
  // click that bypassed the disabled attribute (accessibility
  // tools firing the listener directly, programmatic invocation,
  // a future refactor swapping disabled for a styling-only
  // class) would close the dialog mid-install. The impact is
  // limited (the mutation continues, onInstalled still fires)
  // but the asymmetry would surprise a maintainer reading
  // either path expecting the guards to match. Pattern parity
  // also means future close vectors (e.g. a keyboard shortcut,
  // an outside-click handler) get the guard for free.
  const requestClose = () => {
    if (install.isPending) return;
    onClose();
  };

  return (
    <ControlledModal
      open
      onClose={requestClose}
      title={`Install ${extension.display_name} v${version.version}`}
    >
      <div style={{ display: "grid", gap: 16, padding: "8px 0" }}>
        <div>
          <p style={{ marginTop: 0, color: "#4b5563" }}>
            Review what {extension.display_name} will request, then confirm to
            install it for this tenant.
          </p>
        </div>

        <section>
          <h4 style={{ margin: "0 0 8px" }}>Version</h4>
          <DetailRow label="Version" value={`v${version.version}`} />
          <DetailRow
            label="Published"
            value={formatTimestamp(version.published_at)}
          />
          <DetailRow
            label="Bundle size"
            value={formatBundleSize(version.bundle_size_bytes)}
          />
          <DetailRow
            label="Signed"
            value={
              version.bundle_signature ? (
                <Badge variant="success">
                  {version.bundle_signature_key_id ?? "ed25519"}
                </Badge>
              ) : (
                <Badge variant="outline">Unsigned</Badge>
              )
            }
          />
        </section>

        {(version.features_required.length > 0 ||
          version.permissions_required.length > 0) && (
          <section>
            <h4 style={{ margin: "0 0 8px" }}>Required access</h4>
            {version.features_required.length > 0 && (
              <div style={{ marginBottom: 8 }}>
                <div style={{ fontSize: 12, color: "#6b7280" }}>
                  Tenant feature flags
                </div>
                <PermissionRow items={version.features_required} />
              </div>
            )}
            {version.permissions_required.length > 0 && (
              <div>
                <div style={{ fontSize: 12, color: "#6b7280" }}>
                  Platform permissions
                </div>
                <PermissionRow items={version.permissions_required} />
              </div>
            )}
          </section>
        )}

        <section>
          <h4 style={{ margin: "0 0 8px" }}>Webhook base URL</h4>
          <Input
            type="url"
            value={webhookBase}
            onChange={(e) => setWebhookBase(e.target.value)}
            placeholder="https://your-tenant.example.com"
            disabled={install.isPending}
            aria-label="Webhook base URL"
          />
          <p
            style={{
              margin: "4px 0 0",
              fontSize: 12,
              color: "#6b7280",
            }}
          >
            Outbound webhooks the extension dispatches are POSTed under this
            origin. Defaults to the current window origin; override only if you
            terminate webhooks elsewhere.
          </p>
        </section>

        <section>
          <h4 style={{ margin: "0 0 8px" }}>Settings</h4>
          {/*
           * Round-6 ANALYSIS_0001: key the SettingsForm subtree on
           * version.id so that if the dialog's lifecycle ever
           * changes (e.g. the parent stops force-unmounting via
           * `installVersionId={null}` between version switches —
           * any future "next version" inline action would do this),
           * the uncontrolled FreeformJsonEditor's textarea buffer
           * AND the parent's `settings` state reset together as a
           * single atomic remount. Today the dialog always
           * unmounts between versions so this is dead weight, but
           * the cost is one React key and the gain is defense-in-
           * depth against a future refactor silently introducing a
           * stale-textarea bug.
           */}
          <SettingsForm
            key={version.id}
            schema={schema}
            value={settings}
            onChange={setSettings}
            onValidityChange={handleSettingsValidity}
            disabled={install.isPending}
          />
          {!settingsFormValid && (
            <p
              style={{
                margin: "4px 0 0",
                fontSize: 12,
                color: "#b91c1c",
              }}
            >
              Resolve the JSON parse error above before installing — the
              install will use the text currently on screen.
            </p>
          )}
        </section>

        {validationError && (
          <p style={{ color: "#b91c1c", margin: 0 }}>{validationError}</p>
        )}
        {install.isError && (
          <p style={{ color: "#b91c1c", margin: 0 }}>
            Install failed: {(install.error as Error).message}
          </p>
        )}

        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <Button
            variant="outline"
            onClick={requestClose}
            disabled={install.isPending}
          >
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={onConfirm}
            // Gate Install on settingsFormValid for the exact reason
            // the Save button on InstallationDetailPage does:
            // unparseable JSON in any underlying editor keeps the
            // parent's `settings` state pinned to the LAST valid
            // value, which is NOT what's on screen. Submitting that
            // would silently install the stale-but-valid document.
            disabled={install.isPending || !settingsFormValid}
          >
            {install.isPending ? "Installing…" : "Install extension"}
          </Button>
        </div>
      </div>
    </ControlledModal>
  );
}

function PermissionRow({ items }: { items: string[] }) {
  return (
    <div
      style={{
        display: "flex",
        flexWrap: "wrap",
        gap: 6,
        marginTop: 4,
      }}
    >
      {items.map((p) => (
        <Badge key={p} variant="default">
          {p}
        </Badge>
      ))}
    </div>
  );
}

function DetailRow({
  label,
  value,
}: {
  label: string;
  value: React.ReactNode;
}) {
  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: "140px 1fr",
        gap: 8,
        padding: "4px 0",
        fontSize: 14,
      }}
    >
      <span style={{ color: "#6b7280" }}>{label}</span>
      <span>{value}</span>
    </div>
  );
}
