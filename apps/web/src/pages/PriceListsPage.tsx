import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
  EmptyState,
  Input,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { Plus, Tags, Trash2 } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import {
  RecordSelect,
  buildNameResolver,
  itemOptions as toItemOptions,
  round2,
  type ItemOption,
} from "../components/lineitems";

const KTYPE = "sales.price_list";

interface PriceListItem {
  item_id: string;
  price: number | string;
  discount_percent?: number | string;
  min_qty?: number | string;
}

interface PriceListData {
  name?: string;
  currency?: string;
  customer_id?: string;
  valid_from?: string;
  valid_until?: string;
  items?: PriceListItem[];
  active?: boolean;
}

export function PriceListsPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });
  const orgsQ = useQuery<KRecord[]>({
    queryKey: ["records", "crm.organization"],
    queryFn: () => api.listRecords("crm.organization"),
  });
  const itemsQ = useQuery<KRecord[]>({
    queryKey: ["records", "inventory.item"],
    queryFn: () => api.listRecords("inventory.item"),
  });

  const customerName = useMemo(() => buildNameResolver(orgsQ.data), [orgsQ.data]);
  const itemOpts = useMemo(() => toItemOptions(itemsQ.data ?? []), [itemsQ.data]);

  const selected = useMemo(
    () => (q.data ?? []).find((r) => r.id === selectedId) ?? null,
    [q.data, selectedId],
  );

  const updateMutation = useMutation({
    mutationFn: (r: KRecord) => api.updateRecord(KTYPE, r.id, r.data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
      toast.success("Price list saved");
    },
    onError: (e: Error) => toast.error("Couldn’t save price list", { description: e.message }),
  });

  const fmtDate = (value?: string) => {
    if (!value) return "—";
    const d = new Date(value.length === 10 ? `${value}T00:00:00` : value);
    return Number.isNaN(d.getTime()) ? "—" : fmt.date(d);
  };

  const lists = q.data ?? [];

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-1">
        <Eyebrow>Sales</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">Price Lists</h1>
        <p className="max-w-prose text-sm text-fg-muted">
          Set negotiated pricing per customer or for everyone. Pick a list to edit the items,
          discounts, and minimum quantities it covers.
        </p>
      </header>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[320px_1fr]">
        <div className="rounded-lg border border-border bg-bg-subtle p-2">
          {q.isLoading ? (
            <div className="flex flex-col gap-2 p-1">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} variant="rect" className="h-14 w-full" />
              ))}
            </div>
          ) : q.isError ? (
            <div role="alert" className="flex flex-col items-start gap-2 p-3">
              <p className="text-sm text-danger">
                Couldn’t load price lists. {(q.error as Error)?.message ?? ""}
              </p>
              <Button variant="outline" size="sm" onClick={() => q.refetch()}>
                Try again
              </Button>
            </div>
          ) : lists.length === 0 ? (
            <EmptyState
              icon={<Tags aria-hidden="true" />}
              title="No price lists yet"
              description="Price lists let you offer agreed rates to specific customers."
            />
          ) : (
            <ul className="flex list-none flex-col gap-1 p-0">
              {lists.map((r) => {
                const d = r.data as unknown as PriceListData;
                const isSel = selectedId === r.id;
                const count = d.items?.length ?? 0;
                return (
                  <li key={r.id}>
                    <button
                      type="button"
                      aria-pressed={isSel}
                      onClick={() => setSelectedId(r.id)}
                      className={[
                        "flex w-full flex-col gap-1 rounded-md border px-3 py-2 text-left transition-colors",
                        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                        isSel
                          ? "border-accent bg-bg-elevated"
                          : "border-transparent hover:border-border hover:bg-bg-elevated",
                      ].join(" ")}
                    >
                      <div className="flex items-center justify-between gap-2">
                        <span className="font-medium text-fg">{d.name || "Untitled list"}</span>
                        <Badge variant={d.active === false ? "outline" : "success"} size="xs">
                          {d.active === false ? "Inactive" : "Active"}
                        </Badge>
                      </div>
                      <span className="text-xs text-fg-muted">
                        {(d.currency || "USD")} · {customerName(d.customer_id) || "All customers"}
                      </span>
                      <span className="text-xs text-fg-subtle">
                        {count} item{count === 1 ? "" : "s"}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>

        <div className="rounded-lg border border-border bg-bg-elevated p-4">
          {selected ? (
            <PriceListEditor
              key={selected.id}
              record={selected}
              itemOptions={itemOpts}
              fmtDate={fmtDate}
              saving={updateMutation.isPending}
              onSave={(r) => updateMutation.mutate(r)}
            />
          ) : (
            <div className="flex h-full items-center justify-center py-12 text-center text-sm text-fg-muted">
              Select a price list to edit its items.
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

function PriceListEditor({
  record,
  itemOptions,
  fmtDate,
  saving,
  onSave,
}: {
  record: KRecord;
  itemOptions: ItemOption[];
  fmtDate: (value?: string) => string;
  saving: boolean;
  onSave: (r: KRecord) => void;
}) {
  const fmt = useFormatter();
  const initial = record.data as unknown as PriceListData;
  const currency = initial.currency || "USD";
  const [items, setItems] = useState<PriceListItem[]>(initial.items ?? []);

  const money = (n: number) => fmt.currency(n, currency, { currencyDisplay: "code" });

  const updateRow = (i: number, patch: Partial<PriceListItem>) =>
    setItems((prev) => prev.map((row, idx) => (idx === i ? { ...row, ...patch } : row)));
  const addRow = () => setItems((prev) => [...prev, { item_id: "", price: 0 }]);
  const removeRow = (i: number) => setItems((prev) => prev.filter((_, idx) => idx !== i));

  const save = () => onSave({ ...record, data: { ...record.data, items } });

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-semibold text-fg">{initial.name || "Untitled list"}</h2>
          <p className="text-sm text-fg-muted">
            {currency} · valid {fmtDate(initial.valid_from)} – {fmtDate(initial.valid_until)}
          </p>
        </div>
        <Badge variant={initial.active === false ? "outline" : "success"}>
          {initial.active === false ? "Inactive" : "Active"}
        </Badge>
      </div>

      <div className="overflow-x-auto rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Item</TableHead>
              <TableHead className="text-end">Price ({currency})</TableHead>
              <TableHead className="text-end">Discount %</TableHead>
              <TableHead className="text-end">Min qty</TableHead>
              <TableHead className="text-end">Effective</TableHead>
              <TableHead className="w-12">
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="py-6 text-center text-sm text-fg-muted">
                  No items yet. Add your first row below.
                </TableCell>
              </TableRow>
            ) : (
              items.map((r, i) => {
                const price = Number(r.price) || 0;
                const discount = Number(r.discount_percent ?? 0) || 0;
                const effective = round2(price * (1 - discount / 100));
                return (
                  <TableRow key={i}>
                    <TableCell className="min-w-[200px]">
                      <RecordSelect
                        aria-label={`Item for row ${i + 1}`}
                        value={r.item_id}
                        onChange={(v) => updateRow(i, { item_id: v })}
                        options={itemOptions}
                        placeholder="Select item…"
                      />
                    </TableCell>
                    <TableCell className="text-end">
                      <Input
                        type="number"
                        min={0}
                        step="0.01"
                        aria-label={`Price for row ${i + 1}`}
                        className="text-end"
                        value={String(r.price)}
                        onChange={(e) => updateRow(i, { price: Number(e.target.value) })}
                      />
                    </TableCell>
                    <TableCell className="text-end">
                      <Input
                        type="number"
                        min={0}
                        max={100}
                        aria-label={`Discount percent for row ${i + 1}`}
                        className="text-end"
                        value={String(r.discount_percent ?? 0)}
                        onChange={(e) => updateRow(i, { discount_percent: Number(e.target.value) })}
                      />
                    </TableCell>
                    <TableCell className="text-end">
                      <Input
                        type="number"
                        min={0}
                        aria-label={`Minimum quantity for row ${i + 1}`}
                        className="text-end"
                        value={String(r.min_qty ?? 0)}
                        onChange={(e) => updateRow(i, { min_qty: Number(e.target.value) })}
                      />
                    </TableCell>
                    <TableCell className="text-end tabular-nums text-fg-muted">
                      {money(effective)}
                    </TableCell>
                    <TableCell className="text-end">
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label={`Remove row ${i + 1}`}
                        onClick={() => removeRow(i)}
                      >
                        <Trash2 className="h-4 w-4" aria-hidden="true" />
                      </Button>
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>

      <div className="flex items-center justify-between">
        <Button
          variant="outline"
          onClick={addRow}
          leadingIcon={<Plus className="h-4 w-4" aria-hidden="true" />}
        >
          Add item
        </Button>
        <Button onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save changes"}
        </Button>
      </div>
    </div>
  );
}
