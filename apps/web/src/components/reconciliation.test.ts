import { describe, it, expect } from "vitest";
import type { ExchangeRate } from "@kapp/client";
import { makeKRecord } from "../test/factories";
import {
  buildRateMap,
  convertToBase,
  detectAnomalies,
  detectTransferPairs,
  isBalanced,
  isForeignLine,
  matchesQuery,
  splitRemaining,
  txnCurrency,
} from "./reconciliation";

function rate(
  from: string,
  to: string,
  value: string,
  rate_date = "2024-02-01",
): ExchangeRate {
  return {
    tenant_id: "t",
    from_currency: from,
    to_currency: to,
    rate_date,
    rate: value,
    created_at: "",
    updated_at: "",
  };
}

function line(data: Record<string, unknown>, id = "x") {
  return makeKRecord({ id, ktype: "finance.bank_transaction", data });
}

function transfer(
  id: string,
  account: string,
  amount: number,
  value_date: string,
  currency = "USD",
) {
  return makeKRecord({
    id,
    ktype: "finance.bank_transaction",
    data: {
      bank_account_id: account,
      value_date,
      description: amount < 0 ? "Transfer out" : "Transfer in",
      amount,
      currency,
      status: "transfer",
    },
  });
}

const accountName = (id: string) => id;

describe("detectTransferPairs", () => {
  it("pairs an equal-and-opposite leg into a single row", () => {
    const rows = detectTransferPairs(
      [
        transfer("out", "acct-1", -500, "2024-02-03"),
        transfer("in", "acct-2", 500, "2024-02-03"),
      ],
      accountName,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ amount: 500, currency: "USD" });
    expect(rows[0].out?.txn.id).toBe("out");
    expect(rows[0].in?.txn.id).toBe("in");
  });

  it("prefers the closest-dated credit when several share the amount", () => {
    const rows = detectTransferPairs(
      [
        transfer("out", "acct-1", -500, "2024-02-10"),
        transfer("far", "acct-2", 500, "2024-02-01"),
        transfer("near", "acct-2", 500, "2024-02-09"),
      ],
      accountName,
    );
    const paired = rows.find((r) => r.out?.txn.id === "out");
    expect(paired?.in?.txn.id).toBe("near");
    // The far leg is left unpaired and surfaced on its own.
    const lone = rows.find((r) => r.in?.txn.id === "far" && !r.out);
    expect(lone).toBeDefined();
  });

  it("does not cross legs of different currencies", () => {
    const rows = detectTransferPairs(
      [
        transfer("out", "acct-1", -500, "2024-02-03", "USD"),
        transfer("in", "acct-2", 500, "2024-02-03", "EUR"),
      ],
      accountName,
    );
    expect(rows).toHaveLength(2);
    expect(rows.every((r) => !(r.out && r.in))).toBe(true);
  });

  it("surfaces an unpaired leg on its own rather than hiding it", () => {
    const rows = detectTransferPairs(
      [transfer("out", "acct-1", -500, "2024-02-03")],
      accountName,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0].out?.txn.id).toBe("out");
    expect(rows[0].in).toBeUndefined();
  });

  it("surfaces a zero-amount transfer leg instead of dropping it", () => {
    const rows = detectTransferPairs(
      [transfer("zero", "acct-1", 0, "2024-02-03")],
      accountName,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ amount: 0 });
    expect(rows[0].out?.txn.id).toBe("zero");
  });
});

describe("isForeignLine", () => {
  it("is true only when both currencies are present and differ", () => {
    expect(isForeignLine("EUR", "USD")).toBe(true);
    expect(isForeignLine("usd", "USD")).toBe(false); // case-insensitive
    expect(isForeignLine(" USD ", "USD")).toBe(false); // trimmed
    expect(isForeignLine(undefined, "USD")).toBe(false); // unknown → not foreign
    expect(isForeignLine("EUR", undefined)).toBe(false);
    expect(isForeignLine("", "USD")).toBe(false);
  });
});

describe("txnCurrency", () => {
  it("reads the line currency from record data", () => {
    expect(txnCurrency(line({ currency: "GBP" }))).toBe("GBP");
    expect(txnCurrency(line({}))).toBeUndefined();
  });
});

describe("buildRateMap + convertToBase", () => {
  it("converts using a direct rate", () => {
    const map = buildRateMap([rate("EUR", "USD", "1.08")]);
    const conv = convertToBase(100, "EUR", "USD", map);
    expect(conv).not.toBeNull();
    expect(conv?.base).toBeCloseTo(108, 6);
    expect(conv?.rate).toBeCloseTo(1.08, 6);
  });

  it("falls back to the inverse rate when only the reverse pair exists", () => {
    const map = buildRateMap([rate("USD", "EUR", "0.92")]);
    const conv = convertToBase(92, "EUR", "USD", map);
    expect(conv).not.toBeNull();
    expect(conv?.base).toBeCloseTo(100, 4);
  });

  it("treats a same-currency line as a 1:1 conversion", () => {
    const conv = convertToBase(50, "USD", "USD", buildRateMap([]));
    expect(conv).toEqual({ base: 50, rate: 1 });
  });

  it("returns null when no rate is published", () => {
    expect(convertToBase(100, "JPY", "USD", buildRateMap([]))).toBeNull();
  });

  it("returns null when the base currency is unknown", () => {
    const map = buildRateMap([rate("EUR", "USD", "1.08")]);
    expect(convertToBase(100, "EUR", undefined, map)).toBeNull();
  });

  it("keeps only the latest-dated rate for a currency pair", () => {
    const map = buildRateMap([
      rate("EUR", "USD", "1.05", "2024-01-01"),
      rate("EUR", "USD", "1.20", "2024-03-01"),
    ]);
    expect(convertToBase(100, "EUR", "USD", map)?.base).toBeCloseTo(120, 6);
  });

  it("ignores non-positive or non-numeric rates", () => {
    const map = buildRateMap([
      rate("EUR", "USD", "0"),
      rate("GBP", "USD", "not-a-number"),
    ]);
    expect(convertToBase(100, "EUR", "USD", map)).toBeNull();
    expect(convertToBase(100, "GBP", "USD", map)).toBeNull();
  });
});

describe("matchesQuery", () => {
  const r = line({
    description: "ACME Corp invoice",
    amount: 1250.5,
    value_date: "2024-02-14",
    currency: "USD",
  });

  it("matches an empty query (no filter)", () => {
    expect(matchesQuery(r, "")).toBe(true);
    expect(matchesQuery(r, "   ")).toBe(true);
  });

  it("matches case-insensitively across description, amount and date", () => {
    expect(matchesQuery(r, "acme")).toBe(true);
    expect(matchesQuery(r, "1250.5")).toBe(true);
    expect(matchesQuery(r, "2024-02")).toBe(true);
    expect(matchesQuery(r, "usd")).toBe(true);
  });

  it("does not match unrelated text", () => {
    expect(matchesQuery(r, "globex")).toBe(false);
  });
});

describe("splitRemaining + isBalanced", () => {
  it("computes the remaining difference after allocations", () => {
    expect(splitRemaining(100, [60, 40])).toBeCloseTo(0, 6);
    expect(splitRemaining(100, [60])).toBeCloseTo(40, 6);
    expect(splitRemaining(100, [])).toBe(100);
  });

  it("ignores non-finite allocations rather than producing NaN", () => {
    expect(splitRemaining(100, [60, Number.NaN])).toBeCloseTo(40, 6);
  });

  it("treats a sub-half-cent remainder as balanced", () => {
    expect(isBalanced(0.004)).toBe(true);
    expect(isBalanced(-0.004)).toBe(true);
    expect(isBalanced(0.01)).toBe(false);
  });
});

describe("detectAnomalies", () => {
  it("flags exact duplicate lines", () => {
    const a = line(
      { value_date: "2024-02-01", description: "Stripe payout", amount: 500, currency: "USD" },
      "dup-a",
    );
    const b = line(
      { value_date: "2024-02-01", description: "Stripe payout", amount: 500, currency: "USD" },
      "dup-b",
    );
    const c = line(
      { value_date: "2024-02-02", description: "Other", amount: 9, currency: "USD" },
      "solo",
    );
    const { duplicateIds, reversalIds } = detectAnomalies([a, b, c]);
    expect(duplicateIds.has("dup-a")).toBe(true);
    expect(duplicateIds.has("dup-b")).toBe(true);
    expect(duplicateIds.has("solo")).toBe(false);
    expect(reversalIds.size).toBe(0);
  });

  it("flags equal-and-opposite reversals", () => {
    const charge = line(
      { value_date: "2024-02-01", description: "Refund pair", amount: 120, currency: "USD" },
      "pos",
    );
    const refund = line(
      { value_date: "2024-02-05", description: "Refund pair", amount: -120, currency: "USD" },
      "neg",
    );
    const { reversalIds } = detectAnomalies([charge, refund]);
    expect(reversalIds.has("pos")).toBe(true);
    expect(reversalIds.has("neg")).toBe(true);
  });

  it("does not flag a lone charge as a reversal", () => {
    const { reversalIds } = detectAnomalies([
      line({ value_date: "2024-02-01", description: "x", amount: 10, currency: "USD" }, "only"),
    ]);
    expect(reversalIds.size).toBe(0);
  });

  it("returns empty sets for an empty account", () => {
    const { duplicateIds, reversalIds } = detectAnomalies([]);
    expect(duplicateIds.size).toBe(0);
    expect(reversalIds.size).toBe(0);
  });
});
