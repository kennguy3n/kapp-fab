import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { Button } from "@kapp/ui";
import { api } from "../lib/api";

const KTYPE = "sales.return";

interface SalesReturnData {
  return_number?: string;
  customer_id?: string;
  original_invoice_id?: string;
  warehouse_id?: string;
  return_date?: string;
  reason?: string;
  total?: number | string;
  currency?: string;
  status?: string;
  credit_note_id?: string;
  journal_entry_id?: string;
  received_at?: string;
  refunded_at?: string;
}

// STAGES mirrors the workflow declared in internal/sales/returns.go.
// "cancelled" is a terminal state reachable from any pre-refund column;
// rendering it as the rightmost column gives operators a visible
// drop target for abandoning a return without scattering "cancel"
// affordances across the other columns.
const STAGES: string[] = [
  "requested",
  "approved",
  "received",
  "refunded",
  "cancelled",
];

type Verb = "approve" | "receive" | "refund" | "cancel";

/**
 * SalesReturnsPage renders a kanban over `sales.return`. Each card
 * shows the return number, customer, warehouse, total, and posting
 * footprint (credit-note + JE links once refunded). Drag a card into
 * a downstream column to fire the matching ReturnPoster transition
 * via /api/v1/sales/returns/{id}/{verb}; the kanban is read-only for
 * any transition the state machine rejects (e.g. refunded → cancelled).
 *
 * The page intentionally re-renders against the server snapshot
 * after each mutation rather than optimistically updating the
 * column — Receive and Refund can both fail (validation: missing
 * warehouse, original invoice not posted, etc.) and the operator
 * needs to see the authoritative state, not a transient client
 * guess.
 */
export function SalesReturnsPage() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });

  const moveMutation = useMutation({
    mutationFn: async ({ r, to }: { r: KRecord; to: string }) => {
      const current = (r.data as unknown as SalesReturnData).status ?? "requested";
      if (current === to) return;
      const verb = resolveVerb(current, to);
      if (!verb) {
        // No legal transition between these two states — surface a
        // friendly inline error and bail. We deliberately do NOT
        // fall back to PATCHing the status field directly the way
        // PurchaseOrdersPage does for ad-hoc edits: the sales.return
        // state machine has posting side-effects on every legal
        // transition, so a bare status edit would skip the
        // inventory / JE writes the operator expects.
        throw new Error(`cannot move from "${current}" to "${to}"`);
      }
      await api.runSalesReturnTransition(r.id, verb);
    },
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
    },
    onError: (err: unknown) => {
      setError(err instanceof Error ? err.message : String(err));
      // Resync the board against the server even on failure: if
      // the rejection was a 409 because another operator already
      // advanced the return, the card needs to move to its new
      // (server-authoritative) column instead of sitting in the
      // pre-drag column until React Query's stale-time triggers a
      // background refetch. invalidateQueries is a no-op for the
      // already-fresh case, so this is safe to call unconditionally.
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
    },
  });

  const columns = useMemo(() => {
    const by = new Map<string, KRecord[]>();
    for (const s of STAGES) by.set(s, []);
    for (const r of q.data ?? []) {
      const s = (r.data as unknown as SalesReturnData).status ?? "requested";
      (by.get(s) ?? by.set(s, []).get(s)!).push(r);
    }
    return by;
  }, [q.data]);

  return (
    <section>
      <header className="flex items-center justify-between">
        <h1>Sales Returns</h1>
        <Button onClick={() => nav(`/records/${KTYPE}/new`)}>New Return</Button>
      </header>
      <p className="mt-1 text-[13px] text-fg-muted">
        Customer returns and RMAs. Drag a card into Approved → Received →
        Refunded to advance through the lifecycle. The Received column
        posts inventory receipts back into the warehouse; Refunded posts
        the credit-note journal entry that reverses AR / revenue.
      </p>
      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load returns: {(q.error as Error).message}
        </p>
      )}
      {error && (
        <p className="mt-1.5 text-danger" role="alert">
          {error}
        </p>
      )}
      <div className="mt-3 flex gap-3 overflow-x-auto">
        {STAGES.map((s) => (
          <div
            key={s}
            className="min-w-[240px] rounded-md border border-border bg-bg-subtle p-2"
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              const id = e.dataTransfer.getData("text/plain");
              const r = (q.data ?? []).find((x) => x.id === id);
              if (r) moveMutation.mutate({ r, to: s });
            }}
          >
            <div className="text-xs capitalize text-fg-muted">
              {s} · {(columns.get(s) ?? []).length}
            </div>
            {(columns.get(s) ?? []).map((r) => {
              const d = r.data as unknown as SalesReturnData;
              return (
                <div
                  key={r.id}
                  draggable
                  onDragStart={(e) =>
                    e.dataTransfer.setData("text/plain", r.id)
                  }
                  onClick={() => nav(`/records/${KTYPE}/${r.id}`)}
                  className="mt-1.5 cursor-pointer rounded border border-border bg-bg-elevated p-2 text-[13px]"
                >
                  <div className="font-medium">
                    {d.return_number ?? r.id.slice(0, 8)}
                  </div>
                  <div className="text-xs text-fg-muted">
                    Customer: {d.customer_id ?? "—"}
                  </div>
                  <div className="mt-1 text-xs">
                    {d.total ?? 0} {d.currency ?? "USD"}
                  </div>
                  {d.credit_note_id && (
                    <div className="mt-1 text-[11px] text-fg-muted">
                      CN: {d.credit_note_id.slice(0, 8)}
                    </div>
                  )}
                  {d.journal_entry_id && (
                    <div className="text-[11px] text-fg-muted">
                      JE: {d.journal_entry_id.slice(0, 8)}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        ))}
      </div>
    </section>
  );
}

// resolveVerb maps a (from, to) status pair to the lifecycle verb
// that drives the ReturnPoster transition. Returns undefined for any
// pair the state machine doesn't permit; the caller surfaces an
// inline error in that case rather than silently no-op'ing.
function resolveVerb(from: string, to: string): Verb | undefined {
  if (from === "requested" && to === "approved") return "approve";
  if (from === "approved" && to === "received") return "receive";
  if (from === "received" && to === "refunded") return "refund";
  if (
    (from === "requested" || from === "approved" || from === "received") &&
    to === "cancelled"
  )
    return "cancel";
  return undefined;
}
