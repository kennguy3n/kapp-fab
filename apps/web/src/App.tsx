import { lazy, Suspense, type ComponentType } from "react";
import {
  Link,
  NavLink,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Card,
  CardContent,
  Input,
  Sidebar,
  SidebarBody,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarItem,
  SidebarToggle,
  TooltipProvider,
  initials,
} from "@kapp/ui";
import { api } from "./lib/api";
import { NotificationBell } from "./components/NotificationBell";
import { LocaleSwitcher } from "./components/LocaleSwitcher";
import { LocaleProvider } from "./lib/i18n";

/**
 * Route-level code splitting.  Every page is loaded on first
 * navigation via React.lazy().  Vite's Rollup config emits one
 * chunk per dynamic import (see vite.config.ts `manualChunks` for
 * the shared-vendor split) which keeps the initial bundle small
 * — the dashboard route is the only page that loads at boot.
 *
 * `lazyNamed` is the helper for converting the project's
 * named-export pages (`export function FooPage`) into the
 * default-export shape React.lazy expects.  Using a helper instead
 * of inline `then(m => ({ default: m.X }))` makes the route list
 * scannable and prevents typos that would only surface when the
 * specific route is visited.
 */
// We deliberately type the component slot as `ComponentType<any>`
// because the lazy-route map covers pages with heterogeneous prop
// shapes (e.g. `SubledgerPage({ variant })`, `RecordListPage({
// defaultMode? })`, plus zero-prop pages).  React.lazy's return
// type is `LazyExoticComponent<ComponentType<any>>` regardless,
// so `any` here matches React's own typing — narrowing further
// (e.g. `ComponentType<unknown>`) would force each lazy-page
// callsite to assert its props, which doesn't add type safety
// (the routes pass concrete prop literals already, type-checked
// against the original page's signature).
//
// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyComponent = ComponentType<any>;

// `lazyNamed` converts a named-export page module into the
// default-export shape React.lazy expects.  The TName/TMod two-tuple
// constrains the name argument to `keyof TMod` at compile time:
// `tsc` infers `TMod` from the `import()` call's return type (the
// module's exported namespace), so a typo like
//
//   lazyNamed(() => import("./pages/RecordListPage"), "RecordListPge")
//
// fails type-checking — "RecordListPge" is not in
// `keyof typeof import("./pages/RecordListPage")`.  The previous
// signature (`Record<TName, AnyComponent>`) inferred TName solely from
// the second argument with no anchor against the module, so any
// string was accepted and the typo only surfaced at runtime when the
// route was visited (the dynamic import succeeded but
// `mod[name] === undefined`, which React.lazy then threw on).
//
// We do still need a runtime cast to AnyComponent inside the closure
// because we can't simultaneously constrain (a) "name is a real key
// of TMod" AND (b) "the value at that key is a ComponentType<TProps>"
// without losing the inference path on the import() return type.
// Compile-time typo safety is the load-bearing win — the runtime cast
// only fires when name is a valid key, so it can't mask the bug class
// the previous helper was vulnerable to.
function lazyNamed<TMod extends Record<string, unknown>>(
  loader: () => Promise<TMod>,
  name: Extract<keyof TMod, string>,
) {
  return lazy(async () => {
    const mod = await loader();
    return { default: mod[name] as AnyComponent };
  });
}

const RecordListPage = lazyNamed(
  () => import("./pages/RecordListPage"),
  "RecordListPage",
);
const RecordFormPage = lazyNamed(
  () => import("./pages/RecordFormPage"),
  "RecordFormPage",
);
const LoginPage = lazyNamed(() => import("./pages/LoginPage"), "LoginPage");
const TenantListPage = lazyNamed(
  () => import("./pages/TenantListPage"),
  "TenantListPage",
);
const FormPage = lazyNamed(() => import("./pages/FormPage"), "FormPage");
const ApprovalsPage = lazyNamed(
  () => import("./pages/ApprovalsPage"),
  "ApprovalsPage",
);
const AuditLogPage = lazyNamed(
  () => import("./pages/AuditLogPage"),
  "AuditLogPage",
);
const RoleManagementPage = lazyNamed(
  () => import("./pages/RoleManagementPage"),
  "RoleManagementPage",
);
const SubledgerPage = lazyNamed(
  () => import("./pages/SubledgerPage"),
  "SubledgerPage",
);
const ChartOfAccountsPage = lazyNamed(
  () => import("./pages/ChartOfAccountsPage"),
  "ChartOfAccountsPage",
);
const JournalEntriesPage = lazyNamed(
  () => import("./pages/JournalEntriesPage"),
  "JournalEntriesPage",
);
const TrialBalancePage = lazyNamed(
  () => import("./pages/TrialBalancePage"),
  "TrialBalancePage",
);
const IncomeStatementPage = lazyNamed(
  () => import("./pages/IncomeStatementPage"),
  "IncomeStatementPage",
);
const StockLevelsPage = lazyNamed(
  () => import("./pages/StockLevelsPage"),
  "StockLevelsPage",
);
const InventoryValuationPage = lazyNamed(
  () => import("./pages/InventoryValuationPage"),
  "InventoryValuationPage",
);
const OrgChartPage = lazyNamed(
  () => import("./pages/OrgChartPage"),
  "OrgChartPage",
);
const LearnerProgressPage = lazyNamed(
  () => import("./pages/LearnerProgressPage"),
  "LearnerProgressPage",
);
const ImportPage = lazyNamed(() => import("./pages/ImportPage"), "ImportPage");
const ImportMappingPage = lazyNamed(
  () => import("./pages/ImportMappingPage"),
  "ImportMappingPage",
);
const BankReconciliationPage = lazyNamed(
  () => import("./pages/BankReconciliationPage"),
  "BankReconciliationPage",
);
const CostCentersPage = lazyNamed(
  () => import("./pages/CostCentersPage"),
  "CostCentersPage",
);
const SalesOrdersPage = lazyNamed(
  () => import("./pages/SalesOrdersPage"),
  "SalesOrdersPage",
);
const PurchaseOrdersPage = lazyNamed(
  () => import("./pages/PurchaseOrdersPage"),
  "PurchaseOrdersPage",
);
const PriceListsPage = lazyNamed(
  () => import("./pages/PriceListsPage"),
  "PriceListsPage",
);
const PayrollPage = lazyNamed(
  () => import("./pages/PayrollPage"),
  "PayrollPage",
);
const ShiftCalendarPage = lazyNamed(
  () => import("./pages/ShiftCalendarPage"),
  "ShiftCalendarPage",
);
const SetupWizardPage = lazyNamed(
  () => import("./pages/SetupWizardPage"),
  "SetupWizardPage",
);
const DashboardPage = lazyNamed(
  () => import("./pages/DashboardPage"),
  "DashboardPage",
);
const ExchangeRatesPage = lazyNamed(
  () => import("./pages/ExchangeRatesPage"),
  "ExchangeRatesPage",
);
const HelpdeskPage = lazyNamed(
  () => import("./pages/HelpdeskPage"),
  "HelpdeskPage",
);
const ReportBuilderPage = lazyNamed(
  () => import("./pages/ReportBuilderPage"),
  "ReportBuilderPage",
);
const InsightsQueryBuilderPage = lazyNamed(
  () => import("./pages/InsightsQueryBuilderPage"),
  "InsightsQueryBuilderPage",
);
const KTypeBuilderPage = lazyNamed(
  () => import("./pages/KTypeBuilderPage"),
  "KTypeBuilderPage",
);
const InsightsDashboardPage = lazyNamed(
  () => import("./pages/InsightsDashboardPage"),
  "InsightsDashboardPage",
);
const InsightsDataSourcesPage = lazyNamed(
  () => import("./pages/InsightsDataSourcesPage"),
  "InsightsDataSourcesPage",
);
const InsightsEmbedPage = lazyNamed(
  () => import("./pages/InsightsEmbedPage"),
  "InsightsEmbedPage",
);
const POSPage = lazyNamed(() => import("./pages/POSPage"), "POSPage");
const ProjectGanttPage = lazyNamed(
  () => import("./pages/ProjectGanttPage"),
  "ProjectGanttPage",
);
const TenantFeaturesPage = lazyNamed(
  () => import("./pages/TenantFeaturesPage"),
  "TenantFeaturesPage",
);
const ConsolidationPage = lazyNamed(
  () => import("./pages/ConsolidationPage"),
  "ConsolidationPage",
);
const PlacementPolicyPage = lazyNamed(
  () => import("./pages/PlacementPolicyPage"),
  "PlacementPolicyPage",
);
const RetentionPoliciesPage = lazyNamed(
  () => import("./pages/RetentionPoliciesPage"),
  "RetentionPoliciesPage",
);
const UsageDashboardPage = lazyNamed(
  () => import("./pages/UsageDashboardPage"),
  "UsageDashboardPage",
);
const SearchPage = lazyNamed(() => import("./pages/SearchPage"), "SearchPage");
const WebhooksPage = lazyNamed(
  () => import("./pages/WebhooksPage"),
  "WebhooksPage",
);
const WebhookDeliveryLogPage = lazyNamed(
  () => import("./pages/WebhookDeliveryLogPage"),
  "WebhookDeliveryLogPage",
);
const PortalLoginPage = lazyNamed(
  () => import("./pages/portal/PortalLoginPage"),
  "PortalLoginPage",
);
const PortalTicketListPage = lazyNamed(
  () => import("./pages/portal/PortalTicketListPage"),
  "PortalTicketListPage",
);
const PortalTicketDetailPage = lazyNamed(
  () => import("./pages/portal/PortalTicketDetailPage"),
  "PortalTicketDetailPage",
);
const PortalNewTicketPage = lazyNamed(
  () => import("./pages/portal/PortalNewTicketPage"),
  "PortalNewTicketPage",
);

const tenantKey = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

/**
 * featureFromSection maps a nav-section title to the tenant
 * feature flag that gates it.  Sections without an entry are
 * always shown.  Kept in lock-step with
 * internal/tenant/plans.go FeatureX constants.
 */
const featureFromSection: Record<string, string> = {
  CRM: "crm",
  Finance: "finance",
  Helpdesk: "helpdesk",
  Inventory: "inventory",
  HR: "hr",
  LMS: "lms",
  Insights: "insights",
  POS: "pos",
  Projects: "projects",
};

interface NavSection {
  title: string;
  links: { to: string; label: string }[];
}

const navSections: NavSection[] = [
  {
    title: "Overview",
    links: [{ to: "/", label: "Dashboard" }],
  },
  {
    title: "CRM",
    links: [
      { to: "/records/crm.lead", label: "Leads" },
      { to: "/records/crm.contact", label: "Contacts" },
      { to: "/records/crm.organization", label: "Organizations" },
      { to: "/records/crm.deal", label: "Deals" },
      { to: "/records/crm.activity", label: "Activities" },
      { to: "/records/crm.quote", label: "Quotes" },
    ],
  },
  {
    title: "Work",
    links: [
      { to: "/records/tasks.task", label: "Tasks" },
      { to: "/approvals", label: "Approvals" },
    ],
  },
  {
    title: "Projects",
    links: [
      { to: "/projects/gantt", label: "Gantt" },
      { to: "/records/projects.project", label: "Projects" },
      { to: "/records/projects.milestone", label: "Milestones" },
    ],
  },
  {
    title: "Finance",
    links: [
      { to: "/records/finance.ar_invoice", label: "Invoices" },
      { to: "/records/finance.ap_bill", label: "Bills" },
      { to: "/records/finance.credit_note", label: "Credit Notes" },
      { to: "/records/finance.debit_note", label: "Debit Notes" },
      { to: "/records/finance.recurring_invoice", label: "Recurring Invoices" },
      { to: "/records/finance.payment_terms", label: "Payment Terms" },
      { to: "/finance/accounts", label: "Chart of Accounts" },
      { to: "/finance/journal", label: "Journal Entries" },
      { to: "/finance/reports/trial-balance", label: "Trial Balance" },
      { to: "/finance/reports/income-statement", label: "Income Statement" },
      { to: "/finance/ar-subledger", label: "AR Subledger" },
      { to: "/finance/ap-subledger", label: "AP Subledger" },
      { to: "/finance/cost-centers", label: "Cost Centers" },
      { to: "/finance/bank-reconciliation", label: "Bank Reconciliation" },
      { to: "/finance/exchange-rates", label: "Exchange Rates" },
      { to: "/reports", label: "Report Builder" },
    ],
  },
  {
    title: "Helpdesk",
    links: [
      { to: "/records/helpdesk.ticket", label: "Tickets" },
      { to: "/helpdesk", label: "SLA + Triage" },
    ],
  },
  {
    title: "Sales",
    links: [
      { to: "/sales/orders", label: "Sales Orders" },
      { to: "/sales/price-lists", label: "Price Lists" },
      { to: "/procurement/purchase-orders", label: "Purchase Orders" },
    ],
  },
  {
    title: "POS",
    links: [
      { to: "/pos", label: "Register" },
      { to: "/records/sales.pos_profile", label: "Profiles" },
      { to: "/records/sales.pos_invoice", label: "Receipts" },
    ],
  },
  {
    title: "Inventory",
    links: [
      { to: "/records/inventory.item", label: "Items" },
      { to: "/records/inventory.warehouse", label: "Warehouses" },
      { to: "/inventory/stock-levels", label: "Stock Levels" },
      { to: "/inventory/reports/valuation", label: "Valuation" },
    ],
  },
  {
    title: "HR",
    links: [
      { to: "/records/hr.employee", label: "Employees" },
      { to: "/hr/org-chart", label: "Org Chart" },
      { to: "/records/hr.leave_request", label: "Leave Requests" },
      { to: "/records/hr.attendance", label: "Attendance" },
      { to: "/records/hr.expense_claim", label: "Expense Claims" },
      { to: "/hr/payroll", label: "Payroll" },
      { to: "/hr/shifts", label: "Shift Schedule" },
    ],
  },
  {
    title: "LMS",
    links: [
      { to: "/records/lms.course", label: "Courses" },
      { to: "/records/lms.module", label: "Modules" },
      { to: "/records/lms.lesson", label: "Lessons" },
      { to: "/records/lms.enrollment", label: "Enrollments" },
      { to: "/records/lms.quiz", label: "Quizzes" },
      { to: "/records/lms.assignment", label: "Assignments" },
      { to: "/lms/progress", label: "Learner Progress" },
    ],
  },
  {
    title: "Insights",
    links: [
      { to: "/insights/queries", label: "Query Builder" },
      { to: "/insights/dashboards", label: "Dashboards" },
    ],
  },
  {
    title: "Admin",
    links: [
      { to: "/admin/tenants", label: "Tenants" },
      { to: "/admin/features", label: "Features" },
      { to: "/admin/placement", label: "Placement Policy" },
      { to: "/admin/retention", label: "Retention" },
      { to: "/admin/usage", label: "Usage" },
      { to: "/admin/audit", label: "Audit Log" },
      { to: "/admin/roles", label: "Roles" },
      { to: "/admin/webhooks", label: "Webhooks" },
      { to: "/admin/consolidation", label: "Consolidation" },
      { to: "/admin/ktypes/builder", label: "KType Builder" },
      { to: "/imports", label: "Imports" },
    ],
  },
];

/**
 * ShellRouteFallback is what users see in the gap between clicking
 * a nav item INSIDE the authenticated app shell and the route
 * chunk finishing its network round-trip.  The Card chrome
 * mirrors the page's eventual layout so the reflow when content
 * arrives is minimal — most pages render a top-level Card, so a
 * Card-shaped placeholder is the most layout-stable thing to
 * show.
 *
 * The Card is NOT appropriate for the public-route boundary
 * (login / portal / embed) because there's no sidebar or padding
 * context to anchor it — a stray bordered Card floating on a
 * blank viewport reads like a broken layout.  See
 * `PublicRouteFallback` for that path.
 */
function ShellRouteFallback() {
  return (
    <Card className="border-dashed">
      <CardContent className="flex items-center gap-3 py-12 text-fg-muted">
        <div
          role="status"
          aria-live="polite"
          className="inline-flex h-4 w-4 animate-spin rounded-full border-2 border-current border-r-transparent"
        />
        <span className="text-sm">Loading…</span>
      </CardContent>
    </Card>
  );
}

/**
 * PublicRouteFallback is the Suspense placeholder for the outer
 * routing boundary, which serves anonymous surfaces (login,
 * portal, the public form embed).  These routes have no app
 * shell, so a Card with design-system chrome looks like a broken
 * layout fragment.  We render a minimal centered spinner that
 * fills the viewport instead — it reads as “loading” without
 * leaking any tenant chrome onto a public surface.
 */
function PublicRouteFallback() {
  return (
    <div
      role="status"
      aria-live="polite"
      className="flex h-screen w-screen items-center justify-center bg-bg text-fg-muted"
    >
      <div className="inline-flex h-6 w-6 animate-spin rounded-full border-2 border-current border-r-transparent" />
      <span className="sr-only">Loading…</span>
    </div>
  );
}

export function App() {
  return (
    <LocaleProvider>
      <TooltipProvider delayDuration={300}>
        <Suspense fallback={<PublicRouteFallback />}>
        <Routes>
          {/* Public form route lives outside the app shell so anonymous
              visitors don't see tenant navigation. */}
          <Route path="/forms/:formId" element={<FormPage />} />
          <Route path="/login" element={<LoginPage />} />
          {/* Helpdesk customer portal. Runs outside the authenticated
              AppShell — portal users never see the tenant's internal
              nav/data; only their own tickets. */}
          <Route path="/portal/:tenant_slug" element={<PortalLoginPage />} />
          <Route
            path="/portal/:tenant_slug/tickets"
            element={<PortalTicketListPage />}
          />
          <Route
            path="/portal/:tenant_slug/tickets/new"
            element={<PortalNewTicketPage />}
          />
          <Route
            path="/portal/:tenant_slug/tickets/:id"
            element={<PortalTicketDetailPage />}
          />
          {/* Setup wizard is rendered outside the app shell because the
              tenant has no nav-worthy data until the wizard completes. */}
          <Route path="/setup/:id" element={<SetupWizardPage />} />
          {/* Public dashboard embed. Rendered without app chrome or
              auth so it can be iframed into any external surface.  The
              owning tenant's rate-limit bucket is enforced server-side. */}
          <Route path="/embed/:token" element={<InsightsEmbedPage />} />
          <Route path="/*" element={<AppShell />} />
        </Routes>
        </Suspense>
      </TooltipProvider>
    </LocaleProvider>
  );
}

/**
 * GlobalSearchBox — the shell-level search input.  Submitting routes
 * to /search?q=... which SearchPage debounces and executes via the
 * /api/v1/search endpoint.  The input is the @kapp/ui `Input`
 * primitive so it inherits the same chrome and focus ring as every
 * other form field.
 */
function GlobalSearchBox() {
  const nav = useNavigate();
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        const v = new FormData(e.currentTarget).get("q");
        const trimmed = (typeof v === "string" ? v : "").trim();
        if (!trimmed) return;
        nav(`/search?q=${encodeURIComponent(trimmed)}`);
      }}
      className="flex-1 max-w-md"
    >
      <Input
        type="search"
        name="q"
        placeholder="Search records…"
        aria-label="Global search"
        leadingAddon={
          <svg
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            className="h-4 w-4"
          >
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
        }
      />
    </form>
  );
}

/**
 * AppNavLink is the tenant-shell sidebar item.  We use the
 * `renderAnchor` escape hatch on `<SidebarItem>` to inject a
 * react-router `<NavLink>` so client-side navigation works AND
 * the active state comes from the router's resolved match
 * (not a manual location.pathname compare, which would miss
 * params and nested routes).
 *
 * Defined as a top-level component instead of inline-in-the-map
 * to keep its memoised `renderAnchor` identity stable across
 * re-renders of `<AppShell>` and prevent SidebarItem from
 * re-resolving the active state when only the parent's query
 * cache updated.
 */
function AppNavLink({ to, label }: { to: string; label: string }) {
  // Render-prop bridge so `<NavLink>` controls the href + active
  // state but SidebarItem still owns the chrome (icon slot,
  // collapsed-mode tooltip, badge) AND owns the class
  // composition.  We delegate class generation to SidebarItem via
  // `getClassName(isActive)` rather than string-concatenating
  // active modifiers onto an inactive base; this routes through
  // tailwind-merge inside SidebarItem so the conflicting
  // `hover:text-fg` (inactive base) and `hover:text-accent`
  // (active state) classes resolve deterministically instead of
  // leaving the muted hover live on the active link.
  return (
    <SidebarItem
      label={label}
      renderAnchor={({ getClassName, ref, children }) => (
        <NavLink
          ref={ref}
          to={to}
          className={({ isActive }) => getClassName(isActive)}
          end={to === "/"}
        >
          {children}
        </NavLink>
      )}
    />
  );
}

function AppShell() {
  const location = useLocation();
  const featuresQuery = useQuery({
    queryKey: ["tenant-features", tenantKey()],
    queryFn: () => api.listTenantFeatures(tenantKey()),
    retry: false,
    staleTime: 60_000,
  });
  const features = featuresQuery.data?.features ?? {};
  // Fail-open: when the features API is unreachable we still show
  // every nav item rather than hiding the entire app on a
  // transient network blip.  The backend will 403 disabled
  // sections if the user actually navigates to them.
  const visible = navSections.filter((s) => {
    const key = featureFromSection[s.title];
    if (!key) return true;
    if (!featuresQuery.data) return true;
    return features[key] !== false;
  });

  // Heuristic label for the active route — shown in the header to
  // confirm to the user which page they're on (especially valuable
  // when the sidebar is collapsed).  Walks navSections looking for
  // the link whose `to` is a prefix of the current path; the most
  // specific (longest) match wins so `/records/crm.lead/new` picks
  // "Leads" over "Dashboard".
  let activeLabel = "";
  let activePrefixLen = -1;
  for (const section of navSections) {
    for (const link of section.links) {
      if (
        location.pathname === link.to ||
        (link.to !== "/" && location.pathname.startsWith(`${link.to}/`))
      ) {
        if (link.to.length > activePrefixLen) {
          activeLabel = link.label;
          activePrefixLen = link.to.length;
        }
      }
    }
  }

  return (
    <div className="flex min-h-screen bg-bg">
      <Sidebar defaultCollapsed={false}>
        <SidebarHeader>
          <Link to="/" className="flex items-center gap-2">
            <div className="flex h-7 w-7 items-center justify-center rounded-md bg-accent text-accent-fg font-bold">
              K
            </div>
            <span className="font-semibold tracking-tight">Kapp</span>
          </Link>
          <div className="ms-auto">
            <SidebarToggle />
          </div>
        </SidebarHeader>
        <SidebarBody>
          {visible.map((section) => (
            <SidebarGroup key={section.title} title={section.title}>
              {section.links.map((link) => (
                <AppNavLink key={link.to} to={link.to} label={link.label} />
              ))}
            </SidebarGroup>
          ))}
        </SidebarBody>
        <SidebarFooter>
          <Avatar size="sm">
            <AvatarFallback>{initials(tenantKey())}</AvatarFallback>
          </Avatar>
          <div className="flex flex-col min-w-0 flex-1">
            <span className="text-sm truncate">{tenantKey()}</span>
            <span className="text-[10px] uppercase tracking-wider text-fg-subtle">
              tenant
            </span>
          </div>
        </SidebarFooter>
      </Sidebar>
      <main className="flex-1 flex flex-col min-w-0">
        <header className="flex h-14 shrink-0 items-center gap-3 border-b border-border bg-bg-elevated px-6">
          <GlobalSearchBox />
          <div className="ms-auto flex items-center gap-2">
            {activeLabel && (
              <Badge variant="outline" className="hidden md:inline-flex">
                {activeLabel}
              </Badge>
            )}
            <LocaleSwitcher className="hidden md:inline-flex w-auto" />
            <NotificationBell />
          </div>
        </header>
        <div className="flex-1 p-6 overflow-auto">
          <Suspense fallback={<ShellRouteFallback />}>
            <Routes>
              <Route path="/" element={<DashboardPage />} />
              <Route path="/admin/tenants" element={<TenantListPage />} />
              <Route
                path="/admin/consolidation"
                element={<ConsolidationPage />}
              />
              <Route path="/admin/features" element={<TenantFeaturesPage />} />
              <Route
                path="/admin/placement"
                element={<PlacementPolicyPage />}
              />
              <Route
                path="/admin/retention"
                element={<RetentionPoliciesPage />}
              />
              <Route path="/admin/usage" element={<UsageDashboardPage />} />
              <Route path="/admin/audit" element={<AuditLogPage />} />
              <Route path="/admin/roles" element={<RoleManagementPage />} />
              <Route
                path="/admin/ktypes/builder"
                element={<KTypeBuilderPage />}
              />
              <Route path="/approvals" element={<ApprovalsPage />} />
              <Route
                path="/finance/exchange-rates"
                element={<ExchangeRatesPage />}
              />
              <Route path="/helpdesk" element={<HelpdeskPage />} />
              <Route path="/reports" element={<ReportBuilderPage />} />
              <Route
                path="/insights/queries"
                element={<InsightsQueryBuilderPage />}
              />
              <Route
                path="/insights/dashboards"
                element={<InsightsDashboardPage />}
              />
              <Route
                path="/insights/data-sources"
                element={<InsightsDataSourcesPage />}
              />
              <Route path="/search" element={<SearchPage />} />
              <Route path="/admin/webhooks" element={<WebhooksPage />} />
              <Route
                path="/admin/webhooks/:id/deliveries"
                element={<WebhookDeliveryLogPage />}
              />
              <Route
                path="/finance/accounts"
                element={<ChartOfAccountsPage />}
              />
              <Route path="/finance/journal" element={<JournalEntriesPage />} />
              <Route
                path="/finance/reports/trial-balance"
                element={<TrialBalancePage />}
              />
              <Route
                path="/finance/reports/income-statement"
                element={<IncomeStatementPage />}
              />
              <Route
                path="/finance/ar-subledger"
                element={<SubledgerPage variant="ar" />}
              />
              <Route
                path="/finance/ap-subledger"
                element={<SubledgerPage variant="ap" />}
              />
              <Route
                path="/inventory/stock-levels"
                element={<StockLevelsPage />}
              />
              <Route
                path="/inventory/reports/valuation"
                element={<InventoryValuationPage />}
              />
              <Route path="/hr/org-chart" element={<OrgChartPage />} />
              <Route path="/hr/payroll" element={<PayrollPage />} />
              <Route path="/hr/shifts" element={<ShiftCalendarPage />} />
              <Route path="/pos" element={<POSPage />} />
              <Route path="/projects/gantt" element={<ProjectGanttPage />} />
              <Route
                path="/finance/cost-centers"
                element={<CostCentersPage />}
              />
              <Route
                path="/finance/bank-reconciliation"
                element={<BankReconciliationPage />}
              />
              <Route path="/sales/orders" element={<SalesOrdersPage />} />
              <Route
                path="/sales/price-lists"
                element={<PriceListsPage />}
              />
              <Route
                path="/procurement/purchase-orders"
                element={<PurchaseOrdersPage />}
              />
              <Route path="/imports" element={<ImportPage />} />
              <Route path="/imports/new" element={<ImportPage />} />
              <Route path="/imports/:id" element={<ImportPage />} />
              <Route
                path="/imports/:id/mapping"
                element={<ImportMappingPage />}
              />
              <Route path="/lms/progress" element={<LearnerProgressPage />} />
              <Route
                path="/lms/progress/:enrollmentId"
                element={<LearnerProgressPage />}
              />
              <Route path="/records/:ktype" element={<RecordListPage />} />
              <Route
                path="/records/:ktype/new"
                element={<RecordFormPage />}
              />
              <Route
                path="/records/:ktype/:id"
                element={<RecordFormPage />}
              />
              {/* /kanban/:ktype is a deep-link alias that forces the
                  kanban view via the defaultMode prop. RecordListPage
                  still allows the user to toggle to the list view;
                  defaultMode is only the initial mode, not a lock. */}
              <Route
                path="/kanban/:ktype"
                element={<RecordListPage defaultMode="kanban" />}
              />
            </Routes>
          </Suspense>
        </div>
      </main>
    </div>
  );
}
