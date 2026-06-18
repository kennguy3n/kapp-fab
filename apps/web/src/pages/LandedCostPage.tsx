import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  InventoryItem,
  InventoryWarehouse,
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
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  toast,
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, Download, FileStack, Plus } from "lucide-react";
import { api } from "../lib/api";
import { downloadCsv } from "../lib/csv";
import { useFormatter } from "../lib/i18n";

type AllocationMethod = "by_qty" | "by_amount" | "by_weight";

const MONEY_OPTS: Intl.NumberFormatOptions = {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
};

const ALLOCATION_METHOD_LABELS: Record<AllocationMethod, string> = {
  by_qty: "By quantity",
  by_amount: "By value",
  by_weight: "By weight",
};

const STATUS_LABELS: Record<LandedCostVoucher["status"], string> = {
  draft: "Draft",
  allocated: "Allocated",
  posted: "Posted",
};

const STATUS_VARIANT: Record<LandedCostVoucher["status"], BadgeProps["variant"]> =
  {
    draft: "default",
    allocated: "info",
    posted: "success",
  };

/** Turn a ktype id like `purchasing.goods_receipt` into "Goods Receipt". */
function humanizeKType(ktype: string): string {
  const last = ktype.split(/[./]/).pop() ?? ktype;
  return last
    .split(/[_\s]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(" ");
}

function StatusBadge({ status }: { status: LandedCostVoucher["status"] }) {
  return <Badge variant={STATUS_VARIANT[status]}>{STATUS_LABELS[status]}</Badge>;
}

/**
 * LandedCostPage is the operator UI for the landed-cost voucher
 * lifecycle (draft → allocated → posted).
 *
 * Left column: list of vouchers filtered by status.
 * Right column: selected voucher detail with editable charge + target
 * tables, an Allocate button (preview shares without committing
 * inventory moves), and a Post button (writes per-target reversal +
 * forward inventory moves plus the booking journal entry; idempotent).
 */
export function LandedCostPage() {
  const queryClient = useQueryClient();
  const fmt = useFormatter();
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

  // Item + warehouse catalogues resolve the target rows' foreign keys
  // to human-readable labels and back the picker dropdowns in the
  // target editor, so the UI never surfaces a raw UUID.
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });
  const warehousesQ = useQuery({
    queryKey: ["inventory", "warehouses"],
    queryFn: () => api.listInventoryWarehouses(),
  });
  const items = itemsQ.data ?? [];
  const warehouses = warehousesQ.data ?? [];
  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    items.forEach((it) => m.set(it.id, `${it.sku} — ${it.name}`));
    return m;
  }, [items]);
  const whLabel = useMemo(() => {
    const m = new Map<string, string>();
    warehouses.forEach((w) => m.set(w.id, `${w.code} — ${w.name}`));
    return m;
  }, [warehouses]);

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

  function handleExportList() {
    const vouchers = listQ.data ?? [];
    const header = ["Voucher", "Method", "Status", "Description"];
    const data = vouchers.map((v) => [
      v.voucher_number,
      ALLOCATION_METHOD_LABELS[v.allocation_method],
      STATUS_LABELS[v.status],
      v.description ?? "",
    ]);
    downloadCsv("landed-cost-vouchers.csv", header, data);
    toast.success("Export complete", {
      description: `landed-cost-vouchers.csv · ${data.length} row(s)`,
    });
  }

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Inventory</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Landed Costs
            </h1>
            <p className="mt-1 max-w-2xl text-sm text-fg-muted">
              Spread freight, duty, insurance and other import costs across the
              goods you received. Draft vouchers can be edited; allocating
              previews each item's share, and posting writes the stock and
              accounting entries.
            </p>
          </div>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<Download className="size-4" />}
            onClick={handleExportList}
            disabled={!listQ.data || listQ.data.length === 0}
          >
            Export CSV
          </Button>
        </div>
      </header>

      <CreateVoucherForm
        onCreated={(v) => {
          setSelectedId(v.id);
          invalidate();
        }}
      />

      <div className="flex flex-col gap-6 lg:flex-row">
        <div className="lg:w-[340px] lg:shrink-0">
          <Field label="Status" className="mb-2 w-44">
            <Select
              size="sm"
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
            >
              <option value="">All statuses</option>
              <option value="draft">Draft</option>
              <option value="allocated">Allocated</option>
              <option value="posted">Posted</option>
            </Select>
          </Field>
          {listQ.isLoading ? (
            <div className="flex flex-col gap-2">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : listQ.isError ? (
            <EmptyState
              icon={<AlertTriangle />}
              title="Couldn't load vouchers"
              description={(listQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void listQ.refetch()}
                  disabled={listQ.isFetching}
                >
                  Retry
                </Button>
              }
            />
          ) : (listQ.data ?? []).length === 0 ? (
            <EmptyState
              icon={<FileStack />}
              title={
                statusFilter ? "No vouchers with this status" : "No vouchers yet"
              }
              description={
                statusFilter
                  ? "Try a different status filter."
                  : "Create a voucher above to start allocating landed costs."
              }
            />
          ) : (
            <VoucherList
              vouchers={listQ.data ?? []}
              selectedId={selectedId}
              onSelect={setSelectedId}
            />
          )}
        </div>

        <div className="min-w-0 flex-1">
          {!selectedId ? (
            <EmptyState
              icon={<FileStack />}
              title="Select a voucher"
              description="Pick a voucher from the list to view its charges and allocation."
            />
          ) : detailQ.isLoading ? (
            <div className="flex flex-col gap-3">
              <Skeleton className="h-8 w-64" />
              <Skeleton className="h-40 w-full" />
              <Skeleton className="h-40 w-full" />
            </div>
          ) : detailQ.isError ? (
            <EmptyState
              icon={<AlertTriangle />}
              title="Couldn't load this voucher"
              description={(detailQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void detailQ.refetch()}
                  disabled={detailQ.isFetching}
                >
                  Retry
                </Button>
              }
            />
          ) : detailQ.data ? (
            // key={selectedId} forces a fresh VoucherDetail mount on
            // each selection so the local useState in ChargesSection /
            // TargetsSection (draft description, draft amount, etc.) is
            // reset rather than persisted across vouchers.
            <VoucherDetail
              key={selectedId}
              fmt={fmt}
              voucher={detailQ.data.voucher}
              charges={detailQ.data.charges}
              targets={detailQ.data.targets}
              items={items}
              warehouses={warehouses}
              itemLabel={itemLabel}
              whLabel={whLabel}
              onAllocate={() => allocateMut.mutate(selectedId)}
              onPost={() => postMut.mutate(selectedId)}
              isAllocating={allocateMut.isPending}
              isPosting={postMut.isPending}
              allocateError={allocateMut.error}
              postError={postMut.error}
              onChargeMutated={invalidate}
              onTargetMutated={invalidate}
            />
          ) : null}
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
  const [allocationMethod, setAllocationMethod] =
    useState<AllocationMethod>("by_qty");

  const createMut = useMutation({
    mutationFn: (input: UpsertLandedCostVoucherInput) =>
      api.createLandedCostVoucher(input),
    onSuccess: (v) => {
      // Clear inputs only after the mutation lands so a server
      // error doesn't silently wipe what the user typed.
      setVoucherNumber("");
      setDescription("");
      setAllocationMethod("by_qty");
      toast.success("Voucher created", { description: v.voucher_number });
      props.onCreated(v);
    },
  });

  return (
    <div className="rounded-lg border border-border bg-bg-subtle p-4">
      <h2 className="text-sm font-semibold text-fg">Create a voucher</h2>
      <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-[1fr_2fr_1fr_auto] lg:items-end">
        <Field label="Voucher number" required>
          <Input
            size="sm"
            placeholder="e.g. LC-1042"
            value={voucherNumber}
            onChange={(e) => setVoucherNumber(e.target.value)}
          />
        </Field>
        <Field label="Description" help="Optional — what these costs relate to.">
          <Input
            size="sm"
            placeholder="March ocean freight"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        <Field
          label="Allocation"
          help="How costs are split across items."
        >
          <Select
            size="sm"
            value={allocationMethod}
            onChange={(e) =>
              setAllocationMethod(e.target.value as AllocationMethod)
            }
          >
            <option value="by_qty">By quantity</option>
            <option value="by_amount">By value</option>
            <option value="by_weight">By weight</option>
          </Select>
        </Field>
        <Button
          size="sm"
          leadingIcon={<Plus className="size-4" />}
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
        <p className="mt-2 text-xs text-danger">
          Couldn't create voucher: {(createMut.error as Error).message}
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
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Voucher</TableHead>
          <TableHead>Method</TableHead>
          <TableHead>Status</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {props.vouchers.map((v) => {
          const selected = v.id === props.selectedId;
          return (
            <TableRow
              key={v.id}
              onClick={() => props.onSelect(v.id)}
              aria-selected={selected}
              className={selected ? "cursor-pointer bg-bg-muted" : "cursor-pointer"}
            >
              <TableCell className="font-medium text-fg">
                {v.voucher_number}
              </TableCell>
              <TableCell className="text-fg-muted">
                {ALLOCATION_METHOD_LABELS[v.allocation_method]}
              </TableCell>
              <TableCell>
                <StatusBadge status={v.status} />
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

function VoucherDetail(props: {
  fmt: ReturnType<typeof useFormatter>;
  voucher: LandedCostVoucher;
  charges: LandedCostCharge[];
  targets: LandedCostTarget[];
  items: InventoryItem[];
  warehouses: InventoryWarehouse[];
  itemLabel: Map<string, string>;
  whLabel: Map<string, string>;
  onAllocate: () => void;
  onPost: () => void;
  isAllocating: boolean;
  isPosting: boolean;
  allocateError: unknown;
  postError: unknown;
  onChargeMutated: () => void;
  onTargetMutated: () => void;
}) {
  const { fmt } = props;
  const isDraft = props.voucher.status === "draft";
  const isAllocated = props.voucher.status === "allocated";
  const isPosted = props.voucher.status === "posted";

  const totalCharges = useMemo(
    () => props.charges.reduce((acc, c) => acc + Number(c.amount), 0),
    [props.charges],
  );
  const totalAllocated = useMemo(
    () => props.targets.reduce((acc, t) => acc + Number(t.allocated_amount), 0),
    [props.targets],
  );

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="m-0 text-lg font-semibold text-fg">
          {props.voucher.voucher_number}
        </h2>
        <StatusBadge status={props.voucher.status} />
        <Badge variant="outline">
          {ALLOCATION_METHOD_LABELS[props.voucher.allocation_method]}
        </Badge>
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
        <p className="text-sm text-fg-muted">{props.voucher.description}</p>
      )}
      {props.allocateError ? (
        <p className="text-xs text-danger">
          Couldn't allocate: {(props.allocateError as Error).message}
        </p>
      ) : null}
      {props.postError ? (
        <p className="text-xs text-danger">
          Couldn't post: {(props.postError as Error).message}
        </p>
      ) : null}

      <ChargesSection
        fmt={fmt}
        voucher={props.voucher}
        charges={props.charges}
        editable={isDraft}
        onMutated={props.onChargeMutated}
        totalCharges={totalCharges}
      />

      <TargetsSection
        fmt={fmt}
        voucher={props.voucher}
        targets={props.targets}
        items={props.items}
        warehouses={props.warehouses}
        itemLabel={props.itemLabel}
        whLabel={props.whLabel}
        editable={isDraft}
        onMutated={props.onTargetMutated}
        totalAllocated={totalAllocated}
        totalCharges={totalCharges}
      />
    </div>
  );
}

function ChargesSection(props: {
  fmt: ReturnType<typeof useFormatter>;
  voucher: LandedCostVoucher;
  charges: LandedCostCharge[];
  editable: boolean;
  onMutated: () => void;
  totalCharges: number;
}) {
  const { fmt } = props;
  const [description, setDescription] = useState("");
  const [amount, setAmount] = useState("");
  const [accountCode, setAccountCode] = useState("");

  const upsertMut = useMutation({
    mutationFn: (input: UpsertLandedCostChargeInput) =>
      api.upsertLandedCostCharge(props.voucher.id, input),
    onSuccess: () => {
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
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold text-fg">Charges</h3>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Description</TableHead>
            <TableHead className="text-right">Amount</TableHead>
            <TableHead>Account</TableHead>
            {props.editable && <TableHead className="w-0 text-right">Actions</TableHead>}
          </TableRow>
        </TableHeader>
        <TableBody>
          {props.charges.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={props.editable ? 4 : 3}
                className="text-center text-fg-subtle"
              >
                No charges yet.
              </TableCell>
            </TableRow>
          ) : (
            props.charges.map((c) => (
              <TableRow key={c.id}>
                <TableCell className="text-fg">{c.description}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {fmt.number(Number(c.amount), MONEY_OPTS)}
                </TableCell>
                <TableCell className="text-fg-muted">
                  {c.account_code ?? (
                    <span className="text-fg-subtle">Default</span>
                  )}
                </TableCell>
                {props.editable && (
                  <TableCell className="text-right">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => deleteMut.mutate(c.id)}
                      disabled={deleteMut.isPending}
                    >
                      Delete
                    </Button>
                  </TableCell>
                )}
              </TableRow>
            ))
          )}
        </TableBody>
        <TableFooter>
          <TableRow>
            <TableCell className="font-medium text-fg">Total charges</TableCell>
            <TableCell className="text-right font-semibold tabular-nums text-fg">
              {fmt.number(props.totalCharges, MONEY_OPTS)}
            </TableCell>
            <TableCell />
            {props.editable && <TableCell />}
          </TableRow>
        </TableFooter>
      </Table>
      {props.editable && (
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-[2fr_1fr_1fr_auto] sm:items-end">
          <Field label="Description" required>
            <Input
              size="sm"
              placeholder="Ocean freight"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </Field>
          <Field label="Amount" required>
            <Input
              size="sm"
              placeholder="0.00"
              type="number"
              inputMode="decimal"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
          </Field>
          <Field label="Account" help="Optional GL code.">
            <Input
              size="sm"
              placeholder="Default"
              value={accountCode}
              onChange={(e) => setAccountCode(e.target.value)}
            />
          </Field>
          <Button
            size="sm"
            leadingIcon={<Plus className="size-4" />}
            disabled={
              upsertMut.isPending ||
              description.trim() === "" ||
              amount.trim() === ""
            }
            onClick={() =>
              upsertMut.mutate({
                description: description.trim(),
                amount: amount.trim(),
                account_code: accountCode.trim() || undefined,
              })
            }
          >
            Add charge
          </Button>
        </div>
      )}
      {upsertMut.isError ? (
        <p className="text-xs text-danger">
          Couldn't add charge: {(upsertMut.error as Error).message}
        </p>
      ) : null}
      {deleteMut.isError ? (
        <p className="text-xs text-danger">
          Couldn't delete charge: {(deleteMut.error as Error).message}
        </p>
      ) : null}
    </div>
  );
}

function TargetsSection(props: {
  fmt: ReturnType<typeof useFormatter>;
  voucher: LandedCostVoucher;
  targets: LandedCostTarget[];
  items: InventoryItem[];
  warehouses: InventoryWarehouse[];
  itemLabel: Map<string, string>;
  whLabel: Map<string, string>;
  editable: boolean;
  onMutated: () => void;
  totalAllocated: number;
  totalCharges: number;
}) {
  const { fmt } = props;
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
  // a sub-cent residual even when the backend's decimal type has them
  // exactly equal. The display rounds to two decimals, so anything
  // under half a cent is visual zero; gate the mismatch indicator on
  // the same threshold so a balanced voucher never shows a stray Δ.
  const reconcile = props.totalCharges - props.totalAllocated;
  const reconcileMismatch = Math.abs(reconcile) >= 0.005;

  const colSpan = props.editable ? 8 : 7;

  return (
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold text-fg">Items receiving cost</h3>
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Source</TableHead>
              <TableHead>Item</TableHead>
              <TableHead>Warehouse</TableHead>
              <TableHead className="text-right">Qty</TableHead>
              {/* Weight is only consumed server-side for the "By weight"
                  allocation method, but it stays in the table so operators
                  can audit the per-line weights they entered. */}
              <TableHead className="text-right">Weight</TableHead>
              <TableHead className="text-right">Unit cost</TableHead>
              <TableHead className="text-right">Allocated</TableHead>
              {props.editable && (
                <TableHead className="w-0 text-right">Actions</TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {props.targets.length === 0 ? (
              <TableRow>
                <TableCell colSpan={colSpan} className="text-center text-fg-subtle">
                  No items added yet.
                </TableCell>
              </TableRow>
            ) : (
              props.targets.map((t) => (
                <TableRow key={t.id}>
                  <TableCell className="text-fg-muted">
                    <span className="flex items-center gap-1.5">
                      {t.source_ktype ? humanizeKType(t.source_ktype) : "Manual"}
                      {t.applied && (
                        <Badge variant="success" size="xs">
                          Applied
                        </Badge>
                      )}
                    </span>
                  </TableCell>
                  <TableCell className="text-fg">
                    {props.itemLabel.get(t.item_id) ?? "Unknown item"}
                  </TableCell>
                  <TableCell className="text-fg-muted">
                    {props.whLabel.get(t.warehouse_id) ?? "Unknown warehouse"}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {fmt.number(Number(t.qty))}
                  </TableCell>
                  <TableCell className="text-right tabular-nums text-fg-muted">
                    {Number(t.weight) > 0 ? fmt.number(Number(t.weight)) : "—"}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {fmt.number(Number(t.unit_cost), MONEY_OPTS)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {fmt.number(Number(t.allocated_amount), MONEY_OPTS)}
                  </TableCell>
                  {props.editable && (
                    <TableCell className="text-right">
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => deleteMut.mutate(t.id)}
                        disabled={deleteMut.isPending}
                      >
                        Delete
                      </Button>
                    </TableCell>
                  )}
                </TableRow>
              ))
            )}
          </TableBody>
          <TableFooter>
            <TableRow>
              <TableCell colSpan={6} className="text-right font-medium text-fg">
                Total allocated
              </TableCell>
              <TableCell className="text-right font-semibold tabular-nums text-fg">
                {fmt.number(props.totalAllocated, MONEY_OPTS)}
              </TableCell>
              {props.editable && <TableCell />}
            </TableRow>
            {reconcileMismatch && props.totalAllocated > 0 && (
              <TableRow>
                <TableCell colSpan={colSpan} className="text-right text-xs text-danger">
                  Allocated total is off by {fmt.number(reconcile, MONEY_OPTS)} from
                  charges — re-allocate to balance.
                </TableCell>
              </TableRow>
            )}
          </TableFooter>
        </Table>
      </div>
      {props.editable && (
        <div className="grid grid-cols-1 gap-3 rounded-lg border border-border bg-bg-subtle p-3 sm:grid-cols-2 lg:grid-cols-3">
          <Field
            label="Item"
            required
            className="lg:col-span-1"
          >
            <Select
              size="sm"
              value={itemID}
              onChange={(e) => setItemID(e.target.value)}
            >
              <option value="">Select an item…</option>
              {props.items.map((it) => (
                <option key={it.id} value={it.id}>
                  {it.sku} — {it.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Warehouse" required>
            <Select
              size="sm"
              value={warehouseID}
              onChange={(e) => setWarehouseID(e.target.value)}
            >
              <option value="">Select a warehouse…</option>
              {props.warehouses.map((w) => (
                <option key={w.id} value={w.id}>
                  {w.code} — {w.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Source reference"
            required
            help="Receipt or PO this line came from."
          >
            <Input
              size="sm"
              placeholder="e.g. GRN-2045"
              value={sourceID}
              onChange={(e) => setSourceID(e.target.value)}
            />
          </Field>
          <Field label="Source type" help="Optional document type.">
            <Input
              size="sm"
              placeholder="Goods receipt"
              value={sourceKType}
              onChange={(e) => setSourceKType(e.target.value)}
            />
          </Field>
          <Field label="Quantity" required>
            <Input
              size="sm"
              placeholder="0"
              type="number"
              inputMode="decimal"
              value={qty}
              onChange={(e) => setQty(e.target.value)}
            />
          </Field>
          <Field label="Unit cost" required>
            <Input
              size="sm"
              placeholder="0.00"
              type="number"
              inputMode="decimal"
              value={unitCost}
              onChange={(e) => setUnitCost(e.target.value)}
            />
          </Field>
          <Field
            label="Weight"
            help="Used when allocating by weight."
          >
            <Input
              size="sm"
              placeholder="0"
              type="number"
              inputMode="decimal"
              value={weight}
              onChange={(e) => setWeight(e.target.value)}
            />
          </Field>
          <div className="flex items-end sm:col-span-2 lg:col-span-1">
            <Button
              size="sm"
              leadingIcon={<Plus className="size-4" />}
              disabled={
                upsertMut.isPending ||
                sourceID.trim() === "" ||
                itemID.trim() === "" ||
                warehouseID.trim() === "" ||
                qty.trim() === "" ||
                unitCost.trim() === ""
              }
              onClick={() =>
                upsertMut.mutate({
                  source_ktype: sourceKType.trim() || undefined,
                  source_id: sourceID.trim(),
                  item_id: itemID.trim(),
                  warehouse_id: warehouseID.trim(),
                  qty: qty.trim(),
                  unit_cost: unitCost.trim(),
                  weight: weight.trim() || undefined,
                })
              }
            >
              Add item
            </Button>
          </div>
        </div>
      )}
      {upsertMut.isError ? (
        <p className="text-xs text-danger">
          Couldn't add item: {(upsertMut.error as Error).message}
        </p>
      ) : null}
      {deleteMut.isError ? (
        <p className="text-xs text-danger">
          Couldn't delete item: {(deleteMut.error as Error).message}
        </p>
      ) : null}
    </div>
  );
}
