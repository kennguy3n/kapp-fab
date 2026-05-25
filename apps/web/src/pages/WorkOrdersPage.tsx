import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  InventoryItem,
  InventoryWarehouse,
  WorkOrder,
} from "@kapp/client";
import { api } from "../lib/api";

const COLUMNS: Array<{
  status: WorkOrder["status"];
  label: string;
  accent: string;
}> = [
  { status: "draft", label: "Draft", accent: "#e5e7eb" },
  { status: "released", label: "Released", accent: "#dbeafe" },
  { status: "in_progress", label: "In Progress", accent: "#fde68a" },
  { status: "completed", label: "Completed", accent: "#dcfce7" },
  { status: "cancelled", label: "Cancelled", accent: "#fecaca" },
];

/**
 * WorkOrdersPage renders a kanban view of work orders bucketed by
 * status. Each card exposes the legal state-machine transitions:
 * draft→release, released→start|complete, in_progress→complete, etc.
 * The complete action emits the inventory moves (consumption +
 * receipt) atomically on the server side.
 */
export function WorkOrdersPage() {
  const qc = useQueryClient();
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
    m.set("closed", []);
    (wosQ.data ?? []).forEach((wo: WorkOrder) => {
      const arr = m.get(wo.status) ?? [];
      arr.push(wo);
      m.set(wo.status, arr);
    });
    return m;
  }, [wosQ.data]);

  return (
    <section>
      <h1>Work Orders</h1>
      <p style={{ color: "#6b7280" }}>
        Kanban of production runs. Completing a work order emits the
        consumption + receipt inventory moves atomically.
      </p>

      <CreateWorkOrderForm
        items={itemsQ.data ?? []}
        warehouses={whQ.data ?? []}
      />

      {wosQ.isLoading && <p>Loading…</p>}
      {wosQ.isError && (
        <p style={{ color: "#dc2626" }}>{String(wosQ.error)}</p>
      )}
      <div
        style={{
          display: "grid",
          gridTemplateColumns: `repeat(${COLUMNS.length}, 1fr)`,
          gap: 12,
          marginTop: 16,
        }}
      >
        {COLUMNS.map((col) => (
          <div
            key={col.status}
            style={{
              borderRadius: 8,
              padding: 8,
              background: "#f9fafb",
              minHeight: 200,
            }}
          >
            <div
              style={{
                fontWeight: 600,
                marginBottom: 8,
                background: col.accent,
                padding: "4px 8px",
                borderRadius: 4,
              }}
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
    <div
      style={{
        background: "white",
        border: "1px solid #e5e7eb",
        borderRadius: 6,
        padding: 8,
        marginBottom: 8,
        fontSize: 13,
      }}
    >
      <div style={{ fontWeight: 600 }}>
        {itemLabel.get(wo.item_id) ?? wo.item_id}
      </div>
      <div style={{ color: "#6b7280" }}>{whLabel.get(wo.warehouse_id)}</div>
      <div>
        Planned: {wo.planned_qty}
        {wo.actual_qty ? <> · Actual: {wo.actual_qty}</> : null}
      </div>
      <div style={{ display: "flex", gap: 4, marginTop: 8, flexWrap: "wrap" }}>
        {wo.status === "draft" && (
          <button onClick={() => onTransition("release")} disabled={disabled}>
            Release
          </button>
        )}
        {wo.status === "released" && (
          <>
            <button onClick={() => onTransition("start")} disabled={disabled}>
              Start
            </button>
          </>
        )}
        {(wo.status === "released" || wo.status === "in_progress") && (
          <>
            <input
              aria-label="actual qty"
              type="number"
              step="0.01"
              value={actual}
              onChange={(e) => setActual(e.target.value)}
              style={{ width: 60 }}
            />
            <button
              onClick={() => onTransition("complete", actual)}
              disabled={disabled}
            >
              Complete
            </button>
          </>
        )}
        {(wo.status === "draft" ||
          wo.status === "released" ||
          wo.status === "in_progress") && (
          <button onClick={() => onTransition("cancel")} disabled={disabled}>
            Cancel
          </button>
        )}
        {wo.status === "completed" && (
          <button onClick={() => onTransition("close")} disabled={disabled}>
            Close
          </button>
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
      style={{
        display: "flex",
        gap: 8,
        alignItems: "flex-end",
        padding: 12,
        background: "#f9fafb",
        borderRadius: 6,
      }}
    >
      <label>
        Item
        <select
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
        </select>
      </label>
      <label>
        Warehouse
        <select
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
        </select>
      </label>
      <label>
        Planned qty
        <input
          type="number"
          step="0.01"
          value={plannedQty}
          onChange={(e) => setPlannedQty(e.target.value)}
          required
          style={{ width: 100 }}
        />
      </label>
      <button type="submit" disabled={createMut.isPending}>
        {createMut.isPending ? "Creating…" : "Create work order"}
      </button>
      {createMut.isError && (
        <span style={{ color: "#dc2626" }}>{String(createMut.error)}</span>
      )}
    </form>
  );
}
