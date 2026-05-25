// Package taxpacks resolves a per-country statutory withholding
// engine for the payroll runner.
//
// The payroll engine reads `tenants.country` (set by the Phase M
// setup wizard) and looks up a TaxPack via Lookup(). Unknown country
// codes return ErrNoPack so the engine can fall back to the legacy
// "no statutory pack" behaviour without breaking existing tenants.
//
// Reference shape mirrors the Frappe HRMS payroll-entry statutory-
// deduction pattern (see https://github.com/frappe/hrms): one
// pluggable component per jurisdiction, called once per slip with
// the resolved gross + employee meta + period.
package taxpacks

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// ErrNoPack is returned by Lookup when no tax pack is registered for
// the given country code. Callers in payroll_engine.go treat this
// as "skip statutory withholding" rather than failing the slip.
var ErrNoPack = errors.New("taxpacks: no pack registered for country")

// PayPeriod describes the slip's pay period boundaries. Periodicity
// is derived by the pack from (End - Start) so callers don't have to
// hard-code "weekly" / "fortnightly" / "monthly" in the slip itself.
type PayPeriod struct {
	Start time.Time
	End   time.Time
}

// Days returns the inclusive day count between Start and End. A
// pack uses this to scale annual brackets onto the slip's window.
func (p PayPeriod) Days() int {
	if p.End.Before(p.Start) {
		return 0
	}
	return int(p.End.Sub(p.Start).Hours()/24) + 1
}

// EmployeeInfo carries the subset of employee fields a tax pack
// needs to compute withholding. The payroll engine builds this
// projection from the employee KRecord rather than passing the raw
// JSONB so packs stay decoupled from the record schema.
//
// Every field is optional from a *pack's* perspective: a pack that
// does not care about (e.g.) Canton just ignores it. The engine
// populates whatever fields the employee KRecord supplies and
// leaves the rest at their zero values — packs are responsible for
// defaulting to the "most common" case when an input is missing so
// legacy KRecords from pre-Phase-M2 don't break.
type EmployeeInfo struct {
	ID         string
	FilingType string // US: "single", "married_filing_jointly". AU: "single" / "with_partner" (unused for PAYG).
	Allowances int    // US W-4 allowances (legacy). 0 for AU.
	Resident   bool   // AU residency status — non-residents pay a flat 32.5% bracket from $0.
	HasTFN     bool   // AU: tax file number declared. False forces the 47% no-TFN rate.
	YTDGross   decimal.Decimal
	Currency   string

	// Canton is the 2-letter Swiss canton code (ZH, GE, VD, …)
	// used by the CH pack to resolve cantonal tax on top of
	// federal direct tax. Empty falls back to the federal-only
	// rate; the CH pack documents this in its own comments.
	Canton string

	// Nationality drives the GCC packs' social-security branch
	// selection. "local" (Saudi national in SA, Bahraini in BH,
	// …) pays full GOSI/PIFSS/SIO/PASI/GPSSA contributions;
	// "expat" pays nothing (or, for SA non-Saudis, only the
	// employer-side GOSI which is not an employee deduction).
	// Empty defaults to "expat" so a pre-Phase-M2 employee
	// KRecord without the field gets the safer (smaller-
	// deduction) treatment.
	Nationality string

	// TaxRegime is the IN pack's old-vs-new TDS schedule
	// selector. Values: "old" (FY 2023-24 brackets + deductions)
	// or "new" (FY 2024-25 default regime). Empty defaults to
	// "new" to match the post-Budget-2024 default behaviour for
	// every employee who hasn't explicitly opted out.
	TaxRegime string

	// KiwiSaverRate is the NZ employee KiwiSaver contribution
	// rate (3 / 4 / 6 / 8 / 10%). decimal.Zero means "no
	// KiwiSaver opt-in" (no deduction); positive values are
	// applied verbatim. The NZ pack does not enrol an employee
	// in KiwiSaver automatically — the rate must be set on the
	// KRecord to opt in.
	KiwiSaverRate decimal.Decimal

	// NumDependents is the count of qualifying dependents used
	// by VN PIT (4.4M VND/month per dependent on top of the 11M
	// VND personal deduction) and TH PIT (60k THB/year per child).
	// Zero means "no dependents claimed".
	NumDependents int

	// Age in years on the slip's pay-period end date. Drives the
	// SG CPF tier ladder (rates step down at 55 / 60 / 65 / 70).
	// Zero means "unknown"; the SG pack treats unknown as
	// age ≤55 (the highest CPF rate, fail-safe for over-
	// withholding which the IRAS year-end assessment can refund).
	Age int

	// PermitType drives jurisdiction-specific resident status
	// flags that don't fit cleanly in Resident. For CH this is
	// "C" (settlement permit → not Quellensteuer-liable) vs
	// "B"/"L" (annual/short-term → Quellensteuer applies). For
	// SG this is "EP"/"SP"/"WP" but SG branches off Resident
	// (set by employer payroll classification) so PermitType is
	// only consulted for the source-tax decision today.
	PermitType string

	// Province is the 2-letter Canadian province/territory code
	// (ON, BC, AB, QC, MB, SK, NS, NB, NL, PE, NT, NU, YT). It
	// drives the second bracket walk inside the CA pack
	// (provincial tax on top of federal). QC is special — it
	// uses Revenu Québec's own brackets and gates QPP / QPIP
	// instead of CPP / federal EI. Empty falls back to the
	// federal-only computation (and emits a no-province slip
	// warning in the pack rather than crashing).
	Province string

	// CPPExempt is true when the employee is exempt from Canada
	// Pension Plan contributions for the slip. Statutory CPP
	// exemptions apply below age 18 and above 70 (and to QPP
	// for QC residents under the same age rules). The pack
	// honours this flag verbatim without re-deriving from Age
	// so per-employee exemption letters (e.g. a CRA CPT30
	// election) are respected.
	CPPExempt bool

	// EIExempt is true when the employee is exempt from
	// Employment Insurance premiums for the slip. Typical
	// triggers: a Canadian shareholder with >40% voting equity,
	// or a non-arm's-length related person who isn't
	// insurable employment under EI Act s.5(2).
	EIExempt bool
}

// Deduction is one withholding line a pack appends to the slip's
// deductions array. The Code is canonical (e.g. "FED_TAX",
// "FICA_OASDI", "PAYG_WITHHOLDING") so the ledger can map it to a
// liability account; Name is human-readable for the slip UI.
type Deduction struct {
	Code   string
	Name   string
	Amount decimal.Decimal
}

// TaxPack is the contract every country pack implements.
//
// ComputeWithholding receives the employee projection, the gross
// pay for the slip, and the pay period. It returns zero or more
// Deduction lines; an empty slice means "no statutory withholding"
// (a legitimate result for, say, an under-threshold AU PAYG slip).
//
// EffectiveYear returns the fiscal year the pack's rate tables are
// calibrated for. The payroll engine compares this against the
// slip's pay-period year and logs a warning (not an error) when
// they diverge so operators know the rates may be stale. The
// quarterly maintenance procedure documented in
// docs/TAX_PACK_MAINTENANCE.md drives the bump.
type TaxPack interface {
	Country() string
	EffectiveYear() int
	ComputeWithholding(ctx context.Context, employee EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error)
}

// registry holds the registered packs keyed by uppercased ISO
// 3166-1 alpha-2 country code. Initialised by each pack's init()
// so callers don't need to maintain a central registration list.
var registry = map[string]TaxPack{}

// Register adds a pack to the registry. Re-registration replaces
// the existing entry so tests can swap in a stub without restarting
// the process. Country codes are upper-cased before keying.
func Register(pack TaxPack) {
	if pack == nil {
		return
	}
	registry[strings.ToUpper(pack.Country())] = pack
}

// Lookup returns the TaxPack for the given ISO 3166-1 alpha-2
// country code, or ErrNoPack if none is registered. Empty input
// always returns ErrNoPack so legacy tenants without a country code
// take the no-pack code path.
func Lookup(country string) (TaxPack, error) {
	code := strings.ToUpper(strings.TrimSpace(country))
	if code == "" {
		return nil, ErrNoPack
	}
	pack, ok := registry[code]
	if !ok {
		return nil, ErrNoPack
	}
	return pack, nil
}

// RegisteredCountries returns the sorted set of country codes the
// runtime knows about. Used by the admin UI's country dropdown so
// the frontend doesn't hard-code a list that drifts from the
// backend.
func RegisteredCountries() []string {
	out := make([]string, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// isGCCNational normalises Nationality to the "local" branch for
// every GCC pack (AE, SA, QA, KW, BH, OM). Empty defaults to
// "expat" per EmployeeInfo.Nationality's documented convention.
// "local" is the canonical KRecord value for a national of the
// country whose pack is running; a tenant running GCC-reciprocal
// payroll for, say, a Bahraini national employed in the UAE today
// still records Nationality = "local" from the *UAE pack's*
// perspective because the reciprocal scheme makes them eligible
// for GPSSA contributions. Per-jurisdiction nationality gating
// beyond this binary is out of scope for the current pack set
// and tracked in docs/TAX_PACK_MAINTENANCE.md.
//
// Lives in taxpacks.go (not in ae.go) because it is a cross-pack
// helper consumed by every GCC pack — the file location should
// reflect the helper's ownership, not the order in which the GCC
// packs happen to alphabetise.
func isGCCNational(nat string) bool {
	return strings.EqualFold(strings.TrimSpace(nat), "local")
}
