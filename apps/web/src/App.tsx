import {
  Fragment,
  lazy,
  Suspense,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ComponentType,
  type ReactNode,
} from "react";
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
  Breadcrumb,
  BreadcrumbList,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbPage,
  BreadcrumbSeparator,
  Card,
  CardContent,
  CommandPalette,
  Input,
  Sidebar,
  SidebarBody,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarItem,
  SidebarToggle,
  Spinner,
  Toaster,
  TooltipProvider,
  initials,
  type CommandGroup,
} from "@kapp/ui";
import {
  Activity,
  Archive,
  ArrowLeftRight,
  Award,
  Banknote,
  BarChart3,
  BookOpen,
  BookText,
  BookUser,
  Boxes,
  Building,
  Building2,
  Calendar,
  CalendarClock,
  CheckSquare,
  ClipboardCheck,
  ClipboardList,
  Clock,
  Combine,
  Contact,
  CreditCard,
  Database,
  Factory,
  FileBarChart,
  FileMinus,
  FileSpreadsheet,
  FileText,
  FolderKanban,
  GanttChart,
  Gauge,
  GraduationCap,
  Handshake,
  Headphones,
  HeartPulse,
  HelpCircle,
  Landmark,
  Layers,
  LayoutDashboard,
  LayoutGrid,
  LifeBuoy,
  MapPin,
  MessageSquare,
  Milestone,
  Network,
  Rss,
  Package,
  PackageSearch,
  PiggyBank,
  PieChart,
  Plus,
  Puzzle,
  Receipt,
  ReceiptText,
  Repeat,
  Route as RouteIcon,
  Scale,
  ScanLine,
  ScrollText,
  Search,
  Settings,
  Shield,
  ShoppingCart,
  Stamp,
  Store,
  Tags,
  ToggleLeft,
  TrendingUp,
  Truck,
  Undo2,
  Upload,
  UserCog,
  UserPlus,
  UserSquare,
  Users,
  Warehouse,
  Webhook,
} from "lucide-react";
import type { SearchResult } from "@kapp/client";
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
const CallbackPage = lazyNamed(
  () => import("./pages/CallbackPage"),
  "CallbackPage",
);
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
const BOMPage = lazyNamed(() => import("./pages/BOMPage"), "BOMPage");
const WorkOrdersPage = lazyNamed(
  () => import("./pages/WorkOrdersPage"),
  "WorkOrdersPage",
);
const RoutingPage = lazyNamed(
  () => import("./pages/RoutingPage"),
  "RoutingPage",
);
const CapacityPlanningPage = lazyNamed(
  () => import("./pages/CapacityPlanningPage"),
  "CapacityPlanningPage",
);
const JobCardPage = lazyNamed(
  () => import("./pages/JobCardPage"),
  "JobCardPage",
);
const MrpPage = lazyNamed(() => import("./pages/MrpPage"), "MrpPage");
const SubcontractingPage = lazyNamed(
  () => import("./pages/SubcontractingPage"),
  "SubcontractingPage",
);
const LandedCostPage = lazyNamed(
  () => import("./pages/LandedCostPage"),
  "LandedCostPage",
);
const CycleCountPage = lazyNamed(
  () => import("./pages/CycleCountPage"),
  "CycleCountPage",
);
const OrgChartPage = lazyNamed(
  () => import("./pages/OrgChartPage"),
  "OrgChartPage",
);
const LearnerProgressPage = lazyNamed(
  () => import("./pages/LearnerProgressPage"),
  "LearnerProgressPage",
);
const LearningPathsPage = lazyNamed(
  () => import("./pages/LearningPathsPage"),
  "LearningPathsPage",
);
const InstructorDashboardPage = lazyNamed(
  () => import("./pages/InstructorDashboardPage"),
  "InstructorDashboardPage",
);
const DiscussionsPage = lazyNamed(
  () => import("./pages/DiscussionsPage"),
  "DiscussionsPage",
);
const BadgesPage = lazyNamed(() => import("./pages/BadgesPage"), "BadgesPage");
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
const SalesReturnsPage = lazyNamed(
  () => import("./pages/SalesReturnsPage"),
  "SalesReturnsPage",
);
const PurchaseRequisitionsPage = lazyNamed(
  () => import("./pages/PurchaseRequisitionsPage"),
  "PurchaseRequisitionsPage",
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
const RecruitmentDashboardPage = lazyNamed(
  () => import("./pages/RecruitmentDashboardPage"),
  "RecruitmentDashboardPage",
);
const BankFeedsPage = lazyNamed(
  () => import("./pages/BankFeedsPage"),
  "BankFeedsPage",
);
const JobOpeningsPage = lazyNamed(
  () => import("./pages/JobOpeningsPage"),
  "JobOpeningsPage",
);
const ApplicationsPage = lazyNamed(
  () => import("./pages/ApplicationsPage"),
  "ApplicationsPage",
);
const InterviewSchedulePage = lazyNamed(
  () => import("./pages/InterviewSchedulePage"),
  "InterviewSchedulePage",
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
const BudgetPage = lazyNamed(
  () => import("./pages/BudgetPage"),
  "BudgetPage",
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
// Workstream 6 — public status page + admin operator health dashboard.
const StatusPage = lazyNamed(() => import("./pages/StatusPage"), "StatusPage");
const AdminHealthPage = lazyNamed(
  () => import("./pages/AdminHealthPage"),
  "AdminHealthPage",
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
const MarketplaceBrowsePage = lazyNamed(
  () => import("./pages/marketplace/MarketplaceBrowsePage"),
  "MarketplaceBrowsePage",
);
const MarketplaceExtensionDetailPage = lazyNamed(
  () => import("./pages/marketplace/MarketplaceExtensionDetailPage"),
  "MarketplaceExtensionDetailPage",
);
const MarketplaceInstallationsPage = lazyNamed(
  () => import("./pages/marketplace/MarketplaceInstallationsPage"),
  "MarketplaceInstallationsPage",
);
const InstallationDetailPage = lazyNamed(
  () => import("./pages/marketplace/InstallationDetailPage"),
  "InstallationDetailPage",
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
  // Sales (Sales Orders / Returns / Price Lists / Purchase Orders /
  // Requisitions) rides the same inventory feature key as Inventory
  // because the backend gates these surfaces through TWO
  // complementary paths in internal/platform/feature_middleware.go,
  // both ultimately mapping to tenant.FeatureInventory:
  //   • Direct sub-domain routes (e.g. /api/v1/sales/returns/{id}/
  //     {verb}, /api/v1/procurement/requisitions/{id}/{verb}) —
  //     handled by FeatureFromPath's `case "inventory",
  //     "procurement", "sales":` switch, the only domain prefix
  //     gate for /api/v1/sales/* and /api/v1/procurement/* in this
  //     branch.
  //   • Generic KRecord CRUD (/api/v1/records/{ktype}/...) — handled
  //     by featureFromKType, whose `case "inventory", "procurement",
  //     "warehouse", "sales":` arm pins every domain KType under
  //     the same inventory plan flag (so sales.*, procurement.*,
  //     and warehouse.* KRecords all 403 without inventory).
  // Without this `Sales: "inventory"` entry the Sales nav block
  // would render for tenants without inventory in their plan and
  // clicking any link would 403 from the backend gate.
  Sales: "inventory",
  HR: "hr",
  LMS: "lms",
  Insights: "insights",
  POS: "pos",
  Projects: "projects",
  Manufacturing: "manufacturing",
};

interface NavLink {
  to: string;
  label: string;
  // Icon rendered in the sidebar item's icon slot (and reused as
  // the command-palette entry glyph).  A lucide-react element sized
  // to the 16px slot via `h-4 w-4`.
  icon?: ReactNode;
  // Optional additional feature gates that must ALL be enabled
  // for this link to render, beyond the section-level gate.  Used
  // when a single surface depends on more than one tenant plan
  // flag — e.g. /inventory/landed-costs writes inventory_moves AND
  // posts a ledger JE, so the route is gated on FeatureInventory
  // AND FeatureFinance in services/api/routes.go.  The section
  // ('Inventory' → 'inventory') covers one of the two; declaring
  // `requires: ['finance']` here keeps the nav link in lock-step
  // with the backend so a tenant on inventory-only plan doesn't
  // see a link that 403s on click.
  requires?: string[];
}

interface NavSection {
  title: string;
  links: NavLink[];
}

// Sidebar icons are sized to the 16px SidebarItem slot.  Defined
// once so every nav entry, and the command-palette mirror, share
// the same glyph dimensions.
const navIcon = (Icon: ComponentType<{ className?: string }>): ReactNode => (
  <Icon className="h-4 w-4" />
);

const navSections: NavSection[] = [
  {
    title: "Overview",
    links: [{ to: "/", label: "Dashboard", icon: navIcon(LayoutDashboard) }],
  },
  {
    title: "CRM",
    links: [
      { to: "/records/crm.lead", label: "Leads", icon: navIcon(Users) },
      {
        to: "/records/crm.contact",
        label: "Contacts",
        icon: navIcon(Contact),
      },
      {
        to: "/records/crm.organization",
        label: "Organizations",
        icon: navIcon(Building2),
      },
      { to: "/records/crm.deal", label: "Deals", icon: navIcon(Handshake) },
      {
        to: "/records/crm.activity",
        label: "Activities",
        icon: navIcon(CalendarClock),
      },
      { to: "/records/crm.quote", label: "Quotes", icon: navIcon(FileText) },
    ],
  },
  {
    title: "Work",
    links: [
      {
        to: "/records/tasks.task",
        label: "Tasks",
        icon: navIcon(CheckSquare),
      },
      { to: "/approvals", label: "Approvals", icon: navIcon(Stamp) },
    ],
  },
  {
    title: "Projects",
    links: [
      { to: "/projects/gantt", label: "Gantt", icon: navIcon(GanttChart) },
      {
        to: "/records/projects.project",
        label: "Projects",
        icon: navIcon(FolderKanban),
      },
      {
        to: "/records/projects.milestone",
        label: "Milestones",
        icon: navIcon(Milestone),
      },
    ],
  },
  {
    title: "Finance",
    links: [
      {
        to: "/records/finance.ar_invoice",
        label: "Invoices",
        icon: navIcon(Receipt),
      },
      {
        to: "/records/finance.ap_bill",
        label: "Bills",
        icon: navIcon(FileSpreadsheet),
      },
      {
        to: "/records/finance.credit_note",
        label: "Credit Notes",
        icon: navIcon(CreditCard),
      },
      {
        to: "/records/finance.debit_note",
        label: "Debit Notes",
        icon: navIcon(FileMinus),
      },
      {
        to: "/records/finance.recurring_invoice",
        label: "Recurring Invoices",
        icon: navIcon(Repeat),
      },
      {
        to: "/records/finance.payment_terms",
        label: "Payment Terms",
        icon: navIcon(CalendarClock),
      },
      {
        to: "/finance/accounts",
        label: "Chart of Accounts",
        icon: navIcon(BookOpen),
      },
      {
        to: "/finance/journal",
        label: "Journal Entries",
        icon: navIcon(BookText),
      },
      {
        to: "/finance/reports/trial-balance",
        label: "Trial Balance",
        icon: navIcon(Scale),
      },
      {
        to: "/finance/reports/income-statement",
        label: "Income Statement",
        icon: navIcon(TrendingUp),
      },
      {
        to: "/finance/ar-subledger",
        label: "AR Subledger",
        icon: navIcon(BookUser),
      },
      {
        to: "/finance/ap-subledger",
        label: "AP Subledger",
        icon: navIcon(BookUser),
      },
      {
        to: "/finance/cost-centers",
        label: "Cost Centers",
        icon: navIcon(Building),
      },
      {
        to: "/finance/bank-reconciliation",
        label: "Bank Reconciliation",
        icon: navIcon(Landmark),
      },
      {
        to: "/finance/bank-feeds",
        label: "Bank Feeds",
        icon: navIcon(Rss),
        // Gated on FeatureBankFeed on top of the Finance section gate,
        // matching the backend's /api/v1/finance/bank-feeds/* routes
        // (dynamic FeatureBankFeed + static FeatureFinance).
        requires: ["bankfeed"],
      },
      {
        to: "/finance/exchange-rates",
        label: "Exchange Rates",
        icon: navIcon(ArrowLeftRight),
      },
      { to: "/finance/budgets", label: "Budgets", icon: navIcon(PiggyBank) },
      {
        to: "/reports",
        label: "Report Builder",
        icon: navIcon(FileBarChart),
      },
    ],
  },
  {
    title: "Helpdesk",
    links: [
      {
        to: "/records/helpdesk.ticket",
        label: "Tickets",
        icon: navIcon(Headphones),
      },
      { to: "/helpdesk", label: "SLA + Triage", icon: navIcon(LifeBuoy) },
    ],
  },
  {
    title: "Sales",
    links: [
      {
        to: "/sales/orders",
        label: "Sales Orders",
        icon: navIcon(ShoppingCart),
      },
      { to: "/sales/returns", label: "Returns", icon: navIcon(Undo2) },
      {
        to: "/sales/price-lists",
        label: "Price Lists",
        icon: navIcon(Tags),
      },
      {
        to: "/procurement/purchase-orders",
        label: "Purchase Orders",
        icon: navIcon(ClipboardList),
      },
      {
        to: "/procurement/requisitions",
        label: "Requisitions",
        icon: navIcon(ClipboardCheck),
      },
    ],
  },
  {
    title: "POS",
    links: [
      { to: "/pos", label: "Register", icon: navIcon(ScanLine) },
      {
        to: "/records/sales.pos_profile",
        label: "Profiles",
        icon: navIcon(UserSquare),
      },
      {
        to: "/records/sales.pos_invoice",
        label: "Receipts",
        icon: navIcon(ReceiptText),
      },
    ],
  },
  {
    title: "Inventory",
    links: [
      { to: "/records/inventory.item", label: "Items", icon: navIcon(Package) },
      {
        to: "/records/inventory.warehouse",
        label: "Warehouses",
        icon: navIcon(Warehouse),
      },
      {
        to: "/inventory/stock-levels",
        label: "Stock Levels",
        icon: navIcon(Boxes),
      },
      {
        to: "/inventory/reports/valuation",
        label: "Valuation",
        icon: navIcon(PackageSearch),
      },
      // Landed costs writes inventory_moves AND posts a ledger
      // JE so the backend route is gated on FeatureInventory
      // (section gate) AND FeatureFinance (per-link gate).
      {
        to: "/inventory/landed-costs",
        label: "Landed Costs",
        icon: navIcon(Truck),
        requires: ["finance"],
      },
      {
        to: "/inventory/cycle-counts",
        label: "Cycle Counts",
        icon: navIcon(ClipboardList),
      },
    ],
  },
  {
    title: "Manufacturing",
    links: [
      {
        to: "/manufacturing/boms",
        label: "Bills of Materials",
        icon: navIcon(Layers),
      },
      {
        to: "/manufacturing/work-orders",
        label: "Work Orders",
        icon: navIcon(Factory),
      },
      {
        to: "/manufacturing/routings",
        label: "Routings & Work Centers",
        icon: navIcon(RouteIcon),
      },
      {
        to: "/manufacturing/capacity",
        label: "Capacity Planning",
        icon: navIcon(Gauge),
      },
      {
        to: "/manufacturing/job-cards",
        label: "Job Cards",
        icon: navIcon(ClipboardList),
      },
      {
        to: "/manufacturing/mrp",
        label: "MRP",
        icon: navIcon(Gauge),
      },
      {
        to: "/manufacturing/subcontracting",
        label: "Subcontracting",
        icon: navIcon(Factory),
      },
    ],
  },
  {
    title: "HR",
    links: [
      {
        to: "/records/hr.employee",
        label: "Employees",
        icon: navIcon(UserCog),
      },
      { to: "/hr/org-chart", label: "Org Chart", icon: navIcon(Network) },
      {
        to: "/records/hr.leave_request",
        label: "Leave Requests",
        icon: navIcon(Calendar),
      },
      {
        to: "/records/hr.attendance",
        label: "Attendance",
        icon: navIcon(Clock),
      },
      {
        to: "/records/hr.expense_claim",
        label: "Expense Claims",
        icon: navIcon(Receipt),
      },
      { to: "/hr/payroll", label: "Payroll", icon: navIcon(Banknote) },
      {
        to: "/hr/shifts",
        label: "Shift Schedule",
        icon: navIcon(CalendarClock),
      },
      {
        to: "/hr/recruitment",
        label: "Recruitment",
        icon: navIcon(UserPlus),
        // Gated on FeatureRecruitment on top of the HR section gate,
        // matching the backend's /api/v1/hr/recruitment/* routes.
        requires: ["recruitment"],
      },
    ],
  },
  {
    title: "LMS",
    links: [
      {
        to: "/records/lms.course",
        label: "Courses",
        icon: navIcon(GraduationCap),
      },
      { to: "/records/lms.module", label: "Modules", icon: navIcon(BookOpen) },
      {
        to: "/records/lms.lesson",
        label: "Lessons",
        icon: navIcon(BookText),
      },
      {
        to: "/records/lms.enrollment",
        label: "Enrollments",
        icon: navIcon(UserPlus),
      },
      {
        to: "/records/lms.quiz",
        label: "Quizzes",
        icon: navIcon(HelpCircle),
      },
      {
        to: "/records/lms.assignment",
        label: "Assignments",
        icon: navIcon(ClipboardList),
      },
      {
        to: "/lms/progress",
        label: "Learner Progress",
        icon: navIcon(TrendingUp),
      },
      {
        to: "/lms/learning-paths",
        label: "Learning Paths",
        icon: navIcon(Milestone),
      },
      {
        to: "/lms/instructor",
        label: "Instructor Dashboard",
        icon: navIcon(BarChart3),
      },
      {
        to: "/lms/discussions",
        label: "Discussions",
        icon: navIcon(MessageSquare),
      },
      {
        to: "/lms/badges",
        label: "Badges",
        icon: navIcon(Award),
      },
    ],
  },
  {
    title: "Insights",
    links: [
      {
        to: "/insights/queries",
        label: "Query Builder",
        icon: navIcon(LayoutGrid),
      },
      {
        to: "/insights/dashboards",
        label: "Dashboards",
        icon: navIcon(PieChart),
      },
    ],
  },
  {
    title: "Marketplace",
    links: [
      { to: "/marketplace", label: "Browse", icon: navIcon(Store) },
      {
        to: "/marketplace/installed",
        label: "Installed",
        icon: navIcon(Puzzle),
      },
    ],
  },
  {
    title: "Admin",
    links: [
      { to: "/admin/tenants", label: "Tenants", icon: navIcon(Building2) },
      { to: "/admin/features", label: "Features", icon: navIcon(ToggleLeft) },
      {
        to: "/admin/placement",
        label: "Placement Policy",
        icon: navIcon(MapPin),
      },
      { to: "/admin/retention", label: "Retention", icon: navIcon(Archive) },
      { to: "/admin/usage", label: "Usage", icon: navIcon(Activity) },
      {
        to: "/admin/health",
        label: "System Health",
        icon: navIcon(HeartPulse),
      },
      { to: "/admin/audit", label: "Audit Log", icon: navIcon(ScrollText) },
      { to: "/admin/roles", label: "Roles", icon: navIcon(Shield) },
      { to: "/admin/webhooks", label: "Webhooks", icon: navIcon(Webhook) },
      {
        to: "/admin/consolidation",
        label: "Consolidation",
        icon: navIcon(Combine),
      },
      {
        to: "/admin/ktypes/builder",
        label: "KType Builder",
        icon: navIcon(Database),
      },
      { to: "/imports", label: "Imports", icon: navIcon(Upload) },
    ],
  },
];

/**
 * Flattened view of every nav link with its owning section.  Used
 * by the breadcrumb builder, the recent-pages tracker, and the
 * command palette so all three stay in lock-step with `navSections`
 * (single source of truth for nav destinations and their labels).
 */
interface FlatNav {
  to: string;
  label: string;
  section: string;
  icon?: ReactNode;
}

const flatNav: FlatNav[] = navSections.flatMap((s) =>
  s.links.map((l) => ({
    to: l.to,
    label: l.label,
    section: s.title,
    icon: l.icon,
  })),
);

/**
 * Longest-prefix nav match for a pathname.  `/records/crm.lead/new`
 * resolves to the "Leads" link (not "Dashboard") because the most
 * specific `to` wins.
 */
function bestNavMatch(pathname: string): FlatNav | undefined {
  if (pathname === "/") return flatNav.find((n) => n.to === "/");
  let match: FlatNav | undefined;
  for (const n of flatNav) {
    if (n.to === "/") continue;
    if (pathname === n.to || pathname.startsWith(`${n.to}/`)) {
      if (!match || n.to.length > match.to.length) match = n;
    }
  }
  return match;
}

/** Turn a raw path segment ("crm.lead", "bank-reconciliation") into
 * a human label ("Lead", "Bank Reconciliation"). */
function humanizeSegment(seg: string): string {
  const decoded = decodeURIComponent(seg);
  const base = decoded.includes(".")
    ? (decoded.split(".").pop() ?? decoded)
    : decoded;
  return base.replace(/[-_]/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

interface Crumb {
  label: string;
  to?: string;
}

/**
 * Build the breadcrumb trail for a pathname.  Anchors on the
 * matched nav link (Home → Section → Page) and appends any trailing
 * segments (e.g. `/new`, a record id) as non-link crumbs.  Falls
 * back to humanized path segments for routes with no nav entry.
 */
function buildBreadcrumbs(pathname: string): Crumb[] {
  if (pathname === "/") return [{ label: "Dashboard" }];
  const match = bestNavMatch(pathname);
  const crumbs: Crumb[] = [{ label: "Home", to: "/" }];
  if (match) {
    crumbs.push({ label: match.section });
    crumbs.push({ label: match.label, to: match.to });
    pathname
      .slice(match.to.length)
      .split("/")
      .filter(Boolean)
      .forEach((seg) => crumbs.push({ label: humanizeSegment(seg) }));
  } else {
    pathname
      .split("/")
      .filter(Boolean)
      .forEach((seg) => crumbs.push({ label: humanizeSegment(seg) }));
  }
  return crumbs;
}

// A handful of words don't follow the regular -s/-ies plural rules, so
// they get an explicit singular form.
const SINGULAR_OVERRIDES: Record<string, string> = {
  Quizzes: "Quiz",
};

/** Singularize a single word using the override map + suffix rules. */
function singularizeWord(word: string): string {
  const override = SINGULAR_OVERRIDES[word];
  if (override) return override;
  if (word.endsWith("ies")) return `${word.slice(0, -3)}y`;
  // Only words whose singular truly ends in "s" double up to "sses"
  // (e.g. "addresses" -> "address"). A bare "-ses" like "Warehouses"
  // or "Courses" is a normal plural and just drops the trailing "s".
  if (word.endsWith("sses")) return word.slice(0, -2);
  if (word.endsWith("s")) return word.slice(0, -1);
  return word;
}

/**
 * Light singularization for "Create new {ktype}" command labels. Handles
 * phrase labels, not just single nouns:
 *   - "X of Y"  — the head noun precedes "of", so singularize that and
 *     keep the qualifier ("Bills of Materials" -> "Bill of Materials").
 *   - "A & B"   — two coordinated nouns, singularize each side's final
 *     word ("Routings & Work Centers" -> "Routing & Work Center").
 *   - otherwise singularize the final word ("Credit Notes" -> "Credit
 *     Note", "Leads" -> "Lead").
 */
export function singularizeLabel(label: string): string {
  const override = SINGULAR_OVERRIDES[label];
  if (override) return override;

  const ofIndex = label.indexOf(" of ");
  if (ofIndex !== -1) {
    return singularizeWord(label.slice(0, ofIndex)) + label.slice(ofIndex);
  }

  if (label.includes(" & ")) {
    return label.split(" & ").map(singularizeFinalWord).join(" & ");
  }

  return singularizeFinalWord(label);
}

/** Singularize only the last whitespace-delimited word of a phrase. */
function singularizeFinalWord(phrase: string): string {
  const words = phrase.split(" ");
  const last = words.length - 1;
  words[last] = singularizeWord(words[last]);
  return words.join(" ");
}

// Platform-aware label for the command-palette shortcut. The keydown
// handler binds both metaKey and ctrlKey, so the hint should match the
// user's OS: ⌘K on Mac, Ctrl K elsewhere.
//
// Prefer the modern `navigator.userAgentData.platform` (a high-entropy
// hint) and fall back to the deprecated `navigator.platform` then the
// UA string, since `platform` is empty in some newer browsers.
function detectMacPlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  const uaData = (
    navigator as Navigator & { userAgentData?: { platform?: string } }
  ).userAgentData;
  const source =
    uaData?.platform || navigator.platform || navigator.userAgent || "";
  return /mac|iphone|ipad|ipod/i.test(source);
}

const isMacPlatform = detectMacPlatform();
const commandShortcutLabel = isMacPlatform ? "⌘K" : "Ctrl K";

const RECENT_PAGES_KEY = "kapp:recent-pages";

interface RecentPage {
  to: string;
  label: string;
}

function readRecentPages(): RecentPage[] {
  try {
    const raw = localStorage.getItem(RECENT_PAGES_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (p): p is RecentPage =>
        typeof p === "object" &&
        p !== null &&
        typeof (p as RecentPage).to === "string" &&
        typeof (p as RecentPage).label === "string",
    );
  } catch {
    return [];
  }
}

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
        <Spinner size="sm" label="Loading page" />
        <span className="text-sm" aria-hidden="true">
          Loading…
        </span>
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
    <div className="flex h-screen w-screen items-center justify-center bg-bg text-fg-muted">
      <Spinner size="lg" />
    </div>
  );
}

export function App() {
  return (
    <LocaleProvider>
      <TooltipProvider delayDuration={300}>
        {/* App-wide toast overlay.  A single Toaster at the root is
            the sink every `toast.*()` call renders into (see
            @kapp/ui Toast). */}
        <Toaster />
        <Suspense fallback={<PublicRouteFallback />}>
        <Routes>
          {/* Public form route lives outside the app shell so anonymous
              visitors don't see tenant navigation. */}
          <Route path="/forms/:formId" element={<FormPage />} />
          <Route path="/login" element={<LoginPage />} />
          {/* iam-core (OAuth2/OIDC) login completion. The backend
              redirects here with tokens in the URL fragment after a
              successful Authorization-Code exchange. */}
          <Route path="/callback" element={<CallbackPage />} />
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
          {/* Public platform status page (Workstream 6). Rendered
              outside the app shell so anonymous visitors can check
              availability without a tenant context or login. */}
          <Route path="/status" element={<StatusPage />} />
          <Route path="/*" element={<AppShell />} />
        </Routes>
        </Suspense>
      </TooltipProvider>
    </LocaleProvider>
  );
}

// Recent global searches are persisted in localStorage so the
// dropdown can offer one-tap re-runs across reloads and tabs. Capped
// at MAX_RECENT_SEARCHES newest-first; the store survives a malformed
// payload by falling back to an empty list rather than throwing.
const RECENT_SEARCHES_KEY = "kapp.recent_searches";
const MAX_RECENT_SEARCHES = 5;

function loadRecentSearches(): string[] {
  try {
    const raw = localStorage.getItem(RECENT_SEARCHES_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as unknown;
    if (!Array.isArray(parsed)) return [];
    return parsed
      .filter((x): x is string => typeof x === "string")
      .slice(0, MAX_RECENT_SEARCHES);
  } catch {
    return [];
  }
}

function pushRecentSearch(query: string): string[] {
  const trimmed = query.trim();
  if (!trimmed) return loadRecentSearches();
  const next = [trimmed, ...loadRecentSearches().filter((x) => x !== trimmed)].slice(
    0,
    MAX_RECENT_SEARCHES,
  );
  try {
    localStorage.setItem(RECENT_SEARCHES_KEY, JSON.stringify(next));
  } catch {
    // Storage may be disabled / over quota; the in-memory list still
    // updates so the current session keeps working.
  }
  return next;
}

// quickResultLabel mirrors SearchPage.summaryOf: it picks the most
// human-meaningful top-level field so the dropdown row reads as a
// record name rather than a raw uuid, falling back to the id.
function quickResultLabel(r: SearchResult): string {
  const d = (r.data ?? {}) as Record<string, unknown>;
  for (const k of ["name", "title", "subject", "sku", "code", "email", "reference"]) {
    const v = d[k];
    if (typeof v === "string" && v.trim().length > 0) return v;
  }
  return r.id;
}

type SearchOption =
  | { kind: "recent"; query: string }
  | { kind: "result"; result: SearchResult };

/**
 * GlobalSearchBox — the shell-level search input. It expands on focus
 * and drops a panel that shows recent searches (empty query) or live
 * quick results (debounced /api/v1/search, capped at 6). Selecting a
 * result jumps straight to the record; submitting routes to
 * /search?q=... for the full results page. Keyboard: ↑/↓ move the
 * active row, Enter opens it (or runs the typed query), Esc closes.
 */
function GlobalSearchBox() {
  const nav = useNavigate();
  const [value, setValue] = useState("");
  const [debounced, setDebounced] = useState("");
  const [open, setOpen] = useState(false);
  const [recent, setRecent] = useState<string[]>(() => loadRecentSearches());
  const [activeIndex, setActiveIndex] = useState(-1);
  const containerRef = useRef<HTMLDivElement>(null);

  // Debounce edits into a trailing 200ms window so quick-result
  // fetches don't fire on every keystroke.
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value.trim()), 200);
    return () => window.clearTimeout(id);
  }, [value]);

  // Reset the keyboard cursor when the option set changes shape (the
  // recent ⇄ results swap, or a new result list). Deliberately keyed
  // off `debounced` alone: keying off `open` too would let this fire
  // on the same commit as the ArrowDown handler (which opens the panel
  // and advances the cursor in one event), clobbering its selection so
  // the first ArrowDown after a close failed to highlight anything.
  useEffect(() => {
    setActiveIndex(-1);
  }, [debounced]);

  // Reset the cursor when the panel closes so a later reopen starts
  // fresh rather than restoring a stale highlight. Guarded on `!open`
  // so reopening (false → true) leaves the cursor untouched, keeping
  // the ArrowDown-from-closed case working.
  useEffect(() => {
    if (!open) setActiveIndex(-1);
  }, [open]);

  // Close on outside pointer-down so the panel doesn't linger when
  // the user clicks elsewhere in the shell.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (
        containerRef.current &&
        !containerRef.current.contains(e.target as Node)
      ) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const resultsQuery = useQuery({
    queryKey: ["global-search", debounced],
    queryFn: () => api.searchRecords({ q: debounced, limit: 6 }),
    enabled: open && debounced.length > 0,
  });

  const showRecent = debounced.length === 0;
  const quickResults = resultsQuery.data?.results ?? [];
  const options: SearchOption[] = showRecent
    ? recent.map((query) => ({ kind: "recent", query }))
    : quickResults.map((result) => ({ kind: "result", result }));

  const submitSearch = (query: string) => {
    const trimmed = query.trim();
    if (!trimmed) return;
    setRecent(pushRecentSearch(trimmed));
    setOpen(false);
    setValue("");
    nav(`/search?q=${encodeURIComponent(trimmed)}`);
  };

  const openRecord = (result: SearchResult) => {
    setOpen(false);
    setValue("");
    nav(`/records/${result.ktype}/${result.id}`);
  };

  const selectOption = (opt: SearchOption) => {
    if (opt.kind === "recent") submitSearch(opt.query);
    else openRecord(opt.result);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setOpen(true);
      setActiveIndex((i) => Math.min(i + 1, options.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIndex((i) => Math.max(i - 1, -1));
    } else if (e.key === "Escape") {
      setOpen(false);
    } else if (e.key === "Enter") {
      if (activeIndex >= 0 && options[activeIndex]) {
        e.preventDefault();
        selectOption(options[activeIndex]);
      }
      // Otherwise let the form submit handler run the typed query.
    }
  };

  const showPanel =
    open && (options.length > 0 || (!showRecent && debounced.length > 0));

  return (
    <div
      ref={containerRef}
      className={`relative flex-1 transition-all duration-200 ${
        open ? "max-w-xl" : "max-w-md"
      }`}
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submitSearch(value);
        }}
      >
        <Input
          type="search"
          name="q"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onFocus={() => setOpen(true)}
          onKeyDown={onKeyDown}
          autoComplete="off"
          placeholder="Search records…"
          aria-label="Global search"
          aria-expanded={showPanel}
          role="combobox"
          aria-controls={showPanel ? "global-search-listbox" : undefined}
          leadingAddon={<Search className="h-4 w-4" aria-hidden />}
        />
      </form>

      {showPanel && (
        <div
          id="global-search-listbox"
          role="listbox"
          className="absolute left-0 right-0 top-full z-50 mt-1 overflow-hidden rounded-md border border-border bg-bg-elevated shadow-lg animate-in fade-in-0 slide-in-from-top-2 duration-150"
        >
          {showRecent ? (
            <div className="py-1">
              <div className="px-3 py-1 text-xs font-medium uppercase tracking-wide text-fg-subtle">
                Recent searches
              </div>
              {options.map((opt, i) =>
                opt.kind === "recent" ? (
                  <button
                    key={opt.query}
                    type="button"
                    role="option"
                    aria-selected={i === activeIndex}
                    onMouseEnter={() => setActiveIndex(i)}
                    onClick={() => selectOption(opt)}
                    className={`flex w-full items-center gap-2 px-3 py-2 text-left text-sm ${
                      i === activeIndex ? "bg-bg-muted" : "hover:bg-bg-subtle"
                    }`}
                  >
                    <Clock className="h-4 w-4 shrink-0 text-fg-subtle" aria-hidden />
                    <span className="truncate">{opt.query}</span>
                  </button>
                ) : null,
              )}
            </div>
          ) : (
            <div className="py-1">
              {resultsQuery.isLoading && (
                <div className="px-3 py-2 text-sm text-fg-muted">Searching…</div>
              )}
              {!resultsQuery.isLoading && options.length === 0 && (
                <div className="px-3 py-2 text-sm text-fg-muted">
                  No quick results.
                </div>
              )}
              {options.map((opt, i) =>
                opt.kind === "result" ? (
                  <button
                    key={`${opt.result.ktype}:${opt.result.id}`}
                    type="button"
                    role="option"
                    aria-selected={i === activeIndex}
                    onMouseEnter={() => setActiveIndex(i)}
                    onClick={() => selectOption(opt)}
                    className={`flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-sm ${
                      i === activeIndex ? "bg-bg-muted" : "hover:bg-bg-subtle"
                    }`}
                  >
                    <span className="truncate">{quickResultLabel(opt.result)}</span>
                    <Badge variant="outline" className="shrink-0">
                      {opt.result.ktype}
                    </Badge>
                  </button>
                ) : null,
              )}
              <button
                type="button"
                onClick={() => submitSearch(value)}
                className="flex w-full items-center gap-2 border-t border-border px-3 py-2 text-left text-sm text-accent hover:bg-bg-subtle"
              >
                <Search className="h-4 w-4 shrink-0" aria-hidden />
                Search for “{value.trim()}”
              </button>
            </div>
          )}
        </div>
      )}
    </div>
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
function AppNavLink({
  to,
  label,
  icon,
}: {
  to: string;
  label: string;
  icon?: ReactNode;
}) {
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
      icon={icon}
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
  const navigate = useNavigate();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [recent, setRecent] = useState<RecentPage[]>(() => readRecentPages());

  // Cmd/Ctrl+K toggles the command palette from anywhere in the
  // shell.  Bound once on mount.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((open) => !open);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Record each visited nav destination (most-recent-first, capped
  // at 10) so the palette can surface a "Recent" group.  The updater
  // stays pure (no side effects) — React Strict Mode double-invokes
  // updaters in dev — and bails out when the route is already at the
  // top so an idempotent revisit doesn't churn state.
  useEffect(() => {
    const match = bestNavMatch(location.pathname);
    if (!match || match.to === "/") return;
    setRecent((prev) => {
      if (prev[0]?.to === match.to) return prev;
      return [
        { to: match.to, label: match.label },
        ...prev.filter((p) => p.to !== match.to),
      ].slice(0, 10);
    });
  }, [location.pathname]);

  // Persist recents whenever they change.  Kept separate from the
  // state updater above so the write is a render-commit side effect
  // rather than smuggled into a (should-be-pure) updater function.
  useEffect(() => {
    try {
      localStorage.setItem(RECENT_PAGES_KEY, JSON.stringify(recent));
    } catch {
      // Ignore quota / disabled-storage errors — recents are a
      // best-effort convenience, never load-bearing.
    }
  }, [recent]);

  const featuresQuery = useQuery({
    queryKey: ["tenant-features", tenantKey()],
    queryFn: () => api.listTenantFeatures(tenantKey()),
    retry: false,
    staleTime: 60_000,
  });
  // Fail-open: when the features API is unreachable we still show
  // every nav item rather than hiding the entire app on a transient
  // network blip. The backend will 403 disabled sections if the user
  // actually navigates to them, so this only governs visibility, not
  // authorization.
  // Memoized on the features payload so its reference is stable
  // between renders; downstream memos (e.g. `commandGroups`) that
  // depend on it then actually memoize instead of recomputing every
  // render off a fresh array literal.
  const visible = useMemo(() => {
    const data = featuresQuery.data;
    const map = data?.features ?? {};
    // Fail-open while features haven't loaded: show everything and
    // let the backend 403 on actual navigation.
    const linkEnabled = (link: NavLink) =>
      !data || !link.requires || link.requires.length === 0
        ? true
        : link.requires.every((k) => map[k] !== false);
    return navSections
      .filter((s) => {
        const key = featureFromSection[s.title];
        if (!key || !data) return true;
        return map[key] !== false;
      })
      .map((s) => ({
        ...s,
        // Per-link filter so a section can stay visible while a
        // single link inside it is gated on additional features
        // (e.g. Landed Costs requires `finance` on top of the
        // section's `inventory` gate).
        links: s.links.filter(linkEnabled),
      }))
      // Drop sections whose every link was filtered out so the
      // sidebar doesn't render an empty group header.
      .filter((s) => s.links.length > 0);
  }, [featuresQuery.data]);

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

  const crumbs = buildBreadcrumbs(location.pathname);

  // Command-palette groups, rebuilt when the visible nav or the
  // recent list changes.  Mirrors the sidebar (navigation), offers
  // a "Create new X" for every record KType, a few fixed shortcuts,
  // and the recent-pages list.
  const commandGroups: CommandGroup[] = useMemo(() => {
    const navItems = visible.flatMap((s) =>
      s.links.map((l) => ({
        id: `nav:${l.to}`,
        label: l.label,
        hint: s.title,
        icon: l.icon,
        keywords: [s.title],
        onSelect: () => navigate(l.to),
      })),
    );
    const createItems = visible.flatMap((s) =>
      s.links
        .filter((l) => l.to.startsWith("/records/"))
        .map((l) => ({
          id: `create:${l.to}`,
          label: `Create new ${singularizeLabel(l.label)}`,
          hint: s.title,
          icon: <Plus className="h-4 w-4" />,
          keywords: ["new", "create", "add", l.label],
          onSelect: () => navigate(`${l.to}/new`),
        })),
    );
    const quickItems = [
      {
        id: "quick:search",
        label: "Search records…",
        icon: <Search className="h-4 w-4" />,
        keywords: ["find", "lookup"],
        onSelect: () => navigate("/search"),
      },
      {
        id: "quick:admin",
        label: "Go to admin",
        icon: <Settings className="h-4 w-4" />,
        keywords: ["settings", "administration"],
        onSelect: () => navigate("/admin/tenants"),
      },
      {
        id: "quick:health",
        label: "Go to system health",
        icon: <HeartPulse className="h-4 w-4" />,
        keywords: ["status", "monitoring"],
        onSelect: () => navigate("/admin/health"),
      },
    ];
    const recentItems = recent
      .filter((r) => r.to !== location.pathname)
      .map((r) => ({
        id: `recent:${r.to}`,
        label: r.label,
        hint: "Recent",
        onSelect: () => navigate(r.to),
      }));

    const groups: CommandGroup[] = [];
    if (recentItems.length > 0)
      groups.push({ heading: "Recent", items: recentItems });
    groups.push({ heading: "Navigation", items: navItems });
    if (createItems.length > 0)
      groups.push({ heading: "Create", items: createItems });
    groups.push({ heading: "Quick actions", items: quickItems });
    return groups;
  }, [visible, recent, navigate, location.pathname]);

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
                <AppNavLink
                  key={link.to}
                  to={link.to}
                  label={link.label}
                  icon={link.icon}
                />
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
          <Breadcrumb className="hidden min-w-0 lg:block">
            <BreadcrumbList className="flex-nowrap">
              {crumbs.map((crumb, i) => {
                const last = i === crumbs.length - 1;
                return (
                  <Fragment key={`${crumb.label}-${i}`}>
                    <BreadcrumbItem className="min-w-0">
                      {last ? (
                        <BreadcrumbPage className="truncate">
                          {crumb.label}
                        </BreadcrumbPage>
                      ) : crumb.to ? (
                        <BreadcrumbLink asChild>
                          <Link to={crumb.to}>{crumb.label}</Link>
                        </BreadcrumbLink>
                      ) : (
                        <span className="text-fg-subtle">{crumb.label}</span>
                      )}
                    </BreadcrumbItem>
                    {!last && <BreadcrumbSeparator />}
                  </Fragment>
                );
              })}
            </BreadcrumbList>
          </Breadcrumb>
          <GlobalSearchBox />
          <div className="ms-auto flex items-center gap-2">
            <button
              type="button"
              onClick={() => setPaletteOpen(true)}
              aria-label="Open command palette"
              className="hidden items-center gap-1.5 rounded-md border border-border bg-bg px-2 py-1.5 text-xs text-fg-subtle transition-colors hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) md:inline-flex"
            >
              <Search className="h-3.5 w-3.5" />
              <span className="font-medium">{commandShortcutLabel}</span>
            </button>
            {activeLabel && (
              <Badge variant="outline" className="hidden md:inline-flex">
                {activeLabel}
              </Badge>
            )}
            <LocaleSwitcher className="hidden md:inline-flex w-auto" />
            <NotificationBell />
          </div>
        </header>
        <CommandPalette
          open={paletteOpen}
          onOpenChange={setPaletteOpen}
          groups={commandGroups}
        />
        <div className="flex-1 p-6 overflow-auto">
          {/* Suspense lives OUTSIDE the pathname-keyed wrapper so the
              boundary stays mounted across navigations: switching to an
              already-loaded route no longer flashes the fallback, and an
              in-flight lazy chunk isn't discarded and refetched. The
              keyed wrapper sits inside, so it still re-mounts per route
              and replays the fade-in once the route's content commits. */}
          <Suspense fallback={<ShellRouteFallback />}>
            <div
              key={location.pathname}
              className="animate-in fade-in duration-200"
            >
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
              <Route path="/admin/health" element={<AdminHealthPage />} />
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
              <Route
                path="/finance/budgets"
                element={<BudgetPage />}
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
              <Route path="/marketplace" element={<MarketplaceBrowsePage />} />
              <Route
                path="/marketplace/extensions/:extId"
                element={<MarketplaceExtensionDetailPage />}
              />
              <Route
                path="/marketplace/installed"
                element={<MarketplaceInstallationsPage />}
              />
              <Route
                path="/marketplace/installed/:installId"
                element={<InstallationDetailPage />}
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
              <Route path="/manufacturing/boms" element={<BOMPage />} />
              <Route
                path="/manufacturing/work-orders"
                element={<WorkOrdersPage />}
              />
              <Route
                path="/manufacturing/routings"
                element={<RoutingPage />}
              />
              <Route
                path="/manufacturing/capacity"
                element={<CapacityPlanningPage />}
              />
              <Route
                path="/manufacturing/job-cards"
                element={<JobCardPage />}
              />
              <Route path="/manufacturing/mrp" element={<MrpPage />} />
              <Route
                path="/manufacturing/subcontracting"
                element={<SubcontractingPage />}
              />
              <Route
                path="/inventory/landed-costs"
                element={<LandedCostPage />}
              />
              <Route
                path="/inventory/cycle-counts"
                element={<CycleCountPage />}
              />
              <Route path="/hr/org-chart" element={<OrgChartPage />} />
              <Route path="/hr/payroll" element={<PayrollPage />} />
              <Route path="/hr/shifts" element={<ShiftCalendarPage />} />
              <Route
                path="/hr/recruitment"
                element={<RecruitmentDashboardPage />}
              />
              <Route
                path="/hr/recruitment/job-openings"
                element={<JobOpeningsPage />}
              />
              <Route
                path="/hr/recruitment/applications"
                element={<ApplicationsPage />}
              />
              <Route
                path="/hr/recruitment/interviews"
                element={<InterviewSchedulePage />}
              />
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
              <Route path="/finance/bank-feeds" element={<BankFeedsPage />} />
              <Route path="/sales/orders" element={<SalesOrdersPage />} />
              <Route path="/sales/returns" element={<SalesReturnsPage />} />
              <Route
                path="/sales/price-lists"
                element={<PriceListsPage />}
              />
              <Route
                path="/procurement/purchase-orders"
                element={<PurchaseOrdersPage />}
              />
              <Route
                path="/procurement/requisitions"
                element={<PurchaseRequisitionsPage />}
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
              <Route
                path="/lms/learning-paths"
                element={<LearningPathsPage />}
              />
              <Route
                path="/lms/instructor"
                element={<InstructorDashboardPage />}
              />
              <Route path="/lms/discussions" element={<DiscussionsPage />} />
              <Route path="/lms/badges" element={<BadgesPage />} />
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
            </div>
          </Suspense>
        </div>
      </main>
    </div>
  );
}
