import { type ReactNode } from "react";
import { Link } from "react-router-dom";
import { ArrowLeft } from "lucide-react";
import { cn } from "@kapp/ui";
import { KappMark } from "../auth/AuthScaffold";

/**
 * Branded chrome for the signed-in customer-portal content pages
 * (ticket list / detail / new ticket). Built locally from `@kapp/ui`
 * primitives + tokens — the portal renders outside the AppShell and
 * owns its own full-page layout.
 */
export interface PortalShellProps {
  title: string;
  description?: ReactNode;
  /** Right-aligned page-header actions (e.g. a "New ticket" button). */
  actions?: ReactNode;
  /** Optional back link shown above the page title. */
  backTo?: string;
  backLabel?: string;
  children: ReactNode;
  width?: "md" | "lg";
}

export function PortalShell({
  title,
  description,
  actions,
  backTo,
  backLabel = "Back",
  children,
  width = "lg",
}: PortalShellProps) {
  const maxW = width === "lg" ? "max-w-3xl" : "max-w-xl";
  return (
    <div className="flex min-h-screen flex-col bg-bg">
      <header className="border-b border-border bg-bg-elevated">
        <div className={cn("mx-auto flex w-full items-center gap-2.5 px-4 py-3", maxW)}>
          <KappMark size="sm" />
          <span className="text-sm font-medium text-fg">Support center</span>
        </div>
      </header>

      <main
        className={cn(
          "mx-auto flex w-full flex-1 flex-col gap-6 px-4 py-8",
          maxW,
        )}
      >
        <div className="flex flex-col gap-3">
          {backTo && (
            <Link
              to={backTo}
              className="inline-flex w-fit items-center gap-1 rounded-sm text-sm font-medium text-fg-muted transition-colors hover:text-fg"
            >
              <ArrowLeft aria-hidden="true" className="h-4 w-4" />
              {backLabel}
            </Link>
          )}
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex flex-col gap-1">
              <h1 className="text-2xl font-medium tracking-tight text-fg">
                {title}
              </h1>
              {description && (
                <p className="max-w-prose text-sm text-fg-muted">{description}</p>
              )}
            </div>
            {actions && (
              <div className="flex shrink-0 items-center gap-2">{actions}</div>
            )}
          </div>
        </div>

        {children}
      </main>
    </div>
  );
}
