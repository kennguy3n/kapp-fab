/**
 * adminKit — local, presentational building blocks shared across the
 * Admin & Platform pages (Workstream 10).
 *
 * These are deliberately NOT added to `@kapp/ui`: they compose the
 * shared design-system primitives (Eyebrow, EmptyState, Skeleton,
 * Button, Badge, toast) into the repeated page-level patterns the
 * admin surfaces need — a consistent page header, a teaching error
 * state with retry, a table loading skeleton, and a copyable id chip
 * for the few places an operator genuinely needs a raw UUID for
 * debugging. Keeping them local avoids touching the shared package
 * while still keeping the 13 pages DRY and consistent.
 *
 * Everything here is context-free (no i18n/formatter hooks) so it can
 * be rendered in any test harness without extra providers.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";
import { AlertTriangle, Check, Copy } from "lucide-react";
import {
  Button,
  cn,
  Eyebrow,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";

/**
 * AdminPageHeader — the canonical page-header pattern (Eyebrow area
 * label + truncating h1 + optional description + right-aligned
 * actions) mirrored from the RecordListPage gold standard so every
 * admin screen opens the same way.
 */
export function AdminPageHeader({
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
        </div>
        {actions && (
          <div className="flex flex-wrap items-center gap-2">{actions}</div>
        )}
      </div>
      {description && (
        <div className="max-w-2xl text-sm text-fg-muted">{description}</div>
      )}
    </header>
  );
}

/**
 * AdminErrorState — the teaching error surface with a retry action,
 * used for the "couldn't load" branch of every async page.
 */
export function AdminErrorState({
  title = "Couldn't load this page",
  error,
  onRetry,
  retryLabel = "Try again",
}: {
  title?: string;
  error: unknown;
  onRetry?: () => void;
  retryLabel?: string;
}) {
  const message =
    error instanceof Error
      ? error.message
      : typeof error === "string"
        ? error
        : "Something went wrong. Please try again.";
  return (
    <EmptyState
      icon={<AlertTriangle />}
      title={title}
      description={message}
      action={
        onRetry ? (
          <Button variant="secondary" onClick={onRetry}>
            {retryLabel}
          </Button>
        ) : undefined
      }
    />
  );
}

/**
 * AdminTableSkeleton — a column-aware loading placeholder that mirrors
 * the shape of the table it stands in for, so the layout doesn't jump
 * when real rows arrive.
 */
export function AdminTableSkeleton({
  columns,
  rows = 5,
}: {
  columns: string[];
  rows?: number;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          {columns.map((c) => (
            <TableHead key={c}>{c}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {Array.from({ length: rows }).map((_, r) => (
          <TableRow key={r}>
            {columns.map((c) => (
              <TableCell key={c}>
                <Skeleton variant="text" className="w-3/4" />
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

/**
 * Toggle — an accessible on/off switch (role="switch") built from
 * tokens. Used wherever the admin pages flip a single boolean
 * (feature flags, webhook active state, retention enabled). Prefer
 * this over a bare checkbox so the control reads as a switch to
 * assistive tech and has a comfortable hit area.
 */
export function Toggle({
  checked,
  onChange,
  label,
  disabled = false,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        "inline-flex h-5 w-9 shrink-0 items-center rounded-pill border transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)",
        "disabled:cursor-not-allowed disabled:opacity-50",
        checked
          ? "border-accent bg-accent"
          : "border-border bg-bg-muted",
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "h-3.5 w-3.5 rounded-full bg-bg-elevated shadow-sm transition-transform",
          checked ? "translate-x-[1.125rem]" : "translate-x-0.5",
        )}
      />
    </button>
  );
}

/**
 * CopyableId — renders a shortened, monospaced id with a copy button.
 * For the admin/debugging cases where a raw UUID is genuinely useful,
 * this keeps it labelled and copyable rather than dumped raw into a
 * heading (quality-bar item #1).
 */
export function CopyableId({
  value,
  label = "identifier",
  full = false,
}: {
  value: string;
  label?: string;
  full?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const resetTimer = useRef<number | null>(null);
  const display = full || value.length <= 12 ? value : `${value.slice(0, 8)}…`;

  useEffect(
    () => () => {
      if (resetTimer.current !== null) window.clearTimeout(resetTimer.current);
    },
    [],
  );

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      toast.success("Copied to clipboard");
      if (resetTimer.current !== null) window.clearTimeout(resetTimer.current);
      resetTimer.current = window.setTimeout(() => setCopied(false), 1500);
    } catch {
      toast.error("Couldn't copy to clipboard");
    }
  }

  return (
    <button
      type="button"
      onClick={copy}
      title={value}
      aria-label={`Copy ${label} ${value}`}
      className="inline-flex items-center gap-1 rounded-md border border-border bg-bg-subtle px-1.5 py-0.5 font-mono text-xs text-fg-muted transition-colors hover:bg-bg-muted hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
    >
      <span className="truncate">{display}</span>
      {copied ? (
        <Check className="h-3 w-3 shrink-0 text-success" aria-hidden="true" />
      ) : (
        <Copy className="h-3 w-3 shrink-0" aria-hidden="true" />
      )}
    </button>
  );
}
