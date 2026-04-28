package notifications

import (
	"encoding/json"
	"testing"
)

func TestEvaluateConditionsEmptyAlwaysMatches(t *testing.T) {
	payload := json.RawMessage(`{"ktype":"helpdesk.ticket","status":"open"}`)
	cases := []struct {
		name string
		cond json.RawMessage
	}{
		{"nil", nil},
		{"empty", json.RawMessage("")},
		{"empty object", json.RawMessage("{}")},
		{"explicit null", json.RawMessage("null")},
		{"whitespace", json.RawMessage("   {}  ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !EvaluateConditions(tc.cond, payload) {
				t.Fatalf("expected match for empty condition")
			}
		})
	}
}

func TestEvaluateConditionsScalarEquality(t *testing.T) {
	payload := json.RawMessage(`{"ktype":"helpdesk.ticket","data":{"status":"open","priority":3}}`)
	if !EvaluateConditions(json.RawMessage(`{"ktype":"helpdesk.ticket"}`), payload) {
		t.Fatal("ktype eq match expected")
	}
	if EvaluateConditions(json.RawMessage(`{"ktype":"sales.invoice"}`), payload) {
		t.Fatal("ktype mismatch should not match")
	}
	if !EvaluateConditions(json.RawMessage(`{"data.status":"open"}`), payload) {
		t.Fatal("dotted path match expected")
	}
	if !EvaluateConditions(json.RawMessage(`{"data.priority":3}`), payload) {
		t.Fatal("numeric eq match expected (json number normalisation)")
	}
}

func TestEvaluateConditionsOperators(t *testing.T) {
	payload := json.RawMessage(`{"ktype":"helpdesk.ticket","tags":["urgent","p0"]}`)
	if !EvaluateConditions(json.RawMessage(`{"ktype":{"$in":["helpdesk.ticket","sales.invoice"]}}`), payload) {
		t.Fatal("$in match expected")
	}
	if EvaluateConditions(json.RawMessage(`{"ktype":{"$in":["sales.invoice"]}}`), payload) {
		t.Fatal("$in mismatch should not match")
	}
	if !EvaluateConditions(json.RawMessage(`{"ktype":{"$prefix":"helpdesk."}}`), payload) {
		t.Fatal("$prefix match expected")
	}
	if EvaluateConditions(json.RawMessage(`{"ktype":{"$prefix":"sales."}}`), payload) {
		t.Fatal("$prefix mismatch should not match")
	}
	if !EvaluateConditions(json.RawMessage(`{"tags.0":{"$eq":"urgent"}}`), payload) {
		t.Fatal("array index $eq match expected")
	}
	if !EvaluateConditions(json.RawMessage(`{"tags.0":{"$exists":true}}`), payload) {
		t.Fatal("$exists true match expected for present path")
	}
	if !EvaluateConditions(json.RawMessage(`{"missing":{"$exists":false}}`), payload) {
		t.Fatal("$exists false match expected for absent path")
	}
}

func TestEvaluateConditionsAndCombination(t *testing.T) {
	payload := json.RawMessage(`{"ktype":"helpdesk.ticket","data":{"status":"open"}}`)
	cond := json.RawMessage(`{"ktype":"helpdesk.ticket","data.status":"open"}`)
	if !EvaluateConditions(cond, payload) {
		t.Fatal("AND: both keys match expected")
	}
	failing := json.RawMessage(`{"ktype":"helpdesk.ticket","data.status":"closed"}`)
	if EvaluateConditions(failing, payload) {
		t.Fatal("AND: one key mismatch should fail closed")
	}
}

func TestEvaluateConditionsFailsClosedOnMalformed(t *testing.T) {
	payload := json.RawMessage(`{"ktype":"helpdesk.ticket"}`)
	if EvaluateConditions(json.RawMessage(`{not valid json`), payload) {
		t.Fatal("malformed condition should fail closed")
	}
	if EvaluateConditions(json.RawMessage(`{"ktype":"helpdesk.ticket"}`), json.RawMessage(`{not valid`)) {
		t.Fatal("malformed payload should fail closed")
	}
}

func TestEvaluateConditionsUnknownOperatorFailsClosed(t *testing.T) {
	payload := json.RawMessage(`{"data":{"status":"open"}}`)
	// $prefxi is a typo of $prefix — must NOT silently degrade to
	// "no filter" semantics. Same for any unrecognized operator
	// key (forwards-compat: server may not yet ship a new $regex).
	cases := []string{
		`{"data.status": {"$prefxi": "op"}}`,
		`{"data.status": {"$regex": "open"}}`,
		`{"data.status": {"$eq": "open", "$unknown": true}}`,
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if EvaluateConditions(json.RawMessage(c), payload) {
				t.Fatalf("unknown operator must fail closed: %s", c)
			}
		})
	}
}
