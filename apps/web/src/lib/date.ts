/**
 * Calendar-date helpers shared by the inventory and manufacturing
 * pages. API payloads carry calendar days as `YYYY-MM-DD` (optionally
 * with a time suffix); we want to render the same day the backend
 * meant regardless of the viewer's timezone.
 */

/**
 * Turn a calendar-day string (`YYYY-MM-DD`, optionally with a time
 * suffix) into a Date at LOCAL midnight, so formatting never drifts to
 * the previous day for users east of UTC the way `new Date("2026-01-01")`
 * (parsed as UTC) can.
 */
export function parseCalendarDate(value: string): Date {
  const [y, m, d] = value.slice(0, 10).split("-").map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

/** Format a local Date as a `YYYY-MM-DD` calendar-day string. */
export function toCalendarISO(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}
