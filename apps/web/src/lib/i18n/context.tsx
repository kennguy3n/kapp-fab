/**
 * LocaleContext / LocaleProvider — React-side wiring around the
 * bundle loader and resolver.
 *
 * The provider owns three pieces of state:
 *
 *   1. `locale` — the BCP 47 tag the user (or browser) picked.
 *   2. `catalogue` — the loaded message dictionary for that tag.
 *      Drops back to the English catalogue while the new pick's
 *      dynamic import is in flight.
 *   3. `direction` — "ltr" or "rtl", driven by the locale entry
 *      in locales.ts. Stamped onto `document.documentElement.dir`
 *      so Tailwind's `rtl:` variants (PR-6) and the layout's
 *      logical properties pick up the flip automatically.
 *
 * Initial-resolution precedence (highest first):
 *
 *   1. `localStorage["kapp_locale"]` — last user pick. Persists
 *      across browser sessions on the same device.
 *   2. `document.cookie["kapp_locale"]` — set by the backend
 *      middleware or by an admin overriding via the API. Mirrors
 *      the server-side cookie precedence so the frontend and
 *      backend agree on which catalogue to serve.
 *   3. `navigator.language` — best-effort browser hint, mapped to
 *      a shipped catalogue via the locale registry's strict
 *      whitelist (a navigator value of "de-AT" downgrades to "de"
 *      because de.json ships and de-AT.json doesn't).
 *   4. DefaultLocale ("en").
 *
 * The provider also exposes `setLocale(tag)` to the rest of the
 * app. Calling it writes the new tag to both localStorage and the
 * cookie so a refresh or a server round-trip picks up the same
 * choice. This is the entry point the LocaleSwitcher binds to.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  DefaultLocale,
  LocaleDirection,
  SupportedLocales,
  isSupportedLocale,
  localeInfo,
} from "./locales";
import {
  type MessageCatalogue,
  getCachedCatalogue,
  interpolate,
  loadCatalogue,
  translate,
} from "./bundle";

/** Public shape exposed via useTranslation / useLocaleContext. */
export interface LocaleContextValue {
  /** Current BCP 47 tag. Always a member of SupportedLocales. */
  readonly locale: string;
  /** Layout direction for the current locale. */
  readonly direction: LocaleDirection;
  /**
   * Translates `key` using the active catalogue with three-stage
   * fallback (active → English → literal key). `params` provides
   * `{placeholder}` interpolation for parameterised messages.
   */
  readonly t: (key: string, params?: Record<string, string | number>) => string;
  /**
   * Switches the active locale. Writes the new tag to both
   * localStorage and the kapp_locale cookie so a refresh and the
   * backend middleware pick up the choice. Silently ignored for
   * tags that aren't a shipped catalogue.
   */
  readonly setLocale: (tag: string) => void;
}

const Context = createContext<LocaleContextValue | undefined>(undefined);

/** localStorage key holding the user's last locale pick. */
const STORAGE_KEY = "kapp_locale";
/** Cookie name mirroring the backend's kapp_locale source. */
const COOKIE_NAME = "kapp_locale";

/**
 * Reads the locale from localStorage, the kapp_locale cookie, or
 * navigator.language — whichever wins per the precedence comment
 * above. Returns DefaultLocale when nothing yields a supported
 * tag.
 *
 * The cookie parse is intentionally permissive (does not
 * URL-decode, does not unquote) because the backend writes a
 * plain tag value and a URL-decoded variant of a plain BCP 47
 * tag is identical to the original. The locale set is a small
 * fixed whitelist so a malformed cookie value just fails the
 * isSupportedLocale check and falls through.
 */
function resolveInitialLocale(): string {
  if (typeof window === "undefined") {
    return DefaultLocale;
  }
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    if (stored && isSupportedLocale(stored)) {
      return stored;
    }
  } catch {
    // localStorage access can throw in private-browsing iframes
    // and some embedded contexts; the cookie / navigator path
    // below covers those callers without crashing.
  }
  const cookieMatch = (typeof document !== "undefined" ? document.cookie : "")
    .split(";")
    .map((p) => p.trim())
    .find((p) => p.startsWith(`${COOKIE_NAME}=`));
  if (cookieMatch) {
    const value = cookieMatch.slice(COOKIE_NAME.length + 1);
    if (isSupportedLocale(value)) {
      return value;
    }
  }
  if (typeof navigator !== "undefined" && typeof navigator.language === "string") {
    const nav = navigator.language;
    if (isSupportedLocale(nav)) {
      return nav;
    }
    // Try the primary subtag (e.g. "de-AT" → "de").
    const primary = nav.split("-")[0];
    if (primary && isSupportedLocale(primary)) {
      return primary;
    }
  }
  return DefaultLocale;
}

/**
 * Writes the locale to the kapp_locale cookie. Mirrors the path,
 * SameSite, and lifetime of the backend-set cookie so the two
 * stay in sync: `Path=/; Max-Age=31536000; SameSite=Lax`. Not
 * marked HttpOnly because the frontend reads it back on boot.
 *
 * `secure` is added when the page is served over HTTPS so
 * production deployments don't leak the cookie over plain HTTP.
 * Dev (localhost over HTTP) needs the cookie too — those origins
 * are flagged "secure context" by the browser regardless of the
 * scheme, but we use the `location.protocol` check anyway because
 * the Secure attribute on an http://localhost cookie is silently
 * dropped by some browsers.
 */
function writeCookie(tag: string) {
  if (typeof document === "undefined") {
    return;
  }
  const secure = typeof window !== "undefined" && window.location.protocol === "https:" ? "; Secure" : "";
  document.cookie = `${COOKIE_NAME}=${encodeURIComponent(tag)}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
}

/**
 * Stamps the current locale onto `document.documentElement` so
 * the browser and CSS know the page's language and direction:
 *
 *   - `lang` drives screen-reader pronunciation, search-engine
 *     language detection, and form input behaviour (e.g. the
 *     virtual keyboard hint on mobile).
 *   - `dir` drives Tailwind's `rtl:` variant compilation and the
 *     CSS logical-property flip (margin-inline-start, etc.). PR-6
 *     audits the layout against this signal.
 *
 * Both attributes are stamped on every locale change because the
 * direction may flip (en→ar) and the lang must always match the
 * locale being rendered for accessibility.
 */
function syncDocumentRoot(tag: string, direction: LocaleDirection) {
  if (typeof document === "undefined") {
    return;
  }
  document.documentElement.lang = tag;
  document.documentElement.dir = direction;
}

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<string>(() => resolveInitialLocale());
  const [catalogue, setCatalogue] = useState<MessageCatalogue | undefined>(() =>
    getCachedCatalogue(locale),
  );
  const direction = useMemo(() => localeInfo(locale).direction, [locale]);

  // Eagerly load (and cache) the active catalogue whenever the
  // locale changes. The default-locale catalogue is already in
  // cache (loaded synchronously at module load), so the very
  // first render for an "en" tenant is fully synchronous; non-
  // default locales render with the English fallback for the
  // microseconds the dynamic import takes, then re-render with
  // the resolved catalogue.
  useEffect(() => {
    let cancelled = false;
    loadCatalogue(locale).then((cat) => {
      if (!cancelled) {
        setCatalogue(cat);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [locale]);

  // Keep <html lang> and <html dir> in sync with the active
  // locale on every change. Stamping in an effect (rather than at
  // setLocale time) means SSR-style first paint also gets the
  // right attributes — the provider's useState initializer runs
  // before any markup is committed to the DOM, and the effect
  // fires immediately after mount.
  useEffect(() => {
    syncDocumentRoot(locale, direction);
  }, [locale, direction]);

  const t = useCallback(
    (key: string, params?: Record<string, string | number>) => {
      const template = translate(catalogue, key);
      return params ? interpolate(template, params) : template;
    },
    [catalogue],
  );

  const setLocale = useCallback((tag: string) => {
    if (!isSupportedLocale(tag)) {
      // Silently ignore unsupported picks — the caller is the
      // LocaleSwitcher which only emits values from
      // SupportedLocales, so an unsupported tag here is a
      // programming error and the right response is to keep the
      // current catalogue rather than fall back to English
      // (which the user already chose to leave).
      if (typeof console !== "undefined") {
        console.warn(`[i18n] ignoring unsupported locale "${tag}"`);
      }
      return;
    }
    try {
      window.localStorage.setItem(STORAGE_KEY, tag);
    } catch {
      // localStorage write can fail in private-browsing iframes;
      // the cookie below still persists the choice for the
      // current session and for the backend's resolver.
    }
    writeCookie(tag);
    setLocaleState(tag);
  }, []);

  const value = useMemo<LocaleContextValue>(
    () => ({ locale, direction, t, setLocale }),
    [locale, direction, t, setLocale],
  );

  return <Context.Provider value={value}>{children}</Context.Provider>;
}

/**
 * Hook returning the active locale context. Throws if used
 * outside a LocaleProvider — the alternative (silent default to
 * English) would mask wiring bugs and make missing-context
 * incidents harder to triage.
 */
export function useLocaleContext(): LocaleContextValue {
  const ctx = useContext(Context);
  if (!ctx) {
    throw new Error("useLocaleContext used outside <LocaleProvider>");
  }
  return ctx;
}

/** Re-export for tests and helpers that need the registry. */
export { SupportedLocales };
