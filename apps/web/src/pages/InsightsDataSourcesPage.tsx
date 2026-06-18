// Insights — external data sources.
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
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { Database } from "lucide-react";
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
  const [testResult, setTestResult] = useState<
    Record<string, { ok: boolean; message?: string }>
  >({});
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
        [id]: { ok: res.ok },
      })),
    onError: (err: unknown, id) =>
      setTestResult((prev) => ({
        ...prev,
        [id]: {
          ok: false,
          message: err instanceof Error ? err.message : undefined,
        },
      })),
  });

  const dataSources = list.data?.data_sources ?? [];

  return (
    <section className="flex flex-col gap-6">
      <header>
        <Eyebrow>Insights</Eyebrow>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
          Data sources
        </h1>
        <p className="mt-1 max-w-prose text-sm text-fg-muted">
          Connect a read-only PostgreSQL database so your saved queries can
          report on data that lives outside Kapp. Credentials are encrypted and
          never shown again once saved.
        </p>
      </header>

      <section className="rounded-lg border border-border bg-bg-elevated p-5">
        <h2 className="mb-4 text-base font-semibold text-fg">
          {editing ? "Edit data source" : "Add a data source"}
        </h2>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            upsert.mutate(draft);
          }}
          className="flex flex-col gap-4"
        >
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
            <Field label="Name" required>
              <Input
                placeholder="e.g. Warehouse reporting DB"
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                required
              />
            </Field>
            <Field label="Description" help="Optional — what this connects to.">
              <Input
                placeholder="e.g. Nightly export of order history"
                value={draft.description ?? ""}
                onChange={(e) =>
                  setDraft({ ...draft, description: e.target.value })
                }
              />
            </Field>
          </div>
          <Field
            label="Connection string"
            help={
              editing
                ? "Leave blank to keep the existing credential."
                : "We encrypt this at rest and never display it again."
            }
          >
            <Input
              type="password"
              autoComplete="off"
              placeholder="postgres://user:password@host:5432/dbname"
              value={draft.connection_string ?? ""}
              onChange={(e) =>
                setDraft({ ...draft, connection_string: e.target.value })
              }
            />
          </Field>
          <label className="flex w-fit items-center gap-2 text-sm text-fg">
            <input
              type="checkbox"
              className="h-4 w-4 rounded border-border accent-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
              checked={draft.enabled ?? true}
              onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
            />
            <span>Enabled — available to saved queries</span>
          </label>
          {upsert.isError && (
            <div
              role="alert"
              className="rounded-md border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-danger"
            >
              {(upsert.error as Error).message}
            </div>
          )}
          <div className="flex gap-2">
            <Button type="submit" disabled={upsert.isPending}>
              {upsert.isPending
                ? "Saving…"
                : editing
                  ? "Save changes"
                  : "Add data source"}
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
        </form>
      </section>

      <section className="flex flex-col gap-3">
        <h2 className="text-base font-semibold text-fg">Connected sources</h2>
        {list.isLoading ? (
          <div className="flex flex-col gap-2" aria-hidden>
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-3/4" />
          </div>
        ) : list.isError ? (
          <div className="rounded-lg border border-border p-6 text-center">
            <p className="text-sm text-fg-muted">
              We couldn’t load your data sources.
            </p>
            <Button
              variant="outline"
              className="mt-3"
              onClick={() => list.refetch()}
            >
              Try again
            </Button>
          </div>
        ) : dataSources.length === 0 ? (
          <EmptyState
            icon={<Database className="h-6 w-6" aria-hidden />}
            title="No data sources yet"
            description="Add a PostgreSQL connection above to query data that lives outside Kapp."
          />
        ) : (
          <div className="overflow-x-auto rounded-lg border border-border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Connection</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {dataSources.map((ds: InsightsDataSource) => {
                  const result = testResult[ds.id];
                  return (
                    <TableRow key={ds.id}>
                      <TableCell>
                        <div className="font-medium text-fg">{ds.name}</div>
                        {ds.description && (
                          <div className="text-xs text-fg-muted">
                            {ds.description}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="text-fg-muted">PostgreSQL</TableCell>
                      <TableCell>
                        <Badge variant={ds.enabled ? "success" : "neutral"}>
                          {ds.enabled ? "Enabled" : "Disabled"}
                        </Badge>
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <Button
                            type="button"
                            size="sm"
                            variant="outline"
                            disabled={test.isPending}
                            onClick={() => test.mutate(ds.id)}
                          >
                            Test
                          </Button>
                          {result &&
                            (result.ok ? (
                              <Badge variant="success">Connected</Badge>
                            ) : (
                              <Badge variant="danger" title={result.message}>
                                Failed
                              </Badge>
                            ))}
                        </div>
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          <Button
                            type="button"
                            size="sm"
                            variant="ghost"
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
                            className="text-danger hover:text-danger"
                            onClick={() =>
                              setDeleteTarget({ id: ds.id, name: ds.name })
                            }
                          >
                            Delete
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        )}
      </section>

      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(o) => {
          if (!o && !remove.isPending) setDeleteTarget(null);
        }}
        title={
          deleteTarget ? `Delete ${deleteTarget.name}?` : "Delete data source?"
        }
        description="This removes the data source and its stored connection credential. Saved queries that reference it will stop running."
        confirmLabel="Delete"
        destructive
        loading={remove.isPending}
        onConfirm={() => {
          if (deleteTarget) remove.mutate(deleteTarget.id);
        }}
      />
    </section>
  );
}
