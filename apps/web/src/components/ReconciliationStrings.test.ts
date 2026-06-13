import { describe, it, expect } from "vitest";
import { RECONCILIATION_STRINGS, rt, rtp } from "./ReconciliationStrings";

describe("ReconciliationStrings", () => {
  it("resolves a known key to its copy", () => {
    expect(rt("reconciliation.retry")).toBe("Retry");
  });

  it("interpolates named placeholders", () => {
    const msg = rtp("reconciliation.fx.mismatchWarning", {
      lineCurrency: "EUR",
      baseCurrency: "USD",
    });
    expect(msg).toContain("EUR");
    expect(msg).toContain("USD");
    expect(msg).not.toContain("{lineCurrency}");
    expect(msg).not.toContain("{baseCurrency}");
  });

  it("leaves an unprovided placeholder untouched rather than printing 'undefined'", () => {
    // bulk.undone has {count} and {plural}; omit plural on purpose.
    const msg = rtp("reconciliation.bulk.undone", { count: 3 });
    expect(msg).toContain("3");
    expect(msg).toContain("{plural}");
    expect(msg).not.toContain("undefined");
  });

  it("namespaces every key under reconciliation.*", () => {
    for (const key of Object.keys(RECONCILIATION_STRINGS)) {
      expect(key.startsWith("reconciliation.")).toBe(true);
    }
  });
});
