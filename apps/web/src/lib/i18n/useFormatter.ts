/**
 * useFormatter — locale-aware Intl wrappers for numbers, dates,
 * times, currency, and relative time. Hooks rather than free
 * functions so each formatter is memoised against the active
 * locale; React Intl performance work has shown that constructing
 * an Intl.NumberFormat / Intl.DateTimeFormat instance is ~20-50µs
 * each, and a busy table column that renders 100+ values per
 * frame benefits significantly from instance reuse.
 *
 * The formatters live in this dedicated module so a page can
 * import `useFormatter` without pulling in the whole translation
 * runtime — code-split bundles for read-only views (charts,
 * dashboards) can save ~3-4 KB by skipping the message-bundle
 * import.
 *
 * Locale precedence: every formatter resolves to the active
 * LocaleContext tag. The native Intl runtime then handles the
 * BCP 47 → CLDR data lookup, so e.g. requesting "fr-CA" currency
 * formatting falls back to "fr" automatically (Canadian French
 * shares almost all the relevant rules with European French; the
 * only divergence is the digit grouping and the currency placement,
 * both handled by the CLDR data). Modern browsers ship CLDR data
 * for all 13 catalogue locales; the formatters degrade gracefully
 * to the browser default on a locale the runtime lacks data for.
 */

import { useMemo } from "react";
import { useLocaleContext } from "./context";

export interface Formatters {
  /**
   * Formats a plain number using the active locale's grouping +
   * decimal conventions. Accepts the standard NumberFormat
   * options for callers that need precision overrides (e.g.
   * payroll figures rendered with two decimal places).
   */
  number: (n: number, opts?: Intl.NumberFormatOptions) => string;

  /**
   * Formats a monetary amount in the supplied ISO 4217 currency
   * code. Currency placement, grouping, and decimal rendering
   * follow the active locale's CLDR rules: 1234.5 USD renders
   * as "$1,234.50" in en, "1.234,50 $" in de, "1 234,50 $US" in
   * fr-FR, "￥1,235" in ja (yen is integer-only).
   */
  currency: (amount: number, currencyCode: string, opts?: Intl.NumberFormatOptions) => string;

  /**
   * Formats a Date as a calendar date (no time component) using
   * the active locale's preferred ordering and month-name style.
   * Defaults to the "medium" date style; callers needing a
   * specific style pass the standard DateTimeFormatOptions.
   */
  date: (d: Date | number, opts?: Intl.DateTimeFormatOptions) => string;

  /**
   * Formats a Date as a date-and-time combination using the
   * active locale's preferred ordering. Defaults to the "short"
   * date style + "short" time style so the result fits in a
   * table cell or notification line.
   */
  dateTime: (d: Date | number, opts?: Intl.DateTimeFormatOptions) => string;

  /**
   * Formats a Date as a time-of-day (no date component). Honours
   * the locale's 12h/24h convention by default; callers needing
   * a fixed convention pass `hour12: true | false` explicitly.
   */
  time: (d: Date | number, opts?: Intl.DateTimeFormatOptions) => string;

  /**
   * Formats a duration relative to "now" — e.g. -3 days renders
   * as "3 days ago" (en) / "vor 3 Tagen" (de) / "il y a 3 jours"
   * (fr). The `unit` parameter accepts the standard Intl
   * RelativeTimeFormat units.
   */
  relativeTime: (value: number, unit: Intl.RelativeTimeFormatUnit, opts?: Intl.RelativeTimeFormatOptions) => string;
}

export function useFormatter(): Formatters {
  const { locale } = useLocaleContext();

  return useMemo<Formatters>(() => {
    const numberFmt = new Intl.NumberFormat(locale);
    const dateFmt = new Intl.DateTimeFormat(locale, { dateStyle: "medium" });
    const dateTimeFmt = new Intl.DateTimeFormat(locale, {
      dateStyle: "short",
      timeStyle: "short",
    });
    const timeFmt = new Intl.DateTimeFormat(locale, { timeStyle: "short" });
    const relativeTimeFmt = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });

    // Currency formatters need both a locale and a currency code,
    // so we cache one instance per (currency, options) pair to
    // avoid re-construction on every render. The cache key is
    // built from the currency + a JSON serialisation of the
    // options so callers passing equal-valued options reuse the
    // same Intl instance.
    const currencyCache = new Map<string, Intl.NumberFormat>();
    const currency = (amount: number, currencyCode: string, opts?: Intl.NumberFormatOptions) => {
      const cacheKey = `${currencyCode}::${opts ? JSON.stringify(opts) : ""}`;
      let fmt = currencyCache.get(cacheKey);
      if (!fmt) {
        fmt = new Intl.NumberFormat(locale, {
          style: "currency",
          currency: currencyCode,
          ...opts,
        });
        currencyCache.set(cacheKey, fmt);
      }
      return fmt.format(amount);
    };

    return {
      number: (n, opts) => (opts ? new Intl.NumberFormat(locale, opts).format(n) : numberFmt.format(n)),
      currency,
      date: (d, opts) => (opts ? new Intl.DateTimeFormat(locale, opts).format(d) : dateFmt.format(d)),
      dateTime: (d, opts) => (opts ? new Intl.DateTimeFormat(locale, opts).format(d) : dateTimeFmt.format(d)),
      time: (d, opts) => (opts ? new Intl.DateTimeFormat(locale, opts).format(d) : timeFmt.format(d)),
      relativeTime: (value, unit, opts) =>
        opts
          ? new Intl.RelativeTimeFormat(locale, { numeric: "auto", ...opts }).format(value, unit)
          : relativeTimeFmt.format(value, unit),
    };
  }, [locale]);
}
