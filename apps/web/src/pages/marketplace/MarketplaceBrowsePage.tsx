import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  Badge,
  Button,
  Card,
  CardContent,
  EmptyState,
  Eyebrow,
  Input,
  Select,
  Skeleton,
} from "@kapp/ui";
import { ArrowRight, Store } from "lucide-react";
import { api } from "../../lib/api";
import type {
  MarketplaceCategory,
  MarketplaceExtension,
  MarketplaceListExtensionsResponse,
} from "@kapp/client";
import {
  categoryLabel,
  extensionStatusLabel,
  extensionStatusVariant,
  formatTimestamp,
  MARKETPLACE_CATEGORIES,
} from "./lib";
import { RatingStars } from "./RatingStars";

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
  // Empty string = "All categories". The Select is an exact-match
  // server-side filter, so it is NOT debounced (a single discrete
  // choice, not free text).
  const [categoryFilter, setCategoryFilter] = useState<MarketplaceCategory | "">(
    "",
  );
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
  const qCategory = categoryFilter || undefined;

  const q = useQuery<MarketplaceListExtensionsResponse>({
    queryKey: [
      "marketplace",
      "extensions",
      { q: qSearch, publisher: qPublisher, category: qCategory },
    ],
    queryFn: () =>
      api.listMarketplaceExtensions({
        q: qSearch,
        publisher: qPublisher,
        category: qCategory,
      }),
  });

  const items = q.data?.items ?? [];
  const hasFilter =
    debouncedSearch.trim() !== "" ||
    debouncedPublisher.trim() !== "" ||
    categoryFilter !== "";

  return (
    <section className="flex flex-col gap-6">
      <header>
        <Eyebrow>Marketplace</Eyebrow>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
          Browse extensions
        </h1>
        <p className="mt-1 max-w-prose text-sm text-fg-muted">
          Discover and install extensions for your workspace. Visit{" "}
          <Link
            to="/marketplace/installed"
            className="font-medium text-accent hover:underline"
          >
            Installed extensions
          </Link>{" "}
          to manage what’s already running.
        </p>
      </header>

      <div className="flex flex-wrap gap-3">
        <Input
          type="search"
          placeholder="Search extensions…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search marketplace extensions"
          className="min-w-[240px] flex-1"
        />
        <Input
          type="text"
          placeholder="Publisher (e.g. acme)"
          value={publisherFilter}
          onChange={(e) => setPublisherFilter(e.target.value)}
          aria-label="Filter by publisher"
          className="min-w-[200px]"
        />
        <Select
          value={categoryFilter}
          onChange={(e) =>
            setCategoryFilter(e.target.value as MarketplaceCategory | "")
          }
          aria-label="Filter by category"
          className="min-w-[180px]"
        >
          <option value="">All categories</option>
          {MARKETPLACE_CATEGORIES.map((c) => (
            <option key={c.value} value={c.value}>
              {c.label}
            </option>
          ))}
        </Select>
      </div>

      {q.isLoading && (
        <div
          className="grid grid-cols-[repeat(auto-fill,minmax(320px,1fr))] gap-4"
          aria-hidden
        >
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-44 w-full rounded-lg" />
          ))}
        </div>
      )}

      {q.isError && (
        <div className="rounded-lg border border-border p-8 text-center">
          <p className="text-sm font-medium text-fg">
            We couldn’t load the marketplace.
          </p>
          <p className="mt-1 text-xs text-fg-muted">{(q.error as Error).message}</p>
          <Button variant="outline" className="mt-3" onClick={() => q.refetch()}>
            Try again
          </Button>
        </div>
      )}

      {q.isSuccess && items.length === 0 && (
        <EmptyState
          icon={<Store className="h-6 w-6" aria-hidden />}
          title={
            hasFilter
              ? "No extensions match your filter"
              : "No extensions published yet"
          }
          description={
            hasFilter
              ? "Try a different search term, category, or clear the publisher filter."
              : "There aren’t any extensions published in the marketplace yet."
          }
        />
      )}

      {q.isSuccess && items.length > 0 && (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(320px,1fr))] gap-4">
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
      className="group text-inherit no-underline"
      data-testid={`extension-card-${ext.id}`}
    >
      <Card className="flex h-full flex-col transition-shadow hover:shadow-md">
        <CardContent className="flex flex-1 flex-col pt-4">
          <div className="mb-3 flex items-start gap-3">
            <ExtensionIcon ext={ext} />
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-2">
                <strong className="truncate text-base text-fg">
                  {ext.display_name}
                </strong>
                <Badge variant={extensionStatusVariant(ext.status)}>
                  {extensionStatusLabel(ext.status)}
                </Badge>
              </div>
              <div className="mt-0.5 truncate text-xs text-fg-muted">
                {ext.author ? ext.author : ext.publisher}
                {ext.listed_version ? ` · v${ext.listed_version}` : ""}
              </div>
            </div>
          </div>
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <Badge variant="outline">{categoryLabel(ext.category)}</Badge>
            <RatingStars average={ext.rating_average} count={ext.rating_count} />
          </div>
          <p className="m-0 line-clamp-3 text-sm text-fg-muted">
            {ext.description || (
              <span className="italic text-fg-subtle">
                No description provided.
              </span>
            )}
          </p>
          <div className="mt-auto flex items-center justify-between pt-4 text-xs text-fg-muted">
            <span>Updated {formatTimestamp(ext.updated_at)}</span>
            <span className="inline-flex items-center gap-1 font-medium text-accent">
              View details
              <ArrowRight
                className="h-3.5 w-3.5 transition-transform group-hover:translate-x-0.5"
                aria-hidden
              />
            </span>
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
        className="h-10 w-10 shrink-0 rounded-lg bg-bg-muted object-cover"
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
  // Letter-tile fallback when no icon URL is set — first character of
  // display_name on the KChat accent so the catalogue stays on-brand
  // and the tile reads as a consistent app icon.
  const letter = (ext.display_name || ext.slug || "?").charAt(0).toUpperCase();
  return (
    <div
      aria-hidden
      className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-accent text-lg font-semibold text-accent-fg"
    >
      {letter}
    </div>
  );
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
