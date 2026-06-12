import { describe, it, expect } from "vitest";
import { makeKRecord } from "../test/factories";
import { detectTransferPairs } from "./reconciliation";

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
