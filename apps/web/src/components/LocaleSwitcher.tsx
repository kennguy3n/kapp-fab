/**
 * LocaleSwitcher — header / sidebar control that lets the user
 * pick a UI language. Renders the active locale's native display
 * name (so a French speaker finds "Français" rather than "French
 * (French)") and lists every shipped catalogue.
 *
 * The control writes through to LocaleContext.setLocale which:
 *   1. Persists the pick in localStorage["kapp_locale"].
 *   2. Sets the kapp_locale cookie so the backend middleware
 *      resolves the same catalogue on the next request.
 *   3. Stamps <html lang> + <html dir> for accessibility + RTL.
 *
 * The component is a thin wrapper around `@kapp/ui`'s native
 * <Select> — native is the right primitive for a locale picker
 * because the OS-level dropdown on mobile gets the wheel picker
 * treatment users expect, and the native <option> values
 * participate in the document language attribute. Custom Radix
 * Selects flatten this into a popover that loses keyboard +
 * IME affordances on some platforms.
 */

import { Select } from "@kapp/ui";
import { SupportedLocales, useTranslation } from "../lib/i18n";

export interface LocaleSwitcherProps {
  /** Optional CSS class for layout positioning by the caller. */
  className?: string;
  /** Render size — defaults to "sm" so the switcher fits in a header strip. */
  size?: "sm" | "md" | "lg";
  /**
   * When true, an aria-label is generated from `t("common.language")`
   * so screen readers announce the control's purpose. Default true.
   */
  showAriaLabel?: boolean;
}

export function LocaleSwitcher({
  className,
  size = "sm",
  showAriaLabel = true,
}: LocaleSwitcherProps) {
  const { locale, setLocale, t } = useTranslation();
  const ariaLabel = showAriaLabel ? t("common.language") : undefined;

  return (
    <Select
      size={size}
      value={locale}
      onChange={(e) => setLocale(e.target.value)}
      className={className}
      aria-label={ariaLabel}
    >
      {SupportedLocales.map((l) => (
        <option key={l.tag} value={l.tag}>
          {l.name}
        </option>
      ))}
    </Select>
  );
}
