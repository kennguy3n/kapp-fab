// Demo / screenshot mock data layer.
//
// Populated when the app boots with VITE_DEMO_MODE=true. Every fixture
// below describes a fictional company "Acme Corp" (slug `acme`) and is
// matched to the TypeScript shapes exported from `packages/client/src/index.ts`.
// IDs use deterministic UUIDs so screenshot diffs stay stable across runs.

import type {
  Approval,
  AuditEntry,
  BankFeedRule,
  BankFeedSuggestion,
  BOM,
  BOMComponent,
  Budget,
  BudgetLine,
  BudgetVarianceAccountType,
  BudgetVarianceReport,
  BudgetVarianceRow,
  CapacityDayLoad,
  CapacityPlan,
  CycleCountLine,
  CycleCountSession,
  DashboardSummary,
  ExchangeRate,
  FinanceAccount,
  IncomeStatement,
  InsightsDashboard,
  InsightsDashboardBundle,
  InsightsQuery,
  InsightsRunResult,
  InsightsWidget,
  InventoryItem,
  InventoryValuationReport,
  InventoryWarehouse,
  Interview,
  JobApplication,
  JobCard,
  JobOpening,
  JournalEntry,
  KRecord,
  KType,
  LearningPath,
  LearningPathCourse,
  Badge,
  BadgeAward,
  LandedCostCharge,
  LandedCostTarget,
  LandedCostVoucher,
  MarketplaceExtension,
  MarketplaceExtensionVersion,
  MarketplaceInstallation,
  MRPDemandLine,
  MRPPlannedOrder,
  MRPRun,
  Plan,
  PlacementPolicy,
  RetentionPolicy,
  Routing,
  RoutingOperation,
  SLAPolicy,
  SavedReport,
  SavedView,
  SearchResponse,
  StockLevel,
  SubcontractComponent,
  SubcontractOrder,
  Tenant,
  TenantFeaturesResponse,
  TenantUsageHistoryResponse,
  TenantUsageResponse,
  TrialBalanceReport,
  Webhook,
  WebhookDelivery,
  WorkCenter,
  WorkCenterSchedule,
  WorkOrder,
} from "@kapp/client";
import { toCalendarISO } from "./date";

// --- Constants --------------------------------------------------------

export const DEMO_TENANT_ID = "00000000-0000-0000-0000-000000000001";
export const DEMO_TENANT_SLUG = "acme";
export const DEMO_BASE_CURRENCY = "USD";

const TODAY = new Date();
const NOW_ISO = TODAY.toISOString();
const LAST_WEEK_ISO = new Date(TODAY.getTime() - 7 * 86400_000).toISOString();
const LAST_MONTH_ISO = new Date(TODAY.getTime() - 30 * 86400_000).toISOString();
const NEXT_WEEK_ISO = new Date(TODAY.getTime() + 7 * 86400_000).toISOString();

function addDays(d: Date, n: number): Date {
  const out = new Date(d);
  out.setDate(out.getDate() + n);
  return out;
}
const TODAY_ISO_DATE = toCalendarISO(TODAY);

// uuid generates a deterministic v4-shaped UUID from a counter so that
// fixtures keep the same identifier across reloads — important for
// screenshot reproducibility and for cross-fixture references.
let __uuidCounter = 1000;
function uuid(seed?: string): string {
  if (seed) {
    // Stable hash → UUID for label-based seeds (e.g. account codes).
    let h = 0;
    for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) | 0;
    const hex = Math.abs(h).toString(16).padStart(12, "0").slice(-12);
    return `00000000-0000-4000-8000-${hex}`;
  }
  __uuidCounter += 1;
  return `00000000-0000-4000-8000-${String(__uuidCounter).padStart(12, "0")}`;
}

// kr builds a KRecord with the standard envelope every server response uses.
function kr(
  ktype: string,
  idSeed: string,
  data: Record<string, unknown>,
  status = "active"
): KRecord {
  return {
    id: uuid(`${ktype}:${idSeed}`),
    tenant_id: DEMO_TENANT_ID,
    ktype,
    ktype_version: 1,
    data,
    status,
    version: 1,
    created_at: LAST_MONTH_ISO,
    updated_at: NOW_ISO,
  };
}

// --- KType definitions ------------------------------------------------

function basicSchema(name: string, fields: { name: string; type: string }[]) {
  return {
    name,
    version: 1,
    fields: fields.map((f) => ({ name: f.name, type: f.type })),
    views: { list: { columns: fields.slice(0, 6).map((f) => f.name) } },
  };
}

const KTYPES: KType[] = [
  // CRM
  {
    name: "crm.lead",
    version: 1,
    schema: basicSchema("crm.lead", [
      { name: "name", type: "string" },
      { name: "company", type: "string" },
      { name: "email", type: "string" },
      { name: "phone", type: "string" },
      { name: "source", type: "string" },
      { name: "status", type: "string" },
      { name: "owner", type: "string" },
    ]),
  },
  {
    name: "crm.contact",
    version: 1,
    schema: basicSchema("crm.contact", [
      { name: "name", type: "string" },
      { name: "title", type: "string" },
      { name: "email", type: "string" },
      { name: "phone", type: "string" },
      { name: "organization_id", type: "string" },
    ]),
  },
  {
    name: "crm.organization",
    version: 1,
    schema: basicSchema("crm.organization", [
      { name: "name", type: "string" },
      { name: "industry", type: "string" },
      { name: "website", type: "string" },
      { name: "employees", type: "number" },
    ]),
  },
  {
    name: "crm.deal",
    version: 1,
    schema: {
      name: "crm.deal",
      version: 1,
      fields: [
        { name: "name", type: "string" },
        { name: "organization_id", type: "string" },
        { name: "value", type: "number" },
        { name: "currency", type: "string" },
        { name: "stage", type: "string" },
        { name: "owner", type: "string" },
        { name: "close_date", type: "date" },
      ],
      views: {
        list: { columns: ["name", "organization_id", "value", "stage", "close_date"] },
        kanban: { group_by: "stage", card_title: "name", card_subtitle: "value" },
      },
    },
  },
  {
    name: "crm.activity",
    version: 1,
    schema: basicSchema("crm.activity", [
      { name: "subject", type: "string" },
      { name: "kind", type: "string" },
      { name: "due_date", type: "date" },
      { name: "owner", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "crm.quote",
    version: 1,
    schema: basicSchema("crm.quote", [
      { name: "quote_number", type: "string" },
      { name: "deal_id", type: "string" },
      { name: "total", type: "number" },
      { name: "currency", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  // HR
  {
    name: "hr.employee",
    version: 1,
    schema: basicSchema("hr.employee", [
      { name: "name", type: "string" },
      { name: "designation", type: "string" },
      { name: "department", type: "string" },
      { name: "email", type: "string" },
      { name: "reporting_to", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  // Inventory
  {
    name: "inventory.item",
    version: 1,
    schema: basicSchema("inventory.item", [
      { name: "name", type: "string" },
      { name: "sku", type: "string" },
      { name: "barcode", type: "string" },
      { name: "default_price", type: "number" },
      { name: "default_warehouse_id", type: "string" },
    ]),
  },
  // Tasks
  {
    name: "tasks.task",
    version: 1,
    schema: basicSchema("tasks.task", [
      { name: "title", type: "string" },
      { name: "assignee", type: "string" },
      { name: "due_date", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  // Finance
  {
    name: "finance.ar_invoice",
    version: 1,
    schema: basicSchema("finance.ar_invoice", [
      { name: "invoice_number", type: "string" },
      { name: "customer_id", type: "string" },
      { name: "total", type: "number" },
      { name: "currency", type: "string" },
      { name: "due_date", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "finance.ap_bill",
    version: 1,
    schema: basicSchema("finance.ap_bill", [
      { name: "bill_number", type: "string" },
      { name: "supplier_id", type: "string" },
      { name: "total", type: "number" },
      { name: "currency", type: "string" },
      { name: "due_date", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  // Helpdesk
  {
    name: "helpdesk.ticket",
    version: 1,
    schema: basicSchema("helpdesk.ticket", [
      { name: "subject", type: "string" },
      { name: "status", type: "string" },
      { name: "priority", type: "string" },
      { name: "channel", type: "string" },
      { name: "customer_id", type: "string" },
      { name: "assigned_to", type: "string" },
    ]),
  },
  // Projects
  {
    name: "projects.project",
    version: 1,
    schema: basicSchema("projects.project", [
      { name: "name", type: "string" },
      { name: "code", type: "string" },
      { name: "status", type: "string" },
      { name: "start_date", type: "date" },
      { name: "end_date", type: "date" },
    ]),
  },
  {
    name: "projects.milestone",
    version: 1,
    schema: basicSchema("projects.milestone", [
      { name: "name", type: "string" },
      { name: "project_id", type: "string" },
      { name: "due_date", type: "date" },
      { name: "weight", type: "number" },
      { name: "status", type: "string" },
    ]),
  },
  // Sales / POS
  {
    name: "sales.order",
    version: 1,
    schema: basicSchema("sales.order", [
      { name: "order_number", type: "string" },
      { name: "customer_id", type: "string" },
      { name: "order_date", type: "date" },
      { name: "total", type: "number" },
      { name: "currency", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "sales.price_list",
    version: 1,
    schema: basicSchema("sales.price_list", [
      { name: "name", type: "string" },
      { name: "currency", type: "string" },
      { name: "valid_from", type: "date" },
      { name: "valid_until", type: "date" },
      { name: "active", type: "boolean" },
    ]),
  },
  {
    name: "procurement.purchase_order",
    version: 1,
    schema: basicSchema("procurement.purchase_order", [
      { name: "po_number", type: "string" },
      { name: "supplier_id", type: "string" },
      { name: "order_date", type: "date" },
      { name: "total", type: "number" },
      { name: "currency", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "sales.pos_profile",
    version: 1,
    schema: basicSchema("sales.pos_profile", [
      { name: "name", type: "string" },
      { name: "warehouse_id", type: "string" },
      { name: "currency", type: "string" },
    ]),
  },
  {
    name: "sales.pos_invoice",
    version: 1,
    schema: basicSchema("sales.pos_invoice", [
      { name: "profile_id", type: "string" },
      { name: "total", type: "number" },
    ]),
  },
  // LMS
  {
    name: "lms.course",
    version: 1,
    schema: basicSchema("lms.course", [
      { name: "title", type: "string" },
      { name: "code", type: "string" },
      { name: "description", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "lms.module",
    version: 1,
    schema: basicSchema("lms.module", [
      { name: "title", type: "string" },
      { name: "course_id", type: "string" },
      { name: "order", type: "number" },
    ]),
  },
  {
    name: "lms.lesson",
    version: 1,
    schema: basicSchema("lms.lesson", [
      { name: "title", type: "string" },
      { name: "module_id", type: "string" },
      { name: "order", type: "number" },
    ]),
  },
  {
    name: "lms.enrollment",
    version: 1,
    schema: basicSchema("lms.enrollment", [
      { name: "course_id", type: "string" },
      { name: "employee_id", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "lms.progress",
    version: 1,
    schema: basicSchema("lms.progress", [
      { name: "enrollment_id", type: "string" },
      { name: "lesson_id", type: "string" },
      { name: "percent_complete", type: "number" },
    ]),
  },
  // Finance support
  {
    name: "finance.cost_center",
    version: 1,
    schema: basicSchema("finance.cost_center", [
      { name: "code", type: "string" },
      { name: "name", type: "string" },
      { name: "parent_code", type: "string" },
      { name: "active", type: "boolean" },
    ]),
  },
  {
    name: "finance.bank_account",
    version: 1,
    schema: basicSchema("finance.bank_account", [
      { name: "name", type: "string" },
      { name: "currency", type: "string" },
      { name: "account_number", type: "string" },
    ]),
  },
  {
    name: "finance.bank_transaction",
    version: 1,
    schema: basicSchema("finance.bank_transaction", [
      { name: "bank_account_id", type: "string" },
      { name: "value_date", type: "date" },
      { name: "description", type: "string" },
      { name: "amount", type: "number" },
      { name: "status", type: "string" },
    ]),
  },
  // HR — payroll/shift
  {
    name: "hr.salary_component",
    version: 1,
    schema: basicSchema("hr.salary_component", [
      { name: "code", type: "string" },
      { name: "name", type: "string" },
      { name: "type", type: "string" },
      { name: "amount", type: "number" },
    ]),
  },
  {
    name: "hr.salary_structure",
    version: 1,
    schema: basicSchema("hr.salary_structure", [
      { name: "employee_id", type: "string" },
      { name: "base_salary", type: "number" },
      { name: "currency", type: "string" },
    ]),
  },
  {
    name: "hr.pay_run",
    version: 1,
    schema: basicSchema("hr.pay_run", [
      { name: "name", type: "string" },
      { name: "pay_period_start", type: "date" },
      { name: "pay_period_end", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "hr.payslip",
    version: 1,
    schema: basicSchema("hr.payslip", [
      { name: "pay_run_id", type: "string" },
      { name: "employee_id", type: "string" },
      { name: "gross_pay", type: "number" },
      { name: "net_pay", type: "number" },
    ]),
  },
  {
    name: "hr.shift_type",
    version: 1,
    schema: basicSchema("hr.shift_type", [
      { name: "name", type: "string" },
      { name: "start_time", type: "string" },
      { name: "end_time", type: "string" },
      { name: "color", type: "string" },
    ]),
  },
  {
    name: "hr.shift_assignment",
    version: 1,
    schema: basicSchema("hr.shift_assignment", [
      { name: "employee_id", type: "string" },
      { name: "shift_type_id", type: "string" },
      { name: "shift_date", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "hr.leave_request",
    version: 1,
    schema: basicSchema("hr.leave_request", [
      { name: "employee_id", type: "string" },
      { name: "leave_type", type: "string" },
      { name: "start_date", type: "date" },
      { name: "end_date", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "hr.attendance",
    version: 1,
    schema: basicSchema("hr.attendance", [
      { name: "employee_id", type: "string" },
      { name: "attendance_date", type: "date" },
      { name: "status", type: "string" },
    ]),
  },
  {
    name: "hr.expense_claim",
    version: 1,
    schema: basicSchema("hr.expense_claim", [
      { name: "employee_id", type: "string" },
      { name: "amount", type: "number" },
      { name: "category", type: "string" },
      { name: "status", type: "string" },
    ]),
  },
  // LMS quiz/assignment
  {
    name: "lms.quiz",
    version: 1,
    schema: basicSchema("lms.quiz", [
      { name: "title", type: "string" },
      { name: "module_id", type: "string" },
      { name: "passing_score", type: "number" },
    ]),
  },
  {
    name: "lms.assignment",
    version: 1,
    schema: basicSchema("lms.assignment", [
      { name: "title", type: "string" },
      { name: "module_id", type: "string" },
      { name: "due_date", type: "date" },
    ]),
  },
];

const KTYPES_BY_NAME = new Map<string, KType>(KTYPES.map((k) => [k.name, k]));

// --- Records — CRM ----------------------------------------------------

const ORG_IDS = {
  globex: uuid("crm.organization:globex"),
  initech: uuid("crm.organization:initech"),
  hooli: uuid("crm.organization:hooli"),
  umbrella: uuid("crm.organization:umbrella"),
};

const ORGANIZATIONS: KRecord[] = [
  kr("crm.organization", "globex", { name: "Globex Corporation", industry: "Manufacturing", website: "globex.example", employees: 1200 }),
  kr("crm.organization", "initech", { name: "Initech", industry: "Software", website: "initech.example", employees: 220 }),
  kr("crm.organization", "hooli", { name: "Hooli", industry: "Internet", website: "hooli.example", employees: 5400 }),
  kr("crm.organization", "umbrella", { name: "Umbrella Pharma", industry: "Healthcare", website: "umbrella.example", employees: 3100 }),
];

const CONTACTS: KRecord[] = [
  kr("crm.contact", "alice", { name: "Alice Whitman", title: "VP Operations", email: "alice@globex.example", phone: "+1 415 555 0101", organization_id: ORG_IDS.globex }),
  kr("crm.contact", "bob", { name: "Bob Lin", title: "CFO", email: "bob@initech.example", phone: "+1 415 555 0102", organization_id: ORG_IDS.initech }),
  kr("crm.contact", "carol", { name: "Carol Martinez", title: "Procurement Lead", email: "carol@hooli.example", phone: "+1 415 555 0103", organization_id: ORG_IDS.hooli }),
  kr("crm.contact", "david", { name: "David Park", title: "Director of IT", email: "david@umbrella.example", phone: "+1 415 555 0104", organization_id: ORG_IDS.umbrella }),
  kr("crm.contact", "elena", { name: "Elena Roy", title: "Plant Manager", email: "elena@globex.example", phone: "+1 415 555 0105", organization_id: ORG_IDS.globex }),
  kr("crm.contact", "frank", { name: "Frank Osei", title: "Head of HR", email: "frank@initech.example", phone: "+1 415 555 0106", organization_id: ORG_IDS.initech }),
];

const LEADS: KRecord[] = [
  kr("crm.lead", "l1", { name: "Greta Holm", company: "Stark Industries", email: "greta@stark.example", phone: "+1 415 555 0201", source: "Webinar", status: "new", owner: "Avery N." }),
  kr("crm.lead", "l2", { name: "Hassan Ali", company: "Wayne Enterprises", email: "hassan@wayne.example", phone: "+1 415 555 0202", source: "Inbound", status: "contacted", owner: "Avery N." }),
  kr("crm.lead", "l3", { name: "Ingrid Holm", company: "Acme Robotics", email: "ingrid@acmer.example", phone: "+1 415 555 0203", source: "Trade show", status: "qualified", owner: "Sam K." }),
  kr("crm.lead", "l4", { name: "Jacob Steele", company: "Tyrell Corp", email: "jacob@tyrell.example", phone: "+1 415 555 0204", source: "Referral", status: "qualified", owner: "Sam K." }),
  kr("crm.lead", "l5", { name: "Kira Ohan", company: "Soylent Inc", email: "kira@soylent.example", phone: "+1 415 555 0205", source: "Outbound", status: "nurturing", owner: "Mia P." }),
  kr("crm.lead", "l6", { name: "Lewis Conor", company: "Massive Dynamic", email: "lewis@massive.example", phone: "+1 415 555 0206", source: "LinkedIn", status: "new", owner: "Mia P." }),
  kr("crm.lead", "l7", { name: "Mara Jensen", company: "Cyberdyne Systems", email: "mara@cyber.example", phone: "+1 415 555 0207", source: "Website", status: "contacted", owner: "Avery N." }),
  kr("crm.lead", "l8", { name: "Noah Park", company: "Pied Piper", email: "noah@pp.example", phone: "+1 415 555 0208", source: "Inbound", status: "qualified", owner: "Sam K." }),
  kr("crm.lead", "l9", { name: "Olive Ramos", company: "Aperture Labs", email: "olive@aperture.example", phone: "+1 415 555 0209", source: "Webinar", status: "nurturing", owner: "Mia P." }),
  kr("crm.lead", "l10", { name: "Paul Esquivel", company: "Black Mesa", email: "paul@bmesa.example", phone: "+1 415 555 0210", source: "Inbound", status: "new", owner: "Avery N." }),
];

const DEALS: KRecord[] = [
  kr("crm.deal", "d1", { name: "Globex — Annual License", organization_id: ORG_IDS.globex, value: 42000, currency: "USD", stage: "prospecting", owner: "Avery N.", close_date: toCalendarISO(addDays(TODAY, 30)) }),
  kr("crm.deal", "d2", { name: "Initech — Pilot Expansion", organization_id: ORG_IDS.initech, value: 18000, currency: "USD", stage: "qualification", owner: "Sam K.", close_date: toCalendarISO(addDays(TODAY, 25)) }),
  kr("crm.deal", "d3", { name: "Hooli — Enterprise Tier", organization_id: ORG_IDS.hooli, value: 124000, currency: "USD", stage: "proposal", owner: "Mia P.", close_date: toCalendarISO(addDays(TODAY, 14)) }),
  kr("crm.deal", "d4", { name: "Umbrella — POS Rollout", organization_id: ORG_IDS.umbrella, value: 67500, currency: "USD", stage: "negotiation", owner: "Sam K.", close_date: toCalendarISO(addDays(TODAY, 7)) }),
  kr("crm.deal", "d5", { name: "Globex — Q1 Renewal", organization_id: ORG_IDS.globex, value: 36000, currency: "USD", stage: "closed_won", owner: "Avery N.", close_date: toCalendarISO(addDays(TODAY, -3)) }),
];

const ACTIVITIES: KRecord[] = [
  kr("crm.activity", "a1", { subject: "Discovery call — Hooli", kind: "call", due_date: toCalendarISO(TODAY), owner: "Mia P.", status: "open" }),
  kr("crm.activity", "a2", { subject: "Send proposal — Umbrella", kind: "email", due_date: toCalendarISO(addDays(TODAY, 2)), owner: "Sam K.", status: "open" }),
  kr("crm.activity", "a3", { subject: "Onsite demo — Globex", kind: "meeting", due_date: toCalendarISO(addDays(TODAY, 5)), owner: "Avery N.", status: "open" }),
];

const QUOTES: KRecord[] = [
  kr("crm.quote", "q1", { quote_number: "Q-2026-001", deal_id: DEALS[2].id, total: 124000, currency: "USD", status: "sent" }),
  kr("crm.quote", "q2", { quote_number: "Q-2026-002", deal_id: DEALS[3].id, total: 67500, currency: "USD", status: "draft" }),
];

// --- Records — HR (org chart hierarchy) -------------------------------

const EMP_IDS = {
  ceo: uuid("hr.employee:ceo"),
  vpEng: uuid("hr.employee:vp-eng"),
  vpSales: uuid("hr.employee:vp-sales"),
  vpOps: uuid("hr.employee:vp-ops"),
  mgrPlatform: uuid("hr.employee:mgr-platform"),
  mgrSales: uuid("hr.employee:mgr-sales"),
  ic1: uuid("hr.employee:ic1"),
  ic2: uuid("hr.employee:ic2"),
  ic3: uuid("hr.employee:ic3"),
  ic4: uuid("hr.employee:ic4"),
};

function emp(seed: string, data: Record<string, unknown>, idOverride?: string): KRecord {
  const r = kr("hr.employee", seed, { ...data, status: data.status ?? "active" });
  if (idOverride) r.id = idOverride;
  return r;
}

const EMPLOYEES: KRecord[] = [
  emp("ceo", { name: "Diana Reeve", designation: "Chief Executive Officer", department: "Executive", email: "diana@acme.example" }, EMP_IDS.ceo),
  emp("vp-eng", { name: "Mateo Cruz", designation: "VP Engineering", department: "Engineering", email: "mateo@acme.example", reporting_to: EMP_IDS.ceo }, EMP_IDS.vpEng),
  emp("vp-sales", { name: "Priya Banerjee", designation: "VP Sales", department: "Sales", email: "priya@acme.example", reporting_to: EMP_IDS.ceo }, EMP_IDS.vpSales),
  emp("vp-ops", { name: "Chen Wei", designation: "VP Operations", department: "Operations", email: "chen@acme.example", reporting_to: EMP_IDS.ceo }, EMP_IDS.vpOps),
  emp("mgr-platform", { name: "Ravi Iyer", designation: "Platform Manager", department: "Engineering", email: "ravi@acme.example", reporting_to: EMP_IDS.vpEng }, EMP_IDS.mgrPlatform),
  emp("mgr-sales", { name: "Sara Khan", designation: "Sales Manager", department: "Sales", email: "sara@acme.example", reporting_to: EMP_IDS.vpSales }, EMP_IDS.mgrSales),
  emp("ic1", { name: "Avery Nguyen", designation: "Account Executive", department: "Sales", email: "avery@acme.example", reporting_to: EMP_IDS.mgrSales }, EMP_IDS.ic1),
  emp("ic2", { name: "Sam Kowalski", designation: "Account Executive", department: "Sales", email: "samk@acme.example", reporting_to: EMP_IDS.mgrSales }, EMP_IDS.ic2),
  emp("ic3", { name: "Mia Patel", designation: "Senior Engineer", department: "Engineering", email: "mia@acme.example", reporting_to: EMP_IDS.mgrPlatform }, EMP_IDS.ic3),
  emp("ic4", { name: "Theo Adler", designation: "Operations Analyst", department: "Operations", email: "theo@acme.example", reporting_to: EMP_IDS.vpOps }, EMP_IDS.ic4),
];

const LEAVE_REQUESTS: KRecord[] = [
  kr("hr.leave_request", "lr1", { employee_id: EMP_IDS.ic1, leave_type: "vacation", start_date: toCalendarISO(addDays(TODAY, 10)), end_date: toCalendarISO(addDays(TODAY, 15)), status: "pending" }),
  kr("hr.leave_request", "lr2", { employee_id: EMP_IDS.ic3, leave_type: "sick", start_date: toCalendarISO(addDays(TODAY, -2)), end_date: toCalendarISO(addDays(TODAY, -1)), status: "approved" }),
  kr("hr.leave_request", "lr3", { employee_id: EMP_IDS.mgrSales, leave_type: "vacation", start_date: toCalendarISO(addDays(TODAY, 30)), end_date: toCalendarISO(addDays(TODAY, 37)), status: "pending" }),
];

const ATTENDANCE: KRecord[] = [
  kr("hr.attendance", "att1", { employee_id: EMP_IDS.ic1, attendance_date: TODAY_ISO_DATE, status: "present" }),
  kr("hr.attendance", "att2", { employee_id: EMP_IDS.ic2, attendance_date: TODAY_ISO_DATE, status: "present" }),
  kr("hr.attendance", "att3", { employee_id: EMP_IDS.ic3, attendance_date: TODAY_ISO_DATE, status: "present" }),
  kr("hr.attendance", "att4", { employee_id: EMP_IDS.ic4, attendance_date: TODAY_ISO_DATE, status: "half_day" }),
  kr("hr.attendance", "att5", { employee_id: EMP_IDS.mgrSales, attendance_date: TODAY_ISO_DATE, status: "present" }),
];

const EXPENSE_CLAIMS: KRecord[] = [
  kr("hr.expense_claim", "ec1", { employee_id: EMP_IDS.ic1, amount: 245.5, category: "Travel", status: "submitted" }),
  kr("hr.expense_claim", "ec2", { employee_id: EMP_IDS.ic2, amount: 89.0, category: "Meals", status: "approved" }),
];

const SALARY_COMPONENTS: KRecord[] = [
  kr("hr.salary_component", "sc-base", { code: "BASE", name: "Base Salary", type: "earning", amount_type: "fixed", amount: 0, currency: "USD", active: true }),
  kr("hr.salary_component", "sc-bonus", { code: "BONUS", name: "Quarterly Bonus", type: "earning", amount_type: "fixed", amount: 1500, currency: "USD", active: true }),
  kr("hr.salary_component", "sc-tax", { code: "FED_TAX", name: "Federal Tax", type: "deduction", amount_type: "percentage", amount: 22, currency: "USD", active: true }),
  kr("hr.salary_component", "sc-401k", { code: "401K", name: "401(k) Contribution", type: "deduction", amount_type: "percentage", amount: 6, currency: "USD", active: true }),
];

const SALARY_STRUCTURES: KRecord[] = [
  kr("hr.salary_structure", "ss1", { employee_id: EMP_IDS.ceo, effective_from: "2026-01-01", base_salary: 320000, currency: "USD", payment_frequency: "monthly", status: "active" }),
  kr("hr.salary_structure", "ss2", { employee_id: EMP_IDS.vpEng, effective_from: "2026-01-01", base_salary: 240000, currency: "USD", payment_frequency: "monthly", status: "active" }),
  kr("hr.salary_structure", "ss3", { employee_id: EMP_IDS.ic1, effective_from: "2026-01-01", base_salary: 96000, currency: "USD", payment_frequency: "monthly", status: "active" }),
];

const PAY_RUN_ID = uuid("hr.pay_run:r1");
const PAY_RUNS: KRecord[] = [
  {
    ...kr("hr.pay_run", "r1", {
      name: "April 2026 Payroll",
      pay_period_start: "2026-04-01",
      pay_period_end: "2026-04-30",
      department: "All",
      currency: "USD",
      payslip_count: 10,
      total_gross: 154800,
      total_net: 113400,
      status: "posted",
    }),
    id: PAY_RUN_ID,
  },
];

const PAY_RUN_PAYSLIPS: KRecord[] = [
  kr("hr.payslip", "ps1", { pay_run_id: PAY_RUN_ID, employee_id: EMP_IDS.ceo, gross_pay: 26666, total_deductions: 7466, net_pay: 19200, currency: "USD", status: "paid" }),
  kr("hr.payslip", "ps2", { pay_run_id: PAY_RUN_ID, employee_id: EMP_IDS.vpEng, gross_pay: 20000, total_deductions: 5600, net_pay: 14400, currency: "USD", status: "paid" }),
  kr("hr.payslip", "ps3", { pay_run_id: PAY_RUN_ID, employee_id: EMP_IDS.vpSales, gross_pay: 19500, total_deductions: 5460, net_pay: 14040, currency: "USD", status: "paid" }),
  kr("hr.payslip", "ps4", { pay_run_id: PAY_RUN_ID, employee_id: EMP_IDS.ic1, gross_pay: 8000, total_deductions: 2240, net_pay: 5760, currency: "USD", status: "paid" }),
  kr("hr.payslip", "ps5", { pay_run_id: PAY_RUN_ID, employee_id: EMP_IDS.ic2, gross_pay: 8000, total_deductions: 2240, net_pay: 5760, currency: "USD", status: "paid" }),
];

const SHIFT_TYPES: KRecord[] = [
  kr("hr.shift_type", "st-day", { name: "Day", start_time: "09:00", end_time: "17:00", color: "#2563eb", department: "All", active: true }),
  kr("hr.shift_type", "st-eve", { name: "Evening", start_time: "13:00", end_time: "21:00", color: "#9333ea", department: "Operations", active: true }),
  kr("hr.shift_type", "st-night", { name: "Night", start_time: "21:00", end_time: "05:00", color: "#0891b2", department: "Operations", active: true }),
];

function todayPlus(n: number): string {
  return toCalendarISO(addDays(TODAY, n));
}
const SHIFT_ASSIGNMENTS: KRecord[] = [];
{
  // Two weeks of mostly-day shifts so the calendar looks populated.
  const empSubset = [EMP_IDS.ic1, EMP_IDS.ic2, EMP_IDS.ic3, EMP_IDS.ic4, EMP_IDS.mgrSales];
  const dayShift = SHIFT_TYPES[0].id;
  const eveShift = SHIFT_TYPES[1].id;
  let n = 0;
  for (let d = -3; d < 11; d++) {
    for (let i = 0; i < empSubset.length; i++) {
      const stid = (i + d) % 4 === 0 ? eveShift : dayShift;
      n += 1;
      SHIFT_ASSIGNMENTS.push(
        kr("hr.shift_assignment", `sa${n}`, {
          employee_id: empSubset[i],
          shift_type_id: stid,
          shift_date: todayPlus(d),
          status: "scheduled",
        })
      );
    }
  }
}

// --- Records — Inventory ----------------------------------------------

const WAREHOUSE_IDS = {
  main: uuid("inventory.warehouse:main"),
  west: uuid("inventory.warehouse:west"),
};

export const INVENTORY_WAREHOUSES: InventoryWarehouse[] = [
  { tenant_id: DEMO_TENANT_ID, id: WAREHOUSE_IDS.main, code: "MAIN", name: "Main Distribution Center" },
  { tenant_id: DEMO_TENANT_ID, id: WAREHOUSE_IDS.west, code: "WEST", name: "West Coast Hub" },
];

interface DemoItem { id: string; sku: string; name: string; price: number; barcode: string; reorder: string }
const DEMO_ITEMS_RAW: DemoItem[] = [
  { id: uuid("inventory.item:001"), sku: "ACM-001", name: "Acme Widget Mark II", price: 19.99, barcode: "0810000000011", reorder: "20" },
  { id: uuid("inventory.item:002"), sku: "ACM-002", name: "Acme Gadget Pro", price: 49.5, barcode: "0810000000028", reorder: "10" },
  { id: uuid("inventory.item:003"), sku: "ACM-003", name: "Sprocket Assembly", price: 12.0, barcode: "0810000000035", reorder: "50" },
  { id: uuid("inventory.item:004"), sku: "ACM-004", name: "Hex Bolt M8 (100-pack)", price: 8.75, barcode: "0810000000042", reorder: "100" },
  { id: uuid("inventory.item:005"), sku: "ACM-005", name: "Power Adapter 12V", price: 24.0, barcode: "0810000000059", reorder: "30" },
  { id: uuid("inventory.item:006"), sku: "ACM-006", name: "Cable USB-C 2m", price: 9.0, barcode: "0810000000066", reorder: "75" },
  { id: uuid("inventory.item:007"), sku: "ACM-007", name: "Replacement Filter", price: 14.5, barcode: "0810000000073", reorder: "40" },
  { id: uuid("inventory.item:008"), sku: "ACM-008", name: "Service Toolkit", price: 119.0, barcode: "0810000000080", reorder: "5" },
];

export const INVENTORY_ITEMS: InventoryItem[] = DEMO_ITEMS_RAW.map((it) => ({
  tenant_id: DEMO_TENANT_ID,
  id: it.id,
  sku: it.sku,
  name: it.name,
  uom: "EA",
  active: true,
  reorder_level: it.reorder,
}));

const INVENTORY_ITEM_RECORDS: KRecord[] = DEMO_ITEMS_RAW.map((it) =>
  ({
    ...kr("inventory.item", it.sku, {
      name: it.name,
      sku: it.sku,
      barcode: it.barcode,
      default_price: it.price,
      default_warehouse_id: WAREHOUSE_IDS.main,
    }),
    id: it.id,
  })
);

export const STOCK_LEVELS: StockLevel[] = [];
{
  const stockMain = [12, 4, 320, 1500, 80, 200, 15, 9];
  const stockWest = [5, 0, 110, 800, 25, 60, 22, 2];
  DEMO_ITEMS_RAW.forEach((it, i) => {
    STOCK_LEVELS.push({ tenant_id: DEMO_TENANT_ID, item_id: it.id, warehouse_id: WAREHOUSE_IDS.main, qty: String(stockMain[i]) });
    STOCK_LEVELS.push({ tenant_id: DEMO_TENANT_ID, item_id: it.id, warehouse_id: WAREHOUSE_IDS.west, qty: String(stockWest[i]) });
  });
}

export const INVENTORY_VALUATION: InventoryValuationReport = {
  as_of: TODAY_ISO_DATE,
  rows: DEMO_ITEMS_RAW.map((it, i) => {
    const stockMain = [12, 4, 320, 1500, 80, 200, 15, 9];
    const stockWest = [5, 0, 110, 800, 25, 60, 22, 2];
    const qty = stockMain[i] + stockWest[i];
    const value = qty * it.price * 0.6; // 40% margin → cost ~ 0.6 of price
    return {
      item_id: it.id,
      sku: it.sku,
      name: it.name,
      qty: String(qty),
      value_cost: value.toFixed(2),
    };
  }),
  total_value: "0.00",
};
INVENTORY_VALUATION.total_value = INVENTORY_VALUATION.rows
  .reduce((s, r) => s + Number(r.value_cost), 0)
  .toFixed(2);

// --- Records — Helpdesk ----------------------------------------------

const TICKETS: KRecord[] = [
  kr("helpdesk.ticket", "t1", { subject: "Login fails after SSO migration", status: "open", priority: "high", channel: "email", customer_id: ORG_IDS.globex, assigned_to: EMP_IDS.ic3, sla_resolution_by: addDays(TODAY, 1).toISOString() }),
  kr("helpdesk.ticket", "t2", { subject: "Inventory sync delay", status: "in_progress", priority: "medium", channel: "portal", customer_id: ORG_IDS.initech, assigned_to: EMP_IDS.ic3, sla_resolution_by: addDays(TODAY, 2).toISOString() }),
  kr("helpdesk.ticket", "t3", { subject: "Need invoice PDF reissued", status: "waiting", priority: "low", channel: "portal", customer_id: ORG_IDS.hooli, sla_resolution_by: addDays(TODAY, 3).toISOString() }),
  kr("helpdesk.ticket", "t4", { subject: "POS terminal offline", status: "open", priority: "urgent", channel: "phone", customer_id: ORG_IDS.umbrella, assigned_to: EMP_IDS.mgrPlatform, sla_resolution_by: addDays(TODAY, -1).toISOString() }),
  kr("helpdesk.ticket", "t5", { subject: "Question about retention policy", status: "resolved", priority: "low", channel: "email", customer_id: ORG_IDS.globex }),
  kr("helpdesk.ticket", "t6", { subject: "Cannot run trial balance for Q1", status: "in_progress", priority: "high", channel: "portal", customer_id: ORG_IDS.initech, assigned_to: EMP_IDS.mgrPlatform, sla_resolution_by: addDays(TODAY, 1).toISOString() }),
];

export const SLA_POLICIES: SLAPolicy[] = [
  { tenant_id: DEMO_TENANT_ID, id: uuid("sla:low"), name: "Low priority", priority: "low", response_minutes: 1440, resolution_minutes: 7200, active: true, created_by: null, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: uuid("sla:medium"), name: "Standard", priority: "medium", response_minutes: 240, resolution_minutes: 2880, active: true, created_by: null, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: uuid("sla:high"), name: "Premium", priority: "high", response_minutes: 60, resolution_minutes: 480, active: true, created_by: null, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
];

// --- Records — Projects (fixed Q2 2026 dates for stable Gantt) -------

const PROJECTS: KRecord[] = [
  kr("projects.project", "p1", { name: "ERP Migration", code: "PROJ-001", status: "active", start_date: "2026-04-01", end_date: "2026-05-31" }),
  kr("projects.project", "p2", { name: "Warehouse Expansion", code: "PROJ-002", status: "planning", start_date: "2026-04-15", end_date: "2026-06-15" }),
  kr("projects.project", "p3", { name: "POS Hardware Rollout", code: "PROJ-003", status: "active", start_date: "2026-04-08", end_date: "2026-05-20" }),
];

const MILESTONES: KRecord[] = [
  kr("projects.milestone", "m1", { name: "Data dictionary signed off", project_id: PROJECTS[0].id, due_date: "2026-04-15", weight: 1, status: "completed" }),
  kr("projects.milestone", "m2", { name: "Cutover dry-run", project_id: PROJECTS[0].id, due_date: "2026-05-10", weight: 1, status: "in_progress" }),
  kr("projects.milestone", "m3", { name: "Go-live", project_id: PROJECTS[0].id, due_date: "2026-05-31", weight: 2, status: "planned" }),
  kr("projects.milestone", "m4", { name: "Site selected", project_id: PROJECTS[1].id, due_date: "2026-04-25", weight: 1, status: "completed" }),
  kr("projects.milestone", "m5", { name: "Permits filed", project_id: PROJECTS[1].id, due_date: "2026-05-15", weight: 1, status: "in_progress" }),
  kr("projects.milestone", "m6", { name: "Construction kickoff", project_id: PROJECTS[1].id, due_date: "2026-06-15", weight: 2, status: "planned" }),
  kr("projects.milestone", "m7", { name: "Pilot store live", project_id: PROJECTS[2].id, due_date: "2026-04-22", weight: 1, status: "completed" }),
  kr("projects.milestone", "m8", { name: "All stores cut over", project_id: PROJECTS[2].id, due_date: "2026-05-20", weight: 2, status: "in_progress" }),
];

// --- Records — Sales / Procurement / POS ------------------------------

const SALES_ORDERS: KRecord[] = [
  kr("sales.order", "so1", { order_number: "SO-2026-0001", customer_id: ORG_IDS.globex, order_date: todayPlus(-12), total: 4200, currency: "USD", status: "draft" }),
  kr("sales.order", "so2", { order_number: "SO-2026-0002", customer_id: ORG_IDS.initech, order_date: todayPlus(-9), total: 9800, currency: "USD", status: "confirmed" }),
  kr("sales.order", "so3", { order_number: "SO-2026-0003", customer_id: ORG_IDS.hooli, order_date: todayPlus(-3), total: 25400, currency: "USD", status: "fulfilled" }),
];

const PURCHASE_ORDERS: KRecord[] = [
  kr("procurement.purchase_order", "po1", { po_number: "PO-2026-0001", supplier_id: ORG_IDS.umbrella, order_date: todayPlus(-10), total: 11200, currency: "USD", status: "draft" }),
  kr("procurement.purchase_order", "po2", { po_number: "PO-2026-0002", supplier_id: ORG_IDS.globex, order_date: todayPlus(-4), total: 6700, currency: "USD", status: "confirmed" }),
];

const PRICE_LISTS: KRecord[] = [
  kr("sales.price_list", "pl1", {
    name: "Default — USD Retail",
    currency: "USD",
    valid_from: "2026-01-01",
    valid_until: "2026-12-31",
    active: true,
    items: DEMO_ITEMS_RAW.slice(0, 5).map((it) => ({
      item_id: it.id,
      price: it.price,
      discount_percent: 0,
      min_qty: 1,
    })),
  }),
  kr("sales.price_list", "pl2", {
    name: "Wholesale — USD",
    currency: "USD",
    valid_from: "2026-01-01",
    valid_until: "2026-12-31",
    active: true,
    items: DEMO_ITEMS_RAW.slice(0, 5).map((it) => ({
      item_id: it.id,
      price: Number((it.price * 0.85).toFixed(2)),
      discount_percent: 15,
      min_qty: 50,
    })),
  }),
];

const POS_PROFILES: KRecord[] = [
  kr("sales.pos_profile", "pp1", {
    name: "Acme Flagship Store",
    warehouse_id: WAREHOUSE_IDS.main,
    currency: "USD",
    default_customer_id: ORG_IDS.globex,
  }),
];

// --- Records — LMS ----------------------------------------------------

const COURSE_IDS = {
  c1: uuid("lms.course:onboarding"),
  c2: uuid("lms.course:compliance"),
  c3: uuid("lms.course:product"),
};

const COURSES: KRecord[] = [
  { ...kr("lms.course", "onboarding", { title: "Acme New Hire Onboarding", code: "ONB-101", description: "Two-week onboarding curriculum for new employees", status: "published" }), id: COURSE_IDS.c1 },
  { ...kr("lms.course", "compliance", { title: "Annual Compliance Refresher", code: "COMP-2026", description: "FY2026 annual compliance & security training", status: "published" }), id: COURSE_IDS.c2 },
  { ...kr("lms.course", "product", { title: "Product Mastery — POS Module", code: "PROD-POS", description: "Deep dive into the POS module for support engineers", status: "draft" }), id: COURSE_IDS.c3 },
];

const MODULE_IDS = [
  uuid("lms.module:m1"), uuid("lms.module:m2"), uuid("lms.module:m3"),
  uuid("lms.module:m4"), uuid("lms.module:m5"), uuid("lms.module:m6"),
];

const MODULES: KRecord[] = [
  { ...kr("lms.module", "m1", { title: "Welcome to Acme", course_id: COURSE_IDS.c1, order: 1 }), id: MODULE_IDS[0] },
  { ...kr("lms.module", "m2", { title: "Tools & Systems", course_id: COURSE_IDS.c1, order: 2 }), id: MODULE_IDS[1] },
  { ...kr("lms.module", "m3", { title: "Information Security", course_id: COURSE_IDS.c2, order: 1 }), id: MODULE_IDS[2] },
  { ...kr("lms.module", "m4", { title: "Anti-Harassment", course_id: COURSE_IDS.c2, order: 2 }), id: MODULE_IDS[3] },
  { ...kr("lms.module", "m5", { title: "POS Architecture", course_id: COURSE_IDS.c3, order: 1 }), id: MODULE_IDS[4] },
  { ...kr("lms.module", "m6", { title: "Offline Queue Internals", course_id: COURSE_IDS.c3, order: 2 }), id: MODULE_IDS[5] },
];

const LESSONS: KRecord[] = [
  kr("lms.lesson", "l1", { title: "Welcome video", module_id: MODULE_IDS[0], order: 1, duration_minutes: 5 }),
  kr("lms.lesson", "l2", { title: "Company values", module_id: MODULE_IDS[0], order: 2, duration_minutes: 12 }),
  kr("lms.lesson", "l3", { title: "Setting up your laptop", module_id: MODULE_IDS[1], order: 1, duration_minutes: 20 }),
  kr("lms.lesson", "l4", { title: "Email & calendar", module_id: MODULE_IDS[1], order: 2, duration_minutes: 10 }),
  kr("lms.lesson", "l5", { title: "Phishing awareness", module_id: MODULE_IDS[2], order: 1, duration_minutes: 25 }),
  kr("lms.lesson", "l6", { title: "Data classification", module_id: MODULE_IDS[2], order: 2, duration_minutes: 18 }),
  kr("lms.lesson", "l7", { title: "Code of conduct", module_id: MODULE_IDS[3], order: 1, duration_minutes: 15 }),
  kr("lms.lesson", "l8", { title: "Reporting concerns", module_id: MODULE_IDS[3], order: 2, duration_minutes: 12 }),
  kr("lms.lesson", "l9", { title: "POS data flow", module_id: MODULE_IDS[4], order: 1, duration_minutes: 22 }),
  kr("lms.lesson", "l10", { title: "Offline queue replay", module_id: MODULE_IDS[5], order: 1, duration_minutes: 30 }),
];

const ENROLLMENT_IDS = [uuid("lms.enroll:e1"), uuid("lms.enroll:e2"), uuid("lms.enroll:e3"), uuid("lms.enroll:e4")];
const ENROLLMENTS: KRecord[] = [
  { ...kr("lms.enrollment", "e1", { course_id: COURSE_IDS.c1, employee_id: EMP_IDS.ic1, status: "in_progress", enrolled_at: LAST_WEEK_ISO }), id: ENROLLMENT_IDS[0] },
  { ...kr("lms.enrollment", "e2", { course_id: COURSE_IDS.c2, employee_id: EMP_IDS.ic1, status: "in_progress", enrolled_at: LAST_WEEK_ISO }), id: ENROLLMENT_IDS[1] },
  { ...kr("lms.enrollment", "e3", { course_id: COURSE_IDS.c2, employee_id: EMP_IDS.ic2, status: "completed", enrolled_at: LAST_MONTH_ISO }), id: ENROLLMENT_IDS[2] },
  { ...kr("lms.enrollment", "e4", { course_id: COURSE_IDS.c2, employee_id: EMP_IDS.ic3, status: "in_progress", enrolled_at: LAST_WEEK_ISO }), id: ENROLLMENT_IDS[3] },
];

const PROGRESS: KRecord[] = [];
{
  const progressMatrix: Array<{ enr: number; lessons: Array<[number, number]> }> = [
    { enr: 0, lessons: [[0, 100], [1, 100], [2, 50]] },
    { enr: 1, lessons: [[4, 100], [5, 60]] },
    { enr: 2, lessons: [[4, 100], [5, 100], [6, 100], [7, 100]] },
    { enr: 3, lessons: [[4, 100], [5, 40]] },
  ];
  let n = 0;
  for (const m of progressMatrix) {
    for (const [li, pct] of m.lessons) {
      n += 1;
      PROGRESS.push(
        kr("lms.progress", `pr${n}`, {
          enrollment_id: ENROLLMENT_IDS[m.enr],
          lesson_id: LESSONS[li].id,
          percent_complete: pct,
          completed_at: pct === 100 ? LAST_WEEK_ISO : null,
        })
      );
    }
  }
}

const QUIZZES: KRecord[] = [
  kr("lms.quiz", "qz1", { title: "Information Security Quiz", module_id: MODULE_IDS[2], passing_score: 80 }),
  kr("lms.quiz", "qz2", { title: "Anti-Harassment Acknowledgement", module_id: MODULE_IDS[3], passing_score: 100 }),
];

const ASSIGNMENTS: KRecord[] = [
  kr("lms.assignment", "as1", { title: "POS Architecture Lab", module_id: MODULE_IDS[4], due_date: todayPlus(14) }),
];

// --- Records — Finance support (cost centers, banks) -----------------

const COST_CENTERS: KRecord[] = [
  kr("finance.cost_center", "cc-eng", { code: "ENG", name: "Engineering", active: true }),
  kr("finance.cost_center", "cc-sales", { code: "SALES", name: "Sales & Marketing", active: true }),
];

const BANK_ACCOUNT_ID = uuid("finance.bank_account:main");
const BANK_SAVINGS_ID = uuid("finance.bank_account:savings");
const BANK_ACCOUNTS: KRecord[] = [
  { ...kr("finance.bank_account", "main", { name: "Acme Operating — USD", currency: "USD", account_number: "****4137" }), id: BANK_ACCOUNT_ID },
  { ...kr("finance.bank_account", "savings", { name: "Acme Savings — USD", currency: "USD", account_number: "****9921" }), id: BANK_SAVINGS_ID },
];

// Transaction ids are derived the same way kr() derives them so the
// suggestion fixtures below can reference specific bank lines.
const BT3_ID = uuid("finance.bank_transaction:bt3");
const BT6_ID = uuid("finance.bank_transaction:bt6");

const BANK_TXNS: KRecord[] = [
  kr("finance.bank_transaction", "bt1", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-7), description: "Wire — Globex AR-2026-0001", amount: 4200, currency: "USD", status: "matched", matched_entry_id: "je1" }),
  kr("finance.bank_transaction", "bt2", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-5), description: "ACH — Initech AR-2026-0002", amount: 9800, currency: "USD", status: "matched", matched_entry_id: "je2" }),
  kr("finance.bank_transaction", "bt3", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-3), description: "Card — POS daily settlement", amount: 1240.5, currency: "USD", status: "unreconciled" }),
  kr("finance.bank_transaction", "bt4", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-2), description: "Bill payment — Umbrella PO-2026-0001", amount: -11200, currency: "USD", status: "matched", matched_entry_id: "je3" }),
  kr("finance.bank_transaction", "bt5", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-1), description: "Bank fee", amount: -25, currency: "USD", status: "ignored" }),
  kr("finance.bank_transaction", "bt6", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-1), description: "ACH — Hooli AR-2026-0003", amount: 31000, currency: "USD", status: "unreconciled" }),
  // Inter-account transfer pair (auto-flagged by the backend detector):
  // money out of Operating, the equal-and-opposite leg into Savings.
  kr("finance.bank_transaction", "bt7", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-2), description: "Transfer to savings", amount: -15000, currency: "USD", status: "transfer" }),
  kr("finance.bank_transaction", "bt8", { bank_account_id: BANK_SAVINGS_ID, value_date: todayPlus(-2), description: "Transfer from operating", amount: 15000, currency: "USD", status: "transfer" }),
];

// Smart-matcher review queue: confidence-scored candidate journal
// entries per unreconciled bank line, highest confidence first. bt6
// carries two candidates so the "find alternative" path is exercised.
const BANK_FEED_SUGGESTIONS: BankFeedSuggestion[] = [
  { id: uuid("bfs:s1"), tenant_id: DEMO_TENANT_ID, transaction_id: BT6_ID, journal_entry_id: uuid("je:hooli-ar"), confidence: 0.97, match_reason: "exact amount, same-day, learned counterparty", status: "suggested", created_at: NOW_ISO },
  { id: uuid("bfs:s2"), tenant_id: DEMO_TENANT_ID, transaction_id: BT6_ID, journal_entry_id: uuid("je:hooli-deposit"), confidence: 0.62, match_reason: "amount within tolerance, 3 days apart", status: "suggested", created_at: NOW_ISO },
  { id: uuid("bfs:s3"), tenant_id: DEMO_TENANT_ID, transaction_id: BT3_ID, journal_entry_id: uuid("je:pos-settlement"), confidence: 0.74, match_reason: "amount within tolerance, description match", status: "suggested", created_at: NOW_ISO },
];

const BANK_FEED_RULES: BankFeedRule[] = [
  { id: uuid("bfr:r1"), priority: 10, condition_type: "description_contains", condition_value: "POS daily settlement", target_account_code: "1100", target_cost_center: "SALES", auto_approve: false, bank_account_id: BANK_ACCOUNT_ID, enabled: true, created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { id: uuid("bfr:r2"), priority: 20, condition_type: "counterparty_equals", condition_value: "Hooli", target_account_code: "1020", target_cost_center: "", auto_approve: true, bank_account_id: null, enabled: true, created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { id: uuid("bfr:r3"), priority: 30, condition_type: "description_contains", condition_value: "Bank fee", target_account_code: "6200", target_cost_center: "", auto_approve: true, bank_account_id: null, enabled: false, created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
];

export const BANK_FEED_SUGGESTIONS_FIXTURE = BANK_FEED_SUGGESTIONS;
export const BANK_FEED_RULES_FIXTURE = BANK_FEED_RULES;

// --- Records — Finance: Invoices / Bills ------------------------------

const AR_INVOICES: KRecord[] = [
  kr("finance.ar_invoice", "ar1", { invoice_number: "AR-2026-0001", customer_id: ORG_IDS.globex, total: 4200, currency: "USD", due_date: todayPlus(-2), status: "posted" }),
  kr("finance.ar_invoice", "ar2", { invoice_number: "AR-2026-0002", customer_id: ORG_IDS.initech, total: 9800, currency: "USD", due_date: todayPlus(7), status: "draft" }),
  kr("finance.ar_invoice", "ar3", { invoice_number: "AR-2026-0003", customer_id: ORG_IDS.hooli, total: 31000, currency: "USD", due_date: todayPlus(14), status: "posted" }),
];

const AP_BILLS: KRecord[] = [
  kr("finance.ap_bill", "ap1", { bill_number: "AP-2026-0001", supplier_id: ORG_IDS.umbrella, total: 11200, currency: "USD", due_date: todayPlus(5), status: "posted" }),
  kr("finance.ap_bill", "ap2", { bill_number: "AP-2026-0002", supplier_id: ORG_IDS.globex, total: 6700, currency: "USD", due_date: todayPlus(20), status: "draft" }),
];

// --- Finance: chart of accounts, journal entries, reports -----------

interface AccountSeed { code: string; name: string; type: FinanceAccount["type"]; parent?: string }
const ACCOUNT_SEEDS: AccountSeed[] = [
  { code: "1000", name: "Assets", type: "asset" },
  { code: "1010", name: "Cash & Equivalents", type: "asset", parent: "1000" },
  { code: "1020", name: "Accounts Receivable", type: "asset", parent: "1000" },
  { code: "1030", name: "Inventory", type: "asset", parent: "1000" },
  { code: "1040", name: "Prepaid Expenses", type: "asset", parent: "1000" },
  { code: "1500", name: "Property & Equipment", type: "asset", parent: "1000" },
  { code: "2000", name: "Liabilities", type: "liability" },
  { code: "2010", name: "Accounts Payable", type: "liability", parent: "2000" },
  { code: "2020", name: "Accrued Expenses", type: "liability", parent: "2000" },
  { code: "2030", name: "Sales Tax Payable", type: "liability", parent: "2000" },
  { code: "2500", name: "Long-Term Debt", type: "liability", parent: "2000" },
  { code: "3000", name: "Equity", type: "equity" },
  { code: "3010", name: "Common Stock", type: "equity", parent: "3000" },
  { code: "3020", name: "Retained Earnings", type: "equity", parent: "3000" },
  { code: "4000", name: "Revenue", type: "revenue" },
  { code: "4010", name: "Product Revenue", type: "revenue", parent: "4000" },
  { code: "4020", name: "Service Revenue", type: "revenue", parent: "4000" },
  { code: "5000", name: "Cost of Goods Sold", type: "expense" },
  { code: "6000", name: "Operating Expenses", type: "expense" },
  { code: "6010", name: "Salaries & Wages", type: "expense", parent: "6000" },
  { code: "6020", name: "Rent", type: "expense", parent: "6000" },
  { code: "6030", name: "Marketing", type: "expense", parent: "6000" },
];

export const FINANCE_ACCOUNTS: FinanceAccount[] = ACCOUNT_SEEDS.map((s) => ({
  tenant_id: DEMO_TENANT_ID,
  code: s.code,
  name: s.name,
  type: s.type,
  parent_code: s.parent,
  active: true,
}));

// --- Finance: budgets (Phase N5) -------------------------------------

const BUDGET_IDS = {
  ops2026: uuid("finance.budget:ops-2026"),
  sales2026: uuid("finance.budget:sales-2026"),
  ops2025: uuid("finance.budget:ops-2025"),
  draft2027: uuid("finance.budget:draft-2027"),
};

export const BUDGETS: Budget[] = [
  { tenant_id: DEMO_TENANT_ID, id: BUDGET_IDS.ops2026, name: "FY2026 Operating Plan", fiscal_year: 2026, status: "active", cost_center: "ENG", notes: "Company-wide operating budget for the 2026 fiscal year.", variance_threshold: "10", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BUDGET_IDS.sales2026, name: "FY2026 Sales & Marketing", fiscal_year: 2026, status: "active", cost_center: "SALES", notes: "Go-to-market and demand-generation spend plan.", variance_threshold: "15", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BUDGET_IDS.ops2025, name: "FY2025 Operating Plan", fiscal_year: 2025, status: "closed", cost_center: "ENG", notes: "Prior-year plan, closed for reporting.", variance_threshold: "10", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BUDGET_IDS.draft2027, name: "FY2027 Draft Plan", fiscal_year: 2027, status: "draft", notes: "Early draft pending leadership review.", variance_threshold: null, created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO },
];

function budgetLine(budgetId: string, seed: string, account_code: string, cost_center: string | undefined, monthly: number): BudgetLine {
  return {
    tenant_id: DEMO_TENANT_ID,
    id: uuid(`finance.budget_line:${seed}`),
    budget_id: budgetId,
    account_code,
    cost_center,
    months: Array(12).fill(monthly.toFixed(2)),
    annual_total: (monthly * 12).toFixed(2),
    created_at: LAST_MONTH_ISO,
    updated_at: NOW_ISO,
  };
}

export const BUDGET_LINES_BY_ID: Record<string, BudgetLine[]> = {
  [BUDGET_IDS.ops2026]: [
    budgetLine(BUDGET_IDS.ops2026, "ops26-salaries", "6010", "ENG", 62000),
    budgetLine(BUDGET_IDS.ops2026, "ops26-rent", "6020", undefined, 14000),
    budgetLine(BUDGET_IDS.ops2026, "ops26-marketing", "6030", "SALES", 9500),
    budgetLine(BUDGET_IDS.ops2026, "ops26-cogs", "5000", undefined, 38000),
    budgetLine(BUDGET_IDS.ops2026, "ops26-revenue", "4010", "SALES", 165000),
  ],
  [BUDGET_IDS.sales2026]: [
    budgetLine(BUDGET_IDS.sales2026, "sales26-marketing", "6030", "SALES", 22000),
    budgetLine(BUDGET_IDS.sales2026, "sales26-salaries", "6010", "SALES", 41000),
    budgetLine(BUDGET_IDS.sales2026, "sales26-revenue", "4020", "SALES", 88000),
  ],
  [BUDGET_IDS.ops2025]: [
    budgetLine(BUDGET_IDS.ops2025, "ops25-salaries", "6010", "ENG", 56000),
    budgetLine(BUDGET_IDS.ops2025, "ops25-rent", "6020", undefined, 13000),
    budgetLine(BUDGET_IDS.ops2025, "ops25-revenue", "4010", "SALES", 150000),
  ],
  [BUDGET_IDS.draft2027]: [],
};

// Deterministic per-account "actual vs plan" factors so the variance
// dashboard renders believable favourable / unfavourable rows.
const VARIANCE_FACTORS: Record<string, number> = {
  "6010": 1.04, // salaries slightly over plan
  "6020": 0.98, // rent slightly under plan
  "6030": 1.18, // marketing over plan
  "5000": 0.93, // COGS under plan
  "4010": 1.07, // product revenue over plan
  "4020": 0.95, // service revenue under plan
};

// buildBudgetVariance derives a believable plan-vs-actual report from a
// budget's lines. The backend sign-normalises so a positive variance
// always means actual exceeded plan; favourability then depends on the
// account's chart-of-accounts type (revenue over = good, expense over =
// bad).
export function buildBudgetVariance(
  budget: Budget,
  lines: BudgetLine[]
): BudgetVarianceReport {
  let totalBudgeted = 0;
  let totalActual = 0;
  let totalFav = 0;
  let totalUnfav = 0;
  const rows: BudgetVarianceRow[] = lines.map((l) => {
    const acct = FINANCE_ACCOUNTS.find((a) => a.code === l.account_code);
    const accountType = (acct?.type ?? "") as BudgetVarianceAccountType;
    const budgeted = Number(l.annual_total);
    const factor = VARIANCE_FACTORS[l.account_code] ?? 1;
    const actual = Math.round(budgeted * factor * 100) / 100;
    const variance = Math.round((actual - budgeted) * 100) / 100;
    const variancePct = budgeted ? variance / budgeted : 0;
    const favourable =
      accountType === "revenue" ? variance >= 0 : variance <= 0;
    totalBudgeted += budgeted;
    totalActual += actual;
    if (favourable) totalFav += Math.abs(variance);
    else totalUnfav += Math.abs(variance);
    return {
      budget_id: budget.id,
      account_code: l.account_code,
      account_name: acct?.name,
      account_type: accountType,
      cost_center: l.cost_center,
      period: String(budget.fiscal_year),
      budgeted: budgeted.toFixed(2),
      actual: actual.toFixed(2),
      variance: variance.toFixed(2),
      variance_pct: variancePct.toFixed(4),
      favourable,
      unplanned: false,
    };
  });
  return {
    tenant_id: DEMO_TENANT_ID,
    budget_id: budget.id,
    budget_name: budget.name,
    fiscal_year: budget.fiscal_year,
    from: `${budget.fiscal_year}-01-01`,
    to: `${budget.fiscal_year}-12-31`,
    rows,
    total_budgeted: totalBudgeted.toFixed(2),
    total_actual: totalActual.toFixed(2),
    total_variance: (totalActual - totalBudgeted).toFixed(2),
    total_favourable_variance: totalFav.toFixed(2),
    total_unfavourable_variance: totalUnfav.toFixed(2),
  };
}

// --- Manufacturing: work centers, BOMs, routings, work orders, --------
// job cards, capacity, MRP, subcontracting (Stream 2 / Batch-3) -------
//
// Finished goods and components are drawn from the shared inventory
// catalogue above so item labels resolve everywhere a manufacturing
// page joins to listInventoryItems().

const MFG_ITEMS = {
  widget: INVENTORY_ITEMS[0].id, // ACM-001 Acme Widget Mark II
  gadget: INVENTORY_ITEMS[1].id, // ACM-002 Acme Gadget Pro
  sprocket: INVENTORY_ITEMS[2].id, // ACM-003 Sprocket Assembly
  bolt: INVENTORY_ITEMS[3].id, // ACM-004 Hex Bolt M8 (100-pack)
  adapter: INVENTORY_ITEMS[4].id, // ACM-005 Power Adapter 12V
  cable: INVENTORY_ITEMS[5].id, // ACM-006 Cable USB-C 2m
  filter: INVENTORY_ITEMS[6].id, // ACM-007 Replacement Filter
};

const WC_IDS = {
  assembly: uuid("mfg.work_center:assembly"),
  machining: uuid("mfg.work_center:machining"),
  finishing: uuid("mfg.work_center:finishing"),
  packaging: uuid("mfg.work_center:packaging"),
};

export const WORK_CENTERS: WorkCenter[] = [
  { tenant_id: DEMO_TENANT_ID, id: WC_IDS.assembly, name: "Assembly Line A", capacity_per_hour: "30", operating_hours_per_day: "8", efficiency_percent: "92", status: "active", notes: "Primary final-assembly line.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WC_IDS.machining, name: "CNC Machining Cell", capacity_per_hour: "12", operating_hours_per_day: "16", efficiency_percent: "88", status: "active", notes: "Two-shift CNC cell for machined parts.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WC_IDS.finishing, name: "Finishing & QA", capacity_per_hour: "20", operating_hours_per_day: "8", efficiency_percent: "95", status: "active", notes: "Surface finishing and final inspection.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WC_IDS.packaging, name: "Packaging Station", capacity_per_hour: "60", operating_hours_per_day: "8", efficiency_percent: "97", status: "maintenance", notes: "Offline for conveyor maintenance this week.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
];

const BOM_IDS = {
  widgetV2: uuid("mfg.bom:widget-v2"),
  widgetV1: uuid("mfg.bom:widget-v1"),
  gadgetV1: uuid("mfg.bom:gadget-v1"),
  sprocketV1: uuid("mfg.bom:sprocket-v1"),
};

function bomComponent(bomId: string, componentItemId: string, qty: string, sort: number, scrap?: string): BOMComponent {
  return { bom_id: bomId, component_item_id: componentItemId, qty, uom: "EA", scrap_percent: scrap ?? null, sort_order: sort };
}

export const BOMS: BOM[] = [
  {
    tenant_id: DEMO_TENANT_ID, id: BOM_IDS.widgetV2, item_id: MFG_ITEMS.widget, version: "v2", status: "active", output_qty: "1", uom: "EA",
    notes: "Current production recipe for the Widget Mark II.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    components: [
      bomComponent(BOM_IDS.widgetV2, MFG_ITEMS.sprocket, "2", 1),
      bomComponent(BOM_IDS.widgetV2, MFG_ITEMS.bolt, "4", 2, "5"),
      bomComponent(BOM_IDS.widgetV2, MFG_ITEMS.cable, "1", 3),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: BOM_IDS.widgetV1, item_id: MFG_ITEMS.widget, version: "v1", status: "obsolete", output_qty: "1", uom: "EA",
    notes: "Superseded by v2; kept for historical orders.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO,
    components: [
      bomComponent(BOM_IDS.widgetV1, MFG_ITEMS.sprocket, "2", 1),
      bomComponent(BOM_IDS.widgetV1, MFG_ITEMS.bolt, "6", 2),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: BOM_IDS.gadgetV1, item_id: MFG_ITEMS.gadget, version: "v1", status: "active", output_qty: "1", uom: "EA",
    notes: "Gadget Pro standard build.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    components: [
      bomComponent(BOM_IDS.gadgetV1, MFG_ITEMS.adapter, "1", 1),
      bomComponent(BOM_IDS.gadgetV1, MFG_ITEMS.cable, "1", 2),
      bomComponent(BOM_IDS.gadgetV1, MFG_ITEMS.bolt, "6", 3, "5"),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: BOM_IDS.sprocketV1, item_id: MFG_ITEMS.sprocket, version: "v1", status: "active", output_qty: "1", uom: "EA",
    notes: "Sub-assembly consumed by the Widget Mark II recipe.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    components: [bomComponent(BOM_IDS.sprocketV1, MFG_ITEMS.bolt, "8", 1)],
  },
];

const ROUTING_IDS = {
  widget: uuid("mfg.routing:widget-v1"),
  gadget: uuid("mfg.routing:gadget-v1"),
  sprocket: uuid("mfg.routing:sprocket-v1"),
};

function routingOp(routingId: string, seq: number, name: string, wc: string, setup: string, cycle: string, description?: string): RoutingOperation {
  return { routing_id: routingId, sequence: seq, operation_name: name, work_center_id: wc, setup_time_minutes: setup, cycle_time_minutes: cycle, description };
}

export const ROUTINGS: Routing[] = [
  {
    tenant_id: DEMO_TENANT_ID, id: ROUTING_IDS.widget, item_id: MFG_ITEMS.widget, version: "v1", status: "active",
    notes: "Three-stage build for the Widget Mark II.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    operations: [
      routingOp(ROUTING_IDS.widget, 1, "Machine sprockets", WC_IDS.machining, "30", "4", "Rough + finish machining."),
      routingOp(ROUTING_IDS.widget, 2, "Assemble widget", WC_IDS.assembly, "15", "6", "Press-fit and fasten."),
      routingOp(ROUTING_IDS.widget, 3, "Finish & inspect", WC_IDS.finishing, "10", "3", "Deburr, clean, QA."),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: ROUTING_IDS.gadget, item_id: MFG_ITEMS.gadget, version: "v1", status: "active",
    notes: "Gadget Pro assembly and pack-out.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    operations: [
      routingOp(ROUTING_IDS.gadget, 1, "Assemble gadget", WC_IDS.assembly, "20", "5", "Wire adapter and cable."),
      routingOp(ROUTING_IDS.gadget, 2, "Pack & label", WC_IDS.packaging, "5", "1", "Box, label, palletise."),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: ROUTING_IDS.sprocket, item_id: MFG_ITEMS.sprocket, version: "v1", status: "draft",
    notes: "Draft routing for in-house sprocket machining.", created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO,
    operations: [routingOp(ROUTING_IDS.sprocket, 1, "Machine sprocket", WC_IDS.machining, "25", "3.5")],
  },
];

const WO_IDS = {
  widgetReleased: uuid("mfg.work_order:widget-released"),
  gadgetInProgress: uuid("mfg.work_order:gadget-in-progress"),
  sprocketDraft: uuid("mfg.work_order:sprocket-draft"),
  widgetCompleted: uuid("mfg.work_order:widget-completed"),
  gadgetClosed: uuid("mfg.work_order:gadget-closed"),
  filterCancelled: uuid("mfg.work_order:filter-cancelled"),
};

export const WORK_ORDERS: WorkOrder[] = [
  { tenant_id: DEMO_TENANT_ID, id: WO_IDS.widgetReleased, item_id: MFG_ITEMS.widget, bom_id: BOM_IDS.widgetV2, routing_id: ROUTING_IDS.widget, warehouse_id: WAREHOUSE_IDS.main, planned_qty: "100", actual_qty: null, status: "released", scheduled_start: toCalendarISO(addDays(TODAY, 1)), scheduled_end: toCalendarISO(addDays(TODAY, 5)), started_at: null, completed_at: null, notes: "Replenish Q3 widget stock.", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WO_IDS.gadgetInProgress, item_id: MFG_ITEMS.gadget, bom_id: BOM_IDS.gadgetV1, routing_id: ROUTING_IDS.gadget, warehouse_id: WAREHOUSE_IDS.main, planned_qty: "50", actual_qty: null, status: "in_progress", scheduled_start: toCalendarISO(addDays(TODAY, -1)), scheduled_end: toCalendarISO(addDays(TODAY, 2)), started_at: LAST_WEEK_ISO, completed_at: null, notes: "Priority customer order.", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WO_IDS.sprocketDraft, item_id: MFG_ITEMS.sprocket, bom_id: null, routing_id: null, warehouse_id: WAREHOUSE_IDS.west, planned_qty: "500", actual_qty: null, status: "draft", scheduled_start: null, scheduled_end: null, started_at: null, completed_at: null, notes: "Build sub-assembly buffer.", created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WO_IDS.widgetCompleted, item_id: MFG_ITEMS.widget, bom_id: BOM_IDS.widgetV2, routing_id: ROUTING_IDS.widget, warehouse_id: WAREHOUSE_IDS.main, planned_qty: "80", actual_qty: "78", status: "completed", scheduled_start: toCalendarISO(addDays(TODAY, -10)), scheduled_end: toCalendarISO(addDays(TODAY, -6)), started_at: LAST_MONTH_ISO, completed_at: LAST_WEEK_ISO, notes: "Two units scrapped at QA.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WO_IDS.gadgetClosed, item_id: MFG_ITEMS.gadget, bom_id: BOM_IDS.gadgetV1, routing_id: ROUTING_IDS.gadget, warehouse_id: WAREHOUSE_IDS.main, planned_qty: "40", actual_qty: "40", status: "closed", scheduled_start: toCalendarISO(addDays(TODAY, -20)), scheduled_end: toCalendarISO(addDays(TODAY, -16)), started_at: LAST_MONTH_ISO, completed_at: LAST_MONTH_ISO, notes: undefined, created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: WO_IDS.filterCancelled, item_id: MFG_ITEMS.filter, bom_id: null, routing_id: null, warehouse_id: WAREHOUSE_IDS.west, planned_qty: "30", actual_qty: null, status: "cancelled", scheduled_start: null, scheduled_end: null, started_at: null, completed_at: null, notes: "Cancelled — sourced externally.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
];

function jobCard(woId: string, seq: number, wc: string, status: JobCard["status"], produced: string, rejected: string, start?: string, end?: string): JobCard {
  return {
    tenant_id: DEMO_TENANT_ID,
    id: uuid(`mfg.job_card:${woId}:${seq}`),
    work_order_id: woId,
    routing_operation_seq: seq,
    work_center_id: wc,
    status,
    planned_start: null,
    planned_end: null,
    actual_start: start ?? null,
    actual_end: end ?? null,
    operator_id: null,
    qty_produced: produced,
    qty_rejected: rejected,
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  };
}

export const JOB_CARDS_BY_WO: Record<string, JobCard[]> = {
  [WO_IDS.widgetReleased]: [
    jobCard(WO_IDS.widgetReleased, 1, WC_IDS.machining, "pending", "0", "0"),
    jobCard(WO_IDS.widgetReleased, 2, WC_IDS.assembly, "pending", "0", "0"),
    jobCard(WO_IDS.widgetReleased, 3, WC_IDS.finishing, "pending", "0", "0"),
  ],
  [WO_IDS.gadgetInProgress]: [
    jobCard(WO_IDS.gadgetInProgress, 1, WC_IDS.assembly, "completed", "50", "0", LAST_WEEK_ISO, NOW_ISO),
    jobCard(WO_IDS.gadgetInProgress, 2, WC_IDS.packaging, "in_progress", "0", "0", NOW_ISO),
  ],
};

// buildCapacityPlan derives a finite-capacity utilisation grid across
// the requested [start, end] calendar window. Weekdays carry a stable
// per-work-center load (some intentionally overloaded); weekends are
// idle. Dates are produced as YYYY-MM-DD so the page's calendar
// parser lines them up exactly.
const WC_DOW_LOAD: Record<string, number[]> = {
  // Indexed by Date.getDay(): 0=Sun … 6=Sat.
  [WC_IDS.assembly]: [0, 78, 92, 96, 88, 70, 0],
  [WC_IDS.machining]: [0, 104, 118, 88, 112, 96, 0],
  [WC_IDS.finishing]: [0, 54, 68, 73, 60, 48, 0],
  [WC_IDS.packaging]: [0, 0, 0, 0, 0, 0, 0],
};

export function buildCapacityPlan(start: string, end: string): CapacityPlan {
  const s = new Date(`${start}T00:00:00`);
  const e = new Date(`${end}T00:00:00`);
  const days: Date[] = [];
  if (!isNaN(s.getTime()) && !isNaN(e.getTime()) && e >= s) {
    let cur = s;
    while (cur <= e && days.length < 45) {
      days.push(new Date(cur));
      cur = addDays(cur, 1);
    }
  }
  if (days.length === 0) days.push(new Date(TODAY));
  const rows: WorkCenterSchedule[] = WORK_CENTERS.map((wc) => {
    const available = Number(wc.operating_hours_per_day) * 60;
    const pattern = WC_DOW_LOAD[wc.id] ?? [0, 60, 60, 60, 60, 60, 0];
    const dayLoads: CapacityDayLoad[] = days.map((d) => {
      const util = pattern[d.getDay()] ?? 0;
      const scheduled = Math.round((available * util) / 100);
      return {
        date: toCalendarISO(d),
        scheduled_minutes: String(scheduled),
        available_minutes: String(available),
        utilization_percent: String(util),
        overloaded: util > 100,
      };
    });
    return { work_center_id: wc.id, work_center_name: wc.name, status: wc.status, days: dayLoads };
  });
  return { start, end, rows };
}

// --- MRP runs --------------------------------------------------------

const MRP_IDS = {
  run1: uuid("mfg.mrp_run:1"),
  run2: uuid("mfg.mrp_run:2"),
  run3: uuid("mfg.mrp_run:3"),
};

function demandLine(runId: string, seed: string, itemId: string, qty: string, due: string, source: MRPDemandLine["source"], sourceRef?: string): MRPDemandLine {
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`mfg.mrp_demand:${seed}`), run_id: runId, item_id: itemId, qty, due_date: due, source, source_ref: sourceRef, created_at: LAST_WEEK_ISO };
}

function plannedOrder(runId: string, seed: string, itemId: string, type: MRPPlannedOrder["order_type"], qty: string, due: string, startDate: string, level: number, lead: number, bomId?: string | null): MRPPlannedOrder {
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`mfg.mrp_planned:${seed}`), run_id: runId, item_id: itemId, order_type: type, qty, due_date: due, suggested_start_date: startDate, explosion_level: level, bom_id: bomId ?? null, routing_id: null, lead_time_days: lead, created_at: LAST_WEEK_ISO };
}

// Full MRP runs (header + detail). listMRPRuns strips the detail to
// mirror the real list payload; getMRPRun returns the full record.
export const MRP_RUNS: MRPRun[] = [
  {
    tenant_id: DEMO_TENANT_ID, id: MRP_IDS.run1, status: "completed",
    horizon_start: toCalendarISO(TODAY), horizon_end: toCalendarISO(addDays(TODAY, 30)),
    include_min_stock: true, buy_lead_time_days: 7, demand_line_count: 3, planned_order_count: 6, make_order_count: 3, buy_order_count: 3,
    notes: "Monthly net-requirements run with reorder top-up.", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: LAST_WEEK_ISO,
    demand_lines: [
      demandLine(MRP_IDS.run1, "r1-widget", MFG_ITEMS.widget, "120", toCalendarISO(addDays(TODAY, 20)), "sales_order", "SO-2026-0042"),
      demandLine(MRP_IDS.run1, "r1-gadget", MFG_ITEMS.gadget, "60", toCalendarISO(addDays(TODAY, 25)), "sales_order", "SO-2026-0043"),
      demandLine(MRP_IDS.run1, "r1-sprocket", MFG_ITEMS.sprocket, "200", toCalendarISO(addDays(TODAY, 15)), "manual"),
    ],
    planned_orders: [
      plannedOrder(MRP_IDS.run1, "r1-mk-widget", MFG_ITEMS.widget, "make", "120", toCalendarISO(addDays(TODAY, 20)), toCalendarISO(addDays(TODAY, 15)), 0, 5, BOM_IDS.widgetV2),
      plannedOrder(MRP_IDS.run1, "r1-mk-gadget", MFG_ITEMS.gadget, "make", "60", toCalendarISO(addDays(TODAY, 25)), toCalendarISO(addDays(TODAY, 22)), 0, 3, BOM_IDS.gadgetV1),
      plannedOrder(MRP_IDS.run1, "r1-mk-sprocket", MFG_ITEMS.sprocket, "make", "440", toCalendarISO(addDays(TODAY, 15)), toCalendarISO(addDays(TODAY, 12)), 1, 3, BOM_IDS.sprocketV1),
      plannedOrder(MRP_IDS.run1, "r1-by-bolt", MFG_ITEMS.bolt, "buy", "4040", toCalendarISO(addDays(TODAY, 12)), toCalendarISO(addDays(TODAY, 5)), 2, 7),
      plannedOrder(MRP_IDS.run1, "r1-by-adapter", MFG_ITEMS.adapter, "buy", "60", toCalendarISO(addDays(TODAY, 22)), toCalendarISO(addDays(TODAY, 12)), 1, 10),
      plannedOrder(MRP_IDS.run1, "r1-by-cable", MFG_ITEMS.cable, "buy", "180", toCalendarISO(addDays(TODAY, 20)), toCalendarISO(addDays(TODAY, 13)), 1, 7),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: MRP_IDS.run2, status: "completed",
    horizon_start: toCalendarISO(addDays(TODAY, -7)), horizon_end: toCalendarISO(addDays(TODAY, 14)),
    include_min_stock: false, buy_lead_time_days: 7, demand_line_count: 2, planned_order_count: 3, make_order_count: 2, buy_order_count: 1,
    notes: "Short-horizon check against firm orders.", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: LAST_WEEK_ISO,
    demand_lines: [
      demandLine(MRP_IDS.run2, "r2-widget", MFG_ITEMS.widget, "40", toCalendarISO(addDays(TODAY, 10)), "manual"),
      demandLine(MRP_IDS.run2, "r2-gadget", MFG_ITEMS.gadget, "25", toCalendarISO(addDays(TODAY, 12)), "work_order", "WO-2026-0008"),
    ],
    planned_orders: [
      plannedOrder(MRP_IDS.run2, "r2-mk-widget", MFG_ITEMS.widget, "make", "40", toCalendarISO(addDays(TODAY, 10)), toCalendarISO(addDays(TODAY, 5)), 0, 5, BOM_IDS.widgetV2),
      plannedOrder(MRP_IDS.run2, "r2-mk-gadget", MFG_ITEMS.gadget, "make", "25", toCalendarISO(addDays(TODAY, 12)), toCalendarISO(addDays(TODAY, 9)), 0, 3, BOM_IDS.gadgetV1),
      plannedOrder(MRP_IDS.run2, "r2-by-cable", MFG_ITEMS.cable, "buy", "65", toCalendarISO(addDays(TODAY, 10)), toCalendarISO(addDays(TODAY, 3)), 1, 7),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: MRP_IDS.run3, status: "failed",
    horizon_start: toCalendarISO(addDays(TODAY, -14)), horizon_end: toCalendarISO(addDays(TODAY, -1)),
    include_min_stock: false, buy_lead_time_days: 7, demand_line_count: 1, planned_order_count: 0, make_order_count: 0, buy_order_count: 0,
    notes: "Failed — demand item had no active BOM.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO,
    demand_lines: [
      demandLine(MRP_IDS.run3, "r3-filter", MFG_ITEMS.filter, "30", toCalendarISO(addDays(TODAY, -2)), "manual"),
    ],
    planned_orders: [],
  },
];

// --- Subcontracting --------------------------------------------------

const SO_IDS = {
  draft: uuid("mfg.subcontract:draft"),
  issued: uuid("mfg.subcontract:issued"),
  received: uuid("mfg.subcontract:received"),
  closed: uuid("mfg.subcontract:closed"),
  cancelled: uuid("mfg.subcontract:cancelled"),
};

function subComponent(orderId: string, seed: string, itemId: string, qty: string, issued: string): SubcontractComponent {
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`mfg.subcontract_component:${seed}`), subcontract_order_id: orderId, item_id: itemId, qty, issued_qty: issued, created_at: LAST_WEEK_ISO };
}

export const SUBCONTRACT_ORDERS: SubcontractOrder[] = [
  {
    tenant_id: DEMO_TENANT_ID, id: SO_IDS.draft, work_order_id: null, routing_operation_seq: null, supplier_id: "Globex Precision Castings",
    item_id: MFG_ITEMS.sprocket, warehouse_id: WAREHOUSE_IDS.main, qty: "100", received_qty: "0", status: "draft", charge_amount: "1850.00", charge_currency: "USD",
    issued_at: null, received_at: null, notes: "Outsourced sprocket machining.", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: LAST_WEEK_ISO,
    components: [subComponent(SO_IDS.draft, "draft-bolt", MFG_ITEMS.bolt, "800", "0")],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: SO_IDS.issued, work_order_id: null, routing_operation_seq: null, supplier_id: "Initech Machining LLC",
    item_id: MFG_ITEMS.widget, warehouse_id: WAREHOUSE_IDS.main, qty: "50", received_qty: "0", status: "issued", charge_amount: "1200.00", charge_currency: "USD",
    issued_at: LAST_WEEK_ISO, received_at: null, notes: "Components issued to supplier.", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO,
    components: [
      subComponent(SO_IDS.issued, "issued-bolt", MFG_ITEMS.bolt, "200", "200"),
      subComponent(SO_IDS.issued, "issued-cable", MFG_ITEMS.cable, "50", "50"),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: SO_IDS.received, work_order_id: null, routing_operation_seq: null, supplier_id: "Hooli Assembly Partners",
    item_id: MFG_ITEMS.gadget, warehouse_id: WAREHOUSE_IDS.main, qty: "40", received_qty: "40", status: "received", charge_amount: "980.00", charge_currency: "USD",
    issued_at: LAST_MONTH_ISO, received_at: LAST_WEEK_ISO, notes: "Finished gadgets received back.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    components: [
      subComponent(SO_IDS.received, "received-adapter", MFG_ITEMS.adapter, "40", "40"),
      subComponent(SO_IDS.received, "received-cable", MFG_ITEMS.cable, "40", "40"),
    ],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: SO_IDS.closed, work_order_id: null, routing_operation_seq: null, supplier_id: "Initech Machining LLC",
    item_id: MFG_ITEMS.widget, warehouse_id: WAREHOUSE_IDS.main, qty: "30", received_qty: "30", status: "closed", charge_amount: "720.00", charge_currency: "USD",
    issued_at: LAST_MONTH_ISO, received_at: LAST_MONTH_ISO, notes: "Closed and reconciled.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO,
    components: [subComponent(SO_IDS.closed, "closed-bolt", MFG_ITEMS.bolt, "120", "120")],
  },
  {
    tenant_id: DEMO_TENANT_ID, id: SO_IDS.cancelled, work_order_id: null, routing_operation_seq: null, supplier_id: null,
    item_id: MFG_ITEMS.filter, warehouse_id: WAREHOUSE_IDS.west, qty: "20", received_qty: "0", status: "cancelled", charge_amount: "0.00", charge_currency: "USD",
    issued_at: null, received_at: null, notes: "Cancelled before issue.", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO,
    components: [],
  },
];

// --- Inventory: landed-cost vouchers ---------------------------------
//
// Vouchers spread freight / duty / insurance across received goods.
// Lifecycle: draft → allocated → posted. Charges + targets are loaded
// per-voucher by getLandedCostVoucher.

const LC_IDS = {
  v0006: uuid("inventory.landed_cost:0006"),
  v0007: uuid("inventory.landed_cost:0007"),
  v0008: uuid("inventory.landed_cost:0008"),
  v0009: uuid("inventory.landed_cost:0009"),
};

export const LANDED_COST_VOUCHERS: LandedCostVoucher[] = [
  { tenant_id: DEMO_TENANT_ID, id: LC_IDS.v0009, voucher_number: "LC-2026-0009", description: "Marine insurance — March import", status: "draft", allocation_method: "by_qty", posted_at: null, je_id: null, created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LC_IDS.v0008, voucher_number: "LC-2026-0008", description: "Ocean freight — container HLCU-4471", status: "allocated", allocation_method: "by_amount", posted_at: null, je_id: null, created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LC_IDS.v0007, voucher_number: "LC-2026-0007", description: "Freight + duty — Q1 widget shipment", status: "posted", allocation_method: "by_qty", posted_at: LAST_WEEK_ISO, je_id: uuid("je:lc-0007"), created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LC_IDS.v0006, voucher_number: "LC-2026-0006", description: "Air freight — expedite, handling", status: "posted", allocation_method: "by_weight", posted_at: LAST_MONTH_ISO, je_id: uuid("je:lc-0006"), created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
];

function lcCharge(voucherId: string, seed: string, description: string, amount: string, account_code: string): LandedCostCharge {
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`inventory.landed_cost_charge:${seed}`), voucher_id: voucherId, description, amount, account_code, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO };
}

function lcTarget(voucherId: string, seed: string, itemId: string, warehouseId: string, qty: string, unitCost: string, amount: string, weight: string, allocated: string, applied: boolean): LandedCostTarget {
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`inventory.landed_cost_target:${seed}`), voucher_id: voucherId, source_ktype: "inventory.goods_receipt", source_id: uuid(`inventory.goods_receipt:${seed}`), item_id: itemId, warehouse_id: warehouseId, qty, unit_cost: unitCost, amount, weight, allocated_amount: allocated, applied, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO };
}

export const LANDED_COST_CHARGES_BY_VOUCHER: Record<string, LandedCostCharge[]> = {
  [LC_IDS.v0009]: [lcCharge(LC_IDS.v0009, "0009-insurance", "Marine insurance", "300.00", "5220")],
  [LC_IDS.v0008]: [lcCharge(LC_IDS.v0008, "0008-freight", "Ocean freight", "800.00", "5200")],
  [LC_IDS.v0007]: [
    lcCharge(LC_IDS.v0007, "0007-freight", "Ocean freight", "1200.00", "5200"),
    lcCharge(LC_IDS.v0007, "0007-duty", "Import duty", "450.00", "5210"),
  ],
  [LC_IDS.v0006]: [
    lcCharge(LC_IDS.v0006, "0006-air", "Air freight (expedite)", "2100.00", "5200"),
    lcCharge(LC_IDS.v0006, "0006-handling", "Handling & brokerage", "180.00", "5210"),
  ],
};

export const LANDED_COST_TARGETS_BY_VOUCHER: Record<string, LandedCostTarget[]> = {
  [LC_IDS.v0009]: [
    lcTarget(LC_IDS.v0009, "0009-cable", MFG_ITEMS.cable, WAREHOUSE_IDS.main, "300", "7.00", "2100.00", "15", "0.00", false),
  ],
  [LC_IDS.v0008]: [
    lcTarget(LC_IDS.v0008, "0008-gadget", MFG_ITEMS.gadget, WAREHOUSE_IDS.main, "50", "40.00", "2000.00", "30", "421.05", false),
    lcTarget(LC_IDS.v0008, "0008-adapter", MFG_ITEMS.adapter, WAREHOUSE_IDS.main, "100", "18.00", "1800.00", "20", "378.95", false),
  ],
  [LC_IDS.v0007]: [
    lcTarget(LC_IDS.v0007, "0007-widget", MFG_ITEMS.widget, WAREHOUSE_IDS.main, "100", "15.00", "1500.00", "120", "550.00", true),
    lcTarget(LC_IDS.v0007, "0007-sprocket", MFG_ITEMS.sprocket, WAREHOUSE_IDS.main, "200", "9.00", "1800.00", "60", "1100.00", true),
  ],
  [LC_IDS.v0006]: [
    lcTarget(LC_IDS.v0006, "0006-bolt", MFG_ITEMS.bolt, WAREHOUSE_IDS.west, "1000", "0.08", "80.00", "40", "1403.08", true),
    lcTarget(LC_IDS.v0006, "0006-filter", MFG_ITEMS.filter, WAREHOUSE_IDS.west, "50", "12.00", "600.00", "25", "876.92", true),
  ],
};

// --- Inventory: cycle-count sessions ---------------------------------
//
// Lifecycle: draft → counting → reconciled → posted. Lines carry the
// expected (system) qty, the counted qty and the variance between them.

const CC_IDS = {
  s0010: uuid("inventory.cycle_count:0010"),
  s0011: uuid("inventory.cycle_count:0011"),
  s0012: uuid("inventory.cycle_count:0012"),
  s0013: uuid("inventory.cycle_count:0013"),
};

export const CYCLE_COUNT_SESSIONS: CycleCountSession[] = [
  { tenant_id: DEMO_TENANT_ID, id: CC_IDS.s0013, code: "CC-2026-0013", description: "West hub spot check", warehouse_id: WAREHOUSE_IDS.west, status: "draft", created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO, posted_at: null },
  { tenant_id: DEMO_TENANT_ID, id: CC_IDS.s0012, code: "CC-2026-0012", description: "Fast-movers weekly count", warehouse_id: WAREHOUSE_IDS.main, status: "counting", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO, posted_at: null },
  { tenant_id: DEMO_TENANT_ID, id: CC_IDS.s0011, code: "CC-2026-0011", description: "Bin A reconciliation", warehouse_id: WAREHOUSE_IDS.main, status: "reconciled", created_by: "system", created_at: LAST_WEEK_ISO, updated_at: LAST_WEEK_ISO, posted_at: null },
  { tenant_id: DEMO_TENANT_ID, id: CC_IDS.s0010, code: "CC-2026-0010", description: "Month-end full count", warehouse_id: WAREHOUSE_IDS.main, status: "posted", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO, posted_at: LAST_MONTH_ISO },
];

function ccLine(sessionId: string, seed: string, itemId: string, expected: string, counted: string): CycleCountLine {
  const variance = (Number(counted) - Number(expected)).toFixed(2).replace(/\.00$/, "");
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`inventory.cycle_count_line:${seed}`), session_id: sessionId, item_id: itemId, expected_qty: expected, counted_qty: counted, variance, notes: undefined, created_at: LAST_WEEK_ISO, updated_at: NOW_ISO };
}

export const CYCLE_COUNT_LINES_BY_SESSION: Record<string, CycleCountLine[]> = {
  [CC_IDS.s0013]: [],
  [CC_IDS.s0012]: [
    ccLine(CC_IDS.s0012, "0012-widget", MFG_ITEMS.widget, "120", "118"),
    ccLine(CC_IDS.s0012, "0012-gadget", MFG_ITEMS.gadget, "60", "60"),
    ccLine(CC_IDS.s0012, "0012-cable", MFG_ITEMS.cable, "300", "305"),
  ],
  [CC_IDS.s0011]: [
    ccLine(CC_IDS.s0011, "0011-bolt", MFG_ITEMS.bolt, "1000", "990"),
    ccLine(CC_IDS.s0011, "0011-sprocket", MFG_ITEMS.sprocket, "200", "200"),
  ],
  [CC_IDS.s0010]: [
    ccLine(CC_IDS.s0010, "0010-widget", MFG_ITEMS.widget, "80", "80"),
    ccLine(CC_IDS.s0010, "0010-adapter", MFG_ITEMS.adapter, "100", "97"),
    ccLine(CC_IDS.s0010, "0010-filter", MFG_ITEMS.filter, "40", "41"),
  ],
};

// --- Recruitment: job openings, applications, interviews -------------
//
// A small but believable hiring pipeline: open / draft / on-hold /
// closed requisitions, candidates spread across the application
// lifecycle, and a couple of scheduled + completed interviews.

const JO_IDS = {
  backend: uuid("hr.job_opening:backend"),
  ae: uuid("hr.job_opening:account-exec"),
  designer: uuid("hr.job_opening:product-designer"),
  devops: uuid("hr.job_opening:devops"),
  opsAnalyst: uuid("hr.job_opening:ops-analyst"),
  techWriter: uuid("hr.job_opening:tech-writer"),
};

export const JOB_OPENINGS: JobOpening[] = [
  { id: JO_IDS.backend, tenant_id: DEMO_TENANT_ID, title: "Senior Backend Engineer", department: "Engineering", description: "Own core services across the Kapp kernel — Go APIs, workers and the record engine.", requirements: "5+ years building production Go services; strong SQL; distributed systems.", employment_type: "full_time", location: "San Francisco / Remote", salary_range_min: "150000", salary_range_max: "190000", currency: DEMO_BASE_CURRENCY, status: "open", hiring_manager_id: EMP_IDS.vpEng, max_positions: 2, positions_filled: 0, published_at: LAST_WEEK_ISO, closes_at: toCalendarISO(addDays(TODAY, 21)), created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO },
  { id: JO_IDS.ae, tenant_id: DEMO_TENANT_ID, title: "Account Executive", department: "Sales", description: "Drive net-new revenue across mid-market accounts in the East region.", requirements: "3+ years B2B SaaS closing experience; track record of quota attainment.", employment_type: "full_time", location: "New York, NY", salary_range_min: "85000", salary_range_max: "115000", currency: DEMO_BASE_CURRENCY, status: "open", hiring_manager_id: EMP_IDS.mgrSales, max_positions: 3, positions_filled: 1, published_at: LAST_MONTH_ISO, closes_at: toCalendarISO(addDays(TODAY, 10)), created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { id: JO_IDS.designer, tenant_id: DEMO_TENANT_ID, title: "Product Designer", department: "Product", description: "Shape the end-to-end experience of the KChat UI across all 16 modules.", requirements: "Portfolio of shipped B2B products; fluency in Figma and design systems.", employment_type: "full_time", location: "Remote (US)", salary_range_min: "115000", salary_range_max: "145000", currency: DEMO_BASE_CURRENCY, status: "open", hiring_manager_id: EMP_IDS.vpEng, max_positions: 1, positions_filled: 0, published_at: LAST_WEEK_ISO, closes_at: null, created_by: "system", created_at: LAST_WEEK_ISO, updated_at: NOW_ISO },
  { id: JO_IDS.devops, tenant_id: DEMO_TENANT_ID, title: "DevOps Engineer", department: "Engineering", description: "Own CI/CD, observability and the release pipeline.", requirements: "Kubernetes, Terraform, GitHub Actions; on-call maturity.", employment_type: "full_time", location: "Remote (US)", salary_range_min: "135000", salary_range_max: "165000", currency: DEMO_BASE_CURRENCY, status: "draft", hiring_manager_id: EMP_IDS.mgrPlatform, max_positions: 1, positions_filled: 0, published_at: null, closes_at: null, created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO },
  { id: JO_IDS.opsAnalyst, tenant_id: DEMO_TENANT_ID, title: "Operations Analyst", department: "Operations", description: "Support demand planning and supplier performance reporting.", requirements: "Strong Excel/SQL; supply-chain exposure a plus.", employment_type: "full_time", location: "Austin, TX", salary_range_min: "72000", salary_range_max: "92000", currency: DEMO_BASE_CURRENCY, status: "on_hold", hiring_manager_id: EMP_IDS.vpOps, max_positions: 1, positions_filled: 0, published_at: LAST_MONTH_ISO, closes_at: null, created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { id: JO_IDS.techWriter, tenant_id: DEMO_TENANT_ID, title: "Technical Writer (Contract)", department: "Engineering", description: "Document the public API and onboarding guides.", requirements: "Developer-docs experience; comfortable reading Go + TypeScript.", employment_type: "contract", location: "Remote", salary_range_min: "60", salary_range_max: "85", currency: DEMO_BASE_CURRENCY, status: "filled", hiring_manager_id: EMP_IDS.vpEng, max_positions: 1, positions_filled: 1, published_at: LAST_MONTH_ISO, closes_at: LAST_WEEK_ISO, created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
];

const APP_IDS = {
  maria: uuid("hr.application:maria"),
  david: uuid("hr.application:david"),
  priya: uuid("hr.application:priya"),
  james: uuid("hr.application:james"),
  sofia: uuid("hr.application:sofia"),
  liam: uuid("hr.application:liam"),
  aisha: uuid("hr.application:aisha"),
  tom: uuid("hr.application:tom"),
};

function daysAgoIso(n: number): string {
  return new Date(TODAY.getTime() - n * 86400_000).toISOString();
}

export const JOB_APPLICATIONS: JobApplication[] = [
  { id: APP_IDS.maria, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.backend, applicant_name: "Maria Gonzalez", applicant_email: "maria.gonzalez@example.com", phone: "+1-415-555-0142", resume_file_id: null, cover_letter: "Excited about the record-engine work.", source: "linkedin", referrer_employee_id: null, status: "interview", rating: 4, notes: "Strong systems background; advancing to onsite.", hired_employee_id: null, applied_at: daysAgoIso(9), created_by: "system", created_at: daysAgoIso(9), updated_at: daysAgoIso(2) },
  { id: APP_IDS.david, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.backend, applicant_name: "David Chen", applicant_email: "david.chen@example.com", phone: "+1-408-555-0119", resume_file_id: null, cover_letter: undefined, source: "referral", referrer_employee_id: EMP_IDS.ic1, status: "screening", rating: null, notes: "Referred by platform team.", hired_employee_id: null, applied_at: daysAgoIso(6), created_by: "system", created_at: daysAgoIso(6), updated_at: daysAgoIso(3) },
  { id: APP_IDS.priya, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.backend, applicant_name: "Priya Nair", applicant_email: "priya.nair@example.com", phone: undefined, resume_file_id: null, cover_letter: undefined, source: "website", referrer_employee_id: null, status: "applied", rating: null, notes: undefined, hired_employee_id: null, applied_at: daysAgoIso(2), created_by: "system", created_at: daysAgoIso(2), updated_at: daysAgoIso(2) },
  { id: APP_IDS.james, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.ae, applicant_name: "James Wilson", applicant_email: "james.wilson@example.com", phone: "+1-212-555-0177", resume_file_id: null, cover_letter: "Closed $4M in net-new last year.", source: "referral", referrer_employee_id: EMP_IDS.mgrSales, status: "hired", rating: 5, notes: "Accepted offer; starts next month.", hired_employee_id: EMP_IDS.ic2, applied_at: daysAgoIso(25), created_by: "system", created_at: daysAgoIso(25), updated_at: daysAgoIso(5) },
  { id: APP_IDS.sofia, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.ae, applicant_name: "Sofia Rossi", applicant_email: "sofia.rossi@example.com", phone: "+1-646-555-0163", resume_file_id: null, cover_letter: undefined, source: "agency", referrer_employee_id: null, status: "offered", rating: 5, notes: "Offer sent; awaiting response.", hired_employee_id: null, applied_at: daysAgoIso(14), created_by: "system", created_at: daysAgoIso(14), updated_at: daysAgoIso(1) },
  { id: APP_IDS.liam, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.designer, applicant_name: "Liam O'Brien", applicant_email: "liam.obrien@example.com", phone: undefined, resume_file_id: null, cover_letter: "Portfolio attached.", source: "website", referrer_employee_id: null, status: "shortlisted", rating: 4, notes: "Strong portfolio; schedule design exercise.", hired_employee_id: null, applied_at: daysAgoIso(8), created_by: "system", created_at: daysAgoIso(8), updated_at: daysAgoIso(4) },
  { id: APP_IDS.aisha, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.designer, applicant_name: "Aisha Khan", applicant_email: "aisha.khan@example.com", phone: undefined, resume_file_id: null, cover_letter: undefined, source: "website", referrer_employee_id: null, status: "rejected", rating: 2, notes: "Not enough B2B depth.", hired_employee_id: null, applied_at: daysAgoIso(12), created_by: "system", created_at: daysAgoIso(12), updated_at: daysAgoIso(7) },
  { id: APP_IDS.tom, tenant_id: DEMO_TENANT_ID, job_opening_id: JO_IDS.opsAnalyst, applicant_name: "Tom Becker", applicant_email: "tom.becker@example.com", phone: "+1-512-555-0150", resume_file_id: null, cover_letter: undefined, source: "website", referrer_employee_id: null, status: "applied", rating: null, notes: undefined, hired_employee_id: null, applied_at: daysAgoIso(3), created_by: "system", created_at: daysAgoIso(3), updated_at: daysAgoIso(3) },
];

export const INTERVIEWS: Interview[] = [
  { id: uuid("hr.interview:maria-phone"), tenant_id: DEMO_TENANT_ID, application_id: APP_IDS.maria, interviewer_id: EMP_IDS.mgrPlatform, interview_type: "phone", scheduled_at: daysAgoIso(5), duration_minutes: 30, location: undefined, meeting_link: "https://meet.example.com/maria-screen", status: "completed", rating: 4, feedback: "Solid phone screen; good communication.", recommendation: "yes", created_by: "system", created_at: daysAgoIso(6), updated_at: daysAgoIso(5) },
  { id: uuid("hr.interview:maria-tech"), tenant_id: DEMO_TENANT_ID, application_id: APP_IDS.maria, interviewer_id: EMP_IDS.vpEng, interview_type: "technical", scheduled_at: toCalendarISO(addDays(TODAY, 2)), duration_minutes: 60, location: undefined, meeting_link: "https://meet.example.com/maria-tech", status: "scheduled", rating: null, feedback: undefined, recommendation: undefined, created_by: "system", created_at: daysAgoIso(2), updated_at: daysAgoIso(2) },
  { id: uuid("hr.interview:sofia-panel"), tenant_id: DEMO_TENANT_ID, application_id: APP_IDS.sofia, interviewer_id: EMP_IDS.mgrSales, interview_type: "panel", scheduled_at: daysAgoIso(3), duration_minutes: 45, location: "NYC Office — Room 4B", meeting_link: undefined, status: "completed", rating: 5, feedback: "Excellent discovery skills; strong close.", recommendation: "strong_yes", created_by: "system", created_at: daysAgoIso(6), updated_at: daysAgoIso(3) },
  { id: uuid("hr.interview:liam-portfolio"), tenant_id: DEMO_TENANT_ID, application_id: APP_IDS.liam, interviewer_id: EMP_IDS.vpEng, interview_type: "video", scheduled_at: toCalendarISO(addDays(TODAY, 4)), duration_minutes: 45, location: undefined, meeting_link: "https://meet.example.com/liam-portfolio", status: "scheduled", rating: null, feedback: undefined, recommendation: undefined, created_by: "system", created_at: daysAgoIso(1), updated_at: daysAgoIso(1) },
];

// --- LMS: learning paths, badges, badge awards -----------------------
//
// Curated learning paths across the published / draft / archived
// lifecycle, a gamification badge catalogue, and a feed of awards to
// seeded employees (so the Badges page shows earned-vs-locked state).

const LP_IDS = {
  salesOnboarding: uuid("lms.learning_path:sales-onboarding"),
  engFoundations: uuid("lms.learning_path:eng-foundations"),
  managerEssentials: uuid("lms.learning_path:manager-essentials"),
  securityCompliance: uuid("lms.learning_path:security-compliance"),
  dataModeling: uuid("lms.learning_path:data-modeling"),
  csBootcamp: uuid("lms.learning_path:cs-bootcamp"),
};

export const LEARNING_PATHS: LearningPath[] = [
  { tenant_id: DEMO_TENANT_ID, id: LP_IDS.salesOnboarding, title: "Sales Onboarding", description: "Ramp new account executives on the product, pitch and CRM workflow.", status: "published", target_roles: ["Sales", "Account Executive"], estimated_duration_hours: 12, difficulty: "beginner", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LP_IDS.engFoundations, title: "Engineering Foundations", description: "Core services, the record engine and our development workflow.", status: "published", target_roles: ["Engineering"], estimated_duration_hours: 24, difficulty: "intermediate", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LP_IDS.managerEssentials, title: "Manager Essentials", description: "First-time manager fundamentals: 1:1s, feedback and goal-setting.", status: "published", target_roles: ["Management"], estimated_duration_hours: 8, difficulty: "beginner", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LP_IDS.securityCompliance, title: "Security & Compliance", description: "Annual security awareness, data handling and compliance training.", status: "published", target_roles: ["All Employees"], estimated_duration_hours: 4, difficulty: "beginner", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LP_IDS.dataModeling, title: "Advanced Data Modeling", description: "Designing ktypes, relationships and reporting models at scale.", status: "draft", target_roles: ["Engineering", "Data"], estimated_duration_hours: 16, difficulty: "advanced", created_by: "system", created_at: NOW_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, id: LP_IDS.csBootcamp, title: "Customer Success Bootcamp", description: "Legacy onboarding programme for the customer success team.", status: "archived", target_roles: ["Customer Success"], estimated_duration_hours: 10, difficulty: "beginner", created_by: "system", created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
];

function lpCourse(pathId: string, seed: string, courseId: string, order: number, mandatory: boolean): LearningPathCourse {
  return { tenant_id: DEMO_TENANT_ID, id: uuid(`lms.learning_path_course:${seed}`), learning_path_id: pathId, course_id: courseId, sequence_order: order, is_mandatory: mandatory, prerequisite_course_ids: null };
}

export const LEARNING_PATH_COURSES_BY_PATH: Record<string, LearningPathCourse[]> = {
  [LP_IDS.salesOnboarding]: [
    lpCourse(LP_IDS.salesOnboarding, "sales-1", COURSE_IDS.c1, 1, true),
    lpCourse(LP_IDS.salesOnboarding, "sales-2", COURSE_IDS.c3, 2, false),
  ],
  [LP_IDS.engFoundations]: [
    lpCourse(LP_IDS.engFoundations, "eng-1", COURSE_IDS.c1, 1, true),
    lpCourse(LP_IDS.engFoundations, "eng-2", COURSE_IDS.c2, 2, true),
  ],
  [LP_IDS.securityCompliance]: [
    lpCourse(LP_IDS.securityCompliance, "sec-1", COURSE_IDS.c2, 1, true),
  ],
};

const BADGE_IDS = {
  firstCourse: uuid("lms.badge:first-course"),
  pathfinder: uuid("lms.badge:pathfinder"),
  compliancePro: uuid("lms.badge:compliance-pro"),
  topPerformer: uuid("lms.badge:top-performer"),
  streak: uuid("lms.badge:streak"),
  mentor: uuid("lms.badge:mentor"),
};

export const BADGES: Badge[] = [
  { tenant_id: DEMO_TENANT_ID, id: BADGE_IDS.firstCourse, name: "First Steps", description: "Awarded for completing your very first course.", icon: "footprints", criteria_type: "course_completion", criteria_value: { courses: 1 }, active: true, created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BADGE_IDS.pathfinder, name: "Pathfinder", description: "Complete an entire learning path end to end.", icon: "route", criteria_type: "path_completion", criteria_value: { paths: 1 }, active: true, created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BADGE_IDS.compliancePro, name: "Compliance Pro", description: "Finish the annual security & compliance training.", icon: "shield-check", criteria_type: "course_completion", criteria_value: { course: "Security & Compliance" }, active: true, created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BADGE_IDS.topPerformer, name: "Top Performer", description: "Score 90%+ on five graded assessments.", icon: "trophy", criteria_type: "assessment_score", criteria_value: { min_score: 90, count: 5 }, active: true, created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BADGE_IDS.streak, name: "On a Streak", description: "Learn something every day for 7 days running.", icon: "flame", criteria_type: "learning_streak", criteria_value: { days: 7 }, active: true, created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
  { tenant_id: DEMO_TENANT_ID, id: BADGE_IDS.mentor, name: "Mentor", description: "Answer 10 questions in course discussions.", icon: "users", criteria_type: "discussion_answers", criteria_value: { answers: 10 }, active: false, created_at: LAST_MONTH_ISO, updated_at: LAST_MONTH_ISO },
];

export const BADGE_AWARDS: BadgeAward[] = [
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:1"), user_id: EMP_IDS.ic1, badge_id: BADGE_IDS.firstCourse, earned_at: daysAgoIso(20) },
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:2"), user_id: EMP_IDS.ic2, badge_id: BADGE_IDS.firstCourse, earned_at: daysAgoIso(18) },
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:3"), user_id: EMP_IDS.ic3, badge_id: BADGE_IDS.firstCourse, earned_at: daysAgoIso(12) },
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:4"), user_id: EMP_IDS.ic1, badge_id: BADGE_IDS.compliancePro, earned_at: daysAgoIso(10) },
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:5"), user_id: EMP_IDS.mgrSales, badge_id: BADGE_IDS.compliancePro, earned_at: daysAgoIso(9) },
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:6"), user_id: EMP_IDS.ic3, badge_id: BADGE_IDS.pathfinder, earned_at: daysAgoIso(5) },
  { tenant_id: DEMO_TENANT_ID, id: uuid("lms.badge_award:7"), user_id: EMP_IDS.vpEng, badge_id: BADGE_IDS.topPerformer, earned_at: daysAgoIso(3) },
];

function jl(account_code: string, debit: string, credit: string, memo = "", currency = "USD") {
  return { account_code, debit, credit, memo, currency };
}

function buildEntry(idSeed: string, posted_at: string, memo: string, source_ktype: string, lines: Array<{ account_code: string; debit: string; credit: string; memo: string; currency: string }>): JournalEntry {
  const id = uuid(`je:${idSeed}`);
  return {
    id,
    tenant_id: DEMO_TENANT_ID,
    posted_at,
    memo,
    source_ktype,
    source_id: null,
    created_by: "system",
    created_at: posted_at,
    lines: lines.map((l, i) => ({
      id: i + 1,
      tenant_id: DEMO_TENANT_ID,
      entry_id: id,
      account_code: l.account_code,
      debit: l.debit,
      credit: l.credit,
      currency: l.currency,
      memo: l.memo,
    })),
  };
}

export const JOURNAL_ENTRIES: JournalEntry[] = [
  buildEntry("je1", todayPlus(-12) + "T10:00:00Z", "AR-2026-0001 — Globex license", "finance.ar_invoice", [
    jl("1020", "4200.00", "0.00", "Receivable — Globex"),
    jl("4020", "0.00", "4200.00", "Service revenue"),
  ]),
  buildEntry("je2", todayPlus(-9) + "T14:00:00Z", "AR-2026-0003 — Hooli enterprise", "finance.ar_invoice", [
    jl("1020", "31000.00", "0.00", "Receivable — Hooli"),
    jl("4010", "0.00", "31000.00", "Product revenue"),
  ]),
  buildEntry("je3", todayPlus(-7) + "T09:30:00Z", "AP-2026-0001 — Umbrella supplies", "finance.ap_bill", [
    jl("5000", "11200.00", "0.00", "Cost of goods sold"),
    jl("2010", "0.00", "11200.00", "Payable — Umbrella"),
  ]),
  buildEntry("je4", todayPlus(-5) + "T08:00:00Z", "April payroll posting", "hr.pay_run", [
    jl("6010", "154800.00", "0.00", "Salaries & wages — April"),
    jl("1010", "0.00", "113400.00", "Net pay disbursed"),
    jl("2020", "0.00", "41400.00", "Withheld taxes & deductions"),
  ]),
  buildEntry("je5", todayPlus(-2) + "T16:00:00Z", "Office rent — April", "manual", [
    jl("6020", "8500.00", "0.00", "Rent — HQ"),
    jl("1010", "0.00", "8500.00", "Cash disbursement"),
  ]),
];

// Trial balance precomputed from the journal entries above plus a
// small opening-balance plug so the totals tie out cleanly.
export const TRIAL_BALANCE: TrialBalanceReport = {
  tenant_id: DEMO_TENANT_ID,
  as_of: TODAY_ISO_DATE,
  rows: [
    { account_code: "1010", account_name: "Cash & Equivalents", type: "asset", debit: "120000.00", credit: "0.00", balance: "120000.00" },
    { account_code: "1020", account_name: "Accounts Receivable", type: "asset", debit: "45000.00", credit: "0.00", balance: "45000.00" },
    { account_code: "1030", account_name: "Inventory", type: "asset", debit: "98500.00", credit: "0.00", balance: "98500.00" },
    { account_code: "1500", account_name: "Property & Equipment", type: "asset", debit: "210000.00", credit: "0.00", balance: "210000.00" },
    { account_code: "2010", account_name: "Accounts Payable", type: "liability", debit: "0.00", credit: "18000.00", balance: "18000.00" },
    { account_code: "2020", account_name: "Accrued Expenses", type: "liability", debit: "0.00", credit: "41400.00", balance: "41400.00" },
    { account_code: "3010", account_name: "Common Stock", type: "equity", debit: "0.00", credit: "350000.00", balance: "350000.00" },
    { account_code: "3020", account_name: "Retained Earnings", type: "equity", debit: "0.00", credit: "44900.00", balance: "44900.00" },
    { account_code: "4010", account_name: "Product Revenue", type: "revenue", debit: "0.00", credit: "215000.00", balance: "215000.00" },
    { account_code: "4020", account_name: "Service Revenue", type: "revenue", debit: "0.00", credit: "82000.00", balance: "82000.00" },
    { account_code: "5000", account_name: "Cost of Goods Sold", type: "expense", debit: "94000.00", credit: "0.00", balance: "94000.00" },
    { account_code: "6010", account_name: "Salaries & Wages", type: "expense", debit: "154800.00", credit: "0.00", balance: "154800.00" },
    { account_code: "6020", account_name: "Rent", type: "expense", debit: "8500.00", credit: "0.00", balance: "8500.00" },
    { account_code: "6030", account_name: "Marketing", type: "expense", debit: "20500.00", credit: "0.00", balance: "20500.00" },
  ],
  // Debits and credits both sum to 751,300 — the +150k vs the previous
  // fixture is parked in Common Stock as additional paid-in capital so
  // the trial balance ties out cleanly (residual 0.00).
  total_debit: "751300.00",
  total_credit: "751300.00",
  residual: "0.00",
};

export const INCOME_STATEMENT: IncomeStatement = {
  from: "2026-01-01",
  to: TODAY_ISO_DATE,
  revenue: [
    { account_code: "4010", account_name: "Product Revenue", amount: "215000.00" },
    { account_code: "4020", account_name: "Service Revenue", amount: "82000.00" },
  ],
  expense: [
    { account_code: "5000", account_name: "Cost of Goods Sold", amount: "94000.00" },
    { account_code: "6010", account_name: "Salaries & Wages", amount: "154800.00" },
    { account_code: "6020", account_name: "Rent", amount: "8500.00" },
    { account_code: "6030", account_name: "Marketing", amount: "20500.00" },
  ],
  total_revenue: "297000.00",
  total_expense: "277800.00",
  net_income: "19200.00",
};

export const EXCHANGE_RATES: ExchangeRate[] = [
  { tenant_id: DEMO_TENANT_ID, from_currency: "USD", to_currency: "EUR", rate_date: TODAY_ISO_DATE, rate: "0.92", provider: "ECB", created_by: null, created_at: NOW_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, from_currency: "USD", to_currency: "GBP", rate_date: TODAY_ISO_DATE, rate: "0.79", provider: "BOE", created_by: null, created_at: NOW_ISO, updated_at: NOW_ISO },
  { tenant_id: DEMO_TENANT_ID, from_currency: "USD", to_currency: "AUD", rate_date: TODAY_ISO_DATE, rate: "1.52", provider: "RBA", created_by: null, created_at: NOW_ISO, updated_at: NOW_ISO },
];

// --- Approvals --------------------------------------------------------

export const APPROVALS: Approval[] = [
  {
    id: uuid("approval:1"),
    tenant_id: DEMO_TENANT_ID,
    record_ktype: "finance.ar_invoice",
    record_id: AR_INVOICES[1].id,
    chain: { steps: [{ approvers: ["finance.director"], required_count: 1 }], current_step: 0, requested_by: EMP_IDS.ic1, history: [] },
    state: "pending",
    created_at: LAST_WEEK_ISO,
  },
  {
    id: uuid("approval:2"),
    tenant_id: DEMO_TENANT_ID,
    record_ktype: "procurement.purchase_order",
    record_id: PURCHASE_ORDERS[0].id,
    chain: { steps: [{ approvers: ["ops.manager"], required_count: 1 }], current_step: 0, requested_by: EMP_IDS.ic4, history: [] },
    state: "pending",
    created_at: LAST_WEEK_ISO,
  },
  {
    id: uuid("approval:3"),
    tenant_id: DEMO_TENANT_ID,
    record_ktype: "hr.expense_claim",
    record_id: EXPENSE_CLAIMS[1].id,
    chain: { steps: [{ approvers: ["hr.manager"], required_count: 1 }], current_step: 1, requested_by: EMP_IDS.ic2, history: [{ step_index: 0, actor_id: EMP_IDS.mgrSales, decision: "approve", timestamp: LAST_WEEK_ISO }] },
    state: "approved",
    created_at: LAST_WEEK_ISO,
  },
  {
    id: uuid("approval:4"),
    tenant_id: DEMO_TENANT_ID,
    record_ktype: "hr.leave_request",
    record_id: LEAVE_REQUESTS[2].id,
    chain: { steps: [{ approvers: ["hr.manager"], required_count: 1 }], current_step: 0, requested_by: EMP_IDS.mgrSales, history: [] },
    state: "pending",
    created_at: LAST_WEEK_ISO,
  },
];

// --- Tenants / features / plans / usage / audit ----------------------

export const TENANTS: Tenant[] = [
  { id: DEMO_TENANT_ID, slug: DEMO_TENANT_SLUG, name: "Acme Corp", cell: "us-west-1", status: "active", plan: "growth", quota: null, created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { id: uuid("tenant:beta"), slug: "beta-foods", name: "Beta Foods Ltd", cell: "us-east-1", status: "active", plan: "starter", quota: null, created_at: LAST_MONTH_ISO, updated_at: NOW_ISO },
  { id: uuid("tenant:gamma"), slug: "gamma-build", name: "Gamma Build Co.", cell: "eu-central-1", status: "suspended", plan: "growth", quota: null, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
];

export const TENANT_FEATURES: TenantFeaturesResponse = {
  tenant_id: DEMO_TENANT_ID,
  features: {
    crm: true,
    finance: true,
    helpdesk: true,
    inventory: true,
    hr: true,
    lms: true,
    insights: true,
    pos: true,
    projects: true,
    insights_sql_editor: true,
    insights_data_sources: true,
  },
};

export const PLANS: Plan[] = [
  { name: "starter", display_name: "Starter", limits: { api_calls: 50000, storage_bytes: 10 * 1024 * 1024 * 1024, krecord_count: 25000, user_seats: 10 }, features: { crm: true, finance: true } },
  { name: "growth", display_name: "Growth", limits: { api_calls: 250000, storage_bytes: 100 * 1024 * 1024 * 1024, krecord_count: 250000, user_seats: 50 }, features: { crm: true, finance: true, hr: true, lms: true, insights: true } },
  { name: "enterprise", display_name: "Enterprise", limits: { api_calls: 5_000_000, storage_bytes: 1024 * 1024 * 1024 * 1024, krecord_count: 5_000_000, user_seats: 500 }, features: { crm: true, finance: true, hr: true, lms: true, insights: true, insights_sql_editor: true } },
];

export const TENANT_USAGE: TenantUsageResponse = {
  tenant_id: DEMO_TENANT_ID,
  plan: "growth",
  period_start: TODAY_ISO_DATE.slice(0, 7) + "-01",
  usage: {
    api_calls: 87340,
    storage_bytes: 14 * 1024 * 1024 * 1024,
    krecord_count: 41280,
    user_seats: 28,
  },
  limits: PLANS[1].limits,
  rows: [],
  features: TENANT_FEATURES.features,
};

export const TENANT_USAGE_HISTORY: TenantUsageHistoryResponse = {
  tenant_id: DEMO_TENANT_ID,
  rows: (() => {
    const rows: TenantUsageHistoryResponse["rows"] = [];
    const today = new Date(TODAY);
    for (let m = 5; m >= 0; m--) {
      const d = new Date(today);
      d.setMonth(d.getMonth() - m);
      const period = d.toISOString().slice(0, 7) + "-01";
      rows.push({ period_start: period, metric: "api_calls", value: 60000 + (5 - m) * 5400 });
      rows.push({ period_start: period, metric: "storage_bytes", value: (10 + (5 - m) * 0.7) * 1024 * 1024 * 1024 });
      rows.push({ period_start: period, metric: "krecord_count", value: 28000 + (5 - m) * 2600 });
      rows.push({ period_start: period, metric: "user_seats", value: 22 + (5 - m) });
    }
    return rows;
  })(),
  months: 6,
};

export const AUDIT_LOG: AuditEntry[] = [
  { id: 1, tenant_id: DEMO_TENANT_ID, actor_id: EMP_IDS.ic1, actor_kind: "user", action: "record.create", target_ktype: "crm.lead", target_id: LEADS[0].id, before: null, after: { name: "Greta Holm" }, created_at: LAST_WEEK_ISO },
  { id: 2, tenant_id: DEMO_TENANT_ID, actor_id: EMP_IDS.mgrSales, actor_kind: "user", action: "record.update", target_ktype: "crm.deal", target_id: DEALS[2].id, before: { stage: "qualification" }, after: { stage: "proposal" }, created_at: LAST_WEEK_ISO },
  { id: 3, tenant_id: DEMO_TENANT_ID, actor_id: null, actor_kind: "system", action: "ar_invoice.post", target_ktype: "finance.ar_invoice", target_id: AR_INVOICES[0].id, before: { status: "draft" }, after: { status: "posted" }, created_at: LAST_WEEK_ISO },
  { id: 4, tenant_id: DEMO_TENANT_ID, actor_id: "agent.deal_stage_advancer", actor_kind: "agent", action: "agent.invoke", target_ktype: "crm.deal", target_id: DEALS[1].id, after: { decision: "advance" }, created_at: LAST_WEEK_ISO },
  { id: 5, tenant_id: DEMO_TENANT_ID, actor_id: EMP_IDS.vpEng, actor_kind: "user", action: "feature.toggle", target_ktype: "tenant.features", target_id: DEMO_TENANT_ID, before: { insights_sql_editor: false }, after: { insights_sql_editor: true }, created_at: LAST_WEEK_ISO },
  { id: 6, tenant_id: DEMO_TENANT_ID, actor_id: EMP_IDS.mgrPlatform, actor_kind: "user", action: "webhook.create", target_ktype: "webhook", target_id: uuid("webhook:created"), after: { url: "https://example.test/hooks/1" }, created_at: LAST_WEEK_ISO },
  { id: 7, tenant_id: DEMO_TENANT_ID, actor_id: EMP_IDS.ic3, actor_kind: "user", action: "ticket.assign", target_ktype: "helpdesk.ticket", target_id: TICKETS[0].id, after: { assignee: EMP_IDS.ic3 }, created_at: LAST_WEEK_ISO },
  { id: 8, tenant_id: DEMO_TENANT_ID, actor_id: null, actor_kind: "system", action: "pay_run.post", target_ktype: "hr.pay_run", target_id: PAY_RUN_ID, after: { status: "posted" }, created_at: LAST_WEEK_ISO },
  { id: 9, tenant_id: DEMO_TENANT_ID, actor_id: EMP_IDS.ceo, actor_kind: "user", action: "approval.decide", target_ktype: "approval", target_id: APPROVALS[2].id, after: { decision: "approve" }, created_at: LAST_WEEK_ISO },
  { id: 10, tenant_id: DEMO_TENANT_ID, actor_id: null, actor_kind: "system", action: "tenant.feature_sync", target_ktype: "tenant", target_id: DEMO_TENANT_ID, after: { synced: 11 }, created_at: NOW_ISO },
];

// --- Webhooks ---------------------------------------------------------

export const WEBHOOKS: Webhook[] = [
  {
    id: uuid("webhook:slack"),
    tenant_id: DEMO_TENANT_ID,
    url: "https://hooks.example/slack/finance",
    secret: "*****",
    event_filters: ["finance.ar_invoice.post", "finance.ap_bill.post"],
    conditions: { ktype: "finance.ar_invoice" },
    max_retries: 5,
    backoff_base_seconds: 10,
    active: true,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    id: uuid("webhook:zapier"),
    tenant_id: DEMO_TENANT_ID,
    url: "https://hooks.example/zapier/leads",
    secret: "*****",
    event_filters: ["crm.lead.create", "crm.lead.update"],
    max_retries: 5,
    backoff_base_seconds: 10,
    active: false,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
];

export const WEBHOOK_DELIVERIES: WebhookDelivery[] = [
  { id: uuid("wd:1"), tenant_id: DEMO_TENANT_ID, webhook_id: WEBHOOKS[0].id, event_id: uuid("ev:1"), event_type: "finance.ar_invoice.post", status_code: 200, response_body: "ok", attempt: 1, delivered: true, created_at: LAST_WEEK_ISO },
  { id: uuid("wd:2"), tenant_id: DEMO_TENANT_ID, webhook_id: WEBHOOKS[0].id, event_id: uuid("ev:2"), event_type: "finance.ap_bill.post", status_code: 502, response_body: "bad gateway", attempt: 3, delivered: false, error: "remote 502", next_retry_at: NEXT_WEEK_ISO, created_at: LAST_WEEK_ISO },
  { id: uuid("wd:3"), tenant_id: DEMO_TENANT_ID, webhook_id: WEBHOOKS[1].id, event_id: uuid("ev:3"), event_type: "crm.lead.create", status_code: 200, response_body: "ok", attempt: 1, delivered: true, created_at: LAST_WEEK_ISO },
];

export const RETENTION_POLICIES: RetentionPolicy[] = [
  { tenant_id: DEMO_TENANT_ID, category: "audit", retention_days: 365, enabled: true, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, category: "webhook_delivery", retention_days: 30, enabled: true, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
  { tenant_id: DEMO_TENANT_ID, category: "import_job", retention_days: 90, enabled: false, created_at: LAST_MONTH_ISO, updated_at: LAST_WEEK_ISO },
];

export const PLACEMENT_POLICY: PlacementPolicy = {
  tenant: DEMO_TENANT_ID,
  bucket: `tenant-${DEMO_TENANT_SLUG}`,
  policy: {
    encryption: { mode: "ManagedEncrypted" },
    placement: { provider: ["wasabi"], region: ["us-west-1"], country: ["US"], storage_class: ["standard"], cache_location: "us-west-1" },
  },
};

// --- Insights ---------------------------------------------------------

const QUERY_IDS = {
  pipelineByStage: uuid("ins.q:pipeline-by-stage"),
  arBuckets: uuid("ins.q:ar-buckets"),
  invByCat: uuid("ins.q:inventory-by-category"),
  ticketsByPriority: uuid("ins.q:tickets-by-priority"),
  totalPipelineValue: uuid("ins.q:total-pipeline-value"),
};

export const INSIGHTS_QUERIES: InsightsQuery[] = [
  {
    tenant_id: DEMO_TENANT_ID,
    id: QUERY_IDS.pipelineByStage,
    name: "Pipeline value by stage",
    description: "Sum of crm.deal.value grouped by deal stage",
    definition: {
      source: "ktype:crm.deal",
      columns: ["stage", "value"],
      aggregations: [{ op: "sum", column: "value", alias: "total" }],
      group_by: ["stage"],
    },
    cache_ttl_seconds: 60,
    mode: "visual",
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: QUERY_IDS.arBuckets,
    name: "AR aging buckets",
    description: "Outstanding AR sliced by aging bucket",
    definition: { source: "report:ar_aging", columns: ["bucket", "amount"] },
    mode: "visual",
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: QUERY_IDS.invByCat,
    name: "Inventory units by SKU prefix",
    description: "Total on-hand quantity per item",
    definition: { source: "ktype:inventory.item", columns: ["sku", "qty"] },
    mode: "visual",
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: QUERY_IDS.ticketsByPriority,
    name: "Open tickets by priority",
    description: "Count of helpdesk tickets in open/in_progress status",
    definition: { source: "ktype:helpdesk.ticket", columns: ["priority", "count"] },
    mode: "visual",
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: QUERY_IDS.totalPipelineValue,
    name: "Total pipeline value",
    description: "Sum of open deal values",
    definition: { source: "ktype:crm.deal", columns: ["total"] },
    mode: "visual",
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
];

const QUERY_RESULTS: Record<string, InsightsRunResult> = {
  [QUERY_IDS.pipelineByStage]: {
    result: {
      columns: ["stage", "total"],
      rows: [
        { stage: "prospecting", total: 42000 },
        { stage: "qualification", total: 18000 },
        { stage: "proposal", total: 124000 },
        { stage: "negotiation", total: 67500 },
        { stage: "closed_won", total: 36000 },
      ],
    },
    cache_hit: false,
    query_hash: "h-pipeline",
    filter_hash: "f-default",
    expires_at: null,
  },
  [QUERY_IDS.arBuckets]: {
    result: {
      columns: ["bucket", "amount"],
      rows: [
        { bucket: "0–30 days", amount: 22500 },
        { bucket: "31–60 days", amount: 14800 },
        { bucket: "61–90 days", amount: 5200 },
        { bucket: "90+ days", amount: 2500 },
      ],
    },
    cache_hit: false,
    query_hash: "h-ar",
    filter_hash: "f-default",
    expires_at: null,
  },
  [QUERY_IDS.invByCat]: {
    result: {
      columns: ["sku", "qty"],
      rows: DEMO_ITEMS_RAW.map((it, i) => ({
        sku: it.sku,
        qty: [12, 4, 320, 1500, 80, 200, 15, 9][i] + [5, 0, 110, 800, 25, 60, 22, 2][i],
      })),
    },
    cache_hit: false,
    query_hash: "h-inv",
    filter_hash: "f-default",
    expires_at: null,
  },
  [QUERY_IDS.ticketsByPriority]: {
    result: {
      columns: ["priority", "count"],
      rows: [
        { priority: "urgent", count: 1 },
        { priority: "high", count: 2 },
        { priority: "medium", count: 1 },
        { priority: "low", count: 1 },
      ],
    },
    cache_hit: false,
    query_hash: "h-tickets",
    filter_hash: "f-default",
    expires_at: null,
  },
  [QUERY_IDS.totalPipelineValue]: {
    result: {
      columns: ["total"],
      rows: [{ total: 287500 }],
    },
    cache_hit: false,
    query_hash: "h-total",
    filter_hash: "f-default",
    expires_at: null,
  },
};

const DASHBOARD_ID = uuid("ins.dash:exec");
const WIDGET_IDS = [
  uuid("ins.w:pipeline-bar"),
  uuid("ins.w:ar-line"),
  uuid("ins.w:inv-pie"),
  uuid("ins.w:tot-card"),
  uuid("ins.w:tickets-table"),
];

const DASHBOARD_WIDGETS: InsightsWidget[] = [
  {
    tenant_id: DEMO_TENANT_ID,
    id: WIDGET_IDS[0],
    dashboard_id: DASHBOARD_ID,
    query_id: QUERY_IDS.pipelineByStage,
    viz_type: "bar",
    position: { x: 0, y: 0, w: 6, h: 4 },
    config: { title: "Pipeline by Stage", x_column: "stage", y_column: "total" },
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: WIDGET_IDS[1],
    dashboard_id: DASHBOARD_ID,
    query_id: QUERY_IDS.arBuckets,
    viz_type: "line",
    position: { x: 6, y: 0, w: 6, h: 4 },
    config: { title: "AR Aging", x_column: "bucket", y_column: "amount" },
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: WIDGET_IDS[2],
    dashboard_id: DASHBOARD_ID,
    query_id: QUERY_IDS.invByCat,
    viz_type: "pie",
    position: { x: 0, y: 4, w: 4, h: 4 },
    config: { title: "Inventory by SKU", category_column: "sku", value_column: "qty" },
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: WIDGET_IDS[3],
    dashboard_id: DASHBOARD_ID,
    query_id: QUERY_IDS.totalPipelineValue,
    viz_type: "number_card",
    position: { x: 4, y: 4, w: 3, h: 4 },
    config: { title: "Total Pipeline (USD)", value_column: "total", format: "currency" },
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: WIDGET_IDS[4],
    dashboard_id: DASHBOARD_ID,
    query_id: QUERY_IDS.ticketsByPriority,
    viz_type: "table",
    position: { x: 7, y: 4, w: 5, h: 4 },
    config: { title: "Open Tickets by Priority" },
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  },
];

const DASHBOARD: InsightsDashboard = {
  tenant_id: DEMO_TENANT_ID,
  id: DASHBOARD_ID,
  name: "Executive Overview",
  description: "Pipeline, AR aging, inventory mix, ticket triage",
  layout: { linked_filters: {} },
  auto_refresh_seconds: 0,
  created_by: null,
  created_at: LAST_WEEK_ISO,
  updated_at: NOW_ISO,
  widgets: DASHBOARD_WIDGETS,
};

export const INSIGHTS_DASHBOARDS: InsightsDashboard[] = [DASHBOARD];

export const INSIGHTS_DASHBOARD_BUNDLE: InsightsDashboardBundle = {
  dashboard: DASHBOARD,
  widget_results: Object.fromEntries(
    DASHBOARD_WIDGETS.map((w) => [w.id, w.query_id ? QUERY_RESULTS[w.query_id] ?? null : null])
  ),
};

export function widgetResultForQuery(queryId: string): InsightsRunResult | null {
  return QUERY_RESULTS[queryId] ?? null;
}

// --- Saved reports ----------------------------------------------------

export const SAVED_REPORTS: SavedReport[] = [
  {
    tenant_id: DEMO_TENANT_ID,
    id: uuid("report:1"),
    name: "Top deals by value",
    description: "Open opportunities sorted by deal value",
    definition: {
      source: "ktype:crm.deal",
      columns: ["name", "stage", "value", "owner"],
      sort: [{ column: "value", direction: "desc" }],
      limit: 25,
    },
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    tenant_id: DEMO_TENANT_ID,
    id: uuid("report:2"),
    name: "AR aging summary",
    description: "Outstanding AR by aging bucket and customer",
    definition: { source: "report:ar_aging", columns: ["bucket", "amount"] },
    created_by: null,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
];

// --- Saved views ------------------------------------------------------

export const SAVED_VIEWS_BY_KTYPE: Record<string, SavedView[]> = {
  "crm.deal": [
    {
      tenant_id: DEMO_TENANT_ID,
      id: uuid("view:deal-default"),
      user_id: EMP_IDS.ic1,
      ktype: "crm.deal",
      name: "All deals",
      filters: {},
      sort: "",
      columns: ["name", "stage", "value", "close_date"],
      is_default: true,
      shared: true,
      created_at: LAST_MONTH_ISO,
      updated_at: LAST_WEEK_ISO,
    },
  ],
};

// --- Search results ---------------------------------------------------

export function searchResults(query: string): SearchResponse {
  const q = query.toLowerCase();
  const buckets: KRecord[] = [];
  const candidates = [
    ...AR_INVOICES,
    ...AP_BILLS,
    ...DEALS,
    ...LEADS,
    ...TICKETS,
    ...PROJECTS,
    ...CONTACTS,
  ];
  for (const r of candidates) {
    const blob = JSON.stringify(r.data).toLowerCase();
    if (blob.includes(q)) buckets.push(r);
  }
  return {
    query,
    results: buckets.map((r, i) => ({ ...r, rank: 1 - i * 0.05 })),
  };
}

// --- Portal tickets (subset of helpdesk visible to a portal user) ----

export const PORTAL_TICKETS: KRecord[] = TICKETS.slice(0, 3);

// --- Dashboard summary ------------------------------------------------

export const DASHBOARD_SUMMARY: DashboardSummary = {
  open_deals_count: 5,
  pipeline_value: 125000,
  outstanding_ar: 45000,
  outstanding_ap: 18000,
  low_stock_items_count: 3,
  pending_approvals: 4,
  open_tickets_count: 4,
  overdue_tickets_count: 1,
  present_today: 8,
  pending_reviews: 2,
  base_currency: DEMO_BASE_CURRENCY,
};

// --- Aggregated record table ------------------------------------------

export const RECORDS_BY_KTYPE: Record<string, KRecord[]> = {
  "crm.lead": LEADS,
  "crm.contact": CONTACTS,
  "crm.organization": ORGANIZATIONS,
  "crm.deal": DEALS,
  "crm.activity": ACTIVITIES,
  "crm.quote": QUOTES,
  "tasks.task": [
    kr("tasks.task", "tk1", { title: "Reply to Hooli pricing question", assignee: "Mia P.", due_date: TODAY_ISO_DATE, status: "open" }),
    kr("tasks.task", "tk2", { title: "Prep April board pack", assignee: "Diana R.", due_date: todayPlus(3), status: "open" }),
    kr("tasks.task", "tk3", { title: "Roll Acme Robotics demo data", assignee: "Mateo C.", due_date: todayPlus(1), status: "open" }),
  ],
  "hr.employee": EMPLOYEES,
  "hr.leave_request": LEAVE_REQUESTS,
  "hr.attendance": ATTENDANCE,
  "hr.expense_claim": EXPENSE_CLAIMS,
  "hr.salary_component": SALARY_COMPONENTS,
  "hr.salary_structure": SALARY_STRUCTURES,
  "hr.pay_run": PAY_RUNS,
  "hr.payslip": PAY_RUN_PAYSLIPS,
  "hr.shift_type": SHIFT_TYPES,
  "hr.shift_assignment": SHIFT_ASSIGNMENTS,
  "inventory.item": INVENTORY_ITEM_RECORDS,
  "helpdesk.ticket": TICKETS,
  "projects.project": PROJECTS,
  "projects.milestone": MILESTONES,
  "sales.order": SALES_ORDERS,
  "procurement.purchase_order": PURCHASE_ORDERS,
  "sales.price_list": PRICE_LISTS,
  "sales.pos_profile": POS_PROFILES,
  "sales.pos_invoice": [],
  "lms.course": COURSES,
  "lms.module": MODULES,
  "lms.lesson": LESSONS,
  "lms.enrollment": ENROLLMENTS,
  "lms.progress": PROGRESS,
  "lms.quiz": QUIZZES,
  "lms.assignment": ASSIGNMENTS,
  "finance.cost_center": COST_CENTERS,
  "finance.bank_account": BANK_ACCOUNTS,
  "finance.bank_transaction": BANK_TXNS,
  "finance.ar_invoice": AR_INVOICES,
  "finance.ap_bill": AP_BILLS,
};

export function getKTypeByName(name: string): KType | undefined {
  return KTYPES_BY_NAME.get(name);
}
export const ALL_KTYPES = KTYPES;

// --- Marketplace ------------------------------------------------------
// Demo catalogue for the Marketplace Browse / Detail / Installed
// screens. Each listing carries the WS9 listing-metadata fields
// (category, screenshots, rating rollup) so the redesigned surfaces
// show populated states in demo mode rather than only their empty
// states. Two listings are seeded as already-installed for this tenant
// so the "Installed" badge and the verified-usage rating control are
// both demonstrable.

// xmlEscape guards the few user-facing strings interpolated into the
// inline-SVG screenshot data URIs below.
function xmlEscape(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

// demoShot builds a self-contained 16:10 SVG "app screenshot" data URI
// on the KChat violet palette. Kept inline (not network-fetched) so the
// gallery renders deterministically offline during screenshot capture
// and e2e runs. Real listings ship publisher-hosted HTTPS URLs; the
// demo substitutes these synthetic frames.
function demoShot(title: string, subtitle: string): string {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="800" viewBox="0 0 1280 800" role="img">
    <defs>
      <linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
        <stop offset="0" stop-color="#553BD8"/>
        <stop offset="1" stop-color="#8B79F2"/>
      </linearGradient>
    </defs>
    <rect width="1280" height="800" fill="#F4F2FF"/>
    <rect x="0" y="0" width="1280" height="132" fill="url(#g)"/>
    <circle cx="64" cy="66" r="24" fill="#FFFFFF" opacity="0.92"/>
    <rect x="104" y="48" width="360" height="18" rx="9" fill="#FFFFFF" opacity="0.9"/>
    <rect x="104" y="78" width="220" height="12" rx="6" fill="#FFFFFF" opacity="0.55"/>
    <rect x="1060" y="50" width="156" height="40" rx="20" fill="#FFFFFF" opacity="0.92"/>
    <rect x="64" y="196" width="540" height="540" rx="24" fill="#FFFFFF"/>
    <rect x="96" y="232" width="360" height="22" rx="11" fill="#191919" opacity="0.86"/>
    <rect x="96" y="276" width="280" height="14" rx="7" fill="#5B5B5B" opacity="0.5"/>
    <rect x="96" y="360" width="476" height="64" rx="14" fill="#F0EDFF"/>
    <rect x="96" y="440" width="476" height="64" rx="14" fill="#F0EDFF"/>
    <rect x="96" y="520" width="476" height="64" rx="14" fill="#F0EDFF"/>
    <rect x="96" y="600" width="476" height="64" rx="14" fill="#F0EDFF"/>
    <rect x="640" y="196" width="576" height="280" rx="24" fill="#FFFFFF"/>
    <rect x="672" y="232" width="220" height="18" rx="9" fill="#191919" opacity="0.78"/>
    <rect x="672" y="404" width="64" height="40" rx="8" fill="#553BD8"/>
    <rect x="760" y="368" width="64" height="76" rx="8" fill="#7C66F0"/>
    <rect x="848" y="324" width="64" height="120" rx="8" fill="#553BD8"/>
    <rect x="936" y="288" width="64" height="156" rx="8" fill="#8B79F2"/>
    <rect x="1024" y="344" width="64" height="100" rx="8" fill="#553BD8"/>
    <rect x="1112" y="300" width="64" height="144" rx="8" fill="#7C66F0"/>
    <rect x="640" y="500" width="576" height="236" rx="24" fill="#FFFFFF"/>
    <rect x="672" y="536" width="180" height="16" rx="8" fill="#191919" opacity="0.7"/>
    <circle cx="772" cy="648" r="76" fill="#F0EDFF"/>
    <path d="M772 572 A76 76 0 0 1 838 686 L772 648 Z" fill="#553BD8"/>
    <path d="M838 686 A76 76 0 0 1 724 712 L772 648 Z" fill="#8B79F2"/>
    <rect x="912" y="560" width="272" height="14" rx="7" fill="#F0EDFF"/>
    <rect x="912" y="596" width="240" height="14" rx="7" fill="#F0EDFF"/>
    <rect x="912" y="632" width="256" height="14" rx="7" fill="#F0EDFF"/>
    <rect x="912" y="668" width="208" height="14" rx="7" fill="#F0EDFF"/>
    <text x="96" y="328" font-family="'Mona Sans', sans-serif" font-size="30" font-weight="700" fill="#191919">${xmlEscape(title)}</text>
    <text x="672" y="704" font-family="'Mona Sans', sans-serif" font-size="20" fill="#5B5B5B">${xmlEscape(subtitle)}</text>
  </svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg.replace(/\n\s*/g, " "))}`;
}

let __extVersionSeq = 0;
function mkVersion(
  extId: string,
  version: string,
  publishedAt: string,
  opts: Partial<MarketplaceExtensionVersion> = {},
): MarketplaceExtensionVersion {
  __extVersionSeq += 1;
  return {
    id: uuid(`mkt-ver-${extId}-${version}`),
    extension_id: extId,
    version,
    bundle_hash: `sha256:${uuid(`hash-${extId}-${version}`).replace(/-/g, "")}`,
    bundle_size_bytes: opts.bundle_size_bytes ?? 1_400_000 + __extVersionSeq * 120_000,
    bundle_url: `https://bundles.kapp.example/${extId}/${version}.kpkg`,
    min_kapp_version: opts.min_kapp_version ?? "1.4.0",
    max_kapp_version: opts.max_kapp_version,
    features_required: opts.features_required ?? [],
    permissions_required: opts.permissions_required ?? [],
    ktypes_count: opts.ktypes_count ?? 0,
    workflows_count: opts.workflows_count ?? 0,
    agent_tools_count: opts.agent_tools_count ?? 0,
    ui_extensions_count: opts.ui_extensions_count ?? 0,
    webhooks_count: opts.webhooks_count ?? 0,
    yanked: false,
    published_at: publishedAt,
    bundle_signature: opts.bundle_signature ?? "ed25519:demo",
    bundle_signature_key_id: opts.bundle_signature_key_id ?? "kapp-publisher-2026",
    signed_at: opts.signed_at ?? publishedAt,
  };
}

const EXT_IDS = {
  inventory: uuid("mkt-ext-inventory-sync"),
  quickbooks: uuid("mkt-ext-quickbooks"),
  slack: uuid("mkt-ext-slack-notify"),
  leadScorer: uuid("mkt-ext-lead-scorer"),
  timesheet: uuid("mkt-ext-timesheet"),
  warehouse: uuid("mkt-ext-data-warehouse"),
} as const;

export const MARKETPLACE_VERSIONS: Record<string, MarketplaceExtensionVersion[]> = {
  [EXT_IDS.inventory]: [
    mkVersion(EXT_IDS.inventory, "2.3.1", LAST_WEEK_ISO, {
      features_required: ["inventory"],
      permissions_required: ["records.read", "records.write"],
      ktypes_count: 2,
      workflows_count: 1,
      webhooks_count: 2,
    }),
    mkVersion(EXT_IDS.inventory, "2.2.0", LAST_MONTH_ISO, {
      features_required: ["inventory"],
      permissions_required: ["records.read", "records.write"],
      ktypes_count: 2,
      webhooks_count: 1,
    }),
  ],
  [EXT_IDS.quickbooks]: [
    mkVersion(EXT_IDS.quickbooks, "4.1.0", LAST_WEEK_ISO, {
      features_required: ["finance"],
      permissions_required: ["finance.read", "finance.write"],
      ktypes_count: 1,
      workflows_count: 2,
      webhooks_count: 3,
    }),
    mkVersion(EXT_IDS.quickbooks, "4.0.0", LAST_MONTH_ISO, {
      features_required: ["finance"],
      permissions_required: ["finance.read", "finance.write"],
      ktypes_count: 1,
      workflows_count: 1,
      webhooks_count: 2,
    }),
  ],
  [EXT_IDS.slack]: [
    mkVersion(EXT_IDS.slack, "1.7.2", LAST_MONTH_ISO, {
      permissions_required: ["records.read"],
      agent_tools_count: 1,
      webhooks_count: 1,
    }),
  ],
  [EXT_IDS.leadScorer]: [
    mkVersion(EXT_IDS.leadScorer, "3.0.0", NOW_ISO, {
      features_required: ["crm"],
      permissions_required: ["records.read", "records.write"],
      ktypes_count: 1,
      agent_tools_count: 2,
      ui_extensions_count: 1,
    }),
  ],
  [EXT_IDS.timesheet]: [
    mkVersion(EXT_IDS.timesheet, "1.2.4", LAST_MONTH_ISO, {
      features_required: ["hr"],
      permissions_required: ["records.read", "records.write"],
      ktypes_count: 2,
      workflows_count: 1,
    }),
  ],
  [EXT_IDS.warehouse]: [
    mkVersion(EXT_IDS.warehouse, "0.9.0", NOW_ISO, {
      permissions_required: ["records.read"],
      agent_tools_count: 1,
      webhooks_count: 2,
    }),
  ],
};

function listedVersionString(extId: string): string {
  return MARKETPLACE_VERSIONS[extId][0].version;
}

export const MARKETPLACE_EXTENSIONS: MarketplaceExtension[] = [
  {
    id: EXT_IDS.inventory,
    name: "acme.inventory_sync",
    publisher: "acme",
    slug: "inventory_sync",
    display_name: "Inventory Sync",
    description:
      "Keep stock levels accurate across every warehouse. Inventory Sync reconciles your Kapp inventory with external WMS feeds in near real time, flags drift, and pushes adjustments back automatically.",
    author: "Acme Corp",
    license: "MIT",
    homepage: "https://acme.example/inventory-sync",
    support_email: "support@acme.example",
    status: "listed",
    listed_version: listedVersionString(EXT_IDS.inventory),
    category: "inventory",
    screenshots: [
      { url: demoShot("Live stock reconciliation", "Per-warehouse drift, resolved automatically"), caption: "Live reconciliation dashboard" },
      { url: demoShot("Adjustment history", "Every correction, fully audited"), caption: "Adjustment audit trail" },
      { url: demoShot("Warehouse coverage", "Connect every WMS feed in minutes") },
    ],
    rating_average: 4.6,
    rating_count: 218,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    id: EXT_IDS.quickbooks,
    name: "globex.quickbooks_connector",
    publisher: "globex",
    slug: "quickbooks_connector",
    display_name: "QuickBooks Connector",
    description:
      "Sync invoices, bills, and payments between Kapp Finance and QuickBooks Online. Two-way mapping for accounts, taxes, and customers keeps your books reconciled without copy-paste.",
    author: "Globex Financial",
    license: "Apache-2.0",
    homepage: "https://globex.example/quickbooks",
    support_email: "help@globex.example",
    status: "listed",
    listed_version: listedVersionString(EXT_IDS.quickbooks),
    category: "finance",
    screenshots: [
      { url: demoShot("Two-way ledger sync", "Invoices and bills, always reconciled"), caption: "Ledger sync overview" },
      { url: demoShot("Account mapping", "Map Kapp accounts to QuickBooks once"), caption: "Chart-of-accounts mapping" },
    ],
    rating_average: 4.8,
    rating_count: 342,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_WEEK_ISO,
  },
  {
    id: EXT_IDS.slack,
    name: "initech.slack_notify",
    publisher: "initech",
    slug: "slack_notify",
    display_name: "Slack Notifications",
    description:
      "Send rich Kapp notifications to Slack. Route record changes, approvals, and SLA breaches to the right channels with per-event filters and digest scheduling.",
    author: "Initech Labs",
    license: "MIT",
    support_email: "team@initech.example",
    status: "listed",
    listed_version: listedVersionString(EXT_IDS.slack),
    category: "communication",
    screenshots: [
      { url: demoShot("Channel routing", "The right alert in the right channel"), caption: "Per-event channel routing" },
    ],
    rating_average: 4.2,
    rating_count: 96,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_MONTH_ISO,
  },
  {
    id: EXT_IDS.leadScorer,
    name: "umbrella.lead_scorer",
    publisher: "umbrella",
    slug: "lead_scorer",
    display_name: "AI Lead Scorer",
    description:
      "Prioritise the pipeline that matters. AI Lead Scorer ranks open deals by likelihood to close using your historical win data, and surfaces the next best action for each rep.",
    author: "Umbrella AI",
    license: "Commercial",
    homepage: "https://umbrella.example/lead-scorer",
    support_email: "sales@umbrella.example",
    status: "listed",
    listed_version: listedVersionString(EXT_IDS.leadScorer),
    category: "sales",
    screenshots: [
      { url: demoShot("Pipeline scoring", "Every deal ranked by likelihood to close"), caption: "Scored pipeline view" },
      { url: demoShot("Next best action", "Tell each rep what to do next"), caption: "Per-deal recommendations" },
      { url: demoShot("Model insights", "Understand what drives every score") },
    ],
    rating_average: 4.9,
    rating_count: 174,
    created_at: LAST_WEEK_ISO,
    updated_at: NOW_ISO,
  },
  {
    id: EXT_IDS.timesheet,
    name: "soylent.timesheet_pro",
    publisher: "soylent",
    slug: "timesheet_pro",
    display_name: "Timesheet Pro",
    description:
      "Capture billable hours without the friction. Timesheet Pro adds one-tap timers, approval routing, and payroll-ready exports on top of Kapp HR.",
    author: "Soylent Works",
    license: "GPL-3.0",
    support_email: "support@soylent.example",
    status: "listed",
    listed_version: listedVersionString(EXT_IDS.timesheet),
    category: "hr",
    screenshots: [
      { url: demoShot("One-tap timers", "Start tracking in a single click"), caption: "Time capture" },
      { url: demoShot("Approval routing", "Managers approve in bulk") },
    ],
    rating_average: 3.8,
    rating_count: 41,
    created_at: LAST_MONTH_ISO,
    updated_at: LAST_MONTH_ISO,
  },
  {
    id: EXT_IDS.warehouse,
    name: "hooli.data_warehouse",
    publisher: "hooli",
    slug: "data_warehouse",
    display_name: "Data Warehouse Export",
    description:
      "Stream Kapp records into your analytics warehouse. Incremental change-data-capture to Snowflake, BigQuery, or Redshift with schema mapping you control.",
    author: "Hooli Data",
    license: "Apache-2.0",
    homepage: "https://hooli.example/warehouse",
    support_email: "data@hooli.example",
    status: "listed",
    listed_version: listedVersionString(EXT_IDS.warehouse),
    category: "analytics",
    screenshots: [
      { url: demoShot("Change-data-capture", "Stream every change incrementally"), caption: "CDC pipeline status" },
    ],
    // Newly published — no tenant has rated it yet, so Browse + Detail
    // render the "No ratings yet" state.
    rating_average: 0,
    rating_count: 0,
    created_at: NOW_ISO,
    updated_at: NOW_ISO,
  },
];

// This tenant's own saved ratings, keyed by extension id. Only the
// installed extensions can carry a rating (server gates submission on
// verified usage); Inventory Sync is rated, the QuickBooks install is
// not yet rated so the detail page shows the un-rated prompt.
export const MARKETPLACE_MY_RATINGS: Record<string, number> = {
  [EXT_IDS.inventory]: 5,
};

export const MARKETPLACE_INSTALLATIONS: MarketplaceInstallation[] = [
  {
    id: uuid("mkt-install-inventory"),
    tenant_id: DEMO_TENANT_ID,
    extension_id: EXT_IDS.inventory,
    // Installed on the previous version (2.2.0) while 2.3.1 is the
    // listed default, so the Installed list shows the "Update
    // available" badge.
    extension_version_id: MARKETPLACE_VERSIONS[EXT_IDS.inventory][1].id,
    status: "active",
    settings: { sync_interval_minutes: 15, warehouses: ["main", "west"] },
    webhook_base: "https://acme.kapp.example",
    installed_at: LAST_WEEK_ISO,
    updated_at: LAST_WEEK_ISO,
    last_health_check_at: NOW_ISO,
    last_health_check_status: "ok",
  },
  {
    id: uuid("mkt-install-quickbooks"),
    tenant_id: DEMO_TENANT_ID,
    extension_id: EXT_IDS.quickbooks,
    // Installed on an older version (4.0.0) than the listed default
    // (4.1.0) so the "Update available" badge shows on the Installed
    // list.
    extension_version_id: MARKETPLACE_VERSIONS[EXT_IDS.quickbooks][1].id,
    status: "active",
    settings: { realm_id: "demo-realm", auto_post: true },
    webhook_base: "https://acme.kapp.example",
    installed_at: LAST_MONTH_ISO,
    updated_at: LAST_MONTH_ISO,
    last_health_check_at: NOW_ISO,
    last_health_check_status: "ok",
  },
];
