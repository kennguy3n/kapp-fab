package ktype

import (
	"encoding/json"
	"testing"
)

// TestContentHash_Deterministic asserts that contentHash produces stable
// output across calls regardless of map iteration order (the function
// canonicalizes JSON keys via sort).
func TestContentHash_Deterministic(t *testing.T) {
	kt := KType{
		Name:    "test_ktype",
		Version: 1,
		Schema:  json.RawMessage(`{"z_field":"string","a_field":"number","m_field":{"nested_z":"bool","nested_a":"int"}}`),
	}
	h1 := contentHash(kt)
	h2 := contentHash(kt)
	if h1 != h2 {
		t.Fatalf("contentHash is non-deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char hex SHA-256, got len=%d: %q", len(h1), h1)
	}
}

// TestContentHash_DiffersOnSchemaChange proves that a schema modification
// produces a different hash so RegisterIfChanged detects the change.
func TestContentHash_DiffersOnSchemaChange(t *testing.T) {
	kt1 := KType{
		Name:    "test_ktype",
		Version: 1,
		Schema:  json.RawMessage(`{"field":"string"}`),
	}
	kt2 := KType{
		Name:    "test_ktype",
		Version: 1,
		Schema:  json.RawMessage(`{"field":"number"}`),
	}
	if contentHash(kt1) == contentHash(kt2) {
		t.Fatalf("contentHash should differ for different schemas")
	}
}

// TestContentHash_DiffersOnVersionChange proves that version changes produce
// different hashes (even if schema is identical).
func TestContentHash_DiffersOnVersionChange(t *testing.T) {
	schema := json.RawMessage(`{"field":"string"}`)
	kt1 := KType{Name: "test", Version: 1, Schema: schema}
	kt2 := KType{Name: "test", Version: 2, Schema: schema}
	if contentHash(kt1) == contentHash(kt2) {
		t.Fatalf("contentHash should differ for different versions")
	}
}

// TestCanonicalJSON_SortsKeys verifies that canonicalJSON produces key-sorted
// output regardless of input order.
func TestCanonicalJSON_SortsKeys(t *testing.T) {
	input := map[string]json.RawMessage{
		"zebra": json.RawMessage(`1`),
		"alpha": json.RawMessage(`2`),
		"mid":   json.RawMessage(`3`),
	}
	got := string(canonicalJSON(input))
	want := `{"alpha":2,"mid":3,"zebra":1}`
	if got != want {
		t.Fatalf("canonicalJSON:\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalJSON_NestedObjects verifies recursive canonicalization.
func TestCanonicalJSON_NestedObjects(t *testing.T) {
	input := map[string]json.RawMessage{
		"b": json.RawMessage(`{"zz":1,"aa":2}`),
		"a": json.RawMessage(`"leaf"`),
	}
	got := string(canonicalJSON(input))
	want := `{"a":"leaf","b":{"aa":2,"zz":1}}`
	if got != want {
		t.Fatalf("canonicalJSON nested:\n got: %s\nwant: %s", got, want)
	}
}
