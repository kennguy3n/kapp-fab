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
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, Inbox, PackageCheck, Plus } from "lucide-react";
import { api } from "../lib/api";
import { parseCalendarDate } from "../lib/date";
import { useFormatter } from "../lib/i18n";
import {
  st,
  type SubcontractingStringKey,
} from "../components/SubcontractingStrings";

type Formatters = ReturnType<typeof useFormatter>;

const ORDERS_KEY = ["mfg", "subcontract-orders"] as const;

const MONEY_OPTS: Intl.NumberFormatOptions = {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
};

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

const BADGE_VARIANT: Record<SubcontractStatus, BadgeProps["variant"]> = {
  draft: "neutral",
  issued: "warning",
  received: "info",
  closed: "success",
  cancelled: "danger",
};

function statusLabel(status: SubcontractStatus): string {
  return st(`subcontracting.status.${status}` as SubcontractingStringKey);
}

function formatQty(fmt: Formatters, value: string): string {
  return fmt.number(Number(value));
}

// formatCharge renders a money amount using the order's ISO currency
// when present, falling back to a plain 2-dp number when it is missing
// or invalid (fmt.currency throws RangeError on a non-ISO code).
function formatCharge(
  fmt: Formatters,
  amount: string,
  currency?: string,
): string {
  const n = Number(amount);
  if (currency) {
    try {
      return fmt.currency(n, currency, MONEY_OPTS);
    } catch {
      /* fall through to a plain formatted number */
    }
  }
  return fmt.number(n, MONEY_OPTS);
}

/**
 * SubcontractingPage renders the subcontracting workbench. The left
 * column creates orders (finished item + warehouse + components to
 * issue) and lists existing ones filtered by status; the right column
 * drills into the selected order and drives its lifecycle — issue
 * components → receive the finished item → close, or cancel a draft —
 * each behind a confirmation that explains the inventory side-effect.
 */
export function SubcontractingPage() {
  const fmt = useFormatter();
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
  const orders = ordersQ.data ?? [];

  return (
    <section className="grid gap-6 lg:grid-cols-[minmax(0,3fr)_minmax(0,4fr)]">
      <div className="flex min-w-0 flex-col gap-5">
        <header className="flex flex-col gap-1">
          <Eyebrow>{st("subcontracting.eyebrow")}</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">
            {st("subcontracting.title")}
          </h1>
          <p className="text-sm text-fg-muted">
            {st("subcontracting.subtitle")}
          </p>
        </header>

        <CreateOrderForm
          items={itemsQ.data ?? []}
          warehouses={whQ.data ?? []}
          onCreated={(order) => setSelectedId(order.id)}
        />

        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <h2 className="text-sm font-semibold tracking-tight text-fg">
              {st("subcontracting.orders.heading")}
            </h2>
            <Field label={st("subcontracting.orders.filterStatus")}>
              <Select
                size="sm"
                value={statusFilter}
                onChange={(e) =>
                  setStatusFilter(e.target.value as "" | SubcontractStatus)
                }
              >
                <option value="">
                  {st("subcontracting.orders.filterAll")}
                </option>
                {STATUS_FILTERS.map((s) => (
                  <option key={s} value={s}>
                    {statusLabel(s)}
                  </option>
                ))}
              </Select>
            </Field>
          </div>

          {ordersQ.isLoading ? (
            <div className="flex flex-col gap-2">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-12 w-full" />
              <Skeleton className="h-12 w-full" />
            </div>
          ) : ordersQ.isError ? (
            <EmptyState
              icon={<AlertTriangle className="size-6" />}
              title={st("subcontracting.orders.errorTitle")}
              description={(ordersQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void ordersQ.refetch()}
                  disabled={ordersQ.isFetching}
                >
                  {st("subcontracting.orders.retry")}
                </Button>
              }
            />
          ) : orders.length === 0 ? (
            <EmptyState
              icon={<Inbox className="size-6" />}
              title={st("subcontracting.orders.emptyTitle")}
              description={st("subcontracting.orders.emptyBody")}
            />
          ) : (
            <div className="overflow-x-auto rounded-xl border border-border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{st("subcontracting.orders.item")}</TableHead>
                    <TableHead className="text-right">
                      {st("subcontracting.orders.qty")}
                    </TableHead>
                    <TableHead className="text-right">
                      {st("subcontracting.orders.charge")}
                    </TableHead>
                    <TableHead>{st("subcontracting.orders.status")}</TableHead>
                    <TableHead className="text-right">
                      <span className="sr-only">
                        {st("subcontracting.orders.view")}
                      </span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {orders.map((order: SubcontractOrder) => {
                    const selected = order.id === selectedId;
                    return (
                      <TableRow
                        key={order.id}
                        data-selected={selected ? "" : undefined}
                        className={selected ? "bg-accent/10" : undefined}
                      >
                        <TableCell className="font-medium">
                          {labelFor(order.item_id)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatQty(fmt, order.qty)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums whitespace-nowrap">
                          {formatCharge(
                            fmt,
                            order.charge_amount,
                            order.charge_currency,
                          )}
                        </TableCell>
                        <TableCell>
                          <Badge variant={BADGE_VARIANT[order.status]}>
                            {statusLabel(order.status)}
                          </Badge>
                        </TableCell>
                        <TableCell className="text-right">
                          <Button
                            size="sm"
                            variant={selected ? "secondary" : "outline"}
                            onClick={() => setSelectedId(order.id)}
                            aria-label={`${st("subcontracting.orders.view")} ${labelFor(order.item_id)}`}
                          >
                            {st("subcontracting.orders.view")}
                          </Button>
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          )}
        </div>
      </div>

      <div className="flex min-w-0 flex-col gap-3">
        <h2 className="text-sm font-semibold tracking-tight text-fg">
          {st("subcontracting.detail.heading")}
        </h2>
        {selectedId ? (
          <OrderDetail
            key={selectedId}
            orderId={selectedId}
            labelFor={labelFor}
            whLabel={whLabel}
            fmt={fmt}
          />
        ) : (
          <EmptyState
            icon={<PackageCheck className="size-6" />}
            title={st("subcontracting.detail.selectTitle")}
            description={st("subcontracting.detail.select")}
          />
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
      className="flex flex-col gap-4 rounded-xl border border-border bg-bg-subtle p-4"
    >
      <h2 className="text-sm font-semibold tracking-tight text-fg">
        {st("subcontracting.create.heading")}
      </h2>
      <div className="flex flex-wrap items-start gap-3">
        <Field label={st("subcontracting.create.item")} required>
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
        </Field>
        <Field label={st("subcontracting.create.warehouse")} required>
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
        </Field>
        <Field label={st("subcontracting.create.qty")} required>
          <Input
            type="number"
            step="0.01"
            min="0"
            value={qty}
            onChange={(e) => setQty(e.target.value)}
            required
            className="w-24 tabular-nums"
          />
        </Field>
      </div>

      <div className="flex flex-wrap items-start gap-3">
        <Field label={st("subcontracting.create.charge")}>
          <Input
            type="number"
            step="0.01"
            min="0"
            value={charge}
            onChange={(e) => setCharge(e.target.value)}
            className="w-28 tabular-nums"
          />
        </Field>
        <Field label={st("subcontracting.create.chargeCurrency")}>
          <Input
            value={chargeCurrency}
            onChange={(e) => setChargeCurrency(e.target.value)}
            placeholder="USD"
            maxLength={3}
            className="w-20 uppercase"
          />
        </Field>
        <Field
          label={st("subcontracting.create.supplier")}
          help={st("subcontracting.create.supplierHint")}
        >
          <Input
            value={supplierID}
            onChange={(e) => setSupplierID(e.target.value)}
            placeholder="Organisation reference"
            className="w-56"
          />
        </Field>
      </div>

      <fieldset className="m-0 flex flex-col gap-2 border-0 p-0">
        <legend className="text-sm font-semibold text-fg">
          {st("subcontracting.create.components")}
        </legend>
        <p className="text-xs text-fg-muted">
          {st("subcontracting.create.componentsHint")}
        </p>
        {components.map((row, idx) => (
          <div
            key={idx}
            className="flex flex-wrap items-end gap-2 rounded-lg border border-border bg-bg p-2"
          >
            <Field label={st("subcontracting.detail.componentItem")} hideLabel>
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
            </Field>
            <Field label={st("subcontracting.detail.componentQty")} hideLabel>
              <Input
                aria-label={`component qty ${idx + 1}`}
                type="number"
                step="0.01"
                min="0"
                value={row.qty}
                onChange={(e) => patchComponent(idx, { qty: e.target.value })}
                className="w-24 tabular-nums"
              />
            </Field>
            {components.length > 1 && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => removeComponent(idx)}
              >
                {st("subcontracting.create.removeComponent")}
              </Button>
            )}
          </div>
        ))}
        <div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            leadingIcon={<Plus className="size-4" />}
            onClick={addComponent}
          >
            {st("subcontracting.create.addComponent")}
          </Button>
        </div>
      </fieldset>

      <Field label={st("subcontracting.create.notes")}>
        <Input value={notes} onChange={(e) => setNotes(e.target.value)} />
      </Field>

      <div className="flex flex-wrap items-center gap-2">
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
          <span className="text-sm text-danger">
            {(createMut.error as Error).message}
          </span>
        )}
      </div>
    </form>
  );
}

interface OrderDetailProps {
  orderId: string;
  labelFor: (id: string) => string;
  whLabel: Map<string, string>;
  fmt: Formatters;
}

function OrderDetail({ orderId, labelFor, whLabel, fmt }: OrderDetailProps) {
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

  if (orderQ.isLoading)
    return (
      <div className="flex flex-col gap-2">
        <Skeleton className="h-9 w-2/3" />
        <Skeleton className="h-28 w-full" />
      </div>
    );
  if (orderQ.isError)
    return (
      <EmptyState
        icon={<AlertTriangle className="size-6" />}
        title={st("subcontracting.detail.errorTitle")}
        description={(orderQ.error as Error).message}
        action={
          <Button
            variant="secondary"
            onClick={() => void orderQ.refetch()}
            disabled={orderQ.isFetching}
          >
            {st("subcontracting.orders.retry")}
          </Button>
        }
      />
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
        <span className="font-medium text-fg">{labelFor(order.item_id)}</span>
      </div>

      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm">
        <dt className="text-fg-muted">{st("subcontracting.detail.qty")}</dt>
        <dd className="tabular-nums text-fg">{formatQty(fmt, order.qty)}</dd>
        <dt className="text-fg-muted">{st("subcontracting.detail.received")}</dt>
        <dd className="tabular-nums text-fg">
          {formatQty(fmt, order.received_qty)}
        </dd>
        <dt className="text-fg-muted">
          {st("subcontracting.detail.warehouse")}
        </dt>
        <dd className="text-fg">
          {whLabel.get(order.warehouse_id) ?? order.warehouse_id}
        </dd>
        <dt className="text-fg-muted">{st("subcontracting.detail.charge")}</dt>
        <dd className="tabular-nums text-fg">
          {formatCharge(fmt, order.charge_amount, order.charge_currency)}
        </dd>
        {order.supplier_id && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.supplier")}
            </dt>
            <dd className="truncate text-fg">{order.supplier_id}</dd>
          </>
        )}
        {order.issued_at && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.issuedAt")}
            </dt>
            <dd className="text-fg">
              {fmt.date(parseCalendarDate(order.issued_at))}
            </dd>
          </>
        )}
        {order.received_at && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.receivedAt")}
            </dt>
            <dd className="text-fg">
              {fmt.date(parseCalendarDate(order.received_at))}
            </dd>
          </>
        )}
        {order.notes && (
          <>
            <dt className="text-fg-muted">
              {st("subcontracting.detail.notes")}
            </dt>
            <dd className="text-fg">{order.notes}</dd>
          </>
        )}
      </dl>

      <div className="flex flex-col gap-2">
        <h3 className="text-sm font-semibold tracking-tight text-fg">
          {st("subcontracting.detail.componentsHeading")}
        </h3>
        {components.length === 0 ? (
          <p className="text-sm text-fg-muted">
            {st("subcontracting.detail.none")}
          </p>
        ) : (
          <div className="overflow-x-auto rounded-xl border border-border">
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
                    <TableCell className="text-right tabular-nums">
                      {formatQty(fmt, c.qty)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatQty(fmt, c.issued_qty)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      <LifecycleActions
        status={order.status}
        disabled={actionMut.isPending}
        onAct={(action) => setPending(action)}
      />
      {actionMut.isError && (
        <p className="text-sm text-danger">
          {(actionMut.error as Error).message}
        </p>
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
