// Demo / screenshot mock data layer.
//
// Populated when the app boots with VITE_DEMO_MODE=true. Every fixture
// below describes a fictional company "Acme Corp" (slug `acme`) and is
// matched to the TypeScript shapes exported from `packages/client/src/index.ts`.
// IDs use deterministic UUIDs so screenshot diffs stay stable across runs.

import type {
  Approval,
  AuditEntry,
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
  JournalEntry,
  KRecord,
  KType,
  Plan,
  PlacementPolicy,
  RetentionPolicy,
  SLAPolicy,
  SavedReport,
  SavedView,
  SearchResponse,
  StockLevel,
  Tenant,
  TenantFeaturesResponse,
  TenantUsageHistoryResponse,
  TenantUsageResponse,
  TrialBalanceReport,
  Webhook,
  WebhookDelivery,
} from "@kapp/client";

// --- Constants --------------------------------------------------------

export const DEMO_TENANT_ID = "00000000-0000-0000-0000-000000000001";
export const DEMO_TENANT_SLUG = "acme";
export const DEMO_BASE_CURRENCY = "USD";

const TODAY = new Date();
const NOW_ISO = TODAY.toISOString();
const LAST_WEEK_ISO = new Date(TODAY.getTime() - 7 * 86400_000).toISOString();
const LAST_MONTH_ISO = new Date(TODAY.getTime() - 30 * 86400_000).toISOString();
const NEXT_WEEK_ISO = new Date(TODAY.getTime() + 7 * 86400_000).toISOString();

function isoDate(d: Date): string {
  return d.toISOString().slice(0, 10);
}
function addDays(d: Date, n: number): Date {
  const out = new Date(d);
  out.setDate(out.getDate() + n);
  return out;
}
const TODAY_ISO_DATE = isoDate(TODAY);

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
  kr("crm.deal", "d1", { name: "Globex — Annual License", organization_id: ORG_IDS.globex, value: 42000, currency: "USD", stage: "prospecting", owner: "Avery N.", close_date: isoDate(addDays(TODAY, 30)) }),
  kr("crm.deal", "d2", { name: "Initech — Pilot Expansion", organization_id: ORG_IDS.initech, value: 18000, currency: "USD", stage: "qualification", owner: "Sam K.", close_date: isoDate(addDays(TODAY, 25)) }),
  kr("crm.deal", "d3", { name: "Hooli — Enterprise Tier", organization_id: ORG_IDS.hooli, value: 124000, currency: "USD", stage: "proposal", owner: "Mia P.", close_date: isoDate(addDays(TODAY, 14)) }),
  kr("crm.deal", "d4", { name: "Umbrella — POS Rollout", organization_id: ORG_IDS.umbrella, value: 67500, currency: "USD", stage: "negotiation", owner: "Sam K.", close_date: isoDate(addDays(TODAY, 7)) }),
  kr("crm.deal", "d5", { name: "Globex — Q1 Renewal", organization_id: ORG_IDS.globex, value: 36000, currency: "USD", stage: "closed_won", owner: "Avery N.", close_date: isoDate(addDays(TODAY, -3)) }),
];

const ACTIVITIES: KRecord[] = [
  kr("crm.activity", "a1", { subject: "Discovery call — Hooli", kind: "call", due_date: isoDate(TODAY), owner: "Mia P.", status: "open" }),
  kr("crm.activity", "a2", { subject: "Send proposal — Umbrella", kind: "email", due_date: isoDate(addDays(TODAY, 2)), owner: "Sam K.", status: "open" }),
  kr("crm.activity", "a3", { subject: "Onsite demo — Globex", kind: "meeting", due_date: isoDate(addDays(TODAY, 5)), owner: "Avery N.", status: "open" }),
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
  kr("hr.leave_request", "lr1", { employee_id: EMP_IDS.ic1, leave_type: "vacation", start_date: isoDate(addDays(TODAY, 10)), end_date: isoDate(addDays(TODAY, 15)), status: "pending" }),
  kr("hr.leave_request", "lr2", { employee_id: EMP_IDS.ic3, leave_type: "sick", start_date: isoDate(addDays(TODAY, -2)), end_date: isoDate(addDays(TODAY, -1)), status: "approved" }),
  kr("hr.leave_request", "lr3", { employee_id: EMP_IDS.mgrSales, leave_type: "vacation", start_date: isoDate(addDays(TODAY, 30)), end_date: isoDate(addDays(TODAY, 37)), status: "pending" }),
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
  return isoDate(addDays(TODAY, n));
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
const BANK_ACCOUNTS: KRecord[] = [
  { ...kr("finance.bank_account", "main", { name: "Acme Operating — USD", currency: "USD", account_number: "****4137" }), id: BANK_ACCOUNT_ID },
];

const BANK_TXNS: KRecord[] = [
  kr("finance.bank_transaction", "bt1", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-7), description: "Wire — Globex AR-2026-0001", amount: 4200, currency: "USD", status: "matched" }),
  kr("finance.bank_transaction", "bt2", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-5), description: "ACH — Initech AR-2026-0002", amount: 9800, currency: "USD", status: "matched" }),
  kr("finance.bank_transaction", "bt3", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-3), description: "Card — POS daily settlement", amount: 1240.5, currency: "USD", status: "unmatched" }),
  kr("finance.bank_transaction", "bt4", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-2), description: "Bill payment — Umbrella PO-2026-0001", amount: -11200, currency: "USD", status: "matched" }),
  kr("finance.bank_transaction", "bt5", { bank_account_id: BANK_ACCOUNT_ID, value_date: todayPlus(-1), description: "Bank fee", amount: -25, currency: "USD", status: "ignored" }),
];

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
    { account_code: "3010", account_name: "Common Stock", type: "equity", debit: "0.00", credit: "200000.00", balance: "200000.00" },
    { account_code: "3020", account_name: "Retained Earnings", type: "equity", debit: "0.00", credit: "44900.00", balance: "44900.00" },
    { account_code: "4010", account_name: "Product Revenue", type: "revenue", debit: "0.00", credit: "215000.00", balance: "215000.00" },
    { account_code: "4020", account_name: "Service Revenue", type: "revenue", debit: "0.00", credit: "82000.00", balance: "82000.00" },
    { account_code: "5000", account_name: "Cost of Goods Sold", type: "expense", debit: "94000.00", credit: "0.00", balance: "94000.00" },
    { account_code: "6010", account_name: "Salaries & Wages", type: "expense", debit: "154800.00", credit: "0.00", balance: "154800.00" },
    { account_code: "6020", account_name: "Rent", type: "expense", debit: "8500.00", credit: "0.00", balance: "8500.00" },
    { account_code: "6030", account_name: "Marketing", type: "expense", debit: "20500.00", credit: "0.00", balance: "20500.00" },
  ],
  total_debit: "751300.00",
  total_credit: "601300.00",
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
