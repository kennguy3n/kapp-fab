import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
} from "react";
import { useSearchParams, Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { SearchResult } from "@kapp/client";
import { Button, EmptyState, Eyebrow, Input, Skeleton, cn } from "@kapp/ui";
import { AlertTriangle, Search, SearchX } from "lucide-react";
import { api } from "../lib/api";
import { ktypePlural, ktypeSingular, recordLabel } from "../lib/ktypeView";

const SEARCH_HINTS = [
  "Customers & contacts",
  "Deals & invoices",
  "Tickets",
  "Products by SKU",
];

/**
 * SearchPage renders the /search route. The query is sourced from the
 * URL (?q=...) so deep-links into a specific search work, and the
 * input debounces edits into a trailing 250ms window before firing
 * the API call so rapid typing does not flood the backend. Results
 * are grouped by record type under humanized section headers; arrow
 * keys move focus between the input and the result list for a
 * keyboard-first flow.
 */
export function SearchPage() {
  const [params, setParams] = useSearchParams();
  const urlQ = params.get("q") ?? "";
  const [input, setInput] = useState(urlQ);
  const [debounced, setDebounced] = useState(urlQ);
  const inputRef = useRef<HTMLInputElement>(null);
  const resultsRef = useRef<HTMLDivElement>(null);

  // Sync local input when the URL changes externally (e.g. the
  // global search box navigates /search?q=foo while the page is
  // already mounted). useState(urlQ) only runs on first mount, so
  // without this effect the input stays latched on the initial q
  // and the user's new query is silently discarded.
  useEffect(() => {
    setInput((cur) => (cur === urlQ ? cur : urlQ));
    setDebounced((cur) => (cur === urlQ ? cur : urlQ));
  }, [urlQ]);

  useEffect(() => {
    const id = window.setTimeout(() => {
      setDebounced(input);
      if (input) {
        setParams({ q: input }, { replace: true });
      } else {
        setParams({}, { replace: true });
      }
    }, 250);
    return () => window.clearTimeout(id);
  }, [input, setParams]);

  const searchQuery = useQuery({
    queryKey: ["search", debounced],
    queryFn: () => api.searchRecords({ q: debounced, limit: 100 }),
    enabled: debounced.length > 0,
  });

  const results = searchQuery.data?.results ?? [];

  const grouped = useMemo(() => {
    const out = new Map<string, SearchResult[]>();
    for (const row of results) {
      const bucket = out.get(row.ktype) ?? [];
      bucket.push(row);
      out.set(row.ktype, bucket);
    }
    // Sort KTypes by the top-ranked result in each group so the most
    // relevant domain bubbles up first.
    return [...out.entries()].sort((a, b) => {
      const aRank = a[1][0]?.rank ?? 0;
      const bRank = b[1][0]?.rank ?? 0;
      return bRank - aRank;
    });
  }, [results]);

  function focusResultAt(index: number) {
    const els = resultsRef.current?.querySelectorAll<HTMLAnchorElement>(
      "a[data-result]",
    );
    if (!els || els.length === 0) return;
    const clamped = Math.max(0, Math.min(index, els.length - 1));
    els[clamped].focus();
  }

  function onResultsKeyDown(e: ReactKeyboardEvent<HTMLDivElement>) {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const els = Array.from(
      resultsRef.current?.querySelectorAll<HTMLAnchorElement>("a[data-result]") ??
        [],
    );
    if (els.length === 0) return;
    e.preventDefault();
    const idx = els.indexOf(document.activeElement as HTMLAnchorElement);
    if (e.key === "ArrowDown") {
      focusResultAt(idx < 0 ? 0 : idx + 1);
    } else if (idx <= 0) {
      inputRef.current?.focus();
    } else {
      focusResultAt(idx - 1);
    }
  }

  const showResults = debounced.length > 0;

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-1">
        <Eyebrow>Workspace</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">Search</h1>
        <p className="max-w-prose text-sm text-fg-muted">
          Find anything across your workspace — customers, deals, invoices,
          tickets and products — and jump straight to it.
        </p>
      </header>

      <Input
        ref={inputRef}
        autoFocus
        type="search"
        value={input}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            focusResultAt(0);
          }
        }}
        leadingAddon={<Search className="h-4 w-4" aria-hidden />}
        placeholder="Search records by name, title, description, SKU, email…"
        aria-label="Search records"
        className="w-full"
      />

      {!showResults ? (
        <EmptyState
          icon={<Search className="h-6 w-6" aria-hidden />}
          title="Start typing to search"
          description="Search runs across every record you can see. Try a customer name, an invoice number, a product SKU or an email address."
          action={
            <div className="flex flex-wrap justify-center gap-2">
              {SEARCH_HINTS.map((hint) => (
                <span
                  key={hint}
                  className="rounded-pill bg-bg-muted px-3 py-1 text-xs text-fg-muted"
                >
                  {hint}
                </span>
              ))}
            </div>
          }
        />
      ) : searchQuery.isLoading ? (
        <SearchSkeleton />
      ) : searchQuery.isError ? (
        <EmptyState
          icon={<AlertTriangle className="h-6 w-6" aria-hidden />}
          title="Search failed"
          description={(searchQuery.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void searchQuery.refetch()}
              disabled={searchQuery.isFetching}
            >
              Try again
            </Button>
          }
        />
      ) : results.length === 0 ? (
        <EmptyState
          icon={<SearchX className="h-6 w-6" aria-hidden />}
          title={`No results for “${debounced}”`}
          description="Check the spelling, or try a shorter or more general term."
        />
      ) : (
        <div className="flex flex-col gap-6" ref={resultsRef} onKeyDown={onResultsKeyDown}>
          <p className="text-sm text-fg-muted" aria-live="polite">
            {results.length} {results.length === 1 ? "result" : "results"} for{" "}
            <span className="font-medium text-fg">“{debounced}”</span>
          </p>
          {grouped.map(([ktype, rows]) => (
            <div key={ktype} className="flex flex-col gap-2">
              <div className="flex items-baseline justify-between gap-2 border-b border-border pb-1.5">
                <h2 className="text-sm font-semibold text-fg">
                  {ktypePlural(ktype)}
                </h2>
                <span className="text-xs text-fg-subtle">
                  {rows.length} {rows.length === 1 ? "match" : "matches"}
                </span>
              </div>
              <ul className="flex flex-col">
                {rows.map((r) => (
                  <li key={r.id}>
                    <Link
                      data-result
                      to={`/records/${ktype}/${r.id}`}
                      className={cn(
                        "flex items-center justify-between gap-3 rounded-md px-3 py-2.5 text-sm transition-colors",
                        "hover:bg-bg-subtle focus-visible:bg-bg-subtle",
                        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-1 focus-visible:ring-offset-(--bg)",
                      )}
                    >
                      <span className="truncate font-medium text-fg">
                        {recordLabel(r)}
                      </span>
                      <span className="shrink-0 text-xs text-fg-subtle">
                        {ktypeSingular(ktype)}
                      </span>
                    </Link>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function SearchSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      {Array.from({ length: 2 }).map((_, group) => (
        <div key={group} className="flex flex-col gap-2">
          <Skeleton className="h-4 w-28" />
          {Array.from({ length: 3 }).map((_, row) => (
            <Skeleton key={row} className="h-10 w-full" />
          ))}
        </div>
      ))}
    </div>
  );
}
