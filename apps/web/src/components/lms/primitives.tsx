import type { ComponentType, ReactNode, SVGProps } from "react";
import { BookOpen } from "lucide-react";
import { cn, Eyebrow } from "@kapp/ui";

/**
 * Local LMS presentation primitives.
 *
 * These are small, design-system-faithful building blocks shared by the
 * LMS learner/instructor surfaces (progress, paths, badges, discussions,
 * dashboard). They live here — inside the LMS-owned component folder —
 * rather than in `@kapp/ui` because they are bespoke to the learning
 * experience (progress rings, course cover art, completion bars/charts)
 * and not general enough for the shared library yet. Every visual uses
 * design tokens (accent / success / warning / danger / info + the
 * neutral surface scale); none hardcode hex, font sizes, or radii.
 */

export type Tone = "accent" | "success" | "warning" | "danger" | "info" | "muted";

const TONE_BG: Record<Tone, string> = {
  accent: "bg-accent",
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-danger",
  info: "bg-info",
  muted: "bg-fg-subtle",
};

const TONE_TEXT: Record<Tone, string> = {
  accent: "text-accent",
  success: "text-success",
  warning: "text-warning",
  danger: "text-danger",
  info: "text-info",
  muted: "text-fg-muted",
};

type IconType = ComponentType<SVGProps<SVGSVGElement>>;

/** Clamp an arbitrary completion number into an integer 0–100 percentage. */
export function pct(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.max(0, Math.min(100, Math.round(value)));
}

/** Deterministic small hash so a title maps to a stable cover/style. */
function hashString(input: string): number {
  let hash = 0;
  for (let i = 0; i < input.length; i += 1) {
    hash = (hash * 31 + input.charCodeAt(i)) >>> 0;
  }
  return hash;
}

/**
 * Consistent page header for every LMS screen: a mono accent eyebrow,
 * a single h1 title, an optional supporting line, and a right-aligned
 * actions slot that wraps gracefully on narrow widths.
 */
export function LmsPageHeader({
  area,
  title,
  description,
  actions,
}: {
  area: string;
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="flex flex-col gap-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>{area}</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            {title}
          </h1>
          {description ? (
            <p className="mt-1 max-w-2xl text-sm text-fg-muted">{description}</p>
          ) : null}
        </div>
        {actions ? (
          <div className="flex flex-shrink-0 flex-wrap items-center gap-2">
            {actions}
          </div>
        ) : null}
      </div>
    </header>
  );
}

/**
 * Circular progress indicator. Renders an accessible SVG ring with the
 * percentage centred; the track uses the muted surface token and the
 * arc uses the supplied tone token.
 */
export function ProgressRing({
  value,
  size = 64,
  strokeWidth = 6,
  tone = "accent",
  label,
  className,
}: {
  value: number;
  size?: number;
  strokeWidth?: number;
  tone?: Tone;
  label?: string;
  className?: string;
}) {
  const v = pct(value);
  const radius = (size - strokeWidth) / 2;
  const circumference = 2 * Math.PI * radius;
  const dashOffset = circumference - (v / 100) * circumference;
  return (
    <div
      className={cn("relative inline-flex items-center justify-center", className)}
      style={{ width: size, height: size }}
    >
      <svg
        width={size}
        height={size}
        viewBox={`0 0 ${size} ${size}`}
        className="-rotate-90"
        role="img"
        aria-label={label ?? `${v}% complete`}
      >
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="currentColor"
          strokeWidth={strokeWidth}
          className="text-bg-muted"
        />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="currentColor"
          strokeWidth={strokeWidth}
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={dashOffset}
          className={cn("transition-[stroke-dashoffset] duration-500", TONE_TEXT[tone])}
        />
      </svg>
      <span className="absolute font-tabular text-sm font-semibold text-fg">
        {v}%
      </span>
    </div>
  );
}

/** Linear progress bar with an accessible role and tone-driven fill. */
export function ProgressBar({
  value,
  tone = "accent",
  className,
  label,
}: {
  value: number;
  tone?: Tone;
  className?: string;
  label?: string;
}) {
  const v = pct(value);
  return (
    <div
      className={cn("h-2 w-full overflow-hidden rounded-pill bg-bg-muted", className)}
      role="progressbar"
      aria-valuenow={v}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-label={label}
    >
      <div
        className={cn("h-full rounded-pill transition-[width] duration-500", TONE_BG[tone])}
        style={{ width: `${v}%` }}
      />
    </div>
  );
}

const COVER_GRADIENTS = [
  "from-accent to-accent-hover",
  "from-accent via-accent-hover to-info",
  "from-info to-accent",
  "from-accent to-success",
  "from-accent-hover to-accent",
];

/**
 * Gradient cover art for course / learning-path cards. Variety comes
 * from a stable hash of the seed (so a given title always renders the
 * same cover) over a violet-family token palette; the icon defaults to
 * a book but callers can pass a topical one.
 */
export function CoverArt({
  seed,
  icon: Icon = BookOpen,
  className,
}: {
  seed: string;
  icon?: IconType;
  className?: string;
}) {
  const gradient = COVER_GRADIENTS[hashString(seed) % COVER_GRADIENTS.length];
  return (
    <div
      className={cn(
        "relative flex aspect-[16/9] w-full items-center justify-center overflow-hidden rounded-lg bg-gradient-to-br text-accent-fg",
        gradient,
        className,
      )}
      aria-hidden
    >
      <Icon className="h-9 w-9 opacity-90" />
    </div>
  );
}

export interface BarDatum {
  label: string;
  value: number;
  hint?: string;
  tone?: Tone;
}

/**
 * Compact horizontal bar chart. Each row is a labelled track with a
 * tone-filled bar sized relative to `max` (defaults to the largest
 * value), plus an optional right-aligned hint (e.g. a formatted count
 * or percentage). Used for completion funnels and quiz/lesson analytics.
 */
export function BarChart({
  data,
  max,
  emptyLabel = "No data yet.",
}: {
  data: BarDatum[];
  max?: number;
  emptyLabel?: string;
}) {
  if (data.length === 0) {
    return <p className="text-sm text-fg-muted">{emptyLabel}</p>;
  }
  const ceiling = Math.max(1, max ?? Math.max(...data.map((d) => d.value)));
  return (
    <div className="flex flex-col gap-3">
      {data.map((d) => (
        <div
          key={d.label}
          className="grid grid-cols-[8rem_1fr_3.5rem] items-center gap-3 sm:grid-cols-[10rem_1fr_4rem]"
        >
          <span className="truncate text-sm text-fg-muted" title={d.label}>
            {d.label}
          </span>
          <div className="h-2.5 overflow-hidden rounded-pill bg-bg-muted">
            <div
              className={cn("h-full rounded-pill", TONE_BG[d.tone ?? "accent"])}
              style={{ width: `${pct((d.value / ceiling) * 100)}%` }}
            />
          </div>
          <span className="text-end font-tabular text-sm font-medium text-fg">
            {d.hint ?? d.value}
          </span>
        </div>
      ))}
    </div>
  );
}

export interface BarSegment {
  value: number;
  tone: Tone;
}

/**
 * A single multi-segment bar (e.g. completed vs. dropped-off within the
 * cohort that reached a lesson). Segments are laid out left-to-right and
 * sized relative to `max`.
 */
export function SegmentBar({
  segments,
  max,
  className,
}: {
  segments: BarSegment[];
  max: number;
  className?: string;
}) {
  const ceiling = Math.max(1, max);
  return (
    <div
      className={cn(
        "flex h-2.5 w-full overflow-hidden rounded-pill bg-bg-muted",
        className,
      )}
    >
      {segments.map((segment, index) => (
        <div
          key={index}
          className={cn("h-full first:rounded-l-pill last:rounded-r-pill", TONE_BG[segment.tone])}
          style={{ width: `${pct((segment.value / ceiling) * 100)}%` }}
        />
      ))}
    </div>
  );
}
