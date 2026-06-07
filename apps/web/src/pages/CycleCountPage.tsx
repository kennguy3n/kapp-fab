import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Button,
  ConfirmDialog,
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
import type {
  CycleCountLine,
  CycleCountSession,
  InventoryItem,
  InventoryWarehouse,
} from "@kapp/client";

/**
 * CycleCountPage shows the cycle-count workflow:
 *
 *   draft  →  counting  →  reconciled  →  posted
 *
 * The operator opens a session (header with warehouse + code),
 * seeds expected_qty from stock_levels, walks the warehouse
 * keying counted_qty against each line, then posts the session
 * — at which point the backend writes a variance move for every
 * line where expected != counted and the moving-average cost on
 * each item is preserved (variance posts at the stored cost).
 */
export function CycleCountPage() {
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [statusFilter, setStatusFilter] = useState<string>("");

  const list = useQuery({
    queryKey: ["cycle-counts", "list", statusFilter],
    queryFn: () =>
      api.listCycleCountSessions(statusFilter ? { status: statusFilter } : undefined),
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
    <section>
      <h1>Cycle Counts</h1>
      <p className="mb-3 text-fg-muted">
        Spot-check on-hand stock by warehouse. Posting writes a
        variance inventory move for every line where the counted
        quantity diverges from the expected snapshot.
      </p>
      <div className="flex items-start gap-6">
        <SessionListPanel
          sessions={list.data ?? []}
          selectedId={selectedId}
          onSelect={setSelectedId}
          loading={list.isLoading}
          error={list.error as Error | null}
          statusFilter={statusFilter}
          onStatusFilterChange={setStatusFilter}
        />
        <div className="flex-1">
          {!selectedId && (
            <NewSessionBuilder
              warehouses={warehouses.data ?? []}
              onCreated={(s) => setSelectedId(s.id)}
            />
          )}
          {/* When a session is selected the right panel can be in
              one of four states: still loading, the fetch errored,
              the session was deleted under us (`isError` false but
              `data` undefined after `enabled:true`), or data is
              ready. Without the loading + error rendering below the
              panel briefly goes blank on every detail switch, and
              a network failure silently swallows the click. */}
          {selectedId && detail.isLoading && (
            <p className="text-fg-muted">Loading session…</p>
          )}
          {selectedId && detail.error && (
            <div
              role="alert"
              className="rounded border border-danger/30 bg-danger/10 px-3 py-2 text-[13px] text-danger"
            >
              Failed to load session: {(detail.error as Error).message}{" "}
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="ml-2"
                onClick={() => setSelectedId(null)}
              >
                Back to list
              </Button>
            </div>
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
  selectedId: string | null;
  onSelect: (id: string) => void;
  loading: boolean;
  error: Error | null;
  statusFilter: string;
  onStatusFilterChange: (s: string) => void;
}) {
  return (
    <div className="w-80 border-r border-border pr-4">
      <div className="flex items-center gap-2">
        <label className="text-[13px]">Status:</label>
        <Select
          size="sm"
          value={props.statusFilter}
          onChange={(e) => props.onStatusFilterChange(e.target.value)}
        >
          <option value="">all</option>
          <option value="draft">draft</option>
          <option value="counting">counting</option>
          <option value="reconciled">reconciled</option>
          <option value="posted">posted</option>
        </Select>
      </div>
      {props.loading && <p>Loading…</p>}
      {props.error && (
        <p className="text-danger">Failed: {props.error.message}</p>
      )}
      <ul className="mt-3 list-none p-0">
        {props.sessions.map((s) => {
          const selected = props.selectedId === s.id;
          return (
            <li key={s.id}>
              <Button
                type="button"
                variant={selected ? "secondary" : "outline"}
                onClick={() => props.onSelect(s.id)}
                className="my-1 h-auto w-full flex-col items-start justify-start whitespace-normal py-2 text-left"
              >
                <div className="font-semibold">{s.code}</div>
                <div className="text-xs text-fg-muted">
                  {s.status} · {s.warehouse_id.slice(0, 8)}…
                </div>
              </Button>
            </li>
          );
        })}
        {props.sessions.length === 0 && !props.loading && (
          <li className="text-[13px] text-fg-muted">No sessions.</li>
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
      // Invalidate the list so the newly-created session shows up
      // in SessionListPanel without waiting for staleTime expiry.
      // SessionDetailPanel does the same after every line / status
      // mutation — mirroring that contract here keeps both surfaces
      // consistent.
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
    <div>
      <h2 className="mt-0">New cycle-count session</h2>
      <div className="grid max-w-[400px] gap-2">
        <label className="grid gap-1 text-sm">
          Code
          <Input
            type="text"
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </label>
        <label className="grid gap-1 text-sm">
          Description
          <Input
            type="text"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        <label className="grid gap-1 text-sm">
          Warehouse
          <Select
            value={warehouseId}
            onChange={(e) => setWarehouseId(e.target.value)}
          >
            <option value="">— pick —</option>
            {props.warehouses.map((w) => (
              <option key={w.id} value={w.id}>
                {w.code} — {w.name}
              </option>
            ))}
          </Select>
        </label>
        {error && <p className="text-danger">{error}</p>}
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
  const sessionId = props.session.id;
  // Shared error banner for every detail-panel mutation. Without an
  // onError handler each mutation silently swallowed failures, so a
  // 409 from the duplicate-item constraint or the reconciled-frozen
  // guard would clear the Add line form (see below) with no feedback.
  // NewSessionBuilder uses the same `setError(e.message)` pattern.
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
    mutationFn: (input: {
      id?: string;
      item_id: string;
      expected_qty: string;
      counted_qty: string;
      notes?: string;
    }) => api.upsertCycleCountLine(sessionId, input),
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
    return it ? `${it.sku} — ${it.name}` : id.slice(0, 8) + "…";
  };

  // `reconciled` is line-frozen on the backend (UpsertLine /
  // DeleteLine / SeedExpectedFromStock all reject with
  // ErrCycleCountLineFrozen until the operator transitions back to
  // counting). Mirror that here so the "Seed from stock" button and
  // every inline line editor are disabled in the reconciled view —
  // otherwise the buttons appear active but every mutation 422s.
  const isLocked =
    props.session.status === "posted" ||
    props.session.status === "reconciled";
  const status = props.session.status;

  // Cross-mutation busy guard. Each action mutation already disables
  // its own button via `mutation.isPending`, but when `reconciled`
  // exposes both the Post and Reopen buttons simultaneously a
  // fast-clicking operator could fire both before the first request
  // returns. The backend serialises them via the session FOR UPDATE
  // lock so the loser just receives ErrCycleCountAlreadyPosted /
  // ErrCycleCountNotReconciled, but the operator then sees a
  // confusing error banner for an action they thought they cancelled.
  // Disabling every action button while *any* mutation is in flight
  // makes the UI match the backend's single-in-flight semantics.
  const anyActionPending =
    seed.isPending ||
    advance.isPending ||
    post.isPending;

  // Per-target pending guard. The `advance` mutation drives multiple
  // sibling buttons (e.g. counting state exposes both "Mark
  // reconciled" and "Back to draft", both calling advance.mutate with
  // a different `status`). Without distinguishing which transition is
  // in-flight, clicking one would also flip the sibling's label to
  // its pending text ("Reverting…" / "Reconciling…") because both
  // observe `advance.isPending`. `advance.variables` is the last
  // argument passed to `mutate`, so a strict equality check pins the
  // pending label to the clicked button only — the sibling keeps its
  // resting label while still being disabled by `anyActionPending`.
  const advancingTo = advance.isPending ? advance.variables : null;

  return (
    <div>
      <div className="flex items-center justify-between">
        <h2 className="m-0">{props.session.code}</h2>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={props.onDeselect}
        >
          Back to list
        </Button>
      </div>
      <p className="text-fg-muted">
        Status: <strong>{status}</strong> · Warehouse: {props.session.warehouse_id}
      </p>
      {error && (
        <div
          role="alert"
          className="mb-2 rounded border border-danger/30 bg-danger/10 px-3 py-2 text-[13px] text-danger"
        >
          {error}
        </div>
      )}
      <div className="my-3 flex gap-2">
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
            variant="outline"
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
              variant="outline"
              size="sm"
              disabled={anyActionPending}
              onClick={() => advance.mutate("reconciled")}
            >
              {advancingTo === "reconciled" ? "Reconciling…" : "Mark reconciled"}
            </Button>
            {/* Back-to-draft path: the backend state machine allows
                counting → draft (canTransitionCycleCount in
                internal/inventory/cycle_count.go), and DeleteSession
                only accepts draft sessions. Without this button an
                operator who created a session by mistake (wrong
                warehouse, typo in code, etc.) has to drop to the
                API to back out before they can delete it. The
                confirm modal matches the Reopen / Post buttons —
                this transition is reversible (operator can always
                advance back to counting) but worth a quick pause so
                a misclick doesn't undo work already entered. */}
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
              variant="outline"
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
                reconciled → counting (canTransitionCycleCount in
                internal/inventory/cycle_count.go), so an operator
                who reconciled a session prematurely needs a UI
                affordance to unlock its lines without dropping to
                the API directly. Confirmation matches the post
                button — a reopen is rare and worth pausing on. */}
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
        sessionId={sessionId}
        lines={props.lines}
        items={props.items}
        isLocked={isLocked}
        // Lift `upsert.mutate` so the Add-line form can clear its
        // local inputs only on success (see LineEditor below) rather
        // than optimistically clearing them at click time. Failed
        // mutations therefore keep the operator's input intact for
        // retry, and the shared error banner above explains why.
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
  sessionId: string;
  lines: CycleCountLine[];
  items: InventoryItem[];
  isLocked: boolean;
  // Async-returning upsert so the Add-line form can await success
  // before clearing its inputs. The existing on-blur edit path
  // (LineRow) keeps the input as the source of truth so it doesn't
  // need the promise.
  onUpsertAsync: (input: UpsertLineInput) => Promise<unknown>;
  onDelete: (id: string) => void;
  itemName: (id: string) => string;
}) {
  const [newItem, setNewItem] = useState("");
  const [newExpected, setNewExpected] = useState("");
  const [newCounted, setNewCounted] = useState("");
  const [newNotes, setNewNotes] = useState("");
  const [adding, setAdding] = useState(false);

  return (
    <div>
      <h3>Lines ({props.lines.length})</h3>
      <Table className="text-[13px]">
        <TableHeader>
          <TableRow>
            <TableHead>Item</TableHead>
            <TableHead className="text-right">Expected</TableHead>
            <TableHead className="text-right">Counted</TableHead>
            <TableHead className="text-right">Variance</TableHead>
            <TableHead>Notes</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {props.lines.map((ln) => (
            <LineRow
              key={ln.id}
              line={ln}
              items={props.items}
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

      {!props.isLocked && (
        <div className="mt-4 rounded border border-border p-3">
          <h4 className="mt-0">Add line</h4>
          <div className="grid grid-cols-[2fr_1fr_1fr_2fr_auto] items-center gap-2">
            <Select value={newItem} onChange={(e) => setNewItem(e.target.value)}>
              <option value="">— item —</option>
              {props.items.map((it) => (
                <option key={it.id} value={it.id}>
                  {it.sku} — {it.name}
                </option>
              ))}
            </Select>
            <Input
              type="number"
              step="0.0001"
              placeholder="expected"
              value={newExpected}
              onChange={(e) => setNewExpected(e.target.value)}
            />
            <Input
              type="number"
              step="0.0001"
              placeholder="counted"
              value={newCounted}
              onChange={(e) => setNewCounted(e.target.value)}
            />
            <Input
              type="text"
              placeholder="notes"
              value={newNotes}
              onChange={(e) => setNewNotes(e.target.value)}
            />
            <Button
              type="button"
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
                  // Clear inputs only on success so a failed add
                  // (network error / 409 from the duplicate-item
                  // index / reconciled-frozen guard) leaves the
                  // operator's data in place for retry.
                  setNewItem("");
                  setNewExpected("");
                  setNewCounted("");
                  setNewNotes("");
                } catch {
                  // Error surfaces via the panel-level banner from
                  // the mutation's onError; the inputs stay populated.
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
  line: CycleCountLine;
  items: InventoryItem[];
  isLocked: boolean;
  onUpsert: (input: UpsertLineInput) => void;
  onDelete: (id: string) => void;
  itemName: (id: string) => string;
}) {
  const [counted, setCounted] = useState(props.line.counted_qty);
  const [notes, setNotes] = useState(props.line.notes ?? "");

  // Per-field "is this input the operator's source of truth right
  // now?" refs. An input is the source of truth from the moment it
  // gains focus until blur. While focused we MUST NOT overwrite it
  // from the server-side props, even if a sibling action (most
  // notably "Seed from stock") bumps `updated_at` on every line as
  // a side effect of refreshing `expected_qty`. Without these refs,
  // an operator typing in counted_qty who simultaneously clicks Seed
  // would lose their keystrokes — the seed's query-invalidation
  // re-renders every LineRow, the useEffect below fires because
  // updated_at changed, and `setCounted(props.line.counted_qty)`
  // overwrites whatever the operator had typed back to the server's
  // stored value (typically 0).
  //
  // Refs (not state) on purpose: the effect reads the *current*
  // focus state at the moment a server update arrives; we don't
  // want to re-trigger the effect just because focus changed.
  const countedFocusedRef = useRef(false);
  const notesFocusedRef = useRef(false);

  // Re-sync local state when the server-side row changes. Without
  // this, useState only captures the initial values and a parent
  // re-render after a query invalidation (e.g. another tab posts a
  // line, or `Seed from stock` refreshes expected_qty via the new
  // (tenant_id, session_id, item_id) upsert path) would leave the
  // input out of sync with the persisted row.
  //
  // The per-field focus guard above bounds the overwrite window to
  // "blurred inputs only": the input the operator is actively
  // editing keeps its in-progress value; the others sync to the
  // server. This is the right shape because Seed bumps updated_at
  // on every line as a side effect of writing expected_qty, even
  // though counted_qty / notes are untouched — so a focused
  // counted_qty input must survive a Seed click on the same row.
  //
  // `updated_at` is the primary trigger (every server-side mutation
  // bumps it) and the explicit `counted_qty` + `notes` deps are
  // defensive — if a future schema change ever allowed a
  // server-side mutation without bumping `updated_at`, the row
  // would still re-sync the blurred inputs.
  useEffect(() => {
    if (!countedFocusedRef.current) {
      setCounted(props.line.counted_qty);
    }
    if (!notesFocusedRef.current) {
      setNotes(props.line.notes ?? "");
    }
  }, [props.line.updated_at, props.line.counted_qty, props.line.notes]);

  const variance = props.line.variance;
  const varianceClass =
    Number(variance) === 0
      ? "text-fg-muted"
      : Number(variance) > 0
        ? "text-success"
        : "text-danger";

  return (
    <TableRow>
      <TableCell>{props.itemName(props.line.item_id)}</TableCell>
      <TableCell className="text-right">{props.line.expected_qty}</TableCell>
      <TableCell className="text-right">
        {props.isLocked ? (
          counted
        ) : (
          <Input
            type="number"
            step="0.0001"
            value={counted}
            onChange={(e) => setCounted(e.target.value)}
            onFocus={() => {
              countedFocusedRef.current = true;
            }}
            onBlur={() => {
              // Clear the focus guard BEFORE firing onUpsert so the
              // upsert's invalidation, which triggers the re-sync
              // useEffect with the just-persisted server value, no
              // longer suppresses the field. Order matters here.
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
            className="w-[90px] text-right"
          />
        )}
      </TableCell>
      <TableCell className={`text-right font-medium ${varianceClass}`}>
        {variance}
      </TableCell>
      <TableCell>
        {props.isLocked ? (
          notes
        ) : (
          <Input
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
          />
        )}
      </TableCell>
      <TableCell>
        {!props.isLocked && (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => props.onDelete(props.line.id)}
          >
            ×
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}
