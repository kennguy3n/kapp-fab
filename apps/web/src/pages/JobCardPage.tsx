import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { InventoryItem, JobCard, WorkOrder } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, ClipboardList, Inbox } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";

type JobCardStatus = JobCard["status"];
type Formatters = ReturnType<typeof useFormatter>;

const STATUS_LABEL: Record<JobCardStatus, string> = {
  pending: "Pending",
  in_progress: "In Progress",
  completed: "Completed",
};

const STATUS_VARIANT: Record<JobCardStatus, BadgeProps["variant"]> = {
  pending: "default",
  in_progress: "warning",
  completed: "success",
};

const WO_STATUS_LABEL: Record<WorkOrder["status"], string> = {
  draft: "Draft",
  released: "Released",
  in_progress: "In Progress",
  completed: "Completed",
  closed: "Closed",
  cancelled: "Cancelled",
};

/**
 * JobCardPage is the shop-floor execution surface. Job cards are
 * generated automatically (one per routing operation) when a work order
 * is released, so the page is driven by a work-order selector: pick a
 * released / in-progress order and the operator works the operation
 * checklist top to bottom, starting and completing each step.
 *
 * Completing the last open card on a work order auto-triggers the
 * server-side CompleteWorkOrder inventory-move flow.
 */
export function JobCardPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
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
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });

  const workOrders: WorkOrder[] = useMemo(
    () => [...(releasedQ.data ?? []), ...(inProgressQ.data ?? [])],
    [releasedQ.data, inProgressQ.data],
  );

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it: InventoryItem) =>
      m.set(it.id, `${it.sku} — ${it.name}`),
    );
    return m;
  }, [itemsQ.data]);

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

  const cards = cardsQ.data ?? [];
  const completedCount = cards.filter((c) => c.status === "completed").length;

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-3">
        <div className="min-w-0">
          <Eyebrow>Manufacturing</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Job Cards
          </h1>
          <p className="mt-1 max-w-2xl text-sm text-fg-muted">
            Work the shop floor one operation at a time. Start a step when you
            begin and complete it when you're done — finishing the last step
            books the finished goods in automatically.
          </p>
        </div>
        <div className="max-w-md">
          <Field label="Work order">
            <Select
              value={workOrderID}
              onChange={(e) => setWorkOrderID(e.target.value)}
            >
              <option value="">Select a released or in-progress order…</option>
              {workOrders.map((wo) => (
                <option key={wo.id} value={wo.id}>
                  {(itemLabel.get(wo.item_id) ?? "Work order") +
                    " · " +
                    WO_STATUS_LABEL[wo.status] +
                    " · qty " +
                    wo.planned_qty}
                </option>
              ))}
            </Select>
          </Field>
        </div>
      </header>

      {workOrderID === "" ? (
        <EmptyState
          icon={<ClipboardList />}
          title="Pick a work order"
          description="Choose a released or in-progress work order above to see its operation checklist."
        />
      ) : cardsQ.isLoading ? (
        <Skeleton className="h-48 w-full" />
      ) : cardsQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load job cards"
          description={(cardsQ.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void cardsQ.refetch()}
              disabled={cardsQ.isFetching}
            >
              Retry
            </Button>
          }
        />
      ) : cards.length === 0 ? (
        <EmptyState
          icon={<Inbox />}
          title="No operations on this order"
          description="This item had no active routing when the work order was released, so no operations were generated."
        />
      ) : (
        <div className="flex flex-col gap-3">
          <p className="text-sm text-fg-muted">
            <span className="font-medium tabular-nums text-fg">
              {completedCount}
            </span>{" "}
            of{" "}
            <span className="font-medium tabular-nums text-fg">
              {cards.length}
            </span>{" "}
            operations complete
          </p>
          <div className="overflow-x-auto rounded-xl border border-border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-20">Step</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Produced</TableHead>
                  <TableHead className="text-right">Rejected</TableHead>
                  <TableHead>Started</TableHead>
                  <TableHead>Finished</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {cards.map((jc) => (
                  <JobCardRow
                    key={jc.id}
                    jc={jc}
                    fmt={fmt}
                    onStarted={onStarted}
                    onCompleted={onCompleted}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}
    </section>
  );
}

/**
 * JobCardRow renders one operation and owns its own start / complete
 * mutations. Each row holding its own useMutation is what makes the
 * disabled state per-card: a single shared mutation only tracks the
 * latest mutate() call, so clicking a second card mid-flight would
 * re-enable the first card's button and allow a duplicate submit. With
 * per-row mutations, isPending reflects only this card's request and
 * several cards can be worked independently.
 */
function JobCardRow({
  jc,
  fmt,
  onStarted,
  onCompleted,
}: {
  jc: JobCard;
  fmt: Formatters;
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

  const fmtTime = (value?: string | null) =>
    value ? fmt.dateTime(new Date(value)) : "—";

  return (
    <TableRow>
      <TableCell className="font-medium tabular-nums text-fg">
        {jc.routing_operation_seq}
      </TableCell>
      <TableCell>
        <Badge variant={STATUS_VARIANT[jc.status]} size="sm">
          {STATUS_LABEL[jc.status]}
        </Badge>
      </TableCell>
      <TableCell className="text-right tabular-nums">
        {fmt.number(Number(jc.qty_produced))}
      </TableCell>
      <TableCell className="text-right tabular-nums">
        {fmt.number(Number(jc.qty_rejected))}
      </TableCell>
      <TableCell className="whitespace-nowrap text-fg-muted">
        {fmtTime(jc.actual_start)}
      </TableCell>
      <TableCell className="whitespace-nowrap text-fg-muted">
        {fmtTime(jc.actual_end)}
      </TableCell>
      <TableCell>
        <div className="flex items-center justify-end gap-2">
          {jc.status === "pending" && (
            <Button
              size="sm"
              variant="outline"
              onClick={() => startMut.mutate()}
              disabled={startMut.isPending}
            >
              Start
            </Button>
          )}
          {jc.status !== "completed" && (
            <Button
              size="sm"
              onClick={() => completeMut.mutate()}
              disabled={completeMut.isPending}
            >
              Complete
            </Button>
          )}
        </div>
        {(startMut.isError || completeMut.isError) && (
          <div className="mt-1 text-right text-xs text-danger">
            {((startMut.error ?? completeMut.error) as Error).message}
          </div>
        )}
      </TableCell>
    </TableRow>
  );
}
