import { Route, Routes, Link } from "react-router-dom";
import { RecordListPage } from "./pages/RecordListPage";
import { RecordFormPage } from "./pages/RecordFormPage";
import { LoginPage } from "./pages/LoginPage";

export function App() {
  return (
    <div style={{ display: "flex", minHeight: "100vh" }}>
      <aside
        style={{ width: 220, borderRight: "1px solid #e5e7eb", padding: 16 }}
      >
        <h2>Kapp</h2>
        <nav>
          <ul style={{ listStyle: "none", padding: 0 }}>
            <li>
              <Link to="/records/crm.deal">Deals</Link>
            </li>
            <li>
              <Link to="/records/hr.employee">Employees</Link>
            </li>
          </ul>
        </nav>
      </aside>
      <main style={{ flex: 1, padding: 24 }}>
        <Routes>
          <Route path="/" element={<div>Select a KType from the nav.</div>} />
          <Route path="/login" element={<LoginPage />} />
          <Route path="/records/:ktype" element={<RecordListPage />} />
          <Route path="/records/:ktype/new" element={<RecordFormPage />} />
          <Route path="/records/:ktype/:id" element={<RecordFormPage />} />
        </Routes>
      </main>
    </div>
  );
}
