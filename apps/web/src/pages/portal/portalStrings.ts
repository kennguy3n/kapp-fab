import type { BadgeProps } from "@kapp/ui";

/**
 * Local, human-facing label maps for the customer portal. Follows the
 * sanctioned in-repo pattern (see ReconciliationStrings / MrpStrings):
 * machine tokens (statuses, priorities, author kinds) and raw API error
 * strings are NEVER surfaced to users — they are mapped here to plain,
 * SME-friendly copy and design-system Badge variants.
 *
 * Deliberately NOT wired through i18n: adding keys to the frontend
 * catalogue alone would break the backend/frontend catalogue-parity
 * CI gate, so the portal (which is not i18n-wired today) keeps plain
 * English strings local to the module.
 */

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

export interface BadgeMeta {
  label: string;
  variant: BadgeVariant;
}

/** Title-case a raw machine token as a last-resort humanisation. */
export function humanizeToken(token: string): string {
  const cleaned = token.replace(/[._-]+/g, " ").trim();
  if (!cleaned) return "";
  return cleaned
    .split(/\s+/)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1).toLowerCase())
    .join(" ");
}

const STATUS_META: Record<string, BadgeMeta> = {
  new: { label: "New", variant: "info" },
  open: { label: "Open", variant: "info" },
  pending: { label: "Pending", variant: "warning" },
  waiting: { label: "Waiting on you", variant: "warning" },
  in_progress: { label: "In progress", variant: "accent" },
  on_hold: { label: "On hold", variant: "neutral" },
  resolved: { label: "Resolved", variant: "success" },
  closed: { label: "Closed", variant: "neutral" },
};

export function ticketStatusMeta(status: string | undefined): BadgeMeta {
  if (!status) return { label: "Open", variant: "info" };
  return (
    STATUS_META[status.toLowerCase()] ?? {
      label: humanizeToken(status),
      variant: "neutral",
    }
  );
}

const PRIORITY_META: Record<string, BadgeMeta> = {
  low: { label: "Low", variant: "neutral" },
  medium: { label: "Medium", variant: "info" },
  high: { label: "High", variant: "warning" },
  urgent: { label: "Urgent", variant: "danger" },
};

export function ticketPriorityMeta(priority: string | undefined): BadgeMeta {
  if (!priority) return PRIORITY_META.medium;
  return (
    PRIORITY_META[priority.toLowerCase()] ?? {
      label: humanizeToken(priority),
      variant: "neutral",
    }
  );
}

/** Human-labelled priority options for the new-ticket form. */
export const PRIORITY_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "low", label: "Low" },
  { value: "medium", label: "Medium" },
  { value: "high", label: "High" },
  { value: "urgent", label: "Urgent" },
];

/** Humanise a reply author kind ("customer" / "agent" / "support"). */
export function replyKindLabel(kind: string | undefined): string {
  switch ((kind ?? "").toLowerCase()) {
    case "customer":
      return "Customer";
    case "agent":
    case "support":
    case "staff":
      return "Support";
    default:
      return kind ? humanizeToken(kind) : "Support";
  }
}

/**
 * Map a raw API failure to friendly, plain-language copy. `portalApi`
 * throws `Error("<status>: <body>")`; we surface a calm message keyed
 * off the HTTP status and never echo the raw body to the customer.
 */
export function friendlyPortalError(
  err: unknown,
  fallback = "Something went wrong. Please try again.",
): string {
  const msg = err instanceof Error ? err.message : "";
  const status = Number.parseInt(msg.split(":")[0], 10);
  switch (status) {
    case 400:
      return "Some details look incorrect. Please review your entries and try again.";
    case 401:
    case 403:
      return "Your sign-in link has expired. Please request a new one.";
    case 404:
      return "We couldn't find what you're looking for.";
    case 409:
      return "That request conflicts with an existing one.";
    case 429:
      return "Too many attempts. Please wait a moment and try again.";
    default:
      if (status >= 500)
        return "Our support system is temporarily unavailable. Please try again shortly.";
      return fallback;
  }
}
