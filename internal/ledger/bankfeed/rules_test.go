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
