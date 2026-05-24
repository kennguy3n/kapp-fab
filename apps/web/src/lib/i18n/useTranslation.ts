/**
 * useTranslation — the canonical hook React components consume to
 * render translated strings. The hook is a thin wrapper around
 * useLocaleContext that returns the (locale, t) pair components
 * need most often, plus the setLocale escape hatch for the locale
 * switcher.
 *
 * Usage:
 *
 *   const { t, locale, setLocale } = useTranslation();
 *   return <Button>{t("common.save")}</Button>;
 *
 * The `t(key, params?)` signature matches the Go-side T() so a
 * key referenced on both sides resolves identically — useful when
 * a frontend page renders the same label the backend writes into
 * an event-stream notification.
 */

import { useLocaleContext, type LocaleContextValue } from "./context";

export function useTranslation(): LocaleContextValue {
  return useLocaleContext();
}
