import { describe, expect, it } from "vitest";
import type { FieldSpec } from "@kapp/client";
import type { Formatters } from "./i18n";
import {
  formatValue,
  isMoneyField,
  isStatusField,
  resolveControl,
  schemaHasCurrency,
} from "./ktypeView";

const field = (partial: Partial<FieldSpec> & { name: string }): FieldSpec => ({
  type: "string",
  ...partial,
});

// A real en-US Intl-backed formatter so `currency` throws a RangeError
// on a non-ISO-4217 code exactly as the production `useFormatter` does.
const enFormatters: Formatters = {
  number: (n, opts) => new Intl.NumberFormat("en-US", opts).format(n),
  currency: (amount, currencyCode, opts) =>
    new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: currencyCode,
      ...opts,
    }).format(amount),
  date: (d, opts) => new Intl.DateTimeFormat("en-US", opts).format(d),
  dateTime: (d, opts) => new Intl.DateTimeFormat("en-US", opts).format(d),
  time: (d, opts) => new Intl.DateTimeFormat("en-US", opts).format(d),
  relativeTime: (value, unit, opts) =>
    new Intl.RelativeTimeFormat("en-US", { numeric: "auto", ...opts }).format(
      value,
      unit,
    ),
};

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

describe("isStatusField", () => {
  it("treats an enum type as a status field regardless of letter case", () => {
    // resolveControl lowercases `type`, so isStatusField must too — a
    // schema using `Enum`/`ENUM` (without a values array) still badges.
    expect(isStatusField(field({ name: "phase", type: "Enum" }))).toBe(true);
    expect(isStatusField(field({ name: "phase", type: "ENUM" }))).toBe(true);
  });

  it("matches by values array or a known status field name", () => {
    expect(
      isStatusField(field({ name: "whatever", values: ["a", "b"] })),
    ).toBe(true);
    expect(isStatusField(field({ name: "status" }))).toBe(true);
    expect(isStatusField(field({ name: "title" }))).toBe(false);
  });
});

describe("formatValue", () => {
  it("never throws on a non-ISO currency code — falls back to a plain number", () => {
    const amount = field({ name: "amount", type: "number" });
    expect(() =>
      formatValue(amount, 1234.5, { data: { currency: "dollars" } }, enFormatters),
    ).not.toThrow();
    // The bad code must not leak; a clean grouped number is shown instead.
    expect(
      formatValue(amount, 1234.5, { data: { currency: "dollars" } }, enFormatters),
    ).toBe("1,234.5");
  });

  it("formats a valid currency code with its symbol", () => {
    const amount = field({ name: "amount", type: "number" });
    expect(
      formatValue(amount, 1234.5, { data: { currency: "USD" } }, enFormatters),
    ).toBe("$1,234.50");
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
