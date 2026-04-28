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
type EmployeeInfo struct {
	ID         string
	FilingType string // US: "single", "married_filing_jointly". AU: "single" / "with_partner" (unused for PAYG).
	Allowances int    // US W-4 allowances (legacy). 0 for AU.
	Resident   bool   // AU residency status — non-residents pay a flat 32.5% bracket from $0.
	HasTFN     bool   // AU: tax file number declared. False forces the 47% no-TFN rate.
	YTDGross   decimal.Decimal
	Currency   string
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
type TaxPack interface {
	Country() string
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
