import type { InventoryWarehouse, KRecord } from "@kapp/client";
import type { ItemOption, RecordOption } from "./types";

// Glue that turns the app's KRecords / inventory rows into the
// picker option shapes the editor consumes. Kept out of the editor
// components themselves so those stay free of data-shape assumptions.

function dataStr(r: KRecord, key: string): string {
  const v = r.data[key];
  return typeof v === "string" ? v : "";
}

function dataNum(r: KRecord, key: string): number | undefined {
  const v = r.data[key];
  const n = typeof v === "string" ? Number(v) : (v as number);
  return Number.isFinite(n) ? n : undefined;
}

export function orgOptions(records: KRecord[]): RecordOption[] {
  return records.map((r) => ({ value: r.id, label: dataStr(r, "name") || r.id }));
}

export function invoiceOptions(records: KRecord[]): RecordOption[] {
  return records.map((r) => ({
    value: r.id,
    label: dataStr(r, "invoice_number") || r.id,
  }));
}

export function warehouseOptions(warehouses: InventoryWarehouse[]): RecordOption[] {
  return warehouses.map((w) => ({ value: w.id, label: w.name, hint: w.code }));
}

export function itemOptions(records: KRecord[]): ItemOption[] {
  return records.map((r) => {
    const sku = dataStr(r, "sku");
    const price = dataNum(r, "default_price");
    const uom = dataStr(r, "uom");
    return {
      value: r.id,
      label: dataStr(r, "name") || r.id,
      ...(sku ? { hint: sku } : {}),
      ...(price !== undefined ? { price } : {}),
      ...(uom ? { uom } : {}),
    };
  });
}

/** Build an id→label resolver from a list of records, used to render
 *  reference fields (customer, supplier…) as names instead of UUIDs. */
export function buildNameResolver(
  records: KRecord[] | undefined,
  key = "name",
): (id: string | undefined | null) => string {
  const map = new Map((records ?? []).map((r) => [r.id, dataStr(r, key)]));
  return (id) => (id ? map.get(id) ?? "" : "");
}
