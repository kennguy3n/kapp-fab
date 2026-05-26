import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  LandedCostCharge,
  LandedCostTarget,
  LandedCostVoucher,
  UpsertLandedCostChargeInput,
  UpsertLandedCostTargetInput,
  UpsertLandedCostVoucherInput,
} from "@kapp/client";
import { api } from "../lib/api";

/**
 * LandedCostPage is the operator UI for the Phase N9c landed-cost
 * voucher lifecycle (draft → allocated → posted).
 *
 * Left column: list of vouchers filtered by status.
 * Right column: selected voucher detail with editable charge + target
 * tables, an Allocate button (preview shares without committing
 * inventory moves), and a Post button (writes per-target reversal +
 * forward inventory_moves plus the booking JE; idempotent).
 *
 * The page intentionally keeps the editing surface minimal — there is
 * no row-level validation beyond the backend's CHECK constraints,
 * because every mutation goes through the typed API client which
 * surfaces the backend's typed errors.
 */
export function LandedCostPage() {
  const queryClient = useQueryClient();
  const [statusFilter, setStatusFilter] = useState<string>("");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const listQ = useQuery({
    queryKey: ["landed-costs", statusFilter],
    queryFn: () =>
      api.listLandedCostVouchers(
        statusFilter ? { status: statusFilter } : undefined,
      ),
  });

  const detailQ = useQuery({
    queryKey: ["landed-cost", selectedId],
    queryFn: () => api.getLandedCostVoucher(selectedId!),
    enabled: !!selectedId,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["landed-costs"] });
    if (selectedId) {
      queryClient.invalidateQueries({ queryKey: ["landed-cost", selectedId] });
    }
  };

  const allocateMut = useMutation({
    mutationFn: (id: string) => api.allocateLandedCostVoucher(id),
    onSuccess: invalidate,
  });

  const postMut = useMutation({
    mutationFn: (id: string) => api.postLandedCostVoucher(id),
    onSuccess: invalidate,
  });

  // Reset both allocate + post mutation state whenever the operator
  // selects a different voucher.  Without this, a 409 / 422 from a
  // previous voucher's allocate or post bleeds into the detail panel
  // of the next voucher (the mutations are declared at page scope so
  // they survive across selections).  The detail panel reads
  // `*Mut.error` to render the inline error message, so leaving stale
  // error state attached would falsely flag the new voucher.
  useEffect(() => {
    allocateMut.reset();
    postMut.reset();
    // Mutation handles are stable across renders; resetting only when
    // the selected voucher changes is the intended behaviour.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedId]);

  return (
    <section>
      <h1>Landed Cost Vouchers</h1>
      <p style={{ color: "#6b7280" }}>
        Allocate freight, duty, insurance and other landed costs across
        receipt lines. Draft vouchers may be edited; allocating freezes the
        share preview and a posted voucher writes inventory_moves +
        journal entries (idempotent).
      </p>

      <CreateVoucherForm
        onCreated={(v) => {
          setSelectedId(v.id);
          invalidate();
        }}
      />

      <div style={{ display: "flex", gap: 24, marginTop: 24 }}>
        <div style={{ width: 360 }}>
          <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 8 }}>
            <label style={{ fontSize: 12, color: "#374151" }}>Status</label>
            <select
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
              style={{ fontSize: 12 }}
            >
              <option value="">All</option>
              <option value="draft">Draft</option>
              <option value="allocated">Allocated</option>
              <option value="posted">Posted</option>
            </select>
          </div>
          {listQ.isLoading && <p>Loading…</p>}
          {listQ.isError && (
            <p style={{ color: "#b91c1c" }}>
              Failed to load vouchers: {(listQ.error as Error).message}
            </p>
          )}
          {listQ.data && (
            <VoucherList
              vouchers={listQ.data}
              selectedId={selectedId}
              onSelect={setSelectedId}
            />
          )}
        </div>

        <div style={{ flex: 1, minWidth: 0 }}>
          {!selectedId && (
            <p style={{ color: "#9ca3af" }}>Select a voucher from the list.</p>
          )}
          {selectedId && detailQ.isLoading && <p>Loading detail…</p>}
          {selectedId && detailQ.isError && (
            <p style={{ color: "#b91c1c" }}>
              Failed to load detail: {(detailQ.error as Error).message}
            </p>
          )}
          {selectedId && detailQ.data && (
            // key={selectedId} forces a fresh VoucherDetail mount on
            // each selection so the local useState in ChargesSection /
            // TargetsSection (draft description, draft amount, etc.) is
            // reset rather than persisted across vouchers. Without
            // remount the previous voucher's half-typed inputs would
            // bleed into the next voucher's form, which is a UX
            // surprise even though no data integrity is at risk
            // (mutations key on props.voucher.id).
            <VoucherDetail
              key={selectedId}
              voucher={detailQ.data.voucher}
              charges={detailQ.data.charges}
              targets={detailQ.data.targets}
              onAllocate={() => allocateMut.mutate(selectedId)}
              onPost={() => postMut.mutate(selectedId)}
              isAllocating={allocateMut.isPending}
              isPosting={postMut.isPending}
              allocateError={allocateMut.error}
              postError={postMut.error}
              onChargeMutated={invalidate}
              onTargetMutated={invalidate}
            />
          )}
        </div>
      </div>
    </section>
  );
}

function CreateVoucherForm(props: {
  onCreated: (v: LandedCostVoucher) => void;
}) {
  const [voucherNumber, setVoucherNumber] = useState("");
  const [description, setDescription] = useState("");
  const [allocationMethod, setAllocationMethod] = useState<
    "by_qty" | "by_amount" | "by_weight"
  >("by_qty");

  const createMut = useMutation({
    mutationFn: (input: UpsertLandedCostVoucherInput) =>
      api.createLandedCostVoucher(input),
    onSuccess: (v) => {
      // Clear inputs only after the mutation lands so a server
      // error doesn't silently wipe what the user typed. Same
      // pattern as ChargesSection / TargetsSection.
      setVoucherNumber("");
      setDescription("");
      setAllocationMethod("by_qty");
      props.onCreated(v);
    },
  });

  return (
    <div
      style={{
        marginTop: 12,
        padding: 12,
        border: "1px solid #e5e7eb",
        borderRadius: 6,
      }}
    >
      <strong style={{ fontSize: 13 }}>Create voucher</strong>
      <div style={{ display: "flex", gap: 8, marginTop: 8, flexWrap: "wrap" }}>
        <input
          placeholder="Voucher number"
          value={voucherNumber}
          onChange={(e) => setVoucherNumber(e.target.value)}
          style={{ fontSize: 12 }}
        />
        <input
          placeholder="Description (optional)"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          style={{ fontSize: 12, flex: 1 }}
        />
        <select
          value={allocationMethod}
          onChange={(e) =>
            setAllocationMethod(
              e.target.value as "by_qty" | "by_amount" | "by_weight",
            )
          }
          style={{ fontSize: 12 }}
        >
          <option value="by_qty">by_qty</option>
          <option value="by_amount">by_amount</option>
          <option value="by_weight">by_weight</option>
        </select>
        <button
          disabled={createMut.isPending || voucherNumber.trim() === ""}
          onClick={() =>
            createMut.mutate({
              voucher_number: voucherNumber.trim(),
              description: description.trim() || undefined,
              allocation_method: allocationMethod,
            })
          }
          style={{ fontSize: 12 }}
        >
          Create
        </button>
      </div>
    </div>
  );
}

function VoucherList(props: {
  vouchers: LandedCostVoucher[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 12 }}>
      <thead>
        <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
          <th style={{ padding: "6px 8px" }}>Voucher</th>
          <th style={{ padding: "6px 8px" }}>Method</th>
          <th style={{ padding: "6px 8px" }}>Status</th>
        </tr>
      </thead>
      <tbody>
        {props.vouchers.map((v) => (
          <tr
            key={v.id}
            onClick={() => props.onSelect(v.id)}
            style={{
              cursor: "pointer",
              borderBottom: "1px solid #f3f4f6",
              background: v.id === props.selectedId ? "#eff6ff" : undefined,
            }}
          >
            <td style={{ padding: "6px 8px" }}>{v.voucher_number}</td>
            <td style={{ padding: "6px 8px" }}>{v.allocation_method}</td>
            <td style={{ padding: "6px 8px" }}>
              <StatusBadge status={v.status} />
            </td>
          </tr>
        ))}
        {props.vouchers.length === 0 && (
          <tr>
            <td colSpan={3} style={{ padding: "6px 8px", color: "#9ca3af" }}>
              No vouchers.
            </td>
          </tr>
        )}
      </tbody>
    </table>
  );
}

function StatusBadge({ status }: { status: string }) {
  const palette: Record<string, { bg: string; fg: string }> = {
    draft: { bg: "#f3f4f6", fg: "#374151" },
    allocated: { bg: "#dbeafe", fg: "#1e40af" },
    posted: { bg: "#dcfce7", fg: "#166534" },
  };
  const c = palette[status] ?? { bg: "#fee2e2", fg: "#b91c1c" };
  return (
    <span
      style={{
        background: c.bg,
        color: c.fg,
        padding: "2px 8px",
        borderRadius: 12,
        fontSize: 11,
      }}
    >
      {status}
    </span>
  );
}

function VoucherDetail(props: {
  voucher: LandedCostVoucher;
  charges: LandedCostCharge[];
  targets: LandedCostTarget[];
  onAllocate: () => void;
  onPost: () => void;
  isAllocating: boolean;
  isPosting: boolean;
  allocateError: unknown;
  postError: unknown;
  onChargeMutated: () => void;
  onTargetMutated: () => void;
}) {
  const isDraft = props.voucher.status === "draft";
  const isAllocated = props.voucher.status === "allocated";
  const isPosted = props.voucher.status === "posted";

  const totalCharges = useMemo(() => {
    return props.charges.reduce((acc, c) => acc + Number(c.amount), 0);
  }, [props.charges]);
  const totalAllocated = useMemo(() => {
    return props.targets.reduce(
      (acc, t) => acc + Number(t.allocated_amount),
      0,
    );
  }, [props.targets]);

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <h2 style={{ margin: 0 }}>{props.voucher.voucher_number}</h2>
        <StatusBadge status={props.voucher.status} />
        <span style={{ color: "#6b7280", fontSize: 12 }}>
          {props.voucher.allocation_method}
        </span>
        <div style={{ marginLeft: "auto", display: "flex", gap: 8 }}>
          <button
            disabled={isPosted || props.isAllocating}
            onClick={props.onAllocate}
            style={{ fontSize: 12 }}
          >
            {props.isAllocating ? "Allocating…" : "Allocate"}
          </button>
          <button
            disabled={!isAllocated || props.isPosting}
            onClick={props.onPost}
            style={{
              fontSize: 12,
              background: isAllocated ? "#16a34a" : undefined,
              color: isAllocated ? "white" : undefined,
            }}
          >
            {props.isPosting ? "Posting…" : "Post"}
          </button>
        </div>
      </div>
      {props.voucher.description && (
        <p style={{ color: "#6b7280", marginTop: 4 }}>
          {props.voucher.description}
        </p>
      )}
      {props.allocateError ? (
        <p style={{ color: "#b91c1c", fontSize: 12 }}>
          Allocate failed: {(props.allocateError as Error).message}
        </p>
      ) : null}
      {props.postError ? (
        <p style={{ color: "#b91c1c", fontSize: 12 }}>
          Post failed: {(props.postError as Error).message}
        </p>
      ) : null}

      <ChargesSection
        voucher={props.voucher}
        charges={props.charges}
        editable={isDraft}
        onMutated={props.onChargeMutated}
        totalCharges={totalCharges}
      />

      <TargetsSection
        voucher={props.voucher}
        targets={props.targets}
        editable={isDraft}
        onMutated={props.onTargetMutated}
        totalAllocated={totalAllocated}
        totalCharges={totalCharges}
      />
    </div>
  );
}

function ChargesSection(props: {
  voucher: LandedCostVoucher;
  charges: LandedCostCharge[];
  editable: boolean;
  onMutated: () => void;
  totalCharges: number;
}) {
  const [description, setDescription] = useState("");
  const [amount, setAmount] = useState("");
  const [accountCode, setAccountCode] = useState("");

  const upsertMut = useMutation({
    mutationFn: (input: UpsertLandedCostChargeInput) =>
      api.upsertLandedCostCharge(props.voucher.id, input),
    onSuccess: () => {
      // Clear inputs only after the mutation lands so a server
      // error doesn't silently wipe what the user typed.
      setDescription("");
      setAmount("");
      setAccountCode("");
      props.onMutated();
    },
  });
  const deleteMut = useMutation({
    mutationFn: (chargeId: string) =>
      api.deleteLandedCostCharge(props.voucher.id, chargeId),
    onSuccess: props.onMutated,
  });

  return (
    <div style={{ marginTop: 16 }}>
      <h3 style={{ fontSize: 14, margin: "8px 0" }}>Charges</h3>
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 12 }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={{ padding: "6px 8px" }}>Description</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Amount</th>
            <th style={{ padding: "6px 8px" }}>Account</th>
            <th style={{ padding: "6px 8px" }} />
          </tr>
        </thead>
        <tbody>
          {props.charges.map((c) => (
            <tr key={c.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={{ padding: "6px 8px" }}>{c.description}</td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>
                {c.amount}
              </td>
              <td style={{ padding: "6px 8px" }}>
                {c.account_code ?? <em>(default)</em>}
              </td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>
                {props.editable && (
                  <button
                    onClick={() => deleteMut.mutate(c.id)}
                    style={{ fontSize: 11 }}
                  >
                    Delete
                  </button>
                )}
              </td>
            </tr>
          ))}
          <tr>
            <td style={{ padding: "6px 8px", textAlign: "right" }}>
              <strong>Total</strong>
            </td>
            <td style={{ padding: "6px 8px", textAlign: "right" }}>
              <strong>{props.totalCharges.toFixed(2)}</strong>
            </td>
            <td />
            <td />
          </tr>
        </tbody>
      </table>
      {props.editable && (
        <div style={{ display: "flex", gap: 8, marginTop: 8, flexWrap: "wrap" }}>
          <input
            placeholder="Description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            style={{ fontSize: 12, flex: 1 }}
          />
          <input
            placeholder="Amount"
            type="number"
            value={amount}
            onChange={(e) => setAmount(e.target.value)}
            style={{ fontSize: 12, width: 100 }}
          />
          <input
            placeholder="Account code (optional)"
            value={accountCode}
            onChange={(e) => setAccountCode(e.target.value)}
            style={{ fontSize: 12, width: 140 }}
          />
          <button
            disabled={
              upsertMut.isPending ||
              description.trim() === "" ||
              amount.trim() === ""
            }
            onClick={() => {
              upsertMut.mutate({
                description: description.trim(),
                amount: amount.trim(),
                account_code: accountCode.trim() || undefined,
              });
            }}
            style={{ fontSize: 12 }}
          >
            Add charge
          </button>
        </div>
      )}
    </div>
  );
}

function TargetsSection(props: {
  voucher: LandedCostVoucher;
  targets: LandedCostTarget[];
  editable: boolean;
  onMutated: () => void;
  totalAllocated: number;
  totalCharges: number;
}) {
  const [sourceKType, setSourceKType] = useState("");
  const [sourceID, setSourceID] = useState("");
  const [itemID, setItemID] = useState("");
  const [warehouseID, setWarehouseID] = useState("");
  const [qty, setQty] = useState("");
  const [unitCost, setUnitCost] = useState("");
  const [weight, setWeight] = useState("");

  const upsertMut = useMutation({
    mutationFn: (input: UpsertLandedCostTargetInput) =>
      api.upsertLandedCostTarget(props.voucher.id, input),
    onSuccess: () => {
      // Clear inputs only after the mutation lands — see
      // ChargesSection for rationale.
      setSourceKType("");
      setSourceID("");
      setItemID("");
      setWarehouseID("");
      setQty("");
      setUnitCost("");
      setWeight("");
      props.onMutated();
    },
  });
  const deleteMut = useMutation({
    mutationFn: (targetId: string) =>
      api.deleteLandedCostTarget(props.voucher.id, targetId),
    onSuccess: props.onMutated,
  });

  const reconcile = props.totalCharges - props.totalAllocated;

  return (
    <div style={{ marginTop: 16 }}>
      <h3 style={{ fontSize: 14, margin: "8px 0" }}>Targets</h3>
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 12 }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={{ padding: "6px 8px" }}>Source</th>
            <th style={{ padding: "6px 8px" }}>Item</th>
            <th style={{ padding: "6px 8px" }}>Warehouse</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Qty</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Unit cost</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Weight</th>
            <th style={{ padding: "6px 8px", textAlign: "right" }}>Allocated</th>
            <th style={{ padding: "6px 8px" }}>Applied</th>
            <th style={{ padding: "6px 8px" }} />
          </tr>
        </thead>
        <tbody>
          {props.targets.map((t) => (
            <tr key={t.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={{ padding: "6px 8px", color: "#6b7280" }}>
                {t.source_ktype}
              </td>
              <td style={{ padding: "6px 8px", fontFamily: "monospace" }}>
                {t.item_id.slice(0, 8)}…
              </td>
              <td style={{ padding: "6px 8px", fontFamily: "monospace" }}>
                {t.warehouse_id.slice(0, 8)}…
              </td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>{t.qty}</td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>
                {t.unit_cost}
              </td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>
                {t.weight}
              </td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>
                {t.allocated_amount}
              </td>
              <td style={{ padding: "6px 8px" }}>{t.applied ? "✓" : "—"}</td>
              <td style={{ padding: "6px 8px", textAlign: "right" }}>
                {props.editable && (
                  <button
                    onClick={() => deleteMut.mutate(t.id)}
                    style={{ fontSize: 11 }}
                  >
                    Delete
                  </button>
                )}
              </td>
            </tr>
          ))}
          <tr>
            <td colSpan={6} style={{ padding: "6px 8px", textAlign: "right" }}>
              <strong>Total allocated</strong>
            </td>
            <td style={{ padding: "6px 8px", textAlign: "right" }}>
              <strong>{props.totalAllocated.toFixed(2)}</strong>
            </td>
            <td colSpan={2} style={{ padding: "6px 8px" }}>
              {reconcile !== 0 && props.totalAllocated > 0 && (
                <span style={{ color: "#b91c1c" }}>
                  Δ {reconcile.toFixed(2)}
                </span>
              )}
            </td>
          </tr>
        </tbody>
      </table>
      {props.editable && (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(4, 1fr) auto",
            gap: 6,
            marginTop: 8,
          }}
        >
          <input
            placeholder="Source ktype"
            value={sourceKType}
            onChange={(e) => setSourceKType(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <input
            placeholder="Source id (UUID)"
            value={sourceID}
            onChange={(e) => setSourceID(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <input
            placeholder="Item id (UUID)"
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <input
            placeholder="Warehouse id (UUID)"
            value={warehouseID}
            onChange={(e) => setWarehouseID(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <span />
          <input
            placeholder="Qty"
            type="number"
            value={qty}
            onChange={(e) => setQty(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <input
            placeholder="Unit cost"
            type="number"
            value={unitCost}
            onChange={(e) => setUnitCost(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <input
            placeholder="Weight (optional)"
            type="number"
            value={weight}
            onChange={(e) => setWeight(e.target.value)}
            style={{ fontSize: 12 }}
          />
          <span />
          <button
            disabled={
              upsertMut.isPending ||
              sourceID.trim() === "" ||
              itemID.trim() === "" ||
              warehouseID.trim() === "" ||
              qty.trim() === "" ||
              unitCost.trim() === ""
            }
            onClick={() => {
              upsertMut.mutate({
                source_ktype: sourceKType.trim() || undefined,
                source_id: sourceID.trim(),
                item_id: itemID.trim(),
                warehouse_id: warehouseID.trim(),
                qty: qty.trim(),
                unit_cost: unitCost.trim(),
                weight: weight.trim() || undefined,
              });
            }}
            style={{ fontSize: 12 }}
          >
            Add target
          </button>
        </div>
      )}
    </div>
  );
}

export default LandedCostPage;
