// Phase L deferred — Insights external data sources.
//
// CRUD page for `insights_data_sources`. Connection strings are
// posted as plaintext over the wire (the API layer encrypts at rest)
// and never re-displayed: subsequent edits to a row leave the
// connection string blank to signal "keep the existing credential".
// A test button exercises POST /test which opens a one-shot pool and
// runs SELECT 1 so the operator can distinguish a typo from a stale
// credential.

import { useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type {
  InsightsDataSource,
  InsightsDataSourceInput,
} from "@kapp/client";
import { api } from "../lib/api";

const DEFAULT_INPUT: InsightsDataSourceInput = {
  name: "",
  description: "",
  dialect: "postgres",
  connection_string: "",
  enabled: true,
};

export function InsightsDataSourcesPage() {
  const qc = useQueryClient();
  const list = useQuery({
    queryKey: ["insights", "data-sources"],
    queryFn: () => api.listInsightsDataSources(),
  });
  const [draft, setDraft] = useState<InsightsDataSourceInput>(DEFAULT_INPUT);
  const [editing, setEditing] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<Record<string, string>>({});

  const upsert = useMutation({
    mutationFn: (input: InsightsDataSourceInput) =>
      editing
        ? api.updateInsightsDataSource(editing, input)
        : api.createInsightsDataSource(input),
    onSuccess: () => {
      setDraft(DEFAULT_INPUT);
      setEditing(null);
      qc.invalidateQueries({ queryKey: ["insights", "data-sources"] });
    },
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteInsightsDataSource(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["insights", "data-sources"] }),
  });

  const test = useMutation({
    mutationFn: (id: string) => api.testInsightsDataSource(id),
    onSuccess: (res, id) =>
      setTestResult((prev) => ({
        ...prev,
        [id]: res.ok ? "ok" : "failed",
      })),
    onError: (err: unknown, id) =>
      setTestResult((prev) => ({
        ...prev,
        [id]: err instanceof Error ? err.message : "failed",
      })),
  });

  return (
    <div className="p-6">
      <h1 className="text-2xl font-semibold mb-4">Data sources</h1>
      <p className="text-sm text-gray-600 mb-4">
        Read-only Postgres connections that can be queried from saved
        queries via <code>source: "external:&lt;id&gt;"</code>. Connection
        strings are encrypted at rest with the per-tenant HKDF key.
      </p>

      <section className="mb-8 border rounded p-4">
        <h2 className="text-lg font-medium mb-2">
          {editing ? "Edit data source" : "Add data source"}
        </h2>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            upsert.mutate(draft);
          }}
          className="grid grid-cols-1 md:grid-cols-2 gap-3"
        >
          <input
            className="border p-2 rounded"
            placeholder="Name"
            value={draft.name}
            onChange={(e) =>
              setDraft({ ...draft, name: e.target.value })
            }
            required
          />
          <input
            className="border p-2 rounded"
            placeholder="Description"
            value={draft.description ?? ""}
            onChange={(e) =>
              setDraft({ ...draft, description: e.target.value })
            }
          />
          <input
            className="border p-2 rounded col-span-full"
            placeholder="postgres://user:password@host:5432/dbname"
            value={draft.connection_string ?? ""}
            onChange={(e) =>
              setDraft({ ...draft, connection_string: e.target.value })
            }
          />
          <label className="flex items-center gap-2">
            <input
              type="checkbox"
              checked={draft.enabled ?? true}
              onChange={(e) =>
                setDraft({ ...draft, enabled: e.target.checked })
              }
            />
            <span>Enabled</span>
          </label>
          <div className="col-span-full flex gap-2">
            <button
              type="submit"
              className="bg-blue-600 text-white px-4 py-2 rounded"
              disabled={upsert.isPending}
            >
              {editing ? "Save changes" : "Create data source"}
            </button>
            {editing && (
              <button
                type="button"
                className="border px-4 py-2 rounded"
                onClick={() => {
                  setEditing(null);
                  setDraft(DEFAULT_INPUT);
                }}
              >
                Cancel
              </button>
            )}
          </div>
          {upsert.isError && (
            <p className="col-span-full text-red-600 text-sm">
              {(upsert.error as Error).message}
            </p>
          )}
        </form>
      </section>

      <section>
        <h2 className="text-lg font-medium mb-2">Existing data sources</h2>
        {list.isLoading ? (
          <p>Loading…</p>
        ) : list.error ? (
          <p className="text-red-600">{(list.error as Error).message}</p>
        ) : (
          <table className="w-full border-collapse">
            <thead>
              <tr className="text-left border-b">
                <th className="py-2">Name</th>
                <th className="py-2">Dialect</th>
                <th className="py-2">Enabled</th>
                <th className="py-2">Test</th>
                <th className="py-2">Actions</th>
              </tr>
            </thead>
            <tbody>
              {(list.data?.data_sources ?? []).map(
                (ds: InsightsDataSource) => (
                  <tr key={ds.id} className="border-b">
                    <td className="py-2">{ds.name}</td>
                    <td className="py-2">{ds.dialect}</td>
                    <td className="py-2">
                      {ds.enabled ? "yes" : "no"}
                    </td>
                    <td className="py-2">
                      <button
                        type="button"
                        className="text-sm border px-2 py-1 rounded"
                        onClick={() => test.mutate(ds.id)}
                      >
                        Test
                      </button>
                      {testResult[ds.id] && (
                        <span className="ml-2 text-xs text-gray-600">
                          {testResult[ds.id]}
                        </span>
                      )}
                    </td>
                    <td className="py-2">
                      <button
                        type="button"
                        className="text-sm border px-2 py-1 rounded mr-2"
                        onClick={() => {
                          setEditing(ds.id);
                          setDraft({
                            name: ds.name,
                            description: ds.description ?? "",
                            dialect: ds.dialect,
                            // Connection string stays blank on edit;
                            // server keeps the existing encrypted value
                            // when the field is empty.
                            connection_string: "",
                            enabled: ds.enabled,
                          });
                        }}
                      >
                        Edit
                      </button>
                      <button
                        type="button"
                        className="text-sm text-red-600 border px-2 py-1 rounded"
                        onClick={() => {
                          if (window.confirm(`Delete ${ds.name}?`)) {
                            remove.mutate(ds.id);
                          }
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                )
              )}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
