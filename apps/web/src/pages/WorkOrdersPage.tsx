import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  InventoryItem,
  InventoryWarehouse,
  WorkOrder,
} from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, ClipboardList, Plus } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";

type WOStatus = WorkOrder["status"];
type Formatters = ReturnType<typeof useFormatter>;

interface LaneDef {
  status: WOStatus;
  label: string;
  // Accent classes drive the lane header tint; kept to design tokens so
  // both themes stay intentional. Each lane gets a soft fill + a solid
  // bar so the board reads at a glance on a shop-floor monitor.
  fill: string;
  bar: string;
}

// COLUMNS lists the always-visible kanban lanes. `closed` is
// deliberately NOT here: closed work orders are archival and would
// crowd the active board if shown by default. They are surfaced via a
// `Show closed (N)` toggle that appends a sixth lane on demand. Keeping
// it toggleable rather than a separate page preserves the board
// metaphor and lets operators retrieve a closed order in place.
const COLUMNS: LaneDef[] = [
  { status: "draft", label: "Draft", fill: "bg-bg-muted", bar: "bg-border-strong" },
  { status: "released", label: "Released", fill: "bg-info/10", bar: "bg-info" },
  {
    status: "in_progress",
    label: "In Progress",
    fill: "bg-warning/15",
    bar: "bg-warning",
  },
  {
    status: "completed",
    label: "Completed",
    fill: "bg-success/10",
    bar: "bg-success",
  },
  {
    status: "cancelled",
    label: "Cancelled",
    fill: "bg-danger/10",
    bar: "bg-danger",
  },
];

const CLOSED_COLUMN: LaneDef = {
  status: "closed",
  label: "Closed",
  fill: "bg-accent/10",
  bar: "bg-accent",
};

const STATUS_VARIANT: Record<WOStatus, BadgeProps["variant"]> = {
  draft: "default",
  released: "info",
  in_progress: "warning",
  completed: "success",
  closed: "accent",
  cancelled: "danger",
};

const STATUS_LABEL: Record<WOStatus, string> = {
  draft: "Draft",
  released: "Released",
  in_progress: "In Progress",
  completed: "Completed",
  closed: "Closed",
  cancelled: "Cancelled",
};

type TransitionAction = "release" | "start" | "complete" | "cancel" | "close";

/**
 * WorkOrdersPage renders a touch-friendly status board of production
 * runs bucketed by status. Each card exposes the legal state-machine
 * transitions (draft→release, released→start|complete,
 * in_progress→complete, etc.); completing a work order emits the
 * consumption + receipt inventory moves atomically server-side.
 */
export function WorkOrdersPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const [showClosed, setShowClosed] = useState(false);

  const wosQ = useQuery({
    queryKey: ["mfg", "work-orders"],
    queryFn: () => api.listWorkOrders(),
  });
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });
  const whQ = useQuery({
    queryKey: ["inventory", "warehouses"],
    queryFn: () => api.listInventoryWarehouses(),
  });

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it: InventoryItem) =>
      m.set(it.id, `${it.sku} — ${it.name}`),
    );
    return m;
  }, [itemsQ.data]);
  const whLabel = useMemo(() => {
    const m = new Map<string, string>();
    (whQ.data ?? []).forEach((w: InventoryWarehouse) =>
      m.set(w.id, `${w.code} — ${w.name}`),
    );
    return m;
  }, [whQ.data]);

  const transitionMut = useMutation({
    mutationFn: async ({
      id,
      action,
      actualQty,
    }: {
      id: string;
      action: TransitionAction;
      actualQty?: string;
    }) => {
      switch (action) {
        case "release":
          return api.releaseWorkOrder(id);
        case "start":
          return api.startWorkOrder(id);
        case "complete":
          return api.completeWorkOrder(id, actualQty);
        case "cancel":
          return api.cancelWorkOrder(id);
        case "close":
          return api.closeWorkOrder(id);
      }
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mfg", "work-orders"] }),
  });

  const grouped = useMemo(() => {
    const m = new Map<WOStatus, WorkOrder[]>();
    COLUMNS.forEach((c) => m.set(c.status, []));
    // Pre-allocate the closed bucket so its count is 0 (not NaN) when no
    // closed orders exist yet.
    m.set(CLOSED_COLUMN.status, []);
    (wosQ.data ?? []).forEach((wo: WorkOrder) => {
      const arr = m.get(wo.status) ?? [];
      arr.push(wo);
      m.set(wo.status, arr);
    });
    return m;
  }, [wosQ.data]);

  const visibleColumns = showClosed ? [...COLUMNS, CLOSED_COLUMN] : COLUMNS;
  const closedCount = (grouped.get(CLOSED_COLUMN.status) ?? []).length;
  const total = (wosQ.data ?? []).length;

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Manufacturing</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Work Orders
            </h1>
            <p className="mt-1 max-w-2xl text-sm text-fg-muted">
              Track each production run from draft to done. Completing a work
              order books the components out and the finished goods in
              automatically.
            </p>
          </div>
          <Button
            type="button"
            size="sm"
            variant={showClosed ? "secondary" : "outline"}
            onClick={() => setShowClosed((v) => !v)}
            aria-pressed={showClosed}
          >
            {showClosed ? "Hide" : "Show"} closed ({closedCount})
          </Button>
        </div>
      </header>

      <CreateWorkOrderForm
        items={itemsQ.data ?? []}
        warehouses={whQ.data ?? []}
      />

      {wosQ.isLoading ? (
        <div className="flex gap-3 overflow-x-auto pb-2">
          {COLUMNS.map((c) => (
            <div key={c.status} className="min-w-[220px] flex-1">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="mt-2 h-28 w-full" />
            </div>
          ))}
        </div>
      ) : wosQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load work orders"
          description={(wosQ.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void wosQ.refetch()}
              disabled={wosQ.isFetching}
            >
              Retry
            </Button>
          }
        />
      ) : total === 0 ? (
        <EmptyState
          icon={<ClipboardList />}
          title="No work orders yet"
          description="Create your first work order above to start tracking production."
        />
      ) : (
        <div className="flex gap-3 overflow-x-auto pb-2">
          {visibleColumns.map((col) => {
            const cards = grouped.get(col.status) ?? [];
            return (
              <div
                key={col.status}
                className="flex min-w-[220px] flex-1 flex-col rounded-xl border border-border bg-bg-subtle"
              >
                <div
                  className={`flex items-center gap-2 rounded-t-xl px-3 py-2 ${col.fill}`}
                >
                  <span className={`h-2.5 w-2.5 rounded-full ${col.bar}`} />
                  <span className="font-semibold text-fg">
                    {col.label} ({cards.length})
                  </span>
                </div>
                <div className="flex min-h-[120px] flex-col gap-2 p-2">
                  {cards.map((wo) => (
                    <WorkOrderCard
                      key={wo.id}
                      wo={wo}
                      fmt={fmt}
                      itemLabel={itemLabel}
                      whLabel={whLabel}
                      onTransition={(action, actualQty) =>
                        transitionMut.mutate({ id: wo.id, action, actualQty })
                      }
                      disabled={transitionMut.isPending}
                    />
                  ))}
                  {cards.length === 0 ? (
                    <p className="px-1 py-6 text-center text-xs text-fg-subtle">
                      Nothing here
                    </p>
                  ) : null}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

interface WorkOrderCardProps {
  wo: WorkOrder;
  fmt: Formatters;
  itemLabel: Map<string, string>;
  whLabel: Map<string, string>;
  onTransition: (action: TransitionAction, actualQty?: string) => void;
  disabled: boolean;
}

function WorkOrderCard({
  wo,
  fmt,
  itemLabel,
  whLabel,
  onTransition,
  disabled,
}: WorkOrderCardProps) {
  const [actual, setActual] = useState(wo.planned_qty);
  const canComplete = wo.status === "released" || wo.status === "in_progress";
  return (
    <div className="rounded-lg border border-border bg-bg-elevated p-3 shadow-sm">
      <div className="truncate text-sm font-semibold text-fg">
        {itemLabel.get(wo.item_id) ?? wo.item_id}
      </div>
      <div className="mt-0.5 truncate text-xs text-fg-muted">
        {whLabel.get(wo.warehouse_id) ?? "—"}
      </div>
      <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-fg-muted">
        <span>
          Planned{" "}
          <span className="font-medium tabular-nums text-fg">
            {fmt.number(Number(wo.planned_qty))}
          </span>
        </span>
        {wo.actual_qty ? (
          <span>
            Made{" "}
            <span className="font-medium tabular-nums text-fg">
              {fmt.number(Number(wo.actual_qty))}
            </span>
          </span>
        ) : null}
      </div>

      {canComplete ? (
        <div className="mt-3 flex items-end gap-2">
          <Input
            aria-label="actual qty"
            size="sm"
            type="number"
            step="0.01"
            min="0"
            inputMode="decimal"
            className="w-24 text-right tabular-nums"
            value={actual}
            onChange={(e) => setActual(e.target.value)}
          />
          <Button
            size="sm"
            onClick={() => onTransition("complete", actual)}
            disabled={disabled}
          >
            Complete
          </Button>
        </div>
      ) : null}

      <div className="mt-3 flex flex-wrap items-center gap-2">
        {wo.status === "draft" && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => onTransition("release")}
            disabled={disabled}
          >
            Release
          </Button>
        )}
        {wo.status === "released" && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => onTransition("start")}
            disabled={disabled}
          >
            Start
          </Button>
        )}
        {(wo.status === "draft" ||
          wo.status === "released" ||
          wo.status === "in_progress") && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => onTransition("cancel")}
            disabled={disabled}
          >
            Cancel
          </Button>
        )}
        {wo.status === "completed" && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => onTransition("close")}
            disabled={disabled}
          >
            Close
          </Button>
        )}
        {wo.status === "closed" || wo.status === "cancelled" ? (
          <Badge variant={STATUS_VARIANT[wo.status]} size="xs">
            {STATUS_LABEL[wo.status]}
          </Badge>
        ) : null}
      </div>
    </div>
  );
}

interface CreateFormProps {
  items: InventoryItem[];
  warehouses: InventoryWarehouse[];
}

function CreateWorkOrderForm({ items, warehouses }: CreateFormProps) {
  const qc = useQueryClient();
  const [itemID, setItemID] = useState("");
  const [whID, setWhID] = useState("");
  const [plannedQty, setPlannedQty] = useState("1");
  const createMut = useMutation({
    mutationFn: () =>
      api.createWorkOrder({
        item_id: itemID,
        warehouse_id: whID,
        planned_qty: plannedQty,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mfg", "work-orders"] });
      setItemID("");
      setWhID("");
      setPlannedQty("1");
    },
  });

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        createMut.mutate();
      }}
      className="rounded-xl border border-border bg-bg-subtle p-4"
    >
      <h2 className="m-0 text-sm font-semibold text-fg">Plan a work order</h2>
      <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-[2fr_2fr_1fr_auto] lg:items-end">
        <Field label="Item" required>
          <Select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
          >
            <option value="">Select item…</option>
            {items.map((it) => (
              <option key={it.id} value={it.id}>
                {it.sku} — {it.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Warehouse" required>
          <Select
            value={whID}
            onChange={(e) => setWhID(e.target.value)}
            required
          >
            <option value="">Select warehouse…</option>
            {warehouses.map((w) => (
              <option key={w.id} value={w.id}>
                {w.code} — {w.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Planned qty" required>
          <Input
            type="number"
            step="0.01"
            min="0"
            inputMode="decimal"
            className="text-right tabular-nums"
            value={plannedQty}
            onChange={(e) => setPlannedQty(e.target.value)}
            required
          />
        </Field>
        <Button
          type="submit"
          leadingIcon={<Plus className="size-4" />}
          disabled={createMut.isPending || !itemID || !whID}
        >
          {createMut.isPending ? "Creating…" : "Create work order"}
        </Button>
      </div>
      {createMut.isError ? (
        <p className="mt-2 text-xs text-danger">
          Couldn't create work order: {(createMut.error as Error).message}
        </p>
      ) : null}
    </form>
  );
}
