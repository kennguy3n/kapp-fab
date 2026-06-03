import type { ReactNode } from "react";

// EmptyStates provides context-aware empty-state UI for list views.
//
// Workstream 8 (Default-Wired Onboarding) replaces the generic, dead-
// end "No records found" message with a per-module call-to-action so a
// brand-new tenant always knows the next thing to do (e.g. "No deals
// yet — create your first deal" with a button that opens the form).
//
// The component is intentionally presentational: it takes a label +
// description + an `onAction` callback rather than wiring navigation
// itself, so it can be dropped into any list page (RecordListPage,
// kanban, module dashboards) without coupling to a particular router
// shape. RecordEmptyState resolves the right copy from the KType and
// hands back the same primitive.

export interface EmptyStateProps {
  /** Headline, e.g. "No deals yet". */
  title: string;
  /** Optional supporting sentence explaining the value of the module. */
  description?: string;
  /** Label for the primary CTA button. Omit to render a message-only
   *  empty state (e.g. a filtered list with no matches). */
  actionLabel?: string;
  /** Invoked when the CTA is clicked — typically opens the create form. */
  onAction?: () => void;
  /** Optional secondary action (e.g. "Import data"). */
  secondaryLabel?: string;
  onSecondaryAction?: () => void;
  /** Optional decorative glyph rendered above the title. */
  icon?: ReactNode;
}

export function EmptyState({
  title,
  description,
  actionLabel,
  onAction,
  secondaryLabel,
  onSecondaryAction,
  icon,
}: EmptyStateProps) {
  return (
    <div
      role="status"
      style={{
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        textAlign: "center",
        gap: 8,
        padding: "48px 24px",
        border: "1px dashed #d1d5db",
        borderRadius: 12,
        color: "#374151",
        background: "#fafafa",
      }}
    >
      {icon ? <div style={{ fontSize: 32, lineHeight: 1 }}>{icon}</div> : null}
      <h2 style={{ margin: 0, fontSize: 18, color: "#111827" }}>{title}</h2>
      {description ? (
        <p style={{ margin: 0, maxWidth: 420, fontSize: 14, color: "#6b7280" }}>
          {description}
        </p>
      ) : null}
      {(actionLabel && onAction) || (secondaryLabel && onSecondaryAction) ? (
        <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
          {actionLabel && onAction ? (
            <button type="button" onClick={onAction}>
              {actionLabel}
            </button>
          ) : null}
          {secondaryLabel && onSecondaryAction ? (
            <button type="button" onClick={onSecondaryAction}>
              {secondaryLabel}
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// EmptyStateCopy is the per-module headline + description + CTA label
// keyed by KType. Adding a module's bespoke empty state is a matter of
// dropping an entry here; KTypes without an entry fall back to a
// generic message derived from the KType's display name (see
// RecordEmptyState). The CTA verb is tuned per module so it reads like
// the action the user takes ("create your first deal", "add an item").
interface EmptyStateCopy {
  title: string;
  description: string;
  actionLabel: string;
}

export const MODULE_EMPTY_STATES: Record<string, EmptyStateCopy> = {
  "crm.contact": {
    title: "No contacts yet",
    description:
      "Contacts are the people you sell to and support. Add your first one to start tracking deals and invoices against it.",
    actionLabel: "Create your first contact",
  },
  "crm.lead": {
    title: "No leads yet",
    description:
      "Capture inbound interest as leads, then qualify and convert them into deals.",
    actionLabel: "Add your first lead",
  },
  "crm.deal": {
    title: "No deals yet",
    description:
      "Deals track your sales pipeline. Create your first deal to see it flow through your stages on the board.",
    actionLabel: "Create your first deal",
  },
  "finance.ar_invoice": {
    title: "No invoices yet",
    description:
      "Send your first invoice to start getting paid. Posted invoices flow straight into your AR subledger and reports.",
    actionLabel: "Create your first invoice",
  },
  "inventory.item": {
    title: "No items yet",
    description:
      "Items are the products and materials you buy, sell, and stock. Add your first item to start tracking inventory.",
    actionLabel: "Add your first item",
  },
  "hr.employee": {
    title: "No employees yet",
    description:
      "Add your team to run payroll, build the org chart, and assign work.",
    actionLabel: "Add your first employee",
  },
  "projects.project": {
    title: "No projects yet",
    description:
      "Projects group tasks, budgets, and timelines. Create your first project to start planning.",
    actionLabel: "Create your first project",
  },
  "tasks.task": {
    title: "No tasks yet",
    description: "Create a task to track work and assign it to your team.",
    actionLabel: "Create your first task",
  },
};

export interface RecordEmptyStateProps {
  /** The KType being listed (e.g. "crm.deal"). */
  ktype: string;
  /** Human-readable KType name, used for the generic fallback copy. */
  ktypeName?: string;
  /** Opens the create form for this KType. */
  onCreate: () => void;
  /** Optional secondary "import" CTA. */
  onImport?: () => void;
}

// RecordEmptyState renders the contextual empty state for a KType list.
// It looks up bespoke copy in MODULE_EMPTY_STATES and otherwise derives
// a sensible generic message from the KType display name so every list
// page — even modules without a tailored entry — gets a real CTA
// instead of a dead-end "No records found".
export function RecordEmptyState({
  ktype,
  ktypeName,
  onCreate,
  onImport,
}: RecordEmptyStateProps) {
  const preset = MODULE_EMPTY_STATES[ktype];
  const name = ktypeName?.trim() || "records";
  const copy: EmptyStateCopy = preset ?? {
    title: `No ${name.toLowerCase()} yet`,
    description: `You haven't created any ${name.toLowerCase()} yet. Create the first one to get started.`,
    actionLabel: `Create ${name.toLowerCase()}`,
  };
  return (
    <EmptyState
      title={copy.title}
      description={copy.description}
      actionLabel={copy.actionLabel}
      onAction={onCreate}
      secondaryLabel={onImport ? "Import data" : undefined}
      onSecondaryAction={onImport}
    />
  );
}
