import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  Stepper,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
  type StepperStep,
} from "@kapp/ui";
import {
  AlertTriangle,
  ClipboardList,
  Plus,
  Trash2,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import type {
  CycleCountLine,
  CycleCountSession,
  InventoryItem,
  InventoryWarehouse,
} from "@kapp/client";

type CycleStatus = CycleCountSession["status"];
type Formatters = ReturnType<typeof useFormatter>;

const STATUS_META: Record<
  CycleStatus,
  { label: string; step: number; variant: BadgeProps["variant"] }
> = {
  draft: { label: "Draft", step: 0, variant: "default" },
  counting: { label: "Counting", step: 1, variant: "info" },
  reconciled: { label: "In review", step: 2, variant: "warning" },
  posted: { label: "Posted", step: 3, variant: "success" },
};

const STEPS: StepperStep[] = [
  { label: "Set up", description: "Pick items" },
  { label: "Count", description: "Enter counts" },
  { label: "Review", description: "Check variances" },
  { label: "Post", description: "Commit moves" },
];

/**
 * CycleCountPage guides the operator through the cycle-count workflow:
 *
 *   draft  →  counting  →  reconciled  →  posted
 *
 * The operator creates a session for a warehouse, seeds expected_qty
 * from stock_levels, walks the warehouse keying counted_qty against
 * each line, reviews the variances, then posts — at which point the
 * backend writes a variance move for every line where expected !=
 * counted, valued at each item's stored moving-average cost.
 */
export function CycleCountPage() {
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [statusFilter, setStatusFilter] = useState<string>("");

  const list = useQuery({
    queryKey: ["cycle-counts", "list", statusFilter],
    queryFn: () =>
      api.listCycleCountSessions(
        statusFilter ? { status: statusFilter } : undefined,
      ),
  });

  const detail = useQuery({
    queryKey: ["cycle-counts", "detail", selectedId],
    queryFn: () => api.getCycleCountSession(selectedId!),
    enabled: !!selectedId,
  });

  const items = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });

  const warehouses = useQuery({
    queryKey: ["inventory", "warehouses"],
    queryFn: () => api.listInventoryWarehouses(),
  });

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-1">
        <Eyebrow>Inventory</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Cycle Counts
        </h1>
        <p className="max-w-2xl text-sm text-fg-muted">
          Spot-check what's actually on the shelf. Posting books a variance
          stock move for every line where the count differs from the expected
          on-hand snapshot.
        </p>
      </header>

      <div className="flex flex-col gap-6 lg:flex-row lg:items-start">
        <SessionListPanel
          sessions={list.data ?? []}
          warehouses={warehouses.data ?? []}
          selectedId={selectedId}
          onSelect={setSelectedId}
          loading={list.isLoading}
          error={list.error as Error | null}
          statusFilter={statusFilter}
          onStatusFilterChange={setStatusFilter}
        />
        <div className="min-w-0 flex-1">
          {!selectedId && (
            <NewSessionBuilder
              warehouses={warehouses.data ?? []}
              onCreated={(s) => setSelectedId(s.id)}
            />
          )}
          {/* When a session is selected the right panel can be in one
              of four states: still loading, the fetch errored, the
              session was deleted under us (`isError` false but `data`
              undefined after `enabled:true`), or data is ready. */}
          {selectedId && detail.isLoading && (
            <div className="flex flex-col gap-3">
              <Skeleton className="h-8 w-48" />
              <Skeleton className="h-12 w-full" />
              <Skeleton className="h-40 w-full" />
            </div>
          )}
          {selectedId && detail.error && (
            <EmptyState
              icon={<AlertTriangle />}
              title="Couldn't load this session"
              description={(detail.error as Error).message}
              action={
                <Button variant="secondary" onClick={() => setSelectedId(null)}>
                  Back to list
                </Button>
              }
            />
          )}
          {selectedId && detail.data && (
            <SessionDetailPanel
              session={detail.data.session}
              lines={detail.data.lines}
              items={items.data ?? []}
              warehouses={warehouses.data ?? []}
              onDeselect={() => setSelectedId(null)}
            />
          )}
        </div>
      </div>
    </section>
  );
}

function SessionListPanel(props: {
  sessions: CycleCountSession[];
  warehouses: InventoryWarehouse[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  loading: boolean;
  error: Error | null;
  statusFilter: string;
  onStatusFilterChange: (s: string) => void;
}) {
  const whLabel = (id: string): string => {
    const w = props.warehouses.find((x) => x.id === id);
    return w ? `${w.code} — ${w.name}` : "Unknown warehouse";
  };

  return (
    <div className="lg:w-80 lg:shrink-0 lg:border-r lg:border-border lg:pr-4">
      <Field label="Status" className="w-44">
        <Select
          size="sm"
          value={props.statusFilter}
          onChange={(e) => props.onStatusFilterChange(e.target.value)}
        >
          <option value="">All statuses</option>
          <option value="draft">Draft</option>
          <option value="counting">Counting</option>
          <option value="reconciled">In review</option>
          <option value="posted">Posted</option>
        </Select>
      </Field>
      {props.loading && (
        <div className="mt-3 flex flex-col gap-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-12 w-full" />
          ))}
        </div>
      )}
      {props.error && (
        <p
          role="alert"
          className="mt-3 rounded-md border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          Couldn't load sessions: {props.error.message}
        </p>
      )}
      <ul className="mt-3 flex list-none flex-col gap-1 p-0">
        {props.sessions.map((s) => {
          const selected = props.selectedId === s.id;
          const meta = STATUS_META[s.status];
          return (
            <li key={s.id}>
              <Button
                type="button"
                variant={selected ? "secondary" : "outline"}
                onClick={() => props.onSelect(s.id)}
                aria-current={selected ? "true" : undefined}
                className="h-auto w-full flex-col items-start justify-start gap-1 whitespace-normal py-2 text-left"
              >
                <span className="flex w-full items-center justify-between gap-2">
                  <span className="font-semibold text-fg">{s.code}</span>
                  <Badge variant={meta.variant} size="xs">
                    {meta.label}
                  </Badge>
                </span>
                <span className="text-xs font-normal text-fg-muted">
                  {whLabel(s.warehouse_id)}
                </span>
              </Button>
            </li>
          );
        })}
        {props.sessions.length === 0 && !props.loading && !props.error && (
          <li className="rounded-md border border-dashed border-border px-3 py-6 text-center text-sm text-fg-muted">
            No sessions yet. Create one to start counting.
          </li>
        )}
      </ul>
    </div>
  );
}

function NewSessionBuilder(props: {
  warehouses: InventoryWarehouse[];
  onCreated: (s: CycleCountSession) => void;
}) {
  const qc = useQueryClient();
  const [code, setCode] = useState("");
  const [description, setDescription] = useState("");
  const [warehouseId, setWarehouseId] = useState("");
  const [error, setError] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.createCycleCountSession({
        code: code.trim(),
        description: description.trim(),
        warehouse_id: warehouseId,
      }),
    onSuccess: (s) => {
      // Invalidate the list so the newly-created session shows up in
      // SessionListPanel without waiting for staleTime expiry.
      qc.invalidateQueries({ queryKey: ["cycle-counts", "list"] });
      props.onCreated(s);
      setCode("");
      setDescription("");
      setWarehouseId("");
      setError(null);
    },
    onError: (e: Error) => setError(e.message),
  });

  return (
    <div className="rounded-lg border border-border bg-bg-subtle p-4">
      <div className="flex items-center gap-2">
        <ClipboardList className="size-5 text-accent" aria-hidden="true" />
        <h2 className="m-0 text-base font-semibold text-fg">
          Start a cycle count
        </h2>
      </div>
      <p className="mt-1 text-sm text-fg-muted">
        Pick a warehouse and give the count a name. You'll seed expected
        quantities, count, review variances, then post.
      </p>
      <Stepper steps={STEPS} current={0} className="my-4 max-w-xl" />
      <div className="grid max-w-md gap-3">
        <Field label="Code" required help="A short reference, e.g. CC-2024-01.">
          <Input
            type="text"
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </Field>
        <Field label="Description">
          <Input
            type="text"
            placeholder="Optional — what this count covers"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        <Field label="Warehouse" required>
          <Select
            value={warehouseId}
            onChange={(e) => setWarehouseId(e.target.value)}
          >
            <option value="">Select a warehouse…</option>
            {props.warehouses.map((w) => (
              <option key={w.id} value={w.id}>
                {w.code} — {w.name}
              </option>
            ))}
          </Select>
        </Field>
        {error && <p className="text-sm text-danger">{error}</p>}
        <div>
          <Button
            type="button"
            disabled={!code || !warehouseId || create.isPending}
            onClick={() => create.mutate()}
          >
            {create.isPending ? "Creating…" : "Create draft session"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function SessionDetailPanel(props: {
  session: CycleCountSession;
  lines: CycleCountLine[];
  items: InventoryItem[];
  warehouses: InventoryWarehouse[];
  onDeselect: () => void;
}) {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const sessionId = props.session.id;
  // Shared error banner for every detail-panel mutation. Without an
  // onError handler each mutation silently swallowed failures, so a
  // 409 from the duplicate-item constraint or the reconciled-frozen
  // guard would clear the Add line form with no feedback.
  const [error, setError] = useState<string | null>(null);
  // A single confirm modal drives the reversible state-machine
  // transitions (back-to-draft, reopen, post). Each button stages its
  // copy + action here rather than calling window.confirm.
  const [confirmState, setConfirmState] = useState<{
    title: string;
    description: string;
    confirmLabel: string;
    destructive?: boolean;
    onConfirm: () => void;
  } | null>(null);

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["cycle-counts", "detail", sessionId] });
    qc.invalidateQueries({ queryKey: ["cycle-counts", "list"] });
  };
  const onError = (e: unknown) => {
    setError(e instanceof Error ? e.message : String(e));
  };

  const seed = useMutation({
    mutationFn: () => api.seedCycleCountSession(sessionId),
    onSuccess: invalidate,
    onError,
  });
  const advance = useMutation({
    mutationFn: (status: string) =>
      api.updateCycleCountSession(sessionId, {
        code: props.session.code,
        description: props.session.description ?? "",
        warehouse_id: props.session.warehouse_id,
        status,
      }),
    onSuccess: invalidate,
    onError,
    onSettled: () => setConfirmState(null),
  });
  const post = useMutation({
    mutationFn: () => api.postCycleCountSession(sessionId),
    onSuccess: invalidate,
    onError,
    onSettled: () => setConfirmState(null),
  });

  const upsert = useMutation({
    mutationFn: (input: UpsertLineInput) =>
      api.upsertCycleCountLine(sessionId, input),
    onSuccess: invalidate,
    onError,
  });

  const delLine = useMutation({
    mutationFn: (lineId: string) => api.deleteCycleCountLine(sessionId, lineId),
    onSuccess: invalidate,
    onError,
  });

  const itemName = (id: string): string => {
    const it = props.items.find((x) => x.id === id);
    return it ? `${it.sku} — ${it.name}` : "Unknown item";
  };
  const warehouseName = (() => {
    const w = props.warehouses.find((x) => x.id === props.session.warehouse_id);
    return w ? `${w.code} — ${w.name}` : "Unknown warehouse";
  })();

  // `reconciled` is line-frozen on the backend (UpsertLine /
  // DeleteLine / SeedExpectedFromStock all reject until the operator
  // transitions back to counting). Mirror that here so the "Seed from
  // stock" button and every inline line editor are disabled in the
  // reconciled view — otherwise the buttons appear active but every
  // mutation 422s.
  const isLocked =
    props.session.status === "posted" ||
    props.session.status === "reconciled";
  const status = props.session.status;
  const meta = STATUS_META[status];

  // Cross-mutation busy guard. Each action mutation already disables
  // its own button via `mutation.isPending`, but when `reconciled`
  // exposes both the Post and Reopen buttons simultaneously a
  // fast-clicking operator could fire both before the first request
  // returns. Disabling every action button while *any* mutation is in
  // flight makes the UI match the backend's single-in-flight semantics.
  const anyActionPending = seed.isPending || advance.isPending || post.isPending;

  // Per-target pending guard. The `advance` mutation drives multiple
  // sibling buttons (counting exposes both "Mark reconciled" and "Back
  // to draft"). `advance.variables` is the last argument passed to
  // `mutate`, so a strict equality check pins the pending label to the
  // clicked button only — the sibling keeps its resting label while
  // still being disabled by `anyActionPending`.
  const advancingTo = advance.isPending ? advance.variables : null;

  const countedLines = props.lines.filter(
    (l) => l.counted_qty !== "" && l.counted_qty != null,
  ).length;
  const varianceLines = props.lines.filter((l) => Number(l.variance) !== 0)
    .length;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <h2 className="m-0 truncate text-lg font-semibold text-fg">
            {props.session.code}
          </h2>
          <Badge variant={meta.variant}>{meta.label}</Badge>
        </div>
        <Button type="button" variant="outline" size="sm" onClick={props.onDeselect}>
          Back to list
        </Button>
      </div>
      <p className="text-sm text-fg-muted">
        {props.session.description ? `${props.session.description} · ` : ""}
        Warehouse: {warehouseName}
      </p>

      <Stepper steps={STEPS} current={meta.step} className="max-w-xl" />

      {error && (
        <div
          role="alert"
          className="rounded-md border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          {error}
        </div>
      )}

      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={isLocked || anyActionPending}
          onClick={() => seed.mutate()}
        >
          {seed.isPending ? "Seeding…" : "Seed from stock"}
        </Button>
        {status === "draft" && (
          <Button
            type="button"
            size="sm"
            disabled={anyActionPending}
            onClick={() => advance.mutate("counting")}
          >
            {advancingTo === "counting" ? "Starting…" : "Start counting"}
          </Button>
        )}
        {status === "counting" && (
          <>
            <Button
              type="button"
              size="sm"
              disabled={anyActionPending}
              onClick={() => advance.mutate("reconciled")}
            >
              {advancingTo === "reconciled" ? "Reconciling…" : "Mark reconciled"}
            </Button>
            {/* Back-to-draft path: the backend state machine allows
                counting → draft, and DeleteSession only accepts draft
                sessions. The confirm modal matches the Reopen / Post
                buttons — reversible, but worth a quick pause so a
                misclick doesn't undo work already entered. */}
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={anyActionPending}
              onClick={() =>
                setConfirmState({
                  title: "Back to draft?",
                  description:
                    "This undoes the counting transition and re-allows warehouse/code edits. Counted quantities are preserved.",
                  confirmLabel: "Back to draft",
                  onConfirm: () => advance.mutate("draft"),
                })
              }
            >
              {advancingTo === "draft" ? "Reverting…" : "Back to draft"}
            </Button>
          </>
        )}
        {status === "reconciled" && (
          <>
            <Button
              type="button"
              size="sm"
              disabled={anyActionPending}
              onClick={() =>
                setConfirmState({
                  title: "Post variance moves?",
                  description:
                    "Posting writes variance inventory moves and locks the session. This cannot be undone.",
                  confirmLabel: "Post",
                  destructive: true,
                  onConfirm: () => post.mutate(),
                })
              }
            >
              {post.isPending ? "Posting…" : "Post variance moves"}
            </Button>
            {/* Reopen path: the backend state machine allows
                reconciled → counting, so an operator who reconciled a
                session prematurely can unlock its lines without
                dropping to the API. */}
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={anyActionPending}
              onClick={() =>
                setConfirmState({
                  title: "Reopen to counting?",
                  description:
                    "Reopening unlocks lines for editing and requires re-marking reconciled before post.",
                  confirmLabel: "Reopen",
                  onConfirm: () => advance.mutate("counting"),
                })
              }
            >
              {advancingTo === "counting" ? "Reopening…" : "Reopen to counting"}
            </Button>
          </>
        )}
      </div>

      <LineEditor
        fmt={fmt}
        lines={props.lines}
        items={props.items}
        isLocked={isLocked}
        status={status}
        countedLines={countedLines}
        varianceLines={varianceLines}
        // Lift `upsert.mutate` so the Add-line form can clear its local
        // inputs only on success rather than optimistically clearing
        // them at click time. Failed mutations keep the operator's
        // input intact for retry, and the shared error banner explains
        // why.
        onUpsertAsync={(input) => upsert.mutateAsync(input)}
        onDelete={(id) => delLine.mutate(id)}
        itemName={itemName}
      />

      <ConfirmDialog
        open={confirmState !== null}
        onOpenChange={(o) => {
          if (!o && !(post.isPending || advance.isPending))
            setConfirmState(null);
        }}
        title={confirmState?.title ?? ""}
        description={confirmState?.description}
        confirmLabel={confirmState?.confirmLabel ?? "Confirm"}
        destructive={confirmState?.destructive}
        loading={post.isPending || advance.isPending}
        onConfirm={() => confirmState?.onConfirm()}
      />
    </div>
  );
}

type UpsertLineInput = {
  id?: string;
  item_id: string;
  expected_qty: string;
  counted_qty: string;
  notes?: string;
};

function LineEditor(props: {
  fmt: Formatters;
  lines: CycleCountLine[];
  items: InventoryItem[];
  isLocked: boolean;
  status: CycleStatus;
  countedLines: number;
  varianceLines: number;
  // Async-returning upsert so the Add-line form can await success
  // before clearing its inputs. The on-blur edit path (LineRow) keeps
  // the input as the source of truth so it doesn't need the promise.
  onUpsertAsync: (input: UpsertLineInput) => Promise<unknown>;
  onDelete: (id: string) => void;
  itemName: (id: string) => string;
}) {
  const { fmt } = props;
  const [newItem, setNewItem] = useState("");
  const [newExpected, setNewExpected] = useState("");
  const [newCounted, setNewCounted] = useState("");
  const [newNotes, setNewNotes] = useState("");
  const [adding, setAdding] = useState(false);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold text-fg">
          Count lines ({fmt.number(props.lines.length)})
        </h3>
        {props.lines.length > 0 && (
          <p className="text-xs text-fg-muted">
            {fmt.number(props.countedLines)} of {fmt.number(props.lines.length)}{" "}
            counted
            {props.varianceLines > 0 && (
              <>
                {" · "}
                <span className="text-warning">
                  {fmt.number(props.varianceLines)} with variance
                </span>
              </>
            )}
          </p>
        )}
      </div>

      {props.lines.length === 0 ? (
        <EmptyState
          icon={<ClipboardList />}
          title="No lines yet"
          description={
            props.isLocked
              ? "This session has no count lines."
              : "Seed expected quantities from stock, or add items below."
          }
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Item</TableHead>
              <TableHead className="text-right">Expected</TableHead>
              <TableHead className="text-right">Counted</TableHead>
              <TableHead className="text-right">Variance</TableHead>
              <TableHead>Notes</TableHead>
              {!props.isLocked && (
                <TableHead className="w-0 text-right">Actions</TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {props.lines.map((ln) => (
              <LineRow
                key={ln.id}
                fmt={fmt}
                line={ln}
                isLocked={props.isLocked}
                onUpsert={(input) => {
                  // LineRow's on-blur path fires-and-forgets; failures
                  // surface in the panel-level error banner.
                  void props.onUpsertAsync(input);
                }}
                onDelete={props.onDelete}
                itemName={props.itemName}
              />
            ))}
          </TableBody>
        </Table>
      )}

      {!props.isLocked && (
        <div className="rounded-lg border border-border bg-bg-subtle p-3">
          <h4 className="m-0 text-sm font-semibold text-fg">Add a line</h4>
          <div className="mt-2 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-[2fr_1fr_1fr_2fr_auto] lg:items-end">
            <Field label="Item" required>
              <Select
                size="sm"
                value={newItem}
                onChange={(e) => setNewItem(e.target.value)}
              >
                <option value="">Select an item…</option>
                {props.items.map((it) => (
                  <option key={it.id} value={it.id}>
                    {it.sku} — {it.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Expected" required>
              <Input
                size="sm"
                type="number"
                step="0.0001"
                inputMode="decimal"
                placeholder="0"
                value={newExpected}
                onChange={(e) => setNewExpected(e.target.value)}
                className="text-right"
              />
            </Field>
            <Field label="Counted" required>
              <Input
                size="sm"
                type="number"
                step="0.0001"
                inputMode="decimal"
                placeholder="0"
                value={newCounted}
                onChange={(e) => setNewCounted(e.target.value)}
                className="text-right"
              />
            </Field>
            <Field label="Notes">
              <Input
                size="sm"
                type="text"
                placeholder="Optional"
                value={newNotes}
                onChange={(e) => setNewNotes(e.target.value)}
              />
            </Field>
            <Button
              type="button"
              size="sm"
              leadingIcon={<Plus className="size-4" />}
              disabled={
                adding || !newItem || newExpected === "" || newCounted === ""
              }
              onClick={async () => {
                setAdding(true);
                try {
                  await props.onUpsertAsync({
                    item_id: newItem,
                    expected_qty: newExpected,
                    counted_qty: newCounted,
                    notes: newNotes,
                  });
                  // Clear inputs only on success so a failed add leaves
                  // the operator's data in place for retry.
                  setNewItem("");
                  setNewExpected("");
                  setNewCounted("");
                  setNewNotes("");
                } catch {
                  // Error surfaces via the panel-level banner from the
                  // mutation's onError; the inputs stay populated.
                } finally {
                  setAdding(false);
                }
              }}
            >
              {adding ? "Adding…" : "Add"}
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

function LineRow(props: {
  fmt: Formatters;
  line: CycleCountLine;
  isLocked: boolean;
  onUpsert: (input: UpsertLineInput) => void;
  onDelete: (id: string) => void;
  itemName: (id: string) => string;
}) {
  const { fmt } = props;
  const [counted, setCounted] = useState(props.line.counted_qty);
  const [notes, setNotes] = useState(props.line.notes ?? "");

  // Per-field "is this input the operator's source of truth right
  // now?" refs. An input is the source of truth from focus until blur.
  // While focused we MUST NOT overwrite it from the server-side props,
  // even if a sibling action (most notably "Seed from stock") bumps
  // `updated_at` on every line as a side effect of refreshing
  // `expected_qty`. Refs (not state) on purpose: the effect reads the
  // *current* focus state at the moment a server update arrives.
  const countedFocusedRef = useRef(false);
  const notesFocusedRef = useRef(false);

  // Re-sync local state when the server-side row changes, bounded to
  // blurred inputs only by the focus guards above, so the input the
  // operator is actively editing keeps its in-progress value while the
  // others sync to the server. `updated_at` is the primary trigger;
  // the explicit `counted_qty` + `notes` deps are defensive.
  useEffect(() => {
    if (!countedFocusedRef.current) {
      setCounted(props.line.counted_qty);
    }
    if (!notesFocusedRef.current) {
      setNotes(props.line.notes ?? "");
    }
  }, [props.line.updated_at, props.line.counted_qty, props.line.notes]);

  const varianceNum = Number(props.line.variance);
  const varianceClass =
    varianceNum === 0
      ? "text-fg-muted"
      : varianceNum > 0
        ? "text-success"
        : "text-danger";

  return (
    <TableRow>
      <TableCell className="text-fg">{props.itemName(props.line.item_id)}</TableCell>
      <TableCell className="text-right tabular-nums">
        {fmt.number(Number(props.line.expected_qty))}
      </TableCell>
      <TableCell className="text-right tabular-nums">
        {props.isLocked ? (
          fmt.number(Number(counted))
        ) : (
          <Input
            size="sm"
            type="number"
            step="0.0001"
            inputMode="decimal"
            value={counted}
            onChange={(e) => setCounted(e.target.value)}
            onFocus={() => {
              countedFocusedRef.current = true;
            }}
            onBlur={() => {
              // Clear the focus guard BEFORE firing onUpsert so the
              // upsert's invalidation, which triggers the re-sync
              // effect with the just-persisted server value, no longer
              // suppresses the field. Order matters here.
              countedFocusedRef.current = false;
              if (counted !== props.line.counted_qty) {
                props.onUpsert({
                  id: props.line.id,
                  item_id: props.line.item_id,
                  expected_qty: props.line.expected_qty,
                  counted_qty: counted,
                  notes,
                });
              }
            }}
            aria-label={`Counted quantity for ${props.itemName(props.line.item_id)}`}
            className="ms-auto w-24 text-right"
          />
        )}
      </TableCell>
      <TableCell className={`text-right font-medium tabular-nums ${varianceClass}`}>
        {varianceNum > 0 ? "+" : ""}
        {fmt.number(varianceNum)}
      </TableCell>
      <TableCell>
        {props.isLocked ? (
          <span className="text-fg-muted">{notes || "—"}</span>
        ) : (
          <Input
            size="sm"
            type="text"
            value={notes}
            onChange={(e) => setNotes(e.target.value)}
            onFocus={() => {
              notesFocusedRef.current = true;
            }}
            onBlur={() => {
              notesFocusedRef.current = false;
              if (notes !== (props.line.notes ?? "")) {
                props.onUpsert({
                  id: props.line.id,
                  item_id: props.line.item_id,
                  expected_qty: props.line.expected_qty,
                  counted_qty: counted,
                  notes,
                });
              }
            }}
            aria-label={`Notes for ${props.itemName(props.line.item_id)}`}
          />
        )}
      </TableCell>
      {!props.isLocked && (
        <TableCell className="text-right">
          <Button
            type="button"
            size="sm"
            variant="ghost"
            aria-label={`Remove ${props.itemName(props.line.item_id)}`}
            onClick={() => props.onDelete(props.line.id)}
          >
            <Trash2 className="size-4" />
          </Button>
        </TableCell>
      )}
    </TableRow>
  );
}
