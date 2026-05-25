package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// huPack implements Hungary's payroll-side statutory withholdings
// for the 2025 fiscal year:
//
//   - SZJA (személyi jövedelemadó, personal income tax): flat
//     15% on gross earnings with no progressive brackets. The
//     under-25 (fiatalok kedvezménye) and four-or-more children
//     exemptions are not modelled here — those require per-
//     employee SZJA-nyilatkozat data that is outside the
//     EmployeeInfo schema; tenants needing the exemption can
//     override the rate via custom payroll rules.
//
//   - TB járulék (employee social security contribution): flat
//     18.5% on gross earnings, covering:
//
//        10.0% nyugdíjjárulék (pension contribution)
//         7.0% egészségbiztosítási járulék (health
//              insurance: 4% in-kind + 3% in-cash)
//         1.5% munkaerő-piaci járulék (unemployment / labour
//              market)
//        ----- 18.5% total
//
//     No annual cap from 2020 onwards (the previous 24× minimum-
//     wage cap was abolished).
//
// References:
//
//	NAV — SZJA 2025:
//	  https://www.nav.gov.hu/szja
//	NAV — TB járulék 2025:
//	  https://www.nav.gov.hu/jarulek
type huPack struct{}

func init() { Register(&huPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (huPack) Country() string { return "HU" }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from (NAV + Szocho 2025 mértékek).
func (huPack) EffectiveYear() int { return 2025 }

var (
	huSZJARate = dec("0.15")
	huTBRate   = dec("0.185")
)

// ComputeWithholding emits up to two lines:
//
//   - HU_TB   (TB járulék employee share, 18.5% on gross)
//   - HU_SZJA (személyi jövedelemadó, 15% on gross)
//
// Negative or zero gross returns nil.
func (huPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}

	out := []Deduction{}

	tb := gross.Mul(huTBRate).Round(2)
	if tb.IsPositive() {
		out = append(out, Deduction{
			Code:   "HU_TB",
			Name:   "TB járulék (HU)",
			Amount: tb,
		})
	}

	szja := gross.Mul(huSZJARate).Round(2)
	if szja.IsPositive() {
		out = append(out, Deduction{
			Code:   "HU_SZJA",
			Name:   "SZJA personal income tax (HU)",
			Amount: szja,
		})
	}

	return out, nil
}
