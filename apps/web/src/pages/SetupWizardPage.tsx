import { useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import {
  bestSupportedLocaleForCountry,
  localeInfo,
  useTranslation,
} from "../lib/i18n";

// SetupWizardPage drives the tenant setup wizard on the frontend.
//
// Workstream 8 (Default-Wired Onboarding) collapses the wizard to two
// steps so a new tenant reaches a productive workspace in under two
// minutes:
//
//   1. Company name + country (the country is auto-detected from the
//      browser locale and stays editable).
//   2. Invite team members (entirely optional / skippable).
//
// Everything else the old multi-step wizard asked for — chart of
// accounts, currency, locale, roles, and feature flags — now resolves
// automatically from the country on the backend (see
// internal/tenant/wizard.go SmartDefaults / RunSetupWizard). The
// frontend still posts to POST /api/v1/tenants/{id}/setup; the shape
// of `SetupPayload` mirrors `tenant.SetupWizardConfig`, with the
// statutory fields derived from the country rather than picked by the
// operator.

// COA_TEMPLATES is the catalogue of chart-of-accounts templates the
// platform ships, keyed by the value persisted on the tenant. New
// charts are added by dropping a JSON file in the coa_templates
// folder, registering it in chartOfAccountsTemplates (wizard.go), and
// extending this list. The country-specific charts encode the local
// statutory liability accounts (e.g. CPF Payable for SG, GPSSA Payable
// for AE, AHV/ALV/BVG split for CH) so the payroll engine's deduction
// lines have a matching ledger destination on day one.
//
// The guided 2-step wizard no longer asks the operator to pick a chart
// (it now derives from country — see defaultCoATemplateForCountry),
// but this list is retained as the single source of truth for which
// template values are valid, and is the anchor cmd/new-tax-pack patches
// when a new statutory pack ships.
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
  { value: "ca_aspe_basic", label: "Canada — ASPE + CPP / EI / GST·HST·QST" },
  { value: "br_cpc_basic", label: "Brazil — CPC + IRRF / INSS / FGTS / ICMS·ISS·PIS·COFINS" },
  { value: "mx_nif_basic", label: "Mexico — NIF + ISR / IMSS / INFONAVIT / IVA" },
  { value: "ar_rtfacpce_basic", label: "Argentina — RT-FACPCE + Ganancias / Jubilación / IVA" },
  { value: "cl_ifrs_basic", label: "Chile — IFRS + Impuesto Único / AFP / Salud / IVA" },
  { value: "latam_ifrs_basic", label: "LATAM — IFRS + Generic Payroll Withholding (CO/PE/CR/PA/UY/EC/DO/GT/PY/TT)" },
  // Phase N1 — Europe Core + AU. Each chart carries the
  // country's statutory payroll-liability accounts (HMRC
  // PAYE / NIC, DRV / GKV / SPV / BA for DE, URSSAF + DGFiP
  // for FR, AEAT + Seguridad Social for ES, Agenzia delle
  // Entrate + INPS for IT, Belastingdienst + ZVW for NL,
  // ONSS/RSZ + SPF Finances for BE, Revenue PAYE / USC /
  // PRSI for IE, FA / ÖGK / Gemeinde for AT, AT + Segurança
  // Social for PT, ATO PAYG / Super for AU).
  { value: "gb_basic", label: "United Kingdom — IFRS + PAYE / NIC / Student Loan / VAT" },
  { value: "de_basic", label: "Germany — IFRS + Lohnsteuer / Soli / RV-KV-PV-ALV / USt" },
  { value: "fr_basic", label: "France — IFRS + PAS / CSG / CRDS / Sécu / TVA" },
  { value: "es_basic", label: "Spain — IFRS + IRPF / Seg. Social / IVA" },
  { value: "it_basic", label: "Italy — IFRS + IRPEF / Addizionali / INPS / IVA" },
  { value: "nl_basic", label: "Netherlands — IFRS + Loonheffing / ZVW / BTW" },
  { value: "be_basic", label: "Belgium — IFRS + Précompte / ONSS-RSZ / BTW-TVA" },
  { value: "ie_basic", label: "Ireland — IFRS + PAYE / USC / PRSI / VAT" },
  { value: "at_basic", label: "Austria — IFRS + Lohnsteuer / SV-Beiträge / Kommunalsteuer / USt" },
  { value: "pt_basic", label: "Portugal — IFRS + IRS / Segurança Social / IVA" },
  { value: "au_basic", label: "Australia — AASB + PAYG / Superannuation / FBT / Payroll Tax / GST" },
  // Phase N2 — Europe Extended. Each chart carries the country's
  // statutory payroll-liability accounts (ZUS / NFZ for PL,
  // Skatteverket / Tjänstepension for SE, Skatteetaten / NAV /
  // OTP for NO, Skattestyrelsen / ATP for DK, Verohallinto /
  // TyEL / SAVA for FI, ČSSZ / VZP for CZ, NAV / Szocho for HU,
  // ANAF / CAS / CASS for RO, AADE / EFKA for GR).
  { value: "pl_basic", label: "Poland — IFRS + PIT / ZUS / NFZ / VAT" },
  { value: "se_basic", label: "Sweden — IFRS + Kommunalskatt / Statlig / Pensionsavgift / Moms" },
  { value: "no_basic", label: "Norway — IFRS + Skatt / Trinnskatt / Trygdeavgift / OTP / MVA" },
  { value: "dk_basic", label: "Denmark — IFRS + A-skat / AM-bidrag / ATP / Moms" },
  { value: "fi_basic", label: "Finland — IFRS + Valtio / Kunnallisvero / TyEL / SAVA / ALV" },
  { value: "cz_basic", label: "Czech Republic — IFRS + Daň / SP / ZP / DPH" },
  { value: "hu_basic", label: "Hungary — IFRS + SZJA / TB / Szocho / ÁFA" },
  { value: "ro_basic", label: "Romania — IFRS + Impozit / CAS / CASS / TVA" },
  { value: "gr_basic", label: "Greece — IFRS + Income Tax / EFKA / ΦΠΑ" },
  // Phase N3 — Africa + East Asia. Each chart carries the
  // country's statutory payroll-liability accounts (SARS PAYE
  // / UIF / SDL for ZA, FIRS PAYE / PenCom / NHF for NG, KRA
  // PAYE / NSSF / SHIF / Housing Levy for KE, ETA PIT / Social
  // Insurance for EG, NTA Gensenchōshū / Shakai Hoken for JP,
  // NTS Geunrosodeukse / NPS / NHI / EI for KR).
  { value: "za_basic", label: "South Africa — IFRS + PAYE / UIF / SDL / VAT" },
  { value: "ng_basic", label: "Nigeria — IFRS + PAYE / Pension / NHF / WHT / VAT" },
  { value: "ke_basic", label: "Kenya — IFRS + PAYE / NSSF / SHIF / Housing Levy / VAT" },
  { value: "eg_basic", label: "Egypt — IFRS + PIT / Social Insurance / Stamp Duty / VAT" },
  { value: "jp_basic", label: "Japan — IFRS + Gensenchōshū / Shakai Hoken / Consumption Tax" },
  { value: "kr_basic", label: "South Korea — IFRS + Geunrosodeukse / NPS / NHI / EI / VAT" },
  // SCAFFOLD: cmd/new-tax-pack inserts new COA_TEMPLATES entries above this line.
];

// COUNTRY_COA_DEFAULTS mirrors tenant.DefaultCoATemplateForCountry in
// internal/tenant/wizard.go so the wizard can resolve the
// country-specific chart without a manual CoA selection step. Keeping
// the table in lockstep with the backend means a SG tenant gets
// sg_basic and the payroll deduction lines have matching liability
// accounts on day one.
//
// Drift safety: the backend applies the same country -> template
// mapping when callers omit coa_template entirely. The frontend sends
// an explicit value derived from the country here so the persisted
// chart matches the (read-only) country the operator confirmed — keep
// this map in sync with internal/tenant/wizard.go on every PR that
// adds a tax pack.
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
  CA: "ca_aspe_basic",
  BR: "br_cpc_basic",
  MX: "mx_nif_basic",
  AR: "ar_rtfacpce_basic",
  CL: "cl_ifrs_basic",
  CO: "latam_ifrs_basic",
  PE: "latam_ifrs_basic",
  CR: "latam_ifrs_basic",
  PA: "latam_ifrs_basic",
  UY: "latam_ifrs_basic",
  EC: "latam_ifrs_basic",
  DO: "latam_ifrs_basic",
  GT: "latam_ifrs_basic",
  PY: "latam_ifrs_basic",
  TT: "latam_ifrs_basic",
  GB: "gb_basic",
  DE: "de_basic",
  FR: "fr_basic",
  ES: "es_basic",
  IT: "it_basic",
  NL: "nl_basic",
  BE: "be_basic",
  IE: "ie_basic",
  AT: "at_basic",
  PT: "pt_basic",
  AU: "au_basic",
  PL: "pl_basic",
  SE: "se_basic",
  NO: "no_basic",
  DK: "dk_basic",
  FI: "fi_basic",
  CZ: "cz_basic",
  HU: "hu_basic",
  RO: "ro_basic",
  GR: "gr_basic",
  ZA: "za_basic",
  NG: "ng_basic",
  KE: "ke_basic",
  EG: "eg_basic",
  JP: "jp_basic",
  KR: "kr_basic",
  // SCAFFOLD: cmd/new-tax-pack inserts new COUNTRY_COA_DEFAULTS entries above this line.
};

function defaultCoATemplateForCountry(country: string): string {
  const code = country.trim().toUpperCase();
  const derived = COUNTRY_COA_DEFAULTS[code] ?? "ifrs_basic";
  // Guard against a COUNTRY_COA_DEFAULTS entry pointing at a template
  // value no longer present in COA_TEMPLATES (e.g. a pack renamed
  // without updating both tables): fall back to the generic IFRS chart
  // rather than persisting an unknown template id.
  return COA_TEMPLATES.some((t) => t.value === derived) ? derived : "ifrs_basic";
}

// detectCountryFromBrowser extracts an ISO 3166-1 alpha-2 region code
// from the browser's preferred languages (e.g. "de-CH" -> "CH"). It is
// purely a convenience pre-fill for the country field; the operator
// can always edit it, and the backend re-detects authoritatively from
// the KChat profile locale / geo-IP. Returns "" when no region subtag
// is present so the field starts empty rather than guessing.
function detectCountryFromBrowser(): string {
  if (typeof navigator === "undefined") {
    return "";
  }
  // navigator.languages already begins with navigator.language (per
  // the HTML spec), so prefer it outright and only fall back to the
  // singular navigator.language when languages is empty/unavailable.
  // This avoids scanning the most-preferred tag twice.
  const candidates =
    navigator.languages && navigator.languages.length > 0
      ? navigator.languages
      : [navigator.language];
  for (const tag of candidates) {
    if (!tag) continue;
    const parts = tag.split(/[-_]/);
    for (let i = 1; i < parts.length; i++) {
      const p = parts[i];
      if (p && p.length === 2 && /^[a-zA-Z]{2}$/.test(p)) {
        return p.toUpperCase();
      }
    }
  }
  return "";
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
  country?: string;
  // coa_template is derived from country (defaultCoATemplateForCountry)
  // rather than selected by the operator — the wizard no longer shows a
  // CoA step. Sending the explicit value keeps the persisted chart in
  // lockstep with the confirmed country.
  coa_template: string;
  // locale is the BCP 47 tag derived from country
  // (bestSupportedLocaleForCountry). Always a shipped-catalogue tag so
  // the backend's operator-supplied validator accepts it
  // unconditionally.
  locale?: string;
  users: InitialUser[];
}

interface SetupResult {
  tenant_id: string;
  accounts_inserted: number;
  roles_inserted: number;
  users_inserted: number;
  coa_template_used: string;
  // locale_used reflects the locale the backend actually persisted to
  // tenants.locale after resolver downgrade. The completion screen
  // surfaces this so the user sees what was committed.
  locale_used: string;
}

export function SetupWizardPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { t, setLocale, locale: providerLocale } = useTranslation();

  const [step, setStep] = useState(0);
  const [companyName, setCompanyName] = useState("");
  // The country field is pre-filled from the browser locale so the
  // common case ("my company is in the country my browser says")
  // needs zero typing. detectedCountry is captured once so we can show
  // the "auto-detected" hint only while the value is untouched.
  const [detectedCountry] = useState(detectCountryFromBrowser);
  const [country, setCountry] = useState(detectedCountry);
  const [users, setUsers] = useState<InitialUser[]>([
    { email: "", display_name: "", role: "tenant.admin", roles: ["tenant.admin"] },
  ]);

  // CoA + locale resolve from the (read-only-by-default) country. The
  // operator never picks them; this is the "everything else resolves
  // from country automatically" contract from Workstream 8.
  const effectiveCoaTemplate = defaultCoATemplateForCountry(country);
  const effectiveLocale = country
    ? bestSupportedLocaleForCountry(country)
    : providerLocale;

  const tenantId = id ?? "";

  const submit = useMutation<SetupResult, Error, SetupPayload>({
    mutationFn: async (payload) => {
      const res = await fetch(
        `/api/v1/tenants/${encodeURIComponent(tenantId)}/setup`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "X-Tenant-ID": localStorage.getItem("kapp.tenant") ?? tenantId,
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
      setStep(2);
    },
  });

  // Apply the country-derived locale when the user advances past the
  // company step so the team step renders in the resolved language. We
  // only call setLocale when a country is present (effectiveLocale is
  // otherwise the LocaleProvider's existing value, making the write a
  // redundant no-op that would freeze the navigator-detected value).
  const advancePastCompany = () => {
    if (country) {
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

  // submitWizard posts the aggregated payload. The `includeUsers`
  // flag lets the "Skip" button finish with no invites (sending an
  // empty users array) regardless of any half-typed invite rows.
  const submitWizard = (includeUsers = true) => {
    submit.mutate({
      company_name: companyName.trim(),
      country: country.trim() || undefined,
      coa_template: effectiveCoaTemplate,
      locale: effectiveLocale,
      users: includeUsers ? validUsers : [],
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
        Just confirm your company name and country — we set up your chart
        of accounts, currency, language, roles, and features automatically.
        You can change any of it later from the admin pages.
      </p>
      {/* The two-step indicator is only meaningful while the operator
          is moving through the wizard (steps 0-1). On the completion
          screen (step === 2) neither step is "active", so a dimmed
          indicator above the success message is just noise — hide it. */}
      {step < 2 && (
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
            { stepId: "users", label: t("wizard.step.users") },
          ].map(({ stepId, label }, i) => (
            // The React key is the stable step identifier rather than the
            // translated label so a locale whose translations collide
            // doesn't trigger a duplicate-key warning. `stepId` (not
            // `id`) avoids shadowing the route `id` from useParams.
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
      )}

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
            Country
            <input
              value={country}
              onChange={(e) => setCountry(e.target.value)}
              placeholder="ISO country code (e.g. US, SG, DE)"
            />
            <span style={{ color: "#6b7280", fontSize: 12 }}>
              {detectedCountry && country === detectedCountry
                ? `Auto-detected from your browser (${detectedCountry}). Edit if this isn't right — your chart of accounts, currency, and language follow from it.`
                : country.trim()
                  ? `Chart of accounts, currency, and language will be set up for ${country.trim().toUpperCase()}.`
                  : "Leave blank to use generic IFRS defaults, or enter a country to localise your setup."}
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
          <p style={{ fontSize: 13, color: "#6b7280" }}>
            Invite your team (optional). Each user is seeded into the{" "}
            <code>users</code> table and added to the tenant via{" "}
            <code>user_tenants</code> with the selected role. You can skip
            this and invite people later from the admin pages.
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
                                  // Keep `role` aligned with the first
                                  // selection so the legacy single-role
                                  // back-end column stays populated.
                                  ...row,
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
            <button type="button" onClick={() => setStep(0)}>
              {t("common.back")}
            </button>
            <button
              type="button"
              onClick={() => submitWizard(false)}
              disabled={submit.isPending}
            >
              Skip for now
            </button>
            <button
              type="button"
              onClick={() => submitWizard(true)}
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

      {step === 2 && submit.data && (
        <div style={{ display: "grid", gap: 12 }}>
          <h2>Setup complete</h2>
          <ul style={{ fontSize: 13 }}>
            <li>
              CoA template: <code>{submit.data.coa_template_used}</code>
            </li>
            <li>
              {t("wizard.complete.locale_used", {
                locale: localeInfo(submit.data.locale_used).name,
                tag: submit.data.locale_used,
              })}
            </li>
            <li>Accounts seeded: {submit.data.accounts_inserted}</li>
            <li>Roles seeded: {submit.data.roles_inserted}</li>
            <li>Users invited: {submit.data.users_inserted}</li>
          </ul>
          <p style={{ fontSize: 13, color: "#6b7280" }}>
            Next, work through your <strong>Getting Started</strong> checklist
            to create your first contact, send an invoice, and import data.
          </p>
          <div style={{ display: "flex", gap: 8 }}>
            <button type="button" onClick={() => navigate("/onboarding")}>
              Open Getting Started
            </button>
            <button type="button" onClick={() => navigate("/")}>
              Go to tenant home
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
