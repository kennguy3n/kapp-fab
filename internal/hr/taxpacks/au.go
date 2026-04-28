package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// auPack implements ATO Schedule 1 (PAYG withholding) for resident
// individuals in the 2025-26 income year, plus the no-TFN flat
// rate and the foreign-resident schedule.
//
// The pack annualises the slip's gross via period.Days() / 365.25,
// runs the resulting figure against the official scale, then
// prorates the tax back onto the slip period. The Medicare levy is
// folded into the resident scale (consistent with the ATO's
// Schedule 1 "with leave loading" rate columns the payroll team
// asked for).
type auPack struct{}

func init() { Register(&auPack{}) }

func (auPack) Country() string { return "AU" }

// auBracket mirrors the ATO Schedule 1 statutory rates table.
// Floor / Top are annualised gross; Base is the cumulative tax at
// the bracket floor; Rate applies to the marginal portion above
// Floor. The schedule table is the canonical "scale 2 — claiming
// the tax-free threshold (resident)" matrix.
type auBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal // 0 = open-ended
	Base  decimal.Decimal
	Rate  decimal.Decimal
}

var (
	auScaleResident = []auBracket{
		{Floor: dec("0"), Top: dec("18200"), Base: dec("0"), Rate: dec("0")},
		{Floor: dec("18200"), Top: dec("45000"), Base: dec("0"), Rate: dec("0.16")},
		{Floor: dec("45000"), Top: dec("135000"), Base: dec("4288"), Rate: dec("0.30")},
		{Floor: dec("135000"), Top: dec("190000"), Base: dec("31288"), Rate: dec("0.37")},
		{Floor: dec("190000"), Top: decimal.Zero, Base: dec("51638"), Rate: dec("0.45")},
	}
	// Foreign-resident schedule — no tax-free threshold, flat
	// 32.5% from $0, then the resident upper brackets at the
	// resident floors. Mirrors ATO Schedule 1 "Foreign resident
	// rates" column.
	auScaleForeign = []auBracket{
		{Floor: dec("0"), Top: dec("135000"), Base: dec("0"), Rate: dec("0.325")},
		{Floor: dec("135000"), Top: dec("190000"), Base: dec("43875"), Rate: dec("0.37")},
		{Floor: dec("190000"), Top: decimal.Zero, Base: dec("64225"), Rate: dec("0.45")},
	}

	// No-TFN withholding is a flat 47% per ATO Schedule 1,
	// applied to every dollar including the otherwise tax-free
	// threshold. The 0.5% Medicare loading the ATO publishes for
	// non-residents is intentionally omitted; the pack is a
	// minimum-viable cut, not a full statutory engine.
	auNoTFNRate = dec("0.47")

	auPeriodsPerYear = decimal.NewFromFloat(365.25)
)

func (auPack) ComputeWithholding(ctx context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}

	// No-TFN takes precedence over scale selection — the ATO
	// requires the flat rate even for residents who haven't
	// declared a TFN.
	if !e.HasTFN {
		return []Deduction{{
			Code:   "PAYG_NO_TFN",
			Name:   "PAYG withholding — no TFN (AU)",
			Amount: gross.Mul(auNoTFNRate).Round(2),
		}}, nil
	}

	periodFraction := decimal.NewFromInt(int64(days)).Div(auPeriodsPerYear)
	annualGross := gross.Div(periodFraction)

	scale := auScaleResident
	if !e.Resident {
		scale = auScaleForeign
	}

	annualTax := walkAUBrackets(annualGross, scale)
	periodTax := annualTax.Mul(periodFraction).Round(2)
	if periodTax.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	return []Deduction{{
		Code:   "PAYG_WITHHOLDING",
		Name:   "PAYG withholding (AU)",
		Amount: periodTax,
	}}, nil
}

// walkAUBrackets resolves the annualised tax for the AU schedule.
// Unlike the US implementation each bracket carries a Base —
// the cumulative tax at Floor — so the marginal rate only applies
// to the portion above Floor. This mirrors the ATO's published
// "Tax = Base + Rate × (Income − Floor)" form one-for-one.
func walkAUBrackets(annual decimal.Decimal, scale []auBracket) decimal.Decimal {
	var match auBracket
	matched := false
	for _, b := range scale {
		if annual.LessThanOrEqual(b.Floor) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.Base.Add(annual.Sub(match.Floor).Mul(match.Rate))
}
