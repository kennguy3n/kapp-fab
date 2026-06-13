package hr

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr/taxpacks"
)

// The ordered calculation pipeline (D1 + D2). Pure functions: given a
// salary structure, the variable pay inputs, the employee's join date and
// the pay period, it produces the earning / pre-tax-deduction /
// post-tax-deduction lines and the gross + taxable_gross figures. The
// statutory tax step (D3) is intentionally NOT here — it depends on the
// persisted YTD and runs inside FinalizePayslip's transaction.
//
// Order of operations:
//
//	gross   = Σ earnings          (prorated recurring + LOP dock + inputs)
//	taxable = Σ taxable earnings − Σ pre-tax deductions
//	         (computed BEFORE withholding so packs see the reduced base)
//	post-tax deductions are emitted but do not affect taxable.
//	net (computed later) = gross − pretax − tax − posttax.

// pipelineResult is the D1/D2 output handed to FinalizePayslip.
type pipelineResult struct {
	Earnings          []PayslipLine
	PretaxDeductions  []PayslipLine
	PosttaxDeductions []PayslipLine
	Gross             decimal.Decimal
	TaxableGross      decimal.Decimal
	// ContributionGross is the social-security / contribution base: the
	// full gross minus only those pre-tax deductions flagged to also
	// reduce the contribution base (Section-125 style). Pre-tax
	// deductions that reduce income tax only (the 401(k) case) leave it
	// equal to the full gross.
	ContributionGross decimal.Decimal
	ProrationFactor   decimal.Decimal
}

// twoPlaces rounds money to 2 dp, the convention used throughout the
// payroll engine (matches rollStructure's percentage rounding).
func twoPlaces(d decimal.Decimal) decimal.Decimal {
	return d.Round(2)
}

// prorationFactor returns the employed fraction of the period for an
// employee whose date_of_joining may fall inside it. Calendar-day basis:
// factor = employedDays / totalDays, clamped to [0, 1]. A join date on or
// before the period start (or empty) yields 1 (full period); a join date
// after the period end yields 0.
func prorationFactor(joinDate time.Time, period taxpacks.PayPeriod) decimal.Decimal {
	start := period.Start.UTC().Truncate(24 * time.Hour)
	end := period.End.UTC().Truncate(24 * time.Hour)
	totalDays := int(end.Sub(start).Hours()/24) + 1
	if totalDays <= 0 {
		return decimal.NewFromInt(1)
	}
	if joinDate.IsZero() {
		return decimal.NewFromInt(1)
	}
	join := joinDate.UTC().Truncate(24 * time.Hour)
	if !join.After(start) {
		return decimal.NewFromInt(1)
	}
	if join.After(end) {
		return decimal.Zero
	}
	employedDays := int(end.Sub(join).Hours()/24) + 1
	if employedDays < 0 {
		employedDays = 0
	}
	if employedDays > totalDays {
		employedDays = totalDays
	}
	return decimal.NewFromInt(int64(employedDays)).
		Div(decimal.NewFromInt(int64(totalDays)))
}

// periodDays counts the calendar days in a (inclusive) pay period.
func periodDays(period taxpacks.PayPeriod) int {
	start := period.Start.UTC().Truncate(24 * time.Hour)
	end := period.End.UTC().Truncate(24 * time.Hour)
	d := int(end.Sub(start).Hours()/24) + 1
	if d <= 0 {
		return 1
	}
	return d
}

// monthsEmployedYTD counts months from the tax-year start (or the join
// month, whichever is later) through the period-end month, inclusive.
// Feeds the cumulative-withholding packs (e.g. CN) a real month index
// derived from persisted data rather than a static employee field.
func monthsEmployedYTD(joinDate, periodEnd time.Time, taxYear int) int {
	endMonth := int(periodEnd.UTC().Month())
	startMonth := 1
	if !joinDate.IsZero() {
		j := joinDate.UTC()
		if j.Year() == taxYear && int(j.Month()) > 1 {
			startMonth = int(j.Month())
		}
		if j.Year() > taxYear {
			return 0
		}
	}
	n := endMonth - startMonth + 1
	if n < 1 {
		return 1
	}
	return n
}

// resolveComponentAmount applies the structure's override + percentage
// rules, mirroring rollStructure so the typed pipeline and the legacy
// roll agree on the per-component figure.
func resolveComponentAmount(c *structureComponent, base decimal.Decimal) decimal.Decimal {
	amt := c.OverrideAmount
	if !amt.IsPositive() {
		amt = c.Amount
	}
	amountType := c.OverrideAmountType
	if amountType == "" {
		amountType = c.AmountType
	}
	if amountType == "percentage" {
		amt = base.Mul(amt).Div(decimal.NewFromInt(100)).Round(2)
	}
	return amt
}

// resolveInputAmount returns the money figure for a variable input,
// deriving qty*rate when the explicit amount is zero.
func resolveInputAmount(in *PayInput) decimal.Decimal {
	if !in.Amount.IsZero() {
		return in.Amount
	}
	if !in.Qty.IsZero() && !in.Rate.IsZero() {
		return twoPlaces(in.Qty.Mul(in.Rate))
	}
	return decimal.Zero
}

// buildPipeline runs D1 (variable inputs, proration, LOP) and D2 (pre-tax
// vs post-tax classification, taxable base) for one employee.
func buildPipeline(sv structureData, inputs []PayInput, joinDate time.Time, period taxpacks.PayPeriod) pipelineResult {
	base := sv.BaseSalary
	factor := prorationFactor(joinDate, period)
	prorated := !factor.Equal(decimal.NewFromInt(1))

	var earnings, pretax, posttax []PayslipLine
	var recurringUnprorated decimal.Decimal
	// contribReducers holds the pre-tax deduction codes flagged to also
	// reduce the contribution base, so D2 can subtract exactly those.
	contribReducers := map[string]bool{}

	// --- D1: recurring structure earnings, prorated for joiners. -------
	addEarning := func(code, label string, full decimal.Decimal, taxable bool) {
		recurringUnprorated = recurringUnprorated.Add(full)
		amt := full
		var rate *decimal.Decimal
		var baseAmt *decimal.Decimal
		if prorated {
			amt = twoPlaces(full.Mul(factor))
			f := factor
			b := full
			rate = &f
			baseAmt = &b
		}
		earnings = append(earnings, PayslipLine{
			Code:    code,
			Label:   label,
			Amount:  amt,
			Base:    baseAmt,
			Rate:    rate,
			Taxable: taxable,
		})
	}

	if base.IsPositive() {
		addEarning("BASE", "Base Salary", base, true)
	}
	for i := range sv.Components {
		c := &sv.Components[i]
		amt := resolveComponentAmount(c, base)
		switch c.Type {
		case "deduction":
			// Deductions are fixed statutory/benefit obligations — not
			// prorated. Classified D2 below.
			line := PayslipLine{Code: c.Code, Label: c.Name, Amount: amt, Taxable: false}
			if c.PreTax {
				pretax = append(pretax, line)
				if c.PreTaxReducesContributionBase {
					contribReducers[c.Code] = true
				}
			} else {
				posttax = append(posttax, line)
			}
		default:
			taxable := c.Taxable == nil || *c.Taxable
			addEarning(c.Code, c.Name, amt, taxable)
		}
	}

	// --- D1: loss-of-pay dock (reduces gross and taxable). -------------
	var lopDays decimal.Decimal
	for i := range inputs {
		if inputs[i].Type == PayInputLOPDays {
			lopDays = lopDays.Add(inputs[i].Qty)
		}
	}
	if lopDays.IsPositive() && recurringUnprorated.IsPositive() {
		perDay := recurringUnprorated.Div(decimal.NewFromInt(int64(periodDays(period))))
		lopAmount := twoPlaces(perDay.Mul(lopDays))
		days := lopDays
		earnings = append(earnings, PayslipLine{
			Code:    "LOP",
			Label:   "Loss of pay (unpaid leave)",
			Amount:  lopAmount.Neg(),
			Base:    &perDay,
			Rate:    &days,
			Taxable: true,
		})
	}

	// --- D1: additive variable inputs (bonus / overtime / etc.). -------
	for i := range inputs {
		in := &inputs[i]
		switch in.Type {
		case PayInputLOPDays:
			continue
		case PayInputBonus, PayInputOvertime, PayInputHours, PayInputReimbursement, PayInputAdjustment:
			amt := resolveInputAmount(in)
			if amt.IsZero() {
				continue
			}
			code := in.Code
			if code == "" {
				code = inputDefaultCode(in.Type)
			}
			label := in.Label
			if label == "" {
				label = inputDefaultLabel(in.Type)
			}
			line := PayslipLine{
				Code:    code,
				Label:   label,
				Amount:  amt,
				Taxable: in.Taxable,
			}
			if !in.Qty.IsZero() {
				q := in.Qty
				line.Base = &q
			}
			if !in.Rate.IsZero() {
				r := in.Rate
				line.Rate = &r
			}
			earnings = append(earnings, line)
		}
	}

	// --- D2: gross + taxable base + contribution base. ----------------
	// Income tax runs on the taxable base (gross minus every pre-tax
	// deduction); contributions run on the contribution base (gross minus
	// only the pre-tax deductions explicitly flagged to reduce it).
	var gross, taxable, contribution decimal.Decimal
	for _, e := range earnings {
		gross = gross.Add(e.Amount)
		if e.Taxable {
			taxable = taxable.Add(e.Amount)
		}
	}
	contribution = gross
	for _, p := range pretax {
		taxable = taxable.Sub(p.Amount)
		if contribReducers[p.Code] {
			contribution = contribution.Sub(p.Amount)
		}
	}
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	if contribution.IsNegative() {
		contribution = decimal.Zero
	}

	return pipelineResult{
		Earnings:          earnings,
		PretaxDeductions:  pretax,
		PosttaxDeductions: posttax,
		Gross:             twoPlaces(gross),
		TaxableGross:      twoPlaces(taxable),
		ContributionGross: twoPlaces(contribution),
		ProrationFactor:   factor,
	}
}

func inputDefaultCode(t string) string {
	switch t {
	case PayInputBonus:
		return "BONUS"
	case PayInputOvertime:
		return "OVERTIME"
	case PayInputHours:
		return "HOURS"
	case PayInputReimbursement:
		return "REIMBURSEMENT"
	case PayInputAdjustment:
		return "ADJUSTMENT"
	default:
		return "INPUT"
	}
}

func inputDefaultLabel(t string) string {
	switch t {
	case PayInputBonus:
		return "Bonus"
	case PayInputOvertime:
		return "Overtime"
	case PayInputHours:
		return "Hours"
	case PayInputReimbursement:
		return "Reimbursement"
	case PayInputAdjustment:
		return "Adjustment"
	default:
		return "Input"
	}
}
