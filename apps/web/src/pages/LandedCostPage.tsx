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
  type BadgeProps,
} from "@kapp/ui";
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
      <p className="text-fg-muted">
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

      <div className="mt-6 flex gap-6">
        <div className="w-[360px]">
          <div className="mb-2 flex items-center gap-2">
            <label className="text-xs text-fg">Status</label>
            <Select
              size="sm"
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
            >
              <option value="">All</option>
              <option value="draft">Draft</option>
              <option value="allocated">Allocated</option>
              <option value="posted">Posted</option>
            </Select>
          </div>
          {listQ.isLoading && <p>Loading…</p>}
          {listQ.isError && (
            <p className="text-danger">
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

        <div className="min-w-0 flex-1">
          {!selectedId && (
            <p className="text-fg-subtle">Select a voucher from the list.</p>
          )}
          {selectedId && detailQ.isLoading && <p>Loading detail…</p>}
          {selectedId && detailQ.isError && (
            <p className="text-danger">
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
    <div className="mt-3 rounded-md border border-border p-3">
      <strong className="text-sm">Create voucher</strong>
      <div className="mt-2 flex flex-wrap gap-2">
        <Input
          size="sm"
          placeholder="Voucher number"
          value={voucherNumber}
          onChange={(e) => setVoucherNumber(e.target.value)}
        />
        <Input
          size="sm"
          className="flex-1"
          placeholder="Description (optional)"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
        <Select
          size="sm"
          value={allocationMethod}
          onChange={(e) =>
            setAllocationMethod(
              e.target.value as "by_qty" | "by_amount" | "by_weight",
            )
          }
        >
          <option value="by_qty">by_qty</option>
          <option value="by_amount">by_amount</option>
          <option value="by_weight">by_weight</option>
        </Select>
        <Button
          size="sm"
          disabled={createMut.isPending || voucherNumber.trim() === ""}
          onClick={() =>
            createMut.mutate({
              voucher_number: voucherNumber.trim(),
              description: description.trim() || undefined,
              allocation_method: allocationMethod,
            })
          }
        >
          Create
        </Button>
      </div>
      {createMut.isError ? (
        <p className="mt-1.5 text-xs text-danger">
          Create failed: {(createMut.error as Error).message}
        </p>
      ) : null}
    </div>
  );
}

function VoucherList(props: {
  vouchers: LandedCostVoucher[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <Table className="text-xs">
      <TableHeader>
        <TableRow>
          <TableHead>Voucher</TableHead>
          <TableHead>Method</TableHead>
          <TableHead>Status</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {props.vouchers.map((v) => (
          <TableRow
            key={v.id}
            onClick={() => props.onSelect(v.id)}
            className={
              v.id === props.selectedId
                ? "cursor-pointer bg-bg-muted"
                : "cursor-pointer"
            }
          >
            <TableCell>{v.voucher_number}</TableCell>
            <TableCell>{v.allocation_method}</TableCell>
            <TableCell>
              <StatusBadge status={v.status} />
            </TableCell>
          </TableRow>
        ))}
        {props.vouchers.length === 0 && (
          <TableRow>
            <TableCell colSpan={3} className="text-fg-subtle">
              No vouchers.
            </TableCell>
          </TableRow>
        )}
      </TableBody>
    </Table>
  );
}

function StatusBadge({ status }: { status: string }) {
  // Map the voucher lifecycle to the design-system's semantic Badge
  // variants so the colour meaning survives light/dark themes.
  const variant: BadgeProps["variant"] =
    status === "draft"
      ? "default"
      : status === "allocated"
        ? "info"
        : status === "posted"
          ? "success"
          : "danger";
  return <Badge variant={variant}>{status}</Badge>;
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
      <div className="flex items-center gap-3">
        <h2 className="m-0">{props.voucher.voucher_number}</h2>
        <StatusBadge status={props.voucher.status} />
        <span className="text-xs text-fg-muted">
          {props.voucher.allocation_method}
        </span>
        <div className="ms-auto flex gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={isPosted || props.isAllocating}
            onClick={props.onAllocate}
          >
            {props.isAllocating ? "Allocating…" : "Allocate"}
          </Button>
          <Button
            size="sm"
            disabled={!isAllocated || props.isPosting}
            onClick={props.onPost}
          >
            {props.isPosting ? "Posting…" : "Post"}
          </Button>
        </div>
      </div>
      {props.voucher.description && (
        <p className="mt-1 text-fg-muted">{props.voucher.description}</p>
      )}
      {props.allocateError ? (
        <p className="text-xs text-danger">
          Allocate failed: {(props.allocateError as Error).message}
        </p>
      ) : null}
      {props.postError ? (
        <p className="text-xs text-danger">
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
    <div className="mt-4">
      <h3 className="my-2 text-sm">Charges</h3>
      <Table className="text-xs">
        <TableHeader>
          <TableRow>
            <TableHead>Description</TableHead>
            <TableHead className="text-right">Amount</TableHead>
            <TableHead>Account</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {props.charges.map((c) => (
            <TableRow key={c.id}>
              <TableCell>{c.description}</TableCell>
              <TableCell className="text-right">{c.amount}</TableCell>
              <TableCell>
                {c.account_code ?? <em>(default)</em>}
              </TableCell>
              <TableCell className="text-right">
                {props.editable && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => deleteMut.mutate(c.id)}
                  >
                    Delete
                  </Button>
                )}
              </TableCell>
            </TableRow>
          ))}
          <TableRow>
            <TableCell className="text-right">
              <strong>Total</strong>
            </TableCell>
            <TableCell className="text-right">
              <strong>{props.totalCharges.toFixed(2)}</strong>
            </TableCell>
            <TableCell />
            <TableCell />
          </TableRow>
        </TableBody>
      </Table>
      {props.editable && (
        <div className="mt-2 flex flex-wrap gap-2">
          <Input
            size="sm"
            className="flex-1"
            placeholder="Description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
          <Input
            size="sm"
            className="w-[100px]"
            placeholder="Amount"
            type="number"
            value={amount}
            onChange={(e) => setAmount(e.target.value)}
          />
          <Input
            size="sm"
            className="w-[140px]"
            placeholder="Account code (optional)"
            value={accountCode}
            onChange={(e) => setAccountCode(e.target.value)}
          />
          <Button
            size="sm"
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
          >
            Add charge
          </Button>
        </div>
      )}
      {upsertMut.isError ? (
        <p className="mt-1.5 text-xs text-danger">
          Add charge failed: {(upsertMut.error as Error).message}
        </p>
      ) : null}
      {deleteMut.isError ? (
        <p className="mt-1.5 text-xs text-danger">
          Delete charge failed: {(deleteMut.error as Error).message}
        </p>
      ) : null}
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

  // JS float accumulation across independent reduce() passes can leave
  // a sub-cent residual even when shopspring/decimal on the backend has
  // them exactly equal (classic 0.1 + 0.2 === 0.30000000000000004 case).
  // The display rounds to two decimals, so anything under half a cent is
  // visual zero; gate the red Δ indicator on the same threshold so we
  // don't paint a "Δ 0.00" mismatch for a balanced voucher.
  const reconcile = props.totalCharges - props.totalAllocated;
  const reconcileMismatch = Math.abs(reconcile) >= 0.005;

  return (
    <div className="mt-4">
      <h3 className="my-2 text-sm">Targets</h3>
      <Table className="text-xs">
        <TableHeader>
          <TableRow>
            <TableHead>Source</TableHead>
            <TableHead>Item</TableHead>
            <TableHead>Warehouse</TableHead>
            <TableHead className="text-right">Qty</TableHead>
            <TableHead className="text-right">Unit cost</TableHead>
            <TableHead className="text-right">Weight</TableHead>
            <TableHead className="text-right">Allocated</TableHead>
            <TableHead>Applied</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {props.targets.map((t) => (
            <TableRow key={t.id}>
              <TableCell className="text-fg-muted">
                {t.source_ktype}
              </TableCell>
              <TableCell className="font-mono">
                {t.item_id.slice(0, 8)}…
              </TableCell>
              <TableCell className="font-mono">
                {t.warehouse_id.slice(0, 8)}…
              </TableCell>
              <TableCell className="text-right">{t.qty}</TableCell>
              <TableCell className="text-right">{t.unit_cost}</TableCell>
              <TableCell className="text-right">{t.weight}</TableCell>
              <TableCell className="text-right">
                {t.allocated_amount}
              </TableCell>
              <TableCell>{t.applied ? "✓" : "—"}</TableCell>
              <TableCell className="text-right">
                {props.editable && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => deleteMut.mutate(t.id)}
                  >
                    Delete
                  </Button>
                )}
              </TableCell>
            </TableRow>
          ))}
          <TableRow>
            <TableCell colSpan={6} className="text-right">
              <strong>Total allocated</strong>
            </TableCell>
            <TableCell className="text-right">
              <strong>{props.totalAllocated.toFixed(2)}</strong>
            </TableCell>
            <TableCell colSpan={2}>
              {reconcileMismatch && props.totalAllocated > 0 && (
                <span className="text-danger">
                  Δ {reconcile.toFixed(2)}
                </span>
              )}
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
      {props.editable && (
        <div className="mt-2 grid grid-cols-[repeat(4,1fr)_auto] gap-1.5">
          <Input
            size="sm"
            placeholder="Source ktype"
            value={sourceKType}
            onChange={(e) => setSourceKType(e.target.value)}
          />
          <Input
            size="sm"
            placeholder="Source id (UUID)"
            value={sourceID}
            onChange={(e) => setSourceID(e.target.value)}
          />
          <Input
            size="sm"
            placeholder="Item id (UUID)"
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
          />
          <Input
            size="sm"
            placeholder="Warehouse id (UUID)"
            value={warehouseID}
            onChange={(e) => setWarehouseID(e.target.value)}
          />
          <span />
          <Input
            size="sm"
            placeholder="Qty"
            type="number"
            value={qty}
            onChange={(e) => setQty(e.target.value)}
          />
          <Input
            size="sm"
            placeholder="Unit cost"
            type="number"
            value={unitCost}
            onChange={(e) => setUnitCost(e.target.value)}
          />
          <Input
            size="sm"
            placeholder="Weight (optional)"
            type="number"
            value={weight}
            onChange={(e) => setWeight(e.target.value)}
          />
          <span />
          <Button
            size="sm"
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
          >
            Add target
          </Button>
        </div>
      )}
      {upsertMut.isError ? (
        <p className="mt-1.5 text-xs text-danger">
          Add target failed: {(upsertMut.error as Error).message}
        </p>
      ) : null}
      {deleteMut.isError ? (
        <p className="mt-1.5 text-xs text-danger">
          Delete target failed: {(deleteMut.error as Error).message}
        </p>
      ) : null}
    </div>
  );
}

export default LandedCostPage;
