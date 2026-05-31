// Shared helpers + UI vocabulary for the B5 marketplace pages
// (browse, detail, installations). Centralised here so the badge
// colour for a given InstallStatus / ExtensionStatus is the same
// everywhere a user can see it — a status "active" pill in the
// installations list reads identical to the "active" pill the
// detail page shows on an already-installed extension.

import type {
  ExtensionStatus,
  InstallStatus,
  MarketplaceExtensionVersion,
} from "@kapp/client";

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
export function sortVersionsByPublishedDesc(
  versions: MarketplaceExtensionVersion[],
): MarketplaceExtensionVersion[] {
  return [...versions].sort((a, b) => {
    const ta = new Date(a.published_at).getTime();
    const tb = new Date(b.published_at).getTime();
    if (ta !== tb) return tb - ta;
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
