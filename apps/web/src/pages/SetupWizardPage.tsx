import { useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import {
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  Input,
  Select,
  Stepper,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
} from "@kapp/ui";
import { Check, ChevronDown, Plus, Search } from "lucide-react";
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
  { value: "cn_basic", label: "China — CAS/IFRS + IIT / Social Insurance / Housing Fund / VAT" },
  { value: "hk_basic", label: "Hong Kong — IFRS + MPF (mandatory provident fund)" },
  { value: "tw_basic", label: "Taiwan — IFRS + Income Tax / Labor Insurance / NHI" },
  { value: "kh_basic", label: "Cambodia — IFRS + Tax on Salary / NSSF" },
  { value: "mm_basic", label: "Myanmar — IFRS + Income Tax / Social Security Board" },
  { value: "bd_basic", label: "Bangladesh — IFRS + Income Tax (TDS)" },
  { value: "lk_basic", label: "Sri Lanka — IFRS + APIT / EPF / ETF" },
  { value: "pk_basic", label: "Pakistan — IFRS + Income Tax / EOBI" },
  { value: "jo_basic", label: "Jordan — IFRS + Income Tax / Social Security" },
  { value: "lb_basic", label: "Lebanon — IFRS + Payroll Tax (R10) / NSSF" },
  { value: "ma_basic", label: "Morocco — IFRS + IR / CNSS / AMO" },
  { value: "tn_basic", label: "Tunisia — IFRS + IRPP / CNSS" },
  { value: "gh_basic", label: "Ghana — IFRS + PAYE / SSNIT" },
  // SCAFFOLD: cmd/new-tax-pack inserts new COA_TEMPLATES entries above this line.
];

// CoA templates are grouped by region in the wizard so the 50+
// country charts are navigable.  Each region owns the explicit set
// of template values that belong to it; anything unmatched (e.g. the
// generic IFRS / US GAAP base charts) falls into "General".  The
// order of REGION_ORDER is the on-screen section order.
const REGION_ORDER = [
  "General",
  "Americas",
  "Europe",
  "Middle East",
  "Asia-Pacific",
  "Africa",
] as const;
type CoaRegion = (typeof REGION_ORDER)[number];

const REGION_BY_TEMPLATE: Record<string, CoaRegion> = {
  // General base charts (not country-specific).
  ifrs_basic: "General",
  us_gaap_basic: "Americas",
  // Americas.
  ca_aspe_basic: "Americas",
  br_cpc_basic: "Americas",
  mx_nif_basic: "Americas",
  ar_rtfacpce_basic: "Americas",
  cl_ifrs_basic: "Americas",
  latam_ifrs_basic: "Americas",
  // Europe.
  ch_basic: "Europe",
  gb_basic: "Europe",
  de_basic: "Europe",
  fr_basic: "Europe",
  es_basic: "Europe",
  it_basic: "Europe",
  nl_basic: "Europe",
  be_basic: "Europe",
  ie_basic: "Europe",
  at_basic: "Europe",
  pt_basic: "Europe",
  pl_basic: "Europe",
  se_basic: "Europe",
  no_basic: "Europe",
  dk_basic: "Europe",
  fi_basic: "Europe",
  cz_basic: "Europe",
  hu_basic: "Europe",
  ro_basic: "Europe",
  gr_basic: "Europe",
  // Middle East (GCC).
  ae_basic: "Middle East",
  sa_basic: "Middle East",
  qa_basic: "Middle East",
  kw_basic: "Middle East",
  bh_basic: "Middle East",
  om_basic: "Middle East",
  // Asia-Pacific.
  sg_basic: "Asia-Pacific",
  my_basic: "Asia-Pacific",
  th_basic: "Asia-Pacific",
  id_basic: "Asia-Pacific",
  vn_basic: "Asia-Pacific",
  ph_basic: "Asia-Pacific",
  nz_basic: "Asia-Pacific",
  in_basic: "Asia-Pacific",
  au_basic: "Asia-Pacific",
  jp_basic: "Asia-Pacific",
  kr_basic: "Asia-Pacific",
  cn_basic: "Asia-Pacific",
  // Session 14 Asia-Pacific additions.
  hk_basic: "Asia-Pacific",
  tw_basic: "Asia-Pacific",
  kh_basic: "Asia-Pacific",
  mm_basic: "Asia-Pacific",
  bd_basic: "Asia-Pacific",
  lk_basic: "Asia-Pacific",
  pk_basic: "Asia-Pacific",
  // Session 14 Middle East (non-GCC) additions.
  jo_basic: "Middle East",
  lb_basic: "Middle East",
  // Africa.
  za_basic: "Africa",
  ng_basic: "Africa",
  ke_basic: "Africa",
  eg_basic: "Africa",
  // Session 14 Africa additions (North + West).
  ma_basic: "Africa",
  tn_basic: "Africa",
  gh_basic: "Africa",
  // SCAFFOLD: cmd/new-tax-pack must also add the new template's region
  // here, alongside its COA_TEMPLATES entry. A missing entry is not a
  // crash — regionForTemplate() falls back to "General" — but the chart
  // will be grouped under the wrong region in the wizard.
};

function regionForTemplate(value: string): CoaRegion {
  return REGION_BY_TEMPLATE[value] ?? "General";
}

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
  // PR-2d: Americas — five standards-named charts plus a
  // shared LATAM IFRS chart for the remaining ten jurisdictions.
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
  // Phase N1 — Europe Core + AU.
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
  // Phase N2 — Europe Extended.
  PL: "pl_basic",
  SE: "se_basic",
  NO: "no_basic",
  DK: "dk_basic",
  FI: "fi_basic",
  CZ: "cz_basic",
  HU: "hu_basic",
  RO: "ro_basic",
  GR: "gr_basic",
  // Phase N3 — Africa + East Asia.
  ZA: "za_basic",
  NG: "ng_basic",
  KE: "ke_basic",
  EG: "eg_basic",
  JP: "jp_basic",
  KR: "kr_basic",
  CN: "cn_basic",
  HK: "hk_basic",
  TW: "tw_basic",
  KH: "kh_basic",
  MM: "mm_basic",
  BD: "bd_basic",
  LK: "lk_basic",
  PK: "pk_basic",
  JO: "jo_basic",
  LB: "lb_basic",
  MA: "ma_basic",
  TN: "tn_basic",
  GH: "gh_basic",
  // SCAFFOLD: cmd/new-tax-pack inserts new COUNTRY_COA_DEFAULTS entries above this line.
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
  const { t, setLocale, locale: providerLocale } = useTranslation();

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
  // Step-1 CoA picker UX: a free-text filter plus a set of
  // collapsed region sections.  Regions default to expanded (the
  // set holds the ones the user has *collapsed*) so every template
  // is reachable without a click; an active search overrides
  // collapse so matches are never hidden.
  const [coaQuery, setCoaQuery] = useState("");
  const [collapsedRegions, setCollapsedRegions] = useState<Set<CoaRegion>>(
    () => new Set(),
  );

  const effectiveCoaTemplate =
    coaTemplate || defaultCoATemplateForCountry(country);

  // Templates grouped into their on-screen region sections, filtered
  // by the current search query (matches label or value).  Empty
  // regions are dropped so a search that matches nothing in a region
  // hides its header too.
  const coaRegions = useMemo(() => {
    const q = coaQuery.trim().toLowerCase();
    const byRegion = new Map<CoaRegion, typeof COA_TEMPLATES>();
    for (const tpl of COA_TEMPLATES) {
      if (
        q &&
        !tpl.label.toLowerCase().includes(q) &&
        !tpl.value.toLowerCase().includes(q)
      ) {
        continue;
      }
      const region = regionForTemplate(tpl.value);
      const list = byRegion.get(region) ?? [];
      list.push(tpl);
      byRegion.set(region, list);
    }
    return REGION_ORDER.map((region) => ({
      region,
      templates: byRegion.get(region) ?? [],
    })).filter((g) => g.templates.length > 0);
  }, [coaQuery]);
  // effectiveLocale is the tag the wizard will both submit to the
  // backend AND apply to the live UI. The three-stage fallback
  // mirrors the precedence the user expects:
  //
  //   1. explicit pick from the step-0 dropdown wins outright
  //   2. country-derived locale (downgraded to the shipped catalogue
  //      set via bestSupportedLocaleForCountry) — so typing "DE"
  //      flips the UI to German on step 1+ without an explicit pick
  //   3. the LocaleProvider's current value (navigator / cookie /
  //      localStorage resolution) — so a user with a French
  //      browser who hasn't entered a country yet still sees the
  //      dropdown reading "Français" instead of being forced to
  //      "English" via DefaultLocale
  //
  // The dropdown reflects this value and the wizard submits it as-is
  // so the persisted tenants.locale matches what the user picked
  // (whether explicitly or via country derivation) without the
  // backend silently re-deriving from a different source.
  const effectiveLocale =
    locale || (country ? bestSupportedLocaleForCountry(country) : providerLocale);

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
  // We only persist via setLocale when the user has expressed locale
  // intent — either an explicit dropdown pick (locale is set) or a
  // country that derives one (country is set). When both are empty
  // the LocaleProvider already holds the navigator/cookie-derived
  // value (which is what effectiveLocale falls back to above), so
  // calling setLocale would be a redundant write to the same value;
  // skipping the call keeps the user's existing global locale
  // preference intact rather than "freezing" the navigator-detected
  // value into the cookie just because they advanced past step 0.
  //
  // The LocaleSwitcher in the header writes to the same source of
  // truth (LocaleProvider.setLocale), so a user who picks a locale
  // via the global header instead of the wizard dropdown ends up
  // with the same effective state when they reach step 1.
  const advancePastCompany = () => {
    if (locale || country) {
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
      // Submit the same tag the dropdown displayed, after both the
      // country-derived fallback and the LocaleProvider fallback have
      // resolved. This guarantees the persisted tenants.locale equals
      // what the user saw on the form — no silent re-derivation on the
      // backend.
      //
      // The submitted value is always a shipped catalogue tag because
      // bestSupportedLocaleForCountry pipes the canonical tag through
      // bestSupportedLocale's progressive-subtag downgrade (so CN's
      // canonical "zh-Hans" becomes "zh", IN's "hi" becomes "en")
      // before it surfaces in the dropdown. The backend's strict
      // operator-supplied validator (which skips the resolver and
      // hits IsSupported directly) therefore accepts the value
      // unconditionally — we can never send a canonical-but-unshipped
      // tag like "zh-Hans" or "hi" because the dropdown never holds
      // one.
      //
      // When `hi.json` or `zh-Hans.json` ship in a future PR,
      // bestSupportedLocale will stop downgrading those tags and the
      // wizard will start submitting them directly without any code
      // change to this site — the auto-promotion happens via the
      // dropdown's shipped-catalogue lookup, not via a backend
      // re-derivation we'd have to coordinate.
      //
      // Sending `undefined` here would re-introduce the mismatch the
      // bot flagged: the dropdown might show "Français" (from the
      // LocaleProvider's navigator fallback) while the backend
      // persists "en" because cfg.Country was empty and
      // DefaultLocaleForCountry("") returns "en". Always submitting
      // effectiveLocale closes that mismatch.
      locale: effectiveLocale,
      users: validUsers,
    });
  };

  if (!tenantId) {
    return (
      <section className="mx-auto flex max-w-2xl flex-col gap-4">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Tenant Setup
        </h1>
        <div
          role="alert"
          className="rounded-md border border-border border-s-2 border-s-danger bg-bg-subtle px-3 py-2 text-sm text-danger"
        >
          Missing tenant id in route. Expected <code>/setup/:id</code>.
        </div>
      </section>
    );
  }

  return (
    <section className="mx-auto flex max-w-2xl flex-col gap-6">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Tenant Setup
        </h1>
        <p className="text-sm text-fg-muted">
          Seeds the chart of accounts, default roles, and invites your
          starting team. You can edit every value after setup from the
          admin pages.
        </p>
      </header>
      {/* Stepper marks indices < current as completed (check marker)
          and index === current as active, tracking the company → CoA
          → users → done progression. */}
      <Stepper
        current={step}
        steps={[
          { label: t("wizard.step.company") },
          { label: t("wizard.step.coa") },
          { label: t("wizard.step.users") },
          { label: t("wizard.step.done") },
        ]}
      />

      {step === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>{t("wizard.step.company")}</CardTitle>
            <CardDescription>
              Tell us who you are. These details seed the company profile
              and drive the country-specific defaults on the next step.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <label className="flex flex-col gap-1.5">
              <span className="text-sm font-medium text-fg">Company name</span>
              <Input
                value={companyName}
                onChange={(e) => setCompanyName(e.target.value)}
                required
              />
            </label>
            <label className="flex flex-col gap-1.5">
              <span className="text-sm font-medium text-fg">Industry</span>
              <Input
                value={industry}
                onChange={(e) => setIndustry(e.target.value)}
                placeholder="e.g. Software, Retail"
              />
            </label>
            <label className="flex flex-col gap-1.5">
              <span className="text-sm font-medium text-fg">Country</span>
              <Input
                value={country}
                onChange={(e) => setCountry(e.target.value)}
                placeholder="ISO country code or name"
              />
            </label>
            <label className="flex flex-col gap-1.5">
              <span className="text-sm font-medium text-fg">
                {t("common.language")}
              </span>
              <Select
                value={effectiveLocale}
                onChange={(e) => setLocaleState(e.target.value)}
                aria-label={t("common.language")}
              >
                {SupportedLocales.map((info) => (
                  <option key={info.tag} value={info.tag}>
                    {info.name}
                  </option>
                ))}
              </Select>
              {/* Hint copy reflects the locale-resolution precedence:
                  an explicit pick wins, else a country-derived locale,
                  else the LocaleProvider's navigator/cookie value. */}
              <span className="text-xs font-normal text-fg-muted">
                {locale
                  ? country &&
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
            <div className="flex justify-end">
              <Button
                type="button"
                disabled={!canAdvanceCompany}
                onClick={advancePastCompany}
              >
                {t("common.next")}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {step === 1 && (
        <Card>
          <CardHeader>
            <CardTitle>{t("wizard.step.coa")}</CardTitle>
            <CardDescription>
              Pick the chart of accounts for your statutory jurisdiction.
              Selecting a country on the previous step pre-selects the
              matching chart; search or browse by region to change it.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <Input
              type="search"
              value={coaQuery}
              onChange={(e) => setCoaQuery(e.target.value)}
              placeholder="Search templates…"
              aria-label="Search chart of accounts templates"
              leadingAddon={<Search className="h-4 w-4" />}
            />
            <div
              role="radiogroup"
              aria-label="Chart of Accounts template"
              className="flex flex-col gap-2"
            >
              {coaRegions.length === 0 ? (
                <p className="px-1 py-6 text-center text-sm text-fg-muted">
                  No templates match “{coaQuery.trim()}”.
                </p>
              ) : (
                coaRegions.map(({ region, templates }) => {
                  // A region is collapsed only when the user collapsed
                  // it AND there's no active search (search always
                  // reveals matches so nothing is hidden behind a
                  // collapsed header).
                  const collapsed =
                    !coaQuery.trim() && collapsedRegions.has(region);
                  return (
                    <div
                      key={region}
                      className="overflow-hidden rounded-md border border-border"
                    >
                      <button
                        type="button"
                        aria-expanded={!collapsed}
                        onClick={() =>
                          setCollapsedRegions((prev) => {
                            const next = new Set(prev);
                            if (next.has(region)) next.delete(region);
                            else next.add(region);
                            return next;
                          })
                        }
                        className="flex w-full items-center justify-between gap-2 bg-bg-subtle px-3 py-2 text-left text-sm font-semibold text-fg transition-colors hover:bg-bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
                      >
                        <span>{region}</span>
                        <span className="flex items-center gap-2 text-xs font-normal text-fg-subtle">
                          {templates.length}
                          <ChevronDown
                            className={cn(
                              "h-4 w-4 transition-transform",
                              collapsed && "-rotate-90",
                            )}
                          />
                        </span>
                      </button>
                      {!collapsed && (
                        <div className="flex flex-col border-t border-border">
                          {templates.map((tpl) => {
                            const checked = effectiveCoaTemplate === tpl.value;
                            return (
                              <label
                                key={tpl.value}
                                className={cn(
                                  "flex cursor-pointer items-start gap-2.5 px-3 py-2 text-sm transition-colors hover:bg-bg-subtle",
                                  checked && "bg-bg-subtle font-medium",
                                )}
                              >
                                <input
                                  type="radio"
                                  name="coa"
                                  value={tpl.value}
                                  checked={checked}
                                  onChange={(e) =>
                                    setCoaTemplate(e.target.value)
                                  }
                                  className="mt-0.5 accent-[var(--accent)]"
                                />
                                <span className="text-fg">{tpl.label}</span>
                              </label>
                            );
                          })}
                        </div>
                      )}
                    </div>
                  );
                })
              )}
            </div>
            <p className="text-xs text-fg-muted">
              Templates live in{" "}
              <code>internal/tenant/coa_templates/</code>. Every account is
              inserted with{" "}
              <code>ON CONFLICT (tenant_id, code) DO NOTHING</code> so the
              step is safe to re-run.
            </p>
            <div className="flex justify-between">
              <Button
                type="button"
                variant="secondary"
                onClick={() => setStep(0)}
              >
                {t("common.back")}
              </Button>
              <Button type="button" onClick={() => setStep(2)}>
                {t("common.next")}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {step === 2 && (
        <Card>
          <CardHeader>
            <CardTitle>{t("wizard.step.users")}</CardTitle>
            <CardDescription>
              Invite initial team members. Each user is seeded into the{" "}
              <code>users</code> table and added to the tenant via{" "}
              <code>user_tenants</code> with the selected roles.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Email</TableHead>
                  <TableHead>Display name</TableHead>
                  <TableHead>Roles</TableHead>
                  <TableHead>
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.map((u, i) => (
                  <TableRow key={i}>
                    <TableCell>
                      <Input
                        value={u.email}
                        onChange={(e) =>
                          setUsers((prev) =>
                            prev.map((row, j) =>
                              j === i
                                ? { ...row, email: e.target.value }
                                : row,
                            ),
                          )
                        }
                        type="email"
                        placeholder="name@example.com"
                        aria-label={`Email for user ${i + 1}`}
                      />
                    </TableCell>
                    <TableCell>
                      <Input
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
                        aria-label={`Display name for user ${i + 1}`}
                      />
                    </TableCell>
                    <TableCell>
                      {/* The Select primitive is single-select by
                          design, so roles render as a checkbox group.
                          `role` mirrors the first checked role to keep
                          the legacy single-role back-end column
                          populated. */}
                      <fieldset
                        className="flex flex-col gap-1"
                        aria-label={`Roles for user ${i + 1}`}
                      >
                        {AVAILABLE_ROLES.map((role) => (
                          <label
                            key={role}
                            className="flex items-center gap-2 text-sm text-fg"
                          >
                            <input
                              type="checkbox"
                              value={role}
                              checked={u.roles.includes(role)}
                              className="accent-[var(--accent)]"
                              onChange={(e) =>
                                setUsers((prev) =>
                                  prev.map((row, j) => {
                                    if (j !== i) return row;
                                    const nextRoles = e.target.checked
                                      ? [...row.roles, role]
                                      : row.roles.filter((r) => r !== role);
                                    return {
                                      ...row,
                                      role: nextRoles[0] ?? row.role,
                                      roles: nextRoles,
                                    };
                                  }),
                                )
                              }
                            />
                            {role}
                          </label>
                        ))}
                      </fieldset>
                    </TableCell>
                    <TableCell>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() =>
                          setUsers((prev) => prev.filter((_, j) => j !== i))
                        }
                        disabled={users.length <= 1}
                      >
                        Remove
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <div>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                leadingIcon={<Plus className="h-4 w-4" />}
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
              </Button>
            </div>
            {submit.isError && (
              <div
                role="alert"
                className="rounded-md border border-border border-s-2 border-s-danger bg-bg-subtle px-3 py-2 text-sm text-danger"
              >
                Setup failed: {submit.error.message}
              </div>
            )}
            <div className="flex justify-between">
              <Button
                type="button"
                variant="secondary"
                onClick={() => setStep(1)}
              >
                {t("common.back")}
              </Button>
              <Button
                type="button"
                onClick={submitWizard}
                disabled={submit.isPending}
              >
                {submit.isPending ? "Running setup…" : "Finish setup"}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {step === 3 && submit.data && (
        <Card>
          <CardContent className="flex flex-col items-center gap-5 py-10 text-center">
            <div className="flex h-14 w-14 items-center justify-center rounded-full bg-success text-success-fg animate-in zoom-in-50 duration-300">
              <Check className="h-7 w-7" strokeWidth={2.5} />
            </div>
            <div className="flex flex-col gap-1">
              <h2 className="text-xl font-semibold tracking-tight text-fg">
                Setup complete
              </h2>
              <p className="text-sm text-fg-muted">
                Your tenant is ready. Here's what we seeded.
              </p>
            </div>
            <dl className="grid w-full max-w-sm grid-cols-2 gap-x-4 gap-y-2 text-left text-sm">
              <dt className="text-fg-muted">CoA template</dt>
              <dd className="text-end font-medium text-fg">
                <code>{submit.data.coa_template_used}</code>
              </dd>
              {/* locale_used reflects the locale the backend persisted
                  after its resolver downgrade. May differ from the
                  effectiveLocale the wizard rendered with (e.g. the user
                  picked "hi" but the backend downgraded to "en" because
                  hi.json doesn't ship). The persisted value is the
                  source of truth for subsequent sessions. */}
              <dt className="text-fg-muted">Locale</dt>
              <dd className="text-end font-medium text-fg">
                {t("wizard.complete.locale_used", {
                  locale: localeInfo(submit.data.locale_used).name,
                  tag: submit.data.locale_used,
                })}
              </dd>
              <dt className="text-fg-muted">Accounts seeded</dt>
              <dd className="text-end font-medium text-fg">
                {submit.data.accounts_inserted}
              </dd>
              <dt className="text-fg-muted">Roles seeded</dt>
              <dd className="text-end font-medium text-fg">
                {submit.data.roles_inserted}
              </dd>
              <dt className="text-fg-muted">Users invited</dt>
              <dd className="text-end font-medium text-fg">
                {submit.data.users_inserted}
              </dd>
            </dl>
            <Button type="button" onClick={() => navigate("/")}>
              Go to tenant home
            </Button>
          </CardContent>
        </Card>
      )}
    </section>
  );
}
