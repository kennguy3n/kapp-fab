/**
 * ktypeView — presentation helpers for the generic Record Engine.
 *
 * The list/form/kanban/detail surfaces are rendered entirely from
 * declarative KType field metadata. This module turns that raw
 * metadata (machine ids, snake_case field keys, enum tokens, UUID
 * relations) into the humanized, locale-formatted, design-system
 * shaped values the UI presents. Everything here is pure (no JSX)
 * except `useRelationLabels`, which resolves relation UUIDs to
 * human labels via the existing data layer.
 */
import { useMemo } from "react";
import { useQueries } from "@tanstack/react-query";
import type { FieldSpec, KRecord } from "@kapp/client";
import type { BadgeProps } from "@kapp/ui";
import type { Formatters } from "./i18n";
import { api } from "./api";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Small acronym table so humanized labels read naturally. */
const ACRONYMS: Record<string, string> = {
  id: "ID",
  ar: "AR",
  ap: "AP",
  po: "PO",
  pos: "POS",
  sku: "SKU",
  url: "URL",
  crm: "CRM",
  hr: "HR",
  lms: "LMS",
  bom: "BOM",
  vat: "VAT",
  api: "API",
  pto: "PTO",
  ytd: "YTD",
  qty: "Qty",
};

function splitWords(raw: string): string[] {
  return raw
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .split(/[\s_\-.]+/)
    .filter(Boolean);
}

function titleCaseWord(word: string): string {
  const lower = word.toLowerCase();
  if (ACRONYMS[lower]) return ACRONYMS[lower];
  return lower.charAt(0).toUpperCase() + lower.slice(1);
}

function toTitle(raw: string): string {
  const words = splitWords(raw);
  return words.length ? words.map(titleCaseWord).join(" ") : raw;
}

/** Humanize a field key into a Title Case label (drops a trailing _id). */
export function humanizeLabel(raw: string): string {
  return toTitle(raw.replace(/_id$/i, ""));
}

/** Humanize an enum/status token (e.g. `in_progress` → `In Progress`). */
export function humanizeToken(raw: string): string {
  return toTitle(raw);
}

/** The trailing segment of a ktype id, humanized (`crm.ar_invoice` → `AR Invoice`). */
export function ktypeSingular(ktypeId: string): string {
  const last = ktypeId.split(".").pop() ?? ktypeId;
  return toTitle(last);
}

function pluralizeWord(word: string): string {
  if (/[^aeiou]y$/i.test(word)) return word.replace(/y$/i, "ies");
  if (/(s|x|z|ch|sh)$/i.test(word)) return `${word}es`;
  return `${word}s`;
}

/** A plural display label for a ktype id (`crm.lead` → `Leads`). */
export function ktypePlural(ktypeId: string): string {
  const singular = ktypeSingular(ktypeId);
  const parts = singular.split(" ");
  parts[parts.length - 1] = pluralizeWord(parts[parts.length - 1]!);
  return parts.join(" ");
}

const STATUS_VARIANT: Record<string, BadgeVariant> = {
  // success — healthy / done / positive terminal states
  active: "success",
  paid: "success",
  approved: "success",
  completed: "success",
  complete: "success",
  done: "success",
  "in-stock": "success",
  in_stock: "success",
  won: "success",
  closed_won: "success",
  present: "success",
  published: "success",
  confirmed: "success",
  fulfilled: "success",
  resolved: "success",
  enabled: "success",
  success: "success",
  // warning — needs attention / interim
  pending: "warning",
  draft: "warning",
  "low-stock": "warning",
  low_stock: "warning",
  awaiting: "warning",
  nurturing: "warning",
  planning: "warning",
  planned: "warning",
  submitted: "warning",
  waiting: "warning",
  half_day: "warning",
  on_hold: "warning",
  overdue_soon: "warning",
  // danger — failed / negative terminal states
  failed: "danger",
  overdue: "danger",
  cancelled: "danger",
  canceled: "danger",
  suspended: "danger",
  "out-of-stock": "danger",
  out_of_stock: "danger",
  rejected: "danger",
  lost: "danger",
  closed_lost: "danger",
  urgent: "danger",
  error: "danger",
  blocked: "danger",
  // info — in-flight / informational
  new: "info",
  processing: "info",
  scheduled: "info",
  "in-progress": "info",
  in_progress: "info",
  contacted: "info",
  qualified: "info",
  qualification: "info",
  prospecting: "info",
  proposal: "info",
  negotiation: "info",
  open: "info",
  // accent — highlighted / branded
  featured: "accent",
  branded: "accent",
  // neutral — low-signal / archived
  archived: "neutral",
  closed: "neutral",
  inactive: "neutral",
  expired: "neutral",
  "n/a": "neutral",
  na: "neutral",
  // priority scale
  low: "neutral",
  medium: "info",
  high: "warning",
};

/** Map a status/enum token to a Badge variant (THEME.md contract). */
export function statusVariant(token: string): BadgeVariant {
  return STATUS_VARIANT[token.toLowerCase()] ?? "neutral";
}

/**
 * Convention map of relation field name → target ktype id. Used when
 * a FieldSpec doesn't carry an explicit `ref`/`ktype` (the platform
 * KTypes describe relations purely by the `_id` naming convention).
 */
const RELATION_KTYPE_BY_FIELD: Record<string, string> = {
  organization_id: "crm.organization",
  customer_id: "crm.organization",
  default_customer_id: "crm.organization",
  supplier_id: "crm.organization",
  reporting_to: "hr.employee",
  assigned_to: "hr.employee",
  employee_id: "hr.employee",
  deal_id: "crm.deal",
  project_id: "projects.project",
  course_id: "lms.course",
  module_id: "lms.module",
  lesson_id: "lms.lesson",
  enrollment_id: "lms.enrollment",
  item_id: "inventory.item",
  bank_account_id: "finance.bank_account",
  profile_id: "sales.pos_profile",
};

/** Resolve the target ktype a relation field points at, if known. */
export function relationTargetKtype(field: FieldSpec): string | null {
  return field.ref ?? field.ktype ?? RELATION_KTYPE_BY_FIELD[field.name] ?? null;
}

const STATUS_FIELD_NAMES = new Set([
  "status",
  "stage",
  "state",
  "source",
  "priority",
  "kind",
  "channel",
  "type",
  "severity",
  "phase",
  "disposition",
  "outcome",
  "tier",
  "level",
]);

/** Whether a field's values should render as status Badges. */
export function isStatusField(field: FieldSpec): boolean {
  return (
    field.type === "enum" ||
    (Array.isArray(field.values) && field.values.length > 0) ||
    STATUS_FIELD_NAMES.has(field.name.toLowerCase())
  );
}

const NUMERIC_TYPES = new Set([
  "number",
  "integer",
  "int",
  "float",
  "double",
  "decimal",
  "money",
  "currency",
]);

/** Whether a field holds a numeric value (drives right-alignment). */
export function isNumericField(field: FieldSpec): boolean {
  return NUMERIC_TYPES.has(field.type.toLowerCase());
}

// Curated money-name tokens, matched against the *tokenized* field
// name (so `unit_price` → ["unit", "price"]) rather than as raw
// substrings. This avoids false positives (e.g. `evaluation`) and
// deliberately excludes ambiguous bare words like `due`, `rate`,
// `net`, `gross`, and `paid` that are frequently plain counts/ratios
// rather than monetary amounts.
const MONEY_NAME_TOKENS = new Set([
  "amount",
  "total",
  "subtotal",
  "price",
  "cost",
  "balance",
  "fee",
  "salary",
  "wage",
  "payroll",
  "revenue",
  "budget",
  "discount",
  "tax",
  "charge",
  "payment",
  "premium",
  "refund",
  "deposit",
  "mrr",
  "arr",
  "value",
]);
const MONEY_TYPES = new Set(["money", "currency"]);

/** The control to render for a field in a form. */
export type ControlKind =
  | "boolean"
  | "select"
  | "relation"
  | "date"
  | "datetime"
  | "email"
  | "tel"
  | "url"
  | "textarea"
  | "number"
  | "text";

/** Resolve the right form control for a field, driven by its metadata. */
export function resolveControl(field: FieldSpec): ControlKind {
  const type = field.type.toLowerCase();
  const name = field.name.toLowerCase();

  if (type === "boolean" || type === "bool") return "boolean";
  if (type === "enum" || (Array.isArray(field.values) && field.values.length > 0))
    return "select";
  if (relationTargetKtype(field)) return "relation";
  if (type === "date") return "date";
  if (type === "datetime" || type === "timestamp") return "datetime";
  if (type === "text") return "textarea";
  if (type === "email" || name === "email" || name.endsWith("_email"))
    return "email";
  if (
    type === "tel" ||
    type === "phone" ||
    name === "phone" ||
    name === "mobile" ||
    name.endsWith("_phone")
  )
    return "tel";
  if (type === "url" || name === "url" || name === "website") return "url";
  if (NUMERIC_TYPES.has(type)) return "number";
  // Long free-text fallback runs after the typed/name-based controls so a
  // generous `max_length` on a typed field (numeric, email, url, phone)
  // never overrides its specialised input.
  if (field.max_length != null && field.max_length > 160) return "textarea";
  return "text";
}

/**
 * Whether a numeric field represents a monetary amount. Driven by an
 * explicit money/currency type or a curated money name token. The
 * `$` affordance / currency formatting is gated further at the call
 * site on an actual currency context (a sibling `currency` field on
 * the schema, or a `currency` value on the record) so a plain numeric
 * field never picks up a spurious currency presentation.
 */
export function isMoneyField(field: FieldSpec): boolean {
  if (MONEY_TYPES.has(field.type.toLowerCase())) return true;
  return splitWords(field.name).some((w) =>
    MONEY_NAME_TOKENS.has(w.toLowerCase()),
  );
}

/**
 * Whether a KType schema models currency, i.e. it has a dedicated
 * `currency` field (or an explicit money/currency-typed field). Used
 * to gate the form's `$` affordance so monetary inputs only show a
 * currency symbol when the schema actually tracks a currency.
 */
export function schemaHasCurrency(fields: FieldSpec[]): boolean {
  return fields.some((f) => {
    const t = f.type.toLowerCase();
    return f.name.toLowerCase() === "currency" || MONEY_TYPES.has(t);
  });
}

/** A human label for a record (for relation pickers / resolved cells). */
export function recordLabel(record: KRecord): string {
  const data = record.data;
  const preferred = [
    "name",
    "title",
    "full_name",
    "display_name",
    "label",
    "code",
    "email",
    "subject",
  ];
  for (const key of preferred) {
    const value = data[key];
    if (typeof value === "string" && value.trim()) return value;
  }
  for (const value of Object.values(data)) {
    if (typeof value === "string" && value.trim() && !UUID_RE.test(value))
      return value;
  }
  return "Untitled";
}

function toDate(value: unknown): Date | null {
  if (value instanceof Date) return Number.isNaN(value.getTime()) ? null : value;
  if (typeof value === "number") {
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? null : d;
  }
  if (typeof value === "string" && value.trim()) {
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? null : d;
  }
  return null;
}

/**
 * Format a field value for display. Returns a human, locale-aware
 * string and never leaks a raw UUID or ISO timestamp. Relation
 * fields should be resolved by the caller first; an unresolved UUID
 * falls back to an em dash.
 */
export function formatValue(
  field: FieldSpec,
  value: unknown,
  record: Pick<KRecord, "data">,
  fmt: Formatters,
): string {
  if (value == null || value === "") return "—";
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (Array.isArray(value)) {
    if (value.length === 0) return "—";
    return value.map((v) => humanizeToken(String(v))).join(", ");
  }

  const type = field.type.toLowerCase();

  if (type === "date") {
    const d = toDate(value);
    return d ? fmt.date(d) : String(value);
  }
  if (type === "datetime" || type === "timestamp") {
    const d = toDate(value);
    return d ? fmt.dateTime(d) : String(value);
  }

  if (typeof value === "number" || NUMERIC_TYPES.has(type)) {
    const n = typeof value === "number" ? value : Number(value);
    if (Number.isFinite(n)) {
      const currencyCode = record.data["currency"];
      if (
        isMoneyField(field) &&
        typeof currencyCode === "string" &&
        currencyCode.trim()
      ) {
        return fmt.currency(n, currencyCode);
      }
      return fmt.number(n);
    }
    return String(value);
  }

  if (typeof value === "string" && UUID_RE.test(value)) return "—";
  return String(value);
}

export interface RelationResolver {
  resolve: (field: FieldSpec, value: unknown) => string | null;
  isLoading: boolean;
}

/**
 * Resolve relation UUIDs to human labels. Fetches the distinct target
 * ktypes referenced by `fields` (via the shared react-query data
 * layer) and returns a resolver mapping (field, idValue) → label.
 * Uses `useQueries` so the hook count stays stable as the operator
 * navigates between ktypes with different relation shapes.
 */
export function useRelationLabels(fields: FieldSpec[]): RelationResolver {
  const targets = useMemo(() => {
    const set = new Set<string>();
    for (const field of fields) {
      const target = relationTargetKtype(field);
      if (target) set.add(target);
    }
    return [...set].sort();
  }, [fields]);

  const results = useQueries({
    queries: targets.map((target) => ({
      queryKey: ["records", target],
      queryFn: () => api.listRecords(target),
      staleTime: 60_000,
    })),
  });

  return useMemo<RelationResolver>(() => {
    const byKtype = new Map<string, Map<string, string>>();
    targets.forEach((target, i) => {
      const inner = new Map<string, string>();
      for (const record of results[i]?.data ?? []) {
        inner.set(record.id, recordLabel(record));
      }
      byKtype.set(target, inner);
    });
    return {
      isLoading: results.some((r) => r.isLoading),
      resolve: (field, value) => {
        const target = relationTargetKtype(field);
        if (!target) return null;
        if (value == null || value === "") return null;
        return byKtype.get(target)?.get(String(value)) ?? null;
      },
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targets, results.map((r) => r.dataUpdatedAt).join(",")]);
}
