package tenant

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// defaultSLABreachActionType mirrors helpdesk.ActionTypeSLABreach.
// The tenant package cannot import helpdesk without creating a cycle
// (platform → tenant → helpdesk → ktype → platform), so the literal
// is duplicated here with a test-enforced drift check in
// internal/integrationtest/sla_breach_test.go. Keep them in sync.
const (
	defaultSLABreachActionType      = "sla_breach_check"
	defaultSLABreachIntervalSeconds = 300
)

// SetupWizardConfig is the payload a tenant owner submits to seed their
// newly-created tenant. It covers the first-run choices ERPNext surfaces
// in its own Setup Wizard — company profile, country/industry, the
// chart-of-accounts template, and the initial role roster.
type SetupWizardConfig struct {
	CompanyName  string `json:"company_name"`
	Industry     string `json:"industry,omitempty"`
	Country      string `json:"country,omitempty"`
	CurrencyCode string `json:"currency_code,omitempty"`
	// Locale is the IETF BCP 47 tag the API resolves the i18n
	// bundle from for this tenant. When empty the wizard derives
	// a sensible default from Country (CH→de, SA→ar, …) so
	// operators that don't tick a locale checkbox still land on
	// a locale that matches their statutory jurisdiction. See
	// DefaultLocaleForCountry below for the full mapping.
	Locale      string       `json:"locale,omitempty"`
	CoATemplate string       `json:"coa_template,omitempty"`
	Roles       []WizardRole `json:"roles,omitempty"`
	Users       []WizardUser `json:"users,omitempty"`
	SampleData  bool         `json:"sample_data,omitempty"`
	Plan        string       `json:"plan,omitempty"`
	CreatedBy   uuid.UUID    `json:"created_by,omitempty"`
}

// DefaultLocaleForCountry returns the canonical UI locale tag the
// wizard should pre-fill for the given ISO 3166-1 alpha-2 country
// code. The mapping is intentionally one default per country (not
// "every official language") so the wizard never has to ask the
// operator to disambiguate at first-run; the picked default is
// always changeable from the admin surface afterwards.
//
// The defaults follow the country's most common business locale.
// Most mappings have a directly shipped translation catalogue
// under internal/i18n/locales; two exceptions exist because the
// most-common-business-locale rule is more useful than strict
// catalogue presence for downstream consumers (e.g. the UI's
// "default language" label):
//
//   - IN → "hi". No hi.json catalogue ships today. The matcher
//     wired through WithLocaleBundle normalises "hi" to "en" at
//     resolve time, so a tenant provisioned with this default
//     still renders the English UI rather than a literal-key
//     fallback. If a Hindi catalogue is added later, the
//     normalisation collapses automatically.
//
//   - CN → "zh-Hans". Only the unscripted zh.json catalogue
//     ships; the matcher normalises "zh-Hans" → "zh". Returning
//     the scripted tag here (rather than bare "zh") is what
//     lets TW / HK keep their separate "zh-Hant" mapping — the
//     two values diverge on the script subtag so the wizard can
//     distinguish them even if a single physical catalogue
//     happens to serve both at runtime.
//
// All other mappings (de / fr / it / es / ja / ar / th / id /
// vi / en / zh-Hant) DO have a shipped catalogue and resolve
// without the normalisation step.
//
// IMPORTANT: this matters only when both resolver and validator
// are wired (production path goes through WithLocaleBundle which
// installs them atomically — see deps_build.go:629).  A caller
// that builds the wizard with WithLocaleValidator(bundle) but
// NOT WithLocaleResolver(bundle) would see CN/IN provisioning
// fail the bundle-whitelist gate.  The WithLocaleBundle helper
// is the canonical wiring path precisely to make that misuse
// unreachable by convention.
//
// A country lacking ANY mapping falls through to "en", so the
// loader always has a concrete bundle to resolve. Adding a new
// mapping does NOT require the catalogue to be present first
// — the resolver+validator atomically normalise any unsupported
// tag to "en" — but adding a catalogue DOES make the mapping
// honoured end-to-end without the resolver fallback.
//
//   - DE / AT / CH: German. CH stays German because Swiss-German
//     is the largest business language; admins in the Romandie or
//     Ticino reset to fr or it from the admin surface.
//   - FR: French. IT: Italian. ES: Spanish. JP: Japanese.
//   - SA / AE / QA / KW / BH / OM: Arabic.
//   - SG / MY / PH: English (lingua franca for business — the
//     local-language bundle exists for ms but English remains the
//     conservative B2B default).
//   - TH: Thai. ID: Indonesian. VN: Vietnamese. IN: Hindi.
//   - NZ: English. CN: zh-Hans. HK / TW: zh-Hant.
//   - US / AU / GB / IE / CA: English.
func DefaultLocaleForCountry(country string) string {
	switch strings.ToUpper(strings.TrimSpace(country)) {
	case "DE", "AT", "CH":
		return "de"
	case "FR":
		return "fr"
	case "IT":
		return "it"
	case "ES":
		return "es"
	case "JP":
		return "ja"
	case "KR":
		return "ko"
	case "SA", "AE", "QA", "KW", "BH", "OM":
		return "ar"
	case "TH":
		return "th"
	case "ID":
		return "id"
	case "VN":
		return "vi"
	case "IN":
		return "hi"
	case "CN":
		return "zh-Hans"
	case "TW", "HK":
		return "zh-Hant"
	case "BR":
		return "pt-BR"
	case "MX", "AR", "CO", "CL", "PE", "CR", "PA", "UY", "EC", "DO", "GT", "PY":
		return "es"
	// Phase N1 — Europe Core. Dutch + Portuguese are new
	// catalogues added in this PR; Belgium defaults to French
	// (Wallonia / Brussels majority business locale, with NL
	// admins resetting from the admin surface).
	case "NL":
		return "nl"
	case "PT":
		return "pt"
	case "BE":
		return "fr"
	// Phase N2 — Europe Extended. Nine additional locales
	// added in N2 (pl, sv, nb, da, fi, cs, hu, ro, el).
	// Each country defaults to its national language.
	case "PL":
		return "pl"
	case "SE":
		return "sv"
	case "NO":
		return "nb"
	case "DK":
		return "da"
	case "FI":
		return "fi"
	case "CZ":
		return "cs"
	case "HU":
		return "hu"
	case "RO":
		return "ro"
	case "GR":
		return "el"
	// SCAFFOLD: cmd/new-tax-pack inserts new DefaultLocaleForCountry cases above this line.
	default:
		return "en"
	}
}

// DefaultCoATemplateForCountry returns the chart-of-accounts template
// name the wizard should pre-fill for the given ISO 3166-1 alpha-2
// country code. The mapping uses the country-specific "<cc>_basic"
// template when one is registered for the country, otherwise falls
// back to the generic IFRS chart ("ifrs_basic") for any registered
// country lacking a specialised chart, and to "us_gaap_basic" for
// US tenants.
//
// Returns "us_gaap_basic" for US, "ifrs_basic" for any country
// without an explicit mapping, and the country-specific chart for
// the 15 jurisdictions with a registered tax pack (SG/MY/TH/ID/VN/
// PH/NZ/IN/CH/AE/SA/QA/KW/BH/OM). Keeping the default chooser inside
// the tenant package means the wizard handler doesn't need to embed
// the list — it just calls this helper.
func DefaultCoATemplateForCountry(country string) string {
	switch strings.ToUpper(strings.TrimSpace(country)) {
	case "US":
		return "us_gaap_basic"
	case "SG":
		return "sg_basic"
	case "MY":
		return "my_basic"
	case "TH":
		return "th_basic"
	case "ID":
		return "id_basic"
	case "VN":
		return "vn_basic"
	case "PH":
		return "ph_basic"
	case "NZ":
		return "nz_basic"
	case "IN":
		return "in_basic"
	case "CH":
		return "ch_basic"
	case "AE":
		return "ae_basic"
	case "SA":
		return "sa_basic"
	case "QA":
		return "qa_basic"
	case "KW":
		return "kw_basic"
	case "BH":
		return "bh_basic"
	case "OM":
		return "om_basic"
	case "CA":
		return "ca_aspe_basic"
	case "BR":
		return "br_cpc_basic"
	case "MX":
		return "mx_nif_basic"
	case "AR":
		return "ar_rtfacpce_basic"
	case "CL":
		return "cl_ifrs_basic"
	case "CO", "PE", "CR", "PA", "UY", "EC", "DO", "GT", "PY", "TT":
		return "latam_ifrs_basic"
	// Phase N1 — Europe Core + AU. Each country gets its own
	// chart so payroll deduction lines land on country-specific
	// liability accounts (HMRC PAYE, DRV / GKV, URSSAF,
	// AEAT IRPF, Agenzia delle Entrate IRPEF, Belastingdienst
	// Loonheffing, ONSS/RSZ Précompte, Revenue PAYE, FA / ÖGK
	// Lohnsteuer / SV, AT IRS / Seg. Social, ATO PAYG /
	// Superannuation).
	case "GB":
		return "gb_basic"
	case "DE":
		return "de_basic"
	case "FR":
		return "fr_basic"
	case "ES":
		return "es_basic"
	case "IT":
		return "it_basic"
	case "NL":
		return "nl_basic"
	case "BE":
		return "be_basic"
	case "IE":
		return "ie_basic"
	case "AT":
		return "at_basic"
	case "PT":
		return "pt_basic"
	case "AU":
		return "au_basic"
	// Phase N2 — Europe Extended (PL/SE/NO/DK/FI/CZ/HU/RO/GR).
	// Each country gets its own chart so payroll deduction
	// lines land on country-specific liability accounts:
	// ZUS / NFZ (PL), Skatteverket / Tjänstepension (SE),
	// Skatteetaten / NAV (NO), Skattestyrelsen / ATP (DK),
	// Verohallinto / TyEL (FI), ČSSZ / VZP (CZ), NAV (HU),
	// ANAF / CAS / CASS (RO), AADE / EFKA (GR).
	case "PL":
		return "pl_basic"
	case "SE":
		return "se_basic"
	case "NO":
		return "no_basic"
	case "DK":
		return "dk_basic"
	case "FI":
		return "fi_basic"
	case "CZ":
		return "cz_basic"
	case "HU":
		return "hu_basic"
	case "RO":
		return "ro_basic"
	case "GR":
		return "gr_basic"
	// Phase N3 — Africa + East Asia (ZA/NG/KE/EG/JP/KR). Each
	// country gets its own chart so payroll deduction lines land
	// on country-specific liability accounts: SARS PAYE / UIF (ZA),
	// FIRS PAYE / PenCom (NG), KRA PAYE / NSSF / NHIF (KE),
	// ETA / Social Insurance (EG), NTA gensenchōshū (JP),
	// NTS / NPS / NHIS (KR).
	case "ZA":
		return "za_basic"
	case "NG":
		return "ng_basic"
	case "KE":
		return "ke_basic"
	case "EG":
		return "eg_basic"
	case "JP":
		return "jp_basic"
	case "KR":
		return "kr_basic"
	// SCAFFOLD: cmd/new-tax-pack inserts new DefaultCoATemplateForCountry cases above this line.
	default:
		return "ifrs_basic"
	}
}

// WizardRole captures a role definition the wizard should upsert into
// the tenant's `roles` table. Permissions are a JSON array of action
// strings or action+resource objects; we pass them through verbatim.
type WizardRole struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Permissions json.RawMessage `json:"permissions"`
	// ParentRole, when set, populates the `parent_role` column on
	// the seeded `roles` row so the authz evaluator inherits this
	// role's permissions through the parent chain (migration
	// 000050). Empty means the role is a hierarchy root. The default
	// chain (owner → tenant.admin → tenant.member, every module role
	// → tenant.member) is encoded in DefaultRoles below.
	ParentRole string `json:"parent_role,omitempty"`
}

// WizardUser is the minimum identifier + role(s) needed to seed an initial
// membership row in `user_tenants`. If the user does not exist in
// `users` yet, the wizard will create a stub by email.
//
// Role is the legacy single-role field — retained so existing callers
// keep working — and is mirrored into both user_tenants.role and the new
// user_tenant_roles junction. Roles is the multi-role list; when supplied
// every entry is inserted into user_tenant_roles, and the first non-empty
// entry is used as the primary user_tenants.role for backwards
// compatibility with code paths that still read a single role.
type WizardUser struct {
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name,omitempty"`
	Role        string   `json:"role,omitempty"`
	Roles       []string `json:"roles,omitempty"`
}

// WizardResult summarises the side-effects the wizard applied. The HTTP
// handler surfaces this so the UI can render a completion screen.
//
// LocaleUsed reports the resolved locale tag the wizard wrote to the
// `tenants.locale` column. When the caller supplied an explicit locale
// in cfg.Locale this is the same value; when the caller omitted it,
// LocaleUsed reflects DefaultLocaleForCountry(cfg.Country) after the
// resolver downgrade (so e.g. an IN tenant whose country-derived
// default is `hi` lands on `en` if the bundle doesn't ship `hi.json`).
// Surfacing this lets the wizard UI show the user which locale was
// actually persisted vs. what they asked for, especially after a
// resolver downgrade.
type WizardResult struct {
	TenantID            uuid.UUID `json:"tenant_id"`
	AccountsInserted    int       `json:"accounts_inserted"`
	RolesInserted       int       `json:"roles_inserted"`
	UsersInserted       int       `json:"users_inserted"`
	CoATemplateUsed     string    `json:"coa_template_used"`
	LocaleUsed          string    `json:"locale_used"`
	ZKFabricProvisioned bool      `json:"zk_fabric_provisioned,omitempty"`
}

// ---------------------------------------------------------------------------
// Embedded chart-of-accounts templates.
// ---------------------------------------------------------------------------

//go:embed coa_templates/us_gaap_basic.json
var coaUSGAAPBasic []byte

//go:embed coa_templates/ifrs_basic.json
var coaIFRSBasic []byte

// Country-specific CoA templates align with the tax packs registered
// in internal/hr/taxpacks. Each template encodes the country's
// statutory chart: local-currency cash, country-specific VAT/GST
// receivable/payable, and payroll liability accounts that match the
// Code field on every Deduction emitted by the country's TaxPack
// (e.g. sg_basic includes CPF Payable to receive SG_CPF_EMPLOYEE
// debits, ae_basic includes GPSSA Payable, ch_basic includes
// AHV/ALV/BVG/Quellensteuer/Cantonal split, etc.). End-of-service
// liability accounts are pre-seeded for jurisdictions where the
// employer accrues gratuity / severance (UAE, SA, QA, KW, BH, OM,
// VN, ID, IN, TH, PH).

//go:embed coa_templates/sg_basic.json
var coaSGBasic []byte

//go:embed coa_templates/my_basic.json
var coaMYBasic []byte

//go:embed coa_templates/th_basic.json
var coaTHBasic []byte

//go:embed coa_templates/id_basic.json
var coaIDBasic []byte

//go:embed coa_templates/vn_basic.json
var coaVNBasic []byte

//go:embed coa_templates/ph_basic.json
var coaPHBasic []byte

//go:embed coa_templates/nz_basic.json
var coaNZBasic []byte

//go:embed coa_templates/in_basic.json
var coaINBasic []byte

//go:embed coa_templates/ch_basic.json
var coaCHBasic []byte

//go:embed coa_templates/ae_basic.json
var coaAEBasic []byte

//go:embed coa_templates/sa_basic.json
var coaSABasic []byte

//go:embed coa_templates/qa_basic.json
var coaQABasic []byte

//go:embed coa_templates/kw_basic.json
var coaKWBasic []byte

//go:embed coa_templates/bh_basic.json
var coaBHBasic []byte

//go:embed coa_templates/om_basic.json
var coaOMBasic []byte

//go:embed coa_templates/ca_aspe_basic.json
var coaCAASPEBasic []byte

//go:embed coa_templates/br_cpc_basic.json
var coaBRCPCBasic []byte

//go:embed coa_templates/mx_nif_basic.json
var coaMXNIFBasic []byte

//go:embed coa_templates/ar_rtfacpce_basic.json
var coaARRTBasic []byte

//go:embed coa_templates/cl_ifrs_basic.json
var coaCLIFRSBasic []byte

//go:embed coa_templates/latam_ifrs_basic.json
var coaLATAMIFRSBasic []byte

// Phase N1 — Europe Core (GB / DE / FR / ES / IT / NL / BE / IE /
// AT / PT) and the Australia chart that closes the previously-
// documented fallback to ifrs_basic for AU tenants.

//go:embed coa_templates/gb_basic.json
var coaGBBasic []byte

//go:embed coa_templates/de_basic.json
var coaDEBasic []byte

//go:embed coa_templates/fr_basic.json
var coaFRBasic []byte

//go:embed coa_templates/es_basic.json
var coaESBasic []byte

//go:embed coa_templates/it_basic.json
var coaITBasic []byte

//go:embed coa_templates/nl_basic.json
var coaNLBasic []byte

//go:embed coa_templates/be_basic.json
var coaBEBasic []byte

//go:embed coa_templates/ie_basic.json
var coaIEBasic []byte

//go:embed coa_templates/at_basic.json
var coaATBasic []byte

//go:embed coa_templates/pt_basic.json
var coaPTBasic []byte

//go:embed coa_templates/au_basic.json
var coaAUBasic []byte

// Phase N2 — Europe Extended (PL/SE/NO/DK/FI/CZ/HU/RO/GR).
// Each chart carries the country's statutory payroll-liability
// accounts so deductions emitted by the matching tax pack land
// on the right ledger lines (ZUS/NFZ for PL, Skatteverket /
// Tjänstepension for SE, Skatteetaten/NAV/OTP for NO,
// Skattestyrelsen/ATP for DK, Verohallinto/TyEL/SAVA for FI,
// ČSSZ/VZP for CZ, NAV/Szocho for HU, ANAF/CAS/CASS/CAM for RO,
// AADE/EFKA for GR).

//go:embed coa_templates/pl_basic.json
var coaPLBasic []byte

//go:embed coa_templates/se_basic.json
var coaSEBasic []byte

//go:embed coa_templates/no_basic.json
var coaNOBasic []byte

//go:embed coa_templates/dk_basic.json
var coaDKBasic []byte

//go:embed coa_templates/fi_basic.json
var coaFIBasic []byte

//go:embed coa_templates/cz_basic.json
var coaCZBasic []byte

//go:embed coa_templates/hu_basic.json
var coaHUBasic []byte

//go:embed coa_templates/ro_basic.json
var coaROBasic []byte

//go:embed coa_templates/gr_basic.json
var coaGRBasic []byte

// Phase N3 — Africa + East Asia (ZA/NG/KE/EG/JP/KR). Each chart
// carries the country's statutory payroll-liability accounts so
// deductions emitted by the matching tax pack land on the right
// ledger lines (SARS PAYE/UIF/SDL for ZA, FIRS PAYE/PenCom for NG,
// KRA PAYE/NSSF/NHIF for KE, ETA/Social Insurance for EG,
// NTA gensenchōshū for JP, NTS/NPS/NHIS for KR).

//go:embed coa_templates/za_basic.json
var coaZABasic []byte

//go:embed coa_templates/ng_basic.json
var coaNGBasic []byte

//go:embed coa_templates/ke_basic.json
var coaKEBasic []byte

//go:embed coa_templates/eg_basic.json
var coaEGBasic []byte

//go:embed coa_templates/jp_basic.json
var coaJPBasic []byte

//go:embed coa_templates/kr_basic.json
var coaKRBasic []byte

// SCAFFOLD: cmd/new-tax-pack inserts new //go:embed directives + var decls above this line.

// chartOfAccountsTemplates maps the wizard's template name to the
// embedded JSON payload. Adding a new template is a matter of dropping
// a JSON file in coa_templates/ and registering it here. Country-
// specific templates use the naming convention
// "<iso-3166-1-alpha-2>_basic" so the wizard / UI can map a tenant's
// chosen Country to a sensible default chart in PR-7
// (DefaultCoATemplateForCountry).
var chartOfAccountsTemplates = map[string][]byte{
	"us_gaap_basic": coaUSGAAPBasic,
	"ifrs_basic":    coaIFRSBasic,
	"sg_basic":      coaSGBasic,
	"my_basic":      coaMYBasic,
	"th_basic":      coaTHBasic,
	"id_basic":      coaIDBasic,
	"vn_basic":      coaVNBasic,
	"ph_basic":      coaPHBasic,
	"nz_basic":      coaNZBasic,
	"in_basic":      coaINBasic,
	"ch_basic":      coaCHBasic,
	"ae_basic":      coaAEBasic,
	"sa_basic":      coaSABasic,
	"qa_basic":      coaQABasic,
	"kw_basic":      coaKWBasic,
	"bh_basic":      coaBHBasic,
	"om_basic":      coaOMBasic,
	// Americas: CA + LATAM (PR-2d).
	"ca_aspe_basic":     coaCAASPEBasic,
	"br_cpc_basic":      coaBRCPCBasic,
	"mx_nif_basic":      coaMXNIFBasic,
	"ar_rtfacpce_basic": coaARRTBasic,
	"cl_ifrs_basic":     coaCLIFRSBasic,
	"latam_ifrs_basic":  coaLATAMIFRSBasic,
	// Phase N1 — Europe Core + AU.
	"gb_basic": coaGBBasic,
	"de_basic": coaDEBasic,
	"fr_basic": coaFRBasic,
	"es_basic": coaESBasic,
	"it_basic": coaITBasic,
	"nl_basic": coaNLBasic,
	"be_basic": coaBEBasic,
	"ie_basic": coaIEBasic,
	"at_basic": coaATBasic,
	"pt_basic": coaPTBasic,
	"au_basic": coaAUBasic,
	// Phase N2 — Europe Extended.
	"pl_basic": coaPLBasic,
	"se_basic": coaSEBasic,
	"no_basic": coaNOBasic,
	"dk_basic": coaDKBasic,
	"fi_basic": coaFIBasic,
	"cz_basic": coaCZBasic,
	"hu_basic": coaHUBasic,
	"ro_basic": coaROBasic,
	"gr_basic": coaGRBasic,
	// Phase N3 — Africa + East Asia.
	"za_basic": coaZABasic,
	"ng_basic": coaNGBasic,
	"ke_basic": coaKEBasic,
	"eg_basic": coaEGBasic,
	"jp_basic": coaJPBasic,
	"kr_basic": coaKRBasic,
	// SCAFFOLD: cmd/new-tax-pack inserts new chartOfAccountsTemplates entries above this line.
}

// templateAccount is the shape each entry in a CoA template takes. The
// chart schema mirrors the accounts table columns in
// migrations/000001_initial_schema.sql.
type templateAccount struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ParentCode string `json:"parent_code,omitempty"`
	Active     *bool  `json:"active,omitempty"`
}

// DefaultRoles is the canonical role set the wizard seeds when the
// caller does not supply their own roles list. The permission arrays
// mirror the "role packs" discussed in ARCHITECTURE.md §6.
//
// Phase RBAC expanded the set so KType schemas that reference
// granular roles (e.g. crm.rep, helpdesk.agent) resolve to a real
// definition the wizard inserts. The hierarchy migration
// (000050_role_hierarchy.sql) wires every module role to inherit
// tenant.member so adding a baseline permission lifts the floor for
// the whole set without editing each role.
func DefaultRoles() []WizardRole {
	return []WizardRole{
		{Name: "owner", Description: "Tenant owner", Permissions: json.RawMessage(`["*"]`), ParentRole: "tenant.admin"},
		{Name: "tenant.admin", Description: "Tenant administrator", Permissions: json.RawMessage(`["tenant.admin","tenant.member","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "tenant.member", Description: "Standard member", Permissions: json.RawMessage(`["tenant.member","krecord.read"]`)},
		{Name: "finance.admin", Description: "Finance administrator", Permissions: json.RawMessage(`["tenant.member","finance.*","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "hr.admin", Description: "HR administrator", Permissions: json.RawMessage(`["tenant.member","hr.*","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "lms.admin", Description: "LMS administrator", Permissions: json.RawMessage(`["tenant.member","lms.*","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "crm.rep", Description: "CRM sales representative", Permissions: json.RawMessage(`["tenant.member","crm.*","krecord.read","krecord.write"]`), ParentRole: "tenant.member"},
		{Name: "crm.manager", Description: "CRM manager", Permissions: json.RawMessage(`["tenant.member","crm.*","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "inventory.admin", Description: "Inventory administrator", Permissions: json.RawMessage(`["tenant.member","inventory.*","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "helpdesk.agent", Description: "Helpdesk agent", Permissions: json.RawMessage(`["tenant.member","helpdesk.ticket.*","krecord.read","krecord.write"]`), ParentRole: "tenant.member"},
		{Name: "helpdesk.manager", Description: "Helpdesk manager", Permissions: json.RawMessage(`["tenant.member","helpdesk.*","krecord.*"]`), ParentRole: "tenant.member"},
		{Name: "sales.rep", Description: "Sales representative", Permissions: json.RawMessage(`["tenant.member","sales.*","krecord.read","krecord.write"]`), ParentRole: "tenant.member"},
		{Name: "procurement.rep", Description: "Procurement representative", Permissions: json.RawMessage(`["tenant.member","procurement.*","krecord.read","krecord.write"]`), ParentRole: "tenant.member"},
		{Name: "reporting.viewer", Description: "Read-only report viewer", Permissions: json.RawMessage(`["tenant.member","krecord.read","reports.read","insights.read"]`), ParentRole: "tenant.member"},
	}
}

// ---------------------------------------------------------------------------
// Wizard
// ---------------------------------------------------------------------------

// Wizard encapsulates the setup flow so the HTTP handler can drive
// `RunSetupWizard` against the live pool while tests can substitute a
// fake.
//
// The `accounts`, `roles`, and `user_tenants` tables are all
// RLS-protected (migrations/000001_initial_schema.sql). Under the
// production `kapp_app` role (migrations/000002_admin_role.sql) every
// INSERT/UPDATE must execute inside a transaction that has
// `app.tenant_id` set — otherwise the RLS WITH CHECK clause rejects
// the write. So every seed step here runs inside
// `dbutil.WithTenantTx`, which issues `SELECT set_config('app.tenant_id', …, true)`
// on the tx before calling the closure.
type Wizard struct {
	pool            *pgxpool.Pool
	store           *PGStore
	zkProvisioner   ZKFabricProvisioner
	placementSource PlacementPolicySource
	// localeValidator gates writes to tenants.locale through
	// ValidateLocale. nil means "format gate only" (the boot path
	// before the i18n bundle is wired in, and most unit tests).
	// In production the wizard binds *i18n.Bundle here so an
	// operator picking a tag the runtime can't serve fails fast
	// at provisioning time rather than at first /api/v1/locales
	// fetch.
	localeValidator LocaleValidator
	// localeResolver normalises a country-derived default to the
	// best supported catalogue (DefaultLocaleForCountry can return
	// "hi" for IN, but the shipped bundle whitelist may only know
	// "en" — the resolver downgrades cleanly without rejecting
	// the row). Nil leaves the wizard's derived tag unchanged.
	localeResolver LocaleResolver
	// geoIP is the optional IP→country detector SmartDefaults
	// consults as the last-resort country source. Nil skips the
	// geo step (see WithGeoIPResolver / resolveCountry).
	geoIP GeoIPResolver
}

// ZKFabricProvisioner mints a new tenant + HMAC credential pair on
// the ZK Object Fabric console and returns the bucket / access /
// secret triple that should be persisted on the tenants row. The
// wizard calls it once during RunSetupWizard so per-tenant ZK
// encryption is wired by the time the tenant logs in for the first
// time. A nil provisioner skips the step (legacy MinIO path stays
// in place).
//
// ProvisionTenantWithPolicy is the policy-aware variant the wizard
// uses by default: the wizard derives a plan-appropriate policy via
// DerivePlacementPolicy and threads it through so each new tenant
// lands on the fabric with provider/country/cache hints already in
// place. Implementations that don't support policy management can
// just delegate to ProvisionTenant and ignore the policy argument.
type ZKFabricProvisioner interface {
	ProvisionTenant(ctx context.Context, tenantID uuid.UUID, slug string) (ZKCredentials, error)
	ProvisionTenantWithPolicy(ctx context.Context, tenantID uuid.UUID, slug, plan string, policy PlacementPolicy) (ZKCredentials, error)
}

// PlacementPolicySource lets the wizard pull platform-wide defaults
// (provider allow-list, default cache hint) from a single place
// without taking a hard dependency on env handling. The default
// implementation reads ZK_FABRIC_PROVIDERS / ZK_FABRIC_CACHE_HINT.
type PlacementPolicySource interface {
	DefaultProviders() []string
	DefaultCacheHint() string
}

// NewWizard binds the wizard to the shared pool. ZK fabric
// provisioning is opt-in via WithZKFabricProvisioner so existing
// deployments without a fabric gateway keep working.
func NewWizard(pool *pgxpool.Pool) *Wizard {
	return &Wizard{pool: pool, store: NewPGStore(pool)}
}

// WithZKFabricProvisioner attaches a ZK fabric provisioner to the
// wizard. Returns the wizard for fluent chaining.
func (w *Wizard) WithZKFabricProvisioner(p ZKFabricProvisioner) *Wizard {
	w.zkProvisioner = p
	return w
}

// WithPlacementPolicySource attaches a default-policy source. Without
// one, the wizard derives a policy with no platform-wide overrides
// (a single "wasabi" provider and no cache hint).
func (w *Wizard) WithPlacementPolicySource(s PlacementPolicySource) *Wizard {
	w.placementSource = s
	return w
}

// WithLocaleValidator attaches the runtime locale-bundle whitelist
// the wizard consults when persisting tenants.locale. Production
// wires *i18n.Bundle here so an operator-supplied tag the runtime
// can't serve is rejected at provisioning time. Passing nil is
// equivalent to never calling the setter (format gate only).
func (w *Wizard) WithLocaleValidator(v LocaleValidator) *Wizard {
	w.localeValidator = v
	return w
}

// WithLocaleResolver attaches the matcher used to normalise a
// country-derived default locale tag. When the caller doesn't
// supply cfg.Locale and DefaultLocaleForCountry returns a tag the
// validator wouldn't accept (e.g. "hi" for IN with no hi.json
// shipped), the resolver downgrades it to the nearest supported
// catalogue before the bundle-whitelist gate runs. *i18n.Bundle
// satisfies both this interface and LocaleValidator so callers
// typically pass the same value to both setters — prefer
// WithLocaleBundle for that case.
func (w *Wizard) WithLocaleResolver(r LocaleResolver) *Wizard {
	w.localeResolver = r
	return w
}

// LocaleBundle is the combined interface satisfied by *i18n.Bundle.
// Production wiring always has a single value that gates both the
// whitelist check and the matcher downgrade, so the wizard's primary
// setter takes one argument rather than two — passing the same
// bundle into WithLocaleValidator and WithLocaleResolver separately
// is a latent footgun (forgetting the resolver call leaves the wizard
// rejecting every IN/CN/TW/HK row even though it would have shipped
// fine). The two single-interface setters remain available for
// unit tests that want to exercise just one half of the contract
// against a tiny in-memory stub.
type LocaleBundle interface {
	LocaleValidator
	LocaleResolver
}

// WithLocaleBundle wires the runtime translation bundle as both the
// whitelist gate and the matcher downgrade source. This is the
// supported production wiring; the deps_build path wires
// *i18n.Bundle here so a future contributor cannot accidentally
// install a validator without the matching resolver. Passing nil
// — or, equivalently, a typed-nil interface wrapping a nil pointer
// such as `(*i18n.Bundle)(nil)` — detaches both fields rather than
// installing a non-nil interface that would panic at the first
// Resolve / IsSupported call. The typed-nil detection uses reflect
// (which costs one allocation at wizard construction time, off the
// per-tenant hot path) so we close the classic Go interface-nil
// footgun for every caller that might wire the bundle through a
// variable that turned out to be nil at runtime.
func (w *Wizard) WithLocaleBundle(b LocaleBundle) *Wizard {
	if isNilBundle(b) {
		w.localeValidator = nil
		w.localeResolver = nil
		return w
	}
	w.localeValidator = b
	w.localeResolver = b
	return w
}

// isNilBundle returns true for both the untyped nil interface and
// the typed-nil case (interface non-nil, but wrapping a nil pointer
// like `(*i18n.Bundle)(nil)`). Only reflect can distinguish the
// latter from a valid bundle, because `b == nil` only matches the
// fully untyped form. Kept package-private and minimal — the only
// kinds we expect for LocaleBundle implementations are pointers
// (i18n.Bundle is *Bundle) and interfaces; non-pointer
// implementations sail past the check unchanged because reflect
// cannot meaningfully test them for nil.
func isNilBundle(b LocaleBundle) bool {
	if b == nil {
		return true
	}
	v := reflect.ValueOf(b)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}

// RunSetupWizard applies the supplied config to an existing tenant.
// Account seeding and role seeding share one tenant-scoped tx so a
// failure halfway through rolls both back. User seeding runs in a
// follow-up tenant-scoped tx since the control-plane user upsert on
// `users` (not RLS-gated) is independent of the `user_tenants` write
// (RLS-gated), and we want the `user_tenants` INSERT under the tenant
// GUC regardless.
//
// Default CoA resolution (PR-3 change): when cfg.CoATemplate is empty
// the wizard now resolves a chart from cfg.Country via
// DefaultCoATemplateForCountry. The previous behavior hardcoded
// "us_gaap_basic" for empty inputs; callers that relied on US GAAP
// being the implicit default now resolve to "ifrs_basic" when both
// CoATemplate and Country are empty. Existing callers that pass an
// explicit Country are unaffected — "US" still maps to
// "us_gaap_basic", every country with a registered tax pack maps to
// its country-specific chart, and unmapped countries fall back to
// the generic IFRS chart.
func (w *Wizard) RunSetupWizard(ctx context.Context, tenantID uuid.UUID, cfg SetupWizardConfig) (*WizardResult, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: wizard requires tenant id")
	}
	if cfg.CompanyName == "" {
		return nil, errors.New("tenant: wizard requires company_name")
	}
	// When the caller didn't pre-pick a CoA template the wizard
	// resolves one from the tenant's Country so a SG tenant gets
	// sg_basic (CPF / GST liability accounts), an AE tenant gets
	// ae_basic (GPSSA / Gratuity), etc. DefaultCoATemplateForCountry
	// falls back to us_gaap_basic for US, ifrs_basic for any country
	// without a country-specific chart, and is exhaustively pinned by
	// TestDefaultCoATemplateForCountry.
	templateName := cfg.CoATemplate
	if templateName == "" {
		templateName = DefaultCoATemplateForCountry(cfg.Country)
	}
	accounts, err := loadTemplate(templateName)
	if err != nil {
		return nil, err
	}
	roles := cfg.Roles
	if len(roles) == 0 {
		roles = DefaultRoles()
	}

	out := &WizardResult{TenantID: tenantID, CoATemplateUsed: templateName}

	if err := dbutil.WithTenantTx(ctx, w.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accountsInserted, err := seedAccounts(ctx, tx, tenantID, accounts)
		if err != nil {
			return err
		}
		out.AccountsInserted = accountsInserted

		rolesInserted, err := seedRoles(ctx, tx, tenantID, roles)
		if err != nil {
			return err
		}
		out.RolesInserted = rolesInserted

		// Persist the tenant's functional currency before any
		// finance seeders run so PostJournalEntry can detect
		// foreign-currency lines for the very first invoice. The
		// 3-letter check matches CHECK on tenants.base_currency.
		if cfg.CurrencyCode != "" {
			if len(cfg.CurrencyCode) != 3 {
				return fmt.Errorf("tenant: wizard: currency_code must be ISO-4217 (got %q)", cfg.CurrencyCode)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE tenants SET base_currency = $1, updated_at = now() WHERE id = $2`,
				cfg.CurrencyCode, tenantID,
			); err != nil {
				return fmt.Errorf("tenant: persist base currency: %w", err)
			}
		}

		// Persist the tenant's statutory country so the payroll
		// engine can resolve a per-country tax pack at slip
		// generation. Stored as ISO 3166-1 alpha-2 to match the
		// wizard payload; longer values fail closed rather than
		// silently truncating.
		if cfg.Country != "" {
			if len(cfg.Country) != 2 {
				return fmt.Errorf("tenant: wizard: country must be ISO 3166-1 alpha-2 (got %q)", cfg.Country)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE tenants SET country = $1, updated_at = now() WHERE id = $2`,
				cfg.Country, tenantID,
			); err != nil {
				return fmt.Errorf("tenant: persist country: %w", err)
			}
		}

		// Persist the tenant's UI locale. When the caller did not
		// supply one we derive it from cfg.Country so a "Swiss
		// company" lands on German, a "Saudi company" lands on
		// Arabic, etc. — DefaultLocaleForCountry returns "en" for
		// unmapped countries so the column never gets an empty
		// string. The CHECK on migration 000059 enforces the format
		// regardless of which API path writes the row.
		//
		// Cache-invalidation note: like the base_currency and
		// country writes above, this goes through tx.Exec rather
		// than PGStore.SetLocale so all three columns commit
		// atomically alongside the wizard's account / role
		// seeding. The wizard's own w.store is uncached
		// (NewPGStore, no WithCache), so its own re-read is
		// always consistent. Any *other* PGStore instance that
		// has cached this tenant and was created with WithCache
		// will continue returning the pre-wizard Locale until
		// its TTL (single-digit seconds in production) expires.
		// That cross-instance staleness is consistent with how
		// the SetCountry / SetBaseCurrency paths behave through
		// the wizard today; a real fix would route every tenant
		// mutation through a NOTIFY/LISTEN channel and have all
		// caches subscribe, which is out of scope for the i18n
		// foundation and tracked separately.
		locale := strings.TrimSpace(cfg.Locale)
		operatorSupplied := locale != ""
		if !operatorSupplied {
			locale = DefaultLocaleForCountry(cfg.Country)
			// Country-derived defaults are downgraded through
			// the matcher when a resolver is wired (production
			// path via *i18n.Bundle). This is what makes a tag
			// DefaultLocaleForCountry returns but the bundle
			// can't serve (e.g. "hi" for IN today) collapse to
			// the best supported catalogue rather than being
			// rejected by the whitelist gate below.
			//
			// Operator-supplied locales bypass this branch on
			// purpose: an explicit user pick that the runtime
			// can't serve must fail loudly so the operator is
			// told which tag is available, not silently
			// downgraded to a language they didn't choose.
			if w.localeResolver != nil {
				locale = w.localeResolver.Resolve(locale)
			}
		}
		// Format gate runs unconditionally; the bundle-whitelist
		// gate (validator-driven) is the runtime check that
		// matches the catalogues the API can actually serve. A
		// nil validator preserves the boot-time behaviour for
		// callers that build the wizard before the i18n bundle
		// is available (CLI tools, integration tests).
		if err := ValidateLocale(locale, w.localeValidator); err != nil {
			return fmt.Errorf("tenant: wizard: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tenants SET locale = $1, updated_at = now() WHERE id = $2`,
			locale, tenantID,
		); err != nil {
			return fmt.Errorf("tenant: persist locale: %w", err)
		}
		// Stamp the resolved locale on the result so the wizard UI
		// can render "Locale used: <tag>" on the completion screen.
		// The assignment lives inside the tx closure for consistency
		// with the other out.* fields (AccountsInserted line 578,
		// RolesInserted line 584). The struct mutation itself is
		// NOT undone by a tx rollback — Go heap state is not
		// transactional — but the WithTenantTx wrapper returns the
		// closure's error to the outer function, which surfaces it
		// via "return nil, fmt.Errorf(...)" at line 715. The caller
		// therefore receives (nil, err) and never observes a
		// partially-populated *WizardResult on a rollback path.
		out.LocaleUsed = locale

		// Seed the default per-tenant scheduled_actions rows the
		// worker handlers expect (SLA breach sweeper +
		// recurring-invoice generator). Idempotent on
		// (tenant_id, action_type) so a re-imported tenant never
		// duplicates queue rows. The interval defaults match the
		// values asserted by the integration drift tests in
		// internal/integrationtest/{sla_breach_test,recurring_invoice_test}.go.
		if err := seedDefaultScheduledActions(ctx, tx, tenantID, cfg.Plan); err != nil {
			return err
		}
		// Seed plan-appropriate feature flags. Free plan tenants
		// land on CRM-only; paid tiers unlock the rest. Uses
		// ON CONFLICT DO NOTHING so a re-run of the wizard never
		// overwrites operator-applied overrides.
		if err := seedDefaultFeatures(ctx, tx, tenantID, cfg.Plan); err != nil {
			return err
		}
		// Seed plan-appropriate retention policies. The retention
		// sweeper scheduled action only matters if data_retention_policies
		// has rows for this tenant — without policies the sweeper
		// is a no-op. Free plans get aggressive 90d windows;
		// enterprise gets 365d on the audit_log so compliance
		// inspections can run a year back.
		if err := seedDefaultRetentionPolicies(ctx, tx, tenantID, cfg.Plan); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tenant: wizard seed accounts/roles: %w", err)
	}

	if len(cfg.Users) > 0 {
		usersInserted, err := seedUsers(ctx, w.pool, tenantID, cfg.Users)
		if err != nil {
			return out, err
		}
		out.UsersInserted = usersInserted
	}

	// ZK Object Fabric provisioning runs after the tx so a failure
	// here does not roll back the seeded accounts/roles. The
	// fabric is an external dependency; we'd rather have the
	// tenant ready to use without ZK encryption (operator can
	// re-run provisioning later) than block setup on a fabric
	// outage. Failures are logged via the returned error so the
	// caller surfaces them in the wizard response.
	if w.zkProvisioner != nil && w.store != nil {
		t, err := w.store.Get(ctx, tenantID)
		if err != nil {
			return out, fmt.Errorf("tenant: wizard load tenant for zk provisioning: %w", err)
		}
		if !t.HasZKFabric() {
			policyCfg := PlacementPolicyConfig{
				Plan:    cfg.Plan,
				Country: cfg.Country,
			}
			if w.placementSource != nil {
				policyCfg.DefaultProviders = w.placementSource.DefaultProviders()
				policyCfg.DefaultCacheHint = w.placementSource.DefaultCacheHint()
			}
			policy := DerivePlacementPolicy(policyCfg)
			creds, err := w.zkProvisioner.ProvisionTenantWithPolicy(ctx, tenantID, t.Slug, cfg.Plan, policy)
			if err != nil {
				return out, fmt.Errorf("tenant: wizard zk fabric provision: %w", err)
			}
			if err := w.store.SetZKCredentials(ctx, tenantID, creds.AccessKey, creds.SecretKey, creds.Bucket); err != nil {
				return out, fmt.Errorf("tenant: wizard persist zk credentials: %w", err)
			}
			policy.Tenant = tenantID.String()
			policy.Bucket = creds.Bucket
			if err := w.store.SetPlacementPolicy(ctx, tenantID, policy); err != nil {
				return out, fmt.Errorf("tenant: wizard persist placement policy: %w", err)
			}
			out.ZKFabricProvisioned = true
		}
	}
	return out, nil
}

// loadTemplate returns the parsed CoA for the named template. Unknown
// templates are surfaced as a 4xx via the sentinel error wrap.
func loadTemplate(name string) ([]templateAccount, error) {
	raw, ok := chartOfAccountsTemplates[name]
	if !ok {
		return nil, fmt.Errorf("tenant: unknown coa template %q", name)
	}
	var accounts []templateAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		return nil, fmt.Errorf("tenant: decode coa template %s: %w", name, err)
	}
	return accounts, nil
}

func seedAccounts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, accounts []templateAccount) (int, error) {
	inserted := 0
	for _, a := range accounts {
		active := true
		if a.Active != nil {
			active = *a.Active
		}
		var parent any
		if a.ParentCode != "" {
			parent = a.ParentCode
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO accounts (tenant_id, code, name, type, parent_code, active)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, code) DO NOTHING`,
			tenantID, a.Code, a.Name, a.Type, parent, active,
		)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed account %s: %w", a.Code, err)
		}
		inserted++
	}
	return inserted, nil
}

func seedRoles(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, roles []WizardRole) (int, error) {
	inserted := 0
	for _, r := range roles {
		if r.Name == "" {
			continue
		}
		perms := r.Permissions
		if len(perms) == 0 {
			perms = json.RawMessage(`[]`)
		}
		// Side-fix: the `roles` table (migrations/000001) does not
		// carry a `description` column — the original wizard INSERT
		// referenced one, which made every first-run seed fail with
		// a 42703 once a test finally exercised this path (Task 4).
		// WizardRole.Description is still accepted on the API and
		// preserved in the struct; it is simply not persisted. A
		// follow-up migration can restore storage if the column is
		// ever wanted.
		var parent any
		if r.ParentRole != "" {
			parent = r.ParentRole
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO roles (tenant_id, name, permissions, parent_role)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, name) DO NOTHING`,
			tenantID, r.Name, perms, parent,
		)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed role %s: %w", r.Name, err)
		}
		inserted++
	}
	return inserted, nil
}

// seedUsers upserts into `users` on the control-plane pool (no RLS on
// that table) and then INSERTs into `user_tenants` under a
// tenant-scoped tx so the RLS WITH CHECK clause on `user_tenants`
// (migrations/000001_initial_schema.sql) is satisfied under
// `kapp_app`.
func seedUsers(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, users []WizardUser) (int, error) {
	inserted := 0
	for _, u := range users {
		if u.Email == "" {
			continue
		}
		// Resolve the multi-role list. The legacy Role field is
		// always honoured and merged in so callers that only set
		// Role keep working unchanged.
		roleSet := make(map[string]struct{}, len(u.Roles)+1)
		ordered := make([]string, 0, len(u.Roles)+1)
		add := func(name string) {
			name = strings.TrimSpace(name)
			if name == "" {
				return
			}
			if _, dup := roleSet[name]; dup {
				return
			}
			roleSet[name] = struct{}{}
			ordered = append(ordered, name)
		}
		add(u.Role)
		for _, r := range u.Roles {
			add(r)
		}
		if len(ordered) == 0 {
			continue
		}
		primary := ordered[0]

		var userID uuid.UUID
		err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (email) DO UPDATE SET display_name = COALESCE(EXCLUDED.display_name, users.display_name)
			 RETURNING id`,
			uuid.New(), u.Email, u.DisplayName,
		).Scan(&userID)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed user %s: %w", u.Email, err)
		}
		if err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`INSERT INTO user_tenants (tenant_id, user_id, role, status)
				 VALUES ($1, $2, $3, 'active')
				 ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role, status = 'active'`,
				tenantID, userID, primary,
			); err != nil {
				return err
			}
			for _, r := range ordered {
				if _, err := tx.Exec(ctx,
					`INSERT INTO user_tenant_roles (tenant_id, user_id, role_name)
					 VALUES ($1, $2, $3)
					 ON CONFLICT DO NOTHING`,
					tenantID, userID, r,
				); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return inserted, fmt.Errorf("tenant: seed user_tenants %s: %w", u.Email, err)
		}
		inserted++
	}
	return inserted, nil
}

// Default scheduled-action constants. Kept local — duplicating the
// strings here avoids a tenant → finance import cycle (finance
// already depends on internal/scheduler which depends on internal/
// platform which the wizard reaches indirectly through dbutil).
// The drift-check integration test
// (internal/integrationtest/recurring_invoice_test.go::TestSetupWizardSeedsRecurringInvoiceAction)
// asserts both sides stay in lock-step.
const (
	defaultRecurringInvoiceActionType      = "recurring_invoice"
	defaultRecurringInvoiceIntervalSeconds = 3600
)

// defaultInventoryReorderActionType mirrors inventory.ActionTypeReorder.
// Duplicated for the same cycle reason defaultSLABreachActionType is.
// The hourly cadence matches the finance recurring-invoice sweeper:
// row-level eligibility is gated on the item's reorder_level so a run
// more often than once per day costs only SQL filter passes.
const (
	defaultInventoryReorderActionType      = "inventory_reorder"
	defaultInventoryReorderIntervalSeconds = 3600
)

// defaultUsageSnapshotIntervalSeconds is the cadence at which the
// daily storage_bytes / krecord_count snapshot fires per tenant.
// 24h matches the public/PROGRESS.md commitment that the usage
// dashboard reflects yesterday's footprint within one day.
const defaultUsageSnapshotIntervalSeconds = 86400

// defaultRecordCountRecountActionType / IntervalSeconds drive the
// daily reconciliation of tenant_record_counts against the actual
// krecords row count. The bump path in internal/record/store.go keeps
// the counter accurate per-transaction; this handler exists to detect
// drift caused by direct-SQL repair scripts, future writers that
// bypass the store, or a regression in the bump itself. 24h is the
// shortest cadence that does not add meaningful background load — the
// reconciliation is a single COUNT(*) + UPSERT per tenant and the
// daily storage_bytes / krecord_count snapshot already does exactly
// the same work for telemetry, so the recount adds ~one duplicate
// scan per tenant per day in exchange for a strict drift guarantee.
// Action type is duplicated here (not imported from internal/platform)
// for the same package-cycle reason every other constant in this
// block is — internal/tenant must not depend on internal/platform.
const (
	defaultRecordCountRecountActionType      = "tenant_record_count_recount"
	defaultRecordCountRecountIntervalSeconds = 86400
)

// defaultUnrealizedFXActionType / defaultUnrealizedFXIntervalSeconds
// define the monthly cadence (~30d) at which the worker re-values
// open AR/AP foreign-currency balances per tenant. Seeded only when
// the tenant's plan includes the finance feature.
const (
	defaultUnrealizedFXActionType      = "unrealized_gain_loss"
	defaultUnrealizedFXIntervalSeconds = 30 * 86400
)

// defaultBudgetVarianceActionType / defaultBudgetVarianceIntervalSeconds
// drive the daily budget vs actual variance sweeper. The handler
// reads every active budget for the tenant, computes MTD variance
// against the current calendar month, and raises a notification when
// the variance fraction crosses the per-budget or platform-default
// threshold. Mirrors finance.ActionTypeBudgetVariance /
// finance.DefaultBudgetVarianceIntervalSeconds; duplicated here for
// the same package-cycle reason as the other action-type constants
// above. Only seeded for plans that include the finance feature.
const (
	defaultBudgetVarianceActionType      = "budget_variance"
	defaultBudgetVarianceIntervalSeconds = 86400
)

// defaultDataRetentionActionType / defaultDataRetentionIntervalSeconds
// drive the daily retention sweeper that deletes rows older than the
// per-tenant retention_days threshold (migration 000032).
const (
	defaultDataRetentionActionType      = "data_retention_sweep"
	defaultDataRetentionIntervalSeconds = 86400
)

// defaultReportScheduleActionType / defaultReportScheduleIntervalSeconds
// drive the per-tenant report dispatcher that iterates report_schedules
// and emails the rendered output to the recipient list. Mirrors
// reporting.ActionTypeReportSchedule / DefaultReportScheduleIntervalSeconds;
// duplicated here for the same package-cycle reason as the SLA / reorder
// constants above.
const (
	defaultReportScheduleActionType      = "report_schedule"
	defaultReportScheduleIntervalSeconds = 300
)

// defaultLMSCertificateActionType / defaultLMSCertificateIntervalSeconds
// drive the LMS course-completion certificate auto-issuer. Mirrors
// services/worker/certificate_worker.go's CertificateActionType;
// duplicated here for the package-cycle reason above.
const (
	defaultLMSCertificateActionType      = "lms_issue_certificates"
	defaultLMSCertificateIntervalSeconds = 600
)

// defaultInsightsCacheRefreshActionType / Interval drive the per-tenant
// query_cache_refresh sweeper for Phase L Insights. The worker handler
// re-runs every saved query whose cache row is older than the per-query
// auto-refresh window so dashboards land on warm cache when a user
// opens them. Mirrors insights.ActionTypeQueryCacheRefresh; duplicated
// here for the same package-cycle reason as the constants above. Only
// seeded for plans that include the insights feature so free / starter
// tenants never run the sweeper.
const (
	defaultInsightsCacheRefreshActionType      = "query_cache_refresh"
	defaultInsightsCacheRefreshIntervalSeconds = 300
)

// seedDefaultScheduledActions seeds the per-tenant scheduled_actions
// rows the platform expects to exist after a successful wizard run.
// Uses INSERT … WHERE NOT EXISTS so re-running the wizard is a no-op
// and never duplicates queue rows.
func seedDefaultScheduledActions(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, plan string) error {
	now := time.Now().UTC()
	defaults := []struct {
		actionType      string
		intervalSeconds int
	}{
		{defaultSLABreachActionType, defaultSLABreachIntervalSeconds},
		{defaultRecurringInvoiceActionType, defaultRecurringInvoiceIntervalSeconds},
		{defaultInventoryReorderActionType, defaultInventoryReorderIntervalSeconds},
		{ActionTypeUsageSnapshot, defaultUsageSnapshotIntervalSeconds},
		{defaultDataRetentionActionType, defaultDataRetentionIntervalSeconds},
		{defaultReportScheduleActionType, defaultReportScheduleIntervalSeconds},
		{defaultLMSCertificateActionType, defaultLMSCertificateIntervalSeconds},
		{defaultRecordCountRecountActionType, defaultRecordCountRecountIntervalSeconds},
	}
	if DefaultFeaturesForPlan(plan)[FeatureFinance] {
		defaults = append(defaults, struct {
			actionType      string
			intervalSeconds int
		}{defaultUnrealizedFXActionType, defaultUnrealizedFXIntervalSeconds})
		defaults = append(defaults, struct {
			actionType      string
			intervalSeconds int
		}{defaultBudgetVarianceActionType, defaultBudgetVarianceIntervalSeconds})
	}
	if DefaultFeaturesForPlan(plan)[FeatureInsights] {
		defaults = append(defaults, struct {
			actionType      string
			intervalSeconds int
		}{defaultInsightsCacheRefreshActionType, defaultInsightsCacheRefreshIntervalSeconds})
	}
	for _, d := range defaults {
		if _, err := tx.Exec(ctx,
			`INSERT INTO scheduled_actions
			     (tenant_id, action_type, interval_seconds, next_run_at, payload, enabled)
			 SELECT $1, $2, $3, $4, '{}'::jsonb, TRUE
			  WHERE NOT EXISTS (
			      SELECT 1 FROM scheduled_actions
			       WHERE tenant_id = $1 AND action_type = $2
			  )`,
			tenantID, d.actionType, d.intervalSeconds, now,
		); err != nil {
			return fmt.Errorf("tenant: seed scheduled action %s: %w", d.actionType, err)
		}
	}
	return nil
}

// seedDefaultFeatures inserts one tenant_features row per canonical
// feature flag with enabled = DefaultFeaturesForPlan(plan)[feature].
// INSERT … ON CONFLICT DO NOTHING so re-running the wizard after a
// tenant has manually overridden a flag is a no-op on that flag —
// the platform only seeds the default, it never rewrites operator
// intent.
func seedDefaultFeatures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, plan string) error {
	defaults := DefaultFeaturesForPlan(plan)
	for _, key := range AllFeatures {
		enabled, ok := defaults[key]
		if !ok {
			// Unmapped feature → default enabled so new
			// additions opt in automatically.
			enabled = true
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_features (tenant_id, feature_key, enabled)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, feature_key) DO NOTHING`,
			tenantID, key, enabled,
		); err != nil {
			return fmt.Errorf("tenant: seed feature %q: %w", key, err)
		}
	}
	return nil
}

// retentionDefaultDays returns the (category, retention_days) pairs
// the wizard seeds per plan. Categories are the well-known set
// understood by platform.RetentionSweeper.
func retentionDefaultDays(plan string) map[string]int {
	switch strings.ToLower(plan) {
	case "enterprise":
		return map[string]int{
			"audit_log":          365,
			"events":             180,
			"sla_log":            365,
			"webhook_deliveries": 90,
			"notifications":      180,
			"import_staging":     90,
		}
	case "starter", "professional", "business":
		return map[string]int{
			"audit_log":          180,
			"events":             90,
			"sla_log":            180,
			"webhook_deliveries": 60,
			"notifications":      90,
			"import_staging":     60,
		}
	default: // free / trial / unknown — keep tight retention to control storage.
		return map[string]int{
			"audit_log":          90,
			"events":             30,
			"sla_log":            90,
			"webhook_deliveries": 30,
			"notifications":      30,
			"import_staging":     30,
		}
	}
}

// seedDefaultRetentionPolicies writes one data_retention_policies row
// per category in retentionDefaultDays(plan). ON CONFLICT DO NOTHING
// preserves operator-applied overrides on re-runs of the wizard.
func seedDefaultRetentionPolicies(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, plan string) error {
	for category, days := range retentionDefaultDays(plan) {
		if _, err := tx.Exec(ctx,
			`INSERT INTO data_retention_policies (tenant_id, category, retention_days, enabled)
			 VALUES ($1, $2, $3, TRUE)
			 ON CONFLICT (tenant_id, category) DO NOTHING`,
			tenantID, category, days,
		); err != nil {
			return fmt.Errorf("tenant: seed retention policy %q: %w", category, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Workstream 8 — Default-Wired Onboarding & Guided Setup
//
// SmartDefaults wraps RunSetupWizard with zero-configuration country
// detection so a brand-new tenant lands on a productive workspace in
// under two minutes. SeedSampleData populates realistic, interlinked
// demo records so dashboards / reports / insights have data the moment
// the operator first logs in. Both are additive, clearly-named entry
// points; they do not touch the existing RunSetupWizard control flow.
//
// KType names are duplicated as local string constants rather than
// imported from internal/crm, internal/finance, … to avoid an import
// cycle (those packages import internal/ktype which imports
// internal/platform which imports internal/tenant). This mirrors the
// action-type constant duplication already used throughout this file.
const (
	sampleKTypeContact  = "crm.contact"
	sampleKTypeDeal     = "crm.deal"
	sampleKTypeInvoice  = "finance.ar_invoice"
	sampleKTypeItem     = "inventory.item"
	sampleKTypeEmployee = "hr.employee"
	sampleKTypeProject  = "projects.project"
	// gettingStartedKType is the tasks.task KType the onboarding
	// checklist is stored under so it shows up in the operator's
	// normal task surfaces in addition to the dedicated
	// OnboardingChecklistPage.
	gettingStartedKType = "tasks.task"
)

// Country-detection source labels reported on SmartDefaultsResult so
// the caller / UI can explain *why* a given country was chosen
// (e.g. "auto-detected from your KChat profile").
const (
	CountrySourceExplicit      = "explicit"
	CountrySourceProfileLocale = "profile_locale"
	CountrySourceGeoIP         = "geoip"
	CountrySourceFallback      = "fallback"
)

// GeoIPResolver maps a client IP to an ISO 3166-1 alpha-2 country
// code. It is the last-resort country detector SmartDefaults consults
// when neither an explicit country nor a KChat profile locale is
// available. Implementations back onto a MaxMind-style database or an
// upstream lookup service; a nil resolver simply skips the geo step.
type GeoIPResolver interface {
	CountryForIP(ip string) (country string, ok bool)
}

// WithGeoIPResolver attaches the IP→country detector SmartDefaults
// falls back to when no explicit country or profile locale is
// supplied. Returns the wizard for fluent chaining; passing nil is a
// no-op (the geo step is simply skipped).
func (w *Wizard) WithGeoIPResolver(r GeoIPResolver) *Wizard {
	if r != nil {
		w.geoIP = r
	}
	return w
}

// SmartDefaultsConfig is the minimal payload the onboarding flow needs
// to provision a zero-configuration tenant. Everything statutory
// (CoA, currency, locale, timezone, feature set, roles) is resolved
// automatically from the detected country and the plan — the operator
// only has to confirm a company name.
type SmartDefaultsConfig struct {
	// CompanyName is the only strictly-required field; it is passed
	// straight through to RunSetupWizard.
	CompanyName string `json:"company_name"`
	// Plan drives the feature set and retention windows
	// (DefaultFeaturesForPlan via seedDefaultFeatures). Empty is
	// treated as the free tier by the downstream seeders.
	Plan string `json:"plan,omitempty"`
	// CreatedBy is the tenant owner. It is stamped as created_by on
	// every seeded record and as the assignee of the Getting Started
	// checklist task.
	CreatedBy uuid.UUID `json:"created_by,omitempty"`
	// SampleData, when true, populates the tenant with interlinked
	// demo records (see SeedSampleData).
	SampleData bool `json:"sample_data,omitempty"`
	// Country, when set, wins over every auto-detection source. It
	// is the value the wizard UI sends once the operator confirms or
	// edits the auto-detected country. ISO 3166-1 alpha-2.
	Country string `json:"country,omitempty"`
	// ProfileLocale is the KChat user profile locale (e.g. "de-CH").
	// SmartDefaults extracts the region subtag as the detected
	// country when Country is empty.
	ProfileLocale string `json:"profile_locale,omitempty"`
	// ClientIP is the request source address used for the geo-IP
	// fallback when neither Country nor ProfileLocale yields a
	// region.
	ClientIP string `json:"client_ip,omitempty"`
	// Users carries optional team invites collected on the wizard's
	// second step. Threaded straight through to RunSetupWizard.
	Users []WizardUser `json:"users,omitempty"`
}

// SmartDefaultsResult reports what the zero-config provisioning
// resolved. It embeds the underlying WizardResult so callers keep
// access to AccountsInserted / RolesInserted / LocaleUsed / … while
// also seeing the auto-detected country, currency, timezone, the
// seeded sample-data summary, and the Getting Started checklist id.
type SmartDefaultsResult struct {
	WizardResult
	DetectedCountry string            `json:"detected_country"`
	CountrySource   string            `json:"country_source"`
	CurrencyUsed    string            `json:"currency_used"`
	TimezoneUsed    string            `json:"timezone_used"`
	ChecklistTaskID uuid.UUID         `json:"checklist_task_id"`
	SampleData      *SampleDataResult `json:"sample_data,omitempty"`
}

// SampleDataResult counts the interlinked demo records SeedSampleData
// created so the caller can surface "we set up N example records" in
// the wizard completion screen.
type SampleDataResult struct {
	Contacts  int `json:"contacts"`
	Deals     int `json:"deals"`
	Invoices  int `json:"invoices"`
	Items     int `json:"items"`
	Employees int `json:"employees"`
	Projects  int `json:"projects"`
}

// Total returns the number of demo records created across all KTypes.
func (r *SampleDataResult) Total() int {
	if r == nil {
		return 0
	}
	return r.Contacts + r.Deals + r.Invoices + r.Items + r.Employees + r.Projects
}

// CountryFromLocaleTag extracts the ISO 3166-1 alpha-2 region subtag
// from a BCP 47 locale tag (e.g. "de-CH" → "CH", "en_US" → "US",
// "zh-Hant-HK" → "HK"). It returns "" when the tag carries no region
// subtag ("de", "en") so callers can fall through to the next
// detection source. Both "-" and "_" separators are accepted because
// KChat profiles have historically emitted either.
func CountryFromLocaleTag(tag string) string {
	t := strings.TrimSpace(tag)
	if t == "" {
		return ""
	}
	parts := strings.FieldsFunc(t, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if i == 0 {
			// First subtag is the language; never a region.
			continue
		}
		if len(p) != 2 {
			continue
		}
		allAlpha := true
		for _, c := range p {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
				allAlpha = false
				break
			}
		}
		if allAlpha {
			return strings.ToUpper(p)
		}
	}
	return ""
}

// DefaultCurrencyForCountry returns the ISO 4217 functional currency
// the wizard should pre-fill for the given ISO 3166-1 alpha-2 country.
// The mapping covers every country with a registered CoA template /
// tax pack so SmartDefaults never has to ask; unmapped countries fall
// back to USD, which keeps the 3-letter CHECK on tenants.base_currency
// satisfied and matches the generic IFRS chart's reporting currency.
func DefaultCurrencyForCountry(country string) string {
	switch strings.ToUpper(strings.TrimSpace(country)) {
	case "US", "EC", "SV", "PA":
		return "USD"
	case "DE", "FR", "IT", "ES", "IE", "AT", "NL", "BE", "PT", "FI", "GR":
		return "EUR"
	case "GB":
		return "GBP"
	case "CH":
		return "CHF"
	case "JP":
		return "JPY"
	case "CN":
		return "CNY"
	case "HK":
		return "HKD"
	case "TW":
		return "TWD"
	case "SG":
		return "SGD"
	case "MY":
		return "MYR"
	case "TH":
		return "THB"
	case "ID":
		return "IDR"
	case "VN":
		return "VND"
	case "PH":
		return "PHP"
	case "IN":
		return "INR"
	case "AU":
		return "AUD"
	case "NZ":
		return "NZD"
	case "CA":
		return "CAD"
	case "AE":
		return "AED"
	case "SA":
		return "SAR"
	case "QA":
		return "QAR"
	case "KW":
		return "KWD"
	case "BH":
		return "BHD"
	case "OM":
		return "OMR"
	default:
		return "USD"
	}
}

// DefaultTimezoneForCountry returns the canonical IANA timezone the
// wizard should pre-fill for the given ISO 3166-1 alpha-2 country.
// One representative zone per country is chosen (the commercial
// capital for multi-zone countries) so first-run never has to
// disambiguate; the operator can change it from the admin surface.
// Unmapped countries fall back to "UTC", matching the column default
// on migration 000047.
func DefaultTimezoneForCountry(country string) string {
	switch strings.ToUpper(strings.TrimSpace(country)) {
	case "US", "CA":
		return "America/New_York"
	case "GB", "IE", "PT":
		return "Europe/London"
	case "DE", "AT", "CH", "NL", "BE":
		return "Europe/Berlin"
	case "FR":
		return "Europe/Paris"
	case "IT":
		return "Europe/Rome"
	case "ES":
		return "Europe/Madrid"
	case "JP":
		return "Asia/Tokyo"
	case "CN":
		return "Asia/Shanghai"
	case "HK":
		return "Asia/Hong_Kong"
	case "TW":
		return "Asia/Taipei"
	case "SG":
		return "Asia/Singapore"
	case "MY":
		return "Asia/Kuala_Lumpur"
	case "TH":
		return "Asia/Bangkok"
	case "ID":
		return "Asia/Jakarta"
	case "VN":
		return "Asia/Ho_Chi_Minh"
	case "PH":
		return "Asia/Manila"
	case "IN":
		return "Asia/Kolkata"
	case "AU":
		return "Australia/Sydney"
	case "NZ":
		return "Pacific/Auckland"
	case "AE":
		return "Asia/Dubai"
	case "SA", "QA", "BH", "KW":
		return "Asia/Riyadh"
	case "OM":
		return "Asia/Muscat"
	default:
		return "UTC"
	}
}

// resolveCountry applies the SmartDefaults detection precedence:
// explicit operator choice → KChat profile locale region → geo-IP →
// fallback (no country). It returns the detected ISO 3166-1 alpha-2
// code (or "" when nothing resolves) and a CountrySource* label
// explaining the winning source.
func (w *Wizard) resolveCountry(cfg SmartDefaultsConfig) (country, source string) {
	if c := strings.ToUpper(strings.TrimSpace(cfg.Country)); len(c) == 2 {
		return c, CountrySourceExplicit
	}
	if c := CountryFromLocaleTag(cfg.ProfileLocale); c != "" {
		return c, CountrySourceProfileLocale
	}
	if w.geoIP != nil && strings.TrimSpace(cfg.ClientIP) != "" {
		if c, ok := w.geoIP.CountryForIP(strings.TrimSpace(cfg.ClientIP)); ok {
			if up := strings.ToUpper(strings.TrimSpace(c)); len(up) == 2 {
				return up, CountrySourceGeoIP
			}
		}
	}
	return "", CountrySourceFallback
}

// SmartDefaults provisions a tenant with zero manual configuration. It
// auto-detects the country (from an explicit choice, the KChat profile
// locale, or geo-IP), resolves the statutory CoA template, currency,
// locale, and timezone from that country, enables the plan-appropriate
// feature set, optionally seeds interlinked sample data, and always
// creates a "Getting Started" checklist task. It delegates the
// account / role / feature / locale seeding to RunSetupWizard so there
// is exactly one code path that writes those rows.
func (w *Wizard) SmartDefaults(ctx context.Context, tenantID uuid.UUID, cfg SmartDefaultsConfig) (*SmartDefaultsResult, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: smart defaults requires tenant id")
	}
	if strings.TrimSpace(cfg.CompanyName) == "" {
		return nil, errors.New("tenant: smart defaults requires company_name")
	}

	country, source := w.resolveCountry(cfg)
	currency := DefaultCurrencyForCountry(country)
	timezone := DefaultTimezoneForCountry(country)

	// Delegate the statutory seeding to RunSetupWizard. Country drives
	// CoA + locale resolution there; an empty Country is valid and
	// resolves to the generic IFRS chart + English locale. Leaving
	// CoATemplate / Locale empty lets the wizard's own country-derived
	// defaults run rather than duplicating that logic here.
	wizCfg := SetupWizardConfig{
		CompanyName:  cfg.CompanyName,
		Country:      country,
		CurrencyCode: currency,
		Plan:         cfg.Plan,
		CreatedBy:    cfg.CreatedBy,
		SampleData:   cfg.SampleData,
		Users:        cfg.Users,
	}
	wr, err := w.RunSetupWizard(ctx, tenantID, wizCfg)
	if err != nil {
		return nil, err
	}

	out := &SmartDefaultsResult{
		WizardResult:    *wr,
		DetectedCountry: country,
		CountrySource:   source,
		CurrencyUsed:    currency,
		TimezoneUsed:    timezone,
	}

	// Persist the resolved timezone and seed the Getting Started
	// checklist in one tenant-scoped tx so both land (or roll back)
	// atomically under the app.tenant_id GUC the RLS policies require.
	if err := dbutil.WithTenantTx(ctx, w.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if timezone != "" {
			if _, err := tx.Exec(ctx,
				`UPDATE tenants SET timezone = $1, updated_at = now() WHERE id = $2`,
				timezone, tenantID,
			); err != nil {
				return fmt.Errorf("tenant: smart defaults persist timezone: %w", err)
			}
		}
		taskID, err := seedGettingStartedChecklist(ctx, tx, tenantID, cfg.CreatedBy)
		if err != nil {
			return err
		}
		out.ChecklistTaskID = taskID
		return nil
	}); err != nil {
		return nil, err
	}

	// Sample data runs after the wizard tx so a seeding hiccup never
	// rolls back the (already-committed) statutory setup — the tenant
	// is usable even if the demo records fail to populate.
	if cfg.SampleData {
		sd, err := w.SeedSampleData(ctx, tenantID, cfg.CreatedBy, currency)
		if err != nil {
			return out, fmt.Errorf("tenant: smart defaults seed sample data: %w", err)
		}
		out.SampleData = sd
	}

	return out, nil
}

// gettingStartedSteps are the canonical first-use milestones the
// onboarding checklist tracks. The same three steps back the
// /getting-started KChat card and the OnboardingChecklistPage so the
// surfaces never drift. Each carries a stable key (for completion
// tracking) and a deep link to the page that completes it.
var gettingStartedSteps = []map[string]any{
	{"key": "create_contact", "label": "Create your first contact", "done": false, "link": "/records/crm.contact/new"},
	{"key": "send_invoice", "label": "Send your first invoice", "done": false, "link": "/records/finance.ar_invoice/new"},
	{"key": "import_data", "label": "Import existing data", "done": false, "link": "/imports/new"},
}

// seedGettingStartedChecklist creates the onboarding checklist as a
// tasks.task KRecord. It is idempotent per tenant: a second call is a
// no-op that returns the existing task's id, so re-running the wizard
// never spawns duplicate checklists. The caller must already hold a
// tenant-scoped tx (app.tenant_id set).
func seedGettingStartedChecklist(ctx context.Context, tx pgx.Tx, tenantID, createdBy uuid.UUID) (uuid.UUID, error) {
	const checklistTitle = "Getting Started"

	// Idempotency: reuse an existing checklist if one was already
	// seeded for this tenant (data->>'onboarding' == "checklist").
	var existing uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM krecords
		  WHERE tenant_id = $1 AND ktype = $2 AND deleted_at IS NULL
		    AND data->>'onboarding' = 'checklist'
		  LIMIT 1`,
		tenantID, gettingStartedKType,
	).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through and create it
	default:
		return uuid.Nil, fmt.Errorf("tenant: lookup getting-started checklist: %w", err)
	}

	data := map[string]any{
		"title":       checklistTitle,
		"status":      "open",
		"assignee":    createdBy.String(),
		"description": "Complete these steps to get the most out of your workspace.",
		"onboarding":  "checklist",
		"steps":       gettingStartedSteps,
	}
	id, err := insertSeedRecord(ctx, tx, tenantID, createdBy, gettingStartedKType, data)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant: seed getting-started checklist: %w", err)
	}
	return id, nil
}

// SeedSampleData populates the tenant with realistic, interlinked demo
// records so dashboards, reports, and insights have data immediately:
// 5 contacts, 3 deals, 2 invoices, 5 inventory items, 3 employees, and
// 1 project. Records are linked through their ref fields (deals →
// contacts, invoices → contacts + deals, employees → manager, project
// → owner) so cross-module rollups render on first login. All inserts
// run inside one tenant-scoped tx so a partial failure rolls the whole
// demo set back rather than leaving dangling references.
func (w *Wizard) SeedSampleData(ctx context.Context, tenantID, createdBy uuid.UUID, currency string) (*SampleDataResult, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: seed sample data requires tenant id")
	}
	if len(currency) != 3 {
		currency = "USD"
	}
	owner := createdBy.String()
	res := &SampleDataResult{}

	if err := dbutil.WithTenantTx(ctx, w.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// --- Contacts -------------------------------------------------
		contactSeeds := []struct {
			name, email, phone string
		}{
			{"Ada Lovelace", "ada@example.com", "+1-202-555-0101"},
			{"Alan Turing", "alan@example.com", "+44-20-7946-0102"},
			{"Grace Hopper", "grace@example.com", "+1-202-555-0103"},
			{"Katherine Johnson", "katherine@example.com", "+1-202-555-0104"},
			{"Edsger Dijkstra", "edsger@example.com", "+31-20-555-0105"},
		}
		contactIDs := make([]uuid.UUID, 0, len(contactSeeds))
		for _, c := range contactSeeds {
			id, err := insertSeedRecord(ctx, tx, tenantID, createdBy, sampleKTypeContact, map[string]any{
				"name":  c.name,
				"email": c.email,
				"phone": c.phone,
				"owner": owner,
			})
			if err != nil {
				return err
			}
			contactIDs = append(contactIDs, id)
		}
		res.Contacts = len(contactIDs)

		// --- Deals (linked to the first three contacts) ---------------
		dealSeeds := []struct {
			name   string
			stage  string
			amount float64
		}{
			{"Acme Platform Rollout", "qualification", 12000},
			{"Globex Annual Renewal", "proposal", 48000},
			{"Initech Pilot", "negotiation", 7500},
		}
		dealIDs := make([]uuid.UUID, 0, len(dealSeeds))
		for i, d := range dealSeeds {
			id, err := insertSeedRecord(ctx, tx, tenantID, createdBy, sampleKTypeDeal, map[string]any{
				"name":       d.name,
				"stage":      d.stage,
				"amount":     d.amount,
				"currency":   currency,
				"close_date": sampleDate(30 + i*15),
				"contact":    contactIDs[i].String(),
				"owner":      owner,
				"notes":      "Sample deal created during onboarding.",
			})
			if err != nil {
				return err
			}
			dealIDs = append(dealIDs, id)
		}
		res.Deals = len(dealIDs)

		// --- Invoices (linked to a contact + the matching deal) -------
		for i := 0; i < 2; i++ {
			subtotal := dealSeeds[i].amount
			tax := subtotal * 0.1
			if _, err := insertSeedRecord(ctx, tx, tenantID, createdBy, sampleKTypeInvoice, map[string]any{
				"customer_id":    contactIDs[i].String(),
				"deal_id":        dealIDs[i].String(),
				"invoice_number": fmt.Sprintf("INV-%04d", i+1),
				"issue_date":     sampleDate(0),
				"due_date":       sampleDate(30),
				"subtotal":       subtotal,
				"tax_amount":     tax,
				"total":          subtotal + tax,
				"currency":       currency,
				"status":         "draft",
				"owner":          owner,
			}); err != nil {
				return err
			}
			res.Invoices++
		}

		// --- Inventory items -----------------------------------------
		itemSeeds := []struct {
			sku, name, uom string
		}{
			{"SKU-1001", "Widget", "each"},
			{"SKU-1002", "Gadget", "each"},
			{"SKU-1003", "Cable (2m)", "each"},
			{"SKU-1004", "Mounting Kit", "box"},
			{"SKU-1005", "Replacement Filter", "each"},
		}
		for _, it := range itemSeeds {
			if _, err := insertSeedRecord(ctx, tx, tenantID, createdBy, sampleKTypeItem, map[string]any{
				"sku":           it.sku,
				"name":          it.name,
				"uom":           it.uom,
				"active":        true,
				"reorder_level": 10,
				"reorder_qty":   50,
			}); err != nil {
				return err
			}
			res.Items++
		}

		// --- Employees (reports interlinked to the first hire) --------
		empSeeds := []struct {
			name, email, dept, title string
		}{
			{"Maria Garcia", "maria@example.com", "Operations", "Operations Manager"},
			{"James Smith", "james@example.com", "Sales", "Account Executive"},
			{"Wei Chen", "wei@example.com", "Engineering", "Software Engineer"},
		}
		empIDs := make([]uuid.UUID, 0, len(empSeeds))
		for i, e := range empSeeds {
			data := map[string]any{
				"name":            e.name,
				"email":           e.email,
				"department":      e.dept,
				"designation":     e.title,
				"date_of_joining": sampleDate(-90 + i*10),
				"status":          "active",
			}
			// Reports (index > 0) roll up to the first hire so the
			// org chart renders a two-level tree out of the box.
			if i > 0 && len(empIDs) > 0 {
				data["reporting_to"] = empIDs[0].String()
			}
			id, err := insertSeedRecord(ctx, tx, tenantID, createdBy, sampleKTypeEmployee, data)
			if err != nil {
				return err
			}
			empIDs = append(empIDs, id)
		}
		res.Employees = len(empIDs)

		// --- Project (owned by the tenant owner) ---------------------
		if _, err := insertSeedRecord(ctx, tx, tenantID, createdBy, sampleKTypeProject, map[string]any{
			"name":        "Onboarding Demo Project",
			"description": "A sample project linking the demo records together.",
			"code":        "DEMO-1",
			"owner":       owner,
			"start_date":  sampleDate(0),
			"end_date":    sampleDate(60),
			"status":      "active",
		}); err != nil {
			return err
		}
		res.Projects = 1

		// Keep the denormalised quota counter in lockstep with the
		// rows we just inserted. Mirrors platform.BumpTenantRecordCount
		// (inlined to avoid the platform→tenant import cycle).
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_record_counts (tenant_id, record_count, updated_at)
			 VALUES ($1, GREATEST($2, 0), now())
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET record_count = GREATEST(tenant_record_counts.record_count + $2, 0),
			       updated_at   = now()`,
			tenantID, int64(res.Total()),
		); err != nil {
			return fmt.Errorf("tenant: bump record count for sample data: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return res, nil
}

// insertSeedRecord inserts one KRecord directly via SQL inside the
// caller's tenant-scoped tx. Every seeded KType is at schema version 1
// and lands in the active state. It returns the new record id so demo
// records can be interlinked through their ref fields. Unlike
// record.PGStore.Create it deliberately skips KType-schema validation
// and audit/event emission — the seeder controls the payloads and a
// burst of "demo data created" audit rows would only add noise to a
// brand-new tenant's log.
func insertSeedRecord(ctx context.Context, tx pgx.Tx, tenantID, createdBy uuid.UUID, ktype string, data map[string]any) (uuid.UUID, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant: marshal %s seed: %w", ktype, err)
	}
	id := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO krecords (id, tenant_id, ktype, ktype_version, data, status, version, created_by)
		 VALUES ($1, $2, $3, 1, $4, 'active', 1, $5)`,
		id, tenantID, ktype, raw, createdBy,
	); err != nil {
		return uuid.Nil, fmt.Errorf("tenant: insert %s seed: %w", ktype, err)
	}
	return id, nil
}

// sampleDate returns an ISO-8601 (YYYY-MM-DD) date offsetDays from
// today, used to give the demo records plausible past/future dates so
// time-bucketed reports and pipelines render meaningfully.
func sampleDate(offsetDays int) string {
	return time.Now().UTC().AddDate(0, 0, offsetDays).Format("2006-01-02")
}
