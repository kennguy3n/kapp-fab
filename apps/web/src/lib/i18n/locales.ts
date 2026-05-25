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
  { tag: "fr-CA", name: "Français (Canada)", direction: "ltr" },
  { tag: "it", name: "Italiano", direction: "ltr" },
  { tag: "es", name: "Español", direction: "ltr" },
  { tag: "pt-BR", name: "Português (Brasil)", direction: "ltr" },
  { tag: "ja", name: "日本語", direction: "ltr" },
  { tag: "zh", name: "中文（简体）", direction: "ltr" },
  { tag: "zh-Hant", name: "中文（繁體）", direction: "ltr" },
  { tag: "ar", name: "العربية", direction: "rtl" },
  { tag: "ms", name: "Bahasa Melayu", direction: "ltr" },
  { tag: "th", name: "ไทย", direction: "ltr" },
  { tag: "id", name: "Bahasa Indonesia", direction: "ltr" },
  { tag: "vi", name: "Tiếng Việt", direction: "ltr" },
  // SCAFFOLD: cmd/new-tax-pack inserts new SupportedLocales entries above this line.
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

/**
 * Region-to-script overrides for ambiguous primary subtags. The
 * shipped Chinese catalogues are `zh` (Simplified Chinese) and
 * `zh-Hant` (Traditional Chinese), but `navigator.language` on
 * Taiwanese / Hong Kong / Macau browsers reports `zh-TW` / `zh-HK` /
 * `zh-MO` without an explicit script subtag. A naive
 * `split("-")[0]` reduces those to `zh`, which is Simplified — the
 * wrong catalogue for those regions. This table mirrors the
 * golang.org/x/text/language.Matcher resolution the backend bundle
 * does (pinned by `internal/i18n/bundle_test.go:129-130`).
 *
 * The table is intentionally narrow: only regions whose default
 * script is unambiguous AND differs from the bare-primary-subtag
 * catalogue. Simplified-Chinese regions (CN / SG / MY) resolve via
 * the regular progressive-subtag fallback to `zh`, so they don't
 * need an entry here.
 */
const REGION_SCRIPT_OVERRIDES: Record<string, string> = {
  "zh-TW": "zh-Hant",
  "zh-HK": "zh-Hant",
  "zh-MO": "zh-Hant",
};

/**
 * Country-to-locale mapping — the frontend mirror of
 * `tenant.DefaultLocaleForCountry` in `internal/tenant/wizard.go`. The
 * wizard uses this to pre-select a sensible UI language when the user
 * picks a country in step 0, before they've explicitly chosen a
 * locale from the dropdown. The backend uses its own copy of this
 * mapping when callers omit `cfg.Locale` so a direct API caller that
 * only supplies `Country` still lands on a country-appropriate UI
 * language.
 *
 * Both copies of the table must stay in lockstep. The values follow
 * the same convention as the backend:
 *
 *   - DE / AT / CH         → de  (German-speaking bloc)
 *   - FR                   → fr
 *   - IT                   → it
 *   - ES                   → es
 *   - JP                   → ja
 *   - SA / AE / QA / KW / BH / OM → ar  (Arabic-speaking GCC bloc)
 *   - TH / ID / VN         → th / id / vi (single-language jurisdictions)
 *   - IN                   → hi   (Hindi; downgrades to en until hi.json ships)
 *   - CN                   → zh-Hans (downgrades to zh)
 *   - TW / HK              → zh-Hant
 *   - SG / MY / PH         → en  (B2B lingua franca; the ms / tl catalogues
 *                                 exist for ms but English is the conservative
 *                                 business default)
 *   - NZ / US / AU / GB / IE / CA / everything else → en
 *
 * Unlike `bestSupportedLocale`, this helper does NOT downgrade tags
 * to the shipped catalogue set — it returns the canonical locale tag
 * for the country, even if that tag has no shipped catalogue (e.g.
 * `"hi"` for IN, `"zh-Hans"` for CN). Callers that need a tag the
 * frontend can actually serve should pipe the return value through
 * `bestSupportedLocale()`, which is what `bestSupportedLocaleForCountry`
 * below does for the wizard's pre-select path.
 */
const COUNTRY_LOCALE_DEFAULTS: Record<string, string> = {
  DE: "de",
  AT: "de",
  CH: "de",
  FR: "fr",
  IT: "it",
  ES: "es",
  JP: "ja",
  SA: "ar",
  AE: "ar",
  QA: "ar",
  KW: "ar",
  BH: "ar",
  OM: "ar",
  TH: "th",
  ID: "id",
  VN: "vi",
  IN: "hi",
  CN: "zh-Hans",
  TW: "zh-Hant",
  HK: "zh-Hant",
  // PR-2d: Americas. Brazil ships its own pt-BR catalogue
  // (Brazilian Portuguese differs from European Portuguese in
  // accounting / payroll terminology). Spanish-speaking LATAM
  // jurisdictions share the existing es.json catalogue.
  // Canada and Trinidad & Tobago default to English; Québec /
  // Acadia admins reset to fr-CA from the locale switcher.
  BR: "pt-BR",
  MX: "es",
  AR: "es",
  CO: "es",
  CL: "es",
  PE: "es",
  CR: "es",
  PA: "es",
  UY: "es",
  EC: "es",
  DO: "es",
  GT: "es",
  PY: "es",
  // SCAFFOLD: cmd/new-tax-pack inserts new COUNTRY_LOCALE_DEFAULTS entries above this line.
};

/**
 * Returns the canonical UI locale tag for the supplied ISO 3166-1
 * alpha-2 country code, mirroring `tenant.DefaultLocaleForCountry`.
 * Returns "en" for unmapped countries.
 *
 * The returned tag is the country's canonical locale (e.g. "hi" for
 * IN, "zh-Hans" for CN) — it is NOT downgraded to the shipped
 * catalogue set. The wizard pipes this through `bestSupportedLocale`
 * to find the catalogue the frontend can actually serve, but the
 * unfiltered value is what the backend persists to `tenants.locale`
 * (where the database CHECK gates format, not catalogue presence).
 */
export function defaultLocaleForCountry(country: string): string {
  if (!country) {
    return DefaultLocale;
  }
  const code = country.trim().toUpperCase();
  return COUNTRY_LOCALE_DEFAULTS[code] ?? DefaultLocale;
}

/**
 * Returns the best UI locale the frontend can actually render for the
 * supplied country code — `defaultLocaleForCountry()` piped through
 * `bestSupportedLocale()`. Useful for the wizard's pre-select path
 * where the goal is to show the user the catalogue that will actually
 * render, not the canonical-but-unshipped tag the backend stores.
 *
 * Examples:
 *   IN → "hi" canonical → null after bestSupportedLocale → "en"
 *   CN → "zh-Hans" canonical → "zh" after progressive-subtag drop
 *   TW → "zh-Hant" canonical → "zh-Hant" (exact match)
 *   DE → "de" canonical → "de" (exact match)
 */
export function bestSupportedLocaleForCountry(country: string): string {
  const canonical = defaultLocaleForCountry(country);
  return bestSupportedLocale(canonical) ?? DefaultLocale;
}

/**
 * Returns the best supported locale tag for an arbitrary BCP 47
 * input — the frontend mirror of `golang.org/x/text/language.Matcher`
 * over the shipped catalogue set. Resolution order:
 *
 *   1. Exact match against SupportedLocales.
 *   2. Region-to-script override (e.g. `zh-TW` → `zh-Hant`).
 *   3. Progressive-subtag fallback: drop trailing subtags one at a
 *      time and re-check (e.g. `zh-Hant-TW` → `zh-Hant`,
 *      `de-AT` → `de`). This is what makes a browser reporting
 *      `de-AT` correctly resolve to the German catalogue.
 *   4. Returns `null` when nothing matches; callers decide whether
 *      that means DefaultLocale or rejection.
 *
 * Tag comparison is case-sensitive against the canonical BCP 47
 * casing the SupportedLocales table uses (lowercase primary,
 * Titlecase script, UPPERCASE region). Callers that may receive
 * a tag in non-canonical casing (e.g. an Accept-Language header
 * with `ZH-HANT-tw`) should normalise before calling this, but the
 * common-case inputs from `navigator.language` and the
 * `kapp_locale` cookie are already in canonical form because both
 * are written by code that respects BCP 47 casing.
 */
export function bestSupportedLocale(tag: string): string | null {
  if (!tag) {
    return null;
  }
  if (isSupportedLocale(tag)) {
    return tag;
  }
  if (Object.prototype.hasOwnProperty.call(REGION_SCRIPT_OVERRIDES, tag)) {
    const mapped = REGION_SCRIPT_OVERRIDES[tag];
    if (isSupportedLocale(mapped)) {
      return mapped;
    }
  }
  const subtags = tag.split("-");
  while (subtags.length > 1) {
    subtags.pop();
    const shorter = subtags.join("-");
    if (isSupportedLocale(shorter)) {
      return shorter;
    }
    if (Object.prototype.hasOwnProperty.call(REGION_SCRIPT_OVERRIDES, shorter)) {
      const mapped = REGION_SCRIPT_OVERRIDES[shorter];
      if (isSupportedLocale(mapped)) {
        return mapped;
      }
    }
  }
  return null;
}
