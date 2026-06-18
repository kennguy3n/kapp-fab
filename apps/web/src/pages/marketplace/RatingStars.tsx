import { useState } from "react";
import { Star } from "lucide-react";
import { cn } from "@kapp/ui";
import { formatRatingAverage, formatRatingCount } from "./lib";

// Locally-built marketplace rating widgets (NOT promoted to
// @kapp/ui per the WS9 shared-area guardrail). Two surfaces:
//
//   - RatingStars: read-only summary (filled stars + numeric
//     average + "N ratings"), used on Browse cards and the detail
//     header.
//   - RatingInput: the tenant's own editable 1..5 control, used on
//     the detail page once the extension is installed.
//
// Both render five Star glyphs on KChat accent tokens (filled =
// text-accent, empty = text-fg-subtle) so they track light/dark
// automatically and never hardcode a colour.

const STAR_VALUES = [1, 2, 3, 4, 5] as const;

// Shared focus-ring recipe, copied from the @kapp/ui Button base so
// the star buttons match the design-system focus treatment exactly.
const FOCUS_RING =
  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)";

function StarGlyph({
  filled,
  className,
}: {
  filled: boolean;
  className?: string;
}) {
  return (
    <Star
      aria-hidden
      className={cn(
        "shrink-0",
        filled ? "fill-current text-accent" : "text-fg-subtle",
        className,
      )}
    />
  );
}

/**
 * RatingStars renders the read-only cross-tenant rating summary.
 * `average` is rounded to the nearest whole star for the glyphs; the
 * exact value is shown numerically alongside so precision isn't lost.
 * When `count` is 0 the glyphs render empty and the label reads
 * "No ratings yet" so an unrated listing reads as an invitation.
 */
export function RatingStars({
  average,
  count,
  size = "sm",
  className,
}: {
  average: number;
  count: number;
  size?: "sm" | "md";
  className?: string;
}) {
  const rounded = count > 0 ? Math.round(average) : 0;
  const starSize = size === "md" ? "h-4 w-4" : "h-3.5 w-3.5";
  const label =
    count > 0
      ? `Rated ${formatRatingAverage(average)} out of 5 from ${formatRatingCount(count)}`
      : "No ratings yet";
  return (
    <div
      className={cn("flex items-center gap-1.5", className)}
      role="img"
      aria-label={label}
    >
      <span className="flex items-center gap-0.5">
        {STAR_VALUES.map((v) => (
          <StarGlyph key={v} filled={v <= rounded} className={starSize} />
        ))}
      </span>
      {count > 0 ? (
        <span className="text-xs text-fg-muted">
          <span className="font-medium text-fg">
            {formatRatingAverage(average)}
          </span>{" "}
          ({formatRatingCount(count)})
        </span>
      ) : (
        <span className="text-xs text-fg-subtle">No ratings yet</span>
      )}
    </div>
  );
}

/**
 * RatingInput is the tenant's own editable 1..5 control. Hovering or
 * focusing a star previews that value; clicking commits it via
 * `onRate`. `value` is the tenant's current saved rating (0 = not yet
 * rated). Each star is an individually-labelled button so the control
 * is operable and announced by assistive tech without a custom
 * radiogroup implementation.
 */
export function RatingInput({
  value,
  onRate,
  disabled = false,
  busy = false,
}: {
  value: number;
  onRate: (stars: number) => void;
  disabled?: boolean;
  busy?: boolean;
}) {
  const [hover, setHover] = useState(0);
  const active = hover || value;
  return (
    <div
      className="inline-flex items-center gap-1"
      onMouseLeave={() => setHover(0)}
    >
      {STAR_VALUES.map((v) => (
        <button
          key={v}
          type="button"
          disabled={disabled || busy}
          aria-label={`Rate ${v} ${v === 1 ? "star" : "stars"} out of 5`}
          aria-pressed={value === v}
          onMouseEnter={() => setHover(v)}
          onFocus={() => setHover(v)}
          onBlur={() => setHover(0)}
          onClick={() => onRate(v)}
          className={cn(
            "rounded-md p-1 transition-colors",
            "hover:bg-bg-muted disabled:cursor-not-allowed disabled:opacity-60",
            FOCUS_RING,
          )}
        >
          <StarGlyph filled={v <= active} className="h-5 w-5" />
        </button>
      ))}
    </div>
  );
}
