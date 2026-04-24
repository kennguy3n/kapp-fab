import { Route, Routes, Link } from "react-router-dom";
import { RecordListPage } from "./pages/RecordListPage";
import { RecordFormPage } from "./pages/RecordFormPage";
import { LoginPage } from "./pages/LoginPage";
import { TenantListPage } from "./pages/TenantListPage";
import { FormPage } from "./pages/FormPage";
import { ApprovalsPage } from "./pages/ApprovalsPage";
import { AuditLogPage } from "./pages/AuditLogPage";
import { SubledgerPage } from "./pages/SubledgerPage";
import { ChartOfAccountsPage } from "./pages/ChartOfAccountsPage";
import { JournalEntriesPage } from "./pages/JournalEntriesPage";
import { TrialBalancePage } from "./pages/TrialBalancePage";
import { IncomeStatementPage } from "./pages/IncomeStatementPage";
import { StockLevelsPage } from "./pages/StockLevelsPage";
import { InventoryValuationPage } from "./pages/InventoryValuationPage";
import { OrgChartPage } from "./pages/OrgChartPage";
import { LearnerProgressPage } from "./pages/LearnerProgressPage";
import { ImportPage } from "./pages/ImportPage";
import { ImportMappingPage } from "./pages/ImportMappingPage";
import { BankReconciliationPage } from "./pages/BankReconciliationPage";
import { CostCentersPage } from "./pages/CostCentersPage";
import { SalesOrdersPage } from "./pages/SalesOrdersPage";
import { PurchaseOrdersPage } from "./pages/PurchaseOrdersPage";
import { PriceListsPage } from "./pages/PriceListsPage";
import { PayrollPage } from "./pages/PayrollPage";
import { SetupWizardPage } from "./pages/SetupWizardPage";
import { DashboardPage } from "./pages/DashboardPage";
import { ExchangeRatesPage } from "./pages/ExchangeRatesPage";
import { HelpdeskPage } from "./pages/HelpdeskPage";
import { ReportBuilderPage } from "./pages/ReportBuilderPage";
import { NotificationBell } from "./components/NotificationBell";

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
    title: "Finance",
    links: [
      { to: "/records/finance.ar_invoice", label: "Invoices" },
      { to: "/records/finance.ap_bill", label: "Bills" },
      { to: "/records/finance.credit_note", label: "Credit Notes" },
      { to: "/records/finance.debit_note", label: "Debit Notes" },
      { to: "/records/finance.recurring_invoice", label: "Recurring Invoices" },
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
    title: "Admin",
    links: [
      { to: "/admin/tenants", label: "Tenants" },
      { to: "/admin/audit", label: "Audit Log" },
      { to: "/imports", label: "Imports" },
    ],
  },
];

export function App() {
  return (
    <Routes>
      {/* Public form route lives outside the app shell so anonymous
          visitors don't see tenant navigation. */}
      <Route path="/forms/:formId" element={<FormPage />} />
      <Route path="/login" element={<LoginPage />} />
      {/* Setup wizard is rendered outside the app shell because the
          tenant has no nav-worthy data until the wizard completes. */}
      <Route path="/setup/:id" element={<SetupWizardPage />} />
      <Route path="/*" element={<AppShell />} />
    </Routes>
  );
}

function AppShell() {
  return (
    <div style={{ display: "flex", minHeight: "100vh" }}>
      <aside
        style={{ width: 220, borderRight: "1px solid #e5e7eb", padding: 16 }}
      >
        <h2>Kapp</h2>
        <nav>
          {navSections.map((section) => (
            <div key={section.title} style={{ marginBottom: 12 }}>
              <div
                style={{
                  fontSize: 11,
                  textTransform: "uppercase",
                  color: "#6b7280",
                  marginBottom: 4,
                }}
              >
                {section.title}
              </div>
              <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
                {section.links.map((l) => (
                  <li key={l.to} style={{ padding: "2px 0" }}>
                    <Link to={l.to}>{l.label}</Link>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </nav>
      </aside>
      <main style={{ flex: 1, padding: 24 }}>
        <div
          style={{
            display: "flex",
            justifyContent: "flex-end",
            marginBottom: 12,
          }}
        >
          <NotificationBell />
        </div>
        <Routes>
          <Route path="/" element={<DashboardPage />} />
          <Route path="/admin/tenants" element={<TenantListPage />} />
          <Route path="/admin/audit" element={<AuditLogPage />} />
          <Route path="/approvals" element={<ApprovalsPage />} />
          <Route path="/finance/exchange-rates" element={<ExchangeRatesPage />} />
          <Route path="/helpdesk" element={<HelpdeskPage />} />
          <Route path="/reports" element={<ReportBuilderPage />} />
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
          <Route path="/finance/cost-centers" element={<CostCentersPage />} />
          <Route
            path="/finance/bank-reconciliation"
            element={<BankReconciliationPage />}
          />
          <Route path="/sales/orders" element={<SalesOrdersPage />} />
          <Route path="/sales/price-lists" element={<PriceListsPage />} />
          <Route
            path="/procurement/purchase-orders"
            element={<PurchaseOrdersPage />}
          />
          <Route path="/imports" element={<ImportPage />} />
          <Route path="/imports/new" element={<ImportPage />} />
          <Route path="/imports/:id" element={<ImportPage />} />
          <Route path="/imports/:id/mapping" element={<ImportMappingPage />} />
          <Route path="/lms/progress" element={<LearnerProgressPage />} />
          <Route
            path="/lms/progress/:enrollmentId"
            element={<LearnerProgressPage />}
          />
          <Route path="/records/:ktype" element={<RecordListPage />} />
          <Route path="/records/:ktype/new" element={<RecordFormPage />} />
          <Route path="/records/:ktype/:id" element={<RecordFormPage />} />
          {/* /kanban/:ktype is a deep-link alias that forces the kanban
              view via the defaultMode prop. RecordListPage still allows
              the user to toggle to the list view; defaultMode is only
              the initial mode, not a lock. */}
          <Route
            path="/kanban/:ktype"
            element={<RecordListPage defaultMode="kanban" />}
          />
        </Routes>
      </main>
    </div>
  );
}
