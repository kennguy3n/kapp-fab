import { describe, expect, it } from "vitest";
import type { FieldSpec } from "@kapp/client";
import {
  isMoneyField,
  resolveControl,
  schemaHasCurrency,
} from "./ktypeView";

const field = (partial: Partial<FieldSpec> & { name: string }): FieldSpec => ({
  type: "string",
  ...partial,
});

describe("resolveControl", () => {
  it("maps types and name heuristics to the right control", () => {
    expect(resolveControl(field({ name: "active", type: "boolean" }))).toBe(
      "boolean",
    );
    expect(
      resolveControl(field({ name: "stage", type: "enum", values: ["a"] })),
    ).toBe("select");
    expect(resolveControl(field({ name: "owner", ref: "crm.user" }))).toBe(
      "relation",
    );
    expect(resolveControl(field({ name: "due", type: "date" }))).toBe("date");
    expect(resolveControl(field({ name: "at", type: "datetime" }))).toBe(
      "datetime",
    );
    expect(resolveControl(field({ name: "notes", type: "text" }))).toBe(
      "textarea",
    );
    expect(resolveControl(field({ name: "email" }))).toBe("email");
    expect(resolveControl(field({ name: "work_email" }))).toBe("email");
    expect(resolveControl(field({ name: "phone" }))).toBe("tel");
    expect(resolveControl(field({ name: "website" }))).toBe("url");
    expect(resolveControl(field({ name: "qty", type: "number" }))).toBe(
      "number",
    );
    expect(resolveControl(field({ name: "name" }))).toBe("text");
  });

  it("keeps a specialised control even when max_length is generous", () => {
    // A long max_length must not override an email/url/phone field with
    // a textarea: the typed/name-based control wins first.
    expect(
      resolveControl(field({ name: "email", max_length: 255 })),
    ).toBe("email");
    expect(
      resolveControl(field({ name: "website", max_length: 2048 })),
    ).toBe("url");
    // A long plain string still falls back to a textarea.
    expect(
      resolveControl(field({ name: "summary", max_length: 500 })),
    ).toBe("textarea");
    // A numeric field keeps its number input even with a generous
    // max_length — the textarea heuristic only applies to string fields.
    expect(
      resolveControl(field({ name: "score", type: "number", max_length: 200 })),
    ).toBe("number");
  });
});

describe("isMoneyField", () => {
  it("matches money types and curated name tokens", () => {
    expect(isMoneyField(field({ name: "balance", type: "decimal" }))).toBe(
      true,
    );
    expect(isMoneyField(field({ name: "unit_price", type: "decimal" }))).toBe(
      true,
    );
    expect(isMoneyField(field({ name: "total", type: "number" }))).toBe(true);
    expect(isMoneyField(field({ name: "fee", type: "money" }))).toBe(true);
  });

  it("does not match ambiguous or unrelated names (no substring false positives)", () => {
    // `evaluation` contains "value" as a substring but is not a money
    // token; ambiguous bare words were deliberately excluded.
    expect(isMoneyField(field({ name: "evaluation", type: "number" }))).toBe(
      false,
    );
    expect(isMoneyField(field({ name: "due_date", type: "date" }))).toBe(false);
    expect(isMoneyField(field({ name: "rate", type: "number" }))).toBe(false);
    expect(isMoneyField(field({ name: "quantity", type: "number" }))).toBe(
      false,
    );
  });
});

describe("schemaHasCurrency", () => {
  it("is true when the schema has a currency field or a money-typed field", () => {
    expect(
      schemaHasCurrency([
        field({ name: "value", type: "number" }),
        field({ name: "currency", type: "string" }),
      ]),
    ).toBe(true);
    expect(
      schemaHasCurrency([field({ name: "total", type: "money" })]),
    ).toBe(true);
  });

  it("is false for a schema with only plain numeric fields", () => {
    expect(
      schemaHasCurrency([
        field({ name: "value", type: "number" }),
        field({ name: "qty", type: "integer" }),
      ]),
    ).toBe(false);
  });
});
