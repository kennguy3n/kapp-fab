import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQueries, useQuery } from "@tanstack/react-query";
// useQuery is still used by the installations list above; we
// deliberately dropped the per-row useQuery (was N+1 against
// listMarketplaceVersions for data already in extQueries).
import { Badge, Card, CardContent } from "@kapp/ui";
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
    <section>
      <header style={{ marginBottom: 16 }}>
        <h1 style={{ marginBottom: 4 }}>Installed extensions</h1>
        <p style={{ color: "#6b7280" }}>
          Marketplace extensions currently or previously installed for this
          tenant. <Link to="/marketplace">Browse the marketplace</Link> to
          add more.
        </p>
      </header>

      {installations.isLoading && <p>Loading…</p>}
      {installations.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load installations:{" "}
          {(installations.error as Error).message}
        </p>
      )}

      {installations.isSuccess &&
        (installations.data.items.length === 0 ? (
          <Card>
            <CardContent style={{ padding: 32, textAlign: "center" }}>
              <p style={{ color: "#6b7280" }}>
                No extensions are installed yet.{" "}
                <Link to="/marketplace">Browse the marketplace</Link> to find
                one.
              </p>
            </CardContent>
          </Card>
        ) : (
          <table
            style={{
              width: "100%",
              borderCollapse: "collapse",
              fontSize: 14,
            }}
          >
            <thead>
              <tr style={{ textAlign: "left", color: "#6b7280" }}>
                <Th>Extension</Th>
                <Th>Status</Th>
                <Th>Version</Th>
                <Th>Installed</Th>
                <Th>Last health check</Th>
                <Th>{""}</Th>
              </tr>
            </thead>
            <tbody>
              {installations.data.items.map((row) => (
                <InstallationRow
                  key={row.id}
                  row={row}
                  ext={extLookup[row.extension_id]}
                  versions={versionsLookup[row.extension_id]}
                />
              ))}
            </tbody>
          </table>
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
    <tr style={{ borderTop: "1px solid #e5e7eb" }}>
      <Td>
        {ext ? (
          <Link to={`/marketplace/extensions/${ext.id}`}>
            <strong>{ext.display_name}</strong>
          </Link>
        ) : (
          <span style={{ fontFamily: "monospace", fontSize: 12 }}>
            {row.extension_id}
          </span>
        )}
        {ext && (
          <div style={{ fontSize: 12, color: "#6b7280" }}>{ext.name}</div>
        )}
      </Td>
      <Td>
        <Badge variant={installStatusVariant(row.status)}>
          {installStatusLabel(row.status)}
        </Badge>
        {row.failure_reason && (
          <div
            style={{
              fontSize: 12,
              color: "#b91c1c",
              marginTop: 4,
              maxWidth: 280,
            }}
          >
            {row.failure_reason}
          </div>
        )}
      </Td>
      <Td>{renderVersion(row, ext, versions)}</Td>
      <Td>{formatTimestamp(row.installed_at)}</Td>
      <Td>
        {row.last_health_check_at ? (
          <span>
            {formatTimestamp(row.last_health_check_at)}
            {row.last_health_check_status && (
              <span
                style={{ marginLeft: 6, color: "#6b7280", fontSize: 12 }}
              >
                · {row.last_health_check_status}
              </span>
            )}
          </span>
        ) : (
          <span style={{ color: "#9ca3af" }}>—</span>
        )}
      </Td>
      <Td>
        <Link to={`/marketplace/installed/${row.id}`}>Manage →</Link>
      </Td>
    </tr>
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
    return (
      <span style={{ fontFamily: "monospace", fontSize: 12 }}>
        {row.extension_version_id}
      </span>
    );
  }
  const installed = versions?.find((v) => v.id === row.extension_version_id);
  if (!installed) {
    return (
      <span style={{ fontFamily: "monospace", fontSize: 12 }}>
        {row.extension_version_id.slice(0, 8)}…
      </span>
    );
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
        <Badge variant="warning" style={{ marginLeft: 6 }}>
          Update available
        </Badge>
      )}
    </span>
  );
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th
      style={{
        textAlign: "left",
        padding: "8px 12px",
        fontWeight: 500,
        fontSize: 12,
        textTransform: "uppercase",
        letterSpacing: 0.4,
      }}
    >
      {children}
    </th>
  );
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "12px", verticalAlign: "top" }}>{children}</td>;
}
