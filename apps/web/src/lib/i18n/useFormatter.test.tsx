import { type ReactNode } from "react";
import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { LocaleProvider } from "./context";
import { useFormatter } from "./useFormatter";

// useFormatter wraps Intl with locale-aware number / currency / date
// helpers used by every money-bearing screen (payroll, invoices,
// ledgers) across all tenant jurisdictions. ICU emits locale-specific
// separators and currency placement; assertions normalise the various
// space code-points Intl inserts (regular / NBSP / narrow-NBSP) so the
// tests pin the meaningful formatting rules without being brittle to
// the host ICU build's spacing choices.

const STORAGE_KEY = "kapp_locale";

function wrapperFor(locale: string) {
  // LocaleProvider resolves its initial locale from localStorage on
  // mount, so seeding the key pins the active locale for the hook.
  window.localStorage.setItem(STORAGE_KEY, locale);
  return function Wrapper({ children }: { children: ReactNode }) {
    return <LocaleProvider>{children}</LocaleProvider>;
  };
}

function format(
  locale: string,
  fn: (f: ReturnType<typeof useFormatter>) => string,
): string {
  const { result } = renderHook(() => useFormatter(), {
    wrapper: wrapperFor(locale),
  });
  return fn(result.current);
}

/** Collapse every Unicode space variant Intl may emit to a plain " ". */
const norm = (s: string) => s.replace(/\s/g, " ").trim();

afterEach(() => {
  window.localStorage.clear();
});

describe("useFormatter.number", () => {
  it("uses the locale's grouping separator", () => {
    expect(format("en", (f) => f.number(1234567))).toBe("1,234,567");
    expect(format("de", (f) => f.number(1234567))).toBe("1.234.567");
  });

  it("honours per-call NumberFormat options (e.g. fixed decimals)", () => {
    expect(
      format("en", (f) =>
        f.number(3.5, { minimumFractionDigits: 2, maximumFractionDigits: 2 }),
      ),
    ).toBe("3.50");
  });
});

describe("useFormatter.currency", () => {
  it("places the symbol and separators per the en locale", () => {
    expect(norm(format("en", (f) => f.currency(1234.5, "USD")))).toBe(
      "$1,234.50",
    );
  });

  it("places the symbol and separators per the de locale", () => {
    const out = norm(format("de", (f) => f.currency(1234.5, "EUR")));
    expect(out).toBe("1.234,50 €");
  });

  it("renders integer-only currencies (JPY) without a fraction", () => {
    const out = format("ja", (f) => f.currency(1234.5, "JPY"));
    // Yen has 0 fraction digits; the rounded integer part must appear
    // and there must be no decimal separator before two trailing digits.
    expect(out).toContain("1,235");
    expect(out).not.toMatch(/\.\d{2}$/);
  });

  it("respects an explicit fraction-digit override", () => {
    const out = norm(
      format("en", (f) =>
        f.currency(2, "USD", { minimumFractionDigits: 0, maximumFractionDigits: 0 }),
      ),
    );
    expect(out).toBe("$2");
  });
});

describe("useFormatter.date / dateTime / time", () => {
  // A fixed instant: 2024-03-09T13:05:00Z. Assert via a parallel Intl
  // instance so the test pins "matches the locale's medium date style"
  // rather than a hard-coded string that drifts with ICU data.
  const d = new Date("2024-03-09T13:05:00Z");

  it("formats a medium-style date for the active locale", () => {
    const expected = new Intl.DateTimeFormat("en", { dateStyle: "medium" }).format(d);
    expect(format("en", (f) => f.date(d))).toBe(expected);
  });

  it("formats date differently for a different locale", () => {
    const en = format("en", (f) => f.date(d));
    const de = format("de", (f) => f.date(d));
    expect(de).not.toBe(en);
    expect(de).toBe(
      new Intl.DateTimeFormat("de", { dateStyle: "medium" }).format(d),
    );
  });

  it("formats a relative time using the locale's words", () => {
    expect(format("en", (f) => f.relativeTime(-3, "day"))).toBe("3 days ago");
  });
});
