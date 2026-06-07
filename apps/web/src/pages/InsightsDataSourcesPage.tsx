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
import {
  Button,
  ConfirmDialog,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
  const [deleteTarget, setDeleteTarget] = useState<{
    id: string;
    name: string;
  } | null>(null);

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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights", "data-sources"] });
      setDeleteTarget(null);
    },
    onError: () => setDeleteTarget(null),
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
      <p className="text-sm text-fg-muted mb-4">
        Read-only Postgres connections that can be queried from saved
        queries via <code>source: "external:&lt;id&gt;"</code>. Connection
        strings are encrypted at rest with the per-tenant HKDF key.
      </p>

      <section className="mb-8 border border-border rounded p-4">
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
          <Input
            placeholder="Name"
            value={draft.name}
            onChange={(e) =>
              setDraft({ ...draft, name: e.target.value })
            }
            required
          />
          <Input
            placeholder="Description"
            value={draft.description ?? ""}
            onChange={(e) =>
              setDraft({ ...draft, description: e.target.value })
            }
          />
          <Input
            className="col-span-full"
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
            <Button type="submit" disabled={upsert.isPending}>
              {editing ? "Save changes" : "Create data source"}
            </Button>
            {editing && (
              <Button
                type="button"
                variant="outline"
                onClick={() => {
                  setEditing(null);
                  setDraft(DEFAULT_INPUT);
                }}
              >
                Cancel
              </Button>
            )}
          </div>
          {upsert.isError && (
            <p className="col-span-full text-danger text-sm">
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
          <p className="text-danger">{(list.error as Error).message}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Dialect</TableHead>
                <TableHead>Enabled</TableHead>
                <TableHead>Test</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(list.data?.data_sources ?? []).map(
                (ds: InsightsDataSource) => (
                  <TableRow key={ds.id}>
                    <TableCell>{ds.name}</TableCell>
                    <TableCell>{ds.dialect}</TableCell>
                    <TableCell>{ds.enabled ? "yes" : "no"}</TableCell>
                    <TableCell>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        onClick={() => test.mutate(ds.id)}
                      >
                        Test
                      </Button>
                      {testResult[ds.id] && (
                        <span className="ml-2 text-xs text-fg-muted">
                          {testResult[ds.id]}
                        </span>
                      )}
                    </TableCell>
                    <TableCell>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        className="mr-2"
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
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="ghost"
                        onClick={() =>
                          setDeleteTarget({ id: ds.id, name: ds.name })
                        }
                      >
                        Delete
                      </Button>
                    </TableCell>
                  </TableRow>
                )
              )}
            </TableBody>
          </Table>
        )}
      </section>

      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(o) => {
          if (!o && !remove.isPending) setDeleteTarget(null);
        }}
        title={
          deleteTarget
            ? `Delete ${deleteTarget.name}?`
            : "Delete data source?"
        }
        description="This removes the data source and its stored connection credential. Saved queries that reference it will stop running."
        confirmLabel="Delete"
        destructive
        loading={remove.isPending}
        onConfirm={() => {
          if (deleteTarget) remove.mutate(deleteTarget.id);
        }}
      />
    </div>
  );
}
