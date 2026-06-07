import { useState } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Badge,
  Button,
  Card,
  CardContent,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@kapp/ui";
import { api } from "../../lib/api";
import type {
  MarketplaceExtensionVersion,
  MarketplaceGetExtensionResponse,
  MarketplaceListInstallationsResponse,
} from "@kapp/client";
import {
  extensionStatusLabel,
  extensionStatusVariant,
  formatBundleSize,
  formatTimestamp,
  installStatusLabel,
  installStatusVariant,
  sortVersionsByPublishedDesc,
} from "./lib";
import { InstallExtensionDialog } from "./InstallExtensionDialog";

/**
 * MarketplaceExtensionDetailPage renders the per-extension
 * landing surface. Three tabs:
 *
 *   - Overview: description, publisher, author, license, links
 *   - Versions: every approved+non-yanked version with manifest
 *     counts (ktypes / workflows / agent tools / UI extensions /
 *     webhooks) plus the bundle size / signature posture.
 *   - Permissions: features_required and permissions_required
 *     drawn from the listed version's manifest so an installer
 *     can audit what the extension will request before clicking
 *     Install.
 *
 * The Install CTA is always anchored to the "listed_version"
 * (the publisher-declared default) — installing an older
 * version is a power-user move that goes through the Versions
 * tab's per-row "Install this version" link.
 */
export function MarketplaceExtensionDetailPage() {
  const { extId } = useParams<{ extId: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const detail = useQuery<MarketplaceGetExtensionResponse>({
    queryKey: ["marketplace", "extension", extId],
    queryFn: () => api.getMarketplaceExtension(extId!),
    enabled: !!extId,
  });

  // Cross-query: pull this tenant's installations so we can show
  // the "Already installed" pill on the detail page. The list
  // endpoint is cheap (per-tenant scan) and already cached when
  // the user navigated from the installed-list page.
  const installations = useQuery<MarketplaceListInstallationsResponse>({
    queryKey: ["marketplace", "installations"],
    queryFn: () => api.listMarketplaceInstallations(),
  });

  const [installVersionId, setInstallVersionId] = useState<string | null>(null);

  if (!extId) {
    return <p>No extension specified.</p>;
  }
  if (detail.isLoading) {
    return <p>Loading…</p>;
  }
  if (detail.isError) {
    return (
      <p className="text-danger">
        Failed to load extension: {(detail.error as Error).message}
      </p>
    );
  }
  const ext = detail.data!.extension;
  // The handler returns versions ordered DB-side by published_at
  // DESC, but we re-sort defensively in case a future B6.x ships
  // a different order. See lib.ts for why we use timestamp + not
  // SemVer ordering.
  const versions = sortVersionsByPublishedDesc(detail.data!.versions);

  // Listed version (publisher-declared default install target).
  // May be absent if the extension exists in the catalog but no
  // version is currently approved — in that case the Install
  // CTA is disabled.
  const listedVersion = versions.find((v) => v.version === ext.listed_version) ?? null;

  // Already-installed signal: an install row that points at this
  // extension and is in a non-terminal state. Uninstalled rows
  // are excluded because they're audit-only — the user is free
  // to reinstall the extension. Failed/disabled count as
  // "installed" so the CTA flips to "Manage" rather than
  // appearing to allow a duplicate install.
  const installRow = (installations.data?.items ?? []).find(
    (r) =>
      r.extension_id === ext.id &&
      r.status !== "uninstalled",
  );

  const installable =
    !installRow &&
    ext.status === "listed" &&
    !!listedVersion &&
    !listedVersion.yanked;

  return (
    <section>
      <div className="mb-3">
        <Link to="/marketplace" className="text-[13px] text-fg-muted">
          ← Marketplace
        </Link>
      </div>
      <header className="mb-4 flex items-start gap-4">
        <DetailIcon iconUrl={ext.icon_url} fallback={ext.display_name} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="m-0">{ext.display_name}</h1>
            <Badge variant={extensionStatusVariant(ext.status)}>
              {extensionStatusLabel(ext.status)}
            </Badge>
            {installRow && (
              <Badge variant={installStatusVariant(installRow.status)}>
                Installed · {installStatusLabel(installRow.status)}
              </Badge>
            )}
          </div>
          <div className="mt-1 text-sm text-fg-muted">
            {ext.name}
            {ext.author && ` · By ${ext.author}`}
            {ext.license && ` · ${ext.license}`}
          </div>
        </div>
        <div className="flex gap-2">
          {installRow ? (
            <Button
              variant="outline"
              onClick={() => navigate(`/marketplace/installed/${installRow.id}`)}
            >
              Manage install
            </Button>
          ) : (
            <Button
              variant="primary"
              disabled={!installable}
              onClick={() => listedVersion && setInstallVersionId(listedVersion.id)}
              title={
                !installable
                  ? ext.status !== "listed"
                    ? `Extension is ${ext.status}; new installs are not accepted.`
                    : !listedVersion
                      ? "No installable version is available."
                      : "Default version is yanked."
                  : undefined
              }
            >
              Install
            </Button>
          )}
        </div>
      </header>

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="versions">
            Versions ({versions.length})
          </TabsTrigger>
          <TabsTrigger value="permissions">Permissions</TabsTrigger>
        </TabsList>

        <TabsContent value="overview">
          <OverviewTab ext={ext} listedVersion={listedVersion} />
        </TabsContent>
        <TabsContent value="versions">
          <VersionsTab
            versions={versions}
            ext={ext}
            // Round-8 ANALYSIS_0004: deliberately a weaker gate than
            // the header CTA's `installable` (line ~109). The header
            // CTA installs the listed/default version, so its gate
            // bakes in `!!listedVersion && !listedVersion.yanked`. The
            // VersionsTab gate is a *prerequisite* (no install row +
            // ext.status==="listed") that enables per-row Install
            // buttons; each row then applies its own `!v.yanked`
            // check inside `VersionsTab` (see Install button below).
            // Naming the VersionsTab prop `canInstallVersions`
            // (instead of also calling it `installable`) makes the
            // distinction explicit so future maintainers don't
            // collapse them into one variable and accidentally hide
            // every per-row Install button when the listed default
            // happens to be yanked.
            canInstallVersions={!installRow && ext.status === "listed"}
            onInstall={(id) => setInstallVersionId(id)}
          />
        </TabsContent>
        <TabsContent value="permissions">
          <PermissionsTab listedVersion={listedVersion} />
        </TabsContent>
      </Tabs>

      {installVersionId &&
        (() => {
          // Anchor the dialog to whichever specific version the
          // user picked (header CTA always picks listedVersion,
          // Versions tab picks per-row). versions[] is the
          // authoritative array — the ID always originated from
          // it via setInstallVersionId, so .find() is guaranteed.
          // The previous `?? listedVersion` fallback masked the
          // case where listedVersion was null but a per-row
          // Versions-tab Install was clicked: the gate refused
          // to render, leaving the user with a silent no-op
          // button. Now we render whenever the picked version
          // resolves; if it somehow can't (defensive), we drop
          // the dialog rather than render with a wrong version.
          const picked = versions.find((v) => v.id === installVersionId);
          if (!picked) return null;
          return (
            <InstallExtensionDialog
              extension={ext}
              version={picked}
              onClose={() => setInstallVersionId(null)}
              onInstalled={(res) => {
                setInstallVersionId(null);
                // Invalidate both list queries so the
                // installed-list view and the cross-query
                // already-installed badge above reflect the new
                // row immediately.
                qc.invalidateQueries({ queryKey: ["marketplace", "installations"] });
                navigate(`/marketplace/installed/${res.installation.id}`);
              }}
            />
          );
        })()}
    </section>
  );
}

function DetailIcon({
  iconUrl,
  fallback,
}: {
  iconUrl: string | undefined;
  fallback: string;
}) {
  if (iconUrl) {
    return (
      <img
        src={iconUrl}
        alt=""
        width={64}
        height={64}
        className="rounded-xl bg-bg-muted object-cover"
        onError={(e) => {
          (e.currentTarget as HTMLImageElement).style.visibility = "hidden";
        }}
      />
    );
  }
  return (
    <div
      aria-hidden
      className="flex h-16 w-16 items-center justify-center rounded-xl bg-bg-muted text-[28px] font-bold text-fg"
    >
      {fallback.charAt(0).toUpperCase()}
    </div>
  );
}

function OverviewTab({
  ext,
  listedVersion,
}: {
  ext: MarketplaceGetExtensionResponse["extension"];
  listedVersion: MarketplaceExtensionVersion | null;
}) {
  return (
    <div className="mt-4 grid gap-4">
      <Card>
        <CardContent className="pt-4">
          <h3 className="mt-0">About</h3>
          {ext.description ? (
            <p className="whitespace-pre-wrap">{ext.description}</p>
          ) : (
            <p className="italic text-fg-subtle">
              No description provided.
            </p>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardContent className="pt-4">
          <h3 className="mt-0">Publisher</h3>
          <DetailRow label="Name" value={ext.publisher} />
          <DetailRow label="Author" value={ext.author || "—"} />
          <DetailRow label="License" value={ext.license || "—"} />
          {ext.homepage && (
            <DetailRow
              label="Homepage"
              value={
                <a href={ext.homepage} target="_blank" rel="noreferrer noopener">
                  {ext.homepage}
                </a>
              }
            />
          )}
          {ext.support_email && (
            <DetailRow
              label="Support"
              value={
                <a href={`mailto:${ext.support_email}`}>{ext.support_email}</a>
              }
            />
          )}
        </CardContent>
      </Card>
      {listedVersion && (
        <Card>
          <CardContent className="pt-4">
            <h3 className="mt-0">
              Default version{" "}
              <span className="font-normal text-fg-muted">
                v{listedVersion.version}
              </span>
            </h3>
            <DetailRow
              label="Published"
              value={formatTimestamp(listedVersion.published_at)}
            />
            <DetailRow
              label="Bundle size"
              value={formatBundleSize(listedVersion.bundle_size_bytes)}
            />
            <DetailRow
              label="Min Kapp version"
              value={listedVersion.min_kapp_version}
            />
            {listedVersion.max_kapp_version && (
              <DetailRow
                label="Max Kapp version"
                value={listedVersion.max_kapp_version}
              />
            )}
            <DetailRow
              label="Signed"
              value={
                listedVersion.bundle_signature
                  ? `${listedVersion.bundle_signature_key_id ?? "ed25519"} (${formatTimestamp(listedVersion.signed_at)})`
                  : "Unsigned"
              }
            />
          </CardContent>
        </Card>
      )}
    </div>
  );
}

function VersionsTab({
  versions,
  ext,
  canInstallVersions,
  onInstall,
}: {
  versions: MarketplaceExtensionVersion[];
  ext: MarketplaceGetExtensionResponse["extension"];
  // Whether per-row Install buttons may be shown at all. This is the
  // *enabling* precondition — each row still applies its own
  // `!v.yanked` check before rendering the button (see below). The
  // header CTA's `installable` flag is a different, stricter gate
  // baking in the listed default's yank state; do not unify them.
  canInstallVersions: boolean;
  onInstall: (versionId: string) => void;
}) {
  if (versions.length === 0) {
    return (
      <p className="mt-4 text-fg-muted">
        No approved versions are available.
      </p>
    );
  }
  return (
    <div className="mt-4">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Version</TableHead>
            <TableHead>Published</TableHead>
            <TableHead>Size</TableHead>
            <TableHead>Manifest</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>{""}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {versions.map((v) => (
            <TableRow key={v.id}>
              <TableCell>
                <strong>v{v.version}</strong>
                {ext.listed_version === v.version && (
                  <span className="ml-1.5 text-[11px] text-success">
                    DEFAULT
                  </span>
                )}
              </TableCell>
              <TableCell>{formatTimestamp(v.published_at)}</TableCell>
              <TableCell>{formatBundleSize(v.bundle_size_bytes)}</TableCell>
              <TableCell>
                <ManifestCounts version={v} />
              </TableCell>
              <TableCell>
                {v.yanked ? (
                  <Badge variant="danger" title={v.yanked_reason}>
                    Yanked
                  </Badge>
                ) : v.bundle_signature ? (
                  <Badge variant="success">Signed</Badge>
                ) : (
                  <Badge variant="outline">Unsigned</Badge>
                )}
              </TableCell>
              <TableCell>
                {canInstallVersions && !v.yanked && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => onInstall(v.id)}
                  >
                    Install
                  </Button>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function ManifestCounts({ version }: { version: MarketplaceExtensionVersion }) {
  const counts: Array<[string, number]> = [
    ["KTypes", version.ktypes_count],
    ["Workflows", version.workflows_count],
    ["Agent tools", version.agent_tools_count],
    ["UI exts", version.ui_extensions_count],
    ["Webhooks", version.webhooks_count],
  ];
  const nonZero = counts.filter(([, n]) => n > 0);
  if (nonZero.length === 0) return <span className="text-fg-subtle">—</span>;
  return (
    <span className="inline-flex flex-wrap gap-2">
      {nonZero.map(([label, n]) => (
        <span key={label} className="text-xs text-fg">
          {label}: <strong>{n}</strong>
        </span>
      ))}
    </span>
  );
}

function PermissionsTab({
  listedVersion,
}: {
  listedVersion: MarketplaceExtensionVersion | null;
}) {
  if (!listedVersion) {
    return (
      <p className="mt-4 text-fg-muted">
        No version is available to inspect permissions for.
      </p>
    );
  }
  return (
    <div className="mt-4 grid gap-4">
      <Card>
        <CardContent className="pt-4">
          <h3 className="mt-0">Required tenant features</h3>
          <PermissionList
            items={listedVersion.features_required}
            empty="This extension does not require any tenant feature flags."
          />
        </CardContent>
      </Card>
      <Card>
        <CardContent className="pt-4">
          <h3 className="mt-0">Required platform permissions</h3>
          <PermissionList
            items={listedVersion.permissions_required}
            empty="This extension does not request any platform permissions."
          />
          <p className="mt-3 text-xs text-fg-muted">
            Installing will grant the extension these permissions for this
            tenant. Permissions are pinned per version: an upgrade that adds
            permissions surfaces them in the upgrade dialog.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}

function PermissionList({ items, empty }: { items: string[]; empty: string }) {
  if (!items || items.length === 0) {
    return <p className="italic text-fg-subtle">{empty}</p>;
  }
  return (
    <ul className="m-0 pl-[18px]">
      {items.map((p) => (
        <li key={p} className="font-mono text-[13px]">
          {p}
        </li>
      ))}
    </ul>
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
    <div className="grid grid-cols-[160px_1fr] gap-2 py-1.5 text-sm">
      <span className="text-fg-muted">{label}</span>
      <span>{value}</span>
    </div>
  );
}
