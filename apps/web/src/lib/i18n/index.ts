/**
 * Public surface for the frontend i18n module.
 *
 * Components should import from `@/lib/i18n` (or the relative
 * path equivalent) — direct imports from the sub-modules are
 * supported but discouraged because the public surface here is
 * stable while the internal split between bundle / context /
 * locales / hooks may evolve.
 */

export {
  DefaultLocale,
  SupportedLocales,
  bestSupportedLocale,
  bestSupportedLocaleForCountry,
  defaultLocaleForCountry,
  isSupportedLocale,
  localeInfo,
  type LocaleDirection,
  type LocaleInfo,
} from "./locales";

export { LocaleProvider, useLocaleContext } from "./context";
export { useTranslation } from "./useTranslation";
export { useFormatter, type Formatters } from "./useFormatter";

// Lower-level utilities, exported for tests and edge-case
// callers that want to translate a key outside React (e.g.
// inside a service-worker or a Web Worker). Most consumers use
// `useTranslation().t` instead.
export { translate, interpolate, loadCatalogue, getCachedCatalogue } from "./bundle";
export type { MessageCatalogue } from "./bundle";
