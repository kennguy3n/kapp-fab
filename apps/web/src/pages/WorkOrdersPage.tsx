import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  InventoryItem,
  InventoryWarehouse,
  WorkOrder,
} from "@kapp/client";
import { Button, Input, Select } from "@kapp/ui";
import { api } from "../lib/api";

// COLUMNS lists the always-visible kanban lanes. `closed` is
// deliberately NOT here: closed work orders are archival and
// would crowd the active board if shown by default. They are
// surfaced via a `Show closed (N)` toggle that appends a sixth
// lane on demand (see CLOSED_COLUMN + showClosed below). Keeping
// it as a toggleable column rather than a separate page preserves
// the kanban metaphor and lets operators retrieve a closed order
// without leaving the work-orders surface.
const COLUMNS: Array<{
  status: WorkOrder["status"];
  label: string;
  accent: string;
}> = [
  { status: "draft", label: "Draft", accent: "bg-bg-muted" },
  { status: "released", label: "Released", accent: "bg-info/15" },
  { status: "in_progress", label: "In Progress", accent: "bg-warning/25" },
  { status: "completed", label: "Completed", accent: "bg-success/15" },
  { status: "cancelled", label: "Cancelled", accent: "bg-danger/15" },
];

// CLOSED_COLUMN is the on-demand sixth lane, conditionally
// appended to the rendered set when the user toggles
// `Show closed`. Pulling its shape out keeps the rendering loop
// uniform and avoids special-casing the closed bucket downstream.
const CLOSED_COLUMN: (typeof COLUMNS)[number] = {
  status: "closed",
  label: "Closed",
  accent: "bg-accent/15",
};

/**
 * WorkOrdersPage renders a kanban view of work orders bucketed by
 * status. Each card exposes the legal state-machine transitions:
 * draft→release, released→start|complete, in_progress→complete, etc.
 * The complete action emits the inventory moves (consumption +
 * receipt) atomically on the server side.
 */
export function WorkOrdersPage() {
  const qc = useQueryClient();
  // showClosed defaults to false so the active board stays
  // uncluttered. The count in the toggle label is sourced from
  // the same `grouped` map the columns render against, so it's
  // always live with the latest server snapshot.
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
      action: "release" | "start" | "complete" | "cancel" | "close";
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
    const m = new Map<WorkOrder["status"], WorkOrder[]>();
    COLUMNS.forEach((c) => m.set(c.status, []));
    // Pre-allocate the closed bucket so the count is correct
    // even when no closed orders exist yet (Map.get returns
    // undefined for missing keys, which would render "NaN").
    m.set(CLOSED_COLUMN.status, []);
    (wosQ.data ?? []).forEach((wo: WorkOrder) => {
      const arr = m.get(wo.status) ?? [];
      arr.push(wo);
      m.set(wo.status, arr);
    });
    return m;
  }, [wosQ.data]);

  // visibleColumns is COLUMNS plus the closed lane iff the user
  // toggled it on. Computing once per render is fine; the array
  // is at most 6 entries.
  const visibleColumns = showClosed ? [...COLUMNS, CLOSED_COLUMN] : COLUMNS;
  const closedCount = (grouped.get(CLOSED_COLUMN.status) ?? []).length;

  return (
    <section>
      <h1>Work Orders</h1>
      <p className="text-fg-muted">
        Kanban of production runs. Completing a work order emits the
        consumption + receipt inventory moves atomically.
      </p>

      <CreateWorkOrderForm
        items={itemsQ.data ?? []}
        warehouses={whQ.data ?? []}
      />

      {wosQ.isLoading && <p>Loading…</p>}
      {wosQ.isError && (
        <p className="text-danger">{String(wosQ.error)}</p>
      )}
      <div className="mt-3">
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
      <div
        className="mt-4 grid gap-3"
        style={{
          gridTemplateColumns: `repeat(${visibleColumns.length}, 1fr)`,
        }}
      >
        {visibleColumns.map((col) => (
          <div
            key={col.status}
            className="min-h-[200px] rounded-lg bg-bg-subtle p-2"
          >
            <div
              className={`mb-2 rounded px-2 py-1 font-semibold ${col.accent}`}
            >
              {col.label} ({(grouped.get(col.status) ?? []).length})
            </div>
            {(grouped.get(col.status) ?? []).map((wo) => (
              <WorkOrderCard
                key={wo.id}
                wo={wo}
                itemLabel={itemLabel}
                whLabel={whLabel}
                onTransition={(action, actualQty) =>
                  transitionMut.mutate({ id: wo.id, action, actualQty })
                }
                disabled={transitionMut.isPending}
              />
            ))}
          </div>
        ))}
      </div>
    </section>
  );
}

interface WorkOrderCardProps {
  wo: WorkOrder;
  itemLabel: Map<string, string>;
  whLabel: Map<string, string>;
  onTransition: (
    action: "release" | "start" | "complete" | "cancel" | "close",
    actualQty?: string,
  ) => void;
  disabled: boolean;
}

function WorkOrderCard({
  wo,
  itemLabel,
  whLabel,
  onTransition,
  disabled,
}: WorkOrderCardProps) {
  const [actual, setActual] = useState(wo.planned_qty);
  return (
    <div className="mb-2 rounded-md border border-border bg-bg-elevated p-2 text-[13px]">
      <div className="font-semibold">
        {itemLabel.get(wo.item_id) ?? wo.item_id}
      </div>
      <div className="text-fg-muted">{whLabel.get(wo.warehouse_id)}</div>
      <div>
        Planned: {wo.planned_qty}
        {wo.actual_qty ? <> · Actual: {wo.actual_qty}</> : null}
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-1">
        {wo.status === "draft" && (
          <Button size="sm" variant="outline" onClick={() => onTransition("release")} disabled={disabled}>
            Release
          </Button>
        )}
        {wo.status === "released" && (
          <Button size="sm" variant="outline" onClick={() => onTransition("start")} disabled={disabled}>
            Start
          </Button>
        )}
        {(wo.status === "released" || wo.status === "in_progress") && (
          <>
            <Input
              aria-label="actual qty"
              type="number"
              step="0.01"
              value={actual}
              onChange={(e) => setActual(e.target.value)}
              className="w-16"
            />
            <Button
              size="sm"
              variant="outline"
              onClick={() => onTransition("complete", actual)}
              disabled={disabled}
            >
              Complete
            </Button>
          </>
        )}
        {(wo.status === "draft" ||
          wo.status === "released" ||
          wo.status === "in_progress") && (
          <Button size="sm" variant="outline" onClick={() => onTransition("cancel")} disabled={disabled}>
            Cancel
          </Button>
        )}
        {wo.status === "completed" && (
          <Button size="sm" variant="outline" onClick={() => onTransition("close")} disabled={disabled}>
            Close
          </Button>
        )}
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
      className="flex items-end gap-2 rounded-md bg-bg-subtle p-3"
    >
      <label className="flex flex-col gap-1 text-[13px]">
        Item
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
      </label>
      <label className="flex flex-col gap-1 text-[13px]">
        Warehouse
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
      </label>
      <label className="flex flex-col gap-1 text-[13px]">
        Planned qty
        <Input
          type="number"
          step="0.01"
          value={plannedQty}
          onChange={(e) => setPlannedQty(e.target.value)}
          required
          className="w-[100px]"
        />
      </label>
      <Button type="submit" disabled={createMut.isPending}>
        {createMut.isPending ? "Creating…" : "Create work order"}
      </Button>
      {createMut.isError && (
        <span className="text-danger">{String(createMut.error)}</span>
      )}
    </form>
  );
}
