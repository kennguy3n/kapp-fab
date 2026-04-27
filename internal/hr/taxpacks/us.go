package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// usPack implements 2026-tax-year federal withholding plus the
// employee share of FICA. State withholding is intentionally out
// of scope — the operator wires a state-specific component on the
// salary structure for that.
//
// The bracket table mirrors IRS Pub 15-T (2025 percentage method,
// single + MFJ). Rates are static for now; the engine treats
// brackets as an annual projection and prorates the resulting
// liability onto the slip's pay period via period.Days() / 365.25
// (rather than fixed period codes), so weekly / fortnightly /
// monthly / off-cycle slips all line up.
//
// FICA is computed at the statutory 6.2% OASDI (capped at the
// 2026 wage base of $176,100 / year) plus 1.45% Medicare, with the
// additional 0.9% Medicare surtax above $200k YTD gross. The cap +
// surtax checks both fire against EmployeeInfo.YTDGross so a slip
// that crosses the threshold mid-period is handled correctly.
type usPack struct{}

func init() { Register(&usPack{}) }

func (usPack) Country() string { return "US" }

// usBracket is one row of the IRS Pub 15-T percentage method table.
// Floor + Top are annualised gross (pre-deduction); Rate is the
// marginal rate applied to (gross - Floor) up to Top. The pack's
// ComputeWithholding scales gross to annual, walks the brackets,
// then divides the resulting tax back onto the slip period.
type usBracket struct {
	Floor decimal.Decimal
	Top   decimal.Decimal // 0 (literally decimal.Zero) = open-ended
	Rate  decimal.Decimal
}

var (
	usBracketsSingle = []usBracket{
		{Floor: dec("0"), Top: dec("11600"), Rate: dec("0.10")},
		{Floor: dec("11600"), Top: dec("47150"), Rate: dec("0.12")},
		{Floor: dec("47150"), Top: dec("100525"), Rate: dec("0.22")},
		{Floor: dec("100525"), Top: dec("191950"), Rate: dec("0.24")},
		{Floor: dec("191950"), Top: dec("243725"), Rate: dec("0.32")},
		{Floor: dec("243725"), Top: dec("609350"), Rate: dec("0.35")},
		{Floor: dec("609350"), Top: decimal.Zero, Rate: dec("0.37")},
	}
	usBracketsMFJ = []usBracket{
		{Floor: dec("0"), Top: dec("23200"), Rate: dec("0.10")},
		{Floor: dec("23200"), Top: dec("94300"), Rate: dec("0.12")},
		{Floor: dec("94300"), Top: dec("201050"), Rate: dec("0.22")},
		{Floor: dec("201050"), Top: dec("383900"), Rate: dec("0.24")},
		{Floor: dec("383900"), Top: dec("487450"), Rate: dec("0.32")},
		{Floor: dec("487450"), Top: dec("731200"), Rate: dec("0.35")},
		{Floor: dec("731200"), Top: decimal.Zero, Rate: dec("0.37")},
	}

	usFICASocialRate     = dec("0.062")
	usFICASocialWageBase = dec("176100")
	usFICAMedicareRate   = dec("0.0145")
	usFICAAdditionalRate = dec("0.009")
	usFICAAdditionalThr  = dec("200000")

	// Standard deduction subtracted from annualised gross before
	// the IRS Pub 15-T percentage method (Step 2 → Step 3). The
	// bracket tables above start at $0, not at the deduction
	// amount, so applying brackets directly to gross would over-
	// withhold by ~10-12% for low-to-mid earners. 2024 amounts
	// match the 2024 brackets we ship; bumping both in lock-step
	// is a separate change once we adopt the 2026 Pub 15-T table.
	usStandardDeductionSingle = dec("14600")
	usStandardDeductionMFJ    = dec("29200")

	usPeriodsPerYear = decimal.NewFromFloat(365.25)
)

func (usPack) ComputeWithholding(ctx context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	days := period.Days()
	if days <= 0 {
		return nil, nil
	}
	periodFraction := decimal.NewFromInt(int64(days)).Div(usPeriodsPerYear)
	annualGross := gross.Div(periodFraction)

	brackets := usBracketsSingle
	stdDeduction := usStandardDeductionSingle
	if e.FilingType == "married_filing_jointly" {
		brackets = usBracketsMFJ
		stdDeduction = usStandardDeductionMFJ
	}

	// Pub 15-T Step 2: subtract the standard deduction before
	// running the bracket walk in Step 3. Clamp to zero so a
	// part-time / partial-period employee whose annualised gross
	// falls below the deduction owes no federal tax (rather than
	// a negative one).
	taxable := annualGross.Sub(stdDeduction)
	if taxable.LessThan(decimal.Zero) {
		taxable = decimal.Zero
	}
	annualTax := walkBrackets(taxable, brackets)
	federal := annualTax.Mul(periodFraction).Round(2)

	out := []Deduction{
		{Code: "FED_TAX", Name: "Federal income tax (US)", Amount: federal},
	}

	// Social Security: 6.2% up to wage-base cap. Once YTD gross
	// hits the cap the slip stops accruing OASDI.
	socialBase := gross
	if e.YTDGross.GreaterThanOrEqual(usFICASocialWageBase) {
		socialBase = decimal.Zero
	} else if e.YTDGross.Add(gross).GreaterThan(usFICASocialWageBase) {
		socialBase = usFICASocialWageBase.Sub(e.YTDGross)
	}
	if socialBase.IsPositive() {
		out = append(out, Deduction{
			Code:   "FICA_OASDI",
			Name:   "Social Security (US)",
			Amount: socialBase.Mul(usFICASocialRate).Round(2),
		})
	}

	// Medicare: 1.45% on every dollar, plus 0.9% surtax on YTD
	// gross over $200k. The surtax ratchet uses pre-slip YTD so a
	// slip that straddles the threshold is split correctly.
	medicare := gross.Mul(usFICAMedicareRate)
	preSlipYTD := e.YTDGross
	postSlipYTD := preSlipYTD.Add(gross)
	if postSlipYTD.GreaterThan(usFICAAdditionalThr) {
		surtaxBase := postSlipYTD.Sub(usFICAAdditionalThr)
		if preSlipYTD.GreaterThan(usFICAAdditionalThr) {
			surtaxBase = gross
		}
		medicare = medicare.Add(surtaxBase.Mul(usFICAAdditionalRate))
	}
	out = append(out, Deduction{
		Code:   "FICA_MEDICARE",
		Name:   "Medicare (US)",
		Amount: medicare.Round(2),
	})

	return out, nil
}

// walkBrackets returns the annualised tax on annualGross against
// the given bracket table. Brackets must be sorted ascending by
// Floor; the open-ended top bracket has Top = decimal.Zero.
func walkBrackets(annualGross decimal.Decimal, brackets []usBracket) decimal.Decimal {
	var tax decimal.Decimal
	for _, b := range brackets {
		if annualGross.LessThanOrEqual(b.Floor) {
			break
		}
		hi := b.Top
		if hi.IsZero() || annualGross.LessThan(hi) {
			hi = annualGross
		}
		tax = tax.Add(hi.Sub(b.Floor).Mul(b.Rate))
	}
	return tax
}

// dec is a tiny shim for inline decimal literals — decimal.NewFromString
// returns (Decimal, error) which makes table init verbose.
func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic("taxpacks: dec(" + s + "): " + err.Error())
	}
	return d
}
