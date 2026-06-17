import { computeTotals, lineNet } from "./compute";
import type { DocumentConfig, DocumentKind, LineItem } from "./types";

// Raw line shape as stored on the record's JSONB `data.lines`. Each
// document family uses a subset of these keys (see internal/sales).
interface RawLine {
  item_id?: unknown;
  description?: unknown;
  uom?: unknown;
  qty?: unknown;
  unit_price?: unknown;
  estimated_unit_price?: unknown;
  discount?: unknown;
  line_total?: unknown;
}

/** The data key a document family stores its per-unit price under. */
export function priceKey(kind: DocumentKind): "unit_price" | "estimated_unit_price" {
  return kind === "purchase_requisition" ? "estimated_unit_price" : "unit_price";
}

function num(value: unknown): number {
  const n = typeof value === "string" ? Number(value) : (value as number);
  return Number.isFinite(n) ? n : 0;
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** Read a record's stored lines into the normalised editor model. */
export function linesFromData(kind: DocumentKind, data: Record<string, unknown>): LineItem[] {
  const raw = Array.isArray(data.lines) ? (data.lines as RawLine[]) : [];
  const key = priceKey(kind);
  return raw.map((r) => ({
    itemId: str(r.item_id),
    description: str(r.description),
    uom: str(r.uom),
    qty: num(r.qty),
    unitPrice: num(r[key] ?? r.unit_price ?? r.estimated_unit_price),
    discount: num(r.discount),
  }));
}

/** Serialise normalised lines back to the KType-specific raw shape,
 *  emitting only the columns the document family uses. */
export function buildLines(config: DocumentConfig, lines: LineItem[]): Record<string, unknown>[] {
  const key = priceKey(config.kind);
  return lines.map((l) => {
    const out: Record<string, unknown> = {
      item_id: l.itemId,
      qty: l.qty,
      [key]: l.unitPrice,
      line_total: lineNet(l),
    };
    if (config.columns.description && l.description) out.description = l.description;
    if (config.columns.uom && l.uom) out.uom = l.uom;
    if (config.columns.discount && l.discount) out.discount = l.discount;
    return out;
  });
}

/** Back-compute the effective tax rate (%) of a stored record so the
 *  dialog can re-populate the Tax % control when editing. */
export function deriveTaxRate(data: Record<string, unknown>): number {
  const subtotal = num(data.subtotal);
  const discountTotal = num(data.discount_total);
  const taxAmount = num(data.tax_amount);
  const base = subtotal - discountTotal;
  if (base <= 0 || taxAmount <= 0) return 0;
  return Math.round((taxAmount / base) * 10000) / 100;
}

/**
 * Build the full `data` payload for a create/update call: the header
 * fields (empty values dropped so optional refs aren't sent blank),
 * the serialised lines, the currency, and the computed totals. Tax
 * and discount/total fields are emitted only where the schema has
 * them (requisitions persist subtotal only).
 */
export function buildDocumentData(
  config: DocumentConfig,
  header: Record<string, string>,
  lines: LineItem[],
  currency: string,
  taxRate: number,
): Record<string, unknown> {
  const totals = computeTotals(lines, { taxRate: config.taxable ? taxRate : 0 });
  const data: Record<string, unknown> = { currency };
  for (const [k, v] of Object.entries(header)) {
    if (v !== "") data[k] = v;
  }
  data.lines = buildLines(config, lines);
  data.subtotal = totals.subtotal;
  if (config.kind === "sales_order") data.discount_total = totals.discountTotal;
  if (config.taxable) data.tax_amount = totals.taxAmount;
  if (config.kind !== "purchase_requisition") data.total = totals.total;
  return data;
}
