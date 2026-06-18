import { describe, it, expect } from "vitest";
import { looksLikeId, titleCase } from "./format";

describe("looksLikeId", () => {
  it("detects UUIDs and ktype:slug identifiers", () => {
    expect(looksLikeId("3f2504e0-4f89-41d3-9a0c-0305e82c3301")).toBe(true);
    expect(looksLikeId("crm.organization:globex")).toBe(true);
  });

  it("treats human strings as non-ids", () => {
    expect(looksLikeId("Acme Corporation")).toBe(false);
    expect(looksLikeId("SO-1024")).toBe(false);
  });
});

describe("titleCase", () => {
  it("humanises enum tokens", () => {
    expect(titleCase("in_progress")).toBe("In Progress");
    expect(titleCase("draft")).toBe("Draft");
    expect(titleCase("purchase order")).toBe("Purchase Order");
  });
});
