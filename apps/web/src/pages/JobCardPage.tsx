import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { JobCard, WorkOrder } from "@kapp/client";
import { api } from "../lib/api";

/**
 * JobCardPage is the Stream 2 shop-floor surface. Job cards are
 * generated automatically (one per routing operation) when a work order
 * is released, so the page is driven by a work-order selector: pick a
 * released / in-progress order and the operator can start and complete
 * its individual operations.
 *
 * Completing the last open card on a work order auto-triggers the
 * existing CompleteWorkOrder inventory-move flow server-side.
 */
export function JobCardPage() {
  const qc = useQueryClient();
  const [workOrderID, setWorkOrderID] = useState("");

  // Job cards only exist for released / in-progress work orders, so the
  // selector is limited to those statuses to avoid offering orders that
  // can never have cards.
  const releasedQ = useQuery({
    queryKey: ["mfg", "work-orders", "released"],
    queryFn: () => api.listWorkOrders("released"),
  });
  const inProgressQ = useQuery({
    queryKey: ["mfg", "work-orders", "in_progress"],
    queryFn: () => api.listWorkOrders("in_progress"),
  });
  const workOrders: WorkOrder[] = [
    ...(releasedQ.data ?? []),
    ...(inProgressQ.data ?? []),
  ];

  const cardsQ = useQuery({
    queryKey: ["mfg", "job-cards", workOrderID],
    queryFn: () => api.listJobCards(workOrderID),
    enabled: workOrderID !== "",
  });

  const invalidateCards = () =>
    qc.invalidateQueries({ queryKey: ["mfg", "job-cards", workOrderID] });

  const startMut = useMutation({
    mutationFn: (id: string) => api.startJobCard(id),
    onSuccess: invalidateCards,
  });
  const completeMut = useMutation({
    mutationFn: (id: string) => api.completeJobCard(id),
    onSuccess: () => {
      invalidateCards();
      // Completing the last open card auto-completes the work order
      // server-side, moving it out of released/in_progress. Refresh the
      // selector queries so the finished order stops showing in the
      // dropdown instead of lingering until the next background refetch.
      qc.invalidateQueries({ queryKey: ["mfg", "work-orders"] });
    },
  });

  return (
    <section>
      <h1>Job Cards</h1>
      <p style={{ color: "#6b7280" }}>
        Shop-floor execution cards generated when a work order is released — one
        per routing operation. Completing the last card emits the work order's
        inventory moves.
      </p>

      <label style={{ display: "block", marginBottom: 16 }}>
        Work order
        <select
          value={workOrderID}
          onChange={(e) => setWorkOrderID(e.target.value)}
          style={{ display: "block", minWidth: 360 }}
        >
          <option value="">Select a released / in-progress work order…</option>
          {workOrders.map((wo) => (
            <option key={wo.id} value={wo.id}>
              {wo.id.slice(0, 8)} — {wo.status} — qty {wo.planned_qty}
            </option>
          ))}
        </select>
      </label>

      {workOrderID === "" && <p>Select a work order to view its job cards.</p>}
      {cardsQ.isLoading && <p>Loading…</p>}
      {cardsQ.isError && <p style={{ color: "#dc2626" }}>{String(cardsQ.error)}</p>}
      {cardsQ.data && cardsQ.data.length === 0 && (
        <p>
          No job cards for this work order. The item likely had no active routing
          at release time.
        </p>
      )}
      {cardsQ.data && cardsQ.data.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th>Seq</th>
              <th>Status</th>
              <th>Produced</th>
              <th>Rejected</th>
              <th>Actual start</th>
              <th>Actual end</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {cardsQ.data.map((jc: JobCard) => (
              <tr key={jc.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                <td>{jc.routing_operation_seq}</td>
                <td>
                  <StatusPill status={jc.status} />
                </td>
                <td>{jc.qty_produced}</td>
                <td>{jc.qty_rejected}</td>
                <td>{jc.actual_start ? jc.actual_start.slice(0, 16) : "—"}</td>
                <td>{jc.actual_end ? jc.actual_end.slice(0, 16) : "—"}</td>
                <td>
                  {jc.status === "pending" && (
                    <button
                      onClick={() => startMut.mutate(jc.id)}
                      disabled={startMut.isPending}
                    >
                      Start
                    </button>
                  )}
                  {jc.status !== "completed" && (
                    <button
                      onClick={() => completeMut.mutate(jc.id)}
                      disabled={completeMut.isPending}
                      style={{ marginLeft: 8 }}
                    >
                      Complete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {(startMut.isError || completeMut.isError) && (
        <p style={{ color: "#dc2626" }}>
          {String(startMut.error ?? completeMut.error)}
        </p>
      )}
    </section>
  );
}

function StatusPill({ status }: { status: string }) {
  const background =
    status === "completed"
      ? "#dcfce7"
      : status === "in_progress"
        ? "#dbeafe"
        : "#e5e7eb";
  return (
    <span style={{ padding: "2px 8px", borderRadius: 12, background }}>
      {status}
    </span>
  );
}
