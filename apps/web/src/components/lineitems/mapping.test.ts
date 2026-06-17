import { describe, it, expect } from "vitest";
import { DOCUMENT_CONFIGS } from "./configs";
import {
  buildDocumentData,
  buildLines,
  deriveTaxRate,
  lineCount,
  linesFromData,
  priceKey,
} from "./mapping";
import type { LineItem } from "./types";

function line(overrides: Partial<LineItem> = {}): LineItem {
  return { itemId: "i1", description: "", uom: "", qty: 1, unitPrice: 0, discount: 0, ...overrides };
}

describe("priceKey", () => {
  it("uses estimated_unit_price for requisitions and unit_price otherwise", () => {
    expect(priceKey("purchase_requisition")).toBe("estimated_unit_price");
    expect(priceKey("sales_order")).toBe("unit_price");
    expect(priceKey("purchase_order")).toBe("unit_price");
  });
});

describe("linesFromData", () => {
  it("normalises stored sales-order lines", () => {
    const lines = linesFromData("sales_order", {
      lines: [{ item_id: "i1", qty: 2, unit_price: "10", discount: 1 }],
    });
    expect(lines).toEqual([
      { itemId: "i1", description: "", uom: "", qty: 2, unitPrice: 10, discount: 1 },
    ]);
  });

  it("reads requisition estimated_unit_price into unitPrice", () => {
    const lines = linesFromData("purchase_requisition", {
      lines: [{ item_id: "i9", qty: 4, estimated_unit_price: 7.5, uom: "box", description: "Bolts" }],
    });
    expect(lines[0]).toMatchObject({ itemId: "i9", qty: 4, unitPrice: 7.5, uom: "box", description: "Bolts" });
  });

  it("returns an empty array when no lines are stored", () => {
    expect(linesFromData("sales_order", {})).toEqual([]);
  });
});

describe("lineCount", () => {
  it("counts stored lines without normalising them, and is safe when absent", () => {
    expect(lineCount({ lines: [{ item_id: "a" }, { item_id: "b" }] })).toBe(2);
    expect(lineCount({})).toBe(0);
    expect(lineCount({ lines: "oops" })).toBe(0);
  });
});

describe("buildLines", () => {
  it("emits only the columns a sales order uses (discount, no uom/description)", () => {
    const out = buildLines(DOCUMENT_CONFIGS.sales_order, [
      line({ qty: 2, unitPrice: 10, discount: 1, description: "skip", uom: "skip" }),
    ]);
    expect(out[0]).toEqual({ item_id: "i1", qty: 2, unit_price: 10, line_total: 19, discount: 1 });
  });

  it("clamps a persisted discount to the line gross and omits a zero discount", () => {
    const clamped = buildLines(DOCUMENT_CONFIGS.sales_order, [
      line({ qty: 1, unitPrice: 10, discount: 999 }),
    ]);
    expect(clamped[0]).toEqual({ item_id: "i1", qty: 1, unit_price: 10, line_total: 0, discount: 10 });

    const noDiscount = buildLines(DOCUMENT_CONFIGS.sales_order, [
      line({ qty: 1, unitPrice: 10, discount: 0 }),
    ]);
    expect(noDiscount[0]).not.toHaveProperty("discount");
  });

  it("emits description/uom for purchase orders and uses estimated price for requisitions", () => {
    const po = buildLines(DOCUMENT_CONFIGS.purchase_order, [
      line({ qty: 3, unitPrice: 5, description: "Widget", uom: "ea" }),
    ]);
    expect(po[0]).toEqual({ item_id: "i1", qty: 3, unit_price: 5, line_total: 15, description: "Widget", uom: "ea" });
    expect(po[0]).not.toHaveProperty("discount");

    const req = buildLines(DOCUMENT_CONFIGS.purchase_requisition, [line({ qty: 2, unitPrice: 4 })]);
    expect(req[0]).toMatchObject({ estimated_unit_price: 4 });
    expect(req[0]).not.toHaveProperty("unit_price");
  });
});

describe("deriveTaxRate", () => {
  it("back-computes the effective tax percentage", () => {
    expect(deriveTaxRate({ subtotal: 100, discount_total: 0, tax_amount: 10 })).toBe(10);
    expect(deriveTaxRate({ subtotal: 200, discount_total: 0, tax_amount: 0 })).toBe(0);
    expect(deriveTaxRate({ subtotal: 0, discount_total: 0, tax_amount: 5 })).toBe(0);
  });
});

describe("buildDocumentData", () => {
  it("includes discount_total, tax_amount and total for a sales order and drops blank header fields", () => {
    const data = buildDocumentData(
      DOCUMENT_CONFIGS.sales_order,
      { customer_id: "c1", order_date: "2024-01-01", order_number: "" },
      [line({ qty: 2, unitPrice: 10, discount: 2 })],
      "USD",
      10,
    );
    expect(data).toMatchObject({
      currency: "USD",
      customer_id: "c1",
      order_date: "2024-01-01",
      subtotal: 20,
      discount_total: 2,
      tax_amount: 1.8,
      total: 19.8,
    });
    expect(data).not.toHaveProperty("order_number");
  });

  it("emits cleared optional header fields as null on edit so the merge overwrites them", () => {
    const data = buildDocumentData(
      DOCUMENT_CONFIGS.sales_order,
      { customer_id: "c1", order_date: "2024-01-01", order_number: "" },
      [line({ qty: 1, unitPrice: 10 })],
      "USD",
      0,
      "edit",
    );
    expect(data).toMatchObject({ customer_id: "c1", order_date: "2024-01-01", order_number: null });
  });

  it("persists subtotal only for a requisition (no tax/discount/total)", () => {
    const data = buildDocumentData(
      DOCUMENT_CONFIGS.purchase_requisition,
      { requested_by: "Jane", request_date: "2024-01-01" },
      [line({ qty: 2, unitPrice: 5 })],
      "USD",
      0,
    );
    expect(data).toMatchObject({ currency: "USD", requested_by: "Jane", subtotal: 10 });
    expect(data).not.toHaveProperty("tax_amount");
    expect(data).not.toHaveProperty("discount_total");
    expect(data).not.toHaveProperty("total");
  });
});
