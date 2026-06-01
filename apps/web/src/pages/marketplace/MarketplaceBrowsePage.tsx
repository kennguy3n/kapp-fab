import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Badge, Card, CardContent, Input } from "@kapp/ui";
import { api } from "../../lib/api";
import type {
  MarketplaceExtension,
  MarketplaceListExtensionsResponse,
} from "@kapp/client";
import {
  extensionStatusLabel,
  extensionStatusVariant,
  formatTimestamp,
} from "./lib";

/**
 * MarketplaceBrowsePage is the tenant-facing catalogue view.
 * Backed by GET /api/v1/marketplace/extensions which only ever
 * surfaces ExtensionStatus = "listed" rows; the publisher and
 * search-text filters are applied server-side, the debounced
 * client-side query input below feeds the `q` query param.
 *
 * Rendering choices:
 *   - Card grid (not a table) because every row carries a
 *     description paragraph that doesn't fit a tabular cell at
 *     readable typography.
 *   - Each card links to /marketplace/extensions/:id which is
 *     the detail view (versions, install CTA, etc.).
 *   - "No extensions match" empty-state distinguishes "catalog
 *     is empty" from "your filter matched nothing" so a user
 *     who just typed knows to clear the search.
 */
export function MarketplaceBrowsePage() {
  // Search text + publisher filter are debounced into the query
  // key via useMemo so a fast-typing user doesn't fire one HTTP
  // round trip per keystroke. The keyboard event itself drives
  // the local input state immediately for responsive feedback;
  // the actual `useQuery` only re-runs when the trimmed value
  // changes after a 250 ms quiet period (handled by a
  // setTimeout in useEffect below).
  const [search, setSearch] = useState("");
  const [publisherFilter, setPublisherFilter] = useState("");
  const debouncedSearch = useDebounced(search, 250);
  const debouncedPublisher = useDebounced(publisherFilter, 250);
  // Round-9 ANALYSIS_0001: normalise the filter values ONCE and
  // use the same normalised form in both the cache key and the
  // queryFn. Pre-fix, the key used `.trim()` (which keeps `""`
  // for an empty filter) while the queryFn used `.trim() ||
  // undefined` (which collapses `""` to absent). That divergence
  // was harmless today because the page never passes `undefined`
  // into the key — both sides ALWAYS see `""` for an empty
  // filter, so the response under that key never raced a
  // response under a hypothetical `undefined` key. But the
  // contract "cache key and queryFn agree on the request shape"
  // is load-bearing for React Query's identity guarantees, and
  // a future refactor that read `qSearch` / `qPublisher` from
  // state with conditional spread (a common pattern when a
  // third filter ships) would silently fork cache identity off
  // the wire identity. Single source of truth closes that.
  const qSearch = debouncedSearch.trim() || undefined;
  const qPublisher = debouncedPublisher.trim() || undefined;

  const q = useQuery<MarketplaceListExtensionsResponse>({
    queryKey: ["marketplace", "extensions", { q: qSearch, publisher: qPublisher }],
    queryFn: () =>
      api.listMarketplaceExtensions({ q: qSearch, publisher: qPublisher }),
  });

  const items = q.data?.items ?? [];
  const hasFilter = debouncedSearch.trim() !== "" || debouncedPublisher.trim() !== "";

  return (
    <section>
      <header style={{ marginBottom: 16 }}>
        <h1 style={{ marginBottom: 4 }}>Marketplace</h1>
        <p style={{ color: "#6b7280" }}>
          Browse and install extensions for this tenant. Visit{" "}
          <Link to="/marketplace/installed">Installed extensions</Link> to
          manage what's already running.
        </p>
      </header>

      <div
        style={{
          display: "flex",
          flexWrap: "wrap",
          gap: 12,
          marginBottom: 16,
        }}
      >
        <Input
          type="search"
          placeholder="Search extensions…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search marketplace extensions"
          style={{ minWidth: 240, flex: 1 }}
        />
        <Input
          type="text"
          placeholder="Publisher (e.g. acme)"
          value={publisherFilter}
          onChange={(e) => setPublisherFilter(e.target.value)}
          aria-label="Filter by publisher slug"
          style={{ minWidth: 200 }}
        />
      </div>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load marketplace: {(q.error as Error).message}
        </p>
      )}

      {q.isSuccess && items.length === 0 && (
        <Card>
          <CardContent style={{ padding: 32, textAlign: "center" }}>
            <p style={{ color: "#6b7280" }}>
              {hasFilter
                ? "No extensions match your filter."
                : "No extensions are currently published in the marketplace."}
            </p>
          </CardContent>
        </Card>
      )}

      {q.isSuccess && items.length > 0 && (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fill, minmax(320px, 1fr))",
            gap: 16,
          }}
        >
          {items.map((ext) => (
            <ExtensionCard key={ext.id} ext={ext} />
          ))}
        </div>
      )}
    </section>
  );
}

function ExtensionCard({ ext }: { ext: MarketplaceExtension }) {
  return (
    <Link
      to={`/marketplace/extensions/${ext.id}`}
      style={{ textDecoration: "none", color: "inherit" }}
      data-testid={`extension-card-${ext.id}`}
    >
      <Card style={{ height: "100%", cursor: "pointer" }}>
        <CardContent style={{ padding: 16 }}>
          <div
            style={{
              display: "flex",
              alignItems: "flex-start",
              gap: 12,
              marginBottom: 12,
            }}
          >
            <ExtensionIcon ext={ext} />
            <div style={{ flex: 1, minWidth: 0 }}>
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: 8,
                  flexWrap: "wrap",
                }}
              >
                <strong style={{ fontSize: 16 }}>{ext.display_name}</strong>
                <Badge variant={extensionStatusVariant(ext.status)}>
                  {extensionStatusLabel(ext.status)}
                </Badge>
              </div>
              <div style={{ fontSize: 12, color: "#6b7280", marginTop: 2 }}>
                {ext.name}
                {ext.listed_version ? ` · v${ext.listed_version}` : ""}
              </div>
            </div>
          </div>
          <p
            style={{
              fontSize: 14,
              color: "#374151",
              margin: 0,
              display: "-webkit-box",
              WebkitLineClamp: 3,
              WebkitBoxOrient: "vertical",
              overflow: "hidden",
            }}
          >
            {ext.description || (
              <span style={{ color: "#9ca3af", fontStyle: "italic" }}>
                No description provided.
              </span>
            )}
          </p>
          <div
            style={{
              marginTop: 12,
              display: "flex",
              gap: 12,
              fontSize: 12,
              color: "#6b7280",
              flexWrap: "wrap",
            }}
          >
            {ext.author && <span>By {ext.author}</span>}
            {ext.license && <span>· {ext.license}</span>}
            <span>· Updated {formatTimestamp(ext.updated_at)}</span>
          </div>
        </CardContent>
      </Card>
    </Link>
  );
}

function ExtensionIcon({ ext }: { ext: MarketplaceExtension }) {
  if (ext.icon_url) {
    return (
      <img
        src={ext.icon_url}
        alt=""
        width={40}
        height={40}
        style={{
          borderRadius: 8,
          objectFit: "cover",
          background: "#f3f4f6",
        }}
        // Manifest validation pins this URL but the browser may
        // still 404 (asset deleted post-publish). Falling back
        // to a hidden state lets the layout collapse to text-
        // only rather than render a broken-image glyph.
        onError={(e) => {
          (e.currentTarget as HTMLImageElement).style.visibility = "hidden";
        }}
      />
    );
  }
  // Letter-tile fallback when no icon URL is set — first
  // character of display_name on a coloured background. The
  // colour is derived from the extension name so the same
  // extension always gets the same tile, providing a stable
  // visual anchor across renders.
  const letter = (ext.display_name || ext.slug || "?")
    .charAt(0)
    .toUpperCase();
  const hue = hashString(ext.name) % 360;
  return (
    <div
      aria-hidden
      style={{
        width: 40,
        height: 40,
        borderRadius: 8,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        background: `hsl(${hue}, 60%, 45%)`,
        color: "white",
        fontWeight: 600,
        fontSize: 18,
        flexShrink: 0,
      }}
    >
      {letter}
    </div>
  );
}

// Tiny deterministic string hash for the icon-fallback tile
// colour. FNV-1a 32-bit — collision rate is irrelevant since the
// only consequence is two extensions sharing a hue.
function hashString(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return Math.abs(h);
}

// useDebounced returns the latest `value` after `delay` ms of
// quiet — feeding the marketplace search input into useQuery via
// this prevents firing one request per keystroke.
function useDebounced<T>(value: T, delay: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delay);
    return () => clearTimeout(id);
  }, [value, delay]);
  return debounced;
}
