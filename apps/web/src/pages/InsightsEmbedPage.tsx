// Phase L deferred — Insights embed renderer.
//
// Standalone page (no app chrome, no auth) that fetches a dashboard
// using a long-lived bearer token and renders each widget's result.
// Mounted at /embed/{token} so a tenant operator can iframe it into
// any external surface. The owning tenant's rate-limit bucket
// applies, not the caller's IP.

import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";

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

export function InsightsEmbedPage() {
  const { token } = useParams<{ token: string }>();
  const [data, setData] = useState<EmbedResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!token) {
      setError("missing token");
      return;
    }
    fetchEmbed(token)
      .then(setData)
      .catch((err: Error) => setError(err.message));
  }, [token]);

  if (error) {
    return (
      <div className="p-6">
        <h1 className="text-xl font-semibold mb-2">Embed unavailable</h1>
        <p className="text-red-600">{error}</p>
      </div>
    );
  }
  if (!data) {
    return <div className="p-6">Loading…</div>;
  }

  return (
    <div className="p-6">
      <h1 className="text-2xl font-semibold mb-2">{data.dashboard.name}</h1>
      {data.dashboard.description && (
        <p className="text-gray-600 mb-4">{data.dashboard.description}</p>
      )}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {data.widgets.map((w) => (
          <div key={w.widget_id} className="border rounded p-4">
            {w.error ? (
              <p className="text-red-600 text-sm">{w.error}</p>
            ) : (
              <pre className="text-xs overflow-auto">
                {JSON.stringify(w.result?.rows ?? [], null, 2)}
              </pre>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
