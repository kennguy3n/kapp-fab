import { Route, Routes, Link } from "react-router-dom";
import { RecordListPage } from "./pages/RecordListPage";
import { RecordFormPage } from "./pages/RecordFormPage";
import { LoginPage } from "./pages/LoginPage";
import { TenantListPage } from "./pages/TenantListPage";
import { FormPage } from "./pages/FormPage";
import { ApprovalsPage } from "./pages/ApprovalsPage";

interface NavSection {
  title: string;
  links: { to: string; label: string }[];
}

const navSections: NavSection[] = [
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
    title: "Admin",
    links: [{ to: "/admin/tenants", label: "Tenants" }],
  },
];

export function App() {
  return (
    <Routes>
      {/* Public form route lives outside the app shell so anonymous
          visitors don't see tenant navigation. */}
      <Route path="/forms/:formId" element={<FormPage />} />
      <Route path="/login" element={<LoginPage />} />
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
        <Routes>
          <Route path="/" element={<div>Select a KType from the nav.</div>} />
          <Route path="/admin/tenants" element={<TenantListPage />} />
          <Route path="/approvals" element={<ApprovalsPage />} />
          <Route path="/records/:ktype" element={<RecordListPage />} />
          <Route path="/records/:ktype/new" element={<RecordFormPage />} />
          <Route path="/records/:ktype/:id" element={<RecordFormPage />} />
          {/* /kanban/:ktype is a deep-link alias for the kanban view; the
              underlying RecordListPage already prefers kanban mode when
              the KType defines a kanban view, so the alias is just a
              stable URL for dashboards / KChat cards to point at. */}
          <Route path="/kanban/:ktype" element={<RecordListPage />} />
        </Routes>
      </main>
    </div>
  );
}
