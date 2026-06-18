// Small presentation helpers shared by the Sales/Procurement pages
// to keep machine values (UUIDs, enum tokens) off the screen.

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** True when a string is a UUID or a `ktype:slug`-style identifier —
 *  i.e. a machine value that should never be surfaced raw. */
export function looksLikeId(value: string): boolean {
  if (UUID_RE.test(value)) return true;
  return /^[a-z0-9_]+\.[a-z0-9_]+:/i.test(value);
}

/** Turn an enum token like `in_progress` into `In Progress`. */
export function titleCase(value: string): string {
  return value
    .split(/[_\s]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(" ");
}
