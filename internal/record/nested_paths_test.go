package record

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// stubEncryptor is a minimal Encryptor for testing nested-path
// encryption without wiring a real KeyManager. It reversibly XORs
// plaintext with a fixed byte so we can verify round-trip without
// pulling in crypto dependencies.
type stubEncryptor struct{}

func (stubEncryptor) EncryptString(_ uuid.UUID, plaintext string) (string, error) {
	return "kapp:enc:v1:" + reverseString(plaintext), nil
}

func (stubEncryptor) DecryptString(_ uuid.UUID, value string) (string, error) {
	if !strings.HasPrefix(value, "kapp:enc:v1:") {
		return value, nil
	}
	return reverseString(strings.TrimPrefix(value, "kapp:enc:v1:")), nil
}

func reverseString(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// schemaWithNested builds a KType schema JSON with both a top-level
// encrypted field and a nested-path encrypted field.
func schemaWithNested(t *testing.T) json.RawMessage {
	t.Helper()
	s := ktype.Schema{
		Name:    "test",
		Version: 1,
		Fields: []ktype.FieldSpec{
			{Name: "ssn", Type: "string", Encrypted: true},
			{Name: "bank_iban", Type: "string", Encrypted: true, Path: "employee.bank.iban"},
			{Name: "name", Type: "string"},
		},
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return raw
}

func TestEncryptFields_NestedPath(t *testing.T) {
	schema := schemaWithNested(t)
	data := json.RawMessage(`{
		"ssn": "123-45-6789",
		"name": "Alice",
		"employee": {
			"bank": {
				"iban": "DE89370400440532013000"
			}
		}
	}`)
	store := &PGStore{encryptor: stubEncryptor{}}
	enc, err := store.encryptFields(uuid.Nil, schema, data)
	if err != nil {
		t.Fatalf("encryptFields: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(enc, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Top-level encrypted field
	if ssn, ok := doc["ssn"].(string); !ok || !strings.HasPrefix(ssn, "kapp:enc:v1:") {
		t.Fatalf("ssn not encrypted: %v", doc["ssn"])
	}
	// Non-encrypted field unchanged
	if name, ok := doc["name"].(string); !ok || name != "Alice" {
		t.Fatalf("name changed: %v", doc["name"])
	}
	// Nested encrypted field
	emp, ok := doc["employee"].(map[string]any)
	if !ok {
		t.Fatalf("employee not an object: %v", doc["employee"])
	}
	bank, ok := emp["bank"].(map[string]any)
	if !ok {
		t.Fatalf("bank not an object: %v", emp["bank"])
	}
	iban, ok := bank["iban"].(string)
	if !ok || !strings.HasPrefix(iban, "kapp:enc:v1:") {
		t.Fatalf("iban not encrypted: %v", bank["iban"])
	}
}

func TestDecryptRecordWith_NestedPath(t *testing.T) {
	schema := schemaWithNested(t)
	enc := stubEncryptor{}
	// Produce ciphertext via the same encryptor so the round-trip is exact.
	ssnCT, _ := enc.EncryptString(uuid.Nil, "123-45-6789")
	ibanCT, _ := enc.EncryptString(uuid.Nil, "DE89370400440532013000")
	encData, _ := json.Marshal(map[string]any{
		"ssn":  ssnCT,
		"name": "Alice",
		"employee": map[string]any{
			"bank": map[string]any{
				"iban": ibanCT,
			},
		},
	})
	kt := &ktype.KType{Schema: schema}
	store := &PGStore{encryptor: enc}
	r := &KRecord{TenantID: uuid.Nil, Data: encData}
	dec, err := store.decryptRecordWith(r, kt)
	if err != nil {
		t.Fatalf("decryptRecordWith: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(dec, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ssn, ok := doc["ssn"].(string); !ok || ssn != "123-45-6789" {
		t.Fatalf("ssn not decrypted: %v", doc["ssn"])
	}
	emp, ok := doc["employee"].(map[string]any)
	if !ok {
		t.Fatalf("employee not an object: %v", doc["employee"])
	}
	bank, ok := emp["bank"].(map[string]any)
	if !ok {
		t.Fatalf("bank not an object: %v", emp["bank"])
	}
	iban, ok := bank["iban"].(string)
	if !ok || iban != "DE89370400440532013000" {
		t.Fatalf("iban not decrypted: %v", bank["iban"])
	}
}

func TestRedactData_NestedPath(t *testing.T) {
	schema := schemaWithNested(t)
	data := json.RawMessage(`{
		"ssn": "123-45-6789",
		"name": "Alice",
		"employee": {
			"bank": {
				"iban": "DE89370400440532013000"
			}
		}
	}`)
	redacted := RedactData(schema, data)
	var doc map[string]any
	if err := json.Unmarshal(redacted, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["ssn"] != redactedPlaceholder {
		t.Fatalf("ssn not redacted: %v", doc["ssn"])
	}
	if doc["name"] != "Alice" {
		t.Fatalf("name changed: %v", doc["name"])
	}
	emp, ok := doc["employee"].(map[string]any)
	if !ok {
		t.Fatalf("employee not an object: %v", doc["employee"])
	}
	bank, ok := emp["bank"].(map[string]any)
	if !ok {
		t.Fatalf("bank not an object: %v", emp["bank"])
	}
	if bank["iban"] != redactedPlaceholder {
		t.Fatalf("iban not redacted: %v", bank["iban"])
	}
	marker, ok := doc[redactedMarkerKey].([]any)
	if !ok || len(marker) != 2 {
		t.Fatalf("redacted marker wrong: %v", doc[redactedMarkerKey])
	}
}

func TestWalkPath(t *testing.T) {
	doc := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": "leaf",
			},
		},
		"top": "val",
	}
	tests := []struct {
		segs    []string
		wantOk  bool
		wantVal string
	}{
		{[]string{"top"}, true, "val"},
		{[]string{"a", "b", "c"}, true, "leaf"},
		{[]string{"a", "b", "missing"}, false, ""},
		{[]string{"a", "notmap"}, false, ""},
		{[]string{}, false, ""},
	}
	for _, tc := range tests {
		parent, leaf, ok := walkPath(doc, tc.segs)
		if ok != tc.wantOk {
			t.Fatalf("walkPath(%v) ok=%v want %v", tc.segs, ok, tc.wantOk)
		}
		if ok && parent[leaf] != tc.wantVal {
			t.Fatalf("walkPath(%v) val=%v want %v", tc.segs, parent[leaf], tc.wantVal)
		}
	}
}

func TestSetPath_CreatesIntermediate(t *testing.T) {
	doc := map[string]any{}
	ok := setPath(doc, []string{"x", "y", "z"}, "value")
	if !ok {
		t.Fatal("setPath returned false")
	}
	x, ok := doc["x"].(map[string]any)
	if !ok {
		t.Fatal("x not created")
	}
	y, ok := x["y"].(map[string]any)
	if !ok {
		t.Fatal("y not created")
	}
	if y["z"] != "value" {
		t.Fatalf("z not set: %v", y["z"])
	}
}

func TestSetPath_TypeConflict(t *testing.T) {
	doc := map[string]any{"x": "not-a-map"}
	ok := setPath(doc, []string{"x", "y"}, "value")
	if ok {
		t.Fatal("setPath should return false on type conflict")
	}
}

func TestValidateSchema_PathValidation(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"empty path (top-level)", "", false},
		{"valid dotted", "employee.bank.iban", false},
		{"valid with underscores", "a_b.c_d", false},
		{"invalid char", "employee.bank.iban!", true},
		{"empty segment", "employee..iban", true},
		{"space in segment", "employee. bank", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ktype.Schema{
				Name:    "test",
				Version: 1,
				Fields: []ktype.FieldSpec{
					{Name: "f", Type: "string", Encrypted: true, Path: tc.path},
				},
			}
			raw, _ := json.Marshal(s)
			err := ktype.ValidateSchema(raw)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
