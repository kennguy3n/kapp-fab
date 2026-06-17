import type { DocumentTotals, LineItem } from "./types";

/** Round to 2 decimal places, nudging by EPSILON so values like
 *  1.005 round up rather than down due to binary float drift. */
export function round2(n: number): number {
  return Math.round((n + Number.EPSILON) * 100) / 100;
}

/** Gross amount for a line before its discount (qty × unit price). */
export function lineGross(line: LineItem): number {
  const qty = Number.isFinite(line.qty) ? line.qty : 0;
  const price = Number.isFinite(line.unitPrice) ? line.unitPrice : 0;
  return round2(qty * price);
}

/** Net amount for a line: gross minus the per-line discount, floored
 *  at zero so a discount can never make a line negative. */
export function lineNet(line: LineItem): number {
  const discount = Number.isFinite(line.discount) ? line.discount : 0;
  const net = lineGross(line) - discount;
  return round2(net > 0 ? net : 0);
}

/**
 * Compute document totals from the normalised lines, mirroring the
 * backend posters:
 *   subtotal       = Σ (qty × unit_price)            (gross)
 *   discount_total = Σ line.discount
 *   tax_amount     = (subtotal − discount_total) × taxRate%
 *   total          = subtotal − discount_total + tax_amount
 * Documents without tax (requisitions) pass taxRate 0, so total
 * collapses to subtotal − discount_total.
 */
export function computeTotals(
  lines: LineItem[],
  opts: { taxRate?: number } = {},
): DocumentTotals {
  const taxRate = Number.isFinite(opts.taxRate) ? (opts.taxRate as number) : 0;
  let subtotal = 0;
  let discountTotal = 0;
  for (const line of lines) {
    subtotal += lineGross(line);
    discountTotal += Number.isFinite(line.discount) ? line.discount : 0;
  }
  subtotal = round2(subtotal);
  discountTotal = round2(discountTotal);
  const taxable = subtotal - discountTotal;
  const taxAmount = round2((taxable * taxRate) / 100);
  const total = round2(taxable + taxAmount);
  return { subtotal, discountTotal, taxAmount, total };
}
