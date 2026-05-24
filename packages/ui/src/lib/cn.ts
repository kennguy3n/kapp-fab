import clsx, { type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * `cn` is the canonical class-merge helper for every @kapp/ui
 * component.  Two responsibilities:
 *
 *   1. **clsx**: accept `string | undefined | false | string[] |
 *      Record<string,boolean>` and flatten to a space-separated
 *      class list.  This is how component variants compose
 *      (`cn(base, variantClasses[size], maybeDisabled && disabledClasses,
 *      className)`).
 *
 *   2. **tailwind-merge**: when two Tailwind utilities target the
 *      same property (e.g. `px-2 px-4`), keep only the later one
 *      so caller overrides actually win.  Without this, passing
 *      `className="px-8"` to a Button whose base class is `px-4`
 *      would emit both and the browser would resolve to alphabetical
 *      cascade order — undefined behaviour from the caller's POV.
 *
 * Every component in this package should compose its class list
 * through `cn()`, never via raw template literals — the
 * tailwind-merge pass is what makes the `className` prop a real
 * extension point instead of a passive append.
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
