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

// TestCanonicalJSONValue_SortsObjectKeys verifies that top-level object keys
// are sorted lexicographically regardless of input order.
func TestCanonicalJSONValue_SortsObjectKeys(t *testing.T) {
	input := json.RawMessage(`{"zebra":1,"alpha":2,"mid":3}`)
	got := string(canonicalJSONValue(input))
	want := `{"alpha":2,"mid":3,"zebra":1}`
	if got != want {
		t.Fatalf("canonicalJSONValue:\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalJSONValue_NestedObjects verifies recursive canonicalization
// of nested objects.
func TestCanonicalJSONValue_NestedObjects(t *testing.T) {
	input := json.RawMessage(`{"b":{"zz":1,"aa":2},"a":"leaf"}`)
	got := string(canonicalJSONValue(input))
	want := `{"a":"leaf","b":{"aa":2,"zz":1}}`
	if got != want {
		t.Fatalf("canonicalJSONValue nested:\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalJSONValue_ObjectsInsideArrays verifies that objects nested
// inside arrays (e.g. the per-field schema objects inside a KType's "fields"
// array) get their keys sorted too. This was the Devin Review finding
// against the earlier object-only canonicalization.
func TestCanonicalJSONValue_ObjectsInsideArrays(t *testing.T) {
	// An array of two objects, each with keys in different orders.
	// Expected: each object's keys are sorted, array element order is
	// preserved.
	input := json.RawMessage(`[{"z":1,"a":2},{"b":"x","a":"y"}]`)
	got := string(canonicalJSONValue(input))
	want := `[{"a":2,"z":1},{"a":"y","b":"x"}]`
	if got != want {
		t.Fatalf("canonicalJSONValue arrays-of-objects:\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalJSONValue_DeepNesting walks a realistic schema-like shape:
// objects nested inside arrays nested inside objects. Every nested object
// must be sorted.
func TestCanonicalJSONValue_DeepNesting(t *testing.T) {
	input := json.RawMessage(`{"fields":[{"name":"x","type":"int"},{"type":"str","name":"y"}],"version":1}`)
	got := string(canonicalJSONValue(input))
	want := `{"fields":[{"name":"x","type":"int"},{"name":"y","type":"str"}],"version":1}`
	if got != want {
		t.Fatalf("canonicalJSONValue deep:\n got: %s\nwant: %s", got, want)
	}
}

// TestCanonicalJSONValue_PrimitivesUnchanged verifies that non-container
// JSON values pass through unchanged.
func TestCanonicalJSONValue_PrimitivesUnchanged(t *testing.T) {
	cases := []string{`"hello"`, `42`, `true`, `false`, `null`}
	for _, c := range cases {
		got := string(canonicalJSONValue(json.RawMessage(c)))
		if got != c {
			t.Errorf("canonicalJSONValue(%q) = %q, want %q", c, got, c)
		}
	}
}
