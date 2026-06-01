import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  Badge,
  Button,
  Card,
  CardContent,
  ControlledModal,
} from "@kapp/ui";
import { api } from "../../lib/api";
import type {
  MarketplaceExtensionVersion,
  MarketplaceGetExtensionResponse,
  MarketplaceInstallation,
} from "@kapp/client";
import {
  formatBundleSize,
  formatTimestamp,
  installStatusLabel,
  installStatusVariant,
  sortVersionsByPublishedDesc,
} from "./lib";
import {
  SettingsForm,
  validateAgainstSchema,
} from "./SettingsForm";

/**
 * InstallationDetailPage is the per-install management surface:
 *
 *   - Status header with badge + failure_reason / last health check
 *   - Settings editor — schema-aware form backed by PATCH
 *     /installations/{id}/settings; falls back to free-form JSON
 *     when the version manifest declares no schema.
 *   - Upgrade panel — only shown when a newer non-yanked version
 *     is published. Calls POST /installations/{id}/upgrade with
 *     from_version_id (the optimistic-concurrency token the
 *     engine re-checks under FOR UPDATE).
 *   - Uninstall — confirm dialog gates the DELETE call.
 *
 * Settings edits go through optimistic update so the UI doesn't
 * stutter on the round trip — onMutate stages the new document in
 * cache, the server's response replaces it, and on error we roll
 * back to the cached snapshot.
 */
export function InstallationDetailPage() {
  const { installId } = useParams<{ installId: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const install = useQuery<MarketplaceInstallation>({
    queryKey: ["marketplace", "installation", installId],
    queryFn: () => api.getMarketplaceInstallation(installId!),
    enabled: !!installId,
  });

  // Pull the extension for header context + version resolution.
  // The /extensions/{id} endpoint already returns versions[] from
  // the same listApprovedVersions backend path that the dedicated
  // listMarketplaceVersions endpoint uses, so we read versions
  // off this response rather than firing a second round-trip.
  // (Same N+1 elimination pattern as MarketplaceInstallationsPage
  // — see its renderVersion + versionsLookup for the prior art.)
  const ext = useQuery<MarketplaceGetExtensionResponse>({
    queryKey: ["marketplace", "extension", install.data?.extension_id],
    queryFn: () => api.getMarketplaceExtension(install.data!.extension_id),
    enabled: !!install.data?.extension_id,
  });

  const [settingsDraft, setSettingsDraft] = useState<Record<string, unknown>>(
    {},
  );
  const [settingsTouched, setSettingsTouched] = useState(false);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  // settingsInvalidKeys tracks the parse-validity of every
  // underlying JSON textarea editor (FreeformJsonEditor /
  // NestedJsonEditor). Each editor identifies itself with a
  // stable key when it signals validity; the parent maintains
  // the set of currently-invalid editors, and the form is
  // valid iff the set is empty.
  //
  // The editors keep their own text buffer for cursor-stability
  // reasons (see SettingsForm.tsx) and only call onChange when
  // the buffer parses cleanly — so without this lift, the
  // parent's settingsDraft holds the LAST valid parsed object
  // while the textarea shows unparseable text. Save would then
  // be enabled and would send the stale-but-valid draft instead
  // of the text on screen — confusing UX ("my save succeeded but
  // it's not what I had typed"). We disable Save whenever any
  // child editor signals invalid.
  //
  // The previous shape was a single boolean replaced wholesale
  // on every signal. That worked while exactly one editor was
  // mounted (today: the no-schema FreeformJsonEditor), but
  // raced once B6.2 ships settings_schema with multiple object-
  // typed fields: editor A signalling invalid would be
  // immediately overwritten by editor B signalling valid, and
  // the parent's bit would reflect only the last signal rather
  // than the conjunction. Pinning to per-editor keys lets each
  // editor's signal coexist; the conjunction (set is empty) is
  // the parent's source of truth.
  const [settingsInvalidKeys, setSettingsInvalidKeys] = useState<Set<string>>(
    () => new Set(),
  );
  const settingsFormValid = settingsInvalidKeys.size === 0;
  // handleSettingsValidity is memoised so its identity is stable
  // across renders. The child editors capture it once and re-use
  // it for the rest of their lifetime; identity churn would not
  // affect correctness (the editors guard with a ref against
  // stale-closure cleanup, see SettingsForm.tsx) but a stable
  // identity avoids the redundant-effect-rerun that would
  // follow an inline arrow function.
  const handleSettingsValidity = useCallback((key: string, valid: boolean) => {
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
  }, []);
  const [confirmUninstall, setConfirmUninstall] = useState(false);
  const [upgradeTargetId, setUpgradeTargetId] = useState<string | null>(null);
  // settingsResetKey is the parent contract that lets SettingsForm
  // (and the uncontrolled JSON textareas inside it) re-seed their
  // internal text buffers from the new value prop. The textareas
  // are intentionally uncontrolled so the user can type through
  // mid-stream invalid JSON without the parent re-render erasing
  // their cursor; the trade-off is that a parent-driven reset
  // (Discard, save success, cross-tab refetch) won't propagate on
  // its own. We bump this counter at every reset site, and pass
  // it as React's `key` on SettingsForm — forcing a remount and a
  // fresh seed. Cheap because SettingsForm holds no expensive
  // state of its own, and Discard explicitly DOES want focus to
  // reset (it's a "go back to clean slate" action).
  const [settingsResetKey, setSettingsResetKey] = useState(0);

  // Sync the draft with the server state on first load + after a
  // successful mutation (the cache row changes -> useEffect
  // resets the draft). settingsTouched guards against trampling
  // a user's in-progress edit when the cache invalidates from
  // some other tab.
  //
  // We deliberately do NOT bump settingsResetKey unconditionally
  // here: the save-success path already bumps it in
  // settingsMutation.onSuccess (so the textarea re-seeds from
  // the canonical server response), and then onSettled triggers
  // an invalidation/refetch that re-enters this effect with a
  // fresh install.data reference. If we bumped here too, every
  // successful save would remount SettingsForm 2–3 times in
  // quick succession (initial onSuccess bump + onSettled-driven
  // refetch bump + any background refetch). Each remount tears
  // down the JSON textareas' internal buffers, briefly flashes
  // the editor, and forces the parent's settingsFormValid back
  // to its initial state. Cheap individually, wasteful in
  // aggregate. The fix: gate the bump on whether the draft is
  // already in sync with the canonical document. JSON.stringify
  // is the cheapest deep-equal we can justify here (settings
  // are small documents — keys are user-typed schema names, not
  // pathological structures), and the canonical-order issue
  // doesn't apply because both sides come from the same
  // setSettingsDraft path on the previous tick.
  // BUG_0001 (round-4) — the previous shape called
  // setSettingsDraft(next) unconditionally and only gated the
  // resetKey bump on sameAsDraft. That looks innocuous when
  // install.data.settings is a stable reference (React Query
  // caches the deserialised object), but install.data.settings
  // CAN be null/undefined (the Go side's installationView.Settings
  // is map[string]any without omitempty, and a nil Go map
  // marshals as JSON null). The `?? {}` fallback then synthesises
  // a NEW {} ref on every effect run; settingsDraft is in the
  // dep array, so the unconditional setState scheduled an update
  // with a new ref → re-render → effect re-fires → new {} ref →
  // setState → re-render → infinite loop → max-update-depth
  // crash. The fix is to gate setSettingsDraft itself on
  // !sameAsDraft so the unchanged-document case is a true no-op.
  // (This also removes the redundant single extra render that
  // happened in the non-null case for the same reason — even a
  // referentially-new but semantically-equal `next` no longer
  // triggers a setState.)
  useEffect(() => {
    if (install.data && !settingsTouched) {
      const next = install.data.settings ?? {};
      const sameAsDraft =
        JSON.stringify(next) === JSON.stringify(settingsDraft);
      if (!sameAsDraft) {
        setSettingsDraft(next);
        setSettingsResetKey((k) => k + 1);
      }
    }
  }, [install.data, settingsTouched, settingsDraft]);

  const settingsMutation = useMutation({
    mutationFn: (next: Record<string, unknown>) =>
      api.updateMarketplaceInstallationSettings(installId!, next),
    onMutate: async (next) => {
      // Optimistic update: capture the previous snapshot so we
      // can restore it on error, then stage the new settings in
      // cache. onSettled below invalidates so the next useQuery
      // render syncs to the server's canonical view — important
      // for the error path where the rolled-back snapshot may
      // already be stale from another tab / a server-side mutate.
      await qc.cancelQueries({
        queryKey: ["marketplace", "installation", installId],
      });
      const prev = qc.getQueryData<MarketplaceInstallation>([
        "marketplace",
        "installation",
        installId,
      ]);
      if (prev) {
        qc.setQueryData<MarketplaceInstallation>(
          ["marketplace", "installation", installId],
          { ...prev, settings: next },
        );
      }
      return { prev };
    },
    onError: (_err, _next, ctx) => {
      if (ctx?.prev) {
        qc.setQueryData(
          ["marketplace", "installation", installId],
          ctx.prev,
        );
      }
    },
    onSuccess: (res) => {
      qc.setQueryData<MarketplaceInstallation>(
        ["marketplace", "installation", installId],
        res.installation,
      );
      qc.invalidateQueries({ queryKey: ["marketplace", "installations"] });
      setSettingsTouched(false);
      // Re-mount SettingsForm so the JSON-textarea editors
      // re-seed from the canonical server document. Without
      // this, the textarea would keep showing the user's last
      // pre-save draft text (which happens to equal the saved
      // value today, but only because we send the draft as the
      // payload — if the server ever normalises / canonicalises
      // settings on write, the cache would diverge from the
      // textarea).
      setSettingsResetKey((k) => k + 1);
    },
    onSettled: () => {
      // Always refetch the installation row after the mutation
      // settles (success or error). On error, the cache holds
      // the rolled-back snapshot which may itself be stale; on
      // success, the onSuccess setQueryData already wrote the
      // server response but a background refetch keeps the cache
      // honest if another tab raced the same mutation.
      qc.invalidateQueries({
        queryKey: ["marketplace", "installation", installId],
      });
    },
  });

  const upgradeMutation = useMutation({
    mutationFn: (input: {
      from_version_id: string;
      to_version_id: string;
      keep_settings: boolean;
    }) =>
      api.upgradeMarketplaceInstallation(installId!, {
        from_version_id: input.from_version_id,
        to_version_id: input.to_version_id,
        keep_settings: input.keep_settings,
      }),
    onSuccess: (res) => {
      qc.setQueryData<MarketplaceInstallation>(
        ["marketplace", "installation", installId],
        res.installation,
      );
      qc.invalidateQueries({ queryKey: ["marketplace", "installations"] });
      setUpgradeTargetId(null);
    },
    onSettled: () => {
      // Parity with settingsMutation: always refetch the
      // installation row after the mutation settles. The success
      // path's setQueryData above is correct for the common case,
      // but on error the cache holds the pre-upgrade snapshot
      // which may be stale relative to what the engine
      // committed before failing (e.g. settings normalised, a
      // partial advance the engine then rolled back, or another
      // tab racing the same install). A background refetch
      // converges to canonical with no UI cost.
      qc.invalidateQueries({
        queryKey: ["marketplace", "installation", installId],
      });
    },
  });

  const uninstallMutation = useMutation({
    mutationFn: () => api.uninstallMarketplaceExtension(installId!),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["marketplace", "installations"] });
      navigate("/marketplace/installed");
    },
  });

  if (!installId) return <p>No installation specified.</p>;
  if (install.isLoading) return <p>Loading…</p>;
  if (install.isError) {
    return (
      <p style={{ color: "#b91c1c" }}>
        Failed to load installation: {(install.error as Error).message}
      </p>
    );
  }
  const row = install.data!;
  const extension = ext.data?.extension;
  // ext.data.versions is the authoritative source for the upgrade
  // picker; we sort newest-first by published_at (NOT SemVer —
  // see sortVersionsByPublishedDesc rationale).
  const allVersions = ext.data?.versions
    ? sortVersionsByPublishedDesc(ext.data.versions)
    : [];
  const installedVersion =
    allVersions.find((v) => v.id === row.extension_version_id) ?? null;
  // Upgrade panel must only surface versions that are strictly
  // newer than the installed one (by published_at). A previous
  // implementation included every non-yanked non-current version,
  // which meant a tenant already on the most-recent publish would
  // see older versions in the "A newer version is available"
  // panel and could accidentally downgrade — risking settings
  // schema incompatibility, removed permissions, or workflow
  // drift the upgrade flow can't unwind. We compare timestamps
  // (same ordering basis as sortVersionsByPublishedDesc) rather
  // than SemVer because publishers may backport patches: 1.0.4
  // shipped chronologically AFTER 1.1.0 is still "newer" from a
  // "what the publisher last released" perspective.
  //
  // If installedVersion is null (e.g. the installed row points
  // at a version that was hard-deleted from the catalog, which
  // shouldn't happen but is defensible), the upgrade panel
  // collapses rather than showing every available version as a
  // candidate upgrade target with no anchor.
  const installedPublishedAt = installedVersion
    ? new Date(installedVersion.published_at).getTime()
    : null;
  const upgradableVersions =
    installedPublishedAt === null
      ? []
      : allVersions.filter((v) => {
          if (v.id === row.extension_version_id) return false;
          if (v.yanked) return false;
          const t = new Date(v.published_at).getTime();
          if (!Number.isFinite(t)) return false;
          return t > installedPublishedAt;
        });
  const upgradeTarget = upgradeTargetId
    ? allVersions.find((v) => v.id === upgradeTargetId) ?? null
    : null;

  const onSaveSettings = () => {
    setSettingsError(null);
    // Round-8 BUG_0001: defense-in-depth re-check of the
    // settings-form validity signal at the top of onSaveSettings,
    // mirroring the round-7 ANALYSIS_0002 guard in
    // InstallExtensionDialog.onConfirm. The Save button is
    // already disabled on `!settingsFormValid` (see line ~580),
    // but `<button disabled>` is a UI gate, not a data-path
    // gate — accessibility tools firing synthetic clicks,
    // programmatic invocation (e2e tests calling onSaveSettings
    // directly, third-party scripts wiring keyboard shortcuts),
    // and future refactors that swap the disabled prop for a
    // styling-only class can all bypass it. Since the parent's
    // settingsDraft retains the last valid parsed object while
    // the textarea shows mid-stream unparseable text (see the
    // uncontrolled-with-buffer pattern documented in
    // SettingsForm.tsx), submitting through any of those side
    // doors would silently send the stale-but-valid draft
    // instead of the bytes on screen — confusing UX ("my save
    // succeeded but it's not what I had typed"). Closing this
    // at the data path makes the validity signal load-bearing
    // for correctness, not just for the visual disabled state.
    if (!settingsFormValid) {
      setSettingsError(
        "Fix the settings JSON before saving — the textarea contents are not parseable.",
      );
      return;
    }
    // No schema yet (see InstallExtensionDialog for the rationale)
    // — once B6.2 ships the schema endpoint, fetch it here and
    // run validateAgainstSchema before mutating.
    const schema = null;
    if (schema) {
      const err = validateAgainstSchema(schema, settingsDraft);
      if (err) {
        setSettingsError(err);
        return;
      }
    }
    settingsMutation.mutate(settingsDraft);
  };

  return (
    <section>
      <div style={{ marginBottom: 12 }}>
        <Link
          to="/marketplace/installed"
          style={{ fontSize: 13, color: "#6b7280" }}
        >
          ← Installed extensions
        </Link>
      </div>

      <header
        style={{
          display: "flex",
          gap: 12,
          alignItems: "flex-start",
          marginBottom: 16,
          flexWrap: "wrap",
        }}
      >
        <div style={{ flex: 1, minWidth: 0 }}>
          <h1 style={{ margin: 0 }}>
            {extension ? extension.display_name : row.extension_id}
          </h1>
          <div style={{ color: "#6b7280", fontSize: 14, marginTop: 4 }}>
            {extension && (
              <Link to={`/marketplace/extensions/${extension.id}`}>
                {extension.name}
              </Link>
            )}
            {installedVersion && (
              <span style={{ marginLeft: 8 }}>· v{installedVersion.version}</span>
            )}
          </div>
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <Badge variant={installStatusVariant(row.status)} size="md">
            {installStatusLabel(row.status)}
          </Badge>
        </div>
      </header>

      {row.failure_reason && (
        <Card style={{ marginBottom: 16, borderColor: "#fca5a5" }}>
          <CardContent style={{ padding: 12 }}>
            <strong style={{ color: "#b91c1c" }}>Last failure:</strong>{" "}
            <span style={{ fontFamily: "monospace", fontSize: 13 }}>
              {row.failure_reason}
            </span>
          </CardContent>
        </Card>
      )}

      <div style={{ display: "grid", gap: 16 }}>
        <Card>
          <CardContent style={{ padding: 16 }}>
            <h3 style={{ marginTop: 0 }}>Installation details</h3>
            <DetailRow
              label="Install ID"
              value={
                <code style={{ fontSize: 12 }}>{row.id}</code>
              }
            />
            <DetailRow
              label="Installed at"
              value={formatTimestamp(row.installed_at)}
            />
            <DetailRow
              label="Last updated"
              value={formatTimestamp(row.updated_at)}
            />
            <DetailRow label="Webhook base" value={row.webhook_base || "—"} />
            {row.last_health_check_at && (
              <DetailRow
                label="Health check"
                value={`${formatTimestamp(row.last_health_check_at)}${
                  row.last_health_check_status
                    ? ` · ${row.last_health_check_status}`
                    : ""
                }`}
              />
            )}
            {installedVersion && (
              <DetailRow
                label="Bundle"
                value={`${formatBundleSize(installedVersion.bundle_size_bytes)} · sha256:${installedVersion.bundle_hash.slice(0, 12)}…`}
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardContent style={{ padding: 16 }}>
            <h3 style={{ marginTop: 0 }}>Settings</h3>
            <p style={{ color: "#6b7280", fontSize: 13, marginTop: 0 }}>
              The extension's runtime reads these settings every time it
              dispatches a webhook or evaluates a hook. Changes go live
              immediately after saving.
            </p>
            <SettingsForm
              // key forces remount when the parent legitimately
              // resets the draft (Discard, save success, cross-
              // tab refetch). See settingsResetKey docstring for
              // the full contract.
              key={settingsResetKey}
              schema={null}
              value={settingsDraft}
              onChange={(next) => {
                setSettingsDraft(next);
                setSettingsTouched(true);
              }}
              onValidityChange={handleSettingsValidity}
              disabled={settingsMutation.isPending}
            />
            {settingsError && (
              <p
                style={{
                  color: "#b91c1c",
                  margin: "8px 0 0",
                  fontSize: 13,
                }}
              >
                {settingsError}
              </p>
            )}
            {settingsMutation.isError && (
              <p
                style={{
                  color: "#b91c1c",
                  margin: "8px 0 0",
                  fontSize: 13,
                }}
              >
                Save failed:{" "}
                {(settingsMutation.error as Error).message}
              </p>
            )}
            <div
              style={{
                marginTop: 12,
                display: "flex",
                gap: 8,
                justifyContent: "flex-end",
              }}
            >
              <Button
                variant="outline"
                disabled={!settingsTouched || settingsMutation.isPending}
                onClick={() => {
                  setSettingsDraft(row.settings ?? {});
                  setSettingsTouched(false);
                  setSettingsError(null);
                  // Round-7 ANALYSIS_0004: explicitly clear
                  // settingsInvalidKeys on Discard rather than
                  // relying on the child editor's unmount-
                  // cleanup effect to fire `onValidityChange(
                  // key, true)`. The implicit path is sound
                  // today (React commits cleanup effects of
                  // the outgoing tree before the new tree
                  // mounts, so the resetKey bump on the next
                  // line tears the old editors down first; and
                  // the Save button's `!settingsTouched` check
                  // is also false here, so even a momentarily
                  // stale invalid-keys set can't enable Save).
                  // But: the chain (a) couples Discard's
                  // correctness to the editor's cleanup
                  // contract, (b) silently breaks if a future
                  // refactor swaps SettingsForm for a
                  // component that doesn't fire the
                  // cleanup signal, and (c) is non-obvious to
                  // a reader who has to follow the chain
                  // through SettingsForm.tsx's effect to
                  // convince themselves Discard is safe. Doing
                  // the reset explicitly here makes Discard's
                  // post-condition local and obvious: after
                  // this handler runs, the parent's draft +
                  // touched + error + validity all return to
                  // the clean-slate state regardless of what
                  // the child editors do. The unmount cleanup
                  // remains correct (and load-bearing for
                  // other reset paths like version swap), but
                  // is no longer the SOURCE of truth for the
                  // Discard reset's correctness.
                  setSettingsInvalidKeys(new Set());
                  // Re-mount SettingsForm so the JSON-textarea
                  // editors discard their internal buffer and
                  // re-seed from row.settings. The uncontrolled-
                  // textarea pattern they use intentionally
                  // doesn't auto-sync on prop changes (see
                  // SettingsForm.tsx for rationale) — this is
                  // the parent-side half of that contract.
                  setSettingsResetKey((k) => k + 1);
                }}
              >
                Discard changes
              </Button>
              <Button
                variant="primary"
                disabled={
                  !settingsTouched ||
                  !settingsFormValid ||
                  settingsMutation.isPending
                }
                onClick={onSaveSettings}
              >
                {settingsMutation.isPending ? "Saving…" : "Save settings"}
              </Button>
            </div>
          </CardContent>
        </Card>

        {upgradableVersions.length === 0 ? (
          // Affirmative "you're current" panel. Avoids the
          // ambiguity of a silently-absent upgrade card (users
          // wondered "is the upgrade flow broken or am I current?").
          // Only rendered for non-uninstalled rows — uninstalled
          // installations have no upgrade path by definition.
          row.status !== "uninstalled" && installedVersion ? (
            <Card>
              <CardContent style={{ padding: 16 }}>
                <h3 style={{ marginTop: 0 }}>Upgrade</h3>
                <p style={{ color: "#6b7280", fontSize: 13, marginTop: 0 }}>
                  You are already on the latest approved version
                  (v{installedVersion.version}, published{" "}
                  {formatTimestamp(installedVersion.published_at)}). New
                  publishes will surface here automatically.
                </p>
              </CardContent>
            </Card>
          ) : null
        ) : (
          <Card>
            <CardContent style={{ padding: 16 }}>
              <h3 style={{ marginTop: 0 }}>Upgrade</h3>
              <p style={{ color: "#6b7280", fontSize: 13, marginTop: 0 }}>
                A newer version is available. Upgrades preserve settings by
                default; you'll be prompted to migrate the document if the
                new version requires it.
              </p>
              <table
                style={{
                  width: "100%",
                  borderCollapse: "collapse",
                  fontSize: 14,
                }}
              >
                <tbody>
                  {upgradableVersions.slice(0, 5).map((v) => (
                    <tr
                      key={v.id}
                      style={{ borderTop: "1px solid #e5e7eb" }}
                    >
                      <td style={{ padding: "8px 0" }}>
                        <strong>v{v.version}</strong>
                        {extension?.listed_version === v.version && (
                          <Badge
                            variant="success"
                            size="xs"
                            style={{ marginLeft: 6 }}
                          >
                            DEFAULT
                          </Badge>
                        )}
                        <div style={{ fontSize: 12, color: "#6b7280" }}>
                          Published {formatTimestamp(v.published_at)} ·{" "}
                          {formatBundleSize(v.bundle_size_bytes)}
                        </div>
                      </td>
                      <td
                        style={{ padding: "8px 0", textAlign: "right" }}
                      >
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => setUpgradeTargetId(v.id)}
                        >
                          Upgrade to v{v.version}
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </CardContent>
          </Card>
        )}

        <Card>
          <CardContent style={{ padding: 16 }}>
            <h3 style={{ marginTop: 0, color: "#b91c1c" }}>Uninstall</h3>
            <p style={{ fontSize: 13, color: "#6b7280" }}>
              Removes the registry rows the extension installed and tears
              down its webhook subscriptions. The install row is preserved
              for audit purposes (status flips to <code>uninstalled</code>).
              Reinstalling later starts fresh.
            </p>
            <Button
              variant="destructive"
              onClick={() => setConfirmUninstall(true)}
              disabled={
                row.status === "uninstalled" || uninstallMutation.isPending
              }
            >
              Uninstall extension
            </Button>
            {uninstallMutation.isError && (
              <p
                style={{
                  color: "#b91c1c",
                  marginTop: 8,
                  fontSize: 13,
                }}
              >
                Uninstall failed:{" "}
                {(uninstallMutation.error as Error).message}
              </p>
            )}
          </CardContent>
        </Card>
      </div>

      {confirmUninstall && (
        <ControlledModal
          open
          onClose={() => {
            if (uninstallMutation.isPending) return;
            setConfirmUninstall(false);
          }}
          title="Uninstall extension?"
        >
          <p>
            This will tear down the extension's webhook subscriptions and
            mark the install row as uninstalled. The action cannot be
            undone — reinstalling will start with default settings.
          </p>
          <div
            style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}
          >
            <Button
              variant="outline"
              onClick={() => setConfirmUninstall(false)}
              disabled={uninstallMutation.isPending}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => uninstallMutation.mutate()}
              disabled={uninstallMutation.isPending}
            >
              {uninstallMutation.isPending ? "Uninstalling…" : "Uninstall"}
            </Button>
          </div>
        </ControlledModal>
      )}

      {upgradeTarget && installedVersion && (
        <UpgradeDialog
          installed={installedVersion}
          target={upgradeTarget}
          isPending={upgradeMutation.isPending}
          error={upgradeMutation.error as Error | null}
          onConfirm={(keepSettings) =>
            upgradeMutation.mutate({
              from_version_id: installedVersion.id,
              to_version_id: upgradeTarget.id,
              keep_settings: keepSettings,
            })
          }
          onClose={() => {
            if (upgradeMutation.isPending) return;
            setUpgradeTargetId(null);
          }}
        />
      )}
    </section>
  );
}

function UpgradeDialog({
  installed,
  target,
  isPending,
  error,
  onConfirm,
  onClose,
}: {
  installed: MarketplaceExtensionVersion;
  target: MarketplaceExtensionVersion;
  isPending: boolean;
  error: Error | null;
  onConfirm: (keepSettings: boolean) => void;
  onClose: () => void;
}) {
  const addedPerms = target.permissions_required.filter(
    (p) => !installed.permissions_required.includes(p),
  );
  const addedFeatures = target.features_required.filter(
    (f) => !installed.features_required.includes(f),
  );
  return (
    <ControlledModal
      open
      onClose={onClose}
      title={`Upgrade v${installed.version} → v${target.version}`}
    >
      <div style={{ display: "grid", gap: 12 }}>
        <p style={{ margin: 0, color: "#4b5563" }}>
          Settings are preserved by default. If the new version requires a
          migrated settings document, cancel and follow the publisher's
          migration guide before re-running this upgrade.
        </p>
        <DetailRow
          label="Bundle"
          value={`${formatBundleSize(target.bundle_size_bytes)} · ${
            target.bundle_signature ? "signed" : "unsigned"
          }`}
        />
        {(addedPerms.length > 0 || addedFeatures.length > 0) && (
          <div>
            <strong>New requirements in v{target.version}:</strong>
            {addedFeatures.length > 0 && (
              <div style={{ marginTop: 4 }}>
                <span style={{ color: "#6b7280", fontSize: 12 }}>
                  Tenant features:
                </span>{" "}
                {addedFeatures.map((f) => (
                  <Badge
                    key={f}
                    variant="warning"
                    style={{ marginLeft: 4 }}
                  >
                    {f}
                  </Badge>
                ))}
              </div>
            )}
            {addedPerms.length > 0 && (
              <div style={{ marginTop: 4 }}>
                <span style={{ color: "#6b7280", fontSize: 12 }}>
                  Platform permissions:
                </span>{" "}
                {addedPerms.map((p) => (
                  <Badge
                    key={p}
                    variant="warning"
                    style={{ marginLeft: 4 }}
                  >
                    {p}
                  </Badge>
                ))}
              </div>
            )}
          </div>
        )}
        {error && (
          <p style={{ color: "#b91c1c", margin: 0, fontSize: 13 }}>
            Upgrade failed: {error.message}
          </p>
        )}
        <div
          style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}
        >
          <Button variant="outline" onClick={onClose} disabled={isPending}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={() => onConfirm(true)}
            disabled={isPending}
          >
            {isPending ? "Upgrading…" : "Upgrade & keep settings"}
          </Button>
        </div>
      </div>
    </ControlledModal>
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
        gridTemplateColumns: "160px 1fr",
        gap: 8,
        padding: "6px 0",
        fontSize: 14,
      }}
    >
      <span style={{ color: "#6b7280" }}>{label}</span>
      <span>{value}</span>
    </div>
  );
}
