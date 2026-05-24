/**
 * Translation bundle loader and resolver — the frontend mirror of
 * `internal/i18n/bundle.go`.
 *
 * Catalogues are loaded lazily via Vite's import.meta.glob with
 * `import: "default"` so each `<tag>.json` becomes its own Rollup
 * chunk. The default-locale catalogue ("en") is eager-loaded
 * because every `t()` call may need it as a fallback target; the
 * other catalogues stream in on demand the first time a user
 * picks them. The eager load adds ~1 KB gzipped to the initial
 * bundle (the en.json keyset is small and grows linearly with
 * baseline-key count, not locale count), and trades that for a
 * three-stage fallback that never has to await a promise.
 *
 * Three-stage fallback for t(locale, key):
 *
 *   1. Active locale catalogue (when loaded).
 *   2. DefaultLocale catalogue ("en", always available).
 *   3. The literal key itself.
 *
 * Stage 3 (literal key) is deliberately loud-but-safe: a missing
 * translation renders as e.g. `common.save` rather than blocking
 * the page or throwing. The CI parity test (PR-8) enforces the
 * keyset is identical across all catalogues so stage 3 only
 * fires when a developer added a new key without updating the
 * baseline.
 *
 * The loader is intentionally async at the bundle layer but sync
 * at the `t()` call site (via the LocaleContext caching the
 * currently-loaded catalogue). React Suspense is not used here —
 * a switch to an unloaded locale shows English until the new
 * catalogue's promise resolves, which is faster than a Suspense
 * boundary and keeps the rest of the page interactive during the
 * transition. The frontend doesn't have to handle the
 * loading-spinner-on-locale-switch UX because the bundles are
 * small (< 4 KB each, gzipped) and ship from the same origin.
 */

import enCatalogue from "../../locales/en.json";
import { DefaultLocale } from "./locales";

/** A message catalogue is a flat tag→string dictionary. */
export type MessageCatalogue = Readonly<Record<string, string>>;

/**
 * Vite's glob registry of all *.json files in src/locales EXCEPT
 * the English baseline, which is statically imported above and
 * pre-loaded into the cache below.
 *
 * `eager: false` means each catalogue becomes its own dynamic
 * import; `import: "default"` strips the module wrapper so the
 * loaded value is the parsed JSON object itself rather than
 * `{ default: ... }`.
 *
 * The negative-pattern entry (`!../../locales/en.json`) deliberately
 * excludes the English catalogue from the glob: without it, Vite
 * would emit a separate lazy chunk for `en.json` that is never
 * downloaded at runtime (because `loadCatalogue("en")` hits the
 * pre-populated cache immediately). The exclusion removes the
 * dead chunk from the production build output. Vite also emits a
 * dynamic-vs-static-import build warning when the same module is
 * both eagerly and lazily imported; excluding `en.json` from the
 * glob clears that warning at the same time.
 *
 * The registered keys look like "../../locales/de.json"; we slice
 * out the basename in `loadCatalogue` to map a locale tag to the
 * right loader.
 */
const catalogueLoaders = import.meta.glob<MessageCatalogue>(
  ["../../locales/*.json", "!../../locales/en.json"],
  { import: "default" },
);

/**
 * Eager-loaded English catalogue. The default-locale fallback is
 * the floor of the three-stage chain, so making it sync removes
 * the only async edge case in t() (a `t()` call before the user-
 * picked catalogue finishes loading would otherwise have to
 * return the literal key, which is loud-but-correct but worse UX
 * than the English baseline).
 */
const defaultCatalogue: MessageCatalogue = enCatalogue as MessageCatalogue;

/** In-memory cache of loaded catalogues keyed by locale tag. */
const cache = new Map<string, MessageCatalogue>([
  [DefaultLocale, defaultCatalogue],
]);

/**
 * Loads the catalogue for `tag` and returns it. Repeated calls
 * for the same tag are cheap — the first call resolves the dynamic
 * import and stashes it in cache; subsequent calls hit the cache
 * synchronously through `getCachedCatalogue`.
 *
 * Unknown tags fall back to the DefaultLocale catalogue. The
 * resolver doesn't throw on missing files because the locale
 * registry already gates user-facing picks against the shipped
 * catalogue set — the only way to reach this path with an unknown
 * tag is a programming error, and the loud-but-safe response is
 * to serve English rather than crash the app.
 */
export async function loadCatalogue(tag: string): Promise<MessageCatalogue> {
  if (cache.has(tag)) {
    return cache.get(tag) as MessageCatalogue;
  }
  const key = `../../locales/${tag}.json`;
  const loader = catalogueLoaders[key];
  if (!loader) {
    // Unknown tag — surface the English catalogue. Logging the
    // miss with console.warn so a dev hitting this path during
    // local work sees the typo without the production user
    // seeing a broken page.
    if (typeof console !== "undefined") {
      console.warn(`[i18n] no catalogue shipped for locale "${tag}"; using "${DefaultLocale}"`);
    }
    return defaultCatalogue;
  }
  const cat = (await loader()) as MessageCatalogue;
  cache.set(tag, cat);
  return cat;
}

/**
 * Returns the loaded catalogue for `tag` from the cache, or
 * undefined if it hasn't been loaded yet. Used by `t()` for its
 * sync fast path; the calling context (the LocaleProvider) is
 * responsible for ensuring the active locale's catalogue is
 * loaded before rendering with it.
 */
export function getCachedCatalogue(tag: string): MessageCatalogue | undefined {
  return cache.get(tag);
}

/**
 * Three-stage translation: active catalogue → DefaultLocale →
 * literal key. The signature is sync so React rendering doesn't
 * have to await on every text node — the LocaleProvider's
 * useEffect handles the async load and re-renders once the
 * catalogue is in cache.
 *
 * Returns the literal key when both catalogues miss the key.
 * This makes a typoed `t("common.svae")` render as `common.svae`
 * in the UI, which a code reviewer or QA pass spots immediately
 * (a hard-coded English string wouldn't).
 */
export function translate(active: MessageCatalogue | undefined, key: string): string {
  if (active) {
    const v = active[key];
    if (typeof v === "string" && v.length > 0) {
      return v;
    }
  }
  const fallback = defaultCatalogue[key];
  if (typeof fallback === "string" && fallback.length > 0) {
    return fallback;
  }
  return key;
}

/**
 * Simple {placeholder} interpolation for parameterised messages.
 * The substring matcher is `{name}` style (Mustache-like without
 * the tag escaping — these strings are rendered as text by React,
 * which handles HTML escaping at the DOM layer).
 *
 * Missing parameters leave the placeholder intact so a missing
 * value is visible to the developer rather than silently empty.
 */
export function interpolate(template: string, params?: Record<string, string | number>): string {
  if (!params) {
    return template;
  }
  return template.replace(/\{(\w+)\}/g, (m, name) => {
    if (Object.prototype.hasOwnProperty.call(params, name)) {
      return String(params[name]);
    }
    return m;
  });
}
