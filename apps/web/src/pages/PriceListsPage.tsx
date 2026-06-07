import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

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

/**
 * PriceListsPage lists `sales.price_list` KRecords and lets the user
 * drill into one to edit its `items` matrix. Editing a single row
 * issues a PATCH against the whole record — price lists are low-cardinality
 * so the naive replace-all update is acceptable.
 */
export function PriceListsPage() {
  const qc = useQueryClient();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = useMemo(
    () => (q.data ?? []).find((r) => r.id === selectedId) ?? null,
    [q.data, selectedId]
  );

  const updateMutation = useMutation({
    mutationFn: (r: KRecord) => api.updateRecord(KTYPE, r.id, r.data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
  });

  return (
    <section className="flex gap-4">
      <div className="flex-[0_0_300px]">
        <h1>Price Lists</h1>
        {q.isLoading && <p>Loading…</p>}
        <ul className="list-none p-0 text-[13px]">
          {(q.data ?? []).map((r) => {
            const d = r.data as unknown as PriceListData;
            const isSel = selectedId === r.id;
            return (
              <li
                key={r.id}
                onClick={() => setSelectedId(r.id)}
                className={`cursor-pointer rounded px-2 py-1.5 ${
                  isSel ? "bg-bg-muted" : "bg-transparent"
                }`}
              >
                <div className="font-medium">{d.name ?? "(unnamed)"}</div>
                <div className="text-xs text-fg-muted">
                  {d.currency ?? "—"} · {d.customer_id ?? "all customers"}
                </div>
              </li>
            );
          })}
        </ul>
      </div>
      <div className="flex-1">
        {selected ? (
          <PriceListEditor
            key={selected.id}
            record={selected}
            onSave={(r) => updateMutation.mutate(r)}
            saving={updateMutation.isPending}
          />
        ) : (
          <p className="text-fg-muted">
            Select a price list to edit its item matrix.
          </p>
        )}
      </div>
    </section>
  );
}

function PriceListEditor({
  record,
  onSave,
  saving,
}: {
  record: KRecord;
  onSave: (r: KRecord) => void;
  saving: boolean;
}) {
  const initial = record.data as unknown as PriceListData;
  const [items, setItems] = useState<PriceListItem[]>(initial.items ?? []);

  const updateRow = (i: number, patch: Partial<PriceListItem>) => {
    setItems((prev) => prev.map((row, idx) => (idx === i ? { ...row, ...patch } : row)));
  };
  const addRow = () =>
    setItems((prev) => [...prev, { item_id: "", price: 0 }]);
  const removeRow = (i: number) =>
    setItems((prev) => prev.filter((_, idx) => idx !== i));

  const save = () => {
    onSave({
      ...record,
      data: { ...record.data, items },
    });
  };

  return (
    <div>
      <h2>{initial.name}</h2>
      <div className="text-[13px] text-fg-muted">
        {initial.currency ?? "—"} · valid {initial.valid_from ?? "—"} to {initial.valid_until ?? "—"}
      </div>

      <Table className="mt-3 text-[13px]">
        <TableHeader>
          <TableRow>
            <TableHead>Item</TableHead>
            <TableHead>Price</TableHead>
            <TableHead>Discount %</TableHead>
            <TableHead>Min qty</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((r, i) => (
            <TableRow key={i}>
              <TableCell>
                <Input
                  value={r.item_id}
                  onChange={(e) => updateRow(i, { item_id: e.target.value })}
                />
              </TableCell>
              <TableCell>
                <Input
                  type="number"
                  value={String(r.price)}
                  onChange={(e) => updateRow(i, { price: Number(e.target.value) })}
                />
              </TableCell>
              <TableCell>
                <Input
                  type="number"
                  value={String(r.discount_percent ?? 0)}
                  onChange={(e) => updateRow(i, { discount_percent: Number(e.target.value) })}
                />
              </TableCell>
              <TableCell>
                <Input
                  type="number"
                  value={String(r.min_qty ?? 0)}
                  onChange={(e) => updateRow(i, { min_qty: Number(e.target.value) })}
                />
              </TableCell>
              <TableCell>
                <Button size="sm" variant="outline" onClick={() => removeRow(i)}>
                  Remove
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <div className="mt-2 flex gap-2">
        <Button variant="outline" onClick={addRow}>
          Add row
        </Button>
        <Button onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save"}
        </Button>
      </div>
    </div>
  );
}
