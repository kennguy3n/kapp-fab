package record

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// stubBlindIndexer produces a deterministic 16-byte digest for testing.
type stubBlindIndexer struct{}

func (stubBlindIndexer) BlindIndex(_ uuid.UUID, value string) (string, error) {
	// Simple deterministic digest: reverse + prefix
	return "idx:" + reverseString(value), nil
}

func schemaWithIndexed(t *testing.T) json.RawMessage {
	t.Helper()
	s := ktype.Schema{
		Name:    "test_idx",
		Version: 1,
		Fields: []ktype.FieldSpec{
			{Name: "email", Type: "string", Encrypted: true, Indexed: true},
			{Name: "ssn", Type: "string", Encrypted: true, Indexed: false},
			{Name: "name", Type: "string"},
		},
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return raw
}

func TestComputeBlindIndexes(t *testing.T) {
	schema := schemaWithIndexed(t)
	data := json.RawMessage(`{
		"email": "alice@example.com",
		"ssn": "123-45-6789",
		"name": "Alice"
	}`)
	idx, err := computeBlindIndexes(uuid.Nil, schema, data, stubBlindIndexer{})
	if err != nil {
		t.Fatalf("computeBlindIndexes: %v", err)
	}
	if idx == nil {
		t.Fatal("expected non-nil blind index")
	}
	var m map[string]string
	if err := json.Unmarshal(idx, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// email is indexed — should have a digest
	if _, ok := m["email"]; !ok {
		t.Fatal("email missing from blind index")
	}
	if !strings.HasPrefix(m["email"], "idx:") {
		t.Fatalf("email digest wrong: %v", m["email"])
	}
	// ssn is encrypted but NOT indexed — should not appear
	if _, ok := m["ssn"]; ok {
		t.Fatal("ssn should not be in blind index (not indexed)")
	}
	// name is not sensitive — should not appear
	if _, ok := m["name"]; ok {
		t.Fatal("name should not be in blind index (not sensitive)")
	}
}

func TestComputeBlindIndexes_NilIndexer(t *testing.T) {
	schema := schemaWithIndexed(t)
	data := json.RawMessage(`{"email": "alice@example.com"}`)
	idx, err := computeBlindIndexes(uuid.Nil, schema, data, nil)
	if err != nil {
		t.Fatalf("computeBlindIndexes: %v", err)
	}
	if idx != nil {
		t.Fatalf("expected nil blind index when indexer is nil, got %s", idx)
	}
}

func TestComputeBlindIndexes_NoIndexedFields(t *testing.T) {
	schema := json.RawMessage(`{"name":"test","version":1,"fields":[{"name":"x","type":"string","encrypted":true}]}`)
	data := json.RawMessage(`{"x":"value"}`)
	idx, err := computeBlindIndexes(uuid.Nil, schema, data, stubBlindIndexer{})
	if err != nil {
		t.Fatalf("computeBlindIndexes: %v", err)
	}
	if idx != nil {
		t.Fatalf("expected nil when no indexed fields, got %s", idx)
	}
}

func TestComputeBlindIndexes_NestedPath(t *testing.T) {
	s := ktype.Schema{
		Name:    "test_nested_idx",
		Version: 1,
		Fields: []ktype.FieldSpec{
			{Name: "bank_iban", Type: "string", Encrypted: true, Indexed: true, Path: "employee.bank.iban"},
		},
	}
	schema, _ := json.Marshal(s)
	data := json.RawMessage(`{"employee":{"bank":{"iban":"DE89370400440532013000"}}}`)
	idx, err := computeBlindIndexes(uuid.Nil, schema, data, stubBlindIndexer{})
	if err != nil {
		t.Fatalf("computeBlindIndexes: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(idx, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["bank_iban"]; !ok {
		t.Fatal("bank_iban missing from blind index")
	}
}

func TestBlindIndex_Deterministic(t *testing.T) {
	// The same value should produce the same digest across calls.
	idx1, _ := stubBlindIndexer{}.BlindIndex(uuid.Nil, "test@example.com")
	idx2, _ := stubBlindIndexer{}.BlindIndex(uuid.Nil, "test@example.com")
	if idx1 != idx2 {
		t.Fatalf("blind index not deterministic: %s != %s", idx1, idx2)
	}
	// Different values should produce different digests.
	idx3, _ := stubBlindIndexer{}.BlindIndex(uuid.Nil, "other@example.com")
	if idx1 == idx3 {
		t.Fatal("different values produced same digest")
	}
}
