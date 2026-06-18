import { describe, it, expect } from "vitest";
import { computeTotals, lineDiscount, lineGross, lineNet, round2 } from "./compute";
import type { LineItem } from "./types";

function line(overrides: Partial<LineItem> = {}): LineItem {
  return { itemId: "i", description: "", uom: "", qty: 1, unitPrice: 0, discount: 0, ...overrides };
}

describe("round2", () => {
  it("rounds to two decimal places", () => {
    expect(round2(10.004)).toBe(10);
    expect(round2(10.006)).toBe(10.01);
    expect(round2(0.1 + 0.2)).toBe(0.3);
  });
});

describe("lineGross / lineNet", () => {
  it("multiplies qty by unit price", () => {
    expect(lineGross(line({ qty: 3, unitPrice: 4 }))).toBe(12);
  });

  it("subtracts the discount but never goes negative", () => {
    expect(lineNet(line({ qty: 2, unitPrice: 10, discount: 5 }))).toBe(15);
    expect(lineNet(line({ qty: 1, unitPrice: 10, discount: 999 }))).toBe(0);
  });

  it("treats non-finite qty/price as zero", () => {
    expect(lineGross(line({ qty: Number.NaN, unitPrice: 10 }))).toBe(0);
    expect(lineNet(line({ qty: 1, unitPrice: Number.NaN }))).toBe(0);
  });
});

describe("lineDiscount", () => {
  it("clamps the discount to the line gross and floors negatives at zero", () => {
    expect(lineDiscount(line({ qty: 2, unitPrice: 10, discount: 5 }))).toBe(5);
    expect(lineDiscount(line({ qty: 1, unitPrice: 10, discount: 999 }))).toBe(10);
    expect(lineDiscount(line({ qty: 1, unitPrice: 10, discount: -4 }))).toBe(0);
  });
});

describe("computeTotals", () => {
  it("sums gross subtotal with no tax or discount", () => {
    const totals = computeTotals([
      line({ qty: 2, unitPrice: 10 }),
      line({ qty: 1, unitPrice: 5 }),
    ]);
    expect(totals).toEqual({ subtotal: 25, discountTotal: 0, taxAmount: 0, total: 25 });
  });

  it("subtracts discount and applies tax to the discounted base", () => {
    const totals = computeTotals(
      [line({ qty: 2, unitPrice: 10, discount: 4 })],
      { taxRate: 10 },
    );
    // subtotal 20, discount 4, taxable 16, tax 1.6, total 17.6
    expect(totals).toEqual({ subtotal: 20, discountTotal: 4, taxAmount: 1.6, total: 17.6 });
  });

  it("collapses to subtotal when taxRate is omitted", () => {
    const totals = computeTotals([line({ qty: 1, unitPrice: 30, discount: 5 })]);
    expect(totals.total).toBe(25);
    expect(totals.taxAmount).toBe(0);
  });

  it("never goes negative when a discount exceeds the line gross", () => {
    const totals = computeTotals(
      [line({ qty: 1, unitPrice: 10, discount: 999 })],
      { taxRate: 10 },
    );
    // discount clamps to the line gross (10), so taxable, tax and
    // total all collapse to zero rather than going negative.
    expect(totals).toEqual({ subtotal: 10, discountTotal: 10, taxAmount: 0, total: 0 });
  });
});
