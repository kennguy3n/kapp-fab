import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  CreateRoutingInput,
  InventoryItem,
  Routing,
  WorkCenter,
} from "@kapp/client";
import { api } from "../lib/api";

// OperationDraft is the in-form shape used while authoring routing
// operations, before any have been persisted. It omits routing_id
// (assigned server-side) and sequence (the server derives it from array
// position — see CreateRoutingInput in @kapp/client), mirroring the
// server contract so the UI never lets a user dial in a sequence that
// would be silently overridden on POST.
interface OperationDraft {
  operation_name: string;
  work_center_id: string;
  setup_time_minutes: string;
  cycle_time_minutes: string;
  description?: string;
}

/**
 * RoutingPage renders the Stream 2 manufacturing-depth authoring
 * surface. The model is:
 *   - A work center is a machine / workstation with finite hourly
 *     capacity. Status moves active → maintenance / retired; a
 *     non-active center contributes zero schedulable minutes.
 *   - A routing is a versioned, ordered sequence of operations for an
 *     item. Status moves draft → active → obsolete, and only one
 *     routing per item may be active at a time (enforced by a partial
 *     unique index). A work order snapshots the active routing at
 *     release time and generates one job card per operation.
 *
 * Routings reference work centers, so both are managed here: the
 * left column lists/authors work centers, the right column
 * lists/authors routings against them.
 */
export function RoutingPage() {
  const qc = useQueryClient();
  const [routingFilter, setRoutingFilter] = useState<
    "" | "draft" | "active" | "obsolete"
  >("");

  const workCentersQ = useQuery({
    queryKey: ["mfg", "work-centers"],
    queryFn: () => api.listWorkCenters(),
  });
  const routingsQ = useQuery({
    queryKey: ["mfg", "routings", routingFilter],
    queryFn: () => api.listRoutings(routingFilter || undefined),
  });
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });

  const setRoutingStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) =>
      api.setRoutingStatus(id, status),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mfg", "routings"] }),
  });
  const setWCStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) =>
      api.setWorkCenterStatus(id, status),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mfg", "work-centers"] }),
  });

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it: InventoryItem) =>
      m.set(it.id, `${it.sku} — ${it.name}`),
    );
    return m;
  }, [itemsQ.data]);

  const wcLabel = useMemo(() => {
    const m = new Map<string, string>();
    (workCentersQ.data ?? []).forEach((wc: WorkCenter) => m.set(wc.id, wc.name));
    return m;
  }, [workCentersQ.data]);

  return (
    <section style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 24 }}>
      <div>
        <h1>Work Centers</h1>
        <p style={{ color: "#6b7280" }}>
          Machines / workstations with finite hourly capacity. Only active
          centers contribute schedulable minutes to the capacity plan.
        </p>
        {workCentersQ.isLoading && <p>Loading…</p>}
        {workCentersQ.isError && (
          <p style={{ color: "#dc2626" }}>{String(workCentersQ.error)}</p>
        )}
        {workCentersQ.data && (
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th>Name</th>
                <th>Hrs/day</th>
                <th>Eff %</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {workCentersQ.data.map((wc: WorkCenter) => (
                <tr key={wc.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td>{wc.name}</td>
                  <td>{wc.operating_hours_per_day}</td>
                  <td>{wc.efficiency_percent}</td>
                  <td>
                    <StatusPill status={wc.status} />
                  </td>
                  <td>
                    {wc.status !== "retired" && (
                      <select
                        aria-label={`Set status for ${wc.name}`}
                        value={wc.status}
                        onChange={(e) =>
                          setWCStatus.mutate({ id: wc.id, status: e.target.value })
                        }
                        disabled={setWCStatus.isPending}
                      >
                        <option value="active">active</option>
                        <option value="maintenance">maintenance</option>
                        <option value="retired">retired</option>
                      </select>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <WorkCenterForm />
      </div>

      <div>
        <h1>Routings</h1>
        <p style={{ color: "#6b7280" }}>
          Ordered operations for producing an item. Only one routing per item
          may be active at a time.
        </p>
        <div style={{ marginBottom: 8 }}>
          <label htmlFor="routing-filter" style={{ marginRight: 8 }}>
            Status:
          </label>
          <select
            id="routing-filter"
            value={routingFilter}
            onChange={(e) =>
              setRoutingFilter(
                e.target.value as "" | "draft" | "active" | "obsolete",
              )
            }
          >
            <option value="">All</option>
            <option value="draft">Draft</option>
            <option value="active">Active</option>
            <option value="obsolete">Obsolete</option>
          </select>
        </div>
        {routingsQ.isLoading && <p>Loading…</p>}
        {routingsQ.isError && (
          <p style={{ color: "#dc2626" }}>{String(routingsQ.error)}</p>
        )}
        {routingsQ.data && (
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th>Item</th>
                <th>Version</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {routingsQ.data.map((r: Routing) => (
                <tr key={r.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td>{itemLabel.get(r.item_id) ?? r.item_id}</td>
                  <td>{r.version}</td>
                  <td>
                    <StatusPill status={r.status} />
                  </td>
                  <td>
                    {r.status === "draft" && (
                      <button
                        onClick={() =>
                          setRoutingStatus.mutate({ id: r.id, status: "active" })
                        }
                        disabled={setRoutingStatus.isPending}
                      >
                        Activate
                      </button>
                    )}
                    {r.status === "active" && (
                      <button
                        onClick={() =>
                          setRoutingStatus.mutate({
                            id: r.id,
                            status: "obsolete",
                          })
                        }
                        disabled={setRoutingStatus.isPending}
                      >
                        Obsolete
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <RoutingForm
          items={itemsQ.data ?? []}
          workCenters={workCentersQ.data ?? []}
          wcLabel={wcLabel}
        />
      </div>
    </section>
  );
}

function StatusPill({ status }: { status: string }) {
  const background =
    status === "active"
      ? "#dcfce7"
      : status === "obsolete" || status === "retired"
        ? "#fee2e2"
        : status === "maintenance"
          ? "#fef9c3"
          : "#e5e7eb";
  return (
    <span style={{ padding: "2px 8px", borderRadius: 12, background }}>
      {status}
    </span>
  );
}

function WorkCenterForm() {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [capacityPerHour, setCapacityPerHour] = useState("1");
  const [hoursPerDay, setHoursPerDay] = useState("8");
  const [efficiency, setEfficiency] = useState("100");
  const [notes, setNotes] = useState("");

  const createMut = useMutation({
    mutationFn: () =>
      api.createWorkCenter({
        name,
        capacity_per_hour: capacityPerHour,
        operating_hours_per_day: hoursPerDay,
        efficiency_percent: efficiency,
        notes: notes || undefined,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mfg", "work-centers"] });
      setName("");
      setCapacityPerHour("1");
      setHoursPerDay("8");
      setEfficiency("100");
      setNotes("");
    },
  });

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        createMut.mutate();
      }}
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        padding: 16,
        marginTop: 16,
      }}
    >
      <h2 style={{ marginTop: 0 }}>Add Work Center</h2>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8 }}>
        <label>
          Name
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
        <label>
          Capacity / hour
          <input
            type="number"
            step="0.01"
            value={capacityPerHour}
            onChange={(e) => setCapacityPerHour(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
        <label>
          Operating hrs / day
          <input
            type="number"
            step="0.01"
            value={hoursPerDay}
            onChange={(e) => setHoursPerDay(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
        <label>
          Efficiency %
          <input
            type="number"
            step="0.01"
            value={efficiency}
            onChange={(e) => setEfficiency(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
      </div>
      <label style={{ display: "block", marginTop: 8 }}>
        Notes
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          style={{ width: "100%" }}
        />
      </label>
      {createMut.isError && (
        <p style={{ color: "#dc2626" }}>{String(createMut.error)}</p>
      )}
      <button type="submit" disabled={createMut.isPending} style={{ marginTop: 8 }}>
        {createMut.isPending ? "Saving…" : "Create work center"}
      </button>
    </form>
  );
}

interface RoutingFormProps {
  items: InventoryItem[];
  workCenters: WorkCenter[];
  wcLabel: Map<string, string>;
}

function RoutingForm({ items, workCenters }: RoutingFormProps) {
  const qc = useQueryClient();
  const [itemID, setItemID] = useState("");
  const [version, setVersion] = useState("v1");
  const [notes, setNotes] = useState("");
  const [activate, setActivate] = useState(false);
  const [operations, setOperations] = useState<OperationDraft[]>([
    {
      operation_name: "",
      work_center_id: "",
      setup_time_minutes: "0",
      cycle_time_minutes: "1",
    },
  ]);

  const createMut = useMutation({
    mutationFn: () => {
      const input: CreateRoutingInput = {
        item_id: itemID,
        version,
        notes: notes || undefined,
        activate,
        operations: operations
          .filter((op) => op.operation_name && op.work_center_id)
          .map((op) => ({
            operation_name: op.operation_name,
            work_center_id: op.work_center_id,
            setup_time_minutes: op.setup_time_minutes,
            cycle_time_minutes: op.cycle_time_minutes,
            description: op.description || undefined,
          })),
      };
      return api.createRouting(input);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mfg", "routings"] });
      // Reset every field — the `activate` checkbox is load-bearing:
      // leaving it checked would silently promote the next routing to
      // active and auto-obsolete the item's previously-active routing,
      // so the user must opt in each time.
      setItemID("");
      setVersion("v1");
      setNotes("");
      setActivate(false);
      setOperations([
        {
          operation_name: "",
          work_center_id: "",
          setup_time_minutes: "0",
          cycle_time_minutes: "1",
        },
      ]);
    },
  });

  const updateOperation = (idx: number, patch: Partial<OperationDraft>) => {
    setOperations((prev) =>
      prev.map((op, i) => (i === idx ? { ...op, ...patch } : op)),
    );
  };

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        createMut.mutate();
      }}
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        padding: 16,
        marginTop: 16,
      }}
    >
      <h2 style={{ marginTop: 0 }}>Author Routing</h2>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8 }}>
        <label>
          Item
          <select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
            style={{ width: "100%" }}
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
          Version
          <input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
      </div>
      <label style={{ display: "block", marginTop: 8 }}>
        Notes
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          style={{ width: "100%" }}
        />
      </label>

      <h3>Operations</h3>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th>Seq</th>
            <th>Operation</th>
            <th>Work center</th>
            <th>Setup (min)</th>
            <th>Cycle (min)</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {operations.map((op, idx) => (
            <tr key={idx}>
              <td>{idx + 1}</td>
              <td>
                <input
                  value={op.operation_name}
                  onChange={(e) =>
                    updateOperation(idx, { operation_name: e.target.value })
                  }
                  placeholder="e.g. Cut"
                />
              </td>
              <td>
                <select
                  value={op.work_center_id}
                  onChange={(e) =>
                    updateOperation(idx, { work_center_id: e.target.value })
                  }
                >
                  <option value="">Select…</option>
                  {workCenters.map((wc) => (
                    <option key={wc.id} value={wc.id}>
                      {wc.name}
                    </option>
                  ))}
                </select>
              </td>
              <td>
                <input
                  type="number"
                  step="0.01"
                  value={op.setup_time_minutes}
                  onChange={(e) =>
                    updateOperation(idx, { setup_time_minutes: e.target.value })
                  }
                  style={{ width: 80 }}
                />
              </td>
              <td>
                <input
                  type="number"
                  step="0.01"
                  value={op.cycle_time_minutes}
                  onChange={(e) =>
                    updateOperation(idx, { cycle_time_minutes: e.target.value })
                  }
                  style={{ width: 80 }}
                />
              </td>
              <td>
                <button
                  type="button"
                  onClick={() =>
                    setOperations((prev) => prev.filter((_, i) => i !== idx))
                  }
                  disabled={operations.length === 1}
                >
                  ✕
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <button
        type="button"
        onClick={() =>
          setOperations((prev) => [
            ...prev,
            {
              operation_name: "",
              work_center_id: "",
              setup_time_minutes: "0",
              cycle_time_minutes: "1",
            },
          ])
        }
        style={{ marginTop: 8 }}
      >
        + Add operation
      </button>

      <label style={{ display: "block", marginTop: 12 }}>
        <input
          type="checkbox"
          checked={activate}
          onChange={(e) => setActivate(e.target.checked)}
        />{" "}
        Activate on create (obsoletes any currently-active routing for this item)
      </label>
      {createMut.isError && (
        <p style={{ color: "#dc2626" }}>{String(createMut.error)}</p>
      )}
      <button type="submit" disabled={createMut.isPending} style={{ marginTop: 8 }}>
        {createMut.isPending ? "Saving…" : "Create routing"}
      </button>
    </form>
  );
}
