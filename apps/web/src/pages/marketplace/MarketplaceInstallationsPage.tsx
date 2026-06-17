import { useMemo } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueries, useQuery } from "@tanstack/react-query";
// useQuery is still used by the installations list above; we
// deliberately dropped the per-row useQuery (was N+1 against
// listMarketplaceVersions for data already in extQueries).
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { PackageOpen } from "lucide-react";
import { api } from "../../lib/api";
import type {
  MarketplaceExtension,
  MarketplaceExtensionVersion,
  MarketplaceInstallation,
  MarketplaceListInstallationsResponse,
} from "@kapp/client";
import {
  formatTimestamp,
  installStatusLabel,
  installStatusVariant,
} from "./lib";

/**
 * MarketplaceInstallationsPage lists every install row for the
 * current tenant. Backed by GET /api/v1/marketplace/installations
 * which RLS-isolates rows to the requesting tenant.
 *
 * Each row links to the per-install detail / settings editor at
 * /marketplace/installed/:id. Disabled / failed / uninstalled
 * rows are shown alongside active ones so the operator can see
 * the full history; the status badge carries the colour signal.
 *
 * The extension display name is denormalised at render time by
 * fetching each unique extension_id via useQueries — keeps the
 * list endpoint cheap (no JOIN) while giving the UI a friendly
 * label.
 */
export function MarketplaceInstallationsPage() {
  const navigate = useNavigate();
  const installations = useQuery<MarketplaceListInstallationsResponse>({
    queryKey: ["marketplace", "installations"],
    queryFn: () => api.listMarketplaceInstallations(),
  });

  // Distinct extension IDs we need names for. useMemo so the
  // useQueries below doesn't re-key on every render.
  const extIds = useMemo(() => {
    const set = new Set<string>();
    (installations.data?.items ?? []).forEach((r) => set.add(r.extension_id));
    return [...set];
  }, [installations.data]);

  // Fan-out: one cached lookup per unique extension. React Query
  // dedupes against the per-extension cache so a user navigating
  // from the detail page already has the row.
  const extQueries = useQueries({
    queries: extIds.map((id) => ({
      queryKey: ["marketplace", "extension", id],
      queryFn: () => api.getMarketplaceExtension(id),
    })),
  });

  // Build extId -> Extension AND extId -> Versions lookups once
  // per render. Failed queries are dropped so a per-row 404
  // doesn't take the whole list down — the row just renders the
  // bare ID. We piggyback on getMarketplaceExtension's response
  // (which already includes versions[] from the same
  // listApprovedVersions backend path that GET .../versions uses)
  // rather than firing a second per-row useQuery against
  // listMarketplaceVersions — that was an N+1 round-trip with
  // zero additional data, since the two endpoints return the
  // exact same approved-non-yanked version list.
  const extLookup: Record<string, MarketplaceExtension | undefined> = {};
  const versionsLookup: Record<
    string,
    MarketplaceExtensionVersion[] | undefined
  > = {};
  extQueries.forEach((q, i) => {
    const id = extIds[i];
    if (q.data) {
      extLookup[id] = q.data.extension;
      versionsLookup[id] = q.data.versions;
    }
  });

  return (
    <section className="flex flex-col gap-6">
      <header>
        <Eyebrow>Marketplace</Eyebrow>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
          Installed extensions
        </h1>
        <p className="mt-1 max-w-prose text-sm text-fg-muted">
          Extensions currently or previously installed for this workspace.{" "}
          <Link
            to="/marketplace"
            className="font-medium text-accent hover:underline"
          >
            Browse the marketplace
          </Link>{" "}
          to add more.
        </p>
      </header>

      {installations.isLoading && (
        <div className="flex flex-col gap-2" aria-hidden>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-12 w-full rounded-lg" />
          ))}
        </div>
      )}
      {installations.isError && (
        <div className="rounded-lg border border-border p-8 text-center">
          <p className="text-sm font-medium text-fg">
            We couldn’t load your installed extensions.
          </p>
          <p className="mt-1 text-xs text-fg-muted">
            {(installations.error as Error).message}
          </p>
          <Button
            variant="outline"
            className="mt-3"
            onClick={() => installations.refetch()}
          >
            Try again
          </Button>
        </div>
      )}

      {installations.isSuccess &&
        (installations.data.items.length === 0 ? (
          <EmptyState
            icon={<PackageOpen className="h-6 w-6" aria-hidden />}
            title="No extensions installed yet"
            description="No extensions are installed yet. Browse the marketplace to find one."
            action={
              <Button onClick={() => navigate("/marketplace")}>
                Browse the marketplace
              </Button>
            }
          />
        ) : (
          <Table className="text-sm">
            <TableHeader>
              <TableRow>
                <TableHead>Extension</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Installed</TableHead>
                <TableHead>Last health check</TableHead>
                <TableHead>{""}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {installations.data.items.map((row) => (
                <InstallationRow
                  key={row.id}
                  row={row}
                  ext={extLookup[row.extension_id]}
                  versions={versionsLookup[row.extension_id]}
                />
              ))}
            </TableBody>
          </Table>
        ))}
    </section>
  );
}

function InstallationRow({
  row,
  ext,
  versions,
}: {
  row: MarketplaceInstallation;
  ext: MarketplaceExtension | undefined;
  versions: MarketplaceExtensionVersion[] | undefined;
}) {
  return (
    <TableRow>
      <TableCell className="align-top">
        {ext ? (
          <Link
            to={`/marketplace/extensions/${ext.id}`}
            className="font-medium text-fg hover:text-accent hover:underline"
          >
            {ext.display_name}
          </Link>
        ) : (
          <span className="text-fg-muted">Unknown extension</span>
        )}
        {ext?.author && (
          <div className="text-xs text-fg-muted">{ext.author}</div>
        )}
      </TableCell>
      <TableCell className="align-top">
        <Badge variant={installStatusVariant(row.status)}>
          {installStatusLabel(row.status)}
        </Badge>
        {row.failure_reason && (
          <div className="mt-1 max-w-[280px] text-xs text-danger">
            {row.failure_reason}
          </div>
        )}
      </TableCell>
      <TableCell className="align-top">{renderVersion(row, ext, versions)}</TableCell>
      <TableCell className="align-top">{formatTimestamp(row.installed_at)}</TableCell>
      <TableCell className="align-top">
        {row.last_health_check_at ? (
          <span>
            {formatTimestamp(row.last_health_check_at)}
            {row.last_health_check_status && (
              <span className="ml-1.5 text-xs text-fg-muted">
                · {row.last_health_check_status}
              </span>
            )}
          </span>
        ) : (
          <span className="text-fg-subtle">—</span>
        )}
      </TableCell>
      <TableCell className="align-top">
        <Link
          to={`/marketplace/installed/${row.id}`}
          className="font-medium text-accent hover:underline"
        >
          Manage →
        </Link>
      </TableCell>
    </TableRow>
  );
}

// renderVersion picks the right display for the install row's
// extension_version_id. When the extension's version list is
// loaded and the install's version is in it, we show the SemVer
// label plus an "Update available" badge if the catalogue's
// default version is newer. When the version isn't resolvable
// we fall back to a truncated UUID so the table never breaks.
function renderVersion(
  row: MarketplaceInstallation,
  ext: MarketplaceExtension | undefined,
  versions: MarketplaceExtensionVersion[] | undefined,
): React.ReactNode {
  if (!ext) {
    return <span className="text-fg-subtle">—</span>;
  }
  const installed = versions?.find((v) => v.id === row.extension_version_id);
  if (!installed) {
    return <span className="text-fg-subtle">—</span>;
  }
  // "Update available" needs a published_at-timestamp comparison,
  // not a SemVer-string inequality, for the same reason the
  // upgrade panel on InstallationDetailPage (BUG_0002 in round 1)
  // uses timestamps: publishers may ship a backport patch (e.g.
  // 1.0.4 published chronologically AFTER 1.1.0), and even if the
  // strings don't match, an installation pinned on the LATER
  // publish should not be flagged as "Update available" — doing
  // so would invite a tenant admin to "upgrade" themselves into
  // an older publish that the upgrade-detail page would
  // immediately tell them they're already past.
  //
  // The listed_version field stores a SemVer string; we have to
  // resolve it back to a Version row to read its published_at.
  // If the publisher hasn't marked any version as the listed
  // default (soft-launch), there's no anchor to compare against,
  // and the badge collapses — same logic as the install
  // dialog gate on MarketplaceExtensionDetailPage.
  const listed = ext.listed_version
    ? versions?.find((v) => v.version === ext.listed_version)
    : undefined;
  const ti = new Date(installed.published_at).getTime();
  const tl = listed ? new Date(listed.published_at).getTime() : NaN;
  const isBehind =
    Number.isFinite(ti) && Number.isFinite(tl) && tl > ti;
  return (
    <span>
      v{installed.version}
      {isBehind && (
        <Badge variant="warning" className="ml-1.5">
          Update available
        </Badge>
      )}
    </span>
  );
}


