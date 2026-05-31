import { useEffect, useState } from "react";
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
  const extId = install.data?.extension_id;
  const ext = useQuery<MarketplaceGetExtensionResponse>({
    queryKey: ["marketplace", "extension", extId],
    queryFn: () => api.getMarketplaceExtension(extId!),
    enabled: !!extId,
  });

  // Available versions for the upgrade picker. Already-installed
  // version is excluded server-side from the upgrade target
  // gate, but we filter it from the picker UI too so the user
  // doesn't see a redundant "upgrade to current".
  const versions = useQuery({
    queryKey: ["marketplace", "extension-versions", extId],
    queryFn: () => api.listMarketplaceVersions(extId!),
    enabled: !!extId,
  });

  const [settingsDraft, setSettingsDraft] = useState<Record<string, unknown>>(
    {},
  );
  const [settingsTouched, setSettingsTouched] = useState(false);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  const [confirmUninstall, setConfirmUninstall] = useState(false);
  const [upgradeTargetId, setUpgradeTargetId] = useState<string | null>(null);

  // Sync the draft with the server state on first load + after a
  // successful mutation (the cache row changes -> useEffect
  // resets the draft). settingsTouched guards against trampling
  // a user's in-progress edit when the cache invalidates from
  // some other tab.
  useEffect(() => {
    if (install.data && !settingsTouched) {
      setSettingsDraft(install.data.settings ?? {});
    }
  }, [install.data, settingsTouched]);

  const settingsMutation = useMutation({
    mutationFn: (next: Record<string, unknown>) =>
      api.updateMarketplaceInstallationSettings(installId!, next),
    onMutate: async (next) => {
      // Optimistic update: capture the previous snapshot so we
      // can restore it on error, then stage the new settings in
      // cache. The mutation's onSettled invalidates so the next
      // useQuery render syncs to the server's canonical view.
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
  const allVersions = versions.data?.items
    ? sortVersionsByPublishedDesc(versions.data.items)
    : [];
  const installedVersion =
    allVersions.find((v) => v.id === row.extension_version_id) ?? null;
  const upgradableVersions = allVersions.filter(
    (v) => v.id !== row.extension_version_id && !v.yanked,
  );
  const upgradeTarget = upgradeTargetId
    ? allVersions.find((v) => v.id === upgradeTargetId) ?? null
    : null;

  const onSaveSettings = () => {
    setSettingsError(null);
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
              schema={null}
              value={settingsDraft}
              onChange={(next) => {
                setSettingsDraft(next);
                setSettingsTouched(true);
              }}
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
                }}
              >
                Discard changes
              </Button>
              <Button
                variant="primary"
                disabled={!settingsTouched || settingsMutation.isPending}
                onClick={onSaveSettings}
              >
                {settingsMutation.isPending ? "Saving…" : "Save settings"}
              </Button>
            </div>
          </CardContent>
        </Card>

        {upgradableVersions.length > 0 && (
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
