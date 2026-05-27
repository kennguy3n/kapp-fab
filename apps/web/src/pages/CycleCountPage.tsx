import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
      <p style={{ color: "#6b7280", marginBottom: 12 }}>
        Spot-check on-hand stock by warehouse. Posting writes a
        variance inventory move for every line where the counted
        quantity diverges from the expected snapshot.
      </p>
      <div style={{ display: "flex", gap: 24, alignItems: "flex-start" }}>
        <SessionListPanel
          sessions={list.data ?? []}
          selectedId={selectedId}
          onSelect={setSelectedId}
          loading={list.isLoading}
          error={list.error as Error | null}
          statusFilter={statusFilter}
          onStatusFilterChange={setStatusFilter}
        />
        <div style={{ flex: 1 }}>
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
            <p style={{ color: "#6b7280" }}>Loading session…</p>
          )}
          {selectedId && detail.error && (
            <div
              role="alert"
              style={{
                background: "#fee2e2",
                color: "#991b1b",
                border: "1px solid #fecaca",
                padding: "8px 12px",
                borderRadius: 4,
                fontSize: 13,
              }}
            >
              Failed to load session: {(detail.error as Error).message}{" "}
              <button
                type="button"
                onClick={() => setSelectedId(null)}
                style={{ marginLeft: 8 }}
              >
                Back to list
              </button>
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
    <div style={{ width: 320, borderRight: "1px solid #e5e7eb", paddingRight: 16 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <label style={{ fontSize: 13 }}>Status:</label>
        <select
          value={props.statusFilter}
          onChange={(e) => props.onStatusFilterChange(e.target.value)}
        >
          <option value="">all</option>
          <option value="draft">draft</option>
          <option value="counting">counting</option>
          <option value="reconciled">reconciled</option>
          <option value="posted">posted</option>
        </select>
      </div>
      {props.loading && <p>Loading…</p>}
      {props.error && (
        <p style={{ color: "#b91c1c" }}>Failed: {props.error.message}</p>
      )}
      <ul style={{ listStyle: "none", padding: 0, marginTop: 12 }}>
        {props.sessions.map((s) => {
          const selected = props.selectedId === s.id;
          return (
            <li key={s.id}>
              <button
                type="button"
                onClick={() => props.onSelect(s.id)}
                style={{
                  display: "block",
                  width: "100%",
                  padding: "8px 10px",
                  margin: "4px 0",
                  textAlign: "left",
                  background: selected ? "#dbeafe" : "transparent",
                  border: "1px solid #e5e7eb",
                  borderRadius: 4,
                  cursor: "pointer",
                }}
              >
                <div style={{ fontWeight: 600 }}>{s.code}</div>
                <div style={{ fontSize: 12, color: "#6b7280" }}>
                  {s.status} · {s.warehouse_id.slice(0, 8)}…
                </div>
              </button>
            </li>
          );
        })}
        {props.sessions.length === 0 && !props.loading && (
          <li style={{ fontSize: 13, color: "#6b7280" }}>No sessions.</li>
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
      <h2 style={{ marginTop: 0 }}>New cycle-count session</h2>
      <div style={{ display: "grid", gap: 8, maxWidth: 400 }}>
        <label>
          Code
          <input
            type="text"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            style={{ width: "100%" }}
          />
        </label>
        <label>
          Description
          <input
            type="text"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            style={{ width: "100%" }}
          />
        </label>
        <label>
          Warehouse
          <select
            value={warehouseId}
            onChange={(e) => setWarehouseId(e.target.value)}
            style={{ width: "100%" }}
          >
            <option value="">— pick —</option>
            {props.warehouses.map((w) => (
              <option key={w.id} value={w.id}>
                {w.code} — {w.name}
              </option>
            ))}
          </select>
        </label>
        {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
        <button
          type="button"
          disabled={!code || !warehouseId || create.isPending}
          onClick={() => create.mutate()}
        >
          {create.isPending ? "Creating…" : "Create draft session"}
        </button>
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
  });
  const post = useMutation({
    mutationFn: () => api.postCycleCountSession(sessionId),
    onSuccess: invalidate,
    onError,
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
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h2 style={{ margin: 0 }}>{props.session.code}</h2>
        <button type="button" onClick={props.onDeselect}>
          Back to list
        </button>
      </div>
      <p style={{ color: "#6b7280" }}>
        Status: <strong>{status}</strong> · Warehouse: {props.session.warehouse_id}
      </p>
      {error && (
        <div
          role="alert"
          style={{
            background: "#fee2e2",
            color: "#991b1b",
            border: "1px solid #fecaca",
            padding: "8px 12px",
            borderRadius: 4,
            marginBottom: 8,
            fontSize: 13,
          }}
        >
          {error}
        </div>
      )}
      <div style={{ display: "flex", gap: 8, margin: "12px 0" }}>
        <button
          type="button"
          disabled={isLocked || anyActionPending}
          onClick={() => seed.mutate()}
        >
          {seed.isPending ? "Seeding…" : "Seed from stock"}
        </button>
        {status === "draft" && (
          <button
            type="button"
            disabled={anyActionPending}
            onClick={() => advance.mutate("counting")}
          >
            {advancingTo === "counting" ? "Starting…" : "Start counting"}
          </button>
        )}
        {status === "counting" && (
          <>
            <button
              type="button"
              disabled={anyActionPending}
              onClick={() => advance.mutate("reconciled")}
            >
              {advancingTo === "reconciled" ? "Reconciling…" : "Mark reconciled"}
            </button>
            {/* Back-to-draft path: the backend state machine allows
                counting → draft (canTransitionCycleCount in
                internal/inventory/cycle_count.go), and DeleteSession
                only accepts draft sessions. Without this button an
                operator who created a session by mistake (wrong
                warehouse, typo in code, etc.) has to drop to the
                API to back out before they can delete it. The
                window.confirm matches the Reopen / Post buttons —
                this transition is reversible (operator can always
                advance back to counting) but worth a quick pause so
                a misclick doesn't undo work already entered. */}
            <button
              type="button"
              disabled={anyActionPending}
              onClick={() => {
                if (
                  window.confirm(
                    "Back to draft will undo the counting transition and re-allow warehouse/code edits. Counted quantities are preserved. Continue?"
                  )
                ) {
                  advance.mutate("draft");
                }
              }}
            >
              {advancingTo === "draft" ? "Reverting…" : "Back to draft"}
            </button>
          </>
        )}
        {status === "reconciled" && (
          <>
            <button
              type="button"
              disabled={anyActionPending}
              onClick={() => {
                if (
                  window.confirm(
                    "Posting will write variance inventory moves and lock the session. Continue?"
                  )
                ) {
                  post.mutate();
                }
              }}
            >
              {post.isPending ? "Posting…" : "Post variance moves"}
            </button>
            {/* Reopen path: the backend state machine allows
                reconciled → counting (canTransitionCycleCount in
                internal/inventory/cycle_count.go), so an operator
                who reconciled a session prematurely needs a UI
                affordance to unlock its lines without dropping to
                the API directly. Confirmation matches the post
                button — a reopen is rare and worth pausing on. */}
            <button
              type="button"
              disabled={anyActionPending}
              onClick={() => {
                if (
                  window.confirm(
                    "Reopening will unlock lines for editing and require re-marking reconciled before post. Continue?"
                  )
                ) {
                  advance.mutate("counting");
                }
              }}
            >
              {advancingTo === "counting" ? "Reopening…" : "Reopen to counting"}
            </button>
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
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={{ padding: "6px 8px" }}>Item</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Expected</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Counted</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Variance</th>
            <th style={{ padding: "6px 8px" }}>Notes</th>
            <th />
          </tr>
        </thead>
        <tbody>
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
        </tbody>
      </table>

      {!props.isLocked && (
        <div style={{ marginTop: 16, padding: 12, border: "1px solid #e5e7eb", borderRadius: 4 }}>
          <h4 style={{ marginTop: 0 }}>Add line</h4>
          <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr 1fr 2fr auto", gap: 8 }}>
            <select value={newItem} onChange={(e) => setNewItem(e.target.value)}>
              <option value="">— item —</option>
              {props.items.map((it) => (
                <option key={it.id} value={it.id}>
                  {it.sku} — {it.name}
                </option>
              ))}
            </select>
            <input
              type="number"
              step="0.0001"
              placeholder="expected"
              value={newExpected}
              onChange={(e) => setNewExpected(e.target.value)}
            />
            <input
              type="number"
              step="0.0001"
              placeholder="counted"
              value={newCounted}
              onChange={(e) => setNewCounted(e.target.value)}
            />
            <input
              type="text"
              placeholder="notes"
              value={newNotes}
              onChange={(e) => setNewNotes(e.target.value)}
            />
            <button
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
            </button>
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
  const varianceColour =
    Number(variance) === 0
      ? "#6b7280"
      : Number(variance) > 0
      ? "#16a34a"
      : "#b91c1c";

  return (
    <tr style={{ borderBottom: "1px solid #f3f4f6" }}>
      <td style={{ padding: "6px 8px" }}>{props.itemName(props.line.item_id)}</td>
      <td style={{ padding: "6px 8px", textAlign: "right" }}>
        {props.line.expected_qty}
      </td>
      <td style={{ padding: "6px 8px", textAlign: "right" }}>
        {props.isLocked ? (
          counted
        ) : (
          <input
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
            style={{ width: 90, textAlign: "right" }}
          />
        )}
      </td>
      <td
        style={{
          padding: "6px 8px",
          textAlign: "right",
          color: varianceColour,
          fontWeight: 500,
        }}
      >
        {variance}
      </td>
      <td style={{ padding: "6px 8px" }}>
        {props.isLocked ? (
          notes
        ) : (
          <input
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
            style={{ width: "100%" }}
          />
        )}
      </td>
      <td style={{ padding: "6px 8px" }}>
        {!props.isLocked && (
          <button type="button" onClick={() => props.onDelete(props.line.id)}>
            ×
          </button>
        )}
      </td>
    </tr>
  );
}
