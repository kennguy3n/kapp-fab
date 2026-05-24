/**
 * Locale registry — the single source of truth for which locale
 * tags the frontend ships catalogues for and how each one is
 * presented in the locale switcher UI.
 *
 * Stays in lockstep with `internal/i18n/locales/*.json` on the
 * backend; PR-8 (security + maintenance) adds a CI check that
 * diffs the two directories so a translator can never ship a
 * frontend catalogue the backend doesn't know about (or vice
 * versa). Adding a new locale therefore means:
 *
 *   1. Drop `<tag>.json` into `internal/i18n/locales/` AND
 *      `apps/web/src/locales/` with the same keyset.
 *   2. Add an entry below.
 *   3. The Go test `TestEveryLocaleShipsBaselineKeys` enforces
 *      that the keyset matches en.json across every catalogue,
 *      and the PR-8 parity test enforces that the same files
 *      exist on both sides.
 *
 * `name` is the locale's display string in its own language
 * ("Deutsch", "日本語") — the locale switcher renders this so a
 * speaker of the target language can find their option without
 * relying on the current UI language. `direction` drives the
 * Tailwind `dir` attribute on <html> for RTL locales; PR-6 wires
 * the layout flip logic against this signal.
 *
 * The DefaultLocale ("en") is required to be present and is the
 * fallback target whenever a requested catalogue is missing or
 * empty. The loader at bundle.ts:loadBundle treats "en" as
 * always-available and short-circuits the fallback chain on it.
 */

export type LocaleDirection = "ltr" | "rtl";

export interface LocaleInfo {
  /** BCP 47 tag, exact match against the catalogue filename. */
  readonly tag: string;
  /** Display string in the locale's own language. */
  readonly name: string;
  /** Direction signal used by the layout for RTL flipping. */
  readonly direction: LocaleDirection;
}

export const DefaultLocale = "en" as const;

export const SupportedLocales: readonly LocaleInfo[] = [
  { tag: "en", name: "English", direction: "ltr" },
  { tag: "de", name: "Deutsch", direction: "ltr" },
  { tag: "fr", name: "Français", direction: "ltr" },
  { tag: "it", name: "Italiano", direction: "ltr" },
  { tag: "es", name: "Español", direction: "ltr" },
  { tag: "ja", name: "日本語", direction: "ltr" },
  { tag: "zh", name: "中文（简体）", direction: "ltr" },
  { tag: "zh-Hant", name: "中文（繁體）", direction: "ltr" },
  { tag: "ar", name: "العربية", direction: "rtl" },
  { tag: "ms", name: "Bahasa Melayu", direction: "ltr" },
  { tag: "th", name: "ไทย", direction: "ltr" },
  { tag: "id", name: "Bahasa Indonesia", direction: "ltr" },
  { tag: "vi", name: "Tiếng Việt", direction: "ltr" },
];

/**
 * Lookup of `tag → LocaleInfo` for the resolver path. Built once
 * at module load — supporting 13 locales today, scaling to a few
 * dozen, an object index out-performs Array.find().
 */
const localeIndex: Record<string, LocaleInfo> = SupportedLocales.reduce(
  (acc, info) => {
    acc[info.tag] = info;
    return acc;
  },
  {} as Record<string, LocaleInfo>,
);

/**
 * Returns true if the supplied tag matches a shipped catalogue
 * exactly (case-sensitive). The strict-match contract mirrors the
 * Go-side IsSupported so the frontend and backend agree on which
 * tag values are legitimate user picks vs. resolver downgrades.
 */
export function isSupportedLocale(tag: string): boolean {
  return Object.prototype.hasOwnProperty.call(localeIndex, tag);
}

/**
 * Returns the LocaleInfo for the supplied tag, or the DefaultLocale's
 * info when the tag isn't shipped. Used by the locale switcher to
 * render the active selection's display name without throwing on a
 * stale localStorage value.
 */
export function localeInfo(tag: string): LocaleInfo {
  return localeIndex[tag] ?? localeIndex[DefaultLocale];
}
