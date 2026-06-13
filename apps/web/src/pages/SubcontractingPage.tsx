import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  CreateSubcontractOrderInput,
  InventoryItem,
  InventoryWarehouse,
  SubcontractComponent,
  SubcontractOrder,
  SubcontractStatus,
} from "@kapp/client";
import {
  Badge,
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
import {
  st,
  type SubcontractingStringKey,
} from "../components/SubcontractingStrings";

const ORDERS_KEY = ["mfg", "subcontract-orders"] as const;

const STATUS_FILTERS: SubcontractStatus[] = [
  "draft",
  "issued",
  "received",
  "closed",
  "cancelled",
];

// The four lifecycle verbs the detail panel can drive. Kept as a union
// so the confirm-dialog copy and the mutation switch stay in lock-step.
type LifecycleAction = "issue" | "receive" | "close" | "cancel";

const BADGE_VARIANT: Record<
  SubcontractStatus,
  "default" | "info" | "success" | "warning" | "danger"
> = {
  draft: "default",
  issued: "warning",
  received: "info",
  closed: "success",
  cancelled: "danger",
};

function statusLabel(status: SubcontractStatus): string {
  return st(`subcontracting.status.${status}` as SubcontractingStringKey);
}

/**
 * SubcontractingPage renders the Batch-3 subcontracting workbench. The
 * left column creates orders (finished item + warehouse + components to
 * issue) and lists existing ones filtered by status; the right column
 * drills into the selected order and drives its lifecycle — issue
 * components → receive the finished item → close, or cancel a draft —
 * each behind a confirmation that explains the inventory side-effect.
 */
export function SubcontractingPage() {
  const [statusFilter, setStatusFilter] = useState<"" | SubcontractStatus>("");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const ordersQ = useQuery({
    queryKey: [...ORDERS_KEY, statusFilter],
    queryFn: () => api.listSubcontractOrders(statusFilter || undefined),
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

  const labelFor = (id: string) => itemLabel.get(id) ?? id;

  return (
    <section className="grid grid-cols-[3fr_4fr] gap-6">
      <div className="min-w-0">
        <h1>{st("subcontracting.title")}</h1>
        <p className="text-fg-muted">{st("subcontracting.subtitle")}</p>

        <CreateOrderForm
          items={itemsQ.data ?? []}
          warehouses={whQ.data ?? []}
          onCreated={(order) => setSelectedId(order.id)}
        />

        <div className="mt-6 mb-2 flex items-center gap-2">
          <h2 className="m-0 mr-auto">{st("subcontracting.orders.heading")}</h2>
          <label htmlFor="sub-status" className="text-[13px]">
            {st("subcontracting.orders.filterStatus")}
          </label>
          <Select
            id="sub-status"
            value={statusFilter}
            onChange={(e) =>
              setStatusFilter(e.target.value as "" | SubcontractStatus)
            }
          >
            <option value="">{st("subcontracting.orders.filterAll")}</option>
            {STATUS_FILTERS.map((s) => (
              <option key={s} value={s}>
                {statusLabel(s)}
              </option>
            ))}
          </Select>
        </div>

        {ordersQ.isLoading && <p>{st("subcontracting.orders.loading")}</p>}
        {ordersQ.isError && (
          <p className="text-danger">
            {st("subcontracting.orders.error")} {String(ordersQ.error)}
          </p>
        )}
        {ordersQ.data && ordersQ.data.length === 0 && (
          <p className="text-fg-muted">{st("subcontracting.orders.empty")}</p>
        )}
        {ordersQ.data && ordersQ.data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{st("subcontracting.orders.item")}</TableHead>
                <TableHead className="text-right">
                  {st("subcontracting.orders.qty")}
                </TableHead>
                <TableHead>{st("subcontracting.orders.status")}</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {ordersQ.data.map((order: SubcontractOrder) => (
                <TableRow
                  key={order.id}
                  className={
                    order.id === selectedId ? "bg-accent/10" : undefined
                  }
                >
                  <TableCell>{labelFor(order.item_id)}</TableCell>
                  <TableCell className="text-right">{order.qty}</TableCell>
                  <TableCell>
                    <Badge variant={BADGE_VARIANT[order.status]}>
                      {statusLabel(order.status)}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setSelectedId(order.id)}
                      aria-label={`${st("subcontracting.orders.view")} ${labelFor(order.item_id)}`}
                    >
                      {st("subcontracting.orders.view")}
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <div className="min-w-0">
        <h2>{st("subcontracting.detail.heading")}</h2>
        {selectedId ? (
          <OrderDetail
            key={selectedId}
            orderId={selectedId}
            labelFor={labelFor}
            whLabel={whLabel}
          />
        ) : (
          <p className="text-fg-muted">{st("subcontracting.detail.select")}</p>
        )}
      </div>
    </section>
  );
}

interface CreateOrderFormProps {
  items: InventoryItem[];
  warehouses: InventoryWarehouse[];
  onCreated: (order: SubcontractOrder) => void;
}

interface ComponentDraft {
  item_id: string;
  qty: string;
}

function CreateOrderForm({
  items,
  warehouses,
  onCreated,
}: CreateOrderFormProps) {
  const qc = useQueryClient();
  const [itemID, setItemID] = useState("");
  const [whID, setWhID] = useState("");
  const [qty, setQty] = useState("1");
  const [supplierID, setSupplierID] = useState("");
  const [charge, setCharge] = useState("");
  const [chargeCurrency, setChargeCurrency] = useState("");
  const [notes, setNotes] = useState("");
  const [components, setComponents] = useState<ComponentDraft[]>([
    { item_id: "", qty: "1" },
  ]);

  const createMut = useMutation({
    mutationFn: (input: CreateSubcontractOrderInput) =>
      api.createSubcontractOrder(input),
    onSuccess: (order) => {
      qc.invalidateQueries({ queryKey: ORDERS_KEY });
      onCreated(order);
      setItemID("");
      setWhID("");
      setQty("1");
      setSupplierID("");
      setCharge("");
      setChargeCurrency("");
      setNotes("");
      setComponents([{ item_id: "", qty: "1" }]);
    },
  });

  const validComponents = components.filter((c) => c.item_id !== "");
  const canSubmit = itemID !== "" && whID !== "" && validComponents.length > 0;

  const addComponent = () =>
    setComponents((c) => [...c, { item_id: "", qty: "1" }]);
  const removeComponent = (idx: number) =>
    setComponents((c) => c.filter((_, i) => i !== idx));
  const patchComponent = (idx: number, patch: Partial<ComponentDraft>) =>
    setComponents((c) =>
      c.map((row, i) => (i === idx ? { ...row, ...patch } : row)),
    );

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!canSubmit) return;
    const input: CreateSubcontractOrderInput = {
      item_id: itemID,
      warehouse_id: whID,
      qty,
      supplier_id: supplierID.trim() || undefined,
      charge_amount: charge.trim() || undefined,
      charge_currency: chargeCurrency.trim() || undefined,
      notes: notes.trim() || undefined,
      components: validComponents.map((c) => ({
        item_id: c.item_id,
        qty: c.qty,
      })),
    };
    createMut.mutate(input);
  };

  return (
    <form
      onSubmit={submit}
      className="mt-4 flex flex-col gap-3 rounded-md bg-bg-subtle p-3"
    >
      <h2 className="m-0">{st("subcontracting.create.heading")}</h2>
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-[13px]">
          {st("subcontracting.create.item")}
          <Select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
          >
            <option value="">{st("subcontracting.create.selectItem")}</option>
            {items.map((it) => (
              <option key={it.id} value={it.id}>
                {it.sku} — {it.name}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          {st("subcontracting.create.warehouse")}
          <Select
            value={whID}
            onChange={(e) => setWhID(e.target.value)}
            required
          >
            <option value="">
              {st("subcontracting.create.selectWarehouse")}
            </option>
            {warehouses.map((w) => (
              <option key={w.id} value={w.id}>
                {w.code} — {w.name}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          {st("subcontracting.create.qty")}
          <Input
            type="number"
            step="0.01"
            min="0"
            value={qty}
            onChange={(e) => setQty(e.target.value)}
            required
            className="w-24"
          />
        </label>
      </div>

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-[13px]">
          {st("subcontracting.create.charge")}
          <Input
            type="number"
            step="0.01"
            min="0"
            value={charge}
            onChange={(e) => setCharge(e.target.value)}
            className="w-28"
          />
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          {st("subcontracting.create.chargeCurrency")}
          <Input
            value={chargeCurrency}
            onChange={(e) => setChargeCurrency(e.target.value)}
            placeholder="USD"
            maxLength={3}
            className="w-20 uppercase"
          />
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          {st("subcontracting.create.supplier")}
          <Input
            value={supplierID}
            onChange={(e) => setSupplierID(e.target.value)}
            placeholder="00000000-0000-…"
            className="w-56"
          />
        </label>
      </div>
      <p className="-mt-1 text-xs text-fg-muted">
        {st("subcontracting.create.supplierHint")}
      </p>

      <fieldset className="m-0 border-0 p-0">
        <legend className="text-[13px] font-semibold">
          {st("subcontracting.create.components")}
        </legend>
        <p className="text-xs text-fg-muted">
          {st("subcontracting.create.componentsHint")}
        </p>
        {components.map((row, idx) => (
          <div key={idx} className="mt-2 flex flex-wrap items-end gap-2">
            <label className="flex flex-col gap-1 text-[13px]">
              {st("subcontracting.detail.componentItem")}
              <Select
                aria-label={`component ${idx + 1}`}
                value={row.item_id}
                onChange={(e) =>
                  patchComponent(idx, { item_id: e.target.value })
                }
              >
                <option value="">
                  {st("subcontracting.create.selectItem")}
                </option>
                {items.map((it) => (
                  <option key={it.id} value={it.id}>
                    {it.sku} — {it.name}
                  </option>
                ))}
              </Select>
            </label>
            <label className="flex flex-col gap-1 text-[13px]">
              {st("subcontracting.detail.componentQty")}
              <Input
                aria-label={`component qty ${idx + 1}`}
                type="number"
                step="0.01"
                min="0"
                value={row.qty}
                onChange={(e) => patchComponent(idx, { qty: e.target.value })}
                className="w-24"
              />
            </label>
            {components.length > 1 && (
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={() => removeComponent(idx)}
              >
                {st("subcontracting.create.removeComponent")}
              </Button>
            )}
          </div>
        ))}
        <Button
          type="button"
          size="sm"
          variant="outline"
          className="mt-2"
          onClick={addComponent}
        >
          {st("subcontracting.create.addComponent")}
        </Button>
      </fieldset>

      <label className="flex flex-col gap-1 text-[13px]">
        {st("subcontracting.create.notes")}
        <Input value={notes} onChange={(e) => setNotes(e.target.value)} />
      </label>

      <div className="flex items-center gap-2">
        <Button type="submit" disabled={createMut.isPending || !canSubmit}>
          {createMut.isPending
            ? st("subcontracting.create.submitting")
            : st("subcontracting.create.submit")}
        </Button>
        {!canSubmit && (
          <span className="text-xs text-fg-muted">
            {st("subcontracting.create.needsComponents")}
          </span>
        )}
        {createMut.isError && (
          <span className="text-danger">{String(createMut.error)}</span>
        )}
      </div>
    </form>
  );
}

interface OrderDetailProps {
  orderId: string;
  labelFor: (id: string) => string;
  whLabel: Map<string, string>;
}

function OrderDetail({ orderId, labelFor, whLabel }: OrderDetailProps) {
  const qc = useQueryClient();
  const [pending, setPending] = useState<LifecycleAction | null>(null);

  const orderQ = useQuery({
    queryKey: [...ORDERS_KEY, "detail", orderId],
    queryFn: () => api.getSubcontractOrder(orderId),
  });

  const actionMut = useMutation({
    mutationFn: (action: LifecycleAction) => {
      switch (action) {
        case "issue":
          return api.issueSubcontractOrder(orderId);
        case "receive":
          return api.receiveSubcontractOrder(orderId);
        case "close":
          return api.closeSubcontractOrder(orderId);
        case "cancel":
          return api.cancelSubcontractOrder(orderId);
      }
    },
    onSuccess: () => {
      // Optimistic refresh: the detail query key is constructed under
      // ORDERS_KEY (`[...ORDERS_KEY, "detail", orderId]`), so this single
      // prefix-matched invalidation refetches both the open detail and
      // every status-filtered list — the moved order disappears from /
      // lands in the right bucket without a manual reload.
      qc.invalidateQueries({ queryKey: ORDERS_KEY });
      setPending(null);
    },
  });

  if (orderQ.isLoading) return <p>{st("subcontracting.detail.loading")}</p>;
  if (orderQ.isError)
    return (
      <p className="text-danger">
        {st("subcontracting.detail.error")} {String(orderQ.error)}
      </p>
    );
  const order = orderQ.data;
  if (!order) return null;

  const components = order.components ?? [];

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={BADGE_VARIANT[order.status]}>
          {statusLabel(order.status)}
        </Badge>
        <span className="font-semibold">{labelFor(order.item_id)}</span>
      </div>

      <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-[13px]">
        <dt className="text-fg-muted">{st("subcontracting.detail.qty")}</dt>
        <dd>{order.qty}</dd>
        <dt className="text-fg-muted">{st("subcontracting.detail.received")}</dt>
        <dd>{order.received_qty}</dd>
        <dt className="text-fg-muted">{st("subcontracting.detail.warehouse")}</dt>
        <dd>{whLabel.get(order.warehouse_id) ?? order.warehouse_id}</dd>
        <dt className="text-fg-muted">{st("subcontracting.detail.charge")}</dt>
        <dd>
          {order.charge_amount}
          {order.charge_currency ? ` ${order.charge_currency}` : ""}
        </dd>
        {order.supplier_id && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.supplier")}
            </dt>
            <dd className="truncate">{order.supplier_id}</dd>
          </>
        )}
        {order.issued_at && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.issuedAt")}
            </dt>
            <dd>{order.issued_at.slice(0, 10)}</dd>
          </>
        )}
        {order.received_at && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.receivedAt")}
            </dt>
            <dd>{order.received_at.slice(0, 10)}</dd>
          </>
        )}
        {order.notes && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.notes")}
            </dt>
            <dd>{order.notes}</dd>
          </>
        )}
      </dl>

      <div>
        <h3 className="m-0">{st("subcontracting.detail.componentsHeading")}</h3>
        {components.length === 0 ? (
          <p className="text-fg-muted">{st("subcontracting.detail.none")}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>
                  {st("subcontracting.detail.componentItem")}
                </TableHead>
                <TableHead className="text-right">
                  {st("subcontracting.detail.componentQty")}
                </TableHead>
                <TableHead className="text-right">
                  {st("subcontracting.detail.componentIssued")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {components.map((c: SubcontractComponent) => (
                <TableRow key={c.id}>
                  <TableCell>{labelFor(c.item_id)}</TableCell>
                  <TableCell className="text-right">{c.qty}</TableCell>
                  <TableCell className="text-right">{c.issued_qty}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <LifecycleActions
        status={order.status}
        disabled={actionMut.isPending}
        onAct={(action) => setPending(action)}
      />
      {actionMut.isError && (
        <p className="text-danger">{String(actionMut.error)}</p>
      )}

      <ActionConfirm
        action={pending}
        loading={actionMut.isPending}
        onCancel={() => setPending(null)}
        onConfirm={() => pending && actionMut.mutate(pending)}
      />
    </div>
  );
}

interface LifecycleActionsProps {
  status: SubcontractStatus;
  disabled: boolean;
  onAct: (action: LifecycleAction) => void;
}

// LifecycleActions renders only the legal next transitions for the
// order's current status, mirroring SubcontractOrder.CanTransitionTo on
// the server (draft → issue|cancel, issued → receive, received → close).
function LifecycleActions({ status, disabled, onAct }: LifecycleActionsProps) {
  return (
    <div className="flex flex-wrap gap-2">
      {status === "draft" && (
        <>
          <Button disabled={disabled} onClick={() => onAct("issue")}>
            {st("subcontracting.action.issue")}
          </Button>
          <Button
            variant="outline"
            disabled={disabled}
            onClick={() => onAct("cancel")}
          >
            {st("subcontracting.action.cancel")}
          </Button>
        </>
      )}
      {status === "issued" && (
        <Button disabled={disabled} onClick={() => onAct("receive")}>
          {st("subcontracting.action.receive")}
        </Button>
      )}
      {status === "received" && (
        <Button disabled={disabled} onClick={() => onAct("close")}>
          {st("subcontracting.action.close")}
        </Button>
      )}
    </div>
  );
}

interface ActionConfirmProps {
  action: LifecycleAction | null;
  loading: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}

const CONFIRM_COPY: Record<
  LifecycleAction,
  { title: SubcontractingStringKey; body: SubcontractingStringKey; destructive: boolean }
> = {
  issue: {
    title: "subcontracting.confirm.issueTitle",
    body: "subcontracting.confirm.issueBody",
    destructive: false,
  },
  receive: {
    title: "subcontracting.confirm.receiveTitle",
    body: "subcontracting.confirm.receiveBody",
    destructive: false,
  },
  close: {
    title: "subcontracting.confirm.closeTitle",
    body: "subcontracting.confirm.closeBody",
    destructive: false,
  },
  cancel: {
    title: "subcontracting.confirm.cancelTitle",
    body: "subcontracting.confirm.cancelBody",
    destructive: true,
  },
};

function ActionConfirm({
  action,
  loading,
  onCancel,
  onConfirm,
}: ActionConfirmProps) {
  const copy = action ? CONFIRM_COPY[action] : null;
  return (
    <ConfirmDialog
      open={action !== null}
      onOpenChange={(open) => {
        if (!open) onCancel();
      }}
      title={copy ? st(copy.title) : ""}
      description={copy ? st(copy.body) : undefined}
      confirmLabel={st("subcontracting.confirm.confirm")}
      cancelLabel={st("subcontracting.confirm.cancel")}
      destructive={copy?.destructive ?? false}
      loading={loading}
      onConfirm={onConfirm}
    />
  );
}

export default SubcontractingPage;
