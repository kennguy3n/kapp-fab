// Shared helpers + UI vocabulary for the B5 marketplace pages
// (browse, detail, installations). Centralised here so the badge
// colour for a given InstallStatus / ExtensionStatus is the same
// everywhere a user can see it — a status "active" pill in the
// installations list reads identical to the "active" pill the
// detail page shows on an already-installed extension.

import type {
  ExtensionStatus,
  InstallStatus,
  MarketplaceCategory,
  MarketplaceExtensionVersion,
} from "@kapp/client";

// MARKETPLACE_CATEGORIES is the display-ordered taxonomy used by the
// Browse category filter and the per-card / detail category label.
// The `value`s mirror MarketplaceCategory (packages/client) and the
// marketplace_extensions_category_valid CHECK (migration 000102); the
// `label`s are the SME-facing copy. Ordered most-broad-first so the
// dropdown reads naturally rather than alphabetically.
export const MARKETPLACE_CATEGORIES: ReadonlyArray<{
  value: MarketplaceCategory;
  label: string;
}> = [
  { value: "productivity", label: "Productivity" },
  { value: "finance", label: "Finance" },
  { value: "sales", label: "Sales" },
  { value: "marketing", label: "Marketing" },
  { value: "crm", label: "CRM" },
  { value: "hr", label: "HR" },
  { value: "inventory", label: "Inventory" },
  { value: "analytics", label: "Analytics" },
  { value: "communication", label: "Communication" },
  { value: "developer_tools", label: "Developer Tools" },
  { value: "integrations", label: "Integrations" },
  { value: "other", label: "Other" },
];

const CATEGORY_LABELS: Record<MarketplaceCategory, string> =
  Object.fromEntries(
    MARKETPLACE_CATEGORIES.map((c) => [c.value, c.label]),
  ) as Record<MarketplaceCategory, string>;

// categoryLabel renders the SME-facing label for a category token.
// Falls back to the raw token (defensively title-cased) if the API
// ships a category the bundled taxonomy doesn't yet recognise, so a
// forward-compatible server never surfaces a machine value.
export function categoryLabel(category: string): string {
  const known = CATEGORY_LABELS[category as MarketplaceCategory];
  if (known) return known;
  return category
    .split("_")
    .map((w) => (w ? w.charAt(0).toUpperCase() + w.slice(1) : w))
    .join(" ");
}

// formatRatingAverage renders the cross-tenant average to a single
// decimal place (app-store convention, e.g. "4.6"). Callers gate on
// count > 0 before showing this — a 0-count average is "—".
export function formatRatingAverage(average: number): string {
  if (!Number.isFinite(average) || average <= 0) return "—";
  return average.toFixed(1);
}

// formatRatingCount renders the human "N ratings" suffix, singularised
// and thousands-grouped, with a teaching string for the no-ratings
// case so an unrated listing reads as an invitation rather than a 0.
export function formatRatingCount(count: number): string {
  if (!Number.isFinite(count) || count <= 0) return "No ratings yet";
  return `${count.toLocaleString()} ${count === 1 ? "rating" : "ratings"}`;
}

// formatBundleSize prints a 10 MiB / 256 KiB style human size for
// the bundle_size_bytes field. EXTENSION_SPEC §2 caps bundles at
// 10 MiB (10 * 1024 * 1024 bytes), so the unit ceiling is MiB —
// no need for GiB. We use 1024-byte units (binary prefixes) to
// match the API's hard limit which is also expressed in MiB.
export function formatBundleSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(bytes < 10 * 1024 ? 1 : 0)} KiB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(bytes < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}

// formatTimestamp turns a Go-side RFC3339 string into a locale-
// aware "MMM D, YYYY" rendering. Returns the raw string unchanged
// if the value isn't parseable so the UI never shows "Invalid
// Date" on an unexpected payload.
export function formatTimestamp(value: string | undefined | null): string {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

// installStatusLabel + extensionStatusLabel render the human
// label for a status value. The wire constants are
// lowercase-underscore — these helpers capitalise/space them so
// the UI doesn't have to. Keeping the mapping centralised
// guarantees a future status (e.g. InstallStatusUpgrading) gets
// the same display everywhere it surfaces.
export function installStatusLabel(status: InstallStatus): string {
  switch (status) {
    case "active":
      return "Active";
    case "pending":
      return "Pending";
    case "installing":
      return "Installing";
    case "disabled":
      return "Disabled";
    case "failed":
      return "Failed";
    case "uninstalled":
      return "Uninstalled";
    default: {
      // Exhaustiveness check: TypeScript narrows `status` to never
      // here so a new InstallStatus that lands without a case
      // arm will surface as a type error at compile time. The
      // runtime branch covers the case where the API ships a
      // status the bundled types don't yet recognise (forward-
      // compatibility) — show the raw string rather than crash.
      const _exhaustive: never = status;
      void _exhaustive;
      return String(status);
    }
  }
}

export function extensionStatusLabel(status: ExtensionStatus): string {
  switch (status) {
    case "listed":
      return "Listed";
    case "unpublished":
      return "Unpublished";
    case "deprecated":
      return "Deprecated";
    case "removed":
      return "Removed";
    default: {
      const _exhaustive: never = status;
      void _exhaustive;
      return String(status);
    }
  }
}

// installStatusVariant maps an InstallStatus to a Badge variant
// so the colour palette is consistent (active = success,
// disabled/failed = warn/destructive, etc.). Kept narrow to
// Badge's own variant union so a typo (e.g. "secondary") would
// fail typecheck.
// BadgeVariant mirrors the variant union pinned in
// packages/ui/src/components/Badge.tsx (cva variants). Keeping
// the union here keyed to the same names ensures a typo (e.g.
// "destructive" — a different design-system convention) fails
// typecheck rather than silently rendering with the default
// variant.
export type BadgeVariant =
  | "default"
  | "accent"
  | "success"
  | "warning"
  | "danger"
  | "info"
  | "outline";

export function installStatusVariant(status: InstallStatus): BadgeVariant {
  switch (status) {
    case "active":
      return "success";
    case "pending":
    case "installing":
      return "info";
    case "disabled":
      return "warning";
    case "failed":
      return "danger";
    case "uninstalled":
      return "outline";
    default:
      return "outline";
  }
}

export function extensionStatusVariant(status: ExtensionStatus): BadgeVariant {
  switch (status) {
    case "listed":
      return "success";
    case "deprecated":
      return "warning";
    case "removed":
      return "danger";
    case "unpublished":
      return "outline";
    default:
      return "outline";
  }
}

// sortVersions orders ExtensionVersion rows newest-first by
// PublishedAt. SemVer ordering is intentionally NOT used — a
// publisher may have shipped 1.0.4 chronologically AFTER 1.1.0
// (e.g. a backport patch), in which case the catalog has to show
// the newest publish first regardless of SemVer comparison. The
// tie-breaker on equal timestamps falls back to lexicographic
// SemVer descending so the order is at least deterministic.
//
// NaN safety: server-sent published_at values are always valid
// RFC3339 strings, but a malformed payload (mock, replay, etc.)
// must not break the sort. The Array.prototype.sort contract
// requires the comparator to return a finite number; returning
// NaN produces engine-defined, non-deterministic ordering. We
// treat NaN timestamps as "older than every finite timestamp"
// (sorts to the bottom) and fall back to SemVer-desc when both
// inputs are NaN so ordering remains deterministic.
export function sortVersionsByPublishedDesc(
  versions: MarketplaceExtensionVersion[],
): MarketplaceExtensionVersion[] {
  return [...versions].sort((a, b) => {
    const ta = new Date(a.published_at).getTime();
    const tb = new Date(b.published_at).getTime();
    const aFinite = Number.isFinite(ta);
    const bFinite = Number.isFinite(tb);
    if (aFinite && bFinite) {
      if (ta !== tb) return tb - ta;
      return b.version.localeCompare(a.version);
    }
    // Exactly one side finite — the finite side sorts first
    // (newer than "no known timestamp").
    if (aFinite) return -1;
    if (bFinite) return 1;
    // Both NaN — deterministic SemVer tiebreaker, descending.
    return b.version.localeCompare(a.version);
  });
}

// Marketplace install endpoints reject empty webhook_base with a
// 400 (the engine threads it through every webhook signing/post
// call). Default to the calling tenant's app host so a fresh
// install works without the user having to type a URL — power
// users can still override in the install dialog.
export function defaultWebhookBase(): string {
  if (typeof window === "undefined" || !window.location) return "";
  return window.location.origin;
}
