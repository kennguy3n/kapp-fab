import { useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import {
  SupportedLocales,
  bestSupportedLocaleForCountry,
  localeInfo,
  useTranslation,
} from "../lib/i18n";

// SetupWizardPage drives the tenant setup wizard on the frontend. It
// collects the first-run company profile, CoA template, and initial
// user roster and posts the aggregated payload to
// POST /api/v1/tenants/{id}/setup. The backend seed logic lives in
// internal/tenant/wizard.go — the shape of `SetupPayload` mirrors
// `tenant.SetupWizardConfig`.

// CoA template options match the files in
// internal/tenant/coa_templates/. Adding a new template is a matter of
// dropping a JSON file in that folder, registering it in
// chartOfAccountsTemplates (wizard.go), and extending this list. The
// country-specific charts encode the local statutory liability
// accounts (e.g. CPF Payable for SG, GPSSA Payable for AE,
// AHV/ALV/BVG split for CH) so the payroll engine's deduction lines
// have a matching ledger destination on day one.
const COA_TEMPLATES = [
  { value: "us_gaap_basic", label: "US GAAP Basic" },
  { value: "ifrs_basic", label: "IFRS Basic (Generic)" },
  { value: "sg_basic", label: "Singapore — IFRS + CPF / GST" },
  { value: "my_basic", label: "Malaysia — IFRS + EPF / SOCSO / EIS / SST" },
  { value: "th_basic", label: "Thailand — TFRS + SSF / VAT" },
  { value: "id_basic", label: "Indonesia — PSAK + BPJS / PPN" },
  { value: "vn_basic", label: "Vietnam — Circular 200 + SI/HI/UI / VAT" },
  { value: "ph_basic", label: "Philippines — PFRS + SSS / PhilHealth / Pag-IBIG / VAT" },
  { value: "nz_basic", label: "New Zealand — NZ IFRS + PAYE / ACC / KiwiSaver / GST" },
  { value: "in_basic", label: "India — Ind AS + EPF / ESI / TDS / GST" },
  { value: "ch_basic", label: "Switzerland — Swiss GAAP + AHV / ALV / BVG / MwSt" },
  { value: "ae_basic", label: "UAE — IFRS + GPSSA / VAT / Gratuity" },
  { value: "sa_basic", label: "Saudi Arabia — IFRS + GOSI / Zakat / VAT" },
  { value: "qa_basic", label: "Qatar — IFRS + GRSIA / Gratuity" },
  { value: "kw_basic", label: "Kuwait — IFRS + PIFSS / NLST / Indemnity" },
  { value: "bh_basic", label: "Bahrain — IFRS + SIO / VAT / Indemnity" },
  { value: "om_basic", label: "Oman — IFRS + PASI / VAT / Gratuity" },
];

// defaultCoATemplateForCountry mirrors
// tenant.DefaultCoATemplateForCountry in internal/tenant/wizard.go so
// the wizard's CoA radio pre-selects the country-specific chart when
// the user picks a country in step 0. Keeping the table in lockstep
// with the backend means a SG tenant sees sg_basic checked rather
// than us_gaap_basic, and the payroll deduction lines have matching
// liability accounts on day one.
//
// Drift safety: the backend applies the same country -> template
// mapping when callers omit coa_template entirely (direct API / CLI
// consumers go through that branch). The frontend always sends an
// explicit value matching the user's on-screen selection, so a stale
// frontend with this table out of date would persist its own choice
// rather than triggering the backend re-resolve — keep this map in
// sync with internal/tenant/wizard.go on every PR that adds a tax
// pack.
const COUNTRY_COA_DEFAULTS: Record<string, string> = {
  US: "us_gaap_basic",
  SG: "sg_basic",
  MY: "my_basic",
  TH: "th_basic",
  ID: "id_basic",
  VN: "vn_basic",
  PH: "ph_basic",
  NZ: "nz_basic",
  IN: "in_basic",
  CH: "ch_basic",
  AE: "ae_basic",
  SA: "sa_basic",
  QA: "qa_basic",
  KW: "kw_basic",
  BH: "bh_basic",
  OM: "om_basic",
};

function defaultCoATemplateForCountry(country: string): string {
  const code = country.trim().toUpperCase();
  return COUNTRY_COA_DEFAULTS[code] ?? "ifrs_basic";
}

interface InitialUser {
  email: string;
  display_name: string;
  // role is kept for backwards-compatibility with the previous
  // single-role wizard payload — the backend mirrors it into
  // user_tenants.role for legacy code paths. The full multi-role
  // assignment now lives in `roles`.
  role: string;
  roles: string[];
}

// AVAILABLE_ROLES mirrors internal/tenant/wizard.go DefaultRoles().
// Adding a role here without seeding it server-side will silently fail
// the assignment because the FK on user_tenant_roles requires the
// (tenant_id, role_name) row to exist in `roles`.
const AVAILABLE_ROLES = [
  "owner",
  "tenant.admin",
  "tenant.member",
  "finance.admin",
  "hr.admin",
  "lms.admin",
  "crm.rep",
  "crm.manager",
  "inventory.admin",
  "helpdesk.agent",
  "helpdesk.manager",
  "sales.rep",
  "procurement.rep",
  "reporting.viewer",
];

interface SetupPayload {
  company_name: string;
  industry?: string;
  country?: string;
  coa_template: string;
  // locale is the BCP 47 tag the wizard wants the backend to persist
  // on tenants.locale. Omitting it (empty string → not sent) defers
  // to the backend's DefaultLocaleForCountry mapping for the chosen
  // country, mirroring the cfg.Locale-empty branch in
  // internal/tenant/wizard.go. The frontend always sends an explicit
  // tag the user can see in the step-0 picker.
  locale?: string;
  users: InitialUser[];
}

interface SetupResult {
  tenant_id: string;
  accounts_inserted: number;
  roles_inserted: number;
  users_inserted: number;
  coa_template_used: string;
  // locale_used reflects the locale the backend actually persisted
  // to tenants.locale after resolver downgrade. May differ from the
  // tag the wizard sent when the requested tag has no shipped
  // catalogue (e.g. "hi" → "en" today). The completion screen
  // surfaces this so the user can see what was actually committed.
  locale_used: string;
}

export function SetupWizardPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { t, setLocale } = useTranslation();

  const [step, setStep] = useState(0);
  const [companyName, setCompanyName] = useState("");
  const [industry, setIndustry] = useState("");
  const [country, setCountry] = useState("");
  // coaTemplate is empty until the user explicitly picks one from the
  // step-1 radio list. While empty, the effective value is derived
  // from the country field (see effectiveCoaTemplate below) so the UI
  // pre-selects the country-specific chart without needing a useEffect
  // sync between country and CoA. Once the user picks, the value
  // becomes sticky regardless of subsequent country edits.
  const [coaTemplate, setCoaTemplate] = useState("");
  // locale follows the same sticky-once-picked pattern as coaTemplate.
  // While empty, the effective UI locale is derived from country (via
  // bestSupportedLocaleForCountry, which downgrades unshipped tags to
  // the nearest shipped catalogue — so an IN tenant lands on en
  // until hi.json ships). Once the user picks an explicit locale from
  // the step-0 dropdown, the value becomes sticky and country edits
  // no longer override it. This mirrors the explicit-vs-implicit
  // resolution in internal/tenant/wizard.go where an operator-
  // supplied cfg.Locale bypasses the resolver downgrade.
  const [locale, setLocaleState] = useState("");
  const [users, setUsers] = useState<InitialUser[]>([
    { email: "", display_name: "", role: "tenant.admin", roles: ["tenant.admin"] },
  ]);

  const effectiveCoaTemplate =
    coaTemplate || defaultCoATemplateForCountry(country);
  // effectiveLocale is the tag the wizard will both submit to the
  // backend AND apply to the live UI. When the user hasn't picked
  // explicitly, we use bestSupportedLocaleForCountry so the UI flips
  // to a renderable catalogue immediately when they type a country
  // (e.g. typing "DE" switches the UI to German on step 1+). The
  // backend persistence path uses the same value, so the persisted
  // tenants.locale matches what the wizard renders during setup.
  const effectiveLocale = locale || bestSupportedLocaleForCountry(country);

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

  // Apply the locale switch when the user advances past step 0 so the
  // wizard's remaining steps render in the chosen language. We do this
  // here rather than in a useEffect on `country` change because the
  // user might type a country code slowly (e.g. "S" → "SG") and we
  // don't want the UI to flicker through partial-match locales. The
  // step-0 Next button is the natural commit point.
  //
  // The LocaleSwitcher in the header writes to the same source of
  // truth (LocaleProvider.setLocale), so a user who picks a locale
  // via the global header instead of the wizard dropdown ends up
  // with the same effective state when they reach step 1.
  const advancePastCompany = () => {
    if (effectiveLocale) {
      setLocale(effectiveLocale);
    }
    setStep(1);
  };

  const canAdvanceCompany = companyName.trim().length > 0;
  const validUsers = useMemo(
    () =>
      users
        .map((u) => {
          const trimmed = u.roles
            .map((r) => r.trim())
            .filter((r) => r.length > 0);
          const fallback = u.role.trim() || "tenant.admin";
          const list = trimmed.length > 0 ? trimmed : [fallback];
          return {
            email: u.email.trim(),
            display_name: u.display_name.trim(),
            role: list[0],
            roles: list,
          };
        })
        .filter((u) => u.email !== ""),
    [users],
  );

  const submitWizard = () => {
    submit.mutate({
      company_name: companyName.trim(),
      industry: industry.trim() || undefined,
      country: country.trim() || undefined,
      coa_template: effectiveCoaTemplate,
      // Submit `undefined` when the user hasn't explicitly picked a
      // locale from the dropdown so the backend takes the "not
      // operator-supplied" branch in internal/tenant/wizard.go: it
      // derives the locale from cfg.Country via its own
      // DefaultLocaleForCountry (mirror of the frontend table) and
      // runs the resolver to downgrade canonical-but-unshipped tags
      // ("zh-Hans" -> "zh", "hi" -> "en") before the strict
      // IsSupported gate. Sending the frontend's canonical tag
      // directly would trip that gate — wizard.go intentionally
      // skips the resolver downgrade on the operator-supplied path
      // so explicit picks the runtime can't serve surface as a loud
      // error rather than silent downgrade, and CN ("zh-Hans") / IN
      // ("hi") provisioning would then fail with
      //   tenant: locale "zh-Hans" is not a registered translation bundle
      // even though those values are exactly what
      // DefaultLocaleForCountry emits when called for CN / IN. An
      // earlier revision of this code sent the canonical tag here
      // hoping the persisted value would auto-promote when a future
      // catalogue (hi.json, zh-Hans.json) ships, but the strict
      // operator-supplied validation is incompatible with that
      // intent and the same auto-promotion happens via the backend's
      // resolver path anyway once the validator's IsSupported set
      // grows to include the new tag.
      locale: locale || undefined,
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
        {[
          { stepId: "company", label: t("wizard.step.company") },
          { stepId: "coa", label: t("wizard.step.coa") },
          { stepId: "users", label: t("wizard.step.users") },
          { stepId: "done", label: t("wizard.step.done") },
        ].map(({ stepId, label }, i) => (
          // The React key is the stable step identifier ("company"
          // / "coa" / "users" / "done") rather than the translated
          // label so a locale whose translations collide (e.g.
          // an abbreviation that maps two step names to the same
          // string) doesn't trigger a duplicate-key warning or
          // reorder during reconciliation.  The field is named
          // `stepId` (not `id`) to avoid shadowing the `id` from
          // `useParams` higher up in the component — both are
          // string-typed and a future contributor copy-pasting
          // markup between the outer and inner scopes could miss
          // that they refer to different values otherwise.
          <li
            key={stepId}
            style={{
              color: i === step ? "#111827" : "#9ca3af",
              fontWeight: i === step ? 600 : 400,
            }}
          >
            {i + 1}. {label}
          </li>
        ))}
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
          <label style={{ display: "grid", gap: 4 }}>
            {t("common.language")}
            <select
              value={effectiveLocale}
              onChange={(e) => setLocaleState(e.target.value)}
              aria-label={t("common.language")}
            >
              {SupportedLocales.map((info) => (
                <option key={info.tag} value={info.tag}>
                  {info.name}
                </option>
              ))}
            </select>
            <span style={{ color: "#6b7280", fontSize: 12 }}>
              {locale
                ? // The user has picked explicitly. Show which country
                  // would have selected the same locale (or note that
                  // they're overriding the country-derived default).
                  country &&
                  effectiveLocale !== bestSupportedLocaleForCountry(country)
                  ? t("wizard.locale.override_hint", {
                      country: country.trim().toUpperCase(),
                      default: localeInfo(
                        bestSupportedLocaleForCountry(country),
                      ).name,
                    })
                  : t("wizard.locale.explicit_hint")
                : country
                  ? t("wizard.locale.country_hint", {
                      country: country.trim().toUpperCase(),
                    })
                  : t("wizard.locale.browser_hint")}
            </span>
          </label>
          <div>
            <button
              type="button"
              disabled={!canAdvanceCompany}
              onClick={advancePastCompany}
            >
              {t("common.next")}
            </button>
          </div>
        </div>
      )}

      {step === 1 && (
        <div style={{ display: "grid", gap: 12 }}>
          <fieldset style={{ border: "1px solid #e5e7eb", padding: 12 }}>
            <legend>Chart of Accounts template</legend>
            {COA_TEMPLATES.map((tpl) => (
              <label
                key={tpl.value}
                style={{ display: "block", padding: "4px 0" }}
              >
                <input
                  type="radio"
                  name="coa"
                  value={tpl.value}
                  checked={effectiveCoaTemplate === tpl.value}
                  onChange={(e) => setCoaTemplate(e.target.value)}
                />{" "}
                {tpl.label}
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
              {t("common.back")}
            </button>
            <button type="button" onClick={() => setStep(2)}>
              {t("common.next")}
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
                      multiple
                      size={Math.min(6, AVAILABLE_ROLES.length)}
                      value={u.roles}
                      onChange={(e) => {
                        const next = Array.from(e.target.selectedOptions).map(
                          (o) => o.value,
                        );
                        setUsers((prev) =>
                          prev.map((row, j) =>
                            j === i
                              ? {
                                  ...row,
                                  // Keep `role` aligned with the first
                                  // selection so the legacy single-role
                                  // back-end column stays populated.
                                  role: next[0] ?? row.role,
                                  roles: next,
                                }
                              : row,
                          ),
                        );
                      }}
                    >
                      {AVAILABLE_ROLES.map((role) => (
                        <option key={role} value={role}>
                          {role}
                        </option>
                      ))}
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
                  {
                    email: "",
                    display_name: "",
                    role: "tenant.member",
                    roles: ["tenant.member"],
                  },
                ])
              }
            >
              Add another user
            </button>
          </div>
          <div style={{ display: "flex", gap: 8 }}>
            <button type="button" onClick={() => setStep(1)}>
              {t("common.back")}
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
            <li>
              {/* locale_used reflects the locale the backend persisted
                  after its resolver downgrade. May differ from the
                  effectiveLocale the wizard rendered with (e.g. the
                  user picked "hi" in step 0 but the backend
                  downgraded to "en" because hi.json doesn't ship).
                  Showing the persisted value is the source of truth
                  for what subsequent sessions will render against. */}
              {t("wizard.complete.locale_used", {
                locale: localeInfo(submit.data.locale_used).name,
                tag: submit.data.locale_used,
              })}
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
