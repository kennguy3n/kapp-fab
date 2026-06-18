// Insights embed renderer.
//
// Standalone page (no app chrome, no auth) that fetches a dashboard
// using a long-lived bearer token and renders each widget's result.
// Mounted at /embed/{token} so a tenant operator can iframe it into
// any external surface. The owning tenant's rate-limit bucket
// applies, not the caller's IP.

import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import type { ReportResult } from "@kapp/client";
import { EmptyState, Skeleton } from "@kapp/ui";
import { BarChart3 } from "lucide-react";
import { Viz } from "../components/insights/Charts";

interface EmbedWidget {
  widget_id: string;
  query_id?: string;
  position?: Record<string, unknown>;
  result?: { rows: Array<Record<string, unknown>> };
  cache_hit?: boolean;
  expires_at?: string;
  error?: string;
}

interface EmbedResponse {
  dashboard: {
    id: string;
    name: string;
    description?: string;
  };
  widgets: EmbedWidget[];
  embed_id: string;
}

// fetchEmbed talks directly to the unauth endpoint without the
// authenticated client (which assumes a tenant cookie). Keeps the
// embed self-contained so it can be iframed without leaking session
// state into the host page.
async function fetchEmbed(token: string): Promise<EmbedResponse> {
  const res = await fetch(`/api/v1/insights/embed/${encodeURIComponent(token)}`);
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`embed fetch failed: ${res.status} ${body}`);
  }
  return res.json();
}

// The embed payload carries only the row data per widget; derive the
// column order from the rows so the shared Viz table can render
// humanized headers (union of keys, first-seen order).
function toReportResult(rows: Array<Record<string, unknown>>): ReportResult {
  const columns: string[] = [];
  const seen = new Set<string>();
  for (const row of rows) {
    for (const key of Object.keys(row)) {
      if (!seen.has(key)) {
        seen.add(key);
        columns.push(key);
      }
    }
  }
  return { columns, rows };
}

export function InsightsEmbedPage() {
  const { token } = useParams<{ token: string }>();
  const [data, setData] = useState<EmbedResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [missingToken, setMissingToken] = useState(false);

  useEffect(() => {
    if (!token) {
      setMissingToken(true);
      return;
    }
    fetchEmbed(token)
      .then(setData)
      .catch((err: Error) => setError(err.message));
  }, [token]);

  if (missingToken || error) {
    return (
      <div className="min-h-screen bg-bg p-6">
        <div className="mx-auto max-w-md">
          <EmptyState
            title="This view isn’t available"
            description={
              missingToken
                ? "The embed link is missing its access token. Re-copy the share link and try again."
                : "We couldn’t load this dashboard. The share link may have expired or been revoked."
            }
          />
        </div>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="min-h-screen bg-bg p-6">
        <Skeleton className="h-8 w-64" />
        <div className="mt-6 grid grid-cols-1 gap-4 md:grid-cols-2">
          <Skeleton className="h-64 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-screen bg-bg p-6">
      <header className="mb-6">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          {data.dashboard.name}
        </h1>
        {data.dashboard.description && (
          <p className="mt-1 text-sm text-fg-muted">
            {data.dashboard.description}
          </p>
        )}
      </header>

      {data.widgets.length === 0 ? (
        <EmptyState
          icon={<BarChart3 className="h-6 w-6" aria-hidden />}
          title="Nothing to show yet"
          description="This dashboard doesn’t have any charts."
        />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          {data.widgets.map((w) => {
            const rows = w.result?.rows ?? [];
            return (
              <div
                key={w.widget_id}
                className="rounded-lg border border-border bg-bg-elevated p-4"
              >
                {w.error ? (
                  <p className="text-sm text-danger">
                    This chart couldn’t be loaded right now.
                  </p>
                ) : rows.length === 0 ? (
                  <p className="py-6 text-center text-sm text-fg-muted">
                    No data to display.
                  </p>
                ) : (
                  <Viz vizType="table" result={toReportResult(rows)} />
                )}
              </div>
            );
          })}
        </div>
      )}

      <footer className="mt-8 text-center text-xs text-fg-subtle">
        Powered by Kapp Insights
      </footer>
    </div>
  );
}
