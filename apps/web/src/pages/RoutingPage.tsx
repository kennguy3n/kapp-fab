import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  CreateRoutingInput,
  InventoryItem,
  Routing,
  WorkCenter,
} from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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

  return (
    <section className="grid grid-cols-2 gap-6">
      <div>
        <h1>Work Centers</h1>
        <p className="text-fg-muted">
          Machines / workstations with finite hourly capacity. Only active
          centers contribute schedulable minutes to the capacity plan.
        </p>
        {workCentersQ.isLoading && <p>Loading…</p>}
        {workCentersQ.isError && (
          <p className="text-danger">{String(workCentersQ.error)}</p>
        )}
        {workCentersQ.data && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Hrs/day</TableHead>
                <TableHead>Eff %</TableHead>
                <TableHead>Status</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {workCentersQ.data.map((wc: WorkCenter) => (
                <TableRow key={wc.id}>
                  <TableCell>{wc.name}</TableCell>
                  <TableCell>{wc.operating_hours_per_day}</TableCell>
                  <TableCell>{wc.efficiency_percent}</TableCell>
                  <TableCell>
                    <StatusPill status={wc.status} />
                  </TableCell>
                  <TableCell>
                    {wc.status !== "retired" && (
                      <Select
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
                      </Select>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <WorkCenterForm />
      </div>

      <div>
        <h1>Routings</h1>
        <p className="text-fg-muted">
          Ordered operations for producing an item. Only one routing per item
          may be active at a time.
        </p>
        <div className="mb-2 flex items-center gap-2">
          <label htmlFor="routing-filter">Status:</label>
          <Select
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
          </Select>
        </div>
        {routingsQ.isLoading && <p>Loading…</p>}
        {routingsQ.isError && (
          <p className="text-danger">{String(routingsQ.error)}</p>
        )}
        {routingsQ.data && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Item</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Status</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {routingsQ.data.map((r: Routing) => (
                <TableRow key={r.id}>
                  <TableCell>{itemLabel.get(r.item_id) ?? r.item_id}</TableCell>
                  <TableCell>{r.version}</TableCell>
                  <TableCell>
                    <StatusPill status={r.status} />
                  </TableCell>
                  <TableCell>
                    {r.status === "draft" && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() =>
                          setRoutingStatus.mutate({ id: r.id, status: "active" })
                        }
                        disabled={setRoutingStatus.isPending}
                      >
                        Activate
                      </Button>
                    )}
                    {r.status === "active" && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() =>
                          setRoutingStatus.mutate({
                            id: r.id,
                            status: "obsolete",
                          })
                        }
                        disabled={setRoutingStatus.isPending}
                      >
                        Obsolete
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <RoutingForm
          items={itemsQ.data ?? []}
          workCenters={workCentersQ.data ?? []}
        />
      </div>
    </section>
  );
}

function StatusPill({ status }: { status: string }) {
  const variant =
    status === "active"
      ? "success"
      : status === "obsolete" || status === "retired"
        ? "danger"
        : status === "maintenance"
          ? "warning"
          : "default";
  return <Badge variant={variant}>{status}</Badge>;
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
      className="mt-4 rounded-lg border border-border p-4"
    >
      <h2 className="mt-0">Add Work Center</h2>
      <div className="grid grid-cols-2 gap-2">
        <label>
          Name
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            className="w-full"
          />
        </label>
        <label>
          Capacity / hour
          <Input
            type="number"
            step="0.01"
            value={capacityPerHour}
            onChange={(e) => setCapacityPerHour(e.target.value)}
            required
            className="w-full"
          />
        </label>
        <label>
          Operating hrs / day
          <Input
            type="number"
            step="0.01"
            value={hoursPerDay}
            onChange={(e) => setHoursPerDay(e.target.value)}
            required
            className="w-full"
          />
        </label>
        <label>
          Efficiency %
          <Input
            type="number"
            step="0.01"
            value={efficiency}
            onChange={(e) => setEfficiency(e.target.value)}
            required
            className="w-full"
          />
        </label>
      </div>
      <label className="mt-2 block">
        Notes
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          className="w-full rounded-md border border-border bg-bg px-2 py-1.5 text-sm text-fg outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
        />
      </label>
      {createMut.isError && (
        <p className="text-danger">{String(createMut.error)}</p>
      )}
      <Button type="submit" disabled={createMut.isPending} className="mt-2">
        {createMut.isPending ? "Saving…" : "Create work center"}
      </Button>
    </form>
  );
}

interface RoutingFormProps {
  items: InventoryItem[];
  workCenters: WorkCenter[];
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
      className="mt-4 rounded-lg border border-border p-4"
    >
      <h2 className="mt-0">Author Routing</h2>
      <div className="grid grid-cols-2 gap-2">
        <label>
          Item
          <Select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
            className="w-full"
          >
            <option value="">Select item…</option>
            {items.map((it) => (
              <option key={it.id} value={it.id}>
                {it.sku} — {it.name}
              </option>
            ))}
          </Select>
        </label>
        <label>
          Version
          <Input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            required
            className="w-full"
          />
        </label>
      </div>
      <label className="mt-2 block">
        Notes
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          className="w-full rounded-md border border-border bg-bg px-2 py-1.5 text-sm text-fg outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
        />
      </label>

      <h3>Operations</h3>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Seq</TableHead>
            <TableHead>Operation</TableHead>
            <TableHead>Work center</TableHead>
            <TableHead>Setup (min)</TableHead>
            <TableHead>Cycle (min)</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {operations.map((op, idx) => (
            <TableRow key={idx}>
              <TableCell>{idx + 1}</TableCell>
              <TableCell>
                <Input
                  value={op.operation_name}
                  onChange={(e) =>
                    updateOperation(idx, { operation_name: e.target.value })
                  }
                  placeholder="e.g. Cut"
                />
              </TableCell>
              <TableCell>
                <Select
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
                </Select>
              </TableCell>
              <TableCell>
                <Input
                  type="number"
                  step="0.01"
                  value={op.setup_time_minutes}
                  onChange={(e) =>
                    updateOperation(idx, { setup_time_minutes: e.target.value })
                  }
                  className="w-20"
                />
              </TableCell>
              <TableCell>
                <Input
                  type="number"
                  step="0.01"
                  value={op.cycle_time_minutes}
                  onChange={(e) =>
                    updateOperation(idx, { cycle_time_minutes: e.target.value })
                  }
                  className="w-20"
                />
              </TableCell>
              <TableCell>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() =>
                    setOperations((prev) => prev.filter((_, i) => i !== idx))
                  }
                  disabled={operations.length === 1}
                >
                  ✕
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <Button
        type="button"
        variant="outline"
        size="sm"
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
        className="mt-2"
      >
        + Add operation
      </Button>

      <label className="mt-3 block">
        <input
          type="checkbox"
          checked={activate}
          onChange={(e) => setActivate(e.target.checked)}
        />{" "}
        Activate on create (obsoletes any currently-active routing for this item)
      </label>
      {createMut.isError && (
        <p className="text-danger">{String(createMut.error)}</p>
      )}
      <Button type="submit" disabled={createMut.isPending} className="mt-2">
        {createMut.isPending ? "Saving…" : "Create routing"}
      </Button>
    </form>
  );
}
