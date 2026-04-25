import { useEffect, useMemo, useState } from "react";
import { useSearchParams, Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { SearchResult } from "@kapp/client";
import { api } from "../lib/api";

// SearchPage renders the /search route. The query is sourced from the
// URL (?q=...) so deep-links into a specific search work, and the
// input debounces edits into a trailing 250ms window before firing
// the API call so rapid typing does not flood the backend.
export function SearchPage() {
  const [params, setParams] = useSearchParams();
  const urlQ = params.get("q") ?? "";
  const [input, setInput] = useState(urlQ);
  const [debounced, setDebounced] = useState(urlQ);

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

  const grouped = useMemo(() => {
    const out = new Map<string, SearchResult[]>();
    for (const row of searchQuery.data?.results ?? []) {
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
  }, [searchQuery.data]);

  return (
    <section>
      <h1>Search</h1>
      <input
        autoFocus
        value={input}
        onChange={(e) => setInput(e.target.value)}
        placeholder="Search records by name, title, description, sku, email…"
        style={{
          width: "100%",
          padding: "8px 12px",
          fontSize: 16,
          border: "1px solid #d1d5db",
          borderRadius: 6,
          marginBottom: 16,
        }}
      />
      {debounced && searchQuery.isLoading && <div>Searching…</div>}
      {debounced && searchQuery.error && (
        <div style={{ color: "#b91c1c" }}>
          Search failed: {(searchQuery.error as Error).message}
        </div>
      )}
      {debounced && searchQuery.data && grouped.length === 0 && (
        <div>No results for "{debounced}".</div>
      )}
      {grouped.map(([ktype, rows]) => (
        <div key={ktype} style={{ marginBottom: 24 }}>
          <h2 style={{ fontSize: 14, textTransform: "uppercase", color: "#374151" }}>
            {ktype} ({rows.length})
          </h2>
          <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
            {rows.map((r) => (
              <li
                key={r.id}
                style={{ padding: "6px 0", borderBottom: "1px solid #f3f4f6" }}
              >
                <Link to={`/records/${ktype}/${r.id}`}>
                  {summaryOf(r)}
                </Link>{" "}
                <span style={{ color: "#9ca3af", fontSize: 11 }}>
                  rank {r.rank.toFixed(3)}
                </span>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </section>
  );
}

// summaryOf picks the most human-meaningful top-level field from the
// record payload to render in the result list. Falls back to the
// record id so every row is still clickable.
function summaryOf(r: SearchResult): string {
  const d = (r.data ?? {}) as Record<string, unknown>;
  const candidates = [
    "name",
    "title",
    "subject",
    "sku",
    "code",
    "email",
    "reference",
  ];
  for (const k of candidates) {
    const v = d[k];
    if (typeof v === "string" && v.trim().length > 0) return v;
  }
  return r.id;
}
