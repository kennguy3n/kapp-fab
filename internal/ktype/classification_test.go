package ktype

import (
	"encoding/json"
	"testing"
)

// TestValidateSchemaClassification verifies the registration-time gate
// for the data-classification posture (P1-6 in
// docs/SECURITY_HARDENING_PLAN.md): allowed classification values pass,
// unknown values are rejected.
func TestValidateSchemaClassification(t *testing.T) {
	t.Run("allowed classifications pass", func(t *testing.T) {
		schema := json.RawMessage(`{
			"name":"employee","version":1,
			"fields":[
				{"name":"name","type":"string","classification":"public"},
				{"name":"notes","type":"string","classification":"internal"},
				{"name":"salary","type":"string","classification":"confidential"},
				{"name":"api_token","type":"string","classification":"secret"}
			]
		}`)
		if err := ValidateSchema(schema); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("unknown classification rejected", func(t *testing.T) {
		schema := json.RawMessage(`{
			"name":"employee","version":1,
			"fields":[
				{"name":"salary","type":"string","classification":"top-secret"}
			]
		}`)
		err := ValidateSchema(schema)
		if err == nil {
			t.Fatalf("expected error for unknown classification, got nil")
		}
		verrs, ok := err.(ValidationErrors)
		if !ok || len(verrs) != 1 || verrs[0].Field != "salary" {
			t.Fatalf("expected one ValidationError on salary, got %v", err)
		}
	})

	t.Run("empty classification passes (legacy)", func(t *testing.T) {
		schema := json.RawMessage(`{
			"name":"note","version":1,
			"fields":[{"name":"body","type":"string"}]
		}`)
		if err := ValidateSchema(schema); err != nil {
			t.Fatalf("expected nil for legacy schema, got %v", err)
		}
	})
}

// TestFieldSpecIsSensitive confirms the single predicate that drives
// encryption + audit/event redaction: a field is sensitive when marked
// Encrypted OR classified confidential/secret.
func TestFieldSpecIsSensitive(t *testing.T) {
	cases := []struct {
		name string
		f    FieldSpec
		want bool
	}{
		{"plain", FieldSpec{Name: "x"}, false},
		{"encrypted flag", FieldSpec{Name: "x", Encrypted: true}, true},
		{"public", FieldSpec{Name: "x", Classification: ClassificationPublic}, false},
		{"internal", FieldSpec{Name: "x", Classification: ClassificationInternal}, false},
		{"confidential", FieldSpec{Name: "x", Classification: ClassificationConfidential}, true},
		{"secret", FieldSpec{Name: "x", Classification: ClassificationSecret}, true},
		{"encrypted + public still sensitive", FieldSpec{Name: "x", Encrypted: true, Classification: ClassificationPublic}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.IsSensitive(); got != c.want {
				t.Fatalf("IsSensitive=%v, want %v", got, c.want)
			}
		})
	}
}
