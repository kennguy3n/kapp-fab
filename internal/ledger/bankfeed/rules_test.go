package bankfeed

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func ruleWith(ct, val string, act func(*Rule)) Rule {
	r := Rule{ConditionType: ct, ConditionValue: val, TargetAccountCode: "6000"}
	if act != nil {
		act(&r)
	}
	return r
}

func TestRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"contains ok", ruleWith(CondDescriptionContains, "uber", nil), false},
		{"contains empty value", ruleWith(CondDescriptionContains, "  ", nil), true},
		{"regex ok", ruleWith(CondDescriptionRegex, `^AWS.*`, nil), false},
		{"regex invalid", ruleWith(CondDescriptionRegex, `([`, nil), true},
		{"amount ok", ruleWith(CondAmountRange, "10:100", nil), false},
		{"amount bad", ruleWith(CondAmountRange, "abc", nil), true},
		{"counterparty ok", ruleWith(CondCounterparty, "acme", nil), false},
		{"unknown type", ruleWith("nope", "x", nil), true},
		{"no action", ruleWith(CondDescriptionContains, "x", func(r *Rule) {
			r.TargetAccountCode = ""
			r.TargetCostCenter = ""
			r.AutoApprove = false
		}), true},
		{"auto_approve only is a valid action", ruleWith(CondDescriptionContains, "x", func(r *Rule) {
			r.TargetAccountCode = ""
			r.AutoApprove = true
		}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v; wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseAmountRange(t *testing.T) {
	d := func(s string) *decimal.Decimal { v := decimal.RequireFromString(s); return &v }
	cases := []struct {
		in     string
		lo, hi *decimal.Decimal
		err    bool
	}{
		{"10:100", d("10"), d("100"), false},
		{"100:", d("100"), nil, false},
		{":0", nil, d("0"), false},
		{"-50:50", d("-50"), d("50"), false},
		{"nocolon", nil, nil, true},
		{"x:1", nil, nil, true},
	}
	for _, tc := range cases {
		lo, hi, err := parseAmountRange(tc.in)
		if (err != nil) != tc.err {
			t.Errorf("%q err = %v; want err=%v", tc.in, err, tc.err)
			continue
		}
		if tc.err {
			continue
		}
		if (lo == nil) != (tc.lo == nil) || (lo != nil && !lo.Equal(*tc.lo)) {
			t.Errorf("%q lo = %v; want %v", tc.in, lo, tc.lo)
		}
		if (hi == nil) != (tc.hi == nil) || (hi != nil && !hi.Equal(*tc.hi)) {
			t.Errorf("%q hi = %v; want %v", tc.in, hi, tc.hi)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	txn := RawTransaction{
		Description:  "UBER *EATS 8829",
		Amount:       decimal.RequireFromString("-24.50"),
		Counterparty: "Uber Eats",
	}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"contains hit (case-insensitive)", ruleWith(CondDescriptionContains, "uber", nil), true},
		{"contains miss", ruleWith(CondDescriptionContains, "lyft", nil), false},
		{"regex hit", ruleWith(CondDescriptionRegex, `(?i)uber`, nil), true},
		{"counterparty hit", ruleWith(CondCounterparty, "uber eats", nil), true},
		{"amount in range", ruleWith(CondAmountRange, "-30:-20", nil), true},
		{"amount out of range", ruleWith(CondAmountRange, "0:100", nil), false},
		{"amount open lower", ruleWith(CondAmountRange, ":0", nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.matches(txn); got != tc.want {
				t.Fatalf("matches() = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestRuleCounterpartyFallsBackToDescription(t *testing.T) {
	txn := RawTransaction{Description: "ACME LTD PAYMENT"}
	r := ruleWith(CondCounterparty, "acme", nil)
	if !r.matches(txn) {
		t.Fatal("expected counterparty rule to fall back to description")
	}
}

func TestEvaluateFirstMatchWins(t *testing.T) {
	acc := uuid.New()
	rules := []Rule{
		ruleWith(CondDescriptionContains, "zzz", func(r *Rule) { r.Priority = 1; r.TargetAccountCode = "1000" }),
		ruleWith(CondDescriptionContains, "uber", func(r *Rule) { r.Priority = 2; r.TargetAccountCode = "6000"; r.BankAccountID = &acc }),
		ruleWith(CondDescriptionContains, "uber", func(r *Rule) { r.Priority = 3; r.TargetAccountCode = "6001"; r.AutoApprove = true }),
	}
	txn := RawTransaction{Description: "UBER trip"}
	m, ok := Evaluate(rules, txn)
	if !ok {
		t.Fatal("expected a match")
	}
	if m.TargetAccountCode != "6000" {
		t.Fatalf("account = %q; want first matching (6000)", m.TargetAccountCode)
	}
	if m.AutoApprove {
		t.Fatal("first match did not set auto_approve")
	}
}

func TestEvaluateNoMatch(t *testing.T) {
	rules := []Rule{ruleWith(CondDescriptionContains, "uber", nil)}
	if _, ok := Evaluate(rules, RawTransaction{Description: "grocery"}); ok {
		t.Fatal("expected no match")
	}
}

// compoundRule builds a compound rule with the given match mode and an
// account action so it passes validation.
func compoundRule(match string, conds ...RuleCondition) Rule {
	return Rule{
		Conditions:        conds,
		ConditionMatch:    match,
		TargetAccountCode: "6000",
	}
}

func TestCompoundRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"all text+amount ok", compoundRule(MatchAll,
			RuleCondition{FieldPayee, OpContains, "amazon"},
			RuleCondition{FieldAmount, OpGte, "100"}), false},
		{"any ok", compoundRule(MatchAny,
			RuleCondition{FieldReference, OpEquals, "INV-1"},
			RuleCondition{FieldDescription, OpRegex, `(?i)refund`}), false},
		{"empty match defaults ok", compoundRule("",
			RuleCondition{FieldDescription, OpContains, "x"}), false},
		{"bad match mode", compoundRule("most",
			RuleCondition{FieldDescription, OpContains, "x"}), true},
		{"text op on amount", compoundRule(MatchAll,
			RuleCondition{FieldAmount, OpContains, "10"}), true},
		{"amount op on text", compoundRule(MatchAll,
			RuleCondition{FieldPayee, OpGt, "10"}), true},
		{"bad regex", compoundRule(MatchAll,
			RuleCondition{FieldDescription, OpRegex, `([`}), true},
		{"bad amount range", compoundRule(MatchAll,
			RuleCondition{FieldAmount, OpRange, "abc"}), true},
		{"empty contains value", compoundRule(MatchAll,
			RuleCondition{FieldPayee, OpContains, "  "}), true},
		{"unknown field", compoundRule(MatchAll,
			RuleCondition{"memo", OpContains, "x"}), true},
		{"legacy type set with conditions", func() Rule {
			r := compoundRule(MatchAll, RuleCondition{FieldDescription, OpContains, "x"})
			r.ConditionType = CondDescriptionContains
			return r
		}(), true},
		{"tax code is a valid sole action", func() Rule {
			r := compoundRule(MatchAll, RuleCondition{FieldDescription, OpContains, "x"})
			r.TargetAccountCode = ""
			r.TargetTaxCode = "VAT20"
			return r
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.rule.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v; wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestCompoundRuleMatches(t *testing.T) {
	txn := RawTransaction{
		Description:  "AMAZON WEB SERVICES INV-4471",
		Counterparty: "Amazon",
		Reference:    "INV-4471",
		Amount:       decimal.RequireFromString("-128.40"),
	}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"all hit: payee + amount", compoundRule(MatchAll,
			RuleCondition{FieldPayee, OpContains, "amazon"},
			RuleCondition{FieldAmount, OpLte, "-100"}), true},
		{"all miss: one fails", compoundRule(MatchAll,
			RuleCondition{FieldPayee, OpContains, "amazon"},
			RuleCondition{FieldAmount, OpGt, "0"}), false},
		{"any hit: second matches", compoundRule(MatchAny,
			RuleCondition{FieldPayee, OpEquals, "stripe"},
			RuleCondition{FieldReference, OpContains, "INV-4471"}), true},
		{"any miss: none match", compoundRule(MatchAny,
			RuleCondition{FieldPayee, OpEquals, "stripe"},
			RuleCondition{FieldReference, OpEquals, "INV-0000"}), false},
		{"amount eq", compoundRule(MatchAll,
			RuleCondition{FieldAmount, OpEq, "-128.40"}), true},
		{"amount range inclusive", compoundRule(MatchAll,
			RuleCondition{FieldAmount, OpRange, "-200:-100"}), true},
		{"reference regex", compoundRule(MatchAll,
			RuleCondition{FieldReference, OpRegex, `^INV-\d+$`}), true},
		{"default match mode is all (miss)", compoundRule("",
			RuleCondition{FieldPayee, OpContains, "amazon"},
			RuleCondition{FieldReference, OpEquals, "nope"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.matches(txn); got != tc.want {
				t.Fatalf("matches() = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestCompoundReferenceFallsBackToDescription proves a reference/payee
// condition matches the description when the provider exposes no
// structured field, mirroring the legacy counterparty fallback.
func TestCompoundReferenceFallsBackToDescription(t *testing.T) {
	txn := RawTransaction{Description: "FPS REF ABC123"}
	r := compoundRule(MatchAll, RuleCondition{FieldReference, OpContains, "abc123"})
	if !r.matches(txn) {
		t.Fatal("expected reference condition to fall back to description")
	}
}

func TestEvaluateIndexedReturnsPosition(t *testing.T) {
	rules := []Rule{
		ruleWith(CondDescriptionContains, "zzz", func(r *Rule) { r.Priority = 1 }),
		ruleWith(CondDescriptionContains, "uber", func(r *Rule) { r.Priority = 2; r.TargetTaxCode = "VAT20" }),
	}
	m, idx, ok := EvaluateIndexed(rules, RawTransaction{Description: "UBER trip"})
	if !ok || idx != 1 {
		t.Fatalf("EvaluateIndexed = idx %d ok %v; want idx 1 ok true", idx, ok)
	}
	if m.TargetTaxCode != "VAT20" {
		t.Fatalf("tax code = %q; want VAT20", m.TargetTaxCode)
	}
	if _, idx, ok := EvaluateIndexed(rules, RawTransaction{Description: "grocery"}); ok || idx != -1 {
		t.Fatalf("no-match EvaluateIndexed = idx %d ok %v; want idx -1 ok false", idx, ok)
	}
}

func TestMarshalRoundTripConditions(t *testing.T) {
	in := []RuleCondition{
		{FieldPayee, OpContains, "amazon"},
		{FieldAmount, OpRange, "10:100"},
	}
	raw, err := marshalConditions(in)
	if err != nil {
		t.Fatalf("marshalConditions: %v", err)
	}
	out, err := unmarshalConditions(raw.([]byte))
	if err != nil {
		t.Fatalf("unmarshalConditions: %v", err)
	}
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("round trip = %+v; want %+v", out, in)
	}
	// Empty marshals to a SQL NULL (nil any), and a nil/empty column
	// unmarshals back to a nil slice (legacy path).
	if v, _ := marshalConditions(nil); v != nil {
		t.Fatalf("marshalConditions(nil) = %v; want nil", v)
	}
	if got, _ := unmarshalConditions(nil); got != nil {
		t.Fatalf("unmarshalConditions(nil) = %v; want nil", got)
	}
}
