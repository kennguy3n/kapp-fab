import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { Button } from "@kapp/ui";
import { api } from "../lib/api";

const KTYPE = "procurement.purchase_requisition";

interface RequisitionData {
  requisition_number?: string;
  requested_by?: string;
  department?: string;
  cost_center?: string;
  supplier_id?: string;
  request_date?: string;
  needed_by?: string;
  justification?: string;
  subtotal?: number | string;
  currency?: string;
  status?: string;
  po_id?: string;
  approved_by?: string;
  approved_at?: string;
}

// STAGES mirrors the workflow declared in internal/sales/requisition.go.
// "cancelled" is a terminal state reachable from any pre-ordered
// column; rendering it as the rightmost column gives operators a
// visible drop target for abandoning a requisition without
// scattering "cancel" affordances across the other columns.
const STAGES: string[] = ["requested", "approved", "ordered", "cancelled"];

type Verb = "approve" | "convert" | "cancel";

/**
 * PurchaseRequisitionsPage renders a kanban over
 * `procurement.purchase_requisition`. Each card shows the
 * requisition number, requested-by, supplier (optional), subtotal,
 * and conversion footprint (po_id once converted). Drag a card into
 * a downstream column to fire the matching RequisitionPoster
 * transition via /api/v1/procurement/requisitions/{id}/{verb}; the
 * kanban is read-only for any transition the state machine rejects
 * (e.g. ordered → cancelled).
 *
 * The page intentionally re-renders against the server snapshot
 * after each mutation rather than optimistically updating the
 * column — Convert can fail (validation: no supplier set, lines
 * empty, PO already exists, etc.) and the operator needs to see
 * the authoritative state, not a transient client guess.
 */
export function PurchaseRequisitionsPage() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });

  const moveMutation = useMutation({
    mutationFn: async ({ r, to }: { r: KRecord; to: string }) => {
      const current =
        (r.data as unknown as RequisitionData).status ?? "requested";
      // Note: same-column drops are filtered in the onDrop handler
      // below before mutate() is called, so the mutation never sees
      // current === to. Keeping that guard at the call site means
      // onMutate (which clears the error banner) and onSuccess
      // (which invalidates the records query) only fire on real
      // state changes — a no-op drop should not trigger a refetch.
      const verb = resolveVerb(current, to);
      if (!verb) {
        // No legal transition between these two states — surface a
        // friendly inline error and bail. We deliberately do NOT
        // fall back to PATCHing the status field directly: convert
        // allocates a procurement.purchase_order KRecord on the way
        // through, so a bare status edit would skip that allocation
        // and orphan the requisition from the PO surface.
        throw new Error(`cannot move from "${current}" to "${to}"`);
      }
      await api.runRequisitionTransition(r.id, verb);
    },
    onMutate: () => {
      // Clear any prior error before a new attempt so the inline
      // banner reflects only the current mutation outcome — a retry
      // after a failure should not leave the previous error
      // rendered while the request is in flight.
      setError(null);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
    },
    onError: (err: unknown) => {
      setError(err instanceof Error ? err.message : String(err));
    },
  });

  // columns is the (status → rows) map; allStages is the ordered
  // list of column headers to render. STAGES is the canonical
  // workflow but we also surface any unknown status discovered in
  // the data so a future backend schema migration (or anomalous
  // record) cannot make rows silently disappear from the kanban —
  // operators must always be able to see every requisition somewhere.
  // Unknown statuses are rendered after the canonical columns with
  // a visual tag so they stand out; drop targets on them resolve to
  // resolveVerb() === undefined and surface the inline error,
  // preventing accidental writes to a status the frontend does not
  // understand.
  const { columns, allStages, unknownStages } = useMemo(() => {
    const by = new Map<string, KRecord[]>();
    for (const s of STAGES) by.set(s, []);
    const extras = new Set<string>();
    for (const r of q.data ?? []) {
      const s =
        (r.data as unknown as RequisitionData).status ?? "requested";
      if (!by.has(s)) {
        by.set(s, []);
        extras.add(s);
      }
      by.get(s)!.push(r);
    }
    const extrasList = Array.from(extras).sort();
    return {
      columns: by,
      allStages: [...STAGES, ...extrasList],
      unknownStages: extras,
    };
  }, [q.data]);

  return (
    <section>
      <header className="flex items-center justify-between">
        <h1>Purchase Requisitions</h1>
        <Button onClick={() => nav(`/records/${KTYPE}/new`)}>
          New Requisition
        </Button>
      </header>
      <p className="mt-1 text-[13px] text-fg-muted">
        Internal purchase requests pending approval. Drag a card into
        Approved → Ordered to advance through the lifecycle. The
        Ordered column allocates a procurement.purchase_order
        KRecord and stamps po_id so a retried convert reuses the
        prior PO instead of spawning a duplicate.
      </p>
      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load requisitions: {(q.error as Error).message}
        </p>
      )}
      {error && (
        <p className="mt-1.5 text-danger" role="alert">
          {error}
        </p>
      )}
      <div className="mt-3 flex gap-3 overflow-x-auto">
        {allStages.map((s) => {
          const isUnknown = unknownStages.has(s);
          return (
          <div
            key={s}
            className={`min-w-[240px] rounded-md p-2 ${
              isUnknown
                ? "border border-dashed border-warning bg-warning/15"
                : "border border-border bg-bg-subtle"
            }`}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              const id = e.dataTransfer.getData("text/plain");
              const r = (q.data ?? []).find((x) => x.id === id);
              if (!r) return;
              const current =
                (r.data as unknown as RequisitionData).status ?? "requested";
              // Skip same-column drops at the call site so the
              // mutation lifecycle (onMutate / onSuccess) never
              // fires for no-op moves — otherwise a card dropped
              // back into its own column would clear the inline
              // error and trigger a refetch even though nothing
              // changed.
              if (current === s) return;
              moveMutation.mutate({ r, to: s });
            }}
          >
            <div
              className={`text-xs capitalize ${
                isUnknown ? "font-semibold text-warning" : "text-fg-muted"
              }`}
              title={
                isUnknown
                  ? `"${s}" is not a known requisition status. The backend may have added a new state; update STAGES in PurchaseRequisitionsPage.tsx so this column gets the canonical styling.`
                  : undefined
              }
            >
              {s} · {(columns.get(s) ?? []).length}
              {isUnknown && " (unknown)"}
            </div>
            {(columns.get(s) ?? []).map((r) => {
              const d = r.data as unknown as RequisitionData;
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
                    {d.requisition_number ?? r.id.slice(0, 8)}
                  </div>
                  <div className="text-xs text-fg-muted">
                    Requested by: {d.requested_by ?? "—"}
                  </div>
                  {d.department && (
                    <div className="text-xs text-fg-muted">
                      Dept: {d.department}
                    </div>
                  )}
                  <div className="mt-1 text-xs">
                    {d.subtotal ?? 0} {d.currency ?? "USD"}
                  </div>
                  {d.po_id && (
                    <div className="mt-1 text-[11px] text-fg-muted">
                      PO: {d.po_id.slice(0, 8)}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
          );
        })}
      </div>
    </section>
  );
}

// resolveVerb maps a (from, to) status pair to the lifecycle verb
// that drives the RequisitionPoster transition. The mapping is
// intentionally permissive on the "to=cancelled" side: any drop into
// the Cancelled column resolves to the `cancel` verb regardless of
// origin column, so the backend's RequisitionPoster.Cancel can return
// its specific error message ("ordered requisitions cannot be
// cancelled (cancel the PO instead)" — requisition_poster.go:257)
// rather than the frontend short-circuiting with a generic
// `cannot move from "ordered" to "cancelled"`. The state machine on
// the backend is the single source of truth for which transitions
// are legal; duplicating that logic on the frontend would risk drift
// (e.g., if a future state is added) and would hide the more
// actionable error from operators.
//
// Returns undefined only for pairs that have no corresponding verb
// at all (e.g. cancelled→approved, ordered→requested) — for those
// the caller surfaces a generic inline error because there's no
// backend endpoint to delegate to.
function resolveVerb(from: string, to: string): Verb | undefined {
  if (from === "requested" && to === "approved") return "approve";
  if (from === "approved" && to === "ordered") return "convert";
  if (to === "cancelled") return "cancel";
  return undefined;
}
