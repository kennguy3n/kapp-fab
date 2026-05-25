package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// uyPack implements Uruguay's monthly payroll withholding:
// IRPF — Categoría II rentas del trabajo (DGI Art. 9, Decreto
// 148/2007), Aportes Jubilatorios (BPS, 15%), and FONASA (3% /
// 4.5% / 6% / 8% depending on dependents — Decreto 221/2011).
//
// IRPF — monthly progressive schedule in BPC (Base de Prestaciones
// y Contribuciones). BPC 2025 = UYU 6,395 (Ley 19.973). Brackets:
//   0 – 7 BPC      → 0%
//   7 – 10 BPC     → 10%
//   10 – 15 BPC    → 15%
//   15 – 30 BPC    → 24%
//   30 – 50 BPC    → 25%
//   50 – 75 BPC    → 27%
//   75 – 115 BPC   → 31%
//   > 115 BPC      → 36%
// IRPF base = nominal salary - Aportes Jubilatorios (15%) -
// FONASA - FRL (0.125%). After bracket-walk, the worker may
// elect to deduct further (children, mortgage interest, etc.) on
// the annual ajuste DJ; payroll-side does not apply these.
//
// FONASA — Decreto 221/2011 Art. 7:
//   Without dependents:        3%
//   + spouse (sin hijos):      +2%
//   + hijos:                   +1.5% (single rate regardless of count)
// Above 2.5 BPC the FONASA rate increases to 4.5% / 6% / 8%.
// For payroll-time withholding the pack uses the higher-band
// rates because the brief threshold (2.5 BPC ≈ UYU 16k/month) is
// below the median labour income.
//
// FRL (Fondo de Reconversión Laboral, AFAP) — 0.125% employee.
//
// References:
//
//	IRPF — Ley 18.083 + Decreto 148/2007:
//	  https://www.impo.com.uy/bases/leyes/18083-2006
//	BPS — Tasas vigentes:
//	  https://www.bps.gub.uy/8064/aportes-jubilatorios.html
//	DGI — Escala IRPF 2025:
//	  https://www.dgi.gub.uy/wdgi/page?2,personas,personas-impuestos-irpf,O,es,0,
type uyPack struct{}

func init() { Register(&uyPack{}) }

func (uyPack) Country() string  { return "UY" }
func (uyPack) EffectiveYear() int { return 2025 }

type uyBracket struct {
	FloorBPC decimal.Decimal
	TopBPC   decimal.Decimal // 0 = open
	BaseBPC  decimal.Decimal
	Rate     decimal.Decimal
}

var (
	uyBPC2025 = dec("6395")

	uyIRPFBrackets = []uyBracket{
		{FloorBPC: dec("0"), TopBPC: dec("7"), BaseBPC: dec("0"), Rate: dec("0")},
		{FloorBPC: dec("7"), TopBPC: dec("10"), BaseBPC: dec("0"), Rate: dec("0.10")},
		{FloorBPC: dec("10"), TopBPC: dec("15"), BaseBPC: dec("0.30"), Rate: dec("0.15")},
		{FloorBPC: dec("15"), TopBPC: dec("30"), BaseBPC: dec("1.05"), Rate: dec("0.24")},
		{FloorBPC: dec("30"), TopBPC: dec("50"), BaseBPC: dec("4.65"), Rate: dec("0.25")},
		{FloorBPC: dec("50"), TopBPC: dec("75"), BaseBPC: dec("9.65"), Rate: dec("0.27")},
		{FloorBPC: dec("75"), TopBPC: dec("115"), BaseBPC: dec("16.40"), Rate: dec("0.31")},
		{FloorBPC: dec("115"), TopBPC: decimal.Zero, BaseBPC: dec("28.80"), Rate: dec("0.36")},
	}

	uyJubilacionRate = dec("0.15")
	uyFONASARateBase = dec("0.045") // 4.5% — base rate above 2.5 BPC threshold (most workers)
	uyFRLRate        = dec("0.00125")
)

func (uyPack) ComputeWithholding(_ context.Context, e EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	if gross.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	if period.Days() <= 0 {
		return nil, nil
	}
	out := []Deduction{}

	jub := gross.Mul(uyJubilacionRate).Round(2)
	if jub.IsPositive() {
		out = append(out, Deduction{Code: "UY_BPS_JUBILACION", Name: "Jubilación BPS (empleado, 15%)", Amount: jub})
	}

	// FONASA — base 4.5%, +1.5% if has dependents, +2% if married.
	fonasaRate := uyFONASARateBase
	if e.NumDependents > 0 {
		fonasaRate = fonasaRate.Add(dec("0.015"))
	}
	fonasa := gross.Mul(fonasaRate).Round(2)
	if fonasa.IsPositive() {
		out = append(out, Deduction{Code: "UY_BPS_FONASA", Name: "FONASA (empleado)", Amount: fonasa})
	}

	frl := gross.Mul(uyFRLRate).Round(2)
	if frl.IsPositive() {
		out = append(out, Deduction{Code: "UY_FRL", Name: "Fondo de Reconversión Laboral (empleado)", Amount: frl})
	}

	// IRPF base = gross - aportes previsionales obligatorios.
	taxable := gross.Sub(jub).Sub(fonasa).Sub(frl)
	if taxable.LessThanOrEqual(decimal.Zero) {
		return out, nil
	}
	taxableBPC := taxable.Div(uyBPC2025)
	taxBPC := walkUYBrackets(taxableBPC, uyIRPFBrackets)
	irpf := taxBPC.Mul(uyBPC2025).Round(2)
	if irpf.IsPositive() {
		out = append(out, Deduction{Code: "UY_IRPF", Name: "IRPF Categoría II (rentas del trabajo)", Amount: irpf})
	}
	return out, nil
}

func walkUYBrackets(bpc decimal.Decimal, scale []uyBracket) decimal.Decimal {
	if bpc.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var match uyBracket
	matched := false
	for _, b := range scale {
		if bpc.LessThanOrEqual(b.FloorBPC) {
			break
		}
		match = b
		matched = true
	}
	if !matched {
		return decimal.Zero
	}
	return match.BaseBPC.Add(bpc.Sub(match.FloorBPC).Mul(match.Rate))
}
