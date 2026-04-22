import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { AuditEntry } from "@kapp/client";
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
    <section>
      <h1>Audit Log</h1>
      <p style={{ color: "#6b7280" }}>
        Tenant-scoped trail of mutations. Entries are append-only; applying a
        filter does not change the underlying data, only what's rendered.
      </p>

      <div style={{ display: "flex", gap: 12, margin: "12px 0", flexWrap: "wrap" }}>
        <label style={{ display: "flex", flexDirection: "column", fontSize: 12 }}>
          <span style={{ color: "#6b7280" }}>Target KType</span>
          <input
            value={targetKType}
            onChange={(e) => {
              setTargetKType(e.target.value);
              setPage(0);
            }}
            placeholder="e.g. crm.deal"
          />
        </label>
        <label style={{ display: "flex", flexDirection: "column", fontSize: 12 }}>
          <span style={{ color: "#6b7280" }}>Target ID</span>
          <input
            value={targetID}
            onChange={(e) => {
              setTargetID(e.target.value);
              setPage(0);
            }}
            placeholder="UUID"
            style={{ minWidth: 280 }}
          />
        </label>
      </div>

      {entries.isLoading && <p>Loading…</p>}
      {entries.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load audit entries: {(entries.error as Error).message}
        </p>
      )}

      {entries.data && entries.data.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No audit entries for this filter.
        </p>
      )}

      {entries.data && entries.data.length > 0 && (
        <>
          <table
            style={{
              width: "100%",
              borderCollapse: "collapse",
              marginTop: 12,
              fontSize: 13,
            }}
          >
            <thead>
              <tr style={{ textAlign: "left", color: "#6b7280" }}>
                <Th>Timestamp</Th>
                <Th>Actor</Th>
                <Th>Action</Th>
                <Th>Target</Th>
                <Th>Diff</Th>
              </tr>
            </thead>
            <tbody>
              {entries.data.map((e) => (
                <AuditRow key={e.id} entry={e} />
              ))}
            </tbody>
          </table>
          <div
            style={{
              display: "flex",
              justifyContent: "space-between",
              marginTop: 12,
              fontSize: 13,
            }}
          >
            <span style={{ color: "#6b7280" }}>
              Page {page + 1} · showing up to {pageSize}
            </span>
            <div style={{ display: "flex", gap: 8 }}>
              <button disabled={page === 0} onClick={() => setPage((p) => Math.max(0, p - 1))}>
                Prev
              </button>
              <button
                disabled={entries.data.length < pageSize}
                onClick={() => setPage((p) => p + 1)}
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}
    </section>
  );
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  return (
    <tr style={{ borderTop: "1px solid #e5e7eb", verticalAlign: "top" }}>
      <Td>{new Date(entry.created_at).toLocaleString()}</Td>
      <Td>
        <div>{entry.actor_kind}</div>
        <div style={{ color: "#9ca3af", fontSize: 11 }}>
          {entry.actor_id ? entry.actor_id.slice(0, 8) : "—"}
        </div>
      </Td>
      <Td>
        <code>{entry.action}</code>
      </Td>
      <Td>
        <div>{entry.target_ktype ?? "—"}</div>
        <div style={{ color: "#9ca3af", fontSize: 11 }}>
          {entry.target_id ? entry.target_id.slice(0, 8) : "—"}
        </div>
      </Td>
      <Td>
        <DiffCell before={entry.before} after={entry.after} />
      </Td>
    </tr>
  );
}

function DiffCell({ before, after }: { before: unknown; after: unknown }) {
  if (before == null && after == null) {
    return <span style={{ color: "#9ca3af" }}>—</span>;
  }
  return (
    <details>
      <summary style={{ cursor: "pointer", color: "#2563eb" }}>view</summary>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8, marginTop: 6 }}>
        <pre
          style={{
            margin: 0,
            background: "#f9fafb",
            padding: 6,
            borderRadius: 4,
            fontSize: 11,
            overflow: "auto",
          }}
        >
          {formatJSON(before)}
        </pre>
        <pre
          style={{
            margin: 0,
            background: "#f9fafb",
            padding: 6,
            borderRadius: 4,
            fontSize: 11,
            overflow: "auto",
          }}
        >
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

function Th({ children }: { children: React.ReactNode }) {
  return <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12 }}>{children}</th>;
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "8px" }}>{children}</td>;
}
