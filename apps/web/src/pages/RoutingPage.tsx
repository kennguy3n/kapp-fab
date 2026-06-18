import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  CreateRoutingInput,
  InventoryItem,
  Routing,
  RoutingStatus,
  WorkCenter,
  WorkCenterStatus,
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Textarea,
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, Factory, Plus, Trash2, Workflow } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";

const WC_STATUS_LABEL: Record<WorkCenterStatus, string> = {
  active: "Active",
  maintenance: "Maintenance",
  retired: "Retired",
};
const WC_STATUS_VARIANT: Record<WorkCenterStatus, BadgeProps["variant"]> = {
  active: "success",
  maintenance: "warning",
  retired: "danger",
};

const ROUTING_STATUS_LABEL: Record<RoutingStatus, string> = {
  draft: "Draft",
  active: "Active",
  obsolete: "Obsolete",
};
const ROUTING_STATUS_VARIANT: Record<RoutingStatus, BadgeProps["variant"]> = {
  draft: "neutral",
  active: "success",
  obsolete: "danger",
};

const ROUTING_FILTERS: RoutingStatus[] = ["draft", "active", "obsolete"];

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

function emptyOperation(): OperationDraft {
  return {
    operation_name: "",
    work_center_id: "",
    setup_time_minutes: "0",
    cycle_time_minutes: "1",
  };
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
  const fmt = useFormatter();
  const [routingFilter, setRoutingFilter] = useState<"" | RoutingStatus>("");

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

  const workCenters = workCentersQ.data ?? [];
  const routings = routingsQ.data ?? [];

  return (
    <section className="flex flex-col gap-6">
      <header className="flex flex-col gap-1">
        <Eyebrow>Manufacturing</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Routings & Work Centers
        </h1>
        <p className="text-sm text-fg-muted">
          Define the machines that do the work, then the step-by-step routings
          that turn raw materials into finished items. A work order copies an
          item's active routing and creates a job card for every step.
        </p>
      </header>

      <div className="grid gap-6 lg:grid-cols-2">
        <div className="flex min-w-0 flex-col gap-4">
          <div>
            <h2 className="text-base font-semibold tracking-tight text-fg">
              Work centers
            </h2>
            <p className="text-sm text-fg-muted">
              Machines or workstations with finite hourly capacity. Only active
              centers contribute schedulable minutes to the capacity plan.
            </p>
          </div>

          {workCentersQ.isLoading ? (
            <ListSkeleton />
          ) : workCentersQ.isError ? (
            <EmptyState
              icon={<AlertTriangle className="size-6" />}
              title="Couldn't load work centers"
              description={(workCentersQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void workCentersQ.refetch()}
                  disabled={workCentersQ.isFetching}
                >
                  Try again
                </Button>
              }
            />
          ) : workCenters.length === 0 ? (
            <EmptyState
              icon={<Factory className="size-6" />}
              title="No work centers yet"
              description="Add your first machine or workstation below to start routing work against it."
            />
          ) : (
            <div className="overflow-x-auto rounded-xl border border-border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Name</TableHead>
                    <TableHead className="text-right">Hrs/day</TableHead>
                    <TableHead className="text-right">Efficiency</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>
                      <span className="sr-only">Change status</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {workCenters.map((wc: WorkCenter) => (
                    <TableRow key={wc.id}>
                      <TableCell className="font-medium">{wc.name}</TableCell>
                      <TableCell className="text-right tabular-nums">
                        {fmt.number(Number(wc.operating_hours_per_day))}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {fmt.number(Number(wc.efficiency_percent))}%
                      </TableCell>
                      <TableCell>
                        <Badge variant={WC_STATUS_VARIANT[wc.status]}>
                          {WC_STATUS_LABEL[wc.status]}
                        </Badge>
                      </TableCell>
                      <TableCell>
                        {wc.status !== "retired" && (
                          <Select
                            size="sm"
                            aria-label={`Set status for ${wc.name}`}
                            value={wc.status}
                            onChange={(e) =>
                              setWCStatus.mutate({
                                id: wc.id,
                                status: e.target.value,
                              })
                            }
                            disabled={setWCStatus.isPending}
                          >
                            <option value="active">Active</option>
                            <option value="maintenance">Maintenance</option>
                            <option value="retired">Retired</option>
                          </Select>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}

          <WorkCenterForm />
        </div>

        <div className="flex min-w-0 flex-col gap-4">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <h2 className="text-base font-semibold tracking-tight text-fg">
                Routings
              </h2>
              <p className="text-sm text-fg-muted">
                Ordered operations for producing an item. Only one routing per
                item can be active at a time.
              </p>
            </div>
            <Field label="Status">
              <Select
                size="sm"
                value={routingFilter}
                onChange={(e) =>
                  setRoutingFilter(e.target.value as "" | RoutingStatus)
                }
              >
                <option value="">All</option>
                {ROUTING_FILTERS.map((s) => (
                  <option key={s} value={s}>
                    {ROUTING_STATUS_LABEL[s]}
                  </option>
                ))}
              </Select>
            </Field>
          </div>

          {routingsQ.isLoading ? (
            <ListSkeleton />
          ) : routingsQ.isError ? (
            <EmptyState
              icon={<AlertTriangle className="size-6" />}
              title="Couldn't load routings"
              description={(routingsQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void routingsQ.refetch()}
                  disabled={routingsQ.isFetching}
                >
                  Try again
                </Button>
              }
            />
          ) : routings.length === 0 ? (
            <EmptyState
              icon={<Workflow className="size-6" />}
              title="No routings yet"
              description="Author a routing below to describe how an item is produced, step by step."
            />
          ) : (
            <div className="overflow-x-auto rounded-xl border border-border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Item</TableHead>
                    <TableHead>Version</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead className="text-right">
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {routings.map((r: Routing) => (
                    <TableRow key={r.id}>
                      <TableCell className="font-medium">
                        {itemLabel.get(r.item_id) ?? r.item_id}
                      </TableCell>
                      <TableCell className="tabular-nums">{r.version}</TableCell>
                      <TableCell>
                        <Badge variant={ROUTING_STATUS_VARIANT[r.status]}>
                          {ROUTING_STATUS_LABEL[r.status]}
                        </Badge>
                      </TableCell>
                      <TableCell className="text-right">
                        {r.status === "draft" && (
                          <Button
                            size="sm"
                            variant="outline"
                            onClick={() =>
                              setRoutingStatus.mutate({
                                id: r.id,
                                status: "active",
                              })
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
                            Retire
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}

          <RoutingForm items={itemsQ.data ?? []} workCenters={workCenters} />
        </div>
      </div>
    </section>
  );
}

function ListSkeleton() {
  return (
    <div className="flex flex-col gap-2">
      <Skeleton className="h-9 w-full" />
      <Skeleton className="h-12 w-full" />
      <Skeleton className="h-12 w-full" />
    </div>
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

  const canSubmit = name.trim() !== "";

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (canSubmit) createMut.mutate();
      }}
      className="flex flex-col gap-4 rounded-xl border border-border bg-bg-subtle p-4"
    >
      <h3 className="text-sm font-semibold tracking-tight text-fg">
        Add work center
      </h3>
      <Field label="Name" required>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
          placeholder="e.g. CNC mill"
        />
      </Field>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <Field label="Capacity / hour" help="Units the center can make per hour.">
          <Input
            type="number"
            step="0.01"
            min="0"
            value={capacityPerHour}
            onChange={(e) => setCapacityPerHour(e.target.value)}
            required
            className="tabular-nums"
          />
        </Field>
        <Field label="Operating hrs / day">
          <Input
            type="number"
            step="0.01"
            min="0"
            value={hoursPerDay}
            onChange={(e) => setHoursPerDay(e.target.value)}
            required
            className="tabular-nums"
          />
        </Field>
        <Field label="Efficiency %" help="100 = runs at nameplate speed.">
          <Input
            type="number"
            step="0.01"
            min="0"
            value={efficiency}
            onChange={(e) => setEfficiency(e.target.value)}
            required
            className="tabular-nums"
          />
        </Field>
      </div>
      <Field label="Notes">
        <Textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          rows={2}
        />
      </Field>
      {createMut.isError && (
        <p className="text-sm text-danger">{(createMut.error as Error).message}</p>
      )}
      <div>
        <Button type="submit" disabled={createMut.isPending || !canSubmit}>
          {createMut.isPending ? "Saving…" : "Create work center"}
        </Button>
      </div>
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
    emptyOperation(),
  ]);

  const validOperations = operations.filter(
    (op) => op.operation_name.trim() !== "" && op.work_center_id !== "",
  );
  const canSubmit = itemID !== "" && validOperations.length > 0;

  const createMut = useMutation({
    mutationFn: () => {
      const input: CreateRoutingInput = {
        item_id: itemID,
        version,
        notes: notes || undefined,
        activate,
        operations: validOperations.map((op) => ({
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
      setOperations([emptyOperation()]);
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
        if (canSubmit) createMut.mutate();
      }}
      className="flex flex-col gap-4 rounded-xl border border-border bg-bg-subtle p-4"
    >
      <h3 className="text-sm font-semibold tracking-tight text-fg">
        Author routing
      </h3>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
        <Field label="Version" required>
          <Input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            required
          />
        </Field>
      </div>
      <Field label="Notes">
        <Textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          rows={2}
        />
      </Field>

      <fieldset className="m-0 flex flex-col gap-2 border-0 p-0">
        <legend className="text-sm font-semibold text-fg">Operations</legend>
        <p className="text-xs text-fg-muted">
          Steps run top to bottom. Each becomes a job card when a work order is
          released.
        </p>
        <div className="overflow-x-auto rounded-lg border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10 text-right">#</TableHead>
                <TableHead>Operation</TableHead>
                <TableHead>Work center</TableHead>
                <TableHead className="text-right">Setup (min)</TableHead>
                <TableHead className="text-right">Cycle (min)</TableHead>
                <TableHead>
                  <span className="sr-only">Remove</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {operations.map((op, idx) => (
                <TableRow key={idx}>
                  <TableCell className="text-right tabular-nums text-fg-muted">
                    {idx + 1}
                  </TableCell>
                  <TableCell>
                    <Input
                      aria-label={`Operation ${idx + 1} name`}
                      value={op.operation_name}
                      onChange={(e) =>
                        updateOperation(idx, { operation_name: e.target.value })
                      }
                      placeholder="e.g. Cut"
                    />
                  </TableCell>
                  <TableCell>
                    <Select
                      aria-label={`Operation ${idx + 1} work center`}
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
                      aria-label={`Operation ${idx + 1} setup minutes`}
                      type="number"
                      step="0.01"
                      min="0"
                      value={op.setup_time_minutes}
                      onChange={(e) =>
                        updateOperation(idx, {
                          setup_time_minutes: e.target.value,
                        })
                      }
                      className="w-20 tabular-nums"
                    />
                  </TableCell>
                  <TableCell>
                    <Input
                      aria-label={`Operation ${idx + 1} cycle minutes`}
                      type="number"
                      step="0.01"
                      min="0"
                      value={op.cycle_time_minutes}
                      onChange={(e) =>
                        updateOperation(idx, {
                          cycle_time_minutes: e.target.value,
                        })
                      }
                      className="w-20 tabular-nums"
                    />
                  </TableCell>
                  <TableCell>
                    <Button
                      type="button"
                      size="icon"
                      variant="ghost"
                      aria-label={`Remove operation ${idx + 1}`}
                      onClick={() =>
                        setOperations((prev) =>
                          prev.filter((_, i) => i !== idx),
                        )
                      }
                      disabled={operations.length === 1}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
        <div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            leadingIcon={<Plus className="size-4" />}
            onClick={() => setOperations((prev) => [...prev, emptyOperation()])}
          >
            Add operation
          </Button>
        </div>
      </fieldset>

      <label className="flex items-start gap-2 text-sm text-fg">
        <input
          type="checkbox"
          className="mt-0.5 size-4 rounded-sm accent-(--accent)"
          checked={activate}
          onChange={(e) => setActivate(e.target.checked)}
        />
        <span>
          Activate on create
          <span className="block text-xs text-fg-muted">
            Makes this the live routing and retires the item's current active
            one.
          </span>
        </span>
      </label>
      {createMut.isError && (
        <p className="text-sm text-danger">{(createMut.error as Error).message}</p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" disabled={createMut.isPending || !canSubmit}>
          {createMut.isPending ? "Saving…" : "Create routing"}
        </Button>
        {!canSubmit && (
          <span className="text-xs text-fg-muted">
            Pick an item and add at least one named operation with a work
            center.
          </span>
        )}
      </div>
    </form>
  );
}
