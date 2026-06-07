import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { JobCard, WorkOrder } from "@kapp/client";
import {
  Badge,
  Button,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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

  const onStarted = invalidateCards;
  const onCompleted = () => {
    invalidateCards();
    // Completing the last open card auto-completes the work order
    // server-side, moving it out of released/in_progress. Refresh the
    // selector queries so the finished order stops showing in the
    // dropdown instead of lingering until the next background refetch.
    qc.invalidateQueries({ queryKey: ["mfg", "work-orders"] });
  };

  return (
    <section>
      <h1>Job Cards</h1>
      <p className="text-fg-muted">
        Shop-floor execution cards generated when a work order is released — one
        per routing operation. Completing the last card emits the work order's
        inventory moves.
      </p>

      <label className="mb-4 flex flex-col gap-1">
        Work order
        <Select
          value={workOrderID}
          onChange={(e) => setWorkOrderID(e.target.value)}
          className="min-w-[360px]"
        >
          <option value="">Select a released / in-progress work order…</option>
          {workOrders.map((wo) => (
            <option key={wo.id} value={wo.id}>
              {wo.id.slice(0, 8)} — {wo.status} — qty {wo.planned_qty}
            </option>
          ))}
        </Select>
      </label>

      {workOrderID === "" && <p>Select a work order to view its job cards.</p>}
      {cardsQ.isLoading && <p>Loading…</p>}
      {cardsQ.isError && <p className="text-danger">{String(cardsQ.error)}</p>}
      {cardsQ.data && cardsQ.data.length === 0 && (
        <p>
          No job cards for this work order. The item likely had no active routing
          at release time.
        </p>
      )}
      {cardsQ.data && cardsQ.data.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Seq</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Produced</TableHead>
              <TableHead>Rejected</TableHead>
              <TableHead>Actual start</TableHead>
              <TableHead>Actual end</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {cardsQ.data.map((jc: JobCard) => (
              <JobCardRow
                key={jc.id}
                jc={jc}
                onStarted={onStarted}
                onCompleted={onCompleted}
              />
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

/**
 * JobCardRow renders one job card and owns its own start / complete
 * mutations. Each row holding its own useMutation is what makes the
 * disabled state per-card: a single shared mutation only tracks the
 * latest mutate() call, so clicking a second card mid-flight would
 * re-enable the first card's button and allow a duplicate submit. With
 * per-row mutations, isPending reflects only this card's request and
 * several cards can be in flight independently.
 */
function JobCardRow({
  jc,
  onStarted,
  onCompleted,
}: {
  jc: JobCard;
  onStarted: () => void;
  onCompleted: () => void;
}) {
  const startMut = useMutation({
    mutationFn: () => api.startJobCard(jc.id),
    onSuccess: onStarted,
  });
  const completeMut = useMutation({
    mutationFn: () => api.completeJobCard(jc.id),
    onSuccess: onCompleted,
  });

  return (
    <TableRow>
      <TableCell>{jc.routing_operation_seq}</TableCell>
      <TableCell>
        <StatusPill status={jc.status} />
      </TableCell>
      <TableCell>{jc.qty_produced}</TableCell>
      <TableCell>{jc.qty_rejected}</TableCell>
      <TableCell>{jc.actual_start ? jc.actual_start.slice(0, 16) : "—"}</TableCell>
      <TableCell>{jc.actual_end ? jc.actual_end.slice(0, 16) : "—"}</TableCell>
      <TableCell>
        <div className="flex items-center gap-2">
          {jc.status === "pending" && (
            <Button size="sm" variant="outline" onClick={() => startMut.mutate()} disabled={startMut.isPending}>
              Start
            </Button>
          )}
          {jc.status !== "completed" && (
            <Button
              size="sm"
              variant="outline"
              onClick={() => completeMut.mutate()}
              disabled={completeMut.isPending}
            >
              Complete
            </Button>
          )}
        </div>
        {(startMut.isError || completeMut.isError) && (
          <div className="mt-1 text-xs text-danger">
            {String(startMut.error ?? completeMut.error)}
          </div>
        )}
      </TableCell>
    </TableRow>
  );
}

function StatusPill({ status }: { status: string }) {
  const variant =
    status === "completed"
      ? "success"
      : status === "in_progress"
        ? "info"
        : "default";
  return <Badge variant={variant}>{status}</Badge>;
}
