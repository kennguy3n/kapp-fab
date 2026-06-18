import { describe, it, expect } from "vitest";
import { csvFilename, parseAmount, toCsv, todayLocalISO } from "./format";

describe("toCsv formula-injection guard", () => {
  it("keeps negative amounts numeric so spreadsheet math still works", () => {
    const csv = toCsv(["Account", "Balance"], [["6020", "-120000.00"]]);
    // The amount must NOT be prefixed with a quote (which would make it text).
    expect(csv).toContain("-120000.00");
    expect(csv).not.toContain("'-120000.00");
  });

  it("leaves positive decimals untouched", () => {
    const csv = toCsv(["Balance"], [["120000.00"]]);
    expect(csv).toBe("Balance\r\n120000.00");
  });

  it("neutralises genuine formula vectors", () => {
    const csv = toCsv(
      ["Note"],
      [["=SUM(A1:A9)"], ["+1+2"], ["@cmd"]],
    );
    expect(csv).toContain("'=SUM(A1:A9)");
    expect(csv).toContain("'+1+2");
    expect(csv).toContain("'@cmd");
  });

  it("quotes cells containing commas, quotes, or newlines", () => {
    const csv = toCsv(["Name"], [['Acme, "The" Co.']]);
    expect(csv).toBe('Name\r\n"Acme, ""The"" Co."');
  });
});

describe("csvFilename", () => {
  it("uses the provided stamp verbatim", () => {
    expect(csvFilename("trial-balance", "2026-06-17")).toBe(
      "trial-balance_2026-06-17.csv",
    );
  });

  it("defaults to the local calendar date", () => {
    expect(csvFilename("export")).toBe(`export_${todayLocalISO()}.csv`);
  });
});

describe("todayLocalISO", () => {
  it("formats the local calendar day as YYYY-MM-DD", () => {
    expect(todayLocalISO()).toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });
});

describe("parseAmount", () => {
  it("returns NaN for blank-ish input and a number otherwise", () => {
    expect(Number.isNaN(parseAmount(null))).toBe(true);
    expect(Number.isNaN(parseAmount(""))).toBe(true);
    expect(parseAmount("-120000.00")).toBe(-120000);
    expect(parseAmount(42)).toBe(42);
  });
});
