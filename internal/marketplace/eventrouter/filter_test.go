package eventrouter

import "testing"

func TestFilterMatches_EmptyFilterAlwaysMatches(t *testing.T) {
	matched, err := filterMatches(nil, []byte(`{"foo": "bar"}`))
	if err != nil {
		t.Fatalf("nil filter unexpected err: %v", err)
	}
	if !matched {
		t.Fatalf("nil filter should match any payload; got false")
	}

	matched, err = filterMatches(map[string]string{}, []byte(`{"foo": "bar"}`))
	if err != nil {
		t.Fatalf("empty filter unexpected err: %v", err)
	}
	if !matched {
		t.Fatalf("empty filter should match any payload; got false")
	}
}

func TestFilterMatches_NonEmptyFilterRequiresPayload(t *testing.T) {
	matched, _ := filterMatches(map[string]string{"foo": "bar"}, nil)
	if matched {
		t.Fatalf("non-empty filter with nil payload should NOT match")
	}
	matched, _ = filterMatches(map[string]string{"foo": "bar"}, []byte{})
	if matched {
		t.Fatalf("non-empty filter with empty payload should NOT match")
	}
}

func TestFilterMatches_TopLevelEquality(t *testing.T) {
	payload := []byte(`{"status": "posted", "tenant": "abc"}`)
	matched, _ := filterMatches(map[string]string{"status": "posted"}, payload)
	if !matched {
		t.Fatalf("expected match on top-level string equality")
	}
	matched, _ = filterMatches(map[string]string{"status": "draft"}, payload)
	if matched {
		t.Fatalf("expected no-match on top-level string inequality")
	}
}

func TestFilterMatches_NumericStringification(t *testing.T) {
	// JSON 42 → "42", not "42.0" or "42.000000".
	payload := []byte(`{"count": 42}`)
	matched, _ := filterMatches(map[string]string{"count": "42"}, payload)
	if !matched {
		t.Fatalf("integer 42 should stringify to \"42\" for equality match")
	}
	// Float values that aren't whole numbers preserve decimal.
	payload = []byte(`{"price": 3.14}`)
	matched, _ = filterMatches(map[string]string{"price": "3.14"}, payload)
	if !matched {
		t.Fatalf("float 3.14 should stringify to \"3.14\"")
	}
}

func TestFilterMatches_BoolStringification(t *testing.T) {
	payload := []byte(`{"approved": true, "rejected": false}`)
	matched, _ := filterMatches(map[string]string{"approved": "true"}, payload)
	if !matched {
		t.Fatalf("bool true should stringify to \"true\"")
	}
	matched, _ = filterMatches(map[string]string{"rejected": "false"}, payload)
	if !matched {
		t.Fatalf("bool false should stringify to \"false\"")
	}
}

func TestFilterMatches_DottedPath(t *testing.T) {
	payload := []byte(`{"record": {"status": "posted", "amount": 100}, "tenant": "abc"}`)
	matched, _ := filterMatches(map[string]string{
		"record.status": "posted",
		"record.amount": "100",
	}, payload)
	if !matched {
		t.Fatalf("dotted-path match should succeed")
	}
	matched, _ = filterMatches(map[string]string{
		"record.status": "draft",
	}, payload)
	if matched {
		t.Fatalf("dotted-path mismatch should reject")
	}
}

func TestFilterMatches_DottedPathMissingIntermediateRejects(t *testing.T) {
	// A filter on `record.status` against a payload that has
	// no `record` key should NOT match (we don't treat
	// missing-as-empty-string).
	payload := []byte(`{"tenant": "abc"}`)
	matched, _ := filterMatches(map[string]string{
		"record.status": "posted",
	}, payload)
	if matched {
		t.Fatalf("missing intermediate object should NOT match")
	}
}

func TestFilterMatches_MultipleConjunctive(t *testing.T) {
	// Filter map is AND: every key must match.
	payload := []byte(`{"a": "1", "b": "2", "c": "3"}`)
	matched, _ := filterMatches(map[string]string{
		"a": "1",
		"b": "2",
	}, payload)
	if !matched {
		t.Fatalf("AND match should succeed when both keys match")
	}
	matched, _ = filterMatches(map[string]string{
		"a": "1",
		"b": "WRONG",
	}, payload)
	if matched {
		t.Fatalf("AND match should fail when one key mismatches")
	}
}

func TestFilterMatches_MalformedPayloadReturnsError(t *testing.T) {
	matched, err := filterMatches(map[string]string{"foo": "bar"}, []byte(`{not json`))
	if err == nil {
		t.Fatalf("expected unmarshal error on malformed payload, got nil")
	}
	if matched {
		t.Fatalf("matched should be false on error")
	}
}
