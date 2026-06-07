import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { AuditEntry } from "@kapp/client";
import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * AuditLogPage renders the append-only audit log for the current
 * tenant. It fetches GET /api/v1/audit with optional filters for
 * target KType and target ID. Pagination is offset-based to match the
 * backend; the page size (50) is small enough to keep the before/after
 * diff columns readable but large enough to cover a normal workday of
 * activity in a single request.
 */
export function AuditLogPage() {
  const [targetKType, setTargetKType] = useState("");
  const [targetID, setTargetID] = useState("");
  const [page, setPage] = useState(0);
  const pageSize = 50;

  const params = useMemo(
    () => ({
      target_ktype: targetKType.trim() || undefined,
      target_id: targetID.trim() || undefined,
      limit: pageSize,
      offset: page * pageSize,
    }),
    [targetKType, targetID, page],
  );

  const entries = useQuery({
    queryKey: ["audit", params],
    queryFn: () => api.listAuditLog(params),
  });

  return (
    <section className="flex flex-col gap-2">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">Audit Log</h1>
      <p className="text-sm text-fg-muted">
        Tenant-scoped trail of mutations. Entries are append-only; applying a
        filter does not change the underlying data, only what's rendered.
      </p>

      <div className="my-3 flex flex-wrap gap-3">
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          <span>Target KType</span>
          <Input
            value={targetKType}
            onChange={(e) => {
              setTargetKType(e.target.value);
              setPage(0);
            }}
            placeholder="e.g. crm.deal"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          <span>Target ID</span>
          <Input
            value={targetID}
            onChange={(e) => {
              setTargetID(e.target.value);
              setPage(0);
            }}
            placeholder="UUID"
            className="min-w-[280px]"
          />
        </label>
      </div>

      {entries.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {entries.isError && (
        <p className="text-sm text-danger">
          Failed to load audit entries: {(entries.error as Error).message}
        </p>
      )}

      {entries.data && entries.data.length === 0 && (
        <p className="text-sm italic text-fg-subtle">
          No audit entries for this filter.
        </p>
      )}

      {entries.data && entries.data.length > 0 && (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Timestamp</TableHead>
                <TableHead>Actor</TableHead>
                <TableHead>Action</TableHead>
                <TableHead>Target</TableHead>
                <TableHead>Diff</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.data.map((e) => (
                <AuditRow key={e.id} entry={e} />
              ))}
            </TableBody>
          </Table>
          <div className="mt-3 flex items-center justify-between text-sm">
            <span className="text-fg-muted">
              Page {page + 1} · showing up to {pageSize}
            </span>
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="outline"
                disabled={page === 0}
                onClick={() => setPage((p) => Math.max(0, p - 1))}
              >
                Prev
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={entries.data.length < pageSize}
                onClick={() => setPage((p) => p + 1)}
              >
                Next
              </Button>
            </div>
          </div>
        </>
      )}
    </section>
  );
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  return (
    <TableRow className="align-top">
      <TableCell>{new Date(entry.created_at).toLocaleString()}</TableCell>
      <TableCell>
        <div>{entry.actor_kind}</div>
        <div className="text-[11px] text-fg-subtle">
          {entry.actor_id ? entry.actor_id.slice(0, 8) : "—"}
        </div>
      </TableCell>
      <TableCell>
        <code>{entry.action}</code>
      </TableCell>
      <TableCell>
        <div>{entry.target_ktype ?? "—"}</div>
        <div className="text-[11px] text-fg-subtle">
          {entry.target_id ? entry.target_id.slice(0, 8) : "—"}
        </div>
      </TableCell>
      <TableCell>
        <DiffCell before={entry.before} after={entry.after} />
      </TableCell>
    </TableRow>
  );
}

function DiffCell({ before, after }: { before: unknown; after: unknown }) {
  if (before == null && after == null) {
    return <span className="text-fg-subtle">—</span>;
  }
  return (
    <details>
      <summary className="cursor-pointer text-accent">view</summary>
      <div className="mt-1.5 grid grid-cols-2 gap-2">
        <pre className="m-0 overflow-auto rounded bg-bg-subtle p-1.5 text-[11px]">
          {formatJSON(before)}
        </pre>
        <pre className="m-0 overflow-auto rounded bg-bg-subtle p-1.5 text-[11px]">
          {formatJSON(after)}
        </pre>
      </div>
    </details>
  );
}

function formatJSON(v: unknown): string {
  if (v == null) return "—";
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
