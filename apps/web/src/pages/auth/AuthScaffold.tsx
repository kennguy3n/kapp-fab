import { type ReactNode } from "react";
import { Card, CardContent, cn } from "@kapp/ui";

/**
 * Local, KChat-branded scaffolding for the public auth surfaces
 * (Login, OAuth callback, customer-portal sign-in). These pages render
 * OUTSIDE the AppShell, so they own their full-page layout. Per the
 * Workstream 11 guardrails we do NOT add to `@kapp/ui`; this wrapper is
 * built locally from `@kapp/ui` primitives + design tokens only.
 */

type MarkSize = "sm" | "md" | "lg";

const MARK_SIZES: Record<MarkSize, { box: string; tail: string }> = {
  sm: { box: "h-7 w-7 text-sm", tail: "h-2 w-2 -bottom-0.5 start-1" },
  md: { box: "h-9 w-9 text-base", tail: "h-2.5 w-2.5 -bottom-0.5 start-1" },
  lg: { box: "h-12 w-12 text-xl", tail: "h-3 w-3 -bottom-1 start-1.5" },
};

export interface KappMarkProps {
  size?: MarkSize;
  className?: string;
  /** Hide from the accessibility tree when paired with a visible wordmark. */
  decorative?: boolean;
}

/**
 * The canonical KChat speech-bubble logo: a violet rounded square with
 * the letter "K" and a small rotated-square "tail" at the bottom inline
 * start. Mirrors the AppShell sidebar brand mark, scaled up for the
 * auth pages.
 */
export function KappMark({ size = "lg", className, decorative }: KappMarkProps) {
  const dims = MARK_SIZES[size];
  return (
    <span
      role={decorative ? undefined : "img"}
      aria-label={decorative ? undefined : "Kapp"}
      aria-hidden={decorative || undefined}
      className={cn(
        "relative flex shrink-0 items-center justify-center rounded-lg bg-accent font-semibold text-accent-fg",
        dims.box,
        className,
      )}
    >
      <span aria-hidden="true">K</span>
      <span
        aria-hidden="true"
        className={cn("absolute rotate-45 rounded-xs bg-accent", dims.tail)}
      />
    </span>
  );
}

export interface AuthScaffoldProps {
  /** Main heading rendered as the page's single `<h1>`. */
  title?: string;
  description?: ReactNode;
  children: ReactNode;
  /** Rendered centered, below the card (e.g. a help link or legal note). */
  footer?: ReactNode;
  /** Replaces the card body with a bare centered column (used for the
   *  branded "Signing you in…" state which has no form chrome). */
  bare?: boolean;
}

export function AuthScaffold({
  title,
  description,
  children,
  footer,
  bare,
}: AuthScaffoldProps) {
  return (
    <main className="flex min-h-screen w-full flex-col items-center justify-center gap-6 bg-bg px-4 py-10">
      <div className="flex w-full max-w-md flex-col items-stretch gap-6">
        <div className="flex justify-center">
          <KappMark size="lg" />
        </div>

        {bare ? (
          <div className="flex flex-col items-center gap-4 text-center">
            {children}
          </div>
        ) : (
          <Card>
            <CardContent className="flex flex-col gap-5 p-6">
              {(title || description) && (
                <header className="flex flex-col gap-1.5 text-center">
                  {title && (
                    <h1 className="text-xl font-medium tracking-tight text-fg">
                      {title}
                    </h1>
                  )}
                  {description && (
                    <p className="text-sm text-fg-muted">{description}</p>
                  )}
                </header>
              )}
              {children}
            </CardContent>
          </Card>
        )}

        {footer && (
          <div className="text-center text-sm text-fg-muted">{footer}</div>
        )}
      </div>
    </main>
  );
}

export interface AuthAlertProps {
  tone?: "danger" | "success" | "info";
  children: ReactNode;
  className?: string;
}

/**
 * Inline, token-only status banner for the auth surfaces. Mirrors the
 * proven SetupWizard missing-tenant pattern (bg-bg-subtle + a coloured
 * inline-start border) so error/success copy reads consistently and
 * never leaks a raw exception string.
 */
export function AuthAlert({ tone = "danger", children, className }: AuthAlertProps) {
  const toneClass = {
    danger: "border-s-danger text-danger",
    success: "border-s-success text-success",
    info: "border-s-info text-info",
  }[tone];
  return (
    <div
      role={tone === "danger" ? "alert" : "status"}
      className={cn(
        "rounded-md border border-border border-s-2 bg-bg-subtle px-3 py-2 text-sm",
        toneClass,
        className,
      )}
    >
      {children}
    </div>
  );
}
