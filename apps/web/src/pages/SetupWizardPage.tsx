import { useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";

// SetupWizardPage drives the tenant setup wizard on the frontend. It
// collects the first-run company profile, CoA template, and initial
// user roster and posts the aggregated payload to
// POST /api/v1/tenants/{id}/setup. The backend seed logic lives in
// internal/tenant/wizard.go — the shape of `SetupPayload` mirrors
// `tenant.SetupWizardConfig`.

// CoA template options match the files in
// internal/tenant/coa_templates/. Adding a new template is a matter of
// dropping a JSON file in that folder, registering it in
// chartOfAccountsTemplates, and extending this list.
const COA_TEMPLATES = [
  { value: "us_gaap_basic", label: "US GAAP Basic" },
  { value: "ifrs_basic", label: "IFRS Basic" },
];

interface InitialUser {
  email: string;
  display_name: string;
  role: string;
}

interface SetupPayload {
  company_name: string;
  industry?: string;
  country?: string;
  coa_template: string;
  users: InitialUser[];
}

interface SetupResult {
  tenant_id: string;
  accounts_inserted: number;
  roles_inserted: number;
  users_inserted: number;
  coa_template_used: string;
}

export function SetupWizardPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const [step, setStep] = useState(0);
  const [companyName, setCompanyName] = useState("");
  const [industry, setIndustry] = useState("");
  const [country, setCountry] = useState("");
  const [coaTemplate, setCoaTemplate] = useState(COA_TEMPLATES[0].value);
  const [users, setUsers] = useState<InitialUser[]>([
    { email: "", display_name: "", role: "tenant.admin" },
  ]);

  const tenantId = id ?? "";

  const submit = useMutation<SetupResult, Error, SetupPayload>({
    mutationFn: async (payload) => {
      const res = await fetch(
        `/api/v1/tenants/${encodeURIComponent(tenantId)}/setup`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "X-Tenant-ID":
              localStorage.getItem("kapp.tenant") ?? tenantId,
            ...(localStorage.getItem("kapp.token")
              ? {
                  Authorization: `Bearer ${localStorage.getItem("kapp.token")}`,
                }
              : {}),
          },
          body: JSON.stringify(payload),
        },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || `Setup failed (${res.status})`);
      }
      return (await res.json()) as SetupResult;
    },
    onSuccess: () => {
      // After the wizard seeds the chart of accounts, roles, and
      // initial user memberships, drop the user at the tenant root so
      // they can start working. The success step below still renders
      // a summary before this runs.
      setStep(3);
    },
  });

  const canAdvanceCompany = companyName.trim().length > 0;
  const validUsers = useMemo(
    () =>
      users
        .map((u) => ({
          email: u.email.trim(),
          display_name: u.display_name.trim(),
          role: u.role.trim() || "tenant.admin",
        }))
        .filter((u) => u.email !== ""),
    [users],
  );

  const submitWizard = () => {
    submit.mutate({
      company_name: companyName.trim(),
      industry: industry.trim() || undefined,
      country: country.trim() || undefined,
      coa_template: coaTemplate,
      users: validUsers,
    });
  };

  if (!tenantId) {
    return (
      <section>
        <h1>Tenant Setup</h1>
        <p style={{ color: "#b91c1c" }}>
          Missing tenant id in route. Expected <code>/setup/:id</code>.
        </p>
      </section>
    );
  }

  return (
    <section style={{ maxWidth: 640 }}>
      <h1>Tenant Setup</h1>
      <p style={{ color: "#6b7280" }}>
        Seeds the chart of accounts, default roles, and invites your
        starting team. You can edit every value after setup from the
        admin pages.
      </p>
      <ol
        style={{
          display: "flex",
          gap: 16,
          listStyle: "none",
          padding: 0,
          margin: "16px 0",
          fontSize: 13,
        }}
      >
        {["Company", "Chart of Accounts", "Invite users", "Done"].map(
          (label, i) => (
            <li
              key={label}
              style={{
                color: i === step ? "#111827" : "#9ca3af",
                fontWeight: i === step ? 600 : 400,
              }}
            >
              {i + 1}. {label}
            </li>
          ),
        )}
      </ol>

      {step === 0 && (
        <div style={{ display: "grid", gap: 12 }}>
          <label style={{ display: "grid", gap: 4 }}>
            Company name
            <input
              value={companyName}
              onChange={(e) => setCompanyName(e.target.value)}
              required
            />
          </label>
          <label style={{ display: "grid", gap: 4 }}>
            Industry
            <input
              value={industry}
              onChange={(e) => setIndustry(e.target.value)}
              placeholder="e.g. Software, Retail"
            />
          </label>
          <label style={{ display: "grid", gap: 4 }}>
            Country
            <input
              value={country}
              onChange={(e) => setCountry(e.target.value)}
              placeholder="ISO country code or name"
            />
          </label>
          <div>
            <button
              type="button"
              disabled={!canAdvanceCompany}
              onClick={() => setStep(1)}
            >
              Next
            </button>
          </div>
        </div>
      )}

      {step === 1 && (
        <div style={{ display: "grid", gap: 12 }}>
          <fieldset style={{ border: "1px solid #e5e7eb", padding: 12 }}>
            <legend>Chart of Accounts template</legend>
            {COA_TEMPLATES.map((t) => (
              <label
                key={t.value}
                style={{ display: "block", padding: "4px 0" }}
              >
                <input
                  type="radio"
                  name="coa"
                  value={t.value}
                  checked={coaTemplate === t.value}
                  onChange={(e) => setCoaTemplate(e.target.value)}
                />{" "}
                {t.label}
              </label>
            ))}
          </fieldset>
          <p style={{ color: "#6b7280", fontSize: 12 }}>
            Templates live in <code>internal/tenant/coa_templates/</code>.
            Every account is inserted with{" "}
            <code>ON CONFLICT (tenant_id, code) DO NOTHING</code> so the
            step is safe to re-run.
          </p>
          <div style={{ display: "flex", gap: 8 }}>
            <button type="button" onClick={() => setStep(0)}>
              Back
            </button>
            <button type="button" onClick={() => setStep(2)}>
              Next
            </button>
          </div>
        </div>
      )}

      {step === 2 && (
        <div style={{ display: "grid", gap: 12 }}>
          <p style={{ fontSize: 13, color: "#6b7280" }}>
            Invite initial team members. Each user is seeded into the{" "}
            <code>users</code> table and added to the tenant via{" "}
            <code>user_tenants</code> with the selected role.
          </p>
          <table style={{ width: "100%", fontSize: 13 }}>
            <thead>
              <tr>
                <th style={{ textAlign: "left" }}>Email</th>
                <th style={{ textAlign: "left" }}>Display name</th>
                <th style={{ textAlign: "left" }}>Role</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.map((u, i) => (
                <tr key={i}>
                  <td>
                    <input
                      value={u.email}
                      onChange={(e) =>
                        setUsers((prev) =>
                          prev.map((row, j) =>
                            j === i ? { ...row, email: e.target.value } : row,
                          ),
                        )
                      }
                      type="email"
                      placeholder="name@example.com"
                    />
                  </td>
                  <td>
                    <input
                      value={u.display_name}
                      onChange={(e) =>
                        setUsers((prev) =>
                          prev.map((row, j) =>
                            j === i
                              ? { ...row, display_name: e.target.value }
                              : row,
                          ),
                        )
                      }
                    />
                  </td>
                  <td>
                    <select
                      value={u.role}
                      onChange={(e) =>
                        setUsers((prev) =>
                          prev.map((row, j) =>
                            j === i ? { ...row, role: e.target.value } : row,
                          ),
                        )
                      }
                    >
                      <option value="owner">owner</option>
                      <option value="tenant.admin">tenant.admin</option>
                      <option value="tenant.member">tenant.member</option>
                      <option value="finance.admin">finance.admin</option>
                      <option value="hr.admin">hr.admin</option>
                      <option value="lms.admin">lms.admin</option>
                    </select>
                  </td>
                  <td>
                    <button
                      type="button"
                      onClick={() =>
                        setUsers((prev) => prev.filter((_, j) => j !== i))
                      }
                      disabled={users.length <= 1}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div>
            <button
              type="button"
              onClick={() =>
                setUsers((prev) => [
                  ...prev,
                  { email: "", display_name: "", role: "tenant.member" },
                ])
              }
            >
              Add another user
            </button>
          </div>
          <div style={{ display: "flex", gap: 8 }}>
            <button type="button" onClick={() => setStep(1)}>
              Back
            </button>
            <button
              type="button"
              onClick={submitWizard}
              disabled={submit.isPending}
            >
              {submit.isPending ? "Running setup…" : "Finish setup"}
            </button>
          </div>
          {submit.isError && (
            <p style={{ color: "#b91c1c" }}>
              Setup failed: {submit.error.message}
            </p>
          )}
        </div>
      )}

      {step === 3 && submit.data && (
        <div style={{ display: "grid", gap: 12 }}>
          <h2>Setup complete</h2>
          <ul style={{ fontSize: 13 }}>
            <li>
              CoA template: <code>{submit.data.coa_template_used}</code>
            </li>
            <li>Accounts seeded: {submit.data.accounts_inserted}</li>
            <li>Roles seeded: {submit.data.roles_inserted}</li>
            <li>Users invited: {submit.data.users_inserted}</li>
          </ul>
          <div>
            <button type="button" onClick={() => navigate("/")}>
              Go to tenant home
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
